package main

import (
	"context"
	"strings"
	"testing"
)

func TestServeRejectsInvalidPerDeviceFlushLimitBeforeConfiguration(t *testing.T) {
	for _, value := range []string{"0", "-1", "3"} {
		err := serve(context.Background(), []string{"--flush-per-device", value})
		if err == nil || !strings.Contains(err.Error(), "--flush-per-device must be 1 or 2") {
			t.Fatalf("invalid limit %s: %v", value, err)
		}
	}
}
