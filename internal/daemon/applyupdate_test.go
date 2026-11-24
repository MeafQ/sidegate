package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
)

func TestAnUpdateLands(t *testing.T) {
	r := newUpdateRig(t)
	id := r.launched()
	var out bytes.Buffer
	if err := r.u.apply(context.Background(), id, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	r.systemd.says(unitGone)
	if now := currentRelease(r.data); now == r.releaseOf("one") || !strings.HasSuffix(r.runningDaemon(), filepath.Join("releases", now, bundleDaemon)) {
		t.Errorf("current names %s, and %s runs", now, r.runningDaemon())
	}
	if last := r.wantLast(UpdateLanded, ""); last.Attempt != id || last.Version != "v0.0.2" || last.Message != nil {
		t.Errorf("last %+v", last)
	}
	if _, err := os.Stat(attemptStaged(r.data, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("what it staged stayed: %v", err)
	}
	if !r.lockIsFree() || !r.journalGone() {
		t.Error("its lock or journal stayed")
	}
	// The same job again, as a unit started twice would be, applies nothing.
	before := r.tree()
	if err := r.u.apply(context.Background(), id, io.Discard); err == nil || !sameTree(before, r.tree()) {
		t.Errorf("the job run again: %v", err)
	}
}

func TestAnUpdateWithANewCoreLandsAsConfirmed(t *testing.T) {
	r := newUpdateRig(t)
	r.runCore()
	old := r.coreBytes
	r.coreBytes = []byte("core two")
	r.pinCore()
	r.m = r.server.publish(t, "v0.0.2", r.installRig.candidate("v0.0.2", r.server.address(), ":"), pinnedCore)
	newPin := pinnedCore
	if err := os.WriteFile(filepath.Join(r.root, "core-bytes"), r.coreBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	r.coreBytes = old
	r.pinCore()
	one := currentRelease(r.data)
	r.mustCheck(false)
	if offer := r.mustView().Offer; offer == nil || !offer.RestartsCore {
		t.Fatalf("offered %+v", offer)
	}
	if err := r.applied(); err == nil {
		t.Error("an update that restarts the core ran as one that keeps it")
	}
	r.wantLast(UpdateFailed, ReasonPlanChanged)
	r.wantUntouched(one)
	if _, err := r.u.launch(context.Background(), "v0.0.2", true); err != nil {
		t.Fatal(err)
	}
	a, _ := readAttempt(r.data)
	var out bytes.Buffer
	if err := r.u.apply(context.Background(), a.Attempt, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	r.systemd.says(unitGone)
	r.wantLast(UpdateLanded, "")
	if exe := r.coreExe(); filepath.Base(filepath.Dir(exe)) != newPin {
		t.Errorf("the core runs %s", exe)
	}
}

// Every refusal up to the candidate's own upgrade leaves the gateway, its daemon and its core as they were.
func TestEachRefusalOfTheJob(t *testing.T) {
	manifestSays := func(change func(*manifest)) func(*updateRig) {
		return func(r *updateRig) {
			m := r.m
			change(&m)
			r.server.latest(m)
		}
	}
	bundleIs := func(f http.HandlerFunc) func(*updateRig) {
		return func(r *updateRig) { r.server.answer("/download/v0.0.2/"+r.m.Bundle, f) }
	}
	candidateIs := func(entries func(r *updateRig) string) func(*updateRig) {
		return func(r *updateRig) { r.m = r.server.publish(r.t, "v0.0.2", entries(r), pinnedCore) }
	}
	shared := newUpdateRig(t)
	for name, c := range map[string]struct {
		reason  UpdateReason
		arrange func(*updateRig)
	}{
		"the latest release moved on": {ReasonOfferChanged, manifestSays(func(m *manifest) { m.Version, m.Bundle = "v0.0.3", bundleName("v0.0.3") })},
		"no manifest":                 {ReasonDownloadFailed, func(r *updateRig) { r.server.answer("/latest/download/"+manifestName, http.NotFound) }},
		"a manifest that isn't one":   {ReasonDownloadFailed, manifestSays(func(m *manifest) { m.SHA256 = "x" })},
		"a core restart nobody saw":   {ReasonPlanChanged, manifestSays(func(m *manifest) { m.Core = strings.Repeat("c", 64) })},
		"no room": {ReasonNoRoom, func(r *updateRig) {
			r.u.free = func(string) (uint64, error) { return 3*uint64(r.m.Size) + 64<<20 - 1, nil }
		}},
		"room that can't be read": {ReasonNoRoom, func(r *updateRig) { r.u.free = func(string) (uint64, error) { return 1 << 40, errors.New("statfs") } }},
		"no bundle":               {ReasonDownloadFailed, bundleIs(http.NotFound)},
		"a bundle redirected to http": {ReasonDownloadFailed, func(r *updateRig) {
			body, _ := os.ReadFile(r.installRig.candidate("v0.0.2", r.server.address(), ":"))
			r.server.redirect("/download/v0.0.2/"+r.m.Bundle, plainly(r.t, body))
		}},
		"a bundle cut short": {ReasonDownloadFailed, bundleIs(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "999999")
			w.Write([]byte("x"))
		})},
		"a bundle cut short, cleanly": {ReasonChecksum, bundleIs(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("x")) })},
		"a changed bundle": {ReasonChecksum, func(r *updateRig) {
			bundleIs(func(w http.ResponseWriter, _ *http.Request) { w.Write(make([]byte, r.m.Size)) })(r)
		}},
		"a bundle that never ends":      {ReasonChecksum, bundleIs(func(w http.ResponseWriter, _ *http.Request) { io.Copy(w, zeroes{}) })},
		"a tag that isn't the bundle's": {ReasonWrongRelease, candidateIs(func(r *updateRig) string { return r.installRig.candidate("v0.0.3", r.server.address(), ":") })},
		"another releases address": {ReasonWrongRelease, candidateIs(func(r *updateRig) string {
			return r.installRig.candidate("v0.0.2", "https://elsewhere.test/releases", ":")
		})},
		"no releases address":          {ReasonWrongRelease, candidateIs(func(r *updateRig) string { return r.installRig.candidate("v0.0.2", "", ":") })},
		"a sidegated that doesn't run": {ReasonWrongRelease, candidateIs(func(r *updateRig) string { return r.release("not a program") })},
		"a bundle that isn't one": {ReasonWrongRelease, candidateIs(func(r *updateRig) string {
			path := filepath.Join(r.root, "junk")
			os.WriteFile(path, []byte("junk"), 0o600)
			return path
		})},
		"a bundle with no sidegated": {ReasonWrongRelease, candidateIs(func(r *updateRig) string { return r.bundle("coreonly", map[string][]byte{bundleCore: r.coreBytes}) })},
	} {
		t.Run(name, func(t *testing.T) {
			r := shared.again(t)
			one := currentRelease(r.data)
			r.mustCheck(false)
			c.arrange(r)
			if err := r.applied(); err == nil {
				t.Error("the job passed")
			}
			if last := r.wantLast(UpdateFailed, c.reason); last.Message == nil || *last.Message == "" {
				t.Error("it gives no message")
			}
			r.wantUntouched(one)
			if c.reason == ReasonOfferChanged {
				// The next check is due at once, and offers what is there.
				if offer := r.mustView().Offer; offer != nil {
					t.Errorf("the page still offers %s", offer.Version)
				}
				r.clock = r.clock.Add(time.Minute)
				r.mustCheck(true)
				if offer := r.mustView().Offer; offer == nil || offer.Version != "v0.0.3" {
					t.Errorf("after the next load it offers %+v", offer)
				}
			}
		})
	}

	t.Run("not newer", func(t *testing.T) {
		r := newUpdateRig(t)
		one := currentRelease(r.data)
		r.u.build.Version = "v0.0.2"
		if err := r.applied(); err == nil {
			t.Error("the job passed")
		}
		r.wantLast(UpdateFailed, ReasonNotNewer)
		r.wantUntouched(one)
	})
	// The candidate's bundle is right, and its own upgrade refuses it, as one whose core isn't its pin.
	t.Run("refused by the upgrade", func(t *testing.T) {
		r := newUpdateRig(t)
		one := currentRelease(r.data)
		good := r.coreBytes
		r.coreBytes = []byte("not pinned")
		r.server.publish(t, "v0.0.2", r.installRig.candidate("v0.0.2", r.server.address(), ":"), pinnedCore)
		r.coreBytes = good
		if err := r.applied(); err == nil {
			t.Error("the job passed")
		}
		r.wantLast(UpdateRefused, "")
		r.wantUntouched(one)
	})
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { return len(p), nil }

// The read stops one byte past the size the manifest gives.
func TestADownloadReadsAtMostItsSize(t *testing.T) {
	s := newReleaseServer(t)
	read := make(chan int, 1)
	s.answer("/download/v0.0.2/"+bundleName("v0.0.2"), func(w http.ResponseWriter, _ *http.Request) {
		n := 0
		for {
			wrote, err := w.Write(make([]byte, 64<<10))
			n += wrote
			if err != nil || n > 64<<20 {
				read <- n
				return
			}
		}
	})
	path := filepath.Join(t.TempDir(), "bundle")
	err := s.source().download(context.Background(), manifest{"v0.0.2", bundleName("v0.0.2"), 100, strings.Repeat("a", 64), strings.Repeat("b", 64)}, path)
	var failed updateFailure
	if !errors.As(err, &failed) || failed.reason != ReasonChecksum {
		t.Fatalf("failed as %v", err)
	}
	if info, err := os.Stat(path + ".part"); err != nil || info.Size() != 101 {
		t.Errorf("it kept %v %v", info, err)
	}
	if n := <-read; n > 64<<20 {
		t.Errorf("it took %d bytes", n)
	}
}

// The job touches nothing until it holds the lock: it waits out another lifecycle command, and what that
// command changed decides. An uninstall leaves it nothing to do, and an upgrade over SSH makes it moved.
func TestAJobWaitsForTheLockAndReadsWhatIsThere(t *testing.T) {
	start := func(r *updateRig) (release func(), ended chan error) {
		lock, err := holdLifecycle(r.lockPath(), 0)
		if err != nil {
			r.t.Fatal(err)
		}
		a, _ := readAttempt(r.data)
		before := r.tree()
		ended = make(chan error, 1)
		go func() { ended <- r.u.apply(context.Background(), a.Attempt, io.Discard) }()
		time.Sleep(250 * time.Millisecond)
		if !sameTree(before, r.tree()) {
			r.t.Error("the job changed a file before it held the lock")
		}
		return func() { dropLifecycle(lock, r.lockPath()) }, ended
	}
	t.Run("uninstalled", func(t *testing.T) {
		r := newUpdateRig(t)
		r.launched()
		release, ended := start(r)
		if err := os.RemoveAll(r.data); err != nil {
			t.Fatal(err)
		}
		release()
		if err := <-ended; err == nil {
			t.Error("the job ran")
		}
		if _, err := os.Stat(r.data); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the job made %s again: %v", r.data, err)
		}
		if _, err := os.Stat(r.lockPath()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the job left the lock's file: %v", err)
		}
	})
	t.Run("upgraded over SSH", func(t *testing.T) {
		r := newUpdateRig(t)
		r.launched()
		release, ended := start(r)
		other, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("by hand")))
		if err != nil {
			t.Fatal(err)
		}
		if err := applyLifecycle(context.Background(), other, io.Discard); err != nil {
			t.Fatal(err)
		}
		hand := currentRelease(r.data)
		r.clearCalls()
		release()
		if err := <-ended; err == nil {
			t.Error("the job ran")
		}
		r.systemd.says(unitGone)
		r.wantLast(UpdateFailed, ReasonMoved)
		r.wantUntouched(hand)
		if n := r.server.asked(manifestName); n != 0 {
			t.Errorf("it asked %d times for a release it can't install", n)
		}
	})
	t.Run("another attempt took its place", func(t *testing.T) {
		r := newUpdateRig(t)
		first := r.launched()
		r.systemd.says(unitGone)
		r.launched()
		before := r.tree()
		if err := r.u.apply(context.Background(), first, io.Discard); err == nil || !sameTree(before, r.tree()) {
			t.Errorf("the first attempt's job: %v", err)
		}
	})
}

