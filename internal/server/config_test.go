package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"metadata-api/internal/server"
	"metadata-api/internal/storage"
	"metadata-api/internal/storage/builtin"
	"metadata-api/internal/storage/memory"
	"metadata-api/internal/testsupport"
	"metadata-api/internal/testsupport/probe"
)

func TestStorageOptionValidationAndSafeErrors(t *testing.T) {
	const secret = "private-option-value"
	recorder := &probe.Recorder{}
	catalog, err := builtin.NewRegistry(recorder.Provider())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--storage=" + secret},
		{"--storage=memory", "--storage-option=path=" + secret},
		{"--storage=memory", "--db=" + secret},
		{"--storage=probe", "--db=" + secret},
		{"--storage-option=unknown=" + secret},
		{"--storage-option=path="},
		{"--storage-option=path= \t "},
		{"--storage-option=path=" + secret + "\x00"},
		{"--storage-option=" + secret},
		{"--storage-option==" + secret},
		{"--storage-option=path=" + secret, "--storage-option=path=" + secret},
		{"--storage-option=path=" + secret, "--db=" + secret},
		{"--db=" + secret, "--storage-option=path=" + secret},
		{"--db=" + secret, "--db=" + secret},
		{"--storage=probe", "--storage-option=probe-number=" + secret},
		{"--storage=probe", "--storage-option=probe-number=0"},
		{"--storage=probe", "--storage-option=probe-number=-1"},
		{"--storage=probe", "--storage-option=probe-number=999999999999999999999"},
		{"--storage=memory", "--probe-number=" + secret},
		{"--storage=probe", "--probe-number=" + secret},
		{"--storage=probe", "--probe-number=" + secret, "--probe-number=" + secret},
		{"--storage=probe", "--probe-number=" + secret, "--storage-option=probe-number=" + secret},
	} {
		var output bytes.Buffer
		if _, err := server.ParseConfig(args, &output, catalog); err == nil ||
			strings.Contains(err.Error(), secret) || strings.Contains(output.String(), secret) {
			t.Fatalf("invalid options error = %v, output %q", err, output.String())
		}
	}
	if len(recorder.Opens()) != 0 {
		t.Fatal("configuration validation opened a store")
	}
}

func TestParserRejectsReservedAliasesAndAbsentCatalog(t *testing.T) {
	for _, name := range []string{"listen", "storage", "storage-option", "request-timeout", "shutdown-timeout", "help", "h"} {
		t.Run(name, func(t *testing.T) {
			provider := memory.Provider()
			provider.Options = []storage.OptionSpec{{Key: "collision", FlagAlias: name}}
			catalog, err := storage.NewRegistry(provider.Name, provider)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := server.ParseConfig(nil, io.Discard, catalog); err == nil {
				t.Fatal("accepted a storage alias that shadows a server flag")
			}
		})
	}
	for _, catalog := range []*storage.Registry{nil, {}} {
		if _, err := server.ParseConfig(nil, io.Discard, catalog); err == nil {
			t.Fatal("accepted a missing or zero catalog")
		}
	}
}

func TestParserUsesTheCatalogDefaultAndPreparesOnlyOnce(t *testing.T) {
	recorder := &probe.Recorder{}
	provider := recorder.Provider()
	prepare := provider.Prepare
	preparations := 0
	provider.Prepare = func(values storage.Values) (storage.OpenFunc, error) {
		preparations++
		return prepare(values)
	}
	catalog, err := storage.NewRegistry(provider.Name, provider)
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseWithCatalog(t, catalog, "--listen=127.0.0.1:0")
	if preparations != 1 || len(recorder.Opens()) != 0 {
		t.Fatal("parser did not prepare exactly once without opening storage")
	}
	service := start(t, cfg, provider.Name, provider.Durable)
	response := service.do(t, http.MethodGet, "/readyz", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("custom default readiness = %d", response.StatusCode)
	}
	consume(t, response)
	service.stop(t)
	if preparations != 1 || len(recorder.Opens()) != 1 || recorder.Opens()[0] != 7 || recorder.Closes() != 1 {
		t.Fatal("Run reconstructed a catalog or did not open and close the prepared default")
	}
}

func TestExtensionFlagAliasComesFromItsDescriptor(t *testing.T) {
	recorder := &probe.Recorder{}
	provider := recorder.Provider()
	catalog, err := builtin.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseWithCatalog(t, catalog, "--storage=probe", "--listen=127.0.0.1:0", "--probe-number=41")
	service := start(t, cfg, provider.Name, provider.Durable)
	response := service.do(t, http.MethodGet, "/readyz", "")
	requireStatus(t, response, http.StatusOK)
	consume(t, response)
	service.stop(t)
	if len(recorder.Opens()) != 1 || recorder.Opens()[0] != 41 || recorder.Closes() != 1 {
		t.Fatal("descriptor alias did not reach the selected provider")
	}
}

func TestSQLiteAliasAndGenericOptionPreserveFilesystemPaths(t *testing.T) {
	for _, prefix := range []string{"--db=", "--storage-option=path="} {
		t.Run(prefix, func(t *testing.T) {
			path := filepath.Join(testsupport.Workspace(t), "metadata with spaces = # & %.db")
			cfg := parse(t, "--listen=127.0.0.1:0", prefix+path)
			service := start(t, cfg, "sqlite", true)
			created := service.do(t, http.MethodPost, "/v1/maintainers",
				`{"name":"Path Example","email":"path@example.test"}`)
			if created.StatusCode != http.StatusCreated {
				t.Fatalf("SQLite path create = %d", created.StatusCode)
			}
			location := created.Header.Get("Location")
			consume(t, created)
			service.stop(t)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("selected filesystem path was not used: %v", err)
			}
			restarted := start(t, cfg, "sqlite", true)
			response := restarted.do(t, http.MethodGet, location, "")
			if response.StatusCode != http.StatusOK {
				t.Fatalf("prepared SQLite path did not survive restart: %d", response.StatusCode)
			}
			consume(t, response)
			restarted.stop(t)
		})
	}
}

func TestPreparationFailurePreservesCauseWithoutExposingValues(t *testing.T) {
	cause := errors.New("private-option-value")
	provider := (&probe.Recorder{}).Provider()
	provider.Prepare = func(storage.Values) (storage.OpenFunc, error) { return nil, cause }
	catalog, err := builtin.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := server.ParseConfig([]string{"--storage=probe"}, &output, catalog); !errors.Is(err, cause) ||
		strings.Contains(err.Error(), cause.Error()) || output.Len() != 0 {
		t.Fatalf("unsafe preparation failure: error %v, output %q", err, output.String())
	}
	if err := server.Run(context.Background(), server.Config{}, io.Discard, io.Discard); err == nil {
		t.Fatal("unprepared configuration opened storage")
	}
}
