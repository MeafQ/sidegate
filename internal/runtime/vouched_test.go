package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// drop takes tag off the core's list of inbounds, as a removal that landed does.
func (r *coreRig) drop(tag string) {
	r.setInbounds(slices.DeleteFunc(r.inbounds(), func(t string) bool { return t == tag })...)
}

// deadline waits out move's deadline and fires intake's step at it, as intake's timer does.
func (r *coreRig) deadline(move Passage) {
	time.Sleep(time.Until(move.until))
	r.w.gates.take(nil)
}

// forcedClose is plan p with slot's flows owing a close.
func forcedClose(p *Plan, slot int) *Plan {
	p.slots[slot].CloseFlows = domain.EventID("op" + string(p.slots[slot].Slot.ID))
	return p
}

// W2. A move a good read showed on its Selection, with nothing after it, keeps its gate through passes
// past its deadline whose entry reads fail within readBound, intake's deadline between them; each
// reports nothing it can't prove, and a pass past readBound closes it. A relisten whose add fails on a
// held port drops the claim in its pass: pxa1 is down by its end and no up comes in the next (W6).
func TestAMoveShownOnItsSelectionKeepsItsGateThroughFailedReads(t *testing.T) {
	t.Run("unread passes past the deadline", func(t *testing.T) {
		slowTest(t)
		r := newCoreRig(t)
		move := until(800 * time.Millisecond)
		p := r.plan(1, move, Passage{})
		r.healthy(p)
		for i := range 2 {
			if i == 0 {
				r.deadline(move)
			}
			r.failing("ListInbounds", 1)
			report, _ := r.pass(p)
			if one := r.slot(report, "slot1"); !unvouched(one) || !r.up("pxa1") {
				t.Fatalf("unread pass %d: slot1 reports %v on %v, up %t", i+1, one.Phase, one.Applied, r.up("pxa1"))
			}
		}
		if writes := r.flagWrites(); slices.Contains(writes, "pxa1 down") {
			t.Fatalf("pxa1 written %v", writes)
		}
		r.w.coreRead.at = time.Now().Add(-readBound)
		r.failing("ListInbounds", 1)
		r.pass(p)
		if r.up("pxa1") {
			t.Error("a pass past readBound left pxa1 up")
		}
	})
	t.Run("a relisten on a held port", func(t *testing.T) {
		r := newCoreRig(t)
		move := until(time.Minute)
		r.healthy(r.plan(1, move, Passage{}))
		r.failAdd(inboundTag("slot1", "udp"))
		report, _ := r.pass(forcedClose(r.plan(2, move, Passage{}), 0))
		if one := r.slot(report, "slot1"); one.Applied != (domain.Selection{}) || r.up("pxa1") {
			t.Fatalf("the relisten's pass: slot1 reports %v on %v, up %t", one.Phase, one.Applied, r.up("pxa1"))
		}
		report, _ = r.pass(forcedClose(r.plan(2, move, Passage{}), 0))
		if one := r.slot(report, "slot1"); one.Applied != (domain.Selection{}) || r.up("pxa1") || slices.Contains(r.flagWrites(), "pxa1 up") {
			t.Errorf("the next pass: slot1 reports %v on %v, writes %v", one.Phase, one.Applied, r.flagWrites())
		}
	})
}

// The sixth review, finding 1. A sibling's rule is missing from the core, so the whole rule list
// differs from the plan's, while slot1's own rule is as it was when it vouched for B. With the entry
// read failing after the list, across slot1's deadline, B keeps its ride: no down, and nothing claimed.
func TestASiblingsMissingRuleWithdrawsNothing(t *testing.T) {
	slowTest(t)
	r := newCoreRig(t)
	move := until(800 * time.Millisecond)
	p := r.plan(1, move, Passage{})
	r.healthy(p)
	r.write("rules", "b.slot1\n")
	r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
		if m == "GetInboundUsers" {
			r.deadline(move)
			return nil, "14", true
		}
		return nil, "", false
	})
	report, _ := r.pass(p)
	if one := r.slot(report, "slot1"); !unvouched(one) || !r.up("pxa1") || slices.Contains(r.flagWrites(), "pxa1 down") {
		t.Errorf("slot1 reports %v on %v, up %t, writes %v", one.Phase, one.Applied, r.up("pxa1"), r.flagWrites())
	}
}