// An install, upgrade or uninstall over SSH during the download is busy.
func TestTheJobHoldsTheLockWhileItDownloads(t *testing.T) {
	r := newUpdateRig(t)
	asked, answer := make(chan struct{}), make(chan struct{})
	r.server.answer("/download/v0.0.2/"+r.m.Bundle, func(w http.ResponseWriter, r *http.Request) {
		close(asked)
		<-answer
		http.NotFound(w, r)
	})
	id := r.launched()
	ended := make(chan error, 1)
	go func() { ended <- r.u.apply(context.Background(), id, io.Discard) }()
	<-asked
	for _, request := range []LifecycleRequest{r.request(Upgrade, r.release("by hand")), r.request(Uninstall, ""), r.request(Install, r.release("one"))} {
		if err := RunLifecycle(context.Background(), request); outcomeOf(err) != outcomeBusy {
			t.Errorf("%s during the download: %v", request.Action, err)
		}
	}
	close(answer)
	<-ended
}

// A stop during the download ends the attempt stopped, and one during the upgrade goes on to the upgrade,
// which is waited out: it puts the old release back.
func TestAStoppedJob(t *testing.T) {
	t.Run("in the download", func(t *testing.T) {
		r := newUpdateRig(t)
		one := currentRelease(r.data)
		ctx, stop := context.WithCancel(context.Background())
		r.server.answer("/download/v0.0.2/"+r.m.Bundle, func(w http.ResponseWriter, r *http.Request) {
			stop()
			<-r.Context().Done()
		})
		if err := r.u.apply(ctx, r.launched(), io.Discard); err == nil {
			t.Error("the job passed")
		}
		r.systemd.says(unitGone)
		r.wantLast(UpdateFailed, ReasonStopped)
		r.wantUntouched(one)
	})
	t.Run("in the upgrade", func(t *testing.T) {
		r := newUpdateRig(t)
		one := currentRelease(r.data)
		r.runCore()
		// The core never stays up, so the switch waits for it, past its boundary.
		os.WriteFile(filepath.Join(r.root, "core-flaps"), nil, 0o600)
		r.storeSlot(domain.DirectExit(), domain.Direct())
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		go func() {
			for currentRelease(r.data) == one {
				time.Sleep(time.Millisecond)
			}
			stop()
		}()
		var out bytes.Buffer
		if err := r.u.apply(ctx, r.launched(), &out); err == nil {
			t.Error("the job passed")
		}
		r.systemd.says(unitGone)
		if end := r.mustView().Last; end == nil || (end.Outcome != UpdateRolledBack && end.Outcome != UpdatePutBackFailed) {
			t.Fatalf("it ended %+v\n%s", end, out.String())
		}
		if now := currentRelease(r.data); now != one {
			t.Errorf("current names %s", now)
		}
	})
}

