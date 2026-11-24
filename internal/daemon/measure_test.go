package daemon

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// measureLoop is a signal loop whose route dials straight to the test's target, with a
// baseline that says the WAN is up.
func measureLoop(t *testing.T) *signalLoop {
	t.Helper()
	baseline := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(baseline.Close)
	s := testLoop(t)
	s.baselines = []probeTarget{{URL: baseline.URL}}
	s.dial = func(_ gateway.ProbeRoute, ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	return s
}

func probeOnce(s *signalLoop, url string, purpose domain.ProbePurpose) domain.Signal {
	return s.probe(context.Background(), gateway.ProbeRoute{Server: ref("a"), Epoch: "e1"}, probeTarget{ID: "t", URL: url}, purpose, s.wanAlone)
}

// perConnection serves requests and passes handler each request's number on its own connection.
func perConnection(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, nth int64)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	type key struct{}
	var conns atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, r.Context().Value(key{}).(*atomic.Int64).Add(1))
	}))
	server.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		conns.Add(1)
		return context.WithValue(ctx, key{}, &atomic.Int64{})
	}
	server.Start()
	t.Cleanup(server.Close)
	return server, &conns
}

func TestReuseIsProvenByTheConnection(t *testing.T) {
	var requests atomic.Int64
	server, conns := perConnection(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	got := probeOnce(measureLoop(t), server.URL, domain.Confirmation)
	if got.Verdict != domain.Succeeded || got.Ping == nil || got.NewConn == nil || got.NoReuse {
		t.Fatalf("signal %+v", got)
	}
	if conns.Load() != 1 || requests.Load() != 3 {
		t.Fatalf("%d connections and %d requests, want 1 and 3", conns.Load(), requests.Load())
	}
}

// A request that lost a packet stands out from the one beside it, whatever their median says.
func TestAProbeMeasuresTheSpreadOfItsReusedRequests(t *testing.T) {
	slowTest(t)
	server, _ := perConnection(t, func(w http.ResponseWriter, _ *http.Request, nth int64) {
		if nth == 2 {
			time.Sleep(200 * time.Millisecond)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	got := probeOnce(measureLoop(t), server.URL, domain.Confirmation)
	if got.Verdict != domain.Succeeded || got.Ping == nil || got.PingSpread == nil {
		t.Fatalf("signal %+v", got)
	}
	if *got.PingSpread < 190*time.Millisecond || *got.PingSpread > *got.Ping*2 {
		t.Fatalf("spread %s beside a median of %s, with one of two requests 200 ms late", *got.PingSpread, *got.Ping)
	}
}

func TestAHeartbeatIsOneRequestAndHasNoPing(t *testing.T) {
	var requests atomic.Int64
	server, _ := perConnection(t, func(w http.ResponseWriter, _ *http.Request, _ int64) { requests.Add(1) })
	got := probeOnce(measureLoop(t), server.URL, domain.Heartbeat)
	if got.Verdict != domain.Succeeded || got.Ping != nil || got.NewConn == nil || got.NoReuse {
		t.Fatalf("signal %+v", got)
	}
	if requests.Load() != 1 {
		t.Fatalf("%d requests", requests.Load())
	}
}

func TestATargetThatClosesConnectionsIsAliveWithNoPing(t *testing.T) {
	server, conns := perConnection(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusNoContent)
	})
	got := probeOnce(measureLoop(t), server.URL, domain.Confirmation)
	if got.Verdict != domain.Succeeded || got.Ping != nil || !got.NoReuse || got.NewConn == nil {
		t.Fatalf("signal %+v", got)
	}
	if conns.Load() != 2 {
		t.Fatalf("%d connections: the probe should stop at the first request not reused", conns.Load())
	}
}

// A reused connection that dies before any response is retried by net/http on a new one,
// so GotConn fires twice for one request. That isn't reuse, and it isn't a failure.
func TestARetryOnANewConnectionIsNoReuse(t *testing.T) {
	server, _ := perConnection(t, func(w http.ResponseWriter, _ *http.Request, nth int64) {
		if nth > 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	got := probeOnce(measureLoop(t), server.URL, domain.Confirmation)
	if got.Verdict != domain.Succeeded || got.Ping != nil || !got.NoReuse {
		t.Fatalf("signal %+v", got)
	}
}

// truncatedSecond breaks the second request after its response began, which net/http
// can't retry away.
func truncatedSecond(w http.ResponseWriter, _ *http.Request, nth int64) {
	if nth == 2 {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err == nil {
			buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nx")
			buf.Flush()
			conn.Close()
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func TestAFollowUpErrorIsAFailedProbe(t *testing.T) {
	slowTest(t)
	hung := func(w http.ResponseWriter, r *http.Request, nth int64) {
		if nth == 2 {
			<-r.Context().Done()
		}
		w.WriteHeader(http.StatusNoContent)
	}
	badStatus := func(w http.ResponseWriter, _ *http.Request, nth int64) {
		if nth == 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	for name, handler := range map[string]func(http.ResponseWriter, *http.Request, int64){"a truncated body": truncatedSecond, "a silent server": hung, "a bad status": badStatus} {
		t.Run(name, func(t *testing.T) {
			server, _ := perConnection(t, handler)
			s := measureLoop(t)
			s.timeout = 500 * time.Millisecond
			got := probeOnce(s, server.URL, domain.Confirmation)
			if got.Verdict != domain.Failed || got.WANExcluded || got.Ping != nil {
				t.Fatalf("signal %+v", got)
			}
		})
	}
}

func TestAFollowUpErrorWithTheWANDownBlamesNobody(t *testing.T) {
	server, _ := perConnection(t, truncatedSecond)
	s := measureLoop(t)
	s.baselines = []probeTarget{{URL: "http://127.0.0.1:1/"}}
	s.baselineGood.Store(time.Now().UnixNano())
	if got := probeOnce(s, server.URL, domain.Confirmation); got.Verdict != domain.Unknown || !got.WANExcluded {
		t.Fatalf("signal %+v", got)
	}
}

// The first byte of the status line arrives at once, and the rest after stall. Only
// GotFirstResponseByte can stop the clock before stall has passed, because client.Do
// waits for the whole header.
func TestTimesStopAtTheFirstResponseByte(t *testing.T) {
	slowTest(t)
	const stall = 300 * time.Millisecond
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			request, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			io.Copy(io.Discard, request.Body)
			conn.Write([]byte("H"))
			time.Sleep(stall)
			conn.Write([]byte("TTP/1.1 204 No Content\r\n\r\n"))
		}
	}()
	s := measureLoop(t)
	began := time.Now()
	got := probeOnce(s, "http://"+listener.Addr().String()+"/", domain.Confirmation)
	if took := time.Since(began); took < 3*stall {
		t.Fatalf("three requests took %v, so the server didn't stall", took)
	}
	if got.Verdict != domain.Succeeded || got.Ping == nil || got.NewConn == nil {
		t.Fatalf("signal %+v", got)
	}
	if *got.Ping >= stall/2 || *got.NewConn >= stall/2 {
		t.Fatalf("ping %v and new connection %v include the wait for the rest of the header", *got.Ping, *got.NewConn)
	}
}

func TestBodiesDrainToEOFUnderACap(t *testing.T) {
	t.Run("a body within the cap drains, and the connection comes back", func(t *testing.T) {
		server, conns := perConnection(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
			io.WriteString(w, strings.Repeat("x", 4096))
		})
		s := measureLoop(t)
		got := s.probe(context.Background(), gateway.ProbeRoute{Server: ref("a"), Epoch: "e1"}, probeTarget{ID: "t", URL: server.URL, MaxBytes: 4096}, domain.Confirmation, s.wanAlone)
		if got.Verdict != domain.Succeeded || got.Ping == nil || conns.Load() != 1 {
			t.Fatalf("signal %+v, %d connections", got, conns.Load())
		}
	})
	t.Run("a body past the cap is alive with no Ping", func(t *testing.T) {
		server, _ := perConnection(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
			io.WriteString(w, strings.Repeat("x", 4097))
		})
		s := measureLoop(t)
		got := s.probe(context.Background(), gateway.ProbeRoute{Server: ref("a"), Epoch: "e1"}, probeTarget{ID: "t", URL: server.URL, MaxBytes: 4096}, domain.Confirmation, s.wanAlone)
		if got.Verdict != domain.Succeeded || got.Ping != nil || !got.NoReuse {
			t.Fatalf("signal %+v", got)
		}
	})
}

func TestProbesAskForNoCompression(t *testing.T) {
	var encodings sync.Map
	server, _ := perConnection(t, func(_ http.ResponseWriter, r *http.Request, nth int64) {
		encodings.Store(nth, r.Header.Get("Accept-Encoding"))
	})
	probeOnce(measureLoop(t), server.URL, domain.Confirmation)
	for _, nth := range []int64{1, 2, 3} {
		if got, ok := encodings.Load(nth); !ok || got != "" {
			t.Fatalf("request %d asked for encoding %v, seen %v", nth, got, ok)
		}
	}
}

func TestProbeTransportSpeaksHTTP1EvenWhereHTTP2IsOffered(t *testing.T) {
	var proto atomic.Value
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { proto.Store(r.Proto) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := probeTransport(func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	})
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig
	resp, err := (&http.Client{Transport: transport}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if proto.Load() != "HTTP/1.1" {
		t.Fatalf("spoke %v", proto.Load())
	}
}

func TestTheRankingTargetHoldsWhileHeartbeatsRotate(t *testing.T) {
	s := testLoop(t)
	s.plan = observationPlan{Targets: []probeTarget{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}}}
	seen := map[domain.TargetID]bool{}
	for range 6 {
		seen[s.nextTarget(ref("a")).ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("heartbeats visited %v", seen)
	}
	for range 6 {
		if got := s.rankingTarget().ID; got != "t1" {
			t.Fatalf("ranking target %s", got)
		}
	}
}

func TestTheRankingTargetMovesWhenItMisbehaves(t *testing.T) {
	loop := func(t *testing.T) *signalLoop {
		s := testLoop(t)
		s.plan = observationPlan{Targets: []probeTarget{{ID: "t1"}, {ID: "t2"}}}
		return s
	}
	noReuse := domain.Signal{Purpose: domain.Confirmation, Target: "t1", Verdict: domain.Succeeded, NoReuse: true}
	withPing := domain.Signal{Purpose: domain.Confirmation, Target: "t1", Verdict: domain.Succeeded, Ping: ms(70)}

	t.Run("a run of samples with no reuse moves it", func(t *testing.T) {
		s := loop(t)
		for range badTargetFails - 1 {
			s.judgeRanking(noReuse)
		}
		if got := s.rankingTarget().ID; got != "t1" {
			t.Fatalf("moved early to %s", got)
		}
		s.judgeRanking(noReuse)
		if got := s.rankingTarget().ID; got != "t2" {
			t.Fatalf("still on %s", got)
		}
	})
	t.Run("a Ping from any server breaks the run", func(t *testing.T) {
		s := loop(t)
		for range 3 * badTargetFails {
			for range badTargetFails - 1 {
				s.judgeRanking(noReuse)
			}
			s.judgeRanking(withPing)
		}
		if got := s.rankingTarget().ID; got != "t1" {
			t.Fatalf("moved to %s", got)
		}
	})
	t.Run("a target blamed for failures moves it", func(t *testing.T) {
		s := loop(t)
		for i := range badTargetFails {
			server := []domain.ServerRef{ref("a"), ref("b")}[i%2]
			s.targets.failed("t1", server)
			s.targets.succeeded("t2", server)
		}
		s.judgeRanking(domain.Signal{Purpose: domain.Confirmation, Target: "t1"})
		if got := s.rankingTarget().ID; got != "t2" {
			t.Fatalf("still on %s", got)
		}
	})
	t.Run("one dead server doesn't move it", func(t *testing.T) {
		s := loop(t)
		for range 3 * badTargetFails {
			s.targets.failed("t1", ref("a"))
			s.targets.succeeded("t2", ref("a"))
			s.judgeRanking(domain.Signal{Purpose: domain.Confirmation, Target: "t1", Verdict: domain.Failed})
		}
		if got := s.rankingTarget().ID; got != "t1" {
			t.Fatalf("moved to %s", got)
		}
	})
	t.Run("heartbeats and results from the old target don't count", func(t *testing.T) {
		s := loop(t)
		beat := noReuse
		beat.Purpose = domain.Heartbeat
		late := noReuse
		late.Target = "t2"
		for range 2 * badTargetFails {
			s.judgeRanking(beat)
			s.judgeRanking(late)
		}
		if got := s.rankingTarget().ID; got != "t1" {
			t.Fatalf("moved to %s", got)
		}
	})
}

// An owner's Test with a Ping is a probe with a Ping like any other, so the judge reads it: four
// checks without reuse, then a Test that reused its connection, and one more check without reuse
// leaves the ranking target where it is.
func TestATestsPingKeepsTheRankingTarget(t *testing.T) {
	server, _ := perConnection(t, func(w http.ResponseWriter, _ *http.Request, _ int64) { w.WriteHeader(http.StatusNoContent) })
	s := measureLoop(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.adopt(ctx, observationPlan{Topology: gateway.Topology{Epoch: "e1", Probes: []gateway.ProbeRoute{{Server: ref("a"), Epoch: "e1"}}},
		Targets: []probeTarget{{ID: "t1", URL: server.URL}, {ID: "t2", URL: server.URL}}, Budget: budget})
	noReuse := domain.Signal{Purpose: domain.Check, Target: "t1", Verdict: domain.Succeeded, NoReuse: true}
	for range badTargetFails - 1 {
		s.judgeRanking(noReuse)
	}
	manuals, frames, done := make(chan manualProbe), make(chan signalFrame, 32), make(chan error, 1)
	go func() { done <- s.run(ctx, make(chan observationPlan), manuals, make(chan gateway.Telemetry), frames) }()
	answer := make(chan domain.Signal, 1)
	manuals <- manualProbe{Server: ref("a"), Reply: answer}
	if sig := <-answer; sig.Ping == nil || sig.Target != "t1" {
		t.Fatalf("the Test reads %+v, want a Ping on t1", sig)
	}
	for heard := false; !heard; {
		select {
		case f := <-frames:
			heard = slices.ContainsFunc(f.Signals, func(s domain.Signal) bool { return s.Purpose == domain.ManualProbe })
		case <-time.After(3 * time.Second):
			t.Fatal("the Test never reached the controller")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.judgeRanking(noReuse)
	if got := s.rankingTarget().ID; got != "t1" {
		t.Fatalf("the ranking target moved to %s over a Test's Ping", got)
	}
}
