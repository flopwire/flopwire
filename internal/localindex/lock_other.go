//go:build !unix

package localindex

import "os"

// tryLock is a no-op where flock is unavailable.
func tryLock(*os.File) error { return nil }
