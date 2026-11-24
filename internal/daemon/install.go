package daemon

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"sidegate/deploy"
	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

// Action is a lifecycle command.
type Action uint8

const (
	Install Action = iota + 1
	Upgrade
	Uninstall
	Recover
)

func (a Action) String() string {
	return map[Action]string{Install: "install", Upgrade: "upgrade", Uninstall: "uninstall", Recover: "recover"}[a]
}

const (
	daemonUnitName    = "sidegated.service"
	defaultUnitDir    = "/etc/systemd/system"
	defaultHookDir    = "/data/unifi-core/config/http"
	networkKeyName    = "network-key"
	bundleDaemon      = "sidegated"
	bundleCore        = "xray"
	maxBundleFile     = 256 << 20
	maxAPIKey         = 4 << 10
	teardownLimit     = 90 * time.Second
	answeringLimit    = 45 * time.Second
	unifiInspectLimit = 20 * time.Second
	rollbackLimit     = 2 * time.Minute
	lifecycleLockPath = "/run/sidegate-lifecycle.lock"
)

// keptWithoutPurge is the owner's saved work. Uninstall leaves it unless asked: sources
// and WireGuard keys in state.db, and the API key.
var keptWithoutPurge = []string{stateName, stateName + "-wal", stateName + "-shm", networkKeyName}

// pinnedCore is a variable only so a test can bundle a stand-in core.
var pinnedCore = deploy.XraySHA256

// LifecycleRequest is what the operator asked for. The unexported fields are the places a
// test points somewhere harmless; PlanLifecycle fills in the gateway's.
type LifecycleRequest struct {
	Action     Action
	DataDir    string
	BundlePath string
	Build      Build     // What this sidegated was linked as, which is the bundle's, whose own it is.
	Purge      bool      // Uninstall also removes saved sources, keys and owned VPN Clients.
	APIKey     io.Reader // Where install reads UniFi's API key from: stdin, never an argument.
	Out        io.Writer
	// Upgrade only. Expect refuses with moved unless it is the current release, and RestartsCore with
	// planChanged unless the plan agrees, so a plan the owner confirmed elsewhere is the plan that runs.
	Expect       string
	RestartsCore *bool
	LockFD       int    // The caller holds the lifecycle lock on this descriptor. 0 takes it here.
	Outcome      string // RunLifecycle writes how it ended here, for the updater to read.

	lockPath  string
	unitDir   string
	hookDir   string
	teardown  func(context.Context, Bootstrap) error
	syncer    unifi.Syncer
	answering time.Duration
	proc      string
	steady    steadiness
	// The sidegated a switch's journal carries to put it back: this process's own binary.
	recoveryBinary string
}

// installation is what the data dir holds between lifecycle commands, read once before each plans:
// nothing (no release), a release, or a release with the journal of a switch a kill interrupted.
// A current that names a directory without its daemon, or can't be read, is no release, since
// nothing could run from it or be put back to it.
// Install and upgrade end with no record, and a failed upgrade puts back the release it names.
// Install reads of the record only whether its switch began. An upgrade sets aside bytes that don't parse
// as one, so a journal a disk fault broke stops neither, and refuses to go past bytes it can't read.
type installation struct {
	release     string
	interrupted bool
}

