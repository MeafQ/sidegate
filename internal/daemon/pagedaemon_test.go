package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

// TestPageDaemon serves the real handler with the real controller, its other actors
// played here, behind the same controls as web/tests/e2e/fake-daemon.mjs. The page's
// browser checks and the contract check drive it through real-daemon.mjs, and nothing
// else runs it.
func TestPageDaemon(t *testing.T) {
	port := os.Getenv("PAGE_DAEMON_PORT")
	if port == "" {
		t.Skip("run through web/tests/e2e/real-daemon.mjs")
	}
	h := &pageDaemon{t: t, dist: os.Getenv("PAGE_DAEMON_DIST"), base: os.Getenv("PAGE_DAEMON_BASE")}
	h.reset()
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("page daemon listening")
	server := &http.Server{Handler: h}
	if err := server.Serve(listener); err != nil {
		t.Fatal(err)
	}
}

type logEntry struct {
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	Status         int     `json:"status"`
	IfMatch        *string `json:"ifMatch"`
	IfNoneMatch    *string `json:"ifNoneMatch"`
	CSRF           *string `json:"csrf"`
	UniFiCSRF      *string `json:"unifiCsrf"`
	IdempotencyKey *string `json:"idempotencyKey"`
	ContentType    *string `json:"contentType"`
	Body           string  `json:"body"`
	Replayed       bool    `json:"replayed"`
}

type pageDaemon struct {
	t          *testing.T
	dist, base string

	mu           sync.Mutex
	app          *App
	handler      http.Handler
	stop         func()
	token        int
	log          []*logEntry
	entries      map[*http.Request]*logEntry
	sessionFails int
	writeFails   int
	eventsFail   int
	badShape     bool
	trimNames    bool
	unifi        string
	syncer       *fakeSyncer
	// keyTries is the Network Application's address for each answer a key's try gets, by the
	// fake daemon's unifiKeyAnswer: "" takes any key, keyRefused refuses it, and notAnswering is
	// an address nothing listens on.
	keyTries map[string]string
	// releases is the releases address, and job the update job's unit.
	releases *releaseServer
	job      *fakeSystemd
}

// fakeSyncer plays UniFi: each want gets a client at once, and routes the test names
// point at it.
type fakeSyncer struct {
	mu     sync.Mutex
	routes map[domain.SlotID][]unifi.Route
	bound  map[domain.SlotID]domain.NetworkID
}

func (f *fakeSyncer) Reconcile(_ context.Context, wants []unifi.Want) (unifi.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var report unifi.Report
	f.bound = map[domain.SlotID]domain.NetworkID{}
	for _, w := range wants {
		id := w.Bound
		if id == "" {
			id = domain.NetworkID("n-" + string(w.Slot))
		}
		f.bound[w.Slot] = id
		report.Slots = append(report.Slots, unifi.SlotResult{Slot: w.Slot, NetworkID: id})
		report.Inventory.Owned = append(report.Inventory.Owned, unifi.Owned{NetworkID: id, Slot: w.Slot, Routes: f.routes[w.Slot]})
	}
	return report, nil
}

func (f *fakeSyncer) Inspect(context.Context) (unifi.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var inventory unifi.Inventory
	for slot, id := range f.bound {
		inventory.Owned = append(inventory.Owned, unifi.Owned{NetworkID: id, Slot: slot, Routes: f.routes[slot]})
	}
	return inventory, nil
}

func (h *pageDaemon) current() (*App, http.Handler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.app, h.handler
}

