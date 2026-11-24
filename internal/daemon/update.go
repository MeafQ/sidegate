package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The updater's files are update/ in the data dir. check.json is the last check, the daemon's alone.
// attempt.json is the last attempt, which a launcher writes before it starts the job, and the job
// alone after that. Both hold the lifecycle lock to write it. update/<attempt>/ is that attempt's:
// outcome.json, how it ended, and staged/, which goes when its job does.
func updateDir(dataDir string) string { return filepath.Join(dataDir, "update") }

func checkFile(dataDir string) string { return filepath.Join(updateDir(dataDir), "check.json") }

func attemptFile(dataDir string) string { return filepath.Join(updateDir(dataDir), "attempt.json") }

func attemptDir(dataDir, attempt string) string { return filepath.Join(updateDir(dataDir), attempt) }

func attemptOutcome(dataDir, attempt string) string {
	return filepath.Join(attemptDir(dataDir, attempt), "outcome.json")
}

func attemptStaged(dataDir, attempt string) string {
	return filepath.Join(attemptDir(dataDir, attempt), "staged")
}

const (
	checkEvery     = 24 * time.Hour
	updateUnit     = "sidegate-update.service"
	updateRunLimit = 20 * time.Minute
	// How long systemd lets a stopped job end: its upgrade may have all of its rollback to run.
	updateStopLimit = rollbackLimit + 3*time.Minute
	jobLockWait     = 5 * time.Minute
	collectWait     = 3 * time.Second
)

// UpdateOutcome is how an attempt ended: as its upgrade said, or failed with a reason.
type UpdateOutcome string

const (
	UpdateLanded          UpdateOutcome = "landed"
	UpdateLandedWithError UpdateOutcome = "landedWithError"
	UpdateRefused         UpdateOutcome = "refused"
	UpdateRolledBack      UpdateOutcome = "rolledBack"
	UpdatePutBackFailed   UpdateOutcome = "putBackFailed"
	UpdateFailed          UpdateOutcome = "failed"
)

// UpdateReason is why an attempt failed. Each but interrupted left the gateway as it was.
type UpdateReason string

const (
	ReasonNoStart        UpdateReason = "noStart"      // The job never ran.
	ReasonStopped        UpdateReason = "stopped"      // The job ended before it said how.
	ReasonInterrupted    UpdateReason = "interrupted"  // It ended in a switch that is still to be put back.
	ReasonMoved          UpdateReason = "moved"        // Another release is current than the one updated from.
	ReasonOfferChanged   UpdateReason = "offerChanged" // Another release is the latest now.
	ReasonNotNewer       UpdateReason = "notNewer"
	ReasonPlanChanged    UpdateReason = "planChanged" // The core's restart isn't as confirmed.
	ReasonNoRoom         UpdateReason = "noRoom"
	ReasonDownloadFailed UpdateReason = "downloadFailed"
	ReasonChecksum       UpdateReason = "checksum"
	ReasonWrongRelease   UpdateReason = "wrongRelease" // The bundle's own sidegated says another version or address.
)

// updateFailure is an attempt its job ended before the upgrade.
type updateFailure struct {
	reason UpdateReason
	err    error
}

func (f updateFailure) Error() string { return f.err.Error() }
func (f updateFailure) Unwrap() error { return f.err }

// updateAttempt is attempt.json: what the owner confirmed, and whether the job took it up.
type updateAttempt struct {
	Attempt      string    `json:"attempt"`
	Version      string    `json:"version"`
	Expect       string    `json:"expect"` // The release updated from.
	RestartsCore bool      `json:"restartsCore"`
	At           time.Time `json:"at"`
	Started      bool      `json:"started"`
}

// updateEnd is outcome.json, as the job writes it, or as the upgrade or a recovery does, without a reason.
type updateEnd struct {
	Outcome UpdateOutcome `json:"outcome"`
	Reason  *UpdateReason `json:"reason,omitempty"`
	Message *string       `json:"message"`
}

