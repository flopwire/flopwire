package digest

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript"
)

// How a session's commits are found (issue #80). A commit counts from the
// output of the call that made it, or, when that output names no sha
// (git commit -q), from a later command whose output shows HEAD:
//
//   - git commit, cherry-pick and revert print "[branch sha] subject" once
//     the commit exists: the sha counts, even when a later command of the
//     call failed.
//   - A successful git commit whose output prints no sha opens a window.
//     The next git rev-parse HEAD, git log (HEAD's entry only) or git show
//     of HEAD in the same directory, or a git push line for the commit's
//     branch, gives its sha. Any other command that moves HEAD (commit,
//     merge, rebase, reset, checkout, switch, pull, am, cherry-pick,
//     revert, gh pr checkout) closes the window first.
//   - A commit whose sha never shows is kept in CommitsNoSHA with its
//     subject, branch and time. No sha is made up.
//
// A call "succeeded" when the harness says so (no is_error, exit code 0)
// and the git commit is followed only by "&&" in its command line, so a
// later command's exit status cannot hide its failure. A commit in a
// directory outside the session's repo and cwd does not count.

// maxNoSHA bounds CommitsNoSHA; noSHASubject bounds a subject.
const (
	maxNoSHA     = 5
	noSHASubject = 80
)

// Reveal kinds: how a command's output shows HEAD.
const (
	revRevParse = "rev"  // git rev-parse HEAD: a bare sha
	revOne      = "one"  // git log -1 / git show HEAD: HEAD's entry only
	revLog      = "log"  // git log of HEAD, several entries: the first is HEAD
	revPush     = "push" // git push: "old..new src -> dst"
)

// gitCall is what one shell call says about commits, read from its
// command line.
type gitCall struct {
	printed bool   // a commit, cherry-pick or revert: its output may print "[branch sha]"
	pwhere  string // where the first of those ran
	commit  bool   // a git commit that, run to success, made a commit
	sure    bool   // the call's success implies the commit's
	subject string
	where   string // the directory the commit ran in ("" for the session's)
	mover   bool   // the line moves HEAD (any commit, merge, rebase, ...)
	// commitLast is set when the line's last HEAD move is its commit.
	commitLast bool
	// reveal is how the output shows HEAD after the line ran, revWhere
	// where, self whether that HEAD is this call's own commit (else the
	// one made before the call), revLog for a multi-entry log.
	reveal   string
	revWhere string
	self     bool
	logStyle string // "line" (sha first on the line) or "header" ("commit sha")
}

// readGit reads the git commands of one command line. base is the
// directory it starts in ("" for the session's). ok is false when the line
// does not parse.
func readGit(line, base string) (gitCall, bool) {
	segs, ok := splitShell(line)
	if !ok {
		return gitCall{}, false
	}
	var g gitCall
	where := base
	commitAt, lastMover := -1, -1
	for i, s := range segs {
		w := stripPrefix(s.words)
		if len(w) == 0 {
			continue
		}
		if w[0] == "cd" || w[0] == "pushd" {
			t := "~"
			if len(w) > 1 {
				t = w[1]
			}
			where = joinDir(where, t)
			continue
		}
		if w[0] == "gh" && len(w) >= 3 && w[1] == "pr" && w[2] == "checkout" {
			g.mover, lastMover = true, i
			continue
		}
		if w[0] != "git" && !strings.HasSuffix(w[0], "/git") {
			continue
		}
		dir, sub, args := gitArgs(w[1:])
		at := where
		if dir != "" {
			at = joinDir(where, dir)
		}
		switch sub {
		case "commit":
			c, ok := readCommit(args, s.heredoc)
			if !ok {
				continue
			}
			if !g.printed {
				g.pwhere = at
			}
			g.printed, g.mover, lastMover = true, true, i
			if commitAt < 0 {
				g.commit, g.subject, g.where, commitAt = true, c, at, i
				g.sure = andToEnd(segs, i)
			}
		case "cherry-pick", "revert":
			if hasAny(args, "-n", "--no-commit", "--abort", "--quit") {
				if !hasAny(args, "-n", "--no-commit") {
					g.mover, lastMover = true, i
				}
				continue
			}
			if !g.printed {
				g.pwhere = at
			}
			g.printed, g.mover, lastMover = true, true, i
		case "merge", "rebase", "reset", "checkout", "switch", "pull", "am", "update-ref":
			if sub == "checkout" && slices.Contains(args, "--") && !slices.ContainsFunc(args[:slices.Index(args, "--")], isRev) {
				continue // git checkout -- path: HEAD stays
			}
			g.mover, lastMover = true, i
		case "rev-parse", "log", "show", "push":
			kind, style := revealOf(sub, args)
			if kind == "" {
				continue
			}
			last := i == len(segs)-1
			if kind == revLog && len(nonCd(segs)) != 1 {
				continue // several entries: only alone is the first one HEAD's
			}
			if kind != revPush && !last {
				continue // its output must end the call's output
			}
			switch {
			case lastMover < 0:
				g.self = false
			case lastMover == commitAt && at == g.where:
				g.self = true
			default:
				continue // something else moved HEAD first
			}
			if kind != revPush && (commitAt >= 0 && !andToEnd(segs, commitAt) || commitAt < 0 && !andToEnd(segs, i)) {
				continue // the output may be HEAD's after a failed commit
			}
			g.reveal, g.revWhere, g.logStyle = kind, at, style
		}
	}
	g.commitLast = commitAt >= 0 && lastMover == commitAt
	return g, true
}

