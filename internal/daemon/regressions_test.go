package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// settledGoroutines waits for goroutines that have finished their work to exit.
func settledGoroutines(limit int) int {
	n := runtime.NumGoroutine()
	for deadline := time.Now().Add(2 * time.Second); n > limit && time.Now().Before(deadline); n = runtime.NumGoroutine() {
		time.Sleep(50 * time.Millisecond)
	}
	return n
}

func TestRoundChurnLeaksNoGoroutines(t *testing.T) {
	b := newRoundBench(t)
	before := runtime.NumGoroutine()
	for i := range 400 {
		q := failureRound(domain.CampaignID(fmt.Sprintf("c%d", i)), "a", "b", "c")
		b.now = b.now.Add(time.Minute)
		q.Due = b.now
		b.adopt(q)
		for range 3 {
			<-b.dials
		}
		b.adopt()
		if r := <-b.rounds; b.s.finished(r) {
			t.Fatal("a withdrawn round was kept")
		}
	}
	if after := settledGoroutines(before + 5); after > before+5 {
		t.Errorf("goroutines grew from %d to %d", before, after)
	}
}

func TestProbesLeakNoGoroutines(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer good.Close()
	s := testLoop(t)
	s.dial = func(_ gateway.ProbeRoute, ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, good.Listener.Addr().String())
	}
	target := probeTarget{ID: "t1", URL: "http://probe.invalid/"}
	route := gateway.ProbeRoute{Server: ref("a"), Epoch: "e1"}
	before := runtime.NumGoroutine()
	for range 300 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if m := s.measure(ctx, route, target, pingRequests); m.err != nil {
			t.Fatal(m.err)
		}
		cancel()
	}
	good.CloseClientConnections()
	if after := settledGoroutines(before + 5); after > before+5 {
		t.Errorf("goroutines grew from %d to %d", before, after)
	}
}

func TestEvidenceAndLoopMapsStayBoundedOverRotations(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), lineage(t, "b", "good", "DE", "198.51.100.20"))
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", elected(t, "good"))
	s := testLoop(t)
	for i := range 2000 {
		l, _ := r.c.world.catalogue.Lookup("b")
		l.Ref.Digest[4], l.Ref.Digest[5] = byte(i), byte(i>>8)
		r.storeLineages(transaction{Lineages: []domain.Lineage{l}})
		r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
		r.clock = r.clock.Add(time.Second)
		p, _ := r.c.observation()
		p.Topology = gateway.Topology{Epoch: "e1", Probes: []gateway.ProbeRoute{{Server: r.ref("a"), Epoch: "e1"}, {Server: r.ref("b"), Epoch: "e1"}}}
		p.Targets = []probeTarget{{ID: "t1", URL: "http://probe.invalid/"}}
		s.adopt(context.Background(), p)
		s.targetTurn[r.ref("b")] = 1
	}
	if len(r.c.evidence.servers) > 4 || len(s.routeStops) > 4 || len(s.nextBeat) > 4 || len(s.targetTurn) > 4 {
		t.Errorf("a map grew with rotations: evidence=%d routeStops=%d nextBeat=%d targetTurn=%d", len(r.c.evidence.servers), len(s.routeStops), len(s.nextBeat), len(s.targetTurn))
	}
}

func TestTimelineASilentServerOnAListMovesInTime(t *testing.T) {
	l, _ := silentOnAList(t, fourServers)
	if took := l.runUntil(failover+5*time.Second, "move", l.moved); took >= failover {
		t.Fatalf("moved after %s, limit %s", took, failover)
	}
}

func TestABlockedSlotProbesAndReopensNothing(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.r.seed(lineage(t, "origin", "outside", "", "198.51.100.200"))
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	l.heartbeatJustBeat()
	l.set(current.Lineage, silent(0))
	l.runUntil(failover, "failure campaign", func() bool { return campaignOf(l) != nil })
	row := l.r.c.state.slots["eu"]
	intent, err := domain.NewIntent("EU", domain.BlockExit())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: row.Slot.Version, Intent: intent}); err != nil {
		t.Fatal(err)
	}
	l.r.settle()
	p, _ := l.r.c.observation()
	if _, sourced := p.Sources[l.r.ref("origin")]; len(p.Rounds) != 0 || len(p.Beats) != 0 || sourced || !l.r.c.state.slots["eu"].Selection.IsBlocked() {
		t.Fatalf("a blocked slot still watches: %+v", p)
	}
	l.moves = nil
	l.run(10 * time.Minute)
	for _, m := range l.moves {
		if _, serving := m.to.Server(); serving {
			t.Fatalf("a blocked slot reopened: %+v", m)
		}
	}
	l.restart()
	l.run(time.Minute)
	if !l.r.c.state.slots["eu"].Selection.IsBlocked() {
		t.Fatal("a blocked slot reopened after a restart")
	}
}

