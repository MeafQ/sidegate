package daemon

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sidegate/internal/domain"
	"sidegate/internal/metrics"
	gateway "sidegate/internal/runtime"
)

// probeTarget is one approved public URL. A provider never supplies one.
type probeTarget struct {
	ID             domain.TargetID
	URL            string
	ExpectedStatus int // Zero accepts anything below 500: the point is reaching it.
	MaxBytes       int64
}

// probeBudget is how much probing the plan may cost. Probes per slot or per server
// never exceed these, however many servers have routes.
type probeBudget struct {
	Parallel              int // Closed heartbeats, checks and UDP checks at once. A serving heartbeat doesn't wait for one.
	AttemptsPerMinute     int // UDP checks, across the gateway; past it, a check waits.
	HeartbeatsPerMinute   int // Across every closed slot's near and swept servers.
	SuspicionsPerMinute   int // Refill of what rounds only a passive suspicion backs may spend.
	SuspicionBurst        int // What those rounds may spend at once. A proven failure round spends none of it.
	ImprovementsPerMinute int // Refill and size of the improvement lane's own bucket.
}

const (
	probeTimeout         = 4 * time.Second
	baselineTimeout      = 3 * time.Second
	baselineEvery        = 15 * time.Second
	baselineRetry        = 2 * time.Second  // After a baseline that failed, so a blip reads down, and back up, in seconds. As long as baselineReuse, or it would hear the same answer.
	baselineBlockedAfter = 60 * time.Second // Silence this long means blocked: an outage shows no rival succeeding either.
	passiveEvery         = 5 * time.Second
	passiveRecord        = time.Minute
	baselineReuse        = 2 * time.Second
	proofOfWAN           = 5 * time.Second  // A proxied success this recent proves the WAN whatever the baseline says.
	improveCapacity      = 1                // Improvement rounds at once. The failure lane has no such bound.
	counterStreak        = 3                // Samples of uplink without downlink before suspicion, 15 s at 5 s each.
	testRouteWait        = 15 * time.Second // How long a Test waits for the route the core is given for it.
)

// observationPlan is what signals may measure, published whole by the controller.
type observationPlan struct {
	Generation uint64
	Topology   gateway.Topology
	Sources    map[domain.ServerRef]domain.SourceID
	Beats      map[domain.ServerRef]time.Duration // Heartbeat period of each server that gets one.
	Checks     map[domain.ServerRef]time.Time     // When each server's next check is due, on the ranking target.
	Serving    map[domain.ServerRef]bool          // What a slot is on now, whose heartbeats wait for nothing.
	Endpoints  map[string][]domain.ServerRef      // "host:port" of each root, for the warning log.
	Targets    []probeTarget
	Ranking    domain.TargetID // Where a new loop starts ranking: where the last process ranked. A running loop's judge owns it.
	Budget     probeBudget
	Rounds     []roundRequest                          // The rounds that should exist now, one per running campaign.
	UDPEvery   time.Duration                           // How often each server in Serving has its UDP checked. Zero checks nothing.
	UDPRivals  map[domain.ServerRef][]domain.ServerRef // Who a serving server's failed UDP check is weighed against.
}

// roundRequest asks for one round of one campaign. It stays in the plan until the campaign moves
// on or ends, so a round can't be lost, and leaving the plan is what withdraws it.
type roundRequest struct {
	Campaign  domain.CampaignID
	Round     uint8
	Path      path
	Due       time.Time
	Incumbent domain.ServerRef
	Rivals    []domain.ServerRef
	Target    domain.TargetID // A comparison's one site. Empty lets the round pick its target.
	Passive   bool            // Nothing but a passive suspicion backs it yet, so it spends the failure lane's bucket.
}

// servers is who the round measures: the incumbent and its rivals, each of whom can win it.
func (q roundRequest) servers() []domain.ServerRef {
	return slices.Concat([]domain.ServerRef{q.Incumbent}, q.Rivals)
}

// signalFrame is evidence for the controller. It carries no persistence duty: samples
// are already on their way to metrics.db.
type signalFrame struct {
	Sequence uint64
	Signals  []domain.Signal
	WAN      wanBelief
	Gap      bool
	Ranking  domain.TargetID    // Empty before the loop has its first plan.
	LineBusy bool               // The line's total through our outbounds, as of the last telemetry, is past lineGuard.
	Tests    []domain.ServerRef // Servers a Test waits on or runs on, each of which needs a route until it answers.
}

// manualProbe is one owner-requested probe, answered on reply, capacity 1.
type manualProbe struct {
	Server domain.ServerRef
	Reply  chan domain.Signal
}

// waitingTest is a Test whose server has no route yet.
type waitingTest struct {
	probe manualProbe
	until time.Time
}

// tokenBucket refills continuously up to its capacity.
type tokenBucket struct {
	tokens, capacity float64
	perSecond        float64
	last             time.Time
}

func newBucket(capacity, perMinute int, now time.Time) tokenBucket {
	return tokenBucket{tokens: float64(capacity), capacity: float64(capacity), perSecond: float64(perMinute) / 60, last: now}
}

// take spends n. More than the capacity waits for a full bucket and leaves it owing the rest, so nothing
// is too big to ever run and the rate still holds after it.
func (b *tokenBucket) take(now time.Time, n float64) bool {
	b.tokens = min(b.capacity, b.tokens+now.Sub(b.last).Seconds()*b.perSecond)
	b.last = now
	if b.tokens < min(n, b.capacity) {
		return false
	}
	b.tokens -= n
	return true
}