// stripPrefix drops leading variable assignments and command prefixes
// (env and its options, command, time, nohup, sudo).
func stripPrefix(w []string) []string {
	for len(w) > 0 {
		switch {
		case w[0] == "env":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				if (w[0] == "-u" || w[0] == "-C" || w[0] == "-S") && len(w) > 1 {
					w = w[1:]
				}
				w = w[1:]
			}
		case w[0] == "command" || w[0] == "time" || w[0] == "nohup" || w[0] == "sudo" || w[0] == "exec":
			w = w[1:]
		case assignment.MatchString(w[0]):
			w = w[1:]
		default:
			return w
		}
	}
	return w
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// gitArgs splits git's global options from its subcommand: the -C
// directory (joined when repeated), the subcommand and its arguments.
func gitArgs(w []string) (dir, sub string, args []string) {
	for i := 0; i < len(w); i++ {
		a := w[i]
		switch {
		case a == "-C" && i+1 < len(w):
			i++
			dir = joinDir(dir, w[i])
		case a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace" || a == "--exec-path":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return dir, a, w[i+1:]
		}
	}
	return dir, "", nil
}

// joinDir is directory t entered from base. "" stands for the session's
// directory; a target that needs expansion ($X, ~) stays as written.
func joinDir(base, t string) string {
	switch {
	case t == "" || t == ".":
		return base
	case strings.HasPrefix(t, "/"):
		return path.Clean(t)
	case strings.ContainsAny(t, "$~`"):
		return t
	case base == "":
		return path.Clean(t)
	case strings.ContainsAny(base, "$~`"):
		return base + "/" + t
	}
	return path.Join(base, t)
}

// andToEnd reports whether every command after segs[i] is joined by "&&"
// and none before it by "||" or "&": the line's success implies segs[i]'s.
func andToEnd(segs []seg, i int) bool {
	for j, s := range segs {
		if j < i && (s.op == "||" || s.op == "&") {
			return false
		}
		if j >= i && j < len(segs)-1 && s.op != "&&" {
			return false
		}
	}
	return true
}

func nonCd(segs []seg) []seg {
	var out []seg
	for _, s := range segs {
		if w := stripPrefix(s.words); len(w) > 0 && w[0] != "cd" {
			out = append(out, s)
		}
	}
	return out
}

func hasAny(args []string, opts ...string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return slices.Contains(opts, a) })
}

func isRev(a string) bool { return a != "" && !strings.HasPrefix(a, "-") }

// commitValue are git commit's options that take the next word as their
// value.
var commitValue = map[string]bool{"-m": true, "--message": true, "-F": true, "--file": true, "-C": true, "--reuse-message": true,
	"-c": true, "--reedit-message": true, "--author": true, "--date": true, "-t": true, "--template": true, "--cleanup": true,
	"--trailer": true, "--fixup": true, "--squash": true, "--pathspec-from-file": true}

