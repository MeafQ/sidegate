package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"sidegate/internal/domain"
)

// putSlotAnswer is the page's answer to an Exit write, as its handler encodes it.
func (r *rig) putSlotAnswer(id domain.SlotID, exit domain.Exit) SlotView {
	r.t.Helper()
	row, exists := r.c.state.slots[id]
	cmd := PutSlot{ID: id, Intent: exitIntent(r.t, "EU", exit)}
	if exists {
		cmd.Expected = row.Slot.Version
	}
	receipt, err := r.c.perform(context.Background(), cmd, domain.RequestID("k-"+exit.Kind()), []byte(exit.Kind()), true)
	if err != nil {
		r.t.Fatal(err)
	}
	var view SlotView
	if err := json.Unmarshal(receipt.Answer.Body, &view); err != nil {
		r.t.Fatal(err)
	}
	return view
}

func wantState(t *testing.T, view SlotView, status string, reason string) {
	t.Helper()
	got := ""
	if view.DownReason != nil {
		got = *view.DownReason
	}
	if view.Status != status || got != reason {
		t.Fatalf("the slot reads %s %q, want %s %q", view.Status, got, status, reason)
	}
	if status != "serving" && view.Server != nil {
		t.Fatalf("a slot that reads %s names server %s", status, *view.Server)
	}
}

func twoServers(t *testing.T, r *rig) {
	t.Helper()
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), lineage(t, "b", "good", "DE", "198.51.100.20"))
	r.c.evidence.epoch = "e1"
}

func onBlock(t *testing.T, r *rig) {
	t.Helper()
	r.putSlot("eu", exitIntent(t, "EU", domain.BlockExit()))
	r.frame(domain.WANUp)
	r.reportApplied("eu")
}

func TestASlotMadeOnASourceIsCheckingUntilItsServersAnswer(t *testing.T) {
	r := newRig(t, t.TempDir())
	twoServers(t, r)
	wantState(t, r.putSlotAnswer("eu", domain.SourceExit("good")), "checking", "")
	r.frame(domain.WANUp)
	wantState(t, r.slotViewOf("eu"), "checking", "")
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0))
	wantState(t, r.slotViewOf("eu"), "checking", "")
	r.frame(domain.WANUp, r.probe("b", domain.Failed, 0))
	wantState(t, r.slotViewOf("eu"), "failed", "noUsableServer")
	if r.targetOf("eu").Selection.OffVPN() || r.server("eu") != "" {
		t.Fatalf("the gate opened with nothing usable: %s", describe(r.targetOf("eu").Selection))
	}
}

func TestBlockToASourceWithNothingReadyIsCheckingNeverServing(t *testing.T) {
	r := newRig(t, t.TempDir())
	twoServers(t, r)
	onBlock(t, r)
	wantState(t, r.putSlotAnswer("eu", domain.SourceExit("good")), "checking", "")
	r.settle()
	wantState(t, r.slotViewOf("eu"), "checking", "")
	if got := r.targetOf("eu").Selection; got != domain.Closed(domain.NoExit) {
		t.Fatalf("the gate is %s while nothing is ready", describe(got))
	}
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	if view := r.slotViewOf("eu"); view.Status != "serving" || view.Server == nil || *view.Server != "a" {
		t.Fatalf("a ready server should take the slot: %+v", view)
	}
}

func TestBlockToASourceWhoseServersAllFailedReadsNoUsableServer(t *testing.T) {
	r := newRig(t, t.TempDir())
	twoServers(t, r)
	onBlock(t, r)
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Failed, 0))
	wantState(t, r.putSlotAnswer("eu", domain.SourceExit("good")), "failed", "noUsableServer")
	r.settle()
	wantState(t, r.slotViewOf("eu"), "failed", "noUsableServer")
}

// A server that passed is ready, so a slot the election hasn't put on it yet is still checking, here
// because the WAN isn't known to be up.
func TestAReadyServerTheElectionHasntTakenIsChecking(t *testing.T) {
	r := newRig(t, t.TempDir())
	twoServers(t, r)
	onBlock(t, r)
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Failed, 0))
	r.frame(domain.WANDown)
	r.setExit("eu", domain.SourceExit("good"))
	r.settle()
	if _, down := r.c.state.slots["eu"].Selection.Down(); !down {
		t.Fatalf("the slot opened over a WAN that is down: %s", describe(r.c.state.slots["eu"].Selection))
	}
	wantState(t, r.slotViewOf("eu"), "checking", "")
}

func TestAnExitWithNoServerItCanUseKeepsItsOwnWords(t *testing.T) {
	r := newRig(t, t.TempDir())
	gone := lineage(t, "a", "good", "DE", "198.51.100.10")
	gone.Presence = domain.Missing
	r.seed(gone)
	r.c.evidence.epoch = "e1"
	wantState(t, r.putSlotAnswer("eu", domain.ServerExit("a")), "failed", "serverMissing")
	r.settle()
	wantState(t, r.slotViewOf("eu"), "failed", "serverMissing")
}

func TestASlotMadeOverADeletionAnswersAsANewOne(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	if _, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"}); err != nil {
		t.Fatal(err)
	}
	made := func(id domain.SlotID, intent domain.SlotIntent) SlotView {
		t.Helper()
		receipt, err := r.takePage(pageWrite{Key: domain.RequestID("k-" + id), Fingerprint: []byte(id), Decision: runs{PutSlot{ID: id, Intent: intent}}})
		if err != nil {
			t.Fatal(err)
		}
		var view SlotView
		if err := json.Unmarshal(receipt.Answer.Body, &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	remade := made("eu", elected(t, "good"))
	published := r.app.Snapshot().Slots
	at := slices.IndexFunc(published, func(s SlotView) bool { return s.ID == "eu" })
	if at < 0 {
		t.Fatal("the view at the answer has no slot made over the deletion")
	}
	wantState(t, published[at], "checking", "")
	fresh := made("us", exitIntent(t, "US", domain.GroupExit(sourceGroup("good"))))
	wantState(t, fresh, "checking", "")
	wantState(t, remade, "checking", "")
}
