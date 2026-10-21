//go:build unix

package access

import (
	"os"
	"syscall"
)

func openPolicyFile(path string) (*os.File, error) {
	// A FIFO must not block startup before the opened-file regularity check.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
