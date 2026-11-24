package daemon

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
	"sidegate/internal/metrics"
	gateway "sidegate/internal/runtime"
)

// checked is a scene whose servers have had these UDP checks, oldest first.
func (w *scene) checked(id string, verdicts ...domain.Verdict) *scene {
	for _, v := range verdicts {
		w.ingest(signalFrame{WAN: w.wan, Signals: []domain.Signal{{Stamp: domain.Stamp{At: t0}, Server: ref(id), Kind: domain.ActiveProbe, Purpose: domain.UDPCheck, Verdict: v, Target: "udp:8.8.8.8:53"}}})
	}
	return w
}

const (
	pass    = domain.Succeeded
	fail    = domain.Failed
	nothing = domain.Unknown
)

func brokenChecks() []domain.Verdict { return []domain.Verdict{fail, fail, fail} }

func TestUDPChecksAddUp(t *testing.T) {
	for _, c := range []struct {
		name   string
		checks []domain.Verdict
		want   udpState
	}{
		{"never checked", nil, udpUnknown},
		{"one pass", []domain.Verdict{pass}, udpPassed},
		{"one failure counts for nothing", []domain.Verdict{fail}, udpUnknown},
		{"one failure after a pass counts for nothing", []domain.Verdict{pass, fail}, udpPassed},
		{"two failures don't mark it", []domain.Verdict{pass, fail, fail}, udpPassed},
		{"three in a row mark it", []domain.Verdict{fail, fail, fail}, udpBroken},
		{"three in a row after a pass mark it", []domain.Verdict{pass, fail, fail, fail}, udpBroken},
		{"a pass breaks the streak", []domain.Verdict{fail, fail, pass, fail, fail}, udpPassed},
		{"one pass clears a mark", []domain.Verdict{fail, fail, fail, fail, pass}, udpPassed},
		{"no information doesn't count toward a mark", []domain.Verdict{fail, nothing, nothing, nothing, fail}, udpUnknown},
		{"no information doesn't reset a streak", []domain.Verdict{fail, fail, nothing, fail}, udpBroken},
		{"no information doesn't clear a mark", []domain.Verdict{fail, fail, fail, nothing}, udpBroken},
		{"no information stops a pass vouching", []domain.Verdict{pass, nothing}, udpUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newScene(domain.WANUp).checked("a", c.checks...)
			if got := w.udp(ref("a")).state; got != c.want {
				t.Fatalf("state %s, want %s", udpNames[got], udpNames[c.want])
			}
		})
	}
}

func TestAUDPCheckStaysOutOfTCPHealth(t *testing.T) {
	w := newScene(domain.WANUp).healthy("a", 20)
	before := w.results(ref("a"))
	w.checked("a", brokenChecks()...)
	if got := w.results(ref("a")); !slices.EqualFunc(got, before, func(x, y domain.Signal) bool { return x.Heard == y.Heard }) {
		t.Fatalf("active results changed: %d, was %d", len(got), len(before))
	}
	e := evaluation{now: t0, pool: poolOf(lin("a")), ev: w.evidence, t: defaultTimings()}
	if !e.ready(lin("a")) || e.lastFailed(ref("a")) {
		t.Fatal("broken UDP reached TCP health")
	}
}

func TestACoreRestartForgetsUDPAsItDoesActiveResults(t *testing.T) {
	w := newScene(domain.WANUp).checked("a", brokenChecks()...).checked("b", pass)
	w.coreRestarted("e2")
	if a, b := w.udp(ref("a")).state, w.udp(ref("b")).state; a != udpUnknown || b != udpUnknown {
		t.Fatalf("after a core restart a is %s and b is %s", udpNames[a], udpNames[b])
	}
	if got := w.udp(ref("a")).fails; got != 0 {
		t.Fatalf("a streak of %d survived", got)
	}
}

func TestWhenBrokenUDPStartsTheFailurePath(t *testing.T) {
	tm := defaultTimings()
	three := poolOf(lin("a"), lin("b"), lin("c"))
	cases := []struct {
		name   string
		pool   pool
		wan    domain.WANState
		a      []domain.Verdict
		b, c   []domain.Verdict
		rivals []string // Nil means nothing starts.
	}{
		{"a rival that passed", three, domain.WANUp, brokenChecks(), []domain.Verdict{pass}, []domain.Verdict{pass}, []string{"b", "c"}},
		{"only the rival that passed goes in the round", three, domain.WANUp, brokenChecks(), []domain.Verdict{pass}, nil, []string{"b"}},
		{"a rival two failures in still has passed", three, domain.WANUp, brokenChecks(), []domain.Verdict{pass, fail, fail}, nil, []string{"b"}},
		{"one failure counts for nothing", three, domain.WANUp, []domain.Verdict{pass, fail}, []domain.Verdict{pass}, nil, nil},
		{"two failures count for nothing", three, domain.WANUp, []domain.Verdict{pass, fail, fail}, []domain.Verdict{pass}, nil, nil},
		{"no rival at all", poolOf(lin("a")), domain.WANUp, brokenChecks(), nil, nil, nil},
		{"a server Exit has no rival", directOf(lin("a")), domain.WANUp, brokenChecks(), []domain.Verdict{pass}, nil, nil},
		{"rivals nobody has checked", three, domain.WANUp, brokenChecks(), nil, nil, nil},
		{"rivals with no information", three, domain.WANUp, brokenChecks(), []domain.Verdict{pass, nothing}, []domain.Verdict{nothing}, nil},
		{"every rival broken too", three, domain.WANUp, brokenChecks(), brokenChecks(), brokenChecks(), nil},
		{"a broken rival stays out of the round", three, domain.WANUp, brokenChecks(), brokenChecks(), []domain.Verdict{pass}, []string{"c"}},
		{"the WAN down", three, domain.WANDown, brokenChecks(), []domain.Verdict{pass}, nil, nil},
		{"a mark one pass cleared", three, domain.WANUp, append(brokenChecks(), pass), []domain.Verdict{pass}, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newScene(c.wan).healthy("a", 30).healthy("b", 40).healthy("c", 50)
			w.checked("a", c.a...).checked("b", c.b...).checked("c", c.c...)
			d := decide(t0, groupSlot(t), c.pool, policyMemory{Selection: serving("a")}, w.evidence, tm)
			if d.Switch != nil {
				t.Fatalf("switched at once: %+v", d.Switch)
			}
			if c.rivals == nil {
				if d.Memory.Campaign != nil {
					t.Fatalf("a campaign started: %+v", d.Memory.Campaign)
				}
				return
			}
			got := d.Memory.Campaign
			if got == nil || got.Path != failurePath || got.Cause != udpFailure {
				t.Fatalf("campaign %+v", got)
			}
			var names []string
			for _, r := range got.Rivals {
				names = append(names, string(r.Lineage))
			}
			if !slices.Equal(names, c.rivals) {
				t.Fatalf("rivals %v, want %v", names, c.rivals)
			}
		})
	}
}

