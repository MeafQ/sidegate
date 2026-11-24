package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gateway "sidegate/internal/runtime"
)

// cutAfter runs request's steps up to and with the first whose words start with say, as a kill
// right after it would leave them, and returns the plan.
func (r *installRig) cutAfter(request LifecycleRequest, say string) LifecyclePlan {
	r.t.Helper()
	plan, err := PlanLifecycle(context.Background(), request)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range plan.steps[:stepAt(r.t, plan, say)+1] {
		if err := s.do(context.Background()); err != nil {
			r.t.Fatal(err)
		}
	}
	return plan
}

// recover runs what the daemon unit's pre-start runs.
func (r *installRig) recover() (string, error) {
	r.t.Helper()
	_, out, err := r.run(r.request(Recover, ""))
	return out, err
}

// broken is an upgrade from one to broken with --outcome, cut short once broken is current and its
// daemon restarted: the state a kill or a reboot leaves while the bad daemon owns current.
func (r *installRig) broken() (one string, units map[string]string) {
	r.t.Helper()
	r.mustRun(r.request(Install, r.release("one")))
	one = currentRelease(r.data)
	units = r.earlierUnits()
	request := r.request(Upgrade, r.release("broken"))
	request.Outcome = r.outcomeFile()
	r.cutAfter(request, "restart "+daemonUnitName)
	if currentRelease(r.data) == one {
		r.t.Fatal("broken isn't current, so this proves nothing")
	}
	return one, units
}

func (r *installRig) journalGone() bool {
	_, err := os.Stat(journalDir(r.data))
	return errors.Is(err, os.ErrNotExist)
}

// started runs the daemon unit's main process, whose daemon is none here and fails at once.
func (r *installRig) started() {
	argv := unitCommand(r.t, daemonUnit(r.data), "ExecStart")
	exec.Command(argv[0], argv[1:]...).Run()
}

// The unit's pre-start runs the journal's own sidegated when there is a journal, and nothing when
// there isn't. Its main process becomes current's daemon, and marks a journal that is there. The unit
// starts when either binary is there. All of it holds whatever characters the data dir has.
func TestTheDaemonUnitRunsTheJournalsRecoveryBeforeEachStart(t *testing.T) {
	logs := t.TempDir()
	ran, served := filepath.Join(logs, "ran"), filepath.Join(logs, "served")
	for _, name := range append([]string{"data"}, carriedNames...) {
		t.Run(name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), name, "sidegate")
			unit := daemonUnit(dataDir)
			wantPaths := []string{filepath.Join(dataDir, "current", bundleDaemon), journalBinary(dataDir)}
			var paths []string
			for _, value := range unitValues(unit, "ConditionPathExists") {
				path, triggers := strings.CutPrefix(value, "|")
				if !triggers {
					t.Errorf("the unit needs %s to start", path)
				}
				paths = append(paths, strings.ReplaceAll(path, "%%", "%"))
			}
			if !slices.Equal(paths, wantPaths) {
				t.Errorf("the unit starts when one of %q is there, want %q", paths, wantPaths)
			}
			if pre := unitValues(unit, "ExecStartPre"); len(pre) != 1 || !strings.HasPrefix(strings.TrimLeft(pre[0], `"`), "-") {
				t.Fatalf("the pre-starts are %q, want one whose failure is ignored", pre)
			}
			current := filepath.Join(dataDir, "current", bundleDaemon)
			if err := os.MkdirAll(filepath.Dir(current), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(current, []byte("#!/bin/sh\nprintf '%s\\n' $$ \"$@\" >"+served+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			// start runs the main process and says whether it became the daemon.
			start := func() bool {
				os.Remove(served)
				argv := unitCommand(t, unit, "ExecStart")
				main := exec.Command(argv[0], argv[1:]...)
				if out, err := main.CombinedOutput(); err != nil {
					t.Errorf("the main process says %q: %v", out, err)
				}
				raw, _ := os.ReadFile(served)
				return main.Process != nil && string(raw) == fmt.Sprintf("%d\nserve\n--data-dir\n%s\n", main.Process.Pid, dataDir)
			}
			// preStart skips an executable that isn't there, as systemd does with its failure ignored.
			preStart := func() string {
				os.Remove(ran)
				argv := unitCommand(t, unit, "ExecStartPre")
				if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil && !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the pre-start says %q: %v", out, err)
				}
				raw, _ := os.ReadFile(ran)
				return string(raw)
			}
			if got := preStart(); got != "" {
				t.Errorf("with no journal the pre-start ran %q", got)
			}
			if !start() {
				t.Error("with no journal the main process didn't become the daemon")
			}
			if _, err := os.Stat(journalDir(dataDir)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("with no journal the unit made one: %v", err)
			}
			if err := os.MkdirAll(journalDir(dataDir), 0o700); err != nil {
				t.Fatal(err)
			}
			stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" >" + ran + "\n"
			if err := os.WriteFile(journalBinary(dataDir), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			if got := preStart(); got != "recover\n--data-dir\n"+dataDir+"\n--yes\n" {
				t.Errorf("the journal's sidegated ran with %q", got)
			}
			// A mark that can't be made doesn't keep the daemon from starting.
			os.Chmod(journalDir(dataDir), 0o500)
			if !start() {
				t.Error("with a journal it can't mark, the main process didn't become the daemon")
			}
			os.Chmod(journalDir(dataDir), 0o700)
			if !start() {
				t.Error("with a journal the main process didn't become the daemon")
			}
			if entries, _ := os.ReadDir(journalDir(dataDir)); len(entries) != 2 {
				t.Errorf("the journal holds %v, want the recovery's sidegated and the mark", entries)
			}
		})
	}
}