// signalLoop is the one owner of observation windows, the probe schedule, token
// buckets, and attribution. It records every sample worth keeping to metrics.db and
// sends the controller only what selection needs. Probes run on goroutines it starts
// and counts, so its capacity is explicit and nothing runs per item.
type signalLoop struct {
	sink         *metrics.Writer
	baselines    []probeTarget
	warnLog      string
	dump         func(families []uint8) ([]tcpSocket, error)
	socketsOf    func(pid int) (map[uint32]bool, error)
	dial         func(route gateway.ProbeRoute, ctx context.Context, network, address string) (net.Conn, error)
	direct       *http.Transport
	now          func() time.Time
	timeout      time.Duration
	targets      targetHealth
	baselineMu   sync.Mutex
	proxied      atomic.Int64 // When a probe last succeeded, in Unix nanoseconds.
	baselineGood atomic.Int64 // When a baseline last answered, in Unix nanoseconds.
	baselineAt   time.Time
	baselineOK   bool

	plan       observationPlan
	routes     map[domain.ServerRef]gateway.ProbeRoute
	sequence   uint64
	gap        bool
	wan        wanBelief
	nextBeat   map[domain.ServerRef]time.Time
	targetTurn map[domain.ServerRef]int
	// The ranking target is the one site whose pings compare like with like.
	ranking       domain.TargetID
	rankingMisses int
	inFlight      int
	routeStops    map[domain.ServerRef]routeStop // Withdrawing a route cancels its probes.
	lanes         [pathCount]roundLane
	started       map[roundKey]bool // What the plan asked for and has a job, until it leaves the plan.
	attempts      tokenBucket
	beats         tokenBucket
	counters      map[domain.ServerRef]*counterWindow
	lineAt        time.Time // The telemetry sample the line's total was last read from.
	lineBusy      bool
	sockets       map[uint32]socketMark
	passive       map[domain.ServerRef]*passiveWindow
	logOffset     int64
	checked       map[domain.ServerRef]time.Time // When each check due in the plan started, until the plan wants one due after it.
	nextUDP       map[domain.ServerRef]time.Time
	udpPassed     map[domain.ServerRef]time.Time // When each server whose newest UDP check passed passed it.
	udpAsk        map[domain.ServerRef]bool      // Rivals owed one UDP check now, for a failure that waits on them.
	udpHeld       map[domain.ServerRef]heldUDP   // A serving server's failed check, until its rivals answer.
	udpTurn       int
	waiting       []waitingTest
	tests         map[domain.ServerRef]int // Tests waiting or running, by server.
}

type routeStop struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// roundKey names one request. A campaign asks for each of its rounds once.
type roundKey struct {
	campaign domain.CampaignID
	round    uint8
}

// roundLane is one path's queue, workers and budget, so neither path can spend the other's. The
// failure lane has no worker limit, and its bucket rations only rounds no failure has proven.
type roundLane struct {
	queued   []roundRequest
	running  []*roundJob
	capacity int
	bucket   tokenBucket
}

// roundJob is one round's measurements: each server any of its campaigns asks about, once, at once,
// on one target. Campaigns due together share it whatever their groups, so slots on one dead server
// cost what one failure costs.
type roundJob struct {
	round   uint8
	target  domain.TargetID
	wants   []roundRequest
	servers []domain.ServerRef
	cancel  context.CancelFunc // Set once it runs.
}

// keepWanted drops the campaigns the plan no longer asks this job for, and says whether any remain.
func (r *roundJob) keepWanted(wanted map[roundKey]bool) bool {
	r.wants = slices.DeleteFunc(r.wants, func(q roundRequest) bool { return !wanted[roundKey{q.Campaign, q.Round}] })
	return len(r.wants) > 0
}

// answers gives each campaign still waiting its own servers' results.
func (r *roundJob) answers(signals []domain.Signal) []domain.Signal {
	var all []domain.Signal
	for _, q := range r.wants {
		for _, sig := range signals {
			if slices.Contains(q.servers(), sig.Server) {
				sig.Campaign, sig.Round = q.Campaign, q.Round
				all = append(all, sig)
			}
		}
	}
	return all
}

type counterWindow struct {
	up, down uint64
	streak   int
	known    bool
}

// socketMark remembers one upstream socket's last counters for deltas.
type socketMark struct {
	retrans, segs uint32
}

// passiveWindow aggregates tcp_info for metrics.db, a row a minute at most.
type passiveWindow struct {
	since   time.Time
	srtt    time.Duration
	rttvar  time.Duration
	retrans uint64
	rate    uint64
	stalled bool
	certain bool
}

func newSignalLoop(sink *metrics.Writer, baselines []probeTarget, warnLog string) *signalLoop {
	return &signalLoop{
		sink: sink, baselines: baselines, warnLog: warnLog, dump: dumpTCP, socketsOf: coreSockets, dial: gateway.ProbeRoute.Dial,
		direct:   &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: baselineTimeout},
		now:      time.Now,
		timeout:  probeTimeout,
		routes:   map[domain.ServerRef]gateway.ProbeRoute{},
		nextBeat: map[domain.ServerRef]time.Time{}, targetTurn: map[domain.ServerRef]int{},
		routeStops: map[domain.ServerRef]routeStop{}, started: map[roundKey]bool{},
		lanes:    [pathCount]roundLane{failurePath: {capacity: math.MaxInt}, improvePath: {capacity: improveCapacity}},
		counters: map[domain.ServerRef]*counterWindow{}, sockets: map[uint32]socketMark{}, passive: map[domain.ServerRef]*passiveWindow{},
		nextUDP: map[domain.ServerRef]time.Time{}, udpPassed: map[domain.ServerRef]time.Time{}, checked: map[domain.ServerRef]time.Time{},
		udpAsk: map[domain.ServerRef]bool{}, udpHeld: map[domain.ServerRef]heldUDP{}, tests: map[domain.ServerRef]int{},
	}
}

type roundDone struct {
	job     *roundJob
	path    path
	signals []domain.Signal // One per server, with no campaign on them yet.
}

func (s *signalLoop) run(ctx context.Context, plans <-chan observationPlan,
	manual <-chan manualProbe, telemetry <-chan gateway.Telemetry, out chan<- signalFrame) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	nextRound := time.NewTimer(time.Hour)
	defer nextRound.Stop()
	passive := time.NewTicker(passiveEvery)
	defer passive.Stop()
	baseline := time.NewTicker(baselineEvery)
	defer baseline.Stop()
	results := make(chan domain.Signal, 16)
	manuals := make(chan domain.Signal, 4)
	rounds := make(chan roundDone, 4)
	baselines := make(chan bool, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	runBaseline := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok := s.baselineFresh()
			select {
			case baselines <- ok:
			default:
			}
		}()
	}
	runBaseline()
	for {
		select {
		case <-ctx.Done():
			return nil
		case p := <-plans:
			s.adopt(ctx, p)
			if s.startTests(ctx, &wg, manuals) {
				s.emit(out, nil)
			}
			s.schedule(ctx, &wg, results, rounds)
		case <-nextRound.C:
			s.schedule(ctx, &wg, results, rounds)
		case m := <-manual:
			s.manual(ctx, &wg, m, manuals)
			s.emit(out, nil)
		case sig := <-manuals:
			s.tested(sig.Server)
			s.judgeRanking(sig)
			s.emit(out, []domain.Signal{sig})
		case t := <-telemetry:
			s.emit(out, s.counted(t))
		case sig := <-results:
			s.inFlight--
			s.emit(out, s.accepted([]domain.Signal{sig}))
			s.schedule(ctx, &wg, results, rounds)
		case r := <-rounds:
			if s.finished(r) {
				s.emit(out, r.job.answers(s.accepted(r.signals)))
			}
			s.schedule(ctx, &wg, results, rounds)
		case ok := <-baselines:
			s.judgeWAN(ok)
			s.emit(out, nil)
			if ok {
				baseline.Reset(baselineEvery)
			} else {
				baseline.Reset(baselineRetry)
			}
		case <-baseline.C:
			runBaseline()
		case <-tick.C:
			if s.startTests(ctx, &wg, manuals) {
				s.emit(out, nil)
			}
			s.schedule(ctx, &wg, results, rounds)
		case <-passive.C:
			if signals := append(s.sampleSockets(), s.readWarnings()...); len(signals) > 0 {
				s.emit(out, signals)
			}
		}
		nextRound.Stop()
		if wait, ok := s.untilNextRound(s.now()); ok {
			nextRound.Reset(wait)
		}
	}
}

