package daemon

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"slices"
	"time"

	"sidegate/internal/domain"
)

// The record's numbers are starting values the lab proves, and stay constants like the margins.
const (
	incidentPrior   = 0.25 // The unknown price: incidents charged before any history, over daysPrior.
	daysPrior       = 1.0
	recordHalfLife  = 7 * 24 * time.Hour
	creditCap       = 30 * time.Minute
	recoverPasses   = 3
	recoverSpacing  = 30 * time.Second
	recoverFresh    = 2 * time.Minute // A recovery pass after a longer gap counts as the first, so recovery needs fresh evidence.
	keepOutFor      = 5 * time.Minute
	jitterSamples   = 3
	attributeWithin = 2 * time.Minute // A failure outside a round is the server's when another server passed this recently.

	// 32 samples hold the 11 the introduction takes and about 16 hours of comparison rounds after it. The
	// nearest-rank p95 of 32 is the second highest, so one wild sample can't set it, and the 3-sample
	// jitter rule needs only 3. A full probe makes one, heartbeats none, so an hour's flush writes few.
	maxSamples = 32
)

// site is a probe target by what it asks, so a reordered target list can't change what a sample measured.
type site string

func siteOf(t probeTarget) site {
	sum := sha256.Sum256([]byte(t.URL))
	return site(hex.EncodeToString(sum[:8]))
}

type sample struct {
	At      time.Time
	Site    site
	Ping    time.Duration
	NewConn time.Duration
}

// episode is one outage of one server. End is zero while it lasts, and it ends when the server
// recovers, so failures before that are the same outage.
type episode struct{ Start, End time.Time }

type decayed struct {
	Value float64
	At    time.Time
}

func (d decayed) at(now time.Time) decayed {
	if d.At.IsZero() {
		return decayed{At: now}
	}
	return decayed{Value: d.Value * decay(now.Sub(d.At)), At: now}
}

// record is what the daemon keeps of one server lineage across restarts and refreshes. Groups share it.
type record struct {
	FirstSeen   time.Time
	Samples     []sample    // Oldest first.
	Incidents   []time.Time // Oldest first: each outage's start, and each severe-slowness move.
	Outages     []episode   // Oldest first, and only the newest can be open.
	Past        decayed     // The incident weight of incidents folded out of Incidents.
	Last        time.Time   // The last result's stamp, zero before any.
	Failures    int         // Failed results in a row up to Last.
	Retries     int         // Failed steps of its retry ladder. A pass clears them and a new failure starts them over, except an unverified server's, which stays unverified until its recovery ends.
	SteppedAt   time.Time   // When it last moved on the ladder: the failure that put it there, then each failed check after it.
	Blamed      int         // Those another server's pass makes the server's own.
	BlamedSince time.Time   // The first of them.
	Streak      int         // Spaced passes since the last failure, while short of recoverPasses.
	StreakAt    time.Time
	Seen        decayed       // Days seen working.
	Revision    domain.Digest // The ref the last result was of, so a credential or port change shows.
	Checked     time.Time     // The newest result of a probe that asks for a Ping, which a scheduled step waits for.
	Worked      []interval    // Oldest first and apart: the time it was seen working, back to a week before the newest.
	WorkedFrom  time.Time     // Since when Worked holds all of it. A record written before Worked was kept has none until its next result.
}

type standing uint8

const (
	unprobed standing = iota
	passing
	recovering
	failing
)

func (r record) standing() standing {
	switch {
	case r.Last.IsZero():
		return unprobed
	case r.Failures > 0:
		return failing
	case r.Streak > 0:
		return recovering
	}
	return passing
}

// result is one adjudicated probe of a server, newer than the record's last. Blamed says another server
// passed beside this failure.
type result struct {
	At     time.Time
	Passed bool
	Blamed bool
}

// measured keeps what one probe measured, in whatever order it lands: its sample and, for a probe that
// asks for a Ping, that the step it was asked for has run. Only its verdict waits for being the newest
// result. A press hands its Test's signal to the record itself as well as in the signal loop's frame,
// so a sample already held, by its probe's start and site, is the same one heard again.
func (r record) measured(at time.Time, s *sample, checked bool) record {
	if s != nil && !slices.ContainsFunc(r.Samples, func(have sample) bool { return have.At.Equal(s.At) && have.Site == s.Site }) {
		i := len(r.Samples)
		for i > 0 && r.Samples[i-1].At.After(s.At) {
			i--
		}
		r.Samples = newest(slices.Insert(slices.Clone(r.Samples), i, *s), maxSamples)
	}
	if checked && at.After(r.Checked) {
		r.Checked = at
	}
	return r
}

// stepped takes a ladder step for a failed check of the ref the record knows, in whatever order it
// lands: one that ran after the last step of a server failing is the next step. A passing check is no
// step, since it starts the recovery.
func (r record) stepped(at time.Time) record {
	if r.standing() == failing && at.After(r.SteppedAt) {
		r.Retries, r.SteppedAt = r.Retries+1, at
	}
	return r
}

