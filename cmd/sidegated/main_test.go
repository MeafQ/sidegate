package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sidegate/internal/daemon"
)

const (
	linkedVersion  = "v0.0.7"
	linkedReleases = "https://203.0.113.80/OWNER/sidegate/releases"
	linkedRelease  = "0123456789ab"
	linkedDataDir  = "SIDEGATE_LINKED_DATA_DIR"
)

// The daemon built with scripts/build-bundle.sh's -X flags says the version it was given, then
// the releases address, which a checkout build doesn't have. The
// test then builds itself with the same values, since a test's main package links under its
// import path, puts that binary where an install puts a release's daemon and runs it there: its
// api/session, from serve's own wiring, names the version and the release directory it runs from.
func TestTheSessionCarriesTheVersionTheBuildLinked(t *testing.T) {
	if dataDir := os.Getenv(linkedDataDir); dataDir != "" {
		answerAsTheLinkedBuild(t, dataDir)
		return
	}
	linked := func(pkg string) string {
		return "-X " + pkg + ".version=" + linkedVersion + " -X " + pkg + ".releases=" + linkedReleases
	}
	dir := t.TempDir()
	daemonBinary := filepath.Join(dir, "sidegated")
	if out, err := exec.Command("go", "build", "-trimpath", "-ldflags", linked("main"), "-o", daemonBinary, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	if out, err := exec.Command(daemonBinary, "version").Output(); err != nil || string(out) != linkedVersion+"\n"+linkedReleases+"\n" {
		t.Fatalf("sidegated version: %q %v", out, err)
	}
	checkoutBinary := filepath.Join(dir, "checkout-sidegated")
	if out, err := exec.Command("go", "build", "-trimpath", "-o", checkoutBinary, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	if out, err := exec.Command(checkoutBinary, "version").Output(); err != nil || string(out) != "dev\n" {
		t.Fatalf("a checkout build's sidegated version: %q %v", out, err)
	}

	dataDir := filepath.Join(dir, "data")
	release := filepath.Join(dataDir, "releases", linkedRelease)
	if err := os.MkdirAll(release, 0o700); err != nil {
		t.Fatal(err)
	}
	testBinary := filepath.Join(release, "sidegated")
	if out, err := exec.Command("go", "test", "-c", "-ldflags", linked("sidegate/cmd/sidegated"), "-o", testBinary, ".").CombinedOutput(); err != nil {
		t.Fatalf("go test -c: %v\n%s", err, out)
	}
	run := exec.Command(testBinary, "-test.run", "^"+t.Name()+"$", "-test.v")
	run.Env = append(os.Environ(), linkedDataDir+"="+dataDir)
	out, err := run.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("the linked build: %v\n%s", err, out)
	}
}

func answerAsTheLinkedBuild(t *testing.T, dataDir string) {
	if version != linkedVersion || releases != linkedReleases {
		t.Fatalf("linked as %q from %q", version, releases)
	}
	bootstrap := `{"runDir": ` + quote(filepath.Join(dataDir, "run")) + `}`
	if err := os.WriteFile(filepath.Join(dataDir, "bootstrap.json"), []byte(bootstrap), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon.ProofPath(dataDir), []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := open(t.Context(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	req := httptest.NewRequest(http.MethodGet, daemon.DefaultBasePath+"api/session", nil)
	if err := daemon.VouchLocally(dataDir, req.Header); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	app.Handler(nil).ServeHTTP(rec, req)
	var session daemon.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("api/session: HTTP %d %s", rec.Code, rec.Body)
	}
	if session.Version != linkedVersion || session.Release == nil || *session.Release != linkedRelease {
		t.Errorf("api/session: %s", rec.Body)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

const asSidegated = "SIDEGATED_ARGS"

// A command refuses any argument it did not parse, before it reads or writes anything: a stray word
// would otherwise end flag parsing and drop every flag after it, --expect included, and another
// command's flag would be ignored. A command line that parses still reaches its command.
func TestACommandRefusesWhatItDidntParse(t *testing.T) {
	if raw := os.Getenv(asSidegated); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatal(err)
		}
		os.Args = append([]string{"sidegated"}, args...)
		main()
		return
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "sidegate")
	result := filepath.Join(dir, "result")
	run := func(args ...string) (int, string) {
		raw, _ := json.Marshal(args)
		cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
		cmd.Env = append(os.Environ(), asSidegated+"="+string(raw))
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return cmd.ProcessState.ExitCode(), string(out)
	}
	for _, c := range []struct {
		names string
		args  []string
	}{
		{"stray", []string{"upgrade", "--data-dir", dataDir, "--bundle", filepath.Join(dir, "old.tar.gz"), "--yes", "--outcome", result, "stray", "--expect", "previous"}},
		{"stray", []string{"install", "--data-dir", dataDir, "--bundle", filepath.Join(dir, "old.tar.gz"), "stray"}},
		{"stray", []string{"uninstall", "--data-dir", dataDir, "--yes", "stray"}},
		{"stray", []string{"recover", "--data-dir", dataDir, "--yes", "stray"}},
		{"-bundle", []string{"recover", "--data-dir", dataDir, "--bundle", filepath.Join(dir, "old.tar.gz")}},
		{"-expect", []string{"install", "--data-dir", dataDir, "--expect", "previous"}},
		{"-bundle", []string{"uninstall", "--data-dir", dataDir, "--bundle", filepath.Join(dir, "old.tar.gz")}},
		{"-outcome", []string{"install", "--data-dir", dataDir, "--outcome", result}},
	} {
		code, out := run(c.args...)
		if code != 2 || !strings.Contains(out, c.names) {
			t.Errorf("sidegated %s exited %d, and its refusal should name %q:\n%s", strings.Join(c.args, " "), code, c.names, out)
		}
		if _, err := os.Stat(result); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("sidegated %s wrote an outcome: %v", strings.Join(c.args, " "), err)
		}
	}
	code, out := run("upgrade", "--data-dir", dataDir, "--bundle", filepath.Join(dir, "old.tar.gz"), "--expect", "previous")
	if code != 1 || !strings.Contains(out, "not previous") {
		t.Errorf("an upgrade that parses didn't reach --expect: exit %d\n%s", code, out)
	}
	if code, out := run("recover", "--data-dir", dataDir); code != 0 || !strings.Contains(out, "no switch is unfinished") {
		t.Errorf("recover with no journal: exit %d\n%s", code, out)
	}
}
