package daemon

import (
	"context"
	"database/sql"
	"net/netip"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

func (l *timeline) beatEvery(ref domain.ServerRef) time.Duration {
	p, _ := l.r.c.observation()
	return p.Beats[ref]
}

// The serving server's heartbeat is a minute apart, flowing or idle, and ten seconds while a suspicion
// is raised on it, until the quiet after the rounds that cleared it.
func TestTheServingHeartbeatIsAMinuteAndTenSecondsUnderSuspicion(t *testing.T) {
	l := newTimeline(t, fourServers)
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	if got := l.beatEvery(current); got != time.Minute {
		t.Fatalf("serving, the heartbeat is every %s, want a minute", got)
	}
	suspicion := l.r.probe(string(current.Lineage), domain.Failed, 0)
	suspicion.Kind, suspicion.AttributionCertain = domain.TCPInfo, true
	l.frame([]domain.Signal{suspicion})
	if got := l.beatEvery(current); got != 10*time.Second {
		t.Fatalf("under suspicion, the heartbeat is every %s, want 10 s", got)
	}
	l.run(l.r.c.timings.PassiveQuiet + 10*time.Second)
	if l.moved() || l.beatEvery(current) != time.Minute {
		t.Fatalf("after rounds that cleared it: moved %v, heartbeat every %s", l.moves, l.beatEvery(current))
	}
}

// stalling has the core's one socket to server stop being acknowledged from died on, as a silent death
// with traffic flowing looks to inet_diag, and samples the sockets as often as the loop does.
func (l *timeline) stalling(server domain.ServerRef, died time.Time) {
	remote := netip.MustParseAddrPort("198.51.100.1:443")
	unacked := uint32(1)
	l.loop.socketsOf = func(int) (map[uint32]bool, error) { return map[uint32]bool{7: true}, nil }
	l.loop.dump = func([]uint8) ([]tcpSocket, error) {
		quiet := max(l.now().Sub(died), 0)
		return []tcpSocket{{Remote: remote, Inode: 7, Unacked: &unacked, LastAckRecv: &quiet}}, nil
	}
	next := l.now()
	l.onStep = func() {
		if l.now().Before(next) {
			return
		}
		next = next.Add(passiveEvery)
		l.loop.plan.Topology.CorePID = 1
		l.loop.plan.Topology.Upstreams = []gateway.Upstream{{Remote: remote, Server: server}}
		if signals := l.loop.sampleSockets(); len(signals) > 0 {
			l.frame(signals)
		}
	}
}

// Open point 2. A silent death with traffic flowing is caught by the stall detector, 10 to 15 s in,
// and then by two rounds, so it moves long before the next heartbeat would have seen it.
func TestAStallIsCaughtLongBeforeTheMinuteHeartbeat(t *testing.T) {
	l := newTimeline(t, fourServers)
	current := l.heartbeatJustBeat()
	died := l.now()
	l.set(current.Lineage, silent(0))
	l.stalling(current, died)
	took := l.runUntil(time.Minute, "move", l.moved)
	if took > 25*time.Second || l.moves[0].reason != "failureMove" {
		t.Fatalf("%s after %s, and the plan's stall plus rounds is about 25 s", l.moves[0].reason, took)
	}
	t.Logf("calculated: a silent death with traffic flowing moved after %s (the next heartbeat was a minute off)", took.Round(10*time.Millisecond))
}

// The heartbeat of a serving server reaches metrics.db whether it passed or failed, one a minute.
func TestServingHeartbeatsAreRecorded(t *testing.T) {
	l := newTimeline(t, fourServers)
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	start := l.now()
	l.run(10 * heartbeatServing)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.r.writer.Run(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+metricsPath(l.dir)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var passed int
	if err := db.QueryRow(`SELECT count(*) FROM probe WHERE purpose=? AND verdict=? AND lineage=? AND ts>=?`,
		int(domain.Heartbeat), int(domain.Succeeded), string(current.Lineage), start.UnixMilli()).Scan(&passed); err != nil {
		t.Fatal(err)
	}
	if passed < 9 || passed > 11 {
		t.Fatalf("%d successful heartbeats recorded in ten minutes, want one a minute", passed)
	}
}

// traffic has every server move bytes at perServer bits a second each way, as the core's counters
// read every telemetry interval.
func (l *timeline) traffic(perServer func() float64) {
	next := l.now()
	var up uint64
	l.onStep = func() {
		if l.now().Before(next) {
			return
		}
		next = next.Add(5 * time.Second)
		up += uint64(perServer() / 8 * 5)
		var signals []domain.Signal
		for id := range l.servers {
			n := up
			signals = append(signals, domain.Signal{Stamp: domain.Stamp{Epoch: "e1", At: l.now()}, Server: l.r.ref(id), Kind: domain.OutboundCounters, Up: &n, Down: &n})
		}
		l.frame(l.loop.counted(gateway.Telemetry{Signals: signals}))
	}
}

// Four servers at 6 Mbps each are 24 Mbps on the line, past the guard, though none is past it alone.
// A slow server then moves nothing, and moves once the line is quiet again.
func TestTheLineGuardIgnoresSlownessOnAFullLine(t *testing.T) {
	l := newTimeline(t, fourServers)
	rate := 6e6
	l.traffic(func() float64 { return rate })
	l.warm()
	current := l.heartbeatJustBeat()
	l.set(current.Lineage, slowed(fourServers[string(current.Lineage)]))
	l.run(3 * heartbeatServing)
	if l.moved() || !l.r.c.evidence.lineBusy {
		t.Fatalf("on a line at %.0f Mbps: moved %+v", 4*rate/1e6, l.moves)
	}
	rate = 1e6
	took := l.runUntil(failover+heartbeatServing, "move", l.moved)
	if l.moves[0].reason != "slownessMove" {
		t.Fatalf("on a quiet line: %s", l.moves[0].reason)
	}
	t.Logf("calculated: slowness moved %s after the line went quiet", took.Round(time.Second))
}
