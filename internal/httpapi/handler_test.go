package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"metadata-api/internal/httpapi"
	"metadata-api/internal/metadata"
	"metadata-api/internal/storage/memory"
	"metadata-api/internal/storage/sqlite"
	"metadata-api/internal/testsupport"
)

func TestHTTPCRUDAndConditionalWrites(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store metadata.MetadataStore = memory.New()
			if backend == "sqlite" {
				var err error
				store, err = sqlite.Open(context.Background(), filepath.Join(testsupport.Workspace(t), "records.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = store.Close() })
			api := httpapi.New(store, nil, time.Second)
			created := request(api, "POST", "/v1/maintainers", `{"name":"  River Example  ","email":" RIVER@EXAMPLE.TEST "}`, "")
			assertStatus(t, created, 201)
			owner := decode[metadata.Maintainer](t, created)
			ownerPath := "/v1/maintainers/" + string(owner.ID)
			ownerTag := `"m1:` + string(owner.ID) + `:1"`
			if owner.Name != "River Example" || owner.Email != "river@example.test" || owner.Revision != 1 ||
				created.Header().Get("Location") != ownerPath || created.Header().Get("ETag") != ownerTag {
				t.Fatalf("bad creation response: %s %#v", created.Body, created.Header())
			}
			get := request(api, "GET", ownerPath, "", "")
			assertStatus(t, get, 200)
			if decode[metadata.Maintainer](t, get) != owner || get.Header().Get("ETag") != ownerTag {
				t.Fatal("GET body or tag differs from the stored representation")
			}
			assertError(t, request(api, "PUT", ownerPath, `{"name":"New","email":"river@example.test"}`, ""), 428, "precondition_required")
			assertError(t, request(api, "PUT", ownerPath, `{"name":"New","email":"river@example.test"}`, `W/`+ownerTag), 412, "precondition_failed")
			replace := httptest.NewRequest("PUT", ownerPath, strings.NewReader(`{"name":" Replaced ","email":"river@example.test"}`))
			replace.Header.Set("Content-Type", "application/json")
			replace.Header.Add("If-Match", `"unknown,with,commas"`)
			replace.Header.Add("If-Match", `W/`+ownerTag+`, `+ownerTag)
			replaced := serve(api, replace)
			assertStatus(t, replaced, 200)
			owner = decode[metadata.Maintainer](t, replaced)
			if owner.Name != "Replaced" || owner.Revision != 2 || replaced.Header().Get("ETag") != "" {
				t.Fatalf("PUT must transform fields and omit ETag: %s %#v", replaced.Body, replaced.Header())
			}
			assertError(t, request(api, "PUT", ownerPath, `{"name":"Stale","email":"river@example.test"}`, ownerTag), 412, "precondition_failed")
			get = request(api, "GET", ownerPath, "", "")
			assertStatus(t, get, 200)
			if decode[metadata.Maintainer](t, get) != owner || get.Header().Get("ETag") != `"m1:`+string(owner.ID)+`:2"` {
				t.Fatal("stale write changed the stored representation")
			}
			freshOwnerTag := get.Header().Get("ETag")
			appBody := fmt.Sprintf(`{"title":" Atlas ","version":"1.0","company":" Example ","maintainer_id":%q,"website":null,"source":null,"license":null,"description":null}`, owner.ID)
			appCreated := request(api, "POST", "/v1/applications", appBody, "")
			assertStatus(t, appCreated, 201)
			app := decode[metadata.Application](t, appCreated)
			appPath := "/v1/applications/" + string(app.ID)
			appTag := `"a1:` + string(app.ID) + `:1"`
			if app.MaintainerID != owner.ID || app.Title != "Atlas" || app.Company != "Example" ||
				app.Website != "" || app.Source != "" || app.License != "" || app.Description != "" ||
				appCreated.Header().Get("ETag") != appTag || appCreated.Header().Get("Location") != appPath {
				t.Fatalf("bad application creation: %s", appCreated.Body)
			}
			assertError(t, request(api, "DELETE", ownerPath, "", ownerTag), 409, "maintainer_in_use")
			assertError(t, request(api, "PUT", appPath, `{"title":"Replaced","version":"2","company":"Example"}`, freshOwnerTag), 412, "precondition_failed")
			immutable := fmt.Sprintf(`{"title":"Replaced","version":"2","company":"Example","maintainer_id":%q}`, owner.ID)
			assertError(t, request(api, "PUT", appPath, immutable, appTag), 400, "invalid_json")
			updated := request(api, "PUT", appPath, `{"title":"Replaced","version":"2","company":"Example"}`, appTag)
			assertStatus(t, updated, 200)
			replacedApp := decode[metadata.Application](t, updated)
			if replacedApp.MaintainerID != owner.ID || replacedApp.Revision != 2 ||
				replacedApp.Version != "2" || replacedApp.CreatedAt != app.CreatedAt || updated.Header().Get("ETag") != "" {
				t.Fatalf("bad application replacement: %s %#v", updated.Body, updated.Header())
			}
			assertError(t, request(api, "DELETE", appPath, "", appTag), 412, "precondition_failed")
			appGet := request(api, "GET", appPath, "", "")
			assertStatus(t, appGet, 200)
			if decode[metadata.Application](t, appGet) != replacedApp {
				t.Fatal("stale delete changed application")
			}
			freshAppTag := appGet.Header().Get("ETag")
			deleted := request(api, "DELETE", appPath, "", freshAppTag)
			assertStatus(t, deleted, 204)
			if deleted.Body.Len() != 0 || deleted.Header().Get("ETag") != "" {
				t.Fatal("DELETE returned a body or tag")
			}
			assertError(t, request(api, "GET", appPath, "", ""), 404, "not_found")
			deleted = request(api, "DELETE", ownerPath, "", freshOwnerTag)
			assertStatus(t, deleted, 204)
			assertError(t, request(api, "GET", ownerPath, "", ""), 404, "not_found")
		})
	}
}

