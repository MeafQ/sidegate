package runtime

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sidegate/internal/domain"
)

func labPlumbing(ordinal uint8, open bool) plumbing {
	return plumbing{
		Slot: domain.SlotID("slot" + string('0'+ordinal)),
		Allocation: domain.Allocation{
			Ordinal: ordinal, Tunnel: netip.MustParsePrefix("10.70." + string('0'+ordinal) + ".0/30"),
			PeerListenPort: 51830 + uint16(ordinal), TPROXYPort: 12340 + uint16(ordinal),
			Keys: domain.SlotKeys{ClientPublic: "client" + string('0'+ordinal)},
		},
		HostKey: domain.NewSecret("host" + string('0'+ordinal)),
		Open:    open,
	}
}

// observed is what inspection would read back after spec was applied exactly.
func observed(spec kernelSpec) kernelObs {
	obs := kernelObs{links: map[string]link{}, chains: map[chainRef][]string{}, jumps: map[chainRef][]int{},
		rules: slices.Clone(spec.rules), routes: slices.Clone(spec.routes), allRPFilter: "0"}
	for name, l := range spec.links {
		l.slot = ""
		obs.links[name] = l
	}
	for ref, lines := range spec.chains {
		obs.chains[ref] = slices.Clone(lines)
		obs.jumps[ref] = []int{0}
	}
	return obs
}

func kinds(changes []change) []string {
	var out []string
	for _, c := range changes {
		out = append(out, c.String())
	}
	return out
}

