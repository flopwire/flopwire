package agent

// Path rules (decision D18, package pathpolicy): a session whose placement
// (placement.go: working directory, worktree root, main checkout root,
// origin remote) matches a deny rule is neither indexed nor uploaded, one
// matching a local rule is indexed but never uploaded. A session with no
// placement at all gets the unplaceable setting (default local). The agent
// enforces them before indexing (indexTranscript, indexCompanion, the Devin
// sink, orphan stubs) and before upload (every hand-over to sync, plus a
// filter on the sync scheduler for what it had queued already).
//
// Rules come from two places. User rules: Config.UserRuleList (the client
// config's denylist) and the file Config.UserRules, one rule per line,
// re-read when it changes, plus Config.Unplaceable. Admin rules: the
// server's collection policy (GET /v1/policy, path_rules and
// unplaceable), fetched at start and every AdminEvery, and cached in
// Config.AdminRulesCache so an offline start still applies the last floor.
//
// When the rules change (including at every start), every indexed
// conversation is checked again: rows of a session a deny rule now covers
// are purged from the local index, and the agent logs that copies already
// uploaded stay on the server until the owner or an admin deletes them
// there. The one exception is a session a directory it named later moves
// to local or deny after some of it was uploaded: the agent asks the
// server to delete that session (recordCwds, sendWithholds).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

// policyView is the rules in force and their generation; it changes as a
// whole.
type policyView struct {
	pol pathpolicy.Policy
	gen uint64
	key string // canonical form, to detect a change
}

func policyKey(p pathpolicy.Policy) string {
	var b strings.Builder
	for _, r := range p.Admin {
		b.WriteString("a " + r.String() + "\n")
	}
	for _, r := range p.User {
		b.WriteString("u " + r.String() + "\n")
	}
	fmt.Fprintf(&b, "unplaceable %v %v\n", p.Unplaceable, p.UnplaceableAdmin)
	return b.String()
}

// policy returns the rules in force.
func (a *Agent) policy() *policyView {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pol
}

// fileStamp is enough of a stat to notice that a rules file changed.
type fileStamp struct {
	ok   bool
	size int64
	mod  int64
}

func stampOf(path string) fileStamp {
	fi, err := fsprobe.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{true, fi.Size(), fi.ModTime().UnixNano()}
}

// refreshPolicy re-reads the user rules file when it changed and, with
// fetch, asks the server for the admin rules; a change applies the new
// rules (setPolicy). The first call always applies what it read.
func (a *Agent) refreshPolicy(ctx context.Context, fetch bool) {
	var fetched *AdminPolicy
	if fetch && a.cfg.AdminRules != nil {
		// Outside polMu: a slow server must not hold up a sweep.
		fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		rules, err := a.cfg.AdminRules(fctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				a.log.Info("agent: admin path rules not fetched; using the last known", "err", err)
			}
		} else {
			fetched = &rules
		}
	}
	a.polMu.Lock()
	defer a.polMu.Unlock()
	changed := !a.polApplied
	if st := stampOf(a.cfg.UserRules); a.cfg.UserRules != "" && st != a.userStamp {
		a.userStamp, changed = st, true
	}
	if !a.polLoaded {
		a.adminRaw = a.readAdminCache()
	}
	if fetched != nil && (!slices.Equal(fetched.Rules, a.adminRaw.Rules) || fetched.Unplaceable != a.adminRaw.Unplaceable) {
		a.adminRaw, changed = *fetched, true
		a.writeAdminCache(*fetched)
	}
	a.polLoaded = true
	if !changed {
		return
	}
	pol := a.buildPolicy()
	a.mu.Lock()
	same := a.pol.key == policyKey(pol)
	a.mu.Unlock()
	if same && a.polApplied {
		return
	}
	a.setPolicy(ctx, pol)
	a.polApplied = true
}

// preloadPolicy puts the rules on disk (user rules, cached admin rules)
// in force without applying them (setPolicy), so the sync filter checks
// them before the first refreshPolicy. Called from New.
func (a *Agent) preloadPolicy() {
	a.polMu.Lock()
	defer a.polMu.Unlock()
	a.adminRaw = a.readAdminCache()
	if a.cfg.UserRules != "" {
		a.userStamp = stampOf(a.cfg.UserRules)
	}
	a.polLoaded = true
	pol := a.buildPolicy()
	a.pol = &policyView{pol: pol, gen: 1, key: policyKey(pol)}
}

