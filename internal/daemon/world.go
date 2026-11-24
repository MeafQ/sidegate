package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"sidegate/internal/domain"
)

// world is everything selection reads that isn't per-slot. It is immutable, and setWorld is
// the only way to change it. So a reader can't see a stale pool, and no code decides what to
// invalidate.
type world struct {
	catalogue domain.Catalogue // Lineages and their indexes.
	groups    map[domain.GroupID]domain.Group
	refused   map[domain.ServerRef]bool
	uses      []exitUse
	members   map[domain.GroupID][]domain.Lineage
	routes    map[domain.ServerRef]domain.Lineage // What gets a probe route: every present member of an Exit in use.
	pools     map[domain.GroupID]pool
	sources   map[domain.SourceID]pool
}

type exitUse struct {
	Slot domain.SlotID
	Exit domain.Exit
}

// pool is what one Exit allows now. Every member has a probe route, so a slot can move to any of them.
type pool struct {
	fixed     bool              // A server Exit, Direct or Block, which has no moves.
	off       domain.Selection  // Direct or Blocked, all an Exit off the VPN decides.
	members   []domain.Lineage  // Present, not refused, and permitted by the Exit, in ID order.
	empty     domain.DownReason // Why a slot closes when the pool holds nothing for it.
	catalogue domain.Catalogue
	refused   map[domain.ServerRef]bool
}

func (p pool) member(id domain.LineageID) (domain.Lineage, bool) {
	if i := slices.IndexFunc(p.members, func(l domain.Lineage) bool { return l.ID == id }); i >= 0 {
		return p.members[i], true
	}
	return domain.Lineage{}, false
}

// leaving reads what a slot on a server this pool doesn't allow is leaving, and why. Only a server
// that still exists and runs can be stayed on, which is when the reason is ExitChanged.
func (p pool) leaving(id domain.LineageID) (domain.Lineage, domain.Reason) {
	l, ok := p.catalogue.Lookup(id)
	switch {
	case !ok || l.Presence != domain.Present:
		return l, domain.ServerLeftFeed
	case p.refused[l.Ref]:
		return l, domain.RefusedByCore
	}
	return l, domain.ExitChanged
}

// downReason is why a slot this pool can't serve closes. A server that exists but isn't ready yet
// isn't missing.
func (p pool) downReason() domain.DownReason {
	if len(p.members) > 0 {
		return domain.NoUsableServer
	}
	return p.empty
}

// worldChange names what differs. A nil field keeps what the world holds.
type worldChange struct {
	lineages []domain.Lineage
	gone     []domain.LineageID       // Deleted servers collected.
	revision domain.CatalogueRevision // Zero keeps the catalogue's.
	groups   []domain.Group
	removed  []domain.GroupID
	refused  map[domain.ServerRef]bool
	uses     []exitUse
}

// newWorld derives members, the probe routes and the pools of the Exits in use. It compiles nothing,
// since a Filter compiled at admission. A route is every present member of an Exit in use, a refused one
// too, because the core judges only what it is asked to run.
func newWorld(catalogue domain.Catalogue, groups map[domain.GroupID]domain.Group, refused map[domain.ServerRef]bool, uses []exitUse) world {
	w := world{catalogue: catalogue, groups: groups, refused: refused, uses: uses,
		members: make(map[domain.GroupID][]domain.Lineage, len(groups)),
		routes:  map[domain.ServerRef]domain.Lineage{}, pools: map[domain.GroupID]pool{}, sources: map[domain.SourceID]pool{}}
	for id, g := range groups {
		w.members[id] = catalogue.Members(g.Members, g.Ticked)
	}
	route := func(members []domain.Lineage) {
		for _, l := range members {
			if l.Presence == domain.Present {
				w.routes[l.Ref] = l
			}
		}
	}
	for _, use := range uses {
		if id, ok := use.Exit.Server(); ok {
			if l, ok := catalogue.Lookup(id); ok {
				route([]domain.Lineage{l})
			}
		}
		if id, ok := use.Exit.Group(); ok {
			if _, done := w.pools[id]; !done {
				w.pools[id] = w.poolOf(w.members[id])
				route(w.members[id])
			}
		}
		if id, ok := use.Exit.Source(); ok {
			if _, done := w.sources[id]; !done {
				w.sources[id] = w.poolOf(w.sourceMembers(id))
				route(w.sourceMembers(id))
			}
		}
	}
	return w
}

// sourceMembers is what a source Exit allows: every present server of the source, in ID order.
func (w world) sourceMembers(id domain.SourceID) []domain.Lineage {
	var out []domain.Lineage
	for _, l := range w.catalogue.Source(id) {
		if l.Presence == domain.Present {
			out = append(out, l)
		}
	}
	return out
}

// with is the world as it would be after change. Nothing about it is current until setWorld.
func (w world) with(change worldChange) world {
	catalogue := w.catalogue
	if len(change.lineages) > 0 || change.revision != 0 {
		revision := change.revision
		if revision == 0 {
			revision = catalogue.Revision()
		}
		catalogue = catalogue.With(revision, change.lineages)
	}
	if len(change.gone) > 0 {
		catalogue = catalogue.Without(change.gone)
	}
	groups := w.groups
	if len(change.groups) > 0 || len(change.removed) > 0 {
		groups = maps.Clone(groups)
		for _, g := range change.groups {
			groups[g.ID] = g
		}
		for _, id := range change.removed {
			delete(groups, id)
		}
	}
	refused := w.refused
	if change.refused != nil {
		refused = change.refused
	}
	uses := w.uses
	if change.uses != nil {
		uses = change.uses
	}
	return newWorld(catalogue, groups, refused, uses)
}

