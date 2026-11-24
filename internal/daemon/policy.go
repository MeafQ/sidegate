package daemon

import (
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"slices"
	"time"

	"sidegate/internal/domain"
)

// policyMemory is one owner's decisions and timing, never a lasting Degraded flag. Selection,
// LastServed, LeaveBy, KeepOut, Unshown, the backoff and the pick by hand persist in state.db; the rest
// is transient and starts empty after a restart.
type policyMemory struct {
	Selection      domain.Selection
	LastServed     domain.ServerRef // The exact ref the owner's last applied move put it on.
	LeaveBy        time.Time        // Set exactly while the Exit doesn't allow the current server.
	Unshown        *ownerMove       // The owner's half of its slots' pending move, until the first slot shows Selection and starts its clocks.
	KeepOut        map[domain.LineageID]time.Time
	QuietUntil     time.Time // No campaign from a passive suspicion before this.
	SlowQuietUntil time.Time // No campaign from a slow heartbeat before this.
	Campaign       *campaign
	Campaigns      uint32 // Campaigns begun, so a campaign ID never repeats within a process.
	ReadThrough    uint64 // Evidence heard up to here has been read. A probe still in flight hasn't.
	UDPReadThrough uint64 // UDP checks heard up to here have been read. A pick's Test says nothing of UDP, so it reads none.
	Backoff        reopenBackoff
	// When the pool last finished its refill on the ranking site. Once it has, a member admitted later owes
	// its quick probes but no longer orders the pool on the site before.
	RefilledAt time.Time
	Streaks    map[domain.LineageID]int // Comparison rounds each rival has won in a row. A move starts them all over.
	CompareAt  time.Time                // The next comparison round. Zero until this process first serves the owner.
	// The server the owner picked by hand: a press's move until its evaluate, then its boost while the
	// owner serves it. The press commits the move with its answer before any evaluate runs, so a crash in
	// between still moves.
	Hand handPick
}

// ownerMove is what an owner's clocks need of the move that set its selection.
type ownerMove struct {
	From   domain.Selection
	Reason domain.Reason
}

// path is which of the two ways to move a campaign is on.
type path uint8

const (
	failurePath path = iota
	improvePath
	pathCount
)

// pathRule is everything that differs in how a path confirms. A round with no result by Expiry
// ends the campaign.
type pathRule struct {
	Rounds, Rivals  int
	Spacing, Expiry time.Duration
}

// cause is what started a campaign.
type cause uint8

const (
	failedProbe cause = iota
	passiveSuspicion
	slowHeartbeat
	udpFailure
)

var causeNames = map[cause]string{failedProbe: "failedProbe", passiveSuspicion: "passiveSuspicion", slowHeartbeat: "slowHeartbeat", udpFailure: "udpFailure"}

// campaign is one matched confirmation of one path. One field with a Path makes holding a failure
// and an improvement at once unrepresentable.
type campaign struct {
	ID                 domain.CampaignID
	Path               path
	Cause              cause
	Epoch              domain.CoreEpoch
	Incumbent          domain.ServerRef
	Rivals, Tried      []domain.ServerRef
	Wins               []tally // One per rival: rounds it beat the incumbent since it last failed.
	Round              uint8
	Due                time.Time
	Failed, Slow, Lost int       // Rounds the incumbent failed, lost by the severe line, or lost to a rival up where its UDP is broken.
	FailingSince       time.Time // When the first round it failed began.
	Evidence           []domain.EvidenceSummary
	Target             domain.TargetID // A comparison's one site, the ranking target it began on.
}

// tally is what one rival has won in a campaign since it last failed: every round it beat the
// incumbent, and those of them it beat it on the severe line. A move for slowness counts only the
// second, so a win the incumbent's failure or broken UDP gave can't take one, though the campaign
// changed its cause since.
type tally struct{ All, Severe int }

// on says a campaign runs on path p. No campaign runs on neither.
func (c *campaign) on(p path) bool { return c != nil && c.Path == p }

// counts is how many of a rival's wins count toward this campaign's move.
func (c campaign) counts(t tally) int {
	if moveReason(c) == domain.SlownessMove {
		return t.Severe
	}
	return t.All
}

// proposal is a selection change the controller commits before any effect.
type proposal struct {
	Selection domain.Selection
	Reason    domain.Reason
	Evidence  []domain.EvidenceSummary
}

type decision struct {
	Memory policyMemory
	Switch *proposal
}

// unproven says only the passive suspicion or broken UDP that started this campaign backs it so far.
// A failed probe, a slow heartbeat, or a round that failed or read slow, makes it real.
func (c campaign) unproven() bool {
	return (c.Cause == passiveSuspicion || c.Cause == udpFailure) && c.Failed == 0 && c.Slow == 0
}

// udpOnly says nothing but broken UDP backs this campaign, so it can only end on a rival whose UDP works.
func (c campaign) udpOnly() bool { return c.Cause == udpFailure && c.Failed == 0 }

// provenBy makes a suspicion's campaign real. The round may have waited for the bucket until moments
// before its expiry, and it gets its own time from here, since it has only now been let go.
func (c campaign) provenBy(why suspect, now time.Time) campaign {
	c.Cause, c.Due = why.cause, now
	c.Evidence = append(slices.Clone(c.Evidence), why.signal.Summary())
	return c
}

// suspect is why a campaign begins, with the signal that says so when one does.
type suspect struct {
	cause  cause
	signal domain.Signal
}

// decide is pure: no clock, no I/O, no state of its own.
func decide(now time.Time, who domain.Owner, allowed pool, memory policyMemory, ev *evidence, t timings) decision {
	e := evaluation{now: now, who: who, pool: allowed, memory: memory, ev: ev, t: t}
	e.finishRefill()
	pick, picked := memory.Hand.queued()
	switch current, serving := memory.Selection.Server(); {
	case allowed.off.OffVPN():
		return e.offVPN(current, serving)
	case picked:
		return e.picked(pick)
	case serving:
		return e.serving(current)
	case memory.Selection.OffVPN():
		return e.backOnVPN()
	}
	return e.reopen()
}

