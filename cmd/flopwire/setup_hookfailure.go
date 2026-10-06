package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const hookFailureFile = "hook-last-failure"
const hookFailureMax = 96

// Read only a small regular file. O_NONBLOCK prevents a substituted FIFO
// from blocking setup, and O_NOFOLLOW rejects a substituted symlink.
func readHookFailure(file string) string {
	fd, err := syscall.Open(file, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	f := os.NewFile(uintptr(fd), file)
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > hookFailureMax {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(f, hookFailureMax+1))
	if err != nil || len(b) > hookFailureMax {
		return ""
	}
	return parseHookFailure(b)
}

// The grammar is deliberately closed: never echo arbitrary receipt contents.
func parseHookFailure(b []byte) string {
	fields := strings.Split(string(b), " ")
	if len(fields) != 4 || fields[0] != "v1" {
		return ""
	}
	stamp, err := time.Parse("2006-01-02T15:04:05Z", fields[1])
	if err != nil || stamp.Year() < 1970 || stamp.Format("2006-01-02T15:04:05Z") != fields[1] || stamp.After(time.Now().UTC().Add(5*time.Minute)) {
		return ""
	}
	codeText := strings.TrimSuffix(fields[3], "\n")
	code, err := strconv.Atoi(codeText)
	if err != nil || code < 1 || code > 255 || strconv.Itoa(code) != codeText || fields[3] != codeText+"\n" {
		return ""
	}
	var reason string
	switch fields[2] {
	case "missing-binary":
		if code != 1 {
			return ""
		}
		reason = "no executable Flopwire binary was found"
	case "hook-exit":
		reason = fmt.Sprintf("the hook binary exited %d", code)
	default:
		return ""
	}
	return fmt.Sprintf("historical shared hook-shim failure at %s: %s. This receipt may come from Claude Code, Codex or Devin; it does not establish current hook health. Run flopwire setup to install/update the cached plugin, then retry a hook", stamp.Format("2006-01-02T15:04:05Z"), reason)
}

func lastHookFailureWarning() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return readHookFailure(filepath.Join(dir, hookFailureFile))
}
