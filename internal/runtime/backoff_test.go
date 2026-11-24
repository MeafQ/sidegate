package runtime

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

var backoffStart = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

// routingOf names the routing of slot1 on server, beside a slot2 on each of others.
func routingOf(t *testing.T, server string, others ...string) string {
	t.Helper()
	cp := corePlan{Slots: []coreSlot{{ID: "slot1", Selection: domain.Serving(testRef(server))}}}
	for _, o := range others {
		cp.Slots = append(cp.Slots, coreSlot{ID: "slot2", Selection: domain.Serving(testRef(o))})
	}
	return routingKey(cp)
}

func dueNow(b backoffs, now time.Time) bool {
	_, due := b.due("slot1", now)
	return due
}

func TestAFlappingCheckIsRewrittenAboutOnceAMinute(t *testing.T) {
	routing := routingOf(t, "srvA")
	b := backoffs{}
	rewrites := 0
	for now := backoffStart; now.Before(backoffStart.Add(time.Hour)); now = now.Add(10 * time.Second) {
		b.observe("slot1", routing, "core", checkFailed, now)
		if dueNow(b, now) {
			b.rewritten("slot1", "core")
			rewrites++
			b.observe("slot1", routing, "core", checkPassed, now.Add(10*time.Millisecond))
		}
	}
	if rewrites > 70 {
		t.Errorf("%d rewrites in an hour of a check failing every 10 s pass, want about one a minute", rewrites)
	}
	if rewrites < 45 {
		t.Errorf("%d rewrites in an hour: the wait grew past a minute", rewrites)
	}
}

// failRepeatedly lands rewrites whose check then fails until the wait is long.
func failRepeatedly(b backoffs, routing string, now time.Time) {
	for range 5 {
		landAndFail(b, routing, now)
	}
}

func TestBackoffStartsOverOnASwitchOrAfterACalmMinute(t *testing.T) {
	a, c := routingOf(t, "srvA"), routingOf(t, "srvB")
	now := backoffStart
	b := backoffs{}
	failRepeatedly(b, a, now)
	if b.observe("slot1", a, "core", checkFailed, now.Add(time.Second)); dueNow(b, now.Add(time.Second)) {
		t.Fatal("a fifth failed check in a row was rewritten a second later")
	}
	if b.observe("slot1", c, "core", checkFailed, now.Add(time.Second)); !dueNow(b, now.Add(time.Second)) {
		t.Error("a switch waited out the old selection's backoff")
	}

	b = backoffs{}
	failRepeatedly(b, a, now)
	for at := now.Add(20 * time.Second); at.Before(now.Add(2 * time.Minute)); at = at.Add(10 * time.Second) {
		b.observe("slot1", a, "core", checkPassed, at)
	}
	later := now.Add(2 * time.Minute)
	if b.observe("slot1", a, "core", checkFailed, later); !dueNow(b, later) {
		t.Error("after a calm minute the next failure still waited")
	}
	b.rewritten("slot1", "core")
	b.observe("slot1", a, "core", checkFailed, later)
	if !dueNow(b, later.Add(1100*time.Millisecond)) {
		t.Error("after a calm minute the next failure still waited more than a second")
	}
}

func TestAPlanThatChangesOnlyTheRulesIsNeverHeld(t *testing.T) {
	now := backoffStart
	b := backoffs{}
	for i := range 20 {
		b.observe("slot1", routingOf(t, "srvA", fmt.Sprintf("srv%d", i)), "core", checkFailed, now)
		if !dueNow(b, now) {
			t.Fatalf("plan %d, 3 s after the last, had its rewrite held", i)
		}
		b.rewritten("slot1", "core")
		b.observe("slot1", routingOf(t, "srvA", fmt.Sprintf("srv%d", i)), "core", checkPassed, now.Add(10*time.Millisecond))
		now = now.Add(3 * time.Second)
	}
}

