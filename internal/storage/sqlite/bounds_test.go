package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metadata-api/internal/httpapi"
	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
	"metadata-api/internal/testsupport"
)

func TestInvalidPolicyDoesNotOpenOrCreateAFile(t *testing.T) {
	path := filepath.Join(testsupport.Workspace(t), "untouched", "records.db")
	if store, err := Open(context.Background(), path, metadata.StorePolicy{}); store != nil || err == nil {
		t.Fatal("SQLite accepted a zero policy")
	}
	if open, err := Provider().Prepare(storage.Values{"path": path}, metadata.StorePolicy{}); open != nil || err == nil {
		t.Fatal("SQLite provider accepted a zero policy")
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid policy modified the filesystem: %v", err)
	}
}

func TestV1MigrationPreservesEveryFieldAndValidatorAndSeedsUTF8Bytes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testsupport.Workspace(t), "v1 # & %.db")
	owner, app := seedV1(t, path)
	limits := metadata.StoreLimits{MaxMaintainers: 1, MaxApplications: 1,
		MaxFieldBytes: metadata.MaintainerFieldBytes(owner.MaintainerFields) +
			metadata.ApplicationFieldBytes(app.ApplicationFields, app.MaintainerID)}
	policy, err := metadata.NewStorePolicy(limits)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, err := Open(ctx, path, policy)
		if err != nil {
			t.Fatal(err)
		}
		assertLegacyRecords(t, store, owner, app)
		if got := storedUsage(t, store); got != (usage{1, 1, limits.MaxFieldBytes}) {
			t.Fatalf("migrated usage = %#v, want UTF-8 bytes %d", got, limits.MaxFieldBytes)
		}
		var version int
		if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
			t.Fatalf("schema = %d, %v", version, err)
		}
		_, err = store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Other", Email: "other@example.test"})
		assertCode(t, err, metadata.ResourceCapacityExceeded)
		_, err = store.UpdateApplication(ctx, app.ID, metadata.ApplicationFields{
			Title: app.Title, Version: app.Version, Company: app.Company,
			Website: app.Website, Source: app.Source, License: app.License, Description: app.Description + "x",
		}, []metadata.Revision{app.Revision})
		assertCode(t, err, metadata.ResourceCapacityExceeded)
		assertLegacyRecords(t, store, owner, app)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOverLimitV1MigrationRollsBackWithoutLosingOldData(t *testing.T) {
	for _, bound := range []string{"maintainers", "applications", "UTF-8 bytes"} {
		t.Run(bound, func(t *testing.T) {
			path := filepath.Join(testsupport.Workspace(t), "v1.db")
			owner, app := seedV1(t, path)
			limits := metadata.DefaultStorePolicy().Limits()
			raw := rawDB(t, path)
			switch bound {
			case "maintainers":
				limits.MaxMaintainers = 1
				_, err := raw.Exec("INSERT INTO maintainers ("+maintainerColumns+") VALUES (?, ?, ?, ?, ?, ?)",
					metadata.NewUUID(), "Second", "second@example.test", 1,
					owner.CreatedAt.Format(time.RFC3339Nano), owner.UpdatedAt.Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			case "applications":
				limits.MaxApplications = 1
				_, err := raw.Exec("INSERT INTO applications ("+applicationColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
					metadata.NewUUID(), "Second", app.Version, app.Company, app.Website, app.Source, app.License,
					app.Description, owner.ID, 1, app.CreatedAt.Format(time.RFC3339Nano), app.UpdatedAt.Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			default:
				limits.MaxFieldBytes = metadata.MaintainerFieldBytes(owner.MaintainerFields) +
					metadata.ApplicationFieldBytes(app.ApplicationFields, owner.ID) - 1
			}
			policy, err := metadata.NewStorePolicy(limits)
			if err != nil {
				t.Fatal(err)
			}
			store, err := Open(context.Background(), path, policy)
			if store != nil {
				_ = store.Close()
				t.Fatal("over-limit migration returned a store")
			}
			assertCode(t, err, metadata.ResourceCapacityExceeded)
			var version, tables int
			if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'store_usage'").Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if version != 1 || tables != 0 {
				t.Fatalf("failed migration changed schema: version %d, accounting tables %d", version, tables)
			}
			reopened, err := Open(context.Background(), path, metadata.DefaultStorePolicy())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			assertLegacyRecords(t, reopened, owner, app)
			wantMaintainers, wantApplications := int64(1), int64(1)
			if bound == "maintainers" {
				wantMaintainers++
			}
			if bound == "applications" {
				wantApplications++
			}
			if got := storedUsage(t, reopened); got.maintainers != wantMaintainers || got.applications != wantApplications {
				t.Fatalf("failed migration truncated records: %#v", got)
			}
		})
	}
}

func TestFailedV1MigrationPreservesOldSchemaAndData(t *testing.T) {
	path := filepath.Join(testsupport.Workspace(t), "v1.db")
	owner, app := seedV1(t, path)
	raw := rawDB(t, path)
	if _, err := raw.Exec("CREATE TABLE store_usage (sentinel TEXT NOT NULL); INSERT INTO store_usage VALUES ('preserved')"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), path, metadata.DefaultStorePolicy())
	if store != nil {
		_ = store.Close()
		t.Fatal("conflicting migration succeeded")
	}
	assertCode(t, err, metadata.InternalError)
	var version int
	var sentinel string
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow("SELECT sentinel FROM store_usage").Scan(&sentinel); err != nil {
		t.Fatal(err)
	}
	if version != 1 || sentinel != "preserved" {
		t.Fatal("failed migration rewrote the preexisting schema")
	}
	gotOwner, err := scanMaintainer(raw.QueryRow("SELECT "+maintainerColumns+" FROM maintainers WHERE id = ?", owner.ID))
	if err != nil || gotOwner != owner {
		t.Fatalf("failed migration changed maintainer: %#v, %v", gotOwner, err)
	}
	gotApp, err := scanApplication(raw.QueryRow("SELECT "+applicationColumns+" FROM applications WHERE id = ?", app.ID))
	if err != nil || gotApp != app {
		t.Fatalf("failed migration changed application: %#v, %v", gotApp, err)
	}
}

