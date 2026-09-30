//go:build !darwin && !linux

package transcript

import "os"

// statIdentity has no file identity or change time here. Decide then treats
// every size-preserving change as a rewrite and verifies every append.
// Windows (volume serial + file index, NTFS ChangeTime) lands with the
// Windows device agent.
func statIdentity(os.FileInfo) (FileID, int64, bool) { return FileID{}, 0, false }