// readCommit reads git commit's arguments: false for one that makes no
// commit (--dry-run, --help), else the subject of its message when the
// line gives it (-m, or a here-document).
func readCommit(args []string, heredoc string) (string, bool) {
	var msg, file string
	have := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--dry-run" || a == "--help" || a == "-h":
			return "", false
		case a == "--":
			i = len(args)
		case strings.HasPrefix(a, "--"):
			k, v, eq := strings.Cut(a, "=")
			if !eq && commitValue[k] && i+1 < len(args) {
				i++
				v = args[i]
			}
			switch k {
			case "--message":
				if !have {
					msg, have = v, true
				}
			case "--file":
				file = v
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// A cluster of short options: -qam "msg", -m"msg".
			for j := 1; j < len(a); j++ {
				f := a[j]
				if strings.IndexByte("mFCct", f) < 0 {
					continue
				}
				v := a[j+1:]
				if v == "" && i+1 < len(args) {
					i++
					v = args[i]
				}
				switch f {
				case 'm':
					if !have {
						msg, have = v, true
					}
				case 'F':
					file = v
				}
				break
			}
		}
	}
	switch {
	case have:
		return subjectOf(msg), true
	case file == "-" && heredoc != "":
		return subjectOf(heredoc), true
	}
	return "", true
}

// catHeredoc is a message written "$(cat <<'EOF' … EOF)".
var catHeredoc = regexp.MustCompile(`^\$\(\s*cat\s*<<-?\s*['"]?\w+['"]?\s*\n`)

// subjectOf is the first non-empty line of a commit message, clipped. A
// message that is a command substitution other than a cat of a
// here-document has no subject the line shows.
func subjectOf(msg string) string {
	if loc := catHeredoc.FindStringIndex(msg); loc != nil {
		msg = msg[loc[1]:]
	} else if strings.Contains(msg, "$(") || strings.Contains(msg, "`") {
		return ""
	}
	for _, l := range strings.Split(msg, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return clip(oneLine(l), noSHASubject)
		}
	}
	return ""
}

// logOK are the git log and git show options that keep HEAD's entry first
// and its sha first on the entry's line (or on its "commit" line).
var logOK = map[string]bool{"--oneline": true, "--decorate": true, "--no-decorate": true, "--stat": true, "--shortstat": true,
	"--numstat": true, "--name-only": true, "--name-status": true, "-p": true, "--patch": true, "-s": true, "--no-patch": true,
	"--abbrev-commit": true, "--no-abbrev": true, "--no-color": true, "--color": true, "--graph": true, "--first-parent": true,
	"--no-merges": false, "--show-signature": true, "--no-notes": true, "--quiet": true, "-q": true}

// revealOf says how a git rev-parse, log, show or push shows HEAD: its
// reveal kind ("" for none) and, for log and show, where the sha sits.
func revealOf(sub string, args []string) (kind, style string) {
	switch sub {
	case "push":
		if hasAny(args, "--delete", "-d", "--tags", "--all", "--mirror", "--help") {
			return "", ""
		}
		return revPush, ""
	case "rev-parse":
		var revs []string
		for _, a := range args {
			switch {
			case a == "--short" || strings.HasPrefix(a, "--short=") || a == "--verify" || a == "-q" || a == "--quiet":
			case strings.HasPrefix(a, "-"):
				return "", ""
			default:
				revs = append(revs, a)
			}
		}
		if len(revs) == 1 && (revs[0] == "HEAD" || revs[0] == "@" || revs[0] == "HEAD^{commit}") {
			return revRevParse, ""
		}
		return "", ""
	}
	count := -1
	style = "header"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "HEAD" || a == "@":
		case a == "-1" || a == "-n1" || a == "--max-count=1" || a == "-n=1":
			count = 1
		case (a == "-n" || a == "--max-count") && i+1 < len(args):
			i++
			if args[i] == "1" {
				count = 1
			} else {
				count = 2
			}
		case logCount.MatchString(a):
			count = 2
		case a == "--oneline":
			style = "line"
		case strings.HasPrefix(a, "--format=") || strings.HasPrefix(a, "--pretty=") || a == "--format" || a == "--pretty":
			v := ""
			if _, x, ok := strings.Cut(a, "="); ok {
				v = x
			} else if i+1 < len(args) {
				i++
				v = args[i]
			}
			s, ok := formatStyle(v)
			if !ok {
				return "", ""
			}
			style = s
		case strings.HasPrefix(a, "--decorate=") || strings.HasPrefix(a, "--abbrev=") || strings.HasPrefix(a, "--color=") ||
			strings.HasPrefix(a, "--date="):
		case logOK[a]:
		default:
			return "", "" // a revision, a path or a filter: the first entry may not be HEAD's
		}
	}
	if sub == "show" || count == 1 {
		return revOne, style
	}
	return revLog, style
}