// A kill or a reboot while the bad daemon owns current goes back at the next start: recover puts
// back current, the core link and both units, a unit the owner linked as a link, and reloads
// systemd. It never restarts or waits for the daemon it runs before. It doesn't say rolled back:
// the old release's daemon does, once it listens, and only that release's.
func TestRecoverPutsBackASwitchCutShort(t *testing.T) {
	for _, damage := range []string{"", "the candidate's daemon is gone", "the old release's daemon is broken"} {
		t.Run(damage, func(t *testing.T) {
			r := newInstallRig(t)
			owners := filepath.Join(r.root, "owners-core.service")
			os.WriteFile(owners, []byte("the owner's own core unit"), 0o644)
			r.mustRun(r.request(Install, r.release("one")))
			one := currentRelease(r.data)
			daemonText := "as release one wrote " + daemonUnitName
			os.WriteFile(filepath.Join(r.units, daemonUnitName), []byte(daemonText), 0o644)
			coreUnit := filepath.Join(r.units, gateway.CoreUnitName)
			os.Remove(coreUnit)
			os.Symlink(owners, coreUnit)
			request := r.request(Upgrade, r.release("broken"))
			request.Outcome = r.outcomeFile()
			r.cutAfter(request, "restart "+daemonUnitName)
			switch damage {
			case "the candidate's daemon is gone":
				os.Remove(filepath.Join(r.data, "current", bundleDaemon))
			case "the old release's daemon is broken":
				os.WriteFile(filepath.Join(releaseDir(r.data, one), bundleDaemon), []byte("broken"), 0o755)
			}
			r.clearCalls()

			if out, err := r.recover(); err != nil {
				t.Fatalf("recover: %v\n%s", err, out)
			}
			if got := currentRelease(r.data); got != one {
				t.Errorf("release %s is current, want %s back", got, one)
			}
			if raw, _ := os.ReadFile(filepath.Join(r.units, daemonUnitName)); string(raw) != daemonText {
				t.Errorf("%s holds %q", daemonUnitName, raw)
			}
			if target, err := os.Readlink(coreUnit); err != nil || target != owners {
				t.Errorf("the owner's linked core unit came back as %q: %v", target, err)
			}
			log := r.callLog()
			if !strings.Contains(log, "systemctl daemon-reload") {
				t.Errorf("recover didn't reload systemd:\n%s", log)
			}
			if strings.Contains(log, "sidegated.service") {
				t.Errorf("recover touched its own daemon:\n%s", log)
			}
			if _, err := os.Stat(r.outcomeFile()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("recover said how it ended before the old daemon answered: %v", err)
			}

			broken := r.releaseOf("broken")
			if err := confirmPutBack(r.data, r.lockPath(), &broken); err != nil || r.journalGone() {
				t.Errorf("the candidate's daemon confirmed the put-back: %v", err)
			}
			lock := r.holdLock()
			if err := confirmPutBack(r.data, r.lockPath(), &one); outcomeOf(err) != outcomeBusy || r.journalGone() {
				t.Errorf("a daemon confirmed the put-back under a lifecycle command: %v", err)
			}
			lock.Close()
			if damage == "the old release's daemon is broken" {
				r.started()
				if out, err := r.recover(); err != nil {
					t.Fatalf("recover at the broken release's next start: %v\n%s", err, out)
				}
				if ended := r.readOutcome(); ended.Outcome != outcomePutBackFailed || !strings.Contains(*ended.Message, "hasn't said it answers") {
					t.Errorf("a put-back release that starts again unconfirmed: %+v", ended)
				}
				before := r.tree()
				if out, err := r.recover(); err != nil || !sameTree(before, r.tree()) {
					t.Errorf("a third start changed something: %v\n%s", err, out)
				}
				// The owner's one command: the old release's own bundle, verified, run by its sidegated.
				r.mustRun(r.request(Upgrade, r.release("one")))
				if raw, _ := os.ReadFile(filepath.Join(r.data, "current", bundleDaemon)); string(raw) != "one" {
					t.Errorf("the restore from the bundle left the daemon %q", raw)
				}
				if !r.journalGone() {
					t.Error("the journal outlived the restore")
				}
				return
			}
			if err := confirmPutBack(r.data, r.lockPath(), &one); err != nil {
				t.Fatal(err)
			}
			if ended := r.readOutcome(); ended.Outcome != outcomeRolledBack || !strings.Contains(*ended.Message, "is back and answers") {
				t.Errorf("the old daemon said %+v", ended)
			}
			if !r.journalGone() {
				t.Error("the journal outlived the confirmed put-back")
			}
		})
	}
}

