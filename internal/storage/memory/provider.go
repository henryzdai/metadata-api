package memory

import (
	"context"
	"errors"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
)

func Provider() storage.Provider {
	return storage.Provider{
		Name: "memory", Summary: "volatile in-memory storage",
		Prepare: func(values storage.Values, policy metadata.StorePolicy) (storage.OpenFunc, error) {
			if err := policy.Validate(); err != nil {
				return nil, err
			}
			if len(values) != 0 {
				return nil, errors.New("memory storage does not accept options")
			}
			return func(ctx context.Context) (metadata.MetadataStore, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return New(policy)
			}, nil
		},
	}
}
