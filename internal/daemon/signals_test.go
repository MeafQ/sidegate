package daemon

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

func TestParseDiagMessage(t *testing.T) {
	msg := make([]byte, diagMsgHeader)
	msg[0] = afInet
	binary.BigEndian.PutUint16(msg[6:], 443)
	copy(msg[24:], []byte{198, 51, 100, 10})
	binary.LittleEndian.PutUint32(msg[64:], 0)
	binary.LittleEndian.PutUint32(msg[68:], 777)
	info := make([]byte, 168)
	binary.LittleEndian.PutUint32(info[24:], 3)      // unacked
	binary.LittleEndian.PutUint32(info[56:], 12000)  // last_ack_recv ms
	binary.LittleEndian.PutUint32(info[68:], 20000)  // rtt us
	binary.LittleEndian.PutUint32(info[72:], 5000)   // rttvar us
	binary.LittleEndian.PutUint32(info[100:], 9)     // total_retrans
	binary.LittleEndian.PutUint64(info[120:], 4096)  // bytes_acked
	binary.LittleEndian.PutUint32(info[136:], 200)   // segs_out
	binary.LittleEndian.PutUint64(info[160:], 12345) // delivery_rate
	attr := make([]byte, 4)
	binary.LittleEndian.PutUint16(attr, uint16(4+len(info)))
	binary.LittleEndian.PutUint16(attr[2:], attrTCPInfo)
	msg = append(append(msg, attr...), info...)

	sock, ok := parseDiagMessage(msg)
	if !ok {
		t.Fatal("not parsed")
	}
	if sock.Remote != netip.MustParseAddrPort("198.51.100.10:443") || sock.Inode != 777 {
		t.Fatalf("socket: %+v", sock)
	}
	if *sock.Unacked != 3 || *sock.LastAckRecv != 12*time.Second || *sock.SRTT != 20*time.Millisecond || *sock.RTTVar != 5*time.Millisecond ||
		*sock.TotalRetrans != 9 || *sock.BytesAcked != 4096 || *sock.SegsOut != 200 || *sock.DeliveryRate != 12345 {
		t.Fatalf("info: %+v", sock)
	}

	short, ok := parseDiagMessage(append(msg[:diagMsgHeader:diagMsgHeader], append(attrShort(40), make([]byte, 40)...)...))
	if !ok || short.Unacked == nil || short.SRTT != nil || short.DeliveryRate != nil {
		t.Fatalf("a short tcp_info must leave later fields absent: %+v", short)
	}
	if _, ok := parseDiagMessage(msg[:10]); ok {
		t.Fatal("a truncated message parsed")
	}
}

func attrShort(n int) []byte {
	attr := make([]byte, 4)
	binary.LittleEndian.PutUint16(attr, uint16(4+n))
	binary.LittleEndian.PutUint16(attr[2:], attrTCPInfo)
	return attr
}

func TestTokenBucket(t *testing.T) {
	b := newBucket(2, 60, t0)
	if !b.take(t0, 1) || !b.take(t0, 1) || b.take(t0, 1) {
		t.Fatal("capacity")
	}
	if !b.take(t0.Add(time.Second), 1) || b.take(t0.Add(time.Second), 1) {
		t.Fatal("refill")
	}
	if b.take(t0.Add(time.Second+500*time.Millisecond), 1) {
		t.Fatal("half a token")
	}
}

func TestWarnedServersNeedAnUnambiguousEndpoint(t *testing.T) {
	a, b, c := ref("a"), ref("b"), ref("c")
	endpoints := map[string][]domain.ServerRef{"198.51.100.10:443": {a}, "198.51.100.20:443": {b, c}}
	log := []byte("x [Info] dial tcp 198.51.100.10:443\n" +
		"y [Warning] failed: dial tcp 198.51.100.20:443: refused\n" +
		"z [Warning] failed: dial tcp 198.51.100.10:443: refused\n")
	got := warnedServers(log, endpoints)
	if len(got) != 1 || got[0] != a {
		t.Fatalf("got %v", got)
	}
}

func testLoop(t *testing.T) *signalLoop {
	t.Helper()
	r := newRig(t, t.TempDir())
	return newSignalLoop(r.writer, nil, "")
}

