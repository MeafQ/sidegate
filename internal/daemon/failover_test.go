package daemon

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// holdPassiveRounds empties the bucket for good, which is the worst noise can do to it.
func (l *timeline) holdPassiveRounds() {
	lane := &l.loop.lanes[failurePath]
	lane.bucket.tokens, lane.bucket.last, lane.bucket.perSecond = 0, l.now(), 0
}

func (l *timeline) suspectAll(heads ...domain.LineageID) {
	l.t.Helper()
	var signals []domain.Signal
	for _, id := range heads {
		signals = append(signals, domain.Signal{Stamp: domain.Stamp{Epoch: "e1", At: l.now()}, Server: l.r.ref(string(id)), Kind: domain.TCPInfo, Verdict: domain.Failed, AttributionCertain: true})
	}
	l.frame(signals)
}

// A slow heartbeat makes real a campaign a passive suspicion started and the bucket holds.
func TestSlownessBehindAHeldPassiveRound(t *testing.T) {
	for _, noisy := range []int{1, 4, 7} {
		t.Run(fmt.Sprintf("noisy=%d", noisy), func(t *testing.T) {
			pings, groups, heads := noisyGroups(noisy)
			l := newTimeline(t, pings)
			l.slots(groups)
			for range beatMinSamples + 3 {
				l.run(heartbeatServing)
			}
			l.moves = nil
			target := heads[len(heads)-1]
			slot := domain.SlotID("s" + target)
			for l.loop.nextBeat[l.r.ref(string(target))].Sub(l.now()) != heartbeatServing {
				l.step()
			}
			l.holdPassiveRounds()
			l.set(target, slowed(20))
			start := l.now()
			l.suspectAll(heads...)
			if c := l.r.memory(slot).Campaign; c == nil || c.Cause != passiveSuspicion {
				t.Fatalf("campaign: %+v", c)
			}
			l.runUntil(failover+30*time.Second, "move", func() bool { _, ok := l.movedOf(slot); return ok })
			m, _ := l.movedOf(slot)
			took := m.at.Sub(start)
			t.Logf("calculated: moved after %s, reason %s (limit %s)", took.Round(10*time.Millisecond), m.reason, failover)
			if took >= failover || m.reason != "slownessMove" {
				t.Errorf("severe slowness replaced after %s, reason %s, limit %s", took, m.reason, failover)
			}
		})
	}
}

// Noise on healthy servers empties the bucket, and then eu's server turns slow with a suspicion of
// its own.
func TestSlownessAfterNoiseEmptiedTheBucket(t *testing.T) {
	pings, groups, heads := noisyGroups(4)
	l := newTimeline(t, pings)
	l.slots(groups)
	l.noise(heads)
	l.run(50 * time.Millisecond)
	current := l.heartbeatJustBeat()
	start := l.now()
	l.set(current.Lineage, slowed(20))
	l.suspectAll(current.Lineage)
	l.runUntil(failover+30*time.Second, "move", func() bool { _, ok := l.movedOf("eu"); return ok })
	m, _ := l.movedOf("eu")
	took := m.at.Sub(start)
	t.Logf("calculated: moved after %s, reason %s (limit %s)", took.Round(10*time.Millisecond), m.reason, failover)
	if took >= failover || m.reason != "slownessMove" {
		t.Errorf("severe slowness replaced after %s, reason %s, limit %s", took, m.reason, failover)
	}
}

// A round held until just before its expiry, and then proven, gets its own time to answer. Otherwise
// the expiry ends it with its round running, and the failover loses a whole heartbeat.
func TestAProvenRoundHeldNearlyToItsExpiryStillAnswers(t *testing.T) {
	pings, groups, heads := noisyGroups(1)
	l := newTimeline(t, pings)
	l.slots(groups)
	l.run(90 * time.Second)
	l.moves = nil
	target := heads[0]
	slot := domain.SlotID("s" + target)
	ref := l.r.ref(string(target))
	l.holdPassiveRounds()
	for l.loop.nextBeat[ref].Sub(l.now()) != heartbeatServing {
		l.step()
	}
	l.run(5 * time.Second)
	l.suspectAll(target)
	c := l.r.memory(slot).Campaign
	if c == nil || c.Cause != passiveSuspicion {
		t.Fatalf("campaign: %+v", c)
	}
	expiry := c.Due.Add(l.r.c.timings.FailureExpiry)
	l.run(expiry.Sub(l.now()) - 7*time.Second)
	l.set(target, silent(0))
	start := l.now()
	l.runUntil(failover, "move", func() bool { _, ok := l.movedOf(slot); return ok })
	m, _ := l.movedOf(slot)
	took := m.at.Sub(start)
	t.Logf("calculated: moved after %s, %s past the old expiry (limit %s)", took.Round(10*time.Millisecond), m.at.Sub(expiry).Round(10*time.Millisecond), failover)
	if !m.at.After(expiry) {
		t.Fatalf("moved before the expiry, so this proves nothing: %s", expiry.Sub(m.at))
	}
	if took >= 2*heartbeatServing {
		t.Errorf("moved after %s, which lost a heartbeat", took)
	}
}

