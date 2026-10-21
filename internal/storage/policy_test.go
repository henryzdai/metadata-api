package storage_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
	"metadata-api/internal/testsupport/probe"
)

func TestRegistryRequiresAndPropagatesTheCompletePolicy(t *testing.T) {
	recorder := &probe.Recorder{}
	provider := recorder.Provider()
	prepared := 0
	prepare := provider.Prepare
	provider.Prepare = func(values storage.Values, policy metadata.StorePolicy) (storage.OpenFunc, error) {
		prepared++
		return prepare(values, policy)
	}
	catalog, err := storage.NewRegistry(provider.Name, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Prepare(provider.Name, nil, metadata.StorePolicy{}); err == nil || prepared != 0 {
		t.Fatal("invalid policy reached a provider")
	}
	limits := metadata.StoreLimits{MaxMaintainers: 1, MaxApplications: 2, MaxFieldBytes: 345}
	policy, err := metadata.NewStorePolicy(limits)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.Prepare(provider.Name, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	store, err := plan.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if !reflect.DeepEqual(recorder.Policies(), []metadata.StoreLimits{limits}) {
		t.Fatalf("probe received policies %#v, want %#v", recorder.Policies(), limits)
	}
	if _, err := store.CreateMaintainer(context.Background(), metadata.MaintainerFields{Name: "One", Email: "one@example.test"}); err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateMaintainer(context.Background(), metadata.MaintainerFields{Name: "Two", Email: "two@example.test"})
	var domain *metadata.Error
	if !errors.As(err, &domain) || domain.Code != metadata.ResourceCapacityExceeded {
		t.Fatalf("composed store did not enforce the captured policy: %v", err)
	}
}
