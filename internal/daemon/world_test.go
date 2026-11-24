package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

func (r *rig) wantRefused(err error, kind CommandKind, reason Reason) *CommandError {
	r.t.Helper()
	var refusal *CommandError
	if !asCommand(err, &refusal) || refusal.Kind != kind || refusal.Reason != reason {
		r.t.Fatalf("err = %v (%v), want %s for %s", err, refusal, kind, reason)
	}
	return refusal
}

// boxed is m with its box condition set to box.
func boxed(m domain.Members, box domain.Box) domain.Members {
	m.Box = box
	return m
}

// picked is a hand-picked group's rule: "box is ticked" alone.
var picked = domain.Members{Box: domain.BoxTicked}

// rule is a group's conditions, each one "is any of".
type rule struct {
	Sources   []domain.SourceID
	Countries []string
	Names     domain.Filter
}

func asMembers(r rule) domain.Members {
	return domain.Members{Sources: domain.Among[domain.SourceID]{Values: r.Sources}, Countries: domain.Among[string]{Values: r.Countries}, Names: r.Names}
}

func ruleMembers(t *testing.T, r rule) domain.Members {
	t.Helper()
	m, err := domain.AdmitMembers(asMembers(r))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// improving gives slot's owner a comparison round against rival, after it won every round but the one
// that moves the group.
func (r *rig) improving(slot domain.SlotID, rival domain.ServerRef) campaign {
	r.t.Helper()
	m := r.memory(slot)
	current, ok := m.Selection.Server()
	if !ok {
		r.t.Fatalf("%s isn't serving: %v", slot, describe(m.Selection))
	}
	m.Campaigns++
	c := campaign{ID: domain.CampaignID(fmt.Sprintf("improve/%d", m.Campaigns)), Path: improvePath, Epoch: r.c.evidence.epoch,
		Incumbent: current, Rivals: []domain.ServerRef{rival}, Wins: []tally{{}}, Due: r.clock, Round: 1, Target: r.c.evidence.ranking}
	m.Campaign = &c
	m.Streaks = map[domain.LineageID]int{rival.Lineage: compareWins - 1}
	r.setMemory(slot, m)
	return c
}

// putGroup creates group id with these ticks, or replaces its name and rule, which keeps its ticks.
func (r *rig) putGroup(id domain.GroupID, name string, members domain.Members, ticked ...domain.LineageID) {
	r.t.Helper()
	cmd := PutGroup{ID: id, Name: name, Members: members, Ticked: slices.Sorted(slices.Values(ticked))}
	if g, ok := r.c.world.groups[id]; ok {
		cmd.Expected, cmd.Ticked = domain.IntentRevision(g.Revision), nil
	}
	if _, err := r.c.accept(context.Background(), cmd); err != nil {
		r.t.Fatal(err)
	}
}

// tick leaves exactly these servers ticked in group id.
func (r *rig) tick(id domain.GroupID, ids ...domain.LineageID) {
	r.t.Helper()
	want := map[domain.LineageID]bool{}
	for _, l := range r.c.world.groups[id].Ticked {
		want[l] = false
	}
	for _, l := range ids {
		want[l] = true
	}
	for _, l := range slices.Sorted(maps.Keys(want)) {
		if _, err := r.c.accept(context.Background(), TickServer{Group: id, Server: l, Ticked: want[l]}); err != nil {
			r.t.Fatal(err)
		}
	}
}

// putPicked puts a hand-picked group holding these servers.
func (r *rig) putPicked(id domain.GroupID, name string, ids ...domain.LineageID) {
	r.t.Helper()
	r.putGroup(id, name, picked, ids...)
	r.tick(id, ids...)
}

func (r *rig) setExit(slot domain.SlotID, exit domain.Exit) {
	r.t.Helper()
	row := r.c.state.slots[slot]
	if _, err := r.c.accept(context.Background(), PutSlot{ID: slot, Expected: row.Slot.Version, Intent: exitIntent(r.t, row.Slot.Intent.Name(), exit)}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) targetOf(slot domain.SlotID) gateway.SlotTarget {
	r.t.Helper()
	plan, _, err := r.c.desired()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, target := range plan.Slots() {
		if target.Slot.ID == slot {
			return target
		}
	}
	r.t.Fatalf("no target for %s", slot)
	return gateway.SlotTarget{}
}

func (r *rig) switches() []SwitchEvent {
	r.t.Helper()
	events, err := r.app.Switches(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return events
}

// twoSlotsOnOneGroup moves slots one and two, serving a and b on server Exits, onto group g, one first,
// with every server of a to d fresh and past any KeepOut. Group g takes one's server, and two follows it
// with a move of its own that hasn't applied.
func twoSlotsOnOneGroup(t *testing.T, r *rig) {
	t.Helper()
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), lineage(t, "b", "good", "DE", "198.51.100.20"),
		lineage(t, "c", "good", "DE", "198.51.100.30"), lineage(t, "d", "good", "DE", "198.51.100.40"), lineage(t, "e", "good", "DE", "198.51.100.50"))
	r.c.evidence.epoch = "e1"
	r.putPicked("g", "Group", "a", "b", "c", "d")
	r.putSlot("one", exitIntent(t, "One", domain.ServerExit("a")))
	r.putSlot("two", exitIntent(t, "Two", domain.ServerExit("b")))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	if r.server("one") != "a" || r.server("two") != "b" {
		t.Fatalf("elected %v and %v", r.server("one"), r.server("two"))
	}
	r.reportApplied("one")
	r.reportApplied("two")
	r.clock = r.clock.Add(6 * time.Minute)
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20), r.probe("c", domain.Succeeded, 30), r.probe("d", domain.Succeeded, 40))
	r.setExit("one", domain.GroupExit("g"))
	r.setExit("two", domain.GroupExit("g"))
	r.settle()
}