// A heartbeat in flight when a campaign ends carries news nobody has read, whenever it began. It
// fails just after the expiry of a campaign the bucket held, and this failover needs that very
// heartbeat, since the next one is 10 s further from the death.
func TestAHeartbeatInFlightWhenAHeldCampaignExpiresIsRead(t *testing.T) {
	pings, groups, heads := noisyGroups(1)
	l := newTimeline(t, pings)
	l.slots(groups)
	l.run(90 * time.Second)
	l.moves = nil
	target := heads[0]
	slot := domain.SlotID("s" + target)
	ref := l.r.ref(string(target))
	l.holdPassiveRounds()
	for l.loop.nextBeat[ref].Sub(l.now()) != heartbeatServing {
		l.step()
	}
	l.run(2 * time.Second)
	l.suspectAll(target)
	c := l.r.memory(slot).Campaign
	if c == nil || c.Cause != passiveSuspicion {
		t.Fatalf("campaign: %+v", c)
	}
	expiry := c.Due.Add(l.r.c.timings.FailureExpiry)
	l.run(expiry.Sub(l.now()) - 11*time.Second)
	l.set(target, silent(0))
	start := l.now()
	l.runUntil(failover+time.Minute, "move", func() bool { _, ok := l.movedOf(slot); return ok })
	m, _ := l.movedOf(slot)
	took := m.at.Sub(start)
	t.Logf("calculated: moved after %s, reason %s (limit %s)", took.Round(10*time.Millisecond), m.reason, failover)
	if took >= failover {
		t.Errorf("silent server replaced after %s, limit %s", took, failover)
	}
}

// The server a slot just moved to dies while a heartbeat of it is in flight as the move applies.
func TestAHeartbeatInFlightWhenAMoveAppliesIsRead(t *testing.T) {
	l, current := silentOnAList(t, fourServers)
	l.runUntil(failover, "move", l.moved)
	if next := l.r.c.state.slots["eu"].Selection; next == domain.Serving(current) {
		t.Fatalf("still on %s", current.Lineage)
	}
	to, _ := l.r.c.state.slots["eu"].Selection.Server()
	before := l.r.c.evidence.heard
	if got := l.r.memory("eu").ReadThrough; got > before {
		t.Fatalf("read through %d of %d", got, before)
	}
	l.moves = nil
	sig := domain.Signal{Stamp: domain.Stamp{Epoch: "e1", At: l.now().Add(-time.Minute)}, Server: to, Kind: domain.ActiveProbe, Purpose: domain.Heartbeat, Verdict: domain.Failed}
	l.frame([]domain.Signal{sig})
	if c := campaignOf(l); c == nil || c.Cause != failedProbe {
		t.Fatalf("a failure that landed after the move, begun before it, wasn't read: %+v", c)
	}
}

// structuralGroups is the worst shape there is: eight slots, each on a server of its own, with rivals
// nobody else has, so no measurement is shared.
func structuralGroups() (map[string]int, map[domain.SlotID][]domain.LineageID, []string) {
	pings := map[string]int{}
	groups := map[domain.SlotID][]domain.LineageID{}
	var heads []string
	for i := range 8 {
		slot, head := domain.SlotID(fmt.Sprintf("s%d", i)), fmt.Sprintf("h%d", i)
		if i == 0 {
			slot = "eu"
		}
		pings[head] = 20
		heads = append(heads, head)
		groups[slot] = []domain.LineageID{domain.LineageID(head)}
		for j := range failureRivals {
			rival := fmt.Sprintf("r%d%d", i, j)
			pings[rival] = 70 + j
			groups[slot] = append(groups[slot], domain.LineageID(rival))
		}
	}
	return pings, groups, heads
}