func readInstallation(dataDir string) (installation, error) {
	state := installation{release: currentRelease(dataDir)}
	if daemon, err := os.Stat(filepath.Join(dataDir, "current", bundleDaemon)); err != nil || !daemon.Mode().IsRegular() {
		state.release = ""
	}
	_, err := os.Stat(journalDir(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	state.interrupted = err == nil
	return state, err
}

type lifecycleStep struct {
	say string
	do  func(context.Context) error
}

// LifecyclePlan is the steps themselves, so what is printed is what runs. PlanLifecycle only
// reads. Every step converges, so a crash is repaired by running the command again.
//
// A plan with a rollback has one boundary. steps[:boundary] change nothing the gateway runs or
// selects, so a failure there, or a cancel seen before steps[boundary], removes what they staged and
// ends refused. A failure in steps[boundary:answered] runs the whole rollback, whatever the failed
// step had done, and one after them ends landedWithError. A plan with no rollback stops at its
// first failure, and a cancel stops it before the next step.
type LifecyclePlan struct {
	Action       Action
	Release      string
	RestartsCore bool // Drops every slot's flows, so the owner sees it before saying go.
	Routes       []string
	steps        []lifecycleStep
	boundary     int
	answered     int
	unstage      func() error // Removes what steps[:boundary] staged.
	Prior        string       // The release the rollback selects again; empty when nothing was.
	rollback     []lifecycleStep
	back         string         // What the outcome says once every rollback step has run.
	tidy         *lifecycleStep // Runs after the rollback, which stands whether or not it fails.
}

func (p LifecyclePlan) Print(out io.Writer) {
	fmt.Fprintf(out, "%s", p.Action)
	if p.Release != "" {
		fmt.Fprintf(out, " release %s", p.Release)
	}
	fmt.Fprintln(out, ":")
	for i, s := range p.steps {
		fmt.Fprintf(out, "  %d. %s\n", i+1, s.say)
	}
	if p.Action == Recover && len(p.steps) == 0 {
		nothing := "no switch is unfinished"
		if p.Prior != "" {
			nothing = fmt.Sprintf("release %s is back, and only its daemon can say it answers", p.Prior)
		}
		fmt.Fprintf(out, "  nothing: %s\n", nothing)
	}
	if len(p.rollback) > 0 {
		goes := "nothing is current again"
		if p.Prior != "" {
			goes = fmt.Sprintf("release %s goes back", p.Prior)
		}
		fmt.Fprintf(out, "If step %d to %d fails, %s:\n", p.boundary+1, p.answered, goes)
		undo := p.rollback
		if p.tidy != nil {
			undo = append(slices.Clip(undo), *p.tidy)
		}
		for i, s := range undo {
			fmt.Fprintf(out, "  %c. %s\n", 'a'+i, s.say)
		}
	}
	if p.Action == Upgrade {
		if p.RestartsCore {
			fmt.Fprintln(out, "This bundle's core differs from the running one: restarting it drops every slot's flows.")
		} else {
			fmt.Fprintln(out, "The core stays running: no flow drops.")
		}
	}
	for _, line := range p.Routes {
		fmt.Fprintln(out, line)
	}
}

// PlanLifecycle reads the gateway and the bundle and changes nothing.
func PlanLifecycle(ctx context.Context, request LifecycleRequest) (LifecyclePlan, error) {
	// Uninstall deletes this directory, so it must be plainly ours.
	if !filepath.IsAbs(request.DataDir) || filepath.Base(request.DataDir) != "sidegate" {
		return LifecyclePlan{}, fmt.Errorf("data dir %q must be an absolute path ending in sidegate", request.DataDir)
	}
	if request.Out == nil {
		request.Out = io.Discard
	}
	if request.unitDir == "" {
		request.unitDir = defaultUnitDir
	}
	if request.hookDir == "" {
		request.hookDir = defaultHookDir
	}
	if request.teardown == nil {
		request.teardown = tearDownRuntime
	}
	if request.answering == 0 {
		request.answering = answeringLimit
	}
	if request.proc == "" {
		request.proc = "/proc"
	}
	if request.steady == (steadiness{}) {
		request.steady = coreSteadiness
	}
	if request.recoveryBinary == "" {
		binary, err := os.Executable()
		if err != nil {
			return LifecyclePlan{}, err
		}
		request.recoveryBinary = binary
	}
	switch request.Action {
	case Uninstall:
		return planUninstall(ctx, request)
	case Recover:
		return planRecover(request)
	case Install, Upgrade:
		if err := unitsCarry(request.DataDir); err != nil {
			return LifecyclePlan{}, err
		}
		state, err := readInstallation(request.DataDir)
		if err != nil {
			return LifecyclePlan{}, err
		}
		if request.Action == Install {
			return planInstall(request, state)
		}
		return planUpgrade(ctx, request, state)
	}
	return LifecyclePlan{}, fmt.Errorf("unknown lifecycle action %d", request.Action)
}

// outcome is how an applied lifecycle command ended. Every outcome but landed and landedWithError
// leaves the release that was current. busy, moved, planChanged and a refusal while planning
// change nothing.
type outcome string

const (
	outcomeLanded          outcome = "landed"
	outcomeRefused         outcome = "refused"
	outcomeRolledBack      outcome = "rolledBack"
	outcomePutBackFailed   outcome = "putBackFailed"
	outcomeLandedWithError outcome = "landedWithError" // The new daemon answers, and a step after that failed.
	outcomeBusy            outcome = "busy"
	outcomeMoved           outcome = "moved"
	outcomePlanChanged     outcome = "planChanged"
)

type outcomeError struct {
	outcome outcome
	err     error
}

func (e outcomeError) Error() string { return e.err.Error() }
func (e outcomeError) Unwrap() error { return e.err }

// outcomeOf reads an error RunLifecycle returned. One with no outcome of its own came from planning
// or a step before the switch, so nothing switched.
func outcomeOf(err error) outcome {
	var ended outcomeError
	switch {
	case err == nil:
		return outcomeLanded
	case errors.As(err, &ended):
		return ended.outcome
	}
	return outcomeRefused
}

func writeOutcome(path string, err error) error {
	ended := struct {
		Outcome outcome `json:"outcome"`
		Message *string `json:"message"`
	}{Outcome: outcomeOf(err)}
	if err != nil {
		message := err.Error()
		ended.Message = &message
	}
	data, merr := json.Marshal(ended)
	if merr != nil {
		return merr
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

// holdLifecycle holds the lock install, upgrade and uninstall run under, from planning to the last
// step, so no two of them, nor the updater, change the release at once. It is flock's and not
// fcntl's, so it belongs to the open file: the updater's copy and the copy its upgrade child gets on
// fd are one lock, and closing either releases nothing while the other is open. Nobody unlocks
// it, since that would release the other copy too: it goes when the last copy closes.
//
// A command that took it by its path removes the file as it lets go (dropLifecycle), so the file
// another opened may be gone or replaced by the time it locks it. Only the file at the path is the
// lock: a taker that locked any other opens the path again.
func holdLifecycle(path string, fd int) (*os.File, error) {
	if fd != 0 {
		// An inherited descriptor survives exec, and a tool this run starts and leaves running
		// would hold the lock after it.
		syscall.CloseOnExec(fd)
		lock := os.NewFile(uintptr(fd), path)
		held, err := lock.Stat()
		want, werr := os.Stat(path)
		if err = errors.Join(err, werr); err == nil && !os.SameFile(held, want) {
			err = errors.New("it is another file")
		}
		if err != nil {
			lock.Close()
			return nil, fmt.Errorf("descriptor %d isn't the lifecycle lock %s: %w", fd, path, err)
		}
		if err := flockLifecycle(lock, path); err != nil {
			return nil, err
		}
		return lock, nil
	}
	for {
		lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("the lifecycle lock: %w", err)
		}
		lockOpened()
		if err := flockLifecycle(lock, path); err != nil {
			return nil, err
		}
		held, err := lock.Stat()
		at, aerr := os.Stat(path)
		if err == nil && aerr == nil && os.SameFile(held, at) {
			return lock, nil
		}
		lock.Close()
		if err := errors.Join(err, aerr); !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("the lifecycle lock: %w", err)
		}
	}
}

// lockOpened runs between opening the lock's file and locking it. It is a variable only so a test
// can run an uninstall there.
var lockOpened = func() {}

func flockLifecycle(lock *os.File, path string) error {
	err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	lock.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return outcomeError{outcomeBusy, fmt.Errorf("another install, upgrade or uninstall holds %s", path)}
	}
	return fmt.Errorf("the lifecycle lock: %w", err)
}

// dropLifecycle lets go of a lock this process took by its path and handed to no one. It removes
// the file while it still holds it, so an uninstall leaves nothing in /run, and the next command
// makes the file again.
func dropLifecycle(lock *os.File, path string) error {
	defer lock.Close()
	return removeIfPresent(path)
}

// sameFile compares files, not spellings, so a hard link or another path to a file is that file. A
// path nothing is at yet is another's when both name one entry in one directory.
func sameFile(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	ad, aerr := os.Stat(filepath.Dir(a))
	bd, berr := os.Stat(filepath.Dir(b))
	return aerr == nil && berr == nil && os.SameFile(ad, bd) && filepath.Base(a) == filepath.Base(b)
}

// RunLifecycle is the operator's yes. It plans under the lifecycle lock, so what it checked before
// planning still holds at the last step, prints the plan, and runs it.
func RunLifecycle(ctx context.Context, request LifecycleRequest) (err error) {
	if request.Out == nil {
		request.Out = io.Discard
	}
	if request.lockPath == "" {
		request.lockPath = lifecycleLockPath
	}
	if request.Outcome != "" {
		// Writing the outcome renames a new file over its path, which would split the lock in two.
		if sameFile(request.Outcome, request.lockPath) {
			return fmt.Errorf("the outcome file %s is the lifecycle lock %s", request.Outcome, request.lockPath)
		}
		defer func() { err = errors.Join(err, writeOutcome(request.Outcome, err)) }()
	}
	lock, err := holdLifecycle(request.lockPath, request.LockFD)
	if err != nil {
		return err
	}
	if request.LockFD == 0 {
		defer func() { err = errors.Join(err, dropLifecycle(lock, request.lockPath)) }()
	} else {
		// The caller that handed the lock holds it on and owns its file.
		defer lock.Close()
	}
	plan, err := PlanLifecycle(ctx, request)
	if err != nil {
		return err
	}
	plan.Print(request.Out)
	return applyLifecycle(ctx, plan, request.Out)
}

// applyLifecycle runs the plan's steps in order and stops at the first failure. Up to the boundary
// a cancel stops the plan before the next step, so a SIGTERM never starts a switch. Past it the
// rollback runs to its own limit after a cancel too: a SIGTERM mid-switch ends the upgrade with the
// old release back, not with neither.
func applyLifecycle(ctx context.Context, plan LifecyclePlan, out io.Writer) error {
	boundary, answered := plan.boundary, plan.answered
	if len(plan.rollback) == 0 {
		boundary, answered = len(plan.steps), len(plan.steps)
	}
	for i, s := range plan.steps {
		if i <= boundary && ctx.Err() != nil {
			return plan.refuse(fmt.Errorf("stopped before %s: %w", s.say, ctx.Err()))
		}
		fmt.Fprintf(out, "- %s\n", s.say)
		err := s.do(ctx)
		switch {
		case err == nil:
			continue
		case i < boundary:
			return plan.refuse(fmt.Errorf("%s: %w", s.say, err))
		case i >= answered:
			return outcomeError{outcomeLandedWithError, fmt.Errorf("release %s answers, and then %s: %w", plan.Release, s.say, err)}
		}
		return rollBack(ctx, plan, out, fmt.Errorf("%s: %w", s.say, err))
	}
	return nil
}

func (p LifecyclePlan) refuse(err error) error {
	if p.unstage == nil {
		return err
	}
	if staged := p.unstage(); staged != nil {
		return fmt.Errorf("%w. It changed nothing the gateway runs from, and removing what it staged failed: %w", err, staged)
	}
	return fmt.Errorf("%w. It changed nothing the gateway runs from", err)
}

func rollBack(ctx context.Context, plan LifecyclePlan, out io.Writer, failed error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackLimit)
	defer cancel()
	for _, r := range plan.rollback {
		fmt.Fprintf(out, "- %s\n", r.say)
		if err := r.do(ctx); err != nil {
			return outcomeError{outcomePutBackFailed, fmt.Errorf("%w. Putting it all back failed too, at %s: %w", failed, r.say, err)}
		}
	}
	back := fmt.Errorf("%w. %s", failed, plan.back)
	if plan.tidy != nil {
		fmt.Fprintf(out, "- %s\n", plan.tidy.say)
		if err := plan.tidy.do(ctx); err != nil {
			back = fmt.Errorf("%w, and then %s failed: %w", back, plan.tidy.say, err)
		}
	}
	return outcomeError{outcomeRolledBack, back}
}

