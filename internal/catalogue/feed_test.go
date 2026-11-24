package catalogue

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

func describeFeed(f domain.Feed) []string {
	out := []string{}
	for _, c := range f.Configs {
		line := fmt.Sprintf("%d %s: %d servers, keyed %v", c.Place, c.Name, c.Servers, c.Key != "")
		for _, s := range c.Skipped {
			switch s.Reason {
			case domain.SkipHelper:
				line += fmt.Sprintf(" | %d %s helper %s", s.Index, s.Tag, s.Protocol)
			case domain.SkipHop:
				line += fmt.Sprintf(" | %d %s hop", s.Index, s.Tag)
			case domain.SkipRefused:
				line += fmt.Sprintf(" | %d %s refused at %d: %s %s", s.Index, s.Tag, s.Position, s.Code, s.Field)
			}
		}
		out = append(out, line)
	}
	return out
}

const acmeHelpers = " | 14 DIRECT helper freedom | 15 BLOCK helper blackhole | 16 REJECT helper blackhole | 17 LOOPBACK helper loopback | 18 DNS helper dns"

func TestFeedAcme(t *testing.T) {
	r := newReconciled(t)
	rec := r.apply(acmeFixture(t))
	want := []string{}
	for i, name := range []string{"Germany 🇩🇪", "Netherlands 🇳🇱", "Sweden 🇸🇪", "Bulgaria 🇧🇬", "USA 🇺🇸", "Romania 🇷🇴"} {
		want = append(want, fmt.Sprintf("%d %s: 13 servers, keyed true%s", i+1, name, acmeHelpers))
	}
	if got := describeFeed(rec.Feed); !slices.Equal(got, want) {
		t.Errorf("feed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	keys := map[domain.LineageID]bool{}
	for _, l := range r.lineages {
		for _, o := range l.Seen {
			keys[o.Config] = true
		}
	}
	for _, f := range rec.Feed.Configs {
		if !keys[f.Key] {
			t.Errorf("%s: key %s names no place a server was seen", f.Name, f.Key)
		}
	}
}

// Each of the 6 configs is home to the two servers it calls by a role, and names them.
func TestNamingAcme(t *testing.T) {
	r := newReconciled(t)
	r.apply(acmeFixture(t))
	want := map[string]domain.Naming{}
	for remarks := range acmeConfigs {
		for _, role := range acmeRoles {
			name, _ := domain.NewName(remarks, role)
			want[remarks+" · "+role] = domain.Naming{Name: name, Home: r.configKey(remarks)}
		}
	}
	if len(r.lineages) != len(want) {
		t.Errorf("lineages = %d, want %d", len(r.lineages), len(want))
	}
	for _, l := range r.lineages {
		if l.Naming != want[l.Label] {
			t.Errorf("%s: naming %+v, want %+v", l.Label, l.Naming, want[l.Label])
		}
	}
}

func TestNamingNumber(t *testing.T) {
	links := strings.Join([]string{
		`trojan://pw@a.example.net:443?security=none#Edge`,
		`trojan://pw@b.example.net:443?security=none#Edge`,
		`trojan://pw@c.example.net:443?security=none`,
	}, "\n")
	r := newReconciled(t)
	r.apply([]byte(links))
	got := map[string]domain.Naming{}
	for _, l := range r.lineages {
		got[l.Label] = l.Naming
	}
	edge, _ := domain.NewName("Edge", "")
	homes := map[domain.LineageID]bool{}
	for _, label := range []string{"Edge", "Edge 2"} {
		if n := got[label]; n.Name != edge || n.Home == "" || homes[n.Home] {
			t.Errorf("%s: naming %+v, want Edge from a config of its own", label, n)
		}
		homes[got[label].Home] = true
	}
	if got["Edge"].Number != 0 || got["Edge 2"].Number != 2 || !got["Server"].Name.IsZero() || got["Server"].Home == "" || len(got) != 3 {
		t.Errorf("namings = %+v, want Edge, Edge with 2 and an unnamed Server in a config of its own", got)
	}
}

func TestFeedRemnawave(t *testing.T) {
	doc := parseFile(t, "remnawave-xray.json")
	feed := Reconcile(nil, doc, "rw", 1, 1, t0, 0).Feed
	want := []string{
		"1 🇩🇪 Frankfurt: 1 servers, keyed true | 2 direct helper freedom | 3 block helper blackhole",
		"2 🇳🇱 Amsterdam via Frankfurt: 1 servers, keyed true | 2 relay hop | 3 direct helper freedom",
		"3 🇫🇮 Helsinki pinned: 1 servers, keyed true",
		"4 🇵🇱 Warsaw insecure: 0 servers, keyed false | 1 proxy refused at 1: insecure_tls allowInsecure",
		"5 🇹🇷 Istanbul fragment: 1 servers, keyed true | 2 fragment helper freedom",
		"6 🇸🇪 Stockholm hysteria: 0 servers, keyed false | 1 proxy refused at 1: malformed_entry hysteriaSettings",
	}
	if got := describeFeed(feed); !slices.Equal(got, want) {
		t.Errorf("feed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	istanbul := doc.Entries()[3]
	_, hops := istanbul.Server.Closure()
	mask := []domain.TCPMask{{Type: domain.MaskFragment, Fragment: &domain.FragmentMask{Packets: "tlshello", Length: "100-200", Delay: "10-20"}}}
	if len(hops) != 1 || hops[0].DialVia != "" || !reflect.DeepEqual(hops[0].Transport.TCPMasks, mask) {
		t.Errorf("%s: hops = %+v, want one that fragments its own hello", istanbul.Label, hops)
	}
	for _, e := range doc.Entries() {
		if e.Naming.Name.Tag() != "proxy" || e.Label != e.Naming.Name.Remarks()+" · proxy" || e.home < 0 {
			t.Errorf("%s: naming %+v, want its config's remarks and proxy", e.Label, e.Naming)
		}
	}
}

func TestFeedListsAConfigOfHelpersOnly(t *testing.T) {
	data := []byte(`[{"remarks":"Only helpers","outbounds":[{"tag":"direct","protocol":"freedom"},{"tag":"block","protocol":"blackhole"}]},
		{"remarks":"One","outbounds":[{"tag":"proxy","protocol":"trojan","settings":{"servers":[{"address":"a.example.net","port":443,"password":"pw"}]}}]}]`)
	doc, err := Parse(data, Detect, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1 Only helpers: 0 servers, keyed false | 1 direct helper freedom | 2 block helper blackhole", "2 One: 1 servers, keyed true"}
	if got := describeFeed(Reconcile(nil, doc, "s", 1, 1, t0, 0).Feed); !slices.Equal(got, want) {
		t.Errorf("feed = %q, want %q", got, want)
	}
}

func TestFeedListsAConfigRefusedForADuplicateTag(t *testing.T) {
	trojan := func(tag, host string) string {
		return `{"tag":"` + tag + `","protocol":"trojan","settings":{"servers":[{"address":"` + host + `","port":443,"password":"pw"}]}}`
	}
	data := []byte(`[{"remarks":"Good","outbounds":[` + trojan("proxy", "g.example.net") + `]},
		{"remarks":"Refused config","outbounds":[` + trojan("duplicate", "a.example.net") + `,` + trojan("duplicate", "b.example.net") + `,{"tag":"direct","protocol":"freedom"}]}]`)
	doc, err := Parse(data, Detect, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1 Good: 1 servers, keyed true",
		"2 Refused config: 0 servers, keyed false | 1 duplicate refused at 1: duplicate_tag tag | 2 duplicate refused at 2: duplicate_tag tag | 3 direct helper freedom"}
	if got := describeFeed(Reconcile(nil, doc, "s", 1, 1, t0, 0).Feed); !slices.Equal(got, want) {
		t.Errorf("feed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// An Xray config's remarks and its outbound's tag are two names, even when they read alike.
func TestAnXrayTagLikeItsRemarksIsStillATag(t *testing.T) {
	r := newReconciled(t)
	r.apply([]byte(`[{"remarks":"R","outbounds":[{"tag":"R","protocol":"trojan","settings":{"servers":[{"address":"r.example.net","port":443,"password":"pw"}]}}]}]`))
	for _, l := range r.lineages {
		if want, _ := domain.NewName("R", "R"); l.Label != "R · R" || l.Naming.Name != want {
			t.Errorf("named %q %+v, want R · R", l.Label, l.Naming)
		}
	}
}

// A tag that repeats its config's remarks still carries its flag, and the tags' flag comes first.
func TestATagLikeItsRemarksGivesItsFlag(t *testing.T) {
	trojan := func(tag string) string {
		return `{"tag":"` + tag + `","protocol":"trojan","settings":{"servers":[{"address":"one.example.net","port":443,"password":"pw"}]}}`
	}
	r := newReconciled(t)
	r.apply([]byte(`[{"remarks":"A 🇸🇪","outbounds":[` + trojan("proxy") + `]},{"remarks":"Z 🇳🇱","outbounds":[` + trojan("Z 🇳🇱") + `]}]`))
	if len(r.lineages) != 1 {
		t.Fatalf("lineages = %d, want the one connection", len(r.lineages))
	}
	for _, l := range r.lineages {
		if l.Label != "A 🇸🇪 · proxy" || l.Country != (domain.Country{Code: "NL", Source: domain.CountryTagFlag}) {
			t.Errorf("named %q from %+v, want A 🇸🇪 · proxy in NL from its tags' flag", l.Label, l.Country)
		}
	}
}

// A config with no remarks is still home to the untagged servers that sit in it alone.
func TestAnUnnamedConfigIsHomeToItsServers(t *testing.T) {
	untagged := func(host string) string {
		return `{"protocol":"trojan","settings":{"servers":[{"address":"` + host + `","port":443,"password":"pw"}]}}`
	}
	r := newReconciled(t)
	r.apply([]byte(`[{"outbounds":[` + untagged("a.example.net") + `,` + untagged("b.example.net") + `]}]`))
	keys := r.keyed()
	if len(keys) != 1 {
		t.Fatalf("keyed configs = %+v, want one", keys)
	}
	got := map[string]domain.Naming{}
	for _, l := range r.lineages {
		got[l.Label] = l.Naming
	}
	want := map[string]domain.Naming{"Server": {Home: keys[0].Key}, "Server 2": {Home: keys[0].Key, Number: 2}}
	if !maps.Equal(got, want) {
		t.Errorf("namings = %+v, want %+v", got, want)
	}
}

// A part of flags alone names nothing, since the page draws a flag from the country and never in a name.
// The flag still gives the country, and a rule reads the parts that are left. The name, its key and the
// label agree.
func TestAFlagAloneIsNoPartOfAName(t *testing.T) {
	countries, err := newCountryResolver(CountryPolicy{LabelRules: []CountryRule{{Regex: "Stockholm", Country: "FI"}}})
	if err != nil {
		t.Fatal(err)
	}
	trojan := func(tag string) string {
		return `{"tag":"` + tag + `","protocol":"trojan","settings":{"servers":[{"address":"one.example.net","port":443,"password":"pw"}]}}`
	}
	name := func(remarks, tag string) domain.Name {
		n, _ := domain.NewName(remarks, tag)
		return n
	}
	for _, c := range []struct {
		config, label string
		name          domain.Name
		country       domain.Country
	}{
		{`{"remarks":"🇸🇪","outbounds":[` + trojan("🇸🇪") + `]}`, "Server", domain.Name{}, domain.Country{Code: "SE", Source: domain.CountryTagFlag}},
		{`{"outbounds":[` + trojan("🇸🇪") + `]}`, "Server", domain.Name{}, domain.Country{Code: "SE", Source: domain.CountryTagFlag}},
		{`{"remarks":"🇸🇪 🇸🇪","outbounds":[` + trojan("proxy") + `]}`, "proxy", name("", "proxy"), domain.Country{Code: "SE", Source: domain.CountryConfigFlag}},
		{`{"remarks":"Stockholm","outbounds":[` + trojan("🇸🇪") + `]}`, "Stockholm", name("Stockholm", ""), domain.Country{Code: "FI", Source: domain.CountryRule}},
		{`{"remarks":"🇸🇪 Stockholm","outbounds":[` + trojan("🇸🇪") + `]}`, "🇸🇪 Stockholm", name("🇸🇪 Stockholm", ""), domain.Country{Code: "FI", Source: domain.CountryRule}},
	} {
		doc, err := parse([]byte(`[`+c.config+`]`), Detect, Limits{}, countries)
		if err != nil {
			t.Fatal(err)
		}
		rec := Reconcile(nil, doc, "s", 1, 1, t0, 0)
		if len(rec.Changes) != 1 {
			t.Fatalf("%s: %d servers, want one", c.config, len(rec.Changes))
		}
		l := rec.Changes[0].Lineage
		if l.Label != c.label || l.Naming.Name != c.name || l.Naming.Home == "" || l.Country != c.country || l.Identity.NameKey != nameKey(c.name.String()) {
			t.Errorf("%s: named %q %+v in %+v, want %q %+v at its home in %+v", c.config, l.Label, l.Naming, l.Country, c.label, c.name, c.country)
		}
	}
}
