package main

import (
	"strings"
	"testing"
)

func TestSearchMatchingOption(t *testing.T) {
	for _, mode := range []string{"auto", "all", "any"} {
		o, err := parseArgs("search", []string{"autoreview", "--matching", mode})
		if err != nil || o.vals["matching"] != mode {
			t.Fatalf("%s: %+v %v", mode, o, err)
		}
	}
	if !strings.Contains(toolHelp["search"], "--matching all") || !strings.Contains(toolHelp["search"], "--matching any") {
		t.Fatal("matching modes missing from help")
	}
}
