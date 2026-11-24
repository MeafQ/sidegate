// Package runtime owns all mutations of gateway resources: the proxy core, kernel
// plumbing, and UniFi VPN Clients. One actor, Worker, holds the invariant that a
// slot's WireGuard peer (pxaN) is up only while its core route and interception
// rules are verified. The controller sends desired plans, never individual
// iptables, Network API, or core API commands.
package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"sidegate/internal/domain"
)

type KeyMaterial struct {
	Host, Client domain.Secret
}

// SlotTarget is where a slot takes new connections, one server or the WAN, or why it is closed.
type SlotTarget struct {
	Slot      domain.Slot
	Keys      KeyMaterial
	Selection domain.Selection
	Passage   Passage
	Operation domain.EventID
	// CloseFlows names the operation whose open connections must end, once readback shows Selection. The
	// controller names it until a report's FlowsClosed shows it attempted, whichever move the slot is on
	// by then, so a move that supersedes one owing a close inherits it.
	CloseFlows domain.EventID
}

// Passage is how a slot reaches its Selection. The zero Passage moves at once. A move lets the core
// stay on from until the deadline while the Selection loads. A bridge lets it stay on the Selection's
// own server, which the Exit no longer allows, until the deadline. NewPlan rejects either without a
// deadline or on a Selection that serves no server, and a move from no server or the Selection's own.
type Passage struct {
	kind  passageKind
	from  domain.ServerRef
	until time.Time
}

// passageKind is what a Passage lets the core stay on. The zero kind is no passage.
type passageKind uint8

const (
	noPassage passageKind = iota
	movePassage
	bridgePassage
)

func MoveFrom(from domain.ServerRef, until time.Time) Passage {
	return Passage{kind: movePassage, from: from, until: until}
}
func BridgeUntil(until time.Time) Passage { return Passage{kind: bridgePassage, until: until} }

// Allows is the server a slot on selection may still be served from, and until when. A passage with
// no deadline allows none.
func (p Passage) Allows(selection domain.Selection) (domain.ServerRef, time.Time, bool) {
	switch {
	case p.until.IsZero():
	case p.kind == movePassage:
		return p.from, p.until, true
	case p.kind == bridgePassage:
		server, ok := selection.Server()
		return server, p.until, ok
	}
	return domain.ServerRef{}, time.Time{}, false
}

// stage is where a slot's passage stands at now, given what the last claim showed it applied on.
type stage uint8

const (
	settled  stage = iota // No passage, or a move a claim showed on its Selection past the deadline.
	moving                // Before the deadline: the core may serve the prior or the Selection.
	overdue               // Past the deadline, not shown on the Selection: closed until it is.
	bridging              // Before the deadline: the core stays on the Selection's server.
	lapsed                // A bridge past the deadline: out of the core plan and closed.
)

// unclaimed is what a slot shows when no claim is read: a move past its deadline is then overdue.
var unclaimed domain.Selection

func (t SlotTarget) stage(now time.Time, shown domain.Selection) stage {
	p := t.Passage
	switch before := now.Before(p.until); {
	case p.kind == noPassage:
		return settled
	case p.kind == bridgePassage && before:
		return bridging
	case p.kind == bridgePassage:
		return lapsed
	case before:
		return moving
	case shown == t.Selection:
		return settled
	}
	return overdue
}

// mayBeUp is the one test of whether this slot's link may be up at now, given what a claim showed.
func (t SlotTarget) mayBeUp(now time.Time, shown domain.Selection) bool {
	_, down := t.Selection.Down()
	st := t.stage(now, shown)
	return !down && st != overdue && st != lapsed
}

// held is the server the passage still lets the core serve at now, a move's prior or a bridge's own.
func (t SlotTarget) held(now time.Time) (domain.ServerRef, bool) {
	switch t.stage(now, unclaimed) {
	case moving, bridging:
		server, _, _ := t.Passage.Allows(t.Selection)
		return server, true
	}
	return domain.ServerRef{}, false
}

// Plan owns copied values. Every root in servers gets a probe route. Its generation is
// persisted before delivery. No worker can mutate it in place.
type Plan struct {
	generation uint64
	slots      []SlotTarget
	servers    map[domain.ServerRef]domain.Server
	removals   []domain.Slot
}

// NewPlan checks that every selection has a server, that every passage has a deadline and a server
// to stay on other than the selection's own, and that no two slots share an ID, ordinal, or port. A
// move's server needs no server in the plan: a rotated prior's closure is no longer in the catalogue,
// and the Worker's drain map holds it for the same 30 s. An empty plan with removals is uninstall's
// teardown.
func NewPlan(generation uint64, slots []SlotTarget, servers map[domain.ServerRef]domain.Server,
	removals []domain.Slot) (Plan, error) {
	ids, ordinals, ports := map[domain.SlotID]bool{}, map[uint8]bool{}, map[uint16]bool{}
	copied := make([]SlotTarget, 0, len(slots))
	for _, t := range slots {
		a := t.Slot.Allocation
		switch {
		case !validSlotID(t.Slot.ID) || ids[t.Slot.ID]:
			return Plan{}, fmt.Errorf("slot %q is malformed or repeated", t.Slot.ID)
		case ordinals[a.Ordinal]:
			return Plan{}, fmt.Errorf("slot %s: ordinal %d is taken", t.Slot.ID, a.Ordinal)
		case a.PeerListenPort == 0 || a.TPROXYPort == 0 || a.PeerListenPort == a.TPROXYPort ||
			ports[a.PeerListenPort] || ports[a.TPROXYPort]:
			return Plan{}, fmt.Errorf("slot %s: ports %d/%d are missing or taken", t.Slot.ID, a.PeerListenPort, a.TPROXYPort)
		case !a.Tunnel.IsValid() || !a.Tunnel.Addr().Is4():
			return Plan{}, fmt.Errorf("slot %s: tunnel %s is not an IPv4 prefix", t.Slot.ID, a.Tunnel)
		case t.Keys.Host.IsZero() || a.Keys.ClientPublic == "":
			return Plan{}, fmt.Errorf("slot %s: missing WireGuard keys", t.Slot.ID)
		}
		ids[t.Slot.ID], ordinals[a.Ordinal] = true, true
		ports[a.PeerListenPort], ports[a.TPROXYPort] = true, true
		server, serving := t.Selection.Server()
		if _, have := servers[server]; serving && !have {
			return Plan{}, fmt.Errorf("slot %s: selected %s has no server", t.Slot.ID, refTag(server))
		}
		if p := t.Passage; p.kind != noPassage {
			switch {
			case !serving:
				return Plan{}, fmt.Errorf("slot %s: a passage on a selection that serves no server", t.Slot.ID)
			case p.until.IsZero():
				return Plan{}, fmt.Errorf("slot %s: a passage with no deadline", t.Slot.ID)
			case p.kind == movePassage && (p.from == domain.ServerRef{} || p.from == server):
				return Plan{}, fmt.Errorf("slot %s: a move from no server or from its own selection", t.Slot.ID)
			}
		}
		copied = append(copied, t)
	}
	for _, r := range removals {
		if ids[r.ID] {
			return Plan{}, fmt.Errorf("slot %s is both planned and removed", r.ID)
		}
	}
	return Plan{generation: generation, slots: copied, servers: maps.Clone(servers), removals: slices.Clone(removals)}, nil
}

