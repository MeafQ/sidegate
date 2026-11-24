package domain

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

type GroupID string
type GroupRevision uint64

// Group is what a slot's Exit can name: the present servers its rule matches. Ticked is a fact about
// each server in this group, sorted. It changes no membership while the rule's box condition is unset.
type Group struct {
	ID       GroupID
	Revision GroupRevision
	Name     string
	Members  Members
	Ticked   []LineageID
}

// Box is the rule's condition on a server's tick in its group.
type Box uint8

const (
	BoxUnset Box = iota
	BoxTicked
	BoxUnticked
)

func (b Box) Set() bool { return b != BoxUnset }

func (b Box) Holds(ticked bool) bool { return b == BoxUnset || ticked == (b == BoxTicked) }

// Members is a group's rule: the servers every set condition matches, the box among them. Conditions
// that are all unset match nothing, so a hand-picked group is "box is ticked" alone.
type Members struct {
	Sources   Among[SourceID]
	Countries Among[string]
	Protocols Among[Protocol]
	Names     Filter // Matches the label or any tag the server was seen under.
	Box       Box
}

// AdmitMembers upper-cases country codes, so they compare with the catalogue's.
func AdmitMembers(m Members) (Members, error) {
	countries, err := admitCountries(m.Countries)
	if err != nil {
		return Members{}, err
	}
	if m.Box > BoxUnticked {
		return Members{}, fmt.Errorf("box condition %d is not one", m.Box)
	}
	return Members{Sources: m.Sources.clone(), Countries: countries, Protocols: m.Protocols.clone(), Names: m.Names, Box: m.Box}, nil
}

// AdmitTicked sorts ticks and refuses an empty or repeated one.
func AdmitTicked(ids []LineageID) ([]LineageID, error) {
	for i, id := range ids {
		if id == "" || slices.Contains(ids[:i], id) {
			return nil, fmt.Errorf("server %q is empty or repeated", id)
		}
	}
	return slices.Sorted(slices.Values(ids)), nil
}

func admitCountries(a Among[string]) (Among[string], error) {
	out := Among[string]{Not: a.Not, Values: []string{}}
	for _, code := range a.Values {
		if len(code) != 2 {
			return Among[string]{}, fmt.Errorf("country %q is not a two-letter code", code)
		}
		out.Values = append(out.Values, strings.ToUpper(code))
	}
	return out, nil
}

// Bare says g can hold no server whatever the catalogue lists: no condition is set, or the box is
// ticked and nothing is ticked, since the conditions join by AND. A create is refused bare; a later
// edit may leave a group so.
func (g Group) Bare() bool {
	return !g.Members.Conditioned() || g.Members.Box == BoxTicked && len(g.Ticked) == 0
}

// Conditioned says some condition is set, the box included, so the rule matches something.
func (m Members) Conditioned() bool {
	include, exclude := m.Names.Patterns()
	return m.Sources.Set() || m.Countries.Set() || m.Protocols.Set() || include != "" || exclude != "" || m.Box.Set()
}

func (m Members) matches(l Lineage) bool {
	return m.Sources.Holds(l.Source) && m.Countries.Holds(l.Country.Code) && m.Protocols.Holds(l.Protocol()) && m.Names.Pass(l.names()...)
}

// Matched lists what every set condition but the box matches, in ID order: present servers. A rule
// with no condition set, the box included, matches none. It can't fail and isn't bounded, so a rule
// never closes a serving slot by growing.
func (c Catalogue) Matched(m Members) []Lineage {
	var out []Lineage
	if !m.Conditioned() {
		return out
	}
	for _, id := range c.scan(m) {
		if l := c.lineages[id]; l.Presence == Present && m.matches(l) {
			out = append(out, l)
		}
	}
	return out
}

// Members is the membership rule: what Matched lists that the box condition holds, given the
// group's sorted ticks.
func (c Catalogue) Members(m Members, ticked []LineageID) []Lineage {
	return slices.DeleteFunc(c.Matched(m), func(l Lineage) bool {
		_, on := slices.BinarySearch(ticked, l.ID)
		return !m.Box.Holds(on)
	})
}

// scan picks the index that bounds the scan, in ID order.
func (c Catalogue) scan(m Members) []LineageID {
	var ids []LineageID
	switch {
	case m.Countries.Set() && !m.Countries.Not:
		for _, code := range m.Countries.Values {
			ids = append(ids, c.byCountry[code]...)
		}
	case m.Sources.Set() && !m.Sources.Not:
		for _, s := range m.Sources.Values {
			ids = append(ids, c.bySource[s]...)
		}
	default:
		ids = slices.Collect(maps.Keys(c.lineages))
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// WithoutSource decides what deleting source id does to g. It drops id from the sources condition and
// that source's servers from the ticks. It reports removed when an is-any-of that named only this
// source is left with none, because an unset condition means any source and a group must not widen
// silently, and when a group that is "box is ticked" alone loses the last of its ticks. An is-none-of
// left with none stays set and excludes nothing, which is what it matched among the sources left.
func (g Group) WithoutSource(id SourceID, c Catalogue) (next Group, changed, removed bool) {
	m := g.Members
	sources := slices.DeleteFunc(slices.Clone(m.Sources.Values), func(s SourceID) bool { return s == id })
	ticked := slices.DeleteFunc(slices.Clone(g.Ticked), func(l LineageID) bool {
		row, known := c.Lookup(l)
		return known && row.Source == id
	})
	if len(sources) == len(m.Sources.Values) && len(ticked) == len(g.Ticked) {
		return g, false, false
	}
	if !m.Sources.Not && len(m.Sources.Values) > 0 && len(sources) == 0 || m.TicksAlone() && len(g.Ticked) > 0 && len(ticked) == 0 {
		return g, true, true
	}
	m.Sources.Values = sources
	next = g
	next.Members, next.Ticked = m, ticked
	return next, true, false
}

// TicksAlone says the box is ticked and no other condition is set, so the ticks are the group.
func (m Members) TicksAlone() bool {
	rest := m
	rest.Box = BoxUnset
	return m.Box == BoxTicked && !rest.Conditioned()
}
