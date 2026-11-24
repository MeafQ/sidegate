package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// dropSlot is the owner deleting the slot storeSlot made, and the daemon then stopping the core, which has
// nothing left to run.
func (r *installRig) dropSlot() {
	r.t.Helper()
	ctx := context.Background()
	st, q, err := openStore(ctx, r.data)
	if err != nil {
		r.t.Fatal(err)
	}
	defer st.close()
	defer q.close()
	if err := st.commit(ctx, transaction{DeletedSlots: []domain.SlotID{"eu"}}); err != nil {
		r.t.Fatal(err)
	}
	r.coreDies()
	if needed, err := coreNeeded(ctx, r.data); needed || err != nil {
		r.t.Fatalf("the core is still needed: %v, %v, so this proves nothing", needed, err)
	}
}

// cutBeforeTheSwitch runs all of the steps before the boundary.
func (r *installRig) cutBeforeTheSwitch(request LifecycleRequest) {
	r.t.Helper()
	plan, err := PlanLifecycle(context.Background(), request)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range plan.steps[:plan.boundary] {
		if err := s.do(context.Background()); err != nil {
			r.t.Fatal(err)
		}
	}
	if _, err := readRecord(r.data); err != nil {
		r.t.Fatalf("no record before the boundary, so this proves nothing: %v", err)
	}
}

// An upgrade planned while a core served, and cut before its switch, binds nothing later: the owner deletes
// the slot, the daemon stops the core, and a retry of a good release lands. So it is for a cut after the
// record says the switch began and before it selects its release, and for one to the release that is current.
func TestARetryOverASwitchThatNeverBeganDecidesAfresh(t *testing.T) {
	for name, cut := range map[string]func(r *installRig){
		"before its switch": func(r *installRig) { r.cutBeforeTheSwitch(r.request(Upgrade, r.release("two"))) },
		"before it selected its release": func(r *installRig) {
			r.cutBeforeTheSwitch(r.request(Upgrade, r.release("two")))
			record, _ := readRecord(r.data)
			if err := record.enter(r.data, phaseSwitching); err != nil {
				r.t.Fatal(err)
			}
		},
		"before its switch to the release that is current": func(r *installRig) {
			r.cutBeforeTheSwitch(r.request(Upgrade, r.release("one")))
		},
	} {
		for _, retry := range []string{"two", "one"} {
			t.Run(name+", then "+retry, func(t *testing.T) {
				r := newInstallRig(t)
				r.mustRun(r.request(Install, r.release("one")))
				r.storeSlot(domain.DirectExit(), domain.Direct())
				r.runCore()
				cut(r)
				if record, err := readRecord(r.data); err != nil || !record.CoreRequired {
					t.Fatalf("the cut upgrade's record %+v, %v requires no core, so this proves nothing", record, err)
				}
				r.dropSlot()
				request := r.request(Upgrade, r.release(retry))
				request.Outcome = r.outcomeFile()
				_, out, err := r.run(request)
				if err != nil || currentRelease(r.data) != r.releaseOf(retry) {
					t.Errorf("the retry: release %s is current: %v\n%s", currentRelease(r.data), err, out)
				}
				if ended := r.readOutcome(); ended.Outcome != outcomeLanded || !r.journalGone() {
					t.Errorf("the retry says %+v, its journal gone %v", ended, r.journalGone())
				}
			})
		}
	}
}

// Nothing else of that record stands either: a retry that fails puts back the units as they are when the
// retry is planned, and its own record names its own candidate alone and the core as it is then.
func TestARetryOverASwitchThatNeverBeganRecordsAfresh(t *testing.T) {
	for _, first := range []string{"two", "one"} {
		t.Run(first, func(t *testing.T) {
			r := newInstallRig(t)
			r.mustRun(r.request(Install, r.release("one")))
			one := currentRelease(r.data)
			r.cutBeforeTheSwitch(r.request(Upgrade, r.release(first)))
			// The owner changes one unit and links the other before the retry.
			owners := filepath.Join(r.root, "owners-core.service")
			coreUnit := filepath.Join(r.units, gateway.CoreUnitName)
			daemonText := "as the owner left " + daemonUnitName
			os.WriteFile(filepath.Join(r.units, daemonUnitName), []byte(daemonText), 0o644)
			os.WriteFile(owners, []byte("the owner's own core unit"), 0o644)
			os.Remove(coreUnit)
			os.Symlink(owners, coreUnit)
			r.storeSlot(domain.BlockExit(), domain.Blocked())
			r.runCore()

			plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("broken")))
			if err != nil {
				t.Fatal(err)
			}
			var record rollbackRecord
			during(t, plan, "record release", func() { record, err = readRecord(r.data) })
			var out strings.Builder
			if err := applyLifecycle(context.Background(), plan, &out); outcomeOf(err) != outcomeRolledBack || currentRelease(r.data) != one {
				t.Errorf("a retry whose daemon doesn't answer: release %s is current: %v\n%s", currentRelease(r.data), err, out.String())
			}
			if err != nil || !slices.Equal(record.Selected, []string{r.releaseOf("broken")}) || !record.CoreRequired {
				t.Errorf("the retry's record holds %+v: %v", record, err)
			}
			if raw, _ := os.ReadFile(filepath.Join(r.units, daemonUnitName)); string(raw) != daemonText {
				t.Errorf("%s holds %q, not what the owner left", daemonUnitName, raw)
			}
			if target, err := os.Readlink(coreUnit); err != nil || target != owners {
				t.Errorf("the owner's linked core unit came back as %q: %v", target, err)
			}
		})
	}
}

