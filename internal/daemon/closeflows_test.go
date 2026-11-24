package daemon

import (
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

func TestWhatEachReasonDoesToOpenFlows(t *testing.T) {
	server := domain.Serving(domain.ServerRef{Lineage: "a"})
	cases := []struct {
		reason domain.Reason
		closes bool
	}{
		{domain.InitialSelection, false},
		{domain.FailureMove, true},
		{domain.SlownessMove, true},
		{domain.ImprovementMove, false},
		{domain.ExitChanged, true},
		{domain.ServerLeftFeed, true},
		{domain.RefusedByCore, true},
		{domain.RevisionChanged, false},
		{domain.Recovery, false},
		{domain.UDPMove, true},
	}
	if len(cases) != int(domain.UDPMove) {
		t.Fatalf("%d cases for %d reasons", len(cases), domain.UDPMove)
	}
	for _, c := range cases {
		if _, stated := closesFlows[c.reason]; !stated {
			t.Errorf("%s has no row in the table", reasonNames[c.reason])
		}
		if got := switchCloses(domain.Switch{Reason: c.reason, To: server}); got != c.closes {
			t.Errorf("%s: closes %t, want %t", reasonNames[c.reason], got, c.closes)
		}
		if !switchCloses(domain.Switch{Reason: c.reason, To: domain.Closed(domain.NoUsableServer)}) {
			t.Errorf("a closure with reason %s leaves flows open", reasonNames[c.reason])
		}
	}
}

func TestAForcedMoveOwesACloseAndAnOptionalOneDoesNot(t *testing.T) {
	l := newTimeline(t, fourServers)
	if got := l.r.targetOf("eu").CloseFlows; got != "" {
		t.Fatalf("an initial selection owes a close: %s", got)
	}
	current := l.r.server("eu")
	l.set(current, silent(0))
	l.runUntil(failover, "a failure move", l.moved)
	event := l.r.switches()[0]
	if event.Reason != "failureMove" {
		t.Fatalf("moved by %s", event.Reason)
	}
	if got := l.r.targetOf("eu").CloseFlows; got != domain.EventID(event.ID) {
		t.Fatalf("the plan closes under %q, not the move %q", got, event.ID)
	}
}

func TestALowerPingMoveOwesNoClose(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 20, "b": 40})
	current := l.r.server("eu")
	rival := domain.LineageID("b")
	if current == "b" {
		rival = "a"
	}
	l.set(current, healthy(200))
	l.set(rival, healthy(20))
	l.run(3 * time.Minute)
	l.r.improving("eu", l.r.ref(string(rival)))
	l.runUntil(25*time.Minute, "a move", l.moved)
	if l.moves[0].reason != "improvementMove" {
		t.Fatalf("moved %+v", l.moves[0])
	}
	if got := l.r.targetOf("eu").CloseFlows; got != "" {
		t.Fatalf("a Lower ping move closes under %q", got)
	}
}

// W3. A failure move to b is pending, its close unattempted, when a new revision of b supersedes it and
// owes no close of its own. The daemon restarts before either lands: the plan still closes under the
// failure move, the fresh Worker attempts it once, and once that is reported no plan names it.
func TestARestartKeepsTheCloseASupersedingMoveInherited(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	first := failureMove(t, r)
	rotated := lineage(t, "b", "good", "DE", "198.51.100.20")
	rotated.Ref.Digest = domain.Digest{9, 9}
	r.seed(rotated)
	r.settle()
	if second := r.c.state.pending["eu"]; second.ID == first || second.Reason != domain.RevisionChanged {
		t.Fatalf("pending %s %s, not a revision superseding %s", second.ID, reasonNames[second.Reason], first)
	}
	if got := r.targetOf("eu").CloseFlows; got != first {
		t.Fatalf("the superseding plan closes under %q, not the failure move %q", got, first)
	}
	w := r.reopened().closer()
	if got := w.r.targetOf("eu").CloseFlows; got != first {
		t.Fatalf("after a restart the plan closes under %q, not the failure move %q", got, first)
	}
	w.pass("eu", true, true)
	w.pass("eu", true, true)
	w.closedOnce("eu", first)
}

