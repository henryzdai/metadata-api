package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metadata-api/internal/metadata"
)

type testCredential struct {
	id    string
	token string
}

type authGrant struct {
	ID          string   `json:"id"`
	Role        string   `json:"role"`
	TokenSHA256 []string `json:"token_sha256"`
}

func newCredential(t *testing.T) testCredential {
	t.Helper()
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	return testCredential{id: metadata.NewUUID(), token: base64.RawURLEncoding.EncodeToString(secret[:])}
}

func tokenHash(token string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}

func grant(credential testCredential, role string) authGrant {
	return authGrant{ID: credential.id, Role: role, TokenSHA256: []string{tokenHash(credential.token)}}
}

func writeAuthFile(t *testing.T, path string, principals ...authGrant) {
	t.Helper()
	body := jsonBody(t, struct {
		Principals []authGrant `json:"principals"`
	}{Principals: principals})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func authHeaders(token string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + token}}
}

func authCondition(token, tag string) http.Header {
	headers := authHeaders(token)
	headers.Set("If-Match", tag)
	return headers
}

func TestAuthenticatedRolesPreserveCRUDAndCreatorRules(t *testing.T) {
	for _, description := range backendRegistry(t).Descriptions() {
		t.Run(description.Name, func(t *testing.T) {
			writer, reader := newCredential(t), newCredential(t)
			authFile := filepath.Join(t.TempDir(), "tokens.json")
			writeAuthFile(t, authFile, grant(writer, "write"), grant(reader, "read"))
			s := startServer(t, description.Name, filepath.Join(t.TempDir(), "roles.db"),
				"--auth-mode=tokens", "--auth-file", authFile)
			for _, path := range []string{"/healthz", "/readyz"} {
				s.get(t, path)
				s.request(http.MethodGet, path, nil, authHeaders("not-a-token")).want(t, http.StatusOK)
			}
			missing := s.request(http.MethodGet, "/v1/maintainers", nil, nil)
			missing.wantError(t, http.StatusUnauthorized, "unauthenticated")
			if missing.header.Get("WWW-Authenticate") != `Bearer realm="metadata-api"` ||
				missing.header.Get("Cache-Control") != "no-store" {
				t.Fatal("authentication failure omitted its challenge or safe cache policy")
			}
			ownerBody := jsonBody(t, map[string]string{"name": "Creator", "email": "creator@example.test"})
			createdOwner := s.request(http.MethodPost, "/v1/maintainers", ownerBody, authHeaders(writer.token)).
				want(t, http.StatusCreated)
			owner := decode[record](t, createdOwner)
			ownerPath := "/v1/maintainers/" + owner.ID
			appBody := jsonBody(t, map[string]string{
				"title": "App", "version": "1", "company": "Example", "maintainer_id": owner.ID,
			})
			createdApp := s.request(http.MethodPost, "/v1/applications", appBody, authHeaders(writer.token)).
				want(t, http.StatusCreated)
			appPath := "/v1/applications/" + decode[record](t, createdApp).ID
			for _, path := range []string{"/v1/maintainers", ownerPath, "/v1/applications", appPath} {
				got := s.request(http.MethodGet, path, nil, authHeaders(reader.token)).want(t, http.StatusOK)
				if got.header.Get("Cache-Control") != "no-store" {
					t.Fatal("authenticated metadata response can be cached")
				}
			}
			for _, target := range []struct{ collection, resource string }{
				{"/v1/maintainers", ownerPath}, {"/v1/applications", appPath},
			} {
				s.request(http.MethodPost, target.collection, []byte("{"), authHeaders(reader.token)).
					wantError(t, http.StatusForbidden, "forbidden")
				s.request(http.MethodPut, target.resource, []byte("{"), authHeaders(reader.token)).
					wantError(t, http.StatusForbidden, "forbidden")
				s.request(http.MethodDelete, target.resource, nil, authHeaders(reader.token)).
					wantError(t, http.StatusForbidden, "forbidden")
			}
			for _, before := range []response{createdOwner, createdApp} {
				path := before.header.Get("Location")
				after := s.request(http.MethodGet, path, nil, authHeaders(writer.token)).want(t, http.StatusOK)
				if !bytes.Equal(before.body, after.body) || before.header.Get("ETag") != after.header.Get("ETag") {
					t.Fatal("denied mutation changed a resource or its validator")
				}
			}
			s.request(http.MethodDelete, ownerPath, nil, authCondition(writer.token, createdOwner.header.Get("ETag"))).
				wantError(t, http.StatusConflict, "maintainer_in_use")
			ownerEdit := jsonBody(t, map[string]string{"name": "Renamed", "email": "renamed@example.test"})
			updatedOwner := s.request(http.MethodPut, ownerPath, ownerEdit, authCondition(writer.token, createdOwner.header.Get("ETag"))).
				want(t, http.StatusOK)
			if updatedOwner.header.Get("ETag") != "" {
				t.Fatal("authenticated PUT returned a transformed validator")
			}
			appAfterContact := s.request(http.MethodGet, appPath, nil, authHeaders(reader.token)).want(t, http.StatusOK)
			if !bytes.Equal(appAfterContact.body, createdApp.body) ||
				appAfterContact.header.Get("ETag") != createdApp.header.Get("ETag") {
				t.Fatal("authenticated contact editing changed an Application")
			}
			appEdit := jsonBody(t, map[string]string{"title": "Edited", "version": "2", "company": "Example"})
			updatedApp := s.request(http.MethodPut, appPath, appEdit, authCondition(writer.token, createdApp.header.Get("ETag"))).
				want(t, http.StatusOK)
			if updatedApp.header.Get("ETag") != "" {
				t.Fatal("authenticated Application PUT returned an ETag")
			}
			for _, path := range []string{appPath, ownerPath} {
				current := s.request(http.MethodGet, path, nil, authHeaders(writer.token)).want(t, http.StatusOK)
				s.request(http.MethodDelete, path, nil, authCondition(writer.token, current.header.Get("ETag"))).
					want(t, http.StatusNoContent)
			}
		})
	}
}

