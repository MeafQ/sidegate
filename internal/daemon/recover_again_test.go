package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// coreSwitched is an upgrade from one to two, each with a core of its own, cut short once the core
// runs two's. It returns one, the core one runs on and one's units.
func (r *installRig) coreSwitched() (one, oldCore string, units map[string]string) {
	r.t.Helper()
	r.mustRun(r.request(Install, r.release("one")))
	one = currentRelease(r.data)
	units = r.earlierUnits()
	r.runCore()
	oldCore = r.coreExe()
	r.coreBytes = []byte("core two")
	r.pinCore()
	request := r.request(Upgrade, r.release("two"))
	request.Outcome = r.outcomeFile()
	r.cutAfter(request, "restart the core")
	if r.coreExe() == oldCore {
		r.t.Fatal("the core didn't switch, so this proves nothing")
	}
	return one, oldCore, units
}

func (r *installRig) planRecover() LifecyclePlan {
	r.t.Helper()
	plan, err := PlanLifecycle(context.Background(), r.request(Recover, ""))
	if err != nil {
		r.t.Fatal(err)
	}
	return plan
}

func (r *installRig) wantUnsaid() {
	r.t.Helper()
	if raw, err := os.ReadFile(r.outcomeFile()); !errors.Is(err, os.ErrNotExist) {
		r.t.Errorf("the outcome says %s", raw)
	}
}

// wantPutBack checks what a finished recovery leaves, and that one's daemon then says so.
func (r *installRig) wantPutBack(one, oldCore string, units map[string]string) {
	r.t.Helper()
	if got := currentRelease(r.data); got != one {
		r.t.Errorf("release %s is current, want %s back", got, one)
	}
	for name, want := range units {
		if raw, _ := os.ReadFile(filepath.Join(r.units, name)); string(raw) != want {
			r.t.Errorf("%s holds %q, want %q back", name, raw, want)
		}
		if raw, _ := os.ReadFile(filepath.Join(r.loaded(), name)); string(raw) != want {
			r.t.Errorf("systemd holds %s as %q, want %q back", name, raw, want)
		}
	}
	if oldCore != "" && r.coreExe() != oldCore {
		r.t.Errorf("the core runs %s, not the old release's %s", r.coreExe(), oldCore)
	}
	if err := confirmPutBack(r.data, r.lockPath(), &one); err != nil {
		r.t.Fatal(err)
	}
	if ended := r.readOutcome(); ended.Outcome != outcomeRolledBack {
		r.t.Errorf("the old daemon said %+v", ended)
	}
	if !r.journalGone() {
		r.t.Error("the journal outlived the confirmed put-back")
	}
}

// The disk lost the candidate's whole directory: nothing about it stops the old release from coming
// back.
func TestRecoverPutsBackWithTheCandidatesDirectoryGone(t *testing.T) {
	r := newInstallRig(t)
	one, units := r.broken()
	if err := os.RemoveAll(releaseDir(r.data, r.releaseOf("broken"))); err != nil {
		t.Fatal(err)
	}
	if out, err := r.recover(); err != nil {
		t.Errorf("recover: %v\n%s", err, out)
	}
	r.wantPutBack(one, "", units)
}

