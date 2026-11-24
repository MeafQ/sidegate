package daemon

import (
	"slices"
	"time"

	"sidegate/internal/domain"
)

// Probe and comparison cadence constants (see docs/architecture.md).
const (
	heartbeatServing = 60 * time.Second // The serving server, flowing or idle: bytes moving don't prove a new connection would succeed.
	heartbeatSuspect = 10 * time.Second // The serving server while a suspicion is raised on it.
	heartbeatClosed  = 5 * time.Second  // Often enough that a flicker can't hide between samples.
	heartbeatSweep   = 20 * time.Second // What else a closed slot could use, as often as the workers allow.
	checkAgain       = time.Minute      // A check whose answer moved nothing, such as one the WAN left unknown, asks again after this.
	compareEvery     = 30 * time.Minute // A serving group of two members or more compares its server this often.
	compareRivals    = 2                // A comparison measures the current server and the next this many by score.
	compareWins      = 3                // Wins in a row that move a group. A move starts every streak over, and a preference's fading boost stands in for a hold.
	compareExpiry    = 30 * time.Second // A comparison round with no answer by then is void.
	lineGuard        = 20_000_000       // Bits a second through our outbounds, up or down, past which the line itself inflates RTT.
	udpRivalChecks   = 2                // The next servers whose UDP is checked once when the current server's fails, so the failure can be blamed.
	unverifiedAfter  = 6                // Failed steps of the retry ladder, through its 32 min, that make a server unverified.
)

// introduction is when a server the catalogue calls new is probed, after first sight. Its first three
// are the quick probes a change of ranking site gives the members it leaves short of samples.
var (
	introduction = []time.Duration{0, 2 * time.Minute, 10 * time.Minute, 6 * time.Hour, 12 * time.Hour, 18 * time.Hour,
		24 * time.Hour, 30 * time.Hour, 36 * time.Hour, 42 * time.Hour, 48 * time.Hour}
	quickProbes = introduction[:3]
	// retryLadder is when a failed server is probed again, after its last step. It never ends: past 32 min
	// it keeps doubling and levels off at the introduction's 6 h, its last step, which repeats.
	retryLadder = []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute,
		64 * time.Minute, 128 * time.Minute, 256 * time.Minute, 6 * time.Hour}
)

// schedule is every probe the plan wants now. A heartbeat is a period, so the signal loop keeps its
// clock. Everything else is derived again from durable state on each plan, so a restart asks for the
// same probes and a probe that has answered stops being asked for.
type schedule struct {
	Beats  map[domain.ServerRef]time.Duration // Heartbeat period of each server that gets one.
	Checks map[domain.ServerRef]time.Time     // When each server's next check is due. A zero time is due at once.
	Rounds []roundRequest                     // One per running campaign.
	// The candidates whose UDP a serving server's failed check asks about, each server's next by score.
	// Empty for a server Exit and a one-member group, whose failure stands alone.
	UDPRivals map[domain.ServerRef][]domain.ServerRef
}

func (s *schedule) check(ref domain.ServerRef, due time.Time) {
	if have, ok := s.Checks[ref]; !ok || due.Before(have) {
		s.Checks[ref] = due
	}
}

func (s *schedule) beat(ref domain.ServerRef, every time.Duration) {
	if have, ok := s.Beats[ref]; !ok || every < have {
		s.Beats[ref] = every
	}
}

// probeRow is one event of the plan's table and what it asks of one owner. An owner exists only while a
// slot follows it, so a group nobody uses asks for nothing.
type probeRow struct {
	event string
	ask   func(e *evaluation, s *schedule)
}

var probeRows = []probeRow{
	{"a server the catalogue calls new", introduce},
	{"a credential or port change on a matched lineage", revise},
	{"the ranking site changes", refill},
	{"a member failed a round or a heartbeat", retry},
	{"current server, serving, and while a suspicion is raised", serveBeat},
	{"group has no working server", search},
	{"a failure or comparison round", campaignRound},
	{"UDP on the current server, and on the next two once it fails", udp},
}

