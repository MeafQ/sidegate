package daemon

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

func (r *rig) record(id domain.LineageID) record { return r.c.evidence.book.records[id] }

// tested is an owner's Test of a server, with a ping.
func (r *rig) tested(id string, ms int) domain.Signal {
	s := r.probe(id, domain.Succeeded, ms)
	ping := time.Duration(ms) * time.Millisecond
	s.Purpose, s.Ping = domain.ManualProbe, &ping
	return s
}

// outageOfA plays a failed heartbeat of a with nothing else passing lately, then two rounds of its campaign.
func outageOfA(t *testing.T, r *rig, failing ...string) {
	t.Helper()
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0))
	if r.memory("eu").Campaign == nil {
		t.Fatal("a failed heartbeat began no campaign")
	}
	r.answerRounds(failing...)
	r.answerRounds(failing...)
}

func TestARoundInWhichEveryParticipantFailsChargesNoOne(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	outageOfA(t, r, "a", "b")
	for _, id := range []domain.LineageID{"a", "b"} {
		if got := r.record(id); len(got.Incidents) != 0 || got.Failures < 2 {
			t.Errorf("%s: %d failures in a row charged %d incidents, want none", id, got.Failures, len(got.Incidents))
		}
	}
	r2 := newRig(t, t.TempDir())
	electA(t, r2)
	outageOfA(t, r2, "a")
	if got := r2.record("a"); len(got.Incidents) != 1 {
		t.Errorf("failing while b passed charged a %d incidents, want 1", len(got.Incidents))
	}
	if got := r2.server("eu"); got != "b" {
		t.Errorf("the failover went to %s", got)
	}
}

func TestARestartMidOutageMakesNoSecondIncident(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	electA(t, r)
	outageOfA(t, r, "a")
	before := r.record("a")
	if len(before.Incidents) != 1 || len(before.Outages) != 1 || !before.Outages[0].End.IsZero() {
		t.Fatalf("the record holds incidents %v and outages %+v, want one incident and its outage open", before.Incidents, before.Outages)
	}
	r.close()

	r = newRigAt(t, dir, r.clock.Add(time.Minute))
	r.c.evidence.epoch = "e1"
	if got := r.record("a"); !slices.Equal(got.Outages, before.Outages) || len(got.Incidents) != 1 || got.Failures != before.Failures {
		t.Fatalf("after a restart the record holds %+v and %d failures, want %+v and %d", got.Outages, got.Failures, before.Outages, before.Failures)
	}
	for _, g := range r.app.Snapshot().Groups {
		if g.ID == sourceGroup("good") && (len(g.Order) != 2 || g.Order[0] != "b") {
			t.Errorf("before any probe since the restart the order is %v, want b ahead of the failed a", g.Order)
		}
	}
	for range 3 {
		r.clock = r.clock.Add(time.Minute)
		r.frame(domain.WANUp, r.probe("b", domain.Succeeded, 20), r.probe("a", domain.Failed, 0))
	}
	if got := r.record("a"); len(got.Incidents) != 1 || !slices.Equal(got.Outages, before.Outages) {
		t.Errorf("failing on after a restart made incidents %v and outages %+v, want the outage it had", got.Incidents, got.Outages)
	}
}

func TestTheRecordFollowsALineageAcrossARefresh(t *testing.T) {
	r := newRig(t, t.TempDir())
	remoteSource(t, r, "lw")
	refreshed(t, r.c, "lw", parsedAcme(t, acmeText(t)), r.clock)
	var id domain.LineageID
	for _, l := range r.c.world.catalogue.All() {
		id = l.ID
		if got := r.record(l.ID).FirstSeen; !got.Equal(r.clock) {
			t.Fatalf("a new server was first seen at %s, want the refresh at %s", got, r.clock)
		}
	}
	r.c.evidence.epoch = "e1"
	r.frame(domain.WANUp, r.tested(string(id), 40))
	before := r.ref(string(id))

	r.clock = r.clock.Add(time.Hour)
	refreshed(t, r.c, "lw", parsedAcme(t, rotated(acmeText(t))), r.clock)
	after := r.ref(string(id))
	if after == before {
		t.Fatal("the rotation kept the server's revision, so it proves nothing")
	}
	r.frame(domain.WANUp, r.tested(string(id), 60))
	got := r.record(id)
	if len(got.Samples) != 2 || !got.FirstSeen.Equal(t0) {
		t.Errorf("across the refresh the record kept %d samples from %s, want both Tests and the first sight", len(got.Samples), got.FirstSeen)
	}
	r.clock = r.clock.Add(recordFlushEvery)
	r.settle()
	state, _, err := r.c.store.recover(context.Background(), r.clock, r.c.timings)
	if err != nil {
		t.Fatal(err)
	}
	if stored := state.records[id]; len(stored.Samples) != 2 {
		t.Errorf("the hourly flush stored %d samples, want 2", len(stored.Samples))
	}
}