// credited adds how long a pass saw the server working, which the caller bounds by its previous pass
// and by when the daemon or the WAN was down, in whatever order the pass lands. Worked takes it only
// when no failure came between the two passes, since the server wasn't working all of that time.
func (r record) credited(at time.Time, credit time.Duration) record {
	credit = min(credit, creditCap)
	if at.After(r.Seen.At) {
		r.Seen = r.Seen.at(at)
	}
	r.Seen.Value += credit.Hours() / 24
	if credit > 0 && r.standing() != failing {
		end := at
		if n := len(r.Worked); n > 0 {
			end = latest(end, r.Worked[n-1].to)
		}
		r.Worked = union(append(slices.Clone(r.Worked), interval{at.Add(-credit), at}), interval{end.Add(-weekLength), end})
	}
	return r
}

// heard folds in one result. changed says the server's lifecycle moved, which is worth a write now:
// its standing, its count of failures or passes, or its outage opening or closing. A failure nobody is
// blamed for neither counts toward the two in a row an incident takes nor resets them. The failure that
// puts a server on its ladder isn't a step of it, so every server gets all six, each counted from the
// step before it.
func (r record) heard(x result) (next record, changed bool) {
	before, out := r.standing(), r.outage()
	failures, streak := r.Failures, r.Streak
	r.Last = x.At
	if !x.Passed {
		if before != failing {
			r.restartLadder()
			r.SteppedAt = x.At
		}
		r.Failures++
		if x.Blamed {
			if r.Blamed == 0 {
				r.BlamedSince = x.At
			}
			r.Blamed++
		}
		r.Streak, r.StreakAt = 0, time.Time{}
		return r, true
	}
	switch {
	case before == failing:
		r.restartLadder()
		r.Failures, r.SteppedAt, r.Blamed, r.BlamedSince = 0, time.Time{}, 0, time.Time{}
		r.Streak, r.StreakAt = 1, x.At
	case before == recovering && x.At.Sub(r.StreakAt) > recoverFresh:
		r.Streak, r.StreakAt = 1, x.At
	case before == recovering && x.At.Sub(r.StreakAt) >= recoverSpacing:
		r.Streak, r.StreakAt = r.Streak+1, x.At
	}
	if r.Streak >= recoverPasses {
		r.Streak, r.StreakAt, r.Retries = 0, time.Time{}, 0
	}
	if r.standing() == passing {
		r.Outages = r.ended(x.At)
	}
	return r, before != r.standing() || failures != r.Failures || streak != r.Streak || out != r.outage()
}

// restartLadder starts the retry ladder over as the server starts or stops failing, except an
// unverified server's, which stays unverified until its recovery ends.
func (r *record) restartLadder() {
	if !unverified(*r) {
		r.Retries = 0
	}
}

func (r record) outage() bool {
	n := len(r.Outages)
	return n > 0 && r.Outages[n-1].End.IsZero()
}

// charged opens an outage from start, and its incident, unless it is the outage already: one is open,
// or the newest ended at or after start, since failures before a recovery are its outage. So a restart
// or a second group on the same outage charges nothing more. A record that stands passing has newer
// passes than every failure it heard, and its verdict follows them, so that outage is over: it ends at
// the newest result. An outage a week over by start is dropped, and it can't be the open one.
func (r record) charged(start time.Time) (record, bool) {
	if n := len(r.Outages); n > 0 && (r.outage() || !start.After(r.Outages[n-1].End)) {
		return r, false
	}
	e := episode{Start: start}
	if r.standing() == passing {
		e.End = latest(start, r.Last)
	}
	r = r.incurred(start)
	r.Outages = append(slices.DeleteFunc(slices.Clone(r.Outages), func(o episode) bool { return start.Sub(o.End) >= weekLength }), e)
	return r, true
}

// slowed charges a severe-slowness move: one incident, and no outage. Groups on one slow server move
// moments apart, and that is one incident.
func (r record) slowed(at time.Time) (record, bool) {
	if n := len(r.Incidents); r.outage() || n > 0 && at.Sub(r.Incidents[n-1]) < keepOutFor {
		return r, false
	}
	return r.incurred(at), true
}

// incurred adds an incident at at, in order, after folding into Past every incident a week older than
// the newest. Past decays at the same rate, so the factor stays exact. The week is the bound: an outage
// takes two failures and three passes 30 s apart, so a week holds at most about 10,000 outages and
// their incidents, some 450 KB encoded.
func (r record) incurred(at time.Time) record {
	i := len(r.Incidents)
	for i > 0 && r.Incidents[i-1].After(at) {
		i--
	}
	incidents := slices.Insert(slices.Clone(r.Incidents), i, at)
	newest := incidents[len(incidents)-1]
	fold := 0
	for fold < len(incidents) && newest.Sub(incidents[fold]) >= weekLength {
		fold++
	}
	if fold > 0 {
		r.Past = r.Past.at(newest)
		for _, old := range incidents[:fold] {
			r.Past.Value += decay(newest.Sub(old))
		}
	}
	r.Incidents = incidents[fold:]
	return r
}

