package builtin_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
	"metadata-api/internal/storage/builtin"
	"metadata-api/internal/testsupport"
)

func TestEachPreparedBuiltinCapturesAndEnforcesTheCompletePolicy(t *testing.T) {
	catalog, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := metadata.NewStorePolicy(metadata.StoreLimits{
		MaxMaintainers: 1, MaxApplications: 1, MaxFieldBytes: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range catalog.Descriptions() {
		t.Run(backend.Name, func(t *testing.T) {
			values := make(storage.Values)
			for _, option := range backend.Options {
				if option.FlagAlias == "db" {
					values[option.Key] = filepath.Join(testsupport.Workspace(t), "records.db")
				}
			}
			plan, err := catalog.Prepare(backend.Name, values, policy)
			if err != nil {
				t.Fatal(err)
			}
			store, err := plan.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			owner, err := store.CreateMaintainer(context.Background(), metadata.MaintainerFields{Name: "Owner", Email: "owner@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.CreateMaintainer(context.Background(), metadata.MaintainerFields{Name: "Other", Email: "other@example.test"})
			requireCapacity(t, err)
			fields := metadata.ApplicationFields{Title: "T", Version: "1", Company: "C"}
			input := metadata.CreateApplicationInput{ApplicationFields: fields, MaintainerID: owner.ID}
			app, err := store.CreateApplication(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.CreateApplication(context.Background(), input)
			requireCapacity(t, err)
			fields.Description = strings.Repeat("x", 128)
			_, err = store.UpdateApplication(context.Background(), app.ID, fields, []metadata.Revision{1})
			requireCapacity(t, err)
			got, err := store.GetApplication(context.Background(), app.ID)
			if err != nil || got != app {
				t.Fatal("provider policy rejection changed the resource")
			}
		})
	}
}

func requireCapacity(t *testing.T, err error) {
	t.Helper()
	var domain *metadata.Error
	if !errors.As(err, &domain) || domain.Code != metadata.ResourceCapacityExceeded {
		t.Fatalf("expected captured resource capacity, got %v", err)
	}
}
