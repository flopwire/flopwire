//go:build !darwin

package upgrade

import "context"

// RestartLaunchd does nothing off macOS: the device agent is restarted by
// whatever supervises it.
func RestartLaunchd(context.Context, string) (string, error) { return "", nil }
