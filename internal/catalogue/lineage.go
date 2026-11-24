package catalogue

import (
	"cmp"
	"slices"
	"time"

	"sidegate/internal/domain"
)

type Change struct {
	Lineage  domain.Lineage
	Previous *domain.ServerRef // Nil for a new lineage.
}

// Reconciliation lists only lineages that changed. A refresh that only re-picks Reality values,
// moves configs to other places in the document or names a server from elsewhere changes none, and
// lists those lineages in Restated: no revision and no event, since none of that makes a server another
// one, but the controller stores them. Ambiguous names the lineages whose identity could not be told apart
// from a sibling: the old ones left missing and the new ones created in their place. Replaced names the
// lineages that kept their identity with no part of their connection in common. Feed is the document's
// configs under the keys their servers' Seen name.
type Reconciliation struct {
	Source    domain.SourceID
	Version   domain.SourceRevision
	Feed      domain.Feed
	Changes   []Change
	Restated  []domain.Lineage
	Ambiguous []domain.LineageID
	Replaced  []Replaced
}

// Replaced is one lineage a credential or name match carried across a changed connection.
type Replaced struct {
	Lineage         domain.LineageID
	Label, Previous string
}

// tier is one identity key, strongest first. A tie key may split a group the
// main key cannot, but is never matched on alone.
type tier struct {
	key func(domain.Identity) (string, bool)
	tie func(domain.Identity) string
}

func digestKey(pick func(domain.Identity) domain.Digest) func(domain.Identity) (string, bool) {
	return func(id domain.Identity) (string, bool) {
		d := pick(id)
		return string(d[:]), d != domain.Digest{}
	}
}

var connectionTiers = []tier{
	{key: func(id domain.Identity) (string, bool) { return id.ProviderID, id.ProviderID != "" }},
	{key: digestKey(func(id domain.Identity) domain.Digest { return id.Strong })},
	{key: digestKey(func(id domain.Identity) domain.Digest { return id.Endpoint })},
	{
		key: digestKey(func(id domain.Identity) domain.Digest { return id.HostKey }),
		tie: func(id domain.Identity) string { return string(id.RealityTie[:]) },
	},
}

// replacementTiers run after every connection tier, so they only see servers whose
// connection no longer points anywhere. A provider with one user ID on every server
// has no unique credential, and those servers fall through to the name.
var replacementTiers = []tier{
	{key: digestKey(func(id domain.Identity) domain.Digest { return id.CredentialKey })},
	{key: digestKey(func(id domain.Identity) domain.Digest { return id.NameKey })},
}

