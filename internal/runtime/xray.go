package runtime

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"sidegate/internal/domain"
)

// Capabilities are certified against the running core's API, never assumed from
// its version string. A missing capability rejects the intent that needs it.
type Capabilities struct {
	Protocols        []domain.Protocol
	HotOutbounds     bool
	HotInbounds      bool
	ProbeUserRouting bool
	OverrideReadback bool
}

type coreSlot struct {
	ID        domain.SlotID
	Port      uint16
	Mark      uint32
	Selection domain.Selection
}

type corePlan struct {
	Servers  map[domain.ServerRef]domain.Server
	Slots    []coreSlot                         // Serving slots only.
	Probes   []domain.ServerRef                 // Installed and published in Topology.
	Draining []domain.ServerRef                 // Installed but withdrawn from Topology until the drain ends.
	Waiting  map[domain.ServerRef]domain.Server // No verdict yet: no route, but a live outbound stays.
}

func (p corePlan) empty() bool {
	return len(p.Slots) == 0 && len(p.Probes) == 0 && len(p.Draining) == 0 && len(p.Waiting) == 0
}

// coreObservation is read back from the unit and the API; nothing in it is
// copied from a plan.
type coreObservation struct {
	readErr      error // Why the read stopped short; then nothing below proves anything.
	Running      bool
	Epoch        domain.CoreEpoch
	PID          int
	Capabilities Capabilities
	Applied      map[domain.SlotID]domain.Selection // Slots whose readback serves exactly the planned selection.
	Serving      map[domain.SlotID][]string         // Live outbounds the router picks now for a slot's new TCP and UDP connections.
	Probes       []domain.ServerRef                 // Probe routes whose user, rule, and outbound all exist.
	Outdated     bool
	Versions     CoreVersions

	state     unitState
	inbounds  []string
	outbounds []string
	rules     []routeRule
	users     []string
}

// CoreUnitName is the systemd unit CoreUnit renders.
const CoreUnitName = "sidegate-core.service"

// unitState is the core unit's systemd ActiveState. A core runs only while its unit is active with a
// main process.
type unitState string

const (
	unitActive       unitState = "active"
	unitActivating   unitState = "activating" // Systemd's wait before a crash restart reads as this too.
	unitDeactivating unitState = "deactivating"
	unitInactive     unitState = "inactive"
	unitFailed       unitState = "failed"
)

// stopped says no core runs and none is on its way: the unit is inactive, failed or not loaded.
func (s unitState) stopped() bool { return s == unitInactive || s == unitFailed || s == "" }

// changing says systemd is starting or stopping the core, so a start now would race it.
func (s unitState) changing() bool { return s == unitActivating || s == unitDeactivating }

const (
	blockTag   = "block"
	directTag  = "direct"
	probeTag   = "probe"
	apiTimeout = 10 * time.Second
)

var (
	apiAddr   = netip.MustParseAddrPort("127.0.0.1:10085")
	probeAddr = netip.MustParseAddrPort("127.0.0.1:10086")
)

// xray drives the pinned Xray build running in its own systemd unit. The daemon
// is not its parent: a daemon restart or upgrade leaves it serving, and the next
// reconcile adopts it through its API. Every tag, user, and rule tag is derived
// from a ServerRef or SlotID, so readback alone says what is applied and adoption
// needs no bookkeeping that could be lost.
type xray struct {
	tools
	binary     string // DataDir/current/xray, the release's pinned core.
	configPath string // RunDir/xray.json, rewritten after every applied change.
	proc       string // Where a running core's binary is found: /proc, so a core whose file is gone still answers.
	api        coreAPI
	judged     atomic.Pointer[verdicts]
	learned    func() // Wakes a pass once a version read lands.

	mu       sync.Mutex
	versions map[string]string // The one read of `xray version`, by pinned path or by core epoch; empty while it runs.
	reads    sync.WaitGroup    // The reads out, which hold the runtime lock, so Run waits for them before it lets go.
}

// CoreVersions names the release's pinned core and the core running, each as `xray version`
// prints it. Running is empty while no core runs or its version isn't read yet, and Release while
// the release's isn't.
type CoreVersions struct{ Release, Running string }

// verdicts is what one binary said about the servers it was asked about, immutable once
// published, so a pass reads it without waiting for a job that is still running.
type verdicts struct {
	binary string
	of     map[domain.ServerRef]verdict // A server it hasn't judged has no entry, so it reads as pending.
	err    error                        // Why the last job stopped short of judging everything.
}

// verdict is the pinned core's word on one server. The zero verdict is pending.
type verdict struct {
	state  verdictState
	reason string // Why it refused the server.
}

type verdictState uint8

const (
	verdictPending  verdictState = iota // Not judged: the server gets no route, but a live outbound stays.
	verdictAccepted                     // The pinned core loads it.
	verdictRefused                      // The pinned core won't load it, or the renderer can't express it.
)

