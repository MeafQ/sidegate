package daemon

import (
	"maps"
	"time"

	"sidegate/internal/domain"
)

// recordFlushEvery bounds what a crash loses of samples and seen-working time.
const recordFlushEvery = time.Hour

// book is every lineage's record and what feeding them needs. Records outlive the core, unlike the rest
// of evidence. passed is this process's own, so time the daemon was down earns no credit.
type book struct {
	records map[domain.LineageID]record
	passed  map[domain.LineageID]time.Time
	sites   map[domain.TargetID]site
	unsaved map[domain.LineageID]bool // Not yet written, true where the change is one a crash mustn't lose.
}

func newBook(sites map[domain.TargetID]site) *book {
	return &book{records: map[domain.LineageID]record{}, passed: map[domain.LineageID]time.Time{}, sites: sites, unsaved: map[domain.LineageID]bool{}}
}

func sitesOf(targets []probeTarget) map[domain.TargetID]site {
	out := map[domain.TargetID]site{}
	for _, t := range targets {
		out[t.ID] = siteOf(t)
	}
	return out
}

func (b *book) put(id domain.LineageID, r record, urgent bool) {
	b.records[id] = r
	b.unsaved[id] = b.unsaved[id] || urgent
}

// hear folds one frame's measurements in, each once. A verdict counts only if newer than the server's
// last result, so a probe that completes late can't stand for a newer one, but what it measured counts
// whenever it lands: its day, its sample, the ladder step a failed check spent, and the time it saw the
// server working. A failure is the server's when another server passed: in a round, another
// participant of it; otherwise any server, recently. So a round in which everything failed charges
// nobody.
func (b *book) hear(signals []domain.Signal, wanDownAt time.Time) {
	passers := map[domain.LineageID]bool{}
	for _, s := range signals {
		if s.Verdict == domain.Succeeded {
			passers[s.Server.Lineage] = true
		}
	}
	for _, s := range signals {
		id, at := s.Server.Lineage, s.Stamp.At
		r := b.records[id]
		if s.Verdict == domain.Unknown {
			continue
		}
		if r.FirstSeen.IsZero() {
			r.FirstSeen = at
		}
		if r.WorkedFrom.IsZero() {
			r.WorkedFrom = at
		}
		var measured *sample
		if s.Verdict == domain.Succeeded && s.Ping != nil && s.NewConn != nil {
			measured = &sample{At: at, Site: b.sites[s.Target], Ping: *s.Ping, NewConn: *s.NewConn}
		}
		next := r.measured(at, measured, asksPing(s.Purpose))
		// A check's result is where its schedule stands, so a restart owes none of it again.
		urgent := s.Purpose == domain.Check
		passed := s.Verdict == domain.Succeeded
		if revised := r.Revision != (domain.Digest{}) && r.Revision != s.Server.Digest; s.Purpose == domain.Check && !revised && !passed {
			next = next.stepped(at)
		}
		if passed {
			next = next.credited(at, b.credit(id, at, wanDownAt))
		}
		if at.After(r.Last) {
			x := result{At: at, Passed: passed}
			if !passed {
				x.Blamed = b.attributed(s, passers)
			}
			var changed bool
			next, changed = next.heard(x)
			next.Revision = s.Server.Digest
			urgent = urgent || changed
			if x.Blamed && next.Blamed >= 2 {
				var opened bool
				next, opened = next.charged(next.BlamedSince)
				urgent = urgent || opened
			}
		}
		b.put(id, next, urgent)
	}
}

// heardBy is what id's record would be once it heard s, with nothing written to the book.
func (b *book) heardBy(s domain.Signal, wanDownAt time.Time) record {
	id := s.Server.Lineage
	scratch := &book{records: map[domain.LineageID]record{id: b.records[id]}, passed: maps.Clone(b.passed), sites: b.sites, unsaved: map[domain.LineageID]bool{}}
	scratch.hear([]domain.Signal{s}, wanDownAt)
	return scratch.records[id]
}

// credit is how long id was seen working up to a pass at at: since its previous pass, unless the WAN
// was seen down after it, in which case the next interval starts from this one. A pass older than the
// previous one adds nothing that one didn't.
func (b *book) credit(id domain.LineageID, at, wanDownAt time.Time) time.Duration {
	previous, ok := b.passed[id]
	if ok && !at.After(previous) {
		return 0
	}
	b.passed[id] = at
	if ok && previous.After(wanDownAt) {
		return at.Sub(previous)
	}
	return 0
}

func (b *book) attributed(s domain.Signal, passers map[domain.LineageID]bool) bool {
	for id := range passers {
		if id != s.Server.Lineage {
			return true
		}
	}
	if s.Campaign != "" {
		return false
	}
	for id, at := range b.passed {
		if id != s.Server.Lineage && s.Stamp.At.Sub(at) <= attributeWithin {
			return true
		}
	}
	return false
}

func (b *book) charge(id domain.LineageID, start time.Time) {
	if r, opened := b.records[id].charged(start); opened {
		b.put(id, r, true)
	}
}

func (b *book) slow(id domain.LineageID, at time.Time) {
	if r, opened := b.records[id].slowed(at); opened {
		b.put(id, r, true)
	}
}

// due is what to write now: what a crash mustn't lose, and once an hour everything that changed.
func (b *book) due(flush bool) map[domain.LineageID]record {
	var out map[domain.LineageID]record
	for id, urgent := range b.unsaved {
		if urgent || flush {
			if out == nil {
				out = map[domain.LineageID]record{}
			}
			out[id] = b.records[id]
		}
	}
	return out
}

func (b *book) written(records map[domain.LineageID]record) {
	for id := range records {
		delete(b.unsaved, id)
	}
}

// snapshot is the records as they stand. A record is never changed in place, so this shares them.
func (b *book) snapshot() map[domain.LineageID]record { return maps.Clone(b.records) }

func (b *book) forget(ids []domain.LineageID) {
	for _, id := range ids {
		delete(b.records, id)
		delete(b.passed, id)
		delete(b.unsaved, id)
	}
}