func TestAPlanChangeOwesARewriteEvenWhenTheCheckPasses(t *testing.T) {
	b := backoffs{}
	b.observe("slot1", routingOf(t, "srvA"), "core", checkPassed, backoffStart)
	if owed, _ := b.due("slot1", backoffStart); owed {
		t.Fatal("a slot adopted passing its check owes a rewrite")
	}
	b.observe("slot1", routingOf(t, "srvA", "srvB"), "core", checkPassed, backoffStart)
	if !dueNow(b, backoffStart) {
		t.Error("what a rewrite would write changed, and none is owed")
	}
}

func TestOnlyARewriteThatLandedAndThenFailedSpendsTheBackoff(t *testing.T) {
	routing := routingOf(t, "srvA")
	b := backoffs{}
	for at := backoffStart; at.Before(backoffStart.Add(time.Hour)); at = at.Add(10 * time.Second) {
		if b.observe("slot1", routing, "", checkFailed, at); !dueNow(b, at) {
			t.Fatal("a core that never answered held the rewrite, though none landed")
		}
		if b.observe("slot1", routing, "core", checkFailed, at); !dueNow(b, at) {
			t.Fatal("a failed check held the rewrite, though none landed")
		}
	}

	b.rewritten("slot1", "core")
	if b.observe("slot1", routing, "restarted", checkFailed, backoffStart); !dueNow(b, backoffStart) {
		t.Error("a restarted core, which reset the rewrite, counted as its failed check")
	}
	if b.observe("slot1", routing, "core", checkFailed, backoffStart); dueNow(b, backoffStart) {
		t.Error("a failed check on the core the rewrite landed on was not spent")
	}
	if b.observe("slot1", routing, "core", checkFailed, backoffStart.Add(time.Second)); !dueNow(b, backoffStart.Add(time.Second)) {
		t.Error("one failed check held the next rewrite past a second")
	}
	b.observe("slot1", routing, "core", checkFailed, backoffStart.Add(time.Second))
	if !dueNow(b, backoffStart.Add(time.Second)) {
		t.Error("a second failed check with no rewrite since spent the backoff again")
	}
}

func TestReorderingDisjointSlotsKeepsAnActiveHold(t *testing.T) {
	cp := labCorePlan(t)
	routing := routingKey(cp)
	b := backoffs{}
	now := backoffStart
	for range 7 {
		landAndFail(b, routing, now)
		now = b["slot1"].next
	}
	at := now.Add(-59 * time.Second)
	slices.Reverse(cp.Slots)
	if b.observe("slot1", routingKey(cp), "core", checkFailed, at); dueNow(b, at) {
		t.Errorf("slot order alone released a hold with 59 s left, after %d failed checks", b["slot1"].fails)
	}
}

// landAndFail lands a rewrite and fails its check.
func landAndFail(b backoffs, routing string, now time.Time) {
	b.observe("slot1", routing, "core", checkFailed, now)
	b.rewritten("slot1", "core")
	b.observe("slot1", routing, "core", checkFailed, now)
}

func TestAClosedGateOwesOneRewriteAndSpendsNothing(t *testing.T) {
	routing := routingOf(t, "srvA")
	b := backoffs{}
	b.observe("slot1", routing, "core", checkPassed, backoffStart)
	b.gated("slot1", true)
	if b.gated("slot1", false); !dueNow(b, backoffStart) {
		t.Fatal("a gate that closed owes no rewrite, though readback cannot prove routing")
	}
	b.rewritten("slot1", "core")
	for at := backoffStart; at.Before(backoffStart.Add(time.Minute)); at = at.Add(4 * time.Second) {
		b.observe("slot1", routing, "core", checkPassed, at)
		b.gated("slot1", false)
		if owed, _ := b.due("slot1", at); owed {
			t.Fatalf("a gate that stays closed owes another rewrite %s in", at.Sub(backoffStart))
		}
	}
	if b["slot1"].fails != 0 {
		t.Errorf("%d charges though no check failed", b["slot1"].fails)
	}
}