// While a lifecycle command holds the lock, its own restarts run the pre-start, which reads and
// changes nothing: the command owns its rollback.
func TestRecoverUnderALifecycleCommandDoesNothing(t *testing.T) {
	r := newInstallRig(t)
	r.broken()
	r.holdLock()
	before := r.tree()
	r.clearCalls()
	if _, err := r.recover(); outcomeOf(err) != outcomeBusy {
		t.Errorf("recover under the lock: %v", err)
	}
	if change := r.changed(before); change != "" {
		t.Error(change)
	}
}

// A switch is journaled before it begins, and recover restores only while current names one of its
// candidates. Cut short before its switch, the upgrade changed nothing: refused, and the journal
// goes. A journal cut short while it was written is a staging directory recover doesn't read, and
// the next upgrade lands.
func TestRecoverPutsBackOnlyWhatTheJournalSelected(t *testing.T) {
	t.Run("before the switch", func(t *testing.T) {
		r := newInstallRig(t)
		r.mustRun(r.request(Install, r.release("one")))
		one := currentRelease(r.data)
		request := r.request(Upgrade, r.release("two"))
		request.Outcome = r.outcomeFile()
		r.cutAfter(request, "record release")
		if record, err := readRecord(r.data); err != nil || record.candidate() != r.releaseOf("two") || record.prior(r.data) != one {
			t.Fatalf("the journal before the switch: %+v %v", record, err)
		}
		if raw, _ := os.ReadFile(journalBinary(r.data)); string(raw) != "the recovery sidegated" {
			t.Errorf("the journal carries %q, not the sidegated that wrote it", raw)
		}
		if out, err := r.recover(); err != nil {
			t.Fatalf("recover: %v\n%s", err, out)
		}
		if ended := r.readOutcome(); ended.Outcome != outcomeRefused || currentRelease(r.data) != one || !r.journalGone() {
			t.Errorf("an upgrade cut short before its switch: %+v, current %s", ended, currentRelease(r.data))
		}
	})
	t.Run("a journal cut short while written", func(t *testing.T) {
		r := newInstallRig(t)
		r.mustRun(r.request(Install, r.release("one")))
		one := currentRelease(r.data)
		os.MkdirAll(journalDir(r.data)+".new", 0o700)
		os.WriteFile(filepath.Join(journalDir(r.data)+".new", "record.json"), []byte(`{"phase": "swi`), 0o600)
		before := r.tree()
		if out, err := r.recover(); err != nil || !sameTree(before, r.tree()) || currentRelease(r.data) != one {
			t.Errorf("recover over a half-written journal: %v\n%s", err, out)
		}
		r.mustRun(r.request(Upgrade, r.release("two")))
		if _, err := os.Stat(journalDir(r.data) + ".new"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the half-written journal outlived the next upgrade: %v", err)
		}
	})
	t.Run("current names a release the journal never selected", func(t *testing.T) {
		r := newInstallRig(t)
		one, _ := r.broken()
		r.recover()
		// The owner deliberately makes two current by hand, while the put-back waits on one's answer.
		two := r.release("two")
		b, _ := inspectBundle(two)
		b.place(r.data, true)
		setCurrent(r.data, b.release())
		before := r.tree()
		if out, err := r.recover(); err != nil {
			t.Fatalf("recover: %v\n%s", err, out)
		}
		if currentRelease(r.data) != b.release() {
			t.Errorf("recover moved current from the owner's choice to %s (from %s)", currentRelease(r.data), one)
		}
		for path := range before {
			if _, kept := r.tree()[path]; !kept && !strings.HasPrefix(path, journalDir(r.data)) {
				t.Errorf("recover removed %s", path)
			}
		}
	})
}

