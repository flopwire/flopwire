package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Use the normal planner: forced-index plans do not prove repository reads
// avoid scanning the organization's unrelated retained message history.
func TestMessagesNormalPlannerHistory(t *testing.T) {
	pool, _, me, _ := perfFixture(t, 5000) // 40,000 unrelated messages.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE bus_messages SET from_remote='host/unrelated/repo',from_user=$1,to_user=$1,from_device=$2`, me.UserID, me.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE bus_messages SET from_remote='host/target/repo' WHERE id='m1-0'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `VACUUM ANALYZE bus_messages`); err != nil {
		t.Fatal(err)
	}
	// The caller authors the anchor; all seven other eligible rows in its thread
	// remain available even though they carry a different remote.
	var caller string
	if err := pool.QueryRow(ctx, `SELECT from_user::text FROM bus_messages WHERE id='m1-0'`).Scan(&caller); err != nil {
		t.Fatal(err)
	}
	// Compare identical vacuumed data with the new index temporarily absent.
	// Rollback restores the index before exercising the final query plans.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DROP INDEX bus_messages_remote_idx`); err != nil {
		t.Fatal(err)
	}
	var baselineRaw []byte
	if err = tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+messagesSQL(false), caller, []string{"host/target/repo"}, []string{"/target/main"}, []string{"/target/main"}, "", "", "", nil, nil, nil, "", 21, me.DeviceID).Scan(&baselineRaw); err != nil {
		t.Fatal(err)
	}
	var baseline []map[string]any
	if err = json.Unmarshal(baselineRaw, &baseline); err != nil {
		t.Fatal(err)
	}
	baselineBlocks := baseline[0]["Plan"].(map[string]any)["Shared Hit Blocks"].(float64)
	t.Logf("without remote index: execution_ms=%v shared_hit_blocks=%v", baseline[0]["Execution Time"], baselineBlocks)
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, thread := range []string{"", "m1-0"} {
		var raw []byte
		err := pool.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+messagesSQL(thread != ""), caller, []string{"host/target/repo"}, []string{"/target/main"}, []string{"/target/main"}, "", "", thread, nil, nil, nil, "", 21, me.DeviceID).Scan(&raw)
		if err != nil {
			t.Fatal(err)
		}
		var plan []map[string]any
		if err = json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		rootPlan := plan[0]["Plan"].(map[string]any)
		if thread == "" && rootPlan["Shared Hit Blocks"].(float64) >= baselineBlocks/4 {
			t.Errorf("indexed repository read still consumes history-sized buffers: %v vs %v", rootPlan["Shared Hit Blocks"], baselineBlocks)
		}
		t.Logf("thread=%q normal planner: execution_ms=%v shared_hit_blocks=%v shared_read_blocks=%v", thread, plan[0]["Execution Time"], rootPlan["Shared Hit Blocks"], rootPlan["Shared Read Blocks"])
		// Actual scans of bus_messages must use bounded indexed predicates.
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if x["Relation Name"] == "bus_messages" && x["Node Type"] == "Seq Scan" {
					t.Errorf("whole history scan: %v", x)
				}
				for _, v := range x {
					walk(v)
				}
			case []any:
				for _, v := range x {
					walk(v)
				}
			}
		}
		for _, root := range plan {
			walk(root)
		}
		if !strings.Contains(string(raw), "bus_messages_thread_idx") {
			t.Error("candidate rows did not use thread index")
		}
	}
}
