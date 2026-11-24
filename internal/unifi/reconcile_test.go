package unifi

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

const testAPIKey = "test-api-key-value"

// testKey derives a stable X25519 pair, so a want is the same want across calls.
func testKey(seed byte) (private, public string) {
	raw := make([]byte, 32)
	raw[0] = seed
	raw[31] = 0x40
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(k.Bytes()), base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

func testWant(slot, name string, n int) Want {
	private, public := testKey(byte(2 * n))
	_, peer := testKey(byte(2*n + 1))
	return Want{
		Slot: domain.SlotID(slot), Name: name,
		Endpoint:      netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 51830+uint16(n)),
		Address:       netip.MustParsePrefix(fmt.Sprintf("10.70.%d.2/30", n)),
		DNS:           netip.MustParseAddr("1.1.1.1"),
		PeerPublicKey: peer, PublicKey: public, PrivateKey: domain.NewSecret(private),
	}
}

func setup(t *testing.T) (*Client, *fakeApp) {
	t.Helper()
	app := seededApp(t)
	server := httptest.NewTLSServer(app)
	t.Cleanup(server.Close)
	c, err := New(Config{BaseURL: server.URL, Site: "default", APIKey: domain.NewSecret(testAPIKey)})
	if err != nil {
		t.Fatal(err)
	}
	return c, app
}

// ownerClient adds a client the owner made, with no marker of ours.
func ownerClient(t *testing.T, app *fakeApp, name, peerIP string, n int) string {
	t.Helper()
	_, peer := testKey(byte(100 + n))
	doc := map[string]any{
		"_id": app.id(), "name": name, "purpose": purposeVPNClient, "vpn_type": "wireguard-client",
		"wireguard_client_mode": "manual", "wireguard_client_peer_ip": peerIP, "wireguard_client_peer_port": float64(443),
		"wireguard_client_peer_public_key": peer, "ip_subnet": fmt.Sprintf("10.72.%d.2/30", n), "dhcpd_dns_1": "1.1.1.1",
	}
	app.networks = append(app.networks, doc)
	return doc["_id"].(string)
}

func (f *fakeApp) network(id domain.NetworkID) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.networks {
		if n["_id"] == string(id) {
			return n
		}
	}
	return nil
}

func bind(t *testing.T, c *Client, wants ...Want) []Want {
	t.Helper()
	report, err := c.Reconcile(context.Background(), wants)
	if err != nil {
		t.Fatal(err)
	}
	out := slices.Clone(wants)
	for i, r := range report.Slots {
		if r.Problem != nil || r.NetworkID == "" {
			t.Fatalf("slot %s: %v", r.Slot, r.Problem)
		}
		out[i].Bound = r.NetworkID
	}
	return out
}

func problemKind(r SlotResult) ProblemKind {
	if r.Problem == nil {
		return 0
	}
	return r.Problem.Kind
}

func TestCreatesThenWritesNothing(t *testing.T) {
	c, app := setup(t)
	wants := bind(t, c, testWant("slot-a", "A", 1), testWant("slot-b", "B", 2))
	n := app.network(wants[0].Bound)
	if n["name"] != "A" || n["wireguard_client_peer_ip"] != "127.0.0.1" || n["wireguard_client_peer_port"] != float64(51831) {
		t.Fatalf("created %v", n)
	}
	writes := app.writes
	report, err := c.Reconcile(context.Background(), wants)
	if err != nil {
		t.Fatal(err)
	}
	if app.writes != writes {
		t.Fatalf("a second pass made %d writes", app.writes-writes)
	}
	for _, r := range report.Slots {
		if r.Action != Unchanged || r.Problem != nil {
			t.Fatalf("second pass: %+v", r)
		}
	}
}

