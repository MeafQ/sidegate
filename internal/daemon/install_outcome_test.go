package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	gateway "sidegate/internal/runtime"
)

func (r *installRig) holdLock() *os.File {
	r.t.Helper()
	lock, err := holdLifecycle(r.lockPath(), 0)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { lock.Close() })
	return lock
}

func (r *installRig) lockIsFree() bool {
	r.t.Helper()
	lock, err := holdLifecycle(r.lockPath(), 0)
	if outcomeOf(err) == outcomeBusy {
		return false
	}
	if err != nil {
		r.t.Fatal(err)
	}
	lock.Close()
	return true
}

// changed says what a lifecycle command did to the host: the files it moved, and the systemctl
// and nginx calls that change something. systemctl show only reads.
func (r *installRig) changed(before map[string]string) string {
	r.t.Helper()
	var calls []string
	for _, call := range strings.Split(strings.TrimSpace(r.callLog()), "\n") {
		if call != "" && !strings.HasPrefix(call, "systemctl show") {
			calls = append(calls, call)
		}
	}
	if sameTree(before, r.tree()) && len(calls) == 0 {
		return ""
	}
	return "files changed or calls made: " + strings.Join(calls, "; ")
}

type endedAs struct {
	Outcome outcome `json:"outcome"`
	Message *string `json:"message"`
}

func (r *installRig) outcomeFile() string { return filepath.Join(r.root, "outcome.json") }

func (r *installRig) readOutcome() endedAs {
	r.t.Helper()
	raw, err := os.ReadFile(r.outcomeFile())
	if err != nil {
		r.t.Fatal(err)
	}
	var ended endedAs
	if err := json.Unmarshal(raw, &ended); err != nil {
		r.t.Fatalf("%s: %v", raw, err)
	}
	return ended
}

// An SSH upgrade, an install and an uninstall can't run at once: while one holds the lock, each
// other refuses as busy before it plans, so it reads and changes nothing.
func TestNoTwoLifecycleCommandsRunAtOnce(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	two := r.release("two")
	lock := r.holdLock()
	before := r.tree()
	r.clearCalls()
	for _, request := range []LifecycleRequest{r.request(Install, r.release("one")), r.request(Upgrade, two), r.request(Uninstall, "")} {
		if err := RunLifecycle(context.Background(), request); outcomeOf(err) != outcomeBusy {
			t.Errorf("%s while the lock is held: %v", request.Action, err)
		}
	}
	if r.callLog() != "" {
		t.Errorf("a busy command planned:\n%s", r.callLog())
	}
	if change := r.changed(before); change != "" {
		t.Error(change)
	}
	lock.Close()
	r.mustRun(r.request(Upgrade, two))
}

