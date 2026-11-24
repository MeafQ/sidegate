package catalogue

import (
	"container/heap"
	"time"

	"sidegate/internal/domain"
)

// Queue orders refresh deadlines for the controller, which alone uses it. One timer
// tracks the earliest deadline. A source cannot have overlapping refreshes, and
// there is no goroutine per source. It holds deadlines only; FetchState stays
// with the controller's durable state.
type Queue struct {
	due       dueHeap
	positions map[domain.SourceID]int
	inFlight  map[domain.SourceID]flight
}

// flight is one fetch Next handed out, until Finished. A source is in the heap or in
// flight, never both.
type flight struct {
	since time.Time // When Next handed it out.
	// The deadline Set during the fetch, which Finished queues. The zero time is a real
	// deadline, due at once, so whether one was set is its own field.
	due time.Time
	set bool
}

type entry struct {
	id  domain.SourceID
	due time.Time
}

func NewQueue() *Queue {
	q := &Queue{positions: map[domain.SourceID]int{}, inFlight: map[domain.SourceID]flight{}}
	q.due.positions = q.positions
	return q
}

// Set schedules or reschedules id. While id is in flight the deadline waits for
// Finished, so a manual refresh coalesces into the running fetch instead of
// starting a second one, and the latest Set wins.
func (q *Queue) Set(id domain.SourceID, due time.Time) {
	if f, ok := q.inFlight[id]; ok {
		f.due, f.set = due, true
		q.inFlight[id] = f
		return
	}
	if i, ok := q.positions[id]; ok {
		q.due.entries[i].due = due
		heap.Fix(&q.due, i)
		return
	}
	heap.Push(&q.due, entry{id: id, due: due})
}

// Remove forgets id's deadline. A fetch of it in flight stays in flight until Finished,
// so an id saved again meanwhile waits for that fetch instead of overlapping it.
func (q *Queue) Remove(id domain.SourceID) {
	if f, ok := q.inFlight[id]; ok {
		q.inFlight[id] = flight{since: f.since}
	}
	if i, ok := q.positions[id]; ok {
		heap.Remove(&q.due, i)
	}
}

// Next pops up to capacity due sources, marks them in flight, and returns when to
// wake next. The zero wakeAt means nothing is scheduled.
func (q *Queue) Next(now time.Time, capacity int) (due []domain.SourceID, wakeAt time.Time) {
	for len(due) < capacity && q.due.Len() > 0 && !q.due.entries[0].due.After(now) {
		e := heap.Pop(&q.due).(entry)
		q.inFlight[e.id] = flight{since: now}
		due = append(due, e.id)
	}
	if q.due.Len() > 0 {
		wakeAt = q.due.entries[0].due
	}
	return due, wakeAt
}

// Since is when Next handed out id's fetch in flight, and the zero time when none is.
func (q *Queue) Since(id domain.SourceID) time.Time { return q.inFlight[id].since }

// Finished clears the in-flight mark once the controller has handled the Refresh,
// and queues the deadline Set during the fetch, if any.
func (q *Queue) Finished(id domain.SourceID) {
	f, ok := q.inFlight[id]
	if !ok {
		return
	}
	delete(q.inFlight, id)
	if f.set {
		q.Set(id, f.due)
	}
}

// dueHeap is a min-heap by deadline that keeps positions current for O(log n)
// reschedule and removal.
type dueHeap struct {
	entries   []entry
	positions map[domain.SourceID]int
}

func (h dueHeap) Len() int { return len(h.entries) }
func (h dueHeap) Less(i, j int) bool {
	if h.entries[i].due.Equal(h.entries[j].due) {
		return h.entries[i].id < h.entries[j].id
	}
	return h.entries[i].due.Before(h.entries[j].due)
}
func (h dueHeap) Swap(i, j int) {
	h.entries[i], h.entries[j] = h.entries[j], h.entries[i]
	h.positions[h.entries[i].id] = i
	h.positions[h.entries[j].id] = j
}
func (h *dueHeap) Push(x any) {
	e := x.(entry)
	h.positions[e.id] = len(h.entries)
	h.entries = append(h.entries, e)
}
func (h *dueHeap) Pop() any {
	e := h.entries[len(h.entries)-1]
	h.entries = h.entries[:len(h.entries)-1]
	delete(h.positions, e.id)
	return e
}
