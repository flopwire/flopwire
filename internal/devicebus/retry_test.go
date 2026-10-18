package devicebus

import (
	"fmt"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

func retryEnded(t *testing.T, lb *localBus, id string) {
	t.Helper()
	if _, err := lb.st.db.Exec(`UPDATE devbus_messages SET state='undelivered',reason='session_ended' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func TestLocalRetryIdentityReplayAndChain(t *testing.T) {
	lb := newLocalBus(t)
	first, err := lb.send(t, "aaaa1111", "bbbb", "retry fixture")
	if err != nil {
		t.Fatal(err)
	}
	retry := func(id string) func(*busproto.SendRequest) { return func(r *busproto.SendRequest) { r.RetryOf = id } }
	if _, err = lb.send(t, "aaaa1111", "bbbb", "retry fixture", retry(first.ID)); code(err) != busproto.CodeBadRequest {
		t.Fatalf("queued retry: %v", err)
	}
	retryEnded(t, lb, first.ID)
	if _, err = lb.send(t, "aaaa2222", "bbbb", "retry fixture", retry(first.ID)); code(err) != busproto.CodeNotFound {
		t.Fatalf("foreign sender: %v", err)
	}
	if _, err = lb.send(t, "aaaa1111", "bbbb", "changed", retry(first.ID)); code(err) != busproto.CodeBadRequest {
		t.Fatalf("changed body: %v", err)
	}
	if _, err = lb.send(t, "aaaa1111", "@"+lb.cfg.User, "retry fixture", retry(first.ID)); code(err) != busproto.CodeBadRequest {
		t.Fatalf("retarget: %v", err)
	}
	child, err := lb.send(t, "aaaa1111", "bbbb", "retry fixture", retry(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if child.ThreadID != first.ThreadID || child.RetryOf != first.ID || child.Replayed {
		t.Fatalf("child: %+v", child)
	}
	if _, err = lb.send(t, "aaaa1111", "bbbb", "retry fixture"); code(err) != busproto.CodeDuplicate {
		t.Fatalf("ordinary dedup: %v", err)
	}
	if _, err = lb.st.db.Exec(`UPDATE devbus_messages SET state='delivered' WHERE id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	replay, err := lb.send(t, "aaaa1111", "bbbb", "retry fixture", retry(first.ID))
	if err != nil || replay.ID != child.ID || !replay.Replayed || replay.State != busproto.StateDelivered {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	retryEnded(t, lb, child.ID)
	grandchild, err := lb.send(t, "aaaa1111", "bbbb", "retry fixture", retry(child.ID))
	if err != nil || grandchild.ThreadID != first.ThreadID {
		t.Fatalf("chain: %+v %v", grandchild, err)
	}
}

func TestLocalRetryConcurrentSubmissions(t *testing.T) {
	lb := newLocalBus(t)
	first, err := lb.send(t, "aaaa1111", "bbbb", "concurrent retry")
	if err != nil {
		t.Fatal(err)
	}
	retryEnded(t, lb, first.ID)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := lb.send(t, "aaaa1111", "bbbb", "concurrent retry", func(r *busproto.SendRequest) { r.RetryOf = first.ID })
			if err != nil {
				t.Error(err)
				return
			}
			ids <- out.ID
		}()
	}
	wg.Wait()
	close(ids)
	var id string
	for got := range ids {
		if id != "" && got != id {
			t.Fatalf("multiple successors: %s %s", id, got)
		}
		id = got
	}
	var n int
	if err := lb.st.db.QueryRow(`SELECT count(*) FROM devbus_retries WHERE source_id=?`, first.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("mapping count %d: %v", n, err)
	}
}

func TestLocalRetryRefusalDoesNotConsumeSuccessor(t *testing.T) {
	lb := newLocalBus(t)
	first, err := lb.send(t, "aaaa1111", "bbbb", "capacity retry")
	if err != nil {
		t.Fatal(err)
	}
	retryEnded(t, lb, first.ID)
	for i := 0; i < busproto.MaxUndelivered; i++ {
		_, err = lb.st.db.Exec(`INSERT INTO devbus_messages(id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,envelope,state,created_at,expires_at) SELECT ?,'local',0,to_session,to_agent,'other','other','capacity-thread',envelope,'queued',created_at,expires_at FROM devbus_messages WHERE id=?`, fmt.Sprintf("capacity-%d", i), first.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	retry := func(r *busproto.SendRequest) { r.RetryOf = first.ID }
	if _, err = lb.send(t, "aaaa1111", "bbbb", "capacity retry", retry); code(err) != busproto.CodeRecipientFull {
		t.Fatalf("capacity: %v", err)
	}
	var n int
	if err = lb.st.db.QueryRow(`SELECT count(*) FROM devbus_retries`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refusal consumed source: %d %v", n, err)
	}
	if _, err = lb.st.db.Exec(`DELETE FROM devbus_messages WHERE id LIKE 'capacity-%'`); err != nil {
		t.Fatal(err)
	}
	child, err := lb.send(t, "aaaa1111", "bbbb", "capacity retry", retry)
	if err != nil || child.Replayed {
		t.Fatalf("retry after refusal: %+v %v", child, err)
	}
}

func TestLocalRetrySurvivesReopenAndMissingSuccessor(t *testing.T) {
	lb := newLocalBus(t)
	first, err := lb.send(t, "aaaa1111", "bbbb", "durable retry")
	if err != nil {
		t.Fatal(err)
	}
	retryEnded(t, lb, first.ID)
	retry := func(r *busproto.SendRequest) { r.RetryOf = first.ID }
	child, err := lb.send(t, "aaaa1111", "bbbb", "durable retry", retry)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	var seq int
	var name string
	if err = lb.st.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	// A new connection reads the committed identity after a lost response.
	var successor string
	if err = reopened.db.QueryRow(`SELECT successor_id FROM devbus_retries WHERE source_id=?`, first.ID).Scan(&successor); err != nil || successor != child.ID {
		t.Fatalf("durable successor %q: %v", successor, err)
	}
	if _, err = lb.st.db.Exec(`DELETE FROM devbus_messages WHERE id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = lb.send(t, "aaaa1111", "bbbb", "durable retry", retry); code(err) != busproto.CodeNotFound {
		t.Fatalf("missing successor created replacement: %v", err)
	}
}

func TestLocalRetryRejectsChangedContentAndTerminalReasons(t *testing.T) {
	lb := newLocalBus(t)
	first, err := lb.send(t, "aaaa1111", "bbbb", "immutable", func(r *busproto.SendRequest) { r.Refs = []string{"archive://fixture"} })
	if err != nil {
		t.Fatal(err)
	}
	retryEnded(t, lb, first.ID)
	for name, mutate := range map[string]func(*busproto.SendRequest){
		"refs":      func(r *busproto.SendRequest) { r.Refs = []string{"archive://changed"} },
		"intent":    func(r *busproto.SendRequest) { r.Intent = "done" },
		"reply":     func(r *busproto.SendRequest) { r.ReplyTo = first.ID },
		"recipient": func(r *busproto.SendRequest) { r.To = "aaaa2222" },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := lb.send(t, "aaaa1111", "bbbb", "immutable", func(r *busproto.SendRequest) { r.RetryOf = first.ID; r.Refs = []string{"archive://fixture"}; mutate(r) })
			if code(err) != busproto.CodeBadRequest {
				t.Fatalf("accepted mutation: %v", err)
			}
		})
	}
	for _, state := range []string{"queued", "leased", "delivered", "refused", "undelivered"} {
		if _, err = lb.st.db.Exec(`UPDATE devbus_messages SET state=?,reason='unconfirmed' WHERE id=?`, state, first.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = lb.send(t, "aaaa1111", "bbbb", "immutable", func(r *busproto.SendRequest) { r.RetryOf = first.ID; r.Refs = []string{"archive://fixture"} }); code(err) != busproto.CodeBadRequest {
			t.Fatalf("accepted %s/unconfirmed: %v", state, err)
		}
	}
}
