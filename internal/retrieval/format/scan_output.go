package format

import (
	"encoding/json"
	"strings"
)

// BoundGrepScan preserves all message addresses in a scan batch. The cursor
// advances past the whole batch, so output budgets may shorten excerpts only.
// As with ordinary pages, essential identifiers may exceed a tiny budget.
func BoundGrepScan(p *Page, budget int) *Page {
	if p.Scan == nil {
		return p
	}
	cp := *p
	cp.Next = 0
	if budget <= 0 {
		return &cp
	}
	encoded, _ := json.Marshal(&cp)
	if len(encoded)+400 <= budget {
		return &cp
	}
	cp.SessionInfo = nil
	cp.Notes = []string{"output budget reached; read each address for full context"}
	cp.Hits = make([]Hit, len(p.Hits))
	for i, h := range p.Hits {
		cp.Hits[i] = Hit{Address: h.Address, MessageID: h.MessageID, SessionID: h.SessionID, Ordinal: h.Ordinal, Kind: h.Kind, Agent: h.Agent, MoreLines: h.MoreLines, Provenance: h.Provenance}
		if len(h.Lines) > 0 {
			line := h.Lines[0]
			for _, candidate := range h.Lines {
				if candidate.Match {
					line = candidate
					break
				}
			}
			if len(line.Text) > 100 {
				line.Text = strings.ToValidUTF8(line.Text[:100], "") + "…"
			}
			cp.Hits[i].Lines = []Line{line}
			for _, l := range h.Lines {
				if l.Match && l.N != line.N {
					cp.Hits[i].MoreLines++
				}
			}
		}
	}
	encoded, _ = json.Marshal(&cp)
	if len(encoded)+400 > budget {
		for i := range cp.Hits {
			if len(cp.Hits[i].Lines) > 0 {
				line := cp.Hits[i].Lines[0]
				line.Text = "[excerpt omitted; read this address]"
				cp.Hits[i].Lines = []Line{line}
			}
		}
	}
	return &cp
}
