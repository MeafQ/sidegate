package runtime

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// coreRig runs whole passes of a Worker for slot1 and slot2, both on srv, their kernel plumbing complete
// and their links up. Its core is files a test changes: the unit's start, the inbounds listed, and
// each balancer's override. `xray api adi` lists each tag it adds, and fails a tag named in fail-adi
// as a bind on a held port does, leaving the tag listed. A routing replace resets every override, as
// Xray's does. A test can answer any call itself, and records every flag write.
type coreRig struct {
	t       *testing.T
	w       *Worker
	dir     string
	servers map[domain.ServerRef]domain.Server
	slots   []domain.Slot

	runDir    string
	mu        sync.Mutex
	log       []string // Every call, by method, in order.
	calls     map[string]int
	answer    func(method string, n int, req []byte) (resp []byte, status string, answered bool)
	overrides map[domain.SlotID]string
	writes    []string
	destroyed []string
	closing   func(mark uint32) error // Runs in each TCP close, before it counts.
	stuck     map[string]bool         // Links whose down fails, their flag left as it is.
}

func newCoreRig(t *testing.T) *coreRig {
	t.Helper()
	srv, next := testRef("srv"), testRef("next")
	r := &coreRig{t: t, dir: t.TempDir(), calls: map[string]int{}, servers: map[domain.ServerRef]domain.Server{
		srv: testServer(t, vlessHop(t, "root", "198.51.100.1", "")), next: testServer(t, vlessHop(t, "root", "198.51.100.2", "")),
	}}
	for i, id := range []domain.SlotID{"slot1", "slot2"} {
		o := uint8(i + 1)
		r.slots = append(r.slots, domain.Slot{ID: id, Allocation: domain.Allocation{Ordinal: o, PeerListenPort: 12000 + uint16(o)*2, TPROXYPort: 12001 + uint16(o)*2,
			Tunnel: netip.MustParsePrefix(fmt.Sprintf("10.200.0.%d/30", 4*i)), Keys: domain.SlotKeys{ClientPublic: "client" + string(id)}}})
	}
	r.overrides = map[domain.SlotID]string{"slot1": outboundTag(srv), "slot2": outboundTag(srv)}
	r.write("rules", "b.slot1\nb.slot2\n")
	r.setOutbounds(srv, next)
	r.setInbounds(slices.Concat(inboundTags("slot1"), inboundTags("slot2"), []string{probeTag})...)
	r.setStart("e1")
	api := fakeCore(t, r.call)

	k := filepath.Join(r.dir, "kernel")
	targets := []SlotTarget{workerTarget(r.slots[0], domain.Serving(srv), Passage{}), workerTarget(r.slots[1], domain.Serving(srv), Passage{})}
	spec := (&kernel{}).render(Plan{slots: targets}.kernel(func(SlotTarget) bool { return false }, true))
	var dumps strings.Builder
	for _, s := range r.slots {
		a := s.Allocation
		name := linkName(a)
		r.write("kernel/"+name, fmt.Sprintf("host\tpublic\t%d\toff\n%s\t(none)\t(none)\t%s\t0\t0\t0\t%s\n", a.PeerListenPort, a.Keys.ClientPublic, a.Tunnel.Masked(), peerKeepalive))
		fmt.Fprintf(&dumps, "%s) cat %s/%s ;;\n", name, k, name)
		r.write("sys/"+name+"/netdev_group", fmt.Sprintf("%d\n", linkGroup))
		r.write("sys/"+name+"/flags", "0x1003\n")
		r.write("proc/"+name+"/accept_local", "1\n")
		r.write("proc/"+name+"/rp_filter", "0\n")
	}
	r.write("proc/all/rp_filter", "0\n")
	r.write("kernel/iptables", saveOf(spec, "iptables"))
	r.write("kernel/ip6tables", saveOf(spec, "ip6tables"))
	r.write("kernel/rules", strings.Join(spec.rules, "\n")+"\n")
	r.write("kernel/routes", "2 default dev lo proto 85 scope 254\n")
	r.write("bin/wg", "#!/bin/sh\ncase \"$2\" in\n"+dumps.String()+"esac\n")
	r.write("bin/iptables-save", "#!/bin/sh\ncat "+k+"/iptables\n")
	r.write("bin/ip6tables-save", "#!/bin/sh\ncat "+k+"/ip6tables\n")
	r.write("bin/ip", "#!/bin/sh\ncase \"$*\" in\n'-N rule show') cat "+k+"/rules ;;\n'-N route show table 100') cat "+k+"/routes ;;\n*) echo \"unexpected: ip $*\" >&2; exit 1 ;;\nesac\n")
	fails := r.path("unit-fails")
	r.write("bin/systemctl", "#!/bin/sh\n[ \"$1\" = show ] || exit 0\nn=$(cat "+fails+" 2>/dev/null)\n"+
		"if [ \"${n:-0}\" -gt 0 ]; then echo $((n - 1)) >"+fails+"; echo unit unread >&2; exit 1; fi\ncat "+r.path("unit")+"\n")
	t.Setenv("PATH", r.path("bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	savedSys, savedProc := sysNet, procConf
	sysNet, procConf = r.path("sys"), r.path("proc")
	t.Cleanup(func() { sysNet, procConf = savedSys, savedProc })

	data := t.TempDir()
	inbounds, failing, replaced, rules := r.path("inbounds"), r.path("fail-adi"), r.path("routing-replaced"), r.path("rules")
	r.writeAt(filepath.Join(data, "current", "xray"), "#!/bin/sh\n"+
		"[ \"$1\" = version ] && { echo 'Xray 25.10.15'; exit 0; }\n"+
		"[ \"$2\" = adrules ] && [ \"$3\" != -append ] && { grep -o '\"ruleTag\":\"b\\.[^\"]*\"' | cut -d'\"' -f4 >"+rules+"; touch "+replaced+"; exit 0; }\n"+
		"[ \"$2\" = adi ] || { cat >/dev/null; exit 0; }\n"+
		"tag=$(grep -o '\"tag\":\"[^\"]*\"' | cut -d'\"' -f4)\n"+
		"grep -qx \"$tag\" "+inbounds+" && { echo \"failed to add inbound: existing tag found: $tag\" >&2; exit 1; }\n"+
		"echo \"$tag\" >>"+inbounds+"\n"+
		"grep -qx \"$tag\" "+failing+" 2>/dev/null && { echo \"failed to add inbound: listen udp: bind: address already in use\" >&2; exit 1; }\n"+
		"exit 0\n")
	r.runDir = t.TempDir()
	w, err := Open(Options{DataDir: data, RunDir: r.runDir, CloseFlows: func(pid int, mark uint32) (int, error) {
		r.mu.Lock()
		closing := r.closing
		r.mu.Unlock()
		if closing != nil {
			if err := closing(mark); err != nil {
				return 0, err
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.destroyed = append(r.destroyed, fmt.Sprintf("%d %d", pid, mark))
		return 1, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	w.core.api = api
	t.Cleanup(w.core.reads.Wait)
	w.gates.write = func(link string, up bool) error {
		r.mu.Lock()
		r.writes = append(r.writes, fmt.Sprintf("%s %s", link, map[bool]string{true: "up", false: "down"}[up]))
		stuck := r.stuck[link] && !up
		r.mu.Unlock()
		if stuck {
			return fmt.Errorf("link %s: set down: operation not permitted", link)
		}
		r.write("sys/"+link+"/flags", map[bool]string{true: "0x1003\n", false: "0x1002\n"}[up])
		return nil
	}
	r.w = w
	return r
}

// call answers the core API as Xray does from the rig's files, once the test's answer passes.
func (r *coreRig) call(method string, req []byte) ([]byte, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := strings.TrimPrefix(strings.TrimPrefix(method, handlerService), routingService)
	r.calls[name]++
	r.log = append(r.log, name)
	if r.answer != nil {
		answer := r.answer
		r.mu.Unlock()
		resp, status, answered := answer(name, r.calls[name], req)
		r.mu.Lock()
		if answered {
			return resp, status
		}
	}
	if r.exists("routing-replaced") {
		os.Remove(r.path("routing-replaced"))
		clear(r.overrides)
	}
	field := func(want int) (v string) {
		pbFields(req, func(num int, b []byte) error {
			if num == want {
				v = string(b)
			}
			return nil
		})
		return v
	}
	var resp []byte
	switch name {
	case "ListInbounds":
		for _, tag := range r.inbounds() {
			resp = pbBytes(resp, 1, pbString(nil, 1, tag))
		}
	case "ListOutbounds":
		b, _ := os.ReadFile(r.path("outbounds"))
		for _, tag := range strings.Fields(string(b)) {
			resp = pbBytes(resp, 1, pbString(nil, 1, tag))
		}
	case "ListRule":
		b, _ := os.ReadFile(r.path("rules"))
		for _, tag := range strings.Fields(string(b)) {
			resp = pbBytes(resp, 1, pbString(nil, 2, tag))
		}
		for _, ref := range []domain.ServerRef{testRef("next"), testRef("srv")} {
			resp = pbBytes(resp, 1, pbString(pbString(nil, 1, outboundTag(ref)), 2, probeEmail(ref)))
		}
	case "GetInboundUsers":
		for _, ref := range []domain.ServerRef{testRef("next"), testRef("srv")} {
			resp = pbBytes(resp, 1, pbString(nil, 2, probeEmail(ref)))
		}
	case "GetBalancerInfo":
		id := domain.SlotID(strings.TrimPrefix(field(1), "b."))
		resp = pbBytes(nil, 1, pbBytes(nil, 5, pbString(nil, 2, r.overrides[id])))
	case "OverrideBalancerTarget":
		var balancer, target string
		pbFields(req, func(num int, v []byte) error {
			if num == 1 {
				balancer = string(v)
			} else {
				target = string(v)
			}
			return nil
		})
		r.overrides[domain.SlotID(strings.TrimPrefix(balancer, "b."))] = target
	case "RemoveInbound":
		tag := field(1)
		if !slices.Contains(r.inbounds(), tag) {
			return nil, "2"
		}
		r.setInbounds(slices.DeleteFunc(r.inbounds(), func(t string) bool { return t == tag })...)
	case "TestRoute":
		var inbound string
		pbFields(req, func(num int, routing []byte) error {
			return pbScan(routing, func(num int, _ uint64, b []byte) error {
				if num == 1 {
					inbound = string(b)
				}
				return nil
			})
		})
		id, slot := inboundSlot(inbound)
		if !slot || r.overrides[id] == "" {
			return nil, "2"
		}
		return pbString(nil, 12, r.overrides[id]), "0"
	}
	return resp, "0"
}

func (r *coreRig) path(name string) string { return filepath.Join(r.dir, name) }

// opening says an opening holds the gate lock, from before its confirmation reads the core until its
// link up has landed.
func (r *coreRig) opening() bool {
	lock, err := lockFile(gateLock(r.runDir), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return true
	}
	lock.Close()
	return false
}

// since is every call from the nth on, by method.
func (r *coreRig) since(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.log[min(n, len(r.log)):])
}

func (r *coreRig) logged() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.log)
}

func (r *coreRig) exists(name string) bool {
	_, err := os.Stat(r.path(name))
	return err == nil
}

func (r *coreRig) write(name, body string) { r.writeAt(r.path(name), body) }

func (r *coreRig) writeAt(path, body string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Error(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		r.t.Error(err)
	}
}

func (r *coreRig) inbounds() []string {
	b, _ := os.ReadFile(r.path("inbounds"))
	return strings.Fields(string(b))
}

func (r *coreRig) setInbounds(tags ...string) { r.write("inbounds", strings.Join(tags, "\n")+"\n") }

// setStart names the core's running start, and its main process, which follows from it.
func (r *coreRig) setStart(epoch domain.CoreEpoch) {
	r.write("unit", fmt.Sprintf("ActiveState=active\nMainPID=%d\nInvocationID=%s\n", r.pid(epoch), epoch))
}

// setOutbounds lists block, direct and each server's outbound as the core's.
func (r *coreRig) setOutbounds(refs ...domain.ServerRef) {
	tags := []string{blockTag, directTag}
	for _, ref := range refs {
		tags = append(tags, outboundTag(ref))
	}
	r.write("outbounds", strings.Join(tags, "\n")+"\n")
}

// failUnitReads fails the next n reads of the core's unit.
func (r *coreRig) failUnitReads(n int) { r.write("unit-fails", fmt.Sprint(n)) }

// restart puts a core started from its config in place as epoch: every inbound bound, each override set
// to srv, the rules whole, and the stop hook's closure of the gates.
func (r *coreRig) restart(epoch domain.CoreEpoch) {
	r.setStart(epoch)
	r.setInbounds(slices.Concat(inboundTags("slot1"), inboundTags("slot2"), []string{probeTag})...)
	r.write("rules", "b.slot1\nb.slot2\n")
	r.mu.Lock()
	r.overrides = map[domain.SlotID]string{"slot1": outboundTag(testRef("srv")), "slot2": outboundTag(testRef("srv"))}
	r.mu.Unlock()
	for _, s := range r.slots {
		r.write("sys/"+linkName(s.Allocation)+"/flags", "0x1002\n")
	}
}

// pid is the main process of the core started as epoch.
func (r *coreRig) pid(epoch domain.CoreEpoch) int { return 4000 + int(epoch[len(epoch)-1]) }

// failAdd makes each add of tag fail as a bind on a held port does, its tag left listed.
func (r *coreRig) failAdd(tags ...string) { r.write("fail-adi", strings.Join(tags, "\n")+"\n") }

// answering sets the test's answer, which comes before the core's files for every call.
func (r *coreRig) answering(answer func(method string, n int, req []byte) ([]byte, string, bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer = answer
}

// failing fails the nth call of method from now with Unavailable, and answers the rest from the files.
func (r *coreRig) failing(method string, n int) {
	r.mu.Lock()
	from := r.calls[method]
	r.mu.Unlock()
	r.answering(func(m string, at int, _ []byte) ([]byte, string, bool) { return nil, "14", m == method && at == from+n })
}

func (r *coreRig) plan(generation uint64, one, two Passage, sel ...domain.Selection) *Plan {
	r.t.Helper()
	sels := []domain.Selection{domain.Serving(testRef("srv")), domain.Serving(testRef("srv"))}
	copy(sels, sel)
	p, err := NewPlan(generation, []SlotTarget{workerTarget(r.slots[0], sels[0], one), workerTarget(r.slots[1], sels[1], two)}, r.servers, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return &p
}

// pass hands intake p, as a plan arriving does, and runs one pass on it.
func (r *coreRig) pass(p *Plan) (Report, error) {
	r.w.gates.take(p)
	return r.w.pass(context.Background(), p)
}

// healthy runs the adopting pass on p, which must change nothing.
func (r *coreRig) healthy(p *Plan) {
	r.t.Helper()
	report, err := r.pass(p)
	if err != nil || len(report.Steps) > 0 || r.slot(report, "slot1").Phase != Ready || r.slot(report, "slot2").Phase != Ready {
		r.t.Fatalf("the adopted healthy slots: %+v, steps %v, error %v", report.Slots, report.Steps, err)
	}
	r.mu.Lock()
	r.writes = nil
	r.mu.Unlock()
}

func (r *coreRig) slot(report Report, id domain.SlotID) SlotReport {
	for _, s := range report.Slots {
		if s.Slot == id {
			return s
		}
	}
	r.t.Fatalf("no report for %s", id)
	return SlotReport{}
}

// flagWrites is every flag write since healthy.
func (r *coreRig) flagWrites() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.writes)
}

func (r *coreRig) up(link string) bool {
	up, err := linkIsUp(link)
	return err == nil && up
}

// until is a passage whose deadline is d from now.
func until(d time.Duration) Passage { return MoveFrom(testRef("next"), time.Now().Add(d)) }

// unvouched is a report of a slot the pass can't vouch for while its link is up.
func unvouched(s SlotReport) bool { return s.Phase == Unknown && s.Applied == (domain.Selection{}) }

func TestTheCoreRigAdoptsTwoHealthySlots(t *testing.T) {
	r := newCoreRig(t)
	r.healthy(r.plan(1, Passage{}, Passage{}))
	r.healthy(r.plan(1, Passage{}, Passage{}))
}

var onSrv = domain.Serving(testRef("srv"))

// W40. An opening's confirmation that lists slot1's inbounds without its UDP tag, then fails, has read
// against slot1's claim, so slot1 reports nothing it can't prove, whether the read after the closes
// finds the tag still gone or fails too. A confirmation whose first list fails says nothing against it.
func TestAPartialReadAgainstASlotClaimsNothing(t *testing.T) {
	udp := inboundTag("slot1", "udp")
	for _, c := range []struct {
		name    string
		answer  func(r *coreRig) func(string, int, []byte) ([]byte, string, bool)
		claimed bool
	}{
		{"the tag gone from the core", func(r *coreRig) func(string, int, []byte) ([]byte, string, bool) {
			confirming := false
			return func(m string, _ int, _ []byte) ([]byte, string, bool) {
				switch {
				case m == "ListInbounds" && !confirming && r.opening():
					confirming = true
					r.setInbounds(slices.DeleteFunc(r.inbounds(), func(t string) bool { return t == udp })...)
				case m == "ListOutbounds" && confirming && r.opening():
					return nil, "14", true
				}
				return nil, "", false
			}
		}, false},
		{"the read after the closes failing too", func(r *coreRig) func(string, int, []byte) ([]byte, string, bool) {
			step := 0
			return func(m string, _ int, _ []byte) ([]byte, string, bool) {
				switch {
				case m == "ListInbounds" && step == 0 && r.opening():
					step = 1
					var resp []byte
					for _, tag := range slices.DeleteFunc(r.inbounds(), func(t string) bool { return t == udp }) {
						resp = pbBytes(resp, 1, pbString(nil, 1, tag))
					}
					return resp, "0", true
				case m == "ListOutbounds" && step == 1:
					step = 2
					return nil, "14", true
				case m == "ListInbounds" && step == 2:
					return nil, "14", true
				}
				return nil, "", false
			}
		}, false},
		{"the confirmation's first list failing", func(r *coreRig) func(string, int, []byte) ([]byte, string, bool) {
			return func(m string, _ int, _ []byte) ([]byte, string, bool) {
				return nil, "14", m == "ListInbounds" && r.opening()
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCoreRig(t)
			p := r.plan(1, until(time.Minute), Passage{})
			r.healthy(p)
			r.write("sys/pxa2/flags", "0x1002\n")
			r.answering(c.answer(r))
			report, _ := r.pass(p)
			one := r.slot(report, "slot1")
			if c.claimed != (one.Phase == Ready && one.Applied == onSrv) || !c.claimed && !unvouched(one) {
				t.Errorf("slot1 reports %v on %v, want claimed %t", one.Phase, one.Applied, c.claimed)
			}
			if !r.up("pxa1") || slices.Contains(r.flagWrites(), "pxa1 down") {
				t.Errorf("pxa1: up %t, writes %v", r.up("pxa1"), r.flagWrites())
			}
		})
	}
}

// W41. A forced close whose UDP removal took effect and answered an error adds nothing, and the read
// after it finds the tag gone: the report claims nothing for slot1. So does a confirmation of slot2
// that reads slot1 unapplied.
func TestAReadAgainstAClaimEndsTheUnreadRide(t *testing.T) {
	udp := inboundTag("slot1", "udp")
	t.Run("a removal that answered an error", func(t *testing.T) {
		r := newCoreRig(t)
		p := r.plan(1, until(time.Minute), Passage{})
		r.healthy(p)
		forced := r.plan(2, until(time.Minute), Passage{})
		forced.slots[0].CloseFlows = "op1"
		r.answering(func(m string, _ int, req []byte) ([]byte, string, bool) {
			if m == "RemoveInbound" && strings.Contains(string(req), udp) {
				r.setInbounds(slices.DeleteFunc(r.inbounds(), func(t string) bool { return t == udp })...)
				return nil, "14", true
			}
			return nil, "", false
		})
		report, _ := r.pass(forced)
		if one := r.slot(report, "slot1"); !unvouched(one) || slices.Contains(r.inbounds(), udp) {
			t.Errorf("slot1 reports %v on %v, listed %v", one.Phase, one.Applied, r.inbounds())
		}
	})
	t.Run("a confirmation of slot2 reading slot1 unapplied", func(t *testing.T) {
		r := newCoreRig(t)
		p := r.plan(1, until(time.Minute), Passage{})
		r.healthy(p)
		r.write("sys/pxa2/flags", "0x1002\n")
		r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
			if m == "ListInbounds" && r.opening() {
				r.setInbounds(slices.DeleteFunc(r.inbounds(), func(t string) bool { return t == udp })...)
			}
			return nil, "", false
		})
		report, _ := r.pass(p)
		if one := r.slot(report, "slot1"); !unvouched(one) {
			t.Errorf("slot1 reports %v on %v", one.Phase, one.Applied)
		}
		if two := r.slot(report, "slot2"); two.Phase != Ready || !r.up("pxa2") {
			t.Errorf("slot2 reports %v, up %t", two.Phase, r.up("pxa2"))
		}
	})
}

// W44. A sibling's failed add is followed by its cleanup's list, which lacks slot1's UDP tag, and the
// read after the closes fails at its first list. slot1 claims nothing, though repair's read showed it,
// and slot2's failed add refutes it.
func TestAnAddsListAgainstASiblingClaimsNothing(t *testing.T) {
	r := newCoreRig(t)
	p := r.plan(1, until(time.Minute), Passage{})
	r.healthy(p)
	forced := r.plan(2, until(time.Minute), Passage{})
	forced.slots[1].CloseFlows = "op2"
	r.failAdd(inboundTag("slot2", "udp"))
	// The relisten's removal, its failed add, the cleanup's list, the cleanup's removal of the failed
	// tag, then the read after the closes.
	step := 0
	r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
		switch {
		case m == "RemoveInbound" && (step == 0 || step == 2):
			step++
		case m == "ListInbounds" && step == 1:
			step = 2
			var resp []byte
			for _, tag := range slices.DeleteFunc(r.inbounds(), func(t string) bool { return t == inboundTag("slot1", "udp") }) {
				resp = pbBytes(resp, 1, pbString(nil, 1, tag))
			}
			return resp, "0", true
		case m == "ListInbounds" && step == 3:
			return nil, "14", true
		}
		return nil, "", false
	})
	report, _ := r.pass(forced)
	if one := r.slot(report, "slot1"); !unvouched(one) {
		t.Errorf("slot1 reports %v on %v", one.Phase, one.Applied)
	}
	if two := r.slot(report, "slot2"); two.Applied != (domain.Selection{}) || two.Phase == Ready {
		t.Errorf("slot2, whose add failed, reports %v on %v", two.Phase, two.Applied)
	}
	if !r.up("pxa1") || slices.Contains(r.flagWrites(), "pxa1 down") {
		t.Errorf("pxa1: up %t, writes %v", r.up("pxa1"), r.flagWrites())
	}
}

// W44, the repair path. A probe rule gone gives repair a step, so it reads slot1's balancer again,
// and NotFound sends no override. Repair's read then fails: slot1 claims nothing, and its gate stays.
func TestABalancerNotFoundInRepairClaimsNothing(t *testing.T) {
	r := newCoreRig(t)
	p := r.plan(1, until(time.Minute), Passage{})
	r.healthy(p)
	start := r.logged()
	lookups, lists, rules := 0, 0, 0
	r.answering(func(m string, _ int, req []byte) ([]byte, string, bool) {
		switch {
		case m == "ListRule":
			rules++
			if rules == 1 {
				resp := pbBytes(pbBytes(nil, 1, pbString(nil, 2, "b.slot1")), 1, pbString(nil, 2, "b.slot2"))
				return pbBytes(resp, 1, pbString(pbString(nil, 1, outboundTag(testRef("srv"))), 2, probeEmail(testRef("srv")))), "0", true
			}
		case m == "GetBalancerInfo" && strings.Contains(string(req), "b.slot1"):
			lookups++
			return nil, "2", lookups == 2
		case m == "ListInbounds":
			lists++
			return nil, "14", lists == 2
		}
		return nil, "", false
	})
	report, _ := r.pass(p)
	if one := r.slot(report, "slot1"); !unvouched(one) {
		t.Errorf("slot1 reports %v on %v", one.Phase, one.Applied)
	}
	if slices.Contains(r.since(start), "OverrideBalancerTarget") || !r.up("pxa1") {
		t.Errorf("calls %v, pxa1 up %t", r.since(start), r.up("pxa1"))
	}
}

// without answers a list of the core's inbounds without tag.
func (r *coreRig) without(tag string) []byte {
	var resp []byte
	for _, t := range slices.DeleteFunc(r.inbounds(), func(t string) bool { return t == tag }) {
		resp = pbBytes(resp, 1, pbString(nil, 1, t))
	}
	return resp
}

// W29. A sibling's rewrite resets slot1's override, restoring it fails, and repair's read fails. slot1
// claims nothing while its link is up, and intake keeps B, as today: with the deadline delivered while
// repair's read is held, no down comes while it is held and the open after it closes pxa1; with the
// deadline after the pass, pxa1 stays up at it and the next pass's failed entry read closes it. A
// sibling's failed add with no rewrite, and a good read after it, leaves slot1 claimed.
func TestARoutingRewriteClaimsNothingUntilARead(t *testing.T) {
	next := domain.Serving(testRef("next"))
	rewrite := func(r *coreRig, held func()) {
		r.answering(func(m string, _ int, req []byte) ([]byte, string, bool) {
			switch {
			case m == "OverrideBalancerTarget" && strings.Contains(string(req), "b.slot1"):
				return nil, "14", true
			case m == "ListInbounds" && slices.Contains(r.since(0), "OverrideBalancerTarget"):
				held()
				return nil, "14", true
			}
			return nil, "", false
		})
	}
	t.Run("the deadline after the pass", func(t *testing.T) {
		slowTest(t)
		r := newCoreRig(t)
		move := until(1500 * time.Millisecond)
		r.healthy(r.plan(1, move, Passage{}))
		rewrite(r, func() {})
		report, _ := r.pass(r.plan(2, move, Passage{}, onSrv, next))
		if one := r.slot(report, "slot1"); !unvouched(one) || !r.up("pxa1") {
			t.Fatalf("slot1 reports %v on %v, up %t", one.Phase, one.Applied, r.up("pxa1"))
		}
		time.Sleep(time.Until(move.until))
		r.w.gates.take(nil)
		if !r.up("pxa1") {
			t.Fatal("intake closed pxa1 at the deadline")
		}
		r.failing("ListInbounds", 1)
		r.pass(r.plan(2, move, Passage{}, onSrv, next))
		if r.up("pxa1") {
			t.Error("the failed entry read past the deadline left pxa1 up")
		}
	})
	t.Run("the deadline while repair's read is held", func(t *testing.T) {
		slowTest(t)
		r := newCoreRig(t)
		move := until(1500 * time.Millisecond)
		r.healthy(r.plan(1, move, Passage{}))
		var whileHeld bool
		rewrite(r, func() {
			time.Sleep(time.Until(move.until))
			r.w.gates.take(nil)
			whileHeld = r.up("pxa1")
		})
		report, _ := r.pass(r.plan(2, move, Passage{}, onSrv, next))
		if !whileHeld || r.up("pxa1") {
			t.Errorf("pxa1 up while the read was held %t, after the pass %t", whileHeld, r.up("pxa1"))
		}
		if one := r.slot(report, "slot1"); one.Applied != (domain.Selection{}) {
			t.Errorf("slot1 reports %v", one.Applied)
		}
	})
	t.Run("a sibling's failed add and a good read", func(t *testing.T) {
		r := newCoreRig(t)
		p := r.plan(1, until(time.Minute), Passage{})
		r.healthy(p)
		r.setInbounds(slices.DeleteFunc(r.inbounds(), func(t string) bool { return t == inboundTag("slot2", "udp") })...)
		r.failAdd(inboundTag("slot2", "udp"))
		report, _ := r.pass(p)
		if one := r.slot(report, "slot1"); one.Phase != Ready || one.Applied != onSrv {
			t.Errorf("slot1 reports %v on %v", one.Phase, one.Applied)
		}
	})
}

// W30. The core serves slot1 on Direct and the plan puts it on a server: the entry read sees Direct,
// repair installs the server, and its read fails on the same start. The written slot claims nothing, its
// gate stays up, and no read follows. The next pass, reading the server, reports it.
func TestARepairedFlipKeepsItsGateThroughAFailedRead(t *testing.T) {
	r := newCoreRig(t)
	r.overrides["slot1"] = directTag
	lists := 0
	r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
		if m == "ListInbounds" {
			lists++
			return nil, "14", lists == 3
		}
		return nil, "", false
	})
	p := r.plan(1, Passage{}, Passage{})
	report, _ := r.pass(p)
	if one := r.slot(report, "slot1"); !unvouched(one) || !r.up("pxa1") || lists != 3 {
		t.Fatalf("slot1 reports %v on %v, up %t, %d inbound lists", one.Phase, one.Applied, r.up("pxa1"), lists)
	}
	report, err := r.pass(p)
	if one := r.slot(report, "slot1"); one.Phase != Ready || one.Applied != onSrv {
		t.Errorf("the next pass: slot1 reports %v on %v, error %v", one.Phase, one.Applied, err)
	}
}

