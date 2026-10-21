package builtin

import (
	"metadata-api/internal/storage"
	"metadata-api/internal/storage/memory"
	"metadata-api/internal/storage/sqlite"
)

func NewRegistry(extra ...storage.Provider) (*storage.Registry, error) {
	defaultProvider := sqlite.Provider()
	providers := []storage.Provider{defaultProvider, memory.Provider()}
	providers = append(providers, extra...)
	return storage.NewRegistry(defaultProvider.Name, providers...)
}