func TestBrokenUDPMovesAndClosesFlows(t *testing.T) {
	tm := defaultTimings()
	w := newScene(domain.WANUp).healthy("a", 30).healthy("b", 40).checked("a", brokenChecks()...).checked("b", pass)
	memory := policyMemory{Selection: serving("a")}
	d := runCampaign(t, groupSlot(t), poolOf(lin("a"), lin("b")), memory, w, tm, func(c *campaign, w *scene) {
		w.round(c, "a", pass, 30).round(c, "b", pass, 40)
		c.Due = t0
	})
	wantSwitch(t, d, domain.UDPMove, "b")
	if !switchCloses(domain.Switch{Reason: d.Switch.Reason, To: d.Switch.Selection}) {
		t.Fatal("a UDP move leaves open connections on the broken server")
	}
	applied := appliedMemory(t0, memory, domain.Switch{Reason: domain.UDPMove, From: serving("a"), To: serving("b")}, tm)
	if !t0.Before(applied.KeepOut["a"]) {
		t.Fatal("the broken server isn't kept out")
	}
}

// A slot's selection stays what it is, whatever its rounds say, while TCP holds.
func TestBrokenUDPNeverClosesASlot(t *testing.T) {
	tm := defaultTimings()
	two := poolOf(lin("a"), lin("b"))
	three := poolOf(lin("a"), lin("b"), lin("c"))
	for _, c := range []struct {
		name  string
		pool  pool
		setup func(*scene)
		round func(*campaign, *scene) // Nil when no campaign may start.
	}{
		{"its only server broken", poolOf(lin("a")), func(w *scene) {}, nil},
		{"a server Exit", directOf(lin("a")), func(w *scene) { w.checked("b", pass) }, nil},
		{"every server broken", two, func(w *scene) { w.checked("b", brokenChecks()...) }, nil},
		{"the rival failing every round", two, func(w *scene) { w.checked("b", pass) }, func(c *campaign, w *scene) {
			w.round(c, "a", pass, 30).round(c, "b", fail, 0)
		}},
		{"every rival failing every round", three, func(w *scene) { w.checked("b", pass).checked("c", pass) }, func(c *campaign, w *scene) {
			w.round(c, "a", pass, 30).round(c, "b", fail, 0).round(c, "c", fail, 0)
		}},
		{"a round that says nothing", two, func(w *scene) { w.checked("b", pass) }, func(c *campaign, w *scene) {
			w.round(c, "a", pass, 30).round(c, "b", nothing, 0)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newScene(domain.WANUp).healthy("a", 30).healthy("b", 40).healthy("c", 50).checked("a", brokenChecks()...)
			c.setup(w)
			memory := policyMemory{Selection: serving("a")}
			d := decide(t0, groupSlot(t), c.pool, memory, w.evidence, tm)
			if c.round == nil && d.Memory.Campaign != nil {
				t.Fatalf("a campaign started with nowhere to go: %+v", d.Memory.Campaign)
			}
			for i := 0; d.Switch == nil && d.Memory.Campaign != nil; i++ {
				if i > 10 {
					t.Fatal("campaign never ended")
				}
				c.round(d.Memory.Campaign, w)
				d = decide(t0, groupSlot(t), c.pool, d.Memory, w.evidence, tm)
			}
			if d.Switch != nil {
				t.Fatalf("switched: %+v", d.Switch)
			}
			if d.Memory.Selection != serving("a") {
				t.Fatalf("selection %+v", d.Memory.Selection)
			}
			again := decide(t0, groupSlot(t), c.pool, d.Memory, w.evidence, tm)
			if again.Memory.Campaign != nil {
				t.Fatalf("a campaign that moved nothing began again before the next check: %+v", again.Memory.Campaign)
			}
			w.checked("a", fail)
			if c.round == nil {
				return
			}
			if next := decide(t0, groupSlot(t), c.pool, d.Memory, w.evidence, tm); next.Memory.Campaign == nil {
				t.Fatal("the next failed check didn't try again")
			}
		})
	}
}