func (f updateFailure) end() updateEnd {
	message := f.Error()
	return updateEnd{Outcome: UpdateFailed, Reason: &f.reason, Message: &message}
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

// An attempt's id names its directory, so one that isn't an id of ours is no attempt.
var attemptID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// readAttempt is nil with no attempt, and an attempt that hasn't said how it ended has no end. It reads two
// files, so only the holder of the lifecycle lock may take them for one moment's. Others read steady's.
func readAttempt(dataDir string) (*updateAttempt, *updateEnd) {
	return readUpdateFiles(dataDir).attempt()
}

func readEnd(dataDir, attempt string) *updateEnd {
	raw, _ := os.ReadFile(attemptOutcome(dataDir, attempt))
	return parseEnd(string(raw))
}

func parseEnd(raw string) *updateEnd {
	var end updateEnd
	if json.Unmarshal([]byte(raw), &end) != nil {
		return nil
	}
	switch reason := UpdateReason(end.Outcome); {
	case end.Outcome == UpdateFailed && end.Reason != nil:
	case reason == ReasonMoved || reason == ReasonPlanChanged:
		end.Outcome, end.Reason = UpdateFailed, &reason
	case end.Outcome == UpdateLanded, end.Outcome == UpdateLandedWithError, end.Outcome == UpdateRolledBack, end.Outcome == UpdatePutBackFailed:
	default:
		end.Outcome = UpdateRefused
	}
	return &end
}

// endOf is how an attempt whose job is gone ended. One that said nothing was cut short, and what it left
// says where.
func (a updateAttempt) endOf(end *updateEnd, interrupted bool) updateEnd {
	switch {
	case end != nil:
		return *end
	case interrupted:
		return updateFailure{ReasonInterrupted, errors.New("the update was cut short, and what it switched is still to be put back")}.end()
	case a.Started:
		return updateFailure{ReasonStopped, errors.New("the update was cut short, and changed nothing the gateway runs from")}.end()
	}
	return updateFailure{ReasonNoStart, errors.New("the update didn't start")}.end()
}

// UpdateView is api/update: the build, the last check, what it offers, the update that runs and the last
// one that ended.
type UpdateView struct {
	Version  string         `json:"version"`
	Releases bool           `json:"releases"` // False on a development build, which has no check and no update.
	Guide    *string        `json:"guide"`    // docs/operate.md of the release that runs, which says how to recover over SSH.
	Check    UpdateCheck    `json:"check"`
	Offer    *UpdateOffer   `json:"offered"`
	Running  *UpdateRunning `json:"running"`
	Last     *UpdateLast    `json:"last"`
}

// UpdateCheck is the last check that ended.
type UpdateCheck struct {
	At      *time.Time    `json:"at"`
	Failure *CheckFailure `json:"failure"`
}

// UpdateOffer is a newer release. Notes is its page.
type UpdateOffer struct {
	Version      string `json:"version"`
	RestartsCore bool   `json:"restartsCore"`
	Notes        string `json:"notes"`
}

type UpdateRunning struct {
	Attempt string `json:"attempt"`
	Version string `json:"version"`
}

type UpdateLast struct {
	Attempt string        `json:"attempt"`
	Version string        `json:"version"`
	At      time.Time     `json:"at"`
	Outcome UpdateOutcome `json:"outcome"`
	Reason  *UpdateReason `json:"reason"` // Only on failed.
	Message *string       `json:"message"`
}

// checkRecord is check.json. StartedAt is written before the fetch, so a check a restart cut short counts
// as one. A failure keeps what was offered.
type checkRecord struct {
	StartedAt time.Time     `json:"startedAt"`
	At        *time.Time    `json:"at"`
	Failure   *CheckFailure `json:"failure"`
	Found     *foundRelease `json:"found"`
}

type foundRelease struct {
	Version string `json:"version"`
	Core    string `json:"core"`
}

// updateFiles is the updater's files as read, one after another, so two readings compare. A file that
// isn't there reads empty.
type updateFiles struct {
	checkText, attemptText, endText string
	journal                         bool
}

func readUpdateFiles(dataDir string) updateFiles {
	text := func(path string) string {
		raw, _ := os.ReadFile(path)
		return string(raw)
	}
	f := updateFiles{checkText: text(checkFile(dataDir)), attemptText: text(attemptFile(dataDir))}
	if a, _ := f.attempt(); a != nil {
		f.endText = text(attemptOutcome(dataDir, a.Attempt))
	}
	_, err := os.Stat(journalDir(dataDir))
	f.journal = err == nil
	return f
}

func (f updateFiles) check() checkRecord {
	var record checkRecord
	if json.Unmarshal([]byte(f.checkText), &record) != nil {
		return checkRecord{}
	}
	return record
}

func (f updateFiles) attempt() (*updateAttempt, *updateEnd) {
	var a updateAttempt
	if json.Unmarshal([]byte(f.attemptText), &a) != nil || !attemptID.MatchString(a.Attempt) {
		return nil, nil
	}
	return &a, parseEnd(f.endText)
}

// superseded says the last attempt found another release the latest than the one the check offers.
func (f updateFiles) superseded() bool {
	c := f.check()
	a, end := f.attempt()
	return c.Found != nil && a != nil && end != nil && end.Reason != nil && *end.Reason == ReasonOfferChanged &&
		a.Version == c.Found.Version && !c.StartedAt.After(a.At)
}

// due says an automatic check may fetch: none started in the last day, by a clock that hasn't gone back since.
func (c checkRecord) due(now time.Time) bool {
	return c.StartedAt.After(now) || now.Sub(c.StartedAt) >= checkEvery
}

// updater is the check, the press and the job's state, for the daemon and for update over SSH.
type updater struct {
	dataDir  string
	build    Build
	release  *string // The release this process runs from.
	source   releaseSource
	lockPath string
	proc     string
	run      func(ctx context.Context, name string, args ...string) (string, error)
	free     func(path string) (uint64, error)
	now      func() time.Time
	lockWait time.Duration // How long a launch waits for another command's lock.

	mu       sync.Mutex
	fetching chan struct{} // Closed when the check that runs ends. Nil when none does.
	pressing sync.Mutex
}

func newUpdater(dataDir string, build Build, release *string) *updater {
	return &updater{dataDir: dataDir, build: build, release: release, source: newReleaseSource(build.Releases, nil),
		lockPath: lifecycleLockPath, proc: "/proc", run: runTool, free: freeSpace, now: time.Now, lockWait: 5 * time.Second}
}

// updates says this build takes updates: it is a release, and runs from releases/.
func (u *updater) updates() bool {
	_, ok := u.build.release()
	return ok && u.release != nil
}

// freeSpace is what a process may still write to path's filesystem.
func freeSpace(path string) (uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), nil
}

