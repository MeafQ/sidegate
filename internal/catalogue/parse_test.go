package catalogue

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

var protocolNames = map[domain.Protocol]string{
	domain.VLESS: "vless", domain.VMess: "vmess", domain.Trojan: "trojan",
	domain.Shadowsocks: "ss", domain.SOCKS: "socks", domain.HTTPConnect: "http", domain.Hysteria2: "hysteria2",
}

var transportNames = map[domain.TransportKind]string{
	domain.TCP: "tcp", domain.WebSocket: "ws", domain.GRPC: "grpc", domain.XHTTP: "xhttp", domain.Hysteria: "hysteria",
}

func describeHop(h domain.Hop) string {
	p, _, _, _, flow := h.Protocol.Fields()
	security := "none"
	switch {
	case h.TLS.Reality != nil:
		security = "reality"
	case len(h.TLS.PinnedCertSHA256) > 0:
		security = "tls+pin"
	case h.TLS.Enabled:
		security = "tls"
	}
	s := fmt.Sprintf("%s %s:%d %s %s", protocolNames[p], h.Endpoint.Host, h.Endpoint.Port, transportNames[h.Transport.Kind], security)
	if flow != "" {
		s += " vision"
	}
	return s
}

func describe(e Entry) string {
	_, hops := e.Server.Closure()
	parts := []string{e.Label + " | " + describeHop(hops[0])}
	for _, h := range hops[1:] {
		parts = append(parts, "via "+describeHop(h))
	}
	return strings.Join(parts, " ")
}

func diag(entry int, code DiagnosticCode, field string) Diagnostic {
	return Diagnostic{Entry: entry, Code: code, Field: field}
}

func parseFile(t *testing.T, name string) Document {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(data, Detect, Limits{})
	if err != nil {
		t.Fatalf("Parse(%s) = %v", name, err)
	}
	return doc
}

func TestParsePanelFixtures(t *testing.T) {
	cases := []struct {
		file    string
		entries []string
		diags   []Diagnostic
	}{
		{
			file: "3x-ui.b64",
			entries: []string{
				"DE-Reality-alice-📊2.31GB/50.00GB⏳12D | vless de1.example.net:443 tcp reality vision",
				"DE-WS-alice | vless cdn.example.net:443 ws tls",
				"DE-XHTTP-alice | vless de2.example.net:8443 xhttp reality",
				"DE-gRPC-alice | vless de3.example.net:2053 grpc tls",
				"DE-VMess-alice | vmess de4.example.net:443 ws tls",
				"NL-Trojan-alice | trojan nl1.example.net:443 grpc tls",
				"SS-2022-alice | ss ss1.example.net:8388 tcp none",
			},
		},
		{
			file: "marzban.b64",
			entries: []string{
				"🇩🇪 Germany [alice] 12.3 GB / 100 GB ⏳ 5 days left | vless fra.example.net:443 tcp reality vision",
				"🇳🇱 Netherlands [alice] 12.3 GB / 100 GB ⏳ 5 days left | vmess ams.example.net:8081 tcp none",
				"🇫🇮 Finland [alice] 12.3 GB / 100 GB ⏳ 5 days left | trojan hel.example.net:2083 ws tls",
				"🇵🇱 Poland [alice] 12.3 GB / 100 GB ⏳ 5 days left | ss waw.example.net:1080 tcp none",
			},
		},
		{
			file: "hiddify.txt",
			entries: []string{
				"Hiddify WS 🚀 | vless hd.example.net:443 ws tls",
				"Hiddify Reality gRPC | vless hd.example.net:443 grpc reality",
				"Hiddify Hysteria2 | hysteria2 hd.example.net:8443 hysteria tls",
			},
			diags: []Diagnostic{
				diag(2, CodeInsecureTLS, "allowInsecure"),
				diag(4, CodeUnsupportedProtocol, "tuic"),
				diag(5, CodeUnsupportedPlugin, "plugin"),
				diag(6, CodeUnsupportedProtocol, "wireguard"),
			},
		},
		{
			file: "remnawave-xray.json",
			entries: []string{
				"🇩🇪 Frankfurt · proxy | vless fra.rw.example.net:443 tcp reality vision",
				"🇳🇱 Amsterdam via Frankfurt · proxy | vless ams.rw.example.net:8443 xhttp tls via trojan relay.rw.example.net:443 ws tls",
				"🇫🇮 Helsinki pinned · proxy | vmess hel.rw.example.net:443 grpc tls+pin",
				"🇹🇷 Istanbul fragment · proxy | vless ist.rw.example.net:443 ws tls",
			},
			diags: []Diagnostic{
				diag(3, CodeInsecureTLS, "allowInsecure"),
				diag(5, CodeMalformedEntry, "hysteriaSettings"),
			},
		},
		{
			file: "mihomo.yaml",
			entries: []string{
				"🇩🇪 DE Reality | vless de.mihomo.example.net:443 tcp reality vision",
				"🇳🇱 NL VMess WS | vmess nl.mihomo.example.net:443 ws tls",
				"🇫🇮 FI Trojan gRPC | trojan fi.mihomo.example.net:443 grpc tls",
				"SS 2022 | ss ss.mihomo.example.net:8388 tcp none",
				"Hy2 | hysteria2 hy.mihomo.example.net:443 hysteria tls",
				"Relay SOCKS | socks relay.mihomo.example.net:1080 tcp none",
				"🇺🇸 US via relay | vless us.mihomo.example.net:443 ws tls+pin via socks relay.mihomo.example.net:1080 tcp none",
			},
			diags: []Diagnostic{
				diag(4, CodeUnsupportedPlugin, "plugin"),
				diag(6, CodeUnsupportedProtocol, "tuic"),
				diag(9, CodeUnresolvedDialer, "dialer-proxy"),
			},
		},
		{
			file: "sing-box.json",
			entries: []string{
				"🇩🇪 DE Reality | vless de.sb.example.net:443 tcp reality vision",
				"🇳🇱 NL VMess | vmess nl.sb.example.net:443 ws tls",
				"🇫🇮 FI Trojan | trojan fi.sb.example.net:443 grpc tls",
				"SS | ss ss.sb.example.net:8388 tcp none",
				"Hy2 | hysteria2 hy.sb.example.net:443 hysteria tls",
				"🇺🇸 US chained | vless us.sb.example.net:443 tcp tls via socks relay.sb.example.net:1080 tcp none",
			},
			diags: []Diagnostic{
				diag(5, CodeUnsupportedProtocol, "tuic"),
				diag(6, CodeUnsupportedTransport, "network"),
			},
		},
		{
			file: "sip008.json",
			entries: []string{
				"Tokyo | ss tyo.sip.example.net:8388 tcp none",
				"Osaka | ss osa.sip.example.net:8388 tcp none",
			},
			diags: []Diagnostic{
				diag(2, CodeUnsupportedCipher, "method"),
				diag(3, CodeUnsupportedPlugin, "plugin"),
			},
		},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			doc := parseFile(t, c.file)
			var got []string
			for _, e := range doc.Entries() {
				got = append(got, describe(e))
			}
			if !slices.Equal(got, c.entries) {
				t.Errorf("entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.entries, "\n"))
			}
			if !slices.Equal(doc.Diagnostics(), c.diags) {
				t.Errorf("diagnostics = %+v, want %+v", doc.Diagnostics(), c.diags)
			}
		})
	}
}

