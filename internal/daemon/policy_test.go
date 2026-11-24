package daemon

import (
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func lin(id string) domain.Lineage {
	return domain.Lineage{ID: domain.LineageID(id), Presence: domain.Present,
		Ref: domain.ServerRef{Lineage: domain.LineageID(id), Digest: domain.Digest{1}}}
}

func missing(id string) domain.Lineage {
	l := lin(id)
	l.Presence = domain.Missing
	return l
}

func ref(id string) domain.ServerRef { return lin(id).Ref }

func groupSlot(*testing.T) domain.Owner { return domain.GroupOwner("g") }

func serverSlot(*testing.T, domain.LineageID) domain.Owner {
	return domain.SlotOwner("work")
}

// poolOf is what a group with these rows allows.
func poolOf(lineages ...domain.Lineage) pool {
	p := pool{empty: domain.NoUsableServer}
	for _, l := range lineages {
		if l.Presence == domain.Present {
			p.members = append(p.members, l)
		}
	}
	return p
}

// directOf is what a server Exit on this row allows.
func directOf(l domain.Lineage) pool {
	p := poolOf(l)
	p.fixed, p.empty = true, domain.ServerMissing
	return p
}

// aware lets a pool see what a slot leaving a server needs to read, as a real world's pools do.
func aware(p pool, lineages ...domain.Lineage) pool {
	p.catalogue = domain.NewCatalogue(1, lineages)
	p.refused = map[domain.ServerRef]bool{}
	return p
}

func serving(id string) domain.Selection { return domain.Serving(ref(id)) }

type scene struct{ *evidence }

func newScene(wan domain.WANState) *scene {
	w := &scene{newEvidence("t1", nil)}
	w.wan, w.epoch = wanBelief{State: wan, At: t0}, "e1"
	return w
}

func (w *scene) probe(id string, age time.Duration, verdict domain.Verdict, ms int) *scene {
	s := domain.Signal{Stamp: domain.Stamp{At: t0.Add(-age)}, Server: ref(id), Kind: domain.ActiveProbe, Verdict: verdict, Purpose: domain.Heartbeat}
	if ms > 0 {
		d := time.Duration(ms) * time.Millisecond
		s.NewConn = &d
	}
	s = w.hear(s)
	w.server(ref(id)).active.push(s)
	w.book.hear([]domain.Signal{s}, w.wan.LastDown)
	return w
}

// hear is what ingest does for a signal that arrives now.
func (w *scene) hear(s domain.Signal) domain.Signal {
	w.heard++
	s.Heard = w.heard
	return s
}

func (w *scene) passive(id string, age time.Duration) *scene {
	w.server(ref(id)).suspicion = w.hear(domain.Signal{Stamp: domain.Stamp{At: t0.Add(-age)}, Server: ref(id), Kind: domain.TCPInfo, Verdict: domain.Failed})
	return w
}

func (w *scene) round(c *campaign, id string, verdict domain.Verdict, ms int) *scene {
	return w.roundOn(c, "", id, verdict, ms)
}

func (w *scene) roundOn(c *campaign, target domain.TargetID, id string, verdict domain.Verdict, ms int) *scene {
	s := domain.Signal{Stamp: domain.Stamp{At: t0}, Server: ref(id), Kind: domain.ActiveProbe, Verdict: verdict,
		Purpose: domain.Confirmation, Campaign: c.ID, Round: c.Round, Target: target}
	if ms > 0 {
		d := time.Duration(ms) * time.Millisecond
		s.Ping = &d
	}
	s = w.hear(s)
	w.server(ref(id)).active.push(s)
	w.server(ref(id)).rounds.push(s)
	w.book.hear([]domain.Signal{s}, w.wan.LastDown)
	return w
}

// healthy gives id three fresh successes, each with a new connection of ms.
func (w *scene) healthy(id string, ms int) *scene {
	return w.probe(id, 30*time.Second, domain.Succeeded, ms).probe(id, 20*time.Second, domain.Succeeded, ms).probe(id, 10*time.Second, domain.Succeeded, ms)
}

func wantSwitch(t *testing.T, d decision, reason domain.Reason, server string) {
	t.Helper()
	if d.Switch == nil {
		t.Fatalf("no switch, want reason %d to %s", reason, server)
	}
	if d.Switch.Reason != reason {
		t.Fatalf("reason %d, want %d", d.Switch.Reason, reason)
	}
	got, ok := d.Switch.Selection.Server()
	if !ok || got.Lineage != domain.LineageID(server) {
		t.Fatalf("selection %+v, want %s", d.Switch.Selection, server)
	}
}

func wantClosed(t *testing.T, d decision, down domain.DownReason, reason domain.Reason) {
	t.Helper()
	if d.Switch == nil {
		t.Fatalf("no switch, want closed %d", down)
	}
	if got, isDown := d.Switch.Selection.Down(); !isDown || got != down {
		t.Fatalf("selection down %v %d, want %d", isDown, got, down)
	}
	if d.Switch.Reason != reason {
		t.Fatalf("reason %d, want %d", d.Switch.Reason, reason)
	}
}

func wantQuiet(t *testing.T, d decision) {
	t.Helper()
	if d.Switch != nil {
		t.Fatalf("unexpected switch: reason %d to %+v", d.Switch.Reason, d.Switch.Selection)
	}
	if c := d.Memory.Campaign; c != nil {
		t.Fatalf("unexpected campaign: %+v", c)
	}
}

func TestInitialElection(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	candidates := poolOf(lin("a"), lin("b"))

	t.Run("waits for the WAN", func(t *testing.T) {
		w := newScene(domain.WANUnknown).probe("a", time.Second, domain.Succeeded, 40).probe("b", time.Second, domain.Succeeded, 20)
		wantQuiet(t, decide(t0, slot, candidates, policyMemory{}, w.evidence, tm))
	})
	t.Run("waits for the other candidates' first result", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("a", time.Second, domain.Succeeded, 40)
		wantQuiet(t, decide(t0, slot, candidates, policyMemory{}, w.evidence, tm))
	})
	t.Run("stops waiting after SettleFor", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("a", 9*time.Second, domain.Succeeded, 40)
		wantSwitch(t, decide(t0, slot, candidates, policyMemory{}, w.evidence, tm), domain.InitialSelection, "a")
	})
	t.Run("picks the first of those that answered, once they all have, whatever their pings", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 40).healthy("b", 20)
		wantSwitch(t, decide(t0, slot, candidates, policyMemory{}, w.evidence, tm), domain.InitialSelection, "a")
	})
	t.Run("a failed candidate is not elected", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 40).probe("b", time.Second, domain.Failed, 0)
		wantSwitch(t, decide(t0, slot, candidates, policyMemory{}, w.evidence, tm), domain.InitialSelection, "a")
	})
	t.Run("nothing present closes the slot", func(t *testing.T) {
		w := newScene(domain.WANUp)
		wantClosed(t, decide(t0, slot, poolOf(missing("a"), missing("b")), policyMemory{}, w.evidence, tm),
			domain.NoUsableServer, domain.InitialSelection)
	})
	t.Run("takes the first ready server", func(t *testing.T) {
		wide := groupSlot(t)
		w := newScene(domain.WANUp).probe("a", time.Second, domain.Failed, 0).healthy("b", 40).healthy("c", 20)
		wantSwitch(t, decide(t0, wide, poolOf(lin("a"), lin("b"), lin("c")), policyMemory{}, w.evidence, tm),
			domain.InitialSelection, "b")
	})
}

// runCampaign plays rounds through decide, each with its results from play, until a
// decision that isn't another round.
func runCampaign(t *testing.T, slot domain.Owner, candidates pool, memory policyMemory, w *scene, tm timings,
	play func(c *campaign, w *scene)) decision {
	t.Helper()
	d := decide(t0, slot, candidates, memory, w.evidence, tm)
	for range 10 {
		if d.Switch != nil || d.Memory.Campaign == nil {
			return d
		}
		play(d.Memory.Campaign, w)
		d = decide(t0, slot, candidates, d.Memory, w.evidence, tm)
	}
	t.Fatal("campaign never ended")
	return d
}

func TestFailover(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	candidates := poolOf(lin("a"), lin("b"), lin("c"))
	memory := policyMemory{Selection: serving("a")}

	t.Run("dead incumbent, live alternative switches after two rounds", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20).healthy("c", 30).probe("a", time.Second, domain.Failed, 0)
		rounds := 0
		d := runCampaign(t, slot, candidates, memory, w, tm, func(c *campaign, w *scene) {
			rounds++
			w.round(c, "a", domain.Failed, 0)
			w.round(c, "b", domain.Succeeded, 20)
			w.round(c, "c", domain.Succeeded, 30)
		})
		wantSwitch(t, d, domain.FailureMove, "b")
		if rounds != 2 {
			t.Fatalf("%d rounds", rounds)
		}
		if len(d.Switch.Evidence) == 0 {
			t.Fatal("the switch carries no evidence")
		}
	})
	t.Run("one bad round and a good one switches nothing", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20).healthy("c", 30).probe("a", time.Second, domain.Failed, 0)
		rounds := 0
		d := runCampaign(t, slot, candidates, memory, w, tm, func(c *campaign, w *scene) {
			rounds++
			verdict := domain.Failed
			if rounds > 1 {
				verdict = domain.Succeeded
			}
			w.round(c, "a", verdict, 25)
			w.round(c, "b", domain.Succeeded, 20)
			w.round(c, "c", domain.Succeeded, 30)
		})
		wantQuiet(t, d)
		if d.Memory.Campaign != nil || !d.Memory.QuietUntil.After(t0) {
			t.Fatalf("memory after a recovered campaign: %+v", d.Memory)
		}
	})
	t.Run("everything failing closes rather than leaks", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("a", time.Second, domain.Failed, 0)
		d := runCampaign(t, slot, candidates, memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", domain.Failed, 0)
			for _, alt := range c.Rivals {
				w.round(c, string(alt.Lineage), domain.Failed, 0)
			}
		})
		wantClosed(t, d, domain.ConfirmedDead, domain.FailureMove)
	})
	t.Run("an unknown round blames nobody", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20).healthy("c", 30).probe("a", time.Second, domain.Failed, 0)
		d := runCampaign(t, slot, candidates, memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", domain.Unknown, 0)
			w.round(c, "b", domain.Unknown, 0)
			w.round(c, "c", domain.Unknown, 0)
		})
		wantQuiet(t, d)
	})
	t.Run("a campaign whose round never reports is abandoned", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20).healthy("c", 30).probe("a", time.Second, domain.Failed, 0)
		d := decide(t0, slot, candidates, memory, w.evidence, tm)
		if d.Memory.Campaign == nil {
			t.Fatal("no campaign")
		}
		w.wan.At = t0.Add(time.Minute)
		d = decide(t0.Add(time.Minute), slot, candidates, d.Memory, w.evidence, tm)
		wantQuiet(t, d)
		if d.Memory.Campaign != nil {
			t.Fatal("campaign kept")
		}
	})
	t.Run("a held server is the last resort when the incumbent dies", func(t *testing.T) {
		m := memory
		m.KeepOut = map[domain.LineageID]time.Time{"b": t0.Add(time.Hour), "c": t0.Add(time.Hour)}
		w := newScene(domain.WANUp).healthy("b", 20).healthy("c", 30).probe("a", time.Second, domain.Failed, 0)
		d := runCampaign(t, slot, candidates, m, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", domain.Failed, 0)
			w.round(c, "b", domain.Succeeded, 20)
			w.round(c, "c", domain.Succeeded, 30)
		})
		wantSwitch(t, d, domain.FailureMove, "b")
	})
	t.Run("rounds rotate past alternatives that fail", func(t *testing.T) {
		wide := groupSlot(t)
		all := poolOf(lin("a"), lin("b"), lin("c"), lin("d"), lin("e"))
		w := newScene(domain.WANUp).probe("a", time.Second, domain.Failed, 0)
		d := runCampaign(t, wide, all, memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", domain.Failed, 0)
			for _, alt := range c.Rivals {
				verdict := domain.Failed
				if alt.Lineage == "e" {
					verdict = domain.Succeeded
				}
				w.round(c, string(alt.Lineage), verdict, 30)
			}
		})
		wantSwitch(t, d, domain.FailureMove, "e")
	})
}

