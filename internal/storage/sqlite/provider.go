package sqlite

import (
	"context"
	"errors"
	"strings"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
)

func Provider() storage.Provider {
	return storage.Provider{
		Name: "sqlite", Summary: "durable local SQLite storage", Durable: true,
		Options: []storage.OptionSpec{{
			Key: "path", Default: "data/metadata.db", FlagAlias: "db",
			Help: "SQLite database filesystem path, not a DSN",
		}},
		Prepare: func(values storage.Values, policy metadata.StorePolicy) (storage.OpenFunc, error) {
			if err := policy.Validate(); err != nil {
				return nil, err
			}
			path, exists := values["path"]
			if !exists || len(values) != 1 || strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
				return nil, errors.New("SQLite path must be a nonempty filesystem path without NUL bytes")
			}
			return func(ctx context.Context) (metadata.MetadataStore, error) {
				return Open(ctx, path, policy)
			}, nil
		},
	}
}