// picked moves to the server a press picked, once. The press's own pass is the evidence, so the move
// waits for no readiness, settling, WAN proof or back-off, and a closed owner reopens on it. That pass is
// a Test, which never enters the results read here, so everything it answers that was heard before it
// counts as read: a heartbeat that failed unread before it starts no campaign against the pick. It says
// nothing of UDP, so a UDP check stays unread: a pick whose UDP stands broken begins that confirmation
// as it moves, while the WAN is proven, or else once it is, as any move onto it does. A pick its group
// or source no longer holds on that revision moves nothing, and drops a preference for any server but
// the one in use.
func (e *evaluation) picked(pick domain.ServerRef) decision {
	e.memory.ReadThrough = e.ev.heard
	if l, ok := e.pool.member(pick.Lineage); !ok || l.Ref != pick {
		e.memory.Hand = e.memory.Hand.step(e.now, settledOn{e.memory.Selection})
		return decide(e.now, e.who, e.pool, e.memory, e.ev, e.t)
	}
	e.memory.Hand = e.memory.Hand.step(e.now, settledOn{domain.Serving(pick)})
	d := decision{Memory: e.memory}
	if e.memory.Selection != domain.Serving(pick) {
		d = e.propose(domain.Serving(pick), domain.PreferredMove, nil)
	}
	if fault, broken := e.udpFault(pick); broken && e.wanUp() && !d.Memory.Campaign.on(failurePath) {
		e.memory = d.Memory
		d.Memory = e.begin(failurePath, suspect{udpFailure, fault}, pick, e.udpRivals(pick)).Memory
	}
	return d
}

// offVPN takes the slot to its Exit's Direct or Blocked at once, since no server is wanted.
func (e *evaluation) offVPN(current domain.ServerRef, serving bool) decision {
	if e.memory.Selection == e.pool.off {
		return decision{Memory: e.memory}
	}
	var evidence []domain.EvidenceSummary
	if serving {
		evidence = e.lastActive(current)
	}
	return e.propose(e.pool.off, joinReason(e.memory.Selection), evidence)
}

// backOnVPN closes a slot coming back from Direct or Block until a server is ready, since its traffic
// must stop going where the old Exit sent it.
func (e *evaluation) backOnVPN() decision {
	e.memory.Selection = domain.Closed(domain.NoExit)
	if d := e.reopen(); d.Switch != nil {
		return d
	}
	return e.propose(domain.Closed(domain.NoExit), domain.ExitChanged, nil)
}

type evaluation struct {
	now    time.Time
	who    domain.Owner
	pool   pool
	memory policyMemory
	ev     *evidence
	t      timings
}

// severe says slowness starts the failure path and a slow round counts in it, which is so on a group. A
// server Exit has no moves.
func (e *evaluation) severe() bool { return !e.pool.fixed }

// slowness says slowness means something now: on a group, and while the line isn't so full that it
// inflates every server's RTT.
func (e *evaluation) slowness() bool { return e.severe() && !e.ev.lineBusy }

func (e *evaluation) closeFor(reason domain.DownReason, why domain.Reason) decision {
	if current, down := e.memory.Selection.Down(); down && current == reason {
		return decision{Memory: e.memory}
	}
	return e.propose(domain.Closed(reason), why, nil)
}

// propose ends any campaign. LeaveBy lives exactly while the selection isn't allowed, so a rotation
// of a server being left keeps it, and anything else ends it.
func (e *evaluation) propose(selection domain.Selection, reason domain.Reason, evidence []domain.EvidenceSummary) decision {
	m := e.memory
	m.Campaign, m.Streaks = nil, nil
	server, serving := selection.Server()
	if _, allowed := e.pool.member(server.Lineage); !serving || allowed {
		m.LeaveBy = time.Time{}
	}
	return decision{Memory: m, Switch: &proposal{Selection: selection, Reason: reason, Evidence: evidence}}
}

func (e *evaluation) wanUp() bool {
	return e.ev.wan.upWithin(e.now, e.t.WANTTL)
}

// reopen elects the best of what is ready. A slot coming back from Direct or Block owes no back-off.
func (e *evaluation) reopen() decision {
	current, _ := e.memory.Selection.Down()
	members := e.pool.members
	if len(members) == 0 {
		if current == domain.Unselected || current == domain.NoExit || e.pool.fixed {
			return e.closeFor(e.pool.downReason(), e.emptyReason(current))
		}
		return decision{Memory: e.memory}
	}
	if !e.wanUp() {
		return decision{Memory: e.memory}
	}
	if current == domain.NoExit {
		e.memory.Backoff = e.memory.Backoff.waived()
	}
	ready := e.readyOf(members)
	if e.memory.Backoff.holdsOut(e.now) {
		ready = slices.DeleteFunc(ready, func(l domain.Lineage) bool { return l.ID == e.memory.LastServed.Lineage })
	}
	if len(ready) == 0 || !e.settled(ready) {
		return decision{Memory: e.memory}
	}
	return e.propose(domain.Serving(ready[0].Ref), reopenReason(current), e.lastActive(ready[0].Ref))
}

func reopenReason(from domain.DownReason) domain.Reason {
	switch from {
	case domain.Unselected:
		return domain.InitialSelection
	case domain.NoExit:
		return domain.ExitChanged
	}
	return domain.Recovery
}

func (e *evaluation) emptyReason(from domain.DownReason) domain.Reason {
	switch e.pool.empty {
	case domain.ServerRefused:
		return domain.RefusedByCore
	case domain.ServerMissing:
		return domain.ServerLeftFeed
	}
	return reopenReason(from)
}

// settled gives the other servers a closed slot watches a moment to report, so the first server to
// answer doesn't win just for that.
func (e *evaluation) settled(ready []domain.Lineage) bool {
	reported := true
	for _, l := range e.closest() {
		if len(e.lastActive(l.Ref)) == 0 {
			reported = false
		}
	}
	if reported {
		return true
	}
	return slices.ContainsFunc(ready, func(l domain.Lineage) bool {
		sig := e.active(l.Ref)
		return len(sig) == 0 || e.now.Sub(sig[0].Stamp.At) >= e.t.SettleFor
	})
}