// W7. A relisten's add on a held port drops slot1 from intake as it returns, so intake closes it at its
// deadline though the read after the closes failed.
func TestALostSlotLeavesIntakeEvenWhenTheNextReadFails(t *testing.T) {
	slowTest(t)
	r := newCoreRig(t)
	move := until(1500 * time.Millisecond)
	r.healthy(r.plan(1, move, Passage{}))
	r.failAdd(inboundTag("slot1", "udp"))
	start := r.logged()
	adds := 0
	r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
		if m == "ListInbounds" {
			adds++
			return nil, "14", adds == 2
		}
		return nil, "", false
	})
	r.pass(forcedClose(r.plan(2, move, Passage{}), 0))
	if calls := r.since(start); !slices.Contains(calls[slices.Index(calls, "RemoveInbound")+1:], "ListInbounds") {
		t.Fatalf("no read after the closes: %v", calls)
	}
	r.write("sys/pxa1/flags", "0x1003\n")
	r.deadline(move)
	if r.up("pxa1") {
		t.Error("intake kept the lost slot's link up past its deadline")
	}
}

// W8. A sibling's TCP close error is no core event: the lost slot1 stays unclaimed in intake and the
// report on both sides of it, and slot2's link stays up through its allowed move.
func TestASiblingsCloseErrorDoesntRepublishALostSlot(t *testing.T) {
	slowTest(t)
	r := newCoreRig(t)
	move := until(1500 * time.Millisecond)
	r.healthy(r.plan(1, move, move))
	r.failAdd(inboundTag("slot1", "udp"))
	r.closing = func(mark uint32) error {
		if mark == flowMark(r.slots[1].Allocation) {
			return errors.New("closing slot2's flows failed")
		}
		return nil
	}
	report, _ := r.pass(forcedClose(forcedClose(r.plan(2, move, move), 0), 1))
	if one := r.slot(report, "slot1"); one.Applied != (domain.Selection{}) {
		t.Errorf("slot1 reports %v", one.Applied)
	}
	if !r.up("pxa2") {
		t.Error("slot2's gate closed")
	}
	r.write("sys/pxa1/flags", "0x1003\n")
	r.deadline(move)
	if r.up("pxa1") || !r.up("pxa2") {
		t.Errorf("at the deadline: pxa1 up %t, pxa2 up %t", r.up("pxa1"), r.up("pxa2"))
	}
}

// W9. An add that failed on one start says nothing of the next: the cleanup removes the tag on e1, then
// e2 starts from its config with both sockets bound and the stop hook closes pxa1. The read naming e2
// claims slot1, its link closed, and the next pass's confirmation opens it.
func TestALossOnOneCoreStartSaysNothingOfTheNext(t *testing.T) {
	udp := inboundTag("slot1", "udp")
	for _, c := range []struct {
		name  string
		setup func(r *coreRig) *Plan
	}{
		{"a hot add", func(r *coreRig) *Plan { r.drop(udp); return r.plan(2, until(time.Minute), Passage{}) }},
		{"a move", func(r *coreRig) *Plan { return forcedClose(r.plan(2, until(time.Minute), Passage{}), 0) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCoreRig(t)
			r.healthy(r.plan(1, until(time.Minute), Passage{}))
			p := c.setup(r)
			r.failAdd(udp)
			removals := 0
			r.answering(func(m string, _ int, req []byte) ([]byte, string, bool) {
				if m == "RemoveInbound" && strings.Contains(string(req), udp) {
					removals++
					if removals == map[bool]int{true: 1, false: 2}[c.name == "a hot add"] {
						r.answering(nil)
						r.failAdd()
						r.drop(udp)
						r.restart("e2")
						return nil, "0", true
					}
				}
				return nil, "", false
			})
			report, _ := r.pass(p)
			if one := r.slot(report, "slot1"); one.CoreEpoch != "e2" || one.Applied != onSrv || one.Phase != Closed {
				t.Fatalf("slot1 reports %v on %v on %s", one.Phase, one.Applied, one.CoreEpoch)
			}
			// A drift waits a poll before its closed gate opens.
			r.w.poll = 10 * time.Millisecond
			time.Sleep(r.w.poll)
			report, _ = r.pass(r.plan(3, until(time.Minute), Passage{}))
			if one := r.slot(report, "slot1"); one.Phase != Ready || !r.up("pxa1") {
				t.Errorf("the next pass: slot1 reports %v, up %t, steps %v, error %s", one.Phase, r.up("pxa1"), report.Steps, report.Error)
			}
		})
	}
}

