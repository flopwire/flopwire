package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/google/uuid"
)

// setRules stores admin path rules and an unplaceable floor as an
// administrator's policy update does (rules_version moves on).
func (e *env) setRules(unplaceable string, rules ...string) {
	e.t.Helper()
	var cred string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM credentials WHERE user_id=$1`, e.userID).Scan(&cred); err != nil {
		e.t.Fatal(err)
	}
	if rules == nil {
		rules = []string{}
	}
	if err := e.store.UpdatePolicyWithAudit(e.ctx, cred, domain.Policy{PathRules: rules, Unplaceable: unplaceable, UpdatedBy: e.userID},
		domain.AuditEvent{ID: uuid.NewString(), ActorID: e.userID, Action: "policy.update", TargetType: "policy", CreatedAt: time.Now().UTC()}); err != nil {
		e.t.Fatal(err)
	}
}

// claudeAt writes a one-exchange Claude transcript under home's project
// folder for folder, recording cwd (none when ""), and returns its spec.
func claudeAt(t *testing.T, home, folder, sid, cwd string) devicesync.SourceSpec {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwdField := ""
	if cwd != "" {
		cwdField = fmt.Sprintf(`"cwd":%q,`, cwd)
	}
	var b strings.Builder
	for i, text := range []string{"secret prompt about the merger", "secret answer"} {
		role := map[int]string{0: "user", 1: "assistant"}[i]
		fmt.Fprintf(&b, `{"type":%q,%s"uuid":"%s-%d","sessionId":%q,"timestamp":"2026-09-20T00:00:0%dZ","message":{"role":%q,"content":%q}}`+"\n",
			role, cwdField, sid, i, sid, i, role, text)
	}
	p := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, SessionKey: sid, Parser: claude.ParserName}
}

// stored reports the conversation and message rows the server holds for a
// session.
func (e *env) stored(session string) (convs, msgs int) {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM conversations WHERE session_id=$1`, session),
		e.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.session_id=$1`, session)
}

// A session under a denied directory is never stored: no conversation, no
// messages, no raw evidence; its source is tombstoned with the rule, the
// refusal is audited with the user, device and rule, counted, and the
// device hears of it on its next upload. A "~" rule is the device user's
// home. An allowed session beside it is stored.
func TestDeniedCwdRefusedAtIngest(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	e.setRules("", "deny ~/clients/acme")
	e.drain()
	const denied, allowed = "0b7e2c1a-0000-4000-8000-00000000d001", "0b7e2c1a-0000-4000-8000-00000000d002"
	dsp := claudeAt(t, home, "-x-clients-acme-web", denied, filepath.Join(home, "clients", "Acme", "web"))
	asp := claudeAt(t, home, "-x-open", allowed, filepath.Join(home, "open"))
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, dsp)
	sync1(t, sy, asp)
	e.drain()

	if c, m := e.stored(denied); c != 0 || m != 0 {
		t.Fatalf("denied session stored: %d conversations, %d messages", c, m)
	}
	if c, m := e.stored(allowed); c != 1 || m == 0 {
		t.Fatalf("allowed session: %d conversations, %d messages", c, m)
	}
	var rule string
	if err := e.pool.QueryRow(e.ctx, `SELECT refused_rule FROM sources WHERE path=$1 AND tombstoned_at IS NOT NULL`, dsp.Path).Scan(&rule); err != nil {
		t.Fatalf("refused source not tombstoned: %v", err)
	}
	if rule != "~/clients/acme" {
		t.Errorf("refused_rule = %q", rule)
	}
	if n := e.count(`SELECT count(*) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.path=$1`, dsp.Path); n != 0 {
		t.Errorf("refused source kept %d generations", n)
	}
	if n := e.count(`SELECT count(*) FROM chunks c WHERE state='committed' AND NOT EXISTS (SELECT 1 FROM manifest_entries m WHERE m.chunk_hash=c.hash)`); n != 0 {
		t.Errorf("%d committed chunks without references", n)
	}
	var meta []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT metadata FROM audit_events WHERE action='source.refused' AND actor_id=$1 AND device_id=$2`, e.userID, e.deviceID).Scan(&meta); err != nil {
		t.Fatalf("no source.refused audit event for the user and device: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(meta, &m)
	if m["rule"] != "~/clients/acme" || m["path"] != dsp.Path || m["mode"] != "deny" {
		t.Errorf("audit metadata %v", m)
	}
	if n, err := e.queue.Refused(e.ctx); err != nil || n != 1 {
		t.Errorf("Refused = %d, %v", n, err)
	}

	// The device appends: discarded, and told why.
	appendFile(t, dsp.Path, `{"type":"user","cwd":"`+filepath.Join(home, "clients", "acme", "web")+`","uuid":"late","sessionId":"`+denied+`","timestamp":"2026-09-20T00:01:00Z","message":{"role":"user","content":"late"}}`+"\n")
	sync1(t, sy, dsp)
	e.drain()
	if c, m := e.stored(denied); c != 0 || m != 0 {
		t.Fatalf("append resurrected the denied session: %d, %d", c, m)
	}
	refused, total := sy.Refused()
	if total != 1 || len(refused) != 1 || refused[0].Path != dsp.Path || refused[0].Rule != "~/clients/acme" {
		t.Errorf("device refusals = %v (%d)", refused, total)
	}
}

// A local rule keeps a session off the server as a deny rule does.
func TestLocalRuleRefusedAtIngest(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	e.setRules("", "local /work/private/**")
	const sid = "0b7e2c1a-0000-4000-8000-00000000d101"
	sp := claudeAt(t, home, "-work-private-x", sid, "/work/private/x")
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sp)
	e.drain()
	if c, m := e.stored(sid); c != 0 || m != 0 {
		t.Fatalf("local session stored: %d, %d", c, m)
	}
}

