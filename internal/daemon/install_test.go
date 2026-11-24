package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

const networkFile = `location ^~ /proxy/network/ {
    include /data/unifi-core/config/http/cors.conf;
    include /data/unifi-core/config/http/auth.conf;
    include /data/unifi-core/config/http/proxy.conf;
}
`

type installRig struct {
	t                        *testing.T
	root, data, units, hooks string
	calls, socket            string
	tornDown                 int
	syncer                   unifi.Syncer
	coreBytes                []byte
}

func rigAt(t *testing.T, root string) *installRig {
	return &installRig{t: t, root: root, data: filepath.Join(root, "data", "sidegate"), units: filepath.Join(root, "units"),
		hooks: filepath.Join(root, "http"), calls: filepath.Join(root, "calls"), coreBytes: []byte("core one")}
}

// newInstallRig puts a stand-in nginx and systemctl first on PATH. They append each call to
// a file, and nginx -t and -T fail while nginx-refuses exists, or while nginx-refuses-hook and our hook
// both do. Restarting the daemon fails while restart-fails exists. While nginx-kills exists, nginx -t kills its caller and exits only once the caller is gone,
// since a caller that saw the exit could act on it before the kill lands.
// While core-flaps exists, each show of the core's process names a new one, as of a core that keeps
// exiting and that systemd keeps restarting. systemctl keeps what systemd holds: daemon-runs names the daemon binary a restart started and
// what it held then, and stop removes it. loaded holds the units the last daemon-reload read, and
// a reload fails while reload-fails exists.
func newInstallRig(t *testing.T) *installRig {
	t.Helper()
	root := t.TempDir()
	r := rigAt(t, root)
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{bin, r.units, r.hooks, r.data} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	nginx := fmt.Sprintf(`#!/bin/sh
echo "nginx $*" >>%q
case "$1" in
-t|-T) [ "$1" = -t ] && echo "owed $([ -e %q ] && echo yes || echo no)" >>%q
    [ "$1" = -t ] && [ -e %q ] && { kill -9 "$PPID"; while kill -0 "$PPID" 2>/dev/null; do sleep 0.01; done; exit 1; }
    [ ! -e %q ] && { [ ! -e %q ] || [ ! -e %q ]; } || { echo 'nginx: [emerg] refused' >&2; exit 1; }
    if [ "$1" = -T ]; then echo 'user nobody;'; fi;;
esac
`, r.calls, filepath.Join(r.data, "nginx-owed"), r.calls, filepath.Join(root, "nginx-kills"), filepath.Join(root, "nginx-refuses"), filepath.Join(root, "nginx-refuses-hook"), filepath.Join(r.hooks, hookName))
	systemctl := fmt.Sprintf(`#!/bin/sh
echo "systemctl $*" >>%q
pid=$(cat %q 2>/dev/null)
case "$1 $2" in
"show sidegate-core.service") if [ -n "$pid" ] && [ -e %[16]q ]; then exe=$(readlink %[4]q/$pid/exe); pid=$$; mkdir -p %[4]q/$pid && ln -sfn "$exe" %[4]q/$pid/exe; fi
    echo "MainPID=${pid:-0}";;
"try-restart sidegate-core.service") if [ -n "$pid" ]; then ln -sfn "$(readlink -f %[3]q)" %[4]q/$pid/exe; if grep -q dies %[3]q; then rm -f %[2]q; fi; fi;;
"start sidegate-core.service") mkdir -p %[4]q/4242 && ln -sfn "$(readlink -f %[3]q)" %[4]q/4242/exe && echo 4242 >%[2]q; if grep -q dies %[3]q; then rm -f %[2]q; fi;;
"restart sidegated.service") echo "refused $([ -e %[5]q ] && echo yes || echo no)" >>%q; [ ! -e %q ] || exit 1
    daemon=%q; if [ -e "$daemon" ]; then printf '%%s\n%%s' "$(readlink -f "$daemon")" "$(cat "$daemon")" >%q; else rm -f %q; fi;;
"stop sidegated.service") rm -f %q;;
"daemon-reload ") [ ! -e %q ] || exit 1; rm -rf %q; cp -R %q %q;;
esac
`, r.calls, filepath.Join(root, "core-pid"), filepath.Join(r.data, "current", bundleCore), filepath.Join(root, "proc"), refusedPath(r.data), r.calls, filepath.Join(root, "restart-fails"),
		filepath.Join(r.data, "current", bundleDaemon), r.daemonRuns(), r.daemonRuns(), r.daemonRuns(), filepath.Join(root, "reload-fails"), r.loaded(), r.units, r.loaded(), filepath.Join(root, "core-flaps"))
	for name, text := range map[string]string{"nginx": nginx, "systemctl": systemctl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(r.hooks, "shared-runnable-network.conf"), []byte(networkFile), 0o644); err != nil {
		t.Fatal(err)
	}
	r.pinCore()

	dir, err := os.MkdirTemp("", "sg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r.socket = filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", r.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	// The daemon that answers is the one a restart started, and names the release it runs from, as
	// api/session does. A release named broken never answers.
	go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		raw, err := os.ReadFile(r.daemonRuns())
		path, held, _ := strings.Cut(string(raw), "\n")
		if err != nil || strings.HasPrefix(held, "broken") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"release": filepath.Base(filepath.Dir(path))})
	}))
	r.ownerBootstrap()
	if err := os.WriteFile(r.recoveryBinary(), []byte("the recovery sidegated"), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *installRig) ownerBootstrap() {
	r.t.Helper()
	raw, _ := json.Marshal(map[string]any{"listenSocket": r.socket, "runDir": filepath.Join(r.root, "run", "sidegate"), "clientDns": "9.9.9.9"})
	if err := os.WriteFile(filepath.Join(r.data, "bootstrap.json"), raw, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *installRig) pinCore() {
	old := pinnedCore
	sum := sha256.Sum256(r.coreBytes)
	pinnedCore = hex.EncodeToString(sum[:])
	r.t.Cleanup(func() { pinnedCore = old })
}

func (r *installRig) bundle(name string, entries map[string][]byte) string {
	r.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for file, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: file, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			r.t.Fatal(err)
		}
		tw.Write(content)
	}
	tw.Close()
	gz.Close()
	path := filepath.Join(r.root, name+".tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return path
}

func (r *installRig) release(daemon string) string {
	return r.bundle(daemon, map[string][]byte{bundleDaemon: []byte(daemon), bundleCore: r.coreBytes})
}

