//go:build !darwin && !linux

package agent

import "time"

func processStart(int) (time.Time, bool) { return time.Time{}, false }
