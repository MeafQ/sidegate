package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These run the release workflow's own run: blocks in order, as GitHub runs them, with bash -e.
// gh is a stub over a list of releases, newest first, each "TAG DRAFT PRERELEASE". Like gh it
// filters before it counts to --limit, and it answers -q with jq. The build scripts only log.

const (
	tagStep     = "The tag is a new release version"
	publishStep = "Publish"
)

// ghStub logs to $SIDEGATE_TEST_CI_LOG what a run asked GitHub, and keeps the releases in
// $SIDEGATE_TEST_RELEASES. $SIDEGATE_TEST_LIST is "fails", "fails-after-create" or empty.
const ghStub = `log=$SIDEGATE_TEST_CI_LOG state=$SIDEGATE_TEST_RELEASES
[ "$1" = release ] || exit 2
cmd=$2 tag=$3
has() { [ -n "$(awk -v t="$tag" '$1 == t' "$state")" ]; }
case $cmd in
list)
	limit=30 q=. drafts=0 pre=0
	while [ $# -gt 0 ]; do
		case $1 in --limit) limit=$2; shift ;; -q) q=$2; shift ;; --exclude-drafts) drafts=1 ;; --exclude-pre-releases) pre=1 ;; esac
		shift
	done
	echo list >>"$log"
	case ${SIDEGATE_TEST_LIST:-} in
	fails) ;;
	fails-after-create) grep -q '^create ' "$log" || ok=1 ;;
	*) ok=1 ;;
	esac
	[ -n "${ok:-}" ] || { echo "HTTP 502: Bad Gateway (https://api.github.com/graphql)" >&2; exit 1; }
	awk -v d=$drafts -v p=$pre '!(d && $2 == "true") && !(p && $3 == "true")' "$state" | head -n "$limit" |
		jq -R -s -r 'split("\n") | map(select(length > 0) | split(" ") | {tagName: .[0], isDraft: (.[1] == "true"), isPrerelease: (.[2] == "true")}) | '"$q" ;;
view)
	has || { echo "release not found" >&2; exit 1; }
	awk -v t="$tag" '$1 == t { print $2 }' "$state" ;;
delete)
	echo "delete $tag" >>"$log"
	awk -v t="$tag" '$1 != t' "$state" >"$state.new" && mv "$state.new" "$state" ;;
create)
	echo "create $tag" >>"$log"
	! has || { echo "a release with the same tag name already exists: $tag" >&2; exit 1; }
	{ echo "$tag true false"; cat "$state"; } >"$state.new" && mv "$state.new" "$state" ;;
edit)
	latest=
	for a; do case $a in --latest=*) latest=${a#--latest=} ;; esac; done
	echo "edit $tag latest=$latest" >>"$log"
	awk -v t="$tag" '$1 == t { $2 = "false" } { print }' "$state" >"$state.new" && mv "$state.new" "$state" ;;
*) exit 2 ;;
esac`

type step struct {
	Name, Uses, Run string
	Env             map[string]string
}

func workflowSteps(t *testing.T) []step {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Jobs map[string]struct{ Steps []step }
	}
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	steps := w.Jobs["release"].Steps
	if len(steps) == 0 {
		t.Fatal("release.yml has no release job steps")
	}
	return steps
}

type workflowRun struct {
	stopped, stderr string // the step that failed and what it said, or "" when every step passed
	log             []string
}

var expression = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)