// A recovery cut short after any of its steps runs every step again at the next start, and until one
// run has finished the old release's daemon says nothing.
func TestARecoveryCutShortRunsWholeAgain(t *testing.T) {
	steps := 0
	for cut := 1; cut == 1 || cut < steps; cut++ {
		r := newInstallRig(t)
		one, oldCore, units := r.coreSwitched()
		plan := r.planRecover()
		steps = len(plan.steps)
		for _, s := range plan.steps[:cut] {
			if err := s.do(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if err := confirmPutBack(r.data, r.lockPath(), &one); err != nil {
			t.Fatal(err)
		}
		r.wantUnsaid()
		if out, err := r.recover(); err != nil {
			t.Fatalf("the start after a recovery cut after %q: %v\n%s", plan.steps[cut-1].say, err, out)
		}
		r.wantPutBack(one, oldCore, units)
	}
}

// current goes back before anything else, so a unit file that can't be put back leaves the old
// release selected, and the next start puts back the rest.
func TestRecoverPutsTheSelectionBackFirst(t *testing.T) {
	r := newInstallRig(t)
	one, oldCore, units := r.coreSwitched()
	blocked := filepath.Join(r.units, "."+daemonUnitName+".new.new")
	if err := os.MkdirAll(filepath.Join(blocked, "left"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := r.recover(); err == nil {
		t.Fatalf("recover passed over a unit it can't write, so this proves nothing:\n%s", out)
	}
	if got := currentRelease(r.data); got != one {
		t.Errorf("with a unit that can't be put back, release %s is current, want %s", got, one)
	}
	if ended := r.readOutcome(); ended.Outcome != outcomePutBackFailed {
		t.Errorf("a recovery that failed said %+v", ended)
	}
	os.RemoveAll(blocked)
	if out, err := r.recover(); err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	r.wantPutBack(one, oldCore, units)
}

// The owner's next upgrade over a recovery cut short takes over what it still owes: cut short before
// its own switch, it leaves the put-back to finish at the next start.
func TestAnUpgradeOverARecoveryCutShortStillOwesIt(t *testing.T) {
	r := newInstallRig(t)
	one, oldCore, units := r.coreSwitched()
	if err := r.planRecover().steps[0].do(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := r.request(Upgrade, r.release("three"))
	request.Outcome = r.outcomeFile()
	r.cutAfter(request, "record release")
	if out, err := r.recover(); err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	r.wantPutBack(one, oldCore, units)
}

// A kill once the old daemon said rolledBack and before the journal went: the next start keeps what
// was said, and the journal goes. The next upgrade then puts back the release that answered.
func TestAKillWhileSayingRolledBackKeepsIt(t *testing.T) {
	for _, next := range []string{"the next start", "the next upgrade"} {
		t.Run(next, func(t *testing.T) {
			r := newInstallRig(t)
			one, units := r.broken()
			if out, err := r.recover(); err != nil {
				t.Fatalf("recover: %v\n%s", err, out)
			}
			// A leftover that can't be emptied where the journal goes stops its removal, as a kill before
			// it would.
			if os.Geteuid() == 0 {
				t.Skip("root removes what a read-only directory holds")
			}
			stuck := journalDir(r.data) + ".old"
			os.MkdirAll(filepath.Join(stuck, "held"), 0o700)
			os.Chmod(stuck, 0o500)
			err := confirmPutBack(r.data, r.lockPath(), &one)
			os.Chmod(stuck, 0o700)
			if ended := r.readOutcome(); err == nil || r.journalGone() || ended.Outcome != outcomeRolledBack {
				t.Fatalf("confirming with a journal that can't go: %v, said %+v, so this proves nothing", err, ended)
			}
			if next == "the next upgrade" {
				request := r.request(Upgrade, r.release("broken"))
				if _, out, err := r.run(request); outcomeOf(err) != outcomeRolledBack {
					t.Fatalf("the next upgrade, whose daemon doesn't answer: %v\n%s", err, out)
				}
				r.wantReleaseBack(one, units)
				return
			}
			if out, err := r.recover(); err != nil {
				t.Fatalf("recover: %v\n%s", err, out)
			}
			if ended := r.readOutcome(); ended.Outcome != outcomeRolledBack {
				t.Errorf("the next start said %+v over rolledBack", ended)
			}
			if !r.journalGone() {
				t.Error("the journal outlived the next start")
			}
		})
	}
}

// The owner's one command lands its bundle over a journal the disk damaged, with the bad release
// current: a record nothing can read, or a recovery binary that is gone or can't run. Stopped before
// its switch it changes nothing, the damaged journal included.
func TestTheOwnersUpgradeLandsOverADamagedJournal(t *testing.T) {
	for name, damage := range map[string]func(r *installRig){
		"the record is cut short": func(r *installRig) { os.WriteFile(journalRecord(r.data), []byte(`{"phase": "swi`), 0o600) },
		"the record is empty":     func(r *installRig) { os.WriteFile(journalRecord(r.data), []byte(`{}`), 0o600) },
		"the record is gone":      func(r *installRig) { os.Remove(journalRecord(r.data)) },
		"the journal is empty": func(r *installRig) {
			os.Remove(journalRecord(r.data))
			os.Remove(journalBinary(r.data))
		},
		"the helper is gone": func(r *installRig) { os.Remove(journalBinary(r.data)) },
		"the helper can't run": func(r *installRig) {
			os.WriteFile(journalBinary(r.data), []byte("not a program"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			r.broken()
			bad := currentRelease(r.data)
			damage(r)
			plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("three")))
			if err != nil {
				t.Fatal(err)
			}
			// A term in each step before the switch.
			for at := range plan.boundary {
				plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("three")))
				if err != nil {
					t.Fatal(err)
				}
				ctx, term := context.WithCancel(context.Background())
				landsIn := plan.steps[at]
				plan.steps[at].do = func(ctx context.Context) error {
					err := landsIn.do(ctx)
					term()
					return err
				}
				before := r.tree()
				var out strings.Builder
				if err := applyLifecycle(ctx, plan, &out); outcomeOf(err) != outcomeRefused {
					t.Errorf("stopped before its switch: %v\n%s", err, out.String())
				}
				after := r.tree()
				if _, err := readRecord(r.data); err == nil {
					// A record that reads keeps its journal, whose damaged helper the upgrade put right.
					delete(before, journalBinary(r.data))
					delete(after, journalBinary(r.data))
				}
				if !sameTree(before, after) {
					t.Errorf("an upgrade stopped after %q changed a file", landsIn.say)
				}
			}
			if currentRelease(r.data) != bad {
				t.Fatal("the bad release isn't current, so this proves nothing")
			}
			three := r.request(Upgrade, r.release("three"))
			three.Outcome = r.outcomeFile()
			r.mustRun(three)
			if got := currentRelease(r.data); got != r.releaseOf("three") {
				t.Errorf("release %s is current, want three", got)
			}
			if ended := r.readOutcome(); ended.Outcome != outcomeLanded || !r.journalGone() {
				t.Errorf("the upgrade said %+v, its journal gone %v", ended, r.journalGone())
			}
			before := r.tree()
			if out, err := r.recover(); err != nil || !sameTree(before, r.tree()) {
				t.Errorf("the next start: %v\n%s", err, out)
			}
		})
	}
}

// With the record unreadable and the bad release's daemon gone too, nothing is installed to upgrade,
// and the same bundle's install lands.
func TestTheOwnersInstallLandsOverADamagedJournalWithNoDaemon(t *testing.T) {
	r := newInstallRig(t)
	r.broken()
	os.Remove(filepath.Join(r.data, "current", bundleDaemon))
	os.WriteFile(journalRecord(r.data), []byte("{"), 0o600)
	r.mustRun(r.request(Install, r.release("three")))
	if got := currentRelease(r.data); got != r.releaseOf("three") || !r.journalGone() {
		t.Errorf("release %s is current, the journal gone %v", got, r.journalGone())
	}
}

// storeSlot stores one slot in state.db, with the selection its daemon last gave it.
func (r *installRig) storeSlot(exit domain.Exit, selection domain.Selection) {
	r.t.Helper()
	ctx := context.Background()
	st, q, err := openStore(ctx, r.data)
	if err != nil {
		r.t.Fatal(err)
	}
	defer st.close()
	defer q.close()
	intent, err := domain.NewIntent("EU", exit)
	if err != nil {
		r.t.Fatal(err)
	}
	allocation, err := domain.Allocate(nil, domain.SlotKeys{})
	if err != nil {
		r.t.Fatal(err)
	}
	row := slotRow{Slot: domain.Slot{ID: "eu", Version: 1, Intent: intent, Allocation: allocation}, Selection: selection}
	tx := transaction{Slots: []slotRow{row}}
	if id, ok := exit.Server(); ok {
		tx.Lineages = []domain.Lineage{lineage(r.t, string(id), "aurora", "NL", "a.example")}
		url := domain.SecretRef("source/aurora/url")
		tx.Sources = []domain.Source{{ID: "aurora", Revision: 1, Name: "Aurora", Origin: domain.RemoteOrigin(domain.Remote{Location: url, UserAgent: "x", RefreshEvery: time.Hour})}}
		tx.Secrets = map[domain.SecretRef]domain.Secret{url: domain.NewSecret("https://aurora.example/source")}
	}
	if err := st.commit(ctx, tx); err != nil {
		r.t.Fatal(err)
	}
}

// Whether the new core must run is decided when the upgrade is planned, by what ran then and what the
// stored slots and sources ask for, not by whether the old core lived at the moment the upgrade looked.
// The old core dies just before that. A core that served and doesn't come up, or exits at once, rolls
// the upgrade back onto the old core. One the daemon then starts lands, and one nothing asks for is only
// checked.
func TestACoreTheConfigurationNeedsMustComeUp(t *testing.T) {
	closed := domain.Closed(domain.NoUsableServer)
	for name, c := range map[string]struct {
		exit      domain.Exit
		selection domain.Selection
		core      string
		starts    bool
		landed    bool
	}{
		"a slot off the VPN, and its core exits at once":    {domain.BlockExit(), domain.Blocked(), "core two dies", true, false},
		"a probe route of a closed slot, and no core comes": {domain.ServerExit("a"), closed, "core two", false, false},
		"a slot off the VPN, and the daemon starts it":      {domain.BlockExit(), domain.Blocked(), "core two", true, true},
		"a source Exit with no servers asks for none":       {domain.SourceExit("aurora"), closed, "core two", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			r.mustRun(r.request(Install, r.release("one")))
			one := currentRelease(r.data)
			r.storeSlot(c.exit, c.selection)
			r.runCore()
			oldCore := r.coreExe()
			r.coreBytes = []byte(c.core)
			r.pinCore()
			request := r.request(Upgrade, r.release("two"))
			request.steady.limit = 3 * time.Second
			plan, err := PlanLifecycle(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			at := stepAt(t, plan, "restart the core")
			check := plan.steps[at].do
			plan.steps[at].do = func(ctx context.Context) error {
				os.Remove(filepath.Join(r.root, "core-pid"))
				started := make(chan struct{})
				go func() {
					defer close(started)
					if c.starts {
						time.Sleep(50 * time.Millisecond)
						runTool(ctx, "systemctl", "start", gateway.CoreUnitName)
					}
				}()
				err := check(ctx)
				<-started
				return err
			}
			var out strings.Builder
			err = applyLifecycle(context.Background(), plan, &out)
			if c.landed {
				if err != nil || currentRelease(r.data) != r.releaseOf("two") {
					t.Errorf("release %s is current: %v\n%s", currentRelease(r.data), err, out.String())
				}
				return
			}
			if outcomeOf(err) != outcomeRolledBack || !strings.Contains(err.Error(), "didn't stay up") || currentRelease(r.data) != one {
				t.Errorf("release %s is current: %v\n%s", currentRelease(r.data), err, out.String())
			}
			if r.coreExe() != oldCore {
				t.Errorf("the core runs %q, not the old release's %s", r.coreExe(), oldCore)
			}
		})
	}
}

// An upgrade run again after a cut that left its release current judges its core against the release
// it puts back, so a dead core doesn't pass for one the release already ran.
func TestARetryJudgesTheCoreTheCutUpgradeDidnt(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	r.storeSlot(domain.BlockExit(), domain.Blocked())
	r.runCore()
	r.coreBytes = []byte("core two dies")
	r.pinCore()
	two := r.release("two")
	r.cutAfter(r.request(Upgrade, two), "wait for release")
	os.Remove(filepath.Join(r.root, "core-pid"))
	if _, out, err := r.run(r.request(Upgrade, two)); outcomeOf(err) != outcomeRolledBack || currentRelease(r.data) != one {
		t.Errorf("release %s is current: %v\n%s", currentRelease(r.data), err, out)
	}
}

// coreRuns makes the core's process run the binary core names, or one that can't be read.
func (r *installRig) coreRuns(core string) {
	r.t.Helper()
	exe := filepath.Join(r.root, "proc", "4242", "exe")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.Remove(exe)
	if core != "" {
		if err := os.Symlink(core, exe); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.root, "core-pid"), []byte("4242"), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *installRig) coreDies() { os.Remove(filepath.Join(r.root, "core-pid")) }

// during runs meanwhile once the plan's step whose words start with say is done.
func during(t *testing.T, plan LifecyclePlan, say string, meanwhile func()) {
	t.Helper()
	at := stepAt(t, plan, say)
	step := plan.steps[at]
	plan.steps[at].do = func(ctx context.Context) error {
		err := step.do(ctx)
		meanwhile()
		return err
	}
}

// A core that served before a switch was cut short must serve after the retry, whatever the core the
// cut left does while the retry is planned: it runs the retry's own core for a moment, or its process
// is on its way out and its executable is gone. It dies before the retry keeps its release.
func TestARetryOwesTheCoreTheCutSwitchOwed(t *testing.T) {
	for name, at := range map[string]func(r *installRig, core string){
		"the new core runs":                func(r *installRig, core string) { r.coreRuns(core) },
		"its process's executable is gone": func(r *installRig, _ string) { r.coreRuns("") },
		"none runs":                        func(r *installRig, _ string) { r.coreDies() },
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			r.mustRun(r.request(Install, r.release("one")))
			one := currentRelease(r.data)
			r.storeSlot(domain.BlockExit(), domain.Blocked())
			r.runCore()
			oldCore := r.coreExe()
			r.coreBytes = []byte("core two")
			r.pinCore()
			two := r.release("two")
			r.cutAfter(r.request(Upgrade, two), "wait for release")
			core, err := filepath.EvalSymlinks(filepath.Join(r.data, "current", bundleCore))
			if err != nil || core == oldCore {
				t.Fatalf("current's core is %s, %v, so this proves nothing", core, err)
			}
			at(r, core)
			plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, two))
			if err != nil {
				t.Fatal(err)
			}
			during(t, plan, "wait for release", r.coreDies)
			var out strings.Builder
			err = applyLifecycle(context.Background(), plan, &out)
			if outcomeOf(err) != outcomeRolledBack || currentRelease(r.data) != one {
				t.Errorf("a retry whose core died: release %s is current, want %s: %v\n%s", currentRelease(r.data), one, err, out.String())
			}
			if r.coreExe() != oldCore {
				t.Errorf("the core runs %q, not the old release's %s", r.coreExe(), oldCore)
			}
		})
	}
}

