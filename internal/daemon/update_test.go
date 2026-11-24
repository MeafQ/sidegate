package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// releaseServer stands in for the releases address. files holds what each path under it answers, after a
// redirect to an asset path as GitHub's, and seen every request.
type releaseServer struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]http.HandlerFunc
	seen  []*http.Request
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	s := &releaseServer{files: map[string]http.HandlerFunc{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r)
		answer, ok := s.files[strings.TrimPrefix(r.URL.Path, "/assets")]
		s.mu.Unlock()
		switch {
		case !ok:
			http.NotFound(w, r)
		case !strings.HasPrefix(r.URL.Path, "/assets"):
			http.Redirect(w, r, "/assets"+r.URL.Path, http.StatusFound)
		default:
			answer(w, r)
		}
	}))
	s.Config.ErrorLog = nil
	t.Cleanup(s.Close)
	return s
}

func (s *releaseServer) address() string { return s.URL + "/OWNER/sidegate/releases" }

func (s *releaseServer) source() releaseSource {
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	return newReleaseSource(s.address(), roots)
}

func (s *releaseServer) answer(path string, with http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files["/OWNER/sidegate/releases"+path] = with
}

func (s *releaseServer) serve(path string, body []byte) {
	s.answer(path, func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
}

func (s *releaseServer) redirect(path, to string) {
	s.answer(path, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) })
}

// plainly serves body over http, and is its address.
func plainly(t *testing.T, body []byte) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	t.Cleanup(s.Close)
	return s.URL
}

// asked counts the requests for a file.
func (s *releaseServer) asked(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.seen {
		if filepath.Base(r.URL.Path) == name {
			n++
		}
	}
	return n
}

// manifests counts the fetches of the manifest.
func (s *releaseServer) manifests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.seen {
		if strings.HasPrefix(r.URL.Path, "/assets") && strings.HasSuffix(r.URL.Path, manifestName) {
			n++
		}
	}
	return n
}

func (s *releaseServer) publish(t *testing.T, version, bundlePath, core string) manifest {
	t.Helper()
	body, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	m := manifest{Version: version, Bundle: bundleName(version), Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), Core: core}
	s.serve("/download/"+version+"/"+m.Bundle, body)
	s.latest(m)
	return m
}

func (s *releaseServer) latest(m manifest) {
	raw, _ := json.Marshal(m)
	s.serve("/latest/download/"+manifestName, raw)
}

// fakeSystemd plays the update job's unit: what systemctl show says of it, and each systemd-run.
type fakeSystemd struct {
	mu      sync.Mutex
	show    string
	showErr error
	runs    [][]string
	start   func(args []string) error
}

const (
	unitGone    = "LoadState=not-found\nActiveState=inactive\nJob=\n"
	unitQueued  = "LoadState=loaded\nActiveState=inactive\nJob=812\n"
	unitActive  = "LoadState=loaded\nActiveState=active\nJob=\n"
	unitStops   = "LoadState=loaded\nActiveState=deactivating\nJob=\n"
	unitEnded   = "LoadState=loaded\nActiveState=inactive\nJob=\n"
	unitFailed  = "LoadState=loaded\nActiveState=failed\nJob=\n"
	unitStrange = "LoadState=loaded\nActiveState=\nJob=\n"
)

func (f *fakeSystemd) says(show string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.show = show
}

func (f *fakeSystemd) launches() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.runs)
}

func (f *fakeSystemd) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "systemd-run" {
		return f.show, f.showErr
	}
	if f.show != unitGone {
		return "", errors.New("Unit sidegate-update.service already exists.")
	}
	f.runs = append(f.runs, args)
	if f.start != nil {
		if err := f.start(args); err != nil {
			return "", err
		}
	}
	f.show = unitActive
	return "", nil
}

// updateRig is a gateway with release one installed as v0.0.1, and v0.0.2 the latest of its releases.
type updateRig struct {
	*installRig
	u       *updater
	server  *releaseServer
	systemd *fakeSystemd
	clock   time.Time
	m       manifest
}

// candidate is the bundle of a release whose sidegated says version, and whose upgrade is the real one,
// run in a process of its own by TestCandidateUpgrade. then is what its upgrade does first.
func (r *installRig) candidate(version, releases, then string) string {
	self, err := os.Executable()
	if err != nil {
		r.t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
version) printf '%s\n%s\n';;
upgrade) %s
    CANDIDATE_ROOT=%q CANDIDATE_SOCKET=%q CANDIDATE_ARGS="$*" exec %q -test.run='^TestCandidateUpgrade$';;