// The incumbent is dead on every target, and the rounds rotate over three as the signal loop does. A
// server fails where answers says so and passes elsewhere.
func TestADeadServerBesideACensoredTarget(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	candidates := poolOf(lin("a"), lin("b"), lin("c"), lin("d"))
	memory := policyMemory{Selection: serving("a")}
	targets := []domain.TargetID{"t1", "t2", "t3"}
	play := func(answers func(round uint8, target domain.TargetID) domain.Verdict) func(*campaign, *scene) {
		return func(c *campaign, w *scene) {
			target := targets[int(c.Round)%len(targets)]
			w.roundOn(c, target, "a", domain.Failed, 0)
			for _, rival := range c.Rivals {
				w.roundOn(c, target, string(rival.Lineage), answers(c.Round, target), 20)
			}
		}
	}
	dead := func() *scene { return newScene(domain.WANUp).probe("a", time.Second, domain.Failed, 0) }

	for _, censored := range targets[1:] {
		t.Run("a rival takes over with "+string(censored)+" censored", func(t *testing.T) {
			d := runCampaign(t, slot, candidates, memory, dead(), tm, play(func(_ uint8, target domain.TargetID) domain.Verdict {
				if target == censored {
					return domain.Failed
				}
				return domain.Succeeded
			}))
			wantSwitch(t, d, domain.FailureMove, "b")
		})
	}
	t.Run("a group that is silent on every target closes in two rounds", func(t *testing.T) {
		rounds := 0
		silent := play(func(uint8, domain.TargetID) domain.Verdict { return domain.Failed })
		d := runCampaign(t, slot, candidates, memory, dead(), tm, func(c *campaign, w *scene) {
			rounds++
			if len(c.Rivals) == 0 {
				t.Fatal("a round asked no rival")
			}
			silent(c, w)
		})
		wantClosed(t, d, domain.ConfirmedDead, domain.FailureMove)
		if rounds != failureRounds {
			t.Fatalf("closed after %d rounds", rounds)
		}
	})
	t.Run("a group that falls silent after one pass closes when the rounds return to that target", func(t *testing.T) {
		d := runCampaign(t, slot, candidates, memory, dead(), tm, play(func(round uint8, _ domain.TargetID) domain.Verdict {
			if round == 1 {
				return domain.Succeeded
			}
			return domain.Failed
		}))
		wantClosed(t, d, domain.ConfirmedDead, domain.FailureMove)
	})
}

func TestPassiveSuspicionOnlyStartsAConfirmation(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	candidates := poolOf(lin("a"), lin("b"))
	memory := policyMemory{Selection: serving("a")}
	w := newScene(domain.WANUp).healthy("a", 20).healthy("b", 20).passive("a", time.Second)
	d := decide(t0, slot, candidates, memory, w.evidence, tm)
	if d.Switch != nil || d.Memory.Campaign == nil || d.Memory.Campaign.Incumbent != ref("a") {
		t.Fatalf("decision: %+v", d)
	}
	d2 := runCampaign(t, slot, candidates, memory, w, tm, func(c *campaign, w *scene) {
		w.round(c, "a", domain.Succeeded, 20)
		w.round(c, "b", domain.Succeeded, 20)
	})
	wantQuiet(t, d2)
	quiet := d2.Memory
	w.passive("a", -time.Second)
	wantQuiet(t, decide(t0, slot, candidates, quiet, w.evidence, tm))
}

// improvingOn is a's memory with a comparison round against rival.
func improvingOn(rival string) policyMemory {
	return policyMemory{Selection: serving("a"), Campaigns: 1, Campaign: &campaign{ID: "g/1", Path: improvePath, Epoch: "e1",
		Incumbent: ref("a"), Rivals: []domain.ServerRef{ref(rival)}, Wins: []tally{{}}, Due: t0, Round: 1, Target: "t1"}}
}

func TestJudgeRound(t *testing.T) {
	failure := campaign{Path: failurePath}
	group, server := true, false
	sig := func(v domain.Verdict, ping, newConn *time.Duration) domain.Signal {
		return domain.Signal{Verdict: v, Ping: ping, NewConn: newConn}
	}
	spreadOf := func(s domain.Signal, spread int) domain.Signal {
		s.PingSpread = ms(spread)
		return s
	}
	up, down, unknown := domain.Succeeded, domain.Failed, domain.Unknown
	fast := sig(up, ms(67), ms(300))
	cases := []struct {
		name      string
		c         campaign
		severe    bool
		incumbent domain.Signal
		rivals    []domain.Signal
		want      roundResult
	}{
		{"an unknown incumbent blames no one", failure, group, sig(unknown, nil, nil), []domain.Signal{fast}, roundResult{Inconclusive: true}},
		{"a failed incumbent loses to a rival that passed", failure, group, sig(down, nil, nil), []domain.Signal{fast}, roundResult{Failed: true, Beat: []bool{true}}},
		{"a rival with no Ping still wins against a failure", failure, group, sig(down, nil, nil), []domain.Signal{sig(up, nil, nil)}, roundResult{Failed: true, Beat: []bool{true}}},
		{"everything failing is a failure with no winner", failure, group, sig(down, nil, nil), []domain.Signal{sig(down, nil, nil)}, roundResult{Failed: true, Beat: []bool{false}}},
		{"a failed incumbent alone is a failure", failure, server, sig(down, nil, nil), nil, roundResult{Failed: true}},
		{"failed, with only unknown rivals, blames no one", failure, group, sig(down, nil, nil), []domain.Signal{sig(unknown, nil, nil), sig(down, nil, nil)}, roundResult{Inconclusive: true}},
		{"failed, with one rival passing, doesn't wait for an unknown one", failure, group, sig(down, nil, nil), []domain.Signal{sig(unknown, nil, nil), fast}, roundResult{Failed: true, Beat: []bool{false, true}}},

		{"300 ms added crosses the severe line", failure, group, sig(up, ms(367), ms(1530)), []domain.Signal{fast}, roundResult{Slow: true, Beat: []bool{true}}},
		{"USA stays against a 67 ms rival, 151 ms apart", failure, group, sig(up, ms(218), ms(894)), []domain.Signal{sig(up, ms(67), ms(333))}, roundResult{Beat: []bool{false}}},
		{"USA stays with its requests steady", failure, group, spreadOf(sig(up, ms(218), ms(894)), 5), []domain.Signal{spreadOf(sig(up, ms(67), ms(333)), 3)}, roundResult{Beat: []bool{false}}},
		{"a lost packet shows as spread under a median Ping that looks fine", failure, group, spreadOf(sig(up, ms(104), ms(15)), 207), []domain.Signal{spreadOf(sig(up, ms(1), ms(15)), 1)}, roundResult{Slow: true, Beat: []bool{true}}},
		{"spread the rival shares says nothing", failure, group, spreadOf(sig(up, ms(104), ms(15)), 207), []domain.Signal{spreadOf(sig(up, ms(104), ms(15)), 207)}, roundResult{Beat: []bool{false}}},
		{"Ping must clear the rival's whole Ping too", failure, group, sig(up, ms(700), ms(700)), []domain.Signal{sig(up, ms(400), ms(600))}, roundResult{Beat: []bool{false}}},
		{"loss: a normal Ping over a slow new connection", failure, group, sig(up, ms(70), ms(2000)), []domain.Signal{fast}, roundResult{Slow: true, Beat: []bool{true}}},
		{"new connection needs its ratio", failure, group, sig(up, ms(70), ms(2000)), []domain.Signal{sig(up, ms(67), ms(900))}, roundResult{Beat: []bool{false}}},
		{"new connection needs its floor", failure, group, sig(up, ms(70), ms(900)), []domain.Signal{sig(up, ms(67), ms(100))}, roundResult{Beat: []bool{false}}},
		{"no Ping, but the new-connection arm still reads", failure, group, sig(up, nil, ms(2000)), []domain.Signal{sig(up, nil, ms(300))}, roundResult{Slow: true, Beat: []bool{true}}},
		{"nothing to compare is inconclusive", failure, group, sig(up, nil, nil), []domain.Signal{sig(up, nil, nil)}, roundResult{Inconclusive: true, Beat: []bool{false}}},
		{"a passing incumbent over failed rivals is simply alive", failure, group, sig(up, ms(70), ms(300)), []domain.Signal{sig(down, nil, nil)}, roundResult{Beat: []bool{false}}},
		{"a passing incumbent alone is simply alive", failure, group, sig(up, ms(70), ms(300)), nil, roundResult{}},
		{"a server Exit counts only failures: slow stays", failure, server, sig(up, ms(900), ms(5000)), []domain.Signal{fast}, roundResult{Beat: []bool{false}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := judgeRound(c.c, c.incumbent, c.rivals, c.severe)
			if got.Inconclusive != c.want.Inconclusive || got.Failed != c.want.Failed || got.Slow != c.want.Slow || got.Lost != c.want.Lost {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			if !c.want.Inconclusive && len(c.rivals) > 0 && !slices.Equal(got.Beat, c.want.Beat) {
				t.Fatalf("beat %v, want %v", got.Beat, c.want.Beat)
			}
		})
	}
}

func TestSlowHeartbeatStartsRoundsOnlyInStablePing(t *testing.T) {
	tm := defaultTimings()
	everyone := poolOf(lin("a"), lin("b"))
	memory := policyMemory{Selection: serving("a")}
	beats := func(w *scene, last int) *scene {
		for i := 4; i > 0; i-- {
			w.probe("a", time.Duration(i+1)*10*time.Second, domain.Succeeded, 300)
		}
		return w.probe("a", time.Second, domain.Succeeded, last)
	}
	t.Run("a heartbeat past both lines starts failure rounds", func(t *testing.T) {
		w := beats(newScene(domain.WANUp).healthy("b", 20), 1530)
		d := decide(t0, groupSlot(t), everyone, memory, w.evidence, tm)
		if c := d.Memory.Campaign; c == nil || c.Path != failurePath || c.Cause != slowHeartbeat {
			t.Fatalf("campaign: %+v", c)
		}
	})
	t.Run("a retransmit's worth over the floor starts rounds", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20)
		for i := 4; i > 0; i-- {
			w.probe("a", time.Duration(i+1)*10*time.Second, domain.Succeeded, 10)
		}
		w.probe("a", time.Second, domain.Succeeded, 215)
		d := decide(t0, groupSlot(t), everyone, memory, w.evidence, tm)
		if c := d.Memory.Campaign; c == nil || c.Cause != slowHeartbeat {
			t.Fatalf("campaign: %+v", c)
		}
	})
	for name, last := range map[string]int{"under the ratio": 590, "under the floor": 290} {
		t.Run(name, func(t *testing.T) {
			w := newScene(domain.WANUp).healthy("b", 20)
			base := 300
			if name == "under the floor" {
				base = 100
			}
			for i := 4; i > 0; i-- {
				w.probe("a", time.Duration(i+1)*10*time.Second, domain.Succeeded, base)
			}
			w.probe("a", time.Second, domain.Succeeded, last)
			wantQuiet(t, decide(t0, groupSlot(t), everyone, memory, w.evidence, tm))
		})
	}
	t.Run("a heartbeat with too little history starts nothing", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20).probe("a", 2*time.Second, domain.Succeeded, 10).probe("a", time.Second, domain.Succeeded, 1530)
		wantQuiet(t, decide(t0, groupSlot(t), everyone, memory, w.evidence, tm))
	})
	t.Run("its own quiet period holds it back", func(t *testing.T) {
		m := memory
		m.SlowQuietUntil = t0.Add(time.Second)
		w := beats(newScene(domain.WANUp).healthy("b", 20), 1530)
		wantQuiet(t, decide(t0, groupSlot(t), everyone, m, w.evidence, tm))
	})
	t.Run("a passive suspicion's quiet period doesn't", func(t *testing.T) {
		m := memory
		m.QuietUntil = t0.Add(time.Second)
		w := beats(newScene(domain.WANUp).healthy("b", 20), 1530)
		if c := decide(t0, groupSlot(t), everyone, m, w.evidence, tm).Memory.Campaign; c == nil || c.Cause != slowHeartbeat {
			t.Fatalf("campaign: %+v", c)
		}
	})
	t.Run("a heartbeat already read starts nothing", func(t *testing.T) {
		m := memory
		w := beats(newScene(domain.WANUp).healthy("b", 20), 1530)
		m.ReadThrough = w.heard
		wantQuiet(t, decide(t0, groupSlot(t), everyone, m, w.evidence, tm))
	})
	t.Run("a server Exit ignores it", func(t *testing.T) {
		w := beats(newScene(domain.WANUp), 1530)
		wantQuiet(t, decide(t0, serverSlot(t, "a"), directOf(lin("a")), memory, w.evidence, tm))
	})
	t.Run("with no rival to beat it nothing moves", func(t *testing.T) {
		w := beats(newScene(domain.WANUp).healthy("b", 20), 1530)
		d := runCampaign(t, groupSlot(t), everyone, memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", domain.Succeeded, 700)
			w.round(c, "b", domain.Succeeded, 500)
		})
		wantQuiet(t, d)
		if d.Memory.QuietUntil.IsZero() || d.Memory.SlowQuietUntil.IsZero() {
			t.Fatal("no quiet period after a failure campaign that moved nothing")
		}
	})
}