// probing counts every probe running now.
func (l *timeline) probing() (rounds, others int) {
	for _, job := range l.loop.lanes[failurePath].running {
		rounds += len(job.servers)
	}
	for _, a := range l.inFlight {
		if a.beat != nil {
			others++
		}
	}
	return rounds, others
}

// structuralRun is what one scenario spent, once the slots have settled.
func structuralRun(t *testing.T, name string, window time.Duration, breakIt func(l *timeline, heads []string), wantMoves bool) (*timeline, time.Time, float64, float64) {
	t.Helper()
	pings, groups, heads := structuralGroups()
	l := newTimeline(t, pings)
	l.slots(groups)
	l.run(90 * time.Second)
	l.moves = nil
	breakIt(l, heads)
	start, attempts, requests := l.now(), l.attempts, l.requests
	peakRounds, peakOthers := 0, 0
	for l.now().Sub(start) < window {
		l.step()
		rounds, others := l.probing()
		peakRounds, peakOthers = max(peakRounds, rounds), max(peakOthers, others)
	}
	minutes := window.Minutes()
	spent, sent := float64(l.attempts-attempts)/minutes, float64(l.requests-requests)/minutes
	t.Logf("calculated: %s: peak %d round probes and %d others at once, %.0f round probes and %.0f requests a minute", name, peakRounds, peakOthers, spent, sent)
	if peakRounds > structuralPeak {
		t.Errorf("%d round probes at once, past the %d structure allows", peakRounds, structuralPeak)
	}
	if limit := 8 + budget.Parallel; peakOthers > limit {
		t.Errorf("%d heartbeats and equal-budget probes at once, past %d", peakOthers, limit)
	}
	if !wantMoves && l.moved() {
		t.Errorf("moved: %+v", l.moves)
	}
	return l, start, spent, sent
}

const (
	structuralPeak      = 8 * (1 + failureRivals)                         // One round per slot.
	structuralSustained = structuralPeak * 60 / 10                        // A new campaign needs a new failed heartbeat, one per server per 10 s at most, under suspicion.
	structuralRequests  = structuralSustained*(1+pingRequests) + 8*6 + 24 // All of them passing, the serving heartbeats, and the equal budget.
	structuralTurns     = structuralSustained * campaignRounds            // Rivals taking turns, the only case with more than a round per heartbeat.
)

// The worst any failure can cost. Proven rounds spend no budget, so this is their only bound.
func TestTheStructuralWorstCaseOfFailureRounds(t *testing.T) {
	t.Run("all eight die together", func(t *testing.T) {
		l, start, _, _ := structuralRun(t, "all eight die", failover, func(l *timeline, heads []string) {
			for _, h := range heads {
				l.set(domain.LineageID(h), silent(0))
			}
		}, true)
		if len(l.moves) != 8 {
			t.Fatalf("%d moves: %+v", len(l.moves), l.moves)
		}
		// A wait for the next heartbeat, its timeout, and two rounds: what one death alone costs, with
		// no failure waiting on another.
		alone := heartbeatServing + probeTimeout + 2*probeTimeout + time.Second
		for _, m := range l.moves {
			if took := m.at.Sub(start); took > alone+time.Second {
				t.Errorf("slot %s moved after %s, and one death alone takes %s", m.slot, took, alone)
			}
		}
	})
	t.Run("an outage with every server serving", func(t *testing.T) {
		_, _, spent, _ := structuralRun(t, "an outage", 10*time.Minute, func(l *timeline, _ []string) {
			for id := range l.servers {
				l.servers[id] = excludedAll()
			}
			l.wan.read(domain.WANDown, l.now(), l.now())
		}, false)
		if spent > structuralSustained {
			t.Errorf("%.0f round probes a minute, past %d", spent, structuralSustained)
		}
	})
	t.Run("heartbeats failing and rounds passing", func(t *testing.T) {
		_, _, spent, sent := structuralRun(t, "flaky heartbeats", 10*time.Minute, func(l *timeline, heads []string) {
			for _, h := range heads {
				good := l.servers[h]
				l.set(domain.LineageID(h), func(purpose domain.ProbePurpose) reply {
					if !asksPing(purpose) {
						return reply{verdict: domain.Failed, took: killedFails}
					}
					return good(purpose)
				})
			}
		}, false)
		if spent > structuralSustained || sent > structuralRequests {
			t.Errorf("%.0f round probes and %.0f requests a minute, past %d and %d", spent, sent, structuralSustained, structuralRequests)
		}
	})
	t.Run("everything dead", func(t *testing.T) {
		structuralRun(t, "everything dead", 10*time.Minute, func(l *timeline, _ []string) {
			for id := range l.servers {
				l.servers[id] = silent(0)
			}
		}, true)
	})
	t.Run("rivals overlapping in a ring", func(t *testing.T) {
		for _, slots := range []int{2, 3, 4, 8} {
			for _, pattern := range []string{"FP", "FFP", "FPP", "FFPP"} {
				for _, took := range []time.Duration{20 * time.Millisecond, killedFails} {
					t.Run(fmt.Sprintf("%d slots, %s, failing in %s", slots, pattern, took), func(t *testing.T) {
						l, heads := ringOfRivals(t, slots)
						for _, h := range heads {
							good, n := healthy(20), 0
							l.set(h, func(purpose domain.ProbePurpose) reply {
								n++
								if pattern[n%len(pattern)] == 'F' {
									return reply{verdict: domain.Failed, took: took}
								}
								return good(purpose)
							})
						}
						start, attempts := l.now(), l.attempts
						const window = 5 * time.Minute
						for l.now().Sub(start) < window {
							l.step()
						}
						spent := float64(l.attempts-attempts) / window.Minutes()
						t.Logf("calculated: %.0f round probes a minute, %.1f moves a minute (limit %d)", spent, float64(len(l.moves))/window.Minutes(), structuralTurns)
						if spent > structuralTurns {
							t.Errorf("%.0f round probes a minute, past %d", spent, structuralTurns)
						}
					})
				}
			}
		}
	})
}