func TestOwnersClientIsNeitherClaimedNorTouched(t *testing.T) {
	c, app := setup(t)
	wgclt6 := ownerClient(t, app, "wgclt6", "198.51.100.7", 6)
	onLoopback := ownerClient(t, app, "Theirs", "127.0.0.1", 7)
	before := fmt.Sprint(app.network(domain.NetworkID(wgclt6)), app.network(domain.NetworkID(onLoopback)))
	report, err := c.Reconcile(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Inventory.Foreign) != 2 || report.Inventory.Clients != 2 || len(report.Removed) != 0 || len(report.Retained) != 0 {
		t.Fatalf("report %+v", report)
	}
	inv, err := c.Inspect(context.Background())
	if err != nil || len(inv.Owned) != 0 || len(inv.Foreign) != 2 {
		t.Fatalf("inspect %+v, %v", inv, err)
	}
	if after := fmt.Sprint(app.network(domain.NetworkID(wgclt6)), app.network(domain.NetworkID(onLoopback))); after != before {
		t.Fatalf("owner's clients changed:\n%s\n%s", before, after)
	}
}

func TestAnUploadedFileNamedLikeOurMarkerIsNotOurs(t *testing.T) {
	c, app := setup(t)
	id := ownerClient(t, app, "Uploaded", "198.51.100.7", 6)
	n := app.network(domain.NetworkID(id))
	n["wireguard_client_mode"] = "file"
	n["wireguard_client_configuration_filename"] = markerOf("slot-a")
	report, err := c.Reconcile(context.Background(), []Want{testWant("slot-a", "A", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Inventory.Foreign) != 1 || report.Slots[0].Action != Created || len(report.Removed) != 0 {
		t.Fatalf("%+v", report)
	}
	if app.network(domain.NetworkID(id))["name"] != "Uploaded" {
		t.Fatal("the owner's client was claimed")
	}
}

func TestRenameKeepsID(t *testing.T) {
	c, app := setup(t)
	wants := bind(t, c, testWant("slot-a", "A", 1))
	wants[0].Name = "Renamed"
	report, err := c.Reconcile(context.Background(), wants)
	if err != nil {
		t.Fatal(err)
	}
	if r := report.Slots[0]; r.Action != Updated || r.NetworkID != wants[0].Bound {
		t.Fatalf("%+v", r)
	}
	if app.network(wants[0].Bound)["name"] != "Renamed" {
		t.Fatal("name not applied")
	}
}

func TestWritesOnlyWhatDiffers(t *testing.T) {
	c, app := setup(t)
	wants := bind(t, c, testWant("slot-a", "A", 1))
	app.mu.Lock()
	for _, n := range app.networks {
		if n["_id"] == string(wants[0].Bound) {
			n["mss_clamp"] = "custom"
			n["name"] = "Changed By Owner"
		}
	}
	var sent string
	app.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			buf := make([]byte, r.ContentLength)
			r.Body.Read(buf)
			sent = string(buf)
			r.Body = http.NoBody
			legacyAnswer(w, 200, nil, nil)
			return true
		}
		return false
	}
	app.mu.Unlock()
	if _, err := c.Reconcile(context.Background(), wants); err != nil {
		t.Fatal(err)
	}
	if sent != `{"name":"A"}` {
		t.Fatalf("sent %s", sent)
	}
}

func TestClearedMarkerIsRestoredByIDAndPeerKey(t *testing.T) {
	c, app := setup(t)
	wants := bind(t, c, testWant("slot-a", "A", 1))
	app.network(wants[0].Bound)["wireguard_client_configuration_filename"] = ""
	report, err := c.Reconcile(context.Background(), wants)
	if err != nil {
		t.Fatal(err)
	}
	if r := report.Slots[0]; r.Action != Updated || r.NetworkID != wants[0].Bound {
		t.Fatalf("%+v", r)
	}
	if app.network(wants[0].Bound)["wireguard_client_configuration_filename"] != markerOf("slot-a") {
		t.Fatal("marker not restored")
	}
}

func TestBindingToSomeoneElsesClientIsNotClaimed(t *testing.T) {
	c, app := setup(t)
	id := ownerClient(t, app, "wgclt6", "198.51.100.7", 6)
	w := testWant("slot-a", "A", 1)
	w.Bound = domain.NetworkID(id)
	before := fmt.Sprint(app.network(w.Bound))
	report, err := c.Reconcile(context.Background(), []Want{w})
	if err != nil {
		t.Fatal(err)
	}
	if problemKind(report.Slots[0]) != ExternallyDeleted || report.Slots[0].NetworkID != "" {
		t.Fatalf("%+v", report.Slots[0])
	}
	if fmt.Sprint(app.network(w.Bound)) != before {
		t.Fatal("owner's client changed")
	}
}