// formatStyle is where a --format or --pretty value puts the sha: "line"
// when each entry starts with it, "header" for "commit sha" lines; false
// when it shows none first.
func formatStyle(v string) (string, bool) {
	switch v {
	case "oneline", "reference":
		return "line", true
	case "short", "medium", "full", "fuller", "raw":
		return "header", true
	}
	v = strings.TrimPrefix(strings.TrimPrefix(v, "tformat:"), "format:")
	if strings.HasPrefix(v, "%H") || strings.HasPrefix(v, "%h") {
		return "line", true
	}
	return "", false
}

var logCount = regexp.MustCompile(`^-(n=?)?[0-9]+$|^--max-count=[0-9]+$`)

var (
	shaLine    = regexp.MustCompile(`(?m)^[*|\\/ ]*([0-9a-f]{7,40})(?:[ \t]|$)`)
	shaHeader  = regexp.MustCompile(`(?m)^[*|\\/ ]*commit ([0-9a-f]{7,40})\b`)
	shaBare    = regexp.MustCompile(`(?m)^([0-9a-f]{7,40})\s*$`)
	pushUpdate = regexp.MustCompile(`(?m)^\s*[+ ]?\s*[0-9a-f]{7,40}\.\.\.?([0-9a-f]{7,40})\s+(\S+)\s+->\s+\S+`)
)

// revealed is the sha a reveal's output shows for HEAD ("" for none).
// branch is the commit's branch, which a push line must name (or HEAD).
func revealed(kind, style, out, branch string) string {
	var all [][]string
	switch kind {
	case revRevParse:
		all = shaBare.FindAllStringSubmatch(out, -1)
	case revPush:
		for _, m := range pushUpdate.FindAllStringSubmatch(out, -1) {
			if m[2] == "HEAD" || branch != "" && m[2] == branch {
				return m[1]
			}
		}
		return ""
	default:
		re := shaLine
		if style == "header" {
			re = shaHeader
		}
		all = re.FindAllStringSubmatch(out, -1)
		if kind == revLog && len(all) > 0 {
			return all[0][1]
		}
	}
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1] // the output of the line's last command
}

// shellOut is a shell result's output text and whether it shows a command
// failing: a Codex exec script's JSON chunks ({"exit_code":N,"output":…})
// are decoded, Devin's "Output from command in shell" header and "Exit
// code: N" trailer are dropped.
func shellOut(text string) (string, bool) {
	failed := false
	if strings.Contains(text, `"exit_code":`) {
		// A chunk the text cap cut does not decode: its line stays as is.
		var outs []string
		decoded := false
		for _, l := range strings.Split(text, "\n") {
			var c struct {
				Exit   *int    `json:"exit_code"`
				Output *string `json:"output"`
			}
			if !strings.HasPrefix(l, "{") || !strings.Contains(l, `"exit_code":`) || json.Unmarshal([]byte(l), &c) != nil || c.Output == nil {
				outs = append(outs, l)
				continue
			}
			if c.Exit != nil && *c.Exit != 0 {
				failed = true
			}
			outs, decoded = append(outs, *c.Output), true
		}
		if decoded {
			return strings.Join(outs, "\n"), failed
		}
	}
	if strings.HasPrefix(text, "Output from command in shell ") {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		t := strings.TrimRight(text, "\n ")
		if i := strings.LastIndexByte(t, '\n'); i >= 0 && strings.HasPrefix(t[i+1:], "Exit code: ") {
			failed = failed || strings.TrimSpace(t[i+len("\nExit code: "):]) != "0"
			text = t[:i]
		}
	}
	return text, failed
}

// shellCmd is one command line a tool call ran, with the directory it
// ran in ("" for the session's) and its exit code when the harness
// recorded it (Codex events, Devin).
type shellCmd struct {
	line string
	cwd  string
	exit *int64
}