// The updater holds the lock and hands its upgrade a copy. Each copy it hands lets a plan run,
// closing a copy releases nothing, and a command without one is still busy.
func TestTwoPlansUnderOneLock(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	lock := r.holdLock()
	for _, daemon := range []string{"two", "three"} {
		request := r.request(Upgrade, r.release(daemon))
		if err := RunLifecycle(context.Background(), request); outcomeOf(err) != outcomeBusy {
			t.Errorf("upgrade to %s with no copy of the held lock: %v", daemon, err)
		}
		copied, err := syscall.Dup(int(lock.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		request.LockFD = copied
		if err := RunLifecycle(context.Background(), request); err != nil {
			t.Fatalf("upgrade to %s under the caller's lock: %v", daemon, err)
		}
		if r.lockIsFree() {
			t.Fatalf("the upgrade to %s closed its copy and released the caller's lock", daemon)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(r.data, "current", bundleDaemon)); string(raw) != "three" {
		t.Errorf("current daemon %q", raw)
	}

	other, err := os.Create(filepath.Join(r.root, "not-the-lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	request := r.request(Upgrade, r.release("four"))
	if request.LockFD, err = syscall.Dup(int(other.Fd())); err != nil {
		t.Fatal(err)
	}
	before := r.tree()
	if err := RunLifecycle(context.Background(), request); err == nil || outcomeOf(err) != outcomeRefused {
		t.Errorf("upgrade handed another file as the lock: %v", err)
	}
	if !sameTree(before, r.tree()) {
		t.Error("an upgrade handed another file as the lock changed a file")
	}
}

// The updater's child gets the lock on fd 3. Either may close its copy first and the lock stays
// held, and a tool the child leaves running doesn't hold it once both are gone.
func TestTheLockOnFd3OutlivesEitherCopyAndNoneElse(t *testing.T) {
	r := newInstallRig(t)
	lock := r.holdLock()
	child := exec.Command(os.Args[0], "-test.run=^TestLockChild$")
	child.Env = append(os.Environ(), "LOCK_CHILD_PATH="+r.lockPath())
	child.ExtraFiles = []*os.File{lock}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	sleeper, perr := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "sleeper "))
	if err != nil || perr != nil {
		child.Process.Kill()
		t.Fatalf("the child said %q: %v %v", line, err, perr)
	}
	t.Cleanup(func() { syscall.Kill(sleeper, syscall.SIGKILL) })

	lock.Close()
	if r.lockIsFree() {
		t.Error("closing the updater's copy released the child's")
	}
	stdin.Close()
	if err := child.Wait(); err != nil {
		t.Fatalf("the child: %v", err)
	}
	if syscall.Kill(sleeper, 0) != nil {
		t.Fatal("the child's tool is gone, so this test proves nothing")
	}
	if !r.lockIsFree() {
		t.Error("a tool the child left running holds the lock")
	}
}

// An uninstall leaves no lock file. A command that opened the file before the uninstall removed it
// doesn't hold the removed file: it holds the lock at the path, so a third command is busy.
func TestAnUninstallLeavesNoLockAndAnOpenedOneIsNoLock(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	if _, err := os.Stat(r.lockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the install left the lock file: %v", err)
	}
	uninstalled := false
	lockOpened = func() {
		if uninstalled {
			return
		}
		uninstalled = true
		r.mustRun(r.request(Uninstall, ""))
		if _, err := os.Stat(r.lockPath()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the uninstall left the lock file: %v", err)
		}
	}
	defer func() { lockOpened = func() {} }()
	second, err := holdLifecycle(r.lockPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if !uninstalled {
		t.Fatal("no uninstall ran while the second command had the file open, so this proves nothing")
	}
	if r.lockIsFree() {
		t.Error("a third command takes the lock while the second holds the file it opened before the uninstall")
	}
}

// TestLockChild is the upgrade child TestTheLockOnFd3OutlivesEitherCopyAndNoneElse starts. It takes
// the lock it was handed, leaves a tool running, and exits when its stdin closes.
func TestLockChild(t *testing.T) {
	path := os.Getenv("LOCK_CHILD_PATH")
	if path == "" {
		t.Skip("TestTheLockOnFd3OutlivesEitherCopyAndNoneElse runs this in a process of its own")
	}
	lock, err := holdLifecycle(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("sleeper " + strconv.Itoa(sleeper.Process.Pid) + "\n")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

// Every way an upgrade ends is written to --outcome, and each refusal ends it before its first
// step, with nothing changed.
func TestEachUpgradeOutcomeIsWritten(t *testing.T) {
	newCore := func(r *installRig) { r.coreBytes = []byte("core two"); r.pinCore() }
	yes, no := true, false
	for name, c := range map[string]struct {
		outcome outcome
		arrange func(r *installRig, request *LifecycleRequest)
		says    string
	}{
		"landed": {outcomeLanded, func(r *installRig, request *LifecycleRequest) {
			request.Expect, request.RestartsCore = r.releaseOf("one"), &no
		}, ""},
		"refused": {outcomeRefused, func(r *installRig, request *LifecycleRequest) {
			request.BundlePath = r.bundle("bad-core", map[string][]byte{bundleDaemon: []byte("d"), bundleCore: []byte("not pinned")})
		}, "this build pins"},
		"rolledBack": {outcomeRolledBack, func(r *installRig, request *LifecycleRequest) {
			request.BundlePath = r.release("broken")
		}, "is back and answers"},
		"putBackFailed": {outcomePutBackFailed, func(r *installRig, _ *LifecycleRequest) {
			os.WriteFile(filepath.Join(r.root, "restart-fails"), nil, 0o644)
		}, "back failed too"},
		"landedWithError": {outcomeLandedWithError, func(r *installRig, _ *LifecycleRequest) {
			if os.Geteuid() == 0 {
				r.t.Skip("root removes what a read-only directory holds")
			}
			stale := releaseDir(r.data, "stale")
			os.MkdirAll(stale, 0o700)
			os.WriteFile(filepath.Join(stale, bundleDaemon), nil, 0o600)
			os.Chmod(stale, 0o500)
			r.t.Cleanup(func() { os.Chmod(stale, 0o700) })
		}, "remove the releases and cores nothing uses now"},
		"busy": {outcomeBusy, func(r *installRig, _ *LifecycleRequest) { r.holdLock() }, "holds"},
		"moved, checked before the bundle is read": {outcomeMoved, func(r *installRig, request *LifecycleRequest) {
			request.Expect, request.BundlePath = "0123456789ab", filepath.Join(r.root, "missing.tar.gz")
		}, "is current, not 0123456789ab"},
		"moved, current names a release that is gone": {outcomeMoved, func(r *installRig, request *LifecycleRequest) {
			request.Expect, request.BundlePath = r.releaseOf("one"), filepath.Join(r.root, "missing.tar.gz")
			os.RemoveAll(releaseDir(r.data, r.releaseOf("one")))
		}, "no release is current"},
		"moved, current's daemon is gone": {outcomeMoved, func(r *installRig, request *LifecycleRequest) {
			request.Expect, request.BundlePath = r.releaseOf("one"), filepath.Join(r.root, "missing.tar.gz")
			os.Remove(filepath.Join(releaseDir(r.data, r.releaseOf("one")), bundleDaemon))
		}, "no release is current"},
		"planChanged, a core restart nobody confirmed": {outcomePlanChanged, func(r *installRig, request *LifecycleRequest) {
			newCore(r)
			request.BundlePath, request.RestartsCore = r.release("two"), &no
		}, "restarts the core now"},
		"planChanged, a core restart that no longer happens": {outcomePlanChanged, func(r *installRig, request *LifecycleRequest) {
			request.RestartsCore = &yes
		}, "keeps the core running now"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			r.mustRun(r.request(Install, r.release("one")))
			request := r.request(Upgrade, r.release("two"))
			request.Outcome = r.outcomeFile()
			c.arrange(r, &request)
			before := r.tree()
			r.clearCalls()
			err := RunLifecycle(context.Background(), request)
			ended := r.readOutcome()
			if ended.Outcome != c.outcome || outcomeOf(err) != c.outcome {
				t.Fatalf("wrote %q, want %q: %v", ended.Outcome, c.outcome, err)
			}
			switch {
			case c.outcome == outcomeLanded && ended.Message != nil:
				t.Errorf("a landing wrote the message %q", *ended.Message)
			case c.outcome != outcomeLanded && (ended.Message == nil || !strings.Contains(*ended.Message, c.says)):
				t.Errorf("the message doesn't say %q: %v", c.says, err)
			}
			if refusal := c.outcome == outcomeRefused || c.outcome == outcomeBusy || c.outcome == outcomeMoved || c.outcome == outcomePlanChanged; refusal {
				if change := r.changed(before); change != "" {
					t.Errorf("a refusal changed the host: %s", change)
				}
			}
		})
	}
}

// A SIGTERM while the switch runs cancels the step it lands in. The rollback still runs, so the old
// release is back, rather than neither.
func TestATermMidSwitchStillPutsTheOldReleaseBack(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	units := r.earlierUnits()
	plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("two")))
	if err != nil {
		t.Fatal(err)
	}
	ctx, term := context.WithCancel(context.Background())
	defer term()
	at := stepAt(t, plan, "restart "+daemonUnitName)
	restart := plan.steps[at]
	plan.steps[at].do = func(ctx context.Context) error {
		term()
		return restart.do(ctx)
	}
	var out strings.Builder
	err = applyLifecycle(ctx, plan, &out)
	if outcomeOf(err) != outcomeRolledBack || !errors.Is(err, context.Canceled) {
		t.Fatalf("an upgrade cancelled at the restart: %v\n%s", err, out.String())
	}
	r.wantReleaseBack(one, units)
}

// An upgrade run again after a kill that left its release current keeps that release's directory,
// as an install of the installed bundle does, so current never names a directory that is gone.
// A release whose daemon doesn't match is replaced.
func TestARetryKeepsTheReleaseCurrentNames(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	two := r.release("two")
	plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, two))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plan.steps[:stepAt(t, plan, "make release")+1] {
		if err := s.do(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	dir := releaseDir(r.data, r.releaseOf("two"))
	if currentRelease(r.data) != r.releaseOf("two") {
		t.Fatal("two isn't current, so this test proves nothing")
	}
	kept, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.mustRun(r.request(Upgrade, two))
	r.mustRun(r.request(Install, two))
	if now, err := os.Stat(dir); err != nil || !os.SameFile(kept, now) {
		t.Errorf("the release current names was replaced: %v", err)
	}

	os.WriteFile(filepath.Join(dir, bundleDaemon), []byte("damaged"), 0o755)
	r.mustRun(r.request(Install, two))
	if raw, _ := os.ReadFile(filepath.Join(r.data, "current", bundleDaemon)); string(raw) != "two" {
		t.Errorf("a damaged daemon stayed: %q", raw)
	}
}

// An outcome file that is the lock, by another spelling or a hard link, is refused before anything
// is written, so a busy upgrade can't put a new file where the lock another command holds was.
func TestAnOutcomeIsNeverTheLock(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	spelled := r.root + "/./" + filepath.Base(r.lockPath())
	linked := filepath.Join(r.root, "linked.lock")
	held := r.holdLock()
	if err := os.Link(r.lockPath(), linked); err != nil {
		t.Fatal(err)
	}
	was, err := os.Stat(r.lockPath())
	if err != nil {
		t.Fatal(err)
	}
	before := r.tree()
	for _, outcome := range []string{spelled, linked} {
		request := r.request(Upgrade, r.release("two"))
		request.Outcome = outcome
		if err := RunLifecycle(context.Background(), request); err == nil || outcomeOf(err) == outcomeBusy {
			t.Errorf("--outcome %s while the lock is held: %v", outcome, err)
		}
		if now, err := os.Stat(r.lockPath()); err != nil || !os.SameFile(was, now) {
			t.Errorf("--outcome %s replaced the lock: %v", outcome, err)
		}
		if r.lockIsFree() {
			t.Errorf("after --outcome %s, a third command takes the lock the first one holds", outcome)
		}
	}
	if !sameTree(before, r.tree()) {
		t.Error("a refused outcome path changed a file")
	}

	held.Close()
	os.Remove(linked)
	os.Remove(r.lockPath())
	request := r.request(Upgrade, r.release("two"))
	request.Outcome = spelled
	if err := RunLifecycle(context.Background(), request); err == nil {
		t.Error("--outcome naming a lock not made yet ran")
	}
	if _, err := os.Stat(r.lockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused outcome path left a file at the lock: %v", err)
	}
	if !sameTree(before, r.tree()) {
		t.Error("a refused outcome path changed a file")
	}
}

// liveAt is where the plan first changes what the gateway runs or selects: the release current
// names, or current.
func liveAt(plan LifecyclePlan) int {
	for i, s := range plan.steps {
		if strings.HasPrefix(s.say, "make release") || strings.HasPrefix(s.say, "put release") || strings.HasPrefix(s.say, "put core") {
			return i
		}
	}
	return len(plan.steps)
}

// pointCoreElsewhere points the current release's core link at another core.
func (r *installRig) pointCoreElsewhere() string {
	r.t.Helper()
	elsewhere := filepath.Join("..", "..", "cores", "elsewhere", bundleCore)
	if err := symlinkAtomic(elsewhere, filepath.Join(r.data, "current", bundleCore)); err != nil {
		r.t.Fatal(err)
	}
	return elsewhere
}

// A SIGTERM before the switch ends the upgrade as a refusal with nothing changed, whether it lands
// before the first step or while any step before the switch runs: no release is made current or
// put right, no unit written, and the daemon isn't restarted. That holds for a retry of the bundle
// current names too, whose core link points elsewhere: the link stays as it was. And it holds for a
// new release whose core current's link already names: a binary that went missing there goes back
// only past the switch, since a running core's restart would start it.
func TestATermBeforeTheSwitchChangesNothing(t *testing.T) {
	for name, arrange := range map[string]func(r *installRig) string{
		"a new release": func(r *installRig) string { return r.release("two") },
		"the same bundle, its core link elsewhere": func(r *installRig) string {
			r.pointCoreElsewhere()
			return r.release("one")
		},
		"a new release whose core is the one current's core link names, its binary gone": func(r *installRig) string {
			core, err := filepath.EvalSymlinks(filepath.Join(r.data, "current", bundleCore))
			if err != nil {
				r.t.Fatal(err)
			}
			os.Remove(core)
			return r.release("two")
		},
	} {
		t.Run(name, func(t *testing.T) {
			probe := newInstallRig(t)
			probe.mustRun(probe.request(Install, probe.release("one")))
			plan, err := PlanLifecycle(context.Background(), probe.request(Upgrade, arrange(probe)))
			if err != nil {
				t.Fatal(err)
			}
			for landsIn := -1; landsIn < liveAt(plan); landsIn++ {
				when := fmt.Sprintf("a term in step %d", landsIn+1)
				if landsIn < 0 {
					when = "a term before the first step"
				}
				r := newInstallRig(t)
				r.mustRun(r.request(Install, r.release("one")))
				plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, arrange(r)))
				if err != nil {
					t.Fatal(err)
				}
				ctx, term := context.WithCancel(context.Background())
				if landsIn < 0 {
					term()
				} else {
					step := plan.steps[landsIn]
					plan.steps[landsIn].do = func(ctx context.Context) error {
						err := step.do(ctx)
						term()
						return err
					}
				}
				before := r.tree()
				r.clearCalls()
				var out strings.Builder
				err = applyLifecycle(ctx, plan, &out)
				if outcomeOf(err) != outcomeRefused || !errors.Is(err, context.Canceled) {
					t.Errorf("%s: %v\n%s", when, err, out.String())
				}
				if change := r.changed(before); change != "" {
					t.Errorf("%s changed the host: %s\n%s", when, change, out.String())
				}
				term()
			}
		})
	}
}

// Past the switch the rollback restarts the daemon whatever runs: systemd may have restarted it on
// the new release by itself, through current, or the owner may have stopped it before the upgrade,
// as the upgrade guide says to. Either way a term in the units' reload ends with the old release
// running and answering, and the new one gone.
func TestARollbackRestartsTheDaemonWhateverRuns(t *testing.T) {
	for name, meanwhile := range map[string]func(r *installRig){
		"systemd restarted the new release": func(r *installRig) {
			if _, err := runTool(context.Background(), "systemctl", "restart", daemonUnitName); err != nil {
				r.t.Fatal(err)
			}
		},
		"the owner stopped the daemon first": func(*installRig) {},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			r.mustRun(r.request(Install, r.release("one")))
			one := currentRelease(r.data)
			units := r.earlierUnits()
			if name == "the owner stopped the daemon first" {
				if _, err := runTool(context.Background(), "systemctl", "stop", daemonUnitName); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("two")))
			if err != nil {
				t.Fatal(err)
			}
			ctx, term := context.WithCancel(context.Background())
			defer term()
			at := stepAt(t, plan, "rewrite the units")
			rewrite := plan.steps[at]
			plan.steps[at].do = func(ctx context.Context) error {
				meanwhile(r)
				term()
				return rewrite.do(ctx)
			}
			var out strings.Builder
			if err := applyLifecycle(ctx, plan, &out); outcomeOf(err) != outcomeRolledBack {
				t.Fatalf("a term in the units' reload: %v\n%s", err, out.String())
			}
			r.wantReleaseBack(one, units)
		})
	}
}

