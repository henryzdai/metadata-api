package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestCRUDAndCreatorIntegrity(t *testing.T) {
	for _, description := range backendRegistry(t).Descriptions() {
		backend := description.Name
		t.Run(backend, func(t *testing.T) {
			s := startServer(t, backend, filepath.Join(t.TempDir(), "metadata.db"))
			ownerResponse := s.maintainer(t, "  Example Owner  ", "OWNER@Example.test")
			owner := decode[record](t, ownerResponse)
			ownerPath := "/v1/maintainers/" + owner.ID
			if owner.Name != "Example Owner" || owner.Email != "owner@example.test" {
				t.Fatalf("normalization failed: %s", ownerResponse.body)
			}
			s.get(t, ownerPath)
			duplicate := jsonBody(t, map[string]string{"name": "Other", "email": "Owner@example.test"})
			s.request(http.MethodPost, "/v1/maintainers", duplicate, nil).
				wantError(t, 409, "duplicate_email")
			appResponse := s.application(t, owner.ID, "Example App")
			app := decode[record](t, appResponse)
			appPath := "/v1/applications/" + app.ID
			if app.MaintainerID != owner.ID || app.Version != "1.0" {
				t.Fatalf("application relation/version lost: %s", appResponse.body)
			}
			s.request(http.MethodDelete, ownerPath, nil, condition(`"stale"`)).
				wantError(t, 409, "maintainer_in_use")
			replacement := jsonBody(t, map[string]string{
				"title": "Edited App", "version": "2.0", "company": "New Example",
			})
			updatedResponse := s.request(http.MethodPut, appPath, replacement,
				condition(appResponse.header.Get("ETag"))).want(t, 200)
			assertNoPUTValidator(t, updatedResponse)
			updated := decode[record](t, updatedResponse)
			if updated.Revision != 2 || updated.MaintainerID != owner.ID || updated.CreatedAt != app.CreatedAt {
				t.Errorf("replacement changed immutable fields or missed revision: %s", updatedResponse.body)
			}
			if updated.Title != "Edited App" || updated.Version != "2.0" ||
				updated.Website != "" || updated.Source != "" || updated.License != "" || updated.Description != "" {
				t.Errorf("PUT is not a full mutable-field replacement: %s", updatedResponse.body)
			}
			fields := decode[map[string]any](t, updatedResponse)
			for _, name := range []string{"website", "source", "license", "description"} {
				if value, exists := fields[name]; !exists || value != "" {
					t.Errorf("empty response field %s is absent or not a string", name)
				}
			}
			s.request(http.MethodPut, appPath, replacement, condition(appResponse.header.Get("ETag"))).
				wantError(t, 412, "precondition_failed")
			s.request(http.MethodDelete, appPath, nil, condition(appResponse.header.Get("ETag"))).
				wantError(t, 412, "precondition_failed")
			fresh := s.get(t, appPath)
			s.request(http.MethodDelete, appPath, nil, condition(fresh.header.Get("ETag"))).want(t, 204)
			s.request(http.MethodGet, appPath, nil, nil).wantError(t, 404, "not_found")
			ownerBody := jsonBody(t, map[string]string{"name": "Updated Owner", "email": "updated@example.test"})
			ownerUpdate := s.request(http.MethodPut, ownerPath, ownerBody,
				condition(ownerResponse.header.Get("ETag"))).want(t, 200)
			assertNoPUTValidator(t, ownerUpdate)
			if decode[record](t, ownerUpdate).Revision != 2 {
				t.Error("maintainer revision did not advance")
			}
			ownerFresh := s.get(t, ownerPath)
			s.request(http.MethodDelete, ownerPath, nil, condition(ownerFresh.header.Get("ETag"))).want(t, 204)
			s.request(http.MethodDelete, ownerPath, nil, condition(ownerFresh.header.Get("ETag"))).
				wantError(t, 404, "not_found")
			invalidCreator := jsonBody(t, map[string]string{
				"title": "Orphan", "version": "1", "company": "Example", "maintainer_id": owner.ID,
			})
			s.request(http.MethodPost, "/v1/applications", invalidCreator, nil).
				wantError(t, 422, "invalid_reference")
			for _, path := range []string{"/v1/maintainers", "/v1/applications"} {
				result := decode[page](t, s.get(t, path))
				if result.Items == nil || len(result.Items) != 0 || result.NextAfter != "" {
					t.Errorf("empty collection is not an empty array: %#v", result)
				}
			}
		})
	}
}

