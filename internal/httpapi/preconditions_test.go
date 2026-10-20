package httpapi

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"metadata-api/internal/metadata"
)

func TestIfMatchGrammarAndResourceBinding(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000001"
	tag := `"m1:` + id + `:1"`
	tests := []struct {
		name   string
		values []string
		want   []metadata.Revision
		code   string
	}{
		{"missing", nil, nil, "precondition_required"},
		{"blank", []string{""}, nil, "invalid_precondition"},
		{"whitespace", []string{" \t "}, nil, "invalid_precondition"},
		{"wildcard", []string{"*"}, nil, "invalid_precondition"},
		{"wildcard in list", []string{tag + ", *"}, nil, "invalid_precondition"},
		{"unquoted", []string{"opaque"}, nil, "invalid_precondition"},
		{"unterminated", []string{`"opaque`}, nil, "invalid_precondition"},
		{"embedded space", []string{`"not allowed"`}, nil, "invalid_precondition"},
		{"control", []string{"\"opaque\x7f\""}, nil, "invalid_precondition"},
		{"newline", []string{"\"opaque\n\""}, nil, "invalid_precondition"},
		{"trailing comma", []string{tag + ","}, nil, "invalid_precondition"},
		{"empty member", []string{tag + ",," + tag}, nil, "invalid_precondition"},
		{"bad weak prefix", []string{`w/` + tag}, nil, "invalid_precondition"},
		{"suffix", []string{tag + "junk"}, nil, "invalid_precondition"},
		{"too many", []string{strings.Repeat(`"x",`, 8) + tag}, nil, "invalid_precondition"},
		{"oversized", []string{`"` + strings.Repeat("x", 1023) + `"`}, nil, "invalid_precondition"},
		{"single", []string{tag}, []metadata.Revision{1}, ""},
		{"OWS", []string{" \t" + tag + "\t "}, []metadata.Revision{1}, ""},
		{"list and repeated lines", []string{`"unknown,with,commas", W/` + tag, `"m1:` + id + `:9", ` + tag}, []metadata.Revision{9, 1}, ""},
		{"eight", []string{strings.Repeat(`"x",`, 7) + tag}, []metadata.Revision{1}, ""},
		{"weak", []string{"W/" + tag}, []metadata.Revision{}, ""},
		{"unknown", []string{`"opaque"`}, []metadata.Revision{}, ""},
		{"opaque wildcard", []string{`"*"`}, []metadata.Revision{}, ""},
		{"empty opaque", []string{`""`}, []metadata.Revision{}, ""},
		{"wrong resource kind", []string{`"a1:` + id + `:1"`}, []metadata.Revision{}, ""},
		{"wrong resource id", []string{`"m1:00000000-0000-4000-8000-000000000002:1"`}, []metadata.Revision{}, ""},
		{"noncanonical revision", []string{`"m1:` + id + `:01"`}, []metadata.Revision{}, ""},
		{"zero revision", []string{`"m1:` + id + `:0"`}, []metadata.Revision{}, ""},
		{"negative revision", []string{`"m1:` + id + `:-1"`}, []metadata.Revision{}, ""},
		{"overflow revision", []string{`"m1:` + id + `:9223372036854775808"`}, []metadata.Revision{}, ""},
		{"opaque backslash", []string{`"a\b"`}, []metadata.Revision{}, ""},
		{"opaque obs text", []string{"\"\x80\""}, []metadata.Revision{}, ""},
		{"exact byte cap", []string{`"` + strings.Repeat("x", 1022) + `"`}, []metadata.Revision{}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			for _, value := range test.values {
				header.Add("If-Match", value)
			}
			got, problem := parseIfMatch(header, "m1", id)
			if test.code != "" {
				if problem == nil || problem.code != test.code {
					t.Fatalf("expected %s, got %#v, %#v", test.code, got, problem)
				}
				return
			}
			if problem != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %#v, %#v; want %#v", got, problem, test.want)
			}
		})
	}
	if entityTag("m1", id, 17) != `"m1:`+id+`:17"` {
		t.Fatal("tag format differs from the frozen wire contract")
	}
}

func FuzzIfMatch(f *testing.F) {
	for _, seed := range []string{"", "*", `""`, `"a,b"`, `"m1:00000000-0000-4000-8000-000000000001:1"`, `W/"x", "y"`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		header := make(http.Header)
		header.Set("If-Match", value)
		revisions, problem := parseIfMatch(header, "m1", "00000000-0000-4000-8000-000000000001")
		if problem == nil && len(revisions) > 8 {
			t.Fatal("accepted more than eight revisions")
		}
		for _, revision := range revisions {
			if revision <= 0 {
				t.Fatal("accepted nonpositive revision")
			}
		}
	})
}
