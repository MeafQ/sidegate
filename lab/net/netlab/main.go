// Command netlab drives internal/unifi against the lab's Network Application and
// records every exchange, sanitized, as the contract that go test replays. It
// refuses any address but loopback, so it can't reach a real gateway.
package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"sidegate/internal/domain"
	"sidegate/internal/unifi"
)

const defaultFixture = "internal/unifi/testdata/contract.json"

func main() {
	base := flag.String("base", "https://127.0.0.1:18443", "loopback URL of the lab's app")
	vm := flag.String("vm", "unifi-lab-net", "Lima VM that runs the app")
	state := flag.String("state", "lab/.state/net", "directory for the lab admin password")
	fixture := flag.String("fixture", defaultFixture, "contract file to write")
	only := flag.String("only", "", "comma-separated steps, for working on the driver; give -fixture elsewhere")
	flag.Parse()

	u, err := url.Parse(*base)
	if err != nil || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		fatalf("%q is not a loopback URL", *base)
	}
	l := &lab{base: *base, vm: *vm}
	l.recorder = &recorder{inner: newSession(*base, filepath.Join(*state, "admin"))}
	l.http = &http.Client{Transport: l.recorder, Timeout: 30 * time.Second}
	l.recorder.session().bootstrap(l)
	l.client, err = unifi.New(unifi.Config{
		BaseURL: *base, Site: "default", APIKey: domain.NewSecret("the lab session replaces this"),
		Timeout: 10 * time.Second, Transport: l.recorder,
	})
	if err != nil {
		fatalf("%v", err)
	}

	steps := []struct {
		name string
		run  func(*lab)
	}{
		{"validators", (*lab).stepValidators},
		{"owners", (*lab).stepOwners},
		{"create", (*lab).stepCreate},
		{"rename", (*lab).stepRename},
		{"uisave", (*lab).stepUISave},
		{"marker", (*lab).stepMarkerCleared},
		{"collision", (*lab).stepCollision},
		{"routed", (*lab).stepRouted},
		{"cascade", (*lab).stepCascade},
		{"limit", (*lab).stepLimit},
		{"lostreply", (*lab).stepLostReply},
		{"indexes", (*lab).stepIndexes},
		{"gone", (*lab).stepExternallyDeleted},
		{"reinstall", (*lab).stepReinstall},
		{"killswitch", (*lab).stepKillSwitch},
		{"reprovision", (*lab).stepReprovision},
		{"down", (*lab).stepDown},
	}
	selected := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		if s != "" {
			selected[s] = true
		}
	}
	for _, s := range steps {
		if len(selected) > 0 && !selected[s.name] {
			continue
		}
		l.recorder.step = s.name
		s.run(l)
	}
	if len(selected) == 0 || *fixture != defaultFixture {
		if err := l.recorder.write(*fixture); err != nil {
			fatalf("write contract: %v", err)
		}
		fmt.Printf("       contract: %d exchanges in %s\n", len(l.recorder.exchanges), *fixture)
	}
	fmt.Printf("%d checks, %d failed\n", l.checks, l.failed)
	if l.failed > 0 {
		os.Exit(1)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "netlab: "+format+"\n", args...)
	os.Exit(2)
}

// lab holds one run's clients, its bound slots, and the check count.
type lab struct {
	base     string
	vm       string
	http     *http.Client
	client   *unifi.Client
	recorder *recorder
	checks   int
	failed   int
	slots    map[string]*unifi.Want
	foreign  []string
	ordinal  int
}

func (l *lab) check(ok bool, name string, detail ...any) {
	l.checks++
	if !ok {
		l.failed++
		fmt.Printf("FAIL %s: %s %v\n", l.recorder.step, name, detail)
		return
	}
	suffix := ""
	if len(detail) > 0 {
		suffix = fmt.Sprintf(" %v", detail)
	}
	fmt.Printf("PASS %s: %s%s\n", l.recorder.step, name, suffix)
}

func (l *lab) note(format string, args ...any) { fmt.Printf("       "+format+"\n", args...) }

func (l *lab) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func (l *lab) reconcile(wants ...unifi.Want) (unifi.Report, error) {
	ctx, cancel := l.ctx()
	defer cancel()
	return l.client.Reconcile(ctx, wants)
}