func TestNameCollisions(t *testing.T) {
	c, app := setup(t)
	ownerClient(t, app, "wgclt6", "198.51.100.7", 6)
	wants := bind(t, c, testWant("slot-a", "A", 1))
	writes := app.writes

	for _, name := range []string{"wgclt6", "WGCLT6", "Default"} {
		report, err := c.Reconcile(context.Background(), []Want{wants[0], testWant("slot-new", name, 2)})
		if err != nil {
			t.Fatal(err)
		}
		if r := report.Slots[1]; problemKind(r) != NameCollision || r.NetworkID != "" {
			t.Fatalf("new slot named %q: %+v", name, r)
		}
	}
	if app.writes != writes {
		t.Fatal("a collision wrote something")
	}

	renamed := wants[0]
	renamed.Name = "wgclt6"
	report, err := c.Reconcile(context.Background(), []Want{renamed})
	if err != nil {
		t.Fatal(err)
	}
	if r := report.Slots[0]; problemKind(r) != NameCollision || r.NetworkID != renamed.Bound {
		t.Fatalf("rename: %+v", r)
	}
	if app.network(renamed.Bound)["name"] != "A" {
		t.Fatal("the client took the owner's name")
	}

	report, err = c.Reconcile(context.Background(), []Want{wants[0], testWant("slot-dup", "a", 2)})
	if err != nil {
		t.Fatal(err)
	}
	if problemKind(report.Slots[1]) != NameCollision {
		t.Fatalf("two slots on one name: %+v", report.Slots[1])
	}
}

func TestSwappingNamesIsNotACollision(t *testing.T) {
	c, app := setup(t)
	wants := bind(t, c, testWant("slot-a", "A", 1), testWant("slot-b", "B", 2))
	wants[0].Name, wants[1].Name = "B", "A"
	report, err := c.Reconcile(context.Background(), wants)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range report.Slots {
		if r.Problem != nil {
			t.Fatalf("%+v", r)
		}
	}
	if app.network(wants[0].Bound)["name"] != "B" {
		t.Fatal("swap not applied")
	}
}

func TestLimitCountsTheOwnersClients(t *testing.T) {
	c, app := setup(t)
	ownerClient(t, app, "wgclt6", "198.51.100.7", 6)
	var wants []Want
	for i := 1; i <= 8; i++ {
		wants = append(wants, testWant(fmt.Sprintf("slot-%d", i), fmt.Sprintf("S%d", i), i))
	}
	report, err := c.Reconcile(context.Background(), wants)
	if err != nil {
		t.Fatal(err)
	}
	if r := report.Slots[7]; problemKind(r) != LimitReached || r.NetworkID != "" {
		t.Fatalf("eighth slot: %+v", r)
	}
	if r := report.Slots[6]; r.Action != Created {
		t.Fatalf("seventh slot: %+v", r)
	}
	if report.Inventory.Clients != Limit || len(report.Inventory.Foreign) != 1 {
		t.Fatalf("report %d clients, %v foreign", report.Inventory.Clients, report.Inventory.Foreign)
	}
	for _, r := range report.Slots[:7] {
		if strings.Contains(fmt.Sprint(r.Problem), "limit") {
			t.Fatalf("%+v", r)
		}
	}
}

func TestADeleteFreesCapacityInTheSamePass(t *testing.T) {
	c, app := setup(t)
	var wants []Want
	for i := 1; i <= 8; i++ {
		wants = append(wants, testWant(fmt.Sprintf("slot-%d", i), fmt.Sprintf("S%d", i), i))
	}
	wants = bind(t, c, wants...)
	report, err := c.Reconcile(context.Background(), append(slices.Clone(wants[1:]), testWant("slot-9", "S9", 9)))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Removed) != 1 || report.Slots[7].Action != Created {
		t.Fatalf("%+v", report)
	}
	if app.network(wants[0].Bound) != nil {
		t.Fatal("the dropped client is still there")
	}
}

