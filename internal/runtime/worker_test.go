package runtime

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// slowTest keeps real waits out of the quick check, which is -short. The full run keeps them.
func slowTest(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("real waits: the full, non-short run covers this")
	}
}

func workerSlot(id domain.SlotID, ordinal uint8) domain.Slot {
	return domain.Slot{ID: id, Allocation: domain.Allocation{Ordinal: ordinal, PeerListenPort: 12000 + uint16(ordinal)*2, TPROXYPort: 12001 + uint16(ordinal)*2,
		Tunnel: netip.MustParsePrefix("10.200.0.0/30"), Keys: domain.SlotKeys{ClientPublic: "client"}}}
}

func workerTarget(slot domain.Slot, selection domain.Selection, passage Passage) SlotTarget {
	return SlotTarget{Slot: slot, Selection: selection, Passage: passage, Keys: KeyMaterial{Host: domain.NewSecret("host")}}
}

func book(judged map[domain.ServerRef]verdict) *verdicts { return &verdicts{of: judged} }

func TestAServerWithNoVerdictGetsNoRouteUntilJudged(t *testing.T) {
	waiting, judged, refused := testRef("waiting"), testRef("judged"), testRef("refused")
	servers := map[domain.ServerRef]domain.Server{
		waiting: testServer(t, vlessHop(t, "root", "198.51.100.1", "")),
		judged:  testServer(t, vlessHop(t, "root", "198.51.100.2", "")),
		refused: testServer(t, vlessHop(t, "root", "198.51.100.3", "")),
	}
	now := backoffStart
	verdicts := book(map[domain.ServerRef]verdict{judged: {state: verdictAccepted}, refused: {state: verdictRefused, reason: "bad key"}})
	slot := workerSlot("slot1", 1)
	plan := func(selection domain.Selection, passage Passage) Plan {
		return Plan{slots: []SlotTarget{workerTarget(slot, selection, passage)}, servers: servers}
	}
	onPrior := MoveFrom

	cp := (&Worker{}).corePlan(plan(domain.Serving(judged), Passage{}), now, verdicts)
	if !slices.Equal(cp.Probes, []domain.ServerRef{judged}) || len(cp.Servers) != 1 {
		t.Errorf("routes for what isn't judged: probes %v, servers %d", cp.Probes, len(cp.Servers))
	}
	if _, kept := cp.Waiting[waiting]; !kept || len(cp.Waiting) != 1 {
		t.Errorf("waiting %v", cp.Waiting)
	}

	cases := []struct {
		name      string
		selection domain.Selection
		passage   Passage
		serves    *domain.ServerRef
	}{
		{"a judged selection serves itself", domain.Serving(judged), Passage{}, &judged},
		{"an unjudged selection serves its live prior", domain.Serving(waiting), onPrior(judged, now.Add(time.Second)), &judged},
		{"a refused selection serves its live prior", domain.Serving(refused), onPrior(judged, now.Add(time.Second)), &judged},
		{"an unjudged selection with no prior stays closed", domain.Serving(waiting), Passage{}, nil},
		{"a refused selection with no prior stays closed", domain.Serving(refused), Passage{}, nil},
		{"a prior past its deadline serves nothing", domain.Serving(waiting), onPrior(judged, now), nil},
		{"a prior the plan doesn't hold serves nothing", domain.Serving(waiting), onPrior(testRef("rotated"), now.Add(time.Second)), nil},
		{"an unjudged prior serves nothing", domain.Serving(refused), onPrior(waiting, now.Add(time.Second)), nil},
	}
	for _, c := range cases {
		w := &Worker{}
		got := w.corePlan(plan(c.selection, c.passage), now, verdicts)
		switch {
		case c.serves == nil && len(got.Slots) != 0:
			t.Errorf("%s: serves %+v", c.name, got.Slots)
		case c.serves != nil && (len(got.Slots) != 1 || got.Slots[0].Selection != domain.Serving(*c.serves)):
			t.Errorf("%s: serves %+v, want %v", c.name, got.Slots, *c.serves)
		}
	}

	draining := testRef("drain")
	w := &Worker{drain: drains{leaving: map[domain.ServerRef]drained{draining: {until: now.Add(time.Minute), server: servers[judged]}}}}
	if got := w.corePlan(plan(domain.Serving(judged), Passage{}), now, verdicts); len(got.Draining) != 0 {
		t.Errorf("an unjudged server stayed draining: %v", got.Draining)
	}
}

func TestAReportOnAPriorKeepsTheMovePending(t *testing.T) {
	prior, next := testRef("prior"), testRef("next")
	w := &Worker{}
	slot := workerSlot("slot1", 1)
	plan := Plan{slots: []SlotTarget{workerTarget(slot, domain.Serving(next), MoveFrom(prior, backoffStart.Add(time.Minute)))}}
	core := coreObservation{Applied: map[domain.SlotID]domain.Selection{"slot1": domain.Serving(prior)}}
	report := w.finish(Report{At: backoffStart}, plan, corePlan{}, map[domain.ServerRef]bool{next: true}, core, kernelObs{links: map[string]link{"pxa1": {up: true}}}, &view{read: &core}, map[string]linkSample{"pxa1": {up: true, ours: true}}, false)
	got := report.Slots[0]
	if got.Applied != domain.Serving(prior) || got.Phase != Ready {
		t.Errorf("report %+v", got)
	}
	if got.ErrorCode != ErrorCodeServerRefused {
		t.Errorf("a refused selection reported %q, so leave wouldn't move the slot", got.ErrorCode)
	}
	if clean := w.finish(Report{At: backoffStart}, plan, corePlan{}, nil, core, kernelObs{}, &view{read: &core}, nil, false); clean.Slots[0].ErrorCode != "" {
		t.Errorf("an unjudged selection reported %q", clean.Slots[0].ErrorCode)
	}
}