func TestParseSIP008KeepsProviderID(t *testing.T) {
	doc := parseFile(t, "sip008.json")
	if got := doc.Entries()[0].Identity.ProviderID; got != "27b8a625-4f4b-4428-9f0f-8a2317db7c79" {
		t.Fatalf("provider id = %q", got)
	}
}

func parseOne(t *testing.T, body string) (Entry, []Diagnostic, error) {
	t.Helper()
	doc, err := Parse([]byte(body), Detect, Limits{})
	var e Entry
	if entries := doc.Entries(); len(entries) == 1 {
		e = entries[0]
	}
	return e, doc.Diagnostics(), err
}

const linkUUID = "e5e5e5e5-0000-4000-8000-000000000001"

func TestParseLinkVariants(t *testing.T) {
	pin := "391df8c92c99edba0cc2d8a90c533a9e3ef69e7cbe6e6a6adaf1fcfc92ed5d7d"
	cases := []struct {
		name, link, want string
		check            func(t *testing.T, h domain.Hop)
	}{
		{
			name: "allowInsecure with a pin becomes a pinned certificate",
			link: "vless://" + linkUUID + "@a.example.net:443?security=tls&sni=a.example.net&allowInsecure=1&pcs=" + pin + "#p",
			want: "p | vless a.example.net:443 tcp tls+pin",
			check: func(t *testing.T, h domain.Hop) {
				if !slices.Equal(h.TLS.PinnedCertSHA256, []string{pin}) {
					t.Errorf("pins = %v", h.TLS.PinnedCertSHA256)
				}
			},
		},
		{
			name: "an OpenSSL-style pin is normalized",
			link: "trojan://pw@a.example.net:443?sni=a.example.net&pcs=" + strings.ToUpper(colons(pin)) + "#o",
			want: "o | trojan a.example.net:443 tcp tls+pin",
			check: func(t *testing.T, h domain.Hop) {
				if !slices.Equal(h.TLS.PinnedCertSHA256, []string{pin}) {
					t.Errorf("pins = %v", h.TLS.PinnedCertSHA256)
				}
			},
		},
		{
			name: "allowInsecure with a verified name keeps the name",
			link: "trojan://pw@a.example.net:443?sni=cdn.example.net&allowInsecure=1&vcn=a.example.net#n",
			want: "n | trojan a.example.net:443 tcp tls",
			check: func(t *testing.T, h domain.Hop) {
				if !slices.Equal(h.TLS.VerifyNames, []string{"a.example.net"}) {
					t.Errorf("verify names = %v", h.TLS.VerifyNames)
				}
			},
		},
		{
			name: "allowInsecure is meaningless under reality",
			link: "vless://" + linkUUID + "@a.example.net:443?security=reality&sni=www.example.com&pbk=key&sid=ab&allowInsecure=1#r",
			want: "r | vless a.example.net:443 tcp reality",
		},
		{
			name: "reality without sni defaults to the remote host",
			link: "vless://" + linkUUID + "@edge.example.com:443?security=reality&pbk=" + realityKey + "&fp=chrome#Edge",
			want: "Edge | vless edge.example.com:443 tcp reality",
			check: func(t *testing.T, h domain.Hop) {
				if h.TLS.ServerName != "edge.example.com" {
					t.Errorf("server name = %q", h.TLS.ServerName)
				}
			},
		},
		{
			name: "vmess in the standard URI form",
			link: "vmess://99c80931-f3f1-4f84-bffd-6eed6030f53d@qv2ray.net:31415?encryption=none#VMessTCPNaked",
			want: "VMessTCPNaked | vmess qv2ray.net:31415 tcp none",
			check: func(t *testing.T, h domain.Hop) {
				if _, id, _, cipher, _ := h.Protocol.Fields(); id.Reveal() != "99c80931-f3f1-4f84-bffd-6eed6030f53d" || cipher != "none" {
					t.Errorf("id = %q, cipher = %q", id.Reveal(), cipher)
				}
			},
		},
		{
			name: "a raw percent sign in a vless remark",
			link: "vless://" + linkUUID + "@a.example.net:443?security=tls&sni=a.example.net#DE 100% speed",
			want: "DE 100% speed | vless a.example.net:443 tcp tls",
		},
		{
			name: "a raw percent sign in a trojan remark",
			link: "trojan://pw@a.example.net:443?sni=a.example.net#Traffic 80%",
			want: "Traffic 80% | trojan a.example.net:443 tcp tls",
		},
		{
			name: "a raw percent sign in a shadowsocks remark",
			link: "ss://YWVzLTI1Ni1nY206cGFzcw@a.example.net:8388#SS 50% off",
			want: "SS 50% off | ss a.example.net:8388 tcp none",
		},
		{
			name: "an escaped remark is unescaped",
			link: "trojan://pw@a.example.net:443?sni=a.example.net#DE%20%F0%9F%87%A9%F0%9F%87%AA",
			want: "DE 🇩🇪 | trojan a.example.net:443 tcp tls",
		},
		{
			name: "a raw semicolon in the query",
			link: "vless://" + linkUUID + "@a.example.net:443?type=ws&path=/ws;ed=2048&security=tls&sni=a.example.net#ws",
			want: "ws | vless a.example.net:443 ws tls",
			check: func(t *testing.T, h domain.Hop) {
				if h.Transport.Path != "/ws;ed=2048" {
					t.Errorf("path = %q", h.Transport.Path)
				}
			},
		},
		{
			name: "SIP002 with plain 2022 userinfo",
			link: "ss://2022-blake3-aes-256-gcm:a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2U%3D@a.example.net:8388#ss2022",
			want: "ss2022 | ss a.example.net:8388 tcp none",
		},
		{
			name: "legacy shadowsocks with a base64 authority",
			link: "ss://YWVzLTEyOC1nY206cHdAMTkyLjAuMi4xMDo4Mzg4#legacy",
			want: "legacy | ss 192.0.2.10:8388 tcp none",
		},
		{
			name: "IPv6 literal",
			link: "vless://" + linkUUID + "@[2001:db8::1]:443?security=tls&sni=v6.example.net#v6",
			want: "v6 | vless 2001:db8::1:443 tcp tls",
		},
		{
			name: "vmess grpc reads the service name from path",
			link: "vmess://eyJ2IjoiMiIsInBzIjoiZyIsImFkZCI6ImEuZXhhbXBsZS5uZXQiLCJwb3J0Ijo0NDMsImlkIjoiZTVlNWU1ZTUtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAxIiwiYWlkIjowLCJuZXQiOiJncnBjIiwidHlwZSI6Im11bHRpIiwicGF0aCI6InN2YyIsInRscyI6InRscyJ9",
			want: "g | vmess a.example.net:443 grpc tls",
			check: func(t *testing.T, h domain.Hop) {
				if h.Transport.Service != "svc" || h.Transport.Mode != "multi" {
					t.Errorf("transport = %+v", h.Transport)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, diags, err := parseOne(t, c.link)
			if err != nil || len(diags) > 0 {
				t.Fatalf("err = %v, diagnostics = %+v", err, diags)
			}
			if got := describe(e); got != c.want {
				t.Fatalf("entry = %q, want %q", got, c.want)
			}
			if c.check != nil {
				_, hops := e.Server.Closure()
				c.check(t, hops[0])
			}
		})
	}
}

func TestParseLinkDiagnostics(t *testing.T) {
	cases := []struct {
		name, link string
		code       DiagnosticCode
		field      string
	}{
		{"insecure without pin", "vless://" + linkUUID + "@a.example.net:443?security=tls&allowInsecure=true#x", CodeInsecureTLS, "allowInsecure"},
		{"pin that is not hex", "trojan://pw@a.example.net:443?allowInsecure=1&pcs=not-a-sha256#x", CodeMalformedEntry, "pinnedPeerCertSha256"},
		{"pin shorter than SHA-256", "trojan://pw@a.example.net:443?allowInsecure=1&pcs=391df8c9#x", CodeMalformedEntry, "pinnedPeerCertSha256"},
		{"one bad pin in a list", "trojan://pw@a.example.net:443?pcs=391df8c92c99edba0cc2d8a90c533a9e3ef69e7cbe6e6a6adaf1fcfc92ed5d7d,zz#x", CodeMalformedEntry, "pinnedPeerCertSha256"},
		{"malformed escape", "vless://" + linkUUID + "@a.example.net:443?path=%zz#x", CodeMalformedEntry, "query"},
		{"missing port", "vless://" + linkUUID + "@a.example.net?security=tls#x", CodeMalformedEntry, "port"},
		{"httpupgrade", "vless://" + linkUUID + "@a.example.net:443?type=httpupgrade#x", CodeUnsupportedTransport, "network"},
		{"kcp", "vless://" + linkUUID + "@a.example.net:443?type=kcp#x", CodeUnsupportedTransport, "network"},
		{"http header obfuscation", "vless://" + linkUUID + "@a.example.net:443?type=tcp&headerType=http#x", CodeUnsupportedOption, "headerType"},
		{"xhttp extra", "vless://" + linkUUID + "@a.example.net:443?type=xhttp&extra=%7B%22xmux%22%3A%7B%7D%7D#x", CodeUnsupportedOption, "extra"},
		{"xtls security", "vless://" + linkUUID + "@a.example.net:443?security=xtls#x", CodeUnsupportedSecurity, "security"},
		{"a fragment with no length", "vless://" + linkUUID + "@a.example.net:443?security=tls&fm=%7B%22tcp%22%3A%5B%7B%22type%22%3A%22fragment%22%7D%5D%7D#x", CodeUnsupportedOption, "fm"},
		{"post-quantum reality", "vless://" + linkUUID + "@a.example.net:443?security=reality&pbk=k&sni=s&pqv=abc#x", CodeMalformedEntry, "mldsa65Verify"},
		{"vless encryption with no key", "vless://" + linkUUID + "@a.example.net:443?encryption=mlkem768x25519plus.native.0rtt.abc#x", CodeUnsupportedOption, "encryption"},
		{"reality without key", "vless://" + linkUUID + "@a.example.net:443?security=reality&sni=s#x", CodeMalformedEntry, "reality"},
		{"vision over websocket", "vless://" + linkUUID + "@a.example.net:443?type=ws&security=tls&flow=xtls-rprx-vision#x", CodeInvalidServer, ""},
		{"vmess alterId", "vmess://eyJhZGQiOiJhLmV4YW1wbGUubmV0IiwicG9ydCI6NDQzLCJpZCI6ImU1ZTVlNWU1LTAwMDAtNDAwMC04MDAwLTAwMDAwMDAwMDAwMSIsImFpZCI6NjR9", CodeUnsupportedOption, "alterId"},
		{"stream cipher", "ss://YWVzLTI1Ni1jZmI6cHc@a.example.net:8388#x", CodeUnsupportedCipher, "method"},
		{"socks link", "socks://dXNlcjpwYXNz@a.example.net:1080#x", CodeUnsupportedProtocol, "socks"},
		{"unknown scheme", "foobar://a.example.net:1#x", CodeUnsupportedProtocol, "other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags, err := parseOne(t, c.link)
			if !errors.Is(err, ErrNoUsableEntries) {
				t.Fatalf("err = %v, want ErrNoUsableEntries", err)
			}
			if want := []Diagnostic{diag(0, c.code, c.field)}; !slices.Equal(diags, want) {
				t.Fatalf("diagnostics = %+v, want %+v", diags, want)
			}
		})
	}
}