// A put-back whose reload failed leaves systemd holding the new release's units, though their files
// are back. A retry stopped before the switch is refused and changes nothing, and one that fails past
// it reloads, so the old units are what systemd holds once it says the release is back.
func TestARetryAfterAFailedReloadReloadsTheUnits(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	units := r.earlierUnits()
	if _, err := runTool(context.Background(), "systemctl", "daemon-reload"); err != nil {
		t.Fatal(err)
	}
	broken := r.release("broken")
	plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, broken))
	if err != nil {
		t.Fatal(err)
	}
	at := stepAt(t, plan, "wait for release")
	wait := plan.steps[at]
	plan.steps[at].do = func(ctx context.Context) error {
		err := wait.do(ctx)
		os.WriteFile(filepath.Join(r.root, "reload-fails"), nil, 0o644)
		return err
	}
	var out strings.Builder
	if err := applyLifecycle(context.Background(), plan, &out); outcomeOf(err) != outcomePutBackFailed {
		t.Fatalf("a put-back whose reload fails: %v\n%s", err, out.String())
	}
	os.Remove(filepath.Join(r.root, "reload-fails"))
	if raw, _ := os.ReadFile(filepath.Join(r.loaded(), daemonUnitName)); string(raw) == units[daemonUnitName] {
		t.Fatal("systemd holds the old units already, so this test proves nothing")
	}

	plan, err = PlanLifecycle(context.Background(), r.request(Upgrade, broken))
	if err != nil {
		t.Fatal(err)
	}
	ctx, term := context.WithCancel(context.Background())
	first := plan.steps[0]
	plan.steps[0].do = func(ctx context.Context) error {
		err := first.do(ctx)
		term()
		return err
	}
	before := r.tree()
	r.clearCalls()
	out.Reset()
	if err := applyLifecycle(ctx, plan, &out); outcomeOf(err) != outcomeRefused {
		t.Errorf("a retry stopped before the switch: %v\n%s", err, out.String())
	}
	if change := r.changed(before); change != "" {
		t.Errorf("a retry stopped before the switch changed the host: %s\n%s", change, out.String())
	}

	if _, out, err := r.run(r.request(Upgrade, broken)); outcomeOf(err) != outcomeRolledBack {
		t.Fatalf("a retry that fails past the switch: %v\n%s", err, out)
	}
	r.wantReleaseBack(one, units)
}

