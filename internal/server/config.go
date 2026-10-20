package server

import (
	"errors"
	"flag"
	"io"
	"time"
)

type Config struct {
	Listen          string
	Storage         string
	DBPath          string
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
}

func ParseConfig(args []string, output io.Writer) (Config, error) {
	var cfg Config
	flags := flag.NewFlagSet("metadata-server", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "TCP listen address; port 0 selects a free port")
	flags.StringVar(&cfg.Storage, "storage", "sqlite", "storage backend: sqlite (durable) or memory (volatile)")
	flags.StringVar(&cfg.DBPath, "db", "data/metadata.db", "SQLite database filesystem path, not a DSN")
	flags.DurationVar(&cfg.RequestTimeout, "request-timeout", 5*time.Second, "maximum request duration")
	flags.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", 10*time.Second, "graceful shutdown bound")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, errors.New("positional arguments are not supported")
	}
	explicitDB := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "db" {
			explicitDB = true
		}
	})
	if cfg.Storage == "memory" && explicitDB {
		return Config{}, errors.New("--db is only supported with --storage=sqlite")
	}
	return cfg, cfg.validate()
}

func (cfg Config) validate() error {
	if cfg.Storage != "sqlite" && cfg.Storage != "memory" {
		return errors.New("--storage must be sqlite or memory")
	}
	if cfg.Listen == "" {
		return errors.New("--listen must not be empty")
	}
	if cfg.Storage == "sqlite" && cfg.DBPath == "" {
		return errors.New("--db must not be empty for SQLite")
	}
	if cfg.RequestTimeout <= 0 || cfg.ShutdownTimeout <= 0 {
		return errors.New("request and shutdown timeouts must be positive")
	}
	return nil
}
