package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	"metadata-api/internal/metadata"
)

// Store shares one lock across both resources and the email index so relationship
// checks and mutations cannot interleave.
type Store struct {
	mu           sync.RWMutex
	maintainers  map[metadata.MaintainerID]maintainerEntry
	applications map[metadata.ApplicationID]metadata.Application
	emails       map[string]metadata.MaintainerID
	policy       metadata.StorePolicy
	fieldBytes   int64
	closed       bool
}

type maintainerEntry struct {
	metadata.Maintainer
	applicationRefs int
}

var _ metadata.MetadataStore = (*Store)(nil)

func New(policy metadata.StorePolicy) (*Store, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Store{
		maintainers:  make(map[metadata.MaintainerID]maintainerEntry),
		applications: make(map[metadata.ApplicationID]metadata.Application),
		emails:       make(map[string]metadata.MaintainerID),
		policy:       policy,
	}, nil
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
	if len(s.maintainers) >= s.policy.Limits().MaxMaintainers {
		return metadata.Maintainer{}, metadata.NewError(metadata.ResourceCapacityExceeded, nil)
	}
	nextBytes, err := s.policy.FieldBytesAfter(s.fieldBytes, 0, metadata.MaintainerFieldBytes(fields))
	if err != nil {
		return metadata.Maintainer{}, err
	}
	fields = ownMaintainer(fields)
	now := time.Now().UTC()
	record := metadata.Maintainer{
		ID: metadata.MaintainerID(metadata.NewUUID()), MaintainerFields: fields,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	s.maintainers[record.ID] = maintainerEntry{Maintainer: record}
	s.emails[fields.Email] = record.ID
	s.fieldBytes = nextBytes
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
	return record.Maintainer, nil
}

func (s *Store) ListMaintainers(ctx context.Context, options metadata.MaintainerList) (metadata.Page[metadata.Maintainer], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	page := metadata.Page[metadata.Maintainer]{Items: []metadata.Maintainer{}}
	if err := s.available(ctx); err != nil {
		return page, err
	}
	limit := metadata.PageLimit(options.Limit)
	candidates := newCandidates(limit+1, func(record metadata.Maintainer) string { return string(record.ID) })
	for id, entry := range s.maintainers {
		if err := ctx.Err(); err != nil {
			return page, metadata.WrapError(metadata.Unavailable, err)
		}
		if id > options.After {
			candidates.add(entry.Maintainer)
		}
	}
	used := metadata.PageEnvelopeBytes
	for _, record := range candidates.sorted() {
		size := metadata.MaintainerEncodedBound(record)
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
	nextBytes, err := s.policy.FieldBytesAfter(s.fieldBytes,
		metadata.MaintainerFieldBytes(record.MaintainerFields), metadata.MaintainerFieldBytes(fields))
	if err != nil {
		return metadata.Maintainer{}, err
	}
	fields = ownMaintainer(fields)
	delete(s.emails, record.Email)
	record.MaintainerFields = fields
	record.Revision = next
	record.UpdatedAt = time.Now().UTC()
	s.maintainers[id] = record
	s.emails[fields.Email] = id
	s.fieldBytes = nextBytes
	return record.Maintainer, nil
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
	if record.applicationRefs != 0 {
		return metadata.NewError(metadata.MaintainerInUse, nil)
	}
	if !metadata.Matches(record.Revision, expected) {
		return metadata.NewError(metadata.PreconditionFailed, nil)
	}
	nextBytes, err := s.policy.FieldBytesAfter(s.fieldBytes, metadata.MaintainerFieldBytes(record.MaintainerFields), 0)
	if err != nil {
		return err
	}
	delete(s.maintainers, id)
	delete(s.emails, record.Email)
	s.fieldBytes = nextBytes
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
	owner, exists := s.maintainers[input.MaintainerID]
	if !exists {
		return metadata.Application{}, metadata.NewError(metadata.InvalidReference, nil)
	}
	if len(s.applications) >= s.policy.Limits().MaxApplications {
		return metadata.Application{}, metadata.NewError(metadata.ResourceCapacityExceeded, nil)
	}
	nextBytes, err := s.policy.FieldBytesAfter(s.fieldBytes, 0,
		metadata.ApplicationFieldBytes(input.ApplicationFields, input.MaintainerID))
	if err != nil {
		return metadata.Application{}, err
	}
	input.ApplicationFields = ownApplication(input.ApplicationFields)
	input.MaintainerID = metadata.MaintainerID(strings.Clone(string(input.MaintainerID)))
	now := time.Now().UTC()
	record := metadata.Application{
		ID: metadata.ApplicationID(metadata.NewUUID()), ApplicationFields: input.ApplicationFields,
		MaintainerID: input.MaintainerID, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	s.applications[record.ID] = record
	owner.applicationRefs++
	s.maintainers[record.MaintainerID] = owner
	s.fieldBytes = nextBytes
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
	limit := metadata.PageLimit(options.Limit)
	candidates := newCandidates(limit+1, func(record metadata.Application) string { return string(record.ID) })
	for id, record := range s.applications {
		if err := ctx.Err(); err != nil {
			return page, metadata.WrapError(metadata.Unavailable, err)
		}
		if id > options.After && (options.MaintainerID == "" || options.MaintainerID == record.MaintainerID) {
			candidates.add(record)
		}
	}
	used := metadata.PageEnvelopeBytes
	for _, record := range candidates.sorted() {
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
	nextBytes, err := s.policy.FieldBytesAfter(s.fieldBytes,
		metadata.ApplicationFieldBytes(record.ApplicationFields, record.MaintainerID),
		metadata.ApplicationFieldBytes(fields, record.MaintainerID))
	if err != nil {
		return metadata.Application{}, err
	}
	record.ApplicationFields = ownApplication(fields)
	record.Revision = next
	record.UpdatedAt = time.Now().UTC()
	s.applications[id] = record
	s.fieldBytes = nextBytes
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
	nextBytes, err := s.policy.FieldBytesAfter(s.fieldBytes,
		metadata.ApplicationFieldBytes(record.ApplicationFields, record.MaintainerID), 0)
	if err != nil {
		return err
	}
	delete(s.applications, id)
	owner := s.maintainers[record.MaintainerID]
	owner.applicationRefs--
	s.maintainers[record.MaintainerID] = owner
	s.fieldBytes = nextBytes
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

func ownMaintainer(fields metadata.MaintainerFields) metadata.MaintainerFields {
	return metadata.MaintainerFields{Name: strings.Clone(fields.Name), Email: strings.Clone(fields.Email)}
}

func ownApplication(fields metadata.ApplicationFields) metadata.ApplicationFields {
	return metadata.ApplicationFields{
		Title: strings.Clone(fields.Title), Version: strings.Clone(fields.Version),
		Company: strings.Clone(fields.Company), Website: strings.Clone(fields.Website),
		Source: strings.Clone(fields.Source), License: strings.Clone(fields.License),
		Description: strings.Clone(fields.Description),
	}
}