// An upgrade that dies before it says how it ended: with no journal it changed nothing, and the attempt is
// stopped. With one, the attempt says nothing yet, the stop-post restarts the daemon, and the recovery and
// the put-back daemon say how it ended.
func TestAnUpgradeThatDiesWithoutAWord(t *testing.T) {
	quickSettle(t)
	t.Run("before its journal", func(t *testing.T) {
		r := newUpdateRig(t)
		one := currentRelease(r.data)
		r.m = r.server.publish(t, "v0.0.2", r.installRig.candidate("v0.0.2", r.server.address(), "kill -9 $$"), pinnedCore)
		if err := r.applied(); err == nil {
			t.Error("the job passed")
		}
		if last := r.wantLast(UpdateFailed, ReasonStopped); !strings.Contains(*last.Message, "signal: killed") {
			t.Errorf("it says %s", *last.Message)
		}
		r.wantUntouched(one)
	})
	t.Run("in its switch", func(t *testing.T) {
		r := newUpdateRig(t)
		one := currentRelease(r.data)
		id := r.launched()
		a, _ := readAttempt(r.data)
		a.Started = true
		writeJSON(attemptFile(r.data), a)
		request := r.request(Upgrade, r.release("broken"))
		request.Outcome = attemptOutcome(r.data, id)
		r.cutAfter(request, "restart "+daemonUnitName)
		r.systemd.says(unitGone)
		r.wantLast(UpdateFailed, ReasonInterrupted)
		if err := settleUpdate(context.Background(), r.data, r.lockPath(), io.Discard); err == nil {
			t.Error("the stub systemctl runs no pre-start, so the switch is still owed")
		}
		if !strings.Contains(r.callLog(), "systemctl restart "+daemonUnitName) {
			t.Errorf("the stop-post didn't restart the daemon:\n%s", r.callLog())
		}
		if _, err := r.recover(); err != nil {
			t.Fatal(err)
		}
		r.clearCalls()
		if err := settleUpdate(context.Background(), r.data, r.lockPath(), io.Discard); err != nil || r.callLog() != "" {
			t.Errorf("a put-back release that only has to answer: %v %s", err, r.callLog())
		}
		if err := confirmPutBack(r.data, r.lockPath(), &one); err != nil {
			t.Fatal(err)
		}
		r.wantLast(UpdateRolledBack, "")
	})
}

