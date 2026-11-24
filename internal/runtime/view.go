package runtime

import (
	"slices"

	"sidegate/internal/domain"
)

// event is one thing the pass heard from the core, in the order it happened.
type event interface{ heard() }

// goodRead is a full read of the slots it asked about with no readErr, of a running core or a
// stopped one.
type goodRead struct{ obs coreObservation }

// startRead is a unit read that answered, from any caller: the start it names, "" for no running core.
type startRead struct{ epoch domain.CoreEpoch }

// wrote is a write, sent or about to be, with the slots it can change.
type wrote struct {
	every bool
	slots []domain.SlotID
}

var (
	everySlot = wrote{every: true} // A routing replace resets every override, and a start or a stop changes every slot.
	noSlot    = wrote{}            // Outbounds, the probe inbound, probe users and probe rules: no slot reads them.
)

func slotScope(ids ...domain.SlotID) wrote { return wrote{slots: ids} }

func (w wrote) touches(slot domain.SlotID) bool { return w.every || slices.Contains(w.slots, slot) }

// addFailed is an add of slot's inbound that failed with anything but a duplicate, heard as the add
// returns: the tag may be listed with nothing bound, which no read can tell.
type addFailed struct{ slot domain.SlotID }

// addScoped is the start a failed add's cleanup read, which the failure counts on.
type addScoped struct {
	slot domain.SlotID
	on   domain.CoreEpoch
}

// answer is a call that answered, from any caller. rulesOut is observe's test of that call, asked of
// any Selection: true when the answer shows the core can't be serving slot on sel.
type answer interface {
	event
	rulesOut(slot domain.SlotID, sel domain.Selection) bool
}

type (
	inboundList  []string    // ListInbounds.
	outboundList []string    // ListOutbounds.
	ruleList     []routeRule // ListRule.
	// routeAnswer is TestRoute for one of slot's inbounds. matched is false when no rule matched.
	routeAnswer struct {
		slot    domain.SlotID
		tag     string
		matched bool
	}
	// balancerAnswer is GetBalancerInfo of slot's balancer. found is false on NotFound.
	balancerAnswer struct {
		slot     domain.SlotID
		override string
		found    bool
	}
)

// A slot not listening on exactly its own inbounds serves nothing.
func (l inboundList) rulesOut(slot domain.SlotID, _ domain.Selection) bool {
	return !coreObservation{inbounds: l}.listening(slot)
}

// A Selection whose served tag isn't a live outbound fails closed.
func (l outboundList) rulesOut(_ domain.SlotID, sel domain.Selection) bool {
	return !slices.Contains(l, servedTag(sel))
}

// A slot's own rule is its balancer's, which names no outbound. A sibling's rule matches other
// inbounds, so a list that lacks it says nothing of this slot.
func (l ruleList) rulesOut(slot domain.SlotID, _ domain.Selection) bool {
	tag := balancerTag(coreSlot{ID: slot})
	own := slices.DeleteFunc(slices.Clone(l), func(r routeRule) bool { return r.RuleTag != tag })
	return !slices.Equal(own, []routeRule{{RuleTag: tag}})
}

// No rule matched sends the connection to block, and any other tag isn't sel's.
func (r routeAnswer) rulesOut(slot domain.SlotID, sel domain.Selection) bool {
	return r.slot == slot && (!r.matched || r.tag != servedTag(sel))
}

// A missing balancer serves nothing, and an override names what it serves.
func (b balancerAnswer) rulesOut(slot domain.SlotID, sel domain.Selection) bool {
	return b.slot == slot && (!b.found || b.override != servedTag(sel))
}

func (goodRead) heard()       {}
func (startRead) heard()      {}
func (wrote) heard()          {}
func (addFailed) heard()      {}
func (addScoped) heard()      {}
func (inboundList) heard()    {}
func (outboundList) heard()   {}
func (ruleList) heard()       {}
func (routeAnswer) heard()    {}
func (balancerAnswer) heard() {}

