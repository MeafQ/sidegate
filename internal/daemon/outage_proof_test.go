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

// refusingListener accepts and closes at once, as a core does when the upstream is unreachable.
func refusingListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return l
}

// A baseline answered moments ago, in the time a reused one still counts, and the WAN has died since.
// The round has to ask for itself, whatever a heartbeat beside it would have been told.
func TestARoundBlamesNobodyOnABaselineFromBeforeItBegan(t *testing.T) {
	slowTest(t)
	var down atomic.Bool
	base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			panic(http.ErrAbortHandler)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer base.Close()
	refused := refusingListener(t)
	s := testLoop(t)
	s.now = time.Now
	s.timeout = 300 * time.Millisecond
	s.baselines = []probeTarget{{URL: base.URL}}
	s.dial = func(_ gateway.ProbeRoute, ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, refused.Addr().String())
	}
	if !s.baselineFresh() {
		t.Fatal("the baseline didn't answer")
	}
	down.Store(true)
	if !s.baselineFresh() {
		t.Fatal("this baseline should still be reusable, or this test proves nothing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.adopt(ctx, observationPlan{
		Topology: gateway.Topology{Epoch: "e1", Probes: []gateway.ProbeRoute{{Server: ref("a"), Epoch: "e1"}, {Server: ref("b"), Epoch: "e1"}}},
		Targets:  []probeTarget{{ID: "t1", URL: "http://probe.invalid/"}}, Budget: budget,
		Rounds: []roundRequest{{Campaign: "c", Round: 1, Path: failurePath, Due: time.Now(), Incumbent: ref("a"), Rivals: []domain.ServerRef{ref("b")}}},
	})
	results, rounds := make(chan domain.Signal, 1), make(chan roundDone, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	s.schedule(ctx, &wg, results, rounds)
	select {
	case r := <-rounds:
		for _, sig := range r.signals {
			if sig.Verdict != domain.Unknown || !sig.WANExcluded {
				t.Fatalf("a round with no proof of its own blamed %s: %+v", sig.Server.Lineage, sig)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no round result")
	}
}

// One steady outage where the core answers at once that the upstream is unreachable, starting just
// after a baseline answered, with a's heartbeat coming due moments later. Only the first baseline's
// delay differs between phases, which slides the baseline's phase against the heartbeat's. Real sockets,
// real signal loop and controller.
func TestASteadyOutageRightAfterABaselineMovesNothing(t *testing.T) {
	// A beat that drifts against the 15 s baselines, so the phases below line one up. A minute is four
	// baseline periods and would never slide.
	const beat = 10 * time.Second
	slowTest(t)
	t.Parallel()
	var ran, lined atomic.Int32
	t.Cleanup(func() {
		if ran.Load() > 0 && lined.Load() == 0 {
			t.Error("no phase lined a baseline up with a heartbeat, so this proved nothing")
		}
	})
	for i := range 17 {
		t.Run(fmt.Sprintf("phase%d", i), func(t *testing.T) {
			t.Parallel()
			ran.Add(1)
			l := newLive(t, "a", "b", "c", "d")
			l.r.putPicked("g1", "One", "a", "b", "c", "d")
			intent, err := domain.NewIntent("One", domain.GroupExit("g1"))
			if err != nil {
				t.Fatal(err)
			}
			l.r.putSlot("one", intent)
			refused := refusingListener(t)
			var wanDown, armed atomic.Bool
			var lastBeat, served atomic.Int64
			ctx, cancel := context.WithCancel(context.Background())
			c := l.r.c
			c.now = time.Now
			base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if wanDown.Load() {
					<-r.Context().Done()
					return
				}
				if served.Add(1) == 1 {
					time.Sleep(time.Duration(i) * 150 * time.Millisecond)
				}
				w.WriteHeader(http.StatusNoContent)
				if !armed.Load() {
					return
				}
				last := time.Unix(0, lastBeat.Load())
				if due := time.Until(last.Add(beat)); due > 50*time.Millisecond && due < 800*time.Millisecond {
					armed.Store(false)
					t.Logf("phase %d: baseline answered, %s before a's next heartbeat, and the WAN goes down in 20 ms", i, due.Round(time.Millisecond))
					time.AfterFunc(20*time.Millisecond, func() { wanDown.Store(true) })
				}
			}))
			t.Cleanup(base.Close)
			s := newSignalLoop(l.r.writer, []probeTarget{{ID: "b1", URL: base.URL}}, "/nonexistent")
			s.dump = func([]uint8) ([]tcpSocket, error) { return nil, nil }
			s.socketsOf = func(int) (map[uint32]bool, error) { return nil, nil }
			s.dial = func(route gateway.ProbeRoute, ctx context.Context, network, _ string) (net.Conn, error) {
				if route.Server.Lineage == "a" {
					lastBeat.Store(time.Now().UnixNano())
				}
				address := l.good.Listener.Addr().String()
				if wanDown.Load() {
					address = refused.Addr().String()
				}
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			frames := make(chan signalFrame, 64)
			plans := make(chan observationPlan, 1)
			var wg sync.WaitGroup
			wg.Go(func() {
				for {
					select {
					case p := <-c.plans:
						for ref := range p.Serving {
							p.Beats[ref] = beat
						}
						replace(plans, p)
					case <-ctx.Done():
						return
					}
				}
			})
			wg.Go(func() { _ = s.run(ctx, plans, make(chan manualProbe), make(chan gateway.Telemetry), frames) })
			wg.Go(func() { l.worker(ctx, c.goals) })
			wg.Go(func() { _ = c.run(ctx, make(chan envelope), make(chan catalogue.Refresh), frames, l.reports) })
			t.Cleanup(func() {
				cancel()
				wg.Wait()
			})
			deadline := time.Now().Add(30 * time.Second)
			for l.serving()["one"] != "a" {
				if time.Now().After(deadline) {
					t.Fatalf("never on a: %v", l.serving())
				}
				time.Sleep(20 * time.Millisecond)
			}
			armed.Store(true)
			deadline = time.Now().Add(70 * time.Second)
			for !wanDown.Load() {
				if time.Now().After(deadline) {
					t.Skipf("phase %d: baseline and heartbeat never lined up", i)
				}
				time.Sleep(20 * time.Millisecond)
			}
			lined.Add(1)
			down := time.Now()
			for time.Since(down) < 20*time.Second {
				time.Sleep(50 * time.Millisecond)
				if now := l.serving()["one"]; now != "a" {
					t.Fatalf("phase %d: %s into one steady outage the slot went from a to %q", i, time.Since(down).Round(100*time.Millisecond), now)
				}
			}
			switches, err := l.r.app.Switches(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range switches {
				if e.Reason == reasonNames[domain.FailureMove] {
					t.Fatalf("phase %d: a failure move in an outage: %+v", i, e)
				}
			}
			t.Logf("phase %d: stayed on a through 20 s of outage", i)
		})
	}
}