// ringOfRivals puts each slot on a server of its own, with the next slots' servers as its rivals, so
// every round measures other slots' servers.
func ringOfRivals(t *testing.T, slots int) (*timeline, []domain.LineageID) {
	t.Helper()
	pings := map[string]int{}
	var heads []domain.LineageID
	for i := range slots {
		id := fmt.Sprintf("h%d", i)
		pings[id] = 20 + i
		heads = append(heads, domain.LineageID(id))
	}
	l := newTimeline(t, pings)
	groupOf := func(i int) domain.GroupID {
		if i == 0 {
			return "g-eu"
		}
		return domain.GroupID(fmt.Sprintf("g-s%d", i))
	}
	for i := range slots {
		l.r.putPicked(groupOf(i), "ring", heads[i])
		if i == 0 {
			l.r.setExit("eu", domain.GroupExit(groupOf(0)))
		} else {
			intent, err := domain.NewIntent("ring", domain.GroupExit(groupOf(i)))
			if err != nil {
				t.Fatal(err)
			}
			l.r.putSlot(domain.SlotID(fmt.Sprintf("s%d", i)), intent)
		}
		slot := domain.SlotID(fmt.Sprintf("s%d", i))
		if i == 0 {
			slot = "eu"
		}
		l.runUntil(failover, "election", func() bool { return l.r.server(slot) == heads[i] && l.r.operation(slot) == "" })
	}
	for i := range slots {
		var members []domain.LineageID
		for j := range min(slots, 1+failureRivals) {
			members = append(members, heads[(i+j)%slots])
		}
		l.r.putPicked(groupOf(i), "ring", members...)
	}
	l.run(20 * time.Second)
	l.moves = nil
	return l, heads
}

// A heartbeat of the server is what starts rounds. What other slots' rounds and the owner's Test say of
// it counts toward whether it has recovered, and starts nothing.
func TestOnlyAFailedHeartbeatStartsACampaign(t *testing.T) {
	l := newTimeline(t, fourServers)
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	failed := func(purpose domain.ProbePurpose, campaign domain.CampaignID) domain.Signal {
		return domain.Signal{Stamp: domain.Stamp{Epoch: "e1", At: l.now()}, Server: current, Kind: domain.ActiveProbe, Purpose: purpose, Campaign: campaign, Round: 1, Verdict: domain.Failed}
	}
	l.frame([]domain.Signal{failed(domain.Confirmation, "other/1")})
	if c := campaignOf(l); c != nil {
		t.Fatalf("another slot's round started a campaign: %+v", c)
	}
	l.frame([]domain.Signal{failed(domain.ManualProbe, "")})
	if c := campaignOf(l); c != nil {
		t.Fatalf("the owner's Test started a campaign: %+v", c)
	}
	l.frame([]domain.Signal{failed(domain.Heartbeat, "")})
	if c := campaignOf(l); c == nil || c.Cause != failedProbe {
		t.Fatalf("a failed heartbeat started %+v", c)
	}
}