func TestAFailedRoundIsNeverLabelledLatency(t *testing.T) {
	tm := defaultTimings()
	everyone := poolOf(lin("a"), lin("b"))
	memory := policyMemory{Selection: serving("a")}
	plays := map[string]struct {
		rounds []domain.Verdict
		reason domain.Reason
	}{
		"both slow":        {[]domain.Verdict{domain.Succeeded, domain.Succeeded}, domain.SlownessMove},
		"failed then slow": {[]domain.Verdict{domain.Failed, domain.Succeeded}, domain.FailureMove},
		"slow then failed": {[]domain.Verdict{domain.Succeeded, domain.Failed}, domain.FailureMove},
		"both failed":      {[]domain.Verdict{domain.Failed, domain.Failed}, domain.FailureMove},
	}
	for name, play := range plays {
		t.Run(name, func(t *testing.T) {
			w := newScene(domain.WANUp).healthy("b", 20).healthy("a", 400).passive("a", time.Second)
			round := 0
			d := runCampaign(t, groupSlot(t), everyone, memory, w, tm, func(c *campaign, w *scene) {
				w.round(c, "a", play.rounds[round], 400)
				w.round(c, "b", domain.Succeeded, 20)
				round++
			})
			wantSwitch(t, d, play.reason, "b")
		})
	}
}

func TestAFailureReplacesAnImprovementOnTheSameEvaluation(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	everyone := poolOf(lin("a"), lin("b"), lin("c"))
	start := func(*scene) decision { return decision{Memory: improvingOn("b")} }
	t.Run("a failed heartbeat between rounds", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 150).healthy("b", 20).healthy("c", 30)
		d := start(w)
		w.probe("a", -time.Second, domain.Failed, 0)
		next := decide(t0.Add(time.Second), slot, everyone, d.Memory, w.evidence, tm)
		c := next.Memory.Campaign
		if c == nil || c.Path != failurePath || c.Cause != failedProbe || c.ID == d.Memory.Campaign.ID || len(c.Rivals) != 2 {
			t.Fatalf("campaign: %+v", c)
		}
		if !c.Due.Equal(t0.Add(time.Second)) {
			t.Fatalf("a failure round should be due now: %v", c.Due)
		}
	})
	t.Run("the incumbent failing the improvement round itself", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 150).healthy("b", 20).healthy("c", 30)
		d := start(w)
		w.round(d.Memory.Campaign, "a", domain.Failed, 0).round(d.Memory.Campaign, "b", domain.Succeeded, 20)
		next := decide(t0, slot, everyone, d.Memory, w.evidence, tm)
		if c := next.Memory.Campaign; c == nil || c.Path != failurePath {
			t.Fatalf("campaign: %+v", c)
		}
	})
	t.Run("the incumbent failing the round, with a later heartbeat passing", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 150).healthy("b", 20).healthy("c", 30)
		d := start(w)
		w.round(d.Memory.Campaign, "a", domain.Failed, 0).round(d.Memory.Campaign, "b", domain.Succeeded, 20)
		w.probe("a", -time.Second, domain.Succeeded, 150)
		next := decide(t0.Add(time.Second), slot, everyone, d.Memory, w.evidence, tm)
		c := next.Memory.Campaign
		if c == nil || c.Path != failurePath || c.Cause != failedProbe || len(c.Evidence) != 1 || c.Evidence[0].Verdict != domain.Failed {
			t.Fatalf("campaign: %+v", c)
		}
	})
	t.Run("a passive suspicion", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 150).healthy("b", 20).healthy("c", 30)
		d := start(w)
		w.passive("a", -time.Second)
		if c := decide(t0.Add(time.Second), slot, everyone, d.Memory, w.evidence, tm).Memory.Campaign; c == nil || c.Path != failurePath || c.Cause != passiveSuspicion {
			t.Fatalf("campaign: %+v", c)
		}
	})
	t.Run("an improvement never starts while a failure campaign runs", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 150).healthy("b", 20).healthy("c", 30).probe("a", time.Second, domain.Failed, 0)
		d := decide(t0, slot, everyone, policyMemory{Selection: serving("a")}, w.evidence, tm)
		if c := d.Memory.Campaign; c == nil || c.Path != failurePath {
			t.Fatalf("campaign: %+v", c)
		}
		again := decide(t0.Add(time.Second), slot, everyone, d.Memory, w.evidence, tm)
		if again.Memory.Campaign.ID != d.Memory.Campaign.ID {
			t.Fatal("a campaign started over a running failure")
		}
	})
}

func TestAFailureIgnoresTheQuietPeriodAndKeepsTheServerOut(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	everyone := poolOf(lin("a"), lin("b"))
	memory := policyMemory{Selection: serving("a"), QuietUntil: t0.Add(time.Minute)}
	w := newScene(domain.WANUp).healthy("b", 20).probe("a", time.Second, domain.Failed, 0)
	d := runCampaign(t, slot, everyone, memory, w, tm, func(c *campaign, w *scene) {
		w.round(c, "a", domain.Failed, 0)
		w.round(c, "b", domain.Succeeded, 20)
	})
	wantSwitch(t, d, domain.FailureMove, "b")
	applied := appliedMemory(t0, d.Memory, domain.Switch{From: serving("a"), To: serving("b"), Reason: domain.FailureMove}, tm)
	if !applied.KeepOut["a"].Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("keep out: %v", applied.KeepOut)
	}
}

func TestWANFreezesMembership(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	candidates := poolOf(lin("a"), lin("b"))
	memory := policyMemory{Selection: serving("a")}

	for name, w := range map[string]*scene{
		"down":    newScene(domain.WANDown),
		"unknown": newScene(domain.WANUnknown),
	} {
		t.Run(name+" WAN ignores a passive suspicion and an improvement", func(t *testing.T) {
			w.healthy("a", 90).healthy("b", 20).passive("a", time.Second)
			wantQuiet(t, decide(t0, slot, candidates, memory, w.evidence, tm))
		})
		t.Run(name+" WAN lets a failed probe start rounds, which prove the WAN or blame nobody", func(t *testing.T) {
			w.probe("a", time.Second, domain.Failed, 0)
			d := decide(t0, slot, candidates, memory, w.evidence, tm)
			if c := d.Memory.Campaign; d.Switch != nil || c == nil || c.Cause != failedProbe {
				t.Fatalf("decision: %+v", d)
			}
		})
	}
	t.Run("a stale baseline is no proof the WAN is up", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 90).healthy("b", 20).passive("a", time.Second)
		w.wan.At = t0.Add(-time.Minute)
		wantQuiet(t, decide(t0, slot, candidates, memory, w.evidence, tm))
	})
	t.Run("an unproven WAN ignores a slow heartbeat", func(t *testing.T) {
		w := newScene(domain.WANDown).healthy("b", 20)
		for range beatMinSamples + 1 {
			w.probe("a", time.Second, domain.Succeeded, 100)
		}
		w.probe("a", time.Second, domain.Succeeded, 1530)
		wantQuiet(t, decide(t0, slot, candidates, memory, w.evidence, tm))
	})
	t.Run("a closed slot stays closed", func(t *testing.T) {
		w := newScene(domain.WANDown).healthy("a", 20).healthy("b", 20)
		wantQuiet(t, decide(t0, slot, candidates, policyMemory{Selection: domain.Closed(domain.ConfirmedDead)}, w.evidence, tm))
	})
	t.Run("a missing member is still withdrawn", func(t *testing.T) {
		w := newScene(domain.WANDown)
		wantClosed(t, decide(t0, slot, poolOf(missing("a"), lin("b")), memory, w.evidence, tm),
			domain.NoUsableServer, domain.ServerLeftFeed)
	})
}

