package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// passOn is x as one pass reaches it, with a fresh view.
func passOn(x *xray) *corePass { return &corePass{x: x, view: &view{}, gates: &gates{}} }

func testRef(name string) domain.ServerRef {
	return domain.ServerRef{Lineage: domain.LineageID(name), Digest: sha256.Sum256([]byte(name))}
}

func vlessHop(t *testing.T, id domain.HopID, host string, via domain.HopID) domain.Hop {
	t.Helper()
	p, err := domain.NewProtocol(domain.VLESS, domain.NewSecret("6f1c1e52-0d3c-4b8e-9a51-2c1f7c0a5e11"), "", "", "xtls-rprx-vision")
	if err != nil {
		t.Fatal(err)
	}
	return domain.Hop{ID: id, Endpoint: domain.Endpoint{Host: host, Port: 443}, Protocol: p, DialVia: via,
		Transport: domain.Transport{Kind: domain.TCP},
		TLS:       domain.TLS{Enabled: true, ServerName: host, PinnedCertSHA256: []string{"ab12"}}}
}

func testServer(t *testing.T, hops ...domain.Hop) domain.Server {
	t.Helper()
	s, err := domain.NewServer(hops[0].ID, hops)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func labCorePlan(t *testing.T) corePlan {
	a, b := testRef("srvA"), testRef("srvB")
	return corePlan{
		Servers: map[domain.ServerRef]domain.Server{
			a: testServer(t, vlessHop(t, "root", "198.51.100.10", "")),
			b: testServer(t, vlessHop(t, "root", "198.51.100.20", "")),
		},
		Slots: []coreSlot{
			{ID: "slot1", Port: 12341, Selection: domain.Serving(a)},
			{ID: "slot2", Port: 12342, Selection: domain.Serving(b)},
		},
		Probes: []domain.ServerRef{a, b},
	}
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRenderFailsClosedAndPinsEachSlotToItsSelection(t *testing.T) {
	plan := labCorePlan(t)
	b, err := render(plan)
	if err != nil {
		t.Fatal(err)
	}
	config := decode(t, b)
	outbounds := config["outbounds"].([]any)
	if first := outbounds[0].(map[string]any); first["tag"] != blockTag || first["protocol"] != "blackhole" {
		t.Fatalf("first outbound %v, want block: any routing miss must fail closed", first)
	}
	routing := config["routing"].(map[string]any)
	for _, bal := range routing["balancers"].([]any) {
		bal := bal.(map[string]any)
		if _, ok := bal["fallbackTag"]; ok {
			t.Errorf("balancer %v has a fallbackTag, which needs an observatory", bal["tag"])
		}
		if bal["tag"] == "b.slot1" && !slices.Equal(toStrings(bal["selector"]), []string{outboundTag(testRef("srvA"))}) {
			t.Errorf("slot1 selector %v", bal["selector"])
		}
	}
	srvA, srvB := testRef("srvA"), testRef("srvB")
	want := []routeRule{{RuleTag: "b.slot1"}, {RuleTag: "b.slot2"},
		{RuleTag: probeEmail(srvA), Outbound: outboundTag(srvA)}, {RuleTag: probeEmail(srvB), Outbound: outboundTag(srvB)}}
	if !slices.Equal(plan.rules(), want) {
		t.Errorf("readback compares against %v, want %v", plan.rules(), want)
	}
	if !strings.Contains(string(b), `"pinnedPeerCertSha256": "ab12"`) {
		t.Error("certificate pin not rendered")
	}
	if strings.Contains(string(b), "allowInsecure") {
		t.Error("allowInsecure rendered; Xray v26 removed it")
	}
}

func TestProbeUsersRouteToTheirOwnOutbound(t *testing.T) {
	plan := labCorePlan(t)
	config := decode(t, must(render(plan)))
	var probe map[string]any
	for _, in := range config["inbounds"].([]any) {
		if in.(map[string]any)["tag"] == probeTag {
			probe = in.(map[string]any)
		}
	}
	clients := probe["settings"].(map[string]any)["clients"].([]any)
	if len(clients) != 2 {
		t.Fatalf("probe clients %v", clients)
	}
	for _, c := range clients {
		c := c.(map[string]any)
		ref := testRef(strings.Split(c["email"].(string), ".")[1])
		if c["id"] != uuidString(probeUser(ref)) {
			t.Errorf("user %v has id %v", c["email"], c["id"])
		}
	}
	for _, r := range config["routing"].(map[string]any)["rules"].([]any) {
		r := r.(map[string]any)
		if users, ok := r["user"]; ok && r["outboundTag"] != "o."+strings.TrimPrefix(toStrings(users)[0], "p.") {
			t.Errorf("probe rule %v routes elsewhere", r)
		}
	}
}

func TestAppliedNeedsALiveOverrideTarget(t *testing.T) {
	a := testRef("a")
	sel := domain.Serving(a)
	if (outboundList{blockTag, outboundTag(a)}).rulesOut("slot1", sel) || (balancerAnswer{slot: "slot1", override: outboundTag(a), found: true}).rulesOut("slot1", sel) {
		t.Error("live override not applied")
	}
	if !(outboundList{blockTag}).rulesOut("slot1", sel) {
		t.Error("override to a missing outbound counted as applied; Xray accepts any tag")
	}
	if !(balancerAnswer{slot: "slot1", found: true}).rulesOut("slot1", sel) {
		t.Error("no override counted as applied; after a core restart the gate must wait for it")
	}
}

// The check asks each network on its own inbound, so routing that has lost either tag fails it. The
// fake router matches a request by its inbound tag alone, as Xray's does for the slot's rule, and
// picks nothing for a tag the rule lacks.
func TestTheCheckFailsWhenTheSlotsRuleLacksEitherInbound(t *testing.T) {
	served := outboundTag(testRef("srv"))
	for _, c := range []struct {
		name string
		rule []string
		pass bool
	}{
		{"both inbounds", inboundTags("slot1"), true},
		{"the TCP inbound alone", []string{inboundTag("slot1", "tcp")}, false},
		{"the UDP inbound alone", []string{inboundTag("slot1", "udp")}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := &xray{api: fakeCore(t, func(method string, req []byte) ([]byte, string) {
				for _, tag := range c.rule {
					if method == routingService+"TestRoute" && bytes.Contains(req, []byte(tag)) {
						return pbString(pbString(nil, 1, tag), 12, served), "0"
					}
				}
				return nil, "2"
			})}
			_, live, err := passOn(x).routed(context.Background(), "slot1", []string{blockTag, directTag, served})
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Equal(live, []string{served}); got != c.pass {
				t.Fatalf("routed %v with the rule on %v", live, c.rule)
			}
		})
	}
}

// A slot is applied only while its core holds both of its inbounds. The router never asks whether an
// inbound exists, so a daemon stopped between relisten's remove and its add would leave the slot's
// UDP with no listener while every TestRoute still answered.
func TestASlotMissingEitherInboundReadsBackUnapplied(t *testing.T) {
	srv := testRef("srv")
	cp := corePlan{Servers: map[domain.ServerRef]domain.Server{srv: testServer(t, vlessHop(t, "root", "198.51.100.1", ""))},
		Slots: []coreSlot{{ID: "slot1", Port: 12341, Selection: domain.Serving(srv)}}}
	for _, held := range [][]string{inboundTags("slot1"), {inboundTag("slot1", "tcp")}, {inboundTag("slot1", "udp")}} {
		w, _ := fakeTools(t, activeCore, time.Hour)
		w.core.api = fakeCore(t, func(method string, _ []byte) ([]byte, string) {
			var resp []byte
			switch method {
			case handlerService + "ListInbounds":
				for _, tag := range held {
					resp = pbBytes(resp, 1, pbString(nil, 1, tag))
				}
			case handlerService + "ListOutbounds":
				for _, tag := range []string{blockTag, directTag, outboundTag(srv)} {
					resp = pbBytes(resp, 1, pbString(nil, 1, tag))
				}
			case routingService + "ListRule":
				resp = pbBytes(nil, 1, pbString(nil, 2, "b.slot1"))
			case routingService + "TestRoute":
				resp = pbString(nil, 12, outboundTag(srv))
			case routingService + "GetBalancerInfo":
				resp = pbBytes(nil, 1, pbBytes(nil, 5, pbString(nil, 2, outboundTag(srv))))
			}
			return resp, "0"
		})
		obs, err := passOn(w.core).observe(context.Background(), cp)
		if err != nil {
			t.Fatal(err)
		}
		if _, applied := obs.Applied["slot1"]; applied != (len(held) == 2) {
			t.Errorf("with %v held, applied is %v", held, applied)
		}
	}
}

// A core started by a build that gave a slot one inbound for both networks takes the slot's two
// inbounds, their routing and the old inbound's removal in the pass that finds it. Left in place,
// the old inbound's UDP worker would share the port and keep its sessions on the old server.
func TestACoreWithOneInboundForBothNetworksEndsWithOnePerNetwork(t *testing.T) {
	cp := probeRulePlan(t, "a")
	obs := readbackOf(cp)
	obs.inbounds = []string{"s.slot1", probeTag}
	obs.Applied = map[domain.SlotID]domain.Selection{}
	core := newProbeRuleCore(t)
	if _, err := passOn(core.x).hotApply(context.Background(), cp, obs, true); err != nil {
		t.Fatal(err)
	}
	var added []string
	rewritten := false
	for _, cmd := range core.commands(t) {
		_, payload, _ := strings.Cut(cmd, "stdin: ")
		var body struct {
			Inbounds []struct{ Tag string }
		}
		switch {
		case strings.HasPrefix(cmd, "api adi "):
			if err := json.Unmarshal([]byte(payload), &body); err != nil {
				t.Fatal(err)
			}
			for _, in := range body.Inbounds {
				added = append(added, in.Tag)
			}
		case strings.HasPrefix(cmd, "api adrules --server"):
			rewritten = strings.Contains(payload, `"inboundTag":["s.slot1.tcp","s.slot1.udp"]`)
		}
	}
	if !slices.Equal(added, inboundTags("slot1")) {
		t.Errorf("added inbounds %v, want %v", added, inboundTags("slot1"))
	}
	if !rewritten {
		t.Errorf("routing was not rewritten onto both inbounds: %q", core.commands(t))
	}
	if !slices.Equal(core.removed, []string{"s.slot1"}) {
		t.Errorf("removed inbounds %v, want the old one alone", core.removed)
	}

	before := len(core.commands(t))
	if _, err := passOn(core.x).hotApply(context.Background(), cp, readbackOf(cp), false); err != nil {
		t.Fatal(err)
	}
	if len(core.removed) != 1 || len(core.commands(t)) != before {
		t.Errorf("the next pass changed the core: removed %v, ran %q", core.removed, core.commands(t)[before:])
	}
}

func TestDependencyHopsDialThroughEachOtherAndNeverMatchARootSelector(t *testing.T) {
	ref := testRef("chain")
	plan := corePlan{Servers: map[domain.ServerRef]domain.Server{
		ref: testServer(t, vlessHop(t, "exit", "exit.example", "entry"), vlessHop(t, "entry", "entry.example", "")),
	}, Probes: []domain.ServerRef{ref}}
	outs, err := renderOutbounds(plan, plan.installed())
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 2 {
		t.Fatalf("outbounds %v", outs)
	}
	byTag := map[string]obj{}
	for _, o := range outs {
		byTag[o["tag"].(string)] = o
	}
	root := byTag[outboundTag(ref)]
	hop := "h." + refTag(ref) + ".entry"
	if root == nil || byTag[hop] == nil {
		t.Fatalf("tags %v", slices.Collect(func(yield func(string) bool) {
			for k := range byTag {
				yield(k)
			}
		}))
	}
	if root["streamSettings"].(obj)["sockopt"].(obj)["dialerProxy"] != hop {
		t.Errorf("root does not dial through %s", hop)
	}
	if strings.HasPrefix(hop, outboundTag(ref)) {
		t.Error("hop tag shares the root's prefix, so a balancer selector would pick it")
	}
}

// TestRenderEveryProtocolPassesXray validates the renderer against the pinned
// core when XRAY_BIN names it (the lab runs this inside the VM).
func TestRenderEveryProtocolPassesXray(t *testing.T) {
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN not set")
	}
	servers := map[domain.ServerRef]domain.Server{}
	var probes []domain.ServerRef
	add := func(name string, p domain.Protocol, cred, user, cipher, flow string, tr domain.Transport, tls domain.TLS) {
		spec, err := domain.NewProtocol(p, domain.NewSecret(cred), user, cipher, flow)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ref := testRef(name)
		servers[ref] = testServer(t, domain.Hop{ID: "root", Endpoint: domain.Endpoint{Host: name + ".example", Port: 443},
			Protocol: spec, Transport: tr, TLS: tls})
		probes = append(probes, ref)
	}
	uuid := "6f1c1e52-0d3c-4b8e-9a51-2c1f7c0a5e11"
	pin := strings.Repeat("ab", 32)
	tls := domain.TLS{Enabled: true, ServerName: "x.example", ALPN: []string{"h2"}, Fingerprint: "chrome",
		PinnedCertSHA256: []string{pin, pin}, VerifyNames: []string{"a.example", "b.example"}}
	add("vless-reality", domain.VLESS, uuid, "", "", "xtls-rprx-vision", domain.Transport{Kind: domain.TCP},
		domain.TLS{Enabled: true, ServerName: "x.example", Reality: &domain.Reality{PublicKey: "Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw", ShortID: "6ba85179e30d4fc2"}})
	add("vless-ws", domain.VLESS, uuid, "", "", "", domain.Transport{Kind: domain.WebSocket, Path: "/ws", Host: "x.example"}, tls)
	add("vmess-grpc", domain.VMess, uuid, "", "auto", "", domain.Transport{Kind: domain.GRPC, Service: "svc"}, tls)
	add("vless-xhttp", domain.VLESS, uuid, "", "", "", domain.Transport{Kind: domain.XHTTP, Path: "/x"}, tls)
	add("trojan", domain.Trojan, "secret", "", "", "", domain.Transport{Kind: domain.TCP}, tls)
	add("ss", domain.Shadowsocks, "secret", "", "chacha20-poly1305", "", domain.Transport{Kind: domain.TCP}, domain.TLS{})
	add("socks", domain.SOCKS, "pass", "user", "", "", domain.Transport{Kind: domain.TCP}, domain.TLS{})
	add("http", domain.HTTPConnect, "", "", "", "", domain.Transport{Kind: domain.TCP}, domain.TLS{})
	add("hysteria2", domain.Hysteria2, "secret", "", "", "", domain.Transport{Kind: domain.Hysteria, Salamander: domain.NewSecret("obfs")},
		domain.TLS{Enabled: true, ServerName: "x.example", PinnedCertSHA256: []string{pin}})
	b, err := render(corePlan{Servers: servers, Probes: probes})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "xray.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "run", "-test", "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("xray -test: %v\n%s", err, out)
	}
}