// W11. A TCP close that fails is no core event, so only the relisten's own read follows, and both
// healthy gates stay up. A read after a sibling's relisten that fails, wherever it fails, says nothing
// against slot1 on the same start, which stays claimed.
func TestAFlowCloseErrorReadsTheCoreNoMore(t *testing.T) {
	r := newCoreRig(t)
	p := r.plan(1, until(time.Minute), Passage{})
	r.healthy(p)
	r.closing = func(uint32) error { return errors.New("closing slot1's flows failed") }
	start := r.logged()
	report, _ := r.pass(forcedClose(r.plan(2, until(time.Minute), Passage{}), 0))
	calls := r.since(start)
	after := calls[slices.Index(calls, "RemoveInbound")+1:]
	if n := len(slices.DeleteFunc(slices.Clone(after), func(m string) bool { return m != "ListInbounds" })); n != 1 {
		t.Errorf("%d reads after the closes: %v", n, after)
	}
	for _, id := range []domain.SlotID{"slot1", "slot2"} {
		if s := r.slot(report, id); s.Phase != Ready {
			t.Errorf("%s reports %v", id, s.Phase)
		}
	}
}

func TestAFailedReadKeepsASiblingShownOnItsSelection(t *testing.T) {
	for _, at := range []string{"ListInbounds", "ListOutbounds", "ListRule", "TestRoute", "GetBalancerInfo"} {
		t.Run(at, func(t *testing.T) {
			r := newCoreRig(t)
			r.healthy(r.plan(1, until(time.Minute), until(time.Minute)))
			start := r.logged()
			r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
				calls := r.since(start)
				removal := slices.Index(calls, "RemoveInbound")
				return nil, "14", m == at && removal >= 0 && !slices.Contains(calls[removal+1:len(calls)-1], at)
			})
			report, _ := r.pass(forcedClose(r.plan(2, until(time.Minute), until(time.Minute)), 1))
			if one := r.slot(report, "slot1"); one.Phase != Ready || one.Applied != onSrv {
				t.Errorf("slot1 reports %v on %v", one.Phase, one.Applied)
			}
			if !r.up("pxa1") || !r.up("pxa2") {
				t.Errorf("pxa1 up %t, pxa2 up %t", r.up("pxa1"), r.up("pxa2"))
			}
		})
	}
}

// W19. The relisten's start read fails, its removal lands, its add fails on the held port, and the read
// after the closes fails: slot1 is refuted on the start the cleanup read, nothing claims it, and pxa1 is
// down by the end of the pass.
func TestARemovedListenerProvesNothingUntilARead(t *testing.T) {
	r := newCoreRig(t)
	move := until(time.Minute)
	r.healthy(r.plan(1, move, Passage{}))
	r.failAdd(inboundTag("slot1", "udp"))
	r.answering(relistenUnread(r, true))
	report, _ := r.pass(forcedClose(r.plan(2, move, Passage{}), 0))
	if one := r.slot(report, "slot1"); one.Applied != (domain.Selection{}) || r.up("pxa1") {
		t.Errorf("slot1 reports %v on %v, up %t", one.Phase, one.Applied, r.up("pxa1"))
	}
}

// relistenUnread fails the relisten's start read, the one unit read after the entry read, and, with
// failRead, the first list of the read after the closes.
func relistenUnread(r *coreRig, failRead bool) func(string, int, []byte) ([]byte, string, bool) {
	lists, removed := 0, false
	return func(m string, _ int, _ []byte) ([]byte, string, bool) {
		switch m {
		case "ListInbounds":
			lists++
			if lists == 1 {
				r.failUnitReads(1)
			}
			if removed && lists == 3 && failRead {
				return nil, "14", true
			}
		case "RemoveInbound":
			removed = true
		}
		return nil, "", false
	}
}

