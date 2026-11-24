package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

// pressAnswer is what a press's route answered, and the view the page could read as it was written.
type pressAnswer struct {
	status   int
	body     string
	revision string
	view     View
}

// answerRecorder keeps the view the page could read as the answer was written.
type answerRecorder struct {
	*httptest.ResponseRecorder
	app  *App
	view View
}

func (a *answerRecorder) WriteHeader(code int) {
	a.view = a.app.Snapshot()
	a.ResponseRecorder.WriteHeader(code)
}

func (a pressAnswer) verdict(t *testing.T) string {
	t.Helper()
	var result ProbeResult
	if err := json.Unmarshal([]byte(a.body), &result); err != nil {
		t.Fatalf("%d %s: %v", a.status, a.body, err)
	}
	return result.Verdict
}

func (a pressAnswer) refusal(t *testing.T) Reason {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal([]byte(a.body), &body); err != nil {
		t.Fatalf("%d %s: %v", a.status, a.body, err)
	}
	return body.Reason
}

// send writes through the page's handler, as nginx hands it a request. The rig plays the signal loop,
// answering a Test with test, and hands the controller each write as its loop does.
func (r *rig) send(method, path, key string, test func(domain.ServerRef) domain.Signal) pressAnswer {
	r.t.Helper()
	a, stopped := r.sendStopping(method, path, key, test)
	if stopped != nil {
		r.t.Fatal(stopped)
	}
	return a
}

// sendStopping is send, and the error a turn of the write stopped the daemon with.
func (r *rig) sendStopping(method, path, key string, test func(domain.ServerRef) domain.Signal) (pressAnswer, error) {
	r.t.Helper()
	rec, done := r.request(method, path, key)
	var stopped error
	for {
		select {
		case m := <-r.app.manual:
			m.Reply <- test(m.Server)
		case e := <-r.app.commands:
			stopped = r.c.take(context.Background(), e)
		case <-done:
			return rec.answer(), stopped
		}
	}
}

// request hands the page's handler a write, as nginx does, and closes done once its caller is answered.
func (r *rig) request(method, path, key string) (*answerRecorder, <-chan struct{}) {
	if r.app.commands == nil {
		r.app.proof, r.app.bootstrap.BasePath = []byte("proof"), DefaultBasePath
		r.app.commands, r.app.manual = make(chan envelope), make(chan manualProbe)
		r.page = r.app.handler(nil, func() string { return "token" }, nil)
	}
	req := httptest.NewRequest(method, DefaultBasePath+path, nil)
	req.Header.Set(AuthHeader, "200")
	req.Header.Set(ProofHeader, "proof")
	req.Header.Set(CSRFHeader, "token")
	req.Header.Set("Idempotency-Key", key)
	rec := &answerRecorder{ResponseRecorder: httptest.NewRecorder(), app: r.app}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.page.ServeHTTP(rec, req)
	}()
	return rec, done
}

func (a *answerRecorder) answer() pressAnswer {
	return pressAnswer{a.Code, a.Body.String(), a.Header().Get(RevisionHeader), a.view}
}

// test is a Test's answer from a server, at its ping when it passes, begun now.
func (r *rig) test(verdict domain.Verdict, ms int) func(domain.ServerRef) domain.Signal {
	return func(ref domain.ServerRef) domain.Signal {
		s := domain.Signal{Stamp: domain.Stamp{Epoch: r.c.evidence.epoch, At: r.clock}, Server: ref, Kind: domain.ActiveProbe, Verdict: verdict,
			Purpose: domain.ManualProbe, Target: "t1"}
		if verdict == domain.Succeeded && ms > 0 {
			d := time.Duration(ms) * time.Millisecond
			s.Ping, s.NewConn = &d, &d
		}
		return s
	}
}

func (o preferOwner) route() string {
	if id, ok := o.owner.Group(); ok {
		return "api/groups/" + string(id)
	}
	id, _ := o.owner.Source()
	return "api/sources/" + string(id)
}

// press presses server id on the owner, its Test answering with test.
func (r *preferRig) press(id domain.LineageID, key string, test func(domain.ServerRef) domain.Signal) pressAnswer {
	r.t.Helper()
	return r.send(http.MethodPost, r.o.route()+"/preferred/"+string(id), key, test)
}

// passes presses id, and its Test passes at ms.
func (r *preferRig) passes(id domain.LineageID, ms int) pressAnswer {
	r.t.Helper()
	a := r.press(id, "press-"+string(id)+"-"+r.clock.Format(time.RFC3339Nano), r.test(domain.Succeeded, ms))
	if a.status != http.StatusOK || a.verdict(r.t) != "succeeded" {
		r.t.Fatalf("a press on %s answered %d %s", id, a.status, a.body)
	}
	return a
}

func wantPreferred(t *testing.T, r *rig, id domain.LineageID, size float64) {
	t.Helper()
	p := r.preferred()
	switch {
	case id == "" && p != nil:
		t.Errorf("prefers %+v, want nothing held", *p)
	case id == "":
	case p == nil:
		t.Errorf("prefers nothing, want %s", id)
	case p.Server != id || size > 0 && !near(p.Size, size):
		t.Errorf("prefers %+v, want %s by %.3f", *p, id, size)
	}
}

// storedBoost is the boost of a queued or held pick as state.db holds it, past its deadline too.
func storedBoost(h handPick) *preference {
	switch at := h.at.(type) {
	case queuedPick:
		if at.boost.Size > 0 {
			return &preference{Server: at.move.Lineage, boost: at.boost}
		}
	case heldPick:
		return &preference{Server: at.server, boost: at.boost}
	}
	return nil
}

// pickState names where the pick stands.
func pickState(h handPick) string {
	switch h.at.(type) {
	case queuedPick:
		return "queued"
	case heldPick:
		return "held"
	}
	return "none"
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestAFailedCheckMovesNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		before := len(r.switches())
		first := r.press("c", "failed", r.test(domain.Failed, 0))
		if first.status != http.StatusOK || first.verdict(t) != "failed" {
			t.Fatalf("a failed check answered %d %s", first.status, first.body)
		}
		if got := r.server("eu"); got != "a" || len(r.switches()) != before {
			t.Errorf("a failed check moved eu to %s", got)
		}
		if m := r.memory("eu"); pickState(m.Hand) != "none" {
			t.Errorf("a failed check left the pick %+v", m.Hand)
		}
		again := r.press("c", "failed", r.test(domain.Succeeded, 30))
		if again.body != first.body || len(r.switches()) != before {
			t.Errorf("a resend under the key answered %s, want the first answer %s", again.body, first.body)
		}
	})
}

func TestANotRunCheckMovesNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		a := r.press("c", "unrun", r.test(domain.Unknown, 0))
		if a.status != http.StatusOK || a.verdict(t) != "unknown" {
			t.Fatalf("a check with no route answered %d %s", a.status, a.body)
		}
		if got := r.server("eu"); got != "a" || pickState(r.memory("eu").Hand) == "queued" {
			t.Errorf("a check that didn't run moved eu to %s", got)
		}
	})
}

func TestAPressOnAnOwnerNothingFollowsIsRefused(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.setExit("eu", domain.ServerExit("a"))
		r.settle()
		a := r.press("c", "idle", r.test(domain.Succeeded, 30))
		if a.status != http.StatusUnprocessableEntity || a.refusal(t) != RefusedOwnerIdle {
			t.Errorf("a press on an owner nothing follows answered %d %s", a.status, a.body)
		}
	})
}

func TestAPressOnAServerTheOwnerDoesntHoldIsRefused(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.seed(lineage(t, "x", "other", "DE", "198.51.100.99"))
		r.c.publish(r.clock)
		if a := r.press("x", "other", r.test(domain.Succeeded, 30)); a.status != http.StatusUnprocessableEntity || a.refusal(t) != RefusedNotMember {
			t.Errorf("a press on another source's server answered %d %s", a.status, a.body)
		}
	})
}

func TestAPressWhoseServerChangedDuringItsCheckIsRefused(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		a := r.press("c", "rotated", func(ref domain.ServerRef) domain.Signal {
			rotated, _ := r.c.world.catalogue.Lookup("c")
			rotated.Ref.Digest[5] = 0xee
			r.storeLineages(transaction{Lineages: []domain.Lineage{rotated}})
			return r.test(domain.Succeeded, 30)(ref)
		})
		if a.status != http.StatusUnprocessableEntity || a.refusal(t) != RefusedServerChanged {
			t.Fatalf("a press whose server changed during its check answered %d %s", a.status, a.body)
		}
		if got := r.server("eu"); got != "a" {
			t.Errorf("the refused press moved eu to %s", got)
		}
	})
}

func TestAPressWhoseCoreRestartedDuringItsCheckIsRefused(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		a := r.press("c", "restarted", func(ref domain.ServerRef) domain.Signal {
			s := r.test(domain.Succeeded, 30)(ref)
			r.c.evidence.epoch = "e2"
			return s
		})
		if a.status != http.StatusUnprocessableEntity || a.refusal(t) != RefusedServerChanged {
			t.Errorf("a press whose core restarted during its check answered %d %s", a.status, a.body)
		}
	})
}

func TestAPressWhoseServerFailedSinceItsCheckIsRefused(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		a := r.press("c", "failed-since", func(ref domain.ServerRef) domain.Signal {
			s := r.test(domain.Succeeded, 30)(ref)
			r.clock = r.clock.Add(time.Second)
			r.frame(domain.WANUp, r.check("c", domain.Failed, 0))
			return s
		})
		if a.status != http.StatusUnprocessableEntity || a.refusal(t) != RefusedFailedSince {
			t.Errorf("a press on a server that failed during its check answered %d %s", a.status, a.body)
		}
	})
}

func TestOnePressMovesEverySlotOfTheOwner(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.putSlot("home", exitIntent(t, "Home", o.exit))
		r.settle()
		r.reportApplied("home")
		r.passes("c", 30)
		events := r.switches()
		if got := events[0]; got.Reason != "preferred" || len(got.Slots) != 2 || got.To.Server == nil || *got.To.Server != "c" {
			t.Fatalf("the press's move: %+v", got)
		}
		if r.server("eu") != "c" || r.server("home") != "c" {
			t.Errorf("eu on %s and home on %s, want both on c", r.server("eu"), r.server("home"))
		}
	})
}

func TestPreferringTheBestScoredServerMovesAndHoldsNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		r.passes("a", 10)
		if got := r.server("eu"); got != "a" || r.lastReason() != "preferred" {
			t.Fatalf("serving %s after %s, want a by hand", got, r.lastReason())
		}
		wantPreferred(t, r.rig, "", 0)
	})
}

func TestAZeroGapPressMovesThroughEvaluate(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		before := len(r.switches())
		r.passes("a", 10)
		if events := r.switches(); len(events) != before+1 || events[0].Reason != "preferred" || len(events[0].Slots) != 1 {
			t.Fatalf("a press with no gap made %d switches, newest %+v", len(events)-before, events[0])
		}
		if m := r.memory("eu"); pickState(m.Hand) == "queued" {
			t.Errorf("the evaluate that moved left the pick %+v", m.Hand)
		}
	})
}

func TestAReplacingPreferenceMeasuresItsGapAtRawPrices(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("b", 20)
		wantPreferred(t, r.rig, "b", r.priceOf("b")-r.priceOf("a"))
		r.reportApplied("eu")
		r.clock = r.clock.Add(time.Minute)
		r.passes("c", 30)
		wantPreferred(t, r.rig, "c", r.priceOf("c")-r.priceOf("a"))
	})
}

func TestAnUnpricedMemberDoesNotSetTheGap(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.seed(lineage(t, "d", "good", "DE", "198.51.100.40"))
		r.frame(domain.WANUp, r.probe("d", domain.Succeeded, 5), r.check("a", domain.Failed, 0), r.check("b", domain.Failed, 0))
		r.clock = r.clock.Add(time.Minute)
		r.frame(domain.WANUp, r.check("a", domain.Succeeded, 10), r.check("b", domain.Succeeded, 20))
		wantOrder(t, r.order(), "c", "d", "a", "b")
		r.frame(domain.WANUp, r.check("c", domain.Failed, 0))
		r.clock = r.clock.Add(time.Minute)
		r.passes("c", 30)
		if got := r.server("eu"); got != "c" {
			t.Fatalf("serving %s, want c", got)
		}
		wantPreferred(t, r.rig, "", 0)
	})
}

func TestAPassWithNoSampleMovesOnceAndHoldsNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.seed(lineage(t, "d", "good", "DE", "198.51.100.40"))
		r.c.publish(r.clock)
		before := len(r.switches())
		r.passes("d", 0)
		r.settle()
		if got := r.server("eu"); got != "d" || len(r.switches()) != before+1 {
			t.Fatalf("serving %s after %d switches, want d after one", got, len(r.switches())-before)
		}
		wantPreferred(t, r.rig, "", 0)
	})
}

func TestAPickRankedFirstByClassHoldsNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.frame(domain.WANUp, r.check("a", domain.Failed, 0), r.check("b", domain.Failed, 0))
		r.clock = r.clock.Add(time.Minute)
		r.frame(domain.WANUp, r.check("a", domain.Succeeded, 10), r.check("b", domain.Succeeded, 20))
		r.passes("c", 30)
		if got := r.server("eu"); got != "c" {
			t.Fatalf("serving %s, want c", got)
		}
		wantPreferred(t, r.rig, "", 0)
	})
}

