package sqlite

import (
	"context"
	"database/sql"
	"math"

	"metadata-api/internal/metadata"
)

type usage struct {
	maintainers  int64
	applications int64
	fieldBytes   int64
}

func measureUsage(ctx context.Context, tx *sql.Tx) (usage, error) {
	var actual usage
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM maintainers),
		(SELECT count(*) FROM applications),
		(SELECT coalesce(sum(length(CAST(name AS BLOB)) + length(CAST(email AS BLOB))), 0) FROM maintainers) +
		(SELECT coalesce(sum(
			length(CAST(title AS BLOB)) + length(CAST(version AS BLOB)) +
			length(CAST(company AS BLOB)) + length(CAST(website AS BLOB)) +
			length(CAST(source AS BLOB)) + length(CAST(license AS BLOB)) +
			length(CAST(description AS BLOB)) + length(CAST(maintainer_id AS BLOB))
		), 0) FROM applications)`).Scan(&actual.maintainers, &actual.applications, &actual.fieldBytes)
	return actual, err
}

func readUsage(ctx context.Context, tx *sql.Tx) (usage, error) {
	var current usage
	err := tx.QueryRowContext(ctx, `SELECT maintainer_count, application_count, field_bytes
		FROM store_usage WHERE singleton = 1`).Scan(&current.maintainers, &current.applications, &current.fieldBytes)
	if err == sql.ErrNoRows {
		return current, metadata.NewError(metadata.InternalError, nil)
	}
	return current, err
}

func countAfter(current, delta int64, limit int) (int64, error) {
	if current < 0 || delta < -current || delta > 0 && current > math.MaxInt64-delta {
		return 0, metadata.NewError(metadata.InternalError, nil)
	}
	next := current + delta
	if next > current && next > int64(limit) {
		return 0, metadata.NewError(metadata.ResourceCapacityExceeded, nil)
	}
	return next, nil
}

func (s *Store) adjustUsage(ctx context.Context, tx *sql.Tx, maintainers, applications, removed, added int64) error {
	current, err := readUsage(ctx, tx)
	if err != nil {
		return err
	}
	limits := s.policy.Limits()
	nextMaintainers, err := countAfter(current.maintainers, maintainers, limits.MaxMaintainers)
	if err != nil {
		return err
	}
	nextApplications, err := countAfter(current.applications, applications, limits.MaxApplications)
	if err != nil {
		return err
	}
	nextBytes, err := s.policy.FieldBytesAfter(current.fieldBytes, removed, added)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE store_usage SET maintainer_count = ?, application_count = ?,
		field_bytes = ? WHERE singleton = 1`, nextMaintainers, nextApplications, nextBytes)
	return err
}
