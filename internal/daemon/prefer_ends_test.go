package daemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// newPreferRigOn is slot "eu" opened on first before the others answered, then the others checked at
// their pings, so it serves a server that isn't the best.
func newPreferRigOn(t *testing.T, o preferOwner, first string, pings map[string]int) *preferRig {
	t.Helper()
	r := &preferRig{rig: newRig(t, t.TempDir()), o: o}
	for _, id := range []string{"a", "b", "c"} {
		r.seed(lineage(t, id, "good", "DE", "198.51.100."+id))
	}
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", exitIntent(t, "EU", o.exit))
	r.frame(domain.WANUp, r.check(first, domain.Succeeded, pings[first]))
	r.clock = r.clock.Add(r.c.timings.SettleFor)
	r.frame(domain.WANUp)
	if got := r.server("eu"); string(got) != first {
		t.Fatalf("opened on %v, want %s", got, first)
	}
	r.reportApplied("eu")
	var others []domain.Signal
	for _, id := range []string{"a", "b", "c"} {
		if id != first {
			others = append(others, r.check(id, domain.Succeeded, pings[id]))
		}
	}
	r.frame(domain.WANUp, others...)
	r.clock = r.clock.Add(6 * time.Minute)
	r.frame(domain.WANUp)
	return r
}

// pressed leaves the owner preferring id as a press on it does, its boost the gap the records give,
// stored as a press stores it.
func (r *preferRig) pressed(id domain.LineageID) preference {
	r.t.Helper()
	m := r.memory("eu")
	e := evaluation{now: r.clock, who: r.o.owner, pool: r.c.world.Pool(r.o.exit), memory: m, ev: r.c.evidence, t: r.c.timings}
	p := preference{Server: id, boost: boost{At: r.clock, Size: e.gap(id)}}
	if p.Size <= 0 {
		r.t.Fatalf("a press on %s leaves no gap", id)
	}
	m.Hand.at = heldPick{server: id, boost: p.boost}
	if err := r.c.store.commit(context.Background(), transaction{Owners: map[domain.Owner]policyMemory{r.o.owner: m}}); err != nil {
		r.t.Fatal(err)
	}
	r.setMemory("eu", m)
	return p
}

func (r *rig) preferred() *preference { return storedBoost(r.memory("eu").Hand) }

// compareRound answers the comparison round that is open, or runs the next one, each participant
// answering at its ping.
func (r *rig) compareRound(pings map[domain.LineageID]int) {
	r.t.Helper()
	if c := r.memory("eu").Campaign; c == nil || c.Path != improvePath {
		if r.memory("eu").CompareAt.IsZero() {
			r.frame(domain.WANUp)
		}
		if at := r.memory("eu").CompareAt; at.After(r.clock) {
			r.clock = at
		}
		r.frame(domain.WANUp)
	}
	c := r.memory("eu").Campaign
	if c == nil || c.Path != improvePath {
		r.t.Fatalf("no comparison round began: %+v", c)
	}
	var answers []domain.Signal
	for _, ref := range append([]domain.ServerRef{c.Incumbent}, c.Rivals...) {
		ping := time.Duration(pings[ref.Lineage]) * time.Millisecond
		answers = append(answers, domain.Signal{Stamp: domain.Stamp{Epoch: r.c.evidence.epoch, At: r.clock}, Server: ref, Kind: domain.ActiveProbe,
			Verdict: domain.Succeeded, Purpose: domain.Confirmation, Target: c.Target, Campaign: c.ID, Round: c.Round, Ping: &ping, NewConn: &ping})
	}
	r.frame(domain.WANUp, answers...)
}

// failoverRound answers the failure round that is due: the incumbent fails, and each rival passes when
// passes says so.
func (r *rig) failoverRound(passes map[domain.LineageID]bool) {
	r.t.Helper()
	c := r.memory("eu").Campaign
	if c == nil || c.Path != failurePath {
		r.t.Fatalf("no failure round is due: %+v", c)
	}
	var answers []domain.Signal
	for _, ref := range append([]domain.ServerRef{c.Incumbent}, c.Rivals...) {
		s := domain.Signal{Stamp: domain.Stamp{Epoch: r.c.evidence.epoch, At: r.clock}, Server: ref, Kind: domain.ActiveProbe, Verdict: domain.Failed,
			Purpose: domain.Confirmation, Target: "t1", Campaign: c.ID, Round: c.Round}
		if passes[ref.Lineage] {
			ping := 10 * time.Millisecond
			s.Verdict, s.Ping, s.NewConn = domain.Succeeded, &ping, &ping
		}
		answers = append(answers, s)
	}
	r.frame(domain.WANUp, answers...)
	r.clock = r.clock.Add(r.c.timings.FailureSpacing)
}

func (r *rig) lastReason() string {
	r.t.Helper()
	events, err := r.app.Switches(context.Background())
	if err != nil || len(events) == 0 {
		r.t.Fatalf("no switch: %v", err)
	}
	return events[0].Reason
}

