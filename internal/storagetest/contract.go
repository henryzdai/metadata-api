package storagetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"metadata-api/internal/metadata"
)

func Contract(t *testing.T, open func(*testing.T) metadata.MetadataStore) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, metadata.MetadataStore)
	}{
		{"maintainer CRUD and copies", maintainerCRUD},
		{"application CRUD and references", applicationCRUD},
		{"creator identity is independent of contact details", creatorIdentity},
		{"lexical pagination and filtering", pagination},
		{"conflict and existence precedence", errorPrecedence},
		{"concurrent conditional writes", concurrentWrites},
		{"concurrent uniqueness", concurrentUniqueness},
		{"creator delete versus application create", creatorRace},
		{"cancellation and closure", lifecycle},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := open(t)
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			test.run(t, store)
		})
	}
}

func maintainerCRUD(t *testing.T, store metadata.MetadataStore) {
	ctx := context.Background()
	record, err := store.CreateMaintainer(ctx, metadata.MaintainerFields{
		Name: "  River Example ", Email: " RIVER@EXAMPLE.TEST ",
	})
	must(t, err)
	if !metadata.ValidUUID(string(record.ID)) || record.Name != "River Example" ||
		record.Email != "river@example.test" || record.Revision != 1 ||
		record.CreatedAt.IsZero() || record.CreatedAt.Location() != time.UTC || record.UpdatedAt != record.CreatedAt {
		t.Fatalf("created maintainer = %#v", record)
	}
	saved := record
	record.Name = "not persisted"
	got, err := store.GetMaintainer(ctx, saved.ID)
	must(t, err)
	if got != saved {
		t.Fatalf("creation result aliases store or read differs: %#v, %#v", saved, got)
	}
	got.Email = "changed@example.test"
	got, err = store.GetMaintainer(ctx, saved.ID)
	must(t, err)
	if got != saved {
		t.Fatal("read result mutated stored record")
	}
	_, err = store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Duplicate", Email: "River@Example.Test"})
	wantCode(t, err, metadata.DuplicateEmail)
	updated, err := store.UpdateMaintainer(ctx, saved.ID, metadata.MaintainerFields{
		Name: " New Name ", Email: " NEW@EXAMPLE.TEST ",
	}, []metadata.Revision{99, 1})
	must(t, err)
	if updated.Name != "New Name" || updated.Email != "new@example.test" ||
		updated.Revision != 2 || updated.CreatedAt != saved.CreatedAt ||
		updated.UpdatedAt.Before(saved.UpdatedAt) {
		t.Fatalf("updated = %#v", updated)
	}
	_, err = store.UpdateMaintainer(ctx, saved.ID, saved.MaintainerFields, []metadata.Revision{1})
	wantCode(t, err, metadata.PreconditionFailed)
	wantCode(t, store.DeleteMaintainer(ctx, saved.ID, nil), metadata.PreconditionFailed)
	got, err = store.GetMaintainer(ctx, saved.ID)
	must(t, err)
	if got != updated {
		t.Fatal("stale or empty-condition write changed record")
	}
	unchanged, err := store.UpdateMaintainer(ctx, saved.ID, updated.MaintainerFields, []metadata.Revision{2})
	must(t, err)
	if unchanged.Revision != 3 || unchanged.CreatedAt != saved.CreatedAt {
		t.Fatal("unchanged replacement did not advance revision")
	}
	must(t, store.DeleteMaintainer(ctx, saved.ID, []metadata.Revision{1, 3}))
	_, err = store.GetMaintainer(ctx, saved.ID)
	wantCode(t, err, metadata.NotFound)
	_, err = store.UpdateMaintainer(ctx, saved.ID, saved.MaintainerFields, []metadata.Revision{3})
	wantCode(t, err, metadata.NotFound)
	wantCode(t, store.DeleteMaintainer(ctx, saved.ID, []metadata.Revision{3}), metadata.NotFound)
	_, err = store.CreateMaintainer(ctx, saved.MaintainerFields)
	must(t, err)
}

