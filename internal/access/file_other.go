//go:build !unix

package access

import "os"

func openPolicyFile(path string) (*os.File, error) { return os.Open(path) }