var slowB = map[string]int{"a": 10, "b": 40, "c": 50}

// b, preferred with its gap of 30 ms on record, answers at 100 while a answers at 70: 30% and 30 ms faster,
// which wins without the boost.
var aBeatsB = map[domain.LineageID]int{"a": 70, "b": 100, "c": 100}

func TestARivalTwentyPercentFasterLosesToAFreshPreference(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		r.pressed("b")
		for range compareWins + 1 {
			r.compareRound(aBeatsB)
		}
		if got := r.server("eu"); got != "b" {
			t.Fatalf("moved to %s off a fresh preference", got)
		}
		if r.memory("eu").Streaks["a"] != 0 {
			t.Errorf("a won rounds against a fresh preference: %v", r.memory("eu").Streaks)
		}
	})
}

func TestTheSameRivalWinsOnceThePreferenceHasFaded(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		r.pressed("b")
		r.clock = r.clock.Add(20 * time.Hour)
		for range compareWins {
			r.compareRound(aBeatsB)
		}
		if got := r.server("eu"); got != "a" {
			t.Fatalf("serving %s, want a once the boost faded", got)
		}
		if got := r.lastReason(); got != "improvementMove" {
			t.Errorf("the move reads %s", got)
		}
		if p := r.preferred(); p != nil {
			t.Errorf("the move off b kept its preference %+v", p)
		}
	})
}

func TestASeverelySlowerPreferredServerLosesItsComparisonRounds(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", map[string]int{"a": 20, "b": 1000, "c": 1000})
		r.pressed("b")
		for range compareWins {
			r.compareRound(map[domain.LineageID]int{"a": 20, "b": 400, "c": 1000})
		}
		if got := r.server("eu"); got != "a" {
			t.Fatalf("serving %s, want a: b at 400 ms is 380 ms behind it, severely slower, which the boost doesn't hide", got)
		}
		if got := r.lastReason(); got != "improvementMove" {
			t.Errorf("the move reads %s", got)
		}
	})
}

func TestARevisionOfThePreferredServerKeepsItsPreference(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		was := r.pressed("b")
		rotated, _ := r.c.world.catalogue.Lookup("b")
		rotated.Ref.Digest[5] = 0xee
		r.storeLineages(transaction{Lineages: []domain.Lineage{rotated}})
		r.settle()
		if got, _ := r.c.state.slots["eu"].Selection.Server(); got != rotated.Ref {
			t.Fatalf("serving %+v, want b's new revision", got)
		}
		if got := r.lastReason(); got != "revisionChanged" {
			t.Fatalf("the move reads %s", got)
		}
		if p := r.preferred(); p == nil || *p != was {
			t.Errorf("a new revision of b left the preference %+v, want %+v", p, was)
		}
	})
}

func TestAFailoverOffThePreferredServerIsAServerFailedMove(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		r.pressed("b")
		r.frame(domain.WANUp, r.probe("b", domain.Failed, 0))
		for range failureRounds {
			r.failoverRound(map[domain.LineageID]bool{"a": true, "c": true})
		}
		if got := r.server("eu"); got != "a" {
			t.Fatalf("serving %s after b failed, want a", got)
		}
		if got := r.lastReason(); got != "failureMove" {
			t.Errorf("the failover reads %s", got)
		}
		if p := r.preferred(); p != nil {
			t.Errorf("a failover off b kept its preference %+v", p)
		}
	})
}

func TestAClosureEndsThePreferenceThoughLastServedStays(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		r.pressed("b")
		r.frame(domain.WANUp, r.probe("b", domain.Failed, 0))
		for range failureRounds {
			r.failoverRound(nil)
		}
		m := r.memory("eu")
		if reason, down := m.Selection.Down(); !down || reason != domain.ConfirmedDead {
			t.Fatalf("selection %+v, want closed", m.Selection)
		}
		if p := storedBoost(m.Hand); p != nil {
			t.Errorf("a closure kept the preference %+v", p)
		}
		if m.LastServed.Lineage != "b" {
			t.Errorf("last served %s, want b", m.LastServed.Lineage)
		}
		r.reportApplied("eu")
		r.clock = r.clock.Add(r.c.timings.RecoverBackoffMax)
		for range recoverPasses {
			r.clock = r.clock.Add(recoverSpacing)
			r.frame(domain.WANUp, r.check("a", domain.Succeeded, 10), r.check("b", domain.Succeeded, 40), r.check("c", domain.Succeeded, 50))
		}
		if got := r.server("eu"); got != "a" {
			t.Errorf("reopened on %s, want a, the best with no boost", got)
		}
	})
}

// eu leaves for the other kind of owner on the same servers, which nothing follows yet.
func otherOwner(o preferOwner) domain.Exit {
	if _, ok := o.owner.Group(); ok {
		return domain.SourceExit("good")
	}
	return domain.GroupExit(sourceGroup("good"))
}

