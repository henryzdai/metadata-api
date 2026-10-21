package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storagetest"
	"metadata-api/internal/testsupport"
)

func TestMetadataStoreContract(t *testing.T) {
	storagetest.Contract(t, func(t *testing.T) metadata.MetadataStore {
		store, err := Open(context.Background(), filepath.Join(testsupport.Workspace(t), "records.db"))
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func TestDurableReopenAndEscapedFilesystemPath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testsupport.Workspace(t), "directory with spaces", "metadata # & %.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := first.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Example", Email: "example@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := first.CreateApplication(ctx, metadata.CreateApplicationInput{
		ApplicationFields: metadata.ApplicationFields{Title: "Atlas", Version: "1.0", Company: "Example"},
		MaintainerID:      owner.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	gotOwner, err := second.GetMaintainer(ctx, owner.ID)
	if err != nil || gotOwner != owner {
		t.Fatalf("reopened maintainer = %#v, %v", gotOwner, err)
	}
	gotApp, err := second.GetApplication(ctx, app.ID)
	if err != nil || gotApp != app {
		t.Fatalf("reopened application = %#v, %v", gotApp, err)
	}
	for pragma, want := range map[string]int{"foreign_keys": 1, "synchronous": 2, "busy_timeout": 1000, "user_version": 1} {
		var got int
		if err := second.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", pragma, got, err, want)
		}
	}
	var journal string
	if err := second.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal mode = %q, %v", journal, err)
	}
	if second.db.Stats().MaxOpenConnections != 1 {
		t.Fatal("SQLite connection cap differs from one")
	}
	if _, err := second.db.ExecContext(ctx, "DELETE FROM maintainers WHERE id = ?", owner.ID); err == nil {
		t.Fatal("foreign key failed to restrict direct maintainer deletion")
	}
	if _, err := second.db.ExecContext(ctx,
		"INSERT INTO maintainers ("+maintainerColumns+") VALUES (?, ?, ?, ?, ?, ?)",
		metadata.NewUUID(), "Duplicate", "EXAMPLE@EXAMPLE.TEST", 1,
		owner.CreatedAt.Format(time.RFC3339Nano), owner.UpdatedAt.Format(time.RFC3339Nano)); err == nil {
		t.Fatal("database did not enforce case-insensitive email uniqueness")
	}
}

func TestUnsupportedFutureSchemaAndTransactionalMigration(t *testing.T) {
	for _, test := range []struct {
		name string
		seed string
	}{
		{"future", "PRAGMA user_version = 2"},
		{"conflicting table", "CREATE TABLE applications (id TEXT)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(testsupport.Workspace(t), "records.db")
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(test.seed); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := Open(context.Background(), path)
			if err == nil {
				_ = store.Close()
				t.Fatal("unsupported database initialized")
			}
			raw, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			var version, maintainers int
			if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'maintainers'").Scan(&maintainers); err != nil {
				t.Fatal(err)
			}
			wantVersion := 0
			if test.name == "future" {
				wantVersion = 2
			}
			if version != wantVersion || maintainers != 0 {
				t.Fatalf("failed migration changed schema; version %d, maintainer tables %d", version, maintainers)
			}
		})
	}
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("accepted empty filesystem path")
	}
}

func TestRevisionExhaustionAndBusyAreNonMutating(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testsupport.Workspace(t), "records.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	record, err := store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Example", Email: "example@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE maintainers SET revision = ? WHERE id = ?", int64(math.MaxInt64), record.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.UpdateMaintainer(ctx, record.ID, metadata.MaintainerFields{Name: "Changed", Email: record.Email},
		[]metadata.Revision{math.MaxInt64})
	assertCode(t, err, metadata.InternalError)
	got, err := store.GetMaintainer(ctx, record.ID)
	if err != nil || got.Revision != math.MaxInt64 || got.Name != record.Name {
		t.Fatalf("exhausted revision mutated: %#v, %v", got, err)
	}
	blocker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close() }()
	connection, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = connection.ExecContext(ctx, "ROLLBACK") }()
	_, err = store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Blocked", Email: "blocked@example.test"})
	assertCode(t, err, metadata.Unavailable)
	if strings.Contains(err.Error(), "SQLITE") {
		t.Fatal("storage error exposes SQL details")
	}
}

func TestTwoStoreInstancesHaveOneConditionalWinner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testsupport.Workspace(t), "records.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	record, err := first.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Initial", Email: "initial@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, store := range []*Store{first, second} {
		go func() {
			<-start
			_, err := store.UpdateMaintainer(ctx, record.ID,
				metadata.MaintainerFields{Name: "Replaced", Email: record.Email}, []metadata.Revision{1})
			results <- err
		}()
	}
	close(start)
	success := 0
	for range 2 {
		if err := <-results; err == nil {
			success++
		} else {
			assertCode(t, err, metadata.PreconditionFailed)
		}
	}
	got, err := first.GetMaintainer(ctx, record.ID)
	if err != nil || success != 1 || got.Revision != 2 {
		t.Fatalf("two-store condition failed; successes %d, record %#v, error %v", success, got, err)
	}
}

func assertCode(t *testing.T, err error, want metadata.ErrorCode) {
	t.Helper()
	var problem *metadata.Error
	if !errors.As(err, &problem) || problem.Code != want {
		t.Fatalf("expected %s, got %v", want, err)
	}
}