// The put-back release's daemon says rolledBack whatever it met first: the stop-post holding the lock on its
// next look at the record, or an outcome it couldn't write. One that stops meanwhile doesn't wait for either,
// and one whose switch another command ended says nothing.
func TestAPutBackIsSaidOnceNothingStandsInItsWay(t *testing.T) {
	serve := func(t *testing.T, r *updateRig) {
		h, _ := newPageDaemon(t)
		app, _ := h.current()
		app.bootstrap.DataDir, app.release, app.updates = r.data, r.u.release, r.u
		serveOnSocket(t, app)
	}
	putBack := func(t *testing.T) (*updateRig, string) {
		r := newUpdateRig(t)
		id := r.launched()
		a, _ := readAttempt(r.data)
		a.Started = true
		writeJSON(attemptFile(r.data), a)
		request := r.request(Upgrade, r.release("broken"))
		request.Outcome = attemptOutcome(r.data, id)
		r.cutAfter(request, "restart "+daemonUnitName)
		r.systemd.says(unitGone)
		if _, err := r.recover(); err != nil {
			t.Fatal(err)
		}
		return r, id
	}
	said := func(t *testing.T, r *updateRig) {
		t.Helper()
		for until := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if last := r.mustView().Last; last != nil && last.Outcome == UpdateRolledBack || time.Now().After(until) {
				break
			}
		}
		r.wantLast(UpdateRolledBack, "")
	}
	t.Run("the stop-post holds the lock", func(t *testing.T) {
		r, _ := putBack(t)
		lock := r.holdLock()
		serve(t, r)
		time.Sleep(200 * time.Millisecond)
		r.wantLast(UpdateFailed, ReasonInterrupted)
		dropLifecycle(lock, r.lockPath())
		said(t, r)
		for until := time.Now().Add(5 * time.Second); (!r.journalGone() || !r.lockIsFree()) && time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
		}
		if !r.journalGone() || !r.lockIsFree() {
			t.Error("its journal or its lock stayed")
		}
	})
	t.Run("its outcome can't be written", func(t *testing.T) {
		r, id := putBack(t)
		// A directory takes no file's place.
		blocked := filepath.Join(attemptOutcome(r.data, id), "held")
		if err := os.MkdirAll(blocked, 0o700); err != nil {
			t.Fatal(err)
		}
		serve(t, r)
		time.Sleep(200 * time.Millisecond)
		if r.journalGone() {
			t.Fatal("the journal went unsaid")
		}
		os.RemoveAll(filepath.Dir(blocked))
		said(t, r)
	})
	t.Run("another command holds the lock and ends its switch", func(t *testing.T) {
		r, _ := putBack(t)
		lock := r.holdLock()
		serve(t, r)
		if err := removeJournal(r.data); err != nil {
			t.Fatal(err)
		}
		dropLifecycle(lock, r.lockPath())
		time.Sleep(confirmPause + 200*time.Millisecond)
		if _, end := readAttempt(r.data); end != nil {
			t.Errorf("the daemon said %+v over that command's switch", end)
		}
	})
	t.Run("the daemon stops while the lock is held", func(t *testing.T) {
		r, _ := putBack(t)
		r.holdLock()
		serve(t, r)
	})
}

