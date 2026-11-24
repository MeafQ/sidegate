// Package metrics is metrics.db: measurements, written by one goroutine and read by
// the page. It holds no intent and no decisions. Switch evidence, applied events,
// and refresh events commit synchronously in the controller's state.db; a burst of
// samples here never delays them, and retention here never touches them.
//
// Samples are keyed by lineage. Source, slot, and country are joined by the
// caller from its catalogue, so this package knows no state.db schema.
//
//	probe(ts, lineage, purpose, target, verdict, wan_excluded,
//	      no_reuse, new_conn_us, ping_us)                       raw, 48 h
//	passive(ts, lineage, srtt_us, rttvar_us, retrans,
//	        delivery_rate, stalled, certain)                    raw, 48 h
//	wan(ts, verdict)                                            changes only, 48 h
//	traffic(minute, lineage, up, down)                          deltas, 48 h
//	counter_mark(lineage, digest, epoch, up, down, seen)        watermark
//	rollup(hour, lineage, purpose, target, attempts, successes,
//	       wan_excluded, unknown, ping_p50_us, ping_n,
//	       new_conn_p50_us, up, down)                           30 d
//	gap(ts, dropped, reason)                                    30 d
//
// Purpose 0 in a rollup row is the lineage's traffic, with no target.
package metrics

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // Pure Go; builds with CGO_ENABLED=0 for linux/arm64.

	"sidegate/internal/domain"
)

const schemaVersion = 5

const schema = `
CREATE TABLE probe(ts INTEGER NOT NULL, lineage TEXT NOT NULL, purpose INTEGER NOT NULL, target TEXT NOT NULL,
	verdict INTEGER NOT NULL, wan_excluded INTEGER NOT NULL, no_reuse INTEGER NOT NULL, new_conn_us INTEGER, ping_us INTEGER);
CREATE INDEX probe_ts ON probe(ts);
CREATE TABLE passive(ts INTEGER NOT NULL, lineage TEXT NOT NULL, srtt_us INTEGER, rttvar_us INTEGER,
	retrans INTEGER, delivery_rate INTEGER, stalled INTEGER NOT NULL, certain INTEGER NOT NULL);
CREATE INDEX passive_ts ON passive(ts);
CREATE TABLE wan(ts INTEGER NOT NULL, verdict INTEGER NOT NULL);
CREATE INDEX wan_ts ON wan(ts);
CREATE TABLE traffic(minute INTEGER NOT NULL, lineage TEXT NOT NULL, up INTEGER NOT NULL, down INTEGER NOT NULL,
	PRIMARY KEY(minute, lineage)) WITHOUT ROWID;
CREATE TABLE counter_mark(lineage TEXT NOT NULL, digest BLOB NOT NULL, epoch TEXT NOT NULL, up INTEGER NOT NULL,
	down INTEGER NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY(lineage, digest)) WITHOUT ROWID;
CREATE TABLE rollup(hour INTEGER NOT NULL, lineage TEXT NOT NULL, purpose INTEGER NOT NULL, target TEXT NOT NULL,
	attempts INTEGER NOT NULL, successes INTEGER NOT NULL, wan_excluded INTEGER NOT NULL,
	unknown INTEGER NOT NULL, ping_p50_us INTEGER, ping_n INTEGER NOT NULL,
	new_conn_p50_us INTEGER, up INTEGER NOT NULL, down INTEGER NOT NULL,
	PRIMARY KEY(hour, lineage, purpose, target)) WITHOUT ROWID;
CREATE TABLE gap(ts INTEGER NOT NULL, dropped INTEGER NOT NULL, reason TEXT NOT NULL);
CREATE TABLE meta(key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID;
` + lineageIndexes

// lineageIndexes let Series read a few lineages without every other lineage's rows. They change no
// row, so a file made without them gains them on open, at the same schema version.
const lineageIndexes = `
CREATE INDEX IF NOT EXISTS probe_lineage_ts ON probe(lineage, ts);
CREATE INDEX IF NOT EXISTS traffic_lineage_minute ON traffic(lineage, minute);
CREATE INDEX IF NOT EXISTS rollup_lineage_hour ON rollup(lineage, hour);
`

