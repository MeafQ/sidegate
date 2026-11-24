package catalogue

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

var (
	mldsaKey       = base64.RawURLEncoding.EncodeToString(slices.Repeat([]byte{7}, 1952))
	vlessKey       = base64.RawURLEncoding.EncodeToString(slices.Repeat([]byte{9}, 32))
	vlessEncrypted = "mlkem768x25519plus.native.0rtt." + vlessKey
)

const echList = "AEX+DQBB/zAAIAAg"

func oneHop(t *testing.T, body string) domain.Hop {
	t.Helper()
	e, diags, err := parseOne(t, body)
	if err != nil || len(diags) > 0 {
		t.Fatalf("err = %v, diagnostics = %+v", err, diags)
	}
	_, hops := e.Server.Closure()
	if len(hops) != 1 {
		t.Fatalf("hops = %+v, want one", hops)
	}
	return hops[0]
}

func refuses(t *testing.T, name, body string, code DiagnosticCode, field string) {
	t.Helper()
	_, diags, err := parseOne(t, body)
	if !errors.Is(err, ErrNoUsableEntries) || !slices.Equal(diags, []Diagnostic{diag(0, code, field)}) {
		t.Errorf("%s: err = %v, diagnostics = %+v, want %s %s", name, err, diags, code, field)
	}
}

func xrayVLESS(stream string) string {
	return `{"outbounds":[{"tag":"proxy","protocol":"vless","settings":{"address":"a.example.net","port":443,"id":"` + linkUUID + `"},"streamSettings":{` + stream + `}}]}`
}

func xrayHysteria(finalmask string) string {
	return `{"outbounds":[{"tag":"HY","protocol":"hysteria","settings":{"version":2,"address":"hy.example.net","port":8443},
		"streamSettings":{"network":"hysteria","security":"tls","tlsSettings":{"serverName":"hy.example.net","alpn":["h3"]},
		"hysteriaSettings":{"version":2,"auth":"p@ss"},"finalmask":` + finalmask + `}}]}`
}

func vlessLink(query string) string {
	return "vless://" + linkUUID + "@a.example.net:443?" + query + "#x"
}

func TestParseQuicParams(t *testing.T) {
	everything := `{"congestion":"Force-Brutal","bbrProfile":"Aggressive","brutalUp":" 100 Mbps ","brutalDown":"1g","brutalDisableLossCompensation":true,
		"initStreamReceiveWindow":16384,"maxStreamReceiveWindow":16385,"initConnectionReceiveWindow":16386,"maxConnectionReceiveWindow":16387,
		"maxIdleTimeout":30,"keepAlivePeriod":10,"disablePathMTUDiscovery":true,"disableChromeParrot":true,"disableGSO":true,
		"maxIncomingStreams":8,"disableStatelessReset":true`
	full := &domain.QuicParams{Congestion: "force-brutal", BBRProfile: "aggressive", BrutalUp: "100 mbps", BrutalDown: "1g", BrutalDisableLossCompensation: true,
		InitStreamReceiveWindow: 16384, MaxStreamReceiveWindow: 16385, InitConnectionReceiveWindow: 16386, MaxConnectionReceiveWindow: 16387,
		MaxIdleTimeout: 30, KeepAlivePeriod: 10, DisablePathMTUDiscovery: true, DisableChromeParrot: true, DisableGSO: true,
		DisableStatelessReset: true, MaxIncomingStreams: 8}
	for _, c := range []struct {
		name, quic string
		want       *domain.QuicParams
	}{
		{"everything", everything + `}`, full},
		{"debug is dropped", everything + `,"debug":true}`, full},
		{"only debug", `{"debug":true}`, nil},
		{"debug of any type", `{"debug":{"x":[1]}}`, nil},
		{"nothing", `{}`, nil},
		{"null", `null`, nil},
		{"a rate of zero", `{"brutalUp":"0 mbps"}`, nil},
	} {
		finalmask := `{"udp":[{"type":"salamander","settings":{"password":"obfs"}}],"quicParams":` + c.quic + `}`
		for form, body := range map[string]string{"xray": xrayHysteria(finalmask),
			"link": "hy2://p%40ss@hy.example.net:8443?sni=hy.example.net&alpn=h3&fm=" + url.QueryEscape(finalmask) + "#HY"} {
			t.Run(c.name+"/"+form, func(t *testing.T) {
				tr := oneHop(t, body).Transport
				if !reflect.DeepEqual(tr.QuicParams, c.want) || tr.Salamander.Reveal() != "obfs" {
					t.Errorf("quic = %+v, salamander %q, want %+v and obfs", tr.QuicParams, tr.Salamander.Reveal(), c.want)
				}
			})
		}
	}
	for name, quic := range map[string]string{
		"force-brutal without a rate":      `{"congestion":"force-brutal"}`,
		"force-brutal with a rate of zero": `{"congestion":"force-brutal","brutalUp":"0"}`,
		"a congestion the core lacks":      `{"congestion":"cubic"}`,
		"a profile the core lacks":         `{"bbrProfile":"turbo"}`,
		"a rate under the floor":           `{"brutalUp":"1kbps"}`,
		"a rate in unknown units":          `{"brutalUp":"1 mb/s"}`,
		"a small window":                   `{"maxStreamReceiveWindow":1024}`,
		"a negative window":                `{"maxStreamReceiveWindow":-1}`,
		"a long idle":                      `{"maxIdleTimeout":121}`,
		"a short keepalive":                `{"keepAlivePeriod":1}`,
		"few streams":                      `{"maxIncomingStreams":7}`,
		"an unknown field":                 `{"udpHop":{"port":"1-2"}}`,
		"a list":                           `[]`,
	} {
		refuses(t, name, xrayHysteria(`{"quicParams":`+quic+`}`), CodeUnsupportedOption, "finalmask")
	}
	refuses(t, "on a TCP server", xrayVLESS(`"network":"raw","security":"tls","finalmask":{"quicParams":{"congestion":"bbr"}}`), CodeUnsupportedOption, "finalmask")
	refuses(t, "on a TCP link", vlessLink("security=tls&fm="+url.QueryEscape(`{"quicParams":{"congestion":"bbr"}}`)), CodeUnsupportedOption, "fm")
}