func TestAuthenticationRejectsAmbiguousAndSpoofedCredentials(t *testing.T) {
	writer, reader, outsider := newCredential(t), newCredential(t), newCredential(t)
	path := filepath.Join(t.TempDir(), "tokens.json")
	writeAuthFile(t, path, grant(writer, "write"), grant(reader, "read"))
	s := startServer(t, "memory", "", "--auth-mode=tokens", "--auth-file", path)
	for _, headers := range []http.Header{
		nil,
		{"Authorization": []string{""}},
		{"Authorization": []string{"Bearer"}},
		{"Authorization": []string{"Basic " + writer.token}},
		authHeaders(outsider.token),
		{"Authorization": []string{"Bearer " + writer.token, "Bearer " + reader.token}},
		{"X-Principal-ID": []string{writer.id}, "X-Role": []string{"write"}, "X-Forwarded-User": []string{writer.id}},
	} {
		s.request(http.MethodGet, "/v1/maintainers", nil, headers).
			wantError(t, http.StatusUnauthorized, "unauthenticated")
	}
	spoofed := authHeaders(reader.token)
	spoofed.Set("X-Principal-ID", writer.id)
	spoofed.Set("X-Role", "write")
	s.request(http.MethodPost, "/v1/maintainers", []byte("{"), spoofed).
		wantError(t, http.StatusForbidden, "forbidden")
	s.request(http.MethodGet, "/unknown", nil, nil).wantError(t, http.StatusUnauthorized, "unauthenticated")
	s.request(http.MethodGet, "/unknown", nil, authHeaders(reader.token)).wantError(t, http.StatusNotFound, "not_found")
	s.request(http.MethodPatch, "/v1/maintainers", nil, authHeaders(reader.token)).
		wantError(t, http.StatusMethodNotAllowed, "method_not_allowed")
	rejectUnreadPost(t, s, nil).wantError(t, http.StatusUnauthorized, "unauthenticated")
	rejectUnreadPost(t, s, authHeaders(reader.token)).wantError(t, http.StatusForbidden, "forbidden")
	s.request(http.MethodGet, "/v1/maintainers", nil, authHeaders(reader.token)).want(t, http.StatusOK)
}