// A closure proves the group was failing from its first failed round. A success from before that says
// nothing, and the slot reads closed until a server answers again.
func TestAClosureDiscountsSuccessesFromBeforeIt(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	candidates := poolOf(lin("a"), lin("b"))

	silent := newScene(domain.WANUp).probe("a", time.Second, domain.Failed, 0).healthy("b", 20)
	d := runCampaign(t, slot, candidates, policyMemory{Selection: serving("a")}, silent, tm, func(c *campaign, w *scene) {
		w.round(c, "a", domain.Failed, 0)
		w.round(c, "b", domain.Failed, 0)
	})
	wantClosed(t, d, domain.ConfirmedDead, domain.FailureMove)
	if !d.Memory.Backoff.ProvenFailing.Equal(t0) {
		t.Fatalf("the closure proved nothing from its first failed round: %v", d.Memory.Backoff.ProvenFailing)
	}

	closed := policyMemory{Selection: domain.Closed(domain.ConfirmedDead), Backoff: reopenBackoff{ProvenFailing: t0.Add(-10 * time.Second)}}
	t.Run("a success older than the failing rounds doesn't reopen", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("b", 11*time.Second, domain.Succeeded, 20)
		if decision := decide(t0, slot, candidates, closed, w.evidence, tm); decision.Switch != nil {
			t.Fatalf("reopened on a success from before the failure: %+v", decision.Switch)
		}
	})
	t.Run("a success since then reopens", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("b", 9*time.Second, domain.Succeeded, 20)
		wantSwitch(t, decide(t0, slot, candidates, closed, w.evidence, tm), domain.Recovery, "b")
	})
}

// A lossy server shows in some rounds and not in others, so a clean round doesn't let it off once
// something says it is slow.
func TestACleanRoundDoesntEndACampaignWithCauseToDoubt(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	candidates := poolOf(lin("a"), lin("b"))
	memory := policyMemory{Selection: serving("a")}
	slowBeat := func() *scene {
		w := newScene(domain.WANUp).healthy("b", 20)
		for i := 4; i > 0; i-- {
			w.probe("a", time.Duration(i+1)*10*time.Second, domain.Succeeded, 300)
		}
		return w.probe("a", time.Second, domain.Succeeded, 1530)
	}
	play := func(pattern ...int) func(*campaign, *scene) {
		next := 0
		return func(c *campaign, w *scene) {
			w.round(c, "a", domain.Succeeded, pattern[next])
			w.round(c, "b", domain.Succeeded, 20)
			next++
		}
	}
	t.Run("slow, clean, slow moves", func(t *testing.T) {
		d := runCampaign(t, slot, candidates, memory, slowBeat(), tm, play(300, 25, 300))
		wantSwitch(t, d, domain.SlownessMove, "b")
	})
	t.Run("clean, slow, clean, slow moves", func(t *testing.T) {
		d := runCampaign(t, slot, candidates, memory, slowBeat(), tm, play(25, 300, 25, 300))
		wantSwitch(t, d, domain.SlownessMove, "b")
	})
	t.Run("it stops once no rival could get its wins", func(t *testing.T) {
		rounds := 0
		pattern := play(25, 25, 25, 25, 25)
		d := runCampaign(t, slot, candidates, memory, slowBeat(), tm, func(c *campaign, w *scene) {
			rounds++
			pattern(c, w)
		})
		if d.Switch != nil || d.Memory.Campaign != nil || rounds != 4 {
			t.Fatalf("after %d clean rounds: %+v", rounds, d)
		}
	})
	t.Run("a failed heartbeat with a clean round still ends at once", func(t *testing.T) {
		rounds := 0
		pattern := play(25, 25, 25)
		d := runCampaign(t, slot, candidates, memory, newScene(domain.WANUp).healthy("b", 20).probe("a", time.Second, domain.Failed, 0), tm, func(c *campaign, w *scene) {
			rounds++
			pattern(c, w)
		})
		if d.Switch != nil || d.Memory.Campaign != nil || rounds != 1 {
			t.Fatalf("after %d rounds: %+v", rounds, d)
		}
	})
	t.Run("a rival that failed starts its wins over", func(t *testing.T) {
		rounds := 0
		w := slowBeat()
		d := runCampaign(t, slot, candidates, memory, w, tm, func(c *campaign, w *scene) {
			rounds++
			w.round(c, "a", domain.Succeeded, 300)
			if rounds%2 == 0 {
				w.round(c, "b", domain.Failed, 0)
			} else {
				w.round(c, "b", domain.Succeeded, 20)
			}
		})
		if d.Switch != nil {
			t.Fatalf("a rival that kept failing won: %+v", d.Switch)
		}
	})
}

func TestSoleMemberRecovery(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	candidates := poolOf(lin("a"))
	closed := policyMemory{Selection: domain.Closed(domain.ConfirmedDead), KeepOut: map[domain.LineageID]time.Time{"a": t0.Add(time.Hour)}}

	t.Run("a dead sole member closes after two rounds", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("a", time.Second, domain.Failed, 0)
		d := runCampaign(t, slot, candidates, policyMemory{Selection: serving("a")}, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", domain.Failed, 0)
		})
		wantClosed(t, d, domain.ConfirmedDead, domain.FailureMove)
	})
	t.Run("a success right after a failure doesn't reopen", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("a", 10*time.Second, domain.Failed, 0).probe("a", time.Second, domain.Succeeded, 20)
		wantQuiet(t, decide(t0, slot, candidates, closed, w.evidence, tm))
	})
	t.Run("a server that flickers stays out", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("a", 25*time.Second, domain.Succeeded, 20).probe("a", 15*time.Second, domain.Failed, 0).
			probe("a", 5*time.Second, domain.Succeeded, 20)
		wantQuiet(t, decide(t0, slot, candidates, closed, w.evidence, tm))
	})
	t.Run("two lucky samples after an old failure don't reopen", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("a", 31*time.Second, domain.Failed, 0).probe("a", 16*time.Second, domain.Succeeded, 20).
			probe("a", time.Second, domain.Succeeded, 20)
		wantQuiet(t, decide(t0, slot, candidates, closed, w.evidence, tm))
	})
	t.Run("three passes close together don't reopen", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("a", 40*time.Second, domain.Failed, 0).probe("a", 25*time.Second, domain.Succeeded, 20).
			probe("a", 15*time.Second, domain.Succeeded, 20).probe("a", 5*time.Second, domain.Succeeded, 20)
		wantQuiet(t, decide(t0, slot, candidates, closed, w.evidence, tm))
	})
	t.Run("a server that held still reopens once, despite its hold-down", func(t *testing.T) {
		w := newScene(domain.WANUp).probe("a", 100*time.Second, domain.Failed, 0).probe("a", 70*time.Second, domain.Succeeded, 20).
			probe("a", 40*time.Second, domain.Succeeded, 20).probe("a", 10*time.Second, domain.Succeeded, 20)
		d := decide(t0, slot, candidates, closed, w.evidence, tm)
		wantSwitch(t, d, domain.Recovery, "a")
		next := appliedMemory(t0, d.Memory, domain.Switch{From: closed.Selection, To: d.Switch.Selection}, tm)
		wantQuiet(t, decide(t0, slot, candidates, next, w.evidence, tm))
	})
}

func TestClosuresBackOff(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	serve := serving("a")
	dead := domain.Closed(domain.ConfirmedDead)
	memory := appliedMemory(t0, policyMemory{}, domain.Switch{From: domain.Closed(domain.Unselected), To: serve}, tm)
	now := t0
	close := func() {
		now = now.Add(time.Minute)
		memory = appliedMemory(now, memory, domain.Switch{From: serve, To: dead}, tm)
	}
	reopen := func() {
		now = now.Add(time.Minute)
		memory = appliedMemory(now, memory, domain.Switch{From: dead, To: serve}, tm)
	}
	for _, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		close()
		if got := memory.Backoff.NotBefore.Sub(now); got != want {
			t.Fatalf("closure %d waits %s, want %s", memory.Backoff.Closures, got, want)
		}
		reopen()
	}
	now = now.Add(tm.StableServing)
	close()
	if memory.Backoff.Closures != 1 || memory.Backoff.NotBefore.Sub(now) != 30*time.Second {
		t.Fatalf("a long stable run did not forgive: %+v", memory)
	}

	t.Run("the last server waits, another may serve", func(t *testing.T) {
		closed := policyMemory{Selection: dead, LastServed: ref("a"), Backoff: reopenBackoff{NotBefore: t0.Add(time.Minute)}}
		w := newScene(domain.WANUp).healthy("a", 20)
		wantQuiet(t, decide(t0, groupSlot(t), poolOf(lin("a")), closed, w.evidence, tm))
		w.healthy("b", 90)
		wantSwitch(t, decide(t0, slot, poolOf(lin("a"), lin("b")), closed, w.evidence, tm), domain.Recovery, "b")
	})
}