// validSlotID keeps slot IDs inside the characters core tags and rule tags are
// built from.
func validSlotID(id domain.SlotID) bool {
	if len(id) == 0 || len(id) > 32 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// tunnelAlive says the slot's VPN Client is connected. Only a client holding its key can
// complete a handshake, and WireGuard rekeys every two minutes while a keepalive runs, so
// one within three minutes means the client is there.
func tunnelAlive(l link, now time.Time) bool {
	return l.up && !l.handshake.IsZero() && now.Sub(l.handshake) < 3*time.Minute
}

// Generation, Slots, Removals and Servers read a plan back.
func (p Plan) Generation() uint64          { return p.generation }
func (p Plan) Slots() []SlotTarget         { return slices.Clone(p.slots) }
func (p Plan) Removals() []domain.Slot     { return slices.Clone(p.removals) }
func (p Plan) Servers() []domain.ServerRef { return p.probes() }

// closing names the links this plan closes as of now: every slot's that may not be up, given what
// readback last showed, and every removal's. A later pass opens only what the move asked for, since
// a prior past its deadline no longer counts as passing.
func (p Plan) closing(now time.Time, landed map[domain.SlotID]domain.Selection) []string {
	var links []string
	for _, t := range p.slots {
		if !t.mayBeUp(now, landed[t.Slot.ID]) {
			links = append(links, linkName(t.Slot.Allocation))
		}
	}
	for _, r := range p.removals {
		links = append(links, linkName(r.Allocation))
	}
	return links
}

// nextDeadline is when the earliest passage in this plan ends. A bridge closes then, and a move
// still on its prior needs a pass then, since only readback can say whether it landed.
func (p Plan) nextDeadline(now time.Time) (time.Time, bool) {
	var next time.Time
	for _, t := range p.slots {
		if _, held := t.held(now); held && (next.IsZero() || t.Passage.until.Before(next)) {
			next = t.Passage.until
		}
	}
	return next, !next.IsZero()
}

func (p Plan) target(slot domain.SlotID) (SlotTarget, bool) {
	for _, t := range p.slots {
		if t.Slot.ID == slot {
			return t, true
		}
	}
	return SlotTarget{}, false
}

func (p Plan) probes() []domain.ServerRef {
	return slices.SortedFunc(maps.Keys(p.servers), func(a, b domain.ServerRef) int {
		return cmp.Compare(outboundTag(a), outboundTag(b))
	})
}

// Phase is what the pass's final sample of a slot's pxa link showed.
type Phase uint8

const (
	Closed  Phase = iota // Absent, down, or not in our group.
	Ready                // Ours and up.
	Unknown              // The sample wasn't whole.
)

type SlotReport struct {
	Slot               domain.SlotID
	Intent             domain.IntentRevision
	Operation          domain.EventID
	Phase              Phase
	Applied            domain.Selection
	NetworkID          domain.NetworkID
	NetworkEstablished bool
	CoreEpoch          domain.CoreEpoch
	ErrorCode          string
	FlowsClosed        domain.EventID // The last close this Worker attempted for the slot, or "" before its first.
}

// RefusedServer is a plan server the core cannot be given. It costs the plan only
// that server: a slot that selects it reports ErrorCodeServerRefused, and serves
// its live prior or stays closed.
type RefusedServer struct {
	Server domain.ServerRef
	Reason string // Never quotes a credential.
}

// ErrorCodeServerRefused marks a slot whose selection holds a refused server.
const ErrorCodeServerRefused = "server_refused"

type BindingDiscovered struct {
	Slot            domain.SlotID
	NetworkID       domain.NetworkID
	ClientPublicKey string
}

type Upstream struct {
	Remote netip.AddrPort
	Server domain.ServerRef
}

// ProbeRoute reaches one immutable server revision through the core's single
// probe inbound. The route is a VLESS user whose ID is derived from Server, and a
// routing rule on that user selects Server's outbound. A user ID never names a
// different server, so a stale route cannot be misattributed. Dial is a pure
// client of the loopback inbound and is safe from any goroutine; the wire
// protocol stays private to this package.
type ProbeRoute struct {
	Server     domain.ServerRef
	Epoch      domain.CoreEpoch
	Generation uint64
	inbound    netip.AddrPort
	user       [16]byte
}

// Topology is what signals may measure right now. Probes holds exactly the routes
// installed and verified in Epoch. A route that leaves Topology is withdrawn first
// and its user removed only after the drain interval, so the signal loop cancels
// probes before the core can reject them.
type Topology struct {
	Epoch      domain.CoreEpoch
	Generation uint64
	CorePID    int // The core unit's MainPID; signals attribute sockets to it.
	Upstreams  []Upstream
	Probes     []ProbeRoute
}

// servers lists every server the topology can attribute counters to.
func (t Topology) servers() []domain.ServerRef {
	var refs []domain.ServerRef
	for _, u := range t.Upstreams {
		refs = append(refs, u.Server)
	}
	for _, p := range t.Probes {
		if !slices.Contains(refs, p.Server) {
			refs = append(refs, p.Server)
		}
	}
	return refs
}

// Report is the control fact stream to the controller. Reports with discovered IDs
// or applied transitions are delivered losslessly; others may coalesce.
type Report struct {
	Generation   uint64
	At           time.Time
	Slots        []SlotReport
	Discovered   []BindingDiscovered
	Deleted      []domain.SlotID
	Topology     Topology
	Refused      []RefusedServer // Plan servers left out of the core, each alone.
	Capabilities Capabilities
	Core         CoreVersions
	Steps        []string // External changes this pass made, in order. Empty when converged.
	Error        string   // Why the pass stopped short; empty when it converged.
	Gap          bool
}

// Telemetry is the read-only sample stream to the signal loop: cumulative outbound
// counters, stamped with epoch and generation. It bypasses the controller.
// Overflow drops samples and sets Gap on the next one.
type Telemetry struct {
	Signals []domain.Signal
	Gap     bool
}

const (
	defaultPoll       = 10 * time.Second
	drainInterval     = 30 * time.Second
	telemetryInterval = 5 * time.Second
	// maxRepasses bounds immediate re-verification after a pass that changed
	// something, so a fight with another writer degrades to the poll interval.
	maxRepasses = 3
	// readBound is how long state may go unread before every gate closes, since
	// routing can then no longer be proven. A read only fails after a tool or the
	// API has stalled past its own bound: measured tools take about a second under
	// a 1% CPU cap. So it is not load. One stall costs one failed pass, up to one
	// tool bound plus one poll, about 20 s, and rides through with no reconnect. A
	// second failed pass in a row is past 30 s and is no blip. Gates close at most one
	// failed pass after the bound, about 40 s after the last good read.
	readBound = 3 * commandTimeout
)

// readClock remembers the last good read of one source.
type readClock struct{ at time.Time }

// expired records a read's outcome and reports whether the last good read is past
// readBound. A Worker's first read starts the clock even when it fails.
func (c *readClock) expired(err error, now time.Time) bool {
	if err == nil || c.at.IsZero() {
		c.at = now
		return false
	}
	return now.Sub(c.at) >= readBound
}

// drains keeps each probe root that left the plan installed, with its outbound and user but outside
// Topology, until drainInterval has passed or the plan holds it again.
type drains struct {
	last    map[domain.ServerRef]domain.Server // The last plan's probe roots.
	leaving map[domain.ServerRef]drained
}

type drained struct {
	until  time.Time
	server domain.Server
}

// step moves the drains on to a plan of servers at now, and returns the servers still draining.
func (d *drains) step(servers map[domain.ServerRef]domain.Server, now time.Time) map[domain.ServerRef]domain.Server {
	if d.leaving == nil {
		d.leaving = map[domain.ServerRef]drained{}
	}
	for ref, server := range d.last {
		if _, kept := servers[ref]; !kept {
			if _, draining := d.leaving[ref]; !draining {
				d.leaving[ref] = drained{until: now.Add(drainInterval), server: server}
			}
		}
	}
	d.last = servers
	out := map[domain.ServerRef]domain.Server{}
	for ref, l := range d.leaving {
		if _, back := servers[ref]; back || now.After(l.until) {
			delete(d.leaving, ref)
			continue
		}
		out[ref] = l.server
	}
	return out
}

type Worker struct {
	core                 *xray
	kernel               *kernel
	lockPath             string
	poll                 time.Duration
	drain                drains
	kernelRead, coreRead readClock
	backoff              backoffs
	routing              string // What the last pass's plan would write; see routingKey.
	epoch                domain.CoreEpoch
	// drifted holds when a check last failed with the plan and the core unchanged,
	// which only another writer explains. The repair that follows passes the next
	// check whether or not that writer has stopped, so a closed gate waits a poll
	// for a check that proves more: a writer faster than the poll keeps it closed.
	drifted  map[domain.SlotID]time.Time
	topology atomic.Pointer[Topology]
	gates    *gates

	wake    chan struct{} // Capacity 1: something a pass should see has changed.
	jobs    chan job      // Capacity 1, newest only: servers awaiting a verdict.
	adopted bool          // The first read of the core has happened.

	destroyFlows func(pid int, mark uint32) (int, error)
	flowsClosed  map[domain.SlotID]domain.EventID // The last operation closed per slot, so a repeated plan or pass can't close twice.
}

type Options struct {
	DataDir string        // /data/sidegate; holds the release with the pinned core.
	RunDir  string        // /run/sidegate; the rendered core config and the actor lock.
	Poll    time.Duration // Drift readback interval; zero means 10 s.
	// CloseFlows destroys the established TCP sockets process pid holds with this mark, and says how
	// many. A tool that carries no client's traffic leaves it nil.
	CloseFlows func(pid int, mark uint32) (int, error)
}

// Open builds a Worker. The controller keeps VPN Clients in step through internal/unifi,
// so to the runtime every planned slot is bound to a client someone else manages.
func Open(options Options) (*Worker, error) {
	switch {
	case options.DataDir == "" || options.RunDir == "":
		return nil, errors.New("runtime needs a data dir and a run dir")
	}
	g := &gates{write: writeLink}
	w := &Worker{
		core:     newXray(options),
		kernel:   &kernel{gateLock: gateLock(options.RunDir), gates: g},
		lockPath: filepath.Join(options.RunDir, "runtime.lock"),
		poll:     cmp.Or(options.Poll, defaultPoll),
		backoff:  backoffs{},
		drifted:  map[domain.SlotID]time.Time{},
		gates:    g,
		wake:     make(chan struct{}, 1),
		jobs:     make(chan job, 1),

		destroyFlows: options.CloseFlows,
		flowsClosed:  map[domain.SlotID]domain.EventID{},
	}
	w.core.learned = w.nudge
	return w, nil
}

// Run is the actor. It holds the run-dir lock for its whole life, and every tool
// it runs holds the lock too, so no second process mutates the core or the
// kernel. Run first waits for the lock, so a pass never observes while an earlier
// runtime, or a tool a killed one left running, can still change what it reads,
// and reports the wait once it outlasts any tool's bound. Startup is adoption:
// nothing changes until the first Plan, and the first pass keeps every gate it can
// prove from readback. Plans arrive through intake and verdicts through judging, so
// neither waits for a pass, and a pass waits for neither. Cancellation leaves the core and open gates serving;
// teardown is an empty Plan, not exit.
func (w *Worker) Run(ctx context.Context, goals <-chan Plan, reports chan<- Report, telemetry chan<- Telemetry) error {
	lock, err := awaitLock(ctx, w.lockPath, commandTimeout, func() {
		select {
		case reports <- Report{At: time.Now(), Error: "waiting for " + w.lockPath + ": an earlier runtime or a tool it left running holds it"}:
		case <-ctx.Done():
		}
	})
	if err != nil {
		return err
	}
	if lock == nil {
		return nil
	}
	defer lock.Close()
	w.kernel.tools, w.core.tools = tools{lock}, tools{lock}
	if telemetry != nil {
		go w.core.telemetry(ctx, telemetryInterval, w.currentTopology, telemetry)
	}
	ctx, cancel := context.WithCancel(ctx)
	var actors sync.WaitGroup
	defer w.core.reads.Wait()
	defer actors.Wait()
	defer cancel()
	actors.Go(func() { w.intake(ctx, goals) })
	actors.Go(func() { w.judging(ctx) })
	var seen *Plan
	timer := time.NewTimer(w.poll)
	defer timer.Stop()
	repasses := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.wake:
		case <-timer.C:
		}
		target := w.gates.newest()
		if target == nil {
			timer.Reset(w.poll)
			continue
		}
		if target != seen {
			seen, repasses = target, 0
		}
		report, err := w.pass(ctx, target)
		if err != nil {
			report.Error = err.Error()
		}
		select {
		case reports <- report:
		case <-ctx.Done():
			return nil
		}
		next := w.poll
		if len(report.Steps) > 0 && repasses < maxRepasses {
			next, repasses = 0, repasses+1
		} else if len(report.Steps) == 0 {
			repasses = 0
		}
		timer.Reset(next)
	}
}

