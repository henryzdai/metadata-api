package access

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"metadata-api/internal/metadata"
)

type principalFile struct {
	ID     string   `json:"id"`
	Role   string   `json:"role"`
	Hashes []string `json:"token_sha256"`
}

func randomToken(t *testing.T) (string, string) {
	t.Helper()
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		t.Fatal("generate test credential")
	}
	token := base64.RawURLEncoding.EncodeToString(data[:])
	digest := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(digest[:])
}

func policyJSON(t *testing.T, principals ...principalFile) []byte {
	t.Helper()
	data, err := json.Marshal(struct {
		Principals []principalFile `json:"principals"`
	}{Principals: principals})
	if err != nil {
		t.Fatal("encode test policy")
	}
	return data
}

func parsedPolicy(t *testing.T, data []byte) *Policy {
	t.Helper()
	policy, err := parseTokenPolicy(data)
	if err != nil {
		t.Fatal("valid test policy was rejected")
	}
	return policy
}

func TestLocalAndInvalidPolicies(t *testing.T) {
	for _, policy := range []*Policy{nil, {}} {
		if policy.Valid() || policy.Subjects() != nil {
			t.Fatal("uninitialized policy is valid")
		}
		if _, err := policy.Authenticate(nil); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal("uninitialized policy permits access")
		}
	}
	policy := LocalPolicy()
	principal, err := policy.Authenticate(nil)
	if err != nil || !policy.Valid() || principal.SubjectKey() != "local:workspace" ||
		principal.Role().String() != "write" || !principal.Allows(ReadAction()) || !principal.Allows(WriteAction()) {
		t.Fatal("explicit local workflow did not receive dataset access")
	}
	for _, values := range [][]string{{}, {""}, {"Bearer unchecked"}, {"first", "second"}} {
		if _, err := policy.Authenticate(values); !errors.Is(err, ErrAuthenticationNotEnabled) {
			t.Fatal("local mode silently accepted supplied credentials")
		}
	}
	if principal.Allows(Action{}) || (Principal{}).Allows(ReadAction()) || (Role{}).String() != "" {
		t.Fatal("zero domain values grant permissions")
	}
	subjects := policy.Subjects()
	subjects[0] = Principal{}
	if policy.Subjects()[0].SubjectKey() != "local:workspace" {
		t.Fatal("subject enumeration exposes local policy state")
	}
}

func TestTokensRotationRevocationAndCopiedSubjects(t *testing.T) {
	oldToken, oldHash := randomToken(t)
	newToken, newHash := randomToken(t)
	readToken, readHash := randomToken(t)
	writerID, readerID := metadata.NewUUID(), metadata.NewUUID()
	data := policyJSON(t,
		principalFile{writerID, "write", []string{oldHash, newHash}},
		principalFile{readerID, "read", []string{readHash}},
	)
	policy := parsedPolicy(t, data)
	clear(data)
	var writer Principal
	for _, token := range []string{oldToken, newToken} {
		principal, err := policy.Authenticate([]string{"bEaReR " + token})
		if err != nil || principal.SubjectKey() != "token:"+writerID ||
			principal.Role().String() != "write" || !principal.Allows(ReadAction()) || !principal.Allows(WriteAction()) {
			t.Fatal("rotation credentials did not resolve to the stable writer")
		}
		if writer.SubjectKey() != "" && principal != writer {
			t.Fatal("rotation changed principal identity")
		}
		writer = principal
	}
	reader, err := policy.Authenticate([]string{"Bearer " + readToken})
	if err != nil || reader.SubjectKey() != "token:"+readerID || reader.Role().String() != "read" ||
		!reader.Allows(ReadAction()) || reader.Allows(WriteAction()) {
		t.Fatal("reader permissions are not dataset-wide reads only")
	}
	subjects := policy.Subjects()
	if len(subjects) != 2 || subjects[0] != writer || subjects[1] != reader {
		t.Fatal("subject metadata lost a validated principal")
	}
	subjects[0] = reader
	subjects = append(subjects, Principal{})
	if got := policy.Subjects(); len(got) != 2 || got[0] != writer {
		t.Fatal("subject metadata is not copied")
	}
	downgraded := parsedPolicy(t, policyJSON(t, principalFile{writerID, "read", []string{newHash}}))
	for _, token := range []string{oldToken, readToken} {
		if _, err := downgraded.Authenticate([]string{"Bearer " + token}); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal("new snapshot retained a removed credential")
		}
	}
	nowReader, err := downgraded.Authenticate([]string{"Bearer " + newToken})
	if err != nil || nowReader.SubjectKey() != writer.SubjectKey() || nowReader.Allows(WriteAction()) {
		t.Fatal("role downgrade changed identity or retained write access")
	}
	if _, err := policy.Authenticate([]string{"Bearer " + oldToken}); err != nil {
		t.Fatal("creating a new snapshot mutated an existing immutable policy")
	}
}

