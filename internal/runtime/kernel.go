package runtime

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"sidegate/internal/domain"
)

// Kernel plumbing is driven through the gateway's own ip, wg, and iptables-save /
// iptables-restore. Legacy iptables has no netlink interface: a table is one
// binary blob replaced through setsockopt, which only the iptables tools encode,
// and using the same tools UniFi provisions with keeps us on its backend. Links,
// rules, and peers could use netlink, but a second mechanism would add a
// dependency and code for a few calls per pass; the steady-state pass is a
// handful of short execs plus /sys and /proc reads.

const (
	linkPrefix = "pxa"
	// linkGroup marks this product's pxa links on any gateway. It is
	// below 2^31, the largest group iproute2 accepts. `ip link add` sets it in the
	// same message that creates the link, so no crash can leave a link of ours
	// unmarked; 5.4 ignores an ifalias given at creation. There is one install, so
	// a reinstall after /data was lost owns every link an earlier one made.
	linkGroup = 0x55565041
	// marker is the rtnetlink protocol on this product's ip rules and
	// table 100 route. Another program can pick the same number, so it never makes
	// a rule ours on its own; see sortRules. Deletes name it too, and the kernel
	// then removes only a rule or route that carries it.
	marker = 85
	// tproxyMark is set with a full mask, so it can't alias UniFi's 0x7e0000
	// route marks or the 0x10000 kill-switch bit.
	tproxyMark  = 0x169
	localTable  = 100
	markPref    = 99
	ingressPref = 100 // Unmarked packets from a pxa link: unreachable, never WAN.
)

// Rules and routes are read with `ip -N`, so a name in /etc/iproute2 for our
// protocol, table, or scope changes nothing we read. It numbers rule and route
// types too, though `ip route add` takes a type only as a word, so reads turn those
// two back into the words render writes.
const (
	rtnLocal       = "2"
	rtnUnreachable = "7"
	scopeHost      = 254
)

var localRoute = fmt.Sprintf("local default dev lo proto %d scope %d", marker, scopeHost)

// chainRef names one chain we own, the built-in chain that jumps to it, and the
// tool family, iptables or ip6tables, that saves and restores it.
type chainRef struct{ tool, table, builtin, chain string }

var ownedChains = []chainRef{
	{"iptables", "raw", "PREROUTING", "sidegate-prerouting"},
	{"iptables", "mangle", "PREROUTING", "sidegate-prerouting"},
	{"iptables", "filter", "INPUT", "sidegate-input"},
	{"iptables", "filter", "OUTPUT", "sidegate-output"},
	// WireGuard listens on the IPv6 wildcard too, so the LAN could reach a pxa
	// port over IPv6 without this.
	{"ip6tables", "filter", "INPUT", "sidegate-input"},
}

// kernelWant is what the kernel should hold.
type kernelWant struct {
	slots   []plumbing
	retired []string // Link names of slots being removed, whose rules are still ours once the link is gone.
	core    bool     // The core may be running, so its loopback listeners need the owner guard.
}

type plumbing struct {
	Slot       domain.SlotID
	Allocation domain.Allocation
	HostKey    domain.Secret
	Open       bool
}

// link is one WireGuard peer link as desired or as read back.
type link struct {
	slot        domain.SlotID // Desired links only.
	tunnel      netip.Prefix  // Desired links only.
	up          bool
	listenPort  uint16
	privateKey  string
	peers       map[string]peer // By public key.
	handshake   time.Time       // The newest with any peer. Zero if none yet.
	acceptLocal string
	rpFilter    string
}

// peer is a WireGuard peer as `wg show dump` prints it. The keepalive makes our
// side handshake as soon as a gate opens: without it the UniFi side keeps its
// stale session until its own timeout, about 15 s of dead slot after every
// reopen (measured 16.3 s against 5 ms with it).
type peer struct {
	allowedIPs string
	keepalive  string
}

const peerKeepalive = "25"

// kernelSpec is the owned kernel state, rendered from desired plumbing alone.
// An empty spec owns nothing: no links, chains, jumps, rules, or routes.
type kernelSpec struct {
	links     map[string]link
	chains    map[chainRef][]string                   // iptables-save lines; absent key = no chain.
	slotLines map[domain.SlotID]map[chainRef][]string // Each slot's contiguous run in chains.
	rules     []string                                // `ip rule show` lines at our prefs.
	routes    []string                                // `ip route show table localTable` lines.
	retired   []string                                // See kernelWant.
}