func (r *installRig) request(action Action, bundle string) LifecycleRequest {
	return LifecycleRequest{Action: action, DataDir: r.data, BundlePath: bundle, unitDir: r.units, hookDir: r.hooks, syncer: r.syncer, proc: filepath.Join(r.root, "proc"), lockPath: r.lockPath(),
		teardown: func(context.Context, Bootstrap) error { r.tornDown++; return nil }, answering: 50 * time.Millisecond,
		steady: steadiness{settle: 20 * time.Millisecond, limit: 200 * time.Millisecond}, recoveryBinary: r.recoveryBinary()}
}

// recoveryBinary stands in for the sidegated a journal carries.
func (r *installRig) recoveryBinary() string { return filepath.Join(r.root, "recovery-sidegated") }

func (r *installRig) lockPath() string { return filepath.Join(r.root, "lifecycle.lock") }

// run returns the plan as a preview of the same request shows it, and what RunLifecycle did.
func (r *installRig) run(request LifecycleRequest) (LifecyclePlan, string, error) {
	r.t.Helper()
	var out bytes.Buffer
	request.Out = &out
	plan, err := PlanLifecycle(context.Background(), request)
	if err != nil {
		return plan, out.String(), err
	}
	out.Reset()
	err = RunLifecycle(context.Background(), request)
	return plan, out.String(), err
}

func (r *installRig) mustRun(request LifecycleRequest) (LifecyclePlan, string) {
	r.t.Helper()
	plan, out, err := r.run(request)
	if err != nil {
		r.t.Fatalf("%s: %v\n%s", request.Action, err, out)
	}
	return plan, out
}

// runCore starts the core unit as systemd would: its process runs the binary current names now,
// and a try-restart moves it to the one current names then.
func (r *installRig) runCore() {
	r.t.Helper()
	exe, err := filepath.EvalSymlinks(filepath.Join(r.data, "current", bundleCore))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.root, "proc", "4242"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(r.root, "proc", "4242", "exe")); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.root, "core-pid"), []byte("4242"), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *installRig) coreExe() string {
	exe, _ := os.Readlink(filepath.Join(r.root, "proc", "4242", "exe"))
	return exe
}

func (r *installRig) daemonRuns() string { return filepath.Join(r.root, "daemon-runs") }

func (r *installRig) loaded() string { return filepath.Join(r.root, "loaded") }

// runningDaemon is the binary the daemon a restart started runs, or empty when none runs.
func (r *installRig) runningDaemon() string {
	raw, _ := os.ReadFile(r.daemonRuns())
	path, _, _ := strings.Cut(string(raw), "\n")
	return path
}

// stepAt is where the plan's first step whose words start with say is.
func stepAt(t *testing.T, plan LifecyclePlan, say string) int {
	t.Helper()
	for i, s := range plan.steps {
		if strings.HasPrefix(s.say, say) {
			return i
		}
	}
	t.Fatalf("no step says %q", say)
	return 0
}

func (r *installRig) callLog() string {
	raw, _ := os.ReadFile(r.calls)
	return string(raw)
}

func (r *installRig) clearCalls() { os.Remove(r.calls) }

func (r *installRig) tree() map[string]string {
	r.t.Helper()
	tree := map[string]string{}
	for _, dir := range []string{r.data, r.units, r.hooks} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			entry := info.Mode().String()
			switch {
			case info.Mode().IsRegular():
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				entry += " " + string(raw)
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				entry += " -> " + target
			}
			tree[path] = entry
			return nil
		})
		if err != nil {
			r.t.Fatal(err)
		}
	}
	return tree
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (r *installRig) mode(path string) os.FileMode {
	r.t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return info.Mode().Perm()
}

func inOrder(t *testing.T, log string, parts ...string) {
	t.Helper()
	at := 0
	for _, part := range parts {
		i := strings.Index(log[at:], part)
		if i < 0 {
			t.Errorf("%q isn't in order in:\n%s", part, log)
			return
		}
		at += i + len(part)
	}
}

func TestAPlanChangesNothing(t *testing.T) {
	r := newInstallRig(t)
	before := r.tree()
	for _, request := range []LifecycleRequest{r.request(Install, r.release("one")), r.request(Uninstall, "")} {
		plan, err := PlanLifecycle(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		plan.Print(&out)
		if !strings.Contains(out.String(), "1. ") {
			t.Errorf("%s printed no steps: %s", request.Action, out.String())
		}
	}
	if !sameTree(before, r.tree()) {
		t.Error("planning changed a file")
	}
	if strings.Contains(r.callLog(), "systemctl") || strings.Contains(r.callLog(), "nginx -s") {
		t.Errorf("planning changed the host:\n%s", r.callLog())
	}
}

func TestInstallPutsEverythingInPlaceInOrder(t *testing.T) {
	r := newInstallRig(t)
	request := r.request(Install, r.release("one"))
	request.APIKey = strings.NewReader("the-api-key\n")
	_, out := r.mustRun(request)
	if strings.Contains(out, "the-api-key") {
		t.Errorf("the key reached the output: %s", out)
	}
	inOrder(t, r.callLog(), "nginx -t", "nginx -s reload", "nginx -T", "systemctl daemon-reload", "systemctl enable sidegated.service", "systemctl restart sidegated.service")
	if strings.Contains(r.callLog(), "enable sidegate-core") {
		t.Error("the core must not start at boot on its own")
	}

	proof, err := os.ReadFile(ProofPath(r.data))
	if err != nil {
		t.Fatal(err)
	}
	if len(proof) < 40 {
		t.Errorf("proof %q is short", proof)
	}
	hookPath := filepath.Join(r.hooks, hookName)
	hook, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"include /data/unifi-core/config/http/auth.conf;", ProofHeader + ` "` + string(proof) + `"`} {
		if !strings.Contains(string(hook), want) {
			t.Errorf("hook lacks %q:\n%s", want, hook)
		}
	}
	if strings.Contains(string(hook), "@") {
		t.Errorf("hook has an unfilled placeholder:\n%s", hook)
	}
	for _, path := range []string{hookPath, ProofPath(r.data), filepath.Join(r.data, networkKeyName), filepath.Join(r.data, "bootstrap.json")} {
		if mode := r.mode(path); mode != 0o600 {
			t.Errorf("%s has mode %o", path, mode)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(r.data, networkKeyName)); string(raw) != "the-api-key" {
		t.Errorf("key file holds %q", raw)
	}
	b, err := LoadBootstrap(r.data)
	if err != nil {
		t.Fatal(err)
	}
	if b.NginxHookDir != r.hooks || b.ListenGroup == "" || b.NetworkCredentialFile != filepath.Join(r.data, networkKeyName) {
		t.Errorf("bootstrap: %+v", b)
	}
	if b.ClientDNS != "9.9.9.9" {
		t.Errorf("install lost the owner's setting: %q", b.ClientDNS)
	}
	unit, err := os.ReadFile(filepath.Join(r.units, daemonUnitName))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"RequiredBy", "Before=", "network-online"} {
		if strings.Contains(string(unit), forbidden) {
			t.Errorf("the unit can hold up the boot with %s:\n%s", forbidden, unit)
		}
	}
	if _, err := os.Stat(filepath.Join(r.units, gateway.CoreUnitName)); err != nil {
		t.Error(err)
	}
	daemonBytes, err := os.ReadFile(filepath.Join(r.data, "current", bundleDaemon))
	if err != nil || string(daemonBytes) != "one" {
		t.Errorf("current daemon %q, %v", daemonBytes, err)
	}
	coreBytes, err := os.ReadFile(filepath.Join(r.data, "current", bundleCore))
	if err != nil || !bytes.Equal(coreBytes, r.coreBytes) {
		t.Errorf("current core %q, %v", coreBytes, err)
	}

	before := r.tree()
	r.mustRun(r.request(Install, r.release("one")))
	if !sameTree(before, r.tree()) {
		t.Error("installing the same bundle again changed a file")
	}
	if again, _ := os.ReadFile(ProofPath(r.data)); !bytes.Equal(again, proof) {
		t.Error("a second install changed the proof")
	}
}