// scriptedCore writes a core binary that records each run and then runs exit.
// It returns a core using it and the record's path.
func scriptedCore(t *testing.T, exit string) (*xray, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho run >>" + calls + "\n" + exit + "\n"
	if err := os.WriteFile(filepath.Join(dir, "xray"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &xray{binary: filepath.Join(dir, "xray")}, calls
}

const refusesBad = `if grep -q bad-secret; then echo "Failed to start: invalid key: bad-secret"; exit 23; fi`

func ssServer(t *testing.T, key string) domain.Server {
	t.Helper()
	p, err := domain.NewProtocol(domain.Shadowsocks, domain.NewSecret(key), "", "2022-blake3-aes-128-gcm", "")
	if err != nil {
		t.Fatal(err)
	}
	return testServer(t, domain.Hop{ID: "root", Endpoint: domain.Endpoint{Host: "x.example", Port: 443}, Protocol: p, Transport: domain.Transport{Kind: domain.TCP}})
}

func TestTheCoreRefusesABadServerAloneAndOnce(t *testing.T) {
	x, calls := scriptedCore(t, refusesBad)
	bad, good := testRef("bad"), testRef("good")
	servers := map[domain.ServerRef]domain.Server{bad: ssServer(t, "bad-secret"), good: ssServer(t, "good-key")}
	learned, err := x.judge(context.Background(), servers)
	if err != nil || !learned {
		t.Fatalf("learned %v, err %v", learned, err)
	}
	book := x.current()
	if book.of[bad].state != verdictRefused || book.of[good].state != verdictAccepted {
		t.Fatalf("verdicts %v", book.of)
	}
	if strings.Contains(book.of[bad].reason, "bad-secret") {
		t.Errorf("the reason quotes the credential: %q", book.of[bad].reason)
	}
	first, _ := os.ReadFile(calls)
	if learned, err := x.judge(context.Background(), servers); err != nil || learned {
		t.Fatalf("judging again: learned %v, err %v", learned, err)
	}
	if again, _ := os.ReadFile(calls); string(again) != string(first) {
		t.Errorf("immutable servers judged again: %d then %d runs", strings.Count(string(first), "run"), strings.Count(string(again), "run"))
	}
}

// The running core still serves what it loaded, so a binary that can't be read takes back no verdict.
// What is new waits, and says why. Only a readable binary at another path starts over.
func TestVerdictsOutliveAMissingBinaryAndNotAChangedOne(t *testing.T) {
	x, _ := scriptedCore(t, "exit 0")
	dir := filepath.Dir(x.binary)
	release := filepath.Join(dir, "release-1")
	if err := os.Rename(x.binary, release); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, x.binary); err != nil {
		t.Fatal(err)
	}
	known, fresh := testRef("known"), testRef("fresh")
	servers := map[domain.ServerRef]domain.Server{known: ssServer(t, "good-key")}
	if _, err := x.judge(context.Background(), servers); err != nil {
		t.Fatal(err)
	}
	repoint := func(target string) {
		t.Helper()
		if err := os.Remove(x.binary); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, x.binary); err != nil {
			t.Fatal(err)
		}
	}
	judged := func(ref domain.ServerRef) bool { return x.current().of[ref].state != verdictPending }

	repoint(filepath.Join(dir, "gone"))
	if !judged(known) {
		t.Fatal("a dangling binary took back a verdict")
	}
	servers[fresh] = ssServer(t, "other-key")
	if _, err := x.judge(context.Background(), servers); err == nil {
		t.Fatal("a server judged with no binary reported no error")
	}
	if judged(fresh) || !judged(known) || x.current().err == nil {
		t.Fatalf("with no binary: %+v", x.current())
	}

	unreadable := filepath.Join(dir, "unreadable")
	if err := os.Mkdir(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}
	repoint(unreadable)
	if !judged(known) {
		t.Fatal("something unreadable at the path took back a verdict")
	}

	repoint(release)
	if !judged(known) {
		t.Fatal("the same binary coming back took back a verdict")
	}

	release2 := filepath.Join(dir, "release-2")
	script, err := os.ReadFile(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release2, script, 0o755); err != nil {
		t.Fatal(err)
	}
	repoint(release2)
	if judged(known) {
		t.Fatal("a changed binary kept the old binary's verdicts")
	}
}