// The first slot on a group brings its server, so an Exit change that still allows it moves nothing.
// The second takes the group's server, since a group has one.
func TestASlotJoiningAGroupTakesItsServer(t *testing.T) {
	r := newRig(t, t.TempDir())
	twoSlotsOnOneGroup(t, r)
	if r.server("one") != "a" || r.server("two") != "a" {
		t.Fatalf("on %v and %v, want both on a", r.server("one"), r.server("two"))
	}
	if target := r.targetOf("one"); target.Selection != domain.Serving(r.ref("a")) || target.Passage != (gateway.Passage{}) || target.Operation != "" {
		t.Fatalf("the first slot on the group moved: %+v", target)
	}
	events := r.switches()
	if e := events[0]; e.Owner.Group != "g" || !slices.Equal(e.Slots, []domain.SlotID{"two"}) || e.Reason != "exitChanged" || e.From.Server == nil || *e.From.Server != "b" {
		t.Fatalf("the second slot's move: %+v", e)
	}
	r.reportApplied("two")
	before := len(r.switches())
	for range 3 {
		r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	}
	if after := len(r.switches()); after != before {
		t.Fatalf("%d switches followed", after-before)
	}
}

// A failure seen through either slot is the group's, and the group moves both at once, in one switch
// that names the group.
func TestAGroupMovesEverySlotOnItTogether(t *testing.T) {
	r := newRig(t, t.TempDir())
	twoSlotsOnOneGroup(t, r)
	r.reportApplied("two")
	if r.memory("one").Campaign != nil {
		t.Fatal("a campaign before any failure")
	}
	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20), r.probe("c", domain.Succeeded, 30), r.probe("d", domain.Succeeded, 40))
	campaign := r.memory("two").Campaign
	if campaign == nil || campaign.Incumbent != r.ref("a") || campaign != r.memory("one").Campaign {
		t.Fatalf("the slots don't share one campaign on a: %+v and %+v", campaign, r.memory("one").Campaign)
	}
	before := len(r.switches())
	for range 2 {
		c := r.memory("one").Campaign
		var round []domain.Signal
		for _, ref := range append([]domain.ServerRef{c.Incumbent}, c.Rivals...) {
			s := r.probe(string(ref.Lineage), domain.Succeeded, 20)
			if ref == c.Incumbent {
				s = r.probe(string(ref.Lineage), domain.Failed, 0)
			}
			s.Campaign, s.Round = c.ID, c.Round
			round = append(round, s)
		}
		r.clock = r.clock.Add(time.Second)
		r.frame(domain.WANUp, round...)
	}
	if r.server("one") == "a" || r.server("one") != r.server("two") {
		t.Fatalf("after the failure: one on %v, two on %v", r.server("one"), r.server("two"))
	}
	events := r.switches()[:len(r.switches())-before]
	if len(events) != 1 || events[0].Owner.Group != "g" || !slices.Equal(events[0].Slots, []domain.SlotID{"one", "two"}) || events[0].Reason != "failureMove" {
		t.Fatalf("the move: %+v", events)
	}
	r.reportApplied("one")
	if r.c.state.pending["two"].AppliedAt == nil || !r.memory("two").KeepOut["a"].After(r.clock) {
		t.Fatalf("the first slot to apply didn't apply the group's move: %+v, keep out %v", r.c.state.pending["two"], r.memory("two").KeepOut)
	}
	r.reportApplied("two")
	if len(r.c.state.pending) != 0 {
		t.Fatalf("still pending: %v", r.c.state.pending)
	}
}

func TestAGroupEditMovesNoVersionAndReachesEverySlotOnOneSettle(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	twoSlotsOnOneGroup(t, r)
	r.reportApplied("two")
	one, two := r.c.state.slots["one"].Slot.Version, r.c.state.slots["two"].Slot.Version

	r.frame(domain.WANUp, r.probe("a", domain.Failed, 0), r.probe("b", domain.Succeeded, 20), r.probe("c", domain.Succeeded, 30), r.probe("d", domain.Succeeded, 40))
	campaign := r.memory("one").Campaign
	if campaign == nil || !slices.Contains(campaign.Rivals, r.ref("b")) {
		t.Fatalf("no campaign comparing b: %+v", campaign)
	}

	r.putPicked("g", "Renamed, with one more", "a", "b", "c", "d", "e")
	r.settle()
	kept := r.memory("one").Campaign
	if kept == nil || kept.ID != campaign.ID {
		t.Fatalf("a rename and an unrelated addition cancelled a campaign: %+v", kept)
	}

	r.putPicked("g", "Renamed", "a", "c", "d", "e")
	r.settle()
	if m := r.memory("one"); m.Campaign == nil || m.Campaign.ID != campaign.ID || !m.QuietUntil.IsZero() {
		t.Fatalf("a group edit dropping one of a campaign's rivals cancelled it or set a quiet period: %+v", m)
	}

	moves := len(r.switches())
	r.putPicked("g", "Renamed", "c", "d", "e")
	r.settle()
	if r.server("one") == "a" || r.server("one") == "" || r.server("two") != r.server("one") {
		t.Fatalf("after the group dropped their server: one on %v, two on %v", r.server("one"), r.server("two"))
	}
	if got := len(r.switches()) - moves; got != 1 {
		t.Fatalf("%d switches for one move of the group", got)
	}
	rowOne, rowTwo := r.c.state.slots["one"], r.c.state.slots["two"]
	if rowOne.Slot.Version != one || rowTwo.Slot.Version != two {
		t.Fatalf("a group edit moved Version: %d and %d, were %d and %d", rowOne.Slot.Version, rowTwo.Slot.Version, one, two)
	}
	if _, err := r.c.accept(ctx, PutSlot{ID: "two", Expected: two, Intent: rowTwo.Slot.Intent}); err != nil {
		t.Fatalf("an editor open on a slot got a false refusal after a group edit: %v", err)
	}
}