func TestBrokenUDPDoesntKeepAClosedSlotClosedOrBlockAFailover(t *testing.T) {
	tm := defaultTimings()
	t.Run("a closed slot reopens on the best it has, broken as it is", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 30).healthy("b", 40).checked("a", brokenChecks()...).checked("b", brokenChecks()...)
		d := decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), policyMemory{}, w.evidence, tm)
		wantSwitch(t, d, domain.InitialSelection, "a")
	})
	t.Run("a dead server fails over to a broken rival rather than close", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 40).checked("b", brokenChecks()...).probe("a", time.Second, fail, 0)
		memory := policyMemory{Selection: serving("a")}
		d := runCampaign(t, groupSlot(t), poolOf(lin("a"), lin("b")), memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", fail, 0).round(c, "b", pass, 40)
		})
		wantSwitch(t, d, domain.FailureMove, "b")
	})
	t.Run("a server dead on TCP still closes, and UDP has nothing to do with it", func(t *testing.T) {
		w := newScene(domain.WANUp).checked("a", brokenChecks()...).probe("a", time.Second, fail, 0)
		memory := policyMemory{Selection: serving("a")}
		d := runCampaign(t, groupSlot(t), poolOf(lin("a")), memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", fail, 0)
		})
		wantClosed(t, d, domain.ConfirmedDead, domain.FailureMove)
	})
}

func TestAUDPCampaignGivesWayToWhatTCPSays(t *testing.T) {
	tm := defaultTimings()
	start := func(t *testing.T) (*scene, decision) {
		w := newScene(domain.WANUp).healthy("a", 30).healthy("b", 40).checked("a", brokenChecks()...).checked("b", pass)
		d := decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), policyMemory{Selection: serving("a")}, w.evidence, tm)
		if d.Memory.Campaign == nil || d.Memory.Campaign.Cause != udpFailure {
			t.Fatalf("campaign %+v", d.Memory.Campaign)
		}
		return w, d
	}
	t.Run("only broken UDP backs it, so it spends what a suspicion spends", func(t *testing.T) {
		_, d := start(t)
		if !d.Memory.Campaign.unproven() {
			t.Fatal("a UDP campaign runs for free")
		}
	})
	t.Run("a failed heartbeat makes it real", func(t *testing.T) {
		w, d := start(t)
		w.probe("a", 0, fail, 0)
		next := decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), d.Memory, w.evidence, tm)
		if c := next.Memory.Campaign; c == nil || c.Cause != failedProbe || c.unproven() {
			t.Fatalf("campaign %+v", c)
		}
	})
	t.Run("a failed round is a FailureMove however it began", func(t *testing.T) {
		w, d := start(t)
		for d.Switch == nil {
			w.round(d.Memory.Campaign, "a", fail, 0).round(d.Memory.Campaign, "b", pass, 40)
			d = decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), d.Memory, w.evidence, tm)
		}
		wantSwitch(t, d, domain.FailureMove, "b")
	})
	t.Run("a pass of the incumbent abandons it", func(t *testing.T) {
		w, d := start(t)
		w.checked("a", pass)
		if next := decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), d.Memory, w.evidence, tm); next.Memory.Campaign != nil || next.Switch != nil {
			t.Fatalf("decision %+v", next)
		}
	})
	t.Run("a rival marked meanwhile can't win it", func(t *testing.T) {
		w, d := start(t)
		w.round(d.Memory.Campaign, "a", pass, 30).round(d.Memory.Campaign, "b", pass, 40)
		d = decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), d.Memory, w.evidence, tm)
		w.checked("b", brokenChecks()...)
		if next := decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), d.Memory, w.evidence, tm); next.Memory.Campaign != nil || next.Switch != nil {
			t.Fatalf("decision %+v", next)
		}
	})
	t.Run("the first round's winner needs both rounds", func(t *testing.T) {
		w, d := start(t)
		w.round(d.Memory.Campaign, "a", pass, 30).round(d.Memory.Campaign, "b", pass, 40)
		d = decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), d.Memory, w.evidence, tm)
		if d.Switch != nil {
			t.Fatalf("one round moved it: %+v", d.Switch)
		}
	})
}

func TestBrokenUDPRanksNothingAndBreaksATie(t *testing.T) {
	tm := defaultTimings()
	t.Run("a server nobody has checked ranks as it would", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 20).healthy("b", 90).checked("b", pass)
		wantSwitch(t, decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), policyMemory{}, w.evidence, tm), domain.InitialSelection, "a")
	})
	t.Run("a broken server that works still beats one that failed", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 20).checked("a", brokenChecks()...).probe("b", time.Second, fail, 0)
		wantSwitch(t, decide(t0, groupSlot(t), poolOf(lin("a"), lin("b")), policyMemory{}, w.evidence, tm), domain.InitialSelection, "a")
	})
	t.Run("a tie in a failover goes to the one that works", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20).healthy("c", 90).checked("b", brokenChecks()...).probe("a", time.Second, fail, 0)
		memory := policyMemory{Selection: serving("a")}
		d := runCampaign(t, groupSlot(t), poolOf(lin("a"), lin("b"), lin("c")), memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", fail, 0).round(c, "b", pass, 20).round(c, "c", pass, 90)
		})
		wantSwitch(t, d, domain.FailureMove, "c")
	})
}

