package sqlite

import (
	"context"
	"database/sql"
	"time"

	"metadata-api/internal/metadata"
)

func (s *Store) CreateApplication(ctx context.Context, input metadata.CreateApplicationInput) (metadata.Application, error) {
	input, err := metadata.NormalizeCreateApplication(input)
	if err != nil {
		return metadata.Application{}, err
	}
	now := time.Now().UTC()
	record := metadata.Application{
		ID: metadata.ApplicationID(metadata.NewUUID()), ApplicationFields: input.ApplicationFields,
		MaintainerID: input.MaintainerID, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM maintainers WHERE id = ?", input.MaintainerID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return metadata.NewError(metadata.InvalidReference, nil)
		}
		if err := s.adjustUsage(ctx, tx, 0, 1, 0,
			metadata.ApplicationFieldBytes(input.ApplicationFields, input.MaintainerID)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO applications ("+applicationColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			record.ID, record.Title, record.Version, record.Company, record.Website, record.Source,
			record.License, record.Description, record.MaintainerID, record.Revision,
			record.CreatedAt.Format(time.RFC3339Nano), record.UpdatedAt.Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		return metadata.Application{}, err
	}
	return record, nil
}

func (s *Store) GetApplication(ctx context.Context, id metadata.ApplicationID) (metadata.Application, error) {
	record, err := scanApplication(s.db.QueryRowContext(ctx,
		"SELECT "+applicationColumns+" FROM applications WHERE id = ?", id))
	return record, translate(err)
}

func (s *Store) ListApplications(ctx context.Context, options metadata.ApplicationList) (metadata.Page[metadata.Application], error) {
	page := metadata.Page[metadata.Application]{Items: []metadata.Application{}}
	limit := metadata.PageLimit(options.Limit)
	query := "SELECT " + applicationColumns + " FROM applications WHERE id > ?"
	args := []any{options.After}
	if options.MaintainerID != "" {
		query += " AND maintainer_id = ?"
		args = append(args, options.MaintainerID)
	}
	query += " ORDER BY id LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, translate(err)
	}
	defer func() { _ = rows.Close() }()
	used := metadata.PageEnvelopeBytes
	for rows.Next() {
		record, err := scanApplication(rows)
		if err != nil {
			return page, translate(err)
		}
		size := metadata.ApplicationEncodedBound(record)
		if len(page.Items) == limit || !metadata.PageItemFits(used, size) {
			if len(page.Items) == 0 {
				return page, metadata.NewError(metadata.InternalError, nil)
			}
			page.NextAfter = string(page.Items[len(page.Items)-1].ID)
			break
		}
		page.Items = append(page.Items, record)
		used += size
	}
	if err := rows.Err(); err != nil {
		return page, translate(err)
	}
	return page, nil
}

func (s *Store) UpdateApplication(ctx context.Context, id metadata.ApplicationID, input metadata.ApplicationFields, expected []metadata.Revision) (metadata.Application, error) {
	fields, err := metadata.NormalizeApplication(input)
	if err != nil {
		return metadata.Application{}, err
	}
	var record metadata.Application
	err = s.write(ctx, func(tx *sql.Tx) error {
		var err error
		record, err = scanApplication(tx.QueryRowContext(ctx,
			"SELECT "+applicationColumns+" FROM applications WHERE id = ?", id))
		if err != nil {
			return err
		}
		if !metadata.Matches(record.Revision, expected) {
			return metadata.NewError(metadata.PreconditionFailed, nil)
		}
		next, err := metadata.NextRevision(record.Revision)
		if err != nil {
			return err
		}
		if err := s.adjustUsage(ctx, tx, 0, 0,
			metadata.ApplicationFieldBytes(record.ApplicationFields, record.MaintainerID),
			metadata.ApplicationFieldBytes(fields, record.MaintainerID)); err != nil {
			return err
		}
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx,
			`UPDATE applications SET title = ?, version = ?, company = ?, website = ?, source = ?,
			license = ?, description = ?, revision = ?, updated_at = ? WHERE id = ? AND revision = ?`,
			fields.Title, fields.Version, fields.Company, fields.Website, fields.Source, fields.License,
			fields.Description, next, now.Format(time.RFC3339Nano), id, record.Revision)
		if err := changedOne(result, err); err != nil {
			return err
		}
		record.ApplicationFields = fields
		record.Revision = next
		record.UpdatedAt = now
		return nil
	})
	if err != nil {
		return metadata.Application{}, err
	}
	return record, nil
}

func (s *Store) DeleteApplication(ctx context.Context, id metadata.ApplicationID, expected []metadata.Revision) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		record, err := scanApplication(tx.QueryRowContext(ctx,
			"SELECT "+applicationColumns+" FROM applications WHERE id = ?", id))
		if err != nil {
			return err
		}
		if !metadata.Matches(record.Revision, expected) {
			return metadata.NewError(metadata.PreconditionFailed, nil)
		}
		if err := s.adjustUsage(ctx, tx, 0, -1,
			metadata.ApplicationFieldBytes(record.ApplicationFields, record.MaintainerID), 0); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM applications WHERE id = ? AND revision = ?", id, record.Revision)
		return changedOne(result, err)
	})
}
