package e2e

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSQLiteContentionIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contended.db")
	s := startServer(t, "sqlite", path)
	db, err := sql.Open("sqlite", sqliteFileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				t.Error(err)
			}
		}
	}()
	body := []byte(`{"name":"Contended","email":"contended@example.test"}`)
	start := time.Now()
	s.request(http.MethodPost, "/v1/maintainers", body, nil).wantError(t, 503, "unavailable")
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("contended request exceeded its bound: %s", elapsed)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	locked = false
	items := decode[page](t, s.get(t, "/v1/maintainers"))
	if len(items.Items) != 0 {
		t.Fatal("the rejected contended request wrote a record")
	}
	s.request(http.MethodPost, "/v1/maintainers", body, nil).want(t, 201)
	s.get(t, "/readyz")
}

func sqliteFileURI(path string) string {
	p := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func TestDatabasePathIsNotADSN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state #readonly&_foreign_keys=off.db")
	s := startServer(t, "sqlite", path)
	initial := s.maintainer(t, "Owner", "owner@example.test")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the exact requested database filename was not created: %v", err)
	}
	s.stop(t, false)
	reopened := startServer(t, "sqlite", path)
	got := reopened.get(t, "/v1/maintainers/"+decode[record](t, initial).ID)
	if got.header.Get("ETag") != initial.header.Get("ETag") {
		t.Fatal("the escaped database path did not preserve its data")
	}
}

func TestStartupErrorsNeverFallback(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown backend", []string{"--storage", "not-a-backend"}},
		{"unopenable SQLite", []string{"--storage", "sqlite", "--db", t.TempDir()}},
		{"unknown storage option", []string{"--storage-option", "unknown=value"}},
		{"malformed storage option", []string{"--storage-option", "path"}},
		{"duplicate storage option", []string{"--storage-option", "path=one.db", "--storage-option", "path=two.db"}},
		{"alias and generic conflict", []string{"--db", "one.db", "--storage-option", "path=one.db"}},
		{"memory rejects db alias", []string{"--storage", "memory", "--db", "unused.db"}},
		{"zero request timeout", []string{"--storage", "memory", "--request-timeout", "0s"}},
		{"negative shutdown timeout", []string{"--storage", "memory", "--shutdown-timeout", "-1s"}},
		{"zero connection limit", []string{"--max-connections=0"}},
		{"excess connection limit", []string{"--max-connections=2049"}},
		{"zero request admission", []string{"--max-inflight=0"}},
		{"excess request admission", []string{"--max-inflight=257"}},
		{"zero maintainer limit", []string{"--max-maintainers=0"}},
		{"excess maintainer limit", []string{"--max-maintainers=1000001"}},
		{"negative application limit", []string{"--max-applications=-1"}},
		{"excess application limit", []string{"--max-applications=1000001"}},
		{"zero field byte limit", []string{"--max-metadata-bytes=0"}},
		{"excess field byte limit", []string{"--max-metadata-bytes=1073741825"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			args := append([]string{"--listen", "127.0.0.1:0"}, test.args...)
			cmd := exec.CommandContext(ctx, serverBinary, args...)
			cmd.Dir = t.TempDir()
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("invalid startup did not fail promptly: %s", output)
			}
			if err == nil || strings.Contains(string(output), `"event":"listening"`) {
				t.Fatalf("invalid startup succeeded or silently fell back: %v %s", err, output)
			}
		})
	}
}

func TestLogsCorrelateWithoutPayloads(t *testing.T) {
	s := startServer(t, "sqlite", filepath.Join(t.TempDir(), "metadata.db"))
	created := s.maintainer(t, "Do Not Log This Name", "do-not-log@example.test")
	requestID := created.header.Get("X-Request-ID")
	clientChosen := "client-chosen-id-not-to-trust"
	body := []byte(`{"name":"Other","email":"other@example.test"}`)
	response := s.request(http.MethodPost, "/v1/maintainers", body,
		http.Header{"X-Request-ID": []string{clientChosen}}).want(t, 201)
	if response.header.Get("X-Request-ID") == clientChosen {
		t.Fatal("the server trusted a client-selected request ID")
	}
	s.request(http.MethodGet, "/v1/maintainers?secret=not-for-logs", nil, nil).
		wantError(t, 400, "invalid_query")
	s.stop(t, false)
	data, err := os.ReadFile(s.stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, requestID) || !strings.Contains(log, `"method"`) {
		t.Fatal("access logs are missing request correlation")
	}
	for _, forbidden := range []string{
		"Do Not Log This Name", "do-not-log@example.test",
		clientChosen, "not-for-logs",
	} {
		if strings.Contains(log, forbidden) {
			t.Errorf("access log contains client content %q", forbidden)
		}
	}
}

func TestGracefulShutdownDrainsAnInFlightRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go cannot deliver SIGTERM on Windows; the Linux CI job runs this real-process check")
	}
	database := filepath.Join(t.TempDir(), "drain.db")
	s := startServer(t, "sqlite", database)
	body := `{"name":"Drain","email":"drain@example.test"}`
	request := holdPost(t, s, body)
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForDrain(t, s)
	created := request.finish(t)
	select {
	case <-s.exited:
		s.stopped = true
		if s.waitErr != nil {
			t.Fatalf("graceful exit failed: %v", s.waitErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not finish graceful shutdown")
	}
	reopened := startServer(t, "sqlite", database)
	reopened.get(t, "/v1/maintainers/"+decode[record](t, created).ID)
}

func waitForDrain(t *testing.T, s *server) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.exited:
			t.Fatal("server exited while an accepted request was incomplete")
		case <-timer.C:
			t.Fatal("server did not stop accepting requests during shutdown")
		case <-ticker.C:
			result := s.request(http.MethodGet, "/readyz", nil, nil)
			if result.err != nil || result.status == http.StatusServiceUnavailable {
				return
			}
		}
	}
}