func TestParseTCPMasks(t *testing.T) {
	fragment := func(f domain.FragmentMask) domain.TCPMask {
		return domain.TCPMask{Type: domain.MaskFragment, Fragment: &f}
	}
	custom := func(c domain.HeaderCustomTCP) domain.TCPMask {
		return domain.TCPMask{Type: domain.MaskHeaderCustom, HeaderCustom: &c}
	}
	for _, c := range []struct {
		name, tcp string
		want      []domain.TCPMask
	}{
		{"none", `[]`, nil},
		{"a fragment", `[{"type":"Fragment","settings":{"packets":"TLSHello","length":"100-200","delay":10,"maxSplit":"3-6"}}]`,
			[]domain.TCPMask{fragment(domain.FragmentMask{Packets: "tlshello", Length: "100-200", Delay: "10", MaxSplit: "3-6"})}},
		{"spellings of one range", `[{"type":"fragment","settings":{"length":"200-100","delay":"0","maxSplit":""}}]`,
			[]domain.TCPMask{fragment(domain.FragmentMask{Length: "100-200"})}},
		{"a fragment by segment", `[{"type":"fragment","settings":{"packets":"1-3","lengths":[0,"5-5","10-20"],"delays":["0",5]}}]`,
			[]domain.TCPMask{fragment(domain.FragmentMask{Packets: "1-3", Lengths: []string{"0", "5", "10-20"}, Delays: []string{"0", "5"}})}},
		{"two masks in order", `[{"type":"fragment","settings":{"length":1}},{"type":"fragment","settings":{"length":2}}]`,
			[]domain.TCPMask{fragment(domain.FragmentMask{Length: "1"}), fragment(domain.FragmentMask{Length: "2"})}},
		{"a custom header", `[{"type":"header-custom","settings":{
			"clients":[[{"packet":[0,1,255]},{"type":"str","packet":"GET é"},{"type":"hex","packet":"DEADbeef"},{"type":"base64","packet":"AP8="},{"type":"array","packet":"AP8="}],[]],
			"servers":[[{"rand":4,"randRange":"48-57","capture":"nonce"},{"delay":"1-5"},{"reuse":"nonce","transform":null}]],
			"errors":[[{"rand":8,"delay":3}]]}}]`,
			[]domain.TCPMask{custom(domain.HeaderCustomTCP{
				Clients: [][]domain.CustomTCPItem{{{Packet: []byte{0, 1, 255}}, {Packet: []byte("GET é")}, {Packet: []byte{0xde, 0xad, 0xbe, 0xef}}, {Packet: []byte{0, 255}}, {Packet: []byte{0, 255}}}, {}},
				Servers: [][]domain.CustomTCPItem{{{Rand: 4, RandRange: "48-57", Capture: "nonce"}, {Delay: "1-5"}, {Reuse: "nonce"}}},
				Errors:  [][]domain.CustomTCPItem{{{Rand: 8, Delay: "3"}}}})}},
		{"a range of nothing", `[{"type":"header-custom","settings":{"clients":[[{"rand":0,"randRange":"0-255","type":"str","packet":"x"}]]}}]`,
			[]domain.TCPMask{custom(domain.HeaderCustomTCP{Clients: [][]domain.CustomTCPItem{{{Packet: []byte("x")}}}})}},
	} {
		finalmask := `{"tcp":` + c.tcp + `}`
		for form, body := range map[string]string{
			"xray": xrayVLESS(`"network":"raw","security":"tls","tlsSettings":{"serverName":"a.example.net"},"finalmask":` + finalmask),
			"link": vlessLink("security=tls&sni=a.example.net&fm=" + url.QueryEscape(finalmask)),
		} {
			t.Run(c.name+"/"+form, func(t *testing.T) {
				if got := oneHop(t, body).Transport.TCPMasks; !reflect.DeepEqual(got, c.want) {
					t.Errorf("masks = %+v, want %+v", got, c.want)
				}
			})
		}
	}
	for _, network := range []string{"raw", "ws", "grpc", "xhttp"} {
		h := oneHop(t, xrayVLESS(`"network":"`+network+`","security":"tls","finalmask":{"tcp":[{"type":"fragment","settings":{"length":"5"}}]}`))
		if len(h.Transport.TCPMasks) != 1 {
			t.Errorf("%s: masks = %+v, want one", network, h.Transport.TCPMasks)
		}
	}
	one := `{"type":"fragment","settings":{"length":"5"}}`
	item := func(s string) string {
		return `{"tcp":[{"type":"header-custom","settings":{"clients":[[` + s + `]]}}]}`
	}
	for name, finalmask := range map[string]string{
		"sudoku":                          `{"tcp":[{"type":"sudoku","settings":{"password":"x"}}]}`,
		"xmc":                             `{"tcp":[{"type":"xmc","settings":{}}]}`,
		"filter":                          `{"tcp":[{"type":"filter","settings":{}}]}`,
		"xdns":                            `{"udp":[{"type":"xdns","settings":{"domains":["a.example.net"]}}]}`,
		"noise":                           `{"udp":[{"type":"noise","settings":{}}]}`,
		"salamander on TCP":               `{"udp":[{"type":"salamander","settings":{"password":"obfs"}}]}`,
		"one known mask beside another":   `{"tcp":[` + one + `,{"type":"sudoku","settings":{}}]}`,
		"five masks":                      `{"tcp":[` + strings.Join(slices.Repeat([]string{one}, 5), ",") + `]}`,
		"an unknown part":                 `{"tcp":[` + one + `],"icmp":[]}`,
		"an unknown mask field":           `{"tcp":[{"type":"fragment","settings":{"length":"5"},"extra":1}]}`,
		"an unknown fragment field":       `{"tcp":[{"type":"fragment","settings":{"length":"5","interval":"1"}}]}`,
		"no settings":                     `{"tcp":[{"type":"fragment"}]}`,
		"no length":                       `{"tcp":[{"type":"fragment","settings":{"packets":"tlshello"}}]}`,
		"a last length of zero":           `{"tcp":[{"type":"fragment","settings":{"length":"5","lengths":["5","0-3"]}}]}`,
		"packets from zero":               `{"tcp":[{"type":"fragment","settings":{"packets":"0-3","length":"5"}}]}`,
		"a negative length":               `{"tcp":[{"type":"fragment","settings":{"length":"-5-10"}}]}`,
		"a length over int32":             `{"tcp":[{"type":"fragment","settings":{"length":"4294967297"}}]}`,
		"a transform":                     item(`{"transform":{"op":"concat","args":[{"bytes":[1]}]}}`),
		"an unknown item field":           item(`{"rand":1,"var":"a"}`),
		"a random and a packet":           item(`{"rand":1,"packet":[1]}`),
		"a random and an empty packet":    item(`{"rand":1,"packet":[]}`),
		"an item of nothing":              item(`{}`),
		"too many random bytes":           item(`{"rand":4097}`),
		"a random range over a byte":      item(`{"rand":1,"randRange":"0-256"}`),
		"a variable name":                 item(`{"reuse":"a-b"}`),
		"a packet type":                   item(`{"type":"exp","packet":"x"}`),
		"a packet that is not its type":   item(`{"type":"hex","packet":"xyz"}`),
		"a packet that is not its base64": item(`{"type":"base64","packet":"A"}`),
		"a string packet that is a list":  item(`{"type":"str","packet":[1]}`),
		"a packet too long":               item(`{"type":"str","packet":"` + strings.Repeat("x", 4097) + `"}`),
		"seventeen items":                 item(strings.Join(slices.Repeat([]string{`{"rand":1}`}, 17), ",")),
		"seventeen sequences":             item(`{"rand":1}` + strings.Repeat(`],[{"rand":1}`, 16)),
		"a mask document over its bound":  `{"tcp":[` + one + `],` + strings.Repeat(" ", maxFinalMask) + `"udp":[]}`,
		"a list":                          `[]`,
	} {
		refuses(t, name, xrayVLESS(`"network":"raw","security":"tls","finalmask":`+finalmask), CodeUnsupportedOption, "finalmask")
		refuses(t, name, vlessLink("security=tls&fm="+url.QueryEscape(finalmask)), CodeUnsupportedOption, "fm")
	}
	for name, fm := range map[string]string{"not JSON": "fragment", "half a document": `{"tcp":[`, "two documents": `{} {}`} {
		refuses(t, name, vlessLink("security=tls&fm="+url.QueryEscape(fm)), CodeUnsupportedOption, "fm")
		refuses(t, name, "hy2://pw@hy.example.net?fm="+url.QueryEscape(fm)+"#x", CodeUnsupportedOption, "fm")
	}
	refuses(t, "a TCP mask on Hysteria", xrayHysteria(`{"tcp":[`+one+`]}`), CodeUnsupportedOption, "finalmask")
	refuses(t, "two Salamander masks", xrayHysteria(`{"udp":[{"type":"salamander","settings":{"password":"a"}},{"type":"salamander","settings":{"password":"a"}}]}`), CodeUnsupportedOption, "finalmask")
	fm := url.QueryEscape(`{"udp":[{"type":"salamander","settings":{"password":"other"}}]}`)
	refuses(t, "two Salamander passwords", "hy2://pw@hy.example.net?obfs=salamander&obfs-password=ob&fm="+fm+"#x", CodeUnsupportedOption, "fm")
	if h := oneHop(t, "hy2://pw@hy.example.net?obfs=salamander&obfs-password=other&fm="+fm+"#x"); h.Transport.Salamander.Reveal() != "other" {
		t.Errorf("one Salamander password said twice: %q", h.Transport.Salamander.Reveal())
	}
}