func TestALiveOutboundIsntRemovedWhileItWaits(t *testing.T) {
	waiting := testRef("waiting")
	var removed []string
	x := &xray{api: fakeCore(t, func(method string, req []byte) ([]byte, string) {
		if method == handlerService+"RemoveOutbound" {
			tags, _ := tagsOf(pbBytes(nil, 1, req), 1, 1)
			removed = append(removed, tags...)
		}
		return nil, "0"
	})}
	server := testServer(t, vlessHop(t, "root", "198.51.100.1", ""))
	want := corePlan{Waiting: map[domain.ServerRef]domain.Server{waiting: server}}
	steps, err := passOn(x).hotApply(context.Background(), want, coreObservation{inbounds: []string{probeTag}, outbounds: []string{outboundTag(waiting), "o.gone"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"o.gone"}) {
		t.Errorf("removed %q, steps %q", removed, steps)
	}
	if want.empty() {
		t.Error("a plan with servers awaiting a verdict counts as empty, and that stops the core")
	}
}

func TestAdoptionVouchesOnlyForWhatAPinnedRunningCoreLoaded(t *testing.T) {
	live, fresh := testRef("live"), testRef("fresh")
	plan := Plan{servers: map[domain.ServerRef]domain.Server{live: {}, fresh: {}}}
	running := coreObservation{Running: true, outbounds: []string{outboundTag(live)}}
	if got := loadedBy(running, plan); len(got) != 1 {
		t.Errorf("adopted %v", got)
	}
	if _, judged := loadedBy(running, plan)[fresh]; judged {
		t.Error("a server the core never loaded counts as judged")
	}
	outdated := running
	outdated.Outdated = true
	if got := loadedBy(outdated, plan); len(got) != 0 {
		t.Errorf("an outdated core vouched for %v", got)
	}
	if got := loadedBy(coreObservation{outbounds: running.outbounds}, plan); len(got) != 0 {
		t.Errorf("a stopped core vouched for %v", got)
	}
}

// upgradedCore opens a Worker whose running core, pid 4242 under invocation e1, is an old binary an
// upgrade left running. It holds an outbound for every server of plan and slot1's inbounds. The
// pinned binary is a script that runs pinned for every call but `version`. When restartAt isn't zero,
// systemd replaces that core just before the API answers its restartAt'th inbound list: the new one,
// under invocation e2, holds none of them.
func upgradedCore(t *testing.T, plan Plan, pinned string, restartAt int) *Worker {
	t.Helper()
	dir := t.TempDir()
	epoch := filepath.Join(dir, "epoch")
	if err := os.WriteFile(epoch, []byte("e1"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := fakeTools(t, "#!/bin/sh\nprintf 'ActiveState=active\\nMainPID=4242\\nInvocationID=%s\\n' \"$(cat '"+epoch+"')\"\n", time.Hour)
	if err := os.MkdirAll(filepath.Dir(w.core.binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.core.binary, []byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo 'Xray 26.10.31 (Xray, Penetrates Everything.)'; exit 0; fi\n"+pinned+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "cores", "old", "xray")
	fakeXray(t, old, "25.12.8", filepath.Join(dir, "running.log"))
	w.core.proc = filepath.Join(dir, "proc")
	if err := os.MkdirAll(filepath.Join(w.core.proc, "4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, filepath.Join(w.core.proc, "4242", "exe")); err != nil {
		t.Fatal(err)
	}
	outbounds, inbounds := []string{blockTag, directTag}, inboundTags("slot1")
	for ref := range plan.servers {
		outbounds = append(outbounds, outboundTag(ref))
	}
	var mu sync.Mutex
	lists := 0
	w.core.api = fakeCore(t, func(method string, req []byte) ([]byte, string) {
		mu.Lock()
		defer mu.Unlock()
		var tags []string
		switch strings.TrimPrefix(method, handlerService) {
		case "ListOutbounds":
			tags = outbounds
		case "ListInbounds":
			if lists++; lists == restartAt {
				if err := os.WriteFile(epoch, []byte("e2"), 0o644); err != nil {
					t.Error(err)
				}
				outbounds, inbounds = []string{blockTag, directTag}, nil
			}
			tags = inbounds
		}
		var resp []byte
		for _, tag := range tags {
			resp = pbBytes(resp, 1, pbString(nil, 1, tag))
		}
		return resp, "0"
	})
	t.Cleanup(w.core.reads.Wait)
	return w
}

// An upgrade onto another core restarts the daemon first, under the old core. The pinned core has
// judged nothing the old one holds, so a slot on a server waits for its verdict, its gate closed. The
// servers the slots serve on are judged before the rest of the plan, so the wait is one config test of
// them however large the plan is, and the rest's verdicts follow.
func TestAnUpgradeJudgesWhatTheSlotsServeOnBeforeTheRestOfThePlan(t *testing.T) {
	marks := t.TempDir()
	rest := filepath.Join(marks, "rest")
	server := testRef("server")
	plan := Plan{generation: 1, slots: []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(server), Passage{})},
		servers: map[domain.ServerRef]domain.Server{server: ssServer(t, "good-key"), testRef("bad"): ssServer(t, "bad-secret")}}
	for i := range 20 {
		plan.servers[testRef("other"+strconv.Itoa(i))] = ssServer(t, "other-key-"+strconv.Itoa(i))
	}
	w := upgradedCore(t, plan, `if [ "$1" = run ]; then
	payload=$(cat)
	case "$payload" in *other-key*|*bad-secret*) touch `+rest+`; sleep 30;; esac
fi`, 0)
	ctx, cancel := context.WithCancel(context.Background())
	goals, reports := make(chan Plan, 1), make(chan Report, 64)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, goals, reports, nil) }()
	defer func() { cancel(); <-done }()
	go func() {
		for range reports {
		}
	}()
	goals <- plan
	for limit := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		config, _ := os.ReadFile(w.core.configPath)
		if strings.Contains(string(config), inboundTags("slot1")[0]) {
			break
		}
		if time.Now().After(limit) {
			t.Fatal("slot1 still waits after 3 s, for the verdicts of servers no slot serves on")
		}
	}
	if _, err := os.Stat(rest); err != nil {
		t.Errorf("the rest of the plan isn't being judged: %v", err)
	}
}

// The pinned core's judgment of what an old core holds runs in the judging actor, so a stalled one
// holds no pass: the first one still reconciles and reports, here with a Direct slot to serve.
func TestAStalledJudgeAfterAnUpgradeHoldsNoPass(t *testing.T) {
	marks := t.TempDir()
	started, finished := filepath.Join(marks, "started"), filepath.Join(marks, "finished")
	server := testRef("server")
	plan := Plan{generation: 1, servers: map[domain.ServerRef]domain.Server{server: ssServer(t, "good-key")},
		slots: []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(server), Passage{}), workerTarget(workerSlot("direct", 2), domain.Direct(), Passage{})}}
	w := upgradedCore(t, plan, "if [ \"$1\" = run ]; then touch "+started+"; sleep 30; touch "+finished+"; fi", 0)
	ctx, cancel := context.WithCancel(context.Background())
	goals, reports := make(chan Plan, 1), make(chan Report, 8)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, goals, reports, nil) }()
	defer func() { cancel(); <-done }()
	goals <- plan
	select {
	case r := <-reports:
		if r.Generation != 1 {
			t.Fatalf("first report is of generation %d", r.Generation)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no pass within 3 s while the pinned core judged what the old one holds")
	}
	if _, err := os.Stat(finished); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the pass came only after the judge finished")
	}
}