func (l *lab) raw(method, path string, body any) (int, json.RawMessage) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, l.base+path, reader)
	if err != nil {
		fatalf("%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.http.Do(req)
	if err != nil {
		l.check(false, method+" "+path, err)
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// docs is the data array of a legacy answer, or the array of a v2 one.
func docs(raw json.RawMessage) []map[string]any {
	var legacy struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &legacy) == nil && legacy.Data != nil {
		return legacy.Data
	}
	var list []map[string]any
	json.Unmarshal(raw, &list)
	return list
}

func message(raw json.RawMessage) string {
	var a struct {
		Meta struct {
			Msg string `json:"msg"`
		} `json:"meta"`
	}
	json.Unmarshal(raw, &a)
	return a.Meta.Msg
}

const networks = "/api/s/default/rest/networkconf"
const trafficRoutes = "/v2/api/site/default/trafficroutes"
const staticRoutes = "/api/s/default/rest/routing"

func (l *lab) network(id string) map[string]any {
	_, raw := l.raw(http.MethodGet, networks+"/"+id, nil)
	if d := docs(raw); len(d) == 1 {
		return d[0]
	}
	return nil
}

func (l *lab) vpnClients() map[string]map[string]any {
	_, raw := l.raw(http.MethodGet, networks, nil)
	out := map[string]map[string]any{}
	for _, n := range docs(raw) {
		if n["purpose"] == "vpn-client" {
			out[n["_id"].(string)] = n
		}
	}
	return out
}

func keypair() (private, public string) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		fatalf("%v", err)
	}
	return base64.StdEncoding.EncodeToString(k.Bytes()), base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

func (l *lab) want(slot, name string) unifi.Want {
	l.ordinal++
	private, public := keypair()
	_, peer := keypair()
	return unifi.Want{
		Slot: domain.SlotID(slot), Name: name,
		Endpoint:      netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 51830+uint16(l.ordinal)),
		Address:       netip.MustParsePrefix(fmt.Sprintf("10.70.%d.2/30", l.ordinal)),
		DNS:           netip.MustParseAddr("1.1.1.1"),
		PeerPublicKey: peer, PublicKey: public, PrivateKey: domain.NewSecret(private),
	}
}

func clientFields(name, peerIP string, port int, subnet string) map[string]any {
	private, _ := keypair()
	_, peer := keypair()
	return map[string]any{
		"name": name, "purpose": "vpn-client", "vpn_type": "wireguard-client", "enabled": true,
		"wireguard_client_mode": "manual", "wireguard_client_peer_ip": peerIP, "wireguard_client_peer_port": port,
		"wireguard_client_peer_public_key": peer, "x_wireguard_private_key": private,
		"ip_subnet": subnet, "dhcpd_dns_1": "1.1.1.1",
	}
}

func without(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

func with(m map[string]any, key string, value any) map[string]any {
	out := without(m)
	out[key] = value
	return out
}

func (l *lab) stepValidators() {
	_, raw := l.raw(http.MethodGet, networks, nil)
	l.check(len(docs(raw)) == 1, "a fresh app holds only its Default network")
	loopback := clientFields("probe", "127.0.0.1", 51899, "10.71.0.2/30")

	_, raw = l.raw(http.MethodPost, networks, without(loopback, "dhcpd_dns_1"))
	l.check(message(raw) == "api.err.WireguardMissingInterfaceDns", "a client without DNS is refused", message(raw))
	_, raw = l.raw(http.MethodPost, networks, with(loopback, "x_wireguard_private_key", "notakey"))
	l.check(message(raw) == "api.err.WireguardInvalidKey", "a malformed private key is refused", message(raw))
	_, raw = l.raw(http.MethodPost, networks, with(loopback, "wireguard_client_peer_public_key", "notakey"))
	l.check(message(raw) == "api.err.WireguardInvalidKey", "a malformed peer key is refused", message(raw))
	_, raw = l.raw(http.MethodPost, networks, with(loopback, "wireguard_client_peer_ip", ""))
	l.check(message(raw) == "api.err.WireguardMissingPeerData", "a client without a peer is refused", message(raw))

	status, raw := l.raw(http.MethodPost, networks, with(with(loopback, "sidegate_owner", "x"), "note", "x"))
	created := docs(raw)
	l.check(status == 200 && len(created) == 1, "a loopback endpoint and our keys are accepted with no gateway adopted", message(raw))
	if len(created) == 1 {
		id := created[0]["_id"].(string)
		n := l.network(id)
		_, kept1 := n["sidegate_owner"]
		_, kept2 := n["note"]
		l.check(!kept1 && !kept2, "the app silently drops fields it doesn't know, so no custom marker can exist")
		l.note("the app assigned wireguard_id=%v routing_table_id=%v", n["wireguard_id"], n["routing_table_id"])
		_, raw = l.raw(http.MethodPut, networks+"/"+id, map[string]any{"wireguard_client_configuration_filename": "sidegate-probe.conf"})
		n = l.network(id)
		l.check(n["wireguard_client_configuration_filename"] == "sidegate-probe.conf", "manual mode keeps wireguard_client_configuration_filename")
		_, raw = l.raw(http.MethodPost, networks, with(loopback, "ip_subnet", "10.71.0.2/30"))
		l.check(message(raw) == "api.err.SubnetOverlapped", "an overlapping tunnel subnet is refused", message(raw))
		_, raw = l.raw(http.MethodDelete, networks+"/"+id, nil)
		l.check(message(raw) == "", "an unrouted client deletes")
		status, raw = l.raw(http.MethodDelete, networks+"/"+id, nil)
		l.note("deleting an id already gone answers HTTP %d %s", status, message(raw))
	}
}

func (l *lab) stepOwners() {
	for _, f := range []map[string]any{
		clientFields("wgclt6", "198.51.100.7", 443, "10.72.6.2/30"),
		clientFields("Owner Loopback", "127.0.0.1", 51899, "10.72.7.2/30"),
	} {
		status, raw := l.raw(http.MethodPost, networks, f)
		d := docs(raw)
		if status != 200 || len(d) != 1 {
			l.check(false, "owner's client created", status, message(raw))
			continue
		}
		l.foreign = append(l.foreign, d[0]["_id"].(string))
	}
	l.check(len(l.foreign) == 2, "the owner's wgclt6 and a client of theirs on loopback exist")
}

func (l *lab) foreignUntouched() {
	all := l.vpnClients()
	for _, id := range l.foreign {
		n, ok := all[id]
		l.check(ok, "the owner's client still exists", id)
		if _, marked := n["wireguard_client_configuration_filename"]; marked {
			l.check(false, "the owner's client gained a marker", id)
		}
	}
}

func (l *lab) stepCreate() {
	l.slots = map[string]*unifi.Want{}
	for _, s := range []struct{ id, name string }{{"slot-eu", "EU"}, {"slot-us", "US"}, {"slot-as", "Asia"}} {
		w := l.want(s.id, s.name)
		l.slots[s.id] = &w
	}
	report, err := l.reconcile(l.wants()...)
	l.check(err == nil, "reconcile creates three slots", err)
	for _, r := range report.Slots {
		l.check(r.Action == unifi.Created && r.NetworkID != "" && r.Problem == nil, "slot created", r.Slot, r.NetworkID)
		l.slots[string(r.Slot)].Bound = r.NetworkID
	}
	l.check(len(report.Inventory.Foreign) == 2 && report.Inventory.Clients == 5, "the owner's two are counted and not claimed", report.Inventory.Foreign, report.Inventory.Clients)
	for _, w := range l.slots {
		n := l.network(string(w.Bound))
		l.check(n["name"] == w.Name && n["wireguard_client_peer_ip"] == "127.0.0.1", "the app shows it under the slot's name on loopback", w.Name)
	}
	ctx, cancel := l.ctx()
	defer cancel()
	inv, err := l.client.Inspect(ctx)
	l.check(err == nil && len(inv.Owned) == 3 && len(inv.Foreign) == 2, "Inspect claims exactly ours", err, len(inv.Owned), inv.Foreign)
	writes := l.recorder.writes.Load()
	report, err = l.reconcile(l.wants()...)
	l.check(err == nil && l.recorder.writes.Load() == writes, "a second pass writes nothing", err)
	for _, r := range report.Slots {
		l.check(r.Action == unifi.Unchanged, "slot unchanged", r.Slot)
	}
	l.foreignUntouched()
}

func (l *lab) wants() []unifi.Want {
	ids := make([]string, 0, len(l.slots))
	for id := range l.slots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]unifi.Want, 0, len(ids))
	for _, id := range ids {
		out = append(out, *l.slots[id])
	}
	return out
}