// A deliberate SSH upgrade after a switch cut short keeps the journal's release to put back: if it
// lands, it is kept, the journal goes and the next start changes nothing. If it is cut short too,
// the next start puts back the release that last answered, not the first cut-short candidate.
func TestALaterDeliberateUpgradeAfterACutShortSwitch(t *testing.T) {
	for _, lands := range []bool{true, false} {
		r := newInstallRig(t)
		r.mustRun(r.request(Install, r.release("one")))
		one := currentRelease(r.data)
		r.cutAfter(r.request(Upgrade, r.release("two")), "wait for release")
		three := r.request(Upgrade, r.release("three"))
		if lands {
			r.mustRun(three)
			if !r.journalGone() {
				t.Fatal("the journal outlived a landed upgrade")
			}
			before := r.tree()
			if out, err := r.recover(); err != nil || !sameTree(before, r.tree()) {
				t.Errorf("the next start after a landed upgrade: %v\n%s", err, out)
			}
			if currentRelease(r.data) != r.releaseOf("three") {
				t.Errorf("release %s is current", currentRelease(r.data))
			}
			continue
		}
		r.cutAfter(three, "make release")
		if out, err := r.recover(); err != nil {
			t.Fatalf("recover: %v\n%s", err, out)
		}
		if currentRelease(r.data) != one {
			t.Errorf("after two cut-short upgrades, release %s is current, want %s", currentRelease(r.data), one)
		}
	}
}

