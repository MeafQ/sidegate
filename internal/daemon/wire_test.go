package daemon

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	"sidegate/internal/metrics"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

var updateWire = flag.Bool("update", false, "rewrite web/tests/wire from what the views produce")

const wireDir = "../../web/tests/wire"

// wireRig builds one of everything the page can read, through the real view functions.
func wireRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, t.TempDir())
	r.c.evidence.epoch = "e1"
	r.seed(lineage(t, "a", "s", "DE", "a.example"), lineage(t, "b", "s", "DE", "b.example"), lineage(t, "c", "s", "NL", "c.example"), lineage(t, "d", "s", "NL", "d.example"))
	r.putPicked("mine", "Mine", "a", "b")
	r.putGroup("rule", "Rule", ruleMembers(t, rule{Sources: []domain.SourceID{"s"}, Countries: []string{"DE"}, Names: mustFilter(t, "a", "z")}))
	r.putSlot("serving", exitIntent(t, "Serving", domain.GroupExit("mine")))
	r.putSlot("assigned", exitIntent(t, "Assigned", domain.ServerExit("c")))
	r.putSlot("source", exitIntent(t, "Source", domain.SourceExit("s")))
	r.putSlot("direct", exitIntent(t, "Direct", domain.DirectExit()))
	r.putSlot("block", exitIntent(t, "Block", domain.BlockExit()))
	r.putSlot("dead", exitIntent(t, "Dead", domain.ServerExit("d")))
	r.frame(domain.WANUp, r.probe("a", domain.Succeeded, 10), r.probe("b", domain.Succeeded, 20), r.probe("d", domain.Failed, 0))
	r.reportApplied("block")
	r.reportApplied("direct")
	report := gateway.Report{Generation: r.c.state.runtimeGeneration, Topology: gateway.Topology{Epoch: "e1"}, Slots: []gateway.SlotReport{{Slot: "serving", Phase: gateway.Ready,
		Applied: r.c.state.slots["serving"].Selection, Operation: r.operation("serving"), NetworkID: "n1", NetworkEstablished: true, CoreEpoch: "e1"},
		{Slot: "direct", Phase: gateway.Ready, Applied: domain.Direct(), NetworkID: "n2", NetworkEstablished: true, CoreEpoch: "e1"}},
		Core: gateway.CoreVersions{Release: "26.10.31", Running: "25.12.8"}}
	if err := r.c.applyReport(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	yes := true
	r.c.clientsKnown, r.c.key.answered = true, r.clock
	r.c.foreign = []unifi.ForeignClient{{ID: "n-theirs", Name: "Theirs", Enabled: true}}
	no := false
	r.c.owned["serving"] = unifi.Owned{NetworkID: "n1", Slot: "serving", Name: "Serving", Enabled: &yes, Routes: []unifi.Route{{ID: "r1", Name: "To serving", Kind: unifi.TrafficRoute, Target: "n1", Enabled: true, KillSwitch: &yes}}}
	r.c.owned["direct"] = unifi.Owned{NetworkID: "n2", Slot: "direct", Name: "Direct", Enabled: &no}
	for _, id := range []domain.SlotID{"assigned", "source", "dead"} {
		r.c.owned[id] = unifi.Owned{Slot: id, Name: string(id)}
	}
	r.c.owned["gone"] = unifi.Owned{NetworkID: "n-kept", Slot: "gone", Name: "Kept", Enabled: &yes}
	r.c.clientTroubles = map[domain.SlotID]ClientTrouble{"assigned": clientNameTaken, "gone": clientRouted}
	row := r.c.state.slots["block"]
	row.Slot.Allocation.NetworkID = "n-gone"
	r.c.state.slots["block"] = row
	r.c.publish(r.clock)
	return r
}

