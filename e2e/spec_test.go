package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestOpenAPIContract(t *testing.T) {
	document := openAPIDocument(t)
	if document["openapi"] != "3.1.0" {
		t.Fatalf("unexpected OpenAPI version: %v", document["openapi"])
	}
	walkReferences(t, document, document, "#")
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI paths are absent")
	}
	for _, collection := range []string{"/v1/maintainers", "/v1/applications"} {
		for _, endpoint := range []struct {
			path    string
			methods []string
		}{
			{collection, []string{"get", "post"}},
			{collection + "/{id}", []string{"get", "put", "delete"}},
		} {
			item, ok := paths[endpoint.path].(map[string]any)
			if !ok {
				t.Fatalf("missing documented path %s", endpoint.path)
			}
			for _, method := range endpoint.methods {
				operation, ok := item[method].(map[string]any)
				if !ok {
					t.Errorf("missing documented operation %s %s", method, endpoint.path)
					continue
				}
				if id, ok := operation["operationId"].(string); !ok || id == "" {
					t.Errorf("missing operationId for %s %s", method, endpoint.path)
				}
			}
			if put, ok := item["put"].(map[string]any); ok {
				responses, ok := put["responses"].(map[string]any)
				if !ok {
					t.Fatal("PUT responses are missing")
				}
				success, ok := responses["200"].(map[string]any)
				if !ok {
					t.Fatal("PUT 200 response is missing")
				}
				headers, ok := success["headers"].(map[string]any)
				if !ok {
					t.Fatal("PUT response headers are missing")
				}
				if _, found := headers["ETag"]; found {
					t.Error("OpenAPI promises a forbidden transformed-PUT validator")
				}
				if responses["412"] == nil || responses["428"] == nil {
					t.Error("OpenAPI omits the conditional-write failures")
				}
			}
		}
	}
}

func TestOpenAPIResourceBounds(t *testing.T) {
	document := openAPIDocument(t)
	for _, name := range []string{"MaintainerPage", "ApplicationPage"} {
		page := openAPIObject(t, document, "components", "schemas", name)
		budget, ok := page["x-max-response-bytes"].(float64)
		if !ok || budget != float64(1<<20) {
			t.Errorf("%s does not document the exact encoded-page budget", name)
		}
	}
	unavailable := openAPIObject(t, document, "components", "responses", "Unavailable")
	description, ok := unavailable["description"].(string)
	if !ok || !strings.Contains(description, "overloaded") || !strings.Contains(description, "resource_capacity_exceeded") {
		t.Error("OpenAPI omits overload or resource-capacity outcomes")
	}
	retry := openAPIObject(t, unavailable, "headers", "Retry-After")
	if retry["$ref"] != "#/components/headers/RetryAfter" {
		t.Error("OpenAPI omits retry guidance for overload")
	}
	openAPIObject(t, document, "paths", "/healthz", "get", "responses", "503")
}

func openAPIDocument(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func openAPIObject(t *testing.T, document map[string]any, keys ...string) map[string]any {
	t.Helper()
	current := document
	for _, key := range keys {
		object, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("missing OpenAPI object %s", strings.Join(keys, "/"))
		}
		current = object
	}
	return current
}

func walkReferences(t *testing.T, document map[string]any, value any, at string) {
	t.Helper()
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#/") {
					t.Fatalf("nonlocal or invalid reference at %s: %v", at, child)
				}
				var current any = document
				for _, token := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
					object, ok := current.(map[string]any)
					if !ok {
						t.Fatalf("unresolved reference %s at %s", ref, at)
					}
					current, ok = object[token]
					if !ok {
						t.Fatalf("unresolved reference %s at %s", ref, at)
					}
				}
			}
			walkReferences(t, document, child, at+"/"+key)
		}
	case []any:
		for i, child := range node {
			walkReferences(t, document, child, fmt.Sprintf("%s/%d", at, i))
		}
	}
}
