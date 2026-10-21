package probe

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
	"metadata-api/internal/storage/memory"
)

type Recorder struct {
	mu       sync.Mutex
	opens    []int
	policies []metadata.StoreLimits
	closes   int
}

func (r *Recorder) Provider() storage.Provider {
	return storage.Provider{
		Name: "probe", Summary: "test-only composed memory storage",
		Options: []storage.OptionSpec{{
			Key: "probe-number", Default: "7", FlagAlias: "probe-number", Help: "positive test-only probe number",
		}},
		Prepare: func(values storage.Values, policy metadata.StorePolicy) (storage.OpenFunc, error) {
			if err := policy.Validate(); err != nil {
				return nil, err
			}
			number, err := strconv.Atoi(values["probe-number"])
			if err != nil || number <= 0 || len(values) != 1 {
				return nil, errors.New("probe-number must be a positive integer")
			}
			return func(ctx context.Context) (metadata.MetadataStore, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				inner, err := memory.New(policy)
				if err != nil {
					return nil, err
				}
				store := &recordedStore{MetadataStore: inner, recorder: r}
				r.mu.Lock()
				r.opens = append(r.opens, number)
				r.policies = append(r.policies, policy.Limits())
				r.mu.Unlock()
				return store, nil
			}, nil
		},
	}
}

func (r *Recorder) Opens() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.opens)
}

func (r *Recorder) Closes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closes
}

func (r *Recorder) Policies() []metadata.StoreLimits {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.policies)
}

type recordedStore struct {
	metadata.MetadataStore
	recorder *Recorder
	once     sync.Once
	err      error
}

func (s *recordedStore) Close() error {
	s.once.Do(func() {
		s.err = s.MetadataStore.Close()
		s.recorder.mu.Lock()
		s.recorder.closes++
		s.recorder.mu.Unlock()
	})
	return s.err
}
