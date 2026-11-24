package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// A heartbeat begins in a WAN outage's last second and fails after the WAN is back, so the
// baseline it asks after passes. Seen in the control lab's wan step, 2026-11-10: both failures of 12
// begun in the outage's last 5 s, charged to the provider.
func TestAProbeSpanningTheWANsReturnChargesNobody(t *testing.T) {
	l := newTimeline(t, map[string]int{"a": 20, "b": 30})
	l.warm()
	serving := l.heartbeatJustBeat()
	id := serving.Lineage
	l.runUntil(probeTimeout, "the last heartbeat before the outage", func() bool { return len(l.inFlight) == 0 })
	before := l.r.c.evidence.book.records[id]

	outage := true
	for name, good := range l.servers {
		l.servers[name] = func(purpose domain.ProbePurpose) reply {
			if outage {
				return silent(0)(purpose)
			}
			return good(purpose)
		}
	}
	l.loop.baselineGood.Store(l.now().UnixNano())
	l.run(proofOfWAN)
	l.loop.judgeWAN(false)
	l.loop.judgeWAN(false)
	if l.loop.wan.State != domain.WANDown {
		t.Fatalf("the WAN reads %v after two missed baselines", l.loop.wan.State)
	}
	l.wan.read(domain.WANDown, l.now(), l.now())
	beats := l.count["heartbeat"][id]
	l.runUntil(2*heartbeatServing, "a heartbeat in the outage", func() bool { return l.count["heartbeat"][id] > beats })
	l.run(time.Second)
	outage = false
	l.loop.baselineGood.Store(l.now().UnixNano())
	l.loop.judgeWAN(true)
	l.wan.read(domain.WANUp, l.now(), l.now())
	l.runUntil(2*probeTimeout, "the heartbeat's answer", func() bool { return len(l.inFlight) == 0 })

	after := l.r.c.evidence.book.records[id]
	if after.Failures != 0 || len(after.Incidents) != len(before.Incidents) {
		t.Errorf("the server was charged for the outage: %d failures, %d incidents", after.Failures, len(after.Incidents)-len(before.Incidents))
	}
	var spanning []domain.Signal
	for _, sig := range l.r.c.evidence.results(serving) {
		if sig.Purpose == domain.Heartbeat && sig.Stamp.At.After(before.Last) {
			spanning = append(spanning, sig)
		}
	}
	if len(spanning) == 0 || spanning[0].Verdict != domain.Unknown || !spanning[0].WANExcluded {
		t.Errorf("the heartbeat begun in the outage reads %+v, want excluded", spanning)
	}
	l.run(heartbeatServing)
	if l.moved() {
		t.Errorf("the outage moved the slot: %+v", l.moves)
	}
}

// The accepted gap: once an outage has outlasted baselineBlockedAfter its baselines read blocked, so
// a heartbeat spanning the return is charged when a rival's success proves the WAN. A shorter outage
// keeps the guard.
func TestAProbeSpanningTheWANsReturnIsChargedOnlyAfterTheBaselinesReadBlocked(t *testing.T) {
	for _, c := range []struct {
		name    string
		outage  time.Duration
		charged bool
	}{
		{"an outage longer than baselineBlockedAfter", baselineBlockedAfter + time.Second, true},
		{"an outage shorter than baselineBlockedAfter", baselineBlockedAfter / 2, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
			}))
			defer dead.Close()
			s := testLoop(t)
			now := t0
			s.now = func() time.Time { return now }
			s.plan = observationPlan{Topology: gateway.Topology{Epoch: "e1"}}
			s.routes = map[domain.ServerRef]gateway.ProbeRoute{ref("a"): {Server: ref("a"), Epoch: "e1"}, ref("b"): {Server: ref("b"), Epoch: "e1"}}
			s.wan = wanBelief{State: domain.WANUp}

			s.baselineGood.Store(now.UnixNano())
			s.judgeWAN(false)
			s.judgeWAN(false)
			if s.wan.State != domain.WANDown {
				t.Fatalf("the WAN reads %v after two missed baselines", s.wan.State)
			}
			back := now.Add(c.outage)
			now = back.Add(-2 * time.Second)
			s.dial = func(_ gateway.ProbeRoute, ctx context.Context, network, _ string) (net.Conn, error) {
				now = back
				s.baselineMu.Lock()
				s.baselineAt, s.baselineOK = time.Now(), false
				s.baselineMu.Unlock()
				s.proxied.Store(now.UnixNano())
				s.accepted([]domain.Signal{{Stamp: domain.Stamp{Epoch: "e1", At: now}, Server: ref("b"), Kind: domain.ActiveProbe, Purpose: domain.Heartbeat, Verdict: domain.Succeeded}})
				return (&net.Dialer{}).DialContext(ctx, network, dead.Listener.Addr().String())
			}
			sig := s.probe(context.Background(), s.routes[ref("a")], probeTarget{ID: "t", URL: "http://probe.invalid/"}, domain.Heartbeat, s.wanAlone)
			got := s.accepted([]domain.Signal{sig})
			if len(got) != 1 {
				t.Fatalf("the heartbeat gave %d signals, want 1", len(got))
			}
			charged := got[0].Verdict == domain.Failed && !got[0].WANExcluded
			excused := got[0].Verdict == domain.Unknown && got[0].WANExcluded
			if c.charged && !charged || !c.charged && !excused {
				t.Errorf("the heartbeat begun 2 s before the return reads verdict %v, excluded %v, want charged %v", got[0].Verdict, got[0].WANExcluded, c.charged)
			}
		})
	}
}
