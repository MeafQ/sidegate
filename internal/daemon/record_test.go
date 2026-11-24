package daemon

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

const here site = "here"

// observed is a record seen working every period from from to to, as heartbeats or comparisons see it.
func observed(r record, from, to time.Time, every time.Duration) record {
	for at := from.Add(every); !at.After(to); at = at.Add(every) {
		r, _ = r.credited(at, every).heard(result{At: at, Passed: true})
	}
	return r
}

func pinged(r record, at time.Time, ms int) record {
	r, _ = r.measured(at, &sample{At: at, Site: here, Ping: time.Duration(ms) * time.Millisecond}, true).heard(result{At: at, Passed: true})
	return r
}

func share(r record, now time.Time) int { return int(math.Round(factor(r, now) * 100)) }

func TestTheShareTableRows(t *testing.T) {
	week := 7 * 24 * time.Hour
	incident := func(r record, at time.Time) record {
		r, _ = r.charged(at)
		return r
	}
	for _, c := range []struct {
		name string
		r    record
		at   time.Time
		want int
	}{
		{"never used", record{}, t0, 125},
		{"one clean day under observation", observed(record{}, t0, t0.Add(24*time.Hour), time.Minute), t0.Add(24 * time.Hour), 113},
		{"one clean week", observed(record{}, t0, t0.Add(week), 10*time.Minute), t0.Add(week), 104},
		{"2 hours seen working, one incident", incident(observed(record{}, t0, t0.Add(2*time.Hour), time.Minute), t0.Add(2*time.Hour)), t0.Add(2 * time.Hour), 215},
		{"a clean week, then 4 weeks unobserved", observed(record{}, t0, t0.Add(week), 10*time.Minute), t0.Add(5 * week), 119},
	} {
		if got := share(c.r, c.at); got != c.want {
			t.Errorf("%s: ping counts as %d%%, want %d%%", c.name, got, c.want)
		}
	}
	floor := observed(record{}, t0, t0.Add(20*week), 30*time.Minute)
	if got := factor(floor, t0.Add(20*week)); math.Abs(got-1.0225) > 0.0005 {
		t.Errorf("weeks of clean observation count as %.4f, want the floor of about 1.0225", got)
	}
}

func TestAPassCreditsAtMostHalfAnHour(t *testing.T) {
	r, _ := record{}.credited(t0, 6*time.Hour).heard(result{At: t0, Passed: true})
	if got := r.seenDays(t0) * 24; math.Abs(got-0.5) > 1e-9 {
		t.Errorf("one pass after 6 hours credited %.3f hours, want 0.5", got)
	}
}

func TestANeverUsedServerAgainstALongRecordWithOneIncident(t *testing.T) {
	now := t0.Add(21 * 24 * time.Hour)
	long := observed(record{}, t0, now, 10*time.Minute)
	long, _ = long.charged(now.Add(-3 * 24 * time.Hour))
	long = pinged(long, now, 100)
	fresh := func(ms int) record {
		r := record{FirstSeen: now.Add(-10 * time.Minute)}
		for i := range 3 {
			r = pinged(r, now.Add(time.Duration(i-2)*4*time.Minute), ms)
		}
		return r
	}
	pool := []domain.Lineage{lin("long"), lin("new")}
	if got := order(pool, map[domain.LineageID]record{"long": long, "new": fresh(100)}, nil, nil, here, now); got[0].ID != "long" {
		t.Errorf("at the same ping the unknown server ranks first: %v", ids(got))
	}
	if got := order(pool, map[domain.LineageID]record{"long": long, "new": fresh(75)}, nil, nil, here, now); got[0].ID != "new" {
		t.Errorf("a quarter faster, the unknown server still ranks behind: %v", ids(got))
	}
}

func TestAFarAndANearServerWithTheSameHistoryKeepTheirPlaces(t *testing.T) {
	now := t0.Add(2 * time.Hour)
	history := observed(record{}, t0, now, time.Minute)
	history, _ = history.charged(now.Add(-time.Hour))
	records := map[domain.LineageID]record{"far": pinged(history, now, 250), "near": pinged(history, now, 40)}
	if got := order([]domain.Lineage{lin("far"), lin("near")}, records, nil, nil, here, now); got[0].ID != "near" {
		t.Errorf("the same history moved the far server ahead: %v", ids(got))
	}
	if f, n := score(records["far"], here, now), score(records["near"], here, now); math.Abs(f/n-250.0/40) > 1e-9 {
		t.Errorf("the same history cost the far server %.1f ms and the near one %.1f ms, not the same share", f, n)
	}
}

