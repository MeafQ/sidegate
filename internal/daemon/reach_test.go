package daemon

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// One slot uses a source of 65 servers and another a group of one. Every member of both has a probe
// route, so when every large member fails the group still serves its one server, whichever slot sorts
// and was stored first.
func TestASingletonGroupServesBesideALargeSourceWhoseEveryMemberFailed(t *testing.T) {
	for _, smallFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("small first %t", smallFirst), func(t *testing.T) {
			r := newRig(t, t.TempDir())
			var rows []domain.Lineage
			for i := range 65 {
				rows = append(rows, lineage(t, fmt.Sprintf("a%03d", i), "large", "DE", "large.example"))
			}
			rows = append(rows, lineage(t, "z", "small", "SE", "small.example"))
			r.seed(rows...)
			r.c.evidence.epoch = "e1"
			large, small := domain.SlotID("a"), domain.SlotID("b")
			putLarge := func() { r.putSlot(large, exitIntent(t, "Large", domain.SourceExit("large"))) }
			putSmall := func() { r.putSlot(small, exitIntent(t, "Small", domain.GroupExit(sourceGroup("small")))) }
			if smallFirst {
				large, small = small, large
				putSmall()
				putLarge()
			} else {
				putLarge()
				putSmall()
			}
			var signals []domain.Signal
			for _, row := range rows {
				signals = append(signals, r.probe(string(row.ID), domain.Succeeded, 20))
			}
			r.frame(domain.WANUp, signals...)
			r.clock = r.clock.Add(time.Second)
			signals = nil
			for _, row := range rows[:65] {
				signals = append(signals, r.probe(string(row.ID), domain.Failed, 0))
			}
			r.frame(domain.WANUp, signals...)
			if got := r.server(small); got != "z" {
				t.Fatalf("the group of one serves %q with every large member failed, want z: %s", got, describe(r.c.state.slots[small].Selection))
			}
		})
	}
}

// A group of three working servers and 64 unverified ones. Its comparison sends three probes, the
// current server and the next two, and each is charged to the improvement bucket.
func TestAComparisonSendsAtMostThreeProbesWhateverTheDeadTail(t *testing.T) {
	b := newRoundBench(t)
	w := newScene(domain.WANUp).healthy("a", 20).healthy("b", 30).healthy("c", 40)
	rows := []domain.Lineage{lin("a"), lin("b"), lin("c")}
	for i := range 64 {
		row := lin(fmt.Sprintf("x%03d", i))
		rows = append(rows, row)
		w.book.records[row.ID] = record{Last: t0, Failures: 7, Retries: 6}
	}
	e := evaluation{now: t0, who: groupSlot(t), pool: poolOf(rows...), memory: policyMemory{Selection: serving("a"), CompareAt: t0}, ev: w.evidence, t: defaultTimings()}
	e.memory = e.compare(ref("a")).Memory
	s := schedule{}
	campaignRound(&e, &s)
	p := b.plan(s.Rounds...)
	for _, row := range rows[3:] {
		p.Topology.Probes = append(p.Topology.Probes, gateway.ProbeRoute{Server: row.Ref, Epoch: "e1"})
	}
	b.s.adopt(b.ctx, p)
	before := b.s.lanes[improvePath].bucket.tokens
	b.s.schedule(b.ctx, &b.wg, b.results, b.rounds)
	b.dialed("a", "b", "c")
	b.noDial()
	if got := b.s.lanes[improvePath].bucket.tokens; got != before-3 {
		t.Fatalf("the bucket went from %g to %g tokens for three probes", before, got)
	}
}

// probesOf is every probe id has had, of any kind.
func (l *timeline) probesOf(id domain.LineageID) int {
	n := 0
	for _, byServer := range l.count {
		n += byServer[id]
	}
	return n
}