// intake takes every plan as it arrives, so a closure never waits for a pass. It closes what the
// plan closes with one ioctl, and at any passage's deadline it closes that slot the same way unless a
// read has shown its move landed, however long a pass is blocked. It wakes a pass then too, which is
// what opens the replacement. It closes only links in our group: a removed slot's name can belong to
// another program's link. A failed close of a link that may not exist proves nothing: the pass reads
// every link and closes it again.
func (w *Worker) intake(ctx context.Context, goals <-chan Plan) {
	deadline := time.NewTimer(time.Hour)
	deadline.Stop()
	defer deadline.Stop()
	for {
		var arrived *Plan
		select {
		case <-ctx.Done():
			return
		case p := <-goals:
			arrived = &p
		case <-deadline.C:
		}
		deadline.Stop()
		if next := w.gates.take(arrived); !next.IsZero() {
			deadline.Reset(time.Until(next))
		}
		w.nudge()
	}
}

func (w *Worker) nudge() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// judging owns the verdicts and runs one job at a time, so a slow or wedged `xray run -test` costs
// new servers their routes and nothing else. Only new verdicts wake the loop: a binary that can't
// run fails at once, and its job comes back with the next poll.
func (w *Worker) judging(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-w.jobs:
			for _, servers := range []map[domain.ServerRef]domain.Server{j.serving, j.rest} {
				if len(servers) == 0 {
					continue
				}
				learned, err := w.core.judge(ctx, servers)
				if learned {
					w.nudge()
				}
				if err != nil {
					break
				}
			}
		}
	}
}

