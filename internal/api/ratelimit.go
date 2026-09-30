package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"hash/maphash"
	"io"
	"net/http"
	"sync"
	"time"
)

type Rate struct {
	Burst   int
	Refill  time.Duration
	MaxKeys int
	IdleTTL time.Duration
}
type bucket struct {
	tokens   float64
	at       time.Time
	lastSeen time.Time
}
type tokenBuckets struct {
	mu           sync.Mutex
	rate         Rate
	now          func() time.Time
	hash         func(string) uint64
	slots        []bucketSlot
	overflow     []bucket
	overflowUsed []bool
}

type bucketSlot struct {
	used        bool
	fingerprint [32]byte
	bucket      bucket
}

type bucketPlan struct {
	slotIndex     int
	overflowIndex int
	slot          bucketSlot
	overflow      bucket
	useOverflow   bool
}

const bucketWays = 4

func newTokenBuckets(rate Rate) *tokenBuckets {
	if rate.MaxKeys <= 0 {
		rate.MaxKeys = 10000
	}
	if rate.MaxKeys < 256 {
		rate.MaxKeys = 256
	}
	if rate.IdleTTL <= 0 {
		rate.IdleTTL = time.Duration(rate.Burst) * rate.Refill * 2
		if rate.IdleTTL < 10*time.Minute {
			rate.IdleTTL = 10 * time.Minute
		}
	}
	seed := maphash.MakeSeed()
	setCount := (rate.MaxKeys + bucketWays - 1) / bucketWays
	b := &tokenBuckets{rate: rate, now: time.Now, slots: make([]bucketSlot, setCount*bucketWays), overflow: make([]bucket, setCount), overflowUsed: make([]bool, setCount)}
	b.hash = func(key string) uint64 { return maphash.String(seed, key) }
	return b
}
func (b *tokenBuckets) allow(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	plan, allowed := b.checkLocked(key, b.now())
	b.applyLocked(plan)
	return allowed
}

// checkLocked computes an admission without mutating state. Four protected
// ways retain known clients; unseen collisions share a per-set overflow bucket.
func (b *tokenBuckets) checkLocked(key string, now time.Time) (bucketPlan, bool) {
	partition := b.hash(key)
	fingerprint := sha256.Sum256([]byte(key))
	setIndex := int(partition % uint64(len(b.overflow)))
	base := setIndex * bucketWays
	replacement := -1
	for way := 0; way < bucketWays; way++ {
		idx := base + way
		slot := b.slots[idx]
		if slot.used && slot.fingerprint == fingerprint {
			next, allowed := b.advance(slot.bucket, true, now)
			return bucketPlan{slotIndex: idx, slot: bucketSlot{used: true, fingerprint: fingerprint, bucket: next}}, allowed
		}
		if replacement < 0 && (!slot.used || now.Sub(slot.bucket.lastSeen) > b.rate.IdleTTL) {
			replacement = idx
		}
	}
	if replacement >= 0 {
		next, allowed := b.advance(bucket{}, false, now)
		return bucketPlan{slotIndex: replacement, slot: bucketSlot{used: true, fingerprint: fingerprint, bucket: next}}, allowed
	}
	next, allowed := b.advance(b.overflow[setIndex], b.overflowUsed[setIndex], now)
	return bucketPlan{overflowIndex: setIndex, overflow: next, useOverflow: true}, allowed
}

func (b *tokenBuckets) advance(v bucket, initialized bool, now time.Time) (bucket, bool) {
	if !initialized {
		v = bucket{tokens: float64(b.rate.Burst), at: now, lastSeen: now}
	}
	if elapsed := now.Sub(v.at); elapsed > 0 {
		v.tokens += float64(elapsed) / float64(b.rate.Refill)
		if v.tokens > float64(b.rate.Burst) {
			v.tokens = float64(b.rate.Burst)
		}
		v.at = now
	}
	if v.tokens < 1 {
		v.lastSeen = now
		return v, false
	}
	v.tokens--
	v.lastSeen = now
	return v, true
}

func (b *tokenBuckets) applyLocked(plan bucketPlan) {
	if plan.useOverflow {
		b.overflow[plan.overflowIndex] = plan.overflow
		b.overflowUsed[plan.overflowIndex] = true
		return
	}
	b.slots[plan.slotIndex] = plan.slot
}

func admitDimensions(ip *tokenBuckets, ipKey string, identity *tokenBuckets, identityKey string) (bool, bool) {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	identity.mu.Lock()
	defer identity.mu.Unlock()
	now := ip.now()
	ipPlan, ipAllowed := ip.checkLocked(ipKey, now)
	identityPlan, identityAllowed := identity.checkLocked(identityKey, now)
	if ipAllowed && identityAllowed {
		ip.applyLocked(ipPlan)
		identity.applyLocked(identityPlan)
	} else {
		// Preserve denial state only. An allowed dimension is not consumed when
		// another dimension rejects the request.
		if !ipAllowed {
			ip.applyLocked(ipPlan)
		}
		if !identityAllowed {
			identity.applyLocked(identityPlan)
		}
	}
	return ipAllowed, identityAllowed
}

func identityHint(r *http.Request, field string) string {
	if r.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		return ""
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	v, _ := body[field].(string)
	return hashIdentifier(v)
}

func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
