package server_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metadata-api/internal/access"
	"metadata-api/internal/metadata"
	"metadata-api/internal/server"
	"metadata-api/internal/storage"
	"metadata-api/internal/storage/builtin"
	"metadata-api/internal/testsupport"
	"metadata-api/internal/testsupport/probe"
)

type runtimeCredentials struct {
	path     string
	id       string
	writer   string
	replaced string
	reader   string
	hashes   []string
	data     []byte
}

func privateCredentials(t *testing.T) runtimeCredentials {
	t.Helper()
	result := runtimeCredentials{id: metadata.NewUUID()}
	for range 3 {
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			t.Fatal("generate test credential")
		}
		token := base64.RawURLEncoding.EncodeToString(random[:])
		digest := sha256.Sum256([]byte(token))
		result.hashes = append(result.hashes, hex.EncodeToString(digest[:]))
		switch len(result.hashes) {
		case 1:
			result.writer = token
		case 2:
			result.replaced = token
		case 3:
			result.reader = token
		}
	}
	result.data = []byte(fmt.Sprintf(`{"principals":[
		{"id":%q,"role":"write","token_sha256":[%q,%q]},
		{"id":%q,"role":"read","token_sha256":[%q]}
	]}`, result.id, result.hashes[0], result.hashes[1], metadata.NewUUID(), result.hashes[2]))
	result.path = filepath.Join(testsupport.Workspace(t), "private-auth.json")
	if err := os.WriteFile(result.path, result.data, 0o600); err != nil {
		t.Fatal("write test credentials")
	}
	return result
}

func authenticated(t *testing.T, service *instance, method, path, body, token string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, service.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal("construct runtime request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPost || method == http.MethodPut {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := service.client.Do(request)
	if err != nil {
		t.Fatal("authenticated runtime request failed")
	}
	return response
}

func TestInvalidSecurityConfigurationNeverPreparesOrOpensStorage(t *testing.T) {
	credentials := privateCredentials(t)
	directory := testsupport.Workspace(t)
	badPath := filepath.Join(directory, "private-malformed-auth.json")
	oversizedPath := filepath.Join(directory, "private-oversized-auth.json")
	ambiguous := strings.Replace(string(credentials.data), `"role":"write"`, `"role":"write","role":"read"`, 1)
	if err := os.WriteFile(badPath, []byte(ambiguous), 0o600); err != nil {
		t.Fatal("write malformed test credentials")
	}
	if err := os.WriteFile(oversizedPath, bytes.Repeat([]byte(" "), access.MaxFileBytes+1), 0o600); err != nil {
		t.Fatal("write oversized test credentials")
	}
	recorder := &probe.Recorder{}
	provider := recorder.Provider()
	preparations := 0
	prepare := provider.Prepare
	provider.Prepare = func(values storage.Values, policy metadata.StorePolicy) (storage.OpenFunc, error) {
		preparations++
		return prepare(values, policy)
	}
	catalog, err := storage.NewRegistry(provider.Name, provider)
	if err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{"--auth-mode=private-unsupported-value"},
		{"--auth-mode=Tokens", "--auth-file=" + credentials.path},
		{"--auth-mode=tokens"},
		{"--auth-mode=tokens", "--auth-file="},
		{"--auth-mode=local", "--auth-file="},
		{"--auth-file=" + credentials.path},
		{"--auth-mode=local", "--auth-file=" + credentials.path},
		{"--auth-mode=local", "--auth-mode=tokens", "--auth-file=" + credentials.path},
		{"--auth-mode=tokens", "--auth-file=" + credentials.path, "--auth-file=" + credentials.path},
		{"--auth-mode=tokens", "--auth-file=" + badPath},
		{"--auth-mode=tokens", "--auth-file=" + oversizedPath},
		{"--auth-mode=tokens", "--auth-file=" + directory},
		{"--auth-mode=tokens", "--auth-file=" + filepath.Join(directory, "private-missing-file")},
	}
	for _, listen := range []string{
		"", ":0", "0.0.0.0:0", "[::]:0", "192.0.2.1:0", "private-host.invalid:0",
		"localhost.example:0", "127.0.0.1:http", "127.0.0.1:65536", "user@localhost:0",
	} {
		cases = append(cases, []string{"--listen=" + listen, "--auth-mode=tokens", "--auth-file=" + credentials.path})
	}
	for index, args := range cases {
		var output bytes.Buffer
		if _, err := server.ParseConfig(args, &output, catalog); err == nil {
			t.Fatalf("invalid security configuration case %d was accepted", index)
		} else {
			for _, secret := range append([]string{
				"private-unsupported-value", "private-host.invalid", credentials.path,
				badPath, oversizedPath, credentials.writer, credentials.reader, string(credentials.data), ambiguous,
			}, credentials.hashes...) {
				if strings.Contains(err.Error()+output.String(), secret) {
					t.Fatalf("security configuration case %d exposed unchecked data", index)
				}
			}
		}
	}
	if preparations != 0 || len(recorder.Opens()) != 0 {
		t.Fatal("invalid security configuration reached storage preparation or initialization")
	}
	for _, path := range []string{badPath, oversizedPath} {
		db := filepath.Join(directory, "must-not-create", "records.db")
		_, err := server.ParseConfig([]string{
			"--listen=127.0.0.1:0", "--db=" + db, "--auth-mode=tokens", "--auth-file=" + path,
		}, io.Discard, builtinCatalog(t))
		if err == nil {
			t.Fatal("invalid token file prepared a SQLite runtime")
		}
		if _, err := os.Stat(filepath.Dir(db)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid token configuration created SQLite state")
		}
	}
}

