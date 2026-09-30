//go:build unix

package localindex

import (
	"errors"
	"os"
	"syscall"
)

func tryLock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errWouldBlock
		}
		return err
	}
}
