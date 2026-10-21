package memory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"unsafe"

	"metadata-api/internal/metadata"
)

func TestConstructorsRejectZeroPolicy(t *testing.T) {
	if store, err := New(metadata.StorePolicy{}); store != nil || err == nil {
		t.Fatal("memory accepted an unvalidated policy")
	}
	if open, err := Provider().Prepare(nil, metadata.StorePolicy{}); open != nil || err == nil {
		t.Fatal("provider accepted an unvalidated policy")
	}
}

func TestRetainedNormalizedStringsOwnTheirBackingStorage(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	padding := strings.Repeat(" ", 32768)
	padded := func(text string) string { return padding + text + padding }
	input := metadata.MaintainerFields{Name: padded("Owner"), Email: padded("owner@example.test")}
	normalized, err := metadata.NormalizeMaintainer(input)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateMaintainer(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	assertOwnedFields(t, owner.MaintainerFields, normalized)
	replacement := metadata.MaintainerFields{Name: padded("Replacement"), Email: padded("replacement@example.test")}
	normalized, err = metadata.NormalizeMaintainer(replacement)
	if err != nil {
		t.Fatal(err)
	}
	owner, err = store.UpdateMaintainer(ctx, owner.ID, replacement, []metadata.Revision{1})
	if err != nil {
		t.Fatal(err)
	}
	assertOwnedFields(t, owner.MaintainerFields, normalized)
	appInput := metadata.CreateApplicationInput{
		ApplicationFields: metadata.ApplicationFields{
			Title: padded("Title"), Version: padded("1"), Company: padded("Company"),
			Website: padded("https://example.test"), Source: padded("https://example.test/src"),
			License: padded("MIT"), Description: padded("Description"),
		},
		MaintainerID: metadata.MaintainerID(padded(string(owner.ID))),
	}
	appNormalized, err := metadata.NormalizeCreateApplication(appInput)
	if err != nil {
		t.Fatal(err)
	}
	app, err := store.CreateApplication(ctx, appInput)
	if err != nil {
		t.Fatal(err)
	}
	assertOwnedFields(t, app.ApplicationFields, appNormalized.ApplicationFields)
	if app.MaintainerID != appNormalized.MaintainerID ||
		unsafe.StringData(string(app.MaintainerID)) == unsafe.StringData(string(appNormalized.MaintainerID)) {
		t.Fatal("creator ID retained the padded input allocation")
	}
	app, err = store.UpdateApplication(ctx, app.ID, appInput.ApplicationFields, []metadata.Revision{1})
	if err != nil {
		t.Fatal(err)
	}
	assertOwnedFields(t, app.ApplicationFields, appNormalized.ApplicationFields)
	wantBytes := metadata.MaintainerFieldBytes(owner.MaintainerFields) +
		metadata.ApplicationFieldBytes(app.ApplicationFields, owner.ID)
	if store.fieldBytes != wantBytes || store.maintainers[owner.ID].applicationRefs != 1 {
		t.Fatalf("owned records have incorrect accounting: %d, want %d", store.fieldBytes, wantBytes)
	}
	if err := store.DeleteApplication(ctx, app.ID, []metadata.Revision{2}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteMaintainer(ctx, owner.ID, []metadata.Revision{2}); err != nil {
		t.Fatal(err)
	}
	if store.fieldBytes != 0 || len(store.maintainers) != 0 || len(store.applications) != 0 || len(store.emails) != 0 {
		t.Fatal("delete retained logical usage or index entries")
	}
}

func assertOwnedFields(t *testing.T, retained, normalized any) {
	t.Helper()
	got, input := reflect.ValueOf(retained), reflect.ValueOf(normalized)
	for i := 0; i < got.NumField(); i++ {
		owned, source := got.Field(i).String(), input.Field(i).String()
		if owned != source || owned != "" && unsafe.StringData(owned) == unsafe.StringData(source) {
			t.Fatalf("%s was changed or retained a caller-owned backing allocation", got.Type().Field(i).Name)
		}
	}
}

func TestCandidateSelectionRetainsAtMostLimitPlusOne(t *testing.T) {
	for _, limit := range []int{1, 20, 100} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			selected := newCandidates(limit+1, func(value string) string { return value })
			for i := 999; i >= 0; i-- {
				selected.add(fmt.Sprintf("%04d", i))
				if len(selected.items) > limit+1 || cap(selected.items) > limit+1 {
					t.Fatalf("retained %d/%d candidates for limit %d", len(selected.items), cap(selected.items), limit)
				}
			}
			for i, value := range selected.sorted() {
				if want := fmt.Sprintf("%04d", i); value != want {
					t.Fatalf("candidate %d = %s, want %s", i, value, want)
				}
			}
		})
	}
}

func TestCancellationDuringBoundedScans(t *testing.T) {
	store := newStore(t)
	for i := range 10 {
		owner, err := store.CreateMaintainer(context.Background(), metadata.MaintainerFields{
			Name: "Owner", Email: fmt.Sprintf("owner%d@example.test", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.CreateApplication(context.Background(), metadata.CreateApplicationInput{
			ApplicationFields: metadata.ApplicationFields{Title: "A", Version: "1", Company: "C"}, MaintainerID: owner.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"maintainers", "applications", "nonmatching filter"} {
		t.Run(kind, func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &scanContext{Context: base, cancelAt: 4, cancel: cancel}
			var err error
			if kind == "maintainers" {
				_, err = store.ListMaintainers(ctx, metadata.MaintainerList{})
			} else {
				filter := metadata.MaintainerID("")
				if kind == "nonmatching filter" {
					filter = metadata.MaintainerID(metadata.NewUUID())
				}
				_, err = store.ListApplications(ctx, metadata.ApplicationList{MaintainerID: filter})
			}
			var domain *metadata.Error
			if !errors.Is(err, context.Canceled) || !errors.As(err, &domain) || domain.Code != metadata.Unavailable || ctx.calls.Load() != 4 {
				t.Fatalf("scan did not observe cancellation: %v after %d checks", err, ctx.calls.Load())
			}
			if err := store.Ping(context.Background()); err != nil {
				t.Fatal("canceled scan left its lock or lifecycle unavailable")
			}
		})
	}
}

type scanContext struct {
	context.Context
	calls    atomic.Int32
	cancelAt int32
	cancel   context.CancelFunc
}

func (c *scanContext) Err() error {
	if c.calls.Add(1) == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}
