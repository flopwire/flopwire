package e2e

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// contentRow compares stored semantics and addresses, independently of the
// identity-only checks. Hash text before producing diagnostics: opt-in corpus
// runs must not put private transcript text in reports or assertion output.
type contentRow struct {
	Native, Parent, Kind, Role, Tool, Call, Locator, Parser string
	Ordinal, Line, Offset, Length                           int64
	Part, FullLen                                           int
	IsError                                                 bool
	TextSHA, FullSHA                                        string
}

func (r contentRow) fingerprint() string {
	b, _ := json.Marshal(r)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func parsedContent(messages []*transcript.Message) map[string]map[string]int {
	out := map[string]map[string]int{}
	// Parsers can emit metadata updates or growing text for an existing
	// native ID. Compare the final live version, not intermediate emissions.
	type identity struct {
		session, native, locator string
		part                     int
	}
	live := map[identity]*transcript.Message{}
	for _, m := range messages {
		loc := ""
		if m.NativeID == "" {
			loc = m.Locator
			if loc == "" {
				loc = fmt.Sprintf("@%d", m.ByteOffset)
			}
		}
		live[identity{m.SessionID, m.NativeID, loc, m.Part}] = m
	}
	clean := func(s string) string { return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�") }
	for _, m := range live {
		if m.Superseded {
			continue
		}
		loc := m.Locator
		if loc == "" && m.NativeID == "" {
			loc = fmt.Sprintf("@%d", m.ByteOffset)
		}
		r := contentRow{Native: clean(m.NativeID), Parent: clean(m.ParentNativeID),
			Kind: m.Kind.String(), Role: clean(m.Role), Tool: clean(m.ToolName), Call: clean(m.ToolCallID),
			Locator: clean(loc), Parser: m.Parser, Ordinal: m.Ordinal, Line: m.LineNo,
			Offset: m.ByteOffset, Length: m.ByteLen, Part: m.Part, FullLen: m.FullLen, IsError: m.IsError,
			TextSHA: fmt.Sprintf("%x", sha256.Sum256([]byte(clean(m.Text)))), FullSHA: fmt.Sprintf("%x", m.ContentSHA)}
		if out[m.SessionID] == nil {
			out[m.SessionID] = map[string]int{}
		}
		out[m.SessionID][r.fingerprint()]++
	}
	return out
}

func (h *harness) serverContent(deviceID, agent, session string) (map[string]int, error) {
	rows, err := h.pg.Query(h.ctx, `SELECT COALESCE(m.native_id,''),COALESCE(m.parent_native_id,''),m.kind,
		COALESCE(m.role,''),COALESCE(m.tool_name,''),COALESCE(m.tool_call_id,''),COALESCE(m.locator,''),m.parser,
		m.ordinal,COALESCE(m.line_no,0),COALESCE(m.byte_offset,0),COALESCE(m.byte_len,0),m.part,m.text_len,
		COALESCE(m.is_error,false),m.text,encode(m.content_sha,'hex')
		FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.device_id=$1 AND c.agent=$2 AND c.session_id=$3 AND NOT m.superseded`, deviceID, agent, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var r contentRow
		var text string
		if err := rows.Scan(&r.Native, &r.Parent, &r.Kind, &r.Role, &r.Tool, &r.Call, &r.Locator, &r.Parser,
			&r.Ordinal, &r.Line, &r.Offset, &r.Length, &r.Part, &r.FullLen, &r.IsError, &text, &r.FullSHA); err != nil {
			return nil, err
		}
		r.TextSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
		out[r.fingerprint()]++
	}
	return out, rows.Err()
}

// Parse the redacted bytes the device uploads, with the server's uncapped
// extraction policy. This also covers Devin via its actual export format.
func (h *harness) expectedContent(path string, agent transcript.Agent, session string) (map[string]map[string]int, error) {
	var p transcript.Parser
	src := &transcript.Source{Agent: agent, Path: path, StorageKind: transcript.StorageJSONLAppend}
	uncapped := map[transcript.Kind]transcript.CapConfig{}
	var input transcript.Input
	switch agent {
	case transcript.AgentClaude, transcript.AgentCodex:
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		input = transcript.Input{Source: src, R: redact.NewReaderAt(f, redact.Lines), Size: fi.Size()}
		if agent == transcript.AgentClaude {
			p = &claude.Parser{Caps: uncapped}
		} else {
			p = &codex.Parser{Caps: uncapped}
		}
	case transcript.AgentDevin:
		data, err := devin.Export(h.ctx, path, session)
		if err != nil {
			return nil, err
		}
		dir, err := os.MkdirTemp(h.cfg.root, "content-devin-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		r := redact.NewReaderAt(strings.NewReader(string(data)), redact.Lines)
		db, err := devin.LoadExport(h.ctx, io.NewSectionReader(r, 0, int64(len(data))), session, dir)
		if err != nil {
			return nil, err
		}
		src.Path, src.StorageKind = db, transcript.StorageSQLite
		p = &devin.Parser{Caps: uncapped}
		input.Source = src
	default:
		return nil, fmt.Errorf("content oracle: unsupported agent %s", agent)
	}
	src.Parser = p.Name()
	var c transcript.Collector
	if _, err := transcript.Reparse(h.ctx, p, input, &c); err != nil {
		return nil, err
	}
	return parsedContent(c.Messages), nil
}

func contentDiff(want, got map[string]int) string {
	missing, extra := 0, 0
	for k, n := range want {
		if n > got[k] {
			missing += n - got[k]
		}
	}
	for k, n := range got {
		if n > want[k] {
			extra += n - want[k]
		}
	}
	if missing+extra == 0 {
		return ""
	}
	return fmt.Sprintf("content/address rows missing=%d unexpected=%d", missing, extra)
}

func TestContentOracleDetectsCorruption(t *testing.T) {
	r := contentRow{Native: "same-id", Kind: "assistant", TextSHA: "original", FullSHA: "full", Ordinal: 12, Offset: 3, Length: 9}
	want := map[string]int{r.fingerprint(): 1}
	for name, mutate := range map[string]func(*contentRow){
		"text":      func(r *contentRow) { r.TextSHA = "changed" },
		"full text": func(r *contentRow) { r.FullSHA = "changed" },
		"kind":      func(r *contentRow) { r.Kind = "tool_result" },
		"order":     func(r *contentRow) { r.Ordinal++ },
		"offset":    func(r *contentRow) { r.Offset++ },
		"length":    func(r *contentRow) { r.Length++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := r
			mutate(&changed)
			if contentDiff(want, map[string]int{changed.fingerprint(): 1}) == "" {
				t.Fatal("corruption accepted")
			}
		})
	}
	if contentDiff(want, want) != "" {
		t.Fatal("matching rows rejected")
	}
	if contentDiff(want, map[string]int{r.fingerprint(): 2}) == "" {
		t.Fatal("duplicate accepted")
	}
	if contentDiff(want, nil) == "" {
		t.Fatal("missing row accepted")
	}
}

func TestContentOracleFoldsParserUpdates(t *testing.T) {
	first := &transcript.Message{SessionID: "s", NativeID: "id", Kind: transcript.KindAssistant}
	first.SetText("partial", transcript.CapConfig{})
	last := *first
	last.SetText("complete", transcript.CapConfig{})
	got := parsedContent([]*transcript.Message{first, &last})
	want := parsedContent([]*transcript.Message{&last})
	if len(got["s"]) != 1 || contentDiff(want["s"], got["s"]) != "" {
		t.Fatal("intermediate parser emission counted as a live row")
	}
}