// The first pass after the WAN comes back credits nothing, however late in the outage the WAN was
// last seen down, and the next pass credits from it.
func TestSeenWorkingTimeStopsWhileTheWANIsDown(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	seen := func() float64 { return r.record("a").seenDays(r.clock) * 24 * 3600 }
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10))
	start := seen()
	for range 10 {
		r.clock = r.clock.Add(5 * time.Second)
		r.frame(domain.WANDown)
	}
	r.clock = r.clock.Add(5 * time.Second)
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10))
	if got := seen() - start; got > 0.1 {
		t.Errorf("the first pass after the WAN came back credited %.1f seconds", got)
	}
	r.clock = r.clock.Add(time.Minute)
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10))
	if got := seen() - start; math.Abs(got-60) > 0.1 {
		t.Errorf("a minute seen working with the WAN up credited %.1f seconds", got)
	}
}

func TestThePageReadsTheRecordAndTheGroupsOrder(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	outageOfA(t, r, "a")
	r.c.publish(r.clock)
	page, err := r.app.Servers(context.Background(), PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	health := map[domain.LineageID]HealthDTO{}
	for _, s := range page.Items {
		health[s.ID] = s.Health
	}
	if a := health["a"]; a.Verdict != "failed" || a.Incidents7d != 1 || a.Factor <= 1+incidentPrior || a.CheckedAt == nil {
		t.Errorf("a's health after its outage: %+v", a)
	}
	if b := health["b"]; b.Verdict != "succeeded" || b.Incidents7d != 0 {
		t.Errorf("b's health: %+v", b)
	}
	a, b := health["a"], health["b"]
	if incident := r.record("a").Incidents[0]; a.LastFailureAt == nil || !a.LastFailureAt.Equal(incident) || b.LastFailureAt != nil {
		t.Errorf("a's last failure %v, b's %v, want a's at its incident %s and none for b", a.LastFailureAt, b.LastFailureAt, incident)
	}
	if a.FirstSeenAt == nil || !a.FirstSeenAt.Equal(t0) {
		t.Errorf("a was first seen at %v, want its first result at %s", a.FirstSeenAt, t0)
	}
	if a.SeenWorking7dMs == nil || *a.SeenWorking7dMs != 0 || b.SeenWorking7dMs == nil || *b.SeenWorking7dMs <= 0 {
		t.Errorf("seen working a %v ms and b %v ms, want none for a, which failed after one pass, and some for b", a.SeenWorking7dMs, b.SeenWorking7dMs)
	}
	for _, g := range r.app.Snapshot().Groups {
		if g.ID == sourceGroup("good") && (len(g.Order) != 2 || g.Order[0] != "b" || g.Order[1] != "a" || *g.FirstChoice != "b") {
			t.Errorf("the group's order %v, first choice %v, want b before the a it left", g.Order, *g.FirstChoice)
		}
	}
}

// A failure nobody can be blamed for neither counts toward the two in a row nor resets them, so an
// incident starts at the first failure that was the server's own.
func TestAnIncidentStartsAtTheFirstFailureTheServerIsBlamedFor(t *testing.T) {
	round := func(b *book, at time.Time, campaign domain.CampaignID, rival domain.Verdict) {
		a := domain.Signal{Stamp: domain.Stamp{At: at}, Server: ref("a"), Kind: domain.ActiveProbe, Purpose: domain.Confirmation, Target: "t1", Verdict: domain.Failed, Campaign: campaign}
		other := a
		other.Server, other.Verdict = ref("b"), rival
		b.hear([]domain.Signal{a, other}, time.Time{})
	}
	b := newBook(nil)
	round(b, t0, "all-fail", domain.Failed)
	round(b, t0.Add(time.Second), "all-fail", domain.Failed)
	blamed := t0.Add(10 * time.Minute)
	round(b, blamed, "later", domain.Succeeded)
	if got := b.records["a"].Incidents; len(got) != 0 {
		t.Fatalf("one failure a was blamed for, after rounds where everything failed, charged %+v", got)
	}
	round(b, blamed.Add(time.Second), "all-fail-again", domain.Failed)
	round(b, blamed.Add(2*time.Second), "later", domain.Succeeded)
	if got := b.records["a"].Outages; len(got) != 1 || !got[0].Start.Equal(blamed) {
		t.Errorf("the second failure a was blamed for charged %+v, want one incident from %s", got, blamed)
	}
}

// A result that completes after a newer one changes nothing: not the last result, not the failures in
// a row, not a recovery under way.
func TestAResultOlderThanTheLastChangesNothing(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	newer := r.probe("a", domain.Succeeded, 10)
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, newer)
	late := newer
	late.Stamp.At, late.Verdict, late.NewConn = newer.Stamp.At.Add(-time.Second), domain.Failed, nil
	r.clock = r.clock.Add(3 * time.Second)
	r.frame(domain.WANUp, late)
	if got := r.record("a"); got.Failures != 0 || !got.Last.Equal(newer.Stamp.At) {
		t.Errorf("a late failure left %d failures and the last result at %s, want none and %s", got.Failures, got.Last, newer.Stamp.At)
	}
	h := healthOf(r.record("a"), "", r.clock)
	if h.Verdict != "succeeded" || h.CheckedAt == nil || !h.CheckedAt.Equal(newer.Stamp.At) {
		t.Errorf("health reads %s at %v, want the newer pass", h.Verdict, h.CheckedAt)
	}

	r.clock = r.clock.Add(time.Minute)
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0))
	r.clock = r.clock.Add(time.Minute)
	recovered := r.probe("a", domain.Succeeded, 10)
	r.frame(domain.WANUp, recovered)
	late = recovered
	late.Stamp.At, late.Verdict, late.NewConn = recovered.Stamp.At.Add(-time.Second), domain.Failed, nil
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, late)
	for range recoverPasses - 1 {
		r.clock = r.clock.Add(recoverSpacing)
		r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10))
	}
	if got := r.record("a"); got.standing() != passing {
		t.Errorf("a late failure in a recovery left a %d after three spaced passes, want passing", got.standing())
	}
}

