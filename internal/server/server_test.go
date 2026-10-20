package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"metadata-api/internal/server"
	"metadata-api/internal/testsupport"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := server.ParseConfig(nil, io.Discard)
	if err != nil || cfg.Listen != "127.0.0.1:8080" || cfg.Storage != "sqlite" ||
		cfg.DBPath != "data/metadata.db" || cfg.RequestTimeout != 5*time.Second || cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("defaults = %#v, %v", cfg, err)
	}
	for _, args := range [][]string{
		{"--storage=other"}, {"--listen="}, {"--db="}, {"--request-timeout=0s"},
		{"--shutdown-timeout=-1s"}, {"--storage=memory", "--db=records.db"}, {"extra"},
		{"--unknown"}, {"--request-timeout=invalid"},
	} {
		if _, err := server.ParseConfig(args, io.Discard); err == nil {
			t.Errorf("accepted invalid flags %v", args)
		}
	}
	var help bytes.Buffer
	if _, err := server.ParseConfig([]string{"--help"}, &help); !errors.Is(err, flag.ErrHelp) ||
		!strings.Contains(help.String(), "volatile") || !strings.Contains(help.String(), "filesystem path") {
		t.Fatalf("help = %q, %v", help.String(), err)
	}
}

func TestRealListenerLifecycleAndBackendSelection(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			cfg := server.Config{
				Listen: "127.0.0.1:0", Storage: backend,
				DBPath:         filepath.Join(testsupport.Workspace(t), "records.db"),
				RequestTimeout: time.Second, ShutdownTimeout: 2 * time.Second,
			}
			first := start(t, cfg)
			for _, path := range []string{"/healthz", "/readyz"} {
				response := first.do(t, "GET", path, "")
				if response.StatusCode != 200 {
					t.Fatalf("%s status = %d", path, response.StatusCode)
				}
				consume(t, response)
			}
			created := first.do(t, "POST", "/v1/maintainers", `{"name":"Runtime Example","email":"runtime@example.test"}`)
			if created.StatusCode != 201 || created.Header.Get("ETag") == "" {
				t.Fatalf("runtime create status = %d", created.StatusCode)
			}
			location := created.Header.Get("Location")
			consume(t, created)
			first.stop(t)
			second := start(t, cfg)
			read := second.do(t, "GET", location, "")
			want := 404
			if backend == "sqlite" {
				want = 200
			}
			if read.StatusCode != want {
				t.Fatalf("%s restarted read = %d, want %d", backend, read.StatusCode, want)
			}
			consume(t, read)
			second.stop(t)
		})
	}
}

