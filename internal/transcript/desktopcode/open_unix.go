//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package desktopcode

import (
	"os"
	"syscall"
)

// Nonblocking open avoids a substituted FIFO blocking the metadata scan.
func openMetadata(r *os.Root, p string) (*os.File, error) {
	return r.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