func TestAFailedServerRecoversAfterThreePassesThirtySecondsApart(t *testing.T) {
	r := pinged(record{}, t0, 40)
	r, _ = r.heard(result{At: t0.Add(time.Minute)})
	if r.standing() != failing {
		t.Fatalf("a failure left the server %d", r.standing())
	}
	steps := []struct {
		after time.Duration
		want  standing
	}{
		{2 * time.Minute, recovering},
		{2*time.Minute + 10*time.Second, recovering},
		{2*time.Minute + 30*time.Second, recovering},
		{2*time.Minute + 50*time.Second, recovering},
		{3 * time.Minute, passing},
	}
	for _, s := range steps {
		r, _ = r.heard(result{At: t0.Add(s.after), Passed: true})
		if r.standing() != s.want {
			t.Errorf("after a pass at %s the server is %d, want %d", s.after, r.standing(), s.want)
		}
	}
}

func TestARecoveryPassAfterALongGapStartsTheCountOver(t *testing.T) {
	r, _ := record{}.heard(result{At: t0})
	for _, after := range []time.Duration{time.Minute, 90 * time.Second, 10 * time.Minute, 10*time.Minute + 30*time.Second} {
		r, _ = r.heard(result{At: t0.Add(after), Passed: true})
	}
	if r.standing() != recovering {
		t.Errorf("passes on both sides of a 9-minute gap recovered the server")
	}
}

func TestTheOrderPutsRecoveringAndFailedServersBehindAndKeepOutLast(t *testing.T) {
	now := t0.Add(time.Hour)
	ok := pinged(record{}, now, 300)
	healing, _ := pinged(record{}, now.Add(-time.Minute), 10).heard(result{At: now.Add(-50 * time.Second)})
	healing, _ = healing.heard(result{At: now, Passed: true})
	dead, _ := pinged(record{}, now.Add(-time.Minute), 5).heard(result{At: now})
	left := pinged(record{}, now, 1)
	records := map[domain.LineageID]record{"ok": ok, "healing": healing, "dead": dead, "left": left}
	keepOut := map[domain.LineageID]time.Time{"left": now.Add(time.Minute)}
	got := order([]domain.Lineage{lin("dead"), lin("left"), lin("healing"), lin("ok"), lin("unprobed")}, records, keepOut, nil, here, now)
	if want := []domain.LineageID{"ok", "unprobed", "healing", "dead", "left"}; !slices.Equal(ids(got), want) {
		t.Errorf("order %v, want %v", ids(got), want)
	}
}

func TestFailuresBeforeARecoveryAreOneIncident(t *testing.T) {
	r, _ := record{}.heard(result{At: t0})
	r, _ = r.heard(result{At: t0.Add(time.Minute)})
	r, first := r.charged(t0)
	r, _ = r.heard(result{At: t0.Add(2 * time.Minute), Passed: true})
	r, _ = r.heard(result{At: t0.Add(3 * time.Minute)})
	r, second := r.charged(t0.Add(3 * time.Minute))
	if !first || second || incidents7d(r, t0.Add(time.Hour)) != 1 {
		t.Errorf("a failure before the server recovered made a second incident: %v", r.Incidents)
	}
	for _, after := range []time.Duration{4, 5, 6} {
		r, _ = r.heard(result{At: t0.Add(after * time.Minute), Passed: true})
	}
	if e := r.Outages[0]; e.End != t0.Add(6*time.Minute) {
		t.Errorf("the outage ended at %s, want when the server recovered", e.End)
	}
	r, _ = r.heard(result{At: t0.Add(7 * time.Minute)})
	if _, again := r.charged(t0.Add(7 * time.Minute)); !again {
		t.Errorf("a failure after a recovery made no new incident")
	}
}

// A second group's move on an outage that is over charges nothing more, and a slowness move since
// doesn't make it a new outage. A failure after the recovery is one.
func TestAChargeOnAnOutageAlreadyOverIsThatOutage(t *testing.T) {
	r, _ := record{}.heard(result{At: t0, Blamed: true})
	r, _ = r.heard(result{At: t0.Add(time.Second), Blamed: true})
	r, _ = r.charged(t0)
	for _, s := range []time.Duration{30, 60, 90} {
		r, _ = r.heard(result{At: t0.Add(s * time.Second), Passed: true})
	}
	r, _ = r.slowed(t0.Add(10 * time.Minute))
	if _, again := r.charged(t0); again {
		t.Errorf("a second move on the outage over at %s charged it again", r.Outages[0].End)
	}
	r, _ = r.heard(result{At: t0.Add(11 * time.Minute), Blamed: true})
	r, again := r.charged(t0.Add(11 * time.Minute))
	if !again || incidents7d(r, r.Last) != 3 || len(r.Outages) != 2 {
		t.Errorf("an outage, a slowness move and a later outage made incidents %v and outages %+v", r.Incidents, r.Outages)
	}
}