func (h *pageDaemon) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stop != nil {
		h.stop()
		h.app.feed.dropAll()
		h.app.Close()
	}
	h.token, h.log, h.entries = 1, nil, map[*http.Request]*logEntry{}
	h.sessionFails, h.writeFails, h.eventsFail, h.badShape, h.trimNames, h.unifi = 0, 0, 0, false, false, ""
	dir, err := os.MkdirTemp("", "page-daemon-")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(ProofPath(dir), []byte(harnessProof), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if h.keyTries == nil {
		h.keyTries = keyTries()
	}
	b := Bootstrap{DataDir: dir, RunDir: dir + "/run", BasePath: h.base, ClientDNS: "1.1.1.1", NetworkEndpoint: h.keyTries[""], NetworkSite: "default",
		ProbeTargets: []TargetSpec{{URL: "https://target.example/", Status: 204}}, BaselineTargets: []TargetSpec{{URL: "http://target.example/"}}}
	app, err := Open(context.Background(), b)
	if err != nil {
		h.t.Fatal(err)
	}
	h.syncer = &fakeSyncer{routes: map[domain.SlotID][]unifi.Route{}}
	app.control.key.syncer = h.syncer
	h.seedUpdates(app)
	h.seed(app)
	h.app = app
	h.handler = app.handler(os.DirFS(h.dist), h.currentToken, h.markReplayed)
	h.stop = h.start(app)
}

const (
	harnessRelease = "0123456789ab"
	harnessCore    = "5939649960deaf535bcd5f7ce0467a2b1d63b72a2758a6f42eade714ae8d5328"
)

// seedUpdates makes the daemon v0.1.0, installed in a data dir of the updater's own, whose releases address
// has nothing newer.
func (h *pageDaemon) seedUpdates(app *App) {
	if h.releases == nil {
		h.releases = newReleaseServer(h.t)
	}
	dir, err := os.MkdirTemp("", "page-updates-")
	if err != nil {
		h.t.Fatal(err)
	}
	release := harnessRelease
	core := filepath.Join(coreDir(dir, harnessCore), bundleCore)
	for _, err := range []error{os.MkdirAll(filepath.Dir(core), 0o700), os.WriteFile(core, nil, 0o700), os.MkdirAll(releaseDir(dir, release), 0o700),
		os.Symlink(core, filepath.Join(releaseDir(dir, release), bundleCore)), os.Symlink(releaseDir(dir, release), filepath.Join(dir, "current"))} {
		if err != nil {
			h.t.Fatal(err)
		}
	}
	h.job = &fakeSystemd{show: unitGone}
	u := newUpdater(dir, Build{Version: "v0.1.0", Releases: h.releases.address()}, &release)
	u.source, u.run, u.lockPath, u.proc = h.releases.source(), h.job.run, filepath.Join(dir, "lifecycle.lock"), filepath.Join(dir, "proc")
	app.updates = u
	h.latest("v0.1.0")
}

func (h *pageDaemon) latest(version string) {
	h.releases.latest(manifest{version, bundleName(version), 1 << 20, harnessCore, harnessCore})
}

const harnessProof = "the-harness-proof"

func keyTries() map[string]string {
	answering := func(status int) string {
		return httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			rw.WriteHeader(status)
			if strings.Contains(r.URL.Path, "/v2/") {
				io.WriteString(rw, `[]`)
				return
			}
			io.WriteString(rw, `{"meta":{"rc":"ok"},"data":[]}`)
		})).URL
	}
	silent := httptest.NewTLSServer(http.NotFoundHandler())
	silent.Close()
	return map[string]string{"": answering(http.StatusOK), "keyRefused": answering(http.StatusUnauthorized), "notAnswering": silent.URL}
}

func (h *pageDaemon) currentToken() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return "token-" + strconv.Itoa(h.token)
}

func (h *pageDaemon) markReplayed(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.entries[r]; e != nil {
		e.Replayed = true
	}
}

func fetchedNow() catalogue.FetchState {
	now, _ := json.Marshal(time.Now().Add(-time.Hour))
	var fs catalogue.FetchState
	if err := fs.UnmarshalBinary(fmt.Appendf(nil, `{"checked_at":%s,"updated_at":%s}`, now, now)); err != nil {
		panic(err)
	}
	return fs
}