// W20. A good repair read on e1 proves both slots. slot1's relisten meets e2, and the read after the
// closes names e2 from the unit, then fails: slot2 isn't claimed on e1 in the report or in intake.
func TestAStartNamedByAFailedReadVoidsTheOldProof(t *testing.T) {
	slowTest(t)
	r := newCoreRig(t)
	short := until(1500 * time.Millisecond)
	r.healthy(r.plan(1, until(time.Minute), short))
	r.failAdd(inboundTag("slot1", "udp"))
	lists, removed := 0, false
	r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
		switch {
		case m == "RemoveInbound" && !removed:
			removed = true
			r.setStart("e2")
		case m == "ListInbounds" && removed:
			lists++
			return nil, "14", lists == 2
		}
		return nil, "", false
	})
	report, _ := r.pass(forcedClose(r.plan(2, until(time.Minute), short), 0))
	if two := r.slot(report, "slot2"); !unvouched(two) {
		t.Errorf("slot2 reports %v on %v", two.Phase, two.Applied)
	}
	r.deadline(short)
	if r.up("pxa2") {
		t.Error("intake kept slot2 on e1's proof past its deadline")
	}
}

// W21. A lost slot, moving before its deadline, has no gate through its routes: its tag stays listed
// and routed, yet nothing may be bound behind it, so pxa1 is down by the end of the pass, though the
// pass is the first to see its plan and so waits no poll to open what its read shows passing.
func TestALostSlotKeepsNoGateThroughItsRoutes(t *testing.T) {
	r := newCoreRig(t)
	r.healthy(r.plan(1, until(time.Minute), Passage{}))
	p := r.plan(2, until(time.Minute), Passage{}, onSrv, domain.Serving(testRef("next")))
	udp := inboundTag("slot1", "udp")
	r.drop(udp)
	r.failAdd(udp)
	lists := 0
	r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
		if m == "ListInbounds" {
			lists++
			return nil, "14", lists == 3 || lists == 4
		}
		return nil, "", false
	})
	report, _ := r.pass(p)
	if one := r.slot(report, "slot1"); one.Applied != (domain.Selection{}) || r.up("pxa1") || !slices.Contains(r.inbounds(), udp) {
		t.Errorf("slot1 reports %v on %v, up %t, listed %v", one.Phase, one.Applied, r.up("pxa1"), r.inbounds())
	}
}

// W22. slot1's relisten loses its UDP inbound, and slot2's TCP close is held past slot1's deadline:
// intake closes pxa1 while slot2's close is held, since the loss left intake as the add returned.
func TestALostSlotLeavesIntakeBeforeTheNextSlotsWork(t *testing.T) {
	slowTest(t)
	r := newCoreRig(t)
	move := until(1500 * time.Millisecond)
	r.healthy(r.plan(1, move, Passage{}))
	r.failAdd(inboundTag("slot1", "udp"))
	var whileHeld bool
	r.closing = func(mark uint32) error {
		if mark == flowMark(r.slots[1].Allocation) {
			r.deadline(move)
			whileHeld = r.up("pxa1")
		}
		return nil
	}
	r.pass(forcedClose(forcedClose(r.plan(2, move, Passage{}), 0), 1))
	if whileHeld || r.up("pxa1") {
		t.Errorf("pxa1 up while slot2's close was held %t, after the pass %t", whileHeld, r.up("pxa1"))
	}
}

