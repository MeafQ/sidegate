package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"sidegate/deploy"
	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

func newPageDaemon(t *testing.T) (*pageDaemon, *httptest.Server) {
	t.Helper()
	h := &pageDaemon{t: t, dist: t.TempDir(), base: DefaultBasePath}
	h.reset()
	server := httptest.NewServer(h)
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
		h.mu.Lock()
		defer h.mu.Unlock()
		h.stop()
		h.app.Close()
	})
	return h, server
}

type answer struct {
	status int
	etag   string
	body   string
}

func mutate(t *testing.T, base, method, path, key string, headers map[string]string, body string) answer {
	t.Helper()
	req, err := http.NewRequest(method, base+DefaultBasePath+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(CSRFHeader, "token-1")
	req.Header.Set("Idempotency-Key", key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header.Get("ETag"), string(text)}
}

func TestThePageIsBuiltForTheBasePath(t *testing.T) {
	config, err := os.ReadFile("../../web/vite.config.ts")
	if err != nil {
		t.Fatal(err)
	}
	if want := `process.env.BASE_PATH ?? "` + DefaultBasePath + `"`; !strings.Contains(string(config), want) {
		t.Errorf("web/vite.config.ts doesn't build for %s: it lacks %s", DefaultBasePath, want)
	}
}

func TestHandlerRefusesARequestNginxDidNotVouchFor(t *testing.T) {
	h, _ := newPageDaemon(t)
	app, _ := h.current()
	for _, route := range []string{DefaultBasePath, DefaultBasePath + "api/state", DefaultBasePath + "api/session", DefaultBasePath + "api/events"} {
		rec := httptest.NewRecorder()
		app.Handler(fstest.MapFS{"index.html": {Data: []byte("page")}}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without the hook's header: %d", route, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "kind") {
			t.Errorf("%s: a refusal in front of the API must carry no kind: %s", route, rec.Body)
		}
	}
}

// A daemon run from outside releases/, as a checkout build is, names its version and no release.
func TestTheSessionNamesTheVersionAndNoReleaseOutsideReleases(t *testing.T) {
	h, server := newPageDaemon(t)
	app, _ := h.current()
	app.SetBuild(Build{Version: "dev-7990743"})
	got := read(t, server.URL, "api/session")
	var session Session
	if err := json.Unmarshal([]byte(got.body), &session); err != nil || got.status != http.StatusOK {
		t.Fatalf("api/session: %+v", got)
	}
	if session.Version != "dev-7990743" || session.Release != nil {
		t.Errorf("version %q, release %v", session.Version, session.Release)
	}
}

func TestAnAnswerSurvivesARestartWithItsWrite(t *testing.T) {
	h, server := newPageDaemon(t)
	create := map[string]string{"If-None-Match": "*"}
	body := `{"name":"Borealis","origin":{"kind":"remote","url":"https://borealis.example/source","userAgent":"Happ/3.0.0","refreshEverySeconds":3600,"paused":false}}`
	first := mutate(t, server.URL, http.MethodPut, "api/sources/b", "restart", create, body)
	if first.status != http.StatusOK {
		t.Fatalf("create: %+v", first)
	}
	refused := mutate(t, server.URL, http.MethodPut, "api/sources/b", "refused", create, body)
	if refused.status != http.StatusPreconditionFailed {
		t.Fatalf("second create: %+v", refused)
	}

	h.mu.Lock()
	h.stop()
	dir := h.app.bootstrap.DataDir
	b := h.app.bootstrap
	h.app.Close()
	app, err := Open(context.Background(), b)
	if err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	if b.DataDir != dir {
		t.Fatal("reopened elsewhere")
	}
	app.control.key.syncer = h.syncer
	h.app = app
	h.handler = app.handler(os.DirFS(h.dist), func() string { return "token-1" }, nil)
	h.stop = h.start(app)
	h.mu.Unlock()

	if again := mutate(t, server.URL, http.MethodPut, "api/sources/b", "restart", create, body); again != first {
		t.Errorf("replay after a restart: %+v, first answered %+v", again, first)
	}
	if again := mutate(t, server.URL, http.MethodPut, "api/sources/b", "refused", create, body); again != refused {
		t.Errorf("a refusal's replay after a restart: %+v, first answered %+v", again, refused)
	}
	if again := mutate(t, server.URL, http.MethodPut, "api/sources/b", "restart", map[string]string{"If-Match": `"1"`}, body); again.status != http.StatusUnprocessableEntity {
		t.Errorf("the key under another request after a restart: %+v", again)
	}
	var count int
	if err := app.Queries.state.reader.QueryRow(`SELECT COUNT(*) FROM sources WHERE id='b'`).Scan(&count); err != nil || count != 1 {
		t.Errorf("sources stored: %d, %v", count, err)
	}
}

// A receipt committed before a write's query counted toward its fingerprint: method, path and
// preconditions, then the body.
func fingerprintWithoutQuery(r *http.Request, body []byte) []byte {
	sum := sha256.New()
	for _, part := range []string{r.Method, r.URL.Path, r.Header.Get("If-Match"), r.Header.Get("If-None-Match")} {
		fmt.Fprintf(sum, "%d:%s,", len(part), part)
	}
	sum.Write(body)
	return sum.Sum(nil)
}

func TestAWriteWithNoQueryReplaysAReceiptFromBeforeTheQueryCounted(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "other", "DE", "198.51.100.40"))
	path := "api/groups/" + string(sourceGroup("other"))
	req := httptest.NewRequest(http.MethodDelete, DefaultBasePath+path, nil)
	first, err := r.c.acceptPage(context.Background(), pageWrite{Key: "delete", Fingerprint: fingerprintWithoutQuery(req, nil), Decision: runs{DeleteGroup{ID: sourceGroup("other")}}})
	if err != nil || first.Answer == nil || first.Answer.Status != http.StatusOK {
		t.Fatalf("the first delete: %+v, %v", first, err)
	}
	r.close()
	again := newRigAt(t, r.dir, r.clock.Add(time.Second))
	replay := again.send(http.MethodDelete, path, "delete", nil)
	if replay.status != first.Answer.Status || replay.body != string(first.Answer.Body) || replay.revision != strconv.FormatUint(uint64(first.Revision), 10) {
		t.Errorf("the identical retry answered %d %s at %s, want the first answer %d %s at %d",
			replay.status, replay.body, replay.revision, first.Answer.Status, first.Answer.Body, first.Revision)
	}
}

