//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package desktopcode

import "os"

func openMetadata(r *os.Root, p string) (*os.File, error) { return r.Open(p) }