func TestCreatorContactChangesDoNotRewriteApplications(t *testing.T) {
	for _, description := range backendRegistry(t).Descriptions() {
		backend := description.Name
		t.Run(backend, func(t *testing.T) {
			s := startServer(t, backend, filepath.Join(t.TempDir(), "metadata.db"))
			created := s.maintainer(t, "Creator", "creator@example.test")
			creator := decode[record](t, created)
			application := s.application(t, creator.ID, "Inventory")
			path := "/v1/applications/" + decode[record](t, application).ID
			before := s.get(t, path)

			body := jsonBody(t, map[string]string{
				"name": "Renamed Creator", "email": "renamed@example.test",
			})
			updated := s.request(http.MethodPut, "/v1/maintainers/"+creator.ID, body,
				condition(created.header.Get("ETag"))).want(t, 200)
			if decode[record](t, updated).ID != creator.ID {
				t.Fatal("contact replacement changed creator identity")
			}
			after := s.get(t, path)
			if !bytes.Equal(after.body, before.body) || after.header.Get("ETag") != before.header.Get("ETag") {
				t.Fatal("a creator contact edit changed the application representation or validator")
			}

			other := decode[record](t, s.maintainer(t, "Other", "other@example.test"))
			reassign := jsonBody(t, map[string]string{
				"title": "Inventory", "version": "1.0", "company": "Example",
				"maintainer_id": other.ID,
			})
			s.request(http.MethodPut, path, reassign, condition(before.header.Get("ETag"))).
				wantError(t, 400, "invalid_json")
			unchanged := s.get(t, path)
			if !bytes.Equal(unchanged.body, before.body) || unchanged.header.Get("ETag") != before.header.Get("ETag") {
				t.Fatal("rejected creator reassignment changed the application")
			}

			edit := jsonBody(t, map[string]string{
				"title": "Updated Inventory", "version": "1.1", "company": "Example",
			})
			s.request(http.MethodPut, path, edit, condition(before.header.Get("ETag"))).want(t, 200)
		})
	}
}