func TestParseDiagnosticsNeverEchoInput(t *testing.T) {
	_, diags, _ := parseOne(t, "s3cr3t-scheme://token@a.example.net:1#x")
	for _, d := range diags {
		if strings.Contains(string(d.Code)+d.Field, "s3cr3t") {
			t.Fatalf("diagnostic leaks input: %+v", d)
		}
	}
}

func TestParseLimitsAndEnvelopes(t *testing.T) {
	link := "vless://" + linkUUID + "@a.example.net:443?security=tls#x\n"
	bomb := "a: &a [x, x, x, x, x, x, x, x, x, x]\nb: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a, *a]\n" +
		"c: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b, *b]\nd: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c, *c]\n" +
		"e: &e [*d, *d, *d, *d, *d, *d, *d, *d, *d, *d]\nproxies: [*e]\n"
	cases := []struct {
		name    string
		body    string
		limits  Limits
		err     error
		entries int
	}{
		{"decoded size", strings.Repeat(link, 10), Limits{DecodedBytes: 64}, ErrTooLarge, 0},
		{"entry count", strings.Repeat(link, 3), Limits{Entries: 2}, ErrTooManyEntries, 0},
		{"yaml alias expansion", bomb, Limits{Nodes: 10_000}, ErrTooLarge, 0},
		{"garbage", "<html>maintenance</html>", Limits{}, ErrUnknownFormat, 0},
		{"truncated json", `{"outbounds": [{"protocol": "vless",`, Limits{}, ErrMalformed, 0},
		{"empty body", "", Limits{}, ErrUnknownFormat, 0},
		{"explicitly empty sip008", `{"version": 1, "servers": []}`, Limits{}, nil, 0},
		{"explicitly empty clash", "proxies: []\n", Limits{}, nil, 0},
		{"null clash proxies", "proxies:\n", Limits{}, ErrMalformed, 0},
		{"repeated clash proxies", "proxies: []\nproxies: []\n", Limits{}, ErrMalformed, 0},
		{"base64 with line breaks", "dmxlc3M6Ly9lNWU1ZTVlNS0wMDAwLTQwMDAtODAwMC0wMDAwMDAw\nMDAwMDFAYS5leGFtcGxlLm5ldDo0NDM/c2VjdXJpdHk9dGxzI3gK\n", Limits{}, nil, 1},
		{"utf-8 bom", "\xef\xbb\xbf" + link, Limits{}, nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, err := Parse([]byte(c.body), Detect, c.limits)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			if len(doc.Entries()) != c.entries {
				t.Fatalf("entries = %d, want %d", len(doc.Entries()), c.entries)
			}
		})
	}
}

