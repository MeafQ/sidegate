package daemon

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// onTarget gives r the introduction's three samples at pingMs to the rig's target, the last at at.
func onTarget(r record, at time.Time, pingMs int) record {
	ping := time.Duration(pingMs) * time.Millisecond
	site := siteOf(probeTarget{URL: "http://target.example/"})
	for i := range len(quickProbes) {
		r.Samples = append(r.Samples, sample{At: at.Add(time.Duration(i-len(quickProbes)+1) * time.Minute), Site: site, Ping: ping, NewConn: 3 * ping})
	}
	return r
}

// failOver silences the serving server right after its heartbeat and says where the group went.
func (l *timeline) failOver() domain.LineageID {
	l.t.Helper()
	current := l.heartbeatJustBeat()
	l.set(current.Lineage, silent(0))
	l.runUntil(failover, "a failover", l.moved)
	to, _ := l.moves[0].to.Server()
	return to.Lineage
}

// health is what the page shows of each server's record.
func (l *timeline) health() map[domain.LineageID]HealthDTO {
	l.t.Helper()
	l.r.c.publish(l.now())
	page, err := l.r.app.Servers(context.Background(), PageQuery{})
	if err != nil {
		l.t.Fatal(err)
	}
	health := map[domain.LineageID]HealthDTO{}
	for _, s := range page.Items {
		health[s.ID] = s.Health
	}
	return health
}

func (l *timeline) seenDays(id domain.LineageID) float64 {
	return l.r.c.evidence.book.records[id].seenDays(l.now())
}

// a, the best on record, fails and the group moves to b. a comes back with a fresh incident on a short
// record, too dear to win a comparison. Each comparison it passes credits it with the half hour since
// its last, so it pays the incident down without serving, and three wins in a row take it back.
func TestAFormerlyBestServerEarnsItsWayBackThroughComparisons(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 60, "b": 100})
	if to := l.failOver(); to != "b" {
		t.Fatalf("a's failure moved the group to %s", to)
	}
	l.set("a", healthy(60))
	l.runUntil(5*time.Minute, "a's recovery", func() bool { return l.standing("a") == passing })
	if got := incidents7d(l.r.c.evidence.book.records["a"], l.now()); got != 1 {
		t.Fatalf("a came back with %d incidents", got)
	}
	recovered, seen, beats := l.now(), l.seenDays("a"), l.count["heartbeat"]["a"]
	l.moves = nil
	l.stride = time.Second
	elsewhere := 0
	l.onStep = func() {
		if l.r.server("eu") != "a" {
			elsewhere = l.count["heartbeat"]["a"] - beats
		}
	}
	l.runUntil(compareEvery+time.Minute, "a comparison", func() bool { return l.count["comparison"]["a"] > 0 && l.r.memory("eu").Campaign == nil })
	if m := l.r.memory("eu"); m.Streaks["a"] != 0 {
		t.Fatalf("a won its first comparison back, streaks %v", m.Streaks)
	}
	took := l.runUntil(7*24*time.Hour, "a's return", l.moved)
	if to, _ := l.moves[0].to.Server(); to.Lineage != "a" || l.moves[0].reason != "improvementMove" {
		t.Fatalf("moved to %s for %s", to.Lineage, l.moves[0].reason)
	}
	if took < 24*time.Hour {
		t.Fatalf("a won its place back %s after recovering, before a day of comparisons could pay its incident down", took)
	}
	if elsewhere != 0 {
		t.Fatalf("a had %d heartbeats while the group served b", elsewhere)
	}
	earned := l.seenDays("a") - seen
	if passes := l.count["comparison"]["a"]; earned < 0.9*float64(passes-1)*0.5/24 {
		t.Fatalf("%d comparisons credited a with %.2f days seen working", passes, earned)
	}
	t.Logf("calculated: a won its place back %s after recovering, on %d comparisons, factor %.3f", took.Round(time.Minute), l.count["comparison"]["a"], factor(l.r.c.evidence.book.records["a"], recovered.Add(took)))
}