func applicationCRUD(t *testing.T, store metadata.MetadataStore) {
	ctx := context.Background()
	owner := maintainer(t, store, "owner")
	fields := metadata.ApplicationFields{
		Title: " Atlas ", Version: " 1.0 ", Company: " Example ",
		Website: " https://example.test ", Source: " https://example.test/source ",
		License: " MIT ", Description: " A demo ",
	}
	input := metadata.CreateApplicationInput{ApplicationFields: fields, MaintainerID: owner.ID}
	record, err := store.CreateApplication(ctx, input)
	must(t, err)
	if !metadata.ValidUUID(string(record.ID)) || record.Title != "Atlas" || record.Version != "1.0" ||
		record.Company != "Example" || record.License != "MIT" || record.Description != "A demo" ||
		record.MaintainerID != owner.ID || record.Revision != 1 || record.CreatedAt.IsZero() ||
		record.CreatedAt.Location() != time.UTC || record.UpdatedAt != record.CreatedAt {
		t.Fatalf("created application = %#v", record)
	}
	saved := record
	record.MaintainerID = metadata.MaintainerID(metadata.NewUUID())
	record.Title = "not persisted"
	got, err := store.GetApplication(ctx, saved.ID)
	must(t, err)
	if got != saved {
		t.Fatal("application creation result aliases store")
	}
	got.Description = "changed"
	got, err = store.GetApplication(ctx, saved.ID)
	must(t, err)
	if got != saved {
		t.Fatal("application read result aliases store")
	}
	wantCode(t, store.DeleteMaintainer(ctx, owner.ID, []metadata.Revision{999}), metadata.MaintainerInUse)
	fields.Title = "Replaced"
	updated, err := store.UpdateApplication(ctx, saved.ID, fields, []metadata.Revision{99, 1})
	must(t, err)
	if updated.MaintainerID != owner.ID || updated.Title != "Replaced" || updated.Revision != 2 ||
		updated.CreatedAt != saved.CreatedAt || updated.UpdatedAt.Before(saved.UpdatedAt) {
		t.Fatalf("updated application = %#v", updated)
	}
	_, err = store.UpdateApplication(ctx, saved.ID, saved.ApplicationFields, nil)
	wantCode(t, err, metadata.PreconditionFailed)
	wantCode(t, store.DeleteApplication(ctx, saved.ID, []metadata.Revision{1}), metadata.PreconditionFailed)
	got, err = store.GetApplication(ctx, saved.ID)
	must(t, err)
	if got != updated {
		t.Fatal("stale application write changed record")
	}
	unchanged, err := store.UpdateApplication(ctx, saved.ID, updated.ApplicationFields, []metadata.Revision{2})
	must(t, err)
	if unchanged.Revision != 3 {
		t.Fatal("unchanged application did not advance revision")
	}
	must(t, store.DeleteApplication(ctx, saved.ID, []metadata.Revision{3}))
	_, err = store.GetApplication(ctx, saved.ID)
	wantCode(t, err, metadata.NotFound)
	_, err = store.UpdateApplication(ctx, saved.ID, fields, []metadata.Revision{3})
	wantCode(t, err, metadata.NotFound)
	wantCode(t, store.DeleteApplication(ctx, saved.ID, nil), metadata.NotFound)
	must(t, store.DeleteMaintainer(ctx, owner.ID, []metadata.Revision{1}))
	_, err = store.CreateApplication(ctx, input)
	wantCode(t, err, metadata.InvalidReference)
	input.MaintainerID = ""
	_, err = store.CreateApplication(ctx, input)
	wantCode(t, err, metadata.ValidationFailed)
}

