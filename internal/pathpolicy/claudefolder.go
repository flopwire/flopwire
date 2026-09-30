package pathpolicy

import (
	"regexp"
	"strings"
)

// ClaudeFolderName is how Claude Code names a project folder
// (~/.claude/projects/<name>) for a directory: every UTF-16 code unit
// other than an ASCII letter or digit becomes '-'.
func ClaudeFolderName(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9':
			b.WriteRune(c)
		case c > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// DecideFolder tightens d with the path rules that could cover a session
// placed by its Claude project folder name (any case). The name is lossy
// ('/', '.', '-' ... all became '-'), so a directory that is gone, or two
// that encode alike, may decode to a path no rule names: a path rule
// covers the session when its pattern, encoded the same way, could have
// produced the name.
func (p Policy) DecideFolder(d Decision, folder string) Decision {
	folder = strings.ToLower(folder)
	for _, set := range []struct {
		rules []Rule
		admin bool
	}{{p.Admin, true}, {p.User, false}} {
		for _, r := range set.rules {
			if !r.Repo && r.Mode > d.Mode && MatchClaudeFolder(r.Pattern, folder) {
				d = Decision{Mode: r.Mode, Rule: r, Admin: set.admin}
			}
		}
	}
	return d
}

// MatchClaudeFolder reports whether a path pattern (normalized) could
// cover a directory whose Claude folder name is folder (lowercase). It
// errs towards a match: '-' in the name may be any separator.
func MatchClaudeFolder(pattern, folder string) bool {
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return false
	}
	segs := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	if !strings.HasPrefix(pattern, "/") {
		segs = append([]string{"**"}, segs...)
	}
	var re strings.Builder
	re.WriteString("^")
	for _, seg := range segs {
		if seg == "**" {
			re.WriteString("(?:-.*)?")
			continue
		}
		re.WriteString("-")
		rs := []rune(seg)
		for i := 0; i < len(rs); i++ {
			switch c := rs[i]; {
			case c == '*':
				re.WriteString("[a-z0-9-]*")
			case c == '?':
				re.WriteString("(?:[a-z0-9-]|--)")
			case c == '[':
				j := i + 2 // a class holds at least one character
				for j < len(rs) && rs[j] != ']' {
					j++
				}
				if j >= len(rs) {
					re.WriteString("-") // not a class: a literal '['
					continue
				}
				re.WriteString("(?:[a-z0-9-]|--)")
				i = j
			default:
				re.WriteString(strings.ToLower(ClaudeFolderName(string(c))))
			}
		}
	}
	re.WriteString("(?:-.*)?$")
	m, err := regexp.MatchString(re.String(), folder)
	return err == nil && m
}
