package digest

import "strings"

// seg is one simple command of a shell command line: its words with the
// quotes removed (nothing is expanded), the operator that joins it to the
// next ("&&", "||", ";", "|", "&"; "" for the last), and the body of the
// first here-document it reads.
type seg struct {
	words   []string
	op      string
	heredoc string
}

// splitShell splits a command line into simple commands. It knows quotes,
// backslashes, $(…) and `…` (kept whole inside a word), here-documents and
// comments; a newline, "(" and ")" separate like ";". It reports false
// when the line does not end where a command can (an open quote or
// substitution): the caller then trusts none of it.
func splitShell(s string) ([]seg, bool) {
	var (
		out      []seg
		cur      seg
		word     strings.Builder
		inWord   bool
		heredocs []hd // here-documents opened on this line, read at its end
	)
	flushWord := func() {
		if inWord {
			cur.words = append(cur.words, word.String())
			word.Reset()
			inWord = false
		}
	}
	end := func(op string) {
		flushWord()
		cur.op = op
		// An empty command ("a &&\nb", "( a )") keeps the operator before it.
		if len(cur.words) > 0 {
			out = append(out, cur)
		}
		cur = seg{}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			if i+1 < len(s) {
				i++
				if s[i] != '\n' {
					word.WriteByte(s[i])
					inWord = true
				}
			}
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			word.WriteString(s[i+1 : i+1+j])
			inWord = true
			i += j + 1
		case c == '"':
			j, ok := scanDouble(s, i+1, &word)
			if !ok {
				return nil, false
			}
			inWord = true
			i = j
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			j, ok := scanSubst(s, i+2)
			if !ok {
				return nil, false
			}
			word.WriteString(s[i : j+1])
			inWord = true
			i = j
		case c == '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j < 0 {
				return nil, false
			}
			word.WriteString(s[i : i+j+2])
			inWord = true
			i += j + 1
		case c == '#' && !inWord:
			for i+1 < len(s) && s[i+1] != '\n' {
				i++
			}
		case c == '<' && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<"):
			flushWord()
			j := i + 2
			strip := j < len(s) && s[j] == '-'
			if strip {
				j++
			}
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			var delim strings.Builder
			for j < len(s) && !strings.ContainsRune(" \t\n;&|<>()", rune(s[j])) {
				if s[j] != '\'' && s[j] != '"' && s[j] != '\\' {
					delim.WriteByte(s[j])
				}
				j++
			}
			heredocs = append(heredocs, hd{delim: delim.String(), strip: strip, seg: len(out)})
			i = j - 1
		case c == '\n':
			end(";")
			if len(heredocs) > 0 {
				i = readHeredocs(s, i+1, heredocs, out) - 1
				heredocs = nil
			}
		case c == ' ' || c == '\t' || c == '\r':
			flushWord()
		case c == ';' || c == '(' || c == ')':
			if c == ';' && i+1 < len(s) && s[i+1] == ';' {
				i++
			}
			end(";")
		case c == '&':
			switch {
			case i+1 < len(s) && s[i+1] == '&':
				end("&&")
				i++
			case inWord && strings.HasSuffix(word.String(), ">") || inWord && strings.HasSuffix(word.String(), "<"),
				i+1 < len(s) && s[i+1] == '>':
				word.WriteByte(c) // 2>&1, &>file
				inWord = true
			default:
				end("&")
			}
		case c == '|':
			if i+1 < len(s) && s[i+1] == '|' {
				end("||")
				i++
			} else {
				end("|")
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	end("")
	if len(heredocs) > 0 {
		readHeredocs(s, len(s), heredocs, out)
	}
	if n := len(out); n > 0 {
		out[n-1].op = ""
	}
	return out, true
}

// hd is a here-document opened on a line: its delimiter, whether "<<-"
// strips leading tabs, and the index the command reading it will have.
type hd struct {
	delim string
	strip bool
	seg   int
}

// readHeredocs reads the bodies of hs from s at i, each up to its
// delimiter line, into the commands that read them, and returns where the
// next line starts.
func readHeredocs(s string, i int, hs []hd, out []seg) int {
	for _, h := range hs {
		var body strings.Builder
		for i < len(s) {
			j := strings.IndexByte(s[i:], '\n')
			line := s[i:]
			if j >= 0 {
				line = s[i : i+j]
			}
			if j < 0 {
				i = len(s)
			} else {
				i += j + 1
			}
			cmp := line
			if h.strip {
				cmp = strings.TrimLeft(cmp, "\t")
			}
			if cmp == h.delim {
				break
			}
			body.WriteString(line)
			body.WriteByte('\n')
		}
		if h.seg < len(out) && out[h.seg].heredoc == "" {
			out[h.seg].heredoc = body.String()
		}
	}
	return i
}

// scanDouble reads a double-quoted string from s at i (after the quote)
// into w and returns the index of the closing quote. A $(…) inside is kept
// whole, quotes and all.
func scanDouble(s string, i int, w *strings.Builder) (int, bool) {
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return i, true
		case c == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0:
			i++
			if s[i] != '\n' {
				w.WriteByte(s[i])
			}
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			j, ok := scanSubst(s, i+2)
			if !ok {
				return 0, false
			}
			w.WriteString(s[i : j+1])
			i = j
		default:
			w.WriteByte(c)
		}
	}
	return 0, false
}

// scanSubst finds the ")" that closes a $( opened before s[i], past nested
// substitutions, quotes and here-documents, and returns its index.
func scanSubst(s string, i int) (int, bool) {
	depth := 1
	var pending []string // here-document delimiters to skip at the next newline
	var sink strings.Builder
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			i++
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return 0, false
			}
			i += j + 1
		case c == '"':
			j, ok := scanDouble(s, i+1, &sink)
			if !ok {
				return 0, false
			}
			sink.Reset()
			i = j
		case c == '<' && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<"):
			j := i + 2
			if j < len(s) && s[j] == '-' {
				j++
			}
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			var delim strings.Builder
			for j < len(s) && !strings.ContainsRune(" \t\n;&|<>()", rune(s[j])) {
				if s[j] != '\'' && s[j] != '"' && s[j] != '\\' {
					delim.WriteByte(s[j])
				}
				j++
			}
			pending = append(pending, delim.String())
			i = j - 1
		case c == '\n' && len(pending) > 0:
			i++
			for _, d := range pending {
				for i < len(s) {
					j := strings.IndexByte(s[i:], '\n')
					line := s[i:]
					if j >= 0 {
						line = s[i : i+j]
					}
					if j < 0 {
						i = len(s)
					} else {
						i += j + 1
					}
					if strings.TrimLeft(line, "\t") == d {
						break
					}
				}
			}
			pending = nil
			i--
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}
