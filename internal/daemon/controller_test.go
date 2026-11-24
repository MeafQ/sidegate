package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	"sidegate/internal/metrics"
	gateway "sidegate/internal/runtime"
)

func vlessServer(t *testing.T, host string) domain.Server {
	t.Helper()
	p, err := domain.NewProtocol(domain.VLESS, domain.NewSecret("11111111-2222-4333-8444-555555555555"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := domain.NewServer("root", []domain.Hop{{ID: "root", Endpoint: domain.Endpoint{Host: host, Port: 443}, Protocol: p,
		Transport: domain.Transport{Kind: domain.TCP}, TLS: domain.TLS{Enabled: true, ServerName: "x.example"}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func lineage(t *testing.T, id, source, country, host string) domain.Lineage {
	t.Helper()
	return domain.Lineage{ID: domain.LineageID(id), Source: domain.SourceID(source), Label: country + " " + id, Presence: domain.Present,
		Country: domain.Country{Code: country, Source: domain.CountryRule}, Server: vlessServer(t, host),
		Ref:  domain.ServerRef{Lineage: domain.LineageID(id), Digest: domain.Digest{byte(len(id)), 7}},
		Seen: []domain.Occurrence{{Config: domain.LineageID(id), ConfigName: id, ConfigPlace: 1, Tag: id, Position: 1}}}
}

// rig is a controller with its channels but no other actor, so a test plays the
// runtime and the signal loop itself.
type rig struct {
	t      *testing.T
	dir    string
	c      *controller
	app    *App
	clock  time.Time
	writer *metrics.Writer
	page   http.Handler // The page's handler, once a test writes through it.
	wan    wanBelief    // What the frames a test plays say of the WAN.
}

func newRig(t *testing.T, dir string) *rig {
	t.Helper()
	return newRigAt(t, dir, t0)
}

// newRigAt is a daemon starting at now, on whatever state.db dir holds.
func newRigAt(t *testing.T, dir string, now time.Time) *rig {
	t.Helper()
	return newRigWith(t, dir, now, []probeTarget{{ID: "t1", URL: "http://target.example/"}})
}

// newRigWith is a daemon starting at now with these probe targets, as bootstrap.json lists them.
func newRigWith(t *testing.T, dir string, now time.Time, targets []probeTarget) *rig {
	t.Helper()
	ctx := context.Background()
	st, q, err := openStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	state, recovered, err := st.recover(ctx, now, defaultTimings())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := metrics.OpenWriter(metricsPath(dir), metrics.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := metrics.OpenReader(metricsPath(dir), metrics.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Queries: &Queries{state: q, metrics: reader}, writer: writer, bootstrap: Bootstrap{DataDir: dir}}
	r := &rig{t: t, dir: dir, app: app, clock: now, writer: writer}
	c := newController(st, state, recovered, app, targets)
	c.now = func() time.Time { return r.clock }
	c.goals, c.plans = make(chan gateway.Plan, 1), make(chan observationPlan, 1)
	c.fetchJobs = make(chan catalogue.Request, 1)
	app.control = c
	r.c = c
	c.publish(now)
	t.Cleanup(func() { r.close() })
	return r
}

func (r *rig) memory(slot domain.SlotID) policyMemory { return r.c.memoryOf(r.c.state.slots[slot]) }

func (r *rig) setMemory(slot domain.SlotID, m policyMemory) {
	o, _ := ownerOf(r.c.state.slots[slot])
	r.c.state.owners[o] = m
}

func (r *rig) close() {
	r.c.store.close()
	r.writer.Close()
	r.app.Queries.Close()
}

func (r *rig) settle() {
	r.t.Helper()
	if err := r.c.settle(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

// take hands the controller one command as its loop does, and returns what its caller hears.
func (r *rig) take(command Command) (Receipt, error) {
	r.t.Helper()
	return r.taken(envelope{command: command})
}

// takePage is take for a page write.
func (r *rig) takePage(w pageWrite) (Receipt, error) {
	r.t.Helper()
	return r.taken(envelope{page: &w})
}

func (r *rig) taken(e envelope) (Receipt, error) {
	r.t.Helper()
	reply := make(chan commandResult, 1)
	e.reply = reply
	if err := r.c.take(context.Background(), e); err != nil {
		r.t.Fatal(err)
	}
	result := <-reply
	return result.receipt, result.err
}

func (r *rig) seed(lineages ...domain.Lineage) {
	r.t.Helper()
	tx := transaction{Lineages: lineages}
	for _, l := range lineages {
		if _, ok := r.c.state.sources[l.Source]; !ok {
			source := domain.Source{ID: l.Source, Revision: 1, Name: string(l.Source), Origin: domain.ManualOrigin(domain.Manual{})}
			r.c.state.sources[l.Source] = source
			tx.Sources = append(tx.Sources, source)
		}
	}
	r.storeLineages(tx)
}

// sourceGroup is the group a rig holds of everything in one source.
func sourceGroup(id domain.SourceID) domain.GroupID { return domain.GroupID("all-" + string(id)) }

// storeLineages commits rows the way a refresh does, with a group for each source.
func (r *rig) storeLineages(tx transaction) {
	r.t.Helper()
	change := worldChange{lineages: tx.Lineages}
	for id := range r.c.state.sources {
		if _, held := r.c.world.groups[sourceGroup(id)]; !held {
			members, err := domain.AdmitMembers(asMembers(rule{Sources: []domain.SourceID{id}}))
			if err != nil {
				r.t.Fatal(err)
			}
			change.groups = append(change.groups, domain.Group{ID: sourceGroup(id), Revision: 1, Name: "All " + string(id), Members: members})
		}
	}
	tx.Groups = append(tx.Groups, change.groups...)
	if err := r.c.store.commit(context.Background(), tx); err != nil {
		r.t.Fatal(err)
	}
	r.c.setWorld(r.c.world.with(change))
}

func (r *rig) putSlot(id domain.SlotID, intent domain.SlotIntent) {
	r.t.Helper()
	if _, err := r.c.accept(context.Background(), PutSlot{ID: id, Intent: intent}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) frame(wan domain.WANState, signals ...domain.Signal) {
	r.t.Helper()
	r.wan.read(wan, r.clock, r.clock)
	r.c.ingest(signalFrame{Signals: signals, WAN: r.wan})
	r.settle()
}

// wanReads hands the controller a frame in which the WAN reads state now, and nothing else.
func (r *rig) wanReads(state domain.WANState) {
	r.wan.read(state, r.clock, r.clock)
	r.c.ingest(signalFrame{WAN: r.wan})
}

func (r *rig) probe(id string, verdict domain.Verdict, ms int) domain.Signal {
	s := domain.Signal{Stamp: domain.Stamp{Epoch: r.c.evidence.epoch, At: r.clock}, Server: r.ref(id), Kind: domain.ActiveProbe, Verdict: verdict, Purpose: domain.Heartbeat, Target: "t1"}
	if ms > 0 {
		d := time.Duration(ms) * time.Millisecond
		s.NewConn = &d
	}
	return s
}

func (r *rig) ref(id string) domain.ServerRef {
	l, ok := r.c.world.catalogue.Lookup(domain.LineageID(id))
	if !ok {
		r.t.Fatalf("no lineage %s", id)
	}
	return l.Ref
}

func (r *rig) server(slot domain.SlotID) domain.LineageID {
	server, _ := r.c.state.slots[slot].Selection.Server()
	return server.Lineage
}

func (r *rig) reportApplied(slots ...domain.SlotID) {
	r.t.Helper()
	var reports []gateway.SlotReport
	for _, slot := range slots {
		row := r.c.state.slots[slot]
		report := gateway.SlotReport{Slot: slot, Phase: gateway.Ready, Applied: row.Selection, Operation: r.operation(slot)}
		if _, down := row.Selection.Down(); down {
			report.Phase = gateway.Closed
		}
		reports = append(reports, report)
	}
	if err := r.c.applyReport(context.Background(), gateway.Report{Generation: r.c.state.runtimeGeneration, Slots: reports,
		Topology: gateway.Topology{Epoch: "e1", CorePID: 1, Generation: r.c.state.runtimeGeneration}}); err != nil {
		r.t.Fatal(err)
	}
}

// operation is the pending switch a slot's report would name.
func (r *rig) operation(slot domain.SlotID) domain.EventID {
	return r.c.state.pending[slot].ID
}

func exitIntent(t *testing.T, name string, exit domain.Exit) domain.SlotIntent {
	t.Helper()
	intent, err := domain.NewIntent(name, exit)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

// elected exits through a group of everything in a source.
func elected(t *testing.T, source domain.SourceID) domain.SlotIntent {
	t.Helper()
	return exitIntent(t, "EU", domain.GroupExit(sourceGroup(source)))
}

// electA leaves slot "eu" serving a, applied, and past any KeepOut.
func electA(t *testing.T, r *rig) {
	t.Helper()
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), lineage(t, "b", "good", "DE", "198.51.100.20"))
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", elected(t, "good"))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	if got := r.server("eu"); got != "a" {
		t.Fatalf("elected %v", got)
	}
	r.reportApplied("eu")
	r.clock = r.clock.Add(6 * time.Minute)
}

func TestSwitchIsDurableBeforeEffectAndAppliedOnce(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.frame(domain.WANUp, r.probe("b", domain.Succeeded, 20), r.probe("a", domain.Failed, 0))
	for range 2 {
		c := r.memory("eu").Campaign
		s1, s2 := r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20)
		s1.Campaign, s1.Round, s2.Campaign, s2.Round = c.ID, c.Round, c.ID, c.Round
		r.frame(domain.WANUp, s1, s2)
	}
	if got := r.server("eu"); got != "b" {
		t.Fatalf("after two rounds: %v", got)
	}
	ctx := context.Background()
	events, err := r.app.Switches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Reason != "failureMove" || events[0].AppliedAt != nil || len(events[0].Evidence) == 0 {
		t.Fatalf("pending event: %+v", events[0])
	}
	r.clock = r.clock.Add(time.Second)
	r.reportApplied("eu")
	r.clock = r.clock.Add(time.Second)
	r.reportApplied("eu")
	events, _ = r.app.Switches(ctx)
	if events[0].AppliedAt == nil || !events[0].AppliedAt.Equal(t0.Add(6*time.Minute+time.Second)) {
		t.Fatalf("applied at %v", events[0].AppliedAt)
	}
	m := r.memory("eu")
	if !m.KeepOut["a"].Equal(r.clock.Add(-time.Second).Add(5 * time.Minute)) {
		t.Fatalf("clocks: %+v", m)
	}
}

func TestWANDownCommitsNothingAndKeepsServing(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	before, _ := r.app.Switches(context.Background())
	for range 10 {
		r.clock = r.clock.Add(10 * time.Second)
		excluded := r.probe("a", domain.Unknown, 0)
		excluded.WANExcluded = true
		r.frame(domain.WANDown, excluded)
	}
	after, _ := r.app.Switches(context.Background())
	if len(after) != len(before) {
		t.Fatalf("%d events during a WAN outage", len(after)-len(before))
	}
	if got := r.server("eu"); got != "a" {
		t.Fatalf("after the outage: %v", got)
	}
}

func TestRestartKeepsWhatServes(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	electA(t, r)
	r.settle()
	wantKey := r.c.planKey
	generation := r.c.state.runtimeGeneration
	r.close()

	r2 := newRig(t, dir)
	if got := r2.server("eu"); got != "a" {
		t.Fatalf("recovered %v", got)
	}
	// No evidence yet and an unknown WAN: nothing may move.
	r2.settle()
	if r2.c.planKey != wantKey {
		t.Fatalf("plan changed across a restart:\n%s\nwant\n%s", r2.c.planKey, wantKey)
	}
	if r2.c.state.runtimeGeneration <= generation {
		t.Fatalf("generation went from %d to %d", generation, r2.c.state.runtimeGeneration)
	}
	events, _ := r2.app.Switches(context.Background())
	if len(events) != 1 {
		t.Fatalf("%d events after a restart", len(events))
	}
	if row := r2.c.state.slots["eu"]; row.Slot.Allocation.Ordinal != 1 || r2.c.state.secrets[row.Slot.Allocation.Keys.HostPrivate].IsZero() {
		t.Fatalf("slot row: %+v", row)
	}
}

// A slot owes a move until its readback shows it, however old the move grows, and the move stays while a
// slot is where it took it.
func TestRecoveryKeepsTheMoveEachSlotOwes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, q, err := openStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	defer q.close()
	move := domain.Switch{ID: "move", Owner: domain.GroupOwner("g"), Slots: []domain.SlotID{"s", "t"}, To: domain.Closed(domain.ConfirmedDead), RequestedAt: t0}
	if err := st.commit(ctx, transaction{Switches: []domain.Switch{move}, Pending: map[domain.SlotID]domain.EventID{"s": move.ID, "t": move.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := st.commit(ctx, transaction{Landings: []landing{{Slot: "t", Move: &move, At: t0}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.trimEvents(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	state, _, err := st.recover(ctx, t0, defaultTimings())
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := state.pending["s"]; !ok || p.ID != move.ID || !slices.Equal(p.Slots, move.Slots) || len(state.pending) != 1 {
		t.Fatalf("pending: %v", state.pending)
	}
	if err := st.commit(ctx, transaction{Landings: []landing{{Slot: "s", Move: &move, At: t0}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.trimEvents(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if events, err := q.switches(ctx, 10); err != nil || len(events) != 1 {
		t.Fatalf("the move its slots are on went with its retention: %v, %v", events, err)
	}
	if err := st.commit(ctx, transaction{Landings: []landing{{Slot: "s", At: t0.Add(time.Minute)}, {Slot: "t", At: t0.Add(time.Minute)}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.trimEvents(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if events, err := q.switches(ctx, 10); err != nil || len(events) != 0 {
		t.Fatalf("a move nobody owes or is on outlived its retention: %v, %v", events, err)
	}
}

func TestReplacedSelectionFollowsARotation(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	rotated := lineage(t, "a", "good", "DE", "198.51.100.10")
	rotated.Ref.Digest = domain.Digest{9, 9}
	r.seed(rotated)
	r.settle()
	row := r.c.state.slots["eu"]
	if got := row.Selection; got != domain.Serving(rotated.Ref) {
		t.Fatalf("selection after rotation: %v", got)
	}
	events, _ := r.app.Switches(context.Background())
	if events[0].Reason != "revisionChanged" {
		t.Fatalf("reason %s", events[0].Reason)
	}
}

func TestCommands(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), lineage(t, "b", "good", "DE", "198.51.100.20"))

	wantKind := func(err error, kind CommandKind) {
		t.Helper()
		e, ok := err.(*CommandError)
		if !ok || e.Kind != kind {
			t.Fatalf("err = %v, want %s", err, kind)
		}
	}
	if _, err := r.c.accept(ctx, PutSlot{ID: "bad id", Intent: elected(t, "good")}); err == nil {
		t.Fatal("a bad slot ID was accepted")
	}
	first, err := r.take(PutSlot{Request: "r1", ID: "eu", Intent: elected(t, "good")})
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.c.accept(ctx, PutSlot{Request: "r1", ID: "eu", Intent: elected(t, "good")})
	if err != nil || again.Revision != first.Revision {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	_, err = r.c.accept(ctx, PutSlot{Request: "r1", ID: "eu", Intent: exitIntent(t, "Other", domain.ServerExit("a"))})
	wantKind(err, KindValidation)
	_, err = r.c.accept(ctx, PutSlot{ID: "eu", Intent: elected(t, "good")})
	wantKind(err, KindStale)
	_, err = r.c.accept(ctx, PutSlot{ID: "eu", Expected: 999, Intent: elected(t, "good")})
	wantKind(err, KindStale)
	_, err = r.c.accept(ctx, PutSlot{ID: "other", Expected: 5, Intent: elected(t, "good")})
	wantKind(err, KindNotFound)
	if v := r.app.Snapshot(); len(v.Slots) != 1 || v.Slots[0].Version != first.Revision {
		t.Fatalf("read your own write: %+v", v.Slots)
	}

	_, err = r.c.accept(ctx, PutSource{ID: "s", Input: SourceInput{Name: "S"}})
	wantKind(err, KindValidation)
	_, err = r.c.accept(ctx, PutSource{ID: "s", Input: SourceInput{Name: "S", Remote: &RemoteInput{}}})
	wantKind(err, KindValidation)
	if _, err := r.take(PutSource{ID: "s", Input: SourceInput{Name: "S", Remote: &RemoteInput{URL: domain.NewSecret("https://example.com/secret-token")}}}); err != nil {
		t.Fatal(err)
	}
	view, _ := json.Marshal(r.app.Snapshot())
	if strings.Contains(string(view), "secret-token") {
		t.Fatal("the URL reached the view")
	}
	source := r.c.state.sources["s"]
	remote, _ := source.Origin.Remote()
	if r.c.state.secrets[remote.Location].Reveal() == "" {
		t.Fatal("URL not stored")
	}
	if _, err := r.c.accept(ctx, DeleteSource{ID: "s", Expected: domain.IntentRevision(source.Revision)}); err != nil {
		t.Fatal(err)
	}
	if !r.c.state.secrets[remote.Location].IsZero() {
		t.Fatal("URL kept after delete")
	}

	_, err = r.c.accept(ctx, PutSettings{RetentionDays: 0})
	wantKind(err, KindValidation)
	good := PutSettings{RetentionDays: 100}
	if _, err := r.take(good); err != nil {
		t.Fatal(err)
	}
	if r.app.Settings().AnalyticsRetentionDays != 100 {
		t.Fatalf("settings: %+v", r.app.Settings())
	}
	_, err = r.c.accept(ctx, good)
	wantKind(err, KindStale)

	row := r.c.state.slots["eu"]
	if _, err := r.c.accept(ctx, DeleteSlot{ID: "eu", Expected: row.Slot.Version}); err != nil {
		t.Fatal(err)
	}
	if !r.c.state.slots["eu"].Deleting {
		t.Fatal("not tombstoned")
	}
	if err := r.c.applyReport(ctx, gateway.Report{Deleted: []domain.SlotID{"eu"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.c.state.slots["eu"]; ok {
		t.Fatal("slot kept after the runtime's ack")
	}
}

func TestRefreshFailureTheWANExplainsBlamesNobody(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	if _, err := r.c.accept(ctx, PutSource{ID: "s", Input: SourceInput{Name: "S", Remote: &RemoteInput{URL: domain.NewSecret("https://example.com/f")}}}); err != nil {
		t.Fatal(err)
	}
	fail := func() catalogue.Refresh {
		source := r.c.state.sources["s"]
		r.c.startFetches(r.clock)
		<-r.c.fetchJobs
		return catalogue.Refresh{Basis: catalogue.Basis{Source: source.ID, Revision: source.Revision}, Outcome: catalogue.FetchFailed}
	}
	count := func() int {
		var n int
		if err := r.app.state.reader.QueryRowContext(ctx, `SELECT count(*) FROM refresh_events WHERE outcome=? AND wan_excluded=0`, int(catalogue.FetchFailed)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	r.wanReads(domain.WANUp)
	refresh := fail()
	r.clock = r.clock.Add(time.Second)
	r.wanReads(domain.WANDown)
	r.clock = r.clock.Add(time.Second)
	r.wanReads(domain.WANUp)
	if err := r.c.applyRefresh(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("a fetch the outage overlapped counted %d failures", n)
	}
	r.clock = r.clock.Add(2 * time.Minute)
	refresh = fail()
	r.clock = r.clock.Add(time.Second)
	if err := r.c.applyRefresh(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("a fetch with the WAN up counted %d failures", n)
	}
}

// Before the first baseline answers, the WAN is unknown and a failed fetch says nothing of the
// provider. Where a route can prove the WAN, no fetch starts until the WAN reads up, and one that fails
// after it is the provider's.
func TestAFetchWaitsForTheWANToReadUp(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"))
	r.putSlot("eu", elected(t, "good"))
	if _, err := r.c.accept(ctx, PutSource{ID: "s", Input: SourceInput{Name: "S", Remote: &RemoteInput{URL: domain.NewSecret("https://example.com/f")}}}); err != nil {
		t.Fatal(err)
	}
	r.c.startFetches(r.clock)
	select {
	case <-r.c.fetchJobs:
		t.Fatal("a fetch started with the WAN unknown, where its failure blames nobody")
	default:
	}
	r.wanReads(domain.WANUp)
	r.c.startFetches(r.clock)
	var job catalogue.Request
	select {
	case job = <-r.c.fetchJobs:
	default:
		t.Fatal("no fetch started once the WAN read up")
	}
	r.clock = r.clock.Add(time.Second)
	if err := r.c.applyRefresh(ctx, catalogue.Refresh{Basis: job.Basis(), Outcome: catalogue.FetchFailed}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := r.app.state.reader.QueryRowContext(ctx, `SELECT count(*) FROM refresh_events WHERE outcome=? AND wan_excluded=0`, int(catalogue.FetchFailed)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a fetch with the WAN up counted %d failures, want 1", n)
	}
}

// With no route, only a baseline can prove the WAN, and a network that blocks every baseline target
// reads it down for good. A fresh gateway still fetches its first source there: a failure while the WAN
// is unproven blames nobody and tries again in a minute, and a success brings the servers in.
func TestAFirstFetchStartsWhereNoRouteCanProveTheWAN(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	r.wanReads(domain.WANDown)
	if _, err := r.c.accept(ctx, PutSource{ID: "acme", Input: SourceInput{Name: "Acme", Remote: &RemoteInput{URL: domain.NewSecret("https://example.com/f")}}}); err != nil {
		t.Fatal(err)
	}
	fetch := func(what string) catalogue.Request {
		t.Helper()
		r.c.startFetches(r.clock)
		select {
		case job := <-r.c.fetchJobs:
			return job
		default:
			t.Fatalf("no %s fetch started with no route to prove the WAN", what)
			return catalogue.Request{}
		}
	}
	job := fetch("first")
	r.clock = r.clock.Add(time.Second)
	if err := r.c.applyRefresh(ctx, catalogue.Refresh{Basis: job.Basis(), Outcome: catalogue.FetchFailed}); err != nil {
		t.Fatal(err)
	}
	var blamed int
	if err := r.app.state.reader.QueryRowContext(ctx, `SELECT count(*) FROM refresh_events WHERE outcome=? AND wan_excluded=0`, int(catalogue.FetchFailed)).Scan(&blamed); err != nil {
		t.Fatal(err)
	}
	if blamed != 0 {
		t.Fatalf("a failure with the WAN unproven blamed the provider %d times", blamed)
	}
	r.clock = r.clock.Add(time.Minute)
	fetch("second")
	refreshed(t, r.c, "acme", parsedAcme(t, acmeText(t)), r.clock)
	if got := len(r.c.world.sourceMembers("acme")); got != 12 {
		t.Fatalf("the first fetch brought %d servers, want 12", got)
	}
}

func TestLastDiffSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	st, q, err := openStore(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	defer q.close()
	if err := st.commit(ctx, transaction{RefreshEvents: []refreshEvent{
		{Source: "s", At: t0, Outcome: catalogue.Updated, Added: 9},
		{Source: "s", At: t0.Add(time.Hour), Outcome: catalogue.Updated, Added: 1, Missing: 2, Diagnostics: []catalogue.Diagnostic{{Entry: 3, Code: "c", Field: "f"}}},
		{Source: "s", At: t0.Add(2 * time.Hour), Outcome: catalogue.FetchFailed},
	}}); err != nil {
		t.Fatal(err)
	}
	diffs, err := q.lastDiffs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := diffs["s"]; d.Added != 1 || d.Missing != 2 || len(d.Diagnostics) != 1 {
		t.Fatalf("diff: %+v", d)
	}
}

func TestGeneratedPrivateKeysAreClamped(t *testing.T) {
	keys, secrets, err := generateKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []domain.SecretRef{keys.HostPrivate, keys.ClientPrivate} {
		raw, err := base64.StdEncoding.DecodeString(secrets[ref].Reveal())
		if err != nil || len(raw) != 32 || raw[0]&7 != 0 || raw[31]&128 != 0 || raw[31]&64 == 0 {
			t.Fatalf("key %v, %v", raw, err)
		}
	}
	if keys.HostPublic == keys.ClientPublic {
		t.Fatal("one key pair twice")
	}
}

func TestStoreRoundTripsLineages(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, q, err := openStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	defer q.close()
	in := lineage(t, "a", "good", "DE", "h.example")
	in.Identity = domain.Identity{ProviderID: "p", Strong: domain.Digest{1}, Endpoint: domain.Digest{2}, HostKey: domain.Digest{3}, RealityTie: domain.Digest{4}, CredentialKey: domain.Digest{5}, NameKey: domain.Digest{6}}
	in.MissingSince, in.GraceUntil = t0, t0.Add(time.Hour)
	in.Reality = []domain.RealityPicks{{ServerName: domain.PickAbsence{Since: t0, Misses: 2}}}
	source := domain.Source{ID: "good", Revision: 1, Name: "good", Origin: domain.ManualOrigin(domain.Manual{})}
	if err := st.commit(ctx, transaction{Sources: []domain.Source{source}, Lineages: []domain.Lineage{in}}); err != nil {
		t.Fatal(err)
	}
	_, recovered, err := st.recover(ctx, t0, defaultTimings())
	if err != nil {
		t.Fatal(err)
	}
	out, ok := recovered.catalogue.Lookup("a")
	if !ok {
		t.Fatal("lost")
	}
	ib, _ := encodeLineage(in)
	ob, _ := encodeLineage(out)
	if string(ib) != string(ob) {
		t.Fatalf("round trip:\n%s\n%s", ib, ob)
	}
	root, hops := out.Server.Closure()
	if _, cred, _, _, _ := hops[0].Protocol.Fields(); root != "root" || cred.Reveal() == "" {
		t.Fatalf("server: %s %+v", root, hops)
	}
}

func TestAStoredNameKeepsItsRemarksAndTagAcrossARestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, q, err := openStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	in := lineage(t, "a", "s", "SE", "a.example")
	name, _ := domain.NewName("Sweden", "proxy")
	in.Label, in.Naming = "Sweden · proxy", domain.Naming{Name: name, Home: "a"}
	if err := st.commit(ctx, transaction{Lineages: []domain.Lineage{in}}); err != nil {
		t.Fatal(err)
	}
	q.close()
	st.close()
	st, q, err = openStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	defer q.close()
	_, recovered, err := st.recover(ctx, t0, defaultTimings())
	if err != nil {
		t.Fatal(err)
	}
	out, ok := recovered.catalogue.Lookup("a")
	if !ok {
		t.Fatal("lost")
	}
	if got := out.Naming.Name; got.Remarks() != "Sweden" || got.Tag() != "proxy" || out.Naming.Home != "a" {
		t.Errorf("name after restart: remarks %q tag %q home %q, want Sweden, proxy, a", got.Remarks(), got.Tag(), out.Naming.Home)
	}
	if out.Label != "Sweden · proxy" {
		t.Errorf("label after restart %q, want %q", out.Label, "Sweden · proxy")
	}
}

// A state.db from an older or a newer build is refused and left as it is, with word to delete it.
func TestStoreRefusesAnotherSchemaVersion(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{14, 99} {
		dir := t.TempDir()
		st, q, err := openStore(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, st.writer, fmt.Sprintf("PRAGMA user_version=%d", version))
		st.close()
		q.close()
		_, _, err = openStore(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("schema version %d", version)) || !strings.Contains(err.Error(), "delete the file") {
			t.Fatalf("version %d opened with %v, want a refusal that says to delete the file", version, err)
		}
		db, err := openState(statePath(dir), "")
		if err != nil {
			t.Fatal(err)
		}
		var stored int
		if err := db.QueryRow("PRAGMA user_version").Scan(&stored); err != nil || stored != version {
			t.Errorf("the refused file reads version %d (%v), want %d", stored, err, version)
		}
		db.Close()
	}
}

// A state.db made before switch_events_applied, at the same schema version, gains it on open and keeps
// its rows.
func TestAnExistingStoreGainsTheAppliedIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := openState(statePath(dir), "")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, strings.TrimSuffix(stateSchema, switchAppliedIndex))
	mustExec(t, db, "PRAGMA user_version=15")
	mustExec(t, db, `INSERT INTO meta VALUES('revision', 7)`)
	db.Close()
	for range 2 { // The second open finds it made.
		st, q, err := openStore(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		var on string
		if err := q.reader.QueryRow(`SELECT tbl_name FROM sqlite_schema WHERE type='index' AND name='switch_events_applied'`).Scan(&on); err != nil || on != "switch_events" {
			t.Errorf("switch_events_applied is on %q, %v", on, err)
		}
		if state, _, err := st.recover(ctx, t0, defaultTimings()); err != nil || state.revision != 7 {
			t.Errorf("recovered revision %d, %v, want 7", state.revision, err)
		}
		st.close()
		q.close()
	}
}

// Each slot's newest landing before the window is listed, by its sequence, with every landing in the
// window, even one made before it on a clock set back since.
func TestLandingsSinceKeepsEachSlotsNewestBefore(t *testing.T) {
	ctx := context.Background()
	st, q, err := openStore(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	defer q.close()
	for _, l := range []struct {
		slot   string
		offset time.Duration
	}{{"a", -3}, {"b", -5}, {"a", 4}, {"a", -2}, {"c", 1}, {"a", 2}, {"b", -9}} {
		mustExec(t, st.writer, `INSERT INTO landings(slot, at) VALUES(?,?)`, l.slot, t0.Add(l.offset*time.Minute).UnixMilli())
	}
	got, err := landingsSince(ctx, q.reader, t0)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, l := range got {
		listed = append(listed, fmt.Sprintf("%s%+d", l.Slot, l.At.Sub(t0)/time.Minute))
	}
	if want := []string{"a+4", "a-2", "c+1", "a+2", "b-9"}; !slices.Equal(listed, want) {
		t.Errorf("listed %v, want %v", listed, want)
	}
}

func TestTrimEventsDropsOldHistoryOnly(t *testing.T) {
	ctx := context.Background()
	st, q, err := openStore(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	defer q.close()
	old, recent := t0.Add(-40*24*time.Hour), t0
	if err := st.commit(ctx, transaction{
		Switches: []domain.Switch{
			{ID: "old", Owner: domain.SlotOwner("s"), Slots: []domain.SlotID{"s"}, To: domain.Closed(domain.Deleting), RequestedAt: old, AppliedAt: &old},
			{ID: "new", Owner: domain.SlotOwner("s"), Slots: []domain.SlotID{"s"}, To: domain.Closed(domain.Deleting), RequestedAt: recent, AppliedAt: &recent},
		},
		RefreshEvents: []refreshEvent{{Source: "x", At: old, Outcome: catalogue.FetchFailed}, {Source: "x", At: recent, Outcome: catalogue.FetchFailed}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.trimEvents(ctx, t0.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	events, err := q.switches(ctx, 10)
	if err != nil || len(events) != 1 || events[0].ID != "new" {
		t.Fatalf("events: %+v, %v", events, err)
	}
}

func TestAnalyticsJoin(t *testing.T) {
	start := t0.Truncate(time.Hour)
	ms := func(n int) *time.Duration {
		d := time.Duration(n) * time.Millisecond
		return &d
	}
	dims := map[domain.LineageID]dimension{
		"good1": {Source: "good", Country: "DE"}, "good2": {Source: "good", Country: "DE"},
		"bad": {Source: "bad", Country: "DE"}, "lonely": {Source: "good", Country: "NL"}, "nocountry": {Source: "bad"},
	}
	series := metrics.Series{
		Buckets: []metrics.Bucket{
			{Start: start, Lineage: "good1", Target: "t1", Purpose: domain.Heartbeat, Attempts: 4, Successes: 4},
			{Start: start, Lineage: "good1", Target: "t2", Purpose: domain.Heartbeat, Attempts: 4, Successes: 4},
			{Start: start, Lineage: "good1", Target: "t1", Purpose: domain.Confirmation, Attempts: 5, Successes: 5, PingMedian: ms(100), PingSamples: 4},
			{Start: start, Lineage: "good1", Target: "t2", Purpose: domain.Check, Attempts: 1, Successes: 1, PingMedian: ms(1000), PingSamples: 4},
			{Start: start, Lineage: "bad", Target: "t1", Purpose: domain.Heartbeat, Attempts: 4, Successes: 2, WANExcluded: 2, PingMedian: ms(300), PingSamples: 2},
			{Start: start, Lineage: "bad", Target: "t2", Purpose: domain.Heartbeat, Attempts: 4, Successes: 2, PingMedian: ms(3000), PingSamples: 2},
			{Start: start, Lineage: "lonely", Target: "t1", Purpose: domain.Heartbeat, Attempts: 1, Successes: 1, PingMedian: ms(50), PingSamples: 1},
			{Start: start, Lineage: "nocountry", Target: "t1", Purpose: domain.Heartbeat, Attempts: 1, Successes: 1, PingMedian: ms(50), PingSamples: 1},
		},
		Traffic: []metrics.TrafficBucket{{Start: start, Lineage: "good1", Up: 10, Down: 20}},
		Gaps:    2, CoveredUntil: start.Add(time.Minute),
	}
	events := eventCounts{
		ForcedSwitchAways: map[eventKey[domain.LineageID]]uint64{{start.UnixMilli(), "bad"}: 2},
	}
	switches := []domain.Switch{
		{Owner: domain.GroupOwner("g"), Slots: []domain.SlotID{"eu"}, From: domain.Closed(domain.Unselected), To: domain.Serving(ref("bad")), Reason: domain.InitialSelection, AppliedAt: &start},
		{Owner: domain.GroupOwner("g"), Slots: []domain.SlotID{"eu"}, From: domain.Serving(ref("bad")), To: domain.Serving(ref("good1")), Reason: domain.FailureMove, AppliedAt: ptr(start.Add(2 * time.Minute))},
	}
	served := usageOf(switches, shown(switches...), start.Add(4*time.Minute))
	report := analyticsReport(series, dims, events, served, AnalyticsQuery{From: start, To: start.Add(time.Hour), Bucket: 5 * time.Minute})
	by := map[domain.LineageID]AnalyticsBucket{}
	for _, b := range report.Buckets {
		by[b.Lineage] = b
	}
	if report.GapCount != 2 {
		t.Fatalf("report: %+v", report)
	}
	if b := by["good1"]; b.PingMedianMs == nil || *b.PingMedianMs != 550 || b.ProbeAttempts != 8 || b.Uploaded != 10 || !b.ManagementTrafficIncluded {
		t.Fatalf("good1: %+v", b)
	}
	if b := by["bad"]; b.ForcedSwitchAways != 2 || b.WANExcluded != 2 || b.ServedSeconds != 120 || b.Incidents != 1 {
		t.Fatalf("bad: %+v", b)
	}
	if b := by["good1"]; b.ServedSeconds != 120 || b.Incidents != 0 {
		t.Fatalf("good1 served %v s, %d incidents", b.ServedSeconds, b.Incidents)
	}
	filtered := analyticsReport(series, dims, events, served, AnalyticsQuery{From: start, To: start.Add(time.Hour), Bucket: 5 * time.Minute, Sources: []domain.SourceID{"good"}})
	for _, b := range filtered.Buckets {
		if b.Source != "good" {
			t.Fatalf("filter let through %+v", b)
		}
	}
}

func TestViewAndEventShapes(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	body, err := json.Marshal(r.app.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	slot := view["slots"].([]any)[0].(map[string]any)
	for _, key := range []string{"createdAt", "id", "version", "name", "exit", "status", "downReason", "server", "appliedServer", "leavingBy", "pending", "runtime", "unifi", "backups"} {
		if _, ok := slot[key]; !ok {
			t.Fatalf("slot lacks %s: %s", key, body)
		}
	}
	if slot["unifi"].(map[string]any)["interface"] != "pxa1" {
		t.Fatalf("unifi: %v", slot["unifi"])
	}
	if slot["backups"].([]any)[0] != "b" {
		t.Fatalf("backups: %v", slot["backups"])
	}
	events, err := r.app.Switches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Reason != "initialSelection" || events[0].From.Kind != "down" || events[0].To.Kind != "up" {
		t.Fatalf("event: %+v", events[0])
	}
}