func TestV2ReopenChecksCommittedUsageAndRejectsLowerLimitsIntact(t *testing.T) {
	for _, test := range []string{"lower limit", "stale accounting", "missing accounting", "unaccounted row change"} {
		t.Run(test, func(t *testing.T) {
			path := filepath.Join(testsupport.Workspace(t), "records.db")
			owner, app := seedV1(t, path)
			store, err := Open(context.Background(), path, metadata.DefaultStorePolicy())
			if err != nil {
				t.Fatal(err)
			}
			before := storedUsage(t, store)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			raw := rawDB(t, path)
			limits := metadata.DefaultStorePolicy().Limits()
			wantCode := metadata.InternalError
			switch test {
			case "lower limit":
				limits.MaxFieldBytes = before.fieldBytes - 1
				wantCode = metadata.ResourceCapacityExceeded
			case "stale accounting":
				_, err = raw.Exec("UPDATE store_usage SET field_bytes = field_bytes - 1")
			case "missing accounting":
				_, err = raw.Exec("DELETE FROM store_usage")
			case "unaccounted row change":
				_, err = raw.Exec("UPDATE maintainers SET name = name || 'x' WHERE id = ?", owner.ID)
				owner.Name += "x"
			}
			if err != nil {
				t.Fatal(err)
			}
			policy, err := metadata.NewStorePolicy(limits)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(context.Background(), path, policy)
			if reopened != nil {
				_ = reopened.Close()
				t.Fatal("invalid reopen returned a store")
			}
			assertCode(t, err, wantCode)
			gotOwner, err := scanMaintainer(raw.QueryRow("SELECT "+maintainerColumns+" FROM maintainers WHERE id = ?", owner.ID))
			if err != nil || gotOwner != owner {
				t.Fatalf("failed reopen changed maintainer: %#v, %v", gotOwner, err)
			}
			gotApp, err := scanApplication(raw.QueryRow("SELECT "+applicationColumns+" FROM applications WHERE id = ?", app.ID))
			if err != nil || gotApp != app {
				t.Fatalf("failed reopen changed application: %#v, %v", gotApp, err)
			}
			var version int
			if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
				t.Fatalf("failed reopen changed schema: %d, %v", version, err)
			}
			if test == "lower limit" {
				restored, err := Open(context.Background(), path, metadata.DefaultStorePolicy())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = restored.Close() }()
				assertLegacyRecords(t, restored, owner, app)
				if storedUsage(t, restored) != before {
					t.Fatal("lower-limit rejection changed committed accounting")
				}
			}
		})
	}
}