// W36. A fresh Worker's adoption reads the core's inventory, of no slot, and its entry read then fails:
// neither slot is claimed, and no gate goes down.
func TestAnAdoptionInventoryIsNoReadOfASlot(t *testing.T) {
	r := newCoreRig(t)
	lists := 0
	r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
		if m == "ListInbounds" {
			lists++
			return nil, "14", lists == 2
		}
		return nil, "", false
	})
	report, _ := r.pass(r.plan(1, Passage{}, Passage{}))
	for _, id := range []domain.SlotID{"slot1", "slot2"} {
		if s := r.slot(report, id); !unvouched(s) {
			t.Errorf("%s reports %v on %v", id, s.Phase, s.Applied)
		}
	}
	if writes := r.flagWrites(); len(writes) > 0 {
		t.Errorf("flag writes %v", writes)
	}
}

// W37. A forced close replaces slot1's UDP inbound, and the report claims slot1 only from a read after
// it: Ready on that read; nothing when the core restarted between the start read and the removal, its
// override cleared; nothing when that read fails, while pxa1 stays up.
func TestAMovesCloseClaimsItsSlotOnlyFromAReadAfterIt(t *testing.T) {
	udp := inboundTag("slot1", "udp")
	for _, c := range []struct {
		name    string
		at      func(r *coreRig, m string, req []byte, removed bool) ([]byte, string, bool)
		claimed bool
	}{
		{"read after", func(*coreRig, string, []byte, bool) ([]byte, string, bool) { return nil, "", false }, true},
		{"a restart before the removal", func(r *coreRig, m string, req []byte, removed bool) ([]byte, string, bool) {
			if m == "RemoveInbound" && strings.Contains(string(req), udp) {
				r.setStart("e2")
				r.mu.Lock()
				clear(r.overrides)
				r.mu.Unlock()
			}
			return nil, "", false
		}, false},
		{"the read after failing", func(r *coreRig, m string, _ []byte, removed bool) ([]byte, string, bool) {
			return nil, "14", m == "ListInbounds" && removed
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCoreRig(t)
			r.healthy(r.plan(1, until(time.Minute), Passage{}))
			forced := r.plan(2, until(time.Minute), Passage{})
			forced.slots[0].CloseFlows = "op1"
			start := r.logged()
			r.answering(func(m string, _ int, req []byte) ([]byte, string, bool) {
				return c.at(r, m, req, slices.Contains(r.since(start), "RemoveInbound"))
			})
			report, _ := r.pass(forced)
			calls := r.since(start)
			removal := slices.Index(calls, "RemoveInbound")
			if removal < 0 || !slices.Contains(calls[removal:], "ListInbounds") {
				t.Fatalf("no read after the relisten: %v", calls)
			}
			one := r.slot(report, "slot1")
			if c.claimed != (one.Phase == Ready && one.Applied == onSrv) || !c.claimed && !unvouched(one) || !r.up("pxa1") {
				t.Errorf("slot1 reports %v on %v, up %t, want claimed %t", one.Phase, one.Applied, r.up("pxa1"), c.claimed)
			}
		})
	}
}

