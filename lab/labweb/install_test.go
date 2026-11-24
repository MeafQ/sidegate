package main

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// These run the release's own scripts, from a copy of the repository's files with a pin for a
// stand-in Xray, and the one command against the stand-in release server. The gateway is played
// by a directory for /data, id and uname on PATH, and a bundle whose sidegated only records
// that install ran.

const (
	repo        = "../.."
	testVersion = "v0.0.1"
	lastLine    = "{ main </dev/null; }"
	fakeXray    = "a stand-in for the pinned xray\n"
)

var releaseFileNames = []string{"sidegate-" + testVersion + "-arm64.tar.gz", "sidegate.json", "SHA256SUMS", "install.sh", "XRAY-LICENSE"}

// scriptRoot copies what release-files.sh reads into a new root, with a pin for fakeXray.
// replace swaps a copied file's contents.
func scriptRoot(t *testing.T, replace map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{"scripts/release-files.sh", "scripts/release-version.sh", "deploy/install.sh", "deploy/XRAY-LICENSE"} {
		body, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := replace[f]; ok {
			body = []byte(r)
		}
		write(t, filepath.Join(root, f), body)
	}
	pin := fmt.Sprintf("XRAY_VERSION=v26.10.31\nXRAY_ZIP_SHA256=unused\nXRAY_SHA256=%x\n", sha256.Sum256([]byte(fakeXray)))
	write(t, filepath.Join(root, "deploy/xray.pin"), []byte(pin))
	return root
}

// fakeBundle writes a bundle whose sidegated is the shared stub daemon.
func fakeBundle(t *testing.T, version, xray string) string {
	t.Helper()
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	tw := tar.NewWriter(z)
	if err := tw.WriteHeader(&tar.Header{Name: "sidegated", Typeflag: tar.TypeSymlink, Linkname: filepath.Join(stubs(t), "sidegated"), Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "xray", Mode: 0o755, Size: int64(len(xray))}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(xray))
	tw.Close()
	z.Close()
	name := filepath.Join(t.TempDir(), "sidegate-"+version+"-arm64.tar.gz")
	write(t, name, buf.Bytes())
	return name
}

var (
	stubOnce sync.Once
	stubDir  string
	stubErr  error
)

// stubs holds the gateway's stand-in commands, written once: macOS takes about half a second
// over the first run of each new executable. The daemon says $SIDEGATE_TEST_SAYS to version,
// and on install records its arguments in $SIDEGATE_TEST_RECORD, prints a log and exits
// $SIDEGATE_TEST_INSTALL_EXIT. bin/ is a gateway's id and uname, and the others replace one.
// ci/ is the release workflow's runner: gh, git and the two build scripts, as workflow_test.go has them.
// Each of these adds its name to $SIDEGATE_TEST_RAN, with "piped" when its stdin is a pipe.
func stubs(t *testing.T) string {
	t.Helper()
	stubOnce.Do(func() {
		stubDir, stubErr = os.MkdirTemp("", "sidegate-stubs")
		if stubErr != nil {
			return
		}
		ran := `[ -z "${SIDEGATE_TEST_RAN:-}" ] || { [ -p /dev/stdin ] && echo "${0##*/} piped" || echo "${0##*/}"; } >>"$SIDEGATE_TEST_RAN"` + "\n"
		for name, body := range map[string]string{
			"sidegated": ran + `case $1 in
version) printf '%s' "$SIDEGATE_TEST_SAYS" ;;
install) echo "$*" >"$SIDEGATE_TEST_RECORD"; echo "the install's first line"; echo "the install's last line"; exit "${SIDEGATE_TEST_INSTALL_EXIT:-0}" ;;
*) exit 2 ;;
esac`,
			"bin/id": ran + "echo 0", "bin/uname": ran + "echo aarch64",
			"notroot/id": ran + "echo 501", "notarm/uname": ran + "echo x86_64",
			"dash/sh": "exec /bin/dash \"$@\"", "sh/sh": "exec /bin/sh \"$@\"",
			"ci/gh": ghStub, "ci/git": "exit 0",
			"ci/build-bundle.sh":  `echo build >>"$SIDEGATE_TEST_CI_LOG"; echo "dist/sidegate-$1-arm64.tar.gz"`,
			"ci/release-files.sh": `echo files >>"$SIDEGATE_TEST_CI_LOG"; mkdir -p "$4/files" && echo notes >"$4/NOTES.md" && echo x >"$4/files/install.sh"`,
		} {
			name = filepath.Join(stubDir, name)
			if stubErr = os.MkdirAll(filepath.Dir(name), 0o755); stubErr != nil {
				return
			}
			if stubErr = os.WriteFile(name, []byte("#!/bin/sh\n"+body+"\n"), 0o755); stubErr != nil {
				return
			}
		}
	})
	if stubErr != nil {
		t.Fatal(stubErr)
	}
	return stubDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if stubDir != "" {
		os.RemoveAll(stubDir)
	}
	os.Exit(code)
}

