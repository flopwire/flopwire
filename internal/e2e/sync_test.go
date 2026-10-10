package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// TestTwoDeviceSync is spec §11 B4: one server (Compose), two devices
// (simulated on this machine with separate homes, credentials, indexes and
// sync state), and the scenarios the two-laptop phase has to survive.
// Scenarios share state and run in order.
func TestTwoDeviceSync(t *testing.T) {
	h := newHarness(t)
	defer h.writeReport()
	h.setupIdentity("macbook-m1p", "macbook-m5p-sim")
	d1, d2 := h.devs[0], h.devs[1]
	var corpus string
	for _, d := range h.devs {
		d.seed()
		if n, _ := strconv.Atoi(os.Getenv("FLOPWIRE_E2E_CORPUS")); n > 0 && d == d1 {
			corpus = d.seedCorpus(t, n).String()
		}
		d.start()
	}
	t.Cleanup(func() {
		for _, d := range h.devs {
			d.stop(false)
			if t.Failed() {
				if raw, err := os.ReadFile(d.log); err == nil {
					lines := strings.Split(string(raw), "\n")
					t.Logf("--- %s agent log (tail) ---\n%s", d.name, strings.Join(lines[max(0, len(lines)-60):], "\n"))
				}
			}
		}
	})

	h.scenario("0-initial-sync", func(t *testing.T, r *result) {
		if corpus != "" {
			// Let the first sync of the sample finish, so the scenarios
			// measure steady state.
			r.Notes = append(r.Notes, corpus)
			start := time.Now()
			eventually(t, 5*time.Minute, 2*time.Second, "real-corpus sample synced", func() (bool, error) {
				_, problems, err := h.consistency(d1, nil)
				if err == nil && len(problems) > 0 {
					err = fmt.Errorf("%d mismatches, first: %s", len(problems), strings.Join(head(problems), "; "))
				}
				return err == nil, err
			})
			r.Timings["corpus_synced"] = ms(time.Since(start))
		}
		for _, d := range h.devs {
			h.checkInitialFixtures(t, d)
			el := h.waitSessionSynced(t, d, transcript.AgentClaude, d.claudePath, d.claudeSID, 60*time.Second)
			r.Timings[d.name+"_claude"] = ms(el)
			el = h.waitSessionSynced(t, d, transcript.AgentCodex, d.codexPath, d.codexSID, 60*time.Second)
			r.Timings[d.name+"_codex"] = ms(el)
			eventually(t, 60*time.Second, 250*time.Millisecond, d.name+" devin on server", func() (bool, error) {
				// Both devices hold the same Devin rows, and grep folds
				// identical texts into one hit: ask for this device's copy.
				hits, err := d.serverFind("login() never awaits fetch", "--agent", "devin", "--device", d.name)
				return len(inSession(hitsOn(hits, d), "devin-oracle-001")) == 1, err
			})
		}
	})

	var liveNeedle string
	h.scenario("a-live-append", func(t *testing.T, r *result) {
		var local, server []time.Duration
		for i := range 3 {
			needle := fmt.Sprintf("live append okra %d %d", i, time.Now().UnixNano())
			appendFile(t, d1.claudePath, d1.claudeUser(needle))
			start := time.Now()
			eventually(t, 5*time.Second, 10*time.Millisecond, "local "+needle, func() (bool, error) {
				live, _, err := d1.localLive(needle)
				return live == 1, err
			})
			local = append(local, time.Since(start))
			eventually(t, 30*time.Second, 100*time.Millisecond, "server "+needle, func() (bool, error) {
				hits, err := d1.serverFind(needle)
				return len(hitsOn(hits, d1)) == 1, err
			})
			server = append(server, time.Since(start))
			liveNeedle = needle
		}
		// A hook flush skips the debounce.
		needle := fmt.Sprintf("hook flushed okra %d", time.Now().UnixNano())
		appendFile(t, d1.claudePath, d1.claudeAssistant(needle))
		start := time.Now()
		hookIn, _ := json.Marshal(map[string]string{"transcript_path": d1.claudePath, "session_id": d1.claudeSID, "hook_event_name": "Stop"})
		if _, err := h.run(d1.env(), string(hookIn), "agent", "flush", "--socket", d1.sock); err != nil {
			t.Fatal(err)
		}
		hookLocal := time.Since(start)
		eventually(t, 30*time.Second, 50*time.Millisecond, "server "+needle, func() (bool, error) {
			hits, err := d1.serverFind(needle)
			return len(hitsOn(hits, d1)) == 1, err
		})
		hookServer := time.Since(start)
		r.Timings["local_max"], r.Timings["server_max"] = ms(maxDur(local)), ms(maxDur(server))
		r.Timings["local_all"], r.Timings["server_all"] = fmtDurs(local), fmtDurs(server)
		r.Timings["hook_local"], r.Timings["hook_server"] = ms(hookLocal), ms(hookServer)
		if maxDur(local) > 2*time.Second {
			t.Errorf("local index latency %s over 2s", maxDur(local))
		}
		if maxDur(server) > 10*time.Second {
			t.Errorf("server latency %s over 10s", maxDur(server))
		}
	})

	h.scenario("b-cross-device", func(t *testing.T, r *result) {
		if liveNeedle == "" {
			t.Skip("no needle from scenario a")
		}
		hits, err := d2.serverFind(liveNeedle)
		if err != nil {
			t.Fatal(err)
		}
		if got := hitsOn(hits, d1); len(got) != 1 {
			t.Fatalf("device 2 sees %d hits of device 1's line (all hits %+v)", len(got), hits)
		}
		// Widen the hit from device 2: context over device 1's rows.
		out, err := d2.cli("read", "--server", "--json", "--messages-before", "2", "--messages-after", "0", hits[0].MessageID)
		if err != nil {
			t.Fatal(err)
		}
		var cx format.Context
		if err := json.Unmarshal([]byte(out), &cx); err != nil || len(cx.Messages) < 2 {
			t.Fatalf("context from device 2: %v %s", err, out)
		}
		// Ranked search too, and the other direction.
		out, err = d1.cli("search", "--server", "--json", "codex growth session", h.nonce)
		if err != nil {
			t.Fatal(err)
		}
		var sr struct{ Hits []format.Hit }
		_ = json.Unmarshal([]byte(out), &sr)
		if len(hitsOn(sr.Hits, d2)) == 0 {
			t.Fatalf("device 1 cannot find device 2's codex session: %s", out)
		}
		// The local index holds the device's own data only.
		if live, _, _ := d2.localLive(liveNeedle); live != 0 {
			t.Fatalf("device 2's local index has device 1's line")
		}
		r.Notes = append(r.Notes, fmt.Sprintf("hit device=%s", hits[0].Device))
	})

	h.scenario("b-mcp-credential-rotation", func(t *testing.T, r *result) {
		if liveNeedle == "" {
			t.Fatal("no live fixture from scenario a")
		}
		h.checkMCPRotation(t, d1, liveNeedle, r)
	})

	h.scenario("c-server-down", func(t *testing.T, r *result) {
		if _, err := h.compose("stop", "flopwire"); err != nil {
			t.Fatal(err)
		}
		downAt := time.Now()
		restarted := false
		defer func() {
			if !restarted {
				_, _ = h.compose("start", "flopwire")
			}
		}()
		if _, err := d1.serverFind("anything"); err == nil {
			t.Fatal("server still answers after stop")
		}
		var localLat []time.Duration
		type needle struct {
			d    *device
			text string
		}
		var all []needle
		for round := range 4 {
			var batch []needle
			for _, d := range h.devs {
				a := fmt.Sprintf("offline claude %s round %d kiwi %s", d.name, round, h.nonce)
				b := fmt.Sprintf("offline codex %s round %d kumquat %s", d.name, round, h.nonce)
				appendFile(t, d.claudePath, d.claudeUser(a)+d.claudeAssistant("noted"))
				appendFile(t, d.codexPath, d.codexMsg("user", b)+d.codexMsg("assistant", "noted"))
				batch = append(batch, needle{d, a}, needle{d, b})
			}
			start := time.Now()
			for _, n := range batch {
				eventually(t, 5*time.Second, 20*time.Millisecond, "local while down: "+n.text, func() (bool, error) {
					live, _, err := n.d.localLive(n.text)
					return live == 1, err
				})
			}
			localLat = append(localLat, time.Since(start))
			all = append(all, batch...)
			time.Sleep(3 * time.Second)
		}
		// Devin keeps writing too: a new node on the main chain.
		devinNeedle := "offline devin node pomegranate " + h.nonce
		cm, _ := json.Marshal(map[string]any{"message_id": "dm-e2e-317", "role": "user", "content": devinNeedle, "metadata": map[string]any{"is_user_input": true}})
		d2.devinExec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES ('devin-oracle-003', 17, 16, ?, ?)`, string(cm), time.Now().Unix())
		d2.devinExec(`UPDATE sessions SET main_chain_id = 17, last_activity_at = ? WHERE id = 'devin-oracle-003'`, time.Now().Unix())
		eventually(t, 5*time.Second, 20*time.Millisecond, "local devin while down", func() (bool, error) {
			live, _, err := d2.localLive(devinNeedle)
			return live == 1, err
		})
		for _, d := range h.devs {
			if !d.running() {
				t.Fatalf("%s agent died while the server was down", d.name)
			}
		}
		r.Timings["outage"] = ms(time.Since(downAt))
		r.Timings["local_while_down_max"] = ms(maxDur(localLat))

		if _, err := h.compose("start", "flopwire"); err != nil {
			t.Fatal(err)
		}
		restarted = true
		upAt := time.Now()
		eventually(t, 90*time.Second, 250*time.Millisecond, "server ready", func() (bool, error) { return h.ready(h.cfg.server), nil })
		r.Timings["server_ready"] = ms(time.Since(upAt))
		for _, d := range h.devs {
			h.waitSessionSynced(t, d, transcript.AgentClaude, d.claudePath, d.claudeSID, 3*time.Minute)
			h.waitSessionSynced(t, d, transcript.AgentCodex, d.codexPath, d.codexSID, 3*time.Minute)
		}
		r.Timings["catch_up"] = ms(time.Since(upAt))
		for _, n := range all {
			hits, err := n.d.serverFind(n.text)
			if err != nil {
				t.Fatal(err)
			}
			if got := hitsOn(hits, n.d); len(got) != 1 {
				t.Errorf("%q: %d server hits on %s", n.text, len(got), n.d.name)
			}
		}
		eventually(t, 60*time.Second, 250*time.Millisecond, "devin node on server", func() (bool, error) {
			hits, err := d2.serverFind(devinNeedle)
			return len(hitsOn(hits, d2)) == 1, err
		})
		r.Timings["catch_up_with_devin"] = ms(time.Since(upAt))
		// Local indexes match the files too (no duplicates from the outage).
		for _, d := range h.devs {
			h.checkLocal(t, d, transcript.AgentClaude, d.claudePath, d.claudeSID)
			h.checkLocal(t, d, transcript.AgentCodex, d.codexPath, d.codexSID)
		}
	})

	h.scenario("d-kill-mid-flush", func(t *testing.T, r *result) {
		sid := "e2e00002-0000-4000-8000-00000000000b"
		dir := filepath.Join(d2.home, ".claude", "projects", "-tmp-e2e-big")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, sid+".jsonl")
		saved := d2.claudeSID
		d2.claudeSID, d2.claudeLast = sid, ""
		var b strings.Builder
		// Tool calls with large log-like outputs, the shape that makes real
		// transcripts big.
		for i := 0; b.Len() < 72<<20; i++ {
			id := fmt.Sprintf("toolu_bulk_%d", i)
			b.WriteString(d2.claudeLine(`"type":"assistant","message":{"id":"msg_bulk","type":"message","role":"assistant","model":"claude-e2e","content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"make test"}}]}`))
			b.WriteString(d2.claudeLine(`"type":"user","message":{"role":"user","content":[{"tool_use_id":"` + id + `","type":"tool_result","content":` + jsonStr(logText(i, 96<<10)) + `}]}`))
		}
		d2.claudeSID = saved
		size := int64(b.Len())
		d2.stop(false)
		gate, err := newCrashProxy(h.cfg.server, h.cfg.pin, p, size)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			gate.unblock()
			d2.stop(false)
			gate.close()
			if err := setDeviceEndpoint(d2.cfg, h.cfg.server, h.cfg.pin); err != nil {
				t.Error(err)
				return
			}
			d2.start()
		}()
		if err := setDeviceEndpoint(d2.cfg, gate.server.URL, gate.fingerprint()); err != nil {
			t.Fatal(err)
		}
		writeFile(t, p, b.String())
		start := time.Now()
		d2.start()
		eventually(t, 10*time.Second, 20*time.Millisecond, "restarted agent socket", func() (bool, error) {
			c, err := net.DialTimeout("unix", d2.sock, time.Second)
			if err != nil {
				return false, err
			}
			c.Close()
			return true, nil
		})
		flushed := make(chan error, 1)
		go func() {
			_, err := h.run(d2.env(), "", "agent", "flush", "--socket", d2.sock, "--timeout", "60s", "--path", p)
			flushed <- err
		}()
		flushedJoined := false
		defer func() {
			if flushedJoined {
				return
			}
			gate.unblock()
			d2.stop(true)
			select {
			case <-flushed:
			case <-time.After(65 * time.Second):
				t.Error("hook flush did not exit during cleanup")
			}
		}()
		var boundary crashBoundary
		select {
		case boundary = <-gate.reached:
			if boundary.Err != nil {
				t.Fatal(boundary.Err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("agent did not reach the committed, unacknowledged flush boundary")
		}
		ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
		defer cancel()
		var committedEntries, committedOffset int64
		if err := h.pg.QueryRow(ctx, `SELECT count(*), COALESCE(max(e.byte_offset+c.size),0)
			FROM manifest_entries e JOIN chunks c ON c.hash=e.chunk_hash JOIN sources s ON s.id=e.source_id
			WHERE s.device_id=$1 AND s.path=$2 AND e.generation=$3`, d2.id, p, boundary.Header.Generation).Scan(&committedEntries, &committedOffset); err != nil {
			t.Fatal(err)
		}
		if committedEntries != boundary.Response.AckedEntries || committedOffset != boundary.Response.AckedOffset {
			t.Fatalf("server commit differs from held response: entries=%d offset=%d response=%+v", committedEntries, committedOffset, boundary.Response)
		}
		db, err := sql.Open("sqlite", "file:"+d2.index+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		var localAck int64
		err = db.QueryRowContext(ctx, `SELECT g.acked FROM devsync_gens g JOIN devsync_sources s ON s.id=g.source_id WHERE s.path=? AND g.generation=?`, p, boundary.Header.Generation).Scan(&localAck)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if localAck >= committedEntries {
			t.Fatalf("agent already acknowledged held response: local=%d server=%d", localAck, committedEntries)
		}
		d2.stop(true)
		r.Timings["killed_after"] = ms(time.Since(start))
		gate.unblock()
		select {
		case err := <-flushed:
			flushedJoined = true
			if err != nil {
				r.Notes = append(r.Notes, "hook call interrupted by crash")
			}
		case <-time.After(65 * time.Second):
			t.Fatal("hook flush did not exit after the crash")
		}
		r.Notes = append(r.Notes, fmt.Sprintf("server committed %d/%d bytes; durable client acknowledged %d/%d entries before SIGKILL", committedOffset, size, localAck, committedEntries))
		restart := time.Now()
		d2.start()
		h.waitSessionSynced(t, d2, transcript.AgentClaude, p, sid, 3*time.Minute)
		// Server visibility alone does not prove the restarted agent persisted
		// its acknowledgement. tail_acked includes absence of a tail, so it
		// must also be true when an idle source has sealed its final chunk.
		stateDB, err := sql.Open("sqlite", "file:"+d2.index+"?mode=ro&_pragma=busy_timeout(1000)")
		if err != nil {
			t.Fatal(err)
		}
		defer stateDB.Close()
		eventually(t, 30*time.Second, 100*time.Millisecond, "durable client acknowledgement after restart", func() (bool, error) {
			queryCtx, queryCancel := context.WithTimeout(h.ctx, 5*time.Second)
			defer queryCancel()
			var currentGen, capturedSize, entries, acked, tailOffset, tailSize, ackedBytes int64
			var tailAcked, lost bool
			err := stateDB.QueryRowContext(queryCtx, `SELECT s.generation,g.size,g.entries,g.acked,g.tail_acked,g.lost,g.tail_offset,g.tail_size,
				COALESCE((SELECT sum(m.size) FROM devsync_manifest m WHERE m.source_id=g.source_id AND m.generation=g.generation AND m.ordinal<g.acked),0)
				FROM devsync_gens g JOIN devsync_sources s ON s.id=g.source_id WHERE s.path=? AND g.generation=?`, p, boundary.Header.Generation).
				Scan(&currentGen, &capturedSize, &entries, &acked, &tailAcked, &lost, &tailOffset, &tailSize, &ackedBytes)
			if err != nil {
				return false, err
			}
			if currentGen != boundary.Header.Generation || capturedSize != size || acked != entries || !tailAcked || lost || ackedBytes != tailOffset || ackedBytes+tailSize != size {
				return false, fmt.Errorf("durable state: generation=%d original=%d size=%d expected=%d acked=%d entries=%d tail_acked=%v lost=%v manifest_bytes=%d tail_offset=%d tail_size=%d", currentGen, boundary.Header.Generation, capturedSize, size, acked, entries, tailAcked, lost, ackedBytes, tailOffset, tailSize)
			}
			return true, nil
		})
		r.Timings["resync_after_restart"] = ms(time.Since(restart))
		h.checkRaw(t, d2, p)
		h.checkLocal(t, d2, transcript.AgentClaude, p, sid)
		identity, err := transcript.StatIdentity(p)
		if err != nil {
			t.Fatal(err)
		}
		h.checkFixtureProvenance(t, d2, "claude", sid, p, identity.ID.String(), 0)
		var gens int
		var gen int64
		if err := h.pg.QueryRow(h.ctx, `SELECT count(*),min(g.generation) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.device_id=$1 AND s.path=$2`, d2.id, p).Scan(&gens, &gen); err != nil {
			t.Fatal(err)
		}
		if gens != 1 || gen != boundary.Header.Generation {
			t.Fatalf("restart replaced generation: count=%d generation=%d original=%d", gens, gen, boundary.Header.Generation)
		}
		r.Notes = append(r.Notes, fmt.Sprintf("original generation %d retained after restart", gen))
	})

	h.scenario("e-claude-rewrite", func(t *testing.T, r *result) {
		nonce := time.Now().UnixNano()
		pre := fmt.Sprintf("pre-compaction marmoset %d", nonce)
		post := fmt.Sprintf("post-compaction narwhal %d", nonce)
		appendFile(t, d1.claudePath, d1.claudeUser(pre)+d1.claudeAssistant("noted before"))
		boundary := d1.claudeLine(`"type":"system","subtype":"compact_boundary","content":"Conversation compacted","isMeta":false,"level":"info","compactMetadata":{"trigger":"auto","preTokens":90000}`)
		tail := boundary + d1.claudeUser("Summary of the earlier conversation.") + d1.claudeUser(post) + d1.claudeAssistant("noted after")
		appendFile(t, d1.claudePath, tail)
		h.waitSessionSynced(t, d1, transcript.AgentClaude, d1.claudePath, d1.claudeSID, time.Minute)
		old, _ := transcript.StatIdentity(d1.claudePath)

		// Rewrite without the pre-boundary history, as a new inode.
		tmp := d1.claudePath + ".tmp"
		writeFile(t, tmp, tail)
		if err := os.Rename(tmp, d1.claudePath); err != nil {
			t.Fatal(err)
		}
		cur, _ := transcript.StatIdentity(d1.claudePath)
		if cur.ID == old.ID {
			t.Fatal("rewrite kept the inode")
		}
		start := time.Now()
		h.waitSessionSynced(t, d1, transcript.AgentClaude, d1.claudePath, d1.claudeSID, time.Minute)
		r.Timings["server_converged"] = ms(time.Since(start))
		eventually(t, 30*time.Second, 200*time.Millisecond, "pre-boundary rows superseded on the server", func() (bool, error) {
			hits, err := d1.serverFind(pre)
			return len(hitsOn(hits, d1)) == 0, err
		})
		hits, err := d1.serverFind(pre, "--include-superseded")
		if err != nil {
			t.Fatal(err)
		}
		if got := hitsOn(hits, d1); len(got) < 1 || !allSuperseded(got) {
			t.Fatalf("include_superseded: %d hits, superseded=%v", len(got), allSuperseded(got))
		}
		if hits, _ := d1.serverFind(post); len(hitsOn(hits, d1)) != 1 {
			t.Fatalf("post-boundary line: %d live hits", len(hitsOn(hits, d1)))
		}
		h.checkFixtureProvenance(t, d1, "claude", d1.claudeSID, d1.claudePath, cur.ID.String(), 1)
		var gens, srcs int
		_ = h.pg.QueryRow(h.ctx, `SELECT count(*), count(DISTINCT s.id) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.device_id=$1 AND s.path=$2`,
			d1.id, d1.claudePath).Scan(&gens, &srcs)
		r.Notes = append(r.Notes, fmt.Sprintf("server: %d generation(s) over %d source row(s) at the path", gens, srcs))
		if gens < 2 {
			t.Errorf("no new generation on the server")
		}
		live, sup, err := d1.localLive(pre)
		if err != nil || live != 0 || sup < 1 {
			t.Errorf("local pre-boundary rows: live %d superseded %d (%v)", live, sup, err)
		}
		if live, _, _ := d1.localLive(post); live != 1 {
			t.Errorf("local post-boundary line: %d live rows", live)
		}
	})

	h.scenario("f-devin-session-deleted", func(t *testing.T, r *result) {
		const needle, session = "There are three migrations.", "devin-oracle-002"
		d1.devinExec(`DELETE FROM message_nodes WHERE session_id = ?`, session)
		d1.devinExec(`DELETE FROM sessions WHERE id = ?`, session)
		start := time.Now()
		eventually(t, 10*time.Second, 50*time.Millisecond, "local rows superseded", func() (bool, error) {
			live, sup, err := d1.localLiveIn(needle, session)
			return live == 0 && sup >= 1, err
		})
		r.Timings["local"] = ms(time.Since(start))
		// Both devices hold the same session, and grep folds identical
		// texts into one hit: ask for each device's copy on its own.
		find := func(d *device, extra ...string) ([]format.Hit, error) {
			hits, err := d1.serverFind(needle, append([]string{"--agent", "devin", "--device", d.name}, extra...)...)
			return inSession(hits, session), err
		}
		eventually(t, 60*time.Second, 250*time.Millisecond, "server rows superseded", func() (bool, error) {
			hits, err := find(d1)
			return len(hitsOn(hits, d1)) == 0, err
		})
		r.Timings["server"] = ms(time.Since(start))
		hits, err := find(d1, "--include-superseded")
		if err != nil {
			t.Fatal(err)
		}
		if got := hitsOn(hits, d1); len(got) != 1 || !allSuperseded(got) {
			t.Fatalf("include_superseded on device 1: %+v", got)
		}
		if hits, err = find(d2, "--include-superseded"); err != nil {
			t.Fatal(err)
		}
		if got := hitsOn(hits, d2); len(got) != 1 || got[0].Superseded {
			t.Fatalf("device 2's copy of the session changed: %+v", got)
		}
	})

	h.scenario("g-admin-delete", func(t *testing.T, r *result) {
		needle := d1.codexNeedle
		// Enough bytes for finalized chunks (not only a provisional tail).
		var bulk strings.Builder
		for i := 0; bulk.Len() < 3<<20; i++ {
			bulk.WriteString(d1.codexMsg("assistant", fmt.Sprintf("codex bulk %d\n%s", i, noise(64<<10))))
		}
		appendFile(t, d1.codexPath, bulk.String())
		h.waitSessionSynced(t, d1, transcript.AgentCodex, d1.codexPath, d1.codexSID, time.Minute)
		hits, err := d1.serverFind(needle)
		if err != nil || len(hitsOn(hits, d1)) != 1 {
			t.Fatalf("codex growth session on server: %v %+v", err, hits)
		}
		conv := hitsOn(hits, d1)[0].ConversationID
		// Chunks only this source references, and their objects.
		type obj struct {
			hash []byte
			key  string
		}
		var own []obj
		rows, err := h.pg.Query(h.ctx, `SELECT DISTINCT c.hash, c.object_key FROM chunks c JOIN manifest_entries e ON e.chunk_hash=c.hash JOIN sources s ON s.id=e.source_id
			WHERE s.device_id=$1 AND s.path=$2 AND NOT EXISTS (SELECT 1 FROM manifest_entries o JOIN sources os ON os.id=o.source_id WHERE o.chunk_hash=c.hash AND os.path<>$2)`, d1.id, d1.codexPath)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var o obj
			if err := rows.Scan(&o.hash, &o.key); err != nil {
				t.Fatal(err)
			}
			own = append(own, o)
		}
		rows.Close()
		var job struct{ Deletion struct{ ID, State string } }
		if code, err := h.adminJSON("DELETE", "/v1/admin/conversations/"+conv, &job); err != nil || code != 202 {
			t.Fatalf("delete: %d %v", code, err)
		}
		start := time.Now()
		for _, flt := range [][]string{nil, {"--include-superseded"}} {
			if hits, _ := d1.serverFind(needle, flt...); len(hitsOn(hits, d1)) != 0 {
				t.Fatalf("deleted conversation still found (%v): %+v", flt, hits)
			}
		}
		eventually(t, 90*time.Second, 500*time.Millisecond, "deletion job complete", func() (bool, error) {
			var st struct {
				Deletion struct{ State, LastError string }
			}
			_, err := h.adminJSON("GET", "/v1/admin/deletions/"+job.Deletion.ID, &st)
			if st.Deletion.State == "failed" {
				return false, fmt.Errorf("deletion failed: %s", st.Deletion.LastError)
			}
			return st.Deletion.State == "complete", err
		})
		r.Timings["job_complete"] = ms(time.Since(start))
		if len(own) == 0 {
			t.Fatal("the conversation had no chunks of its own")
		}
		eventually(t, 3*time.Minute, time.Second, "own chunks purged", func() (bool, error) {
			for _, o := range own {
				var n int
				if err := h.pg.QueryRow(h.ctx, `SELECT count(*) FROM chunks WHERE hash=$1`, o.hash).Scan(&n); err != nil || n > 0 {
					return false, err
				}
				if _, err := h.s3.StatObject(h.ctx, h.cfg.bucket, o.key, minio.StatObjectOptions{}); err == nil {
					return false, fmt.Errorf("object %s still stored", o.key)
				}
			}
			return true, nil
		})
		r.Timings["chunks_purged"] = ms(time.Since(start))
		r.Notes = append(r.Notes, fmt.Sprintf("%d own chunks purged", len(own)))
		var tails, msgs int
		_ = h.pg.QueryRow(h.ctx, `SELECT count(*) FROM provisional_tails t JOIN sources s ON s.id=t.source_id WHERE s.device_id=$1 AND s.path=$2`, d1.id, d1.codexPath).Scan(&tails)
		_ = h.pg.QueryRow(h.ctx, `SELECT count(*) FROM messages WHERE conversation_id=$1`, conv).Scan(&msgs)
		r.Notes = append(r.Notes, fmt.Sprintf("after purge: %d provisional tails, %d message rows kept as tombstones", tails, msgs))
		if tails != 0 {
			t.Errorf("provisional tail of the deleted source survived")
		}

		// The device keeps writing: an append and a copy under a new path
		// (same session id) must not resurrect the conversation.
		again := "resurrection attempt " + h.nonce
		appendFile(t, d1.codexPath, d1.codexMsg("user", again))
		cp := strings.Replace(d1.codexPath, "T10-00-00", "T11-00-00", 1)
		raw, _ := os.ReadFile(d1.codexPath)
		writeFile(t, cp, string(raw))
		for _, p := range []string{d1.codexPath, cp} {
			if _, err := h.run(d1.env(), "", "agent", "flush", "--socket", d1.sock, "--path", p); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, 30*time.Second, 250*time.Millisecond, "server saw the copy", func() (bool, error) {
			var n int
			err := h.pg.QueryRow(h.ctx, `SELECT count(*) FROM sources WHERE device_id=$1 AND path=$2`, d1.id, cp).Scan(&n)
			return n > 0, err
		})
		time.Sleep(5 * time.Second) // let the parse queue run
		for _, n := range []string{needle, again} {
			for _, flt := range [][]string{nil, {"--include-superseded"}} {
				if hits, _ := d1.serverFind(n, flt...); len(hitsOn(hits, d1)) != 0 {
					t.Errorf("re-upload resurrected %q (%v): %+v", n, flt, hitsOn(hits, d1))
				}
			}
		}
		// Device 2's own codex session is untouched.
		if hits, _ := d2.serverFind(d2.codexNeedle); len(hitsOn(hits, d2)) != 1 {
			t.Errorf("device 2's codex session disappeared")
		}
	})

	h.scenario("h-companions-raw", func(t *testing.T, r *result) {
		nonce := time.Now().UnixNano()
		sessDir := filepath.Join(filepath.Dir(d1.claudePath), d1.claudeSID)
		trDir := filepath.Join(sessDir, "tool-results")
		subDir := filepath.Join(sessDir, "subagents")
		for _, p := range []string{trDir, subDir} {
			if err := os.MkdirAll(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		headMark := fmt.Sprintf("e2e companion head marker %d", nonce)
		midMark := fmt.Sprintf("deep middle needle %d", nonce)
		secret := "ghp_" + strings.Repeat("aB3dE5", 6) // synthetic token; never a credential
		big := headMark + "\n" + secret + "\n" + noise(150<<10) + midMark + "\n" + noise(150<<10) + "e2e companion tail marker\n"
		bigPath := filepath.Join(trDir, "e2ebig01.txt")
		writeFile(t, bigPath, big)
		preview := fmt.Sprintf("<persisted-output>\nOutput too large (%dKB). Full output saved to: %s\n\nPreview (first 2KB):\npreview only text\n</persisted-output>", len(big)>>10, bigPath)
		call := d1.claudeLine(`"type":"assistant","message":{"id":"msg_e2e_tool","type":"message","role":"assistant","model":"claude-e2e","content":[{"type":"tool_use","id":"toolu_e2e_big","name":"Bash","input":{"command":"cat big.log"}}]}`)
		res := d1.claudeLine(`"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_e2e_big","type":"tool_result","content":` + jsonStr(preview) + `}]}`)
		// A subagent with its meta.json sidecar.
		subPath := filepath.Join(subDir, "agent-e2e0001.jsonl")
		metaPath := filepath.Join(subDir, "agent-e2e0001.meta.json")
		writeFile(t, metaPath, `{"agentType":"Explore","description":"e2e subagent","toolUseId":"toolu_e2e_sub","spawnDepth":1,"parentAgentId":null}`)
		subText := fmt.Sprintf("subagent says hello %d", nonce)
		writeFile(t, subPath, `{"parentUuid":null,"isSidechain":true,"userType":"external","cwd":"/tmp/e2e-growth","sessionId":"`+d1.claudeSID+
			`","version":"2.1.0","agentId":"e2e0001","type":"user","message":{"role":"user","content":`+jsonStr(subText)+`},"uuid":"`+d1.nextUUID()+`","timestamp":"`+now()+`"}`+"\n")
		appendFile(t, d1.claudePath, call+res)
		start := time.Now()
		eventually(t, 60*time.Second, 250*time.Millisecond, "tool result carries the companion text", func() (bool, error) {
			hits, err := d1.serverFind(headMark)
			return len(hitsOn(hits, d1)) == 1, err
		})
		r.Timings["companion_text_on_server"] = ms(time.Since(start))
		hit := hitsOn(must(d1.serverFind(headMark)), d1)[0]
		if hit.Kind != "tool_result" {
			t.Errorf("head marker hit kind %s", hit.Kind)
		}
		out, err := d1.cli("read", "--server", "--json", hit.MessageID)
		if err != nil {
			t.Fatal(err)
		}
		var cx format.Context
		if err := json.Unmarshal([]byte(out), &cx); err != nil || len(cx.Messages) != 1 {
			t.Fatalf("context: %v %s", err, out)
		}
		m := cx.Messages[0]
		r.Notes = append(r.Notes, fmt.Sprintf("row text %dB of %dB", len(m.Text), m.TextLen))
		if m.TextLen < len(big) || len(m.Text) >= len(big) || strings.Contains(m.Text, midMark) {
			t.Errorf("tool result row: text %dB, text_len %d, want capped text of a %dB output", len(m.Text), m.TextLen, len(big))
		}
		// raw() on the companion returns the whole output.
		eventually(t, 30*time.Second, 250*time.Millisecond, "companion archived", func() (bool, error) {
			_, _, size, err := h.latestGen(d1.id, bigPath)
			return size == int64(len(big)), err
		})
		id, gen, size, err := h.latestGen(d1.id, bigPath)
		if err != nil {
			t.Fatal(err)
		}
		got, err := d1.cli("raw", "--server", id, fmt.Sprint(gen), "0", fmt.Sprint(size))
		if err != nil {
			t.Fatal(err)
		}
		wantRaw := make([]byte, len(big))
		rr := redact.NewReaderAt(strings.NewReader(big), redact.ModeFor(string(transcript.StorageCompanion), bigPath))
		if n, err := rr.ReadAt(wantRaw, 0); n != len(wantRaw) || err != nil {
			t.Fatalf("redact companion fixture: n=%d err=%v", n, err)
		}
		if strings.Contains(got, secret) || strings.Contains(string(wantRaw), secret) {
			t.Fatal("synthetic secret survived companion redaction")
		}
		if got != string(wantRaw) {
			t.Fatalf("raw companion: %d bytes, want %d identical", len(got), len(big))
		}
		// meta.json reached the server and linked the subagent.
		eventually(t, 30*time.Second, 250*time.Millisecond, "subagent linked with its meta.json", func() (bool, error) {
			var kind string
			var parent *string
			if err := h.pg.QueryRow(h.ctx, `SELECT m.storage_kind, p.path FROM sources m LEFT JOIN sources p ON p.id=m.parent_source_id
				WHERE m.device_id=$1 AND m.path=$2`, d1.id, metaPath).Scan(&kind, &parent); err != nil {
				return false, err
			}
			var linked bool
			var extra string
			if err := h.pg.QueryRow(h.ctx, `SELECT c.parent_conversation_id IS NOT NULL, c.extra::text FROM conversations c JOIN sources s ON s.id=c.source_id
				WHERE s.device_id=$1 AND s.path=$2`, d1.id, subPath).Scan(&linked, &extra); err != nil {
				return false, err
			}
			if kind != "companion" || parent == nil || *parent != subPath || !linked || !strings.Contains(extra, "Explore") {
				return false, fmt.Errorf("meta kind %s parent %v, subagent linked %v extra %s", kind, parent, linked, extra)
			}
			return true, nil
		})
		// The local index has the subagent with its meta.json too.
		eventually(t, 10*time.Second, 100*time.Millisecond, "local subagent carries its meta.json", func() (bool, error) {
			extra, err := d1.localConversationExtra(transcript.AgentClaude, "agent-e2e0001")
			return strings.Contains(extra, "Explore"), err
		})
		// The oracle's orphaned tool-results file (its transcript is gone)
		// is archived too.
		orphan := filepath.Join(d1.home, ".claude", "projects", "-tmp-oracle-beta", "0b7e2c1a-0000-4000-8000-00000000000f", "tool-results", "orphan01.txt")
		if _, _, size, err := h.latestGen(d1.id, orphan); err != nil || size == 0 {
			t.Errorf("orphaned tool-results file not archived: %v", err)
		}
	})

	h.scenario("i-backup-restore", func(t *testing.T, r *result) {
		h.backupRestore(t, r, d2, liveNeedle)
	})

	// Every transcript on both devices, after all of the above: the server's
	// live rows and the local index both equal a fresh parse of the files.
	// The conversation deleted in g stays gone.
	h.scenario("z-final-consistency", func(t *testing.T, r *result) {
		deleted := map[string]bool{d1.codexSID: true}
		start := time.Now()
		var notes []string
		first := true
		eventually(t, 5*time.Minute, 2*time.Second, "every transcript on both devices matches the server and the local index", func() (bool, error) {
			defer func() { first = false }()
			notes = notes[:0]
			var problems []string
			for _, d := range h.devs {
				n, p, err := h.consistency(d, deleted)
				if err != nil {
					return false, err
				}
				notes = append(notes, fmt.Sprintf("%s: %s", d.name, n))
				problems = append(problems, p...)
			}
			if len(problems) > 0 {
				err := fmt.Errorf("%d mismatches, first: %s", len(problems), strings.Join(head(problems), "; "))
				if first {
					t.Logf("first check: %v", err)
				}
				return false, err
			}
			return true, nil
		})
		r.Notes = append(r.Notes, notes...)
		for _, d := range h.devs {
			r.Notes = append(r.Notes, fmt.Sprintf("%s agent peak RSS %d MB", d.name, d.peakRSS()>>20))
		}
		r.Timings["converged_after"] = ms(time.Since(start))
		var n int
		_ = h.pg.QueryRow(h.ctx, `SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id
			WHERE c.device_id=$1 AND c.session_id=$2`, d1.id, d1.codexSID).Scan(&n)
		if n != 0 {
			t.Errorf("deleted conversation has %d rows on the server", n)
		}
	})
}

// consistency compares, for every transcript on the device, a fresh parse
// with the server's live rows and with the local index; and for every
// Devin session, the server with the local index. It returns a summary and
// the mismatches.
func (h *harness) consistency(d *device, skip map[string]bool) (string, []string, error) {
	files, err := d.transcripts()
	if err != nil {
		return "", nil, err
	}
	var problems []string
	sessions, rows := 0, 0
	for path, agent := range files {
		all, err := parseAll(path, agent)
		if err != nil {
			return "", nil, err
		}
		content, err := h.expectedContent(path, agent, "")
		if err != nil {
			return "", nil, err
		}
		for session, want := range all {
			if skip[session] && d == h.devs[0] {
				continue
			}
			sessions++
			rows += len(want)
			got, err := h.serverKeys(d.id, string(agent), session)
			if err != nil {
				return "", nil, err
			}
			if diff := diffKeys(want, got); diff != "" {
				problems = append(problems, fmt.Sprintf("server %s %s: %s", filepath.Base(path), session, diff))
			}
			stored, err := h.serverContent(d.id, string(agent), session)
			if err != nil {
				return "", nil, err
			}
			if diff := contentDiff(content[session], stored); diff != "" {
				problems = append(problems, fmt.Sprintf("server %s %s: %s", filepath.Base(path), session, diff))
			}
			local, err := d.localKeys(string(agent), session)
			if err != nil {
				return "", nil, err
			}
			if diff := diffKeys(want, local); diff != "" {
				problems = append(problems, fmt.Sprintf("local %s %s: %s", filepath.Base(path), session, diff))
			}
		}
	}
	devinSessions, err := devin.ListSessions(h.ctx, d.devinDB())
	if err != nil {
		return "", nil, err
	}
	placed, err := d.devinPlaced()
	if err != nil {
		return "", nil, err
	}
	devinRows := 0
	for _, sid := range devinSessions {
		local, err := d.localKeys(string(transcript.AgentDevin), sid)
		if err != nil {
			return "", nil, err
		}
		got, err := h.serverKeys(d.id, string(transcript.AgentDevin), sid)
		if err != nil {
			return "", nil, err
		}
		devinRows += len(local)
		want := local
		if !placed[sid] {
			// No sessions row, so no directory: the unplaceable setting
			// (default local) keeps it off the server.
			want = map[string]int{}
		}
		if diff := diffKeys(want, got); diff != "" {
			problems = append(problems, fmt.Sprintf("devin %s server vs local: %s", sid, diff))
		}
		content, err := h.expectedContent(d.devinDB(), transcript.AgentDevin, sid)
		if err != nil {
			return "", nil, err
		}
		stored, err := h.serverContent(d.id, string(transcript.AgentDevin), sid)
		if err != nil {
			return "", nil, err
		}
		expected := content[sid]
		if !placed[sid] {
			expected = nil
		}
		if diff := contentDiff(expected, stored); diff != "" {
			problems = append(problems, fmt.Sprintf("devin %s: %s", sid, diff))
		}
	}
	return fmt.Sprintf("%d files, %d sessions, %d rows; %d Devin sessions, %d rows", len(files), sessions, rows, len(devinSessions), devinRows), problems, nil
}

// waitSessionSynced waits until the server's live rows for a session match
// a local parse of the file exactly: no gaps, no duplicates.
func (h *harness) waitSessionSynced(t *testing.T, d *device, agent transcript.Agent, path, session string, limit time.Duration) time.Duration {
	t.Helper()
	return eventually(t, limit, 250*time.Millisecond, fmt.Sprintf("%s %s %s synced", d.name, agent, filepath.Base(path)), func() (bool, error) {
		want, err := parseKeys(path, agent, session)
		if err != nil {
			return false, err
		}
		got, err := h.serverKeys(d.id, string(agent), session)
		if err != nil {
			return false, err
		}
		if diff := diffKeys(want, got); diff != "" {
			return false, fmt.Errorf("server vs file: %s", diff)
		}
		return true, nil
	})
}

// checkLocal asserts the local index holds exactly the file's rows.
func (h *harness) checkLocal(t *testing.T, d *device, agent transcript.Agent, path, session string) {
	t.Helper()
	eventually(t, 10*time.Second, 100*time.Millisecond, d.name+" local index matches "+filepath.Base(path), func() (bool, error) {
		want, err := parseKeys(path, agent, session)
		if err != nil {
			return false, err
		}
		got, err := d.localKeys(string(agent), session)
		if err != nil {
			return false, err
		}
		if diff := diffKeys(want, got); diff != "" {
			return false, fmt.Errorf("local vs file: %s", diff)
		}
		return true, nil
	})
}

// checkRaw asserts raw() of the whole current generation equals the file.
func (h *harness) checkRaw(t *testing.T, d *device, path string) {
	t.Helper()
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	var gen, size int64
	eventually(t, time.Minute, 250*time.Millisecond, "server generation covers the file", func() (bool, error) {
		var err error
		id, gen, size, err = h.latestGen(d.id, path)
		return size == int64(len(want)), err
	})
	got, err := d.rawAll(id, gen, size)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("raw: %d bytes (sha %x), file %d bytes (sha %x)", len(got), sha256.Sum256([]byte(got)), len(want), sha256.Sum256(want))
	}
}

// rawAll reads a whole generation through `flopwire raw --server`, in
// reads of at most MaxRaw (16MB).
func (d *device) rawAll(id string, gen, size int64) ([]byte, error) {
	var out []byte
	for off := int64(0); off < size; off += 16 << 20 {
		part, err := d.cli("raw", "--server", id, fmt.Sprint(gen), fmt.Sprint(off), fmt.Sprint(min(16<<20, size-off)))
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

// latestGen returns the newest source row at path and its newest
// generation's size.
func (h *harness) latestGen(deviceID, path string) (id string, gen, size int64, err error) {
	err = h.pg.QueryRow(h.ctx, `SELECT s.id::text, g.generation, g.size FROM sources s JOIN generations g ON g.source_id=s.id
		WHERE s.device_id=$1 AND s.path=$2 ORDER BY s.first_seen_at DESC, g.generation DESC LIMIT 1`, deviceID, path).Scan(&id, &gen, &size)
	return id, gen, size, err
}

func allSuperseded(hits []format.Hit) bool {
	for _, h := range hits {
		if !h.Superseded {
			return false
		}
	}
	return len(hits) > 0
}

func maxDur(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		m = max(m, d)
	}
	return m
}

func fmtDurs(ds []time.Duration) string {
	s := make([]string, len(ds))
	for i, d := range ds {
		s[i] = ms(d)
	}
	sort.Strings(s)
	return strings.Join(s, "/")
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
