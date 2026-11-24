package daemon

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// udpTargets are the resolvers a check asks, from two providers, so one's outage or block can't fail a
// server. A provider never supplies one.
var udpTargets = []string{"8.8.8.8:53", "9.9.9.9:53"}

// udpState is what a server's UDP checks add up to. Only a server whose checks say udpPassed can take a
// slot off a server whose checks say udpBroken.
type udpState uint8

const (
	udpUnknown udpState = iota // Nobody has checked it, or the newest check could say nothing.
	udpPassed
	udpBroken
)

var udpNames = map[udpState]string{udpUnknown: "unknown", udpPassed: "passed", udpBroken: "failed"}

// udpEvidence folds one server's checks. A check that could say nothing neither adds to nor clears a
// streak, since the failures either side of it are still consecutive, but it does stop vouching for a
// server: during a mass failure nobody has passed lately, so nobody is a rival to move to.
type udpEvidence struct {
	state      udpState
	fails      int           // Failures since the last pass.
	lastFailed domain.Signal // The newest failure, which a move for broken UDP cites.
}

func (u *udpEvidence) hear(s domain.Signal) {
	switch s.Verdict {
	case domain.Succeeded:
		u.state, u.fails = udpPassed, 0
	case domain.Failed:
		u.fails++
		u.lastFailed = s
		if u.fails >= udpBrokenAfter {
			u.state = udpBroken
		}
	case domain.Unknown:
		if u.state == udpPassed {
			u.state = udpUnknown
		}
	}
}

// udpStates reads each known server's state, by lineage, for a reader off the controller's goroutine.
func (ev *evidence) udpStates() map[domain.LineageID]udpState {
	out := make(map[domain.LineageID]udpState, len(ev.servers))
	for ref, s := range ev.servers {
		out[ref.Lineage] = s.udp.state
	}
	return out
}

// heldUDP is a serving server's failed check waiting on its rivals' checks to say whose fault it is.
type heldUDP struct {
	signal   domain.Signal
	rivals   []domain.ServerRef
	answered int
	until    time.Time // Past this the rivals can't answer for it, and it says nothing.
}

// pickUDP is the check to start now: a rival a failure waits on first, then a server a slot is on, the
// longest overdue first. It spends the attempts budget and leaves a worker free for a heartbeat. Past the
// budget a check waits, its server stays what it was, and nothing is blamed. With the WAN not known to be
// up a check could say nothing, so it costs nothing either.
func (s *signalLoop) pickUDP(now time.Time) (domain.ServerRef, bool) {
	every := s.plan.UDPEvery
	if every <= 0 || !s.wan.up() || s.inFlight >= s.plan.Budget.Parallel-1 {
		return domain.ServerRef{}, false
	}
	var pick domain.ServerRef
	found := false
	for ref := range s.routes {
		var due time.Time
		switch {
		case s.udpAsk[ref]:
			due = time.Time{}
		case !s.plan.Serving[ref]:
			continue
		default:
			next, known := s.nextUDP[ref]
			if !known {
				next = udpSlot(ref, now, every)
				s.nextUDP[ref] = next
			}
			due = next
		}
		if due.After(now) {
			continue
		}
		if !found || due.Before(s.udpDue(pick)) || due.Equal(s.udpDue(pick)) && compareRefs(ref, pick) < 0 {
			pick, found = ref, true
		}
	}
	if !found || !s.attempts.take(now, 1) {
		return domain.ServerRef{}, false
	}
	if s.udpAsk[pick] {
		delete(s.udpAsk, pick)
	} else {
		s.nextUDP[pick] = now.Add(every)
	}
	return pick, true
}

func (s *signalLoop) udpDue(ref domain.ServerRef) time.Time {
	if s.udpAsk[ref] {
		return time.Time{}
	}
	return s.nextUDP[ref]
}

// udpSlot is a server's first check after now, at a moment of the period fixed by its name. So checks
// spread over the period rather than land together, and a daemon restarting in a loop asks each server
// no more often than one running steadily, with nothing stored.
func udpSlot(ref domain.ServerRef, now time.Time, every time.Duration) time.Time {
	h := fnv.New64a()
	h.Write([]byte(ref.Lineage))
	phase := time.Duration(h.Sum64() % uint64(every))
	wait := (phase - time.Duration(now.UnixNano()%int64(every)) + every) % every
	return now.Add(wait)
}

func (s *signalLoop) startUDP(ctx context.Context, wg *sync.WaitGroup, route gateway.ProbeRoute, results chan<- domain.Signal) {
	turn := s.udpTurn
	s.udpTurn++
	stop := s.routeStops[route.Server]
	wg.Add(1)
	go func() {
		defer wg.Done()
		signal := s.checkUDP(stop.ctx, route, turn)
		select {
		case results <- signal:
		case <-ctx.Done():
		}
	}()
}

