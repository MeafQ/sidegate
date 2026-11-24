package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

func TestARemovedSlotsServiceSurvivesARestartAndARecreate(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	left := r.clock
	if _, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"}); err != nil {
		t.Fatal(err)
	}
	dir := r.dir
	r.close()
	r = newRigAt(t, dir, left.Add(time.Second))
	r.settle()
	removed := r.clock
	if err := r.c.applyReport(context.Background(), gateway.Report{Deleted: []domain.SlotID{"eu"}}); err != nil {
		t.Fatal(err)
	}
	r.close()
	r = newRigAt(t, dir, removed.Add(time.Hour))
	r.settle()
	if got, want := servedSeconds(t, r, "a"), removed.Sub(t0).Seconds(); got != want {
		t.Errorf("deleted served=%g want=%g", got, want)
	}
	r.putSlot("eu", exitIntent(t, "Again", domain.BlockExit()))
	r.settle()
	r.runtimeReport("eu")
	r.clock = r.clock.Add(time.Hour)
	if got, want := servedSeconds(t, r, "a"), removed.Sub(t0).Seconds(); got != want {
		t.Errorf("recreated served=%g want=%g", got, want)
	}
}

func countRows(t *testing.T, r *rig, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.c.store.writer.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestALandingThatFailsToCommitIsOwedAfterARestart(t *testing.T) {
	for _, point := range []string{"DELETE ON pending", "INSERT ON landings", "UPDATE ON switch_events"} {
		t.Run(point, func(t *testing.T) {
			r, move := sharedMove(t)
			mustExec(t, r.c.store.writer, `CREATE TRIGGER fail_commit BEFORE `+point+` BEGIN SELECT RAISE(ABORT,'commit fails'); END`)
			report := gateway.Report{Slots: []gateway.SlotReport{{Slot: "eu", Phase: gateway.Ready, Applied: r.c.state.pending["eu"].To, Operation: move}}}
			if err := r.c.applyReport(context.Background(), report); err == nil {
				t.Fatal("commit was not aborted")
			}
			if got := countRows(t, r, `SELECT count(*) FROM pending WHERE slot='eu' AND event=?`, string(move)); got != 1 {
				t.Fatalf("failed commit lost pending, rows=%d", got)
			}
			if got := countRows(t, r, `SELECT count(*) FROM landings WHERE event=?`, string(move)); got != 0 {
				t.Fatalf("failed commit left landings=%d", got)
			}
			mustExec(t, r.c.store.writer, `DROP TRIGGER fail_commit`)
			dir, at := r.dir, r.clock
			r.close()
			r = newRigAt(t, dir, at)
			r.settle()
			r.runtimeReport("eu")
			r.runtimeReport("eu")
			if got := countRows(t, r, `SELECT count(*) FROM landings WHERE event=? AND slot='eu'`, string(move)); got != 1 {
				t.Fatalf("retried readback landed %d times", got)
			}
			if got := countRows(t, r, `SELECT count(*) FROM pending WHERE slot='eu'`); got != 0 {
				t.Fatalf("retried readback still pending")
			}
			r.close()
			r = newRigAt(t, dir, at)
			if r.operation("eu") != "" {
				t.Fatal("restart after commit owes completed move")
			}
			r.runtimeReport("eu")
			if got := countRows(t, r, `SELECT count(*) FROM landings WHERE event=? AND slot='eu'`, string(move)); got != 1 {
				t.Fatalf("restart repeated landing %d times", got)
			}
		})
	}
}

func TestTrimBoundsLandingsAndSwitches(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	electA(t, r)
	old := t0.Add(-100 * 24 * time.Hour)
	var tx transaction
	for n := 0; n < 1601; n++ {
		slot := domain.SlotID(fmt.Sprintf("gone-%d", n))
		at := old.Add(time.Duration(n) * time.Second)
		e := domain.Switch{ID: domain.EventID(fmt.Sprintf("slot/%s-%d-%d", slot, at.UnixMilli(), n)), Owner: domain.SlotOwner(slot), Slots: []domain.SlotID{slot}, To: domain.Serving(r.ref("a")), Reason: domain.InitialSelection, RequestedAt: at, AppliedAt: &at}
		tx.Switches = append(tx.Switches, e)
		tx.Landings = append(tx.Landings, landing{Slot: slot, Move: &e, At: at}, landing{Slot: slot, At: at.Add(time.Second)})
	}
	if err := r.c.store.commit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err := r.c.store.trimEvents(ctx, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRows(t, r, `SELECT count(*) FROM landings`); got != 1 {
		t.Errorf("landings=%d want 1", got)
	}
	if got := countRows(t, r, `SELECT count(*) FROM switch_events`); got != 1 {
		t.Errorf("switches=%d want 1", got)
	}
	now := t0.Add(20 * 24 * time.Hour)
	u, err := usageSince(ctx, r.c.store.writer, now.Add(-weekLength), now)
	if err != nil {
		t.Fatal(err)
	}
	if got := measure(u.servers["a"], lastWeek(now)); got != weekLength {
		t.Errorf("unchanged slot served %s want %s", got, weekLength)
	}
	t.Log("1601 removed slots trim to zero landings and switches; active slot keeps one of each and all 7 days after 20 days unchanged")
}

func TestARemovalReportedAfterARecreateEndsTheOldService(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	if _, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"}); err != nil {
		t.Fatal(err)
	}
	r.settle()
	r.clock = r.clock.Add(time.Second)
	removed := r.clock
	oldReport := gateway.Report{Deleted: []domain.SlotID{"eu"}, Generation: r.c.state.runtimeGeneration}
	r.putSlot("eu", exitIntent(t, "Again", domain.BlockExit()))
	r.settle()
	if err := r.c.applyReport(context.Background(), oldReport); err != nil {
		t.Fatal(err)
	}
	if r.c.state.slots["eu"].Deleting {
		t.Fatal("new slot marked deleting")
	}
	r.clock = r.clock.Add(time.Minute)
	r.runtimeReport("eu")
	r.clock = r.clock.Add(time.Hour)
	dir, at := r.dir, r.clock
	r.close()
	r = newRigAt(t, dir, at)
	r.settle()
	got, want := servedSeconds(t, r, "a"), removed.Sub(t0).Seconds()
	t.Logf("new slot pending=%q; removal landings=%d", r.operation("eu"), countRows(t, r, `SELECT count(*) FROM landings WHERE slot='eu' AND event IS NULL`))
	if got != want {
		t.Errorf("a servedSeconds=%g want=%g; runtime removed it before the new slot landed", got, want)
	}
}
