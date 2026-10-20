package metadata_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"metadata-api/internal/metadata"
)

func TestUUIDs(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id := metadata.NewUUID()
		if !metadata.ValidUUID(id) || seen[id] {
			t.Fatalf("invalid or repeated UUID %q", id)
		}
		seen[id] = true
		if _, err := metadata.ParseMaintainerID(id); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.ParseApplicationID(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []string{
		"", "not-an-id", "00000000-0000-0000-0000-000000000000",
		"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "00000000-0000-4000-7000-000000000000",
		"00000000-0000-4000-8000-00000000000g", " 00000000-0000-4000-8000-000000000000",
	} {
		if _, err := metadata.ParseMaintainerID(invalid); err == nil {
			t.Errorf("accepted maintainer ID %q", invalid)
		}
		if _, err := metadata.ParseApplicationID(invalid); err == nil {
			t.Errorf("accepted application ID %q", invalid)
		}
	}
}

func TestMaintainerValidation(t *testing.T) {
	record, err := metadata.NormalizeMaintainer(metadata.MaintainerFields{
		Name: "  River Example \n", Email: " River+Demo@EXAMPLE.TEST ",
	})
	if err != nil || record.Name != "River Example" || record.Email != "river+demo@example.test" {
		t.Fatalf("normalization = %#v, %v", record, err)
	}
	tests := []struct {
		name  string
		input metadata.MaintainerFields
		field string
	}{
		{"missing name", metadata.MaintainerFields{Email: "a@example.test"}, "name"},
		{"missing email", metadata.MaintainerFields{Name: "A"}, "email"},
		{"name bytes", metadata.MaintainerFields{Name: strings.Repeat("界", 43), Email: "a@example.test"}, "name"},
		{"email bytes", metadata.MaintainerFields{Name: "A", Email: strings.Repeat("a", 250) + "@example.test"}, "email"},
		{"display name", metadata.MaintainerFields{Name: "A", Email: "Person <a@example.test>"}, "email"},
		{"unicode email", metadata.MaintainerFields{Name: "A", Email: "é@example.test"}, "email"},
		{"unicode folding to ASCII", metadata.MaintainerFields{Name: "A", Email: "\u212a@example.test"}, "email"},
		{"newline email", metadata.MaintainerFields{Name: "A", Email: "a@\nexample.test"}, "email"},
		{"bad email", metadata.MaintainerFields{Name: "A", Email: "not-an-address"}, "email"},
		{"invalid UTF8", metadata.MaintainerFields{Name: string([]byte{0xff}), Email: "a@example.test"}, "name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := metadata.NormalizeMaintainer(test.input)
			assertValidation(t, err, test.field)
		})
	}
	if _, err := metadata.NormalizeMaintainer(metadata.MaintainerFields{
		Name: strings.Repeat("x", 128), Email: "a@example.test",
	}); err != nil {
		t.Fatalf("rejected exact name byte limit: %v", err)
	}
}