// Rivals that take turns to pass never give one of them two wins in a row, so nothing else would end
// this campaign. Each dead server's rounds stop at campaignRounds, and the next failed heartbeat starts
// the next campaign.
func TestRivalsTakingTurnsCantKeepACampaignRunning(t *testing.T) {
	l := newTimeline(t, fourServers)
	l.run(30 * time.Second)
	current := l.heartbeatJustBeat()
	l.set(current.Lineage, killed())
	var rivals []domain.LineageID
	for _, id := range l.otherServers() {
		if id != current.Lineage {
			rivals = append(rivals, id)
		}
	}
	for i, id := range rivals {
		l.set(id, func(purpose domain.ProbePurpose) reply {
			if purpose != domain.Confirmation || len(l.rounds)%len(rivals) == i {
				return healthy(70)(purpose)
			}
			return reply{verdict: domain.Failed, took: killedFails}
		})
	}
	began := l.r.memory("eu").Campaigns
	most := uint8(0)
	spent := l.attempts
	const window = 5 * time.Minute
	for start := l.now(); l.now().Sub(start) < window; {
		l.step()
		if c := campaignOf(l); c != nil {
			most = max(most, c.Round)
		}
	}
	campaigns := l.r.memory("eu").Campaigns - began
	t.Logf("calculated: %d campaigns in %s, the longest %d rounds, %d round probes", campaigns, window, most, l.attempts-spent)
	if most > campaignRounds {
		t.Errorf("a campaign ran %d rounds, limit %d", most, campaignRounds)
	}
	if most != campaignRounds {
		t.Errorf("the rivals never took turns: the longest campaign ran %d rounds", most)
	}
	if got, limit := float64(l.attempts-spent)/window.Minutes(), float64(structuralTurns)/8; got > limit {
		t.Errorf("%.0f round probes a minute on one slot, past %.0f", got, limit)
	}
}

// disjointOnOne puts n slots on one server a, each with three rivals nobody else has.
func disjointOnOne(n int) (map[string]int, map[domain.SlotID][]domain.LineageID) {
	pings := map[string]int{"a": 20}
	groups := map[domain.SlotID][]domain.LineageID{}
	for i := range n {
		slot := domain.SlotID(fmt.Sprintf("s%d", i))
		if i == 0 {
			slot = "eu"
		}
		members := []domain.LineageID{"a"}
		for j := range failureRivals {
			rival := fmt.Sprintf("r%d%d", i, j)
			pings[rival] = 70 + j
			members = append(members, domain.LineageID(rival))
		}
		groups[slot] = members
	}
	return pings, groups
}

func TestOneDeadServerWithDisjointRivalsIsOneFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		n    int
		dead behaviour
	}{
		{"3 slots silent", 3, silent(0)},
		{"4 slots silent", 4, silent(0)},
		{"8 slots silent", 8, silent(0)},
		{"3 slots killed", 3, killed()},
		{"4 slots killed", 4, killed()},
		{"8 slots killed", 8, killed()},
	} {
		t.Run(c.name, func(t *testing.T) {
			pings, groups := disjointOnOne(c.n)
			l := newTimeline(t, pings)
			l.slots(groups)
			l.run(90 * time.Second)
			l.moves = nil
			current := l.heartbeatJustBeat()
			l.set(current.Lineage, c.dead)
			start := l.now()
			for len(l.moves) < c.n && l.now().Sub(start) < 5*time.Minute {
				l.step()
			}
			worst := time.Duration(0)
			for slot := range groups {
				m, ok := l.movedOf(slot)
				if !ok {
					t.Errorf("slot %s never moved in 5 min", slot)
					continue
				}
				took := m.at.Sub(start)
				worst = max(worst, took)
				t.Logf("calculated: slot %s: %s after %s", slot, m.reason, took.Round(10*time.Millisecond))
			}
			if worst >= failover {
				t.Errorf("one dead server: slowest slot moved after %s, limit %s", worst.Round(10*time.Millisecond), failover)
			}
		})
	}
}

