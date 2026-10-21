package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metadata-api/internal/access"
	"metadata-api/internal/httpapi"
	"metadata-api/internal/metadata"
	"metadata-api/internal/testsupport"
)

type authFixture struct {
	policy   *access.Policy
	writer   string
	reader   string
	hashes   []string
	document string
}

func tokenFixture(t *testing.T) authFixture {
	t.Helper()
	fixture := authFixture{}
	var principals []map[string]any
	for _, role := range []string{"write", "read"} {
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			t.Fatal("generate test credential")
		}
		token := base64.RawURLEncoding.EncodeToString(random[:])
		digest := sha256.Sum256([]byte(token))
		hash := hex.EncodeToString(digest[:])
		principals = append(principals, map[string]any{
			"id": metadata.NewUUID(), "role": role, "token_sha256": []string{hash},
		})
		fixture.hashes = append(fixture.hashes, hash)
		if role == "write" {
			fixture.writer = token
		} else {
			fixture.reader = token
		}
	}
	data, err := json.Marshal(map[string]any{"principals": principals})
	if err != nil {
		t.Fatal("encode test credentials")
	}
	fixture.document = string(data)
	path := filepath.Join(testsupport.Workspace(t), "private-auth.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal("write test credentials")
	}
	fixture.policy, err = access.LoadTokenPolicy(path)
	if err != nil {
		t.Fatal("load valid private test credentials")
	}
	return fixture
}

