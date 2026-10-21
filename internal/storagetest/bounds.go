package storagetest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"metadata-api/internal/metadata"
)

func BoundsContract(t *testing.T, open func(*testing.T, metadata.StorePolicy) metadata.MetadataStore) {
	t.Helper()
	withLimits := func(t *testing.T, limits metadata.StoreLimits) metadata.MetadataStore {
		t.Helper()
		policy, err := metadata.NewStorePolicy(limits)
		must(t, err)
		store := open(t, policy)
		t.Cleanup(func() { must(t, store.Close()) })
		return store
	}
	t.Run("maintainer count and precedence", func(t *testing.T) {
		limits := metadata.DefaultStorePolicy().Limits()
		limits.MaxMaintainers = 1
		store := withLimits(t, limits)
		owner := maintainer(t, store, "one")
		_, err := store.CreateMaintainer(context.Background(), metadata.MaintainerFields{Name: "Other", Email: "other@example.test"})
		wantCode(t, err, metadata.ResourceCapacityExceeded)
		_, err = store.CreateMaintainer(context.Background(), owner.MaintainerFields)
		wantCode(t, err, metadata.DuplicateEmail)
		_, err = store.CreateMaintainer(context.Background(), metadata.MaintainerFields{})
		wantCode(t, err, metadata.ValidationFailed)
		updated, err := store.UpdateMaintainer(context.Background(), owner.ID, owner.MaintainerFields, []metadata.Revision{1})
		must(t, err)
		if updated.Revision != 2 {
			t.Fatal("count-full replacement did not preserve revision semantics")
		}
		must(t, store.DeleteMaintainer(context.Background(), owner.ID, []metadata.Revision{2}))
		maintainer(t, store, "other")
	})
	t.Run("application count and precedence", func(t *testing.T) {
		limits := metadata.DefaultStorePolicy().Limits()
		limits.MaxApplications = 1
		store := withLimits(t, limits)
		owner := maintainer(t, store, "owner")
		input := metadata.CreateApplicationInput{
			ApplicationFields: metadata.ApplicationFields{Title: "A", Version: "1", Company: "C"},
			MaintainerID:      owner.ID,
		}
		app, err := store.CreateApplication(context.Background(), input)
		must(t, err)
		_, err = store.CreateApplication(context.Background(), input)
		wantCode(t, err, metadata.ResourceCapacityExceeded)
		input.MaintainerID = metadata.MaintainerID(metadata.NewUUID())
		_, err = store.CreateApplication(context.Background(), input)
		wantCode(t, err, metadata.InvalidReference)
		input.Title = ""
		_, err = store.CreateApplication(context.Background(), input)
		wantCode(t, err, metadata.ValidationFailed)
		wantCode(t, store.DeleteMaintainer(context.Background(), owner.ID, nil), metadata.MaintainerInUse)
		updated, err := store.UpdateApplication(context.Background(), app.ID, app.ApplicationFields, []metadata.Revision{1})
		must(t, err)
		if updated.Revision != 2 {
			t.Fatal("count-full application replacement consumed an extra revision")
		}
		must(t, store.DeleteApplication(context.Background(), app.ID, []metadata.Revision{2}))
		input.Title, input.MaintainerID = "A", owner.ID
		_, err = store.CreateApplication(context.Background(), input)
		must(t, err)
	})
	t.Run("maintainer normalized bytes and nonmutating rejection", func(t *testing.T) {
		ctx := context.Background()
		firstFields := metadata.MaintainerFields{Name: strings.Repeat("é", 10), Email: "a@example.test"}
		secondFields := metadata.MaintainerFields{Name: "B", Email: "b@example.test"}
		limits := metadata.DefaultStorePolicy().Limits()
		limits.MaxFieldBytes = metadata.MaintainerFieldBytes(firstFields) + metadata.MaintainerFieldBytes(secondFields)
		store := withLimits(t, limits)
		padded := firstFields
		padded.Name, padded.Email = " "+padded.Name+" ", " A@EXAMPLE.TEST "
		first, err := store.CreateMaintainer(ctx, padded)
		must(t, err)
		second, err := store.CreateMaintainer(ctx, secondFields)
		must(t, err)
		growing := metadata.MaintainerFields{Name: first.Name + "x", Email: "z@example.test"}
		_, err = store.UpdateMaintainer(ctx, first.ID, growing, []metadata.Revision{1})
		wantCode(t, err, metadata.ResourceCapacityExceeded)
		got, err := store.GetMaintainer(ctx, first.ID)
		must(t, err)
		if got != first {
			t.Fatal("rejected replacement changed a field, timestamp or revision")
		}
		_, err = store.CreateMaintainer(ctx, firstFields)
		wantCode(t, err, metadata.DuplicateEmail)
		_, err = store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "C", Email: "c@example.test"})
		wantCode(t, err, metadata.ResourceCapacityExceeded)
		_, err = store.UpdateMaintainer(ctx, first.ID, growing, []metadata.Revision{999})
		wantCode(t, err, metadata.PreconditionFailed)
		growing.Email = second.Email
		_, err = store.UpdateMaintainer(ctx, first.ID, growing, nil)
		wantCode(t, err, metadata.DuplicateEmail)
		_, err = store.UpdateMaintainer(ctx, metadata.MaintainerID(metadata.NewUUID()), growing, nil)
		wantCode(t, err, metadata.NotFound)
		_, err = store.UpdateMaintainer(ctx, metadata.MaintainerID(metadata.NewUUID()), metadata.MaintainerFields{}, nil)
		wantCode(t, err, metadata.ValidationFailed)
		equal, err := store.UpdateMaintainer(ctx, first.ID, firstFields, []metadata.Revision{1})
		must(t, err)
		if equal.Revision != 2 || equal.CreatedAt != first.CreatedAt {
			t.Fatal("rejections consumed a revision or creation timestamp")
		}
		small, err := store.UpdateMaintainer(ctx, first.ID,
			metadata.MaintainerFields{Name: "A", Email: first.Email}, []metadata.Revision{2})
		must(t, err)
		if small.Revision != 3 {
			t.Fatal("shrinking replacement did not succeed")
		}
		freed, err := store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "Z", Email: "z@example.test"})
		must(t, err)
		must(t, store.DeleteMaintainer(ctx, freed.ID, []metadata.Revision{1}))
		must(t, store.DeleteMaintainer(ctx, first.ID, []metadata.Revision{3}))
		_, err = store.CreateMaintainer(ctx, firstFields)
		must(t, err)
	})
	t.Run("application normalized bytes growth shrink and delete", func(t *testing.T) {
		ctx := context.Background()
		ownerFields := metadata.MaintainerFields{Name: "O", Email: "o@example.test"}
		fields := metadata.ApplicationFields{Title: "T", Version: "1", Company: "C", Description: strings.Repeat("é", 10)}
		limits := metadata.DefaultStorePolicy().Limits()
		limits.MaxFieldBytes = metadata.MaintainerFieldBytes(ownerFields) +
			metadata.ApplicationFieldBytes(fields, metadata.MaintainerID(strings.Repeat("x", 36)))
		store := withLimits(t, limits)
		owner, err := store.CreateMaintainer(ctx, ownerFields)
		must(t, err)
		padded := fields
		padded.Title, padded.Description = " T ", " "+fields.Description+" "
		input := metadata.CreateApplicationInput{ApplicationFields: padded, MaintainerID: metadata.MaintainerID(" " + string(owner.ID) + " ")}
		app, err := store.CreateApplication(ctx, input)
		must(t, err)
		if app.ApplicationFields != fields || app.MaintainerID != owner.ID {
			t.Fatal("application quota did not use normalized fields")
		}
		_, err = store.CreateApplication(ctx, input)
		wantCode(t, err, metadata.ResourceCapacityExceeded)
		growing := fields
		growing.Description += "\x00"
		_, err = store.UpdateApplication(ctx, app.ID, growing, []metadata.Revision{1})
		wantCode(t, err, metadata.ResourceCapacityExceeded)
		got, err := store.GetApplication(ctx, app.ID)
		must(t, err)
		if got != app {
			t.Fatal("application capacity rejection changed the resource or creator")
		}
		_, err = store.UpdateApplication(ctx, app.ID, growing, []metadata.Revision{999})
		wantCode(t, err, metadata.PreconditionFailed)
		_, err = store.UpdateApplication(ctx, metadata.ApplicationID(metadata.NewUUID()), growing, nil)
		wantCode(t, err, metadata.NotFound)
		wantCode(t, store.DeleteMaintainer(ctx, owner.ID, nil), metadata.MaintainerInUse)
		equal, err := store.UpdateApplication(ctx, app.ID, fields, []metadata.Revision{1})
		must(t, err)
		if equal.Revision != 2 || equal.CreatedAt != app.CreatedAt {
			t.Fatal("failed application growth consumed a revision")
		}
		fields.Description = ""
		small, err := store.UpdateApplication(ctx, app.ID, fields, []metadata.Revision{2})
		must(t, err)
		if small.Revision != 3 || small.MaintainerID != owner.ID {
			t.Fatal("application shrink did not preserve creator and revision")
		}
		must(t, store.DeleteApplication(ctx, small.ID, []metadata.Revision{3}))
		_, err = store.CreateApplication(ctx, input)
		must(t, err)
	})
	for _, resource := range []string{"maintainer", "application"} {
		for _, bound := range []string{"count", "bytes"} {
			t.Run(resource+" last "+bound+" slot", func(t *testing.T) {
				ctx := context.Background()
				limits := metadata.DefaultStorePolicy().Limits()
				fields := metadata.ApplicationFields{Title: "T", Version: "1", Company: "C"}
				ownerFields := metadata.MaintainerFields{Name: "O", Email: "o@example.test"}
				if bound == "count" {
					if resource == "maintainer" {
						limits.MaxMaintainers = 1
					} else {
						limits.MaxApplications = 1
					}
				} else if resource == "maintainer" {
					limits.MaxFieldBytes = metadata.MaintainerFieldBytes(metadata.MaintainerFields{Name: "S", Email: "slot0@example.test"})
				} else {
					limits.MaxFieldBytes = metadata.MaintainerFieldBytes(ownerFields) +
						metadata.ApplicationFieldBytes(fields, metadata.MaintainerID(strings.Repeat("x", 36)))
				}
				store := withLimits(t, limits)
				var owner metadata.Maintainer
				if resource == "application" {
					var err error
					owner, err = store.CreateMaintainer(ctx, ownerFields)
					must(t, err)
				}
				start := make(chan struct{})
				results := make(chan error, 2)
				for i := range 2 {
					go func() {
						<-start
						var err error
						if resource == "maintainer" {
							_, err = store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "S", Email: fmt.Sprintf("slot%d@example.test", i)})
						} else {
							_, err = store.CreateApplication(ctx, metadata.CreateApplicationInput{ApplicationFields: fields, MaintainerID: owner.ID})
						}
						results <- err
					}()
				}
				close(start)
				success := 0
				for range 2 {
					if err := <-results; err == nil {
						success++
					} else {
						wantCode(t, err, metadata.ResourceCapacityExceeded)
					}
				}
				if success != 1 {
					t.Fatalf("last slot committed %d creates", success)
				}
				if resource == "maintainer" {
					page, err := store.ListMaintainers(ctx, metadata.MaintainerList{})
					must(t, err)
					if len(page.Items) != 1 {
						t.Fatalf("last slot retained %d maintainers", len(page.Items))
					}
					winner := page.Items[0]
					must(t, store.DeleteMaintainer(ctx, winner.ID, []metadata.Revision{1}))
					for _, email := range []string{"slot0@example.test", "slot1@example.test"} {
						recreated, err := store.CreateMaintainer(ctx, metadata.MaintainerFields{Name: "S", Email: email})
						must(t, err)
						must(t, store.DeleteMaintainer(ctx, recreated.ID, []metadata.Revision{1}))
					}
				} else {
					page, err := store.ListApplications(ctx, metadata.ApplicationList{})
					must(t, err)
					if len(page.Items) != 1 {
						t.Fatalf("last slot retained %d applications", len(page.Items))
					}
					wantCode(t, store.DeleteMaintainer(ctx, owner.ID, nil), metadata.MaintainerInUse)
					must(t, store.DeleteApplication(ctx, page.Items[0].ID, []metadata.Revision{1}))
					recreated, err := store.CreateApplication(ctx, metadata.CreateApplicationInput{ApplicationFields: fields, MaintainerID: owner.ID})
					must(t, err)
					must(t, store.DeleteApplication(ctx, recreated.ID, []metadata.Revision{1}))
					must(t, store.DeleteMaintainer(ctx, owner.ID, []metadata.Revision{1}))
				}
			})
		}
	}
	t.Run("escaped page envelopes and complete cursors", func(t *testing.T) {
		store := withLimits(t, metadata.DefaultStorePolicy().Limits())
		boundedPageTraversal(t, store)
	})
}

