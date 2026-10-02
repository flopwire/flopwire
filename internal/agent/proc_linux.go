//go:build linux

package agent

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, which Linux fixes at 100 for /proc.
const clockTicks = 100

// processStart is when pid started: its start in ticks after boot
// (/proc/<pid>/stat field 22) plus the boot time (/proc/stat btime).
func processStart(pid int) (time.Time, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return time.Time{}, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	st, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, l := range strings.Split(string(st), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / clockTicks), true
		}
	}
	return time.Time{}, false
}