func TestConcurrentHTTPWritersHaveOneWinner(t *testing.T) {
	api := httpapi.New(memory.New(), nil, time.Second)
	created := request(api, "POST", "/v1/maintainers", `{"name":"Initial","email":"initial@example.test"}`, "")
	assertStatus(t, created, 201)
	path, tag := created.Header().Get("Location"), created.Header().Get("ETag")
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := range 2 {
		go func() {
			<-start
			results <- request(api, "PUT", path, fmt.Sprintf(`{"name":"Writer %d","email":"initial@example.test"}`, i), tag)
		}()
	}
	close(start)
	var winner metadata.Maintainer
	success, stale := 0, 0
	for range 2 {
		response := <-results
		switch response.Code {
		case 200:
			success++
			winner = decode[metadata.Maintainer](t, response)
			if response.Header().Get("ETag") != "" {
				t.Fatal("concurrent PUT returned a validator")
			}
		case 412:
			stale++
			assertError(t, response, 412, "precondition_failed")
		default:
			t.Fatalf("unexpected concurrent status %d: %s", response.Code, response.Body)
		}
	}
	fresh := request(api, "GET", path, "", "")
	assertStatus(t, fresh, 200)
	if success != 1 || stale != 1 || winner.Revision != 2 || decode[metadata.Maintainer](t, fresh) != winner {
		t.Fatal("concurrent HTTP writes did not preserve the single winner")
	}
	next := request(api, "PUT", path, `{"name":"Next edit","email":"initial@example.test"}`, fresh.Header().Get("ETag"))
	assertStatus(t, next, 200)
	if decode[metadata.Maintainer](t, next).Revision != 3 {
		t.Fatal("fresh GET tag did not permit the next edit")
	}
}