// fragmentFeed is a config whose root dials through a freedom outbound, the way clients fragmented
// before Xray had a mask for it.
func fragmentFeed(root, freedom string) string {
	return `{"outbounds":[{"tag":"proxy",` + root + `},{"tag":"frag","protocol":"freedom",` + freedom + `}]}`
}

const fragmentRoot = `"protocol":"vless","settings":{"address":"a.example.net","port":443,"id":"` + linkUUID + `"},
	"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"a.example.net"},"sockopt":{"dialerProxy":"frag"}}`

func TestParseFreedomFragmentDialer(t *testing.T) {
	const fragment = `"fragment":{"packets":"1-3","length":"10-20","interval":"5","maxSplit":"3"}`
	want := []domain.TCPMask{{Type: domain.MaskFragment, Fragment: &domain.FragmentMask{Packets: "1-3", Length: "10-20", Delay: "5", MaxSplit: "3"}}}
	for name, freedom := range map[string]string{
		"bare":                `"settings":{` + fragment + `}`,
		"with harmless parts": `"settings":{` + fragment + `,"domainStrategy":"UseIP","userLevel":0,"noises":[],"redirect":""},"streamSettings":{"network":"tcp","security":"none","sockopt":{"tcpNoDelay":true,"mark":255}}`,
	} {
		h := oneHop(t, fragmentFeed(fragmentRoot, freedom))
		if h.DialVia != "" || h.Transport.Kind != domain.WebSocket || !reflect.DeepEqual(h.Transport.TCPMasks, want) {
			t.Errorf("%s: dials via %q with %+v", name, h.DialVia, h.Transport.TCPMasks)
		}
	}
	for name, freedom := range map[string]string{
		"no fragment":        `"settings":{"domainStrategy":"UseIP"}`,
		"no settings":        `"settings":null`,
		"noises":             `"settings":{` + fragment + `,"noises":[{"type":"rand","packet":"10-20"}]}`,
		"a redirect":         `"settings":{` + fragment + `,"redirect":"127.0.0.1:53"}`,
		"a proxy protocol":   `"settings":{` + fragment + `,"proxyProtocol":1}`,
		"final rules":        `"settings":{` + fragment + `,"finalRules":[{"action":"block"}]}`,
		"an unknown part":    `"settings":{` + fragment + `,"padding":true}`,
		"a chained dialer":   `"settings":{` + fragment + `},"streamSettings":{"sockopt":{"dialerProxy":"other"}}`,
		"a chained proxy":    `"settings":{` + fragment + `},"proxySettings":{"tag":"other"}`,
		"a mask of its own":  `"settings":{` + fragment + `},"streamSettings":{"finalmask":{"tcp":[{"type":"fragment","settings":{"length":"5"}}]}}`,
		"a transport":        `"settings":{` + fragment + `},"streamSettings":{"network":"ws"}`,
		"tls":                `"settings":{` + fragment + `},"streamSettings":{"security":"tls"}`,
		"a length of zero":   `"settings":{"fragment":{"packets":"tlshello","length":"0","interval":"1"}}`,
		"no length":          `"settings":{"fragment":{"packets":"tlshello","interval":"1"}}`,
		"an unknown setting": `"settings":{"fragment":{"length":"5","interval":"1","host1_header":"x"}}`,
	} {
		refuses(t, name, fragmentFeed(fragmentRoot, freedom), CodeUnresolvedDialer, "dialerProxy")
	}
	refuses(t, "a blackhole dialer", strings.Replace(fragmentFeed(fragmentRoot, `"settings":{`+fragment+`}`), `"freedom"`, `"blackhole"`, 1), CodeUnresolvedDialer, "dialerProxy")
	hysteria := `"protocol":"hysteria","settings":{"version":2,"address":"hy.example.net","port":443},"streamSettings":{"network":"hysteria","security":"tls",
		"hysteriaSettings":{"version":2,"auth":"x"},"sockopt":{"dialerProxy":"frag"}}`
	refuses(t, "under Hysteria", fragmentFeed(hysteria, `"settings":{`+fragment+`}`), CodeUnresolvedDialer, "dialerProxy")

	// The outbound's own masks stay above the dialer's, and a chain's last hop is the one that dials through it.
	own := strings.Replace(fragmentRoot, `"sockopt"`, `"finalmask":{"tcp":[{"type":"fragment","settings":{"length":"7"}}]},"sockopt"`, 1)
	masks := oneHop(t, fragmentFeed(own, `"settings":{`+fragment+`}`)).Transport.TCPMasks
	if len(masks) != 2 || masks[0].Fragment.Length != "7" || !reflect.DeepEqual(masks[1:], want) {
		t.Errorf("masks = %+v, want its own and then the dialer's", masks)
	}
	chain := `{"outbounds":[{"tag":"exit","protocol":"trojan","settings":{"address":"e.example.net","port":443,"password":"pw"},"streamSettings":{"security":"tls","sockopt":{"dialerProxy":"proxy"}}},` +
		strings.TrimPrefix(fragmentFeed(fragmentRoot, `"settings":{`+fragment+`}`), `{"outbounds":[`)
	e, diags, err := parseOne(t, chain)
	if err != nil || len(diags) > 0 {
		t.Fatalf("chain: err = %v, diagnostics = %+v", err, diags)
	}
	if _, hops := e.Server.Closure(); len(hops) != 2 || hops[0].Transport.TCPMasks != nil || !reflect.DeepEqual(hops[1].Transport.TCPMasks, want) {
		t.Errorf("chain: hops = %+v", hops)
	}
}

