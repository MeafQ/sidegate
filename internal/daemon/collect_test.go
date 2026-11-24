package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
	"sidegate/internal/metrics"
)

func stored(t *testing.T, r *rig, id domain.LineageID) (lineage, rec bool) {
	t.Helper()
	db := r.c.store.writer
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM lineages WHERE id=?`, string(id)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	lineage = n > 0
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE lineage=?`, string(id)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return lineage, n > 0
}

func TestADeletedServerNothingNamesIsCollectedAfterTheRetention(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	retention := time.Duration(DefaultSettings().AnalyticsRetentionDays) * 24 * time.Hour
	long, recent := t0.Add(-retention-time.Hour), t0.Add(-retention+time.Hour)
	deleted := func(id string, at time.Time) domain.Lineage {
		l := lineage(t, id, "good", "DE", "198.51.100.9")
		l.Presence, l.MissingSince = domain.Tombstone, at
		return l
	}
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.1"), deleted("gone", long), deleted("picked", long), deleted("switched", long),
		deleted("young", recent), deleted("probed", long))
	ctx := context.Background()
	if err := r.c.store.commit(ctx, transaction{
		Groups: []domain.Group{{ID: "picks", Revision: 1, Name: "Picks", Members: domain.Members{Box: domain.BoxTicked}, Ticked: []domain.LineageID{"picked"}}},
		Switches: []domain.Switch{{ID: "slot/eu-1-1", Owner: domain.SlotOwner("eu"), Slots: []domain.SlotID{"eu"}, Intent: 1,
			From: domain.Serving(domain.ServerRef{Lineage: "switched", Digest: domain.Digest{1}}), Reason: domain.FailureMove, RequestedAt: t0}},
		Records: map[domain.LineageID]record{"gone": {Last: long}, "probed": {Last: recent}},
	}); err != nil {
		t.Fatal(err)
	}
	r.close()

	r = newRigAt(t, dir, t0)
	r.settle()
	wants := map[domain.LineageID]string{"gone": "", "a": "present", "picked": "a group's pick",
		"switched": "a switch event", "young": "deleted inside the retention", "probed": "a result inside the retention"}
	for id, why := range wants {
		_, inCatalogue := r.c.world.catalogue.Lookup(id)
		row, _ := stored(t, r, id)
		switch {
		case why == "" && (inCatalogue || row):
			t.Errorf("%s, deleted long ago and named by nothing, stayed: catalogue %v, row %v", id, inCatalogue, row)
		case why != "" && (!inCatalogue || !row):
			t.Errorf("%s went, though %s names it", id, why)
		}
	}
	if _, rec := stored(t, r, "gone"); rec {
		t.Error("a collected server's record stayed")
	}
	if _, rec := stored(t, r, "probed"); !rec {
		t.Error("a kept server lost its record")
	}
}

// sampled has metrics.db keep these samples, as the running daemon's writer does.
func sampled(t *testing.T, r *rig, signals ...domain.Signal) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- r.writer.Run(ctx) }()
	for _, s := range signals {
		r.writer.Record(s)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Raw samples stay 48 hours whatever the retention, so with a day's retention a server deleted a
// day ago can still have samples analytics shows: a probe, a passive sample, traffic, or an hour's
// rollup. Each names it until it goes.
func TestADeletedServerARetainedSampleNamesStays(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Minute)
	tested := now.Add(-25 * time.Hour)
	r := newRigAt(t, dir, tested)
	ctx := context.Background()
	if _, err := r.c.accept(ctx, PutSettings{RetentionDays: 1}); err != nil {
		t.Fatal(err)
	}
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.1"), lineage(t, "passive", "good", "DE", "198.51.100.2"),
		lineage(t, "traffic", "good", "DE", "198.51.100.3"), lineage(t, "rollup", "good", "DE", "198.51.100.4"),
		lineage(t, "unsampled", "good", "DE", "198.51.100.5"))
	srtt, up, down := 40*time.Millisecond, uint64(1000), uint64(2000)
	sampled(t, r,
		domain.Signal{Stamp: domain.Stamp{At: tested}, Server: domain.ServerRef{Lineage: "a"}, Kind: domain.ActiveProbe,
			Purpose: domain.Confirmation, Target: "t1", Verdict: domain.Succeeded},
		domain.Signal{Stamp: domain.Stamp{At: tested}, Server: domain.ServerRef{Lineage: "passive"}, Kind: domain.TCPInfo, SRTT: &srtt},
		domain.Signal{Stamp: domain.Stamp{At: tested, Epoch: "e1"}, Server: domain.ServerRef{Lineage: "traffic"}, Kind: domain.OutboundCounters,
			Up: &up, Down: &down})
	db, err := openState(metricsPath(dir), "")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO rollup VALUES(?,'rollup',0,'',0,0,0,0,NULL,0,NULL,1000,2000)`, tested.Unix()/3600)
	db.Close()
	if err := r.c.store.commit(ctx, transaction{Records: map[domain.LineageID]record{"a": {Last: tested}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.accept(ctx, DeleteSource{ID: "good"}); err != nil {
		t.Fatal(err)
	}
	r.close()

	r = newRigAt(t, dir, now)
	r.settle()
	for _, id := range []domain.LineageID{"a", "passive", "traffic", "rollup"} {
		if _, kept := r.c.world.catalogue.Lookup(id); !kept {
			t.Errorf("%s went though a retained sample names it", id)
		}
	}
	if _, kept := r.c.world.catalogue.Lookup("unsampled"); kept {
		t.Error("a server no sample names stayed, so this test proves nothing")
	}
	report, err := r.app.Queries.Analytics(ctx, AnalyticsQuery{From: tested.Truncate(time.Hour), To: now, Bucket: time.Hour, Lineages: []domain.LineageID{"a"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Buckets) == 0 {
		t.Error("analytics dropped the retained sample of a deleted server")
	}
}

// A read of metrics.db that fails during collection defers it to the next trim, and the daemon
// keeps working.
func TestAFailedMetricsReadDefersCollection(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	retention := time.Duration(DefaultSettings().AnalyticsRetentionDays) * 24 * time.Hour
	gone := lineage(t, "gone", "good", "DE", "198.51.100.9")
	gone.Presence, gone.MissingSince = domain.Tombstone, t0.Add(-retention-time.Hour)
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.1"), gone)
	r.close()

	r = newRigAt(t, dir, t0)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r.app.Queries.metrics.Close()
	if err := r.c.settle(context.Background()); err != nil {
		t.Fatalf("a failed read of metrics.db stopped the daemon: %v", err)
	}
	if _, kept := r.c.world.catalogue.Lookup("gone"); !kept {
		t.Error("collection went ahead without knowing what metrics.db names")
	}
	if !strings.Contains(logs.String(), "metrics.db") {
		t.Errorf("the deferred collection wasn't logged: %q", logs.String())
	}

	reader, err := metrics.OpenReader(metricsPath(dir), metrics.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	r.app.Queries.metrics = reader
	r.clock = r.clock.Add(trimEvery)
	r.settle()
	if _, kept := r.c.world.catalogue.Lookup("gone"); kept {
		t.Error("the next trim didn't collect what the failed read deferred")
	}
}