// A repo: rule matches the remote a Codex session recorded.
func TestRepoRuleMatchesCodexRemote(t *testing.T) {
	e := newEnv(t)
	e.setRules("", "deny repo:github.com/acme/*")
	dir := filepath.Join(t.TempDir(), ".codex", codex.SessionsDir, "2026", "09", "15")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(id, url string) devicesync.SourceSpec {
		lines := []string{
			codexRec(0, "session_meta", `{"id":"`+id+`","cwd":"/work/app","source":"cli","git":{"repository_url":"`+url+`"}}`),
			codexRec(1, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}`),
			codexRec(2, "response_item", `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
		}
		p := filepath.Join(dir, "rollout-2026-09-15T10-00-00-"+id+".jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return devicesync.SourceSpec{Path: p, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name}
	}
	const deniedID, allowedID = "019f0000-0000-7000-8000-00000000e001", "019f0000-0000-7000-8000-00000000e002"
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, write(deniedID, "git@github.com:Acme/secret.git"))
	sync1(t, sy, write(allowedID, "https://github.com/other/app.git"))
	e.drain()
	if c, m := e.stored(deniedID); c != 0 || m != 0 {
		t.Fatalf("session in a denied repo stored: %d, %d", c, m)
	}
	if c, _ := e.stored(allowedID); c != 1 {
		t.Fatal("session in another repo not stored")
	}
	var rule string
	if err := e.pool.QueryRow(e.ctx, `SELECT metadata->>'rule' FROM audit_events WHERE action='source.refused'`).Scan(&rule); err != nil || rule != "repo:github.com/acme/*" {
		t.Errorf("audited rule %q, %v", rule, err)
	}
}

// A session with no placement follows the admin floor: local and exclude
// keep it off the server, upload and no floor store it. A Claude session
// that names no directory is placed by its project folder, as the agent
// places it, and is stored under every floor.
func TestUnplaceableFloor(t *testing.T) {
	for floor, want := range map[string]bool{"": true, "upload": true, "local": false, "exclude": false} {
		t.Run("floor="+floor, func(t *testing.T) {
			e := newEnv(t)
			e.setRules(floor)
			const loose, folder = "0b7e2c1a-0000-4000-8000-00000000f001", "0b7e2c1a-0000-4000-8000-00000000f002"
			// Not under a projects folder and no cwd: unplaceable.
			sp := claudeAt(t, t.TempDir(), "", loose, "")
			sp2 := claudeAt(t, t.TempDir(), "-work-app", folder, "")
			sy := e.syncer(devicesync.Config{SealAfter: -1})
			sync1(t, sy, sp)
			sync1(t, sy, sp2)
			e.drain()
			if c, m := e.stored(loose); (c == 1 && m > 0) != want {
				t.Errorf("unplaceable session: %d conversations, %d messages; want stored=%v", c, m, want)
			}
			if c, _ := e.stored(folder); c != 1 {
				t.Error("session placed by its project folder was refused")
			}
			if !want {
				var rule string
				if err := e.pool.QueryRow(e.ctx, `SELECT refused_rule FROM sources WHERE path=$1`, sp.Path).Scan(&rule); err != nil || rule != "unplaceable="+floor {
					t.Errorf("refused_rule %q, %v", rule, err)
				}
			}
		})
	}
}

