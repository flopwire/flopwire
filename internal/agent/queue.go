package agent

// queue is a FIFO of targets with constant-time push, pop and removal. A
// target links itself into the one queue it is in (target.qnext, qprev,
// in), so moving it between queues (a promotion) does not search. Guarded
// by Agent.mu.
type queue struct {
	head, tail *target
	n          int
	steps      int // targets examined, for the complexity guard (queue_test.go)
}

func (q *queue) len() int { return q.n }

// has reports whether t is in q.
func (q *queue) has(t *target) bool {
	q.steps++
	return t.in == q
}

func (q *queue) push(t *target) {
	if t.in != nil {
		t.in.remove(t)
	}
	q.steps++
	t.in, t.qprev, t.qnext = q, q.tail, nil
	if q.tail != nil {
		q.tail.qnext = t
	} else {
		q.head = t
	}
	q.tail = t
	q.n++
}

// pop removes and returns the first target, nil when q is empty.
func (q *queue) pop() *target {
	t := q.head
	if t != nil {
		q.remove(t)
	}
	return t
}

// remove takes t out of q; a target not in q is left alone.
func (q *queue) remove(t *target) {
	q.steps++
	if t.in != q {
		return
	}
	if t.qprev != nil {
		t.qprev.qnext = t.qnext
	} else {
		q.head = t.qnext
	}
	if t.qnext != nil {
		t.qnext.qprev = t.qprev
	} else {
		q.tail = t.qprev
	}
	t.in, t.qprev, t.qnext = nil, nil, nil
	q.n--
}

// targets lists q in order.
func (q *queue) targets() []*target {
	out := make([]*target, 0, q.n)
	for t := q.head; t != nil; t = t.qnext {
		out = append(out, t)
	}
	return out
}