func newXray(options Options) *xray {
	return &xray{
		binary:     filepath.Join(options.DataDir, "current", "xray"),
		configPath: filepath.Join(options.RunDir, "xray.json"),
		proc:       "/proc",
		api:        newCoreAPI(apiAddr),
		learned:    func() {},
		versions:   map[string]string{},
	}
}

// version is what binary has said its version is, empty while unread. It is the only place a
// version is read, and it reads each key once: a binary never changes under its pinned path, and a
// running core never changes within its epoch. The first call for a key starts its read off the
// caller's path, so no caller waits on a binary and none reads a key a second time. A read that
// fails, since the binary is gone, timed out or said something else, leaves the key unread, so a
// later call starts it again. One that answers wakes a pass.
func (x *xray) version(ctx context.Context, key, binary string) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if said, ok := x.versions[key]; ok {
		return said
	}
	x.versions[key] = ""
	x.reads.Go(func() {
		said := x.readVersion(ctx, binary)
		x.mu.Lock()
		if said == "" {
			delete(x.versions, key)
		} else {
			x.versions[key] = said
		}
		x.mu.Unlock()
		if said != "" {
			x.learned()
		}
	})
	return ""
}

func (x *xray) readVersion(ctx context.Context, binary string) string {
	out, err := x.run(ctx, nil, binary, "version")
	if err != nil {
		return ""
	}
	l := lines(out)
	if len(l) == 0 {
		return ""
	}
	f := strings.Fields(l[0])
	if len(f) < 2 || f[0] != "Xray" {
		return ""
	}
	return f[1]
}

// Tags. Outbound roots are "o.", dependency hops "h.", so a balancer selector,
// which Xray matches by prefix, never selects a hop. The digest suffix has a
// fixed length, so no root tag is a prefix of another.

func refTag(ref domain.ServerRef) string {
	return string(ref.Lineage) + "." + hex.EncodeToString(ref.Digest[:8])
}
func outboundTag(ref domain.ServerRef) string { return "o." + refTag(ref) }
func probeEmail(ref domain.ServerRef) string  { return "p." + refTag(ref) }

// slotNetworks are a slot's inbounds, one per network on its one port, so a move can replace the UDP
// worker alone. code is the network as TestRoute takes it.
var slotNetworks = []struct {
	name string
	code uint64
}{{"tcp", networkTCP}, {"udp", networkUDP}}

func inboundTag(slot domain.SlotID, network string) string {
	return "s." + string(slot) + "." + network
}

// inboundSlot is the slot an inbound tag s.<slot> or s.<slot>.<network> belongs to. A slot ID has no dot.
func inboundSlot(tag string) (domain.SlotID, bool) {
	rest, ok := strings.CutPrefix(tag, "s.")
	id, _, _ := strings.Cut(rest, ".")
	return domain.SlotID(id), ok
}

func inboundTags(slot domain.SlotID) []string {
	var tags []string
	for _, n := range slotNetworks {
		tags = append(tags, inboundTag(slot, n.name))
	}
	return tags
}

// balancerTag names a slot's balancer and the rule that feeds it. The tag lasts the
// slot's life, and the slot moves by override.
func balancerTag(s coreSlot) string { return "b." + string(s.ID) }

func servedTag(sel domain.Selection) string {
	switch {
	case sel.IsDirect():
		return directTag
	case sel.IsBlocked():
		return blockTag
	}
	server, _ := sel.Server()
	return outboundTag(server)
}

// probeUser derives the VLESS user ID of ref's probe route (RFC 4122 variant,
// version 5 layout over a SHA-256 prefix).
func probeUser(ref domain.ServerRef) [16]byte {
	sum := sha256.Sum256(append([]byte(ref.Lineage+"/"), ref.Digest[:]...))
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = id[6]&0x0f | 0x50
	id[8] = id[8]&0x3f | 0x80
	return id
}

