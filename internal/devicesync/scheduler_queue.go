package devicesync

import (
	"container/heap"
	"container/list"
	"strings"
	"time"
)

type queueLane uint8

const (
	historicalLane queueLane = iota
	changedLane
	interactiveLane
)

type scheduleHints struct {
	ActivityAt, WaitingSince time.Time
}

type queueEntry struct {
	path    string
	job     *job
	lane    queueLane // index placement, updated when the job is reindexed
	due     time.Time
	element *list.Element
	indexes [3]int
}

type queueHeapKind uint8

const (
	recentHeap queueHeapKind = iota
	oldestHeap
	delayedHeap
)

type jobHeap struct {
	kind  queueHeapKind
	items []*queueEntry
}

func (h jobHeap) Len() int { return len(h.items) }
func (h jobHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	var x, y time.Time
	switch h.kind {
	case recentHeap:
		x, y = a.job.hints.ActivityAt, b.job.hints.ActivityAt
	case oldestHeap:
		x, y = a.job.hints.WaitingSince, b.job.hints.WaitingSince
	case delayedHeap:
		x, y = a.due, b.due
	}
	if x.Equal(y) {
		return strings.Compare(a.path, b.path) < 0
	}
	if h.kind == recentHeap {
		return x.After(y)
	}
	return x.Before(y)
}
func (h jobHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].indexes[h.kind], h.items[j].indexes[h.kind] = i, j
}
func (h *jobHeap) Push(x any) {
	e := x.(*queueEntry)
	e.indexes[h.kind] = len(h.items)
	h.items = append(h.items, e)
}
func (h *jobHeap) Pop() any {
	n := len(h.items) - 1
	e := h.items[n]
	h.items[n] = nil
	h.items = h.items[:n]
	e.indexes[h.kind] = -1
	return e
}

// turnQueue owns only scheduling indexes. Scheduler.ready remains the source
// of truth for queued jobs. Delayed jobs enter a lane only when eligible.
type turnQueue struct {
	interactive, changed    list.List
	recent, oldest, delayed jobHeap
	slot                    int
	historicalTurns         uint64
}

var turnSlots = [...]queueLane{interactiveLane, interactiveLane, interactiveLane, interactiveLane, changedLane, changedLane, historicalLane}

func newTurnQueue() turnQueue {
	return turnQueue{recent: jobHeap{kind: recentHeap}, oldest: jobHeap{kind: oldestHeap}, delayed: jobHeap{kind: delayedHeap}}
}

func (q *turnQueue) insert(path string, j *job, due, now time.Time) {
	if e := j.queued; e != nil && e.lane == j.lane {
		wasDelayed := e.indexes[delayedHeap] >= 0
		if wasDelayed == now.Before(due) {
			e.due = due
			if wasDelayed {
				heap.Fix(&q.delayed, e.indexes[delayedHeap])
			} else if e.lane == historicalLane {
				heap.Fix(&q.recent, e.indexes[recentHeap])
				heap.Fix(&q.oldest, e.indexes[oldestHeap])
			}
			return
		}
	}
	q.remove(j)
	e := &queueEntry{path: path, job: j, lane: j.lane, due: due, indexes: [3]int{-1, -1, -1}}
	j.queued = e
	if now.Before(due) {
		heap.Push(&q.delayed, e)
	} else {
		q.admit(e)
	}
}

func (q *turnQueue) admit(e *queueEntry) {
	switch e.lane {
	case interactiveLane:
		e.element = q.interactive.PushBack(e)
	case changedLane:
		e.element = q.changed.PushBack(e)
	default:
		heap.Push(&q.recent, e)
		heap.Push(&q.oldest, e)
	}
}

func (q *turnQueue) remove(j *job) {
	e := j.queued
	if e == nil {
		return
	}
	if e.element != nil {
		if e.lane == interactiveLane {
			q.interactive.Remove(e.element)
		} else {
			q.changed.Remove(e.element)
		}
	}
	for _, h := range []*jobHeap{&q.recent, &q.oldest, &q.delayed} {
		if e.indexes[h.kind] >= 0 {
			heap.Remove(h, e.indexes[h.kind])
		}
	}
	j.queued = nil
}

func (q *turnQueue) promote(now time.Time) {
	for len(q.delayed.items) > 0 && !now.Before(q.delayed.items[0].due) {
		q.admit(heap.Pop(&q.delayed).(*queueEntry))
	}
}

func (q *turnQueue) laneEntry(lane queueLane) *queueEntry {
	switch lane {
	case interactiveLane:
		if e := q.interactive.Front(); e != nil {
			return e.Value.(*queueEntry)
		}
	case changedLane:
		if e := q.changed.Front(); e != nil {
			return e.Value.(*queueEntry)
		}
	default:
		if len(q.recent.items) > 0 {
			if (q.historicalTurns+1)%8 == 0 {
				return q.oldest.items[0]
			}
			return q.recent.items[0]
		}
	}
	return nil
}

// peek does not spend a slot or an oldest-waiter turn. Both the idle timer
// and dispatch call it, so selection accounting belongs to take.
func (q *turnQueue) peek(now time.Time) (*queueEntry, time.Duration) {
	q.promote(now)
	e := q.laneEntry(turnSlots[q.slot])
	if e == nil {
		for _, lane := range [...]queueLane{interactiveLane, changedLane, historicalLane} {
			if e = q.laneEntry(lane); e != nil {
				break
			}
		}
	}
	if e != nil {
		return e, 0
	}
	if len(q.delayed.items) > 0 {
		return nil, max(q.delayed.items[0].due.Sub(now), time.Millisecond)
	}
	return nil, time.Hour
}

func (q *turnQueue) take(now time.Time) (*queueEntry, time.Duration) {
	e, wait := q.peek(now)
	if e == nil {
		return nil, wait
	}
	q.slot = (q.slot + 1) % len(turnSlots)
	if e.lane == historicalLane {
		q.historicalTurns++
	}
	q.remove(e.job)
	return e, 0
}

func mergeJobHints(dst, src *job) {
	if src.lane > dst.lane {
		dst.lane = src.lane
	}
	if src.hints.ActivityAt.After(dst.hints.ActivityAt) {
		dst.hints.ActivityAt = src.hints.ActivityAt
	}
	if !src.hints.WaitingSince.IsZero() && (dst.hints.WaitingSince.IsZero() || src.hints.WaitingSince.Before(dst.hints.WaitingSince)) {
		dst.hints.WaitingSince = src.hints.WaitingSince
	}
}
