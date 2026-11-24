package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// Every baseline target is blocked, as in a censored network, and the WAN works. A failed heartbeat
// has to start rounds with nothing proving the WAN, wherever the kill falls among the baseline ticks.
func TestLiveASilentDeathWithEveryBaselineBlocked(t *testing.T) {
	t.Parallel()
	for _, wait := range []time.Duration{0, 2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second, 11 * time.Second, 13 * time.Second, 17 * time.Second} {
		t.Run(fmt.Sprintf("killed after %s", wait), func(t *testing.T) {
			t.Parallel()
			l := newLive(t, "a", "b", "c", "d")
			l.r.putPicked("g1", "One", "a", "b", "c", "d")
			l.r.putSlot("one", exitIntent(t, "One", domain.GroupExit("g1")))
			l.baselineDown.Store(true)
			l.start()
			deadline := time.Now().Add(90 * time.Second)
			for l.serving()["one"] != "a" {
				if time.Now().After(deadline) {
					t.Fatalf("never on a: %v", l.serving())
				}
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(wait + 30*time.Second)
			if l.serving()["one"] != "a" {
				t.Fatalf("left a before the kill: %v", l.serving())
			}
			l.wantMoved(l.killJustAfterABeat(), "one")
		})
	}
}

// One WAN outage of 40 s, at a different phase each run. In
// half the runs the core answers at once that the upstream is unreachable, and in half nothing answers.
// Failed heartbeats start rounds through all of it, and nothing moves.
func TestLiveAnOutageBlamesNobody(t *testing.T) {
	t.Parallel()
	for i := range 16 {
		t.Run(fmt.Sprintf("run %d", i), func(t *testing.T) {
			t.Parallel()
			fast := i%2 == 0
			l := newLive(t, "a", "b", "c", "d")
			l.r.putPicked("g1", "One", "a", "b", "c", "d")
			intent, err := domain.NewIntent("One", domain.GroupExit("g1"))
			if err != nil {
				t.Fatal(err)
			}
			l.r.putSlot("one", intent)
			reset, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reset.Close() })
			go func() {
				for {
					conn, err := reset.Accept()
					if err != nil {
						return
					}
					conn.Close()
				}
			}()
			var wanDown atomic.Bool
			ctx, cancel := context.WithCancel(context.Background())
			c := l.r.c
			c.now = time.Now
			base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if wanDown.Load() {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(204)
			}))
			t.Cleanup(base.Close)
			s := newSignalLoop(l.r.writer, []probeTarget{{ID: "b1", URL: base.URL}}, "/nonexistent")
			s.dump = func([]uint8) ([]tcpSocket, error) { return nil, nil }
			s.socketsOf = func(int) (map[uint32]bool, error) { return nil, nil }
			s.dial = func(route gateway.ProbeRoute, ctx context.Context, network, _ string) (net.Conn, error) {
				if route.Server.Lineage == "a" {
					time.Sleep(300 * time.Millisecond)
				}
				address := l.good.Listener.Addr().String()
				if wanDown.Load() {
					address = l.hole.Addr().String()
					if fast {
						address = reset.Addr().String()
					}
				}
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			frames := make(chan signalFrame, 64)
			var wg sync.WaitGroup
			wg.Go(func() { _ = s.run(ctx, c.plans, make(chan manualProbe), make(chan gateway.Telemetry), frames) })
			wg.Go(func() { l.worker(ctx, c.goals) })
			wg.Go(func() { _ = c.run(ctx, make(chan envelope), make(chan catalogue.Refresh), frames, l.reports) })
			t.Cleanup(func() {
				cancel()
				wg.Wait()
			})
			deadline := time.Now().Add(30 * time.Second)
			for l.serving()["one"] == "" {
				if time.Now().After(deadline) {
					t.Fatalf("never elected: %v", l.serving())
				}
				time.Sleep(20 * time.Millisecond)
			}
			first := l.serving()["one"]
			time.Sleep(time.Duration(i) * 1900 * time.Millisecond)
			wanDown.Store(true)
			for range 800 {
				time.Sleep(50 * time.Millisecond)
				if now := l.serving()["one"]; now != first {
					t.Fatalf("fast=%t during one outage the slot went from %s to %q", fast, first, now)
				}
			}
			wanDown.Store(false)
			for range 400 {
				time.Sleep(50 * time.Millisecond)
				if now := l.serving()["one"]; now != first {
					t.Fatalf("fast=%t after one outage the slot went from %s to %q", fast, first, now)
				}
			}
		})
	}
}

// One dead server is one failure, whatever groups its slots use, and with nothing to share but it.
func TestLiveThreeDisjointGroupsOnOneDeadServer(t *testing.T) {
	t.Parallel()
	l := newLive(t, "a", "b1", "b2", "b3", "c1", "c2", "c3", "d1", "d2", "d3")
	l.r.putPicked("g1", "One", "a", "b1", "b2", "b3")
	l.r.putPicked("g2", "Two", "a", "c1", "c2", "c3")
	l.r.putPicked("g3", "Three", "a", "d1", "d2", "d3")
	l.r.putSlot("one", exitIntent(t, "One", domain.GroupExit("g1")))
	l.r.putSlot("two", exitIntent(t, "Two", domain.GroupExit("g2")))
	l.r.putSlot("three", exitIntent(t, "Three", domain.GroupExit("g3")))
	l.start()
	l.waitAll(3, "a")
	l.wantMoved(l.killJustAfterABeat(), "one", "two", "three")
}
