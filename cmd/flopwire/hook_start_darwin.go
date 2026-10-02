package main

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// processStart is when this process was created (its fork, before the
// exec of this binary), from the kernel's process table; the zero time
// when it cannot be read.
func processStart() time.Time {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", os.Getpid())
	if err != nil || kp.Proc.P_starttime.Sec == 0 {
		return time.Time{}
	}
	return time.Unix(kp.Proc.P_starttime.Sec, int64(kp.Proc.P_starttime.Usec)*1000)
}