func (l *lab) stepRename() {
	eu := l.slots["slot-eu"]
	eu.Name = "Europe"
	report, err := l.reconcile(l.wants()...)
	l.check(err == nil, "reconcile after a rename", err)
	got := resultOf(report, "slot-eu")
	l.check(got.Action == unifi.Updated && got.NetworkID == eu.Bound, "a rename updates the same _id", got.Action, got.NetworkID)
	l.check(l.network(string(eu.Bound))["name"] == "Europe", "the app shows the new name")
}

func resultOf(r unifi.Report, slot string) unifi.SlotResult {
	for _, s := range r.Slots {
		if string(s.Slot) == slot {
			return s
		}
	}
	return unifi.SlotResult{}
}

// stepUISave repeats what the UI's form sends: the object it read, its own fields
// set, the file fields omitted.
func (l *lab) stepUISave() {
	eu := l.slots["slot-eu"]
	doc := without(l.network(string(eu.Bound)), "wireguard_client_configuration_filename")
	doc["name"] = "Renamed In UI"
	doc["mss_clamp"] = "auto"
	status, raw := l.raw(http.MethodPut, networks+"/"+string(eu.Bound), doc)
	l.check(status == 200, "a UI-style save is accepted", message(raw))
	n := l.network(string(eu.Bound))
	l.check(n["wireguard_client_configuration_filename"] == "sidegate-slot-eu.conf", "the marker survives a UI-style save")
	report, err := l.reconcile(l.wants()...)
	got := resultOf(report, "slot-eu")
	l.check(err == nil && got.Action == unifi.Updated, "the slot's name is restored", err, got.Action)
	n = l.network(string(eu.Bound))
	l.check(n["name"] == "Europe" && n["mss_clamp"] == "auto", "only the name changed back; the owner's other field stays", n["name"], n["mss_clamp"])
}

func (l *lab) stepMarkerCleared() {
	us := l.slots["slot-us"]
	l.raw(http.MethodPut, networks+"/"+string(us.Bound), map[string]any{"wireguard_client_configuration_filename": ""})
	report, err := l.reconcile(l.wants()...)
	got := resultOf(report, "slot-us")
	l.check(err == nil && got.Action == unifi.Updated && got.NetworkID == us.Bound, "a cleared marker is restored by _id and peer key", err, got.Action)
	l.check(l.network(string(us.Bound))["wireguard_client_configuration_filename"] == "sidegate-slot-us.conf", "the marker is back")
}

