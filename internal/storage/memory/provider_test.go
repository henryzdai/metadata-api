package memory

import (
	"context"
	"errors"
	"testing"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
)

func TestProviderOpensIndependentStoresAndHonorsCancellation(t *testing.T) {
	provider := Provider()
	if _, err := provider.Prepare(storage.Values{"path": "private-value"}, metadata.DefaultStorePolicy()); err == nil {
		t.Fatal("memory provider accepted an option")
	}
	open, err := provider.Prepare(nil, metadata.DefaultStorePolicy())
	if err != nil {
		t.Fatal(err)
	}
	first, err := open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if first == second {
		t.Fatal("volatile provider reused a store")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if store, err := open(ctx); store != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open = %v, %v", store, err)
	}
}