esac
`, version, releases, then, r.root, r.socket, self)
	sum := sha256.Sum256([]byte(script + string(r.coreBytes)))
	return r.bundle(version+"-"+hex.EncodeToString(sum[:4]), map[string][]byte{bundleDaemon: []byte(script), bundleCore: r.coreBytes})
}

// TestCandidateUpgrade is the candidate's sidegated upgrade, as the job runs it.
func TestCandidateUpgrade(t *testing.T) {
	root := os.Getenv("CANDIDATE_ROOT")
	if root == "" {
		t.Skip("the update job's tests run this in a process of its own")
	}
	r := rigAt(t, root)
	r.pinCore()
	if raw, err := os.ReadFile(filepath.Join(root, "core-bytes")); err == nil {
		r.coreBytes = raw
		r.pinCore()
	}
	request := r.request(Upgrade, "")
	request.answering = 5 * time.Second
	fields := strings.Fields(os.Getenv("CANDIDATE_ARGS"))[1:]
	for i := 0; i < len(fields); i++ {
		name, value, joined := strings.Cut(fields[i], "=")
		if !joined && name != "--yes" {
			i++
			value = fields[i]
		}
		switch name {
		case "--data-dir", "--yes":
		case "--bundle":
			request.BundlePath = value
		case "--expect":
			request.Expect = value
		case "--outcome":
			request.Outcome = value
		case "--restarts-core":
			yes := value == "yes"
			request.RestartsCore = &yes
		case "--lock-fd":
			fmt.Sscan(value, &request.LockFD)
		default:
			t.Fatalf("the job gave its upgrade %s", name)
		}
	}
	request.Out = os.Stdout
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	if err := RunLifecycle(ctx, request); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	os.Exit(0)
}

func newUpdateRig(t *testing.T) *updateRig {
	t.Helper()
	r := &updateRig{installRig: newInstallRig(t), server: newReleaseServer(t), systemd: &fakeSystemd{show: unitGone}, clock: time.Now().UTC().Truncate(time.Second)}
	r.mustRun(r.request(Install, r.release("one")))
	r.m = r.server.publish(t, "v0.0.2", r.installRig.candidate("v0.0.2", r.server.address(), ":"), pinnedCore)
	r.u = r.updater()
	r.clearCalls()
	return r
}

// again is the same gateway, with no attempt and no check, for another case that leaves it as it was.
func (r *updateRig) again(t *testing.T) *updateRig {
	t.Helper()
	if err := os.RemoveAll(updateDir(r.data)); err != nil {
		t.Fatal(err)
	}
	next := *r
	next.installRig = &installRig{}
	*next.installRig = *r.installRig
	next.t = t
	next.m = next.server.publish(t, "v0.0.2", r.installRig.candidate("v0.0.2", r.server.address(), ":"), pinnedCore)
	next.systemd.says(unitGone)
	next.u = next.updater()
	next.clearCalls()
	return &next
}

// updater is a new process's, over the same data dir.
func (r *updateRig) updater() *updater {
	release := currentRelease(r.data)
	u := newUpdater(r.data, Build{Version: "v0.0.1", Releases: r.server.address()}, &release)
	u.source, u.lockPath, u.proc, u.run = r.server.source(), r.lockPath(), filepath.Join(r.root, "proc"), r.systemd.run
	u.now, u.lockWait = func() time.Time { return r.clock }, 300*time.Millisecond
	return u
}

func (r *updateRig) mustView() UpdateView {
	r.t.Helper()
	view, err := r.u.view(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return view
}

func (r *updateRig) mustCheck(auto bool) {
	r.t.Helper()
	if err := r.u.check(context.Background(), auto); err != nil {
		r.t.Fatal(err)
	}
}

func (r *updateRig) launched() string {
	r.t.Helper()
	if _, err := r.u.launch(context.Background(), "v0.0.2", false); err != nil {
		r.t.Fatal(err)
	}
	a, _ := readAttempt(r.data)
	return a.Attempt
}

// applied launches an update and runs its job to its end, as systemd would.
func (r *updateRig) applied() error {
	r.t.Helper()
	err := r.u.apply(context.Background(), r.launched(), io.Discard)
	r.systemd.says(unitGone)
	return err
}

func (r *updateRig) wantLast(outcome UpdateOutcome, reason UpdateReason) UpdateLast {
	r.t.Helper()
	last := r.mustView().Last
	if last == nil || last.Outcome != outcome || (reason == "") != (last.Reason == nil) || (reason != "" && *last.Reason != reason) {
		raw, _ := json.Marshal(last)
		r.t.Fatalf("the last update is %s, want %s %s", raw, outcome, reason)
	}
	return *last
}

// wantUntouched says nothing the gateway runs from changed since the rig was made.
func (r *updateRig) wantUntouched(one string) {
	r.t.Helper()
	if now := currentRelease(r.data); now != one {
		r.t.Errorf("current names %s, want %s", now, one)
	}
	for _, call := range strings.Split(strings.TrimSpace(r.callLog()), "\n") {
		if call != "" && !strings.HasPrefix(call, "systemctl show") {
			r.t.Errorf("a failed update ran %s", call)
		}
	}
	if releases, _ := os.ReadDir(filepath.Join(r.data, "releases")); len(releases) != 1 {
		r.t.Errorf("a failed update left %d releases", len(releases))
	}
	if !r.journalGone() {
		r.t.Error("a failed update left a journal")
	}
	a, _ := readAttempt(r.data)
	if _, err := os.Stat(attemptStaged(r.data, a.Attempt)); !errors.Is(err, os.ErrNotExist) {
		r.t.Errorf("a failed update left what it staged: %v", err)
	}
	if !r.lockIsFree() {
		r.t.Error("a failed update left its lock held")
	}
}

// What is a release's version is the table's, which holds the release scripts to the same.
func TestAReleasesVersionIsTheTables(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "release-tags.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tags []struct {
		Tag     string
		Release bool
	}
	if err := json.Unmarshal(raw, &tags); err != nil || len(tags) == 0 {
		t.Fatalf("%v, %d tags", err, len(tags))
	}
	for _, c := range tags {
		if _, ok := parseReleaseVersion(c.Tag); ok != c.Release {
			t.Errorf("%q is a release's version: %t", c.Tag, ok)
		}
	}
}

func TestOnlyAReleaseIsOrderedAndUpdates(t *testing.T) {
	const address = "https://example.test/OWNER/sidegate/releases"
	for _, c := range []struct {
		build Build
		ok    bool
	}{
		{Build{"v0.1.0", address}, true}, {Build{"v10.0.0", address}, true},
		{Build{"v0.1.0", ""}, false}, {Build{"v0.1.0", "http://example.test/releases"}, false}, {Build{"v0.1.0", "https:///releases"}, false},
		{Build{"dev-1234", address}, false}, {Build{"v01.0.0", address}, false}, {Build{"v0.1", address}, false}, {Build{"v0.1.0-rc1", address}, false},
		{Build{"v0.1.0\n", address}, false}, {Build{"v9999999999.0.0", address}, false},
	} {
		if _, ok := c.build.release(); ok != c.ok {
			t.Errorf("%+v is a release: %t", c.build, ok)
		}
	}
	order := []string{"v0.0.9", "v0.0.10", "v0.2.0", "v0.10.0", "v1.0.0"}
	for i := range order {
		for j := range order {
			a, _ := parseReleaseVersion(order[i])
			b, _ := parseReleaseVersion(order[j])
			if a.above(b) != (i > j) {
				t.Errorf("%s above %s: %t", order[i], order[j], a.above(b))
			}
		}
	}
}

// The check sends a GET of the releases address, its User-Agent and nothing else, along each redirect.
func TestACheckSaysNothingOfTheGateway(t *testing.T) {
	s := newReleaseServer(t)
	s.latest(manifest{"v0.0.2", bundleName("v0.0.2"), 1, strings.Repeat("a", 64), strings.Repeat("b", 64)})
	if _, err := s.source().latest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.seen) != 2 || s.seen[0].URL.String() != "/OWNER/sidegate/releases/latest/download/sidegate.json" {
		t.Fatalf("%d requests, the first of %s", len(s.seen), s.seen[0].URL)
	}
	for _, r := range s.seen {
		if want := (http.Header{"User-Agent": {"sidegated"}}); r.Method != http.MethodGet || fmt.Sprint(r.Header) != fmt.Sprint(want) || r.URL.RawQuery != "" {
			t.Errorf("%s %s sent %v", r.Method, r.URL, r.Header)
		}
	}
}

func TestEachCheckFailure(t *testing.T) {
	good := manifest{"v0.0.2", bundleName("v0.0.2"), 1, strings.Repeat("a", 64), strings.Repeat("b", 64)}
	raw, _ := json.Marshal(good)
	with := func(change func(*manifest)) func(*releaseServer) {
		return func(s *releaseServer) {
			m := good
			change(&m)
			s.latest(m)
		}
	}
	says := func(f http.HandlerFunc) func(*releaseServer) {
		return func(s *releaseServer) { s.answer("/latest/download/"+manifestName, f) }
	}
	hops := func(n int) func(*releaseServer) {
		left := n - 1
		return says(func(w http.ResponseWriter, r *http.Request) {
			if left > 0 {
				left--
				http.Redirect(w, r, r.URL.Path, http.StatusFound)
				return
			}
			w.Write(raw)
		})
	}
	for name, c := range map[string]struct {
		want    CheckFailure
		arrange func(*releaseServer)
	}{
		"five redirects":    {"", hops(5)},
		"six redirects":     {CheckUnreachable, hops(6)},
		"a field more":      {"", says(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"new":1,` + string(raw[1:]))) })},
		"nothing published": {CheckFilesUnavailable, func(*releaseServer) {}},
		"a server error":    {CheckUnreachable, says(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })},
		"over 64 KiB": {CheckMalformed, says(func(w http.ResponseWriter, _ *http.Request) {
			w.Write(append(raw, strings.Repeat(" ", manifestLimit)...))
		})},
		"endless": {CheckMalformed, says(func(w http.ResponseWriter, _ *http.Request) {
			for {
				if _, err := w.Write(make([]byte, 32<<10)); err != nil {
					return
				}
			}
		})},
		"cut short": {CheckUnreachable, says(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
			w.Write(raw[:10])
		})},
		"cut short, ended cleanly":  {CheckMalformed, says(func(w http.ResponseWriter, _ *http.Request) { w.Write(raw[:10]) })},
		"empty":                     {CheckMalformed, says(func(http.ResponseWriter, *http.Request) {})},
		"a redirect to http":        {CheckUnreachable, func(s *releaseServer) { s.redirect("/latest/download/"+manifestName, plainly(t, raw)) }},
		"a version that isn't one":  {CheckMalformed, with(func(m *manifest) { m.Version = "latest" })},
		"another release's bundle":  {CheckMalformed, with(func(m *manifest) { m.Bundle = bundleName("v0.0.3") })},
		"a bundle that is a path":   {CheckMalformed, with(func(m *manifest) { m.Bundle = "../" + m.Bundle })},
		"a size of nothing":         {CheckMalformed, with(func(m *manifest) { m.Size = 0 })},
		"a size no bundle has":      {CheckMalformed, with(func(m *manifest) { m.Size = maxBundleFile + 1 })},
		"a checksum that isn't one": {CheckMalformed, with(func(m *manifest) { m.SHA256 = "abc" })},
		"a core that isn't one":     {CheckMalformed, with(func(m *manifest) { m.Core = "" })},
	} {
		t.Run(name, func(t *testing.T) {
			s := newReleaseServer(t)
			c.arrange(s)
			m, err := s.source().latest(context.Background())
			switch {
			case c.want == "" && (err != nil || m != good):
				t.Errorf("read %+v: %v", m, err)
			case c.want != "" && (err == nil || checkFailureOf(err) != c.want):
				t.Errorf("failed as %v, want %s", err, c.want)
			}
		})
	}

	t.Run("a clock outside the certificate", func(t *testing.T) {
		s := newReleaseServer(t)
		s.latest(good)
		source := s.source()
		source.client.Transport.(*http.Transport).TLSClientConfig.Time = func() time.Time { return time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) }
		if _, err := source.latest(context.Background()); err == nil || checkFailureOf(err) != CheckClockWrong {
			t.Errorf("failed as %v", err)
		}
	})
	t.Run("a certificate nobody trusts", func(t *testing.T) {
		s := newReleaseServer(t)
		s.latest(good)
		source := newReleaseSource(s.address(), x509.NewCertPool())
		if _, err := source.latest(context.Background()); err == nil || checkFailureOf(err) != CheckUnreachable {
			t.Errorf("failed as %v", err)
		}
	})
	t.Run("an address over http", func(t *testing.T) {
		plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("it was asked") }))
		defer plain.Close()
		if _, err := newReleaseSource(plain.URL, nil).latest(context.Background()); err == nil {
			t.Error("it was read")
		}
	})
}