func TestDiffFromNothingBuildsGuardBeforeListenersAndOpensLast(t *testing.T) {
	k := &kernel{}
	spec := k.render(kernelWant{slots: []plumbing{labPlumbing(1, true)}})
	got := kinds(diff(spec, kernelObs{allRPFilter: "0"}))
	want := []string{
		"iptables raw sidegate-prerouting write",
		"iptables mangle sidegate-prerouting write",
		"iptables filter sidegate-input write",
		"iptables filter sidegate-output write",
		"ip6tables filter sidegate-input write",
		"ip rule add 99: from all fwmark 0x169 lookup 100 proto 85",
		"ip rule add 100: from all iif pxa1 unreachable proto 85",
		"ip route add local default dev lo proto 85 scope 254 table 100",
		"link pxa1 create",
		"link pxa1 wireguard config",
		"link pxa1 sysctl accept_local=1 rp_filter=0",
		"link pxa1 up",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("changes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDiffOfAppliedSpecIsEmpty(t *testing.T) {
	k := &kernel{}
	spec := k.render(kernelWant{slots: []plumbing{labPlumbing(1, true), labPlumbing(2, false)}})
	if got := diff(spec, observed(spec)); len(got) != 0 {
		t.Fatalf("second reconcile would change %v", kinds(got))
	}
}

func TestForeignFlushRewritesOnlyOurChainAndClosesNothingItCanRepair(t *testing.T) {
	k := &kernel{}
	spec := k.render(kernelWant{slots: []plumbing{labPlumbing(1, true)}})
	have := observed(spec)
	mangle := ownedChains[1]
	delete(have.jumps, mangle)
	have.rules = have.rules[1:]

	if ready(spec, have, "slot1") {
		t.Fatal("slot ready with its jump and mark rule gone")
	}
	got := diff(spec, have)
	if want := []string{"iptables mangle sidegate-prerouting write", "ip rule add 99: from all fwmark 0x169 lookup 100 proto 85"}; !slices.Equal(kinds(got), want) {
		t.Fatalf("repair = %v, want %v", kinds(got), want)
	}
	payload := got[0].text
	for _, line := range []string{"*mangle", ":sidegate-prerouting - [0:0]", "-I PREROUTING 1 -j sidegate-prerouting", "COMMIT"} {
		if !strings.Contains(payload, line+"\n") {
			t.Errorf("payload lacks %q:\n%s", line, payload)
		}
	}
	if strings.Contains(payload, "-D PREROUTING") || strings.Contains(payload, "-F PREROUTING") {
		t.Errorf("payload touches more than our jump:\n%s", payload)
	}
}

func TestMisplacedOrDuplicatedJumpIsReinsertedFirst(t *testing.T) {
	k := &kernel{}
	spec := k.render(kernelWant{slots: []plumbing{labPlumbing(1, true)}})
	have := observed(spec)
	have.jumps[ownedChains[1]] = []int{1, 3}
	got := diff(spec, have)
	if len(got) != 1 {
		t.Fatalf("changes %v", kinds(got))
	}
	if n := strings.Count(got[0].text, "-D PREROUTING -j sidegate-prerouting\n"); n != 2 {
		t.Errorf("deletes %d jumps, want 2:\n%s", n, got[0].text)
	}
}

func TestAddingASlotKeepsTheOtherReady(t *testing.T) {
	k := &kernel{}
	before := k.render(kernelWant{slots: []plumbing{labPlumbing(1, true)}})
	after := k.render(kernelWant{slots: []plumbing{labPlumbing(1, true), labPlumbing(2, false)}})
	have := observed(before)
	if !ready(after, have, "slot1") {
		t.Fatal("adding slot2 made slot1 unready, which would close its gate")
	}
	if ready(after, have, "slot2") {
		t.Fatal("slot2 ready before its plumbing exists")
	}
	for _, c := range diff(after, have) {
		if c.link == "pxa1" {
			t.Errorf("adding slot2 changes pxa1: %s", c)
		}
	}
}

func egressGuard(tunnel string) []string {
	return []string{
		"-A sidegate-output -s " + tunnel + " -o wgclt+ -j RETURN",
		"-A sidegate-output -s " + tunnel + " ! -o lo -j DROP",
	}
}

func TestEachSlotsTunnelLeavesOnlyByAClientOrLoopback(t *testing.T) {
	k := &kernel{}
	spec := k.render(kernelWant{slots: []plumbing{labPlumbing(1, true), labPlumbing(2, false)}})
	output := ownedChains[3]
	chain := spec.chains[output]
	for _, tunnel := range []string{"10.70.1.0/30", "10.70.2.0/30"} {
		if !containsRun(chain, egressGuard(tunnel)) {
			t.Errorf("sidegate-output lacks %s's RETURN by wgclt+ then DROP but by lo:\n%s", tunnel, strings.Join(chain, "\n"))
		}
	}
	var payload string
	for _, c := range diff(spec, kernelObs{allRPFilter: "0"}) {
		if c.kind == chainWrite && c.chain == output {
			payload = c.text
		}
	}
	if !strings.Contains(payload, "-I OUTPUT 1 -j sidegate-output\n") {
		t.Errorf("the guard's chain is not jumped to first in OUTPUT:\n%s", payload)
	}
}

func TestASlotIsNotReadyWithoutItsEgressGuard(t *testing.T) {
	k := &kernel{}
	spec := k.render(kernelWant{slots: []plumbing{labPlumbing(1, true), labPlumbing(2, true)}})
	output := ownedChains[3]
	for _, line := range egressGuard("10.70.1.0/30") {
		have := observed(spec)
		i := slices.Index(have.chains[output], line)
		if i < 0 {
			t.Fatalf("render lacks %q", line)
		}
		have.chains[output] = slices.Delete(have.chains[output], i, i+1)
		if ready(spec, have, "slot1") {
			t.Errorf("slot1 ready over a read without %q", line)
		}
		if !ready(spec, have, "slot2") {
			t.Errorf("slot1's missing %q made slot2 unready", line)
		}
		if got := kinds(diff(spec, have)); !slices.Equal(got, []string{"iptables filter sidegate-output write"}) {
			t.Errorf("repair of a missing %q = %v", line, got)
		}
	}
}

func TestEmptySpecRemovesOnlyWhatWeOwn(t *testing.T) {
	k := &kernel{}
	have := observed(k.render(kernelWant{slots: []plumbing{labPlumbing(1, true)}}))
	got := kinds(diff(k.render(kernelWant{}), have))
	want := []string{
		"link pxa1 down",
		"ip rule delete 99: from all fwmark 0x169 lookup 100 proto 85",
		"ip rule delete 100: from all iif pxa1 unreachable proto 85",
		"link pxa1 delete",
		"iptables raw sidegate-prerouting write",
		"iptables mangle sidegate-prerouting write",
		"iptables filter sidegate-input write",
		"iptables filter sidegate-output write",
		"ip6tables filter sidegate-input write",
		"ip route delete local default dev lo proto 85 scope 254 table 100",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("teardown:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	payload := diff(k.render(kernelWant{}), have)[5].text
	if want := "*mangle\n-D PREROUTING -j sidegate-prerouting\n-F sidegate-prerouting\n-X sidegate-prerouting\nCOMMIT\n"; payload != want {
		t.Fatalf("teardown payload:\n%s", payload)
	}
}

func TestClosingAGateTouchesOnlyItsLink(t *testing.T) {
	k := &kernel{}
	have := observed(k.render(kernelWant{slots: []plumbing{labPlumbing(1, true), labPlumbing(2, true)}}))
	got := kinds(diff(k.render(kernelWant{slots: []plumbing{labPlumbing(1, false), labPlumbing(2, true)}}), have))
	if !slices.Equal(got, []string{"link pxa1 down"}) {
		t.Fatalf("changes %v", got)
	}
}

func TestParseSaveFindsOurChainsAndJumpPositions(t *testing.T) {
	save := `# Generated by iptables-save v1.8.4
*mangle
:PREROUTING ACCEPT [10:20]
:UBIOS_PREROUTING_USER_HOOK - [0:0]
:sidegate-prerouting - [0:0]
-A PREROUTING -j UBIOS_PREROUTING_USER_HOOK
-A PREROUTING -j sidegate-prerouting
-A UBIOS_PREROUTING_USER_HOOK -d 203.0.113.81/32 -j MARK --set-xmark 0x6b0000/0x7f0000
-A sidegate-prerouting -i pxa1 -j DROP
COMMIT
*filter
:INPUT ACCEPT [0:0]
-A INPUT -j ACCEPT
COMMIT
`
	chains, jumps := parseSave([]byte(save), "iptables")
	mangle := ownedChains[1]
	if !slices.Equal(chains[mangle], []string{"-A sidegate-prerouting -i pxa1 -j DROP"}) {
		t.Errorf("mangle chain %q", chains[mangle])
	}
	if !slices.Equal(jumps[mangle], []int{1}) {
		t.Errorf("mangle jumps %v, want [1]", jumps[mangle])
	}
	if _, ok := chains[ownedChains[2]]; ok {
		t.Error("filter chain reported without a declaration")
	}
	if _, ok := chains[ownedChains[0]]; ok {
		t.Error("raw chain reported for a table iptables-save did not print")
	}
	if v6, _ := parseSave([]byte(save), "ip6tables"); len(v6) != 0 {
		t.Errorf("iptables-save output read as ip6tables chains: %v", v6)
	}
}

func TestParseRulesKeepsOurPreferencesNormalized(t *testing.T) {
	out := "0:\tfrom all lookup 255\n99:\tfrom all fwmark 0x169 lookup 100\n100:\tfrom all iif pxa3 [detached] 7\n32766:\tfrom all lookup 254\n"
	got, err := parseRules([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range got {
		texts = append(texts, r.text)
	}
	if want := []string{"0: from all lookup 255", "99: from all fwmark 0x169 lookup 100", "100: from all iif pxa3 unreachable"}; !slices.Equal(texts, want) {
		t.Fatalf("rules %q", texts)
	}
}

// classify runs rules, as `ip -N rule show` prints them, through sortRules with
// slot1 on pxa1.
func classify(t *testing.T, lines ...string) (ours, foreign []string) {
	t.Helper()
	rules, err := parseRules([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return sortRules(rules, map[string]netip.Prefix{"pxa1": netip.MustParsePrefix("10.70.1.0/30")}, nil)
}

func TestOnlyOurExactRulesInReachAreOurs(t *testing.T) {
	ours, foreign := classify(t,
		"99: from all fwmark 0x169 lookup 100 proto 85",
		"100: from all iif pxa1 7 proto 85",
		"100: from all iif pxa1 7",
		"100: from all iif pxa1 lookup 254 proto 85",
		"99: from all fwmark 0x169 lookup 100",
		"100: from all iif pxa9 7",
		"100: from all iif pxa9 7 proto 85",
		"100: from all iif foreign0 7 proto 85",
	)
	if want := []string{"99: from all fwmark 0x169 lookup 100 proto 85", "100: from all iif pxa1 unreachable proto 85"}; !slices.Equal(ours, want) {
		t.Errorf("ours %q, want %q", ours, want)
	}
	if want := []string{"100: from all iif pxa1 unreachable", "100: from all iif pxa1 lookup 254 proto 85", "99: from all fwmark 0x169 lookup 100"}; !slices.Equal(foreign, want) {
		t.Errorf("foreign %q, want %q: a rule on a link outside reach is neither, marker or no marker", foreign, want)
	}
}

func TestAForeignRuleIsAConflictOnlyIfItCanMatchPxaTraffic(t *testing.T) {
	cases := []struct {
		name, rule string
		conflict   bool
	}{
		{"iif outside our links", "100:\tfrom all iif wg9 [detached] lookup 52", false},
		{"source, then iif outside", "100:\tfrom 10.0.0.0/8 iif wg9 [detached] lookup 52", false},
		{"fwmark, then iif outside", "100:\tfrom all fwmark 0x80000/0xff0000 iif wg9 [detached] lookup 52", false},
		{"destination, then iif outside", "100:\tfrom all to 10.0.0.0/8 iif wg9 [detached] lookup 52", false},
		{"tos, then iif outside", "100:\tfrom all tos 0x10 iif wg9 [detached] lookup 52", false},
		{"negated iif outside", "100:\tnot from all iif wg9 [detached] lookup 52", true},
		{"negated iif on the only link", "100:\tnot from all iif pxa1 lookup 52", false},
		{"fwmark, then iif on our link", "100:\tfrom all fwmark 0x169 iif pxa1 lookup 52", true},
		{"a mark pxa traffic never carries", "100:\tfrom all fwmark 0x1 iif pxa1 7 proto 85", false},
		{"Tailscale's mark", "100:\tfrom all fwmark 0x80000/0xff0000 lookup 52", false},
		{"Tailscale's negated mark at our preference", "100:\tnot from all fwmark 0x80000/0xff0000 lookup 52", true},
		{"Tailscale's negated mark after ours", "5270:\tnot from all fwmark 0x80000/0xff0000 lookup 52", false},
		{"goto after ours", "5290:\tfrom 10.77.0.0/16 goto 32000", false},
		{"goto at our preference", "100:\tfrom all goto 5270 [unresolved]", true},
		{"goto from outside the tunnels", "100:\tfrom 10.77.0.0/16 goto 5270", false},
		{"a source no tunnel holds", "100:\tfrom 10.99.0.1 lookup 52", false},
		{"a source inside the tunnel", "100:\tfrom 10.70.1.2 lookup 52", true},
		{"a destination", "100:\tfrom all to 8.8.8.8 lookup 52", true},
		{"tos", "100:\tfrom all tos 0x10 lookup 52", true},
		{"output device", "100:\tfrom all oif eth4 lookup 52", false},
		{"a selector this reader does not know", "100:\tfrom all dscp 8 lookup 52", true},
		{"before ours", "50:\tfrom all lookup 52", true},
		{"the local table", "0:\tfrom all lookup 255", false},
		{"blackhole", "99:\tfrom all blackhole", true},
		{"after ours", "32765:\tfrom all fwmark 0x10000/0x10000 blackhole", false},
	}
	for _, c := range cases {
		_, foreign := classify(t, c.rule)
		if got := len(foreign) > 0; got != c.conflict {
			t.Errorf("%s: %q: conflict = %v, want %v", c.name, c.rule, got, c.conflict)
		}
	}
}

func TestTeardownRemovesOurRulesWithoutTheirLinksAndNothingUnmarked(t *testing.T) {
	k := &kernel{}
	have := observed(k.render(kernelWant{slots: []plumbing{labPlumbing(1, true)}}))
	clear(have.links)
	parsed, err := parseRules([]byte(strings.Join(append(slices.Clone(have.rules), "100: from all iif pxa1 unreachable"), "\n")))
	if err != nil {
		t.Fatal(err)
	}
	have.rules, have.foreignRules = sortRules(parsed, map[string]netip.Prefix{}, []string{"pxa1"})
	got := kinds(diff(k.render(kernelWant{}), have))
	for _, c := range got {
		if strings.Contains(c, "rule") && !strings.HasSuffix(c, " proto 85") {
			t.Errorf("teardown touches an unmarked rule: %s", c)
		}
	}
	if !slices.Contains(got, "ip rule delete 100: from all iif pxa1 unreachable proto 85") {
		t.Errorf("teardown kept our pxa1 rule once pxa1 was gone: %v", got)
	}
	if len(conflicts(have)) != 0 {
		t.Errorf("an unmarked rule at a name nobody wants is a conflict: %v", conflicts(have))
	}
}

func TestConflictsAreReportedNotTakenOver(t *testing.T) {
	k := &kernel{}
	have := observed(k.render(kernelWant{slots: []plumbing{labPlumbing(1, true)}}))
	if c := conflicts(have); len(c) != 0 {
		t.Fatalf("our own state reported as conflicts: %v", c)
	}
	have.foreignRules = []string{"100: from all iif pxa1 lookup main"}
	have.foreignRoutes = []string{"local default dev lo scope host"}
	have.foreign = []string{"pxa2"}
	have.allRPFilter = "2"
	if c := conflicts(have); len(c) != 4 {
		t.Fatalf("conflicts %q, want the rule, the route, the link and rp_filter", c)
	}
}

// fakeIP puts an ip on PATH that records its arguments, and returns the record.
func fakeIP(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte("#!/bin/sh\necho \"$*\" >>"+record+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

// opener is an opening that confirms and records each slot it opens.
type opener struct {
	mu     sync.Mutex
	opened []domain.SlotID
}

func (o *opener) open(_ context.Context, slot domain.SlotID) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened = append(o.opened, slot)
	return nil
}

func TestAGateOpensOnlyOnAConfirmationCloseGatesCannotInterleave(t *testing.T) {
	calls := fakeIP(t)
	k := &kernel{gateLock: filepath.Join(t.TempDir(), "gates.lock")}
	ctx := context.Background()

	err := k.open(ctx, "slot1", func(context.Context, domain.SlotID) error {
		if lock, err := lockFile(k.gateLock, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			lock.Close()
			t.Error("close-gates could take the gate lock between the confirmation and the link up")
		}
		return errors.New("core restarted")
	})
	if err == nil {
		t.Fatal("a failed confirmation opened the gate")
	}
	if err := k.open(ctx, "slot1", nil); err == nil {
		t.Fatal("a gate opened with nothing to confirm it")
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a tool ran for an opening")
	}

	var o opener
	if err := k.open(ctx, "slot1", o.open); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.opened, []domain.SlotID{"slot1"}) {
		t.Errorf("opened %v", o.opened)
	}
	lock, err := lockFile(k.gateLock, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		t.Fatal("the gate lock is still held after the gate opened")
	}
	lock.Close()
}

func TestOneLinksFailureStopsOnlyWhatDependsOnIt(t *testing.T) {
	calls := fakeIP(t)
	script := "#!/bin/sh\necho \"$*\" >>" + calls + "\ncase \"$*\" in *pxa1*) echo busy >&2; exit 2;; esac\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(calls), "ip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	k := &kernel{gateLock: filepath.Join(t.TempDir(), "gates.lock")}
	spec := k.render(kernelWant{slots: []plumbing{labPlumbing(1, true), labPlumbing(2, true)}})
	var o opener
	confirm := o.open
	changes := []change{
		{kind: ruleAdd, link: "pxa1", text: ingressRule("pxa1")},
		{kind: linkUp, link: "pxa1"},
		{kind: linkUp, link: "pxa2"},
	}
	_, done, err := k.applyAll(context.Background(), changes, spec, kernelObs{}, confirm)
	if err == nil || !strings.Contains(err.Error(), "pxa1") {
		t.Errorf("error = %v, want pxa1's", err)
	}
	if !slices.Equal(done, []string{"link pxa2 up"}) {
		t.Errorf("done %q", done)
	}
	got, _ := os.ReadFile(calls)
	if want := "rule add pref 100 iif pxa1 unreachable proto 85\n"; string(got) != want || !slices.Equal(o.opened, []domain.SlotID{"slot2"}) {
		t.Errorf("ip calls %q, want %q, and opened %v, want slot2: a link whose rule failed must not open", got, want, o.opened)
	}

	os.Remove(calls)
	shared := []change{{kind: ruleAdd, text: "100: from all iif pxa1 unreachable proto 85"}, {kind: linkUp, link: "pxa2"}}
	if _, done, _ := k.applyAll(context.Background(), shared, spec, kernelObs{}, confirm); len(done) != 0 {
		t.Errorf("a gate opened after shared state failed: %q", done)
	}
}

func TestWhileTheCoreRunsOnlyRootReachesItsListeners(t *testing.T) {
	k := &kernel{}
	output := ownedChains[3]
	probesOnly := k.render(kernelWant{core: true})
	if want := []string{"-A sidegate-output -d 127.0.0.1/32 -o lo -p tcp -m multiport --dports 10085,10086 -m owner --uid-owner 1-4294967294 -j REJECT --reject-with tcp-reset"}; !slices.Equal(probesOnly.chains[output], want) {
		t.Errorf("a core with no slots guards %q", probesOnly.chains[output])
	}
	if len(probesOnly.links) != 0 || len(probesOnly.rules) != 0 || len(probesOnly.routes) != 0 {
		t.Error("a core with no slots got slot plumbing")
	}
	slots := k.render(kernelWant{slots: []plumbing{labPlumbing(2, true), labPlumbing(1, true)}, core: true})
	guard := slots.chains[output][:2]
	if !strings.Contains(guard[0], "-p tcp -m multiport --dports 10085,10086,12341,12342 ") ||
		!strings.Contains(guard[1], "-p udp -m multiport --dports 12341,12342 ") {
		t.Errorf("guard %q", guard)
	}
	if !ready(slots, observed(slots), "slot1") {
		t.Error("the guard ahead of a slot's lines keeps it from reading back ready")
	}
	if got := kinds(diff(k.render(kernelWant{}), observed(probesOnly))); !slices.Contains(got, "iptables filter sidegate-output write") {
		t.Errorf("teardown left the guard: %v", got)
	}
}

func TestDumpReadsBothWireGuardFormats(t *testing.T) {
	for name, dump := range map[string]string{
		"standard": "priv\tpub\t51831\toff\npeer\t(none)\t(none)\t10.70.1.0/30\t0\t0\t0\t25\n",
		"ubiquiti": "priv\tpub\t(none)\t51831\toff\npeer\t(none)\t(none)\t10.70.1.0/30\t0\t0\t0\t25\t0\n",
	} {
		l := link{peers: map[string]peer{}}
		if err := parseDump(&l, "pxa1", []byte(dump)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if l.privateKey != "priv" || l.listenPort != 51831 || l.peers["peer"] != (peer{allowedIPs: "10.70.1.0/30", keepalive: "25"}) {
			t.Errorf("%s: %+v", name, l)
		}
	}
	if err := parseDump(&link{peers: map[string]peer{}}, "pxa1", []byte("a\tb\tc\n")); err == nil {
		t.Error("a line of another shape was accepted")
	}
}

func TestAHandshakeSaysTheClientIsConnected(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	dump := func(at int64) []byte {
		// Ubiquiti's wg adds a field to each line.
		return fmt.Appendf(nil, "priv\tpub\t0\t51831\toff\npeer\t(none)\t127.0.0.1:1\t10.70.1.2/32\t%d\t1\t2\t25\t0\n", at)
	}
	cases := []struct {
		name string
		at   int64
		up   bool
		want bool
	}{
		{"never", 0, true, false},
		{"just now", now.Unix() - 5, true, true},
		{"a rekey ago", now.Unix() - 125, true, true},
		{"gone quiet", now.Unix() - 200, true, false},
		{"gate closed", now.Unix() - 5, false, false},
	}
	for _, c := range cases {
		l := link{peers: map[string]peer{}, up: c.up}
		if err := parseDump(&l, "pxa1", dump(c.at)); err != nil {
			t.Fatal(err)
		}
		if got := tunnelAlive(l, now); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// Step portheld passes only on the refusal of pxa1's own up, which a link writer reports in Go's words
// for the errno, lowercase, where iproute2 capitalised them.
func TestTheHeldPortStepReadsTheRefusalALinkUpReports(t *testing.T) {
	srv := domain.Serving(testRef("srv"))
	plan := &Plan{slots: []SlotTarget{workerTarget(workerSlot("slot1", 1), srv, Passage{}), workerTarget(workerSlot("slot2", 2), srv, Passage{})}}
	fakeLinks(t, map[string]int{"pxa1": linkGroup, "pxa2": linkGroup})
	g := &gates{plan: plan, write: func(string, bool) error { return syscall.EADDRINUSE }}
	k := &kernel{gateLock: filepath.Join(t.TempDir(), "gates.lock"), gates: g}
	want := kernelSpec{links: map[string]link{"pxa1": {slot: "slot1"}, "pxa2": {slot: "slot2"}}}
	open := func(_ context.Context, slot domain.SlotID) error { return g.open(plan, slot, srv) }
	refused := func(c change) string {
		err := k.apply(context.Background(), c, want, open)
		if err == nil {
			t.Fatalf("%s was not refused", c)
		}
		return err.Error()
	}
	for log, reported := range map[string]bool{
		refused(change{kind: linkUp, link: "pxa1"}):   true,
		refused(change{kind: linkUp, link: "pxa2"}):   false,
		refused(change{kind: linkDown, link: "pxa1"}): false,
	} {
		step := exec.Command("bash", "-c", `source "$1" 2>/dev/null && refused_up pxa1`, "portheld", "../../lab/runtime.sh")
		step.Stdin = strings.NewReader("pass 3: 1 steps: " + log + "\n")
		if err := step.Run(); (err == nil) != reported {
			t.Errorf("refused_up pxa1 on %q: %v, want reported %t", log, err, reported)
		}
	}
}

// linkMessage is one RTM_NEWLINK message of a dump, as rtnetlink writes it.
func linkMessage(flags uint16, name string, ifiFlags uint32, group uint32) []byte {
	order := binary.NativeEndian
	attr := func(kind uint16, value []byte) []byte {
		b := order.AppendUint16(nil, uint16(4+len(value)))
		b = append(order.AppendUint16(b, kind), value...)
		return append(b, make([]byte, align4(len(b))-len(b))...)
	}
	body := make([]byte, ifinfoHeader)
	order.PutUint32(body[8:], ifiFlags)
	body = append(body, attr(iflaIfname, append([]byte(name), 0))...)
	body = append(body, attr(iflaGroup, order.AppendUint32(nil, group))...)
	return netlinkMessage(rtmNewLink, flags, body)
}

func doneMessage(status int32) []byte {
	return netlinkMessage(nlmsgDone, 0, binary.NativeEndian.AppendUint32(nil, uint32(status)))
}

func netlinkMessage(kind, flags uint16, body []byte) []byte {
	order := binary.NativeEndian
	b := order.AppendUint32(nil, uint32(nlmsgHeader+len(body)))
	b = order.AppendUint16(order.AppendUint16(b, kind), flags)
	b = order.AppendUint32(order.AppendUint32(b, 1), 0)
	return append(b, body...)
}

// A link's up and its group come from one message, so no two moments meet in one link. A dump the
// kernel marks interrupted, or one that ends on an error, is no sample.
func TestALinkSampleIsOneMessage(t *testing.T) {
	whole := slices.Concat(linkMessage(0, "lo", syscall.IFF_UP, 0), linkMessage(0, "pxa1", syscall.IFF_UP, linkGroup),
		linkMessage(0, "pxa2", 0, linkGroup), linkMessage(0, "pxa3", syscall.IFF_UP, 4242), doneMessage(0))
	links, err := parseLinkDump(whole)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]linkSample{"lo": {up: true}, "pxa1": {up: true, ours: true}, "pxa2": {ours: true}, "pxa3": {up: true}}
	if !maps.Equal(links, want) {
		t.Errorf("the dump read %v, want %v", links, want)
	}
	for name, dump := range map[string][]byte{
		"interrupted":            slices.Concat(linkMessage(0, "pxa1", syscall.IFF_UP, linkGroup), linkMessage(nlmFDumpIntr, "pxa2", 0, linkGroup), doneMessage(0)),
		"ended on an error":      slices.Concat(linkMessage(0, "lo", syscall.IFF_UP, 0), doneMessage(-4)),
		"with no NLMSG_DONE":     linkMessage(0, "pxa1", syscall.IFF_UP, linkGroup),
		"cut inside its message": whole[:20],
	} {
		if links, err := parseLinkDump(dump); err == nil {
			t.Errorf("a dump %s read as %v", name, links)
		}
	}
}