func TestLeave(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	memory := policyMemory{Selection: serving("a")}
	others := func(p pool) pool { return aware(p, lin("a"), lin("b"), lin("c")) }
	exitElsewhere := others(poolOf(lin("b"), lin("c")))
	bridge := func(t *testing.T, d decision) {
		t.Helper()
		if d.Switch != nil {
			t.Fatalf("a bridge switched: reason %d to %+v", d.Switch.Reason, d.Switch.Selection)
		}
		if d.Memory.LeaveBy.IsZero() {
			t.Fatal("a bridge with no deadline")
		}
	}
	t.Run("a server that left the feed moves to a ready one, whatever the clocks say", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20)
		wantSwitch(t, decide(t0, slot, aware(poolOf(lin("b"))), memory, w.evidence, tm), domain.ServerLeftFeed, "b")
	})
	t.Run("a server that left the feed closes with nothing ready, and no bridge", func(t *testing.T) {
		wantClosed(t, decide(t0, slot, aware(poolOf(lin("b"))), memory, newScene(domain.WANUp).evidence, tm), domain.NoUsableServer, domain.ServerLeftFeed)
	})
	t.Run("a refused server closes with nothing ready, and says so", func(t *testing.T) {
		p := others(poolOf(lin("b")))
		p.refused[ref("a")] = true
		wantClosed(t, decide(t0, slot, p, memory, newScene(domain.WANUp).evidence, tm), domain.NoUsableServer, domain.RefusedByCore)
	})
	t.Run("a refused server moves to a ready one", func(t *testing.T) {
		p := others(poolOf(lin("b")))
		p.refused[ref("a")] = true
		wantSwitch(t, decide(t0, slot, p, memory, newScene(domain.WANUp).healthy("b", 20).evidence, tm), domain.RefusedByCore, "b")
	})
	t.Run("an Exit that stopped allowing it moves to a ready one", func(t *testing.T) {
		d := decide(t0, slot, exitElsewhere, memory, newScene(domain.WANUp).healthy("a", 20).healthy("b", 20).evidence, tm)
		wantSwitch(t, d, domain.ExitChanged, "b")
		if !d.Memory.LeaveBy.IsZero() {
			t.Fatal("a move kept a deadline the new server doesn't have")
		}
	})
	t.Run("with nothing ready it bridges and sets the deadline once", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 20)
		d := decide(t0, slot, exitElsewhere, memory, w.evidence, tm)
		bridge(t, d)
		if !d.Memory.LeaveBy.Equal(t0.Add(tm.Handover)) {
			t.Fatalf("deadline %v", d.Memory.LeaveBy)
		}
		later := decide(t0.Add(10*time.Second), slot, exitElsewhere, d.Memory, w.evidence, tm)
		bridge(t, later)
		if !later.Memory.LeaveBy.Equal(d.Memory.LeaveBy) {
			t.Fatalf("a later evaluation renewed the deadline: %v", later.Memory.LeaveBy)
		}
	})
	t.Run("a further edit that still doesn't allow it keeps the earlier deadline", func(t *testing.T) {
		m := memory
		m.LeaveBy = t0.Add(5 * time.Second)
		d := decide(t0, slot, others(poolOf(lin("c"))), m, newScene(domain.WANUp).healthy("a", 20).evidence, tm)
		bridge(t, d)
		if !d.Memory.LeaveBy.Equal(m.LeaveBy) {
			t.Fatalf("deadline %v", d.Memory.LeaveBy)
		}
	})
	t.Run("an Exit that allows it again clears the deadline", func(t *testing.T) {
		m := memory
		m.LeaveBy = t0.Add(5 * time.Second)
		d := decide(t0, slot, others(poolOf(lin("a"), lin("b"))), m, newScene(domain.WANUp).healthy("a", 20).evidence, tm)
		if d.Switch != nil || !d.Memory.LeaveBy.IsZero() {
			t.Fatalf("decision: %+v", d)
		}
	})
	t.Run("at the deadline it closes, and not a moment before", func(t *testing.T) {
		m := memory
		m.LeaveBy = t0
		w := newScene(domain.WANUp).healthy("a", 20)
		bridge(t, decide(t0.Add(-time.Millisecond), slot, exitElsewhere, m, w.evidence, tm))
		wantClosed(t, decide(t0, slot, exitElsewhere, m, w.evidence, tm), domain.NoUsableServer, domain.ExitChanged)
	})
	t.Run("a closure carries no deadline into the closed slot", func(t *testing.T) {
		m := memory
		m.LeaveBy = t0
		if d := decide(t0, slot, exitElsewhere, m, newScene(domain.WANUp).evidence, tm); !d.Memory.LeaveBy.IsZero() {
			t.Fatalf("deadline %v", d.Memory.LeaveBy)
		}
	})
	t.Run("a server that fails on the bridge closes at once, with no rounds", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 20).probe("a", time.Second, domain.Failed, 0)
		d := decide(t0, slot, exitElsewhere, memory, w.evidence, tm)
		wantClosed(t, d, domain.NoUsableServer, domain.ExitChanged)
	})
	t.Run("an outage can't close a bridge early", func(t *testing.T) {
		w := newScene(domain.WANDown).healthy("a", 20).probe("a", time.Second, domain.Unknown, 0)
		bridge(t, decide(t0, slot, exitElsewhere, memory, w.evidence, tm))
	})
	t.Run("an outage can't extend a bridge either", func(t *testing.T) {
		m := memory
		m.LeaveBy = t0
		wantClosed(t, decide(t0, slot, exitElsewhere, m, newScene(domain.WANDown).evidence, tm), domain.NoUsableServer, domain.ExitChanged)
	})
	t.Run("a rotation on the bridge is followed and keeps the deadline", func(t *testing.T) {
		rotated := lin("a")
		rotated.Ref.Digest = domain.Digest{9}
		m := memory
		m.LeaveBy = t0.Add(5 * time.Second)
		d := decide(t0, slot, aware(poolOf(lin("b")), rotated, lin("b")), m, newScene(domain.WANUp).evidence, tm)
		if d.Switch == nil || d.Switch.Reason != domain.RevisionChanged || d.Switch.Selection != domain.Serving(rotated.Ref) {
			t.Fatalf("decision: %+v", d)
		}
		if !d.Memory.LeaveBy.Equal(m.LeaveBy) {
			t.Fatalf("a rotation renewed the deadline: %v", d.Memory.LeaveBy)
		}
	})
	t.Run("it drops a running campaign", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 20).healthy("b", 20).probe("a", time.Second, domain.Failed, 0)
		started := decide(t0, slot, poolOf(lin("a"), lin("b")), policyMemory{Selection: serving("a")}, w.evidence, tm)
		if started.Memory.Campaign == nil {
			t.Fatal("no campaign")
		}
		w2 := newScene(domain.WANUp).healthy("a", 20)
		d := decide(t0, slot, exitElsewhere, started.Memory, w2.evidence, tm)
		bridge(t, d)
		if d.Memory.Campaign != nil {
			t.Fatal("campaign kept")
		}
	})
	t.Run("a rotated server in the pool is followed without a campaign", func(t *testing.T) {
		rotated := lin("a")
		rotated.Ref.Digest = domain.Digest{9}
		d := decide(t0, slot, poolOf(rotated, lin("b")), memory, newScene(domain.WANDown).evidence, tm)
		if d.Switch == nil || d.Switch.Reason != domain.RevisionChanged {
			t.Fatalf("decision: %+v", d)
		}
	})
}

func TestServerExit(t *testing.T) {
	tm := defaultTimings()
	slot := serverSlot(t, "p")
	serve := policyMemory{Selection: serving("p")}

	t.Run("a missing server closes at once", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("x", 20)
		wantClosed(t, decide(t0, slot, aware(directOf(missing("p")), missing("p")), serve, w.evidence, tm), domain.ServerMissing, domain.ServerLeftFeed)
	})
	t.Run("a missing server closes during a WAN outage too", func(t *testing.T) {
		wantClosed(t, decide(t0, slot, aware(directOf(missing("p")), missing("p")), serve, newScene(domain.WANDown).evidence, tm), domain.ServerMissing, domain.ServerLeftFeed)
	})
	t.Run("a refused server says so", func(t *testing.T) {
		refused := aware(directOf(missing("p")), lin("p"))
		refused.empty, refused.refused[ref("p")] = domain.ServerRefused, true
		wantClosed(t, decide(t0, slot, refused, serve, newScene(domain.WANUp).evidence, tm), domain.ServerRefused, domain.RefusedByCore)
	})
	t.Run("a dead server closes and never takes another", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("x", 20).probe("p", time.Second, domain.Failed, 0)
		d := runCampaign(t, slot, directOf(lin("p")), serve, w, tm, func(c *campaign, w *scene) {
			if len(c.Rivals) != 0 {
				t.Fatalf("a server Exit compared %v", c.Rivals)
			}
			w.round(c, "p", domain.Failed, 0)
		})
		wantClosed(t, d, domain.ConfirmedDead, domain.FailureMove)
	})
	t.Run("a slow server stays", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("p", 400).healthy("x", 20)
		wantQuiet(t, decide(t0, slot, directOf(lin("p")), serve, w.evidence, tm))
	})
	t.Run("a same-lineage rotation is followed", func(t *testing.T) {
		rotated := lin("p")
		rotated.Ref.Digest = domain.Digest{2}
		d := decide(t0, slot, directOf(rotated), serve, newScene(domain.WANUp).evidence, tm)
		if d.Switch == nil || d.Switch.Reason != domain.RevisionChanged || d.Switch.Selection != domain.Serving(rotated.Ref) {
			t.Fatalf("decision: %+v", d)
		}
	})
	t.Run("a slot on another server goes to its Exit once that is ready", func(t *testing.T) {
		m := policyMemory{Selection: serving("x")}
		exit := aware(directOf(lin("p")), lin("p"), lin("x"))
		d := decide(t0, slot, exit, m, newScene(domain.WANDown).evidence, tm)
		if d.Switch != nil || !d.Memory.LeaveBy.Equal(t0.Add(tm.Handover)) {
			t.Fatalf("decision: %+v", d)
		}
		wantSwitch(t, decide(t0, slot, exit, m, newScene(domain.WANDown).healthy("p", 20).evidence, tm), domain.ExitChanged, "p")
	})
	t.Run("past the deadline its Exit isn't called missing", func(t *testing.T) {
		m := policyMemory{Selection: serving("x"), LeaveBy: t0}
		wantClosed(t, decide(t0, slot, aware(directOf(lin("p")), lin("p"), lin("x")), m, newScene(domain.WANUp).evidence, tm), domain.NoUsableServer, domain.ExitChanged)
	})
	t.Run("a closed slot on a missing server waits", func(t *testing.T) {
		wantQuiet(t, decide(t0, slot, directOf(missing("p")), policyMemory{Selection: domain.Closed(domain.ServerMissing)}, newScene(domain.WANUp).evidence, tm))
	})
}

func TestACampaignIsAbandonedOnlyWhenItsIncumbentOrEveryRivalIsGone(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	everyone := poolOf(lin("a"), lin("b"), lin("c"))
	memory := policyMemory{Selection: serving("a")}
	scene := func() *scene {
		return newScene(domain.WANUp).healthy("b", 20).healthy("c", 30).probe("a", time.Second, domain.Failed, 0)
	}
	started := decide(t0, slot, everyone, memory, scene().evidence, tm)
	if started.Memory.Campaign == nil || len(started.Memory.Campaign.Rivals) != 2 {
		t.Fatalf("no campaign: %+v", started)
	}
	keeps := func(t *testing.T, d decision) {
		t.Helper()
		if d.Memory.Campaign == nil || d.Memory.Campaign.ID != started.Memory.Campaign.ID {
			t.Fatalf("campaign after: %+v", d.Memory.Campaign)
		}
	}
	abandons := func(t *testing.T, d decision) {
		t.Helper()
		if c := d.Memory.Campaign; c != nil && c.ID == started.Memory.Campaign.ID {
			t.Fatal("campaign kept")
		}
		if !d.Memory.QuietUntil.IsZero() || d.Memory.ReadThrough != 0 {
			t.Fatalf("an abandoned campaign set a clock: %+v", d.Memory)
		}
	}
	t.Run("an unrelated addition cancels nothing", func(t *testing.T) {
		keeps(t, decide(t0, slot, poolOf(lin("a"), lin("b"), lin("c"), lin("d")), started.Memory, scene().evidence, tm))
	})
	t.Run("a non-participant leaving cancels nothing", func(t *testing.T) {
		m := started.Memory
		c := *m.Campaign
		c.Rivals = []domain.ServerRef{ref("b")}
		m.Campaign = &c
		keeps(t, decide(t0, slot, poolOf(lin("a"), lin("b")), m, scene().evidence, tm))
	})
	t.Run("a rival leaving cancels nothing", func(t *testing.T) {
		keeps(t, decide(t0, slot, poolOf(lin("a"), lin("c")), started.Memory, scene().evidence, tm))
	})
	t.Run("a rival rotating cancels nothing", func(t *testing.T) {
		rotated := lin("b")
		rotated.Ref.Digest = domain.Digest{7}
		keeps(t, decide(t0, slot, poolOf(lin("a"), rotated, lin("c")), started.Memory, scene().evidence, tm))
	})
	t.Run("every rival leaving abandons, and the failed probe confirms again at once", func(t *testing.T) {
		d := decide(t0, slot, poolOf(lin("a"), lin("d")), started.Memory, scene().evidence, tm)
		abandons(t, d)
		if d.Memory.Campaign == nil || d.Memory.Campaign.Incumbent != ref("a") || !slices.Equal(d.Memory.Campaign.Rivals, []domain.ServerRef{ref("d")}) {
			t.Fatalf("a real failure didn't confirm again: %+v", d.Memory.Campaign)
		}
	})
	rotatedB := lin("b")
	rotatedB.Ref.Digest = domain.Digest{7}
	answered := func(b domain.Verdict) *evidence {
		w := scene()
		c := started.Memory.Campaign
		w.round(c, "a", domain.Failed, 0)
		w.round(c, "b", b, 20)
		w.round(c, "c", domain.Failed, 0)
		return w.evidence
	}
	t.Run("the next round asks a rotated rival on its new ref, with what it has won", func(t *testing.T) {
		d := decide(t0, slot, poolOf(lin("a"), rotatedB, lin("c")), started.Memory, answered(domain.Succeeded), tm)
		c := d.Memory.Campaign
		if c == nil || c.Round != 2 || !slices.Equal(c.Rivals, []domain.ServerRef{rotatedB.Ref, ref("c")}) || !slices.Equal(c.Wins, []tally{{1, 0}, {}}) {
			t.Fatalf("campaign: %+v", c)
		}
	})
	t.Run("a rival that rotated in its last round wins on its new ref", func(t *testing.T) {
		m := started.Memory
		c := *m.Campaign
		c.Wins = []tally{{1, 0}, {}}
		m.Campaign = &c
		wantSwitch(t, decide(t0, slot, poolOf(lin("a"), rotatedB, lin("c")), m, answered(domain.Succeeded), tm), domain.FailureMove, "b")
	})
	t.Run("a rival that left in its last round can't win", func(t *testing.T) {
		m := started.Memory
		c := *m.Campaign
		c.Wins, c.Failed = []tally{{1, 0}, {}}, 1
		m.Campaign = &c
		wantClosed(t, decide(t0, slot, poolOf(lin("a"), lin("c")), m, answered(domain.Succeeded), tm), domain.ConfirmedDead, domain.FailureMove)
	})
	t.Run("a round a rotation left with no answer runs again on the new ref", func(t *testing.T) {
		d := decide(t0, slot, poolOf(lin("a"), rotatedB, lin("c")), started.Memory, answered(domain.Unknown), tm)
		c := d.Memory.Campaign
		if c == nil || c.ID != started.Memory.Campaign.ID || c.Round != 2 || !slices.Contains(c.Rivals, rotatedB.Ref) || !d.Memory.QuietUntil.IsZero() {
			t.Fatalf("memory: %+v", d.Memory)
		}
	})
	t.Run("a new core epoch abandons", func(t *testing.T) {
		next := newScene(domain.WANUp)
		next.epoch = "e2"
		abandons(t, decide(t0, slot, everyone, started.Memory, next.evidence, tm))
	})
}

