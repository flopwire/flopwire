package main

import (
	"math"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/store"
)

func TestFlushAdmissionPoolBudget(t *testing.T) {
	for _, tc := range []struct {
		pool          int32
		workers, want int
	}{
		{43, 2, 8}, {45, 4, 8}, {100, 4, 8}, {45, 0, 8}, {45, -3, 8},
	} {
		got, err := flushAdmissionBudget(tc.pool, tc.workers)
		if err != nil || got != tc.want {
			t.Fatalf("pool=%d workers=%d budget=%d err=%v", tc.pool, tc.workers, got, err)
		}
	}
	// With four effective workers, 37 connections are reserved for search,
	// parse and other work; overrides leaving 1..7 flush slots stay bounded.
	for slots := 1; slots < 8; slots++ {
		got, err := flushAdmissionBudget(int32(37+slots), 4)
		if err != nil || got != slots {
			t.Fatalf("shrunk pool: slots=%d got=%d err=%v", slots, got, err)
		}
	}
	if _, err := flushAdmissionBudget(37, 4); err == nil || !strings.Contains(err.Error(), "at least 38") || !strings.Contains(err.Error(), "maximum is 37") {
		t.Fatalf("undersized pool error=%v", err)
	}
	if _, err := flushAdmissionBudget(math.MaxInt32, math.MaxInt32); err == nil {
		t.Fatal("overflowing worker budget accepted")
	}
}

func TestFlushAdmissionUsesEffectiveQueueWorkersAndPoolOverride(t *testing.T) {
	for _, raw := range []int{-5, 0, 2, 4} {
		workers, err := serverParseWorkers(raw)
		if err != nil {
			t.Fatal(err)
		}
		q := &ingest.Queue{Workers: raw}
		q.Status() // Initializes defaults without database access.
		if workers != q.Workers {
			t.Fatalf("effective workers=%d queue=%d", workers, q.Workers)
		}
	}
	config, err := store.PoolConfig("postgres://example.invalid/db?pool_max_conns=39", 21)
	if err != nil {
		t.Fatal(err)
	}
	got, err := flushAdmissionBudget(config.MaxConns, 4)
	if err != nil || got != 2 {
		t.Fatalf("URL override budget=%d err=%v", got, err)
	}
}
