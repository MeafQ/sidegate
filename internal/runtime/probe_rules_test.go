package runtime

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"sidegate/internal/domain"
)

// probeRuleCore is a core holding one slot on srv and probe routes for its probes. Its gRPC side answers
// readback and records every call, and its `xray api` side is a script that logs each command line.
type probeRuleCore struct {
	x       *xray
	log     string
	mu      sync.Mutex
	calls   []string
	removed []string // The tags of the inbounds removed.
}

func newProbeRuleCore(t *testing.T) *probeRuleCore {
	t.Helper()
	dir := t.TempDir()
	c := &probeRuleCore{log: filepath.Join(dir, "xray.log")}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("xray", "#!/bin/sh\necho \"$* $(cat)\" >>"+c.log+"\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	c.x = &xray{binary: filepath.Join(dir, "xray"), api: fakeCore(t, func(method string, req []byte) ([]byte, string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls = append(c.calls, method)
		if method == handlerService+"RemoveInbound" {
			pbFields(req, func(num int, v []byte) error {
				if num == 1 {
					c.removed = append(c.removed, string(v))
				}
				return nil
			})
		}
		if method == routingService+"GetBalancerInfo" {
			return pbBytes(nil, 1, pbBytes(nil, 5, pbString(nil, 2, outboundTag(testRef("srv"))))), "0"
		}
		return nil, "0"
	})}
	return c
}

func (c *probeRuleCore) commands(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(c.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return slices.DeleteFunc(strings.Split(string(body), "\n"), func(l string) bool { return l == "" })
}

func (c *probeRuleCore) called(method string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.calls, method)
}

// probeRulePlan is the one slot on srv with a probe route for each of probes.
func probeRulePlan(t *testing.T, probes ...string) corePlan {
	srv := testRef("srv")
	cp := corePlan{Servers: map[domain.ServerRef]domain.Server{srv: testServer(t, vlessHop(t, "root", "198.51.100.1", ""))},
		Slots: []coreSlot{{ID: "slot1", Port: 12341, Selection: domain.Serving(srv)}}}
	for i, p := range probes {
		ref := testRef(p)
		cp.Probes = append(cp.Probes, ref)
		cp.Servers[ref] = testServer(t, vlessHop(t, "root", "198.51.100."+string(rune('2'+i)), ""))
	}
	return cp
}

// readbackOf is what readback shows once cp is in the core: its outbounds, users and rules, and the slot
// on its override.
func readbackOf(cp corePlan) coreObservation {
	obs := coreObservation{Running: true, Applied: map[domain.SlotID]domain.Selection{}, inbounds: append(inboundTags("slot1"), probeTag),
		outbounds: []string{blockTag, directTag}, rules: cp.rules()}
	for _, ref := range cp.installed() {
		obs.outbounds = append(obs.outbounds, outboundTag(ref))
	}
	for _, ref := range cp.probeUsers() {
		obs.users = append(obs.users, probeEmail(ref))
	}
	for _, s := range cp.Slots {
		obs.Applied[s.ID] = s.Selection
	}
	return obs
}

// A probe route added or removed beside a serving slot changes that route's own rule, by appending it
// or removing it by tag. The slot's routing is never rewritten, so the core never resets the slot's
// override, and the slot's balancer is left alone.
func TestAProbeRouteChangeLeavesASlotsOverrideAlone(t *testing.T) {
	for _, c := range []struct {
		name          string
		before, after []string
		want          string
	}{
		{"an added route", []string{"a"}, []string{"a", "b"}, "api adrules -append"},
		{"a removed route", []string{"a", "b"}, []string{"a"}, "api rmrules"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before, after := probeRulePlan(t, c.before...), probeRulePlan(t, c.after...)
			if routingKey(before) != routingKey(after) {
				t.Fatal("a probe route change made every slot owe a rewrite of its routing")
			}
			core := newProbeRuleCore(t)
			if _, err := passOn(core.x).hotApply(context.Background(), after, readbackOf(before), false); err != nil {
				t.Fatal(err)
			}
			var rules []string
			for _, cmd := range core.commands(t) {
				if strings.Contains(cmd, " adrules ") || strings.Contains(cmd, " rmrules ") {
					rules = append(rules, cmd)
				}
			}
			if len(rules) != 1 || !strings.HasPrefix(rules[0], c.want) || !strings.Contains(rules[0], probeEmail(testRef("b"))) || strings.Contains(rules[0], probeEmail(testRef("a"))) {
				t.Fatalf("rule commands %q, want one %q for b's rule alone", rules, c.want)
			}
			if core.called(routingService + "OverrideBalancerTarget") {
				t.Error("the slot's override was set again, as after a rewrite of its routing")
			}
		})
	}
}