// A failed probe that couldn't prove the WAN worked starts rounds and blames nobody, and only a probe
// that was never asked says nothing.
func TestAFailedProbeTheBaselineCouldntJudgeStartsRounds(t *testing.T) {
	tm := defaultTimings()
	everyone := poolOf(lin("a"), lin("b"))
	memory := policyMemory{Selection: serving("a")}
	excluded := func(unreachable bool) *scene {
		w := newScene(domain.WANUp).healthy("b", 20)
		s := domain.Signal{Stamp: domain.Stamp{At: t0.Add(-time.Second)}, Server: ref("a"), Kind: domain.ActiveProbe, Verdict: domain.Unknown, Purpose: domain.Heartbeat, WANExcluded: !unreachable}
		w.server(ref("a")).active.push(w.hear(s))
		return w
	}
	d := decide(t0, groupSlot(t), everyone, memory, excluded(false).evidence, tm)
	if d.Switch != nil || d.Memory.Campaign == nil || d.Memory.Campaign.Cause != failedProbe {
		t.Fatalf("decision: %+v", d)
	}
	wantQuiet(t, decide(t0, groupSlot(t), everyone, memory, excluded(true).evidence, tm))
	read := memory
	seen := excluded(false)
	read.ReadThrough = seen.heard
	wantQuiet(t, decide(t0, groupSlot(t), everyone, read, seen.evidence, tm))
	down := excluded(false)
	down.wan.State = domain.WANDown
	if c := decide(t0, groupSlot(t), everyone, memory, down.evidence, tm).Memory.Campaign; c == nil || c.Cause != failedProbe {
		t.Fatalf("with the WAN unproven, campaign: %+v", c)
	}
}

// A frame saying the WAN is down lands in the middle of a confirmation. The round already running
// proves the WAN or blames nobody, so the campaign goes on.
func TestAWANBlipLeavesARunningFailureCampaignAlone(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	everyone := poolOf(lin("a"), lin("b"))
	w := newScene(domain.WANUp).healthy("b", 20).probe("a", time.Second, domain.Failed, 0)
	started := decide(t0, slot, everyone, policyMemory{Selection: serving("a")}, w.evidence, tm)
	c := started.Memory.Campaign
	if c == nil || c.Path != failurePath {
		t.Fatalf("campaign: %+v", c)
	}
	w.wan.State = domain.WANDown
	t.Run("with its round out, it waits", func(t *testing.T) {
		d := decide(t0, slot, everyone, started.Memory, w.evidence, tm)
		if d.Memory.Campaign == nil || d.Memory.Campaign.ID != c.ID {
			t.Fatalf("campaign: %+v", d.Memory.Campaign)
		}
	})
	t.Run("a round that came back counts", func(t *testing.T) {
		w.round(c, "a", domain.Failed, 0)
		w.round(c, "b", domain.Succeeded, 20)
		d := decide(t0, slot, everyone, started.Memory, w.evidence, tm)
		if d.Memory.Campaign == nil || d.Memory.Campaign.Round != 2 || d.Memory.Campaign.Failed != 1 {
			t.Fatalf("campaign: %+v", d.Memory.Campaign)
		}
	})
	t.Run("a real outage ends it in an inconclusive round, blaming nobody", func(t *testing.T) {
		out := newScene(domain.WANDown).healthy("b", 20).probe("a", time.Second, domain.Failed, 0)
		out.round(c, "a", domain.Unknown, 0)
		out.round(c, "b", domain.Unknown, 0)
		d := decide(t0, slot, everyone, started.Memory, out.evidence, tm)
		if d.Switch != nil || d.Memory.Campaign != nil {
			t.Fatalf("decision: %+v", d)
		}
	})
	t.Run("an improvement doesn't survive it", func(t *testing.T) {
		m := policyMemory{Selection: serving("a"), ReadThrough: w.heard, Campaign: &campaign{ID: "i", Path: improvePath, Epoch: "e1", Incumbent: ref("a"), Rivals: []domain.ServerRef{ref("b")}, Wins: []tally{{}}, Due: t0, Round: 1}}
		if d := decide(t0, slot, everyone, m, w.evidence, tm); d.Memory.Campaign != nil {
			t.Fatalf("campaign: %+v", d.Memory.Campaign)
		}
	})
}

func TestOffVPN(t *testing.T) {
	tm := defaultTimings()
	slot := domain.SlotOwner("eu")
	everyone := poolOf(lin("a"), lin("b"))
	off := newWorld(domain.NewCatalogue(1, []domain.Lineage{lin("a"), lin("b")}), nil, nil, nil).Pool(domain.BlockExit())
	wantBlocked := func(t *testing.T, d decision, reason domain.Reason) {
		t.Helper()
		if d.Switch == nil || !d.Switch.Selection.IsBlocked() || d.Switch.Reason != reason {
			t.Fatalf("switch %+v, want Blocked by reason %d", d.Switch, reason)
		}
	}
	t.Run("blocks a serving slot at once and drops its campaign, even with the WAN down", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20).probe("a", time.Second, domain.Failed, 0)
		started := decide(t0, groupSlot(t), everyone, policyMemory{Selection: serving("a")}, w.evidence, tm)
		w.wan.State = domain.WANDown
		d := decide(t0, slot, off, started.Memory, w.evidence, tm)
		wantBlocked(t, d, domain.ExitChanged)
		if d.Memory.Campaign != nil {
			t.Fatal("campaign kept")
		}
	})
	t.Run("stays blocked, whatever is ready", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 20)
		wantQuiet(t, decide(t0, slot, off, policyMemory{Selection: domain.Blocked()}, w.evidence, tm))
	})
	t.Run("moves from Direct", func(t *testing.T) {
		wantBlocked(t, decide(t0, slot, off, policyMemory{Selection: domain.Direct()}, newScene(domain.WANUp).evidence, tm), domain.ExitChanged)
	})
	t.Run("beats nothing", func(t *testing.T) {
		e := evaluation{now: t0, who: slot, pool: off, memory: policyMemory{Selection: domain.Blocked()}, ev: newScene(domain.WANUp).evidence, t: tm}
		if got := e.near(); len(got) != 0 {
			t.Fatalf("near %v", got)
		}
	})
	t.Run("coming back is a normal reopening with no back-off", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 20).healthy("b", 20)
		m := policyMemory{Selection: domain.Blocked(), LastServed: ref("a"), Backoff: reopenBackoff{NotBefore: t0.Add(time.Hour)}}
		wantSwitch(t, decide(t0, groupSlot(t), poolOf(lin("a")), m, w.evidence, tm), domain.ExitChanged, "a")
	})
	t.Run("coming back with nothing ready closes at once", func(t *testing.T) {
		w := newScene(domain.WANDown).healthy("a", 20)
		wantClosed(t, decide(t0, groupSlot(t), poolOf(lin("a")), policyMemory{Selection: domain.Blocked()}, w.evidence, tm), domain.NoExit, domain.ExitChanged)
	})
	t.Run("a new slot on Block starts there", func(t *testing.T) {
		wantBlocked(t, decide(t0, slot, off, policyMemory{}, newScene(domain.WANUp).evidence, tm), domain.InitialSelection)
	})
}

func TestAppliedMemoryStartsClocksOnApply(t *testing.T) {
	tm := defaultTimings()
	from, to := serving("a"), serving("b")
	previous := policyMemory{Selection: from, KeepOut: map[domain.LineageID]time.Time{"z": t0.Add(-time.Second), "y": t0.Add(time.Minute)}}
	cases := []struct {
		reason domain.Reason
		keep   bool
	}{
		{domain.FailureMove, true},
		{domain.SlownessMove, true},
		{domain.UDPMove, true},
		{domain.ImprovementMove, false},
		{domain.ExitChanged, false},
		{domain.ServerLeftFeed, false},
		{domain.RefusedByCore, false},
		{domain.RevisionChanged, false},
		{domain.Recovery, false},
	}
	for _, c := range cases {
		m := appliedMemory(t0, previous, domain.Switch{From: from, To: to, Reason: c.reason}, tm)
		if _, got := m.KeepOut["a"]; got != c.keep {
			t.Errorf("reason %d: keep out %v, want %v", c.reason, m.KeepOut, c.keep)
		}
		if _, expired := m.KeepOut["z"]; expired {
			t.Error("an expired KeepOut survived")
		}
		if _, running := m.KeepOut["y"]; !running {
			t.Error("a running KeepOut was lost")
		}
		if m.LastServed != ref("b") {
			t.Errorf("last served: %v", m.LastServed)
		}
	}
	if m := appliedMemory(t0, previous, domain.Switch{From: from, To: domain.Closed(domain.ConfirmedDead), Reason: domain.FailureMove}, tm); m.KeepOut["a"].IsZero() {
		t.Errorf("a confirmed closure: %+v", m)
	}
}