func TestAPressOnTheSelectedServerPrefersItAndMovesNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRigOn(t, o, "b", slowB)
		before := len(r.switches())
		r.passes("b", 40)
		if got := r.server("eu"); got != "b" || len(r.switches()) != before {
			t.Fatalf("a press on the server in use moved eu to %s", got)
		}
		wantPreferred(t, r.rig, "b", r.priceOf("b")-r.priceOf("a"))
		if m := r.memory("eu"); pickState(m.Hand) == "queued" {
			t.Errorf("the pick outlived its evaluate: %+v", m.Hand)
		}
	})
}

func TestAPressOnTheSelectedBestServerHoldsNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		before := len(r.switches())
		r.passes("a", 10)
		if got := r.server("eu"); got != "a" || len(r.switches()) != before {
			t.Fatalf("a press on the best server in use moved eu to %s", got)
		}
		wantPreferred(t, r.rig, "", 0)
	})
}

func TestASecondPressOnThePreferredServerKeepsItsDeadline(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("c", 30)
		was := *r.preferred()
		r.reportApplied("eu")
		r.clock = r.clock.Add(time.Hour)
		r.passes("c", 30)
		if p := r.preferred(); p == nil || *p != was {
			t.Errorf("a second press on c prefers %+v, want %+v", p, was)
		}
	})
}

func TestAPressJustPastTheDeadlineStartsANewPreference(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("b", 20)
		r.reportApplied("eu")
		ends := r.preferred().endsAt()
		r.clock = ends.Add(-time.Second)
		r.frame(domain.WANUp)
		r.clock = ends.Add(time.Second)
		r.passes("b", 20)
		if p := r.preferred(); p == nil || p.Server != "b" || !p.At.Equal(r.clock) {
			t.Errorf("a press a second past the deadline prefers %+v, want b from now", p)
		}
	})
}

func TestAPressThatBeganBeforeALaterPressLandedMovesNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		older := r.press("b", "older", func(ref domain.ServerRef) domain.Signal {
			sig := r.test(domain.Succeeded, 20)(ref)
			r.clock = r.clock.Add(time.Second)
			r.passes("c", 30)
			r.reportApplied("eu")
			r.clock = r.clock.Add(time.Second)
			return sig
		})
		if older.status != http.StatusPreconditionFailed || older.refusal(t) != RefusedStale {
			t.Errorf("the older press answered %d %s, want 412 stale", older.status, older.body)
		}
		r.reportApplied("eu")
		if got := r.server("eu"); got != "c" {
			t.Errorf("serving %s, want c, the later press", got)
		}
		wantPreferred(t, r.rig, "c", 0)
	})
}

func TestAPressThatBeganBeforeAClearMovesNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("b", 20)
		r.reportApplied("eu")
		ends := r.preferred().endsAt()
		r.clock = r.clock.Add(time.Minute)
		older := r.press("b", "repeat", func(ref domain.ServerRef) domain.Signal {
			sig := r.test(domain.Succeeded, 20)(ref)
			r.clock = r.clock.Add(time.Second)
			if a := r.clear("b", ends, "clear"); a.status != http.StatusOK {
				t.Fatalf("Clear answered %d %s", a.status, a.body)
			}
			return sig
		})
		if older.status != http.StatusPreconditionFailed || older.refusal(t) != RefusedStale {
			t.Errorf("the press begun before Clear answered %d %s, want 412 stale", older.status, older.body)
		}
		wantPreferred(t, r.rig, "", 0)
	})
}

func TestAPressThatBeganAfterAnotherLandedStillMoves(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		later := r.press("c", "later", func(ref domain.ServerRef) domain.Signal {
			r.passes("b", 20)
			r.clock = r.clock.Add(time.Second)
			return r.test(domain.Succeeded, 30)(ref)
		})
		if later.status != http.StatusOK {
			t.Fatalf("a press begun after b's landed answered %d %s", later.status, later.body)
		}
		if got := r.server("eu"); got != "c" {
			t.Errorf("serving %s, want c", got)
		}
	})
}

func TestAClearThatClearedNothingLeavesAPressBegunBeforeItToMove(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("b", 20)
		r.reportApplied("eu")
		shown := r.preferred().endsAt()
		r.clock = r.clock.Add(time.Minute)
		r.passes("c", 30)
		r.reportApplied("eu")
		r.clock = r.clock.Add(time.Minute)
		again := r.press("b", "again", func(ref domain.ServerRef) domain.Signal {
			sig := r.test(domain.Succeeded, 20)(ref)
			r.clock = r.clock.Add(time.Second)
			if a := r.clear("b", shown, "stale-clear"); a.status != http.StatusOK {
				t.Fatalf("a stale Clear answered %d %s", a.status, a.body)
			}
			wantPreferred(t, r.rig, "c", 0)
			return sig
		})
		if again.status != http.StatusOK {
			t.Fatalf("a press begun before a Clear that cleared nothing answered %d %s", again.status, again.body)
		}
		if got := r.server("eu"); got != "b" {
			t.Errorf("serving %s, want b", got)
		}
	})
}

func TestAPressOnAnotherServerReplacesThePreference(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("c", 30)
		r.reportApplied("eu")
		r.clock = r.clock.Add(time.Hour)
		r.passes("b", 20)
		if got := r.server("eu"); got != "b" {
			t.Fatalf("serving %s, want b", got)
		}
		if p := r.preferred(); p == nil || p.Server != "b" || !p.At.Equal(r.clock) {
			t.Errorf("prefers %+v, want b from now", p)
		}
	})
}

func TestASlotThatJoinedDuringTheCheckFollowsThePick(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		a := r.press("c", "joined", func(ref domain.ServerRef) domain.Signal {
			r.putSlot("home", exitIntent(t, "Home", o.exit))
			r.settle()
			return r.test(domain.Succeeded, 30)(ref)
		})
		if a.status != http.StatusOK {
			t.Fatalf("the press answered %d %s", a.status, a.body)
		}
		if r.server("eu") != "c" || r.server("home") != "c" {
			t.Errorf("eu on %s and home on %s, want both on c", r.server("eu"), r.server("home"))
		}
	})
}

func TestAPickIsProposedOnceAndNeverSurvivesItsEvaluate(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		before := len(r.switches())
		r.passes("c", 30)
		r.settle()
		r.reportApplied("eu")
		r.settle()
		if got := len(r.switches()) - before; got != 1 {
			t.Errorf("one press made %d switches", got)
		}
		r.close()
		again := newRigAt(t, r.dir, r.clock)
		if h := again.c.state.owners[o.owner].Hand; pickState(h) == "queued" {
			t.Errorf("state.db keeps the pick %+v after its evaluate", h)
		}
	})
}