func TestRoutedSlotIsRetained(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind RouteKind
		add  func(app *fakeApp, id string)
	}{
		{"traffic", TrafficRoute, func(app *fakeApp, id string) {
			app.traffic = append(app.traffic, map[string]any{"_id": "r1", "description": "my route", "network_id": id})
		}},
		{"static", StaticRoute, func(app *fakeApp, id string) {
			app.static = append(app.static, map[string]any{"_id": "r1", "name": "my route", "static-route_interface": id})
		}},
		{"policy, nested under an unrecorded field", PolicyRoute, func(app *fakeApp, id string) {
			app.policy = append(app.policy, map[string]any{"_id": "r1", "name": "my route", "targets": []any{map[string]any{"vpn": id}}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, app := setup(t)
			wants := bind(t, c, testWant("slot-a", "A", 1))
			tc.add(app, string(wants[0].Bound))
			writes := app.writes

			report, err := c.Reconcile(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if app.writes != writes || app.network(wants[0].Bound) == nil {
				t.Fatal("a routed client was written to")
			}
			if len(report.Removed) != 0 || len(report.Retained) != 1 {
				t.Fatalf("%+v", report)
			}
			got := report.Retained[0]
			if got.Slot != "slot-a" || got.Problem.Kind != RoutedElsewhere || len(got.Problem.Routes) != 1 {
				t.Fatalf("%+v", got)
			}
			if r := got.Problem.Routes[0]; r.Kind != tc.kind || r.Name != "my route" || r.Target != wants[0].Bound {
				t.Fatalf("%+v", r)
			}
			inv, err := c.Inspect(context.Background())
			if err != nil || len(inv.Owned) != 1 || len(inv.Owned[0].Routes) != 1 {
				t.Fatalf("inspect %+v, %v", inv, err)
			}
		})
	}
}

func TestUnreadableRoutesKeepTheClient(t *testing.T) {
	c, app := setup(t)
	wants := bind(t, c, testWant("slot-a", "A", 1))
	writes := app.writes
	app.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/object-oriented-network-configs") {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return false
	}
	report, err := c.Reconcile(context.Background(), nil)
	if isKind(err, Rejected) || err == nil {
		t.Fatalf("a store that can't be read must end the pass: %v", err)
	}
	if app.writes != writes || app.network(wants[0].Bound) == nil {
		t.Fatal("deleted without proof that nothing routes to it")
	}
	if len(report.Retained) != 1 {
		t.Fatalf("%+v", report)
	}
}

func TestAppDownKeepsBindingsAndSaysUnavailable(t *testing.T) {
	c, app := setup(t)
	wants := bind(t, c, testWant("slot-a", "A", 1))
	for name, intercept := range map[string]func(http.ResponseWriter, *http.Request) bool{
		"503 from the proxy": func(w http.ResponseWriter, r *http.Request) bool { w.WriteHeader(503); return true },
		"connection dropped": func(w http.ResponseWriter, r *http.Request) bool { panic(http.ErrAbortHandler) },
	} {
		app.intercept = intercept
		report, err := c.Reconcile(context.Background(), wants)
		if !isKind(err, Unavailable) {
			t.Fatalf("%s: %v", name, err)
		}
		if len(report.Slots) != 0 {
			t.Fatalf("%s: %+v", name, report)
		}
	}
	app.intercept = nil
	if _, err := c.Reconcile(context.Background(), wants); err != nil {
		t.Fatal(err)
	}
}

func TestAppDownMidPassReportsWhatWasNotReached(t *testing.T) {
	c, app := setup(t)
	app.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost {
			w.WriteHeader(502)
			return true
		}
		return false
	}
	report, err := c.Reconcile(context.Background(), []Want{testWant("slot-a", "A", 1), testWant("slot-b", "B", 2)})
	if !isKind(err, Unavailable) {
		t.Fatal(err)
	}
	for _, r := range report.Slots {
		if problemKind(r) != NotReached || r.NetworkID != "" {
			t.Fatalf("%+v", r)
		}
	}
}

