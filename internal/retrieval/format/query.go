package format

import (
	"strings"
)

// Query helpers shared by the local index and the server, so a filter or
// a query means the same on both.

// MaxRepoRoots bounds Filters.RepoRoots and Filters.RepoRemotes, so the
// filter fits a request line.
const MaxRepoRoots = 256

// FitRoots fits a repository's checkout roots to n: a root under another
// one adds nothing and goes first; then the last ones go, so the
// argument's own checkouts and the live worktrees, which come first,
// stay.
func FitRoots(roots []string, n int) []string {
	if len(roots) <= n {
		return roots
	}
	var out []string
	for _, r := range roots {
		under := false
		for _, o := range roots {
			if o != r && strings.HasPrefix(r, strings.TrimSuffix(o, "/")+"/") {
				under = true
				break
			}
		}
		if !under {
			out = append(out, r)
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// RepoMatch interprets a repo filter that is not a relative path (callers
// resolve "." and "./x" to absolute paths first): an absolute path matches
// that repo and every directory under it (prefix); a glob (* ? [) matches
// the repo root, or its last element when the glob has no "/"; a bare name
// matches the repo root's last element. like is a LIKE pattern with
// backslash escapes.
func RepoMatch(repo string) (prefix, like string) {
	switch {
	case repo == "":
		return "", ""
	case strings.ContainsAny(repo, "*?["):
		like = globLike(repo)
		if !strings.Contains(repo, "/") {
			like = "%/" + like
		}
		return "", like
	case strings.HasPrefix(repo, "/"):
		return strings.TrimSuffix(repo, "/"), ""
	}
	return "", "%/" + likeEscape(strings.TrimSuffix(repo, "/"))
}

// GlobLike is the LIKE pattern for a sessions glob: * and ? are wildcards,
// and a glob without any matches anywhere inside (as *glob*).
func GlobLike(glob string) string {
	if glob == "" {
		return ""
	}
	if !strings.ContainsAny(glob, "*?") {
		return "%" + likeEscape(glob) + "%"
	}
	return globLike(glob)
}

func globLike(glob string) string {
	var b strings.Builder
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		case '[', ']':
			// Character classes are not supported; match the characters.
			b.WriteRune(r)
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// SplitQuery separates a search query's "quoted phrases" from its words.
// An unclosed quote runs to the end.
func SplitQuery(q string) (words, phrases []string) {
	for {
		i := strings.IndexByte(q, '"')
		if i < 0 {
			break
		}
		words = append(words, strings.Fields(q[:i])...)
		rest := q[i+1:]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			j = len(rest)
		}
		if p := strings.Join(strings.Fields(rest[:j]), " "); p != "" {
			phrases = append(phrases, p)
		}
		if j == len(rest) {
			q = ""
			break
		}
		q = rest[j+1:]
	}
	words = append(words, strings.Fields(q)...)
	return words, phrases
}

// stopwords are left out of the any-term retry of a search that matched
// nothing with every term.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a about after all also an and any are as at be been but by can could did do does
		for from had has have how i if in into is it its just me my no not of on or our so some than that the their them
		then there these they this to too us was we were what when where which while who why will with would you your`) {
		stopwords[w] = true
	}
}

// DropStopwords returns words without common English words and trailing
// punctuation ("tokenizer?" becomes "tokenizer"); all of words when every
// one is a stopword.
func DropStopwords(words []string) []string {
	var out []string
	for _, w := range words {
		t := strings.TrimRight(w, "?!.,;:")
		if t == "" || stopwords[strings.ToLower(t)] {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return words
	}
	return out
}