// A failure move to b owes a close no one has attempted, and optional moves follow it, new revisions of
// b: two in a row, two with a restart between them, or one after the failure move landed. Every plan the
// Worker gets closes under the failure move until a report shows it attempted, and it is attempted once.
func TestACloseOwedOutlastsOptionalMovesUntilItIsAttempted(t *testing.T) {
	t.Run("two revisions", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		first := failureMove(t, r)
		w := r.closer()
		for n := range byte(2) {
			reviseB(t, r, n)
			if got := w.target("eu").CloseFlows; got != first {
				t.Fatalf("after revision %d the plan closes under %q, not the failure move %q", n+1, got, first)
			}
		}
		w.pass("eu", true, true)
		w.pass("eu", true, true)
		w.closedOnce("eu", first)
	})
	t.Run("two revisions, a restart between", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		first := failureMove(t, r)
		reviseB(t, r, 0)
		r = r.reopened()
		reviseB(t, r, 1)
		w := r.closer()
		if got := w.target("eu").CloseFlows; got != first {
			t.Fatalf("the plan closes under %q, not the failure move %q", got, first)
		}
		w.pass("eu", true, true)
		w.pass("eu", true, true)
		w.closedOnce("eu", first)
	})
	t.Run("landed unclosed, then a revision", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		first := failureMove(t, r)
		w := r.closer()
		w.pass("eu", false, true)
		if _, pending := r.c.state.pending["eu"]; pending {
			t.Fatal("the failure move didn't land")
		}
		reviseB(t, r, 0)
		if got := w.target("eu").CloseFlows; got != first {
			t.Fatalf("the plan closes under %q, not the failure move %q", got, first)
		}
		w.pass("eu", true, true)
		w.pass("eu", true, true)
		w.closedOnce("eu", first)
	})
}

// reviseB gives b revision n, which moves eu onto it with a switch that ends no flows.
func reviseB(t *testing.T, r *rig, n byte) {
	t.Helper()
	before := r.operation("eu")
	b := lineage(t, "b", "good", "DE", "198.51.100.20")
	b.Ref.Digest = domain.Digest{9, n}
	r.seed(b)
	r.settle()
	if p, pending := r.c.state.pending["eu"]; !pending || p.ID == before || p.Reason != domain.RevisionChanged {
		t.Fatalf("pending %t %s %s, not a revision after %q", pending, p.ID, reasonNames[p.Reason], before)
	}
}

// W35. A forced move's close is attempted, and reported, while the move is still pending: the read after
// it failed. The daemon restarts. The fresh Worker's first report names no close, which acknowledges
// nothing, so the move lands with nothing closed again and no later plan names it.
func TestAnEmptyReportAfterARestartKeepsTheAcknowledgment(t *testing.T) {
	r := newRig(t, t.TempDir())
	op := failureMove(t, r)
	w := r.closer()
	w.pass("eu", true, false)
	if got := r.operation("eu"); got != op {
		t.Fatalf("pending %q, not the unlanded %q", got, op)
	}
	w = w.restart()
	w.pass("eu", true, true)
	if _, pending := w.r.c.state.pending["eu"]; pending {
		t.Fatal("the move didn't land")
	}
	w.pass("eu", true, true)
	w.closedOnce("eu", op)
}

