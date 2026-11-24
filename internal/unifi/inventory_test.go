package unifi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

const trafficPath = "/v2/api/site/default/trafficroutes"

func trafficRoute(id, network string, blocks any) map[string]any {
	doc := map[string]any{
		"_id": id, "description": "route " + id, "enabled": true, "matching_target": "DOMAIN", "network_id": network,
		"target_devices": []any{map[string]any{"type": "ALL_CLIENTS"}},
	}
	if blocks != nil {
		doc["kill_switch_enabled"] = blocks
	}
	return doc
}

// The product reads a route's kill switch and never writes one: a pass that does everything else
// a pass can do sends nothing to the routes.
func TestAKillSwitchIsReadAndNeverWritten(t *testing.T) {
	c, app := setup(t)
	w := testWant("slot-a", "A", 1)
	report, err := c.Reconcile(context.Background(), []Want{w})
	if err != nil {
		t.Fatal(err)
	}
	w.Bound = report.Slots[0].NetworkID
	id := string(w.Bound)
	app.traffic = []map[string]any{trafficRoute("on", id, true), trafficRoute("off", id, false), trafficRoute("lacking", id, nil)}
	app.sent = nil
	w.Name = "Renamed"
	report, err = c.Reconcile(context.Background(), []Want{w})
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]any{}
	for _, r := range report.Inventory.Owned[0].Routes {
		read[r.ID] = nil
		if r.KillSwitch != nil {
			read[r.ID] = *r.KillSwitch
		}
	}
	if want := map[string]any{"on": true, "off": false, "lacking": nil}; !reflect.DeepEqual(read, want) {
		t.Fatalf("read %v, want %v", read, want)
	}
	if len(app.sent) == 0 {
		t.Fatal("the rename reached nothing, so this pass proves nothing")
	}
	for _, sent := range app.sent {
		if strings.Contains(sent.Path, "trafficroutes") {
			t.Fatalf("a pass wrote a route: %s %s", sent.Method, sent.Path)
		}
	}
}

func TestAMarkerClearedClientStaysOurs(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("restoring it refused %v", refused), func(t *testing.T) {
			c, app := setup(t)
			w := testWant("slot-a", "A", 1)
			report, err := c.Reconcile(context.Background(), []Want{w})
			if err != nil {
				t.Fatal(err)
			}
			w.Bound = report.Slots[0].NetworkID
			app.network(w.Bound)["wireguard_client_configuration_filename"] = ""
			app.traffic = []map[string]any{trafficRoute("route", string(w.Bound), true)}
			if refused {
				app.intercept = func(rw http.ResponseWriter, r *http.Request) bool {
					if r.Method != http.MethodPut {
						return false
					}
					refuse(rw, "api.err.Nope")
					return true
				}
			}
			report, err = c.Reconcile(context.Background(), []Want{w})
			if err != nil {
				t.Fatal(err)
			}
			inv := report.Inventory
			if len(inv.Foreign) != 0 {
				t.Fatalf("it shows as foreign: %+v", inv.Foreign)
			}
			if len(inv.Owned) != 1 || inv.Owned[0].Slot != w.Slot || len(inv.Owned[0].Routes) != 1 {
				t.Fatalf("its routes are lost: %+v", inv.Owned)
			}
			if blocks := inv.Owned[0].Routes[0].KillSwitch; blocks == nil || !*blocks {
				t.Fatalf("kill switch %v", blocks)
			}
		})
	}
}