func creatorIdentity(t *testing.T, store metadata.MetadataStore) {
	ctx := context.Background()
	creator := maintainer(t, store, "creator")
	application, err := store.CreateApplication(ctx, metadata.CreateApplicationInput{
		ApplicationFields: metadata.ApplicationFields{
			Title: "Inventory", Version: "1.0", Company: "Example",
		},
		MaintainerID: creator.ID,
	})
	must(t, err)
	updatedCreator, err := store.UpdateMaintainer(ctx, creator.ID, metadata.MaintainerFields{
		Name: "Renamed Creator", Email: "renamed@example.test",
	}, []metadata.Revision{creator.Revision})
	must(t, err)
	if updatedCreator.ID != creator.ID || updatedCreator.Revision != creator.Revision+1 {
		t.Fatal("contact replacement changed creator identity or did not advance its revision")
	}
	unchanged, err := store.GetApplication(ctx, application.ID)
	must(t, err)
	if unchanged != application {
		t.Fatal("updating creator contact details rewrote the application")
	}
	page, err := store.ListApplications(ctx, metadata.ApplicationList{MaintainerID: creator.ID})
	must(t, err)
	if len(page.Items) != 1 || page.Items[0] != application {
		t.Fatal("creator lookup no longer finds the unchanged application")
	}
	wantCode(t, store.DeleteMaintainer(ctx, creator.ID, []metadata.Revision{updatedCreator.Revision}), metadata.MaintainerInUse)
}

func pagination(t *testing.T, store metadata.MetadataStore) {
	ctx := context.Background()
	empty, err := store.ListMaintainers(ctx, metadata.MaintainerList{})
	must(t, err)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextAfter != "" {
		t.Fatalf("empty page = %#v", empty)
	}
	var ids []string
	var owners []metadata.Maintainer
	for i := range 7 {
		record := maintainer(t, store, fmt.Sprintf("page%d", i))
		ids = append(ids, string(record.ID))
		owners = append(owners, record)
	}
	sort.Strings(ids)
	var seen []string
	var cursor metadata.MaintainerID
	for {
		page, err := store.ListMaintainers(ctx, metadata.MaintainerList{Limit: 3, After: cursor})
		must(t, err)
		if len(page.Items) > 3 {
			t.Fatal("page exceeded limit")
		}
		for _, record := range page.Items {
			seen = append(seen, string(record.ID))
		}
		if page.NextAfter == "" {
			break
		}
		if page.NextAfter != string(page.Items[len(page.Items)-1].ID) {
			t.Fatal("cursor did not identify last returned record")
		}
		cursor = metadata.MaintainerID(page.NextAfter)
	}
	if !reflect.DeepEqual(seen, ids) {
		t.Fatalf("pagination = %v, want %v", seen, ids)
	}
	page, err := store.ListMaintainers(ctx, metadata.MaintainerList{After: metadata.MaintainerID(ids[len(ids)-1])})
	must(t, err)
	if page.Items == nil || len(page.Items) != 0 || page.NextAfter != "" {
		t.Fatal("terminal cursor did not return an empty terminal page")
	}
	page, err = store.ListMaintainers(ctx, metadata.MaintainerList{})
	must(t, err)
	page.Items[0].Name = "not persisted"
	read, err := store.GetMaintainer(ctx, page.Items[0].ID)
	must(t, err)
	if read.Name == "not persisted" {
		t.Fatal("list results alias store")
	}
	var wanted []string
	var all []string
	for i := range 9 {
		owner := owners[i%2]
		app, err := store.CreateApplication(ctx, metadata.CreateApplicationInput{
			ApplicationFields: applicationFields(fmt.Sprintf("App %d", i)), MaintainerID: owner.ID,
		})
		must(t, err)
		all = append(all, string(app.ID))
		if owner.ID == owners[0].ID {
			wanted = append(wanted, string(app.ID))
		}
	}
	sort.Strings(wanted)
	sort.Strings(all)
	for _, filter := range []struct {
		owner metadata.MaintainerID
		want  []string
	}{
		{"", all}, {owners[0].ID, wanted}, {metadata.MaintainerID(metadata.NewUUID()), []string{}},
	} {
		actual := []string{}
		var after metadata.ApplicationID
		for {
			page, err := store.ListApplications(ctx, metadata.ApplicationList{
				Limit: 2, After: after, MaintainerID: filter.owner,
			})
			must(t, err)
			if page.Items == nil || len(page.Items) > 2 {
				t.Fatalf("invalid application page %#v", page)
			}
			for _, record := range page.Items {
				actual = append(actual, string(record.ID))
				record.Title = "not persisted"
				read, err := store.GetApplication(ctx, record.ID)
				must(t, err)
				if read.Title == record.Title {
					t.Fatal("application list aliases store")
				}
			}
			if page.NextAfter == "" {
				break
			}
			after = metadata.ApplicationID(page.NextAfter)
		}
		if !reflect.DeepEqual(actual, filter.want) {
			t.Fatalf("filtered pagination = %v, want %v", actual, filter.want)
		}
	}
}

