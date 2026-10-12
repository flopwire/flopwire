package format

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func lexicalProbe(docs []string) func(context.Context, SearchMatch, int) (int, error) {
	return func(_ context.Context, m SearchMatch, cap int) (int, error) {
		n := 0
		for _, doc := range docs {
			words := strings.Fields(doc)
			matches := !m.Any
			for _, w := range m.Words {
				has := slices.Contains(words, w)
				if m.Any {
					matches = matches || has
				} else {
					matches = matches && has
				}
			}
			for _, p := range m.Phrases {
				matches = matches && strings.Contains(doc, p)
			}
			if matches {
				n++
				if n == cap {
					break
				}
			}
		}
		return n, nil
	}
}

func TestSearchRelaxation(t *testing.T) {
	docs := []string{"autoreview review only", "review ux automatic edits", "review ux off", "review always edits"}
	for _, query := range []string{"autoreview automatic edits off always review only", "autoreview autoreview, missing? review", "how did autoreview review vanish?"} {
		m, err := PlanSearch(context.Background(), query, "", lexicalProbe(docs))
		if err != nil || m.Any || !slices.Contains(m.Words, "autoreview") || m.Note == "" {
			t.Fatalf("%q: %+v %v", query, m, err)
		}
		n, _ := lexicalProbe(docs)(context.Background(), m, 10)
		if n != 1 {
			t.Fatalf("%q matched %d docs: %+v", query, n, m)
		}
	}
	m, err := PlanSearch(context.Background(), `autoreview missing "review ux"`, "auto", lexicalProbe(docs))
	if err != nil || m.Raw == "" || m.Note != "" {
		t.Fatalf("must not discard anchor/phrase: %+v %v", m, err)
	}
	for _, query := range []string{"nonexistent vanished", "how did we", "review OR autoreview", "review -ux", `"OR -ux"`} {
		m, err := PlanSearch(context.Background(), query, "auto", lexicalProbe(nil))
		if err != nil || m.Note != "" || m.Raw != query {
			t.Fatalf("strict %q: %+v %v", query, m, err)
		}
	}
}

func TestSearchModesAndSafety(t *testing.T) {
	for _, mode := range []string{"all", "any"} {
		m, err := PlanSearch(context.Background(), `can't café? "review only"`, mode, func(context.Context, SearchMatch, int) (int, error) {
			t.Fatal("explicit modes must not probe")
			return 0, nil
		})
		if err != nil || m.Any != (mode == "any") || !slices.Contains(m.Phrases, "review only") {
			t.Fatalf("%s: %+v %v", mode, m, err)
		}
		if !strings.Contains(m.WebQuery(), "can't") || !strings.Contains(m.WebQuery(), "café?") {
			t.Fatalf("punctuation lost: %+v", m)
		}
	}
	if _, err := SearchMatching("bogus"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	for _, query := range []string{"alpha OR beta", "alpha -beta"} {
		m, _ := PlanSearch(context.Background(), query, "any", nil)
		if m.WebQuery() != query {
			t.Fatalf("operator query changed: %+v", m)
		}
	}
}

func TestSearchIncompleteEvidence(t *testing.T) {
	calls := 0
	_, err := PlanSearch(context.Background(), "alpha beta", "auto", func(context.Context, SearchMatch, int) (int, error) {
		calls++
		if calls == 3 {
			return 0, context.DeadlineExceeded
		}
		return 0, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 3 {
		t.Fatalf("continued incomplete evidence: %d %v", calls, err)
	}
	m, err := PlanSearch(context.Background(), "alpha beta", "auto", func(_ context.Context, _ SearchMatch, cap int) (int, error) {
		if cap == 1 {
			return 0, nil
		}
		return cap, nil
	})
	if err != nil || !strings.Contains(m.Note, "rarity probe limit") || m.Raw != "alpha beta" {
		t.Fatalf("capped evidence invented anchor: %+v %v", m, err)
	}
	probes := map[string]int{}
	base := lexicalProbe([]string{"alpha beta", "beta other"})
	_, err = PlanSearch(context.Background(), "alpha alpha, beta beta? missing", "auto", func(ctx context.Context, m SearchMatch, cap int) (int, error) {
		if m.Raw == "" && len(m.Words) == 1 {
			probes[m.Words[0]]++
		}
		return base(ctx, m, cap)
	})
	if err != nil || probes["alpha"] != 1 || probes["beta"] != 1 || probes["missing"] != 1 {
		t.Fatalf("duplicate probes: %v %v", probes, err)
	}
}

func TestSearchRarityProbesShrink(t *testing.T) {
	base := lexicalProbe([]string{"alpha beta", "alpha gamma", "alpha extra", "gamma"})
	var caps []int
	match, err := PlanSearch(context.Background(), "alpha beta gamma missing", "auto", func(ctx context.Context, m SearchMatch, cap int) (int, error) {
		if m.Raw == "" && len(m.Words) == 1 {
			caps = append(caps, cap)
		}
		return base(ctx, m, cap)
	})
	if err != nil || !slices.Equal(caps, []int{SearchProbeCap, 3, 1, 1}) || !slices.Contains(match.Words, "beta") || !strings.Contains(match.Note, "rarest matching term: beta") {
		t.Fatalf("probe bounds=%v match=%+v error=%v", caps, match, err)
	}
}
