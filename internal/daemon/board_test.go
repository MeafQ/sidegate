package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
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

// A daemon with a key and no slots knows what UniFi holds, and writes none of it.
func TestAKeyWithNoSlotsStillReadsTheBoard(t *testing.T) {
	r := newRig(t, t.TempDir())
	s := &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}, foreign: []unifi.ForeignClient{{ID: "b", Name: "Office", Enabled: true}}}
	wants, out := make(chan []unifi.Want, 1), make(chan clientReport, 1)
	r.c.wants, r.c.clientReports = wants, out
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go runClients(ctx, s, nil, wants, out, time.After)
	r.settle()
	select {
	case report := <-out:
		if err := r.c.applyClients(ctx, report); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no slots, so UniFi was never read")
	}
	clients := r.app.Snapshot().VPNClients
	if !clients.Known || len(clients.Foreign) != 1 || clients.Foreign[0].Name != "Office" {
		t.Fatalf("the board with no slots: %+v", clients)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.passes) != 0 {
		t.Fatalf("a read with nothing wanted reconciled: %v", s.passes)
	}
}

func TestTheBoardReadsForeignClientsFromAPass(t *testing.T) {
	r, s, pass := clientRig(t)
	if clients := r.app.Snapshot().VPNClients; clients.Known || len(clients.Foreign) != 0 || clients.Limit != unifi.Limit {
		t.Fatalf("before a pass: %+v", clients)
	}
	feed := r.app.feed.join()
	defer r.app.feed.leave(feed)
	s.foreign = []unifi.ForeignClient{{ID: "z", Name: "Later", Enabled: false}, {ID: "b", Name: "Earlier", Enabled: true}}
	s.routes["gone"] = []unifi.Route{{ID: "r"}}
	r.putSlot("eu", poolIntent(t, "EU"))
	pass()
	clients := r.app.Snapshot().VPNClients
	if !clients.Known {
		t.Fatal("a pass read UniFi")
	}
	want := []ForeignClientDTO{{ID: "b", Name: "Earlier", Enabled: true}, {ID: "net-gone", Name: "Kept gone", Enabled: true}, {ID: "z", Name: "Later"}}
	if len(clients.Foreign) != 3 || clients.Foreign[0] != want[0] || clients.Foreign[1] != want[1] || clients.Foreign[2] != want[2] {
		t.Fatalf("foreign %+v", clients.Foreign)
	}
	if s.inspected != 1 {
		t.Fatalf("Inspect ran %d times, and only the first read comes before any wants", s.inspected)
	}
	sawNews := func() bool {
		heard := false
		for {
			select {
			case e := <-feed:
				heard = heard || e.Kind == "slot" && e.Slot == nil
			default:
				return heard
			}
		}
	}
	if !sawNews() {
		t.Fatal("the page heard nothing of the new foreign clients")
	}
	r.c.publish(r.clock)
	if sawNews() {
		t.Fatal("no change, and the page heard of one")
	}
}

// fakeNetworkApp is the little of UniFi a pass needs, over the real client, so a test
// reads what would go over the wire.
type fakeNetworkApp struct {
	mu      sync.Mutex
	clients []map[string]any
	traffic []map[string]any
	writes  []string
}

func (f *fakeNetworkApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+string(raw))
	}
	ok := func(data any) {
		json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"rc": "ok"}, "data": data})
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/rest/networkconf") && r.Method == http.MethodPost:
		doc["_id"] = "client-1"
		f.clients = append(f.clients, doc)
		ok([]any{doc})
	case strings.HasSuffix(r.URL.Path, "/rest/networkconf"):
		ok(f.clients)
	case strings.Contains(r.URL.Path, "/rest/networkconf/"):
		for k, v := range doc {
			f.clients[0][k] = v
		}
		ok([]any{f.clients[0]})
	case strings.HasSuffix(r.URL.Path, "/rest/routing"):
		ok([]any{})
	case strings.HasSuffix(r.URL.Path, "/trafficroutes") && f.traffic != nil:
		json.NewEncoder(w).Encode(f.traffic)
	default:
		w.Write([]byte("[]"))
	}
}

func TestBlockAndDirectWriteNothingToUniFi(t *testing.T) {
	for _, exit := range []domain.Exit{domain.BlockExit(), domain.DirectExit()} {
		t.Run(exit.Kind(), func(t *testing.T) { writesNothingToUniFi(t, exit) })
	}
}

func writesNothingToUniFi(t *testing.T, exit domain.Exit) {
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
	wants := make(chan []unifi.Want, 1)
	r.c.wants = wants
	r.putSlot("eu", poolIntent(t, "EU"))
	r.settle()

	pass := func() unifi.Report {
		t.Helper()
		row := r.c.state.slots["eu"]
		want, err := r.c.wantOf(row)
		if err != nil {
			t.Fatal(err)
		}
		report, err := client.Reconcile(context.Background(), []unifi.Want{want})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	report := pass()
	row := r.c.state.slots["eu"]
	row.Slot.Allocation.NetworkID = report.Slots[0].NetworkID
	r.c.state.slots["eu"] = row
	<-wants
	r.settle()
	<-wants
	app.mu.Lock()
	app.writes = nil
	app.mu.Unlock()

	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: row.Slot.Version, Intent: exitIntent(t, "EU", exit)}); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if got := r.app.Snapshot().Slots[0]; got.Status != exit.Kind() {
		t.Fatalf("the slot reads %+v", got)
	}
	select {
	case again := <-wants:
		t.Fatalf("it asked UniFi for something: %+v", again)
	default:
	}
	if report := pass(); report.Inventory.Owned[0].Enabled == nil || !*report.Inventory.Owned[0].Enabled {
		t.Fatalf("owned %+v", report.Inventory.Owned)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.writes) != 0 {
		t.Fatalf("it wrote to UniFi: %v", app.writes)
	}
}
