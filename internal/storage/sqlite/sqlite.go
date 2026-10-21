package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"metadata-api/internal/metadata"

	driver "modernc.org/sqlite"
)

type Store struct {
	db     *sql.DB
	policy metadata.StorePolicy
}

var _ metadata.MetadataStore = (*Store)(nil)

const schemaVersion = 2

// Same-file writers must use matching limits and stop older binaries before an
// offline schema upgrade. Downgrade requires a compatible backup.
func Open(ctx context.Context, path string, policy metadata.StorePolicy) (*Store, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("the database filesystem path must not be empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid database filesystem path")
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, errors.New("cannot create the database directory")
	}
	uriPath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	uri := url.URL{Scheme: "file", Path: uriPath}
	// Reserve the writer before checking revisions; deferred upgrades can fail with SQLITE_BUSY_SNAPSHOT.
	query := url.Values{"_txlock": {"immediate"}}
	for _, pragma := range []string{
		"foreign_keys(1)", "journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(1000)",
		// A page-cache target is not a bound on process RSS or database file size.
		"cache_size(-8192)", "temp_store(FILE)",
	} {
		query.Add("_pragma", pragma)
	}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, translate(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, policy: policy}
	if err := store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	for pragma, want := range map[string]int{
		"foreign_keys": 1, "synchronous": 2, "busy_timeout": 1000,
		"cache_size": -8192, "temp_store": 1,
	} {
		var got int
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			return translate(err)
		}
		if got != want {
			return fmt.Errorf("required SQLite setting %s was not applied", pragma)
		}
	}
	var journal string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return translate(err)
	}
	if !strings.EqualFold(journal, "wal") {
		return errors.New("required SQLite WAL mode was not applied")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if version < 0 || version > schemaVersion {
			return fmt.Errorf("unsupported database schema version %d", version)
		}
		if version == 0 {
			for _, statement := range []string{
				`CREATE TABLE maintainers (
				id TEXT PRIMARY KEY NOT NULL,
				name TEXT NOT NULL,
				email TEXT NOT NULL COLLATE NOCASE,
				revision INTEGER NOT NULL CHECK (revision >= 1),
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
				`CREATE UNIQUE INDEX maintainers_email_unique ON maintainers (email COLLATE NOCASE)`,
				`CREATE TABLE applications (
				id TEXT PRIMARY KEY NOT NULL,
				title TEXT NOT NULL,
				version TEXT NOT NULL,
				company TEXT NOT NULL,
				website TEXT NOT NULL,
				source TEXT NOT NULL,
				license TEXT NOT NULL,
				description TEXT NOT NULL,
				maintainer_id TEXT NOT NULL REFERENCES maintainers(id) ON DELETE RESTRICT,
				revision INTEGER NOT NULL CHECK (revision >= 1),
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
				`CREATE INDEX applications_maintainer_id ON applications (maintainer_id, id)`,
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
		}
		if version < 2 {
			if _, err := tx.ExecContext(ctx, `CREATE TABLE store_usage (
				singleton INTEGER PRIMARY KEY NOT NULL CHECK (singleton = 1),
				maintainer_count INTEGER NOT NULL CHECK (maintainer_count >= 0),
				application_count INTEGER NOT NULL CHECK (application_count >= 0),
				field_bytes INTEGER NOT NULL CHECK (field_bytes >= 0)
			)`); err != nil {
				return err
			}
		}
		actual, err := measureUsage(ctx, tx)
		if err != nil {
			return err
		}
		limits := s.policy.Limits()
		if actual.maintainers > int64(limits.MaxMaintainers) ||
			actual.applications > int64(limits.MaxApplications) || actual.fieldBytes > limits.MaxFieldBytes {
			return metadata.NewError(metadata.ResourceCapacityExceeded, nil)
		}
		if version < 2 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO store_usage
				(singleton, maintainer_count, application_count, field_bytes) VALUES (1, ?, ?, ?)`,
				actual.maintainers, actual.applications, actual.fieldBytes); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
			return err
		}
		persisted, err := readUsage(ctx, tx)
		if err != nil {
			return err
		}
		if persisted != actual {
			return errors.New("resource accounting does not match committed rows")
		}
		return nil
	})
}

// The callback must use tx, not s.db, to avoid waiting on its own pooled connection.
func (s *Store) write(ctx context.Context, operation func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return translate(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := operation(tx); err != nil {
		return translate(err)
	}
	return translate(tx.Commit())
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	var domain *metadata.Error
	if errors.As(err, &domain) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return metadata.NewError(metadata.NotFound, nil)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return metadata.WrapError(metadata.Unavailable, err)
	}
	var sqliteError *driver.Error
	if errors.As(err, &sqliteError) && (sqliteError.Code()&0xff == 5 || sqliteError.Code()&0xff == 6) {
		return metadata.WrapError(metadata.Unavailable, err)
	}
	if errors.Is(err, sql.ErrConnDone) || strings.Contains(err.Error(), "database is closed") {
		return metadata.WrapError(metadata.Unavailable, err)
	}
	return metadata.WrapError(metadata.InternalError, err)
}

type scanner interface {
	Scan(...any) error
}

const maintainerColumns = "id, name, email, revision, created_at, updated_at"
const applicationColumns = "id, title, version, company, website, source, license, description, maintainer_id, revision, created_at, updated_at"

func scanMaintainer(row scanner) (metadata.Maintainer, error) {
	var record metadata.Maintainer
	var created, updated string
	if err := row.Scan(&record.ID, &record.Name, &record.Email, &record.Revision, &created, &updated); err != nil {
		return record, err
	}
	var err error
	record.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return record, err
	}
	record.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return record, err
}

func scanApplication(row scanner) (metadata.Application, error) {
	var record metadata.Application
	var created, updated string
	if err := row.Scan(
		&record.ID, &record.Title, &record.Version, &record.Company, &record.Website,
		&record.Source, &record.License, &record.Description, &record.MaintainerID,
		&record.Revision, &created, &updated,
	); err != nil {
		return record, err
	}
	var err error
	record.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return record, err
	}
	record.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return record, err
}

func (s *Store) Ping(ctx context.Context) error { return translate(s.db.PingContext(ctx)) }
func (s *Store) Close() error                   { return s.db.Close() }
