package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

var serverBinary string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) (code int) {
	serverBinary = os.Getenv("METADATA_SERVER_BIN")
	if serverBinary == "" {
		dir, err := os.MkdirTemp("", "metadata-e2e-build-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() {
			if err := os.RemoveAll(dir); err != nil {
				fmt.Fprintln(os.Stderr, "remove E2E build directory:", err)
				code = 1
			}
		}()
		serverBinary = filepath.Join(dir, "metadata-server")
		if runtime.GOOS == "windows" {
			serverBinary += ".exe"
		}
		build := exec.Command("go", "build", "-o", serverBinary, "../cmd/metadata-server")
		if output, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build real E2E server: %v\n%s\n", err, output)
			return 1
		}
	} else {
		absolute, err := filepath.Abs(serverBinary)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		serverBinary = absolute
	}
	if info, err := os.Stat(serverBinary); err != nil || !info.Mode().IsRegular() {
		fmt.Fprintf(os.Stderr, "E2E executable is unavailable: %s (%v)\n", serverBinary, err)
		return 1
	}
	return m.Run()
}

type server struct {
	cmd       *exec.Cmd
	exited    chan struct{}
	waitErr   error
	baseURL   string
	client    *http.Client
	stdout    *os.File
	stderr    *os.File
	directory string
	stopped   bool
}

type response struct {
	status int
	header http.Header
	body   []byte
	err    error
}

