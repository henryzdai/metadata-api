package builtin_test

import (
	"context"
	"reflect"
	"testing"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
	"metadata-api/internal/storage/builtin"
	"metadata-api/internal/testsupport/probe"
)

func TestStockCatalogAndExplicitExtensions(t *testing.T) {
	recorder := &probe.Recorder{}
	catalog, err := builtin.NewRegistry(recorder.Provider())
	if err != nil {
		t.Fatal(err)
	}
	if catalog.DefaultName() != "sqlite" {
		t.Fatalf("default provider = %q", catalog.DefaultName())
	}
	descriptions := catalog.Descriptions()
	if len(descriptions) != 3 {
		t.Fatalf("catalog contains %d providers", len(descriptions))
	}
	durability := make(map[string]bool)
	for _, description := range descriptions {
		durability[description.Name] = description.Durable
		if description.Name == "sqlite" {
			if !reflect.DeepEqual(description.Options, []storage.OptionSpec{{
				Key: "path", Default: "data/metadata.db", FlagAlias: "db",
				Help: "SQLite database filesystem path, not a DSN",
			}}) {
				t.Fatalf("SQLite options = %#v", description.Options)
			}
		}
		if description.Name == "memory" && len(description.Options) != 0 {
			t.Fatal("memory advertises storage options")
		}
	}
	if !reflect.DeepEqual(durability, map[string]bool{"sqlite": true, "memory": false, "probe": false}) {
		t.Fatalf("catalog durability = %v", durability)
	}
	plan, err := catalog.Prepare("probe", storage.Values{"probe-number": "19"}, metadata.DefaultStorePolicy())
	if err != nil {
		t.Fatal(err)
	}
	store, err := plan.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorder.Opens(), []int{19}) || recorder.Closes() != 1 {
		t.Fatal("explicit extension did not use its provider")
	}
}

func TestExtensionsCannotReplaceBuiltins(t *testing.T) {
	for _, name := range []string{"sqlite", "memory"} {
		t.Run(name, func(t *testing.T) {
			provider := (&probe.Recorder{}).Provider()
			provider.Name = name
			if catalog, err := builtin.NewRegistry(provider); err == nil || catalog != nil {
				t.Fatalf("extension replaced a built-in provider: %v, %v", catalog, err)
			}
		})
	}
}