// closest is what a closed or leaving slot watches closely: the top of what it could use that isn't
// failing. History can't hold a place for a dead server, and a server that answers again earns one,
// so it collects the passes it needs in seconds.
func (e *evaluation) closest() []domain.Lineage {
	top := slices.DeleteFunc(e.ranked(e.pool.members), func(l domain.Lineage) bool { return e.ev.book.records[l.ID].standing() == failing })
	return top[:min(watchClosed, len(top))]
}

// readyOf lists pool's ready servers in the order, those under KeepOut last: a demoted server that
// works beats a closed slot.
func (e *evaluation) readyOf(pool []domain.Lineage) []domain.Lineage {
	return slices.DeleteFunc(e.ranked(pool), func(l domain.Lineage) bool { return !e.ready(l) })
}

// serving checks what the Exit allows first, since that is no probe's opinion and no outage can
// extend it. Then a failed heartbeat starts or proves a campaign whether or not the WAN is proven. Each
// round proves the WAN or blames nobody, so neither a blocked baseline nor an outage can make one wrong,
// and a censored baseline can't freeze switching. Everything else waits until the WAN is known to be
// up: an improvement, a slow heartbeat and a passive suspicion mean nothing over a WAN that may be down.
func (e *evaluation) serving(current domain.ServerRef) decision {
	if d, done := e.leave(current); done {
		return d
	}
	if l, _ := e.pool.member(current.Lineage); l.Ref != current {
		return e.propose(domain.Serving(l.Ref), domain.RevisionChanged, nil)
	}
	c := e.memory.Campaign
	wan := e.wanUp()
	if c != nil && (!e.stillValid(*c, current) || !wan && c.on(improvePath)) {
		e.memory.Campaign, c = nil, nil
	}
	if why, ok := e.failureCause(current, wan); ok {
		switch {
		case !c.on(failurePath):
			rivals := e.rivals([]domain.ServerRef{current}, failurePath)
			if why.cause == udpFailure {
				rivals = e.udpRivals(current)
			}
			return e.begin(failurePath, why, current, rivals)
		case c.unproven() && (why.cause == failedProbe || why.cause == slowHeartbeat):
			proven := c.provenBy(why, e.now)
			e.memory.Campaign, c = &proven, &proven
		}
	}
	if c != nil {
		return e.advance(*c, current)
	}
	if wan && e.compares() {
		return e.compare(current)
	}
	return decision{Memory: e.memory}
}

// compares says the owner compares its server with its next ones: a group, or a source, of two members
// or more. A server Exit never switches.
func (e *evaluation) compares() bool { return !e.pool.fixed && len(e.pool.members) >= 2 }

// compare begins the comparison round that is due: the current server and the next two by score, on
// the ranking target at the same moment. Rounds come every CompareEvery, at a moment of the period fixed
// by the owner's name, so a restart neither adds one nor brings the next forward.
func (e *evaluation) compare(current domain.ServerRef) decision {
	if e.memory.CompareAt.IsZero() {
		e.memory.CompareAt = compareSlot(e.who, e.now, e.t.CompareEvery)
	}
	if e.now.Before(e.memory.CompareAt) {
		return decision{Memory: e.memory}
	}
	e.memory.CompareAt = compareSlot(e.who, e.now, e.t.CompareEvery)
	d := e.begin(improvePath, suspect{}, current, e.rivals([]domain.ServerRef{current}, improvePath))
	d.Memory.Campaign.Target = e.ev.ranking
	return d
}

func compareSlot(o domain.Owner, now time.Time, every time.Duration) time.Time {
	h := fnv.New64a()
	h.Write([]byte(o.String()))
	phase := time.Duration(h.Sum64() % uint64(every))
	wait := (phase - time.Duration(now.UnixNano()%int64(every)) + every) % every
	if wait == 0 {
		wait = every
	}
	return now.Add(wait)
}

// compared judges a comparison round once every participant has answered. A rival wins when it is ready
// and its live Ping times its factor beats the current server's by both margins, and its third win in a
// row moves the group. A rival that passes without winning, or a round without a Ping, starts its streak
// over. A current server that failed starts the failure path at once, which none of this holds back. A
// round on a full line is void: its Pings measure the line, so no streak moves either way.
func (e *evaluation) compared(c campaign) decision {
	m := e.memory
	m.Campaign = nil
	incumbent, rivals, ok := e.roundAnswers(c)
	switch {
	case !ok && e.now.Sub(c.Due) <= e.t.rule(improvePath).Expiry:
		return decision{Memory: e.memory}
	case !ok:
		m.Streaks = nil
		return decision{Memory: m}
	case incumbent.Verdict == domain.Failed:
		return e.begin(failurePath, suspect{failedProbe, incumbent}, c.Incumbent, e.rivals([]domain.ServerRef{c.Incumbent}, failurePath))
	case e.ev.lineBusy:
		return decision{Memory: m}
	}
	streaks := map[domain.LineageID]int{}
	evidence := []domain.EvidenceSummary{incumbent.Summary()}
	winner := -1
	for i, s := range rivals {
		evidence = append(evidence, s.Summary())
		if !e.wins(incumbent, s) {
			continue
		}
		id := s.Server.Lineage
		streaks[id] = m.Streaks[id] + 1
		if streaks[id] >= compareWins && (winner < 0 || e.preferred(s, rivals[winner])) {
			winner = i
		}
	}
	m.Streaks = streaks
	if winner < 0 {
		return decision{Memory: m}
	}
	e.memory = m
	to, _ := e.pool.member(c.Rivals[winner].Lineage)
	return e.propose(domain.Serving(to.Ref), domain.ImprovementMove, evidence)
}