func TestABridgesDeadlineSurvivesARotationAnEditAndARestart(t *testing.T) {
	l := newTimeline(t, fourServers)
	rest := l.otherServers()
	l.silence(rest)
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	l.exitTo(rest...)
	deadline := l.leaveBy()
	l.run(8 * time.Second)
	rotated, _ := l.r.c.world.catalogue.Lookup(current.Lineage)
	rotated.Ref.Digest[5] = 0xaa
	rotated.Seen = nil
	l.r.storeLineages(transaction{Lineages: []domain.Lineage{rotated}})
	l.r.settle()
	l.run(4 * time.Second)
	l.exitTo(rest[0])
	l.run(2 * time.Second)
	if !l.leaveBy().Equal(deadline) {
		t.Fatalf("a rotation or an edit renewed the deadline: %v, was %v", l.leaveBy(), deadline)
	}
	l.restart()
	if target := l.r.targetOf("eu"); !bridgesUntil(target, deadline) {
		t.Fatalf("passage %+v, deadline %v", target.Passage, deadline)
	}
	l.runUntil(failover, "closure", l.closed)
	if late := l.now().Sub(deadline); late < 0 || late > evaluateEvery+timelineStep {
		t.Fatalf("closed %s from the deadline", late)
	}
}

func TestDurableMemorySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	electA(t, r)
	m := r.memory("eu")
	m.LeaveBy = r.clock.Add(10 * time.Second)
	m.KeepOut = map[domain.LineageID]time.Time{"b": r.clock.Add(time.Minute)}
	owner, _ := ownerOf(r.c.state.slots["eu"])
	if err := r.c.store.commit(context.Background(), transaction{Owners: map[domain.Owner]policyMemory{owner: m}}); err != nil {
		t.Fatal(err)
	}
	row := r.c.state.slots["eu"]
	clock := r.clock
	r.close()
	after := newRigAt(t, dir, clock)
	got := after.memory("eu")
	if got.Selection != m.Selection || got.LastServed != m.LastServed || !got.LeaveBy.Equal(m.LeaveBy) || !got.KeepOut["b"].Equal(m.KeepOut["b"]) {
		t.Fatalf("memory changed across a restart: %+v", got)
	}
	if carried := after.c.state.slots["eu"]; carried.Selection != row.Selection || carried.LastServed != row.LastServed {
		t.Fatalf("what the slot carries changed across a restart: %+v", carried)
	}
}

func TestAGroupEditReachesOnlyItsSlots(t *testing.T) {
	r := newRig(t, t.TempDir())
	twoSlotsOnOneGroup(t, r)
	r.putPicked("other", "Other", "c", "d")
	r.putSlot("three", exitIntent(t, "Three", domain.GroupExit("other")))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20), r.probe("c", domain.Succeeded, 30), r.probe("d", domain.Succeeded, 40))
	r.reportApplied("three")
	on := r.server("three")
	versions := map[domain.SlotID]domain.IntentRevision{}
	for id, row := range r.c.state.slots {
		versions[id] = row.Slot.Version
	}
	r.putPicked("g", "Group", "b", "c", "d")
	r.settle()
	if r.server("one") == "a" || r.server("two") != "b" || r.server("three") != on {
		t.Fatalf("one=%s two=%s three=%s", r.server("one"), r.server("two"), r.server("three"))
	}
	for id, row := range r.c.state.slots {
		if row.Slot.Version != versions[id] {
			t.Fatalf("slot %s moved version", id)
		}
	}
}

func TestIdentitySurvivesAReorderedRotatedPaste(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	feed := func(password string, order ...int) string {
		out := "["
		for n, i := range order {
			if n > 0 {
				out += ","
			}
			out += fmt.Sprintf(`{"remarks":"Renamed%s %c","outbounds":[{"tag":"first","protocol":"trojan","settings":{"address":"h%d.example.net","port":443,"password":%q}},{"tag":"ORIGIN-%d","protocol":"trojan","settings":{"address":"o%d.example.net","port":443,"password":%q}}]}`,
				password, rune('A'+i), i, password, i, i, password)
		}
		return out + "]"
	}
	put := func(text string) {
		t.Helper()
		doc, err := catalogue.Parse([]byte(text), catalogue.Detect, catalogue.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		var expected domain.IntentRevision
		if s, ok := r.c.state.sources["src"]; ok {
			expected = domain.IntentRevision(s.Revision)
		}
		if _, err := r.c.accept(ctx, PutSource{ID: "src", Expected: expected, Input: SourceInput{Name: "Src", Manual: &ManualInput{Text: domain.NewSecret(text), Document: &doc}}}); err != nil {
			t.Fatal(err)
		}
		r.settle()
	}
	put(feed("one", 0, 1, 2))
	snapshot := func() map[domain.LineageID]string {
		out := map[domain.LineageID]string{}
		for _, l := range r.c.world.catalogue.Source("src") {
			out[l.ID] = fmt.Sprintf("present=%v", l.Presence == domain.Present)
		}
		return out
	}
	before := snapshot()
	if len(before) != 6 {
		t.Fatalf("lineages %v, want the 6 servers", before)
	}
	put(feed("two", 2, 0, 1))
	if after := snapshot(); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("a reordered, renamed, rotated paste changed identity: %v, then %v", before, after)
	}
}
