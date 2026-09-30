//go:build !darwin && !linux

package agent

import "log/slog"

// watcher is a no-op here: the sweep and the fast lane cover everything.
type watcher struct{ events chan watchEvent }

func newWatcher(*slog.Logger) *watcher   { return &watcher{events: make(chan watchEvent)} }
func (w *watcher) set([]string) []string { return nil }
func (w *watcher) close()                {}