// releaseFilesRun runs release-files.sh with the bundle's sidegated saying says to version.
func releaseFilesRun(root, version, releases, bundle, out, says string) (string, error) {
	cmd := exec.Command("sh", filepath.Join(root, "scripts/release-files.sh"), version, releases, bundle, out)
	cmd.Env = append(os.Environ(), "SIDEGATE_TEST_SAYS="+says)
	got, err := cmd.CombinedOutput()
	return string(got), err
}

// cutRelease is a release written by release-files.sh, in out.
func cutRelease(t *testing.T, releases string) string {
	t.Helper()
	root := scriptRoot(t, nil)
	out := filepath.Join(t.TempDir(), "release")
	if got, err := releaseFilesRun(root, testVersion, releases, fakeBundle(t, testVersion, fakeXray), out, testVersion+"\n"+releases+"\n"); err != nil {
		t.Fatalf("release-files.sh: %v\n%s", err, got)
	}
	return out
}

func TestReleaseFilesWritesTheFiveFilesAndNotes(t *testing.T) {
	releases := "https://example.test/OWNER/sidegate/releases"
	out := cutRelease(t, releases)
	entries, err := os.ReadDir(filepath.Join(out, "files"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := append([]string(nil), releaseFileNames...)
	slices.Sort(want)
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("files: %v, want %v", names, want)
	}
	bundle := file(t, out, "files", releaseFileNames[0])
	sum := fmt.Sprintf("%x", sha256.Sum256(bundle))
	if got := string(file(t, out, "files", "SHA256SUMS")); got != sum+"  "+releaseFileNames[0]+"\n" {
		t.Errorf("SHA256SUMS: %q", got)
	}
	var manifest struct {
		Version, Bundle, Sha256, Core string
		Size                          int64
	}
	if err := json.Unmarshal(file(t, out, "files", "sidegate.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	core := fmt.Sprintf("%x", sha256.Sum256([]byte(fakeXray)))
	if manifest.Version != testVersion || manifest.Bundle != releaseFileNames[0] || manifest.Sha256 != sum || manifest.Size != int64(len(bundle)) || manifest.Core != core {
		t.Errorf("sidegate.json: %+v", manifest)
	}
	script := string(file(t, out, "files", "install.sh"))
	if !strings.Contains(script, "version="+testVersion+"\n") || !strings.Contains(script, "from="+releases+"/download/$version\n") || strings.Contains(script, "@") {
		t.Errorf("install.sh isn't filled in:\n%s", script)
	}
	if !strings.HasSuffix(script, "\n"+lastLine+"\n") {
		t.Errorf("install.sh doesn't end with %s", lastLine)
	}
	for _, shell := range []string{"sh", "dash", "bash"} {
		if got, err := exec.Command(shell, "-n", filepath.Join(out, "files", "install.sh")).CombinedOutput(); err != nil {
			t.Errorf("%s -n install.sh: %v %s", shell, err, got)
		}
	}
	if license := file(t, repo, "deploy", "XRAY-LICENSE"); !bytes.Equal(file(t, out, "files", "XRAY-LICENSE"), license) || !bytes.HasPrefix(license, []byte("Mozilla Public License Version 2.0")) {
		t.Errorf("XRAY-LICENSE isn't Xray's MPL-2.0 text")
	}
	notes := string(file(t, out, "", "NOTES.md"))
	if !strings.Contains(notes, "https://github.com/XTLS/Xray-core/tree/v26.10.31") {
		t.Errorf("NOTES.md:\n%s", notes)
	}
	if got, want := oneCommand(t, out), "curl --proto '=https' --tlsv1.2 -fsSL "+releases+"/latest/download/install.sh | sh"; got != want {
		t.Errorf("the one command is\n%s\nwant\n%s", got, want)
	}
}

func TestReleaseFilesRefusesABadInput(t *testing.T) {
	releases := "https://example.test/OWNER/sidegate/releases"
	for _, c := range []struct {
		name, want                    string
		version, releases, says, xray string
		bundleName                    string
		replace                       map[string]string
		outExists                     bool
	}{
		{name: "an empty install.sh", want: "install.sh is empty", replace: map[string]string{"deploy/install.sh": ""}},
		{name: "an install.sh that doesn't parse", want: "install.sh doesn't parse", replace: map[string]string{"deploy/install.sh": "main() { if then; }\n" + lastLine + "\n"}},
		{name: "an install.sh that calls main bare", want: "doesn't end with " + lastLine, replace: map[string]string{"deploy/install.sh": "main() { echo hi; }\nmain </dev/null\n"}},
		{name: "an install.sh with a line after the call", want: "doesn't end with " + lastLine, replace: map[string]string{"deploy/install.sh": "main() { echo hi; }\n" + lastLine + "\n# the end\n"}},
		{name: "a blank left in install.sh", want: "a blank left: @OWNER@", replace: map[string]string{"deploy/install.sh": "main() { echo @OWNER@; }\n" + lastLine + "\n"}},
		{name: "an empty licence", want: "XRAY-LICENSE is empty", replace: map[string]string{"deploy/XRAY-LICENSE": ""}},
		{name: "a daemon that names another address", want: "the bundle's sidegated says", says: testVersion + "\nhttps://elsewhere.test/OWNER/sidegate/releases\n"},
		{name: "a daemon with no address", want: "the bundle's sidegated says", says: testVersion + "\n"},
		{name: "a daemon that names another version", want: "the bundle's sidegated says", version: "v0.0.2", says: testVersion + "\n" + releases + "\n"},
		{name: "another xray", want: "isn't Xray v26.10.31", xray: "another core\n"},
		{name: "a misnamed bundle", want: "is named sidegate-v0.0.1-arm64.tar.gz", bundleName: "bundle.tar.gz"},
		{name: "a version that isn't a release", want: "isn't a release version", version: "dev-1234"},
		{name: "an http address", want: "isn't an https releases address", releases: "http://example.test/OWNER/sidegate/releases"},
		{name: "an out that exists", want: "already exists", outExists: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			version, rel, xray := cmp.Or(c.version, testVersion), cmp.Or(c.releases, releases), cmp.Or(c.xray, fakeXray)
			says := cmp.Or(c.says, version+"\n"+rel+"\n")
			bundle := fakeBundle(t, version, xray)
			if c.bundleName != "" {
				renamed := filepath.Join(filepath.Dir(bundle), c.bundleName)
				os.Rename(bundle, renamed)
				bundle = renamed
			}
			out := filepath.Join(t.TempDir(), "release")
			if c.outExists {
				os.Mkdir(out, 0o755)
			}
			got, err := releaseFilesRun(scriptRoot(t, c.replace), version, rel, bundle, out, says)
			if err == nil || !strings.Contains(got, c.want) {
				t.Fatalf("got %v %q, want a refusal saying %q", err, got, c.want)
			}
		})
	}
}

// oneCommand is the line NOTES.md gives, as the README is to give it.
func oneCommand(t *testing.T, out string) string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(string(file(t, out, "", "NOTES.md")), "\n") {
		if strings.Contains(l, "curl ") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("NOTES.md has %d command lines", len(lines))
	}
	return lines[0]
}

type gateway struct {
	data, record, ran, home, ca, path string
}

// newGateway plays root on an arm64 UniFi OS console with /data at dir/data. shell is what its
// /bin/sh is, dash or the Mac's sh. Each of instead, a directory of stubs, replaces a command.
func newGateway(t *testing.T, s *httptest.Server, dir, shell string, instead ...string) gateway {
	t.Helper()
	g := gateway{data: filepath.Join(dir, "data"), record: filepath.Join(dir, "record"), ran: filepath.Join(dir, "ran"), home: filepath.Join(dir, "home"), ca: filepath.Join(dir, "ca.pem")}
	for _, d := range []string{filepath.Join(g.data, "unifi-core"), g.home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var path []string
	for _, d := range append(instead, shell, "bin") {
		path = append(path, filepath.Join(stubs(t), d))
	}
	g.path = strings.Join(append(path, os.Getenv("PATH")), ":")
	write(t, g.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	return g
}

type ran struct {
	code           int
	stdout, stderr string
}

// run runs the one command in a login shell.
func (g gateway) run(t *testing.T, login, command string, env ...string) ran {
	t.Helper()
	cmd := exec.Command(login, "-c", command)
	cmd.Env = append([]string{
		"PATH=" + g.path, "HOME=" + g.home, "CURL_HOME=" + g.home,
		"CURL_CA_BUNDLE=" + g.ca, "SIDEGATE_TEST_RECORD=" + g.record, "SIDEGATE_TEST_RAN=" + g.ran,
	}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return ran{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

type left struct {
	staging   []string // what is in /data beside unifi-core
	installed string   // the installer's arguments, when it ran
	commands  []string // the gateway's commands the script ran
}

func (l left) nothing() bool { return len(l.staging) == 0 && l.installed == "" && len(l.commands) == 0 }

func (l left) String() string {
	return fmt.Sprintf("in /data %v, installer ran %q, commands %v", l.staging, l.installed, l.commands)
}

func (g gateway) left(t *testing.T) left {
	t.Helper()
	entries, err := os.ReadDir(g.data)
	if err != nil {
		t.Fatal(err)
	}
	var l left
	for _, e := range entries {
		if e.Name() != "unifi-core" {
			l.staging = append(l.staging, filepath.Join(g.data, e.Name()))
		}
	}
	b, _ := os.ReadFile(g.record)
	l.installed = string(b)
	b, _ = os.ReadFile(g.ran)
	l.commands = strings.Fields(strings.ReplaceAll(string(b), " piped", "+piped"))
	return l
}

// forget clears what the last run recorded.
func (g gateway) forget(t *testing.T) {
	t.Helper()
	for _, f := range []string{g.record, g.ran} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// publish puts a cut in the stand-in, with install.sh's /data where g has it.
func publish(t *testing.T, dir, out string, g gateway) {
	t.Helper()
	for _, f := range releaseFileNames {
		body := file(t, out, "files", f)
		if f == "install.sh" {
			body = bytes.ReplaceAll(body, []byte("/data/"), []byte(g.data+"/"))
		}
		write(t, filepath.Join(dir, testVersion, f), body)
	}
}

// From a bash login to a dash /bin/sh, as on the gateway, and from dash to the Mac's sh, which
// is bash and reads a piped script a byte at a time where dash reads it in blocks.
var shells = [][2]string{{"bash", "dash"}, {"dash", "sh"}}

const installed = "install --bundle sidegate-" + testVersion + "-arm64.tar.gz --yes\n"

// eachGateway runs f over HTTP/1.1 and HTTP/2 and for each pair of shells.
func eachGateway(t *testing.T, f func(t *testing.T, dir, command string, newG func(t *testing.T, instead ...string) gateway, login string)) {
	dir := t.TempDir()
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		out := cutRelease(t, s.URL+"/OWNER/sidegate/releases")
		command := oneCommand(t, out)
		for _, sh := range shells {
			t.Run(sh[0]+"+"+sh[1], func(t *testing.T) {
				newG := func(t *testing.T, instead ...string) gateway {
					os.RemoveAll(filepath.Join(dir, testVersion))
					mustMode(t, dir)
					g := newGateway(t, s, t.TempDir(), sh[1], instead...)
					publish(t, dir, out, g)
					return g
				}
				f(t, dir, command, newG, sh[0])
			})
		}
	})
}

func TestTheOneCommandInstallsTheBundleItChecked(t *testing.T) {
	eachGateway(t, func(t *testing.T, dir, command string, newG func(t *testing.T, instead ...string) gateway, login string) {
		g := newG(t)
		r := g.run(t, login, command)
		l := g.left(t)
		if r.code != 0 || !strings.Contains(r.stdout, "Installed. Open https://<this gateway>/sidegate/") {
			t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		if l.installed != installed || len(l.staging) != 0 {
			t.Errorf("left %s", l)
		}
		if got := strings.Join(l.commands, " "); got != "id uname sidegated" {
			t.Errorf("the script ran %s, want id uname sidegated, none with the script's pipe as stdin", got)
		}

		g.forget(t)
		var wg sync.WaitGroup
		results := make([]ran, 2)
		for i := range results {
			wg.Add(1)
			go func() { defer wg.Done(); results[i] = g.run(t, login, command) }()
		}
		wg.Wait()
		for i, r := range results {
			if r.code != 0 || !strings.Contains(r.stdout, "Installed.") {
				t.Errorf("run %d of two at once: exit %d\n%s%s", i, r.code, r.stdout, r.stderr)
			}
		}
		if l := g.left(t); l.installed != installed || len(l.staging) != 0 || len(l.commands) != 6 {
			t.Errorf("two at once left %s", l)
		}
	})
}

// cutLines are the offsets a -short run cuts install.sh at: each line's first two bytes, its middle
// and its newline, and every byte of the last two lines.
func cutLines(script []byte) []int {
	var at []int
	start := 0
	for i, b := range script {
		if b != '\n' {
			continue
		}
		at = append(at, start, min(start+1, i), (start+i)/2, i)
		start = i + 1
	}
	tail := bytes.LastIndexByte(script[:bytes.LastIndexByte(script[:len(script)-1], '\n')], '\n') + 1
	for i := tail; i < len(script); i++ {
		at = append(at, i)
	}
	slices.Sort(at)
	return slices.Compact(at)
}

// The pipe gives sh every first n bytes of install.sh, close-delimited and with the full length
// declared, for each n short of the whole file. sh finds the function unfinished or never called,
// so nothing runs and nothing is left behind, whatever the status. Only the cut that drops the
// last newline alone leaves the script whole, and that one installs.
func TestTheScriptCutAnywhereInstallsNothing(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, sh := range shells {
			t.Run(fmt.Sprintf("h2=%v/%s+%s", h2, sh[0], sh[1]), func(t *testing.T) {
				t.Parallel()
				cutEverywhere(t, h2, sh[0], sh[1])
			})
		}
	}
}

// ownWords is how the script's own messages start.
var ownWords = regexp.MustCompile(`(?m)^sidegate: `)

func cutEverywhere(t *testing.T, h2 bool, login, shell string) {
	base := t.TempDir()
	type worker struct {
		dir     string
		g       gateway
		command string
	}
	workers := make([]worker, max(2, runtime.NumCPU()/2))
	var out, releases string
	for i := range workers {
		w := &workers[i]
		w.dir = filepath.Join(base, fmt.Sprintf("r%03d", i))
		s := standIn(t, w.dir, h2)
		if i == 0 {
			releases = s.URL + "/OWNER/sidegate/releases"
			out = cutRelease(t, releases)
		}
		w.g = newGateway(t, s, filepath.Join(base, fmt.Sprintf("g%03d", i)), shell)
		publish(t, w.dir, out, w.g)
		w.command = strings.Replace(oneCommand(t, out), releases, s.URL+"/OWNER/sidegate/releases", 1)
	}
	script := file(t, workers[0].dir, testVersion, "install.sh")
	if script[len(script)-1] != '\n' {
		t.Fatal("install.sh doesn't end with a newline")
	}
	offsets := make([]int, len(script))
	for i := range offsets {
		offsets[i] = i
	}
	if testing.Short() {
		offsets = cutLines(script)
	}

	type job struct {
		at   int
		kind string
	}
	jobs := make(chan job)
	var mu sync.Mutex
	tally := map[string]int{}
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				w.g.forget(t)
				mustMode(t, w.dir, "install.sh", j.kind, strconv.Itoa(j.at))
				r := w.g.run(t, login, w.command)
				l := w.g.left(t)
				line := bytes.Count(script[:j.at], []byte("\n")) + 1
				if j.at == len(script)-1 {
					if l.installed != installed || len(l.staging) != 0 {
						t.Errorf("%s at %d, only the last newline gone: exit %d, left %s\n%s", j.kind, j.at, r.code, l, r.stderr)
					}
					continue
				}
				if !l.nothing() || r.stdout != "" || ownWords.MatchString(r.stderr) || strings.Contains(r.stderr, "not found") {
					t.Errorf("%s at byte %d, line %d, after %q: exit %d, left %s\n%s%s", j.kind, j.at, line, script[max(0, j.at-20):j.at], r.code, l, r.stdout, r.stderr)
				}
				class := "ran nothing"
				if strings.Contains(strings.ToLower(r.stderr), "syntax error") {
					class = "parse error"
				}
				mu.Lock()
				tally[fmt.Sprintf("%s, %s, exit %d", j.kind, class, r.code)]++
				mu.Unlock()
			}
		}()
	}
	for _, at := range offsets {
		for _, kind := range []string{"short", "cut"} {
			jobs <- job{at, kind}
		}
	}
	close(jobs)
	wg.Wait()
	keys := slices.Sorted(maps.Keys(tally))
	for _, k := range keys {
		t.Logf("%s: %d", k, tally[k])
	}
	t.Logf("%d of %d bytes cut at, each close-delimited and with its length declared", len(offsets), len(script))
}

// Each fails before the installer runs, and leaves nothing in /data. A script that never arrived
// runs nothing, and only curl says why: sh read nothing and the pipe's status is sh's, 0. The
// script's own failures say why and exit 1.
func TestTheOneCommandRunsNothingItCouldNotCheck(t *testing.T) {
	for _, c := range []struct {
		name string
		mode []string
		says string                   // the script's own message, when it ran
		curl string                   // curl's, when install.sh never arrived
		sums func(line string) string // SHA256SUMS made from the bundle's good line
	}{
		{name: "install.sh empty", mode: []string{"install.sh", "empty"}},
		{name: "install.sh missing", mode: []string{"install.sh", "404"}, curl: "returned error: 404"},
		{name: "install.sh redirected to http", mode: []string{"install.sh", "http"}, curl: `Protocol "http" disabled`},
		{name: "a changed bundle", mode: []string{"*.tar.gz", "changed"}, says: "the bundle doesn't match its checksum"},
		{name: "a bundle cut with its length declared", mode: []string{"*.tar.gz", "cut"}, says: "the download of sidegate-v0.0.1-arm64.tar.gz failed"},
		{name: "a bundle cut close-delimited", mode: []string{"*.tar.gz", "short"}, says: "the bundle doesn't match its checksum"},
		{name: "a bundle missing", mode: []string{"*.tar.gz", "404"}, says: "the download of sidegate-v0.0.1-arm64.tar.gz failed"},
		{name: "a bundle redirected to http", mode: []string{"*.tar.gz", "http"}, says: "the download of sidegate-v0.0.1-arm64.tar.gz failed"},
		{name: "SHA256SUMS empty", mode: []string{"SHA256SUMS", "empty"}, says: "SHA256SUMS isn't the bundle's one line"},
		{name: "SHA256SUMS with a junk line", sums: func(l string) string { return l + "junk\n" }, says: "SHA256SUMS isn't the bundle's one line"},
		{name: "SHA256SUMS with the line twice", sums: func(l string) string { return l + l }, says: "SHA256SUMS isn't the bundle's one line"},
		{name: "SHA256SUMS with another file's line first", sums: func(l string) string { return strings.Repeat("0", 64) + "  install.sh\n" + l }, says: "SHA256SUMS isn't the bundle's one line"},
		{name: "SHA256SUMS missing", mode: []string{"SHA256SUMS", "404"}, says: "the download of SHA256SUMS failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eachGateway(t, func(t *testing.T, dir, command string, newG func(t *testing.T, instead ...string) gateway, login string) {
				g := newG(t)
				mustMode(t, dir, c.mode...)
				if c.sums != nil {
					sums := filepath.Join(dir, testVersion, "SHA256SUMS")
					write(t, sums, []byte(c.sums(string(file(t, sums, "", "")))))
				}
				r := g.run(t, login, command)
				l := g.left(t)
				if l.installed != "" || len(l.staging) != 0 {
					t.Fatalf("exit %d, left %s\n%s%s", r.code, l, r.stdout, r.stderr)
				}
				if c.says == "" {
					if len(l.commands) != 0 || r.stdout != "" || ownWords.MatchString(r.stderr) || !strings.Contains(r.stderr, c.curl) {
						t.Errorf("exit %d, ran %v\n%s%s", r.code, l.commands, r.stdout, r.stderr)
					}
					return
				}
				if r.code != 1 || !strings.Contains(r.stderr, "sidegate: "+c.says) {
					t.Errorf("exit %d, stderr %q, want 1 and %q", r.code, r.stderr, c.says)
				}
			})
		})
	}
}

