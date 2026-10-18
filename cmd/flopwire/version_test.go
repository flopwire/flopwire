package main

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	mod := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: v}}
	}
	cases := []struct {
		name   string
		ldflag string
		info   *debug.BuildInfo
		want   string
	}{
		{"ldflag wins", "v1.2.3", mod("v9.9.9"), "v1.2.3"},
		{"go install module version", "dev", mod("v0.2.0"), "v0.2.0"},
		{"checkout build stays dev", "dev", mod("(devel)"), "dev"},
		{"no module version", "dev", mod(""), "dev"},
		{"no build info", "dev", nil, "dev"},
	}
	for _, tc := range cases {
		if got := resolveVersion(tc.ldflag, tc.info); got != tc.want {
			t.Errorf("%s: resolveVersion(%q) = %q, want %q", tc.name, tc.ldflag, got, tc.want)
		}
	}
}