func TestValidationAndHTTPContract(t *testing.T) {
	s := startServer(t, "sqlite", filepath.Join(t.TempDir(), "metadata.db"))
	owner := decode[record](t, s.maintainer(t, "Owner", "owner@example.test"))
	path := "/v1/maintainers/" + owner.ID
	valid := []byte(`{"name":"Owner","email":"owner@example.test"}`)
	cases := []struct {
		name, method, path, body string
		headers                  http.Header
		status                   int
		code                     string
	}{
		{"unknown field", "POST", "/v1/maintainers", `{"name":"A","email":"a@example.test","role":"admin"}`, nil, 400, "invalid_json"},
		{"trailing document", "POST", "/v1/maintainers", `{"name":"A","email":"a@example.test"} {}`, nil, 400, "invalid_json"},
		{"wrong root", "POST", "/v1/maintainers", `[]`, nil, 400, "invalid_json"},
		{"invalid UTF8", "POST", "/v1/maintainers", "{\"name\":\"\xff\",\"email\":\"a@example.test\"}", nil, 400, "invalid_json"},
		{"missing required", "POST", "/v1/maintainers", `{"email":"a@example.test"}`, nil, 422, "validation_failed"},
		{"null required", "POST", "/v1/maintainers", `{"name":null,"email":"a@example.test"}`, nil, 422, "validation_failed"},
		{"display-name email", "POST", "/v1/maintainers", `{"name":"A","email":"Example <a@example.test>"}`, nil, 422, "validation_failed"},
		{"unicode folding to ASCII", "POST", "/v1/maintainers", `{"name":"A","email":"\u212a@example.test"}`, nil, 422, "validation_failed"},
		{"oversized field", "POST", "/v1/maintainers", `{"name":"` + strings.Repeat("n", 129) + `","email":"a@example.test"}`, nil, 422, "validation_failed"},
		{"oversized body", "POST", "/v1/maintainers", `{"name":"` + strings.Repeat("n", 65536) + `","email":"a@example.test"}`, nil, 413, "body_too_large"},
		{"wrong media", "POST", "/v1/maintainers", string(valid), http.Header{"Content-Type": []string{"text/plain"}}, 415, "unsupported_media_type"},
		{"missing media", "POST", "/v1/maintainers", string(valid), http.Header{"Content-Type": []string{""}}, 415, "unsupported_media_type"},
		{"invalid path ID", "GET", "/v1/maintainers/not-a-uuid", "", nil, 400, "invalid_id"},
		{"invalid cursor", "GET", "/v1/maintainers?after=bad", "", nil, 400, "invalid_id"},
		{"limit zero", "GET", "/v1/maintainers?limit=0", "", nil, 400, "invalid_query"},
		{"limit over cap", "GET", "/v1/maintainers?limit=101", "", nil, 400, "invalid_query"},
		{"limit fraction", "GET", "/v1/maintainers?limit=1.5", "", nil, 400, "invalid_query"},
		{"duplicate query", "GET", "/v1/maintainers?limit=1&limit=2", "", nil, 400, "invalid_query"},
		{"unknown query", "GET", "/v1/maintainers?sort=email", "", nil, 400, "invalid_query"},
		{"unsupported method", "PATCH", path, string(valid), nil, 405, "method_not_allowed"},
		{"unknown route", "GET", "/v1/not-a-resource", "", nil, 404, "not_found"},
		{"missing precondition", "PUT", path, string(valid), nil, 428, "precondition_required"},
		{"missing delete precondition", "DELETE", path, "", nil, 428, "precondition_required"},
		{"validation before precondition", "PUT", path, `{}`, nil, 422, "validation_failed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var body []byte
			if test.body != "" {
				body = []byte(test.body)
			}
			result := s.request(test.method, test.path, body, test.headers)
			result.wantError(t, test.status, test.code)
			if test.status == 405 && result.header.Get("Allow") == "" {
				t.Error("405 response omitted Allow")
			}
		})
	}
	app := s.application(t, owner.ID, "App")
	appPath := "/v1/applications/" + decode[record](t, app).ID
	for name, extra := range map[string]string{
		"immutable creator": `"maintainer_id":"` + owner.ID + `"`,
		"server-managed ID": `"id":"` + owner.ID + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"title":"A","version":"1","company":"Example",` + extra + `}`)
			s.request(http.MethodPut, appPath, body, condition(app.header.Get("ETag"))).
				wantError(t, 400, "invalid_json")
		})
	}
	for _, website := range []string{"file:///etc/passwd", "https://user:pass@example.test", "/relative"} {
		body := jsonBody(t, map[string]string{
			"title": "A", "version": "1", "company": "Example", "website": website,
		})
		s.request(http.MethodPut, appPath, body, condition(app.header.Get("ETag"))).
			wantError(t, 422, "validation_failed")
	}
	nulls := []byte(`{"title":"A","version":"1","company":"Example","website":null,"description":null}`)
	nullResponse := s.request(http.MethodPut, appPath, nulls, condition(app.header.Get("ETag"))).want(t, 200)
	assertNoPUTValidator(t, nullResponse)
	if value := decode[record](t, nullResponse); value.Website != "" || value.Description != "" {
		t.Error("optional null strings did not become empty")
	}
}

func TestConditionalHeaderSemantics(t *testing.T) {
	s := startServer(t, "sqlite", filepath.Join(t.TempDir(), "metadata.db"))
	initial := s.maintainer(t, "Owner", "owner@example.test")
	item := decode[record](t, initial)
	path := "/v1/maintainers/" + item.ID
	other := s.maintainer(t, "Other", "other@example.test")
	app := s.application(t, decode[record](t, other).ID, "Other App")
	body := []byte(`{"name":"Edited","email":"owner@example.test"}`)
	cases := []struct {
		name   string
		header string
		status int
		code   string
	}{
		{"blank condition", "", 400, "invalid_precondition"},
		{"wildcard condition", "*", 400, "invalid_precondition"},
		{"unterminated tag", `"unterminated`, 400, "invalid_precondition"},
		{"unquoted tag", `unquoted`, 400, "invalid_precondition"},
		{"invalid weak prefix", `w/"lowercase-weak"`, 400, "invalid_precondition"},
		{"header exceeds byte limit", strings.Repeat("x", 1025), 400, "invalid_precondition"},
		{"unknown tag at byte limit", `"` + strings.Repeat("x", 1022) + `"`, 412, "precondition_failed"},
		{"too many tags", strings.TrimSuffix(strings.Repeat(`"x",`, 9), ","), 400, "invalid_precondition"},
		{"weak current tag", `W/` + initial.header.Get("ETag"), 412, "precondition_failed"},
		{"quoted weak wildcard", `W/"*"`, 412, "precondition_failed"},
		{"comma inside opaque tag", `"unknown,opaque-tag"`, 412, "precondition_failed"},
		{"zero revision", `"m1:` + item.ID + `:0"`, 412, "precondition_failed"},
		{"another maintainer's tag", other.header.Get("ETag"), 412, "precondition_failed"},
		{"another resource kind's tag", app.header.Get("ETag"), 412, "precondition_failed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s.request(http.MethodPut, path, body, condition(test.header)).
				wantError(t, test.status, test.code)
		})
	}
	unchanged := s.get(t, path)
	if !bytes.Equal(unchanged.body, initial.body) || unchanged.header.Get("ETag") != initial.header.Get("ETag") {
		t.Fatal("rejected preconditions changed the resource")
	}
	list := condition(`"opaque,comma", W/"weak", ` + initial.header.Get("ETag"))
	first := s.request(http.MethodPut, path, body, list).want(t, 200)
	assertNoPUTValidator(t, first)
	fresh := s.get(t, path)
	repeated := http.Header{"If-Match": []string{`"other"`, fresh.header.Get("ETag")}}
	second := s.request(http.MethodPut, path, body, repeated).want(t, 200)
	if decode[record](t, second).Revision != 3 {
		t.Error("unchanged replacement did not advance the revision")
	}
	fresh = s.get(t, path)
	eightTags := condition(strings.Repeat(`"unused", `, 7) + fresh.header.Get("ETag"))
	s.request(http.MethodPut, path, body, eightTags).want(t, 200)
	absent := "/v1/maintainers/00000000-0000-4000-8000-000000000000"
	s.request(http.MethodPut, absent, body, condition(`"unknown"`)).wantError(t, 404, "not_found")
}

