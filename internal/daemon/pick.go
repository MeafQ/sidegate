package daemon

import (
	"time"

	"sidegate/internal/domain"
)

// preferFor is how long a pick's boost lasts. It fades to nothing over it.
const preferFor = 24 * time.Hour

// boost is what a pick takes off its server's price: Size at At, fading to nothing over preferFor. Size
// is the gap the press's pass left its server behind the best member, in price's units, ms times the
// factor: a boost that ties it for first.
type boost struct {
	At   time.Time
	Size float64
}

// left is what is left of the boost: slowly at first and faster later, three quarters after 12 hours
// (owner, 2026-11-12), and nothing at 24.
func (b boost) left(now time.Time) float64 {
	t := min(max(now.Sub(b.At), 0), preferFor).Hours() / preferFor.Hours()
	return b.Size * (1 - t*t)
}

func (b boost) endsAt() time.Time { return b.At.Add(preferFor) }

func (b boost) holds(now time.Time) bool { return now.Before(b.endsAt()) }

func (b boost) same(o boost) bool { return b.At.Equal(o.At) && b.Size == o.Size }

// preference is a boost that holds on one server, as a reader gets it.
type preference struct {
	Server domain.LineageID
	boost
}

// pickAt is where an owner's pick by hand stands. nil is none.
type pickAt interface{ pickAt() }

// queuedPick is a press's move, committed with its answer, until the evaluate that makes it. Its boost is
// on the move's lineage, zero when the press left no gap.
type queuedPick struct {
	move  domain.ServerRef
	boost boost
}

// heldPick is a pick the owner is on: the boost its server keeps while the owner serves it, until its day
// ends.
type heldPick struct {
	server domain.LineageID
	boost  boost
}

func (queuedPick) pickAt() {}
func (heldPick) pickAt()   {}

// handPick is an owner's pick by hand and the owner's last word: when the Test of its last press to land
// began, or when its last Clear that cleared a boost landed. A press whose Test began before the last word
// moves nothing, so the last word stands. No press is out across a restart, so the last word lives in
// memory only.
type handPick struct {
	at   pickAt
	said time.Time
}

type pickEvent interface{ pickEvent() }

// pressed is a press whose Test began at began landing on move. gap is the boost a new one would start
// with, read only when the press keeps no boost.
type pressed struct {
	move  domain.ServerRef
	began time.Time
	gap   func() float64
}

// cleared is a Clear of the boost the page showed on server, ending at endsAt.
type cleared struct {
	server domain.LineageID
	endsAt time.Time
}

// settledOn is an evaluate leaving the owner on selection.
type settledOn struct{ selection domain.Selection }

// choseAnew is this memory going to a new Exit.
type choseAnew struct{}

// restarted is the pick as state.db loads it.
type restarted struct{}

func (pressed) pickEvent()   {}
func (cleared) pickEvent()   {}
func (settledOn) pickEvent() {}
func (choseAnew) pickEvent() {}
func (restarted) pickEvent() {}

// step is what ev leaves of the pick at now. A press on the server whose boost holds keeps its deadline,
// and one on another starts its own gap. A Clear that names the boost holding ends it and is the last
// word; one that names anything else changes nothing. An evaluate keeps a boost only on the server it
// leaves the owner on, and only while its day lasts.
func (h handPick) step(now time.Time, ev pickEvent) handPick {
	switch ev := ev.(type) {
	case pressed:
		var b boost
		if p := h.boosted(now); p != nil && p.Server == ev.move.Lineage {
			b = p.boost
		} else if size := ev.gap(); size > 0 {
			b = boost{At: now, Size: size}
		}
		return handPick{at: queuedPick{move: ev.move, boost: b}, said: ev.began}
	case cleared:
		if p := h.boosted(now); p == nil || p.Server != ev.server || !p.endsAt().Equal(ev.endsAt) {
			return h
		}
		h.said = now
		if q, ok := h.at.(queuedPick); ok {
			h.at = queuedPick{move: q.move}
		} else {
			h.at = nil
		}
	case settledOn:
		server, serving := ev.selection.Server()
		switch at := h.at.(type) {
		case queuedPick:
			h.at = nil
			if at.boost.Size > 0 && at.boost.holds(now) && serving && server.Lineage == at.move.Lineage {
				h.at = heldPick{server: at.move.Lineage, boost: at.boost}
			}
		case heldPick:
			if !at.boost.holds(now) || !serving || server.Lineage != at.server {
				h.at = nil
			}
		}
	case choseAnew:
		return handPick{}
	case restarted:
		h.said = time.Time{}
		switch at := h.at.(type) {
		case queuedPick:
			at.boost.At = earliest(at.boost.At, now)
			h.at = at
		case heldPick:
			at.boost.At = earliest(at.boost.At, now)
			h.at = at
		}
	}
	return h
}

func earliest(a, b time.Time) time.Time {
	if a.After(b) {
		return b
	}
	return a
}

// stale says a press whose Test began at began came before the owner's last word.
func (h handPick) stale(began time.Time) bool { return began.Before(h.said) }

// queued is the move a press asked for, until the evaluate that makes it.
func (h handPick) queued() (domain.ServerRef, bool) {
	q, ok := h.at.(queuedPick)
	return q.move, ok
}

// boosted is the boost while it holds at now, queued or held, and nil once its day is over, whether or
// not an evaluate has ended it yet.
func (h handPick) boosted(now time.Time) *preference {
	switch at := h.at.(type) {
	case queuedPick:
		if at.boost.holds(now) {
			return &preference{Server: at.move.Lineage, boost: at.boost}
		}
	case heldPick:
		if at.boost.holds(now) {
			return &preference{Server: at.server, boost: at.boost}
		}
	}
	return nil
}

// sameStored compares what state.db holds of two picks. The last word is memory only.
func (h handPick) sameStored(o handPick) bool {
	switch a := h.at.(type) {
	case queuedPick:
		b, ok := o.at.(queuedPick)
		return ok && a.move == b.move && a.boost.same(b.boost)
	case heldPick:
		b, ok := o.at.(heldPick)
		return ok && a.server == b.server && a.boost.same(b.boost)
	}
	return o.at == nil
}