// switchFromView is a switch as Go stores it, from the page's view of it that a check seeds.
func switchFromView(e SwitchEvent) domain.Switch {
	selection := func(s SelectionDTO) domain.Selection {
		switch s.Kind {
		case "direct":
			return domain.Direct()
		case "block":
			return domain.Blocked()
		case "down":
			return domain.Closed(named(downNames, s.Reason))
		}
		return domain.Serving(domain.ServerRef{Lineage: *s.Server})
	}
	owner := domain.SlotOwner(e.Owner.Slot)
	switch e.Owner.Kind {
	case "group":
		owner = domain.GroupOwner(e.Owner.Group)
	case "source":
		owner = domain.SourceOwner(e.Owner.Source)
	}
	ms := func(v *float64) *time.Duration {
		if v == nil {
			return nil
		}
		d := time.Duration(*v * float64(time.Millisecond))
		return &d
	}
	s := domain.Switch{ID: e.ID, Owner: owner, Slots: e.Slots, Intent: e.Intent, From: selection(e.From), To: selection(e.To), Reason: named(reasonNames, e.Reason),
		RequestedAt: e.RequestedAt, AppliedAt: e.AppliedAt}
	for _, ev := range e.Evidence {
		s.Evidence = append(s.Evidence, domain.EvidenceSummary{Server: domain.ServerRef{Lineage: ev.Server}, Kind: named(kindNames, ev.Kind), Verdict: named(verdictNames, ev.Verdict),
			Target: domain.TargetID(ev.Target), Round: ev.Round, At: ev.At, Ping: ms(ev.PingMs), NewConn: ms(ev.NewConnectionMs)})
	}
	return s
}

// named is the value a name map gives that name.
func named[K comparable](names map[K]string, name string) K {
	for k, n := range names {
		if n == name {
			return k
		}
	}
	var none K
	return none
}

// seed stores what the fake starts with, at the fake's revisions.
func (h *pageDaemon) seed(app *App) {
	ctx := context.Background()
	c := app.control
	server := func(id, label, country string) domain.Lineage {
		l := lineage(h.t, id, "aurora", country, id+".example")
		l.Label = label
		return l
	}
	lineages := []domain.Lineage{server("l-ams", "Amsterdam", "NL"), server("l-fra", "Frankfurt", "DE"), server("l-par", "Paris", "FR")}
	keys, secrets, err := c.newKeys()
	if err != nil {
		h.t.Fatal(err)
	}
	allocation, err := domain.Allocate(nil, keys)
	if err != nil {
		h.t.Fatal(err)
	}
	allocation.NetworkID, allocation.Unrouted = "n1", true
	intent, err := domain.NewIntent("EU", domain.GroupExit(sourceGroup("aurora")))
	if err != nil {
		h.t.Fatal(err)
	}
	row := slotRow{Slot: domain.Slot{ID: "eu", Version: 3, Intent: intent, Allocation: allocation}, Selection: domain.Serving(lineages[0].Ref)}
	secrets["source/aurora/url"] = domain.NewSecret("https://aurora.example/source")
	source := domain.Source{ID: "aurora", Revision: 1, Name: "Aurora",
		Origin: domain.RemoteOrigin(domain.Remote{Location: "source/aurora/url", UserAgent: catalogue.DefaultUserAgent, RefreshEvery: 6 * time.Hour})}
	settings := DefaultSettings()
	fs := fetchedNow()
	members, err := domain.AdmitMembers(asMembers(rule{Sources: []domain.SourceID{source.ID}}))
	if err != nil {
		h.t.Fatal(err)
	}
	group := domain.Group{ID: sourceGroup(source.ID), Revision: 1, Name: "All Aurora", Members: members}
	next := c.world.with(worldChange{lineages: lineages, groups: []domain.Group{group}, uses: []exitUse{{Slot: "eu", Exit: intent.Exit()}}})
	tx := transaction{Lineages: lineages, Groups: []domain.Group{group}, Slots: []slotRow{row}, Secrets: secrets, Sources: []domain.Source{source},
		Owners:  map[domain.Owner]policyMemory{domain.GroupOwner(group.ID): {Selection: row.Selection}},
		Fetches: map[domain.SourceID]catalogue.FetchState{"aurora": fs}, Settings: &settings,
		Meta: map[string]uint64{"revision": 41, "settings_revision": 5, "catalogue_revision": 1}}
	if err := c.store.commit(ctx, tx); err != nil {
		h.t.Fatal(err)
	}
	c.state.revision, c.state.settings, c.state.settingsRevision, c.state.catalogueRevision = 41, settings, 5, 1
	c.seenSettings = 5
	c.state.slots["eu"] = row
	c.state.owners[domain.GroupOwner(group.ID)] = tx.Owners[domain.GroupOwner(group.ID)]
	c.state.sources["aurora"] = source
	c.state.fetches["aurora"] = fs
	for ref, secret := range secrets {
		c.state.secrets[ref] = secret
	}
	c.queue = catalogue.NewQueue()
	c.queue.Set("aurora", c.due(source, fs))
	c.setWorld(next)
	c.publish(time.Now())
	c.changes = nil
}