func TestCounterSuspicionNeedsAStreakOfUplinkOnly(t *testing.T) {
	s := testLoop(t)
	sample := func(up, down uint64) []domain.Signal {
		return s.counted(gateway.Telemetry{Signals: []domain.Signal{{Stamp: domain.Stamp{Epoch: "e", At: t0}, Server: ref("a"), Kind: domain.OutboundCounters, Up: &up, Down: &down}}})
	}
	sample(10, 10)
	sample(10, 10)
	if len(sample(10, 10)) != 0 {
		t.Fatal("a quiet server is suspect")
	}
	sample(20, 10)
	sample(30, 10)
	if got := sample(40, 10); len(got) != 1 || got[0].Verdict != domain.Failed {
		t.Fatalf("no suspicion after a streak: %v", got)
	}
	sample(50, 20)
	if got := sample(60, 20); len(got) != 0 {
		t.Fatalf("a reply did not reset the streak: %v", got)
	}
}

func TestWANJudgment(t *testing.T) {
	s := testLoop(t)
	s.now = func() time.Time { return t0 }
	s.judgeWAN(false)
	if s.wan.State != domain.WANDown {
		t.Fatalf("with nothing known, one failure says down: wan %d", s.wan.State)
	}
	s.judgeWAN(true)
	if s.wan.State != domain.WANUp {
		t.Fatalf("wan %d", s.wan.State)
	}
	s.judgeWAN(false)
	if s.wan.State != domain.WANUp {
		t.Fatal("one failed baseline called the WAN down")
	}
	s.judgeWAN(true)
	s.judgeWAN(false)
	if s.wan.State != domain.WANUp {
		t.Fatal("a failure after a success counted the one before it")
	}
	s.judgeWAN(false)
	if s.wan.State != domain.WANDown {
		t.Fatal("two failed baselines in a row left the WAN up")
	}
	s.wan.lastProxy = t0.Add(-time.Second)
	s.judgeWAN(false)
	if s.wan.State != domain.WANUp {
		t.Fatal("a recent proxied success did not prove the WAN")
	}
}

// A baseline that failed is asked again in seconds, not at the next 15 s tick.
func TestAFailedBaselineIsAskedAgainSoon(t *testing.T) {
	slowTest(t)
	var asked atomic.Int32
	baseline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if asked.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer baseline.Close()
	s := testLoop(t)
	s.now = time.Now
	s.baselines = []probeTarget{{URL: baseline.URL}}
	s.wan.State = domain.WANUp
	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan signalFrame, 64)
	done := make(chan error, 1)
	go func() { done <- s.run(ctx, make(chan observationPlan), nil, nil, frames) }()
	defer func() {
		cancel()
		<-done
	}()
	limit := time.After(baselineRetry + time.Second)
	for asked.Load() < 2 {
		select {
		case f := <-frames:
			if f.WAN.State == domain.WANDown {
				t.Fatal("one failed baseline called the WAN down")
			}
		case <-limit:
			t.Fatalf("asked %d times", asked.Load())
		}
	}
}

func TestProbeJudgesThroughARoute(t *testing.T) {
	slowTest(t)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusBadGateway)
		}
		io.WriteString(w, "ok")
	}))
	defer web.Close()
	baseline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s := testLoop(t)
	s.baselines = []probeTarget{{URL: baseline.URL}}
	s.dial = func(_ gateway.ProbeRoute, ctx context.Context, network, address string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}
	route := gateway.ProbeRoute{Server: ref("a"), Epoch: "e1"}
	ctx := context.Background()

	if got := s.probe(ctx, route, probeTarget{ID: "t", URL: web.URL + "/"}, domain.Confirmation, s.wanAlone); got.Verdict != domain.Succeeded || got.Ping == nil || got.NewConn == nil || got.Target != "t" {
		t.Fatalf("success: %+v", got)
	}
	if got := s.probe(ctx, route, probeTarget{ID: "t", URL: web.URL + "/bad"}, domain.Confirmation, s.wanAlone); got.Verdict != domain.Failed {
		t.Fatalf("a 502 with the WAN up: %+v", got)
	}
	baseline.Close()
	time.Sleep(baselineReuse + 100*time.Millisecond)
	if got := s.probe(ctx, route, probeTarget{ID: "t", URL: web.URL + "/bad"}, domain.Confirmation, s.wanAlone); got.Verdict != domain.Unknown || !got.WANExcluded {
		t.Fatalf("a failure with the WAN down must blame nobody: %+v", got)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := s.probe(cancelled, route, probeTarget{ID: "t", URL: web.URL + "/"}, domain.Confirmation, s.wanAlone); got.Verdict != domain.Unknown || got.WANExcluded {
		t.Fatalf("a withdrawn route must blame nobody: %+v", got)
	}
	s.dial = func(gateway.ProbeRoute, context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("refused")
	}
	if got := s.probe(ctx, route, probeTarget{ID: "t", URL: web.URL + "/"}, domain.Confirmation, s.wanAlone); got.Verdict != domain.Unknown || got.WANExcluded {
		t.Fatalf("a core that can't be reached must blame nobody: %+v", got)
	}
}

