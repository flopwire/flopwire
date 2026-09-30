package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/oracle"
)

const expectedDir = "../../../testdata/oracle/expected"

// Documented divergences between this parser and FAD 0.3.1's claude_code
// connector. Two more are structural and handled by fadView: FAD merges a
// line's text and tool_use blocks into one assistant row (tool calls as
// "[Tool: Name - description]") and drops thinking; and it emits a user
// line's tool results before that line's text. FAD also reads the title
// from "title" while Claude writes "aiTitle", so its titles fall back to
// the first prompt (titles are not compared).
var claudeDivergences = []oracle.Divergence{
	{
		Name: "FAD keeps isMeta user lines (caveats, reminders); Flopwire skips them as agentsview does",
		DropFAD: func(m oracle.Message) bool {
			var e struct {
				IsMeta bool `json:"isMeta"`
			}
			return json.Unmarshal(m.Extra, &e) == nil && e.IsMeta
		},
	},
	{
		Name:     "FAD indexes the 2KB <persisted-output> preview; Flopwire indexes the capped tool-results/ file",
		DropFAD:  func(m oracle.Message) bool { return strings.Contains(m.Content, persistedMarker) },
		DropOurs: func(m *transcript.Message) bool { return m.Enrichment["persisted_output"] != nil },
	},
	{
		Name: "FAD drops tool results with empty text; Flopwire keeps the row (call id, error flag)",
		DropOurs: func(m *transcript.Message) bool {
			return m.Kind == transcript.KindToolResult && strings.TrimSpace(m.Text) == ""
		},
	},
	{
		// Mixed blocks are normalized by claudeRules: FAD's text is
		// compared without its injected blocks.
		Name: "FAD indexes <system-reminder> and <task-notification> blocks in user turns as prompts; Flopwire makes them kind injected (D6)",
		DropFAD: func(m oracle.Message) bool {
			p, inj := splitInjected(m.Content)
			return m.Role == "user" && inj != "" && p == ""
		},
	},
	{
		Name:     "FAD drops every system record; Flopwire keeps compact_boundary and text-bearing subtypes",
		DropOurs: func(m *transcript.Message) bool { return m.Kind == transcript.KindSystem },
	},
}

// claudeRules compares with the divergences above; content is compared
// without injected blocks (fadView leaves injected rows out).
func claudeRules() oracle.Rules {
	return oracle.Rules{
		Divergences: claudeDivergences,
		Normalize: func(s string) string {
			p, _ := splitInjected(s)
			return oracle.CollapseSpace(p)
		},
	}
}

// fadView folds our rows into FAD's shape, line by line.
func fadView(msgs []*transcript.Message) []*transcript.Message {
	var out []*transcript.Message
	for i := 0; i < len(msgs); {
		j := i
		for j < len(msgs) && msgs[j].LineNo == msgs[i].LineNo {
			j++
		}
		line := msgs[i:j]
		i = j

		var results, texts []*transcript.Message
		var asst []string
		for _, m := range line {
			switch m.Kind {
			case transcript.KindToolResult:
				results = append(results, m)
			case transcript.KindUser:
				texts = append(texts, m)
			case transcript.KindAssistant:
				asst = append(asst, m.Text)
			case transcript.KindToolCall:
				asst = append(asst, toolLabel(m))
			case transcript.KindSystem:
				if m.Enrichment["subtype"] == "compact_summary" {
					c := *m
					c.Kind = transcript.KindUser // FAD: a plain user line
					texts = append(texts, &c)
				} else {
					out = append(out, m)
				}
			}
		}
		out = append(out, results...)
		if len(texts) > 0 {
			c := *texts[0]
			for _, t := range texts[1:] {
				c.Text += "\n" + t.Text
			}
			out = append(out, &c)
		}
		if len(asst) > 0 {
			c := *line[0]
			c.Kind, c.Text = transcript.KindAssistant, strings.Join(asst, "\n")
			out = append(out, &c)
		}
	}
	return out
}

// toolLabel is FAD's rendering of a tool_use block.
func toolLabel(m *transcript.Message) string {
	var in struct {
		Description string `json:"description"`
		FilePath    string `json:"file_path"`
	}
	_ = json.Unmarshal([]byte(m.Text), &in)
	desc := in.Description
	if desc == "" {
		desc = in.FilePath
	}
	if desc == "" {
		return "[Tool: " + m.ToolName + "]"
	}
	return "[Tool: " + m.ToolName + " - " + desc + "]"
}

func TestFADParity(t *testing.T) {
	files, err := oracle.Load(expectedDir)
	if err != nil {
		t.Fatal(err)
	}
	files = oracle.ForConnector(files, "claude")
	if len(files) < 6 {
		t.Fatalf("want >= 6 claude expected files, got %d (run scripts/regen-oracle.sh claude)", len(files))
	}
	for _, f := range files {
		path := oracle.RealPath(fixtureHome, f)
		c, _ := parseFile(t, &Parser{}, path)
		if len(f.Conversations) != 1 {
			t.Fatalf("%s: %d FAD conversations", f.Name, len(f.Conversations))
		}
		oracle.Assert(t, f, f.Conversations[0], lastConv(t, c), fadView(c.Messages), claudeRules())
	}
}
