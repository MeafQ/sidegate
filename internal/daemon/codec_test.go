package daemon

import (
	"encoding/base64"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

var noDigest = strings.Repeat("0", 64)

// storedBefore is a lineage's body as the release before the evasion options wrote it.
var storedBefore = `{"label":"DE","naming":{},"identity":{"strong":"` + noDigest + `","endpoint":"` + noDigest + `","host":"` + noDigest + `","tie":"` + noDigest +
	`","credential":"` + noDigest + `","name":"` + noDigest + `"},"ref":{"l":"a","d":"0107000000000000000000000000000000000000000000000000000000000000"},"root":"root",` +
	`"hops":[{"id":"root","host":"h.example","port":443,"protocol":1,"credential":"11111111-2222-4333-8444-555555555555","transport":{"kind":1},` +
	`"tls":{"Enabled":true,"ServerName":"x.example","ALPN":null,"Fingerprint":"chrome","PinnedCertSHA256":null,"VerifyNames":null,` +
	`"Reality":{"PublicKey":"pk","ShortID":"6ba85179e30d4fc2","SpiderX":"/"}},"via":"hy"},` +
	`{"id":"hy","host":"hy.example","port":8443,"protocol":7,"credential":"auth","transport":{"kind":5,"salamander":"obfs"},` +
	`"tls":{"Enabled":true,"ServerName":"hy.example","ALPN":["h3"],"Fingerprint":"","PinnedCertSHA256":null,"VerifyNames":["v.example"],"Reality":null}}],` +
	`"country":{"Code":"DE","Source":1},"presence":1,"missingSince":"0001-01-01T00:00:00Z","graceUntil":"0001-01-01T00:00:00Z"}`

// A server that uses none of the evasion options reads from, and writes to, the same bytes as before.
func TestALineageStoredBeforeTheEvasionOptionsIsUnchanged(t *testing.T) {
	l, err := decodeLineage("a", "s", []byte(storedBefore))
	if err != nil {
		t.Fatal(err)
	}
	_, hops := l.Server.Closure()
	if r := hops[0].TLS.Reality; r == nil || *r != (domain.Reality{PublicKey: "pk", ShortID: "6ba85179e30d4fc2", SpiderX: "/"}) {
		t.Errorf("reality %+v", r)
	}
	if hops[1].Transport.Salamander.Reveal() != "obfs" || !slices.Equal(hops[1].TLS.VerifyNames, []string{"v.example"}) {
		t.Errorf("hysteria hop %+v", hops[1])
	}
	for _, h := range hops {
		if !h.Protocol.VLESSEncryption().IsZero() || h.TLS.ECHConfigList != "" || h.Transport.TCPMasks != nil || h.Transport.QuicParams != nil {
			t.Errorf("hop %s gained an option: %+v", h.ID, h)
		}
	}
	body, err := encodeLineage(l)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != storedBefore {
		t.Errorf("written as\n%s\nwant\n%s", body, storedBefore)
	}
}

func TestALineageKeepsItsEvasionOptions(t *testing.T) {
	vless, err := domain.NewProtocol(domain.VLESS, domain.NewSecret("11111111-2222-4333-8444-555555555555"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if vless, err = vless.WithVLESSEncryption("mlkem768x25519plus.native.0rtt." + base64.RawURLEncoding.EncodeToString(slices.Repeat([]byte{9}, 32))); err != nil {
		t.Fatal(err)
	}
	hysteria, err := domain.NewProtocol(domain.Hysteria2, domain.NewSecret("auth"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	hops := []domain.Hop{
		{ID: "root", Endpoint: domain.Endpoint{Host: "h.example", Port: 443}, Protocol: vless, DialVia: "tls",
			Transport: domain.Transport{Kind: domain.TCP, TCPMasks: []domain.TCPMask{
				{Type: domain.MaskFragment, Fragment: &domain.FragmentMask{Packets: "tlshello", Length: "5", Delay: "1-2", Lengths: []string{"0", "5"}, Delays: []string{"0", "6"}, MaxSplit: "3"}},
				{Type: domain.MaskHeaderCustom, HeaderCustom: &domain.HeaderCustomTCP{
					Clients: [][]domain.CustomTCPItem{{}, {{Packet: []byte{0, 255}, Delay: "1", Capture: "a"}}},
					Servers: [][]domain.CustomTCPItem{{{Rand: 4, RandRange: "48-57"}}}, Errors: [][]domain.CustomTCPItem{{{Reuse: "a"}}}}},
			}},
			TLS: domain.TLS{Enabled: true, ServerName: "x.example", Reality: &domain.Reality{PublicKey: "pk", MLDSA65Verify: base64.RawURLEncoding.EncodeToString(make([]byte, 1952))}}},
		{ID: "tls", Endpoint: domain.Endpoint{Host: "t.example", Port: 443}, Protocol: vless, DialVia: "hy",
			Transport: domain.Transport{Kind: domain.TCP}, TLS: domain.TLS{Enabled: true, ServerName: "t.example", ECHConfigList: "AEX+DQBB"}},
		{ID: "hy", Endpoint: domain.Endpoint{Host: "hy.example", Port: 443}, Protocol: hysteria, TLS: domain.TLS{Enabled: true},
			Transport: domain.Transport{Kind: domain.Hysteria, Salamander: domain.NewSecret("obfs"), QuicParams: &domain.QuicParams{
				Congestion: "force-brutal", BBRProfile: "standard", BrutalUp: "1g", BrutalDown: "2g", BrutalDisableLossCompensation: true,
				InitStreamReceiveWindow: 16384, MaxStreamReceiveWindow: 16385, InitConnectionReceiveWindow: 16386, MaxConnectionReceiveWindow: 16387,
				MaxIdleTimeout: 30, KeepAlivePeriod: 10, DisablePathMTUDiscovery: true, DisableChromeParrot: true, DisableGSO: true,
				DisableStatelessReset: true, MaxIncomingStreams: 8}}},
	}
	in := lineage(t, "a", "s", "DE", "h.example")
	if in.Server, err = domain.NewServer("root", hops); err != nil {
		t.Fatal(err)
	}
	body, err := encodeLineage(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeLineage("a", "s", body)
	if err != nil {
		t.Fatal(err)
	}
	if _, got := out.Server.Closure(); !reflect.DeepEqual(got, hops) {
		t.Errorf("read back\n%+v\nwant\n%+v", got, hops)
	}
	again, err := encodeLineage(out)
	if err != nil || string(again) != string(body) {
		t.Errorf("written again as %s (%v)\nwant %s", again, err, body)
	}
	bad := strings.Replace(string(body), `"lengths":["0","5"]`, `"lengths":["0"]`, 1)
	if bad == string(body) {
		t.Fatal("the row has no lengths to spoil")
	}
	if _, err := decodeLineage("a", "s", []byte(bad)); err == nil {
		t.Error("a stored mask the core refuses read back")
	}
}