// Systemd can replace the core between a pass's read of the unit and its read of the API, so the API
// answers for a core the pass doesn't know. Whatever core it is, the pinned core is the one to load
// what a hot add gives it, so none is given a server the pinned core hasn't accepted: here slot1's,
// which it refuses.
func TestACoreReplacedMidReadIsGivenNothingThePinnedCoreHasntAccepted(t *testing.T) {
	given := filepath.Join(t.TempDir(), "given")
	bad, good := testRef("bad"), testRef("good")
	plan := Plan{generation: 1, slots: []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(bad), Passage{})},
		servers: map[domain.ServerRef]domain.Server{bad: ssServer(t, "bad-secret"), good: ssServer(t, "good-key")}}
	w := upgradedCore(t, plan, `case "$1:$2" in
run:-test) `+refusesBad+`;;
api:ado) cat >>`+given+`;;
esac`, 2)
	w.gates.plan = &plan
	ctx := context.Background()
	_, _ = w.pass(ctx, &plan)
	if added, _ := os.ReadFile(given); strings.Contains(string(added), "bad-secret") {
		t.Fatal("a hot add gave the replacement core a server the pinned core hasn't accepted")
	}
	if _, err := w.core.judge(ctx, plan.servers); err != nil {
		t.Fatal(err)
	}
	_, _ = w.pass(ctx, &plan)
	added, _ := os.ReadFile(given)
	if strings.Contains(string(added), "bad-secret") || !strings.Contains(string(added), "good-key") {
		t.Errorf("once judged, the replacement core was given %q, want the accepted server alone", added)
	}
}

// A slot moving onto a server the pinned core hasn't judged yet serves its live Prior once the pinned
// core accepts that, and the config a restart loads, the upgrade's own included, holds it there.
func TestTheConfigARestartLoadsServesASlotOnItsAcceptedPrior(t *testing.T) {
	prior, next := testRef("a-prior"), testRef("b-next")
	plan := Plan{generation: 1, slots: []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(next), MoveFrom(prior, time.Now().Add(time.Minute)))},
		servers: map[domain.ServerRef]domain.Server{prior: ssServer(t, "good-key"), next: ssServer(t, "next-key")}}
	w := upgradedCore(t, plan, `if [ "$1" = run ]; then
	payload=$(cat)
	case "$payload" in *good-key*next-key*|*next-key*good-key*) exit 23;; *next-key*) exit 1;; esac
fi`, 0)
	w.gates.plan = &plan
	ctx := context.Background()
	if _, err := w.core.judge(ctx, plan.servers); err == nil {
		t.Fatal("the pinned core judged b-next, which it could not test")
	}
	_, _ = w.pass(ctx, &plan)
	config, err := os.ReadFile(w.core.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), inboundTags("slot1")[0]) || !strings.Contains(string(config), outboundTag(prior)) {
		t.Fatalf("the config a restart loads doesn't serve slot1 on its accepted Prior:\n%s", config)
	}
	if strings.Contains(string(config), "next-key") {
		t.Error("the config holds b-next, which the pinned core hasn't accepted")
	}
}

func TestAnUnreadCoreKeepsEveryPlannedSlotWanted(t *testing.T) {
	server := testRef("server")
	dir := t.TempDir()
	systemctl := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	w, err := Open(Options{DataDir: t.TempDir(), RunDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	plan := Plan{slots: []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(server), Passage{})},
		servers: map[domain.ServerRef]domain.Server{server: testServer(t, vlessHop(t, "root", "198.51.100.1", ""))}}
	verdict, err := w.adopt(context.Background(), w.corePass(), plan)
	if err == nil || verdict != nil || w.adopted {
		t.Fatalf("adoption with an unreadable core: %v, %v, adopted %v", verdict, err, w.adopted)
	}
	cp := w.corePlan(plan, backoffStart, verdict)
	if len(cp.Slots) != 1 || len(cp.Waiting) != 0 || len(cp.Probes) != 1 {
		t.Errorf("an unread core changed the plan: %+v", cp)
	}
	g := gate{up: true, wanted: len(cp.Slots) == 1, kernelRead: false, stays: plan.slots[0].mayBeUp(backoffStart, w.gates.vouches("slot1"))}
	if !g.keep() || !g.open() {
		t.Error("the first pass after a restart closed a gate over an unreadable core")
	}
}

// An opening lands only on the newest plan, and only while its slot may be up on what its own
// confirmation read.
func TestAnOpeningLandsOnlyWhileItsSlotMayBeUp(t *testing.T) {
	server, next := testRef("server"), testRef("next")
	slot := workerSlot("slot1", 1)
	plan := func(selection domain.Selection, passage Passage) *Plan {
		return &Plan{slots: []SlotTarget{workerTarget(slot, selection, passage)}}
	}
	ahead, past := time.Now().Add(time.Minute), time.Now().Add(-time.Second)
	fakeLinks(t, map[string]int{"pxa1": linkGroup})
	opens := func(newest, planned *Plan, fresh domain.Selection) bool {
		var ups []string
		g := &gates{plan: newest, write: func(link string, up bool) error {
			if up {
				ups = append(ups, link)
			}
			return nil
		}}
		err := g.open(planned, "slot1", fresh)
		if (err == nil) != slices.Equal(ups, []string{"pxa1"}) {
			t.Errorf("open said %v and wrote %v", err, ups)
		}
		return err == nil
	}
	bridge, lapsed := plan(domain.Serving(server), BridgeUntil(ahead)), plan(domain.Serving(server), BridgeUntil(past))
	late := plan(domain.Serving(next), MoveFrom(server, past))
	if !opens(bridge, bridge, domain.Serving(server)) {
		t.Error("a bridge inside its deadline didn't open")
	}
	if opens(lapsed, lapsed, domain.Serving(server)) {
		t.Error("a bridge opened past its deadline")
	}
	if opens(late, bridge, domain.Serving(server)) {
		t.Error("an opening landed for a plan a newer one replaced")
	}
	if !opens(late, late, domain.Serving(next)) {
		t.Error("a late move read on its Selection can't open, so it could never report applied")
	}
	if opens(late, late, domain.Serving(server)) {
		t.Error("a late move read on the server it leaves opened")
	}
	if opens(plan(domain.Closed(domain.ConfirmedDead), Passage{}), plan(domain.Closed(domain.ConfirmedDead), Passage{}), domain.Selection{}) {
		t.Error("a closed slot opened")
	}
}

