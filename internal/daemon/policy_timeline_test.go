package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// Every duration these tests report is calculated: a scripted world on a fake clock, driven through
// the real controller. The control lab measures the real ones.

const (
	timelineStep  = 50 * time.Millisecond
	timelineApply = 50 * time.Millisecond // Measured in the lab, control-2026-11-04.md.
	killedFails   = 500 * time.Millisecond
)

// reply is how one server answers one probe.
type reply struct {
	verdict      domain.Verdict
	excluded     bool // It failed, and the baseline couldn't say the WAN worked meanwhile.
	ping, newCon *time.Duration
	took         time.Duration
}

// behaviour answers a probe. A heartbeat is one request, and anything else is a full probe.
type behaviour func(purpose domain.ProbePurpose) reply

func healthy(pingMs int) behaviour {
	return func(purpose domain.ProbePurpose) reply {
		newConn := 3 * time.Duration(pingMs) * time.Millisecond
		if !asksPing(purpose) {
			return reply{verdict: domain.Succeeded, newCon: &newConn, took: newConn}
		}
		ping := time.Duration(pingMs) * time.Millisecond
		return reply{verdict: domain.Succeeded, ping: &ping, newCon: &newConn, took: newConn + 2*ping}
	}
}

func killed() behaviour {
	return func(domain.ProbePurpose) reply { return reply{verdict: domain.Failed, took: killedFails} }
}

// silent times out at the probe deadline. A heartbeat then waits for its own baseline, and a round
// reads the baseline that ran beside it.
func silent(baseline time.Duration) behaviour {
	return func(purpose domain.ProbePurpose) reply {
		if !asksPing(purpose) {
			return reply{verdict: domain.Failed, took: probeTimeout + baseline}
		}
		return reply{verdict: domain.Failed, took: probeTimeout}
	}
}

// slowed is the lab's 300 ms of added delay: 1530 ms on a new connection, as measured there.
func slowed(pingMs int) behaviour {
	return func(purpose domain.ProbePurpose) reply {
		newConn := 1530 * time.Millisecond
		if !asksPing(purpose) {
			return reply{verdict: domain.Succeeded, newCon: &newConn, took: newConn}
		}
		ping := time.Duration(pingMs+300) * time.Millisecond
		return reply{verdict: domain.Succeeded, ping: &ping, newCon: &newConn, took: 2100 * time.Millisecond}
	}
}

// lossy is the lab's 25% loss: two lucky reused requests show a normal Ping over a new connection
// measured at 1649 to 2305 ms. failFirst makes the first full probe fail outright.
func lossy(pingMs int, failFirst bool) behaviour {
	failed := !failFirst
	return func(purpose domain.ProbePurpose) reply {
		newConn := 2000 * time.Millisecond
		if !asksPing(purpose) {
			return reply{verdict: domain.Succeeded, newCon: &newConn, took: newConn}
		}
		if !failed {
			failed = true
			return reply{verdict: domain.Failed, took: probeTimeout}
		}
		ping := time.Duration(pingMs) * time.Millisecond
		return reply{verdict: domain.Succeeded, ping: &ping, newCon: &newConn, took: 2100 * time.Millisecond}
	}
}

type move struct {
	slot   domain.SlotID
	at     time.Time
	reason string
	to     domain.Selection
	// The server the plan's passage let the core stay on, and until when.
	prior   domain.ServerRef
	until   time.Time
	held    bool
	passage gateway.Passage
}

type arrival struct {
	at    time.Time
	beat  *domain.Signal
	round *roundDone
}

// timeline plays the probes and the Worker around a real controller and the real signal loop's
// scheduling: what starts when, on what budget, with which workers free. Only the probes' answers
// and how long they take are scripted, and a move is reported applied a moment after it is proposed.
type timeline struct {
	t        *testing.T
	r        *rig
	dir      string
	loop     *signalLoop
	servers  map[string]behaviour
	rounds   []roundRequest // Every round a job has run.
	attempts int            // Probes rounds have spent.
	requests int            // Requests every probe has made: a success on a full probe makes three.
	checks   map[string]int // UDP checks each server has had.
	inFlight []arrival
	moves    []move
	lastTick time.Time
	wan      wanBelief // What the signal loop would believe, which the timeline sets itself.
	onStep   func()    // A test's own events, on the clock.
	stride   time.Duration
	count    map[string]map[domain.LineageID]int // Probes started, by kind and server.
	noReuse  map[domain.TargetID]bool            // Targets that pass a full probe without reusing its connection.
}

// newTimeline starts on servers introduced long ago, each with samples at its Ping, so the group is
// in Ping order and the introduction asks nothing.
func newTimeline(t *testing.T, pings map[string]int) *timeline {
	t.Helper()
	return startTimeline(t, pings, true)
}

// freshTimeline starts on servers the catalogue has only just called new.
func freshTimeline(t *testing.T, pings map[string]int) *timeline {
	t.Helper()
	return startTimeline(t, pings, false)
}

// introduced is a record of a server whose introduction ended a day ago, at its Ping on the rig's target.
func introduced(now time.Time, pingMs int) record {
	ping := time.Duration(pingMs) * time.Millisecond
	at := siteOf(probeTarget{URL: "http://target.example/"})
	r := record{FirstSeen: now.Add(-72 * time.Hour), Last: now.Add(-24 * time.Hour), Checked: now.Add(-24 * time.Hour)}
	for i := range len(quickProbes) {
		r.Samples = append(r.Samples, sample{At: r.Last.Add(-time.Duration(i) * time.Hour), Site: at, Ping: ping, NewConn: 3 * ping})
	}
	return r
}

func startTimeline(t *testing.T, pings map[string]int, known bool) *timeline {
	t.Helper()
	dir := t.TempDir()
	l := &timeline{t: t, r: newRig(t, dir), dir: dir, servers: map[string]behaviour{}, checks: map[string]int{}, wan: wanBelief{State: domain.WANUp},
		stride: timelineStep, count: map[string]map[domain.LineageID]int{}, noReuse: map[domain.TargetID]bool{}}
	l.newLoop()
	var rows []domain.Lineage
	for _, id := range slices.Sorted(mapKeys(pings)) {
		rows = append(rows, lineage(t, id, "good", "DE", "198.51.100."+id))
		l.servers[id] = healthy(pings[id])
	}
	l.r.seed(rows...)
	l.r.c.evidence.epoch = "e1"
	if known {
		for id, ms := range pings {
			l.r.c.evidence.book.put(domain.LineageID(id), introduced(l.now(), ms), true)
		}
	}
	intent, err := domain.NewIntent("EU", domain.GroupExit(sourceGroup("good")))
	if err != nil {
		t.Fatal(err)
	}
	l.r.putSlot("eu", intent)
	l.run(25 * time.Second)
	if _, serving := l.r.c.state.slots["eu"].Selection.Server(); !serving {
		t.Fatalf("never elected: %+v", l.r.c.state.slots["eu"].Selection)
	}
	l.moves = nil
	return l
}

func (l *timeline) newLoop() {
	l.loop = newSignalLoop(l.r.writer, nil, "")
	l.loop.now = l.now
}

// restart is a daemon restart at this moment with no downtime.
func (l *timeline) restart() {
	clock := l.now()
	targets := l.r.c.targets
	l.r.close()
	l.r = newRigWith(l.t, l.dir, clock, targets)
	l.r.c.evidence.epoch = "e1"
	l.newLoop()
	l.inFlight, l.lastTick = nil, time.Time{}
}

func (l *timeline) now() time.Time { return l.r.clock }

func (l *timeline) run(d time.Duration) {
	l.t.Helper()
	for end := l.now().Add(d); l.now().Before(end); {
		l.step()
	}
}

// runUntil steps until ok, and says how long that took. It fails past limit.
func (l *timeline) runUntil(limit time.Duration, what string, ok func() bool) time.Duration {
	l.t.Helper()
	start := l.now()
	for !ok() {
		if l.now().Sub(start) > limit {
			l.t.Fatalf("no %s within %s: %+v", what, limit, l.r.memory("eu"))
		}
		l.step()
	}
	return l.now().Sub(start)
}