func TestStartupFailuresHaveNoListeningEventOrFallback(t *testing.T) {
	base := server.Config{
		Listen: "127.0.0.1:0", Storage: "sqlite", RequestTimeout: time.Second, ShutdownTimeout: time.Second,
	}
	base.DBPath = testsupport.Workspace(t)
	var stdout, stderr bytes.Buffer
	if err := server.Run(context.Background(), base, &stdout, &stderr); err == nil || stdout.Len() != 0 {
		t.Fatalf("unusable SQLite path did not fail startup: stdout %q, error %v", stdout.String(), err)
	}
	base.Storage = "memory"
	base.Listen = "not-an-address"
	if err := server.Run(context.Background(), base, &stdout, &stderr); err == nil || stdout.Len() != 0 {
		t.Fatalf("invalid listen address did not fail startup: stdout %q, error %v", stdout.String(), err)
	}
	base.Storage = "unknown"
	if err := server.Run(context.Background(), base, &stdout, &stderr); err == nil {
		t.Fatal("unrecognized storage silently fell back")
	}
	base.Storage = "memory"
	base.Listen = "127.0.0.1:0"
	if err := server.Run(context.Background(), base, brokenWriter{}, &stderr); err == nil {
		t.Fatal("stdout failure was ignored")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	base.Listen = listener.Addr().String()
	if err := server.Run(context.Background(), base, &stdout, &stderr); err == nil || stdout.Len() != 0 {
		t.Fatal("occupied listener did not fail cleanly")
	}
}

func TestShutdownDrainsAnInFlightRequest(t *testing.T) {
	cfg := server.Config{
		Listen: "127.0.0.1:0", Storage: "memory",
		RequestTimeout: 2 * time.Second, ShutdownTimeout: time.Second,
	}
	service := start(t, cfg)
	body := `{"name":"Draining Example","email":"drain@example.test"}`
	connection, reader := beginBody(t, service, len(body))
	defer func() { _ = connection.Close() }()
	service.cancel()
	if _, err := io.WriteString(connection, body); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 201 {
		t.Fatalf("in-flight request was not drained: %d", response.StatusCode)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	service.stop(t)
}

func TestShutdownForcesClosedStalledRequestsAtTheBound(t *testing.T) {
	cfg := server.Config{
		Listen: "127.0.0.1:0", Storage: "memory",
		RequestTimeout: 2 * time.Second, ShutdownTimeout: 30 * time.Millisecond,
	}
	service := start(t, cfg)
	connection, _ := beginBody(t, service, 100)
	defer func() { _ = connection.Close() }()
	service.stopExpecting(t, "graceful shutdown deadline exceeded")
}

func TestListenerBoundsHeaders(t *testing.T) {
	cfg := server.Config{
		Listen: "127.0.0.1:0", Storage: "memory",
		RequestTimeout: 100 * time.Millisecond, ShutdownTimeout: time.Second,
	}
	service := start(t, cfg)
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(service.url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(connection, "GET /healthz HTTP/1.1\r\nHost: example.test\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(connection).ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("stalled headers were not closed by read-header timeout: %v", err)
	}
	_ = connection.Close()
	connection, err = net.DialTimeout("tcp", strings.TrimPrefix(service.url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(connection,
		"GET /healthz HTTP/1.1\r\nHost: example.test\r\nX-Large: "+strings.Repeat("x", 32*1024)+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("header size cap returned %d, want 431", response.StatusCode)
	}
}

func beginBody(t *testing.T, service *instance, length int) (net.Conn, *bufio.Reader) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(service.url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = fmt.Fprintf(connection,
		"POST /v1/maintainers HTTP/1.1\r\nHost: example.test\r\nContent-Type: application/json\r\nContent-Length: %d\r\nExpect: 100-continue\r\nConnection: close\r\n\r\n", length)
	if err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusContinue {
		_ = connection.Close()
		t.Fatalf("request did not reach the body-reading handler: %d", response.StatusCode)
	}
	_ = response.Body.Close()
	return connection, reader
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

type eventWriter struct {
	events chan []byte
}

func (w *eventWriter) Write(data []byte) (int, error) {
	w.events <- bytes.Clone(data)
	return len(data), nil
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *lockedBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

type instance struct {
	url    string
	client *http.Client
	cancel context.CancelFunc
	result chan error
	logs   *lockedBuffer
	once   sync.Once
}

func start(t *testing.T, cfg server.Config) *instance {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	output := &eventWriter{events: make(chan []byte, 2)}
	instance := &instance{
		client: &http.Client{Timeout: 2 * time.Second}, cancel: cancel,
		result: make(chan error, 1), logs: &lockedBuffer{},
	}
	go func() { instance.result <- server.Run(ctx, cfg, output, instance.logs) }()
	t.Cleanup(func() { instance.stop(t) })
	select {
	case data := <-output.events:
		var event struct {
			Event   string `json:"event"`
			Addr    string `json:"addr"`
			Storage string `json:"storage"`
			Durable bool   `json:"durable"`
		}
		if err := json.Unmarshal(data, &event); err != nil || event.Event != "listening" ||
			event.Storage != cfg.Storage || event.Durable != (cfg.Storage == "sqlite") ||
			!strings.HasPrefix(event.Addr, "127.0.0.1:") || strings.HasSuffix(event.Addr, ":0") {
			t.Fatalf("startup event = %s, error %v", data, err)
		}
		instance.url = "http://" + event.Addr
	case err := <-instance.result:
		instance.once.Do(cancel)
		t.Fatalf("runtime stopped before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not announce a listener")
	}
	return instance
}

func (s *instance) stop(t *testing.T) {
	t.Helper()
	s.stopExpecting(t, "")
}

func (s *instance) stopExpecting(t *testing.T, expected string) {
	t.Helper()
	s.once.Do(func() {
		s.client.CloseIdleConnections()
		s.cancel()
		select {
		case err := <-s.result:
			if expected == "" && err != nil || expected != "" && (err == nil || !strings.Contains(err.Error(), expected)) {
				t.Errorf("runtime shutdown: %v; expected %q", err, expected)
			}
		case <-time.After(3 * time.Second):
			t.Error("runtime did not honor shutdown bound")
		}
		if strings.Contains(s.logs.text(), "runtime@example.test") {
			t.Error("runtime log exposes payload")
		}
	})
}

func (s *instance) do(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, s.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if method == "POST" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(request)
	if err != nil {
		t.Fatal(fmt.Errorf("runtime request: %w", err))
	}
	return response
}

func consume(t *testing.T, response *http.Response) {
	t.Helper()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Error(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Error(err)
	}
}
