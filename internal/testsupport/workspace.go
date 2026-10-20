package testsupport

import (
	"os"
	"path/filepath"
	"testing"

	"metadata-api/internal/metadata"
)

func Workspace(t testing.TB) string {
	t.Helper()
	path := filepath.Join(".test-work", metadata.NewUUID())
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Errorf("remove test workspace: %v", err)
		}
		_ = os.Remove(".test-work")
	})
	return path
}