// A core that was down before the upgrade vetoes nothing: the one server the stored slot has is one
// both cores refuse, so the daemon stops the core, or runs it for a moment until the refusal. The
// upgrade lands, over a cut one too, and a cut one goes back.
func TestACoreThatWasDownVetoesNothing(t *testing.T) {
	arrange := func(t *testing.T) (*installRig, string) {
		r := newInstallRig(t)
		r.mustRun(r.request(Install, r.release("one")))
		r.storeSlot(domain.ServerExit("a"), domain.Closed(domain.NoUsableServer))
		r.coreBytes = []byte("core two")
		r.pinCore()
		return r, currentRelease(r.data)
	}
	runsForAMoment := func(r *installRig) {
		core, err := filepath.EvalSymlinks(filepath.Join(r.data, "current", bundleCore))
		if err != nil {
			r.t.Fatal(err)
		}
		r.coreRuns(core)
		os.WriteFile(filepath.Join(r.root, "core-flaps"), nil, 0o644)
	}
	for name, core := range map[string]func(r *installRig){
		"and stays down":        func(*installRig) {},
		"and runs for a moment": runsForAMoment,
	} {
		t.Run(name, func(t *testing.T) {
			for _, cut := range []string{"", "record release", "wait for release"} {
				r, _ := arrange(t)
				if cut != "" {
					r.cutAfter(r.request(Upgrade, r.release("two")), cut)
				}
				// Cut before its switch, the retry is planned as a first upgrade is, over a core that is down.
				if cut == "wait for release" {
					core(r)
				}
				plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("two")))
				if err != nil {
					t.Fatal(err)
				}
				during(t, plan, "wait for release", func() { core(r) })
				var out strings.Builder
				if err := applyLifecycle(context.Background(), plan, &out); err != nil || currentRelease(r.data) != r.releaseOf("two") {
					t.Errorf("cut %q: release %s is current: %v\n%s", cut, currentRelease(r.data), err, out.String())
				}
			}
			r, one := arrange(t)
			request := r.request(Upgrade, r.release("two"))
			request.Outcome = r.outcomeFile()
			r.cutAfter(request, "wait for release")
			core(r)
			if out, err := r.recover(); err != nil {
				t.Errorf("recover: %v\n%s", err, out)
			}
			r.wantPutBack(one, "", nil)
		})
	}
}

