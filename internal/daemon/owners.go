package daemon

import (
	"cmp"
	"maps"
	"slices"

	"sidegate/internal/domain"
)

// ownerOf names who decides for a slot: the group or source its Exit names, or else the slot itself.
// A slot being deleted follows nobody.
func ownerOf(row slotRow) (domain.Owner, bool) {
	if row.Deleting {
		return domain.Owner{}, false
	}
	exit := row.Slot.Intent.Exit()
	if id, ok := exit.Group(); ok {
		return domain.GroupOwner(id), true
	}
	if id, ok := exit.Source(); ok {
		return domain.SourceOwner(id), true
	}
	return domain.SlotOwner(row.Slot.ID), true
}

type following struct {
	owner domain.Owner
	slots []domain.SlotID
	exit  domain.Exit
}

// followings lists every owner a slot follows, in key order. An owner nothing follows isn't here, so
// it neither decides nor campaigns.
func followings(rows map[domain.SlotID]slotRow) []following {
	byOwner := map[domain.Owner]*following{}
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		row := rows[id]
		o, live := ownerOf(row)
		if !live {
			continue
		}
		f, ok := byOwner[o]
		if !ok {
			f = &following{owner: o, exit: row.Slot.Intent.Exit()}
			byOwner[o] = f
		}
		f.slots = append(f.slots, id)
	}
	out := make([]following, 0, len(byOwner))
	for _, f := range byOwner {
		out = append(out, *f)
	}
	slices.SortFunc(out, func(a, b following) int { return cmp.Compare(a.owner.String(), b.owner.String()) })
	return out
}

func (c *controller) memoryOf(row slotRow) policyMemory {
	o, _ := ownerOf(row)
	return c.state.owners[o]
}

// rehome keeps owners in step with one slot's next row. The owner the slot now follows starts from it
// when nothing else follows that owner yet: with the whole memory of the owner it left, when it was
// that owner's last slot, or else with its selection and any deadline it was leaving by or move it
// hasn't shown, so an edit never renews the one or drops the other. The owner it left goes with its last slot.
func (c *controller) rehome(s *stage, before slotRow, existed bool, after slotRow) {
	from, wasOwned := ownerOf(before)
	wasOwned = wasOwned && existed
	to, owned := ownerOf(after)
	left := c.state.owners[from]
	exitChanged := before.Slot.Intent.Exit() != after.Slot.Intent.Exit()
	if wasOwned && owned && from == to {
		if exitChanged {
			c.stageOwner(s, to, left.choiceChanged(s.now))
		}
		return
	}
	rows := maps.Clone(c.state.slots)
	rows[after.Slot.ID] = after
	last := wasOwned && !slices.ContainsFunc(followings(rows), func(f following) bool { return f.owner == from })
	if _, used := c.state.owners[to]; owned && !used && last {
		c.stageOwner(s, to, left.choiceChanged(s.now))
	} else if owned && !used {
		c.stageOwner(s, to, c.newOwner(after, left))
	}
	if last {
		if s.tx.Owners != nil {
			delete(s.tx.Owners, from)
		}
		s.tx.DeletedOwners = append(s.tx.DeletedOwners, from)
		s.then(func() { delete(c.state.owners, from) })
	}
}

// newOwner is an owner's memory as its first slot finds it, but for a deletion's closure, which is no
// choice of any owner's. A slot on the selection of the owner it left takes that owner's LastServed
// too, since a slot that lags behind a move another slot showed still reads the server before it.
// Each life of an owner gets its own range of campaign IDs, because a round's results outlive the
// memory that asked for them.
func (c *controller) newOwner(row slotRow, left policyMemory) policyMemory {
	c.lives++
	m := policyMemory{LastServed: row.LastServed, Campaigns: c.lives << 16}
	if reason, down := row.Selection.Down(); !down || reason != domain.Deleting {
		m.Selection = row.Selection
	}
	if left.Selection == row.Selection {
		m.LeaveBy, m.Unshown, m.LastServed = left.LeaveBy, left.Unshown, left.LastServed
	}
	return m
}

func (c *controller) stageOwner(s *stage, o domain.Owner, m policyMemory) {
	if s.tx.Owners == nil {
		s.tx.Owners = map[domain.Owner]policyMemory{}
	}
	s.tx.Owners[o] = m
	s.then(func() { c.state.owners[o] = m })
}

func joinReason(from domain.Selection) domain.Reason {
	if reason, down := from.Down(); down && reason == domain.Unselected {
		return domain.InitialSelection
	}
	return domain.ExitChanged
}
