package daemon

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// probed is every server the observation plan asks anything of.
func probed(p observationPlan) []domain.LineageID {
	var out []domain.LineageID
	add := func(ref domain.ServerRef) {
		if !slices.Contains(out, ref.Lineage) {
			out = append(out, ref.Lineage)
		}
	}
	for ref := range p.Beats {
		add(ref)
	}
	for _, q := range p.Rounds {
		for _, ref := range q.servers() {
			add(ref)
		}
	}
	slices.Sort(out)
	return out
}

// usedAndUnused is a used group of a, b and c, an unused group of d and e, and f in no group.
func usedAndUnused(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, t.TempDir())
	var rows []domain.Lineage
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		rows = append(rows, lineage(t, id, "good", "DE", "198.51.100."+string(rune('1'+i))))
	}
	r.seed(rows...)
	r.c.evidence.epoch = "e1"
	r.putPicked("used", "Used", "a", "b", "c")
	r.putPicked("unused", "Unused", "d", "e")
	r.putSlot("eu", exitIntent(t, "EU", domain.GroupExit("used")))
	return r
}

// Members of unused groups and servers in no group get no probes, whether the used group searches
// or serves.
func TestNothingElseIsProbed(t *testing.T) {
	r := usedAndUnused(t)
	r.settle()
	p, _ := r.c.observation()
	if got := probed(p); !slices.Equal(got, []domain.LineageID{"a", "b", "c"}) {
		t.Fatalf("searching, the plan probes %v", got)
	}
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 30), r.probe("b", domain.Succeeded, 40), r.probe("c", domain.Succeeded, 50))
	r.reportApplied("eu")
	p, _ = r.c.observation()
	if got := probed(p); !slices.Equal(got, []domain.LineageID{r.server("eu")}) {
		t.Fatalf("serving %s, the plan probes %v", r.server("eu"), got)
	}
}

// The scheduler reads its inputs and changes nothing, so planning again asks for the same probes.
func TestPlanningTwicePlansTheSame(t *testing.T) {
	r := usedAndUnused(t)
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 40), r.probe("c", domain.Succeeded, 50))
	first, key := r.c.observation()
	second, again := r.c.observation()
	if key != again || !maps.Equal(first.Beats, second.Beats) || len(first.Rounds) != len(second.Rounds) {
		t.Fatalf("two plans of one moment differ:\n%s\n%s", key, again)
	}
}

func fiveServers() map[string]int { return map[string]int{"a": 40, "b": 50, "c": 60, "d": 70, "e": 80} }

// A new server gets the introduction's eleven checks over two days, and a member ranked below the
// next two gets nothing after it.
func TestAnIntroductionThenSilence(t *testing.T) {
	l := freshTimeline(t, fiveServers())
	l.stride = time.Second
	l.run(49 * time.Hour)
	for id := range fiveServers() {
		if got := l.count["check"][domain.LineageID(id)]; got != 11 {
			t.Errorf("%s had %d checks in its first two days, want 11", id, got)
		}
	}
	below := slices.DeleteFunc(l.groupOrder(), func(id domain.LineageID) bool { return id == l.r.server("eu") })[2:]
	probes := func() int {
		n := 0
		for _, servers := range l.count {
			for _, id := range below {
				n += servers[id]
			}
		}
		return n
	}
	before := probes()
	l.run(24 * time.Hour)
	if got := probes() - before; got != 0 {
		t.Errorf("%v, ranked below the next 2, had %d probes on day three", below, got)
	}
}

// A restart in the middle of the quick probes owes nothing again: each server still has three in its
// first quarter hour.
func TestARestartMidIntroductionResumesWhereTheRecordSays(t *testing.T) {
	l := freshTimeline(t, fiveServers())
	l.stride = 200 * time.Millisecond
	l.run(5 * time.Minute)
	l.restart()
	l.run(10 * time.Minute)
	for id := range fiveServers() {
		if got := l.count["check"][domain.LineageID(id)]; got != 3 {
			t.Errorf("%s had %d checks in its first quarter hour across a restart, want 3", id, got)
		}
	}
}

