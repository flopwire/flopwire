package redact

// A case-insensitive Aho-Corasick automaton over every rule keyword and
// assignment word, compiled to a dense DFA, so one pass over a segment
// finds every place a rule could match. Rules then run their regex only
// on a window around each hit, which keeps redaction near memory speed
// on the common line that holds nothing secret.

type acHit struct {
	pat int // index into acPatterns
	end int // offset just past the match in the scanned buffer
}

type acPattern struct {
	text   []byte // as written (case-sensitive keywords are checked against it)
	rules  []int  // indexes into rules that use this keyword
	assign bool   // an assignment word
}

type automaton struct {
	next []int32 // state*256 + byte -> state
	out  [][]int // state -> pattern indexes ending here (failure closure)
}

var (
	acPatterns []acPattern
	ac         automaton
)

func fold(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

func buildAutomaton() {
	idx := map[string]int{}
	add := func(kw string) int {
		k := string(foldBytes([]byte(kw)))
		if i, ok := idx[k+"\x00"+kw]; ok {
			return i
		}
		idx[k+"\x00"+kw] = len(acPatterns)
		acPatterns = append(acPatterns, acPattern{text: []byte(kw)})
		return len(acPatterns) - 1
	}
	for ri := range rules {
		for _, kw := range rules[ri].keywords {
			i := add(kw)
			acPatterns[i].rules = append(acPatterns[i].rules, ri)
		}
	}
	for _, w := range assignWords {
		i := add(string(w))
		acPatterns[i].assign = true
	}

	// Trie over folded patterns.
	type node struct {
		child map[byte]int
		fail  int
		out   []int
	}
	nodes := []node{{child: map[byte]int{}}}
	for pi, p := range acPatterns {
		s := 0
		for _, c := range foldBytes(p.text) {
			n, ok := nodes[s].child[c]
			if !ok {
				n = len(nodes)
				nodes = append(nodes, node{child: map[byte]int{}})
				nodes[s].child[c] = n
			}
			s = n
		}
		nodes[s].out = append(nodes[s].out, pi)
	}
	// BFS for failure links and the dense transition table.
	next := make([]int32, len(nodes)*256)
	queue := []int{}
	for c := 0; c < 256; c++ {
		if n, ok := nodes[0].child[byte(c)]; ok {
			next[c] = int32(n)
			queue = append(queue, n)
		}
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		f := nodes[s].fail
		nodes[s].out = append(nodes[s].out, nodes[f].out...)
		for c := 0; c < 256; c++ {
			if n, ok := nodes[s].child[byte(c)]; ok {
				nodes[n].fail = int(next[f*256+c])
				next[s*256+c] = int32(n)
				queue = append(queue, n)
			} else {
				next[s*256+c] = next[f*256+c]
			}
		}
	}
	ac.next = next
	ac.out = make([][]int, len(nodes))
	for i := range nodes {
		ac.out[i] = nodes[i].out
	}
	// Fold the input bytes by making uppercase transitions equal to
	// lowercase ones.
	for s := range nodes {
		for c := 'A'; c <= 'Z'; c++ {
			next[s*256+int(c)] = next[s*256+int(c)+32]
		}
	}
}

func foldBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = fold(c)
	}
	return out
}

// scan appends every keyword hit in b to hits.
func (a *automaton) scan(b []byte, hits []acHit) []acHit {
	next, out := a.next, a.out
	var s int32
	for i, c := range b {
		s = next[int(s)<<8|int(c)]
		if s != 0 {
			if o := out[s]; len(o) > 0 {
				for _, p := range o {
					hits = append(hits, acHit{pat: p, end: i + 1})
				}
			}
		}
	}
	return hits
}