// start runs the controller with the actors it talks to played here: the runtime applies
// every plan at once, each candidate keeps answering probes, and a fetch finds nothing new.
func (h *pageDaemon) start(app *App) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	c := app.control
	goals := make(chan gateway.Plan, 1)
	// Unbuffered, so the controller takes a plan's report, and the core epoch in it, before the
	// plan's signals. It drops a signal from an epoch no report has named yet.
	reports := make(chan gateway.Report)
	frames := make(chan signalFrame, 8)
	jobs := make(chan catalogue.Request, fetchCapacity)
	results := make(chan catalogue.Refresh, fetchCapacity)
	wants, syncers := make(chan []unifi.Want, 1), make(chan unifi.Syncer, 1)
	c.goals, c.plans, c.fetchJobs = goals, make(chan observationPlan, 1), jobs
	c.wants, c.syncers, c.clientReports = wants, syncers, make(chan clientReport, 1)
	syncer := app.control.key.syncer
	var wg sync.WaitGroup
	actor := func(run func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run()
		}()
	}
	actor(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case plan := <-goals:
				report := gateway.Report{Generation: plan.Generation(), Topology: gateway.Topology{Epoch: "lab", Generation: plan.Generation()}, Core: gateway.CoreVersions{Release: "26.10.31", Running: "26.10.31"}}
				var signals []domain.Signal
				for _, target := range plan.Slots() {
					phase := gateway.Closed
					if _, down := target.Selection.Down(); !down {
						phase = gateway.Ready
					}
					report.Slots = append(report.Slots, gateway.SlotReport{Slot: target.Slot.ID, Intent: target.Slot.Version, Operation: target.Operation,
						Phase: phase, Applied: target.Selection, NetworkID: target.Slot.Allocation.NetworkID, CoreEpoch: "lab", NetworkEstablished: true})
				}
				for _, ref := range plan.Servers() {
					latency := 20 * time.Millisecond
					signals = append(signals, domain.Signal{Stamp: domain.Stamp{Epoch: "lab", At: time.Now()}, Server: ref, Kind: domain.ActiveProbe,
						Verdict: domain.Succeeded, Purpose: domain.Heartbeat, Target: "t1", NewConn: &latency})
				}
				for _, removed := range plan.Removals() {
					report.Deleted = append(report.Deleted, removed.ID)
				}
				select {
				case reports <- report:
				case <-ctx.Done():
					return
				}
				select {
				case frames <- signalFrame{Signals: signals, WAN: wanBelief{State: domain.WANUp, At: time.Now()}}:
				case <-ctx.Done():
					return
				}
			}
		}
	})
	actor(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-app.manual:
				latency := 222 * time.Millisecond
				if m.Server.Lineage == "l-ams" {
					latency = 111 * time.Millisecond
				}
				m.Reply <- domain.Signal{Stamp: domain.Stamp{Epoch: "lab", At: time.Now()}, Server: m.Server, Kind: domain.ActiveProbe, Purpose: domain.ManualProbe,
					Target: "t1", Verdict: domain.Succeeded, Ping: &latency, NewConn: &latency}
			}
		}
	})
	actor(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-jobs:
				select {
				case results <- catalogue.Refresh{Basis: job.Basis(), Outcome: catalogue.Unchanged, Next: fetchedNow()}:
				case <-ctx.Done():
					return
				}
			}
		}
	})
	actor(func() { runClients(ctx, syncer, syncers, wants, c.clientReports, time.After) })
	actor(func() { c.run(ctx, app.commands, results, frames, reports) })
	return func() {
		cancel()
		wg.Wait()
	}
}

