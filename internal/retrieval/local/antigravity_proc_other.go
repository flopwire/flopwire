//go:build !darwin && !linux

package local

import "context"

func antigravityPIDs() ([]string, string, bool) { return nil, "", false }

func antigravityOpenHolders(context.Context, string, []string) (map[string]int, bool) {
	return nil, false
}