// adopt installs a new plan. A route that left Topology is cancelled before anything
// else, since the runtime removes its user only after the drain.
func (s *signalLoop) adopt(ctx context.Context, p observationPlan) {
	if p.Topology.Epoch != s.plan.Topology.Epoch {
		clear(s.counters)
		clear(s.sockets)
		clear(s.udpPassed)
		clear(s.udpAsk)
		clear(s.udpHeld)
	}
	routes := make(map[domain.ServerRef]gateway.ProbeRoute, len(p.Topology.Probes))
	for _, r := range p.Topology.Probes {
		routes[r.Server] = r
		if _, ok := s.routeStops[r.Server]; !ok {
			stop, cancel := context.WithCancel(ctx)
			s.routeStops[r.Server] = routeStop{stop, cancel}
		}
	}
	for ref, stop := range s.routeStops {
		if _, kept := routes[ref]; !kept {
			stop.cancel()
			delete(s.routeStops, ref)
		}
	}
	for ref := range s.routes {
		if _, kept := routes[ref]; !kept {
			delete(s.targetTurn, ref)
			delete(s.counters, ref)
			delete(s.passive, ref)
			delete(s.nextUDP, ref)
			delete(s.udpPassed, ref)
			delete(s.udpAsk, ref)
			delete(s.udpHeld, ref)
		}
	}
	budgetChanged := p.Budget != s.plan.Budget
	s.routes, s.plan = routes, p
	if !slices.ContainsFunc(p.Targets, func(t probeTarget) bool { return t.ID == s.ranking }) {
		s.ranking = p.Ranking
	}
	now := s.now()
	if budgetChanged {
		s.attempts = newBucket(max(p.Budget.AttemptsPerMinute, 1), p.Budget.AttemptsPerMinute, now)
		s.beats = newBucket(max(p.Budget.HeartbeatsPerMinute, 1), p.Budget.HeartbeatsPerMinute, now)
		s.lanes[failurePath].bucket = newBucket(max(p.Budget.SuspicionBurst, 1), p.Budget.SuspicionsPerMinute, now)
		s.lanes[improvePath].bucket = newBucket(max(p.Budget.ImprovementsPerMinute, 1), p.Budget.ImprovementsPerMinute, now)
	}
	for ref := range s.nextBeat {
		if _, kept := p.Beats[ref]; !kept {
			delete(s.nextBeat, ref)
		}
	}
	for ref, every := range p.Beats {
		if due, known := s.nextBeat[ref]; !known || due.After(now.Add(every)) {
			s.nextBeat[ref] = now
		}
	}
	maps.DeleteFunc(s.checked, func(ref domain.ServerRef, started time.Time) bool {
		due, wanted := p.Checks[ref]
		return !wanted || due.After(started)
	})
	s.reconcileRounds(p.Rounds)
}

// reconcileRounds makes the queues what the plan asks for. A queued request takes what the plan says
// of it now, since a suspicion's round can be proven while it waits. A withdrawn one goes, a running
// job no campaign wants any more is cancelled and frees its worker at once, and a shared job lives
// while any campaign still wants it. What has started stays remembered until it leaves the plan, so a
// round that has reported isn't run again while the controller catches up.
func (s *signalLoop) reconcileRounds(requests []roundRequest) {
	wanted := make(map[roundKey]bool, len(requests))
	latest := make(map[roundKey]roundRequest, len(requests))
	for _, q := range requests {
		wanted[roundKey{q.Campaign, q.Round}] = true
		latest[roundKey{q.Campaign, q.Round}] = q
	}
	for i := range s.lanes {
		lane := &s.lanes[i]
		lane.queued = slices.DeleteFunc(lane.queued, func(q roundRequest) bool { return !wanted[roundKey{q.Campaign, q.Round}] })
		for j, q := range lane.queued {
			lane.queued[j] = latest[roundKey{q.Campaign, q.Round}]
		}
		lane.running = slices.DeleteFunc(lane.running, func(r *roundJob) bool {
			if r.keepWanted(wanted) {
				return false
			}
			r.cancel()
			return true
		})
	}
	maps.DeleteFunc(s.started, func(key roundKey, _ bool) bool { return !wanted[key] })
	for _, q := range requests {
		key := roundKey{q.Campaign, q.Round}
		if !s.started[key] {
			s.started[key] = true
			s.lanes[q.Path].queued = append(s.lanes[q.Path].queued, q)
		}
	}
}

// finished takes a job off the loop and says whether anyone still wants its results. A withdrawn
// job's results say nothing anyone asked, and most of them are cancellations.
func (s *signalLoop) finished(r roundDone) bool {
	lane := &s.lanes[r.path]
	i := slices.Index(lane.running, r.job)
	if i < 0 {
		return false
	}
	lane.running = slices.Delete(lane.running, i, i+1)
	return true
}

// untilNextRound is how long until the next queued round comes due, so spacing adds no tick of lag.
func (s *signalLoop) untilNextRound(now time.Time) (time.Duration, bool) {
	var next time.Time
	for i := range s.lanes {
		for _, q := range s.lanes[i].queued {
			if q.Due.After(now) && (next.IsZero() || q.Due.Before(next)) {
				next = q.Due
			}
		}
	}
	return next.Sub(now), !next.IsZero()
}

