package daemon

import (
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// failedOnce is a failed check of id, as a round or a heartbeat that failed it would report.
func (l *timeline) failedOnce(id domain.LineageID) {
	l.set(id, killed())
	l.frame([]domain.Signal{{Stamp: domain.Stamp{Epoch: "e1", At: l.now()}, Server: l.r.ref(string(id)), Kind: domain.ActiveProbe,
		Purpose: domain.Check, Verdict: domain.Failed, Target: "t1"}})
}

func (l *timeline) standing(id domain.LineageID) standing {
	return l.r.c.evidence.book.records[id].standing()
}

// A member that failed is retried at 1, 2 and 4 minutes, and once a retry passes it is checked twice
// more, 30 s apart, which is its recovery. Ranked below the next two, it then gets nothing.
func TestTheLadderRetriesAndRecovers(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = 100 * time.Millisecond
	failed := l.now()
	l.failedOnce("d")
	l.run(time.Minute + 2*time.Minute + 2*time.Second)
	if got := l.count["check"]["d"]; got != 2 {
		t.Fatalf("%d retries in the first three minutes, want those at 1 and 2 minutes", got)
	}
	l.set("d", healthy(fourServers["d"]))
	l.runUntil(6*time.Minute, "recovery", func() bool { return l.standing("d") == passing })
	if got := l.count["check"]["d"]; got != 5 {
		t.Fatalf("%d checks to recover, want 3 retries and 2 spaced passes", got)
	}
	if took := l.now().Sub(failed); took < 8*time.Minute || took > 8*time.Minute+2*time.Second {
		t.Fatalf("recovered %s after the failure, want the retries at 1, 2 and 4 minutes and then 2 passes 30 s apart", took)
	}
	before := l.count["check"]["d"]
	l.run(time.Hour)
	if got := l.count["check"]["d"] - before; got != 0 {
		t.Fatalf("%d checks an hour after it recovered", got)
	}
}

// The ladder never ends. Its first six steps fail, so d is unverified, and it goes on doubling to
// 6 h, which repeats. When d works again its next retry passes and starts its recovery.
func TestTheLadderDoublesToSixHoursAndTheMemberComesBackOnIt(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = time.Second
	l.failedOnce("d")
	var steps []time.Duration
	last, checks := l.now(), 0
	l.onStep = func() {
		if n := l.count["check"]["d"]; n != checks {
			steps, last, checks = append(steps, l.now().Sub(last).Round(time.Minute)), l.now(), n
		}
		if n := l.count["check"]["d"]; n == unverifiedAfter && !unverified(l.r.record("d")) {
			t.Fatalf("d is not unverified after its first %d steps failed", n)
		}
	}
	l.run(21 * time.Hour)
	l.onStep = nil
	ladder := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute,
		64 * time.Minute, 128 * time.Minute, 256 * time.Minute, 6 * time.Hour, 6 * time.Hour}
	if !slices.Equal(steps, ladder) || !unverified(l.r.record("d")) {
		t.Fatalf("d was retried after %v and is unverified %v, want the ladder %v", steps, unverified(l.r.record("d")), ladder)
	}
	l.set("d", healthy(fourServers["d"]))
	l.runUntil(6*time.Hour, "d's retry passing", func() bool { return l.standing("d") == recovering })
	l.runUntil(2*time.Minute, "recovery", func() bool { return l.standing("d") == passing })
	if unverified(l.r.record("d")) {
		t.Fatal("d recovered and is still unverified")
	}
}

// A restart in the middle of the ladder, after ten minutes down, owes one retry at once and then the
// next step after it, never every step it slept through.
func TestARestartMidLadderOwesOnlyTheNextStep(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = 100 * time.Millisecond
	l.failedOnce("d")
	l.run(70 * time.Second)
	if got := l.count["check"]["d"]; got != 1 {
		t.Fatalf("%d retries in 70 s", got)
	}
	l.r.clock = l.r.clock.Add(10 * time.Minute)
	l.restart()
	l.run(30 * time.Second)
	if got := l.count["check"]["d"]; got != 2 {
		t.Fatalf("%d retries 30 s after a restart that slept through two steps, want one more", got)
	}
	l.run(3 * time.Minute)
	if got := l.count["check"]["d"]; got != 2 {
		t.Fatalf("%d retries within the 4 minutes the next step waits", got)
	}
	l.run(2 * time.Minute)
	if got := l.count["check"]["d"]; got != 3 {
		t.Fatalf("%d retries once the next step came", got)
	}
}

