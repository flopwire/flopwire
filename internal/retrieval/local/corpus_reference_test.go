package local

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

type corpusQuery struct {
	kind, text string
	regex      bool
}

func corpusPattern(q corpusQuery) (*regexp.Regexp, error) {
	pattern := q.text
	if !q.regex {
		pattern = regexp.QuoteMeta(pattern)
	}
	// TestCorpus passes GrepQuery.CaseSensitive=false directly. Unlike CLI
	// Spec.Query, that path does not infer smart case from capital letters.
	return regexp.Compile("(?i)" + pattern)
}

// Machine transcripts need not mention any fixed smoke phrase. Establish
// presence independently of FTS, and require an actual visible-text witness.
// The ID SQL mirrors zero Filter.hitWhere: live, active-path, non-injected
// messages joined to conversations. It deliberately adds no tombstone filter
// that the production message query does not have.
func corpusReference(s *localindex.Store, queries []corpusQuery) ([]bool, *localindex.Row, string, error) {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	patterns := make([]*regexp.Regexp, len(queries))
	for i, q := range queries {
		if q.kind != "search" {
			var err error
			patterns[i], err = corpusPattern(q)
			if err != nil {
				return nil, nil, "", err
			}
		}
	}
	rows, err := s.DB().QueryContext(c, `SELECT m.id FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.superseded=0 AND m.on_active_path IS NOT 0 AND m.kind<>'injected' ORDER BY m.id`)
	if err != nil {
		return nil, nil, "", err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, "", err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, "", err
	}
	present := make([]bool, len(queries))
	words := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	var witness *localindex.Row
	var token string
	for start := 0; start < len(ids); start += 128 {
		batch := ids[start:min(start+128, len(ids))]
		decoded, err := s.Messages(c, batch)
		if err != nil {
			return nil, nil, "", err
		}
		if len(decoded) != len(batch) {
			return nil, nil, "", fmt.Errorf("corpus reference lost rows: %d/%d", len(decoded), len(batch))
		}
		for _, r := range decoded {
			for i, re := range patterns {
				if re != nil && re.MatchString(r.Text) {
					present[i] = true
				}
			}
			if witness == nil && len(r.Text) <= format.DefaultMaxChars {
				for _, word := range strings.Fields(r.Text) {
					if len(word) >= 6 && len(word) <= 40 && words.MatchString(word) {
						witness = r
						token = word
						break
					}
				}
			}
		}
	}
	if err := c.Err(); err != nil {
		return nil, nil, "", err
	}
	if witness == nil {
		return nil, nil, "", fmt.Errorf("corpus has no visible short message containing an ASCII search witness")
	}
	return present, witness, token, nil
}

func corpusPresence(want bool, page *format.Page) error {
	if page == nil {
		return fmt.Errorf("nil corpus result")
	}
	if len(page.Hits) > 0 && !want {
		return fmt.Errorf("hits absent from complete text reference")
	}
	if len(page.Hits) == 0 && want {
		return fmt.Errorf("missing known-positive result (partial=%v)", page.Truncated)
	}
	if len(page.Hits) == 0 && page.Truncated {
		return fmt.Errorf("absence is inconclusive: incomplete query")
	}
	return nil
}