func TestAPickOntoAServerWhoseLastHeartbeatFailedStartsNoCampaign(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.frame(domain.WANUp, r.probe("c", domain.Failed, 0))
		r.clock = r.clock.Add(time.Second)
		r.passes("c", 30)
		r.frame(domain.WANUp)
		if got := r.server("eu"); got != "c" {
			t.Fatalf("serving %s, want c", got)
		}
		if c := r.memory("eu").Campaign; c != nil && c.Path == failurePath {
			t.Errorf("the pick's old failed heartbeat started a campaign against it: %+v", c)
		}
	})
}

func (r *rig) udpCheck(id string, verdict domain.Verdict) domain.Signal {
	s := r.probe(id, verdict, 0)
	s.Purpose = domain.UDPCheck
	return s
}

func TestAPickOntoAServerWhoseUDPStandsBrokenLeavesItForUDP(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.putSlot("fixed-c", exitIntent(t, "Fixed c", domain.ServerExit("c")))
		r.frame(domain.WANUp)
		r.reportApplied("fixed-c")
		for range udpBrokenAfter {
			r.clock = r.clock.Add(r.c.timings.UDPCheckEvery)
			r.frame(domain.WANUp, r.udpCheck("a", domain.Succeeded), r.udpCheck("c", domain.Failed))
		}
		r.clock = r.clock.Add(time.Second)
		r.passes("c", 30)
		if got := r.server("eu"); got != "c" {
			t.Fatalf("serving %s, want c, the pick", got)
		}
		c := r.memory("eu").Campaign
		if c == nil || c.Cause != udpFailure || c.Incumbent.Lineage != "c" {
			t.Fatalf("no UDP confirmation began on the pick whose UDP stands broken: %+v", c)
		}
		for range failureRounds {
			c := r.memory("eu").Campaign
			if c == nil || c.Cause != udpFailure {
				t.Fatalf("the UDP confirmation ended early: %+v", c)
			}
			var answers []domain.Signal
			for _, ref := range append([]domain.ServerRef{c.Incumbent}, c.Rivals...) {
				s := r.check(string(ref.Lineage), domain.Succeeded, 20)
				s.Purpose, s.Campaign, s.Round = domain.Confirmation, c.ID, c.Round
				answers = append(answers, s)
			}
			r.frame(domain.WANUp, answers...)
			r.clock = r.clock.Add(r.c.timings.FailureSpacing)
		}
		if got := r.server("eu"); got != "a" || r.lastReason() != "udpMove" {
			t.Errorf("serving %s after %s, want a for broken UDP, which the boost doesn't hold back", got, r.lastReason())
		}
	})
}

func TestAPickOntoBrokenUDPBeginsItsConfirmationOnceTheWANIsProvenAfterIt(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.putSlot("fixed-c", exitIntent(t, "Fixed c", domain.ServerExit("c")))
		r.frame(domain.WANUp)
		r.reportApplied("eu", "fixed-c")
		for range udpBrokenAfter {
			r.clock = r.clock.Add(r.c.timings.UDPCheckEvery)
			r.frame(domain.WANUp, r.udpCheck("a", domain.Succeeded), r.udpCheck("c", domain.Failed))
		}
		r.clock = r.clock.Add(r.c.timings.WANTTL + time.Second)
		pass := r.test(domain.Succeeded, 30)(r.ref("c"))
		if a := r.press("c", "unproven", func(domain.ServerRef) domain.Signal { return pass }); a.status != http.StatusOK {
			t.Fatalf("the press answered %d %s", a.status, a.body)
		}
		if got := r.server("eu"); got != "c" {
			t.Fatalf("serving %s, want c, the pick", got)
		}
		r.frame(domain.WANUp, pass)
		if c := r.memory("eu").Campaign; c == nil || c.Cause != udpFailure || c.Incumbent.Lineage != "c" {
			t.Errorf("no UDP confirmation began on the pick once the WAN was proven: %+v", c)
		}
	})
}

// closedRig is eu closed on a, b and c, every one failing, and d added since, never probed.
func closedRig(t *testing.T, o preferOwner) *preferRig {
	t.Helper()
	r := newPreferRig(t, o)
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0))
	for range failureRounds {
		r.failoverRound(nil)
	}
	if reason, down := r.memory("eu").Selection.Down(); !down || reason != domain.ConfirmedDead {
		t.Fatalf("selection %+v, want closed", r.memory("eu").Selection)
	}
	r.reportApplied("eu")
	r.seed(lineage(t, "d", "good", "DE", "198.51.100.40"))
	r.settle()
	return r
}

func TestAPressReopensAClosedSourceOnTheFailingServerItPicked(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := closedRig(t, o)
		if r.c.evidence.book.records["c"].standing() != failing {
			t.Fatal("c isn't failing")
		}
		r.passes("c", 30)
		if got := r.server("eu"); got != "c" || r.lastReason() != "preferred" {
			t.Fatalf("serving %s after %s, want c reopened by hand", got, r.lastReason())
		}
		if r.c.evidence.book.records["c"].standing() != recovering {
			t.Errorf("c stands %d after its one pass, want recovering", r.c.evidence.book.records["c"].standing())
		}
	})
}

func TestAPressReopensAClosedGroupInsideItsBackOff(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := closedRig(t, o)
		if m := r.memory("eu"); !m.Backoff.holdsOut(r.clock) {
			t.Fatalf("no back-off runs: %v", m.Backoff.NotBefore)
		}
		r.passes("a", 10)
		if got := r.server("eu"); got != "a" {
			t.Errorf("serving %s inside the back-off, want a, the pick", got)
		}
	})
}

func TestAPickOnAClosedOwnerWhoseOthersAllFailHoldsNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := closedRig(t, o)
		r.passes("b", 20)
		if got := r.server("eu"); got != "b" {
			t.Fatalf("serving %s, want b", got)
		}
		wantPreferred(t, r.rig, "", 0)
	})
}

