package metadata

import (
	"errors"
	"math"
	"testing"
)

func TestStorePolicyRequiresFinitePositiveLimitsAndOwnsValues(t *testing.T) {
	defaults := DefaultStorePolicy().Limits()
	if defaults != (StoreLimits{MaxMaintainers: 10_000, MaxApplications: 10_000, MaxFieldBytes: 67_108_864}) {
		t.Fatalf("defaults = %#v", defaults)
	}
	for _, limits := range []StoreLimits{
		{}, {1, 1, 0}, {0, 1, 1}, {1, 0, 1}, {-1, 1, 1}, {1, -1, 1}, {1, 1, -1},
		{1_000_001, 1, 1}, {1, 1_000_001, 1}, {1, 1, 1<<30 + 1}, {1, 1, math.MaxInt64},
	} {
		if policy, err := NewStorePolicy(limits); err == nil || policy != (StorePolicy{}) {
			t.Fatalf("accepted invalid limits %#v", limits)
		}
	}
	if err := (StorePolicy{}).Validate(); err == nil {
		t.Fatal("zero policy is valid")
	}
	for _, limits := range []StoreLimits{{1, 1, 1}, {1_000_000, 1_000_000, 1 << 30}} {
		policy, err := NewStorePolicy(limits)
		if err != nil {
			t.Fatal(err)
		}
		copy := policy.Limits()
		copy.MaxMaintainers = 0
		limits.MaxApplications = 0
		if policy.Limits().MaxMaintainers == 0 || policy.Limits().MaxApplications == 0 {
			t.Fatal("policy shares mutable caller limits")
		}
	}
}

func TestFieldByteArithmeticRejectsOverflowAndOnlyChargesGrowth(t *testing.T) {
	policy, err := NewStorePolicy(StoreLimits{1, 1, 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                    string
		current, removed, added int64
		want                    int64
		code                    ErrorCode
	}{
		{"last byte", 9, 0, 1, 10, ""},
		{"capacity", 10, 0, 1, 0, ResourceCapacityExceeded},
		{"replace equal", 10, 4, 4, 10, ""},
		{"shrink", 10, 4, 3, 9, ""},
		{"delete", 10, 10, 0, 0, ""},
		{"over-limit shrink", 11, 2, 1, 10, ""},
		{"over-limit same size", 11, 1, 1, 11, ""},
		{"overflow", math.MaxInt64, 0, 1, 0, InternalError},
		{"overflow after subtraction", math.MaxInt64, 1, 2, 0, InternalError},
		{"negative current", -1, 0, 0, 0, InternalError},
		{"negative removal", 1, -1, 0, 0, InternalError},
		{"negative addition", 1, 0, -1, 0, InternalError},
		{"underflow", 1, 2, 0, 0, InternalError},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := policy.FieldBytesAfter(test.current, test.removed, test.added)
			if test.code == "" {
				if err != nil || got != test.want {
					t.Fatalf("result = %d, %v; want %d", got, err, test.want)
				}
			} else {
				var domain *Error
				if !errors.As(err, &domain) || domain.Code != test.code {
					t.Fatalf("error = %v; want %s", err, test.code)
				}
			}
		})
	}
}

func TestLogicalFieldBytesUseNormalizedUTF8AndCreator(t *testing.T) {
	owner, err := NormalizeMaintainer(MaintainerFields{Name: "  名  ", Email: " OWNER@EXAMPLE.TEST "})
	if err != nil {
		t.Fatal(err)
	}
	if got := MaintainerFieldBytes(owner); got != int64(len("名owner@example.test")) {
		t.Fatalf("maintainer weight = %d", got)
	}
	input, err := NormalizeCreateApplication(CreateApplicationInput{
		ApplicationFields: ApplicationFields{
			Title: " 标题 ", Version: " 1 ", Company: " 公司 ", Website: " https://example.test ",
			Source: " https://example.test/src ", License: " MIT ", Description: " \x00é ",
		},
		MaintainerID: MaintainerID(" " + NewUUID() + " "),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := len("标题1公司https://example.testhttps://example.test/srcMIT\x00é") + 36
	if got := ApplicationFieldBytes(input.ApplicationFields, input.MaintainerID); got != int64(want) {
		t.Fatalf("application weight = %d, want %d", got, want)
	}
}
