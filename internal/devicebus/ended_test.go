package devicebus

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

var (
	refB = Ref{"claude", "bbbb3333"}
	refA = Ref{"codex", "aaaa2222"}
)

func held(refs ...Ref) Registry {
	r := Registry{Held: map[Ref]Holder{}, Read: map[string]bool{"claude": true, "codex": true, "devin": true}}
	for _, ref := range refs {
		r.Held[ref] = Holder{ID: "pid:1:" + ref.Session}
	}
	return r
}

func (r Registry) gone(refs ...Ref) Registry { r.Gone = append(r.Gone, refs...); return r }

func sentItem(t *testing.T, b *Bus, session, id string) busproto.InboxItem {
	t.Helper()
	in, err := b.Inbox(ctx, busproto.InboxQuery{Session: session, SentOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range in.Messages {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("%s not in %s's inbox", id, session)
	return busproto.InboxItem{}
}

func observe(t *testing.T, b *Bus, r Registry) map[Ref]bool {
	t.Helper()
	ended, err := b.Observe(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	return ended
}

// A session whose registry entry names a process that is gone ended: its
// queued message is undelivered (session_ended) in the sender's inbox at
// once, and no hook gets it.
func TestSessionEndedByItsRegistry(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "aaaa1111", "bbbb", "to a session that is about to end")
	if err != nil {
		t.Fatal(err)
	}
	if ended := observe(t, lb.Bus, held(refB)); ended[refB] {
		t.Fatal("a held session reads as ended")
	}
	lb.advance(time.Second)
	if ended := observe(t, lb.Bus, held().gone(refB)); !ended[refB] {
		t.Fatalf("a dead registry entry did not end the session: %v", ended)
	}
	if m := sentItem(t, lb.Bus, "aaaa1111", out.ID); m.State != busproto.StateUndelivered || m.Reason != busproto.ReasonSessionEnded {
		t.Fatalf("sender's inbox: %s (%s)", m.State, m.Reason)
	}
	if got, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); len(got) != 0 {
		t.Fatalf("a message of an ended session was taken: %v", ids(got))
	}
	if st := lb.Status(ctx); st.Pending != 0 {
		t.Fatalf("pending after the end: %d", st.Pending)
	}
}

// A registry entry that is missing (not dead) ends a session held before
// only when it is still missing a read EndDebounce later; idleness, an
// unreadable registry and an entry that could not be read end nothing.
func TestSessionEndedWhenItsEntryStaysMissing(t *testing.T) {
	lb := newLocalBus(t)
	observe(t, lb.Bus, held(refB, refA))
	lb.advance(time.Hour) // idle for an hour, still held
	if ended := observe(t, lb.Bus, held(refB, refA)); len(ended) != 0 {
		t.Fatalf("idleness ended %v", ended)
	}
	r := held(refA)
	if ended := observe(t, lb.Bus, r); ended[refB] {
		t.Fatal("ended at the first read without its entry")
	}
	lb.advance(EndDebounce / 2)
	if ended := observe(t, lb.Bus, r); ended[refB] {
		t.Fatal("ended before EndDebounce")
	}
	lb.advance(EndDebounce)
	if ended := observe(t, lb.Bus, r); !ended[refB] || ended[refA] {
		t.Fatalf("after EndDebounce: %v", ended)
	}
	// codex's registry not read, or its entry unreadable: no end.
	r = held()
	delete(r.Read, "codex")
	lb.advance(time.Minute)
	observe(t, lb.Bus, r)
	lb.advance(time.Minute)
	if ended := observe(t, lb.Bus, r); ended[refA] {
		t.Fatal("ended by a registry that was not read")
	}
	r = held()
	r.Unknown = []Ref{refA}
	observe(t, lb.Bus, r)
	lb.advance(time.Minute)
	if ended := observe(t, lb.Bus, r); ended[refA] {
		t.Fatal("ended by an entry that could not be read")
	}
	// It comes back (rewritten entry): not missing any more.
	observe(t, lb.Bus, held(refA))
	lb.advance(time.Minute)
	if ended := observe(t, lb.Bus, held(refA)); ended[refA] {
		t.Fatal("held again and ended")
	}
}

// The SessionEnd hook ends a session at once. The process that held it
// when it ended still holding it does not revive it (it is exiting); a
// resume does: a later process holding it, or a later hook of it. A hook
// that started before the end and reports late does not.
func TestSessionEndHookAndResume(t *testing.T) {
	lb := newLocalBus(t)
	r := held(refB)
	r.Held[refB] = Holder{ID: "pid:10", Start: lb.cfg.Now().Add(-time.Minute)}
	observe(t, lb.Bus, r)
	out, _ := lb.send(t, "aaaa1111", "bbbb", "before the end")
	end := lb.cfg.Now()
	if err := lb.End(ctx, Ref{Session: "bbbb3333"}, end); err != nil { // the hook does not always know its harness
		t.Fatal(err)
	}
	if ended := observe(t, lb.Bus, r); !ended[refB] {
		t.Fatal("the exiting process revived its session")
	}
	if m := sentItem(t, lb.Bus, "aaaa1111", out.ID); m.Reason != busproto.ReasonSessionEnded {
		t.Fatalf("after SessionEnd: %s (%s)", m.State, m.Reason)
	}
	if err := lb.Revive(ctx, "bbbb3333", end.Add(-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if ended := observe(t, lb.Bus, held()); !ended[refB] {
		t.Fatal("a hook that started before the end revived the session")
	}
	lb.advance(time.Minute)
	if err := lb.Revive(ctx, "bbbb3333", lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	r.Held[refB] = Holder{ID: "pid:11", Start: lb.cfg.Now()}
	if ended := observe(t, lb.Bus, r); ended[refB] {
		t.Fatal("a hook after the end did not revive the session")
	}
	// Ended again, and still held by the same process: ended. Then held by
	// a process that started later: a resume.
	lb.advance(time.Second)
	if err := lb.End(ctx, refB, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if ended := observe(t, lb.Bus, r); !ended[refB] {
		t.Fatal("the exiting process revived its session")
	}
	r.Held[refB] = Holder{ID: "pid:12", Start: lb.cfg.Now().Add(time.Second)}
	lb.advance(2 * time.Second)
	if ended := observe(t, lb.Bus, r); ended[refB] {
		t.Fatal("a resumed process did not revive the session")
	}
	// A message from before the resume stays undelivered.
	if m := sentItem(t, lb.Bus, "aaaa1111", out.ID); m.State != busproto.StateUndelivered {
		t.Fatalf("after the resume: %s", m.State)
	}
}

// With a server: the messages of a session that ended are reported as
// session_ended. One sent within EndGrace of the session's last live
// sighting arrives after the end and is reported too (the sender was told
// the session is live); one sent later stays queued for a resume (the
// sender was told only_if_resumed). A leased message is left to its
// lease: its late confirmation still counts.
func TestSessionEndedReportedToTheServer(t *testing.T) {
	srv := newFakeServer()
	cfg := testConfig(srv, nil)
	var mu sync.Mutex
	now := time.Now().UTC().Truncate(time.Millisecond)
	cfg.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	p := &presenceSrc{}
	p.set(Session{PresenceSession: busproto.PresenceSession{SessionID: "s1", Agent: "claude"}})
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, p)
	if _, err := b.sessions(ctx); err != nil { // seen live now
		t.Fatal(err)
	}
	at := func(id string, sent time.Time) busproto.Envelope { e := env(id, "s1"); e.Sent = sent; return e }
	queued, leased := at("mq", now), at("ml", now)
	if err := b.st.reconcile(ctx, []busproto.Envelope{leased}, nil, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Take(ctx, "s1", "", Limit{}); len(got) != 1 {
		t.Fatal("take")
	}
	if err := b.st.reconcile(ctx, []busproto.Envelope{leased, queued}, nil, now); err != nil {
		t.Fatal(err)
	}
	observe(t, b, held(Ref{"claude", "s1"}))
	advance(3 * time.Second)
	if err := b.End(ctx, Ref{"claude", "s1"}, now); err != nil {
		t.Fatal(err)
	}
	// Arriving after the end: sent 2 s after the last sighting, and 1 min.
	inGrace, late := at("mg", now.Add(-time.Second)), at("mlate", now.Add(time.Minute))
	if err := b.st.reconcile(ctx, []busproto.Envelope{leased, queued, inGrace, late}, nil, now); err != nil {
		t.Fatal(err)
	}
	b.settleEnded(ctx)
	drainAcks(t, b)
	if got := srv.endedIDs(); !slices.Equal(slices.Sorted(slices.Values(got)), []string{"mg", "mq"}) {
		t.Fatalf("session_ended reports: %v", got)
	}
	if len(srv.undeliveredIDs()) != 0 {
		t.Fatalf("reported unconfirmed: %v", srv.undeliveredIDs())
	}
	// The leased one: its hook confirms late; delivered.
	if err := b.Confirm(ctx, "s1", []string{"ml"}); err != nil {
		t.Fatal(err)
	}
	drainAcks(t, b)
	if a := srv.ackedIDs(); !slices.Equal(a, []string{"ml"}) {
		t.Fatalf("receipts: %v", a)
	}
	// The late one waits for a resume, then a hook takes it.
	if err := b.Revive(ctx, "s1", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	advance(2 * time.Second)
	b.settleEnded(ctx)
	if got, _ := deliver(b, "s1", "", Limit{}); !slices.Equal(ids(got), []string{"mlate"}) {
		t.Fatalf("after the resume: %v", ids(got))
	}
}

// A leased message of a session that ended, whose lease ends unconfirmed,
// is undelivered as session_ended, not offered again.
func TestLeaseEndsAfterTheSessionEnded(t *testing.T) {
	lb := newLocalBus(t)
	out, _ := lb.send(t, "aaaa1111", "bbbb", "taken, then the session ended")
	if got, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); len(got) != 1 {
		t.Fatal("take")
	}
	if err := lb.End(ctx, refB, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if m := sentItem(t, lb.Bus, "aaaa1111", out.ID); m.State != busproto.StateQueued {
		t.Fatalf("while leased: %s", m.State)
	}
	lb.advance(LeaseFor)
	lb.expireLeases(ctx)
	lb.settleEnded(ctx)
	if m := sentItem(t, lb.Bus, "aaaa1111", out.ID); m.State != busproto.StateUndelivered || m.Reason != busproto.ReasonSessionEnded {
		t.Fatalf("after the lease: %s (%s)", m.State, m.Reason)
	}
}

// The ended state survives an agent restart: a session held before the
// restart and missing after it ends.
func TestEndedAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.db")
	lb := newLocalBus(t)
	b1 := openBus(t, path, lb.cfg, lb.p)
	if _, err := b1.Observe(ctx, held(refB)); err != nil {
		t.Fatal(err)
	}
	b1.Close()
	b2 := openBus(t, path, lb.cfg, lb.p)
	observe(t, b2, held())
	lb.advance(EndDebounce)
	if ended := observe(t, b2, held()); !ended[refB] {
		t.Fatal("a session held before the restart and gone after it did not end")
	}
}