// paused runs change with the controller stopped, for what no command can do.
func (h *pageDaemon) paused(change func(app *App)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stop()
	change(h.app)
	h.stop = h.start(h.app)
}

type recorder struct {
	http.ResponseWriter
	entry *logEntry
	// handOut is UniFi's token, which nginx adds to an answer as X-Csrf-Token, or empty.
	handOut string
}

func (r *recorder) WriteHeader(status int) {
	r.entry.Status = status
	if r.handOut != "" && slices.Contains(handedOut, status) {
		r.Header().Set("X-Csrf-Token", r.handOut)
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.entry.Status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// handedOut are the statuses nginx's add_header acts on, with no "always".
var handedOut = []int{200, 201, 204, 206, 301, 302, 303, 304, 307, 308}

// unifiLogin plays UniFi's login and nginx in front of the daemon, as measured on the
// gateway: a PUT, POST or DELETE whose X-Csrf-Token isn't the
// session's token gets UniFi's 403, and an answer to a request that carried none hands
// the token out. The hook clears X-Csrf-Token, so the daemon never sees it. It reports
// whether the request goes on to the daemon.
func unifiLogin(token string, rw *recorder, r *http.Request) bool {
	provided := r.Header.Get("X-Csrf-Token")
	r.Header.Del("X-Csrf-Token")
	if provided != token && !slices.Contains([]string{http.MethodGet, http.MethodHead, http.MethodOptions}, r.Method) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusForbidden)
		io.WriteString(rw, `{"error":{"code":403,"message":"Forbidden"}}`)
		return false
	}
	if provided == "" {
		rw.handOut = token
	}
	return true
}

func (r *recorder) Flush() { r.ResponseWriter.(http.Flusher).Flush() }

func headerOf(r *http.Request, name string) *string {
	if v, ok := r.Header[http.CanonicalHeaderKey(name)]; ok && len(v) > 0 {
		return &v[0]
	}
	return nil
}

func (h *pageDaemon) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if name, ok := strings.CutPrefix(r.URL.Path, "/__control/"); ok {
		h.control(rw, r, name)
		return
	}
	app, handler := h.current()
	body, _ := io.ReadAll(r.Body)
	entry := &logEntry{Method: r.Method, Path: r.URL.Path, IfMatch: headerOf(r, "If-Match"), IfNoneMatch: headerOf(r, "If-None-Match"),
		CSRF: headerOf(r, CSRFHeader), UniFiCSRF: headerOf(r, "X-Csrf-Token"), IdempotencyKey: headerOf(r, "Idempotency-Key"), ContentType: headerOf(r, "Content-Type"), Body: string(body)}
	h.mu.Lock()
	h.log = append(h.log, entry)
	h.entries[r] = entry
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.entries, r)
		h.mu.Unlock()
	}()
	recorded := &recorder{ResponseWriter: rw, entry: entry}
	rw = recorded
	if unifi := h.unifiToken(); unifi != "" && !unifiLogin(unifi, recorded, r) {
		return
	}
	if h.fault(app, rw, r, body) {
		return
	}
	if h.trim() {
		var fields map[string]any
		if json.Unmarshal(body, &fields) == nil {
			if name, ok := fields["name"].(string); ok {
				fields["name"] = strings.TrimSpace(name)
				body, _ = json.Marshal(fields)
			}
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.Header.Set(AuthHeader, "200")
	r.Header.Set(ProofHeader, harnessProof)
	if h.isBadShape() && r.URL.Path == h.base+"api/state" {
		view := app.Snapshot()
		view.WAN = "degraded"
		encoded, _ := json.Marshal(view)
		writeAnswer(rw, &Answer{Status: http.StatusOK, Body: encoded})
		return
	}
	handler.ServeHTTP(rw, r)
}

func (h *pageDaemon) unifiToken() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unifi
}