func TestParseExplicitFormat(t *testing.T) {
	_, err := Parse([]byte(`{"version":1,"servers":[]}`), ClashYAML, Limits{})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("SIP008 parsed as Clash: err = %v, want ErrMalformed", err)
	}
}

func TestParseAcceptsEmptyTrailingYAMLDocuments(t *testing.T) {
	proxy := "proxies:\n  - {name: n0, type: trojan, server: h.example.com, port: 443, password: pw}\n"
	for _, body := range []string{proxy + "---\n", proxy + "---\n---\n", proxy + "--- # end\n", "---\n" + proxy + "...\n"} {
		doc, err := Parse([]byte(body), Detect, Limits{})
		if err != nil || len(doc.Entries()) != 1 {
			t.Errorf("%q: err = %v, entries = %d, want 1", body, err, len(doc.Entries()))
		}
	}
}

func TestParseRejectsATrailingYAMLDocument(t *testing.T) {
	for _, body := range []string{
		"proxies: []\n---\nproxies: [unterminated",
		"proxies: []\n---\nproxies: []\n",
		"proxies: []\n---\n---\nproxies: []\n",
	} {
		sim := newSim(t, vless("DE"))
		doc, err := Parse([]byte(body), Detect, Limits{})
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: err = %v, want ErrMalformed", body, err)
		}
		if r := sim.apply(doc); len(r.Changes) != 0 {
			t.Errorf("%q: %d lineage changes, want none", body, len(r.Changes))
		}
	}
}

