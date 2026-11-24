package daemon

import "sidegate/internal/domain"

// closesFlows says what each reason does to a slot's open connections. A move forced on the slot or
// asked for by the owner ends them, so apps reconnect on the path it left for. An optional move or a
// new credential for the same server lets them drain. Lab step closeflows proves each kind of row.
var closesFlows = map[domain.Reason]bool{
	domain.ExitChanged:    true,
	domain.FailureMove:    true,
	domain.SlownessMove:   true,
	domain.UDPMove:        true,
	domain.ServerLeftFeed: true,
	domain.RefusedByCore:  true,
	domain.PreferredMove:  true,

	domain.ImprovementMove:  false,
	domain.InitialSelection: false,
	domain.Recovery:         false,
	domain.RevisionChanged:  false,
}

// switchCloses adds the one case a reason can't state: a closure leaves nowhere for a flow to go,
// whichever reason took the slot there.
func switchCloses(s domain.Switch) bool {
	_, down := s.To.Down()
	return down || closesFlows[s.Reason]
}

// closeOwed is the operation whose flows the slot must still end, or "": its pending switch's when that
// switch ends flows, or else the one the row owes, until the runtime reports it attempted. The plan names
// it, and the Worker attempts each once in its life.
func closeOwed(row slotRow, pending *domain.Switch) domain.EventID {
	owed := row.Owes
	if pending != nil && switchCloses(*pending) {
		owed = pending.ID
	}
	if owed == row.Closed {
		return ""
	}
	return owed
}

// switched is the row as switch e replaces pending. A switch that ends flows covers any close owed before
// it, and one that ends none keeps what is still owed, so a move that supersedes one owing a close still
// pays it.
func (r slotRow) switched(pending *domain.Switch, e domain.Switch) slotRow {
	owed := closeOwed(r, pending)
	r.Owes = ""
	if !switchCloses(e) {
		r.Owes = owed
	}
	return r
}

// acknowledged is the row once a report names flowsClosed as the last close its Worker attempted. Only
// the close owed is news: a fresh Worker names none, and its first report must not undo an attempt the
// row already holds.
func (r slotRow) acknowledged(pending *domain.Switch, flowsClosed domain.EventID) slotRow {
	if owed := closeOwed(r, pending); owed != "" && owed == flowsClosed {
		r.Closed = owed
	}
	return r
}

// landed is the row as pending lands: a close not yet reported attempted stays owed.
func (r slotRow) landed(pending domain.Switch) slotRow {
	r.Owes = closeOwed(r, &pending)
	return r
}

// pendingOf is the switch the slot's readback hasn't shown yet, or nil.
func (s *durableState) pendingOf(slot domain.SlotID) *domain.Switch {
	if p, ok := s.pending[slot]; ok {
		return &p
	}
	return nil
}
