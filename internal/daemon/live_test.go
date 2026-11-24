package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// These run the real controller and the real signal loop in real time, with real probes over local
// sockets and a scripted Worker. Each takes over a minute, since a heartbeat's 60 s and a silent
// server's 4 s rounds are constants, and they share that time by running in parallel. Their servers
// were introduced long ago, so the introduction asks nothing of them.

// live is a daemon whose servers are local sockets: a good one answers, and a dead one accepts and
// goes silent, which is the worst case for how long a failure takes to see.
type live struct {
	t            *testing.T
	r            *rig
	mu           sync.Mutex
	dead         map[domain.LineageID]bool
	hole         net.Listener
	good         *httptest.Server
	reports      chan gateway.Report
	plan         atomic.Pointer[gateway.Plan]
	beat         atomic.Int64   // When a's last heartbeat, or any other probe over TCP, dialled it.
	deadProbes   chan time.Time // When each probe over TCP dialled a after its death, the first being the one that finds it.
	baselineDown atomic.Bool
}

// slowTest keeps real waits out of the quick check, which is -short. The full run keeps them.
func slowTest(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("real waits: the full, non-short run covers this")
	}
}

func newLive(t *testing.T, ids ...string) *live {
	t.Helper()
	slowTest(t)
	hole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hole.Close() })
	go func() {
		for {
			conn, err := hole.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(good.Close)
	l := &live{t: t, r: newRigAt(t, t.TempDir(), time.Now()), dead: map[domain.LineageID]bool{}, hole: hole, good: good, reports: make(chan gateway.Report, 8), deadProbes: make(chan time.Time, 64)}
	var rows []domain.Lineage
	for i, id := range ids {
		rows = append(rows, lineage(t, id, "good", "DE", fmt.Sprintf("198.51.100.%d", i+1)))
	}
	l.r.seed(rows...)
	l.r.c.evidence.epoch = "e1"
	for i, id := range ids {
		l.r.c.evidence.book.put(domain.LineageID(id), introduced(time.Now(), 10*(i+1)), true)
	}
	return l
}

func (l *live) start() {
	ctx, cancel := context.WithCancel(context.Background())
	c := l.r.c
	c.now = time.Now
	base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.baselineDown.Load() {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(204)
	}))
	l.t.Cleanup(base.Close)
	s := newSignalLoop(l.r.writer, []probeTarget{{ID: "b1", URL: base.URL}}, "/nonexistent")
	s.dump = func([]uint8) ([]tcpSocket, error) { return nil, nil }
	s.socketsOf = func(int) (map[uint32]bool, error) { return nil, nil }
	s.dial = func(route gateway.ProbeRoute, ctx context.Context, network, _ string) (net.Conn, error) {
		l.mu.Lock()
		dead := l.dead[route.Server.Lineage]
		l.mu.Unlock()
		if route.Server.Lineage == "a" && network == "tcp" {
			if dead {
				select {
				case l.deadProbes <- time.Now():
				default:
				}
			} else {
				l.beat.Store(time.Now().UnixNano())
			}
		}
		address := l.good.Listener.Addr().String()
		if dead {
			address = l.hole.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	frames := make(chan signalFrame, 64)
	var wg sync.WaitGroup
	wg.Go(func() { _ = s.run(ctx, c.plans, make(chan manualProbe), make(chan gateway.Telemetry), frames) })
	wg.Go(func() { l.worker(ctx, c.goals) })
	wg.Go(func() { _ = c.run(ctx, make(chan envelope), make(chan catalogue.Refresh), frames, l.reports) })
	l.t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

func goodReport(p gateway.Plan) gateway.Report {
	topology := gateway.Topology{Epoch: "e1", CorePID: 1, Generation: p.Generation()}
	for _, ref := range p.Servers() {
		topology.Probes = append(topology.Probes, gateway.ProbeRoute{Server: ref, Epoch: "e1", Generation: p.Generation()})
	}
	report := gateway.Report{Generation: p.Generation(), At: time.Now(), Topology: topology}
	for _, target := range p.Slots() {
		phase := gateway.Ready
		if _, down := target.Selection.Down(); down {
			phase = gateway.Closed
		}
		report.Slots = append(report.Slots, gateway.SlotReport{Slot: target.Slot.ID, Phase: phase, Applied: target.Selection, Operation: target.Operation, CoreEpoch: "e1"})
	}
	return report
}

// worker answers each plan after the lab's 50 ms, and reads back every 10 s, as a Worker does.
func (l *live) worker(ctx context.Context, goals <-chan gateway.Plan) {
	poll := time.NewTicker(10 * time.Second)
	defer poll.Stop()
	send := func(r gateway.Report) {
		select {
		case l.reports <- r:
		case <-ctx.Done():
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-goals:
			l.plan.Store(&p)
			time.Sleep(50 * time.Millisecond)
			send(goodReport(p))
		case <-poll.C:
			if p := l.plan.Load(); p != nil {
				send(goodReport(*p))
			}
		}
	}
}

func (l *live) serving() map[domain.SlotID]domain.LineageID {
	out := map[domain.SlotID]domain.LineageID{}
	for _, v := range l.r.app.Snapshot().Slots {
		if v.Server != nil && !v.Pending {
			out[v.ID] = *v.Server
		}
	}
	return out
}

func (l *live) waitAll(slots int, on domain.LineageID) {
	l.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		n := 0
		got := l.serving()
		for _, server := range got {
			if server == on {
				n++
			}
		}
		if n == slots {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("not all on %s: %v", on, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killJustAfterABeat kills a just after a dial over TCP, so the next heartbeat is as far off as it gets.
// A UDP check isn't one: it comes at a moment fixed by a's name, and a heartbeat can follow it at once.
func (l *live) killJustAfterABeat() time.Time {
	seen := l.beat.Load()
	for l.beat.Load() == seen {
		time.Sleep(5 * time.Millisecond)
	}
	l.mu.Lock()
	l.dead["a"] = true
	l.mu.Unlock()
	return time.Now()
}

// probedDead waits for the next probe over TCP of a since its death, and says when it dialled.
func (l *live) probedDead() time.Time {
	l.t.Helper()
	select {
	case at := <-l.deadProbes:
		return at
	case <-time.After(heartbeatServing + 5*time.Second):
		l.t.Fatal("nothing probed a since its death")
		return time.Time{}
	}
}

// wantMoved waits until every slot has left a, and fails any that took the whole failover.
func (l *live) wantMoved(died time.Time, slots ...domain.SlotID) {
	l.t.Helper()
	l.wantMovedWithin(died, failover, slots...)
}

// wantMovedWithin times each move by the landing the controller committed when it applied it, so a
// test goroutine that looks late doesn't count against the daemon.
func (l *live) wantMovedWithin(since time.Time, limit time.Duration, slots ...domain.SlotID) map[domain.SlotID]domain.LineageID {
	l.t.Helper()
	deadline := since.Add(limit + time.Minute)
	for {
		serving, n := l.serving(), 0
		for _, slot := range slots {
			if server, ok := serving[slot]; ok && server != "a" {
				n++
			}
		}
		if n == len(slots) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	landings, err := landingsSince(context.Background(), l.r.c.store.writer, since)
	if err != nil {
		l.t.Fatal(err)
	}
	moves := movesOff(landings, "a", since)
	to := map[domain.SlotID]domain.LineageID{}
	for _, slot := range slots {
		move, ok := moves[slot]
		if ok {
			to[slot] = move.on
			l.t.Logf("measured: slot %s landed on %s after %s (limit %s)", slot, move.on, move.after.Round(time.Millisecond), limit)
		}
		if !ok || move.after >= limit {
			l.t.Errorf("slot %s: moved %v after %s, limit %s", slot, ok, move.after, limit)
		}
	}
	return to
}

type serverMove struct {
	on    domain.LineageID
	after time.Duration
}

// movesOff finds each slot's first landing since on a server other than from. A closure lands on no
// server, so it is no move.
func movesOff(landings []landing, from domain.LineageID, since time.Time) map[domain.SlotID]serverMove {
	moves := map[domain.SlotID]serverMove{}
	for _, landed := range landings {
		if landed.Move == nil || landed.At.Before(since.Truncate(time.Millisecond)) {
			continue
		}
		server, ok := landed.Move.To.Server()
		if _, done := moves[landed.Slot]; !done && ok && server.Lineage != from {
			moves[landed.Slot] = serverMove{on: server.Lineage, after: landed.At.Sub(since)}
		}
	}
	return moves
}

func TestAMoveIsALandingOnAServer(t *testing.T) {
	since := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	to := func(sel domain.Selection) *domain.Switch { return &domain.Switch{To: sel} }
	onB := to(domain.Serving(domain.ServerRef{Lineage: "b"}))
	moves := movesOff([]landing{
		{Slot: "before", Move: onB, At: since.Add(-time.Second)},
		{Slot: "closed", Move: to(domain.Closed(domain.ConfirmedDead)), At: since.Add(time.Second)},
		{Slot: "reopened", Move: to(domain.Closed(domain.ConfirmedDead)), At: since.Add(time.Second)},
		{Slot: "reopened", Move: onB, At: since.Add(5 * time.Second)},
		{Slot: "moved", Move: to(domain.Serving(domain.ServerRef{Lineage: "a"})), At: since.Add(time.Second)},
		{Slot: "moved", Move: onB, At: since.Add(2 * time.Second)},
		{Slot: "moved", Move: to(domain.Serving(domain.ServerRef{Lineage: "c"})), At: since.Add(3 * time.Second)},
	}, "a", since)
	want := map[domain.SlotID]serverMove{"reopened": {on: "b", after: 5 * time.Second}, "moved": {on: "b", after: 2 * time.Second}}
	if !reflect.DeepEqual(moves, want) {
		t.Errorf("moves %v, want %v", moves, want)
	}
}

func TestLiveASilentServerIsReplacedWithin30s(t *testing.T) {
	t.Parallel()
	l := newLive(t, "a", "b", "c", "d")
	l.r.putPicked("g1", "One", "a", "b", "c", "d")
	l.r.putSlot("one", exitIntent(t, "One", domain.GroupExit("g1")))
	l.start()
	l.waitAll(1, "a")
	l.wantMoved(l.killJustAfterABeat(), "one")
}

// Twelve servers go silent together over two probe targets. Failures every server shares blame no
// target, so the rounds close the slot, and nothing it heard before the silence reopens it. The one
// server that answers again is found by the sweep.
func TestLiveAGroupThatGoesSilentClosesAndFindsTheOneThatAnswers(t *testing.T) {
	t.Parallel()
	var ids []string
	var members []domain.LineageID
	for i := range 12 {
		ids = append(ids, fmt.Sprintf("s%02d", i))
		members = append(members, domain.LineageID(ids[i]))
	}
	l := newLive(t, ids...)
	l.r.putPicked("g1", "One", members...)
	l.r.putSlot("one", exitIntent(t, "One", domain.GroupExit("g1")))
	l.r.c.targets = []probeTarget{{ID: "t1", URL: "http://t1.invalid/"}, {ID: "t2", URL: "http://t2.invalid/"}}
	l.start()
	deadline := time.Now().Add(30 * time.Second)
	for l.serving()["one"] == "" {
		if time.Now().After(deadline) {
			t.Fatal("never elected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(25 * time.Second)
	setDead := func(dead bool, which ...string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, id := range which {
			l.dead[domain.LineageID(id)] = dead
		}
	}
	setDead(true, ids...)
	died := time.Now()
	for l.serving()["one"] != "" {
		if time.Since(died) > failover {
			t.Fatalf("still serving %s %s after the whole group went silent", l.serving()["one"], time.Since(died))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("measured: closed after %s", time.Since(died).Round(100*time.Millisecond))
	for range 1500 {
		if server := l.serving()["one"]; server != "" {
			t.Fatalf("reopened on silent %s", server)
		}
		time.Sleep(20 * time.Millisecond)
	}
	setDead(false, ids[len(ids)-1])
	back := time.Now()
	// The sweep fails every member before it answers again, so it needs the sweep to reach it and then
	// its recovery's spaced passes.
	limit := heartbeatSweep + 12*probeTimeout/time.Duration(budget.Parallel) + (recoverPasses-1)*recoverSpacing + 10*time.Second
	for l.serving()["one"] != domain.LineageID(ids[len(ids)-1]) {
		if time.Since(back) > limit {
			t.Fatalf("not on the one server that answers after %s: %v", time.Since(back), l.serving())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("measured: reopened %s after it answered", time.Since(back).Round(100*time.Millisecond))
}

func TestLiveThreeGroupsOnOneDeadServerAreOneFailure(t *testing.T) {
	t.Parallel()
	l := newLive(t, "a", "b", "c", "d", "e")
	l.r.putPicked("g1", "One", "a", "b", "c", "d")
	l.r.putPicked("g2", "Two", "a", "b", "c", "e")
	l.r.putPicked("g3", "Three", "a", "b", "d", "e")
	l.r.putSlot("one", exitIntent(t, "One", domain.GroupExit("g1")))
	l.r.putSlot("two", exitIntent(t, "Two", domain.GroupExit("g2")))
	l.r.putSlot("three", exitIntent(t, "Three", domain.GroupExit("g3")))
	l.start()
	l.waitAll(3, "a")
	l.wantMoved(l.killJustAfterABeat(), "one", "two", "three")
}

// What a Worker sends after one failed read: the last Topology, an error, and nothing read about the
// slots. TestAFailedCoreReadKeepsTheLastTopology holds the Worker to that. The read fails as the first
// round of the confirmation goes out, after the heartbeat that found the death, and the move still
// comes when that heartbeat's rounds give it, to the best of a's rivals.
func TestLiveOneFailedCoreReadCostsAFailoverNothing(t *testing.T) {
	t.Parallel()
	l := newLive(t, "a", "b", "c", "d")
	l.r.putPicked("g1", "One", "a", "b", "c", "d")
	l.r.putSlot("one", exitIntent(t, "One", domain.GroupExit("g1")))
	l.start()
	l.waitAll(1, "a")
	l.killJustAfterABeat()
	found := l.probedDead()
	l.probedDead()
	if l.serving()["one"] != "a" {
		t.Fatalf("moved before the failed read: %v", l.serving())
	}
	p := *l.plan.Load()
	failed := goodReport(p)
	failed.Error = "systemctl show: exit status 1"
	failed.Slots = []gateway.SlotReport{{Slot: "one", Phase: gateway.Ready}}
	select {
	case l.reports <- failed:
	case <-time.After(time.Second):
		t.Fatal("controller stuck")
	}
	// The heartbeat's timeout, then two rounds a second apart, lands at 13 s, and 13.05 s measured. A
	// round more lands a probe timeout later, at 17 s. Half a probe timeout over the nominal leaves the
	// daemon 2 s of delay and still fails that extra round.
	anyway := 3*probeTimeout + defaultTimings().FailureSpacing
	if to := l.wantMovedWithin(found, anyway+probeTimeout/2, "one"); to["one"] != "b" {
		t.Errorf("moved to %s, not to b, the best of a's rivals", to["one"])
	}
}

// The one baseline target stops answering for 5 s while the rivals of the round go on passing through
// the same WAN. It lands on the heartbeat that finds the death, on either round, or between them,
// wherever the 15 s baseline tick falls. The tests named TestTimelineABaselineBlip and
// TestARivalPassingInTheRoundProvesTheWANThroughABaselineBlip hold each part of this without the clock.
func TestLiveABaselineBlipWhileRivalsPassCostsAFailoverNothing(t *testing.T) {
	t.Parallel()
	for _, offset := range []time.Duration{0, 2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second} {
		t.Run(fmt.Sprintf("%s after the heartbeat that finds the death", offset), func(t *testing.T) {
			t.Parallel()
			l := newLive(t, "a", "b", "c", "d")
			l.r.putPicked("g1", "One", "a", "b", "c", "d")
			l.r.putSlot("one", exitIntent(t, "One", domain.GroupExit("g1")))
			l.start()
			l.waitAll(1, "a")
			died := l.killJustAfterABeat()
			time.Sleep(time.Until(l.probedDead().Add(offset)))
			l.baselineDown.Store(true)
			time.Sleep(5 * time.Second)
			l.baselineDown.Store(false)
			l.wantMoved(died, "one")
		})
	}
}
