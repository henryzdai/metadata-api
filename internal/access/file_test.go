package access

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"metadata-api/internal/metadata"
	"metadata-api/internal/testsupport"
)

func TestStrictTokenFileRejectsAmbiguousAndMalformedDocuments(t *testing.T) {
	_, hash := randomToken(t)
	const id = "11111111-2222-4333-8444-55555555555a"
	entry := principalFile{id, "write", []string{hash}}
	good := string(policyJSON(t, entry))
	var root struct {
		Principals []json.RawMessage `json:"principals"`
	}
	if err := json.Unmarshal([]byte(good), &root); err != nil {
		t.Fatal("decode test policy")
	}
	principal := string(root.Principals[0])
	tests := []struct{ name, data string }{
		{"empty", ""},
		{"root null", "null"},
		{"root array", "[]"},
		{"missing principals", "{}"},
		{"null principals", `{"principals":null}`},
		{"object principals", `{"principals":{}}`},
		{"empty principals", `{"principals":[]}`},
		{"null principal", `{"principals":[null]}`},
		{"string principal", `{"principals":["unchecked"]}`},
		{"unknown root member", strings.TrimSuffix(good, "}") + `,"private-value":"unchecked"}`},
		{"case root member", strings.Replace(good, `"principals"`, `"Principals"`, 1)},
		{"duplicate principals", strings.TrimSuffix(good, "}") + `,"principals":[]}`},
		{"escaped duplicate principals", strings.TrimSuffix(good, "}") + `,"princi\u0070als":[]}`},
		{"unknown principal member", strings.Replace(good, `"role":"write"`, `"role":"write","private-value":"unchecked"`, 1)},
		{"case id", strings.Replace(good, `"id"`, `"ID"`, 1)},
		{"case role", strings.Replace(good, `"role"`, `"Role"`, 1)},
		{"case hashes", strings.Replace(good, `"token_sha256"`, `"Token_sha256"`, 1)},
		{"duplicate id", strings.Replace(good, `"id":`, `"id":"`+id+`","id":`, 1)},
		{"duplicate role", strings.Replace(good, `"role":`, `"role":"read","role":`, 1)},
		{"escaped duplicate role", strings.Replace(good, `"role":`, `"rol\u0065":"read","role":`, 1)},
		{"duplicate hashes", strings.Replace(good, `"token_sha256":`, `"token_sha256":[],"token_sha256":`, 1)},
		{"missing id", strings.Replace(good, `"id":"`+id+`",`, "", 1)},
		{"missing role", strings.Replace(good, `"role":"write",`, "", 1)},
		{"missing hashes", `{"principals":[{"id":"` + id + `","role":"write"}]}`},
		{"null id", strings.Replace(good, `"`+id+`"`, "null", 1)},
		{"number id", strings.Replace(good, `"`+id+`"`, "1", 1)},
		{"null role", strings.Replace(good, `"write"`, "null", 1)},
		{"object role", strings.Replace(good, `"write"`, "{}", 1)},
		{"unknown role", strings.Replace(good, `"write"`, `"admin"`, 1)},
		{"case role value", strings.Replace(good, `"write"`, `"Write"`, 1)},
		{"null hashes", strings.Replace(good, `["`+hash+`"]`, "null", 1)},
		{"empty hashes", strings.Replace(good, `["`+hash+`"]`, "[]", 1)},
		{"string hashes", strings.Replace(good, `["`+hash+`"]`, `"`+hash+`"`, 1)},
		{"null digest", strings.Replace(good, `"`+hash+`"`, "null", 1)},
		{"number digest", strings.Replace(good, `"`+hash+`"`, "1", 1)},
		{"array digest", strings.Replace(good, `"`+hash+`"`, "[]", 1)},
		{"duplicate digest same subject", strings.Replace(good, `["`+hash+`"]`, `["`+hash+`","`+hash+`"]`, 1)},
		{"too many digests", strings.Replace(good, `["`+hash+`"]`, `["`+hash+`","`+hash+`","`+hash+`"]`, 1)},
		{"duplicate principal", `{"principals":[` + principal + `,` + principal + `]}`},
		{"trailing document", good + "{}"},
		{"trailing null", good + "null"},
		{"trailing garbage", good + "unchecked"},
		{"unterminated root", strings.TrimSuffix(good, "}")},
		{"invalid UTF8", good + "\xff"},
		{"BOM", "\ufeff" + good},
	}
	for _, badID := range []string{
		"", strings.ToUpper(id), strings.Replace(id, "-", "", 1), " " + id,
		id[:14] + "1" + id[15:], id[:19] + "7" + id[20:], id[:35] + "g",
	} {
		tests = append(tests, struct{ name, data string }{
			"malformed id", strings.Replace(good, `"`+id+`"`, `"`+badID+`"`, 1),
		})
	}
	for _, badHash := range []string{"", hash[:63], hash + "0", strings.Repeat("a", 63) + "g",
		strings.Repeat("A", 64), " " + hash, strings.Repeat("x", 64)} {
		tests = append(tests, struct{ name, data string }{
			"malformed digest", strings.Replace(good, `"`+hash+`"`, `"`+badHash+`"`, 1),
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, err := parseTokenPolicy([]byte(test.data))
			if policy != nil || !errors.Is(err, errInvalidFile) || err.Error() != "invalid authentication file" {
				t.Fatal("invalid credential document was accepted or exposed unchecked details")
			}
		})
	}
}

