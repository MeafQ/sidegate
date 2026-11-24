package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// The journal of a switch is the directory rollback/ in the data dir. record.json says what the
// switch changes, as it was, and sidegated is the binary that wrote it. The daemon unit's pre-start
// runs that binary's recover at every start, so a release that doesn't start, or a prior release
// whose own binary is broken, can't stop it, and the code that reads the record is the code that
// wrote it. The directory is built under rollback.new and renamed into place, and goes by one rename
// to rollback.old, so a kill leaves it whole or gone. It exists only while a switch is unfinished.
func journalDir(dataDir string) string { return filepath.Join(dataDir, "rollback") }

func journalRecord(dataDir string) string { return filepath.Join(journalDir(dataDir), "record.json") }

// Where a journal whose record doesn't parse stays, out of the way, until a release answers.
func asideJournal(dataDir string) string { return journalDir(dataDir) + ".unread" }

// The recovery entry point, which the unit's pre-start and the owner over SSH both run.
func journalBinary(dataDir string) string { return filepath.Join(journalDir(dataDir), bundleDaemon) }

// startedMark is there once systemd has started the daemon since the put-back was restored. The
// unit's main process makes it, since nothing that runs before that can say it happened.
func startedMark(dataDir string) string { return filepath.Join(journalDir(dataDir), "started") }

// phase is how far a switch got. Each is written before what it allows, so a kill leaves a phase whose
// steps, run again from the first, finish it, and nothing else says which of them ran.
type phase string

const (
	phasePlanned    phase = "planned"    // Nothing the gateway runs from changed. Nothing is owed.
	phaseSwitching  phase = "switching"  // The switch may have begun, and goes back unless it commits.
	phaseRestoring  phase = "restoring"  // Putting back began, and all of it is owed.
	phaseRestored   phase = "restored"   // All of it is back. Only the prior release's answer is owed.
	phaseRolledBack phase = "rolledBack" // The prior release answered. Saying so is owed.
	phaseCommitted  phase = "committed"  // The candidate answered, and stays. Saying so is owed.
)

var phases = []phase{phasePlanned, phaseSwitching, phaseRestoring, phaseRestored, phaseRolledBack, phaseCommitted}

// ended says the release the switch ended on answered, so nothing is owed but saying so.
func (p phase) ended() bool { return p == phaseRolledBack || p == phaseCommitted }

// was is what a path held: a link's target, a file's text, or nothing.
type was struct {
	Link *string `json:"link,omitempty"`
	Text *string `json:"text,omitempty"`
}

func readWas(path string) (was, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return was{}, nil
	case err != nil:
		return was{}, err
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		return was{Link: &target}, err
	case info.Mode().IsRegular():
		raw, err := os.ReadFile(path)
		text := string(raw)
		return was{Text: &text}, err
	}
	return was{}, fmt.Errorf("%s is neither a link nor a file", path)
}