func (r record) ended(at time.Time) []episode {
	if !r.outage() {
		return r.Outages
	}
	out := slices.Clone(r.Outages)
	out[len(out)-1].End = at
	return out
}

func newest[T any](items []T, limit int) []T {
	return items[max(len(items)-limit, 0):]
}

func decay(age time.Duration) float64 {
	return math.Exp2(-max(age, 0).Hours() / recordHalfLife.Hours())
}

func (r record) seenDays(now time.Time) float64 { return r.Seen.at(now).Value }

func (r record) incidentWeight(now time.Time) float64 {
	total := r.Past.at(now).Value
	for _, at := range r.Incidents {
		total += decay(now.Sub(at))
	}
	return total
}

func factor(r record, now time.Time) float64 {
	return 1 + (r.incidentWeight(now)+incidentPrior)/(r.seenDays(now)+daysPrior)
}

// worked7d is how long the server was seen working in the week before now, or false when the record
// hasn't kept Worked for all of that week since the server was first seen.
func worked7d(r record, now time.Time) (time.Duration, bool) {
	week := interval{now.Add(-weekLength), now}
	if r.WorkedFrom.IsZero() || r.WorkedFrom.After(latest(r.FirstSeen, week.from)) {
		return 0, false
	}
	total := time.Duration(0)
	for _, i := range union(r.Worked, week) {
		total += i.to.Sub(i.from)
	}
	return total, true
}

func incidents7d(r record, now time.Time) int {
	n := 0
	for _, at := range r.Incidents {
		if now.Sub(at) < weekLength {
			n++
		}
	}
	return n
}

func pingOf(r record, at site) (time.Duration, bool) {
	var pings []time.Duration
	for _, s := range r.Samples {
		if s.Site == at {
			pings = append(pings, s.Ping)
		}
	}
	if len(pings) == 0 {
		return 0, false
	}
	jitter := time.Duration(0)
	if len(pings) >= jitterSamples {
		jitter = domain.MedianDeviation(pings)
	}
	return domain.Median(pings) + jitter, true
}

// score is ping plus jitter times the factor, in milliseconds, on a site the server has samples to.
func score(r record, at site, now time.Time) float64 {
	ping, _ := pingOf(r, at)
	return float64(ping.Microseconds()) / 1000 * factor(r, now)
}

// price is what orders a server: its score on the site, or, with no samples there, its score on the
// last site it has samples to. Its factor already carries the price of what it hasn't shown, so a site
// change can't put a slower server ahead of a faster one with the same history. A server with no
// samples at all has no price, and ranks after every server that has one.
func price(r record, at site, now time.Time) (float64, bool) {
	if _, ok := pingOf(r, at); ok {
		return score(r, at, now), true
	}
	if n := len(r.Samples); n > 0 {
		return score(r, r.Samples[n-1].Site, now), true
	}
	return 0, false
}

func samplesTo(r record, at site) (pings, newConns []time.Duration, newest *time.Time) {
	for _, s := range r.Samples {
		if s.Site == at {
			pings, newConns, newest = append(pings, s.Ping), append(newConns, s.NewConn), &s.At
		}
	}
	return pings, newConns, newest
}

func class(r record) int {
	switch r.standing() {
	case recovering:
		return 1
	case failing:
		return 2
	}
	return 0
}

// order ranks a group's candidates: passed or never probed, then recovering, then failed, each by
// price, and keep-out after all of them. Readiness orders here and excludes nobody. A preferred server's
// price is lowered by what is left of its boost, and it wins an exact tie, so the boost's start ties it
// for first. Class still comes first.
func order(candidates []domain.Lineage, records map[domain.LineageID]record, keepOut map[domain.LineageID]time.Time, preferred *preference, at site, now time.Time) []domain.Lineage {
	type ranked struct {
		l         domain.Lineage
		out       bool
		class     int
		unpriced  bool
		price     float64
		preferred bool
	}
	rows := make([]ranked, 0, len(candidates))
	for _, l := range candidates {
		r := records[l.ID]
		p, priced := price(r, at, now)
		mine := preferred != nil && preferred.Server == l.ID
		if mine {
			p -= preferred.left(now)
		}
		rows = append(rows, ranked{l, now.Before(keepOut[l.ID]), class(r), !priced, p, mine})
	}
	slices.SortStableFunc(rows, func(a, b ranked) int {
		return cmp.Or(compareBool(a.out, b.out), cmp.Compare(a.class, b.class), compareBool(a.unpriced, b.unpriced), cmp.Compare(a.price, b.price),
			compareBool(!a.preferred, !b.preferred), cmp.Compare(a.l.ID, b.l.ID))
	})
	out := make([]domain.Lineage, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.l)
	}
	return out
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}