// a fails and the group moves to b, next in order. a passes its retries and recovers, but b failing
// next moves the group to c, never used, ahead of a and its fresh incident.
func TestASecondFailureTriesTheNextBeforeEitherReturns(t *testing.T) {
	l := newTimeline(t, fourServers)
	if current := l.r.server("eu"); current != "a" {
		t.Fatalf("serving %s, want a", current)
	}
	l.heartbeatJustBeat()
	l.set("a", silent(0))
	l.runUntil(failover, "move off a", l.moved)
	if to, _ := l.moves[0].to.Server(); to.Lineage != "b" {
		t.Fatalf("a's failure moved the group to %s, want b", to.Lineage)
	}
	l.set("a", healthy(fourServers["a"]))
	l.runUntil(5*time.Minute, "a's recovery", func() bool { return l.standing("a") == passing })
	l.moves = nil
	l.heartbeatJustBeat()
	l.set("b", silent(0))
	l.runUntil(failover, "move off b", l.moved)
	if to, _ := l.moves[0].to.Server(); to.Lineage != "c" {
		t.Fatalf("b's failure moved the group to %s, want c before a or b return", to.Lineage)
	}
}

// b's first six retries fail while a serves, so it is unverified when it works again and a dies. No
// failure round measures b, which it couldn't win, so the group serves b only once its spaced passes
// have made it stand passing.
func TestAnUnverifiedMemberIsInNoRoundAndServesOnlyOnceRecovered(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 40, "b": 50})
	l.stride = time.Second
	l.failedOnce("b")
	l.run(65 * time.Minute)
	if !unverified(l.r.c.evidence.book.records["b"]) {
		t.Fatal("b is not unverified after its ladder")
	}
	l.set("b", healthy(50))
	l.heartbeatJustBeat()
	l.set("a", silent(0))
	earlier := len(l.rounds)
	l.runUntil(failover, "a move off a", l.moved)
	l.runUntil(5*time.Minute, "serving b", func() bool { return l.r.server("eu") == "b" })
	if got := l.standing("b"); got != passing {
		t.Fatalf("serving b while it stands %d, want it to have recovered first", got)
	}
	b := l.r.ref("b")
	for _, q := range l.rounds[earlier:] {
		if q.Path == failurePath && slices.Contains(q.servers(), b) {
			t.Fatalf("failure round %d measured %v, with b in it while it stood %d", q.Round, q.servers(), l.standing("b"))
		}
	}
}

// d spends its ladder, and then its credentials are corrected. The new ref costs one check, which
// passes and starts its recovery.
func TestACredentialChangeOnAnUnverifiedMemberCostsOneCheck(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = time.Second
	l.failedOnce("d")
	l.run(65 * time.Minute)
	if !unverified(l.r.c.evidence.book.records["d"]) {
		t.Fatal("d is not unverified after its ladder")
	}
	l.set("d", healthy(fourServers["d"]))
	before := l.count["check"]["d"]
	l.rotate("d")
	l.run(10 * time.Second)
	r := l.r.c.evidence.book.records["d"]
	if got := l.count["check"]["d"] - before; got != 1 || r.Revision != l.r.ref("d").Digest || r.standing() != recovering {
		t.Fatalf("the new ref had %d checks and d stands %d, want one check that starts its recovery", got, r.standing())
	}
}

// The serving server dies, and the failure that moved the group off it puts it on the ladder. The
// rounds and heartbeats that saw it fail spend no step, so its first six retries all come.
func TestAServingFailureGetsTheLaddersFirstSixSteps(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = time.Second
	old := l.heartbeatJustBeat()
	l.set(old.Lineage, silent(0))
	l.runUntil(failover, "a move", l.moved)
	before := l.count["check"][old.Lineage]
	l.run(65 * time.Minute)
	if got := l.count["check"][old.Lineage] - before; got != unverifiedAfter || !unverified(l.r.c.evidence.book.records[old.Lineage]) {
		t.Fatalf("%s had %d retries after the move, want all %d and then unverified", old.Lineage, got, unverifiedAfter)
	}
}

// d fails five retries and passes its sixth. That pass starts its recovery and spends no step, so
// across a restart d is recovering and not unverified, and its spaced passes make it stand passing.
func TestAPassingLastRetryLeavesTheMemberVerifiedAcrossARestart(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = 100 * time.Millisecond
	l.failedOnce("d")
	l.onStep = func() {
		if l.r.record("d").Retries == unverifiedAfter-1 {
			l.set("d", healthy(fourServers["d"]))
		}
	}
	l.runUntil(70*time.Minute, "the passing sixth retry", func() bool { return l.standing("d") == recovering })
	l.onStep = nil
	l.restart()
	if r := l.r.record("d"); unverified(r) || r.standing() != recovering {
		t.Fatalf("after a restart d stands %d with %d retries, want recovering and not unverified", r.standing(), r.Retries)
	}
	l.runUntil(2*time.Minute, "recovery", func() bool { return l.standing("d") == passing })
}