func TestStrictJSONAndMediaBoundary(t *testing.T) {
	good := `{"name":"Example","email":"example@example.test"}`
	tests := []struct {
		name  string
		body  string
		media string
		code  int
		error string
	}{
		{"missing media", good, "", 415, "unsupported_media_type"},
		{"wrong media", good, "text/plain", 415, "unsupported_media_type"},
		{"bad media", good, `application/json; charset="`, 415, "unsupported_media_type"},
		{"unsupported charset", good, "application/json; charset=iso-8859-1", 415, "unsupported_media_type"},
		{"empty", "", "application/json", 400, "invalid_json"},
		{"invalid", "{", "application/json", 400, "invalid_json"},
		{"trailing document", good + "{}", "application/json", 400, "invalid_json"},
		{"trailing garbage", good + "x", "application/json", 400, "invalid_json"},
		{"unknown field", `{"name":"Example","email":"example@example.test","revision":1}`, "application/json", 400, "invalid_json"},
		{"case-sensitive names", `{"Name":"Example","email":"example@example.test"}`, "application/json", 400, "invalid_json"},
		{"duplicate field", `{"name":"First","name":"Second","email":"example@example.test"}`, "application/json", 400, "invalid_json"},
		{"number field", `{"name":42,"email":"example@example.test"}`, "application/json", 400, "invalid_json"},
		{"object field", `{"name":{},"email":"example@example.test"}`, "application/json", 400, "invalid_json"},
		{"array field", `{"name":[],"email":"example@example.test"}`, "application/json", 400, "invalid_json"},
		{"array root", "[]", "application/json", 400, "invalid_json"},
		{"null root", "null", "application/json", 400, "invalid_json"},
		{"invalid UTF8", "{\"name\":\"\xff\",\"email\":\"example@example.test\"}", "application/json", 400, "invalid_json"},
		{"oversized", good + strings.Repeat(" ", 65537), "application/json", 413, "body_too_large"},
		{"missing required", `{"email":"example@example.test"}`, "application/json", 422, "validation_failed"},
		{"null required", `{"name":null,"email":"example@example.test"}`, "application/json", 422, "validation_failed"},
		{"display mailbox", `{"name":"Example","email":"Person <example@example.test>"}`, "application/json", 422, "validation_failed"},
		{"oversized name", `{"name":"` + strings.Repeat("x", 129) + `","email":"example@example.test"}`, "application/json", 422, "validation_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := memory.New()
			api := httpapi.New(store, nil, time.Second)
			req := httptest.NewRequest("POST", "/v1/maintainers", strings.NewReader(test.body))
			if test.media != "" {
				req.Header.Set("Content-Type", test.media)
			}
			response := serve(api, req)
			assertError(t, response, test.code, test.error)
			if strings.Contains(response.Body.String(), "example@example.test") {
				t.Fatal("error response echoed input data")
			}
			page, err := store.ListMaintainers(context.Background(), metadata.MaintainerList{})
			if err != nil || len(page.Items) != 0 {
				t.Fatal("invalid request mutated storage")
			}
		})
	}
	api := httpapi.New(memory.New(), nil, time.Second)
	exact := httptest.NewRequest("POST", "/v1/maintainers", strings.NewReader(good+strings.Repeat(" ", 65536-len(good))))
	exact.Header.Set("Content-Type", "application/json; charset=utf-8")
	assertStatus(t, serve(api, exact), 201)
	repeatedMedia := httptest.NewRequest("POST", "/v1/maintainers", strings.NewReader(good))
	repeatedMedia.Header.Add("Content-Type", "application/json")
	repeatedMedia.Header.Add("Content-Type", "application/json")
	assertError(t, serve(api, repeatedMedia), 415, "unsupported_media_type")
}

