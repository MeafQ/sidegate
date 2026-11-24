package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
)

// TestProbeRouteCost measures one on-demand probe route on the pinned core: the Worker's hotApply
// adding a probe's outbound, user and routing beside n routes already there and then taking them
// away, and each call those make on its own. It runs only with PROBE_COST_CYCLES set, in a network
// namespace of its own, since the core listens on the runtime's fixed API address. lab/probe-routes.sh
// runs it in the VM.
func TestProbeRouteCost(t *testing.T) {
	cycles, _ := strconv.Atoi(os.Getenv("PROBE_COST_CYCLES"))
	if cycles == 0 {
		t.Skip("PROBE_COST_CYCLES is not set")
	}
	bin := xrayBin(t)
	for _, n := range []int{12, 64} {
		t.Run(fmt.Sprintf("beside %d routes", n), func(t *testing.T) { probeRouteCost(t, bin, n, cycles) })
	}
}

func probeRouteCost(t *testing.T, bin string, n, cycles int) {
	ctx := context.Background()
	servers := map[domain.ServerRef]domain.Server{}
	var refs []domain.ServerRef
	for i := range n + 1 {
		vnext := obj{"address": fmt.Sprintf("198.51.100.%d", i+1), "port": 443, "users": []obj{{"id": liveUser, "encryption": "none"}}}
		stream := obj{"network": "raw", "security": "tls", "tlsSettings": obj{"serverName": "x.lab"}}
		ref := testRef(fmt.Sprintf("s%02d", i))
		servers[ref] = parsedServer(t, xrayFeed(t, obj{"protocol": "vless", "settings": obj{"vnext": []obj{vnext}}, "streamSettings": stream}), catalogue.XrayJSON)
		refs = append(refs, ref)
	}
	extra := refs[n]
	base := corePlan{Servers: servers, Probes: refs[:n]}
	with := corePlan{Servers: servers, Probes: refs}
	config, err := render(base)
	if err != nil {
		t.Fatal(err)
	}
	var parsed obj
	if err := json.Unmarshal(config, &parsed); err != nil {
		t.Fatal(err)
	}
	core := startXray(t, parsed)
	x := &xray{binary: bin, api: newCoreAPI(apiAddr)}
	if got := readCore(t, x); len(got.rules) != n || len(got.users) != n {
		t.Fatalf("the core came up with %d rules and %d probe users, want %d", len(got.rules), len(got.users), n)
	}

	routed := func(obs coreObservation) bool {
		return slices.Contains(obs.outbounds, outboundTag(extra)) && slices.Contains(obs.users, probeEmail(extra)) &&
			slices.Contains(obs.rules, routeRule{RuleTag: probeEmail(extra), Outbound: outboundTag(extra)})
	}
	apply := func(plan corePlan, want ...string) time.Duration {
		obs := readCore(t, x)
		began := time.Now()
		steps, err := passOn(x).hotApply(ctx, plan, obs, true)
		took := time.Since(began)
		if err != nil {
			t.Fatalf("hotApply: %v", err)
		}
		for _, w := range want {
			if !slices.Contains(steps, w) {
				t.Fatalf("hotApply took %v, without %q", steps, w)
			}
		}
		return took
	}
	add := []string{"core add outbound " + outboundTag(extra), "core add probe user " + probeEmail(extra), routingStep}
	remove := []string{"core remove probe user " + probeEmail(extra), "core remove outbound " + outboundTag(extra), routingStep}
	for range 5 {
		apply(with, add...)
		apply(base, remove...)
	}

	var adds, removes durations
	coreBefore, rssBefore := cpuOf(t, core.Pid), memOf(t, core.Pid, "VmRSS")
	self, children := usage(t, syscall.RUSAGE_SELF), usage(t, syscall.RUSAGE_CHILDREN)
	began := time.Now()
	for range cycles {
		adds = append(adds, apply(with, add...))
		if !routed(readCore(t, x)) {
			t.Fatal("the core doesn't route the probe hotApply added")
		}
		removes = append(removes, apply(base, remove...))
		obs := readCore(t, x)
		if slices.Contains(obs.outbounds, outboundTag(extra)) || slices.Contains(obs.users, probeEmail(extra)) || len(obs.rules) != n {
			t.Fatal("the core still holds part of the probe hotApply removed")
		}
	}
	wall := time.Since(began)
	coreCPU := cpuOf(t, core.Pid) - coreBefore
	selfCPU, childCPU := usage(t, syscall.RUSAGE_SELF)-self, usage(t, syscall.RUSAGE_CHILDREN)-children
	t.Logf("measured: hotApply add, one route beside %d: %s", n, adds)
	t.Logf("measured: hotApply remove, one route beside %d: %s", n, removes)
	t.Logf("measured: %d cycles in %s wall. CPU per cycle: core %s, xray api and timeout processes %s, this process %s (readback included)",
		cycles, wall.Round(time.Millisecond), per(coreCPU, cycles), per(childCPU, cycles), per(selfCPU, cycles))
	t.Logf("measured: core RSS %d MB before, %d MB after, high water %d MB", rssBefore/1024, memOf(t, core.Pid, "VmRSS")/1024, memOf(t, core.Pid, "VmHWM")/1024)

	outbounds, err := renderOutbounds(with, []domain.ServerRef{extra})
	if err != nil {
		t.Fatal(err)
	}
	server := "--server=" + apiAddr.String()
	oneRule := obj{"routing": obj{"rules": []obj{renderRouting(with)["rules"].([]obj)[n]}}}
	addOutbound := func() error { return passOn(x).addConfig(ctx, noSlot, "ado", obj{"outbounds": outbounds}) }
	removeOutbound := func() error { return passOn(x).removeOutbound(ctx, outboundTag(extra)) }
	addUser := func() error {
		return passOn(x).addUser(ctx, probeEmail(extra), uuidString(probeUser(extra)))
	}
	removeUser := func() error { return passOn(x).removeUser(ctx, probeEmail(extra)) }
	addRule := func() error { return appendRule(ctx, x, bin, server, oneRule) }
	removeRule := func() error { _, err := x.run(ctx, nil, bin, "api", "rmrules", server, probeEmail(extra)); return err }
	// Between calls the extra outbound stands alone, so a rule naming it always has it.
	calls := []struct {
		name                 string
		setup, do, undo, end func() error
	}{
		{name: "start of the xray binary (xray version)", do: func() error { _, err := x.run(ctx, nil, bin, "version"); return err }},
		{name: "ado one outbound, xray api", setup: removeOutbound, do: addOutbound, undo: removeOutbound, end: addOutbound},
		{name: "RemoveOutbound, gRPC", do: removeOutbound, undo: addOutbound},
		{name: "AlterInbound add user, gRPC", do: addUser, undo: removeUser},
		{name: "AlterInbound remove user, gRPC", setup: addUser, do: removeUser, undo: addUser, end: removeUser},
		{name: fmt.Sprintf("adrules whole routing of %d rules, xray api", n+1), do: func() error {
			return passOn(x).addConfig(ctx, everySlot, "adrules", obj{"routing": renderRouting(with)})
		},
			undo: func() error {
				return passOn(x).addConfig(ctx, everySlot, "adrules", obj{"routing": renderRouting(base)})
			}},
		{name: "adrules -append one rule, xray api", do: addRule, undo: removeRule},
		{name: "rmrules one rule, xray api", setup: addRule, do: removeRule, undo: addRule, end: removeRule},
	}
	if err := addOutbound(); err != nil {
		t.Fatal(err)
	}
	step := func(name string, f func() error) {
		if f == nil {
			return
		}
		if err := f(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, c := range calls {
		step(c.name+", setting up", c.setup)
		var times durations
		for range cycles {
			began := time.Now()
			step(c.name, c.do)
			times = append(times, time.Since(began))
			step(c.name+", undoing", c.undo)
		}
		step(c.name+", putting back", c.end)
		t.Logf("measured: %s, beside %d: %s", c.name, n, times)
	}
	step("removing the extra outbound", removeOutbound)
	if obs := readCore(t, x); len(obs.rules) != n || len(obs.users) != n {
		t.Fatalf("the calls left %d rules and %d users, want %d", len(obs.rules), len(obs.users), n)
	}
}

func appendRule(ctx context.Context, x *xray, bin, server string, routing obj) error {
	body, err := json.Marshal(routing)
	if err != nil {
		return err
	}
	_, err = x.run(ctx, body, bin, "api", "adrules", "-append", server, "stdin:")
	return err
}

// readCore is what a Worker pass reads back before it applies anything.
func readCore(t *testing.T, x *xray) coreObservation {
	t.Helper()
	ctx := context.Background()
	var obs coreObservation
	var err error
	if obs.inbounds, err = x.api.listInbounds(ctx); err == nil {
		if obs.outbounds, err = x.api.listOutbounds(ctx); err == nil {
			if obs.rules, err = x.api.listRules(ctx); err == nil {
				obs.users, err = x.api.inboundUsers(ctx, probeTag)
			}
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

type durations []time.Duration

// String is the median and the nearest-rank 95th percentile.
func (d durations) String() string {
	s := slices.Sorted(slices.Values(d))
	rank := func(q float64) time.Duration { return s[int(math.Ceil(q*float64(len(s))))-1] }
	ms := func(v time.Duration) float64 { return float64(v.Microseconds()) / 1000 }
	return fmt.Sprintf("p50 %.1f ms, p95 %.1f ms, max %.1f ms, n=%d", ms(rank(0.5)), ms(rank(0.95)), ms(s[len(s)-1]), len(s))
}

func per(d time.Duration, n int) string {
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000/float64(n))
}

// cpuOf is pid's user and system time. Linux reports it in USER_HZ, 100 a second on arm64.
func cpuOf(t *testing.T, pid int) time.Duration {
	t.Helper()
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))
	user, _ := strconv.ParseInt(fields[11], 10, 64)
	system, _ := strconv.ParseInt(fields[12], 10, 64)
	return time.Duration(user+system) * 10 * time.Millisecond
}

// memOf is a VmRSS or VmHWM line of pid's status, in KB.
func memOf(t *testing.T, pid int, field string) int {
	t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, field+":"); ok {
			kb, _ := strconv.Atoi(strings.Fields(rest)[0])
			return kb
		}
	}
	t.Fatalf("no %s in /proc/%d/status", field, pid)
	return 0
}

func usage(t *testing.T, who int) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(who, &ru); err != nil {
		t.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
