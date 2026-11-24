package daemon

import (
	"context"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

func (r *rig) slotViewOf(id domain.SlotID) SlotView {
	r.t.Helper()
	r.c.publish(r.clock)
	for _, s := range r.app.Snapshot().Slots {
		if s.ID == id {
			return s
		}
	}
	r.t.Fatalf("no slot %s in the view", id)
	return SlotView{}
}

func TestDirectAndBlockTakeASlotOffItsServerAtOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		exit   domain.Exit
		target func(domain.Selection) bool
	}{
		"Direct": {domain.DirectExit(), domain.Selection.IsDirect},
		"Block":  {domain.BlockExit(), domain.Selection.IsBlocked},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, t.TempDir())
			electA(t, r)
			r.reportApplied("eu")
			r.setExit("eu", tc.exit)
			r.settle()
			if got := r.targetOf("eu"); !tc.target(got.Selection) || got.Passage != (gateway.Passage{}) {
				t.Fatalf("target %+v", got)
			}
			if !tc.target(r.c.state.slots["eu"].Selection) {
				t.Fatalf("the slot stayed on %s", describe(r.c.state.slots["eu"].Selection))
			}
			if r.targetOf("eu").CloseFlows == "" {
				t.Error("leaving the server for the WAN or the drop left its connections open")
			}
			view := r.slotViewOf("eu")
			if view.Status != tc.exit.Kind() || view.DownReason != nil || view.Server != nil {
				t.Fatalf("view %+v", view)
			}
			r.reportApplied("eu")
			r.setExit("eu", domain.GroupExit(sourceGroup("good")))
			r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
			if got := r.targetOf("eu").Selection; got != domain.Serving(r.ref("a")) {
				t.Fatalf("back on the group the target is %+v", got)
			}
			events := r.switches()
			if events[0].Reason != "exitChanged" || events[1].Reason != "exitChanged" {
				t.Errorf("events %+v %+v, want two Exit changes", events[0], events[1])
			}
		})
	}
}

// A renamed slot writes no row: only a move does.
func TestARenameOfADirectSlotLogsNothing(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.setExit("eu", domain.DirectExit())
	r.settle()
	r.reportApplied("eu")
	before := len(r.switches())
	row := r.c.state.slots["eu"]
	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: row.Slot.Version, Intent: exitIntent(t, "Renamed", domain.DirectExit())}); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if after := len(r.switches()); after != before {
		t.Fatalf("%d events, were %d", after, before)
	}
}

// A gate opens only for a client UniFi has. Direct is no way round that.
func TestDirectOpensNoGateWithoutAClient(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.setExit("eu", domain.DirectExit())
	r.settle()
	r.app.control.key.syncer = &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}}
	r.settle()
	got := r.targetOf("eu").Selection
	if reason, down := got.Down(); !down || reason != domain.PlumbingUnavailable {
		t.Fatalf("target %+v", got)
	}
}

func TestBlockSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	electA(t, r)
	r.setExit("eu", domain.BlockExit())
	r.settle()
	r.reportApplied("eu")
	r.settle()
	wantKey := r.c.planKey
	events := len(r.switches())
	r.close()

	r2 := newRig(t, dir)
	if !r2.c.state.slots["eu"].Slot.Intent.Exit().Block() {
		t.Fatal("Block wasn't durable")
	}
	r2.settle()
	if !r2.targetOf("eu").Selection.IsBlocked() {
		t.Fatalf("after a restart the target is %+v", r2.targetOf("eu").Selection)
	}
	if r2.c.planKey != wantKey {
		t.Fatalf("plan changed across a restart:\n%s\nwant\n%s", r2.c.planKey, wantKey)
	}
	if after := len(r2.switches()); after != events {
		t.Fatalf("%d events after a restart, were %d", after, events)
	}
}