// kernelObs is what inspection read back. Only owned links are kept; a link is
// owned when its group is linkGroup.
type kernelObs struct {
	readErr       error // Why the read stopped short; then only each link's up flag is known.
	links         map[string]link
	foreign       []string // Link names we want but someone else holds.
	chains        map[chainRef][]string
	jumps         map[chainRef][]int // Positions of our jump in the built-in chain.
	rules         []string           // Ours only.
	foreignRules  []string           // Foreign software's, before or beside ours, able to match our traffic.
	routes        []string           // Ours only.
	foreignRoutes []string           // Anyone else's in table localTable.
	allRPFilter   string
}

type kernel struct {
	tools
	// gateLock orders opening a gate against close-gates, the core's stop hook.
	// An opener confirms the core and sets the link up while holding it, so a
	// core that dies after the confirmation has a stop hook that runs after the
	// link is up and closes it.
	gateLock string
	gates    *gates // Every link flag the Worker writes.
}

// openFunc re-proves, immediately before a gate opens, that the running core still
// routes slot as planned, and then opens it.
type openFunc func(ctx context.Context, slot domain.SlotID) error

func linkName(a domain.Allocation) string { return fmt.Sprintf("%s%d", linkPrefix, a.Ordinal) }

// flowMark is the SO_MARK on a slot's inbound. Its accepted sockets inherit it, whatever the kernel's
// tcp_fwmark_accept says, and it is the one thing that names their slot: a peer in the tunnel holds
// only while UniFi masquerades. Like tproxyMark it stays clear of UniFi's route marks.
func flowMark(a domain.Allocation) uint32 { return tproxyMark + uint32(a.Ordinal) }

func markRule() string {
	return fmt.Sprintf("%d: from all fwmark 0x%x lookup %d proto %d", markPref, tproxyMark, localTable, marker)
}

func ingressRule(link string) string {
	return fmt.Sprintf("%d: from all iif %s unreachable proto %d", ingressPref, link, marker)
}

func (k *kernel) render(want kernelWant) kernelSpec {
	spec := kernelSpec{
		links:     map[string]link{},
		chains:    map[chainRef][]string{},
		slotLines: map[domain.SlotID]map[chainRef][]string{},
		retired:   want.retired,
	}
	if len(want.slots) == 0 && !want.core {
		return spec
	}
	desired := slices.SortedFunc(slices.Values(want.slots), func(a, b plumbing) int {
		return cmp.Compare(a.Allocation.Ordinal, b.Allocation.Ordinal)
	})
	for _, ref := range ownedChains {
		spec.chains[ref] = []string{}
	}
	if want.core {
		spec.chains[ownedChains[3]] = coreGuard(desired)
	}
	if len(desired) > 0 {
		spec.rules = []string{markRule()}
		spec.routes = []string{localRoute}
	}
	mark := fmt.Sprintf("0x%x/0xffffffff", tproxyMark)
	for _, p := range desired {
		a := p.Allocation
		name, tunnel := linkName(a), a.Tunnel.Masked().String()
		spec.links[name] = link{
			slot: p.Slot, tunnel: a.Tunnel.Masked(), up: p.Open, listenPort: a.PeerListenPort, privateKey: p.HostKey.Reveal(),
			peers:       map[string]peer{a.Keys.ClientPublic: {tunnel, peerKeepalive}},
			acceptLocal: "1", rpFilter: "0",
		}
		in, listener := "-i "+name, fmt.Sprintf("-A sidegate-input ! -i lo -p udp -m udp --dport %d -j DROP", a.PeerListenPort)
		tproxy := fmt.Sprintf("-j TPROXY --on-port %d --on-ip 127.0.0.1 --tproxy-mark %s", a.TPROXYPort, mark)
		lines := map[chainRef][]string{
			ownedChains[0]: {"-A sidegate-prerouting " + in + " -j CT --notrack"},
			ownedChains[1]: {
				"-A sidegate-prerouting " + in + " -p udp -m udp --dport 443 -j DROP",
				"-A sidegate-prerouting " + in + " -p tcp " + tproxy,
				"-A sidegate-prerouting " + in + " -p udp " + tproxy,
				"-A sidegate-prerouting " + in + " -j DROP",
			},
			ownedChains[2]: {listener},
			ownedChains[4]: {listener},
			// The reply shortcut leans on UniFi's masquerade conntrack entry. A core
			// packet for a flow whose entry is gone is delivered to the VPN client's
			// own address, and the kernel answers with a RST toward the target that
			// routes out the WAN unNATed. Packets from the tunnel leave only by the
			// VPN client or loopback.
			ownedChains[3]: {
				"-A sidegate-output -s " + tunnel + " -o wgclt+ -j RETURN",
				"-A sidegate-output -s " + tunnel + " ! -o lo -j DROP",
			},
		}
		spec.slotLines[p.Slot] = lines
		for ref, l := range lines {
			spec.chains[ref] = append(spec.chains[ref], l...)
		}
		spec.rules = append(spec.rules, ingressRule(name))
	}
	return spec
}

