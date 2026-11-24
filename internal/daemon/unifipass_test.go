package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sidegate/internal/domain"
	"sidegate/internal/unifi"
)

// passClock is the clock runClients waits on in a test: it says on armed how long each wait runClients
// starts is, and a send on fire ends the one it is in.
type passClock struct {
	armed chan time.Duration
	fire  chan time.Time
}

func (c *passClock) after(d time.Duration) <-chan time.Time {
	c.armed <- d
	return c.fire
}

// passRig is a controller wired to a real runClients that waits on pace and takes a new key's syncer
// from syncers.
type passRig struct {
	*rig
	pace    *passClock
	syncers chan unifi.Syncer
	out     chan clientReport
	ctx     context.Context
}

func newPassRig(t *testing.T, syncer unifi.Syncer) *passRig {
	t.Helper()
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "s", "DE", "a.example"))
	r.app.control.key.syncer = syncer
	r.app.bootstrap.ClientDNS = "1.1.1.1"
	r.c.publish(r.clock)
	wants := make(chan []unifi.Want, 1)
	p := &passRig{rig: r, pace: &passClock{armed: make(chan time.Duration, 8), fire: make(chan time.Time)}, syncers: make(chan unifi.Syncer, 1), out: make(chan clientReport, 1)}
	r.c.wants, r.c.clientReports = wants, p.out
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.ctx = ctx
	go runClients(ctx, syncer, p.syncers, wants, p.out, p.pace.after)
	return p
}

// next waits for runClients' pass to end, which it does by arming its next wait, applies the report it
// sent, if any, as the controller does, and says whether it sent one and how long that wait is.
func (p *passRig) next() (reported bool, wait time.Duration) {
	p.t.Helper()
	wait = <-p.pace.armed
	select {
	case report := <-p.out:
		if report.From == p.c.key.syncer {
			if err := p.c.applyClients(p.ctx, report); err != nil {
				p.t.Fatal(err)
			}
		}
		return true, wait
	default:
		return false, wait
	}
}

// wake ends the wait runClients is in, as its timer would.
func (p *passRig) wake() { p.pace.fire <- time.Time{} }

func (p *passRig) expect(what string, reported bool, wait time.Duration, trouble *UniFiTrouble) {
	p.t.Helper()
	gotReported, gotWait := p.next()
	got, want := troubleName(p.app.Snapshot().UniFiTrouble), troubleName(trouble)
	if gotReported != reported || gotWait != wait || got != want {
		p.t.Errorf("%s: reported %t, next pass in %s, trouble %s; want reported %t, next in %s, trouble %s", what, gotReported, gotWait, got, reported, wait, want)
	}
}

func troubleName(trouble *UniFiTrouble) UniFiTrouble {
	if trouble == nil {
		return "none"
	}
	return *trouble
}

func (s *recordingSyncer) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

func (s *recordingSyncer) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inspected + len(s.passes)
}

var (
	timedOut   = &unifi.Error{Kind: unifi.Unavailable, Op: "list networks", Reason: "timed out"}
	refusedKey = &unifi.Error{Kind: unifi.Unauthorized, Op: "list networks", Status: 401}
)

// busyUniFi is a UniFi that keeps the VPN Clients it is sent and doesn't answer the first read after
// its first create, as UniFi did on the gateway while busy with the client the daemon had just made.
type busyUniFi struct {
	*httptest.Server
	t        *testing.T
	mu       sync.Mutex
	networks []map[string]any
	busy     bool // The next read is the first after the first create.
	timedOut bool
}

func newBusyUniFi(t *testing.T) *busyUniFi {
	t.Helper()
	b := &busyUniFi{t: t}
	b.Server = httptest.NewTLSServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.Close)
	return b
}