// buildPolicy parses the user and admin rules, with "~" as this user's
// home. Unparseable rules are logged and skipped.
func (a *Agent) buildPolicy() pathpolicy.Policy {
	var pol pathpolicy.Policy
	parse := func(lines []string, what string) []pathpolicy.Rule {
		rules, err := pathpolicy.ParseRules(lines)
		if err != nil {
			a.log.Error("agent: path rules", "source", what, "err", err)
		}
		for i := range rules {
			rules[i] = rules[i].ExpandHome(a.cfg.Home)
		}
		return withPhysical(rules)
	}
	pol.Admin = parse(a.adminRaw.Rules, "server")
	user := slices.Clone(a.cfg.UserRuleList)
	if a.cfg.UserRules != "" {
		if b, err := fsprobe.ReadFile(a.cfg.UserRules); err == nil {
			user = append(user, strings.Split(string(b), "\n")...)
		} else if !errors.Is(err, os.ErrNotExist) {
			a.log.Error("agent: path rules", "file", a.cfg.UserRules, "err", err)
		}
	}
	pol.User = parse(user, a.cfg.UserRules)
	// Unplaceable sessions: the user's setting (default local), with the
	// admin's as a floor.
	um := pathpolicy.DefaultUnplaceable
	if a.cfg.Unplaceable != "" {
		if m, ok := pathpolicy.ParseUnplaceable(a.cfg.Unplaceable); ok {
			um = m
		} else {
			a.log.Error("agent: unplaceable setting not understood; using local", "value", a.cfg.Unplaceable)
		}
	}
	pol.Unplaceable = um
	if a.adminRaw.Unplaceable != "" {
		if m, ok := pathpolicy.ParseUnplaceable(a.adminRaw.Unplaceable); !ok {
			a.log.Error("agent: server unplaceable setting not understood", "value", a.adminRaw.Unplaceable)
		} else if m >= um {
			pol.Unplaceable, pol.UnplaceableAdmin = m, true
		}
	}
	return pol
}

// DevicePolicy is the path rules an agent configured with cfg puts in
// force before it asks the server: the user's rules (UserRules,
// UserRuleList), the admin rules it cached last (AdminRulesCache) and the
// unplaceable settings. A command other than the agent uses it to judge a
// directory as the agent would before it names that directory to the
// server. Unparseable rules are skipped, as the agent skips them.
func DevicePolicy(cfg Config) pathpolicy.Policy {
	if cfg.Home == "" {
		cfg.Home = defaultHome()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	a := &Agent{cfg: cfg, log: cfg.Logger}
	a.adminRaw = a.readAdminCache()
	return a.buildPolicy()
}

// Uploads reports whether pol lets a session placed at pl reach the
// server: pl as recorded and with its symlinks resolved, as decideOne
// matches it.
func Uploads(pol pathpolicy.Policy, pl pathpolicy.Placement) bool {
	if pol.Mode(pl) != pathpolicy.Allow {
		return false
	}
	rp := pl
	rp.Cwd, rp.Worktree, rp.Main = physicalPath(pl.Cwd), physicalPath(pl.Worktree), physicalPath(pl.Main)
	return rp == pl || pol.Mode(rp) == pathpolicy.Allow
}

// withPhysical adds, for each absolute path rule whose literal prefix (up
// to the first glob) passes through a symlink, the same rule on the
// resolved path: "deny /tmp/x" also covers a session that recorded
// /private/tmp/x. A pattern is lowercase (pathpolicy.Normalize), so the
// prefix is looked up on disk in any case (foldPaths): on a case-sensitive
// file system the lowercase path does not exist.
func withPhysical(rules []pathpolicy.Rule) []pathpolicy.Rule {
	out := slices.Clone(rules)
	seen := map[pathpolicy.Rule]bool{}
	for _, r := range rules {
		seen[r] = true
	}
	for _, r := range rules {
		if r.Repo || !strings.HasPrefix(r.Pattern, "/") {
			continue
		}
		segs := strings.Split(strings.Trim(r.Pattern, "/"), "/")
		i := 0
		for i < len(segs) && !strings.ContainsAny(segs[i], "*?[") {
			i++
		}
		prefix := "/" + strings.Join(segs[:i], "/")
		for _, v := range foldPaths(prefix) {
			phys := pathpolicy.Normalize(physicalPath(v))
			if phys == "" || phys == pathpolicy.Normalize(prefix) {
				continue
			}
			p := phys
			if i < len(segs) {
				p = strings.TrimSuffix(p, "/") + "/" + strings.Join(segs[i:], "/")
			}
			nr := r
			nr.Pattern = pathpolicy.Normalize(p)
			if !seen[nr] {
				seen[nr] = true
				out = append(out, nr)
			}
		}
	}
	return out
}

// maxFold bounds the spellings foldPaths returns.
const maxFold = 8

// foldPaths returns the spellings on disk of the absolute path p, compared
// without case: its longest existing ancestor as the directory entries
// spell it, with the rest of p appended. A directory that cannot be listed
// is matched by p's own spelling.
func foldPaths(p string) []string {
	segs := strings.Split(strings.Trim(filepath.Clean(p), "/"), "/")
	cur := []string{"/"}
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		var next []string
		for _, d := range cur {
			ents, err := fsprobe.ReadDir(d)
			if err != nil {
				if _, err := fsprobe.Lstat(filepath.Join(d, seg)); err == nil {
					next = append(next, filepath.Join(d, seg))
				}
				continue
			}
			for _, e := range ents {
				if strings.EqualFold(e.Name(), seg) && len(next) < maxFold {
					next = append(next, filepath.Join(d, e.Name()))
				}
			}
		}
		if len(next) == 0 {
			rest := filepath.Join(segs[i:]...)
			for j := range cur {
				cur[j] = filepath.Join(cur[j], rest)
			}
			return cur
		}
		cur = next
	}
	return cur
}