// blocked gives a probe a context whose probes end at once.
func blocked(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func TestABlockedBaselineStillBlamesAServerWhileARivalWorks(t *testing.T) {
	slowTest(t)
	s := testLoop(t)
	s.now = time.Now
	s.timeout = time.Second
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer dead.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer good.Close()
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer blocked.Close()
	s.baselines = []probeTarget{{URL: blocked.URL}}
	s.dial = func(route gateway.ProbeRoute, ctx context.Context, network, _ string) (net.Conn, error) {
		address := dead.Listener.Addr().String()
		if route.Server == ref("b") {
			address = good.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	target := probeTarget{ID: "t", URL: "http://probe.invalid/"}
	probe := func(id string) domain.Signal {
		return s.probe(context.Background(), gateway.ProbeRoute{Server: ref(id), Epoch: "e1"}, target, domain.Heartbeat, s.wanAlone)
	}

	// Every server failing is an outage: no one's fault, however recent the last success.
	if sig := probe("a"); sig.Verdict != domain.Unknown || !sig.WANExcluded {
		t.Fatalf("alone, with no proof: %+v", sig)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); probe("b") }()
	sig := probe("a")
	wg.Wait()
	if sig.Verdict != domain.Failed || sig.WANExcluded {
		t.Fatalf("with a rival succeeding during it: %+v", sig)
	}
	if sig := probe("a"); sig.Verdict != domain.Unknown || !sig.WANExcluded {
		t.Fatalf("a success before this probe proved something: %+v", sig)
	}

	// Baselines that answered moments ago mean an outage has just begun, so a rival's
	// success during this probe may be from before it.
	s.baselineGood.Store(time.Now().UnixNano())
	wg.Add(1)
	go func() { defer wg.Done(); probe("b") }()
	sig = probe("a")
	wg.Wait()
	if sig.Verdict != domain.Unknown || !sig.WANExcluded {
		t.Fatalf("an outage just begun was blamed on the server: %+v", sig)
	}
}

func TestABadTargetIsOneServersPassElsewhereWhileFailingOnIt(t *testing.T) {
	a, b := ref("a"), ref("b")
	censored := func(h *targetHealth, rounds int) {
		for i := range rounds {
			server := []domain.ServerRef{a, b}[i%2]
			h.failed("bad", server)
			h.succeeded("good", server)
		}
	}

	t.Run("failures that later passes elsewhere explain blame the target", func(t *testing.T) {
		var h targetHealth
		censored(&h, badTargetFails-1)
		if h.blamed("bad") {
			t.Fatalf("blamed after %d failures", badTargetFails-1)
		}
		censored(&h, 1)
		if !h.failed("bad", a) {
			t.Fatal("still blaming servers for a target that fails on all of them while they pass elsewhere")
		}
		h.succeeded("bad", a)
		if h.failed("bad", b) {
			t.Fatal("a target that came back is still blamed")
		}
	})

	t.Run("servers that passed once and then all went silent blame no target", func(t *testing.T) {
		var h targetHealth
		for _, server := range []domain.ServerRef{a, b} {
			h.succeeded("t1", server)
			h.succeeded("t2", server)
		}
		for i := range 40 {
			server := []domain.ServerRef{a, b}[i%2]
			if h.failed([]domain.TargetID{"t1", "t2"}[i%4/2], server) {
				t.Fatalf("blamed a target on failure %d, with every server failing every target", i+1)
			}
		}
	})

	t.Run("one dead server fails every target, however many failures", func(t *testing.T) {
		var h targetHealth
		for range 20 {
			if h.failed("bad", a) {
				t.Fatal("blamed the target for one dead server")
			}
			h.succeeded("good", a)
			if h.blamed("bad") {
				t.Fatal("one server's failures blamed the target")
			}
		}
	})

	t.Run("another server's pass elsewhere explains nothing", func(t *testing.T) {
		var h targetHealth
		for i := range 40 {
			h.failed("bad", []domain.ServerRef{a, b}[i%2])
			h.succeeded("good", ref("live"))
		}
		if h.blamed("bad") {
			t.Fatal("dead servers blamed a target because a third server is alive")
		}
	})

	t.Run("a pass on the target itself clears it", func(t *testing.T) {
		var h targetHealth
		for i := range 40 {
			server := []domain.ServerRef{a, b}[i%2]
			h.failed("flaky", server)
			h.succeeded("flaky", ref("live"))
			h.succeeded("good", server)
		}
		if h.blamed("flaky") {
			t.Fatal("a target that passes for a live server was blamed")
		}
	})
}

// A blamed target can't judge anything, so a round doesn't spend itself on it.
func TestARoundAsksATargetThatCanAnswer(t *testing.T) {
	s := testLoop(t)
	s.plan = observationPlan{Targets: []probeTarget{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}}}
	if got := s.roundTarget(1).ID; got != "t2" {
		t.Fatalf("round 1 asks %s", got)
	}
	blame := func(id domain.TargetID) {
		for i := range badTargetFails {
			server := []domain.ServerRef{ref("a"), ref("b")}[i%2]
			s.targets.failed(id, server)
			s.targets.succeeded("elsewhere", server)
		}
	}
	blame("t2")
	if got := s.roundTarget(1).ID; got != "t3" {
		t.Fatalf("round 1 asks %s with t2 blamed", got)
	}
	if got := s.roundTarget(2).ID; got != "t3" {
		t.Fatalf("round 2 asks %s", got)
	}
	blame("t1")
	blame("t3")
	if got := s.roundTarget(1).ID; got != "t2" {
		t.Fatalf("with every target blamed round 1 asks %s", got)
	}
}

// roundBench runs a signal loop's scheduling by hand against dials a test controls.
type roundBench struct {
	t       *testing.T
	s       *signalLoop
	ctx     context.Context
	wg      sync.WaitGroup
	results chan domain.Signal
	rounds  chan roundDone
	now     time.Time
	dials   chan domain.ServerRef
	release chan struct{}
}

func newRoundBench(t *testing.T) *roundBench {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b := &roundBench{t: t, s: testLoop(t), ctx: ctx, results: make(chan domain.Signal, 64), rounds: make(chan roundDone, 8), now: t0,
		dials: make(chan domain.ServerRef, 64), release: make(chan struct{})}
	b.s.now = func() time.Time { return b.now }
	b.s.baselines = nil
	b.s.timeout = time.Second
	b.s.dial = func(route gateway.ProbeRoute, ctx context.Context, _, _ string) (net.Conn, error) {
		b.dials <- route.Server
		select {
		case <-b.release:
		case <-ctx.Done():
		}
		return nil, ctx.Err()
	}
	t.Cleanup(func() {
		cancel()
		b.wg.Wait()
	})
	b.adopt()
	return b
}

func (b *roundBench) plan(rounds ...roundRequest) observationPlan {
	p := observationPlan{Topology: gateway.Topology{Epoch: "e1"}, Sources: map[domain.ServerRef]domain.SourceID{}, Beats: map[domain.ServerRef]time.Duration{},
		Targets: []probeTarget{{ID: "t1", URL: "http://probe.invalid/"}}, Budget: budget, Rounds: rounds}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		p.Topology.Probes = append(p.Topology.Probes, gateway.ProbeRoute{Server: ref(id), Epoch: "e1"})
	}
	return p
}

