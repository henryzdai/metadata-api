package storage_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
	"metadata-api/internal/storage/memory"
	"metadata-api/internal/testsupport/probe"
)

func TestRegistryRejectsInvalidCatalogs(t *testing.T) {
	valid := memory.Provider()
	option := storage.OptionSpec{Key: "path", FlagAlias: "db"}
	for _, test := range []struct {
		name        string
		defaultName string
		providers   []storage.Provider
	}{
		{"empty catalog", "memory", nil},
		{"empty default", "", []storage.Provider{valid}},
		{"missing default", "missing", []storage.Provider{valid}},
		{"duplicate provider", "memory", []storage.Provider{valid, valid}},
		{"missing preparation", "empty", []storage.Provider{{Name: "empty"}}},
		{"empty provider name", "", []storage.Provider{{Prepare: valid.Prepare}}},
		{"uppercase provider", "Memory", []storage.Provider{{Name: "Memory", Prepare: valid.Prepare}}},
		{"numeric provider", "1memory", []storage.Provider{{Name: "1memory", Prepare: valid.Prepare}}},
		{"provider whitespace", " memory", []storage.Provider{{Name: " memory", Prepare: valid.Prepare}}},
		{"provider punctuation", "memory=x", []storage.Provider{{Name: "memory=x", Prepare: valid.Prepare}}},
		{"trailing hyphen", "memory-", []storage.Provider{{Name: "memory-", Prepare: valid.Prepare}}},
		{"double hyphen", "memory--test", []storage.Provider{{Name: "memory--test", Prepare: valid.Prepare}}},
		{"empty option key", "memory", []storage.Provider{{Name: "memory", Prepare: valid.Prepare, Options: []storage.OptionSpec{{}}}}},
		{"invalid option key", "memory", []storage.Provider{{Name: "memory", Prepare: valid.Prepare, Options: []storage.OptionSpec{{Key: "path=value"}}}}},
		{"duplicate option key", "memory", []storage.Provider{{Name: "memory", Prepare: valid.Prepare, Options: []storage.OptionSpec{option, option}}}},
		{"invalid alias", "memory", []storage.Provider{{Name: "memory", Prepare: valid.Prepare, Options: []storage.OptionSpec{{Key: "path", FlagAlias: "-db"}}}}},
		{"duplicate local alias", "memory", []storage.Provider{{Name: "memory", Prepare: valid.Prepare, Options: []storage.OptionSpec{option, {Key: "other", FlagAlias: "db"}}}}},
		{"duplicate catalog alias", "memory", []storage.Provider{
			{Name: "memory", Prepare: valid.Prepare, Options: []storage.OptionSpec{option}},
			{Name: "other", Prepare: valid.Prepare, Options: []storage.OptionSpec{option}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if registry, err := storage.NewRegistry(test.defaultName, test.providers...); err == nil || registry != nil {
				t.Fatalf("invalid catalog returned %v, %v", registry, err)
			}
		})
	}
	first, second := valid, valid
	first.Options = []storage.OptionSpec{{Key: "shared-key"}}
	second.Name = "memory-2"
	second.Options = []storage.OptionSpec{{Key: "shared-key"}}
	if _, err := storage.NewRegistry("memory-2", first, second); err != nil {
		t.Fatalf("provider-local option keys cannot be shared: %v", err)
	}
}

func TestRegistryCopiesMetadataOptionsAndPreparedValues(t *testing.T) {
	recorder := &probe.Recorder{}
	provider := recorder.Provider()
	provider.Durable = true
	original := storage.Description{
		Name: provider.Name, Summary: provider.Summary, Durable: provider.Durable,
		Options: append([]storage.OptionSpec(nil), provider.Options...),
	}

	providers := []storage.Provider{provider}
	registry, err := storage.NewRegistry(provider.Name, providers...)
	if err != nil {
		t.Fatal(err)
	}
	provider.Options[0].Default = "91"
	providers[0].Name, providers[0].Durable, providers[0].Prepare = "changed", false, nil
	descriptions := registry.Descriptions()
	descriptions[0].Name, descriptions[0].Durable = "changed-again", false
	descriptions[0].Options[0].Key, descriptions[0].Options[0].Default = "wrong", "92"
	if got := registry.Descriptions(); !reflect.DeepEqual(got, []storage.Description{original}) {
		t.Fatalf("catalog metadata changed through caller-owned values: %#v", got)
	}
	if registry.DefaultName() != original.Name {
		t.Fatal("default name changed")
	}
	supplied := storage.Values{"probe-number": "23"}
	explicit, err := registry.Prepare(original.Name, supplied, metadata.DefaultStorePolicy())
	if err != nil {
		t.Fatal(err)
	}
	supplied["probe-number"] = "93"
	defaulted, err := registry.Prepare(original.Name, nil, metadata.DefaultStorePolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range []storage.PreparedStore{explicit, defaulted} {
		if plan.Name() != original.Name || !plan.Durable() {
			t.Fatal("prepared metadata differs from the selected provider")
		}
		store, err := plan.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := recorder.Opens(); !reflect.DeepEqual(got, []int{23, 7}) || recorder.Closes() != 2 {
		t.Fatalf("typed captured options or lifecycle = %v opens, %d closes", got, recorder.Closes())
	}
}

func TestRegistryDoesNotShareValuesWithPreparation(t *testing.T) {
	var received []string
	provider := storage.Provider{
		Name: "mutating", Options: []storage.OptionSpec{{Key: "label", Default: "original"}},
		Prepare: func(values storage.Values, policy metadata.StorePolicy) (storage.OpenFunc, error) {
			received = append(received, values["label"])
			values["label"] = "mutated"
			delete(values, "label")
			return memory.Provider().Prepare(nil, policy)
		},
	}
	registry, err := storage.NewRegistry(provider.Name, provider)
	if err != nil {
		t.Fatal(err)
	}
	supplied := storage.Values{"label": "supplied"}
	for _, values := range []storage.Values{supplied, nil, nil} {
		if _, err := registry.Prepare(provider.Name, values, metadata.DefaultStorePolicy()); err != nil {
			t.Fatal(err)
		}
	}
	if supplied["label"] != "supplied" || !reflect.DeepEqual(received, []string{"supplied", "original", "original"}) {
		t.Fatalf("preparation shares caller or catalog values: supplied %v, received %v", supplied, received)
	}
}

func TestRegistryRejectsInvalidPreparationWithoutExposingValues(t *testing.T) {
	secret := "private-option-value"
	cause := errors.New(secret)
	called := 0
	provider := storage.Provider{
		Name: "rejecting", Options: []storage.OptionSpec{{Key: "secret"}},
		Prepare: func(storage.Values, metadata.StorePolicy) (storage.OpenFunc, error) {
			called++
			return nil, cause
		},
	}
	registry, err := storage.NewRegistry(provider.Name, provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		values storage.Values
	}{
		{secret, nil},
		{provider.Name, storage.Values{secret: secret}},
	} {
		if _, err := registry.Prepare(test.name, test.values, metadata.DefaultStorePolicy()); err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("selection or option error = %v", err)
		}
	}
	if called != 0 {
		t.Fatal("invalid selection or unknown options reached preparation")
	}
	if _, err := registry.Prepare(provider.Name, storage.Values{"secret": secret}, metadata.DefaultStorePolicy()); !errors.Is(err, cause) ||
		strings.Contains(err.Error(), secret) {
		t.Fatalf("preparation failure lost its cause or exposed values: %v", err)
	}
	provider.Prepare = func(storage.Values, metadata.StorePolicy) (storage.OpenFunc, error) { return nil, nil }
	registry, err = storage.NewRegistry(provider.Name, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Prepare(provider.Name, nil, metadata.DefaultStorePolicy()); err == nil {
		t.Fatal("accepted a nil prepared opener")
	}
}

func TestPreparedStoreRejectsInvalidOpenResults(t *testing.T) {
	cause := errors.New("private-open-value")
	partial, err := memory.New(metadata.DefaultStorePolicy())
	if err != nil {
		t.Fatal(err)
	}
	var typedNil *memory.Store
	for _, test := range []struct {
		name string
		open storage.OpenFunc
		want error
	}{
		{"nil store", func(context.Context) (metadata.MetadataStore, error) { return nil, nil }, nil},
		{"typed nil store", func(context.Context) (metadata.MetadataStore, error) { return typedNil, nil }, nil},
		{"factory error", func(context.Context) (metadata.MetadataStore, error) { return nil, cause }, cause},
		{"partial initialization", func(context.Context) (metadata.MetadataStore, error) { return partial, cause }, cause},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := storage.Provider{
				Name:    "invalid",
				Prepare: func(storage.Values, metadata.StorePolicy) (storage.OpenFunc, error) { return test.open, nil },
			}
			registry, err := storage.NewRegistry(provider.Name, provider)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := registry.Prepare(provider.Name, nil, metadata.DefaultStorePolicy())
			if err != nil {
				t.Fatal(err)
			}
			store, err := plan.Open(context.Background())
			if store != nil || err == nil || strings.Contains(err.Error(), cause.Error()) ||
				test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("invalid open result = %v, %v", store, err)
			}
		})
	}
	if err := partial.Ping(context.Background()); err == nil {
		t.Fatal("partial initialization leaked an open store")
	}
	if _, err := (storage.PreparedStore{}).Open(context.Background()); err == nil {
		t.Fatal("zero plan opened a default store")
	}
	var absent *storage.Registry
	if absent.DefaultName() != "" || len(absent.Descriptions()) != 0 {
		t.Fatal("nil registry exposes a default catalog")
	}
	if _, err := absent.Prepare("memory", nil, metadata.DefaultStorePolicy()); err == nil {
		t.Fatal("nil registry prepared a store")
	}
}