// The rollback puts back all the record holds, whether or not it looks changed: a retry of the
// bundle current names puts its core link right, and when its daemon then doesn't answer, the link
// points where it did before.
func TestARollbackPutsBackAllTheRecordHolds(t *testing.T) {
	r := newInstallRig(t)
	one := r.release("one")
	r.mustRun(r.request(Install, one))
	units := r.earlierUnits()
	elsewhere := r.pointCoreElsewhere()
	plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, one))
	if err != nil {
		t.Fatal(err)
	}
	at := stepAt(t, plan, "wait for release")
	plan.steps[at].do = func(context.Context) error { return errors.New("no answer") }
	var out strings.Builder
	if err := applyLifecycle(context.Background(), plan, &out); outcomeOf(err) != outcomeRolledBack {
		t.Fatalf("a retry whose daemon doesn't answer: %v\n%s", err, out.String())
	}
	if now, _ := os.Readlink(filepath.Join(r.data, "current", bundleCore)); now != elsewhere {
		t.Errorf("the core link points at %q, want %q back", now, elsewhere)
	}
	r.wantReleaseBack(currentRelease(r.data), units)
}

// A switch a leftover current.new blocks is rolled back as any other: the rollback doesn't look for
// what changed, so it reloads and restarts the old release, which then answers.
func TestASwitchALeftoverBlocksIsRolledBack(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	units := map[string]string{}
	for _, name := range []string{daemonUnitName, gateway.CoreUnitName} {
		raw, _ := os.ReadFile(filepath.Join(r.units, name))
		units[name] = string(raw)
	}
	if err := os.MkdirAll(filepath.Join(r.data, "current.new", "left"), 0o700); err != nil {
		t.Fatal(err)
	}
	request := r.request(Upgrade, r.release("two"))
	request.Outcome = r.outcomeFile()
	err := RunLifecycle(context.Background(), request)
	if ended := r.readOutcome(); ended.Outcome != outcomeRolledBack || outcomeOf(err) != outcomeRolledBack {
		t.Fatalf("wrote %q for a blocked switch: %v", ended.Outcome, err)
	}
	r.wantReleaseBack(one, units)
}