// visible reports the conversations of a session retrieval shows (not
// hidden).
func (e *env) visible(session string) int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND hidden_at IS NULL`, session)
}

// enforce runs one EnforceRules pass.
func (e *env) enforce() Enforcement {
	e.t.Helper()
	n, err := e.queue.EnforceRules(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// A rule added after sessions are stored hides the ones it covers (not
// deleted): the hide records the rule, rules version, time and
// administrator, is audited, runs once per change, and the uncovered
// session stays visible. The device keeps appending to the hidden
// session: the append is stored hidden, not refused, so the session can
// be restored whole.
func TestRuleAddedLaterHidesStored(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	const secret, open = "0b7e2c1a-0000-4000-8000-00000000a001", "0b7e2c1a-0000-4000-8000-00000000a002"
	ssp := claudeAt(t, home, "-work-secret", secret, "/work/secret")
	osp := claudeAt(t, home, "-work-open", open, "/work/open")
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, ssp)
	sync1(t, sy, osp)
	e.drain()
	if n := e.enforce(); n != (Enforcement{}) {
		t.Fatalf("no rules: %+v", n)
	}

	e.setRules("", "/work/secret")
	if n := e.enforce(); n.Hidden != 1 || n.Purged != 0 {
		t.Fatalf("EnforceRules = %+v; want 1 hidden", n)
	}
	if c, m := e.stored(secret); c != 1 || m == 0 {
		t.Fatalf("hidden session deleted: %d, %d", c, m)
	}
	if e.visible(secret) != 0 || e.visible(open) != 1 {
		t.Fatalf("visible: covered %d, uncovered %d", e.visible(secret), e.visible(open))
	}
	var rule, by string
	var version int64
	if err := e.pool.QueryRow(e.ctx, `SELECT hidden_rule,hidden_rules_version,hidden_by::text FROM conversations WHERE session_id=$1 AND hidden_at IS NOT NULL`,
		secret).Scan(&rule, &version, &by); err != nil {
		t.Fatal(err)
	}
	if rule != "/work/secret" || version != mustRules(t, e).version || by != e.userID {
		t.Errorf("hide recorded rule %q version %d by %s", rule, version, by)
	}
	if n := e.count(`SELECT count(*) FROM conversation_tombstones WHERE session_id=$1`, secret); n != 0 {
		t.Errorf("hidden session tombstoned: %d", n)
	}
	var meta []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT metadata FROM audit_events WHERE action='conversation.hidden' AND actor_id=$1`, e.userID).Scan(&meta); err != nil {
		t.Fatalf("no conversation.hidden audit event: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(meta, &m)
	if m["rule"] != "/work/secret" || m["user_id"] != e.userID || m["device_id"] != e.deviceID || m["purge_after"] == nil {
		t.Errorf("audit metadata %v", m)
	}
	// Once per change.
	if n := e.enforce(); n != (Enforcement{}) {
		t.Fatalf("second sweep = %+v", n)
	}
	// The device keeps appending: stored hidden.
	_, before := e.stored(secret)
	appendFile(t, ssp.Path, `{"type":"user","cwd":"/work/secret","uuid":"late","sessionId":"`+secret+`","timestamp":"2026-09-20T00:01:00Z","message":{"role":"user","content":"late"}}`+"\n")
	sync1(t, sy, ssp)
	e.drain()
	if c, m := e.stored(secret); c != 1 || m <= before {
		t.Fatalf("append to a hidden session: %d conversations, %d messages (had %d)", c, m, before)
	}
	if e.visible(secret) != 0 {
		t.Fatal("append made the hidden session visible")
	}
	if _, total := sy.Refused(); total != 0 {
		t.Errorf("hidden session reported refused: %d", total)
	}
}

// A hidden session is restored (and audited) when its rule is removed, or
// edited so it no longer matches; a change that still covers it keeps it
// hidden under the new rule, and its window runs on.
func TestRuleRemovedRestoresHidden(t *testing.T) {
	e := newEnv(t)
	const sid = "0b7e2c1a-0000-4000-8000-00000000a101"
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), claudeAt(t, t.TempDir(), "-work-secret-app", sid, "/work/secret/app"))
	e.drain()
	e.setRules("", "/work/secret")
	e.enforce()
	var at time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT hidden_at FROM conversations WHERE session_id=$1`, sid).Scan(&at); err != nil {
		t.Fatal(err)
	}
	e.setRules("", "/work/**")
	if n := e.enforce(); n != (Enforcement{}) || e.visible(sid) != 0 {
		t.Fatalf("still covered: %+v, visible %d", n, e.visible(sid))
	}
	var rule string
	var at2 time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT hidden_rule,hidden_at FROM conversations WHERE session_id=$1`, sid).Scan(&rule, &at2); err != nil {
		t.Fatal(err)
	}
	if rule != "/work/**" || !at2.Equal(at) {
		t.Errorf("rule %q hidden_at %v (was %v)", rule, at2, at)
	}
	e.setRules("", "/work/secret-other") // edited: no longer matches
	if n := e.enforce(); n.Restored != 1 || e.visible(sid) != 1 {
		t.Fatalf("edited rule: %+v, visible %d", n, e.visible(sid))
	}
	if n := e.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND (hidden_rule IS NOT NULL OR hidden_root IS NOT NULL OR hidden_by IS NOT NULL)`, sid); n != 0 {
		t.Error("restore left hide fields")
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action='conversation.restored' AND target_id=(SELECT id::text FROM conversations WHERE session_id=$1)`, sid); n != 1 {
		t.Errorf("restored audits: %d", n)
	}
	e.setRules("", "/work/secret")
	e.enforce()
	e.setRules("")
	if n := e.enforce(); n.Restored != 1 || e.visible(sid) != 1 {
		t.Fatalf("rule removed: %+v, visible %d", n, e.visible(sid))
	}
}