func TestAPlanNamesWhatItClosesAndWhen(t *testing.T) {
	server, next := testRef("server"), testRef("next")
	until := backoffStart.Add(30 * time.Second)
	plan := Plan{
		slots: []SlotTarget{
			workerTarget(workerSlot("closed", 1), domain.Closed(domain.ConfirmedDead), Passage{}),
			workerTarget(workerSlot("bridge", 2), domain.Serving(server), BridgeUntil(until)),
			workerTarget(workerSlot("move", 3), domain.Serving(next), MoveFrom(server, until.Add(-time.Minute))),
			workerTarget(workerSlot("open", 4), domain.Serving(next), Passage{}),
		},
		removals: []domain.Slot{workerSlot("gone", 5)},
	}
	landed := map[domain.SlotID]domain.Selection{"move": domain.Serving(next), "bridge": domain.Serving(server)}
	if got := plan.closing(until.Add(-2*time.Minute), nil); !slices.Equal(got, []string{"pxa1", "pxa5"}) {
		t.Errorf("before any deadline: %q", got)
	}
	if got := plan.closing(until.Add(-time.Second), nil); !slices.Equal(got, []string{"pxa1", "pxa3", "pxa5"}) {
		t.Errorf("past the move's deadline, not landed: %q", got)
	}
	if got := plan.closing(until.Add(-time.Second), landed); !slices.Equal(got, []string{"pxa1", "pxa5"}) {
		t.Errorf("past the move's deadline, landed: %q", got)
	}
	if got := plan.closing(until, landed); !slices.Equal(got, []string{"pxa1", "pxa2", "pxa5"}) {
		t.Errorf("past the bridge's deadline, whatever it shows: %q", got)
	}
	still := map[domain.SlotID]domain.Selection{"move": domain.Serving(server)}
	if got := plan.closing(until.Add(-time.Second), still); !slices.Equal(got, []string{"pxa1", "pxa3", "pxa5"}) {
		t.Errorf("a move read back on its Prior isn't landed: %q", got)
	}
	if next, ok := plan.nextDeadline(until.Add(-2 * time.Minute)); !ok || !next.Equal(until.Add(-time.Minute)) {
		t.Errorf("a move's deadline wasn't next: %v %v", next, ok)
	}
	if next, ok := plan.nextDeadline(until.Add(-time.Minute)); !ok || !next.Equal(until) {
		t.Errorf("the bridge's deadline wasn't next: %v %v", next, ok)
	}
	if _, ok := plan.nextDeadline(until); ok {
		t.Error("a deadline that has passed is still next")
	}
}

// fakeTools puts a systemctl script first on PATH and opens a Worker with a short poll that counts
// the links it closes.
func fakeTools(t *testing.T, systemctl string, poll time.Duration) (*Worker, func(string) int) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(systemctl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	w, err := Open(Options{DataDir: t.TempDir(), RunDir: t.TempDir(), Poll: poll})
	if err != nil {
		t.Fatal(err)
	}
	fakeLinks(t, map[string]int{"pxa1": linkGroup, "pxa2": linkGroup})
	var mu sync.Mutex
	closed := map[string]int{}
	w.gates.write = func(name string, up bool) error {
		mu.Lock()
		defer mu.Unlock()
		if !up {
			closed[name]++
		}
		return nil
	}
	return w, func(name string) int {
		mu.Lock()
		defer mu.Unlock()
		return closed[name]
	}
}