func TestBodyLimitBoundary(t *testing.T) {
	s := startServer(t, "sqlite", filepath.Join(t.TempDir(), "metadata.db"))
	body := []byte(`{"name":"Boundary","email":"boundary@example.test"}`)
	exact := append(body, bytes.Repeat([]byte(" "), 65536-len(body))...)
	s.request(http.MethodPost, "/v1/maintainers", exact, nil).want(t, 201)
	over := append(append([]byte(nil), exact...), ' ')
	s.request(http.MethodPost, "/v1/maintainers", over, nil).wantError(t, 413, "body_too_large")
	multibyte := jsonBody(t, map[string]string{
		"name": strings.Repeat("\u00e9", 65), "email": "multibyte@example.test",
	})
	s.request(http.MethodPost, "/v1/maintainers", multibyte, nil).
		wantError(t, 422, "validation_failed")
}

func TestPaginationAndCreatorFilter(t *testing.T) {
	for _, description := range backendRegistry(t).Descriptions() {
		backend := description.Name
		t.Run(backend, func(t *testing.T) {
			s := startServer(t, backend, filepath.Join(t.TempDir(), "metadata.db"))
			var expected []string
			var owners []record
			for i := 0; i < 5; i++ {
				owner := decode[record](t, s.maintainer(t, fmt.Sprintf("Owner %d", i), fmt.Sprintf("owner%d@example.test", i)))
				owners = append(owners, owner)
				expected = append(expected, owner.ID)
				s.application(t, owner.ID, fmt.Sprintf("App %d", i))
			}
			sort.Strings(expected)
			var actual []string
			cursor := ""
			for pageNumber := 0; pageNumber < 4; pageNumber++ {
				query := "/v1/maintainers?limit=2"
				if cursor != "" {
					query += "&after=" + cursor
				}
				result := decode[page](t, s.get(t, query))
				if len(result.Items) > 2 || result.Items == nil {
					t.Fatalf("invalid page: %#v", result)
				}
				for _, item := range result.Items {
					actual = append(actual, item.ID)
				}
				if result.NextAfter == "" {
					break
				}
				if len(result.Items) != 2 || result.NextAfter != result.Items[len(result.Items)-1].ID {
					t.Fatalf("invalid next_after: %#v", result)
				}
				cursor = result.NextAfter
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("pagination skipped, duplicated or reordered IDs: got %v want %v", actual, expected)
			}
			for _, owner := range owners {
				result := decode[page](t, s.get(t, "/v1/applications?maintainer_id="+owner.ID))
				if len(result.Items) != 1 || result.Items[0].MaintainerID != owner.ID {
					t.Errorf("creator filter leaked other records: %#v", result)
				}
			}
			empty := decode[page](t, s.get(t, "/v1/applications?maintainer_id=00000000-0000-4000-8000-000000000000"))
			if empty.Items == nil || len(empty.Items) != 0 {
				t.Errorf("nonexistent creator filter is not an empty array: %#v", empty)
			}
		})
	}
}

