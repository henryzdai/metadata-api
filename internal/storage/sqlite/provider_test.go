package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
	"metadata-api/internal/testsupport"
)

func TestProviderCapturesValidatedFilesystemPath(t *testing.T) {
	path := filepath.Join(testsupport.Workspace(t), "captured = # & %.db")
	unused := filepath.Join(testsupport.Workspace(t), "unused.db")
	values := storage.Values{"path": path}
	open, err := Provider().Prepare(values)
	if err != nil {
		t.Fatal(err)
	}
	values["path"] = unused
	store, err := open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.CreateMaintainer(context.Background(), metadata.MaintainerFields{
		Name: "Captured Path", Email: "captured@example.test",
	})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got, err := reopened.GetMaintainer(context.Background(), record.ID); err != nil || got != record {
		t.Fatalf("captured path did not persist the record: %#v, %v", got, err)
	}
	if _, err := os.Stat(unused); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed caller map affected the opened path: %v", err)
	}
}

func TestProviderRejectsInvalidValuesWithoutOpening(t *testing.T) {
	for _, values := range []storage.Values{
		nil, {"path": ""}, {"path": " \t "}, {"path": "private\x00value"},
		{"path": "valid.db", "unknown": "private-value"},
	} {
		if open, err := Provider().Prepare(values); open != nil || err == nil ||
			strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid provider values returned opener %v, error %v", open != nil, err)
		}
	}
}