type record struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Title        string `json:"title"`
	Version      string `json:"version"`
	Company      string `json:"company"`
	Website      string `json:"website"`
	Source       string `json:"source"`
	License      string `json:"license"`
	Description  string `json:"description"`
	MaintainerID string `json:"maintainer_id"`
	Revision     int64  `json:"revision"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type page struct {
	Items     []record `json:"items"`
	NextAfter string   `json:"next_after"`
}

type apiError struct {
	Error struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func startServer(t *testing.T, backend, database string) *server {
	t.Helper()
	root := os.Getenv("E2E_ARTIFACTS_DIR")
	if root == "" {
		root = t.TempDir()
	} else if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, strings.ReplaceAll(t.Name(), "/", "_")+"-")
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := os.Create(filepath.Join(dir, "stdout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr.jsonl"))
	if err != nil {
		stdout.Close()
		t.Fatal(err)
	}
	args := []string{
		"--listen", "127.0.0.1:0", "--storage", backend,
		"--shutdown-timeout", "3s",
	}
	if backend == "sqlite" && database != "" {
		args = append(args, "--db", database)
	}
	cmd := exec.Command(serverBinary, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	s := &server{
		cmd: cmd, exited: make(chan struct{}), stdout: stdout, stderr: stderr,
		directory: dir,
		client: &http.Client{
			Timeout: 8 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns: 32, MaxIdleConnsPerHost: 16,
			},
		},
	}
	if err := cmd.Start(); err != nil {
		stdout.Close()
		stderr.Close()
		t.Fatalf("start server: %v", err)
	}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.exited)
	}()
	t.Cleanup(func() {
		if !s.stopped {
			s.stop(t, false)
		}
		s.client.CloseIdleConnections()
		if err := stdout.Close(); err != nil {
			t.Error(err)
		}
		if err := stderr.Close(); err != nil {
			t.Error(err)
		}
		if t.Failed() {
			log, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Logf("cannot read server log: %v", err)
			} else {
				t.Logf("server log (%s):\n%s", dir, log)
			}
		}
	})
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.exited:
			t.Fatalf("server exited before readiness: %v; logs in %s", s.waitErr, dir)
		case <-timer.C:
			t.Fatalf("server did not become ready; logs in %s", dir)
		case <-ticker.C:
			data, err := os.ReadFile(stdout.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range bytes.Split(data, []byte("\n")) {
				var event struct {
					Event   string `json:"event"`
					Addr    string `json:"addr"`
					Storage string `json:"storage"`
					Durable *bool  `json:"durable"`
				}
				if json.Unmarshal(line, &event) == nil && event.Event == "listening" {
					if event.Storage != backend || event.Durable == nil || *event.Durable != (backend == "sqlite") {
						t.Fatalf("incorrect backend announcement: %s", line)
					}
					s.baseURL = "http://" + event.Addr
				}
			}
			if s.baseURL != "" {
				result := s.request(http.MethodGet, "/readyz", nil, nil)
				if result.err == nil && result.status == http.StatusOK {
					return s
				}
			}
		}
	}
}

func (s *server) stop(t *testing.T, graceful bool) {
	t.Helper()
	if s.stopped {
		return
	}
	select {
	case <-s.exited:
		s.stopped = true
		if s.waitErr != nil {
			t.Errorf("server exited unexpectedly: %v", s.waitErr)
		}
		return
	default:
	}
	var err error
	if graceful {
		err = s.cmd.Process.Signal(syscall.SIGTERM)
	} else {
		err = s.cmd.Process.Kill()
	}
	if err != nil {
		t.Errorf("stop server: %v", err)
	}
	select {
	case <-s.exited:
		s.stopped = true
		if graceful && s.waitErr != nil {
			t.Errorf("graceful shutdown returned %v", s.waitErr)
		}
	case <-time.After(6 * time.Second):
		if err := s.cmd.Process.Kill(); err != nil {
			t.Errorf("kill server after shutdown timeout: %v", err)
		}
		select {
		case <-s.exited:
			s.stopped = true
		case <-time.After(3 * time.Second):
			t.Error("server did not exit after kill")
		}
		t.Error("server exceeded its shutdown bound")
	}
}

func (s *server) request(method, path string, body []byte, headers http.Header) response {
	req, err := http.NewRequest(method, s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return response{err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		req.Header[key] = append([]string(nil), values...)
	}
	result, err := s.client.Do(req)
	if err != nil {
		return response{err: err}
	}
	defer result.Body.Close()
	data, err := io.ReadAll(io.LimitReader(result.Body, 4<<20))
	return response{status: result.StatusCode, header: result.Header, body: data, err: err}
}

func (r response) want(t *testing.T, status int) response {
	t.Helper()
	if r.err != nil {
		t.Fatalf("request failed: %v", r.err)
	}
	if r.status != status {
		t.Fatalf("HTTP %d, want %d: %s", r.status, status, r.body)
	}
	if status != http.StatusNoContent && !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Errorf("unexpected Content-Type %q", r.header.Get("Content-Type"))
	}
	if r.header.Get("X-Request-ID") == "" {
		t.Error("missing X-Request-ID")
	}
	if status == http.StatusNoContent && len(r.body) != 0 {
		t.Errorf("204 response contains a body: %s", r.body)
	}
	return r
}

func (r response) wantError(t *testing.T, status int, code string) {
	t.Helper()
	r.want(t, status)
	value := decode[apiError](t, r)
	if value.Error.Code != code || value.Error.Message == "" {
		t.Errorf("unexpected error: %s", r.body)
	}
	if value.RequestID == "" || value.RequestID != r.header.Get("X-Request-ID") {
		t.Errorf("error request ID does not match its header: %s", r.body)
	}
}

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var value T
	decoder := json.NewDecoder(bytes.NewReader(r.body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("invalid response JSON: %v: %s", err, r.body)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("response contains trailing data: %v: %s", err, r.body)
	}
	return value
}

func jsonBody(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func condition(tag string) http.Header {
	return http.Header{"If-Match": []string{tag}}
}

func (s *server) get(t *testing.T, path string) response {
	t.Helper()
	return s.request(http.MethodGet, path, nil, nil).want(t, http.StatusOK)
}

func (s *server) maintainer(t *testing.T, name, email string) response {
	t.Helper()
	body := jsonBody(t, map[string]string{"name": name, "email": email})
	result := s.request(http.MethodPost, "/v1/maintainers", body, nil).want(t, http.StatusCreated)
	checkCreated(t, result, "/v1/maintainers/", "m1")
	return result
}

func (s *server) application(t *testing.T, creator, title string) response {
	t.Helper()
	body := jsonBody(t, map[string]string{
		"title": title, "version": "1.0", "company": "Example",
		"maintainer_id": creator, "website": "https://example.test",
		"source": "https://example.test/source", "license": "Example license",
		"description": "A fictional test application.",
	})
	result := s.request(http.MethodPost, "/v1/applications", body, nil).want(t, http.StatusCreated)
	checkCreated(t, result, "/v1/applications/", "a1")
	return result
}

func checkCreated(t *testing.T, result response, prefix, tagKind string) {
	t.Helper()
	value := decode[record](t, result)
	if value.Revision != 1 || value.ID == "" {
		t.Errorf("invalid new record: %s", result.body)
	}
	if result.header.Get("Location") != prefix+value.ID {
		t.Errorf("unexpected Location %q", result.header.Get("Location"))
	}
	if result.header.Get("ETag") != fmt.Sprintf(`"%s:%s:1"`, tagKind, value.ID) {
		t.Errorf("unexpected create ETag %q", result.header.Get("ETag"))
	}
	for _, timestamp := range []string{value.CreatedAt, value.UpdatedAt} {
		if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil || !strings.HasSuffix(timestamp, "Z") {
			t.Errorf("timestamp is not UTC RFC3339: %q", timestamp)
		}
	}
}

func assertNoPUTValidator(t *testing.T, result response) {
	t.Helper()
	if result.header.Get("ETag") != "" || result.header.Get("Last-Modified") != "" {
		t.Errorf("transformed PUT returned a forbidden validator: %v", result.header)
	}
}