// A loop's verdict for each check, in order, as one server after another reports.
func TestAttributionOfAUDPFailure(t *testing.T) {
	check := func(id string, v domain.Verdict) domain.Signal {
		return domain.Signal{Stamp: domain.Stamp{Epoch: "e1", At: t0}, Server: ref(id), Purpose: domain.UDPCheck, Kind: domain.ActiveProbe, Verdict: v}
	}
	type result struct {
		id      string
		verdict domain.Verdict
		after   time.Duration // Since the check before it.
	}
	const every = 5 * time.Minute
	for _, c := range []struct {
		name   string
		wan    domain.WANState
		checks []result
		want   domain.Verdict // The last check's.
	}{
		{"another server's pass proves the path", domain.WANUp, []result{{"b", pass, 0}, {"a", fail, time.Minute}}, fail},
		{"nobody passed", domain.WANUp, []result{{"a", fail, 0}}, nothing},
		{"its own pass proves nothing", domain.WANUp, []result{{"a", pass, 0}, {"a", fail, every}}, nothing},
		{"the WAN down", domain.WANDown, []result{{"b", pass, 0}, {"a", fail, time.Minute}}, nothing},
		{"the WAN unknown", domain.WANUnknown, []result{{"b", pass, 0}, {"a", fail, time.Minute}}, nothing},
		{"the other's newest check failed", domain.WANUp, []result{{"b", pass, 0}, {"b", fail, time.Minute}, {"a", fail, time.Minute}}, nothing},
		{"the other's newest check says nothing", domain.WANUp, []result{{"b", pass, 0}, {"b", nothing, time.Minute}, {"a", fail, time.Minute}}, nothing},
		{"a pass two periods old still counts", domain.WANUp, []result{{"b", pass, 0}, {"a", fail, 2 * every}}, fail},
		{"a pass older than two periods doesn't", domain.WANUp, []result{{"b", pass, 0}, {"a", fail, 2*every + time.Second}}, nothing},
		{"a pass stays a pass with the WAN down", domain.WANDown, []result{{"a", pass, 0}}, pass},
		{"no information stays none", domain.WANUp, []result{{"b", pass, 0}, {"a", nothing, 0}}, nothing},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testLoop(t)
			now := t0
			s.now = func() time.Time { return now }
			s.wan.State = c.wan
			s.plan = observationPlan{UDPEvery: every, Topology: gateway.Topology{Epoch: "e1"}}
			s.routes = map[domain.ServerRef]gateway.ProbeRoute{ref("a"): {}, ref("b"): {}}
			var got []domain.Signal
			for _, r := range c.checks {
				now = now.Add(r.after)
				got = s.accepted([]domain.Signal{check(r.id, r.verdict)})
			}
			if len(got) != 1 || got[0].Verdict != c.want {
				t.Fatalf("%+v, want %d", got, c.want)
			}
		})
	}
}

func TestAUDPCheckSaysNothingOfTheWAN(t *testing.T) {
	s := testLoop(t)
	s.wan = wanBelief{State: domain.WANDown, At: t0.Add(-time.Hour)}
	before := s.wan
	s.plan = observationPlan{UDPEvery: time.Minute, Topology: gateway.Topology{Epoch: "e1"}}
	s.routes = map[domain.ServerRef]gateway.ProbeRoute{ref("a"): {}}
	for _, v := range []domain.Verdict{pass, fail, nothing} {
		s.accepted([]domain.Signal{{Stamp: domain.Stamp{Epoch: "e1", At: t0}, Server: ref("a"), Kind: domain.ActiveProbe, Purpose: domain.UDPCheck, Verdict: v}})
	}
	if s.wan != before {
		t.Fatalf("the WAN reads %+v, was %+v", s.wan, before)
	}
	if s.proxied.Load() != 0 {
		t.Fatal("a check reached what proves the WAN")
	}
}

func TestAUDPCheckNeverReachesMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	options := metrics.DefaultOptions()
	options.FlushEvery = 10 * time.Millisecond
	writer, err := metrics.OpenWriter(path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := metrics.OpenReader(path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- writer.Run(ctx) }()

	s := newSignalLoop(writer, nil, "")
	s.wan.State = domain.WANUp
	s.plan = observationPlan{UDPEvery: time.Minute, Targets: []probeTarget{{ID: "t1"}}, Topology: gateway.Topology{Epoch: "e1"}}
	s.routes = map[domain.ServerRef]gateway.ProbeRoute{ref("a"): {}, ref("b"): {}}
	now := time.Now()
	for _, v := range []domain.Verdict{pass, fail, nothing} {
		s.accepted([]domain.Signal{{Stamp: domain.Stamp{Epoch: "e1", At: now}, Server: ref("a"), Kind: domain.ActiveProbe, Purpose: domain.UDPCheck, Verdict: v}})
	}
	s.accepted([]domain.Signal{{Stamp: domain.Stamp{Epoch: "e1", At: now}, Server: ref("b"), Kind: domain.ActiveProbe, Purpose: domain.Confirmation, Verdict: fail, Target: "t1"}})
	for deadline := time.Now().Add(5 * time.Second); writer.Batches() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no batch was flushed")
		}
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	series, err := reader.Series(context.Background(), metrics.Window{From: now.Add(-time.Hour), To: now.Add(time.Hour), Step: 5 * time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	attempts := map[domain.LineageID]uint64{}
	for _, b := range series.Buckets {
		attempts[b.Lineage] += b.Attempts
	}
	if attempts["a"] != 0 {
		t.Fatalf("a's UDP checks read as %d probes", attempts["a"])
	}
	if attempts["b"] == 0 {
		t.Fatal("the batch held no probe")
	}
}

func TestAWithdrawnRouteOrOldCoreLeavesACheckUnheard(t *testing.T) {
	s := testLoop(t)
	s.plan = observationPlan{UDPEvery: time.Minute, Topology: gateway.Topology{Epoch: "e1"}}
	s.routes = map[domain.ServerRef]gateway.ProbeRoute{ref("a"): {}}
	if got := s.accepted([]domain.Signal{{Stamp: domain.Stamp{Epoch: "e1"}, Server: ref("gone"), Purpose: domain.UDPCheck, Verdict: pass}}); len(got) != 0 {
		t.Fatalf("withdrawn route: %+v", got)
	}
	if got := s.accepted([]domain.Signal{{Stamp: domain.Stamp{Epoch: "e0"}, Server: ref("a"), Purpose: domain.UDPCheck, Verdict: pass}}); len(got) != 0 {
		t.Fatalf("old core: %+v", got)
	}
}

// resolver answers each dial for what address replies with.
type resolver func(address string, query []byte) []byte

func resolverLoop(t *testing.T, answer resolver) (*signalLoop, *[]string) {
	t.Helper()
	s := testLoop(t)
	s.timeout = 200 * time.Millisecond
	var asked []string
	s.dial = func(_ gateway.ProbeRoute, _ context.Context, network, address string) (net.Conn, error) {
		if network != "udp" {
			t.Errorf("network %s", network)
		}
		asked = append(asked, address)
		client, server := net.Pipe()
		go func() {
			buf := make([]byte, 512)
			n, err := server.Read(buf)
			if err != nil {
				return
			}
			if r := answer(address, buf[:n]); r != nil {
				server.Write(r)
			}
		}()
		return client, nil
	}
	return s, &asked
}

func dnsReply(query []byte) []byte {
	out := slices.Clone(query)
	out[2] |= 0x80
	return out
}

func TestAUDPCheckAsksInTurnAndOneAnswerPasses(t *testing.T) {
	slowTest(t)
	route := gateway.ProbeRoute{Server: ref("a"), Epoch: "e1"}
	good, other := udpTargets[0], udpTargets[1]
	dead := func(down string) resolver {
		return func(address string, query []byte) []byte {
			if address == down {
				return nil
			}
			return dnsReply(query)
		}
	}
	for _, c := range []struct {
		name   string
		answer resolver
		turn   int
		asked  []string
		want   domain.Verdict
	}{
		{"a healthy server costs one question", dead(""), 0, []string{good}, pass},
		{"the next turn starts at the other", dead(""), 1, []string{other}, pass},
		{"one provider down fails no server", dead(good), 0, []string{good, other}, pass},
		{"the other provider down fails no server", dead(other), 1, []string{other, good}, pass},
		{"both silent fail", func(string, []byte) []byte { return nil }, 0, []string{good, other}, fail},
		{"another question's answer fails", func(_ string, q []byte) []byte {
			r := dnsReply(q)
			r[0] ^= 0xff
			return r
		}, 0, []string{good, other}, fail},
		{"the question echoed back fails", func(_ string, q []byte) []byte { return slices.Clone(q) }, 0, []string{good, other}, fail},
		{"a stub too short fails", func(_ string, q []byte) []byte { return dnsReply(q)[:4] }, 0, []string{good, other}, fail},
		{"a refusal crossed the path, so it passes", func(_ string, q []byte) []byte {
			r := dnsReply(q)
			r[3] |= 5
			return r
		}, 0, []string{good}, pass},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, asked := resolverLoop(t, c.answer)
			got := s.checkUDP(context.Background(), route, c.turn)
			if got.Verdict != c.want || got.Purpose != domain.UDPCheck || got.Stamp.Epoch != "e1" || got.Server != ref("a") {
				t.Fatalf("%+v", got)
			}
			if !slices.Equal(*asked, c.asked) {
				t.Fatalf("asked %v, want %v", *asked, c.asked)
			}
		})
	}
	t.Run("the core's own inbound not answering says nothing", func(t *testing.T) {
		s := testLoop(t)
		s.dial = func(gateway.ProbeRoute, context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("refused")
		}
		if got := s.checkUDP(context.Background(), route, 0); got.Verdict != nothing {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a withdrawn route says nothing", func(t *testing.T) {
		s, _ := resolverLoop(t, func(string, []byte) []byte { return nil })
		s.timeout = time.Minute
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Millisecond, cancel)
		if got := s.checkUDP(ctx, route, 0); got.Verdict != nothing {
			t.Fatalf("%+v", got)
		}
	})
}

func TestDNSQueryIsOneQuestion(t *testing.T) {
	q := dnsQuery(0xbeef)
	if len(q) != 28 || q[0] != 0xbe || q[1] != 0xef || q[2] != 1 || q[5] != 1 {
		t.Fatalf("query % x", q)
	}
}

func udpLoop(t *testing.T, every time.Duration, ids ...string) (*signalLoop, *time.Time) {
	t.Helper()
	s := testLoop(t)
	now := t0
	s.now = func() time.Time { return now }
	s.wan.State = domain.WANUp
	s.plan = observationPlan{UDPEvery: every, Sources: map[domain.ServerRef]domain.SourceID{}, Serving: map[domain.ServerRef]bool{},
		Budget: probeBudget{Parallel: 2, AttemptsPerMinute: 24}, Topology: gateway.Topology{Epoch: "e1"}}
	s.routes = map[domain.ServerRef]gateway.ProbeRoute{}
	for _, id := range ids {
		s.routes[ref(id)] = gateway.ProbeRoute{Server: ref(id), Epoch: "e1"}
		s.plan.Sources[ref(id)] = "p"
		s.plan.Serving[ref(id)] = true
	}
	s.attempts = newBucket(24, 24, now)
	s.pickUDP(now)
	return s, &now
}

