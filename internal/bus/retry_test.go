package bus_test

import (
	"context"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestExplicitRetryIdentityReplayAndAncestry(t *testing.T) {
	f := newFixture(t)
	c := f.device(f.user("retry"))
	f.present(c, live("source-retry", "claude", "/retry", false), live("target-retry", "codex", "/retry", false))
	req := busproto.SendRequest{FromSession: "source-retry", To: "target-retry", Body: "synthetic retry", Refs: []string{"archive:synthetic"}}
	source, err := f.s.Send(context.Background(), c, req)
	if err != nil {
		t.Fatal(err)
	}
	req.RetryOf = source.ID
	if _, err = f.s.Send(context.Background(), c, req); err == nil {
		t.Fatal("queued source accepted")
	}
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='session_ended' WHERE id=$1`, source.ID)
	changed := req
	changed.Body = "different"
	if _, err = f.s.Send(context.Background(), c, changed); err == nil {
		t.Fatal("changed body accepted")
	}
	changed = req
	changed.Refs = nil
	if _, err = f.s.Send(context.Background(), c, changed); err == nil {
		t.Fatal("changed refs accepted")
	}
	const n = 8
	var wg sync.WaitGroup
	results := make(chan busproto.SendResponse, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := f.s.Send(context.Background(), c, req)
			results <- out
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	fresh := 0
	for out := range results {
		if id == "" {
			id = out.ID
		}
		if out.ID != id || out.RetryOf != source.ID || out.ThreadID != source.ThreadID {
			t.Fatalf("bad receipt: %+v", out)
		}
		if !out.Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("fresh sends=%d", fresh)
	}
	f.exec(`UPDATE bus_messages SET state='delivered' WHERE id=$1`, id)
	replay, err := f.s.Send(context.Background(), c, req)
	if err != nil || !replay.Replayed || replay.State != busproto.StateDelivered {
		t.Fatalf("delivered replay: %+v %v", replay, err)
	}
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='session_ended' WHERE id=$1`, id)
	req.RetryOf = id
	next, err := f.s.Send(context.Background(), c, req)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == id || next.ThreadID != source.ThreadID {
		t.Fatalf("bad second generation: %+v", next)
	}
	req.RetryOf = ""
	if _, err = f.s.Send(context.Background(), c, req); err == nil {
		t.Fatal("ordinary duplicate bypassed")
	}
	// An unrelated ordinary duplicate still blocks even validated ancestry.
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='session_ended' WHERE id=$1`, next.ID)
	f.exec(`INSERT INTO bus_messages SELECT (jsonb_populate_record(NULL::bus_messages,to_jsonb(m)||jsonb_build_object('id','munrelatedretry','seq',nextval('bus_messages_seq'),'retry_of',NULL))).* FROM bus_messages m WHERE id=$1`, source.ID)
	req.RetryOf = next.ID
	if _, err = f.s.Send(context.Background(), c, req); err == nil {
		t.Fatal("unrelated duplicate bypassed")
	}
	if f.count(`SELECT count(*) FROM bus_messages WHERE retry_of=$1`, next.ID) != 0 {
		t.Fatal("refusal consumed source")
	}
	f.exec(`DELETE FROM bus_messages WHERE id='munrelatedretry'`)
	if _, err = f.s.Send(context.Background(), c, req); err != nil {
		t.Fatalf("refused retry unavailable: %v", err)
	}
}

func TestExplicitRetryRejectsInvalidSourcesAndChanges(t *testing.T) {
	f := newFixture(t)
	user := f.user("invalidretry")
	c := f.device(user)
	f.present(c, live("retry-sender", "claude", "/retry", false), live("retry-target", "codex", "/retry", false), live("retry-other", "codex", "/retry", false), live("retry-sender-other", "claude", "/retry", false))
	req := busproto.SendRequest{FromSession: "retry-sender", FromAgent: "claude", To: "retry-target", Body: "synthetic invalid retry", Intent: "inform", Refs: []string{"archive:synthetic"}}
	source, err := f.s.Send(context.Background(), c, req)
	if err != nil {
		t.Fatal(err)
	}
	req.RetryOf = source.ID
	for _, state := range []string{"queued", "held", "claimed", "delivered", "read", "expired", "refused"} {
		f.exec(`UPDATE bus_messages SET state=$2,reason='' WHERE id=$1`, source.ID, state)
		if _, err := f.s.Send(context.Background(), c, req); err == nil {
			t.Fatalf("state %s accepted", state)
		}
	}
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='unconfirmed' WHERE id=$1`, source.ID)
	if _, err := f.s.Send(context.Background(), c, req); err == nil {
		t.Fatal("unconfirmed accepted")
	}
	f.exec(`UPDATE bus_messages SET reason='session_ended' WHERE id=$1`, source.ID)
	for _, change := range []func(*busproto.SendRequest){
		func(r *busproto.SendRequest) { r.To = "retry-other" },
		func(r *busproto.SendRequest) { r.To = "@invalidretry" },
		func(r *busproto.SendRequest) { r.FromSession = "retry-sender-other" },
		func(r *busproto.SendRequest) { r.Intent = "request" },
		func(r *busproto.SendRequest) { r.ReplyTo = source.ID },
		func(r *busproto.SendRequest) { r.RetryOf = "mnonexistent" },
	} {
		changed := req
		change(&changed)
		if _, err := f.s.Send(context.Background(), c, changed); err == nil {
			t.Fatalf("change accepted: %+v", changed)
		}
	}
	otherDevice := f.device(user)
	f.present(otherDevice, live("retry-sender", "claude", "/retry", false))
	if _, err := f.s.Send(context.Background(), otherDevice, req); err == nil {
		t.Fatal("other device accepted")
	}
	stranger := f.device(f.user("strangerretry"))
	f.present(stranger, live("retry-sender", "claude", "/retry", false))
	if _, err := f.s.Send(context.Background(), stranger, req); err == nil {
		t.Fatal("other user accepted")
	}
	if got := f.count(`SELECT count(*) FROM bus_messages WHERE retry_of=$1`, source.ID); got != 0 {
		t.Fatalf("invalid attempts produced %d successors", got)
	}
}