// The stop-post does nothing where no journal is left, and never restarts the daemon under another command
// that holds the lock and owns its own switch.
func TestTheStopPostLeavesAnotherCommandsSwitchAlone(t *testing.T) {
	quickSettle(t)
	r := newUpdateRig(t)
	if err := settleUpdate(context.Background(), r.data, r.lockPath(), io.Discard); err != nil || r.callLog() != "" {
		t.Errorf("with no journal: %v %s", err, r.callLog())
	}
	r.cutAfter(r.request(Upgrade, r.release("two")), "restart "+daemonUnitName)
	r.clearCalls()
	lock := r.holdLock()
	ended := make(chan error, 1)
	go func() { ended <- settleUpdate(context.Background(), r.data, r.lockPath(), io.Discard) }()
	time.Sleep(250 * time.Millisecond)
	if strings.Contains(r.callLog(), "restart") {
		t.Errorf("it restarted the daemon under the lock:\n%s", r.callLog())
	}
	// That command ends its switch.
	if err := removeJournal(r.data); err != nil {
		t.Fatal(err)
	}
	dropLifecycle(lock, r.lockPath())
	if err := <-ended; err != nil || strings.Contains(r.callLog(), "restart") {
		t.Errorf("after it: %v %s", err, r.callLog())
	}

	// One that ended without finishing it leaves it to the stop-post.
	r.cutAfter(r.request(Upgrade, r.release("three")), "restart "+daemonUnitName)
	r.clearCalls()
	lock = r.holdLock()
	time.AfterFunc(200*time.Millisecond, func() { dropLifecycle(lock, r.lockPath()) })
	settleUpdate(context.Background(), r.data, r.lockPath(), io.Discard)
	if !strings.Contains(r.callLog(), "systemctl restart "+daemonUnitName) {
		t.Errorf("a switch another command left wasn't taken up:\n%s", r.callLog())
	}
}