// repeated fills a document of about size bytes with copies of elem.
func repeated(open, elem, sep, close string, size int) []byte {
	var b bytes.Buffer
	b.WriteString(open)
	for i := 0; b.Len()+len(sep)+len(elem)+len(close) <= size; i++ {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(elem)
	}
	b.WriteString(close)
	return b.Bytes()
}

// directives fills a document of about size bytes with distinct %TAG
// directives, each checked against every earlier one.
func directives(size int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < size-64; i++ {
		fmt.Fprintf(&b, "%%TAG !t%d! tag:x,2000:\n", i)
	}
	b.WriteString("---\nproxies: []\n")
	return b.Bytes()
}

func TestParseFailsCheaplyOnHostileStructure(t *testing.T) {
	const budget = 64 << 20 // A YAML parse that spends the whole node budget allocates about 50 MB.
	cases := []struct {
		name string
		body []byte
		err  error
	}{
		{"xray config list", repeated("[", "{}", ",", "]", 8<<20), ErrTooLarge},
		{"xray outbounds", repeated(`{"outbounds":[`, "{}", ",", "]}", 8<<20), ErrTooLarge},
		{"xray users", repeated(`{"outbounds":[{"protocol":"vless","settings":{"users":[`, "{}", ",", "]}}]}", 8<<20), ErrTooLarge},
		{"sing-box outbounds", repeated(`{"outbounds":[`, `{"type":"x"}`, ",", "]}", 8<<20), ErrTooLarge},
		{"outbounds under the value budget", repeated(`{"outbounds":[`, "{}", ",", "]}", 60_000), ErrTooManyEntries},
		{"users under the value budget", repeated(`{"outbounds":[{"protocol":"vless","settings":{"users":[`, "{}", ",", "]}}]}", 290_000), ErrNoUsableEntries},
		{"valid outbounds under the element budget", repeated(`{"outbounds":[`,
			`{"protocol":"vless","settings":{"address":"a.example.net","port":1,"id":"x"}}`, ",", "]}", 16_000*80), ErrTooManyEntries},
		{"yaml proxies under the node budget", []byte("proxies:\n" + strings.Repeat("- {}\n", 199_000)), ErrTooManyEntries},
		{"yaml flow list", repeated("proxies: [", "{}", ",", "]\n", 8<<20), ErrTooLarge},
		{"escaped outbounds key", repeated(`{"outb\u006funds":[`, "{}", ",", "]}", 8<<20), ErrTooLarge},
		{"case-folded outbounds key", repeated(`{"OUTBOUND\u017f":[`, "{}", ",", "]}", 8<<20), ErrTooLarge},
		{"unicode-folded outbounds key", repeated(`{"outboundſ":[`, "{}", ",", "]}", 8<<20), ErrTooLarge},
		{"yaml list after a block scalar", repeated("a: |\n  \"\nproxies: [", "{}", ",", "]\nb: \"\n", 8<<20), ErrTooLarge},
		{"yaml list after a plain continuation", repeated("a: x\n  [\nproxies:\n- x, 'y: [", "{}", ",", "]'\n", 8<<20), ErrTooLarge},
		{"yaml list after a multi-line quote", repeated("a: \"x\n  \"\nproxies: [", "{}", ",", "]\n", 8<<20), ErrTooLarge},
		{"yaml lone CR breaks", repeated("proxies:\r", "- {a: b, c: d}", "\r", "\r", 8<<20), ErrTooLarge},
		{"yaml lone CR before a flow list", repeated("proxies:\r- x\r- [", "a", ",", "]\r", 8<<20), ErrTooLarge},
		{"yaml NEL breaks", repeated("proxies:\u0085", "- [a, b, c]", "\u0085", "\u0085", 8<<20), ErrTooLarge},
		{"yaml LS breaks", repeated("proxies:\n x:\u2028", " - {a: b, c: d}", "\u2028", "\u2028", 8<<20), ErrTooLarge},
		{"yaml PS breaks", repeated("proxies:\u2029", "- [a, b, c]", "\u2029", "\u2029", 8<<20), ErrTooLarge},
		{"yaml tagged empty key", repeated("!!str : x\nproxies: [", "a", ",", "]\n", 8<<20), ErrTooLarge},
		{"yaml anchored empty key", repeated("&k : x\nproxies: [", "a", ",", "]\n", 8<<20), ErrTooLarge},
		{"yaml flow colon before a tag", repeated("proxies: [", "!t :!t ", ",", "]\n", 8<<20), ErrTooLarge},
		{"yaml flow colon between an anchor and a tag", repeated("proxies: [", "&x :!t ", ",", "]\n", 8<<20), ErrTooLarge},
		{"yaml flow colon before a tagged scalar", repeated("proxies: [", "!t :!t a", ",", "]\n", 8<<20), ErrTooLarge},
		{"yaml aliases", repeated("proxies: [&x a, ", "*x", ",", "]\n", 8<<20), ErrTooLarge},
		{"yaml empty documents", repeated("proxies: []\n", "---", "\n", "\n", 8<<20), ErrTooLarge},
		{"yaml a comment per node", repeated("proxies:\n", "- a #c", "\n", "\n", 8<<20), ErrTooLarge},
		{"yaml comment lines between nodes", repeated("proxies:\n", "- a\n#c", "\n", "\n", 8<<20), ErrTooLarge},
		{"yaml tag directives", directives(8 << 20), ErrTooLarge},
		{"yaml duplicate keys in a flow proxy", []byte("proxies:\n- {name: a, type: trojan, server: h.example.com, port: 443, password: pw" +
			strings.Repeat(", x: v", 2000) + "}\n"), ErrNoUsableEntries},
		{"yaml duplicate keys in a block proxy", []byte("proxies:\n- name: a\n  type: trojan\n  server: h.example.com\n  port: 443\n  password: pw\n" +
			strings.Repeat("  x: v\n", 20_000)), ErrNoUsableEntries},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := Parse(c.body, Detect, Limits{})
			runtime.ReadMemStats(&after)
			if !errors.Is(err, c.err) {
				t.Errorf("err = %v, want %v", err, c.err)
			}
			alloc := after.TotalAlloc - before.TotalAlloc
			t.Logf("%d KB body allocated %d MB", len(c.body)>>10, alloc>>20)
			if alloc > budget {
				t.Errorf("%d KB body allocated %d MB, want at most %d MB", len(c.body)>>10, alloc>>20, budget>>20)
			}
		})
	}
}

