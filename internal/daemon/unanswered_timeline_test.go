package daemon

import (
	"net/netip"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// The switching predicate's bound, written out rather than derived, so a timing change that breaks
// the promise fails here.
const predicateBound = 80 * time.Second

// reshape gives a lineage another server under the same Ref, so the slot keeps it and only the
// Worker's view of it changes.
func (l *timeline) reshape(id domain.LineageID, shape func(root domain.Hop) []domain.Hop) {
	l.t.Helper()
	row, _ := l.r.c.world.catalogue.Lookup(id)
	_, hops := row.Server.Closure()
	server, err := domain.NewServer(hops[0].ID, shape(hops[0]))
	if err != nil {
		l.t.Fatal(err)
	}
	row.Server = server
	l.r.seed(row)
}

// upstreams is what the Worker reports for a server, by runtime.go's rule: its root's address when
// that is an IP and dials the WAN, else nothing.
func upstreams(ref domain.ServerRef, server domain.Server) []gateway.Upstream {
	root, hops := server.Closure()
	for _, h := range hops {
		if addr, err := netip.ParseAddr(h.Endpoint.Host); h.ID == root && h.DialVia == "" && err == nil {
			return []gateway.Upstream{{Remote: netip.AddrPortFrom(addr, h.Endpoint.Port), Server: ref}}
		}
	}
	return nil
}

// oneRequest is one device request into the slot through server, as the core shows it, sampled as
// often as the loop samples. On a server that answers, its socket is established and acked and the
// uplink grows once. On one that doesn't, the core's dial stays SYN-SENT, which a dump of established
// sockets never returns, and no counter moves (lab, 6 of 6 runs with the server blackholed).
func (l *timeline) oneRequest(server domain.ServerRef, answers bool) time.Time {
	row, _ := l.r.c.world.catalogue.Lookup(server.Lineage)
	routes := upstreams(server, row.Server)
	acked, unacked := time.Duration(0), uint32(0)
	l.loop.socketsOf = func(int) (map[uint32]bool, error) { return map[uint32]bool{7: true}, nil }
	l.loop.dump = func([]uint8) ([]tcpSocket, error) {
		if !answers || len(routes) == 0 {
			return nil, nil
		}
		return []tcpSocket{{Remote: routes[0].Remote, Inode: 7, Unacked: &unacked, LastAckRecv: &acked}}, nil
	}
	samples := 0
	next := l.now()
	l.onStep = func() {
		if l.now().Before(next) {
			return
		}
		next = next.Add(passiveEvery)
		samples++
		l.loop.plan.Topology.CorePID = 1
		l.loop.plan.Topology.Upstreams = routes
		var signals []domain.Signal
		for id := range l.servers {
			ref := l.r.ref(id)
			up, down := uint64(0), uint64(0)
			if answers && ref == server && samples > 1 {
				up = 1200
			}
			signals = append(signals, domain.Signal{Stamp: domain.Stamp{Epoch: "e1", At: l.now()}, Server: ref, Kind: domain.OutboundCounters, Up: &up, Down: &down})
		}
		passive := append(l.loop.counted(gateway.Telemetry{Signals: signals}), l.loop.sampleSockets()...)
		if len(passive) > 0 {
			l.frame(passive)
		}
	}
	return l.now()
}

// newRequest is a device's new request through the slot as the controller now reads it back: one
// request on a new connection to the slot's server, which is what a heartbeat sends.
func (l *timeline) newRequest() (domain.ServerRef, time.Time, bool) {
	server, serving := l.r.c.state.slots["eu"].Selection.Server()
	if !serving {
		return server, time.Time{}, false
	}
	a := l.servers[string(server.Lineage)](domain.Heartbeat)
	return server, l.now().Add(a.took), a.verdict == domain.Succeeded
}

func addressed(root domain.Hop) []domain.Hop {
	root.Endpoint.Host = "198.51.100.10"
	return []domain.Hop{root}
}

func hostnamed(root domain.Hop) []domain.Hop {
	root.Endpoint.Host = "eu1.provider.example"
	return []domain.Hop{root}
}

func chained(root domain.Hop) []domain.Hop {
	via := root
	via.ID, via.Endpoint.Host = "via", "198.51.100.200"
	root.DialVia = via.ID
	return []domain.Hop{root, via}
}

// The server dies just after a good heartbeat, and a device request, or none, arrives at once. No
// shape of server gives a passive signal for it, so each case waits for the minute heartbeat. With a
// request, a new one through the slot works within 80 s of the first that got no answer. Idle, the
// slot moves within 80 s of the death.
func TestTimelineAnUnansweredServerMovesWithinTheBound(t *testing.T) {
	cases := []struct {
		name    string
		shape   func(domain.Hop) []domain.Hop
		request bool
		dies    behaviour
	}{
		{"one request, IP server", addressed, true, silent(0)},
		{"one request, hostname server", hostnamed, true, silent(0)},
		{"one request, chained server", chained, true, silent(0)},
		{"idle", addressed, false, silent(0)},
		{"idle, slow baseline", addressed, false, silent(baselineTimeout)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newTimeline(t, fourServers)
			current, _ := l.r.c.state.slots["eu"].Selection.Server()
			if c.shape != nil {
				l.reshape(current.Lineage, c.shape)
			}
			l.heartbeatJustBeat()
			from := l.now()
			l.set(current.Lineage, c.dies)
			if c.request {
				from = l.oneRequest(current, false)
			}
			l.runUntil(predicateBound, "move", l.moved)
			m := l.moves[0]
			to, _ := m.to.Server()
			if m.reason != "failureMove" || to.Lineage == current.Lineage {
				t.Fatalf("%s to %+v, want a failure move off %s", m.reason, m.to, current.Lineage)
			}
			if !c.request {
				if took := m.at.Sub(from); took > predicateBound {
					t.Fatalf("idle death found after %s, past %s", took, predicateBound)
				}
				t.Logf("calculated: %s moved %s after the death", c.name, m.at.Sub(from).Round(10*time.Millisecond))
				return
			}
			server, answered, works := l.newRequest()
			if server != to || !works {
				t.Fatalf("a new request through the slot went to %+v and answered %t, want an answer from %+v", server, works, to)
			}
			if took := answered.Sub(from); took > predicateBound {
				t.Fatalf("a new request worked %s after the first unanswered one, past %s", took, predicateBound)
			}
			t.Logf("calculated: %s moved %s and a new request worked %s after the first unanswered request",
				c.name, m.at.Sub(from).Round(10*time.Millisecond), answered.Sub(from).Round(10*time.Millisecond))
		})
	}
}

// A device request to a destination that never answers, through a healthy server, reaches the server
// and gets nothing back. The server's heartbeats keep passing, so the slot stays.
func TestTimelineADeadDestinationOnAHealthyServerMovesNothing(t *testing.T) {
	l := newTimeline(t, fourServers)
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	l.reshape(current.Lineage, addressed)
	l.heartbeatJustBeat()
	l.oneRequest(current, true)
	l.run(3 * predicateBound)
	if l.moved() {
		t.Fatalf("a request to a dead destination moved the slot: %+v", l.moves)
	}
	if server, _ := l.r.c.state.slots["eu"].Selection.Server(); server != current {
		t.Fatalf("serving %+v, want %+v", server, current)
	}
}
