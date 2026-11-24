package daemon

import (
	"maps"
	"slices"
	"time"

	"sidegate/internal/domain"
)

const activeResults = 32

// ring keeps the newest limit values, oldest first.
type ring[T any] struct {
	items []T
	limit int
}

func (r *ring[T]) push(v T) {
	r.items = append(r.items, v)
	if len(r.items) > r.limit {
		r.items = slices.Delete(r.items, 0, len(r.items)-r.limit)
	}
}

// serverEvidence is what one server's results say. Everything in it describes the running core and is
// cleared when the core restarts.
type serverEvidence struct {
	active    ring[domain.Signal] // Every active measurement, heartbeats included, each once.
	rounds    ring[domain.Signal] // What each campaign's rounds heard, one copy per campaign sharing a measurement.
	suspicion domain.Signal       // The newest passive suspicion.
	udp       udpEvidence         // What its UDP checks say.
}

// evidence is what the controller knows from signal frames. Only ingest and the core's
// lifecycle change it, and decide only reads it.
type evidence struct {
	wan         wanBelief // The signal loop's, as of its last frame.
	epoch       domain.CoreEpoch
	heard       uint64          // Signals heard so far, which is what a campaign marks as read.
	ranking     domain.TargetID // The signal loop's ranking target, as of its last frame.
	previous    site            // The ranking site before the last change. Empty before any.
	rankedSince time.Time       // When the ranking target became this one, which a refill's quick probes count from.
	lineBusy    bool            // The line is too full for slowness to say anything about a server.
	servers     map[domain.ServerRef]*serverEvidence
	book        *book
}

func newEvidence(ranking domain.TargetID, sites map[domain.TargetID]site) *evidence {
	return &evidence{ranking: ranking, servers: map[domain.ServerRef]*serverEvidence{}, book: newBook(sites)}
}

func (ev *evidence) server(ref domain.ServerRef) *serverEvidence {
	s, ok := ev.servers[ref]
	if !ok {
		s = &serverEvidence{active: ring[domain.Signal]{limit: activeResults}, rounds: ring[domain.Signal]{limit: activeResults}}
		ev.servers[ref] = s
	}
	return s
}

func (ev *evidence) ingest(f signalFrame) {
	ev.wan, ev.lineBusy = f.WAN, f.LineBusy
	var measured []domain.Signal
	for _, s := range f.Signals {
		if !ev.admits(s) {
			continue
		}
		ev.heard++
		s.Heard = ev.heard
		if s.Purpose == domain.UDPCheck {
			ev.server(s.Server).udp.hear(s)
			continue
		}
		server := ev.server(s.Server)
		if s.Kind != domain.ActiveProbe {
			server.suspicion = s
			continue
		}
		if s.Purpose == domain.ManualProbe {
			measured = append(measured, s)
			continue
		}
		if s.Campaign != "" {
			server.rounds.push(s)
			if server.lastIsMeasurement(s) {
				continue
			}
		}
		server.active.push(s)
		measured = append(measured, s)
	}
	ev.book.hear(measured, ev.wan.LastDown)
}

// admits says a signal is of the running core, or names none. A core that restarted since it began
// withdrew its route, so it says nothing about the server.
func (ev *evidence) admits(s domain.Signal) bool {
	return s.Stamp.Epoch == "" || s.Stamp.Epoch == ev.epoch
}

// withRecord is the evidence with id's record replaced, for reading what a result would make of it
// before it is heard.
func (ev *evidence) withRecord(id domain.LineageID, r record) *evidence {
	b := *ev.book
	b.records = maps.Clone(b.records)
	b.records[id] = r
	out := *ev
	out.book = &b
	return &out
}

// rankBy follows the signal loop's ranking target, and says whether that changed what state.db keeps.
// A frame that names none keeps the one there is.
func (ev *evidence) rankBy(target domain.TargetID, at time.Time) bool {
	switch {
	case ev.rankedSince.IsZero():
		ev.rankedSince = at
	case target != "" && target != ev.ranking:
		ev.previous, ev.rankedSince = ev.book.sites[ev.ranking], at
	default:
		return false
	}
	if target != "" {
		ev.ranking = target
	}
	return true
}

// rankedSite is the ranking target as state.db keeps it, by what it asks rather than by its place in
// the list, so a restart ranks where the last process did and still owes any refill it began.
type rankedSite struct {
	Site, Previous site
	Since          time.Time
}

func (ev *evidence) rankedSite() rankedSite {
	return rankedSite{Site: ev.book.sites[ev.ranking], Previous: ev.previous, Since: ev.rankedSince}
}

// rankAs takes up what the last process ranked by. When no target asks its site any more, the targets
// changed, which is a change of ranking site like any other: the first target ranks, refilling from the
// first frame, and the old site's numbers order the pool until it has.
func (ev *evidence) rankAs(r rankedSite, targets []probeTarget) {
	for _, t := range targets {
		if siteOf(t) == r.Site {
			ev.ranking, ev.previous, ev.rankedSince = t.ID, r.Previous, r.Since
			return
		}
	}
	ev.previous = r.Site
}

// lastIsMeasurement says s is another campaign's copy of the newest measurement. Campaigns sharing a
// round each hear it, and it still counts once toward whether a server has recovered.
func (s *serverEvidence) lastIsMeasurement(sig domain.Signal) bool {
	items := s.active.items
	if len(items) == 0 {
		return false
	}
	last := items[len(items)-1]
	return last.Stamp.At.Equal(sig.Stamp.At) && last.Purpose == sig.Purpose && last.Target == sig.Target
}

// coreRestarted forgets what only described the old core, which is all of it.
func (ev *evidence) coreRestarted(epoch domain.CoreEpoch) {
	ev.epoch = epoch
	clear(ev.servers)
}

// forget drops the servers that are no longer wanted.
func (ev *evidence) forget(gone func(domain.ServerRef) bool) {
	for ref := range ev.servers {
		if gone(ref) {
			delete(ev.servers, ref)
		}
	}
}

// results lists server's active results, oldest first, Unknown ones included.
func (ev *evidence) results(ref domain.ServerRef) []domain.Signal {
	if s, ok := ev.servers[ref]; ok {
		return s.active.items
	}
	return nil
}

// roundResults lists what campaigns' rounds heard of ref, oldest first.
func (ev *evidence) roundResults(ref domain.ServerRef) []domain.Signal {
	if s, ok := ev.servers[ref]; ok {
		return s.rounds.items
	}
	return nil
}

// udp reads what ref's UDP checks say. A server nobody has checked is unknown.
func (ev *evidence) udp(ref domain.ServerRef) udpEvidence {
	if s, ok := ev.servers[ref]; ok {
		return s.udp
	}
	return udpEvidence{}
}

func (ev *evidence) suspicion(ref domain.ServerRef) domain.Signal {
	if s, ok := ev.servers[ref]; ok {
		return s.suspicion
	}
	return domain.Signal{}
}