// admit moves what is due on a lane into running jobs. Requests of one round number share a job, so
// a round stays on one target and each server is measured once. A failure round something has proven
// spends nothing: one campaign per slot, 8 slots, 1+3 servers and one failed probe per server per
// heartbeat already bound it, and a budget could only make it late. A round only a passive suspicion
// backs pays its bucket for every server it adds to the job. An improvement round pays its own bucket,
// waits for an empty failure queue, and holds neither that lane's workers nor any of its budget. A round
// measures only servers that can win it, at most 1+3, so what it pays stays within a bucket.
func (s *signalLoop) admit(p path, now time.Time) []*roundJob {
	if p == improvePath && len(s.lanes[failurePath].queued) > 0 {
		return nil
	}
	lane := &s.lanes[p]
	var jobs []*roundJob
	lane.queued = slices.DeleteFunc(lane.queued, func(q roundRequest) bool {
		if q.Due.After(now) || !s.askable(q) {
			return false
		}
		i := slices.IndexFunc(jobs, func(r *roundJob) bool { return r.round == q.Round && r.target == q.Target })
		if i < 0 && len(lane.running)+len(jobs) >= lane.capacity {
			return false
		}
		job := &roundJob{round: q.Round, target: q.Target}
		if i >= 0 {
			job = jobs[i]
		}
		measured := func(ref domain.ServerRef) bool { return slices.Contains(job.servers, ref) }
		fresh := slices.DeleteFunc(q.servers(), measured)
		if (p == improvePath || q.Passive) && !lane.bucket.take(now, float64(len(fresh))) {
			return false
		}
		job.servers, job.wants = append(job.servers, fresh...), append(job.wants, q)
		if i < 0 {
			jobs = append(jobs, job)
		}
		return true
	})
	lane.running = append(lane.running, jobs...)
	return jobs
}

// askable holds a round until one of its rivals has a route, since with none it could only come back
// inconclusive. A rival that just rotated gets its route once the core has judged it, and the
// campaign's Expiry bounds the wait.
func (s *signalLoop) askable(q roundRequest) bool {
	return len(q.Rivals) == 0 || slices.ContainsFunc(q.Rivals, func(ref domain.ServerRef) bool {
		_, routed := s.routes[ref]
		return routed
	})
}

// schedule starts what is due.
func (s *signalLoop) schedule(ctx context.Context, wg *sync.WaitGroup, results chan<- domain.Signal, rounds chan<- roundDone) {
	s.dispatch(s.now(), func(p path, r *roundJob) { s.startRound(ctx, wg, p, r, rounds) },
		func(ref domain.ServerRef, purpose domain.ProbePurpose) {
			s.start(ctx, wg, s.routes[ref], purpose, results)
		})
}

// dispatch decides what starts now: rounds and serving heartbeats, which wait for nothing, then a
// closed slot's search, then checks, then UDP checks.
func (s *signalLoop) dispatch(now time.Time, round func(path, *roundJob), probe func(domain.ServerRef, domain.ProbePurpose)) {
	if len(s.routes) == 0 {
		return
	}
	for p := range s.lanes {
		for _, job := range s.admit(path(p), now) {
			round(path(p), job)
		}
	}
	beats := func(closed bool, purpose domain.ProbePurpose) {
		for {
			ref, ok := s.pickBeat(now, closed)
			if !ok {
				return
			}
			s.inFlight++
			probe(ref, purpose)
		}
	}
	beats(false, domain.Heartbeat)
	beats(true, domain.Search)
	for {
		ref, ok := s.pickCheck(now)
		if !ok {
			break
		}
		s.inFlight++
		probe(ref, domain.Check)
	}
	for {
		ref, ok := s.pickUDP(now)
		if !ok {
			return
		}
		s.inFlight++
		probe(ref, domain.UDPCheck)
	}
}

// pickBeat is the heartbeat to start now, of one class. A server a slot serves goes at once: at most 8
// exist, a worker it waited for would come off the 30 s, and in an outage each worker is held for 7 s.
// The rest wait for a worker and the budget, the shortest period first, so a server that answered again
// collects its successes ahead of the sweep, and then the longest overdue, so a sweep reaches every
// server however many are dead and however often the ones ahead of it come due again.
func (s *signalLoop) pickBeat(now time.Time, closed bool) (domain.ServerRef, bool) {
	if closed && s.inFlight >= s.plan.Budget.Parallel {
		return domain.ServerRef{}, false
	}
	var pick domain.ServerRef
	found := false
	for ref, every := range s.plan.Beats {
		due, ok := s.nextBeat[ref]
		if _, routed := s.routes[ref]; !routed || !ok || due.After(now) || s.plan.Serving[ref] == closed {
			continue
		}
		if !found || beatBefore(s.plan.Beats[pick], s.nextBeat[pick], pick, every, due, ref) > 0 {
			pick, found = ref, true
		}
	}
	if !found || closed && !s.beats.take(now, 1) {
		return domain.ServerRef{}, false
	}
	s.nextBeat[pick] = now.Add(s.plan.Beats[pick])
	return pick, true
}

// pickCheck is the check to start now, the longest overdue first. Each is asked once at its due time,
// and again only if the plan still wants it a minute later, which an answer that moved nothing leaves.
// With the WAN not known to be up a check could say nothing, so it waits. It spends the attempts budget.
func (s *signalLoop) pickCheck(now time.Time) (domain.ServerRef, bool) {
	if !s.wan.up() || s.inFlight >= s.plan.Budget.Parallel {
		return domain.ServerRef{}, false
	}
	var pick domain.ServerRef
	found := false
	for ref, due := range s.plan.Checks {
		started, asked := s.checked[ref]
		if _, routed := s.routes[ref]; !routed || due.After(now) || asked && now.Sub(started) < checkAgain {
			continue
		}
		if !found || due.Before(s.plan.Checks[pick]) || due.Equal(s.plan.Checks[pick]) && compareRefs(ref, pick) < 0 {
			pick, found = ref, true
		}
	}
	if !found || !s.attempts.take(now, 1) {
		return domain.ServerRef{}, false
	}
	s.checked[pick] = now
	return pick, true
}

func beatBefore(everyA time.Duration, dueA time.Time, a domain.ServerRef, everyB time.Duration, dueB time.Time, b domain.ServerRef) int {
	if c := cmp.Compare(everyA, everyB); c != 0 {
		return c
	}
	if c := dueA.Compare(dueB); c != 0 {
		return c
	}
	return compareRefs(a, b)
}

