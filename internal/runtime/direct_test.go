package runtime

import (
	"encoding/json"
	"slices"
	"testing"

	"sidegate/internal/domain"
)

func TestADirectSlotTakesTheCoreWithNoServer(t *testing.T) {
	selected := testRef("srvA")
	slot := workerSlot("slot1", 1)
	servers := map[domain.ServerRef]domain.Server{selected: testServer(t, vlessHop(t, "root", "198.51.100.1", ""))}
	plan := Plan{slots: []SlotTarget{workerTarget(slot, domain.Direct(), Passage{})}, servers: servers}
	cp := (&Worker{}).corePlan(plan, backoffStart, book(map[domain.ServerRef]verdict{selected: {state: verdictAccepted}}))
	if len(cp.Slots) != 1 || !cp.Slots[0].Selection.IsDirect() {
		t.Fatalf("slots %+v", cp.Slots)
	}
	if !slices.Equal(cp.Probes, []domain.ServerRef{selected}) {
		t.Errorf("the server a direct slot returns to lost its route: %v", cp.Probes)
	}
	if got := plan.closing(backoffStart, nil); len(got) != 0 {
		t.Errorf("a direct slot closes %v", got)
	}
}

func TestTheCoreSendsADirectSlotToTheWAN(t *testing.T) {
	want := corePlan{Slots: []coreSlot{{ID: "slot1", Port: 12341, Selection: domain.Direct()}}}
	config := decode(t, must(render(want)))
	outbounds := config["outbounds"].([]any)
	if first := outbounds[0].(map[string]any); first["tag"] != blockTag {
		t.Fatalf("first outbound %v: a routing miss must stay blocked, not reach the WAN", first)
	}
	if direct := outbounds[1].(map[string]any); direct["tag"] != directTag || direct["protocol"] != "freedom" {
		t.Fatalf("second outbound %v", direct)
	}
	balancers := config["routing"].(map[string]any)["balancers"].([]any)
	if got := toStrings(balancers[0].(map[string]any)["selector"]); !slices.Equal(got, []string{directTag}) {
		t.Errorf("selector %v: a rewrite before the override lands must not put new connections on a server", got)
	}
}

func TestDirectIsPartOfWhatARewriteWrites(t *testing.T) {
	server := testRef("srvA")
	on := corePlan{Slots: []coreSlot{{ID: "slot1", Port: 12341, Selection: domain.Serving(server)}}}
	off := corePlan{Slots: []coreSlot{{ID: "slot1", Port: 12341, Selection: domain.Direct()}}}
	if routingKey(on) == routingKey(off) {
		t.Fatal("a flip must read as a new plan, or the pass that sees it closes the gate")
	}
}

func TestDirectOutboundNeverReachesTheGatewayOrItsNetworks(t *testing.T) {
	var settings struct {
		FinalRules []struct {
			Action string
			IP     []string
		}
	}
	raw, err := json.Marshal(directOutbound()["settings"])
	if err != nil || json.Unmarshal(raw, &settings) != nil {
		t.Fatal(err)
	}
	if len(settings.FinalRules) != 1 || settings.FinalRules[0].Action != "block" {
		t.Fatalf("rules %+v", settings.FinalRules)
	}
	for _, private := range []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "::1/128", "fc00::/7", "fe80::/10"} {
		if !slices.Contains(settings.FinalRules[0].IP, private) {
			t.Errorf("%s isn't blocked", private)
		}
	}
}

func TestAFlipKeepsTheGateUntilThePassHasRepairedIt(t *testing.T) {
	selected := testRef("srvA")
	slot := domain.Slot{ID: "slot1", Allocation: domain.Allocation{Ordinal: 1, TPROXYPort: 12341}}
	onTag := func(target SlotTarget, tag string, routine, passed bool) gate {
		core := coreObservation{Serving: map[domain.SlotID][]string{"slot1": {tag}}}
		return gate{up: true, wanted: true, coreRead: true, kernelRead: true, ingress: true, plumbed: true, settled: true, routine: routine, passed: passed,
			onInstalled: onInstalled(target, core, backoffStart)}
	}
	direct := SlotTarget{Slot: slot, Selection: domain.Direct()}
	vpn := SlotTarget{Slot: slot, Selection: domain.Serving(selected)}
	for _, c := range []struct {
		name       string
		g          gate
		keep, open bool
	}{
		{"to Direct, still on the server", onTag(direct, outboundTag(selected), true, false), true, false},
		{"to Direct, landed", onTag(direct, directTag, true, true), true, true},
		{"back to VPN, still on the WAN", onTag(vpn, directTag, true, false), true, false},
		{"back to VPN, landed", onTag(vpn, outboundTag(selected), true, true), true, true},
		{"on the WAN nobody asked for, with the plan unchanged", onTag(vpn, directTag, false, false), false, false},
		{"Direct wanted and the override lost, with the plan unchanged", onTag(direct, "block", false, false), false, false},
	} {
		if c.g.keep() != c.keep || c.g.open() != c.open {
			t.Errorf("%s: keep %v open %v, want %v %v", c.name, c.g.keep(), c.g.open(), c.keep, c.open)
		}
	}
}

func TestTheCoreDropsABlockedSlotsTraffic(t *testing.T) {
	plan := Plan{slots: []SlotTarget{workerTarget(workerSlot("slot1", 1), domain.Blocked(), Passage{})}}
	cp := (&Worker{}).corePlan(plan, backoffStart, book(nil))
	if len(cp.Slots) != 1 || !cp.Slots[0].Selection.IsBlocked() {
		t.Fatalf("slots %+v: a blocked slot keeps its inbound, so its tunnel stays up and drops", cp.Slots)
	}
	config := decode(t, must(render(cp)))
	balancers := config["routing"].(map[string]any)["balancers"].([]any)
	if got := toStrings(balancers[0].(map[string]any)["selector"]); !slices.Equal(got, []string{blockTag}) {
		t.Errorf("selector %v, want the blackhole", got)
	}
}