// One server listed plain and through a fragment dialer is two, and the two ways of saying one
// fragment are one.
func TestAFragmentedServerIsItsOwn(t *testing.T) {
	plain := strings.Replace(fragmentRoot, `,"sockopt":{"dialerProxy":"frag"}`, "", 1)
	feed := func(outbounds string) []byte {
		return []byte(`{"outbounds":[{"tag":"proxy",` + plain + `},` + outbounds + `]}`)
	}
	dialer := feed(`{"tag":"proxy frag",` + fragmentRoot + `},{"tag":"frag","protocol":"freedom","settings":{"fragment":{"packets":"tlshello","length":"10-20","interval":"0"}}}`)
	doc, err := Parse(dialer, Detect, Limits{})
	if err != nil || len(doc.Entries()) != 2 {
		t.Fatalf("err = %v, entries = %d, want the plain server and the fragmented one", err, len(doc.Entries()))
	}
	a, b := doc.Entries()[0].Identity, doc.Entries()[1].Identity
	if a.Strong == b.Strong || a.Endpoint == b.Endpoint {
		t.Error("the plain server and the fragmented one have one identity")
	}
	r := newReconciled(t)
	r.apply(dialer)
	if len(r.lineages) != 2 {
		t.Errorf("lineages = %d, want 2", len(r.lineages))
	}
	native := strings.Replace(fragmentRoot, `"sockopt":{"dialerProxy":"frag"}`, `"finalmask":{"tcp":[{"type":"fragment","settings":{"packets":"tlshello","length":"10-20"}}]}`, 1)
	if got := identityOf(t, `{"outbounds":[{"tag":"x",`+native+`}]}`); got.Strong != b.Strong {
		t.Error("a fragment dialer and the same fragment mask are two servers")
	}
}

