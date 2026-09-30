// Package pathpolicy defines path rules (decision D18): which sessions a
// device indexes and uploads, by where the session ran (its Placement:
// working directory, git worktree, main checkout and remote).
//
// A rule is a mode and a glob:
//
//	deny  ~/personal                  neither index nor upload
//	local ~/clients/acme/**           index locally, never upload
//	allow /work/**                    the default; changes nothing
//	deny  repo:github.com/acme/*      by the repository's origin remote
//
// A rule is written "MODE PATTERN" or "MODE:PATTERN"; a bare pattern is a
// deny rule. A path pattern is a glob over '/'-separated segments ('*' and
// '?' within a segment, '**' across segments) and covers the directory it
// names and everything below it. A path pattern that does not start with
// '/' or '~' matches at any depth ("secret-client" is "**/secret-client").
// Matching is lexical and case-insensitive, with '\' read as '/'. A path
// pattern is checked against the session's working directory, its
// worktree root and its main checkout root.
//
// A "repo:" pattern is checked against the normalized origin remote
// (host/owner/name, no scheme, user or ".git"): "repo:github.com/acme/*",
// or a URL, which is normalized the same way. It is anchored at the host
// and covers everything below it.
//
// The most restrictive matching rule wins (deny over local over allow).
// Admin rules come from the server and user rules from the device; since
// both apply and the most restrictive wins, admin rules are a floor a user
// can tighten but never loosen. A session with no placement at all is
// handled by the Unplaceable mode instead of the rules.
package pathpolicy

import (
	"fmt"
	"path"
	"strings"

	"github.com/flopwire/flopwire/internal/provenance"
)

// Mode is what a rule does with a matching session. Larger is more
// restrictive.
type Mode uint8

const (
	Allow Mode = iota // index and upload (default)
	Local             // index locally, never upload
	Deny              // neither index nor upload
)

var modeNames = [...]string{"allow", "local", "deny"}

func (m Mode) String() string {
	if int(m) < len(modeNames) {
		return modeNames[m]
	}
	return fmt.Sprintf("mode(%d)", uint8(m))
}

// ParseMode parses "allow", "local" or "deny" (any case).
func ParseMode(s string) (Mode, bool) {
	for i, n := range modeNames {
		if strings.EqualFold(s, n) {
			return Mode(i), true
		}
	}
	return Allow, false
}

// Rule is one parsed rule. Pattern is normalized (Normalize, or
// NormalizeRepo for a repo rule).
type Rule struct {
	Mode    Mode
	Pattern string
	Repo    bool // Pattern matches the remote, not a path
}

// repoPrefix marks a rule that matches the origin remote.
const repoPrefix = "repo:"

// ParseRule parses "MODE PATTERN", "MODE:PATTERN" or a bare PATTERN
// (deny). It fails on an empty pattern.
func ParseRule(raw string) (Rule, error) {
	s := strings.TrimSpace(raw)
	r := Rule{Mode: Deny}
	if _, ok := ParseMode(strings.TrimSuffix(s, ":")); ok {
		return Rule{}, fmt.Errorf("pathpolicy: rule %q has no pattern", raw)
	}
	if i := strings.IndexAny(s, ": \t"); i > 0 {
		if m, ok := ParseMode(s[:i]); ok {
			r.Mode, s = m, strings.TrimSpace(s[i+1:])
		}
	}
	if len(s) >= len(repoPrefix) && strings.EqualFold(s[:len(repoPrefix)], repoPrefix) {
		r.Repo, r.Pattern = true, NormalizeRepo(s[len(repoPrefix):])
	} else {
		r.Pattern = Normalize(s)
	}
	if r.Pattern == "" {
		return Rule{}, fmt.Errorf("pathpolicy: rule %q has no pattern", raw)
	}
	return r, nil
}

// String renders the rule in its normalized form: a bare pattern for deny,
// "MODE:PATTERN" otherwise.
func (r Rule) String() string {
	p := r.Pattern
	if r.Repo {
		p = repoPrefix + p
	}
	if r.Mode == Deny {
		return p
	}
	return r.Mode.String() + ":" + p
}