// An automatic check fetches once a day whatever came of it, across restarts. A press always fetches.
func TestAnAutomaticCheckIsDueOnceADay(t *testing.T) {
	r := newUpdateRig(t)
	want := func(n int, why string) {
		t.Helper()
		if got := r.server.manifests(); got != n {
			t.Fatalf("%s: %d fetches, want %d", why, got, n)
		}
	}
	r.mustCheck(true)
	want(1, "the first load")
	offer := r.mustView().Offer
	if offer == nil || offer.Version != "v0.0.2" || offer.RestartsCore || offer.Notes != r.server.address()+"/tag/v0.0.2" {
		t.Fatalf("offered %+v", offer)
	}
	r.mustCheck(true)
	want(1, "a second load")
	r.clock = r.clock.Add(checkEvery - time.Second)
	r.u = r.updater()
	r.mustCheck(true)
	want(1, "a load after a restart, within the day")
	r.mustCheck(false)
	want(2, "a press")
	r.clock = r.clock.Add(checkEvery - time.Second)
	r.mustCheck(true)
	want(2, "a load within a day of the press")
	r.clock = r.clock.Add(time.Second)
	r.mustCheck(true)
	want(3, "a load a day after it")

	r.server.answer("/latest/download/"+manifestName, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	r.clock = r.clock.Add(checkEvery)
	r.mustCheck(true)
	view := r.mustView()
	if view.Check.Failure == nil || *view.Check.Failure != CheckUnreachable || view.Check.At == nil || !view.Check.At.Equal(r.clock) {
		t.Errorf("a failed check reads %+v", view.Check)
	}
	if view.Offer == nil || view.Offer.Version != "v0.0.2" {
		t.Error("a failed check forgot the offer")
	}
	r.mustCheck(true)
	want(4, "a load after a failed check")

	r.clock = r.clock.Add(-72 * time.Hour)
	r.mustCheck(true)
	want(5, "a load by a clock that went back before the last check")
}

// The start is on disk before the fetch: a restart in the middle of one doesn't ask again that day. Loads
// and a press that overlap are one fetch, and one that gives up leaves it running.
func TestOverlappingChecksAreOneFetchAndARestartMidFetchCounts(t *testing.T) {
	r := newUpdateRig(t)
	asked, answer := make(chan struct{}), make(chan struct{})
	raw, _ := json.Marshal(r.m)
	r.server.answer("/latest/download/"+manifestName, func(w http.ResponseWriter, _ *http.Request) {
		close(asked)
		<-answer
		w.Write(raw)
	})
	gone, leave := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() { left <- r.u.check(gone, true) }()
	<-asked
	leave()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Fatalf("a load that gave up: %v", err)
	}
	var wg sync.WaitGroup
	for _, auto := range []bool{true, false, true} {
		wg.Go(func() {
			if err := r.u.check(context.Background(), auto); err != nil {
				t.Error(err)
			}
		})
	}
	restarted := r.updater()
	if err := restarted.check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if r.server.manifests() != 1 {
		t.Fatalf("%d fetches while one runs, with a load after a restart", r.server.manifests())
	}
	time.Sleep(20 * time.Millisecond)
	close(answer)
	wg.Wait()
	if n, view := r.server.manifests(), r.mustView(); n != 1 || view.Offer == nil {
		t.Errorf("%d fetches, offered %+v", n, view.Offer)
	}
}