func TestConcurrentWritersAcrossRestart(t *testing.T) {
	for _, kind := range []string{"maintainer", "application"} {
		t.Run(kind, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "durable metadata.db")
			s := startServer(t, "sqlite", db)
			owner := s.maintainer(t, "Owner", "owner@example.test")
			initial := owner
			path := "/v1/maintainers/" + decode[record](t, owner).ID
			bodies := [][]byte{
				[]byte(`{"name":"Writer A","email":"owner@example.test"}`),
				[]byte(`{"name":"Writer B","email":"owner@example.test"}`),
			}
			if kind == "application" {
				initial = s.application(t, decode[record](t, owner).ID, "Original")
				path = "/v1/applications/" + decode[record](t, initial).ID
				bodies = [][]byte{
					[]byte(`{"title":"Writer A","version":"2","company":"Example"}`),
					[]byte(`{"title":"Writer B","version":"2","company":"Example"}`),
				}
			}
			clientA, clientB := s.get(t, path), s.get(t, path)
			tag := clientA.header.Get("ETag")
			if tag == "" || tag != clientB.header.Get("ETag") {
				t.Fatal("both clients did not observe the same initial tag")
			}
			results := raceWrites(s, s, path, tag, bodies)
			winner := -1
			for i, result := range results {
				if result.err != nil {
					t.Fatal(result.err)
				}
				switch result.status {
				case 200:
					if winner != -1 {
						t.Fatal("both concurrent writers succeeded")
					}
					winner = i
					assertNoPUTValidator(t, result)
				case 412:
					result.wantError(t, 412, "precondition_failed")
				default:
					t.Fatalf("unexpected concurrent result: %d %s", result.status, result.body)
				}
			}
			if winner == -1 {
				t.Fatal("neither concurrent writer succeeded")
			}
			current := s.get(t, path)
			if decode[record](t, current).Revision != 2 ||
				!reflect.DeepEqual(decode[record](t, current), decode[record](t, results[winner])) {
				t.Fatalf("stored content/revision does not match the winning writer: %s", current.body)
			}
			s.request(http.MethodDelete, path, nil, condition(tag)).
				wantError(t, 412, "precondition_failed")
			retry := s.request(http.MethodPut, path, bodies[1-winner], condition(current.header.Get("ETag"))).want(t, 200)
			if decode[record](t, retry).Revision != 3 {
				t.Fatal("fresh-tag retry did not apply once")
			}
			beforeRestart := s.get(t, path)
			s.stop(t, false)
			reopened := startServer(t, "sqlite", db)
			afterRestart := reopened.get(t, path)
			if !reflect.DeepEqual(decode[record](t, beforeRestart), decode[record](t, afterRestart)) ||
				beforeRestart.header.Get("ETag") != afterRestart.header.Get("ETag") {
				t.Fatal("abrupt process restart lost an acknowledged mutation or changed its validator")
			}
			t.Log("same-tag writers returned 200 and 412; revision advanced once; fresh-tag retry and abrupt restart preserved the result")
		})
	}
}