func wireSamples(t *testing.T) map[string]any {
	t.Helper()
	r := wireRig(t)
	view := r.app.Snapshot()
	samples := map[string]any{"View.board": view}
	for _, s := range view.Slots {
		samples["SlotView."+string(s.ID)] = s
	}
	for _, g := range view.Groups {
		samples["GroupView."+string(g.ID)] = g
	}

	ms := func(n int) *time.Duration {
		d := time.Duration(n) * time.Millisecond
		return &d
	}
	at := r.clock
	remote := domain.Source{ID: "remote", Revision: 2, Name: "Remote",
		Origin: domain.RemoteOrigin(domain.Remote{Location: "ref", UserAgent: "Happ/3.0.0", HWID: "hwid", RefreshEvery: time.Hour}),
		Feed: &domain.Feed{Configs: []domain.FeedConfig{
			{Key: "a", Place: 1, Name: "Sweden 🇸🇪", Servers: 2, Skipped: []domain.Skip{{Index: 2, Tag: "relay", Reason: domain.SkipHop}, {Index: 4, Tag: "DIRECT", Reason: domain.SkipHelper, Protocol: "freedom"}}},
			{Place: 3, Name: "Warsaw", Skipped: []domain.Skip{{Index: 1, Tag: "proxy", Reason: domain.SkipRefused, Position: 1, Code: string(catalogue.CodeInsecureTLS), Field: "allowInsecure"}}},
		}}, Added: at.Add(-72 * time.Hour)}
	var fetch catalogue.FetchState
	body := fmt.Sprintf(`{"checked_at":%q,"updated_at":%q,"failures":2,"failure":"http_503"}`, at.Format(time.RFC3339), at.Format(time.RFC3339))
	if err := fetch.UnmarshalBinary([]byte(body)); err != nil {
		t.Fatal(err)
	}
	diff := refreshEvent{Added: 1, Missing: 2, Changed: 3, Diagnostics: []catalogue.Diagnostic{{Entry: 4, Code: catalogue.CodeUnsupportedProtocol, Field: "field"}}}
	world := r.c.world
	facts := ownerFacts{Moves7d: 2, FirstChoice: ptr(domain.LineageID("a")), Order: []domain.LineageID{"a", "b"},
		Preferred: &PreferredDTO{Server: "a", EndsAt: at.Add(21 * time.Hour)}}
	samples["SourceView.remote"] = sourceView(remote, "Remote", fetch, world, at.Add(time.Hour), &diff, facts)
	samples["GroupView.preferred"] = groupView(world.groups["mine"], world, facts)
	samples["SourceView.manual"] = sourceView(domain.Source{ID: "s", Revision: 1, Name: "Manual",
		Origin: domain.ManualOrigin(domain.Manual{Text: "ref"})}, "Manual", catalogue.FetchState{}, world, at, nil, ownerFacts{})
	samples["SourceView.unread"] = sourceView(domain.Source{ID: "new", Revision: 1, Name: "New", Origin: domain.ManualOrigin(domain.Manual{})}, "New", catalogue.FetchState{}, world, at, nil, ownerFacts{})

	present, _ := world.catalogue.Lookup("a")
	missing := present
	missing.Presence, missing.MissingSince, missing.Country = domain.Missing, at, domain.Country{}
	flagged := present
	flagged.Country = domain.Country{Code: "NL", Source: domain.CountryRule}
	flagged.Presence = domain.Tombstone
	homed, _ := domain.NewName("Sweden 🇸🇪", "proxy")
	tagged, _ := domain.NewName("", "EDGE-Sweden 🇸🇪")
	present.Label, present.Naming = "Sweden 🇸🇪 · proxy", domain.Naming{Name: homed, Home: "k1"}
	present.Country = domain.Country{Code: "SE", Source: domain.CountryConfigFlag}
	missing.Label, missing.Naming = "EDGE-Sweden 🇸🇪", domain.Naming{Name: tagged}
	byTag := missing
	byTag.Presence, byTag.MissingSince, byTag.Country = domain.Present, time.Time{}, domain.Country{Code: "SE", Source: domain.CountryTagFlag}
	flagged.Label, flagged.Naming = "Server 2", domain.Naming{Number: 2}
	onPresent := domain.Switch{Owner: domain.GroupOwner("mine"), Slots: []domain.SlotID{"serving"}, From: domain.Closed(domain.Unselected), To: domain.Serving(present.Ref), AppliedAt: ptr(at.Add(-time.Hour))}
	measured := record{FirstSeen: at.Add(-3 * 24 * time.Hour), WorkedFrom: at.Add(-3 * 24 * time.Hour)}
	for i, ping := range []int{70, 90} {
		when := at.Add(-time.Duration(1-i) * 24 * time.Hour)
		measured, _ = measured.measured(when, &sample{At: when, Ping: *ms(ping), NewConn: *ms(210)}, true).credited(when, time.Hour).heard(result{At: when, Passed: true})
	}
	measured, _ = measured.charged(at.Add(-30 * time.Minute))
	failed, _ := record{}.measured(at.Add(-time.Hour), nil, false).heard(result{At: at.Add(-time.Hour)})
	withHealth := func(l domain.Lineage, r record) ServerView {
		v := serverView(l)
		v.Health = healthOf(r, "", at)
		return v
	}
	samples["ServerPage.mixed"] = ServerPage{Items: []ServerView{withHealth(present, measured), withHealth(missing, record{}), withHealth(byTag, record{}), withHealth(flagged, failed)}, Next: "a"}
	samples["ServerPage.last"] = ServerPage{Items: []ServerView{}}
	unnamed := present
	unnamed.Label, unnamed.Naming = "Server", domain.Naming{Home: "k1"}
	unnamed.Country = domain.Country{Code: "SE", Source: domain.CountryTagFlag}
	unnamed.Seen = []domain.Occurrence{{Config: "k1", ConfigName: "🇸🇪", ConfigPlace: 1, Tag: "🇸🇪", Position: 1}}
	samples["ServerPage.unnamed"] = ServerPage{Items: []ServerView{withHealth(unnamed, record{})}}

	samples["GroupPreview.matched"] = GroupPreview{Matched: []domain.LineageID{"a"}}

	up := domain.Serving(present.Ref)
	samples["SwitchEvent.down"] = switchEvent(domain.Switch{ID: "e1", Owner: domain.SlotOwner("serving"), Slots: []domain.SlotID{"serving"}, Intent: 1, From: domain.Closed(domain.Unselected), To: up, Reason: domain.InitialSelection, RequestedAt: at})
	samples["SwitchEvent.group"] = switchEvent(domain.Switch{ID: "e3", Owner: domain.GroupOwner("mine"), Slots: []domain.SlotID{"serving", "other"}, Intent: 2, From: up, To: domain.Serving(ref("b")), Reason: domain.ImprovementMove, RequestedAt: at, AppliedAt: &at})
	samples["SwitchEvent.preferred"] = switchEvent(domain.Switch{ID: "e5", Owner: domain.GroupOwner("mine"), Slots: []domain.SlotID{"serving", "other"}, Intent: 2, From: domain.Serving(ref("b")), To: up, Reason: domain.PreferredMove, RequestedAt: at, AppliedAt: &at, AllowUntil: at.Add(30 * time.Second)})
	samples["SwitchEvent.source"] = switchEvent(domain.Switch{ID: "e4", Owner: domain.SourceOwner("s"), Slots: []domain.SlotID{"source"}, Intent: 1, From: domain.Direct(), To: up, Reason: domain.ExitChanged, RequestedAt: at})
	samples["SwitchEvent.up"] = switchEvent(domain.Switch{ID: "e2", Owner: domain.SlotOwner("serving"), Slots: []domain.SlotID{"serving"}, Intent: 2, From: up, To: domain.Closed(domain.ConfirmedDead), Reason: domain.FailureMove, RequestedAt: at, AppliedAt: &at, AllowUntil: at,
		Evidence: []domain.EvidenceSummary{{Server: present.Ref, Kind: domain.ActiveProbe, Verdict: domain.Failed, Target: "t1", Round: 1, At: at, NewConn: ms(10), Ping: ms(5)}}})
	samples["ReplacedEvent.moved"] = ReplacedEvent{Source: "s", Server: "a", Label: "Deutschland", PreviousLabel: "Germany", At: at}
	samples["ProbeResult.ok"] = ProbeResult{Lineage: "a", Verdict: "succeeded", PingMs: millis(ms(5)), NewConnectionMs: millis(ms(9)), At: at}
	samples["ProbeResult.failed"] = ProbeResult{Lineage: "a", Verdict: "failed", At: at}
	samples["SettingsView.defaults"] = SettingsView{Settings: DefaultSettings(), Revision: 1}
	release := "0123456789ab"
	noStart, unreachable, message := ReasonNoStart, CheckUnreachable, "the update didn't start"
	offer := &UpdateOffer{Version: "v0.2.0", Notes: "https://example.test/OWNER/sidegate/releases/tag/v0.2.0"}
	guide := Build{Version: "v0.1.0", Releases: "https://example.test/OWNER/sidegate/releases"}.guide()
	samples["UpdateView.dev"] = UpdateView{Version: "dev-7990743"}
	samples["UpdateView.running"] = UpdateView{Version: "v0.1.0", Releases: true, Guide: &guide, Check: UpdateCheck{At: &at}, Offer: offer, Running: &UpdateRunning{Attempt: "0123456789abcdef", Version: "v0.2.0"}}
	samples["UpdateView.failed"] = UpdateView{Version: "v0.1.0", Releases: true, Guide: &guide, Check: UpdateCheck{At: &at, Failure: &unreachable}, Offer: offer,
		Last: &UpdateLast{Attempt: "0123456789abcdef", Version: "v0.2.0", At: at, Outcome: UpdateFailed, Reason: &noStart, Message: &message}}
	samples["UpdateView.landed"] = UpdateView{Version: "v0.2.0", Releases: true, Guide: &guide, Check: UpdateCheck{At: &at},
		Last: &UpdateLast{Attempt: "0123456789abcdef", Version: "v0.2.0", At: at, Outcome: UpdateLanded}}
	samples["Session.owner"] = Session{User: "owner", CSRF: "token", Version: "v0.1.0", Release: &release}
	samples["Session.dev"] = Session{User: "owner", CSRF: "token", Version: "dev-7990743"}
	slot, source := domain.SlotID("serving"), domain.SourceID("s")
	samples["ChangeEvent.slot"] = ChangeEvent{ID: "1", At: at, Revision: 1, Kind: "slot", Slot: &slot}
	samples["ChangeEvent.source"] = ChangeEvent{ID: "2", At: at, Revision: 1, Kind: "source", Source: &source}
	samples["ChangeEvent.group"] = ChangeEvent{ID: "3", At: at, Revision: 1, Kind: "group"}

	dims := map[domain.LineageID]dimension{"a": {Source: "s", Country: "DE"}}
	series := metrics.Series{Buckets: []metrics.Bucket{{Start: at, Lineage: "a", Target: "t1", Purpose: domain.Heartbeat, Attempts: 4, Successes: 4, PingMedian: ms(100), PingP95: ms(140), PingSamples: 4}}, CoveredUntil: at}
	onPresent.AppliedAt = &at
	week := usageOf([]domain.Switch{onPresent}, shown(onPresent), at.Add(time.Minute))
	samples["AnalyticsReport.one"] = analyticsReport(series, dims, eventCounts{}, week, AnalyticsQuery{From: at, To: at.Add(5 * time.Minute), Bucket: 5 * time.Minute})

	samples["SlotUnifi.unknown"] = SlotUnifi{Name: "Client", Interface: "pxa1", Client: "none"}
	samples["VpnClients.unknown"] = vpnClientsDTO()
	samples["VpnClients.known"] = VpnClients{Known: true, Limit: 8, Foreign: []ForeignClientDTO{{ID: "n", Name: "Theirs", Enabled: true}}}

	samples["SlotInput.group"] = SlotInputDTO{Name: "S", Exit: ExitDTO{Kind: "group", Group: "mine"}}
	samples["SlotInput.server"] = SlotInputDTO{Name: "S", Exit: ExitDTO{Kind: "server", Server: "a"}}
	samples["SlotInput.source"] = SlotInputDTO{Name: "S", Exit: ExitDTO{Kind: "source", Source: "s"}}
	samples["SlotInput.direct"] = SlotInputDTO{Name: "S", Exit: ExitDTO{Kind: "direct"}}
	samples["SlotInput.block"] = SlotInputDTO{Name: "S", Exit: ExitDTO{Kind: "block"}}
	samples["GroupInput.rule"] = GroupInputDTO{Name: "G", Members: MembersDTO{Sources: AmongDTO[domain.SourceID]{Values: []domain.SourceID{"s"}}, Countries: AmongDTO[string]{Not: true, Values: []string{"DE"}},
		Protocols: AmongDTO[string]{Values: []string{"vless"}}, Include: "a", Exclude: "b", Box: boxDTO(domain.BoxUnticked)}}
	samples["GroupInput.list"] = GroupInputDTO{Name: "G", Members: MembersDTO{Sources: AmongDTO[domain.SourceID]{Values: []domain.SourceID{}}, Countries: AmongDTO[string]{Values: []string{}},
		Protocols: AmongDTO[string]{Values: []string{}}, Box: boxDTO(domain.BoxTicked)}}
	samples["GroupCreate.picked"] = GroupCreateDTO{GroupInputDTO: samples["GroupInput.list"].(GroupInputDTO), Ticked: []domain.LineageID{"a", "b"}}
	return samples
}

