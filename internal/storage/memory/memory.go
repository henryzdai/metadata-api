package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"metadata-api/internal/metadata"
)

// Store shares one lock across both resources and the email index so relationship
// checks and mutations cannot interleave.
type Store struct {
	mu           sync.RWMutex
	maintainers  map[metadata.MaintainerID]metadata.Maintainer
	applications map[metadata.ApplicationID]metadata.Application
	emails       map[string]metadata.MaintainerID
	closed       bool
}

var _ metadata.MetadataStore = (*Store)(nil)

func New() *Store {
	return &Store{
		maintainers:  make(map[metadata.MaintainerID]metadata.Maintainer),
		applications: make(map[metadata.ApplicationID]metadata.Application),
		emails:       make(map[string]metadata.MaintainerID),
	}
}

func (s *Store) available(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return metadata.WrapError(metadata.Unavailable, err)
	}
	if s.closed {
		return metadata.NewError(metadata.Unavailable, nil)
	}
	return nil
}

func (s *Store) CreateMaintainer(ctx context.Context, input metadata.MaintainerFields) (metadata.Maintainer, error) {
	fields, err := metadata.NormalizeMaintainer(input)
	if err != nil {
		return metadata.Maintainer{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(ctx); err != nil {
		return metadata.Maintainer{}, err
	}
	if _, exists := s.emails[fields.Email]; exists {
		return metadata.Maintainer{}, metadata.NewError(metadata.DuplicateEmail, nil)
	}
	now := time.Now().UTC()
	record := metadata.Maintainer{
		ID: metadata.MaintainerID(metadata.NewUUID()), MaintainerFields: fields,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	s.maintainers[record.ID] = record
	s.emails[fields.Email] = record.ID
	return record, nil
}

func (s *Store) GetMaintainer(ctx context.Context, id metadata.MaintainerID) (metadata.Maintainer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.available(ctx); err != nil {
		return metadata.Maintainer{}, err
	}
	record, exists := s.maintainers[id]
	if !exists {
		return metadata.Maintainer{}, metadata.NewError(metadata.NotFound, nil)
	}
	return record, nil
}

func (s *Store) ListMaintainers(ctx context.Context, options metadata.MaintainerList) (metadata.Page[metadata.Maintainer], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	page := metadata.Page[metadata.Maintainer]{Items: []metadata.Maintainer{}}
	if err := s.available(ctx); err != nil {
		return page, err
	}
	for id, record := range s.maintainers {
		if id > options.After {
			page.Items = append(page.Items, record)
		}
	}
	sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].ID < page.Items[j].ID })
	limit := metadata.PageLimit(options.Limit)
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = string(page.Items[limit-1].ID)
	}
	return page, nil
}

