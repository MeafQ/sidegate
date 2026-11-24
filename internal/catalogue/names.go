package catalogue

import (
	"bytes"
	"cmp"
	"slices"
	"strconv"
	"strings"

	"sidegate/internal/domain"
)

// unnamed labels a server no document names, as a share link with no fragment.
const unnamed = "Server"

// merge joins entries with the same provider ID and closure, whatever their labels. The stored server is
// the one with the bytewise least root tag, which is stable under reordering. Which
// duplicate represents a server doesn't move its revision.
func (d *Document) merge() {
	type key struct {
		provider string
		strong   domain.Digest
	}
	at := map[key]int{}
	merged := make([]Entry, 0, len(d.entries))
	for _, e := range d.entries {
		k := key{e.Identity.ProviderID, e.Identity.Strong}
		j, seen := at[k]
		switch {
		case !seen:
			j = len(merged)
			at[k] = j
			merged = append(merged, e)
		default:
			sightings := append(merged[j].Seen, e.Seen...)
			if representsBetter(e, merged[j]) {
				merged[j] = e
			}
			merged[j].Seen = sightings
		}
	}
	d.entries = merged
}

func representsBetter(a, b Entry) bool {
	rootA, _ := a.Server.Closure()
	rootB, _ := b.Server.Closure()
	if rootA != rootB {
		return rootA < rootB
	}
	da, db := revisionDigest(a.Identity.Strong, a.Server), revisionDigest(b.Identity.Strong, b.Server)
	return bytes.Compare(da[:], db[:]) < 0
}

// name sets every merged entry's naming, label, country and name key from the places it is seen
// in. An outbound's place in its config decides nothing.
func (d *Document) name(countries *countryResolver) {
	servers, identities := make([][]place, len(d.entries)), make([]domain.Identity, len(d.entries))
	for i, e := range d.entries {
		for _, s := range e.Seen {
			servers[i] = append(servers[i], place{config: s.Config, remarks: s.ConfigName, tag: s.Tag})
		}
		identities[i] = e.Identity
	}
	for i, n := range nameServers(servers, identities) {
		e := &d.entries[i]
		e.Label, e.Naming, e.home = n.numbered(), domain.Naming{Name: n.name, Number: n.number}, n.home
		e.Identity.NameKey = nameKey(n.name.String())
		e.Country = countries.country(n)
	}
}

// place is one config outbound a server is seen in, as naming reads it.
type place struct {
	config       int
	remarks, tag string
}

// named is what naming gives one server: its name, the config it is named from or -1, its number or
// 0, and what its country reads.
type named struct {
	name    domain.Name
	home    int
	number  int
	remarks string // The home config's remarks.
	tags    []string
}

func (n named) label() string { return cmp.Or(n.name.String(), unnamed) }

func (n named) numbered() string {
	if n.number == 0 {
		return n.label()
	}
	return n.label() + " " + strconv.Itoa(n.number)
}

// nameServers names each server from its places. A tag is a role when it sits on different servers in
// different configs. A place is a home when its tag is a role, when its config holds no other server, or
// when its config is the only one the server sits in. A server takes the home whose remarks sort first
// by bytes, then its tag, and is named by the parts it has there, which can be none. A server with no
// home is named by its most frequent tag alone, ties to the bytewise least, and with no tag it is named
// nowhere. A part of flags alone names nothing. Servers left with one label are numbered in identity
// order.
func nameServers(servers [][]place, identities []domain.Identity) []named {
	holds := map[int]map[int]bool{}
	sittings := map[string]struct{ configs, servers map[int]bool }{}
	for i, places := range servers {
		for _, p := range places {
			if holds[p.config] == nil {
				holds[p.config] = map[int]bool{}
			}
			holds[p.config][i] = true
			if p.tag == "" {
				continue
			}
			s, ok := sittings[p.tag]
			if !ok {
				s = struct{ configs, servers map[int]bool }{map[int]bool{}, map[int]bool{}}
				sittings[p.tag] = s
			}
			s.configs[p.config], s.servers[i] = true, true
		}
	}
	role := func(tag string) bool {
		s := sittings[tag]
		return len(s.configs) > 1 && len(s.servers) > 1
	}
	out := make([]named, len(servers))
	for i, places := range servers {
		configs := map[int]bool{}
		counts := map[string]int{}
		n := named{home: -1}
		for _, p := range places {
			configs[p.config] = true
			if p.tag != "" {
				counts[p.tag]++
				n.tags = append(n.tags, p.tag)
			}
		}
		var home *place
		for _, p := range places {
			if !role(p.tag) && len(holds[p.config]) > 1 && len(configs) > 1 {
				continue
			}
			if home == nil || cmp.Or(strings.Compare(p.remarks, home.remarks), strings.Compare(p.tag, home.tag)) < 0 {
				home = &p
			}
		}
		switch {
		case home != nil:
			n.name = newName(home.remarks, home.tag)
			n.home, n.remarks = home.config, home.remarks
		case len(counts) > 0:
			n.name = newName("", mostFrequent(counts))
		}
		out[i] = n
	}
	number(out, identities)
	return out
}

// newName names a server by the parts of a place that say more than a flag. The page draws a flag from
// the country, so a part of flags alone is no part, though its flag can still give the country.
func newName(remarks, tag string) domain.Name {
	part := func(text string) string {
		if withoutFlags(text) == "" {
			return ""
		}
		return text
	}
	n, _ := domain.NewName(part(remarks), part(tag))
	return n
}

func mostFrequent(counts map[string]int) string {
	best, most := "", 0
	for name, n := range counts {
		if n > most || n == most && name < best {
			best, most = name, n
		}
	}
	return best
}

// number gives the later servers of a label, in identity order, a number from 2. A number never lands
// on a label another server already asks for.
func number(servers []named, identities []domain.Identity) {
	asked := map[string]bool{}
	order := make([]int, len(servers))
	for i, n := range servers {
		asked[n.label()] = true
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return compareIdentities(identities[a], identities[b]) })
	used := map[string]bool{}
	for _, i := range order {
		n := &servers[i]
		for next := 2; used[n.numbered()]; next++ {
			if candidate := (named{name: n.name, number: next}).numbered(); !asked[candidate] && !used[candidate] {
				n.number = next
			}
		}
		used[n.numbered()] = true
	}
}

func compareEntries(a, b Entry) int { return compareIdentities(a.Identity, b.Identity) }

func compareIdentities(a, b domain.Identity) int {
	return cmp.Or(bytes.Compare(a.Strong[:], b.Strong[:]), cmp.Compare(a.ProviderID, b.ProviderID))
}
