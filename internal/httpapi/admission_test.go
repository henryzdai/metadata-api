package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"metadata-api/internal/httpapi"
	"metadata-api/internal/metadata"
	"metadata-api/internal/storage/memory"
)

func TestOptionsRejectNilStoreAndUnboundedAdmission(t *testing.T) {
	store := newMemory(t)
	var typedNil *memory.Store
	for _, test := range []struct {
		store   metadata.MetadataStore
		timeout time.Duration
		limit   int
	}{
		{nil, time.Second, 1}, {typedNil, time.Second, 1},
		{store, 0, 1}, {store, -1, 1}, {store, time.Second, 0}, {store, time.Second, -1},
		{store, time.Second, 257}, {store, time.Second, math.MaxInt},
	} {
		if handler, err := httpapi.New(test.store, nil, httpapi.Options{
			RequestTimeout: test.timeout, MaxInFlight: test.limit,
		}); err == nil || handler != nil {
			t.Fatal("invalid options constructed a handler")
		}
	}
	for _, limit := range []int{1, httpapi.MaxInFlightLimit} {
		if _, err := httpapi.New(store, nil, httpapi.Options{RequestTimeout: time.Second, MaxInFlight: limit}); err != nil {
			t.Fatalf("finite supported limit %d failed: %v", limit, err)
		}
	}
}

func TestNormalAdmissionRejectsBeforeBodyReadsAndKeepsProbesAvailable(t *testing.T) {
	release := make(chan struct{})
	store := &admissionStore{MetadataStore: newMemory(t), entered: make(chan struct{}, 1), release: release}
	handler := boundedHandler(t, store, time.Second)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- request(handler, "GET", "/v1/maintainers/"+metadata.NewUUID(), "", "") }()
	awaitEntry(t, store.entered)
	body := &unreadBody{}
	rejected := httptest.NewRequest("POST", "/v1/maintainers", body)
	rejected.Header.Set("Content-Type", "application/json")
	response := serve(handler, rejected)
	assertOverload(t, response)
	if body.reads.Load() != 0 {
		t.Fatal("overload read an unadmitted body")
	}
	if store.getCalls.Load() != 1 {
		t.Fatal("rejected request reached storage")
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		assertStatus(t, request(handler, "GET", path, "", ""), http.StatusOK)
	}
	close(release)
	assertError(t, awaitResponse(t, result), 404, "not_found")
	assertError(t, request(handler, "GET", "/v1/maintainers/"+metadata.NewUUID(), "", ""), 404, "not_found")
	assertStatus(t, request(handler, "POST", "/v1/maintainers", `{"name":"Free","email":"free@example.test"}`, ""), 201)
}

func TestOverloadDeadlineFailureIsLoggedWithoutDetails(t *testing.T) {
	var logs bytes.Buffer
	release := make(chan struct{})
	store := &admissionStore{MetadataStore: newMemory(t), entered: make(chan struct{}, 1), release: release}
	handler, err := httpapi.New(store, slog.New(slog.NewJSONHandler(&logs, nil)),
		httpapi.Options{RequestTimeout: time.Minute, MaxInFlight: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "/v1/maintainers/"+metadata.NewUUID(), nil).WithContext(ctx)
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() { responses <- serve(handler, request) }()
	awaitEntry(t, store.entered)
	body := &unreadBody{}
	rejected := httptest.NewRequest("POST", "/v1/maintainers", body)
	rejected.Header.Set("Content-Type", "application/json")
	writer := &deadlineFailureResponse{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(writer, rejected)
	assertOverload(t, writer.ResponseRecorder)
	close(release)
	awaitResponse(t, responses)
	if writer.attempts != 1 || body.reads.Load() != 0 {
		t.Fatal("overload did not attempt to bound unread-body cleanup")
	}
	if !strings.Contains(logs.String(), "overload_read_deadline_failed") {
		t.Fatal("unavailable read-deadline protection was silent")
	}
	if strings.Contains(logs.String(), "private-deadline-error-details") {
		t.Fatal("deadline failure logging exposed unchecked error details")
	}
}

type deadlineFailureResponse struct {
	*httptest.ResponseRecorder
	attempts int
}

func (w *deadlineFailureResponse) SetReadDeadline(time.Time) error {
	w.attempts++
	return errors.New("private-deadline-error-details")
}

func TestProbeAdmissionHasExactlyFourSeparateSlots(t *testing.T) {
	release := make(chan struct{})
	store := &probeAdmissionStore{MetadataStore: newMemory(t), entered: make(chan struct{}, 4), release: release}
	handler := boundedHandler(t, store, time.Second)
	results := make(chan *httptest.ResponseRecorder, 4)
	for range 4 {
		go func() { results <- request(handler, "GET", "/readyz", "", "") }()
	}
	for range 4 {
		awaitEntry(t, store.entered)
	}
	for _, path := range []string{"/readyz", "/healthz"} {
		assertOverload(t, request(handler, "GET", path, "", ""))
	}
	assertStatus(t, request(handler, "GET", "/v1/maintainers", "", ""), 200)
	assertError(t, request(handler, "POST", "/healthz", "", ""), 405, "method_not_allowed")
	close(release)
	for range 4 {
		assertStatus(t, awaitResponse(t, results), 200)
	}
	assertStatus(t, request(handler, "GET", "/readyz", "", ""), 200)
	assertStatus(t, request(handler, "GET", "/healthz", "", ""), 200)
}

func TestAdmissionReleasesOnCancellationAndDrainDoesNotCancelAcceptedWork(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "drain"}[draining], func(t *testing.T) {
			release := make(chan struct{})
			store := &admissionStore{MetadataStore: newMemory(t), entered: make(chan struct{}, 1), release: release}
			handler := boundedHandler(t, store, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("GET", "/v1/maintainers/"+metadata.NewUUID(), nil).WithContext(ctx)
			results := make(chan *httptest.ResponseRecorder, 1)
			go func() { results <- serve(handler, req) }()
			awaitEntry(t, store.entered)
			if draining {
				handler.BeginDrain()
				assertError(t, request(handler, "GET", "/readyz", "", ""), 503, "unavailable")
				if ctx.Err() != nil {
					t.Fatal("drain canceled an accepted request")
				}
				close(release)
				assertError(t, awaitResponse(t, results), 404, "not_found")
			} else {
				cancel()
				assertError(t, awaitResponse(t, results), 503, "unavailable")
				close(release)
			}
			assertStatus(t, request(handler, "GET", "/v1/maintainers", "", ""), 200)
		})
	}
}