// wins says a rival beat the current server in a comparison. Each Ping counts times its server's factor,
// so a rival with a worse record must be that much faster, and a failed or recovering rival can't win. A
// preferred current server counts what is left of its boost off its own, so a rival needs less to win as
// the boost fades, unless the rival beats it on the severe line, which reads the raw results. Failure
// and broken UDP read raw values too.
func (e *evaluation) wins(incumbent, rival domain.Signal) bool {
	l, allowed := e.pool.member(rival.Server.Lineage)
	if !allowed || l.Ref != rival.Server || !e.ready(l) || rival.Verdict != domain.Succeeded || incumbent.Ping == nil || rival.Ping == nil {
		return false
	}
	records := e.ev.book.records
	mine := time.Duration(float64(*incumbent.Ping) * factor(records[incumbent.Server.Lineage], e.now))
	theirs := time.Duration(float64(*rival.Ping) * factor(records[rival.Server.Lineage], e.now))
	if p := e.memory.Hand.boosted(e.now); p != nil && p.Server == incumbent.Server.Lineage {
		if severe, _ := severelyBeats(incumbent, rival); !severe {
			mine -= time.Duration(p.left(e.now) * float64(time.Millisecond))
		}
	}
	return beatsByGain(mine, theirs)
}

// leave is the one rule for a current server the Exit no longer allows. It moves as soon as a
// server is ready. Otherwise it stays, until the deadline, on a server that still exists and still
// answers. The deadline is never renewed: a further edit keeps it, and only an Exit that allows the
// server again clears it.
func (e *evaluation) leave(current domain.ServerRef) (decision, bool) {
	if _, ok := e.pool.member(current.Lineage); ok {
		e.memory.LeaveBy = time.Time{}
		return decision{}, false
	}
	if e.memory.LeaveBy.IsZero() {
		e.memory.LeaveBy = e.now.Add(e.t.Handover)
	}
	e.memory.Campaign = nil
	here, why := e.pool.leaving(current.Lineage)
	if ready := e.readyOf(e.pool.members); len(ready) > 0 {
		return e.propose(domain.Serving(ready[0].Ref), why, e.lastActive(ready[0].Ref)), true
	}
	if why == domain.ExitChanged && here.Ref != current {
		return e.propose(domain.Serving(here.Ref), domain.RevisionChanged, nil), true
	}
	if why != domain.ExitChanged || e.lastFailed(current) || !e.now.Before(e.memory.LeaveBy) {
		return e.propose(domain.Closed(e.pool.downReason()), why, e.lastActive(current)), true
	}
	return decision{Memory: e.memory}, true
}

// lastFailed reads the last adjudicated result, so an outage, which gives Unknown, can't close a
// bridge early.
func (e *evaluation) lastFailed(server domain.ServerRef) bool {
	sig := e.active(server)
	return len(sig) > 0 && sig[len(sig)-1].Verdict == domain.Failed
}

// stillValid abandons a campaign when its incumbent is no longer the current server, the core
// restarted, or the Exit left it none of the rivals it had. It reads nothing else, so a rename or an unrelated refresh cancels nothing, and a rival that
// rotates or leaves costs what that rival knew and nothing the others know. It sets no clock, so a
// real failure confirms again from this tick.
func (e *evaluation) stillValid(c campaign, current domain.ServerRef) bool {
	if c.Epoch != e.ev.epoch || c.Incumbent != current || c.Path == improvePath && c.Target != e.ev.ranking {
		return false
	}
	if c.udpOnly() && (!e.udpBroken(current) || !slices.ContainsFunc(c.Rivals, e.udpRival)) {
		return false
	}
	return len(c.Rivals) == 0 || slices.ContainsFunc(c.Rivals, func(ref domain.ServerRef) bool {
		_, ok := e.pool.member(ref.Lineage)
		return ok
	})
}

// followRivals is who the next round asks: the rivals the Exit still allows, each on its lineage's
// current ref, keeping what it has won. A round already asked stays as it was asked, so its answers
// still match.
func (e *evaluation) followRivals(c campaign) ([]domain.ServerRef, []tally) {
	var rivals []domain.ServerRef
	var wins []tally
	for i, ref := range c.Rivals {
		if l, ok := e.pool.member(ref.Lineage); ok {
			rivals, wins = append(rivals, l.Ref), append(wins, c.Wins[i])
		}
	}
	return rivals, wins
}

// failureCause reads what proves a failure first: the last heartbeat and, on a group, a slow one. Then it reads unread passive suspicions, so a suspicion never hides what
// would make its campaign real. Only a failed heartbeat counts while the WAN is unproven. Each trigger
// answers to its own quiet clock, so a suspicion that came to nothing can't delay real slowness, and a
// failed probe has none.
func (e *evaluation) failureCause(current domain.ServerRef, wan bool) (suspect, bool) {
	if s, failed := e.failedProbe(current); failed {
		return suspect{failedProbe, s}, true
	}
	if !wan {
		return suspect{}, false
	}
	if e.slowness() && !e.now.Before(e.memory.SlowQuietUntil) {
		if s, ok := e.slowBeat(current); ok {
			return suspect{slowHeartbeat, s}, true
		}
	}
	if s := e.ev.suspicion(current); !e.now.Before(e.memory.QuietUntil) && s.Verdict == domain.Failed && s.Heard > e.memory.ReadThrough {
		return suspect{passiveSuspicion, s}, true
	}
	if s, ok := e.udpFault(current); ok {
		return suspect{udpFailure, s}, true
	}
	return suspect{}, false
}

// udpFault finds a current server whose UDP is broken, with a rival to take the slot and a check not yet
// read, so a campaign that moved nothing waits for the next check. With no such rival nothing starts,
// and the slot keeps serving on what it has.
func (e *evaluation) udpFault(current domain.ServerRef) (domain.Signal, bool) {
	u := e.ev.udp(current)
	return u.lastFailed, u.state == udpBroken && u.lastFailed.Heard > e.memory.UDPReadThrough && len(e.udpRivals(current)) > 0
}

func (e *evaluation) udpBroken(server domain.ServerRef) bool {
	return e.ev.udp(server).state == udpBroken
}

// udpRival says a server isn't broken and has passed a check, which is what a move for broken UDP may
// go to. A server nobody has checked might be no better than the one being left.
func (e *evaluation) udpRival(server domain.ServerRef) bool {
	return e.ev.udp(server).state == udpPassed
}

// udpRivals ranks who a move for broken UDP may go to.
func (e *evaluation) udpRivals(current domain.ServerRef) []domain.ServerRef {
	working := slices.DeleteFunc(e.others([]domain.ServerRef{current}), func(l domain.Lineage) bool { return !e.udpRival(l.Ref) })
	return e.rivalsAmong(working, failurePath)
}

