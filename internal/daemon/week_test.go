package daemon

import (
	"math"
	"testing"
	"time"

	"sidegate/internal/domain"
	"sidegate/internal/metrics"
)

func ptr[T any](v T) *T { return &v }

// shown lands every slot each applied event names, when it applied, in the order given.
func shown(events ...domain.Switch) []landing {
	var out []landing
	for i := range events {
		for _, slot := range events[i].Slots {
			out = append(out, landing{Slot: slot, Move: &events[i], At: *events[i].AppliedAt})
		}
	}
	return out
}

func lastWeek(now time.Time) interval { return interval{now.Add(-weekLength), now} }

// measure is how long the union of these intervals covers inside window.
func measure(intervals []interval, window interval) time.Duration {
	var total time.Duration
	for _, i := range union(intervals, window) {
		total += i.to.Sub(i.from)
	}
	return total
}

func applied(owner domain.Owner, slots []domain.SlotID, from, to domain.Selection, reason domain.Reason, at time.Time, evidence ...domain.EvidenceSummary) domain.Switch {
	return domain.Switch{Owner: owner, Slots: slots, From: from, To: to, Reason: reason, AppliedAt: &at, RequestedAt: at, Evidence: evidence}
}

// Two slots on one group serve one server, so the time counts once. A failure move counts once for the
// group, and is an incident of the server it left.
func TestTheWeekOfAGroup(t *testing.T) {
	g, both := domain.GroupOwner("g"), []domain.SlotID{"one", "two"}
	a, b := domain.Serving(ref("a")), domain.Serving(ref("b"))
	events := []domain.Switch{
		applied(g, both, domain.Closed(domain.Unselected), a, domain.InitialSelection, t0),
		applied(g, both, a, b, domain.FailureMove, t0.Add(time.Hour)),
	}
	now := t0.Add(2 * time.Hour)
	u := usageOf(events, shown(events...), now)
	for _, id := range []domain.LineageID{"a", "b"} {
		if got := measure(u.servers[id], lastWeek(now)); got != time.Hour {
			t.Errorf("two slots on %s for an hour served %v", id, got)
		}
	}
	if got := u.moves7d(g, now); got != 1 {
		t.Errorf("one group move of two slots counted %d times", got)
	}
	if _, used := u.servers["c"]; used {
		t.Error("a server nobody used served")
	}
	if got := u.incidentSteps("a", interval{t0, now}, time.Hour); got[t0.Add(time.Hour).UnixMilli()] != 1 {
		t.Errorf("incidents %v", got)
	}
	if moves := usageOf(events, shown(events...), t0.Add(9*24*time.Hour)).moves7d(g, t0.Add(9*24*time.Hour)); moves != 0 {
		t.Errorf("a move 9 days back counted %d", moves)
	}
}

// A slot serves a server until its next landing, or until the runtime removed it.
func TestASpanEndsWhenItsSlotMovesOrGoes(t *testing.T) {
	g := domain.GroupOwner("g")
	a, b := domain.Serving(ref("a")), domain.Serving(ref("b"))
	events := []domain.Switch{
		applied(g, []domain.SlotID{"one"}, domain.Closed(domain.Unselected), a, domain.InitialSelection, t0),
		applied(g, []domain.SlotID{"one"}, a, b, domain.FailureMove, t0.Add(time.Hour)),
		applied(g, []domain.SlotID{"gone"}, domain.Closed(domain.Unselected), a, domain.InitialSelection, t0.Add(90*time.Minute)),
	}
	now := t0.Add(3 * time.Hour)
	u := usageOf(events, append(shown(events...), landing{Slot: "gone", At: t0.Add(150 * time.Minute)}), now)
	if got := measure(u.servers["a"], lastWeek(now)); got != 2*time.Hour {
		t.Errorf("a served %v, want the hour before one left and the hour gone served", got)
	}
	if got := measure(u.servers["b"], lastWeek(now)); got != 2*time.Hour {
		t.Errorf("b served %v, want from the move until now", got)
	}
}