func TestParsePostQuantumReality(t *testing.T) {
	link := vlessLink("security=reality&pbk=" + realityKey + "&sni=www.example.com&fp=chrome&pqv=" + mldsaKey)
	xray := xrayVLESS(`"security":"reality","realitySettings":{"serverName":"www.example.com","publicKey":"` + realityKey + `","fingerprint":"chrome","mldsa65Verify":"` + mldsaKey + `"}`)
	for form, body := range map[string]string{"link": link, "xray": xray} {
		if h := oneHop(t, body); h.TLS.Reality == nil || h.TLS.Reality.MLDSA65Verify != mldsaKey {
			t.Errorf("%s: reality = %+v", form, h.TLS.Reality)
		}
	}
	if identityOf(t, link).Strong != identityOf(t, xray).Strong {
		t.Error("the two forms are two servers")
	}
	refuses(t, "a short key", strings.Replace(link, mldsaKey, mldsaKey[:100], 1), CodeMalformedEntry, "mldsa65Verify")
	refuses(t, "a short key in JSON", strings.Replace(xray, mldsaKey, mldsaKey[:100], 1), CodeMalformedEntry, "mldsa65Verify")
	refuses(t, "a padded key", strings.Replace(link, mldsaKey, mldsaKey+"%3D", 1), CodeMalformedEntry, "mldsa65Verify")
	refuses(t, "under TLS", vlessLink("security=tls&pqv="+mldsaKey), CodeUnsupportedOption, "mldsa65Verify")
}