func TestTheInventoryNamesForeignClientsAndKeepsEnabled(t *testing.T) {
	c, app := setup(t)
	on := ownerClient(t, app, "wgclt6", "198.51.100.7", 6)
	off := ownerClient(t, app, "Theirs off", "198.51.100.8", 7)
	app.network(domain.NetworkID(on))["enabled"] = true
	app.network(domain.NetworkID(off))["enabled"] = false
	silent := ownerClient(t, app, "Theirs silent", "198.51.100.9", 8)

	report, err := c.Reconcile(context.Background(), []Want{testWant("slot-a", "A", 1)})
	if err != nil {
		t.Fatal(err)
	}
	want := []ForeignClient{{ID: domain.NetworkID(on), Name: "wgclt6", Enabled: true}, {ID: domain.NetworkID(off), Name: "Theirs off"}, {ID: domain.NetworkID(silent), Name: "Theirs silent", Enabled: true}}
	if !reflect.DeepEqual(report.Inventory.Foreign, want) {
		t.Fatalf("foreign %+v", report.Inventory.Foreign)
	}
	if got := report.Inventory.Owned; len(got) != 1 || got[0].Enabled == nil || !*got[0].Enabled || got[0].Name != "A" {
		t.Fatalf("a client just created: %+v", got)
	}
	if report.Inventory.Clients != 4 {
		t.Fatalf("%d clients", report.Inventory.Clients)
	}
	inspected, err := c.Inspect(context.Background())
	if err != nil || !reflect.DeepEqual(inspected.Foreign, want) {
		t.Fatalf("inspect %+v, %v", inspected.Foreign, err)
	}
}

func TestAClientTheOwnerDisabledStaysDisabled(t *testing.T) {
	c, app := setup(t)
	w := testWant("slot-a", "A", 1)
	report, err := c.Reconcile(context.Background(), []Want{w})
	if err != nil {
		t.Fatal(err)
	}
	w.Bound = report.Slots[0].NetworkID
	app.network(w.Bound)["enabled"] = false
	app.sent = nil

	w.Name = "Renamed"
	report, err = c.Reconcile(context.Background(), []Want{w})
	if err != nil {
		t.Fatal(err)
	}
	if len(app.sent) != 1 || strings.Contains(string(app.sent[0].Body), `"enabled"`) {
		t.Fatalf("a rename must write only the name: %s", app.sent[0].Body)
	}
	if got := report.Inventory.Owned[0]; got.Enabled == nil || *got.Enabled || got.Name != "Renamed" {
		t.Fatalf("owned %+v", got)
	}
	if app.network(w.Bound)["enabled"] != false {
		t.Fatal("the client was enabled")
	}
}

func TestARetainedClientKeepsItsRoutesInTheInventory(t *testing.T) {
	c, app := setup(t)
	report, err := c.Reconcile(context.Background(), []Want{testWant("slot-a", "A", 1)})
	if err != nil {
		t.Fatal(err)
	}
	id := report.Slots[0].NetworkID
	app.traffic = []map[string]any{trafficRoute("route", string(id), false)}
	report, err = c.Reconcile(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := report.Inventory.Owned
	if len(report.Retained) != 1 || len(got) != 1 || got[0].NetworkID != id || got[0].Slot != "slot-a" || got[0].Name != "A" || len(got[0].Routes) != 1 {
		t.Fatalf("owned %+v, retained %+v", got, report.Retained)
	}
}

func TestRoutesUnreadAfterAPassKeepTheResults(t *testing.T) {
	c, app := setup(t)
	app.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == trafficPath {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	report, err := c.Reconcile(context.Background(), []Want{testWant("slot-a", "A", 1)})
	var failure *Error
	if !errors.As(err, &failure) || failure.Kind != Unavailable {
		t.Fatalf("err %v", err)
	}
	if report.Slots[0].Action != Created || report.Inventory.Owned != nil {
		t.Fatalf("report %+v", report)
	}
}

// A pass reads the routes to the owner's clients as well as ours, and a client it lists nowhere has none.
func TestAPassReadsTheRoutesToTheOwnersClients(t *testing.T) {
	c, app := setup(t)
	theirs := domain.NetworkID(ownerClient(t, app, "Theirs", "198.51.100.7", 6))
	app.traffic = []map[string]any{trafficRoute("route", string(theirs), nil)}
	report, err := c.Reconcile(context.Background(), []Want{testWant("slot-a", "A", 1)})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := c.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ours := report.Slots[0].NetworkID
	for pass, inv := range map[string]Inventory{"reconcile": report.Inventory, "inspect": inspected} {
		if !inv.Routed(theirs) || inv.Routed(ours) || inv.Routed("gone") {
			t.Errorf("%s: routed theirs %t, ours %t, gone %t", pass, inv.Routed(theirs), inv.Routed(ours), inv.Routed("gone"))
		}
	}
}
