package server

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"metadata-api/internal/httpapi"
	"metadata-api/internal/metadata"
	"metadata-api/internal/storage"
)

type Config struct {
	Listen          string
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	MaxConnections  int
	MaxInFlight     int
	store           storage.PreparedStore
}

func ParseConfig(args []string, output io.Writer, catalog *storage.Registry) (Config, error) {
	if catalog == nil || catalog.DefaultName() == "" {
		return Config{}, errors.New("a prepared storage catalog is required")
	}
	var cfg Config
	flags := flag.NewFlagSet("metadata-server", flag.ContinueOnError)
	// flag includes raw argument values in parse errors, which may contain provider secrets.
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "TCP listen address; port 0 selects a free port")
	flags.DurationVar(&cfg.RequestTimeout, "request-timeout", 5*time.Second, "maximum request duration")
	flags.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", 10*time.Second, "graceful shutdown bound")
	flags.IntVar(&cfg.MaxConnections, "max-connections", 128, "maximum accepted connections (1-2048)")
	flags.IntVar(&cfg.MaxInFlight, "max-inflight", httpapi.DefaultMaxInFlight, "maximum normal HTTP handlers (1-256)")
	limits := metadata.DefaultStorePolicy().Limits()
	flags.IntVar(&limits.MaxMaintainers, "max-maintainers", limits.MaxMaintainers, "maximum stored maintainers (1-1000000)")
	flags.IntVar(&limits.MaxApplications, "max-applications", limits.MaxApplications, "maximum stored applications (1-1000000)")
	flags.Int64Var(&limits.MaxFieldBytes, "max-metadata-bytes", limits.MaxFieldBytes, "maximum normalized user-field bytes (1-1073741824)")
	descriptions := catalog.Descriptions()
	var choices []string
	for _, description := range descriptions {
		choices = append(choices, description.Name+" ("+description.Summary+")")
	}
	selected := flags.String("storage", catalog.DefaultName(), "storage provider: "+strings.Join(choices, ", "))
	options := storageOptions{values: make(storage.Values)}
	flags.Var(&options, "storage-option", "provider option key=value; repeat for distinct keys")
	var aliases []*storageAlias
	for _, description := range descriptions {
		for _, option := range description.Options {
			if option.FlagAlias == "" {
				continue
			}
			if flags.Lookup(option.FlagAlias) != nil || option.FlagAlias == "help" || option.FlagAlias == "h" {
				return Config{}, errors.New("storage option alias conflicts with a server flag")
			}
			alias := &storageAlias{
				provider: description.Name, key: option.Key, flag: option.FlagAlias, value: option.Default,
			}
			flags.Var(alias, option.FlagAlias, option.Help)
			aliases = append(aliases, alias)
		}
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return Config{}, flag.ErrHelp
		}
		return Config{}, errors.New("invalid command-line options; use --help for usage")
	}
	if flags.NArg() != 0 {
		return Config{}, errors.New("positional arguments are not supported")
	}
	for _, alias := range aliases {
		if !alias.set {
			continue
		}
		if alias.provider != *selected {
			return Config{}, fmt.Errorf("--%s is only supported with --storage=%s", alias.flag, alias.provider)
		}
		if _, exists := options.values[alias.key]; exists {
			return Config{}, fmt.Errorf("storage option %q is set by both --%s and --storage-option", alias.key, alias.flag)
		}
		options.values[alias.key] = alias.value
	}
	policy, err := metadata.NewStorePolicy(limits)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.validateRuntime(); err != nil {
		return Config{}, err
	}
	plan, err := catalog.Prepare(*selected, options.values, policy)
	if err != nil {
		return Config{}, err
	}
	cfg.store = plan
	return cfg, cfg.validate()
}

func (cfg Config) validate() error {
	if cfg.store.Name() == "" {
		return errors.New("storage must be prepared by ParseConfig")
	}
	return cfg.validateRuntime()
}

func (cfg Config) validateRuntime() error {
	if cfg.Listen == "" {
		return errors.New("--listen must not be empty")
	}
	if cfg.RequestTimeout <= 0 || cfg.ShutdownTimeout <= 0 {
		return errors.New("request and shutdown timeouts must be positive")
	}
	if cfg.MaxConnections < 1 || cfg.MaxConnections > 2048 {
		return errors.New("max-connections must be between 1 and 2048")
	}
	if cfg.MaxInFlight < 1 || cfg.MaxInFlight > httpapi.MaxInFlightLimit {
		return errors.New("max-inflight must be between 1 and 256")
	}
	return nil
}

type storageOptions struct {
	values storage.Values
}

func (*storageOptions) String() string { return "" }

func (o *storageOptions) Set(argument string) error {
	key, value, ok := strings.Cut(argument, "=")
	if !ok || key == "" {
		return errors.New("storage options must use key=value")
	}
	if _, exists := o.values[key]; exists {
		return errors.New("duplicate storage option key")
	}
	o.values[key] = value
	return nil
}

type storageAlias struct {
	provider string
	key      string
	flag     string
	value    string
	set      bool
}

func (a *storageAlias) String() string { return a.value }

func (a *storageAlias) Set(value string) error {
	if a.set {
		return errors.New("duplicate storage option alias")
	}
	a.value, a.set = value, true
	return nil
}