// failedProbe finds a heartbeat of the server not yet read that failed, whether or not it could prove
// the WAN worked meanwhile. One that couldn't blames nobody, and it still starts rounds, which prove the
// WAN or blame nobody themselves, so a baseline blip on a failing heartbeat costs no heartbeat. Only a
// heartbeat starts anything: where slots overlap, each round would otherwise hand the slots on its rivals
// a failure to start on, and one period between heartbeats is what bounds how many campaigns can run.
func (e *evaluation) failedProbe(server domain.ServerRef) (domain.Signal, bool) {
	for _, last := range slices.Backward(e.ev.results(server)) {
		if last.Purpose == domain.Heartbeat && (last.Verdict != domain.Unknown || last.WANExcluded) {
			return last, (last.Verdict == domain.Failed || last.WANExcluded) && last.Heard > e.memory.ReadThrough
		}
	}
	return domain.Signal{}, false
}

// slowBeat finds a heartbeat far above the server's own recent heartbeats. A server is compared only
// with itself, so xhttp, whose new connections can look quick, can't cause a false start. It only
// starts rounds, and those decide.
func (e *evaluation) slowBeat(server domain.ServerRef) (domain.Signal, bool) {
	var beats []domain.Signal
	var earlier []time.Duration
	for _, s := range e.ev.results(server) {
		if s.Purpose == domain.Heartbeat && s.Verdict == domain.Succeeded && s.NewConn != nil {
			beats = append(beats, s)
		}
	}
	if len(beats) <= beatMinSamples {
		return domain.Signal{}, false
	}
	last := beats[len(beats)-1]
	for _, s := range beats[:len(beats)-1] {
		earlier = append(earlier, *s.NewConn)
	}
	median := domain.Median(earlier)
	return last, last.Heard > e.memory.ReadThrough && *last.NewConn >= beatRatio*median && *last.NewConn-median >= beatFloor
}

func beatsByGain(mine, theirs time.Duration) bool {
	return float64(theirs) <= float64(mine)*(1-gainFraction) && mine-theirs >= gainAbsolute
}

func (e *evaluation) others(exclude []domain.ServerRef) []domain.Lineage {
	return slices.DeleteFunc(slices.Clone(e.pool.members), func(l domain.Lineage) bool { return slices.Contains(exclude, l.Ref) })
}

// rivals ranks who could replace the excluded servers on a path, which only a contender can. A failure
// may reach a server under KeepOut as a last resort, since a demoted server that works beats a closed
// slot.
func (e *evaluation) rivals(exclude []domain.ServerRef, p path) []domain.ServerRef {
	return e.rivalsAmong(e.others(exclude), p)
}

func (e *evaluation) rivalsAmong(others []domain.Lineage, p path) []domain.ServerRef {
	var refs []domain.ServerRef
	for _, l := range e.ranked(others) {
		if contender(e.ev.book.records[l.ID]) {
			refs = append(refs, l.Ref)
		}
	}
	return refs[:min(e.t.rule(p).Rivals, len(refs))]
}

func (e *evaluation) begin(p path, why suspect, incumbent domain.ServerRef, rivals []domain.ServerRef) decision {
	m := e.memory
	m.Campaigns++
	c := &campaign{
		ID: domain.CampaignID(fmt.Sprintf("%s/%d", e.who, m.Campaigns)), Path: p, Cause: why.cause, Epoch: e.ev.epoch,
		Incumbent: incumbent, Rivals: rivals, Wins: make([]tally, len(rivals)), Due: e.now, Round: 1,
	}
	if why.signal.Kind != 0 {
		c.Evidence = []domain.EvidenceSummary{why.signal.Summary()}
	}
	m.Campaign = c
	return decision{Memory: m}
}

// roundAnswers is the current round's result of the incumbent and of each rival, and whether every one
// of them has answered.
func (e *evaluation) roundAnswers(c campaign) (domain.Signal, []domain.Signal, bool) {
	incumbent, ok := e.roundSignal(c, c.Incumbent)
	rivals := make([]domain.Signal, len(c.Rivals))
	for i, ref := range c.Rivals {
		var have bool
		rivals[i], have = e.roundSignal(c, ref)
		ok = ok && have
	}
	return incumbent, rivals, ok
}

// roundSignal finds one server's result in the campaign's current round.
func (e *evaluation) roundSignal(c campaign, server domain.ServerRef) (domain.Signal, bool) {
	for _, s := range slices.Backward(e.ev.roundResults(server)) {
		if s.Campaign == c.ID && s.Round == c.Round {
			return s, true
		}
	}
	return domain.Signal{}, false
}

// end closes a campaign that moved nothing, and starts its own path's clock. A failure path's clock
// never reaches an improvement, and the other way round.
func (e *evaluation) end(c campaign) decision {
	m := e.memory
	m.Campaign = nil
	if c.Path == improvePath {
		return decision{Memory: m}
	}
	m.ReadThrough, m.UDPReadThrough = e.ev.heard, e.ev.heard
	m.QuietUntil = e.now.Add(e.t.PassiveQuiet)
	if c.Cause == slowHeartbeat {
		m.SlowQuietUntil = e.now.Add(e.t.QuietAfter)
	}
	return decision{Memory: m}
}