// bundle is an inspected tarball. A release's name comes from its contents, so the same
// bundle is the same release, and installing it twice changes nothing.
type bundle struct {
	path      string
	daemonSHA string
	coreSHA   string
}

func (b bundle) release() string {
	sum := sha256.Sum256([]byte(b.daemonSHA + b.coreSHA))
	return hex.EncodeToString(sum[:6])
}

func walkBundle(path string, visit func(name string, content io.Reader) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	unzipped, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("%s isn't a bundle: %w", path, err)
	}
	reader := tar.NewReader(unzipped)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s isn't a bundle: %w", path, err)
		}
		name := strings.TrimPrefix(header.Name, "./")
		if name == "." || name == "" {
			continue
		}
		if header.Typeflag != tar.TypeReg || (name != bundleDaemon && name != bundleCore) || header.Size > maxBundleFile {
			return fmt.Errorf("%s holds %q, which no bundle should", path, header.Name)
		}
		if err := visit(name, reader); err != nil {
			return err
		}
	}
}

func inspectBundle(path string) (bundle, error) {
	b := bundle{path: path}
	err := walkBundle(path, func(name string, content io.Reader) error {
		hash := sha256.New()
		if _, err := io.Copy(hash, content); err != nil {
			return err
		}
		if name == bundleDaemon {
			b.daemonSHA = hex.EncodeToString(hash.Sum(nil))
		} else {
			b.coreSHA = hex.EncodeToString(hash.Sum(nil))
		}
		return nil
	})
	switch {
	case err != nil:
		return bundle{}, err
	case b.daemonSHA == "" || b.coreSHA == "":
		return bundle{}, fmt.Errorf("%s needs both %s and %s", path, bundleDaemon, bundleCore)
	case b.coreSHA != pinnedCore:
		return bundle{}, fmt.Errorf("%s carries core %s, and this build pins %s. Run the bundle's own sidegated", path, b.coreSHA, pinnedCore)
	}
	return b, nil
}

func releaseDir(dataDir, release string) string { return filepath.Join(dataDir, "releases", release) }

func coreDir(dataDir, digest string) string { return filepath.Join(dataDir, "cores", digest) }

// place makes releases/<id> whole: its daemon, and a link to its core under cores/<digest>, and with
// withCore the core binary there. The runtime compares a running core's path with the release's, so an
// upgrade that keeps the core must keep its path too. Each entry goes in by a rename and the
// directory itself is never replaced, so a release current names is never missing while it is put
// right, and an entry already right is left as it is.
func (b bundle) place(dataDir string, withCore bool) error {
	release := releaseDir(dataDir, b.release())
	core := filepath.Join(coreDir(dataDir, b.coreSHA), bundleCore)
	for _, dir := range []string{filepath.Dir(core), release} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	daemon := filepath.Join(release, bundleDaemon)
	_, coreErr := os.Stat(core)
	right := map[string]bool{bundleDaemon: fileSHA(daemon) == b.daemonSHA, bundleCore: coreErr == nil || !withCore}
	err := walkBundle(b.path, func(name string, content io.Reader) error {
		if right[name] {
			return nil
		}
		return copyAtomic(map[string]string{bundleDaemon: daemon, bundleCore: core}[name], content, 0o755)
	})
	if err != nil {
		return err
	}
	return symlinkAtomic(filepath.Join("..", "..", "cores", b.coreSHA, bundleCore), filepath.Join(release, bundleCore))
}