func TestApplicationValidation(t *testing.T) {
	base := metadata.ApplicationFields{Title: "Atlas", Version: "1.0", Company: "Example"}
	normalized, err := metadata.NormalizeApplication(metadata.ApplicationFields{
		Title: " Atlas ", Version: " 1.0 ", Company: " Example ",
		Website: " https://example.test/docs?q=1#part ", Source: "http://example.test/src",
		License: " MIT ", Description: " Demo ",
	})
	if err != nil || normalized.Title != "Atlas" || normalized.Version != "1.0" ||
		normalized.Company != "Example" || normalized.License != "MIT" || normalized.Description != "Demo" {
		t.Fatalf("normalization = %#v, %v", normalized, err)
	}
	tests := []struct {
		field  string
		change func(*metadata.ApplicationFields)
	}{
		{"title", func(f *metadata.ApplicationFields) { f.Title = " " }},
		{"version", func(f *metadata.ApplicationFields) { f.Version = "" }},
		{"company", func(f *metadata.ApplicationFields) { f.Company = "" }},
		{"title", func(f *metadata.ApplicationFields) { f.Title = strings.Repeat("x", 257) }},
		{"company", func(f *metadata.ApplicationFields) { f.Company = strings.Repeat("x", 257) }},
		{"version", func(f *metadata.ApplicationFields) { f.Version = strings.Repeat("x", 65) }},
		{"license", func(f *metadata.ApplicationFields) { f.License = strings.Repeat("x", 129) }},
		{"description", func(f *metadata.ApplicationFields) { f.Description = strings.Repeat("x", 16385) }},
		{"website", func(f *metadata.ApplicationFields) { f.Website = "https://example.test/" + strings.Repeat("x", 2048) }},
		{"source", func(f *metadata.ApplicationFields) { f.Source = "https://example.test/" + strings.Repeat("x", 2048) }},
		{"website", func(f *metadata.ApplicationFields) { f.Website = "/relative" }},
		{"website", func(f *metadata.ApplicationFields) { f.Website = "ftp://example.test/archive" }},
		{"website", func(f *metadata.ApplicationFields) { f.Website = "https://user:password@example.test/" }},
		{"source", func(f *metadata.ApplicationFields) { f.Source = "https:///missing-host" }},
		{"source", func(f *metadata.ApplicationFields) { f.Source = "https://:80/" }},
		{"source", func(f *metadata.ApplicationFields) { f.Source = "https://example.test/%zz" }},
		{"description", func(f *metadata.ApplicationFields) { f.Description = string([]byte{0xff}) }},
	}
	for i, test := range tests {
		t.Run(test.field+strings.Repeat("_", i), func(t *testing.T) {
			input := base
			test.change(&input)
			_, err := metadata.NormalizeApplication(input)
			assertValidation(t, err, test.field)
		})
	}
	exact := metadata.ApplicationFields{
		Title: strings.Repeat("x", 256), Version: strings.Repeat("x", 64), Company: strings.Repeat("x", 256),
		License: strings.Repeat("x", 128), Description: strings.Repeat("x", 16384),
		Website: "https://example.test/" + strings.Repeat("x", 2027),
		Source:  "http://example.test/" + strings.Repeat("x", 2028),
	}
	if _, err := metadata.NormalizeApplication(exact); err != nil {
		t.Fatalf("rejected exact byte limits: %v", err)
	}
	for _, creator := range []metadata.MaintainerID{"", " ", "invalid"} {
		_, err := metadata.NormalizeCreateApplication(metadata.CreateApplicationInput{
			ApplicationFields: base, MaintainerID: creator,
		})
		assertValidation(t, err, "maintainer_id")
	}
}

func TestRevisionConditions(t *testing.T) {
	if metadata.Matches(1, nil) || metadata.Matches(1, []metadata.Revision{0, 2}) {
		t.Fatal("empty or stale revisions matched")
	}
	if !metadata.Matches(1, []metadata.Revision{5, 1, 7}) {
		t.Fatal("matching member was not recognized")
	}
	if next, err := metadata.NextRevision(1); err != nil || next != 2 {
		t.Fatalf("next = %v, %v", next, err)
	}
	for _, revision := range []metadata.Revision{0, -1, math.MaxInt64} {
		if _, err := metadata.NextRevision(revision); err == nil {
			t.Fatalf("invalid revision %d advanced", revision)
		}
	}
	if metadata.PageLimit(0) != 20 || metadata.PageLimit(1000) != 100 || metadata.PageLimit(3) != 3 {
		t.Fatal("incorrect page limits")
	}
}

func assertValidation(t *testing.T, err error, field string) {
	t.Helper()
	var problem *metadata.Error
	if !errors.As(err, &problem) || problem.Code != metadata.ValidationFailed || problem.Fields[field] == "" {
		t.Fatalf("expected validation error for %s; got %v", field, err)
	}
}