// fakeLinks points link reads at a directory holding these links in these groups.
func fakeLinks(t *testing.T, groups map[string]int) {
	t.Helper()
	dir := t.TempDir()
	for name, group := range groups {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "netdev_group"), []byte(strconv.Itoa(group)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	saved := sysNet
	sysNet = dir
	t.Cleanup(func() { sysNet = saved })
}

// Removing a slot closes its link only while the link is ours. Another program can hold the name.
func TestRemovingASlotLeavesAForeignLinkAtItsName(t *testing.T) {
	w, closed := fakeTools(t, activeCore, time.Hour)
	fakeLinks(t, map[string]int{"pxa1": 4242, "pxa2": linkGroup})
	plan, err := NewPlan(1, nil, nil, []domain.Slot{workerSlot("foreign", 1), workerSlot("ours", 2)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	goals := make(chan Plan, 1)
	go w.intake(ctx, goals)
	goals <- plan
	for limit := time.Now().Add(3 * time.Second); closed("pxa2") == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(limit) {
			t.Fatal("the removed slot's own link was never closed")
		}
	}
	if got := closed("pxa1"); got != 0 {
		t.Errorf("closed the foreign pxa1 %d times", got)
	}
}

const activeCore = "#!/bin/sh\nprintf 'ActiveState=active\\nMainPID=4242\\nInvocationID=e1\\n'\n"

// closedAddr is a port nothing listens on. The core's real API port won't do, because a lab VM's
// port forward can answer on it.
func closedAddr(t *testing.T) netip.AddrPort {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return netip.MustParseAddrPort(l.Addr().String())
}

// One read that fails says nothing about the core. The signal loop's routes, and through them the
// controller's evidence and any running confirmation, must outlive it.
func TestAFailedCoreReadKeepsTheLastTopology(t *testing.T) {
	server := testRef("server")
	servers := map[domain.ServerRef]domain.Server{server: testServer(t, vlessHop(t, "root", "198.51.100.1", ""))}
	plan, err := NewPlan(2, []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(server), Passage{})}, servers, nil)
	if err != nil {
		t.Fatal(err)
	}
	route := ProbeRoute{Server: server, Epoch: "e1", Generation: 1}
	for name, systemctl := range map[string]string{"systemctl show fails": "#!/bin/sh\nexit 1\n", "the core API fails": activeCore} {
		t.Run(name, func(t *testing.T) {
			w, _ := fakeTools(t, systemctl, time.Hour)
			w.core.api = newCoreAPI(closedAddr(t))
			w.adopted = true
			judged := w.core.current()
			judged.of[server] = verdict{state: verdictAccepted}
			w.core.judged.Store(&judged)
			w.coreRead.at = time.Now()
			w.topology.Store(&Topology{Epoch: "e1", Generation: 1, CorePID: 4242, Probes: []ProbeRoute{route}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			goals, reports := make(chan Plan, 1), make(chan Report, 8)
			done := make(chan error, 1)
			go func() { done <- w.Run(ctx, goals, reports, nil) }()
			goals <- plan
			select {
			case r := <-reports:
				if r.Error == "" {
					t.Fatal("this read was meant to fail")
				}
				if r.Topology.Epoch != "e1" || r.Topology.CorePID != 4242 || !slices.Equal(r.Topology.Probes, []ProbeRoute{route}) {
					t.Errorf("one failed read changed what signals may measure: %+v", r.Topology)
				}
				if r.Topology.Generation != 2 {
					t.Errorf("generation %d", r.Topology.Generation)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("no report")
			}
			cancel()
			<-done
		})
	}
}

// The read stalls and fails past readBound, in a pass that began before it. The gates read the bound
// when the read finished, and the routes must go in that same pass.
func TestRoutesGoInThePassThatClosesTheGates(t *testing.T) {
	slowTest(t)
	server := testRef("server")
	servers := map[domain.ServerRef]domain.Server{server: testServer(t, vlessHop(t, "root", "198.51.100.1", ""))}
	plan, err := NewPlan(2, []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(server), Passage{})}, servers, nil)
	if err != nil {
		t.Fatal(err)
	}
	route := ProbeRoute{Server: server, Epoch: "e1", Generation: 1}
	w, _ := fakeTools(t, "#!/bin/sh\nsleep 6\nexit 1\n", time.Hour)
	w.adopted = true
	judged := w.core.current()
	judged.of[server] = verdict{state: verdictAccepted}
	w.core.judged.Store(&judged)
	w.coreRead.at = time.Now().Add(-readBound + 2*time.Second)
	w.topology.Store(&Topology{Epoch: "e1", Generation: 1, CorePID: 4242, Probes: []ProbeRoute{route}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	goals, reports := make(chan Plan, 1), make(chan Report, 8)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, goals, reports, nil) }()
	goals <- plan
	select {
	case r := <-reports:
		if !strings.Contains(r.Error, "every gate closed") {
			t.Fatalf("the read that failed past the bound closed nothing: %s", r.Error)
		}
		if slices.Contains(r.Topology.Probes, route) {
			t.Errorf("gates closed past the bound and the routes stayed: %+v", r.Topology)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no report")
	}
	cancel()
	<-done
}

// A core that really stopped, read correctly, loses its routes in the first report.
func TestADeadCoreReadAsDeadLosesItsRoutesAtOnce(t *testing.T) {
	slowTest(t)
	server := testRef("server")
	servers := map[domain.ServerRef]domain.Server{server: testServer(t, vlessHop(t, "root", "198.51.100.1", ""))}
	plan, err := NewPlan(2, []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(server), Passage{})}, servers, nil)
	if err != nil {
		t.Fatal(err)
	}
	route := ProbeRoute{Server: server, Epoch: "e1", Generation: 1}
	w, _ := fakeTools(t, "#!/bin/sh\nprintf 'ActiveState=inactive\\nSubState=dead\\nMainPID=0\\nExecMainStartTimestampMonotonic=0\\nInvocationID=\\n'\n", time.Hour)
	w.adopted = true
	judged := w.core.current()
	judged.of[server] = verdict{state: verdictAccepted}
	w.core.judged.Store(&judged)
	w.coreRead.at = time.Now()
	w.topology.Store(&Topology{Epoch: "e1", Generation: 1, CorePID: 4242, Probes: []ProbeRoute{route}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	goals, reports := make(chan Plan, 1), make(chan Report, 8)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, goals, reports, nil) }()
	goals <- plan
	select {
	case r := <-reports:
		if slices.Contains(r.Topology.Probes, route) {
			t.Errorf("a dead core kept its route")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no report")
	}
	cancel()
	<-done
}

// Every combination of what a gate reads: past a Prior's deadline, a slot not passing never opens.
func TestNoGateOpensPastItsDeadlineOffItsSelection(t *testing.T) {
	for bits := range 1 << 13 {
		b := func(i int) bool { return bits&(1<<i) != 0 }
		g := gate{up: b(0), wanted: b(1), blind: b(2), coreRead: b(3), kernelRead: b(4), ingress: b(5), passed: b(6), routine: b(7), onInstalled: b(8), moving: b(9), stays: !b(10), plumbed: b(11), settled: b(12)}
		if !g.stays && !g.moving && !g.passed && g.open() {
			t.Errorf("opens: %+v", g)
		}
	}
}

// A pass blocked in a read can't stop a pending move's deadline, or a slot would go on exiting
// through a server its Exit forbids. And a move the last read showed landed keeps its gate.
func TestAPendingMovesDeadlineHoldsWhileAPassIsBlocked(t *testing.T) {
	slowTest(t)
	for _, c := range []struct {
		name   string
		landed bool
	}{{"not landed", false}, {"already landed", true}} {
		t.Run(c.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "read-started")
			w, closed := fakeTools(t, "#!/bin/sh\ntouch '"+marker+"'\nsleep 5\nexit 1\n", time.Hour)
			w.adopted = true
			prior, next := testRef("forbidden"), testRef("allowed")
			w.core.judged.Store(book(map[domain.ServerRef]verdict{prior: {state: verdictAccepted}}))
			if c.landed {
				w.gates.publish(claims{of: map[domain.SlotID]domain.Selection{"slot1": domain.Serving(next)}, vouched: map[domain.SlotID]bool{"slot1": true}, epoch: "e1"})
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			goals, reports := make(chan Plan, 1), make(chan Report, 16)
			go func() { done <- w.Run(ctx, goals, reports, nil) }()
			defer func() { cancel(); <-done }()
			until := time.Now().Add(2 * time.Second)
			goals <- Plan{generation: 1, slots: []SlotTarget{
				workerTarget(workerSlot("slot1", 1), domain.Serving(next), MoveFrom(prior, until)),
			}, servers: map[domain.ServerRef]domain.Server{prior: {}, next: {}}}
			for limit := time.Now().Add(2 * time.Second); ; {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(limit) {
					t.Fatal("the pass never entered the core read")
				}
				time.Sleep(time.Millisecond)
			}
			if got := closed("pxa1"); got != 0 {
				t.Fatalf("closed %d times before its deadline", got)
			}
			time.Sleep(time.Until(until.Add(time.Second)))
			select {
			case <-reports:
				t.Fatal("the pass wasn't blocked at the deadline, so this proves nothing")
			default:
			}
			if got := closed("pxa1") > 0; got == c.landed {
				t.Errorf("gate closed at the deadline: %t", got)
			}
		})
	}
}