// An administrator's confirmation purges hidden sessions through the
// deletion machinery (tombstone, job, purge of the raw evidence), audited
// as the administrator; the device then hears of the refusal.
func TestHiddenPurgedOnConfirmation(t *testing.T) {
	e := newEnv(t)
	const secret, other = "0b7e2c1a-0000-4000-8000-00000000a201", "0b7e2c1a-0000-4000-8000-00000000a202"
	home := t.TempDir()
	ssp := claudeAt(t, home, "-work-secret", secret, "/work/secret")
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, ssp)
	sync1(t, sy, claudeAt(t, home, "-work-other", other, "/work/other"))
	e.drain()
	e.setRules("", "/work/secret", "/work/other")
	e.enforce()
	h, err := e.queue.Hidden(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Total != 2 || len(h.ByUser) != 1 || h.ByUser[0].Sessions != 2 || h.ByUser[0].UserID != e.userID || len(h.ByRule) != 2 || h.OldestHiddenAt == nil {
		t.Fatalf("preview %+v", h)
	}
	purged, restored, err := e.queue.PurgeHidden(e.ctx, e.userID, e.deviceID, "/work/secret")
	if err != nil || purged != 1 || restored != 0 {
		t.Fatalf("PurgeHidden = %d, %d, %v", purged, restored, err)
	}
	if c, _ := e.stored(secret); c != 0 {
		t.Fatal("confirmed session still stored")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND hidden_at IS NOT NULL`, other) != 1 {
		t.Fatal("purge of one rule took another rule's session")
	}
	var meta []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT metadata FROM audit_events WHERE action='conversation.purged' AND actor_id=$1 AND device_id=$2`, e.userID, e.deviceID).Scan(&meta); err != nil {
		t.Fatalf("no conversation.purged audit event: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(meta, &m)
	if m["reason"] != "admin_confirmed" || m["rule"] != "/work/secret" || m["job_id"] == "" {
		t.Errorf("audit metadata %v", m)
	}
	if done, err := e.store.ProcessDeletionJobs(e.ctx); err != nil || done != 1 {
		t.Fatalf("deletion job: %d, %v", done, err)
	}
	if n := e.count(`SELECT count(*) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.path=$1`, ssp.Path); n != 0 {
		t.Errorf("purged source kept %d generations", n)
	}
	appendFile(t, ssp.Path, `{"type":"user","cwd":"/work/secret","uuid":"late","sessionId":"`+secret+`","timestamp":"2026-09-20T00:01:00Z","message":{"role":"user","content":"late"}}`+"\n")
	sync1(t, sy, ssp)
	e.drain()
	if c, _ := e.stored(secret); c != 0 {
		t.Fatal("append resurrected the purged session")
	}
	if _, total := sy.Refused(); total != 1 {
		t.Errorf("device refusals: %d", total)
	}
}

// A session hidden for HiddenPurgeAfter is purged by the sweep, audited;
// one hidden for less is not; one no rule covers any more is restored
// instead of purged.
func TestHiddenExpiresIntoPurge(t *testing.T) {
	e := newEnv(t)
	const old, young = "0b7e2c1a-0000-4000-8000-00000000a301", "0b7e2c1a-0000-4000-8000-00000000a302"
	home := t.TempDir()
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, claudeAt(t, home, "-work-secret-a", old, "/work/secret/a"))
	sync1(t, sy, claudeAt(t, home, "-work-secret-b", young, "/work/secret/b"))
	e.drain()
	e.setRules("", "/work/secret")
	e.enforce()
	e.exec(`UPDATE conversations SET hidden_at=now()-interval '7 days 1 minute' WHERE session_id=$1`, old)
	e.exec(`UPDATE conversations SET hidden_at=now()-interval '6 days 23 hours' WHERE session_id=$1`, young)
	if n := e.enforce(); n.Purged != 1 {
		t.Fatalf("EnforceRules = %+v; want 1 purged", n)
	}
	if c, _ := e.stored(old); c != 0 {
		t.Error("expired hide not purged")
	}
	if c, _ := e.stored(young); c != 1 {
		t.Error("young hide purged")
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action='conversation.purged' AND metadata->>'reason'='hidden_expired' AND actor_id=$1`, e.userID); n != 1 {
		t.Errorf("expiry audits: %d", n)
	}
	// Rules gone, but the sweep has not applied the change yet: an expired
	// hide is re-checked and restored, not purged.
	e.exec(`UPDATE conversations SET hidden_at=now()-interval '8 days' WHERE session_id=$1`, young)
	e.exec(`UPDATE collection_policy SET path_rules='[]'`)
	if n := e.enforce(); n.Purged != 0 || n.Restored != 1 || e.visible(young) != 1 {
		t.Fatalf("uncovered expired hide: %+v, visible %d", n, e.visible(young))
	}
}

// A subagent that arrives after its parent was hidden is hidden with it.
func TestHiddenTreeTakesLateSubagent(t *testing.T) {
	e := newEnv(t)
	const parent, child = "0b7e2c1a-0000-4000-8000-00000000a401", "0b7e2c1a-0000-4000-8000-00000000a402"
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), claudeAt(t, t.TempDir(), "-work-secret", parent, "/work/secret"))
	e.drain()
	e.setRules("", "/work/secret")
	e.enforce()
	// A child conversation in an allowed directory naming the parent.
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,cwd,parent_native_session_id,depth) VALUES($1,'claude',$2,$3,$4,'/work/open',$5,1)`,
		uuid.NewString(), child, e.deviceID, e.userID, parent)
	e.enforce()
	if e.visible(child) != 0 {
		t.Fatal("late subagent of a hidden session visible")
	}
}

// Rules that changed while a parse ran are applied to what it stored.
func TestRulesChangedDuringParseRecheck(t *testing.T) {
	e := newEnv(t)
	const sid = "0b7e2c1a-0000-4000-8000-00000000b001"
	sp := claudeAt(t, t.TempDir(), "-work-secret", sid, "/work/secret")
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sp)
	e.drain()
	var srcID string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM sources WHERE path=$1`, sp.Path).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	r, err := loadRules(e.ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	e.setRules("", "/work/secret")
	ref, sessions, err := recheckStored(e.ctx, e.pool, mustRules(t, e), source{id: srcID, agent: "claude"}, sp.Path, deviceDirs{})
	if err != nil || ref == nil || len(sessions) != 1 {
		t.Fatalf("recheck: %v %v %v", ref, sessions, err)
	}
	if now := mustRules(t, e); now.version == r.version {
		t.Fatal("rules_version did not move")
	}
}

func mustRules(t *testing.T, e *env) serverRules {
	t.Helper()
	r, err := loadRules(e.ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestServerDecisionInputs(t *testing.T) {
	if got := homeOf("/Users/me/.claude/projects/-x/s.jsonl"); got != "/Users/me" {
		t.Errorf("homeOf = %q", got)
	}
	if got := homeOf(`C:\Users\me\.codex\sessions\r.jsonl`); got != "C:/Users/me" {
		t.Errorf("homeOf windows = %q", got)
	}
	if got := claudeFolder("", "/Users/me/.claude/projects/-Users-me-app/sid/subagents/agent-1.jsonl"); got != "-Users-me-app" {
		t.Errorf("claudeFolder = %q", got)
	}
	if claudeFolder("", "/tmp/s.jsonl") != "" || claudeFolder("/data/claude/projects", "/Users/me/.claude/projects/-x/s.jsonl") != "" ||
		claudeFolder("/data/claude/projects/", "/data/claude/projects/-x/s.jsonl") != "-x" || absPath(".") || !absPath(`D:\work`) {
		t.Error("placement inputs")
	}
}

// codexTagged writes an older Codex rollout that names its directory only
// in the <environment_context> of its first user message.
func codexTagged(t *testing.T, id, cwd string) devicesync.SourceSpec {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".codex", codex.SessionsDir, "2026", "09", "15")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		codexRec(0, "session_meta", `{"id":"`+id+`","source":"cli"}`),
		codexRec(1, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n<cwd>`+cwd+`</cwd></environment_context>"}]}`),
		codexRec(2, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}`),
		codexRec(3, "response_item", `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
	}
	p := filepath.Join(dir, "rollout-2026-09-15T10-00-00-"+id+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return devicesync.SourceSpec{Path: p, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name}
}

