package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
)

var t0 = time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)

func sig(lineage string, at time.Time, verdict domain.Verdict, ms int) domain.Signal {
	s := domain.Signal{Stamp: domain.Stamp{At: at}, Server: domain.ServerRef{Lineage: domain.LineageID(lineage)},
		Kind: domain.ActiveProbe, Verdict: verdict, Purpose: domain.Confirmation, Target: "t1"}
	if ms > 0 {
		d := time.Duration(ms) * time.Millisecond
		newConn := 3 * d
		s.Ping, s.NewConn = &d, &newConn
	}
	return s
}

func counter(lineage string, epoch domain.CoreEpoch, at time.Time, up, down uint64) domain.Signal {
	return domain.Signal{Stamp: domain.Stamp{Epoch: epoch, At: at}, Server: domain.ServerRef{Lineage: domain.LineageID(lineage)},
		Kind: domain.OutboundCounters, Up: &up, Down: &down}
}

func setup(t *testing.T, o Options) (*Writer, *Reader) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.db")
	w, err := OpenWriter(path, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	r, err := OpenReader(path, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return w, r
}

func TestFlushAndSeries(t *testing.T) {
	ctx := context.Background()
	w, r := setup(t, DefaultOptions())
	excluded := sig("a", t0.Add(5*time.Minute), domain.Unknown, 0)
	excluded.WANExcluded = true
	heartbeat := sig("a", t0.Add(time.Minute), domain.Succeeded, 1)
	heartbeat.Purpose = domain.Heartbeat
	batch := []domain.Signal{
		sig("a", t0, domain.Succeeded, 100), sig("a", t0.Add(time.Minute), domain.Succeeded, 300), sig("a", t0.Add(2*time.Minute), domain.Failed, 0),
		sig("a", t0.Add(3*time.Minute), domain.Unknown, 0), excluded, heartbeat,
		sig("b", t0, domain.Succeeded, 50),
	}
	if err := w.flush(ctx, batch, nil); err != nil {
		t.Fatal(err)
	}
	s, err := r.Series(ctx, Window{From: t0, To: t0.Add(time.Hour), Step: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Buckets) != 3 {
		t.Fatalf("buckets: %+v", s.Buckets)
	}
	if beat := s.Buckets[1]; beat.Lineage != "a" || beat.Purpose != domain.Heartbeat || beat.Attempts != 1 || beat.Successes != 1 {
		t.Fatalf("a successful heartbeat counts as one attempt and one success: %+v", beat)
	}
	a := s.Buckets[0]
	if a.Lineage != "a" || a.Purpose != domain.Confirmation || a.Attempts != 3 || a.Successes != 2 || a.WANExcluded != 1 || a.Unknown != 1 {
		t.Fatalf("a: %+v", a)
	}
	if a.PingMedian == nil || *a.PingMedian != 200*time.Millisecond || a.PingSamples != 2 {
		t.Fatalf("median: %v over %d", a.PingMedian, a.PingSamples)
	}
	if a.NewConnMedian == nil || *a.NewConnMedian != 600*time.Millisecond {
		t.Fatalf("new connection median: %v", a.NewConnMedian)
	}
	if s.CoveredUntil.IsZero() {
		t.Fatal("no coverage mark")
	}
	only, err := r.Series(ctx, Window{From: t0, To: t0.Add(time.Hour), Step: time.Hour}, []domain.LineageID{"b"})
	if err != nil || len(only.Buckets) != 1 || only.Buckets[0].Lineage != "b" {
		t.Fatalf("filter: %+v, %v", only, err)
	}
	for _, bad := range []Window{{From: t0, To: t0, Step: time.Hour}, {From: t0, To: t0.Add(time.Hour), Step: time.Minute}, {From: t0.Add(-1000 * time.Hour), To: t0, Step: 5 * time.Minute}} {
		if _, err := r.Series(ctx, bad, nil); err == nil {
			t.Fatalf("window %+v accepted", bad)
		}
	}
}

func TestCountersBecomeDeltasAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	o := DefaultOptions()
	path := filepath.Join(t.TempDir(), "metrics.db")
	w, err := OpenWriter(path, o)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(path, o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := w.flush(ctx, []domain.Signal{counter("a", "e1", t0, 100, 200), counter("a", "e1", t0.Add(time.Second), 150, 260)}, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	w, err = OpenWriter(path, o) // A daemon restart: the mark must come from disk.
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.flush(ctx, []domain.Signal{counter("a", "e1", t0.Add(2*time.Second), 170, 300)}, nil); err != nil {
		t.Fatal(err)
	}
	s, err := r.Series(ctx, Window{From: t0, To: t0.Add(time.Hour), Step: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Traffic) != 1 || s.Traffic[0].Up != 170 || s.Traffic[0].Down != 300 || s.Gaps != 0 {
		t.Fatalf("same epoch: %+v, gaps %d", s.Traffic, s.Gaps)
	}
	if err := w.flush(ctx, []domain.Signal{counter("a", "e2", t0.Add(3*time.Second), 10, 20)}, nil); err != nil {
		t.Fatal(err)
	}
	s, err = r.Series(ctx, Window{From: t0, To: time.Now().Add(time.Hour), Step: 4 * time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Traffic) != 1 || s.Traffic[0].Up != 180 || s.Traffic[0].Down != 320 || s.Gaps != 1 {
		t.Fatalf("new epoch: %+v, gaps %d", s.Traffic, s.Gaps)
	}
}

func TestIdleCountersWriteNothing(t *testing.T) {
	ctx := context.Background()
	w, _ := setup(t, DefaultOptions())
	var changes int
	if err := w.flush(ctx, []domain.Signal{counter("a", "e1", t0, 1, 1)}, nil); err != nil {
		t.Fatal(err)
	}
	w.db.QueryRow(`SELECT total_changes()`).Scan(&changes)
	if err := w.flush(ctx, []domain.Signal{counter("a", "e1", t0.Add(time.Second), 1, 1)}, nil); err != nil {
		t.Fatal(err)
	}
	var after int
	w.db.QueryRow(`SELECT total_changes()`).Scan(&after)
	if after-changes > 1 { // Only the coverage mark.
		t.Fatalf("an idle counter wrote %d rows", after-changes)
	}
}

func TestRollupSurvivesRawRetention(t *testing.T) {
	ctx := context.Background()
	o := DefaultOptions()
	o.Raw = 24 * time.Hour
	w, r := setup(t, o)
	old := time.Now().UTC().Truncate(time.Hour).Add(-20 * time.Hour)
	w.now = func() time.Time { return old.Add(90 * time.Minute) }
	if err := w.flush(ctx, []domain.Signal{sig("a", old, domain.Succeeded, 100), sig("a", old.Add(time.Minute), domain.Failed, 0),
		counter("a", "e1", old, 7, 9)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	future := old.Add(40 * time.Hour)
	w.now = func() time.Time { return future }
	if err := w.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	var raw, rolled int
	w.db.QueryRow(`SELECT COUNT(*) FROM probe`).Scan(&raw)
	w.db.QueryRow(`SELECT COUNT(*) FROM rollup`).Scan(&rolled)
	if raw != 0 || rolled != 2 {
		t.Fatalf("raw %d rolled %d", raw, rolled)
	}
	r.options.Raw = -40 * time.Hour // Reads as if it were 40 h later: everything is past raw retention.
	s, err := r.Series(ctx, Window{From: old, To: old.Add(time.Hour), Step: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Buckets) != 1 || s.Buckets[0].Attempts != 2 || s.Buckets[0].Successes != 1 || s.Buckets[0].PingMedian == nil || s.Buckets[0].NewConnMedian == nil {
		t.Fatalf("buckets: %+v", s.Buckets)
	}
	if len(s.Traffic) != 1 || s.Traffic[0].Up != 7 || s.Traffic[0].Down != 9 {
		t.Fatalf("traffic: %+v", s.Traffic)
	}
}

// The size bound sheds raw hours it has just rolled up, so their rollups answer for them.
func TestAnHourTheSizeBoundShedReadsFromItsRollup(t *testing.T) {
	ctx := context.Background()
	w, r := setup(t, DefaultOptions())
	old := time.Now().UTC().Truncate(time.Hour).Add(-20 * time.Hour)
	var batch []domain.Signal
	for i := range 5000 {
		batch = append(batch, sig("a", old.Add(time.Duration(i)*time.Second/2), domain.Succeeded, 100))
	}
	batch = append(batch, sig("a", old.Add(2*time.Hour), domain.Succeeded, 100))
	if err := w.flush(ctx, batch, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	size, err := w.size(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w.options.MaxBytes = size - 1
	if err := w.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	var raw int
	w.db.QueryRow(`SELECT COUNT(*) FROM probe WHERE ts<?`, old.Add(time.Hour).UnixMilli()).Scan(&raw)
	if raw != 0 {
		t.Fatalf("the bound shed nothing of the oldest hour: %d rows left", raw)
	}
	s, err := r.Series(ctx, Window{From: old, To: old.Add(3 * time.Hour), Step: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	attempts := map[time.Time]uint64{}
	for _, b := range s.Buckets {
		attempts[b.Start] += b.Attempts
	}
	if attempts[old] != 5000 || attempts[old.Add(2*time.Hour)] != 1 {
		t.Fatalf("attempts by hour after the shed: %v", attempts)
	}
}

// A shed that ends on an hour leaves that hour's raw rows whole, so raw answers for it, late rows
// included.
func TestTheHourAfterAShedOnTheHourReadsFromRaw(t *testing.T) {
	ctx := context.Background()
	w, r := setup(t, DefaultOptions())
	old := time.Now().UTC().Truncate(time.Hour).Add(-20 * time.Hour)
	next := old.Add(time.Hour)
	var batch []domain.Signal
	for i := range 5000 {
		batch = append(batch, sig("a", old.Add(time.Duration(i)*time.Millisecond), domain.Succeeded, 100))
	}
	batch = append(batch, sig("a", next.Add(time.Minute), domain.Succeeded, 100))
	if err := w.flush(ctx, batch, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	size, err := w.size(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w.options.MaxBytes = size - 1
	if err := w.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.flush(ctx, []domain.Signal{sig("a", next.Add(2*time.Minute), domain.Succeeded, 100)}, nil); err != nil {
		t.Fatal(err)
	}
	var raw int
	w.db.QueryRow(`SELECT COUNT(*) FROM probe WHERE ts>=? AND ts<?`, next.UnixMilli(), next.Add(time.Hour).UnixMilli()).Scan(&raw)
	if raw != 2 {
		t.Fatalf("the shed reached the next hour: %d of its 2 raw rows left", raw)
	}
	s, err := r.Series(ctx, Window{From: next, To: next.Add(time.Hour), Step: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Buckets) != 1 || s.Buckets[0].Attempts != 2 || s.Buckets[0].PingP95 == nil {
		t.Fatalf("the hour after the shed, with 2 raw probes: %+v", s.Buckets)
	}
}

// The first hour raw rows answer for is rolled up too, and only raw answers for it.
func TestTheHourAtTheRawHorizonCountsOnce(t *testing.T) {
	ctx := context.Background()
	o := DefaultOptions()
	w, r := setup(t, o)
	horizon := time.Now().UTC().Add(-o.Raw).Truncate(time.Hour).Add(time.Hour)
	if err := w.flush(ctx, []domain.Signal{sig("a", horizon.Add(30*time.Minute), domain.Succeeded, 100)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := r.Series(ctx, Window{From: horizon.Add(-2 * time.Hour), To: horizon.Add(2 * time.Hour), Step: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var attempts uint64
	for _, b := range s.Buckets {
		attempts += b.Attempts
	}
	if attempts != 1 {
		t.Fatalf("one probe at the raw horizon counted %d times: %+v", attempts, s.Buckets)
	}
}

func TestSizeBoundShedsOldestRawFirst(t *testing.T) {
	ctx := context.Background()
	o := DefaultOptions()
	o.MaxBytes = 256 << 10
	w, _ := setup(t, o)
	base := time.Now().UTC().Add(-40 * time.Hour)
	var batch []domain.Signal
	for i := range 20000 {
		batch = append(batch, sig(fmt.Sprintf("lineage-%03d", i%64), base.Add(time.Duration(i)*7*time.Second), domain.Succeeded, 100))
	}
	if err := w.flush(ctx, batch, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	size, err := w.size(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kept int
	w.db.QueryRow(`SELECT COUNT(*) FROM probe`).Scan(&kept)
	t.Logf("kept %d rows in %d bytes", kept, size)
	if kept >= 20000 {
		t.Fatal("nothing shed")
	}
	if size > o.MaxBytes {
		// Raw down to minRawKept may stay over a very small bound; rollups went first then.
		var newest int64
		w.db.QueryRow(`SELECT MIN(ts) FROM probe`).Scan(&newest)
		if newest < time.Now().Add(-minRawKept-time.Hour).UnixMilli() {
			t.Fatalf("over the bound at %d bytes with raw older than the floor", size)
		}
	}
	select {
	case reason := <-w.gaps:
		if reason == "" {
			t.Fatal("empty gap")
		}
	default:
		t.Fatal("shedding left no gap")
	}
}

func TestRecordNeverBlocksAndCountsLoss(t *testing.T) {
	ctx := context.Background()
	w, r := setup(t, DefaultOptions())
	for range inbox + 5 {
		w.Record(sig("a", t0, domain.Succeeded, 1))
	}
	if w.dropped.Load() != 5 {
		t.Fatalf("dropped %d", w.dropped.Load())
	}
	if err := w.flush(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if w.dropped.Load() != 0 {
		t.Fatalf("dropped after flush %d", w.dropped.Load())
	}
	s, err := r.Series(ctx, Window{From: t0, To: time.Now().Add(time.Hour), Step: 4 * time.Hour}, nil)
	if err != nil || s.Gaps != 1 {
		t.Fatalf("gaps %d, %v", s.Gaps, err)
	}
}

func TestRunFlushesOnCancel(t *testing.T) {
	o := DefaultOptions()
	w, r := setup(t, o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	w.Record(sig("a", t0, domain.Succeeded, 1))
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s, err := r.Series(context.Background(), Window{From: t0, To: t0.Add(time.Hour), Step: time.Hour}, nil)
	if err != nil || len(s.Buckets) != 1 {
		t.Fatalf("buckets %+v, %v", s.Buckets, err)
	}
}

func TestOtherSchemaVersionsAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	w, err := OpenWriter(path, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	for _, version := range []int{1, 3, 4, 99} {
		db, err := open(path, "_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err = OpenWriter(path, DefaultOptions())
		if err == nil || !strings.Contains(err.Error(), "delete "+path) {
			t.Fatalf("schema %d: %v", version, err)
		}
	}
}

func TestRunWritesWhatWaitsWhenCancelled(t *testing.T) {
	w, r := setup(t, DefaultOptions())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cancel()
	start := time.Now().Add(-time.Minute)
	for i := range 100 {
		w.Record(sig("a", start.Add(time.Duration(i)*time.Millisecond), domain.Succeeded, 7))
	}
	go func() { done <- w.Run(ctx) }()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM probe`).Scan(&n); err != nil || n != 100 {
		t.Fatalf("%d of 100 probes, %v", n, err)
	}
}

// planner records the query plan of every read that goes through it.
type planner struct {
	db    *sql.DB
	plans []string
}

func (p *planner) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var steps []string
	err := scan(ctx, p.db, "EXPLAIN QUERY PLAN "+query, func(r *sql.Rows) error {
		var id, parent, unused int
		var step string
		err := r.Scan(&id, &parent, &unused, &step)
		steps = append(steps, step)
		return err
	}, args...)
	p.plans = append(p.plans, strings.Join(steps, "; "))
	if err != nil {
		return nil, err
	}
	return p.db.QueryContext(ctx, query, args...)
}

func TestAFewLineagesAreReadThroughTheirIndexes(t *testing.T) {
	ctx := context.Background()
	w, _ := setup(t, DefaultOptions())
	for _, c := range []struct {
		lineages []domain.LineageID
		want     []string
	}{
		{nil, []string{"probe USING INDEX probe_ts (ts>? AND ts<?)", "traffic USING PRIMARY KEY (minute>? AND minute<?)", "rollup USING PRIMARY KEY (hour>? AND hour<?)"}},
		{lineageIDs(1), []string{"probe USING INDEX probe_lineage_ts (lineage=? AND ts>? AND ts<?)", "traffic USING INDEX traffic_lineage_minute (lineage=? AND minute>? AND minute<?)",
			"rollup USING INDEX rollup_lineage_hour (lineage=? AND hour>? AND hour<?)"}},
		{lineageIDs(maxIndexedLineages), []string{"probe USING INDEX probe_lineage_ts (lineage=? AND ts>? AND ts<?)", "traffic USING INDEX traffic_lineage_minute (lineage=? AND minute>? AND minute<?)",
			"rollup USING INDEX rollup_lineage_hour (lineage=? AND hour>? AND hour<?)"}},
	} {
		p := &planner{db: w.db}
		from, to := t0.Add(-time.Hour).UnixMilli(), t0.UnixMilli()
		if _, err := readRaw(ctx, p, from, to, c.lineages, time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := readRollup(ctx, p, from, to, c.lineages, time.Hour); err != nil {
			t.Fatal(err)
		}
		if len(p.plans) != len(c.want) {
			t.Fatalf("%d lineages ran %d reads: %q", len(c.lineages), len(p.plans), p.plans)
		}
		for i, want := range c.want {
			if p.plans[i] != "SEARCH "+want {
				t.Errorf("%d lineages: plan %q, want SEARCH %s", len(c.lineages), p.plans[i], want)
			}
		}
	}
}

func lineageIDs(n int) []domain.LineageID {
	var out []domain.LineageID
	for i := range n {
		out = append(out, domain.LineageID(fmt.Sprintf("lineage-%03d", i)))
	}
	return out
}

// A file made before the lineage indexes, at the same schema version, gains them on open and keeps its rows.
func TestAnExistingFileGainsTheLineageIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	db, err := open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{strings.TrimSuffix(schema, lineageIndexes), "PRAGMA user_version=5"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "b"} {
		if _, err := db.Exec(`INSERT INTO probe VALUES(?,?,?,'t1',?,0,0,NULL,NULL)`, t0.UnixMilli(), id, int(domain.Heartbeat), int(domain.Succeeded)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO traffic VALUES(?,?,7,9)`, t0.Unix()/60, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO rollup VALUES(?,?,?,'t1',4,3,0,0,NULL,0,NULL,0,0)`, t0.Add(-72*time.Hour).Unix()/3600, id, int(domain.Heartbeat)); err != nil {
			t.Fatal(err)
		}
	}
	indexes := func() []string {
		var out []string
		if err := scan(context.Background(), db, `SELECT name FROM sqlite_schema WHERE type='index' AND name LIKE '%lineage%' ORDER BY name`, func(r *sql.Rows) error {
			var name string
			err := r.Scan(&name)
			out = append(out, name)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := indexes(); len(got) != 0 {
		t.Fatalf("the old file already has %v", got)
	}
	for range 2 { // The second open finds them made.
		w, err := OpenWriter(path, DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
	}
	if got, want := indexes(), []string{"probe_lineage_ts", "rollup_lineage_hour", "traffic_lineage_minute"}; !slices.Equal(got, want) {
		t.Errorf("indexes %v, want %v", got, want)
	}
	db.Close()
	r, err := OpenReader(path, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, err := r.Series(context.Background(), Window{From: t0.Add(-96 * time.Hour), To: t0.Add(time.Hour), Step: time.Hour}, []domain.LineageID{"b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Buckets) != 2 || s.Buckets[0].Attempts != 4 || s.Buckets[1].Attempts != 1 || len(s.Traffic) != 1 || s.Traffic[0].Down != 9 {
		t.Errorf("series: %+v", s)
	}
}

// Series for some lineages is Series for every lineage without the rest's rows, in the same order.
func TestSeriesForSomeLineagesIsTheWholeSeriesFiltered(t *testing.T) {
	ctx := context.Background()
	w, r := setup(t, DefaultOptions())
	ids := lineageIDs(maxIndexedLineages + 8)
	horizon := time.Now().UTC().Add(-DefaultOptions().Raw).Truncate(time.Hour).Add(time.Hour)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := w.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Raw and rollup rows on both sides of the horizon, which only one of them answers for.
	for i, id := range ids {
		for h := -6; h < 6; h++ {
			at := horizon.Add(time.Duration(h) * time.Hour)
			for n := range 3 {
				ts := at.Add(time.Duration(i+n*20) * time.Minute / 2)
				exec(`INSERT INTO probe VALUES(?,?,?,?,?,0,0,?,?)`, ts.UnixMilli(), string(id), 1+n%2, fmt.Sprintf("t%d", n), int(domain.Succeeded), 900+i*n, 300+i+h*n)
				exec(`INSERT OR IGNORE INTO traffic VALUES(?,?,?,?)`, ts.Unix()/60, string(id), i+1, h+10)
			}
			for purpose := range 3 {
				exec(`INSERT INTO rollup VALUES(?,?,?,'t1',?,?,0,0,?,?,?,?,?)`, at.Unix()/3600, string(id), purpose, 9, 8-purpose, 400+i+h, 8, 1000+i, i+2, h+20)
			}
		}
	}
	windows := map[string]Window{
		"raw":    {From: horizon, To: horizon.Add(6 * time.Hour), Step: 5 * time.Minute},
		"rollup": {From: horizon.Add(-6 * time.Hour), To: horizon, Step: time.Hour},
		"both":   {From: horizon.Add(-6 * time.Hour), To: horizon.Add(6 * time.Hour), Step: 4 * time.Hour},
		"empty":  {From: horizon.Add(12 * time.Hour), To: horizon.Add(18 * time.Hour), Step: time.Hour},
	}
	lists := map[string][]domain.LineageID{
		"none":         nil,
		"not nil":      {},
		"one":          {ids[3]},
		"several":      {ids[7], ids[0], ids[3]},
		"repeated":     {ids[3], ids[0], ids[3], ids[3]},
		"unknown":      {"nowhere"},
		"part unknown": {"nowhere", ids[2], ""},
		"quoted":       {`it's "a" lineage`},
		"most indexed": ids[:maxIndexedLineages],
		"long":         ids[:maxIndexedLineages+1],
		"long repeat":  slices.Repeat([]domain.LineageID{ids[1]}, maxIndexedLineages+1),
		"longer":       append(lineageIDs(500)[200:], ids[4], ids[5]),
	}
	for windowName, window := range windows {
		all, err := r.Series(ctx, window, nil)
		if err != nil {
			t.Fatal(err)
		}
		if (len(all.Buckets) == 0 || len(all.Traffic) == 0) != (windowName == "empty") {
			t.Fatalf("%s: %d buckets, %d traffic", windowName, len(all.Buckets), len(all.Traffic))
		}
		for listName, list := range lists {
			t.Run(windowName+"/"+listName, func(t *testing.T) {
				want := Series{CoveredUntil: all.CoveredUntil, Gaps: all.Gaps}
				wanted := func(id domain.LineageID) bool { return len(list) == 0 || slices.Contains(list, id) }
				for _, b := range all.Buckets {
					if wanted(b.Lineage) {
						want.Buckets = append(want.Buckets, b)
					}
				}
				for _, b := range all.Traffic {
					if wanted(b.Lineage) {
						want.Traffic = append(want.Traffic, b)
					}
				}
				got, err := r.Series(ctx, window, list)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("got %d buckets and %d traffic, want %d and %d: %+v", len(got.Buckets), len(got.Traffic), len(want.Buckets), len(want.Traffic), got)
				}
			})
		}
	}
}
