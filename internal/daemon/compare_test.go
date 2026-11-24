package daemon

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

func (l *timeline) comparisons() int {
	n := 0
	for _, q := range l.rounds {
		if q.Path == improvePath {
			n++
		}
	}
	return n
}

// improvementOn starts the timeline on a, fastest on record, and then makes it as fast as aMs while
// the others answer as their pings say.
func improvementOn(t *testing.T, aMs int, others map[string]int) *timeline {
	t.Helper()
	pings := map[string]int{"a": 10}
	for id := range others {
		pings[id] = 20
	}
	l := newTimeline(t, pings)
	if l.r.server("eu") != "a" {
		t.Fatalf("serving %s, want a", l.r.server("eu"))
	}
	l.stride = time.Second
	l.set("a", healthy(aMs))
	for id, ms := range others {
		l.set(domain.LineageID(id), healthy(ms))
	}
	return l
}

// Every half hour the group compares a with the next two, and b and c beat it each time. Their third
// win in a row moves the group, to b, the better ranked of the two.
func TestAGroupMovesOnThreeConsecutiveWins(t *testing.T) {
	l := improvementOn(t, 100, map[string]int{"b": 67, "c": 70})
	l.runUntil(2*time.Hour, "a move", l.moved)
	if to, _ := l.moves[0].to.Server(); to.Lineage != "b" || l.moves[0].reason != "improvementMove" {
		t.Fatalf("moved to %s for %s", to.Lineage, l.moves[0].reason)
	}
	if got := l.comparisons(); got != 3 {
		t.Fatalf("moved after %d comparisons, want 3", got)
	}
	moved := l.now()
	l.moves = nil
	l.set("a", healthy(20))
	l.run(75 * time.Minute)
	if l.moved() {
		t.Fatalf("moved again %s after the last move, with every streak starting over: %+v", l.moves[0].at.Sub(moved), l.moves[0])
	}
}

// alternating is fast in one comparison round and slow in the next, starting fast or not.
func alternating(fastMs, slowMs int, fast bool) behaviour {
	return func(purpose domain.ProbePurpose) reply {
		if purpose != domain.Confirmation {
			return healthy(fastMs)(purpose)
		}
		fast = !fast
		if fast {
			return healthy(slowMs)(purpose)
		}
		return healthy(fastMs)(purpose)
	}
}

// b and c take turns beating a, so neither wins two in a row and the group never moves.
func TestTwoRivalsAlternatingWinsNeverMoveTheGroup(t *testing.T) {
	l := improvementOn(t, 100, map[string]int{"b": 67, "c": 70})
	l.set("b", alternating(60, 100, true))
	l.set("c", alternating(60, 100, false))
	l.run(6 * time.Hour)
	if l.moved() {
		t.Fatalf("moved: %+v", l.moves[0])
	}
	if got := l.comparisons(); got < 11 || got > 12 {
		t.Fatalf("%d comparisons in six hours, want one every half hour", got)
	}
}

// A win needs the rival 20% faster and 20 ms faster, both.
func TestAComparisonWinNeedsBothMargins(t *testing.T) {
	cases := []struct {
		name           string
		current, rival int
		moves          bool
	}{
		{"a fifth faster but 15 ms", 80, 65, false},
		{"40 ms faster but an eighth", 300, 260, false},
		{"30 ms and 30 percent faster", 100, 70, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := improvementOn(t, c.current, map[string]int{"b": c.rival})
			l.run(4 * compareEvery)
			if l.moved() != c.moves {
				t.Fatalf("%d ms against %d ms: moved %v, want %v", c.current, c.rival, l.moved(), c.moves)
			}
		})
	}
}

// After two wins a restart starts the streak over, so the group moves on the third win after it.
func TestARestartMidComparisonStartsTheStreakOver(t *testing.T) {
	l := improvementOn(t, 100, map[string]int{"b": 60})
	l.runUntil(3*compareEvery, "two wins", func() bool { return l.comparisons() == 2 && l.r.memory("eu").Campaign == nil })
	l.restart()
	l.stride = time.Second
	before := l.comparisons()
	l.runUntil(4*compareEvery, "a move", l.moved)
	if got := l.comparisons() - before; got != 3 {
		t.Fatalf("moved after %d comparisons since the restart, want 3", got)
	}
}