// view is what one pass can still say of the core. It begins empty with each pass and is gone when
// the pass ends.
type view struct {
	read    *coreObservation            // The newest good full read; nil before one.
	since   *domain.CoreEpoch           // A start named after read that differs from read's.
	touched map[domain.SlotID]bool      // Slots read shows applied that a write or an answer touched since.
	failed  map[domain.SlotID]failedAdd // Adds of the slot's inbound that failed this pass.
	lost    bool                        // An add failed since read.
}

// failedAdd is a failed add of a slot's inbound: on every start until its cleanup read one, then on
// that one.
type failedAdd struct {
	on     domain.CoreEpoch
	scoped bool
}

// claim is what the pass can still say of one slot.
type claim uint8

const (
	claimNone    claim = iota // Nothing the pass holds shows it: no good read, a touch or another start after it, or not applied.
	claimShown                // The newest good read shows it applied, and nothing since touched it or named another start.
	claimRefuted              // An add of its inbound failed on the start the pass last named, or on one it couldn't read.
)

// hear is the view's one transition. A read's answers are heard before it lands, so a good read
// clears what they touched, being the newer proof, and a read that fails keeps every touch.
func (v *view) hear(e event) {
	switch e := e.(type) {
	case goodRead:
		v.read, v.since, v.touched, v.lost = &e.obs, nil, nil, false
	case startRead:
		if v.read == nil || v.read.Epoch != e.epoch {
			v.since = &e.epoch
		}
	case addFailed:
		v.fail(e.slot, failedAdd{})
	case addScoped:
		v.fail(e.slot, failedAdd{on: e.on, scoped: true})
	case wrote:
		v.touch(func(id domain.SlotID, _ domain.Selection) bool { return e.touches(id) })
	case answer:
		v.touch(e.rulesOut)
	}
}

// touch marks each slot read shows applied that hit says the event touched.
func (v *view) touch(hit func(domain.SlotID, domain.Selection) bool) {
	if v.read == nil {
		return
	}
	for id, sel := range v.read.Applied {
		if hit(id, sel) {
			if v.touched == nil {
				v.touched = map[domain.SlotID]bool{}
			}
			v.touched[id] = true
		}
	}
}

func (v *view) fail(slot domain.SlotID, f failedAdd) {
	if v.failed == nil {
		v.failed = map[domain.SlotID]failedAdd{}
	}
	v.failed[slot], v.lost = f, true
}

// current is the start the pass last named: since, else read's, nil before the pass named one.
func (v *view) current() *domain.CoreEpoch {
	if v.since != nil {
		return v.since
	}
	if v.read != nil {
		return &v.read.Epoch
	}
	return nil
}

// claim decides what the pass can still say of slot: a failed add of it on the current start, or on
// a start it couldn't read, refutes it; else the newest good read shows it, while nothing since
// touched it or named another start.
func (v *view) claim(slot domain.SlotID) claim {
	on := v.current()
	if f, failed := v.failed[slot]; failed && (!f.scoped || on == nil || f.on == *on) {
		return claimRefuted
	}
	if v.read == nil || v.touched[slot] || v.since != nil {
		return claimNone
	}
	if _, ok := v.read.Applied[slot]; ok {
		return claimShown
	}
	return claimNone
}

// claims is every slot the view can claim, on the start the pass last named, each vouched: what the
// gates are given after a good read of repair.
func (v *view) claims() claims {
	c := claims{of: map[domain.SlotID]domain.Selection{}, vouched: map[domain.SlotID]bool{}}
	if on := v.current(); on != nil {
		c.epoch = *on
	}
	if v.read == nil {
		return c
	}
	for id, sel := range v.read.Applied {
		if v.claim(id) == claimShown {
			c.of[id], c.vouched[id] = sel, true
		}
	}
	return c
}

// stale says something since the newest good read keeps a claim it showed from standing: a slot
// touched, an add failed, or another start named.
func (v *view) stale() bool { return len(v.touched) > 0 || v.lost || v.since != nil }
