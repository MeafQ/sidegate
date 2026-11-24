package catalogue

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// acmeFixture is an invented provider's feed: 6 configs of 13 proxy outbounds and 5 utility
// ones, each with routing and an observatory. Every name, address and credential in it is made up.
func acmeFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/acme-shape.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// acmeConfigs are the remarks of the 6 configs, each with the country its flag names.
var acmeConfigs = map[string]string{"Bulgaria 🇧🇬": "BG", "Germany 🇩🇪": "DE", "Netherlands 🇳🇱": "NL", "Romania 🇷🇴": "RO", "USA 🇺🇸": "US", "Sweden 🇸🇪": "SE"}

// acmeRoles are the tags that sit on a different server in each config.
var acmeRoles = []string{"proxy", "EDGE-STANDBY"}

// reconciled is a source's lineages after applying documents, as the controller does.
type reconciled struct {
	t        *testing.T
	lineages map[domain.LineageID]domain.Lineage
	at       domain.CatalogueRevision
	feed     domain.Feed
}

func newReconciled(t *testing.T) *reconciled {
	return &reconciled{t: t, lineages: map[domain.LineageID]domain.Lineage{}}
}

func (r *reconciled) apply(data []byte) Reconciliation {
	r.t.Helper()
	doc, err := Parse(data, Detect, Limits{})
	if err != nil {
		r.t.Fatal(err)
	}
	r.at++
	rec := Reconcile(slices.Collect(maps.Values(r.lineages)), doc, "acme", 1, r.at, t0, time.Hour)
	for _, c := range rec.Changes {
		r.lineages[c.Lineage.ID] = c.Lineage
	}
	for _, l := range rec.Restated {
		r.lineages[l.ID] = l
	}
	r.feed = rec.Feed
	return rec
}

func (r *reconciled) catalogue() domain.Catalogue {
	return domain.NewCatalogue(r.at, slices.Collect(maps.Values(r.lineages)))
}

// keyed lists the latest feed's configs that have a server, in document order.
func (r *reconciled) keyed() []domain.FeedConfig {
	var out []domain.FeedConfig
	for _, c := range r.feed.Configs {
		if c.Key != "" {
			out = append(out, c)
		}
	}
	return out
}

func (r *reconciled) configKey(name string) domain.LineageID {
	r.t.Helper()
	for _, c := range r.keyed() {
		if c.Name == name {
			return c.Key
		}
	}
	r.t.Fatalf("no config named %q in %+v", name, r.feed.Configs)
	return ""
}

// under lists the present servers seen under a config's key, in ID order.
func (r *reconciled) under(key domain.LineageID) []domain.LineageID {
	var out []domain.LineageID
	for _, l := range r.lineages {
		if l.Presence == domain.Present && slices.ContainsFunc(l.Seen, func(o domain.Occurrence) bool { return o.Config == key }) {
			out = append(out, l.ID)
		}
	}
	slices.Sort(out)
	return out
}