// Provider routing and rules never become servers, so they must not count
// against the value budget the servers share.
func TestParseIgnoresRoutingInTheValueBudget(t *testing.T) {
	var clash strings.Builder
	clash.WriteString("proxies:\n")
	for i := range 2 {
		fmt.Fprintf(&clash, "  - {name: n%d, type: trojan, server: h%d.example.com, port: 443, password: pw}\n", i, i)
	}
	clash.WriteString("proxy-groups:\n  - {name: PROXY, type: select, proxies: [n0, n1]}\nrules:\n")
	for i := range 40_000 {
		fmt.Fprintf(&clash, "  - DOMAIN-SUFFIX,site-%d.example-cdn.com,PROXY\n", i)
	}
	clash.WriteString("  - MATCH,PROXY\n")

	var domains []string
	for i := range 600 {
		domains = append(domains, fmt.Sprintf(`"domain:site%d.example.ru"`, i))
	}
	var configs []string
	for i := range 400 {
		configs = append(configs, fmt.Sprintf(`{"remarks":"N%d","outbounds":[{"tag":"proxy","protocol":"trojan",
			"settings":{"servers":[{"address":"n%d.example.net","port":443,"password":"pw"}]},
			"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"n%d.example.net"}}},
			{"tag":"direct","protocol":"freedom"}],"routing":{"rules":[{"outboundTag":"direct","domain":[%s]}]}}`,
			i, i, i, strings.Join(domains, ",")))
	}
	xray := "[" + strings.Join(configs, ",") + "]"

	for _, c := range []struct {
		name, body string
		entries    int
	}{{"clash rules", clash.String(), 2}, {"xray routing", xray, 400}} {
		t.Run(c.name, func(t *testing.T) {
			doc, err := Parse([]byte(c.body), Detect, DefaultLimits())
			if err != nil || len(doc.Entries()) != c.entries {
				t.Fatalf("%d KB feed: err = %v, entries = %d, want %d", len(c.body)>>10, err, len(doc.Entries()), c.entries)
			}
		})
	}
}

