package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"sidegate/deploy"
)

const (
	hookName = "shared-runnable-sidegate.conf"
	// A hook nginx refused is tried again this often, since the cause may be someone else's
	// and every try writes to flash.
	hookRetryEvery = time.Hour
	nginxKeepEvery = time.Minute
)

var authIncludeLine = regexp.MustCompile(`(?m)^\s*(include\s+\S*auth\.conf\s*;)\s*$`)

// readAuthInclude reads the login from the generated network file beside the hook, so the
// page's login is whatever UniFi's own apps use. Finding none, or more than one, means the
// contract changed, and no hook is better than a guessed one.
func readAuthInclude(hookDir string) (string, error) {
	files, err := filepath.Glob(filepath.Join(hookDir, "shared-runnable-*network*.conf"))
	if err != nil {
		return "", err
	}
	var includes []string
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		for _, match := range authIncludeLine.FindAllSubmatch(data, -1) {
			includes = append(includes, string(match[1]))
		}
	}
	slices.Sort(includes)
	includes = slices.Compact(includes)
	if len(includes) != 1 {
		return "", fmt.Errorf("the network file in %s has %d auth includes, want 1", hookDir, len(includes))
	}
	return includes[0], nil
}

func renderHook(authInclude string, proof []byte) []byte {
	return []byte(strings.NewReplacer("@AUTH_INCLUDE@", authInclude, "@PROOF@", string(proof),
		"@AUTH_HEADER@", AuthHeader, "@PROOF_HEADER@", ProofHeader, "@SOCKET@", DefaultListenSocket,
		"@PAGE@", strings.TrimSuffix(DefaultBasePath, "/")).Replace(deploy.NginxHook))
}

// runNginx runs nginx with the proof kept out of whatever it prints.
func runNginx(ctx context.Context, proof []byte, args ...string) (string, error) {
	out, err := runTool(ctx, "nginx", args...)
	if len(proof) > 0 {
		out = strings.ReplaceAll(out, string(proof), "<proof>")
		if err != nil {
			err = redacted{strings.ReplaceAll(err.Error(), string(proof), "<proof>"), errors.Unwrap(err)}
		}
	}
	return out, err
}

// redacted is a tool's error with the proof cut out of its text. It unwraps to the process's own
// error, whose text holds none of the output.
type redacted struct {
	text  string
	cause error
}

func (e redacted) Error() string { return e.text }
func (e redacted) Unwrap() error { return e.cause }

// refusedBy says nginx ran and exited with an error, as it does for a config it refuses. A test
// cancelled, killed or never started says nothing of the hook.
func refusedBy(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() > 0
}