// A move still on the server it must leave has until its deadline. The next pass comes then, not a
// poll later, and that pass is what closes the gate.
func TestAMoveOffAForbiddenServerGetsAPassAtItsDeadline(t *testing.T) {
	slowTest(t)
	bad, good := testRef("forbidden"), testRef("allowed")
	servers := map[domain.ServerRef]domain.Server{bad: testServer(t, vlessHop(t, "root", "198.51.100.1", "")), good: testServer(t, vlessHop(t, "root", "198.51.100.2", ""))}
	const poll = 4 * time.Second
	w, _ := fakeTools(t, activeCore, poll)
	w.adopted = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	goals, reports := make(chan Plan, 1), make(chan Report, 64)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, goals, reports, nil) }()
	until := time.Now().Add(500 * time.Millisecond)
	plan, err := NewPlan(1, []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(good), MoveFrom(bad, until))}, servers, nil)
	if err != nil {
		t.Fatal(err)
	}
	goals <- plan
	var first time.Time
	limit := time.After(2 * poll)
	for first.IsZero() {
		select {
		case r := <-reports:
			if !r.At.Before(until) {
				first = r.At
			}
		case <-limit:
			t.Fatal("no pass after the deadline")
		}
	}
	if late := first.Sub(until); late > time.Second {
		t.Errorf("the first pass past the deadline came %s late, on the poll of %s", late, poll)
	}
	cancel()
	<-done
}

// A pass can make its core plan before a deadline and read the core after it. The core then matches
// that plan exactly, on a server it may no longer serve. Only the requested replacement may open.
func TestAnExpiredPriorNeitherStaysOpenNorReopens(t *testing.T) {
	prior, next := testRef("prior"), testRef("next")
	until := backoffStart.Add(30 * time.Second)
	target := workerTarget(workerSlot("slot1", 1), domain.Serving(next), MoveFrom(prior, until))
	server := testServer(t, vlessHop(t, "root", "198.51.100.1", ""))
	plan := &Plan{slots: []SlotTarget{target}, servers: map[domain.ServerRef]domain.Server{prior: server, next: server}}
	w := &Worker{}
	made := w.corePlan(*plan, until.Add(-time.Second), book(map[domain.ServerRef]verdict{prior: {state: verdictAccepted}}))
	if len(made.Slots) != 1 || made.Slots[0].Selection != domain.Serving(prior) {
		t.Fatalf("the core plan didn't fall back on the prior: %+v", made.Slots)
	}
	gateAt := func(on domain.ServerRef, at time.Time, routine bool) gate {
		core := coreObservation{Applied: map[domain.SlotID]domain.Selection{"slot1": domain.Serving(on)}, Serving: map[domain.SlotID][]string{"slot1": {outboundTag(on)}}}
		_, moving := target.held(at)
		return gate{up: true, wanted: true, coreRead: true, kernelRead: true, ingress: true, passed: passedNow(target, core, at), routine: routine,
			onInstalled: onInstalled(target, core, at), moving: moving, stays: target.mayBeUp(at, unclaimed), plumbed: true, settled: true}
	}
	cases := []struct {
		name       string
		on         domain.ServerRef
		at         time.Time
		routine    bool
		keep, open bool
	}{
		{"on the prior before the deadline", prior, until.Add(-time.Second), false, true, true},
		{"on the prior at the deadline", prior, until, false, false, false},
		{"on the prior past the deadline", prior, until.Add(time.Second), false, false, false},
		{"on the prior past the deadline, in a pass that sees a new plan", prior, until.Add(time.Second), true, false, false},
		{"on the replacement past the deadline", next, until.Add(time.Second), false, true, true},
	}
	for _, c := range cases {
		g := gateAt(c.on, c.at, c.routine)
		if g.keep() != c.keep || g.open() != c.open {
			t.Errorf("%s: keep %v open %v, want %v %v", c.name, g.keep(), g.open(), c.keep, c.open)
		}
	}
}

// stalledWorker runs a Worker whose every `xray run -test` hangs, against a stopped core.
type stalledWorker struct {
	t        *testing.T
	worker   *Worker
	goals    chan Plan
	reports  chan Report
	started  string
	finished string
	mu       sync.Mutex
	closed   []string
	stop     func()
}

func newStalledWorker(t *testing.T) *stalledWorker {
	t.Helper()
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "systemctl"), []byte("#!/bin/sh\ncase \"$1\" in show) printf 'ActiveState=inactive\\nMainPID=0\\n';; *) exit 1;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	data, run := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "current"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &stalledWorker{t: t, goals: make(chan Plan, 1), reports: make(chan Report, 64),
		started: filepath.Join(tools, "started"), finished: filepath.Join(tools, "finished")}
	script := "#!/bin/sh\ntouch " + s.started + "\nsleep 30\ntouch " + s.finished + "\n"
	if err := os.WriteFile(filepath.Join(data, "current", "xray"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := Open(Options{DataDir: data, RunDir: run, Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	fakeLinks(t, map[string]int{"pxa1": linkGroup, "pxa2": linkGroup})
	w.gates.write = func(name string, up bool) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !up {
			s.closed = append(s.closed, name)
		}
		return nil
	}
	s.worker = w
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, s.goals, s.reports, nil) }()
	s.stop = func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the Worker didn't stop with a judge stalled")
		}
	}
	t.Cleanup(s.stop)
	return s
}

