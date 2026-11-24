package daemon

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// answerRounds plays one round of every campaign an owner runs, with failing failing and every other
// participant passing.
func (r *rig) answerRounds(failing ...string) {
	r.t.Helper()
	var round []domain.Signal
	for _, m := range r.c.state.owners {
		c := m.Campaign
		if c == nil {
			continue
		}
		for _, ref := range append([]domain.ServerRef{c.Incumbent}, c.Rivals...) {
			s := r.probe(string(ref.Lineage), domain.Succeeded, 20)
			if slices.Contains(failing, string(ref.Lineage)) {
				s = r.probe(string(ref.Lineage), domain.Failed, 0)
			}
			s.Campaign, s.Round = c.ID, c.Round
			round = append(round, s)
		}
	}
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, round...)
}

func TestAGroupNobodyUsesHoldsNothingAndDoesntCampaign(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	group := domain.GroupOwner(sourceGroup("good"))
	if _, held := r.c.state.owners[group]; !held {
		t.Fatal("the group a slot uses holds no memory")
	}
	r.setExit("eu", domain.ServerExit("b"))
	r.settle()
	if _, held := r.c.state.owners[group]; held {
		t.Fatal("a group no slot uses still holds memory")
	}
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20))
	for o, m := range r.c.state.owners {
		if c := m.Campaign; c != nil && c.Incumbent == r.ref("a") {
			t.Fatalf("%s campaigns on the server only the unused group had: %+v", o, c)
		}
	}
	p, _ := r.c.observation()
	if len(p.Rounds) != 0 {
		t.Fatalf("rounds with nothing on the group: %+v", p.Rounds)
	}
	state, _, err := r.c.store.recover(context.Background(), r.clock, r.c.timings)
	if err != nil {
		t.Fatal(err)
	}
	if _, stored := state.owners[group]; stored {
		t.Fatal("state.db keeps memory for a group no slot uses")
	}
}

// A slot whose Exit is one server keeps its own memory beside a group on the same server, so the
// group's move leaves it where it is.
func TestAServerExitSlotKeepsItsOwnSelection(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.putSlot("fixed", exitIntent(t, "Fixed", domain.ServerExit("a")))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	r.reportApplied("fixed")
	if r.server("eu") != "a" || r.server("fixed") != "a" {
		t.Fatalf("on %v and %v", r.server("eu"), r.server("fixed"))
	}
	r.clock = r.clock.Add(time.Minute)
	moves := len(r.switches())
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20))
	for range 2 {
		r.answerRounds("a")
	}
	if r.server("eu") != "b" {
		t.Fatalf("the group stayed on %v", r.server("eu"))
	}
	if r.server("fixed") == "b" {
		t.Fatal("a server Exit followed the group")
	}
	for _, e := range r.switches()[:len(r.switches())-moves] {
		if e.Owner.Kind == "group" && slices.Contains(e.Slots, "fixed") || e.Owner.Slot == "eu" {
			t.Fatalf("a move crossed owners: %+v", e)
		}
	}
	if r.memory("fixed").KeepOut["a"].After(r.clock) {
		t.Fatal("the group's keep-out reached the server Exit")
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// failOverToB takes slot "eu", elected on a, to b after a fails, with nothing shown yet.
func failOverToB(t *testing.T, r *rig) domain.EventID {
	t.Helper()
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20))
	for range 2 {
		r.answerRounds("a")
	}
	if r.server("eu") != "b" || r.memory("eu").KeepOut["a"].After(r.clock) {
		t.Fatalf("the failover: on %v, keep-out %v", r.server("eu"), r.memory("eu").KeepOut)
	}
	return r.operation("eu")
}

func wantFailedOver(t *testing.T, r *rig, slot domain.SlotID) {
	t.Helper()
	m := r.memory(slot)
	if m.Selection != domain.Serving(r.ref("b")) || m.LastServed != r.ref("b") || !m.KeepOut["a"].After(r.clock) {
		t.Fatalf("after the failover showed: selection %v, last served %v, keep-out %v", describe(m.Selection), m.LastServed.Lineage, m.KeepOut)
	}
}