// coreGuard keeps the core's loopback listeners to root, which runs the daemon
// and the core. Without it any local user could re-point routing through the
// API, or send traffic through our servers by the probe and slot inbounds. It
// rejects every packet from another user's socket, so a connection opened while
// the guard was missing dies on its next packet. It names uids 1 and up rather
// than negating uid 0: on 5.4 a negated owner match, with or without
// --socket-exists, also matches the RSTs and ACKs the kernel sends from its
// control socket, and rejecting those strands the core's side of root's own
// connections.
func coreGuard(slots []plumbing) []string {
	tcp := []string{strconv.Itoa(int(apiAddr.Port())), strconv.Itoa(int(probeAddr.Port()))}
	var udp []string
	for _, p := range slots {
		port := strconv.Itoa(int(p.Allocation.TPROXYPort))
		tcp, udp = append(tcp, port), append(udp, port)
	}
	const loopback, notRoot = "-A sidegate-output -d 127.0.0.1/32 -o lo -p ", " -m owner --uid-owner 1-4294967294 -j REJECT --reject-with "
	lines := []string{loopback + "tcp -m multiport --dports " + strings.Join(tcp, ",") + notRoot + "tcp-reset"}
	if len(udp) > 0 {
		lines = append(lines, loopback+"udp -m multiport --dports "+strings.Join(udp, ",")+notRoot+"icmp-port-unreachable")
	}
	return lines
}

type changeKind uint8

// Change kinds in apply order: close gates first, then drop stale rules and only then
// their links, since a rule is ours by the link it names and a stale rule would
// outlive it. Then the loopback guard and interception before any new WireGuard
// listener exists, and open gates last.
const (
	linkDown changeKind = iota
	ruleDelete
	linkDelete
	chainWrite
	ruleAdd
	routeDelete
	routeAdd
	linkCreate
	linkConfigure
	linkSysctl
	linkUp
)

type change struct {
	kind  changeKind
	link  string
	chain chainRef
	text  string // Rule or route text, or the iptables-restore payload.
	stale []string
}

func (c change) String() string {
	switch c.kind {
	case linkDown:
		return "link " + c.link + " down"
	case linkUp:
		return "link " + c.link + " up"
	case linkDelete:
		return "link " + c.link + " delete"
	case linkCreate:
		return "link " + c.link + " create"
	case linkConfigure:
		return "link " + c.link + " wireguard config"
	case linkSysctl:
		return "link " + c.link + " sysctl accept_local=1 rp_filter=0"
	case chainWrite:
		return c.chain.tool + " " + c.chain.table + " " + c.chain.chain + " write"
	case ruleDelete:
		return "ip rule delete " + c.text
	case ruleAdd:
		return "ip rule add " + c.text
	case routeDelete:
		return fmt.Sprintf("ip route delete %s table %d", c.text, localTable)
	case routeAdd:
		return fmt.Sprintf("ip route add %s table %d", c.text, localTable)
	}
	return "unknown change"
}