// A handler that found the key's receipt sends no decision of its own. If the 48 h trim removes the
// receipt before the controller reads it, the key is left with no answer, so a resend decides afresh.
func TestAReceiptTrimmedAfterTheHandlerFoundItLeavesTheKeyUnanswered(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "other", "DE", "198.51.100.40"))
	ctx := context.Background()
	write := pageWrite{Key: "delete", Fingerprint: []byte("delete"), Decision: runs{DeleteGroup{ID: sourceGroup("other")}}}
	if first, err := r.c.acceptPage(ctx, write); err != nil || first.Answer == nil || first.Answer.Status != http.StatusOK {
		t.Fatalf("the first delete: %+v, %v", first, err)
	}
	if _, _, found, err := r.app.Queries.state.receipt(ctx, write.Key); !found || err != nil {
		t.Fatalf("the handler found no receipt: %v", err)
	}
	if _, err := r.c.store.writer.ExecContext(ctx, `UPDATE request_receipts SET at=at-?`, (49 * time.Hour).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if err := r.c.store.trimEvents(ctx, r.clock); err != nil {
		t.Fatal(err)
	}

	replayed, err := r.c.acceptPage(ctx, pageWrite{Key: write.Key, Fingerprint: write.Fingerprint, Decision: replays{}})
	if err == nil && replayed.Answer != nil && replayed.Answer.Status == http.StatusOK {
		t.Errorf("a receipt the trim removed was replayed: %+v", replayed)
	}
	if stored, _, found, _ := r.c.store.receipt(ctx, write.Key); found {
		t.Errorf("the key now answers %d %s for good, which nothing decided", stored.Answer.Status, stored.Answer.Body)
	}
}

func TestRequestsUnderOneKeyCommitOnce(t *testing.T) {
	h, server := newPageDaemon(t)
	var wg sync.WaitGroup
	answers := make([]answer, 16)
	body := `{"name":"Asia","exit":{"kind":"group","group":"all-aurora"}}`
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = mutate(t, server.URL, http.MethodPut, "api/slots/asia", "same", map[string]string{"If-None-Match": "*"}, body)
		}()
	}
	wg.Wait()
	for _, a := range answers {
		if a.status != http.StatusOK {
			t.Errorf("a request under the shared key answered %+v", a)
		}
	}
	app, _ := h.current()
	for _, s := range app.Snapshot().Slots {
		if s.ID == "asia" && s.Version != 1 {
			t.Errorf("version %d: more than one commit", s.Version)
		}
	}
}