// b dies. Its ladder runs on past its first six steps, doubling to 6 h, so at steady state it costs four
// checks a day and nothing else: it ranks last, so it is no round's rival, and rounds hold no extras.
func TestADeadMemberCostsFourProbesADay(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = 2 * time.Second
	l.failedOnce("b")
	l.run(10 * time.Hour)
	before := l.probesOf("b")
	l.run(24 * time.Hour)
	if got := l.probesOf("b") - before; got != 4 || l.count["check"]["b"] < 10 {
		t.Fatalf("b had %d probes in a day at steady state and %d checks in all, want 4 a day", got, l.count["check"]["b"])
	}
}

// A Test of a server with no route holds that server in every frame until it answers, and runs once the
// route comes. One whose route never comes answers unknown and reaches no record, and lets go.
func TestATestHoldsARouteForItsLengthAndAMissingOneIsNeverAFailure(t *testing.T) {
	b := newRoundBench(t)
	done := make(chan domain.Signal, 4)
	frames := make(chan signalFrame, 8)
	held := func() []domain.ServerRef {
		b.s.emit(frames, nil)
		return (<-frames).Tests
	}
	z := manualProbe{Server: ref("z"), Reply: make(chan domain.Signal, 1)}
	b.s.manual(b.ctx, &b.wg, z, done)
	if got := held(); !slices.Equal(got, []domain.ServerRef{ref("z")}) {
		t.Fatalf("a Test with no route holds %v", got)
	}
	b.noDial()
	p := b.plan()
	p.Topology.Probes = append(p.Topology.Probes, gateway.ProbeRoute{Server: ref("z"), Epoch: "e1"})
	b.s.adopt(b.ctx, p)
	b.s.startTests(b.ctx, &b.wg, done)
	b.dialed("z")

	y := manualProbe{Server: ref("y"), Reply: make(chan domain.Signal, 1)}
	b.s.manual(b.ctx, &b.wg, y, done)
	b.now = b.now.Add(testRouteWait)
	if !b.s.startTests(b.ctx, &b.wg, done) {
		t.Fatal("a Test whose route never came still holds it")
	}
	if got := <-y.Reply; got.Verdict != domain.Unknown {
		t.Fatalf("a Test with no route answered %v", got.Verdict)
	}
	if got := held(); !slices.Equal(got, []domain.ServerRef{ref("z")}) {
		t.Fatalf("after y let go the Tests hold %v, want z alone", got)
	}
	select {
	case sig := <-done:
		t.Fatalf("a Test with no route reached the record: %+v", sig)
	default:
	}
}

// A server no Exit in use holds gets a route while a Test holds it, and loses it after.
func TestAServerOutsideEveryUsedExitIsRoutedWhileATestHoldsIt(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "s", "DE", "a.example"), lineage(t, "lone", "other", "SE", "lone.example"))
	r.putSlot("eu", exitIntent(t, "EU", domain.GroupExit(sourceGroup("s"))))
	routed := func() bool {
		plan, _, err := r.c.desired()
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(plan.Servers(), r.ref("lone"))
	}
	if routed() {
		t.Fatal("a server no slot uses has a route")
	}
	r.c.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: r.clock}, Tests: []domain.ServerRef{r.ref("lone")}})
	if !routed() {
		t.Fatal("a Test of it gave it no route")
	}
	if obs, _ := r.c.observation(); obs.Sources[r.ref("lone")] != "other" {
		t.Error("the tested server's probe has no provider")
	}
	r.c.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: r.clock}})
	if routed() {
		t.Fatal("the route outlived its Test")
	}
}

// Once a group has settled, a day of heartbeats, comparisons and checks adds and removes no route.
func TestAStablePoolMakesNoRouteChangesAfterItSettles(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = 2 * time.Second
	l.run(time.Hour)
	servers := func() []domain.ServerRef {
		plan, _, err := l.r.c.desired()
		if err != nil {
			t.Fatal(err)
		}
		return slices.SortedFunc(slices.Values(plan.Servers()), compareRefs)
	}
	settled, changes := servers(), 0
	l.onStep = func() {
		if !slices.Equal(servers(), settled) {
			changes++
		}
	}
	l.run(24 * time.Hour)
	if changes != 0 || len(settled) != 4 || l.comparisons() < 40 {
		t.Fatalf("%d steps of a day changed the routes from %v, after %d comparisons", changes, settled, l.comparisons())
	}
}