func (l *timeline) step() {
	l.t.Helper()
	l.r.clock = l.now().Add(l.stride)
	now := l.now()
	var due []arrival
	l.inFlight = slices.DeleteFunc(l.inFlight, func(a arrival) bool {
		if a.at.After(now) {
			return false
		}
		due = append(due, a)
		return true
	})
	for _, a := range due {
		l.deliver(a)
	}
	if now.Sub(l.lastTick) >= evaluateEvery {
		l.lastTick = now
		l.r.settle()
	}
	if l.onStep != nil {
		l.onStep()
	}
	l.adoptPlan()
	l.loop.dispatch(now, l.startRound, l.startProbe)
	l.apply(now)
}

func (l *timeline) frame(signals []domain.Signal) {
	l.t.Helper()
	l.wan.read(l.wan.State, l.now(), l.now())
	f := signalFrame{Signals: signals, WAN: l.wan, LineBusy: l.loop.lineBusy}
	if len(l.loop.plan.Targets) > 0 {
		f.Ranking = l.loop.rankingTarget().ID
	}
	l.r.c.ingest(f)
	l.r.settle()
}

func (l *timeline) deliver(a arrival) {
	l.t.Helper()
	switch {
	case a.beat != nil:
		l.loop.inFlight--
		l.frame(l.loop.accepted([]domain.Signal{*a.beat}))
	case l.loop.finished(*a.round):
		l.frame(a.round.job.answers(l.loop.accepted(a.round.signals)))
	}
}

// adoptPlan takes what the controller last published. The scripted Worker has a route for every
// server the plan attributes, and for nothing else.
func (l *timeline) adoptPlan() {
	select {
	case p := <-l.r.c.plans:
		p.Topology.Epoch = "e1"
		for _, ref := range slices.SortedFunc(mapKeys(p.Sources), compareRefs) {
			p.Topology.Probes = append(p.Topology.Probes, gateway.ProbeRoute{Server: ref, Epoch: "e1"})
		}
		l.loop.adopt(context.Background(), p)
	default:
	}
}

func (l *timeline) signal(ref domain.ServerRef, purpose domain.ProbePurpose, target domain.TargetID, now time.Time) (domain.Signal, time.Duration) {
	a := l.servers[string(ref.Lineage)](purpose)
	sig := domain.Signal{Stamp: domain.Stamp{Epoch: "e1", At: now}, Server: ref, Kind: domain.ActiveProbe, Verdict: a.verdict, WANExcluded: a.excluded, Purpose: purpose, Target: target, Ping: a.ping, NewConn: a.newCon}
	if l.noReuse[target] && sig.Ping != nil {
		sig.Ping, sig.NoReuse = nil, true
	}
	return sig, a.took
}

// counted adds one probe of kind on ref.
func (l *timeline) counted(kind string, ref domain.ServerRef) {
	if l.count[kind] == nil {
		l.count[kind] = map[domain.LineageID]int{}
	}
	l.count[kind][ref.Lineage]++
}

// total is how many probes of kind every server has had.
func (l *timeline) total(kind string) int {
	n := 0
	for _, c := range l.count[kind] {
		n += c
	}
	return n
}

var kindOf = map[domain.ProbePurpose]string{domain.Heartbeat: "heartbeat", domain.Search: "search", domain.Check: "check", domain.UDPCheck: "udp", domain.ManualProbe: "manual"}

// requestsOf is what measure sends: a heartbeat is one request, a failure stops at the first, and a
// full probe that passes makes one on a new connection and two reused. A UDP check sends no request.
func requestsOf(s domain.Signal) int {
	if s.Purpose == domain.UDPCheck {
		return 0
	}
	if !asksPing(s.Purpose) || s.Verdict != domain.Succeeded {
		return 1
	}
	return 1 + pingRequests
}

func (l *timeline) startProbe(ref domain.ServerRef, purpose domain.ProbePurpose) {
	now := l.now()
	target := domain.TargetID("t1")
	if purpose == domain.Check {
		target = l.loop.rankingTarget().ID
	}
	l.counted(kindOf[purpose], ref)
	sig, took := l.signal(ref, purpose, target, now)
	l.requests += requestsOf(sig)
	if purpose == domain.UDPCheck {
		l.checks[string(ref.Lineage)]++
	}
	l.inFlight = append(l.inFlight, arrival{at: now.Add(took), beat: &sig})
}

func (l *timeline) startRound(p path, job *roundJob) {
	now := l.now()
	job.cancel = func() {}
	l.rounds = append(l.rounds, job.wants...)
	l.attempts += len(job.servers)
	var signals []domain.Signal
	var longest time.Duration
	kind := map[path]string{failurePath: "failure round", improvePath: "comparison"}[p]
	for _, ref := range job.servers {
		l.counted(kind, ref)
		sig, took := l.signal(ref, domain.Confirmation, l.loop.jobTarget(job).ID, now)
		l.requests += requestsOf(sig)
		signals = append(signals, sig)
		longest = max(longest, took)
	}
	l.inFlight = append(l.inFlight, arrival{at: now.Add(longest), round: &roundDone{job, p, signals}})
}

func (l *timeline) apply(now time.Time) {
	l.t.Helper()
	for _, slot := range slices.Sorted(maps.Keys(l.r.c.state.pending)) {
		p := l.r.c.state.pending[slot]
		if now.Sub(p.RequestedAt) < timelineApply {
			continue
		}
		target := l.r.targetOf(slot)
		prior, until, held := target.Passage.Allows(target.Selection)
		l.moves = append(l.moves, move{slot: slot, at: now, reason: reasonNames[p.Reason], to: p.To, prior: prior, until: until, held: held, passage: target.Passage})
		l.r.reportApplied(slot)
	}
}

func (l *timeline) moved() bool { return len(l.moves) > 0 }

// heartbeatJustBeat runs to the moment after the current server's heartbeat started, so what
// happens next waits the whole period for the next one, which is the worst case.
func (l *timeline) heartbeatJustBeat() domain.ServerRef {
	l.t.Helper()
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	start := l.now()
	for l.loop.nextBeat[current].Sub(l.now()) != heartbeatServing {
		l.step()
		if l.now().Sub(start) > 10*time.Minute {
			l.t.Fatal("no heartbeat")
		}
	}
	return current
}

func (l *timeline) set(id domain.LineageID, b behaviour) { l.servers[string(id)] = b }

// warm runs until the serving server has the heartbeats the slow-heartbeat trigger compares with,
// which at one a minute takes minutes.
func (l *timeline) warm() { l.run(time.Duration(beatMinSamples+1) * heartbeatServing) }

// wantMove asserts the first move's reason, and when it landed against what the arithmetic says.
func (l *timeline) wantMove(name, reason string, took, nominal time.Duration, limit time.Duration) move {
	l.t.Helper()
	m := l.moves[0]
	if m.reason != reason {
		l.t.Fatalf("%s: reason %s, want %s", name, m.reason, reason)
	}
	if took >= limit {
		l.t.Fatalf("%s: moved after %s, past %s", name, took, limit)
	}
	if diff := took - nominal; diff < -timelineStep || diff > 4*timelineStep {
		l.t.Fatalf("%s: moved after %s, and the arithmetic says %s", name, took, nominal)
	}
	l.t.Logf("calculated: %s moved after %s, reason %s (limit %s)", name, took.Round(10*time.Millisecond), reason, limit)
	return m
}

var fourServers = map[string]int{"a": 65, "b": 67, "c": 70, "d": 72}

// failover bounds a move a heartbeat starts. An idle silent server's worst case is the probing plan's
// 60 + 4 + 4 + 1 + 4 = 73 s, and a baseline slow to answer adds its 3 s.
const failover = heartbeatServing + 20*time.Second

