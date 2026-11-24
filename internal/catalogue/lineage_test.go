package catalogue

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
)

const (
	source = domain.SourceID("source-a")
	grace  = 24 * time.Hour
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// vless builds one share link; kv overrides the defaults by parameter name.
func vless(label string, kv ...string) string {
	p := map[string]string{
		"id": linkUUID, "host": "de1.example.net", "port": "443", "type": "tcp", "security": "reality",
		"pbk": realityKey, "sid": "63ecb9e95ef79ad3", "sni": "www.example.com", "fp": "chrome",
	}
	for i := 0; i < len(kv); i += 2 {
		p[kv[i]] = kv[i+1]
	}
	return fmt.Sprintf("vless://%s@%s:%s?type=%s&security=%s&pbk=%s&sid=%s&sni=%s&fp=%s#%s",
		p["id"], p["host"], p["port"], p["type"], p["security"], p["pbk"], p["sid"], p["sni"], p["fp"], url.PathEscape(label))
}

func uuid(n int) string { return fmt.Sprintf("e5e5e5e5-0000-4000-8000-%012d", n) }

func docOf(t *testing.T, body string) Document {
	t.Helper()
	doc, err := Parse([]byte(body), Detect, Limits{})
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	return doc
}

// catalogueSim applies reconciliations the way the controller commits them.
type catalogueSim struct {
	t        *testing.T
	lineages map[domain.LineageID]domain.Lineage
	at       domain.CatalogueRevision
	now      time.Time
}

func newSim(t *testing.T, lines ...string) *catalogueSim {
	s := &catalogueSim{t: t, lineages: map[domain.LineageID]domain.Lineage{}, now: t0}
	s.refresh(lines...)
	return s
}

func (s *catalogueSim) refresh(lines ...string) Reconciliation {
	s.t.Helper()
	return s.apply(docOf(s.t, strings.Join(lines, "\n")))
}

func (s *catalogueSim) apply(doc Document) Reconciliation {
	s.at++
	previous := slices.Collect(maps.Values(s.lineages))
	r := Reconcile(previous, doc, source, 1, s.at, s.now, grace)
	for _, c := range r.Changes {
		s.lineages[c.Lineage.ID] = c.Lineage
	}
	for _, l := range r.Restated {
		s.lineages[l.ID] = l
	}
	return r
}

// present maps each present label to its lineage ID.
func (s *catalogueSim) present() map[string]domain.LineageID {
	s.t.Helper()
	out := map[string]domain.LineageID{}
	for _, l := range s.lineages {
		if l.Presence != domain.Present {
			continue
		}
		if _, dup := out[l.Label]; dup {
			s.t.Fatalf("label %q is present twice", l.Label)
		}
		out[l.Label] = l.ID
	}
	return out
}

func (s *catalogueSim) presence(id domain.LineageID) domain.Presence { return s.lineages[id].Presence }

func TestReconcileProviderFeedFailures(t *testing.T) {
	type scenario struct {
		name   string
		before []string
		after  []string
		// kept maps a label before to the label after that must share its lineage.
		kept map[string]string
		// fresh labels must get lineages that did not exist before.
		fresh []string
		// missing labels from before must now be missing.
		missing   []string
		ambiguous int
	}
	gb := func(used string, days int) string {
		return fmt.Sprintf("🇩🇪 Germany [alice] %s GB / 100 GB ⏳ %d days left", used, days)
	}
	cases := []scenario{
		{
			name:   "UUID rotation",
			before: []string{vless("DE", "id", uuid(1)), vless("NL", "id", uuid(1), "host", "nl1.example.net")},
			after:  []string{vless("DE", "id", uuid(2)), vless("NL", "id", uuid(2), "host", "nl1.example.net")},
			kept:   map[string]string{"DE": "DE", "NL": "NL"},
		},
		{
			name:   "multi-user same host:port, every user rotates, names tell them apart",
			before: []string{vless("DE alice", "id", uuid(1)), vless("DE bob", "id", uuid(2))},
			after:  []string{vless("DE alice", "id", uuid(3)), vless("DE bob", "id", uuid(4))},
			kept:   map[string]string{"DE alice": "DE alice", "DE bob": "DE bob"},
		},
		{
			name:   "multi-user same host:port, one user rotates",
			before: []string{vless("DE 1", "id", uuid(1)), vless("DE 2", "id", uuid(2))},
			after:  []string{vless("DE 1", "id", uuid(1)), vless("DE 2b", "id", uuid(9))},
			kept:   map[string]string{"DE 1": "DE 1", "DE 2": "DE 2b"},
		},
		{
			name:      "multi-user same host:port with identical remarks is ambiguous",
			before:    []string{vless("DE", "id", uuid(1)), vless("DE", "id", uuid(2))},
			after:     []string{vless("DE", "id", uuid(3)), vless("DE", "id", uuid(4))},
			fresh:     []string{"DE", "DE 2"},
			missing:   []string{"DE", "DE 2"},
			ambiguous: 4,
		},
		{
			name:   "a unique name carries identity across a changed server",
			before: []string{vless("Germany", "host", "de1.example.net"), vless("Finland", "host", "fi1.example.net")},
			after:  []string{vless("Germany", "host", "de7.example.net", "id", uuid(7)), vless("Finland", "host", "fi1.example.net")},
			kept:   map[string]string{"Germany": "Germany", "Finland": "Finland"},
		},
		{
			name: "order shuffle with rotation",
			before: []string{vless("A", "host", "a.example.net"), vless("B", "host", "b.example.net"),
				vless("C", "host", "c.example.net")},
			after: []string{vless("C", "host", "c.example.net", "id", uuid(5)), vless("A", "host", "a.example.net", "id", uuid(5)),
				vless("B", "host", "b.example.net", "id", uuid(5))},
			kept: map[string]string{"A": "A", "B": "B", "C": "C"},
		},
		{
			name:   "split into transports keeps the surviving variant",
			before: []string{vless("DE")},
			after:  []string{vless("DE"), vless("DE WS", "type", "ws", "security", "tls", "sni", "de1.example.net")},
			kept:   map[string]string{"DE": "DE"},
			fresh:  []string{"DE WS"},
		},
		{
			name:   "split into transports with no surviving variant keeps the one with the name",
			before: []string{vless("DE")},
			after: []string{
				vless("DE", "type", "ws", "security", "tls", "sni", "de1.example.net"),
				vless("🇩🇪 DE", "type", "grpc", "security", "tls", "sni", "de1.example.net"),
			},
			kept:      map[string]string{"DE": "DE"},
			fresh:     []string{"🇩🇪 DE"},
			ambiguous: 1,
		},
		{
			name:   "exact duplicates collapse",
			before: []string{vless("DE")},
			after:  []string{vless("DE"), vless("DE"), vless("DE")},
			kept:   map[string]string{"DE": "DE"},
		},
		{
			name:   "reality short id and public key rotation",
			before: []string{vless("DE")},
			after:  []string{vless("DE", "sid", "0123456789abcdef", "pbk", "JKfC68h07m-NaOnVzJqoBWenFpzidYSUGv2DaOFWE8Q")},
			kept:   map[string]string{"DE": "DE"},
		},
		{
			name: "reality keys break a host tie",
			before: []string{vless("R", "id", uuid(1), "pbk", "key-one"),
				vless("⭐ R", "id", uuid(1), "pbk", "key-two")},
			after: []string{vless("R", "id", uuid(2), "pbk", "key-two", "fp", "safari"),
				vless("⭐ R", "id", uuid(2), "pbk", "key-one", "fp", "safari")},
			kept: map[string]string{"R": "⭐ R", "⭐ R": "R"},
		},
		{
			name:   "small panel tweak: fingerprint",
			before: []string{vless("DE", "fp", "chrome"), vless("NL", "host", "nl1.example.net")},
			after:  []string{vless("DE", "fp", "randomized"), vless("NL", "host", "nl1.example.net")},
			kept:   map[string]string{"DE": "DE", "NL": "NL"},
		},
		{
			name:   "a label change alone keeps the lineage",
			before: []string{vless(gb("12.3", 5)), vless("🇳🇱 Netherlands [alice] 12.3 GB / 100 GB ⏳ 5 days left", "host", "nl1.example.net")},
			after:  []string{vless(gb("14.9", 4)), vless("🇳🇱 Netherlands [alice] 14.9 GB / 100 GB ⏳ 4 days left", "host", "nl1.example.net")},
			kept: map[string]string{
				gb("12.3", 5): gb("14.9", 4),
				"🇳🇱 Netherlands [alice] 12.3 GB / 100 GB ⏳ 5 days left": "🇳🇱 Netherlands [alice] 14.9 GB / 100 GB ⏳ 4 days left",
			},
		},
		{
			name:    "a label change with every connection key changed is a new lineage",
			before:  []string{vless(gb("12.3", 5))},
			after:   []string{vless(gb("14.9", 4), "host", "de8.example.net", "id", uuid(8))},
			fresh:   []string{gb("14.9", 4)},
			missing: []string{gb("12.3", 5)},
		},
		{
			name:    "a weak match never steals a stronger one",
			before:  []string{vless("DE", "host", "de1.example.net"), vless("Backup", "host", "de2.example.net")},
			after:   []string{vless("Backup", "host", "de1.example.net"), vless("DE", "host", "de3.example.net", "id", uuid(3))},
			kept:    map[string]string{"DE": "Backup"},
			fresh:   []string{"DE"},
			missing: []string{"Backup"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sim := newSim(t, c.before...)
			before := sim.present()
			if len(before) != len(dedupe(c.before)) {
				t.Fatalf("setup: %d lineages for %d entries", len(before), len(c.before))
			}
			r := sim.refresh(c.after...)
			after := sim.present()
			for old, now := range c.kept {
				if after[now] != before[old] {
					t.Errorf("%q -> %q: lineage %s, want %s", old, now, after[now], before[old])
				}
			}
			for _, label := range c.fresh {
				if id := after[label]; id == "" || slices.Contains(slices.Collect(maps.Values(before)), id) {
					t.Errorf("%q: lineage %q, want a new one", label, id)
				}
			}
			for _, label := range c.missing {
				if got := sim.presence(before[label]); got != domain.Missing {
					t.Errorf("%q: presence %d, want Missing", label, got)
				}
			}
			if len(after) != len(dedupe(c.after)) {
				t.Errorf("present lineages = %d, want %d", len(after), len(dedupe(c.after)))
			}
			if len(r.Ambiguous) != c.ambiguous {
				t.Errorf("ambiguous = %v, want %d", r.Ambiguous, c.ambiguous)
			}
		})
	}
}

func dedupe(lines []string) []string { return slices.Compact(slices.Sorted(slices.Values(lines))) }

func TestReconcileRevisionsKeepLineage(t *testing.T) {
	sim := newSim(t, vless("DE"))
	id := sim.present()["DE"]
	before := sim.lineages[id].Ref
	r := sim.refresh(vless("DE", "fp", "firefox"))
	if len(r.Changes) != 1 || r.Changes[0].Previous == nil || *r.Changes[0].Previous != before {
		t.Fatalf("changes = %+v, want one revision of %s", r.Changes, id)
	}
	after := sim.lineages[id].Ref
	if after.Lineage != id || after.Digest == before.Digest {
		t.Fatalf("ref = %+v, want lineage %s with a new digest", after, id)
	}
}

func TestReconcileUnchangedFeedHasNoChanges(t *testing.T) {
	lines := []string{vless("A", "host", "a.example.net"), vless("B", "host", "b.example.net")}
	sim := newSim(t, lines...)
	if r := sim.refresh(lines[1], lines[0]); len(r.Changes) != 0 {
		t.Fatalf("changes = %+v, want none", r.Changes)
	}
}

func TestReconcileMuxTweakIsNotARevision(t *testing.T) {
	config := func(mux bool) string {
		return fmt.Sprintf(`{"outbounds": [{"tag": "proxy", "protocol": "trojan", "mux": {"enabled": %v, "concurrency": 8},
			"settings": {"servers": [{"address": "t.example.net", "port": 443, "password": "pw"}]},
			"streamSettings": {"security": "tls", "tlsSettings": {"serverName": "t.example.net"}}}]}`, mux)
	}
	sim := &catalogueSim{t: t, lineages: map[domain.LineageID]domain.Lineage{}, now: t0}
	sim.apply(docOf(t, config(false)))
	if r := sim.apply(docOf(t, config(true))); len(r.Changes) != 0 {
		t.Fatalf("changes = %+v, want none", r.Changes)
	}
}

func TestReconcileProviderID(t *testing.T) {
	sip := func(id, host string) string {
		return fmt.Sprintf(`{"version": 1, "servers": [{"id": %q, "remarks": "Tokyo", "server": %q, "server_port": 8388, "password": "pw", "method": "aes-256-gcm"}]}`, id, host)
	}
	sim := &catalogueSim{t: t, lineages: map[domain.LineageID]domain.Lineage{}, now: t0}
	sim.apply(docOf(t, sip("srv-1", "tyo1.example.net")))
	first := sim.present()["Tokyo"]

	sim.apply(docOf(t, sip("srv-1", "tyo9.example.net")))
	if got := sim.present()["Tokyo"]; got != first {
		t.Fatalf("same provider id, new host: lineage %s, want %s", got, first)
	}
	r := sim.apply(docOf(t, sip("srv-2", "tyo9.example.net")))
	if got := sim.present()["Tokyo"]; got == first || sim.presence(first) != domain.Missing {
		t.Fatalf("conflicting provider id reused lineage %s (changes %+v)", got, r.Changes)
	}
}

func TestReconcileMissingGraceAndTombstone(t *testing.T) {
	a, b := vless("A", "host", "a.example.net"), vless("B", "host", "b.example.net")
	sim := newSim(t, a, b)
	idA := sim.present()["A"]

	sim.refresh(b)
	l := sim.lineages[idA]
	if l.Presence != domain.Missing || !l.MissingSince.Equal(t0) || !l.GraceUntil.Equal(t0.Add(grace)) {
		t.Fatalf("after removal: %+v, want missing since t0 with grace", l)
	}

	sim.now = t0.Add(grace / 2)
	if r := sim.refresh(b); len(r.Changes) != 0 {
		t.Fatalf("a still-missing lineage changed: %+v", r.Changes)
	}
	sim.refresh(a, b)
	if got := sim.present()["A"]; got != idA {
		t.Fatalf("reappearing within grace: lineage %s, want %s", got, idA)
	}
	if l := sim.lineages[idA]; !l.MissingSince.IsZero() || !l.GraceUntil.IsZero() {
		t.Fatalf("present lineage keeps missing markers: %+v", l)
	}

	sim.refresh(b)
	sim.now = sim.now.Add(grace + time.Second)
	sim.refresh(b)
	if sim.presence(idA) != domain.Tombstone {
		t.Fatalf("after grace: presence %d, want Tombstone", sim.presence(idA))
	}
	sim.refresh(a, b)
	if got := sim.present()["A"]; got == idA || got == "" {
		t.Fatalf("after tombstone: lineage %q, want a new one", got)
	}
	if sim.presence(idA) != domain.Tombstone {
		t.Fatal("tombstone was revived")
	}
}

func TestReconcileIsPerSource(t *testing.T) {
	doc := docOf(t, vless("DE"))
	other := Reconcile(nil, doc, "source-b", 1, 1, t0, grace).Changes[0].Lineage
	r := Reconcile([]domain.Lineage{other}, doc, source, 1, 2, t0, grace)
	if len(r.Changes) != 1 || r.Changes[0].Previous != nil || r.Changes[0].Lineage.Source != source {
		t.Fatalf("changes = %+v, want one new lineage of %s", r.Changes, source)
	}
}

func TestReconcileRefusesAnIncompleteDocument(t *testing.T) {
	sim := newSim(t, vless("DE"))
	if r := sim.apply(Document{}); len(r.Changes) != 0 {
		t.Fatalf("a zero document changed lineages: %+v", r.Changes)
	}
	r := sim.apply(docOf(t, `{"version": 1, "servers": []}`))
	if len(r.Changes) != 1 || r.Changes[0].Lineage.Presence != domain.Missing {
		t.Fatalf("explicitly empty feed: %+v, want the lineage missing", r.Changes)
	}
}

func TestReconcileNewIDsIgnoreProviderOrder(t *testing.T) {
	a, b := vless("A", "host", "a.example.net"), vless("B", "host", "b.example.net")
	ids := func(lines ...string) map[string]domain.LineageID {
		out := map[string]domain.LineageID{}
		for _, c := range Reconcile(nil, docOf(t, strings.Join(lines, "\n")), source, 1, 7, t0, grace).Changes {
			out[c.Lineage.Label] = c.Lineage.ID
		}
		return out
	}
	if x, y := ids(a, b), ids(b, a); !maps.Equal(x, y) {
		t.Fatalf("ids depend on order: %v vs %v", x, y)
	}
}

// 3x-ui picks the Reality sni and sid at random from the inbound's accepted lists
// on every subscription request (internal/sub/service.go applyShareRealityParams).
func TestReconcileRealityPicksAreNotIdentity(t *testing.T) {
	feed := func(sni, sid string) []string {
		return []string{
			vless("alice", "host", "de1.example.net", "sni", sni, "sid", sid),
			vless("alice", "host", "nl1.example.net", "sni", sni, "sid", sid),
		}
	}
	sim := newSim(t, feed("www.a.com", "63ecb9e95ef79ad3")...)
	before := maps.Clone(sim.lineages)
	if r := sim.refresh(feed("www.b.com", "13f0a742")...); len(r.Changes) != 0 || len(r.Ambiguous) != 0 {
		t.Fatalf("new random picks: changes = %d, ambiguous = %v, want none", len(r.Changes), r.Ambiguous)
	}
	if !maps.EqualFunc(before, sim.lineages, func(a, b domain.Lineage) bool { return a.Ref == b.Ref && a.Presence == b.Presence }) {
		t.Fatal("new random picks changed a lineage or its revision")
	}
	for id, l := range sim.lineages {
		if sni, sid := picks(l); sni != "www.a.com" || sid != "63ecb9e95ef79ad3" {
			t.Fatalf("%s: stored picks = %s/%s, want the first ones", id, sni, sid)
		}
	}
}

func picks(l domain.Lineage) (sni, shortID string) {
	_, hops := l.Server.Closure()
	return hops[0].TLS.ServerName, hops[0].TLS.Reality.ShortID
}

// canonicalSID is a short ID as the catalogue stores it.
func canonicalSID(t *testing.T, s string) string {
	t.Helper()
	id, err := realityShortID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// pickSim refreshes one Reality server hourly, feeding it pick(hour), and
// reports the hours at which its revision changed. A refresh reports Changes
// only when it makes a revision or the label changes.
func pickSim(t *testing.T, hours int, pick func(hour int) (sni, shortID string), label func(hour int) string) (*catalogueSim, domain.LineageID, []int) {
	t.Helper()
	line := func(h int) string {
		sni, shortID := pick(h)
		return vless(label(h), "sni", sni, "sid", shortID)
	}
	sim := newSim(t, line(0))
	id := sim.present()[label(0)]
	ref := sim.lineages[id].Ref
	var revisions []int
	for h := 1; h <= hours; h++ {
		sim.now = t0.Add(time.Duration(h) * time.Hour)
		r := sim.refresh(line(h))
		l := sim.lineages[id]
		if changed := l.Ref != ref || label(h) != label(h-1); changed != (len(r.Changes) > 0) {
			t.Fatalf("hour %d: %d changes for a refresh that changed the revision or label: %t", h, len(r.Changes), changed)
		}
		if l.Ref != ref {
			revisions, ref = append(revisions, h), l.Ref
			sni, shortID := pick(h)
			if storedSNI, storedID := picks(l); storedSNI != sni || storedID != canonicalSID(t, shortID) {
				t.Fatalf("hour %d: a new revision without the latest picks", h)
			}
		}
	}
	return sim, id, revisions
}

func alice(int) string { return "alice" }

// 3x-ui re-picks from small lists: the stored pick keeps coming back.
func TestReconcileRandomPicksMakeNoRevision(t *testing.T) {
	snis, sids := []string{"www.a.com", "www.b.com", "www.c.com"}, []string{"aa11", "bb22", "cc33"}
	sim, id, revisions := pickSim(t, 7*24, func(h int) (string, string) { return snis[h%3], sids[h/2%3] }, alice)
	if len(revisions) != 0 {
		t.Fatalf("random re-picks made revisions at hours %v", revisions)
	}
	if sni, stored := picks(sim.lineages[id]); sni != "www.a.com" || stored != canonicalSID(t, "aa11") {
		t.Fatalf("stored picks = %s/%s, want the first ones", sni, stored)
	}
}

func TestReconcileRepicksReportNoChanges(t *testing.T) {
	snis, sids := []string{"www.a.com", "www.b.com", "www.c.com"}, []string{"aa11", "bb22", "cc33"}
	line := func(h int) string { return vless("alice", "sni", snis[h%3], "sid", sids[h/2%3]) }
	sim := newSim(t, line(0))
	withChanges, withReality := 0, 0
	for h := 1; h <= 24; h++ {
		sim.now = t0.Add(time.Duration(h) * time.Hour)
		r := sim.refresh(line(h))
		withChanges += min(len(r.Changes), 1)
		withReality += min(len(r.Restated), 1)
	}
	if withChanges != 0 || withReality == 0 {
		t.Fatalf("24 re-pick refreshes: %d reported Changes and %d pick bookkeeping, want 0 and some", withChanges, withReality)
	}
}

// A provider replaced both lists: the stored pick never comes back.
func TestReconcileAdoptsARotatedPickOnce(t *testing.T) {
	snis, sids := []string{"www.a.com", "www.b.com", "www.c.com"}, []string{"aa11", "bb22", "cc33"}
	pick := func(h int) (string, string) {
		if h == 0 {
			return snis[0], sids[0]
		}
		return snis[1+h%2], sids[1+h/2%2]
	}
	if _, _, revisions := pickSim(t, 72, pick, alice); !slices.Equal(revisions, []int{25}) {
		t.Fatalf("revisions at hours %v, want one, a day after the first refresh without the stored pick", revisions)
	}
}

func TestReconcileAdoptsAtMostOncePerDay(t *testing.T) {
	pick := func(h int) (string, string) { return "www.a.com", fmt.Sprintf("%04x", h) }
	if _, _, revisions := pickSim(t, 72, pick, alice); !slices.Equal(revisions, []int{25, 50}) {
		t.Fatalf("revisions at hours %v, want [25 50]", revisions)
	}
}

func TestReconcileAdoptsAfterThreeRefreshes(t *testing.T) {
	sim := newSim(t, vless("alice", "sid", "aa11"))
	id := sim.present()["alice"]
	ref := sim.lineages[id].Ref
	for _, h := range []int{1, 25} {
		sim.now = t0.Add(time.Duration(h) * time.Hour)
		sim.refresh(vless("alice", "sid", "bb22"))
		if sim.lineages[id].Ref != ref {
			t.Fatalf("hour %d: adopted after fewer than three refreshes", h)
		}
	}
	sim.now = t0.Add(26 * time.Hour)
	sim.refresh(vless("alice", "sid", "bb22"))
	if _, stored := picks(sim.lineages[id]); stored != canonicalSID(t, "bb22") || sim.lineages[id].Ref == ref {
		t.Fatalf("third refresh a day later: stored short ID = %s, want bb22 as a new revision", stored)
	}
}

// Panels rewrite remarks with traffic counters on every refresh; a label change
// must not carry a re-pick into the stored server.
func TestReconcileLabelChangeKeepsTheStoredPick(t *testing.T) {
	snis := []string{"www.a.com", "www.b.com"}
	label := func(h int) string { return fmt.Sprintf("alice %d GB", h) }
	sim, id, revisions := pickSim(t, 11, func(h int) (string, string) { return snis[h%2], "aa11" }, label)
	if len(revisions) != 0 {
		t.Fatalf("label changes made revisions at hours %v", revisions)
	}
	if l := sim.lineages[id]; l.Label != label(11) {
		t.Fatalf("label = %q, want the latest", l.Label)
	} else if sni, _ := picks(l); sni != "www.a.com" {
		t.Fatalf("stored server name = %s, want the first pick", sni)
	}
}

func TestReconcileDuplicateOrderIsNotAChange(t *testing.T) {
	germany := vless("Germany")
	auto := strings.Replace(germany, "#Germany", "#Auto", 1)
	sim := newSim(t, germany, auto)
	for _, order := range [][]string{{auto, germany}, {germany, auto}} {
		if r := sim.refresh(order...); len(r.Changes) != 0 {
			t.Fatalf("reordered duplicates: %d changes, want none", len(r.Changes))
		}
	}
	if a, b := newSim(t, germany, auto).present(), newSim(t, auto, germany).present(); !slices.Equal(slices.Sorted(maps.Keys(a)), slices.Sorted(maps.Keys(b))) {
		t.Fatalf("labels depend on feed order: %v vs %v", slices.Sorted(maps.Keys(a)), slices.Sorted(maps.Keys(b)))
	}
}

// chainedReality is an Xray feed of one Reality hop dialed through another,
// with the short ID each carries.
func chainedReality(first, second string) string {
	hop := func(tag, host, sid, dialer string) string {
		return fmt.Sprintf(`{"tag":%q,"protocol":"vless","settings":{"address":%q,"port":443,"id":%q,"encryption":"none"},`+
			`"streamSettings":{"security":"reality","realitySettings":{"serverName":"www.example.com","publicKey":%q,`+
			`"fingerprint":"chrome","shortId":%q},"sockopt":{"dialerProxy":%q}}}`, tag, host, linkUUID, realityKey, sid, dialer)
	}
	return `{"outbounds":[` + hop("node", "a.example.net", first, "relay") + `,` + hop("relay", "b.example.net", second, "") + `]}`
}

// Each stored pick comes back every other hour, but never all of them in the
// same refresh: no pick has been rotated out.
func TestReconcileSeesEachPickOnItsOwn(t *testing.T) {
	for _, c := range []struct {
		name, first string
		feed        func(hour int) string
	}{
		{"one hop per refresh", chainedReality("aa", "bb"), func(h int) string {
			if h%2 == 0 {
				return chainedReality("cc", "bb")
			}
			return chainedReality("aa", "cc")
		}},
		{"one pick per refresh", vless("node", "sni", "www.a.com", "sid", "bb"), func(h int) string {
			if h%2 == 0 {
				return vless("node", "sni", "www.c.com", "sid", "bb")
			}
			return vless("node", "sni", "www.a.com", "sid", "cc")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sim := newSim(t, c.first)
			id := sim.present()["node"]
			before := sim.lineages[id].Ref
			for h := 1; h <= 72; h++ {
				sim.now = t0.Add(time.Duration(h) * time.Hour)
				if r := sim.refresh(c.feed(h)); len(r.Changes) != 0 || sim.lineages[id].Ref != before {
					t.Fatalf("hour %d: a revision although every stored pick appeared within the last two hours", h)
				}
			}
		})
	}
}

func TestReconcileAdoptsAPickRotatedOnOneHop(t *testing.T) {
	sim := newSim(t, chainedReality("aa", "bb"))
	id := sim.present()["node"]
	ref := sim.lineages[id].Ref
	var revisions []int
	for h := 1; h <= 48; h++ {
		sim.now = t0.Add(time.Duration(h) * time.Hour)
		sim.refresh(chainedReality("aa", "cc"))
		if l := sim.lineages[id]; l.Ref != ref {
			revisions, ref = append(revisions, h), l.Ref
		}
	}
	if _, hops := sim.lineages[id].Server.Closure(); !slices.Equal(revisions, []int{25}) || hops[1].TLS.Reality.ShortID != canonicalSID(t, "cc") {
		t.Fatalf("revisions at hours %v, want one at 25 that adopts the relay's new short ID", revisions)
	}
}