// shellCmds are a shell tool call's command lines: the ones its
// enrichment records, with their directories and exit codes, else its
// arguments' command.
func shellCmds(m *transcript.Message) []shellCmd {
	if v, ok := m.Enrichment["commands"]; ok {
		if b, err := json.Marshal(v); err == nil {
			var cs []struct {
				Cmd  string   `json:"cmd"`
				Cwd  string   `json:"cwd"`
				Exit *float64 `json:"exit_code"`
			}
			if json.Unmarshal(b, &cs) == nil {
				var out []shellCmd
				for _, c := range cs {
					if c.Cmd == "" {
						continue
					}
					sc := shellCmd{line: unwrapShell(c.Cmd), cwd: c.Cwd}
					if c.Exit != nil {
						e := int64(*c.Exit)
						sc.exit = &e
					}
					out = append(out, sc)
				}
				if len(out) > 0 {
					return out
				}
			}
		}
	}
	args := jsonArgs(m.Text)
	if args == nil {
		return scriptCmds(m.Text)
	}
	for _, k := range []string{"command", "cmd"} {
		if s := argString(args[k]); s != "" {
			cwd := argString(args["workdir"])
			if cwd == "" {
				cwd = argString(args["cwd"])
			}
			return []shellCmd{{line: unwrapShell(s), cwd: cwd}}
		}
	}
	return nil
}

// execCall is a Codex exec script's call of exec_command: its cmd (a JS
// string literal, which JSON decodes) and, when given, its workdir.
var execCall = regexp.MustCompile(`exec_command\(\{\s*cmd:\s*("(?:[^"\\]|\\.)*")(?:\s*,\s*"?workdir"?:\s*("(?:[^"\\]|\\.)*"))?`)

// scriptCmds are the commands a Codex exec script runs, read from its
// text, for a call whose command events are missing.
func scriptCmds(text string) []shellCmd {
	if !strings.Contains(text, "exec_command(") {
		return nil
	}
	var out []shellCmd
	for _, m := range execCall.FindAllStringSubmatch(text, -1) {
		var c shellCmd
		if json.Unmarshal([]byte(m[1]), &c.line) != nil {
			return nil // a literal JSON does not read: trust none of it
		}
		if m[2] != "" {
			_ = json.Unmarshal([]byte(m[2]), &c.cwd)
		}
		out = append(out, c)
	}
	return out
}

// Success of a commit or a reveal, as a call's command lines say it.
const (
	okNone   = 0 // no such command, or its success cannot be told
	okResult = 1 // it succeeded if the call did
	okExit   = 2 // its exit code was 0
)

// callGit is gitCall over all of a call's command lines, in order (a
// Codex exec script runs several, each with its exit code).
type callGit struct {
	printed  bool
	pwhere   string
	commit   int // okNone, okResult, okExit
	subject  string
	where    string
	mover    bool
	reveal   string
	style    string
	revWhere string
	self     bool
	revOK    int
}

// readCall reads a shell call's command lines. ok is false when one does
// not parse.
func readCall(cmds []shellCmd) (callGit, bool) {
	var c callGit
	commitLine, moverLine := -1, -1
	commitLast := false // the commit line's last HEAD move was its commit
	for k, sc := range cmds {
		g, ok := readGit(sc.line, sc.cwd)
		if !ok {
			return callGit{}, false
		}
		known := sc.exit != nil
		zero := known && *sc.exit == 0
		if g.printed && !c.printed {
			c.printed, c.pwhere = true, g.pwhere
		}
		if g.reveal != "" && (k == len(cmds)-1 || g.reveal == revPush) {
			self, use := g.self, true
			switch {
			case g.self:
			case moverLine < 0:
			case moverLine == commitLine && commitLast && c.commit == okExit && g.revWhere == c.where:
				self = true
			default:
				use = false
			}
			if use {
				c.reveal, c.style, c.revWhere, c.self = g.reveal, g.logStyle, g.revWhere, self
				switch {
				case g.reveal == revPush || zero:
					c.revOK = okExit
				case !known:
					c.revOK = okResult
				default:
					c.revOK = okNone
				}
			}
		}
		if g.commit && commitLine < 0 {
			commitLine, c.subject, c.where = k, g.subject, g.where
			switch {
			case !g.sure:
			case zero:
				c.commit = okExit
			case !known && len(cmds) == 1:
				c.commit = okResult
			}
		}
		if g.mover {
			c.mover, moverLine = true, k
			commitLast = k == commitLine && g.commitLast
		}
	}
	if c.reveal != "" && c.self && c.commit == okNone {
		c.reveal = ""
	}
	return c, true
}