func TestParseECH(t *testing.T) {
	q := url.QueryEscape(echList)
	pem := `"-----BEGIN ECH CONFIGS-----","AEX+DQBB/zAA","IAAg","-----END ECH CONFIGS-----"`
	singBox := func(ech string) string {
		return `{"outbounds":[{"type":"vless","tag":"x","server":"a.example.net","server_port":443,"uuid":"` + linkUUID + `",
			"tls":{"enabled":true,"server_name":"a.example.net","ech":` + ech + `}}]}`
	}
	clash := func(ech string) string {
		return "proxies:\n- {name: x, type: vless, server: a.example.net, port: 443, uuid: " + linkUUID + ", tls: true, servername: a.example.net, ech-opts: " + ech + "}\n"
	}
	forms := map[string]string{
		"ech":                  vlessLink("security=tls&sni=a.example.net&ech=" + q),
		"echConfigList":        vlessLink("security=tls&sni=a.example.net&echConfigList=" + q),
		"xray":                 xrayVLESS(`"security":"tls","tlsSettings":{"serverName":"a.example.net","echConfigList":"` + echList + `"}`),
		"clash":                clash(`{enable: true, config: ` + echList + `}`),
		"sing-box lines":       singBox(`{"enabled":true,"config":[` + pem + `]}`),
		"sing-box text":        singBox(`{"enabled":true,"config":"` + strings.ReplaceAll(strings.Trim(pem, `"`), `","`, `\n`) + `\n"}`),
		"sing-box bare base64": singBox(`{"enabled":true,"config":"` + echList + `"}`),
	}
	strong := map[domain.Digest]bool{}
	for form, body := range forms {
		if h := oneHop(t, body); h.TLS.ECHConfigList != echList || !h.TLS.Enabled {
			t.Errorf("%s: tls = %+v", form, h.TLS)
		}
		strong[identityOf(t, body).Strong] = true
	}
	if len(strong) != 1 {
		t.Errorf("the forms of one server have %d strong identities, want 1", len(strong))
	}
	if h := oneHop(t, "hy2://pw@hy.example.net?sni=hy.example.net&ech="+q+"#x"); h.TLS.ECHConfigList != echList {
		t.Errorf("hysteria2: tls = %+v", h.TLS)
	}
	if h := oneHop(t, vlessLink("security=tls&ech="+url.QueryEscape("example.com+https://1.1.1.1/dns-query"))); h.TLS.ECHConfigList != "example.com+https://1.1.1.1/dns-query" {
		t.Errorf("a resolver: tls = %+v", h.TLS)
	}
	for name, body := range map[string]string{"clash off": clash(`{enable: false, config: ` + echList + `}`), "sing-box off": singBox(`{"enabled":false,"config":5}`)} {
		if h := oneHop(t, body); h.TLS.ECHConfigList != "" {
			t.Errorf("%s: tls = %+v", name, h.TLS)
		}
	}
	// Xray reads tlsSettings under TLS alone.
	if h := oneHop(t, xrayVLESS(`"security":"none","tlsSettings":{"echConfigList":"`+echList+`"}`)); h.TLS.ECHConfigList != "" {
		t.Errorf("no TLS: tls = %+v", h.TLS)
	}
	refuses(t, "a raw plus", vlessLink("security=tls&ech="+echList), CodeMalformedEntry, "echConfigList")
	refuses(t, "over the length", vlessLink("security=tls&ech="+strings.Repeat("A", 4097)), CodeMalformedEntry, "echConfigList")
	refuses(t, "under Reality", vlessLink("security=reality&pbk="+realityKey+"&sni=s&ech="+q), CodeUnsupportedOption, "echConfigList")
	refuses(t, "without TLS", vlessLink("ech="+q), CodeUnsupportedOption, "echConfigList")
	refuses(t, "clash that asks a resolver", clash(`{enable: true}`), CodeUnsupportedOption, "ech-opts")
	refuses(t, "sing-box that asks a resolver", singBox(`{"enabled":true}`), CodeUnsupportedOption, "ech")
	refuses(t, "sing-box that names a file", singBox(`{"enabled":true,"config_path":"/ech.pem"}`), CodeUnsupportedOption, "ech")
	refuses(t, "sing-box with no text", singBox(`{"enabled":true,"config":5}`), CodeUnsupportedOption, "ech")
}