func compareRefs(a, b domain.ServerRef) int {
	switch {
	case a.Lineage < b.Lineage:
		return -1
	case a.Lineage > b.Lineage:
		return 1
	}
	return bytes.Compare(a.Digest[:], b.Digest[:])
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func (s *signalLoop) start(ctx context.Context, wg *sync.WaitGroup, route gateway.ProbeRoute, purpose domain.ProbePurpose, results chan<- domain.Signal) {
	if purpose == domain.UDPCheck {
		s.startUDP(ctx, wg, route, results)
		return
	}
	target := s.rankingTarget()
	if purpose != domain.Check {
		target = s.nextTarget(route.Server)
	}
	stop := s.routeStops[route.Server]
	wg.Add(1)
	go func() {
		defer wg.Done()
		signal := s.probe(stop.ctx, route, target, purpose, s.wanAlone)
		select {
		case results <- signal:
		case <-ctx.Done():
		}
	}()
}

func (s *signalLoop) nextTarget(server domain.ServerRef) probeTarget {
	targets := s.plan.Targets
	turn := s.targetTurn[server]
	s.targetTurn[server] = turn + 1
	return targets[turn%len(targets)]
}

func (s *signalLoop) rankingTarget() probeTarget {
	if i := slices.IndexFunc(s.plan.Targets, func(t probeTarget) bool { return t.ID == s.ranking }); i >= 0 {
		return s.plan.Targets[i]
	}
	return s.plan.Targets[0]
}

// judgeRanking moves the ranking target to the next one when the current one misbehaves. It reads
// every probe on it that asks for a Ping: a check, a round's or a Test. A target that refuses keep-alive is
// alive for every liveness purpose and useless here, so a sample with no reuse counts against it
// although the probe passed. One server's streak is broken by another server's Ping, so a server can't
// move everyone's target.
func (s *signalLoop) judgeRanking(sig domain.Signal) {
	if !asksPing(sig.Purpose) || sig.Target != s.rankingTarget().ID {
		return
	}
	switch {
	case sig.Ping != nil:
		s.rankingMisses = 0
	case sig.NoReuse:
		s.rankingMisses++
	case s.targets.blamed(sig.Target):
		s.rankingMisses = badTargetFails
	}
	if s.rankingMisses >= badTargetFails {
		i := slices.IndexFunc(s.plan.Targets, func(t probeTarget) bool { return t.ID == sig.Target })
		s.ranking = s.plan.Targets[(i+1)%len(s.plan.Targets)].ID
		s.rankingMisses = 0
	}
}

// startRound runs one matched comparison: every server at once on one target under one deadline,
// with one baseline beside them that any failing probe reads. It asks that baseline itself, since a
// reused answer can predate the round, and two rounds a second apart could then share one from before
// an outage and close the slot. baselineTimeout is shorter than probeTimeout, so that answer exists by
// the deadline, and a round takes probeTimeout at most. A rival passing in the same round proves the
// WAN as well as a baseline does, so a baseline that blips can't void it. If that proof was the last
// before an outage, the next round finds none and blames nobody, and a move or a closure takes two.
func (s *signalLoop) startRound(ctx context.Context, wg *sync.WaitGroup, p path, r *roundJob, out chan<- roundDone) {
	target := s.jobTarget(r)
	servers := r.servers
	routes, epoch := s.routes, s.plan.Topology.Epoch
	deadline, cancel := context.WithTimeout(ctx, s.timeout)
	r.cancel = cancel
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		began := s.now()
		baseline := sync.OnceValue(s.baselineNow)
		wg.Go(func() { baseline() })
		wan := func(ctx context.Context, _ time.Time) bool { return baseline() || s.provenDuring(ctx, began) }
		signals := make([]domain.Signal, len(servers))
		var group sync.WaitGroup
		for i, ref := range servers {
			group.Go(func() {
				if route, ok := routes[ref]; ok {
					signals[i] = s.probe(deadline, route, target, domain.Confirmation, wan)
				} else {
					signals[i] = domain.Signal{Stamp: domain.Stamp{Epoch: epoch, At: s.now()}, Server: ref, Kind: domain.ActiveProbe, Purpose: domain.Confirmation, Target: target.ID}
				}
			})
		}
		group.Wait()
		select {
		case out <- roundDone{r, p, signals}:
		case <-ctx.Done():
		}
	}()
}

// jobTarget is the target a job asked for while the plan still lists it, and otherwise its round's.
func (s *signalLoop) jobTarget(r *roundJob) probeTarget {
	if i := slices.IndexFunc(s.plan.Targets, func(t probeTarget) bool { return t.ID == r.target }); i >= 0 {
		return s.plan.Targets[i]
	}
	return s.roundTarget(r.round)
}

// roundTarget rotates by round and passes over a blamed target, whose answers could only end the
// campaign, so a dead server still gets its two rounds beside a censored target.
func (s *signalLoop) roundTarget(round uint8) probeTarget {
	targets := s.plan.Targets
	for i := range targets {
		if t := targets[(int(round)+i)%len(targets)]; !s.targets.blamed(t.ID) {
			return t
		}
	}
	return targets[int(round)%len(targets)]
}

// manual runs a Test, or holds it until its server has a route. Every frame names the servers Tests
// hold, so the controller gives each a route until its Test answers.
func (s *signalLoop) manual(ctx context.Context, wg *sync.WaitGroup, m manualProbe, done chan<- domain.Signal) {
	s.tests[m.Server]++
	if _, ok := s.routes[m.Server]; ok {
		s.runTest(ctx, wg, m, done)
		return
	}
	s.waiting = append(s.waiting, waitingTest{probe: m, until: s.now().Add(testRouteWait)})
}

// startTests runs each waiting Test whose route has come, and answers Unknown to one whose route
// didn't in time. A missing route says nothing about the server, so that answer reaches no record. It
// says whether any Test let go of its server.
func (s *signalLoop) startTests(ctx context.Context, wg *sync.WaitGroup, done chan<- domain.Signal) bool {
	now, released := s.now(), false
	s.waiting = slices.DeleteFunc(s.waiting, func(w waitingTest) bool {
		if _, ok := s.routes[w.probe.Server]; ok {
			s.runTest(ctx, wg, w.probe, done)
			return true
		}
		if now.Before(w.until) {
			return false
		}
		w.probe.Reply <- domain.Signal{Stamp: domain.Stamp{At: now}, Server: w.probe.Server, Kind: domain.ActiveProbe, Purpose: domain.ManualProbe}
		s.tested(w.probe.Server)
		released = true
		return true
	})
	return released
}