// ParseRules parses every non-empty line of rules that is not a '#'
// comment. It returns the rules it could parse and an error naming each
// one it could not.
func ParseRules(lines []string) ([]Rule, error) {
	var out []Rule
	var bad []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		r, err := ParseRule(l)
		if err != nil {
			bad = append(bad, l)
			continue
		}
		out = append(out, r)
	}
	if len(bad) > 0 {
		return out, fmt.Errorf("pathpolicy: cannot parse rules %q", bad)
	}
	return out, nil
}

// Normalize converts both Unix and Windows separators to a stable,
// case-insensitive lexical form. A trailing separator is kept (it changes
// nothing when matching).
func Normalize(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	if value == "" {
		return ""
	}
	hadTrailingSeparator := strings.HasSuffix(value, "/")
	value = path.Clean(value)
	if value == "." {
		return ""
	}
	value = strings.ToLower(value)
	if hadTrailingSeparator && value != "/" {
		value += "/"
	}
	return value
}

// NormalizeRepo normalizes a repo pattern: a remote URL (https, ssh or
// scp-like) becomes host/owner/name as provenance.NormalizeRemote writes
// it; anything else is trimmed of slashes and a ".git" suffix. The result
// is lowercase.
func NormalizeRepo(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "://") || strings.Contains(value, "@") {
		if n, err := provenance.NormalizeRemote(value); err == nil {
			value = n
		}
	}
	value = strings.Trim(strings.TrimSuffix(strings.Trim(value, "/"), ".git"), "/")
	if value == "" {
		return ""
	}
	value = path.Clean(value)
	if value == "." || strings.HasPrefix(value, "..") {
		return ""
	}
	return strings.ToLower(value)
}

// NormalizeRules parses, normalizes and deduplicates rules, preserving
// the administrator's order. Unparseable rules are dropped.
func NormalizeRules(rules []string) []string {
	clean := make([]string, 0, len(rules))
	seen := make(map[string]struct{}, len(rules))
	for _, raw := range rules {
		r, err := ParseRule(raw)
		if err != nil {
			continue
		}
		s := r.String()
		if _, exists := seen[s]; exists {
			continue
		}
		seen[s] = struct{}{}
		clean = append(clean, s)
	}
	return clean
}

// ExpandHome returns the rule with a leading "~" in its pattern replaced
// by home. Rules are written once for every device; "~" is each device's
// own home directory.
func (r Rule) ExpandHome(home string) Rule {
	if home == "" || r.Pattern != "~" && !strings.HasPrefix(r.Pattern, "~/") {
		return r
	}
	r.Pattern = Normalize(home + r.Pattern[1:])
	return r
}

// Matches reports whether the rule covers p: p is the directory the
// pattern names, or below it. A repo rule never matches a path.
func (r Rule) Matches(p string) bool {
	return !r.Repo && Match(r.Pattern, p)
}

// MatchesRemote reports whether a repo rule covers the normalized remote.
func (r Rule) MatchesRemote(remote string) bool {
	return r.Repo && MatchRepo(r.Pattern, remote)
}

// MatchRepo reports whether a repo pattern covers remote (host/owner/name,
// any case): the pattern is anchored at the host and covers what is below
// it.
func MatchRepo(pattern, remote string) bool {
	pattern, remote = NormalizeRepo(pattern), NormalizeRepo(remote)
	if pattern == "" || remote == "" {
		return false
	}
	return matchSegs(append(strings.Split(pattern, "/"), "**"), strings.Split(remote, "/"))
}

// Match reports whether pattern covers p (see the package comment).
func Match(pattern, p string) bool {
	pattern = strings.TrimSuffix(Normalize(pattern), "/")
	p = strings.TrimSuffix(Normalize(p), "/")
	if pattern == "" || p == "" {
		return false
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = "**/" + pattern
	}
	segs := append(strings.Split(pattern, "/"), "**")
	return matchSegs(segs, strings.Split(p, "/"))
}