func TestAcmeShape(t *testing.T) {
	doc, err := Parse(acmeFixture(t), Detect, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	occurrences := 0
	for _, e := range doc.Entries() {
		occurrences += len(e.Seen)
	}
	if occurrences != 78 {
		t.Errorf("entries = %d, want 78", occurrences)
	}
	if got := len(doc.Entries()); got != 12 {
		t.Errorf("merged = %d, want 12", got)
	}
	r := newReconciled(t)
	r.apply(acmeFixture(t))
	c := r.catalogue()
	if got := len(c.All()); got != 12 {
		t.Errorf("lineages = %d, want 12", got)
	}
	got := map[string]string{}
	for _, l := range c.All() {
		got[PlainLabel(l.Label)] = l.Country.Code
		if l.Country.Source != domain.CountryTagFlag {
			t.Errorf("%s: country %+v, want one from its tags' flag", l.Label, l.Country)
		}
	}
	want := map[string]string{}
	for remarks, country := range acmeConfigs {
		for _, role := range acmeRoles {
			want[PlainLabel(remarks)+" · "+role] = country
		}
	}
	if !maps.Equal(got, want) {
		t.Errorf("names and countries = %q\nwant %q", got, want)
	}
	rows := r.keyed()
	if len(rows) != 6 {
		t.Fatalf("configs = %+v, want 6", rows)
	}
	keys := map[domain.LineageID]bool{}
	for _, row := range rows {
		under := r.under(row.Key)
		if len(under) != 12 {
			t.Errorf("config %q holds %d servers, want 12", row.Name, len(under))
		}
		if !slices.Contains(under, row.Key) || keys[row.Key] {
			t.Errorf("config %q is keyed by %s, want a server of its own no other config is keyed by", row.Name, row.Key)
		}
		keys[row.Key] = true
	}
}

// editAcme rewrites the decoded document, keeping the order of each object's outbounds
// as lists.
func editAcme(t *testing.T, edit func(configs []map[string]any)) []byte {
	t.Helper()
	var configs []map[string]any
	if err := json.Unmarshal(acmeFixture(t), &configs); err != nil {
		t.Fatal(err)
	}
	edit(configs)
	out, err := json.Marshal(configs)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func reorderAcme(t *testing.T) []byte {
	return editAcme(t, func(configs []map[string]any) {
		slices.Reverse(configs)
		for _, c := range configs {
			outbounds := c["outbounds"].([]any)
			slices.Reverse(outbounds[1:])
		}
	})
}

func rotateAcme(t *testing.T) []byte {
	return []byte(strings.ReplaceAll(string(acmeFixture(t)), "aaaaaaaa-", "bbbbbbbb-"))
}

func moveAcme(t *testing.T) []byte {
	return []byte(strings.ReplaceAll(string(acmeFixture(t)), ".fixture.example.net", ".moved.example.net"))
}

func TestAcmeConfigKeySurvives(t *testing.T) {
	for name, next := range map[string]func(*testing.T) []byte{"reordered": reorderAcme, "rotated": rotateAcme, "moved": moveAcme} {
		t.Run(name, func(t *testing.T) {
			r := newReconciled(t)
			r.apply(acmeFixture(t))
			key := r.configKey("Sweden 🇸🇪")
			before := r.under(key)
			if len(before) != 12 {
				t.Fatalf("under the key before = %q", before)
			}

			rec := r.apply(next(t))

			c := r.catalogue()
			if got := r.under(key); !slices.Equal(got, before) {
				t.Errorf("under the key after = %q, want %q", got, before)
			}
			if got := r.configKey("Sweden 🇸🇪"); got != key {
				t.Errorf("key = %s, want %s", got, key)
			}
			if len(c.All()) != 12 {
				t.Errorf("lineages = %d, want the same 12", len(c.All()))
			}
			for _, change := range rec.Changes {
				if name == "reordered" && (change.Previous == nil || change.Lineage.Ref != *change.Previous) {
					t.Errorf("a reorder changed %s beyond its positions", change.Lineage.ID)
				}
			}
		})
	}
}

// A refused root gives its own reason and leaves its config listed with the rest of its servers.
func TestAcmeARefusedRootLeavesItsConfigListed(t *testing.T) {
	data := editAcme(t, func(configs []map[string]any) {
		first := configs[0]["outbounds"].([]any)[0].(map[string]any)
		first["protocol"] = "wireguard"
		first["settings"] = map[string]any{}
		delete(first, "streamSettings")
	})
	doc, err := Parse(data, Detect, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Diagnostic{{Entry: 0, Code: CodeUnsupportedProtocol, Field: "wireguard"}}; !slices.Equal(doc.Diagnostics(), want) {
		t.Errorf("diagnostics = %+v, want %+v", doc.Diagnostics(), want)
	}
	r := newReconciled(t)
	r.apply(data)
	if rows := r.keyed(); len(rows) != 6 {
		t.Fatalf("configs = %+v, want the config still listed", rows)
	}
	if got := r.under(r.configKey("Germany 🇩🇪")); len(got) != 11 {
		t.Errorf("under Germany = %q, want the 11 servers left", got)
	}
}

func TestConfigsSharingARootKeepTheirOwnKeys(t *testing.T) {
	data := editAcme(t, func(configs []map[string]any) {
		outbounds := configs[1]["outbounds"].([]any)
		outbounds[0] = configs[0]["outbounds"].([]any)[0]
	})
	r := newReconciled(t)
	r.apply(data)
	rows := r.keyed()
	if len(rows) != 6 || len(slices.CompactFunc(slices.SortedFunc(slices.Values(rows), func(a, b domain.FeedConfig) int { return strings.Compare(string(a.Key), string(b.Key)) }),
		func(a, b domain.FeedConfig) bool { return a.Key == b.Key })) != 6 {
		t.Fatalf("configs = %+v, want a key each though two share a root", rows)
	}
	key := r.configKey("Germany 🇩🇪")
	for _, l := range r.lineages {
		for _, o := range l.Seen {
			if o.ConfigName == "Netherlands 🇳🇱" && o.Config == key {
				t.Errorf("%s: seen in %q under the key of another config", l.Label, o.ConfigName)
			}
		}
	}
}

func TestAConfigKeepsItsKeyWhenARootChanges(t *testing.T) {
	r := newReconciled(t)
	r.apply(acmeFixture(t))
	key := r.configKey("Germany 🇩🇪")
	r.apply(editAcme(t, func(configs []map[string]any) {
		first := configs[0]["outbounds"].([]any)[0].(map[string]any)
		first["settings"].(map[string]any)["vnext"].([]any)[0].(map[string]any)["address"] = "elsewhere.example.net"
	}))
	if got := r.configKey("Germany 🇩🇪"); got != key {
		t.Errorf("key = %s, want %s kept across a changed root", got, key)
	}
}

// A config placed first doesn't take the key of a config already there that shares a root with it.
func TestANewConfigLeavesAnExistingConfigItsKey(t *testing.T) {
	config := func(name string, hosts ...string) string {
		var out []string
		for i, host := range hosts {
			out = append(out, fmt.Sprintf(`{"tag":"proxy%d","protocol":"trojan","settings":{"address":%q,"port":443,"password":"pw"}}`, i, host))
		}
		return fmt.Sprintf(`{"remarks":%q,"outbounds":[%s]}`, name, strings.Join(out, ","))
	}
	a, b, newcomer := config("A", "a.example.net", "other-a.example.net"), config("B", "b.example.net"), config("New", "a.example.net")
	r := newReconciled(t)
	r.apply([]byte("[" + a + "," + b + "]"))
	key := r.configKey("A")
	before := r.under(key)
	r.apply([]byte("[" + newcomer + "," + a + "," + b + "]"))
	if got := r.configKey("A"); got != key {
		t.Errorf("A's key = %s, want %s", got, key)
	}
	if after := r.under(key); !slices.Equal(after, before) {
		t.Errorf("under A's key = %v, was %v", after, before)
	}
}

func TestConfigWithNoValidRootHasNoKey(t *testing.T) {
	data := `[{"remarks":"Bad","outbounds":[{"tag":"a","protocol":"wireguard"},{"tag":"b","protocol":"wireguard"}]},
		{"remarks":"Empty","outbounds":[{"tag":"direct","protocol":"freedom"}]},
		{"remarks":"Good","outbounds":[{"tag":"c","protocol":"trojan","settings":{"address":"good.example.net","port":443,"password":"pw"}}]}]`
	r := newReconciled(t)
	r.apply([]byte(data))
	rows := r.keyed()
	if len(rows) != 1 || rows[0].Name != "Good" {
		t.Errorf("configs = %+v, want only Good", rows)
	}
	doc, _ := Parse([]byte(data), Detect, Limits{})
	want := []Diagnostic{{Entry: 0, Code: CodeUnsupportedProtocol, Field: "wireguard"}, {Entry: 1, Code: CodeUnsupportedProtocol, Field: "wireguard"}}
	if !slices.Equal(doc.Diagnostics(), want) {
		t.Errorf("diagnostics = %+v, want %+v: a reason for each refused root and none for a config of helpers", doc.Diagnostics(), want)
	}
}

func TestDuplicateTagsRejectTheirConfig(t *testing.T) {
	trojan := func(tag, host string) string {
		return fmt.Sprintf(`{"tag":%q,"protocol":"trojan","settings":{"address":%q,"port":443,"password":"pw"}}`, tag, host)
	}
	data := fmt.Sprintf(`[{"remarks":"Dup","outbounds":[%s,%s]},{"remarks":"Fine","outbounds":[%s]}]`,
		trojan("a", "one.example.net"), trojan("a", "two.example.net"), trojan("b", "three.example.net"))
	doc, err := Parse([]byte(data), Detect, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	entries := doc.Entries()
	if len(entries) != 1 || entries[0].Label != "Fine · b" {
		t.Errorf("entries = %+v, want only Fine · b", entries)
	}
	if !slices.ContainsFunc(doc.Diagnostics(), func(d Diagnostic) bool { return d.Code == CodeDuplicateTag }) {
		t.Errorf("diagnostics = %+v, want duplicate_tag", doc.Diagnostics())
	}
}

func TestClashDialerNamingARepeatedProxyIsRefused(t *testing.T) {
	data := `proxies:
  - {name: relay, type: trojan, server: one.example.net, port: 443, password: pw}
  - {name: relay, type: trojan, server: two.example.net, port: 443, password: pw}
  - {name: exit, type: trojan, server: three.example.net, port: 443, password: pw, dialer-proxy: relay}
`
	doc, err := Parse([]byte(data), Detect, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(doc.Diagnostics(), diag(2, CodeDuplicateTag, "dialer-proxy")) {
		t.Errorf("diagnostics = %+v, want the exit refused", doc.Diagnostics())
	}
	var labels []string
	for _, e := range doc.Entries() {
		labels = append(labels, e.Label)
	}
	slices.Sort(labels)
	if !slices.Equal(labels, []string{"relay", "relay 2"}) {
		t.Errorf("labels = %q, want the two plain servers kept apart", labels)
	}
}

func TestNames(t *testing.T) {
	trojan := func(tag, host string) string {
		return fmt.Sprintf(`{"tag":%q,"protocol":"trojan","settings":{"address":%q,"port":443,"password":"pw"}}`, tag, host)
	}
	link := func(host, name string) string { return "trojan://pw@" + host + ":443?security=none#" + name }
	cases := []struct {
		name string
		feed string
		want []string
	}{
		{"two links with one name get a number, in identity order", link("a.example.net", "Germany") + "\n" + link("b.example.net", "Germany"),
			[]string{"Germany", "Germany 2"}},
		{"three links", link("a.example.net", "Germany") + "\n" + link("b.example.net", "Germany") + "\n" + link("c.example.net", "Germany"),
			[]string{"Germany", "Germany 2", "Germany 3"}},
		{"a number skips a label another server has", link("a.example.net", "G") + "\n" + link("b.example.net", "G") + "\n" + link("c.example.net", "G 2"),
			[]string{"G", "G 2", "G 3"}},
		{"one config of two servers names both by its remarks and their tags",
			fmt.Sprintf(`[{"remarks":"R","outbounds":[%s,%s]}]`, trojan("a", "a.example.net"), trojan("b", "b.example.net")),
			[]string{"R · a", "R · b"}},
		{"a tag on different servers in different configs is a role, and each config is home to its server",
			fmt.Sprintf(`[{"remarks":"A","outbounds":[%s,%s]},{"remarks":"B","outbounds":[%s,%s]}]`,
				trojan("one", "one.example.net"), trojan("node", "a.example.net"), trojan("two", "two.example.net"), trojan("node", "b.example.net")),
			[]string{"A · node", "A · one", "B · node", "B · two"}},
		{"a config that holds no other server is its home",
			fmt.Sprintf(`[{"remarks":"Shared","outbounds":[%s,%s]},{"remarks":"Own","outbounds":[%s]}]`,
				trojan("first", "one.example.net"), trojan("second", "two.example.net"), trojan("again", "two.example.net")),
			[]string{"Own · again", "Shared · first"}},
		{"a server several configs list under one tag, none by a role, among other servers is named by that tag alone",
			fmt.Sprintf(`[{"remarks":"A","outbounds":[%s,%s,%s]},{"remarks":"B","outbounds":[%s,%s,%s]}]`,
				trojan("first-a", "f.example.net"), trojan("rare", "s.example.net"), trojan("common", "s.example.net"),
				trojan("first-b", "g.example.net"), trojan("common", "s.example.net"), trojan("alpha", "s.example.net")),
			[]string{"A · first-a", "B · first-b", "common"}},
		{"a tag tie goes to the bytewise least",
			fmt.Sprintf(`[{"remarks":"A","outbounds":[%s,%s]},{"remarks":"B","outbounds":[%s,%s]}]`,
				trojan("first-a", "f.example.net"), trojan("beta", "s.example.net"), trojan("first-b", "g.example.net"), trojan("alpha", "s.example.net")),
			[]string{"A · first-a", "B · first-b", "alpha"}},
		{"two configs that each hold only the same server name it from the remarks that sort first",
			fmt.Sprintf(`[{"remarks":"Z","outbounds":[%s]},{"remarks":"Y","outbounds":[%s]}]`, trojan("one", "s.example.net"), trojan("two", "s.example.net")),
			[]string{"Y · two"}},
		{"a home's remarks tie goes to the least tag",
			fmt.Sprintf(`[{"remarks":"Y","outbounds":[%s,%s]}]`, trojan("two", "s.example.net"), trojan("one", "s.example.net")),
			[]string{"Y · one"}},
		{"a home with no remarks names by its tag", fmt.Sprintf(`[{"outbounds":[%s]}]`, trojan("solo", "s.example.net")), []string{"solo"}},
		{"no name at all", "trojan://pw@a.example.net:443?security=none\ntrojan://pw@b.example.net:443?security=none", []string{"Server", "Server 2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, err := Parse([]byte(c.feed), Detect, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range doc.Entries() {
				got = append(got, e.Label)
			}
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("labels = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSuffixFollowsIdentityOrderNotProviderOrder(t *testing.T) {
	a, b := "trojan://pw@a.example.net:443?security=none#G", "trojan://pw@b.example.net:443?security=none#G"
	labels := func(feed string) map[domain.Digest]string {
		doc, err := Parse([]byte(feed), Detect, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		out := map[domain.Digest]string{}
		for _, e := range doc.Entries() {
			out[e.Identity.Strong] = e.Label
		}
		return out
	}
	if one, two := labels(a+"\n"+b), labels(b+"\n"+a); !maps.Equal(one, two) {
		t.Errorf("reordering moved a suffix: %v, %v", one, two)
	}
}

func TestCountries(t *testing.T) {
	resolver, err := newCountryResolver(CountryPolicy{LabelRules: []CountryRule{{Regex: "Frankfurt", Country: "DE"}}})
	if err != nil {
		t.Fatal(err)
	}
	server := func(remarks, tag string, tags ...string) named {
		name, _ := domain.NewName(remarks, tag)
		return named{name: name, remarks: remarks, tags: append([]string{tag}, tags...)}
	}
	cases := []struct {
		name   string
		server named
		code   string
		source domain.CountrySource
	}{
		{"the tags' flag", server("Config", "Germany 🇩🇪"), "DE", domain.CountryTagFlag},
		{"one flag in two tags", server("Config", "🇩🇪 one", "two 🇩🇪"), "DE", domain.CountryTagFlag},
		{"the tags' flag beats the config's", server("Sweden 🇸🇪", "🇳🇱 EDGE-STANDBY"), "NL", domain.CountryTagFlag},
		{"two flags in the tags give the config's", server("Sweden 🇸🇪", "🇩🇪 via 🇳🇱"), "SE", domain.CountryConfigFlag},
		{"two flags across tags give the config's", server("Sweden 🇸🇪", "🇩🇪 one", "🇳🇱 two"), "SE", domain.CountryConfigFlag},
		{"the config's flag", server("Sweden 🇸🇪", "proxy"), "SE", domain.CountryConfigFlag},
		{"two flags at every step give none", server("🇩🇪🇳🇱", "🇫🇷 a 🇮🇹"), "", domain.CountryUnknown},
		{"no flag", server("Config", "no flag"), "", domain.CountryUnknown},
		{"half a flag", server("Config", "half a flag \U0001f1e9"), "", domain.CountryUnknown},
		{"a rule matches the name and beats every flag", server("Frankfurt 🇳🇱", "🇳🇱 proxy"), "DE", domain.CountryRule},
	}
	for _, c := range cases {
		if got := resolver.country(c.server); got.Code != c.code || got.Source != c.source {
			t.Errorf("%s: %+v, want %s from %d", c.name, got, c.code, c.source)
		}
	}
}

// A config Sweden 🇸🇪 whose outbound is tagged 🇳🇱 EDGE-STANDBY gives that server the Netherlands,
// while its name keeps both parts and shows no flag.
func TestATagsFlagBeatsItsConfigsFlag(t *testing.T) {
	trojan := func(tag, host string) string {
		return fmt.Sprintf(`{"tag":%q,"protocol":"trojan","settings":{"address":%q,"port":443,"password":"pw"}}`, tag, host)
	}
	feed := fmt.Sprintf(`[{"remarks":"Sweden 🇸🇪","outbounds":[%s,%s]}]`, trojan("proxy", "se.example.net"), trojan("🇳🇱 EDGE-STANDBY", "nl.example.net"))
	doc, err := Parse([]byte(feed), Detect, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]domain.Country{}
	for _, e := range doc.Entries() {
		got[PlainLabel(e.Label)] = e.Country
	}
	want := map[string]domain.Country{
		"Sweden · EDGE-STANDBY": {Code: "NL", Source: domain.CountryTagFlag},
		"Sweden · proxy":        {Code: "SE", Source: domain.CountryConfigFlag},
	}
	if !maps.Equal(got, want) {
		t.Errorf("countries = %+v, want %+v", got, want)
	}
}

func TestLabelChangeAloneCannotChangeIdentity(t *testing.T) {
	r := newReconciled(t)
	r.apply(acmeFixture(t))
	before := map[string]domain.LineageID{}
	for id, l := range r.lineages {
		before[l.Label] = id
	}
	renamed := []byte(strings.ReplaceAll(string(acmeFixture(t)), "Sweden", "Svealand"))
	r.apply(renamed)
	if len(r.lineages) != 12 {
		t.Fatalf("lineages = %d, want the same 12", len(r.lineages))
	}
	if l := r.lineages[before["Sweden 🇸🇪 · proxy"]]; l.Label != "Svealand 🇸🇪 · proxy" || l.Presence != domain.Present {
		t.Errorf("renamed lineage = %q %v", l.Label, l.Presence)
	}
	moved := []byte(strings.ReplaceAll(strings.ReplaceAll(string(renamed), "node1.fixture", "elsewhere.fixture"), "aaaaaaaa-0000-4000-8000-000000000001", "cccccccc-0000-4000-8000-000000000001"))
	rec := r.apply(moved)
	if got := len(r.lineages); got != 12 {
		t.Errorf("lineages = %d, want the same 12 for a server with every connection key changed under its own label", got)
	}
	if len(rec.Replaced) != 1 {
		t.Errorf("replaced = %+v, want the one moved server", rec.Replaced)
	}
}

func TestTheStoredServerIsTheLeastRootTag(t *testing.T) {
	doc, err := Parse(reorderAcme(t), Detect, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range doc.Entries() {
		root, _ := e.Server.Closure()
		for _, s := range e.Seen {
			if string(root) > s.Tag {
				t.Errorf("%s: root %q is above tag %q", e.Label, root, s.Tag)
			}
		}
	}
}

func TestOccurrencesCountAgainstTheEntryLimit(t *testing.T) {
	if _, err := Parse(acmeFixture(t), Detect, Limits{Entries: 77}); err == nil {
		t.Error("78 occurrences passed a limit of 77")
	}
	if _, err := Parse(acmeFixture(t), Detect, Limits{Entries: 78}); err != nil {
		t.Errorf("78 occurrences at a limit of 78: %v", err)
	}
}

func keyedConfig(name string, hosts ...string) string {
	var out []string
	for i, host := range hosts {
		out = append(out, fmt.Sprintf(`{"tag":"proxy%d","protocol":"trojan","settings":{"address":%q,"port":443,"password":"pw"}}`, i, host+".example.net"))
	}
	return fmt.Sprintf(`{"remarks":%q,"outbounds":[%s]}`, name, strings.Join(out, ","))
}

// Each config keeps its key through a reorder and the other's removal, however many roots they share.
func TestConfigKeysOutliveAReorderAndARemoval(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []string
	}{
		{"one root shared", []string{"a", "b"}, []string{"a", "c"}},
		{"one inside the other", []string{"a"}, []string{"a", "b"}},
		{"the same roots", []string{"a", "b"}, []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconciled(t)
			a, b := keyedConfig("A", tc.a...), keyedConfig("B", tc.b...)
			r.apply([]byte("[" + a + "," + b + "]"))
			keyA, keyB := r.configKey("A"), r.configKey("B")
			for _, doc := range []string{"[" + b + "," + a + "]", "[" + a + "]", "[" + a + "," + b + "]"} {
				r.apply([]byte(doc))
				if got := r.configKey("A"); got != keyA {
					t.Errorf("%s: A's key = %s, want %s", doc, got, keyA)
				}
				if strings.Contains(doc, `"B"`) {
					if got := r.configKey("B"); got != keyB {
						t.Errorf("%s: B's key = %s, want %s", doc, got, keyB)
					}
				}
			}
		})
	}
}

// A config no old key goes to takes the least lineage ID among its roots, so the order of its
// outbounds gives it no other key and no other names.
func TestANewConfigsKeyIgnoresTheOrderOfItsOutbounds(t *testing.T) {
	first, reordered := newReconciled(t), newReconciled(t)
	first.apply([]byte("[" + keyedConfig("A", "a", "b", "c") + "," + keyedConfig("B", "d", "a") + "]"))
	reordered.apply([]byte("[" + keyedConfig("A", "c", "a", "b") + "," + keyedConfig("B", "a", "d") + "]"))
	for _, name := range []string{"A", "B"} {
		if got, want := reordered.configKey(name), first.configKey(name); got != want {
			t.Errorf("%s: key %s after a reorder, want %s", name, got, want)
		}
	}
	ids := func(r *reconciled) map[domain.Digest]domain.LineageID {
		out := map[domain.Digest]domain.LineageID{}
		for id, l := range r.lineages {
			out[l.Identity.Strong] = id
		}
		return out
	}
	if got, want := ids(reordered), ids(first); !maps.Equal(got, want) {
		t.Errorf("lineage IDs = %v after a reorder, want %v", got, want)
	}
}

// A newcomer can't outvote a config by naming a root it shares more than once, nor by coming first
// with the same roots and more beside them.
func TestANewcomerTakesNoKeyByRepeatsOrPlace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		next     func(a string) string
		renamedA string
	}{
		{"repeating a shared root", func(a string) string { return "[" + a + "," + keyedConfig("New", "a", "a", "a", "new") + "]" }, "A"},
		{"first, renamed, more roots", func(string) string {
			return "[" + keyedConfig("New", "a", "other-a", "new") + "," + keyedConfig("A2", "a", "other-a") + "]"
		}, "A2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconciled(t)
			a := keyedConfig("A", "a", "other-a")
			r.apply([]byte("[" + a + "]"))
			key := r.configKey("A")
			before := r.under(key)
			r.apply([]byte(tc.next(a)))
			if got := r.configKey(tc.renamedA); got != key {
				t.Errorf("A's key = %s, want %s", got, key)
			}
			if after := r.under(key); !slices.Equal(after, before) {
				t.Errorf("under A's key = %v, was %v", after, before)
			}
		})
	}
}

// Configs come in the order the document lists them, each with its place in it, whatever their names.
// A document that only lists its configs in another order moves them and changes nothing else.
func TestConfigsComeInDocumentOrder(t *testing.T) {
	r := newReconciled(t)
	r.apply(acmeFixture(t))
	inOrder := func(want ...string) {
		t.Helper()
		var got []string
		for i, row := range r.keyed() {
			if row.Place != i+1 {
				t.Errorf("config %s at row %d has place %d", row.Name, i+1, row.Place)
			}
			got = append(got, row.Name)
		}
		if !slices.Equal(got, want) {
			t.Errorf("configs %q, want %q", got, want)
		}
	}
	listed := []string{"Germany 🇩🇪", "Netherlands 🇳🇱", "Sweden 🇸🇪", "Bulgaria 🇧🇬", "USA 🇺🇸", "Romania 🇷🇴"}
	inOrder(listed...)

	var configs []json.RawMessage
	if err := json.Unmarshal(acmeFixture(t), &configs); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(configs)
	reversed, _ := json.Marshal(configs)
	if rec := r.apply(reversed); len(rec.Changes) != 0 || len(rec.Restated) != 12 {
		t.Errorf("the configs in another order made %d changes and restated %d, want none and all 12", len(rec.Changes), len(rec.Restated))
	}
	slices.Reverse(listed)
	inOrder(listed...)
}

// A config's place is its item in the provider's list, so a config with no runnable server still
// takes its place and the next one keeps the number the provider gave it.
func TestAConfigWithNoServerKeepsItsPlace(t *testing.T) {
	vless := func(tag string) string {
		return `{"tag": "` + tag + `", "protocol": "vless", "settings": {"vnext": [{"address": "a.example.net", "port": 443,
			"users": [{"id": "` + linkUUID + `", "encryption": "none"}]}]},
			"streamSettings": {"network": "tcp", "security": "tls", "tlsSettings": {"serverName": "a.example.net"}}}`
	}
	direct := `{"tag": "direct", "protocol": "freedom"}`
	link := "vless://" + linkUUID + "@a.example.net:443?security=tls&sni=a.example.net#Proxy"
	for _, tc := range []struct{ name, doc string }{
		{"xray utility only", `[{"remarks": "Direct", "outbounds": [` + direct + `]}, {"remarks": "Proxy", "outbounds": [` + vless("proxy") + `]}]`},
		{"xray duplicate tag", `[{"remarks": "Twice", "outbounds": [` + vless("a") + `, ` + vless("a") + `]}, {"remarks": "Proxy", "outbounds": [` + vless("proxy") + `]}]`},
		{"sing-box utility first", `{"outbounds": [{"type": "direct", "tag": "direct"}, {"type": "vless", "tag": "Proxy", "server": "a.example.net",
			"server_port": 443, "uuid": "` + linkUUID + `", "tls": {"enabled": true, "server_name": "a.example.net"}}]}`},
		{"links with a rejected one", "# header\nvless://" + linkUUID + "@a.example.net?security=tls#x\n" + link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconciled(t)
			r.apply([]byte(tc.doc))
			var places []int
			for _, row := range r.keyed() {
				places = append(places, row.Place)
			}
			if !slices.Equal(places, []int{2}) {
				t.Errorf("config places %v, want [2]", places)
			}
		})
	}
}

// feedOf is a document's feed as a source's first read reconciles it.
func feedOf(doc Document) domain.Feed {
	return Reconcile(nil, doc, "acme", 1, 1, t0, time.Hour).Feed
}

// shown is what a server shows that its place in its config must not decide.
type shown struct {
	label, country string
	member         bool
}

// showing reads a document's servers by their connection, as a first refresh stores them with the
// country rules, and whether a group whose names condition is names takes each one.
func showing(t *testing.T, data []byte, rules []CountryRule, names string) map[domain.Digest]shown {
	t.Helper()
	countries, err := newCountryResolver(CountryPolicy{LabelRules: rules})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parse(data, Detect, Limits{}, countries)
	if err != nil {
		t.Fatal(err)
	}
	var lineages []domain.Lineage
	for _, c := range Reconcile(nil, doc, "acme", 1, 1, t0, time.Hour).Changes {
		lineages = append(lineages, c.Lineage)
	}
	filter, err := domain.NewFilter(names, "")
	if err != nil {
		t.Fatal(err)
	}
	members := domain.NewCatalogue(1, lineages).Members(domain.Members{Names: filter}, nil)
	out := map[domain.Digest]shown{}
	for _, l := range lineages {
		member := slices.ContainsFunc(members, func(m domain.Lineage) bool { return m.ID == l.ID })
		out[l.Identity.Strong] = shown{label: l.Label, country: l.Country.Code, member: member}
	}
	return out
}

// unmoved says each server shows after what it showed before.
func unmoved(t *testing.T, before, after map[domain.Digest]shown) {
	t.Helper()
	if len(after) != len(before) {
		t.Errorf("servers = %d after, %d before", len(after), len(before))
	}
	for id, was := range before {
		if now := after[id]; now != was {
			t.Errorf("server %+v became %+v", was, now)
		}
	}
}

// No outbound's place in its config names a server, gives it a country or puts it in a group.
func TestMovingEachFirstOutboundLastChangesNoNameCountryOrGroup(t *testing.T) {
	rules := []CountryRule{{Regex: "^USA.* · EDGE-STANDBY$", Country: "CA"}}
	const group = "Sweden|USA"
	before := showing(t, acmeFixture(t), rules, group)
	after := showing(t, editAcme(t, func(configs []map[string]any) {
		for _, c := range configs {
			outbounds := c["outbounds"].([]any)
			c["outbounds"] = append(slices.Clone(outbounds[1:]), outbounds[0])
		}
	}), rules, group)
	unmoved(t, before, after)
	want := map[string]shown{
		"Sweden 🇸🇪 · proxy": {"Sweden 🇸🇪 · proxy", "SE", true}, "Sweden 🇸🇪 · EDGE-STANDBY": {"Sweden 🇸🇪 · EDGE-STANDBY", "SE", true},
		"USA 🇺🇸 · proxy": {"USA 🇺🇸 · proxy", "US", true}, "USA 🇺🇸 · EDGE-STANDBY": {"USA 🇺🇸 · EDGE-STANDBY", "CA", true},
		"Germany 🇩🇪 · proxy": {"Germany 🇩🇪 · proxy", "DE", false},
	}
	labels := map[string]shown{}
	for _, s := range before {
		labels[s.label] = s
	}
	for label, w := range want {
		if labels[label] != w {
			t.Errorf("%s = %+v, want %+v", label, labels[label], w)
		}
	}
	if len(before) != 12 {
		t.Errorf("servers = %d, want the 12 acme servers", len(before))
	}
}

// An outbound with no tag is named from what its config carries. Two of them in one config keep their
// names, countries and groups when the config lists them in the other order.
func TestUntaggedOutboundsKeepTheirNamesInAnyOrder(t *testing.T) {
	xray := func(hosts ...string) string {
		var out []string
		for _, host := range hosts {
			out = append(out, fmt.Sprintf(`{"protocol":"trojan","settings":{"address":%q,"port":443,"password":"pw"}}`, host))
		}
		return fmt.Sprintf(`[{"remarks":"Two","outbounds":[%s]}]`, strings.Join(out, ","))
	}
	singBox := func(hosts ...string) string {
		var out []string
		for _, host := range hosts {
			out = append(out, fmt.Sprintf(`{"type":"trojan","server":%q,"server_port":443,"password":"pw"}`, host))
		}
		return fmt.Sprintf(`{"outbounds":[%s]}`, strings.Join(out, ","))
	}
	rules := []CountryRule{{Regex: "^#0$", Country: "DE"}, {Regex: "^(Two|Server)$", Country: "NL"}}
	const group = "^#0$|(Two|Server) 2$"
	for _, tc := range []struct {
		name   string
		doc    func(...string) string
		labels []string
	}{
		{"xray", xray, []string{"Two", "Two 2"}},
		{"sing-box", singBox, []string{"Server", "Server 2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := showing(t, []byte(tc.doc("a.example.net", "b.example.net")), rules, group)
			after := showing(t, []byte(tc.doc("b.example.net", "a.example.net")), rules, group)
			unmoved(t, before, after)
			var labels []string
			members := 0
			for _, s := range before {
				labels = append(labels, s.label)
				if s.country != "NL" {
					t.Errorf("%s has country %q, want NL from its name", s.label, s.country)
				}
				if s.member {
					members++
				}
			}
			if slices.Sort(labels); !slices.Equal(labels, tc.labels) || members != 1 {
				t.Errorf("labels = %q with %d in the group, want %q with 1", labels, members, tc.labels)
			}
		})
	}
}

// An outbound with no tag still dials through a chain, though the chain uses the ID it would take.
func TestAnUntaggedOutboundDialsThroughAChainTaggedProxy(t *testing.T) {
	data := `[{"remarks":"Chain","outbounds":[
		{"protocol":"trojan","settings":{"address":"exit.example.net","port":443,"password":"pw"},"streamSettings":{"sockopt":{"dialerProxy":"proxy"}}},
		{"tag":"proxy","protocol":"trojan","settings":{"address":"entry.example.net","port":443,"password":"pw"}}]}]`
	doc, err := Parse([]byte(data), Detect, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	entries := doc.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want the chained server", entries)
	}
	if _, hops := entries[0].Server.Closure(); len(hops) != 2 || entries[0].Label != "Chain" {
		t.Errorf("%s has %d hops, want Chain with 2", entries[0].Label, len(hops))
	}
}