func (e *evaluation) advance(c campaign, current domain.ServerRef) decision {
	if c.Path == improvePath {
		return e.compared(c)
	}
	if rivals, wins, demoted := e.verified(c); demoted {
		c.Rivals, c.Wins = rivals, wins
		kept := c
		e.memory.Campaign = &kept
	}
	rule := e.t.rule(c.Path)
	incumbent, rivals, ok := e.roundAnswers(c)
	if !ok {
		if e.everyRivalRotated(c) {
			c.Rivals, c.Wins = e.followRivals(c)
			return e.nextRound(c, rule)
		}
		if e.now.Sub(c.Due) > rule.Expiry {
			return e.end(c)
		}
		return decision{Memory: e.memory}
	}
	r := judgeRound(c, incumbent, rivals, e.slowness())
	switch {
	case r.Inconclusive && e.rivalRotated(c):
		c.Rivals, c.Wins = e.followRivals(c)
		return e.nextRound(c, rule)
	case r.Inconclusive:
		return e.end(c)
	case !r.Failed && !r.Slow && !r.Lost && !e.keepsAsking(c, rule):
		return e.end(c)
	case r.Failed && blockedTarget(c, incumbent.Target, rivals):
		return e.nextRound(c, rule)
	}
	if r.Failed {
		if c.Failed == 0 {
			c.FailingSince = incumbent.Stamp.At
		}
		c.Failed++
	}
	if r.Slow {
		c.Slow++
	}
	if r.Lost {
		c.Lost++
	}
	c.Wins = slices.Clone(c.Wins)
	c.Evidence = slices.Clone(c.Evidence)
	c.Evidence = append(c.Evidence, incumbent.Summary())
	for i, s := range rivals {
		c.Evidence = append(c.Evidence, s.Summary())
		switch {
		case r.Beat[i]:
			// A round lost on the severe line was judged on it, so whoever beat the incumbent in it
			// beat it there.
			c.Wins[i].All++
			if r.Slow {
				c.Wins[i].Severe++
			}
		case s.Verdict != domain.Succeeded:
			c.Wins[i] = tally{}
		}
	}
	eligible := func(i int) bool {
		_, allowed := e.pool.member(c.Rivals[i].Lineage)
		return allowed && (!c.udpOnly() || e.udpRival(c.Rivals[i]))
	}
	ready := func(i int) bool {
		l, _ := e.pool.member(c.Rivals[i].Lineage)
		return eligible(i) && e.ready(l)
	}
	if moveReason(c) == domain.SlownessMove {
		// Only a ready rival that itself beat the incumbent on the severe line in the required rounds
		// takes a move for slowness. If one did that isn't ready, nothing moves.
		if winner, wins := e.winner(c, rivals, ready); wins >= rule.Rounds {
			to, _ := e.pool.member(c.Rivals[winner].Lineage)
			return e.propose(domain.Serving(to.Ref), domain.SlownessMove, c.Evidence)
		}
		if _, wins := e.winner(c, rivals, eligible); wins >= rule.Rounds {
			return e.end(c)
		}
		c.Rivals, c.Wins = e.followRivals(c)
		return e.nextRound(c, rule)
	}
	winner, wins := e.winner(c, rivals, eligible)
	switch {
	case wins >= rule.Rounds && r.Slow:
		// The deciding round is one the incumbent passed and lost on the severe line, after failing an
		// earlier one, so only a ready rival with the required wins takes the move. If none has them,
		// nothing moves and the campaign ends. A deciding round it failed, or one for broken UDP, keeps
		// 7c through readyFirst below, whatever came before.
		if winner, wins = e.winner(c, rivals, ready); wins < rule.Rounds {
			return e.end(c)
		}
		to, _ := e.pool.member(c.Rivals[winner].Lineage)
		return e.propose(domain.Serving(to.Ref), moveReason(c), c.Evidence)
	case wins >= rule.Rounds:
		to, _ := e.pool.member(c.Rivals[e.readyFirst(c, winner, rivals, eligible)].Lineage)
		return e.propose(domain.Serving(to.Ref), moveReason(c), c.Evidence)
	case c.Failed >= rule.Rounds && wins == 0:
		e.memory.Backoff = e.memory.Backoff.proven(c.FailingSince)
		return e.propose(domain.Closed(domain.ConfirmedDead), domain.FailureMove, c.Evidence)
	case r.Failed && wins == 0 && len(c.Rivals) > 0:
		c.Tried = append(slices.Clone(c.Tried), c.Rivals...)
		next := e.rivals(append([]domain.ServerRef{c.Incumbent}, c.Tried...), c.Path)
		if len(next) == 0 {
			next, _ = e.followRivals(c)
		}
		c.Rivals, c.Wins = next, make([]tally, len(next))
	default:
		c.Rivals, c.Wins = e.followRivals(c)
	}
	return e.nextRound(c, rule)
}

// verified is the campaign's rivals without the unverified ones, read again at each round's start and
// when it is judged. One that became unverified since it was picked leaves the rounds, which it can't
// win.
func (e *evaluation) verified(c campaign) ([]domain.ServerRef, []tally, bool) {
	var rivals []domain.ServerRef
	var wins []tally
	for i, ref := range c.Rivals {
		if !unverified(e.ev.book.records[ref.Lineage]) {
			rivals, wins = append(rivals, ref), append(wins, c.Wins[i])
		}
	}
	return rivals, wins, len(rivals) < len(c.Rivals)
}

// winner is the rival of those eligible with the most wins that count, ties to the preferred, and
// its count. It is -1 when none is eligible.
func (e *evaluation) winner(c campaign, rivals []domain.Signal, eligible func(int) bool) (int, int) {
	winner, wins := -1, 0
	for i, t := range c.Wins {
		if n := c.counts(t); eligible(i) && (n > wins || n == wins && winner >= 0 && e.preferred(rivals[i], rivals[winner])) {
			winner, wins = i, n
		}
	}
	return winner, wins
}

// readyFirst is who takes a move winner earned for a failure or broken UDP. A server not ready wins a failover only when nothing ready
// passes, so a ready rival that passed this round takes it over one that isn't, whatever their wins and
// whether it beat the incumbent.
func (e *evaluation) readyFirst(c campaign, winner int, rivals []domain.Signal, eligible func(int) bool) int {
	ready := func(i int) bool {
		l, _ := e.pool.member(c.Rivals[i].Lineage)
		return e.ready(l)
	}
	if ready(winner) {
		return winner
	}
	best := -1
	for i := range c.Rivals {
		if eligible(i) && rivals[i].Verdict == domain.Succeeded && ready(i) && (best < 0 || c.counts(c.Wins[i]) > c.counts(c.Wins[best]) || c.counts(c.Wins[i]) == c.counts(c.Wins[best]) && e.preferred(rivals[i], rivals[best])) {
			best = i
		}
	}
	if best < 0 {
		return winner
	}
	return best
}