func TestTheLastSlotLeavingCarriesNoPreferenceToItsNewExit(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		held := r.pressed("b")
		m := r.memory("eu")
		m.Hand.at = queuedPick{move: r.ref("b"), boost: held.boost}
		r.setMemory("eu", m)
		r.setExit("eu", otherOwner(o))
		r.settle()
		if got := r.server("eu"); got != "b" {
			t.Fatalf("serving %s after the Exit changed, want b", got)
		}
		if m := r.memory("eu"); pickState(m.Hand) != "none" {
			t.Errorf("the new Exit took the pick %+v", m.Hand)
		}
	})
}

func TestTheLastSlotLeavingCarriesNoLastWordToItsNewExit(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		there := &preferRig{rig: r.rig, o: preferOwners[0]}
		if there.o == o {
			there.o = preferOwners[1]
		}
		a := there.press("c", "there", func(ref domain.ServerRef) domain.Signal {
			sig := there.test(domain.Succeeded, 30)(ref)
			r.clock = r.clock.Add(time.Second)
			r.passes("b", 20)
			r.reportApplied("eu")
			r.setExit("eu", there.o.exit)
			r.settle()
			return sig
		})
		if a.status != http.StatusOK {
			t.Fatalf("a press on %s begun before a press on %s answered %d %s", there.o.name, o.name, a.status, a.body)
		}
		if got := r.server("eu"); got != "c" {
			t.Errorf("serving %s, want c", got)
		}
	})
}

func TestAPreferenceStaysWithTheSlotsThatRemain(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		r.putSlot("home", exitIntent(t, "Home", o.exit))
		r.settle()
		was := r.pressed("b")
		r.setExit("home", otherOwner(o))
		r.settle()
		if p := r.preferred(); p == nil || *p != was {
			t.Errorf("eu's owner prefers %+v after home left, want %+v", p, was)
		}
		home, _ := ownerOf(r.c.state.slots["home"])
		if p := storedBoost(r.c.state.owners[home].Hand); p != nil {
			t.Errorf("home took the preference %+v to its new Exit", p)
		}
	})
}

func TestAPreferenceEndsAtTwentyFourHoursWithoutARestart(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		r.pressed("b")
		r.clock = r.clock.Add(preferFor - time.Second)
		r.frame(domain.WANUp)
		if r.preferred() == nil {
			t.Fatal("the preference ended before its day was over")
		}
		r.clock = r.clock.Add(time.Second)
		r.frame(domain.WANUp)
		if p := r.preferred(); p != nil {
			t.Fatalf("the preference outlived its day: %+v", p)
		}
		r.close()
		again := newRigAt(t, r.dir, r.clock)
		if p := storedBoost(again.c.state.owners[o.owner].Hand); p != nil {
			t.Errorf("the end wasn't stored: a restart prefers %+v", p)
		}
	})
}

func TestARestartResumesThePreferenceWhereItWas(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		was := r.pressed("b")
		r.close()
		again := newRigAt(t, r.dir, r.clock.Add(time.Hour))
		if p := storedBoost(again.c.state.owners[o.owner].Hand); p == nil || p.Server != was.Server || !p.At.Equal(was.At) || p.Size != was.Size {
			t.Errorf("a restart prefers %+v, want %+v", p, was)
		}
	})
}

func TestAPreferenceThatExpiredWhileDownIsGone(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		r.pressed("b")
		r.close()
		again := newRigAt(t, r.dir, r.clock.Add(preferFor+time.Hour))
		again.c.evidence.epoch = "e1"
		again.frame(domain.WANUp)
		if p := storedBoost(again.c.state.owners[o.owner].Hand); p != nil {
			t.Errorf("a preference whose day ran out while the daemon was down outlived the first evaluate: %+v", p)
		}
	})
}

func TestAPreferenceFromTheFutureIsClampedOnLoad(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		r.pressed("b")
		r.close()
		back := r.clock.Add(-time.Hour)
		again := newRigAt(t, r.dir, back)
		if p := storedBoost(again.c.state.owners[o.owner].Hand); p == nil || !p.At.Equal(back) {
			t.Errorf("a clock gone back an hour loads the preference %+v, want it begun now", p)
		}
	})
}

func TestAReportPastAPreferencesEndRanksItWithoutItsTie(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", map[string]int{"a": 10, "b": 10, "c": 50})
		r.prefers("b", 1)
		wantOrder(t, r.order(), "b", "a", "c")
		r.clock = r.clock.Add(preferFor + time.Second)
		if a, b := r.priceOf("a"), r.priceOf("b"); a != b {
			t.Fatalf("a is priced %v and b %v, want a tie", a, b)
		}
		report := r.c.last
		report.Slots, report.Core.Running = nil, "another core"
		if err := r.c.applyReport(context.Background(), report); err != nil {
			t.Fatal(err)
		}
		order, first := r.published()
		wantOrder(t, order, "a", "b", "c")
		switch {
		case first == nil:
			t.Error("no first choice, want a")
		case *first != "a":
			t.Errorf("first choice %s, want a", *first)
		}
	})
}