func (b *busyUniFi) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	listed := func(docs []map[string]any) {
		json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"rc": "ok"}, "data": append([]map[string]any{}, docs...)})
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/s/default/rest/networkconf":
		var doc map[string]any
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		doc["_id"] = fmt.Sprintf("net-%d", len(b.networks)+1)
		b.networks = append(b.networks, doc)
		b.busy = !b.timedOut
		listed([]map[string]any{doc})
	case r.Method != http.MethodGet:
		b.t.Errorf("UniFi was sent %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	case b.busy:
		b.busy, b.timedOut = false, true
		w.WriteHeader(http.StatusGatewayTimeout)
	case r.URL.Path == "/api/s/default/rest/networkconf":
		listed(b.networks)
	case r.URL.Path == "/api/s/default/rest/routing":
		listed(nil)
	case strings.HasPrefix(r.URL.Path, "/v2/api/site/default/"):
		io.WriteString(w, "[]")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// made is the _id of each client UniFi keeps, and whether a read after a create went unanswered.
func (b *busyUniFi) made() (ids []domain.NetworkID, timedOut bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range b.networks {
		ids = append(ids, domain.NetworkID(n["_id"].(string)))
	}
	return ids, b.timedOut
}

// One pass that times out after a create is UniFi busy with the daemon's own write, not UniFi gone: it
// shows nothing and runs again within seconds, and that pass binds the client the first one made.
func TestOneSilentPassAfterACreateShowsNothing(t *testing.T) {
	busy := newBusyUniFi(t)
	syncer, err := unifi.New(unifi.Config{BaseURL: busy.URL, Site: "default", APIKey: domain.NewSecret("the-key")})
	if err != nil {
		t.Fatal(err)
	}
	p := newPassRig(t, syncer)
	p.expect("the start's read", true, clientPassEvery, nil)
	p.putSlot("eu", poolIntent(t, "EU"))
	p.settle()
	p.expect("the pass that made the client and timed out", false, clientRetryAfter, nil)
	made, timedOut := busy.made()
	if len(made) != 1 || !timedOut {
		t.Fatalf("the pass made clients %v and timed out: %t; want one client and a timeout", made, timedOut)
	}
	p.wake()
	p.expect("its retry", true, clientPassEvery, nil)
	if id := p.c.state.slots["eu"].Slot.Allocation.NetworkID; id != made[0] {
		t.Errorf("the retry bound client %q, want %q", id, made[0])
	}
	if again, _ := busy.made(); len(again) != 1 {
		t.Errorf("UniFi holds clients %v after the retry, want the one the first pass made", again)
	}
}

// A UniFi that stays silent from the start is "Not answering" from its second pass, and the passes
// then go back to the minute. A pass that works ends it.
func TestUniFisSilenceShowsFromTheSecondPassInARow(t *testing.T) {
	s := &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}, fail: timedOut}
	p := newPassRig(t, s)
	p.expect("the start's read", false, clientRetryAfter, nil)
	if p.app.Snapshot().VPNClients.Known {
		t.Error("a read UniFi didn't answer made the board known")
	}
	p.wake()
	p.expect("the second silent pass", true, clientPassEvery, new(unifiNotAnswering))
	p.wake()
	p.expect("the third", true, clientPassEvery, new(unifiNotAnswering))
	s.failWith(nil)
	p.wake()
	p.expect("a pass UniFi answered", true, clientPassEvery, nil)
	if !p.app.Snapshot().VPNClients.Known {
		t.Error("the answered pass left the board unknown")
	}
	if asked := s.asked(); asked != 4 {
		t.Errorf("UniFi was asked %d times in four passes", asked)
	}
}

// A refused key is shown at once, and a silent pass between two refusals leaves it shown.
func TestARefusedKeyStaysShownThroughOneSilentPass(t *testing.T) {
	s := &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}, fail: refusedKey}
	p := newPassRig(t, s)
	p.expect("a refusal", true, clientPassEvery, new(unifiKeyRefused))
	s.failWith(timedOut)
	p.wake()
	p.expect("a silent pass after it", false, clientRetryAfter, new(unifiKeyRefused))
	s.failWith(refusedKey)
	p.wake()
	p.expect("the next refusal", true, clientPassEvery, new(unifiKeyRefused))
	s.failWith(timedOut)
	p.wake()
	p.expect("a silent pass after that", false, clientRetryAfter, new(unifiKeyRefused))
	p.wake()
	p.expect("a second silent pass", true, clientPassEvery, new(unifiNotAnswering))
}

// A key saved while a silent pass waits for its retry passes at once, and its own first silent pass is
// one in a row: nothing of the old key's silence is shown.
func TestAKeySavedDuringARetryStartsItsOwnCount(t *testing.T) {
	old := &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}}
	p := newPassRig(t, old)
	p.expect("the old key's read", true, clientPassEvery, nil)
	old.failWith(timedOut)
	p.wake()
	p.expect("the old key's silent pass", false, clientRetryAfter, nil)
	saved := &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}, fail: timedOut}
	p.c.key.saved(saved, p.clock)
	p.syncers <- saved
	p.expect("the saved key's first pass, silent", false, clientRetryAfter, nil)
	if saved.asked() != 1 || old.asked() != 2 {
		t.Errorf("the saved key was asked %d times and the old one %d, want 1 and 2", saved.asked(), old.asked())
	}
	p.wake()
	p.expect("the saved key's second silent pass", true, clientPassEvery, new(unifiNotAnswering))
}

// New wants while a silent pass waits for its retry run that retry at once: one pass, which is the
// second in a row, and the minute after it.
func TestWantsDuringARetryRunItOnce(t *testing.T) {
	s := &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}, fail: timedOut}
	p := newPassRig(t, s)
	p.expect("the start's read", false, clientRetryAfter, nil)
	p.putSlot("eu", poolIntent(t, "EU"))
	p.settle()
	p.expect("the pass the wants started", true, clientPassEvery, new(unifiNotAnswering))
	if asked := s.asked(); asked != 2 {
		t.Errorf("UniFi was asked %d times, want the start's read and one pass", asked)
	}
	s.failWith(nil)
	p.wake()
	p.expect("the minute's pass", true, clientPassEvery, nil)
	if id := p.c.state.slots["eu"].Slot.Allocation.NetworkID; id != "net-eu" {
		t.Errorf("the pass that worked saved client %q", id)
	}
}