func (s *stalledWorker) waitFor(what string, ok func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			s.t.Fatalf("no %s within 3 s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(s.finished); !errors.Is(err, os.ErrNotExist) {
		s.t.Fatalf("%s came only after the judge finished", what)
	}
}

func (s *stalledWorker) closedLink(name string) func() bool {
	return func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return slices.Contains(s.closed, name)
	}
}

func (s *stalledWorker) reported(generation uint64) func() bool {
	return func() bool {
		for {
			select {
			case r := <-s.reports:
				if r.Generation == generation {
					return true
				}
			default:
				return false
			}
		}
	}
}

func TestAStalledCoreValidationHoldsNoClosureAndNoPlan(t *testing.T) {
	slowTest(t)
	s := newStalledWorker(t)
	bad, good := testRef("bad"), testRef("good")
	servers := map[domain.ServerRef]domain.Server{bad: testServer(t, vlessHop(t, "root", "198.51.100.1", "")), good: testServer(t, vlessHop(t, "root", "198.51.100.2", ""))}
	failing, bridging := workerSlot("failing", 1), workerSlot("bridging", 2)
	plan := func(generation uint64, slots ...SlotTarget) Plan {
		p, err := NewPlan(generation, slots, servers, nil)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	s.goals <- plan(1, workerTarget(failing, domain.Serving(bad), Passage{}))
	s.waitFor("xray run -test", func() bool { _, err := os.Stat(s.started); return err == nil })

	s.goals <- plan(2, workerTarget(failing, domain.Closed(domain.ConfirmedDead), Passage{}))
	s.waitFor("a closed gate", s.closedLink("pxa1"))

	s.goals <- plan(3, workerTarget(failing, domain.Serving(good), MoveFrom(bad, time.Now().Add(time.Minute))))
	s.waitFor("a pass over an urgent move", s.reported(3))

	s.goals <- plan(4, workerTarget(bridging, domain.Serving(bad), BridgeUntil(time.Now().Add(200*time.Millisecond))))
	s.waitFor("a pass over the bridge", s.reported(4))
	if s.closedLink("pxa2")() {
		t.Fatal("a bridge inside its deadline closed")
	}
	s.waitFor("a bridge closed at its deadline", s.closedLink("pxa2"))
}

// linkWrite is one write of a link's flag as it landed, and who wrote it.
type linkWrite struct {
	link string
	up   bool
	by   string // intake, an opening, or a pass
}

// openingRig is a Worker whose intake runs and whose core reads back slot1 on srv, with every link
// write recorded. No pass runs: a test drives one opening, as a pass's converge does once it has
// verified the core.
type openingRig struct {
	t       *testing.T
	worker  *Worker
	core    *inboundCore
	goals   chan Plan
	servers map[domain.ServerRef]domain.Server
	mu      sync.Mutex
	writes  []linkWrite
	hold    chan struct{} // While set, pxa1's up waits inside its write until it closes.
	holding chan struct{} // Closed once pxa1's up waits.
}

func newOpeningRig(t *testing.T) *openingRig {
	t.Helper()
	core := newInboundCore(t, probeRulePlan(t), append(inboundTags("slot1"), probeTag)...)
	r := &openingRig{t: t, worker: core.daemon(), core: core, goals: make(chan Plan, 1),
		servers: map[domain.ServerRef]domain.Server{testRef("srv"): testServer(t, vlessHop(t, "root", "198.51.100.1", ""))}}
	t.Cleanup(r.worker.core.reads.Wait)
	fakeLinks(t, map[string]int{"pxa1": linkGroup})
	r.worker.gates.write = func(link string, up bool) error {
		by, stack := "a pass", string(debug.Stack())
		switch {
		case strings.Contains(stack, "(*Worker).intake"):
			by = "intake"
		case strings.Contains(stack, "(*kernel).open"):
			by = "an opening"
		}
		r.mu.Lock()
		hold, holding := r.hold, r.holding
		r.mu.Unlock()
		if up && link == "pxa1" && hold != nil {
			close(holding)
			<-hold
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.writes = append(r.writes, linkWrite{link, up, by})
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.worker.intake(ctx, r.goals)
	return r
}

// take hands intake a plan and waits until it holds it.
func (r *openingRig) take(generation uint64, slots []SlotTarget, removals []domain.Slot) *Plan {
	r.t.Helper()
	p, err := NewPlan(generation, slots, r.servers, removals)
	if err != nil {
		r.t.Fatal(err)
	}
	r.goals <- p
	for limit := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		if newest := r.worker.gates.newest(); newest != nil && newest.generation == generation {
			return newest
		}
		if time.Now().After(limit) {
			r.t.Fatalf("intake never took plan %d", generation)
		}
	}
}

// open reads the core as a pass does, and starts slot1's opening on that readback.
func (r *openingRig) open(planned *Plan, beforeOpening func()) <-chan error {
	r.t.Helper()
	ctx := context.Background()
	cp := r.worker.corePlan(*planned, time.Now(), book(map[domain.ServerRef]verdict{testRef("srv"): {state: verdictAccepted}}))
	c := r.worker.corePass()
	verified := c.read(ctx, cp)
	if _, ok := verified.Applied["slot1"]; verified.readErr != nil || !ok {
		r.t.Fatalf("the pass's readback doesn't show slot1 applied: %v", verified.readErr)
	}
	beforeOpening()
	done := make(chan error, 1)
	go func() { done <- r.worker.kernel.open(ctx, "slot1", r.worker.opening(c, planned, cp, verified)) }()
	return done
}

// holdUnitRead makes the next read of the core's unit wait until release exists, once reading does.
func (r *openingRig) holdUnitRead() (reading, release string) {
	dir := r.t.TempDir()
	reading, release = filepath.Join(dir, "reading"), filepath.Join(dir, "release")
	script := "#!/bin/sh\ntouch '" + reading + "'\nwhile [ ! -e '" + release + "' ]; do sleep 0.01; done\n" +
		"printf 'ActiveState=active\\nMainPID=4242\\nInvocationID=e4242\\n'\n"
	if err := os.WriteFile(filepath.Join(r.core.dir, "bin", "systemctl"), []byte(script), 0o755); err != nil {
		r.t.Fatal(err)
	}
	return reading, release
}

// pxa1 is every write to pxa1 so far, within wait once ok holds of them.
func (r *openingRig) pxa1(wait time.Duration, ok func([]linkWrite) bool) []linkWrite {
	for limit := time.Now().Add(wait); ; time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		got := slices.DeleteFunc(slices.Clone(r.writes), func(w linkWrite) bool { return w.link != "pxa1" })
		r.mu.Unlock()
		if ok(got) || time.Now().After(limit) {
			return got
		}
	}
}

// An opening re-reads the core under the gate lock, so it can begin before a plan or a deadline that
// closes its slot and finish after it. Intake's close is then the gate's last word: the opening lands
// no up after it, whatever the pass does next.
func TestAClosureMadeDuringAnOpeningStaysClosed(t *testing.T) {
	slot := workerSlot("slot1", 1)
	closed := linkWrite{"pxa1", false, "intake"}
	closedLast := func(ws []linkWrite) bool { return len(ws) > 0 && ws[len(ws)-1] == closed }

	t.Run("a deletion while the opening reads the core", func(t *testing.T) {
		r := newOpeningRig(t)
		planned := r.take(1, []SlotTarget{workerTarget(slot, domain.Serving(testRef("srv")), Passage{})}, nil)
		var reading, release string
		done := r.open(planned, func() { reading, release = r.holdUnitRead() })
		for limit := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
			if _, err := os.Stat(reading); err == nil {
				break
			}
			if time.Now().After(limit) {
				t.Fatal("the opening never read the core")
			}
		}
		r.take(2, nil, []domain.Slot{slot})
		if got := r.pxa1(3*time.Second, closedLast); !closedLast(got) {
			t.Fatalf("intake didn't close the deleted slot: %v", got)
		}
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		err := <-done
		if got := r.pxa1(0, closedLast); !closedLast(got) {
			t.Fatalf("pxa1 written %v: the opening set the deleted slot's link up after intake closed it (opening: %v)", got, err)
		}
	})

	t.Run("a bridge's deadline while the opening writes", func(t *testing.T) {
		r := newOpeningRig(t)
		until := time.Now().Add(time.Second)
		planned := r.take(1, []SlotTarget{workerTarget(slot, domain.Serving(testRef("srv")), BridgeUntil(until))}, nil)
		hold, holding := make(chan struct{}), make(chan struct{})
		done := r.open(planned, func() {
			r.mu.Lock()
			r.hold, r.holding = hold, holding
			r.mu.Unlock()
		})
		select {
		case <-holding:
		case err := <-done:
			t.Fatalf("the opening never reached its write before the deadline: %v", err)
		}
		time.Sleep(time.Until(until))
		r.pxa1(300*time.Millisecond, closedLast)
		close(hold)
		if err := <-done; err != nil {
			t.Fatalf("the opening confirmed the bridge before its deadline and still failed: %v", err)
		}
		if got := r.pxa1(3*time.Second, closedLast); !closedLast(got) || !slices.Contains(got, linkWrite{"pxa1", true, "an opening"}) {
			t.Fatalf("pxa1 written %v: the opening set the bridge's link up after intake closed it at the deadline", got)
		}
	})
}

