package ingest

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript/opencode"
)

// opencodeExportFunc is the device agent's exporter for one session: appends
// to the export the saved state describes (opencode-export@2).
func opencodeExportFunc(db, session string) devicesync.ExportFunc {
	return func(ctx context.Context, prev []byte) (devicesync.Export, error) {
		data, appended, state, err := opencode.ExportFrom(ctx, db, session, prev)
		return devicesync.Export{Data: data, Append: appended, State: state}, err
	}
}

func opencodeOnly(m map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		if strings.HasPrefix(k, "opencode|") {
			out[k] = v
		}
	}
	return out
}

// Appends must reconstruct exactly the full parse on the real server.
func TestOpencodeAppendedExportIngest(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := newFixture(t)
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync := func() {
		t.Helper()
		for _, id := range f.opencodeSessions {
			if err := sy.SyncExportFunc(ctx, opencodeSpec(f.opencodeDB, id), opencodeExportFunc(f.opencodeDB, id)); err != nil {
				t.Fatal(err)
			}
		}
		e.drain()
		sameRows(t, opencodeOnly(e.live()), opencodeOnly(f.want(t)))
	}
	sync()
	db, err := sql.Open("sqlite", "file:"+f.opencodeDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var session, part string
	var at int64
	if err := db.QueryRow(`SELECT session_id,id,time_updated FROM part ORDER BY time_updated DESC LIMIT 1`).Scan(&session, &part, &at); err != nil {
		t.Fatal(err)
	}
	generation := func() int {
		return e.count(`SELECT max(g.generation) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.path=$1`, opencode.ExportPath(f.opencodeDB, session))
	}
	gen := generation()
	for _, step := range []struct {
		name, query string
		args        []any
		reset       bool
	}{
		{"stream part", `UPDATE part SET data='{"type":"text","text":"new streaming text"}',time_updated=? WHERE id=?`, []any{at + 10000, part}, false},
		{"rename", `UPDATE session SET title='Renamed export' WHERE id=?`, []any{session}, false},
		{"delete part", `DELETE FROM part WHERE id=?`, []any{part}, true},
		{"new part", `INSERT INTO part SELECT 'prt_export_added',id,session_id,?,?, '{"type":"text","text":"after reset"}' FROM message WHERE session_id=? LIMIT 1`, []any{at + 20000, at + 20000, session}, false},
	} {
		if _, err := db.Exec(step.query, step.args...); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		sync()
		next := generation()
		if (next != gen) != step.reset {
			t.Fatalf("%s: generation %d -> %d", step.name, gen, next)
		}
		gen = next
	}
}
