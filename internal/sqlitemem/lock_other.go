//go:build !(linux && (amd64 || arm64 || loong64 || ppc64le || s390x || riscv64 || 386 || arm))

package sqlitemem

import (
	"sync"
	_ "unsafe" // go:linkname
)

//go:linkname allocMu modernc.org/libc.allocMu
var allocMu sync.Mutex

func lock()   { allocMu.Lock() }
func unlock() { allocMu.Unlock() }