// W43. B shown with its passage still planned, a sibling's rewrite and B's restoration both land, and
// repair's read fails before the deadline. slot1 claims nothing, and pxa1 stays up through the pass and
// through intake's deadline after it, as today: the next pass reading B reports it Ready, and a next
// entry read that fails past the deadline closes pxa1. On a move already past its deadline, the open
// after the failed read closes pxa1.
func TestARestoredRewriteKeepsTodaysGateThroughAFailedRead(t *testing.T) {
	next := domain.Serving(testRef("next"))
	restored := func(r *coreRig) {
		r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
			return nil, "14", m == "ListInbounds" && slices.Contains(r.since(0), "OverrideBalancerTarget")
		})
	}
	for _, c := range []struct {
		name     string
		readNext bool
	}{{"the next pass reading B", true}, {"the next entry read failing", false}} {
		t.Run(c.name, func(t *testing.T) {
			slowTest(t)
			r := newCoreRig(t)
			move := until(1500 * time.Millisecond)
			r.healthy(r.plan(1, move, Passage{}))
			restored(r)
			p := r.plan(2, move, Passage{}, onSrv, next)
			report, _ := r.pass(p)
			if one := r.slot(report, "slot1"); !unvouched(one) || !r.up("pxa1") {
				t.Fatalf("slot1 reports %v on %v, up %t", one.Phase, one.Applied, r.up("pxa1"))
			}
			time.Sleep(time.Until(move.until))
			r.w.gates.take(nil)
			if !r.up("pxa1") {
				t.Fatal("intake closed pxa1 at the deadline")
			}
			r.answering(nil)
			if !c.readNext {
				r.failing("ListInbounds", 1)
			}
			report, _ = r.pass(p)
			if one := r.slot(report, "slot1"); c.readNext && (one.Phase != Ready || one.Applied != onSrv) || !c.readNext && r.up("pxa1") {
				t.Errorf("the next pass: slot1 reports %v on %v, up %t", one.Phase, one.Applied, r.up("pxa1"))
			}
		})
	}
	t.Run("a move already past its deadline", func(t *testing.T) {
		slowTest(t)
		r := newCoreRig(t)
		late := until(300 * time.Millisecond)
		r.healthy(r.plan(1, late, Passage{}))
		time.Sleep(time.Until(late.until))
		restored(r)
		r.pass(r.plan(2, late, Passage{}, onSrv, next))
		if r.up("pxa1") {
			t.Error("the open after the failed read left an overdue move's pxa1 up")
		}
	})
}

// W10. An add the core answers with "existing tag found" changed nothing and proves nothing bound: it
// never refutes its slot, which the read after it claims, whether the list before the add failed or
// answered without the tag.
func TestADuplicateAddIsNeverALoss(t *testing.T) {
	udp := inboundTag("slot1", "udp")
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "the list before read", true: "the list before failed"}[failed], func(t *testing.T) {
			r := newCoreRig(t)
			p := r.plan(1, Passage{}, Passage{})
			r.healthy(p)
			lists := 0
			r.answering(func(m string, _ int, _ []byte) ([]byte, string, bool) {
				if m != "ListInbounds" {
					return nil, "", false
				}
				lists++
				switch {
				case lists == 1, lists == 2 && !failed:
					return r.without(udp), "0", true
				case lists == 2:
					return nil, "14", true
				}
				return nil, "", false
			})
			report, _ := r.pass(p)
			if one := r.slot(report, "slot1"); one.Applied != onSrv || !slices.Contains(report.Steps, "core inbound "+udp+" already listed") {
				t.Errorf("slot1 reports %v on %v, steps %v", one.Phase, one.Applied, report.Steps)
			}
		})
	}
}
