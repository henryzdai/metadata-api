package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"metadata-api/internal/metadata"
	"metadata-api/internal/testsupport"
)

func TestCancellationRollsBackReservedUsageAndResourceTogether(t *testing.T) {
	for _, insertResource := range []bool{false, true} {
		t.Run(map[bool]string{false: "before row", true: "after row"}[insertResource], func(t *testing.T) {
			path := filepath.Join(testsupport.Workspace(t), "records.db")
			fields := metadata.MaintainerFields{Name: "Owner", Email: "owner@example.test"}
			policy, err := metadata.NewStorePolicy(metadata.StoreLimits{
				MaxMaintainers: 1, MaxApplications: 1, MaxFieldBytes: metadata.MaintainerFieldBytes(fields),
			})
			if err != nil {
				t.Fatal(err)
			}
			store, err := Open(context.Background(), path, policy)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err = store.write(ctx, func(tx *sql.Tx) error {
				if err := store.adjustUsage(ctx, tx, 1, 0, 0, metadata.MaintainerFieldBytes(fields)); err != nil {
					return err
				}
				if insertResource {
					now := time.Now().UTC().Format(time.RFC3339Nano)
					if _, err := tx.ExecContext(ctx, "INSERT INTO maintainers ("+maintainerColumns+") VALUES (?, ?, ?, ?, ?, ?)",
						metadata.NewUUID(), fields.Name, fields.Email, 1, now, now); err != nil {
						return err
					}
				}
				cancel()
				return ctx.Err()
			})
			assertCode(t, err, metadata.Unavailable)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause was lost: %v", err)
			}
			if got := storedUsage(t, store); got != (usage{}) {
				t.Fatalf("canceled transaction retained usage %#v", got)
			}
			page, err := store.ListMaintainers(context.Background(), metadata.MaintainerList{})
			if err != nil || len(page.Items) != 0 {
				t.Fatal("canceled transaction retained a resource")
			}
			if _, err := store.CreateMaintainer(context.Background(), fields); err != nil {
				t.Fatalf("cancellation consumed the last byte, count or email slot: %v", err)
			}
			if got := storedUsage(t, store); got != (usage{maintainers: 1, fieldBytes: metadata.MaintainerFieldBytes(fields)}) {
				t.Fatalf("post-cancellation usage = %#v", got)
			}
		})
	}
}
