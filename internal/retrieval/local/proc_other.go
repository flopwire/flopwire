//go:build !darwin && !linux

package local

import "context"

func procInfo(int) (int, string, bool) { return 0, "", false }

func openFiles(context.Context, int) []string { return nil }

func codexOpenFiles() []string { return nil }

func pidAlive(int) bool { return false }