// W26. The relisten's start read alone fails: the removal lands, the bind fails and leaves the tag
// listed, and the cleanup, with no start from before, leaves it. A good read after the closes, on the
// same start, lists both tags with the routes right, and still claims nothing: pxa1 is down by the end
// of the pass. A read after the closes that names another start claims slot1 on it.
func TestAGoodListAfterAFailedBindClaimsNothing(t *testing.T) {
	udp := inboundTag("slot1", "udp")
	t.Run("the same start", func(t *testing.T) {
		r := newCoreRig(t)
		move := until(time.Minute)
		r.healthy(r.plan(1, move, Passage{}))
		r.failAdd(udp)
		r.answering(relistenUnread(r, false))
		report, _ := r.pass(forcedClose(r.plan(2, move, Passage{}), 0))
		if one := r.slot(report, "slot1"); one.Applied != (domain.Selection{}) || r.up("pxa1") || !slices.Contains(r.inbounds(), udp) {
			t.Errorf("slot1 reports %v on %v, up %t, listed %v", one.Phase, one.Applied, r.up("pxa1"), r.inbounds())
		}
	})
	t.Run("another start", func(t *testing.T) {
		r := newCoreRig(t)
		move := until(time.Minute)
		r.healthy(r.plan(1, move, Passage{}))
		r.failAdd(udp)
		r.answering(relistenUnread(r, false))
		r.closing = func(uint32) error { r.failAdd(); r.restart("e2"); return nil }
		report, _ := r.pass(forcedClose(r.plan(2, move, Passage{}), 0))
		if one := r.slot(report, "slot1"); one.Applied != onSrv || one.CoreEpoch != "e2" {
			t.Errorf("slot1 reports %v on %v on %s", one.Phase, one.Applied, one.CoreEpoch)
		}
	})
}

// W27. The relisten's add fails, and its cleanup's list is held past slot1's deadline: intake closes
// pxa1 while the list is held. The cleanup then reads e1, and the core restarts as e2 from its config:
// the read after the closes claims slot1 on e2, its link closed by the stop hook. When both tries of the
// cleanup's start read fail, the failure counts on every start, and e2's read claims nothing.
func TestALostBindReachesIntakeBeforeItsCleanupReturns(t *testing.T) {
	slowTest(t)
	for _, unread := range []bool{false, true} {
		t.Run(map[bool]string{false: "the cleanup reads e1", true: "the cleanup reads no start"}[unread], func(t *testing.T) {
			r := newCoreRig(t)
			move := until(1500 * time.Millisecond)
			r.healthy(r.plan(1, move, Passage{}))
			r.failAdd(inboundTag("slot1", "udp"))
			removed, held, whileHeld := false, false, true
			r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
				switch {
				case m == "RemoveInbound":
					removed = true
				case m == "ListInbounds" && removed && !held:
					held = true
					r.deadline(move)
					whileHeld = r.up("pxa1")
					if unread {
						r.failUnitReads(2)
					}
				}
				return nil, "", false
			})
			r.closing = func(uint32) error { r.failAdd(); r.restart("e2"); return nil }
			report, _ := r.pass(forcedClose(r.plan(2, move, Passage{}), 0))
			if !held || whileHeld {
				t.Fatalf("the cleanup's list held %t, pxa1 up while held %t", held, whileHeld)
			}
			one := r.slot(report, "slot1")
			if want := map[bool]domain.Selection{false: onSrv, true: {}}[unread]; one.Applied != want || one.Phase != Closed || r.up("pxa1") {
				t.Errorf("slot1 reports %v on %v on %s, up %t", one.Phase, one.Applied, one.CoreEpoch, r.up("pxa1"))
			}
		})
	}
}