func TestTheNearServersAreTheCurrentOneAndTheTopFewWhileLooking(t *testing.T) {
	candidates := poolOf(lin("a"), missing("b"), lin("c"), lin("d"), lin("e"), lin("f"))
	slot := groupSlot(t)
	watching := func(m policyMemory) []domain.ServerRef {
		e := evaluation{now: t0, who: slot, pool: candidates, memory: m, ev: newScene(domain.WANUp).evidence, t: defaultTimings()}
		return e.near()
	}
	if got := watching(policyMemory{Selection: serving("d")}); !slices.Equal(got, []domain.ServerRef{ref("d")}) {
		t.Fatalf("serving: %v", got)
	}
	if got := watching(policyMemory{}); !slices.Equal(got, []domain.ServerRef{ref("a"), ref("c"), ref("d"), ref("e")}) {
		t.Fatalf("closed: %v", got)
	}
	if got := watching(policyMemory{Selection: serving("x"), LeaveBy: t0.Add(time.Second)}); !slices.Equal(got, []domain.ServerRef{ref("x"), ref("a"), ref("c"), ref("d"), ref("e")}) {
		t.Fatalf("leaving: %v", got)
	}
}

// The measured table: Germany 65 to 68 ms, Bulgaria 67, Netherlands 74 to 75, Sweden 100, USA 218.
func TestGainMarginsOnTheMeasuredTable(t *testing.T) {
	cases := []struct {
		mine, theirs int
		want         bool
	}{
		{68, 65, false}, {75, 65, false}, {74, 67, false}, {100, 67, true}, {218, 67, true},
		{1000, 900, false}, {40, 25, false},
	}
	for _, c := range cases {
		if got := beatsByGain(time.Duration(c.mine)*time.Millisecond, time.Duration(c.theirs)*time.Millisecond); got != c.want {
			t.Errorf("%d ms against %d ms: %v, want %v", c.mine, c.theirs, got, c.want)
		}
	}
}

// Ranking reads the record's class and then its score, which a heartbeat's new connection doesn't move.
func TestRankingIsClassThenScore(t *testing.T) {
	w := newScene(domain.WANUp).healthy("b", 20).healthy("c", 900).probe("a", time.Second, domain.Failed, 0)
	e := evaluation{now: t0, pool: poolOf(), ev: w.evidence, t: defaultTimings()}
	got := e.ranked([]domain.Lineage{lin("d"), lin("c"), lin("b"), lin("a")})
	var ids []domain.LineageID
	for _, l := range got {
		ids = append(ids, l.ID)
	}
	if !slices.Equal(ids, []domain.LineageID{"b", "c", "d", "a"}) {
		t.Fatalf("ranked %v", ids)
	}
}

func TestARoundWithNoPingEndsTheComparison(t *testing.T) {
	tm := defaultTimings()
	slot := groupSlot(t)
	candidates := poolOf(lin("a"), lin("b"))
	memory := policyMemory{Selection: serving("a")}
	t.Run("a passing incumbent with no Ping blames no one", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 150).healthy("b", 20)
		wantQuiet(t, runCampaign(t, slot, candidates, memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", domain.Succeeded, 0)
			w.round(c, "b", domain.Succeeded, 20)
		}))
	})
	t.Run("a rival with no Ping can't beat a passing incumbent", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("a", 150).healthy("b", 20)
		wantQuiet(t, runCampaign(t, slot, candidates, memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", domain.Succeeded, 150)
			w.round(c, "b", domain.Succeeded, 0)
		}))
	})
	t.Run("a rival with no Ping can replace a failed incumbent", func(t *testing.T) {
		w := newScene(domain.WANUp).healthy("b", 20).probe("a", time.Second, domain.Failed, 0)
		wantSwitch(t, runCampaign(t, slot, candidates, memory, w, tm, func(c *campaign, w *scene) {
			w.round(c, "a", domain.Failed, 0)
			w.round(c, "b", domain.Succeeded, 0)
		}), domain.FailureMove, "b")
	})
}

// Two rivals reaching their second win in one round: the better ranked takes over, not the one whose
// live ping was lower.
func TestATieGoesToTheBetterRankedRival(t *testing.T) {
	w := newScene(domain.WANUp).healthy("b", 20).healthy("c", 30).probe("a", time.Second, domain.Failed, 0)
	flaky := record{Samples: []sample{{At: t0.Add(-time.Hour), Ping: 50 * time.Millisecond}}}
	flaky, _ = flaky.charged(t0.Add(-2 * time.Hour))
	flaky.Incidents = append(flaky.Incidents, t0.Add(-time.Hour))
	w.book.records["b"] = flaky
	w.book.records["c"] = record{Samples: []sample{{At: t0.Add(-time.Hour), Ping: 50 * time.Millisecond}}}
	d := runCampaign(t, groupSlot(t), poolOf(lin("a"), lin("b"), lin("c")), policyMemory{Selection: serving("a")}, w, defaultTimings(), func(c *campaign, w *scene) {
		w.round(c, "a", domain.Failed, 0)
		w.round(c, "b", domain.Succeeded, 20)
		w.round(c, "c", domain.Succeeded, 30)
	})
	wantSwitch(t, d, domain.FailureMove, "c")
}

// Candidacy needs no fresh evidence: a server whose record stands passing is ready however old its pass.
func TestAPassingRecordIsReadyWithoutAFreshPass(t *testing.T) {
	w := newScene(domain.WANUp).probe("a", 10*time.Minute, domain.Succeeded, 50)
	d := decide(t0, groupSlot(t), poolOf(lin("a")), policyMemory{Selection: domain.Closed(domain.Unselected)}, w.evidence, defaultTimings())
	wantSwitch(t, d, domain.InitialSelection, "a")
}

// b is picked as a rival while it waits for its sixth retry, and that retry fails before the round:
// b is unverified. It passes both rounds and still can't win them.
func TestARivalUnverifiedBeforeItsRoundCantWinIt(t *testing.T) {
	w := newScene(domain.WANUp).healthy("a", 40).probe("b", time.Hour, domain.Failed, 0)
	r := w.book.records["b"]
	r.Failures, r.Retries, r.SteppedAt = 6, 5, t0.Add(-32*time.Minute)
	w.book.put("b", r, true)
	e := evaluation{now: t0, who: groupSlot(t), pool: poolOf(lin("a"), lin("b")), memory: policyMemory{Selection: serving("a")}, ev: w.evidence, t: defaultTimings()}
	d := e.begin(failurePath, suspect{cause: passiveSuspicion}, ref("a"), e.rivals([]domain.ServerRef{ref("a")}, failurePath))
	w.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: t0}, Signals: []domain.Signal{{Stamp: domain.Stamp{At: t0, Epoch: "e1"}, Server: ref("b"), Kind: domain.ActiveProbe, Purpose: domain.Check, Verdict: domain.Failed}}})
	if !unverified(w.book.records["b"]) {
		t.Fatal("the sixth retry left b on the ladder")
	}
	for n := 1; n <= 2; n++ {
		e.memory = d.Memory
		c := *e.memory.Campaign
		e.now = t0.Add(time.Duration(n) * 5 * time.Second)
		w.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: e.now}, Signals: []domain.Signal{
			{Stamp: domain.Stamp{At: e.now, Epoch: "e1"}, Server: ref("a"), Kind: domain.ActiveProbe, Purpose: domain.Confirmation, Verdict: domain.Failed, Campaign: c.ID, Round: c.Round},
			{Stamp: domain.Stamp{At: e.now, Epoch: "e1"}, Server: ref("b"), Kind: domain.ActiveProbe, Purpose: domain.Confirmation, Verdict: domain.Succeeded, Campaign: c.ID, Round: c.Round},
		}})
		d = e.advance(c, ref("a"))
	}
	if d.Switch != nil {
		if to, _ := d.Switch.Selection.Server(); to.Lineage == "b" {
			t.Fatalf("unverified b won the failure with %d spaced passes", w.book.records["b"].Streak)
		}
	}
}

// b is a's rival after five failed retries, and its sixth passes before the round. A passing retry
// starts b's recovery and spends no step, so b is not unverified, and it takes the group when a fails
// both rounds and it passes them.
func TestARivalWhoseLastRetryPassesCanWinItsRound(t *testing.T) {
	for _, spent := range []int{unverifiedAfter - 2, unverifiedAfter - 1} {
		w := newScene(domain.WANUp).healthy("a", 40).probe("b", time.Hour, domain.Failed, 0)
		r := w.book.records["b"]
		r.Failures, r.Retries, r.SteppedAt = spent+1, spent, t0.Add(-32*time.Minute)
		w.book.put("b", r, true)
		e := evaluation{now: t0, who: groupSlot(t), pool: poolOf(lin("a"), lin("b")), memory: policyMemory{Selection: serving("a")}, ev: w.evidence, t: defaultTimings()}
		d := e.begin(failurePath, suspect{cause: failedProbe}, ref("a"), e.rivals([]domain.ServerRef{ref("a")}, failurePath))
		if len(d.Memory.Campaign.Rivals) != 1 {
			t.Fatalf("after %d failed retries b is not a's rival", spent)
		}
		w.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: t0.Add(time.Second)}, Signals: []domain.Signal{{Stamp: domain.Stamp{At: t0.Add(time.Second), Epoch: "e1"}, Server: ref("b"), Kind: domain.ActiveProbe, Purpose: domain.Check, Verdict: domain.Succeeded}}})
		if unverified(w.book.records["b"]) {
			t.Fatalf("a passing retry after %d failed ones made b unverified", spent)
		}
		for n := 1; n <= 2; n++ {
			e.memory = d.Memory
			c := *e.memory.Campaign
			e.now = t0.Add(time.Duration(n) * 5 * time.Second)
			w.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: e.now}, Signals: []domain.Signal{
				{Stamp: domain.Stamp{At: e.now, Epoch: "e1"}, Server: ref("a"), Kind: domain.ActiveProbe, Purpose: domain.Confirmation, Verdict: domain.Failed, Campaign: c.ID, Round: c.Round},
				{Stamp: domain.Stamp{At: e.now, Epoch: "e1"}, Server: ref("b"), Kind: domain.ActiveProbe, Purpose: domain.Confirmation, Verdict: domain.Succeeded, Campaign: c.ID, Round: c.Round},
			}})
			d = e.advance(c, ref("a"))
		}
		if d.Switch == nil {
			t.Fatalf("after %d failed retries and a passing one, no move when a failed both rounds", spent)
		}
		if to, ok := d.Switch.Selection.Server(); !ok || to.Lineage != "b" {
			t.Fatalf("after %d failed retries and a passing one, the group went to %+v, want b", spent, d.Switch.Selection)
		}
	}
}

