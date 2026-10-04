package local

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// diskCase is the path of the directory or file at p as the file system
// spells it (fcntl F_GETPATH): on a case-insensitive volume, /p/App for
// /p/app. One open and one fcntl, no directory reads. p unchanged when
// it cannot be opened.
func diskCase(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return p
	}
	defer f.Close()
	buf := make([]byte, unix.PathMax)
	_, err = unix.FcntlInt(f.Fd(), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0]))))
	runtime.KeepAlive(buf)
	if err != nil {
		return p
	}
	for i, c := range buf {
		if c == 0 {
			if i == 0 {
				return p
			}
			return string(buf[:i])
		}
	}
	return p
}
