//go:build linux

package local

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func openFiles(pid int) []string {
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
			out = append(out, openFiles(pid)...)
		}
	}
	return out
}

// pidAlive reports whether a process with this pid exists.
func pidAlive(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}