// A group grows from one server to 129. Every member gets a probe route, the slot stays on the one it
// had, and nothing says a server was left out.
func TestAGroupThatGrowsGivesEveryMemberARoute(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "z", "s", "DE", "z.example"))
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", exitIntent(t, "EU", domain.GroupExit(sourceGroup("s"))))
	r.frame(domain.WANUp, r.probe("z", domain.Succeeded, 10))
	if r.server("eu") != "z" {
		t.Fatalf("elected %v", r.server("eu"))
	}
	r.reportApplied("eu")
	before := len(r.switches())

	var grown []domain.Lineage
	for i := range 128 {
		grown = append(grown, lineage(t, fmt.Sprintf("a%03d", i), "s", "DE", "g.example"))
	}
	r.seed(grown...)
	r.settle()

	if r.server("eu") != "z" || len(r.switches()) != before {
		t.Fatalf("the slot moved to %v", r.server("eu"))
	}
	if got := len(r.c.world.Pool(domain.GroupExit(sourceGroup("s"))).members); got != 129 || len(r.c.world.routes) != 129 {
		t.Fatalf("%d members and %d routes, want all 129 of each", got, len(r.c.world.routes))
	}
	plan, _, err := r.c.desired()
	if err != nil {
		t.Fatal(err)
	}
	if servers := plan.Servers(); len(servers) != 129 || !slices.Contains(servers, r.ref("z")) || !slices.Contains(servers, r.ref("a127")) {
		t.Fatalf("the plan holds %d servers, want every member", len(servers))
	}
	obs, _ := r.c.observation()
	if _, ok := obs.Sources[r.ref("a127")]; !ok {
		t.Error("the last member's probes have no provider")
	}
	if _, ok := obs.Beats[r.ref("z")]; !ok {
		t.Error("z gets no heartbeat")
	}
	r.c.publish(r.clock)
	page, err := r.app.Servers(ctx, PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.ID == "z" && !item.Supported {
			t.Errorf("z's row: %+v", item)
		}
	}
}

// The routes are every present member of an Exit in use: a server Exit's server, and each member of a
// group a slot uses, however many. A present server no slot uses gets none.
func TestTheRoutesAreEveryPresentMemberOfAnExitInUse(t *testing.T) {
	var lineages []domain.Lineage
	for i := range 80 {
		lineages = append(lineages, lineage(t, fmt.Sprintf("k%03d", i), "kept", "DE", "k.example"))
	}
	for i := range 70 {
		lineages = append(lineages, lineage(t, fmt.Sprintf("r%03d", i), "second", "NL", "r.example"))
	}
	assigned := lineage(t, "zz-assigned", "other", "SE", "u.example")
	gone := lineage(t, "zz-gone", "second", "NL", "g.example")
	gone.Presence = domain.Missing
	catalogue := domain.NewCatalogue(1, append(lineages, assigned, gone))
	groups := map[domain.GroupID]domain.Group{"second": {ID: "second", Name: "Second", Members: ruleMembers(t, rule{Sources: []domain.SourceID{"second"}})}}
	refs := func(w world, prefix string) int {
		n := 0
		for ref := range w.routes {
			if strings.HasPrefix(string(ref.Lineage), prefix) {
				n++
			}
		}
		return n
	}

	w := newWorld(catalogue, groups, nil, []exitUse{{Slot: "a", Exit: domain.ServerExit("zz-assigned")}, {Slot: "b", Exit: domain.GroupExit("second")}})
	if got := refs(w, "zz-assigned"); got != 1 {
		t.Error("an assigned server is left out")
	}
	if got := refs(w, "r"); got != 70 {
		t.Errorf("routes for the group's members = %d, want all 70", got)
	}
	if got, gone := refs(w, "k"), refs(w, "zz-gone"); got != 0 || gone != 0 {
		t.Errorf("routes for %d servers no slot uses and %d for a missing member", got, gone)
	}
	if got := len(w.Pool(domain.GroupExit("second")).members); got != 70 {
		t.Errorf("the group allows %d, want 70", got)
	}
	if got := w.slotsReferencing(map[domain.GroupID]bool{"second": true}, nil, nil); !slices.Equal(got, []domain.SlotID{"b"}) {
		t.Errorf("a slot on the group doesn't block its delete: %v", got)
	}
	if got := w.Pool(domain.GroupExit("missing")); got.fixed || len(got.members) != 0 || got.empty != domain.NoUsableServer {
		t.Errorf("a missing group: %+v", got)
	}
}

func TestAnExitChangeLeavesUniFiAlone(t *testing.T) {
	r, s, pass := clientRig(t)
	r.seed(lineage(t, "b", "s", "DE", "b.example"))
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", poolIntent(t, "EU"))
	pass()
	r.settle()
	key, passes := r.c.wantsKey, len(s.lastPass(t))
	exits := []domain.Exit{domain.ServerExit("a"), domain.ServerExit("b"), domain.GroupExit(sourceGroup("s")), domain.ServerExit("a")}
	for _, exit := range exits {
		r.setExit("eu", exit)
		r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
		if r.c.wantsKey != key {
			t.Fatalf("an Exit change to %+v changed what UniFi is asked:\n%s\nwas\n%s", exit, r.c.wantsKey, key)
		}
	}
	if got := len(s.lastPass(t)); got != passes {
		t.Fatalf("%d wants, were %d", got, passes)
	}
	if r.server("eu") != "a" {
		t.Fatalf("the last Exit should have taken the slot to a, and it serves %v", r.server("eu"))
	}
}