// An install over a current whose daemon is gone either lands whole or leaves current naming exactly
// what it named, so neither a hook nginx refuses nor a daemon that doesn't answer leaves an older
// release selected.
func TestAnInstallOverADamagedInstallationLandsWholeOrLeavesCurrent(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("two")))
	current := filepath.Join(r.data, "current")
	was, err := os.Readlink(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.data, "current", bundleDaemon)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		arrange func() func()
		bundle  string
	}{
		{"nginx refuses the hook", func() func() {
			refuses := filepath.Join(r.root, "nginx-refuses")
			os.WriteFile(refuses, nil, 0o644)
			return func() { os.Remove(refuses) }
		}, "one"},
		{"the daemon doesn't answer", func() func() { return func() {} }, "broken"},
	} {
		undo := c.arrange()
		if _, out, err := r.run(r.request(Install, r.release(c.bundle))); err == nil {
			t.Errorf("%s: install passed\n%s", c.name, out)
		}
		undo()
		if now, err := os.Readlink(current); err != nil || now != was {
			t.Errorf("%s: current names %q, want %q: %v", c.name, now, was, err)
		}
	}
	r.mustRun(r.request(Install, r.release("one")))
	if got := currentRelease(r.data); got != r.releaseOf("one") {
		t.Errorf("release %s is current after an install that answered", got)
	}
}

// Keeping a release whose daemon matches still leaves it whole: a core link that went missing
// comes back, and the directory current names is the same one.
func TestAKeptReleaseGetsItsCoreLinkBack(t *testing.T) {
	r := newInstallRig(t)
	one := r.release("one")
	r.mustRun(r.request(Install, one))
	dir := releaseDir(r.data, r.releaseOf("one"))
	kept, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []Action{Install, Upgrade} {
		if err := os.Remove(filepath.Join(dir, bundleCore)); err != nil {
			t.Fatal(err)
		}
		r.mustRun(r.request(action, one))
		if raw, err := os.ReadFile(filepath.Join(r.data, "current", bundleCore)); err != nil || string(raw) != string(r.coreBytes) {
			t.Errorf("after %s of the same bundle, the core link reads %q: %v", action, raw, err)
		}
		if now, err := os.Stat(dir); err != nil || !os.SameFile(kept, now) {
			t.Errorf("%s replaced the release current names: %v", action, err)
		}
	}
}
