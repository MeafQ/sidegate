package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ApplyUpdate is the update job. It takes the lifecycle lock and holds it to its end, so an install, an
// upgrade or an uninstall during it is busy, and touches nothing of update/ before that: an uninstall that
// came first left no attempt, and then it does nothing. Everything up to the candidate's own upgrade
// leaves the gateway as it was and ends the attempt failed. The upgrade owns the switch and its rollback.
func ApplyUpdate(ctx context.Context, dataDir string, build Build, attempt string, out io.Writer) error {
	return newUpdater(dataDir, build, runningRelease(dataDir)).apply(ctx, attempt, out)
}

func (u *updater) apply(ctx context.Context, attempt string, out io.Writer) error {
	lock, err := waitLifecycle(ctx, u.lockPath, jobLockWait)
	if err != nil {
		return err
	}
	defer dropLifecycle(lock, u.lockPath)
	a, end := readAttempt(u.dataDir)
	if a == nil || a.Attempt != attempt || a.Started || end != nil {
		return fmt.Errorf("no update %s is to be applied", attempt)
	}
	a.Started = true
	if err := writeJSON(attemptFile(u.dataDir), a); err != nil {
		return err
	}
	staged := attemptStaged(u.dataDir, attempt)
	bundle, err := u.stage(ctx, *a, staged, out)
	if err == nil {
		err = runUpgrade(ctx, *a, u.dataDir, filepath.Join(staged, bundleDaemon), bundle, lock, out)
	}
	// First, since a full disk may have room for the outcome only then.
	removed := os.RemoveAll(staged)
	var failed updateFailure
	switch {
	case errors.As(err, &failed) && ctx.Err() != nil:
		failed.reason = ReasonStopped
		fallthrough
	case errors.As(err, &failed):
		err = errors.Join(err, writeJSON(attemptOutcome(u.dataDir, attempt), failed.end()))
	}
	return errors.Join(err, removed)
}

// stage downloads and checks the release the owner confirmed, and leaves its bundle and the bundle's own
// sidegated in dir. Its every error is an updateFailure.
func (u *updater) stage(ctx context.Context, a updateAttempt, dir string, out io.Writer) (string, error) {
	fail := func(reason UpdateReason, err error) (string, error) { return "", updateFailure{reason, err} }
	say := func(format string, args ...any) { fmt.Fprintf(out, "- "+format+"\n", args...) }

	if current := currentRelease(u.dataDir); current != a.Expect {
		return fail(ReasonMoved, fmt.Errorf("release %q is current, not %s", current, a.Expect))
	}
	say("read the latest release's %s", manifestName)
	m, err := u.source.latest(ctx)
	if err != nil {
		return fail(ReasonDownloadFailed, err)
	}
	if m.Version != a.Version {
		return fail(ReasonOfferChanged, fmt.Errorf("%s is the latest release now, not %s", m.Version, a.Version))
	}
	running, _ := u.build.release()
	if next, _ := parseReleaseVersion(m.Version); !next.above(running) {
		return fail(ReasonNotNewer, fmt.Errorf("%s isn't newer than %s", m.Version, u.build.Version))
	}
	restarts, err := u.restartsCore(ctx, m.Core)
	if err != nil {
		return fail(ReasonPlanChanged, err)
	}
	if restarts != a.RestartsCore {
		return fail(ReasonPlanChanged, fmt.Errorf("this update %s now, and the plan confirmed %s", saysCore[restarts], saysCore[a.RestartsCore]))
	}
	// The bundle, its sidegated here, and the release, the journal and perhaps a core it unpacks into.
	need := 3*uint64(m.Size) + 64<<20
	if free, err := u.free(dir); err != nil || free < need {
		return fail(ReasonNoRoom, errors.Join(fmt.Errorf("the update needs %d MB free in %s, which has %d", need>>20, u.dataDir, free>>20), err))
	}
	say("download %s, %d MB", m.Bundle, m.Size>>20)
	path := filepath.Join(dir, m.Bundle)
	if err := u.source.download(ctx, m, path); err != nil {
		return "", err
	}
	say("check that its own %s is %s", bundleDaemon, m.Version)
	candidate := filepath.Join(dir, bundleDaemon)
	if err := unpackDaemon(path, candidate); err != nil {
		return fail(ReasonWrongRelease, err)
	}
	switch said, err := versionOf(ctx, candidate); {
	case err != nil:
		return fail(ReasonWrongRelease, err)
	case said != Build{Version: m.Version, Releases: u.build.Releases}:
		return fail(ReasonWrongRelease, fmt.Errorf("the bundle's own %s says %s from %q, not %s from %q", bundleDaemon, said.Version, said.Releases, m.Version, u.build.Releases))
	}
	return path, nil
}

func unpackDaemon(bundle, to string) error {
	found := false
	err := walkBundle(bundle, func(name string, content io.Reader) error {
		if name != bundleDaemon {
			return nil
		}
		found = true
		return copyAtomic(to, content, 0o755)
	})
	if err == nil && !found {
		err = fmt.Errorf("%s has no %s", bundle, bundleDaemon)
	}
	return err
}

