//go:build !race

package perfguard

// Race reports whether the binary was built with the race detector, which
// inflates allocation counts.
const Race = false
