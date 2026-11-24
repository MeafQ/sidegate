package catalogue

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

type server struct{ name, id, host, port, key, network, path string }

func (s server) link() string {
	return fmt.Sprintf("vless://%s@%s:%s?security=reality&pbk=%s&sni=a.example.com&sid=ab&fp=chrome&type=%s&path=%s#%s",
		s.id, s.host, s.port, s.key, s.network, s.path, url.PathEscape(s.name))
}

func links(servers []server) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s.link()
	}
	return out
}

func continuityBase() []server {
	return []server{
		{"Germany", "11111111-1111-4111-8111-111111111111", "de.example.net", "443", "key-de", "tcp", "/"},
		{"Sweden", "22222222-2222-4222-8222-222222222222", "se.example.net", "443", "key-se", "tcp", "/"},
		{"Nether", "33333333-3333-4333-8333-333333333333", "nl.example.net", "443", "key-nl", "xhttp", "/a"},
		{"SharedA", "44444444-4444-4444-8444-444444444444", "shared.example.net", "443", "key-sh", "tcp", "/"},
		{"SharedB", "55555555-5555-4555-8555-555555555555", "shared.example.net", "443", "key-sh", "tcp", "/"},
	}
}

func everyServer(edit func(*server)) func([]server) []server {
	return func(in []server) []server {
		out := slices.Clone(in)
		for i := range out {
			edit(&out[i])
		}
		return out
	}
}

func oneServer(i int, edit func(*server)) func([]server) []server {
	return func(in []server) []server {
		out := slices.Clone(in)
		edit(&out[i])
		return out
	}
}

func moveAndRotate(s *server) {
	s.host, s.port, s.key = "moved-"+s.host, "8443", "new-"+s.key
	s.id = "cccccccc" + s.id[8:]
}