func TestTimelines(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(pingMs int) behaviour
		reason  string
		nominal time.Duration
	}{
		{"killed", func(int) behaviour { return killed() }, "failureMove",
			heartbeatServing + 3*killedFails + time.Second + timelineApply},
		{"silent", func(int) behaviour { return silent(0) }, "failureMove",
			heartbeatServing + 3*probeTimeout + time.Second + timelineApply},
		{"silent with a slow baseline", func(int) behaviour { return silent(baselineTimeout) }, "failureMove",
			heartbeatServing + 3*probeTimeout + baselineTimeout + time.Second + timelineApply},
		{"300 ms added", slowed, "slownessMove",
			heartbeatServing + 1530*time.Millisecond + 2*2100*time.Millisecond + time.Second + timelineApply},
		{"25% loss", func(ms int) behaviour { return lossy(ms, false) }, "slownessMove",
			heartbeatServing + 2*time.Second + 2*2100*time.Millisecond + time.Second + timelineApply},
		{"25% loss with a failed round", func(ms int) behaviour { return lossy(ms, true) }, "failureMove",
			heartbeatServing + 2*time.Second + probeTimeout + 2100*time.Millisecond + time.Second + timelineApply},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newTimeline(t, fourServers)
			l.warm()
			current := l.heartbeatJustBeat()
			l.set(current.Lineage, c.breakIt(fourServers[string(current.Lineage)]))
			took := l.runUntil(failover+5*time.Second, "move", l.moved)
			m := l.wantMove(c.name, c.reason, took, c.nominal, failover)
			if !m.held || m.prior != current {
				t.Fatalf("the move named no exact prior, so the gate would drop: %+v", m.passage)
			}
		})
	}
}

// A passive suspicion that a matched round cleared leaves a quiet period, and severe slowness starting
// right after it still moves within failover.
func TestTimelineSlownessRightAfterAHarmlessSuspicion(t *testing.T) {
	l := newTimeline(t, map[string]int{"1": 20, "2": 40})
	sig := l.r.probe("1", domain.Failed, 0)
	sig.Kind = domain.TCPInfo
	sig.AttributionCertain = true
	l.r.frame(domain.WANUp, sig)
	l.run(2 * time.Second)
	if m := l.r.memory("eu"); m.Campaign != nil || !m.QuietUntil.After(l.now()) {
		t.Fatalf("the suspicion didn't clear: %+v", m)
	}
	l.set("1", slowed(20))
	took := l.runUntil(failover, "move", l.moved)
	if l.moves[0].reason != "slownessMove" {
		t.Fatalf("reason %s", l.moves[0].reason)
	}
	t.Logf("calculated: slowness after a harmless suspicion moved after %s (limit %s)", took.Round(10*time.Millisecond), failover)
}

// A server that reads slow against itself but no worse than its rivals loses nothing in its rounds,
// so it runs at most four per quiet period.
func TestTimelineASlowServerThatLosesNothingDoesntKeepRunningRounds(t *testing.T) {
	l := newTimeline(t, map[string]int{"1": 20, "2": 40})
	l.run(2 * time.Minute)
	l.set("1", slowed(20))
	l.set("2", slowed(20))
	before := len(l.rounds)
	l.run(70 * time.Second)
	if l.moved() {
		t.Fatalf("moved: %+v", l.moves)
	}
	if got := len(l.rounds) - before; got < 1 || got > 12 {
		t.Fatalf("%d rounds in 70 s, with a quiet period of %s", got, l.r.c.timings.QuietAfter)
	}
}

// measuredLoss is the lab's 25% loss as one sequence: some new connections and rounds lose a packet
// and some lose none, measured at 39 to 55% of heartbeats over 500 ms and 64% of rounds over the severe
// line. Each answer takes the next of its own pattern, and the last repeats.
func measuredLoss(beats []int, rounds []int) behaviour {
	return func(purpose domain.ProbePurpose) reply {
		if !asksPing(purpose) {
			newConn := time.Duration(beats[0]) * time.Millisecond
			beats = beats[min(1, len(beats)-1):]
			return reply{verdict: domain.Succeeded, newCon: &newConn, took: newConn}
		}
		newConn := time.Duration(rounds[0]) * time.Millisecond
		rounds = rounds[min(1, len(rounds)-1):]
		ping := 2 * time.Millisecond
		return reply{verdict: domain.Succeeded, ping: &ping, newCon: &newConn, took: newConn + 2*ping}
	}
}

func TestTimelineLossShowsInSomeProbesAndStillMovesInTime(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.warm()
	current := l.heartbeatJustBeat()
	l.set(current.Lineage, measuredLoss([]int{20, 450}, []int{20, 1500, 20, 1500}))
	limit := heartbeatServing + failover
	took := l.runUntil(limit+5*time.Second, "move", l.moved)
	if l.moves[0].reason != "slownessMove" || took >= limit {
		t.Fatalf("%s after %s, limit %s", l.moves[0].reason, took, limit)
	}
	t.Logf("calculated: a heartbeat short of slow, then one over, then clean and slow rounds in turn, moved after %s", took.Round(100*time.Millisecond))
}

// lossWindow is the loss step's window in lab/control.sh: the probing plan's open point 2 gives slowness
// three heartbeats at 60 s to be noticed, two rounds then move it, and the rest is the lab's overhead.
// 25% loss on the lab path leaves 22% of heartbeats clean, so 1 run in 21 shows nothing in the two
// heartbeats a 150 s window holds.
const lossWindow = 200 * time.Second

// The lossy server's first two heartbeats lose no packet, and its third does.
func TestTimelineLossWhoseFirstTwoHeartbeatsShowNothingMovesWithinTheLabWindow(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.warm()
	current := l.heartbeatJustBeat()
	clean := 3 * fourServers[string(current.Lineage)]
	l.set(current.Lineage, measuredLoss([]int{clean, clean, 1500}, []int{2000}))
	took := l.runUntil(lossWindow, "move", l.moved)
	if l.moves[0].reason != "slownessMove" {
		t.Fatalf("%s after %s, want a slowness move", l.moves[0].reason, took)
	}
	t.Logf("calculated: moved after %s, window %s", took.Round(100*time.Millisecond), lossWindow)
}

// Spikes on the server's own heartbeats start rounds, and rounds that show nothing move nothing.
func TestTimelineAJitteryServerRunsRoundsAndStays(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.warm()
	current := l.heartbeatJustBeat()
	l.set(current.Lineage, measuredLoss([]int{20, 600}, []int{195}))
	before := len(l.rounds)
	l.run(4 * heartbeatServing)
	if l.moved() {
		t.Fatalf("moved: %+v", l.moves)
	}
	if got := len(l.rounds) - before; got < 1 || got > 16 {
		t.Fatalf("%d rounds in %s", got, 4*heartbeatServing)
	}
}

// Every server goes silent at once, moments after each answered. Those successes say nothing after
// the closure, so it stands until a server answers again.
func TestTimelineASilentGroupClosesInTimeAndStaysClosed(t *testing.T) {
	pings := map[string]int{}
	var last string
	for i := range 12 {
		last = fmt.Sprintf("s%02d", i)
		pings[last] = 60 + i*5
	}
	l := newTimeline(t, pings)
	for id := range pings {
		l.r.c.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: l.now()}, Signals: []domain.Signal{l.r.probe(id, domain.Succeeded, 180)}})
		l.set(domain.LineageID(id), silent(0))
	}
	took := l.runUntil(failover, "closure", l.closed)
	t.Logf("calculated: closed %s after the whole group went silent", took.Round(100*time.Millisecond))
	l.moves = nil
	for range 3 {
		l.run(30 * time.Second)
		if !l.closed() {
			t.Fatalf("reopened on a silent server: %+v", l.moves)
		}
	}
	l.set(domain.LineageID(last), healthy(pings[last]))
	back := l.runUntil(2*time.Minute, "reopening", func() bool { return !l.closed() })
	t.Logf("calculated: reopened %s after one server answered", back.Round(100*time.Millisecond))
}

// fastSearchBound is the plan's search for a group with no working server: the rest every 20 s finds a
// server that answers again, and its top-four beats every 5 s give it the spaced passes that reopen the
// slot, with 2 s for the last pass and the apply.
const fastSearchBound = heartbeatSweep + (recoverPasses-1)*recoverSpacing + 2*time.Second

