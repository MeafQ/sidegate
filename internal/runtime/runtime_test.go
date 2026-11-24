package runtime

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
)

func TestNewPlanRejectsInconsistentSlots(t *testing.T) {
	a, b := testRef("srvA"), testRef("srvB")
	servers := map[domain.ServerRef]domain.Server{a: testServer(t, vlessHop(t, "root", "198.51.100.10", ""))}
	target := func(id domain.SlotID, ordinal uint8, sel domain.ServerRef) SlotTarget {
		return SlotTarget{
			Slot: domain.Slot{ID: id, Allocation: domain.Allocation{
				Ordinal: ordinal, Tunnel: netip.MustParsePrefix("10.70.1.0/30"),
				PeerListenPort: 51830 + uint16(ordinal), TPROXYPort: 12340 + uint16(ordinal),
				Keys: domain.SlotKeys{ClientPublic: "pub"},
			}},
			Keys: KeyMaterial{Host: domain.NewSecret("key")}, Selection: domain.Serving(sel),
		}
	}
	cases := map[string][]SlotTarget{
		"no server":        {target("slot1", 1, b)},
		"repeated ordinal": {target("slot1", 1, a), target("slot2", 1, a)},
		"malformed":        {target("b.x", 1, a)},
	}
	for name, slots := range cases {
		if _, err := NewPlan(1, slots, servers, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	withPassage := func(p Passage, sel domain.Selection) SlotTarget {
		t := target("slot1", 1, a)
		t.Passage, t.Selection = p, sel
		return t
	}
	if _, err := NewPlan(1, []SlotTarget{withPassage(MoveFrom(b, backoffStart), domain.Serving(a))}, servers, nil); err != nil {
		t.Errorf("a prior whose closure left the catalogue: %v", err)
	}
	if _, err := NewPlan(1, []SlotTarget{withPassage(BridgeUntil(backoffStart), domain.Serving(a))}, servers, nil); err != nil {
		t.Errorf("a bridge: %v", err)
	}
	for name, slot := range map[string]SlotTarget{
		"a move on a closed slot":    withPassage(MoveFrom(b, backoffStart), domain.Closed(domain.ConfirmedDead)),
		"a bridge on Direct":         withPassage(BridgeUntil(backoffStart), domain.Direct()),
		"a move on Block":            withPassage(MoveFrom(b, backoffStart), domain.Blocked()),
		"a move from its own server": withPassage(MoveFrom(a, backoffStart), domain.Serving(a)),
		"a move from no server":      withPassage(MoveFrom(domain.ServerRef{}, backoffStart), domain.Serving(a)),
		"a move with no deadline":    withPassage(MoveFrom(b, time.Time{}), domain.Serving(a)),
		"a bridge with no deadline":  withPassage(BridgeUntil(time.Time{}), domain.Serving(a)),
		"a move from nothing, ever":  withPassage(MoveFrom(domain.ServerRef{}, time.Time{}), domain.Serving(a)),
	} {
		if _, err := NewPlan(1, []SlotTarget{slot}, servers, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	shared := target("slot2", 2, a)
	shared.Slot.Allocation.TPROXYPort = 51831
	if _, err := NewPlan(1, []SlotTarget{target("slot1", 1, a), shared}, servers, nil); err == nil || !strings.Contains(err.Error(), "ports") {
		t.Errorf("a TPROXY port equal to another slot's WireGuard port: %v", err)
	}
	if _, err := NewPlan(1, []SlotTarget{target("slot1", 1, a), target("slot2", 2, a)}, servers, nil); err != nil {
		t.Errorf("valid plan: %v", err)
	}
}

func TestAGateFollowsTheCheck(t *testing.T) {
	serving := gate{up: true, wanted: true, coreRead: true, kernelRead: true, ingress: true, passed: true, plumbed: true, settled: true, stays: true}
	unread := func(g *gate) { g.coreRead, g.passed, g.onInstalled = false, false, false }
	noKernel := func(g *gate) { g.kernelRead, g.ingress, g.plumbed = false, false, false }
	with := func(change func(*gate)) gate {
		g := serving
		change(&g)
		return g
	}
	cases := []struct {
		name       string
		g          gate
		keep, open bool
	}{
		{"check passing", serving, true, true},
		{"check failing, plan unchanged", with(func(g *gate) { g.passed = false }), false, false},
		{"check failing, on what is installed, plan unchanged", with(func(g *gate) { g.passed, g.onInstalled = false, true }), false, false},
		{"plan just moved, core on what is installed", with(func(g *gate) { g.passed, g.routine, g.onInstalled = false, true, true }), true, false},
		{"plan just moved, core elsewhere", with(func(g *gate) { g.passed, g.routine = false, true }), false, false},
		{"moving, core on the prior", with(func(g *gate) { g.passed, g.moving, g.onInstalled = false, true, true }), true, true},
		{"moving, core elsewhere", with(func(g *gate) { g.passed, g.moving = false, true }), false, false},
		{"moving, closed, core on the prior", with(func(g *gate) { g.passed, g.moving, g.onInstalled, g.up = false, true, true, false }), false, false},
		{"moving, already landed", with(func(g *gate) { g.moving = true }), true, true},
		{"overdue, core on the prior", with(func(g *gate) { g.passed, g.stays, g.onInstalled = false, false, true }), false, false},
		{"overdue, already landed", with(func(g *gate) { g.stays = false }), true, true},
		{"overdue, plan just moved", with(func(g *gate) { g.passed, g.stays, g.onInstalled, g.routine = false, false, true, true }), true, false},
		{"moving, core unread within the bound", with(func(g *gate) { unread(g); g.moving = true }), true, true},
		{"overdue, core unread within the bound", with(func(g *gate) { unread(g); g.stays = false }), false, false},
		{"closed, check passing", with(func(g *gate) { g.up = false }), false, true},
		{"closed, check passing just after drift", with(func(g *gate) { g.up, g.settled = false, false }), false, false},
		{"open, check passing just after drift", with(func(g *gate) { g.settled = false }), true, true},
		{"not served", with(func(g *gate) { g.wanted = false }), false, false},
		{"ingress rule gone", with(func(g *gate) { g.ingress = false }), false, true},
		{"plumbing incomplete", with(func(g *gate) { g.plumbed = false }), true, false},
		{"core unread within the bound", with(unread), true, true},
		{"kernel unread within the bound", with(noKernel), true, true},
		{"closed, core unread within the bound", with(func(g *gate) { unread(g); g.up = false }), false, false},
		{"closed, kernel unread within the bound", with(func(g *gate) { noKernel(g); g.up = false }), false, false},
		{"check failing, kernel unread", with(func(g *gate) { noKernel(g); g.passed = false }), false, false},
		{"past the bound", with(func(g *gate) { unread(g); g.blind = true }), false, false},
	}
	for _, c := range cases {
		if got := c.g.keep(); got != c.keep {
			t.Errorf("%s: keep = %v, want %v", c.name, got, c.keep)
		}
		if got := c.g.open(); got != c.open {
			t.Errorf("%s: open = %v, want %v", c.name, got, c.open)
		}
	}
}

// A move's gate, end to end from what readback shows: the exact prior holds it open until the
// deadline, and neither another revision of that lineage nor another planned server does.
func TestAPriorIsAllowedOnlyUntilItsDeadlineAndOnlyForThatRef(t *testing.T) {
	prior, next := testRef("srvA"), testRef("srvB")
	rotated := prior
	rotated.Digest[0] ^= 0xff
	kept := testRef("srvC")
	until := backoffStart.Add(30 * time.Second)
	slot := domain.Slot{ID: "slot1", Allocation: domain.Allocation{Ordinal: 1, TPROXYPort: 12341}}
	target := SlotTarget{Slot: slot, Selection: domain.Serving(next), Passage: MoveFrom(prior, until)}

	gateOn := func(t SlotTarget, tag string, at time.Time) gate {
		core := coreObservation{Serving: map[domain.SlotID][]string{"slot1": {tag}}}
		_, moving := t.held(at)
		return gate{up: true, wanted: true, coreRead: true, kernelRead: true, ingress: true, plumbed: true, settled: true,
			onInstalled: onInstalled(t, core, at), moving: moving, stays: t.mayBeUp(at, unclaimed)}
	}
	before, at := until.Add(-time.Nanosecond), until
	cases := []struct {
		name       string
		tag        string
		at         time.Time
		keep, open bool
	}{
		{"the prior before its deadline", outboundTag(prior), before, true, true},
		{"the prior at its deadline", outboundTag(prior), at, false, false},
		{"the prior past its deadline", outboundTag(prior), at.Add(time.Second), false, false},
		{"another revision of the prior's lineage", outboundTag(rotated), before, false, false},
		{"another server in the plan", outboundTag(kept), before, false, false},
		{"a tag nothing named", "block", before, false, false},
	}
	for _, c := range cases {
		g := gateOn(target, c.tag, c.at)
		if g.keep() != c.keep || g.open() != c.open {
			t.Errorf("%s: keep %v open %v, want %v %v", c.name, g.keep(), g.open(), c.keep, c.open)
		}
	}
	if onInstalled(target, coreObservation{Serving: map[domain.SlotID][]string{"slot1": {outboundTag(prior), outboundTag(kept)}}}, before) {
		t.Error("a slot routing partly to another server in the plan counted as installed")
	}
	if onInstalled(SlotTarget{Slot: slot, Selection: domain.Serving(next)}, coreObservation{Serving: map[domain.SlotID][]string{"slot1": {outboundTag(prior)}}}, before) {
		t.Error("with no allowance, the server it is leaving counted as installed")
	}
	if !onInstalled(target, coreObservation{Serving: map[domain.SlotID][]string{"slot1": {outboundTag(next)}}}, at) {
		t.Error("the selection itself didn't count as installed")
	}
}

func TestAPassageWithNoDeadlineAllowsNothing(t *testing.T) {
	selection := domain.Serving(testRef("srv"))
	for _, p := range []Passage{MoveFrom(testRef("prior"), time.Time{}), MoveFrom(domain.ServerRef{}, time.Time{}), BridgeUntil(time.Time{})} {
		if from, _, ok := p.Allows(selection); ok {
			t.Errorf("a passage from %q with no deadline allows it", from.Lineage)
		}
	}
}

func TestAnOverdueBridgeIsntWanted(t *testing.T) {
	server, other := testRef("srvA"), testRef("srvB")
	until := backoffStart.Add(30 * time.Second)
	slot := domain.Slot{ID: "slot1", Allocation: domain.Allocation{Ordinal: 1, TPROXYPort: 12341}}
	bridge := SlotTarget{Slot: slot, Selection: domain.Serving(server), Passage: BridgeUntil(until)}
	move := SlotTarget{Slot: slot, Selection: domain.Serving(other), Passage: MoveFrom(server, until)}
	servers := map[domain.ServerRef]domain.Server{server: testServer(t, vlessHop(t, "root", "198.51.100.10", "")), other: testServer(t, vlessHop(t, "root", "198.51.100.11", ""))}
	planned := func(target SlotTarget, at time.Time) int {
		w := &Worker{}
		return len(w.corePlan(Plan{slots: []SlotTarget{target}, servers: servers}, at, nil).Slots)
	}
	if planned(bridge, until.Add(-time.Second)) != 1 {
		t.Error("a bridge inside its deadline left the core plan")
	}
	if planned(bridge, until) != 0 {
		t.Error("a bridge at its deadline stayed in the core plan")
	}
	if planned(move, until) != 1 {
		t.Error("a move past its prior's deadline left the core plan, though its selection is the new server")
	}
}

func TestAReadThatKeepsFailingExpiresAtTheBound(t *testing.T) {
	var c readClock
	start := backoffStart
	stall := errors.New("stalled")
	if c.expired(stall, start) {
		t.Fatal("a Worker's first failed read expired at once")
	}
	if c.expired(stall, start.Add(readBound-time.Second)) {
		t.Error("expired inside the bound")
	}
	if !c.expired(stall, start.Add(readBound)) {
		t.Error("not expired at the bound")
	}
	if c.expired(nil, start.Add(readBound)) {
		t.Error("a good read did not restart the clock")
	}
	if c.expired(stall, start.Add(readBound+20*time.Second)) {
		t.Error("one stall after a good read, up to a tool bound and a poll long, closed the gates")
	}
}

// A probe root that leaves the plan stays installed, outside Topology, for the drain interval, and
// one the plan takes back stops draining at once.
func TestAProbeRootThatLeavesDrainsUntilTheIntervalEndsOrItComesBack(t *testing.T) {
	kept, left := testRef("kept"), testRef("left")
	servers := map[domain.ServerRef]domain.Server{kept: testServer(t, vlessHop(t, "root", "198.51.100.10", "")), left: testServer(t, vlessHop(t, "root", "198.51.100.11", ""))}
	both, one := Plan{servers: servers}, Plan{servers: map[domain.ServerRef]domain.Server{kept: servers[kept]}}
	w := &Worker{}
	draining := func(p Plan, at time.Time) []domain.ServerRef {
		cp := w.corePlan(p, at, nil)
		for _, ref := range cp.Draining {
			if _, held := cp.Servers[ref]; !held || slices.Contains(cp.Probes, ref) {
				t.Errorf("%v drains without its outbound or inside Topology", ref)
			}
		}
		return cp.Draining
	}
	at := backoffStart
	if got := draining(both, at); len(got) != 0 {
		t.Fatalf("drains %v with nothing gone", got)
	}
	if got := draining(one, at.Add(time.Second)); !slices.Equal(got, []domain.ServerRef{left}) {
		t.Fatalf("a root that left drains %v", got)
	}
	if got := draining(one, at.Add(time.Second+drainInterval)); !slices.Equal(got, []domain.ServerRef{left}) {
		t.Fatalf("at the interval's end it drains %v", got)
	}
	if got := draining(one, at.Add(2*time.Second+drainInterval)); len(got) != 0 {
		t.Fatalf("past the interval it still drains %v", got)
	}
	at = at.Add(time.Hour)
	draining(both, at)
	draining(one, at.Add(time.Second))
	if got := draining(both, at.Add(2*time.Second)); len(got) != 0 {
		t.Fatalf("a root the plan took back still drains %v", got)
	}
}