func TestInstallLeavesNginxAloneWhenItRefusesTheHook(t *testing.T) {
	r := newInstallRig(t)
	os.WriteFile(filepath.Join(r.root, "nginx-refuses"), nil, 0o644)
	_, out, err := r.run(r.request(Install, r.release("one")))
	if err == nil {
		t.Fatalf("install passed: %s", out)
	}
	if _, err := os.Stat(filepath.Join(r.hooks, hookName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused hook stayed: %v", err)
	}
	if log := r.callLog(); strings.Contains(log, "nginx -s") || strings.Contains(log, "systemctl") {
		t.Errorf("a refused hook must reload and enable nothing:\n%s", log)
	}
	for _, name := range []string{daemonUnitName, gateway.CoreUnitName} {
		if _, err := os.Stat(filepath.Join(r.units, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was written before nginx validated", name)
		}
	}
	proof, _ := os.ReadFile(ProofPath(r.data))
	if len(proof) > 0 && strings.Contains(err.Error(), string(proof)) {
		t.Error("the proof reached an error")
	}

	os.Remove(filepath.Join(r.root, "nginx-refuses"))
	r.mustRun(r.request(Install, r.release("one")))
}

func TestInstallRefusesWhatItCantTrust(t *testing.T) {
	r := newInstallRig(t)
	cases := map[string]string{
		"another core":       r.bundle("bad-core", map[string][]byte{bundleDaemon: []byte("d"), bundleCore: []byte("not pinned")}),
		"an unexpected file": r.bundle("extra", map[string][]byte{bundleDaemon: []byte("d"), bundleCore: r.coreBytes, "../evil": []byte("x")}),
		"no daemon":          r.bundle("nodaemon", map[string][]byte{bundleCore: r.coreBytes}),
	}
	before := r.tree()
	for name, path := range cases {
		if _, _, err := r.run(r.request(Install, path)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if !sameTree(before, r.tree()) {
		t.Error("a refused bundle changed a file")
	}

	os.WriteFile(filepath.Join(r.hooks, "shared-runnable-network.conf"), []byte("location / {}\n"), 0o644)
	if _, _, err := r.run(r.request(Install, r.release("one"))); err == nil {
		t.Error("install guessed a login with no auth include to read")
	}
}

func TestInstallRefusesABadAPIKey(t *testing.T) {
	r := newInstallRig(t)
	request := r.request(Install, r.release("one"))
	request.APIKey = strings.NewReader("two\nlines\n")
	if _, out, err := r.run(request); err == nil {
		t.Errorf("a two-line key was stored: %s", out)
	} else if strings.Contains(err.Error(), "two") && strings.Contains(err.Error(), "lines\n") {
		t.Errorf("the key reached an error: %v", err)
	}
}

func TestInstallWithNoBundleRepairsAfterAFirmwareUpdate(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	os.Remove(filepath.Join(r.units, daemonUnitName))
	os.Remove(filepath.Join(r.units, gateway.CoreUnitName))
	os.Remove(filepath.Join(r.hooks, hookName))
	r.mustRun(r.request(Install, ""))
	for _, path := range []string{filepath.Join(r.units, daemonUnitName), filepath.Join(r.units, gateway.CoreUnitName), filepath.Join(r.hooks, hookName)} {
		if _, err := os.Stat(path); err != nil {
			t.Error(err)
		}
	}
}

func TestUpgradeKeepsTheCoreAndItsPath(t *testing.T) {
	r := newInstallRig(t)
	if _, _, err := r.run(r.request(Upgrade, r.release("two"))); err == nil {
		t.Error("upgrade passed with nothing installed")
	}
	r.mustRun(r.request(Install, r.release("one")))
	corePath, _ := filepath.EvalSymlinks(filepath.Join(r.data, "current", bundleCore))
	old := currentRelease(r.data)
	r.clearCalls()

	plan, _ := r.mustRun(r.request(Upgrade, r.release("two")))
	if plan.RestartsCore {
		t.Error("the same core was marked for a restart")
	}
	log := r.callLog()
	if strings.Contains(log, "restart sidegate-core") {
		t.Errorf("the core was restarted:\n%s", log)
	}
	if !strings.Contains(log, "systemctl restart sidegated.service") {
		t.Errorf("the daemon didn't restart:\n%s", log)
	}
	if after, _ := filepath.EvalSymlinks(filepath.Join(r.data, "current", bundleCore)); after != corePath {
		t.Errorf("the core moved from %s to %s, which the runtime would call outdated", corePath, after)
	}
	raw, _ := os.ReadFile(filepath.Join(r.data, "current", bundleDaemon))
	if string(raw) != "two" {
		t.Errorf("current daemon %q", raw)
	}
	if _, err := os.Stat(releaseDir(r.data, old)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old release stayed: %v", err)
	}
}

// A new core's upgrade says it drops flows. A running core restarts onto it, before the upgrade
// counts as landed, and must stay up. No core runs while no slot needs one, and then the upgrade only
// checks the new core's digest and starts none, since starting one would be a change of its own.
func TestUpgradeToANewCoreSaysSoAndRestartsIt(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(fmt.Sprintf("the core runs %v", running), func(t *testing.T) {
			r := newInstallRig(t)
			r.mustRun(r.request(Install, r.release("one")))
			if running {
				r.runCore()
			}
			oldCore := currentCore(r.data)
			r.coreBytes = []byte("core two")
			r.pinCore()
			r.clearCalls()
			plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("two")))
			if err != nil {
				t.Fatal(err)
			}
			var printed bytes.Buffer
			plan.Print(&printed)
			if !plan.RestartsCore || !strings.Contains(printed.String(), "drops every slot's flows") {
				t.Errorf("the plan hid the core restart: %s", printed.String())
			}
			if err := applyLifecycle(context.Background(), plan, &printed); err != nil {
				t.Fatal(err)
			}
			if running {
				inOrder(t, r.callLog(), "systemctl restart sidegated.service", "systemctl try-restart sidegate-core.service")
				if filepath.Base(filepath.Dir(r.coreExe())) != currentCore(r.data) {
					t.Errorf("the core runs %s", r.coreExe())
				}
			} else if strings.Contains(r.callLog(), "start sidegate-core.service") {
				t.Errorf("an idle core was started:\n%s", r.callLog())
			}
			if _, err := os.Stat(coreDir(r.data, oldCore)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the old core stayed: %v", err)
			}
		})
	}
}

// A new core that doesn't stay up where one served, or one whose binary isn't the pinned one where none
// did, rolls the upgrade back, and a core that served is back on the old release's core and stays up.
func TestANewCoreThatDoesntStayUpRollsBack(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(fmt.Sprintf("the core runs %v", running), func(t *testing.T) {
			r := newInstallRig(t)
			r.mustRun(r.request(Install, r.release("one")))
			one := currentRelease(r.data)
			r.coreBytes = []byte("core two")
			if running {
				r.runCore()
				r.coreBytes = []byte("core two dies")
			}
			r.storeSlot(domain.BlockExit(), domain.Blocked())
			oldCore := r.coreExe()
			r.pinCore()
			request := r.request(Upgrade, r.release("two"))
			request.Outcome = r.outcomeFile()
			plan, err := PlanLifecycle(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if !running {
				at := stepAt(t, plan, "restart the core")
				check := plan.steps[at].do
				plan.steps[at].do = func(ctx context.Context) error {
					core, _ := filepath.EvalSymlinks(filepath.Join(r.data, "current", bundleCore))
					os.WriteFile(core, []byte("damaged"), 0o755)
					return check(ctx)
				}
			}
			r.clearCalls()
			var out strings.Builder
			err = applyLifecycle(context.Background(), plan, &out)
			if outcomeOf(err) != outcomeRolledBack || !strings.Contains(err.Error(), "core") {
				t.Fatalf("an upgrade whose new core fails: %v\n%s", err, out.String())
			}
			if currentRelease(r.data) != one {
				t.Errorf("release %s is current", currentRelease(r.data))
			}
			if running && r.coreExe() != oldCore {
				t.Errorf("the core runs %q, not the old release's %s", r.coreExe(), oldCore)
			}
			if !running && strings.Contains(r.callLog(), "start sidegate-core.service") {
				t.Errorf("the rollback started a core that wasn't running:\n%s", r.callLog())
			}
		})
	}
}

// The units as an earlier release wrote them, which an upgrade's own text would not restore.
func (r *installRig) earlierUnits() map[string]string {
	r.t.Helper()
	units := map[string]string{}
	for _, name := range []string{daemonUnitName, gateway.CoreUnitName} {
		units[name] = "as release one wrote " + name
		if err := os.WriteFile(filepath.Join(r.units, name), []byte(units[name]), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	return units
}

// wantReleaseBack checks what a rollback promises: the release is current, the daemon runs it, and
// its units are back in their files and in systemd.
func (r *installRig) wantReleaseBack(release string, units map[string]string) {
	r.t.Helper()
	if got := currentRelease(r.data); got != release {
		r.t.Errorf("release %s is current, want %s back", got, release)
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(releaseDir(r.data, release), bundleDaemon)); r.runningDaemon() != want || want == "" {
		r.t.Errorf("the daemon runs %q, want release %s's", r.runningDaemon(), release)
	}
	for name, want := range units {
		if raw, _ := os.ReadFile(filepath.Join(r.units, name)); string(raw) != want {
			r.t.Errorf("%s holds %q, want %q back", name, raw, want)
		}
		if raw, _ := os.ReadFile(filepath.Join(r.loaded(), name)); string(raw) != want {
			r.t.Errorf("systemd holds %s as %q, want %q back", name, raw, want)
		}
	}
	if _, err := os.Stat(journalDir(r.data)); !errors.Is(err, os.ErrNotExist) {
		r.t.Errorf("the journal outlived the upgrade: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(r.data, "releases")); len(entries) != 1 {
		r.t.Errorf("releases left: %v, want %s alone", entries, release)
	}
}

func TestAnUpgradeWhoseDaemonDoesntAnswerPutsTheOldReleaseBack(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	units := r.earlierUnits()
	r.clearCalls()

	plan, out, err := r.run(r.request(Upgrade, r.release("broken")))
	if err == nil || !strings.Contains(err.Error(), "Release "+one+" is back and answers") {
		t.Fatalf("upgrade to a daemon that doesn't answer: %v\n%s", err, out)
	}
	var printed bytes.Buffer
	plan.Print(&printed)
	if !strings.Contains(printed.String(), "release "+one+" goes back") {
		t.Errorf("the plan doesn't say what goes back:\n%s", printed.String())
	}
	r.wantReleaseBack(one, units)
	inOrder(t, r.callLog(), "systemctl restart sidegated.service", "systemctl daemon-reload", "systemctl restart sidegated.service")
	if _, _, err := r.run(r.request(Upgrade, r.release("two"))); err != nil {
		t.Errorf("an upgrade after the rollback: %v", err)
	}
}

// Killed after the new release is current, the upgrade run again still knows what answered.
func TestAnInterruptedUpgradeRunAgainPutsBackTheReleaseThatAnswered(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	one := currentRelease(r.data)
	units := r.earlierUnits()
	broken := r.release("broken")
	plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, broken))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plan.steps[:stepAt(t, plan, "rewrite the units")+1] {
		if err := s.do(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if currentRelease(r.data) == one {
		t.Fatal("the new release isn't current yet, so this test proves nothing")
	}

	plan, out, err := r.run(r.request(Upgrade, broken))
	if plan.Prior != one || err == nil {
		t.Fatalf("the upgrade run again would put back %s, not %s: %v\n%s", plan.Prior, one, err, out)
	}
	r.wantReleaseBack(one, units)
}

// Killed after the new release is current, an upgrade that install then finishes leaves no record
// of the release before it, so the next failed upgrade puts back the release install made answer.
func TestAnInstallAfterAnInterruptedUpgradeForgetsTheReleaseBeforeIt(t *testing.T) {
	for _, bundled := range []bool{true, false} {
		t.Run(fmt.Sprintf("bundled %v", bundled), func(t *testing.T) {
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
			if !bundled {
				two = ""
			}
			r.mustRun(r.request(Install, two))
			units := map[string]string{}
			for _, name := range []string{daemonUnitName, gateway.CoreUnitName} {
				raw, _ := os.ReadFile(filepath.Join(r.units, name))
				units[name] = string(raw)
			}

			_, out, err := r.run(r.request(Upgrade, r.release("broken")))
			if err == nil || !strings.Contains(err.Error(), "Release "+r.releaseOf("two")+" is back and answers") {
				t.Fatalf("the next upgrade, whose daemon doesn't answer: %v\n%s", err, out)
			}
			r.wantReleaseBack(r.releaseOf("two"), units)
		})
	}
}

// coreLeftBehind is an upgrade from one to two, each with a core of its own, killed once two's
// daemon answered and before it restarted the core: two is current, the journal still names one,
// and the core runs one's. It returns the binary the core runs.
func (r *installRig) coreLeftBehind() string {
	r.t.Helper()
	r.mustRun(r.request(Install, r.release("one")))
	r.runCore()
	old := r.coreExe()
	r.coreBytes = []byte("core two")
	r.pinCore()
	plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, r.release("two")))
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range plan.steps[:stepAt(r.t, plan, "restart the core")] {
		if err := s.do(context.Background()); err != nil {
			r.t.Fatal(err)
		}
	}
	if r.coreExe() != old {
		r.t.Fatal("the core restarted before the kill, so this test proves nothing")
	}
	return old
}

// Killed before it restarts the core, an upgrade to a new core run again restarts it, and the
// binary the core runs is never deleted under it.
func TestAnUpgradeRunAgainRestartsTheCoreItDidntRestart(t *testing.T) {
	r := newInstallRig(t)
	old := r.coreLeftBehind()

	plan, _ := r.mustRun(r.request(Upgrade, r.release("two")))
	if !plan.RestartsCore {
		t.Error("the upgrade run again doesn't say the core restarts")
	}
	if r.coreExe() == old {
		t.Errorf("the core still runs %s", old)
	}
	if _, err := os.Stat(r.coreExe()); err != nil {
		t.Errorf("the binary the core runs is gone: %v", err)
	}
}

// A failed upgrade after that kill puts one back, the release that last finished a switch, and
// restarts no core, since the core still runs one's binary, which the rollback's prune leaves.
func TestARollbackAfterAnInterruptedUpgradeKeepsTheBinaryTheCoreRuns(t *testing.T) {
	r := newInstallRig(t)
	old := r.coreLeftBehind()
	record, err := readRecord(r.data)
	if err != nil {
		t.Fatal(err)
	}
	one := record.prior(r.data)
	r.clearCalls()

	_, out, err := r.run(r.request(Upgrade, r.release("broken")))
	if err == nil || !strings.Contains(err.Error(), "Release "+one+" is back and answers") {
		t.Fatalf("the upgrade whose daemon doesn't answer: %v\n%s", err, out)
	}
	if r.coreExe() != old {
		t.Fatalf("the core runs %s, not %s", r.coreExe(), old)
	}
	if strings.Contains(r.callLog(), "restart sidegate-core.service") {
		t.Errorf("the rollback restarted a core that runs the right binary:\n%s", r.callLog())
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the rollback deleted the binary the core runs: %v", err)
	}
}

// An upgrade that can't tell which binary the core runs deletes no core.
func TestAnUpgradeThatCantNameTheRunningCoreDeletesNoCore(t *testing.T) {
	for name, blind := range map[string]func(r *installRig){
		"systemd doesn't answer": func(r *installRig) {
			os.WriteFile(filepath.Join(r.root, "bin", "systemctl"), []byte("#!/bin/sh\n[ \"$1\" = show ] && exit 1\nexit 0\n"), 0o755)
		},
		"its process can't be read": func(r *installRig) {
			os.Remove(filepath.Join(r.root, "proc", "4242", "exe"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			old := r.coreLeftBehind()
			blind(r)
			r.mustRun(r.request(Upgrade, r.release("two")))
			if _, err := os.Stat(old); err != nil {
				t.Errorf("the upgrade deleted %s, which the core may run: %v", old, err)
			}
		})
	}
}

// A rollback record a disk fault left unreadable stops no install: install doesn't read it, and
// removes it once its daemon answers.
func TestAnInstallRepairsPastAnUnreadableRollbackRecord(t *testing.T) {
	r := newInstallRig(t)
	r.mustRun(r.request(Install, r.release("one")))
	if err := os.MkdirAll(journalDir(r.data), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalRecord(r.data), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.mustRun(r.request(Install, ""))
	if _, err := os.Stat(journalDir(r.data)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record outlived the install: %v", err)
	}
}

// toCommit is an upgrade from one to two whose new daemon answered, stopped before it keeps two.
// It returns two's units and the request it ran.
func (r *installRig) toCommit() (map[string]string, LifecycleRequest) {
	r.t.Helper()
	r.mustRun(r.request(Install, r.release("one")))
	r.earlierUnits()
	request := r.request(Upgrade, r.release("two"))
	request.Outcome = r.outcomeFile()
	plan, err := PlanLifecycle(context.Background(), request)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range plan.steps[:stepAt(r.t, plan, "keep release")] {
		if err := s.do(context.Background()); err != nil {
			r.t.Fatal(err)
		}
	}
	units := map[string]string{}
	for _, name := range []string{daemonUnitName, gateway.CoreUnitName} {
		raw, _ := os.ReadFile(filepath.Join(r.units, name))
		units[name] = string(raw)
	}
	return units, request
}

// Keeping the new release marks the record kept before it says landed or forgets the record, so a
// kill anywhere in it leaves two selected: the next start's recovery finishes the keeping, and the
// next upgrade whose daemon doesn't answer puts two back, the release that answered.
func TestAKillWhileKeepingTheNewReleaseKeepsIt(t *testing.T) {
	for _, killed := range []string{"once the record says kept", "once landed is said", "once the record is renamed away"} {
		t.Run(killed, func(t *testing.T) {
			r := newInstallRig(t)
			two, request := r.toCommit()
			record, err := readRecord(r.data)
			if err != nil {
				t.Fatal(err)
			}
			record.Phase = phaseCommitted
			if err := record.write(r.data); err != nil {
				t.Fatal(err)
			}
			if killed != "once the record says kept" {
				if err := record.say(outcomeLanded, ""); err != nil {
					t.Fatal(err)
				}
			}
			if killed == "once the record is renamed away" {
				if err := os.Rename(journalDir(r.data), journalDir(r.data)+".old"); err != nil {
					t.Fatal(err)
				}
			}
			recover := r.request(Recover, "")
			recover.Outcome = ""
			if _, out, err := r.run(recover); err != nil {
				t.Fatalf("recover: %v\n%s", err, out)
			}
			if got := currentRelease(r.data); got != r.releaseOf("two") {
				t.Errorf("release %s is current after recovery, want two kept", got)
			}
			if ended := r.readOutcome(); ended.Outcome != outcomeLanded {
				t.Errorf("the outcome says %s", ended.Outcome)
			}
			if _, err := os.Stat(journalDir(r.data)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the journal outlived the recovery: %v", err)
			}
			request.BundlePath = r.release("broken")
			_, out, err := r.run(request)
			if err == nil || !strings.Contains(err.Error(), "Release "+r.releaseOf("two")+" is back and answers") {
				t.Fatalf("the next upgrade, whose daemon doesn't answer: %v\n%s", err, out)
			}
			r.wantReleaseBack(r.releaseOf("two"), two)
		})
	}
}

func (r *installRig) releaseOf(daemon string) string {
	r.t.Helper()
	b, err := inspectBundle(r.release(daemon))
	if err != nil {
		r.t.Fatal(err)
	}
	return b.release()
}

type lifecycleSyncer struct {
	inventory  unifi.Inventory
	report     unifi.Report
	err        error
	reconciled [][]unifi.Want
}

func (s *lifecycleSyncer) Inspect(context.Context) (unifi.Inventory, error) {
	return s.inventory, s.err
}

func (s *lifecycleSyncer) Reconcile(_ context.Context, wants []unifi.Want) (unifi.Report, error) {
	s.reconciled = append(s.reconciled, wants)
	return s.report, s.err
}

func (r *installRig) saveOwnersWork() {
	r.t.Helper()
	for _, name := range keptWithoutPurge {
		if err := os.WriteFile(filepath.Join(r.data, name), []byte("saved"), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
}

func TestUninstallKeepsTheOwnersWorkAndReportsRoutes(t *testing.T) {
	r := newInstallRig(t)
	before := r.tree()
	syncer := &lifecycleSyncer{inventory: unifi.Inventory{Owned: []unifi.Owned{
		{NetworkID: domain.NetworkID("n1"), Name: "EU", Routes: []unifi.Route{{Name: "example.com", Target: "n1"}}},
		{NetworkID: domain.NetworkID("n2"), Name: "US"},
	}}}
	r.syncer = syncer
	r.mustRun(r.request(Install, r.release("one")))
	r.saveOwnersWork()
	r.clearCalls()

	_, out := r.mustRun(r.request(Uninstall, ""))
	if !strings.Contains(out, `Route "example.com" still targets our VPN Client "EU"`) {
		t.Errorf("the route wasn't reported:\n%s", out)
	}
	if len(syncer.reconciled) != 0 {
		t.Errorf("uninstall without --purge wrote to UniFi: %v", syncer.reconciled)
	}
	inOrder(t, r.callLog(), "systemctl disable --now sidegated.service", "nginx -t", "nginx -s reload", "systemctl daemon-reload")
	if r.tornDown != 1 {
		t.Errorf("teardown ran %d times", r.tornDown)
	}
	entries, _ := os.ReadDir(r.data)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if strings.Join(left, " ") != "network-key state.db state.db-shm state.db-wal" {
		t.Errorf("left in the data dir: %v", left)
	}
	onHost := func(tree map[string]string) map[string]string {
		for path := range tree {
			if strings.HasPrefix(path, r.data) {
				delete(tree, path)
			}
		}
		return tree
	}
	if !sameTree(onHost(before), onHost(r.tree())) {
		t.Error("units or the hook directory differ from before install")
	}

	r.mustRun(r.request(Uninstall, ""))
	if r.tornDown != 2 {
		t.Errorf("a second uninstall didn't converge again: %d", r.tornDown)
	}
}

func TestPurgeRemovesSavedWorkAndOnlyUnroutedClients(t *testing.T) {
	r := newInstallRig(t)
	syncer := &lifecycleSyncer{report: unifi.Report{
		Removed:  []domain.NetworkID{"n2"},
		Retained: []unifi.Retained{{NetworkID: "n1", Name: "EU", Problem: &unifi.Problem{Detail: "1 route targets it"}}},
	}}
	r.syncer = syncer
	r.mustRun(r.request(Install, r.release("one")))
	r.saveOwnersWork()
	request := r.request(Uninstall, "")
	request.Purge = true
	_, out := r.mustRun(request)
	if _, err := os.Stat(r.data); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data dir stayed: %v", err)
	}
	if len(syncer.reconciled) != 1 || len(syncer.reconciled[0]) != 0 {
		t.Errorf("reconciled %v", syncer.reconciled)
	}
	if !strings.Contains(out, "Deleted our VPN Client n2") || !strings.Contains(out, `"EU" (n1) stays`) {
		t.Errorf("output:\n%s", out)
	}
}

func TestUninstallFinishesWhenUniFiIsDownOrNginxIsBroken(t *testing.T) {
	r := newInstallRig(t)
	r.syncer = &lifecycleSyncer{err: errors.New("unavailable")}
	r.mustRun(r.request(Install, r.release("one")))
	os.WriteFile(filepath.Join(r.root, "nginx-refuses"), nil, 0o644)
	_, out := r.mustRun(r.request(Uninstall, ""))
	if !strings.Contains(out, "aren't known") || !strings.Contains(out, "wasn't reloaded") {
		t.Errorf("output:\n%s", out)
	}
	if strings.Contains(r.callLog()[strings.LastIndex(r.callLog(), "disable"):], "nginx -s reload") {
		t.Errorf("nginx was reloaded while it didn't validate:\n%s", r.callLog())
	}
	if _, err := os.Stat(filepath.Join(r.units, daemonUnitName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the unit stayed")
	}
}

func TestLifecycleRefusesADataDirThatIsntPlainlyOurs(t *testing.T) {
	r := newInstallRig(t)
	request := r.request(Uninstall, "")
	request.DataDir, request.Purge = "/data", true
	if _, err := PlanLifecycle(context.Background(), request); err == nil {
		t.Error("purging /data was planned")
	}
}

func TestEnsureNginxPutsTheHookBack(t *testing.T) {
	r := newInstallRig(t)
	keeper := nginxKeeper{hookDir: r.hooks, dataDir: r.data, proof: []byte("p")}
	path := filepath.Join(r.hooks, hookName)
	ctx := context.Background()

	if err := keeper.ensureNginx(ctx); err != nil {
		t.Fatal(err)
	}
	inOrder(t, r.callLog(), "nginx -t", "nginx -s reload")
	hook, _ := os.ReadFile(path)
	if !strings.Contains(string(hook), "location ^~ "+DefaultBasePath+" {") || strings.Contains(string(hook), "@") {
		t.Errorf("the hook doesn't serve %s, or keeps a placeholder:\n%s", DefaultBasePath, hook)
	}

	r.clearCalls()
	if err := keeper.ensureNginx(ctx); err != nil {
		t.Fatal(err)
	}
	if r.callLog() != "" {
		t.Errorf("a right hook still called:\n%s", r.callLog())
	}

	os.Remove(path)
	if err := keeper.ensureNginx(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the hook didn't come back")
	}
	inOrder(t, r.callLog(), "nginx -t", "nginx -s reload")

	r.clearCalls()
	os.Chmod(path, 0o644)
	if err := keeper.ensureNginx(ctx); err != nil {
		t.Fatal(err)
	}
	if mode := r.mode(path); mode != 0o600 {
		t.Errorf("a hook others could read stayed at %o", mode)
	}
}

func TestEnsureNginxReloadsOnlyWhatValidates(t *testing.T) {
	r := newInstallRig(t)
	keeper := nginxKeeper{hookDir: r.hooks, dataDir: r.data, proof: []byte("p")}
	path := filepath.Join(r.hooks, hookName)
	ctx := context.Background()
	os.WriteFile(filepath.Join(r.root, "nginx-refuses"), nil, 0o644)

	if err := keeper.ensureNginx(ctx); err == nil {
		t.Error("a refused hook passed")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused hook stayed, and would fail UniFi's own next reload")
	}
	if strings.Contains(r.callLog(), "nginx -s") {
		t.Errorf("reloaded:\n%s", r.callLog())
	}

	r.clearCalls()
	if err := keeper.ensureNginx(ctx); err == nil {
		t.Error("a hook refused a moment ago was taken")
	}
	if r.callLog() != "" {
		t.Errorf("a refused hook was written again at once, and flash wears:\n%s", r.callLog())
	}

	hourAgo := time.Now().Add(-2 * hookRetryEvery)
	if err := os.Chtimes(refusedPath(r.data), hourAgo, hourAgo); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(r.root, "nginx-refuses"))
	if err := keeper.ensureNginx(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the hook didn't come back after nginx recovered")
	}
}

// A daemon that crash-loops starts every 10 s, and each start is a new keeper. A hook nginx refused
// within the hour isn't written again by any of them, so the flash doesn't wear.
func TestARefusedHookOutlivesTheDaemonThatRefusedIt(t *testing.T) {
	r := newInstallRig(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(r.root, "nginx-refuses"), nil, 0o644)
	first := nginxKeeper{hookDir: r.hooks, dataDir: r.data, proof: []byte("p")}
	if err := first.ensureNginx(ctx); err == nil {
		t.Fatal("a refused hook passed")
	}
	r.clearCalls()
	next := nginxKeeper{hookDir: r.hooks, dataDir: r.data, proof: []byte("p")}
	if err := next.ensureNginx(ctx); err == nil {
		t.Error("the next daemon took a hook refused a moment ago")
	}
	if log := r.callLog(); log != "" {
		t.Errorf("the next daemon wrote the refused hook again:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(r.hooks, hookName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused hook is back: %v", err)
	}
}

// An upgrade is the operator asking, as an install is: the daemon it starts tries a hook nginx
// refused a moment ago at once, rather than leaving the page closed for the rest of the hour.
func TestAnUpgradeTriesARefusedHookAgainAtOnce(t *testing.T) {
	r := newInstallRig(t)
	ctx := context.Background()
	r.mustRun(r.request(Install, r.release("one")))
	running := nginxKeeper{hookDir: r.hooks, dataDir: r.data, proof: loadProof(r.data)}
	os.Remove(filepath.Join(r.hooks, hookName))
	os.WriteFile(filepath.Join(r.root, "nginx-refuses-hook"), nil, 0o644)
	if err := running.ensureNginx(ctx); err == nil {
		t.Fatal("a refused hook passed")
	}
	os.Remove(filepath.Join(r.root, "nginx-refuses-hook"))
	r.clearCalls()

	plan, _ := r.mustRun(r.request(Upgrade, r.release("two")))
	var printed bytes.Buffer
	plan.Print(&printed)
	inOrder(t, printed.String(), "forget that nginx refused the hook", "restart sidegated.service only")
	inOrder(t, r.callLog(), "systemctl restart sidegated.service\nrefused no")
	upgraded := nginxKeeper{hookDir: r.hooks, dataDir: r.data, proof: loadProof(r.data)}
	if err := upgraded.ensureNginx(ctx); err != nil {
		t.Errorf("the upgraded daemon kept the page closed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.hooks, hookName)); err != nil {
		t.Errorf("the upgraded daemon didn't put the hook back: %v", err)
	}
}

// A test of the hook that never ran is no refusal: the next daemon, with nginx healthy, puts the
// hook in at once.
func TestAHookTestThatDidntRunIsTriedAgain(t *testing.T) {
	for name, cut := range map[string]func(t *testing.T) context.Context{
		"cancelled": func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		},
		"nginx won't start": func(t *testing.T) context.Context {
			t.Setenv("PATH", t.TempDir())
			return context.Background()
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			path := os.Getenv("PATH")
			first := nginxKeeper{hookDir: r.hooks, dataDir: r.data, proof: []byte("p")}
			if err := first.ensureNginx(cut(t)); err == nil {
				t.Fatal("a hook whose test didn't run passed")
			}
			os.Setenv("PATH", path)
			next := nginxKeeper{hookDir: r.hooks, dataDir: r.data, proof: []byte("p")}
			if err := next.ensureNginx(context.Background()); err != nil {
				t.Errorf("the next daemon, with nginx healthy: %v", err)
			}
			if _, err := os.Stat(filepath.Join(r.hooks, hookName)); err != nil {
				t.Errorf("the hook isn't back: %v", err)
			}
		})
	}
}

func TestEnsureNginxClosesThePageWhenTheLoginContractGoes(t *testing.T) {
	r := newInstallRig(t)
	keeper := nginxKeeper{hookDir: r.hooks, dataDir: r.data, proof: []byte("p")}
	ctx := context.Background()
	if err := keeper.ensureNginx(ctx); err != nil {
		t.Fatal(err)
	}
	r.clearCalls()
	os.Remove(filepath.Join(r.hooks, "shared-runnable-network.conf"))
	if err := keeper.ensureNginx(ctx); err == nil {
		t.Error("no error with no network file")
	}
	if _, err := os.Stat(filepath.Join(r.hooks, hookName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the hook outlived the login it copied")
	}
	inOrder(t, r.callLog(), "nginx -t", "nginx -s reload")
}

// installKilledAtTest runs install in a process of its own, which the stand-in nginx kills at
// nginx -t: after install wrote the hook and before nginx tested it.
func (r *installRig) installKilledAtTest(bundle string) {
	r.t.Helper()
	kills := filepath.Join(r.root, "nginx-kills")
	if err := os.WriteFile(kills, nil, 0o644); err != nil {
		r.t.Fatal(err)
	}
	defer os.Remove(kills)
	installer := exec.Command(os.Args[0], "-test.run=^TestKilledInstaller$")
	installer.Env = append(os.Environ(), "KILLED_INSTALLER_ROOT="+r.root, "KILLED_INSTALLER_BUNDLE="+bundle)
	out, err := installer.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		r.t.Fatalf("the installer wasn't killed at nginx -t: %v\n%s", err, out)
	}
}

// TestKilledInstaller is the installer installKilledAtTest kills.
func TestKilledInstaller(t *testing.T) {
	root := os.Getenv("KILLED_INSTALLER_ROOT")
	if root == "" {
		t.Skip("installKilledAtTest runs this in a process it kills")
	}
	r := rigAt(t, root)
	r.pinCore()
	_, out, err := r.run(r.request(Install, os.Getenv("KILLED_INSTALLER_BUNDLE")))
	t.Fatalf("install outlived nginx -t: %v\n%s", err, out)
}

// An install killed between writing a hook nginx refuses and testing it leaves a hook that would
// fail UniFi's own next reload. The next install, uninstall or daemon start tests it, after a first
// install too, which was killed before it wrote bootstrap.json's hook dir.
func TestAHookAKilledInstallLeftIsTestedByTheNextRun(t *testing.T) {
	ctx := context.Background()
	for name, next := range map[string]func(*installRig){
		"install": func(r *installRig) { r.run(r.request(Install, r.release("one"))) },
		"uninstall": func(r *installRig) {
			if _, out, err := r.run(r.request(Uninstall, "")); err != nil {
				t.Errorf("uninstall: %v\n%s", err, out)
			}
		},
		"daemon start": func(r *installRig) {
			b, err := LoadBootstrap(r.data)
			if err != nil {
				t.Fatal(err)
			}
			app := &App{bootstrap: b, proof: loadProof(r.data)}
			if app.keepsNginx(ctx) {
				keeper := nginxKeeper{hookDir: b.NginxHookDir, dataDir: b.DataDir, proof: app.proof}
				keeper.ensureNginx(ctx)
			}
		},
	} {
		for _, first := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, first install %t", name, first), func(t *testing.T) {
				r := newInstallRig(t)
				if !first {
					r.mustRun(r.request(Install, r.release("one")))
					os.Remove(filepath.Join(r.hooks, hookName))
				}
				os.WriteFile(filepath.Join(r.root, "nginx-refuses-hook"), nil, 0o644)
				r.installKilledAtTest(r.release("one"))
				if _, err := runTool(ctx, "nginx", "-t"); err == nil {
					t.Fatal("nginx takes what the killed install left, so this test proves nothing")
				}

				next(r)
				if _, err := runTool(ctx, "nginx", "-t"); err != nil {
					t.Errorf("after the next %s, nginx -t still fails: %v", name, err)
				}
			})
		}
	}
}

func TestReadAuthInclude(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"as the gateway has it": {map[string]string{"shared-runnable-network.conf": networkFile}, "include /data/unifi-core/config/http/auth.conf;"},
		"a relative include":    {map[string]string{"shared-runnable-network.conf": "  include auth.conf;\n"}, "include auth.conf;"},
		"the same in two files": {map[string]string{"shared-runnable-network.conf": networkFile, "shared-runnable-network-v2.conf": networkFile}, "include /data/unifi-core/config/http/auth.conf;"},
		"two different logins":  {map[string]string{"shared-runnable-network.conf": "include a/auth.conf;\ninclude b/auth.conf;\n"}, ""},
		"none":                  {map[string]string{"shared-runnable-network.conf": "include cors.conf;\n"}, ""},
		"only another app's":    {map[string]string{"shared-runnable-protect.conf": networkFile}, ""},
		"a commented one":       {map[string]string{"shared-runnable-network.conf": "# include auth.conf;\n"}, ""},
	} {
		dir := t.TempDir()
		for file, text := range c.files {
			os.WriteFile(filepath.Join(dir, file), []byte(text), 0o644)
		}
		got, err := readAuthInclude(dir)
		if got != c.want || (err != nil) != (c.want == "") {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
}