// Twelve servers go silent and the slot closes. Then the lowest-ranked answers again, at every phase of
// the sweep. Eleven silent servers hold a worker 4 s each, and the search still keeps its periods.
func TestTimelineASilentGroupFindsTheServerThatAnswersOnTheFastSearch(t *testing.T) {
	pings := map[string]int{}
	var last domain.LineageID
	for i := range 12 {
		last = domain.LineageID(fmt.Sprintf("s%02d", i))
		pings[string(last)] = 60 + i*5
	}
	for phase := time.Duration(0); phase < heartbeatSweep+5*time.Second; phase += time.Second {
		l := newTimeline(t, pings)
		for id := range pings {
			l.set(domain.LineageID(id), silent(0))
		}
		l.runUntil(failover, "closure", l.closed)
		l.run(90*time.Second + phase)
		l.set(last, healthy(pings[string(last)]))
		start := l.now()
		var found time.Duration
		for l.closed() {
			if l.now().Sub(start) > 2*fastSearchBound {
				t.Fatalf("phase %s: still closed %s after %s answered", phase, l.now().Sub(start), last)
			}
			l.step()
			if found == 0 && l.r.record(last).standing() != failing {
				found = l.now().Sub(start)
			}
		}
		took := l.now().Sub(start)
		if found > heartbeatSweep+time.Second || took > fastSearchBound {
			t.Errorf("phase %s: %s passed %s and the slot reopened %s after it answered, want within %s and %s",
				phase, last, found.Round(100*time.Millisecond), took.Round(100*time.Millisecond), heartbeatSweep, fastSearchBound)
		}
	}
}

// The current server goes silent as a comparison round starts. The round's failure starts the failure
// path at once, whatever the comparison's streaks say, and the comparison leaves the plan.
func TestTimelineFailureDuringAComparison(t *testing.T) {
	l := newTimeline(t, fourServers)
	current := l.heartbeatJustBeat()
	comparing := l.r.improving("eu", l.r.ref("b")).ID
	l.set(current.Lineage, silent(0))
	took := l.runUntil(failover, "move", l.moved)
	if l.moves[0].reason != "failureMove" || took > 3*probeTimeout+time.Second+timelineApply+4*timelineStep {
		t.Fatalf("%s after %s, want a failure move from the comparison's own round and two failure rounds", l.moves[0].reason, took)
	}
	t.Logf("calculated: a failure during a comparison moved after %s", took.Round(10*time.Millisecond))
	p, _ := l.r.c.observation()
	if slices.ContainsFunc(p.Rounds, func(q roundRequest) bool { return q.Campaign == comparing }) {
		t.Fatal("the comparison round stayed in the plan")
	}
}

func TestTimelinePoolShrink(t *testing.T) {
	l := newTimeline(t, fourServers)
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	var rest []domain.LineageID
	for _, id := range []domain.LineageID{"a", "b", "c", "d"} {
		if id != current.Lineage {
			rest = append(rest, id)
		}
	}
	l.r.putPicked("g", "G", "a", "b", "c", "d")
	l.r.setExit("eu", domain.GroupExit("g"))
	l.r.settle()
	l.run(2 * evaluateEvery)
	if l.moved() {
		t.Fatalf("a group allowing the server moved it: %+v", l.moves)
	}
	version := l.r.c.state.slots["eu"].Slot.Version
	l.r.putPicked("g", "G", rest...)
	l.r.settle()
	took := l.runUntil(5*time.Second, "move", l.moved)
	l.wantMove("pool shrink", "exitChanged", took, timelineApply, failover)
	if got := l.r.c.state.slots["eu"].Slot.Version; got != version {
		t.Fatalf("a group edit moved Version from %d to %d", version, got)
	}
}

// otherServers is everything but the current server, for an Exit that no longer allows it.
func (l *timeline) otherServers() []domain.LineageID {
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	var rest []domain.LineageID
	for _, id := range slices.Sorted(mapKeys(l.servers)) {
		if id != string(current.Lineage) {
			rest = append(rest, domain.LineageID(id))
		}
	}
	return rest
}

func (l *timeline) exitTo(members ...domain.LineageID) {
	l.t.Helper()
	l.r.putPicked("elsewhere", "Elsewhere", members...)
	l.r.setExit("eu", domain.GroupExit("elsewhere"))
	l.r.settle()
}

func (l *timeline) leaveBy() time.Time { return l.r.memory("eu").LeaveBy }

// silence has servers fail a probe now and stay silent, so their records stand failed and none of
// them is ready.
func (l *timeline) silence(ids []domain.LineageID) {
	var failed []domain.Signal
	for _, id := range ids {
		l.set(id, silent(0))
		failed = append(failed, l.r.probe(string(id), domain.Failed, 0))
	}
	l.r.c.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: l.now()}, Signals: failed})
}

func (l *timeline) closed() bool {
	_, down := l.r.c.state.slots["eu"].Selection.Down()
	return down
}

func TestTimelineExitChange(t *testing.T) {
	t.Run("a ready server takes over at once and the gate stays up", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		current, _ := l.r.c.state.slots["eu"].Selection.Server()
		l.exitTo(l.otherServers()...)
		took := l.runUntil(5*time.Second, "move", l.moved)
		m := l.wantMove("Exit change", "exitChanged", took, timelineApply, failover)
		if !m.held || m.prior != current || m.until.After(l.now().Add(l.r.c.timings.Handover)) {
			t.Fatalf("passage: %+v", m.passage)
		}
		if !l.leaveBy().IsZero() {
			t.Fatal("a slot on an allowed server kept a deadline")
		}
	})
	t.Run("with nothing ready it bridges to exactly 30 s and closes", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		rest := l.otherServers()
		l.silence(rest)
		current, _ := l.r.c.state.slots["eu"].Selection.Server()
		l.exitTo(rest...)
		deadline := l.leaveBy()
		if !deadline.Equal(l.now().Add(l.r.c.timings.Handover)) {
			t.Fatalf("deadline %v", deadline)
		}
		if target := l.r.targetOf("eu"); !bridgesUntil(target, deadline) || target.Selection != domain.Serving(current) {
			t.Fatalf("the bridge: %+v", target)
		}
		took := l.runUntil(failover+5*time.Second, "closure", l.closed)
		if got := l.now(); got.Before(deadline) || got.Sub(deadline) > evaluateEvery+timelineStep {
			t.Fatalf("closed %s after its deadline", got.Sub(deadline))
		}
		t.Logf("calculated: bridge with nothing ready closed after %s (limit %s)", took.Round(10*time.Millisecond), failover)
		if d, _ := l.r.c.state.slots["eu"].Selection.Down(); d != domain.NoUsableServer {
			t.Fatalf("down reason %d", d)
		}
		l.runUntil(time.Second, "applied closure", l.moved)
		if l.moves[0].reason != "exitChanged" {
			t.Fatalf("reason %s", l.moves[0].reason)
		}
		if !l.leaveBy().IsZero() {
			t.Fatal("a closed slot kept a deadline")
		}
	})
	t.Run("a bridge that fails closes at once with no rounds", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		rest := l.otherServers()
		l.silence(rest)
		current, _ := l.r.c.state.slots["eu"].Selection.Server()
		l.exitTo(rest...)
		roundsBefore := len(l.rounds)
		l.set(current.Lineage, killed())
		took := l.runUntil(failover, "closure", l.closed)
		if took >= heartbeatClosed+killedFails+evaluateEvery {
			t.Fatalf("a failing bridge closed after %s", took)
		}
		t.Logf("calculated: a failing bridge closed after %s (limit %s)", took.Round(10*time.Millisecond), failover)
		if len(l.rounds) != roundsBefore {
			t.Fatalf("a server the owner is leaving got confirmation rounds: %+v", l.rounds[roundsBefore:])
		}
		l.runUntil(time.Second, "applied closure", l.moved)
		if l.moves[0].reason != "exitChanged" {
			t.Fatalf("reason %s", l.moves[0].reason)
		}
	})
	t.Run("a move landing during a bridge can't outlast the bridge", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		rest := l.otherServers()
		l.silence(rest[1:])
		delete(l.r.c.evidence.book.records, rest[0])
		current, _ := l.r.c.state.slots["eu"].Selection.Server()
		l.exitTo(rest...)
		deadline := l.leaveBy()
		if deadline.IsZero() {
			t.Fatal("a server never probed was ready")
		}
		took := l.runUntil(heartbeatClosed+probeTimeout+evaluateEvery, "move", l.moved)
		m := l.moves[0]
		if m.reason != "exitChanged" || !m.held || m.prior != current || !m.until.Equal(deadline) {
			t.Fatalf("after %s: %+v, deadline %v", took, m, deadline)
		}
		if l.closed() || !l.leaveBy().IsZero() {
			t.Fatalf("landed: %+v", l.r.memory("eu"))
		}
	})
	t.Run("an edit during a bridge keeps the deadline, and an Exit allowing it clears it", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		rest := l.otherServers()
		l.silence(rest)
		current, _ := l.r.c.state.slots["eu"].Selection.Server()
		l.exitTo(rest...)
		deadline := l.leaveBy()
		l.run(10 * time.Second)
		l.exitTo(rest[0])
		if got := l.leaveBy(); !got.Equal(deadline) {
			t.Fatalf("an edit moved the deadline from %v to %v", deadline, got)
		}
		l.r.putPicked("back", "Back", current.Lineage)
		l.r.setExit("eu", domain.GroupExit("back"))
		l.r.settle()
		if !l.leaveBy().IsZero() {
			t.Fatal("an Exit allowing the server again kept the deadline")
		}
		if l.closed() || l.moved() {
			t.Fatalf("moves: %+v", l.moves)
		}
		l.exitTo(rest...)
		if got := l.leaveBy(); !got.Equal(l.now().Add(l.r.c.timings.Handover)) {
			t.Fatalf("a new departure's deadline: %v", got)
		}
	})
}