func TestLocalModeAndAuthorityAreExplicit(t *testing.T) {
	s := startServer(t, "memory", "", "--listen=localhost:0")
	s.maintainer(t, "Local", "local@example.test")
	for _, headers := range []http.Header{authHeaders(newCredential(t).token), {"Authorization": []string{""}}} {
		s.request(http.MethodGet, "/v1/maintainers", nil, headers).
			wantError(t, http.StatusBadRequest, "authentication_not_enabled")
	}
	address := strings.TrimPrefix(s.baseURL, "http://")
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{address, "localhost:" + port} {
		request, err := http.NewRequest(http.MethodGet, s.baseURL+"/v1/maintainers", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = host
		s.send(request).want(t, http.StatusOK)
	}
	for _, host := range []string{"attacker.invalid:" + port, "localhost:0", "127.0.0.1:0"} {
		request, err := http.NewRequest(http.MethodGet, s.baseURL+"/v1/maintainers", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = host
		request.Header.Set("X-Forwarded-Host", address)
		request.Header.Set("X-Forwarded-For", "127.0.0.1")
		s.send(request).wantError(t, http.StatusBadRequest, "invalid_host")
	}
}

func TestCredentialRotationRevocationAndContactSeparation(t *testing.T) {
	database := filepath.Join(t.TempDir(), "rotation.db")
	local := startServer(t, "sqlite", database)
	created := local.maintainer(t, "Contact", "contact@example.test")
	contact := decode[record](t, created)
	local.stop(t, false)

	writer, reader := newCredential(t), newCredential(t)
	reader.id = contact.ID
	authFile := filepath.Join(t.TempDir(), "tokens.json")
	writeAuthFile(t, authFile, grant(writer, "write"), grant(reader, "read"))
	s := startServer(t, "sqlite", database, "--auth-mode=tokens", "--auth-file", authFile)
	application := jsonBody(t, map[string]string{
		"title": "Not ownership", "version": "1", "company": "Example", "maintainer_id": contact.ID,
	})
	s.request(http.MethodPost, "/v1/applications", application, authHeaders(reader.token)).
		wantError(t, http.StatusForbidden, "forbidden")
	s.request(http.MethodGet, "/v1/maintainers/"+contact.ID, nil, authHeaders(writer.token)).
		want(t, http.StatusOK)
	s.stop(t, false)

	rotated := newCredential(t)
	rotated.id = writer.id
	overlap := grant(writer, "write")
	overlap.TokenSHA256 = append(overlap.TokenSHA256, tokenHash(rotated.token))
	writeAuthFile(t, authFile, overlap, grant(reader, "read"))
	s = startServer(t, "sqlite", database, "--auth-mode=tokens", "--auth-file", authFile)
	for _, token := range []string{writer.token, rotated.token} {
		s.request(http.MethodGet, "/v1/maintainers", nil, authHeaders(token)).want(t, http.StatusOK)
	}
	s.stop(t, false)

	writeAuthFile(t, authFile, grant(rotated, "read"), grant(reader, "read"))
	s = startServer(t, "sqlite", database, "--auth-mode=tokens", "--auth-file", authFile)
	s.request(http.MethodGet, "/v1/maintainers", nil, authHeaders(writer.token)).
		wantError(t, http.StatusUnauthorized, "unauthenticated")
	s.request(http.MethodGet, "/v1/maintainers", nil, authHeaders(rotated.token)).want(t, http.StatusOK)
	s.request(http.MethodPost, "/v1/applications", application, authHeaders(rotated.token)).
		wantError(t, http.StatusForbidden, "forbidden")
	after := s.request(http.MethodGet, "/v1/maintainers/"+contact.ID, nil, authHeaders(reader.token)).
		want(t, http.StatusOK)
	if !bytes.Equal(created.body, after.body) || created.header.Get("ETag") != after.header.Get("ETag") {
		t.Fatal("access-policy changes rewrote existing metadata")
	}
}

func TestInvalidAccessConfigurationHasNoStorageSideEffects(t *testing.T) {
	credential := newCredential(t)
	valid := jsonBody(t, struct {
		Principals []authGrant `json:"principals"`
	}{Principals: []authGrant{grant(credential, "write")}})
	tests := []struct {
		name string
		body []byte
		args []string
	}{
		{"malformed JSON", []byte("{"), nil},
		{"unknown member", []byte(`{"principals":[],"unexpected":"private-config-marker"}`), nil},
		{"duplicate member", []byte(`{"principals":[],"principals":[]}`), nil},
		{"case mismatched member", bytes.Replace(valid, []byte(`"principals"`), []byte(`"Principals"`), 1), nil},
		{"empty principals", []byte(`{"principals":[]}`), nil},
		{"null principals", []byte(`{"principals":null}`), nil},
		{"invalid role", bytes.Replace(valid, []byte(`"write"`), []byte(`"administrator"`), 1), nil},
		{"trailing document", append(append([]byte(nil), valid...), []byte(` {}`)...), nil},
		{"invalid UTF8", []byte{0xff}, nil},
		{"oversized file", append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), 256<<10)...), nil},
		{"file in local mode", valid, []string{"--auth-mode=local"}},
		{"unknown mode", valid, []string{"--auth-mode=invalid"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			authFile := filepath.Join(directory, "tokens.json")
			if err := os.WriteFile(authFile, test.body, 0o600); err != nil {
				t.Fatal(err)
			}
			database := filepath.Join(directory, "must-not-exist.db")
			args := []string{"--listen=127.0.0.1:0", "--db", database}
			if len(test.args) == 0 {
				args = append(args, "--auth-mode=tokens")
			} else {
				args = append(args, test.args...)
			}
			args = append(args, "--auth-file", authFile)
			assertRejectedAuthStartup(t, directory, database, args, credential)
		})
	}
	for _, test := range []struct {
		name string
		args []string
	}{
		{"missing file option", []string{"--auth-mode=tokens"}},
		{"missing file", []string{"--auth-mode=tokens", "--auth-file=missing.json"}},
		{"repeated security mode", []string{"--auth-mode=local", "--auth-mode=local"}},
		{"nonlocal bind", []string{"--listen=198.51.100.10:0"}},
		{"listener DNS name", []string{"--listen=example.invalid:0"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			database := filepath.Join(directory, "must-not-exist.db")
			args := append([]string{"--listen=127.0.0.1:0", "--db", database}, test.args...)
			assertRejectedAuthStartup(t, directory, database, args, credential)
		})
	}
}