func TestUDPChecksRunOncePerPeriodPerServer(t *testing.T) {
	const every = 5 * time.Minute
	ids := []string{"a", "b", "c", "d", "e", "f"}
	s, now := udpLoop(t, every, ids...)
	last := map[domain.ServerRef]time.Time{}
	count := map[domain.ServerRef]int{}
	for range 3600 {
		*now = now.Add(time.Second)
		for {
			pick, ok := s.pickUDP(*now)
			if !ok {
				break
			}
			if at, seen := last[pick]; seen && now.Sub(at) < every {
				t.Fatalf("%s checked again after %s", pick.Lineage, now.Sub(at))
			}
			last[pick] = *now
			count[pick]++
		}
	}
	for _, id := range ids {
		if n := count[ref(id)]; n < 11 || n > 12 {
			t.Fatalf("%s had %d checks in an hour, want 12 at most", id, n)
		}
	}
}

func TestOnlyAServerASlotIsOnIsChecked(t *testing.T) {
	s, now := udpLoop(t, time.Minute, "a")
	delete(s.plan.Serving, ref("a"))
	*now = now.Add(time.Hour)
	if pick, ok := s.pickUDP(*now); ok {
		t.Fatalf("checked %s", pick.Lineage)
	}
}

func TestUDPChecksStayUnderTheBudget(t *testing.T) {
	t.Run("they leave a worker", func(t *testing.T) {
		s, now := udpLoop(t, time.Minute, "a", "b")
		*now = now.Add(time.Hour)
		s.inFlight = 1
		if _, ok := s.pickUDP(*now); ok {
			t.Fatal("a check took the last worker")
		}
		s.inFlight = 0
		first, ok := s.pickUDP(*now)
		if !ok {
			t.Fatal("no check with a worker free")
		}
		s.inFlight++
		if second, ok := s.pickUDP(*now); ok {
			t.Fatalf("%s and %s ran at once with two workers", first.Lineage, second.Lineage)
		}
	})
	t.Run("they spend the attempts bucket", func(t *testing.T) {
		s, now := udpLoop(t, time.Minute, "a")
		*now = now.Add(time.Hour)
		s.attempts = newBucket(24, 24, *now)
		before := s.attempts.tokens
		if _, ok := s.pickUDP(*now); !ok {
			t.Fatal("no check")
		}
		if s.attempts.tokens != before-1 {
			t.Fatalf("tokens %f, was %f", s.attempts.tokens, before)
		}
	})
	t.Run("they wait out an outage for nothing", func(t *testing.T) {
		s, now := udpLoop(t, time.Minute, "a")
		*now = now.Add(time.Hour)
		s.wan.State = domain.WANDown
		if _, ok := s.pickUDP(*now); ok {
			t.Fatal("a check ran with the WAN down")
		}
	})
	t.Run("no period means no checks", func(t *testing.T) {
		s, now := udpLoop(t, 0, "a")
		*now = now.Add(time.Hour)
		if _, ok := s.pickUDP(*now); ok {
			t.Fatal("a check ran")
		}
	})
}

func TestAUDPSlotIsFixedAcrossARestart(t *testing.T) {
	const every = 5 * time.Minute
	a := udpSlot(ref("a"), t0, every)
	if wait := a.Sub(t0); wait < 0 || wait >= every {
		t.Fatalf("first check after %s", wait)
	}
	if again := udpSlot(ref("a"), t0.Add(10*time.Second), every); a.Sub(t0) >= 10*time.Second && !again.Equal(a) {
		t.Fatalf("a restart moved the slot from %s to %s", a, again)
	}
	if b := udpSlot(ref("much-longer-name"), t0, every); b.Equal(a) {
		t.Fatal("two servers share one moment")
	}
}

// udpFails is a server whose TCP is what it was, and whose UDP checks fail at their full cost.
func udpFails(inner behaviour) behaviour {
	return func(purpose domain.ProbePurpose) reply {
		if purpose == domain.UDPCheck {
			return reply{verdict: domain.Failed, took: probeTimeout}
		}
		return inner(purpose)
	}
}

func (l *timeline) waitForChecks(ids ...string) {
	l.t.Helper()
	l.runUntil(2*defaultTimings().UDPCheckEvery, "first checks", func() bool {
		for _, id := range ids {
			if l.r.c.evidence.udp(l.r.ref(id)).state != udpPassed {
				return false
			}
		}
		return true
	})
}

// beside puts slot pin on a server Exit to a rival of eu's server, so that rival's checks run too and a
// failure on eu's server has a pass to be measured against. It returns eu's server and the rival.
func (l *timeline) beside() (current, rival domain.LineageID) {
	l.t.Helper()
	current = l.r.server("eu")
	rival = "a"
	if current == rival {
		rival = "b"
	}
	l.r.putSlot("pin", exitIntent(l.t, "Pin", domain.ServerExit(rival)))
	l.runUntil(time.Minute, "pin serving", func() bool { return l.r.server("pin") == rival && l.r.operation("pin") == "" })
	l.waitForChecks(string(current), string(rival))
	l.moves = nil
	return current, rival
}

