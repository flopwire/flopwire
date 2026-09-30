//go:build darwin

package transcript

import (
	"os"
	"syscall"
)

func statIdentity(fi os.FileInfo) (FileID, int64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileID{}, 0, false
	}
	return FileID{Dev: uint64(st.Dev), Ino: st.Ino}, st.Ctimespec.Nano(), true
}