// An older Codex rollout placed only by its <cwd> tag is placed on the
// server as the agent places it, at parse time and by the sweep after a
// rule change: a rule on its directory hides it, and an unrelated rule
// change under an exclude floor does not hide it as unplaceable.
func TestCodexCwdTagPlacesStoredSession(t *testing.T) {
	e := newEnv(t)
	e.setRules("exclude")
	const secret, open = "019f0000-0000-7000-8000-00000000c001", "019f0000-0000-7000-8000-00000000c002"
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, codexTagged(t, secret, "/work/secret"))
	sync1(t, sy, codexTagged(t, open, "/work/open"))
	e.drain()
	for _, sid := range []string{secret, open} {
		if c, m := e.stored(sid); c != 1 || m == 0 {
			t.Fatalf("placed session %s refused at parse: %d, %d", sid, c, m)
		}
	}
	e.setRules("exclude", "deny /work/secret", "deny /elsewhere-near-miss")
	if _, err := e.queue.EnforceRules(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.visible(open) != 1 {
		t.Error("sweep hid a placed session no rule covers")
	}
	if e.visible(secret) != 0 {
		t.Error("sweep left visible a session its rule covers")
	}
}

// The sweep hides only what a rule covers: directories that share a
// prefix, a "~" rule's path under another home, and names one character
// off stay stored.
func TestRuleSweepNearMisses(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	keep := map[string]string{
		"0b7e2c1a-0000-4000-8000-00000000e101": "/work/secret-other",
		"0b7e2c1a-0000-4000-8000-00000000e102": "/work/secretx",
		"0b7e2c1a-0000-4000-8000-00000000e103": "/work/secre",
		"0b7e2c1a-0000-4000-8000-00000000e104": "/other/work/secret",
		"0b7e2c1a-0000-4000-8000-00000000e105": "/elsewhere/clients/acme",
		"0b7e2c1a-0000-4000-8000-00000000e106": filepath.Join(home, "clients", "acme-corp"),
		"0b7e2c1a-0000-4000-8000-00000000e107": "/x/secret-projx",
		"0b7e2c1a-0000-4000-8000-00000000e108": "/x/my-secret-proj",
	}
	drop := map[string]string{
		"0b7e2c1a-0000-4000-8000-00000000e201": "/work/secret",
		"0b7e2c1a-0000-4000-8000-00000000e202": "/work/secret/deep/er",
		"0b7e2c1a-0000-4000-8000-00000000e203": filepath.Join(home, "clients", "acme", "web"),
		"0b7e2c1a-0000-4000-8000-00000000e204": "/x/y/secret-proj/src",
	}
	for _, set := range []map[string]string{keep, drop} {
		for sid, cwd := range set {
			sync1(t, sy, claudeAt(t, home, pathpolicy.ClaudeFolderName(cwd), sid, cwd))
		}
	}
	e.drain()
	e.setRules("", "deny /work/secret", "deny ~/clients/acme", "local secret-proj")
	if _, err := e.queue.EnforceRules(e.ctx); err != nil {
		t.Fatal(err)
	}
	for sid, cwd := range keep {
		if e.visible(sid) != 1 {
			t.Errorf("%s hidden", cwd)
		}
	}
	for sid, cwd := range drop {
		if e.visible(sid) != 0 {
			t.Errorf("%s visible", cwd)
		}
	}
}

