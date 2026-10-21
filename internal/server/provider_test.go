package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"metadata-api/internal/metadata"
	"metadata-api/internal/server"
	"metadata-api/internal/storage"
	"metadata-api/internal/storage/builtin"
	"metadata-api/internal/storage/memory"
	"metadata-api/internal/testsupport/probe"
)

func TestRegisteredProbeServesRealHTTPWithoutBackendChanges(t *testing.T) {
	recorder := &probe.Recorder{}
	provider := recorder.Provider()
	// Synthetic durability metadata proves Run does not infer it from the provider name.
	provider.Durable = true
	catalog, err := builtin.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseWithCatalog(t, catalog, "--listen=127.0.0.1:0", "--storage=probe", "--storage-option=probe-number=31")
	service := start(t, cfg, provider.Name, provider.Durable)
	for _, path := range []string{"/healthz", "/readyz"} {
		response := service.do(t, http.MethodGet, path, "")
		requireStatus(t, response, http.StatusOK)
		consume(t, response)
	}
	created := service.do(t, http.MethodPost, "/v1/maintainers",
		`{"name":"Probe Creator","email":"probe@example.test"}`)
	ownerPath, ownerTag := created.Header.Get("Location"), created.Header.Get("ETag")
	owner := decodeResponse[metadata.Maintainer](t, created, http.StatusCreated)
	if ownerPath != "/v1/maintainers/"+string(owner.ID) || ownerTag != fmt.Sprintf(`"m1:%s:1"`, owner.ID) {
		t.Fatal("probe maintainer creation did not return its location and validator")
	}
	if got := decodeResponse[metadata.Maintainer](t, service.do(t, http.MethodGet, ownerPath, ""), http.StatusOK); got != owner {
		t.Fatal("probe maintainer read differs from creation")
	}
	owners := decodeResponse[metadata.Page[metadata.Maintainer]](t,
		service.do(t, http.MethodGet, "/v1/maintainers", ""), http.StatusOK)
	if len(owners.Items) != 1 || owners.Items[0] != owner {
		t.Fatal("probe maintainer list differs from creation")
	}
	appCreated := service.do(t, http.MethodPost, "/v1/applications",
		fmt.Sprintf(`{"title":"Probe App","version":"1.0","company":"Example","maintainer_id":%q}`, owner.ID))
	appPath, appTag := appCreated.Header.Get("Location"), appCreated.Header.Get("ETag")
	app := decodeResponse[metadata.Application](t, appCreated, http.StatusCreated)
	if app.MaintainerID != owner.ID || appPath != "/v1/applications/"+string(app.ID) ||
		appTag != fmt.Sprintf(`"a1:%s:1"`, app.ID) {
		t.Fatal("probe application creation lost its creator, location, or validator")
	}
	apps := decodeResponse[metadata.Page[metadata.Application]](t,
		service.do(t, http.MethodGet, "/v1/applications?maintainer_id="+string(owner.ID), ""), http.StatusOK)
	if len(apps.Items) != 1 || apps.Items[0] != app {
		t.Fatal("probe application filtering lost the recorded creator")
	}
	assertResponseCode(t, service.do(t, http.MethodDelete, ownerPath, "", ownerTag),
		http.StatusConflict, "maintainer_in_use")
	replaced := service.do(t, http.MethodPut, ownerPath,
		`{"name":"Renamed Probe Creator","email":"renamed-probe@example.test"}`, ownerTag)
	if replaced.Header.Get("ETag") != "" {
		t.Fatal("transformed maintainer PUT returned an ETag")
	}
	updatedOwner := decodeResponse[metadata.Maintainer](t, replaced, http.StatusOK)
	if updatedOwner.ID != owner.ID || updatedOwner.Name != "Renamed Probe Creator" ||
		updatedOwner.Revision != 2 || updatedOwner.CreatedAt != owner.CreatedAt {
		t.Fatal("probe maintainer replacement changed identity or lost the revision")
	}
	read := service.do(t, http.MethodGet, ownerPath, "")
	ownerTag = read.Header.Get("ETag")
	if got := decodeResponse[metadata.Maintainer](t, read, http.StatusOK); got != updatedOwner || ownerTag == "" {
		t.Fatal("probe maintainer GET did not expose the current validator")
	}
	read = service.do(t, http.MethodGet, appPath, "")
	if read.Header.Get("ETag") != appTag ||
		decodeResponse[metadata.Application](t, read, http.StatusOK) != app {
		t.Fatal("editing creator contact details rewrote the probe application")
	}
	immutable := fmt.Sprintf(`{"title":"Moved","version":"2","company":"Example","maintainer_id":%q}`, owner.ID)
	assertResponseCode(t, service.do(t, http.MethodPut, appPath, immutable, appTag),
		http.StatusBadRequest, "invalid_json")
	replaced = service.do(t, http.MethodPut, appPath,
		`{"title":"Replaced Probe App","version":"2","company":"Example"}`, appTag)
	if replaced.Header.Get("ETag") != "" {
		t.Fatal("transformed application PUT returned an ETag")
	}
	updatedApp := decodeResponse[metadata.Application](t, replaced, http.StatusOK)
	if updatedApp.ID != app.ID || updatedApp.MaintainerID != owner.ID || updatedApp.Title != "Replaced Probe App" ||
		updatedApp.Revision != 2 || updatedApp.CreatedAt != app.CreatedAt {
		t.Fatal("probe application replacement lost its immutable creator or revision")
	}
	assertResponseCode(t, service.do(t, http.MethodDelete, appPath, "", appTag),
		http.StatusPreconditionFailed, "precondition_failed")
	read = service.do(t, http.MethodGet, appPath, "")
	appTag = read.Header.Get("ETag")
	if got := decodeResponse[metadata.Application](t, read, http.StatusOK); got != updatedApp || appTag == "" {
		t.Fatal("stale deletion changed the probe application")
	}
	assertResponseCode(t, service.do(t, http.MethodDelete, ownerPath, "", ownerTag),
		http.StatusConflict, "maintainer_in_use")
	for _, target := range []struct{ path, tag string }{{appPath, appTag}, {ownerPath, ownerTag}} {
		deleted := service.do(t, http.MethodDelete, target.path, "", target.tag)
		requireStatus(t, deleted, http.StatusNoContent)
		body, err := io.ReadAll(deleted.Body)
		consume(t, deleted)
		if err != nil || len(body) != 0 || deleted.Header.Get("ETag") != "" {
			t.Fatal("probe deletion returned a body, validator, or unreadable response")
		}
		assertResponseCode(t, service.do(t, http.MethodGet, target.path, ""), http.StatusNotFound, "not_found")
	}
	owners = decodeResponse[metadata.Page[metadata.Maintainer]](t,
		service.do(t, http.MethodGet, "/v1/maintainers", ""), http.StatusOK)
	apps = decodeResponse[metadata.Page[metadata.Application]](t,
		service.do(t, http.MethodGet, "/v1/applications", ""), http.StatusOK)
	if owners.Items == nil || len(owners.Items) != 0 || apps.Items == nil || len(apps.Items) != 0 {
		t.Fatal("probe deletion left records in the lists")
	}
	service.stop(t)
	if !reflect.DeepEqual(recorder.Opens(), []int{31}) || recorder.Closes() != 1 {
		t.Fatalf("probe lifecycle = %v opens, %d closes", recorder.Opens(), recorder.Closes())
	}
}