// checkUDP asks the targets in turn, starting at this check's place in the rotation, and one answer
// passes. Each asks over its own stream under its share of the probe deadline, so a healthy server costs
// one question and a broken one two. The verdict says what this server did, and attributeUDP decides
// whose fault a failure is. A target the core's own inbound never carried a question to says nothing.
func (s *signalLoop) checkUDP(ctx context.Context, route gateway.ProbeRoute, turn int) domain.Signal {
	sig := domain.Signal{
		Stamp:  domain.Stamp{Epoch: route.Epoch, Generation: route.Generation, At: s.now()},
		Server: route.Server, Kind: domain.ActiveProbe, Purpose: domain.UDPCheck, Verdict: domain.Failed,
	}
	for i := range udpTargets {
		address := udpTargets[(turn+i)%len(udpTargets)]
		sig.Target = domain.TargetID("udp:" + address)
		attempt, cancel := context.WithTimeout(ctx, s.timeout/time.Duration(len(udpTargets)))
		answered, neverAsked := s.askDNS(attempt, route, address)
		cancel()
		switch {
		case answered:
			sig.Verdict = domain.Succeeded
			return sig
		case neverAsked || ctx.Err() != nil:
			sig.Verdict = domain.Unknown
			return sig
		}
	}
	return sig
}

// askDNS asks one question over UDP through route and waits for its answer. A datagram that isn't the
// answer to this question is ignored, and only the deadline ends the wait.
func (s *signalLoop) askDNS(ctx context.Context, route gateway.ProbeRoute, address string) (answered, neverAsked bool) {
	conn, err := s.dial(route, ctx, "udp", address)
	if err != nil {
		return false, true
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	id := uint16(rand.Uint32())
	if _, err := conn.Write(dnsQuery(id)); err != nil {
		return false, false
	}
	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return false, false
		}
		if dnsAnswers(buf[:n], id) {
			return true, false
		}
	}
}

// dnsQuery is one recursive question for dns.google's address, which both targets hold in cache.
func dnsQuery(id uint16) []byte {
	return append(binary.BigEndian.AppendUint16(nil, id), "\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x03dns\x06google\x00\x00\x01\x00\x01"...)
}

// dnsAnswers says p is a response to the question with this ID. Any response code counts, since even a
// refusal crossed the path both ways.
func dnsAnswers(p []byte, id uint16) bool {
	return len(p) >= 12 && binary.BigEndian.Uint16(p) == id && p[2]&0x80 != 0
}

// attributeUDP decides what a finished check says, on the loop, and releases any failure it settles. A
// failure stands only if the WAN reads up and another server's newest check passed within two periods,
// which is the reasoning of a round: a rival passing proves the path, and with nothing proving it nobody
// is blamed. So targets unreachable from every server read as no information, and a check earlier than
// those two periods can't vouch for a third failure in a row. A serving server's failure moves its
// group, so only its own rivals' passes prove it, and until one does it waits for one check each on
// them. A server Exit and a one-member group have none, so their failure stands alone.
func (s *signalLoop) attributeUDP(sig domain.Signal) []domain.Signal {
	if sig.Verdict == domain.Succeeded {
		s.udpPassed[sig.Server] = s.now()
	} else {
		delete(s.udpPassed, sig.Server)
	}
	released := s.settleUDP(sig)
	rivals, weighed := s.plan.UDPRivals[sig.Server]
	switch {
	case sig.Verdict != domain.Failed:
	case !s.wan.up():
		sig.Verdict = domain.Unknown
	case !s.plan.Serving[sig.Server] || !weighed:
		if !s.passedUDP(func(ref domain.ServerRef) bool { return ref != sig.Server }) {
			sig.Verdict = domain.Unknown
		}
	case len(rivals) == 0 || s.passedUDP(func(ref domain.ServerRef) bool { return slices.Contains(rivals, ref) }):
	default:
		s.udpHeld[sig.Server] = heldUDP{signal: sig, rivals: rivals, until: s.now().Add(s.plan.UDPEvery)}
		for _, r := range rivals {
			s.udpAsk[r] = true
		}
		return released
	}
	return append(released, sig)
}

// settleUDP releases the failures that waited on this rival's check: as failed once a rival passed, and
// as unknown once every rival answered without passing. One past its period goes first, saying nothing.
func (s *signalLoop) settleUDP(rival domain.Signal) []domain.Signal {
	now := s.now()
	maps.DeleteFunc(s.udpHeld, func(_ domain.ServerRef, h heldUDP) bool { return now.After(h.until) })
	var out []domain.Signal
	for ref, h := range s.udpHeld {
		if !slices.Contains(h.rivals, rival.Server) {
			continue
		}
		h.answered++
		switch {
		case rival.Verdict == domain.Succeeded:
		case h.answered < len(h.rivals):
			s.udpHeld[ref] = h
			continue
		default:
			h.signal.Verdict = domain.Unknown
		}
		delete(s.udpHeld, ref)
		out = append(out, h.signal)
	}
	return out
}

// passedUDP says one of these servers' newest check passed within two periods.
func (s *signalLoop) passedUDP(among func(domain.ServerRef) bool) bool {
	now := s.now()
	for ref, at := range s.udpPassed {
		if among(ref) && now.Sub(at) <= 2*s.plan.UDPEvery {
			return true
		}
	}
	return false
}