// Options bounds what metrics.db keeps and how often it touches the flash.
type Options struct {
	Raw, Rollup time.Duration // Proposed 48 h and 30 d.
	MaxBytes    int64         // Past this, the oldest rows go first, raw before rollups.
	FlushEvery  time.Duration
}

// DefaultOptions keep a day and a half of raw rows at the full probe budget well
// inside MaxBytes, and write at most one batch every 30 s, since flash writes are
// scarce and a power cut losing 30 s of samples costs nothing.
func DefaultOptions() Options {
	return Options{Raw: 48 * time.Hour, Rollup: 30 * 24 * time.Hour, MaxBytes: 64 << 20, FlushEvery: 30 * time.Second}
}

const (
	inbox        = 2048
	flushRows    = 500
	deleteBatch  = 2000
	minRawKept   = 6 * time.Hour
	maintainEach = time.Hour
)

// Writer is the only writer of metrics.db.
type Writer struct {
	in      chan domain.Signal
	gaps    chan string
	dropped atomic.Uint64
	db      *sql.DB
	options Options
	rollup  atomic.Int64 // The rollup retention in force; the owner can change it live.
	batches atomic.Uint64
	marks   map[markKey]mark
	now     func() time.Time
}

type markKey struct {
	lineage domain.LineageID
	digest  domain.Digest
}

type mark struct {
	epoch    domain.CoreEpoch
	up, down uint64
}

func open(path, query string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?"+query)
}

// OpenWriter opens or creates metrics.db, root-only next to state.db. A file at another schema
// version is refused.
func OpenWriter(path string, options Options) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := open(path, "_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	w := &Writer{in: make(chan domain.Signal, inbox), gaps: make(chan string, 16), db: db, options: options,
		marks: map[markKey]mark{}, now: time.Now}
	w.rollup.Store(int64(options.Rollup))
	if err := w.ensureSchema(path); err != nil {
		db.Close()
		return nil, fmt.Errorf("metrics.db: %w", err)
	}
	return w, nil
}

// ensureSchema sets the pragmas, then creates an empty file's tables, or the indexes an existing
// file lacks. auto_vacuum must be set before the first table exists.
func (w *Writer) ensureSchema(path string) error {
	for _, p := range []string{"PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=-512", "PRAGMA journal_size_limit=4194304", "PRAGMA wal_autocheckpoint=256"} {
		if _, err := w.db.Exec(p); err != nil {
			return err
		}
	}
	var version int
	if err := w.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		_, err := w.db.Exec(lineageIndexes)
		return err
	}
	if version != 0 {
		return fmt.Errorf("schema version %d, this build needs %d: delete %s to start fresh", version, schemaVersion, path)
	}
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func (w *Writer) Close() error { return w.db.Close() }

// Batches counts the batches of samples committed so far, so a reader can tell that new
// measurements exist without reading them.
func (w *Writer) Batches() uint64 { return w.batches.Load() }

// SetRollupRetention changes how long rollups stay, from the owner's settings.
func (w *Writer) SetRollupRetention(d time.Duration) { w.rollup.Store(int64(d)) }

// Record never blocks. A full buffer drops the sample and counts it, and the next
// batch writes a gap row, so the page shows lost coverage instead of a zero.
func (w *Writer) Record(s domain.Signal) {
	select {
	case w.in <- s:
	default:
		w.dropped.Add(1)
	}
}

// Gap records collector loss that happened upstream of this Writer.
func (w *Writer) Gap(reason string) {
	select {
	case w.gaps <- reason:
	default:
		w.dropped.Add(1)
	}
}

// Run batches inserts (every FlushEvery or 500 rows, one transaction). Once an hour it
// rolls up closed hours, applies retention in small batches and the size bound, then
// runs an incremental vacuum and a WAL checkpoint. A failed batch stays in memory until
// the next try, up to the row limit, and then counts as dropped. Cancelling it writes what
// is waiting, so the actors that record must have stopped first.
func (w *Writer) Run(ctx context.Context) error {
	flush := time.NewTicker(w.options.FlushEvery)
	defer flush.Stop()
	maintain := time.NewTicker(maintainEach)
	defer maintain.Stop()
	var batch []domain.Signal
	var gaps []string
	write := func() {
		if err := w.flush(ctx, batch, gaps); err != nil {
			if len(batch) > 4*flushRows {
				w.dropped.Add(uint64(len(batch)))
				batch = batch[:0]
			}
			return
		}
		batch, gaps = batch[:0], gaps[:0]
	}
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for len(w.in) > 0 {
				batch = append(batch, <-w.in)
			}
			return w.flush(last, batch, gaps)
		case s := <-w.in:
			if batch = append(batch, s); len(batch) >= flushRows {
				write()
			}
		case g := <-w.gaps:
			gaps = append(gaps, g)
		case <-flush.C:
			if len(batch) > 0 || len(gaps) > 0 || w.dropped.Load() > 0 {
				write()
			}
		case <-maintain.C:
			write()
			if err := w.maintain(ctx); err != nil {
				gaps = append(gaps, "maintenance failed")
			}
		}
	}
}