// job is the servers awaiting a verdict. The ones a slot would serve on are judged and published
// first, so no slot waits on the rest. After an upgrade onto another core the rest is every other
// server, and one refusal among them costs a config test of each.
type job struct {
	serving, rest map[domain.ServerRef]domain.Server
}

// job splits waiting into the servers a slot would serve on, its selection or its live prior, and
// the rest.
func (p Plan) job(waiting map[domain.ServerRef]domain.Server, now time.Time) job {
	j := job{serving: map[domain.ServerRef]domain.Server{}, rest: maps.Clone(waiting)}
	for _, t := range p.slots {
		var on []domain.ServerRef
		if server, serving := t.Selection.Server(); serving {
			on = append(on, server)
		}
		if prior, held := t.held(now); held {
			on = append(on, prior)
		}
		for _, ref := range on {
			if server, waits := j.rest[ref]; waits {
				j.serving[ref] = server
				delete(j.rest, ref)
			}
		}
	}
	return j
}

// loadedBy lists the plan servers a running core of the pinned binary already loaded.
func loadedBy(obs coreObservation, plan Plan) map[domain.ServerRef]domain.Server {
	loaded := map[domain.ServerRef]domain.Server{}
	if !obs.Running || obs.Outdated {
		return loaded
	}
	for ref, server := range plan.servers {
		if slices.Contains(obs.outbounds, outboundTag(ref)) {
			loaded[ref] = server
		}
	}
	return loaded
}

func (w *Worker) sendJob(j job) {
	select {
	case <-w.jobs:
	default:
	}
	w.jobs <- j
}

// adopt runs before the first pass that can read the core. The running core already loaded every
// plan server it holds an outbound for, so those count as judged and a daemon restart removes
// nothing. A core that isn't the pinned binary vouches for nothing: after an upgrade onto another
// core, what the old one holds waits for the pinned one like any new server. A nil result means the
// core can't be read yet, and then nothing can be called unjudged.
func (w *Worker) adopt(ctx context.Context, c *corePass, plan Plan) (*verdicts, error) {
	if !w.adopted {
		obs, err := c.inventory(ctx)
		if err != nil {
			return nil, err
		}
		w.adopted = true
		book := w.core.current().loaded(loadedBy(obs, plan))
		w.core.judged.Store(&book)
	}
	book := w.core.current()
	return &book, nil
}

