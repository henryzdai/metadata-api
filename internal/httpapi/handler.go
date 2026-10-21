// Package httpapi maps JSON requests to atomic metadata store operations.
// PUT responses omit validators because normalization and generated metadata
// transform the request representation. See RFC 9110 section 9.3.4.
package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"metadata-api/internal/metadata"
)

type Handler struct {
	store          metadata.MetadataStore
	logger         *slog.Logger
	requestTimeout time.Duration
	draining       atomic.Bool
	normal         chan struct{}
	probes         chan struct{}
}

const (
	DefaultMaxInFlight = 16
	MaxInFlightLimit   = 256
	probeSlots         = 4
)

type Options struct {
	RequestTimeout time.Duration
	MaxInFlight    int
}

func New(store metadata.MetadataStore, logger *slog.Logger, options Options) (*Handler, error) {
	if store == nil {
		return nil, errors.New("a metadata store is required")
	}
	value := reflect.ValueOf(store)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return nil, errors.New("a metadata store is required")
		}
	}
	if options.RequestTimeout <= 0 {
		return nil, errors.New("request timeout must be positive")
	}
	if options.MaxInFlight < 1 || options.MaxInFlight > MaxInFlightLimit {
		return nil, errors.New("max-inflight must be between 1 and 256")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Handler{
		store: store, logger: logger, requestTimeout: options.RequestTimeout,
		normal: make(chan struct{}, options.MaxInFlight), probes: make(chan struct{}, probeSlots),
	}, nil
}

func (h *Handler) BeginDrain() { h.draining.Store(true) }

type route struct {
	kind    string
	id      string
	pattern string
	allow   string
}

func matchRoute(path string) route {
	switch path {
	case "/healthz", "/readyz":
		return route{kind: path, pattern: path, allow: "GET"}
	case "/v1/maintainers":
		return route{kind: "m1", pattern: path, allow: "GET, POST"}
	case "/v1/applications":
		return route{kind: "a1", pattern: path, allow: "GET, POST"}
	}
	for _, resource := range []struct{ prefix, kind string }{
		{"/v1/maintainers/", "m1"}, {"/v1/applications/", "a1"},
	} {
		if strings.HasPrefix(path, resource.prefix) {
			id := strings.TrimPrefix(path, resource.prefix)
			if id != "" && !strings.ContainsRune(id, '/') {
				return route{kind: resource.kind, id: id, pattern: resource.prefix + "{id}", allow: "GET, PUT, DELETE"}
			}
		}
	}
	return route{pattern: "unmatched"}
}

type statusWriter struct {
	http.ResponseWriter
	status  int
	started bool
}

func (w *statusWriter) WriteHeader(status int) {
	if !w.started {
		w.started = true
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := metadata.NewUUID()
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	matched := matchRoute(r.URL.Path)
	var admitted chan struct{}
	defer func() {
		if admitted != nil {
			defer func() { <-admitted }()
		}
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				panic(http.ErrAbortHandler)
			}
			status := http.StatusInternalServerError
			if writer.started {
				status = writer.status
			}
			// Panic values can contain request data; retain the stack without logging the value.
			h.logger.Error("request_panic", "request_id", requestID, "method", r.Method,
				"route", matched.pattern, "status", status, "response_started", writer.started,
				"duration_ms", time.Since(start).Milliseconds(), "stack", string(debug.Stack()))
			if writer.started {
				// A partial response cannot be replaced with a complete JSON error.
				panic(http.ErrAbortHandler)
			}
			writeProblem(writer, fromError(metadata.NewError(metadata.InternalError, nil)))
		}
		h.logger.Info("request", "request_id", requestID, "method", r.Method,
			"route", matched.pattern, "status", writer.status, "duration_ms", time.Since(start).Milliseconds())
	}()
	ctx, cancel := context.WithTimeout(r.Context(), h.requestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	slots := h.normal
	if r.Method == http.MethodGet && (matched.kind == "/healthz" || matched.kind == "/readyz") {
		slots = h.probes
	}
	select {
	case slots <- struct{}{}:
		admitted = slots
	default:
		writer.Header().Set("Connection", "close")
		writer.Header().Set("Retry-After", "1")
		if r.Body != nil && r.Body != http.NoBody {
			// Connection: close alone still permits net/http's post-handler body
			// drain. Expire reads on this rejected connection, not admitted work.
			if err := http.NewResponseController(w).SetReadDeadline(time.Now()); err != nil {
				h.logger.Warn("overload_read_deadline_failed", "request_id", requestID, "route", matched.pattern)
			}
		}
		writeProblem(writer, &problem{
			status: http.StatusServiceUnavailable, code: "overloaded",
			message: "The server is handling its maximum number of requests.",
		})
		return
	}
	if matched.kind == "" {
		writeProblem(writer, &problem{
			status:  http.StatusNotFound,
			code:    "not_found",
			message: "The route was not found.",
		})
		return
	}
	if !methodAllowed(matched.allow, r.Method) {
		writer.Header().Set("Allow", matched.allow)
		writeProblem(writer, &problem{
			status:  http.StatusMethodNotAllowed,
			code:    "method_not_allowed",
			message: "The method is not supported for this route.",
		})
		return
	}
	if matched.id != "" && !metadata.ValidUUID(matched.id) {
		writeProblem(writer, fromError(metadata.NewError(metadata.InvalidID, nil)))
		return
	}
	isList := matched.id == "" && r.Method == http.MethodGet && (matched.kind == "m1" || matched.kind == "a1")
	if !isList {
		if problem := rejectQuery(r.URL.RawQuery); problem != nil {
			writeProblem(writer, problem)
			return
		}
	}
	switch matched.kind {
	case "/healthz":
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	case "/readyz":
		if h.draining.Load() || h.store.Ping(ctx) != nil {
			writeProblem(writer, fromError(metadata.NewError(metadata.Unavailable, nil)))
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	case "m1":
		h.maintainers(writer, r, matched)
	case "a1":
		h.applications(writer, r, matched)
	}
}

func methodAllowed(allow, method string) bool {
	for _, candidate := range strings.Split(allow, ", ") {
		if candidate == method {
			return true
		}
	}
	return false
}
