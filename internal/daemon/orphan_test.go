package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// testCredential is every test VLESS server's user ID: vlessServer's.
const testCredential = "11111111-2222-4333-8444-555555555555"

func rowsHolding(t *testing.T, dir, text string) []string {
	t.Helper()
	db, err := openState(statePath(dir), "mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables []string
	if err := scanAll(context.Background(), db, `SELECT name FROM sqlite_master WHERE type='table'`, func(r *sql.Rows) error {
		var name string
		err := r.Scan(&name)
		tables = append(tables, name)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var holding []string
	for _, table := range tables {
		rows, err := db.Query(fmt.Sprintf("SELECT * FROM %q", table))
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			for i := range values {
				values[i] = new(any)
			}
			if err := rows.Scan(values...); err != nil {
				t.Fatal(err)
			}
			for _, v := range values {
				if strings.Contains(fmt.Sprint(*v.(*any)), text) || bytes.Contains(asBytes(*v.(*any)), []byte(text)) {
					holding = append(holding, table)
				}
			}
		}
		rows.Close()
	}
	return holding
}

func asBytes(v any) []byte {
	b, _ := v.([]byte)
	return b
}

func TestDeletingASourceLeavesNoCredentialOfItsServers(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	gone := lineage(t, "b", "good", "DE", "198.51.100.2")
	gone.Presence = domain.Tombstone
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.1"), gone)
	if got := rowsHolding(t, dir, testCredential); len(got) == 0 {
		t.Fatal("no row holds the credential before the delete, so this test proves nothing")
	}
	if _, err := r.c.accept(context.Background(), DeleteSource{ID: "good"}); err != nil {
		t.Fatal(err)
	}
	if got := rowsHolding(t, dir, testCredential); len(got) > 0 {
		t.Errorf("after the delete, the credential is in %v", got)
	}
	if got := filesHolding(dir, testCredential); len(got) > 0 {
		t.Errorf("after the delete, the credential is in the free space of %v", got)
	}
	for _, id := range []domain.LineageID{"a", "b"} {
		if l, ok := r.c.world.catalogue.Lookup(id); !ok || l.Presence != domain.Tombstone || l.Label != "DE "+string(id) {
			t.Errorf("server %s after the delete: %+v, want a tombstone that keeps its name", id, l)
		}
	}
	r.close()
	l, ok := newRig(t, dir).c.world.catalogue.Lookup("a")
	if view := serverView(l); !ok || view.Label != "DE a" || view.Protocol != "vless" || view.Transport != "tcp" {
		t.Errorf("an orphan reads back as %+v, want its name and what it spoke, as the page's history shows", view)
	}
}

// A build before this one tombstoned a deleted source's servers and kept their closures.
func TestAServerDeletedByAnEarlierBuildLosesItsCredentialAtTheNextStart(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	l := lineage(t, "a", "old", "DE", "198.51.100.1")
	r.seed(l)
	l.Presence, l.MissingSince = domain.Tombstone, r.clock
	if err := r.c.store.commit(context.Background(), transaction{Lineages: []domain.Lineage{l}, DeletedSources: []domain.SourceID{"old"}}); err != nil {
		t.Fatal(err)
	}
	r.close()
	if got := rowsHolding(t, dir, testCredential); len(got) == 0 {
		t.Fatal("the earlier build's row holds no credential, so this test proves nothing")
	}
	again := newRig(t, dir)
	again.settle()
	if got := rowsHolding(t, dir, testCredential); len(got) > 0 {
		t.Errorf("after a start, the credential is in %v", got)
	}
	if got, ok := again.c.world.catalogue.Lookup("a"); !ok || got.Presence != domain.Tombstone || !got.MissingSince.Equal(l.MissingSince) {
		t.Errorf("the orphan is %+v, want the tombstone it was, deleted when it was", got)
	}
	if got := filesHolding(dir, testCredential); len(got) > 0 {
		t.Errorf("after a start, the credential is in the free space of %v", got)
	}
}

// filesHolding lists state.db's files whose bytes hold text, in a row or in space SQLite freed.
func filesHolding(dir, text string) []string {
	var holding []string
	for _, name := range []string{"state.db", "state.db-wal"} {
		if raw, _ := os.ReadFile(filepath.Join(dir, name)); bytes.Contains(raw, []byte(text)) {
			holding = append(holding, name)
		}
	}
	return holding
}

// fileSizeCap makes every file this process writes stop growing past limit, as a full flash does,
// until the returned func lifts it.
func fileSizeCap(t *testing.T, limit uint64) func() {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: limit, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	lifted := false
	lift := func() {
		if !lifted {
			lifted = true
			syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
			signal.Reset(syscall.SIGXFSZ)
		}
	}
	t.Cleanup(lift)
	return lift
}

// A delete commits, and then the rebuild that erases it can't get the space it needs. The delete
// has happened, so the daemon acts on it, and the erase is owed until a later start pays it.
func TestADeleteWhoseEraseFailsStillLandsAndIsErasedLater(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.1"))
	mustExec(t, r.c.store.writer, `CREATE TABLE filler(b BLOB)`)
	mustExec(t, r.c.store.writer, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<1024) INSERT INTO filler SELECT randomblob(4096) FROM n`)
	mustExec(t, r.c.store.writer, `PRAGMA wal_checkpoint(TRUNCATE)`)
	r.c.publish(r.clock)
	if len(r.app.view.Load().Sources) != 1 {
		t.Fatal("the page doesn't show the source, so this test proves nothing")
	}

	lift := fileSizeCap(t, 1<<20)
	_, err := r.take(DeleteSource{ID: "good", Request: "delete-1"})
	lift()
	if err != nil {
		t.Errorf("a delete that committed answered %v", err)
	}
	if sources := r.app.view.Load().Sources; len(sources) != 0 {
		t.Errorf("the page still shows %v after the delete committed", sources)
	}
	if _, kept := r.c.state.sources["good"]; kept {
		t.Error("the daemon still holds the deleted source, so a refresh could bring its servers back")
	}
	if got := filesHolding(dir, testCredential); len(got) == 0 {
		t.Fatal("the erase passed under the cap, so this test proves nothing")
	}

	r.close()
	newRig(t, dir).settle()
	if got := filesHolding(dir, testCredential); len(got) > 0 {
		t.Errorf("after the next start, the credential is still in %v", got)
	}
}

// A page read that began before the delete holds the WAL's older frames past the busy timeout,
// so the erase can't finish then. It is owed, and paid at the next try once the read lets go.
func TestAnEraseAReaderHeldUpIsPaidOnceItLetsGo(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.1"))
	ctx := context.Background()
	read, err := r.app.Queries.state.reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := read.QueryRowContext(ctx, `SELECT count(*) FROM lineages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.accept(ctx, DeleteSource{ID: "good"}); err != nil {
		t.Fatal(err)
	}
	read.Rollback()

	r.clock = r.clock.Add(eraseRetry - time.Second)
	r.settle()
	if got := filesHolding(dir, testCredential); len(got) == 0 {
		t.Fatal("the erase was tried again before its interval")
	}
	r.clock = r.clock.Add(time.Second)
	r.settle()
	if got := filesHolding(dir, testCredential); len(got) > 0 {
		t.Errorf("%s after the failed try, the credential is still in %v", eraseRetry, got)
	}
}

// A page read older than a delete keeps the checkpoint from finishing for half an hour. The vacuum
// is done once, the WAL doesn't grow while only the checkpoint is owed, and the tries back off.
func TestAnEraseAReaderHoldsUpRetriesOnlyTheCheckpointAndBacksOff(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.1"))
	mustExec(t, r.c.store.writer, `CREATE TABLE filler(b BLOB)`)
	mustExec(t, r.c.store.writer, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<256) INSERT INTO filler SELECT randomblob(4096) FROM n`)
	mustExec(t, r.c.store.writer, `PRAGMA wal_checkpoint(TRUNCATE)`)
	mustExec(t, r.c.store.writer, `PRAGMA busy_timeout=20`)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	ctx := context.Background()
	read, err := r.app.Queries.state.reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	var n int
	if err := read.QueryRowContext(ctx, `SELECT count(*) FROM lineages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.accept(ctx, DeleteSource{ID: "good"}); err != nil {
		t.Fatal(err)
	}
	walSize := func() int64 {
		info, err := os.Stat(filepath.Join(dir, "state.db-wal"))
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	database, err := os.Stat(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	vacuumed := walSize()
	tries := []int{}
	for minute := 1; minute <= 30; minute++ {
		r.clock = r.clock.Add(time.Minute)
		before := strings.Count(logs.String(), "state.db still")
		r.settle()
		if strings.Count(logs.String(), "state.db still") > before {
			tries = append(tries, minute)
		}
	}
	if grown := walSize() - vacuumed; grown >= database.Size()/2 {
		t.Errorf("while only the checkpoint was owed, the WAL grew %d bytes, against a %d byte file: the vacuum ran again", grown, database.Size())
	}
	if len(tries) == 0 || len(tries) > 5 {
		t.Errorf("tried again at minutes %v of 30", tries)
	}
}

// A start that owes an erase and can't open metrics.db still pays the erase, since the state file's
// debt doesn't wait on the metrics file.
func TestAStartWhoseMetricsFileFailsStillPaysTheErase(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.1"))
	mustExec(t, r.c.store.writer, `CREATE TABLE filler(b BLOB)`)
	mustExec(t, r.c.store.writer, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<1024) INSERT INTO filler SELECT randomblob(4096) FROM n`)
	mustExec(t, r.c.store.writer, `PRAGMA wal_checkpoint(TRUNCATE)`)
	lift := fileSizeCap(t, 1<<20)
	_, err := r.c.accept(context.Background(), DeleteSource{ID: "good"})
	lift()
	if err != nil {
		t.Fatal(err)
	}
	r.close()
	if got := filesHolding(dir, testCredential); len(got) == 0 {
		t.Fatal("the erase passed under the cap, so this test proves nothing")
	}
	for _, name := range []string{"metrics.db", "metrics.db-wal", "metrics.db-shm"} {
		os.Remove(filepath.Join(dir, name))
	}
	if err := os.Mkdir(metricsPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}

	b := Bootstrap{DataDir: dir, RunDir: filepath.Join(dir, "run"),
		ProbeTargets: []TargetSpec{{URL: "https://target.example/"}}, BaselineTargets: []TargetSpec{{URL: "http://target.example/"}}}
	if app, err := Open(context.Background(), b); err == nil || !strings.Contains(err.Error(), "metrics.db") {
		if app != nil {
			app.Close()
		}
		t.Fatalf("Open with metrics.db a directory: %v, so this test proves nothing", err)
	}
	if got := filesHolding(dir, testCredential); len(got) > 0 {
		t.Errorf("after a start metrics.db stopped, the credential is still in %v", got)
	}
}