// AdminPolicy is the part of the server's collection policy the agent
// applies: the admin path rules and the unplaceable floor ("" for none).
type AdminPolicy struct {
	Rules       []string `json:"rules"`
	Unplaceable string   `json:"unplaceable,omitempty"`
}

type adminCache struct {
	AdminPolicy
	FetchedAt time.Time `json:"fetched_at"`
}

func (a *Agent) readAdminCache() AdminPolicy {
	if a.cfg.AdminRulesCache == "" {
		return AdminPolicy{}
	}
	b, err := fsprobe.ReadFile(a.cfg.AdminRulesCache)
	if err != nil {
		return AdminPolicy{}
	}
	var c adminCache
	if err := json.Unmarshal(b, &c); err != nil {
		a.log.Error("agent: admin path rules cache unreadable", "file", a.cfg.AdminRulesCache, "err", err)
		return AdminPolicy{}
	}
	return c.AdminPolicy
}

func (a *Agent) writeAdminCache(p AdminPolicy) {
	if a.cfg.AdminRulesCache == "" {
		return
	}
	b, _ := json.Marshal(adminCache{AdminPolicy: p, FetchedAt: time.Now().UTC()})
	tmp := a.cfg.AdminRulesCache + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, a.cfg.AdminRulesCache)
		if err != nil {
			a.log.Warn("agent: admin path rules cache", "err", err)
		}
	}
}

// setPolicy puts pol in force after a rule change (or at start): stored
// placements whose directory still exists are resolved again first, so a
// remote or worktree added since is seen, then the rules are enforced.
func (a *Agent) setPolicy(ctx context.Context, pol pathpolicy.Policy) {
	a.mu.Lock()
	clear(a.phys) // a symlink may have changed too
	a.mu.Unlock()
	a.reresolve()
	if !pol.Empty() {
		a.log.Info("agent: path rules in force", "admin", len(pol.Admin), "user", len(pol.User),
			"unplaceable", pathpolicy.UnplaceableName(pol.Unplaceable))
	}
	a.devin.dirty.Store(true)
	a.enforce(ctx, pol)
}

// enforce puts pol in force: cached decisions are dropped, files a deny
// rule kept out are checked again at the next sweep (a loosened rule lets
// them in), every source is offered to sync again (a loosened local rule
// lets it upload), and conversations a deny rule now covers are purged.
// It also runs after the recovery pass changed placements.
func (a *Agent) enforce(ctx context.Context, pol pathpolicy.Policy) {
	a.mu.Lock()
	a.pol = &policyView{pol: pol, gen: a.pol.gen + 1, key: policyKey(pol)}
	for _, t := range a.targets {
		if t.modeGen != 0 && t.mode == pathpolicy.Deny {
			t.seen, t.seenAt = transcript.Identity{}, 0
		}
		t.modeGen = 0
	}
	clear(a.notified)
	a.devinModes = nil
	a.mu.Unlock()
	// Devin sessions too: the next poll hands every session over again.
	a.devin.mu.Lock()
	a.devin.synced = false
	a.devin.mu.Unlock()
	a.devin.dirty.Store(true)
	if err := a.purgeDenied(ctx); err != nil && ctx.Err() == nil {
		a.stats.Errors.Add(1)
		a.log.Error("agent: purging sessions a deny rule covers", "err", err)
	}
	if err := a.devinResync(ctx); err != nil && ctx.Err() == nil {
		a.log.Warn("agent: devin after a path rules change", "err", err)
	}
}

