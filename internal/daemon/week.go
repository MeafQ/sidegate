package daemon

import (
	"context"
	"log"
	"slices"
	"time"

	"sidegate/internal/domain"
)

// The 7-day fields read the per-lineage record for results and incidents, and landings and switch events
// for what slots served and how often an owner moved.

const (
	weekLength = 7 * 24 * time.Hour
	currentFor = 10 * time.Minute
)

// movedOff are the reasons a forced move counts: its server failed, slowed badly, or lost UDP.
var movedOff = map[domain.Reason]bool{domain.FailureMove: true, domain.SlownessMove: true, domain.UDPMove: true}

type interval struct{ from, to time.Time }

// usage is what slots served, from their landings: which server each served, and when. Forced moves
// and incidents are the switch events'.
type usage struct {
	servers   map[domain.LineageID][]interval
	moves     map[domain.Owner][]time.Time
	incidents map[domain.LineageID][]time.Time
}

// usageOf reads landings in the order they were made, and applied events. A slot serves the server its
// newest landing took it to until its next landing or until now. A forced
// move counts once for its owner, when it applied, whatever slots it took.
func usageOf(events []domain.Switch, landings []landing, now time.Time) usage {
	out := usage{servers: map[domain.LineageID][]interval{}, moves: map[domain.Owner][]time.Time{},
		incidents: map[domain.LineageID][]time.Time{}}
	for _, e := range events {
		left, fromServer := e.From.Server()
		if !movedOff[e.Reason] || !fromServer {
			continue
		}
		out.moves[e.Owner] = append(out.moves[e.Owner], *e.AppliedAt)
		if e.Reason != domain.UDPMove {
			out.incidents[left.Lineage] = append(out.incidents[left.Lineage], *e.AppliedAt)
		}
	}
	type on struct {
		server domain.LineageID
		since  time.Time
	}
	at := map[domain.SlotID]on{}
	add := func(o on, end time.Time) {
		if end.After(o.since) {
			out.servers[o.server] = append(out.servers[o.server], interval{o.since, end})
		}
	}
	for _, l := range landings {
		if was, ok := at[l.Slot]; ok {
			add(was, l.At)
			delete(at, l.Slot)
		}
		if l.Move == nil {
			continue
		}
		if next, ok := l.Move.To.Server(); ok {
			at[l.Slot] = on{server: next.Lineage, since: l.At}
		}
	}
	for _, was := range at {
		add(was, now)
	}
	return out
}

// usageSince reads what slots served from since until now.
func usageSince(ctx context.Context, db querier, since, now time.Time) (usage, error) {
	events, err := switchesSince(ctx, db, since)
	if err != nil {
		return usage{}, err
	}
	landings, err := landingsSince(ctx, db, since)
	if err != nil {
		return usage{}, err
	}
	return usageOf(events, landings, now), nil
}