func campaignOf(l *timeline) *campaign { return l.r.memory("eu").Campaign }

// silentOnAList has a group slot's server just gone silent, on the worst heartbeat, and returns it.
func silentOnAList(t *testing.T, members map[string]int) (*timeline, domain.ServerRef) {
	t.Helper()
	l := newTimeline(t, members)
	var ids []domain.LineageID
	for _, id := range slices.Sorted(mapKeys(members)) {
		ids = append(ids, domain.LineageID(id))
	}
	l.r.putPicked("g", "G", ids...)
	l.r.setExit("eu", domain.GroupExit("g"))
	l.r.settle()
	l.run(2 * evaluateEvery)
	l.moves = nil
	current := l.heartbeatJustBeat()
	l.set(current.Lineage, silent(0))
	return l, current
}

// lateInRoundTwo runs to the last moment of a failure campaign's second round, where restarting
// would cost the most.
func (l *timeline) lateInRoundTwo() *campaign {
	l.t.Helper()
	l.runUntil(failover, "round 2 nearly done", func() bool {
		c := campaignOf(l)
		return c != nil && c.Path == failurePath && c.Round == 2 && !l.now().Before(c.Due.Add(3900*time.Millisecond))
	})
	return campaignOf(l)
}

func (l *timeline) rotate(id domain.LineageID) {
	l.t.Helper()
	rotated, _ := l.r.c.world.catalogue.Lookup(id)
	rotated.Ref.Digest[5] = 0xee
	l.r.storeLineages(transaction{Lineages: []domain.Lineage{rotated}})
	l.r.settle()
}

func TestTimelineAConfirmationOutlivesARivalsRotationOrRemoval(t *testing.T) {
	cases := []struct {
		name    string
		members map[string]int
		change  func(*timeline, domain.ServerRef, *campaign)
	}{
		{"one of three rivals rotates", fourServers, func(l *timeline, _ domain.ServerRef, c *campaign) { l.rotate(c.Rivals[0].Lineage) }},
		{"every rival rotates", fourServers, func(l *timeline, _ domain.ServerRef, c *campaign) {
			for _, rival := range c.Rivals {
				l.rotate(rival.Lineage)
			}
		}},
		{"the only rival rotates", map[string]int{"a": 65, "b": 67}, func(l *timeline, _ domain.ServerRef, c *campaign) { l.rotate(c.Rivals[0].Lineage) }},
		{"a group edit drops one of three rivals", fourServers, func(l *timeline, current domain.ServerRef, c *campaign) {
			l.r.putPicked("g", "G", current.Lineage, c.Rivals[0].Lineage, c.Rivals[1].Lineage)
			l.r.settle()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, current := silentOnAList(t, c.members)
			start := l.now()
			running := l.lateInRoundTwo()
			c.change(l, current, running)
			if got := campaignOf(l); got != nil && got.ID != running.ID {
				t.Fatalf("the confirmation restarted as %s", got.ID)
			}
			l.runUntil(failover, "move", l.moved)
			took := l.moves[0].at.Sub(start)
			if took >= failover || l.moves[0].reason != "failureMove" {
				t.Fatalf("moved after %s, reason %s, limit %s", took, l.moves[0].reason, failover)
			}
			t.Logf("calculated: %s: moved after %s (limit %s)", c.name, took.Round(10*time.Millisecond), failover)
			if server, _ := l.moves[0].to.Server(); server != l.r.ref(string(server.Lineage)) {
				t.Fatalf("moved onto a rotated rival's old ref: %s", server.Lineage)
			}
		})
	}
	t.Run("a group edit that drops every rival abandons it", func(t *testing.T) {
		l, current := silentOnAList(t, fourServers)
		running := l.lateInRoundTwo()
		l.r.putPicked("g", "G", current.Lineage)
		l.r.settle()
		if got := campaignOf(l); got != nil && got.ID == running.ID {
			t.Fatalf("still running with no rival: %+v", got)
		}
	})
}

// slots puts slots beyond "eu" on their groups, with eu on the first, and runs until they all serve
// the server they were meant to. Each is served by the member with the lowest Ping, its head.
func (l *timeline) slots(groups map[domain.SlotID][]domain.LineageID) {
	l.t.Helper()
	for _, id := range slices.Sorted(mapKeys(groups)) {
		group := domain.GroupID("g-" + id)
		l.r.putPicked(group, string(id), groups[id]...)
		if id == "eu" {
			l.r.setExit("eu", domain.GroupExit(group))
		} else {
			l.r.putSlot(id, exitIntent(l.t, string(id), domain.GroupExit(group)))
		}
	}
	l.r.settle()
	l.runUntil(failover, "every slot on its head", func() bool {
		for id, members := range groups {
			if l.r.server(id) != members[0] || l.r.operation(id) != "" {
				return false
			}
		}
		return true
	})
	l.moves = nil
}

func (l *timeline) movedOf(slot domain.SlotID) (move, bool) {
	for _, m := range l.moves {
		if m.slot == slot {
			return m, true
		}
	}
	return move{}, false
}

// One dead server is one failure. Slots on it with different groups measure each server once, so the
// third of them waits for nothing.
func TestTimelineThreeGroupsOnOneDeadServer(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 20, "b": 30, "c": 40, "d": 50, "e": 60})
	l.slots(map[domain.SlotID][]domain.LineageID{"eu": {"a", "b", "c", "d"}, "two": {"a", "b", "c", "e"}, "three": {"a", "b", "d", "e"}})
	current := l.heartbeatJustBeat()
	l.set(current.Lineage, silent(0))
	start, spent := l.now(), l.attempts
	l.runUntil(failover+5*time.Second, "all three moved", func() bool { return len(l.moves) == 3 })
	for _, slot := range []domain.SlotID{"eu", "two", "three"} {
		m, _ := l.movedOf(slot)
		took := m.at.Sub(start)
		if took >= failover || m.reason != "failureMove" {
			t.Errorf("slot %s moved after %s, reason %s, limit %s", slot, took, m.reason, failover)
		}
		t.Logf("calculated: slot %s moved after %s (limit %s)", slot, took.Round(10*time.Millisecond), failover)
	}
	if got := l.attempts - spent; got != 10 {
		t.Errorf("two rounds on five servers cost %d attempts", got)
	}
}

// noisyGroups has eu on a, and a slot on each noisy server, whose rivals share nothing with anyone's,
// so sharing a measurement can't hide what the noise costs. Every member but the heads is slower.
func noisyGroups(noisy int) (map[string]int, map[domain.SlotID][]domain.LineageID, []domain.LineageID) {
	pings := map[string]int{"a": 20, "e": 60, "f": 61, "g": 62}
	groups := map[domain.SlotID][]domain.LineageID{"eu": {"a", "e", "f", "g"}}
	var heads []domain.LineageID
	for i := range noisy {
		head := fmt.Sprintf("n%d", i)
		pings[head] = 20
		members := []domain.LineageID{domain.LineageID(head)}
		for j := range failureRivals {
			rival := fmt.Sprintf("r%d%d", i, j)
			pings[rival] = 70 + j
			members = append(members, domain.LineageID(rival))
		}
		groups[domain.SlotID("s"+head)] = members
		heads = append(heads, domain.LineageID(head))
	}
	return pings, groups, heads
}