func micros(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return d.Microseconds()
}

func nullable(n *uint64) any {
	if n == nil {
		return nil
	}
	return int64(*n)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// flush commits one batch, with counter deltas and their marks in one transaction.
func (w *Writer) flush(ctx context.Context, batch []domain.Signal, gaps []string) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pending := map[markKey]mark{}
	newGaps := slices.Clone(gaps)
	for _, s := range batch {
		switch s.Kind {
		case domain.ActiveProbe:
			_, err = tx.Exec(`INSERT INTO probe VALUES(?,?,?,?,?,?,?,?,?)`, s.Stamp.At.UnixMilli(), string(s.Server.Lineage), int(s.Purpose),
				string(s.Target), int(s.Verdict), boolInt(s.WANExcluded), boolInt(s.NoReuse), micros(s.NewConn), micros(s.Ping))
		case domain.TCPInfo:
			_, err = tx.Exec(`INSERT INTO passive VALUES(?,?,?,?,?,?,?,?)`, s.Stamp.At.UnixMilli(), string(s.Server.Lineage), micros(s.SRTT), micros(s.RTTVar),
				nullable(s.Retransmits), nullable(s.DeliveryRate), boolInt(s.Verdict == domain.Failed), boolInt(s.AttributionCertain))
		case domain.WANBaseline:
			_, err = tx.Exec(`INSERT INTO wan VALUES(?,?)`, s.Stamp.At.UnixMilli(), int(s.Verdict))
		case domain.OutboundCounters:
			err = w.countTraffic(tx, s, pending, &newGaps)
		}
		if err != nil {
			return err
		}
	}
	dropped := w.dropped.Load()
	if dropped > 0 {
		if _, err := tx.Exec(`INSERT INTO gap VALUES(?,?,?)`, w.now().UnixMilli(), dropped, "samples dropped"); err != nil {
			return err
		}
	}
	for _, g := range newGaps {
		if _, err := tx.Exec(`INSERT INTO gap VALUES(?,?,?)`, w.now().UnixMilli(), 0, g); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta VALUES('covered_until',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, w.now().UnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(batch) > 0 {
		w.batches.Add(1)
	}
	w.dropped.Add(^(dropped - 1))
	for k, m := range pending {
		w.marks[k] = m
	}
	return nil
}

// countTraffic turns a cumulative counter into a delta against its persisted mark. The
// core outlives the daemon, so a restart within an epoch continues from the mark. A new
// epoch or a decrease means the counter started over from zero, so its whole value is
// new, and what the old one counted after its last sample is lost: that is a gap.
func (w *Writer) countTraffic(tx *sql.Tx, s domain.Signal, pending map[markKey]mark, gaps *[]string) error {
	if s.Up == nil || s.Down == nil {
		return nil
	}
	key := markKey{s.Server.Lineage, s.Server.Digest}
	prev, ok := pending[key]
	if !ok {
		prev, ok = w.marks[key]
	}
	if !ok {
		var epoch string
		var up, down int64
		err := tx.QueryRow(`SELECT epoch, up, down FROM counter_mark WHERE lineage=? AND digest=?`, string(key.lineage), key.digest[:]).Scan(&epoch, &up, &down)
		switch {
		case err == nil:
			prev, ok = mark{domain.CoreEpoch(epoch), uint64(up), uint64(down)}, true
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	up, down := *s.Up, *s.Down
	switch {
	case !ok:
	case prev.epoch == s.Stamp.Epoch && up >= prev.up && down >= prev.down:
		up, down = up-prev.up, down-prev.down
	default:
		*gaps = append(*gaps, "counter restarted")
	}
	if ok && up == 0 && down == 0 && prev.epoch == s.Stamp.Epoch {
		return nil
	}
	pending[key] = mark{s.Stamp.Epoch, *s.Up, *s.Down}
	minute := s.Stamp.At.Unix() / 60
	if up > 0 || down > 0 {
		if _, err := tx.Exec(`INSERT INTO traffic VALUES(?,?,?,?) ON CONFLICT(minute, lineage) DO UPDATE SET up=up+excluded.up, down=down+excluded.down`,
			minute, string(key.lineage), int64(up), int64(down)); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO counter_mark VALUES(?,?,?,?,?,?) ON CONFLICT(lineage, digest)
		DO UPDATE SET epoch=excluded.epoch, up=excluded.up, down=excluded.down, seen=excluded.seen`,
		string(key.lineage), key.digest[:], string(s.Stamp.Epoch), int64(*s.Up), int64(*s.Down), s.Stamp.At.UnixMilli())
	return err
}

// maintain rolls up, applies retention, then the size bound.
func (w *Writer) maintain(ctx context.Context) error {
	now := w.now()
	if err := w.rollUp(ctx, now); err != nil {
		return err
	}
	rawCut := now.Add(-w.options.Raw).UnixMilli()
	rollCut := now.Add(-time.Duration(w.rollup.Load())).UnixMilli()
	for _, d := range []struct{ query, arg string }{
		{`DELETE FROM probe WHERE rowid IN (SELECT rowid FROM probe WHERE ts<? LIMIT ?)`, "raw"},
		{`DELETE FROM passive WHERE rowid IN (SELECT rowid FROM passive WHERE ts<? LIMIT ?)`, "raw"},
		{`DELETE FROM wan WHERE rowid IN (SELECT rowid FROM wan WHERE ts<? LIMIT ?)`, "raw"},
		{`DELETE FROM gap WHERE rowid IN (SELECT rowid FROM gap WHERE ts<? LIMIT ?)`, "rollup"},
	} {
		cut := rawCut
		if d.arg == "rollup" {
			cut = rollCut
		}
		if err := w.deleteAll(ctx, d.query, cut); err != nil {
			return err
		}
	}
	for _, q := range []struct {
		query string
		cut   int64
	}{
		{`DELETE FROM traffic WHERE minute<?`, rawCut / 60000},
		{`DELETE FROM rollup WHERE hour<?`, rollCut / 3600000},
		{`DELETE FROM counter_mark WHERE seen<?`, rollCut},
	} {
		if _, err := w.db.ExecContext(ctx, q.query, q.cut); err != nil {
			return err
		}
	}
	if err := w.bound(ctx, now); err != nil {
		return err
	}
	if _, err := w.db.ExecContext(ctx, "PRAGMA incremental_vacuum(2048)"); err != nil {
		return err
	}
	_, err := w.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

func (w *Writer) deleteAll(ctx context.Context, query string, cut int64) error {
	for {
		r, err := w.db.ExecContext(ctx, query, cut, deleteBatch)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n < deleteBatch {
			return nil
		}
	}
}

// size is the live data's size. Free pages don't count: they are reused before the
// file grows, and the vacuum hands them back.
func (w *Writer) size(ctx context.Context) (int64, error) {
	var pages, free, pageSize int64
	if err := w.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := w.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return 0, err
	}
	if err := w.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, err
	}
	return (pages - free) * pageSize, nil
}

// bound sheds the oldest raw rows hour by hour down to minRawKept, then the oldest
// rollups, and notes what it shed as a gap, so the page doesn't read the loss as quiet.
// Raw rows it sheds were rolled up just before, so it moves the raw horizon first, and
// Series reads those hours from their rollups.
func (w *Writer) bound(ctx context.Context, now time.Time) error {
	shed := false
	defer func() {
		if shed {
			w.Gap("size bound shed oldest rows")
		}
	}()
	for range 64 {
		size, err := w.size(ctx)
		if err != nil {
			return err
		}
		if size <= w.options.MaxBytes {
			return nil
		}
		shed = true
		var oldest sql.NullInt64
		if err := w.db.QueryRowContext(ctx, `SELECT MIN(ts) FROM probe`).Scan(&oldest); err != nil {
			return err
		}
		if oldest.Valid && oldest.Int64 < now.Add(-minRawKept).UnixMilli() {
			cut := min(oldest.Int64+time.Hour.Milliseconds(), now.Add(-minRawKept).UnixMilli())
			if _, err := w.db.ExecContext(ctx, `INSERT INTO meta VALUES('shed_until',?) ON CONFLICT(key) DO UPDATE SET value=MAX(value, excluded.value)`, cut); err != nil {
				return err
			}
			for _, q := range []string{`DELETE FROM probe WHERE ts<?`, `DELETE FROM passive WHERE ts<?`, `DELETE FROM wan WHERE ts<?`} {
				if _, err := w.db.ExecContext(ctx, q, cut); err != nil {
					return err
				}
			}
			if _, err := w.db.ExecContext(ctx, `DELETE FROM traffic WHERE minute<?`, cut/60000); err != nil {
				return err
			}
		} else {
			r, err := w.db.ExecContext(ctx, `DELETE FROM rollup WHERE hour IN (SELECT DISTINCT hour FROM rollup ORDER BY hour LIMIT 24)`)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				return nil
			}
		}
	}
	return nil
}

// rollUp folds every closed hour since the last one into rollup rows, in one
// transaction with the new horizon, so a crash can't fold an hour twice.
func (w *Writer) rollUp(ctx context.Context, now time.Time) error {
	var rolled sql.NullInt64
	if err := w.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='rolled_until'`).Scan(&rolled); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	closed := now.UnixMilli() / time.Hour.Milliseconds()
	from := rolled.Int64
	if !rolled.Valid {
		var first sql.NullInt64
		if err := w.db.QueryRowContext(ctx, `SELECT MIN(ts) FROM probe`).Scan(&first); err != nil {
			return err
		}
		from = max(first.Int64/time.Hour.Milliseconds(), closed-int64(w.options.Raw/time.Hour))
	}
	if from >= closed {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	start, end := from*time.Hour.Milliseconds(), closed*time.Hour.Milliseconds()
	series, err := readRaw(ctx, tx, start, end, nil, time.Hour)
	if err != nil {
		return err
	}
	for _, b := range series.Buckets {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO rollup VALUES(?,?,?,?,?,?,?,?,?,?,?,0,0)`, b.Start.UnixMilli()/time.Hour.Milliseconds(), string(b.Lineage), int(b.Purpose), string(b.Target),
			b.Attempts, b.Successes, b.WANExcluded, b.Unknown, micros(b.PingMedian), b.PingSamples, micros(b.NewConnMedian)); err != nil {
			return err
		}
	}
	for _, t := range series.Traffic {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO rollup VALUES(?,?,0,'',0,0,0,0,NULL,0,NULL,?,?)`, t.Start.UnixMilli()/time.Hour.Milliseconds(), string(t.Lineage),
			int64(t.Up), int64(t.Down)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta VALUES('rolled_until',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, closed); err != nil {
		return err
	}
	return tx.Commit()
}

// Reader serves the page from its own read-only connection pool.
type Reader struct {
	db      *sql.DB
	options Options
}

func OpenReader(path string, options Options) (*Reader, error) {
	db, err := open(path, "mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)&_pragma=cache_size(-256)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return &Reader{db: db, options: options}, nil
}

func (r *Reader) Close() error { return r.db.Close() }

// Named says which of ids a retained sample names, one Series could still return. This is the
// rule that a sample is a reference: state.db keeps a deleted server it names, so no sample
// outlives the name analytics joins it to.
func (r *Reader) Named(ctx context.Context, ids []domain.LineageID) (map[domain.LineageID]bool, error) {
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	var parts []string
	for _, table := range []string{"probe", "passive", "traffic", "rollup"} {
		parts = append(parts, fmt.Sprintf(`SELECT lineage FROM %s WHERE lineage IN (SELECT value FROM json_each(?1))`, table))
	}
	rows, err := r.db.QueryContext(ctx, strings.Join(parts, " UNION "), string(list))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.LineageID]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[domain.LineageID(id)] = true
	}
	return out, rows.Err()
}

type Window struct {
	From, To time.Time
	Step     time.Duration // A multiple of 5 m. Rows past raw retention resolve to the hour.
}

// Bucket is the probe results of one lineage, purpose, target and step. Attempts are successes
// plus failures, which is availability's denominator; samples the WAN excused or no one could
// judge are counted apart.
type Bucket struct {
	Start         time.Time
	Lineage       domain.LineageID
	Purpose       domain.ProbePurpose
	Target        domain.TargetID
	Attempts      uint64
	Successes     uint64
	WANExcluded   uint64
	Unknown       uint64
	PingMedian    *time.Duration
	PingP95       *time.Duration // Raw rows only: an hour's rollup keeps no percentile.
	PingSamples   uint64
	NewConnMedian *time.Duration
}

// TrafficBucket is one lineage's bytes in one step. Counters include this daemon's own probe traffic.
type TrafficBucket struct {
	Start    time.Time
	Lineage  domain.LineageID
	Up, Down uint64
}

type Series struct {
	Buckets      []Bucket
	Traffic      []TrafficBucket
	CoveredUntil time.Time // The last batch flushed; later samples are not yet visible.
	Gaps         uint64
}

const maxBuckets = 4096

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Validate says whether Series can answer this window.
func (w Window) Validate() error {
	if !w.To.After(w.From) || w.Step < 5*time.Minute || w.Step%(5*time.Minute) != 0 {
		return fmt.Errorf("window %s..%s by %s is not a range in steps of 5 m", w.From, w.To, w.Step)
	}
	if w.To.Sub(w.From)/w.Step > maxBuckets {
		return fmt.Errorf("window %s..%s by %s holds too many buckets", w.From, w.To, w.Step)
	}
	return nil
}

// Series returns probe buckets and traffic for lineages over w, or for every lineage when
// lineages is empty, in one bounded read transaction.
func (r *Reader) Series(ctx context.Context, w Window, lineages []domain.LineageID) (Series, error) {
	if err := w.Validate(); err != nil {
		return Series{}, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Series{}, err
	}
	defer tx.Rollback()
	rawFrom, err := rawHorizon(ctx, tx, time.Now(), r.options.Raw)
	if err != nil {
		return Series{}, err
	}
	from, to := w.From.UnixMilli(), w.To.UnixMilli()

	out := Series{}
	raw, err := readRaw(ctx, tx, max(from, rawFrom), to, lineages, w.Step)
	if err != nil {
		return Series{}, err
	}
	out.Buckets, out.Traffic = raw.Buckets, raw.Traffic
	if from < rawFrom {
		old, err := readRollup(ctx, tx, from, min(to, rawFrom), lineages, w.Step)
		if err != nil {
			return Series{}, err
		}
		out.Buckets, out.Traffic = append(old.Buckets, out.Buckets...), append(old.Traffic, out.Traffic...)
	}
	var covered sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='covered_until'`).Scan(&covered); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Series{}, err
	}
	if covered.Valid {
		out.CoveredUntil = time.UnixMilli(covered.Int64).UTC()
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gap WHERE ts>=? AND ts<?`, from, to).Scan(&out.Gaps); err != nil {
		return Series{}, err
	}
	return out, nil
}

// rawHorizon is the first whole hour raw rows still answer for: inside raw retention, and
// at or after where the size bound's shed stopped. Rollups answer before it.
func rawHorizon(ctx context.Context, tx *sql.Tx, now time.Time, raw time.Duration) (int64, error) {
	var shed sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='shed_until'`).Scan(&shed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	hour := time.Hour.Milliseconds()
	return max((now.Add(-raw).UnixMilli()/hour+1)*hour, (shed.Int64+hour-1)/hour*hour), nil
}

type bucketKey struct {
	start   int64
	lineage domain.LineageID
	purpose domain.ProbePurpose
	target  domain.TargetID
}

type trafficKey struct {
	start   int64
	lineage domain.LineageID
}

// maxIndexedLineages is the most lineages read through lineageIndexes, which look up each row in its
// table. A longer list can name most of a catalogue, which the time order reads in fewer pages.
const maxIndexedLineages = 16

// within is the WHERE clause, and its arguments, of the rows in [from, to) by column, of lineages or
// of every lineage when lineages is empty.
func within(column string, from, to int64, lineages []domain.LineageID) (string, []any) {
	where, args := fmt.Sprintf(" WHERE %[1]s>=? AND %[1]s<?", column), []any{from, to}
	switch n := len(lineages); {
	case n == 0:
	case n <= maxIndexedLineages:
		where += " AND lineage IN (?" + strings.Repeat(",?", n-1) + ")"
		for _, id := range lineages {
			args = append(args, string(id))
		}
	default:
		// Strings always encode, and the + keeps the query off lineageIndexes.
		list, _ := json.Marshal(lineages)
		where += " AND +lineage IN (SELECT value FROM json_each(?))"
		args = append(args, string(list))
	}
	return where, args
}

// readRaw aggregates raw rows in Go, since SQLite has no median.
func readRaw(ctx context.Context, q querier, from, to int64, lineages []domain.LineageID, step time.Duration) (Series, error) {
	if from >= to {
		return Series{}, nil
	}
	stepMs := step.Milliseconds()
	buckets := map[bucketKey]*Bucket{}
	pings, newConns := map[bucketKey][]time.Duration{}, map[bucketKey][]time.Duration{}
	where, args := within("ts", from, to, lineages)
	rows, err := q.QueryContext(ctx, `SELECT ts, lineage, purpose, target, verdict, wan_excluded, ping_us, new_conn_us FROM probe`+where, args...)
	if err != nil {
		return Series{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var lineage, target string
		var purpose, verdict, excluded int
		var ping, newConn sql.NullInt64
		if err := rows.Scan(&ts, &lineage, &purpose, &target, &verdict, &excluded, &ping, &newConn); err != nil {
			return Series{}, err
		}
		id := domain.LineageID(lineage)
		k := bucketKey{ts / stepMs * stepMs, id, domain.ProbePurpose(purpose), domain.TargetID(target)}
		b := buckets[k]
		if b == nil {
			b = &Bucket{Start: time.UnixMilli(k.start).UTC(), Lineage: id, Purpose: k.purpose, Target: k.target}
			buckets[k] = b
		}
		switch {
		case excluded != 0:
			b.WANExcluded++
		case domain.Verdict(verdict) == domain.Unknown:
			b.Unknown++
		default:
			b.Attempts++
			if domain.Verdict(verdict) == domain.Succeeded {
				b.Successes++
				if ping.Valid {
					pings[k] = append(pings[k], microseconds(ping.Int64))
				}
				if newConn.Valid {
					newConns[k] = append(newConns[k], microseconds(newConn.Int64))
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return Series{}, err
	}
	rows.Close()
	out := Series{}
	for k, b := range buckets {
		if l := pings[k]; len(l) > 0 {
			m, p := domain.Median(l), domain.P95(l)
			b.PingMedian, b.PingP95, b.PingSamples = &m, &p, uint64(len(l))
		}
		if l := newConns[k]; len(l) > 0 {
			m := domain.Median(l)
			b.NewConnMedian = &m
		}
		out.Buckets = append(out.Buckets, *b)
	}
	sortBuckets(out.Buckets)

	traffic := map[trafficKey]*TrafficBucket{}
	at := func(ts int64, id domain.LineageID) *TrafficBucket {
		k := trafficKey{ts / stepMs * stepMs, id}
		t := traffic[k]
		if t == nil {
			t = &TrafficBucket{Start: time.UnixMilli(k.start).UTC(), Lineage: id}
			traffic[k] = t
		}
		return t
	}
	where, args = within("minute", from/60000, to/60000, lineages)
	tr, err := q.QueryContext(ctx, `SELECT minute, lineage, up, down FROM traffic`+where, args...)
	if err != nil {
		return Series{}, err
	}
	defer tr.Close()
	for tr.Next() {
		var minute, up, down int64
		var lineage string
		if err := tr.Scan(&minute, &lineage, &up, &down); err != nil {
			return Series{}, err
		}
		t := at(minute*60000, domain.LineageID(lineage))
		t.Up += uint64(up)
		t.Down += uint64(down)
	}
	if err := tr.Err(); err != nil {
		return Series{}, err
	}
	for _, t := range traffic {
		out.Traffic = append(out.Traffic, *t)
	}
	sortTraffic(out.Traffic)
	return out, nil
}

// readRollup folds hourly rows into step-wide buckets. A step wider than an hour takes
// the sample-weighted median of the hourly medians, which is close enough for a page
// that compares providers over days.
func readRollup(ctx context.Context, q querier, from, to int64, lineages []domain.LineageID, step time.Duration) (Series, error) {
	hour, stepMs := time.Hour.Milliseconds(), step.Milliseconds()
	where, args := within("hour", from/hour, (to+hour-1)/hour, lineages)
	rows, err := q.QueryContext(ctx, `SELECT hour, lineage, purpose, target, attempts, successes, wan_excluded, unknown,
		ping_p50_us, ping_n, new_conn_p50_us, up, down FROM rollup`+where, args...)
	if err != nil {
		return Series{}, err
	}
	defer rows.Close()
	buckets := map[bucketKey]*Bucket{}
	pings, newConns := map[bucketKey][]weightedMedian{}, map[bucketKey][]weightedMedian{}
	traffic := map[trafficKey]*TrafficBucket{}
	for rows.Next() {
		var h, purpose int64
		var lineage, target string
		var attempts, successes, excluded, unknown, n, up, down uint64
		var ping, newConn sql.NullInt64
		if err := rows.Scan(&h, &lineage, &purpose, &target, &attempts, &successes, &excluded, &unknown, &ping, &n, &newConn, &up, &down); err != nil {
			return Series{}, err
		}
		id := domain.LineageID(lineage)
		start := h * hour / stepMs * stepMs
		if purpose == 0 {
			k := trafficKey{start, id}
			t := traffic[k]
			if t == nil {
				t = &TrafficBucket{Start: time.UnixMilli(start).UTC(), Lineage: id}
				traffic[k] = t
			}
			t.Up += up
			t.Down += down
			continue
		}
		k := bucketKey{start, id, domain.ProbePurpose(purpose), domain.TargetID(target)}
		b := buckets[k]
		if b == nil {
			b = &Bucket{Start: time.UnixMilli(start).UTC(), Lineage: id, Purpose: k.purpose, Target: k.target}
			buckets[k] = b
		}
		b.Attempts += attempts
		b.Successes += successes
		b.WANExcluded += excluded
		b.Unknown += unknown
		if ping.Valid {
			pings[k] = append(pings[k], weightedMedian{microseconds(ping.Int64), n})
		}
		if newConn.Valid {
			newConns[k] = append(newConns[k], weightedMedian{microseconds(newConn.Int64), successes})
		}
	}
	if err := rows.Err(); err != nil {
		return Series{}, err
	}
	out := Series{}
	for k, b := range buckets {
		b.PingMedian, b.PingSamples = foldMedians(pings[k])
		b.NewConnMedian, _ = foldMedians(newConns[k])
		out.Buckets = append(out.Buckets, *b)
	}
	sortBuckets(out.Buckets)
	for _, t := range traffic {
		out.Traffic = append(out.Traffic, *t)
	}
	sortTraffic(out.Traffic)
	return out, nil
}

type weightedMedian struct {
	median time.Duration
	n      uint64
}

func foldMedians(m []weightedMedian) (*time.Duration, uint64) {
	slices.SortFunc(m, func(a, b weightedMedian) int { return cmp.Compare(a.median, b.median) })
	var total, seen uint64
	for _, w := range m {
		total += w.n
	}
	for _, w := range m {
		if seen += w.n; seen*2 >= total {
			median := w.median
			return &median, total
		}
	}
	return nil, 0
}

func microseconds(us int64) time.Duration { return time.Duration(us) * time.Microsecond }

func sortBuckets(b []Bucket) {
	slices.SortFunc(b, func(x, y Bucket) int {
		return cmp.Or(x.Start.Compare(y.Start), cmp.Compare(x.Lineage, y.Lineage), cmp.Compare(x.Purpose, y.Purpose), cmp.Compare(x.Target, y.Target))
	})
}

func sortTraffic(t []TrafficBucket) {
	slices.SortFunc(t, func(x, y TrafficBucket) int {
		switch {
		case !x.Start.Equal(y.Start):
			return x.Start.Compare(y.Start)
		case x.Lineage < y.Lineage:
			return -1
		case x.Lineage > y.Lineage:
			return 1
		}
		return 0
	})
}

func scan(ctx context.Context, db querier, query string, row func(*sql.Rows) error, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := row(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