func TestTokenShapeSchemeAndTextHash(t *testing.T) {
	token, hash := randomToken(t)
	policy := parsedPolicy(t, policyJSON(t, principalFile{metadata.NewUUID(), "write", []string{hash}}))
	for _, value := range []string{"Bearer " + token, "bearer " + token, "BEARER   " + token} {
		if _, err := policy.Authenticate([]string{value}); err != nil {
			t.Fatal("valid Bearer credential was rejected")
		}
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	noncanonical := token[:42] + string(alphabet[strings.IndexByte(alphabet, token[42])|1])
	unknown, _ := randomToken(t)
	invalid := []string{
		"", token, "Basic " + token, "Bearer", " Bearer " + token, "Bearer\t" + token,
		"Bearer " + token + " ", "Bearer " + token + ",", "Bearer " + token + "=",
		"Bearer " + token + "\n", "Bearer " + token[:42], "Bearer " + token + "A",
		"Bearer " + noncanonical, "Bearer " + unknown, "Bearer " + strings.Repeat("/", 43),
		"Bearer " + strings.Repeat("+", 43), "Bearer " + strings.Repeat("é", 43),
		"Bearer " + token + ", Bearer " + token,
	}
	for i, value := range invalid {
		if _, err := policy.Authenticate([]string{value}); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("malformed credential case %d was accepted", i)
		}
	}
	for _, values := range [][]string{nil, {}, {""}, {"Bearer " + token, "Bearer " + token}} {
		if _, err := policy.Authenticate(values); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal("absent, empty, or repeated credentials were accepted")
		}
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal("decode generated test token")
	}
	wrongDigest := sha256.Sum256(decoded)
	wrong := parsedPolicy(t, policyJSON(t,
		principalFile{metadata.NewUUID(), "write", []string{hex.EncodeToString(wrongDigest[:])}},
	))
	if _, err := wrong.Authenticate([]string{"Bearer " + token}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("authentication hashed decoded bytes instead of token text")
	}
}

func TestDigestMatchingAlwaysScansEveryConfiguredDigest(t *testing.T) {
	credentials := make([]credential, MaxDigests)
	for i := range credentials {
		credentials[i] = credential{digest: sha256.Sum256([]byte{byte(i)}), principal: i / 2}
	}
	for _, position := range []int{0, 1, MaxDigests / 2, MaxDigests - 1, -1} {
		digest := sha256.Sum256([]byte("not a configured digest"))
		want := -1
		if position >= 0 {
			digest = credentials[position].digest
			want = credentials[position].principal
		}
		comparisons := 0
		got := matchDigest(credentials, digest, func(actual, configured []byte) int {
			if comparisons >= len(credentials) || !bytes.Equal(configured, credentials[comparisons].digest[:]) ||
				len(actual) != sha256.Size || len(configured) != sha256.Size {
				t.Fatal("digest scan skipped, repeated, or shortened a comparison")
			}
			comparisons++
			return subtle.ConstantTimeCompare(actual, configured)
		})
		if got != want || comparisons != MaxDigests {
			t.Fatalf("digest scan at position %d selected %d with %d comparisons", position, got, comparisons)
		}
	}
}