func TestParseVLESSEncryption(t *testing.T) {
	xray := `{"outbounds":[{"tag":"x","protocol":"vless","settings":{"vnext":[{"address":"a.example.net","port":443,"users":[{"id":"` + linkUUID +
		`","flow":"xtls-rprx-vision","encryption":"` + vlessEncrypted + `"}]}]},"streamSettings":{"network":"xhttp","xhttpSettings":{"path":"/x"}}}]}`
	forms := map[string]string{
		"link": vlessLink("type=xhttp&path=%2Fx&flow=xtls-rprx-vision&encryption=" + vlessEncrypted),
		"xray": xray,
		"clash": "proxies:\n- {name: x, type: vless, server: a.example.net, port: 443, uuid: " + linkUUID + ", flow: xtls-rprx-vision, encryption: " + vlessEncrypted +
			", network: xhttp, xhttp-opts: {path: /x}}\n",
	}
	strong := map[domain.Digest]bool{}
	for form, body := range forms {
		if h := oneHop(t, body); h.Protocol.VLESSEncryption().Reveal() != vlessEncrypted {
			t.Errorf("%s: encryption = %q", form, h.Protocol.VLESSEncryption().Reveal())
		}
		strong[identityOf(t, body).Strong] = true
	}
	if len(strong) != 1 {
		t.Errorf("the forms of one server have %d strong identities, want 1", len(strong))
	}
	if h := oneHop(t, vlessLink("security=tls&encryption=NONE")); !h.Protocol.VLESSEncryption().IsZero() {
		t.Error("none is an encryption")
	}
	for name, v := range map[string]string{"another scheme": "aes-128-gcm", "no key": "mlkem768x25519plus.native.0rtt.100-111-1111", "a bad key": vlessEncrypted + "A"} {
		refuses(t, name, vlessLink("security=tls&encryption="+v), CodeUnsupportedOption, "encryption")
	}
	refuses(t, "vision over xhttp without it", vlessLink("type=xhttp&flow=xtls-rprx-vision&security=tls"), CodeInvalidServer, "")
}