func TestBlockKeepsTheVPNClientWantedAndStopsWatching(t *testing.T) {
	r, s, pass := clientRig(t)
	r.c.evidence.epoch = "e1"
	assigned := lineage(t, "only-assigned", "other", "SE", "o.example")
	r.seed(assigned)
	exit := domain.ServerExit("only-assigned")
	r.putSlot("eu", exitIntent(t, "EU", exit))
	pass()
	r.frame(domain.WANUp, r.probe("only-assigned", domain.Succeeded, 10))
	r.reportApplied("eu")
	r.settle()
	key := r.c.wantsKey
	bound := s.lastPass(t)[0]

	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: r.c.state.slots["eu"].Slot.Version, Intent: exitIntent(t, "EU", domain.BlockExit())}); err != nil {
		t.Fatal(err)
	}
	r.frame(domain.WANUp, r.probe("only-assigned", domain.Succeeded, 10))
	if got := r.c.state.slots["eu"].Selection; !got.IsBlocked() {
		t.Fatalf("selection %s", describe(got))
	}
	if r.c.wantsKey != key {
		t.Fatalf("Block changed what UniFi is asked:\n%s\nwas\n%s", r.c.wantsKey, key)
	}
	r.c.wantsKey = ""
	pass()
	again := s.lastPass(t)
	if len(again) != 1 || again[0].Slot != "eu" || again[0].Name != bound.Name || again[0].PublicKey != bound.PublicKey {
		t.Fatalf("a blocked slot's client is no longer wanted: %+v", again)
	}
	if got := r.c.state.slots["eu"].Slot.Allocation.NetworkID; got == "" {
		t.Fatal("a blocked slot lost its client")
	}
	if target := r.targetOf("eu"); !target.Selection.IsBlocked() {
		t.Fatalf("the plan doesn't drop: %+v", target.Selection)
	}
	if _, routed := r.c.world.routes[r.ref("only-assigned")]; routed {
		t.Error("a blocked slot's old assignment still has a probe route")
	}
	if obs, _ := r.c.observation(); len(obs.Beats) != 0 {
		t.Errorf("a blocked slot still gets heartbeats: %v", obs.Beats)
	}
	view := r.app.Snapshot().Slots[0]
	if view.Status != "block" || view.DownReason != nil {
		t.Errorf("view: %s %v", view.Status, view.DownReason)
	}
	if events := r.switches(); events[0].Reason != "exitChanged" {
		t.Errorf("reason %s", events[0].Reason)
	}

	if _, err := r.c.accept(context.Background(), PutSlot{ID: "eu", Expected: r.c.state.slots["eu"].Slot.Version, Intent: exitIntent(t, "EU", exit)}); err != nil {
		t.Fatal(err)
	}
	r.reportApplied("eu")
	r.settle()
	r.frame(domain.WANUp, r.probe("only-assigned", domain.Succeeded, 10))
	if r.server("eu") != "only-assigned" {
		t.Fatalf("coming back left %v", r.c.state.slots["eu"].Selection)
	}
	if events := r.switches(); events[0].Reason != "exitChanged" {
		t.Errorf("reason %s", events[0].Reason)
	}
}

// pendingMove takes slot eu from a to b, and leaves that unapplied.
func pendingMove(t *testing.T, r *rig) domain.EventID {
	t.Helper()
	electA(t, r)
	r.c.evidence.epoch = "e1"
	r.setExit("eu", domain.ServerExit("b"))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20))
	if r.server("eu") != "b" {
		t.Fatalf("serving %v", r.server("eu"))
	}
	return r.operation("eu")
}

func TestAStaleReportCantMarkASupersededSwitchApplied(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	first := pendingMove(t, r)
	r.setExit("eu", domain.ServerExit("a"))
	r.settle()
	second := r.operation("eu")
	if second == first || second == "" || r.server("eu") != "a" {
		t.Fatalf("no second operation: %v, serving %v", second, r.server("eu"))
	}

	// The core never left a, so a report about the first move reads exactly like the second's target.
	stale := gateway.Report{Generation: r.c.state.runtimeGeneration, Topology: gateway.Topology{Epoch: "e1", CorePID: 7, Generation: r.c.state.runtimeGeneration},
		Slots: []gateway.SlotReport{{Slot: "eu", Phase: gateway.Ready, Applied: domain.Serving(r.ref("a")), Operation: first}}}
	if err := r.c.applyReport(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if r.operation("eu") != second {
		t.Fatal("a report for a superseded move marked the new switch applied")
	}
	if r.c.last.Topology.CorePID != 7 {
		t.Fatal("a report for a superseded move didn't update what is observed")
	}
	for _, e := range r.switches() {
		if string(e.ID) == string(second) && e.AppliedAt != nil {
			t.Fatal("the stored event says applied")
		}
	}

	stale.Slots[0].Operation = second
	if err := r.c.applyReport(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if r.operation("eu") != "" {
		t.Fatal("the report for the switch's own operation didn't apply it")
	}
}

func TestAPendingMovesPriorIsWhatTheCoreLastReported(t *testing.T) {
	r := newRig(t, t.TempDir())
	pendingMove(t, r)
	movedAt := r.clock
	target := r.targetOf("eu")
	if from, until, ok := target.Passage.Allows(target.Selection); !ok || from != r.ref("a") || !until.Equal(movedAt.Add(r.c.timings.Handover)) {
		t.Fatalf("passage: %+v", target.Passage)
	}
	if target.Selection != domain.Serving(r.ref("b")) || target.Operation != r.operation("eu") {
		t.Fatalf("target: %+v", target)
	}

	r.seed(lineage(t, "c", "good", "DE", "198.51.100.30"))
	r.clock = r.clock.Add(5 * time.Second)
	r.setExit("eu", domain.ServerExit("c"))
	r.frame(domain.WANUp, r.probe("b", domain.Succeeded, 20), r.probe("c", domain.Succeeded, 30))
	target = r.targetOf("eu")
	from, until, ok := target.Passage.Allows(target.Selection)
	if r.server("eu") != "c" || !ok || from != r.ref("a") {
		t.Fatalf("a second move must still name what the core last reported, not the unlanded b: %+v", target.Passage)
	}
	if !until.Equal(movedAt.Add(r.c.timings.Handover)) {
		t.Fatalf("a superseding move extended what the core may stay on: until %v", until)
	}

	r.reportApplied("eu")
	if got := r.targetOf("eu"); got.Passage != (gateway.Passage{}) || got.Operation != "" {
		t.Fatalf("a landed move kept its passage: %+v", got.Passage)
	}
	if got := r.memory("eu").LastServed; got != r.ref("c") {
		t.Fatalf("last served: %v", got)
	}

	rotated := lineage(t, "c", "good", "DE", "198.51.100.30")
	rotated.Ref.Digest = domain.Digest{9, 9}
	r.seed(rotated)
	r.settle()
	target = r.targetOf("eu")
	from, _, ok = target.Passage.Allows(target.Selection)
	if !ok || from != (domain.ServerRef{Lineage: "c", Digest: lineage(t, "c", "good", "DE", "x").Ref.Digest}) {
		t.Fatalf("a rotated prior: %+v", target.Passage)
	}
	plan, _, _ := r.c.desired()
	if slices.Contains(plan.Servers(), from) {
		t.Error("a rotated prior's closure can't be in the plan, since the catalogue no longer holds it")
	}
}

func TestAMoveFromAClosedSlotHasNoPrior(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"))
	r.c.evidence.epoch = "e1"
	r.putSlot("eu", elected(t, "good"))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10))
	if target := r.targetOf("eu"); target.Passage != (gateway.Passage{}) || target.Operation == "" {
		t.Fatalf("an initial selection: %+v", target)
	}
}