// W23. state.db as release 98c73c9, installed on 2026-11-21, wrote it: slot eu on a, then a failure move
// to b still pending, its close unattempted. Its row names no debt, so the plan closes under the pending
// move as that release's did. A report of the attempt outlasts a restart, so the close is attempted once
// in all; and a debt a later switch inherits from that row outlasts one too.
func TestAnUpgradeKeepsAPendingForcedClose(t *testing.T) {
	t.Run("acknowledged once", func(t *testing.T) {
		r := newRigAt(t, releaseState(t), t0.Add(10*time.Minute))
		p, pending := r.c.state.pending["eu"]
		if !pending || p.Reason != domain.FailureMove {
			t.Fatalf("pending %+v", p)
		}
		if got := r.targetOf("eu").CloseFlows; got != p.ID {
			t.Fatalf("the plan closes under %q, not the pending failure move %q", got, p.ID)
		}
		w := r.closer()
		w.pass("eu", true, false)
		w = w.restart()
		if got := w.r.targetOf("eu").CloseFlows; got != "" {
			t.Fatalf("after a restart the plan closes under %q again", got)
		}
		w.pass("eu", true, true)
		w.pass("eu", true, true)
		w.closedOnce("eu", p.ID)
	})
	t.Run("inherited", func(t *testing.T) {
		r := newRigAt(t, releaseState(t), t0.Add(10*time.Minute))
		first := r.operation("eu")
		rotated := lineage(t, "b", "good", "DE", "198.51.100.20")
		rotated.Ref.Digest = domain.Digest{9, 9}
		r.seed(rotated)
		r.settle()
		if r.operation("eu") == first {
			t.Fatal("no switch superseded the failure move")
		}
		w := r.reopened().closer()
		if got := w.r.targetOf("eu").CloseFlows; got != first {
			t.Fatalf("after a restart the plan closes under %q, not the inherited %q", got, first)
		}
		w.pass("eu", true, true)
		w.closedOnce("eu", first)
	})
}