// blockedTarget says a round proves nothing about its rivals: all of them failed on this target, and a
// rival passed on another one in this campaign. One slot's lone server can't get a target blamed, so
// this is what tells a censored target from a dead group.
func blockedTarget(c campaign, target domain.TargetID, rivals []domain.Signal) bool {
	if len(rivals) == 0 || slices.ContainsFunc(rivals, func(s domain.Signal) bool { return s.Verdict != domain.Failed }) {
		return false
	}
	return slices.ContainsFunc(c.Evidence, func(s domain.EvidenceSummary) bool {
		return s.Server != c.Incumbent && s.Verdict == domain.Succeeded && s.Target != target
	})
}

// keepsAsking says a round that showed nothing wrong doesn't end a campaign with cause to doubt the
// incumbent, on a group. Lost packets slow some requests and not others, so a lossy
// server shows in about two rounds of three, and two in a row would let it off more often than not.
// It stops once no rival could still get its wins.
func (e *evaluation) keepsAsking(c campaign, rule pathRule) bool {
	if c.Path != failurePath || !e.severe() || c.Cause != slowHeartbeat && c.Failed+c.Slow == 0 {
		return false
	}
	best := 0
	for _, t := range c.Wins {
		best = max(best, c.counts(t))
	}
	return best+campaignRounds-int(c.Round) >= rule.Rounds
}

func (e *evaluation) nextRound(c campaign, rule pathRule) decision {
	if c.Round >= campaignRounds {
		return e.end(c)
	}
	c.Round++
	c.Due = e.now.Add(rule.Spacing)
	m := e.memory
	m.Campaign = &c
	return decision{Memory: m}
}

// rivalRotated says a rival's lineage moved to a new ref since this round asked it. Its old route is
// withdrawn, so its answer says nothing about it, and the round runs again on the new one.
func (e *evaluation) rivalRotated(c campaign) bool {
	return slices.ContainsFunc(c.Rivals, func(ref domain.ServerRef) bool {
		l, ok := e.pool.member(ref.Lineage)
		return ok && l.Ref != ref
	})
}

// everyRivalRotated says nothing this round asked about still has a route, so its answers would never
// come or could say nothing, and the campaign asks again on the rivals' new refs.
func (e *evaluation) everyRivalRotated(c campaign) bool {
	return len(c.Rivals) > 0 && !slices.ContainsFunc(c.Rivals, func(ref domain.ServerRef) bool {
		l, _ := e.pool.member(ref.Lineage)
		return l.Ref == ref
	})
}

// moveReason states which path took a move. Any failed round makes it a failure, so a failed round
// can't be labelled latency.
func moveReason(c campaign) domain.Reason {
	switch {
	case c.Failed > 0:
		return domain.FailureMove
	case c.Cause == udpFailure:
		return domain.UDPMove
	}
	return domain.SlownessMove
}

// roundResult is what one round says. Beat has one entry per rival, and a rival that beat the
// incumbent has won this round.
type roundResult struct {
	Inconclusive, Failed, Slow, Lost bool
	Beat                             []bool
}

// judgeRound is pure. A failed incumbent needs only a rival that passed, so a rival with no Ping can
// still win it. Where the round compares a passing incumbent, a rival it can't compare can't beat
// it, and a round in which nothing could be compared is inconclusive, which blames no one. A round for
// broken UDP compares only liveness, since the checks have decided. A passing incumbent never fails a
// round, so broken UDP alone can't close a slot.
func judgeRound(c campaign, incumbent domain.Signal, rivals []domain.Signal, severe bool) roundResult {
	r := roundResult{Beat: make([]bool, len(rivals))}
	unknown := slices.ContainsFunc(rivals, func(s domain.Signal) bool { return s.Verdict == domain.Unknown })
	switch incumbent.Verdict {
	case domain.Unknown:
		r.Inconclusive = true
		return r
	case domain.Failed:
		r.Failed = true
		won := false
		for i, s := range rivals {
			r.Beat[i] = s.Verdict == domain.Succeeded
			won = won || r.Beat[i]
		}
		if !won && unknown {
			return roundResult{Inconclusive: true, Beat: r.Beat}
		}
		return r
	}
	if c.Cause == udpFailure {
		for i, s := range rivals {
			r.Beat[i] = s.Verdict == domain.Succeeded
			r.Lost = r.Lost || r.Beat[i]
		}
		r.Inconclusive = !r.Lost && unknown
		return r
	}
	if !severe {
		return r
	}
	compared, won := false, false
	for i, s := range rivals {
		if s.Verdict != domain.Succeeded {
			continue
		}
		var comparable bool
		r.Beat[i], comparable = severelyBeats(incumbent, s)
		compared = compared || comparable
		won = won || r.Beat[i]
	}
	if !compared {
		r.Inconclusive = unknown || slices.ContainsFunc(rivals, func(s domain.Signal) bool { return s.Verdict == domain.Succeeded })
	}
	r.Slow = compared && won
	return r
}

// severelyBeats is the severe line: Ping apart by at least the larger of severePing and the rival's
// whole Ping, the reused requests' spread apart by severeSpread, or new connection apart by both its
// ratio and its floor. The last two catch loss, where a lucky request or two can show a normal Ping over
// a new connection of two seconds, and neither can move a slot off a server that is only far away.
func severelyBeats(incumbent, rival domain.Signal) (beat, comparable bool) {
	if incumbent.Ping != nil && rival.Ping != nil {
		comparable = true
		beat = *incumbent.Ping-*rival.Ping >= max(severePing, *rival.Ping)
	}
	if incumbent.PingSpread != nil && rival.PingSpread != nil {
		comparable = true
		beat = beat || *incumbent.PingSpread-*rival.PingSpread >= severeSpread
	}
	if incumbent.NewConn != nil && rival.NewConn != nil {
		comparable = true
		beat = beat || *incumbent.NewConn >= severeConnRatio**rival.NewConn && *incumbent.NewConn-*rival.NewConn >= severeConnFloor
	}
	return beat, comparable
}

