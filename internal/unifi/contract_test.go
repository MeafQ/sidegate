package unifi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// exchange is one line of testdata/contract.json, which lab/net/verify.sh records
// from Network Application 10.6.109 and sanitizes.
type exchange struct {
	Step     string          `json:"step"`
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Request  json.RawMessage `json:"request"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

func contract(t *testing.T) []exchange {
	t.Helper()
	raw, err := os.ReadFile("testdata/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var exchanges []exchange
	if err := json.Unmarshal(raw, &exchanges); err != nil {
		t.Fatal(err)
	}
	if len(exchanges) == 0 {
		t.Fatal("empty contract")
	}
	return exchanges
}

type sentWrite struct {
	Method, Path string
	Body         []byte
}

// fakeApp behaves as the recording shows the real app behaves. TestFakeMatchesRecording
// replays every recorded exchange against it, so the other tests can't drift from it.
type fakeApp struct {
	mu       sync.Mutex
	nextID   int
	networks []map[string]any
	traffic  []map[string]any
	static   []map[string]any
	policy   []map[string]any
	writes   int
	sent     []sentWrite
	keys     []string
	// intercept answers a request instead of the app when it returns true.
	intercept func(w http.ResponseWriter, r *http.Request) bool
}

func seededApp(t *testing.T) *fakeApp {
	t.Helper()
	var first struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(contract(t)[0].Response, &first); err != nil || len(first.Data) != 1 {
		t.Fatalf("the contract must open with the fresh app's one network: %v", err)
	}
	return &fakeApp{nextID: 1, networks: first.Data}
}

func (f *fakeApp) id() string {
	f.nextID++
	return fmt.Sprintf("id-%04d", f.nextID)
}

func legacyAnswer(w http.ResponseWriter, status int, docs []map[string]any, meta map[string]any) {
	if docs == nil {
		docs = []map[string]any{}
	}
	if meta == nil {
		meta = map[string]any{"rc": "ok"}
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"meta": meta, "data": docs})
}

func refuse(w http.ResponseWriter, msg string, extra ...string) {
	meta := map[string]any{"rc": "error", "msg": msg}
	for i := 0; i+1 < len(extra); i += 2 {
		meta[extra[i]] = extra[i+1]
	}
	legacyAnswer(w, http.StatusBadRequest, nil, meta)
}

var fakeKey = regexp.MustCompile(`^(<key-\d+>|[A-Za-z0-9+/]{43}=)$`)

// validate returns false after answering, as the real validators do.
func (f *fakeApp) validate(w http.ResponseWriter, doc map[string]any, self string) bool {
	if doc["purpose"] != purposeVPNClient {
		return true
	}
	for _, field := range []string{"x_wireguard_private_key", "wireguard_client_peer_public_key"} {
		if s, _ := doc[field].(string); s != "" && !fakeKey.MatchString(s) {
			refuse(w, "api.err.WireguardInvalidKey", "field_name", field)
			return false
		}
	}
	if s, _ := doc["wireguard_client_peer_ip"].(string); s == "" {
		refuse(w, "api.err.WireguardMissingPeerData", "field_name", "wireguard_client_peer_ip")
		return false
	}
	if s, _ := doc["dhcpd_dns_1"].(string); s == "" {
		refuse(w, "api.err.WireguardMissingInterfaceDns")
		return false
	}
	subnet, _ := doc["ip_subnet"].(string)
	mine, err := netip.ParsePrefix(subnet)
	if err != nil {
		return true
	}
	for _, n := range f.networks {
		other, _ := n["ip_subnet"].(string)
		if theirs, err := netip.ParsePrefix(other); err == nil && n["_id"] != self && theirs.Overlaps(mine) {
			refuse(w, "api.err.SubnetOverlapped", "ip_subnet", subnet, "name", fmt.Sprint(doc["name"]))
			return false
		}
	}
	return true
}

func (f *fakeApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.intercept != nil && f.intercept(w, r) {
		return
	}
	f.keys = append(f.keys, r.Header.Get("X-API-KEY"))
	var body map[string]any
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		f.writes++
		f.sent = append(f.sent, sentWrite{r.Method, r.URL.Path, raw})
	}
	if r.Method == http.MethodDelete {
		f.writes++
	}
	path := r.URL.Path
	switch rest, _ := strings.CutPrefix(path, "/api/s/default/rest/"); {
	case rest == "networkconf" && r.Method == http.MethodGet:
		legacyAnswer(w, 200, f.networks, nil)
	case rest == "networkconf" && r.Method == http.MethodPost:
		f.createNetwork(w, body)
	case strings.HasPrefix(rest, "networkconf/"):
		f.oneNetwork(w, r.Method, strings.TrimPrefix(rest, "networkconf/"), body)
	case rest == "routing" && r.Method == http.MethodGet:
		legacyAnswer(w, 200, f.static, nil)
	case rest == "routing" && r.Method == http.MethodPost:
		body["_id"], body["site_id"] = f.id(), "<site>"
		f.static = append(f.static, body)
		legacyAnswer(w, 200, []map[string]any{body}, nil)
	case strings.HasPrefix(rest, "routing/"):
		id := strings.TrimPrefix(rest, "routing/")
		f.static = slices.DeleteFunc(f.static, func(d map[string]any) bool { return d["_id"] == id })
		legacyAnswer(w, 200, nil, nil)
	case path == "/v2/api/site/default/trafficroutes" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(orEmpty(f.traffic))
	case path == "/v2/api/site/default/trafficroutes" && r.Method == http.MethodPost:
		body["_id"] = f.id()
		defaultKillSwitch(body)
		f.traffic = append(f.traffic, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(body)
	case strings.HasPrefix(path, "/v2/api/site/default/trafficroutes/"):
		id := strings.TrimPrefix(path, "/v2/api/site/default/trafficroutes/")
		f.traffic = slices.DeleteFunc(f.traffic, func(d map[string]any) bool { return d["_id"] == id })
	case path == "/v2/api/site/default/object-oriented-network-configs":
		json.NewEncoder(w).Encode(orEmpty(f.policy))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// defaultKillSwitch is what the recording shows of a route sent without the field.
func defaultKillSwitch(doc map[string]any) {
	if _, stated := doc["kill_switch_enabled"]; !stated {
		doc["kill_switch_enabled"] = false
	}
}

func orEmpty(docs []map[string]any) []map[string]any {
	if docs == nil {
		return []map[string]any{}
	}
	return docs
}

// The app drops what it doesn't know. These are the two the contract tries.
func dropUnknown(doc map[string]any) {
	delete(doc, "sidegate_owner")
	delete(doc, "note")
}

func (f *fakeApp) createNetwork(w http.ResponseWriter, doc map[string]any) {
	if !f.validate(w, doc, "") {
		return
	}
	dropUnknown(doc)
	doc["_id"] = f.id()
	doc["site_id"], doc["external_id"] = "<site>", "<uuid>"
	if doc["purpose"] == purposeVPNClient {
		// The recording shows wireguard_id as highest plus one, and the table as lowest free.
		highest := 0.0
		tables := map[float64]bool{}
		for _, n := range f.networks {
			if v, ok := n["wireguard_id"].(float64); ok && v > highest {
				highest = v
			}
			if v, ok := n["routing_table_id"].(float64); ok {
				tables[v] = true
			}
		}
		doc["wireguard_id"] = highest + 1
		table := 178.0
		for tables[table] {
			table++
		}
		if doc["enabled"] != false {
			doc["routing_table_id"] = table
		}
	}
	f.networks = append(f.networks, doc)
	legacyAnswer(w, 200, []map[string]any{doc}, nil)
}

func (f *fakeApp) oneNetwork(w http.ResponseWriter, method, id string, body map[string]any) {
	at := slices.IndexFunc(f.networks, func(d map[string]any) bool { return d["_id"] == id })
	if at < 0 {
		refuse(w, "api.err.IdInvalid")
		return
	}
	switch method {
	case http.MethodGet:
		legacyAnswer(w, 200, []map[string]any{f.networks[at]}, nil)
	case http.MethodPut:
		merged := map[string]any{}
		for k, v := range f.networks[at] {
			merged[k] = v
		}
		for k, v := range body {
			merged[k] = v
		}
		dropUnknown(merged)
		if !f.validate(w, merged, id) {
			return
		}
		f.networks[at] = merged
		legacyAnswer(w, 200, []map[string]any{merged}, nil)
	case http.MethodDelete:
		var refs []any
		for _, d := range f.static {
			if d["static-route_interface"] == id {
				refs = append(refs, map[string]any{
					"reference_type": "NETWORK_IS_USED_IN_ROUTING_AS_INTERFACE_ROUTE", "resource": d, "resource_collection": "routing",
				})
			}
		}
		if refs != nil {
			legacyAnswer(w, http.StatusBadRequest, nil, map[string]any{
				"rc": "error", "msg": "api.err.ResourceReferredBy", "reference_resources": refs,
				"referred_resource_id": id, "referred_resource_type": "HbbPiWRMjwyCNlRKri",
			})
			return
		}
		f.traffic = slices.DeleteFunc(f.traffic, func(d map[string]any) bool { return d["network_id"] == id })
		f.networks = slices.Delete(f.networks, at, at+1)
		legacyAnswer(w, 200, nil, nil)
	}
}

// refusalNotCompared stands in for the app's text on a v2 400 that has no code. That
// text names obfuscated Java classes, and nothing in the client reads it.
const refusalNotCompared = "the fake does not repeat the app's text"

func canonical(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "<not json>"
	}
	if answer, ok := v.(map[string]any); ok && answer["errorCode"] != nil && answer["code"] == nil {
		answer["message"] = refusalNotCompared
	}
	return v
}

// TestFakeMatchesRecording holds the fake to what the real app answered.
func TestFakeMatchesRecording(t *testing.T) {
	app := seededApp(t)
	for i, e := range contract(t) {
		var body io.Reader
		if len(e.Request) > 0 {
			body = bytes.NewReader(e.Request)
		}
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(e.Method, e.Path, body))
		name := fmt.Sprintf("#%d %s: %s %s", i, e.Step, e.Method, e.Path)
		if rec.Code != e.Status {
			t.Fatalf("%s: status %d, the app answered %d\nfake: %s\napp:  %s", name, rec.Code, e.Status, rec.Body, e.Response)
		}
		got := canonical(t, rec.Body.Bytes())
		if !reflect.DeepEqual(got, canonical(t, e.Response)) {
			t.Fatalf("%s:\nfake: %s\napp:  %s", name, rec.Body, e.Response)
		}
	}
}

// TestCreationMatchesRecording checks that what the client sends today is what
// the app was recorded accepting.
func TestCreationMatchesRecording(t *testing.T) {
	var recorded map[string]any
	for _, e := range contract(t) {
		if e.Step == "create" && e.Method == http.MethodPost && e.Status == 200 {
			if err := json.Unmarshal(e.Request, &recorded); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if recorded == nil {
		t.Fatal("the contract has no recorded create")
	}
	sent := map[string]any{}
	encoded, _ := json.Marshal(creation(testWant("slot-a", "A", 1)))
	json.Unmarshal(encoded, &sent)
	var sentKeys, recordedKeys []string
	for k := range sent {
		sentKeys = append(sentKeys, k)
	}
	for k := range recorded {
		recordedKeys = append(recordedKeys, k)
	}
	slices.Sort(sentKeys)
	slices.Sort(recordedKeys)
	if !slices.Equal(sentKeys, recordedKeys) {
		t.Fatalf("the client sends %v, the app was recorded accepting %v", sentKeys, recordedKeys)
	}
}

// TestContractIsSanitized guards the checked-in recording against real values.
func TestContractIsSanitized(t *testing.T) {
	raw, err := os.ReadFile("testdata/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{
		`[A-Za-z0-9+/]{43}=`, `\b[0-9a-f]{24}\b`,
		`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`,
		`10\.0\.10\.`, `x_password`, `X-API-KEY`,
	} {
		if m := regexp.MustCompile(pattern).Find(raw); m != nil {
			t.Errorf("contract holds %q, matching %s", m, pattern)
		}
	}
}