func TestResourceAndAccountingRollbackTogether(t *testing.T) {
	for _, test := range []struct{ table, event string }{
		{"maintainers", "INSERT"}, {"maintainers", "UPDATE"}, {"maintainers", "DELETE"},
		{"applications", "INSERT"}, {"applications", "UPDATE"}, {"applications", "DELETE"},
	} {
		t.Run(test.table+" "+test.event, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(testsupport.Workspace(t), "records.db")
			store, err := Open(ctx, path, metadata.DefaultStorePolicy())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			owner, err := store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Owner", Email: "owner@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			fields := metadata.ApplicationFields{Title: "A", Version: "1", Company: "C", Description: "before"}
			var app metadata.Application
			if test.table == "applications" && test.event != "INSERT" {
				app, err = store.CreateApplication(ctx, metadata.CreateApplicationInput{ApplicationFields: fields, MaintainerID: owner.ID})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := storedUsage(t, store)
			_, err = store.db.Exec("CREATE TRIGGER fault BEFORE " + test.event + " ON " + test.table +
				" BEGIN SELECT RAISE(ABORT, 'private-accounting-fault'); END")
			if err != nil {
				t.Fatal(err)
			}
			mutate := func() error {
				if test.table == "maintainers" {
					switch test.event {
					case "INSERT":
						_, err := store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Second", Email: "second@example.test"})
						return err
					case "UPDATE":
						_, err := store.UpdateMaintainer(ctx, owner.ID,
							metadata.MaintainerFields{Name: "Longer owner", Email: "changed@example.test"}, []metadata.Revision{1})
						return err
					default:
						return store.DeleteMaintainer(ctx, owner.ID, []metadata.Revision{1})
					}
				}
				switch test.event {
				case "INSERT":
					_, err := store.CreateApplication(ctx, metadata.CreateApplicationInput{ApplicationFields: fields, MaintainerID: owner.ID})
					return err
				case "UPDATE":
					replacement := fields
					replacement.Description = "a longer description"
					_, err := store.UpdateApplication(ctx, app.ID, replacement, []metadata.Revision{1})
					return err
				default:
					return store.DeleteApplication(ctx, app.ID, []metadata.Revision{1})
				}
			}
			err = mutate()
			assertCode(t, err, metadata.InternalError)
			if strings.Contains(err.Error(), "private-accounting-fault") || storedUsage(t, store) != before {
				t.Fatal("failed resource transaction exposed SQL or changed usage")
			}
			gotOwner, err := store.GetMaintainer(ctx, owner.ID)
			if err != nil || gotOwner != owner {
				t.Fatal("failed transaction changed maintainer fields or revision")
			}
			if app.ID != "" {
				gotApp, err := store.GetApplication(ctx, app.ID)
				if err != nil || gotApp != app {
					t.Fatal("failed transaction changed application fields or revision")
				}
			}
			if _, err := store.db.Exec("DROP TRIGGER fault"); err != nil {
				t.Fatal(err)
			}
			if err := mutate(); err != nil {
				t.Fatalf("rolled-back mutation consumed capacity, revision or index: %v", err)
			}
			again, err := Open(ctx, path, metadata.DefaultStorePolicy())
			if err != nil {
				t.Fatalf("resource mutation did not commit accurate accounting: %v", err)
			}
			if err := again.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTwoStoreInstancesShareTheLastCountAndByteSlot(t *testing.T) {
	for _, bound := range []string{"count", "bytes"} {
		for _, resource := range []string{"maintainers", "applications"} {
			t.Run(resource+" "+bound, func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(testsupport.Workspace(t), "shared.db")
				ownerFields := metadata.MaintainerFields{Name: "O", Email: "o@example.test"}
				fields := metadata.ApplicationFields{Title: "A", Version: "1", Company: "C"}
				limits := metadata.DefaultStorePolicy().Limits()
				if bound == "count" {
					if resource == "maintainers" {
						limits.MaxMaintainers = 1
					} else {
						limits.MaxApplications = 1
					}
				} else if resource == "maintainers" {
					limits.MaxFieldBytes = metadata.MaintainerFieldBytes(metadata.MaintainerFields{Name: "S", Email: "slot0@example.test"})
				} else {
					limits.MaxFieldBytes = metadata.MaintainerFieldBytes(ownerFields) +
						metadata.ApplicationFieldBytes(fields, metadata.MaintainerID(strings.Repeat("x", 36)))
				}
				policy, err := metadata.NewStorePolicy(limits)
				if err != nil {
					t.Fatal(err)
				}
				first, err := Open(ctx, path, policy)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = first.Close() }()
				second, err := Open(ctx, path, policy)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = second.Close() }()
				var owner metadata.Maintainer
				if resource == "applications" {
					owner, err = first.CreateMaintainer(ctx, ownerFields)
					if err != nil {
						t.Fatal(err)
					}
				}
				start := make(chan struct{})
				results := make(chan error, 2)
				for i, store := range []*Store{first, second} {
					go func() {
						<-start
						var err error
						if resource == "maintainers" {
							_, err = store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "S", Email: fmt.Sprintf("slot%d@example.test", i)})
						} else {
							_, err = store.CreateApplication(ctx, metadata.CreateApplicationInput{ApplicationFields: fields, MaintainerID: owner.ID})
						}
						results <- err
					}()
				}
				close(start)
				successes := 0
				for range 2 {
					if err := <-results; err == nil {
						successes++
					} else {
						assertCode(t, err, metadata.ResourceCapacityExceeded)
					}
				}
				wantMaintainers, wantApplications := int64(1), int64(0)
				if resource == "applications" {
					wantApplications = 1
				}
				got := storedUsage(t, first)
				if successes != 1 || got.maintainers != wantMaintainers || got.applications != wantApplications {
					t.Fatalf("same-file last slot had %d winners and usage %#v", successes, got)
				}
				if storedUsage(t, second) != got {
					t.Fatal("same-file stores disagree on committed usage")
				}
			})
		}
	}
}