// Pool answers what an Exit may use now.
func (w world) Pool(e domain.Exit) pool {
	if off, ok := e.OffVPN(); ok {
		return pool{fixed: true, off: off, catalogue: w.catalogue, refused: w.refused}
	}
	if id, ok := e.Server(); ok {
		return w.serverPool(id)
	}
	if id, ok := e.Source(); ok {
		if p, ok := w.sources[id]; ok {
			return p
		}
		return w.poolOf(w.sourceMembers(id))
	}
	id, _ := e.Group()
	if p, ok := w.pools[id]; ok {
		return p
	}
	return w.poolOf(w.members[id])
}

// serverPool holds the one server the slot asked for, while it is present and not refused.
func (w world) serverPool(id domain.LineageID) pool {
	p := pool{fixed: true, empty: domain.ServerMissing, catalogue: w.catalogue, refused: w.refused}
	l, ok := w.catalogue.Lookup(id)
	switch {
	case !ok || l.Presence != domain.Present:
	case w.refused[l.Ref]:
		p.empty = domain.ServerRefused
	default:
		p.members = []domain.Lineage{l}
	}
	return p
}

func (w world) poolOf(members []domain.Lineage) pool {
	p := pool{empty: domain.NoUsableServer, catalogue: w.catalogue, refused: w.refused}
	for _, l := range members {
		if !w.refused[l.Ref] {
			p.members = append(p.members, l)
		}
	}
	return p
}

// supported says the core hasn't refused a lineage's current revision. It leaves Presence alone,
// because Presence says what the feed did.
func (w world) supported(id domain.LineageID) bool {
	l, ok := w.catalogue.Lookup(id)
	return !ok || !w.refused[l.Ref]
}

// slotsReferencing answers one question for groups and sources alike: which slots would lose
// their Exit if these groups, sources and servers went.
func (w world) slotsReferencing(groups map[domain.GroupID]bool, sources map[domain.SourceID]bool, servers map[domain.LineageID]bool) []domain.SlotID {
	var out []domain.SlotID
	for _, use := range w.uses {
		server, isServer := use.Exit.Server()
		group, isGroup := use.Exit.Group()
		source, isSource := use.Exit.Source()
		if (isServer && servers[server]) || (isGroup && groups[group]) || (isSource && sources[source]) {
			out = append(out, use.Slot)
		}
	}
	slices.Sort(out)
	return out
}

// serverKey changes when what a ServerView shows from memory changes.
func (w world) serverKey() string {
	var b strings.Builder
	for _, ref := range slices.SortedFunc(maps.Keys(w.refused), compareRefs) {
		fmt.Fprintf(&b, "r %s.%x\n", ref.Lineage, ref.Digest[:6])
	}
	return b.String()
}

// usedBy lists the slots whose Exit is this group.
func (w world) usedBy(id domain.GroupID) []domain.SlotID {
	return w.slotsReferencing(map[domain.GroupID]bool{id: true}, nil, nil)
}

// sourceDeletion is what deleting source id does: the groups it removes, the groups it rewrites, and
// the slots it would be refused for, those whose Exit is the source, one of its servers, or a group
// it removes.
type sourceDeletion struct {
	removed   map[domain.GroupID]bool
	rewritten []domain.Group
	usedBy    []domain.SlotID
}

func (w world) sourceDeletion(id domain.SourceID) sourceDeletion {
	servers := map[domain.LineageID]bool{}
	for _, l := range w.catalogue.Source(id) {
		servers[l.ID] = true
	}
	d := sourceDeletion{removed: map[domain.GroupID]bool{}}
	for _, gid := range slices.Sorted(maps.Keys(w.groups)) {
		next, changed, gone := w.groups[gid].WithoutSource(id, w.catalogue)
		switch {
		case gone:
			d.removed[gid] = true
		case changed:
			next.Revision++
			d.rewritten = append(d.rewritten, next)
		}
	}
	d.usedBy = w.slotsReferencing(d.removed, map[domain.SourceID]bool{id: true}, servers)
	return d
}

// usesOf reads each live slot's Exit. rows may hold a row a command is about to store.
func usesOf(rows map[domain.SlotID]slotRow) []exitUse {
	uses := make([]exitUse, 0, len(rows))
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		if row := rows[id]; !row.Deleting {
			uses = append(uses, exitUse{Slot: id, Exit: row.Slot.Intent.Exit()})
		}
	}
	return uses
}

// setWorld makes next current. Every path that changes what selection reads ends here.
func (c *controller) setWorld(next world) {
	c.world = next
	c.evidence.forget(func(ref domain.ServerRef) bool {
		l, ok := next.catalogue.Lookup(ref.Lineage)
		return !ok || l.Ref != ref
	})
}

// stage is one command on its way to a commit. Nothing it holds is current until then.
type stage struct {
	now     time.Time
	tx      transaction
	receipt Receipt
	next    world
	apply   []func()
	undo    []func()
}

// then adds what happens once the commit has landed.
func (s *stage) then(apply func()) { s.apply = append(s.apply, apply) }

// otherwise adds what happens if the commit doesn't land.
func (s *stage) otherwise(undo func()) { s.undo = append(s.undo, undo) }

func (s *stage) abandon() {
	for _, undo := range s.undo {
		undo()
	}
}

func (s *stage) finish() {
	for _, apply := range s.apply {
		apply()
	}
}