func TestRoutingQueriesAndPagination(t *testing.T) {
	api := httpapi.New(memory.New(), nil, time.Second)
	for _, path := range []string{"/missing", "/shutdown", "/v1/maintainers/", "/v1/maintainers/a/b", "/v2/maintainers"} {
		assertError(t, request(api, "GET", path, "", ""), 404, "not_found")
	}
	for _, test := range []struct{ method, path, allow string }{
		{"PATCH", "/v1/maintainers", "GET, POST"},
		{"POST", "/v1/applications/" + metadata.NewUUID(), "GET, PUT, DELETE"},
		{"HEAD", "/healthz", "GET"},
		{"OPTIONS", "/readyz", "GET"},
	} {
		response := request(api, test.method, test.path, "", "")
		assertError(t, response, 405, "method_not_allowed")
		if response.Header().Get("Allow") != test.allow {
			t.Fatalf("Allow = %q, want %q", response.Header().Get("Allow"), test.allow)
		}
	}
	for _, path := range []string{"/v1/maintainers/invalid", "/v1/applications/AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"} {
		assertError(t, request(api, "GET", path, "", ""), 400, "invalid_id")
	}
	for _, query := range []string{
		"limit=0", "limit=-1", "limit=101", "limit=", "limit=1.5", "limit=%2B1",
		"limit=999999999999999999999", "limit=1&limit=2", "unknown=1",
		"after=a&after=b", "maintainer_id=" + metadata.NewUUID(), "limit=1;after=x", "limit=%zz",
	} {
		assertError(t, request(api, "GET", "/v1/maintainers?"+query, "", ""), 400, "invalid_query")
	}
	for _, path := range []string{
		"/v1/maintainers?after=", "/v1/maintainers?after=invalid", "/v1/applications?maintainer_id=invalid",
		"/v1/applications?after=invalid",
	} {
		assertError(t, request(api, "GET", path, "", ""), 400, "invalid_id")
	}
	assertError(t, request(api, "GET", "/healthz?unknown=x", "", ""), 400, "invalid_query")
	empty := request(api, "GET", "/v1/maintainers", "", "")
	assertStatus(t, empty, 200)
	if strings.TrimSpace(empty.Body.String()) != `{"items":[]}` {
		t.Fatalf("empty collection = %s", empty.Body)
	}
	for i := range 4 {
		assertStatus(t, request(api, "POST", "/v1/maintainers", fmt.Sprintf(`{"name":"Example %d","email":"page%d@example.test"}`, i, i), ""), 201)
	}
	first := request(api, "GET", "/v1/maintainers?limit=2", "", "")
	assertStatus(t, first, 200)
	page := decode[metadata.Page[metadata.Maintainer]](t, first)
	if len(page.Items) != 2 || page.NextAfter != string(page.Items[1].ID) || page.Items[0].ID >= page.Items[1].ID {
		t.Fatalf("first page = %#v", page)
	}
	second := request(api, "GET", "/v1/maintainers?limit=2&after="+page.NextAfter, "", "")
	assertStatus(t, second, 200)
	last := decode[metadata.Page[metadata.Maintainer]](t, second)
	if len(last.Items) != 2 || last.NextAfter != "" || last.Items[0].ID <= page.Items[1].ID || strings.Contains(second.Body.String(), "next_after") {
		t.Fatalf("terminal page = %s", second.Body)
	}
	filtered := request(api, "GET", "/v1/applications?maintainer_id="+metadata.NewUUID(), "", "")
	assertStatus(t, filtered, 200)
	if strings.TrimSpace(filtered.Body.String()) != `{"items":[]}` {
		t.Fatal("missing maintainer filter did not return an empty array")
	}
}

