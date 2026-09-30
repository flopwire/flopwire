package backup

import (
	"testing"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/jackc/pgx/v5"
)

func TestRestoreTargetRejectsEveryUserObjectClass(t *testing.T) {
	conn, err := pgx.Connect(t.Context(), pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	empty, err := restoreTargetDatabaseEmpty(t.Context(), conn)
	if err != nil || !empty {
		t.Fatalf("fresh target empty=%v err=%v", empty, err)
	}
	cases := map[string]string{
		"sequence":             `CREATE SEQUENCE public.restore_sequence`,
		"view":                 `CREATE VIEW public.restore_view AS SELECT 1 AS value`,
		"function":             `CREATE FUNCTION public.restore_function() RETURNS integer LANGUAGE SQL AS 'SELECT 1'`,
		"type":                 `CREATE TYPE public.restore_type AS ENUM ('one')`,
		"extra_schema":         `CREATE SCHEMA restore_extra`,
		"publication":          `CREATE PUBLICATION restore_publication`,
		"foreign_data_wrapper": `CREATE FOREIGN DATA WRAPPER restore_fdw NO HANDLER`,
		"large_object":         `SELECT lo_create(982451653)`,
	}
	for name, statement := range cases {
		t.Run(name, func(t *testing.T) {
			tx, err := conn.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = tx.Exec(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
			empty, err := restoreTargetDatabaseEmpty(t.Context(), tx)
			if err != nil {
				t.Fatal(err)
			}
			if empty {
				t.Fatal("restore accepted non-empty target")
			}
		})
	}
}