func TestAClosureAfterAPickedReopenStillDoublesTheBackOff(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := closedRig(t, o)
		r.passes("c", 30)
		r.reportApplied("eu")
		r.c.timings.RecoverStable = 2 * time.Minute
		r.clock = r.clock.Add(time.Minute)
		r.frame(domain.WANUp, r.probe("c", domain.Failed, 0))
		for range failureRounds {
			r.failoverRound(nil)
		}
		r.reportApplied("eu")
		closed := r.clock
		for range recoverPasses {
			r.clock = r.clock.Add(recoverSpacing)
			r.frame(domain.WANUp, r.check("c", domain.Succeeded, 30))
		}
		r.clock = closed.Add(r.c.timings.RecoverStable + time.Minute)
		r.frame(domain.WANUp)
		if got := r.server("eu"); got != "" {
			t.Fatalf("reopened on %s past one back-off, inside the doubled one", got)
		}
		r.clock = closed.Add(2 * r.c.timings.RecoverStable)
		r.frame(domain.WANUp)
		if got := r.server("eu"); got != "c" {
			t.Errorf("serving %q once the doubled back-off ran out, want c", got)
		}
	})
}

func TestAPickCommittedBeforeACrashMovesAfterTheRestart(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := closedRig(t, o)
		sig := r.test(domain.Succeeded, 30)(r.ref("c"))
		if _, err := r.c.acceptPage(context.Background(), pageWrite{Key: "crash", Fingerprint: []byte("crash"), Decision: runs{PreferServer{Owner: o.owner, Signal: sig}}}); err != nil {
			t.Fatal(err)
		}
		r.close()
		again := newRigAt(t, r.dir, r.clock.Add(time.Second))
		if got := again.c.evidence.book.records["c"].standing(); got != recovering {
			t.Errorf("c stands %d after the restart, want recovering on its press's pass", got)
		}
		again.settle()
		if got := again.server("eu"); got != "c" {
			t.Errorf("serving %s after the restart, want c, the pick", got)
		}
	})
}

func TestAPressReadsItsOwnPassAndTheFrameAddsNoSecondSample(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		var sig domain.Signal
		r.press("c", "own", func(ref domain.ServerRef) domain.Signal {
			sig = r.test(domain.Succeeded, 25)(ref)
			return sig
		})
		r.frame(domain.WANUp, sig)
		wantSamplesAt(t, r.rig, "c", sig.Stamp.At, 1)
	})
}

func TestAFrameBeforeThePressAddsNoSecondSample(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.press("c", "frame-first", func(ref domain.ServerRef) domain.Signal {
			sig := r.test(domain.Succeeded, 25)(ref)
			r.frame(domain.WANUp, sig)
			return sig
		})
		wantSamplesAt(t, r.rig, "c", r.clock, 1)
		if got := r.server("eu"); got != "c" {
			t.Errorf("serving %s, want c", got)
		}
	})
}

func TestAPressWhoseFrameWasDroppedStillCountsItsSample(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		at := r.clock
		r.passes("c", 25)
		wantSamplesAt(t, r.rig, "c", at, 1)
		r.close()
		again := newRigAt(t, r.dir, r.clock)
		wantSamplesAt(t, again, "c", at, 1)
	})
}

func TestADistinctSampleAtAnotherInstantStillCounts(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		at := r.clock
		r.passes("c", 25)
		r.frame(domain.WANUp, r.test(domain.Succeeded, 25)(r.ref("c")))
		r.clock = r.clock.Add(time.Second)
		r.frame(domain.WANUp, r.test(domain.Succeeded, 25)(r.ref("c")))
		wantSamplesAt(t, r.rig, "c", at, 1)
		wantSamplesAt(t, r.rig, "c", r.clock, 1)
	})
}

func wantSamplesAt(t *testing.T, r *rig, id domain.LineageID, at time.Time, want int) {
	t.Helper()
	n := 0
	for _, s := range r.c.evidence.book.records[id].Samples {
		if s.At.Equal(at) {
			n++
		}
	}
	if n != want {
		t.Errorf("%s holds %d samples from %v, want %d", id, n, at, want)
	}
}

// leftByForce is eu on b, where a failover off a put it, with a kept out.
func leftByForce(t *testing.T, o preferOwner, shown bool) *preferRig {
	t.Helper()
	r := newPreferRig(t, o)
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0))
	for range failureRounds {
		r.failoverRound(map[domain.LineageID]bool{"b": true})
	}
	if got := r.server("eu"); got != "b" {
		t.Fatalf("failed over to %s, want b", got)
	}
	if shown {
		r.reportApplied("eu")
	}
	return r
}

func TestAPreferenceOnAServerAnUnshownFailureLeftIsNotKeptOut(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := leftByForce(t, o, false)
		r.clock = r.clock.Add(time.Second)
		r.passes("a", 10)
		if got := r.server("eu"); got != "a" {
			t.Fatalf("serving %s, want a", got)
		}
		r.reportApplied("eu")
		if until := r.memory("eu").KeepOut["a"]; r.clock.Before(until) {
			t.Errorf("a, picked by hand, is kept out until %v", until)
		}
	})
}

func TestAPressDuringALandingFailoverKeepsTheOtherKeepOut(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := leftByForce(t, o, false)
		r.clock = r.clock.Add(time.Second)
		r.passes("c", 30)
		if got := r.server("eu"); got != "c" {
			t.Fatalf("serving %s, want c", got)
		}
		m := r.memory("eu")
		if until := m.KeepOut["a"]; !r.clock.Before(until) {
			t.Errorf("the failover's keep-out on a went with the press on c: %v", m.KeepOut)
		}
		if _, out := m.KeepOut["c"]; out {
			t.Errorf("c, picked by hand, is kept out: %v", m.KeepOut)
		}
	})
}

func TestPreferringAKeptOutServerInUseListsItByItsPrice(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := leftByForce(t, o, true)
		r.frame(domain.WANUp, r.probe("b", domain.Failed, 0))
		for range failureRounds {
			r.failoverRound(map[domain.LineageID]bool{"a": true})
		}
		if got := r.server("eu"); got != "a" {
			t.Fatalf("failed back to %s, want a, kept out but the last that works", got)
		}
		r.reportApplied("eu")
		if order := r.order(); slices.Index(order, "a") < slices.Index(order, "c") {
			t.Fatalf("order %v: a, kept out, ranks over c, which failed", order)
		}
		r.passes("a", 10)
		if order := r.order(); slices.Index(order, "a") > slices.Index(order, "c") {
			t.Errorf("order %v after a press on a, want a by its class and price over c", order)
		}
	})
}

func TestTheRoutesAreTheGroupsAndTheSources(t *testing.T) {
	r := newPreferRig(t, preferOwners[0])
	for _, path := range []string{"api/slots/eu/preferred/c", "api/groups/all-good/preferred"} {
		if a := r.send(http.MethodPost, path, "nowhere-"+strings.ReplaceAll(path, "/", "-"), r.test(domain.Succeeded, 30)); a.status != http.StatusNotFound {
			t.Errorf("POST %s answered %d, want no route", path, a.status)
		}
	}
}