// W28. slot1 shown on e1, and slot2 opening. The core restarts as e2 before slot2's confirmation, which
// names e2 and then fails: intake drops both claims, and the read after the closes says what can be
// claimed on e2, nothing when it fails. A confirmation on e1 that reads slot1 unapplied ends slot1's
// ride while intake keeps it: past its deadline, the next pass's failed entry read closes pxa1.
func TestAConfirmationThatNamesANewStartWithdrawsEveryClaim(t *testing.T) {
	for _, reads := range []bool{false, true} {
		t.Run(map[bool]string{false: "the read after failing", true: "the read after naming e2"}[reads], func(t *testing.T) {
			if !reads {
				slowTest(t)
			}
			r := newCoreRig(t)
			p := r.plan(1, until(1500*time.Millisecond), Passage{})
			r.healthy(p)
			r.write("sys/pxa2/flags", "0x1002\n")
			restarted, lists := false, 0
			r.answering(func(m string, _ int, req []byte) ([]byte, string, bool) {
				switch {
				case m == "GetBalancerInfo" && strings.Contains(string(req), "b.slot2") && !restarted:
					lists++
					if lists == 3 {
						restarted = true
						r.restart("e2")
					}
				case m == "ListInbounds" && restarted && r.opening():
					return nil, "14", true
				case m == "ListInbounds" && restarted && !reads:
					return nil, "14", true
				}
				return nil, "", false
			})
			report, _ := r.pass(p)
			if !restarted {
				t.Fatal("the core never restarted")
			}
			if !reads {
				r.write("sys/pxa1/flags", "0x1003\n")
				time.Sleep(time.Until(p.slots[0].Passage.until))
				r.w.gates.take(nil)
				if r.up("pxa1") {
					t.Error("intake kept e1's claim on slot1 past its deadline")
				}
			}
			for _, id := range []domain.SlotID{"slot1", "slot2"} {
				s := r.slot(report, id)
				if want := map[bool]domain.Selection{false: {}, true: onSrv}[reads]; s.Applied != want {
					t.Errorf("%s reports %v on %v on %s", id, s.Phase, s.Applied, s.CoreEpoch)
				}
			}
		})
	}
	t.Run("a confirmation reading slot1 unapplied on e1", func(t *testing.T) {
		slowTest(t)
		r := newCoreRig(t)
		move := until(1500 * time.Millisecond)
		p := r.plan(1, move, Passage{})
		r.healthy(p)
		r.write("sys/pxa2/flags", "0x1002\n")
		r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
			if m == "ListInbounds" && r.opening() {
				r.answering(nil)
				return r.without(inboundTag("slot1", "udp")), "0", true
			}
			return nil, "", false
		})
		report, _ := r.pass(p)
		if one := r.slot(report, "slot1"); !unvouched(one) {
			t.Errorf("slot1 reports %v on %v", one.Phase, one.Applied)
		}
		r.deadline(move)
		if !r.up("pxa1") {
			t.Fatal("intake dropped slot1's claim")
		}
		r.failing("ListInbounds", 1)
		r.pass(p)
		if r.up("pxa1") {
			t.Error("the failed entry read past the deadline left pxa1 up")
		}
	})
}

// W32. A unit read that answers with no running core, deactivating or with no main process, drops
// every claim, and keep closes the gate: nothing is claimed. A unit read that fails says nothing, and
// the gate stays up.
func TestAStoppedCoreClaimsNothing(t *testing.T) {
	for name, unit := range map[string]string{
		"deactivating": "ActiveState=deactivating\nMainPID=4002\nInvocationID=e1\n",
		"no process":   "ActiveState=activating\nMainPID=0\nInvocationID=e1\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := newCoreRig(t)
			p := r.plan(1, until(time.Minute), Passage{})
			r.healthy(p)
			r.write("unit", unit)
			report, _ := r.pass(p)
			if one := r.slot(report, "slot1"); one.Applied != (domain.Selection{}) || r.up("pxa1") || r.w.gates.vouches("slot1") != unclaimed {
				t.Errorf("slot1 reports %v on %v, up %t", one.Phase, one.Applied, r.up("pxa1"))
			}
		})
	}
	t.Run("an unread unit", func(t *testing.T) {
		r := newCoreRig(t)
		p := r.plan(1, until(time.Minute), Passage{})
		r.healthy(p)
		r.failUnitReads(1)
		r.pass(p)
		if !r.up("pxa1") {
			t.Error("an unread unit closed pxa1")
		}
	})
}

// W34. W19's refuted slot whose closing ioctl fails, its link still ours and up: the sample shows it up,
// and with no claim it is Unknown, never Ready, so a Down move can't land on it. With the next plan
// Down, the next pass closes it.
func TestAFailedCloseOfAnUnclaimedSlotIsNeverReady(t *testing.T) {
	r := newCoreRig(t)
	move := until(time.Minute)
	r.healthy(r.plan(1, move, Passage{}))
	r.failAdd(inboundTag("slot1", "udp"))
	r.answering(relistenUnread(r, true))
	r.stuck = map[string]bool{"pxa1": true}
	report, err := r.pass(forcedClose(r.plan(2, move, Passage{}), 0))
	if one := r.slot(report, "slot1"); one.Phase != Unknown || !r.up("pxa1") || err == nil {
		t.Fatalf("slot1 reports %v, up %t, error %v", one.Phase, r.up("pxa1"), err)
	}
	r.answering(nil)
	r.stuck = nil
	report, _ = r.pass(r.plan(3, Passage{}, Passage{}, domain.Closed(domain.ConfirmedDead)))
	if one := r.slot(report, "slot1"); one.Phase != Closed || r.up("pxa1") {
		t.Errorf("the Down plan's pass: slot1 reports %v, up %t", one.Phase, r.up("pxa1"))
	}
}