func TestAdmissionReleasesAfterStorageFailurePanicDeadlineAndAbort(t *testing.T) {
	for _, test := range []struct {
		name   string
		store  *faultStore
		status int
		code   string
	}{
		{"error", &faultStore{MetadataStore: newMemory(t), failure: errors.New("private failure")}, 500, "internal_error"},
		{"panic", &faultStore{MetadataStore: newMemory(t), panicGet: true}, 500, "internal_error"},
		{"deadline", &faultStore{MetadataStore: newMemory(t), waitForDeadline: true}, 503, "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := boundedHandler(t, test.store, 10*time.Millisecond)
			for range 2 {
				assertError(t, request(handler, "GET", "/v1/maintainers/"+metadata.NewUUID(), "", ""), test.status, test.code)
			}
			assertStatus(t, request(handler, "GET", "/v1/maintainers", "", ""), 200)
		})
	}
	t.Run("store abort", func(t *testing.T) {
		handler := boundedHandler(t, &abortStore{MetadataStore: newMemory(t)}, time.Second)
		for range 2 {
			recovered := recoverAbort(func() { request(handler, "GET", "/v1/maintainers/"+metadata.NewUUID(), "", "") })
			if recovered != http.ErrAbortHandler {
				t.Fatalf("abort was swallowed or slot leaked: %v", recovered)
			}
		}
		assertStatus(t, request(handler, "GET", "/v1/maintainers", "", ""), 200)
	})
	for _, path := range []string{"/v1/maintainers", "/healthz"} {
		t.Run("partial write "+path, func(t *testing.T) {
			handler := boundedHandler(t, newMemory(t), time.Second)
			for range 5 {
				recovered := recoverAbort(func() {
					handler.ServeHTTP(&panicAfterWrite{ResponseWriter: httptest.NewRecorder()}, httptest.NewRequest("GET", path, nil))
				})
				if recovered != http.ErrAbortHandler {
					t.Fatalf("partial-write panic did not release admission: %v", recovered)
				}
			}
			assertStatus(t, request(handler, "GET", path, "", ""), 200)
		})
	}
}

func TestResourceCapacityMapsTo503WithoutRetryTime(t *testing.T) {
	policy, err := metadata.NewStorePolicy(metadata.StoreLimits{MaxMaintainers: 1, MaxApplications: 1, MaxFieldBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	handler := boundedHandler(t, store, time.Second)
	first := request(handler, "POST", "/v1/maintainers", `{"name":"One","email":"one@example.test"}`, "")
	assertStatus(t, first, 201)
	response := request(handler, "POST", "/v1/maintainers", `{"name":"Two","email":"two@example.test"}`, "")
	assertError(t, response, 503, "resource_capacity_exceeded")
	if response.Header().Get("Retry-After") != "" {
		t.Fatal("resource capacity invented a retry time")
	}
}

func boundedHandler(t *testing.T, store metadata.MetadataStore, timeout time.Duration) *httpapi.Handler {
	t.Helper()
	handler, err := httpapi.New(store, nil, httpapi.Options{RequestTimeout: timeout, MaxInFlight: 1})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func assertOverload(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	assertError(t, response, 503, "overloaded")
	if response.Header().Get("Retry-After") != "1" || response.Header().Get("Connection") != "close" {
		t.Fatalf("overload headers = %#v", response.Header())
	}
}

func awaitEntry(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach the controlled store")
	}
}

func awaitResponse(t *testing.T, responses <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-responses:
		return response
	case <-time.After(2 * time.Second):
		t.Fatal("admitted request did not finish")
		return nil
	}
}

func recoverAbort(operation func()) (recovered any) {
	defer func() { recovered = recover() }()
	operation()
	return nil
}

type unreadBody struct{ reads atomic.Int32 }

func (b *unreadBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }
func (*unreadBody) Close() error               { return nil }

type admissionStore struct {
	metadata.MetadataStore
	entered  chan struct{}
	release  <-chan struct{}
	getCalls atomic.Int32
}

func (s *admissionStore) GetMaintainer(ctx context.Context, _ metadata.MaintainerID) (metadata.Maintainer, error) {
	s.getCalls.Add(1)
	s.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return metadata.Maintainer{}, metadata.WrapError(metadata.Unavailable, ctx.Err())
	case <-s.release:
		return metadata.Maintainer{}, metadata.NewError(metadata.NotFound, nil)
	}
}

type probeAdmissionStore struct {
	metadata.MetadataStore
	entered chan struct{}
	release <-chan struct{}
}

func (s *probeAdmissionStore) Ping(ctx context.Context) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return nil
	}
}

type abortStore struct{ metadata.MetadataStore }

func (*abortStore) GetMaintainer(context.Context, metadata.MaintainerID) (metadata.Maintainer, error) {
	panic(http.ErrAbortHandler)
}