func (h *pageDaemon) trim() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.trimNames
}

func (h *pageDaemon) isBadShape() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.badShape
}

// fault plays what the fake's failure switches do. A 503 for a write comes after the
// checks and the replay, as a real one would.
func (h *pageDaemon) fault(app *App, rw http.ResponseWriter, r *http.Request, body []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.URL.Path == h.base+"api/session" && h.sessionFails > 0:
		h.sessionFails--
		writeAnswer(rw, webRefusal(http.StatusServiceUnavailable, "unavailable", RefusedUnavailable, "Session store busy."))
		return true
	case r.URL.Path == h.base+"api/events" && h.eventsFail > 0:
		h.eventsFail--
		rw.Header().Set("Content-Type", "text/html")
		rw.WriteHeader(http.StatusBadGateway)
		io.WriteString(rw, "<h1>502 Bad Gateway</h1>")
		return true
	case r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, h.base+"api/") && h.writeFails > 0:
		key := r.Header.Get("Idempotency-Key")
		if r.Header.Get(CSRFHeader) != "token-"+strconv.Itoa(h.token) || key == "" || (len(body) > 0 && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")) {
			return false
		}
		if _, _, found, _ := app.Queries.state.receipt(r.Context(), domain.RequestID(key)); found {
			return false
		}
		h.writeFails--
		writeAnswer(rw, webRefusal(http.StatusServiceUnavailable, "unavailable", RefusedUnavailable, "Controller busy."))
		return true
	}
	return false
}

