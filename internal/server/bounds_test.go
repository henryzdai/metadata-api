package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"metadata-api/internal/metadata"
	"metadata-api/internal/server"
	"metadata-api/internal/storage"
	"metadata-api/internal/storage/builtin"
	"metadata-api/internal/testsupport"
	"metadata-api/internal/testsupport/probe"
)

func TestFiniteLimitsAreValidatedBeforeProviderPreparation(t *testing.T) {
	recorder := &probe.Recorder{}
	provider := recorder.Provider()
	prepare := provider.Prepare
	preparations := 0
	provider.Prepare = func(values storage.Values, policy metadata.StorePolicy) (storage.OpenFunc, error) {
		preparations++
		return prepare(values, policy)
	}
	catalog, err := builtin.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []struct {
		name string
		over string
	}{
		{"max-connections", "2049"}, {"max-inflight", "257"},
		{"max-maintainers", "1000001"}, {"max-applications", "1000001"},
		{"max-metadata-bytes", "1073741825"},
	} {
		for _, value := range []string{"0", "-1", flag.over, "9223372036854775808", "NaN", "Inf", "1.5"} {
			t.Run(flag.name+"="+value, func(t *testing.T) {
				if _, err := server.ParseConfig([]string{"--storage=probe", "--" + flag.name + "=" + value}, io.Discard, catalog); err == nil {
					t.Fatal("invalid bound was accepted")
				}
			})
		}
	}
	if preparations != 0 || len(recorder.Opens()) != 0 {
		t.Fatal("invalid runtime limits reached provider preparation or opening")
	}
	cfg := parseWithCatalog(t, catalog, "--storage=probe", "--listen=127.0.0.1:0",
		"--max-connections=1", "--max-inflight=1", "--max-maintainers=1", "--max-applications=2", "--max-metadata-bytes=345")
	service := start(t, cfg, provider.Name, provider.Durable)
	service.stop(t)
	if !reflect.DeepEqual(recorder.Policies(), []metadata.StoreLimits{{
		MaxMaintainers: 1, MaxApplications: 2, MaxFieldBytes: 345,
	}}) {
		t.Fatalf("runtime did not propagate the complete policy: %#v", recorder.Policies())
	}
	if _, err := server.ParseConfig([]string{
		"--max-connections=2048", "--max-inflight=256", "--max-maintainers=1000000",
		"--max-applications=1000000", "--max-metadata-bytes=1073741824",
	}, io.Discard, catalog); err != nil {
		t.Fatalf("valid ceilings were rejected: %v", err)
	}
}

func TestRunRevalidatesMutableRuntimeBoundsBeforeOpeningStore(t *testing.T) {
	for _, field := range []string{"connections", "inflight"} {
		t.Run(field, func(t *testing.T) {
			recorder := &probe.Recorder{}
			catalog, err := builtin.NewRegistry(recorder.Provider())
			if err != nil {
				t.Fatal(err)
			}
			cfg := parseWithCatalog(t, catalog, "--storage=probe", "--listen=127.0.0.1:0")
			if field == "connections" {
				cfg.MaxConnections = 0
			} else {
				cfg.MaxInFlight = 257
			}
			if err := server.Run(context.Background(), cfg, io.Discard, io.Discard); err == nil || len(recorder.Opens()) != 0 {
				t.Fatal("mutated runtime bounds reached a provider open")
			}
		})
	}
}

func TestRealListenerOverloadClosesUnreadExpectRequestAndPreservesAdmittedBody(t *testing.T) {
	catalog := builtinCatalog(t)
	for _, backend := range catalog.Descriptions() {
		t.Run(backend.Name, func(t *testing.T) {
			args := []string{"--storage=" + backend.Name, "--listen=127.0.0.1:0", "--max-inflight=1",
				"--max-connections=8", "--request-timeout=5s", "--shutdown-timeout=2s"}
			for _, option := range backend.Options {
				if option.FlagAlias == "db" {
					args = append(args, "--db="+filepath.Join(testsupport.Workspace(t), "records.db"))
				}
			}
			service := start(t, parseWithCatalog(t, catalog, args...), backend.Name, backend.Durable)
			body := `{"name":"Held","email":"held@example.test"}`
			held, heldReader := beginBody(t, service, len(body))
			defer func() { _ = held.Close() }()
			for _, path := range []string{"/healthz", "/readyz"} {
				response := service.do(t, "GET", path, "")
				requireStatus(t, response, 200)
				consume(t, response)
			}
			excess, err := net.DialTimeout("tcp", strings.TrimPrefix(service.url, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = excess.Close() }()
			if err := excess.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			_, err = fmt.Fprintf(excess, "POST /v1/maintainers HTTP/1.1\r\nHost: %s\r\n"+
				"Content-Type: application/json\r\nContent-Length: 100\r\nExpect: 100-continue\r\n\r\n",
				strings.TrimPrefix(service.url, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(excess)
			response, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 503 || response.Header.Get("Retry-After") != "1" || !response.Close {
				_ = response.Body.Close()
				t.Fatalf("first response must be 503 and close, never 100: status %d, close %t, headers %#v",
					response.StatusCode, response.Close, response.Header)
			}
			assertResponseCode(t, response, 503, "overloaded")
			if _, err := reader.ReadByte(); err == nil {
				t.Fatal("unread overload connection stayed open")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatalf("unread overload body lingered until a socket timeout: %v", err)
			}
			if _, err := io.WriteString(held, body); err != nil {
				t.Fatal(err)
			}
			admitted, err := http.ReadResponse(heldReader, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireStatus(t, admitted, 201)
			consume(t, admitted)
			page := decodeResponse[metadata.Page[metadata.Maintainer]](t,
				service.do(t, "GET", "/v1/maintainers", ""), 200)
			if len(page.Items) != 1 || page.Items[0].Email != "held@example.test" {
				t.Fatal("overload consumed storage or prevented admitted work from committing")
			}
			next := service.do(t, "POST", "/v1/maintainers", `{"name":"Next","email":"next@example.test"}`)
			requireStatus(t, next, 201)
			consume(t, next)
			service.stop(t)
		})
	}
}

func TestDrainClosesTheStoreOnlyAfterAdmittedRequestCompletes(t *testing.T) {
	recorder := &probe.Recorder{}
	catalog, err := builtin.NewRegistry(recorder.Provider())
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseWithCatalog(t, catalog, "--storage=probe", "--listen=127.0.0.1:0",
		"--max-inflight=1", "--request-timeout=3s", "--shutdown-timeout=2s")
	service := start(t, cfg, "probe", false)
	body := `{"name":"Drain","email":"drain@example.test"}`
	connection, reader := beginBody(t, service, len(body))
	defer func() { _ = connection.Close() }()
	service.cancel()
	if recorder.Closes() != 0 {
		t.Fatal("shutdown closed the store before the admitted handler drained")
	}
	if _, err := io.WriteString(connection, body); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 201 {
		t.Fatalf("admitted draining request failed with %d", response.StatusCode)
	}
	var created metadata.Maintainer
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil || created.Name != "Drain" {
		t.Fatalf("draining request did not commit: %#v, %v", created, err)
	}
	consume(t, response)
	service.stop(t)
	if recorder.Closes() != 1 {
		t.Fatalf("drained store closed %d times", recorder.Closes())
	}
}