// Reconcile matches the entire source, independent of provider order. It is
// pure: new lineages get domain.NewLineageID(source, at, ordinal), where at is the
// catalogue revision the controller is about to commit.
//
// Each tier matches globally: an edge is accepted only when its key is unique
// among the still-unmatched old and new entries on both sides, so no early weak
// match can take an entry a stronger tier would claim. Entries left over are new
// lineages; old lineages left over go missing, and missing ones past their grace
// become tombstones that never match again. After matching it assigns config keys,
// because a new lineage has no ID before then, and fills Seen. A document that is neither non-empty nor explicitly empty changes nothing.
func Reconcile(previous []domain.Lineage, incoming Document, source domain.SourceID,
	version domain.SourceRevision, at domain.CatalogueRevision, now time.Time, grace time.Duration) Reconciliation {
	r := Reconciliation{Source: source, Version: version}
	if len(incoming.entries) == 0 && !incoming.explicitlyEmpty {
		return r
	}
	var olds []domain.Lineage
	for _, l := range previous {
		if l.Source != source || l.Presence == domain.Tombstone {
			continue
		}
		if buried, ok := l.Buried(now); ok {
			r.changed(buried, l)
			continue
		}
		olds = append(olds, l)
	}
	news := incoming.entries
	m := newMatcher(olds, news)
	for _, t := range connectionTiers {
		m.run(t)
	}
	connected := slices.Clone(m.oldTo)
	for _, t := range replacementTiers {
		m.run(t)
	}

	var fresh []int
	for j := range news {
		if m.newTo[j] < 0 {
			fresh = append(fresh, j)
		}
	}
	slices.SortFunc(fresh, func(a, b int) int { return compareEntries(news[a], news[b]) })
	ids := make([]domain.LineageID, len(news))
	for j, i := range m.newTo {
		if i >= 0 {
			ids[j] = olds[i].ID
		}
	}
	for ordinal, j := range fresh {
		ids[j] = domain.NewLineageID(source, at, ordinal)
	}
	keys := configKeys(olds, m.newTo, news, incoming.configs, ids, source, at)
	r.Feed = feed(incoming.configs, keys)
	seen := func(j int) []domain.Occurrence { return occurrences(news[j], keys) }
	homed := func(j int) Entry {
		e := news[j]
		if e.home >= 0 {
			e.Naming.Home = keys[e.home]
		}
		return e
	}

	for i, l := range olds {
		if j := m.oldTo[i]; j >= 0 {
			next := update(l, homed(j), now)
			next.Seen = seen(j)
			if connected[i] < 0 && news[j].Identity.Strong != l.Identity.Strong {
				r.Replaced = append(r.Replaced, Replaced{Lineage: l.ID, Label: next.Label, Previous: l.Label})
			}
			switch {
			case !sameLineage(next, l):
				r.changed(next, l)
			case !slices.EqualFunc(next.Reality, l.Reality, sameHopPicks) || !slices.Equal(next.Seen, l.Seen) || next.Naming != l.Naming:
				r.Restated = append(r.Restated, next)
			}
			continue
		}
		if m.oldAmbiguous[i] {
			r.Ambiguous = append(r.Ambiguous, l.ID)
		}
		if missed, ok := l.Unlisted(now, grace); ok {
			r.changed(missed, l)
		}
	}
	for _, j := range fresh {
		l := revise(domain.Lineage{ID: ids[j], Source: source}, homed(j))
		l.Seen = seen(j)
		r.Changes = append(r.Changes, Change{Lineage: l})
		if m.newAmbiguous[j] {
			r.Ambiguous = append(r.Ambiguous, ids[j])
		}
	}
	slices.SortFunc(r.Changes, func(a, b Change) int { return cmp.Compare(a.Lineage.ID, b.Lineage.ID) })
	slices.SortFunc(r.Restated, func(a, b domain.Lineage) int { return cmp.Compare(a.ID, b.ID) })
	slices.Sort(r.Ambiguous)
	slices.SortFunc(r.Replaced, func(a, b Replaced) int { return cmp.Compare(a.Lineage, b.Lineage) })
	return r
}

func feed(configs []config, keys []domain.LineageID) domain.Feed {
	f := domain.Feed{Configs: make([]domain.FeedConfig, 0, len(configs))}
	for ci, c := range configs {
		row := domain.FeedConfig{Place: c.place, Name: c.name, Servers: c.servers, Skipped: slices.Clone(c.skipped)}
		if c.servers > 0 {
			row.Key = keys[ci]
		}
		f.Configs = append(f.Configs, row)
	}
	return f
}

// occurrences turns an entry's sightings into stored form. They are sorted, so a document that only
// lists its servers in another order changes no more than config places.
func occurrences(e Entry, keys []domain.LineageID) []domain.Occurrence {
	out := make([]domain.Occurrence, 0, len(e.Seen))
	for _, s := range e.Seen {
		out = append(out, domain.Occurrence{Config: keys[s.Config], ConfigName: s.ConfigName, ConfigPlace: s.ConfigPlace, Tag: s.Tag, Position: s.Position})
	}
	slices.SortFunc(out, func(a, b domain.Occurrence) int {
		return cmp.Or(cmp.Compare(a.Config, b.Config), cmp.Compare(a.ConfigName, b.ConfigName), cmp.Compare(a.Tag, b.Tag), cmp.Compare(a.Position, b.Position),
			cmp.Compare(a.ConfigPlace, b.ConfigPlace))
	})
	return out
}