// psychHeader is how Ruby's Psych, over libyaml, emits a long remark: a
// double-quoted scalar folded onto a second line.
const psychHeader = "---\nproxies:\n- name: \"\\U0001F1E9\\U0001F1EA Germany Frankfurt 01 | Premium | x2 | Netflix, Disney+\n" +
	"    unlocked | IPv6\"\n  type: trojan\n  server: h.example.com\n  port: 443\n  password: pw\nrules:\n"

func TestParseImportsLargeClashFeeds(t *testing.T) {
	rules := func(indent string, n int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "%s- DOMAIN-SUFFIX,site-%d.example-cdn.com,PROXY\n", indent, i)
		}
		return b.String()
	}
	trojan := "proxies:\n  - {name: n0, type: trojan, server: h0.example.com, port: 443, password: pw}\n"
	var full strings.Builder
	full.WriteString("proxies:\n")
	for i := range 2000 {
		fmt.Fprintf(&full, "  - name: \"DE %d\"\n    type: vless\n    server: de%d.example.net\n    port: 443\n    uuid: %s\n"+
			"    network: ws\n    tls: true\n    servername: de%d.example.net\n    client-fingerprint: chrome\n"+
			"    ws-opts:\n      path: /ws\n      headers:\n        Host: de%d.example.net\n", i, i, uuid(i), i, i)
	}
	full.WriteString("proxy-groups:\n")
	for g := range 4 {
		fmt.Fprintf(&full, "  - name: G%d\n    type: select\n    proxies:\n", g)
		for i := range 2000 {
			fmt.Fprintf(&full, "      - DE %d\n", i)
		}
	}
	full.WriteString("rules:\n" + rules("  ", 40_000))
	mihomo := string(fixture(t, "mihomo.yaml"))
	for _, c := range []struct {
		name, body string
		entries    int
	}{
		{"40k rules", trojan + "rules:\n" + rules("  ", 40_000), 1},
		{"40k rules, CRLF", strings.ReplaceAll(trojan+"rules:\n"+rules("  ", 40_000), "\n", "\r\n"), 1},
		{"40k rules after a two-line quoted header", trojan + "  - {name: n1, type: trojan, server: h1.example.com, port: 443, password: pw,\n" +
			"     ws-opts: {headers: {User-Agent: \"Mozilla/5.0 (X11)\n       Chrome/120\"}}}\nrules:\n" + rules("  ", 40_000), 2},
		{"Psych-emitted, 40k rules", psychHeader + rules("", 40_000), 1},
		{"2000 proxies, 4 groups, 40k rules", full.String(), 2000},
		{"fixture with a trailing empty document", mihomo + "\n---\n# end\n", 7},
		{"fixture with a trailing end marker", mihomo + "\n...\n---\n", 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc, err := Parse([]byte(c.body), Detect, DefaultLimits())
			if err != nil || len(doc.Entries()) != c.entries {
				t.Fatalf("%d KB feed: err = %v, entries = %d, want %d", len(c.body)>>10, err, len(doc.Entries()), c.entries)
			}
		})
	}
}