// Another program can take pxa1's name while an opening reads the core. The opening raises only a link
// that reads ours at its write, so the foreign link keeps its flag.
func TestAnOpeningNeverRaisesALinkThatIsntOurs(t *testing.T) {
	r := newOpeningRig(t)
	planned := r.take(1, []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Serving(testRef("srv")), Passage{})}, nil)
	var reading, release string
	done := r.open(planned, func() { reading, release = r.holdUnitRead() })
	for limit := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(reading); err == nil {
			break
		}
		if time.Now().After(limit) {
			t.Fatal("the opening never read the core")
		}
	}
	if err := os.WriteFile(filepath.Join(sysNet, "pxa1", "netdev_group"), []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if got := r.pxa1(0, func([]linkWrite) bool { return true }); len(got) != 0 || err == nil {
		t.Fatalf("pxa1 written %v, opening error %v: the opening raised a link in another group", got, err)
	}
}

// An opening already superseded when it starts, by a newer plan or by its bridge's deadline, is refused
// before it reads the core. A pass waits on its openings, so one that read a stalled core would hold up
// the pass that opens the replacement, and close-gates' second sweep behind the gate lock.
func TestASupersededOpeningReadsNoCore(t *testing.T) {
	slot := workerSlot("slot1", 1)
	refusedUnread := func(t *testing.T, r *openingRig, done <-chan error, reading, release string) {
		t.Helper()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("the superseded opening landed")
			}
		case <-time.After(2 * time.Second):
			_, entered := os.Stat(reading)
			t.Errorf("the superseded opening still waits after 2 s, in the core read: %t", entered == nil)
			if err := os.WriteFile(release, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			<-done
		}
		if _, err := os.Stat(reading); err == nil {
			t.Error("the superseded opening read the core")
		}
		if got := r.pxa1(0, func([]linkWrite) bool { return true }); slices.Contains(got, linkWrite{"pxa1", true, "an opening"}) {
			t.Errorf("pxa1 written %v", got)
		}
	}

	t.Run("a newer plan", func(t *testing.T) {
		r := newOpeningRig(t)
		planned := r.take(1, []SlotTarget{workerTarget(slot, domain.Serving(testRef("srv")), Passage{})}, nil)
		r.take(2, nil, []domain.Slot{slot})
		var reading, release string
		done := r.open(planned, func() { reading, release = r.holdUnitRead() })
		refusedUnread(t, r, done, reading, release)
	})

	t.Run("a bridge past its deadline", func(t *testing.T) {
		r := newOpeningRig(t)
		until := time.Now().Add(300 * time.Millisecond)
		planned := r.take(1, []SlotTarget{workerTarget(slot, domain.Serving(testRef("srv")), BridgeUntil(until))}, nil)
		var reading, release string
		done := r.open(planned, func() {
			time.Sleep(time.Until(until))
			reading, release = r.holdUnitRead()
		})
		refusedUnread(t, r, done, reading, release)
	})
}