func (l *lab) stepCollision() {
	status, raw := l.raw(http.MethodPost, networks, clientFields("wgclt6", "198.51.100.9", 443, "10.72.8.2/30"))
	d := docs(raw)
	l.check(status == 200 && len(d) == 1, "the app itself accepts a duplicate name", message(raw))
	if len(d) == 1 {
		l.raw(http.MethodDelete, networks+"/"+d[0]["_id"].(string), nil)
	}
	before := l.vpnClients()
	w := l.want("slot-clash", "WGCLT6")
	report, err := l.reconcile(append(l.wants(), w)...)
	got := resultOf(report, "slot-clash")
	l.check(err == nil && got.Problem != nil && got.Problem.Kind == unifi.NameCollision && got.NetworkID == "", "a new slot on the owner's name, in any case, is refused and reported", err, got.Problem)

	asia := l.slots["slot-as"]
	asia.Name = "Default"
	report, err = l.reconcile(l.wants()...)
	got = resultOf(report, "slot-as")
	l.check(err == nil && got.Problem != nil && got.Problem.Kind == unifi.NameCollision && got.NetworkID == asia.Bound, "a rename onto another network's name is refused and the client kept", err, got.Problem)
	l.check(l.network(string(asia.Bound))["name"] == "Asia", "the client keeps its old name")
	asia.Name = "Asia"
	after := l.vpnClients()
	for _, id := range l.foreign {
		l.check(reflect.DeepEqual(before[id], after[id]), "the owner's client is byte-identical", id)
	}
}

func (l *lab) routeDoc(target, description string) map[string]any {
	return map[string]any{
		"description": description, "enabled": true, "matching_target": "DOMAIN",
		"domains":    []any{map[string]any{"domain": "example.com", "port_ranges": []any{}, "ports": []any{}}},
		"network_id": target, "next_hop": "", "kill_switch_enabled": true,
		"ip_addresses": []any{}, "ip_ranges": []any{}, "regions": []any{},
		"target_devices": []any{map[string]any{"type": "ALL_CLIENTS"}},
	}
}

func staticDoc(target, name string) map[string]any {
	return map[string]any{
		"name": name, "enabled": true, "type": "static-route", "static-route_type": "interface-route",
		"static-route_network": "203.0.113.0/24", "static-route_interface": target, "static-route_distance": 1, "gateway_type": "default",
	}
}

func (l *lab) stepRouted() {
	us := l.slots["slot-us"]
	status, raw := l.raw(http.MethodPost, trafficRoutes, l.routeDoc(string(us.Bound), "lab traffic route"))
	var route map[string]any
	json.Unmarshal(raw, &route)
	l.check(status == 201 && route["network_id"] == string(us.Bound), "a Policy Table route can target our client by the API", status)
	routeID, _ := route["_id"].(string)
	status, raw = l.raw(http.MethodPost, staticRoutes, staticDoc(string(us.Bound), "lab static route"))
	static := docs(raw)
	l.check(status == 200 && len(static) == 1, "a static route can target our client too", status, message(raw))

	ctx, cancel := l.ctx()
	inv, err := l.client.Inspect(ctx)
	cancel()
	var seen int
	for _, o := range inv.Owned {
		if o.NetworkID == us.Bound {
			seen = len(o.Routes)
		}
	}
	l.check(err == nil && seen == 2, "Inspect finds both routes on the slot", err, seen)

	delete(l.slots, "slot-us")
	report, err := l.reconcile(l.wants()...)
	l.check(err == nil && len(report.Retained) == 1 && report.Retained[0].Problem.Kind == unifi.RoutedElsewhere && len(report.Retained[0].Problem.Routes) == 2,
		"deleting a slot that routes target is refused and names the routes", err, report.Retained)
	l.check(l.network(string(us.Bound)) != nil, "the client is still there")
	_, raw = l.raw(http.MethodGet, trafficRoutes, nil)
	l.check(len(docs(raw)) == 1, "the owner's route is still there")

	if routeID != "" {
		l.raw(http.MethodDelete, trafficRoutes+"/"+routeID, nil)
	}
	if len(static) == 1 {
		l.raw(http.MethodDelete, staticRoutes+"/"+static[0]["_id"].(string), nil)
	}
	report, err = l.reconcile(l.wants()...)
	l.check(err == nil && len(report.Removed) == 1 && report.Removed[0] == us.Bound, "once the routes are gone the client deletes", err, report.Removed)
	l.check(len(l.vpnClients()) == 4, "and the owner's two remain beside our two")
	l.foreignUntouched()
}

// stepCascade shows why the refusal above can't be left to the app.
func (l *lab) stepCascade() {
	disposable := func(name string, last int) string {
		_, raw := l.raw(http.MethodPost, networks, clientFields(name, "127.0.0.1", 51890+last, fmt.Sprintf("10.73.%d.2/30", last)))
		d := docs(raw)
		if len(d) != 1 {
			l.check(false, "disposable client created", message(raw))
			return ""
		}
		return d[0]["_id"].(string)
	}
	id := disposable("disposable traffic", 1)
	l.raw(http.MethodPost, trafficRoutes, l.routeDoc(id, "disposable route"))
	status, raw := l.raw(http.MethodDelete, networks+"/"+id, nil)
	l.check(status == 200 && message(raw) == "", "the app answers ok to deleting a routed client", status, message(raw))
	took := l.waitNoTrafficRoutes()
	_, list := l.raw(http.MethodGet, trafficRoutes, nil)
	l.check(len(docs(list)) == 0, "and then deletes the owner's traffic route with it", took.Round(100*time.Millisecond))

	id = disposable("disposable static", 2)
	l.raw(http.MethodPost, staticRoutes, staticDoc(id, "disposable static"))
	status, raw = l.raw(http.MethodDelete, networks+"/"+id, nil)
	_, list = l.raw(http.MethodGet, staticRoutes, nil)
	l.check(status == 400 && message(raw) == "api.err.ResourceReferredBy" && len(docs(list)) == 1, "a client with a static route is refused instead", status, message(raw))
	for _, s := range docs(list) {
		l.raw(http.MethodDelete, staticRoutes+"/"+s["_id"].(string), nil)
	}
	if status != 200 {
		l.raw(http.MethodDelete, networks+"/"+id, nil)
	}
}