// W38. An overdue move on B, its gate up and its claim held. The judge accepts slot2's server, so the
// same plan renders slot2's rule, which the core lacks: the entry read's rules differ while B's own
// routes name only B. Repair rewrites, its read shows B, and nothing closes pxa1: the gates vouch for B.
func TestARoutineRuleChangeKeepsAnOverdueMoveUp(t *testing.T) {
	slowTest(t)
	r := newCoreRig(t)
	third := testRef("third")
	r.servers[third] = testServer(t, vlessHop(t, "root", "198.51.100.3", ""))
	r.write("rules", "b.slot1\n")
	r.setInbounds(slices.Concat(inboundTags("slot1"), []string{probeTag})...)
	r.write("sys/pxa2/flags", "0x1002\n")
	move := until(800 * time.Millisecond)
	p := r.plan(1, move, Passage{}, onSrv, domain.Serving(third))
	r.pass(p)
	if !r.up("pxa1") || r.up("pxa2") {
		t.Fatalf("the first pass: pxa1 up %t, pxa2 up %t", r.up("pxa1"), r.up("pxa2"))
	}
	r.deadline(move)
	r.setOutbounds(testRef("srv"), testRef("next"), third)
	r.w.core.judged.Store(book(map[domain.ServerRef]verdict{testRef("srv"): {state: verdictAccepted}, testRef("next"): {state: verdictAccepted}, third: {state: verdictAccepted}}))
	r.mu.Lock()
	r.writes = nil
	r.mu.Unlock()
	report, _ := r.pass(p)
	if one := r.slot(report, "slot1"); one.Phase != Ready || slices.Contains(r.flagWrites(), "pxa1 down") || r.w.gates.vouches("slot1") != onSrv {
		t.Errorf("slot1 reports %v, writes %v, vouched %v", one.Phase, r.flagWrites(), r.w.gates.vouches("slot1"))
	}
}

// W45. The plan asks for B with prior A, and only A is accepted, so the core plan serves A before the
// deadline, while the gates vouch for B. An entry read whose outbound list lacks B, held past the
// deadline and then failed, ends B's ride: no down at the deadline while it is held, since intake keeps
// B, then one down as keep runs on its return. With B among the outbounds, nothing goes down.
func TestAnAnswerAgainstTheVouchedSelectionEndsTheRide(t *testing.T) {
	slowTest(t)
	next := domain.Serving(testRef("next"))
	for _, listed := range []bool{false, true} {
		t.Run(map[bool]string{false: "B not listed", true: "B listed"}[listed], func(t *testing.T) {
			r := newCoreRig(t)
			r.setOutbounds(testRef("srv"))
			move := MoveFrom(testRef("srv"), time.Now().Add(1500*time.Millisecond))
			p := r.plan(1, move, Passage{}, next, onSrv)
			r.pass(p)
			if listed {
				r.setOutbounds(testRef("srv"), testRef("next"))
			}
			r.w.gates.publish(claims{of: map[domain.SlotID]domain.Selection{"slot1": next, "slot2": onSrv},
				vouched: map[domain.SlotID]bool{"slot1": true, "slot2": true}, epoch: "e1"})
			r.mu.Lock()
			r.writes = nil
			r.mu.Unlock()
			whileHeld := false
			r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
				if m == "ListRule" {
					r.deadline(move)
					whileHeld = r.up("pxa1")
					return nil, "14", true
				}
				return nil, "", false
			})
			report, _ := r.pass(p)
			downs := slices.DeleteFunc(r.flagWrites(), func(w string) bool { return w != "pxa1 down" })
			if !whileHeld || listed != r.up("pxa1") || !listed && len(downs) != 1 || r.slot(report, "slot1").Applied != (domain.Selection{}) {
				t.Errorf("pxa1 up while held %t, after %t, downs %v, slot1 %v", whileHeld, r.up("pxa1"), downs, r.slot(report, "slot1").Applied)
			}
		})
	}
}

