package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/fsprobe"
)

func writeChildFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverChildrenForParentsMatchesDiscovery(t *testing.T) {
	root := t.TempDir()
	const parent = "abcdefab-0000-4000-8000-000000000001"
	const upper = "ABCDEFAB-0000-4000-8000-000000000002"
	for _, rel := range []string{
		"a/" + parent + ".jsonl",
		"a/" + parent + "/subagents/agent-flat.jsonl",
		"a/" + parent + "/subagents/agent-flat.meta.json",
		"a/" + parent + "/subagents/workflows/run/deeper/agent-nested.jsonl",
		"a/" + parent + "/subagents/workflows/run/deeper/agent-nested.meta.json",
		"b/" + parent + "/subagents/agent-orphan.jsonl",
		// The same child identity in two parents must keep project/session order.
		"b/" + upper + "/subagents/agent-flat.jsonl",
		"b/not-a-uuid/subagents/agent-invalid.jsonl",
	} {
		writeChildFixture(t, filepath.Join(root, rel))
	}
	// These symlinks must not turn external projects, parents, or children into
	// capture evidence. Sidecar handling remains the shared discoverer's behavior.
	external := t.TempDir()
	writeChildFixture(t, filepath.Join(external, parent, "subagents/agent-external.jsonl"))
	for link, target := range map[string]string{
		filepath.Join(root, "linked-project"):                            external,
		filepath.Join(root, "b", "abcdefab-0000-4000-8000-000000000003"): filepath.Join(external, parent),
		filepath.Join(root, "a", parent, "subagents/agent-linked.jsonl"): filepath.Join(external, parent, "subagents/agent-external.jsonl"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	parents := []string{parent, upper, strings.ToLower(upper), "abcdefab-0000-4000-8000-000000000003", "not-a-uuid", "../escape", parent}
	sessions, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	var want []*Session
	for _, s := range sessions {
		for _, id := range parents {
			if s.SessionID == id && isUUID(id) {
				s.Transcript = "" // Child discovery does not inspect the main transcript.
				want = append(want, s)
				break
			}
		}
	}
	got, err := DiscoverChildrenForParents(root, parents)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("children = %+v; want %+v", got, want)
	}
	if len(got) != 3 || got[0].SessionID != parent || len(got[0].Subagents) != 2 || got[1].SessionID != upper || got[2].SessionID != parent || len(got[2].Subagents) != 1 {
		t.Fatalf("unexpected identities: %+v", got)
	}
	// A refresh must discover newly written children without a cached listing.
	writeChildFixture(t, filepath.Join(root, "b", parent, "subagents/agent-fresh.jsonl"))
	got, err = DiscoverChildrenForParents(root, parents)
	if err != nil || len(got) != 3 || len(got[2].Subagents) != 2 {
		t.Fatalf("fresh children: %+v, %v", got, err)
	}
}

func TestDiscoverChildrenForParentsBoundedFilesystemWork(t *testing.T) {
	root := t.TempDir()
	const parent = "abcdefab-0000-4000-8000-000000000001"
	writeChildFixture(t, filepath.Join(root, "project", parent, "subagents/agent-selected.jsonl"))
	for i := range 100 {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		writeChildFixture(t, filepath.Join(root, "project", id, "subagents/workflows/run/agent-unrelated.jsonl"))
	}
	var lists, stats, opens int
	fsprobe.SetHook(func(op fsprobe.Op, path string) {
		switch op {
		case fsprobe.OpList:
			lists++
		case fsprobe.OpStat:
			stats++
		case fsprobe.OpOpen:
			opens++
		}
		if rel, err := filepath.Rel(filepath.Join(root, "project"), path); err == nil && !strings.HasPrefix(rel, "..") && rel != "." && !strings.HasPrefix(rel, parent+string(filepath.Separator)) && rel != parent {
			t.Errorf("inspected unrelated session: %s", rel)
		}
	})
	defer fsprobe.SetHook(nil)
	children, err := DiscoverChildrenForParents(root, []string{parent})
	if err != nil || len(children) != 1 || len(children[0].Subagents) != 1 {
		t.Fatalf("children %+v, %v", children, err)
	}
	if lists != 4 || stats != 2 || opens != 0 {
		t.Fatalf("filesystem work: lists=%d stats=%d opens=%d", lists, stats, opens)
	}
}

func TestDiscoverChildrenForParentsMissingAndEmpty(t *testing.T) {
	const parent = "abcdefab-0000-4000-8000-000000000001"
	root := filepath.Join(t.TempDir(), "missing")
	if got, err := DiscoverChildrenForParents(root, []string{parent}); got != nil || err != nil {
		t.Fatalf("missing root: %+v, %v", got, err)
	}
	calls := 0
	fsprobe.SetHook(func(fsprobe.Op, string) { calls++ })
	defer fsprobe.SetHook(nil)
	for _, ids := range [][]string{nil, {}, {"../escape", "not-a-uuid"}} {
		if got, err := DiscoverChildrenForParents(root, ids); got != nil || err != nil {
			t.Fatalf("empty parents: %+v, %v", got, err)
		}
	}
	if calls != 0 {
		t.Fatalf("empty parents performed %d filesystem calls", calls)
	}
}

func TestDiscoverChildrenForParentsReportsRelevantErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not restrict root")
	}
	const parent = "abcdefab-0000-4000-8000-000000000001"
	const other = "abcdefab-0000-4000-8000-000000000002"
	root := t.TempDir()
	selected := filepath.Join(root, "project", parent, "subagents")
	unrelated := filepath.Join(root, "project", other, "subagents")
	writeChildFixture(t, filepath.Join(selected, "agent-selected.jsonl"))
	writeChildFixture(t, filepath.Join(unrelated, "agent-other.jsonl"))
	if err := os.Chmod(unrelated, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unrelated, 0755) })
	if got, err := DiscoverChildrenForParents(root, []string{parent}); err != nil || len(got) != 1 || len(got[0].Subagents) != 1 {
		t.Fatalf("unrelated error affected parent: %+v, %v", got, err)
	}
	if err := os.Chmod(selected, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(selected, 0755) })
	if got, err := DiscoverChildrenForParents(root, []string{parent}); err == nil || got != nil {
		t.Fatalf("selected error lost: %+v, %v", got, err)
	}
	if err := os.Chmod(filepath.Join(root, "project"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "project"), 0755) })
	if got, err := DiscoverChildrenForParents(root, []string{parent}); err == nil || got != nil {
		t.Fatalf("project error lost: %+v, %v", got, err)
	}
}
