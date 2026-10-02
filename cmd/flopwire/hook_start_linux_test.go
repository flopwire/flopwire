package main

import (
	"fmt"
	"testing"
	"time"
)

// starttime is read from a stat line whose command name holds spaces and
// a parenthesis, also on a machine up for years.
func TestStartSinceBoot(t *testing.T) {
	for _, up := range []time.Duration{90 * time.Second, 4 * 365 * 24 * time.Hour} {
		ticks := int64(up / (time.Second / 100))
		stat := fmt.Sprintf("4242 (flop wire) x) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 %d 1000000 200 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n", ticks)
		got, ok := startSinceBoot([]byte(stat))
		if !ok || got != up {
			t.Fatalf("up %v: got %v %v", up, got, ok)
		}
	}
}