// reresolve resolves again every stored placement whose directory still
// exists and whose main checkout or remote is unknown, and fills in only
// what was unknown: a remote added since, or the repository of a directory
// that was not one. A main checkout or remote already stored is never
// replaced, since the directory may have been deleted and made again as
// another repository, and moving the session there would drop the rules
// of the one it ran in.
func (a *Agent) reresolve() {
	type entry struct {
		key placeKey
		p   placed
	}
	a.mu.Lock()
	var list []entry
	for k, p := range a.places {
		if p.pl.Cwd != "" && (p.pl.Main == "" || p.pl.Remote == "") {
			list = append(list, entry{k, p})
		}
	}
	a.mu.Unlock()
	type res struct {
		pl pathpolicy.Placement
		ok bool
	}
	byCwd := map[string]res{}
	for _, e := range list {
		r, done := byCwd[e.p.pl.Cwd]
		if !done {
			if fi, err := fsprobe.Stat(e.p.pl.Cwd); err == nil && fi.IsDir() {
				r = res{a.resolve(e.p.pl.Cwd, ""), true}
			}
			byCwd[e.p.pl.Cwd] = r
		}
		if !r.ok {
			continue
		}
		np := e.p
		if np.pl.Main == "" && r.pl.Main != "" {
			np.pl.Worktree, np.pl.Main = r.pl.Worktree, r.pl.Main
			if e.p.how != localindex.PlacedByFolder {
				np.how = cwdHow(np.pl) // the directory exists again: git places it
			}
		}
		if np.pl.Remote == "" {
			np.pl.Remote = r.pl.Remote
		}
		if np != e.p {
			a.replacePlace(e.key, e.p, np)
		}
	}
}