func TestRunRevalidatesAlteredListenerBeforeOpeningStorage(t *testing.T) {
	for _, listen := range []string{
		":0", "0.0.0.0:0", "[::]:0", "192.0.2.1:0", "private-host.invalid:0", "localhost:65536",
	} {
		recorder := &probe.Recorder{}
		catalog, err := builtin.NewRegistry(recorder.Provider())
		if err != nil {
			t.Fatal(err)
		}
		cfg := parseWithCatalog(t, catalog, "--storage=probe", "--listen=127.0.0.1:0")
		cfg.Listen = listen
		var stdout, stderr bytes.Buffer
		if err := server.Run(context.Background(), cfg, &stdout, &stderr); err == nil ||
			len(recorder.Opens()) != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatal("altered unsafe listener reached storage or listening side effects")
		}
	}
}

func TestStartupCapturesOneCredentialSnapshotAndRestartAppliesRevocation(t *testing.T) {
	credentials := privateCredentials(t)
	recorder := &probe.Recorder{}
	catalog, err := builtin.NewRegistry(recorder.Provider())
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--storage=probe", "--listen=localhost:0",
		"--auth-mode=tokens", "--auth-file=" + credentials.path}
	cfg := parseWithCatalog(t, catalog, args...)
	if len(recorder.Opens()) != 0 {
		t.Fatal("loading authentication initialized storage")
	}
	if err := os.Remove(credentials.path); err != nil {
		t.Fatal("credential loader did not close its file")
	}
	service := start(t, cfg, "probe", false)
	for _, token := range []string{credentials.writer, credentials.replaced, credentials.reader} {
		response := authenticated(t, service, "GET", "/v1/maintainers", "", token)
		requireStatus(t, response, 200)
		consume(t, response)
	}
	downgraded := []byte(fmt.Sprintf(
		`{"principals":[{"id":%q,"role":"read","token_sha256":[%q]}]}`,
		credentials.id, credentials.hashes[1]))
	if err := os.WriteFile(credentials.path, downgraded, 0o600); err != nil {
		t.Fatal("replace test credentials")
	}
	response := authenticated(t, service, "POST", "/v1/maintainers",
		`{"name":"Snapshot","email":"snapshot@example.test"}`, credentials.writer)
	requireStatus(t, response, 201)
	consume(t, response)
	service.stop(t)
	restarted := start(t, parseWithCatalog(t, catalog, args...), "probe", false)
	for _, token := range []string{credentials.writer, credentials.reader} {
		assertResponseCode(t, authenticated(t, restarted, "GET", "/v1/maintainers", "", token), 401, "unauthenticated")
	}
	response = authenticated(t, restarted, "GET", "/v1/maintainers", "", credentials.replaced)
	requireStatus(t, response, 200)
	consume(t, response)
	assertResponseCode(t, authenticated(t, restarted, "POST", "/v1/maintainers",
		`{"name":"Denied","email":"denied@example.test"}`, credentials.replaced), 403, "forbidden")
	restarted.stop(t)
	if len(recorder.Opens()) != 2 || recorder.Closes() != 2 {
		t.Fatal("credential snapshots disrupted store lifecycle")
	}
	for _, secret := range append([]string{
		credentials.writer, credentials.replaced, credentials.reader, string(credentials.data), string(downgraded),
	}, credentials.hashes...) {
		if strings.Contains(service.logs.text()+restarted.logs.text(), secret) {
			t.Fatal("runtime logs exposed credential material")
		}
	}
}

