package metadata

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestPageBudgetIncludesEscapingEnvelopeAndOneMaximumRecord(t *testing.T) {
	ownerFields, err := NormalizeMaintainer(MaintainerFields{
		Name: strings.Repeat("<", 128), Email: strings.Repeat("a", 64) + "@" + strings.Repeat("b", 185) + ".com",
	})
	if err != nil {
		t.Fatal(err)
	}
	fields, err := NormalizeApplication(ApplicationFields{
		Title: strings.Repeat("<", 256), Version: strings.Repeat(`"`, 64), Company: strings.Repeat("&", 256),
		Website: "https://example.test/" + strings.Repeat("x", 2027),
		Source:  "https://example.test/" + strings.Repeat("y", 2027),
		License: strings.Repeat("\x00", 128), Description: strings.Repeat("\x00", 16384),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 1, 2, 3, 999999999, time.UTC)
	owner := Maintainer{
		ID: MaintainerID(NewUUID()), MaintainerFields: ownerFields,
		Revision: math.MaxInt64, CreatedAt: now, UpdatedAt: now,
	}
	app := Application{
		ID: ApplicationID(NewUUID()), ApplicationFields: fields, MaintainerID: owner.ID,
		Revision: math.MaxInt64, CreatedAt: now, UpdatedAt: now,
	}
	for _, test := range []struct {
		name  string
		value any
		page  any
		bound int64
	}{
		{"maintainer", owner, Page[Maintainer]{Items: []Maintainer{owner}, NextAfter: string(owner.ID)}, MaintainerEncodedBound(owner)},
		{"application", app, Page[Application]{Items: []Application{app}, NextAfter: string(app.ID)}, ApplicationEncodedBound(app)},
	} {
		t.Run(test.name, func(t *testing.T) {
			item, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			page, err := json.Marshal(test.page)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(item)) > test.bound || int64(len(page)+1) > PageEnvelopeBytes+test.bound ||
				test.bound >= 128<<10 || !PageItemFits(PageEnvelopeBytes, test.bound) {
				t.Fatalf("unsafe bound; item %d, envelope %d, estimate %d", len(item), len(page)+1, test.bound)
			}
		})
	}
	for _, text := range []string{"<>&", "\x00\x01\x1f", "\"\\\n", "\u2028\u2029", "名é"} {
		app.Description = strings.Repeat(text, 100)
		data, err := json.Marshal(app)
		if err != nil || int64(len(data)) > ApplicationEncodedBound(app) {
			t.Fatalf("escaping %q exceeds bound: %d, %v", text, len(data), err)
		}
	}
}

func TestPageBudgetArithmetic(t *testing.T) {
	for _, test := range []struct {
		used, item int64
		want       bool
	}{
		{PageEnvelopeBytes, PageByteLimit - PageEnvelopeBytes, true},
		{PageEnvelopeBytes, PageByteLimit - PageEnvelopeBytes + 1, false},
		{math.MaxInt64, 1, false}, {PageEnvelopeBytes, math.MaxInt64, false},
		{-1, 1, false}, {PageEnvelopeBytes, -1, false}, {0, 0, false},
	} {
		if got := PageItemFits(test.used, test.item); got != test.want {
			t.Fatalf("PageItemFits(%d, %d) = %t, want %t", test.used, test.item, got, test.want)
		}
	}
}