func TestARefusedServerIsACapabilityExclusion(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.setExit("eu", domain.GroupExit(sourceGroup("good")))
	r.putSlot("assigned", exitIntent(t, "Assigned", domain.ServerExit("a")))
	report := gateway.Report{Generation: r.c.state.runtimeGeneration, Topology: gateway.Topology{Epoch: "e1"},
		Refused: []gateway.RefusedServer{{Server: r.ref("a"), Reason: "unknown cipher"}}}
	if err := r.c.applyReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	r.frame(domain.WANUp, r.probe("b", domain.Succeeded, 20))
	r.reportApplied("eu")
	r.reportApplied("assigned")
	if err := r.c.applyReport(ctx, report); err != nil {
		t.Fatal(err)
	}

	if l, _ := r.c.world.catalogue.Lookup("a"); l.Presence != domain.Present {
		t.Fatalf("a refusal rewrote presence: %v", l.Presence)
	}
	if r.server("eu") != "b" {
		t.Fatalf("a group slot stayed on a refused server: %v", r.server("eu"))
	}
	if got, down := r.c.state.slots["assigned"].Selection.Down(); !down || got != domain.ServerRefused {
		t.Fatalf("a server Exit on a refused server: %v %v", got, down)
	}
	page, err := r.app.Servers(ctx, PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.ID == "a" && (item.Supported || item.Presence != "present") {
			t.Errorf("a's row: %+v", item)
		}
	}
	plan, key, err := r.c.desired()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Servers(), r.ref("a")) {
		t.Error("a refused server left the plan, so the core can no longer report it and it flips back")
	}
	if err := r.c.applyReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if _, again, _ := r.c.desired(); again != key {
		t.Errorf("the plan flipped:\n%s\nwas\n%s", again, key)
	}
	if err := r.c.applyReport(ctx, gateway.Report{Generation: r.c.state.runtimeGeneration, Topology: gateway.Topology{Epoch: "e1"}}); err != nil {
		t.Fatal(err)
	}
	if !r.c.world.supported("a") {
		t.Error("a server the core no longer refuses stayed excluded")
	}
}

func TestAnExitMustBeUsableWhenItIsChosen(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	gone := lineage(t, "gone", "good", "DE", "g.example")
	gone.Presence = domain.Tombstone
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"), gone)
	put := func(exit domain.Exit) error {
		_, err := r.c.accept(ctx, PutSlot{ID: "s", Intent: exitIntent(t, "S", exit)})
		return err
	}
	r.wantRefused(put(domain.GroupExit("no-such-group")), KindValidation, RefusedGroupGone)
	r.wantRefused(put(domain.ServerExit("no-such-server")), KindValidation, RefusedServerGone)
	r.wantRefused(put(domain.ServerExit("gone")), KindValidation, RefusedServerGone)
	if err := put(domain.ServerExit("a")); err != nil {
		t.Fatal(err)
	}
}

func TestReferencedSourcesAndGroupsRefuseDeletionNamingTheSlot(t *testing.T) {
	h, server := newPageDaemon(t)
	app, _ := h.current()
	version := app.Snapshot().Slots[0].Version
	wantReferenced := func(what string, got answer) {
		t.Helper()
		if got.status != http.StatusConflict || !strings.Contains(got.body, `"kind":"referenced"`) || !strings.Contains(got.body, "EU") {
			t.Errorf("%s: %+v", what, got)
		}
	}
	group := sourceGroup("aurora")
	for _, s := range app.Snapshot().Sources {
		if s.ID == "aurora" && (!slices.Equal(s.UsedBy, []domain.SlotID{"eu"}) || !slices.Equal(s.Removes, []domain.GroupID{group})) {
			t.Errorf("the view names what refuses the delete and what goes with it: usedBy %v, removes %v", s.UsedBy, s.Removes)
		}
	}
	wantReferenced("deleting a source a slot's group would go with", mutate(t, server.URL, http.MethodDelete, "api/sources/aurora", "d1", nil, ""))
	wantReferenced("deleting a group a slot uses", mutate(t, server.URL, http.MethodDelete, "api/groups/"+string(group), "d3", nil, ""))

	rule, _ := json.Marshal(membersDTO(picked))
	owner := `{"name":"Owner's","members":` + string(rule) + `,"ticked":["l-ams","l-fra"]}`
	if created := mutate(t, server.URL, http.MethodPut, "api/groups/mine", "d4", map[string]string{"If-None-Match": "*"}, owner); created.status != http.StatusOK {
		t.Fatalf("create: %+v", created)
	}
	exit := `{"name":"EU","exit":{"kind":"group","group":"mine"}}`
	if moved := mutate(t, server.URL, http.MethodPut, "api/slots/eu", "d5", map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version)}, exit); moved.status != http.StatusOK {
		t.Fatalf("moving the Exit: %+v", moved)
	}
	wantReferenced("deleting an owner's group a slot uses", mutate(t, server.URL, http.MethodDelete, "api/groups/mine", "d6", nil, ""))

	assigned := `{"name":"EU","exit":{"kind":"server","server":"l-ams"}}`
	if moved := mutate(t, server.URL, http.MethodPut, "api/slots/eu", "d7", map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version+1)}, assigned); moved.status != http.StatusOK {
		t.Fatalf("assigning a server: %+v", moved)
	}
	wantReferenced("deleting a source whose server a paused slot is assigned", mutate(t, server.URL, http.MethodDelete, "api/sources/aurora", "d8", nil, ""))
	if deleted := mutate(t, server.URL, http.MethodDelete, "api/groups/mine", "d9", nil, ""); deleted.status != http.StatusOK {
		t.Errorf("deleting an unused group: %+v", deleted)
	}
	recreated := mutate(t, server.URL, http.MethodPut, "api/groups/mine", "d10", map[string]string{"If-None-Match": "*"}, owner)
	if !strings.Contains(recreated.body, `"revision":2`) {
		t.Errorf("a group's revisions must carry on past a delete: %+v", recreated)
	}
}

