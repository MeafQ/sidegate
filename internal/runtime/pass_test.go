package runtime

import (
	"context"
	"errors"
	"fmt"
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

// passRig runs whole passes of a fresh Worker for slot1, on srv or next. Its core answers route queries
// by port: while narrowed, another writer's rules send every port but the sampled ones to block, until
// a routing rewrite replaces them. Its kernel is the files and tools the kernel reads, with slot1's
// plumbing complete, and each up of pxa1 records where TCP port 80 goes at that moment.
type passRig struct {
	t       *testing.T
	w       *Worker
	dir     string
	servers map[domain.ServerRef]domain.Server
	mu      sync.Mutex
	at80    []string
	held    <-chan error
}

func newPassRig(t *testing.T, up bool) *passRig {
	t.Helper()
	srv, next := testRef("srv"), testRef("next")
	r := &passRig{t: t, dir: t.TempDir(), servers: map[domain.ServerRef]domain.Server{
		srv: testServer(t, vlessHop(t, "root", "198.51.100.1", "")), next: testServer(t, vlessHop(t, "root", "198.51.100.2", "")),
	}}
	served := outboundTag(srv)
	api := fakeCore(t, func(method string, req []byte) ([]byte, string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.exists("unread") {
			return nil, "14"
		}
		if method == handlerService+"ListOutbounds" && r.exists("swap") {
			r.swap()
		}
		var resp []byte
		switch strings.TrimPrefix(strings.TrimPrefix(method, handlerService), routingService) {
		case "ListInbounds":
			for _, tag := range append(inboundTags("slot1"), probeTag) {
				resp = pbBytes(resp, 1, pbString(nil, 1, tag))
			}
		case "ListOutbounds":
			for _, tag := range []string{blockTag, directTag, outboundTag(srv), outboundTag(next)} {
				resp = pbBytes(resp, 1, pbString(nil, 1, tag))
			}
		case "ListRule":
			resp = pbBytes(nil, 1, pbString(nil, 2, "b.slot1"))
			for _, ref := range []domain.ServerRef{srv, next} {
				resp = pbBytes(resp, 1, pbString(pbString(nil, 1, outboundTag(ref)), 2, probeEmail(ref)))
			}
		case "GetInboundUsers":
			for _, ref := range []domain.ServerRef{srv, next} {
				resp = pbBytes(resp, 1, pbString(nil, 2, probeEmail(ref)))
			}
		case "GetBalancerInfo":
			resp = pbBytes(nil, 1, pbBytes(nil, 5, pbString(nil, 2, served)))
		case "OverrideBalancerTarget":
			pbFields(req, func(num int, v []byte) error {
				if num == 2 {
					served = string(v)
				}
				return nil
			})
		case "TestRoute":
			var inbound string
			var port uint64
			pbFields(req, func(num int, routing []byte) error {
				return pbScan(routing, func(num int, v uint64, b []byte) error {
					switch num {
					case 1:
						inbound = string(b)
					case 6:
						port = v
					}
					return nil
				})
			})
			switch {
			case r.exists("routeunavailable"):
				return nil, "14"
			case !slices.Contains(inboundTags("slot1"), inbound):
				return nil, "2"
			case r.exists("narrowed") && !slices.Contains(routeSamples, uint32(port)):
				return pbString(nil, 12, blockTag), "0"
			}
			return pbString(nil, 12, served), "0"
		}
		return resp, "0"
	})

	bin := filepath.Join(r.dir, "bin")
	r.write("bin/systemctl", activeCore)
	r.write("narrowed", "")
	slot := workerSlot("slot1", 1)
	a := slot.Allocation
	spec := (&kernel{}).render(Plan{slots: []SlotTarget{workerTarget(slot, domain.Serving(srv), Passage{})}}.kernel(func(SlotTarget) bool { return false }, true))
	r.write("kernel/dump", fmt.Sprintf("host\tpublic\t%d\toff\n%s\t(none)\t(none)\t%s\t0\t0\t0\t%s\n",
		a.PeerListenPort, a.Keys.ClientPublic, a.Tunnel.Masked(), peerKeepalive))
	r.write("kernel/iptables", saveOf(spec, "iptables"))
	r.write("kernel/ip6tables", saveOf(spec, "ip6tables"))
	r.write("kernel/rules", strings.Join(spec.rules, "\n")+"\n")
	r.write("kernel/routes", "2 default dev lo proto 85 scope 254\n")
	k := filepath.Join(r.dir, "kernel")
	r.write("bin/wg", "#!/bin/sh\ncat "+k+"/dump\n")
	r.write("bin/iptables-save", "#!/bin/sh\ncat "+k+"/iptables\n")
	r.write("bin/ip6tables-save", "#!/bin/sh\ncat "+k+"/ip6tables\n")
	r.write("bin/ip", "#!/bin/sh\ncase \"$*\" in\n"+
		"'-N rule show') cat "+k+"/rules ;;\n"+
		"'-N route show table 100') cat "+k+"/routes ;;\n"+
		"'rule add pref '*) pref=$4; shift 4; echo \"$pref: from all $*\" >>"+k+"/rules ;;\n"+
		"*) echo \"unexpected: ip $*\" >&2; exit 1 ;;\nesac\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.write("sys/pxa1/netdev_group", fmt.Sprintf("%d\n", linkGroup))
	r.flags(up)
	r.write("proc/all/rp_filter", "0\n")
	r.write("proc/pxa1/accept_local", "1\n")
	r.write("proc/pxa1/rp_filter", "0\n")
	savedSys, savedProc := sysNet, procConf
	sysNet, procConf = filepath.Join(r.dir, "sys"), filepath.Join(r.dir, "proc")
	t.Cleanup(func() { sysNet, procConf = savedSys, savedProc })

	data := t.TempDir()
	r.writeAt(filepath.Join(data, "current", "xray"), "#!/bin/sh\n"+
		"[ \"$1\" = version ] && { echo 'Xray 25.10.15'; exit 0; }\n"+
		"[ \"$2\" = adrules ] && [ \"$3\" != -append ] && rm -f "+filepath.Join(r.dir, "narrowed")+"\nexit 0\n")
	w, err := Open(Options{DataDir: data, RunDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	w.core.api = api
	t.Cleanup(w.core.reads.Wait)
	w.gates.write = func(link string, up bool) error {
		if up && link == "pxa1" {
			to, _, err := api.routeOf(context.Background(), inboundTag("slot1", "tcp"), networkTCP, 80)
			if err != nil {
				to = err.Error()
			}
			r.mu.Lock()
			r.at80 = append(r.at80, to)
			r.mu.Unlock()
		}
		r.flags(up)
		return nil
	}
	r.w = w
	return r
}

// saveOf is what tool's -save prints once spec's chains are in place.
func saveOf(spec kernelSpec, tool string) string {
	var b strings.Builder
	var tables []string
	for _, ref := range ownedChains {
		if ref.tool == tool && !slices.Contains(tables, ref.table) {
			tables = append(tables, ref.table)
		}
	}
	for _, table := range tables {
		refs := slices.DeleteFunc(slices.Clone(ownedChains), func(ref chainRef) bool { return ref.tool != tool || ref.table != table })
		fmt.Fprintf(&b, "*%s\n", table)
		for _, ref := range refs {
			fmt.Fprintf(&b, ":%s - [0:0]\n", ref.chain)
		}
		for _, ref := range refs {
			fmt.Fprintf(&b, "-A %s -j %s\n", ref.builtin, ref.chain)
		}
		for _, ref := range refs {
			for _, line := range spec.chains[ref] {
				b.WriteString(line + "\n")
			}
		}
		b.WriteString("COMMIT\n")
	}
	return b.String()
}

func (r *passRig) exists(name string) bool {
	_, err := os.Stat(filepath.Join(r.dir, name))
	return err == nil
}

func (r *passRig) write(name, body string) { r.writeAt(filepath.Join(r.dir, name), body) }

func (r *passRig) writeAt(path, body string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

// swap runs once, at the next outbound list after the pass's kernel read: pxa1 becomes the link the
// swap file names, "down" for ours closed by the stop hook, "foreign" for another program's, up.
func (r *passRig) swap() {
	b, err := os.ReadFile(filepath.Join(r.dir, "swap"))
	if err != nil {
		r.t.Error(err)
	}
	os.Remove(filepath.Join(r.dir, "swap"))
	switch strings.TrimSpace(string(b)) {
	case "down":
		r.flags(false)
	case "foreign":
		r.write("sys/pxa1/netdev_group", "4242\n")
		r.flags(true)
	}
}

func (r *passRig) flags(up bool) {
	r.write("sys/pxa1/flags", map[bool]string{true: "0x1003\n", false: "0x1002\n"}[up])
}

func (r *passRig) up() bool {
	up, err := linkIsUp("pxa1")
	return err == nil && up
}

func (r *passRig) plan(generation uint64, selection domain.Selection, passage Passage) *Plan {
	r.t.Helper()
	p, err := NewPlan(generation, []SlotTarget{workerTarget(workerSlot("slot1", 1), selection, passage)}, r.servers, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return &p
}

// pass hands intake p, as a plan arriving does, and runs one pass on it.
func (r *passRig) pass(p *Plan) (Report, error) {
	r.w.gates.take(p)
	return r.w.pass(context.Background(), p)
}

// afterFirstUp runs then once, inside the write of pxa1's first up, once it has landed.
func (r *passRig) afterFirstUp(then func()) {
	write, armed := r.w.gates.write, true
	r.w.gates.write = func(link string, up bool) error {
		err := write(link, up)
		if err == nil && up && link == "pxa1" && armed {
			armed = false
			then()
		}
		return err
	}
}

// holdFlagReads holds each of the next reads of pxa1's flags at a fifo until it has begun, one per
// answer. The read then gets what its answer returns, run with the flags file back in place. held
// says when every read has its answer.
func (r *passRig) holdFlagReads(answers ...func() string) {
	path := filepath.Join(r.dir, "sys", "pxa1", "flags")
	file := path + ".held"
	hold := func() error {
		if err := syscall.Mkfifo(path+".fifo", 0o600); err != nil {
			return err
		}
		if err := os.Rename(path, file); err != nil {
			return err
		}
		return os.Rename(path+".fifo", path)
	}
	done := make(chan error, 1)
	r.held = done
	if err := hold(); err != nil {
		done <- err
		return
	}
	go func() {
		done <- func() error {
			for i, answer := range answers {
				read, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					return err
				}
				err = os.Rename(file, path)
				flags := answer()
				if err == nil && i < len(answers)-1 {
					err = hold()
				}
				if err == nil {
					_, err = read.WriteString(flags)
				}
				if err := errors.Join(err, read.Close()); err != nil {
					return err
				}
			}
			return nil
		}()
	}()
}

func (r *passRig) flagsNow() string {
	b, err := os.ReadFile(filepath.Join(r.dir, "sys", "pxa1", "flags"))
	if err != nil {
		r.t.Error(err)
	}
	return string(b)
}

// lastAt80 is where port 80 went at pxa1's last up.
func (r *passRig) lastAt80() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.at80) == 0 {
		return "no up"
	}
	return r.at80[len(r.at80)-1]
}

// A gate closed while another writer has narrowed routing to the ports the check samples owes a routing
// rewrite, however the closure is read: by the pass's first read, by a pass whose core read failed, or
// by a read inside the pass that closed it. It owes one too when no read saw the link up between this
// Worker's up and the closure, whoever closed it. So the link never comes up with port 80 going to
// block. A link a kernel read stops short of is unknown, so a healthy slot's link owes nothing for it.
func TestEveryGateClosureAReadSeesOwesARewrite(t *testing.T) {
	srv, next := domain.Serving(testRef("srv")), domain.Serving(testRef("next"))
	served := outboundTag(testRef("srv"))
	healthy := func(t *testing.T, r *passRig, p *Plan) {
		t.Helper()
		report, err := r.pass(p)
		if err != nil || len(report.Steps) > 0 || !r.up() {
			t.Fatalf("the adopted healthy slot: up %t, steps %v, error %v", r.up(), report.Steps, err)
		}
	}

	t.Run("a link adopted down", func(t *testing.T) {
		r := newPassRig(t, false)
		report, err := r.pass(r.plan(1, srv, Passage{}))
		if err != nil || !r.up() || !slices.Contains(report.Steps, routingStep) || r.lastAt80() != served {
			t.Fatalf("up %t with port 80 on %s, steps %v, error %v", r.up(), r.lastAt80(), report.Steps, err)
		}
	})

	t.Run("a closure read by a pass whose core read fails", func(t *testing.T) {
		r := newPassRig(t, false)
		if _, err := r.pass(r.plan(1, srv, Passage{})); err != nil || r.lastAt80() != served {
			t.Fatalf("the adoption: port 80 on %s, error %v", r.lastAt80(), err)
		}
		r.write("narrowed", "")
		overdue := r.plan(2, next, MoveFrom(testRef("srv"), time.Now().Add(-time.Second)))
		r.w.gates.take(overdue)
		if r.up() {
			t.Fatal("intake left an overdue move's link up")
		}
		r.write("unread", "")
		if _, err := r.w.pass(context.Background(), overdue); err == nil || r.up() {
			t.Fatalf("the pass over an unread core: up %t, error %v", r.up(), err)
		}
		if err := os.Remove(filepath.Join(r.dir, "unread")); err != nil {
			t.Fatal(err)
		}
		report, err := r.pass(r.plan(3, srv, Passage{}))
		if err != nil || !r.up() || r.lastAt80() != served {
			t.Fatalf("reopened %t with port 80 on %s, steps %v, error %v", r.up(), r.lastAt80(), report.Steps, err)
		}
	})

	t.Run("a closure and a reopening within one pass", func(t *testing.T) {
		r := newPassRig(t, true)
		p := r.plan(1, srv, Passage{})
		healthy(t, r, p)
		r.write("kernel/rules", markRule()+"\n")
		report, err := r.pass(p)
		if err != nil || !r.up() || !slices.Contains(report.Steps, "link pxa1 down") || r.lastAt80() != served {
			t.Fatalf("reopened %t with port 80 on %s, steps %v, error %v", r.up(), r.lastAt80(), report.Steps, err)
		}
	})

	// The adoption opens pxa1 on srv, and the link is closed again before a read shows it up.
	adoption := func(t *testing.T, r *passRig) {
		t.Helper()
		_, err := r.pass(r.plan(1, srv, Passage{}))
		if r.held != nil {
			err = errors.Join(err, <-r.held)
		}
		if err != nil || r.up() || r.lastAt80() != served {
			t.Fatalf("the adoption: up %t, port 80 on %s at its up, error %v", r.up(), r.lastAt80(), err)
		}
	}
	reopen := func(t *testing.T, r *passRig) {
		t.Helper()
		report, err := r.pass(r.plan(3, srv, Passage{}))
		if err != nil || !r.up() || !slices.Contains(report.Steps, routingStep) || r.lastAt80() != served {
			t.Fatalf("reopened %t with port 80 on %s, steps %v, error %v", r.up(), r.lastAt80(), report.Steps, err)
		}
	}
	closing := func(r *passRig) func() string {
		return func() string {
			r.write("narrowed", "")
			r.w.gates.take(r.plan(2, next, MoveFrom(testRef("srv"), time.Now().Add(-time.Second))))
			return r.flagsNow()
		}
	}

	t.Run("a closure by intake between a read's two samples of the flags", func(t *testing.T) {
		r := newPassRig(t, false)
		var samples []string
		sample := func(answer func() string) func() string {
			return func() string { flags := answer(); samples = append(samples, flags); return flags }
		}
		r.afterFirstUp(func() { r.holdFlagReads(sample(r.flagsNow), sample(closing(r))) })
		adoption(t, r)
		if !slices.Equal(samples, []string{"0x1003\n", "0x1002\n"}) {
			t.Fatalf("the read after the up sampled %q", samples)
		}
		reopen(t, r)
	})

	t.Run("a closure by intake before the next read", func(t *testing.T) {
		r := newPassRig(t, false)
		r.afterFirstUp(func() { r.holdFlagReads(closing(r)) })
		adoption(t, r)
		reopen(t, r)
	})

	t.Run("a closure by another writer before the next read", func(t *testing.T) {
		r := newPassRig(t, false)
		r.afterFirstUp(func() { r.write("narrowed", ""); r.flags(false) })
		adoption(t, r)
		reopen(t, r)
	})

	t.Run("a link a partial read misses", func(t *testing.T) {
		r := newPassRig(t, true)
		p := r.plan(1, srv, Passage{})
		healthy(t, r, p)
		r.write("sys/pxa1/flags", "unreadable\n")
		report, err := r.pass(p)
		if err == nil || slices.Contains(report.Steps, routingStep) {
			t.Fatalf("a read that missed pxa1: steps %v, error %v", report.Steps, err)
		}
		r.flags(true)
		healthy(t, r, p)
	})
}

// A route test the API couldn't answer is a failed read, not a router that matched nothing. So a
// healthy slot's gate rides it as it rides any unread core within readBound, and the report claims
// nothing from it.
func TestAnUnavailableRouteTestIsAFailedRead(t *testing.T) {
	r := newPassRig(t, true)
	p := r.plan(1, domain.Serving(testRef("srv")), Passage{})
	if report, err := r.pass(p); err != nil || !r.up() || len(report.Steps) > 0 {
		t.Fatalf("the adopted healthy slot: up %t, steps %v, error %v", r.up(), report.Steps, err)
	}
	r.write("routeunavailable", "")
	report, err := r.pass(p)
	if err == nil || !strings.Contains(err.Error(), "grpc status 14") {
		t.Errorf("the pass whose route test was unavailable: error %v, want the read's failure", err)
	}
	if !r.up() || slices.Contains(report.Steps, "link pxa1 down") {
		t.Errorf("the healthy gate: up %t, steps %v", r.up(), report.Steps)
	}
	if applied := report.Slots[0].Applied; applied != (domain.Selection{}) {
		t.Errorf("the report claims %v from a read that failed", applied)
	}
}

// The stop hook can close a gate after the pass's last read of the links, and a pass whose converges
// change nothing reads them no more. The final sample sees the closure.
func TestAStopHookClosureIsReportedClosed(t *testing.T) {
	r := newPassRig(t, true)
	p := r.plan(1, domain.Serving(testRef("srv")), Passage{})
	if _, err := r.pass(p); err != nil || !r.up() {
		t.Fatalf("the adopted healthy slot: up %t, error %v", r.up(), err)
	}
	r.write("swap", "down")
	report, err := r.pass(p)
	if err != nil || len(report.Steps) > 0 {
		t.Fatalf("the pass: steps %v, error %v", report.Steps, err)
	}
	if phase := report.Slots[0].Phase; phase != Closed {
		t.Errorf("a gate the stop hook closed reads %v", phase)
	}
}

// Another program can take pxa1's name with its own link, up, while our slot's core reads applied.
// That link is never our gate, so the slot reads closed and its move can't land on it.
func TestAForeignLinkAtOurNameIsNeverReady(t *testing.T) {
	r := newPassRig(t, true)
	p := r.plan(1, domain.Serving(testRef("srv")), Passage{})
	if _, err := r.pass(p); err != nil || !r.up() {
		t.Fatalf("the adopted healthy slot: up %t, error %v", r.up(), err)
	}
	r.write("swap", "foreign")
	report, _ := r.pass(p)
	if phase := report.Slots[0].Phase; phase != Closed {
		t.Errorf("a foreign pxa1, up, reads %v", phase)
	}
	if !r.up() {
		t.Error("the foreign link was closed")
	}
}

// A Down move lands only on a sample that shows its link closed. A link the sample couldn't read is
// unknown, and lands nothing.
func TestAnUnreadLinkLandsNothing(t *testing.T) {
	down := domain.Closed(domain.ConfirmedDead)
	t.Run("a link whose flags can't be read", func(t *testing.T) {
		r := newPassRig(t, false)
		p := r.plan(1, down, Passage{})
		r.w.gates.take(p)
		r.write("sys/pxa1/flags", "unreadable\n")
		report, _ := r.w.pass(context.Background(), p)
		if phase := report.Slots[0].Phase; phase != Unknown {
			t.Errorf("an unread link reads %v", phase)
		}
	})
	t.Run("a dump that ends on an error after a list without pxa1", func(t *testing.T) {
		r := newPassRig(t, false)
		saved := linkDump
		t.Cleanup(func() { linkDump = saved })
		linkDump = func() (map[string]linkSample, error) {
			return parseLinkDump(slices.Concat(linkMessage(0, "lo", 1, 0), doneMessage(-4)))
		}
		report, _ := r.pass(r.plan(1, down, Passage{}))
		if phase := report.Slots[0].Phase; phase != Unknown {
			t.Errorf("a link missing from a dump that failed reads %v", phase)
		}
	})
}