// A retry stopped before its own switch leaves the record it found, and the next start says of that one
// that it changed nothing.
func TestARetryRefusedOverASwitchThatNeverBeganLeavesIt(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	request := r.request(Upgrade, r.release("two"))
	request.Outcome = r.outcomeFile()
	r.cutBeforeTheSwitch(request)
	before, _ := os.ReadFile(journalRecord(r.data))
	plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("three")))
	if err != nil {
		t.Fatal(err)
	}
	ctx, term := context.WithCancel(context.Background())
	during(t, plan, "record release", term)
	var out strings.Builder
	if err := applyLifecycle(ctx, plan, &out); outcomeOf(err) != outcomeRefused {
		t.Errorf("a retry stopped before its switch: %v\n%s", err, out.String())
	}
	if after, _ := os.ReadFile(journalRecord(r.data)); string(after) != string(before) {
		t.Errorf("the refused retry left the record %s, not %s", after, before)
	}
	if out, err := r.recover(); err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	if ended := r.readOutcome(); ended.Outcome != outcomeRefused || !r.journalGone() {
		t.Errorf("the next start said %+v, the journal gone %v", ended, r.journalGone())
	}
}

// An upgrade to the release that is current, cut before its switch, is refused at the next start as any
// other is: nothing is put back, and the core its plan required isn't started.
func TestRecoverOverASwitchThatNeverBeganOnlySaysSo(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	r.storeSlot(domain.DirectExit(), domain.Direct())
	r.runCore()
	request := r.request(Upgrade, r.release("one"))
	request.Outcome = r.outcomeFile()
	r.cutBeforeTheSwitch(request)
	r.dropSlot()
	r.clearCalls()
	if out, err := r.recover(); err != nil {
		t.Errorf("recover: %v\n%s", err, out)
	}
	if ended := r.readOutcome(); ended.Outcome != outcomeRefused || currentRelease(r.data) != one || !r.journalGone() {
		t.Errorf("recover said %+v, with release %s current and the journal gone %v", ended, currentRelease(r.data), r.journalGone())
	}
	if log := r.callLog(); strings.Contains(log, "systemctl") {
		t.Errorf("recover touched systemd:\n%s", log)
	}
}

// An install over a record of a switch that never began journals its own: cut once it selected its release,
// the next start goes back to what was current when it was planned.
func TestAnInstallOverASwitchThatNeverBeganJournalsItsOwn(t *testing.T) {
	for name, arrange := range map[string]func(r *installRig) (was string){
		"a first install": func(r *installRig) string {
			r.cutAfter(r.request(Install, r.release("one")), "record what current names")
			return ""
		},
		"an upgrade, whose daemon was then lost": func(r *installRig) string {
			r.mustRun(r.request(Install, r.release("one")))
			r.cutBeforeTheSwitch(r.request(Upgrade, r.release("two")))
			os.Remove(filepath.Join(r.data, "current", bundleDaemon))
			return r.releaseOf("one")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			was := arrange(r)
			r.cutAfter(r.request(Install, r.release("broken")), "make release")
			if currentRelease(r.data) != r.releaseOf("broken") {
				t.Fatal("broken isn't current, so this proves nothing")
			}
			if out, err := r.recover(); err != nil {
				t.Errorf("recover: %v\n%s", err, out)
			}
			if got := currentRelease(r.data); got != was {
				t.Errorf("release %q is current, want %q back", got, was)
			}
		})
	}
}