// A new credential or port on a lineage the catalogue matched keeps its record and costs one check.
func TestACredentialChangeKeepsTheRecordAndCostsOneCheck(t *testing.T) {
	l := freshTimeline(t, fiveServers())
	l.stride = time.Second
	l.run(20 * time.Minute)
	samples := len(l.r.c.evidence.book.records["d"].Samples)
	l.rotate("d")
	l.run(time.Hour)
	if got := l.count["check"]["d"]; got != 4 {
		t.Fatalf("d had %d checks, want its 3 quick probes and one for the new ref", got)
	}
	r := l.r.c.evidence.book.records["d"]
	if len(r.Samples) != samples+1 || r.Revision != l.r.ref("d").Digest {
		t.Fatalf("after the change the record holds %d samples of %x, want %d of the new ref", len(r.Samples), r.Revision[:6], samples+1)
	}
}

// groupOrder is the order the page shows for the timeline's group.
func (l *timeline) groupOrder() []domain.LineageID {
	l.r.c.publish(l.now())
	for _, g := range l.r.app.Snapshot().Groups {
		if g.ID == sourceGroup("good") {
			return g.Order
		}
	}
	l.t.Fatal("no group")
	return nil
}

// addTarget gives the controller a second probe target, as bootstrap.json lists them.
func (l *timeline) addTarget(id domain.TargetID) {
	target := probeTarget{ID: id, URL: "http://" + string(id) + ".example/"}
	l.r.c.targets = append(l.r.c.targets, target)
	l.r.c.evidence.book.sites[id] = siteOf(target)
	l.r.settle()
}

// The ranking target stops reusing connections, so its checks give no Ping. The judge moves the target
// on those checks, every member gets its three quick probes against the new one, and the old site's
// numbers order the group until they have landed.
func TestTheSiteJudgeMovesTheTargetAndTheRecordRefills(t *testing.T) {
	l := freshTimeline(t, fiveServers())
	l.stride = time.Second
	l.addTarget("t2")
	l.run(20 * time.Minute)
	introduced := l.groupOrder()
	if !slices.Equal(introduced, []domain.LineageID{"a", "b", "c", "d", "e"}) {
		t.Fatalf("introduced in the order %v", introduced)
	}
	l.noReuse["t1"] = true
	for id, ms := range map[string]int{"a": 90, "b": 80, "c": 70, "d": 60, "e": 50} {
		l.set(domain.LineageID(id), healthy(ms))
	}
	l.runUntil(13*time.Hour, "a new ranking target", func() bool { return l.r.c.evidence.ranking == "t2" })
	moved := l.now()
	t.Logf("the judge moved the ranking target %s after the first check without a Ping", (moved.Sub(t0) - 6*time.Hour).Round(time.Minute))
	checks := maps.Clone(l.count["check"])
	l.run(time.Minute)
	if got := l.groupOrder(); !slices.Equal(got, introduced) {
		t.Fatalf("before the quick probes landed the order is %v, want the old site's %v", got, introduced)
	}
	l.run(11 * time.Minute)
	if got := l.groupOrder(); !slices.Equal(got, []domain.LineageID{"e", "d", "c", "b", "a"}) {
		t.Fatalf("after the quick probes the order is %v, want the new site's", got)
	}
	for id := range fiveServers() {
		lineage := domain.LineageID(id)
		pings, _, _ := samplesTo(l.r.c.evidence.book.records[lineage], siteOf(probeTarget{URL: "http://t2.example/"}))
		if extra := l.count["check"][lineage] - checks[lineage]; len(pings) < 3 || extra > 3 {
			t.Errorf("%s has %d samples to the new site after %d checks since the move", id, len(pings), extra)
		}
	}
}

// moveRanking has the judge give up on t1, as checks that never reuse their connection make it.
func (l *timeline) moveRanking() {
	l.t.Helper()
	l.adoptPlan()
	l.loop.plan.Targets = l.r.c.targets
	for range badTargetFails {
		l.loop.judgeRanking(domain.Signal{Purpose: domain.Check, Target: "t1", Verdict: domain.Succeeded, NoReuse: true})
	}
	l.frame(nil)
	if l.r.c.evidence.ranking != "t2" {
		l.t.Fatalf("ranking on %s, want t2", l.r.c.evidence.ranking)
	}
}