func TestSelectedProviderOpenFailuresNeverFallBack(t *testing.T) {
	cause := errors.New("private-provider-open-value")
	var typedNil *memory.Store
	for _, test := range []struct {
		name    string
		open    func(storage.OpenFunc, context.Context) (metadata.MetadataStore, error)
		want    error
		partial bool
	}{
		{"factory failure", func(storage.OpenFunc, context.Context) (metadata.MetadataStore, error) { return nil, cause }, cause, false},
		{"nil store", func(storage.OpenFunc, context.Context) (metadata.MetadataStore, error) { return nil, nil }, nil, false},
		{"typed nil store", func(storage.OpenFunc, context.Context) (metadata.MetadataStore, error) { return typedNil, nil }, nil, false},
		{"partial initialization", func(open storage.OpenFunc, ctx context.Context) (metadata.MetadataStore, error) {
			store, err := open(ctx)
			if err != nil {
				return nil, err
			}
			return store, cause
		}, cause, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := &probe.Recorder{}
			provider := recorder.Provider()
			prepare := provider.Prepare
			attempts, fallbackOpens := 0, 0
			provider.Prepare = func(values storage.Values) (storage.OpenFunc, error) {
				open, err := prepare(values)
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context) (metadata.MetadataStore, error) {
					attempts++
					return test.open(open, ctx)
				}, nil
			}
			fallback := storage.Provider{
				Name: "fallback",
				Prepare: func(storage.Values) (storage.OpenFunc, error) {
					return func(context.Context) (metadata.MetadataStore, error) {
						fallbackOpens++
						return memory.New(), nil
					}, nil
				},
			}
			catalog, err := storage.NewRegistry(fallback.Name, fallback, provider)
			if err != nil {
				t.Fatal(err)
			}
			cfg := parseWithCatalog(t, catalog, "--storage=probe", "--listen=127.0.0.1:0")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			err = server.Run(ctx, cfg, &stdout, &stderr)
			if err == nil || stdout.Len() != 0 || attempts != 1 || fallbackOpens != 0 ||
				test.want != nil && !errors.Is(err, test.want) ||
				strings.Contains(err.Error(), cause.Error()) || strings.Contains(stderr.String(), cause.Error()) {
				t.Fatalf("failure did not stop safely: attempts %d, fallback %d, stdout %q, error %v",
					attempts, fallbackOpens, stdout.String(), err)
			}
			wantStores := 0
			if test.partial {
				wantStores = 1
			}
			if len(recorder.Opens()) != wantStores || recorder.Closes() != wantStores {
				t.Fatal("failed initialization leaked or opened an unexpected probe store")
			}
		})
	}
}