func TestCredentialFileBoundsAndGlobalUniqueness(t *testing.T) {
	principals := make([]principalFile, MaxPrincipals)
	for i := range principals {
		_, first := randomToken(t)
		_, second := randomToken(t)
		principals[i] = principalFile{metadata.NewUUID(), "read", []string{first, second}}
	}
	data := policyJSON(t, principals...)
	policy := parsedPolicy(t, data)
	if len(policy.Subjects()) != MaxPrincipals || len(policy.credentials) != MaxDigests {
		t.Fatal("valid finite credential ceilings were rejected")
	}
	_, extra := randomToken(t)
	over := append(append([]principalFile(nil), principals...), principalFile{metadata.NewUUID(), "write", []string{extra}})
	if _, err := parseTokenPolicy(policyJSON(t, over...)); err == nil {
		t.Fatal("excess principals were accepted")
	}
	principals[1].ID = principals[0].ID
	if _, err := parseTokenPolicy(policyJSON(t, principals...)); err == nil {
		t.Fatal("duplicate identity with different credentials was accepted")
	}
	principals[1].ID = metadata.NewUUID()
	principals[1].Hashes = []string{principals[0].Hashes[0]}
	if _, err := parseTokenPolicy(policyJSON(t, principals...)); err == nil {
		t.Fatal("a digest was assigned to two identities")
	}
	exact := append(bytes.Clone(data), bytes.Repeat([]byte(" "), MaxFileBytes-len(data))...)
	parsedPolicy(t, exact)
	if _, err := parseTokenPolicy(append(exact, ' ')); err == nil {
		t.Fatal("oversized credential input was accepted")
	}
}

func TestLoadTokenPolicyInspectsBoundsAndKeepsOnlyASnapshot(t *testing.T) {
	directory := testsupport.Workspace(t)
	path := filepath.Join(directory, "private-auth.json")
	token, hash := randomToken(t)
	data := policyJSON(t, principalFile{metadata.NewUUID(), "write", []string{hash}})
	exact := append(bytes.Clone(data), bytes.Repeat([]byte(" "), MaxFileBytes-len(data))...)
	if err := os.WriteFile(path, exact, 0o600); err != nil {
		t.Fatal("write test credential file")
	}
	policy, err := LoadTokenPolicy(path)
	if err != nil {
		t.Fatal("valid private regular credential file was rejected")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal("loader left its file open")
	}
	if _, err := policy.Authenticate([]string{"Bearer " + token}); err != nil {
		t.Fatal("loaded policy depends on the file after startup")
	}
	if err := os.WriteFile(path, append(exact, ' '), 0o600); err != nil {
		t.Fatal("write oversized test file")
	}
	for _, rejected := range []string{path, directory, filepath.Join(directory, "private-missing-file")} {
		if got, err := LoadTokenPolicy(rejected); err == nil || got != nil ||
			strings.Contains(err.Error(), directory) || strings.Contains(err.Error(), hash) {
			t.Fatal("invalid credential file was accepted or disclosed a path or hash")
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, mode := range []os.FileMode{0o620, 0o602, 0o666} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal("write permission test file")
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal("set test file permissions")
		}
		if _, err := LoadTokenPolicy(path); err == nil {
			t.Fatal("group- or world-writable credential file was accepted")
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal("restore test file permissions")
	}
	if _, err := LoadTokenPolicy(path); err != nil {
		t.Fatal("safe POSIX credential permissions were rejected")
	}
}