func (s *Store) UpdateMaintainer(ctx context.Context, id metadata.MaintainerID, input metadata.MaintainerFields, expected []metadata.Revision) (metadata.Maintainer, error) {
	fields, err := metadata.NormalizeMaintainer(input)
	if err != nil {
		return metadata.Maintainer{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(ctx); err != nil {
		return metadata.Maintainer{}, err
	}
	record, exists := s.maintainers[id]
	if !exists {
		return metadata.Maintainer{}, metadata.NewError(metadata.NotFound, nil)
	}
	if owner, exists := s.emails[fields.Email]; exists && owner != id {
		return metadata.Maintainer{}, metadata.NewError(metadata.DuplicateEmail, nil)
	}
	if !metadata.Matches(record.Revision, expected) {
		return metadata.Maintainer{}, metadata.NewError(metadata.PreconditionFailed, nil)
	}
	next, err := metadata.NextRevision(record.Revision)
	if err != nil {
		return metadata.Maintainer{}, err
	}
	delete(s.emails, record.Email)
	record.MaintainerFields = fields
	record.Revision = next
	record.UpdatedAt = time.Now().UTC()
	s.maintainers[id] = record
	s.emails[fields.Email] = id
	return record, nil
}

func (s *Store) DeleteMaintainer(ctx context.Context, id metadata.MaintainerID, expected []metadata.Revision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(ctx); err != nil {
		return err
	}
	record, exists := s.maintainers[id]
	if !exists {
		return metadata.NewError(metadata.NotFound, nil)
	}
	for _, application := range s.applications {
		if application.MaintainerID == id {
			return metadata.NewError(metadata.MaintainerInUse, nil)
		}
	}
	if !metadata.Matches(record.Revision, expected) {
		return metadata.NewError(metadata.PreconditionFailed, nil)
	}
	delete(s.maintainers, id)
	delete(s.emails, record.Email)
	return nil
}

func (s *Store) CreateApplication(ctx context.Context, input metadata.CreateApplicationInput) (metadata.Application, error) {
	input, err := metadata.NormalizeCreateApplication(input)
	if err != nil {
		return metadata.Application{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(ctx); err != nil {
		return metadata.Application{}, err
	}
	if _, exists := s.maintainers[input.MaintainerID]; !exists {
		return metadata.Application{}, metadata.NewError(metadata.InvalidReference, nil)
	}
	now := time.Now().UTC()
	record := metadata.Application{
		ID: metadata.ApplicationID(metadata.NewUUID()), ApplicationFields: input.ApplicationFields,
		MaintainerID: input.MaintainerID, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	s.applications[record.ID] = record
	return record, nil
}

func (s *Store) GetApplication(ctx context.Context, id metadata.ApplicationID) (metadata.Application, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.available(ctx); err != nil {
		return metadata.Application{}, err
	}
	record, exists := s.applications[id]
	if !exists {
		return metadata.Application{}, metadata.NewError(metadata.NotFound, nil)
	}
	return record, nil
}

func (s *Store) ListApplications(ctx context.Context, options metadata.ApplicationList) (metadata.Page[metadata.Application], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	page := metadata.Page[metadata.Application]{Items: []metadata.Application{}}
	if err := s.available(ctx); err != nil {
		return page, err
	}
	for id, record := range s.applications {
		if id > options.After && (options.MaintainerID == "" || options.MaintainerID == record.MaintainerID) {
			page.Items = append(page.Items, record)
		}
	}
	sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].ID < page.Items[j].ID })
	limit := metadata.PageLimit(options.Limit)
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = string(page.Items[limit-1].ID)
	}
	return page, nil
}

func (s *Store) UpdateApplication(ctx context.Context, id metadata.ApplicationID, input metadata.ApplicationFields, expected []metadata.Revision) (metadata.Application, error) {
	fields, err := metadata.NormalizeApplication(input)
	if err != nil {
		return metadata.Application{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(ctx); err != nil {
		return metadata.Application{}, err
	}
	record, exists := s.applications[id]
	if !exists {
		return metadata.Application{}, metadata.NewError(metadata.NotFound, nil)
	}
	if !metadata.Matches(record.Revision, expected) {
		return metadata.Application{}, metadata.NewError(metadata.PreconditionFailed, nil)
	}
	next, err := metadata.NextRevision(record.Revision)
	if err != nil {
		return metadata.Application{}, err
	}
	record.ApplicationFields = fields
	record.Revision = next
	record.UpdatedAt = time.Now().UTC()
	s.applications[id] = record
	return record, nil
}

func (s *Store) DeleteApplication(ctx context.Context, id metadata.ApplicationID, expected []metadata.Revision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(ctx); err != nil {
		return err
	}
	record, exists := s.applications[id]
	if !exists {
		return metadata.NewError(metadata.NotFound, nil)
	}
	if !metadata.Matches(record.Revision, expected) {
		return metadata.NewError(metadata.PreconditionFailed, nil)
	}
	delete(s.applications, id)
	return nil
}

func (s *Store) Ping(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.available(ctx)
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