// waitNoTrafficRoutes polls off the record, because the app removes a route some
// time after it answers and the contract must replay the same on every run.
func (l *lab) waitNoTrafficRoutes() time.Duration {
	l.recorder.paused.Store(true)
	defer l.recorder.paused.Store(false)
	start := time.Now()
	for time.Since(start) < 2*time.Minute {
		_, raw := l.raw(http.MethodGet, trafficRoutes, nil)
		if len(docs(raw)) == 0 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	return time.Since(start)
}

func (l *lab) stepLimit() {
	for i := len(l.slots); i < 7; i++ {
		id := fmt.Sprintf("slot-fill-%d", i)
		w := l.want(id, fmt.Sprintf("Fill %d", i))
		l.slots[id] = &w
	}
	report, err := l.reconcile(l.wants()...)
	l.check(err == nil, "reconcile with seven slots beside the owner's two", err)
	var created, limited int
	for _, r := range report.Slots {
		if r.Action == unifi.Created {
			created++
			l.slots[string(r.Slot)].Bound = r.NetworkID
		}
		if r.Problem != nil && r.Problem.Kind == unifi.LimitReached {
			limited++
			l.note("%s: %v", r.Slot, r.Problem)
			delete(l.slots, string(r.Slot))
		}
	}
	l.check(report.Inventory.Clients == unifi.Limit && limited == 1, "the eighth client is the last, and the slot past it is reported", report.Inventory.Clients, limited)
	status, raw := l.raw(http.MethodPost, networks, clientFields("over the limit", "127.0.0.1", 51899, "10.74.0.2/30"))
	d := docs(raw)
	l.check(status == 200 && len(d) == 1, "the app's API itself accepts a ninth, so the client must enforce the limit", status, message(raw))
	if len(d) == 1 {
		l.raw(http.MethodDelete, networks+"/"+d[0]["_id"].(string), nil)
	}
	for id := range l.slots {
		if strings.HasPrefix(id, "slot-fill") {
			delete(l.slots, id)
		}
	}
	report, err = l.reconcile(l.wants()...)
	l.check(err == nil && report.Inventory.Clients == 4, "dropping the fill slots frees them", err, report.Inventory.Clients)
}

func (l *lab) stepLostReply() {
	w := l.want("slot-lost", "Lost Reply")
	l.recorder.loseNextCreate.Store(true)
	report, err := l.reconcile(append(l.wants(), w)...)
	got := resultOf(report, "slot-lost")
	var e *unifi.Error
	l.check(errors.As(err, &e) && e.Kind == unifi.Unavailable && got.NetworkID == "", "a POST whose reply is lost reports Unavailable", err)
	before := len(l.vpnClients())
	report, err = l.reconcile(append(l.wants(), w)...)
	got = resultOf(report, "slot-lost")
	l.check(err == nil && got.Action == unifi.Adopted && got.NetworkID != "", "the next pass adopts the object by its marker", err, got.Action)
	l.check(len(l.vpnClients()) == before, "and creates no second one")
	report, err = l.reconcile(l.wants()...)
	l.check(err == nil && len(report.Removed) == 1, "dropping the slot removes it", err)
}

func (l *lab) stepIndexes() {
	a := l.want("slot-idx-a", "Idx A")
	b := l.want("slot-idx-b", "Idx B")
	report, err := l.reconcile(append(l.wants(), a, b)...)
	l.check(err == nil, "two more slots", err)
	a.Bound = resultOf(report, "slot-idx-a").NetworkID
	b.Bound = resultOf(report, "slot-idx-b").NetworkID
	deleted := l.network(string(a.Bound))["wireguard_id"]
	before := l.network(string(b.Bound))
	report, err = l.reconcile(append(l.wants(), b)...)
	l.check(err == nil && len(report.Removed) == 1, "the earlier of them deleted", err)
	after := l.network(string(b.Bound))
	l.check(reflect.DeepEqual(before["wireguard_id"], after["wireguard_id"]) && reflect.DeepEqual(before["routing_table_id"], after["routing_table_id"]),
		"deleting a client leaves the others' wireguard_id and routing_table_id", after["wireguard_id"], after["routing_table_id"])
	c := l.want("slot-idx-c", "Idx C")
	report, err = l.reconcile(append(l.wants(), b, c)...)
	newID := resultOf(report, "slot-idx-c").NetworkID
	n := l.network(string(newID))
	l.note("a client created after that delete took wireguard_id=%v, not the freed %v", n["wireguard_id"], deleted)
	report, err = l.reconcile(l.wants()...)
	l.check(err == nil && len(report.Removed) == 2, "both removed", err)
}

func (l *lab) stepExternallyDeleted() {
	w := l.want("slot-gone", "Gone")
	report, err := l.reconcile(append(l.wants(), w)...)
	w.Bound = resultOf(report, "slot-gone").NetworkID
	l.check(err == nil && w.Bound != "", "a slot to lose", err)
	l.raw(http.MethodDelete, networks+"/"+string(w.Bound), nil)
	before := len(l.vpnClients())
	report, err = l.reconcile(append(l.wants(), w)...)
	got := resultOf(report, "slot-gone")
	l.check(err == nil && got.Problem != nil && got.Problem.Kind == unifi.ExternallyDeleted, "a bound client deleted in UniFi is reported", err, got.Problem)
	l.check(len(l.vpnClients()) == before, "and not silently re-created")
}

func (l *lab) stepReinstall() {
	w := l.want("slot-old-install", "Household")
	report, err := l.reconcile(append(l.wants(), w)...)
	id := resultOf(report, "slot-old-install").NetworkID
	l.check(err == nil && id != "", "a slot from the install that loses /data", err)
	_, raw := l.raw(http.MethodPost, trafficRoutes, l.routeDoc(string(id), "household route"))
	var route map[string]any
	json.Unmarshal(raw, &route)
	fresh := l.want("slot-new-install", "Household")
	report, err = l.reconcile(append(l.wants(), fresh)...)
	got := resultOf(report, "slot-new-install")
	l.check(err == nil && got.Action == unifi.Adopted && got.NetworkID == id, "a new install's slot of the same name takes the same _id", err, got.Action, got.NetworkID)
	n := l.network(string(id))
	l.check(n["wireguard_client_peer_public_key"] == fresh.PeerPublicKey && n["wireguard_client_configuration_filename"] == "sidegate-slot-new-install.conf", "with its new keys and marker")
	_, list := l.raw(http.MethodGet, trafficRoutes, nil)
	l.check(len(docs(list)) == 1 && docs(list)[0]["network_id"] == string(id), "and the owner's route still targets it")
	l.raw(http.MethodDelete, trafficRoutes+"/"+route["_id"].(string), nil)
	report, err = l.reconcile(l.wants()...)
	l.check(err == nil && len(report.Removed) == 1, "cleanup", err)
}

// stepKillSwitch records how routes hold the kill switch the page reads. Nothing in this
// product writes it. A question with no assumed answer is a note, so the recording shows
// what the app does whichever way it goes.
func (l *lab) stepKillSwitch() {
	target := string(l.slots["slot-eu"].Bound)
	post := func(doc map[string]any) string {
		status, raw := l.raw(http.MethodPost, trafficRoutes, doc)
		var route map[string]any
		json.Unmarshal(raw, &route)
		id, _ := route["_id"].(string)
		l.check(status == 201 && id != "", "a route for the kill switch exists", doc["description"], status)
		return id
	}
	route := func(id string) map[string]any {
		_, raw := l.raw(http.MethodGet, trafficRoutes, nil)
		for _, d := range docs(raw) {
			if d["_id"] == id {
				return d
			}
		}
		return nil
	}
	blocks := post(l.routeDoc(target, "kill switch on"))
	disabled := post(with(with(l.routeDoc(target, "kill switch off, disabled"), "enabled", false), "kill_switch_enabled", false))
	lacking := post(without(l.routeDoc(target, "kill switch not sent"), "kill_switch_enabled"))
	if blocks == "" || disabled == "" || lacking == "" {
		return
	}
	got := route(lacking)
	_, stated := got["kill_switch_enabled"]
	l.note("a route created without the field reads back with it: %v, value %v", stated, got["kill_switch_enabled"])

	_, raw := l.raw(http.MethodPost, staticRoutes, staticDoc(target, "kill switch static"))
	static := docs(raw)
	_, raw = l.raw(http.MethodGet, staticRoutes, nil)
	for _, s := range docs(raw) {
		_, has := s["kill_switch_enabled"]
		l.check(!has, "a static route has no kill switch field", s["name"])
	}

	_, raw = l.raw(http.MethodPost, networks, with(clientFields("Owner Disabled", "198.51.100.11", 443, "10.72.9.2/30"), "enabled", false))
	if d := docs(raw); len(d) == 1 {
		id := d[0]["_id"].(string)
		l.check(l.network(id)["enabled"] == false, "an owner's disabled client reads enabled false")
		l.raw(http.MethodDelete, networks+"/"+id, nil)
	}

	for _, id := range []string{blocks, disabled, lacking} {
		l.raw(http.MethodDelete, trafficRoutes+"/"+id, nil)
	}
	if len(static) == 1 {
		l.raw(http.MethodDelete, staticRoutes+"/"+static[0]["_id"].(string), nil)
	}
	_, list := l.raw(http.MethodGet, trafficRoutes, nil)
	l.check(len(docs(list)) == 0, "the lab's routes are gone")
}

func (l *lab) vmSh(script string) (string, error) {
	out, err := exec.Command("limactl", "shell", "--workdir", "/", l.vm, "sudo", "bash", "-c", script).CombinedOutput()
	return string(out), err
}

// stepReprovision adopts a simulated UXG over inform on the VM's loopback. A
// gateway makes the app provision, so ours must read back the same afterwards.
// A UXG stands in because this app won't list a console model such as UDRULT.
func (l *lab) stepReprovision() {
	const mac = "02:ab:cd:00:00:15"
	sim := func(rounds int) string {
		return fmt.Sprintf("/opt/unifi-lab-net/informsim -model UXG -version 5.1.26.33914 -mac %s -out /tmp/inf -rounds %d -every 2s", mac, rounds)
	}
	if out, err := l.vmSh("rm -rf /tmp/inf; timeout 20 " + sim(1)); err != nil {
		l.check(false, "announce the simulated gateway", err, out)
		return
	}
	before := l.vpnClients()
	status, raw := l.raw(http.MethodPost, "/api/s/default/cmd/devmgr", map[string]any{"cmd": "adopt", "mac": mac})
	l.check(status == 200, "the app accepts adopting the simulated gateway", status, message(raw))
	out, err := l.vmSh("timeout 40 " + sim(15) + " 2>&1 | tail -2; ls /tmp/inf | wc -l; grep -il -e wgclt -e wireguard /tmp/inf/* | wc -l")
	l.check(err == nil, "the simulated gateway informs", err)
	l.note("inform log tail and counts: %s", strings.ReplaceAll(strings.TrimSpace(out), "\n", " | "))
	_, raw = l.raw(http.MethodGet, "/api/s/default/stat/device", nil)
	adopted := false
	for _, d := range docs(raw) {
		if d["mac"] == mac && d["adopted"] == true {
			adopted = true
			l.note("simulated gateway: model=%v state=%v version=%v", d["model"], d["state"], d["version"])
		}
	}
	l.check(adopted, "the app holds it adopted")
	status, raw = l.raw(http.MethodPost, "/api/s/default/cmd/devmgr", map[string]any{"cmd": "force-provision", "mac": mac})
	l.note("force-provision answers HTTP %d %s, so inform can't show a UXG's rendered config", status, message(raw))
	_, raw = l.raw(http.MethodGet, "/v2/api/site/default/vpn/connections", nil)
	l.note("v2 vpn/connections with a gateway: %s", summarizeConnections(raw))

	after := l.vpnClients()
	for id, n := range before {
		l.check(reflect.DeepEqual(n, after[id]), "adopting a gateway changed no field of the client", n["name"])
	}
	writes := l.recorder.writes.Load()
	_, err = l.reconcile(l.wants()...)
	l.check(err == nil && l.recorder.writes.Load() == writes, "a pass with a gateway adopted writes nothing", err)
	w := l.want("slot-with-gw", "With Gateway")
	report, err := l.reconcile(append(l.wants(), w)...)
	got := resultOf(report, "slot-with-gw")
	l.check(err == nil && got.Action == unifi.Created, "a loopback client creates with a gateway adopted too", err, got.Problem)
	report, err = l.reconcile(l.wants()...)
	l.check(err == nil && len(report.Removed) == 1, "and deletes", err)

	before = l.vpnClients()
	if out, err := l.vmSh("systemctl restart unifi"); err != nil {
		l.check(false, "restart the app", err, out)
	}
	l.waitUp()
	after = l.vpnClients()
	for id, n := range before {
		l.check(reflect.DeepEqual(n, after[id]), "an app restart changed no field of the client", n["name"])
	}
}

func summarizeConnections(raw json.RawMessage) string {
	var a struct {
		Connections []struct {
			NetworkID string `json:"network_id"`
			Status    string `json:"status"`
		} `json:"connections"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return "unreadable"
	}
	counts := map[string]int{}
	for _, c := range a.Connections {
		counts[c.Status]++
	}
	return fmt.Sprint(counts)
}

func (l *lab) waitUp() {
	for range 90 {
		req, _ := http.NewRequest(http.MethodGet, l.base+"/status", nil)
		resp, err := l.recorder.session().inner.RoundTrip(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if bytes.Contains(b, []byte(`"up":true`)) {
				return
			}
		}
		time.Sleep(3 * time.Second)
	}
	l.check(false, "the app came back up")
}

func (l *lab) stepDown() {
	if out, err := l.vmSh("systemctl stop unifi"); err != nil {
		l.check(false, "stop the app", err, out)
		return
	}
	wants := l.wants()
	start := time.Now()
	report, err := l.reconcile(wants...)
	took := time.Since(start)
	var e *unifi.Error
	l.check(errors.As(err, &e) && e.Kind == unifi.Unavailable, "with the app down, reconcile reports Unavailable", err)
	l.check(len(report.Slots) == 0 && took < 15*time.Second, "changes no binding and returns within a request timeout", took.Round(time.Millisecond))
	ctx, cancel := l.ctx()
	_, err = l.client.Inspect(ctx)
	cancel()
	l.check(errors.As(err, &e) && e.Kind == unifi.Unavailable, "Inspect reports the same", err)
	if out, err := l.vmSh("systemctl start unifi"); err != nil {
		l.check(false, "start the app", err, out)
	}
	l.waitUp()
	writes := l.recorder.writes.Load()
	report, err = l.reconcile(wants...)
	l.check(err == nil && l.recorder.writes.Load() == writes, "once it returns, a pass converges with no writes", err)
	for _, r := range report.Slots {
		l.check(r.NetworkID == l.slots[string(r.Slot)].Bound && r.Problem == nil, "slot keeps its _id", r.Slot)
	}
}

// session is the lab's stand-in for the API key. The app asks UniFi Core about
// keys, and a plain Network Application has no UniFi Core.
type session struct {
	inner    http.RoundTripper
	base     string
	password string
	path     string
	jar      *cookiejar.Jar
}

func newSession(base, path string) *session {
	jar, _ := cookiejar.New(nil)
	return &session{
		inner: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		base:  base, path: path, jar: jar,
	}
}

func (s *session) send(req *http.Request) (*http.Response, error) {
	for _, c := range s.jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	resp, err := s.inner.RoundTrip(req)
	if err == nil {
		if cookies := resp.Cookies(); len(cookies) > 0 {
			s.jar.SetCookies(req.URL, cookies)
		}
	}
	return resp, err
}

func (s *session) login() error {
	body, _ := json.Marshal(map[string]string{"username": "lab", "password": s.password})
	req, _ := http.NewRequest(http.MethodPost, s.base+"/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("lab login: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *session) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	attempt := func() (*http.Response, error) {
		r := req.Clone(req.Context())
		r.Header.Del("X-API-KEY")
		r.Body = io.NopCloser(bytes.NewReader(body))
		return s.send(r)
	}
	resp, err := attempt()
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	resp.Body.Close()
	if err := s.login(); err != nil {
		return nil, err
	}
	return attempt()
}

// bootstrap creates the lab's admin on a fresh app, or logs in to an existing one.
func (s *session) bootstrap(l *lab) {
	raw, err := os.ReadFile(s.path)
	if err == nil {
		s.password = strings.TrimSpace(string(raw))
		if s.login() == nil {
			return
		}
	}
	b := make([]byte, 16)
	rand.Read(b)
	s.password = hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		fatalf("%v", err)
	}
	if err := os.WriteFile(s.path, []byte(s.password), 0o600); err != nil {
		fatalf("%v", err)
	}
	post := func(path string, body map[string]any) {
		encoded, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, s.base+path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.send(req)
		if err != nil {
			fatalf("bootstrap %s: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			fatalf("bootstrap %s: HTTP %d", path, resp.StatusCode)
		}
	}
	post("/api/cmd/sitemgr", map[string]any{"cmd": "add-default-admin", "name": "lab", "email": "lab@example.invalid", "x_password": s.password})
	post("/api/cmd/system", map[string]any{"cmd": "set-installed"})
	if err := s.login(); err != nil {
		fatalf("%v", err)
	}
}

// exchange is one recorded request and answer, sanitized.
type exchange struct {
	Step     string          `json:"step"`
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Request  json.RawMessage `json:"request,omitempty"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

// recorder keeps every exchange and can lose the reply to the next create.
type recorder struct {
	inner          *session
	step           string
	writes         atomic.Int64
	paused         atomic.Bool
	loseNextCreate atomic.Bool
	exchanges      []exchange
	ids            map[string]string
	keys           map[string]string
}

func (r *recorder) session() *session { return r.inner }

var (
	objectID = regexp.MustCompile(`\b[0-9a-f]{24}\b`)
	uuidText = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	wgKey    = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
)

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	if req.Method != http.MethodGet {
		r.writes.Add(1)
	}
	resp, err := r.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(answer))
	if r.step != "reprovision" && r.step != "down" && !r.paused.Load() {
		r.record(req, body, resp.StatusCode, answer)
	}
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/rest/networkconf") && r.loseNextCreate.CompareAndSwap(true, false) {
		return nil, errors.New("reply lost by the lab")
	}
	return resp, nil
}