func policyHandler(t *testing.T, store metadata.MetadataStore, logger *slog.Logger, policy *access.Policy) *httpapi.Handler {
	t.Helper()
	handler, err := httpapi.New(store, logger, httpapi.Options{
		RequestTimeout: time.Second, MaxInFlight: httpapi.DefaultMaxInFlight,
		Access: policy, Authority: localAuthority(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func credentialRequest(api http.Handler, method, path, body, token, tag string) *httptest.ResponseRecorder {
	incoming := localRequest(method, path, strings.NewReader(body))
	incoming.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPost || method == http.MethodPut {
		incoming.Header.Set("Content-Type", "application/json")
	}
	if tag != "" {
		incoming.Header.Set("If-Match", tag)
	}
	return serve(api, incoming)
}

func TestHandlerRequiresExplicitValidAccessAndAuthority(t *testing.T) {
	store := newMemory(t)
	for _, options := range []httpapi.Options{
		{RequestTimeout: time.Second, MaxInFlight: 1, Authority: localAuthority(t)},
		{RequestTimeout: time.Second, MaxInFlight: 1, Access: &access.Policy{}, Authority: localAuthority(t)},
		{RequestTimeout: time.Second, MaxInFlight: 1, Access: access.LocalPolicy()},
	} {
		if handler, err := httpapi.New(store, nil, options); err == nil || handler != nil {
			t.Fatal("handler constructed an implicit open policy or disabled authority checks")
		}
	}
}

func TestHandlerOwnsItsPolicySnapshot(t *testing.T) {
	fixture := tokenFixture(t)
	handler := policyHandler(t, newMemory(t), nil, fixture.policy)
	*fixture.policy = *access.LocalPolicy()
	assertError(t, request(handler, "GET", "/v1/maintainers", "", ""), 401, "unauthenticated")
	assertStatus(t, credentialRequest(handler, "GET", "/v1/maintainers", "", fixture.writer, ""), 200)
}

func TestAuthenticationPrecedesRoutingValidationAndStorage(t *testing.T) {
	fixture := tokenFixture(t)
	handler := policyHandler(t, &untouchedStore{}, nil, fixture.policy)
	for _, test := range []struct{ method, path string }{
		{"GET", "/missing?unsupported=1"},
		{"PATCH", "/v1/maintainers"},
		{"GET", "/v1/maintainers/not-an-id"},
		{"GET", "/v1/applications?limit=invalid"},
		{"POST", "/v1/maintainers"},
		{"PUT", "/v1/applications/not-an-id?unsupported=1"},
		{"DELETE", "/v1/maintainers/not-an-id"},
		{"POST", "/healthz"},
		{"HEAD", "/readyz"},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			incoming := localRequest(test.method, test.path, strings.NewReader("not JSON"))
			incoming.Header.Set("Content-Type", "text/plain")
			incoming.Header.Set("Cookie", "role=write")
			incoming.Header.Set("X-Principal-ID", fixture.policy.Subjects()[0].SubjectKey())
			incoming.Header.Set("X-Role", "write")
			incoming.Header.Set("X-Maintainer-ID", metadata.NewUUID())
			incoming.Header.Set("Forwarded", "for=127.0.0.1;host="+localHost)
			incoming.Header.Set("X-Forwarded-Host", localHost)
			incoming.Header.Set("X-Forwarded-User", "writer")
			response := serve(handler, incoming)
			assertError(t, response, 401, "unauthenticated")
			if response.Header().Get("WWW-Authenticate") != `Bearer realm="metadata-api"` {
				t.Fatal("authentication challenge is not fixed")
			}
		})
	}
	for _, values := range [][]string{
		{}, {""}, {"Basic " + fixture.writer}, {"Bearer invalid"},
		{"Bearer " + fixture.writer, "Bearer " + fixture.writer},
		{"Bearer " + fixture.writer + ", Bearer " + fixture.writer},
	} {
		incoming := localRequest("GET", "/v1/maintainers", nil)
		incoming.Header["authorization"] = values
		assertError(t, serve(handler, incoming), 401, "unauthenticated")
	}
	incoming := localRequest("GET", "/v1/maintainers", nil)
	incoming.Header["Authorization"] = []string{"Bearer " + fixture.writer}
	incoming.Header["authorization"] = nil
	assertError(t, serve(handler, incoming), 401, "unauthenticated")
	assertError(t, credentialRequest(handler, "GET", "/missing", "", fixture.reader, ""), 404, "not_found")
	for _, test := range []struct{ method, path, allow string }{
		{"PATCH", "/v1/maintainers", "GET, POST"},
		{"PUT", "/v1/applications", "GET, POST"},
		{"POST", "/v1/maintainers/not-an-id", "GET, PUT, DELETE"},
		{"POST", "/healthz", "GET"},
	} {
		response := credentialRequest(handler, test.method, test.path, "", fixture.reader, "")
		assertError(t, response, 405, "method_not_allowed")
		if response.Header().Get("Allow") != test.allow {
			t.Fatal("authenticated method failure lost its Allow header")
		}
	}
}

func TestReadPrincipalRejectsEveryRecognizedMutationBeforeBodyOrIDChecks(t *testing.T) {
	fixture := tokenFixture(t)
	handler := policyHandler(t, &untouchedStore{}, nil, fixture.policy)
	for _, kind := range []string{"maintainers", "applications"} {
		for _, test := range []struct{ method, suffix string }{
			{"POST", ""}, {"PUT", "/not-an-id"}, {"DELETE", "/not-an-id"},
		} {
			body := &unreadBody{}
			incoming := localRequest(test.method, "/v1/"+kind+test.suffix+"?invalid=query", body)
			incoming.Header.Set("Authorization", "Bearer "+fixture.reader)
			incoming.Header.Set("Content-Type", "text/plain")
			incoming.Header.Set("Expect", "100-continue")
			response := serve(handler, incoming)
			assertError(t, response, 403, "forbidden")
			if body.reads.Load() != 0 || response.Header().Get("Connection") != "close" ||
				response.Header().Get("WWW-Authenticate") != "" {
				t.Fatal("forbidden mutation read a body, reused its connection, or challenged a verified identity")
			}
		}
	}
}

func TestTokenWriterCRUDAndDatasetWideReaderReads(t *testing.T) {
	fixture := tokenFixture(t)
	store := newMemory(t)
	handler := policyHandler(t, store, nil, fixture.policy)
	ownerCreated := credentialRequest(handler, "POST", "/v1/maintainers",
		`{"name":"Contact","email":"contact@example.test"}`, fixture.writer, "")
	assertStatus(t, ownerCreated, 201)
	owner := decode[metadata.Maintainer](t, ownerCreated)
	ownerPath, ownerTag := ownerCreated.Header().Get("Location"), ownerCreated.Header().Get("ETag")
	for _, subject := range fixture.policy.Subjects() {
		if strings.Contains(subject.SubjectKey(), string(owner.ID)) {
			t.Fatal("authentication subject became a Maintainer identity")
		}
	}
	appCreated := credentialRequest(handler, "POST", "/v1/applications",
		fmt.Sprintf(`{"title":"App","version":"1","company":"Example","maintainer_id":%q}`, owner.ID), fixture.writer, "")
	assertStatus(t, appCreated, 201)
	app := decode[metadata.Application](t, appCreated)
	appPath, appTag := appCreated.Header().Get("Location"), appCreated.Header().Get("ETag")
	for _, path := range []string{
		"/v1/maintainers", "/v1/applications", ownerPath, appPath,
		"/v1/applications?maintainer_id=" + string(owner.ID),
	} {
		for _, token := range []string{fixture.reader, fixture.writer} {
			assertStatus(t, credentialRequest(handler, "GET", path, "", token, ""), 200)
		}
	}
	for _, test := range []struct{ path, tag, body string }{
		{ownerPath, ownerTag, `{"name":"Renamed","email":"contact@example.test"}`},
		{appPath, appTag, `{"title":"Renamed","version":"2","company":"Example"}`},
	} {
		assertError(t, credentialRequest(handler, "PUT", test.path, test.body, fixture.reader, test.tag), 403, "forbidden")
		assertError(t, credentialRequest(handler, "DELETE", test.path, "", fixture.reader, test.tag), 403, "forbidden")
	}
	if decode[metadata.Maintainer](t, credentialRequest(handler, "GET", ownerPath, "", fixture.reader, "")) != owner ||
		decode[metadata.Application](t, credentialRequest(handler, "GET", appPath, "", fixture.reader, "")) != app {
		t.Fatal("reader mutation changed stored fields or revisions")
	}
	assertError(t, credentialRequest(handler, "DELETE", ownerPath, "", fixture.writer, ownerTag), 409, "maintainer_in_use")
	updated := credentialRequest(handler, "PUT", appPath,
		`{"title":"Renamed","version":"2","company":"Example"}`, fixture.writer, appTag)
	assertStatus(t, updated, 200)
	if updated.Header().Get("ETag") != "" || decode[metadata.Application](t, updated).MaintainerID != owner.ID {
		t.Fatal("authenticated PUT changed the creator or emitted a transformed validator")
	}
	assertError(t, credentialRequest(handler, "DELETE", appPath, "", fixture.writer, appTag), 412, "precondition_failed")
	read := credentialRequest(handler, "GET", appPath, "", fixture.reader, "")
	assertStatus(t, read, 200)
	assertStatus(t, credentialRequest(handler, "DELETE", appPath, "", fixture.writer, read.Header().Get("ETag")), 204)
	assertStatus(t, credentialRequest(handler, "DELETE", ownerPath, "", fixture.writer, ownerTag), 204)
	page, err := store.ListMaintainers(context.Background(), metadata.MaintainerList{})
	if err != nil || len(page.Items) != 0 {
		t.Fatal("writer deletion did not commit")
	}
}

func TestOnlyGETProbesBypassSuppliedCredentials(t *testing.T) {
	fixture := tokenFixture(t)
	for _, policy := range []*access.Policy{access.LocalPolicy(), fixture.policy} {
		handler := policyHandler(t, newMemory(t), nil, policy)
		for _, path := range []string{"/healthz", "/readyz"} {
			incoming := localRequest("GET", path, nil)
			incoming.Header["Authorization"] = []string{"", "not a credential"}
			assertStatus(t, serve(handler, incoming), 200)
			incoming.URL.RawQuery = "invalid=1"
			assertError(t, serve(handler, incoming), 400, "invalid_query")
			incoming.URL.RawQuery = ""
			incoming.Host = "evil.invalid:8080"
			assertError(t, serve(handler, incoming), 400, "invalid_host")
		}
	}
}

func TestLocalModeRejectsAnySuppliedAuthorizationOnProtectedRoutes(t *testing.T) {
	handler := newHandler(t, &untouchedStore{}, nil, time.Second)
	for _, path := range []string{"/missing?invalid=1", "/v1/maintainers/not-an-id", "/v1/applications"} {
		for _, values := range [][]string{{}, {""}, {"Bearer unchecked"}, {"first", "second"}} {
			incoming := localRequest("POST", path, &unreadBody{})
			incoming.Header["aUtHoRiZaTiOn"] = values
			response := serve(handler, incoming)
			assertError(t, response, 400, "authentication_not_enabled")
			if response.Header().Get("WWW-Authenticate") != "" || response.Header().Get("Connection") != "close" {
				t.Fatal("local mode implied token verification or retained an unread connection")
			}
		}
	}
}

func TestHTTPAuthorityPrecedesAccessAndIgnoresForwardedAuthorities(t *testing.T) {
	fixture := tokenFixture(t)
	handler := policyHandler(t, newMemory(t), nil, fixture.policy)
	for _, host := range []string{
		"evil.invalid:8080", "user@localhost:8080", "localhost:8081", "localhost",
		"127.0.0.1:8080/path", "localhost:8080,evil.invalid:8080", "[::1%zone]:8080",
	} {
		incoming := localRequest("GET", "/missing", nil)
		incoming.Host = host
		incoming.Header.Set("X-Forwarded-Host", localHost)
		incoming.Header.Set("Forwarded", "host="+localHost)
		response := serve(handler, incoming)
		assertError(t, response, 400, "invalid_host")
		if response.Header().Get("WWW-Authenticate") != "" {
			t.Fatal("authentication ran before authority validation")
		}
	}
	for _, host := range []string{localHost, "localhost:8080", "[::1]:8080"} {
		incoming := localRequest("GET", "/v1/maintainers", nil)
		incoming.Host = host
		incoming.Header.Set("Authorization", "Bearer "+fixture.writer)
		incoming.Header.Set("X-Forwarded-Host", "evil.invalid:8080")
		incoming.Header.Set("X-Forwarded-Proto", "https")
		incoming.Header.Set("Forwarded", "host=evil.invalid:8080;proto=https")
		assertStatus(t, serve(handler, incoming), 200)
	}
}

func TestRejectedCredentialsAndPermissionsBoundUnreadCleanupAndSafeWarnings(t *testing.T) {
	fixture := tokenFixture(t)
	for _, test := range []struct {
		name   string
		policy *access.Policy
		header string
		host   string
		status int
		code   string
	}{
		{"missing", fixture.policy, "", localHost, 401, "unauthenticated"},
		{"invalid", fixture.policy, "Bearer unchecked", localHost, 401, "unauthenticated"},
		{"forbidden", fixture.policy, "Bearer " + fixture.reader, localHost, 403, "forbidden"},
		{"local", access.LocalPolicy(), "Bearer unchecked", localHost, 400, "authentication_not_enabled"},
		{"authority", fixture.policy, "", "unchecked.invalid:8080", 400, "invalid_host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler := policyHandler(t, &untouchedStore{}, slog.New(slog.NewJSONHandler(&logs, nil)), test.policy)
			body := &unreadBody{}
			incoming := localRequest("POST", "/v1/maintainers", body)
			incoming.Host = test.host
			if test.header != "" {
				incoming.Header.Set("Authorization", test.header)
			}
			incoming.Header.Set("Expect", "100-continue")
			writer := &deadlineFailureResponse{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(writer, incoming)
			assertError(t, writer.ResponseRecorder, test.status, test.code)
			if writer.attempts != 1 || body.reads.Load() != 0 || writer.Header().Get("Connection") != "close" {
				t.Fatal("rejection did not expire unread-body cleanup without reading")
			}
			if !strings.Contains(logs.String(), "rejected_body_read_deadline_failed") ||
				strings.Contains(logs.String(), "private-deadline-error-details") ||
				strings.Contains(logs.String(), "unchecked") || strings.Contains(logs.String(), fixture.reader) {
				t.Fatal("read-deadline protection failed silently or logged unchecked details")
			}
		})
	}
}

func TestSecurityLogsAndResponsesExcludeCredentialsAndUncheckedInputs(t *testing.T) {
	fixture := tokenFixture(t)
	var logs bytes.Buffer
	handler := policyHandler(t, newMemory(t), slog.New(slog.NewJSONHandler(&logs, nil)), fixture.policy)
	incoming := localRequest("GET", "/v1/maintainers?private="+fixture.hashes[0], nil)
	incoming.Header.Set("Authorization", "Bearer "+fixture.writer)
	invalidQuery := serve(handler, incoming)
	assertError(t, invalidQuery, 400, "invalid_query")
	incoming = localRequest("GET", "/v1/maintainers", nil)
	incoming.Header.Set("Authorization", "Bearer "+fixture.writer[:42])
	invalidToken := serve(handler, incoming)
	assertError(t, invalidToken, 401, "unauthenticated")
	incoming = localRequest("GET", "/missing", nil)
	incoming.Host = fixture.reader + ".invalid:8080"
	invalidHost := serve(handler, incoming)
	assertError(t, invalidHost, 400, "invalid_host")
	invalidMethod := credentialRequest(handler, fixture.writer, "/v1/maintainers", "", fixture.reader, "")
	assertError(t, invalidMethod, 405, "method_not_allowed")
	all := logs.String() + invalidQuery.Body.String() + invalidToken.Body.String() +
		invalidHost.Body.String() + invalidMethod.Body.String()
	for _, secret := range append([]string{fixture.writer, fixture.writer[:42], fixture.reader, fixture.document}, fixture.hashes...) {
		if strings.Contains(all, secret) {
			t.Fatal("security response or logs exposed credential material")
		}
	}
	if !strings.Contains(logs.String(), `"route":"/v1/maintainers"`) ||
		!strings.Contains(logs.String(), `"route":"unmatched"`) || !strings.Contains(logs.String(), `"method":"OTHER"`) {
		t.Fatal("safe correlated route logging was lost")
	}
}

type untouchedStore struct{ metadata.MetadataStore }