// releaseState is a data dir holding testdata/release-98c73c9.state.db.gz, unpacked.
func releaseState(t *testing.T) string {
	t.Helper()
	packed, err := os.Open(filepath.Join("testdata", "release-98c73c9.state.db.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer packed.Close()
	unpacked, err := gzip.NewReader(packed)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(unpacked)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(statePath(dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// failureMove takes slot eu from a to b in two failed rounds of a, and leaves that unapplied.
func failureMove(t *testing.T, r *rig) domain.EventID {
	t.Helper()
	electA(t, r)
	r.frame(domain.WANUp, r.probe("b", domain.Succeeded, 20), r.probe("a", domain.Failed, 0))
	for range 2 {
		c := r.memory("eu").Campaign
		s1, s2 := r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20)
		s1.Campaign, s1.Round, s2.Campaign, s2.Round = c.ID, c.Round, c.ID, c.Round
		r.frame(domain.WANUp, s1, s2)
	}
	if p := r.c.state.pending["eu"]; r.server("eu") != "b" || p.Reason != domain.FailureMove {
		t.Fatalf("serving %s, pending %s", r.server("eu"), reasonNames[p.Reason])
	}
	return r.operation("eu")
}

// reopened is the daemon started again on r's state.db, at r's clock.
func (r *rig) reopened() *rig {
	r.t.Helper()
	r.close()
	return newRigAt(r.t, r.dir, r.clock)
}

// closer stands in for the runtime's half of a close, as closeFlows does it: one Worker attempts each
// operation the plan it holds names once in its life, and every report names the last one it attempted.
type closer struct {
	r         *rig
	plan      gateway.Plan // The newest the controller sent.
	attempted map[domain.SlotID]domain.EventID
	attempts  *[]domain.EventID // Every life's.
}

func (r *rig) closer() *closer {
	return &closer{r: r, attempted: map[domain.SlotID]domain.EventID{}, attempts: new([]domain.EventID)}
}

// restart is the daemon started again, with a fresh Worker that has attempted nothing.
func (w *closer) restart() *closer {
	return &closer{r: w.r.reopened(), attempted: map[domain.SlotID]domain.EventID{}, attempts: w.attempts}
}

// target is slot's in the plan the Worker holds once the controller settles, as it does after every
// event.
func (w *closer) target(slot domain.SlotID) gateway.SlotTarget {
	w.r.t.Helper()
	w.r.settle()
	select {
	case w.plan = <-w.r.c.goals:
	default:
	}
	for _, target := range w.plan.Slots() {
		if target.Slot.ID == slot {
			return target
		}
	}
	w.r.t.Fatalf("the Worker holds no target for %s", slot)
	return gateway.SlotTarget{}
}

// pass runs one pass on the plan for slot. closes says the slot stands on its selection at its close's
// turn, shows that the report claims it there.
func (w *closer) pass(slot domain.SlotID, closes, shows bool) {
	w.r.t.Helper()
	target := w.target(slot)
	if op := target.CloseFlows; closes && op != "" && w.attempted[slot] != op {
		w.attempted[slot] = op
		*w.attempts = append(*w.attempts, op)
	}
	report := gateway.SlotReport{Slot: slot, Operation: target.Operation, Phase: gateway.Unknown}
	report.FlowsClosed = w.attempted[slot]
	if shows {
		report.Phase, report.Applied = gateway.Ready, target.Selection
	}
	generation := w.r.c.state.runtimeGeneration
	if err := w.r.c.applyReport(context.Background(), gateway.Report{Generation: generation, Slots: []gateway.SlotReport{report},
		Topology: gateway.Topology{Epoch: "e1", CorePID: 1, Generation: generation}}); err != nil {
		w.r.t.Fatal(err)
	}
}

// closedOnce says op's flows were the only ones closed, once in all, and that no plan names a close of
// slot's any more.
func (w *closer) closedOnce(slot domain.SlotID, op domain.EventID) {
	w.r.t.Helper()
	if !slices.Equal(*w.attempts, []domain.EventID{op}) {
		w.r.t.Errorf("closed under %v, want %s once", *w.attempts, op)
	}
	if got := w.r.targetOf(slot).CloseFlows; got != "" {
		w.r.t.Errorf("a plan after the close still closes under %q", got)
	}
}

// The clock goes back across a restart. eu goes to Direct at T, the first switch of its process, and that
// close is attempted and lands. Ten minutes on the daemon restarts, the clock is set back to T, and eu goes
// to Block, again the first switch of its process: a switch of its own, whose close the fresh Worker
// attempts, so no connection opened on Direct survives.
func TestAMoveAfterARestartAndAClockSetBackOwesItsOwnClose(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r = r.reopened()
	at := r.clock
	r.setExit("eu", domain.DirectExit())
	r.settle()
	direct := r.operation("eu")
	w := r.closer()
	w.pass("eu", true, true)
	if _, pending := r.c.state.pending["eu"]; pending {
		t.Fatal("Direct didn't land")
	}
	r.clock = at.Add(10 * time.Minute)
	w = w.restart()
	w.r.settle()
	w.r.clock = at
	w.r.setExit("eu", domain.BlockExit())
	w.r.settle()
	block := w.r.operation("eu")
	if block == direct {
		t.Errorf("Block's switch takes Direct's ID %s", direct)
	}
	if got := w.target("eu").CloseFlows; got != block {
		t.Errorf("Block's plan closes under %q, not its own %q", got, block)
	}
	w.pass("eu", true, true)
	if !slices.Equal(*w.attempts, []domain.EventID{direct, block}) {
		t.Errorf("closed under %v, want %s then %s", *w.attempts, direct, block)
	}
}

func TestDirectAndBlockOweACloseEachWay(t *testing.T) {
	for _, exit := range []domain.Exit{domain.DirectExit(), domain.BlockExit()} {
		t.Run(exit.Kind(), func(t *testing.T) {
			r := newRig(t, t.TempDir())
			electA(t, r)
			r.reportApplied("eu")
			r.setExit("eu", exit)
			r.settle()
			off := r.switches()[0]
			if off.Reason != "exitChanged" || r.targetOf("eu").CloseFlows != domain.EventID(off.ID) {
				t.Fatalf("off %+v, plan closes under %q", off, r.targetOf("eu").CloseFlows)
			}
			r.reportApplied("eu")
			r.clock = r.clock.Add(time.Minute)
			r.setExit("eu", domain.GroupExit(sourceGroup("good")))
			r.settle()
			back := r.switches()[0]
			if back.Reason != "exitChanged" || r.targetOf("eu").CloseFlows != domain.EventID(back.ID) {
				t.Fatalf("back %+v, plan closes under %q", back, r.targetOf("eu").CloseFlows)
			}
		})
	}
}

func TestAPendingMoveOwesItsCloseAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	op := pendingMove(t, r)
	if r.targetOf("eu").CloseFlows != op {
		t.Fatalf("the plan closes under %q, not %q", r.targetOf("eu").CloseFlows, op)
	}
	r.close()
	again := newRig(t, dir)
	again.settle()
	if got := again.targetOf("eu").CloseFlows; got != op {
		t.Fatalf("after a restart the plan closes under %q, not %q", got, op)
	}
}

func TestACloseDestroysOnlyTheCoresSocketsWithTheSlotsMark(t *testing.T) {
	const slot, other uint32 = 0x16a, 0x16b
	sockets := []tcpSocket{
		{Inode: 1, Mark: slot},
		{Inode: 2, Mark: other},
		{Inode: 3, Mark: 0},
		{Inode: 4, Mark: slot},
	}
	got := flowsOf(sockets, slot, map[uint32]bool{1: true, 2: true, 3: true})
	if len(got) != 1 || got[0].Inode != 1 {
		t.Fatalf("destroys %+v", got)
	}
}

func TestParseDiagMessageReadsTheMarkAndTheSocketID(t *testing.T) {
	msg := make([]byte, diagMsgHeader)
	msg[0] = afInet
	binary.BigEndian.PutUint16(msg[4:], 80)
	binary.BigEndian.PutUint16(msg[6:], 40000)
	copy(msg[8:], []byte{203, 0, 113, 81})
	copy(msg[24:], []byte{10, 70, 1, 2})
	attr := make([]byte, 8)
	binary.LittleEndian.PutUint16(attr, 8)
	binary.LittleEndian.PutUint16(attr[2:], attrMark)
	binary.LittleEndian.PutUint32(attr[4:], 0x16a)
	sock, ok := parseDiagMessage(append(msg, attr...))
	if !ok || sock.Mark != 0x16a || sock.Remote != netip.MustParseAddrPort("10.70.1.2:40000") {
		t.Fatalf("socket %+v, ok %t", sock, ok)
	}
	if [48]byte(msg[4:52]) != sock.id {
		t.Fatalf("id %x", sock.id)
	}
}

// Direct and Block send traffic to different places, so a move between them ends the slot's connections.
func TestMovingBetweenDirectAndBlockOwesAClose(t *testing.T) {
	for _, tc := range []struct{ from, to domain.Exit }{{domain.DirectExit(), domain.BlockExit()}, {domain.BlockExit(), domain.DirectExit()}} {
		t.Run(tc.from.Kind()+" to "+tc.to.Kind(), func(t *testing.T) {
			r := newRig(t, t.TempDir())
			electA(t, r)
			r.setExit("eu", tc.from)
			r.settle()
			r.reportApplied("eu")
			before := len(r.switches())
			r.setExit("eu", tc.to)
			r.settle()
			events := r.switches()
			if len(events) != before+1 {
				t.Fatalf("%d events after the Exit change, were %d", len(events), before)
			}
			if e := events[0]; e.Reason != "exitChanged" || e.To.Kind != tc.to.Kind() || r.targetOf("eu").CloseFlows != domain.EventID(e.ID) {
				t.Fatalf("event %+v, plan closes under %q", e, r.targetOf("eu").CloseFlows)
			}
		})
	}
}

// A slot made again on an id still being deleted reuses the allocation, which may still carry the
// connections of the slot before it, so its first move ends them wherever it goes.
func TestASlotMadeAgainBeforeItsDeletionEndsTheOldFlows(t *testing.T) {
	for _, exit := range []domain.Exit{domain.DirectExit(), domain.BlockExit(), domain.GroupExit(sourceGroup("good"))} {
		t.Run(exit.Kind(), func(t *testing.T) {
			r := newRig(t, t.TempDir())
			electA(t, r)
			if _, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"}); err != nil {
				t.Fatal(err)
			}
			r.settle()
			r.putSlot("eu", exitIntent(t, "Again", exit))
			r.settle()
			if target := r.targetOf("eu"); target.Operation == "" || target.CloseFlows != target.Operation {
				t.Fatalf("target %s, operation %q, closes under %q", describe(target.Selection), target.Operation, target.CloseFlows)
			}
		})
	}
}