// b's quick probes against the new site pass without a Ping, so the refill brings it no sample there.
// b then ranks on the Ping it had, at the unknown price, so a's failure goes to c, 40 ms on the new
// site, and not to b at 200.
func TestARefillWithoutAPingLeavesTheServerWhereItsNumbersPutIt(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 20, "b": 200, "c": 40})
	l.stride = time.Second
	l.addTarget("t2")
	l.set("b", func(p domain.ProbePurpose) reply { r := healthy(200)(p); r.ping = nil; return r })
	l.moveRanking()
	l.run(12 * time.Minute)
	if got := l.groupOrder(); !slices.Equal(got, []domain.LineageID{"a", "c", "b"}) {
		t.Fatalf("after the refill the order is %v, want b last", got)
	}
	l.heartbeatJustBeat()
	l.set("a", silent(0))
	l.runUntil(failover, "a move", l.moved)
	if to, _ := l.moves[0].to.Server(); to.Lineage != "c" {
		t.Fatalf("a's failure moved the group to %s, want c", to.Lineage)
	}
}

// The judge moved the ranking target to t2 and the refill reordered the group. A restart keeps t2 and
// the order it gave.
func TestARestartKeepsTheRankingTarget(t *testing.T) {
	l := newTimeline(t, fiveServers())
	l.stride = time.Second
	l.addTarget("t2")
	for id, ms := range map[string]int{"a": 90, "b": 80, "c": 70, "d": 60, "e": 50} {
		l.set(domain.LineageID(id), healthy(ms))
	}
	l.moveRanking()
	l.run(12 * time.Minute)
	before := l.groupOrder()
	if !slices.Equal(before, []domain.LineageID{"e", "d", "c", "b", "a"}) {
		t.Fatalf("after the refill the order is %v", before)
	}
	l.restart()
	l.run(time.Minute)
	if l.r.c.evidence.ranking != "t2" || l.loop.rankingTarget().ID != "t2" {
		t.Fatalf("after the restart the controller ranks on %s and the loop on %s, want t2", l.r.c.evidence.ranking, l.loop.rankingTarget().ID)
	}
	if got := l.groupOrder(); !slices.Equal(got, before) {
		t.Fatalf("after the restart the order is %v, want %v", got, before)
	}
}

// A restart a minute into a refill still owes the two quick probes each member hasn't had.
func TestARestartMidRefillOwesTheRest(t *testing.T) {
	l := newTimeline(t, fiveServers())
	l.stride = time.Second
	l.addTarget("t2")
	l.moveRanking()
	checks := maps.Clone(l.count["check"])
	l.run(time.Minute)
	l.restart()
	l.run(12 * time.Minute)
	t2 := siteOf(probeTarget{URL: "http://t2.example/"})
	for id := range fiveServers() {
		lineage := domain.LineageID(id)
		pings, _, _ := samplesTo(l.r.c.evidence.book.records[lineage], t2)
		if got := l.count["check"][lineage] - checks[lineage]; got > 3 || len(pings) < 3 {
			t.Errorf("%s had %d checks and has %d samples to t2 across the restart, want 3 at most and as many samples", id, got, len(pings))
		}
	}
}

// The serving server's refill check and its heartbeat start together, and the heartbeat lands first.
// The check's sample and its step still count, so the refill costs its three probes.
func TestAHeartbeatLandingFirstCostsTheRefillNothing(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = time.Second
	l.addTarget("t2")
	l.moveRanking()
	l.adoptPlan()
	p, _ := l.r.c.observation()
	current := l.r.ref("a")
	l.loop.nextBeat[current] = p.Checks[current]
	before := l.count["check"]["a"]
	l.run(12 * time.Minute)
	pings, _, _ := samplesTo(l.r.c.evidence.book.records["a"], siteOf(probeTarget{URL: "http://t2.example/"}))
	if got := l.count["check"]["a"] - before; got > 3 || len(pings) < 3 {
		t.Fatalf("a had %d refill checks and has %d samples to t2, want 3 at most and as many samples", got, len(pings))
	}
}

// A member with no samples to the site ranks on the Ping it last had elsewhere times its own factor,
// which already carries the price of what it hasn't shown, and one with no samples at all ranks after
// every member that has some.
func TestAServerTheSiteHasntMeasuredRanksOnItsPingElsewhere(t *testing.T) {
	old, now := site("old"), site("new")
	at := func(s site, ms int) record {
		ping := time.Duration(ms) * time.Millisecond
		return record{FirstSeen: t0.Add(-time.Hour), Last: t0.Add(-time.Minute), Samples: []sample{{At: t0.Add(-time.Minute), Site: s, Ping: ping, NewConn: ping}}}
	}
	records := map[domain.LineageID]record{
		"near": at(now, 230), "far": at(now, 260), "elsewhere": at(old, 200), "never": {FirstSeen: t0.Add(-time.Hour), Last: t0.Add(-time.Minute)},
	}
	var ids []domain.LineageID
	for _, l := range order([]domain.Lineage{lin("never"), lin("elsewhere"), lin("far"), lin("near")}, records, nil, nil, now, t0) {
		ids = append(ids, l.ID)
	}
	if want := []domain.LineageID{"elsewhere", "near", "far", "never"}; !slices.Equal(ids, want) {
		t.Fatalf("ranked %v, want %v: 200 ms elsewhere counts as 200", ids, want)
	}
}