func TestPingIsTheMedianPlusJitterOnlyFromThreeSamples(t *testing.T) {
	two := pinged(pinged(record{}, t0, 40), t0.Add(time.Minute), 60)
	if got, _ := pingOf(two, here); got != 50*time.Millisecond {
		t.Errorf("two samples ping %s, want their median with no jitter", got)
	}
	three := pinged(two, t0.Add(2*time.Minute), 100)
	if got, _ := pingOf(three, here); got != 80*time.Millisecond {
		t.Errorf("three samples ping %s, want the median 60 ms plus 20 ms of jitter", got)
	}
	if got, known := pingOf(three, "elsewhere"); known || got != 0 {
		t.Errorf("another site's samples gave a ping of %s", got)
	}
}

func TestTheRecordBoundsItsSamples(t *testing.T) {
	r := record{}
	for i := range 2 * maxSamples {
		r = pinged(r, t0.Add(time.Duration(i)*time.Minute), i)
	}
	if len(r.Samples) != maxSamples || r.Samples[0].Ping != maxSamples*time.Millisecond {
		t.Errorf("kept %d samples from %s, want the newest %d", len(r.Samples), r.Samples[0].Ping, maxSamples)
	}
}

func TestTheRetryLadderAndTheIntroductionReadFromTheRecord(t *testing.T) {
	r, _ := record{}.stepped(t0).heard(result{At: t0})
	r, _ = r.stepped(t0.Add(time.Minute)).heard(result{At: t0.Add(time.Minute)})
	r, _ = r.heard(result{At: t0.Add(2 * time.Minute)})
	if due, ok := retryDue(r); !ok || !due.Equal(t0.Add(3*time.Minute)) {
		t.Errorf("after a failure, a failed retry and a failed round the next retry is at %s, want the second step after the retry", due)
	}
	var at time.Time
	for i := range unverifiedAfter - 1 {
		at = t0.Add(time.Duration(i+2) * time.Hour)
		r, _ = r.stepped(at).heard(result{At: at})
	}
	if due, ok := retryDue(r); !ok || !unverified(r) || !due.Equal(at.Add(64*time.Minute)) {
		t.Errorf("after its first six steps failed a server is unverified %v with its next retry at %s, want one 64 min after the last", unverified(r), due)
	}
	for range 20 {
		at = at.Add(6 * time.Hour)
		r, _ = r.stepped(at).heard(result{At: at})
	}
	if due, ok := retryDue(r); !ok || !due.Equal(at.Add(6*time.Hour)) {
		t.Errorf("a server failing for days is next retried at %s, want 6 h after its last step", due)
	}
	if due, ok := introductionDue(record{FirstSeen: t0, Checked: t0.Add(3 * time.Minute)}); !ok || !due.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("after probes at 0 and 3 minutes the introduction is next due at %s, want 10 minutes", due)
	}
}

func ids(ls []domain.Lineage) []domain.LineageID {
	out := make([]domain.LineageID, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.ID)
	}
	return out
}

// Every episode of the last week counts and shows as outage, however many came after it, and one
// older than that still weighs on the factor until it decays.
func TestEveryEpisodeOfTheWeekCountsAndOlderOnesStillWeigh(t *testing.T) {
	var r record
	var starts []time.Time
	outage := func(at time.Time, lasting time.Duration) {
		r, _ = r.charged(at)
		r, _ = r.heard(result{At: at.Add(lasting), Passed: true})
		starts = append(starts, at)
	}
	exact := func(now time.Time) float64 {
		weight := 0.0
		for _, at := range starts {
			weight += decay(now.Sub(at))
		}
		return 1 + (weight+incidentPrior)/daysPrior
	}
	for i := range 40 {
		outage(t0.Add(time.Duration(i)*time.Hour), 10*time.Minute)
	}
	now := t0.Add(40 * time.Hour)
	if got := incidents7d(r, now); got != 40 {
		t.Errorf("40 incidents in two days count as %d", got)
	}
	if got, want := factor(r, now), exact(now); math.Abs(got-want) > 1e-9 {
		t.Errorf("the factor is %.9f, want %.9f", got, want)
	}

	for i := range 512 {
		outage(t0.Add(9*24*time.Hour+time.Duration(i)*10*time.Minute), time.Minute)
	}
	now = starts[len(starts)-1].Add(time.Hour)
	if got, want := factor(r, now), exact(now); math.Abs(got-want) > 1e-9 {
		t.Errorf("a week on, the factor is %.9f, want %.9f", got, want)
	}
}