// firstFailed is when evidence first saw server fail, or fallback.
func firstFailed(evidence []domain.EvidenceSummary, server domain.LineageID, fallback time.Time) time.Time {
	first := fallback
	for _, ev := range evidence {
		if ev.Server.Lineage == server && ev.Verdict == domain.Failed && ev.At.Before(first) {
			first = ev.At
		}
	}
	return first
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// union merges intervals clipped to window into disjoint ones, in order.
func union(intervals []interval, window interval) []interval {
	var clipped []interval
	for _, i := range intervals {
		from, to := latest(i.from, window.from), i.to
		if to.After(window.to) {
			to = window.to
		}
		if to.After(from) {
			clipped = append(clipped, interval{from, to})
		}
	}
	slices.SortFunc(clipped, func(a, b interval) int { return a.from.Compare(b.from) })
	var out []interval
	for _, i := range clipped {
		if n := len(out); n > 0 && !i.from.After(out[n-1].to) {
			out[n-1].to = latest(out[n-1].to, i.to)
			continue
		}
		out = append(out, i)
	}
	return out
}

// moves7d counts an owner's forced moves in the week. An event moves an owner once, whatever slots it took.
func (s usage) moves7d(o domain.Owner, now time.Time) int {
	n := 0
	for _, at := range s.moves[o] {
		if !at.Before(now.Add(-weekLength)) {
			n++
		}
	}
	return n
}

// steps is how long the union of these intervals covers each step of window, by the step's start.
func steps(intervals []interval, window interval, step time.Duration) map[int64]time.Duration {
	out := map[int64]time.Duration{}
	stepMs := step.Milliseconds()
	for _, i := range union(intervals, window) {
		for from := i.from; from.Before(i.to); {
			start := from.UnixMilli() / stepMs * stepMs
			end := time.UnixMilli(start + stepMs)
			if end.After(i.to) {
				end = i.to
			}
			out[start] += end.Sub(from)
			from = end
		}
	}
	return out
}

// incidentSteps counts the failure and slowness moves off a server inside window, by the step's start.
func (s usage) incidentSteps(id domain.LineageID, window interval, step time.Duration) map[int64]int {
	out := map[int64]int{}
	for _, at := range s.incidents[id] {
		if !at.Before(window.from) && at.Before(window.to) {
			out[at.UnixMilli()/step.Milliseconds()*step.Milliseconds()]++
		}
	}
	return out
}

var standingVerdicts = map[standing]domain.Verdict{unprobed: domain.Unknown, passing: domain.Succeeded, recovering: domain.Succeeded, failing: domain.Failed}

// healthOf is a server's health block from its record and the samples to the ranking site whatever
// their age.
func healthOf(r record, at site, now time.Time) HealthDTO {
	pings, newConns, pingAt := samplesTo(r, at)
	out := HealthDTO{PingAt: pingAt, Verdict: verdictNames[standingVerdicts[r.standing()]], Current: !r.Last.IsZero() && now.Sub(r.Last) <= currentFor,
		PingP50Ms: p50(pings), PingP95Ms: p95(pings), NewConnectionP50Ms: p50(newConns), NewConnectionP95Ms: p95(newConns),
		Factor: factor(r, now), Incidents7d: incidents7d(r, now), SeenDays: r.seenDays(now)}
	if _, known := pingOf(r, at); known {
		out.Score = ptrTo(score(r, at, now))
	}
	if !r.Last.IsZero() {
		out.CheckedAt = ptrTo(r.Last)
	}
	if worked, known := worked7d(r, now); known {
		out.SeenWorking7dMs = ptrTo(worked.Milliseconds())
	}
	if n := len(r.Incidents); n > 0 {
		out.LastFailureAt = ptrTo(r.Incidents[n-1])
	}
	if !r.FirstSeen.IsZero() {
		out.FirstSeenAt = ptrTo(r.FirstSeen)
	}
	return out
}

func ptrTo[T any](v T) *T { return &v }

func p50(ds []time.Duration) *float64 {
	if len(ds) == 0 {
		return nil
	}
	m := domain.Median(ds)
	return millis(&m)
}

func p95(ds []time.Duration) *float64 {
	if len(ds) == 0 {
		return nil
	}
	m := domain.P95(ds)
	return millis(&m)
}

const servedEvery = time.Minute

// facts reads the week again when a switch has landed since, or it is a minute old. slots is the same
// view's slots, so the owner names its preference exactly while a box of it draws that server applied.
func (c *controller) facts(o domain.Owner, exit domain.Exit, w world, now time.Time, slots []SlotView) ownerFacts {
	if c.servedAt.IsZero() || now.Sub(c.servedAt) >= servedEvery {
		c.readServed(now)
	}
	e := evaluation{now: now, who: o, pool: w.Pool(exit), memory: c.state.owners[o], ev: c.evidence, t: c.timings}
	f := ownerFacts{Moves7d: c.served.moves7d(o, now), Order: []domain.LineageID{}}
	for _, l := range e.ranked(e.pool.members) {
		f.Order = append(f.Order, l.ID)
	}
	if len(f.Order) > 0 {
		f.FirstChoice = &f.Order[0]
	}
	if p := e.memory.Hand.boosted(now); p != nil && c.shownOn(o, p.Server, slots) {
		f.Preferred = &PreferredDTO{Server: p.Server, EndsAt: p.endsAt()}
	}
	return f
}

// shownOn says a slot whose Exit is o is given in slots as applied on server. A preference stands from
// its press's commit, before any slot moves, and only the boxes say the owner is on it: what the gate,
// the core or a read says reaches the owner through them alone.
func (c *controller) shownOn(o domain.Owner, server domain.LineageID, slots []SlotView) bool {
	return slices.ContainsFunc(slots, func(v SlotView) bool {
		row, known := c.state.slots[v.ID]
		owner, live := ownerOf(row)
		on, applied := v.appliedOn()
		return known && live && owner == o && applied && on == server
	})
}

func (c *controller) readServed(now time.Time) {
	served, err := usageSince(context.Background(), c.store.writer, now.Add(-weekLength), now)
	if err != nil {
		log.Printf("what slots served this week: %v", err)
		return
	}
	c.served, c.servedAt = served, now
}