func TestTimelineOfBrokenUDP(t *testing.T) {
	every := defaultTimings().UDPCheckEvery
	t.Run("the selected server's UDP breaks beside a server that passes", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		current, rival := l.beside()
		l.set(current, udpFails(healthy(fourServers[string(current)])))
		took := l.runUntil(4*every, "a UDP move", l.moved)
		if l.moves[0].reason != "udpMove" || l.moves[0].slot != "eu" {
			t.Fatalf("moved %+v", l.moves[0])
		}
		if took < 2*every || took > 3*every+time.Minute {
			t.Fatalf("moved after %s, and three checks take 10 to 15 min", took)
		}
		t.Logf("calculated: moved after %s", took.Round(time.Second))
		if got := l.r.server("eu"); got != rival {
			t.Fatalf("on %s, want the one rival whose UDP was checked, %s", got, rival)
		}
		if got := l.r.c.evidence.udp(l.r.ref(string(current))).state; got != udpBroken {
			t.Fatalf("the left server reads %s", udpNames[got])
		}
		if !switchCloses(domain.Switch{Reason: domain.UDPMove, To: l.moves[0].to}) || l.r.targetOf("eu").CloseFlows == "" {
			t.Fatal("the move owes no close")
		}
	})
	t.Run("the selected server's UDP breaks, and one check each on the next two blames it", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		current := l.r.server("eu")
		l.waitForChecks(string(current))
		l.set(current, udpFails(healthy(fourServers[string(current)])))
		took := l.runUntil(4*every, "a UDP move", l.moved)
		if l.moves[0].reason != "udpMove" || l.r.server("eu") != "b" {
			t.Fatalf("moved %+v, on %s", l.moves[0], l.r.server("eu"))
		}
		t.Logf("calculated: moved after %s", took.Round(time.Second))
		if l.checks["b"] != 1 || l.checks["c"] != 1 || l.checks["d"] != 0 {
			t.Fatalf("checks %v, want one on each of the next two, whose passes vouch for the failures after it, and none below them", l.checks)
		}
	})
	t.Run("every checked server's UDP breaks at once", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		current, rival := l.beside()
		for id, ms := range fourServers {
			l.set(domain.LineageID(id), udpFails(healthy(ms)))
		}
		l.run(time.Hour)
		if len(l.moves) != 0 {
			t.Fatalf("moved: %+v", l.moves)
		}
		if u := l.r.c.evidence.udp(l.r.ref(string(current))); u.state != udpUnknown || u.fails >= udpBrokenAfter {
			t.Fatalf("the group's %s reads %s after %d failures its rivals shared", current, udpNames[u.state], u.fails)
		}
		if u := l.r.c.evidence.udp(l.r.ref(string(rival))); u.state != udpBroken {
			t.Fatalf("%s, a server Exit's, reads %s: with no rival to weigh it against its failure stands", rival, udpNames[u.state])
		}
		for id := range fourServers {
			l.set(domain.LineageID(id), healthy(fourServers[id]))
		}
		l.waitForChecks(string(current), string(rival))
	})
	t.Run("a server another slot is on breaks while eu's passes", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		_, rival := l.beside()
		l.set(rival, udpFails(healthy(fourServers[string(rival)])))
		l.run(4 * every)
		if got := l.r.c.evidence.udp(l.r.ref(string(rival))).state; got != udpBroken {
			t.Fatalf("%s reads %s, with eu's server passing", rival, udpNames[got])
		}
	})
	t.Run("a server Exit stays on its broken server", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		current, _ := l.beside()
		l.r.setExit("eu", domain.ServerExit(current))
		l.run(time.Minute)
		l.moves = nil
		l.set(current, udpFails(healthy(fourServers[string(current)])))
		l.run(4 * every)
		if got := l.r.c.evidence.udp(l.r.ref(string(current))).state; got != udpBroken {
			t.Fatalf("%s reads %s", current, udpNames[got])
		}
		if len(l.moves) != 0 || l.r.server("eu") != current {
			t.Fatalf("moves %+v, on %s", l.moves, l.r.server("eu"))
		}
	})
	t.Run("a one-member group reports its server's UDP down with no rival to check", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		current := l.r.server("eu")
		l.r.putPicked("alone", "Alone", current)
		l.r.setExit("eu", domain.GroupExit("alone"))
		l.run(time.Minute)
		l.set(current, udpFails(healthy(fourServers[string(current)])))
		for id := range l.checks {
			l.checks[id] = 0
		}
		l.run(4 * every)
		if got := l.r.c.evidence.udp(l.r.ref(string(current))).state; got != udpBroken || l.moved() {
			t.Fatalf("%s reads %s, moves %+v", current, udpNames[got], l.moves)
		}
		for id, n := range l.checks {
			if id != string(current) && n != 0 {
				t.Fatalf("%s, no rival of a one-member group, had %d checks", id, n)
			}
		}
	})
	t.Run("a check comes once per period, and only on a server a slot is on", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		current, rival := l.beside()
		for id := range l.checks {
			l.checks[id] = 0
		}
		l.run(time.Hour)
		for id := range fourServers {
			n := l.checks[id]
			switch domain.LineageID(id) {
			case current, rival:
				if n < 11 || n > 12 {
					t.Fatalf("%s had %d checks in an hour", id, n)
				}
			default:
				if n != 0 {
					t.Fatalf("%s, which no slot is on, had %d checks", id, n)
				}
			}
		}
	})
	t.Run("a restart starts over from no information", func(t *testing.T) {
		l := newTimeline(t, fourServers)
		current, _ := l.beside()
		l.set(current, udpFails(healthy(fourServers[string(current)])))
		l.runUntil(4*every, "a mark", func() bool { return l.r.c.evidence.udp(l.r.ref(string(current))).state == udpBroken || l.moved() })
		l.restart()
		if got := l.r.c.evidence.udp(l.r.ref(string(current))).state; got != udpUnknown {
			t.Fatalf("after a restart it reads %s", udpNames[got])
		}
	})
}