func (w *Worker) currentTopology() Topology {
	if t := w.topology.Load(); t != nil {
		return *t
	}
	return Topology{}
}

// gate is what one slot's gate may do, from the current readback alone. No rewrite
// budget is among its inputs: rewrites repair what the check cannot see, and a gate
// follows the check whether or not one could run.
type gate struct {
	up          bool
	wanted      bool // The plan serves this slot.
	blind       bool // State has gone unread past readBound.
	coreRead    bool // The core was read, so the core's fields below mean something. A refuted slot reads as read.
	kernelRead  bool // The kernel was read, so ingress and plumbed mean something.
	ingress     bool // Its iif unreachable rule holds, so a foreign iptables flush drops traffic and the repair needs no reconnect.
	passed      bool // Readback shows the core serving exactly the planned selection, and no add of it failed.
	routine     bool // This pass is the first to see what a rewrite would write, so nothing has had a chance to apply it.
	onInstalled bool // Readback shows the core routing only to the selection or a live prior, by exact tag.
	moving      bool // Its passage is before its deadline.
	stays       bool // Unread, it may stay up: on what the gates vouch for, at the gate's own clock.
	plumbed     bool // Its kernel plumbing reads back complete.
	settled     bool // No check has failed by drift within a poll; see Worker.drifted.
}

// keep decides before this pass repairs anything. A failed check closes the gate,
// unless the plan just moved or a move is landing: a switch or a credential rotation
// never drops the tunnel, and open below judges it once the core has been given the plan.
func (g gate) keep() bool {
	return g.up && g.wanted && !g.blind && (g.ingress || !g.kernelRead) &&
		(g.passed || g.onInstalled && (g.routine || g.moving) || !g.coreRead && g.stays)
}

// open decides after this pass has repaired what it can. What could not be read
// within the bound changes nothing while the gate may stay: an open gate stays, and a
// closed one waits. A move that hasn't landed by its deadline closes the gate, unless
// the gates still vouch for it on its Selection.
func (g gate) open() bool {
	switch {
	case !g.wanted || g.blind:
		return false
	case !g.coreRead:
		return g.up && g.stays
	case !g.passed:
		return g.up && g.moving && g.onInstalled
	case !g.kernelRead:
		return g.up
	}
	return g.plumbed && (g.up || g.settled)
}

// pass is one level-triggered reconcile: read back the kernel and the core, close
// every gate whose check fails, converge the closed plumbing, repair the core, then
// check again and set every gate from that check. Readback cannot prove the core
// routes as planned (see routed), and its routing is ours alone, so routing is
// rewritten from the plan before a closed gate opens and after any failed check, as
// each slot's backoff allows. A gate does not wait for that rewrite.
func (w *Worker) pass(ctx context.Context, planned *Plan) (Report, error) {
	plan := *planned
	now := time.Now()
	c := w.corePass()
	book, adoptErr := w.adopt(ctx, c, plan)
	cp := w.corePlan(plan, now, book)
	routing := routingKey(cp)
	routine := routing != w.routing
	w.routing = routing
	report := Report{Generation: plan.generation, At: now}
	var judgeErr error
	refused := map[domain.ServerRef]bool{}
	if book != nil {
		judgeErr = book.err
		for _, ref := range slices.SortedFunc(maps.Keys(plan.servers), func(a, b domain.ServerRef) int { return cmp.Compare(outboundTag(a), outboundTag(b)) }) {
			if v := book.of[ref]; v.state == verdictRefused {
				refused[ref] = true
				report.Refused = append(report.Refused, RefusedServer{Server: ref, Reason: v.reason})
			}
		}
		if len(cp.Waiting) > 0 {
			w.sendJob(plan.job(cp.Waiting, now))
		}
	}

	spec := w.kernel.render(plan.kernel(func(SlotTarget) bool { return false }, !cp.empty()))
	have := w.kernel.observe(ctx, spec)
	core := coreObservation{readErr: adoptErr}
	if adoptErr == nil {
		core = c.read(ctx, cp)
	}
	read := time.Now()
	kernelBlind, coreBlind := w.kernelRead.expired(have.readErr, read), w.coreRead.expired(core.readErr, read)
	problems := []error{judgeErr, have.readErr, core.readErr}
	if kernelBlind || coreBlind {
		problems = append(problems, fmt.Errorf("every gate closed: state unread for over %s", readBound))
	}
	wanted := func(t SlotTarget) bool {
		return slices.ContainsFunc(cp.Slots, func(s coreSlot) bool { return s.ID == t.Slot.ID })
	}
	gateOf := func(t SlotTarget) gate {
		link := linkName(t.Slot.Allocation)
		kernelRead := have.readErr == nil
		// A slot an add failed on is read, whatever its tags say: nothing may be bound behind them.
		refuted := c.view.claim(t.Slot.ID) == claimRefuted
		coreRead := core.readErr == nil && !refuted
		// A pass can outlast a deadline, so the deadline is read when the gate is.
		at := time.Now()
		_, moving := t.held(at)
		return gate{
			up: have.links[link].up, wanted: wanted(t), blind: kernelBlind || coreBlind, coreRead: coreRead || refuted, kernelRead: kernelRead,
			ingress: kernelRead && slices.Contains(have.rules, ingressRule(link)), passed: coreRead && passedNow(t, core, at), routine: routine,
			onInstalled: coreRead && onInstalled(t, core, at), moving: moving, stays: t.mayBeUp(at, w.gates.vouches(t.Slot.ID)),
			plumbed: kernelRead && ready(spec, have, t.Slot.ID), settled: now.Sub(w.drifted[t.Slot.ID]) >= w.poll,
		}
	}
	if core.readErr == nil {
		w.observeChecks(plan, wanted, routing, routine, core, now)
	}
	w.gated(plan, have)

	// The core's listeners stay guarded while it may run, until teardown has stopped it.
	guarded := func() bool { return !cp.empty() || core.Running || core.readErr != nil }
	spec = w.kernel.render(plan.kernel(func(t SlotTarget) bool { return gateOf(t).keep() }, guarded()))
	have, steps, err := w.kernel.converge(ctx, spec, have, nil)
	w.gated(plan, have)
	report.Steps = steps
	problems = append(problems, err)

	if core.readErr == nil {
		problems = append(problems, w.repairCore(ctx, c, plan, cp, &core, &report, wanted, now)...)
	}
	if core.readErr == nil {
		w.gates.publish(c.view.claims())
	}

	spec = w.kernel.render(plan.kernel(func(t SlotTarget) bool { return gateOf(t).open() }, guarded()))
	have, steps, err = w.kernel.converge(ctx, spec, have, w.opening(c, planned, cp, core))
	w.gated(plan, have)
	report.Steps = append(report.Steps, steps...)
	problems = append(problems, err, have.readErr, core.readErr)
	// Flows close only once the core and the kernel both read after repair.
	if core.readErr == nil && have.readErr == nil {
		steps, closeErrs := w.closeFlows(ctx, c, plan, cp, have)
		report.Steps = append(report.Steps, steps...)
		problems = append(problems, closeErrs...)
	}
	// One read once the closes have run, when anything since the newest good read keeps a claim from
	// standing: a relisten, a failed add, another start named, or an answer against a shown slot.
	if core.readErr == nil && have.readErr == nil && c.view.stale() {
		problems = append(problems, c.read(ctx, cp).readErr)
	}
	// A slot an add failed on closes in this pass, whatever its last read showed.
	for _, t := range plan.slots {
		if c.view.claim(t.Slot.ID) == claimRefuted {
			problems = append(problems, w.gates.close(linkName(t.Slot.Allocation)))
		}
	}
	links, err := w.kernel.sample()
	w.sampled(plan, links)
	problems = append(problems, err)
	return w.finish(report, plan, cp, refused, core, have, c.view, links, coreBlind), joinOnce(problems)
}