// steady reads the updater's files and, with between, whatever else is to be of the same moment. The daemon,
// a launcher and the job are separate processes, so each reads the others' files as they change, and between
// is run again until the files read the same before it and after it. No state of theirs comes back once it
// has gone, so all of it was then so together.
func (u *updater) steady(ctx context.Context, between func(updateFiles) error) (updateFiles, error) {
	for was := readUpdateFiles(u.dataDir); ; {
		if err := between(was); err != nil {
			return was, err
		}
		now := readUpdateFiles(u.dataDir)
		if now == was {
			return now, nil
		}
		was = now
		if err := ctx.Err(); err != nil {
			return was, err
		}
	}
}

func filesAlone(updateFiles) error { return nil }

// check asks the releases address for the latest release, unless an automatic one isn't due. Overlapping
// checks are one fetch, which outlives a caller that gives up.
func (u *updater) check(ctx context.Context, auto bool) error {
	u.mu.Lock()
	if u.fetching == nil {
		files, err := u.steady(ctx, filesAlone)
		record := files.check()
		if err != nil || auto && !record.due(u.now()) && !files.superseded() {
			u.mu.Unlock()
			return err
		}
		record.StartedAt = u.now()
		err = os.MkdirAll(updateDir(u.dataDir), 0o700)
		if err == nil {
			err = writeJSON(checkFile(u.dataDir), record)
		}
		if err != nil {
			u.mu.Unlock()
			return err
		}
		u.fetching = make(chan struct{})
		go u.fetch(record)
	}
	done := u.fetching
	u.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (u *updater) fetch(record checkRecord) {
	m, err := u.source.latest(context.Background())
	at := u.now()
	record.At, record.Failure = &at, nil
	if err != nil {
		log.Printf("the check for a newer release: %v", err)
		failure := checkFailureOf(err)
		record.Failure = &failure
	} else {
		record.Found = &foundRelease{Version: m.Version, Core: m.Core}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := writeJSON(checkFile(u.dataDir), record); err != nil {
		log.Printf("the check for a newer release isn't kept: %v", err)
	}
	close(u.fetching)
	u.fetching = nil
}

// offer is what the last check offers over the build that runs.
func (u *updater) offer(ctx context.Context, files updateFiles) *UpdateOffer {
	found := files.check().Found
	if found == nil || files.superseded() {
		return nil
	}
	return u.offerOf(ctx, found.Version, found.Core)
}

func (u *updater) offerOf(ctx context.Context, version, core string) *UpdateOffer {
	running, _ := u.build.release()
	next, ok := parseReleaseVersion(version)
	if !ok || !next.above(running) {
		return nil
	}
	restarts, err := u.restartsCore(ctx, core)
	if err != nil {
		// The job refuses it too, and says why.
		log.Printf("whether an update restarts the core: %v", err)
		restarts = true
	}
	return &UpdateOffer{Version: version, RestartsCore: restarts, Notes: u.build.Releases + "/tag/" + version}
}

// restartsCore is the upgrade's own answer for a release with this core.
func (u *updater) restartsCore(ctx context.Context, core string) (bool, error) {
	state, err := readInstallation(u.dataDir)
	if err != nil {
		return false, err
	}
	base, err := readSwitchBase(ctx, u.dataDir, u.proc, state)
	return base.restartsCore(core), err
}

// view is what api/update answers. Its error is systemd's: nothing says whether an update runs.
func (u *updater) view(ctx context.Context) (UpdateView, error) {
	view := UpdateView{Version: u.build.Version, Releases: u.updates()}
	if !view.Releases {
		return view, nil
	}
	guide := u.build.guide()
	view.Guide = &guide
	var running bool
	files, err := u.steady(ctx, func(files updateFiles) (err error) {
		running, err = u.running(ctx, files)
		if err == nil {
			view.Offer = u.offer(ctx, files)
		}
		return err
	})
	if err != nil {
		return view, err
	}
	record := files.check()
	view.Check = UpdateCheck{At: record.At, Failure: record.Failure}
	a, end := files.attempt()
	switch {
	case a == nil:
	case running:
		view.Running = &UpdateRunning{Attempt: a.Attempt, Version: a.Version}
	default:
		last := a.endOf(end, files.journal)
		view.Last = &UpdateLast{Attempt: a.Attempt, Version: a.Version, At: a.At, Outcome: last.Outcome, Reason: last.Reason, Message: last.Message}
	}
	return view, nil
}

// running says the attempt is still to end. Its launcher writes it before systemd has its job, and holds the
// lifecycle lock until it does. So while the lock is held, no job says nothing yet of an attempt that never
// started, and the lock is looked at first: once it is free, the launch has ended, and a job it made is there.
func (u *updater) running(ctx context.Context, files updateFiles) (bool, error) {
	launching := false
	if a, end := files.attempt(); a != nil && !a.Started && end == nil {
		var err error
		if launching, err = lifecycleHeld(u.lockPath); err != nil {
			return false, err
		}
	}
	job, err := u.job(ctx)
	return launching || job == jobRunning, err
}

// jobState is the update job's unit, as systemd has it.
type jobState uint8

const (
	jobGone    jobState = iota
	jobEnded            // Loaded and at rest, so --collect is about to remove it.
	jobRunning          // Queued, running, or being stopped.
)

func (u *updater) job(ctx context.Context) (jobState, error) {
	out, err := u.run(ctx, "systemctl", "show", updateUnit, "-p", "LoadState", "-p", "ActiveState", "-p", "Job")
	if err != nil {
		return jobGone, err
	}
	is := map[string]string{}
	for line := range strings.Lines(out) {
		name, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		is[name] = value
	}
	switch is["ActiveState"] {
	case "activating", "active", "reloading", "deactivating":
		return jobRunning, nil
	case "inactive", "failed":
	default:
		return jobGone, fmt.Errorf("systemd says %s is %q", updateUnit, is["ActiveState"])
	}
	switch {
	case is["Job"] != "" && is["Job"] != "0":
		return jobRunning, nil
	case is["LoadState"] == "not-found":
		return jobGone, nil
	}
	return jobEnded, nil
}

var errUpdateRunning = errors.New("an update is running")

// launch starts an update of this release to version as a systemd job, which outlives the daemon it
// restarts. It persists the attempt before the job can run, so neither a daemon's restart nor a job that
// never starts loses it, and holds the lifecycle lock over both, so nothing comes between them: the job
// waits for that lock. It answers with the attempt's id. A job that didn't start is an attempt that says so.
func (u *updater) launch(ctx context.Context, version string, restartsCore bool) (string, error) {
	lock, err := waitLifecycle(ctx, u.lockPath, u.lockWait)
	if outcomeOf(err) == outcomeBusy {
		if job, jerr := u.job(ctx); jerr == nil && job == jobRunning {
			return "", errUpdateRunning
		}
	}
	if err != nil {
		return "", err
	}
	defer dropLifecycle(lock, u.lockPath)
	job, err := u.job(ctx)
	for until := time.Now().Add(collectWait); err == nil && job == jobEnded && time.Now().Before(until); job, err = u.job(ctx) {
		time.Sleep(100 * time.Millisecond)
	}
	switch {
	case err != nil:
		return "", err
	case job == jobRunning:
		return "", errUpdateRunning
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	a := updateAttempt{Attempt: hex.EncodeToString(raw), Version: version, Expect: *u.release, RestartsCore: restartsCore, At: u.now()}
	if err := os.MkdirAll(attemptStaged(u.dataDir, a.Attempt), 0o700); err != nil {
		return "", err
	}
	if err := writeJSON(attemptFile(u.dataDir), a); err != nil {
		return "", err
	}
	dropAttempts(u.dataDir, a.Attempt)
	binary := filepath.Join(releaseDir(u.dataDir, a.Expect), bundleDaemon)
	if _, err := u.run(ctx, "systemd-run", jobLine(u.dataDir, binary, a.Attempt)...); err != nil {
		if job, jerr := u.job(context.WithoutCancel(ctx)); jerr != nil || job == jobRunning {
			return a.Attempt, jerr
		}
		log.Printf("the update to %s didn't start: %v", version, err)
		os.RemoveAll(attemptStaged(u.dataDir, a.Attempt))
		return a.Attempt, writeJSON(attemptOutcome(u.dataDir, a.Attempt), updateFailure{ReasonNoStart, fmt.Errorf("the update didn't start: %w", err)}.end())
	}
	return a.Attempt, nil
}

// dropAttempts removes every attempt's directory but one's.
func dropAttempts(dataDir, keep string) {
	entries, _ := os.ReadDir(updateDir(dataDir))
	for _, entry := range entries {
		if attemptID.MatchString(entry.Name()) && entry.Name() != keep {
			os.RemoveAll(attemptDir(dataDir, entry.Name()))
		}
	}
}

// jobLine is the job's systemd-run arguments. Its type is simple, which RuntimeMaxSec acts on. The two
// stop-posts run however the job ends, a SIGKILL included, once every process of it is gone: the first
// removes what it staged, and the second is the journal's own sidegated, which is there only while a switch
// is unfinished. systemd skips one whose binary isn't there. It expands $ in an argument and not in an
// executable, and the words it takes a path as carry every character a data dir may have.
func jobLine(dataDir, binary, attempt string) []string {
	arg := func(path string) string { return strings.ReplaceAll(path, "$", "$$") }
	word := func(path string) string { return `"` + path + `"` }
	seconds := func(d time.Duration) string { return fmt.Sprintf("%ds", int(d.Seconds())) }
	return []string{"--unit=" + strings.TrimSuffix(updateUnit, ".service"), "--collect", "--no-block", "--quiet",
		"-p", "RuntimeMaxSec=" + seconds(updateRunLimit), "-p", "TimeoutStopSec=" + seconds(updateStopLimit),
		"-p", "ExecStopPost=-/bin/rm -rf " + word(arg(attemptStaged(dataDir, attempt))),
		"-p", "ExecStopPost=-" + word(journalBinary(dataDir)) + " settle-update --data-dir " + word(arg(dataDir)),
		"--", binary, "apply-update", "--data-dir", arg(dataDir), "--attempt", attempt}
}

// waitLifecycle takes the lifecycle lock, and waits as long as limit for another command to let it go. A
// limit of 0 waits until ctx ends.
func waitLifecycle(ctx context.Context, path string, limit time.Duration) (*os.File, error) {
	until := time.Now().Add(limit)
	for {
		lock, err := holdLifecycle(path, 0)
		if outcomeOf(err) != outcomeBusy || limit > 0 && !time.Now().Before(until) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// lifecycleHeld says whether a command holds the lifecycle lock. Its file is there only while one does, or
// after one was killed, and only then is the lock itself tried, so a look gets in no command's way.
func lifecycleHeld(path string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	lock, err := holdLifecycle(path, 0)
	if outcomeOf(err) == outcomeBusy {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, dropLifecycle(lock, path)
}

// press starts the update the owner confirmed, as the last check offers it. One that runs already is the
// answer, so two presses are one update.
func (u *updater) press(ctx context.Context, version string, restartsCore bool) (*UpdateView, *Answer) {
	if !u.updates() {
		return nil, noReleases()
	}
	u.pressing.Lock()
	defer u.pressing.Unlock()
	view, err := u.view(ctx)
	if err != nil {
		log.Printf("update: %v", err)
		return nil, unavailable()
	}
	switch offer := view.Offer; {
	case view.Running != nil:
		return &view, nil
	case offer == nil:
		return nil, invalid(RefusedNothingNewer, "No newer release is offered.")
	case offer.Version != version || offer.RestartsCore != restartsCore:
		return nil, invalid(RefusedOfferChanged, "That isn't what is offered now.")
	}
	// A press the owner left still starts whole, or not at all.
	_, err = u.launch(context.WithoutCancel(ctx), version, restartsCore)
	switch {
	case outcomeOf(err) == outcomeBusy:
		return nil, webRefusal(http.StatusConflict, string(KindCapacity), RefusedLifecycle, "Another install, upgrade or uninstall is running.")
	case err != nil && !errors.Is(err, errUpdateRunning):
		log.Printf("update to %s: %v", version, err)
		return nil, unavailable()
	}
	if view, err = u.view(ctx); err != nil {
		log.Printf("update: %v", err)
		return nil, unavailable()
	}
	return &view, nil
}