// A campaign's failed confirmation probes can land behind newer passes of the server. The record's verdict
// follows the newest result, so it stands passing, and the incident the move charges is already over.
// state.db holds what the live record holds.
func TestAFailureConfirmedBehindNewerPassesIsAClosedIncidentOnDiskAndLive(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	for range 3 {
		r.clock = r.clock.Add(time.Second)
		r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 20))
	}
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 2000))
	var newest domain.Signal
	for range 2 {
		c := r.memory("eu").Campaign
		if c == nil {
			t.Fatal("a slow heartbeat began no campaign")
		}
		failed, good := r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20)
		failed.Purpose, failed.Campaign, failed.Round = domain.Confirmation, c.ID, c.Round
		good.Purpose, good.Campaign, good.Round = domain.Confirmation, c.ID, c.Round
		r.clock = r.clock.Add(time.Second)
		newest = r.tested("a", 10)
		r.frame(domain.WANUp, newest)
		r.clock = r.clock.Add(time.Second)
		r.frame(domain.WANUp, failed, good)
	}
	moved := r.record("a")
	if len(moved.Incidents) != 1 || len(moved.Outages) != 1 {
		t.Fatalf("the confirmed move charged incidents %v and outages %+v, want one of each", moved.Incidents, moved.Outages)
	}
	if moved.standing() != passing || !moved.Last.Equal(newest.Stamp.At) {
		t.Errorf("the record stands %d at %s, want passing on the newest Test at %s", moved.standing(), moved.Last, newest.Stamp.At)
	}
	if moved.Outages[0].End.IsZero() {
		t.Errorf("the outage %+v is open on a record that stands passing", moved.Outages[0])
	}
	persisted := func(step string) {
		t.Helper()
		stored, err := readRecords(context.Background(), r.app.Queries.state.reader)
		if err != nil {
			t.Fatal(err)
		}
		if live := r.record("a"); !slices.Equal(stored["a"].Outages, live.Outages) || !slices.Equal(stored["a"].Incidents, live.Incidents) {
			t.Errorf("%s state.db holds %v and %+v, live %v and %+v", step, stored["a"].Incidents, stored["a"].Outages, live.Incidents, live.Outages)
		}
	}
	persisted("after the move")

	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, r.tested("a", 10))
	persisted("after the next pass")
}