// removedFlows is the operation a removal closes under. A removal has no event of its own.
const removedFlows domain.EventID = "removed"

// closeFlows ends the open connections of each slot its plan asks about, once per operation and only
// once readback shows the plan's target, so a client reconnecting at once takes the new path. Each slot
// is judged at its own turn against the view as it stands then: a slot the core serves lands while the
// view claims it on its Selection and the kernel read shows its link up, a Down slot and a removal
// while the link reads down and no other start was named. A served slot that lands reads the core's
// start first, and is judged again on what that read leaves, so a close is spent only on the start its
// claim stands on. Its UDP sessions end with their inbound, and its TCP connections by their mark, on
// the process the view read. A crash before this leaves flows draining or, while the move is still
// pending, closes them on restart. A failure isn't retried: a second try would reset what already
// reconnected.
func (w *Worker) closeFlows(ctx context.Context, c *corePass, plan Plan, cp corePlan, have kernelObs) ([]string, []error) {
	v := c.view
	var steps []string
	var errs []error
	lands := func(id domain.SlotID, a domain.Allocation, sel domain.Selection, removed bool) bool {
		up := have.links[linkName(a)].up
		if _, down := sel.Down(); down || removed {
			return !up && v.since == nil && v.read != nil
		}
		return up && v.claim(id) == claimShown && v.read.Applied[id] == sel
	}
	closeOne := func(id domain.SlotID, a domain.Allocation, sel domain.Selection, op domain.EventID, removed bool) {
		if w.flowsClosed[id] == op || !lands(id, a, sel, removed) {
			return
		}
		i := slices.IndexFunc(cp.Slots, func(s coreSlot) bool { return s.ID == id })
		var before *domain.CoreEpoch
		if i >= 0 {
			epoch, err := c.start(ctx)
			if before = known(epoch, err); !lands(id, a, sel, removed) {
				return
			}
		}
		w.flowsClosed[id] = op
		if i >= 0 {
			if err := c.relisten(ctx, cp.Slots[i], before); err != nil {
				errs = append(errs, fmt.Errorf("relisten %s for %s: %w", id, op, err))
			}
		}
		if !v.read.Running || w.destroyFlows == nil {
			return
		}
		n, err := w.destroyFlows(v.read.PID, flowMark(a))
		if err != nil {
			errs = append(errs, fmt.Errorf("close flows of %s for %s: %w", id, op, err))
		}
		steps = append(steps, fmt.Sprintf("closed flows slot=%s op=%s tcp=%d udp=%t", id, op, n, i >= 0))
	}
	for _, t := range plan.slots {
		if t.CloseFlows != "" {
			closeOne(t.Slot.ID, t.Slot.Allocation, t.Selection, t.CloseFlows, false)
		}
	}
	for _, r := range plan.removals {
		closeOne(r.ID, r.Allocation, domain.Selection{}, removedFlows, true)
	}
	return steps, errs
}

// opening opens a slot's gate for planned. The core can die or change between the pass's readback,
// verified, and a link up, so the opening re-reads it under the gate lock and opens only on the same
// running core, still applied. A plan or a deadline can close the slot while it reads, so gates judges
// what it read against the newest plan and the clock as it writes the link up. One already closed reads
// nothing, so a stalled core holds up no pass behind it. The backoff records each up it lands, because
// the link can close again before any read shows it up.
func (w *Worker) opening(c *corePass, planned *Plan, cp corePlan, verified coreObservation) openFunc {
	return func(ctx context.Context, slot domain.SlotID) error {
		if err := w.gates.stale(planned, slot); err != nil {
			return err
		}
		now, err := c.observe(ctx, cp)
		target, _ := planned.target(slot)
		switch {
		case err != nil:
			return err
		case !now.Running || now.Epoch != verified.Epoch || now.PID != verified.PID:
			return fmt.Errorf("core changed before %s opened: epoch %q pid %d, verified %q pid %d",
				slot, now.Epoch, now.PID, verified.Epoch, verified.PID)
		case !passedNow(target, now, time.Now()):
			return fmt.Errorf("slot %s no longer reads back as applied", slot)
		}
		if err := w.gates.open(planned, slot, now.Applied[slot]); err != nil {
			return err
		}
		w.backoff.gated(slot, true)
		return nil
	}
}

