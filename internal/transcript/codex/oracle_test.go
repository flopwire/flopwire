package codex

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/oracle"
)

const oracleExpected = "../../../testdata/oracle/expected"
const oracleHome = "../../../testdata/oracle/home"

// codexRules maps our rows onto FAD 0.3.1's Codex output. Every
// divergence is a deliberate difference, named with its reason.
func codexRules(historyStart int64) oracle.Rules {
	return oracle.Rules{
		Project: func(m *transcript.Message) []oracle.Row {
			var ts int64
			if !m.TS.IsZero() {
				ts = m.TS.UnixMilli()
			}
			switch m.Kind {
			case transcript.KindUser, transcript.KindAssistant, transcript.KindSystem, transcript.KindInjected, transcript.KindAgentMessage:
				// FAD keeps the payload role: user, assistant, developer, and
				// "agent" for agent_message items. Flopwire's injected and
				// agent_message kinds are FAD user and agent rows.
				return []oracle.Row{{Role: m.Role, Content: m.Text, CreatedAt: ts}}
			case transcript.KindToolCall:
				return []oracle.Row{{Role: "assistant", Content: "[Tool: " + m.ToolName + "]", CreatedAt: ts}}
			case transcript.KindToolResult:
				return []oracle.Row{{Role: "tool", Content: m.Text, CreatedAt: ts}}
			}
			return nil // thinking: FAD drops response_item reasoning
		},
		Divergences: []oracle.Divergence{
			{
				Name:    "FAD indexes event_msg/agent_reasoning; Flopwire reads reasoning summaries from response_item only (rows of kind thinking)",
				DropFAD: func(m oracle.Message) bool { return m.Author != nil && *m.Author == "reasoning" },
			},
			{
				Name:     "codex-events@1 rows for events no call claimed have no FAD counterpart",
				DropOurs: func(m *transcript.Message) bool { return m.Parser == EventsVersion },
			},
			{
				Name: "FAD ignores compacted records; Flopwire indexes the window summary once and replacement items not seen before",
				DropOurs: func(m *transcript.Message) bool {
					return m.Role == "compaction" || m.Enrichment["from_compaction"] == true
				},
			},
			{
				Name: "FAD drops web_search_call, tool_search_* and image_generation_call (no content field); Flopwire keeps them as tool rows",
				DropOurs: func(m *transcript.Message) bool {
					return slices.Contains([]string{"web_search", "tool_search", "image_generation"}, m.ToolName)
				},
			},
			{
				Name: "FAD keeps the parent history a forked subagent copied in; Flopwire skips lines below subagent_history_start_ordinal (indexed in the parent)",
				DropFAD: func(m oracle.Message) bool {
					if historyStart == 0 {
						return false
					}
					var extra struct {
						Ordinal *int64 `json:"ordinal"`
					}
					return json.Unmarshal(m.Extra, &extra) == nil && extra.Ordinal != nil && *extra.Ordinal < historyStart
				},
			},
		},
	}
}

func TestOracleParity(t *testing.T) {
	files, err := oracle.Load(oracleExpected)
	if err != nil {
		t.Fatal(err)
	}
	files = oracle.ForConnector(files, "codex")
	if len(files) < 5 {
		t.Fatalf("want >= 5 codex oracle files, got %d (run scripts/regen-oracle.sh codex)", len(files))
	}
	for _, f := range files {
		t.Run(f.Name, func(t *testing.T) {
			c := parseFile(t, oracle.RealPath(oracleHome, f), nil)
			conv := lastConv(c)
			var hso int64
			if v, ok := conv.Extra["subagent_history_start_ordinal"].(int64); ok {
				hso = v
			}
			if len(f.Conversations) != 1 {
				t.Fatalf("FAD emitted %d conversations for one rollout", len(f.Conversations))
			}
			oracle.Assert(t, f, f.Conversations[0], conv, upsert(c.Messages), codexRules(hso))
		})
	}
}