// diff lists the changes that take have to want, in apply order. It is pure.
func diff(want kernelSpec, have kernelObs) []change {
	var out []change
	for _, name := range slices.Sorted(maps.Keys(have.links)) {
		got := have.links[name]
		w, ok := want.links[name]
		if got.up && !w.up {
			out = append(out, change{kind: linkDown, link: name})
		}
		if !ok {
			out = append(out, change{kind: linkDelete, link: name})
		}
	}
	for _, ref := range ownedChains {
		lines, wanted := want.chains[ref]
		got, exists := have.chains[ref]
		jumps := have.jumps[ref]
		if wanted && (!exists || !slices.Equal(lines, got) || !slices.Equal(jumps, []int{0})) ||
			!wanted && (exists || len(jumps) > 0) {
			out = append(out, change{kind: chainWrite, chain: ref, text: restorePayload(ref, lines, wanted, exists, len(jumps))})
		}
	}
	for _, r := range have.rules {
		if !slices.Contains(want.rules, r) {
			out = append(out, change{kind: ruleDelete, link: ruleLink(r), text: r})
		}
	}
	for _, r := range want.rules {
		if !slices.Contains(have.rules, r) {
			out = append(out, change{kind: ruleAdd, link: ruleLink(r), text: r})
		}
	}
	for _, r := range have.routes {
		if !slices.Contains(want.routes, r) {
			out = append(out, change{kind: routeDelete, text: r})
		}
	}
	for _, r := range want.routes {
		if !slices.Contains(have.routes, r) {
			out = append(out, change{kind: routeAdd, text: r})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want.links)) {
		w := want.links[name]
		got, exists := have.links[name]
		if !exists {
			out = append(out, change{kind: linkCreate, link: name})
		}
		if !exists || got.listenPort != w.listenPort || got.privateKey != w.privateKey || !maps.Equal(got.peers, w.peers) {
			var stale []string
			for pub := range got.peers {
				if _, keep := w.peers[pub]; !keep {
					stale = append(stale, pub)
				}
			}
			slices.Sort(stale)
			out = append(out, change{kind: linkConfigure, link: name, stale: stale})
		}
		if !exists || got.acceptLocal != w.acceptLocal || got.rpFilter != w.rpFilter {
			out = append(out, change{kind: linkSysctl, link: name})
		}
		if w.up && !got.up {
			out = append(out, change{kind: linkUp, link: name})
		}
	}
	slices.SortStableFunc(out, func(a, b change) int { return cmp.Compare(a.kind, b.kind) })
	return out
}

// ruleLink names the link one of our rules belongs to, if any.
func ruleLink(text string) string {
	r, _, err := parseRule(text)
	if err != nil {
		return ""
	}
	return r.iif
}