// repairCore asks the core for the rewrites each slot's backoff allows and converges
// it on cp, leaving the core as last read in core.
func (w *Worker) repairCore(ctx context.Context, c *corePass, plan Plan, cp corePlan, core *coreObservation, report *Report, wanted func(SlotTarget) bool, now time.Time) []error {
	var asked []domain.SlotID
	var problems []error
	for _, t := range plan.slots {
		switch owed, due := w.backoff.due(t.Slot.ID, now); {
		case !wanted(t) || !owed:
		case due:
			asked = append(asked, t.Slot.ID)
		default:
			problems = append(problems, w.backoff.held(t.Slot.ID))
		}
	}
	epoch := core.Epoch
	after, steps, err := c.reconcile(ctx, cp, *core, len(asked) > 0)
	*core = after
	report.Steps = append(report.Steps, steps...)
	// A core that just started read its routing from the plan, which answers a rewrite.
	started := slices.Contains(steps, coreStartStep)
	if started {
		epoch = after.Epoch
	}
	if started || slices.Contains(steps, routingStep) {
		for _, id := range asked {
			w.backoff.rewritten(id, epoch)
		}
	}
	return append(problems, err)
}

// joinOnce joins errs, each distinct message once: one failed read shows in every
// list that met it.
func joinOnce(errs []error) error {
	seen := map[string]bool{}
	var out []error
	for _, err := range errs {
		if err != nil && !seen[err.Error()] {
			seen[err.Error()] = true
			out = append(out, err)
		}
	}
	return errors.Join(out...)
}

// passedNow says readback shows the core serving what the plan allows at this moment. A core plan
// made before a deadline can put a slot on its prior while the Selection awaits a verdict, and that
// stops counting at the deadline, whenever the pass that made it ends.
func passedNow(t SlotTarget, core coreObservation, now time.Time) bool {
	applied, ok := core.Applied[t.Slot.ID]
	return ok && t.mayBeUp(now, applied)
}

// onInstalled reports whether readback shows slot t routing only to live outbounds of its
// selection or, until its deadline, its prior. It compares exact tags, because every server with a
// probe route is in the plan, so a check against the plan would permit anything. A flip between a server and
// the WAN changes the routing, so this counts only for the pass that first sees it. Either end of a
// flip is somewhere the owner just asked for, so the flip never drops the tunnel.
func onInstalled(t SlotTarget, core coreObservation, now time.Time) bool {
	if t.Selection.IsDirect() || t.Selection.IsBlocked() {
		return len(core.Serving[t.Slot.ID]) > 0
	}
	want := []string{directTag}
	if server, ok := t.Selection.Server(); ok {
		want = append(want, outboundTag(server))
	}
	if prior, held := t.held(now); held {
		want = append(want, outboundTag(prior))
	}
	serving := core.Serving[t.Slot.ID]
	return len(serving) > 0 && !slices.ContainsFunc(serving, func(tag string) bool { return !slices.Contains(want, tag) })
}

// observeChecks records each planned slot's check in its backoff, and any drift.
func (w *Worker) observeChecks(plan Plan, wanted func(SlotTarget) bool, routing string, routine bool, core coreObservation, now time.Time) {
	planned := map[domain.SlotID]bool{}
	for _, t := range plan.slots {
		if !wanted(t) {
			continue
		}
		_, passed := core.Applied[t.Slot.ID]
		planned[t.Slot.ID] = true
		c := checkFailed
		if passed {
			c = checkPassed
		} else if !core.listening(t.Slot.ID) {
			c = checkUnmade
		}
		w.backoff.observe(t.Slot.ID, routing, core.Epoch, c, now)
		if !passed && !routine && core.Running && core.Epoch == w.epoch {
			w.drifted[t.Slot.ID] = now
		}
	}
	w.backoff.retain(planned)
	maps.DeleteFunc(w.drifted, func(id domain.SlotID, _ time.Time) bool { return !planned[id] })
	w.epoch = core.Epoch
}

// sampled records each planned slot's gate in its backoff as the final sample shows it: the links it
// shows ours. A sample that wasn't whole records nothing.
func (w *Worker) sampled(plan Plan, links map[string]linkSample) {
	for _, t := range plan.slots {
		if l := links[linkName(t.Slot.Allocation)]; l.ours {
			w.backoff.gated(t.Slot.ID, l.up)
		}
	}
}

// gated records each planned slot's gate in its backoff as a kernel read shows it. A link the read
// stopped short of is unknown, and records nothing.
func (w *Worker) gated(plan Plan, have kernelObs) {
	for _, t := range plan.slots {
		if l, read := have.links[linkName(t.Slot.Allocation)]; read {
			w.backoff.gated(t.Slot.ID, l.up)
		}
	}
}

func (p Plan) kernel(open func(SlotTarget) bool, core bool) kernelWant {
	want := kernelWant{core: core}
	for _, t := range p.slots {
		want.slots = append(want.slots, plumbing{Slot: t.Slot.ID, Allocation: t.Slot.Allocation, HostKey: t.Keys.Host, Open: open(t)})
	}
	for _, r := range p.removals {
		want.retired = append(want.retired, linkName(r.Allocation))
	}
	return want
}

