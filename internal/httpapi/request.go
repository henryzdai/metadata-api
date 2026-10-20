package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"metadata-api/internal/metadata"
)

const maxBodyBytes = 65536

var maintainerInputFields = []string{"name", "email"}
var applicationInputFields = []string{"title", "version", "company", "website", "source", "license", "description"}
var applicationCreateFields = []string{"title", "version", "company", "website", "source", "license", "description", "maintainer_id"}

func decodeBody(w http.ResponseWriter, r *http.Request, target any, allowed []string) *problem {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return &problem{
			status:  http.StatusUnsupportedMediaType,
			code:    "unsupported_media_type",
			message: "Content-Type must be application/json.",
		}
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" ||
		parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8") {
		return &problem{
			status:  http.StatusUnsupportedMediaType,
			code:    "unsupported_media_type",
			message: "Content-Type must be application/json with UTF-8 encoding.",
		}
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &problem{
				status:  http.StatusRequestEntityTooLarge,
				code:    "body_too_large",
				message: "The request body exceeds 65536 bytes.",
			}
		}
		if r.Context().Err() != nil {
			return fromError(metadata.WrapError(metadata.Unavailable, r.Context().Err()))
		}
		return invalidJSON()
	}
	if !utf8.Valid(data) {
		return invalidJSON()
	}
	// Inspect keys first because encoding/json otherwise accepts duplicate and
	// case-mismatched field names when decoding into a struct.
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return invalidJSON()
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return invalidJSON()
		}
		name, ok := token.(string)
		if !ok || !slices.Contains(allowed, name) || seen[name] {
			return invalidJSON()
		}
		seen[name] = true
		var field json.RawMessage
		if err := decoder.Decode(&field); err != nil {
			return invalidJSON()
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return invalidJSON()
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return invalidJSON()
	}
	if err := json.Unmarshal(data, target); err != nil {
		return invalidJSON()
	}
	return nil
}

func invalidJSON() *problem {
	return &problem{
		status:  http.StatusBadRequest,
		code:    "invalid_json",
		message: "The body must be one UTF-8 JSON object with only supported fields and string or null values.",
	}
}

func parseQuery(raw string, application bool) (int, string, metadata.MaintainerID, *problem) {
	invalid := &problem{
		status:  http.StatusBadRequest,
		code:    "invalid_query",
		message: "Query parameters must be supported, unrepeated, and use a limit from 1 to 100.",
	}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return 0, "", "", invalid
	}
	for key, values := range query {
		if len(values) != 1 || !(key == "limit" || key == "after" || application && key == "maintainer_id") {
			return 0, "", "", invalid
		}
	}
	limit := 20
	if values, present := query["limit"]; present {
		for _, char := range values[0] {
			if char < '0' || char > '9' {
				return 0, "", "", invalid
			}
		}
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > 100 {
			return 0, "", "", invalid
		}
	}
	after := query.Get("after")
	if _, present := query["after"]; present && !metadata.ValidUUID(after) {
		return 0, "", "", fromError(metadata.NewError(metadata.InvalidID, nil))
	}
	owner := query.Get("maintainer_id")
	if _, present := query["maintainer_id"]; present && !metadata.ValidUUID(owner) {
		return 0, "", "", fromError(metadata.NewError(metadata.InvalidID, nil))
	}
	return limit, after, metadata.MaintainerID(owner), nil
}

func rejectQuery(raw string) *problem {
	if raw != "" {
		return &problem{
			status:  http.StatusBadRequest,
			code:    "invalid_query",
			message: "This endpoint does not accept query parameters.",
		}
	}
	return nil
}
