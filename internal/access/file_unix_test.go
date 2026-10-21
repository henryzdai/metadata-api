//go:build unix

package access

import (
	"path/filepath"
	"syscall"
	"testing"

	"metadata-api/internal/testsupport"
)

func TestCredentialFIFODoesNotBlockBeforeRegularFileInspection(t *testing.T) {
	path := filepath.Join(testsupport.Workspace(t), "not-regular")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal("create credential file-type test")
	}
	if policy, err := LoadTokenPolicy(path); err == nil || policy != nil {
		t.Fatal("a credential FIFO was accepted")
	}
}