// A group's move to b shown by one of its slots leaves the other still owing it, restart or not: still
// allowed on a, still under the move's operation, and still closing its flows when it lands.
func TestASlotThatHasntShownItsGroupsMoveStillOwesItAfterARestart(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	twoSlotsOnOneGroup(t, r)
	r.reportApplied("two")
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20), r.probe("c", domain.Succeeded, 30), r.probe("d", domain.Succeeded, 40))
	for range 2 {
		r.answerRounds("a")
	}
	move := r.operation("two")
	if r.server("one") == "a" || move == "" || r.operation("one") != move {
		t.Fatalf("no group move: one on %v, operations %v and %v", r.server("one"), r.operation("one"), move)
	}
	r.reportApplied("one")
	owed := r.targetOf("two")
	if owed.Operation != move || owed.CloseFlows != move || !movesFrom(owed, r.ref("a")) {
		t.Fatalf("before the restart, two owes %+v", owed)
	}
	r.close()

	r2 := newRigAt(t, dir, r.clock)
	got := r2.targetOf("two")
	if got.Operation != move || got.CloseFlows != move || !samePassage(got, owed) {
		t.Fatalf("after a restart, two owes %+v, want %+v", got, owed)
	}
	if got := r2.c.state.slots["two"].LastServed; got != r2.ref("a") {
		t.Fatalf("two's last server: %v", got.Lineage)
	}
	if target := r2.targetOf("one"); target.Operation != "" || target.Passage != (gateway.Passage{}) {
		t.Fatalf("one, which showed the move, owes %+v", target)
	}
	r2.reportApplied("two")
	if len(r2.c.state.pending) != 0 {
		t.Fatalf("still pending: %v", r2.c.state.pending)
	}
}

func TestAJoinShownWithItsGroupsMoveKeepsTheKeepOut(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	failOverToB(t, r)
	r.putSlot("late", elected(t, "good"))
	r.settle()
	if r.server("late") != "b" || r.operation("late") == "" || r.operation("late") == r.operation("eu") {
		t.Fatalf("the join: on %v under %v", r.server("late"), r.operation("late"))
	}
	r.reportApplied("eu", "late")
	wantFailedOver(t, r, "eu")
	if len(r.c.state.pending) != 0 {
		t.Fatalf("still pending: %v", r.c.state.pending)
	}
}

func TestAMovingGroupsLastSlotBringsTheMoveToItsNewGroup(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.putPicked("other", "Other", "a", "b")
	move := failOverToB(t, r)
	r.setExit("eu", domain.GroupExit("other"))
	r.settle()
	if o, _ := ownerOf(r.c.state.slots["eu"]); o != domain.GroupOwner("other") || r.operation("eu") != move {
		t.Fatalf("eu follows %v under %v", o, r.operation("eu"))
	}
	r.reportApplied("eu")
	wantFailedOver(t, r, "eu")
}

func TestASlotLeavingAMovingGroupBringsTheMoveAlong(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.putPicked("other", "Other", "a", "b")
	r.putSlot("stay", elected(t, "good"))
	r.settle()
	r.reportApplied("stay")
	failOverToB(t, r)
	if r.operation("eu") != r.operation("stay") {
		t.Fatalf("no group move: on %v", r.server("eu"))
	}
	r.setExit("eu", domain.GroupExit("other"))
	r.settle()
	r.reportApplied("eu")
	wantFailedOver(t, r, "eu")
	if r.memory("stay").KeepOut["a"].After(r.clock) {
		t.Fatal("a slot of another owner started the group's clocks")
	}
	r.reportApplied("stay")
	wantFailedOver(t, r, "stay")
}

// Joining a group closed after a confirmed failure is no new outage, so its backoff stays as it was.
func TestJoiningAClosedGroupLeavesItsBackoff(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Failed, 0))
	for range 3 {
		r.answerRounds("a", "b")
	}
	if reason, down := r.c.state.slots["eu"].Selection.Down(); !down || reason != domain.ConfirmedDead {
		t.Fatalf("eu isn't closed as confirmed dead: %v", describe(r.c.state.slots["eu"].Selection))
	}
	r.reportApplied("eu")
	closed := r.memory("eu")
	if closed.Backoff.Closures != 1 {
		t.Fatalf("%d closures", closed.Backoff.Closures)
	}
	r.clock = r.clock.Add(5 * time.Second)
	r.putSlot("late", elected(t, "good"))
	r.settle()
	if r.operation("late") == "" {
		t.Fatal("the join has no move to show")
	}
	r.reportApplied("late")
	if m := r.memory("late"); m.Backoff.Closures != closed.Backoff.Closures || !m.Backoff.NotBefore.Equal(closed.Backoff.NotBefore) {
		t.Fatalf("a join took the group from %d closures to %d, reopening %v later", closed.Backoff.Closures, m.Backoff.Closures, m.Backoff.NotBefore.Sub(closed.Backoff.NotBefore))
	}
}

// movesFrom says the target's passage is a move from the server from.
func movesFrom(target gateway.SlotTarget, from domain.ServerRef) bool {
	prior, until, ok := target.Passage.Allows(target.Selection)
	return ok && prior == from && target.Passage == gateway.MoveFrom(from, until)
}

// samePassage says two targets let the core stay on the same server until the same deadline.
func samePassage(a, b gateway.SlotTarget) bool {
	pa, ua, oka := a.Passage.Allows(a.Selection)
	pb, ub, okb := b.Passage.Allows(b.Selection)
	return oka == okb && pa == pb && ua.Equal(ub)
}