// Two groups share a, and their campaigns run a second apart, each scheduled on its own. One moves off a
// for slowness, which is an incident and no outage. The other then confirms failures that began before
// it, and that outage starts at the first confirmed failure, on disk and live.
func TestASlownessIncidentKeepsAnOutageConfirmedAfterIt(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.putPicked("other", "Other", "a", "b")
	for range 3 {
		r.clock = r.clock.Add(time.Second)
		r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 20))
	}
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 2000))
	start := r.clock
	loop := newSignalLoop(r.writer, nil, "")
	loop.routes[r.ref("b")] = gateway.ProbeRoute{Server: r.ref("b")}
	scheduled := func(slot domain.SlotID) {
		t.Helper()
		plan, _ := r.c.observation()
		loop.reconcileRounds(plan.Rounds)
		jobs := loop.admit(failurePath, r.clock)
		if len(jobs) != 1 || len(jobs[0].wants) != 1 || jobs[0].wants[0].Campaign != r.memory(slot).Campaign.ID {
			t.Fatalf("the scheduler started %+v, want one job of %s's round alone", jobs, slot)
		}
		jobs[0].cancel = func() {}
	}
	round := func(slot domain.SlotID, failed bool) []domain.Signal {
		t.Helper()
		c := r.memory(slot).Campaign
		if c == nil {
			t.Fatalf("%s has no campaign", slot)
		}
		a, b := r.probe("a", domain.Succeeded, 1500), r.probe("b", domain.Succeeded, 20)
		slow, quick := 400*time.Millisecond, 20*time.Millisecond
		a.Ping, b.Ping = &slow, &quick
		if failed {
			a.Verdict, a.NewConn, a.Ping = domain.Failed, nil, nil
		}
		for _, s := range []*domain.Signal{&a, &b} {
			s.Purpose, s.Campaign, s.Round = domain.Confirmation, c.ID, c.Round
		}
		return []domain.Signal{a, b}
	}
	scheduled("eu")
	failed1 := round("eu", true)
	r.clock = r.clock.Add(time.Second)
	r.putSlot("other", exitIntent(t, "Other", domain.GroupExit("other")))
	r.settle()
	r.reportApplied("other")
	if r.server("other") != "a" {
		t.Fatalf("other serves %s, want a", r.server("other"))
	}
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 2000))
	if r.memory("other").Campaign == nil {
		t.Fatal("a slow heartbeat began no campaign for other")
	}
	scheduled("other")
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, round("other", false)...)
	r.clock = r.clock.Add(time.Second)
	scheduled("other")
	slow2 := round("other", false)
	r.frame(domain.WANUp, failed1...)
	r.clock = r.clock.Add(time.Second)
	scheduled("eu")
	failed2 := round("eu", true)
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, slow2...)
	if r.server("other") != "b" {
		t.Fatalf("slowness left other on %s, want b", r.server("other"))
	}
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, failed2...)
	if r.server("eu") != "b" {
		t.Fatalf("the failures left eu on %s, want b", r.server("eu"))
	}
	if got := incidents7d(r.record("a"), r.clock); got != 2 {
		t.Errorf("a slowness move and a failure move on a charged %d incidents, want two", got)
	}
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, r.tested("a", 10))
	stored, err := readRecords(context.Background(), r.app.Queries.state.reader)
	if err != nil {
		t.Fatal(err)
	}
	for where, outages := range map[string][]episode{"on disk": stored["a"].Outages, "live": r.record("a").Outages} {
		if len(outages) != 1 || !outages[0].Start.Equal(start) || !outages[0].End.IsZero() {
			t.Errorf("%s a's outages are %+v, want one open since the first confirmed failure at %s", where, outages, start)
		}
	}
}