func raceWrites(a, b *server, path, tag string, bodies [][]byte) [2]response {
	start := make(chan struct{})
	var wg sync.WaitGroup
	var results [2]response
	for i, s := range []*server{a, b} {
		wg.Add(1)
		go func(i int, s *server) {
			defer wg.Done()
			<-start
			results[i] = s.request(http.MethodPut, path, bodies[i], condition(tag))
		}(i, s)
	}
	close(start)
	wg.Wait()
	return results
}

func TestConcurrentProcessesShareSQLiteCAS(t *testing.T) {
	db := filepath.Join(t.TempDir(), "shared.db")
	a, b := startServer(t, "sqlite", db), startServer(t, "sqlite", db)
	initial := a.maintainer(t, "Owner", "owner@example.test")
	path := "/v1/maintainers/" + decode[record](t, initial).ID
	for i := 0; i < 12; i++ {
		first, second := a.get(t, path), b.get(t, path)
		if first.header.Get("ETag") != second.header.Get("ETag") {
			t.Fatal("server processes disagree before concurrent writes")
		}
		bodies := [][]byte{
			jsonBody(t, map[string]string{"name": fmt.Sprintf("A-%d", i), "email": "owner@example.test"}),
			jsonBody(t, map[string]string{"name": fmt.Sprintf("B-%d", i), "email": "owner@example.test"}),
		}
		results := raceWrites(a, b, path, first.header.Get("ETag"), bodies)
		statuses := []int{results[0].status, results[1].status}
		sort.Ints(statuses)
		if results[0].err != nil || results[1].err != nil || !reflect.DeepEqual(statuses, []int{200, 412}) {
			t.Fatalf("cross-process race did not have exactly one winner: %#v", results)
		}
		current := decode[record](t, a.get(t, path))
		if current.Revision != int64(i+2) {
			t.Fatalf("cross-process revision advanced incorrectly: %d", current.Revision)
		}
	}
}

func TestApplicationCreateVersusCreatorDelete(t *testing.T) {
	for _, description := range backendRegistry(t).Descriptions() {
		backend := description.Name
		t.Run(backend, func(t *testing.T) {
			s := startServer(t, backend, filepath.Join(t.TempDir(), "metadata.db"))
			for iteration := 0; iteration < 12; iteration++ {
				created := s.maintainer(t, "Creator", fmt.Sprintf("creator%d@example.test", iteration))
				owner := decode[record](t, created)
				body := jsonBody(t, map[string]string{
					"title": "App", "version": "1", "company": "Example", "maintainer_id": owner.ID,
				})
				start := make(chan struct{})
				var wg sync.WaitGroup
				var create, remove response
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					create = s.request(http.MethodPost, "/v1/applications", body, nil)
				}()
				go func() {
					defer wg.Done()
					<-start
					remove = s.request(http.MethodDelete, "/v1/maintainers/"+owner.ID, nil, condition(created.header.Get("ETag")))
				}()
				close(start)
				wg.Wait()
				if create.err != nil || remove.err != nil {
					t.Fatalf("creator race request failed: %v %v", create.err, remove.err)
				}
				switch {
				case create.status == 201 && remove.status == 409:
					remove.wantError(t, 409, "maintainer_in_use")
					s.get(t, "/v1/maintainers/"+owner.ID)
					s.get(t, "/v1/applications/"+decode[record](t, create).ID)
				case create.status == 422 && remove.status == 204:
					create.wantError(t, 422, "invalid_reference")
					items := decode[page](t, s.get(t, "/v1/applications?maintainer_id="+owner.ID))
					if len(items.Items) != 0 {
						t.Fatal("application references a deleted creator")
					}
				default:
					t.Fatalf("invalid creator race result: create=%d %s delete=%d %s", create.status, create.body, remove.status, remove.body)
				}
			}
		})
	}
}
