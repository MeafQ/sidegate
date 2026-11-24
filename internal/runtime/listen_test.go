package runtime

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"sidegate/internal/domain"
)

// fakeSocket is a socket a process holds on 127.0.0.1.
type fakeSocket struct {
	Network string
	Port    uint16
	Inode   uint32
}

// coreState is what the fake core holds. The test's API answers from it and the scripted
// `xray api adi`, a run of this test binary, adds to it, so it lives in a file.
type coreState struct {
	PID       int
	Listed    []string
	Own       map[string]fakeSocket // The core's sockets by inbound tag; apiTag is its API listener.
	Foreign   []fakeSocket          // Another process's sockets, which a bind of the core's fails on.
	Inode     uint32                // The last inode given out.
	Removed   []string              // The tags the API removed, in order.
	Requested []string              // The tags an add was asked for, in order, whether or not it registered them.
	Added     []string              // The tags an add registered, in order.
	Adds      int                   // The `xray api adi` runs.
	Next      *coreState            // A core that replaces this one as the next add starts.
	// OnFail is how many inbound lists and unit reads fail once a bind has failed.
	OnFail   struct{ Lists, Shows int }
	Failing  int  // Inbound lists still to fail.
	Narrowed bool // Its routing matches only the sampled ports, until a routing replace.
}

const (
	apiTag      = "api"
	fakeXrayEnv = "SIDEGATE_FAKE_XRAY"
)

func (st *coreState) bind(tag, network string, port uint16) {
	st.Inode++
	st.Own[tag] = fakeSocket{network, port, st.Inode}
}

func loadState(dir string) (coreState, error) {
	var st coreState
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	return st, err
}

// saveState writes the state and the unit's systemctl show for its PID, one invocation per PID. A
// show fails while the shows file counts failures still to come.
func saveState(dir string, st coreState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		return err
	}
	shows := filepath.Join(dir, "shows")
	show := fmt.Sprintf("#!/bin/sh\nn=$(cat %[1]s 2>/dev/null)\nif [ \"${n:-0}\" -gt 0 ]; then echo $((n - 1)) >%[1]s; exit 1; fi\n"+
		"printf 'ActiveState=active\\nMainPID=%[2]d\\nInvocationID=e%[2]d\\n'\n", shows, st.PID)
	return os.WriteFile(filepath.Join(dir, "bin", "systemctl"), []byte(show), 0o755)
}