// TestContinuity runs each provider change through Parse and Reconcile. follows maps a label
// before to the label after that must share its lineage, with nil meaning every label to
// itself. replaced lists the labels after that say "Server replaced".
func TestContinuity(t *testing.T) {
	cases := []struct {
		name      string
		base      func() []server
		edit      func([]server) []server
		follows   map[string]string
		fresh     []string
		missing   []string
		replaced  []string
		ambiguous int
	}{
		{
			name:    "slight rename of one",
			edit:    oneServer(0, func(s *server) { s.name = "Germany-1" }),
			follows: map[string]string{"Germany": "Germany-1", "Sweden": "Sweden", "Nether": "Nether", "SharedA": "SharedA", "SharedB": "SharedB"},
		},
		{
			name: "rename all",
			edit: everyServer(func(s *server) { s.name = "X-" + s.name }),
			follows: map[string]string{"Germany": "X-Germany", "Sweden": "X-Sweden", "Nether": "X-Nether",
				"SharedA": "X-SharedA", "SharedB": "X-SharedB"},
		},
		{
			name: "names of two swapped",
			edit: func(in []server) []server {
				out := slices.Clone(in)
				out[0].name, out[1].name = out[1].name, out[0].name
				return out
			},
			follows: map[string]string{"Germany": "Sweden", "Sweden": "Germany", "Nether": "Nether", "SharedA": "SharedA", "SharedB": "SharedB"},
		},
		{
			name: "reorder",
			edit: func(in []server) []server {
				out := slices.Clone(in)
				slices.Reverse(out)
				return out
			},
		},
		{name: "password rotated on one", edit: oneServer(0, func(s *server) { s.id = "aaaaaaaa" + s.id[8:] })},
		{
			name:     "password rotated on all",
			edit:     everyServer(func(s *server) { s.id = "bbbbbbbb" + s.id[8:] }),
			replaced: []string{"SharedA", "SharedB"},
		},
		{
			name:     "address changed on one",
			edit:     oneServer(0, func(s *server) { s.host = "de2.example.net" }),
			replaced: []string{"Germany"},
		},
		{
			name:     "port changed on one",
			edit:     oneServer(0, func(s *server) { s.port = "8443" }),
			replaced: []string{"Germany"},
		},
		{
			name:     "address changed on all",
			edit:     everyServer(func(s *server) { s.host = "new-" + s.host }),
			replaced: []string{"Germany", "Nether", "SharedA", "SharedB", "Sweden"},
		},
		{name: "reality key changed on one", edit: oneServer(0, func(s *server) { s.key = "new-key" })},
		{name: "transport path changed on one", edit: oneServer(2, func(s *server) { s.path = "/x" })},
		{
			name: "address and password changed, same name",
			edit: oneServer(0, func(s *server) {
				s.host = "de2.example.net"
				s.id = "cccccccc" + s.id[8:]
			}),
			replaced: []string{"Germany"},
		},
		{
			name: "address and key changed, same name",
			edit: oneServer(0, func(s *server) {
				s.host, s.key = "de2.example.net", "new-key"
			}),
			replaced: []string{"Germany"},
		},
		{
			name:     "everything changed, same name",
			edit:     oneServer(0, moveAndRotate),
			replaced: []string{"Germany"},
		},
		{
			name: "everything changed, name too",
			edit: oneServer(0, func(s *server) {
				moveAndRotate(s)
				s.name = "Deutschland"
			}),
			follows: map[string]string{"Sweden": "Sweden", "Nether": "Nether", "SharedA": "SharedA", "SharedB": "SharedB"},
			fresh:   []string{"Deutschland"},
			missing: []string{"Germany"},
		},
		{
			name: "two old servers with one name match by no name tier",
			base: func() []server {
				out := continuityBase()[:3]
				out[1].name = "Germany"
				return out
			},
			edit: func(in []server) []server {
				out := slices.Clone(in)
				moveAndRotate(&out[0])
				moveAndRotate(&out[1])
				return out
			},
			follows:   map[string]string{"Nether": "Nether"},
			fresh:     []string{"Germany", "Germany 2"},
			missing:   []string{"Germany", "Germany 2"},
			ambiguous: 4,
		},
		{
			name: "swapped names and moved addresses stay with their credentials",
			edit: func(in []server) []server {
				out := slices.Clone(in)
				out[0].name, out[1].name = out[1].name, out[0].name
				out[0].host, out[1].host = "moved-a.example.net", "moved-b.example.net"
				return out
			},
			follows:  map[string]string{"Germany": "Sweden", "Sweden": "Germany", "Nether": "Nether", "SharedA": "SharedA", "SharedB": "SharedB"},
			replaced: []string{"Germany", "Sweden"},
		},
		{
			name: "one user ID on all servers, addresses all changed, names kept",
			base: func() []server {
				return everyServer(func(s *server) { s.id = "99999999-9999-4999-8999-999999999999" })(continuityBase()[:4])
			},
			edit:     everyServer(func(s *server) { s.host = "new-" + s.host }),
			replaced: []string{"Germany", "Nether", "SharedA", "Sweden"},
		},
		{
			name: "a generated number is not a name",
			base: func() []server {
				out := continuityBase()[:3]
				out[1].name = "Germany"
				return out
			},
			edit: func(in []server) []server {
				out := slices.Clone(in)[1:]
				moveAndRotate(&out[0])
				out[0].name = "Germany 2"
				return out
			},
			follows: map[string]string{"Nether": "Nether"},
			fresh:   []string{"Germany 2"},
			missing: []string{"Germany", "Germany 2"},
		},
		{
			name: "a default name is not a name",
			base: func() []server {
				out := continuityBase()[:2]
				out[0].name = ""
				return out
			},
			edit:    oneServer(0, moveAndRotate),
			follows: map[string]string{"Sweden": "Sweden"},
			fresh:   []string{"Server"},
			missing: []string{"Server"},
		},
		{
			name: "a provider's own name of Server is a name",
			base: func() []server {
				out := continuityBase()[:2]
				out[0].name = "Server"
				return out
			},
			edit:     oneServer(0, moveAndRotate),
			replaced: []string{"Server"},
		},
		{
			name: "space and case are folded",
			edit: oneServer(0, func(s *server) {
				moveAndRotate(s)
				s.name = "  GERMANY "
			}),
			follows:  map[string]string{"Germany": "GERMANY", "Sweden": "Sweden", "Nether": "Nether", "SharedA": "SharedA", "SharedB": "SharedB"},
			replaced: []string{"GERMANY"},
		},
		{
			name: "a near name is not a name",
			edit: oneServer(0, func(s *server) {
				moveAndRotate(s)
				s.name = "Germany 1"
			}),
			follows: map[string]string{"Sweden": "Sweden", "Nether": "Nether", "SharedA": "SharedA", "SharedB": "SharedB"},
			fresh:   []string{"Germany 1"},
			missing: []string{"Germany"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := continuityBase
			if c.base != nil {
				base = c.base
			}
			sim := newSim(t, links(base())...)
			before := sim.present()
			r := sim.refresh(links(c.edit(base()))...)
			after := sim.present()

			follows := c.follows
			if follows == nil {
				follows = map[string]string{}
				for label := range before {
					follows[label] = label
				}
			}
			kept := 0
			for old, now := range follows {
				if after[now] == "" || after[now] != before[old] {
					t.Errorf("%q -> %q: lineage %q, want %q", old, now, after[now], before[old])
				}
				kept++
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
			if len(after) != kept+len(c.fresh) {
				t.Errorf("present = %d, want %d kept and %d new", len(after), kept, len(c.fresh))
			}
			if len(r.Ambiguous) != c.ambiguous {
				t.Errorf("ambiguous = %v, want %d", r.Ambiguous, c.ambiguous)
			}
			var replaced []string
			for _, e := range r.Replaced {
				replaced = append(replaced, e.Label)
				if sim.lineages[e.Lineage].Label != e.Label {
					t.Errorf("replaced %+v names a lineage labelled %q", e, sim.lineages[e.Lineage].Label)
				}
			}
			slices.Sort(replaced)
			if !slices.Equal(replaced, c.replaced) {
				t.Errorf("replaced = %q, want %q", replaced, c.replaced)
			}
			t.Logf("RESULT %-68s kept %d/%d new %d missing %d replaced %d ambiguous %d", c.name, kept, len(before), len(c.fresh), len(c.missing), len(replaced), len(r.Ambiguous))
		})
	}
}

func TestReplacedNamesTheLabelItHadBefore(t *testing.T) {
	base := continuityBase()[:1]
	sim := newSim(t, links(base)...)
	moved := oneServer(0, func(s *server) {
		s.host = "moved.example.net"
		s.name = "Deutschland"
	})(base)
	r := sim.refresh(links(moved)...)
	if len(r.Replaced) != 1 || r.Replaced[0].Previous != "Germany" || r.Replaced[0].Label != "Deutschland" {
		t.Fatalf("replaced = %+v, want Germany now named Deutschland", r.Replaced)
	}
}

func TestASameServerThroughTheNameTierIsNotReplaced(t *testing.T) {
	sip := func(otherID string) string {
		return fmt.Sprintf(`{"version": 1, "servers": [
			{"remarks": "Tokyo", "server": "t.example.net", "server_port": 8388, "password": "pw", "method": "aes-256-gcm"},
			{"id": %q, "remarks": "Osaka", "server": "t.example.net", "server_port": 8388, "password": "pw", "method": "aes-256-gcm"}]}`, otherID)
	}
	sim := &catalogueSim{t: t, lineages: map[domain.LineageID]domain.Lineage{}, now: t0}
	sim.apply(docOf(t, sip("one")))
	tokyo := sim.present()["Tokyo"]
	r := sim.apply(docOf(t, sip("two")))
	if got := sim.present()["Tokyo"]; got != tokyo {
		t.Fatalf("Tokyo: lineage %s, want %s", got, tokyo)
	}
	if len(r.Replaced) != 0 {
		t.Errorf("replaced = %+v, want none: the connection did not change", r.Replaced)
	}
}

func TestMissingWithinGraceStillMatches(t *testing.T) {
	for name, moved := range map[string]func(*server){
		"by credentials": func(s *server) { s.host = "moved.example.net"; s.name = "Deutschland" },
		"by name":        moveAndRotate,
	} {
		t.Run(name, func(t *testing.T) {
			servers := continuityBase()[:2]
			sim := newSim(t, links(servers)...)
			id := sim.present()["Germany"]
			sim.refresh(servers[1].link())
			if sim.presence(id) != domain.Missing {
				t.Fatalf("presence %d, want Missing", sim.presence(id))
			}
			sim.now = t0.Add(grace / 2)
			r := sim.refresh(links(oneServer(0, moved)(servers))...)
			if sim.presence(id) != domain.Present || len(r.Replaced) != 1 || r.Replaced[0].Lineage != id {
				t.Fatalf("presence %d, replaced %+v, want %s back and replaced", sim.presence(id), r.Replaced, id)
			}
		})
	}
}

func TestATombstoneNeverMatches(t *testing.T) {
	for name, edit := range map[string]func(*server){
		"same server":      func(*server) {},
		"same credentials": func(s *server) { s.host = "moved.example.net"; s.name = "Deutschland" },
		"same name":        moveAndRotate,
	} {
		t.Run(name, func(t *testing.T) {
			servers := continuityBase()[:2]
			sim := newSim(t, links(servers)...)
			id := sim.present()["Germany"]
			sim.refresh(servers[1].link())
			sim.now = t0.Add(grace)
			sim.refresh(servers[1].link())
			if sim.presence(id) != domain.Tombstone {
				t.Fatalf("presence %d, want Tombstone", sim.presence(id))
			}
			next := oneServer(0, edit)(servers)
			r := sim.refresh(links(next)...)
			if sim.presence(id) != domain.Tombstone || sim.present()[next[0].name] == id || len(r.Replaced) != 0 {
				t.Fatalf("presence %d, replaced %+v: the tombstone matched", sim.presence(id), r.Replaced)
			}
		})
	}
}

// Each config's first server and its EDGE twin are put on one host, so no connection key
// tells a rotated pair apart and only the names can.
func TestAcmeRotatedWithEveryTwinOnASharedHostKeepsAll(t *testing.T) {
	text := string(acmeFixture(t))
	for i := 1; i <= 11; i += 2 {
		text = strings.ReplaceAll(text, fmt.Sprintf("node%d.fixture", i+1), fmt.Sprintf("node%d.fixture", i))
	}
	r := newReconciled(t)
	r.apply([]byte(text))
	before := map[string]domain.LineageID{}
	hosts := map[domain.Digest]int{}
	for id, l := range r.lineages {
		before[l.Label] = id
		hosts[l.Identity.HostKey]++
	}
	if len(before) != 12 || len(hosts) != 6 {
		t.Fatalf("%d servers on %d host keys, want 12 on 6", len(before), len(hosts))
	}

	rec := r.apply([]byte(strings.ReplaceAll(text, "aaaaaaaa-", "bbbbbbbb-")))

	if len(r.lineages) != 12 {
		t.Errorf("lineages = %d, want the same 12", len(r.lineages))
	}
	for label, id := range before {
		if l := r.lineages[id]; l.Label != label || l.Presence != domain.Present {
			t.Errorf("%q: now %q, presence %d", label, l.Label, l.Presence)
		}
	}
	if len(rec.Replaced) != 12 {
		t.Errorf("replaced = %d, want all 12", len(rec.Replaced))
	}
}