// A core that served must serve after an upgrade that keeps its binary too.
func TestAServingCoreTheNewDaemonLosesRollsBack(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	r.storeSlot(domain.BlockExit(), domain.Blocked())
	r.runCore()
	plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("two")))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RestartsCore {
		t.Fatal("the plan restarts the core, so this proves nothing")
	}
	during(t, plan, "wait for release", r.coreDies)
	var out strings.Builder
	if err := applyLifecycle(context.Background(), plan, &out); outcomeOf(err) != outcomeRolledBack || currentRelease(r.data) != one {
		t.Errorf("release %s is current: %v\n%s", currentRelease(r.data), err, out.String())
	}
	if _, live, _ := coreProcess(context.Background(), filepath.Join(r.root, "proc")); live == coreStopped {
		t.Error("the rollback left the core down")
	}
}

// Planning reads the stored configuration and changes nothing.
func TestPlanningTheCoreRequirementChangesNothing(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	r.storeSlot(domain.BlockExit(), domain.Blocked())
	r.runCore()
	before := r.tree()
	if _, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("two"))); err != nil || !sameTree(before, r.tree()) {
		t.Errorf("planning changed a file: %v", err)
	}
}

// A recovery cut once all of it is back, and before systemd first starts the old release's daemon, has
// seen nothing fail: a pre-start writes nothing, however often it runs, and whatever started before the
// put-back. Only a start of the put-back release that says nothing is a failure.
func TestARestoredReleaseFailsOnlyOnceItWasStarted(t *testing.T) {
	r := newInstallRig(t)
	one, units := r.broken()
	r.started()
	if out, err := r.recover(); err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	before := r.tree()
	for range 2 {
		if out, err := r.recover(); err != nil {
			t.Fatalf("recover before the first start: %v\n%s", err, out)
		}
		r.wantUnsaid()
	}
	if !sameTree(before, r.tree()) {
		t.Error("a pre-start with only the answer owed changed a file")
	}
	r.started()
	if out, err := r.recover(); err != nil {
		t.Fatalf("recover after the first start: %v\n%s", err, out)
	}
	if ended := r.readOutcome(); ended.Outcome != outcomePutBackFailed || !strings.Contains(*ended.Message, "was started, but hasn't said it answers") {
		t.Errorf("a release started and silent: %+v", ended)
	}
	r.wantPutBack(one, "", units)
}