// A source Exit is a group of what the source keeps: its slots follow one server, and the source
// can't go while a slot uses it.
func TestASourceExitIsAGroupOfWhatItKeeps(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.putSlot("two", exitIntent(t, "Two", domain.SourceExit("good")))
	r.setExit("eu", domain.SourceExit("good"))
	r.settle()
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	o, _ := ownerOf(r.c.state.slots["two"])
	if want := domain.SourceOwner("good"); o != want || r.server("two") != r.server("eu") || r.server("eu") == "" {
		t.Fatalf("owner %v, servers %s and %s", o, r.server("eu"), r.server("two"))
	}
	r.c.publish(r.clock)
	if used := r.app.Snapshot().Sources[0].UsedBy; len(used) != 2 {
		t.Errorf("usedBy %v", used)
	}
	_, err := r.c.accept(context.Background(), DeleteSource{ID: "good"})
	r.wantRefused(err, KindReferenced, RefusedInUse)
}

func TestAnExitThatWouldOnlyCloseIsRefused(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.putGroup("empty", "Empty", ruleMembers(t, rule{Countries: []string{"SE"}}))
	for name, refused := range map[string]struct {
		exit   domain.Exit
		reason Reason
	}{"an empty group": {domain.GroupExit("empty"), RefusedGroupEmpty}, "a missing source": {domain.SourceExit("nobody"), RefusedSourceGone}} {
		_, err := r.c.accept(context.Background(), PutSlot{ID: "new", Intent: exitIntent(t, "New", refused.exit)})
		if r.wantRefused(err, KindValidation, refused.reason) == nil {
			t.Errorf("%s was admitted", name)
		}
	}
}

func TestTheOwnerTurnsAClientBackOnAndRecreatesOne(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	ctx := context.Background()
	r.app.bootstrap.ClientDNS = "1.1.1.1"
	off := false
	r.c.clientsKnown = true
	r.c.owned["eu"] = unifi.Owned{NetworkID: r.c.state.slots["eu"].Slot.Allocation.NetworkID, Slot: "eu", Enabled: &off}
	_, err := r.c.accept(ctx, RecreateClient{ID: "eu"})
	r.wantRefused(err, KindValidation, RefusedClientExists)
	if _, err := r.c.accept(ctx, EnableClient{ID: "eu"}); err != nil {
		t.Fatal(err)
	}
	if want, err := r.c.wantOf(r.c.state.slots["eu"]); err != nil || !want.Enable {
		t.Error("the next pass won't turn the client on")
	}

	row := r.c.state.slots["eu"]
	row.Slot.Allocation.NetworkID = "n-gone"
	r.c.state.slots["eu"] = row
	delete(r.c.owned, "eu")
	if got := r.slotViewOf("eu").Unifi.Client; got != "deleted" {
		t.Fatalf("client %s", got)
	}
	_, err = r.c.accept(ctx, EnableClient{ID: "eu"})
	r.wantRefused(err, KindValidation, RefusedNotDisabled)
	if _, err := r.c.accept(ctx, RecreateClient{ID: "eu"}); err != nil {
		t.Fatal(err)
	}
	if id := r.c.state.slots["eu"].Slot.Allocation.NetworkID; id != "" {
		t.Errorf("the slot still wants client %s", id)
	}
	_, err = r.c.accept(ctx, EnableClient{ID: "nobody"})
	r.wantRefused(err, KindNotFound, RefusedSlotGone)
}

