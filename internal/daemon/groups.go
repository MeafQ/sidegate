package daemon

import (
	"slices"
	"strings"

	"sidegate/internal/domain"
)

func (cmd PutGroup) perform(c *controller, s *stage) error {
	current, exists := c.world.groups[cmd.ID]
	switch {
	case exists && cmd.Expected == 0:
		return stale(uint64(current.Revision))
	case !exists && cmd.Expected != 0:
		return refuse(KindNotFound, RefusedGroupGone, "That group is gone.")
	case exists && domain.IntentRevision(current.Revision) != cmd.Expected:
		return stale(uint64(current.Revision))
	}
	if exists && len(cmd.Ticked) > 0 {
		return refuse(KindValidation, RefusedMalformed, "A group's ticks change one at a time once it exists.")
	}
	if strings.TrimSpace(cmd.Name) == "" {
		return refuse(KindValidation, RefusedNoName, "A group needs a name.")
	}
	if !exists {
		if err := validateID(string(cmd.ID), "group"); err != nil {
			return err
		}
	}
	members, err := c.admitMembers(cmd.Members)
	if err != nil {
		return err
	}
	g := domain.Group{ID: cmd.ID, Revision: current.Revision + 1, Name: cmd.Name, Members: members, Ticked: current.Ticked}
	if !exists {
		g.Revision = domain.GroupRevision(c.state.retired[retiredGroup(cmd.ID)]) + 1
		ticked, err := domain.AdmitTicked(cmd.Ticked)
		if err != nil {
			return refuse(KindValidation, RefusedMalformed, sentence(err))
		}
		if err := c.admitTicks(ticked); err != nil {
			return err
		}
		g.Ticked = ticked
		if g.Bare() {
			return refuse(KindValidation, RefusedNoRule, "A new group needs a filter, or Box is ticked with a server ticked.")
		}
	}
	s.tx.Groups = append(s.tx.Groups, g)
	s.next = s.next.with(worldChange{groups: []domain.Group{g}})
	return nil
}

// admitMembers runs the rule a source's delete runs, for an editor opened before that delete. A
// condition dropping one of several sources is silent. One left with none would widen to any source,
// so the owner chooses.
func (c *controller) admitMembers(m domain.Members) (domain.Members, error) {
	g := domain.Group{Members: m}
	var gone []domain.SourceID
	for _, id := range m.Sources.Values {
		if _, known := c.state.sources[id]; !known {
			gone = append(gone, id)
		}
	}
	for _, id := range gone {
		next, _, removed := g.WithoutSource(id, c.world.catalogue)
		if removed {
			return domain.Members{}, refuse(KindValidation, RefusedSourcesGone, "Every source this group named is gone. Choose a source.")
		}
		g = next
	}
	return g.Members, nil
}

// admitTicks lets a create tick only servers this gateway knows and still has, as a tick does.
func (c *controller) admitTicks(ids []domain.LineageID) error {
	for _, id := range ids {
		if l, known := c.world.catalogue.Lookup(id); !known || l.Presence == domain.Tombstone {
			return refuse(KindValidation, RefusedServerGone, "That server is gone.")
		}
	}
	return nil
}

// perform saves one tick at once, beside any rule edit the owner has open, so it keeps the group's
// revision: the rule's Save never meets a tick as a conflict. A server ticked must be one this gateway
// knows and still has. Unticking any is allowed.
func (cmd TickServer) perform(c *controller, s *stage) error {
	g, exists := c.world.groups[cmd.Group]
	if !exists {
		return refuse(KindNotFound, RefusedGroupGone, "That group is gone.")
	}
	at, on := slices.BinarySearch(g.Ticked, cmd.Server)
	if cmd.Ticked == on {
		return nil
	}
	ticked := slices.Clone(g.Ticked)
	if cmd.Ticked {
		if err := c.admitTicks([]domain.LineageID{cmd.Server}); err != nil {
			return err
		}
		ticked = slices.Insert(ticked, at, cmd.Server)
	} else {
		ticked = slices.Delete(ticked, at, at+1)
	}
	g.Ticked = ticked
	s.tx.Groups = append(s.tx.Groups, g)
	s.next = s.next.with(worldChange{groups: []domain.Group{g}})
	return nil
}

func (cmd DeleteGroup) perform(c *controller, s *stage) error {
	current, exists := c.world.groups[cmd.ID]
	switch {
	case !exists:
		return refuse(KindNotFound, RefusedGroupGone, "That group is gone.")
	case cmd.Expected != 0 && domain.IntentRevision(current.Revision) != cmd.Expected:
		return stale(uint64(current.Revision))
	}
	if used := c.world.usedBy(cmd.ID); len(used) > 0 {
		return c.refuseReferenced("This group", used)
	}
	s.tx.DeletedGroups = []domain.GroupID{cmd.ID}
	key := retiredGroup(cmd.ID)
	s.tx.Retired = map[string]uint64{key: uint64(current.Revision)}
	s.next = s.next.with(worldChange{removed: []domain.GroupID{cmd.ID}})
	s.then(func() { c.state.retired[key] = uint64(current.Revision) })
	return nil
}