func runTool(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// nginxKeeper keeps our hook in unifi-core's directory, which unifi-core may rewrite.
type nginxKeeper struct {
	hookDir string
	dataDir string
	proof   []byte
}

// owedPath marks a hook nginx hasn't yet validated and loaded, and holds the hook's dir. It is
// written before the hook changes and removed once nginx reloaded it or a refused hook is gone. A
// file, not a field, so a write a kill cut short is still tested by the next install, uninstall or
// daemon start, where nginx would otherwise be left holding a hook that fails UniFi's own next
// reload. The dir is in it because a first install killed there never wrote bootstrap.json's. A
// boot can also put the hook back before nginx is up to reload it.
func (k *nginxKeeper) owedPath() string { return owedPath(k.dataDir) }

func owedPath(dataDir string) string { return filepath.Join(dataDir, "nginx-owed") }

func (k *nginxKeeper) owe() error { return writeFileAtomic(k.owedPath(), []byte(k.hookDir), 0o600) }

func (k *nginxKeeper) owed() bool {
	_, err := os.Stat(k.owedPath())
	return err == nil
}

// refusedPath holds the digest of the hook nginx last refused, and its time is when. A file, not a
// field, so a daemon that crash-loops every 10 s doesn't write a refused hook to flash at each start.
func refusedPath(dataDir string) string { return filepath.Join(dataDir, "nginx-refused") }

func hookDigest(hook []byte) []byte {
	sum := sha256.Sum256(hook)
	return []byte(hex.EncodeToString(sum[:]))
}

// refusedAgo says how long ago nginx refused hook, if it did within hookRetryEvery.
func (k *nginxKeeper) refusedAgo(hook []byte) (time.Duration, bool) {
	path := refusedPath(k.dataDir)
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	digest, err := os.ReadFile(path)
	ago := time.Since(info.ModTime())
	return ago, err == nil && bytes.Equal(digest, hookDigest(hook)) && ago < hookRetryEvery
}

// ensureNginx converges the hook. It changes nothing while the hook is right. A changed
// hook is validated before any reload, and one nginx refuses is removed, because left in
// place it would also fail UniFi's own next reload of nginx. With no login to copy the
// hook goes too, which closes the page.
func (k *nginxKeeper) ensureNginx(ctx context.Context) error {
	path := filepath.Join(k.hookDir, hookName)
	include, contractErr := readAuthInclude(k.hookDir)
	var want []byte
	if contractErr == nil {
		want = renderHook(include, k.proof)
	}
	have, err := os.ReadFile(path)
	present := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ago, refused := k.refusedAgo(want)
	switch {
	case contractErr != nil && present:
		if err := k.owe(); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	case contractErr != nil:
	case !present && refused:
		return fmt.Errorf("nginx refused this hook %s ago", ago.Round(time.Second))
	case !present || !bytes.Equal(have, want) || !rootOnly(path):
		if err := k.owe(); err != nil {
			return err
		}
		if err := writeFileAtomic(path, want, 0o600); err != nil {
			return err
		}
	}
	if !k.owed() {
		return contractErr
	}
	if contractErr != nil {
		if _, err := runNginx(ctx, k.proof, "-t"); err != nil {
			return errors.Join(contractErr, err)
		}
		return errors.Join(contractErr, k.reload(ctx))
	}
	refused, err = k.settle(ctx)
	if refused {
		err = errors.Join(err, writeFileAtomic(refusedPath(k.dataDir), hookDigest(want), 0o600))
	}
	return err
}

// settle tests the owed hook. One whose test fails is removed and nothing is owed, and one nginx
// takes is reloaded. Only a test that ran and failed is a refusal.
func (k *nginxKeeper) settle(ctx context.Context) (refused bool, err error) {
	if _, err := runNginx(ctx, k.proof, "-t"); err != nil {
		refused = refusedBy(err)
		failure := fmt.Errorf("nginx refused the hook, so it's removed: %w", err)
		if !refused {
			failure = fmt.Errorf("the hook's test didn't run, so it's removed: %w", err)
		}
		if err := removeIfPresent(filepath.Join(k.hookDir, hookName)); err != nil {
			return refused, errors.Join(failure, err)
		}
		return refused, errors.Join(failure, removeIfPresent(k.owedPath()))
	}
	return false, k.reload(ctx)
}

func (k *nginxKeeper) reload(ctx context.Context) error {
	if _, err := runNginx(ctx, k.proof, "-s", "reload"); err != nil {
		return err
	}
	return removeIfPresent(k.owedPath())
}

func rootOnly(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0o077 == 0
}

// removeIfPresent syncs the directory after a removal, so it lasts past a power cut.
func removeIfPresent(path string) error {
	err := os.Remove(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	return syncDir(filepath.Dir(path))
}

// keepsNginx says whether the nginx actor runs. A daemon that doesn't keep the hook leaves nginx
// alone, but for a hook a killed install left owed, which it settles here.
func (a *App) keepsNginx(ctx context.Context) bool {
	if a.bootstrap.NginxHookDir != "" && len(a.proof) > 0 {
		return true
	}
	dir, err := os.ReadFile(owedPath(a.bootstrap.DataDir))
	if err != nil {
		return false
	}
	keeper := nginxKeeper{hookDir: string(dir), dataDir: a.bootstrap.DataDir, proof: a.proof}
	if _, err := keeper.settle(ctx); err != nil {
		log.Printf("nginx hook: %s", err)
	}
	return false
}

// keepNginx is the actor. An error is logged once until it changes, so a closed page
// doesn't fill the journal.
func (a *App) keepNginx(ctx context.Context) error {
	keeper := nginxKeeper{hookDir: a.bootstrap.NginxHookDir, dataDir: a.bootstrap.DataDir, proof: a.proof}
	tick := time.NewTicker(nginxKeepEvery)
	defer tick.Stop()
	last := ""
	for {
		text := ""
		if err := keeper.ensureNginx(ctx); err != nil {
			text = err.Error()
		}
		if text != last && text != "" {
			log.Printf("nginx hook: %s", text)
		}
		last = text
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
