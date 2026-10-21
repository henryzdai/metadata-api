package storage

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"metadata-api/internal/metadata"
)

type Values map[string]string

type OpenFunc func(context.Context) (metadata.MetadataStore, error)

type OptionSpec struct {
	Key       string
	Default   string
	Help      string
	FlagAlias string
}

type Provider struct {
	Name    string
	Summary string
	Durable bool
	Options []OptionSpec
	Prepare func(Values, metadata.StorePolicy) (OpenFunc, error)
}

type Description struct {
	Name    string
	Summary string
	Durable bool
	Options []OptionSpec
}

type Registry struct {
	defaultName string
	providers   map[string]Provider
	names       []string
}

func NewRegistry(defaultName string, providers ...Provider) (*Registry, error) {
	registry := &Registry{
		defaultName: defaultName,
		providers:   make(map[string]Provider, len(providers)),
	}
	aliases := make(map[string]bool)
	for _, provider := range providers {
		if !validIdentifier(provider.Name) {
			return nil, errors.New("storage provider names must be lowercase identifiers")
		}
		if _, exists := registry.providers[provider.Name]; exists {
			return nil, fmt.Errorf("duplicate storage provider %q", provider.Name)
		}
		if provider.Prepare == nil {
			return nil, fmt.Errorf("storage provider %q has no preparation function", provider.Name)
		}
		keys := make(map[string]bool, len(provider.Options))
		for _, option := range provider.Options {
			if !validIdentifier(option.Key) || keys[option.Key] {
				return nil, fmt.Errorf("storage provider %q has invalid or duplicate option keys", provider.Name)
			}
			keys[option.Key] = true
			if option.FlagAlias != "" {
				if !validIdentifier(option.FlagAlias) || aliases[option.FlagAlias] {
					return nil, errors.New("storage option flag aliases must be valid and unique")
				}
				aliases[option.FlagAlias] = true
			}
		}
		provider.Options = append([]OptionSpec(nil), provider.Options...)
		registry.providers[provider.Name] = provider
		registry.names = append(registry.names, provider.Name)
	}
	if _, exists := registry.providers[defaultName]; !exists {
		return nil, errors.New("default storage provider is not registered")
	}
	return registry, nil
}

func (r *Registry) DefaultName() string {
	if r == nil {
		return ""
	}
	return r.defaultName
}

func (r *Registry) Descriptions() []Description {
	if r == nil {
		return nil
	}
	descriptions := make([]Description, 0, len(r.names))
	for _, name := range r.names {
		provider := r.providers[name]
		descriptions = append(descriptions, Description{
			Name: provider.Name, Summary: provider.Summary, Durable: provider.Durable,
			Options: append([]OptionSpec(nil), provider.Options...),
		})
	}
	return descriptions
}

func (r *Registry) Prepare(name string, supplied Values, policy metadata.StorePolicy) (PreparedStore, error) {
	if r == nil {
		return PreparedStore{}, errors.New("storage catalog is unavailable")
	}
	if err := policy.Validate(); err != nil {
		return PreparedStore{}, err
	}
	provider, exists := r.providers[name]
	if !exists {
		return PreparedStore{}, errors.New("unknown storage provider")
	}
	values := make(Values, len(provider.Options))
	for _, option := range provider.Options {
		values[option.Key] = option.Default
	}
	for key, value := range supplied {
		if _, exists := values[key]; !exists {
			return PreparedStore{}, fmt.Errorf("unknown option for storage provider %q", name)
		}
		values[key] = value
	}
	open, err := provider.Prepare(values, policy)
	if err != nil {
		return PreparedStore{}, &providerFailure{name: name, operation: "preparation", err: err}
	}
	if open == nil {
		return PreparedStore{}, fmt.Errorf("storage provider %q returned no opener", name)
	}
	return PreparedStore{
		name: provider.Name, durable: provider.Durable, open: open,
	}, nil
}

type PreparedStore struct {
	name    string
	durable bool
	open    OpenFunc
}

func (p PreparedStore) Name() string  { return p.name }
func (p PreparedStore) Durable() bool { return p.durable }

func (p PreparedStore) Open(ctx context.Context) (metadata.MetadataStore, error) {
	if p.name == "" || p.open == nil {
		return nil, errors.New("storage plan is not prepared")
	}
	store, err := p.open(ctx)
	if err != nil {
		if !nilStore(store) {
			err = errors.Join(err, store.Close())
		}
		return nil, &providerFailure{name: p.name, operation: "open", err: err}
	}
	if nilStore(store) {
		return nil, fmt.Errorf("storage provider %q returned a nil store", p.name)
	}
	return store, nil
}

type providerFailure struct {
	name      string
	operation string
	err       error
}

func (e *providerFailure) Error() string {
	return fmt.Sprintf("storage provider %q %s failed", e.name, e.operation)
}

func (e *providerFailure) Unwrap() error { return e.err }

func nilStore(store metadata.MetadataStore) bool {
	if store == nil {
		return true
	}
	value := reflect.ValueOf(store)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func validIdentifier(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		char := name[i]
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		if char != '-' || i == len(name)-1 || name[i-1] == '-' {
			return false
		}
	}
	return true
}