// A move and the incident it charges commit together, so a crash after the move's effects can't keep
// the move and lose the incident.
func TestASlownessMoveAndItsIncidentCommitTogether(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	electA(t, r)
	for range 3 {
		r.clock = r.clock.Add(time.Second)
		r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 20))
	}
	r.clock = r.clock.Add(time.Second)
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 2000))
	if r.memory("eu").Campaign == nil {
		t.Fatal("a slow heartbeat began no campaign")
	}
	for round := range 2 {
		c := r.memory("eu").Campaign
		r.clock = r.clock.Add(time.Second)
		a, b := r.probe("a", domain.Succeeded, 1500), r.probe("b", domain.Succeeded, 20)
		slow, quick := 400*time.Millisecond, 20*time.Millisecond
		a.Ping, b.Ping = &slow, &quick
		for _, s := range []*domain.Signal{&a, &b} {
			s.Purpose, s.Campaign, s.Round = domain.Confirmation, c.ID, c.Round
		}
		r.c.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: r.clock}, Signals: []domain.Signal{a, b}})
		if round == 0 {
			r.settle()
			continue
		}
		if err := r.c.evaluate(context.Background(), r.clock); err != nil {
			t.Fatal(err)
		}
	}
	if r.server("eu") != "b" || incidents7d(r.record("a"), r.clock) != 1 {
		t.Fatalf("slowness moved eu to %s and charged a %d incidents, want b and one", r.server("eu"), incidents7d(r.record("a"), r.clock))
	}
	r.close()

	r = newRigAt(t, dir, r.clock.Add(time.Second))
	if r.server("eu") != "b" || incidents7d(r.record("a"), r.clock) != 1 {
		t.Errorf("after a crash right after the move eu is on %s and a has %d incidents, want b and one", r.server("eu"), incidents7d(r.record("a"), r.clock))
	}
}

// A server that fails as often as the rules let it keeps every outage of the week in its count.
func TestEveryOutageOfTheWeekCountsHoweverOftenTheServerFails(t *testing.T) {
	signal := func(id string, at time.Time, v domain.Verdict) domain.Signal {
		return domain.Signal{Stamp: domain.Stamp{At: at}, Server: ref(id), Kind: domain.ActiveProbe, Purpose: domain.Confirmation, Target: "t1", Verdict: v}
	}
	b := newBook(nil)
	for i := range 257 {
		at := t0.Add(time.Duration(i) * 30 * time.Minute)
		for _, failed := range []time.Time{at, at.Add(time.Second)} {
			b.hear([]domain.Signal{signal("a", failed, domain.Failed), signal("b", failed, domain.Succeeded)}, time.Time{})
		}
		for _, passed := range []time.Duration{time.Minute, 90 * time.Second, 2 * time.Minute} {
			b.hear([]domain.Signal{signal("a", at.Add(passed), domain.Succeeded)}, time.Time{})
		}
	}
	now := t0.Add(129 * time.Hour)
	r := b.records["a"]
	if got := incidents7d(r, now); got != 257 {
		t.Errorf("257 outages in 129 hours count as %d", got)
	}
}

// A full probe and a heartbeat start together and the heartbeat lands first. The verdict is the
// heartbeat's, the newer result, but the full probe's sample and its check are facts of their own and
// still count.
func TestAFullProbeLandingAfterANewerResultKeepsItsSample(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	beat := r.probe("a", domain.Succeeded, 10)
	r.frame(domain.WANUp, beat)
	check := beat
	ping := 30 * time.Millisecond
	check.Purpose, check.Ping = domain.Check, &ping
	samples := len(r.record("a").Samples)
	r.clock = r.clock.Add(2 * time.Second)
	r.frame(domain.WANUp, check)
	got := r.record("a")
	if len(got.Samples) != samples+1 || got.Samples[len(got.Samples)-1].Ping != ping || !got.Checked.Equal(check.Stamp.At) {
		t.Fatalf("the full probe left %d samples and checked at %s, want %d and %s", len(got.Samples), got.Checked, samples+1, check.Stamp.At)
	}
	if !got.Last.Equal(beat.Stamp.At) || got.standing() != passing {
		t.Fatalf("the last result is at %s standing %d", got.Last, got.standing())
	}
}

// Each minute a heartbeat and a retry of a failing server land with the same stamp, the heartbeat
// first. The retry's verdict isn't the newest, but its step is its own, so seven of them spend the ladder.
func TestARetryLandingBesideANewerResultStillSteps(t *testing.T) {
	b := newBook(nil)
	failed := func(at time.Time, p domain.ProbePurpose) []domain.Signal {
		return []domain.Signal{{Stamp: domain.Stamp{At: at}, Server: ref("a"), Kind: domain.ActiveProbe, Purpose: p, Verdict: domain.Failed}}
	}
	b.hear(failed(t0, domain.Heartbeat), time.Time{})
	for i := 1; i <= unverifiedAfter; i++ {
		at := t0.Add(time.Duration(i) * time.Minute)
		b.hear(failed(at, domain.Heartbeat), time.Time{})
		b.hear(failed(at, domain.Check), time.Time{})
	}
	if r := b.records["a"]; !unverified(r) {
		t.Fatalf("after %d failed retries the ladder stands at %d", unverifiedAfter, r.Retries)
	}
}