// A kill after the core switch, the new core running and the record not yet kept, goes back at the
// next start onto the old core too, within the same boot. After a reboot no core runs, and the daemon
// starts the one it needs, so recover starts none.
func TestRecoverPutsTheCoreBackWithinTheBoot(t *testing.T) {
	for _, rebooted := range []bool{false, true} {
		r := newInstallRig(t)
		bootID := filepath.Join(r.root, "proc", "sys", "kernel", "random", "boot_id")
		os.MkdirAll(filepath.Dir(bootID), 0o755)
		os.WriteFile(bootID, []byte("boot one\n"), 0o644)
		r.mustRun(r.request(Install, r.release("one")))
		one := currentRelease(r.data)
		r.runCore()
		old := r.coreExe()
		r.coreBytes = []byte("core two")
		r.pinCore()
		r.cutAfter(r.request(Upgrade, r.release("two")), "restart the core")
		if r.coreExe() == old {
			t.Fatal("the core didn't switch, so this proves nothing")
		}
		if rebooted {
			os.WriteFile(bootID, []byte("boot two\n"), 0o644)
			os.Remove(filepath.Join(r.root, "core-pid"))
		}
		r.clearCalls()
		if out, err := r.recover(); err != nil {
			t.Fatalf("recover, rebooted %v: %v\n%s", rebooted, err, out)
		}
		if currentRelease(r.data) != one {
			t.Errorf("rebooted %v: release %s is current", rebooted, currentRelease(r.data))
		}
		switch {
		case rebooted && strings.Contains(r.callLog(), "start sidegate-core.service"):
			t.Errorf("recover after a reboot started the core:\n%s", r.callLog())
		case !rebooted && r.coreExe() != old:
			t.Errorf("the core runs %s, not the old release's %s", r.coreExe(), old)
		}
	}
}

// An install over no release journals what current named, so a kill after its switch goes back to
// no release at the next start, and its own failure stops the unready daemon it started.
func TestAnInstallCutShortGoesBackToNoRelease(t *testing.T) {
	r := newInstallRig(t)
	r.cutAfter(r.request(Install, r.release("one")), "enable")
	if currentRelease(r.data) == "" {
		t.Fatal("one isn't current, so this proves nothing")
	}
	if out, err := r.recover(); err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(r.data, "current")); !errors.Is(err, os.ErrNotExist) || !r.journalGone() {
		t.Errorf("after a cut-short first install, current: %v, journal gone: %v", err, r.journalGone())
	}

	r.clearCalls()
	if _, _, err := r.run(r.request(Install, r.release("broken"))); err == nil {
		t.Fatal("an install whose daemon doesn't answer passed")
	}
	if r.runningDaemon() != "" || !strings.Contains(r.callLog(), "systemctl stop sidegated.service") {
		t.Errorf("the failed install left its daemon %q running:\n%s", r.runningDaemon(), r.callLog())
	}
	if !r.journalGone() {
		t.Error("the journal outlived the failed install")
	}
}

// The wait for an answer wants the release it switched to: the old daemon still answering is not
// the new one answering.
func TestAnAnswerFromAnotherReleaseIsNoAnswer(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	request := r.request(Upgrade, r.release("two"))
	plan, err := PlanLifecycle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	at := stepAt(t, plan, "restart "+daemonUnitName)
	plan.steps[at].do = func(context.Context) error { return nil }
	var out strings.Builder
	err = applyLifecycle(context.Background(), plan, &out)
	if outcomeOf(err) != outcomeRolledBack || !strings.Contains(err.Error(), "release "+one+" answers, not "+r.releaseOf("two")) {
		t.Errorf("an upgrade the old daemon answered for: %v\n%s", err, out.String())
	}
}

// An upgrade run again after a switch cut short puts its release right past the boundary, its core
// link too, and one that then fails ends with the release that last answered current.
func TestARetryAfterACutShortSwitchGoesBackToTheReleaseThatAnswered(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	broken := r.release("broken")
	r.cutAfter(r.request(Upgrade, broken), "make release")
	r.pointCoreElsewhere()
	if _, out, err := r.run(r.request(Upgrade, broken)); outcomeOf(err) != outcomeRolledBack {
		t.Fatalf("the retry: %v\n%s", err, out)
	}
	if currentRelease(r.data) != one {
		t.Errorf("release %s is current, want %s", currentRelease(r.data), one)
	}
}