func TestOpenedProviderClosesOnListenerAndOutputFailures(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	for _, test := range []struct {
		name   string
		listen string
		output io.Writer
	}{
		{"invalid listener", "not-an-address", io.Discard},
		{"occupied listener", listener.Addr().String(), io.Discard},
		{"output failure", "127.0.0.1:0", brokenWriter{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := &probe.Recorder{}
			catalog, err := builtin.NewRegistry(recorder.Provider())
			if err != nil {
				t.Fatal(err)
			}
			cfg := parseWithCatalog(t, catalog, "--storage=probe", "--listen="+test.listen)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := server.Run(ctx, cfg, test.output, io.Discard); err == nil {
				t.Fatal("startup failure was ignored")
			}
			if !reflect.DeepEqual(recorder.Opens(), []int{7}) || recorder.Closes() != 1 {
				t.Fatal("startup failure did not close the selected provider")
			}
		})
	}
}

func requireStatus(t *testing.T, response *http.Response, want int) {
	t.Helper()
	if response.StatusCode != want {
		body, err := io.ReadAll(response.Body)
		consume(t, response)
		t.Fatalf("HTTP status = %d, want %d; body %q, read error %v", response.StatusCode, want, body, err)
	}
}

func decodeResponse[T any](t *testing.T, response *http.Response, want int) T {
	t.Helper()
	requireStatus(t, response, want)
	defer consume(t, response)
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertResponseCode(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	problem := decodeResponse[struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}](t, response, status)
	if problem.Error.Code != code {
		t.Fatalf("HTTP error code = %q, want %q", problem.Error.Code, code)
	}
}