// fakeXrayAPI is `xray api` as Xray 26.10.31 runs it. Its adi adds a payload's inbounds in order and
// exits at the first that fails. The manager registers a tag and then binds, and a bind that fails
// returns its error with the tag still listed. An add of a listed tag fails and changes nothing. An
// adrules without -append replaces the routing.
func fakeXrayAPI(dir string, args []string, stdin io.Reader) int {
	body, err := io.ReadAll(stdin)
	if err != nil || len(args) < 2 || args[0] != "api" {
		return 0
	}
	if args[1] == "adrules" && !slices.Contains(args, "-append") {
		st, err := loadState(dir)
		if err == nil {
			st.Narrowed = false
			err = saveState(dir, st)
		}
		if err != nil {
			panic(err)
		}
		return 0
	}
	if args[1] != "adi" {
		return 0
	}
	var payload struct {
		Inbounds []struct {
			Tag      string
			Port     uint16
			Settings struct{ Network string }
		}
	}
	st, err := loadState(dir)
	if err == nil {
		err = json.Unmarshal(body, &payload)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if st.Next != nil {
		next := *st.Next
		next.Removed, next.Requested, next.Added, next.Adds = st.Removed, st.Requested, st.Added, st.Adds
		st = next
	}
	st.Adds++
	defer func() {
		if err := saveState(dir, st); err != nil {
			panic(err)
		}
	}()
	for _, in := range payload.Inbounds {
		network := cmp.Or(in.Settings.Network, "tcp")
		st.Requested = append(st.Requested, in.Tag)
		if slices.Contains(st.Listed, in.Tag) {
			fmt.Fprintln(os.Stderr, "failed to add inbound: existing tag found: "+in.Tag)
			return 1
		}
		st.Listed = append(st.Listed, in.Tag)
		st.Added = append(st.Added, in.Tag)
		if slices.ContainsFunc(st.Foreign, func(s fakeSocket) bool { return s.Network == network && s.Port == in.Port }) {
			fmt.Fprintf(os.Stderr, "failed to add inbound: listen %s 127.0.0.1:%d: bind: address already in use\n", network, in.Port)
			st.Failing = st.OnFail.Lists
			if err := os.WriteFile(filepath.Join(dir, "shows"), []byte(fmt.Sprint(st.OnFail.Shows)), 0o644); err != nil {
				panic(err)
			}
			return 1
		}
		st.bind(in.Tag, network, in.Port)
	}
	return 0
}

// inboundCore is a running core on a slot plan's routing with fakeXrayAPI as its `xray api` and
// an API that lists and removes its inbounds.
type inboundCore struct {
	t          *testing.T
	dir        string
	cp         corePlan
	api        coreAPI
	mu         sync.Mutex
	failRemove map[string]int // Removals of a tag still to fail.
}

// newInboundCore is a core with PID 4242 that lists tags, each with its socket bound.
func newInboundCore(t *testing.T, cp corePlan, tags ...string) *inboundCore {
	t.Helper()
	c := &inboundCore{t: t, dir: t.TempDir(), cp: cp, failRemove: map[string]int{}}
	if err := os.MkdirAll(c.path("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", c.path("bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	c.save(c.started(coreState{}, 4242, tags...))
	served := outboundTag(testRef("srv"))
	c.api = fakeCore(t, func(method string, req []byte) ([]byte, string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		st := c.load()
		var resp []byte
		switch strings.TrimPrefix(strings.TrimPrefix(method, handlerService), routingService) {
		case "ListInbounds":
			if st.Failing > 0 {
				st.Failing--
				c.save(st)
				return nil, "14"
			}
			for _, tag := range st.Listed {
				resp = pbBytes(resp, 1, pbString(nil, 1, tag))
			}
		case "ListOutbounds":
			for _, tag := range []string{blockTag, directTag, served} {
				resp = pbBytes(resp, 1, pbString(nil, 1, tag))
			}
		case "ListRule":
			for _, s := range c.cp.Slots {
				resp = pbBytes(resp, 1, pbString(nil, 2, balancerTag(s)))
			}
		case "TestRoute":
			for _, s := range c.cp.Slots {
				for _, tag := range inboundTags(s.ID) {
					if !strings.Contains(string(req), tag) {
						continue
					}
					if st.Narrowed && bytes.Contains(req, []byte{6 << 3, 80}) {
						return pbString(nil, 12, blockTag), "0"
					}
					return pbString(nil, 12, served), "0"
				}
			}
			return nil, "2"
		case "GetBalancerInfo":
			resp = pbBytes(nil, 1, pbBytes(nil, 5, pbString(nil, 2, served)))
		case "RemoveInbound":
			var tag string
			pbFields(req, func(num int, v []byte) error {
				tag = string(v)
				return nil
			})
			if c.failRemove[tag] > 0 {
				c.failRemove[tag]--
				return nil, "14"
			}
			if !slices.Contains(st.Listed, tag) {
				return nil, "2"
			}
			st.Listed = slices.DeleteFunc(st.Listed, func(t string) bool { return t == tag })
			delete(st.Own, tag)
			st.Removed = append(st.Removed, tag)
			c.save(st)
		}
		return resp, "0"
	})
	return c
}

func (c *inboundCore) path(name string) string { return filepath.Join(c.dir, name) }

func (c *inboundCore) load() coreState {
	st, err := loadState(c.dir)
	if err != nil {
		c.t.Fatal(err)
	}
	return st
}

func (c *inboundCore) save(st coreState) {
	if err := saveState(c.dir, st); err != nil {
		c.t.Fatal(err)
	}
}

// state is the core as it stands.
func (c *inboundCore) state() coreState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.load()
}

func (c *inboundCore) change(f func(*coreState)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.load()
	f(&st)
	c.save(st)
}

// started is a new core process in old's place, one started from a config of tags, each bound
// unless another process holds its port. Other processes' sockets and the records of removals and
// adds stay.
func (c *inboundCore) started(old coreState, pid int, tags ...string) coreState {
	st := old
	st.PID, st.Listed, st.Own, st.Next = pid, slices.Clone(tags), map[string]fakeSocket{}, nil
	st.bind(apiTag, "tcp", apiAddr.Port())
	for _, tag := range tags {
		network, port := "tcp", probeAddr.Port()
		if id, ok := inboundSlot(tag); ok {
			_, network, _ = strings.Cut(strings.TrimPrefix(tag, "s."), ".")
			port = c.cp.Slots[slices.IndexFunc(c.cp.Slots, func(s coreSlot) bool { return s.ID == id })].Port
		}
		if !slices.ContainsFunc(st.Foreign, func(s fakeSocket) bool { return s.Network == network && s.Port == port }) {
			st.bind(tag, network, port)
		}
	}
	return st
}

// replace puts a core started from a config of tags in place now.
func (c *inboundCore) replace(pid int, tags ...string) {
	c.change(func(st *coreState) { *st = c.started(*st, pid, tags...) })
}

// replaceOnAdd puts a core started from a config of tags in place as the next add starts, after
// the Worker's last read and before the add reaches the API.
func (c *inboundCore) replaceOnAdd(pid int, tags ...string) {
	c.change(func(st *coreState) {
		next := c.started(*st, pid, tags...)
		st.Next = &next
	})
}

// hold gives another process the port for network, as a bind without SO_REUSEPORT does.
func (c *inboundCore) hold(network string, port uint16) {
	c.change(func(st *coreState) { st.Foreign = append(st.Foreign, fakeSocket{network, port, 9000 + uint32(port)}) })
}

func (c *inboundCore) free() { c.change(func(st *coreState) { st.Foreign = nil }) }

// daemon is a fresh daemon driving this core.
func (c *inboundCore) daemon() *Worker {
	c.t.Helper()
	w, err := Open(Options{DataDir: c.t.TempDir(), RunDir: c.t.TempDir()})
	if err != nil {
		c.t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		c.t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\n%s=%s exec %s \"$@\"\n", fakeXrayEnv, c.dir, exe)
	if err := os.MkdirAll(filepath.Dir(w.core.binary), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(w.core.binary, []byte(script), 0o755); err != nil {
		c.t.Fatal(err)
	}
	w.core.api, w.core.proc = c.api, c.path("proc")
	return w
}

// pass is one reconcile from a fresh read, as a Worker's pass runs it.
func (c *inboundCore) pass(w *Worker) (coreObservation, error) {
	ctx := context.Background()
	p := passOn(w.core)
	obs, _, err := p.reconcile(ctx, c.cp, p.read(ctx, c.cp), false)
	return obs, err
}

// claimPass is one pass as pass runs it, and what its view can claim of id.
func (c *inboundCore) claimPass(w *Worker, id domain.SlotID) (claim, error) {
	ctx := context.Background()
	p := passOn(w.core)
	_, _, err := p.reconcile(ctx, c.cp, p.read(ctx, c.cp), false)
	return p.view.claim(id), err
}

// isApplied says readback shows the slot applied, what opens a closed gate.
func isApplied(obs coreObservation, id domain.SlotID) bool {
	_, ok := obs.Applied[id]
	return ok
}

// inUse says readback shows the slot applied or served, either of which can keep a gate open.
func inUse(obs coreObservation, id domain.SlotID) bool {
	return isApplied(obs, id) || len(obs.Serving[id]) > 0
}

// twoSlotPlan is probeRulePlan with a second slot on the next port.
func twoSlotPlan(t *testing.T) corePlan {
	cp := probeRulePlan(t)
	cp.Slots = append(cp.Slots, coreSlot{ID: "slot2", Port: 12342, Selection: cp.Slots[0].Selection})
	return cp
}

// A slot whose UDP inbound failed to bind is not applied, on this daemon or a new one, until the port
// is free, and then the next pass binds it. Xray keeps a failed add's tag, and the router answers for
// a tag whether or not a listener is behind it, so the tag alone would show the slot served while its
// DNS timed out. The failed add's own tag is removed, so the core lists only what listens, and the
// slot's TCP listener stays.
func TestASlotWhosePortIsHeldIsNotAppliedUntilItBinds(t *testing.T) {
	tcp, udp := inboundTag("slot1", "tcp"), inboundTag("slot1", "udp")
	for _, c := range []struct {
		name   string
		listed []string
		fail   func(core *inboundCore, w *Worker) (coreObservation, error)
	}{
		{"a relisten", append(inboundTags("slot1"), probeTag), func(core *inboundCore, w *Worker) (coreObservation, error) {
			p := passOn(w.core)
			before, err := p.start(context.Background())
			if err := p.relisten(context.Background(), core.cp.Slots[0], known(before, err)); err == nil {
				return coreObservation{}, nil
			}
			return core.pass(w)
		}},
		{"a hot add whose TCP inbound bound", []string{probeTag}, func(core *inboundCore, w *Worker) (coreObservation, error) {
			return core.pass(w)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			core := newInboundCore(t, probeRulePlan(t), c.listed...)
			core.hold("udp", 12341)
			w := core.daemon()
			obs, err := c.fail(core, w)
			if err == nil || inUse(obs, "slot1") {
				t.Fatalf("after the failed add: applied %v serving %v, error %v", obs.Applied, obs.Serving, err)
			}
			st := core.state()
			if !slices.Contains(st.Added, udp) || slices.Contains(st.Listed, udp) || st.Own[tcp].Inode == 0 {
				t.Fatalf("the failed add's tag stayed listed: added %v, listed %v, sockets %v", st.Added, st.Listed, st.Own)
			}
			listener := st.Own[tcp]
			for range 3 {
				if obs, err := core.pass(w); err == nil || inUse(obs, "slot1") || slices.Contains(core.state().Listed, udp) {
					t.Fatalf("a pass on the held port: applied %v serving %v, listed %v, error %v", obs.Applied, obs.Serving, core.state().Listed, err)
				}
			}
			restarted := core.daemon()
			if obs, err := core.pass(restarted); err == nil || inUse(obs, "slot1") || slices.Contains(core.state().Listed, udp) {
				t.Fatalf("a restarted daemon: applied %v serving %v, listed %v, error %v", obs.Applied, obs.Serving, core.state().Listed, err)
			}

			core.free()
			obs, err = core.pass(restarted)
			if err != nil {
				t.Fatal(err)
			}
			st = core.state()
			if _, bound := st.Own[udp]; !isApplied(obs, "slot1") || !bound {
				t.Fatalf("once the port was free: applied %v, sockets %v", obs.Applied, st.Own)
			}
			if st.Own[tcp] != listener || slices.ContainsFunc(st.Removed, func(t string) bool { return t != udp }) || slices.Contains(st.Added[1:], tcp) {
				t.Fatalf("the slot's bound TCP inbound was touched: %v then %v, removed %v, added %v", listener, st.Own[tcp], st.Removed, st.Added)
			}
		})
	}
}

// Two slots added at once, one of them on a held UDP port, end with the other applied from the first
// pass, and no later pass asks for, adds or removes any of its inbounds, whichever of the two comes
// first, through the pass that binds the held one once its port is free.
func TestOneSlotsHeldPortLeavesTheOtherApplied(t *testing.T) {
	for _, held := range []coreSlot{{ID: "slot1", Port: 12341}, {ID: "slot2", Port: 12342}} {
		t.Run(string(held.ID), func(t *testing.T) {
			cp := twoSlotPlan(t)
			healthy := cp.Slots[0].ID
			if healthy == held.ID {
				healthy = cp.Slots[1].ID
			}
			core := newInboundCore(t, cp, probeTag)
			core.hold("udp", held.Port)
			w := core.daemon()
			obs, err := core.pass(w)
			if err == nil || !isApplied(obs, healthy) || inUse(obs, held.ID) {
				t.Fatalf("after the add: applied %v serving %v, error %v", obs.Applied, obs.Serving, err)
			}
			first := core.state()
			untouched := func(when string) {
				t.Helper()
				st := core.state()
				touched := func(tag string) bool { return slices.Contains(inboundTags(healthy), tag) }
				if slices.ContainsFunc(st.Removed, touched) || slices.ContainsFunc(st.Requested[len(first.Requested):], touched) {
					t.Fatalf("%s: %s's inbounds were touched after the first pass: removed %v, requested %v then %v",
						when, healthy, st.Removed, first.Requested, st.Requested)
				}
				for _, tag := range inboundTags(healthy) {
					if st.Own[tag] != first.Own[tag] {
						t.Fatalf("%s: %s's %s socket changed: %v then %v", when, healthy, tag, first.Own[tag], st.Own[tag])
					}
				}
			}
			for range 3 {
				obs, _ := core.pass(w)
				if !isApplied(obs, healthy) || inUse(obs, held.ID) {
					t.Fatalf("a later pass: applied %v serving %v", obs.Applied, obs.Serving)
				}
			}
			untouched("on the held port")
			core.free()
			obs, err = core.pass(w)
			if err != nil || !isApplied(obs, healthy) || !isApplied(obs, held.ID) {
				t.Fatalf("once the port was free: applied %v, error %v", obs.Applied, err)
			}
			if _, bound := core.state().Own[inboundTag(held.ID, "udp")]; !bound {
				t.Fatalf("the freed port wasn't bound: %v", core.state().Own)
			}
			untouched("through recovery")
		})
	}
}

// A core started since the Worker's read already holds the slot's inbounds from its config, bound,
// since a core whose bind fails at start doesn't run. The add finds them listed, and nothing of the
// new core's is removed, whether it started before the Worker's adds or as one began.
func TestACoreStartedSinceTheReadKeepsItsInbounds(t *testing.T) {
	for name, replace := range map[string]func(core *inboundCore, tags ...string){
		"before the adds":  func(core *inboundCore, tags ...string) { core.replace(4243, tags...) },
		"as the add began": func(core *inboundCore, tags ...string) { core.replaceOnAdd(4243, tags...) },
	} {
		t.Run(name, func(t *testing.T) {
			core := newInboundCore(t, probeRulePlan(t), inboundTag("slot1", "tcp"), probeTag)
			w := core.daemon()
			obs := passOn(w.core).read(context.Background(), core.cp)
			if obs.readErr != nil || obs.PID != 4242 {
				t.Fatalf("read pid %d, error %v", obs.PID, obs.readErr)
			}
			replace(core, append(inboundTags("slot1"), probeTag)...)
			core.change(func(st *coreState) {
				if st.Next != nil {
					st.Next.Removed, st.Next.Added = nil, nil
				}
				st.Removed, st.Added = nil, nil
			})
			_, _, err := passOn(w.core).reconcile(context.Background(), core.cp, obs, false)
			st := core.state()
			if st.PID != 4243 || len(st.Removed) > 0 || len(st.Own) != 4 {
				t.Fatalf("the new core lost an inbound: pid %d, removed %v, sockets %v, error %v", st.PID, st.Removed, st.Own, err)
			}
			if obs, err := core.pass(w); err != nil || !isApplied(obs, "slot1") {
				t.Fatalf("the new core reads back applied %v, error %v", obs.Applied, err)
			}
		})
	}
}

// A core can start between one slot's adds and the next. Each add reads which start it meets, so the
// held port's add that fails on the new core removes its own tag there, and the slot binds once the
// port is free.
func TestACoreStartedBetweenTwoAddsKeepsNoTagOfTheFailedOne(t *testing.T) {
	cp := twoSlotPlan(t)
	udp := inboundTag("slot2", "udp")
	core := newInboundCore(t, cp, append(inboundTags("slot1"), probeTag)...)
	core.hold("udp", 12342)
	w := core.daemon()
	obs := passOn(w.core).read(context.Background(), cp)
	core.replaceOnAdd(4243, append(inboundTags("slot1"), probeTag)...)
	obs, _, err := passOn(w.core).reconcile(context.Background(), cp, obs, false)
	st := core.state()
	if err == nil || st.PID != 4243 || inUse(obs, "slot2") || slices.Contains(st.Listed, udp) || !slices.Equal(st.Removed, []string{udp}) {
		t.Fatalf("on the new core: pid %d, applied %v, listed %v, removed %v, error %v", st.PID, obs.Applied, st.Listed, st.Removed, err)
	}
	core.free()
	obs, err = core.pass(w)
	if _, bound := core.state().Own[udp]; err != nil || !isApplied(obs, "slot1") || !isApplied(obs, "slot2") || !bound {
		t.Fatalf("once the port was free: applied %v, sockets %v, error %v", obs.Applied, core.state().Own, err)
	}
}

// A core that starts as a slot's add begins holds that slot bound from its config, and the add fails
// on a listed tag. The pass reads that slot as the new core holds it, applied, while the other slot's
// held port keeps only that one unapplied.
func TestADuplicateAddOnABoundSlotLeavesItApplied(t *testing.T) {
	cp := twoSlotPlan(t)
	core := newInboundCore(t, cp, probeTag)
	core.hold("udp", 12342)
	w := core.daemon()
	obs := passOn(w.core).read(context.Background(), cp)
	core.replaceOnAdd(4243, append(inboundTags("slot1"), probeTag)...)
	obs, _, err := passOn(w.core).reconcile(context.Background(), cp, obs, false)
	st := core.state()
	if err == nil || st.PID != 4243 || !isApplied(obs, "slot1") || inUse(obs, "slot2") {
		t.Fatalf("pid %d, applied %v serving %v, error %v", st.PID, obs.Applied, obs.Serving, err)
	}
	if !slices.Equal(st.Removed, []string{inboundTag("slot2", "udp")}) {
		t.Fatalf("removed %v, want only slot2's failed UDP add", st.Removed)
	}
}

// A read that fails just after a failed add is tried once more in the same pass, so a stray failure
// still removes the tag. One that fails again leaves the tag. Either way the slot can't be claimed.
func TestAReadThatFailsAfterAFailedAddIsTriedAgain(t *testing.T) {
	udp := inboundTag("slot1", "udp")
	for _, c := range []struct {
		name         string
		lists, shows int
		removed      bool
	}{
		{"the list fails once", 1, 0, true},
		{"the unit read fails once", 0, 1, true},
		{"the list fails twice", 2, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			core := newInboundCore(t, probeRulePlan(t), inboundTag("slot1", "tcp"), probeTag)
			core.hold("udp", 12341)
			core.change(func(st *coreState) { st.OnFail.Lists, st.OnFail.Shows = c.lists, c.shows })
			w := core.daemon()
			claimed, err := core.claimPass(w, "slot1")
			st := core.state()
			if err == nil || claimed != claimRefuted || slices.Contains(st.Removed, udp) != c.removed || slices.Contains(st.Listed, udp) == c.removed {
				t.Fatalf("claim %v, listed %v, removed %v, error %v", claimed, st.Listed, st.Removed, err)
			}
		})
	}
}

// An add that reaches a core started since the Worker's read, and fails to bind there, leaves no tag
// on the new core.
func TestAFailedAddOnANewCoreLeavesNoTag(t *testing.T) {
	udp := inboundTag("slot1", "udp")
	core := newInboundCore(t, probeRulePlan(t), inboundTag("slot1", "tcp"), probeTag)
	w := core.daemon()
	obs := passOn(w.core).read(context.Background(), core.cp)
	core.hold("udp", 12341)
	core.replace(4243, inboundTag("slot1", "tcp"), probeTag)
	obs, _, err := passOn(w.core).reconcile(context.Background(), core.cp, obs, false)
	st := core.state()
	if err == nil || inUse(obs, "slot1") || !slices.Contains(st.Added, udp) || slices.Contains(st.Listed, udp) {
		t.Fatalf("on the new core: applied %v serving %v, added %v, listed %v, error %v", obs.Applied, obs.Serving, st.Added, st.Listed, err)
	}
	if !slices.Equal(st.Removed, []string{udp}) {
		t.Fatalf("removed %v, want only the failed add's %s", st.Removed, udp)
	}
	core.free()
	if obs, err := core.pass(w); err != nil || !isApplied(obs, "slot1") || obs.PID != 4243 {
		t.Fatalf("once the port was free: applied %v, pid %d, error %v", obs.Applied, obs.PID, err)
	}
}

// A failed add whose tag can't be removed, the API gone as a core goes down, is the pass's error, and
// the slot can't be claimed on that pass though its tag is listed. The core that comes up in its place
// starts from its config, so the next pass adds what that lacks, removes what then fails, and
// removes nothing else.
func TestAFailedAddWhoseRemovalFailsIsRepairedOnTheNextPass(t *testing.T) {
	tcp, udp := inboundTag("slot1", "tcp"), inboundTag("slot1", "udp")
	core := newInboundCore(t, probeRulePlan(t), probeTag)
	core.hold("udp", 12341)
	core.failRemove[udp] = 1
	w := core.daemon()
	claimed, err := core.claimPass(w, "slot1")
	if st := core.state(); err == nil || claimed != claimRefuted || !slices.Contains(st.Listed, udp) || len(st.Removed) > 0 {
		t.Fatalf("the pass whose removal failed: claim %v, listed %v, removed %v, error %v", claimed, st.Listed, st.Removed, err)
	}
	var obs coreObservation
	core.replace(4243, probeTag)
	obs, err = core.pass(w)
	st := core.state()
	if err == nil || inUse(obs, "slot1") || slices.Contains(st.Listed, udp) || !slices.Equal(st.Removed, []string{udp}) {
		t.Fatalf("the next pass: applied %v serving %v, listed %v, removed %v, error %v", obs.Applied, obs.Serving, st.Listed, st.Removed, err)
	}
	listener := st.Own[tcp]
	core.free()
	if obs, err := core.pass(w); err != nil || !isApplied(obs, "slot1") || core.state().Own[tcp] != listener {
		t.Fatalf("once the port was free: applied %v, error %v, tcp %v then %v", obs.Applied, err, listener, core.state().Own[tcp])
	}
}

// A healthy slot is left alone, pass after pass.
func TestAHealthySlotIsLeftAlone(t *testing.T) {
	core := newInboundCore(t, probeRulePlan(t), append(inboundTags("slot1"), probeTag)...)
	w := core.daemon()
	before := core.state()
	for range 20 {
		if obs, err := core.pass(w); err != nil || !isApplied(obs, "slot1") {
			t.Fatalf("applied %v, error %v", obs.Applied, err)
		}
	}
	if st := core.state(); len(st.Removed) > 0 || st.Adds > 0 || !maps.Equal(st.Own, before.Own) {
		t.Fatalf("the healthy slot was touched: removed %v, %d adds", st.Removed, st.Adds)
	}
}

// A slot is not applied while its core still holds the inbound an older build gave both networks.
// That inbound shares the port through SO_REUSEPORT, and a connection it accepts carries a tag no rule
// matches, so it goes to block.
func TestTheOldCombinedInboundHoldsItsSlotUnappliedUntilItsRemoved(t *testing.T) {
	core := newInboundCore(t, probeRulePlan(t), append(inboundTags("slot1"), "s.slot1", probeTag)...)
	core.failRemove["s.slot1"] = 1
	w := core.daemon()

	obs, err := core.pass(w)
	if err == nil {
		t.Fatal("the failed removal went unreported")
	}
	if inUse(obs, "slot1") {
		t.Fatalf("the slot reads back applied %v serving %v beside the old inbound", obs.Applied, obs.Serving)
	}
	obs, err = core.pass(w)
	if err != nil {
		t.Fatal(err)
	}
	if !isApplied(obs, "slot1") || slices.Contains(core.state().Listed, "s.slot1") {
		t.Fatalf("after the removal: applied %v, listed %v", obs.Applied, core.state().Listed)
	}
}