func boundedPageTraversal(t *testing.T, store metadata.MetadataStore) {
	t.Helper()
	ctx := context.Background()
	owner := maintainer(t, store, "pages")
	other := maintainer(t, store, "other-pages")
	expected := make(map[metadata.ApplicationID]metadata.Application)
	description := strings.Repeat("<>&\x00\"\\\u2028\u2029", 1000)
	for i := range 30 {
		creator := owner.ID
		if i%10 == 0 {
			creator = other.ID
		}
		record, err := store.CreateApplication(ctx, metadata.CreateApplicationInput{
			ApplicationFields: metadata.ApplicationFields{
				Title: fmt.Sprintf("Item %d", i), Version: "1", Company: "Example", Description: description,
			},
			MaintainerID: creator,
		})
		must(t, err)
		expected[record.ID] = record
	}
	for _, filter := range []metadata.MaintainerID{"", owner.ID} {
		seen := make(map[metadata.ApplicationID]bool)
		after := metadata.ApplicationID("")
		pages := 0
		for {
			page, err := store.ListApplications(ctx, metadata.ApplicationList{Limit: 100, After: after, MaintainerID: filter})
			must(t, err)
			data, err := json.Marshal(page)
			must(t, err)
			if int64(len(data)+1) > metadata.PageByteLimit || len(page.Items) == 0 {
				t.Fatalf("invalid bounded page size %d, items %d", len(data)+1, len(page.Items))
			}
			pages++
			for _, record := range page.Items {
				if seen[record.ID] || record.ID <= after || record != expected[record.ID] ||
					filter != "" && record.MaintainerID != filter {
					t.Fatalf("cursor repeated, skipped order, changed fields or ignored filtering for %s", record.ID)
				}
				seen[record.ID] = true
				after = record.ID
			}
			if page.NextAfter == "" {
				break
			}
			if page.NextAfter != string(after) || len(page.Items) >= 100 || pages > len(expected) {
				t.Fatal("byte-shortened page has an incorrect or non-progressing cursor")
			}
		}
		want := 0
		for id, record := range expected {
			if filter == "" || record.MaintainerID == filter {
				want++
				if !seen[id] {
					t.Fatalf("cursor silently skipped %s", id)
				}
			}
		}
		if len(seen) != want || pages < 2 {
			t.Fatalf("bounded traversal saw %d/%d records in %d pages", len(seen), want, pages)
		}
	}
}