// clear sends the page's Clear of the preference it showed: server's, ending at endsAt.
func (r *preferRig) clear(id domain.LineageID, endsAt time.Time, key string) pressAnswer {
	r.t.Helper()
	path := r.o.route() + "/preferred/" + string(id) + "?endsAt=" + url.QueryEscape(endsAt.Format(time.RFC3339Nano))
	return r.send(http.MethodDelete, path, key, nil)
}

func TestClearMovesNothing(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("c", 30)
		r.reportApplied("eu")
		wantOrder(t, r.order(), "c", "a", "b")
		before := len(r.switches())
		if a := r.clear("c", r.preferred().endsAt(), "clear"); a.status != http.StatusOK {
			t.Fatalf("Clear answered %d %s", a.status, a.body)
		}
		wantPreferred(t, r.rig, "", 0)
		if got := r.server("eu"); got != "c" || len(r.switches()) != before {
			t.Errorf("Clear moved eu to %s", got)
		}
		wantOrder(t, r.order(), "a", "b", "c")
	})
}

func TestAStaleClearLeavesANewerPreference(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("b", 20)
		shown := r.preferred().endsAt()
		r.reportApplied("eu")
		r.clock = r.clock.Add(time.Minute)
		r.passes("c", 30)
		newer := *r.preferred()
		first := r.clear("b", shown, "stale")
		if first.status != http.StatusOK {
			t.Fatalf("a stale Clear answered %d %s", first.status, first.body)
		}
		if again := r.clear("c", newer.endsAt().Add(-time.Minute), "wrong-deadline"); again.status != http.StatusOK {
			t.Fatalf("a Clear of another deadline answered %d %s", again.status, again.body)
		}
		if p := r.preferred(); p == nil || *p != newer {
			t.Errorf("a stale Clear left %+v, want %+v", p, newer)
		}
		if replay := r.clear("b", shown, "stale"); replay.body != first.body || replay.revision != first.revision {
			t.Errorf("a resend of the stale Clear answered %s at %s, want the first answer at %s", replay.body, replay.revision, first.revision)
		}
	})
}

func TestAClearWithoutItsDeadlineIsRefused(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("c", 30)
		a := r.send(http.MethodDelete, o.route()+"/preferred/c", "no-deadline", nil)
		if a.status != http.StatusBadRequest || a.refusal(t) != RefusedBadQuery {
			t.Errorf("a Clear with no endsAt answered %d %s", a.status, a.body)
		}
		wantPreferred(t, r.rig, "c", 0)
	})
}

func TestAClearAnExpiryAndAConsumedPickAreWritten(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		stored := func(r *preferRig) policyMemory {
			r.close()
			return newRigAt(t, r.dir, r.clock).c.state.owners[o.owner]
		}
		cleared := newPreferRig(t, o)
		cleared.passes("c", 30)
		cleared.clear("c", cleared.preferred().endsAt(), "clear")
		if p := storedBoost(stored(cleared).Hand); p != nil {
			t.Errorf("a Clear wasn't written: a restart prefers %+v", p)
		}
		expired := newPreferRig(t, o)
		expired.passes("c", 30)
		expired.reportApplied("eu")
		expired.clock = expired.clock.Add(preferFor)
		expired.frame(domain.WANUp)
		if p := storedBoost(stored(expired).Hand); p != nil {
			t.Errorf("an expiry wasn't written: a restart prefers %+v", p)
		}
		consumed := newPreferRigOn(t, o, "b", slowB)
		consumed.passes("b", 40)
		if h := stored(consumed).Hand; pickState(h) != "held" {
			t.Errorf("a pick of the server in use wasn't written as consumed: %+v", h)
		}
	})
}

func TestAClearUnderAKeyThatClearedAnotherDeadlineIsRefused(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("c", 30)
		ends := r.preferred().endsAt()
		r.clear("c", ends.Add(-time.Hour), "one-key")
		if a := r.clear("c", ends, "one-key"); a.status != http.StatusUnprocessableEntity || a.refusal(t) != RefusedKeyReused {
			t.Errorf("a key sent again for another deadline answered %d %s", a.status, a.body)
		}
		wantPreferred(t, r.rig, "c", 0)
	})
}

// preferredInView is the owner's preference as api/state gives it, in JSON.
func (r *preferRig) preferredInView() string {
	r.t.Helper()
	r.c.publish(r.clock)
	v := r.app.Snapshot()
	var owner any
	if id, ok := r.o.owner.Group(); ok {
		owner, _ = v.groupByID(id)
	} else {
		id, _ := r.o.owner.Source()
		for _, s := range v.Sources {
			if s.ID == id {
				owner = s
			}
		}
	}
	encoded, err := json.Marshal(owner)
	if err != nil {
		r.t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		r.t.Fatal(err)
	}
	return string(fields["preferred"])
}

// movingTo fails t unless every slot of v, a view at or past the revision a press answered, is on to: moving there, or there.
func movingTo(t *testing.T, when string, v View, answered domain.IntentRevision, to domain.LineageID) {
	t.Helper()
	if v.Revision < answered {
		return
	}
	for _, s := range v.Slots {
		on := domain.LineageID("nothing")
		if s.Server != nil {
			on = *s.Server
		}
		if on != to {
			t.Errorf("%s, the view at revision %d, past the answer's %d, has %s on %s, not moving to %s", when, v.Revision, answered, s.ID, on, to)
		}
	}
}

func TestAPressIsAnsweredAtTheRevisionWhereItsMoveIsStaged(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.putSlot("home", exitIntent(t, "Home", o.exit))
		r.settle()
		r.reportApplied("home")
		sig := r.test(domain.Succeeded, 30)(r.ref("c"))
		receipt, err := r.c.acceptPage(context.Background(), pageWrite{Key: "staged", Fingerprint: []byte("staged"), Decision: runs{PreferServer{Owner: o.owner, Signal: sig}}})
		if err != nil {
			t.Fatal(err)
		}
		movingTo(t, "inside the press's turn", r.app.Snapshot(), receipt.Revision, "c")
		r.settle()
		movingTo(t, "once the turn settled", r.app.Snapshot(), receipt.Revision, "c")
		if v := r.app.Snapshot(); v.Revision < receipt.Revision {
			t.Errorf("the turn ended on the view at %d, before the answer's %d", v.Revision, receipt.Revision)
		}
		r.reportApplied("eu", "home")
		r.clock = r.clock.Add(time.Second)

		a := r.passes("b", 20)
		revision, err := strconv.ParseUint(a.revision, 10, 64)
		if err != nil {
			t.Fatalf("the press answered revision %q", a.revision)
		}
		if a.view.Revision < domain.IntentRevision(revision) {
			t.Errorf("as the press's answer was written the view was at %d, before the answer's %s", a.view.Revision, a.revision)
		}
		movingTo(t, "as the press's answer was written", a.view, domain.IntentRevision(revision), "b")
	})
}