func (b *roundBench) adopt(rounds ...roundRequest) {
	b.s.adopt(b.ctx, b.plan(rounds...))
	b.s.schedule(b.ctx, &b.wg, b.results, b.rounds)
}

// dialed waits for one dial of each server named, in any order.
func (b *roundBench) dialed(ids ...string) {
	b.t.Helper()
	want := map[domain.ServerRef]int{}
	for _, id := range ids {
		want[ref(id)]++
	}
	for range ids {
		select {
		case got := <-b.dials:
			if want[got]--; want[got] < 0 {
				b.t.Fatalf("too many dials of %s", got.Lineage)
			}
		case <-time.After(2 * time.Second):
			b.t.Fatalf("still waiting for dials: %v", want)
		}
	}
}

func (b *roundBench) noDial() {
	b.t.Helper()
	select {
	case got := <-b.dials:
		b.t.Fatalf("unexpected dial of %s", got.Lineage)
	case <-time.After(50 * time.Millisecond):
	}
}

func failureRound(campaign domain.CampaignID, incumbent string, rivals ...string) roundRequest {
	q := roundRequest{Campaign: campaign, Round: 1, Path: failurePath, Due: t0, Incumbent: ref(incumbent)}
	for _, r := range rivals {
		q.Rivals = append(q.Rivals, ref(r))
	}
	return q
}

func improveRound(campaign domain.CampaignID, incumbent, rival string) roundRequest {
	q := failureRound(campaign, incumbent, rival)
	q.Path = improvePath
	return q
}

