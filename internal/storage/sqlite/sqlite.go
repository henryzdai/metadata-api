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
	db *sql.DB
}

var _ metadata.MetadataStore = (*Store)(nil)

func Open(ctx context.Context, path string) (*Store, error) {
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
	store := &Store{db: db}
	if err := store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	for pragma, want := range map[string]int{"foreign_keys": 1, "synchronous": 2, "busy_timeout": 1000} {
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
		if version > 1 {
			return fmt.Errorf("unsupported database schema version %d", version)
		}
		if version == 1 {
			return nil
		}
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
			`PRAGMA user_version = 1`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
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