func TestServerPageShowsUDP(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.r.c.evidence.server(l.r.ref("a")).udp = udpEvidence{state: udpPassed}
	l.r.c.evidence.server(l.r.ref("b")).udp = udpEvidence{state: udpPassed}
	l.r.c.evidence.server(l.r.ref("d")).udp = udpEvidence{state: udpBroken}
	l.r.c.evidence.server(l.r.ref("c")).udp = udpEvidence{state: udpUnknown}
	l.r.c.publish(l.now())
	page, err := l.r.app.Servers(context.Background(), PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[domain.LineageID]string{}
	for _, item := range page.Items {
		got[item.ID] = item.UDP
	}
	for id, want := range map[domain.LineageID]string{"a": "passed", "b": "passed", "c": "unknown", "d": "failed"} {
		if got[id] != want {
			t.Errorf("%s reads %q, want %q", id, got[id], want)
		}
	}
}

// a serves and fails its UDP checks while x, serving another group, passes every one of its own. x's
// passes say the path works, but a's move is to b, so a's failure waits for b's check, and b's pass is
// what lets a's group move.
func TestAPassInAnotherGroupDoesntStandInForTheRivalsCheck(t *testing.T) {
	s := testLoop(t)
	now := t0
	s.now = func() time.Time { return now }
	s.wan.State = domain.WANUp
	a, b, x := ref("a"), ref("b"), ref("x")
	s.plan = observationPlan{Topology: gateway.Topology{Epoch: "e1"}, UDPEvery: 5 * time.Minute, Serving: map[domain.ServerRef]bool{a: true, x: true},
		UDPRivals: map[domain.ServerRef][]domain.ServerRef{a: {b}, x: nil}}
	s.routes = map[domain.ServerRef]gateway.ProbeRoute{a: {}, b: {}, x: {}}
	check := func(server domain.ServerRef, v domain.Verdict) domain.Signal {
		return domain.Signal{Stamp: domain.Stamp{At: now, Epoch: "e1"}, Server: server, Kind: domain.ActiveProbe, Purpose: domain.UDPCheck, Verdict: v}
	}
	if got := s.accepted([]domain.Signal{check(x, pass), check(a, fail)}); len(got) != 1 || got[0].Server != x {
		t.Fatalf("a's failure was settled at once as %+v, want it held for b", got)
	}
	if !s.udpAsk[b] {
		t.Fatal("b's UDP wasn't asked for")
	}
	now = now.Add(10 * time.Second)
	if got := s.accepted([]domain.Signal{check(b, pass)}); len(got) != 2 || got[0].Server != a || got[0].Verdict != fail {
		t.Fatalf("b's pass released %+v, want a's failure standing", got)
	}
	now = now.Add(5 * time.Minute)
	if got := s.accepted([]domain.Signal{check(a, fail)}); len(got) != 1 || got[0].Verdict != fail {
		t.Fatalf("a's next failure reads %+v, want it standing on b's recent pass", got)
	}
}

// A serving server's failure held for its rival's check says nothing once its period is over, even
// when that check passes just after: the WAN is up throughout, but no worker came free to start a
// check, so nothing dropped the hold before the late pass arrived.
func TestAHeldUDPFailurePastItsPeriodSaysNothing(t *testing.T) {
	s := testLoop(t)
	now := t0
	s.now = func() time.Time { return now }
	s.wan.State = domain.WANUp
	a, b := ref("a"), ref("b")
	const every = 5 * time.Minute
	s.plan = observationPlan{Topology: gateway.Topology{Epoch: "e1"}, UDPEvery: every, Serving: map[domain.ServerRef]bool{a: true},
		UDPRivals: map[domain.ServerRef][]domain.ServerRef{a: {b}}}
	s.routes = map[domain.ServerRef]gateway.ProbeRoute{a: {}, b: {}}
	check := func(server domain.ServerRef, v domain.Verdict) domain.Signal {
		return domain.Signal{Stamp: domain.Stamp{At: now, Epoch: "e1"}, Server: server, Kind: domain.ActiveProbe, Purpose: domain.UDPCheck, Verdict: v}
	}
	if got := s.accepted([]domain.Signal{check(a, fail)}); len(got) != 0 {
		t.Fatalf("a's failure was settled at once as %+v, want it held for b", got)
	}
	now = now.Add(every + time.Second)
	if got := s.accepted([]domain.Signal{check(b, pass)}); len(got) != 1 || got[0].Server != b {
		t.Fatalf("b's pass after a's hold ran out released %+v, want b's pass alone", got)
	}
}

// a's UDP breaks in a group of a, two working rivals and fourteen unverified members. No round measures
// the fourteen, which couldn't win it, so its rounds run within their expiry and the group moves to a
// working rival.
func TestUnverifiedMembersNeverHoldUpAUDPFailover(t *testing.T) {
	pings := map[string]int{"a": 40, "b": 50, "c": 60}
	var extras []domain.LineageID
	for i := range 14 {
		id := "x" + string(rune('a'+i))
		pings[id] = 100 + i
		extras = append(extras, domain.LineageID(id))
	}
	l := newTimeline(t, pings)
	l.stride = time.Second
	for _, id := range extras {
		r := l.r.record(id)
		r.Failures, r.Retries, r.Last, r.SteppedAt = 7, unverifiedAfter, l.now(), l.now()
		l.r.c.evidence.book.put(id, r, true)
		l.set(id, killed())
	}
	l.set("a", udpFails(healthy(40)))
	l.r.settle()
	l.run(35 * time.Minute)
	if !l.moved() {
		t.Fatalf("a's broken UDP never moved the group off it, serving %s", l.r.server("eu"))
	}
	if to, _ := l.moves[0].to.Server(); to.Lineage != "b" && to.Lineage != "c" {
		t.Fatalf("moved to %s, want a working rival", to.Lineage)
	}
	for _, q := range l.rounds {
		if slices.ContainsFunc(q.servers(), func(s domain.ServerRef) bool { return unverified(l.r.record(s.Lineage)) }) {
			t.Fatalf("a round measured an unverified member: %v", q.servers())
		}
	}
}