// x dies, and b and c both pass both failure rounds. c answers faster, but its incident ranks it below
// b, and the better ranked rival takes over.
func TestAFailoverTieGoesToTheBetterRankedRival(t *testing.T) {
	l := newTimeline(t, map[string]int{"x": 10, "b": 50, "c": 30})
	now := l.now()
	flaky, _ := introduced(now, 30).charged(now.Add(-30 * time.Hour))
	l.r.c.evidence.book.put("c", flaky, true)
	if order := l.groupOrder(); !slices.Equal(order, []domain.LineageID{"x", "b", "c"}) {
		t.Fatalf("the group's order is %v, want c behind b", order)
	}
	if to := l.failOver(); to != "b" {
		t.Fatalf("the tie went to %s, want b", to)
	}
	if got := l.count["failure round"]["c"]; got < 2 {
		t.Fatalf("c was in %d failure rounds, want both", got)
	}
}

// x dies with two rivals left: long, seen working for three weeks with one incident three days ago,
// and new, met ten minutes ago. By the plan's formula long counts its 100 ms Ping at about 110%, and
// new pays the unknown price of 125%, so new takes over only below about 88 ms.
func TestAFailoverWeighsANeverUsedServerAgainstALongRecordWithOneIncident(t *testing.T) {
	decayed := func(days float64) float64 { return math.Exp2(-days / 7) }
	seen := 7 / math.Ln2 * (1 - decayed(21))
	longShare := 1 + (decayed(3)+0.25)/(seen+1)
	breakEven := 100 * longShare / 1.25
	for _, c := range []struct {
		newMs int
		want  domain.LineageID
	}{{100, "long"}, {int(math.Ceil(breakEven)) + 2, "long"}, {int(math.Floor(breakEven)) - 2, "new"}, {75, "new"}} {
		t.Run(fmt.Sprintf("new at %d ms", c.newMs), func(t *testing.T) {
			l := newTimeline(t, map[string]int{"x": 10, "long": 100, "new": c.newMs})
			now := l.now()
			from := now.Add(-21 * 24 * time.Hour)
			long, _ := observed(record{FirstSeen: from}, from, now, 10*time.Minute).charged(now.Add(-3 * 24 * time.Hour))
			l.r.c.evidence.book.put("long", onTarget(long, now, 100), true)
			l.r.c.evidence.book.put("new", onTarget(record{FirstSeen: now.Add(-10 * time.Minute), Last: now, Checked: now}, now, c.newMs), true)
			if to := l.failOver(); to != c.want {
				t.Fatalf("x's failure went to %s, want %s", to, c.want)
			}
		})
	}
}

// far and near have the same history: two hours seen working and one incident. Each pays the same
// share of its own Ping, so near stays ahead of far by their Pings alone and takes over when x dies.
func TestAFarAndANearServerWithTheSameHistoryKeepTheirPlacesInAFailover(t *testing.T) {
	l := newTimeline(t, map[string]int{"x": 10, "far": 250, "near": 40})
	now := l.now()
	history := observed(record{FirstSeen: now.Add(-72 * time.Hour)}, now.Add(-2*time.Hour), now, time.Minute)
	history, _ = history.charged(now.Add(-time.Hour))
	l.r.c.evidence.book.put("far", onTarget(history, now, 250), true)
	l.r.c.evidence.book.put("near", onTarget(history, now, 40), true)
	h := l.health()
	if far, near := h["far"], h["near"]; far.Factor != near.Factor || far.Score == nil || near.Score == nil || math.Abs(*far.Score / *near.Score - 250.0/40) > 0.01 {
		t.Fatalf("the same history priced far at %v x %.3f and near at %v x %.3f", far.Score, far.Factor, near.Score, near.Factor)
	}
	if order := l.groupOrder(); !slices.Equal(order, []domain.LineageID{"x", "near", "far"}) {
		t.Fatalf("the group's order is %v", order)
	}
	if to := l.failOver(); to != "near" {
		t.Fatalf("x's failure went to %s", to)
	}
}

