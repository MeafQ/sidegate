package catalogue

import (
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

func TestQueueOrdersByDeadline(t *testing.T) {
	q := NewQueue()
	q.Set("c", t0.Add(3*time.Minute))
	q.Set("a", t0.Add(1*time.Minute))
	q.Set("b", t0.Add(2*time.Minute))
	q.Set("c", t0.Add(30*time.Second)) // Reschedule moves c to the front.

	due, wake := q.Next(t0, 10)
	if len(due) != 0 || !wake.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("Next(t0) = %v, %v; want nothing due, wake at +30s", due, wake)
	}
	due, wake = q.Next(t0.Add(2*time.Minute), 2)
	if !slices.Equal(due, []domain.SourceID{"c", "a"}) || !wake.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("Next(+2m, 2) = %v, %v; want [c a] and b still due", due, wake)
	}
	due, wake = q.Next(t0.Add(2*time.Minute), 2)
	if !slices.Equal(due, []domain.SourceID{"b"}) || !wake.IsZero() {
		t.Fatalf("Next = %v, %v; want [b] and an empty queue", due, wake)
	}
}

func TestQueueNeverOverlapsAFetch(t *testing.T) {
	q := NewQueue()
	q.Set("a", t0)
	if due, _ := q.Next(t0, 1); !slices.Equal(due, []domain.SourceID{"a"}) {
		t.Fatalf("due = %v", due)
	}
	q.Set("a", t0) // Manual refresh while in flight.
	if due, _ := q.Next(t0.Add(time.Hour), 5); len(due) != 0 {
		t.Fatalf("a in-flight source was handed out again: %v", due)
	}
	q.Set("a", t0.Add(6*time.Hour)) // The controller's reschedule from Next.Due wins.
	q.Finished("a")
	if due, wake := q.Next(t0.Add(time.Hour), 5); len(due) != 0 || !wake.Equal(t0.Add(6*time.Hour)) {
		t.Fatalf("after Finished: %v, %v; want a at +6h", due, wake)
	}
}

func TestQueueRemove(t *testing.T) {
	q := NewQueue()
	q.Set("a", t0)
	q.Set("b", t0.Add(time.Minute))
	q.Remove("a")
	if due, wake := q.Next(t0, 5); len(due) != 0 || !wake.Equal(t0.Add(time.Minute)) {
		t.Fatalf("removed source still queued: %v, %v", due, wake)
	}
	due, _ := q.Next(t0.Add(time.Minute), 5)
	q.Set("b", t0) // Reschedule during the fetch of a deleted source.
	q.Remove("b")
	q.Finished("b")
	if !slices.Equal(due, []domain.SourceID{"b"}) {
		t.Fatalf("due = %v", due)
	}
	if due, wake := q.Next(t0.Add(time.Hour), 5); len(due) != 0 || !wake.IsZero() {
		t.Fatalf("deleted in-flight source came back: %v, %v", due, wake)
	}
}

func TestQueueKeepsADueNowSetDuringAFetch(t *testing.T) {
	q := NewQueue()
	const source = domain.SourceID("a")
	q.Set(source, FetchState{}.Due(source, domain.Remote{})) // Never fetched: due at the zero time.
	if due, _ := q.Next(t0, 1); !slices.Equal(due, []domain.SourceID{"a"}) {
		t.Fatalf("due = %v", due)
	}
	q.Set(source, FetchState{}.Due(source, domain.Remote{})) // The location changed mid-fetch, so its state was reset.
	q.Finished(source)
	if due, _ := q.Next(t0, 1); !slices.Equal(due, []domain.SourceID{"a"}) {
		t.Fatalf("after Finished: due = %v, want [a]", due)
	}
}