// b failed and c is ready. Both are a's rivals. c says nothing in the first round and passes the
// second, while b passes both. A not-ready server wins a failover only when nothing ready passes, so
// the move goes to c.
func TestAReadyRivalPassingTheDecisiveRoundBeatsARecoveringOne(t *testing.T) {
	w := newScene(domain.WANUp).healthy("a", 40).healthy("c", 60).probe("b", time.Second, domain.Failed, 0)
	e := evaluation{now: t0, who: groupSlot(t), pool: poolOf(lin("a"), lin("b"), lin("c")), memory: policyMemory{Selection: serving("a")}, ev: w.evidence, t: defaultTimings()}
	d := e.begin(failurePath, suspect{cause: failedProbe}, ref("a"), e.rivals([]domain.ServerRef{ref("a")}, failurePath))
	for n := 1; n <= 2; n++ {
		e.memory = d.Memory
		c := *e.memory.Campaign
		e.now = t0.Add(time.Duration(n) * 5 * time.Second)
		verdicts := map[string]domain.Verdict{"a": domain.Failed, "b": domain.Succeeded, "c": domain.Succeeded}
		if n == 1 {
			verdicts["c"] = domain.Unknown
		}
		var signals []domain.Signal
		for id, v := range verdicts {
			signals = append(signals, domain.Signal{Stamp: domain.Stamp{At: e.now, Epoch: "e1"}, Server: ref(id), Kind: domain.ActiveProbe, Purpose: domain.Confirmation, Verdict: v, Campaign: c.ID, Round: c.Round})
		}
		w.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: e.now}, Signals: signals})
		d = e.advance(c, ref("a"))
	}
	if d.Switch == nil {
		t.Fatal("no move after a failed both rounds")
	}
	if to, _ := d.Switch.Selection.Server(); to.Lineage != "c" {
		t.Fatalf("moved to %s, want ready c", to.Lineage)
	}
}

// slowRounds runs two rounds of a slowness campaign on a, each server measuring the same in both, and
// returns the last decision. b failed and stands recovering, and c is ready.
func slowRounds(t *testing.T, measured map[string]domain.Signal) decision {
	t.Helper()
	w := newScene(domain.WANUp).healthy("a", 40).healthy("c", 60).probe("b", time.Second, domain.Failed, 0)
	e := evaluation{now: t0, who: groupSlot(t), pool: poolOf(lin("a"), lin("b"), lin("c")), memory: policyMemory{Selection: serving("a")}, ev: w.evidence, t: defaultTimings()}
	d := e.begin(failurePath, suspect{cause: slowHeartbeat}, ref("a"), e.rivals([]domain.ServerRef{ref("a")}, failurePath))
	for n := 1; n <= 2; n++ {
		e.memory = d.Memory
		c := *e.memory.Campaign
		e.now = t0.Add(time.Duration(n) * 5 * time.Second)
		var signals []domain.Signal
		for id, s := range measured {
			s.Stamp, s.Server, s.Kind, s.Purpose, s.Verdict, s.Campaign, s.Round = domain.Stamp{At: e.now, Epoch: "e1"}, ref(id), domain.ActiveProbe, domain.Confirmation, domain.Succeeded, c.ID, c.Round
			signals = append(signals, s)
		}
		w.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: e.now}, Signals: signals})
		d = e.advance(c, ref("a"))
	}
	if w.book.records["b"].standing() != recovering || !e.ready(lin("c")) {
		t.Fatalf("b stands %d and c ready is %v, want b recovering beside a ready c", w.book.records["b"].standing(), e.ready(lin("c")))
	}
	return d
}

// pings is a round where a, b and c measure these Pings.
func pings(a, b, c int) map[string]domain.Signal {
	return map[string]domain.Signal{"a": {Ping: ms(a)}, "b": {Ping: ms(b)}, "c": {Ping: ms(c)}}
}

func wantEnded(t *testing.T, d decision) {
	t.Helper()
	if d.Switch != nil {
		t.Fatalf("moved to %+v, want nothing to move", d.Switch.Selection)
	}
	if d.Memory.Campaign != nil {
		t.Fatalf("campaign still runs at round %d, want it ended", d.Memory.Campaign.Round)
	}
}

// In a round a passed, only a ready rival can win. Recovering b beat a on the severe line in both
// rounds and ready c didn't, so nothing moves and the campaign ends.
func TestASlowRoundWhoseOnlyWinnerIsNotReadyMovesNothingAndEnds(t *testing.T) {
	wantEnded(t, slowRounds(t, pings(400, 20, 350)))
}

// The same with c slower than a.
func TestASlowRoundMovesNothingWhenItsReadyRivalIsSlowerThanTheIncumbent(t *testing.T) {
	wantEnded(t, slowRounds(t, pings(400, 20, 600)))
}

// The same with c as fast as a.
func TestASlowRoundMovesNothingWhenItsReadyRivalIsNoFasterThanTheIncumbent(t *testing.T) {
	wantEnded(t, slowRounds(t, pings(400, 20, 400)))
}

// b beats a on its reused requests' spread and is longer on both other measures, and c measures what a
// does. b is not ready, so nothing moves.
func TestASlowRoundMovesNothingWhenOnlyARecoveringRivalBeatsTheIncumbentOnSpread(t *testing.T) {
	slow := domain.Signal{Ping: ms(400), PingSpread: ms(250), NewConn: ms(1200)}
	wantEnded(t, slowRounds(t, map[string]domain.Signal{"a": slow, "b": {Ping: ms(600), PingSpread: ms(0), NewConn: ms(1800)}, "c": slow}))
}

// Loss: ready c is 2 ms longer on Ping than a and 10 times shorter on new connection. It beat a on the
// severe line in both rounds, so it takes the move.
func TestAReadyRivalBeatingTheIncumbentOnNewConnectionTakesTheMoveThoughItsPingIsLonger(t *testing.T) {
	lossy := domain.Signal{Ping: ms(100), NewConn: ms(2000)}
	d := slowRounds(t, map[string]domain.Signal{"a": lossy, "b": lossy, "c": {Ping: ms(102), NewConn: ms(200)}})
	wantSwitch(t, d, domain.SlownessMove, "c")
}

// udpCampaign starts the campaign a's broken UDP begins over decide, with b's and c's UDP working, and
// returns a step that answers its current round with what each server measured, then a step that
// proves the campaign with a slow heartbeat of a's.
func udpCampaign(t *testing.T, w *scene, pool pool) (decision, func(d decision, at time.Time, measured map[string]domain.Signal) decision, func(d decision, at time.Time) decision) {
	t.Helper()
	owner, tm := groupSlot(t), defaultTimings()
	d := decide(t0, owner, pool, policyMemory{Selection: serving("a")}, w.evidence, tm)
	if d.Memory.Campaign == nil || d.Memory.Campaign.Cause != udpFailure {
		t.Fatalf("campaign %+v, want one for broken UDP", d.Memory.Campaign)
	}
	round := func(d decision, at time.Time, measured map[string]domain.Signal) decision {
		c := d.Memory.Campaign
		var signals []domain.Signal
		for id, s := range measured {
			s.Stamp, s.Server, s.Kind, s.Purpose, s.Campaign, s.Round = domain.Stamp{At: at, Epoch: "e1"}, ref(id), domain.ActiveProbe, domain.Confirmation, c.ID, c.Round
			signals = append(signals, s)
		}
		w.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: at}, Signals: signals})
		return decide(at, owner, pool, d.Memory, w.evidence, tm)
	}
	slow := func(d decision, at time.Time) decision {
		w.ingest(signalFrame{WAN: wanBelief{State: domain.WANUp, At: at}, Signals: []domain.Signal{{Stamp: domain.Stamp{At: at, Epoch: "e1"}, Server: ref("a"), Kind: domain.ActiveProbe, Purpose: domain.Heartbeat, Verdict: pass, NewConn: ms(400)}}})
		d = decide(at, owner, pool, d.Memory, w.evidence, tm)
		if d.Memory.Campaign == nil || d.Memory.Campaign.Cause != slowHeartbeat {
			t.Fatalf("campaign %+v, want a slow heartbeat to prove it", d.Memory.Campaign)
		}
		return d
	}
	return d, round, slow
}

// Recovering b passes two rounds of a's broken-UDP campaign beside it, and its own UDP breaks before the
// second, so neither moves to it. A slow heartbeat proves the campaign. In the next round b is far
// slower than a: a move for slowness counts only wins on the severe line, b has none, so nothing moves.
func TestWinsBrokenUDPGaveTakeNoMoveForSlownessAfterASlowHeartbeat(t *testing.T) {
	w := newScene(domain.WANUp).healthy("a", 40).probe("b", time.Second, fail, 0).probe("c", time.Second, fail, 0).
		checked("a", brokenChecks()...).checked("b", pass).checked("c", pass)
	d, round, slow := udpCampaign(t, w, poolOf(lin("a"), lin("b"), lin("c")))
	alike := map[string]domain.Signal{"a": {Verdict: pass, Ping: ms(40)}, "b": {Verdict: pass, Ping: ms(40)}, "c": {Verdict: nothing}}
	d = round(d, t0.Add(5*time.Second), alike)
	w.checked("b", brokenChecks()...)
	d = round(d, t0.Add(10*time.Second), alike)
	d = slow(d, t0.Add(11*time.Second))
	d = round(d, t0.Add(15*time.Second), map[string]domain.Signal{"a": {Verdict: pass, Ping: ms(40)}, "b": {Verdict: pass, Ping: ms(600)}, "c": {Verdict: nothing}})
	if w.book.records["b"].standing() != recovering {
		t.Fatalf("b stands %d, want recovering", w.book.records["b"].standing())
	}
	if d.Switch != nil {
		t.Fatalf("moved to %+v for reason %d, want nothing to move", d.Switch.Selection, d.Switch.Reason)
	}
}

// Ready b wins one round of a's broken-UDP campaign on liveness alone, then a slow heartbeat proves it.
// Its first win on the severe line is not a second: the move for slowness waits for the next one.
func TestAWinBrokenUDPGaveDoesNotCountTowardAMoveForSlowness(t *testing.T) {
	w := newScene(domain.WANUp).healthy("a", 40).healthy("b", 40).checked("a", brokenChecks()...).checked("b", pass)
	d, round, slow := udpCampaign(t, w, poolOf(lin("a"), lin("b")))
	d = round(d, t0.Add(5*time.Second), map[string]domain.Signal{"a": {Verdict: pass, Ping: ms(40)}, "b": {Verdict: pass, Ping: ms(40)}})
	if d.Memory.Campaign == nil || d.Memory.Campaign.Wins[0].All != 1 {
		t.Fatalf("campaign %+v, want b to have won the UDP round", d.Memory.Campaign)
	}
	d = slow(d, t0.Add(6*time.Second))
	faster := map[string]domain.Signal{"a": {Verdict: pass, Ping: ms(400)}, "b": {Verdict: pass, Ping: ms(20)}}
	if d = round(d, t0.Add(10*time.Second), faster); d.Switch != nil {
		t.Fatalf("moved to %+v for reason %d after one win on the severe line", d.Switch.Selection, d.Switch.Reason)
	}
	wantSwitch(t, round(d, t0.Add(15*time.Second), faster), domain.SlownessMove, "b")
}