// A recovery cut after it restarted the old core, and before that core had stayed up, owes the wait
// again: with the old core exiting over and over, no run says all of it is back.
func TestARecoveryCutInItsWaitForTheCoreOwesIt(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	units := r.earlierUnits()
	r.storeSlot(domain.BlockExit(), domain.Blocked())
	r.runCore()
	oldCore := r.coreExe()
	r.coreBytes = []byte("core two")
	r.pinCore()
	request := r.request(Upgrade, r.release("two"))
	request.Outcome = r.outcomeFile()
	r.cutAfter(request, "restart the core")

	waiting := r.request(Recover, "")
	waiting.steady = steadiness{settle: time.Minute, limit: time.Minute}
	plan, err := PlanLifecycle(context.Background(), waiting)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cut := context.WithCancel(context.Background())
	go func() {
		for r.coreExe() != oldCore {
			time.Sleep(time.Millisecond)
		}
		cut()
	}()
	if err := applyLifecycle(ctx, plan, io.Discard); err == nil || r.coreExe() != oldCore {
		t.Fatalf("the recovery wasn't cut in its wait: %v, so this proves nothing", err)
	}
	os.Remove(r.outcomeFile())

	flaps := filepath.Join(r.root, "core-flaps")
	os.WriteFile(flaps, nil, 0o644)
	if out, err := r.recover(); err == nil {
		t.Errorf("recover says a core that keeps exiting is back:\n%s", out)
	}
	if err := confirmPutBack(r.data, r.lockPath(), &one); err != nil {
		t.Fatal(err)
	}
	if ended := r.readOutcome(); ended.Outcome != outcomePutBackFailed || r.journalGone() {
		t.Errorf("over a core that keeps exiting it said %+v, its journal gone %v", ended, r.journalGone())
	}
	os.Remove(flaps)
	if out, err := r.recover(); err != nil {
		t.Fatalf("recover once the core stays up: %v\n%s", err, out)
	}
	r.wantPutBack(one, oldCore, units)
}