// configKeys names each config of the document so a server's places keep naming the same config. Old keys go first, across every config at once, so a config placed ahead of another never
// takes its key. A config claims a key with its distinct roots seen under it, so repeating a root adds
// nothing. The most such roots wins, then a config of the name seen under the key, so it survives a
// changed root, then the fewest roots the key never held, so a newcomer that adds roots to the
// same ones loses to the config that kept them. Document order decides only between configs no part
// of that tells apart. Otherwise a config takes the least lineage ID among its roots that no other
// config's key holds, so two configs sharing a root never merge, and last a key of its own.
func configKeys(olds []domain.Lineage, newTo []int, news []Entry, configs []config, ids []domain.LineageID,
	source domain.SourceID, at domain.CatalogueRevision) []domain.LineageID {
	roots := make([][]int, len(configs))
	for j, e := range news {
		for _, s := range e.Seen {
			roots[s.Config] = append(roots[s.Config], j)
		}
	}
	type claim struct {
		config         int
		key            domain.LineageID
		shared, others int
		named          bool
	}
	var claims []claim
	for ci, c := range configs {
		distinct := slices.Compact(slices.Sorted(slices.Values(roots[ci])))
		shared := map[domain.LineageID]int{}
		named := map[domain.LineageID]bool{}
		for _, j := range distinct {
			i := newTo[j]
			if i < 0 {
				continue
			}
			under := map[domain.LineageID]bool{}
			for _, o := range olds[i].Seen {
				if !under[o.Config] {
					under[o.Config] = true
					shared[o.Config]++
				}
				named[o.Config] = named[o.Config] || o.ConfigName == c.name
			}
		}
		for key, n := range shared {
			claims = append(claims, claim{ci, key, n, len(distinct) - n, named[key]})
		}
	}
	slices.SortFunc(claims, func(a, b claim) int {
		return cmp.Or(cmp.Compare(b.shared, a.shared), cmp.Compare(boolBit(b.named), boolBit(a.named)), cmp.Compare(a.others, b.others),
			cmp.Compare(a.key, b.key), cmp.Compare(a.config, b.config))
	})
	keys := make([]domain.LineageID, len(configs))
	taken := map[domain.LineageID]bool{}
	for _, c := range claims {
		if keys[c.config] == "" && !taken[c.key] {
			keys[c.config], taken[c.key] = c.key, true
		}
	}
	for ci := range configs {
		if keys[ci] != "" {
			continue
		}
		order := slices.Clone(roots[ci])
		slices.SortFunc(order, func(a, b int) int { return cmp.Compare(ids[a], ids[b]) })
		for _, j := range order {
			if !taken[ids[j]] {
				keys[ci] = ids[j]
				break
			}
		}
		if keys[ci] == "" {
			keys[ci] = domain.NewLineageID(source+"#config", at, ci)
		}
		taken[keys[ci]] = true
	}
	return keys
}

func (r *Reconciliation) changed(next, prev domain.Lineage) {
	ref := prev.Ref
	r.Changes = append(r.Changes, Change{Lineage: next, Previous: &ref})
}

func revise(l domain.Lineage, e Entry) domain.Lineage {
	l.Label, l.Naming, l.Identity, l.Server, l.Country = e.Label, e.Naming, e.Identity, e.Server, e.Country
	l.Ref = domain.ServerRef{Lineage: l.ID, Digest: revisionDigest(e.Identity.Strong, e.Server)}
	l.Reality = nil
	return l.Listed()
}

// update matches l to e. When the server differs from the stored one only in its
// Reality picks, the stored server stays, so a random re-pick is no revision.
// Each stored pick of each hop has its own absence, which a refresh that carries
// that pick on that hop resets, so a pick still in use is never rotated out.
// Once one is, the latest picks are adopted as a new revision. The window
// restarts with them, so a lineage adopts at most once per rotation window.
func update(l domain.Lineage, e Entry, now time.Time) domain.Lineage {
	next := revise(l, e)
	if e.Identity.Strong != l.Identity.Strong {
		return next
	}
	stored, latest := realityPicks(l.Server), realityPicks(e.Server)
	picks := make([]domain.RealityPicks, len(stored))
	carried := true
	for i, p := range stored {
		var prev domain.RealityPicks
		if i < len(l.Reality) {
			prev = l.Reality[i]
		}
		picks[i] = domain.RealityPicks{
			ServerName: prev.ServerName.Refreshed(p.serverName == latest[i].serverName, now),
			ShortID:    prev.ShortID.Refreshed(p.shortID == latest[i].shortID, now),
		}
		switch picks[i].State(now) {
		case domain.PickRotated:
			return next
		case domain.PickMissing:
			carried = false
		}
	}
	if carried {
		picks = nil
	}
	next.Server, next.Ref, next.Reality = l.Server, l.Ref, picks
	return next
}

type realityPick struct{ serverName, shortID string }