func (s *signalLoop) tested(server domain.ServerRef) {
	if s.tests[server]--; s.tests[server] <= 0 {
		delete(s.tests, server)
	}
}

// runTest answers its caller and tells the controller too, since a Test is a result the record keeps.
func (s *signalLoop) runTest(ctx context.Context, wg *sync.WaitGroup, m manualProbe, done chan<- domain.Signal) {
	route := s.routes[m.Server]
	target := s.nextTarget(m.Server)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sig := s.probe(ctx, route, target, domain.ManualProbe, s.wanAlone)
		s.sink.Record(sig)
		m.Reply <- sig
		select {
		case done <- sig:
		case <-ctx.Done():
		}
	}()
}

// accepted drops results whose route no longer exists: the core restarted or the plan
// withdrew it, so a failure there says nothing about the server. A round still needs an answer
// from every server to finish, so its result stays and says it knows nothing. A failure of a
// probe the WAN read down during blames nobody, though the WAN was back when it ended. What
// remains goes to metrics.db, heartbeats included: at one a minute they are what analytics'
// probe counts read.
func (s *signalLoop) accepted(signals []domain.Signal) []domain.Signal {
	out := signals[:0:0]
	for _, sig := range signals {
		if sig.Stamp.Epoch != s.plan.Topology.Epoch {
			continue
		}
		if sig.Purpose == domain.UDPCheck {
			if _, routed := s.routes[sig.Server]; routed {
				out = append(out, s.attributeUDP(sig)...)
			}
			continue
		}
		if _, ok := s.routes[sig.Server]; !ok && sig.Verdict != domain.Unknown {
			if sig.Purpose != domain.Confirmation {
				continue
			}
			sig.Verdict, sig.NewConn, sig.Ping, sig.PingSpread, sig.NoReuse = domain.Unknown, nil, nil, nil, false
		}
		if sig.Verdict == domain.Succeeded {
			s.wan.proxied(sig.Stamp.At, s.now())
		}
		if sig.Verdict == domain.Failed && s.readDownSince(sig.Stamp.At) {
			sig.Verdict, sig.WANExcluded = domain.Unknown, true
		}
		s.sink.Record(sig)
		s.judgeRanking(sig)
		out = append(out, sig)
	}
	return out
}

func (s *signalLoop) emit(out chan<- signalFrame, signals []domain.Signal) {
	s.sequence++
	frame := signalFrame{Sequence: s.sequence, Signals: signals, WAN: s.wan, Gap: s.gap, LineBusy: s.lineBusy,
		Tests: slices.SortedFunc(maps.Keys(s.tests), compareRefs)}
	if len(s.plan.Targets) > 0 {
		frame.Ranking = s.rankingTarget().ID
	}
	select {
	case out <- frame:
		s.gap = false
	default:
		s.gap = true
		s.sink.Gap("controller behind")
	}
}

// judgeWAN folds in a finished baseline. A change is recorded, not every sample.
func (s *signalLoop) judgeWAN(ok bool) {
	now := s.now()
	if !s.wan.judged(ok, now) {
		return
	}
	verdict := domain.Failed
	if s.wan.up() {
		verdict = domain.Succeeded
	}
	s.sink.Record(domain.Signal{Stamp: domain.Stamp{At: now}, Kind: domain.WANBaseline, Verdict: verdict})
}

// probe measures one server through route. A heartbeat is one request, for liveness.
// A failure to reach the core's own inbound is Unknown: the server was never asked. Any
// other failure stands only if wan proves the WAN worked meanwhile, so a WAN outage never
// counts against a provider, however it was timed.
func (s *signalLoop) probe(ctx context.Context, route gateway.ProbeRoute, target probeTarget,
	purpose domain.ProbePurpose, wan func(context.Context, time.Time) bool) domain.Signal {
	start := s.now()
	sig := domain.Signal{
		Stamp:  domain.Stamp{Epoch: route.Epoch, Generation: route.Generation, At: start},
		Server: route.Server, Kind: domain.ActiveProbe, Target: target.ID, Purpose: purpose,
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	reused := 0
	if asksPing(purpose) {
		reused = pingRequests
	}
	m := s.measure(ctx, route, target, reused)
	switch {
	case m.err == nil:
		s.proxied.Store(s.now().UnixNano())
		s.targets.succeeded(target.ID, route.Server)
		sig.Verdict, sig.NewConn, sig.Ping, sig.PingSpread, sig.NoReuse = domain.Succeeded, &m.newConn, m.ping, m.pingSpread, m.noReuse
	case m.neverAsked || errors.Is(context.Cause(ctx), context.Canceled):
		sig.Verdict = domain.Unknown
	case !wan(ctx, start):
		sig.Verdict, sig.WANExcluded = domain.Unknown, true
	case s.targets.failed(target.ID, route.Server):
		sig.Verdict = domain.Unknown
	default:
		sig.Verdict = domain.Failed
	}
	return sig
}

// targetHealth keeps one bad probe target from looking like every server failing. A failure counts
// against a target only once the same server has passed on another target since, because only that
// separates a bad target from a dead server: servers that went silent together fail every target, and
// they don't pass on any. The target keeps being probed, so it counts again from its first success.
type targetHealth struct {
	mu     sync.Mutex
	states map[domain.TargetID]*targetState
}

type targetState struct {
	unexplained map[domain.ServerRef]int // Failures that no later pass of that server has laid on the target.
	fails       int
	servers     map[domain.ServerRef]bool // Whose failures count.
}

const (
	badTargetFails   = 6
	badTargetServers = 2
)

func (h *targetHealth) state(id domain.TargetID) *targetState {
	if h.states == nil {
		h.states = map[domain.TargetID]*targetState{}
	}
	t, ok := h.states[id]
	if !ok {
		t = &targetState{unexplained: map[domain.ServerRef]int{}, servers: map[domain.ServerRef]bool{}}
		h.states[id] = t
	}
	return t
}

func (h *targetHealth) succeeded(id domain.TargetID, server domain.ServerRef) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state(id)
	for other, t := range h.states {
		if other == id {
			t.fails = 0
			clear(t.unexplained)
			clear(t.servers)
		} else if n := t.unexplained[server]; n > 0 {
			t.fails += n
			t.servers[server] = true
			delete(t.unexplained, server)
		}
	}
}

