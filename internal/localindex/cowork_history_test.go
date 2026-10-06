package localindex

import (
	"reflect"
	"testing"
)

func TestCoworkHistoryDeviceBoundMonotoneAndDurable(t *testing.T) {
	s := openEvidenceTest(t)
	if facts, err := s.CoworkHistoricalUnknownFacts(ctx); err != nil || len(facts) != 0 {
		t.Fatalf("optional facts: %v %v", facts, err)
	}
	if err := s.MarkCoworkHistoricalUnknown(ctx, []string{"native", "agent-child"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCoworkHistoricalUnknown(ctx, []string{"native"}); err != nil {
		t.Fatal(err)
	}
	r, err := Open(s.Path(), Options{DeviceID: "local-device", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if facts, err := r.CoworkHistoricalUnknownFacts(ctx); err != nil || !reflect.DeepEqual(facts, []string{"agent-child", "native"}) {
		t.Fatalf("committed facts %v %v", facts, err)
	}
	foreign, err := Open(s.Path(), Options{DeviceID: "other-device", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if have, err := foreign.CoworkHistoricalUnknown(ctx, "native"); err != nil || have {
		t.Fatalf("foreign fact %v %v", have, err)
	}
	if err = s.write(ctx, func(w *writeTx) error {
		for _, table := range []string{"messages", "companions", "conversations", "generations", "sources", "placements"} {
			if _, err := w.exec("DELETE FROM " + table); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if have, err := s.CoworkHistoricalUnknown(ctx, "native"); err != nil || !have {
		t.Fatalf("GC erased fact %v %v", have, err)
	}
}

func TestCoworkHistoryWholeFamilyRollbackOnFailure(t *testing.T) {
	s := openEvidenceTest(t)
	if err := s.MarkCoworkHistoricalUnknown(ctx, []string{"existing"}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeWait(ctx, func(w *writeTx) error {
		_, err := w.exec(`CREATE TRIGGER fail_fact BEFORE INSERT ON cowork_history WHEN NEW.session_id='agent-child' BEGIN SELECT RAISE(FAIL,'synthetic fact failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCoworkHistoricalUnknown(ctx, []string{"parent", "agent-child"}); err == nil {
		t.Fatal("fact failure ignored")
	}
	if have, err := s.CoworkHistoricalUnknown(ctx, "parent"); err != nil || have {
		t.Fatalf("partial family fact committed %v %v", have, err)
	}
}