// clean, seen working through a week, and flaky, charged an incident in its two hours, sit in no group
// a slot uses. Nothing probes them, and each record drifts back toward the unknown price from its side.
// Each shares a group with a rival met long ago and never seen working, which keeps the unknown price
// of 125%. The rivals' Pings put each one halfway between where its record starts and where 4 weeks of
// decay take it, so each group's published order flips.
func TestDormantRecordsDecayTowardTheUnknownPriceFromBothSides(t *testing.T) {
	flakyAfter := 1 + (math.Exp2(-4)+0.25)/(2.0/24*math.Exp2(-4)+1)
	rival := func(fromShare, toShare float64) int { return int(math.Round(60 * (fromShare + toShare) / 2 / 1.25)) }
	l := newTimeline(t, map[string]int{"x": 40, "y": 50, "clean": 60, "flaky": 60, "fair-clean": rival(1.04, 1.19), "fair-flaky": rival(2.15, flakyAfter)})
	l.r.putPicked("pair", "Pair", "x", "y")
	l.r.setExit("eu", domain.GroupExit("pair"))
	l.r.putPicked("clean-order", "Clean", "clean", "fair-clean")
	l.r.putPicked("flaky-order", "Flaky", "flaky", "fair-flaky")
	now := l.now()
	week := 7 * 24 * time.Hour
	clean := observed(record{FirstSeen: now.Add(-week)}, now.Add(-week), now, 10*time.Minute)
	flaky, _ := observed(record{FirstSeen: now.Add(-72 * time.Hour)}, now.Add(-2*time.Hour), now, time.Minute).charged(now)
	l.r.c.evidence.book.put("clean", onTarget(clean, now, 60), true)
	l.r.c.evidence.book.put("flaky", onTarget(flaky, now, 60), true)
	order := func(id domain.GroupID) []domain.LineageID {
		l.r.c.publish(l.now())
		for _, g := range l.r.app.Snapshot().Groups {
			if g.ID == id {
				return g.Order
			}
		}
		t.Fatalf("no group %s", id)
		return nil
	}
	share := func(id domain.LineageID) int {
		return int(math.Round(factor(l.r.c.evidence.book.records[id], l.now()) * 100))
	}
	probes := func() int {
		n := 0
		for _, kind := range l.count {
			n += kind["clean"] + kind["flaky"] + kind["fair-clean"] + kind["fair-flaky"]
		}
		return n
	}
	before := probes()
	c, f := share("clean"), share("flaky")
	if c != 104 || f != 215 {
		t.Fatalf("clean starts at %d%% and flaky at %d%%, want the share table's 104%% and 215%%", c, f)
	}
	if co, fo := order("clean-order"), order("flaky-order"); !slices.Equal(co, []domain.LineageID{"clean", "fair-clean"}) || !slices.Equal(fo, []domain.LineageID{"fair-flaky", "flaky"}) {
		t.Fatalf("before any decay the groups are ordered %v and %v", co, fo)
	}
	l.stride = 30 * time.Second
	for w := 1; w <= 4; w++ {
		l.run(week)
		nc, nf := share("clean"), share("flaky")
		if nc <= c || nc >= 125 || nf >= f || nf <= 125 {
			t.Fatalf("week %d: clean %d%% after %d%%, flaky %d%% after %d%%, want both closer to 125%% and neither past it", w, nc, c, nf, f)
		}
		c, f = nc, nf
	}
	if c != 119 {
		t.Fatalf("a clean week then 4 weeks unobserved counts as %d%%, want the share table's 119%%", c)
	}
	if co, fo := order("clean-order"), order("flaky-order"); !slices.Equal(co, []domain.LineageID{"fair-clean", "clean"}) || !slices.Equal(fo, []domain.LineageID{"flaky", "fair-flaky"}) {
		t.Fatalf("after 4 weeks dormant the groups are ordered %v and %v, want clean behind its rival and flaky ahead of its own", co, fo)
	}
	if got := probes() - before; got != 0 {
		t.Fatalf("%d probes of servers no slot uses", got)
	}
}
