package runtime

import (
	"fmt"
	"maps"
	"sync"
	"time"

	"sidegate/internal/domain"
)

// gates is the one writer of this process's pxa link flags. The newest plan, the claims, and every up
// and down this process writes change under mu, so an opening that began before a plan or a deadline
// closed its slot cannot land after the close. Nothing under mu runs a tool, and no caller holds mu
// across a core call, so intake waits at most one flag write or one event heard.
type gates struct {
	mu     sync.Mutex
	plan   *Plan
	claims claims
	write  func(link string, up bool) error // SIOCSIFFLAGS. Tests and the lab wrap it.
}

// claims is what the gates trust across passes. of is what the last good read after repair showed
// each slot applied on, which intake trusts. vouched is the part of of the unread gate may ride past a
// deadline: a slot leaves it at any write to it and at any answer that rules out its Selection in of,
// and comes back on a good full read that shows it on that Selection. epoch is the start the
// publishing read named.
type claims struct {
	of      map[domain.SlotID]domain.Selection
	vouched map[domain.SlotID]bool
	epoch   domain.CoreEpoch
}

// writeLink is what a Worker's gates write with, the ioctl. A lab build wraps it (gates_lab.go).
var writeLink = setFlag

// newest is the newest plan intake took, or nil before the first.
func (g *gates) newest() *Plan {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.plan
}

// take is intake's one step. It stores a new plan, or keeps the newest at a deadline when p is nil,
// closes every owned link the plan doesn't let be up, and returns the plan's next deadline, or zero
// when it has none. It reads the clock once it holds mu, so a deadline that came due while it waited
// on an opening closes now.
func (g *gates) take(p *Plan) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p != nil {
		g.plan = p
	}
	if g.plan == nil {
		return time.Time{}
	}
	now := time.Now()
	g.closeForbidden(now)
	next, _ := g.plan.nextDeadline(now)
	return next
}

// closeForbidden closes every owned link the newest plan refuses at now under the claims. mu is held.
func (g *gates) closeForbidden(now time.Time) {
	if g.plan == nil {
		return
	}
	for _, link := range g.plan.closing(now, g.claims.of) {
		_ = g.set(link, false)
	}
}

// publish stores what a good read after repair showed, once a pass, every slot of it vouched.
func (g *gates) publish(c claims) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.claims = c
}

// hear is the cross-pass half of the one transition. A failed add drops its slot, and a start other
// than the claims', or no running core, drops every claim: each closes at once what the plan then
// forbids, since a deadline may already have been spent while the claim stood. A write or an answer
// against a slot's Selection ends its unread ride, and a good read on the claims' start that shows it
// there brings the ride back.
func (g *gates) hear(e event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch e := e.(type) {
	case addFailed:
		if _, held := g.claims.of[e.slot]; held {
			delete(g.claims.of, e.slot)
			delete(g.claims.vouched, e.slot)
			g.closeForbidden(time.Now())
		}
	case startRead:
		g.restart(e.epoch)
	case goodRead:
		if g.restart(e.obs.Epoch) {
			return
		}
		for id, sel := range g.claims.of {
			if e.obs.Applied[id] == sel {
				g.claims.vouched[id] = true
			}
		}
	case wrote:
		maps.DeleteFunc(g.claims.vouched, func(id domain.SlotID, _ bool) bool { return e.touches(id) })
	case answer:
		maps.DeleteFunc(g.claims.vouched, func(id domain.SlotID, _ bool) bool { return e.rulesOut(id, g.claims.of[id]) })
	}
}

// restart empties the claims when epoch isn't the start they stand on, and says whether it did. The
// first start named drops nothing, since there is nothing yet.
func (g *gates) restart(epoch domain.CoreEpoch) bool {
	if epoch == g.claims.epoch {
		return false
	}
	held := len(g.claims.of) > 0
	g.claims = claims{of: map[domain.SlotID]domain.Selection{}, vouched: map[domain.SlotID]bool{}, epoch: epoch}
	if held {
		g.closeForbidden(time.Now())
	}
	return true
}

// vouches is the Selection the unread gate may ride past a deadline for slot: its claim while
// vouched, else unclaimed.
func (g *gates) vouches(slot domain.SlotID) domain.Selection {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.claims.vouched[slot] {
		return unclaimed
	}
	return g.claims.of[slot]
}

// stale is an opening's first step, before its confirmation reads the core: why no read could let it
// land, a newer plan or a deadline that holds slot closed whatever the read shows.
func (g *gates) stale(planned *Plan, slot domain.SlotID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, _ := planned.target(slot)
	return g.refuses(planned, slot, t.Selection)
}

// open is an opening's last step, taken under the gate lock once its confirmation has read fresh. It
// sets slot's link up only while planned is the newest plan and lets slot be up on fresh. A move past
// its deadline may still open on its Selection, because only an open gate on its server can report that
// move applied.
func (g *gates) open(planned *Plan, slot domain.SlotID, fresh domain.Selection) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.refuses(planned, slot, fresh); err != nil {
		return err
	}
	t, _ := planned.target(slot)
	return g.set(linkName(t.Slot.Allocation), true)
}

// refuses says why slot's link may not come up for planned now, on a read that showed shown. mu is held.
func (g *gates) refuses(planned *Plan, slot domain.SlotID, shown domain.Selection) error {
	t, ok := planned.target(slot)
	switch {
	case g.plan != planned:
		return fmt.Errorf("a newer plan holds %s closed until the next pass", slot)
	case !ok:
		return fmt.Errorf("slot %s isn't planned", slot)
	case !t.mayBeUp(time.Now(), shown):
		return fmt.Errorf("%s is past its deadline", slot)
	}
	return nil
}

// close sets link down. It is every down a pass writes.
func (g *gates) close(link string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.set(link, false)
}

// set is every flag write. It reads link's group just before the ioctl and writes only a link in ours:
// another program can take the name between the pass's read and the write. mu is held.
func (g *gates) set(link string, up bool) error {
	if !linkOwned(link) {
		return fmt.Errorf("link %s isn't in our group, so its flag is left as it is", link)
	}
	return g.write(link, up)
}