func TestAProbeUnderOneKeyAnswersOnce(t *testing.T) {
	_, server := newPageDaemon(t)
	// A server has no probe route until the plan it's in has been applied.
	for i := 0; mutate(t, server.URL, http.MethodPost, "api/servers/l-ams/probe", "ready-"+strconv.Itoa(i), nil, "").status != http.StatusOK; i++ {
		if i == 100 {
			t.Fatal("the server never got a probe route")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var wg sync.WaitGroup
	answers := make([]answer, 8)
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = mutate(t, server.URL, http.MethodPost, "api/servers/l-ams/probe", "probe-once", nil, "")
		}()
	}
	wg.Wait()
	for _, a := range answers {
		if a != answers[0] || a.status != http.StatusOK {
			t.Errorf("answers under one key differ: %+v, %+v", a, answers[0])
		}
	}
}

func TestNoSecretReachesAnAnswerOrALog(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	h, server := newPageDaemon(t)
	const token, device = "tok3n-in-the-url", "device-s3cret"
	secretURL := "https://user:pa55@provider.example/source/" + token
	create := map[string]string{"If-None-Match": "*"}
	input := func(url string) string {
		encoded, _ := json.Marshal(map[string]any{"name": "P", "origin": map[string]any{"kind": "remote", "url": url, "userAgent": "", "hwid": device, "refreshEverySeconds": 3600, "paused": false}})
		return string(encoded)
	}
	var seen []string
	seen = append(seen, mutate(t, server.URL, http.MethodPut, "api/sources/p", "k1", create, input(secretURL)).body)
	seen = append(seen, mutate(t, server.URL, http.MethodPut, "api/sources/p", "k2", create, input(secretURL)).body)
	seen = append(seen, mutate(t, server.URL, http.MethodPut, "api/sources/q", "k3", create, input("ftp://"+token)).body)
	seen = append(seen, mutate(t, server.URL, http.MethodPut, "api/sources/q", "k4", create, input("::"+token)).body)
	for _, route := range []string{"api/state", "api/servers", "api/session"} {
		resp, err := http.Get(server.URL + DefaultBasePath + route)
		if err != nil {
			t.Fatal(err)
		}
		text, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		seen = append(seen, string(text))
	}
	app, _ := h.current()
	rows, err := app.Queries.state.reader.Query(`SELECT COALESCE(body,'') FROM request_receipts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, body)
	}
	if len(seen) < 8 {
		t.Fatalf("only %d answers read", len(seen))
	}
	keys, _ := json.Marshal(app.Snapshot().Slots)
	seen = append(seen, string(keys), logs.String())
	for _, text := range seen {
		for _, secret := range []string{token, device, "pa55"} {
			if strings.Contains(text, secret) {
				t.Errorf("%q appears in %s", secret, text)
			}
		}
		if strings.Contains(text, "x_wireguard") || strings.Contains(text, "PrivateKey") {
			t.Errorf("a private key field appears in %s", text)
		}
	}
}

// slowClient never reads, and must not hold up a commit.
func TestAClientThatFallsBehindIsClosedNotWaitedFor(t *testing.T) {
	var feed eventFeed
	client := feed.join()
	done := make(chan struct{})
	go func() {
		for range feedQueue + 4 {
			feed.emit(ChangeEvent{Kind: "slot"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked on a slow client")
	}
	count := 0
	for range client {
		count++
	}
	if count != feedQueue {
		t.Errorf("read %d queued events before the close, want %d", count, feedQueue)
	}
	if feed.size() != 0 {
		t.Error("the closed client is still joined")
	}
}

// recordingSyncer plays UniFi for the wiring tests. It gives each slot a client on the
// first pass, keeps what a pass was asked, and can be told to fail.
type recordingSyncer struct {
	mu        sync.Mutex
	passes    [][]unifi.Want
	fail      error
	routes    map[domain.SlotID][]unifi.Route
	foreign   []unifi.ForeignClient
	problem   *unifi.Problem
	retained  []unifi.Retained
	inspected int
}

func (s *recordingSyncer) Reconcile(_ context.Context, wants []unifi.Want) (unifi.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.passes = append(s.passes, wants)
	if s.fail != nil {
		return unifi.Report{}, s.fail
	}
	report := unifi.Report{Inventory: unifi.Inventory{Foreign: s.foreign}, Retained: s.retained}
	for _, w := range wants {
		id := domain.NetworkID("net-" + string(w.Slot))
		report.Slots = append(report.Slots, unifi.SlotResult{Slot: w.Slot, NetworkID: id, Problem: s.problem})
		report.Inventory.Owned = append(report.Inventory.Owned, unifi.Owned{NetworkID: id, Slot: w.Slot, Name: w.Name, Routes: s.routes[w.Slot]})
	}
	for slot, routes := range s.routes {
		if !slices.ContainsFunc(wants, func(w unifi.Want) bool { return w.Slot == slot }) {
			report.Inventory.Owned = append(report.Inventory.Owned, unifi.Owned{NetworkID: domain.NetworkID("net-" + string(slot)), Slot: slot, Name: "Kept " + string(slot), Routes: routes})
		}
	}
	return report, nil
}

func (s *recordingSyncer) Inspect(context.Context) (unifi.Inventory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inspected++
	return unifi.Inventory{Foreign: s.foreign}, s.fail
}

// clientRig is a controller wired to a real runClients, stepped by hand.
func clientRig(t *testing.T) (*rig, *recordingSyncer, func()) {
	t.Helper()
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "s", "DE", "a.example"))
	s := &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}}
	r.app.control.key.syncer = s
	r.app.bootstrap.ClientDNS = "1.1.1.1"
	wants := make(chan []unifi.Want, 1)
	out := make(chan clientReport, 1)
	r.c.wants, r.c.clientReports = wants, out
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go runClients(ctx, s, nil, wants, out, time.After)
	<-out // Its first read, before anything was wanted.
	pass := func() {
		t.Helper()
		r.settle()
		select {
		case report := <-out:
			if err := r.c.applyClients(ctx, report); err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no pass over UniFi")
		}
	}
	return r, s, pass
}

func poolIntent(t *testing.T, name string) domain.SlotIntent {
	t.Helper()
	return exitIntent(t, name, domain.GroupExit(sourceGroup("s")))
}

func TestASlotsVPNClientFollowsTheSlot(t *testing.T) {
	r, s, pass := clientRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	plan := r.nextPlan()
	for _, target := range plan.Slots() {
		if _, down := target.Selection.Down(); !down {
			t.Error("a slot with no durable client must keep its gate closed")
		}
	}
	pass()
	first := s.lastPass(t)
	if len(first) != 1 || first[0].Name != "EU" || first[0].Bound != "" {
		t.Fatalf("first want: %+v", first)
	}
	want := first[0]
	if want.Address.String() != "10.70.1.2/30" || want.DNS.String() != "1.1.1.1" || want.Endpoint.String() != "127.0.0.1:51831" {
		t.Errorf("the client is %s, dns %s, endpoint %s", want.Address, want.DNS, want.Endpoint)
	}
	row := r.c.state.slots["eu"]
	if want.PeerPublicKey != row.Slot.Allocation.Keys.HostPublic || want.PublicKey != row.Slot.Allocation.Keys.ClientPublic ||
		want.PrivateKey.Reveal() != r.c.state.secrets[row.Slot.Allocation.Keys.ClientPrivate].Reveal() {
		t.Error("the client's keys are not the slot's")
	}
	if row.Slot.Allocation.NetworkID != "net-eu" {
		t.Fatalf("the _id wasn't committed: %q", row.Slot.Allocation.NetworkID)
	}
	st, _, err := r.c.store.recover(context.Background(), r.clock, defaultTimings())
	if err != nil || st.slots["eu"].Slot.Allocation.NetworkID != "net-eu" {
		t.Errorf("the _id isn't durable: %q, %v", st.slots["eu"].Slot.Allocation.NetworkID, err)
	}
	if id := r.app.Snapshot().Slots[0].Runtime.NetworkID; id == nil || *id != "net-eu" {
		t.Errorf("the page doesn't show the client: %v", id)
	}

	version := r.c.state.slots["eu"].Slot.Version
	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: version, Intent: poolIntent(t, "Europe")}); err != nil {
		t.Fatal(err)
	}
	pass()
	if renamed := s.lastPass(t); len(renamed) != 1 || renamed[0].Name != "Europe" || renamed[0].Bound != "net-eu" {
		t.Errorf("a rename: %+v", renamed)
	}

	if _, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"}); err != nil {
		t.Fatal(err)
	}
	pass()
	if gone := s.lastPass(t); len(gone) != 0 {
		t.Errorf("a deleted slot still wants a client: %+v", gone)
	}
}

func TestADeleteThatRoutesPointAtIsRefused(t *testing.T) {
	r, s, pass := clientRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	s.mu.Lock()
	s.routes["eu"] = []unifi.Route{{ID: "r", Name: "To EU", Kind: unifi.TrafficRoute, Enabled: true}}
	s.mu.Unlock()
	pass()
	_, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"})
	var refusal *CommandError
	if !asCommand(err, &refusal) || refusal.Kind != KindReferenced || refusal.Reason != RefusedRouted {
		t.Errorf("delete: %v", err)
	}
}

// refusedRouted says a delete of eu answers 409 routed, and leaves the slot live.
func refusedRouted(t *testing.T, r *rig) {
	t.Helper()
	_, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"})
	var refusal *CommandError
	if !asCommand(err, &refusal) || refusal.Kind != KindReferenced || refusal.Reason != RefusedRouted {
		t.Fatalf("delete: %v", err)
	}
	if r.c.state.slots["eu"].Deleting {
		t.Fatal("a refused delete took the slot down")
	}
}

func deletes(t *testing.T, r *rig) {
	t.Helper()
	if _, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !r.c.state.slots["eu"].Deleting {
		t.Fatal("a delete that landed left the slot live")
	}
}

// passFound applies a pass from syncer that found inv, as the controller does with the current key's.
func passFound(t *testing.T, r *rig, syncer unifi.Syncer, inv unifi.Inventory) {
	t.Helper()
	if err := r.c.applyClients(context.Background(), clientReport{From: syncer, Report: unifi.Report{Inventory: inv}}); err != nil {
		t.Fatal(err)
	}
}

func toEU() []unifi.Route {
	return []unifi.Route{{ID: "r", Name: "To EU", Kind: unifi.TrafficRoute, Enabled: true}}
}

func TestARestartKeepsUniFisLastReadOfABoundSlotsRoutes(t *testing.T) {
	r, s, pass := clientRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	s.mu.Lock()
	s.routes["eu"] = toEU()
	s.mu.Unlock()
	pass()
	r.close()
	again := newRig(t, r.dir)
	again.c.key.syncer = s
	refusedRouted(t, again)
	next := &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}}
	if _, err := again.c.accept(context.Background(), PutUniFiKey{Key: domain.NewSecret("k2"), Syncer: next}); err != nil {
		t.Fatal(err)
	}
	refusedRouted(t, again)
	passFound(t, again, next, unifi.Inventory{Owned: []unifi.Owned{{NetworkID: "net-eu", Slot: "eu", Name: "EU"}}})
	deletes(t, again)
}

func TestAnUpgradedBoundSlotCountsAsRoutedUntilItsClientIsRead(t *testing.T) {
	r, s, pass := clientRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	pass()
	ctx := context.Background()
	var stored []byte
	if err := r.c.store.writer.QueryRowContext(ctx, `SELECT allocation FROM slots WHERE id = 'eu'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(stored, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "unrouted")
	older, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.store.writer.ExecContext(ctx, `UPDATE slots SET allocation = ? WHERE id = 'eu'`, older); err != nil {
		t.Fatal(err)
	}
	r.close()
	again := newRig(t, r.dir)
	again.c.key.syncer = s
	refusedRouted(t, again)
	passFound(t, again, s, unifi.Inventory{Owned: []unifi.Owned{{NetworkID: "net-eu", Slot: "eu", Name: "EU"}}})
	deletes(t, again)
}

func TestABoundSlotWhoseClientUniFiNoLongerHoldsDeletes(t *testing.T) {
	r, s, pass := clientRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	s.mu.Lock()
	s.routes["eu"] = toEU()
	s.mu.Unlock()
	pass()
	refusedRouted(t, r)
	passFound(t, r, s, unifi.Inventory{Foreign: []unifi.ForeignClient{{ID: "f1", Name: "Office", Enabled: true}}})
	deletes(t, r)
}

func TestAManualStartDeletesABindingFromAnEarlierRun(t *testing.T) {
	r, s, pass := clientRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	s.mu.Lock()
	s.routes["eu"] = toEU()
	s.mu.Unlock()
	pass()
	r.close()
	deletes(t, newRig(t, r.dir))
}

// networkAppRig is a controller whose passes run the real client against fakeNetworkApp, one by
// hand each time pass is called, with the wants the controller last sent.
func networkAppRig(t *testing.T) (*rig, *fakeNetworkApp, func()) {
	t.Helper()
	app := &fakeNetworkApp{}
	server := httptest.NewTLSServer(app)
	t.Cleanup(server.Close)
	client, err := unifi.New(unifi.Config{BaseURL: server.URL, Site: "default", APIKey: domain.NewSecret("key"), Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "s", "DE", "a.example"))
	r.app.control.key.syncer = client
	r.app.bootstrap.ClientDNS = "1.1.1.1"
	var wants []unifi.Want
	pass := func() {
		t.Helper()
		r.settle()
		select {
		case wants = <-r.c.wants:
		default:
		}
		report, err := client.Reconcile(context.Background(), wants)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.c.applyClients(context.Background(), clientReport{From: client, Report: report}); err != nil {
			t.Fatal(err)
		}
	}
	return r, app, pass
}

// The owner takes the slot's client over in UniFi, clearing its marker and giving it another peer key.
// A pass then lists it as the owner's, and its routes still say whether the slot can go.
func TestAClientTheOwnerTookOverDeletesOnceItsRoutesAreGone(t *testing.T) {
	r, app, pass := networkAppRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	pass()
	id := r.c.state.slots["eu"].Slot.Allocation.NetworkID
	if id == "" {
		t.Fatal("the pass bound no client")
	}
	app.mu.Lock()
	app.traffic = []map[string]any{{"_id": "r1", "description": "To EU", "enabled": true, "network_id": string(id)}}
	app.mu.Unlock()
	pass()
	refusedRouted(t, r)
	app.mu.Lock()
	app.clients[0]["wireguard_client_configuration_filename"] = ""
	app.clients[0]["wireguard_client_peer_public_key"] = "the owner's"
	app.mu.Unlock()
	pass()
	if got := r.slotViewOf("eu").Unifi.Client; got != "deleted" {
		t.Fatalf("a client the owner took over reads %q", got)
	}
	refusedRouted(t, r)
	app.mu.Lock()
	app.traffic = nil
	app.mu.Unlock()
	pass()
	deletes(t, r)
}

func asCommand(err error, into **CommandError) bool {
	e, ok := err.(*CommandError)
	if ok {
		*into = e
	}
	return ok
}

func slotTrouble(t *testing.T, view View, id domain.SlotID) *ClientTrouble {
	t.Helper()
	for _, s := range view.Slots {
		if s.ID == id {
			return s.Unifi.Trouble
		}
	}
	t.Fatalf("no slot %s", id)
	return nil
}

func TestUniFiProblemsReachThePageAsTheirSlotsTroubles(t *testing.T) {
	r, s, pass := clientRig(t)
	s.mu.Lock()
	s.problem = &unifi.Problem{Kind: unifi.NameCollision, Detail: `another network is already named "EU"`, Err: &unifi.Error{Kind: unifi.Rejected, Op: "secret-op", Reason: "internal detail"}}
	s.foreign = []unifi.ForeignClient{{ID: "f1", Name: "One"}, {ID: "f2", Name: "Two"}}
	s.mu.Unlock()
	r.putSlot("eu", poolIntent(t, "EU"))
	pass()
	view := r.app.Snapshot()
	if len(view.VPNClients.Foreign) != 2 {
		t.Errorf("foreign clients: %+v", view.VPNClients)
	}
	if got := slotTrouble(t, view, "eu"); got == nil || *got != clientNameTaken {
		t.Errorf("a taken name: trouble %v", got)
	}
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(body); strings.Contains(text, "already named") || strings.Contains(text, "secret-op") || strings.Contains(text, "internal detail") {
		t.Errorf("the view carries a problem's words: %s", text)
	}
	s.mu.Lock()
	s.problem = nil
	s.fail = &unifi.Error{Kind: unifi.Unauthorized, Op: "list networks"}
	s.mu.Unlock()
	version := r.c.state.slots["eu"].Slot.Version
	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: version, Intent: poolIntent(t, "Other")}); err != nil {
		t.Fatal(err)
	}
	pass()
	if got := r.app.Snapshot(); got.UniFiTrouble == nil || *got.UniFiTrouble != unifiKeyRefused || slotTrouble(t, got, "eu") != nil {
		t.Errorf("after a refused key: trouble %v, the slot's %v", got.UniFiTrouble, slotTrouble(t, got, "eu"))
	}
	if r.c.state.slots["eu"].Slot.Allocation.NetworkID != "net-eu" {
		t.Error("an unavailable app must change nothing known")
	}
	s.mu.Lock()
	s.fail = nil
	s.mu.Unlock()
	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: r.c.state.slots["eu"].Slot.Version, Intent: poolIntent(t, "Third")}); err != nil {
		t.Fatal(err)
	}
	pass()
	if got := r.app.Snapshot().UniFiTrouble; got != nil {
		t.Errorf("after a pass that worked: trouble %v", *got)
	}
}