func quickSettle(t *testing.T) {
	old := settlePause
	settlePause = 10 * time.Millisecond
	t.Cleanup(func() { settlePause = old })
}

// A switch whose outcome file's directory is gone, as a later attempt or a reboot removes one, still ends.
func TestASwitchWhoseOutcomeHasNoPlaceLeftEnds(t *testing.T) {
	r := newUpdateRig(t)
	one := currentRelease(r.data)
	gone := filepath.Join(r.root, "gone")
	os.Mkdir(gone, 0o700)
	request := r.request(Upgrade, r.release("broken"))
	request.Outcome = filepath.Join(gone, "outcome.json")
	r.cutAfter(request, "restart "+daemonUnitName)
	os.RemoveAll(gone)
	if _, err := r.recover(); err != nil {
		t.Fatal(err)
	}
	if err := confirmPutBack(r.data, r.lockPath(), &one); err != nil {
		t.Fatal(err)
	}
	if !r.journalGone() {
		t.Error("the journal stayed")
	}
}

// update over SSH: the plan changes nothing, --yes follows the job to its end, and only a landing is success.
func TestUpdateOverSSH(t *testing.T) {
	ctx := context.Background()
	run := func(r *updateRig, apply bool) (string, error) {
		var out bytes.Buffer
		err := r.u.update(ctx, apply, &out)
		return out.String(), err
	}
	r := newUpdateRig(t)
	before := r.tree()
	out, err := run(r, false)
	if err != nil || !strings.Contains(out, "update v0.0.1 to v0.0.2, whose notes are "+r.server.address()+"/tag/v0.0.2") || !strings.Contains(out, "no flow drops") || !strings.Contains(out, "Nothing changed") {
		t.Errorf("the plan: %v\n%s", err, out)
	}
	if !sameTree(before, r.tree()) || len(r.systemd.launches()) != 0 {
		t.Error("the plan changed something")
	}
	for name, c := range map[string]struct {
		change func(*updateRig)
		says   string
	}{
		"a development build": {func(r *updateRig) { r.u.build = Build{Version: "dev-1234"} }, "takes no updates"},
		"a bundle's own":      {func(r *updateRig) { r.u.release = nil }, "current/sidegated update"},
		"a release that isn't current": {func(r *updateRig) {
			other := "0123456789ab"
			r.u.release = &other
		}, "current/sidegated update"},
		"no manifest": {func(r *updateRig) { r.server.answer("/latest/download/"+manifestName, http.NotFound) }, "nothing changed"},
	} {
		r := newUpdateRig(t)
		c.change(r)
		if out, err := run(r, true); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	m := r.m
	m.Version, m.Bundle = "v0.0.1", bundleName("v0.0.1")
	r.server.latest(m)
	if out, err := run(r, true); err != nil || !strings.Contains(out, "v0.0.1 is the newest release") || len(r.systemd.launches()) != 0 {
		t.Errorf("nothing newer: %v %s", err, out)
	}
	r.server.latest(r.m)
	if os.Geteuid() != 0 {
		if _, err := run(r, true); err == nil || !strings.Contains(err.Error(), "root") {
			t.Errorf("not root: %v", err)
		}
	}

	follow := func(r *updateRig) (string, error) {
		id := r.launched()
		go func() {
			r.u.apply(ctx, id, io.Discard)
			r.systemd.says(unitGone)
		}()
		var out bytes.Buffer
		err := r.u.follow(ctx, id, &out)
		return out.String(), err
	}
	if out, err := follow(newUpdateRig(t)); err != nil || !strings.Contains(out, "v0.0.2 is installed") {
		t.Errorf("followed to its landing: %v %s", err, out)
	}
	bad := newUpdateRig(t)
	bad.server.answer("/download/v0.0.2/"+bad.m.Bundle, http.NotFound)
	if out, err := follow(bad); err == nil || !strings.Contains(err.Error(), "downloadFailed") {
		t.Errorf("followed to its failure: %v %s", err, out)
	}
}

// An install over another release names the versions and the command that works.
func TestAnInstallOverAnotherReleaseSaysHowToMove(t *testing.T) {
	const address = "https://example.test/OWNER/sidegate/releases"
	for name, c := range map[string]struct {
		installed, this string
		own             bool
		says            string
	}{
		"newer":                      {"v0.0.1\n" + address, "v0.0.2", true, "sidegate v0.0.1 is installed, and this is v0.0.2. Update it: %s update --yes"},
		"older":                      {"v0.0.2\n" + address, "v0.0.1", true, "sidegate v0.0.2 is installed, and this is v0.0.1. To change to it, run upgrade --bundle with the bundle's own sidegated"},
		"over a checkout":            {"dev-1234", "v0.0.1", true, "sidegate dev-1234 is installed, and this is v0.0.1. To change to it, run upgrade --bundle with the bundle's own sidegated"},
		"not the bundle's own":       {"v0.0.1\n" + address, "v0.0.2", false, "sidegate v0.0.1 is installed, and this is release "},
		"a daemon that says nothing": {"", "v0.0.2", true, "sidegate release "},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			script := "#!/bin/sh\n[ -z '" + c.installed + "' ] || echo '" + c.installed + "'\n"
			r.mustRun(r.request(Install, r.bundle("installed", map[string][]byte{bundleDaemon: []byte(script), bundleCore: r.coreBytes})))
			request := r.request(Install, r.release("next"))
			request.Build = Build{Version: c.this, Releases: address}
			if c.own {
				request.recoveryBinary = filepath.Join(r.root, "own")
				os.WriteFile(request.recoveryBinary, []byte("next"), 0o755)
			}
			before := r.tree()
			err := RunLifecycle(context.Background(), request)
			if want := strings.ReplaceAll(c.says, "%s", filepath.Join(r.data, "current", bundleDaemon)); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("it says %v, want %s", err, want)
			}
			if !sameTree(before, r.tree()) {
				t.Error("it changed a file")
			}
		})
	}
}