// A daemon started with the binary gone takes its book from the running core. When the binary appears,
// that book stays.
func TestABookTakenWithNoBinaryKeepsItsVerdictsWhenOneAppears(t *testing.T) {
	x, _ := scriptedCore(t, "exit 0")
	script, err := os.ReadFile(x.binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(x.binary); err != nil {
		t.Fatal(err)
	}
	ref := testRef("loaded")
	book := x.current()
	book.of[ref] = verdict{state: verdictAccepted}
	x.judged.Store(&book)
	if err := os.WriteFile(x.binary, script, 0o755); err != nil {
		t.Fatal(err)
	}
	got := x.current()
	if got.of[ref].state != verdictAccepted || got.binary == "" {
		t.Fatalf("after the binary appeared: %+v", got)
	}
}

func TestAServerTheRendererCannotExpressNeverReachesTheCore(t *testing.T) {
	x, calls := scriptedCore(t, "exit 0")
	ref := testRef("odd")
	hop := vlessHop(t, "root", "x.example", "")
	hop.Transport.Path = "/ignored"
	if _, err := x.judge(context.Background(), map[domain.ServerRef]domain.Server{ref: testServer(t, hop)}); err != nil {
		t.Fatal(err)
	}
	if v := x.current().of[ref]; v.state != verdictRefused || !strings.Contains(v.reason, "Path") {
		t.Fatalf("verdict %+v", v)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("the core ran for a server the renderer had refused")
	}
}

func TestACoreThatCannotRunRefusesNothing(t *testing.T) {
	x, _ := scriptedCore(t, "exit 1")
	ref := testRef("ok")
	learned, err := x.judge(context.Background(), map[domain.ServerRef]domain.Server{ref: ssServer(t, "good-key")})
	if err == nil || learned {
		t.Fatalf("learned %v, err %v", learned, err)
	}
	book := x.current()
	if book.of[ref].state != verdictPending {
		t.Error("a server the core never read counts as judged")
	}
	if book.err == nil {
		t.Error("the failure isn't published for the next pass to report")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