// noise raises a passive suspicion on healthy servers every 5 s, as a socket stalled or over 5%
// retransmits does.
func (l *timeline) noise(heads []domain.LineageID) {
	next := l.now()
	l.onStep = func() {
		if l.now().Before(next) {
			return
		}
		next = l.now().Add(5 * time.Second)
		var signals []domain.Signal
		for _, id := range heads {
			signals = append(signals, domain.Signal{Stamp: domain.Stamp{Epoch: "e1", At: l.now()}, Server: l.r.ref(string(id)), Kind: domain.TCPInfo, Verdict: domain.Failed, AttributionCertain: true})
		}
		l.frame(signals)
	}
}

func TestTimelineSuspicionNoiseCantDelayAFailover(t *testing.T) {
	for _, noisy := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("noisy=%d", noisy), func(t *testing.T) {
			pings, groups, heads := noisyGroups(noisy)
			l := newTimeline(t, pings)
			l.slots(groups)
			l.noise(heads)
			l.run(45 * time.Second)
			if l.moved() {
				t.Fatalf("noise moved a slot: %+v", l.moves)
			}
			current := l.heartbeatJustBeat()
			if current.Lineage != "a" {
				t.Fatalf("eu is on %s", current.Lineage)
			}
			l.set("a", silent(0))
			took := l.runUntil(failover+5*time.Second, "move", l.moved)
			if took >= failover || l.moves[0].slot != "eu" || l.moves[0].reason != "failureMove" {
				t.Fatalf("moved after %s, %+v, limit %s", took, l.moves[0], failover)
			}
			t.Logf("calculated: with %d noisy servers, moved after %s (limit %s)", noisy, took.Round(10*time.Millisecond), failover)
		})
	}
}

// Noise can empty the bucket, and that costs a failure nothing.
func TestTimelineAFailureWithTheBudgetDrainedByNoiseStillMoves(t *testing.T) {
	pings, groups, heads := noisyGroups(3)
	l := newTimeline(t, pings)
	l.slots(groups)
	l.noise(heads)
	l.run(45 * time.Second)
	current := l.heartbeatJustBeat()
	l.set(current.Lineage, silent(0))
	l.runUntil(heartbeatServing+probeTimeout+time.Second, "failure campaign", func() bool {
		c := campaignOf(l)
		return c != nil && c.Cause == failedProbe
	})
	lane := &l.loop.lanes[failurePath]
	lane.bucket.tokens, lane.bucket.last = 0, l.now()
	lane.bucket.perSecond = 0
	took := l.runUntil(failover, "move", l.moved)
	if got := l.moves[0].reason; got != "failureMove" {
		t.Fatalf("reason %s after %s", got, took)
	}
}

// A server that was only suspected dies while its round waits for the bucket. The heartbeat that
// fails makes that campaign real, and it doesn't wait for the campaign to expire.
func TestTimelineASuspectedServerThatDiesIsntHeldForTheBucket(t *testing.T) {
	pings, groups, heads := noisyGroups(1)
	l := newTimeline(t, pings)
	l.slots(groups)
	head := heads[0]
	slot := domain.SlotID("s" + head)
	campaign := func() *campaign { return l.r.memory(slot).Campaign }
	for l.loop.nextBeat[l.r.ref(string(head))].Sub(l.now()) != heartbeatServing {
		l.step()
	}
	start := l.now()
	l.set(head, silent(0))
	l.run(time.Second)
	lane := &l.loop.lanes[failurePath]
	lane.bucket.tokens, lane.bucket.last = 0, l.now()
	lane.bucket.perSecond = 0
	l.frame([]domain.Signal{{Stamp: domain.Stamp{Epoch: "e1", At: l.now()}, Server: l.r.ref(string(head)), Kind: domain.TCPInfo, Verdict: domain.Failed, AttributionCertain: true}})
	if c := campaign(); c == nil || c.Cause != passiveSuspicion {
		t.Fatalf("campaign: %+v", c)
	}
	l.runUntil(failover, "move", func() bool { _, ok := l.movedOf(slot); return ok })
	m, _ := l.movedOf(slot)
	took := m.at.Sub(start)
	if took >= failover || m.reason != "failureMove" {
		t.Fatalf("moved after %s, %+v, limit %s", took, m, failover)
	}
	t.Logf("calculated: a suspected server that died moved after %s (limit %s)", took.Round(10*time.Millisecond), failover)
}

// A healthy server's passing rounds buy it a long quiet, so noise that never stops costs little.
func TestTimelineAServerWhoseRoundsKeepPassingStopsSpending(t *testing.T) {
	pings, groups, heads := noisyGroups(3)
	l := newTimeline(t, pings)
	l.slots(groups)
	l.noise(heads)
	spent := l.attempts
	const window = 10 * time.Minute
	l.run(window)
	got := l.attempts - spent
	limit := 3 * (1 + failureRivals) * int(window/l.r.c.timings.PassiveQuiet+1)
	if got > limit {
		t.Fatalf("%d attempts in %s of noise, limit %d", got, window, limit)
	}
	t.Logf("calculated: %d attempts on noise in %s, against a refill of %d", got, window, int(window.Minutes())*budget.SuspicionsPerMinute)
	if l.moved() {
		t.Fatalf("noise moved a slot: %+v", l.moves)
	}
}

// The baseline goes silent for 5 s in the middle of a confirmation, so the WAN reads down, while the
// rivals go on passing through it.
func TestTimelineABaselineBlipCostsARunningConfirmationNothing(t *testing.T) {
	l, _ := silentOnAList(t, fourServers)
	start := l.now()
	l.onStep = func() {
		switch l.now().Sub(start) {
		case 14 * time.Second:
			l.wan.read(domain.WANDown, l.now(), l.now())
			l.frame(nil)
		case 19 * time.Second:
			l.wan.read(domain.WANUp, l.now(), l.now())
			l.frame(nil)
		}
	}
	l.runUntil(failover, "move", l.moved)
	took := l.moves[0].at.Sub(start)
	if took >= failover || l.moves[0].reason != "failureMove" {
		t.Fatalf("moved after %s, %+v, limit %s", took, l.moves[0], failover)
	}
	t.Logf("calculated: a blip from +14 s to +19 s: moved after %s (limit %s)", took.Round(10*time.Millisecond), failover)
}

// The server goes silent and the baseline blips just as its heartbeat needs an answer, so that
// heartbeat blames nobody. It still starts rounds, and the rivals passing in them prove the WAN.
func TestTimelineABaselineBlipOnTheFailingHeartbeatCostsNoHeartbeat(t *testing.T) {
	l, _ := silentOnAList(t, fourServers)
	start := l.now()
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	l.set(current.Lineage, func(purpose domain.ProbePurpose) reply {
		if !asksPing(purpose) {
			return reply{verdict: domain.Unknown, excluded: true, took: probeTimeout + baselineTimeout}
		}
		return reply{verdict: domain.Failed, took: probeTimeout}
	})
	took := l.runUntil(failover, "move", l.moved)
	l.wantMove("a blip on the failing heartbeat", "failureMove", took, heartbeatServing+probeTimeout+baselineTimeout+2*probeTimeout+time.Second+timelineApply, failover)
	if l.now().Sub(start) >= failover {
		t.Fatalf("moved after %s", l.now().Sub(start))
	}
}