// A change of ranking site voids a comparison round that hasn't answered: it leaves the plan, and the
// streaks it would have added to stay as they were.
func TestARankingSiteChangeVoidsAComparisonInFlight(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.addTarget("t2")
	c := l.r.improving("eu", l.r.ref("b"))
	l.r.c.ingest(signalFrame{Ranking: "t2", WAN: wanBelief{State: domain.WANUp, At: l.now()}})
	l.r.settle()
	m := l.r.memory("eu")
	p, _ := l.r.c.observation()
	if m.Campaign != nil || slices.ContainsFunc(p.Rounds, func(q roundRequest) bool { return q.Campaign == c.ID }) {
		t.Fatalf("the comparison survived the change of site: %+v", m.Campaign)
	}
	if m.Streaks["b"] != compareWins-1 {
		t.Fatalf("streaks %v after a void round", m.Streaks)
	}
	if _, refilling := p.Checks[l.r.ref("b")]; !refilling {
		t.Fatalf("b has no quick probe against the new site: %v", p.Checks)
	}
}

// A slot on a server Exit never switches: no comparison, no rival, and when the server dies the slot
// closes until it answers again.
func TestAServerExitNeverSwitches(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.r.setExit("eu", domain.ServerExit("d"))
	l.stride = 200 * time.Millisecond
	l.runUntil(time.Minute, "d", func() bool { return l.r.server("eu") == "d" && l.r.operation("eu") == "" })
	l.moves = nil
	l.set("d", healthy(400))
	l.run(2 * time.Hour)
	if l.moved() || l.comparisons() != 0 {
		t.Fatalf("a slow server Exit moved %+v after %d comparisons", l.moves, l.comparisons())
	}
	l.set("d", silent(0))
	l.runUntil(failover, "closure", l.closed)
	l.set("d", healthy(72))
	l.runUntil(5*time.Minute, "reopening", func() bool { return l.r.server("eu") == "d" })
	for _, m := range l.moves {
		if to, serving := m.to.Server(); serving && to.Lineage != "d" {
			t.Fatalf("a server Exit moved to %s", to.Lineage)
		}
	}
}

// The plan's Cost table for a new 12-server feed in two groups of six, one slot on each, with s05 dead
// from its second introduction step. Over the introduction's two days the 11 others have their 11
// checks each, 121, and s05 has 17: the passing first step, the failing second at 2 min, 9 ladder steps
// that double from 1 min to 256 min and end at 8 h 33 min, and then one every 6 h until hour 49, 6 more.
// On a day after it: 4 checks, s05's every 6 h; 288 comparison probes, 2 groups at 48 rounds a day of
// the current server and the next 2, which s05, ranked last, never is; 1,440 heartbeats for each of the
// 2 serving servers; and no failure round.
func TestTheDailyProbeCountOfTwelveServersInTwoGroups(t *testing.T) {
	pings := map[string]int{}
	var first, second []domain.LineageID
	for i := range 12 {
		id := fmt.Sprintf("s%02d", i)
		pings[id] = 60 + i%6
		if i < 6 {
			first = append(first, domain.LineageID(id))
		} else {
			second = append(second, domain.LineageID(id))
		}
	}
	l := freshTimeline(t, pings)
	l.r.putPicked("one", "One", first...)
	l.r.putPicked("two", "Two", second...)
	l.r.setExit("eu", domain.GroupExit("one"))
	l.r.putSlot("other", exitIntent(t, "Other", domain.GroupExit("two")))
	l.stride = time.Second
	l.set("s05", killed())
	l.run(49 * time.Hour)
	if got := l.total("check"); got != 138 {
		t.Errorf("%d checks in the introduction's two days, want 138", got)
	}
	day := map[string]int{}
	for kind := range l.count {
		day[kind] = l.total(kind)
	}
	l.run(24 * time.Hour)
	for kind := range l.count {
		day[kind] = l.total(kind) - day[kind]
	}
	t.Logf("calculated: probes on a day after the introduction, by kind: %v", day)
	if day["check"] != 4 || day["comparison"] != 288 || day["heartbeat"] != 2*1440 || day["failure round"] != 0 {
		t.Fatalf("a day's probes %v, want 4 checks, 288 comparison probes and 1440 heartbeats for each of 2 serving servers", day)
	}
	if l.moved() && len(l.moves) > 2 {
		t.Logf("moves: %+v", l.moves)
	}
}