// An Enable answered 202 is the owner's until a pass shows the client on: a restart before that pass
// still asks, and one after it doesn't.
func TestATurnOnOutlivesARestartBeforeThePassThatDoesIt(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.app.bootstrap.ClientDNS = "1.1.1.1"
	off := false
	r.c.clientsKnown = true
	id := r.c.state.slots["eu"].Slot.Allocation.NetworkID
	r.c.owned["eu"] = unifi.Owned{NetworkID: id, Slot: "eu", Enabled: &off}
	if _, err := r.c.accept(context.Background(), EnableClient{ID: "eu"}); err != nil {
		t.Fatal(err)
	}
	r.close()

	again := newRigAt(t, r.dir, r.clock.Add(time.Second))
	again.app.bootstrap.ClientDNS = "1.1.1.1"
	if want, err := again.c.wantOf(again.c.state.slots["eu"]); err != nil || !want.Enable {
		t.Fatalf("after a restart before any pass, the next pass won't turn the client on: %v", err)
	}
	on := true
	report := clientReport{From: again.c.key.syncer, Report: unifi.Report{Inventory: unifi.Inventory{Owned: []unifi.Owned{{NetworkID: id, Slot: "eu", Enabled: &on}}}}}
	if err := again.c.applyClients(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	again.close()

	third := newRigAt(t, r.dir, r.clock.Add(2*time.Second))
	third.app.bootstrap.ClientDNS = "1.1.1.1"
	if want, err := third.c.wantOf(third.c.state.slots["eu"]); err != nil || want.Enable {
		t.Errorf("a pass showed the client on, and a restart still asks to turn it on: %v", err)
	}
}

// runtimeReport is what the runtime reads back for a slot on its target: a closed gate when the
// target is down, and otherwise an open one carrying it.
func (r *rig) runtimeReport(slot domain.SlotID) {
	r.t.Helper()
	target := r.targetOf(slot)
	report := gateway.SlotReport{Slot: slot, Phase: gateway.Ready, Applied: target.Selection, Operation: r.operation(slot)}
	if _, down := target.Selection.Down(); down {
		report.Phase = gateway.Closed
	}
	if err := r.c.applyReport(context.Background(), gateway.Report{Generation: r.c.state.runtimeGeneration, Slots: []gateway.SlotReport{report}}); err != nil {
		r.t.Fatal(err)
	}
}

func TestReadbackOnTheWANOrTheDropLandsTheMove(t *testing.T) {
	for _, exit := range []domain.Exit{domain.DirectExit(), domain.BlockExit()} {
		t.Run(exit.Kind(), func(t *testing.T) {
			r := newRig(t, t.TempDir())
			electA(t, r)
			r.setExit("eu", exit)
			r.settle()
			r.runtimeReport("eu")
			if r.slotViewOf("eu").Pending || r.switches()[0].AppliedAt == nil {
				t.Fatalf("readback on %s left the move pending: %+v", exit.Kind(), r.switches()[0])
			}
		})
	}
}

// A slot with no VPN Client yet has a closed gate, which isn't Direct.
func TestAClosedGateDoesNotLandAMoveToDirect(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.app.control.key.syncer = &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}}
	row := r.c.state.slots["eu"]
	row.Slot.Allocation.NetworkID = ""
	r.c.state.slots["eu"] = row
	r.setExit("eu", domain.DirectExit())
	r.settle()
	r.runtimeReport("eu")
	if !r.slotViewOf("eu").Pending || r.switches()[0].AppliedAt != nil {
		t.Fatalf("a closed gate landed the move to Direct: %+v", r.switches()[0])
	}
}

func TestASourceSwitchCarriesTheSourcesRevision(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.setExit("eu", domain.DirectExit())
	r.settle()
	r.reportApplied("eu")
	r.setExit("eu", domain.SourceExit("good"))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	event := r.switches()[0]
	if want := domain.IntentRevision(r.c.state.sources["good"].Revision); event.Owner.Kind != "source" || event.Intent != want {
		t.Fatalf("event %+v, want the source's revision %d", event, want)
	}
}

// Off a server, a slot is nobody's to probe, serves no server, and leaving by choice is no forced move.
func TestAnOffSlotIsNotProbedNorServingNorForcedOff(t *testing.T) {
	for _, exit := range []domain.Exit{domain.DirectExit(), domain.BlockExit()} {
		t.Run(exit.Kind(), func(t *testing.T) {
			r := newRig(t, t.TempDir())
			electA(t, r)
			r.setExit("eu", exit)
			r.settle()
			r.runtimeReport("eu")
			left := r.clock
			r.clock = r.clock.Add(time.Hour)
			if obs, _ := r.c.observation(); len(obs.Serving) != 0 || len(obs.Beats) != 0 || len(obs.Rounds) != 0 {
				t.Errorf("an off slot is still probed: %+v", obs)
			}
			served, err := usageSince(context.Background(), r.c.store.writer, t0, r.clock)
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range served.servers["a"] {
				if i.to.After(left) {
					t.Errorf("time off a server counted as served: %+v", i)
				}
			}
			counts, err := r.app.Queries.state.events(context.Background(), t0, r.clock, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if len(counts.ForcedSwitchAways) != 0 {
				t.Errorf("an Exit change counted as forced: %+v", counts.ForcedSwitchAways)
			}
			if r.slotViewOf("eu").Pending {
				t.Error("the move off stayed pending")
			}
		})
	}
}