// Every new option tells servers apart, and a key its provider rotates leaves the endpoint's identity alone.
func TestEvasionOptionsAreIdentity(t *testing.T) {
	tcp := func(settings string) string {
		return vlessLink("security=tls&sni=s&fm=" + url.QueryEscape(`{"tcp":[`+settings+`]}`))
	}
	custom := func(list string, item string) string {
		return tcp(`{"type":"header-custom","settings":{"` + list + `":[[` + item + `]]}}`)
	}
	quic := func(params string) string {
		return "hy2://pw@hy.example.net?sni=s&fm=" + url.QueryEscape(`{"quicParams":{`+params+`}}`) + "#x"
	}
	bodies := []string{
		vlessLink("security=tls&sni=s"),
		tcp(`{"type":"fragment","settings":{"length":"5"}}`), tcp(`{"type":"fragment","settings":{"length":"6"}}`),
		tcp(`{"type":"fragment","settings":{"packets":"tlshello","length":"5"}}`), tcp(`{"type":"fragment","settings":{"length":"5","delay":"6"}}`),
		tcp(`{"type":"fragment","settings":{"length":"5","maxSplit":"6"}}`), tcp(`{"type":"fragment","settings":{"lengths":["5"]}}`),
		tcp(`{"type":"fragment","settings":{"length":"5","delays":["5"]}}`),
		tcp(`{"type":"fragment","settings":{"length":"5"}},{"type":"fragment","settings":{"length":"5"}}`),
		custom("clients", `{"rand":1}`), custom("servers", `{"rand":1}`), custom("errors", `{"rand":1}`), custom("clients", `{"rand":2}`),
		custom("clients", `{"rand":1,"randRange":"0-9"}`), custom("clients", `{"rand":1,"delay":1}`), custom("clients", `{"rand":1,"capture":"a"}`),
		custom("clients", `{"reuse":"a"}`), custom("clients", `{"packet":[1]}`), custom("clients", `{"rand":1},{"rand":1}`), custom("clients", `{"rand":1}],[{"rand":1}`),
		"hy2://pw@hy.example.net?sni=s#x",
		quic(`"congestion":"bbr"`), quic(`"bbrProfile":"standard"`), quic(`"brutalUp":"1g"`), quic(`"brutalDown":"1g"`), quic(`"brutalDisableLossCompensation":true`),
		quic(`"initStreamReceiveWindow":20000`), quic(`"maxStreamReceiveWindow":20000`), quic(`"initConnectionReceiveWindow":20000`), quic(`"maxConnectionReceiveWindow":20000`),
		quic(`"maxIdleTimeout":10`), quic(`"keepAlivePeriod":10`), quic(`"disablePathMTUDiscovery":true`), quic(`"disableChromeParrot":true`), quic(`"disableGSO":true`),
		quic(`"disableStatelessReset":true`), quic(`"maxIncomingStreams":10`),
		vlessLink("security=reality&sni=s&pbk=" + realityKey), vlessLink("security=reality&sni=s&pbk=" + realityKey + "&pqv=" + mldsaKey),
		vlessLink("security=tls&sni=s&ech=AEX"), vlessLink("security=tls&sni=s&encryption=" + vlessEncrypted),
	}
	strong, endpoint := map[domain.Digest]string{}, map[domain.Digest]string{}
	for _, body := range bodies {
		id := identityOf(t, body)
		if other, ok := strong[id.Strong]; ok {
			t.Errorf("one strong identity for\n%s\n%s", other, body)
		}
		if other, ok := endpoint[id.Endpoint]; ok {
			t.Errorf("one endpoint identity for\n%s\n%s", other, body)
		}
		strong[id.Strong], endpoint[id.Endpoint] = body, body
	}
	for name, pair := range map[string][2]string{
		"ech":        {vlessLink("security=tls&sni=s&ech=AEX"), vlessLink("security=tls&sni=s&ech=AEY")},
		"encryption": {vlessLink("security=tls&sni=s&encryption=" + vlessEncrypted), vlessLink("security=tls&sni=s&encryption=" + strings.Replace(vlessEncrypted, "CQ", "Cg", 1))},
	} {
		a, b := identityOf(t, pair[0]), identityOf(t, pair[1])
		if a.Strong == b.Strong || a.Endpoint != b.Endpoint {
			t.Errorf("%s: a rotated key is a new revision of one endpoint, got strong same %v, endpoint same %v", name, a.Strong == b.Strong, a.Endpoint == b.Endpoint)
		}
	}
}

// A stored server's digest is its reference, so one that uses nothing new keeps the digest it was stored under.
func TestIdentityWithoutEvasionOptionsIsUnchanged(t *testing.T) {
	for _, c := range []struct{ body, strong, endpoint string }{
		{"vless://" + linkUUID + "@de1.example.net:443?type=tcp&security=reality&sni=www.example.com&pbk=" + realityKey + "&sid=6ba85179e30d4fc2&fp=chrome&flow=xtls-rprx-vision#DE",
			"aceeeec26f75f89820e2471c8f434fcebf7efc37ac781d384dd5a501337f4130", "be84130f373b2c47b673738e77e2c4952b4234207b96c54d695cc87b1cb3bd5b"},
		{"trojan://pw@a.example.net:443?type=ws&path=%2Fws&host=cdn.example.net&sni=a.example.net&alpn=h2#T",
			"c419d7e4641283a5d2786089109b578ef033eae28a26a6c44265f830dab28c1f", "98dd15f66f3c1915a99b14a120ae8adf349d18991a9071f16730ba71b6db77e0"},
		{hysteria2Forms["link"],
			"fcbd40f1b753c29c1af90e633da36102270f5d009c6d001fa59eebe1ab612263", "bdecdccf2938298ed1b98c883187979e107961256e7d23422ad059a3b712aca6"},
	} {
		id := identityOf(t, c.body)
		if hex.EncodeToString(id.Strong[:]) != c.strong || hex.EncodeToString(id.Endpoint[:]) != c.endpoint {
			t.Errorf("%s: strong %x endpoint %x, want the digests of the build before", c.body, id.Strong, id.Endpoint)
		}
	}
}
