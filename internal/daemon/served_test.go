package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// servedSeconds is the time the page's analytics say slots spent on a server since t0.
func servedSeconds(t *testing.T, r *rig, server domain.LineageID) float64 {
	t.Helper()
	report, err := r.app.Queries.Analytics(context.Background(), AnalyticsQuery{From: t0, To: r.clock, Bucket: 5 * time.Minute, Lineages: []domain.LineageID{server}}, r.clock)
	if err != nil {
		t.Fatal(err)
	}
	var seconds float64
	for _, b := range report.Buckets {
		seconds += b.ServedSeconds
	}
	return seconds
}

// sharedMove leaves slots eu and two shown on a, and their group's move to b owed by both.
func sharedMove(t *testing.T) (*rig, domain.EventID) {
	t.Helper()
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.putSlot("two", elected(t, "good"))
	r.settle()
	r.reportApplied("two")
	move := failOverToB(t, r)
	if r.operation("two") != move {
		t.Fatalf("two owes %q, not the group's move %q", r.operation("two"), move)
	}
	return r, move
}

func storedSwitch(t *testing.T, r *rig, id domain.EventID) domain.Switch {
	t.Helper()
	events, err := r.app.Queries.state.switches(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no event %s", id)
	return domain.Switch{}
}

// A slot that leaves its group's move before showing it, for an Exit off the VPN, another owner's
// server, or by being deleted or made again, served nothing through that move. Its peer shows the move
// later, serves b for a second and goes Direct, and b served that second alone, restart or not.
func TestASlotThatLeavesItsGroupsMoveServesNothingThroughIt(t *testing.T) {
	for _, leave := range []string{"direct", "block", "group", "source", "delete", "made again", "made again after removal"} {
		for _, restart := range []bool{false, true} {
			t.Run(leave+map[bool]string{false: "", true: " and restart"}[restart], func(t *testing.T) {
				r, move := sharedMove(t)
				switch leave {
				case "direct":
					r.setExit("eu", domain.DirectExit())
				case "block":
					r.setExit("eu", domain.BlockExit())
				case "group", "source":
					r.seed(lineage(t, "c", "else", "US", "198.51.100.30"))
					exit := domain.GroupExit(sourceGroup("else"))
					if leave == "source" {
						exit = domain.SourceExit("else")
					}
					r.putSlot("holder", exitIntent(t, "Holder", exit))
					r.frame(domain.WANUp, r.probe("c", domain.Succeeded, 20))
					r.runtimeReport("holder")
					r.setExit("eu", exit)
				default:
					if _, err := r.c.accept(context.Background(), DeleteSlot{ID: "eu"}); err != nil {
						t.Fatal(err)
					}
					if leave == "made again after removal" {
						if err := r.c.applyReport(context.Background(), gateway.Report{Deleted: []domain.SlotID{"eu"}}); err != nil {
							t.Fatal(err)
						}
					}
					if leave != "delete" {
						r.putSlot("eu", exitIntent(t, "Again", domain.BlockExit()))
					}
				}
				r.settle()
				if leave == "delete" {
					if err := r.c.applyReport(context.Background(), gateway.Report{Deleted: []domain.SlotID{"eu"}}); err != nil {
						t.Fatal(err)
					}
				} else {
					if r.operation("eu") == move {
						t.Fatal("eu still owes the group's move")
					}
					r.runtimeReport("eu")
				}
				if restart {
					dir, at := r.dir, r.clock
					r.close()
					r = newRigAt(t, dir, at)
					r.settle()
				}
				r.clock = r.clock.Add(time.Second)
				r.runtimeReport("two")
				r.clock = r.clock.Add(time.Second)
				r.setExit("two", domain.DirectExit())
				r.settle()
				r.runtimeReport("two")
				r.clock = r.clock.Add(time.Hour)
				r.settle()
				if got := servedSeconds(t, r, "b"); got != 1 {
					t.Errorf("b served %g s, want 1: only two served it", got)
				}
			})
		}
	}
}

// A slot that shows its group's move 30 s after its peer stays on a until then, and serves b from its
// own readback.
func TestASlotShowingItsGroupsMoveLateServesFromItsOwnReadback(t *testing.T) {
	r, _ := sharedMove(t)
	r.runtimeReport("eu")
	shown := r.clock
	r.clock = shown.Add(10 * time.Second)
	r.setExit("eu", domain.DirectExit())
	r.settle()
	r.runtimeReport("eu")
	r.clock = shown.Add(30 * time.Second)
	r.runtimeReport("two")
	r.clock = shown.Add(time.Minute)
	r.setExit("two", domain.DirectExit())
	r.settle()
	r.runtimeReport("two")
	r.clock = r.clock.Add(time.Hour)
	r.settle()
	if got := servedSeconds(t, r, "b"); got != 40 {
		t.Errorf("b served %g s, want 40: eu for 10 s, then two for 30 s from its own readback", got)
	}
	if got, want := servedSeconds(t, r, "a"), shown.Add(30*time.Second).Sub(t0).Seconds(); got != want {
		t.Errorf("a served %g s, want %g: two stayed on it until it showed the move", got, want)
	}
}

// A slot re-homed onto an owner on the same server still owes its group's move, restart or not, and
// lands it on readback.
func TestASlotReHomedOwingItsGroupsMoveStillOwesIt(t *testing.T) {
	for _, peer := range []bool{false, true} {
		for _, restart := range []bool{false, true} {
			t.Run(map[bool]string{false: "peer owing", true: "peer shown"}[peer]+map[bool]string{false: "", true: " and restart"}[restart], func(t *testing.T) {
				r, move := sharedMove(t)
				if peer {
					r.runtimeReport("two")
				}
				r.putPicked("other", "Other", "a", "b")
				r.setExit("eu", domain.GroupExit("other"))
				r.settle()
				if restart {
					dir, at := r.dir, r.clock
					r.close()
					r = newRigAt(t, dir, at)
					r.settle()
				}
				if p := r.c.state.pending["eu"]; p.ID != move || !slices.Contains(p.Slots, "eu") {
					t.Fatalf("re-homed eu owes %+v, want %s", p, move)
				}
				r.runtimeReport("eu")
				if r.operation("eu") != "" || !slices.Contains(storedSwitch(t, r, move).Slots, "eu") {
					t.Fatal("readback left the move owing, or the move forgot it asked eu")
				}
			})
		}
	}
}

// The last slot on a group re-homed onto the server, group or source of the move it owes keeps owing
// it across a restart, and lands it on readback.
func TestTheLastSlotReHomedOwingItsGroupsMoveStillOwesIt(t *testing.T) {
	for _, exit := range []domain.Exit{domain.GroupExit("other"), domain.SourceExit("good"), domain.ServerExit("b")} {
		t.Run(exit.Kind(), func(t *testing.T) {
			r := newRig(t, t.TempDir())
			electA(t, r)
			r.putPicked("other", "Other", "a", "b")
			move := failOverToB(t, r)
			r.setExit("eu", exit)
			r.settle()
			dir, at := r.dir, r.clock
			r.close()
			r = newRigAt(t, dir, at)
			r.settle()
			if p := r.c.state.pending["eu"]; p.ID != move || !slices.Contains(p.Slots, "eu") {
				t.Fatalf("eu owes %+v, want %s", p, move)
			}
			r.runtimeReport("eu")
			if r.operation("eu") != "" {
				t.Fatal("readback left the move owing")
			}
		})
	}
}
