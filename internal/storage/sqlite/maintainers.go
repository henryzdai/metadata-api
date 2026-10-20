package sqlite

import (
	"context"
	"database/sql"
	"time"

	"metadata-api/internal/metadata"
)

func (s *Store) CreateMaintainer(ctx context.Context, input metadata.MaintainerFields) (metadata.Maintainer, error) {
	fields, err := metadata.NormalizeMaintainer(input)
	if err != nil {
		return metadata.Maintainer{}, err
	}
	now := time.Now().UTC()
	record := metadata.Maintainer{
		ID: metadata.MaintainerID(metadata.NewUUID()), MaintainerFields: fields,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := emailAvailable(ctx, tx, fields.Email, ""); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO maintainers ("+maintainerColumns+") VALUES (?, ?, ?, ?, ?, ?)",
			record.ID, record.Name, record.Email, record.Revision,
			record.CreatedAt.Format(time.RFC3339Nano), record.UpdatedAt.Format(time.RFC3339Nano),
		)
		return err
	})
	if err != nil {
		return metadata.Maintainer{}, err
	}
	return record, nil
}

func (s *Store) GetMaintainer(ctx context.Context, id metadata.MaintainerID) (metadata.Maintainer, error) {
	record, err := scanMaintainer(s.db.QueryRowContext(ctx,
		"SELECT "+maintainerColumns+" FROM maintainers WHERE id = ?", id))
	return record, translate(err)
}

func (s *Store) ListMaintainers(ctx context.Context, options metadata.MaintainerList) (metadata.Page[metadata.Maintainer], error) {
	page := metadata.Page[metadata.Maintainer]{Items: []metadata.Maintainer{}}
	limit := metadata.PageLimit(options.Limit)
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+maintainerColumns+" FROM maintainers WHERE id > ? ORDER BY id LIMIT ?", options.After, limit+1)
	if err != nil {
		return page, translate(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record, err := scanMaintainer(rows)
		if err != nil {
			return page, translate(err)
		}
		page.Items = append(page.Items, record)
	}
	if err := rows.Err(); err != nil {
		return page, translate(err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = string(page.Items[limit-1].ID)
	}
	return page, nil
}

func emailAvailable(ctx context.Context, tx *sql.Tx, email string, except metadata.MaintainerID) error {
	var count int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM maintainers WHERE email = ? AND id != ?", email, except).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return metadata.NewError(metadata.DuplicateEmail, nil)
	}
	return nil
}

func (s *Store) UpdateMaintainer(ctx context.Context, id metadata.MaintainerID, input metadata.MaintainerFields, expected []metadata.Revision) (metadata.Maintainer, error) {
	fields, err := metadata.NormalizeMaintainer(input)
	if err != nil {
		return metadata.Maintainer{}, err
	}
	var record metadata.Maintainer
	err = s.write(ctx, func(tx *sql.Tx) error {
		var err error
		record, err = scanMaintainer(tx.QueryRowContext(ctx,
			"SELECT "+maintainerColumns+" FROM maintainers WHERE id = ?", id))
		if err != nil {
			return err
		}
		if err := emailAvailable(ctx, tx, fields.Email, id); err != nil {
			return err
		}
		if !metadata.Matches(record.Revision, expected) {
			return metadata.NewError(metadata.PreconditionFailed, nil)
		}
		next, err := metadata.NextRevision(record.Revision)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx,
			"UPDATE maintainers SET name = ?, email = ?, revision = ?, updated_at = ? WHERE id = ? AND revision = ?",
			fields.Name, fields.Email, next, now.Format(time.RFC3339Nano), id, record.Revision)
		if err := changedOne(result, err); err != nil {
			return err
		}
		record.MaintainerFields = fields
		record.Revision = next
		record.UpdatedAt = now
		return nil
	})
	if err != nil {
		return metadata.Maintainer{}, err
	}
	return record, nil
}

func (s *Store) DeleteMaintainer(ctx context.Context, id metadata.MaintainerID, expected []metadata.Revision) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var revision metadata.Revision
		if err := tx.QueryRowContext(ctx, "SELECT revision FROM maintainers WHERE id = ?", id).Scan(&revision); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM applications WHERE maintainer_id = ?", id).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return metadata.NewError(metadata.MaintainerInUse, nil)
		}
		if !metadata.Matches(revision, expected) {
			return metadata.NewError(metadata.PreconditionFailed, nil)
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM maintainers WHERE id = ? AND revision = ?", id, revision)
		return changedOne(result, err)
	})
}

func changedOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return metadata.NewError(metadata.PreconditionFailed, nil)
	}
	return nil
}
