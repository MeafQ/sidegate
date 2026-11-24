package runtime

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

// closeRig is a Worker whose core API, `xray api` and destroy all write to one log, in order, each
// inbound call with the tags it names.
func closeRig(t *testing.T) (*Worker, func() []string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "log")
	note := func(line string) {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.WriteString(line + "\n")
	}
	tools := t.TempDir()
	adi := "*' adi '*) echo adi $(grep -o '\"tag\":\"[^\"]*\"' | cut -d'\"' -f4) >>" + log + ";;"
	if err := os.WriteFile(filepath.Join(tools, "timeout"), []byte("#!/bin/sh\nshift 3\ncase \" $* \" in "+adi+" esac\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	w, err := Open(Options{DataDir: t.TempDir(), RunDir: t.TempDir(), CloseFlows: func(pid int, mark uint32) (int, error) {
		note(fmt.Sprintf("destroy %d %d", pid, mark))
		return 1, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	w.core.api = fakeCore(t, func(method string, req []byte) ([]byte, string) {
		if strings.HasSuffix(method, "RemoveInbound") {
			pbFields(req, func(num int, v []byte) error {
				note("rmi " + string(v))
				return nil
			})
		}
		return nil, "0"
	})
	return w, func() []string {
		data, _ := os.ReadFile(log)
		return strings.Fields(strings.ReplaceAll(string(data), " ", "_"))
	}
}

func closePlan(t *testing.T, w *Worker, targets ...SlotTarget) (Plan, corePlan) {
	t.Helper()
	plan := Plan{slots: targets, servers: map[domain.ServerRef]domain.Server{}}
	for _, target := range targets {
		if server, serving := target.Selection.Server(); serving {
			plan.servers[server] = testServer(t, vlessHop(t, "root", "198.51.100.1", ""))
		}
	}
	return plan, w.corePlan(plan, backoffStart, nil)
}

func upLinks(ordinals ...uint8) kernelObs {
	have := kernelObs{links: map[string]link{}}
	for _, o := range ordinals {
		have.links[linkName(domain.Allocation{Ordinal: o})] = link{up: true}
	}
	return have
}

// viewOn is a pass that has read core.
func viewOn(w *Worker, core coreObservation) *corePass {
	c := w.corePass()
	c.hear(goodRead{obs: core})
	return c
}

func onCore(selections map[domain.SlotID]domain.Selection) coreObservation {
	return coreObservation{Running: true, PID: 7, Applied: selections}
}

func TestAClosePointsOnlyAtTheOperationsSlotAfterItsOverrideReadsBack(t *testing.T) {
	prior, next := testRef("prior"), testRef("next")
	w, log := closeRig(t)
	slot, other := workerSlot("slot1", 1), workerSlot("slot2", 2)
	target := workerTarget(slot, domain.Serving(next), MoveFrom(prior, backoffStart.Add(30e9)))
	target.CloseFlows = "op1"
	plan, cp := closePlan(t, w, target, workerTarget(other, domain.Serving(next), Passage{}))
	have := upLinks(1, 2)
	ctx := context.Background()

	onPrior := onCore(map[domain.SlotID]domain.Selection{"slot1": domain.Serving(prior), "slot2": domain.Serving(next)})
	if steps, errs := w.closeFlows(ctx, viewOn(w, onPrior), plan, cp, have); len(steps) != 0 || len(errs) != 0 || len(log()) != 0 {
		t.Fatalf("closed before the override read back: %v %v %v", steps, errs, log())
	}
	landed := onCore(map[domain.SlotID]domain.Selection{"slot1": domain.Serving(next), "slot2": domain.Serving(next)})
	steps, errs := w.closeFlows(ctx, viewOn(w, landed), plan, cp, have)
	if len(errs) != 0 || len(steps) != 1 {
		t.Fatalf("steps %v, errors %v", steps, errs)
	}
	udp := inboundTag("slot1", "udp")
	want := []string{"rmi_" + udp, "adi_" + udp, fmt.Sprintf("destroy_7_%d", flowMark(slot.Allocation))}
	if !slices.Equal(log(), want) {
		t.Fatalf("did %v, want %v: the UDP inbound replaced, the TCP listener left alone", log(), want)
	}

	if steps, _ := w.closeFlows(ctx, viewOn(w, landed), plan, cp, have); len(steps) != 0 || len(log()) != len(want) {
		t.Fatalf("a repeated plan closed twice: %v", log())
	}

	target.CloseFlows = "op2"
	plan, cp = closePlan(t, w, target)
	if steps, _ := w.closeFlows(ctx, viewOn(w, landed), plan, cp, have); len(steps) != 1 || len(log()) != 2*len(want) {
		t.Fatalf("a new operation closed %v", log())
	}
}

func TestAClosureClosesFlowsOnceItsGateIsDownAndHasNoInboundToReplace(t *testing.T) {
	w, log := closeRig(t)
	slot := workerSlot("slot1", 1)
	target := workerTarget(slot, domain.Closed(domain.ConfirmedDead), Passage{})
	target.CloseFlows = "dead"
	plan, cp := closePlan(t, w, target)
	core := onCore(map[domain.SlotID]domain.Selection{})
	if steps, _ := w.closeFlows(context.Background(), viewOn(w, core), plan, cp, upLinks(1)); len(steps) != 0 {
		t.Fatalf("closed with the gate up: %v", steps)
	}
	if steps, _ := w.closeFlows(context.Background(), viewOn(w, core), plan, cp, upLinks()); len(steps) != 1 {
		t.Fatalf("steps %v", steps)
	}
	if got := log(); len(got) != 1 || !strings.HasPrefix(got[0], "destroy") {
		t.Fatalf("did %v", got)
	}
}

func TestDirectCloseWaitsForItsOverride(t *testing.T) {
	server := testRef("server")
	w, log := closeRig(t)
	target := workerTarget(workerSlot("slot1", 1), domain.Direct(), Passage{})
	target.CloseFlows = "flip"
	plan, cp := closePlan(t, w, target)
	before := onCore(map[domain.SlotID]domain.Selection{"slot1": domain.Serving(server)})
	if steps, _ := w.closeFlows(context.Background(), viewOn(w, before), plan, cp, upLinks(1)); len(steps) != 0 {
		t.Fatalf("closed before the flip read back: %v", steps)
	}
	after := onCore(map[domain.SlotID]domain.Selection{"slot1": domain.Direct()})
	if steps, _ := w.closeFlows(context.Background(), viewOn(w, after), plan, cp, upLinks(1)); len(steps) != 1 {
		t.Fatalf("steps %v, log %v", steps, log())
	}
}

func TestARemovedSlotClosesItsFlowsOnce(t *testing.T) {
	w, log := closeRig(t)
	slot := workerSlot("slot1", 1)
	plan := Plan{removals: []domain.Slot{slot}}
	core := onCore(nil)
	if steps, _ := w.closeFlows(context.Background(), viewOn(w, core), plan, corePlan{}, upLinks(1)); len(steps) != 0 {
		t.Fatalf("closed with the gate up: %v", steps)
	}
	for range 2 {
		w.closeFlows(context.Background(), viewOn(w, core), plan, corePlan{}, upLinks())
	}
	if got := log(); len(got) != 1 {
		t.Fatalf("did %v", got)
	}
}

func TestEachSlotInboundCarriesItsOwnMark(t *testing.T) {
	server := testRef("server")
	w := &Worker{}
	plan := Plan{servers: map[domain.ServerRef]domain.Server{server: testServer(t, vlessHop(t, "root", "198.51.100.1", ""))}, slots: []SlotTarget{
		workerTarget(workerSlot("slot1", 1), domain.Serving(server), Passage{}), workerTarget(workerSlot("slot2", 2), domain.Direct(), Passage{}),
	}}
	marks := map[string]any{}
	for _, in := range renderInbounds(w.corePlan(plan, backoffStart, nil)) {
		if stream, ok := in["streamSettings"].(obj); ok {
			tag := in["tag"].(string)
			marks[tag] = stream["sockopt"].(obj)["mark"]
			if network := in["settings"].(obj)["network"]; !strings.HasSuffix(tag, "."+network.(string)) {
				t.Errorf("inbound %s listens on %v", tag, network)
			}
		}
	}
	want := map[string]any{"s.slot1.tcp": uint32(0x16a), "s.slot1.udp": uint32(0x16a), "s.slot2.tcp": uint32(0x16b), "s.slot2.udp": uint32(0x16b)}
	if !maps.Equal(marks, want) {
		t.Fatalf("marks %v, want %v", marks, want)
	}
}

// A report names, for each slot, the last close its Worker attempted, so the controller can stop owing
// it: none from a fresh Worker, the operation from the pass that spends it, and the same in every pass
// after, whatever its plan names.
func TestAReportNamesTheLastCloseItsWorkerAttempted(t *testing.T) {
	r := newCoreRig(t)
	fresh, err := r.pass(r.plan(1, Passage{}, Passage{}))
	if one, two := r.slot(fresh, "slot1"), r.slot(fresh, "slot2"); err != nil || one.FlowsClosed != "" || two.FlowsClosed != "" {
		t.Fatalf("a fresh Worker reports %q and %q, error %v", one.FlowsClosed, two.FlowsClosed, err)
	}
	spent, err := r.pass(forcedClose(r.plan(2, Passage{}, Passage{}), 0))
	if one, two := r.slot(spent, "slot1"), r.slot(spent, "slot2"); err != nil || len(r.destroyed) != 1 || one.FlowsClosed != "opslot1" || two.FlowsClosed != "" {
		t.Fatalf("the closing pass reports %q and %q, destroyed %v, error %v", one.FlowsClosed, two.FlowsClosed, r.destroyed, err)
	}
	after, err := r.pass(r.plan(3, Passage{}, Passage{}))
	if one := r.slot(after, "slot1"); err != nil || len(r.destroyed) != 1 || one.FlowsClosed != "opslot1" {
		t.Fatalf("the pass after reports %q, destroyed %v, error %v", one.FlowsClosed, r.destroyed, err)
	}
}