func TestErrorPrecedenceAndReferenceValidation(t *testing.T) {
	api := httpapi.New(memory.New(), nil, time.Second)
	first := request(api, "POST", "/v1/maintainers", `{"name":"First","email":"first@example.test"}`, "")
	second := request(api, "POST", "/v1/maintainers", `{"name":"Second","email":"second@example.test"}`, "")
	assertStatus(t, first, 201)
	assertStatus(t, second, 201)
	firstPath := first.Header().Get("Location")
	assertError(t, request(api, "PUT", firstPath, `{"name":"Conflict","email":"second@example.test"}`, `"unknown"`), 409, "duplicate_email")
	absentMaintainer := "/v1/maintainers/" + metadata.NewUUID()
	absentApp := "/v1/applications/" + metadata.NewUUID()
	assertError(t, request(api, "PUT", absentMaintainer, `{"name":"Valid","email":"second@example.test"}`, `"unknown"`), 404, "not_found")
	assertError(t, request(api, "DELETE", absentMaintainer, "", `"unknown"`), 404, "not_found")
	assertError(t, request(api, "PUT", absentApp, `{"title":"Valid","version":"1","company":"Example"}`, `"unknown"`), 404, "not_found")
	assertError(t, request(api, "DELETE", absentApp, "", `"unknown"`), 404, "not_found")
	assertError(t, request(api, "PUT", absentMaintainer, `{"name":"Valid","email":"valid@example.test"}`, "*"), 400, "invalid_precondition")
	assertError(t, request(api, "PUT", absentMaintainer, `{"name":null,"email":"valid@example.test"}`, `"unknown"`), 422, "validation_failed")
	assertError(t, request(api, "PUT", absentMaintainer, `{"unknown":1}`, "*"), 400, "invalid_json")
	assertError(t, request(api, "POST", "/v1/applications", `{"title":"Atlas","version":"1","company":"Example"}`, ""), 422, "validation_failed")
	assertError(t, request(api, "POST", "/v1/applications",
		fmt.Sprintf(`{"title":"Atlas","version":"1","company":"Example","maintainer_id":%q}`, metadata.NewUUID()), ""), 422, "invalid_reference")
	assertError(t, request(api, "DELETE", firstPath, "", "*"), 400, "invalid_precondition")
	assertError(t, request(api, "DELETE", firstPath, "", ""), 428, "precondition_required")
	assertError(t, request(api, "GET", firstPath+"?limit=1", "", ""), 400, "invalid_query")
}

func TestReadinessTimeoutAndSafeLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := memory.New()
	api := httpapi.New(store, logger, time.Second)
	assertStatus(t, request(api, "GET", "/healthz", "", ""), 200)
	assertStatus(t, request(api, "GET", "/readyz", "", ""), 200)
	owner, err := store.CreateMaintainer(context.Background(), metadata.MaintainerFields{Name: "Example", Email: "not-logged@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	incoming := httptest.NewRequest("GET", "/v1/maintainers/"+string(owner.ID), nil)
	incoming.Header.Set("X-Request-ID", "untrusted-client-marker")
	response := serve(api, incoming)
	assertStatus(t, response, 200)
	id := response.Header().Get("X-Request-ID")
	if !metadata.ValidUUID(id) || id == incoming.Header.Get("X-Request-ID") {
		t.Fatal("request ID was not generated server-side")
	}
	for _, forbidden := range []string{"untrusted-client-marker", string(owner.ID), owner.Email, owner.Name} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("logs contain input data %q", forbidden)
		}
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	last := map[string]any{}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	if last["request_id"] != id || last["method"] != "GET" || last["route"] != "/v1/maintainers/{id}" ||
		last["status"] != float64(200) || last["duration_ms"] == nil {
		t.Fatalf("request log missing required fields: %#v", last)
	}
	api.BeginDrain()
	assertError(t, request(api, "GET", "/readyz", "", ""), 503, "unavailable")
	assertStatus(t, request(api, "GET", "/healthz", "", ""), 200)
	_ = store.Close()
	closed := httpapi.New(store, nil, time.Second)
	assertError(t, request(closed, "GET", "/readyz", "", ""), 503, "unavailable")
	assertError(t, request(closed, "GET", "/v1/maintainers", "", ""), 503, "unavailable")
	for _, test := range []struct {
		name   string
		fault  *faultStore
		status int
		code   string
	}{
		{"raw storage error", &faultStore{MetadataStore: memory.New(), failure: errors.New("SQL SELECT private payload")}, 500, "internal_error"},
		{"panic", &faultStore{MetadataStore: memory.New(), panicGet: true}, 500, "internal_error"},
		{"deadline", &faultStore{MetadataStore: memory.New(), waitForDeadline: true}, 503, "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			faulty := httpapi.New(test.fault, nil, 10*time.Millisecond)
			response := request(faulty, "GET", "/v1/maintainers/"+metadata.NewUUID(), "", "")
			assertError(t, response, test.status, test.code)
			if strings.Contains(response.Body.String(), "SQL") || strings.Contains(response.Body.String(), "private") {
				t.Fatal("internal details leaked")
			}
		})
	}
}

