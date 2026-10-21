package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestResourceCountLimitsReleaseCapacity(t *testing.T) {
	for _, description := range backendRegistry(t).Descriptions() {
		t.Run(description.Name, func(t *testing.T) {
			s := startServer(t, description.Name, filepath.Join(t.TempDir(), "limits.db"),
				"--max-maintainers=1", "--max-applications=1")
			first := s.maintainer(t, "First", "first@example.test")
			firstOwner := decode[record](t, first)
			secondBody := jsonBody(t, map[string]string{"name": "Second", "email": "second@example.test"})
			s.request(http.MethodPost, "/v1/maintainers", secondBody, nil).
				wantError(t, http.StatusServiceUnavailable, "resource_capacity_exceeded")
			unchanged := s.get(t, "/v1/maintainers/"+firstOwner.ID)
			if !bytes.Equal(first.body, unchanged.body) || first.header.Get("ETag") != unchanged.header.Get("ETag") {
				t.Fatal("rejected creation changed the existing Maintainer")
			}
			s.request(http.MethodDelete, "/v1/maintainers/"+firstOwner.ID, nil,
				condition(first.header.Get("ETag"))).want(t, http.StatusNoContent)
			owner := decode[record](t, s.request(http.MethodPost, "/v1/maintainers", secondBody, nil).
				want(t, http.StatusCreated))
			app := s.application(t, owner.ID, "One")
			another := jsonBody(t, map[string]string{
				"title": "Two", "version": "1", "company": "Example", "maintainer_id": owner.ID,
			})
			s.request(http.MethodPost, "/v1/applications", another, nil).
				wantError(t, http.StatusServiceUnavailable, "resource_capacity_exceeded")
			s.request(http.MethodDelete, "/v1/applications/"+decode[record](t, app).ID, nil,
				condition(app.header.Get("ETag"))).want(t, http.StatusNoContent)
			s.request(http.MethodPost, "/v1/applications", another, nil).want(t, http.StatusCreated)
		})
	}
}

func TestResourceByteLimitPreservesRejectedUpdates(t *testing.T) {
	for _, description := range backendRegistry(t).Descriptions() {
		t.Run(description.Name, func(t *testing.T) {
			s := startServer(t, description.Name, filepath.Join(t.TempDir(), "bytes.db"),
				"--max-metadata-bytes=64")
			owner := decode[record](t, s.maintainer(t, "O", "o@x.test"))
			input := map[string]string{"title": "A", "version": "1", "company": "C", "maintainer_id": owner.ID}
			created := s.request(http.MethodPost, "/v1/applications", jsonBody(t, input), nil).
				want(t, http.StatusCreated)
			path := "/v1/applications/" + decode[record](t, created).ID
			growing := jsonBody(t, map[string]string{
				"title": "A", "version": "2", "company": "C", "description": strings.Repeat("x", 80),
			})
			s.request(http.MethodPut, path, growing, condition(created.header.Get("ETag"))).
				wantError(t, http.StatusServiceUnavailable, "resource_capacity_exceeded")
			after := s.get(t, path)
			if !bytes.Equal(created.body, after.body) || created.header.Get("ETag") != after.header.Get("ETag") {
				t.Fatal("byte-capacity rejection changed the Application or its validator")
			}
			smaller := jsonBody(t, map[string]string{
				"title": "A", "version": "2", "company": "C", "description": "fit",
			})
			updated := s.request(http.MethodPut, path, smaller, condition(after.header.Get("ETag"))).
				want(t, http.StatusOK)
			if updated.header.Get("ETag") != "" || decode[record](t, updated).Revision != 2 {
				t.Fatal("a fitting replacement broke the existing revision/PUT contract")
			}
			nextOwner := jsonBody(t, map[string]string{"name": "N", "email": "second@x.test"})
			s.request(http.MethodPost, "/v1/maintainers", nextOwner, nil).
				wantError(t, http.StatusServiceUnavailable, "resource_capacity_exceeded")
			fresh := s.get(t, path)
			s.request(http.MethodDelete, path, nil, condition(fresh.header.Get("ETag"))).
				want(t, http.StatusNoContent)
			s.request(http.MethodPost, "/v1/maintainers", nextOwner, nil).want(t, http.StatusCreated)
		})
	}
}