// b is picked as the rival of a's failure round while it waits for its sixth retry. That retry fails
// while the round waits for tokens, so b is unverified when the round runs. It passes both rounds and
// still can't take the group until its spaced passes have made it stand passing.
func TestARivalThatSpendsItsLadderBeforeItsRoundCantWinIt(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 40, "b": 50})
	l.stride = time.Second
	l.adoptPlan()
	r := l.r.record("b")
	r.Revision = l.r.ref("b").Digest
	r.Failures, r.Retries, r.Last, r.SteppedAt = 6, 5, l.now().Add(-32*time.Minute), l.now().Add(-32*time.Minute)
	l.r.c.evidence.book.put("b", r, true)
	l.set("b", killed())
	l.set("a", func(p domain.ProbePurpose) reply {
		if p == domain.Confirmation {
			return killed()(p)
		}
		return healthy(40)(p)
	})
	l.loop.lanes[failurePath].bucket.tokens = 0
	l.loop.lanes[failurePath].bucket.last = l.now()
	l.frame([]domain.Signal{{Stamp: domain.Stamp{At: l.now(), Epoch: "e1"}, Server: l.r.ref("a"), Kind: domain.TCPInfo, Verdict: domain.Failed}})
	spent := false
	l.onStep = func() {
		if !spent && unverified(l.r.record("b")) {
			spent = true
			l.set("b", healthy(50))
		}
	}
	l.runUntil(time.Minute, "a move", l.moved)
	if !spent {
		t.Fatal("b never spent its ladder")
	}
	if to, _ := l.moves[0].to.Server(); to.Lineage == "b" && l.standing("b") != passing {
		t.Fatalf("moved to b standing %d with %d spaced passes", l.standing("b"), l.r.record("b").Streak)
	}
}

// b's retry and a comparison start together, and the comparison's failure lands first. The retry's
// failed check is still a step of the ladder, so b gets six checks and no more.
func TestAComparisonLandingFirstCostsTheLadderNoStep(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 40, "b": 50})
	l.stride = time.Second
	l.failedOnce("b")
	due, _ := retryDue(l.r.record("b"))
	m := l.r.memory("eu")
	m.CompareAt = due
	l.r.setMemory("eu", m)
	l.r.settle()
	before := l.count["check"]["b"]
	l.run(70 * time.Minute)
	if got := l.count["check"]["b"] - before; got != unverifiedAfter || !unverified(l.r.record("b")) {
		t.Fatalf("b had %d retries and is unverified %v, want all %d and then unverified", got, unverified(l.r.record("b")), unverifiedAfter)
	}
}

// b spends its ladder, passes once, and fails again before its recovery ends. It stays unverified, so
// it is no failure round's rival, until three spaced passes make it stand passing.
func TestAFailureDuringItsRecoveryLeavesASpentLadderSpent(t *testing.T) {
	w := newScene(domain.WANUp)
	at := t0
	hear := func(passed bool) {
		v := domain.Failed
		if passed {
			v = domain.Succeeded
		}
		w.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: at}, Signals: []domain.Signal{{Stamp: domain.Stamp{At: at, Epoch: "e1"}, Server: ref("b"), Kind: domain.ActiveProbe, Purpose: domain.Check, Verdict: v}}})
	}
	hear(false)
	for _, wait := range retryLadder[:unverifiedAfter] {
		at = at.Add(wait)
		hear(false)
	}
	if !unverified(w.book.records["b"]) {
		t.Fatal("b is not unverified after its ladder")
	}
	at = at.Add(time.Second)
	hear(true)
	at = at.Add(time.Second)
	hear(false)
	r := w.book.records["b"]
	e := evaluation{now: at, who: groupSlot(t), pool: poolOf(lin("a"), lin("b")), ev: w.evidence, t: defaultTimings()}
	if rivals := e.rivals([]domain.ServerRef{ref("a")}, failurePath); !unverified(r) || contender(r) || len(rivals) != 0 {
		t.Fatalf("after a pass and a failure b has %d retries and %d rivals, want it unverified and no rival", r.Retries, len(rivals))
	}
	for range recoverPasses {
		at = at.Add(recoverSpacing + time.Second)
		hear(true)
	}
	if r := w.book.records["b"]; unverified(r) || r.standing() != passing {
		t.Fatalf("after %d spaced passes b stands %d with %d retries, want passing", recoverPasses, r.standing(), r.Retries)
	}
}
