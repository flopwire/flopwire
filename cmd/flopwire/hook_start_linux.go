package main

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// processStart is when this process was created (its fork, before the
// exec of this binary): /proc/self/stat's starttime, in clock ticks after
// boot (USER_HZ, 100 on every Linux ABI), plus /proc/stat's btime. The
// zero time when either cannot be read.
func processStart() time.Time {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return time.Time{}
	}
	// The command name (field 2) is in parentheses and may hold spaces;
	// the fields after the last ")" start with field 3.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return time.Time{}
	}
	f := strings.Fields(string(stat[i+1:]))
	const starttime = 22 - 3
	if len(f) <= starttime {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(f[starttime], 10, 64)
	if err != nil {
		return time.Time{}
	}
	all, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for line := range strings.SplitSeq(string(all), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / 100)
		}
	}
	return time.Time{}
}