// fileSHA is empty for a file that can't be read.
func fileSHA(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func copyAtomic(path string, content io.Reader, mode os.FileMode) error {
	temp := path + ".new"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, content)
	if err == nil {
		err = file.Sync()
	}
	if closed := file.Close(); err == nil {
		err = closed
	}
	if err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// writeFileAtomic's temporary name starts with a dot, so nginx's shared-runnable-*.conf can
// never include it half written.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".new")
	if err := copyAtomic(temp, strings.NewReader(string(data)), mode); err != nil {
		return err
	}
	if err := os.Chmod(temp, mode); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func setCurrent(dataDir, release string) error {
	return symlinkAtomic(filepath.Join("releases", release), filepath.Join(dataDir, "current"))
}

// symlinkAtomic points link at target through a rename, so the link is never missing, and leaves
// a link already there alone.
func symlinkAtomic(target, link string) error {
	if now, err := os.Readlink(link); err == nil && now == target {
		return nil
	}
	temp := link + ".new"
	if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(target, temp); err != nil {
		return err
	}
	if err := os.Rename(temp, link); err != nil {
		return err
	}
	return syncDir(filepath.Dir(link))
}

// currentRelease is empty when nothing is installed.
func currentRelease(dataDir string) string {
	target, err := os.Readlink(filepath.Join(dataDir, "current"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// runningRelease names the release under releases/ this process runs from, or is nil for a
// daemon run from anywhere else. It reads the executable, not current, which an upgrade moves
// before it restarts the daemon.
func runningRelease(dataDir string) *string {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return nil
	}
	releases, err := filepath.EvalSymlinks(filepath.Join(dataDir, "releases"))
	if err != nil || filepath.Base(exe) != bundleDaemon || filepath.Dir(filepath.Dir(exe)) != releases {
		return nil
	}
	release := filepath.Base(filepath.Dir(exe))
	return &release
}

func currentCore(dataDir string) string { return linkedCore(filepath.Join(dataDir, "current")) }

func linkedCore(dir string) string {
	target, err := filepath.EvalSymlinks(filepath.Join(dir, bundleCore))
	if err != nil {
		return ""
	}
	return filepath.Base(filepath.Dir(target))
}

// liveCore is the digest of the core the core unit's process runs, read from the process, since a
// kill before an upgrade's core restart leaves current naming a core nothing runs yet. It is
// coreStopped when no process runs, and coreUnknown when systemd or the process can't be read.
type liveCore string

const (
	coreStopped liveCore = ""
	coreUnknown liveCore = "unknown"
)

func readLiveCore(ctx context.Context, proc string) liveCore {
	_, live, _ := coreProcess(ctx, proc)
	return live
}

// pruneReleases removes every release the current one isn't, and every core that neither current
// uses nor the core's process runs. A core whose process can't be named may run any of them, so
// none goes.
func pruneReleases(ctx context.Context, dataDir, proc string) error {
	used := map[string]func(name string) bool{
		"releases": func(name string) bool { return name == currentRelease(dataDir) },
		"cores": func(name string) bool {
			live := readLiveCore(ctx, proc)
			return live == coreUnknown || name == string(live) || name == currentCore(dataDir)
		},
	}
	for dir, keep := range used {
		entries, err := os.ReadDir(filepath.Join(dataDir, dir))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !keep(entry.Name()) {
				if err := os.RemoveAll(filepath.Join(dataDir, dir, entry.Name())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func ensureProof(dataDir string) ([]byte, error) {
	if proof, err := readProof(dataDir); err == nil {
		return proof, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	proof := []byte(base64.RawURLEncoding.EncodeToString(raw))
	return proof, writeFileAtomic(ProofPath(dataDir), proof, 0o600)
}

func storeAPIKey(dataDir string, source io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(source, maxAPIKey+1))
	if err != nil {
		return fmt.Errorf("reading the API key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" || len(raw) > maxAPIKey || strings.ContainsAny(key, " \t\r\n") {
		return errors.New("the API key on stdin must be one non-empty line")
	}
	return writeFileAtomic(filepath.Join(dataDir, networkKeyName), []byte(key), 0o600)
}

// writeBootstrap sets only what install owns, and keeps whatever else the owner put there.
func writeBootstrap(dataDir, hookDir, group string) error {
	path := bootstrapPath(dataDir)
	fields := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fmt.Errorf("bootstrap.json: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	fields["listenGroup"], fields["nginxHookDir"] = group, hookDir
	key := filepath.Join(dataDir, networkKeyName)
	if _, err := os.Stat(key); err == nil {
		fields["networkCredentialFile"] = key
	}
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

var nginxUserLine = regexp.MustCompile(`(?m)^\s*user\s+([^\s;]+)(?:\s+([^\s;]+))?\s*;`)

// nginxWorkerGroup reads who nginx's workers run as, since they must reach the socket.
// nginx's own default applies when its config names nobody.
func nginxWorkerGroup(ctx context.Context) (string, error) {
	config, err := runTool(ctx, "nginx", "-T")
	if err != nil {
		return "", err
	}
	name := "nobody"
	if match := nginxUserLine.FindStringSubmatch(config); match != nil {
		if match[2] != "" {
			return match[2], nil
		}
		name = match[1]
	}
	worker, err := user.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("nginx's user: %w", err)
	}
	group, err := user.LookupGroupId(worker.Gid)
	if err != nil {
		return "", fmt.Errorf("nginx's group: %w", err)
	}
	return group.Name, nil
}

// daemonUnit starts the daemon at boot, this project's one exception to no autostart. Every
// tie to the boot is soft: WantedBy lets multi-user.target finish whatever happens here, and
// Type=simple keeps boot from waiting. No other unit needs this one, and it orders itself
// only after the network, so a failed start can't hold up the gateway's own networking.
// A rebooted gateway has no pxa links, so every gate is closed until the Worker has proved
// the core it starts. KillMode=process leaves a running tool to finish, which the next
// Worker waits for.
//
// Every start, at boot and each of Restart's, first runs the journal's own sidegated recover, which
// puts back a switch a kill or a reboot cut short. Its failure is ignored, so a journal it can't
// read costs nothing but the attempt, as does one that isn't there, and it ends before ExecStart: it never
// restarts or waits for this unit. ExecStart itself marks a journal that is there, and then becomes the
// daemon, so the next pre-start knows the daemon was started. Only a shell does both in one process: an
// ExecStartPre would mark a start that may never come, and an ExecStartPost would keep the start job open,
// and fail it when the daemon exits at once. The shell gets each path as an argument and none in its text.
// The unit starts when either binary is there, so a release current names but the disk lost still reaches
// the recovery.
func daemonUnit(dataDir string) []byte {
	binary := filepath.Join(dataDir, "current", bundleDaemon)
	word, recovery := gateway.UnitWord, journalBinary(dataDir)
	return fmt.Appendf(nil, `[Unit]
Description=sidegate gateway daemon
After=network.target
ConditionPathExists=|%s
ConditionPathExists=|%s
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStartPre=-:%s recover --data-dir %s --yes
ExecStart=:/bin/sh -c 'test ! -d "$1" || touch "$2"; exec "$3" serve --data-dir "$4"' sh %s %s %s %s
SyslogIdentifier=%s
Restart=on-failure
RestartSec=10s
KillMode=process
TimeoutStopSec=30s

[Install]
WantedBy=multi-user.target
`, gateway.UnitText(binary), gateway.UnitText(recovery), word(recovery), word(dataDir),
		word(journalDir(dataDir)), word(startedMark(dataDir)), word(binary), word(dataDir), bundleDaemon)
}

// unitsCarry refuses a path writeUnits would put in a unit that can't hold it.
func unitsCarry(dataDir string) error {
	if err := gateway.UnitCarries(dataDir); err != nil {
		return fmt.Errorf("data dir %w", err)
	}
	b, err := LoadBootstrap(dataDir)
	if err != nil {
		return err
	}
	for _, path := range []string{b.DataDir, b.RunDir} {
		if err := gateway.UnitCarries(path); err != nil {
			return fmt.Errorf("bootstrap.json's path %w", err)
		}
	}
	return nil
}

func writeUnits(ctx context.Context, request LifecycleRequest) error {
	b, err := LoadBootstrap(request.DataDir)
	if err != nil {
		return err
	}
	coreText := gateway.CoreUnit(gateway.Options{DataDir: b.DataDir, RunDir: b.RunDir})
	for name, text := range map[string][]byte{daemonUnitName: daemonUnit(request.DataDir), gateway.CoreUnitName: coreText} {
		if err := writeFileAtomic(filepath.Join(request.unitDir, name), text, 0o644); err != nil {
			return err
		}
	}
	_, err = runTool(ctx, "systemctl", "daemon-reload")
	return err
}

// waitAnswering waits for release's daemon to answer api/session on the socket. Another release
// answering is not it.
func waitAnswering(ctx context.Context, dataDir, release string, limit time.Duration) error {
	b, err := LoadBootstrap(dataDir)
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", b.ListenSocket)
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 5 * time.Second}
	deadline := time.Now().Add(limit)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sidegated"+b.BasePath+"api/session", nil)
		if err != nil {
			return err
		}
		if err := VouchLocally(dataDir, req.Header); err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			var session struct {
				Release *string `json:"release"`
			}
			decoded := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&session)
			resp.Body.Close()
			switch {
			case resp.StatusCode != http.StatusOK:
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			case decoded != nil:
				err = fmt.Errorf("its session: %w", decoded)
			case session.Release == nil:
				err = fmt.Errorf("a daemon from outside releases/ answers, not release %s", release)
			case *session.Release != release:
				err = fmt.Errorf("release %s answers, not %s", *session.Release, release)
			default:
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the daemon didn't answer within %s: %w", limit, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(500*time.Millisecond, time.Until(deadline))):
		}
	}
}

// installedOther is install's refusal over another release, in versions and the command that works: a newer
// release is update's, and anything else is the bundle's own upgrade. A sidegated that isn't the bundle's
// own can't say the bundle's version.
func installedOther(request LifecycleRequest, installed string, from bundle) error {
	is, this := Build{Version: "release " + installed}, Build{Version: "release " + from.release()}
	if said, err := versionOf(context.Background(), filepath.Join(releaseDir(request.DataDir, installed), bundleDaemon)); err == nil {
		is = said
	}
	if fileSHA(request.recoveryBinary) == from.daemonSHA {
		this = request.Build
	}
	says := fmt.Sprintf("sidegate %s is installed, and this is %s.", is.Version, this.Version)
	now, updates := is.release()
	if next, released := this.release(); updates && released && next.above(now) {
		return fmt.Errorf("%s Update it: %s update --yes", says, filepath.Join(request.DataDir, "current", bundleDaemon))
	}
	return fmt.Errorf("%s To change to it, run upgrade --bundle with the bundle's own sidegated", says)
}

func planInstall(request LifecycleRequest, state installation) (LifecyclePlan, error) {
	installed := state.release
	plan := LifecyclePlan{Action: Install, Release: installed}
	var from bundle
	if request.BundlePath != "" {
		var err error
		if from, err = inspectBundle(request.BundlePath); err != nil {
			return plan, err
		}
		plan.Release = from.release()
		if installed != "" && installed != plan.Release {
			return plan, installedOther(request, installed, from)
		}
	} else if installed == "" {
		return plan, errors.New("nothing is installed yet, so install needs --bundle")
	}
	if _, err := readAuthInclude(request.hookDir); err != nil {
		return plan, err
	}
	dataDir := request.DataDir
	if from.path != "" {
		plan.steps = append(plan.steps, lifecycleStep{
			fmt.Sprintf("unpack the daemon and core %s under %s", from.coreSHA[:12], dataDir),
			func(context.Context) error {
				if err := os.MkdirAll(dataDir, 0o700); err != nil {
					return err
				}
				return from.place(dataDir, true)
			},
		})
	}
	if request.APIKey != nil {
		plan.steps = append(plan.steps, lifecycleStep{
			fmt.Sprintf("read UniFi's API key from stdin into root-only %s", filepath.Join(dataDir, networkKeyName)),
			func(context.Context) error { return storeAPIKey(dataDir, request.APIKey) },
		})
	}
	plan.steps = append(plan.steps,
		lifecycleStep{
			fmt.Sprintf("write %s and root-only %s, validate with nginx -t, then reload nginx. If nginx refuses it, remove it and stop", ProofPath(dataDir), filepath.Join(request.hookDir, hookName)),
			func(ctx context.Context) error {
				proof, err := ensureProof(dataDir)
				if err != nil {
					return err
				}
				// The operator asks again now, whenever nginx last refused it.
				if err := removeIfPresent(refusedPath(dataDir)); err != nil {
					return err
				}
				keeper := nginxKeeper{hookDir: request.hookDir, dataDir: dataDir, proof: proof}
				if err := keeper.ensureNginx(ctx); err != nil {
					return errors.Join(err, removeIfPresent(filepath.Join(request.hookDir, hookName)))
				}
				return nil
			},
		},
		// After the hook's test, since nginx -T also validates, and a refused hook a killed run
		// left would otherwise stop every install before the step that removes it.
		lifecycleStep{
			"write bootstrap.json: nginx's group, read from nginx -T, may reach the socket",
			func(ctx context.Context) error {
				group, err := nginxWorkerGroup(ctx)
				if err != nil {
					return err
				}
				return writeBootstrap(dataDir, request.hookDir, group)
			},
		},
	)
	// The release becomes current only once nginx has taken the hook, and goes back to exactly what
	// current named, or to none, unless its daemon answers: a current whose daemon is gone counts as
	// no release, and an install over it must not leave another release selected. Over no release it
	// first journals what current names, so a kill or a reboot after the switch puts that back too,
	// and the unready daemon it started stops, as none ran before.
	journaled := from.path != "" && installed == "" && !(state.interrupted && switchBegan(dataDir))
	var record rollbackRecord
	if journaled {
		plan.steps = append(plan.steps, lifecycleStep{"record what current names, with this sidegated to put it back at the next start", func(context.Context) error {
			record = rollbackRecord{Phase: phasePlanned, Selected: []string{plan.Release}, Links: map[string]was{}, Units: map[string]was{}, Boot: bootID(request.proc)}
			if err := remember(record.Links, filepath.Join(dataDir, "current")); err != nil {
				return err
			}
			if err := recordSwitch(dataDir, request.recoveryBinary, record); err != nil {
				return err
			}
			syscall.Sync()
			return nil
		}})
	}
	plan.boundary = len(plan.steps)
	if from.path != "" {
		current := filepath.Join(dataDir, "current")
		was, err := os.Readlink(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return plan, fmt.Errorf("current: %w", err)
		}
		selects := lifecycleStep{fmt.Sprintf("make release %s current", plan.Release), func(context.Context) error { return setCurrent(dataDir, plan.Release) }}
		if journaled {
			selects = record.begins(dataDir, selects)
		}
		plan.steps = append(plan.steps, selects)
		put := lifecycleStep{"remove current again: nothing was current", func(context.Context) error { return removeIfPresent(current) }}
		plan.back = "Nothing is current again"
		if err == nil {
			plan.Prior = filepath.Base(was)
			put = lifecycleStep{fmt.Sprintf("make current name %s again", was), func(context.Context) error { return symlinkAtomic(was, current) }}
			plan.back = fmt.Sprintf("current names %s again", was)
		}
		plan.rollback = []lifecycleStep{put}
		if installed == "" {
			plan.rollback = append(plan.rollback, lifecycleStep{fmt.Sprintf("stop %s: no release of ours ran before", daemonUnitName), func(ctx context.Context) error {
				_, err := runTool(ctx, "systemctl", "stop", daemonUnitName)
				return err
			}})
		}
		if journaled {
			plan.tidy = &lifecycleStep{"forget the record", func(context.Context) error { return dropJournal(dataDir) }}
		}
	}
	plan.steps = append(plan.steps,
		lifecycleStep{
			fmt.Sprintf("write %s and %s in %s", daemonUnitName, gateway.CoreUnitName, request.unitDir),
			func(ctx context.Context) error { return writeUnits(ctx, request) },
		},
		lifecycleStep{
			fmt.Sprintf("enable %s at boot and start it. It starts the core. %s stays not enabled", daemonUnitName, gateway.CoreUnitName),
			func(ctx context.Context) error {
				if _, err := runTool(ctx, "systemctl", "enable", daemonUnitName); err != nil {
					return err
				}
				_, err := runTool(ctx, "systemctl", "restart", daemonUnitName)
				return err
			},
		},
		lifecycleStep{fmt.Sprintf("wait for release %s to answer on its socket", plan.Release), func(ctx context.Context) error {
			return waitAnswering(ctx, dataDir, plan.Release, request.answering)
		}},
	)
	plan.answered = len(plan.steps)
	if state.interrupted || journaled {
		plan.steps = append(plan.steps, lifecycleStep{
			fmt.Sprintf("forget the record of what to put back: release %s answers", plan.Release),
			func(context.Context) error { return removeJournal(dataDir) },
		})
	}
	return plan, nil
}

// switchBase is what an upgrade starts from, whatever its bundle: the release it puts back if it fails,
// the record it goes on with and the core it would leave.
type switchBase struct {
	prior     string
	record    rollbackRecord
	kept      []byte // The earlier record's bytes.
	unparsed  bool   // They aren't a record.
	takenOver bool   // They are one whose switch began, and goes on under this upgrade.
	live      liveCore
	running   liveCore
}

// readSwitchBase reads what the journal and the core's process say. A journal a kill left once its switch
// began names the release that last answered, so it stays and this switch adds its candidate, and takes
// over a put-back it began. One that ended is a switch whose release answered, and that release is prior.
// One whose switch never began is a plan nobody carried out, and nothing of it stands. One whose bytes
// don't parse says nothing, so what is current is prior. One whose bytes can't be read may say anything,
// so nothing goes past it.
func readSwitchBase(ctx context.Context, dataDir, proc string, state installation) (switchBase, error) {
	base := switchBase{prior: state.release, record: rollbackRecord{Phase: phasePlanned, Links: map[string]was{}, Units: map[string]was{}}}
	if state.interrupted {
		kept, err := readJournal(dataDir)
		if err != nil {
			return base, fmt.Errorf("the record of a switch cut short can't be read, and it may name the release to put back, so nothing changes: %w", err)
		}
		base.kept = kept
		earlier, err := parseRecord(dataDir, kept)
		base.unparsed = err != nil
		if base.takenOver = !base.unparsed && !earlier.Phase.ended() && earlier.began(dataDir); base.takenOver {
			if base.prior = earlier.prior(dataDir); base.prior == "" {
				return base, errors.New("an install was cut short before its daemon answered; run install again")
			}
			base.record = earlier
			if base.record.Phase == phaseRestored {
				base.record.Phase = phaseRestoring
			}
		}
	}
	base.live = readLiveCore(ctx, proc)
	base.running = base.live
	switch base.live {
	case coreStopped:
		// Not what current names: after a cut-short switch that is a core nothing has judged.
		base.running = liveCore(linkedCore(releaseDir(dataDir, base.prior)))
	case coreUnknown:
		base.running = liveCore(currentCore(dataDir))
	}
	return base, nil
}

// restartsCore says whether a release with core digest drops every slot's flows.
func (b switchBase) restartsCore(digest string) bool { return digest != string(b.running) }

var saysCore = map[bool]string{true: "restarts the core", false: "keeps the core running"}

func planUpgrade(ctx context.Context, request LifecycleRequest, state installation) (LifecyclePlan, error) {
	dataDir := request.DataDir
	installed := state.release
	if request.Expect != "" && installed != request.Expect {
		current := "no release"
		if installed != "" {
			current = "release " + installed
		}
		return LifecyclePlan{}, outcomeError{outcomeMoved, fmt.Errorf("%s is current, not %s", current, request.Expect)}
	}
	if installed == "" {
		return LifecyclePlan{}, errors.New("nothing is installed; use install")
	}
	from, err := inspectBundle(request.BundlePath)
	if err != nil {
		return LifecyclePlan{}, err
	}
	outcomePath := request.Outcome
	if outcomePath != "" {
		if outcomePath, err = filepath.Abs(outcomePath); err != nil {
			return LifecyclePlan{}, err
		}
	}
	base, err := readSwitchBase(ctx, dataDir, request.proc, state)
	if err != nil {
		return LifecyclePlan{}, err
	}
	prior, record, kept, unparsed, takenOver, live := base.prior, base.record, base.kept, base.unparsed, base.takenOver, base.live
	plan := LifecyclePlan{Action: Upgrade, Release: from.release(), Prior: prior, RestartsCore: base.restartsCore(from.coreSHA)}
	if request.RestartsCore != nil && *request.RestartsCore != plan.RestartsCore {
		return LifecyclePlan{}, outcomeError{outcomePlanChanged, fmt.Errorf("this upgrade %s now, and the plan confirmed %s", saysCore[plan.RestartsCore], saysCore[*request.RestartsCore])}
	}
	// A switch cut short has already disturbed the core, so what it runs now decides nothing.
	if !takenOver {
		record.CoreRequired = coreRequired(ctx, dataDir, live)
	}
	newCore := from.coreSHA != linkedCore(releaseDir(dataDir, prior))
	record.Outcome, record.Boot = outcomePath, bootID(request.proc)
	if !slices.Contains(record.Selected, plan.Release) {
		record.Selected = append(slices.Clip(record.Selected), plan.Release)
	}

	// Before the boundary: the new release, if current doesn't name it, and the journal. A core file
	// current's core link already names goes in after the boundary, since a running core's restart
	// would start it. A refusal removes the journal, or puts the earlier record back, and what it
	// created under releases/ and cores/.
	place := lifecycleStep{fmt.Sprintf("put release %s right, which current names: its daemon and its core link", plan.Release), func(context.Context) error { return from.place(dataDir, true) }}
	placedLive := plan.Release == installed
	core := filepath.Join(coreDir(dataDir, from.coreSHA), bundleCore)
	coreLive := false
	if link, err := os.Readlink(filepath.Join(dataDir, "current", bundleCore)); err == nil {
		coreLive = filepath.Join(releaseDir(dataDir, installed), link) == core
	}
	if !placedLive {
		plan.steps = append(plan.steps, lifecycleStep{fmt.Sprintf("unpack release %s beside %s", plan.Release, installed), func(context.Context) error { return from.place(dataDir, !coreLive) }})
	}
	aside := false
	if unparsed {
		plan.steps = append(plan.steps, lifecycleStep{"set aside the record of a switch cut short, which isn't one", func(context.Context) (err error) {
			aside, err = setJournalAside(dataDir)
			return err
		}})
	}
	plan.steps = append(plan.steps, lifecycleStep{
		fmt.Sprintf("record release %s to put back, with this sidegated to put it back at the next start: current, its core link and the units. Flush it all to disk", prior),
		func(context.Context) error {
			current := filepath.Join(dataDir, "current")
			paths := []string{current, filepath.Join(releaseDir(dataDir, prior), bundleCore)}
			var errs []error
			for _, path := range paths {
				errs = append(errs, remember(record.Links, path))
			}
			for _, name := range []string{daemonUnitName, gateway.CoreUnitName} {
				errs = append(errs, remember(record.Units, filepath.Join(request.unitDir, name)))
			}
			if err := errors.Join(errs...); err != nil {
				return err
			}
			if err := recordSwitch(dataDir, request.recoveryBinary, record); err != nil {
				return err
			}
			// The release it puts back, the new one and the journal, on disk before the switch.
			syscall.Sync()
			return nil
		},
	})
	created := []string{}
	for _, dir := range []string{releaseDir(dataDir, plan.Release), coreDir(dataDir, from.coreSHA)} {
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			created = append(created, dir)
		}
	}
	plan.unstage = func() error {
		errs := []error{os.RemoveAll(journalDir(dataDir) + ".new")}
		switch {
		case aside:
			errs = append(errs, bringJournalBack(dataDir))
		case unparsed:
		case kept != nil:
			errs = append(errs, writeFileAtomic(journalRecord(dataDir), kept, 0o600))
		default:
			errs = append(errs, dropJournal(dataDir))
		}
		for _, dir := range created {
			errs = append(errs, os.RemoveAll(dir))
		}
		return errors.Join(errs...)
	}

	restart := func(ctx context.Context) error {
		_, err := runTool(ctx, "systemctl", "restart", daemonUnitName)
		return err
	}
	answers := func(release string) func(context.Context) error {
		return func(ctx context.Context) error { return waitAnswering(ctx, dataDir, release, request.answering) }
	}
	plan.boundary = len(plan.steps)
	switch {
	case placedLive:
		plan.steps = append(plan.steps, place)
	case coreLive:
		plan.steps = append(plan.steps, lifecycleStep{fmt.Sprintf("put core %s in place, which current's core link names", short(from.coreSHA)), place.do})
	}
	plan.steps = append(plan.steps,
		lifecycleStep{fmt.Sprintf("make release %s current", plan.Release), func(context.Context) error { return setCurrent(dataDir, plan.Release) }},
		lifecycleStep{"rewrite the units", func(ctx context.Context) error { return writeUnits(ctx, request) }},
		lifecycleStep{"forget that nginx refused the hook, if it did, so the new daemon tries it again at once", func(context.Context) error { return removeIfPresent(refusedPath(dataDir)) }},
		lifecycleStep{fmt.Sprintf("restart %s only. It adopts the running core and its gates, and puts the hook right", daemonUnitName), restart},
		lifecycleStep{fmt.Sprintf("wait for release %s to answer on its socket", plan.Release), answers(plan.Release)},
	)
	sayCore := fmt.Sprintf("restart the core onto core %s if it runs another. None had to run before, so none is waited for", short(from.coreSHA))
	if record.CoreRequired {
		sayCore = fmt.Sprintf("restart the core onto core %s if it runs another, and wait for it to stay up %s, since it served before", short(from.coreSHA), request.steady.settle)
	}
	plan.steps = append(plan.steps, lifecycleStep{sayCore, func(ctx context.Context) error {
		return switchCore(ctx, dataDir, request.proc, from.coreSHA, record.CoreRequired, newCore, request.steady)
	}})
	plan.answered = len(plan.steps)
	if !takenOver {
		plan.steps[plan.boundary] = record.begins(dataDir, plan.steps[plan.boundary])
	}
	plan.steps = append(plan.steps,
		lifecycleStep{fmt.Sprintf("keep release %s: mark the record kept, say landed, then forget it", plan.Release), func(context.Context) error {
			record, err := readRecord(dataDir)
			if err != nil {
				return err
			}
			return record.end(dataDir, phaseCommitted)
		}},
		lifecycleStep{"remove the releases and cores nothing uses now", func(ctx context.Context) error { return pruneReleases(ctx, dataDir, request.proc) }},
	)

	// Past the boundary nothing is judged by how it looks: systemd may have restarted the daemon on
	// its own, or still hold units its files no longer show, so every step runs. Only the core's
	// restart reads what runs, since restarting a core that runs the right binary drops every flow.
	// The core is waited for last, since until the old daemon is back the new one may still be what
	// keeps it from staying up.
	plan.rollback = []lifecycleStep{
		{fmt.Sprintf("put back all the record holds, current first: current names release %s, its core link and the units as they were", prior), func(context.Context) error { return putBack(dataDir) }},
		{"reload systemd's units", reloadUnits},
		{fmt.Sprintf("put the core back on release %s's core: restart it if it runs another, start it if it was required and runs no more", prior), func(ctx context.Context) error {
			return coreBack(ctx, dataDir, request.proc)
		}},
		{fmt.Sprintf("restart %s on release %s, whether or not it runs", daemonUnitName, prior), restart},
		{fmt.Sprintf("wait for release %s to answer on its socket", prior), answers(prior)},
		{"wait for a core that was required to stay up", func(ctx context.Context) error {
			return waitCoreBack(ctx, dataDir, request.proc, request.steady)
		}},
	}
	plan.back = fmt.Sprintf("Release %s is back and answers", prior)
	plan.tidy = &lifecycleStep{fmt.Sprintf("forget the upgrade, and remove release %s and anything else nothing uses", plan.Release), func(ctx context.Context) error {
		if err := removeJournal(dataDir); err != nil {
			return err
		}
		return pruneReleases(ctx, dataDir, request.proc)
	}}
	return plan, nil
}

func planUninstall(ctx context.Context, request LifecycleRequest) (LifecyclePlan, error) {
	dataDir := request.DataDir
	b, err := LoadBootstrap(dataDir)
	if err != nil {
		return LifecyclePlan{}, err
	}
	if !strings.HasPrefix(filepath.Base(b.RunDir), "sidegate") {
		return LifecyclePlan{}, fmt.Errorf("bootstrap.json's run dir %q isn't plainly ours, and uninstall would delete it", b.RunDir)
	}
	key := filepath.Join(dataDir, networkKeyName)
	if _, err := os.Stat(key); b.NetworkCredentialFile == "" && err == nil {
		b.NetworkCredentialFile = key
	}
	syncer := request.syncer
	if syncer == nil {
		if syncer, err = openSyncer(b); err != nil {
			fmt.Fprintf(request.Out, "UniFi can't be asked about routes: %v\n", err)
			syncer = nil
		}
	}
	plan := LifecyclePlan{Action: Uninstall, Routes: routeReport(ctx, syncer)}
	report := func(context.Context) error {
		for _, line := range plan.Routes {
			fmt.Fprintln(request.Out, line)
		}
		return nil
	}
	plan.steps = []lifecycleStep{
		{fmt.Sprintf("stop and disable %s", daemonUnitName), func(ctx context.Context) error {
			if _, err := os.Stat(filepath.Join(request.unitDir, daemonUnitName)); errors.Is(err, os.ErrNotExist) {
				return nil
			}
			_, err := runTool(ctx, "systemctl", "disable", "--now", daemonUnitName)
			return err
		}},
		{fmt.Sprintf("remove %s and reload nginx if it validates", filepath.Join(request.hookDir, hookName)), func(ctx context.Context) error {
			if err := removeIfPresent(filepath.Join(request.hookDir, hookName)); err != nil {
				return err
			}
			if _, err := runNginx(ctx, nil, "-t"); err != nil {
				fmt.Fprintf(request.Out, "nginx doesn't validate without our hook, so it wasn't reloaded: %v\n", err)
				return nil
			}
			if _, err := runNginx(ctx, nil, "-s", "reload"); err != nil {
				fmt.Fprintf(request.Out, "nginx wasn't reloaded, so it serves the old location until it is: %v\n", err)
			}
			return nil
		}},
		{"remove every slot's links, rules and firewall chains, and stop the core", func(ctx context.Context) error {
			return request.teardown(ctx, b)
		}},
		{fmt.Sprintf("remove %s and %s", daemonUnitName, gateway.CoreUnitName), func(ctx context.Context) error {
			return removeUnits(ctx, request.unitDir)
		}},
	}
	if request.Purge {
		plan.steps = append(plan.steps, lifecycleStep{"delete our VPN Clients that no route targets. A client a route still targets stays", func(ctx context.Context) error {
			return removeOwnedClients(ctx, syncer, request.Out)
		}})
	}
	remove := "remove the release, proof, analytics and run dirs, keeping saved sources, keys and our VPN Clients. --purge removes those too"
	if request.Purge {
		remove = fmt.Sprintf("remove %s, with saved sources and keys, and the run dirs", dataDir)
	}
	plan.steps = append(plan.steps,
		lifecycleStep{remove, func(context.Context) error { return removeFiles(request, b) }},
		lifecycleStep{"report the owner's routes that still target our VPN Clients. They and all the owner's objects stay", report},
	)
	return plan, nil
}

// routeReport never fails: what it can't read, it says it can't read.
func routeReport(ctx context.Context, syncer unifi.Syncer) []string {
	if syncer == nil {
		return []string{"UniFi has no API key here, so no routes were read."}
	}
	ctx, cancel := context.WithTimeout(ctx, unifiInspectLimit)
	defer cancel()
	inventory, err := syncer.Inspect(ctx)
	if err != nil {
		return []string{fmt.Sprintf("UniFi couldn't be read, so routes targeting our VPN Clients aren't known: %v", err)}
	}
	var lines []string
	for _, owned := range inventory.Owned {
		for _, route := range owned.Routes {
			lines = append(lines, fmt.Sprintf("Route %q still targets our VPN Client %q (%s). It stays: remove or retarget it in UniFi.", route.Name, owned.Name, owned.NetworkID))
		}
	}
	if len(lines) == 0 {
		lines = []string{fmt.Sprintf("No route targets any of our %d VPN Clients.", len(inventory.Owned))}
	}
	return lines
}

func removeOwnedClients(ctx context.Context, syncer unifi.Syncer, out io.Writer) error {
	if syncer == nil {
		fmt.Fprintln(out, "UniFi has no API key here, so no VPN Client was deleted.")
		return nil
	}
	report, err := syncer.Reconcile(ctx, nil)
	if err != nil {
		fmt.Fprintf(out, "UniFi couldn't delete our VPN Clients, and any that exist stay: %v\n", err)
		return nil
	}
	for _, id := range report.Removed {
		fmt.Fprintf(out, "Deleted our VPN Client %s.\n", id)
	}
	for _, kept := range report.Retained {
		fmt.Fprintf(out, "Our VPN Client %q (%s) stays: %v\n", kept.Name, kept.NetworkID, kept.Problem)
	}
	return nil
}

func removeUnits(ctx context.Context, unitDir string) error {
	removed := false
	for _, name := range []string{daemonUnitName, gateway.CoreUnitName} {
		path := filepath.Join(unitDir, name)
		if _, err := os.Stat(path); err == nil {
			removed = true
		}
		if err := removeIfPresent(path); err != nil {
			return err
		}
	}
	if !removed {
		return nil
	}
	if _, err := runTool(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	// A unit that failed stays listed until this, and a snapshot must not see it. It answers
	// an error for a unit systemd has already forgotten, which is the state we want.
	runTool(ctx, "systemctl", "reset-failed", daemonUnitName, gateway.CoreUnitName)
	return nil
}

func removeFiles(request LifecycleRequest, b Bootstrap) error {
	if err := errors.Join(os.RemoveAll(b.RunDir), removeIfPresent(b.ListenSocket)); err != nil {
		return err
	}
	// The socket's directory goes only if nothing else is in it: bootstrap.json names it.
	os.Remove(filepath.Dir(b.ListenSocket))
	if request.Purge {
		return os.RemoveAll(request.DataDir)
	}
	entries, err := os.ReadDir(request.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if slices.Contains(keptWithoutPurge, entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(request.DataDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// tearDownRuntime converges an empty plan with the real Worker: the core stops and every
// link, rule and chain of ours goes. Naming the stored slots also removes a rule whose
// link someone else deleted. It leaves the owner's rules and links alone, as any plan does.
func tearDownRuntime(ctx context.Context, b Bootstrap) error {
	ctx, cancel := context.WithTimeout(ctx, teardownLimit)
	defer cancel()
	plan, err := gateway.NewPlan(1, nil, nil, storedSlots(ctx, b.DataDir))
	if err != nil {
		return err
	}
	worker, err := gateway.Open(gateway.Options{DataDir: b.DataDir, RunDir: b.RunDir, Poll: time.Second})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(b.RunDir, 0o700); err != nil {
		return err
	}
	goals, reports := make(chan gateway.Plan, 1), make(chan gateway.Report)
	goals <- plan
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx, goals, reports, nil) }()
	stop := func() {
		cancel()
		<-done
	}
	for {
		select {
		case err := <-done:
			if err == nil {
				err = ctx.Err()
			}
			return fmt.Errorf("the runtime stopped before it converged: %w", err)
		case report := <-reports:
			if len(report.Steps) == 0 {
				stop()
				if report.Error != "" {
					return errors.New(report.Error)
				}
				return nil
			}
		}
	}
}

// storedState reads state.db, and is empty where there is none.
func storedState(ctx context.Context, dataDir string) (durableState, world, error) {
	if _, err := os.Stat(statePath(dataDir)); errors.Is(err, os.ErrNotExist) {
		return durableState{}, world{}, nil
	} else if err != nil {
		return durableState{}, world{}, err
	}
	st, q, err := openStore(ctx, dataDir)
	if err != nil {
		return durableState{}, world{}, err
	}
	defer st.close()
	defer q.close()
	return st.recover(ctx, time.Now(), defaultTimings())
}

// storedSlots is best effort: without state.db, teardown still removes whatever is in
// our link group.
func storedSlots(ctx context.Context, dataDir string) []domain.Slot {
	state, _, err := storedState(ctx, dataDir)
	if err != nil {
		return nil
	}
	var slots []domain.Slot
	for _, row := range state.slots {
		slots = append(slots, row.Slot)
	}
	return slots
}

// InstallStatus says what is installed, whether or not the daemon is up.
func InstallStatus(ctx context.Context, dataDir string, out io.Writer) {
	release := currentRelease(dataDir)
	if release == "" {
		release = "none"
	}
	fmt.Fprintf(out, "release %s\n", release)
	for _, unit := range []string{daemonUnitName, gateway.CoreUnitName} {
		enabled, _ := runTool(ctx, "systemctl", "is-enabled", unit)
		active, _ := runTool(ctx, "systemctl", "is-active", unit)
		fmt.Fprintf(out, "%s: %s, %s\n", unit, strings.TrimSpace(firstLine(enabled)), strings.TrimSpace(firstLine(active)))
	}
	b, err := LoadBootstrap(dataDir)
	if err == nil {
		dir := b.NginxHookDir
		if dir == "" {
			dir = defaultHookDir
		}
		if _, err := os.Stat(filepath.Join(dir, hookName)); err == nil {
			fmt.Fprintln(out, "nginx hook: present")
		} else {
			fmt.Fprintln(out, "nginx hook: missing")
		}
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}