// preferred breaks a tie between rivals with equal wins: working UDP first, then the better ranked.
func (e *evaluation) preferred(a, b domain.Signal) bool {
	if x, y := e.udpBroken(a.Server), e.udpBroken(b.Server); x != y {
		return y
	}
	ranks := e.ranked(e.pool.members)
	rank := func(s domain.Signal) int {
		if i := slices.IndexFunc(ranks, func(l domain.Lineage) bool { return l.ID == s.Server.Lineage }); i >= 0 {
			return i
		}
		return len(ranks)
	}
	return rank(a) < rank(b)
}

// ranked is pool in the group's order, from the records, with this owner's keep-outs last and its
// preference boosted.
func (e *evaluation) ranked(pool []domain.Lineage) []domain.Lineage {
	return order(pool, e.ev.book.records, e.memory.KeepOut, e.memory.Hand.boosted(e.now), e.orderSite(), e.now)
}

// gap is the boost a press on pick starts: how far its price stands above the best member's, read at raw
// prices with the pick's keep-out released, since the press releases it. The best is the first member in
// that order with a price that ready admits, the pick included. A pick with no price, or none that
// qualifies, has no gap.
func (e *evaluation) gap(pick domain.LineageID) float64 {
	site, records := e.orderSite(), e.ev.book.records
	mine, ok := price(records[pick], site, e.now)
	if !ok {
		return 0
	}
	keepOut := maps.Clone(e.memory.KeepOut)
	delete(keepOut, pick)
	for _, l := range order(e.pool.members, records, keepOut, nil, site, e.now) {
		if best, priced := price(records[l.ID], site, e.now); priced && e.ready(l) {
			return boostTo(mine, best)
		}
	}
	return 0
}

// boostTo is the least boost that brings price down to best's as order subtracts it, so a fresh
// preference ties for first however the subtraction rounds.
func boostTo(price, best float64) float64 {
	if price <= best {
		return 0
	}
	size := price - best
	for price-size > best {
		size = math.Nextafter(size, math.Inf(1))
	}
	return size
}

// active lists server's probe results, oldest first. Unknown results say nothing
// about the server, so they aren't here.
func (e *evaluation) active(server domain.ServerRef) []domain.Signal {
	var out []domain.Signal
	for _, s := range e.ev.results(server) {
		if s.Verdict != domain.Unknown {
			out = append(out, s)
		}
	}
	return out
}

func (e *evaluation) lastActive(server domain.ServerRef) []domain.EvidenceSummary {
	sig := e.active(server)
	if len(sig) == 0 {
		return nil
	}
	return []domain.EvidenceSummary{sig[len(sig)-1].Summary()}
}

// ready says the server's record stands passing, however old its last pass, and that pass is no older
// than a closure that proved the group failing. A failed server stands passing again only after its
// spaced passes, so one that flickers never comes back until it has held still.
func (e *evaluation) ready(l domain.Lineage) bool {
	r := e.ev.book.records[l.ID]
	return r.standing() == passing && !r.Last.Before(e.memory.Backoff.ProvenFailing)
}

// appliedMemory starts the clocks, which run from the apply, never from a proposal. Only an applied
// failure, slowness or UDP move keeps a server out, since an improvement isn't a server's fault.
func appliedMemory(now time.Time, previous policyMemory, event domain.Switch, t timings) policyMemory {
	m := previous
	m.Selection = event.To
	out := map[domain.LineageID]time.Time{}
	for id, until := range previous.KeepOut {
		if now.Before(until) {
			out[id] = until
		}
	}
	to, serving := event.To.Server()
	_, wasServing := event.From.Server()
	if left, ok := forcedOff(event.From, event.To, event.Reason); ok {
		out[left] = now.Add(keepOutFor)
	}
	m.KeepOut = out
	if serving {
		m.LastServed = to
		if !wasServing {
			m.Backoff = m.Backoff.serving(now)
		}
	} else if reason, _ := event.To.Down(); reason == domain.ConfirmedDead {
		m.Backoff = m.Backoff.closed(now, t)
	}
	return m
}

// forcedOff is the server a failure, slowness or UDP move takes the owner off.
func forcedOff(from, to domain.Selection, reason domain.Reason) (domain.LineageID, bool) {
	left, wasServing := from.Server()
	next, serving := to.Server()
	forced := reason == domain.FailureMove || reason == domain.SlownessMove || reason == domain.UDPMove
	return left.Lineage, forced && wasServing && (!serving || next.Lineage != left.Lineage)
}

// supersede is the memory of a move proposed before any slot showed the one it replaces. That one's
// clocks never start, so the server it left by force is kept out from now.
func supersede(now time.Time, m policyMemory, replaced ownerMove, shownTo domain.Selection) policyMemory {
	left, ok := forcedOff(replaced.From, shownTo, replaced.Reason)
	if !ok {
		return m
	}
	m.KeepOut = maps.Clone(m.KeepOut)
	if m.KeepOut == nil {
		m.KeepOut = map[domain.LineageID]time.Time{}
	}
	m.KeepOut[left] = now.Add(keepOutFor)
	return m
}

// looking says the slot needs another server: it is closed, or leaving the one it has.
func (e *evaluation) looking() bool {
	_, serving := e.memory.Selection.Server()
	return !serving || !e.memory.LeaveBy.IsZero()
}

// near lists the servers that get close heartbeats for this slot: what it serves, and, while it is
// looking, the top few it could use. A slot on Direct or Block has none.
func (e *evaluation) near() []domain.ServerRef {
	var out []domain.ServerRef
	if server, serving := e.memory.Selection.Server(); serving {
		out = append(out, server)
	}
	if e.looking() {
		for _, l := range e.closest() {
			if !slices.Contains(out, l.Ref) {
				out = append(out, l.Ref)
			}
		}
	}
	return out
}

// swept lists everything else a looking slot could use. Those get a heartbeat in turn, so a live
// server is found in one sweep, however many dead servers rank above it.
func (e *evaluation) swept() []domain.ServerRef {
	if !e.looking() {
		return nil
	}
	near := e.near()
	var out []domain.ServerRef
	for _, l := range e.pool.members {
		if !slices.Contains(near, l.Ref) {
			out = append(out, l.Ref)
		}
	}
	return out
}