func TestEachClientProblemIsItsSlotsTroubleUntilAPassWorks(t *testing.T) {
	r, s, pass := clientRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	for _, c := range []struct {
		kind unifi.ProblemKind
		want *ClientTrouble
	}{
		{unifi.NameCollision, new(clientNameTaken)},
		{unifi.LimitReached, new(clientLimitReached)},
		{unifi.RefusedByApp, new(clientRefused)},
		{unifi.ExternallyDeleted, nil},
		{0, nil},
	} {
		s.mu.Lock()
		s.problem = nil
		if c.kind != 0 {
			s.problem = &unifi.Problem{Kind: c.kind, Detail: "detail"}
		}
		s.mu.Unlock()
		version := r.c.state.slots["eu"].Slot.Version
		if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: version, Intent: poolIntent(t, "EU "+strconv.Itoa(int(c.kind)))}); err != nil {
			t.Fatal(err)
		}
		pass()
		if got := slotTrouble(t, r.app.Snapshot(), "eu"); (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("problem %d: trouble %v, want %v", c.kind, got, c.want)
		}
	}
}

func TestAClientKeptForItsRoutesSaysSoOnItsRow(t *testing.T) {
	r, s, pass := clientRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	s.mu.Lock()
	s.routes["gone"] = []unifi.Route{{ID: "r", Name: "To Gone", Kind: unifi.TrafficRoute, Enabled: true}}
	s.retained = []unifi.Retained{{NetworkID: "net-gone", Slot: "gone", Name: "Kept gone", Problem: &unifi.Problem{Kind: unifi.RoutedElsewhere, Detail: "1 Policy Table routes still target it"}}}
	s.mu.Unlock()
	version := r.c.state.slots["eu"].Slot.Version
	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: version, Intent: poolIntent(t, "EU again")}); err != nil {
		t.Fatal(err)
	}
	pass()
	view := r.app.Snapshot()
	i := slices.IndexFunc(view.VPNClients.Foreign, func(f ForeignClientDTO) bool { return f.ID == "net-gone" })
	if i < 0 || view.VPNClients.Foreign[i].Trouble == nil || *view.VPNClients.Foreign[i].Trouble != clientRouted {
		t.Fatalf("the kept client's row: %+v", view.VPNClients.Foreign)
	}
	if got := slotTrouble(t, view, "eu"); got != nil {
		t.Errorf("a trouble of a kept client reached a live slot: %v", *got)
	}
}

