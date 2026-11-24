package daemon

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// preferOwner is one kind of owner a preference belongs to. A source is an owner exactly as a group is,
// so every test of a preference runs for both.
type preferOwner struct {
	name  string
	exit  domain.Exit
	owner domain.Owner
}

var preferOwners = []preferOwner{
	{"group", domain.GroupExit(sourceGroup("good")), domain.GroupOwner(sourceGroup("good"))},
	{"source", domain.SourceExit("good"), domain.SourceOwner("good")},
}

func eachOwner(t *testing.T, test func(t *testing.T, o preferOwner)) {
	for _, o := range preferOwners {
		t.Run(o.name, func(t *testing.T) { test(t, o) })
	}
}

// preferRig is slot "eu" on an owner of servers a, b and c, checked at 10, 20 and 30 ms, serving a.
type preferRig struct {
	*rig
	o preferOwner
}

func newPreferRig(t *testing.T, o preferOwner) *preferRig {
	t.Helper()
	r := &preferRig{rig: newRig(t, t.TempDir()), o: o}
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), lineage(t, "b", "good", "DE", "198.51.100.20"), lineage(t, "c", "good", "DE", "198.51.100.30"))
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", exitIntent(t, "EU", o.exit))
	r.frame(domain.WANUp, r.check("a", domain.Succeeded, 10), r.check("b", domain.Succeeded, 20), r.check("c", domain.Succeeded, 30))
	if got := r.server("eu"); got != "a" {
		t.Fatalf("elected %v, want a", got)
	}
	r.reportApplied("eu")
	r.clock = r.clock.Add(6 * time.Minute)
	r.settle()
	return r
}

// check is a full probe of a server, which measures a sample of its ping.
func (r *rig) check(id string, verdict domain.Verdict, ms int) domain.Signal {
	s := r.probe(id, verdict, ms)
	s.Purpose = domain.Check
	if verdict == domain.Succeeded {
		ping := time.Duration(ms) * time.Millisecond
		s.Ping = &ping
	}
	return s
}

// order is the owner's order as the view gives it.
func (r *preferRig) order() []domain.LineageID {
	r.t.Helper()
	r.c.publish(r.clock)
	order, _ := r.published()
	return order
}

// published is the owner's order and first choice in the view as last published.
func (r *preferRig) published() ([]domain.LineageID, *domain.LineageID) {
	r.t.Helper()
	v := r.app.Snapshot()
	if id, ok := r.o.owner.Group(); ok {
		g, _ := v.groupByID(id)
		return g.Order, g.FirstChoice
	}
	id, _ := r.o.owner.Source()
	for _, s := range v.Sources {
		if s.ID == id {
			return s.Order, s.FirstChoice
		}
	}
	r.t.Fatalf("no %s in the view", r.o.owner)
	return nil, nil
}

// priceOf is a server's price as its record stands.
func (r *rig) priceOf(id domain.LineageID) float64 {
	p, _ := price(r.c.evidence.book.records[id], r.c.evidence.book.sites[r.c.evidence.ranking], r.clock)
	return p
}

// prefers sets the owner's preference as a press would leave it.
func (r *preferRig) prefers(id domain.LineageID, size float64) {
	m := r.memory("eu")
	m.Hand.at = heldPick{server: id, boost: boost{At: r.clock, Size: size}}
	r.setMemory("eu", m)
}

func wantOrder(t *testing.T, got []domain.LineageID, want ...domain.LineageID) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

func TestTheBoostFadesSlowlyThenFasterToThreeQuartersAtTwelveHoursAndNothingAtADay(t *testing.T) {
	p := boost{At: t0, Size: 20}
	for _, c := range []struct {
		after time.Duration
		want  float64
	}{
		{-time.Hour, 20}, {0, 20}, {6 * time.Hour, 18.75}, {12 * time.Hour, 15}, {18 * time.Hour, 8.75}, {24 * time.Hour, 0}, {30 * time.Hour, 0},
	} {
		if got := p.left(t0.Add(c.after)); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("after %s the boost is %.4f, want %.4f", c.after, got, c.want)
		}
	}
	if got := p.endsAt(); !got.Equal(t0.Add(24 * time.Hour)) {
		t.Errorf("a preference ends at %s, want a day after it began", got)
	}
}