func TestExplicitRetryCapacityRefusalDoesNotConsumeSource(t *testing.T) {
	f := newFixture(t)
	c := f.device(f.user("capacityretry"))
	f.present(c, live("capacity-source", "claude", "/retry", false), live("capacity-target", "codex", "/retry", false))
	req := busproto.SendRequest{FromSession: "capacity-source", To: "capacity-target", Body: "synthetic capacity retry"}
	source, err := f.s.Send(context.Background(), c, req)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='session_ended' WHERE id=$1`, source.ID)
	f.exec(`INSERT INTO bus_messages SELECT (jsonb_populate_record(NULL::bus_messages,to_jsonb(m)||jsonb_build_object('id','mcapacity'||n,'seq',nextval('bus_messages_seq'),'body_sha',decode(repeat('00',32),'hex'),'state','queued','reason',''))).* FROM bus_messages m CROSS JOIN generate_series(1,$2::integer) n WHERE m.id=$1`, source.ID, busproto.MaxUndelivered)
	req.RetryOf = source.ID
	if _, err := f.s.Send(context.Background(), c, req); err == nil {
		t.Fatal("capacity refusal missing")
	}
	if f.count(`SELECT count(*) FROM bus_messages WHERE retry_of=$1`, source.ID) != 0 {
		t.Fatal("capacity refusal consumed retry")
	}
	f.exec(`DELETE FROM bus_messages WHERE id LIKE 'mcapacity%'`)
	out, err := f.s.Send(context.Background(), c, req)
	if err != nil || out.Replayed || out.RetryOf != source.ID {
		t.Fatalf("retry after capacity cleared: %+v %v", out, err)
	}
}

func TestExplicitRetryPreservesReplyAndAcceptance(t *testing.T) {
	f := newFixture(t)
	sender := f.device(f.user("replyretry"))
	recipient := f.device(f.user("recipientretry"))
	f.present(sender, live("reply-source", "claude", "/retry", false))
	f.present(recipient, live("reply-target", "codex", "/retry", false))
	root, err := f.send(sender, "reply-source", "reply-target", "synthetic root")
	if err != nil {
		t.Fatal(err)
	}
	req := busproto.SendRequest{FromSession: "reply-source", To: "reply-target", Body: "synthetic reply", ReplyTo: root.ID}
	source, err := f.s.Send(context.Background(), sender, req)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='session_ended' WHERE id=$1`, source.ID)
	req.RetryOf = source.ID
	out, err := f.s.Send(context.Background(), sender, req)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != busproto.StateHeld || out.ThreadID != root.ThreadID {
		t.Fatalf("retry lost acceptance or thread: %+v", out)
	}
	inbox, err := f.s.Inbox(context.Background(), sender, busproto.InboxQuery{Session: "reply-source", SentOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range inbox.Messages {
		if m.ID == out.ID {
			found = true
			if m.RetryOf != source.ID || m.ReplyTo != root.ID {
				t.Fatalf("retry envelope lost links: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("successor missing from inbox")
	}
}

func TestExplicitRetryRootHasNoThreadCeiling(t *testing.T) {
	f := newFixture(t)
	c := f.device(f.user("threadretry"))
	f.present(c, live("thread-source", "claude", "/retry", false), live("thread-target", "codex", "/retry", false))
	req := busproto.SendRequest{FromSession: "thread-source", To: "thread-target", Body: "synthetic thread retry"}
	source, err := f.s.Send(context.Background(), c, req)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='session_ended' WHERE id=$1`, source.ID)
	f.exec(`INSERT INTO bus_messages SELECT (jsonb_populate_record(NULL::bus_messages,to_jsonb(m)||jsonb_build_object('id','mthreadretry'||n,'seq',nextval('bus_messages_seq'),'body_sha',decode(repeat('00',32),'hex'),'state','delivered','reason',''))).* FROM bus_messages m CROSS JOIN generate_series(1,$2::integer) n WHERE m.id=$1`, source.ID, 8)
	req.RetryOf = source.ID
	out, err := f.s.Send(context.Background(), c, req)
	if err != nil || out.ThreadID != source.ThreadID || out.RetryOf != source.ID {
		t.Fatalf("retry after eight thread messages: %+v %v", out, err)
	}
}