// hookObjects runs onGet once, on the first object read.
type hookObjects struct {
	Objects
	once  sync.Once
	onGet func()
}

func (h *hookObjects) Get(ctx context.Context, key string) ([]byte, error) {
	h.once.Do(h.onGet)
	return h.Objects.Get(ctx, key)
}

// A rule added while a parse reads the source (after it loaded the rules,
// before it committed) refuses what that parse stored, with no sweep.
func TestRuleAddedMidParseRefusesStored(t *testing.T) {
	e := newEnv(t)
	const sid = "0b7e2c1a-0000-4000-8000-00000000b101"
	sp := claudeAt(t, t.TempDir(), "-work-secret", sid, "/work/secret")
	// Sealed at once, so the parse reads it from object storage.
	sync1(t, e.syncer(devicesync.Config{SealAfter: time.Nanosecond}), sp)
	e.queue.Objects = &hookObjects{Objects: e.objects, onGet: func() { e.setRules("", "deny /work/secret") }}
	e.drain()
	if mustRules(t, e).version == 0 {
		t.Fatal("the rules did not change during the parse")
	}
	if c, m := e.stored(sid); c != 0 || m != 0 {
		t.Fatalf("session stored under a rule added mid-parse: %d, %d", c, m)
	}
	if n := e.count(`SELECT count(*) FROM sources WHERE path=$1 AND refused_rule='/work/secret' AND tombstoned_at IS NOT NULL`, sp.Path); n != 1 {
		t.Error("source not refused")
	}
}