// runWorkflow runs the workflow for tag with releases already on GitHub, as a run pushed now would.
func runWorkflow(t *testing.T, tag, list string, releases []string) workflowRun {
	t.Helper()
	abs, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ci := filepath.Join(stubs(t), "ci")
	os.MkdirAll(filepath.Join(dir, "work/scripts"), 0o755)
	for f, from := range map[string]string{
		"release-latest.sh":  filepath.Join(abs, "scripts/release-latest.sh"),
		"release-version.sh": filepath.Join(abs, "scripts/release-version.sh"),
		"build-bundle.sh":    filepath.Join(ci, "build-bundle.sh"),
		"release-files.sh":   filepath.Join(ci, "release-files.sh"),
	} {
		if err := os.Symlink(from, filepath.Join(dir, "work/scripts", f)); err != nil {
			t.Fatal(err)
		}
	}
	var state strings.Builder
	for _, r := range releases {
		f := strings.Fields(r)
		fmt.Fprintf(&state, "%s %v %v\n", f[0], slices.Contains(f, "draft"), slices.Contains(f, "pre"))
	}
	logFile, stateFile := filepath.Join(dir, "log"), filepath.Join(dir, "releases")
	write(t, stateFile, []byte(state.String()))
	write(t, logFile, nil)
	github := map[string]string{"github.token": "a-token", "github.repository": "OWNER/sidegate", "github.ref_name": tag, "github.server_url": "https://github.test"}
	env := []string{
		"PATH=" + ci + ":" + os.Getenv("PATH"), "HOME=" + dir,
		"GITHUB_REF_NAME=" + tag, "GITHUB_SHA=0123abcd", "GITHUB_SERVER_URL=https://github.test", "GITHUB_REPOSITORY=OWNER/sidegate",
		"SIDEGATE_TEST_CI_LOG=" + logFile, "SIDEGATE_TEST_RELEASES=" + stateFile, "SIDEGATE_TEST_LIST=" + list,
	}
	var run workflowRun
	for i, s := range workflowSteps(t) {
		if s.Run == "" {
			continue
		}
		script := filepath.Join(dir, fmt.Sprintf("step%d.sh", i))
		write(t, script, []byte(s.Run))
		cmd := exec.Command("bash", "-e", script)
		cmd.Dir = filepath.Join(dir, "work")
		cmd.Env = slices.Clone(env)
		for k, v := range s.Env {
			v = expression.ReplaceAllStringFunc(v, func(e string) string {
				name := expression.FindStringSubmatch(e)[1]
				if _, ok := github[name]; !ok {
					t.Fatalf("step %q: no value for %s", s.Name, e)
				}
				return github[name]
			})
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			run.stopped, run.stderr = s.Name, stderr.String()
			break
		}
	}
	b, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	run.log = strings.Fields(strings.ReplaceAll(string(b), " ", "_"))
	return run
}

// A release is published once, and Latest is decided against every release published at that
// moment. A tag that can't be released stops before anything is built.
func TestTheWorkflowPublishesATagOnceAndMarksLatestOnlyAboveEveryPublishedOne(t *testing.T) {
	var many []string
	for i := 999; i >= 0; i-- {
		many = append(many, fmt.Sprintf("v1.0.%d", i))
	}
	var limit []string
	for i := range 10000 {
		limit = append(limit, fmt.Sprintf("v0.0.%d", i))
	}
	for _, c := range []struct {
		name, tag  string
		published  []string
		list       string
		latest     string // when it publishes
		stop, says string // when it stops
	}{
		{name: "the first release", tag: "v0.1.0", latest: "true"},
		{name: "a newer release", tag: "v0.2.0", published: []string{"v0.1.0"}, latest: "true"},
		{name: "an older tag that finishes after a newer one", tag: "v0.1.1", published: []string{"v0.2.0", "v0.1.0"}, latest: "false"},
		{name: "a higher tag whose run failed left its draft", tag: "v0.2.1", published: []string{"v0.3.0 draft", "v0.2.0"}, latest: "true"},
		{name: "its own draft from a failed run", tag: "v0.2.0", published: []string{"v0.2.0 draft", "v0.1.0"}, latest: "true"},
		{name: "ordered by number, not text", tag: "v0.10.0", published: []string{"v0.9.9", "v0.2.0"}, latest: "true"},
		{name: "below by number, above by text", tag: "v0.9.0", published: []string{"v0.10.0"}, latest: "false"},
		{name: "a published tag's leading zeros", tag: "v0.9.0", published: []string{"v0.010.0"}, latest: "false"},
		{name: "a major above minors", tag: "v1.0.0", published: []string{"v0.99.99"}, latest: "true"},
		{name: "prereleases and other tags don't count", tag: "v0.3.0", published: []string{"v9.0.0 pre", "nightly", "v0.2.0"}, latest: "true"},
		{name: "v2.0.0 published before a thousand others", tag: "v1.1.0", published: append(slices.Clone(many), "v2.0.0"), latest: "false"},
		{name: "the list fails at publish", tag: "v0.2.0", published: []string{"v0.1.0"}, list: "fails-after-create", stop: publishStep, says: "can't read the published releases"},
		{name: "the list fails at the start", tag: "v0.2.0", published: []string{"v0.1.0"}, list: "fails", stop: tagStep, says: "can't read the published releases"},
		{name: "as many releases as the list reads", tag: "v1.0.0", published: limit, stop: tagStep, says: "10000 releases or more"},
		{name: "a rerun of a published tag", tag: "v0.1.0", published: []string{"v0.1.0"}, stop: tagStep, says: "v0.1.0 is already published"},
		{name: "a tag with a published one's numbers", tag: "v0.1.0", published: []string{"v0.01.0"}, stop: tagStep, says: "has the numbers of the published v0.01.0"},
		{name: "a leading zero", tag: "v01.0.0", stop: tagStep, says: "isn't a release version"},
		{name: "a leading zero in the last number", tag: "v1.0.00", stop: tagStep, says: "isn't a release version"},
		{name: "two numbers", tag: "v0.1", stop: tagStep, says: "isn't a release version"},
		{name: "four numbers", tag: "v0.1.0.1", stop: tagStep, says: "isn't a release version"},
		{name: "no v", tag: "0.1.0", stop: tagStep, says: "isn't a release version"},
		{name: "a prerelease", tag: "v0.1.0-rc1", stop: tagStep, says: "isn't a release version"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := runWorkflow(t, c.tag, c.list, c.published)
			wrote := slices.ContainsFunc(r.log, func(l string) bool { return !strings.HasPrefix(l, "list") && l != "build" && l != "files" })
			if c.stop == "" {
				if want := "edit_" + c.tag + "_latest=" + c.latest; r.stopped != "" || r.log[len(r.log)-1] != want || !slices.Contains(r.log, "create_"+c.tag) {
					t.Fatalf("stopped at %q: %s\nlog %v, want it to end with %s", r.stopped, r.stderr, r.log, want)
				}
				return
			}
			if r.stopped != c.stop || !strings.Contains(r.stderr, c.says) {
				t.Fatalf("stopped at %q saying %q, want %q saying %q; log %v", r.stopped, r.stderr, c.stop, c.says, r.log)
			}
			if c.stop == tagStep && (wrote || slices.Contains(r.log, "build")) {
				t.Errorf("a tag stopped at the start built or wrote: %v", r.log)
			}
			if slices.ContainsFunc(r.log, func(l string) bool { return strings.HasPrefix(l, "edit") }) {
				t.Errorf("a stopped run published: %v", r.log)
			}
		})
	}
}