// devinResync re-reads Devin's store from the start when a session the
// rules allow is missing from the index: a deny rule kept it out before,
// and the parser's cursor already counts it as read.
func (a *Agent) devinResync(ctx context.Context) error {
	d := &a.devin
	d.mu.Lock()
	sourceID := d.sourceID
	d.mu.Unlock()
	if d.path == "" || sourceID == 0 {
		return nil
	}
	cwds, err := devinCwds(ctx, d.path)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	rows, err := a.store.DB().QueryContext(ctx, `SELECT session_id FROM conversations WHERE agent = ?`, string(transcript.AgentDevin))
	if err != nil {
		return err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		have[s] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	pv := a.policy()
	for s, cwd := range cwds {
		if !have[s] && a.devinDecide(pv, s, cwd).Mode != pathpolicy.Deny {
			d.mu.Lock()
			defer d.mu.Unlock()
			if err := a.store.SaveWatermark(ctx, sourceID, transcript.Watermark{}, nil); err != nil {
				return err
			}
			d.dirty.Store(true)
			return nil
		}
	}
	return nil
}

// devinDecide applies the rules to a Devin session, placed by the
// working directory Devin recorded (stored the first time).
func (a *Agent) devinDecide(pv *policyView, session, cwd string) pathpolicy.Decision {
	key := placeKey{transcript.AgentDevin, session}
	if !filepath.IsAbs(cwd) {
		cwd = "" // "." or garbage names nothing
	}
	p, ok := a.storedPlace(key)
	if !ok || !p.final() && cwd != "" {
		np := placed{how: localindex.PlacedByNone}
		if cwd != "" {
			np.pl = a.resolve(cwd, "")
			np.how = cwdHow(np.pl)
		}
		p = a.savePlace(key, np)
	}
	return a.decide(pv.pol, p)
}

// placeKeyOf is the session a tracked file's placement belongs to, and
// the transcript that places it: a transcript's own session, a
// companion's owner and its parent transcript.
func (t *target) placeKeyOf() (placeKey, string) {
	if t.kind == kindCompanion {
		return placeKey{transcript.AgentClaude, t.owner}, t.parent
	}
	s := t.src.SessionKey
	if s == "" {
		s = t.path
	}
	return placeKey{t.src.Agent, s}, t.path
}

// decisionOf returns the rules' decision for a tracked file. known is
// false while its session cannot be placed yet (a young transcript that
// has not named its directory, or a deleted worktree the recovery pass
// has not looked at: settled). final is true when the placement came from a
// directory the transcript named: a fallback placement is replaced when
// the transcript later names one, so its decision must not be cached.
func (a *Agent) decisionOf(pv *policyView, t *target) (d pathpolicy.Decision, known, final bool) {
	a.mu.Lock()
	var parent *target
	if t.kind == kindCompanion {
		parent = a.targets[t.parent]
	}
	key, path := t.placeKeyOf()
	a.mu.Unlock()
	if parent != nil {
		return a.decisionOf(pv, parent)
	}
	p, known := a.placeOf(key, path)
	if !known || !settled(pv.pol, p) {
		return pathpolicy.Decision{}, false, false
	}
	d = a.decide(pv.pol, p)
	a.mu.Lock()
	root := t.root
	a.mu.Unlock()
	if root != "" {
		// A Claude subagent goes with its session: at least its verdict.
		if rp, ok := a.storedPlace(placeKey{transcript.AgentClaude, root}); ok {
			if !settled(pv.pol, rp) {
				return pathpolicy.Decision{}, false, false
			}
			if d2 := a.decide(pv.pol, rp); d2.Mode > d.Mode {
				d = d2
			}
		}
	}
	return d, true, p.final()
}

// modeOf returns the rules' verdict for a tracked file. known is false
// while the session cannot be placed yet (a new file whose first complete
// line has not been written); callers then do not upload it, do not index
// it when a rule could deny it (worstMode), and ask again later.
func (a *Agent) modeOf(t *target) (mode pathpolicy.Mode, known bool) {
	pv := a.policy()
	if pv.pol.Empty() {
		// Nothing to decide, but the placement is still recorded when the
		// session is first seen, for rules added after its worktree is gone.
		if t.kind == kindTranscript {
			a.mu.Lock()
			key, path := t.placeKeyOf()
			a.mu.Unlock()
			if _, ok := a.storedPlace(key); !ok {
				a.placeOf(key, path)
			}
		}
		return pathpolicy.Allow, true
	}
	a.mu.Lock()
	if t.modeGen == pv.gen {
		m := t.mode
		a.mu.Unlock()
		return m, true
	}
	a.mu.Unlock()
	d, known, final := a.decisionOf(pv, t)
	if !known {
		return pathpolicy.Allow, false
	}
	a.mu.Lock()
	if final && a.pol.gen == pv.gen {
		t.mode, t.modeGen = d.Mode, pv.gen
	}
	a.mu.Unlock()
	return d.Mode, true
}

// indexable reports whether t may be indexed now: its verdict is not
// deny, or, while it is unknown, no rule or setting could deny it.
func (a *Agent) indexable(t *target) bool {
	m, known := a.modeOf(t)
	if !known {
		return worstMode(a.policy().pol) != pathpolicy.Deny
	}
	return m != pathpolicy.Deny
}

// worstMode is the most restrictive verdict the policy can give.
func worstMode(p pathpolicy.Policy) pathpolicy.Mode {
	m := p.Unplaceable
	for _, rs := range [][]pathpolicy.Rule{p.Admin, p.User} {
		for _, r := range rs {
			m = max(m, r.Mode)
		}
	}
	return m
}

// uploadable reports whether t may be handed to sync.
func (a *Agent) uploadable(t *target) bool {
	m, known := a.modeOf(t)
	return known && m == pathpolicy.Allow
}

// devinMode is the verdict for one Devin session.
func (a *Agent) devinMode(ctx context.Context, session string) pathpolicy.Mode {
	pv := a.policy()
	if pv.pol.Empty() {
		return pathpolicy.Allow
	}
	a.mu.Lock()
	m, ok := a.devinModes[session]
	a.mu.Unlock()
	if ok {
		return m
	}
	a.loadDevinModes(ctx, pv)
	a.mu.Lock()
	m, ok = a.devinModes[session]
	a.mu.Unlock()
	if !ok { // no sessions row: no directory
		m = a.devinDecide(pv, session, "").Mode
	}
	return m
}

// loadDevinModes reads every Devin session's working directory and caches
// the verdicts.
func (a *Agent) loadDevinModes(ctx context.Context, pv *policyView) {
	cwds, err := devinCwds(ctx, a.devin.path)
	if err != nil {
		a.log.Debug("agent: devin session directories", "err", err)
	}
	modes := make(map[string]pathpolicy.Mode, len(cwds))
	for s, cwd := range cwds {
		modes[s] = a.devinDecide(pv, s, cwd).Mode
	}
	a.mu.Lock()
	if a.pol.gen == pv.gen {
		a.devinModes = modes
	}
	a.mu.Unlock()
}

// allowUpload is the sync scheduler's filter: it drops a queued flush the
// rules no longer allow (a deny or local rule added after the capture).
func (a *Agent) allowUpload(spec devicesync.SourceSpec) bool {
	if a.policy().pol.Empty() {
		return true
	}
	a.mu.Lock()
	loaded := a.placesOK
	a.mu.Unlock()
	if !loaded {
		return false // placing it now could replace a stored placement with a weaker one
	}
	if spec.Agent == transcript.AgentDevin && spec.Export {
		if p, ok := a.storedPlace(placeKey{transcript.AgentDevin, spec.SessionKey}); ok && !settled(a.policy().pol, p) {
			return false // a deleted worktree the recovery pass has not looked at
		}
		return a.devinMode(context.Background(), spec.SessionKey) == pathpolicy.Allow
	}
	a.mu.Lock()
	t := a.targets[spec.Path]
	a.mu.Unlock()
	if t != nil {
		return a.uploadable(t)
	}
	// Not tracked (the file is gone, or not listed yet): place it the way
	// a tracked file would be, from the stored placement, the transcript
	// or a fallback.
	nt := &target{path: spec.Path, kind: kindTranscript, src: transcript.Source{Agent: spec.Agent, SessionKey: spec.SessionKey}}
	if spec.Parent != "" {
		nt.kind, nt.owner, nt.parent = kindCompanion, spec.SessionKey, spec.Parent
	}
	pv := a.policy()
	d, known, _ := a.decisionOf(pv, nt)
	return known && d.Mode == pathpolicy.Allow
}

// purgeDenied removes from the local index every conversation a deny
// rule covers, with its subagent conversations.
func (a *Agent) purgeDenied(ctx context.Context) error {
	pv := a.policy()
	if pv.pol.Empty() {
		return nil
	}
	if err := a.store.Sync(ctx); err != nil {
		return err
	}
	rows, err := a.store.DB().QueryContext(ctx, `SELECT c.id, c.agent, c.session_id, ifnull(c.cwd, ''), ifnull(c.repo_root, ''),
		ifnull(c.parent_conversation_id, 0), ifnull(c.source_id, 0), ifnull(s.storage_kind, '')
		FROM conversations c LEFT JOIN sources s ON s.id = c.source_id ORDER BY c.depth, c.id`)
	if err != nil {
		return err
	}
	var all []purgeConv
	for rows.Next() {
		var c purgeConv
		if err := rows.Scan(&c.id, &c.agent, &c.session, &c.cwd, &c.repo, &c.parent, &c.source, &c.kind); err != nil {
			rows.Close()
			return err
		}
		all = append(all, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	denied := map[int64]pathpolicy.Decision{}
	var todo []purgeConv
	for _, c := range all { // parents first (ORDER BY depth)
		p, ok := a.storedPlace(placeKey{transcript.Agent(c.agent), c.session})
		abs := filepath.IsAbs(c.cwd)
		if !ok && abs {
			p.pl = a.resolve(c.cwd, "")
			p.how = cwdHow(p.pl)
		}
		if !ok && !abs {
			// Never placed (an orphan stub, or a row from before the
			// placement was stored): not decided here; its files are.
			continue
		}
		d := a.decide(pv.pol, p)
		if pd, ok := denied[c.parent]; ok && d.Mode != pathpolicy.Deny {
			d = pd // a subagent goes with its parent
		}
		if d.Mode == pathpolicy.Deny {
			denied[c.id] = d
			c.why = d
			todo = append(todo, c)
		}
	}
	return a.purge(ctx, todo)
}

// purgeSource removes the conversations recorded from one source (a
// tracked file a deny rule now covers).
func (a *Agent) purgeSource(ctx context.Context, sourceID int64, why pathpolicy.Decision) error {
	rows, err := a.store.DB().QueryContext(ctx, `SELECT c.id, c.agent, c.session_id, ifnull(s.storage_kind, '')
		FROM conversations c JOIN sources s ON s.id = c.source_id WHERE c.source_id = ?`, sourceID)
	if err != nil {
		return err
	}
	var todo []purgeConv
	for rows.Next() {
		c := purgeConv{source: sourceID, why: why}
		if err := rows.Scan(&c.id, &c.agent, &c.session, &c.kind); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return a.purge(ctx, todo)
}

type purgeConv struct {
	id, parent, source int64
	agent, session     string
	cwd, repo, kind    string
	why                pathpolicy.Decision
}

// purge hard-deletes the conversations and forgets what their sources
// were indexed up to, so a rule loosened later indexes them again from
// the start. It logs what it removed and, with a server configured, that
// uploaded copies stay there.
func (a *Agent) purge(ctx context.Context, todo []purgeConv) error {
	if len(todo) == 0 {
		return nil
	}
	sources := map[int64]string{}
	var names []string
	for _, c := range todo {
		if err := a.store.PurgeConversation(ctx, transcript.Agent(c.agent), c.session); err != nil {
			return err
		}
		if c.source != 0 && c.kind != string(transcript.StorageCompanion) {
			sources[c.source] = c.kind
		}
		if len(names) < 20 {
			names = append(names, fmt.Sprintf("%s:%s (%s)", c.agent, c.session, c.why.Reason()))
		}
	}
	for id, kind := range sources {
		if err := a.store.SaveWatermark(ctx, id, transcript.Watermark{}, nil); err != nil {
			return err
		}
		if kind == string(transcript.StorageSQLite) {
			a.devin.dirty.Store(true)
		}
	}
	if err := a.store.Sync(ctx); err != nil {
		return err
	}
	a.log.Warn("agent: path rules: removed sessions from the local index", "count", len(todo), "sessions", strings.Join(names, ", "))
	if a.cfg.Sync != nil {
		a.noteServerCopies(len(todo), names)
		a.log.Warn("agent: path rules: copies of these sessions already uploaded stay on the server; " +
			"the owner or an admin must delete them there (flopwire never deletes server data for a local rule)")
	}
	return nil
}

// ServerCopies is the D18 notice for `flopwire agent status`: sessions a
// path rule removed from the local index since the agent started, whose
// uploaded copies stay on the server until the owner or an admin deletes
// them there.
type ServerCopies struct {
	Count    int       `json:"count"`
	Sessions []string  `json:"sessions"` // the first 20
	At       time.Time `json:"at"`       // the latest purge
}

func (a *Agent) noteServerCopies(n int, names []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.serverCopies == nil {
		a.serverCopies = &ServerCopies{}
	}
	c := a.serverCopies
	c.Count += n
	c.At = time.Now()
	c.Sessions = append(c.Sessions, names[:min(len(names), 20-min(len(c.Sessions), 20))]...)
}

func (a *Agent) serverCopiesNotice() *ServerCopies {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.serverCopies == nil {
		return nil
	}
	v := *a.serverCopies
	v.Sessions = slices.Clone(v.Sessions)
	return &v
}

// FetchAdminRules returns a Config.AdminRules that reads the server's
// path rules and unplaceable floor (GET /v1/policy) with the device's
// credential, over hc, the configured client (it carries the TLS pin).
func FetchAdminRules(server, token string, hc *http.Client) func(context.Context) (AdminPolicy, error) {
	if hc == nil {
		// The caller's client carries the TLS pin; a default one would
		// bypass it.
		panic("agent: FetchAdminRules needs the server's HTTP client")
	}
	url := strings.TrimRight(server, "/") + "/v1/policy"
	return func(ctx context.Context) (AdminPolicy, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return AdminPolicy{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := hc.Do(req)
		if err != nil {
			return AdminPolicy{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return AdminPolicy{}, fmt.Errorf("GET /v1/policy: %s", resp.Status)
		}
		var body struct {
			Rules       []string `json:"path_rules"`
			Unplaceable string   `json:"unplaceable"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
			return AdminPolicy{}, fmt.Errorf("GET /v1/policy: %w", err)
		}
		if body.Rules == nil {
			body.Rules = []string{}
		}
		return AdminPolicy{Rules: body.Rules, Unplaceable: body.Unplaceable}, nil
	}
}

// defaultHome is the home directory "~" in rules stands for.
func defaultHome() string {
	h, _ := os.UserHomeDir()
	return filepath.Clean(h)
}

// tightening is what recordCwds found: the verdict a directory a
// transcript's new lines named moved its session to.
type tightening struct {
	pv       *policyView
	was, now pathpolicy.Decision
	name     string // agent:session (rule), for logs and the status
	withhold bool   // a server deletion is owed (placements.withhold)
}

// recordCwds adds the directories a transcript's new lines named (cwds)
// to its session's placement set, and to its Claude session's when it is
// a subagent's. It runs before the parse's watermark is saved. When a
// directory tightens the rules' verdict it returns what tighten applies
// once the parse's rows are saved, and, when the session was uploadable
// before and sync is on, records a server deletion owed for the session
// (its Claude session for a subagent), so a crash cannot lose it.
func (a *Agent) recordCwds(ctx context.Context, t *target, cwds []string) (*tightening, error) {
	if len(cwds) == 0 || t.kind != kindTranscript {
		return nil, nil
	}
	a.mu.Lock()
	key, path := t.placeKeyOf()
	root := t.root
	a.mu.Unlock()
	pv := a.policy()
	was, wasKnown, _ := a.decisionOf(pv, t)
	_, grew := a.addCwds(key, path, cwds)
	owner := key
	if root != "" {
		// The directories a subagent named are its session's too. Its
		// transcript places it only when never placed (addCwds).
		rootKey := placeKey{transcript.AgentClaude, root}
		if _, ok := a.storedPlace(rootKey); ok {
			_, g := a.addCwds(rootKey, "", cwds)
			grew = grew || g
			owner = rootKey // the server deletion cascades to subagents
		}
	}
	if !grew || pv.pol.Empty() {
		return nil, nil
	}
	d, known, _ := a.decisionOf(pv, t)
	if !known || d.Mode <= was.Mode {
		return nil, nil
	}
	tt := &tightening{pv: pv, was: was, now: d, name: fmt.Sprintf("%s:%s (%s)", key.agent, key.session, d.Reason())}
	// Only an allowed session was handed to sync; one never placed before
	// was not (uploadBound refuses it).
	if wasKnown && was.Mode == pathpolicy.Allow && a.cfg.Sync != nil && a.cfg.Withhold != nil {
		if err := a.store.SetWithhold(ctx, owner.agent, owner.session, d.Mode.String(), d.Reason()); err != nil {
			return nil, err
		}
		tt.withhold = true
	}
	return tt, nil
}

// tighten applies a tightened verdict (recordCwds) after the parse saved
// its rows: cached verdicts are dropped (enforce), so nothing more of the
// session is handed to sync; a deny also purges the transcript's rows
// (source sourceID), and denied reports that. The server is asked to
// delete what it holds of the session (sendWithholds); without a way to
// ask, the copies stay there and the status says so.
func (a *Agent) tighten(ctx context.Context, t *target, sourceID int64, tt *tightening) (denied bool, err error) {
	if tt == nil {
		return false, nil
	}
	d := tt.now
	a.log.Warn("agent: path rules: a directory the session named later tightens its verdict; no further upload",
		"session", tt.name, "from", tt.was.Mode.String(), "to", d.Mode.String())
	a.polMu.Lock()
	a.enforce(ctx, a.policy().pol)
	a.polMu.Unlock()
	if d.Mode == pathpolicy.Deny && sourceID != 0 {
		// enforce purged the session's conversations by session id; this
		// transcript's rows go by source too.
		if err := a.purgeSource(ctx, sourceID, d); err != nil {
			return true, err
		}
	}
	switch {
	case tt.withhold:
		a.log.Warn("agent: path rules: asking the server to delete what was uploaded of this session", "session", tt.name)
		a.kickWithholds(ctx)
	case a.cfg.Sync != nil && tt.was.Mode == pathpolicy.Allow:
		a.noteServerCopies(1, []string{tt.name})
		a.log.Warn("agent: path rules: what was uploaded of this session before stays on the server; " +
			"the owner or an admin must delete it there")
	}
	return d.Mode == pathpolicy.Deny, nil
}

// kickWithholds sends the server deletions owed (sendWithholds): in the
// background while Run runs, else now.
func (a *Agent) kickWithholds(ctx context.Context) {
	if a.cfg.Withhold == nil {
		return
	}
	a.mu.Lock()
	bg := a.bgOn
	if bg {
		a.bgWG.Add(1)
	}
	a.mu.Unlock()
	if !bg {
		a.sendWithholds(ctx)
		return
	}
	go func() {
		defer a.bgWG.Done()
		a.sendWithholds(ctx)
	}()
}

// sendWithholds asks the server to delete each session a deletion is
// owed for (placements.withhold), and clears what it answered. One that
// fails stays owed: it is sent again at the next admin refresh and at
// start.
func (a *Agent) sendWithholds(ctx context.Context) {
	if a.cfg.Withhold == nil || !a.withholdMu.TryLock() {
		return
	}
	defer a.withholdMu.Unlock()
	ws, err := a.store.Withholds(ctx)
	if err != nil {
		a.log.Warn("agent: reading the server deletions owed", "err", err)
		return
	}
	for _, w := range ws {
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := a.cfg.Withhold(wctx, w)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				a.log.Warn("agent: path rules: the server did not delete a withheld session; will retry",
					"session", string(w.Agent)+":"+w.SessionID, "err", err)
			}
			continue
		}
		if err := a.store.SetWithhold(ctx, w.Agent, w.SessionID, "", ""); err != nil {
			a.log.Warn("agent: clearing a server deletion owed", "err", err)
			continue
		}
		a.log.Info("agent: path rules: the server deleted what was uploaded of a withheld session",
			"session", string(w.Agent)+":"+w.SessionID, "mode", w.Mode, "rule", w.Rule)
	}
}

// WithholdSession returns a Config.Withhold that asks the server to delete
// a session of this member's (POST /v1/conversations/withhold) with the
// device's credential over hc, the configured client (it carries the TLS
// pin). Only acceptance acknowledges a durable tombstone. An older server
// returning 404 leaves the deletion owed for retry.
func WithholdSession(server, token string, hc *http.Client) func(context.Context, localindex.Withhold) error {
	if hc == nil {
		panic("agent: WithholdSession needs the server's HTTP client")
	}
	url := strings.TrimRight(server, "/") + "/v1/conversations/withhold"
	return func(ctx context.Context, w localindex.Withhold) error {
		body, _ := json.Marshal(map[string]string{"agent": string(w.Agent), "session_id": w.SessionID, "mode": w.Mode, "rule": w.Rule})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		switch resp.StatusCode {
		case http.StatusAccepted:
			return nil
		}
		return fmt.Errorf("POST /v1/conversations/withhold: %s", resp.Status)
	}
}

// uploadBound is the sync scheduler's bound (devicesync.Scheduler.SetBound):
// while path rules are in force, sync reads a transcript only as far as it
// was parsed, so a directory a later line names is placed (widen) before
// those bytes can leave the device. A transcript not parsed by the current
// parser yet, or whose verdict is not allow, is not read at all (-1): it
// is handed to sync again once parsed.
func (a *Agent) uploadBound(spec devicesync.SourceSpec) (int64, bool) {
	if spec.Export || a.policy().pol.Empty() {
		return 0, false
	}
	a.mu.Lock()
	t := a.targets[spec.Path]
	var scanned int64 = -1
	if t != nil && t.kind == kindTranscript {
		if t.parser != nil && transcript.ReparseKey(t.indexedWith) == indexingVersion(t.parser) {
			scanned = t.scanned
		}
	}
	a.mu.Unlock()
	if t == nil || t.kind != kindTranscript {
		return 0, false
	}
	// Read after scanned: a parse that tightened the verdict did so before
	// it advanced scanned.
	if !a.uploadable(t) {
		return -1, true
	}
	return scanned, true
}