func TestCountArithmetic(t *testing.T) {
	for _, test := range []struct {
		current, delta int64
		limit          int
		want           int64
		code           metadata.ErrorCode
	}{
		{0, 1, 1, 1, ""}, {1, 1, 1, 0, metadata.ResourceCapacityExceeded},
		{1, -1, 1, 0, ""}, {2, -1, 1, 1, ""}, {2, 0, 1, 2, ""},
		{math.MaxInt64, 1, 1, 0, metadata.InternalError},
		{math.MaxInt64, math.MaxInt64, 1, 0, metadata.InternalError},
		{0, math.MinInt64, 1, 0, metadata.InternalError},
		{1, -2, 1, 0, metadata.InternalError}, {-1, 1, 1, 0, metadata.InternalError},
	} {
		got, err := countAfter(test.current, test.delta, test.limit)
		if test.code != "" {
			assertCode(t, err, test.code)
		} else if err != nil || got != test.want {
			t.Fatalf("countAfter(%d, %d) = %d, %v; want %d", test.current, test.delta, got, err, test.want)
		}
	}
}

func storedUsage(t *testing.T, store *Store) usage {
	t.Helper()
	var result usage
	if err := store.write(context.Background(), func(tx *sql.Tx) error {
		var err error
		result, err = readUsage(context.Background(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func seedV1(t *testing.T, path string) (metadata.Maintainer, metadata.Application) {
	t.Helper()
	raw := rawDB(t, path)
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE maintainers (id TEXT PRIMARY KEY NOT NULL, name TEXT NOT NULL,
			email TEXT NOT NULL COLLATE NOCASE, revision INTEGER NOT NULL CHECK (revision >= 1),
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX maintainers_email_unique ON maintainers (email COLLATE NOCASE)`,
		`CREATE TABLE applications (id TEXT PRIMARY KEY NOT NULL, title TEXT NOT NULL, version TEXT NOT NULL,
			company TEXT NOT NULL, website TEXT NOT NULL, source TEXT NOT NULL, license TEXT NOT NULL,
			description TEXT NOT NULL, maintainer_id TEXT NOT NULL REFERENCES maintainers(id) ON DELETE RESTRICT,
			revision INTEGER NOT NULL CHECK (revision >= 1), created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE INDEX applications_maintainer_id ON applications (maintainer_id, id)`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	created := time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC)
	updated := time.Date(2026, 10, 20, 6, 7, 8, 987654321, time.UTC)
	owner := metadata.Maintainer{
		ID:               "11111111-1111-4111-8111-111111111111",
		MaintainerFields: metadata.MaintainerFields{Name: "名 Owner", Email: "legacy@example.test"},
		Revision:         42, CreatedAt: created, UpdatedAt: updated,
	}
	app := metadata.Application{
		ID: "22222222-2222-4222-8222-222222222222", MaintainerID: owner.ID,
		ApplicationFields: metadata.ApplicationFields{
			Title: "Atlas 名", Version: "2.3-β", Company: "Company", Website: "https://example.test",
			Source: "https://example.test/src", License: "MIT", Description: "NUL \x00 quoted \" <>& \u2028 名",
		},
		Revision: 19, CreatedAt: created, UpdatedAt: updated,
	}
	if _, err := raw.Exec("INSERT INTO maintainers ("+maintainerColumns+") VALUES (?, ?, ?, ?, ?, ?)",
		owner.ID, owner.Name, owner.Email, owner.Revision,
		owner.CreatedAt.Format(time.RFC3339Nano), owner.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("INSERT INTO applications ("+applicationColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		app.ID, app.Title, app.Version, app.Company, app.Website, app.Source, app.License, app.Description,
		app.MaintainerID, app.Revision, app.CreatedAt.Format(time.RFC3339Nano), app.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return owner, app
}

func assertLegacyRecords(t *testing.T, store *Store, owner metadata.Maintainer, app metadata.Application) {
	t.Helper()
	gotOwner, err := store.GetMaintainer(context.Background(), owner.ID)
	if err != nil || gotOwner != owner {
		t.Fatalf("migration changed maintainer fields: %#v, %v", gotOwner, err)
	}
	gotApp, err := store.GetApplication(context.Background(), app.ID)
	if err != nil || gotApp != app {
		t.Fatalf("migration changed application fields: %#v, %v", gotApp, err)
	}
	handler, err := httpapi.New(store, nil, httpapi.Options{RequestTimeout: time.Second, MaxInFlight: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ path, tag string }{
		{"/v1/maintainers/" + string(owner.ID), fmt.Sprintf(`"m1:%s:%d"`, owner.ID, owner.Revision)},
		{"/v1/applications/" + string(app.ID), fmt.Sprintf(`"a1:%s:%d"`, app.ID, app.Revision)},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != 200 || response.Header().Get("ETag") != test.tag || !json.Valid(response.Body.Bytes()) {
			t.Fatalf("migration changed validator: %d %q", response.Code, response.Header().Get("ETag"))
		}
	}
}