// outage makes every server fail with no proof of the WAN.
func excludedAll() behaviour {
	return func(domain.ProbePurpose) reply {
		return reply{verdict: domain.Unknown, excluded: true, took: probeTimeout}
	}
}

// A flaky WAN: every minute, a 5 s blip that fails whatever heartbeats it catches and blames nobody.
// No server is at fault. What does it leave of the budget, and what does a death then cost?
func TestWANBlipsLeaveARealFailureItsTime(t *testing.T) {
	const slots = 8
	pings, groups, _ := noisyGroups(slots - 1)
	l := newTimeline(t, pings)
	l.slots(groups)
	l.run(90 * time.Second)
	l.moves = nil
	saved := map[string]behaviour{}
	var ids []string
	for id, b := range l.servers {
		saved[id] = b
		ids = append(ids, id)
	}
	slices.Sort(ids)
	blip := func(on bool) {
		for _, id := range ids {
			if on {
				l.servers[id] = excludedAll()
			} else {
				l.servers[id] = saved[id]
			}
		}
	}
	for range 4 {
		blip(true)
		l.run(5 * time.Second)
		blip(false)
		l.run(55 * time.Second)
	}
	blip(true)
	l.run(5 * time.Second)
	blip(false)
	l.run(2 * time.Second)
	if l.moved() {
		t.Fatalf("blips moved a slot: %+v", l.moves)
	}
	l.moves = nil
	current := l.heartbeatJustBeat()
	l.set(current.Lineage, silent(0))
	saved[string(current.Lineage)] = silent(0)
	took := l.runUntil(3*time.Minute, "move", func() bool { _, ok := l.movedOf("eu"); return ok })
	t.Logf("calculated: moved after %s (limit %s)", took.Round(10*time.Millisecond), failover)
	if took >= failover {
		t.Errorf("silent server replaced after %s, limit %s", took.Round(10*time.Millisecond), failover)
	}
}

// One WAN outage with the slots serving, ending just before one heartbeat's period. Nothing is blamed,
// and then a server dies 20 s after the WAN is back.
func TestADeathAfterAnOutageTakesWhatAnyDeathTakes(t *testing.T) {
	for _, after := range []time.Duration{0, 10 * time.Second, 20 * time.Second, 30 * time.Second, 45 * time.Second} {
		t.Run(fmt.Sprintf("death %s after the outage", after), func(t *testing.T) {
			const slots = 8
			pings, groups, _ := noisyGroups(slots - 1)
			l := newTimeline(t, pings)
			l.slots(groups)
			l.run(90 * time.Second)
			l.moves = nil
			saved := map[string]behaviour{}
			for id, b := range l.servers {
				saved[id] = b
				l.servers[id] = excludedAll()
			}
			// The WAN reads down only after the second failed baseline.
			l.run(8 * time.Second)
			l.wan.read(domain.WANDown, l.now(), l.now())
			l.frame(nil)
			l.run(22 * time.Second)
			for id, b := range saved {
				l.servers[id] = b
			}
			l.wan.read(domain.WANUp, l.now(), l.now())
			l.frame(nil)
			l.run(after)
			if l.moved() {
				t.Fatalf("the outage moved a slot: %+v", l.moves)
			}
			current := l.heartbeatJustBeat()
			l.set(current.Lineage, silent(0))
			took := l.runUntil(3*time.Minute, "move", func() bool { _, ok := l.movedOf("eu"); return ok })
			t.Logf("calculated: moved after %s (limit %s)", took.Round(10*time.Millisecond), failover)
			if took >= failover {
				t.Errorf("silent server replaced after %s, limit %s", took.Round(10*time.Millisecond), failover)
			}
		})
	}
}

// A rival rotates in the second between two rounds.
func TestEveryRivalRotatingBetweenRoundsCostsNothing(t *testing.T) {
	l, current := silentOnAList(t, fourServers)
	start := l.now()
	l.runUntil(failover, "round 2 queued", func() bool {
		c := campaignOf(l)
		return c != nil && c.Path == failurePath && c.Round == 2 && l.now().Before(c.Due)
	})
	c := campaignOf(l)
	for _, rival := range c.Rivals {
		l.rotate(rival.Lineage)
	}
	l.runUntil(3*time.Minute, "move", l.moved)
	t.Logf("calculated: %s left after %s, reason %s (limit %s)", current.Lineage, l.now().Sub(start).Round(10*time.Millisecond), l.moves[0].reason, failover)
	if l.now().Sub(start) >= failover {
		t.Errorf("moved after %s, limit %s", l.now().Sub(start).Round(10*time.Millisecond), failover)
	}
}