func TestAServersHealthFromItsRecord(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	r := record{}
	for i, ping := range []int{10, 20, 30, 40} {
		at := t0.Add(-time.Duration(3-i) * 48 * time.Hour)
		r, _ = r.measured(at, &sample{At: at, Site: here, Ping: ms(ping), NewConn: ms(90)}, true).heard(result{At: at, Passed: true})
	}
	r, _ = r.measured(t0, &sample{At: t0, Site: "elsewhere", Ping: ms(500), NewConn: ms(900)}, true).heard(result{At: t0, Passed: true})
	got := healthOf(r, here, t0.Add(5*time.Minute))
	if got.Verdict != "succeeded" || !got.Current || *got.PingP50Ms != 25 || *got.PingP95Ms != 40 || *got.NewConnectionP50Ms != 90 {
		t.Errorf("health %+v, want the samples to the ranking site whatever their age", got)
	}
	if got.Score == nil || got.Incidents7d != 0 || math.Abs(*got.Score-(25+10)*got.Factor) > 1e-9 {
		t.Errorf("score %v from factor %.3f, want ping plus jitter times the factor", got.Score, got.Factor)
	}
	if stale := healthOf(r, here, t0.Add(11*time.Minute)); stale.Current {
		t.Error("a result 11 minutes old is current")
	}
	failed, _ := r.heard(result{At: t0.Add(time.Minute)})
	if got := healthOf(failed, here, t0.Add(2*time.Minute)); got.Verdict != "failed" || !got.CheckedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("after a failure the health reads %s at %v", got.Verdict, got.CheckedAt)
	}
	if none := healthOf(record{}, here, t0); none.CheckedAt != nil || none.PingP50Ms != nil || none.Score != nil || none.Verdict != "unknown" || none.Factor != 1.25 {
		t.Errorf("no record: %+v", none)
	}
}

// A heartbeat carries no ping, so the ping is as old as its newest sample to the site, not the last result.
func TestAPingIsAsOldAsItsNewestSampleToTheSite(t *testing.T) {
	now := t0.Add(72 * time.Hour)
	r := record{}
	for i, n := range []int{40, 60, 80} {
		at := t0.Add(time.Duration(i) * time.Second)
		r, _ = r.measured(at, &sample{At: at, Site: here, Ping: time.Duration(n) * time.Millisecond}, true).heard(result{At: at, Passed: true})
	}
	elsewhere := now.Add(-time.Hour)
	r, _ = r.measured(elsewhere, &sample{At: elsewhere, Site: "elsewhere", Ping: time.Second}, true).heard(result{At: elsewhere, Passed: true})
	r, _ = r.credited(now, time.Minute).heard(result{At: now, Passed: true})
	h := healthOf(r, here, now)
	if h.PingAt == nil || !h.PingAt.Equal(t0.Add(2*time.Second)) || !h.CheckedAt.Equal(now) {
		t.Errorf("ping at %v, checked at %v: want the newest sample to the site and the heartbeat", h.PingAt, h.CheckedAt)
	}
	if fresh := healthOf(r, "new-site", now); fresh.PingAt != nil {
		t.Errorf("a site with no sample has a ping from %v", *fresh.PingAt)
	}
}

// A bucket's served time counts two slots on one server once, and a heartbeat missed costs nothing.
func TestABucketCountsServedTimeOnce(t *testing.T) {
	now := t0.Add(24 * time.Hour)
	served := usage{servers: map[domain.LineageID][]interval{"a": {{t0, t0.Add(9 * time.Hour)}, {t0, t0.Add(9 * time.Hour)}, {t0.Add(18 * time.Hour), now}}}}
	series := metrics.Series{Buckets: []metrics.Bucket{{Start: t0, Lineage: "a", Purpose: domain.Heartbeat, Attempts: 360, Successes: 359}}}
	dims := map[domain.LineageID]dimension{"a": {Source: "s", Country: "NL"}}
	week := lastWeek(now)
	report := analyticsReport(series, dims, eventCounts{}, served, AnalyticsQuery{From: week.from, To: week.to, Bucket: 6 * time.Hour})
	got := map[time.Time]float64{}
	for _, b := range report.Buckets {
		got[b.Start] = b.ServedSeconds
	}
	want := map[time.Time]float64{t0: 6 * 3600, t0.Add(6 * time.Hour): 3 * 3600, t0.Add(18 * time.Hour): 6 * 3600}
	if len(got) != len(want) {
		t.Fatalf("buckets %v, want %v", got, want)
	}
	for start, w := range want {
		if got[start] != w {
			t.Errorf("bucket %v served %v s, want %v", start, got[start], w)
		}
	}
}