func TestAWithdrawnQueuedRoundNeverRunsAndAWithdrawnRunningOneIsCancelled(t *testing.T) {
	b := newRoundBench(t)
	running, later := failureRound("running", "a"), failureRound("later", "c")
	later.Due = t0.Add(time.Minute)
	b.adopt(running, later)
	b.dialed("a")
	b.noDial()
	if q := b.s.lanes[failurePath].queued; len(q) != 1 || q[0].Campaign != "later" {
		t.Fatalf("a round not yet due should wait: %+v", q)
	}

	b.adopt(running)
	if len(b.s.lanes[failurePath].queued) != 0 {
		t.Fatal("a withdrawn queued round stayed queued")
	}

	job := b.s.lanes[failurePath].running[0]
	b.adopt()
	select {
	case r := <-b.rounds:
		if r.job != job || b.s.finished(r) {
			t.Fatalf("a withdrawn round's results were kept: %+v", r.job)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a withdrawn running round wasn't cancelled")
	}
	if len(b.s.lanes[failurePath].running) != 0 {
		t.Fatalf("running %d", len(b.s.lanes[failurePath].running))
	}

	next := failureRound("next", "d")
	b.adopt(next)
	b.dialed("d")
}

func TestARoundRunsOnceUntilItLeavesThePlan(t *testing.T) {
	slowTest(t)
	b := newRoundBench(t)
	q := failureRound("c", "a")
	b.adopt(q)
	b.dialed("a")
	close(b.release)
	r := <-b.rounds
	if !b.s.finished(r) {
		t.Fatal("a wanted round's results were dropped")
	}
	signals := r.job.answers(r.signals)
	if len(signals) != 1 || signals[0].Campaign != "c" || signals[0].Round != 1 {
		t.Fatalf("results: %+v", signals)
	}
	b.release = make(chan struct{})
	b.adopt(q)
	b.noDial()
	following := q
	following.Round = 2
	b.adopt(following)
	b.dialed("a")
}

// One dead server is one failure, whatever groups its slots use. They measure each server once, and
// each hears only about its own.
func TestCampaignsDueTogetherShareOneMeasurementPerServer(t *testing.T) {
	slowTest(t)
	b := newRoundBench(t)
	one, two, three := failureRound("one", "a", "b", "c", "d"), failureRound("two", "a", "b", "c", "e"), failureRound("three", "a", "b", "d", "e")
	b.adopt(one, two, three)
	b.dialed("a", "b", "c", "d", "e")
	b.noDial()
	if got := len(b.s.lanes[failurePath].running); got != 1 {
		t.Fatalf("%d jobs", got)
	}
	b.adopt(two, three)
	close(b.release)
	r := <-b.rounds
	if !b.s.finished(r) {
		t.Fatal("a wanted round's results were dropped")
	}
	heard := map[domain.CampaignID][]domain.LineageID{}
	for _, sig := range r.job.answers(r.signals) {
		heard[sig.Campaign] = append(heard[sig.Campaign], sig.Server.Lineage)
	}
	slices.Sort(heard["two"])
	slices.Sort(heard["three"])
	if len(heard) != 2 || !slices.Equal(heard["two"], []domain.LineageID{"a", "b", "c", "e"}) || !slices.Equal(heard["three"], []domain.LineageID{"a", "b", "d", "e"}) {
		t.Fatalf("heard: %v", heard)
	}
}

func TestDifferentRoundNumbersMeasureApart(t *testing.T) {
	b := newRoundBench(t)
	first, second := failureRound("first", "a", "b"), failureRound("second", "a", "b")
	second.Round = 2
	b.adopt(first, second)
	b.dialed("a", "b", "a", "b")
	if got := len(b.s.lanes[failurePath].running); got != 2 {
		t.Fatalf("%d jobs", got)
	}
}

// A round something has proven spends nothing, however empty the bucket, and only a round no failure
// has proven waits for it.
func TestOnlyARoundNoFailureHasProvenSpendsTheBucket(t *testing.T) {
	b := newRoundBench(t)
	lane := &b.s.lanes[failurePath]
	passive := failureRound("passive", "a", "b", "c", "d")
	passive.Passive = true
	real := failureRound("real", "e", "f")

	lane.bucket.tokens = 3
	b.adopt(passive, real)
	b.dialed("e", "f")
	b.noDial()
	if q := lane.queued; len(q) != 1 || q[0].Campaign != "passive" {
		t.Fatalf("queued: %+v", q)
	}
	if got := lane.bucket.tokens; got != 3 {
		t.Fatalf("a proven round spent %v tokens", 3-got)
	}

	lane.bucket.tokens, lane.bucket.last = 4, b.now
	b.s.schedule(b.ctx, &b.wg, b.results, b.rounds)
	b.dialed("a", "b", "c", "d")
	if got := lane.bucket.tokens; got != 0 {
		t.Fatalf("tokens %v", got)
	}
}

// A suspicion's round waiting for the bucket goes at once when the server really fails.
func TestAQueuedPassiveRoundTheServerFailsGoesAhead(t *testing.T) {
	b := newRoundBench(t)
	lane := &b.s.lanes[failurePath]
	q := failureRound("c", "a", "b", "c", "d")
	q.Passive = true
	lane.bucket.tokens = 0
	b.adopt(q)
	b.noDial()
	q.Passive = false
	b.adopt(q)
	b.dialed("a", "b", "c", "d")
}

func TestAPassiveRoundJoiningARealOneCostsNothing(t *testing.T) {
	b := newRoundBench(t)
	lane := &b.s.lanes[failurePath]
	passive := failureRound("passive", "a", "b")
	passive.Passive = true
	lane.bucket.tokens = 0
	b.adopt(failureRound("real", "a", "b"), passive)
	b.dialed("a", "b")
	if len(lane.queued) != 0 || len(lane.running[0].wants) != 2 {
		t.Fatalf("queued %+v, running %+v", lane.queued, lane.running)
	}
}

// A round whose only rival has no route could only come back inconclusive, and one route appearing
// starts it.
func TestARoundWaitsForARivalsRoute(t *testing.T) {
	b := newRoundBench(t)
	q := failureRound("c", "a", "b")
	q.Rivals = []domain.ServerRef{ref("rotated")}
	b.adopt(q)
	b.noDial()
	if len(b.s.lanes[failurePath].queued) != 1 {
		t.Fatal("the round left the queue")
	}
	p := b.plan(q)
	p.Topology.Probes = append(p.Topology.Probes, gateway.ProbeRoute{Server: ref("rotated"), Epoch: "e1"})
	b.s.adopt(b.ctx, p)
	b.s.schedule(b.ctx, &b.wg, b.results, b.rounds)
	b.dialed("a", "rotated")
}

func TestAnImprovementRoundHoldsNoFailureCapacityAndWaitsForTheFailureQueue(t *testing.T) {
	b := newRoundBench(t)
	improve := improveRound("improve", "a", "b")
	b.adopt(improve)
	b.dialed("a", "b")

	failures := []roundRequest{failureRound("f1", "c"), failureRound("f2", "d")}
	b.adopt(append(failures, improve)...)
	b.dialed("c", "d")

	waiting := failureRound("waiting", "f")
	waiting.Due = t0.Add(time.Minute)
	free := improve
	free.Campaign = "free"
	b.s.lanes[improvePath].capacity = 2
	b.adopt(append(failures, improve, waiting, free)...)
	b.noDial()
	if got := len(b.s.lanes[improvePath].queued); got != 1 {
		t.Fatalf("an improvement round started over a queued failure round: %d queued", got)
	}
}

func TestLanesSpendTheirOwnBudgets(t *testing.T) {
	b := newRoundBench(t)
	b.s.lanes[improvePath].capacity = 100
	var requests []roundRequest
	for i := range 4 {
		q := improveRound(domain.CampaignID(rune('a'+i)), "a", "b")
		q.Round = uint8(i + 1)
		requests = append(requests, q)
	}
	b.adopt(requests...)
	b.dialed("a", "b", "a", "b", "a", "b")
	b.noDial()
	if got := len(b.s.lanes[improvePath].queued); got != 1 {
		t.Fatalf("a bucket of 6 should run 3 rounds of 2: %d left queued", got)
	}
	failure := failureRound("failure", "c", "d", "e", "f")
	b.adopt(append(requests, failure)...)
	b.dialed("c", "d", "e", "f")
}

// A serving heartbeat that waited has lost that wait from the 30 s, so it waits for neither a worker
// nor the budget, even when a closed slot's watch has given it a closed slot's period.
func TestAServingHeartbeatWaitsForNoWorkerAndNoBudget(t *testing.T) {
	slowTest(t)
	for _, every := range []time.Duration{heartbeatServing, heartbeatClosed} {
		b := newRoundBench(t)
		p := b.plan()
		p.Beats = map[domain.ServerRef]time.Duration{ref("a"): heartbeatSweep, ref("d"): every}
		p.Serving = map[domain.ServerRef]bool{ref("d"): true}
		b.s.adopt(b.ctx, p)
		b.s.inFlight = p.Budget.Parallel
		b.s.beats.tokens, b.s.beats.last, b.s.beats.perSecond = 0, b.now, 0
		b.s.schedule(b.ctx, &b.wg, b.results, b.rounds)
		b.dialed("d")
		b.noDial()
		if got := b.s.nextBeat[ref("d")].Sub(b.now); got != every {
			t.Fatalf("next heartbeat in %s", got)
		}
	}
}

// A server that answered again goes ahead of servers long overdue, or they would keep it waiting a
// whole sweep for each of the successes it needs.
func TestAServerThatAnsweredAgainGoesAheadOfTheSweep(t *testing.T) {
	b := newRoundBench(t)
	p := b.plan()
	p.Beats = map[domain.ServerRef]time.Duration{ref("a"): heartbeatSweep, ref("b"): heartbeatClosed}
	b.s.adopt(b.ctx, p)
	b.s.nextBeat[ref("a")] = b.now.Add(-time.Minute)
	if got, ok := b.s.pickBeat(b.now, true); !ok || got != ref("b") {
		t.Fatalf("picked %v, %v", got, ok)
	}
}

// The longest overdue goes first, or the servers ahead of it in order would come due again before the
// workers ever reached it.
func TestASweepReachesEveryServerHoweverManyAreDead(t *testing.T) {
	b := newRoundBench(t)
	p := b.plan()
	p.Budget.Parallel = 2
	p.Beats = map[domain.ServerRef]time.Duration{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		p.Beats[ref(id)] = heartbeatClosed
	}
	b.s.adopt(b.ctx, p)
	seen := map[domain.LineageID]int{}
	for now := t0; now.Before(t0.Add(probeTimeout * 3)); now = now.Add(probeTimeout) {
		b.s.inFlight = 0
		for range p.Budget.Parallel {
			got, ok := b.s.pickBeat(now, true)
			if !ok {
				t.Fatalf("no heartbeat at %s", now.Sub(t0))
			}
			seen[got.Lineage]++
			b.s.inFlight++
		}
	}
	if len(seen) != 6 {
		t.Fatalf("three rounds of two workers reached %v", seen)
	}
}

// A silent server with a hung baseline, under the real constants. Serial probes and a baseline after
// the probe's deadline would take 7 s.
func TestARoundEndsAtOneDeadlineWithTheBaselineBesideIt(t *testing.T) {
	slowTest(t)
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hung.Close()
	s := testLoop(t)
	s.baselines = []probeTarget{{URL: hung.URL}}
	s.baselineGood.Store(time.Now().UnixNano())
	s.dial = func(_ gateway.ProbeRoute, ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p := observationPlan{Topology: gateway.Topology{Epoch: "e1"}, Targets: []probeTarget{{ID: "t1", URL: "http://probe.invalid/"}}, Budget: budget}
	for _, id := range []string{"a", "b", "c", "d"} {
		p.Topology.Probes = append(p.Topology.Probes, gateway.ProbeRoute{Server: ref(id), Epoch: "e1"})
	}
	p.Rounds = []roundRequest{{Campaign: "c", Round: 1, Path: failurePath, Due: time.Now(), Incumbent: ref("a"), Rivals: []domain.ServerRef{ref("b"), ref("c"), ref("d")}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.adopt(ctx, p)
	results, rounds := make(chan domain.Signal, 1), make(chan roundDone, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	started := time.Now()
	s.schedule(ctx, &wg, results, rounds)
	select {
	case r := <-rounds:
		if took := time.Since(started); took > probeTimeout+500*time.Millisecond {
			t.Fatalf("a round took %s", took)
		}
		for _, sig := range r.signals {
			if sig.Verdict != domain.Unknown || !sig.WANExcluded {
				t.Fatalf("the baseline's answer wasn't read: %+v", sig)
			}
		}
	case <-time.After(probeTimeout + 2*time.Second):
		t.Fatal("no round result")
	}
}

// The baseline blips while the WAN works, so the rival passing beside the silent server says whose
// fault it is. Baselines that answered moments ago make no difference to a round.
func TestARivalPassingInTheRoundProvesTheWANThroughABaselineBlip(t *testing.T) {
	slowTest(t)
	good := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer good.Close()
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hung.Close()
	s := testLoop(t)
	s.now = time.Now
	s.timeout = 500 * time.Millisecond
	s.baselines = []probeTarget{{URL: hung.URL}}
	s.direct.ResponseHeaderTimeout = 200 * time.Millisecond
	s.baselineGood.Store(time.Now().UnixNano())
	s.dial = func(route gateway.ProbeRoute, ctx context.Context, network, _ string) (net.Conn, error) {
		if route.Server == ref("a") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, good.Listener.Addr().String())
	}
	p := observationPlan{Topology: gateway.Topology{Epoch: "e1", Probes: []gateway.ProbeRoute{{Server: ref("a"), Epoch: "e1"}, {Server: ref("b"), Epoch: "e1"}}},
		Targets: []probeTarget{{ID: "t1", URL: "http://probe.invalid/"}}, Budget: budget,
		Rounds: []roundRequest{{Campaign: "c", Round: 1, Path: failurePath, Due: time.Now(), Incumbent: ref("a"), Rivals: []domain.ServerRef{ref("b")}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.adopt(ctx, p)
	results, rounds := make(chan domain.Signal, 1), make(chan roundDone, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	s.schedule(ctx, &wg, results, rounds)
	select {
	case r := <-rounds:
		if got := r.signals[0]; got.Verdict != domain.Failed || got.WANExcluded {
			t.Fatalf("a silent server beside a passing rival: %+v", got)
		}
		if got := r.signals[1]; got.Verdict != domain.Succeeded {
			t.Fatalf("the rival: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no round result")
	}
}

// A baseline that answered at once is the round's answer. Asking again after the deadline would add
// another baseline's wait to the round.
func TestAFailingProbeReadsTheBaselineThatRanBesideItsRound(t *testing.T) {
	slowTest(t)
	var asked atomic.Int32
	baseline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1) > 1 {
			<-r.Context().Done()
		}
	}))
	defer baseline.Close()
	s := testLoop(t)
	s.baselines = []probeTarget{{URL: baseline.URL}}
	s.dial = func(_ gateway.ProbeRoute, ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p := observationPlan{Topology: gateway.Topology{Epoch: "e1", Probes: []gateway.ProbeRoute{{Server: ref("a"), Epoch: "e1"}}},
		Targets: []probeTarget{{ID: "t1", URL: "http://probe.invalid/"}}, Budget: budget,
		Rounds: []roundRequest{{Campaign: "c", Round: 1, Path: failurePath, Due: time.Now(), Incumbent: ref("a")}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.adopt(ctx, p)
	results, rounds := make(chan domain.Signal, 1), make(chan roundDone, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	started := time.Now()
	s.schedule(ctx, &wg, results, rounds)
	select {
	case r := <-rounds:
		if took := time.Since(started); took > probeTimeout+500*time.Millisecond {
			t.Fatalf("a round took %s", took)
		}
		if got := r.signals[0]; got.Verdict != domain.Failed || got.WANExcluded {
			t.Fatalf("a silent server with the WAN proven: %+v", got)
		}
	case <-time.After(probeTimeout + baselineTimeout + 2*time.Second):
		t.Fatal("no round result")
	}
}

// The loop is real here: plans arrive on its channel and only its timer can start the next round.
func TestTheLoopStartsARoundAtItsDueWithNoTickOfLag(t *testing.T) {
	slowTest(t)
	s := testLoop(t)
	dials := make(chan time.Time, 8)
	s.dial = func(_ gateway.ProbeRoute, ctx context.Context, _, _ string) (net.Conn, error) {
		dials <- time.Now()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s.baselines = []probeTarget{{URL: "http://127.0.0.1:1/"}}
	plans := make(chan observationPlan, 1)
	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan signalFrame, 64)
	done := make(chan error, 1)
	go func() { done <- s.run(ctx, plans, nil, nil, frames) }()
	defer func() {
		cancel()
		<-done
	}()
	p := observationPlan{Topology: gateway.Topology{Epoch: "e1", Probes: []gateway.ProbeRoute{{Server: ref("a"), Epoch: "e1"}}},
		Targets: []probeTarget{{ID: "t1", URL: "http://probe.invalid/"}}, Budget: budget}
	due := time.Now().Add(300 * time.Millisecond)
	p.Rounds = []roundRequest{{Campaign: "c", Round: 2, Path: failurePath, Due: due, Incumbent: ref("a")}}
	plans <- p
	select {
	case at := <-dials:
		if early := due.Sub(at); early > 0 {
			t.Fatalf("started %s early", early)
		}
		if late := at.Sub(due); late > 200*time.Millisecond {
			t.Fatalf("started %s after its due", late)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the round never started")
	}
}

// A round pays its bucket for every server it measures, and a server two rounds of one job share once.
// Rounds hold only the servers that can win them, so a round of four and one of three fit the bucket
// together and both run at once.
func TestARoundPaysForEveryServerItMeasures(t *testing.T) {
	b := newRoundBench(t)
	big := failureRound("big", "a", "b", "c", "d")
	big.Passive = true
	small := failureRound("small", "e", "f", "d")
	small.Passive = true
	p := b.plan(big, small)
	b.s.adopt(b.ctx, p)
	b.s.schedule(b.ctx, &b.wg, b.results, b.rounds)
	b.dialed("a", "b", "c", "d", "e", "f")
	b.noDial()
	if got, want := b.s.lanes[failurePath].bucket.tokens, float64(budget.SuspicionBurst-6); got != want {
		t.Fatalf("the bucket holds %g tokens, want %g", got, want)
	}
}