func TestNoAPIKeyLeavesUniFiAloneAndSaysSo(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "s", "DE", "a.example"))
	r.putSlot("eu", poolIntent(t, "EU"))
	r.settle()
	if got := r.app.Snapshot(); got.UniFiTrouble == nil || *got.UniFiTrouble != unifiNoKey || slotTrouble(t, got, "eu") != nil {
		t.Errorf("trouble %v, the slot's %v", got.UniFiTrouble, slotTrouble(t, got, "eu"))
	}
}

func TestCapacityCountsTheOwnersClients(t *testing.T) {
	r, s, pass := clientRig(t)
	s.mu.Lock()
	for i := range unifi.Limit - 1 {
		s.foreign = append(s.foreign, unifi.ForeignClient{ID: domain.NetworkID(strconv.Itoa(i))})
	}
	s.mu.Unlock()
	r.putSlot("eu", poolIntent(t, "EU"))
	pass()
	_, err := r.c.accept(context.Background(), PutSlot{ID: "two", Intent: poolIntent(t, "Two")})
	var refusal *CommandError
	if !asCommand(err, &refusal) || refusal.Kind != KindCapacity || refusal.Reason != RefusedClientsFull {
		t.Errorf("a ninth client: %v", err)
	}
}

func TestACreateOverADeleteInProgressKeepsTheAllocation(t *testing.T) {
	r, _, pass := clientRig(t)
	r.putSlot("eu", poolIntent(t, "EU"))
	pass()
	before := r.c.state.slots["eu"].Slot
	if _, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: before.Version, Intent: poolIntent(t, "EU")}); err == nil {
		t.Error("a replace over a delete in progress must find the slot gone")
	}
	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Intent: poolIntent(t, "EU again")}); err != nil {
		t.Fatal(err)
	}
	after := r.c.state.slots["eu"]
	if after.Deleting || after.Slot.Version != before.Version+1 || after.Slot.Allocation.Ordinal != before.Allocation.Ordinal || after.Slot.Allocation.NetworkID != "" {
		t.Errorf("revived: %+v", after)
	}
}

