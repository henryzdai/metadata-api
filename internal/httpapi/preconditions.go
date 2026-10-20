package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"metadata-api/internal/metadata"
)

func entityTag(kind, id string, revision metadata.Revision) string {
	return `"` + kind + ":" + id + ":" + strconv.FormatInt(int64(revision), 10) + `"`
}

func parseIfMatch(header http.Header, kind, id string) ([]metadata.Revision, *problem) {
	values := header.Values("If-Match")
	if len(values) == 0 {
		return nil, &problem{
			status:  http.StatusPreconditionRequired,
			code:    "precondition_required",
			message: "If-Match is required.",
		}
	}
	value := strings.Join(values, ",")
	invalid := &problem{
		status:  http.StatusBadRequest,
		code:    "invalid_precondition",
		message: "If-Match must contain one to eight quoted entity tags within 1024 bytes; wildcard is not supported.",
	}
	if len(value) == 0 || len(value) > 1024 {
		return nil, invalid
	}
	expected := []metadata.Revision{}
	prefix := kind + ":" + id + ":"
	index, count := 0, 0
	for {
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		weak := strings.HasPrefix(value[index:], "W/")
		if weak {
			index += 2
		}
		if index >= len(value) || value[index] != '"' {
			return nil, invalid
		}
		index++
		start := index
		// A comma inside a quoted entity tag is content, not a list separator.
		for index < len(value) && value[index] != '"' {
			char := value[index]
			if char < 0x21 || char == 0x7f {
				return nil, invalid
			}
			index++
		}
		if index == len(value) {
			return nil, invalid
		}
		opaque := value[start:index]
		index++
		count++
		if count > 8 {
			return nil, invalid
		}
		// Only strong tags for this resource can supply expected store revisions.
		if !weak && strings.HasPrefix(opaque, prefix) {
			text := strings.TrimPrefix(opaque, prefix)
			revision, err := strconv.ParseInt(text, 10, 64)
			if err == nil && revision > 0 && strconv.FormatInt(revision, 10) == text {
				expected = append(expected, metadata.Revision(revision))
			}
		}
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index == len(value) {
			return expected, nil
		}
		if value[index] != ',' {
			return nil, invalid
		}
		index++
	}
}