// Serving heartbeats beside a closed slot sweeping many servers, dead or alive.
func TestASweepDelaysNoServingHeartbeat(t *testing.T) {
	slowTest(t)
	for _, c := range []struct {
		name    string
		serving int
		kept    int
		dead    behaviour
	}{
		{"1 serving, 60 silent", 1, 60, silent(0)},
		{"7 serving, 40 silent", 7, 40, silent(0)},
		{"7 serving, 40 killed", 7, 40, killed()},
	} {
		t.Run(c.name, func(t *testing.T) {
			pings := map[string]int{}
			groups := map[domain.SlotID][]domain.LineageID{}
			var members []domain.LineageID
			for i := range c.kept {
				id := fmt.Sprintf("s%02d", i)
				pings[id] = 60 + i
				members = append(members, domain.LineageID(id))
			}
			groups["eu"] = members
			var heads []domain.LineageID
			for i := range c.serving {
				id := fmt.Sprintf("x%d", i)
				pings[id] = 20
				groups[domain.SlotID("z"+id)] = []domain.LineageID{domain.LineageID(id)}
				heads = append(heads, domain.LineageID(id))
			}
			l := newTimeline(t, pings)
			l.slots(groups)
			for _, id := range members {
				l.set(id, c.dead)
			}
			l.runUntil(10*time.Minute, "closure", l.closed)
			l.run(2 * time.Minute)
			// Worst gap between one serving server's heartbeats while the sweep runs.
			last := map[domain.LineageID]time.Time{}
			worst := time.Duration(0)
			for start := l.now(); l.now().Sub(start) < 5*time.Minute; {
				before := map[domain.LineageID]time.Time{}
				for _, id := range heads {
					before[id] = l.loop.nextBeat[l.r.ref(string(id))]
				}
				l.step()
				for _, id := range heads {
					if after := l.loop.nextBeat[l.r.ref(string(id))]; !after.Equal(before[id]) {
						if !last[id].IsZero() {
							worst = max(worst, l.now().Sub(last[id]))
						}
						last[id] = l.now()
					}
				}
			}
			t.Logf("calculated: worst gap between serving heartbeats beside the sweep: %s, nominal %s", worst.Round(50*time.Millisecond), heartbeatServing)
			// A serving server dies just after a beat, while the sweep runs.
			for l.loop.nextBeat[l.r.ref(string(heads[0]))].Sub(l.now()) != heartbeatServing {
				l.step()
			}
			l.set(heads[0], silent(0))
			start := l.now()
			l.runUntil(heartbeatServing+probeTimeout+10*time.Second, "failed heartbeat", func() bool {
				r := l.r.c.evidence.results(l.r.ref(string(heads[0])))
				return len(r) > 0 && r[len(r)-1].Verdict == domain.Failed
			})
			t.Logf("calculated: the serving server's failure landed %s after its death, nominal %s", l.now().Sub(start).Round(50*time.Millisecond), heartbeatServing+probeTimeout)
			if worst > heartbeatServing+timelineStep {
				t.Errorf("a sweep delayed a serving heartbeat to a %s gap", worst)
			}
		})
	}
}

// A restart during a pending move off a forbidden server renews nothing.
func TestARestartRenewsNoDeadline(t *testing.T) {
	l := newTimeline(t, fourServers)
	current, _ := l.r.c.state.slots["eu"].Selection.Server()
	var others []domain.LineageID
	for _, id := range l.otherServers() {
		if id != current.Lineage {
			others = append(others, id)
		}
	}
	l.exitTo(others...)
	l.r.settle()
	if len(l.r.c.state.pending) == 0 {
		t.Fatalf("no pending move: %+v", l.r.memory("eu"))
	}
	before := l.r.targetOf("eu")
	_, until, ok := before.Passage.Allows(before.Selection)
	if !ok {
		t.Fatalf("no passage: %+v", before)
	}
	l.r.clock = l.now().Add(10 * time.Second)
	l.restart()
	l.r.settle()
	after := l.r.targetOf("eu")
	if _, got, ok := after.Passage.Allows(after.Selection); !ok || !got.Equal(until) {
		t.Errorf("deadline changed: %v to %+v", until, after.Passage)
	}
}
