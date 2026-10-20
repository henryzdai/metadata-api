package memory

import (
	"context"
	"errors"
	"math"
	"testing"

	"metadata-api/internal/metadata"
)

func TestExhaustedRevisionDoesNotMutate(t *testing.T) {
	store := New()
	ctx := context.Background()
	record, err := store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Example", Email: "example@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	record.Revision = math.MaxInt64
	store.maintainers[record.ID] = record
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