// A "~" rule means the home the device reports, not one inferred from the
// transcript's path (wrong when CLAUDE_CONFIG_DIR lies outside the home);
// the reported Claude projects directory places a folder the same way.
func TestTildeIsReportedHome(t *testing.T) {
	e := newEnv(t)
	e.setRules("", "deny ~/clients/acme")
	harness := t.TempDir() // CLAUDE_CONFIG_DIR, outside the home
	const reported, inferred = "0b7e2c1a-0000-4000-8000-00000000c101", "0b7e2c1a-0000-4000-8000-00000000c102"
	sy := e.syncer(devicesync.Config{SealAfter: -1, Device: syncproto.DeviceDirs{Home: "/custom/home", ClaudeProjects: filepath.Join(harness, ".claude", "projects")}})
	sync1(t, sy, claudeAt(t, harness, "-x", reported, "/custom/home/clients/acme/web"))
	sync1(t, sy, claudeAt(t, harness, "-y", inferred, filepath.Join(harness, "clients", "acme")))
	e.drain()
	if c, _ := e.stored(reported); c != 0 {
		t.Error("session under the reported home's ~/clients/acme stored")
	}
	if c, _ := e.stored(inferred); c != 1 {
		t.Error("session under the path-inferred home refused though the device reported another home")
	}
	var home, projects string
	if err := e.pool.QueryRow(e.ctx, `SELECT home,claude_projects FROM devices WHERE id=$1`, e.deviceID).Scan(&home, &projects); err != nil || home != "/custom/home" {
		t.Errorf("device home %q projects %q: %v", home, projects, err)
	}
}