func TestTheSocketIsRootsAlone(t *testing.T) {
	h, _ := newPageDaemon(t)
	app, _ := h.current()
	dir, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "http", "s.sock")
	app.bootstrap.ListenSocket = socket
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.serveHTTP(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the socket never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("socket mode %o", mode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveHTTP: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("serveHTTP didn't stop")
	}
}

func (s *recordingSyncer) lastPass(t *testing.T) []unifi.Want {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.passes) == 0 {
		t.Fatal("no pass yet")
	}
	return s.passes[len(s.passes)-1]
}

func (r *rig) nextPlan() gateway.Plan {
	r.t.Helper()
	r.settle()
	select {
	case plan := <-r.c.goals:
		return plan
	default:
		r.t.Fatal("no plan")
		return gateway.Plan{}
	}
}

func serveOnSocket(t *testing.T, app *App) (*http.Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	app.bootstrap.ListenSocket = socket
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.serveHTTP(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serveHTTP: %v", err)
		}
	})
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			conn.Close()
			return &http.Client{Transport: transport, Timeout: 5 * time.Second}, socket
		}
		if time.Now().After(deadline) {
			t.Fatal("the socket never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAProcessOnTheSocketCannotForgeTheLogin(t *testing.T) {
	h, _ := newPageDaemon(t)
	app, _ := h.current()
	const proof = harnessProof
	client, _ := serveOnSocket(t, app)

	get := func(auth, sentProof string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://daemon"+DefaultBasePath+"api/session", nil)
		if auth != "" {
			req.Header.Set(AuthHeader, auth)
		}
		if sentProof != "" {
			req.Header.Set(ProofHeader, sentProof)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		name, auth, proof string
		want              int
	}{
		{"a forged login and no proof", "200", "", http.StatusUnauthorized},
		{"a forged login and a wrong proof", "200", "guess", http.StatusUnauthorized},
		{"a forged login and a proof one byte off", "200", proof[:len(proof)-1] + "X", http.StatusUnauthorized},
		{"the proof with no auth subrequest", "", proof, http.StatusUnauthorized},
		{"the hook's two headers", "204", proof, http.StatusOK},
	} {
		if got := get(c.auth, c.proof); got != c.want {
			t.Errorf("%s: HTTP %d, want %d", c.name, got, c.want)
		}
	}

	req, _ := http.NewRequest(http.MethodDelete, "http://daemon"+DefaultBasePath+"api/sources/aurora", nil)
	req.Header.Set(AuthHeader, "anything")
	req.Header.Set(CSRFHeader, "token-1")
	req.Header.Set("Idempotency-Key", "forged-delete")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a forged delete: HTTP %d", resp.StatusCode)
	}
}

func TestWithNoProofTheSocketStaysRootsAndAnswersNobody(t *testing.T) {
	h, _ := newPageDaemon(t)
	app, _ := h.current()
	app.proof = nil
	// Binding a group needs root, so a build that tried would fail here on a developer's Mac.
	app.bootstrap.ListenGroup = "nobody"
	client, socket := serveOnSocket(t, app)

	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("socket mode %o with no proof", mode)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://daemon"+DefaultBasePath+"api/session", nil)
	req.Header.Set(AuthHeader, "200")
	req.Header.Set(ProofHeader, "")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no proof configured and an empty proof sent: HTTP %d", resp.StatusCode)
	}
}