// failed records a failure and reports whether the target itself is to blame.
func (h *targetHealth) failed(id domain.TargetID, server domain.ServerRef) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.state(id)
	t.unexplained[server]++
	return t.blamed()
}

// blamed reports whether the target is to blame right now, without recording anything.
func (h *targetHealth) blamed(id domain.TargetID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state(id).blamed()
}

func (t *targetState) blamed() bool {
	return t.fails >= badTargetFails && len(t.servers) >= badTargetServers
}

// readDownSince reports whether the WAN read down at any moment since start. A probe's failure
// asks a baseline after it, which a WAN back since answers, so this is what blames nobody for an
// outage the probe began in. Blocked baselines say nothing of the WAN, so there the probe's own
// proof stands, as wanAlone takes it.
func (s *signalLoop) readDownSince(start time.Time) bool {
	return !s.baselinesBlocked() && s.wan.downSince(start)
}

// wanAlone is what proves the WAN to a probe with no round around it: a baseline or, once the
// baselines have been silent long enough to be blocked, another probe succeeding during this one.
func (s *signalLoop) wanAlone(ctx context.Context, start time.Time) bool {
	return s.baselineFresh() || s.baselinesBlocked() && s.provenDuring(ctx, start)
}

// baselinesBlocked tells a network that blocks the baseline targets from an outage that
// has just begun: there they answered moments ago, and a rival's success may be from just
// before the WAN died.
func (s *signalLoop) baselinesBlocked() bool {
	return s.now().UnixNano()-s.baselineGood.Load() >= int64(baselineBlockedAfter)
}

// provenDuring reports whether another probe succeeded after this one started, which
// shows the WAN worked while this server didn't, so it can blame this server with every
// baseline target blocked. An older success can't: the WAN may have died since. It waits
// out the probe's deadline, so a server that fails fast still overlaps its rivals.
func (s *signalLoop) provenDuring(ctx context.Context, start time.Time) bool {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if s.proxied.Load() >= start.UnixNano() {
			return true
		}
		select {
		case <-ctx.Done():
			return s.proxied.Load() >= start.UnixNano()
		case <-tick.C:
		}
	}
}

// baselineFresh reuses a baseline from the last moments, so a burst of failed probes runs one.
func (s *signalLoop) baselineFresh() bool {
	s.baselineMu.Lock()
	defer s.baselineMu.Unlock()
	if !s.baselineAt.IsZero() && time.Since(s.baselineAt) < baselineReuse {
		return s.baselineOK
	}
	s.baselineOK = s.baselineNow()
	s.baselineAt = time.Now()
	return s.baselineOK
}

// baselineNow asks the baselines now, under its own deadline: a probe's has often just expired.
func (s *signalLoop) baselineNow() bool {
	ctx, cancel := context.WithTimeout(context.Background(), baselineTimeout)
	defer cancel()
	return s.askBaselines(ctx)
}

// askBaselines asks every direct target at once; one answer proves the WAN. The
// daemon's own traffic takes the main table, never a UniFi policy route, so it can't
// follow a slot.
func (s *signalLoop) askBaselines(ctx context.Context) bool {
	results := make(chan bool, len(s.baselines))
	for _, target := range s.baselines {
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
			if err != nil {
				results <- false
				return
			}
			client := http.Client{Transport: s.direct, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Do(req)
			if err != nil {
				results <- false
				return
			}
			resp.Body.Close()
			results <- resp.StatusCode < 500
		}()
	}
	for range s.baselines {
		if <-results {
			s.baselineGood.Store(s.now().UnixNano())
			return true
		}
	}
	return false
}

// counted turns one telemetry sample into metrics rows, blackhole suspicion and the line's total. A
// suspicion is uplink growing while downlink stays flat, three samples in a row. Empty traffic proves
// nothing, so a quiet server is never suspect. The counters are per server, so the line's total is their
// sum, up and down apart, over the time since the last sample.
func (s *signalLoop) counted(t gateway.Telemetry) []domain.Signal {
	if t.Gap {
		s.sink.Gap("telemetry lost")
	}
	var suspicions []domain.Signal
	var up, down uint64
	at := s.lineAt
	for _, sig := range t.Signals {
		if w := s.counters[sig.Server]; w != nil && w.known {
			up += *sig.Up - min(w.up, *sig.Up)
			down += *sig.Down - min(w.down, *sig.Down)
		}
		at = sig.Stamp.At
	}
	if took := at.Sub(s.lineAt).Seconds(); took > 0 && !s.lineAt.IsZero() {
		s.lineBusy = float64(max(up, down))*8/took > lineGuard
	}
	s.lineAt = at
	for _, sig := range t.Signals {
		s.sink.Record(sig)
		w := s.counters[sig.Server]
		if w == nil {
			w = &counterWindow{}
			s.counters[sig.Server] = w
		}
		up, down := *sig.Up, *sig.Down
		if w.known && up > w.up && down == w.down {
			w.streak++
		} else {
			w.streak = 0
		}
		w.up, w.down, w.known = up, down, true
		if w.streak == counterStreak {
			suspicions = append(suspicions, domain.Signal{Stamp: sig.Stamp, Server: sig.Server, Kind: domain.OutboundCounters,
				Verdict: domain.Failed, AttributionCertain: true})
		}
	}
	return suspicions
}

// tcpSocket is one socket from an inet_diag dump. Absent fields stay nil.
type tcpSocket struct {
	Remote       netip.AddrPort
	Inode        uint32
	UID          uint32
	Mark         uint32
	id           [48]byte // struct inet_diag_sockid, which is what SOCK_DESTROY names a socket by.
	SRTT, RTTVar *time.Duration
	Unacked      *uint32
	LastAckRecv  *time.Duration
	TotalRetrans *uint32
	SegsOut      *uint32
	BytesAcked   *uint64
	DeliveryRate *uint64
}

const (
	stallAfter        = 10 * time.Second
	retransFloorSegs  = 100
	retransSuspicious = 0.05
)