func (w was) same(other was) bool {
	equal := func(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return equal(w.Link, other.Link) && equal(w.Text, other.Text)
}

func (w was) putBack(path string) error {
	switch {
	case w.Link != nil:
		return symlinkAtomic(*w.Link, path)
	case w.Text != nil:
		return writeFileAtomic(path, []byte(*w.Text), 0o644)
	}
	return removeIfPresent(path)
}

// rollbackRecord is record.json. Selected is the releases each switch under it selected: a
// switch cut short and tried again, perhaps to another bundle, adds its own, and puts back the same
// release, the one that last answered. CoreRequired is decided once, when the first of those switches
// was planned: the core ran then and the stored configuration needed it, so the switch, a retry over it
// and a put-back must each end with a core that stays up. A core that was already down is nobody's to
// wait for. Boot is the boot the last of them began in, the only one whose core a put-back touches. None
// of it binds anything until a switch under it began.
type rollbackRecord struct {
	Phase        phase          `json:"phase"`
	Selected     []string       `json:"selected"`
	Links        map[string]was `json:"links"`
	Units        map[string]was `json:"units"`
	Outcome      string         `json:"outcome,omitempty"`
	Boot         string         `json:"boot"`
	CoreRequired bool           `json:"coreRequired"`
}

// readJournal reads the record's bytes. A journal without one has none, and any other failure to
// read says nothing about what they are.
func readJournal(dataDir string) ([]byte, error) {
	raw, err := os.ReadFile(journalRecord(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

func parseRecord(dataDir string, raw []byte) (rollbackRecord, error) {
	var record rollbackRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, fmt.Errorf("the rollback record: %w", err)
	}
	if _, named := record.Links[filepath.Join(dataDir, "current")]; !named || len(record.Selected) == 0 || !slices.Contains(phases, record.Phase) {
		return record, errors.New("the rollback record is incomplete")
	}
	return record, nil
}

func readRecord(dataDir string) (rollbackRecord, error) {
	raw, err := readJournal(dataDir)
	if err != nil {
		return rollbackRecord{}, err
	}
	return parseRecord(dataDir, raw)
}

// prior is the release the record puts back, empty when nothing was current.
func (r rollbackRecord) prior(dataDir string) string {
	if link := r.Links[filepath.Join(dataDir, "current")].Link; link != nil {
		return filepath.Base(*link)
	}
	return ""
}

// began says a switch under the record may have changed what the gateway runs from. One to another
// release changes none of it before it selects that release, so until then the record is only a plan, as
// it is while planned, and a later command decides everything as if it weren't there.
func (r rollbackRecord) began(dataDir string) bool {
	switch r.Phase {
	case phasePlanned:
		return false
	case phaseSwitching:
		return slices.Contains(r.Selected, currentRelease(dataDir))
	}
	return true
}

// switchBegan says the journal holds a switch that began, or may: a record that doesn't read can say anything.
func switchBegan(dataDir string) bool {
	record, err := readRecord(dataDir)
	return err != nil || record.began(dataDir)
}

func (r rollbackRecord) candidate() string { return r.Selected[len(r.Selected)-1] }

// back says current is what the record holds for it.
func (r rollbackRecord) back(dataDir string) bool {
	current := filepath.Join(dataDir, "current")
	now, err := readWas(current)
	return err == nil && now.same(r.Links[current])
}

// say writes how the switch ended where its upgrade was told to. A directory that is gone, as one a
// reboot or a later update removed, has nobody reading it, and the switch ends all the same.
func (r rollbackRecord) say(ended outcome, message string) error {
	if r.Outcome == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Dir(r.Outcome)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if ended == outcomeLanded {
		return writeOutcome(r.Outcome, nil)
	}
	return writeOutcome(r.Outcome, outcomeError{ended, errors.New(message)})
}

func (r rollbackRecord) write(dataDir string) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeFileAtomic(journalRecord(dataDir), append(data, '\n'), 0o600)
}

// enter writes that the switch is in p. A start counts for a put-back only once it is restored.
func (r *rollbackRecord) enter(dataDir string, p phase) error {
	if r.Phase == p {
		return nil
	}
	if p == phaseRestored {
		if err := removeIfPresent(startedMark(dataDir)); err != nil {
			return err
		}
	}
	r.Phase = p
	return r.write(dataDir)
}

// end says how the switch ended and forgets it. A kill part way leaves the ended phase, and the next
// start says the same.
func (r *rollbackRecord) end(dataDir string, p phase) error {
	if err := r.enter(dataDir, p); err != nil {
		return err
	}
	ended, message := outcomeLanded, ""
	if p == phaseRolledBack {
		ended = outcomeRolledBack
		message = fmt.Sprintf("the switch to release %s didn't finish. Release %s is back and answers", r.candidate(), r.prior(dataDir))
	}
	if err := r.say(ended, message); err != nil {
		return err
	}
	return removeJournal(dataDir)
}

// remember adds what path holds now, unless into already holds what it was.
func remember(into map[string]was, path string) error {
	if _, held := into[path]; held {
		return nil
	}
	now, err := readWas(path)
	into[path] = now
	return err
}

// recordSwitch publishes the record with binary beside it, before a switch. A journal already there
// keeps its directory, and takes the binary and then the record, each by a rename.
func recordSwitch(dataDir, binary string, record rollbackRecord) error {
	dir := journalDir(dataDir)
	if _, err := os.Stat(dir); err == nil {
		if err := copyFile(binary, journalBinary(dataDir)); err != nil {
			return err
		}
		return record.write(dataDir)
	}
	staged := dir + ".new"
	if err := errors.Join(os.RemoveAll(staged), os.RemoveAll(dir+".old")); err != nil {
		return err
	}
	if err := os.Mkdir(staged, 0o700); err != nil {
		return err
	}
	if err := copyFile(binary, filepath.Join(staged, bundleDaemon)); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(staged, "record.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(staged, dir); err != nil {
		return err
	}
	return syncDir(dataDir)
}

func copyFile(from, to string) error {
	file, err := os.Open(from)
	if err != nil {
		return err
	}
	defer file.Close()
	return copyAtomic(to, file, 0o755)
}

// begins is the first step past the boundary, with the record saying so before it changes anything.
func (r *rollbackRecord) begins(dataDir string, first lifecycleStep) lifecycleStep {
	return lifecycleStep{first.say + ", once the record says the switch began", func(ctx context.Context) error {
		if err := r.enter(dataDir, phaseSwitching); err != nil {
			return err
		}
		return first.do(ctx)
	}}
}

// removeJournal forgets a switch once a release has answered, and any journal set aside before it.
func removeJournal(dataDir string) error {
	if err := dropJournal(dataDir); err != nil {
		return err
	}
	return os.RemoveAll(asideJournal(dataDir))
}

// dropJournal takes the journal away in one rename, then deletes it.
func dropJournal(dataDir string) error {
	dir := journalDir(dataDir)
	gone := dir + ".old"
	if err := os.RemoveAll(gone); err != nil {
		return err
	}
	if err := os.Rename(dir, gone); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(dataDir); err != nil {
		return err
	}
	return os.RemoveAll(gone)
}

// setJournalAside moves a journal whose record doesn't parse out of the way of the next one, and says
// whether it moved.
func setJournalAside(dataDir string) (bool, error) {
	if err := os.RemoveAll(asideJournal(dataDir)); err != nil {
		return false, err
	}
	if err := os.Rename(journalDir(dataDir), asideJournal(dataDir)); err != nil {
		return false, err
	}
	return true, syncDir(dataDir)
}

// bringJournalBack undoes setJournalAside. Only the command that moved it may, before its switch and
// under the lock it still holds: any other journal there is then that command's own, and no release
// has landed since.
func bringJournalBack(dataDir string) error {
	if err := dropJournal(dataDir); err != nil {
		return err
	}
	if err := os.Rename(asideJournal(dataDir), journalDir(dataDir)); err != nil {
		return err
	}
	return syncDir(dataDir)
}

// putBack puts back all the record holds, whatever it looks like now and whatever an earlier run put
// back, and leaves the record saying all of it is owed. current goes first, so nothing that fails after
// it leaves a candidate selected. The candidate itself is left as it is: nothing runs from it then.
func putBack(dataDir string) error {
	record, err := readRecord(dataDir)
	if err != nil {
		return err
	}
	if err := record.enter(dataDir, phaseRestoring); err != nil {
		return err
	}
	current := filepath.Join(dataDir, "current")
	if err := record.Links[current].putBack(current); err != nil {
		return err
	}
	var errs []error
	for _, held := range []map[string]was{record.Units, record.Links} {
		for _, path := range slices.Sorted(maps.Keys(held)) {
			errs = append(errs, held[path].putBack(path))
		}
	}
	return errors.Join(errs...)
}

func reloadUnits(ctx context.Context) error {
	_, err := runTool(ctx, "systemctl", "daemon-reload")
	return err
}

// steadiness is how long a core must stay up, and how long it may take to.
type steadiness struct{ settle, limit time.Duration }

var coreSteadiness = steadiness{settle: 5 * time.Second, limit: 15 * time.Second}

// bootID is empty where the kernel doesn't say, as off Linux.
func bootID(proc string) string {
	raw, _ := os.ReadFile(filepath.Join(proc, "sys", "kernel", "random", "boot_id"))
	return strings.TrimSpace(string(raw))
}

// coreProcess is the core's MainPID, 0 when nothing runs, and the digest of the core it runs.
func coreProcess(ctx context.Context, proc string) (int, liveCore, error) {
	out, err := runTool(ctx, "systemctl", "show", gateway.CoreUnitName, "-p", "MainPID")
	if err != nil {
		return 0, coreUnknown, err
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(out), "MainPID="))
	if err != nil {
		return 0, coreUnknown, fmt.Errorf("systemd's MainPID %q: %w", strings.TrimSpace(out), err)
	}
	if pid == 0 {
		return 0, coreStopped, nil
	}
	exe, err := os.Readlink(filepath.Join(proc, strconv.Itoa(pid), "exe"))
	if err != nil {
		return pid, coreUnknown, nil
	}
	return pid, liveCore(filepath.Base(filepath.Dir(exe))), nil
}

// waitCoreSteady waits for one process of core digest to stay up for the settle time.
func waitCoreSteady(ctx context.Context, proc, digest string, steady steadiness) error {
	deadline := time.Now().Add(steady.limit)
	since, seen := time.Time{}, 0
	for {
		pid, runs, err := coreProcess(ctx, proc)
		switch {
		case err != nil || pid == 0 || string(runs) != digest:
			seen = 0
		case pid != seen:
			since, seen = time.Now(), pid
		}
		if seen != 0 && time.Since(since) >= steady.settle {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("core %s didn't stay up %s within %s", short(digest), steady.settle, steady.limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(250*time.Millisecond, time.Until(deadline))):
		}
	}
}

func short(digest string) string { return digest[:min(12, len(digest))] }

// coreOnto restarts a core that runs another binary than core digest, which current names by now. One
// whose binary can't be read is most likely on its way out, and is left to the wait.
func coreOnto(ctx context.Context, proc, digest string) (liveCore, error) {
	_, runs, err := coreProcess(ctx, proc)
	if err != nil || runs == coreStopped || runs == coreUnknown || string(runs) == digest {
		return runs, err
	}
	_, err = runTool(ctx, "systemctl", "try-restart", gateway.CoreUnitName)
	return runs, err
}

// switchCore puts the upgrade's core to work. A core the record requires must come to run it and stay
// up: the daemon, which answers by now, starts one that doesn't run. With none required nothing about
// what runs fails the upgrade, and a core that is new to the gateway must only be the pinned one on disk.
func switchCore(ctx context.Context, dataDir, proc, digest string, required, isNew bool, steady steadiness) error {
	_, err := coreOnto(ctx, proc, digest)
	if required {
		if err != nil {
			return err
		}
		return waitCoreSteady(ctx, proc, digest, steady)
	}
	if got := fileSHA(filepath.Join(dataDir, "current", bundleCore)); isNew && got != digest {
		return fmt.Errorf("the core current names has digest %q, not %s", got, digest)
	}
	return nil
}

// coreRequired decides, before anything is touched, whether a core must be up once the switch is over:
// one runs now, and the stored configuration needs it. A core that runs over a configuration that can't
// be read is taken to be needed.
func coreRequired(ctx context.Context, dataDir string, live liveCore) bool {
	if live == coreStopped {
		return false
	}
	needed, err := coreNeeded(ctx, dataDir)
	return needed || err != nil
}

// coreNeeded says whether the stored slots and sources give the core anything to run, as
// controller.desired plans it: a probe route of an Exit in use, a server a slot still serves on, or a
// slot off the VPN whose VPN Client is there when UniFi makes them.
func coreNeeded(ctx context.Context, dataDir string) (bool, error) {
	b, err := LoadBootstrap(dataDir)
	if err != nil {
		return false, err
	}
	state, world, err := storedState(ctx, dataDir)
	if err != nil || len(world.routes) > 0 {
		return len(world.routes) > 0, err
	}
	syncer, _ := openSyncer(b)
	for _, row := range state.slots {
		ref, serving := row.Selection.Server()
		l, known := world.catalogue.Lookup(ref.Lineage)
		switch {
		case row.Deleting:
		case serving && known && l.Ref == ref && l.Presence != domain.Tombstone:
			return true, nil
		case row.Selection.OffVPN() && (syncer == nil || row.Slot.Allocation.NetworkID != ""):
			return true, nil
		}
	}
	return false, nil
}

// coreBack makes the core run the core current names again, within the boot the record was written in:
// one running another core restarts, and a required one that runs no more starts. After a reboot nothing
// of then runs, and the daemon starts the one it needs.
func coreBack(ctx context.Context, dataDir, proc string) error {
	record, err := readRecord(dataDir)
	if err != nil || record.Boot != bootID(proc) {
		return err
	}
	runs, err := coreOnto(ctx, proc, currentCore(dataDir))
	if err != nil || runs != coreStopped || !record.CoreRequired {
		return err
	}
	// A core that kept failing may have reached its start limit.
	runTool(ctx, "systemctl", "reset-failed", gateway.CoreUnitName)
	_, err = runTool(ctx, "systemctl", "start", gateway.CoreUnitName)
	return err
}

// waitCoreBack waits for a required core to stay up on the core current names, whatever it looks
// like now: one an earlier run restarted and didn't wait out is owed the wait again.
func waitCoreBack(ctx context.Context, dataDir, proc string, steady steadiness) error {
	record, err := readRecord(dataDir)
	if err != nil || record.Boot != bootID(proc) || !record.CoreRequired {
		return err
	}
	return waitCoreSteady(ctx, proc, currentCore(dataDir), steady)
}

// planRecover is what the unit's pre-start runs at every start, and the owner over SSH: it finishes
// what the journal says an install or upgrade cut short left. It never restarts or waits for the
// daemon it runs before, and does nothing under a lifecycle command, which owns its own rollback.
//
//   - A switch that ended, on either release, says so again, and the record goes.
//   - A switch whose current names one of its candidates goes back: current first, then every link and
//     unit as it was, a reload, and the core back within the same boot. A put-back that began and
//     didn't finish does all of it again, whatever current names, and is restored only once all of it
//     has run. The put-back release's daemon says rolledBack once it runs, so the record stays for it.
//   - A restored release owes only its answer. Once systemd has started its daemon, another start
//     before that answer says putBackFailed. Until then nothing has failed, and nothing is said.
//   - A switch that never began changed nothing the gateway runs from: refused, and the record goes.
func planRecover(request LifecycleRequest) (LifecyclePlan, error) {
	dataDir := request.DataDir
	plan := LifecyclePlan{Action: Recover}
	if _, err := os.Stat(journalDir(dataDir)); errors.Is(err, os.ErrNotExist) {
		return plan, nil
	}
	record, err := readRecord(dataDir)
	if err != nil {
		return plan, err
	}
	candidate, prior := record.candidate(), record.prior(dataDir)
	plan.Prior = prior
	forget := lifecycleStep{"forget the record", func(context.Context) error { return dropJournal(dataDir) }}
	tell := func(ended outcome, message string) lifecycleStep {
		return lifecycleStep{fmt.Sprintf("say %s: %s", ended, message), func(context.Context) error { return record.say(ended, message) }}
	}
	switch {
	case record.Phase.ended():
		plan.steps = []lifecycleStep{{fmt.Sprintf("say how the switch to release %s ended: %s. Then forget it", candidate, record.Phase), func(context.Context) error {
			return record.end(dataDir, record.Phase)
		}}}
	case !record.began(dataDir):
		plan.steps = []lifecycleStep{tell(outcomeRefused, fmt.Sprintf("the upgrade to release %s stopped before its switch, and changed nothing the gateway runs from", candidate)), forget}
	case record.Phase == phaseRestored && record.back(dataDir):
		if _, err := os.Stat(startedMark(dataDir)); err != nil {
			break
		}
		message := fmt.Sprintf("the switch to release %s didn't finish. Release %s is back and was started, but hasn't said it answers", candidate, prior)
		if said, _ := os.ReadFile(record.Outcome); !bytes.Contains(said, []byte(string(outcomePutBackFailed))) {
			plan.steps = []lifecycleStep{tell(outcomePutBackFailed, message)}
		}
	case slices.Contains(record.Selected, currentRelease(dataDir)), record.Phase == phaseRestoring && record.back(dataDir):
		goes := "nothing is current again"
		if prior != "" {
			goes = "release " + prior + " is current again"
		}
		plan.steps = []lifecycleStep{
			{fmt.Sprintf("put back all the record holds, current first: %s, with its core link and the units", goes), func(context.Context) error { return putBack(dataDir) }},
			{"reload systemd's units", reloadUnits},
			{"put the core back on the core current names, in the boot of the switch: restart it if it runs another, start it if the switch required it and it runs no more", func(ctx context.Context) error {
				return coreBack(ctx, dataDir, request.proc)
			}},
			{"wait for a core the switch required to stay up", func(ctx context.Context) error {
				return waitCoreBack(ctx, dataDir, request.proc, request.steady)
			}},
		}
		if prior == "" {
			plan.steps = append(plan.steps, forget)
		} else {
			plan.steps = append(plan.steps, lifecycleStep{fmt.Sprintf("record that all of it is back, for release %s's daemon to say so once it answers", prior), func(context.Context) error {
				record, err := readRecord(dataDir)
				if err != nil {
					return err
				}
				return record.enter(dataDir, phaseRestored)
			}})
		}
		for i, s := range plan.steps {
			plan.steps[i].do = func(ctx context.Context) error {
				err := s.do(ctx)
				if err != nil {
					err = errors.Join(err, record.say(outcomePutBackFailed, fmt.Sprintf("the switch to release %s didn't finish, and putting back failed at %s: %v", candidate, s.say, err)))
				}
				return err
			}
		}
	default:
		plan.steps = []lifecycleStep{tell(outcomePutBackFailed, fmt.Sprintf("current names release %s, which the switch to release %s neither selected nor put back", currentRelease(dataDir), candidate)), forget}
	}
	return plan, nil
}

// ConfirmPutBack is the put-back release's daemon saying it answers, once its socket listens: a switch
// recover put back is rolledBack only then. No other process says it, so it is owed until it is said or
// another command ends the switch: a try that met the lock held, as the update's stop-post holds it to look
// at the record, or that couldn't write, is made again until ctx ends.
func ConfirmPutBack(ctx context.Context, dataDir, lockPath string, running *string) {
	for logged := ""; ; {
		err := confirmPutBack(dataDir, lockPath, running)
		if err != nil && outcomeOf(err) != outcomeBusy && err.Error() != logged {
			logged = err.Error()
			log.Printf("the put-back switch's record: %v", err)
		}
		if owed := (owedError{}); !errors.As(err, &owed) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(confirmPause):
		}
	}
}

const confirmPause = time.Second

// owedError is a try that left the put-back still to be said.
type owedError struct{ err error }

func (e owedError) Error() string { return e.err.Error() }
func (e owedError) Unwrap() error { return e.err }

// confirmPutBack is one try. Under a lifecycle command it says nothing, since that command owns the record.
func confirmPutBack(dataDir, lockPath string, running *string) (err error) {
	if _, err := os.Stat(journalDir(dataDir)); err != nil {
		return nil
	}
	lock, err := holdLifecycle(lockPath, 0)
	if err != nil {
		return owedError{err}
	}
	defer func() { err = errors.Join(err, dropLifecycle(lock, lockPath)) }()
	record, err := readRecord(dataDir)
	if err != nil {
		return err
	}
	prior := record.prior(dataDir)
	answers := record.Phase == phaseRestored || record.Phase == phaseRolledBack // An earlier try may have got that far.
	if !answers || prior == "" || !record.back(dataDir) || running == nil || *running != prior {
		return nil
	}
	if err := record.end(dataDir, phaseRolledBack); err != nil {
		return owedError{err}
	}
	return nil
}
