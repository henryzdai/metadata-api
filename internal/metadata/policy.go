package metadata

import (
	"errors"
	"math"
)

type StoreLimits struct {
	MaxMaintainers  int
	MaxApplications int
	MaxFieldBytes   int64
}

type StorePolicy struct {
	limits StoreLimits
}

func DefaultStorePolicy() StorePolicy {
	return StorePolicy{limits: StoreLimits{
		MaxMaintainers: 10_000, MaxApplications: 10_000, MaxFieldBytes: 64 << 20,
	}}
}

func NewStorePolicy(limits StoreLimits) (StorePolicy, error) {
	policy := StorePolicy{limits: limits}
	if err := policy.Validate(); err != nil {
		return StorePolicy{}, err
	}
	return policy, nil
}

func (p StorePolicy) Limits() StoreLimits { return p.limits }

func (p StorePolicy) Validate() error {
	if p.limits.MaxMaintainers < 1 || p.limits.MaxMaintainers > 1_000_000 {
		return errors.New("max-maintainers must be between 1 and 1000000")
	}
	if p.limits.MaxApplications < 1 || p.limits.MaxApplications > 1_000_000 {
		return errors.New("max-applications must be between 1 and 1000000")
	}
	if p.limits.MaxFieldBytes < 1 || p.limits.MaxFieldBytes > 1<<30 {
		return errors.New("max-metadata-bytes must be between 1 and 1073741824")
	}
	return nil
}

// FieldBytesAfter charges normalized UTF-8 user fields, not allocator or disk usage.
// Non-growing changes remain possible even when current usage exceeds the policy.
func (p StorePolicy) FieldBytesAfter(current, removed, added int64) (int64, error) {
	if current < 0 || removed < 0 || added < 0 || removed > current {
		return 0, NewError(InternalError, nil)
	}
	remainder := current - removed
	if added > math.MaxInt64-remainder {
		return 0, NewError(InternalError, nil)
	}
	next := remainder + added
	if next > current && next > p.limits.MaxFieldBytes {
		return 0, NewError(ResourceCapacityExceeded, nil)
	}
	return next, nil
}

func MaintainerFieldBytes(fields MaintainerFields) int64 {
	return int64(len(fields.Name)) + int64(len(fields.Email))
}

func ApplicationFieldBytes(fields ApplicationFields, creator MaintainerID) int64 {
	return int64(len(fields.Title)) + int64(len(fields.Version)) + int64(len(fields.Company)) +
		int64(len(fields.Website)) + int64(len(fields.Source)) + int64(len(fields.License)) +
		int64(len(fields.Description)) + int64(len(creator))
}
