package e2e

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorageOptionAndLegacyAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store #option=value.db")
	legacy := startServer(t, "sqlite", "", "--db", path)
	created := legacy.maintainer(t, "Owner", "owner@example.test")
	owner := decode[record](t, created)
	legacy.stop(t, false)

	generic := startServer(t, "sqlite", "", "--storage-option", "path="+path)
	reloaded := generic.get(t, "/v1/maintainers/"+owner.ID)
	if !bytes.Equal(created.body, reloaded.body) ||
		created.header.Get("ETag") != reloaded.header.Get("ETag") {
		t.Fatal("generic storage options did not reopen the legacy path unchanged")
	}
}

func TestStorageHelpUsesRegisteredProviders(t *testing.T) {
	command := exec.Command(serverBinary, "--help")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("server help failed: %v\n%s", err, output)
	}
	for _, description := range backendRegistry(t).Descriptions() {
		if !strings.Contains(string(output), description.Name) {
			t.Errorf("help omitted registered provider %q", description.Name)
		}
	}
	if !strings.Contains(string(output), "storage-option") {
		t.Error("help omitted generic storage configuration")
	}
}

func TestStorageOptionErrorsDoNotExposeValues(t *testing.T) {
	const marker = "storage-option-value-do-not-log"
	tests := []struct {
		name string
		args []string
	}{
		{"unknown option", []string{"--storage-option", "unknown=" + marker}},
		{"invalid syntax", []string{"--storage-option", marker}},
		{"duplicate option", []string{"--storage-option", "path=" + marker, "--storage-option", "path=second.db"}},
		{"alias conflict", []string{"--db", marker, "--storage-option", "path=second.db"}},
		{"unsupported option", []string{"--storage", "memory", "--storage-option", "path=" + marker}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			args := append([]string{"--listen", "127.0.0.1:0"}, test.args...)
			command := exec.CommandContext(ctx, serverBinary, args...)
			command.Dir = t.TempDir()
			output, err := command.CombinedOutput()
			if ctx.Err() != nil || err == nil {
				t.Fatalf("invalid storage configuration did not fail promptly: %v", err)
			}
			if strings.Contains(string(output), marker) {
				t.Fatal("startup error exposed a storage option value")
			}
			if strings.Contains(string(output), `"event":"listening"`) {
				t.Fatal("invalid storage configuration started a listener")
			}
		})
	}
}