func TestLostCreateReplyAdoptsAndNeverDuplicates(t *testing.T) {
	c, app := setup(t)
	want := testWant("slot-a", "A", 1)
	app.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}
		app.intercept = nil
		app.mu.Unlock()
		app.ServeHTTP(httptest.NewRecorder(), r)
		app.mu.Lock()
		panic(http.ErrAbortHandler)
	}
	if _, err := c.Reconcile(context.Background(), []Want{want}); !isKind(err, Unavailable) {
		t.Fatal(err)
	}
	report, err := c.Reconcile(context.Background(), []Want{want})
	if err != nil {
		t.Fatal(err)
	}
	if r := report.Slots[0]; r.Action != Adopted || r.NetworkID == "" {
		t.Fatalf("%+v", r)
	}
	if report.Inventory.Clients != 1 {
		t.Fatalf("%d clients", report.Inventory.Clients)
	}
}

func TestExternallyDeletedIsReportedNotRecreated(t *testing.T) {
	c, app := setup(t)
	want := bind(t, c, testWant("slot-a", "A", 1))[0]
	app.networks = slices.DeleteFunc(app.networks, func(n map[string]any) bool { return n["_id"] == string(want.Bound) })
	writes := app.writes
	report, err := c.Reconcile(context.Background(), []Want{want})
	if err != nil {
		t.Fatal(err)
	}
	if r := report.Slots[0]; problemKind(r) != ExternallyDeleted || r.NetworkID != "" {
		t.Fatalf("%+v", r)
	}
	if app.writes != writes {
		t.Fatal("re-created")
	}
	want.Bound = ""
	report, err = c.Reconcile(context.Background(), []Want{want})
	if err != nil || report.Slots[0].Action != Created {
		t.Fatalf("%+v, %v", report, err)
	}
}

func TestReinstallAdoptsByNameAndKeepsRoutes(t *testing.T) {
	c, app := setup(t)
	old := bind(t, c, testWant("slot-old", "Household", 1))[0]
	app.traffic = append(app.traffic, map[string]any{"_id": "r1", "description": "route", "network_id": string(old.Bound)})
	report, err := c.Reconcile(context.Background(), []Want{testWant("slot-new", "household", 2)})
	if err != nil {
		t.Fatal(err)
	}
	r := report.Slots[0]
	if r.Action != Adopted || r.NetworkID != old.Bound {
		t.Fatalf("%+v", r)
	}
	if len(report.Retained)+len(report.Removed) != 0 {
		t.Fatalf("%+v", report)
	}
	if app.network(old.Bound)["wireguard_client_configuration_filename"] != markerOf("slot-new") {
		t.Fatal("marker not moved")
	}
}

func TestDuplicateMarkersKeepTheBoundOne(t *testing.T) {
	c, app := setup(t)
	want := testWant("slot-a", "A", 1)
	first := bind(t, c, want)[0]
	app.networks = append(app.networks, map[string]any{
		"_id": "id-9999", "name": "stray", "purpose": purposeVPNClient, "vpn_type": "wireguard-client", "wireguard_client_mode": "manual",
		"wireguard_client_peer_ip": "127.0.0.1", "wireguard_client_configuration_filename": markerOf("slot-a"),
	})
	report, err := c.Reconcile(context.Background(), []Want{first})
	if err != nil {
		t.Fatal(err)
	}
	if report.Slots[0].NetworkID != first.Bound || len(report.Removed) != 1 || report.Removed[0] != "id-9999" {
		t.Fatalf("%+v", report)
	}
}

