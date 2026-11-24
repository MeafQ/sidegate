package runtime

import (
	"fmt"
	"maps"
	"time"

	"sidegate/internal/domain"
)

const (
	firstBackoff = time.Second
	maxBackoff   = time.Minute
	// calmReset is how long every pass must find a slot's check passing before its
	// failed checks are forgotten.
	calmReset = time.Minute
)

// backoff limits how often one slot asks for a routing rewrite, and decides nothing
// else: no gate reads it. A slot owes a rewrite once a check fails, its gate closes,
// or what a rewrite would write changes. A rewrite is charged once it has landed and
// a check then fails on the same core: an outage in which none can land spends
// nothing, and neither does a new core, whose routing and overrides a restart reset.
// A check reads only the core, so a gate closed for any other reason spends nothing.
// Each charge doubles the wait before the next rewrite, from a second up to a
// minute. The count belongs to what a rewrite would write, so a plan change is never
// held: a rule set the core has not been given yet reads back as different without
// anything having failed.
type backoff struct {
	routing string
	owed    bool
	open    bool             // The gate as last read or set up; a closure is a rewrite owed once.
	landed  domain.CoreEpoch // The core the last rewrite landed on, until a check fails it.
	fails   int
	wait    time.Duration
	next    time.Time // No rewrite for this slot before then.
	healthy time.Time // Since when every pass found its check passing; zero otherwise.
}

type backoffs map[domain.SlotID]*backoff

// check is one pass's check of a slot's routing.
type check uint8

const (
	checkFailed check = iota
	checkPassed
	// checkUnmade is a slot with an inbound unlisted, as an add on a held port leaves it. Nothing
	// listens to be routed, and a rewrite can't bind a port, so it owes and spends nothing.
	checkUnmade
)

// observe records one pass's check of a slot on the core epoch. A Worker that just
// adopted a slot owes nothing until a check or its gate says so.
func (b backoffs) observe(slot domain.SlotID, routing string, epoch domain.CoreEpoch, c check, now time.Time) {
	s, known := b[slot]
	if !known || s.routing != routing {
		s = &backoff{routing: routing, owed: known, open: true}
		b[slot] = s
	}
	switch {
	case c == checkUnmade:
		s.healthy = time.Time{}
	case c == checkFailed:
		s.owed, s.healthy = true, time.Time{}
		if s.landed != "" && s.landed == epoch {
			s.landed = ""
			s.fails++
			s.wait = min(max(2*s.wait, firstBackoff), maxBackoff)
			s.next = now.Add(s.wait)
		}
	case s.healthy.IsZero():
		s.healthy = now
	case now.Sub(s.healthy) >= calmReset:
		s.landed, s.fails, s.wait, s.next = "", 0, 0, time.Time{}
	}
}

// gated records slot's gate as a read of the kernel shows it, or as an up just set it. A gate
// read closed after it was last read or set open owes a rewrite, though it may open again
// within the pass, before the next pass reads it.
func (b backoffs) gated(slot domain.SlotID, open bool) {
	if s, ok := b[slot]; ok {
		s.owed = s.owed || s.open && !open
		s.open = open
	}
}

// due reports whether slot owes a rewrite and whether it may run now.
func (b backoffs) due(slot domain.SlotID, now time.Time) (owed, due bool) {
	s, ok := b[slot]
	if !ok || !s.owed {
		return false, false
	}
	return true, !now.Before(s.next)
}

// rewritten records that a rewrite slot asked for landed on epoch.
func (b backoffs) rewritten(slot domain.SlotID, epoch domain.CoreEpoch) {
	b[slot].landed, b[slot].owed = epoch, false
}

func (b backoffs) retain(slots map[domain.SlotID]bool) {
	maps.DeleteFunc(b, func(id domain.SlotID, _ *backoff) bool { return !slots[id] })
}

func (b backoffs) held(slot domain.SlotID) error {
	s := b[slot]
	return fmt.Errorf("slot %s: routing rewrite held until %s after %d failed checks",
		slot, s.next.Format("15:04:05"), s.fails)
}