func (r *recorder) record(req *http.Request, body []byte, status int, answer []byte) {
	if r.ids == nil {
		r.ids, r.keys = map[string]string{}, map[string]string{}
	}
	e := exchange{Step: r.step, Method: req.Method, Status: status}
	e.Response = r.sanitize(answer)
	e.Path = objectID.ReplaceAllStringFunc(req.URL.Path, r.id)
	if len(body) > 0 {
		e.Request = r.sanitize(body)
	}
	r.exchanges = append(r.exchanges, e)
}

func (r *recorder) id(raw string) string {
	id, ok := r.ids[raw]
	if !ok {
		id = fmt.Sprintf("id-%04d", len(r.ids)+1)
		r.ids[raw] = id
	}
	return id
}

func (r *recorder) sanitize(raw []byte) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return json.RawMessage(`"<not json>"`)
	}
	v = r.clean("", v)
	out, _ := json.Marshal(v)
	return out
}

// clean walks in key order so an id's number depends only on the exchange.
func (r *recorder) clean(key string, v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := map[string]any{}
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out[k] = r.clean(k, value[k])
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = r.clean(key, value[i])
		}
		return out
	case string:
		switch {
		case key == "site_id":
			return "<site>"
		case key == "external_id" || uuidText.MatchString(value):
			return "<uuid>"
		case key == "x_password":
			return "<password>"
		case wgKey.MatchString(value):
			k, ok := r.keys[value]
			if !ok {
				k = fmt.Sprintf("<key-%d>", len(r.keys)+1)
				r.keys[value] = k
			}
			return k
		case objectID.MatchString(value) && len(value) == 24:
			return r.id(value)
		}
	}
	return v
}

func (r *recorder) write(path string) error {
	var out bytes.Buffer
	out.WriteString("[\n")
	for i, e := range r.exchanges {
		encoded, err := json.Marshal(e)
		if err != nil {
			return err
		}
		out.Write(encoded)
		if i < len(r.exchanges)-1 {
			out.WriteByte(',')
		}
		out.WriteByte('\n')
	}
	out.WriteString("]\n")
	return os.WriteFile(path, out.Bytes(), 0o644)
}