func assertRejectedAuthStartup(t *testing.T, directory, database string, args []string, credential testCredential) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, serverBinary, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if ctx.Err() != nil || err == nil || strings.Contains(string(output), `"event":"listening"`) {
		t.Fatal("unsafe access configuration did not fail before listening")
	}
	if _, err := os.Stat(database); !os.IsNotExist(err) {
		t.Fatal("invalid access configuration touched its storage file")
	}
	for _, forbidden := range []string{credential.token, tokenHash(credential.token), "private-config-marker"} {
		if strings.Contains(string(output), forbidden) {
			t.Fatal("startup diagnostics exposed credential configuration")
		}
	}
}

func TestAccessLogsExcludeCredentials(t *testing.T) {
	writer, reader, outsider := newCredential(t), newCredential(t), newCredential(t)
	authFile := filepath.Join(t.TempDir(), "tokens.json")
	writeAuthFile(t, authFile, grant(writer, "write"), grant(reader, "read"))
	s := startServer(t, "memory", "", "--auth-mode=tokens", "--auth-file", authFile)
	s.request(http.MethodGet, "/v1/maintainers", nil, authHeaders(writer.token)).want(t, http.StatusOK)
	s.request(http.MethodGet, "/v1/maintainers", nil, authHeaders(outsider.token)).
		wantError(t, http.StatusUnauthorized, "unauthenticated")
	s.request(http.MethodPost, "/v1/maintainers", []byte("{"), authHeaders(reader.token)).
		wantError(t, http.StatusForbidden, "forbidden")
	s.stop(t, false)
	logs, err := os.ReadFile(s.stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(logs, []byte(`"status":401`)) || !bytes.Contains(logs, []byte(`"status":403`)) {
		t.Fatal("credential exclusion was checked against missing access outcomes")
	}
	for _, key := range []testCredential{writer, reader, outsider} {
		if bytes.Contains(logs, []byte(key.token)) || bytes.Contains(logs, []byte(tokenHash(key.token))) {
			t.Fatal("access logs contain a token or verifier digest")
		}
	}
}