func TestPanicRecoveryLogsWithoutPayloads(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := &faultStore{MetadataStore: memory.New(), panicGet: true}
	api := httpapi.New(store, logger, time.Second)
	response := request(api, "GET", "/v1/maintainers/"+metadata.NewUUID(), "", "")
	assertError(t, response, 500, "internal_error")
	log := logs.String()
	if !strings.Contains(log, `"level":"ERROR"`) ||
		!strings.Contains(log, `"msg":"request_panic"`) ||
		!strings.Contains(log, response.Header().Get("X-Request-ID")) ||
		!strings.Contains(log, `"stack"`) {
		t.Fatalf("panic has no actionable correlated error log: %s", log)
	}
	if strings.Contains(log, "private panic payload") {
		t.Fatal("panic value leaked into logs")
	}
}

func TestPanicAfterResponseStartAbortsTheConnection(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	api := httpapi.New(memory.New(), logger, time.Second)
	recorder := httptest.NewRecorder()
	writer := &panicAfterWrite{ResponseWriter: recorder}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		api.ServeHTTP(writer, httptest.NewRequest("GET", "/healthz", nil))
	}()
	if recovered != http.ErrAbortHandler {
		t.Fatalf("partial response did not abort: %v", recovered)
	}
	if recorder.Body.String() != "{" {
		t.Fatalf("recovery wrote onto the partial response: %q", recorder.Body.String())
	}
	if !strings.Contains(logs.String(), `"msg":"request_panic"`) ||
		strings.Contains(logs.String(), "private write payload") {
		t.Fatalf("partial-response panic was hidden or leaked its value: %s", logs.String())
	}
	var event struct {
		Duration *int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil || event.Duration == nil || *event.Duration < 0 {
		t.Fatalf("aborted request has no valid elapsed time: %v %s", err, logs.String())
	}
}

type panicAfterWrite struct {
	http.ResponseWriter
}

func (w *panicAfterWrite) Write(data []byte) (int, error) {
	_, _ = w.ResponseWriter.Write(data[:1])
	panic("private write payload")
}

type faultStore struct {
	metadata.MetadataStore
	failure         error
	panicGet        bool
	waitForDeadline bool
}

func (s *faultStore) GetMaintainer(ctx context.Context, _ metadata.MaintainerID) (metadata.Maintainer, error) {
	if s.panicGet {
		panic("private panic payload")
	}
	if s.waitForDeadline {
		<-ctx.Done()
		return metadata.Maintainer{}, metadata.WrapError(metadata.Unavailable, ctx.Err())
	}
	return metadata.Maintainer{}, s.failure
}

func request(api http.Handler, method, path, body, tag string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == "POST" || method == "PUT" {
		req.Header.Set("Content-Type", "application/json")
	}
	if tag != "" {
		req.Header.Set("If-Match", tag)
	}
	return serve(api, req)
}

func serve(api http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	return response
}

func decode[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %s: %v", response.Body, err)
	}
	return value
}

func assertStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d, want %d: %s", response.Code, status, response.Body)
	}
	if !metadata.ValidUUID(response.Header().Get("X-Request-ID")) {
		t.Fatal("missing server-generated request ID")
	}
	if status != 204 && response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("non-JSON response: %#v", response.Header())
	}
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	assertStatus(t, response, status)
	var envelope struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Fields  map[string]string `json:"fields"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != code || envelope.Error.Message == "" || envelope.RequestID != response.Header().Get("X-Request-ID") {
		t.Fatalf("wrong error envelope: %s", response.Body)
	}
	if status == 422 && code == "validation_failed" && reflect.DeepEqual(envelope.Error.Fields, map[string]string(nil)) {
		t.Fatal("validation error has no fields")
	}
}
