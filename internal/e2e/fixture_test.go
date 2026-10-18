package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// fixtureRow extends the content oracle with the durable raw-evidence identity.
// LEFT JOIN in the reader preserves rows whose source link is missing.
type fixtureRow struct {
	Content                     contentRow
	Device, Agent, Path, FileID string
	Generation                  int64
	GenerationExists            bool
}

func (r fixtureRow) fingerprint() string {
	return fmt.Sprintf("%s|%q|%q|%q|%q|%d|%t", r.Content.fingerprint(), r.Device, r.Agent, r.Path, r.FileID, r.Generation, r.GenerationExists)
}

func fixtureCounts(rows []fixtureRow) map[string]int {
	out := make(map[string]int)
	for _, row := range rows {
		out[row.fingerprint()]++
	}
	return out
}

// Only hash known fixture text. Do not obtain expectations from any transcript
// parser, redactor, exporter, local index, or server response.
func fixtureText(native, parent, kind, parser, text string) contentRow {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	return contentRow{Native: native, Parent: parent, Kind: kind, Role: kind, Parser: parser,
		FullLen: len(text), TextSHA: hash, FullSHA: hash}
}

func (h *harness) serverFixtureRows(d *device, agent, session string) ([]fixtureRow, error) {
	rows, err := h.pg.Query(h.ctx, `SELECT COALESCE(m.native_id,''),COALESCE(m.parent_native_id,''),m.kind,
		COALESCE(m.role,''),COALESCE(m.tool_name,''),COALESCE(m.tool_call_id,''),COALESCE(m.locator,''),m.parser,
		m.ordinal,COALESCE(m.line_no,0),COALESCE(m.byte_offset,0),COALESCE(m.byte_len,0),m.part,m.text_len,
		COALESCE(m.is_error,false),m.text,encode(m.content_sha,'hex'),
		COALESCE(s.device_id::text,''),COALESCE(s.agent,''),COALESCE(s.path,''),COALESCE(s.file_id,''),m.source_generation,g.source_id IS NOT NULL
		FROM messages m JOIN conversations c ON c.id=m.conversation_id LEFT JOIN sources s ON s.id=m.source_id
		LEFT JOIN generations g ON g.source_id=m.source_id AND g.generation=m.source_generation
		WHERE c.device_id=$1 AND c.agent=$2 AND c.session_id=$3 AND NOT m.superseded`, d.id, agent, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fixtureRow
	for rows.Next() {
		var r fixtureRow
		var text string
		c := &r.Content
		if err := rows.Scan(&c.Native, &c.Parent, &c.Kind, &c.Role, &c.Tool, &c.Call, &c.Locator, &c.Parser,
			&c.Ordinal, &c.Line, &c.Offset, &c.Length, &c.Part, &c.FullLen, &c.IsError, &text, &c.FullSHA,
			&r.Device, &r.Agent, &r.Path, &r.FileID, &r.Generation, &r.GenerationExists); err != nil {
			return nil, err
		}
		c.TextSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
		out = append(out, r)
	}
	return out, rows.Err()
}

// The growth seeds have exactly one user record each. Address expectations
// count the seed bytes directly; ordinals use the documented byte-offset *
// 4096 rule. Devin seed rows 8 and 9 have node IDs 1 and 2, respectively.
type fixtureExpectation struct {
	agent, session, path, fileID string
	content                      []contentRow
}

func (h *harness) initialFixtureExpectations(t *testing.T, d *device) []fixtureExpectation {
	t.Helper()
	claudeBytes, err := os.ReadFile(d.claudePath)
	if err != nil {
		t.Fatal(err)
	}
	codexBytes, err := os.ReadFile(d.codexPath)
	if err != nil {
		t.Fatal(err)
	}
	claudeID, err := transcript.StatIdentity(d.claudePath)
	if err != nil {
		t.Fatal(err)
	}
	codexID, err := transcript.StatIdentity(d.codexPath)
	if err != nil {
		t.Fatal(err)
	}
	metaEnd := strings.IndexByte(string(codexBytes), '\n') + 1
	if metaEnd == 0 {
		t.Fatal("Codex seed has no metadata line")
	}
	claude := fixtureText(fmt.Sprintf("e2e%05d-0001-4000-8000-000000000001#0", d.n), "", "user", "claude@4.1",
		fmt.Sprintf("growth session on %s starts here %s", d.name, h.nonce))
	claude.Line, claude.Length = 1, int64(len(claudeBytes))
	codex := fixtureText(fmt.Sprintf("msg_e2e_%d_1", d.n), "", "user", "codex@4.1", d.codexNeedle)
	codex.Line, codex.Offset, codex.Length, codex.Ordinal = 2, int64(metaEnd), int64(len(codexBytes)-metaEnd), int64(metaEnd)*4096
	du := fixtureText("dm-101", "", "user", "devin@3.0", "list the migrations")
	du.Ordinal, du.Locator = 8*4096, "devin-oracle-002/1/8"
	da := fixtureText("dm-102", "dm-101", "assistant", "devin@3.0", "There are three migrations.")
	da.Ordinal, da.Locator = 9*4096+1, "devin-oracle-002/2/9"
	return []fixtureExpectation{
		{"claude", d.claudeSID, d.claudePath, claudeID.ID.String(), []contentRow{claude}},
		{"codex", d.codexSID, d.codexPath, codexID.ID.String(), []contentRow{codex}},
		{"devin", "devin-oracle-002", d.devinDB() + "#devin-oracle-002", "", []contentRow{du, da}},
	}
}

func (h *harness) checkInitialFixtures(t *testing.T, d *device) {
	t.Helper()
	for _, tc := range h.initialFixtureExpectations(t, d) {
		var want []fixtureRow
		for _, c := range tc.content {
			want = append(want, fixtureRow{Content: c, Device: d.id, Agent: tc.agent, Path: tc.path, FileID: tc.fileID, Generation: 0, GenerationExists: true})
		}
		eventually(t, time.Minute, 250*time.Millisecond, d.name+" independent "+tc.agent+" fixture", func() (bool, error) {
			got, err := h.serverFixtureRows(d, tc.agent, tc.session)
			if err != nil {
				return false, err
			}
			if diff := contentDiff(fixtureCounts(want), fixtureCounts(got)); diff != "" {
				return false, fmt.Errorf("fixture content/provenance: %s", diff)
			}
			return true, nil
		})
	}
}

// Recovery and rewrite must preserve the raw address's identity, rather than
// merely recreate matching searchable text. The caller fixes the expected
// file identity and generation from the scenario's explicit state transition.
func (h *harness) checkFixtureProvenance(t *testing.T, d *device, agent, session, path, fileID string, generation int64) {
	t.Helper()
	eventually(t, time.Minute, 250*time.Millisecond, d.name+" fixture source/generation", func() (bool, error) {
		got, err := h.serverFixtureRows(d, agent, session)
		if err != nil {
			return false, err
		}
		if len(got) == 0 {
			return false, fmt.Errorf("no live fixture rows")
		}
		for _, row := range got {
			if row.Device != d.id || row.Agent != agent || row.Path != path || row.FileID != fileID || row.Generation != generation || !row.GenerationExists {
				return false, fmt.Errorf("fixture source/generation differs from scenario expectation")
			}
		}
		return true, nil
	})
}

// Prove a matching semantic row cannot hide a wrong durable source identity.
func TestFixtureOracleRejectsCorruption(t *testing.T) {
	want := fixtureRow{Content: fixtureText("id", "", "user", "fixture@1", "known text"), Device: "device", Agent: "claude", Path: "/fixture", FileID: "inode", Generation: 0, GenerationExists: true}
	for name, mutate := range map[string]func(*fixtureRow){
		"text":                        func(r *fixtureRow) { r.Content.TextSHA = "wrong" },
		"source device":               func(r *fixtureRow) { r.Device = "other" },
		"source agent":                func(r *fixtureRow) { r.Agent = "codex" },
		"source path":                 func(r *fixtureRow) { r.Path = "/other" },
		"source file":                 func(r *fixtureRow) { r.FileID = "other" },
		"source generation":           func(r *fixtureRow) { r.Generation++ },
		"missing generation relation": func(r *fixtureRow) { r.GenerationExists = false },
		"missing source":              func(r *fixtureRow) { r.Device, r.Agent, r.Path, r.FileID = "", "", "", "" },
	} {
		t.Run(name, func(t *testing.T) {
			got := want
			mutate(&got)
			if contentDiff(fixtureCounts([]fixtureRow{want}), fixtureCounts([]fixtureRow{got})) == "" {
				t.Fatal("corruption accepted")
			}
		})
	}
	counts := fixtureCounts([]fixtureRow{want})
	if contentDiff(counts, counts) != "" {
		t.Fatal("matching fixture rejected")
	}
	if contentDiff(counts, fixtureCounts([]fixtureRow{want, want})) == "" {
		t.Fatal("duplicate accepted")
	}
	if contentDiff(counts, nil) == "" {
		t.Fatal("missing row accepted")
	}
}

// This lightweight contract test validates the hand-derived expectations on
// temporary synthetic seeds. The parser is the subject under test here; its
// output never supplies the E2E expectations.
func TestIndependentFixtureExpectations(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	h := &harness{t: t, cfg: &config{root: root, repoRoot: repo}, ctx: context.Background(), sockDir: root, nonce: "fixture-test"}
	for n := 1; n <= 2; n++ {
		d := h.newDevice(n, fmt.Sprintf("fixture-device-%d", n))
		d.seed()
		for _, tc := range h.initialFixtureExpectations(t, d) {
			t.Run(fmt.Sprintf("device%d/%s", n, tc.agent), func(t *testing.T) {
				path := tc.path
				if tc.agent == "devin" {
					path = d.devinDB()
				}
				got, err := h.expectedContent(path, transcript.Agent(tc.agent), tc.session)
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]int{}
				for _, row := range tc.content {
					want[row.fingerprint()]++
				}
				if diff := contentDiff(want, got[tc.session]); diff != "" {
					t.Fatalf("hand-derived fixture expectation: %s", diff)
				}
			})
		}
	}
}
