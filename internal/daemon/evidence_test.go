package daemon

import (
	"testing"
	"time"

	"sidegate/internal/domain"
)

func ms(n int) *time.Duration {
	d := time.Duration(n) * time.Millisecond
	return &d
}

func TestTheActiveRingKeepsTheNewest(t *testing.T) {
	ev := newEvidence("t1", nil)
	var last time.Time
	for i := range 3 * activeResults {
		last = t0.Add(time.Duration(i) * time.Second)
		beat := domain.Signal{Stamp: domain.Stamp{At: last}, Server: ref("a"), Kind: domain.ActiveProbe,
			Verdict: domain.Succeeded, Purpose: domain.Heartbeat, Target: "t2", NewConn: ms(900)}
		ev.ingest(signalFrame{Signals: []domain.Signal{beat}})
	}
	got := ev.results(ref("a"))
	if len(got) != activeResults || !got[len(got)-1].Stamp.At.Equal(last) {
		t.Fatalf("active holds %d ending at %v, want %d ending at %v", len(got), got[len(got)-1].Stamp.At, activeResults, last)
	}
}

func TestACoreRestartForgetsWhatTheOldCoreSaid(t *testing.T) {
	ev := newEvidence("t1", nil)
	ev.ingest(signalFrame{Signals: []domain.Signal{
		{Stamp: domain.Stamp{At: t0}, Server: ref("a"), Kind: domain.ActiveProbe, Verdict: domain.Succeeded, Purpose: domain.ManualProbe, Target: "t1", Ping: ms(70)},
		{Stamp: domain.Stamp{At: t0}, Server: ref("a"), Kind: domain.TCPInfo, Verdict: domain.Failed}}})
	ev.coreRestarted("e2")
	if len(ev.results(ref("a"))) != 0 || ev.suspicion(ref("a")).Verdict != domain.Unknown {
		t.Fatal("a result about the old core survived")
	}
}

func TestEvidenceFollowsTheLoopsRankingTarget(t *testing.T) {
	ev := newEvidence("t1", map[domain.TargetID]site{"t1": "one", "t2": "two"})
	ev.rankBy("t1", t0)
	ev.rankBy("t2", t0.Add(time.Minute))
	ev.rankBy("", t0.Add(2*time.Minute))
	if ev.ranking != "t2" || ev.previous != "one" || !ev.rankedSince.Equal(t0.Add(time.Minute)) {
		t.Fatalf("ranking %s since %s after %s", ev.ranking, ev.rankedSince, ev.previous)
	}
}

// Campaigns sharing a measurement each hear it, and it still counts once. Otherwise one round among
// three slots would look like the three successes a server needs to recover.
func TestASharedMeasurementCountsOnce(t *testing.T) {
	ev := newEvidence("t1", nil)
	ev.epoch = "e1"
	copyFor := func(campaign domain.CampaignID, at time.Time) domain.Signal {
		return domain.Signal{Stamp: domain.Stamp{Epoch: "e1", At: at}, Server: ref("a"), Kind: domain.ActiveProbe, Verdict: domain.Succeeded, Purpose: domain.Confirmation, Target: "t1", Campaign: campaign, Round: 1}
	}
	ev.ingest(signalFrame{Signals: []domain.Signal{copyFor("one", t0), copyFor("two", t0), copyFor("three", t0)}})
	if got := len(ev.results(ref("a"))); got != 1 {
		t.Fatalf("one measurement among three campaigns reads as %d", got)
	}
	if got := len(ev.roundResults(ref("a"))); got != 3 {
		t.Fatalf("%d campaigns heard it", got)
	}
	ev.ingest(signalFrame{Signals: []domain.Signal{copyFor("one", t0.Add(time.Second))}})
	if got := len(ev.results(ref("a"))); got != 2 {
		t.Fatalf("a later measurement reads as a copy: %d", got)
	}
}
