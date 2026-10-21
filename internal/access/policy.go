// Package access owns immutable, startup-loaded dataset access policies.
package access

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
)

var (
	ErrUnauthenticated          = errors.New("authentication is required")
	ErrAuthenticationNotEnabled = errors.New("authentication is not enabled")
)

type level uint8

const (
	read level = iota + 1
	write
)

type Role struct{ level level }

func (r Role) String() string {
	switch r.level {
	case read:
		return "read"
	case write:
		return "write"
	default:
		return ""
	}
}

type Action struct{ level level }

func ReadAction() Action  { return Action{level: read} }
func WriteAction() Action { return Action{level: write} }

type Principal struct {
	key  string
	role Role
}

func (p Principal) SubjectKey() string { return p.key }
func (p Principal) Role() Role         { return p.role }

func (p Principal) Allows(action Action) bool {
	if p.key == "" {
		return false
	}
	switch action.level {
	case read:
		return p.role.level == read || p.role.level == write
	case write:
		return p.role.level == write
	default:
		return false
	}
}

type mode uint8

const (
	local mode = iota + 1
	tokens
)

type credential struct {
	digest    [sha256.Size]byte
	principal int
}

type Policy struct {
	mode        mode
	principals  []Principal
	credentials []credential
}

func LocalPolicy() *Policy {
	return &Policy{
		mode: local,
		principals: []Principal{{
			key: "local:workspace", role: Role{level: write},
		}},
	}
}

func (p *Policy) Valid() bool {
	return p != nil && (p.mode == local || p.mode == tokens)
}

func (p *Policy) Subjects() []Principal {
	if !p.Valid() {
		return nil
	}
	return append([]Principal(nil), p.principals...)
}

// Authenticate treats nil as an absent Authorization field. Any non-nil
// slice represents a supplied field, even if it contains no values.
func (p *Policy) Authenticate(authorization []string) (Principal, error) {
	if !p.Valid() {
		return Principal{}, ErrUnauthenticated
	}
	if p.mode == local {
		if authorization != nil {
			return Principal{}, ErrAuthenticationNotEnabled
		}
		return p.principals[0], nil
	}
	if len(authorization) != 1 {
		return Principal{}, ErrUnauthenticated
	}
	scheme, token, ok := strings.Cut(authorization[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return Principal{}, ErrUnauthenticated
	}
	token = strings.TrimLeft(token, " ")
	if !canonicalToken(token) {
		return Principal{}, ErrUnauthenticated
	}
	// Credentials hash the token's UTF-8 text, not the decoded random bytes.
	digest := sha256.Sum256([]byte(token))
	index := matchDigest(p.credentials, digest, subtle.ConstantTimeCompare)
	if index < 0 {
		return Principal{}, ErrUnauthenticated
	}
	return p.principals[index], nil
}

func canonicalToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	for _, c := range token {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
			c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(decoded) == 32
}

func matchDigest(credentials []credential, digest [sha256.Size]byte, equal func([]byte, []byte) int) int {
	selected := 0
	for _, candidate := range credentials {
		matched := equal(digest[:], candidate.digest[:])
		selected = subtle.ConstantTimeSelect(matched, candidate.principal+1, selected)
	}
	return selected - 1
}
