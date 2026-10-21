package access

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"unicode/utf8"

	"metadata-api/internal/metadata"
)

const (
	MaxFileBytes  = 256 * 1024
	MaxPrincipals = 128
	MaxDigests    = 256
)

var errInvalidFile = errors.New("invalid authentication file")

func LoadTokenPolicy(path string) (policy *Policy, loadErr error) {
	file, err := openPolicyFile(path)
	if err != nil {
		return nil, errors.New("cannot open authentication file")
	}
	defer func() {
		if err := file.Close(); err != nil && loadErr == nil {
			policy = nil
			loadErr = errors.New("cannot close authentication file")
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("cannot inspect authentication file")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("authentication file must be a regular file")
	}
	// Portable mode bits do not establish the safety of a Windows ACL.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("authentication file must not be group- or world-writable")
	}
	if info.Size() > MaxFileBytes {
		return nil, errors.New("authentication file exceeds 262144 bytes")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, errors.New("cannot read authentication file")
	}
	return parseTokenPolicy(data)
}

func parseTokenPolicy(data []byte) (*Policy, error) {
	if len(data) > MaxFileBytes || !utf8.Valid(data) {
		return nil, errInvalidFile
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if !consumeDelimiter(decoder, '{') {
		return nil, errInvalidFile
	}
	policy := &Policy{mode: tokens}
	seenPrincipals := false
	identities := make(map[string]bool)
	hashes := make(map[[sha256.Size]byte]bool)
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil || name != "principals" || seenPrincipals || !consumeDelimiter(decoder, '[') {
			return nil, errInvalidFile
		}
		seenPrincipals = true
		for decoder.More() {
			if len(policy.principals) == MaxPrincipals {
				return nil, errInvalidFile
			}
			principal, digests, err := readPrincipal(decoder)
			if err != nil || identities[principal.key] {
				return nil, errInvalidFile
			}
			identities[principal.key] = true
			for _, digest := range digests {
				if hashes[digest] || len(policy.credentials) == MaxDigests {
					return nil, errInvalidFile
				}
				hashes[digest] = true
				policy.credentials = append(policy.credentials, credential{
					digest: digest, principal: len(policy.principals),
				})
			}
			policy.principals = append(policy.principals, principal)
		}
		if !consumeDelimiter(decoder, ']') {
			return nil, errInvalidFile
		}
	}
	if !seenPrincipals || len(policy.principals) == 0 || !consumeDelimiter(decoder, '}') {
		return nil, errInvalidFile
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errInvalidFile
	}
	return policy, nil
}

func readPrincipal(decoder *json.Decoder) (Principal, [][sha256.Size]byte, error) {
	if !consumeDelimiter(decoder, '{') {
		return Principal{}, nil, errInvalidFile
	}
	var id string
	var role Role
	var digests [][sha256.Size]byte
	seen := make(map[string]bool, 3)
	for decoder.More() {
		member, err := decoder.Token()
		name, ok := member.(string)
		if err != nil || !ok || seen[name] {
			return Principal{}, nil, errInvalidFile
		}
		seen[name] = true
		switch name {
		case "id":
			id, ok = readString(decoder)
			if !ok || !metadata.ValidUUID(id) {
				return Principal{}, nil, errInvalidFile
			}
		case "role":
			value, valid := readString(decoder)
			if !valid {
				return Principal{}, nil, errInvalidFile
			}
			switch value {
			case "read":
				role = Role{level: read}
			case "write":
				role = Role{level: write}
			default:
				return Principal{}, nil, errInvalidFile
			}
		case "token_sha256":
			digests, err = readDigests(decoder)
			if err != nil {
				return Principal{}, nil, errInvalidFile
			}
		default:
			return Principal{}, nil, errInvalidFile
		}
	}
	if len(seen) != 3 || !consumeDelimiter(decoder, '}') {
		return Principal{}, nil, errInvalidFile
	}
	return Principal{key: "token:" + id, role: role}, digests, nil
}

func readDigests(decoder *json.Decoder) ([][sha256.Size]byte, error) {
	if !consumeDelimiter(decoder, '[') {
		return nil, errInvalidFile
	}
	var digests [][sha256.Size]byte
	for decoder.More() {
		if len(digests) == 2 {
			return nil, errInvalidFile
		}
		value, ok := readString(decoder)
		if !ok || len(value) != sha256.Size*2 || strings.Trim(value, "0123456789abcdef") != "" {
			return nil, errInvalidFile
		}
		var digest [sha256.Size]byte
		if _, err := hex.Decode(digest[:], []byte(value)); err != nil {
			return nil, errInvalidFile
		}
		digests = append(digests, digest)
	}
	if len(digests) == 0 || !consumeDelimiter(decoder, ']') {
		return nil, errInvalidFile
	}
	return digests, nil
}

func readString(decoder *json.Decoder) (string, bool) {
	token, err := decoder.Token()
	value, ok := token.(string)
	return value, err == nil && ok
}

func consumeDelimiter(decoder *json.Decoder, delimiter json.Delim) bool {
	token, err := decoder.Token()
	return err == nil && token == delimiter
}
