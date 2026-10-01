package agent

import "golang.org/x/sys/unix"

// processAlive reports whether a process with this pid exists.
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}
