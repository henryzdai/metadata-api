package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"metadata-api/internal/httpapi"
)

func Run(ctx context.Context, cfg Config, stdout, stderr io.Writer) (runErr error) {
	if err := cfg.validate(); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	startupCtx, startupCancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	store, err := cfg.store.Open(startupCtx)
	startupCancel()
	if err != nil {
		return fmt.Errorf("initialize storage: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil && runErr == nil {
			runErr = errors.New("could not close storage")
		}
	}()
	handler, err := httpapi.New(store, logger, httpapi.Options{
		RequestTimeout: cfg.RequestTimeout, MaxInFlight: cfg.MaxInFlight,
	})
	if err != nil {
		return err
	}
	tcpListener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	listener := &connectionListener{Listener: tcpListener, slots: make(chan struct{}, cfg.MaxConnections)}
	defer func() { _ = listener.Close() }()
	// The shutdown signal must not cancel in-flight requests before they can drain.
	serverCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	writeTimeout := cfg.RequestTimeout
	if writeTimeout < time.Duration(1<<63-1)-time.Second {
		writeTimeout += time.Second
	}
	service := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: min(cfg.RequestTimeout, 5*time.Second),
		ReadTimeout:       cfg.RequestTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
		BaseContext:       func(net.Listener) context.Context { return serverCtx },
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	if err := json.NewEncoder(stdout).Encode(struct {
		Event   string `json:"event"`
		Addr    string `json:"addr"`
		Storage string `json:"storage"`
		Durable bool   `json:"durable"`
	}{
		Event:   "listening",
		Addr:    listener.Addr().String(),
		Storage: cfg.store.Name(),
		Durable: cfg.store.Durable(),
	}); err != nil {
		return errors.New("cannot write listening event")
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- service.Serve(listener) }()
	select {
	case err := <-serveResult:
		handler.BeginDrain()
		cancelRequests()
		_ = service.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("HTTP listener stopped unexpectedly")
	case <-ctx.Done():
		handler.BeginDrain()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		err := service.Shutdown(shutdownCtx)
		shutdownCancel()
		if err != nil {
			cancelRequests()
			_ = service.Close()
		}
		<-serveResult
		if err != nil {
			return errors.New("graceful shutdown deadline exceeded")
		}
		return nil
	}
}