// planProbes is the one scheduler: what every used owner's rows ask for, now. It reads and changes
// nothing else, so planning twice plans the same.
func planProbes(owners []evaluation) schedule {
	s := schedule{Beats: map[domain.ServerRef]time.Duration{}, Checks: map[domain.ServerRef]time.Time{}, UDPRivals: map[domain.ServerRef][]domain.ServerRef{}}
	for i := range owners {
		for _, row := range probeRows {
			row.ask(&owners[i], &s)
		}
	}
	return s
}

func serveBeat(e *evaluation, s *schedule) {
	current, serving := e.memory.Selection.Server()
	if !serving {
		return
	}
	every := heartbeatServing
	switch {
	case e.looking():
		every = heartbeatClosed
	case e.suspicious():
		every = heartbeatSuspect
	}
	s.beat(current, every)
}

// suspicious says a failure campaign runs, or one ended moments ago, which is when a failure is likeliest.
func (e *evaluation) suspicious() bool {
	return e.memory.Campaign.on(failurePath) || e.now.Before(e.memory.QuietUntil)
}

func search(e *evaluation, s *schedule) {
	if !e.looking() {
		return
	}
	for _, ref := range e.near() {
		s.beat(ref, heartbeatClosed)
	}
	for _, ref := range e.swept() {
		s.beat(ref, heartbeatSweep)
	}
}

func campaignRound(e *evaluation, s *schedule) {
	r := e.memory.Campaign
	if r == nil {
		return
	}
	s.Rounds = append(s.Rounds, roundRequest{Campaign: r.ID, Round: r.Round, Path: r.Path, Due: r.Due, Incumbent: r.Incumbent, Rivals: r.Rivals, Passive: r.unproven(), Target: r.Target})
}

// udp names who a serving server's failed UDP check is weighed against: the next contenders by score
// that aren't failing. The current server's own check runs every UDPEvery in the signal loop.
func udp(e *evaluation, s *schedule) {
	current, serving := e.memory.Selection.Server()
	if !serving {
		return
	}
	rivals := s.UDPRivals[current]
	if e.compares() {
		for _, l := range e.ranked(e.others([]domain.ServerRef{current})) {
			if len(rivals) == udpRivalChecks {
				break
			}
			if r := e.ev.book.records[l.ID]; r.standing() != failing && contender(r) && !slices.Contains(rivals, l.Ref) {
				rivals = append(rivals, l.Ref)
			}
		}
	}
	s.UDPRivals[current] = rivals
}

// retry is the ladder of a member that failed: a retry at each step after the one before, for as long
// as it fails, and once one passes, the spaced passes of its recovery. Each step counts from the record,
// so a restart owes only the next one. A group searching for a server already probes every member more
// often than this.
func retry(e *evaluation, s *schedule) {
	if e.looking() {
		return
	}
	for _, l := range e.pool.members {
		r := e.ev.book.records[l.ID]
		switch r.standing() {
		case failing:
			if due, ok := retryDue(r); ok {
				s.check(l.Ref, due)
			}
		case recovering:
			s.check(l.Ref, r.StreakAt.Add(recoverSpacing))
		}
	}
}

// members is what a used owner checks: its members, each with its record, but a failing one,
// which the retry ladder owns.
func (e *evaluation) members(yield func(domain.Lineage, record) bool) {
	for _, l := range e.pool.members {
		if r := e.ev.book.records[l.ID]; r.standing() != failing && !yield(l, r) {
			return
		}
	}
}

func introduce(e *evaluation, s *schedule) {
	for l, r := range e.members {
		if o, due := e.owes(r); o == owesIntroduction {
			s.check(l.Ref, due)
		}
	}
}

// revise checks a lineage once its ref changes, which a new credential or port does, failing or not.
// The record stays.
func revise(e *evaluation, s *schedule) {
	for _, l := range e.pool.members {
		if r := e.ev.book.records[l.ID]; r.Revision != (domain.Digest{}) && r.Revision != l.Ref.Digest {
			s.check(l.Ref, time.Time{})
		}
	}
}

func refill(e *evaluation, s *schedule) {
	for l, r := range e.members {
		if o, due := e.owes(r); o == owesRefill {
			s.check(l.Ref, due)
		}
	}
}

// owed is what a member's record still owes the ranking site.
type owed uint8