// A press whose Test passed and whose move then failed to stage stops the daemon, as any failed write
// does, and its caller hears no success. Its receipt was stored with its pick, so the same key answers
// it once the daemon is back and has staged the move from that pick.
func TestAPressWhoseMoveFailedToStageIsAnsweredOnceTheMoveIs(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		mustExec(t, r.c.store.writer, `CREATE TRIGGER fail_switch BEFORE INSERT ON switch_events BEGIN SELECT RAISE(ABORT,'switch fails'); END`)
		a, stopped := r.sendStopping(http.MethodPost, o.route()+"/preferred/c", "staging-fails", r.test(domain.Succeeded, 30))
		if stopped == nil {
			t.Fatal("the daemon went on after the move failed to stage")
		}
		if a.status < http.StatusInternalServerError {
			t.Errorf("the press whose move failed to stage answered %d %s", a.status, a.body)
		}
		mustExec(t, r.c.store.writer, `DROP TRIGGER fail_switch`)
		back := &preferRig{rig: r.reopened(), o: o}
		back.settle()
		replay := back.press("c", "staging-fails", back.test(domain.Failed, 0))
		if replay.status != http.StatusOK || replay.verdict(t) != "succeeded" {
			t.Fatalf("once the daemon was back the same key answered %d %s", replay.status, replay.body)
		}
		revision, err := strconv.ParseUint(replay.revision, 10, 64)
		if err != nil {
			t.Fatalf("the replay answered revision %q", replay.revision)
		}
		if replay.view.Revision < domain.IntentRevision(revision) {
			t.Errorf("as the replay was written the view was at %d, before the answer's %d", replay.view.Revision, revision)
		}
		movingTo(t, "as the replay was written", replay.view, domain.IntentRevision(revision), "c")
	})
}

// A command's caller hears once its turn has published the view at the answer's revision. What the turn
// does after that, the plans, records, trim and erase, comes after the answer: the caller hears while
// the runtime has yet to take the turn's plan, and a failure there stops the daemon without taking back
// an answer whose move is staged and published.
func TestACommandIsAnsweredOnceItsViewIsPublished(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		held := newPreferRig(t, o)
		held.c.goals = make(chan gateway.Plan)
		rec, done := held.request(http.MethodPost, o.route()+"/preferred/c", "plan-held")
		turn := make(chan error, 1)
		go func() {
			test := held.test(domain.Succeeded, 30)
			for {
				select {
				case m := <-held.app.manual:
					m.Reply <- test(m.Server)
				case e := <-held.app.commands:
					turn <- held.c.take(context.Background(), e)
					return
				}
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the press was answered only once the runtime took the turn's plan")
		}
		<-held.c.goals
		if err := <-turn; err != nil {
			t.Fatal(err)
		}
		<-done
		if a := rec.answer(); a.status != http.StatusOK || a.verdict(t) != "succeeded" {
			t.Fatalf("the press whose plan the runtime had yet to take answered %d %s", a.status, a.body)
		}

		r := newPreferRig(t, o)
		mustExec(t, r.c.store.writer, `CREATE TRIGGER fail_plan BEFORE UPDATE ON meta WHEN NEW.key='runtime_generation' BEGIN SELECT RAISE(ABORT,'plan write fails'); END`)
		a, stopped := r.sendStopping(http.MethodPost, o.route()+"/preferred/c", "plan-fails", r.test(domain.Succeeded, 30))
		if stopped == nil {
			t.Fatal("the daemon went on after the plan failed to write")
		}
		if a.status != http.StatusOK || a.verdict(t) != "succeeded" {
			t.Fatalf("a press whose move was staged answered %d %s", a.status, a.body)
		}
		revision, err := strconv.ParseUint(a.revision, 10, 64)
		if err != nil {
			t.Fatalf("the press answered revision %q", a.revision)
		}
		if a.view.Revision < domain.IntentRevision(revision) {
			t.Errorf("as the press's answer was written the view was at %d, before the answer's %d", a.view.Revision, revision)
		}
		movingTo(t, "as the press's answer was written", a.view, domain.IntentRevision(revision), "c")
	})
}

func TestTheViewNamesThePreferenceOnlyOnceTheSlotsAreOnIt(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("c", 30)
		if got := r.preferredInView(); got != "null" {
			t.Errorf("at the press's revision, while eu is still landing on c, the view prefers %s", got)
		}
		r.reportApplied("eu")
		if got := r.preferredInView(); !strings.Contains(got, `"server":"c"`) {
			t.Errorf("once eu is on c the view prefers %s, want c", got)
		}
	})
}

func TestTheViewNamesNoPreferenceWhileTheSlotStillReportsTheServerBeforeARevision(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("c", 30)
		rotated, _ := r.c.world.catalogue.Lookup("c")
		rotated.Ref.Digest[5] ^= 0xff
		r.storeLineages(transaction{Lineages: []domain.Lineage{rotated}})
		r.settle()
		if got, _ := r.c.state.slots["eu"].Selection.Server(); got != rotated.Ref {
			t.Fatalf("eu is on %+v, want c's new revision", got)
		}
		if got := r.preferredInView(); got != "null" {
			t.Errorf("while eu still reports a the view prefers %s", got)
		}
		r.reportApplied("eu")
		if got := r.preferredInView(); !strings.Contains(got, `"server":"c"`) {
			t.Errorf("once eu reports c's new revision the view prefers %s, want c", got)
		}
	})
}

// reportAs sends one runtime report in which each slot shows what applied names, landed or not.
func (r *rig) reportAs(applied map[domain.SlotID]domain.Selection) {
	r.t.Helper()
	var reports []gateway.SlotReport
	for _, slot := range slices.Sorted(maps.Keys(applied)) {
		reports = append(reports, gateway.SlotReport{Slot: slot, Phase: gateway.Ready, Applied: applied[slot], Operation: r.operation(slot)})
	}
	if err := r.c.applyReport(context.Background(), gateway.Report{Generation: r.c.state.runtimeGeneration, Slots: reports,
		Topology: gateway.Topology{Epoch: "e1", CorePID: 1, Generation: r.c.state.runtimeGeneration}}); err != nil {
		r.t.Fatal(err)
	}
}