func uuidString(id [16]byte) string {
	h := hex.EncodeToString(id[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// installed lists every server the core must hold an outbound for.
func (p corePlan) installed() []domain.ServerRef {
	set := map[domain.ServerRef]bool{}
	for _, s := range p.Slots {
		if server, ok := s.Selection.Server(); ok {
			set[server] = true
		}
	}
	for _, r := range slices.Concat(p.Probes, p.Draining) {
		set[r] = true
	}
	return slices.SortedFunc(maps.Keys(set), func(a, b domain.ServerRef) int { return cmp.Compare(outboundTag(a), outboundTag(b)) })
}

func (p corePlan) probeUsers() []domain.ServerRef {
	return slices.SortedFunc(slices.Values(slices.Concat(p.Probes, p.Draining)), func(a, b domain.ServerRef) int {
		return cmp.Compare(probeEmail(a), probeEmail(b))
	})
}

type obj = map[string]any

// render is the only place that knows Xray's JSON. Its pieces are also the
// payloads of hot adds, so a restart from the file and a hot apply agree.
//
//	log:        warnings to the journal, access off
//	api:        HandlerService, RoutingService, StatsService on apiAddr
//	stats:      per-outbound uplink/downlink; no observatory
//	inbounds:   per slot, two tunnel (dokodemo) inbounds on 127.0.0.1:Port, one tcp and one udp, each
//	            with tproxy sockopt and the slot's mark, followRedirect, sniffing off (UniFi already routed);
//	            one VLESS probe inbound on probeAddr, one user per probe root
//	outbounds:  "block" first, so it is the default for any routing miss and an
//	            override naming a missing tag fails closed; "direct", the WAN; then
//	            "o.<ref>" per root and "h.<ref>.<hop>" per dependency hop
//	routing:    inbound slot -> balancer slot; probe user -> its root
//	balancers:  random over exactly the selected root or direct, also pinned by override.
//	            No fallbackTag: with random it needs an observatory.
func render(want corePlan) ([]byte, error) {
	outbounds, err := renderOutbounds(want, want.installed())
	if err != nil {
		return nil, err
	}
	config := obj{
		"log": obj{"loglevel": "warning", "access": "none"},
		"api": obj{"tag": "api", "listen": apiAddr.String(),
			"services": []string{"HandlerService", "RoutingService", "StatsService"}},
		"stats":     obj{},
		"policy":    obj{"system": obj{"statsOutboundUplink": true, "statsOutboundDownlink": true}},
		"inbounds":  renderInbounds(want),
		"outbounds": append([]obj{{"tag": blockTag, "protocol": "blackhole"}, directOutbound()}, outbounds...),
		"routing":   renderRouting(want),
	}
	return json.MarshalIndent(config, "", "  ")
}

// directOutbound is where a slot on Direct leaves. It needs no mark: UniFi's route marks are set on
// forwarded traffic in PREROUTING, and a socket of the core's own goes out through OUTPUT and the
// main table, as its connections to servers do. That same path skips the firewall forwarded traffic
// meets, and the core's freedom has no default block for a tunnel inbound, so these rules keep a
// client from reaching the gateway and its other networks by way of a slot (lab step direct).
func directOutbound() obj {
	return obj{"tag": directTag, "protocol": "freedom", "settings": obj{"domainStrategy": "AsIs", "finalRules": []obj{
		{"action": "block", "ip": []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7", "fe80::/10"}},
	}}}
}

// slotInbound is s's inbound for one network. Both carry the mark: accepted TCP sockets inherit it,
// which is how a close finds them, and UDP replies are sent with it.
func slotInbound(s coreSlot, network string) obj {
	return obj{
		"tag": inboundTag(s.ID, network), "listen": "127.0.0.1", "port": s.Port, "protocol": "tunnel",
		"settings":       obj{"network": network, "followRedirect": true},
		"streamSettings": obj{"sockopt": obj{"tproxy": "tproxy", "mark": s.Mark}},
	}
}

func renderInbounds(want corePlan) []obj {
	var inbounds []obj
	for _, s := range want.Slots {
		for _, n := range slotNetworks {
			inbounds = append(inbounds, slotInbound(s, n.name))
		}
	}
	clients := []obj{}
	for _, ref := range want.probeUsers() {
		clients = append(clients, obj{"id": uuidString(probeUser(ref)), "email": probeEmail(ref)})
	}
	return append(inbounds, obj{
		"tag": probeTag, "listen": probeAddr.Addr().String(), "port": probeAddr.Port(), "protocol": "vless",
		"settings": obj{"decryption": "none", "clients": clients},
	})
}

func renderRouting(want corePlan) obj {
	rules, balancers := []obj{}, []obj{}
	// Slot rules match disjoint inbounds and probe rules disjoint users, so their
	// order routes nothing differently. Sorting makes it one payload for one routing.
	for _, s := range slices.SortedFunc(slices.Values(want.Slots), func(a, b coreSlot) int { return cmp.Compare(a.ID, b.ID) }) {
		tag := balancerTag(s)
		rules = append(rules, obj{"ruleTag": tag, "inboundTag": inboundTags(s.ID), "balancerTag": tag})
		balancers = append(balancers, obj{"tag": tag, "selector": []string{servedTag(s.Selection)}, "strategy": obj{"type": "random"}})
	}
	for _, ref := range want.probeUsers() {
		rules = append(rules, obj{"ruleTag": probeEmail(ref), "inboundTag": []string{probeTag},
			"user": []string{probeEmail(ref)}, "outboundTag": outboundTag(ref)})
	}
	return obj{"domainStrategy": "AsIs", "rules": rules, "balancers": balancers}
}

// routingKey names what a rewrite of want would write for its slots. Probe rules are left out: each is
// added and removed by itself, so a probe route coming or going owes no slot a rewrite, and a rewrite,
// which resets every slot's override, never runs for one.
func routingKey(want corePlan) string {
	payload, err := json.Marshal(renderRouting(corePlan{Slots: want.Slots}))
	if err != nil {
		panic(err) // renderRouting builds only strings, slices and maps.
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// rules is what ListRule reads back once renderRouting is applied.
func (p corePlan) rules() []routeRule {
	var out []routeRule
	for _, r := range renderRouting(p)["rules"].([]obj) {
		outbound, _ := r["outboundTag"].(string)
		out = append(out, routeRule{RuleTag: r["ruleTag"].(string), Outbound: outbound})
	}
	return out
}

func isProbeRule(tag string) bool { return strings.HasPrefix(tag, "p.") }

// slotRules is every rule but the probe rules, which is what a whole rewrite owns.
func slotRules(rules []routeRule) []routeRule {
	return slices.DeleteFunc(slices.Clone(rules), func(r routeRule) bool { return isProbeRule(r.RuleTag) })
}

// renderOutbounds renders refs' closures. Shared candidates appear once.
func hopTag(ref domain.ServerRef, root, id domain.HopID) string {
	if id == root {
		return outboundTag(ref)
	}
	return "h." + refTag(ref) + "." + string(id)
}

func renderOutbounds(want corePlan, refs []domain.ServerRef) ([]obj, error) {
	var out []obj
	for _, ref := range refs {
		server, ok := want.Servers[ref]
		if !ok {
			return nil, fmt.Errorf("server %s is not in the plan", refTag(ref))
		}
		root, hops := server.Closure()
		for _, h := range hops {
			o, err := renderHop(hopTag(ref, root, h.ID), h)
			if err != nil {
				return nil, fmt.Errorf("server %s hop %s: %w", refTag(ref), h.ID, err)
			}
			if h.DialVia != "" {
				o["streamSettings"].(obj)["sockopt"] = obj{"dialerProxy": hopTag(ref, root, h.DialVia)}
			}
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b obj) int { return cmp.Compare(a["tag"].(string), b["tag"].(string)) })
	return out, nil
}

var renderedProtocols = []domain.Protocol{domain.VLESS, domain.VMess, domain.Trojan, domain.Shadowsocks, domain.SOCKS, domain.HTTPConnect, domain.Hysteria2}

func renderHop(tag string, h domain.Hop) (obj, error) {
	p, credential, username, cipher, flow := h.Protocol.Fields()
	server := obj{"address": h.Endpoint.Host, "port": h.Endpoint.Port}
	var protocol string
	switch p {
	case domain.VLESS:
		user := obj{"id": credential.Reveal(), "encryption": cmp.Or(h.Protocol.VLESSEncryption().Reveal(), "none")}
		if flow != "" {
			user["flow"] = flow
		}
		protocol, server["users"] = "vless", []obj{user}
	case domain.VMess:
		protocol, server["users"] = "vmess", []obj{{"id": credential.Reveal(), "security": cmp.Or(cipher, "auto")}}
	case domain.Trojan:
		protocol, server["password"] = "trojan", credential.Reveal()
	case domain.Shadowsocks:
		protocol, server["method"], server["password"] = "shadowsocks", cipher, credential.Reveal()
	case domain.SOCKS, domain.HTTPConnect:
		protocol = map[domain.Protocol]string{domain.SOCKS: "socks", domain.HTTPConnect: "http"}[p]
		if username != "" {
			server["users"] = []obj{{"user": username, "pass": credential.Reveal()}}
		}
	case domain.Hysteria2: // The credential travels in the stream's hysteriaSettings.
		protocol = "hysteria"
	default:
		return nil, fmt.Errorf("protocol %d has no Xray renderer", p)
	}
	settings := obj{"servers": []obj{server}}
	switch p {
	case domain.VLESS, domain.VMess:
		settings = obj{"vnext": []obj{server}}
	case domain.Hysteria2:
		settings = obj{"version": 2, "address": h.Endpoint.Host, "port": h.Endpoint.Port}
	}
	stream, err := renderStream(h)
	if err != nil {
		return nil, err
	}
	return obj{"tag": tag, "protocol": protocol, "settings": settings, "streamSettings": stream}, nil
}

const salamanderMinPassword = 4

// renderStream renders a hop's transport and TLS. Every field it reads it clears
// from a copy, so a field it does not express refuses the server instead of being
// dropped: a server rendered without one is a different server.
func renderStream(h domain.Hop) (obj, error) {
	t, left := h.Transport, h.Transport
	left.Kind = 0
	stream, mask := obj{}, obj{}
	if t.Kind != domain.Hysteria && len(t.TCPMasks) > 0 {
		mask["tcp"] = renderTCPMasks(t.TCPMasks)
		left.TCPMasks = nil
	}
	switch t.Kind {
	case domain.TCP:
		stream["network"] = "raw"
	case domain.WebSocket:
		ws := obj{"path": t.Path}
		if t.Host != "" {
			ws["host"] = t.Host
		}
		if len(t.Headers) > 0 {
			ws["headers"] = t.Headers
		}
		stream["network"], stream["wsSettings"] = "ws", ws
		left.Path, left.Host, left.Headers = "", "", nil
	case domain.GRPC:
		g := obj{"serviceName": t.Service}
		if t.Host != "" {
			g["authority"] = t.Host
		}
		if t.Mode == "multi" {
			g["multiMode"] = true
			left.Mode = ""
		}
		stream["network"], stream["grpcSettings"] = "grpc", g
		left.Service, left.Host = "", ""
	case domain.XHTTP:
		x := obj{"path": t.Path}
		if t.Host != "" {
			x["host"] = t.Host
		}
		if t.Mode != "" {
			x["mode"] = t.Mode
		}
		if len(t.Headers) > 0 {
			x["headers"] = t.Headers
		}
		stream["network"], stream["xhttpSettings"] = "xhttp", x
		left.Path, left.Host, left.Mode, left.Headers = "", "", "", nil
	case domain.Hysteria:
		_, auth, _, _, _ := h.Protocol.Fields()
		stream["network"], stream["hysteriaSettings"] = "hysteria", obj{"version": 2, "auth": auth.Reveal()}
		if !t.Salamander.IsZero() {
			// The core takes a shorter one in its config test and fails on every dial
			// (transport/internet/finalmask/salamander).
			if len(t.Salamander.Reveal()) < salamanderMinPassword {
				return nil, fmt.Errorf("a Salamander password needs %d bytes", salamanderMinPassword)
			}
			mask["udp"] = []obj{{"type": "salamander", "settings": obj{"password": t.Salamander.Reveal()}}}
		}
		if t.QuicParams != nil {
			mask["quicParams"] = renderQuicParams(*t.QuicParams)
		}
		left.Salamander, left.QuicParams = domain.Secret{}, nil
	default:
		return nil, fmt.Errorf("transport %d has no Xray renderer", t.Kind)
	}
	if len(mask) > 0 {
		stream["finalmask"] = mask
	}
	if names := setFields(left); len(names) > 0 {
		return nil, fmt.Errorf("transport %d cannot express %s", t.Kind, strings.Join(names, ", "))
	}
	tls, leftTLS := h.TLS, h.TLS
	switch {
	case tls.Reality != nil:
		r := obj{"serverName": tls.ServerName, "fingerprint": cmp.Or(tls.Fingerprint, "chrome"),
			"publicKey": tls.Reality.PublicKey, "shortId": tls.Reality.ShortID}
		if tls.Reality.SpiderX != "" {
			r["spiderX"] = tls.Reality.SpiderX
		}
		if tls.Reality.MLDSA65Verify != "" {
			r["mldsa65Verify"] = tls.Reality.MLDSA65Verify
		}
		stream["security"], stream["realitySettings"] = "reality", r
		leftTLS.Enabled, leftTLS.ServerName, leftTLS.Fingerprint, leftTLS.Reality = false, "", "", nil
	case tls.Enabled:
		s := obj{"serverName": tls.ServerName}
		alpn := tls.ALPN
		if t.Kind == domain.Hysteria {
			// With no ALPN Xray's client offers h2 and http/1.1, and a Hysteria
			// server takes only h3 (transport/internet/hysteria/hub.go).
			if len(alpn) > 0 && !slices.Contains(alpn, "h3") {
				return nil, fmt.Errorf("Hysteria speaks only h3, and this server offers %v", alpn)
			}
			alpn = []string{"h3"}
		}
		if len(alpn) > 0 {
			s["alpn"] = alpn
		}
		if tls.Fingerprint != "" {
			s["fingerprint"] = tls.Fingerprint
		}
		if len(tls.PinnedCertSHA256) > 0 {
			s["pinnedPeerCertSha256"] = strings.Join(tls.PinnedCertSHA256, ",")
		}
		if len(tls.VerifyNames) > 0 {
			s["verifyPeerCertByName"] = strings.Join(tls.VerifyNames, ",")
		}
		if tls.ECHConfigList != "" {
			s["echConfigList"] = tls.ECHConfigList
		}
		stream["security"], stream["tlsSettings"] = "tls", s
		leftTLS = domain.TLS{}
	default:
		stream["security"] = "none"
	}
	if names := setFields(leftTLS); len(names) > 0 {
		return nil, fmt.Errorf("this TLS mode cannot express %s", strings.Join(names, ", "))
	}
	return stream, nil
}

func renderTCPMasks(masks []domain.TCPMask) []obj {
	var out []obj
	for _, m := range masks {
		settings := obj{}
		if f := m.Fragment; f != nil {
			settings = nonZero(obj{"packets": f.Packets, "length": f.Length, "delay": f.Delay, "lengths": f.Lengths, "delays": f.Delays, "maxSplit": f.MaxSplit})
		}
		if c := m.HeaderCustom; c != nil {
			settings = obj{"clients": renderSequences(c.Clients), "servers": renderSequences(c.Servers), "errors": renderSequences(c.Errors)}
		}
		out = append(out, obj{"type": m.Type, "settings": settings})
	}
	return out
}

func renderSequences(sequences [][]domain.CustomTCPItem) [][]obj {
	out := make([][]obj, 0, len(sequences))
	for _, items := range sequences {
		sequence := make([]obj, 0, len(items))
		for _, i := range items {
			o := nonZero(obj{"delay": i.Delay, "rand": i.Rand, "randRange": i.RandRange, "capture": i.Capture, "reuse": i.Reuse})
			if len(i.Packet) > 0 {
				o["type"], o["packet"] = "base64", base64.StdEncoding.EncodeToString(i.Packet)
			}
			sequence = append(sequence, o)
		}
		out = append(out, sequence)
	}
	return out
}

func renderQuicParams(q domain.QuicParams) obj {
	return nonZero(obj{
		"congestion": q.Congestion, "bbrProfile": q.BBRProfile, "brutalUp": q.BrutalUp, "brutalDown": q.BrutalDown,
		"brutalDisableLossCompensation": q.BrutalDisableLossCompensation,
		"initStreamReceiveWindow":       q.InitStreamReceiveWindow,
		"maxStreamReceiveWindow":        q.MaxStreamReceiveWindow,
		"initConnectionReceiveWindow":   q.InitConnectionReceiveWindow,
		"maxConnectionReceiveWindow":    q.MaxConnectionReceiveWindow,
		"maxIdleTimeout":                q.MaxIdleTimeout,
		"keepAlivePeriod":               q.KeepAlivePeriod,
		"disablePathMTUDiscovery":       q.DisablePathMTUDiscovery,
		"disableChromeParrot":           q.DisableChromeParrot,
		"disableGSO":                    q.DisableGSO,
		"disableStatelessReset":         q.DisableStatelessReset,
		"maxIncomingStreams":            q.MaxIncomingStreams,
	})
}

// nonZero is o without its zero values, which the core reads as its own defaults.
func nonZero(o obj) obj {
	maps.DeleteFunc(o, func(_ string, v any) bool { return reflect.ValueOf(v).IsZero() })
	return o
}

// setFields names the fields of struct v that are not zero. It reads names only, so
// no credential reaches an error.
func setFields(v any) []string {
	var names []string
	rv := reflect.ValueOf(v)
	for i := range rv.NumField() {
		if !rv.Field(i).IsZero() {
			names = append(names, rv.Type().Field(i).Name)
		}
	}
	return names
}

// coreRefusedConfig is the exit status of `xray run` on a config it will not load
// (main/run.go). Any other failure says nothing about the config.
const coreRefusedConfig = 23

// current reads the verdicts of the pinned binary. A readable binary that changed starts over. A
// missing or unreadable one has changed nothing, since the running core still serves what it loaded,
// and it judges nothing new until it comes back. A book kept from a core with no binary to compare it
// with takes the first binary that appears.
func (x *xray) current() verdicts {
	pinned, readable := x.pinned()
	v := x.judged.Load()
	switch {
	case v == nil:
		return verdicts{binary: pinned, of: map[domain.ServerRef]verdict{}}
	case !readable || v.binary == pinned:
		return *v
	case v.binary == "":
		book := *v
		book.binary = pinned
		return book
	}
	return verdicts{binary: pinned, of: map[domain.ServerRef]verdict{}}
}

// loaded is this book with refs accepted. The published book stays as it was.
func (v verdicts) loaded(refs map[domain.ServerRef]domain.Server) verdicts {
	v.of = maps.Clone(v.of)
	for ref := range refs {
		v.of[ref] = verdict{state: verdictAccepted}
	}
	return v
}

// pinned resolves the binary path, and says whether a file can be read there.
func (x *xray) pinned() (string, bool) {
	path, err := filepath.EvalSymlinks(x.binary)
	if err != nil {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	return path, err == nil && info.Mode().IsRegular()
}

// judge decides which of servers the core cannot be given, each with why, and reports whether it
// learned anything. One such server in a config stops the whole core from starting, and one in a
// hot add stops the batch, so each is refused alone before anything is rendered for the core.
// A ServerRef never changes, so the pinned binary judges each one once. Servers it has not
// judged because it could not run stay unjudged, and the published error says so.
func (x *xray) judge(ctx context.Context, servers map[domain.ServerRef]domain.Server) (bool, error) {
	book := x.current()
	of := maps.Clone(book.of)
	want := corePlan{Servers: servers}
	var fresh []domain.ServerRef
	for _, ref := range slices.SortedFunc(maps.Keys(servers), func(a, b domain.ServerRef) int { return cmp.Compare(outboundTag(a), outboundTag(b)) }) {
		if of[ref].state != verdictPending {
			continue
		}
		if _, err := renderOutbounds(want, []domain.ServerRef{ref}); err != nil {
			of[ref] = verdict{state: verdictRefused, reason: err.Error()}
			continue
		}
		fresh = append(fresh, ref)
	}
	_, _, err := x.judgeBatch(ctx, want, fresh, of)
	x.judged.Store(&verdicts{binary: book.binary, of: of, err: err})
	return len(of) > len(book.of), err
}

// judgeBatch tests refs together, since nearly every plan loads whole, and one at a
// time only when that fails.
func (x *xray) judgeBatch(ctx context.Context, want corePlan, refs []domain.ServerRef, of map[domain.ServerRef]verdict) (bool, string, error) {
	if len(refs) == 0 {
		return true, "", nil
	}
	loads, reason, err := x.coreLoads(ctx, want, refs)
	if err != nil {
		return false, "", err
	}
	switch {
	case loads:
		for _, ref := range refs {
			of[ref] = verdict{state: verdictAccepted}
		}
	case len(refs) == 1:
		of[refs[0]] = verdict{state: verdictRefused, reason: redactCredentials(reason, want.Servers[refs[0]])}
	default:
		for _, ref := range refs {
			if _, _, err := x.judgeBatch(ctx, want, []domain.ServerRef{ref}, of); err != nil {
				return false, "", err
			}
		}
	}
	return loads, reason, nil
}

// coreLoads runs the pinned binary's config test on refs' outbounds. They go over
// stdin, so no credential reaches a file or argv.
func (x *xray) coreLoads(ctx context.Context, want corePlan, refs []domain.ServerRef) (bool, string, error) {
	outbounds, err := renderOutbounds(want, refs)
	if err != nil {
		return false, "", err
	}
	body, err := json.Marshal(obj{"outbounds": outbounds})
	if err != nil {
		return false, "", err
	}
	out, err := x.run(ctx, body, x.binary, "run", "-test", "-config", "stdin:", "-format", "json")
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, "", nil
	case !errors.As(err, &exit) || exit.ExitCode() != coreRefusedConfig:
		return false, "", fmt.Errorf("could not test servers against the core: %w", err)
	}
	reason := "the core refused it"
	if l := lines(out); len(l) > 0 {
		reason = l[len(l)-1]
	}
	return false, reason, nil
}

// redactCredentials keeps the core's words about a server while dropping what the
// core may have quoted from it: it prints a bad key back.
func redactCredentials(reason string, server domain.Server) string {
	_, hops := server.Closure()
	for _, h := range hops {
		_, credential, _, _, _ := h.Protocol.Fields()
		for _, secret := range []domain.Secret{credential, h.Transport.Salamander, h.Protocol.VLESSEncryption()} {
			if !secret.IsZero() {
				reason = strings.ReplaceAll(reason, secret.Reveal(), secret.String())
			}
		}
	}
	return reason
}

// unitProps reads the core unit's state, main process and start.
func (x *xray) unitProps(ctx context.Context) (map[string]string, error) {
	out, err := x.run(ctx, nil, "systemctl", "show", CoreUnitName, "-p", "ActiveState", "-p", "MainPID", "-p", "InvocationID")
	if err != nil {
		return nil, err
	}
	props := map[string]string{}
	for _, l := range lines(out) {
		k, v, _ := strings.Cut(l, "=")
		props[k] = v
	}
	return props, nil
}

// running is the start a unit's properties name, "" when no core runs.
func running(props map[string]string) domain.CoreEpoch {
	if unitState(props["ActiveState"]) != unitActive || props["MainPID"] == "0" || props["MainPID"] == "" {
		return ""
	}
	return domain.CoreEpoch(props["InvocationID"])
}

// unit reads the core's unit, and what the reads of the pinned and the running core's versions have
// said, starting those not yet read. It never waits for one.
func (x *xray) unit(ctx context.Context) (coreObservation, error) {
	obs := coreObservation{Applied: map[domain.SlotID]domain.Selection{}, Serving: map[domain.SlotID][]string{}}
	props, err := x.unitProps(ctx)
	if err != nil {
		return obs, err
	}
	obs.state = unitState(props["ActiveState"])
	obs.PID, _ = strconv.Atoi(props["MainPID"])
	pinned, readable := x.pinned()
	if readable {
		obs.Versions.Release = x.version(ctx, pinned, pinned)
	}
	if obs.state != unitActive || obs.PID == 0 {
		return obs, nil
	}
	obs.Running, obs.Epoch = true, domain.CoreEpoch(props["InvocationID"])
	exe := filepath.Join(x.proc, strconv.Itoa(obs.PID), "exe")
	if target, err := os.Readlink(exe); err == nil {
		obs.Outdated = readable && target != pinned
	}
	if obs.Epoch != "" {
		obs.Versions.Running = x.version(ctx, "epoch "+string(obs.Epoch), exe)
	}
	return obs, nil
}

// listening says the core holds exactly slot's inbounds. Any other inbound on the slot, such as the
// one an older build gave both networks, shares its port through SO_REUSEPORT, and a connection it
// accepts matches no rule and goes to block.
func (o coreObservation) listening(slot domain.SlotID) bool {
	own := inboundTags(slot)
	for _, t := range o.inbounds {
		if id, ok := inboundSlot(t); ok && id == slot && !slices.Contains(own, t) {
			return false
		}
	}
	for _, t := range own {
		if !slices.Contains(o.inbounds, t) {
			return false
		}
	}
	return true
}

const (
	// routingStep is the step a landed routing rewrite reports.
	routingStep = "core replace routing"
	// coreStartStep is the step a core start reports.
	coreStartStep = "core start"
)

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".xray-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// telemetry reads cumulative outbound counters without mutating the core. It
// stamps every sample with the epoch and generation of topology(), skips a read
// whose epoch changed underneath it, and maps a counter to a server only when its
// tag belongs to a server in the current topology.
func (x *xray) telemetry(ctx context.Context, interval time.Duration, topology func() Topology, out chan<- Telemetry) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var sequence uint64
	gap := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		t := topology()
		if t.Epoch == "" {
			continue
		}
		stats, err := x.api.queryStats(ctx, "outbound>>>o.")
		if err != nil || topology().Epoch != t.Epoch {
			gap = true
			continue
		}
		sequence++
		sample := Telemetry{Gap: gap}
		at := time.Now()
		for _, ref := range t.servers() {
			up, okUp := stats["outbound>>>"+outboundTag(ref)+">>>traffic>>>uplink"]
			down, okDown := stats["outbound>>>"+outboundTag(ref)+">>>traffic>>>downlink"]
			if !okUp && !okDown {
				continue
			}
			u, d := uint64(up), uint64(down)
			sample.Signals = append(sample.Signals, domain.Signal{
				Stamp:  domain.Stamp{Epoch: t.Epoch, Generation: t.Generation, Sequence: sequence, At: at},
				Server: ref, Kind: domain.OutboundCounters, Up: &u, Down: &d, AttributionCertain: true,
			})
		}
		select {
		case out <- sample:
			gap = false
		default:
			gap = true
		}
	}
}

// CoreUnit returns the systemd unit that runs the core. The runtime owns its text,
// so lifecycle installs exactly what reconcile expects. The unit is installed but
// not enabled: the daemon starts it when the first slot or probe needs it. Its
// stop hook closes our gates whether or not the daemon is running.
func CoreUnit(options Options) []byte {
	current := filepath.Join(options.DataDir, "current")
	return fmt.Appendf(nil, `[Unit]
Description=sidegate proxy core (Xray)

[Service]
Type=simple
ExecStart=:%s run -c %s
ExecStopPost=:%s close-gates --run-dir %s
Restart=on-failure
RestartSec=2s
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
StandardOutput=append:%s
StandardError=inherit
`, UnitWord(filepath.Join(current, "xray")), UnitWord(filepath.Join(options.RunDir, "xray.json")),
		UnitWord(filepath.Join(current, "sidegated")), UnitWord(options.RunDir), UnitText(CoreLogPath(options.RunDir)))
}

// UnitText is a path UnitCarries as all or the end of a unit's value, and UnitWord as one word of a command.
// The command's executable must have the prefix ":", which stops systemd's own $ expansion.
func UnitText(path string) string { return strings.ReplaceAll(path, "%", "%%") }

func UnitWord(path string) string { return `"` + UnitText(path) + `"` }

// UnitCarries says whether a unit can hold path. systemd runs no executable with a quote, a backslash or a
// control character in its path, however the unit spells it, and skips a line that isn't what it calls
// UTF-8, so no release ever ran from such a data dir.
func UnitCarries(path string) error {
	no := func(r rune) bool {
		return r < ' ' || r == 0x7f || strings.ContainsRune(`"'\`, r) || unicode.Is(unicode.Noncharacter_Code_Point, r)
	}
	if utf8.ValidString(path) && strings.IndexFunc(path, no) < 0 {
		return nil
	}
	return fmt.Errorf("%q can't be in a systemd unit: it has a quote, a backslash, a control character or text that isn't UTF-8", path)
}

// CoreLogPath is where the core's warnings land: the run dir, on tmpfs, so they cost
// the flash nothing and the daemon can tail them without a journal reader.
func CoreLogPath(runDir string) string { return filepath.Join(runDir, "core.log") }