// The routes: what each answers, and the checks in front of it.
func TestTheUpdateRoutes(t *testing.T) {
	h, server := newPageDaemon(t)
	app, _ := h.current()
	r := newUpdateRig(t)
	app.updates = r.u
	body := func(reply answer) (view UpdateView) {
		t.Helper()
		if err := json.Unmarshal([]byte(reply.body), &view); err != nil {
			t.Fatalf("%+v: %v", reply, err)
		}
		return view
	}
	post := func(path, body string) answer { return mutate(t, server.URL, http.MethodPost, path, "", nil, body) }

	if view := body(read(t, server.URL, "api/update")); !view.Releases || view.Version != "v0.0.1" || view.Check.At != nil || view.Offer != nil || view.Last != nil {
		t.Errorf("at first %+v", view)
	}
	settings, _ := json.Marshal(app.Settings())
	if !strings.Contains(string(settings), `"checkForUpdates":true`) {
		t.Fatalf("settings %s", settings)
	}
	set := func(on bool) {
		t.Helper()
		if _, err := app.Execute(context.Background(), PutCheckForUpdates{On: on}); err != nil {
			t.Fatal(err)
		}
	}
	set(false)
	if reply := post("api/update/check", `{"auto":true}`); reply.status != 200 || r.server.manifests() != 0 || body(reply).Check.At != nil {
		t.Errorf("an automatic check turned off: %+v, %d fetches", reply, r.server.manifests())
	}
	if reply := post("api/update/check", `{"auto":false}`); reply.status != 200 || r.server.manifests() != 1 || body(reply).Offer == nil {
		t.Errorf("a press with it off: %+v, %d fetches", reply, r.server.manifests())
	}
	set(true)
	r.clock = r.clock.Add(checkEvery)
	for range 2 {
		if reply := post("api/update/check", `{"auto":true}`); reply.status != 200 || r.server.manifests() != 2 {
			t.Errorf("an automatic check with it on: %+v, %d fetches", reply, r.server.manifests())
		}
	}
	for _, c := range []struct{ path, body string }{{"api/update/check", ``}, {"api/update/check", `{}`}, {"api/update/check", `{"auto":"no"}`}, {"api/update/check", `{"auto":true,"x":1}`},
		{"api/update", ``}, {"api/update", `{"version":"v0.0.2"}`}, {"api/update", `{"restartsCore":false}`}, {"api/update", `{"version":"v0.0.2","restartsCore":false,"attempt":"x"}`}} {
		if reply := post(c.path, c.body); reply.status != 400 || !strings.Contains(reply.body, "malformed") {
			t.Errorf("%s %s: %+v", c.path, c.body, reply)
		}
	}
	for _, path := range []string{"api/update/check", "api/update"} {
		if reply := mutate(t, server.URL, http.MethodPost, path, "", map[string]string{CSRFHeader: "stale"}, `{"auto":true}`); reply.status != 403 || !strings.Contains(reply.body, "csrf") {
			t.Errorf("%s with another token: %+v", path, reply)
		}
	}
	if len(r.systemd.launches()) != 0 {
		t.Fatal("a refused request started a job")
	}
	if view := body(read(t, server.URL, "api/update")); view.Guide == nil || *view.Guide != strings.TrimSuffix(r.server.address(), "/releases")+"/blob/v0.0.1/docs/operate.md" {
		t.Errorf("the recovery guide: %+v", view)
	}
	first := post("api/update", `{"version":"v0.0.2","restartsCore":false}`)
	again := post("api/update", `{"version":"v0.0.2","restartsCore":false}`)
	if first.status != 202 || again.status != 202 || first.body != again.body || body(first).Running == nil || len(r.systemd.launches()) != 1 {
		t.Errorf("a press and another: %+v %+v", first, again)
	}
	r.systemd.showErr = errors.New("no bus")
	if reply := read(t, server.URL, "api/update"); reply.status != 503 {
		t.Errorf("with systemd silent: %+v", reply)
	}
	r.systemd.showErr = nil

	app.SetBuild(Build{Version: "dev-1234"})
	if view := body(read(t, server.URL, "api/update")); view != (UpdateView{Version: "dev-1234"}) {
		t.Errorf("a development build says %+v", view)
	}
	if reply := post("api/update/check", `{"auto":true}`); reply.status != 200 || body(reply).Releases {
		t.Errorf("its automatic check: %+v", reply)
	}
	for path, body := range map[string]string{"api/update/check": `{"auto":false}`, "api/update": `{"version":"v0.0.2","restartsCore":false}`} {
		if reply := post(path, body); reply.status != 422 || !strings.Contains(reply.body, string(RefusedNoReleases)) {
			t.Errorf("its %s: %+v", path, reply)
		}
	}
}