func names[K comparable](m map[K]string) []string {
	return slices.Sorted(maps.Values(m))
}

// constsOf reads the values of every constant the package in dir declares with the named type, from
// its source, so a new code reaches the page's word tables without a list to keep.
func constsOf(t *testing.T, dir, typeName string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
				for _, spec := range gen.Specs {
					value := spec.(*ast.ValueSpec)
					if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != typeName {
						continue
					}
					for _, v := range value.Values {
						s, err := strconv.Unquote(v.(*ast.BasicLit).Value)
						if err != nil {
							t.Fatal(err)
						}
						out = append(out, s)
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no %s constants", dir, typeName)
	}
	slices.Sort(out)
	return out
}

func wireEnums(t *testing.T) map[string][]string {
	return map[string][]string{"DownReason": names(downNames), "SwitchReason": names(reasonNames), "SignalKind": names(kindNames),
		"Verdict": names(verdictNames), "RuntimePhase": names(phaseNames), "UDPState": names(udpNames),
		"FailureCode": constsOf(t, "../catalogue", "FailureCode"), "DiagnosticCode": constsOf(t, "../catalogue", "DiagnosticCode"),
		"RefusalReason": constsOf(t, ".", "Reason"),
		"UpdateOutcome": constsOf(t, ".", "UpdateOutcome"), "UpdateReason": constsOf(t, ".", "UpdateReason"), "CheckFailure": constsOf(t, ".", "CheckFailure")}
}

func encodedSample(t *testing.T, value any) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetIndent("", "  ")
	if err := e.Encode(value); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestWireSamples holds the views to the committed samples, so a Go change without new samples
// fails here, and web/src/lib/wire.test.ts holds the samples to schema.ts, so new samples without
// a schema change fail there. -update rewrites them.
func TestWireSamples(t *testing.T) {
	want := map[string][]byte{"enums": encodedSample(t, wireEnums(t))}
	for name, value := range wireSamples(t) {
		want[name] = encodedSample(t, value)
	}
	if *updateWire {
		if err := os.RemoveAll(wireDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(wireDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range want {
			if err := os.WriteFile(filepath.Join(wireDir, name+".json"), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for name, body := range want {
		got, err := os.ReadFile(filepath.Join(wireDir, name+".json"))
		if err != nil {
			t.Errorf("%s: %v. Run go test ./internal/daemon -run TestWireSamples -update", name, err)
			continue
		}
		if !bytes.Equal(got, body) {
			t.Errorf("%s differs from what the view produces. Run with -update, and change schema.ts to match.\ncommitted:\n%s\nnow:\n%s", name, got, body)
		}
	}
	files, err := filepath.Glob(filepath.Join(wireDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if _, ok := want[filepath.Base(file[:len(file)-len(".json")])]; !ok {
			t.Errorf("%s is a sample nothing produces", file)
		}
	}
}

// The inputs the page sends must admit on the Go side, one arm each.
func TestWireInputsAdmit(t *testing.T) {
	samples := wireSamples(t)
	for name, value := range samples {
		body := encodedSample(t, value)
		switch value.(type) {
		case SlotInputDTO:
			var dto SlotInputDTO
			if refused := decode(body, &dto); refused != nil {
				t.Errorf("%s: %s", name, refused.Body)
			}
			intent, err := dto.admit()
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if got := encodedSample(t, SlotInputDTO{Name: intent.Name(), Exit: exitDTO(intent.Exit())}); !bytes.Equal(got, body) {
				t.Errorf("%s doesn't round-trip:\n%s\n%s", name, body, got)
			}
		case GroupInputDTO:
			var dto GroupInputDTO
			if refused := decode(body, &dto); refused != nil {
				t.Errorf("%s: %s", name, refused.Body)
			}
			members, err := dto.Members.admit()
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if got := encodedSample(t, GroupInputDTO{Name: dto.Name, Members: membersDTO(members)}); !bytes.Equal(got, body) {
				t.Errorf("%s doesn't round-trip:\n%s\n%s", name, body, got)
			}
		case GroupCreateDTO:
			var dto GroupCreateDTO
			if refused := decode(body, &dto); refused != nil {
				t.Errorf("%s: %s", name, refused.Body)
			}
			members, err := dto.Members.admit()
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			ticked, err := domain.AdmitTicked(dto.Ticked)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if got := encodedSample(t, GroupCreateDTO{GroupInputDTO{Name: dto.Name, Members: membersDTO(members)}, ticked}); !bytes.Equal(got, body) {
				t.Errorf("%s doesn't round-trip:\n%s\n%s", name, body, got)
			}
		}
	}
}

func TestStateCodecRoundTrips(t *testing.T) {
	for _, intent := range []domain.SlotIntent{exitIntent(t, "S", domain.ServerExit("a")), exitIntent(t, "G", domain.GroupExit("g")), exitIntent(t, "R", domain.SourceExit("s")),
		exitIntent(t, "D", domain.DirectExit()), exitIntent(t, "B", domain.BlockExit())} {
		body, err := encodeIntent(intent)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeIntent(body)
		if err != nil || got != intent {
			t.Errorf("intent %+v came back %+v, %v", intent, got, err)
		}
	}
	if got, err := decodeIntent([]byte(`{"name":"S","exit":{}}`)); err == nil {
		t.Errorf("an intent with no Exit decoded: %+v", got)
	}
	for _, g := range []domain.Group{
		{ID: "r", Revision: 2, Name: "Rule", Members: ruleMembers(t, rule{Sources: []domain.SourceID{"s"}, Countries: []string{"de"}, Names: mustFilter(t, "a", "b")})},
		{ID: "l", Revision: 3, Name: "List", Members: picked, Ticked: []domain.LineageID{"a", "b"}},
		{ID: "u", Revision: 4, Name: "Unticked", Members: boxed(ruleMembers(t, rule{Countries: []string{"DE"}}), domain.BoxUnticked), Ticked: []domain.LineageID{"a"}},
	} {
		body, err := encodeGroup(g)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeGroup(g.ID, g.Revision, body)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := encodeGroup(got)
		if !bytes.Equal(body, again) || got.Name != g.Name {
			t.Errorf("group %+v came back %+v", g, got)
		}
	}
	if _, err := decodeGroup("x", 1, []byte(`{"members":{"include":"("}}`)); err == nil {
		t.Error("a group with a broken pattern decoded")
	}
	memory := policyMemory{Selection: domain.Closed(domain.NoExit), LastServed: ref("a"), LeaveBy: t0, KeepOut: map[domain.LineageID]time.Time{"b": t0}}
	body, err := encodeMemory(memory)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeMemory(body)
	if err != nil || got.Selection != memory.Selection || got.LastServed != memory.LastServed || !got.LeaveBy.Equal(t0) || !got.KeepOut["b"].Equal(t0) {
		t.Errorf("memory came back %+v, %v", got, err)
	}
	sw := domain.Switch{From: domain.Serving(ref("a")), To: domain.Serving(ref("b")), AllowUntil: t0}
	encoded, err := encodeSwitch(sw)
	if err != nil {
		t.Fatal(err)
	}
	var back domain.Switch
	if err := decodeSwitch(encoded, &back); err != nil || !back.AllowUntil.Equal(t0) || back.From != sw.From || back.To != sw.To {
		t.Errorf("switch came back %+v, %v", back, err)
	}
}

func TestAnOwnersPickLoadsInEachShapeAReleaseWrote(t *testing.T) {
	a, err := json.Marshal(refToWire(ref("a")))
	if err != nil {
		t.Fatal(err)
	}
	boosted := `{"server":"a","at":"2026-11-04T12:00:00Z","size":12.5}`
	for _, c := range []struct {
		name, fields, state string
		boost               *preference
	}{
		{"a held boost", `"preferred":` + boosted, "held", &preference{Server: "a", boost: boost{At: t0, Size: 12.5}}},
		{"a move with its boost", `"preferred":` + boosted + `,"pick":` + string(a), "queued", &preference{Server: "a", boost: boost{At: t0, Size: 12.5}}},
		{"a move with no boost", `"pick":` + string(a), "queued", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := `{"selection":{"server":` + string(a) + `},"lastServed":` + string(a) + `,"leaveBy":"0001-01-01T00:00:00Z",` + c.fields + `}`
			m, err := decodeMemory([]byte(row))
			if err != nil {
				t.Fatal(err)
			}
			if got := pickState(m.Hand); got != c.state {
				t.Errorf("loads %s, want %s", got, c.state)
			}
			if move, queued := m.Hand.queued(); queued && move != ref("a") {
				t.Errorf("loads the move %+v, want a", move)
			}
			if got := storedBoost(m.Hand); (got == nil) != (c.boost == nil) || got != nil && (got.Server != c.boost.Server || !got.same(c.boost.boost)) {
				t.Errorf("loads the boost %+v, want %+v", got, c.boost)
			}
			body, err := encodeMemory(m)
			if err != nil {
				t.Fatal(err)
			}
			var stored, want map[string]json.RawMessage
			if err := json.Unmarshal(body, &stored); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(row), &want); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"pick", "preferred"} {
				if !bytes.Equal(stored[key], want[key]) {
					t.Errorf("stores %s as %s, want %s", key, stored[key], want[key])
				}
			}
		})
	}
}