func TestDeletingASourceDecidesEachGroup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r := newRig(t, dir)
	r.seed(lineage(t, "x1", "x", "DE", "x.example"), lineage(t, "y1", "y", "NL", "y.example"))
	only := ruleMembers(t, rule{Sources: []domain.SourceID{"x"}})
	both := ruleMembers(t, rule{Sources: []domain.SourceID{"x", "y"}})
	r.putGroup("only-x", "Only x", only)
	r.putGroup("x-and-y", "Both", both)
	r.putPicked("list-x", "List of x", "x1")
	r.putPicked("list-both", "List of both", "x1", "y1")
	r.putGroup("both-less-x1", "Both less x1", boxed(both, domain.BoxUnticked))
	r.tick("both-less-x1", "x1")
	r.putSlot("survivor", exitIntent(t, "Survivor", domain.GroupExit("x-and-y")))
	revision := r.c.world.groups["x-and-y"].Revision

	if _, err := r.c.accept(ctx, DeleteSource{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.GroupID{"only-x", "list-x", sourceGroup("x")} {
		if _, kept := r.c.world.groups[id]; kept {
			t.Errorf("%s survived, and a rule with no source would widen to any", id)
		}
	}
	rewritten := r.c.world.groups["x-and-y"]
	if sources := rewritten.Members.Sources.Values; !slices.Equal(sources, []domain.SourceID{"y"}) || rewritten.Revision != revision+1 {
		t.Errorf("rewritten: %+v at %d", sources, rewritten.Revision)
	}
	if got := r.c.world.members["x-and-y"]; len(got) != 1 || got[0].ID != "y1" {
		t.Errorf("members widened or lost: %+v", got)
	}
	if ids := r.c.world.groups["list-both"].Ticked; !slices.Equal(ids, []domain.LineageID{"y1"}) {
		t.Errorf("list: %v", ids)
	}
	if out := r.c.world.groups["both-less-x1"]; len(out.Ticked) != 0 || !slices.Equal(out.Members.Sources.Values, []domain.SourceID{"y"}) {
		t.Errorf("a tick of the deleted source's server stayed: %+v", out)
	}
	if r.c.state.slots["survivor"].Slot.Version != 1 {
		t.Error("a rewrite moved a slot's Version")
	}

	_, recovered, err := r.c.store.recover(ctx, r.clock, defaultTimings())
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := recovered.groups["only-x"]; kept || recovered.groups["x-and-y"].Revision != revision+1 {
		t.Errorf("the delete isn't durable: %+v", recovered.groups)
	}

	put := func(members domain.Members) error {
		_, err := r.c.accept(ctx, PutGroup{ID: "editor", Name: "Editor", Members: members})
		return err
	}
	r.wantRefused(put(ruleMembers(t, rule{Sources: []domain.SourceID{"x"}})), KindValidation, RefusedSourcesGone)
	r.putGroup("ticks", "Ticks", ruleMembers(t, rule{Sources: []domain.SourceID{"y"}}))
	tick := func(l domain.LineageID, on bool) error {
		_, err := r.c.accept(ctx, TickServer{Group: "ticks", Server: l, Ticked: on})
		return err
	}
	r.wantRefused(tick("x1", true), KindValidation, RefusedServerGone)
	r.wantRefused(tick("never-seen", true), KindValidation, RefusedServerGone)
	r.wantRefused(func() error {
		_, err := r.c.accept(ctx, TickServer{Group: "nobody", Server: "y1", Ticked: true})
		return err
	}(), KindNotFound, RefusedGroupGone)
	if err := tick("never-seen", false); err != nil {
		t.Fatalf("unticking what isn't ticked should change nothing: %v", err)
	}
	if err := put(ruleMembers(t, rule{Sources: []domain.SourceID{"x", "y"}})); err != nil {
		t.Fatalf("a clause dropped from several sources should be silent: %v", err)
	}
	if stored := r.c.world.groups["editor"].Members.Sources.Values; !slices.Equal(stored, []domain.SourceID{"y"}) {
		t.Errorf("stored %v, a stored group must never name a removed source", stored)
	}
}

// A tick leaves the group's slots alone while the box condition is unset. The rule edit that sets it
// to "not ticked" moves them off the ticked server, as any rule edit that drops a server does, and the
// tick and the condition come back after a restart.
func TestATickMovesNothingUntilTheBoxConditionDoes(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	twoSlotsOnOneGroup(t, r)
	german := ruleMembers(t, rule{Countries: []string{"DE"}})
	r.putGroup("g", "Group", german)
	r.settle()
	on := r.server("one")
	one, two := r.c.state.slots["one"].Slot.Version, r.c.state.slots["two"].Slot.Version
	revision := r.c.world.groups["g"].Revision
	moves := len(r.switches())

	r.tick("g", on)
	r.settle()
	if r.server("one") != on || len(r.switches()) != moves || r.c.world.groups["g"].Revision != revision {
		t.Fatalf("a tick with the box unset moved the slot to %v, or the group to revision %d", r.server("one"), r.c.world.groups["g"].Revision)
	}
	r.putGroup("g", "Group", boxed(german, domain.BoxUnticked))
	r.settle()
	if r.server("one") == on || r.server("one") == "" || r.server("two") != r.server("one") {
		t.Fatalf("after %s's box ruled it out: one on %v, two on %v", on, r.server("one"), r.server("two"))
	}
	if got := len(r.switches()) - moves; got != 1 {
		t.Fatalf("%d switches for one move of the group", got)
	}
	if r.c.state.slots["one"].Slot.Version != one || r.c.state.slots["two"].Slot.Version != two {
		t.Fatal("a box condition moved a slot's Version")
	}

	r.close()
	r = newRigAt(t, dir, r.clock)
	if g := r.c.world.groups["g"]; !slices.Equal(g.Ticked, []domain.LineageID{on}) || g.Members.Box != domain.BoxUnticked {
		t.Fatalf("after a restart the group is %+v", g)
	}
	if slices.ContainsFunc(r.c.world.members["g"], func(l domain.Lineage) bool { return l.ID == on }) {
		t.Fatalf("after a restart %s is back in the group", on)
	}
}

// A tick names a lineage, so the source's next refresh keeps it.
func TestATickStaysAcrossARefresh(t *testing.T) {
	r := newRig(t, t.TempDir())
	remoteSource(t, r, "acme")
	refreshed(t, r.c, "acme", parsedAcme(t, acmeText(t)), r.clock)
	r.c.publish(r.clock)
	held := r.c.world.catalogue.Source("acme")
	if len(held) < 2 {
		t.Fatalf("acme lists %d servers", len(held))
	}
	out := held[0].ID
	r.putGroup("g", "Group", boxed(ruleMembers(t, rule{Sources: []domain.SourceID{"acme"}}), domain.BoxUnticked))
	r.tick("g", out)
	refreshed(t, r.c, "acme", parsedAcme(t, acmeText(t)), r.clock.Add(time.Hour))
	r.c.publish(r.clock.Add(time.Hour))
	members := r.c.world.members["g"]
	if len(members) == 0 || slices.ContainsFunc(members, func(l domain.Lineage) bool { return l.ID == out }) {
		t.Fatalf("after a refresh the group holds %v, %s ticked out", ids(members), out)
	}
}

// A group an earlier build stored keeps its conditions and reads its hand lists as nothing, so a group
// of picks alone matches nothing.
func TestAStoredGroupLosesItsHandLists(t *testing.T) {
	for body, want := range map[string]domain.Members{
		`{"name":"Rule","members":{"sources":{"values":["s"]},"countries":{},"protocols":{},"removed":["a"]}}`: ruleMembers(t, rule{Sources: []domain.SourceID{"s"}}),
		`{"name":"Picks","members":{"sources":{},"countries":{},"protocols":{},"added":["a","b"]}}`:            ruleMembers(t, rule{}),
	} {
		g, err := decodeGroup("g", 1, []byte(body))
		if err != nil || len(g.Ticked) != 0 || g.Members.Box != domain.BoxUnset || !slices.Equal(g.Members.Sources.Values, want.Sources.Values) || g.Members.Conditioned() != want.Conditioned() {
			t.Errorf("%s read as %+v, %v", body, g, err)
		}
	}
}

func TestPutGroupRefusals(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "a", "good", "DE", "198.51.100.10"))
	members := ruleMembers(t, rule{Countries: []string{"DE"}})
	put := func(cmd PutGroup) error {
		_, err := r.c.accept(ctx, cmd)
		return err
	}
	r.wantRefused(put(PutGroup{ID: "mine", Members: members}), KindValidation, RefusedNoName)
	r.wantRefused(put(PutGroup{ID: "mine", Name: "Mine", Members: ruleMembers(t, rule{})}), KindValidation, RefusedNoRule)
	r.wantRefused(put(PutGroup{ID: "mine", Name: "Mine", Members: ruleMembers(t, rule{}), Ticked: []domain.LineageID{"a"}}), KindValidation, RefusedNoRule)
	r.wantRefused(put(PutGroup{ID: "mine", Name: "Mine", Members: picked}), KindValidation, RefusedNoRule)
	r.wantRefused(put(PutGroup{ID: "mine", Name: "Mine", Members: picked, Ticked: []domain.LineageID{"never-seen"}}), KindValidation, RefusedServerGone)
	boxed := members
	boxed.Box = domain.BoxTicked
	r.wantRefused(put(PutGroup{ID: "mine", Name: "Mine", Members: boxed}), KindValidation, RefusedNoRule)
	r.wantRefused(put(PutGroup{ID: "mine", Name: "Mine", Members: picked, Ticked: []domain.LineageID{"a", "a"}}), KindValidation, RefusedMalformed)
	r.wantRefused(put(PutGroup{ID: "bad id", Name: "Mine", Members: members}), KindValidation, RefusedBadID)
	r.wantRefused(put(PutGroup{ID: "mine", Expected: 3, Name: "Mine", Members: members}), KindNotFound, RefusedGroupGone)
	if err := put(PutGroup{ID: "mine", Name: "Mine", Members: members}); err != nil {
		t.Fatal(err)
	}
	r.wantRefused(put(PutGroup{ID: "mine", Name: "Mine", Members: members}), KindStale, RefusedStale)
	r.wantRefused(put(PutGroup{ID: "mine", Expected: 9, Name: "Mine", Members: members}), KindStale, RefusedStale)
	r.wantRefused(put(PutGroup{ID: "mine", Expected: 1, Name: "Mine", Members: members, Ticked: []domain.LineageID{"a"}}), KindValidation, RefusedMalformed)
	if _, err := r.c.accept(ctx, DeleteGroup{ID: "mine", Expected: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := r.c.accept(ctx, DeleteGroup{ID: "mine"})
	r.wantRefused(err, KindNotFound, RefusedGroupGone)
}

func TestARefreshMakesNoGroup(t *testing.T) {
	r := newRig(t, t.TempDir())
	remoteSource(t, r, "acme")
	refreshed(t, r.c, "acme", parsedAcme(t, acmeText(t)), r.clock)
	r.c.publish(r.clock)
	if groups := r.app.Snapshot().Groups; len(groups) != 0 {
		t.Errorf("groups = %+v", groups)
	}
	if len(r.c.world.routes) != 0 {
		t.Errorf("%d probe routes with no slot on the source", len(r.c.world.routes))
	}
	obs, _ := r.c.observation()
	if len(obs.Sources) != 0 {
		t.Errorf("%d servers to probe with no slot on the source", len(obs.Sources))
	}
}

func TestTheModelSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	pendingMove(t, r)
	movedAt := r.clock
	r.putGroup("mine", "Mine", ruleMembers(t, rule{Countries: []string{"de"}, Names: mustFilter(t, "a", "z")}))
	m := r.memory("eu")
	m.LeaveBy = r.clock.Add(time.Hour)
	owner, _ := ownerOf(r.c.state.slots["eu"])
	if err := r.c.store.commit(context.Background(), transaction{Owners: map[domain.Owner]policyMemory{owner: m}}); err != nil {
		t.Fatal(err)
	}
	r.close()

	r2 := newRig(t, dir)
	got := r2.c.state.slots["eu"]
	if exit, ok := got.Slot.Intent.Exit().Server(); !ok || exit != "b" {
		t.Errorf("exit: %+v", got.Slot.Intent.Exit())
	}
	if got.LastServed.Lineage != "a" {
		t.Errorf("last served: %+v", got.LastServed)
	}
	if leaveBy := r2.memory("eu").LeaveBy; leaveBy.After(t0.Add(r2.c.timings.Handover)) {
		t.Errorf("a restart granted LeaveBy more time: %v", leaveBy)
	}
	var pending domain.Switch
	for _, p := range r2.c.state.pending {
		pending = p
	}
	if !pending.AllowUntil.Equal(movedAt.Add(r2.c.timings.Handover)) {
		t.Errorf("allow until: %v", pending.AllowUntil)
	}
	mine := r2.c.world.groups["mine"]
	include, exclude := mine.Members.Names.Patterns()
	if mine.Name != "Mine" || mine.Revision != 1 || !slices.Equal(mine.Members.Countries.Values, []string{"DE"}) || include != "a" || exclude != "z" {
		t.Errorf("group: %+v", mine)
	}
	if len(r2.c.world.uses) != 1 || r2.c.world.uses[0].Slot != "eu" {
		t.Errorf("uses: %+v", r2.c.world.uses)
	}
}