// updaterTags is the updater's own table of tags, and whether each is a release's.
func updaterTags(t *testing.T) (tags []struct {
	Tag     string
	Release bool
}) {
	t.Helper()
	if err := json.Unmarshal(file(t, repo, "internal/daemon/testdata", "release-tags.json"), &tags); err != nil || len(tags) == 0 {
		t.Fatalf("release-tags.json: %v, %d tags", err, len(tags))
	}
	return tags
}

// The workflow's first step, release-latest.sh itself and release-files.sh take a tag exactly when the updater
// does, as internal/daemon's TestAReleasesVersionIsTheTables holds it to the same table. So nothing is published
// that can't be installed.
func TestTheScriptsTakeTheTagsTheUpdaterDoes(t *testing.T) {
	releases := "https://example.test/OWNER/sidegate/releases"
	root := scriptRoot(t, nil)
	for _, c := range updaterTags(t) {
		t.Run(fmt.Sprintf("%q", c.Tag), func(t *testing.T) {
			t.Parallel()
			said := func(by string, took bool, says string) {
				t.Helper()
				if took != c.Release || !took && !strings.Contains(says, "isn't a release version") {
					t.Errorf("%s takes it: %t, want %t: %s", by, took, c.Release, says)
				}
			}
			r := runWorkflow(t, c.Tag, "", nil)
			said("the workflow", r.stopped == "", r.stderr)
			if !c.Release && r.stopped != tagStep {
				t.Errorf("the workflow stopped at %q", r.stopped)
			}
			var bundle string
			if c.Release {
				bundle = fakeBundle(t, c.Tag, fakeXray)
			}
			got, err := releaseFilesRun(root, c.Tag, releases, bundle, filepath.Join(t.TempDir(), "release"), c.Tag+"\n"+releases+"\n")
			said("release-files.sh", err == nil, got)
		})
	}
}

// GitHub runs each block with bash, and each keeps to what dash takes too.
func TestEveryWorkflowStepParses(t *testing.T) {
	for _, s := range workflowSteps(t) {
		if s.Run == "" {
			continue
		}
		for _, shell := range []string{"bash", "sh", "dash"} {
			cmd := exec.Command(shell, "-n")
			cmd.Stdin = strings.NewReader(s.Run)
			if got, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s -n on %q: %v %s", shell, s.Name, err, got)
			}
		}
	}
}
