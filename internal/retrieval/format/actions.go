package format

import (
	"fmt"

	"github.com/flopwire/flopwire/internal/coverage"
)

// CoverageAction reports evidence gaps without making a completeness claim.
func CoverageAction(s *Scope) string {
	if s == nil || s.Coverage == nil || s.CoverageMode == coverage.DetailMode {
		return ""
	}
	r := s.Coverage
	gap := false

	if u := r.Upload; u != nil {
		gap = gap || u.BlockingReason != "" || u.FailingSources > 0
		if c := u.Captured; c != nil {
			gap = gap || c.PendingGenerations > 0 || c.LostGenerations > 0 || c.TruncatedGenerations > 0
		}
	}
	if p := r.Parse; p != nil {
		gap = gap || p.Pending > 0 || p.Failing > 0 || p.Quarantined > 0 || p.UntrackedSources > 0
	}
	if p := r.Policy; p != nil {
		if p.HistoricalMappingUnknown != nil {
			gap = gap || *p.HistoricalMappingUnknown > 0
		}
		if c := p.Cowork; c != nil {
			gap = gap || c.SharedHeld > 0 || c.HistoricalUnknown > 0
		}
	}
	if gap {
		return "If recorded gaps affect your conclusion, inspect coverage=detail on this retrieval tool; it cannot establish exhaustive discovery."
	}
	return ""
}

func PageAction(p *Page) string {
	if p.Scan != nil && p.Scan.NextCursor != "" {
		return "If more evidence is needed, pass scan.next_cursor as cursor with unchanged query arguments."
	}
	if p.Next > 0 {
		return fmt.Sprintf("If more evidence is needed, pass offset=%d with unchanged query arguments.", p.Next)
	}
	for _, h := range p.Hits {
		if h.MoreLines > 0 {
			return fmt.Sprintf("If more evidence is needed, read address=%s for the message containing omitted matching lines.", h.Address)
		}
	}
	if p.Truncated {
		return "Narrow the pattern, session or filters and retry; this partial query cannot establish absence."
	}
	if h := CoverageAction(p.Scope); h != "" {
		return h
	}
	if len(p.Hits)+len(p.Sessions) == 0 {
		return "No matches in this query scope. Adjust the query or filters if more evidence is needed."
	}
	return "Read a relevant returned address before relying on its snippet."
}

func SessionsAction(s *Sessions) string {
	if s.Next != "" {
		return "If more evidence is needed, pass next_cursor as cursor with unchanged filters."
	}
	if h := CoverageAction(s.Scope); h != "" {
		return h
	}
	if len(s.Sessions) == 0 {
		return "Adjust the filters if more evidence is needed."
	}
	return "Read a relevant returned session address to inspect its evidence."
}

func ReadAction(c *Context) string {
	for _, focusOnly := range []bool{true, false} {
		for _, m := range c.Messages {
			if focusOnly && m.ID != c.Focus {
				continue
			}
			if m.Clipped {
				if m.LineTo < m.Lines {
					return fmt.Sprintf("If more evidence is needed, read address=%s line_offset=%d to continue this message.", m.Address, m.LineTo+1)
				}
				if m.StoredLen > 0 && m.StoredLen < m.TextLen {
					return fmt.Sprintf("If more evidence is needed, read address=%s raw=true for the transcript record.", m.Address)
				}
			}
		}
	}

	if len(c.Messages) > 0 && c.MoreAfter {
		return fmt.Sprintf("If more evidence is needed, read address=%s messages_after=10.", c.Messages[len(c.Messages)-1].Address)
	}
	if len(c.Messages) > 0 && c.MoreBefore {
		return fmt.Sprintf("If more evidence is needed, read address=%s messages_before=10.", c.Messages[0].Address)
	}

	if c.OutlineNext != "" {
		return "If more evidence is needed, pass outline_next as cursor with the same address and outline=true."
	}
	if h := CoverageAction(c.Scope); h != "" {
		return h
	}
	if c.Outline != nil || c.OutlineMore {
		return "Read a relevant returned address to inspect its full evidence."
	}
	return "Compare this evidence with the requested claim; state remaining gaps."
}

func actionFooter(e *errWriter, st Style, action string) {
	if st.MCP && action != "" {
		e.printf("[next: %s]\n", action)
	}
}

// Reserve guidance before selecting rows; any clipping retains its exact action.
func reserveAction(st Style, action string) Style {
	if st.MCP && st.Budget > 0 {
		if st.reportedBudget == 0 {
			st.reportedBudget = st.Budget
		}
		st.Budget = max(1, st.Budget-len(action)-10)
	}
	return st
}