func TestParseSingBoxKeepsTransportHeaders(t *testing.T) {
	singBox := `{"outbounds":[{"type":"vless","tag":"edge","server":"edge.example.com","server_port":443,"uuid":"` + linkUUID + `",
		"tls":{"enabled":true,"server_name":"front.example.com"},
		"transport":{"type":"ws","path":"/ws","headers":{"Host":"front.example.com","Cookie":"session=one"}}}]}`
	xray := `{"outbounds":[{"protocol":"vless","settings":{"address":"edge.example.com","port":443,"id":"` + linkUUID + `"},
		"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"front.example.com"},
		"wsSettings":{"path":"/ws","headers":{"Host":"front.example.com","Cookie":"session=one"}}}}]}`
	e, diags, err := parseOne(t, singBox)
	if err != nil || len(diags) > 0 {
		t.Fatalf("err = %v, diagnostics = %+v", err, diags)
	}
	_, hops := e.Server.Closure()
	if tr := hops[0].Transport; tr.Host != "front.example.com" || !maps.Equal(tr.Headers, map[string]string{"Cookie": "session=one"}) {
		t.Fatalf("transport = %+v", tr)
	}
	if identityOf(t, singBox).Strong != identityOf(t, xray).Strong {
		t.Fatal("the same headers from sing-box and Xray JSON give different identities")
	}

	for _, value := range []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36",
		"en-US,en;q=0.9",
		"a~b",
	} {
		for _, form := range []string{`"` + value + `"`, `["` + value + `"]`} {
			body := strings.Replace(singBox, `"Cookie":"session=one"`, `"X-Test":`+form, 1)
			e, diags, err := parseOne(t, body)
			if err != nil || len(diags) > 0 {
				t.Fatalf("header %s: err = %v, diagnostics = %+v", form, err, diags)
			}
			if _, hops := e.Server.Closure(); !maps.Equal(hops[0].Transport.Headers, map[string]string{"X-Test": value}) {
				t.Fatalf("header %s: headers = %q", form, hops[0].Transport.Headers)
			}
		}
	}

	multi := strings.Replace(singBox, `"Cookie":"session=one"`, `"Cookie":["a=1","b=2"]`, 1)
	if _, diags, _ := parseOne(t, multi); !slices.Equal(diags, []Diagnostic{diag(0, CodeUnsupportedOption, "headers")}) {
		t.Fatalf("multi-valued header: diagnostics = %+v", diags)
	}
}

func TestParseVMessFormsShareIdentity(t *testing.T) {
	uri := "vmess://" + linkUUID + "@a.example.net:443?type=ws&path=%2Fws&host=cdn.example.net&security=tls&sni=cdn.example.net&encryption=auto#u"
	legacy := "vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"v":"2","ps":"l","add":"a.example.net","port":"443","id":"`+
		linkUUID+`","aid":"0","scy":"auto","net":"ws","path":"/ws","host":"cdn.example.net","tls":"tls","sni":"cdn.example.net"}`))
	if identityOf(t, uri).Strong != identityOf(t, legacy).Strong {
		t.Fatal("the URI and base64 JSON forms of one VMess server have different identities")
	}
	for _, suffix := range []string{"?remarks=t.me%2F@chan", "#DE@chan", "?remarks=DE#a@b"} {
		e, diags, err := parseOne(t, legacy+suffix)
		if err != nil || len(diags) > 0 {
			t.Fatalf("base64 link with %q: err = %v, diagnostics = %+v", suffix, err, diags)
		}
		if e.Identity.Strong != identityOf(t, legacy).Strong {
			t.Fatalf("base64 link with %q parsed as another server", suffix)
		}
	}
}

// colons writes hex as OpenSSL prints a fingerprint: byte pairs joined by ':'.
func colons(hex string) string {
	var pairs []string
	for i := 0; i+1 < len(hex); i += 2 {
		pairs = append(pairs, hex[i:i+2])
	}
	return strings.Join(pairs, ":")
}