func TestEncodedPageBudgetPreservesCursorTraversal(t *testing.T) {
	for _, description := range backendRegistry(t).Descriptions() {
		t.Run(description.Name, func(t *testing.T) {
			s := startServer(t, description.Name, filepath.Join(t.TempDir(), "pages.db"))
			owner := decode[record](t, s.maintainer(t, "Owner", "owner@example.test"))
			const count = 15
			large := strings.Repeat("<", 16<<10)
			var expected []string
			for index := 0; index < count; index++ {
				// Raw '<' keeps the request small while exercising escaped response bytes.
				body := fmt.Sprintf(`{"title":"Large %d","version":"1","company":"Example","maintainer_id":%q,"description":"%s"}`,
					index, owner.ID, large)
				created := s.request(http.MethodPost, "/v1/applications", []byte(body), nil).
					want(t, http.StatusCreated)
				expected = append(expected, decode[record](t, created).ID)
			}
			sort.Strings(expected)
			var actual []string
			cursor := ""
			finished := false
			for pageNumber := 0; pageNumber < count; pageNumber++ {
				path := "/v1/applications?limit=100"
				if cursor != "" {
					path += "&after=" + cursor
				}
				result := s.get(t, path)
				if len(result.body) > 1<<20 {
					t.Fatalf("encoded page has %d bytes, exceeding its budget", len(result.body))
				}
				value := decode[page](t, result)
				if pageNumber == 0 && (len(value.Items) == 0 || len(value.Items) >= count || value.NextAfter == "") {
					t.Fatal("the escaped payload did not exercise a byte-shortened page")
				}
				for _, item := range value.Items {
					if item.Description != large {
						t.Fatal("page byte limiting truncated a resource field")
					}
					actual = append(actual, item.ID)
				}
				if value.NextAfter == "" {
					finished = true
					break
				}
				if len(value.Items) == 0 || value.NextAfter <= cursor ||
					value.NextAfter != value.Items[len(value.Items)-1].ID {
					t.Fatal("byte-shortened pagination did not advance from its last returned ID")
				}
				cursor = value.NextAfter
			}
			if !finished || !reflect.DeepEqual(actual, expected) {
				t.Fatal("byte-shortened pages skipped, duplicated, or reordered resources")
			}
		})
	}
}

func TestRequestAdmissionRejectsWithoutBlockingProbes(t *testing.T) {
	s := startServer(t, "memory", "", "--max-inflight=1")
	held := holdPost(t, s, `{"name":"Held","email":"held@example.test"}`)
	rejected := s.request(http.MethodGet, "/v1/maintainers", nil, nil)
	rejected.wantError(t, http.StatusServiceUnavailable, "overloaded")
	if rejected.header.Get("Retry-After") != "1" {
		t.Fatal("admission rejection omitted its retry guidance")
	}
	rejectUnreadPost(t, s, nil).
		wantError(t, http.StatusServiceUnavailable, "overloaded")
	s.get(t, "/healthz")
	s.get(t, "/readyz")
	created := held.finish(t)
	s.get(t, "/v1/maintainers/"+decode[record](t, created).ID)
}

func TestConnectionCapacityRecoversAfterClose(t *testing.T) {
	s := startServer(t, "memory", "", "--max-connections=1")
	s.client.CloseIdleConnections()
	held := waitForProbeConnection(t, s)
	defer held.Close()
	address := strings.TrimPrefix(s.baseURL, "http://")
	excess, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("listener stopped accepting TCP connections: %v", err)
	}
	defer excess.Close()
	if err := excess.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(excess, "GET /healthz HTTP/1.1\r\nHost: %s\r\n\r\n", address)
	if err == nil {
		var result *http.Response
		result, err = http.ReadResponse(bufio.NewReader(excess), &http.Request{Method: http.MethodGet})
		if err == nil {
			result.Body.Close()
			t.Fatal("an excess connection entered HTTP handling")
		}
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("an excess connection queued instead of closing")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	recovered := waitForProbeConnection(t, s)
	recovered.Close()
}

func waitForProbeConnection(t *testing.T, s *server) net.Conn {
	t.Helper()
	address := strings.TrimPrefix(s.baseURL, "http://")
	deadline := time.Now().Add(2 * time.Second)
	var lastError error
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err == nil {
			if err = connection.SetDeadline(time.Now().Add(250 * time.Millisecond)); err == nil {
				_, err = fmt.Fprintf(connection, "GET /healthz HTTP/1.1\r\nHost: %s\r\n\r\n", address)
			}
			if err == nil {
				var result *http.Response
				result, err = http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
				if err == nil {
					_, err = io.ReadAll(result.Body)
					result.Body.Close()
					if result.StatusCode != http.StatusOK {
						connection.Close()
						t.Fatalf("probe returned HTTP %d while waiting for a released connection", result.StatusCode)
					}
					if err == nil {
						if err := connection.SetDeadline(time.Time{}); err != nil {
							connection.Close()
							t.Fatal(err)
						}
						return connection
					}
				}
			}
			connection.Close()
		}
		lastError = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("connection capacity was not released within its lifecycle bound: %v", lastError)
	return nil
}

func TestLowerSQLiteQuotaRejectsReopenWithoutDataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	s := startServer(t, "sqlite", path)
	first := s.maintainer(t, "One", "one@example.test")
	second := s.maintainer(t, "Two", "two@example.test")
	s.stop(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, serverBinary, "--listen=127.0.0.1:0", "--db", path, "--max-maintainers=1")
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	if ctx.Err() != nil || err == nil || strings.Contains(string(output), `"event":"listening"`) {
		t.Fatal("an over-capacity reopen did not fail safely before listening")
	}
	reopened := startServer(t, "sqlite", path)
	for _, before := range []response{first, second} {
		after := reopened.get(t, "/v1/maintainers/"+decode[record](t, before).ID)
		if !bytes.Equal(before.body, after.body) || before.header.Get("ETag") != after.header.Get("ETag") {
			t.Fatal("rejected lower limits changed persisted data or validators")
		}
	}
}