func TestThePreferredServerWinsAnExactTie(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		wantOrder(t, r.order(), "a", "b", "c")
		r.prefers("b", r.priceOf("b")-r.priceOf("a"))
		wantOrder(t, r.order(), "b", "a", "c")
	})
}

func TestAFreshPreferenceIsFirstWhateverItsGapRoundsTo(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := &preferRig{rig: newRig(t, t.TempDir()), o: o}
		r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), lineage(t, "b", "good", "DE", "198.51.100.20"))
		r.c.evidence.epoch = "e1"
		r.putSlot("eu", exitIntent(t, "EU", o.exit))
		a := r.check("a", domain.Succeeded, 10)
		ping := 10*time.Millisecond + time.Microsecond
		a.Ping, a.NewConn = &ping, &ping
		r.frame(domain.WANUp, a, r.check("b", domain.Succeeded, 500))
		r.reportApplied("eu")
		r.clock = r.clock.Add(time.Second)
		r.passes("b", 500)
		wantPreferred(t, r.rig, "b", 0)
		wantOrder(t, r.order(), "b", "a")
	})
}

func TestARecoveringPickRanksUnderPassingOnes(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.frame(domain.WANUp, r.check("c", domain.Failed, 0))
		r.clock = r.clock.Add(time.Minute)
		r.frame(domain.WANUp, r.check("c", domain.Succeeded, 30))
		r.prefers("c", r.priceOf("c"))
		wantOrder(t, r.order(), "a", "b", "c")
	})
}

func TestARecoveredPickRanksByItsBoostedPrice(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.frame(domain.WANUp, r.check("c", domain.Failed, 0))
		for range recoverPasses {
			r.clock = r.clock.Add(recoverSpacing)
			r.frame(domain.WANUp, r.check("c", domain.Succeeded, 30))
		}
		r.prefers("c", r.priceOf("c")-r.priceOf("a"))
		wantOrder(t, r.order(), "c", "a", "b")
		r.clock = r.clock.Add(12 * time.Hour)
		wantOrder(t, r.order(), "a", "c", "b")
	})
}

func TestAKeptOutPreferredServerStillRanksLast(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.prefers("b", r.priceOf("b"))
		m := r.memory("eu")
		m.KeepOut = map[domain.LineageID]time.Time{"b": r.clock.Add(keepOutFor)}
		r.setMemory("eu", m)
		wantOrder(t, r.order(), "a", "c", "b")
	})
}

func TestABoostPastItsServersPriceRanksItFirstAndAnUnpricedRivalUnderIt(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.seed(lineage(t, "d", "good", "DE", "198.51.100.40"))
		r.prefers("c", 10*r.priceOf("c"))
		wantOrder(t, r.order(), "c", "a", "b", "d")
	})
}

func TestAnOwnersRowFromBeforePreferLoads(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		dir := t.TempDir()
		r := newRig(t, dir)
		r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"))
		r.putSlot("eu", exitIntent(t, "EU", o.exit))
		a := refToWire(r.ref("a"))
		row := `{"selection":{"server":{"l":"a","d":"` + a.Digest + `"}},"lastServed":{"l":"a","d":"` + a.Digest + `"},"leaveBy":"0001-01-01T00:00:00Z","closures":1}`
		if _, err := r.c.store.writer.ExecContext(context.Background(), `UPDATE owners SET memory = ? WHERE id = ?`, row, o.owner.String()); err != nil {
			t.Fatal(err)
		}
		r.close()
		again := newRig(t, dir)
		m := again.c.state.owners[o.owner]
		if got, _ := m.Selection.Server(); got != again.ref("a") || m.Backoff.Closures != 1 {
			t.Fatalf("the row loads as %+v", m)
		}
		if pickState(m.Hand) != "none" {
			t.Errorf("a row from before preferences loads with the pick %+v", m.Hand)
		}
	})
}