func mustFilter(t *testing.T, include, exclude string) domain.Filter {
	t.Helper()
	f, err := domain.NewFilter(include, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// BenchmarkNewWorld measures the rebuild every model change pays, at the feed limit, on rules whose
// filters are compiled, with every slot on a different group.
func BenchmarkNewWorld(b *testing.B) {
	server := domain.Server{}
	lineages := make([]domain.Lineage, 0, 2000)
	countries := []string{"DE", "NL", "SE", "FI", "US", "GB", "FR", "PL", "CH", "AT"}
	for i := range 2000 {
		id := domain.NewLineageID("s", 1, i)
		code := countries[i%len(countries)]
		lineages = append(lineages, domain.Lineage{ID: id, Source: domain.SourceID(fmt.Sprintf("s%d", i%4)), Label: fmt.Sprintf("%s server %d", code, i),
			Ref: domain.ServerRef{Lineage: id, Digest: domain.Digest{byte(i), byte(i >> 8)}}, Server: server, Country: domain.Country{Code: code, Source: domain.CountryTagFlag},
			Presence: domain.Present, Seen: []domain.Occurrence{{Config: id, ConfigName: "config", Tag: "tag", Position: 1 + i%2}}})
	}
	names, err := domain.NewFilter("server", "EDGE")
	if err != nil {
		b.Fatal(err)
	}
	catalogue := domain.NewCatalogue(1, lineages)
	groups := map[domain.GroupID]domain.Group{}
	var uses []exitUse
	for i, code := range countries {
		id := domain.GroupID("country-" + code)
		m, err := domain.AdmitMembers(asMembers(rule{Countries: []string{code}, Names: names}))
		if err != nil {
			b.Fatal(err)
		}
		groups[id] = domain.Group{ID: id, Members: m}
		if i < domain.MaxSlots {
			uses = append(uses, exitUse{Slot: domain.SlotID(code), Exit: domain.GroupExit(id)})
		}
	}
	for i := range 4 {
		id := sourceGroup(domain.SourceID(fmt.Sprintf("s%d", i)))
		m, err := domain.AdmitMembers(asMembers(rule{Sources: []domain.SourceID{domain.SourceID(fmt.Sprintf("s%d", i))}}))
		if err != nil {
			b.Fatal(err)
		}
		groups[id] = domain.Group{ID: id, Members: m}
	}
	b.ReportAllocs()
	for b.Loop() {
		newWorld(catalogue, groups, nil, uses)
	}
	b.StopTimer()
	w := newWorld(catalogue, groups, nil, uses)
	want := 0
	for _, use := range uses {
		id, _ := use.Exit.Group()
		want += len(w.members[id])
	}
	if want == 0 || len(w.routes) != want {
		b.Fatalf("%d routes for %d members of the groups in use", len(w.routes), want)
	}
}

// Is none of a deleted source excludes nothing that's left, so a group of everything but that source
// keeps everything else.
func TestDeletingTheOnlySourceAGroupExcludesKeepsTheRest(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.seed(lineage(t, "x1", "x", "DE", "x.example"), lineage(t, "y1", "y", "NL", "y.example"))
	members, err := domain.AdmitMembers(domain.Members{Sources: domain.Among[domain.SourceID]{Not: true, Values: []domain.SourceID{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	r.putGroup("not-x", "Everything but x", members)
	r.putSlot("stay", exitIntent(t, "Stay", domain.GroupExit("not-x")))
	r.frame(domain.WANUp, r.probe("y1", domain.Succeeded, 10))
	r.reportApplied("stay")
	events := len(r.switches())
	if _, err := r.c.accept(context.Background(), DeleteSource{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	r.frame(domain.WANUp, r.probe("y1", domain.Succeeded, 10))
	if got := r.c.world.members["not-x"]; len(got) != 1 || got[0].ID != "y1" {
		t.Errorf("members after deleting x: %+v", got)
	}
	if r.server("stay") != "y1" || len(r.switches()) != events {
		t.Fatalf("the slot moved to %s with %d events, were %d", describe(r.c.state.slots["stay"].Selection), len(r.switches()), events)
	}
}