const (
	owesNothing      owed = iota
	owesIntroduction      // The checks a server the catalogue calls new gets, from first sight.
	owesRefill            // The quick probes a member seen before the ranking site ranked gets, short of samples there.
)

// owes is what r owes now and when its next probe is due. A refill comes first: it is the one that
// holds the pool on the site before.
func (e *evaluation) owes(r record) (owed, time.Time) {
	if due, ok := refillDue(r, e.ev.book.sites[e.ev.ranking], e.ev.rankedSince, e.ev.previous != ""); ok {
		return owesRefill, due
	}
	if due, ok := introductionDue(r); ok {
		return owesIntroduction, due
	}
	return owesNothing, time.Time{}
}

// introductionDue is the next introduction probe, from first sight: a step is taken once a full probe's
// result is at or after it, so a restart resumes where the record says and owes no step it slept
// through. A server never heard from is due at once. False once every step is taken.
func introductionDue(r record) (time.Time, bool) {
	for _, offset := range introduction {
		if at := r.FirstSeen.Add(offset); r.Checked.IsZero() || r.Checked.Before(at) {
			return at, true
		}
	}
	return time.Time{}, false
}

// refillDue is the next quick probe of a member with fewer than three samples to the ranking site,
// counted from when this process first heard that site, and taken as an introduction's are. A member
// still in its introduction gets them only for a change of site this process saw, so a restart leaves
// the introduction where it was. A server first seen once the site ranked, or never, owes none: it
// had nothing on the site before, and its introduction measures the site there is.
func refillDue(r record, at site, since time.Time, changed bool) (time.Time, bool) {
	if pings, _, _ := samplesTo(r, at); since.IsZero() || len(pings) >= len(quickProbes) || r.FirstSeen.IsZero() || !r.FirstSeen.Before(since) {
		return time.Time{}, false
	}
	if _, introducing := introductionDue(r); introducing && !changed {
		return time.Time{}, false
	}
	for _, offset := range quickProbes {
		if due := since.Add(offset); r.Checked.Before(due) {
			return due, true
		}
	}
	return time.Time{}, false
}

// asksPing says a probe of this purpose makes the reused requests a Ping needs, which a liveness probe
// and a UDP check don't.
func asksPing(p domain.ProbePurpose) bool {
	return p != domain.Heartbeat && p != domain.Search && p != domain.UDPCheck
}

// orderSite is the site whose samples order the pool: the ranking site, but the one before it while
// the pool refills.
func (e *evaluation) orderSite() site {
	if e.refilling() {
		return e.ev.previous
	}
	return e.ev.book.sites[e.ev.ranking]
}

// refillOpen says the ranking site changed since the pool last finished a refill.
func (e *evaluation) refillOpen() bool {
	return e.ev.previous != "" && !e.ev.rankedSince.IsZero() && e.memory.RefilledAt.Before(e.ev.rankedSince)
}

// refilling says the pool's refill is open and a member still owes its quick probes, so it could
// otherwise rank on samples it hasn't got.
func (e *evaluation) refilling() bool {
	if !e.refillOpen() {
		return false
	}
	for _, r := range e.members {
		if o, _ := e.owes(r); o == owesRefill {
			return true
		}
	}
	return false
}

// finishRefill closes an open refill no member owes anything to, so a member the pool admits later,
// however old, can't take the pool back to the site before.
func (e *evaluation) finishRefill() {
	if e.refillOpen() && !e.refilling() {
		e.memory.RefilledAt = e.now
	}
}

// retryDue is when a failed server's next retry is, from its last step and the steps it has failed.
// False when it isn't failing.
func retryDue(r record) (time.Time, bool) {
	if r.standing() != failing {
		return time.Time{}, false
	}
	return r.SteppedAt.Add(retryLadder[min(r.Retries, len(retryLadder)-1)]), true
}

// unverified says the first six steps of its ladder failed. It ranks last until its recovery ends.
func unverified(r record) bool { return r.Retries >= unverifiedAfter }

// contender says a member may be a round's rival. An unverified or recovering one can't win a round,
// so no round measures it: its ladder and its recovery's spaced passes do.
func contender(r record) bool { return r.standing() != recovering && !unverified(r) }