// Two members with the same history, one at 200 ms on the site before and one at 230 on this one.
// The score never prefers a slower server over a faster one with the same history.
func TestTheSameHistoryKeepsTheFasterAheadAcrossASiteChange(t *testing.T) {
	old, now := site("old"), site("new")
	base := record{FirstSeen: t0.Add(-72 * time.Hour), Last: t0.Add(-time.Minute), Checked: t0, Seen: decayed{At: t0, Value: 1}}
	fast, slow := base, base
	fast.Samples = []sample{{At: t0, Site: old, Ping: 200 * time.Millisecond}}
	slow.Samples = []sample{{At: t0, Site: now, Ping: 230 * time.Millisecond}}
	records := map[domain.LineageID]record{"fast": fast, "slow": slow}
	if got := ids(order([]domain.Lineage{lin("fast"), lin("slow")}, records, nil, nil, now, t0)); !slices.Equal(got, []domain.LineageID{"fast", "slow"}) {
		t.Fatalf("ranked %v, want the 200 ms member first", got)
	}
}

// b's quick probes against the new site pass without a Ping, so b keeps its 200 ms from before, and
// c measures 230 there. a fails, both pass, and the group moves to b, the faster.
func TestARefillWithoutAPingKeepsTheFasterServerFirstForAFailover(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 20, "b": 200, "c": 230})
	l.stride = time.Second
	l.addTarget("t2")
	l.set("b", func(p domain.ProbePurpose) reply { r := healthy(200)(p); r.ping = nil; return r })
	l.moveRanking()
	l.run(12 * time.Minute)
	l.heartbeatJustBeat()
	l.set("a", silent(0))
	l.runUntil(failover, "a move", l.moved)
	if to, _ := l.moves[0].to.Server(); to.Lineage != "b" {
		t.Fatalf("a's failure moved the group to %s, want b", to.Lineage)
	}
}

// The ranking site changed three days ago and every member refilled on the new one. A server the
// source adds now owes its introduction, not a refill, so the pool still ranks on the new site.
func TestAServerAddedAfterARefillLeavesThePoolOnTheNewSite(t *testing.T) {
	old, now := site("old"), site("new")
	ev := newEvidence("t2", map[domain.TargetID]site{"t1": old, "t2": now})
	ev.previous, ev.rankedSince = old, t0.Add(-72*time.Hour)
	member := func(onOld, onNew time.Duration) record {
		r := record{FirstSeen: t0.Add(-30 * 24 * time.Hour), Last: t0.Add(-time.Minute), Checked: t0.Add(-time.Minute), Seen: decayed{At: t0, Value: 1}}
		for i := range len(quickProbes) {
			at := t0.Add(-time.Duration(i+1) * time.Hour)
			r.Samples = append(r.Samples, sample{At: at, Site: old, Ping: onOld}, sample{At: at, Site: now, Ping: onNew})
		}
		return r
	}
	ev.book.records["a"] = member(100*time.Millisecond, 300*time.Millisecond)
	ev.book.records["b"] = member(300*time.Millisecond, 100*time.Millisecond)
	e := evaluation{now: t0, pool: poolOf(lin("a"), lin("b")), ev: ev, t: defaultTimings()}
	if got := ids(e.ranked(e.pool.members)); !slices.Equal(got, []domain.LineageID{"b", "a"}) {
		t.Fatalf("after the refill the pool ranks %v, want b first on the new site", got)
	}
	e.pool = poolOf(lin("a"), lin("b"), lin("n"))
	if got := ids(e.ranked(e.pool.members)); !slices.Equal(got, []domain.LineageID{"b", "a", "n"}) {
		t.Fatalf("with a new server the pool ranks %v, want b, a, n: the old site ordered it again", got)
	}
	if due, ok := planProbes([]evaluation{e}).Checks[lin("n").Ref]; !ok || !due.IsZero() {
		t.Fatalf("the new server's first check is due %s (%v), want at once", due, ok)
	}
}