// An unverified member is in no comparison, however fast it answers. Its ladder's next retry, 64 min
// after the sixth, finds it working and starts its recovery.
func TestAnUnverifiedMemberStaysOutOfComparisonsUntilItsLadderBringsItBack(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.stride = time.Second
	l.failedOnce("d")
	l.run(65 * time.Minute)
	if !unverified(l.r.c.evidence.book.records["d"]) {
		t.Fatal("d is still on its first six steps")
	}
	l.set("d", healthy(5))
	before, comparisons := l.count["comparison"]["d"], l.comparisons()
	l.runUntil(65*time.Minute, "d's retry passing", func() bool { return l.standing("d") == recovering })
	if l.count["comparison"]["d"] != before || l.comparisons() == comparisons || l.moved() {
		t.Fatalf("d was in %d of %d comparisons while unverified, moves %+v", l.count["comparison"]["d"]-before, l.comparisons()-comparisons, l.moves)
	}
	l.runUntil(2*time.Minute, "d's recovery", func() bool { return l.standing("d") == passing })
}

// A rival wins at four fifths of the current server's Ping times its factor and 20 ms below it, both
// at once and each exactly.
func TestTheComparisonMarginIsAFifthAndTwentyMilliseconds(t *testing.T) {
	for _, c := range []struct {
		mine, theirs time.Duration
		want         bool
	}{
		{250 * time.Millisecond, 200 * time.Millisecond, true},
		{250 * time.Millisecond, 200*time.Millisecond + time.Microsecond, false},
		{100 * time.Millisecond, 80 * time.Millisecond, true},
		{100 * time.Millisecond, 80*time.Millisecond + time.Microsecond, false},
		{50 * time.Millisecond, 30 * time.Millisecond, true},
		{50 * time.Millisecond, 30*time.Millisecond + time.Microsecond, false},
		{250 * time.Millisecond, 205 * time.Millisecond, false},
	} {
		if got := beatsByGain(c.mine, c.theirs); got != c.want {
			t.Errorf("%s against %s: %v, want %v", c.mine, c.theirs, got, c.want)
		}
	}
}

// b answers in 205 ms to a's 250, 18% faster with the same record, so it never wins and the group stays.
func TestARivalLessThanAFifthFasterNeverWins(t *testing.T) {
	l := improvementOn(t, 250, map[string]int{"b": 205})
	l.run(4 * compareEvery)
	if l.moved() || len(l.r.memory("eu").Streaks) > 0 {
		t.Fatalf("moved %+v with streaks %v", l.moves, l.r.memory("eu").Streaks)
	}
}

// Two servers at 12 Mbps each are 24 Mbps on the line. Comparisons on that line are void: b, far
// faster, keeps the two wins it had, gains none, and its third, on a quiet line, moves the group.
func TestAComparisonOnAFullLineIsVoid(t *testing.T) {
	l := improvementOn(t, 100, map[string]int{"b": 60})
	rate := 0.0
	l.traffic(func() float64 { return rate })
	l.runUntil(3*compareEvery, "two wins", func() bool { return l.r.memory("eu").Streaks["b"] == 2 })
	rate = 12e6
	l.run(2 * compareEvery)
	if l.moved() || !l.r.c.evidence.lineBusy || l.r.memory("eu").Streaks["b"] != 2 {
		t.Fatalf("on a full line moved %+v with streaks %v", l.moves, l.r.memory("eu").Streaks)
	}
	rate = 0
	l.runUntil(compareEvery+time.Minute, "a move", l.moved)
	if l.moves[0].reason != "improvementMove" {
		t.Fatalf("moved for %s", l.moves[0].reason)
	}
}

// Four of seven members are unverified. A comparison still runs every half hour, and measures the
// current server and the next two alone, none of the four, all charged to the improvement bucket.
func TestAComparisonBesideFourUnverifiedMembersMeasuresThreeAndRuns(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 40, "b": 50, "c": 60, "d": 70, "e": 80, "f": 90, "g": 100})
	l.stride = time.Second
	for _, id := range []domain.LineageID{"d", "e", "f", "g"} {
		l.failedOnce(id)
	}
	l.run(65 * time.Minute)
	before := l.comparisons()
	l.run(compareEvery + time.Minute)
	if got := l.comparisons() - before; got == 0 {
		t.Fatalf("no comparison in %s with four unverified members", compareEvery+time.Minute)
	}
	for _, q := range l.rounds {
		if q.Path == improvePath && (len(q.servers()) != 1+compareRivals || slices.ContainsFunc(q.servers(), func(s domain.ServerRef) bool { return unverified(l.r.record(s.Lineage)) })) {
			t.Fatalf("a comparison measured %v", q.servers())
		}
	}
}