// hideRecord makes every read of the record fail until the returned func puts it back, as a disk that
// answers with I/O errors for a while does.
func (r *installRig) hideRecord() func() {
	r.t.Helper()
	hidden := filepath.Join(r.root, "hidden-record")
	if err := errors.Join(os.Rename(journalRecord(r.data), hidden), os.Mkdir(journalRecord(r.data), 0o700)); err != nil {
		r.t.Fatal(err)
	}
	return func() {
		r.t.Helper()
		if err := errors.Join(os.Remove(journalRecord(r.data)), os.Rename(hidden, journalRecord(r.data))); err != nil {
			r.t.Fatal(err)
		}
	}
}

// While the record can't be read, the owner's next upgrade is refused and changes nothing, so it never
// goes past the only record that names the release that last answered. Cut or not, that release comes
// back once the record reads again.
func TestAnUpgradeOverARecordItCantReadIsRefused(t *testing.T) {
	r := newInstallRig(t)
	one, units := r.broken()
	show := r.hideRecord()
	request := r.request(Upgrade, r.release("three"))
	before := r.tree()
	r.clearCalls()
	if err := RunLifecycle(context.Background(), request); outcomeOf(err) != outcomeRefused {
		t.Errorf("an upgrade over a record it can't read: %v", err)
	}
	if change := r.changed(before); change != "" {
		t.Errorf("a refusal changed the host: %s", change)
	}
	show()
	if out, err := r.recover(); err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	r.wantPutBack(one, "", units)
}