// refillEvidence is a ranking site moved 72 hours ago. A and B have their quick probes on both sites, A
// first on the old one and B on the new, and N, older still, has samples only on the old site.
func refillEvidence() *evidence {
	old, now := site("old"), site("new")
	ev := newEvidence("t2", map[domain.TargetID]site{"t1": old, "t2": now})
	ev.previous, ev.rankedSince = old, t0.Add(-72*time.Hour)
	member := func(onOld, onNew time.Duration) record {
		r := record{FirstSeen: t0.Add(-30 * 24 * time.Hour), Last: t0.Add(-time.Minute), Checked: t0.Add(-time.Minute), Seen: decayed{At: t0, Value: 1}}
		for i := range len(quickProbes) {
			at := t0.Add(-time.Duration(i+1) * time.Hour)
			r.Samples = append(r.Samples, sample{At: at, Site: old, Ping: onOld}, sample{At: at, Site: now, Ping: onNew})
		}
		return r
	}
	ev.book.records["a"] = member(100*time.Millisecond, 300*time.Millisecond)
	ev.book.records["b"] = member(300*time.Millisecond, 100*time.Millisecond)
	n := record{FirstSeen: t0.Add(-30 * 24 * time.Hour), Last: t0.Add(-80 * time.Hour), Checked: t0.Add(-80 * time.Hour), Seen: decayed{At: t0, Value: 1}}
	for i := range len(quickProbes) {
		n.Samples = append(n.Samples, sample{At: t0.Add(-time.Duration(80+i) * time.Hour), Site: old, Ping: 200 * time.Millisecond})
	}
	ev.book.records["n"] = n
	return ev
}

// The pool finished its refill on the new site, so it ranks there. A server the catalogue held since
// before the change, which the pool admits only now, owes its own quick probes there but no longer holds
// the pool on the old site, across a restart too. A pool that never finished its refill still orders on
// the old site.
func TestAServerAdmittedAfterTheRefillLeavesThePoolOnTheNewSite(t *testing.T) {
	ev := refillEvidence()
	tm := defaultTimings()
	refilled := decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), policyMemory{}, ev, tm).Memory
	wire, err := encodeMemory(refilled)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := decodeMemory(wire)
	if err != nil {
		t.Fatal(err)
	}
	admitted := poolOf(lin("a"), lin("b"), lin("n"))
	for name, memory := range map[string]policyMemory{"as it runs": refilled, "after a restart": restarted} {
		e := evaluation{now: t0.Add(time.Minute), who: groupSlot(t), pool: admitted, memory: memory, ev: ev, t: tm}
		if got := ids(e.ranked(e.pool.members)); got[0] != "b" {
			t.Errorf("%s, with n admitted the pool ranks %v, want b first on the new site", name, got)
		}
		if due, ok := planProbes([]evaluation{e}).Checks[lin("n").Ref]; !ok || !due.Equal(ev.rankedSince) {
			t.Errorf("%s, n's first quick probe is due %s (%v), want at once", name, due, ok)
		}
	}
	other := evaluation{now: t0.Add(time.Minute), who: domain.GroupOwner("h"), pool: admitted, ev: ev, t: tm}
	if got := ids(other.ranked(other.pool.members)); got[0] != "a" {
		t.Errorf("a pool that never finished its refill ranks %v, want a first on the old site", got)
	}
}

// A finished refill belongs to the pool that earned it. Direct's empty pool finishes at once, and a slot
// moved from Direct to a group nothing follows yet still owes N's quick probes, so it picks A on the old site.
func TestAnExitChangeStartsTheNewPoolsRefillOpen(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "s", "DE", "198.51.100.10"), lineage(t, "b", "s", "DE", "198.51.100.11"), lineage(t, "n", "s", "DE", "198.51.100.12"))
	r.putSlot("eu", exitIntent(t, "EU", domain.DirectExit()))
	r.clock = t0
	r.c.evidence = refillEvidence()
	r.c.evidence.wan.read(domain.WANUp, r.clock, r.clock)
	if err := r.c.evaluate(ctx, r.clock); err != nil {
		t.Fatal(err)
	}
	r.setExit("eu", domain.GroupExit(sourceGroup("s")))
	if err := r.c.evaluate(ctx, r.clock); err != nil {
		t.Fatal(err)
	}
	if got, serving := r.c.state.slots["eu"].Selection.Server(); !serving || got.Lineage != "a" {
		t.Fatalf("the slot moved to the group serves %+v (serving %v), want a on the old site", got, serving)
	}
}