// corePlan derives the core's target, with the probe roots still draining. A server with no verdict
// or a refusal gets no route, and a slot that selects one serves its live prior if the plan still
// holds that server, so a move onto it stays pending with its gate up. Any other such slot stays out,
// so it stays closed. A nil book means nothing could be read, and then the plan stands as it is.
func (w *Worker) corePlan(plan Plan, now time.Time, book *verdicts) corePlan {
	cp := corePlan{Servers: maps.Clone(plan.servers), Probes: plan.probes()}
	for ref, server := range w.drain.step(plan.servers, now) {
		cp.Draining = append(cp.Draining, ref)
		cp.Servers[ref] = server
	}
	slices.SortFunc(cp.Draining, func(a, b domain.ServerRef) int { return cmp.Compare(outboundTag(a), outboundTag(b)) })
	if book != nil {
		cp.Waiting = map[domain.ServerRef]domain.Server{}
		for ref, server := range cp.Servers {
			switch book.of[ref].state {
			case verdictPending:
				cp.Waiting[ref] = server
				delete(cp.Servers, ref)
			case verdictRefused:
				delete(cp.Servers, ref)
			}
		}
		gone := func(ref domain.ServerRef) bool { _, held := cp.Servers[ref]; return !held }
		cp.Probes = slices.DeleteFunc(cp.Probes, gone)
		cp.Draining = slices.DeleteFunc(cp.Draining, gone)
	}
	for _, t := range plan.slots {
		if t.Selection.IsDirect() || t.Selection.IsBlocked() {
			cp.Slots = append(cp.Slots, coreSlot{ID: t.Slot.ID, Port: t.Slot.Allocation.TPROXYPort, Mark: flowMark(t.Slot.Allocation), Selection: t.Selection})
			continue
		}
		server, serving := t.Selection.Server()
		if !serving || t.stage(now, unclaimed) == lapsed {
			continue
		}
		if _, kept := cp.Servers[server]; !kept {
			prior, held := t.held(now)
			if _, priorKept := cp.Servers[prior]; !held || !priorKept {
				continue
			}
			server = prior
		}
		cp.Slots = append(cp.Slots, coreSlot{ID: t.Slot.ID, Port: t.Slot.Allocation.TPROXYPort, Mark: flowMark(t.Slot.Allocation), Selection: domain.Serving(server)})
	}
	return cp
}

// finish publishes Topology and assembles the report from readback alone. A slot is Applied only on
// what the pass can still claim, and Ready only when the final sample shows its link ours and up as
// well: one up with no claim is Unknown, as is every slot of a sample that wasn't whole. A read that
// failed says nothing about the core, so until readBound what signals may measure stays as last read:
// one stall is ridden through, and it must cost no route, no evidence and no running confirmation.
// blind is the gates' own reading of that bound, so the routes go in the pass that closes the gates.
func (w *Worker) finish(report Report, plan Plan, cp corePlan, refused map[domain.ServerRef]bool, core coreObservation, have kernelObs, v *view, links map[string]linkSample, blind bool) Report {
	report.Capabilities, report.Core = core.Capabilities, core.Versions
	var epoch domain.CoreEpoch
	if on := v.current(); on != nil {
		epoch = *on
	}
	for _, t := range plan.slots {
		shown := v.claim(t.Slot.ID) == claimShown
		var applied domain.Selection
		if shown {
			applied = v.read.Applied[t.Slot.ID]
		}
		phase := Unknown
		if l := links[linkName(t.Slot.Allocation)]; links != nil && (!l.ours || !l.up) {
			phase = Closed
		} else if links != nil && shown {
			phase = Ready
		}
		code := ""
		if server, serving := t.Selection.Server(); serving && refused[server] {
			code = ErrorCodeServerRefused
		}
		report.Slots = append(report.Slots, SlotReport{
			Slot: t.Slot.ID, Intent: t.Slot.Version, Operation: t.Operation, Phase: phase,
			Applied: applied, NetworkID: t.Slot.Allocation.NetworkID, CoreEpoch: epoch, ErrorCode: code,
			NetworkEstablished: tunnelAlive(have.links[linkName(t.Slot.Allocation)], report.At), FlowsClosed: w.flowsClosed[t.Slot.ID],
		})
	}
	for _, r := range plan.removals {
		if _, ok := have.links[linkName(r.Allocation)]; !ok && have.readErr == nil {
			report.Deleted = append(report.Deleted, r.ID)
		}
	}
	topology := Topology{Epoch: core.Epoch, Generation: plan.generation, CorePID: core.PID}
	if core.Running {
		for _, ref := range cp.Probes {
			root, hops := cp.Servers[ref].Closure()
			for _, h := range hops {
				if addr, err := netip.ParseAddr(h.Endpoint.Host); h.ID == root && h.DialVia == "" && err == nil {
					topology.Upstreams = append(topology.Upstreams, Upstream{Remote: netip.AddrPortFrom(addr, h.Endpoint.Port), Server: ref})
				}
			}
		}
		for _, ref := range core.Probes {
			topology.Probes = append(topology.Probes, ProbeRoute{
				Server: ref, Epoch: core.Epoch, Generation: plan.generation, inbound: probeAddr, user: probeUser(ref),
			})
		}
	}
	if last := w.topology.Load(); core.readErr != nil && last != nil && !blind {
		topology = *last
		topology.Generation = plan.generation
	}
	report.Topology = topology
	w.topology.Store(&topology)
	return report
}

// lockFile takes an flock on path, held until the returned file is closed. The
// kernel drops it once every process holding the file is gone, so a crash never
// leaves it held.
func lockFile(path string, how int) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// awaitLock takes the flock on path, waiting while anyone holds it and calling
// stalled after each stall of the wait. It returns nil and no error when ctx ends
// first.
func awaitLock(ctx context.Context, path string, stall time.Duration, stalled func()) (*os.File, error) {
	next := time.Now().Add(stall)
	for {
		lock, err := lockFile(path, syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return lock, err
		}
		if time.Now().After(next) {
			stalled()
			next = time.Now().Add(stall)
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// lockWithin takes the flock on path, or fails once within has passed.
func lockWithin(ctx context.Context, path string, within time.Duration) (*os.File, error) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	lock, err := awaitLock(ctx, path, within, func() {})
	if err == nil && lock == nil {
		return nil, fmt.Errorf("%s still held after %s", path, within)
	}
	return lock, err
}

func gateLock(runDir string) string { return filepath.Join(runDir, "gates.lock") }

// CloseGates sets every pxa link of ours down and touches nothing else. The
// core unit runs it as ExecStopPost, so a core that stops or crashes while the
// daemon is down never leaves a gate open with no listener behind it. It is
// idempotent and safe to run concurrently with a Worker, which re-verifies and
// reopens after the core is back. It closes at once, whatever anyone holds, then
// closes again under the gate lock in runDir, so a Worker that confirmed the core
// just before it died has its link up closed too.
func CloseGates(ctx context.Context, runDir string) error {
	k := &kernel{gateLock: gateLock(runDir)}
	return k.closeAll(ctx)
}