func matchSegs(pat, p []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true
			}
			for i := range len(p) + 1 {
				if matchSegs(pat[1:], p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], p[0]); err != nil || !ok {
			return false
		}
		pat, p = pat[1:], p[1:]
	}
	return len(p) == 0
}

// Placement is where a session ran. Every field may be empty.
type Placement struct {
	Cwd      string // the working directory
	Worktree string // the git worktree root holding Cwd
	Main     string // the main checkout root (for a bare repository, its directory)
	Remote   string // the origin remote, normalized (host/owner/name)
}

// Unplaced reports whether nothing about the session's place is known.
func (p Placement) Unplaced() bool {
	return p.Cwd == "" && p.Worktree == "" && p.Main == "" && p.Remote == ""
}

// Unplaceable names: what happens to a session with no placement.
var unplaceableNames = map[string]Mode{"upload": Allow, "local": Local, "exclude": Deny}

// DefaultUnplaceable is the user's unplaceable setting when none is set.
const DefaultUnplaceable = Local

// ParseUnplaceable parses an unplaceable setting: "upload", "local" or
// "exclude" (any case). "" is not a setting (ok false).
func ParseUnplaceable(s string) (Mode, bool) {
	m, ok := unplaceableNames[strings.ToLower(strings.TrimSpace(s))]
	return m, ok
}

// UnplaceableName is the setting name of m.
func UnplaceableName(m Mode) string {
	for n, v := range unplaceableNames {
		if v == m {
			return n
		}
	}
	return m.String()
}

// Policy is every rule that applies on a device.
type Policy struct {
	Admin []Rule // from the server: a floor
	User  []Rule // from this device
	// Unplaceable is the mode of a session with no placement: the most
	// restrictive of the user's setting (DefaultUnplaceable when unset)
	// and the admin's (no floor when unset). UnplaceableAdmin says the
	// admin setting decided.
	Unplaceable      Mode
	UnplaceableAdmin bool
}

// Empty reports whether the policy changes nothing: no rules, and
// sessions with no placement upload.
func (p Policy) Empty() bool { return len(p.Admin)+len(p.User) == 0 && p.Unplaceable == Allow }

// HasRules reports whether any rule is in force.
func (p Policy) HasRules() bool { return len(p.Admin)+len(p.User) > 0 }

// Decision is the outcome of a policy for a session.
type Decision struct {
	Mode        Mode
	Rule        Rule // the rule that decided; zero for the default
	Admin       bool // Rule (or the unplaceable setting) is the admin's
	Unplaceable bool // the session has no placement; Mode is the unplaceable setting
}

// Reason describes the decision for a log line.
func (d Decision) Reason() string {
	who := map[bool]string{true: "admin", false: "user"}[d.Admin]
	if d.Unplaceable {
		return fmt.Sprintf("%s unplaceable=%s", who, UnplaceableName(d.Mode))
	}
	return fmt.Sprintf("%s rule %q", who, d.Rule.String())
}

// Decide returns the most restrictive rule matching the placement: path
// rules against its working directory, worktree root and main checkout
// root, repo rules against its remote. On a tie an admin rule wins, so the
// reason names the floor. A placement with nothing known gets the
// Unplaceable mode.
func (p Policy) Decide(pl Placement) Decision {
	if pl.Unplaced() {
		return Decision{Mode: p.Unplaceable, Admin: p.UnplaceableAdmin, Unplaceable: true}
	}
	paths := [...]string{pl.Cwd, pl.Worktree, pl.Main}
	var d Decision
	found := false
	try := func(rules []Rule, admin bool) {
		for _, r := range rules {
			if found && r.Mode <= d.Mode {
				continue
			}
			hit := r.MatchesRemote(pl.Remote)
			for _, q := range paths {
				if hit {
					break
				}
				hit = q != "" && r.Matches(q)
			}
			if hit {
				d, found = Decision{Mode: r.Mode, Rule: r, Admin: admin}, true
			}
		}
	}
	try(p.Admin, true)
	try(p.User, false)
	return d
}

// Mode is Decide(pl).Mode.
func (p Policy) Mode(pl Placement) Mode { return p.Decide(pl).Mode }