func TestAnOwnersClientWithOurFilenameStaysTheirs(t *testing.T) {
	c, app := setup(t)
	id := ownerClient(t, app, "Personal VPN", "198.51.100.7", 6)
	app.network(domain.NetworkID(id))["wireguard_client_configuration_filename"] = "sidegate-personal.conf"
	report, err := c.Reconcile(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if app.network(domain.NetworkID(id)) == nil || len(report.Inventory.Foreign) != 1 {
		t.Fatalf("%+v", report)
	}
}

func TestARouteSavedDuringTheEarlierStoreReadsStopsTheDelete(t *testing.T) {
	c, app := setup(t)
	want := bind(t, c, testWant("slot-a", "A", 1))[0]
	saved := false
	app.intercept = func(_ http.ResponseWriter, r *http.Request) bool {
		if !saved && r.Method == http.MethodGet && r.URL.Path == "/api/s/default/rest/routing" {
			saved = true
			app.traffic = append(app.traffic, map[string]any{"_id": "owner-route", "description": "Owner", "network_id": string(want.Bound)})
		}
		return false
	}
	report, err := c.Reconcile(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !saved || app.network(want.Bound) == nil || len(app.traffic) != 1 || len(report.Retained) != 1 {
		t.Fatalf("saved %v, traffic %d, %+v", saved, len(app.traffic), report)
	}
}

func TestRefusedWriteIsThatSlotsAlone(t *testing.T) {
	c, _ := setup(t)
	clash := testWant("slot-a", "A", 1)
	clash.Address = netip.MustParsePrefix("192.168.1.2/30") // Inside the Default network.
	report, err := c.Reconcile(context.Background(), []Want{clash, testWant("slot-b", "B", 2)})
	if err != nil {
		t.Fatal(err)
	}
	var e *Error
	if r := report.Slots[0]; problemKind(r) != RefusedByApp || !errors.As(r.Problem.Err, &e) || e.Code != "api.err.SubnetOverlapped" {
		t.Fatalf("%+v", r)
	}
	if report.Slots[1].Action != Created {
		t.Fatalf("%+v", report.Slots[1])
	}
}

func TestDeleteOfAnIDAlreadyGoneCounts(t *testing.T) {
	c, app := setup(t)
	bind(t, c, testWant("slot-a", "A", 1))
	app.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			refuse(w, "api.err.IdInvalid")
			return true
		}
		return false
	}
	report, err := c.Reconcile(context.Background(), nil)
	if err != nil || len(report.Removed) != 1 {
		t.Fatalf("%+v, %v", report, err)
	}
}

func TestStaticRouteRaceIsARefusalNotACrash(t *testing.T) {
	c, app := setup(t)
	bind(t, c, testWant("slot-a", "A", 1))
	app.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			refuse(w, "api.err.ResourceReferredBy")
			return true
		}
		return false
	}
	report, err := c.Reconcile(context.Background(), nil)
	if err != nil || len(report.Retained) != 1 || report.Retained[0].Problem.Kind != RefusedByApp {
		t.Fatalf("%+v, %v", report, err)
	}
}

func TestFailureKinds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		want   FailureKind
	}{
		{"expired or bad key", func(w http.ResponseWriter) {
			w.WriteHeader(401)
			w.Write([]byte(`{"meta":{"rc":"error","msg":"api.err.LoginRequired"},"data":[]}`))
		}, Unauthorized},
		{"forbidden", func(w http.ResponseWriter) { w.WriteHeader(403) }, Unauthorized},
		{"HTML from a proxy", func(w http.ResponseWriter) { w.Write([]byte("<!doctype html>")) }, Schema},
		{"a redirect to the login page", func(w http.ResponseWriter) {
			w.Header().Set("Location", "/login")
			w.WriteHeader(302)
		}, Schema},
		{"a network with no _id", func(w http.ResponseWriter) {
			w.Write([]byte(`{"meta":{"rc":"ok"},"data":[{"name":"x"}]}`))
		}, Schema},
		{"no envelope", func(w http.ResponseWriter) { w.Write([]byte(`{}`)) }, Schema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, app := setup(t)
			app.intercept = func(w http.ResponseWriter, r *http.Request) bool { tc.answer(w); return true }
			if _, err := c.Reconcile(context.Background(), []Want{testWant("slot-a", "A", 1)}); !isKind(err, tc.want) {
				t.Fatalf("got %v", err)
			}
			if app.writes != 0 {
				t.Fatal("wrote after a failed read")
			}
		})
	}
}

func TestEverythingSendsTheKeyAndNothingPrintsIt(t *testing.T) {
	c, app := setup(t)
	want := testWant("slot-a", "A", 1)
	secret := want.PrivateKey.Reveal()
	bind(t, c, want)
	for _, k := range app.keys {
		if k != testAPIKey {
			t.Fatalf("a request went with key %q", k)
		}
	}
	app.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost {
			refuse(w, "api.err.WireguardInvalidKey", "field_name", "x_wireguard_private_key")
			return true
		}
		return false
	}
	report, _ := c.Reconcile(context.Background(), []Want{testWant("slot-b", "B", 2)})
	nets, err := c.networks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, printed := range []string{
		fmt.Sprintf("%+v", report), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%+v", want),
		fmt.Sprintf("%+v", nets), fmt.Sprint(report.Slots[0].Problem),
	} {
		if strings.Contains(printed, testAPIKey) || strings.Contains(printed, secret) {
			t.Fatalf("a secret was printed: %s", printed)
		}
	}
}

