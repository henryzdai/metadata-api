package metadata

import (
	"context"
	"math"
)

type MaintainerList struct {
	Limit int
	After MaintainerID
}

type ApplicationList struct {
	Limit        int
	After        ApplicationID
	MaintainerID MaintainerID
}

// Page uses an exclusive ID cursor; an empty NextAfter marks the terminal page.
type Page[T any] struct {
	Items     []T    `json:"items"`
	NextAfter string `json:"next_after,omitempty"`
}

// MetadataStore owns both resource kinds so relationship checks and mutations stay atomic.
// Updates and deletes check existence and domain conflicts before expected revisions.
// Expected revisions are an any-of condition; an empty set never authorizes a write.
type MetadataStore interface {
	CreateMaintainer(context.Context, MaintainerFields) (Maintainer, error)
	GetMaintainer(context.Context, MaintainerID) (Maintainer, error)
	ListMaintainers(context.Context, MaintainerList) (Page[Maintainer], error)
	UpdateMaintainer(context.Context, MaintainerID, MaintainerFields, []Revision) (Maintainer, error)
	DeleteMaintainer(context.Context, MaintainerID, []Revision) error

	CreateApplication(context.Context, CreateApplicationInput) (Application, error)
	GetApplication(context.Context, ApplicationID) (Application, error)
	ListApplications(context.Context, ApplicationList) (Page[Application], error)
	UpdateApplication(context.Context, ApplicationID, ApplicationFields, []Revision) (Application, error)
	DeleteApplication(context.Context, ApplicationID, []Revision) error

	Ping(context.Context) error
	Close() error
}

func Matches(revision Revision, expected []Revision) bool {
	for _, candidate := range expected {
		if candidate == revision {
			return true
		}
	}
	return false
}

func NextRevision(revision Revision) (Revision, error) {
	if revision < 1 || revision == Revision(math.MaxInt64) {
		return 0, NewError(InternalError, nil)
	}
	return revision + 1, nil
}

func PageLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return limit
}
