//go:build !darwin && !linux

package main

import "time"

// processStart is unknown on this OS; the hook measures its age from its
// own start instead.
func processStart() time.Time { return time.Time{} }