// Seen working is the time the checks found the server up in the week before now: not the time since
// it was first seen, and not the time it was checked.
func TestSeenWorkingIsTheWeeksTimeTheChecksFoundTheServerUp(t *testing.T) {
	day := 24 * time.Hour
	now := t0.Add(30 * day)
	listed := record{FirstSeen: t0, WorkedFrom: t0}
	worked := func(r record) time.Duration {
		t.Helper()
		got, known := worked7d(r, now)
		if !known {
			t.Fatal("a record kept since the server was first seen reads as unknown")
		}
		return got
	}

	month := observed(observed(listed, t0, t0.Add(2*day), time.Minute), now.Add(-12*time.Hour), now, time.Minute)
	if got := worked(month); got != 12*time.Hour {
		t.Errorf("on the list for a month and checked for half a day this week, seen working %s", got)
	}

	down := observed(listed, now.Add(-7*day), now.Add(-5*day), time.Minute)
	down, _ = down.heard(result{At: now.Add(-5*day + time.Minute)})
	down, _ = down.credited(now.Add(-time.Minute), 5*day).heard(result{At: now.Add(-time.Minute), Passed: true})
	if got := worked(down); got != 2*day {
		t.Errorf("down for five days of the week, seen working %s, want 2 days", got)
	}

	if got := worked(observed(listed, now.Add(-8*day), now.Add(-6*day), time.Minute)); got != day {
		t.Errorf("two days seen working across the week's start count %s, want the day inside it", got)
	}

	fresh := record{FirstSeen: now.Add(-time.Hour), WorkedFrom: now.Add(-time.Hour)}
	if got := worked(observed(fresh, now.Add(-time.Hour), now, time.Minute)); got != time.Hour {
		t.Errorf("a server first seen an hour ago and up since, seen working %s", got)
	}
}

func TestTheRecordKeepsAWeekOfTimeSeenWorking(t *testing.T) {
	r := record{FirstSeen: t0, WorkedFrom: t0}
	for at := t0; at.Before(t0.Add(21 * 24 * time.Hour)); at = at.Add(6 * time.Hour) {
		r = r.credited(at, time.Hour)
	}
	newest := r.Worked[len(r.Worked)-1].to
	if len(r.Worked) != 28 || r.Worked[0].from.Before(newest.Add(-weekLength)) {
		t.Errorf("three weeks of half-hour passes 6 hours apart kept %d intervals from %s, want a week's 28 back to %s", len(r.Worked), r.Worked[0].from, newest.Add(-weekLength))
	}
	if got, _ := worked7d(r, newest); got != 28*creditCap {
		t.Errorf("each pass counts at most %s: the week holds %s", creditCap, got)
	}
}

// A record written before Worked was kept starts keeping it at its next result, and reads as unknown
// until it has kept a whole week.
func TestAnOlderRecordReadsTimeSeenWorkingAsUnknownForAWeek(t *testing.T) {
	older, err := decodeRecord([]byte(fmt.Sprintf(`{"first":%d,"last":%d,"seen":5,"seenAt":%d}`, t0.UnixMilli(), t0.UnixMilli(), t0.UnixMilli())))
	if err != nil {
		t.Fatal(err)
	}
	now := t0.Add(30 * 24 * time.Hour)
	if _, known := worked7d(older, now); known {
		t.Error("an older record reads a time seen working")
	}
	b := newBook(nil)
	b.records["a"] = older
	b.hear([]domain.Signal{{Stamp: domain.Stamp{At: now}, Server: domain.ServerRef{Lineage: "a"}, Verdict: domain.Succeeded}}, time.Time{})
	kept := b.records["a"]
	if !kept.WorkedFrom.Equal(now) {
		t.Fatalf("the first result after the upgrade keeps Worked from %s, want %s", kept.WorkedFrom, now)
	}
	if _, known := worked7d(kept, now.Add(6*24*time.Hour)); known {
		t.Error("six days after the upgrade the week reads as known")
	}
	if _, known := worked7d(kept, now.Add(7*24*time.Hour)); !known {
		t.Error("a week after the upgrade the week still reads as unknown")
	}

	body, err := encodeRecord(observed(kept, now, now.Add(time.Hour), time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeRecord(body)
	if err != nil {
		t.Fatal(err)
	}
	if !back.WorkedFrom.Equal(now) || !slices.Equal(back.Worked, []interval{{now, now.Add(time.Hour)}}) {
		t.Errorf("the record stored Worked %v from %s", back.Worked, back.WorkedFrom)
	}
}