// A check whose start can't be written fetches nothing, so a full disk doesn't make every load ask.
func TestACheckThatCantBeRecordedFetchesNothing(t *testing.T) {
	r := newUpdateRig(t)
	if err := os.WriteFile(updateDir(r.data), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.u.check(context.Background(), true); err == nil || r.server.manifests() != 0 {
		t.Errorf("%v, %d fetches", err, r.server.manifests())
	}
}

// Only a newer release is offered, and what the offer says of the core is what an upgrade to it would say.
func TestTheOfferIsNewerAndSaysWhatTheUpgradeWould(t *testing.T) {
	r := newUpdateRig(t)
	for version, offered := range map[string]bool{"v0.0.0": false, "v0.0.1": false, "v0.0.2": true, "v0.1.0": true} {
		m := r.m
		m.Version, m.Bundle = version, bundleName(version)
		r.server.latest(m)
		r.mustCheck(false)
		if got := r.mustView().Offer; (got != nil) != offered {
			t.Errorf("%s offered %+v", version, got)
		}
	}
	r.server.latest(r.m)
	r.mustCheck(false)
	r.runCore()
	for _, core := range []string{"", "another core"} {
		bundle := r.release("two")
		if core != "" {
			r.coreBytes = []byte(core)
			r.pinCore()
			bundle = r.release("three")
			r.m.Core = pinnedCore
			r.server.latest(r.m)
			r.mustCheck(false)
		}
		plan, err := PlanLifecycle(context.Background(), r.request(Upgrade, bundle))
		if err != nil {
			t.Fatal(err)
		}
		if got := r.mustView().Offer; got == nil || got.RestartsCore != plan.RestartsCore || got.RestartsCore != (core != "") {
			t.Errorf("core %q: the offer says %+v, the upgrade %t", core, got, plan.RestartsCore)
		}
	}
}

// A development build, and one that runs from outside releases/, offers no update and says so.
func TestABuildThatTakesNoUpdatesSaysSo(t *testing.T) {
	r := newUpdateRig(t)
	r.mustCheck(false)
	for name, change := range map[string]func(*updater){
		"a checkout":                 func(u *updater) { u.build = Build{Version: "dev-1234"} },
		"a release with no address":  func(u *updater) { u.build.Releases = "" },
		"a release outside releases": func(u *updater) { u.release = nil },
	} {
		u := r.updater()
		change(u)
		view, err := u.view(context.Background())
		if want := (UpdateView{Version: u.build.Version}); err != nil || view != want {
			t.Errorf("%s: %+v %v", name, view, err)
		}
		if _, refused := u.press(context.Background(), "v0.0.2", false); refused == nil || refused.Status != http.StatusUnprocessableEntity || !strings.Contains(string(refused.Body), string(RefusedNoReleases)) {
			t.Errorf("%s: a press answered %+v", name, refused)
		}
	}
	if len(r.systemd.launches()) != 0 {
		t.Error("a job was started")
	}
}

// Where the job and its files stand says whether an update runs and how the last ended.
func TestTheViewReadsTheJobAndItsFiles(t *testing.T) {
	reason := func(r UpdateReason) *UpdateReason { return &r }
	shared := newUpdateRig(t)
	for name, c := range map[string]struct {
		unit    string
		started bool
		outcome string
		journal bool
		running bool
		want    UpdateOutcome
		reason  *UpdateReason
	}{
		"queued":                              {unit: unitQueued, running: true},
		"running":                             {unit: unitActive, started: true, running: true},
		"being stopped":                       {unit: unitStops, started: true, journal: true, running: true},
		"landed, and the unit not gone yet":   {unit: unitStops, started: true, outcome: `{"outcome":"landed","message":null}`, running: true},
		"never ran":                           {unit: unitGone, want: UpdateFailed, reason: reason(ReasonNoStart)},
		"never ran, awaiting collection":      {unit: unitFailed, want: UpdateFailed, reason: reason(ReasonNoStart)},
		"killed":                              {unit: unitGone, started: true, want: UpdateFailed, reason: reason(ReasonStopped)},
		"killed, awaiting collection":         {unit: unitFailed, started: true, want: UpdateFailed, reason: reason(ReasonStopped)},
		"ended, awaiting collection":          {unit: unitEnded, started: true, outcome: `{"outcome":"landed","message":null}`, want: UpdateLanded},
		"killed in its switch, not put back":  {unit: unitGone, started: true, journal: true, want: UpdateFailed, reason: reason(ReasonInterrupted)},
		"landed":                              {unit: unitGone, started: true, outcome: `{"outcome":"landed","message":null}`, want: UpdateLanded},
		"landed with an error":                {unit: unitGone, started: true, outcome: `{"outcome":"landedWithError","message":"prune"}`, want: UpdateLandedWithError},
		"rolled back":                         {unit: unitGone, started: true, outcome: `{"outcome":"rolledBack","message":"x"}`, want: UpdateRolledBack},
		"not put back":                        {unit: unitGone, started: true, journal: true, outcome: `{"outcome":"putBackFailed","message":"x"}`, want: UpdatePutBackFailed},
		"refused by the upgrade":              {unit: unitGone, started: true, outcome: `{"outcome":"refused","message":"x"}`, want: UpdateRefused},
		"moved, as the upgrade said":          {unit: unitGone, started: true, outcome: `{"outcome":"moved","message":"x"}`, want: UpdateFailed, reason: reason(ReasonMoved)},
		"another plan, as the upgrade said":   {unit: unitGone, started: true, outcome: `{"outcome":"planChanged","message":"x"}`, want: UpdateFailed, reason: reason(ReasonPlanChanged)},
		"failed, as the job said":             {unit: unitGone, started: true, outcome: `{"outcome":"failed","reason":"checksum","message":"x"}`, want: UpdateFailed, reason: reason(ReasonChecksum)},
		"an outcome a power cut left half of": {unit: unitGone, started: true, outcome: `{"outco`, want: UpdateFailed, reason: reason(ReasonStopped)},
	} {
		t.Run(name, func(t *testing.T) {
			r := shared.again(t)
			os.Remove(journalDir(r.data))
			id := r.launched()
			a, _ := readAttempt(r.data)
			a.Started = c.started
			if err := writeJSON(attemptFile(r.data), a); err != nil {
				t.Fatal(err)
			}
			if c.outcome != "" {
				os.WriteFile(attemptOutcome(r.data, id), []byte(c.outcome), 0o600)
			}
			if c.journal {
				os.Mkdir(journalDir(r.data), 0o700)
			}
			r.systemd.says(c.unit)
			// A daemon that restarted knows the same.
			r.u = r.updater()
			view := r.mustView()
			if c.running {
				if view.Running == nil || *view.Running != (UpdateRunning{id, "v0.0.2"}) || view.Last != nil {
					t.Fatalf("running %+v, last %+v", view.Running, view.Last)
				}
				return
			}
			if view.Running != nil || view.Last == nil || view.Last.Attempt != id || view.Last.Version != "v0.0.2" || !view.Last.At.Equal(r.clock) {
				t.Fatalf("running %+v, last %+v", view.Running, view.Last)
			}
			want := ""
			if c.reason != nil {
				want = string(*c.reason)
			}
			r.wantLast(c.want, UpdateReason(want))
		})
	}

	t.Run("systemd can't say", func(t *testing.T) {
		r := newUpdateRig(t)
		r.launched()
		r.systemd.says(unitStrange)
		if view, err := r.u.view(context.Background()); err == nil {
			t.Errorf("it reads %+v", view)
		}
	})
	t.Run("an attempt whose id isn't one", func(t *testing.T) {
		r := newUpdateRig(t)
		os.Mkdir(updateDir(r.data), 0o700)
		os.WriteFile(attemptFile(r.data), []byte(`{"attempt":"../../releases","version":"v0.0.2"}`), 0o600)
		if view := r.mustView(); view.Last != nil || view.Running != nil {
			t.Errorf("it reads %+v", view)
		}
	})
}

// A read that overlaps a launch, by this daemon or over SSH, never says the launched update failed: not with
// the job's unit sampled before systemd had it, not with the unit of the update before it, and not before it
// is queued.
func TestAReadDuringALaunchNeverSaysTheUpdateFailed(t *testing.T) {
	shared := newUpdateRig(t)
	wantRunning := func(t *testing.T, view UpdateView, attempt string) {
		t.Helper()
		if view.Running == nil || view.Running.Attempt != attempt || view.Last != nil {
			raw, _ := json.Marshal(view.Last)
			t.Errorf("running %+v, last %s, want %s running", view.Running, raw, attempt)
		}
	}
	for name, before := range map[string]func(*updateRig){
		"the first": func(*updateRig) {},
		"one after another landed": func(r *updateRig) {
			os.WriteFile(attemptOutcome(r.data, r.launched()), []byte(`{"outcome":"landed","message":null}`), 0o600)
			r.systemd.says(unitGone)
		},
	} {
		t.Run("the unit sampled before it, "+name, func(t *testing.T) {
			r := shared.again(t)
			before(r)
			launched := ""
			r.u.run = func(ctx context.Context, tool string, args ...string) (string, error) {
				out, err := r.systemd.run(ctx, tool, args...)
				if launched == "" && len(args) > 1 && args[1] == updateUnit {
					other := r.updater()
					if _, err := other.launch(ctx, "v0.0.2", false); err != nil {
						t.Error(err)
					}
					r.systemd.says(unitQueued)
					a, _ := readAttempt(r.data)
					launched = a.Attempt
				}
				return out, err
			}
			wantRunning(t, r.mustView(), launched)
		})
	}
	t.Run("before it is queued", func(t *testing.T) {
		r := shared.again(t)
		launcher := r.updater()
		launcher.run = func(ctx context.Context, tool string, args ...string) (string, error) {
			if tool == "systemd-run" {
				a, _ := readAttempt(r.data)
				wantRunning(t, r.mustView(), a.Attempt)
			}
			return r.systemd.run(ctx, tool, args...)
		}
		if _, err := launcher.launch(context.Background(), "v0.0.2", false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("its launcher killed before it was queued", func(t *testing.T) {
		r := shared.again(t)
		r.launched()
		r.systemd.says(unitGone)
		// What a killed holder leaves of the lock.
		if err := os.WriteFile(r.lockPath(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		r.wantLast(UpdateFailed, ReasonNoStart)
	})
}

// The files change between two of a read's own: what it answers is one moment's. A read that opens the file
// at path waits there for change, which puts another file in its place, and then reads text.
func TestAReadTakesTheFilesAsOneMomentsOwn(t *testing.T) {
	during := func(t *testing.T, path, text string, change func()) {
		t.Helper()
		os.Remove(path)
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Error(err)
			return
		}
		go func() {
			fifo, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			change()
			fifo.WriteString(text)
			fifo.Close()
		}()
	}
	write := func(path, text string) {
		if err := writeFileAtomic(path, []byte(text), 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Run("a put-back is said, and its journal goes", func(t *testing.T) {
		r := newUpdateRig(t)
		id := r.launched()
		a, _ := readAttempt(r.data)
		a.Started = true
		writeJSON(attemptFile(r.data), a)
		r.systemd.says(unitGone)
		os.Mkdir(journalDir(r.data), 0o700)
		outcome := attemptOutcome(r.data, id)
		// The first read of it finds nothing, and the put-back falls in the second.
		during(t, outcome, "", func() {
			during(t, outcome, "", func() {
				write(outcome, `{"outcome":"rolledBack","message":"x"}`)
				os.Remove(journalDir(r.data))
			})
		})
		r.wantLast(UpdateRolledBack, "")
	})
	t.Run("another update ends", func(t *testing.T) {
		r := newUpdateRig(t)
		r.mustCheck(false)
		first := r.launched()
		write(attemptOutcome(r.data, first), `{"outcome":"rolledBack","message":"x"}`)
		r.systemd.says(unitGone)
		if view := r.mustView(); view.Offer == nil {
			t.Fatal("nothing is offered, so this proves nothing")
		}
		raw, _ := os.ReadFile(attemptFile(r.data))
		second := "00000000000000ff"
		during(t, attemptFile(r.data), string(raw), func() {
			os.Mkdir(attemptDir(r.data, second), 0o700)
			write(attemptOutcome(r.data, second), `{"outcome":"failed","reason":"offerChanged","message":"x"}`)
			write(attemptFile(r.data), strings.ReplaceAll(string(raw), first, second))
			dropAttempts(r.data, second)
		})
		if view := r.mustView(); view.Last == nil || view.Last.Attempt != second || view.Last.Reason == nil || *view.Last.Reason != ReasonOfferChanged || view.Offer != nil {
			t.Errorf("last %+v, offered %+v", view.Last, view.Offer)
		}
	})
}

// A press starts one job: the attempt is on disk before it, under the lifecycle lock, its id the daemon's
// own. Another press meanwhile, here or after a restart, is answered with the update that runs, and one
// after it ended starts a new attempt and drops the old one's files.
func TestAPressStartsOneJob(t *testing.T) {
	r := newUpdateRig(t)
	r.mustCheck(false)
	var before *updateAttempt
	locked := false
	r.systemd.start = func([]string) error {
		before, _ = readAttempt(r.data)
		locked = !r.lockIsFree()
		return nil
	}
	view, refused := r.u.press(context.Background(), "v0.0.2", false)
	if refused != nil || view.Running == nil {
		t.Fatalf("%+v %+v", view, refused)
	}
	first := view.Running.Attempt
	if before == nil || before.Attempt != first || before.Version != "v0.0.2" || before.Expect != currentRelease(r.data) || before.Started {
		t.Errorf("at the launch the attempt on disk was %+v", before)
	}
	if !locked {
		t.Error("the lock was free between the attempt and its job")
	}
	if _, err := os.Stat(r.lockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the press left the lock's file: %v", err)
	}
	if got, want := r.systemd.launches()[0], jobLine(r.data, filepath.Join(releaseDir(r.data, currentRelease(r.data)), bundleDaemon), first); !slices.Equal(got, want) {
		t.Errorf("systemd-run %q", got)
	}
	// The job holds the lock as it runs.
	lock := r.holdLock()
	for _, unit := range []string{unitQueued, unitActive, unitStops} {
		r.systemd.says(unit)
		for _, u := range []*updater{r.u, r.updater()} {
			for _, version := range []string{"v0.0.2", "v0.0.3"} {
				again, refused := u.press(context.Background(), version, false)
				if refused != nil || again.Running == nil || again.Running.Attempt != first {
					t.Errorf("a press while one runs: %+v %+v", again, refused)
				}
			}
			if _, err := u.launch(context.Background(), "v0.0.2", false); !errors.Is(err, errUpdateRunning) {
				t.Errorf("update over SSH while one runs: %v", err)
			}
		}
	}
	lock.Close()
	if n := len(r.systemd.launches()); n != 1 {
		t.Fatalf("%d jobs", n)
	}
	r.systemd.says(unitGone)
	view, refused = r.u.press(context.Background(), "v0.0.2", false)
	if refused != nil || view.Running == nil || view.Running.Attempt == first {
		t.Fatalf("a press after it ended: %+v %+v", view, refused)
	}
	if _, err := os.Stat(attemptDir(r.data, first)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the first attempt's files stayed: %v", err)
	}
}

func TestPressesAtOnceAreOneJob(t *testing.T) {
	r := newUpdateRig(t)
	r.mustCheck(false)
	// Two daemons can't run, but the page's press and update over SSH can meet.
	other := r.updater()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if view, refused := r.u.press(context.Background(), "v0.0.2", false); refused != nil || view.Running == nil {
				t.Errorf("%+v %+v", view, refused)
			}
		})
		wg.Go(func() {
			if _, err := other.launch(context.Background(), "v0.0.2", false); err != nil && !errors.Is(err, errUpdateRunning) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	jobs := r.systemd.launches()
	if a, _ := readAttempt(r.data); len(jobs) != 1 || a == nil || a.Attempt != jobs[0][len(jobs[0])-1] {
		t.Errorf("%d jobs, and the attempt on disk is %+v", len(jobs), a)
	}
}

func TestAPressIsRefused(t *testing.T) {
	r := newUpdateRig(t)
	want := func(status int, reason Reason, version string, restarts bool) {
		t.Helper()
		_, refused := r.u.press(context.Background(), version, restarts)
		if refused == nil || refused.Status != status || !strings.Contains(string(refused.Body), `"`+string(reason)+`"`) {
			t.Errorf("want %d %s: %+v", status, reason, refused)
		}
	}
	want(422, RefusedNothingNewer, "v0.0.2", false)
	r.mustCheck(false)
	want(422, RefusedOfferChanged, "v0.0.3", false)
	want(422, RefusedOfferChanged, "v0.0.2", true)
	lock := r.holdLock()
	want(409, RefusedLifecycle, "v0.0.2", false)
	if a, _ := readAttempt(r.data); a != nil {
		t.Errorf("a refused press left the attempt %+v", a)
	}
	lock.Close()
	r.systemd.showErr = errors.New("no bus")
	want(503, RefusedUnavailable, "v0.0.2", false)
	r.systemd.showErr = nil
	r.systemd.start = func([]string) error { return errors.New("no bus") }
	r.systemd.show = unitGone
	view, refused := r.u.press(context.Background(), "v0.0.2", false)
	if refused != nil || view.Running != nil || view.Last == nil || *view.Last.Reason != ReasonNoStart || !strings.Contains(*view.Last.Message, "no bus") {
		t.Fatalf("a press systemd refused: %+v %+v", view, refused)
	}
	if _, err := os.Stat(attemptStaged(r.data, view.Last.Attempt)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("it left its staging directory: %v", err)
	}
	// It can also start, and lose the answer.
	r.systemd.start = func([]string) error {
		r.systemd.show = unitQueued
		return errors.New("timed out")
	}
	if view, refused = r.u.press(context.Background(), "v0.0.2", false); refused != nil || view.Running == nil {
		t.Errorf("a press whose answer was lost: %+v %+v", view, refused)
	}
}

// A failed unit that --collect hasn't removed yet is waited out, since systemd-run refuses its name.
func TestALaunchWaitsForTheLastUnitToGo(t *testing.T) {
	r := newUpdateRig(t)
	r.systemd.says(unitFailed)
	time.AfterFunc(300*time.Millisecond, func() { r.systemd.says(unitGone) })
	r.launched()
	if len(r.systemd.launches()) != 1 {
		t.Error("no job started")
	}
}

// systemd gives a stopped job longer than its upgrade's rollback may take, and the words it runs after the
// job carry each character a data dir may have.
func TestTheJobsLine(t *testing.T) {
	line := jobLine("/data/a b;$HOME%n/sidegate", "/data/a b;$HOME%n/sidegate/releases/one/sidegated", "0123456789abcdef")
	want := []string{"--unit=sidegate-update", "--collect", "--no-block", "--quiet", "-p", "RuntimeMaxSec=1200s", "-p", "TimeoutStopSec=300s",
		"-p", `ExecStopPost=-/bin/rm -rf "/data/a b;$$HOME%n/sidegate/update/0123456789abcdef/staged"`,
		"-p", `ExecStopPost=-"/data/a b;$HOME%n/sidegate/rollback/sidegated" settle-update --data-dir "/data/a b;$$HOME%n/sidegate"`,
		"--", "/data/a b;$HOME%n/sidegate/releases/one/sidegated", "apply-update", "--data-dir", "/data/a b;$$HOME%n/sidegate", "--attempt", "0123456789abcdef"}
	if !slices.Equal(line, want) {
		t.Errorf("the job's line is\n%s", strings.Join(line, "\n"))
	}
	if updateStopLimit < rollbackLimit+time.Minute || updateStopLimit <= time.Minute {
		t.Errorf("a stopped job has %s, and its rollback may take %s", updateStopLimit, rollbackLimit)
	}
}

// The client refuses what the one command refuses: TLS below 1.2.
func TestTheSourceNeedsTLS12(t *testing.T) {
	s := newReleaseServer(t)
	s.latest(manifest{"v0.0.2", bundleName("v0.0.2"), 1, strings.Repeat("a", 64), strings.Repeat("b", 64)})
	old := httptest.NewUnstartedServer(s.Config.Handler)
	old.TLS = &tls.Config{MaxVersion: tls.VersionTLS11}
	old.StartTLS()
	defer old.Close()
	roots := x509.NewCertPool()
	roots.AddCert(old.Certificate())
	if _, err := newReleaseSource(old.URL+"/OWNER/sidegate/releases", roots).latest(context.Background()); err == nil {
		t.Error("it was read over TLS 1.1")
	}
}