func TestLoadProofRefusesAFileOthersCanRead(t *testing.T) {
	dir := t.TempDir()
	if proof := loadProof(dir); proof != nil {
		t.Errorf("no file gave a proof: %q", proof)
	}
	path := ProofPath(dir)
	if err := os.WriteFile(path, []byte("secret\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if proof := loadProof(dir); proof != nil {
		t.Errorf("a group-readable file gave a proof: %q", proof)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if proof := loadProof(dir); string(proof) != "secret" {
		t.Errorf("proof %q", proof)
	}
}

// nginx refuses a body over the hook's limit before Go sees it, so the hook must pass the
// largest body Go takes: a pasted source.
func TestTheHookPassesThePasteGoTakes(t *testing.T) {
	match := regexp.MustCompile(`client_max_body_size (\d+)([km]?);`).FindStringSubmatch(deploy.NginxHook)
	if match == nil {
		t.Fatal("the hook sets no client_max_body_size, and nginx's own 1 MB default isn't written down")
	}
	size, _ := strconv.Atoi(match[1])
	size <<= map[string]int{"": 0, "k": 10, "m": 20}[match[2]]
	if size != maxSourceBody {
		t.Errorf("the hook passes %d bytes and Go takes a pasted source of %d", size, maxSourceBody)
	}

	_, server := newPageDaemon(t)
	encoded, _ := json.Marshal(map[string]any{"name": "Big", "origin": map[string]any{"kind": "manual", "text": strings.Repeat("not a server\n", 900<<10/13)}})
	got := mutate(t, server.URL, http.MethodPut, "api/sources/big", "k1", map[string]string{"If-None-Match": "*"}, string(encoded))
	if got.status != http.StatusUnprocessableEntity {
		t.Errorf("a 900 KiB paste of no servers answered %d, want 422 from its parse: %s", got.status, got.body)
	}
}

func TestAPlainHTTPSourceIsRefusedInWordsThePageShows(t *testing.T) {
	_, server := newPageDaemon(t)
	encoded, _ := json.Marshal(map[string]any{"name": "P", "origin": map[string]any{"kind": "remote", "url": "http://provider.example/source/token", "userAgent": "", "refreshEverySeconds": 3600, "paused": false}})
	got := mutate(t, server.URL, http.MethodPut, "api/sources/p", "k1", map[string]string{"If-None-Match": "*"}, string(encoded))
	var refusal errorBody
	json.Unmarshal([]byte(got.body), &refusal)
	if got.status != http.StatusUnprocessableEntity || refusal.Kind != "validation" || refusal.Reason != RefusedNotHTTPS {
		t.Errorf("an http:// source answered %d %+v", got.status, refusal)
	}
}
