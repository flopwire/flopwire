package main

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// processStart is when this process was created (its fork, before the
// exec of this binary): /proc/self/stat's starttime, in clock ticks after
// boot (USER_HZ, 100 on every Linux ABI), against CLOCK_BOOTTIME, the
// clock starttime counts on. (Not /proc/stat's btime: it is whole seconds,
// which would add up to 1 s to the hook's age.) The zero time when either
// cannot be read.
func processStart() time.Time {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return time.Time{}
	}
	since, ok := startSinceBoot(stat)
	if !ok {
		return time.Time{}
	}
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts) != nil {
		return time.Time{}
	}
	return time.Now().Add(since - time.Duration(ts.Nano()))
}

// startSinceBoot is the starttime field of a /proc/<pid>/stat line: how
// long after boot the process was created.
func startSinceBoot(stat []byte) (time.Duration, bool) {
	// The command name (field 2) is in parentheses and may hold spaces;
	// the fields after the last ")" start with field 3.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(stat[i+1:]))
	const starttime = 22 - 3
	if len(f) <= starttime {
		return 0, false
	}
	ticks, err := strconv.ParseInt(f[starttime], 10, 64)
	if err != nil || ticks < 0 {
		return 0, false
	}
	// One tick is 10 ms; ticks*time.Second would overflow after 2.9 years
	// of uptime.
	return time.Duration(ticks) * (time.Second / 100), true
}