func TestLocalListenerAuthorityUsesTheActualBoundPort(t *testing.T) {
	for _, listen := range []string{"localhost:0", "127.0.0.1:0", "[::1]:0"} {
		t.Run(listen, func(t *testing.T) {
			if strings.HasPrefix(listen, "[") {
				check, err := net.Listen("tcp", listen)
				if err != nil {
					t.Skip("IPv6 loopback listener is not available")
				}
				if err := check.Close(); err != nil {
					t.Fatal(err)
				}
			}
			service := start(t, parse(t, "--storage=memory", "--listen="+listen), "memory", false)
			_, port, err := net.SplitHostPort(strings.TrimPrefix(service.url, "http://"))
			if err != nil {
				t.Fatal("invalid startup authority")
			}
			for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port} {
				request, err := http.NewRequest("GET", service.url+"/v1/maintainers", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Host = host
				request.Header.Set("X-Forwarded-Host", "private-host.invalid:"+port)
				response, err := service.client.Do(request)
				if err != nil {
					t.Fatal("valid local Host failed")
				}
				requireStatus(t, response, 200)
				if response.Header.Get("Cache-Control") != "no-store" {
					t.Fatal("metadata response can be cached")
				}
				consume(t, response)
			}
			for _, host := range []string{"private-host.invalid:" + port, "localhost:0", "user@localhost:" + port} {
				request, err := http.NewRequest("GET", service.url+"/healthz", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Host = host
				request.Header.Set("Forwarded", "host=localhost:"+port)
				request.Header.Set("X-Forwarded-Host", "localhost:"+port)
				response, err := service.client.Do(request)
				if err != nil {
					t.Fatal("rejected Host request did not receive an HTTP error")
				}
				assertResponseCode(t, response, 400, "invalid_host")
			}
			service.stop(t)
		})
	}
}

func TestAccessRejectionsCloseUnreadBodiesWithoutContinueOrPostHandlerDrain(t *testing.T) {
	credentials := privateCredentials(t)
	for _, test := range []struct {
		name, mode, authorization string
		badHost                   bool
		status                    int
		code                      string
	}{
		{"missing", "tokens", "", false, 401, "unauthenticated"},
		{"invalid", "tokens", "Bearer invalid", false, 401, "unauthenticated"},
		{"reader", "tokens", "Bearer " + credentials.reader, false, 403, "forbidden"},
		{"local", "local", "Bearer " + credentials.writer, false, 400, "authentication_not_enabled"},
		{"authority", "tokens", "", true, 400, "invalid_host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"--storage=memory", "--listen=127.0.0.1:0", "--request-timeout=5s", "--auth-mode=" + test.mode}
			if test.mode == "tokens" {
				args = append(args, "--auth-file="+credentials.path)
			}
			service := start(t, parse(t, args...), "memory", false)
			for _, expectContinue := range []bool{false, true} {
				connection, err := net.DialTimeout("tcp", strings.TrimPrefix(service.url, "http://"), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				func() {
					defer func() { _ = connection.Close() }()
					if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
						t.Fatal(err)
					}
					host := strings.TrimPrefix(service.url, "http://")
					if test.badHost {
						host = "private-host.invalid:8080"
					}
					headers := fmt.Sprintf("POST /v1/maintainers HTTP/1.1\r\nHost: %s\r\n"+
						"Content-Type: application/json\r\nContent-Length: 100\r\n", host)
					if test.authorization != "" {
						headers += "Authorization: " + test.authorization + "\r\n"
					}
					if expectContinue {
						headers += "Expect: 100-continue\r\n"
					}
					if _, err := io.WriteString(connection, headers+"\r\n"); err != nil {
						t.Fatal("send headers for unread-body test")
					}
					reader := bufio.NewReader(connection)
					response, err := http.ReadResponse(reader, nil)
					if err != nil {
						t.Fatal("access rejection did not respond before its body arrived")
					}
					if response.StatusCode == http.StatusContinue || !response.Close {
						_ = response.Body.Close()
						t.Fatal("access rejection consumed a body or retained its connection")
					}
					assertResponseCode(t, response, test.status, test.code)
					if _, err := reader.ReadByte(); err == nil {
						t.Fatal("access rejection left its unread connection open")
					} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
						t.Fatal("access rejection waited for the socket timeout to close")
					}
				}()
			}
			for _, path := range []string{"/healthz", "/readyz"} {
				response := service.do(t, "GET", path, "")
				requireStatus(t, response, 200)
				consume(t, response)
			}
			service.stop(t)
		})
	}
}

func TestOutputFailureClosesTheBoundAuthorityAndOpenedStore(t *testing.T) {
	recorder := &probe.Recorder{}
	catalog, err := builtin.NewRegistry(recorder.Provider())
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseWithCatalog(t, catalog, "--storage=probe", "--listen=localhost:0")
	output := &failedEventWriter{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Run(ctx, cfg, output, io.Discard); err == nil || strings.Contains(err.Error(), "private-output-error") {
		t.Fatal("startup output failure was ignored or exposed unchecked details")
	}
	if len(recorder.Opens()) != 1 || recorder.Closes() != 1 {
		t.Fatal("startup output failure leaked the opened store")
	}
	var event struct {
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal(output.data, &event); err != nil || event.Addr == "" {
		t.Fatal("test did not observe the bound startup authority")
	}
	rebound, err := net.Listen("tcp", event.Addr)
	if err != nil {
		t.Fatal("startup output failure leaked its listener")
	}
	if err := rebound.Close(); err != nil {
		t.Fatal(err)
	}
}

type failedEventWriter struct{ data []byte }

func (w *failedEventWriter) Write(data []byte) (int, error) {
	w.data = bytes.Clone(data)
	return 0, errors.New("private-output-error")
}
