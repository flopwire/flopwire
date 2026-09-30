package devin

import (
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/oracle"
)

const expectedDir = "../../../testdata/oracle/expected"

// fadView folds our rows into FAD's shape, one message per node, for the
// rows FAD sees: FAD 0.3.1 walks only the main chain, so only rows with
// on_active_path = true take part. Documented divergences:
//
//   - off-chain rows (branches, compacted-away history, summarizer trees)
//     exist only on our side: FAD never reads them;
//   - hidden sessions and nodes without a sessions row exist only on our
//     side: FAD skips hidden sessions and reads sessions first;
//   - FAD drops system nodes and thinking; we keep both as their own kinds;
//   - FAD folds tool calls into the assistant text as "[tool_call: name]"
//     lines; we emit tool_call rows (folded back here);
//   - user nodes with is_user_input null (cache keepalive "continue",
//     compaction summarization prompts) are system rows for us, user rows
//     for FAD (projected back to user here);
//   - a collapsed compaction copy takes its time and position from the
//     first copy for us, from the chain copy for FAD. Its text is the
//     on-chain copy's on both sides.
func fadView(msgs []*transcript.Message) []*transcript.Message {
	var on []*transcript.Message
	for _, m := range msgs {
		if m.OnActivePath != nil && *m.OnActivePath {
			on = append(on, m)
		}
	}
	slices.SortStableFunc(on, func(a, b *transcript.Message) int {
		switch {
		case a.Ordinal < b.Ordinal:
			return -1
		case a.Ordinal > b.Ordinal:
			return 1
		}
		return 0
	})
	var out []*transcript.Message
	var cur *transcript.Message // the folded assistant message of the current node
	var calls []string
	flush := func() {
		if cur == nil {
			return
		}
		cur.Text = strings.Join(slices.DeleteFunc(append([]string{cur.Text}, calls...), func(s string) bool { return s == "" }), "\n")
		out = append(out, cur)
		cur, calls = nil, nil
	}
	lastLoc := ""
	for _, m := range on {
		if m.Locator != lastLoc {
			flush()
			lastLoc = m.Locator
		}
		switch {
		case m.Kind == transcript.KindAssistant:
			cp := *m
			cur = &cp
		case m.Kind == transcript.KindToolCall:
			if cur == nil {
				cp := *m
				cp.Kind, cp.Text = transcript.KindAssistant, ""
				cur = &cp
			}
			calls = append(calls, "[tool_call: "+m.ToolName+"]")
		case m.Kind == transcript.KindSystem && m.Role == "user":
			cp := *m
			cp.Kind = transcript.KindUser
			out = append(out, &cp)
		case m.Kind == transcript.KindSystem, m.Kind == transcript.KindThinking:
			// FAD drops these.
		default:
			out = append(out, m)
		}
	}
	flush()
	return out
}

func TestOracleParityMainChain(t *testing.T) {
	path, _ := buildDB(t)
	c, _ := parse(t, path, transcript.Cursor{})
	files, err := oracle.Load(expectedDir)
	if err != nil {
		t.Fatal(err)
	}
	files = oracle.ForConnector(files, "devin")
	convs := map[string]*transcript.Conversation{}
	for _, cv := range c.Conversations {
		convs[cv.SessionID] = cv
	}
	matched := map[string]bool{}
	for _, f := range files {
		for _, exp := range f.Conversations {
			if exp.ExternalID == nil {
				t.Fatalf("%s: FAD conversation without external id", f.Name)
			}
			id := *exp.ExternalID
			cv := convs[id]
			if cv == nil {
				t.Errorf("%s: session %s not emitted", f.Name, id)
				continue
			}
			matched[id] = true
			rules := oracle.Rules{
				// Collapsed compaction copies: first-sight time (ours)
				// versus the chain copy's time (FAD).
				IgnoreTimes: id == "devin-oracle-003",
			}
			oracle.Assert(t, f, exp, cv, fadView(c.MessagesFor(id)), rules)
		}
	}
	for _, id := range []string{"devin-oracle-001", "devin-oracle-002", "devin-oracle-003"} {
		if !matched[id] {
			t.Errorf("no FAD expectation for %s (run scripts/regen-oracle.sh)", id)
		}
	}
	for _, id := range []string{"devin-oracle-004", "devin-oracle-005"} {
		if matched[id] {
			t.Errorf("FAD emitted %s; the hidden/orphan divergence is stale", id)
		}
		if len(c.MessagesFor(id)) == 0 {
			t.Errorf("we emitted no rows for %s", id)
		}
	}
}