func TestTheViewNamesNoPreferenceOnceTheOnlySlotOnItLeavesTheOwner(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.putSlot("home", exitIntent(t, "Home", o.exit))
		r.settle()
		r.reportApplied("eu", "home")
		r.passes("c", 30)
		onA := domain.Serving(r.ref("a"))
		r.reportAs(map[domain.SlotID]domain.Selection{"eu": onA, "home": domain.Serving(r.ref("c"))})
		if got := r.preferredInView(); !strings.Contains(got, `"server":"c"`) {
			t.Fatalf("once home reports c the view prefers %s, want c", got)
		}
		r.setExit("home", domain.DirectExit())
		r.settle()
		r.reportAs(map[domain.SlotID]domain.Selection{"eu": onA, "home": r.c.state.slots["home"].Selection})
		if got := r.preferredInView(); got != "null" {
			t.Errorf("with home gone Direct and eu still reporting a the view prefers %s", got)
		}
	})
}

func TestTheViewShowsThePreferenceUntilItsDeadlineAndNullAfter(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		if got := r.preferredInView(); got != "null" {
			t.Fatalf("before a press the view prefers %s", got)
		}
		r.passes("c", 30)
		r.reportApplied("eu")
		ends := r.clock.Add(preferFor)
		want := `{"server":"c","endsAt":"` + ends.Format(time.RFC3339Nano) + `"}`
		if got := r.preferredInView(); got != want {
			t.Errorf("the view prefers %s, want %s", got, want)
		}
		r.clock = ends.Add(-time.Second)
		if got := r.preferredInView(); got != want {
			t.Errorf("a second before its end the view prefers %s, want %s", got, want)
		}
		r.clock = ends
		if got := r.preferredInView(); got != "null" {
			t.Errorf("at its end the view prefers %s, want null", got)
		}
	})
}

// reportSlots sends one runtime report of exactly these slots, each naming its pending switch.
func (r *rig) reportSlots(slots ...gateway.SlotReport) {
	r.t.Helper()
	report := r.c.last
	report.Slots = slots
	report.Generation = r.c.state.runtimeGeneration
	report.At = r.clock
	for i := range report.Slots {
		report.Slots[i].Operation = r.operation(report.Slots[i].Slot)
	}
	if err := r.c.applyReport(context.Background(), report); err != nil {
		r.t.Fatal(err)
	}
	r.settle()
}

func TestAClosedSlotWhoseCoreRoutesThePreferredServerShowsNoPreference(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.putSlot("home", exitIntent(t, "Home", o.exit))
		r.settle()
		r.reportApplied("eu", "home")
		r.passes("c", 30)

		closed := gateway.SlotReport{Slot: "eu", Phase: gateway.Closed, Applied: domain.Serving(r.ref("c"))}
		home := gateway.SlotReport{Slot: "home", Phase: gateway.Ready, Applied: domain.Serving(r.ref("a"))}
		r.reportSlots(closed, home)
		if got := r.preferredInView(); got != "null" {
			t.Errorf("closed eu with c routed in the core and home still on a: view reports preference %s, want null", got)
		}

		home.Applied = domain.Serving(r.ref("c"))
		r.reportSlots(closed, home)
		if got := r.preferredInView(); !strings.Contains(got, `"server":"c"`) {
			t.Errorf("home is ready on c while eu is closed: view reports preference %s", got)
		}
	})
}

// boxesOn is every server a box of o draws as applied, read from the view's slot fields as the page
// reads them: a serving slot whose move isn't pending, on the server it draws. That is the one it
// reports, or, after a pass that vouched for none, the one the daemon has it on.
func (r *preferRig) boxesOn() map[domain.LineageID]bool {
	r.t.Helper()
	r.c.publish(r.clock)
	on := map[domain.LineageID]bool{}
	for _, v := range r.app.Snapshot().Slots {
		drawn := v.AppliedServer
		if drawn == nil && v.Runtime.Phase == "unknown" && !v.Pending {
			drawn = v.Server
		}
		if v.Exit == exitDTO(r.o.exit) && v.Status == "serving" && !v.Pending && drawn != nil && v.Server != nil && *drawn == *v.Server {
			on[*drawn] = true
		}
	}
	return on
}

// A pass that could vouch for no slot leaves a settled box drawing the server the daemon has it on, so
// the pick it shows stands through that pass. A pass that reads the slot on nothing still drops it.
func TestAPickStaysShownThroughAnUnconfirmedPass(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		r.passes("c", 30)
		r.reportSlots(gateway.SlotReport{Slot: "eu", Phase: gateway.Ready, Applied: domain.Serving(r.ref("c"))})
		if got := r.preferredInView(); !strings.Contains(got, `"server":"c"`) {
			t.Fatalf("landed on c: the view prefers %s", got)
		}
		r.reportSlots(gateway.SlotReport{Slot: "eu", Phase: gateway.Unknown})
		if got := r.preferredInView(); !strings.Contains(got, `"server":"c"`) || !r.boxesOn()["c"] {
			t.Errorf("a pass that vouched for nothing: the view prefers %s, boxes on %v", got, r.boxesOn())
		}
		r.reportSlots(gateway.SlotReport{Slot: "eu", Phase: gateway.Ready})
		if got := r.preferredInView(); got != "null" {
			t.Errorf("a ready read that says nothing of the core: the view prefers %s, want null", got)
		}
	})
}

func TestTheViewNamesThePreferenceExactlyWhileABoxOfItsOwnerDrawsThatServerApplied(t *testing.T) {
	eachOwner(t, func(t *testing.T, o preferOwner) {
		r := newPreferRig(t, o)
		on := func(id string) gateway.SlotReport {
			return gateway.SlotReport{Slot: "eu", Phase: gateway.Ready, Applied: domain.Serving(r.ref(id))}
		}
		step := func(name string, want domain.LineageID) {
			t.Helper()
			got := r.preferredInView()
			if want == "" && got != "null" || want != "" && !strings.Contains(got, `"server":"`+string(want)+`"`) {
				t.Errorf("%s: the view prefers %s, want %q", name, got, want)
			}
			held := r.preferred()
			if shown := held != nil && r.boxesOn()[held.Server]; shown != (got != "null") {
				t.Errorf("%s: a box draws the preferred server applied: %v, but the view prefers %s", name, shown, got)
			}
		}
		step("on a, nothing preferred", "")
		r.passes("c", 30)
		step("c picked, eu landing", "")
		r.reportSlots(gateway.SlotReport{Slot: "eu", Phase: gateway.Closed, Applied: domain.Serving(r.ref("c"))})
		step("c routed in the core, the gate closed", "")
		r.reportSlots(on("a"))
		step("still reporting a", "")
		r.reportSlots(on("c"))
		step("landed on c", "c")
		r.reportSlots(gateway.SlotReport{Slot: "eu", Phase: gateway.Ready})
		step("a read that says nothing of the core", "")
		r.reportSlots(on("c"))
		step("read on c again", "c")
		r.passes("b", 20)
		step("b picked, eu still on c", "")
		r.reportSlots(on("b"))
		step("landed on b", "b")
	})
}