// restorePayload rewrites one owned chain and its jump through
// `iptables-restore --noflush`. Declaring a user chain there creates or flushes
// it, and the table commits in one step, so traffic never sees a partial chain or
// a missing jump. Nothing outside our chain and our jump is named. The jump is
// always re-inserted first in the built-in chain, ahead of rules UniFi appends.
func restorePayload(ref chainRef, lines []string, wanted, exists bool, jumps int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%s\n", ref.table)
	if wanted {
		fmt.Fprintf(&b, ":%s - [0:0]\n", ref.chain)
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	for range jumps {
		fmt.Fprintf(&b, "-D %s -j %s\n", ref.builtin, ref.chain)
	}
	if wanted {
		fmt.Fprintf(&b, "-I %s 1 -j %s\n", ref.builtin, ref.chain)
	} else if exists {
		fmt.Fprintf(&b, "-F %s\n-X %s\n", ref.chain, ref.chain)
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// ready reports whether slot's plumbing in have is complete: its link is
// configured, every shared jump, rule, and route is in place, and its own rules
// sit in our chains as one contiguous run. A gate opens only when ready.
func ready(want kernelSpec, have kernelObs, slot domain.SlotID) bool {
	var name string
	for n, l := range want.links {
		if l.slot == slot {
			name = n
		}
	}
	got, ok := have.links[name]
	if name == "" || !ok {
		return false
	}
	w := want.links[name]
	if got.listenPort != w.listenPort || got.privateKey != w.privateKey || !maps.Equal(got.peers, w.peers) ||
		got.acceptLocal != w.acceptLocal || got.rpFilter != w.rpFilter || have.allRPFilter != "0" {
		return false
	}
	for _, r := range []string{markRule(), ingressRule(name)} {
		if !slices.Contains(have.rules, r) {
			return false
		}
	}
	if !slices.Contains(have.routes, localRoute) {
		return false
	}
	for ref, lines := range want.slotLines[slot] {
		if !slices.Equal(have.jumps[ref], []int{0}) || !containsRun(have.chains[ref], lines) {
			return false
		}
	}
	return true
}

func containsRun(chain, run []string) bool {
	for i := 0; i+len(run) <= len(chain); i++ {
		if slices.Equal(chain[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

// conflicts lists state at our names and preferences that foreign software holds.
// Conflict is an error, never an opportunity to take over or flush.
func conflicts(have kernelObs) []string {
	var out []string
	for _, name := range have.foreign {
		out = append(out, "link "+name+" exists and is not ours")
	}
	if have.allRPFilter != "0" {
		out = append(out, "net.ipv4.conf.all.rp_filter is "+have.allRPFilter+", pxa links need 0")
	}
	for _, r := range have.foreignRules {
		out = append(out, fmt.Sprintf("foreign ip rule %q can match pxa traffic", r))
	}
	for _, r := range have.foreignRoutes {
		out = append(out, fmt.Sprintf("foreign route %q in table %d", r, localTable))
	}
	return out
}

func (k *kernel) observe(ctx context.Context, want kernelSpec) kernelObs {
	obs := kernelObs{links: map[string]link{}, chains: map[chainRef][]string{}, jumps: map[chainRef][]int{}}
	obs.readErr = k.read(ctx, want, &obs)
	return obs
}

// read fills obs. It reads every link's up flag first, from /sys with no tool, so a
// wedged tool cannot hide which gates are open.
func (k *kernel) read(ctx context.Context, want kernelSpec, obs *kernelObs) error {
	names, err := k.ownedLinks()
	if err != nil {
		return err
	}
	for name, owned := range names {
		if owned {
			up, err := linkIsUp(name)
			if err != nil {
				return err
			}
			obs.links[name] = link{up: up}
		}
	}
	reach := map[string]netip.Prefix{}
	for name, owned := range names {
		if owned {
			reach[name] = netip.Prefix{}
		}
	}
	for name, l := range want.links {
		reach[name] = l.tunnel
	}
	retired := slices.DeleteFunc(slices.Clone(want.retired), func(name string) bool {
		_, held := names[name]
		return held
	})
	for name, owned := range names {
		if !owned {
			if _, ok := want.links[name]; ok {
				obs.foreign = append(obs.foreign, name)
			}
			continue
		}
		l, err := k.readLink(ctx, name)
		if err != nil {
			return err
		}
		obs.links[name] = l
	}
	slices.Sort(obs.foreign)

	for _, tool := range []string{"iptables", "ip6tables"} {
		save, err := k.run(ctx, nil, tool+"-save")
		if err != nil {
			return err
		}
		chains, jumps := parseSave(save, tool)
		maps.Copy(obs.chains, chains)
		maps.Copy(obs.jumps, jumps)
	}

	rules, err := k.run(ctx, nil, "ip", "-N", "rule", "show")
	if err != nil {
		return err
	}
	atPrefs, err := parseRules(rules)
	if err != nil {
		return err
	}
	obs.rules, obs.foreignRules = sortRules(atPrefs, reach, retired)
	// The kernel creates a table with its first route, so on a fresh boot table 100
	// does not exist yet and the dump fails. That is an empty table.
	routes, err := k.run(ctx, nil, "ip", "-N", "route", "show", "table", strconv.Itoa(localTable))
	if err != nil && !strings.Contains(err.Error(), "FIB table does not exist") {
		return err
	}
	for _, r := range lines(routes) {
		if rest, ok := strings.CutPrefix(r, rtnLocal+" "); ok && "local "+rest == localRoute {
			obs.routes = append(obs.routes, localRoute)
		} else {
			obs.foreignRoutes = append(obs.foreignRoutes, r)
		}
	}
	obs.allRPFilter = readSysctl("all", "rp_filter")
	return nil
}

// sysNet is where links are read, and procConf where their IPv4 settings are. Tests point them at
// directories of their own.
var sysNet, procConf = "/sys/class/net", "/proc/sys/net/ipv4/conf"

// ownedLinks maps every pxa-prefixed link to whether its group is ours.
func (k *kernel) ownedLinks() (map[string]bool, error) {
	entries, err := os.ReadDir(sysNet)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, linkPrefix) {
			continue
		}
		out[name] = linkOwned(name)
	}
	return out, nil
}

// linkOwned says the link exists and its group is ours.
func linkOwned(name string) bool {
	group, err := os.ReadFile(filepath.Join(sysNet, name, "netdev_group"))
	return err == nil && strings.TrimSpace(string(group)) == strconv.Itoa(linkGroup)
}

func (k *kernel) readLink(ctx context.Context, name string) (link, error) {
	l := link{peers: map[string]peer{}}
	up, err := linkIsUp(name)
	if err != nil {
		return l, err
	}
	l.up = up
	dump, err := k.run(ctx, nil, "wg", "show", name, "dump")
	if err != nil {
		return l, err
	}
	if err := parseDump(&l, name, dump); err != nil {
		return l, err
	}
	l.acceptLocal = readSysctl(name, "accept_local")
	l.rpFilter = readSysctl(name, "rp_filter")
	return l, nil
}

func parseDump(l *link, name string, dump []byte) error {
	for i, line := range lines(dump) {
		fields := strings.Split(line, "\t")
		switch {
		// Ubiquiti's wg adds a field to each line: the third on the interface line, and
		// a last one on a peer line. The fields read here stay at the positions below.
		case i == 0 && (len(fields) == 4 || len(fields) == 5):
			l.privateKey = fields[0]
			port, _ := strconv.ParseUint(fields[len(fields)-2], 10, 16)
			l.listenPort = uint16(port)
		case i > 0 && (len(fields) == 8 || len(fields) == 9):
			l.peers[fields[0]] = peer{allowedIPs: fields[3], keepalive: fields[7]}
			if at, err := strconv.ParseInt(fields[4], 10, 64); err == nil && at > 0 && time.Unix(at, 0).After(l.handshake) {
				l.handshake = time.Unix(at, 0)
			}
		default:
			return fmt.Errorf("wg show %s dump: unexpected line %d", name, i)
		}
	}
	return nil
}

func linkIsUp(name string) (bool, error) {
	flags, err := os.ReadFile(filepath.Join(sysNet, name, "flags"))
	if err != nil {
		return false, err
	}
	f, err := strconv.ParseUint(strings.TrimSpace(string(flags)), 0, 32)
	return f&syscall.IFF_UP != 0, err
}

func readSysctl(dev, key string) string {
	b, err := os.ReadFile(filepath.Join(procConf, dev, key))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// parseSave extracts our chains and the positions of our jumps from tool's
// -save output. A table it does not print has neither.
func parseSave(save []byte, tool string) (map[chainRef][]string, map[chainRef][]int) {
	chains, jumps := map[chainRef][]string{}, map[chainRef][]int{}
	var table string
	builtinIndex := map[string]int{}
	for _, line := range lines(save) {
		switch {
		case strings.HasPrefix(line, "*"):
			table = line[1:]
			clear(builtinIndex)
		case strings.HasPrefix(line, ":"):
			name, _, _ := strings.Cut(line[1:], " ")
			for _, ref := range ownedChains {
				if ref.tool == tool && ref.table == table && ref.chain == name {
					chains[ref] = []string{}
				}
			}
		case strings.HasPrefix(line, "-A "):
			name, _, _ := strings.Cut(line[3:], " ")
			for _, ref := range ownedChains {
				if ref.tool != tool || ref.table != table {
					continue
				}
				if name == ref.chain {
					chains[ref] = append(chains[ref], line)
				}
				if name == ref.builtin && line == "-A "+ref.builtin+" -j "+ref.chain {
					jumps[ref] = append(jumps[ref], builtinIndex[name])
				}
			}
			builtinIndex[name]++
		}
	}
	return chains, jumps
}

// parseRules reads the rules that run before or beside ours. A rule after ours sees
// no pxa traffic: the iif unreachable rule and table 100's default route end it.
func parseRules(out []byte) ([]ipRule, error) {
	var rules []ipRule
	for _, line := range lines(out) {
		r, pref, err := parseRule(line)
		if err != nil {
			return nil, err
		}
		if pref <= ingressPref {
			rules = append(rules, r)
		}
	}
	return rules, nil
}

func lines(b []byte) []string {
	var out []string
	s := bufio.NewScanner(bytes.NewReader(b))
	for s.Scan() {
		if l := strings.TrimSpace(s.Text()); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// sortRules splits rules into ours and the foreign ones that can match our traffic.
// reach holds the links of ours and the names we want, each with its tunnel when we
// know it; retired holds the names of slots being removed that no link holds now. A
// rule is ours only in the exact shape render writes, marker included, and with an
// iif in one of those; only ours are ever deleted. Any other rule is foreign, marker
// or no marker, and a conflict only if it can match.
func sortRules(rules []ipRule, reach map[string]netip.Prefix, retired []string) (ours, foreign []string) {
	for _, r := range rules {
		_, reached := reach[r.iif]
		switch {
		case r.text == markRule() || (reached || slices.Contains(retired, r.iif)) && r.text == ingressRule(r.iif):
			ours = append(ours, r.text)
		case r.canMatch(reach):
			foreign = append(foreign, r.text)
		}
	}
	return ours, foreign
}

// converge applies diff(want, have) and reads the result back. With nothing to
// change it returns have untouched, so a steady-state pass costs one inspection.
// When have could not be read whole it can only close the gates want no longer
// holds open. When foreign software holds state at our names or preferences, it
// changes nothing of theirs: it closes our open gates, since nothing behind them
// can be proven, and reports the conflict. A gate opens only through open; with no
// open, none does.
func (k *kernel) converge(ctx context.Context, want kernelSpec, have kernelObs, open openFunc) (kernelObs, []string, error) {
	if have.readErr != nil {
		var closes []change
		for _, name := range slices.Sorted(maps.Keys(have.links)) {
			if have.links[name].up && !want.links[name].up {
				closes = append(closes, change{kind: linkDown, link: name})
			}
		}
		return k.applyAll(ctx, closes, want, have, nil)
	}
	changes := diff(want, have)
	if c := conflicts(have); len(c) > 0 {
		changes = nil
		for _, name := range slices.Sorted(maps.Keys(have.links)) {
			if have.links[name].up {
				changes = append(changes, change{kind: linkDown, link: name})
			}
		}
		after, done, err := k.applyAll(ctx, changes, want, have, nil)
		return after, done, errors.Join(fmt.Errorf("kernel conflicts: %s", strings.Join(c, "; ")), err)
	}
	return k.applyAll(ctx, changes, want, have, open)
}

// applyAll applies changes in order. A change that fails stops only what depends on
// it: the rest of its own link's changes, or, for shared state or a gate lock nobody
// can take, every gate still to open. Each failure is returned, and the result is
// read back whatever happened; a failed read is in the returned kernelObs.
func (k *kernel) applyAll(ctx context.Context, changes []change, want kernelSpec, have kernelObs, open openFunc) (kernelObs, []string, error) {
	if len(changes) == 0 {
		return have, nil, nil
	}
	var done []string
	var errs []error
	failed, noOpens := map[string]bool{}, false
	for _, c := range changes {
		if c.link != "" && failed[c.link] || c.kind == linkUp && noOpens {
			continue
		}
		err := k.apply(ctx, c, want, open)
		if err == nil {
			done = append(done, c.String())
			continue
		}
		errs = append(errs, err)
		if c.link != "" {
			failed[c.link] = true
		}
		noOpens = noOpens || c.link == "" || errors.Is(err, errGateLocked)
	}
	return k.observe(ctx, want), done, errors.Join(errs...)
}

func (k *kernel) apply(ctx context.Context, c change, want kernelSpec, open openFunc) error {
	var err error
	switch c.kind {
	case linkDown:
		err = k.gates.close(c.link)
	case linkUp:
		err = k.open(ctx, want.links[c.link].slot, open)
	case linkDelete:
		_, err = k.run(ctx, nil, "ip", "link", "del", "dev", c.link)
	case linkCreate:
		_, err = k.run(ctx, nil, "ip", "link", "add", "dev", c.link, "group", strconv.Itoa(linkGroup), "type", "wireguard")
	case linkConfigure:
		w := want.links[c.link]
		args := []string{"set", c.link, "listen-port", strconv.Itoa(int(w.listenPort)), "private-key", "/dev/stdin"}
		for _, pub := range c.stale {
			args = append(args, "peer", pub, "remove")
		}
		for _, pub := range slices.Sorted(maps.Keys(w.peers)) {
			args = append(args, "peer", pub, "allowed-ips", w.peers[pub].allowedIPs,
				"persistent-keepalive", w.peers[pub].keepalive)
		}
		_, err = k.run(ctx, []byte(w.privateKey+"\n"), "wg", args...)
	case linkSysctl:
		err = errors.Join(
			os.WriteFile(filepath.Join(procConf, c.link, "accept_local"), []byte("1"), 0),
			os.WriteFile(filepath.Join(procConf, c.link, "rp_filter"), []byte("0"), 0))
	case chainWrite:
		_, err = k.run(ctx, []byte(c.text), c.chain.tool+"-restore", "-w", "5", "--noflush")
	case ruleAdd, ruleDelete:
		verb := map[changeKind]string{ruleAdd: "add", ruleDelete: "del"}[c.kind]
		pref, selector, _ := strings.Cut(c.text, ": ")
		args := append([]string{"rule", verb, "pref", pref}, strings.Fields(strings.TrimPrefix(selector, "from all"))...)
		_, err = k.run(ctx, nil, "ip", args...)
	case routeAdd, routeDelete:
		verb := map[changeKind]string{routeAdd: "add", routeDelete: "del"}[c.kind]
		args := append([]string{"route", verb}, strings.Fields(c.text)...)
		_, err = k.run(ctx, nil, "ip", append(args, "table", strconv.Itoa(localTable))...)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", c, err)
	}
	return nil
}

var errGateLocked = errors.New("gate lock")

// open runs a slot's opening under the gate lock, from before its confirmation until its link up has
// landed, so close-gates closes the link after it.
func (k *kernel) open(ctx context.Context, slot domain.SlotID, open openFunc) error {
	if open == nil {
		return errors.New("no confirmation to open on")
	}
	lock, err := lockWithin(ctx, k.gateLock, commandTimeout)
	if err != nil {
		return fmt.Errorf("%w: %w", errGateLocked, err)
	}
	defer lock.Close()
	return open(ctx, slot)
}

// closeWait bounds how long close-gates waits for the gate lock: an opener holds it
// for a confirmation, bounded by commandTimeout, and an ioctl. It stays below
// systemd's 90 s stop timeout, which would kill close-gates.
const closeWait = 3 * commandTimeout

// closeAll sets down every pxa link in linkGroup. It reads nothing else and writes
// nothing else, so it is safe outside the Worker. It closes first, with no tool
// and no lock, so nothing wedged can keep a gate up over a dead core. It closes
// again once it has the gate lock, which an opener holds from its confirmation
// until its link up has landed.
func (k *kernel) closeAll(ctx context.Context) error {
	first := k.closeOwned()
	lock, err := lockWithin(ctx, k.gateLock, closeWait)
	if err != nil {
		return errors.Join(first, err)
	}
	defer lock.Close()
	return k.closeOwned()
}

func (k *kernel) closeOwned() error {
	names, err := k.ownedLinks()
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if names[name] {
			errs = append(errs, setFlag(name, false))
		}
	}
	return errors.Join(errs...)
}

// setFlag sets or clears a link's IFF_UP flag with the ioctl `ifconfig up` and `down` use. No tool
// runs, so nothing wedged can keep a gate open and nothing outlives the process that wrote it.
func setFlag(name string, up bool) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var ifreq struct {
		name  [syscall.IFNAMSIZ]byte
		flags int16
		_     [22]byte
	}
	copy(ifreq.name[:], name)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifreq))); errno != 0 {
		return fmt.Errorf("link %s: read flags: %w", name, errno)
	}
	verb := "down"
	ifreq.flags &^= syscall.IFF_UP
	if up {
		verb = "up"
		ifreq.flags |= syscall.IFF_UP
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifreq))); errno != 0 {
		return fmt.Errorf("link %s: set %s: %w", name, verb, errno)
	}
	return nil
}