// realityPicks lists the server name and short ID of each Reality hop, in dial
// order. Servers with the same strong identity have the same Reality hops.
func realityPicks(s domain.Server) []realityPick {
	_, hops := s.Closure()
	var picks []realityPick
	for _, h := range hops {
		if r := h.TLS.Reality; r != nil {
			picks = append(picks, realityPick{h.TLS.ServerName, r.ShortID})
		}
	}
	return picks
}

// sameLineage compares everything a Change would store, which is all but the
// Reality absences, the config places and the naming. The digest in Ref covers the whole stored server closure.
func sameLineage(a, b domain.Lineage) bool {
	return a.Label == b.Label && a.Identity == b.Identity && a.Ref == b.Ref && a.Presence == b.Presence &&
		a.Country == b.Country && slices.EqualFunc(a.Seen, b.Seen, samePlace)
}

func samePlace(a, b domain.Occurrence) bool {
	return a.Config == b.Config && a.ConfigName == b.ConfigName && a.Tag == b.Tag && a.Position == b.Position
}

func sameHopPicks(a, b domain.RealityPicks) bool {
	return sameAbsence(a.ServerName, b.ServerName) && sameAbsence(a.ShortID, b.ShortID)
}

func sameAbsence(a, b domain.PickAbsence) bool { return a.Since.Equal(b.Since) && a.Misses == b.Misses }

type matcher struct {
	olds                       []domain.Lineage
	news                       []Entry
	oldTo, newTo               []int
	oldAmbiguous, newAmbiguous []bool
}

func newMatcher(olds []domain.Lineage, news []Entry) *matcher {
	m := &matcher{
		olds: olds, news: news,
		oldTo: make([]int, len(olds)), newTo: make([]int, len(news)),
		oldAmbiguous: make([]bool, len(olds)), newAmbiguous: make([]bool, len(news)),
	}
	for i := range m.oldTo {
		m.oldTo[i] = -1
	}
	for j := range m.newTo {
		m.newTo[j] = -1
	}
	return m
}

// run accepts every edge of one tier whose key is unique both ways among the
// entries still unmatched. Groups are disjoint, so the result does not depend on
// the order they are visited in.
func (m *matcher) run(t tier) {
	oldBy, newBy := map[string][]int{}, map[string][]int{}
	for i, l := range m.olds {
		if k, ok := t.key(l.Identity); ok && m.oldTo[i] < 0 {
			oldBy[k] = append(oldBy[k], i)
		}
	}
	for j, e := range m.news {
		if k, ok := t.key(e.Identity); ok && m.newTo[j] < 0 {
			newBy[k] = append(newBy[k], j)
		}
	}
	for k, os := range oldBy {
		ns := newBy[k]
		if len(ns) == 0 {
			continue
		}
		if len(os) == 1 && len(ns) == 1 || t.tie == nil {
			m.pair(os, ns)
			continue
		}
		oldTie, newTie := map[string][]int{}, map[string][]int{}
		for _, i := range os {
			oldTie[t.tie(m.olds[i].Identity)] = append(oldTie[t.tie(m.olds[i].Identity)], i)
		}
		for _, j := range ns {
			newTie[t.tie(m.news[j].Identity)] = append(newTie[t.tie(m.news[j].Identity)], j)
		}
		for tk, tos := range oldTie {
			if tns := newTie[tk]; len(tns) > 0 {
				m.pair(tos, tns)
			}
		}
		for _, i := range os {
			m.oldAmbiguous[i] = m.oldAmbiguous[i] || m.oldTo[i] < 0
		}
		for _, j := range ns {
			m.newAmbiguous[j] = m.newAmbiguous[j] || m.newTo[j] < 0
		}
	}
}

// pair links a one-to-one group and flags any larger group as ambiguous.
// Conflicting provider IDs veto a link: the provider says these are different
// servers, whatever else they share.
func (m *matcher) pair(os, ns []int) {
	if len(os) != 1 || len(ns) != 1 {
		for _, i := range os {
			m.oldAmbiguous[i] = true
		}
		for _, j := range ns {
			m.newAmbiguous[j] = true
		}
		return
	}
	i, j := os[0], ns[0]
	a, b := m.olds[i].Identity.ProviderID, m.news[j].Identity.ProviderID
	if a != "" && b != "" && a != b {
		return
	}
	m.oldTo[i], m.newTo[j] = j, i
}
