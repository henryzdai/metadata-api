package memory

import (
	"context"
	"errors"
	"math"
	"testing"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storagetest"
)

func TestMetadataStoreContract(t *testing.T) {
	storagetest.Contract(t, func(t *testing.T) metadata.MetadataStore { return newStore(t) })
}

func TestResourceBoundsContract(t *testing.T) {
	storagetest.BoundsContract(t, func(t *testing.T, policy metadata.StorePolicy) metadata.MetadataStore {
		store, err := New(policy)
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func TestExhaustedRevisionDoesNotMutate(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	record, err := store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Example", Email: "example@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	record.Revision = math.MaxInt64
	store.maintainers[record.ID] = maintainerEntry{Maintainer: record}
	_, err = store.UpdateMaintainer(ctx, record.ID, metadata.MaintainerFields{Name: "Changed", Email: record.Email},
		[]metadata.Revision{math.MaxInt64})
	var problem *metadata.Error
	if !errors.As(err, &problem) || problem.Code != metadata.InternalError {
		t.Fatalf("expected safe failure, got %v", err)
	}

	got, err := store.GetMaintainer(ctx, record.ID)
	if err != nil || got != record {
		t.Fatalf("exhaustion mutated record: %#v, %v", got, err)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(metadata.DefaultStorePolicy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