// A rule added and not yet swept: an append to a stored session it
// covers is written hidden at parse time (audited), not refused.
func TestAppendUnderUnsweptRuleHides(t *testing.T) {
	e := newEnv(t)
	const sid = "0b7e2c1a-0000-4000-8000-00000000c201"
	sp := claudeAt(t, t.TempDir(), "-work-secret", sid, "/work/secret")
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	e.setRules("", "/work/secret")
	appendFile(t, sp.Path, `{"type":"user","cwd":"/work/secret","uuid":"late","sessionId":"`+sid+`","timestamp":"2026-09-20T00:01:00Z","message":{"role":"user","content":"late"}}`+"\n")
	sync1(t, sy, sp)
	e.drain()
	if c, m := e.stored(sid); c != 1 || m != 3 {
		t.Fatalf("append: %d conversations, %d messages", c, m)
	}
	if e.visible(sid) != 0 {
		t.Fatal("covered session visible")
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action='conversation.hidden' AND metadata->>'at'='parse'`); n != 1 {
		t.Errorf("parse-time hide audits: %d", n)
	}
	if n := e.enforce(); n.Hidden != 0 {
		t.Errorf("sweep hid it again: %+v", n)
	}
}

// When a flush's in-transaction check finds the source refused (the
// refusal committed after the pre-check), the reply still names the rule.
func TestRefusedRuleInRacedFlushReply(t *testing.T) {
	e := newEnv(t)
	e.setRules("", "deny /work/secret")
	sp := claudeAt(t, t.TempDir(), "-work-secret", "0b7e2c1a-0000-4000-8000-00000000c301", "/work/secret")
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sp)
	e.drain()
	var fileID string
	if err := e.pool.QueryRow(e.ctx, `SELECT file_id FROM sources WHERE path=$1 AND refused_rule IS NOT NULL`, sp.Path).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	conn, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	h := &syncproto.FlushHeader{Source: syncproto.Source{Agent: "claude", Path: sp.Path, FileID: fileID, StorageKind: string(transcript.StorageJSONLAppend), Parser: claude.ParserName}, Generation: 1}
	f := &flush{s: &Server{Pool: e.pool}, deviceID: e.deviceID, h: h, conn: conn, reserved: map[syncproto.Hash]bool{}, sizes: map[syncproto.Hash]int64{}}
	tx, err := conn.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	resp, _, err := f.commit(e.ctx, tx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Refused != "/work/secret" {
		t.Errorf("Refused = %q", resp.Refused)
	}
}

// The admin API previews hidden sessions per user and rule, counts them
// in the status, and purges them only on an explicit confirmation.
func TestHiddenAdminAPI(t *testing.T) {
	e := newEnv(t)
	const sid = "0b7e2c1a-0000-4000-8000-00000000c401"
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), claudeAt(t, t.TempDir(), "-work-secret", sid, "/work/secret"))
	e.drain()
	e.setRules("", "/work/secret")
	e.enforce()
	e.exec(`UPDATE users SET role='admin' WHERE id=$1`, e.userID)
	plain, hash, _ := auth.NewToken()
	e.exec(`INSERT INTO credentials(id,user_id,kind,token_hash,created_at,expires_at) VALUES($1,$2,'session',$3,now(),now()+interval '1 hour')`, uuid.NewString(), e.userID, hash)
	c := client.HTTP{Server: e.http.URL, Token: plain}
	var h domain.HiddenSummary
	if err := c.JSON(e.ctx, "GET", "/v1/admin/policy/hidden", nil, &h); err != nil {
		t.Fatal(err)
	}
	if h.Total != 1 || len(h.ByRule) != 1 || h.ByRule[0].Rule != "/work/secret" || len(h.ByUser) != 1 || h.ByUser[0].Email == "" || h.PurgeAfter == "" {
		t.Fatalf("preview %+v", h)
	}
	var st struct {
		Index map[string]any `json:"index"`
	}
	if err := c.JSON(e.ctx, "GET", "/v1/admin/status", nil, &st); err != nil || st.Index["hidden_sessions"] != float64(1) {
		t.Fatalf("status %v, %v", st.Index, err)
	}
	var out map[string]int
	if err := c.JSON(e.ctx, "POST", "/v1/admin/policy/hidden/purge", map[string]any{"rule": "/work/secret"}, &out); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("purge without confirm: %v", err)
	}
	if c, _ := e.stored(sid); c != 1 {
		t.Fatal("unconfirmed purge deleted")
	}
	if err := c.JSON(e.ctx, "POST", "/v1/admin/policy/hidden/purge", map[string]any{"confirm": true}, &out); err != nil || out["purged"] != 1 {
		t.Fatalf("purge: %v, %v", out, err)
	}
	if c, _ := e.stored(sid); c != 0 {
		t.Fatal("confirmed purge kept the session")
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action='policy.hidden.purge' AND actor_id=$1`, e.userID); n != 1 {
		t.Errorf("purge confirmation audits: %d", n)
	}
}
