package daemon

import (
	"fmt"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// closeOnA fails a's heartbeat and both rounds of its campaign with nothing beside it, and shows the
// closure.
func closeOnA(t *testing.T, r *rig) {
	t.Helper()
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0))
	r.answerRounds("a")
	r.answerRounds("a")
	if reason, down := r.c.state.slots["eu"].Selection.Down(); !down || reason != domain.ConfirmedDead {
		t.Fatalf("eu is %v, want closed on a dead server", r.c.state.slots["eu"].Selection)
	}
	r.reportApplied("eu")
}

func passA(r *rig, times int) {
	for range times {
		r.clock = r.clock.Add(time.Second)
		r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10))
	}
}

// recoverA gives a the spaced passes a recovery takes.
func recoverA(r *rig) {
	for pass := range recoverPasses {
		if pass > 0 {
			r.clock = r.clock.Add(recoverSpacing)
		}
		passA(r, 1)
	}
}

// A third closure soon after reopenings keeps the slot's last server out for two minutes, and a
// restart in them doesn't shorten it.
func TestAReopeningsBackoffSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"))
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", elected(t, "good"))
	passA(r, 1)
	r.reportApplied("eu")
	for closure := 1; closure < 3; closure++ {
		closeOnA(t, r)
		recoverA(r)
		if got := r.server("eu"); got != "a" {
			t.Fatalf("after closure %d eu came back on %q", closure, got)
		}
		r.reportApplied("eu")
	}
	closeOnA(t, r)
	closedAt := r.clock
	r.close()

	r = newRigAt(t, dir, closedAt.Add(time.Second))
	r.c.evidence.epoch = "e1"
	recoverA(r)
	if _, down := r.c.state.slots["eu"].Selection.Down(); !down {
		t.Fatalf("after a restart eu reopened on a %s after its third closure", r.clock.Sub(closedAt))
	}
	r.clock = closedAt.Add(2 * time.Minute)
	passA(r, 1)
	if got := r.server("eu"); got != "a" {
		t.Errorf("past its two minutes eu is still closed")
	}
}

// One slot showed its group's move and the other lags. The lagging one moving to an unused group
// gives it the server the move put the group on, not the one the slot still reads.
func TestALaggingSlotLeavingItsGroupBringsTheGroupsLastServer(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), lineage(t, "b", "good", "DE", "198.51.100.20"))
	r.c.evidence.epoch = "e1"
	r.putSlot("one", elected(t, "good"))
	r.putSlot("two", elected(t, "good"))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	r.reportApplied("one", "two")
	r.clock = r.clock.Add(6 * time.Minute)
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0))
	r.answerRounds("a")
	r.answerRounds("a")
	if got := r.server("one"); got != "b" {
		t.Fatalf("the failover went to %q", got)
	}
	r.reportApplied("one")
	r.putPicked("h", "H", "a", "b")
	r.setExit("two", domain.GroupExit("h"))
	if got := r.memory("two").LastServed; got != r.ref("b") {
		t.Errorf("the group two moved to holds %s as its last server, want b, where the move it owes put its old group", got.Lineage)
	}
}

// A failover to b that no slot has shown yet is replaced by another, off b. The server the first one
// left still stays out.
func TestAFailoverReplacedBeforeAnySlotShowedItKeepsItsServerOut(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.seed(lineage(t, "c", "good", "DE", "198.51.100.30"))
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0))
	r.answerRounds("a")
	r.answerRounds("a")
	if got := r.server("eu"); got != "b" {
		t.Fatalf("the failover went to %q", got)
	}
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, r.probe("b", domain.Failed, 0))
	r.answerRounds("a", "b")
	r.answerRounds("a", "b")
	if got := r.server("eu"); got != "c" {
		t.Fatalf("the second failover went to %q", got)
	}
	r.reportApplied("eu")
	keepOut := r.memory("eu").KeepOut
	for _, id := range []domain.LineageID{"a", "b"} {
		if !r.clock.Before(keepOut[id]) {
			t.Errorf("%s isn't kept out after the moves off it: %v", id, keepOut)
		}
	}
}

// A closure leaves its server failed on the record, so after a restart the slot waits for the three
// spaced passes a recovery takes, not the first pass since.
func TestAfterARestartAClosedSlotWaitsForItsServerToRecover(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"))
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", elected(t, "good"))
	passA(r, 1)
	r.reportApplied("eu")
	closeOnA(t, r)
	closedAt := r.clock
	r.close()

	r = newRigAt(t, dir, closedAt.Add(time.Minute))
	r.c.evidence.epoch = "e1"
	for pass := 1; pass < recoverPasses; pass++ {
		passA(r, 1)
		if got := r.server("eu"); got != "" {
			t.Fatalf("after a restart eu reopened on %s after %d passes", got, pass)
		}
		r.clock = r.clock.Add(recoverSpacing)
	}
	passA(r, 1)
	if got := r.server("eu"); got != "a" {
		t.Errorf("after %d spaced passes eu is on %q, want a", recoverPasses, got)
	}
}

// A closure proves the group failing from its first failed round, across a restart too, so a server
// the campaign never reached doesn't reopen the slot on a pass from before it.
func TestAfterARestartAClosureStillDiscountsOlderPasses(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	var lineages []domain.Lineage
	for i, id := range names {
		lineages = append(lineages, lineage(t, id, "good", "DE", fmt.Sprintf("198.51.100.%d", 10+i)))
	}
	r.seed(lineages...)
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", elected(t, "good"))
	var passes []domain.Signal
	for _, id := range names {
		passes = append(passes, r.probe(id, domain.Succeeded, 10))
	}
	r.frame(domain.WANUp, passes...)
	if got := r.server("eu"); got != "a" {
		t.Fatalf("elected %q", got)
	}
	r.reportApplied("eu")
	r.clock = r.clock.Add(6 * time.Minute)
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0))
	r.answerRounds(names...)
	r.answerRounds(names...)
	if _, down := r.c.state.slots["eu"].Selection.Down(); !down {
		t.Fatalf("eu is %v, want closed", r.c.state.slots["eu"].Selection)
	}
	if got := r.record("h"); got.standing() != passing || got.Last.After(t0) {
		t.Fatalf("h was reached by the campaign: %+v", got)
	}
	r.reportApplied("eu")
	r.close()

	r = newRigAt(t, dir, r.clock.Add(time.Minute))
	r.c.evidence.epoch = "e1"
	r.frame(domain.WANUp)
	if got := r.server("eu"); got != "" {
		t.Fatalf("after a restart eu reopened on %s on its pass from before the closure", got)
	}
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, r.probe("h", domain.Succeeded, 10))
	if got := r.server("eu"); got != "h" {
		t.Errorf("a pass of h since the closure left eu on %q", got)
	}
}