// W42, the closing. slot1's relisten add fails before its deadline, and while the read after the closes
// runs, another program's pxa1, already up, takes the name: the step that closes the refuted slot
// writes nothing to it, and slot1 reads closed.
func TestAClosingNeverLowersALinkThatIsntOurs(t *testing.T) {
	r := newCoreRig(t)
	move := until(time.Minute)
	r.healthy(r.plan(1, move, Passage{}))
	r.failAdd(inboundTag("slot1", "udp"))
	removed, lists := false, 0
	r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
		switch {
		case m == "RemoveInbound":
			removed = true
		case m == "ListInbounds" && removed:
			lists++
			if lists == 2 {
				r.write("sys/pxa1/netdev_group", "4242\n")
				r.write("sys/pxa1/flags", "0x1003\n")
			}
		}
		return nil, "", false
	})
	report, _ := r.pass(forcedClose(r.plan(2, move, Passage{}), 0))
	if one := r.slot(report, "slot1"); one.Phase != Closed || !r.up("pxa1") || slices.Contains(r.flagWrites(), "pxa1 down") {
		t.Errorf("slot1 reports %v, foreign pxa1 up %t, writes %v", one.Phase, r.up("pxa1"), r.flagWrites())
	}
}

// W31. Both slots owe a forced close and are claimed on e1. At slot1's own turn its start read names
// e2: neither close is spent, and nothing is removed, added or closed in that pass. The next pass,
// reading e2, closes each once with e2's process.
func TestACloseWhoseStartChangedIsNotSpent(t *testing.T) {
	r := newCoreRig(t)
	r.healthy(r.plan(1, until(time.Minute), until(time.Minute)))
	forced := forcedClose(forcedClose(r.plan(2, until(time.Minute), until(time.Minute)), 0), 1)
	lookups := 0
	r.answering(func(m string, _ int, req []byte) ([]byte, string, bool) {
		if m == "GetBalancerInfo" && strings.Contains(string(req), "b.slot2") {
			if lookups++; lookups == 1 {
				r.setStart("e2")
			}
		}
		return nil, "", false
	})
	start := r.logged()
	r.pass(forced)
	if slices.Contains(r.since(start), "RemoveInbound") || len(r.destroyed) > 0 {
		t.Fatalf("the pass that met e2 closed: calls %v, destroyed %v", r.since(start), r.destroyed)
	}
	r.answering(nil)
	start = r.logged()
	r.pass(forced)
	r.pass(forced)
	want := []string{fmt.Sprintf("%d %d", r.pid("e2"), flowMark(r.slots[0].Allocation)), fmt.Sprintf("%d %d", r.pid("e2"), flowMark(r.slots[1].Allocation))}
	removals := slices.DeleteFunc(r.since(start), func(m string) bool { return m != "RemoveInbound" })
	if !slices.Equal(r.destroyed, want) || len(removals) != 2 {
		t.Errorf("destroyed %v, want %v; %d removals", r.destroyed, want, len(removals))
	}
}

// W39. A Down slot owes a close, and intake has closed its link. The entry read fails at the unit while
// the core holds marked sockets: nothing is closed or recorded in that pass. The next pass, reading the
// core, closes once with its process.
func TestADownCloseWaitsForAReadOfTheCore(t *testing.T) {
	r := newCoreRig(t)
	r.healthy(r.plan(1, Passage{}, Passage{}))
	down := forcedClose(r.plan(2, Passage{}, Passage{}, domain.Closed(domain.ConfirmedDead)), 0)
	r.failUnitReads(1)
	r.pass(down)
	if r.up("pxa1") || len(r.destroyed) > 0 {
		t.Fatalf("the unread pass: pxa1 up %t, destroyed %v", r.up("pxa1"), r.destroyed)
	}
	r.pass(down)
	r.pass(down)
	if want := []string{fmt.Sprintf("%d %d", r.pid("e1"), flowMark(r.slots[0].Allocation))}; !slices.Equal(r.destroyed, want) {
		t.Errorf("destroyed %v, want %v", r.destroyed, want)
	}
}
