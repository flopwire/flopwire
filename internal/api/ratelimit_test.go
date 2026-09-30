package api

import (
	"fmt"
	"testing"
	"time"
)

func TestTokenBucketsPartitionedSaturationCannotRefreshDepletedTarget(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTokenBuckets(Rate{Burst: 1, Refill: time.Hour, MaxKeys: 2, IdleTTL: time.Minute})
	b.now = func() time.Time { return now }
	b.hash = func(string) uint64 { return 0 }
	if !b.allow("target") || b.allow("target") {
		t.Fatal("target bucket did not deplete")
	}
	for i := 0; i < bucketWays-1; i++ {
		if !b.allow(fmt.Sprintf("protected-%d", i)) {
			t.Fatal("set-associative protected way rejected")
		}
	}
	if !b.allow("legitimate-overflow") {
		t.Fatal("full protected set denied its shared overflow allowance")
	}
	for i := 0; i < 100; i++ {
		now = now.Add(10 * time.Second)
		if b.allow("target") {
			t.Fatal("depleted target refreshed before its refill interval")
		}
		_ = b.allow(fmt.Sprintf("attacker-%d", i))
	}
	if b.allow("target") {
		t.Fatal("target received fresh allowance after churn")
	}
	now = now.Add(2 * time.Minute)
	if !b.allow("replacement") {
		t.Fatal("an idle partition was not reusable")
	}
}

func TestAtomicDimensionsDoNotConsumeIdentityWhenIPRejects(t *testing.T) {
	now := time.Unix(1000, 0)
	newBuckets := func() *tokenBuckets {
		b := newTokenBuckets(Rate{Burst: 1, Refill: time.Hour, MaxKeys: 8, IdleTTL: time.Hour})
		b.now = func() time.Time { return now }
		b.hash = func(key string) uint64 {
			switch key {
			case "ip-a", "account-a":
				return 0
			default:
				return 1
			}
		}
		return b
	}
	ip, identity := newBuckets(), newBuckets()
	if ipAllowed, identityAllowed := admitDimensions(ip, "ip-a", identity, "account-a"); !ipAllowed || !identityAllowed {
		t.Fatal("initial request rejected")
	}
	if ipAllowed, identityAllowed := admitDimensions(ip, "ip-a", identity, "account-b"); ipAllowed || !identityAllowed {
		t.Fatalf("dimensions ip=%v identity=%v", ipAllowed, identityAllowed)
	}
	if ipAllowed, identityAllowed := admitDimensions(ip, "ip-b", identity, "account-b"); !ipAllowed || !identityAllowed {
		t.Fatal("IP rejection consumed the untouched identity bucket")
	}
}

func TestFullSetSaturationAdmitsLegitimateOverflowWithoutEviction(t *testing.T) {
	b := newTokenBuckets(Rate{Burst: 1, Refill: time.Hour, MaxKeys: 4, IdleTTL: time.Hour})
	b.hash = func(string) uint64 { return 0 }
	for i := 0; i < bucketWays; i++ {
		if !b.allow(fmt.Sprintf("known-%d", i)) {
			t.Fatalf("known key %d rejected", i)
		}
	}
	if !b.allow("legitimate-new") {
		t.Fatal("full fixed set denied the first legitimate unseen key")
	}
	if b.allow("attacker-churn") {
		t.Fatal("shared overflow did not bound unseen-key churn")
	}
	if b.allow("known-0") {
		t.Fatal("overflow churn reset a protected depleted target")
	}
}