// A journal an earlier command set aside, and a kill left there, never comes back: not over the journal
// of a later switch, and not after a release has landed. The later switch is cut once journaled, its
// record then can't be read or doesn't parse, and the retry is stopped before its first step.
func TestAJournalSetAsideNeverComesBack(t *testing.T) {
	for name, harm := range map[string]func(r *installRig) (undo func()){
		"the record can't be read": (*installRig).hideRecord,
		"the record doesn't parse": func(r *installRig) func() {
			raw, _ := os.ReadFile(journalRecord(r.data))
			os.WriteFile(journalRecord(r.data), []byte("{"), 0o600)
			return func() { os.WriteFile(journalRecord(r.data), raw, 0o600) }
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			r.mustRun(r.request(Install, r.release("one")))
			two := r.release("two")
			r.cutAfter(r.request(Upgrade, two), "make release")
			stale := filepath.Join(r.root, "stale")
			if err := os.CopyFS(stale, os.DirFS(journalDir(r.data))); err != nil {
				t.Fatal(err)
			}
			r.mustRun(r.request(Upgrade, two))
			// As a kill after the journal's last rename, and before the one set aside is deleted, leaves it.
			if err := os.Rename(stale, journalDir(r.data)+".unread"); err != nil {
				t.Fatal(err)
			}
			r.cutAfter(r.request(Upgrade, r.release("three")), "record release")
			undo := harm(r)
			if plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("three"))); err == nil {
				ctx, term := context.WithCancel(context.Background())
				term()
				applyLifecycle(ctx, plan, io.Discard)
			}
			// The next start, which can make nothing of the harmed record, and one once it reads again.
			r.recover()
			if got := currentRelease(r.data); got != r.releaseOf("two") {
				t.Fatalf("release %s is current, want %s, which landed", got, r.releaseOf("two"))
			}
			undo()
			if out, err := r.recover(); err != nil || currentRelease(r.data) != r.releaseOf("two") {
				t.Errorf("release %s is current, want %s, which landed: %v\n%s", currentRelease(r.data), r.releaseOf("two"), err, out)
			}
			r.mustRun(r.request(Upgrade, r.release("three")))
			if entries, _ := filepath.Glob(journalDir(r.data) + "*"); len(entries) > 0 {
				t.Errorf("a landed upgrade leaves %v", entries)
			}
		})
	}
}
