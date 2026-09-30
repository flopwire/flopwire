package claude

import "strings"

// injectedTags are the blocks the harness writes into a user turn:
// <system-reminder> (CLAUDE.md, hook output, environment notes) and
// <task-notification> (background task results). Neither is typed by a
// person.
var injectedTags = []struct{ open, close string }{
	{"<system-reminder>", "</system-reminder>"},
	{"<task-notification>", "</task-notification>"},
}

// splitInjected separates the injected blocks (injectedTags) in a user text
// block from the text around them. prompt is the text with the blocks
// removed and trimmed ("" when nothing else is there); injected is the
// blocks, tags included, joined by blank lines ("" when there are none).
//
// The harness always writes a block closed and starting a line. An open tag
// in the middle of a line, or one never closed, is a person writing the tag
// name, so it stays in the prompt.
func splitInjected(text string) (prompt, injected string) {
	if !strings.Contains(text, "<system-reminder>") && !strings.Contains(text, "<task-notification>") {
		return text, ""
	}
	var p, inj strings.Builder
	rest := text
	for rest != "" {
		start, end := nextBlock(text, len(text)-len(rest))
		if start < 0 {
			p.WriteString(rest)
			break
		}
		base := len(text) - len(rest)
		p.WriteString(rest[:start-base])
		if inj.Len() > 0 {
			inj.WriteString("\n\n")
		}
		inj.WriteString(text[start:end])
		rest = text[end:]
	}
	if inj.Len() == 0 {
		return text, ""
	}
	prompt = strings.TrimSpace(p.String())
	if prompt == "" {
		return "", text
	}
	return prompt, inj.String()
}

// nextBlock returns the bounds in text of the first injected block at or
// after from, or -1: an open tag at the start of a line whose close tag
// follows it.
func nextBlock(text string, from int) (start, end int) {
	best, bestEnd := -1, -1
	for _, t := range injectedTags {
		for at := from; at < len(text); {
			j := strings.Index(text[at:], t.open)
			if j < 0 {
				break
			}
			i := at + j
			if best >= 0 && i >= best {
				break
			}
			if i == 0 || text[i-1] == '\n' {
				if k := strings.Index(text[i:], t.close); k >= 0 {
					best, bestEnd = i, i+k+len(t.close)
					break
				}
			}
			at = i + len(t.open)
		}
	}
	return best, bestEnd
}