func errorPrecedence(t *testing.T, store metadata.MetadataStore) {
	ctx := context.Background()
	first := maintainer(t, store, "first")
	second := maintainer(t, store, "second")
	_, err := store.UpdateMaintainer(ctx, first.ID, second.MaintainerFields, []metadata.Revision{999})
	wantCode(t, err, metadata.DuplicateEmail)
	_, err = store.UpdateMaintainer(ctx, metadata.MaintainerID(metadata.NewUUID()), second.MaintainerFields, nil)
	wantCode(t, err, metadata.NotFound)
	_, err = store.UpdateMaintainer(ctx, first.ID, metadata.MaintainerFields{}, nil)
	wantCode(t, err, metadata.ValidationFailed)
	got, err := store.GetMaintainer(ctx, first.ID)
	must(t, err)
	if got != first {
		t.Fatal("conflict changed stored maintainer")
	}
}

func concurrentWrites(t *testing.T, store metadata.MetadataStore) {
	ctx := context.Background()
	owner := maintainer(t, store, "concurrent")
	start := make(chan struct{})
	results := make(chan struct {
		record metadata.Maintainer
		err    error
	}, 2)
	for i := range 2 {
		go func() {
			<-start
			record, err := store.UpdateMaintainer(ctx, owner.ID, metadata.MaintainerFields{
				Name: fmt.Sprintf("Winner %d", i), Email: fmt.Sprintf("winner%d@example.test", i),
			}, []metadata.Revision{1})
			results <- struct {
				record metadata.Maintainer
				err    error
			}{record, err}
		}()
	}
	close(start)
	var winner metadata.Maintainer
	success, stale := 0, 0
	for range 2 {
		result := <-results
		if result.err == nil {
			success++
			winner = result.record
		} else {
			wantCode(t, result.err, metadata.PreconditionFailed)
			stale++
		}
	}
	if success != 1 || stale != 1 || winner.Revision != 2 {
		t.Fatalf("concurrent writes = %d success, %d stale; winner %#v", success, stale, winner)
	}
	got, err := store.GetMaintainer(ctx, owner.ID)
	must(t, err)
	if got != winner {
		t.Fatalf("stored record %#v differs from winner %#v", got, winner)
	}
	app, err := store.CreateApplication(ctx, metadata.CreateApplicationInput{
		ApplicationFields: applicationFields("Initial"), MaintainerID: owner.ID,
	})
	must(t, err)
	appResults := make(chan struct {
		record metadata.Application
		err    error
	}, 2)
	start = make(chan struct{})
	for i := range 2 {
		go func() {
			<-start
			record, err := store.UpdateApplication(ctx, app.ID, applicationFields(fmt.Sprintf("Winner %d", i)), []metadata.Revision{1})
			appResults <- struct {
				record metadata.Application
				err    error
			}{record, err}
		}()
	}
	close(start)
	var appWinner metadata.Application
	success, stale = 0, 0
	for range 2 {
		result := <-appResults
		if result.err == nil {
			success++
			appWinner = result.record
		} else {
			wantCode(t, result.err, metadata.PreconditionFailed)
			stale++
		}
	}
	gotApp, err := store.GetApplication(ctx, app.ID)
	must(t, err)
	if success != 1 || stale != 1 || gotApp != appWinner || gotApp.Revision != 2 {
		t.Fatalf("concurrent application updates lost atomicity; %#v", gotApp)
	}
	must(t, store.DeleteApplication(ctx, app.ID, []metadata.Revision{gotApp.Revision}))
}