func (h *pageDaemon) control(rw http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	app, _ := h.current()
	query := r.URL.Query()
	var in map[string]json.RawMessage
	json.NewDecoder(r.Body).Decode(&in)
	refused := false
	reply := func(value any) {
		if refused {
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(value)
	}
	slotOf := func(id string) *SlotView {
		for _, s := range app.Snapshot().Slots {
			if string(s.ID) == id {
				return &s
			}
		}
		return nil
	}
	subOf := func(id string) *SourceView {
		for _, s := range app.Snapshot().Sources {
			if string(s.ID) == id {
				return &s
			}
		}
		return nil
	}
	// A control the daemon refuses fails its call, so the check that asked for it sees the refusal.
	must := func(_ Receipt, err error) {
		if err != nil && !refused {
			refused = true
			http.Error(rw, fmt.Sprintf("%s: %v", name, err), http.StatusConflict)
		}
	}
	id := query.Get("id")
	switch name {
	case "reset":
		h.reset()
		reply(true)
	case "log":
		h.mu.Lock()
		defer h.mu.Unlock()
		reply(h.log)
	case "slot":
		reply(slotOf(id))
	case "source":
		reply(subOf(id))
	case "sources":
		reply(app.Snapshot().Sources)
	case "settings":
		reply(app.Settings())
	case "sseClients":
		reply(app.feed.size())
	case "rotateToken":
		h.mu.Lock()
		h.token++
		h.mu.Unlock()
		reply(true)
	case "dropEvents":
		app.feed.dropAll()
		reply(true)
	case "set":
		h.mu.Lock()
		for key, value := range in {
			switch key {
			case "sessionFails":
				json.Unmarshal(value, &h.sessionFails)
			case "writeFails":
				json.Unmarshal(value, &h.writeFails)
			case "eventsFail":
				json.Unmarshal(value, &h.eventsFail)
			case "badShape":
				json.Unmarshal(value, &h.badShape)
			case "trimNames":
				json.Unmarshal(value, &h.trimNames)
			case "unifi":
				h.unifi = ""
				json.Unmarshal(value, &h.unifi)
			}
		}
		h.mu.Unlock()
		if raw, ok := in["unifiKeyAnswer"]; ok {
			var answer *string
			json.Unmarshal(raw, &answer)
			endpoint, known := h.keyTries[""], true
			if answer != nil {
				endpoint, known = h.keyTries[*answer]
			}
			if !known {
				http.Error(rw, "no unifiKeyAnswer "+string(raw), http.StatusBadRequest)
				return
			}
			h.paused(func(app *App) { app.bootstrap.NetworkEndpoint = endpoint })
		}
		if raw, ok := in["latest"]; ok {
			var version *string
			json.Unmarshal(raw, &version)
			if version == nil {
				h.releases.answer("/latest/download/"+manifestName, http.NotFound)
			} else {
				h.latest(*version)
			}
		}
		if raw, ok := in["version"]; ok {
			var version string
			json.Unmarshal(raw, &version)
			h.paused(func(app *App) { app.SetBuild(Build{Version: version}) })
		}
		if raw, ok := in["switches"]; ok {
			var events []SwitchEvent
			json.Unmarshal(raw, &events)
			h.paused(func(app *App) {
				tx := transaction{}
				for _, e := range events {
					tx.Switches = append(tx.Switches, switchFromView(e))
				}
				if err := app.control.store.commit(ctx, tx); err != nil {
					h.t.Error(err)
				}
			})
		}
		reply(true)
	case "forgetKeys":
		if _, err := app.control.store.writer.ExecContext(ctx, `DELETE FROM request_receipts`); err != nil {
			h.t.Error(err)
		}
		reply(true)
	case "secrets":
		h.paused(func(app *App) {
			c := app.control
			source := c.state.sources[domain.SourceID(id)]
			remote, _ := source.Origin.Remote()
			reply(map[string]any{"url": c.state.secrets[remote.Location].Reveal(), "hwid": c.state.secrets[remote.HWID].Reveal()})
		})
	case "renameSlot":
		var text string
		json.Unmarshal(in["name"], &text)
		slot := slotOf(id)
		intent, err := SlotInputDTO{Name: text, Exit: slot.Exit}.admit()
		if err != nil {
			h.t.Error(err)
		}
		must(app.Execute(ctx, PutSlot{ID: slot.ID, Expected: slot.Version, Intent: intent}))
		reply(true)
	case "deleteSlot":
		must(app.Execute(ctx, DeleteSlot{ID: domain.SlotID(id)}))
		reply(true)
	case "slots":
		reply(app.Snapshot().Slots)
	case "groups":
		reply(app.Snapshot().Groups)
	case "group":
		g, ok := app.Snapshot().groupByID(domain.GroupID(id))
		if !ok {
			reply(nil)
			return
		}
		reply(g)
	case "renameGroup":
		var text string
		json.Unmarshal(in["name"], &text)
		g, _ := app.Snapshot().groupByID(domain.GroupID(id))
		members, err := g.Members.admit()
		if err != nil {
			h.t.Error(err)
		}
		must(app.Execute(ctx, PutGroup{ID: g.ID, Expected: domain.IntentRevision(g.Revision), Name: text, Members: members}))
		reply(true)
	case "endUpdate":
		h.job.says(unitGone)
		dir := app.updates.dataDir
		if a, _ := readAttempt(dir); a == nil || writeJSON(attemptOutcome(dir, a.Attempt), in) != nil {
			http.Error(rw, "no update to end", http.StatusBadRequest)
			return
		}
		reply(true)
	case "deleteGroup":
		must(app.Execute(ctx, DeleteGroup{ID: domain.GroupID(id)}))
		reply(true)
	case "deleteSource":
		must(app.Execute(ctx, DeleteSource{ID: domain.SourceID(id)}))
		reply(true)
	case "renameSource":
		var text string
		json.Unmarshal(in["name"], &text)
		source := subOf(id)
		input := SourceInput{Name: text, Manual: &ManualInput{}}
		if source.Origin.Kind == "remote" {
			input = SourceInput{Name: text, Remote: &RemoteInput{UserAgent: source.Origin.UserAgent,
				RefreshEvery: time.Duration(source.Origin.RefreshEverySeconds) * time.Second, Paused: source.Origin.Paused}}
		}
		must(app.Execute(ctx, PutSource{ID: source.ID, Expected: domain.IntentRevision(source.Revision), Input: input}))
		reply(true)
	case "changeSettings":
		settings := app.Settings()
		encoded, _ := json.Marshal(in)
		json.Unmarshal(encoded, &settings.Settings)
		if _, check := in["checkForUpdates"]; check {
			must(app.Execute(ctx, PutCheckForUpdates{On: settings.CheckForUpdates}))
		} else {
			must(app.Execute(ctx, PutSettings{Expected: settings.Revision, RetentionDays: settings.AnalyticsRetentionDays}))
		}
		reply(true)
	case "prependServer":
		h.paused(func(app *App) {
			c := app.control
			l := lineage(h.t, "l-ber", "aurora", "DE", "berlin.example")
			l.Label = "Berlin"
			if err := c.store.commit(ctx, transaction{Lineages: []domain.Lineage{l}}); err != nil {
				h.t.Error(err)
			}
			c.setWorld(c.world.with(worldChange{lineages: []domain.Lineage{l}}))
			c.change("catalogue", "", "")
			c.publish(time.Now())
		})
		reply(true)
	case "missServer":
		h.paused(func(app *App) {
			c := app.control
			l, _ := c.world.catalogue.Lookup(domain.LineageID(id))
			l.Presence, l.MissingSince = domain.Missing, time.Now()
			if err := c.store.commit(ctx, transaction{Lineages: []domain.Lineage{l}}); err != nil {
				h.t.Error(err)
			}
			c.setWorld(c.world.with(worldChange{lineages: []domain.Lineage{l}}))
			c.change("catalogue", "", "")
			c.publish(time.Now())
		})
		reply(true)
	case "reference":
		h.syncer.mu.Lock()
		h.syncer.routes[domain.SlotID(id)] = []unifi.Route{{ID: "r1", Name: "To EU", Kind: unifi.TrafficRoute, Target: "n1", Enabled: true}}
		h.syncer.mu.Unlock()
		h.paused(func(app *App) {
			app.control.owned[domain.SlotID(id)] = unifi.Owned{Routes: h.syncer.routes[domain.SlotID(id)]}
		})
		reply(true)
	case "failFetch":
		var facts struct {
			Failures    uint32 `json:"failures"`
			FailureCode string `json:"failureCode"`
			Diagnostics []struct {
				Entry int                      `json:"entry"`
				Code  catalogue.DiagnosticCode `json:"code"`
				Field string                   `json:"field"`
			} `json:"diagnostics"`
		}
		encoded, _ := json.Marshal(in)
		json.Unmarshal(encoded, &facts)
		h.paused(func(app *App) {
			c := app.control
			now, _ := json.Marshal(time.Now())
			var fs catalogue.FetchState
			fs.UnmarshalBinary(fmt.Appendf(nil, `{"checked_at":%s,"updated_at":%s,"failures":%d,"failure":%q}`, now, now, facts.Failures, facts.FailureCode))
			c.state.fetches[domain.SourceID(id)] = fs
			diff := refreshEvent{}
			for _, d := range facts.Diagnostics {
				diff.Diagnostics = append(diff.Diagnostics, catalogue.Diagnostic{Entry: d.Entry, Code: d.Code, Field: d.Field})
			}
			c.diffs[domain.SourceID(id)] = diff
			c.publish(time.Now())
		})
		reply(true)
	default:
		http.Error(rw, "no control "+name, http.StatusNotFound)
	}
}

func (v View) groupByID(id domain.GroupID) (GroupView, bool) {
	for _, g := range v.Groups {
		if g.ID == id {
			return g, true
		}
	}
	return GroupView{}, false
}
