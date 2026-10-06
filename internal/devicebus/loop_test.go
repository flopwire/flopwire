package devicebus

import (
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestPollPresenceUsesLearnedSkewOnCopy(t *testing.T) {
	for _, skew := range []time.Duration{-time.Hour, 0, time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			b := &Bus{st: &store{}}
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			idle := now.Add(-2 * time.Hour)
			local := []busproto.PresenceSession{{SessionID: "known", IdleSince: idle, IdleKnown: true}, {SessionID: "unknown"}}
			first := b.pollPresence(local)
			if first[0].IdleKnown || !first[0].IdleSince.IsZero() {
				t.Fatal("invented server time before learning skew")
			}
			b.learnSkew(now, now, now.Add(skew))
			got := b.pollPresence(local)
			if !got[0].IdleKnown || !got[0].IdleSince.Equal(idle.Add(skew)) {
				t.Fatalf("converted: %+v", got[0])
			}
			age := busproto.IdleAge(false, got[0].IdleSince, now.Add(skew))
			if age == nil || *age != 7200 {
				t.Fatalf("age: %v", age)
			}
			if got[1].IdleKnown || !got[1].IdleSince.IsZero() {
				t.Fatal("fabricated unknown evidence")
			}
			if !local[0].IdleSince.Equal(idle) || !local[0].IdleKnown {
				t.Fatal("mutated local presence")
			}
			b.learnSkew(now, now, now.Add(skew-time.Minute))
			_ = b.pollPresence(local)
			if !local[0].IdleSince.Equal(idle) {
				t.Fatal("skew update changed comparison state")
			}
		})
	}
}