func concurrentUniqueness(t *testing.T, store metadata.MetadataStore) {
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, email := range []string{" UNIQUE@example.test ", "unique@EXAMPLE.TEST"} {
		go func() {
			<-start
			_, err := store.CreateMaintainer(context.Background(), metadata.MaintainerFields{Name: "Example", Email: email})
			results <- err
		}()
	}
	close(start)
	success := 0
	for range 2 {
		if err := <-results; err == nil {
			success++
		} else {
			wantCode(t, err, metadata.DuplicateEmail)
		}
	}
	if success != 1 {
		t.Fatalf("uniqueness admitted %d writers", success)
	}
}

func creatorRace(t *testing.T, store metadata.MetadataStore) {
	ctx := context.Background()
	for i := range 20 {
		owner := maintainer(t, store, fmt.Sprintf("race%d", i))
		start := make(chan struct{})
		var app metadata.Application
		var createErr, deleteErr error
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			app, createErr = store.CreateApplication(ctx, metadata.CreateApplicationInput{
				ApplicationFields: applicationFields("Race"), MaintainerID: owner.ID,
			})
		}()
		go func() {
			defer group.Done()
			<-start
			deleteErr = store.DeleteMaintainer(ctx, owner.ID, []metadata.Revision{1})
		}()
		close(start)
		group.Wait()
		switch {
		case createErr == nil:
			wantCode(t, deleteErr, metadata.MaintainerInUse)
			_, err := store.GetMaintainer(ctx, app.MaintainerID)
			must(t, err)
			must(t, store.DeleteApplication(ctx, app.ID, []metadata.Revision{1}))
			must(t, store.DeleteMaintainer(ctx, owner.ID, []metadata.Revision{1}))
		case deleteErr == nil:
			wantCode(t, createErr, metadata.InvalidReference)
			page, err := store.ListApplications(ctx, metadata.ApplicationList{MaintainerID: owner.ID})
			must(t, err)
			if len(page.Items) != 0 {
				t.Fatal("orphaned application after maintainer deletion")
			}
		default:
			t.Fatalf("race had no valid winner: create %v, delete %v", createErr, deleteErr)
		}
	}
}

func lifecycle(t *testing.T, store metadata.MetadataStore) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wantCode(t, store.Ping(ctx), metadata.Unavailable)
	_, err := store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Cancelled", Email: "cancelled@example.test"})
	wantCode(t, err, metadata.Unavailable)
	page, err := store.ListMaintainers(context.Background(), metadata.MaintainerList{})
	must(t, err)
	if len(page.Items) != 0 {
		t.Fatal("cancelled write persisted")
	}
	must(t, store.Close())
	wantCode(t, store.Ping(context.Background()), metadata.Unavailable)
	_, err = store.GetMaintainer(context.Background(), metadata.MaintainerID(metadata.NewUUID()))
	wantCode(t, err, metadata.Unavailable)
}

func maintainer(t *testing.T, store metadata.MetadataStore, prefix string) metadata.Maintainer {
	t.Helper()
	record, err := store.CreateMaintainer(context.Background(), metadata.MaintainerFields{
		Name: "Example " + prefix, Email: prefix + "@example.test",
	})
	must(t, err)
	return record
}

func applicationFields(title string) metadata.ApplicationFields {
	return metadata.ApplicationFields{Title: title, Version: "1.0", Company: "Example Company"}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code metadata.ErrorCode) {
	t.Helper()
	var problem *metadata.Error
	if !errors.As(err, &problem) || problem.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}
