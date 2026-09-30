//go:build darwin

package local

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func procInfo(pid int) (int, string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return 0, "", false
	}
	comm := kp.Proc.P_comm[:]
	if i := bytes.IndexByte(comm, 0); i >= 0 {
		comm = comm[:i]
	}
	return int(kp.Eproc.Ppid), string(comm), true
}

// openFiles lists the regular files pid has open, via lsof.
func openFiles(pid int) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-n", "-P", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	var files []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if l := sc.Text(); len(l) > 1 && l[0] == 'n' {
			files = append(files, l[1:])
		}
	}
	return files
}

// codexOpenFiles lists the regular files every process named codex has
// open, with one lsof call.
func codexOpenFiles() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-n", "-P", "-c", "codex", "-Fn").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	var files []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if l := sc.Text(); len(l) > 1 && l[0] == 'n' {
			files = append(files, l[1:])
		}
	}
	return files
}

// pidAlive reports whether a process with this pid exists.
func pidAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}