// runUpgrade runs the candidate's own upgrade under the job's lock, its fd 3. A SIGTERM goes on to it, and
// it is waited out, since past its boundary it puts the old release back on its own limit. It says
// how it ended in the attempt's outcome. It writes to the job's own output and not to a pipe, so a job that
// is killed doesn't take it along at its next line. One that ended without a word, killed or unable to parse its
// command, is an updateFailure, unless it left its journal: then the unit's stop-post restarts the daemon,
// and the recovery before that start, or the put-back daemon, says how it ended.
func runUpgrade(ctx context.Context, a updateAttempt, dataDir, candidate, bundle string, lock *os.File, out io.Writer) error {
	restarts := "no"
	if a.RestartsCore {
		restarts = "yes"
	}
	cmd := exec.CommandContext(ctx, candidate, "upgrade", "--data-dir", dataDir, "--bundle", bundle, "--yes", "--lock-fd", "3",
		"--expect", a.Expect, "--restarts-core="+restarts, "--outcome", attemptOutcome(dataDir, a.Attempt))
	cmd.ExtraFiles = []*os.File{lock}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if end := readEnd(dataDir, a.Attempt); end != nil {
		return err
	}
	if _, journal := os.Stat(journalDir(dataDir)); journal == nil {
		return errors.Join(errors.New("the upgrade ended in its switch"), err)
	}
	return updateFailure{ReasonStopped, fmt.Errorf("the upgrade ended without its outcome: %w", err)}
}

// SettleUpdate is the update job's stop-post, which the journal's own sidegated runs. A journal that is
// still there once the job is gone is a switch nobody finishes: the candidate's daemon may run and never
// answer, which systemd never restarts. Restarting it runs the recovery before its start. Another
// lifecycle command owns the journal while it holds the lock, and one that ends without finishing it is
// waited out. A put-back release that only has to answer is owed nothing more.
func SettleUpdate(ctx context.Context, dataDir string, out io.Writer) error {
	return settleUpdate(ctx, dataDir, lifecycleLockPath, out)
}

const settleTries = 5

// settlePause is a variable only so a test needn't wait it out.
var settlePause = time.Second

func settleUpdate(ctx context.Context, dataDir, lockPath string, out io.Writer) error {
	for range settleTries {
		// The lock is only looked at. The recovery takes it itself.
		lock, err := waitLifecycle(ctx, lockPath, 0)
		if err != nil {
			return err
		}
		record, rerr := readRecord(dataDir)
		if err := dropLifecycle(lock, lockPath); err != nil {
			return err
		}
		if _, err := os.Stat(journalDir(dataDir)); errors.Is(err, os.ErrNotExist) || rerr == nil && record.Phase == phaseRestored {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		fmt.Fprintf(out, "- restart %s, so its recovery puts back the switch to release %s the update left\n", daemonUnitName, record.candidate())
		if _, err := runTool(ctx, "systemctl", "restart", daemonUnitName); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settlePause):
		}
	}
	return fmt.Errorf("the switch the update left is still to be put back: %s recover --yes", journalBinary(dataDir))
}

// RunUpdate is update over SSH, with no daemon in it: the check, the plan, and with apply the same job a
// press on the page starts, followed to its end. Its error is an update that didn't land.
func RunUpdate(ctx context.Context, dataDir string, build Build, apply bool, out io.Writer) error {
	return newUpdater(dataDir, build, runningRelease(dataDir)).update(ctx, apply, out)
}

func (u *updater) update(ctx context.Context, apply bool, out io.Writer) error {
	if _, release := u.build.release(); !release {
		return fmt.Errorf("sidegate %s is a development build, which takes no updates", u.build.Version)
	}
	if u.release == nil || currentRelease(u.dataDir) != *u.release {
		return fmt.Errorf("run the installed sidegated: %s update", filepath.Join(u.dataDir, "current", bundleDaemon))
	}
	m, err := u.source.latest(ctx)
	if err != nil {
		return fmt.Errorf("the latest release isn't known, and nothing changed: %w", err)
	}
	offer := u.offerOf(ctx, m.Version, m.Core)
	if offer == nil {
		fmt.Fprintf(out, "sidegate %s is the newest release.\n", u.build.Version)
		return nil
	}
	fmt.Fprintf(out, "update %s to %s, whose notes are %s:\n", u.build.Version, offer.Version, offer.Notes)
	fmt.Fprintf(out, "  1. download %s, %d MB, and check it against its checksum\n", m.Bundle, m.Size>>20)
	fmt.Fprintf(out, "  2. run its own upgrade, which puts release %s back unless the new one answers\n", *u.release)
	if offer.RestartsCore {
		fmt.Fprintln(out, "The new release's core differs from the running one: restarting it drops every slot's flows.")
	} else {
		fmt.Fprintln(out, "The core stays running: no flow drops.")
	}
	if !apply {
		fmt.Fprintln(out, "Nothing changed. Run again with --yes to apply.")
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("an update needs root on the gateway")
	}
	attempt, err := u.launch(ctx, offer.Version, offer.RestartsCore)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "The update runs on its own, and goes on if this connection doesn't. Its log: journalctl -u %s\n", strings.TrimSuffix(updateUnit, ".service"))
	return u.follow(ctx, attempt, out)
}

// follow waits for an attempt to end and says how.
func (u *updater) follow(ctx context.Context, attempt string, out io.Writer) error {
	for {
		view, err := u.view(ctx)
		switch {
		case err != nil:
			log.Printf("following the update: %v", err)
		case view.Running != nil && view.Running.Attempt == attempt:
		case view.Last == nil || view.Last.Attempt != attempt:
			return errors.New("another update took this one's place")
		case view.Last.Outcome == UpdateLanded:
			fmt.Fprintf(out, "sidegate %s is installed.\n", view.Last.Version)
			return nil
		default:
			return errors.New(ended(*view.Last))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func ended(last UpdateLast) string {
	text := string(last.Outcome)
	if last.Reason != nil {
		text += ", " + string(*last.Reason)
	}
	if last.Message != nil {
		text += ": " + *last.Message
	}
	return "the update to " + last.Version + " " + text
}
