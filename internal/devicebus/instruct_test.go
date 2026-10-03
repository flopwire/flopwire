package devicebus

import (
	"path/filepath"
	"testing"
	"time"
)

func (lb *localBus) takeWith(t *testing.T, session, source string, started time.Time) (bool, []string) {
	t.Helper()
	instruct, got, err := lb.TakeWith(ctx, session, "", Limit{}, &Instruction{Source: source, HookStart: started})
	if err != nil {
		t.Fatal(err)
	}
	return instruct, ids(got)
}

// The SessionStart hook took the instruction and was killed before
// printing it (#101): the session's next hook prints it, before any
// message, once the lease ends; while the lease is out no hook gets a
// message, so none arrives before the instruction.
func TestInstructionReofferedAfterAKilledSessionStart(t *testing.T) {
	lb := newLocalBus(t)
	if instruct, _ := lb.takeWith(t, "bbbb3333", "startup", lb.cfg.Now()); !instruct {
		t.Fatal("the first SessionStart did not get the instruction")
	}
	// The hook is killed. A message arrives; a prompt hook within the
	// lease gets nothing.
	out, _ := lb.send(t, "aaaa1111", "bbbb", "after the start")
	if instruct, got := lb.takeWith(t, "bbbb3333", "", lb.cfg.Now()); instruct || len(got) != 0 {
		t.Fatalf("within the lease: instruct %v, messages %v", instruct, got)
	}
	lb.advance(LeaseFor)
	instruct, got := lb.takeWith(t, "bbbb3333", "", lb.cfg.Now())
	if !instruct || len(got) != 1 || got[0] != out.ID {
		t.Fatalf("after the lease: instruct %v, messages %v", instruct, got)
	}
	if err := lb.ConfirmInstruction(ctx, "bbbb3333"); err != nil {
		t.Fatal(err)
	}
	if err := lb.Confirm(ctx, "bbbb3333", got); err != nil {
		t.Fatal(err)
	}
	lb.advance(LeaseFor)
	if instruct, _ := lb.takeWith(t, "bbbb3333", "", lb.cfg.Now()); instruct {
		t.Fatal("offered again after its confirmation")
	}
}

// Normal operation: one instruction per session, also when two hook
// configs run SessionStart at once, and across an agent restart; a
// resume or compaction after the confirmation renews it, once.
func TestInstructionOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.db")
	lb := newLocalBus(t)
	lb.Bus = openBus(t, path, lb.cfg, lb.p)
	start := lb.cfg.Now()
	a, _ := lb.takeWith(t, "bbbb3333", "startup", start)
	b, _ := lb.takeWith(t, "bbbb3333", "startup", start)
	if !a || b {
		t.Fatalf("two SessionStart hooks at once: %v %v", a, b)
	}
	if err := lb.ConfirmInstruction(ctx, "bbbb3333"); err != nil {
		t.Fatal(err)
	}
	if again, _ := lb.takeWith(t, "bbbb3333", "startup", start); again {
		t.Fatal("the second hook got it after the first confirmed")
	}
	lb.Close()
	lb.Bus = openBus(t, path, lb.cfg, lb.p)
	lb.advance(time.Minute)
	for _, src := range []string{"", "startup"} {
		if instruct, _ := lb.takeWith(t, "bbbb3333", src, lb.cfg.Now()); instruct {
			t.Fatalf("source %q after a restart: offered again", src)
		}
	}
	// A compaction: renewed for the hook that started after the
	// confirmation, and only once.
	lb.advance(time.Minute)
	started := lb.cfg.Now()
	c1, _ := lb.takeWith(t, "bbbb3333", "compact", started)
	c2, _ := lb.takeWith(t, "bbbb3333", "compact", started)
	if !c1 || c2 {
		t.Fatalf("compact: %v %v", c1, c2)
	}
	lb.advance(time.Millisecond)
	if err := lb.ConfirmInstruction(ctx, "bbbb3333"); err != nil {
		t.Fatal(err)
	}
	if again, _ := lb.takeWith(t, "bbbb3333", "compact", started); again {
		t.Fatal("a compact hook that started before the confirmation renewed it")
	}
	if resumed, _ := lb.takeWith(t, "bbbb3333", "resume", lb.cfg.Now().Add(time.Millisecond)); !resumed {
		t.Fatal("a resume did not renew it")
	}
	// Another session is owed its own.
	if other, _ := lb.takeWith(t, "aaaa1111", "", lb.cfg.Now()); !other {
		t.Fatal("a session whose first hook is a prompt hook was not offered it")
	}
}

// A hook that never received the answer gives the instruction back:
// owed as before, the lease not counted. A hook that takes it and never
// confirms, MaxAttempts times, ends the offers.
func TestInstructionReturnedAndCapped(t *testing.T) {
	lb := newLocalBus(t)
	lb.takeWith(t, "bbbb3333", "startup", lb.cfg.Now())
	if err := lb.ReturnInstruction(ctx, "bbbb3333"); err != nil {
		t.Fatal(err)
	}
	for i := range MaxAttempts {
		if instruct, _ := lb.takeWith(t, "bbbb3333", "", lb.cfg.Now()); !instruct {
			t.Fatalf("offer %d after the return", i+1)
		}
		lb.advance(LeaseFor)
	}
	out, _ := lb.send(t, "aaaa1111", "bbbb", "after the instruction gave up")
	instruct, got := lb.takeWith(t, "bbbb3333", "", lb.cfg.Now())
	if instruct || len(got) != 1 || got[0] != out.ID {
		t.Fatalf("after %d unconfirmed offers: instruct %v, messages %v", MaxAttempts, instruct, got)
	}
}