func TestAFailedInstallKeepsOnlyItsLog(t *testing.T) {
	eachGateway(t, func(t *testing.T, dir, command string, newG func(t *testing.T, instead ...string) gateway, login string) {
		g := newG(t)
		r := g.run(t, login, command, "SIDEGATE_TEST_INSTALL_EXIT=1")
		l := g.left(t)
		if r.code == 0 || l.installed == "" || len(l.staging) != 1 {
			t.Fatalf("exit %d, left %s", r.code, l)
		}
		if !strings.Contains(r.stderr, "the install's last line\n") || !strings.Contains(r.stderr, "The whole log: "+l.staging[0]+"/install.log") {
			t.Errorf("stderr: %s", r.stderr)
		}
		if log := file(t, l.staging[0], "", "install.log"); !bytes.Contains(log, []byte("the install's first line")) {
			t.Errorf("install.log: %q", log)
		}
		if kept, err := os.ReadDir(l.staging[0]); err != nil || len(kept) != 1 {
			t.Errorf("it keeps %v %v", kept, err)
		}
	})
}

func TestTheInstallerRefusesAMachineThatIsNotAGateway(t *testing.T) {
	eachGateway(t, func(t *testing.T, dir, command string, newG func(t *testing.T, instead ...string) gateway, login string) {
		for _, c := range []struct{ name, instead, says string }{
			{"not root", "notroot", "run it as root on the gateway"},
			{"not arm64", "notarm", "this isn't an arm64 gateway"},
			{"not UniFi OS", "", "this isn't a UniFi OS console"},
		} {
			var g gateway
			if c.instead != "" {
				g = newG(t, c.instead)
			} else {
				g = newG(t)
				os.Remove(filepath.Join(g.data, "unifi-core"))
			}
			r := g.run(t, login, command)
			if l := g.left(t); r.code != 1 || l.installed != "" || len(l.staging) != 0 || !strings.Contains(r.stderr, "sidegate: "+c.says) {
				t.Errorf("%s: exit %d, left %s\n%s", c.name, r.code, l, r.stderr)
			}
		}
	})
}
