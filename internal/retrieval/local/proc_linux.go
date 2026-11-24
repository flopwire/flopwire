//go:build linux

package local

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func procInfo(pid int) (int, string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", false
	}
	s := string(b)
	open, closeIdx := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || closeIdx < open {
		return 0, "", false
	}
	f := strings.Fields(s[closeIdx+1:])
	if len(f) < 2 {
		return 0, "", false
	}
	ppid, err := strconv.Atoi(f[1])
	return ppid, s[open+1 : closeIdx], err == nil
}

func openFiles(_ context.Context, pid int) []string {
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if t, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// codexOpenFiles lists the files every process named codex has open.
func codexOpenFiles() []string {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if _, name, ok := procInfo(pid); ok && strings.Contains(strings.ToLower(name), "codex") {
			out = append(out, openFiles(context.Background(), pid)...)
		}
	}
	return out
}

// pidAlive reports whether a process with this pid exists.
func pidAlive(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// antigravityPIDs reads names only from processes owned by this user.
func antigravityPIDs() ([]string, string, bool) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, "", false
	}
	var pids []string
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			continue
		}
		if _, name, ok := procInfo(pid); ok && name == "agy" {
			pids = append(pids, e.Name())
		}
	}
	return pids, "", true
}

func antigravityOpenHolders(ctx context.Context, dir string, pids []string) (map[string]int, bool) {
	holders := make(map[string]int)
	for _, p := range pids {
		if ctx.Err() != nil {
			return nil, false
		}
		pid, _ := strconv.Atoi(p)
		files := openFiles(ctx, pid)
		if files == nil {
			return nil, false
		}
		for _, f := range files {
			if id := antigravityLockID(dir, f); id != "" {
				noteAntigravityHolder(holders, id, pid)
			}
		}
	}
	return holders, true
}