// UniFi may quote the key it was sent anywhere in what it answers. No error the client returns carries
// it, and the rest of what UniFi said is kept.
func TestNoErrorCarriesTheKeyUniFiEchoes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		kept   string
	}{
		{"a 401 whose v2 code is the key", func(w http.ResponseWriter) {
			w.WriteHeader(401)
			fmt.Fprintf(w, `{"code":%q}`, testAPIKey)
		}, ""},
		{"a 401 whose message holds the key", func(w http.ResponseWriter) {
			w.WriteHeader(401)
			fmt.Fprintf(w, `{"meta":{"rc":"error","msg":"no key %s for this site"},"data":[]}`, testAPIKey)
		}, "no key <API key> for this site"},
		{"a 400 that blames the key's field", func(w http.ResponseWriter) {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"meta":{"rc":"error","msg":"api.err.%s","field_name":"x%sx"},"data":[]}`, testAPIKey, testAPIKey)
		}, "api.err.<API key> on x<API key>x"},
		{"an rc:error in a 200", func(w http.ResponseWriter) {
			fmt.Fprintf(w, `{"meta":{"rc":"error","msg":"%s%s"},"data":[]}`, testAPIKey, testAPIKey)
		}, "<API key><API key>"},
		{"a 400 that isn't JSON", func(w http.ResponseWriter) {
			w.WriteHeader(400)
			fmt.Fprintf(w, "bad key %s", testAPIKey)
		}, ""},
		{"a status line that holds the key", func(w http.ResponseWriter) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				panic(err)
			}
			fmt.Fprintf(buf, "HTTP/1.1 %s OK\r\n\r\n", testAPIKey)
			buf.Flush()
			conn.Close()
		}, "<API key>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, app := setup(t)
			app.intercept = func(w http.ResponseWriter, r *http.Request) bool { tc.answer(w); return true }
			err := c.Check(context.Background())
			if err == nil {
				t.Fatal("the answer was taken")
			}
			if printed := fmt.Sprintf("%v %+v", err, err); strings.Contains(printed, testAPIKey) {
				t.Errorf("the error carries the key: %s", printed)
			}
			if !strings.Contains(err.Error(), tc.kept) {
				t.Errorf("the error %q lost %q", err, tc.kept)
			}
		})
	}
}

func TestNewRefusesAnythingOffLoopback(t *testing.T) {
	key := domain.NewSecret("k")
	for _, base := range []string{
		"https://192.0.2.1/proxy/network", "http://127.0.0.1/proxy/network", "https://unifi.local", "https://localhost", "://",
	} {
		if _, err := New(Config{BaseURL: base, Site: "default", APIKey: key}); err == nil {
			t.Errorf("accepted %q", base)
		}
	}
	if _, err := New(Config{BaseURL: "https://127.0.0.1/proxy/network", Site: "default"}); err == nil {
		t.Error("accepted an empty key")
	}
	if _, err := New(Config{BaseURL: "https://127.0.0.1/proxy/network", Site: "default", APIKey: key}); err != nil {
		t.Error(err)
	}
}

func TestAClientTurnedOffStaysOffUntilTheOwnerAsks(t *testing.T) {
	c, app := setup(t)
	wants := bind(t, c, testWant("slot-a", "A", 1))
	n := app.network(wants[0].Bound)
	app.mu.Lock()
	n["enabled"] = false
	app.mu.Unlock()
	report, err := c.Reconcile(context.Background(), wants)
	if err != nil {
		t.Fatal(err)
	}
	if on := report.Inventory.Owned[0].Enabled; on == nil || *on || app.network(wants[0].Bound)["enabled"] != false {
		t.Fatalf("a pass turned the owner's client back on: %v", on)
	}
	wants[0].Enable = true
	if report, err = c.Reconcile(context.Background(), wants); err != nil {
		t.Fatal(err)
	}
	if on := report.Inventory.Owned[0].Enabled; on == nil || !*on || app.network(wants[0].Bound)["enabled"] != true {
		t.Fatalf("asked to, the pass left it off: %v", on)
	}
}