// sampleSockets dumps upstream sockets once and judges them. A socket counts only if
// the core owns it. An endpoint two servers share is ambiguous, not guessed, so it is
// recorded uncertain and raises no suspicion. A kernel that lacks a field gives absence,
// not a zero.
func (s *signalLoop) sampleSockets() []domain.Signal {
	topology := s.plan.Topology
	if topology.CorePID == 0 || len(topology.Upstreams) == 0 {
		return nil
	}
	byRemote := map[netip.AddrPort][]domain.ServerRef{}
	families := map[uint8]bool{}
	for _, u := range topology.Upstreams {
		byRemote[u.Remote] = append(byRemote[u.Remote], u.Server)
		if u.Remote.Addr().Is4() {
			families[afInet] = true
		} else {
			families[afInet6] = true
		}
	}
	sockets, err := s.dump(slices.Sorted(mapKeys(families)))
	if err != nil {
		return nil
	}
	var mine map[uint32]bool
	now := s.now()
	seen := map[uint32]bool{}
	type aggregate struct {
		sockets           int
		srtt, rttvar      time.Duration
		retrans, segs     uint64
		rate              uint64
		stalled, measured bool
	}
	found := map[domain.ServerRef]*aggregate{}
	ambiguous := map[domain.ServerRef]bool{}
	for _, sock := range sockets {
		refs := byRemote[sock.Remote]
		if len(refs) == 0 || sock.UID != 0 {
			continue
		}
		if mine == nil {
			if mine, err = s.socketsOf(topology.CorePID); err != nil {
				return nil
			}
		}
		if !mine[sock.Inode] {
			continue
		}
		seen[sock.Inode] = true
		if len(refs) > 1 {
			for _, r := range refs {
				ambiguous[r] = true
			}
			continue
		}
		a := found[refs[0]]
		if a == nil {
			a = &aggregate{}
			found[refs[0]] = a
		}
		a.sockets++
		if sock.SRTT != nil {
			a.srtt += *sock.SRTT
			a.measured = true
		}
		if sock.RTTVar != nil {
			a.rttvar += *sock.RTTVar
		}
		if sock.DeliveryRate != nil {
			a.rate = max(a.rate, *sock.DeliveryRate)
		}
		if sock.Unacked != nil && sock.LastAckRecv != nil && *sock.Unacked > 0 && *sock.LastAckRecv >= stallAfter {
			a.stalled = true
		}
		if sock.TotalRetrans != nil && sock.SegsOut != nil {
			prev, known := s.sockets[sock.Inode]
			if known && *sock.TotalRetrans >= prev.retrans && *sock.SegsOut >= prev.segs {
				a.retrans += uint64(*sock.TotalRetrans - prev.retrans)
				a.segs += uint64(*sock.SegsOut - prev.segs)
			}
			s.sockets[sock.Inode] = socketMark{*sock.TotalRetrans, *sock.SegsOut}
		}
	}
	for inode := range s.sockets {
		if !seen[inode] {
			delete(s.sockets, inode)
		}
	}
	var suspicions []domain.Signal
	for _, ref := range slices.SortedFunc(mapKeys(found), compareRefs) {
		a := found[ref]
		w := s.passive[ref]
		if w == nil {
			w = &passiveWindow{since: now, certain: true}
			s.passive[ref] = w
		}
		if a.measured {
			w.srtt, w.rttvar = a.srtt/time.Duration(a.sockets), a.rttvar/time.Duration(a.sockets)
		}
		w.retrans += a.retrans
		w.rate = max(w.rate, a.rate)
		w.stalled = w.stalled || a.stalled
		lossy := a.segs >= retransFloorSegs && float64(a.retrans)/float64(a.segs) >= retransSuspicious
		if a.stalled || lossy {
			retrans := a.retrans
			sig := domain.Signal{Stamp: domain.Stamp{Epoch: topology.Epoch, Generation: topology.Generation, At: now}, Server: ref,
				Kind: domain.TCPInfo, Verdict: domain.Failed, Retransmits: &retrans, AttributionCertain: true}
			if a.measured {
				srtt := w.srtt
				sig.SRTT = &srtt
			}
			suspicions = append(suspicions, sig)
		}
	}
	for ref, w := range s.passive {
		if now.Sub(w.since) >= passiveRecord {
			s.recordPassive(ref, w, now, topology)
			delete(s.passive, ref)
		}
	}
	return suspicions
}

func (s *signalLoop) recordPassive(ref domain.ServerRef, w *passiveWindow, now time.Time, topology gateway.Topology) {
	srtt, rttvar, retrans, rate := w.srtt, w.rttvar, w.retrans, w.rate
	verdict := domain.Succeeded
	if w.stalled {
		verdict = domain.Failed
	}
	s.sink.Record(domain.Signal{Stamp: domain.Stamp{Epoch: topology.Epoch, Generation: topology.Generation, At: now}, Server: ref,
		Kind: domain.TCPInfo, Verdict: verdict, SRTT: &srtt, RTTVar: &rttvar, Retransmits: &retrans, DeliveryRate: &rate,
		AttributionCertain: w.certain})
}

const warnLogBound = 1 << 20

// readWarnings tails the core's log. Xray writes it with O_APPEND, so truncating it
// here is safe, and it lives in the run dir, on tmpfs, so it costs the flash nothing.
// Only a warning that names a known endpoint is suspicion.
func (s *signalLoop) readWarnings() []domain.Signal {
	info, err := os.Stat(s.warnLog)
	if err != nil {
		return nil
	}
	if info.Size() < s.logOffset {
		s.logOffset = 0
	}
	if info.Size() == s.logOffset {
		return nil
	}
	f, err := os.Open(s.warnLog)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(s.logOffset, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, 256<<10))
	if err != nil {
		return nil
	}
	s.logOffset += int64(len(data))
	now := s.now()
	suspects := warnedServers(data, s.plan.Endpoints)
	var out []domain.Signal
	for _, ref := range suspects {
		out = append(out, domain.Signal{Stamp: domain.Stamp{Epoch: s.plan.Topology.Epoch, Generation: s.plan.Topology.Generation, At: now},
			Server: ref, Kind: domain.CoreWarning, Verdict: domain.Failed, AttributionCertain: true})
	}
	if s.logOffset >= warnLogBound {
		if os.Truncate(s.warnLog, 0) == nil {
			s.logOffset = 0
		}
	}
	return out
}

// warnedServers finds the servers warning lines name by endpoint. An endpoint two
// servers share proves nothing about either.
func warnedServers(data []byte, endpoints map[string][]domain.ServerRef) []domain.ServerRef {
	var out []domain.ServerRef
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.Contains(line, "[Warning]") && !strings.Contains(line, "[Error]") {
			continue
		}
		for endpoint, refs := range endpoints {
			if len(refs) == 1 && strings.Contains(line, endpoint) && !slices.Contains(out, refs[0]) {
				out = append(out, refs[0])
			}
		}
	}
	slices.SortFunc(out, compareRefs)
	return out
}