// Every server dies and the slot closes, then one comes back that nothing has ranked well for an hour.
// Dead servers keep no place for their good history, so a sweep finds it and it earns one, and its
// spaced passes reopen the slot.
func TestTimelineAClosedSlotFindsALiveServerWithinTwoMinutes(t *testing.T) {
	slowTest(t)
	for _, c := range []struct {
		name string
		kept int
		dead behaviour
	}{
		{"4 kept, killed", 4, killed()},
		{"6 kept, killed", 6, killed()},
		{"12 kept, killed", 12, killed()},
		{"4 kept, silent", 4, silent(0)},
		{"6 kept, silent", 6, silent(0)},
		{"12 kept, silent", 12, silent(0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			pings := map[string]int{}
			var worst string
			for i := range c.kept {
				worst = fmt.Sprintf("s%02d", i)
				pings[worst] = 60 + i*5
			}
			l := newTimeline(t, pings)
			for id := range pings {
				l.set(domain.LineageID(id), c.dead)
			}
			l.runUntil(10*time.Minute, "closure", l.closed)
			l.run(10 * time.Minute)
			if !l.closed() {
				t.Fatalf("reopened on a dead pool: %+v", l.r.c.state.slots["eu"].Selection)
			}
			l.set(domain.LineageID(worst), healthy(pings[worst]))
			took := l.runUntil(5*time.Minute, "reopening", func() bool { return !l.closed() })
			t.Logf("calculated: %s: reopened on %s after %s", c.name, worst, took.Round(100*time.Millisecond))
			if took > 2*time.Minute {
				t.Errorf("a closed slot stayed closed %s beside a live allowed server", took.Round(time.Second))
			}
		})
	}
}

// Another slot serving costs the sweep a worker, and costs that slot nothing.
func TestTimelineASweepBesideAServingSlot(t *testing.T) {
	pings := map[string]int{"x": 20}
	var members []domain.LineageID
	var worst domain.LineageID
	for i := range 12 {
		id := fmt.Sprintf("s%02d", i)
		pings[id] = 60 + i*5
		worst = domain.LineageID(id)
		members = append(members, worst)
	}
	l := newTimeline(t, pings)
	l.slots(map[domain.SlotID][]domain.LineageID{"eu": members, "other": {"x"}})
	for _, id := range members {
		l.set(id, silent(0))
	}
	l.runUntil(10*time.Minute, "closure", l.closed)
	l.run(10 * time.Minute)
	l.set(worst, healthy(pings[string(worst)]))
	took := l.runUntil(5*time.Minute, "reopening", func() bool { return !l.closed() })
	t.Logf("calculated: reopened after %s beside a serving slot", took.Round(100*time.Millisecond))
	if took > 2*time.Minute {
		t.Errorf("a closed slot stayed closed %s beside a live allowed server", took.Round(time.Second))
	}

	// The serving slot's server dies during another sweep, and its heartbeat hasn't waited for one.
	for _, id := range members {
		l.set(id, silent(0))
	}
	l.runUntil(5*time.Minute, "closure", l.closed)
	l.moves = nil
	for l.loop.nextBeat[l.r.ref("x")].Sub(l.now()) != heartbeatServing {
		l.step()
	}
	l.set("x", killed())
	start := l.now()
	l.runUntil(heartbeatServing+time.Second, "the serving heartbeat's failure", func() bool {
		return len(l.r.c.evidence.results(l.r.ref("x"))) > 0 && l.r.c.evidence.results(l.r.ref("x"))[len(l.r.c.evidence.results(l.r.ref("x")))-1].Verdict == domain.Failed
	})
	if got := l.now().Sub(start); got > heartbeatServing+killedFails+2*timelineStep {
		t.Errorf("a serving heartbeat's failure landed %s after the death, behind a sweep", got)
	}
}

func TestTimelineRestartDuringABridge(t *testing.T) {
	l := newTimeline(t, fourServers)
	rest := l.otherServers()
	l.silence(rest)
	l.exitTo(rest...)
	deadline := l.leaveBy()
	if stored := l.storedLeaveBy(); !stored.Equal(deadline) {
		t.Fatalf("the deadline isn't durable: %v, want %v", stored, deadline)
	}
	l.run(10 * time.Second)
	l.restart()
	if got := l.leaveBy(); !got.Equal(deadline) {
		t.Fatalf("a restart moved the deadline to %v, want %v", got, deadline)
	}
	if target := l.r.targetOf("eu"); !bridgesUntil(target, deadline) {
		t.Fatalf("after a restart the bridge is %+v", target.Passage)
	}
	took := l.runUntil(failover, "closure", l.closed)
	if got := l.now(); got.Before(deadline) || got.Sub(deadline) > evaluateEvery+timelineStep {
		t.Fatalf("closed %s after its deadline", got.Sub(deadline))
	}
	t.Logf("calculated: a restart 10 s into a bridge closed it after %s more, on its original deadline", took.Round(10*time.Millisecond))
}

// A switch superseded before it applied stays unapplied on its row forever. A restart must read the
// slot's newest event, or that old closure comes back as pending and the first closed report applies
// it to a slot that is serving and not blocked.
func TestARestartDoesntBringBackASupersededSwitch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r := newRig(t, dir)
	electA(t, r)
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	onGroup := r.c.state.slots["eu"].Slot.Intent.Exit()
	put := func(blocked bool) {
		t.Helper()
		exit := onGroup
		if blocked {
			exit = domain.BlockExit()
		}
		r.setExit("eu", exit)
		r.settle()
	}
	put(true)
	superseded := r.operation("eu")
	put(false)
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	latest := r.operation("eu")
	if latest == superseded || latest == "" {
		t.Fatalf("no newer switch: %q, then %q", superseded, latest)
	}
	r.reportApplied("eu")
	if n := len(r.c.state.pending); n != 0 {
		t.Fatalf("still pending before the restart: %d", n)
	}
	clock := r.clock
	r.close()

	r = newRigAt(t, dir, clock.Add(10*time.Second))
	r.c.evidence.epoch = "e1"
	if view := r.app.Snapshot().Slots[0]; view.Pending {
		t.Errorf("a slot whose last switch applied reads pending after a restart: %v", r.c.state.pending)
	}
	target := r.targetOf("eu")
	if err := r.c.applyReport(ctx, gateway.Report{Generation: r.c.state.runtimeGeneration, Topology: gateway.Topology{Epoch: "e1", CorePID: 1},
		Slots: []gateway.SlotReport{{Slot: "eu", Phase: gateway.Closed, Operation: target.Operation}}}); err != nil {
		t.Fatal(err)
	}
	if got := r.c.state.slots["eu"].Selection; got != domain.Serving(r.ref("a")) && got != domain.Serving(r.ref("b")) {
		t.Errorf("a serving, unpaused slot became %s from a switch superseded before the restart", describe(got))
	}
}

func (l *timeline) storedLeaveBy() time.Time {
	l.t.Helper()
	state, _, err := l.r.c.store.recover(context.Background(), l.now(), l.r.c.timings)
	if err != nil {
		l.t.Fatal(err)
	}
	o, _ := ownerOf(state.slots["eu"])
	return state.owners[o].LeaveBy
}

func TestAStaleReportDoesntApplyTheMoveThatSupersededIt(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), lineage(t, "b", "good", "DE", "198.51.100.20"))
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", elected(t, "good"))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	superseded := r.operation("eu")
	if superseded == "" || r.server("eu") != "a" {
		t.Fatalf("elected %v, operation %v", r.server("eu"), superseded)
	}
	c := r.improving("eu", r.ref("b"))
	a, b := r.probe("a", domain.Succeeded, 0), r.probe("b", domain.Succeeded, 0)
	a.Ping, b.Ping = ms(200), ms(20)
	a.Campaign, a.Round, b.Campaign, b.Round = c.ID, c.Round, c.ID, c.Round
	r.frame(domain.WANUp, a, b)
	if r.server("eu") != "b" {
		t.Fatalf("serving %v", r.server("eu"))
	}
	own := r.operation("eu")

	report := func(operation domain.EventID) gateway.Report {
		return gateway.Report{Generation: r.c.state.runtimeGeneration, Topology: gateway.Topology{Epoch: "e1"},
			Slots: []gateway.SlotReport{{Slot: "eu", Phase: gateway.Ready, Applied: domain.Serving(r.ref("b")), Operation: operation}}}
	}
	if err := r.c.applyReport(ctx, report(superseded)); err != nil {
		t.Fatal(err)
	}
	if r.operation("eu") != own {
		t.Fatal("a report about a superseded move marked the new one applied")
	}
	if err := r.c.applyReport(ctx, report(own)); err != nil {
		t.Fatal(err)
	}
	if r.operation("eu") != "" {
		t.Fatal("the move's own report didn't apply it")
	}
}

