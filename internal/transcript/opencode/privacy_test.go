package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// poison is planted in every table and session column the parser must not
// read. None of it may reach a row, a conversation, an export or the
// directories the path rules see.
const poison = "POISON-access-token-7f3a"

// The store also holds account tokens (account, control_account,
// credential) and share secrets. Only session, message and part are read,
// and of session only the columns a conversation needs.
func TestTokensNeverLeave(t *testing.T) {
	f := newFixture(t)
	seed(f)
	f.session(sesB, sesA, cwd, "child", t0+30)
	f.Prompt(sesB, t0+31, "explore widget.go")
	for _, q := range []string{
		"CREATE TABLE `account` (`id` text PRIMARY KEY, `email` text NOT NULL, `url` text NOT NULL, `access_token` text NOT NULL, `refresh_token` text NOT NULL, `token_expiry` integer, `time_created` integer NOT NULL, `time_updated` integer NOT NULL)",
		"CREATE TABLE `control_account` (`email` text NOT NULL, `url` text NOT NULL, `access_token` text NOT NULL, `refresh_token` text NOT NULL, `token_expiry` integer, `active` integer NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL)",
		"CREATE TABLE `credential` (`id` text PRIMARY KEY, `session_id` text, `data` text)",
		"CREATE TABLE `session_share` (`session_id` text PRIMARY KEY, `id` text NOT NULL, `secret` text NOT NULL, `url` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL)",
		"CREATE TABLE `session_message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `type` text NOT NULL, `seq` integer NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL)",
	} {
		f.exec(q)
	}
	for _, s := range []string{sesA, sesB} {
		f.exec("INSERT INTO account VALUES (?, ?, ?, ?, ?, 0, 1, 1)", "acc_"+s, poison+"@x", "https://"+poison, poison, poison)
		f.exec("INSERT INTO control_account VALUES (?, ?, ?, ?, 0, 1, 1, 1)", poison+s, "https://x", poison, poison)
		f.exec("INSERT INTO credential VALUES (?, ?, ?)", "cred_"+s, s, `{"type":"text","text":"`+poison+`"}`)
		f.exec("INSERT INTO session_share VALUES (?, ?, ?, ?, 1, 1)", s, poison, poison, "https://"+poison)
		f.exec("INSERT INTO session_message VALUES (?, ?, 'text', 1, 1, 1, ?)", "sm_"+s, s, `{"type":"text","text":"`+poison+`"}`)
		f.exec("UPDATE session SET share_url = ?, permission = ?, metadata = ?, revert = ?, summary_diffs = ?, path = ?, workspace_id = ?, project_id = ? WHERE id = ?",
			poison, poison, `{"k":"`+poison+`"}`, poison, poison, poison, poison, poison, s)
	}

	ctx := context.Background()
	check := func(what string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), poison) || strings.Contains(fmt.Sprintf("%+v", v), poison) {
			t.Errorf("%s carries a token: %s", what, b)
		}
	}
	c, _ := parse(t, f.path, transcript.Cursor{})
	if len(c.Messages) == 0 || len(c.Conversations) != 2 {
		t.Fatalf("parsed %d rows, %d conversations", len(c.Messages), len(c.Conversations))
	}
	for _, m := range c.Messages {
		check("row "+m.NativeID, m)
	}
	for _, v := range c.Conversations {
		check("conversation "+v.SessionID, v)
	}
	cwds, err := Cwds(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	check("cwds", cwds)
	ids, err := ListSessions(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	check("session list", ids)
	for _, id := range ids {
		b, err := Export(ctx, f.path, id)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(poison)) {
			t.Errorf("export of %s carries a token:\n%s", id, b)
		}
		db, err := LoadExport(ctx, bytes.NewReader(b), id, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := parse(t, db, transcript.Cursor{})
		for _, v := range got.Conversations {
			check("server conversation "+v.SessionID, v)
		}
	}
}