// A slot on Block keeps its tunnel, drops what it carries, and watches nothing.
func TestABlockedSlotDropsAndWatchesNothing(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"))
	r.c.evidence.epoch = "e1"
	r.putSlot("none", exitIntent(t, "None", domain.BlockExit()))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10))
	row := r.c.state.slots["none"]
	if !row.Selection.IsBlocked() {
		t.Fatalf("selection: %+v", row.Selection)
	}
	view := r.c.slotView(row, r.memory("none"), gateway.SlotReport{}, r.clock, r.c.world)
	if view.Status != "block" || view.Exit.Kind != "block" || view.DownReason != nil {
		t.Fatalf("view: %+v", view)
	}
	plan, _, err := r.c.desired()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Slots()) != 1 || !plan.Slots()[0].Selection.IsBlocked() {
		t.Fatalf("the slot must stay in the plan, dropping: %+v", plan.Slots())
	}
	p, _ := r.c.observation()
	if _, beats := p.Beats[r.ref("a")]; beats {
		t.Fatal("a blocked slot has a server's heartbeat")
	}
	for _, s := range r.switches() {
		if s.Reason == "failureMove" {
			t.Fatalf("Block reads as a failure: %+v", s)
		}
	}
}

// Leaving for Block closes at once, and coming back owes no back-off.
func TestBlockClosesAtOnceAndComingBackOpensAtOnce(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.setExit("eu", domain.BlockExit())
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10))
	if !r.c.state.slots["eu"].Selection.IsBlocked() || !r.memory("eu").LeaveBy.IsZero() {
		t.Fatalf("right after the edit: %v, leaving by %v", describe(r.c.state.slots["eu"].Selection), r.memory("eu").LeaveBy)
	}
	if !slices.ContainsFunc(r.switches(), func(e SwitchEvent) bool { return e.To.Kind == "block" && e.Reason == "exitChanged" }) {
		t.Fatalf("no closure reads exitChanged: %+v", r.switches())
	}
	r.reportApplied("eu")
	m := r.memory("eu")
	m.Backoff.NotBefore = r.clock.Add(time.Hour)
	r.setMemory("eu", m)
	r.setExit("eu", domain.GroupExit(sourceGroup("good")))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	if got := r.server("eu"); got != "a" {
		t.Fatalf("a slot back on its group opened on %v", got)
	}
}

// An improvement in progress was for the Exit it began under, so a new Exit clears it and nothing else does.
func TestAnExitChangeClearsAnyImprovement(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) *rig {
		t.Helper()
		r := newRig(t, t.TempDir())
		electA(t, r)
		r.improving("eu", r.ref("b"))
		return r
	}
	put := func(t *testing.T, r *rig, name string, exit domain.Exit) policyMemory {
		t.Helper()
		intent, err := domain.NewIntent(name, exit)
		if err != nil {
			t.Fatal(err)
		}
		row := r.c.state.slots["eu"]
		if _, err := r.c.accept(ctx, PutSlot{ID: "eu", Expected: row.Slot.Version, Intent: intent}); err != nil {
			t.Fatal(err)
		}
		return r.memory("eu")
	}
	group := domain.GroupExit(sourceGroup("good"))
	t.Run("a rename keeps it", func(t *testing.T) {
		r := setup(t)
		if m := put(t, r, "Renamed", group); m.Campaign == nil {
			t.Fatalf("memory: %+v", m)
		}
	})
	t.Run("a group edit keeps it", func(t *testing.T) {
		r := setup(t)
		g := r.c.world.groups[sourceGroup("good")]
		r.putGroup(g.ID, "Renamed", g.Members)
		if m := r.memory("eu"); m.Campaign == nil {
			t.Fatalf("memory: %+v", m)
		}
	})
	t.Run("an Exit change clears it", func(t *testing.T) {
		r := setup(t)
		if m := put(t, r, "EU", domain.ServerExit("a")); m.Campaign != nil {
			t.Fatalf("memory: %+v", m)
		}
	})
	t.Run("a failure campaign outlives a change of choice", func(t *testing.T) {
		failure := &campaign{ID: "f", Path: failurePath}
		if got := (policyMemory{Campaign: failure}).choiceChanged(t0).Campaign; got != failure {
			t.Fatalf("campaign after a change of choice: %+v", got)
		}
	})
	t.Run("a stored keep-out survives a restart, clamped", func(t *testing.T) {
		dir := t.TempDir()
		r := newRig(t, dir)
		electA(t, r)
		m := r.memory("eu")
		m.KeepOut = map[domain.LineageID]time.Time{"b": r.clock.Add(24 * time.Hour)}
		owner, _ := ownerOf(r.c.state.slots["eu"])
		if err := r.c.store.commit(ctx, transaction{Owners: map[domain.Owner]policyMemory{owner: m}}); err != nil {
			t.Fatal(err)
		}
		r.close()
		got := newRigAt(t, dir, r.clock).memory("eu")
		if !got.KeepOut["b"].Equal(r.clock.Add(5 * time.Minute)) {
			t.Fatalf("clamped to %v", got.KeepOut)
		}
	})
}

func TestTheObservationPlanCarriesTheRoundsAndReplansWhenTheyChange(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	_, before := r.c.observation()
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20))
	c := r.memory("eu").Campaign
	p, key := r.c.observation()
	if key == before {
		t.Fatal("a new campaign didn't change the plan's key")
	}
	if len(p.Rounds) != 1 || p.Rounds[0].Campaign != c.ID || p.Rounds[0].Path != failurePath || p.Rounds[0].Incumbent != r.ref("a") || !slices.Equal(p.Rounds[0].Rivals, []domain.ServerRef{r.ref("b")}) {
		t.Fatalf("rounds: %+v", p.Rounds)
	}
	s1, s2 := r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20)
	s1.Campaign, s1.Round, s2.Campaign, s2.Round = c.ID, c.Round, c.ID, c.Round
	r.frame(domain.WANUp, s1, s2)
	next, nextKey := r.c.observation()
	if nextKey == key || len(next.Rounds) != 1 || next.Rounds[0].Round != 2 || !next.Rounds[0].Due.Equal(r.clock.Add(r.c.timings.FailureSpacing)) {
		t.Fatalf("next round: %+v", next.Rounds)
	}
	r.setExit("eu", domain.ServerExit("a"))
	if p, _ := r.c.observation(); len(p.Rounds) != 1 || p.Rounds[0].Campaign != c.ID {
		t.Fatalf("the campaign should stand until decide sees the edit: %+v", p.Rounds)
	}
	r.frame(domain.WANUp)
	if p, _ := r.c.observation(); slices.ContainsFunc(p.Rounds, func(q roundRequest) bool { return q.Campaign == c.ID }) {
		t.Fatalf("a campaign that lost a rival left its round in the plan: %+v", p.Rounds)
	}
}

func TestABlockedSlotWithdrawsItsRound(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20))
	if p, _ := r.c.observation(); len(p.Rounds) != 1 {
		t.Fatalf("rounds: %+v", p.Rounds)
	}
	r.setExit("eu", domain.BlockExit())
	r.settle()
	if p, _ := r.c.observation(); len(p.Rounds) != 0 {
		t.Fatalf("a blocked slot left its round in the plan: %+v", p.Rounds)
	}
}

func TestASlotBackFromBlockWithNothingReadyReadsFailed(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	onGroup := r.c.state.slots["eu"].Slot.Intent.Exit()
	r.setExit("eu", domain.BlockExit())
	r.settle()
	r.reportApplied("eu")
	if view := r.app.Snapshot().Slots[0]; view.Status != "block" || view.DownReason != nil {
		t.Fatalf("blocked: %s, %v", view.Status, view.DownReason)
	}
	r.setExit("eu", onGroup)
	r.settle()
	r.clock = r.clock.Add(time.Minute)
	for range 3 {
		r.clock = r.clock.Add(5 * time.Second)
		r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Failed, 0))
	}
	r.c.publish(r.clock)
	view := r.app.Snapshot().Slots[0]
	if view.Status != "failed" || *view.DownReason != "noUsableServer" {
		t.Errorf("a slot back on its group, with every server dead, reads %s, %s", view.Status, *view.DownReason)
	}
}

// bridgesUntil says the target's passage is a bridge on its own server until deadline.
func bridgesUntil(target gateway.SlotTarget, deadline time.Time) bool {
	_, until, ok := target.Passage.Allows(target.Selection)
	return ok && until.Equal(deadline) && target.Passage == gateway.BridgeUntil(until)
}
