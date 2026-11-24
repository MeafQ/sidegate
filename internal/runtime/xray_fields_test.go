package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

// streamPath walks a rendered stream to one value, with "" when it is not there.
func streamPath(stream obj, path string) string {
	var at any = stream
	for key := range strings.SplitSeq(path, ".") {
		m, ok := at.(obj)
		if !ok {
			return ""
		}
		at = m[key]
	}
	if at == nil {
		return ""
	}
	return fmt.Sprint(at)
}

const sentinelNumber = 7919

// sentinelFor returns a value for a field that shows up wherever it renders.
func sentinelFor(field reflect.Value, name string) reflect.Value {
	text := "sentinel" + name
	switch field.Interface().(type) {
	case string:
		return reflect.ValueOf(text)
	case []string:
		return reflect.ValueOf([]string{text})
	case map[string]string:
		return reflect.ValueOf(map[string]string{"X-Sentinel": text})
	case domain.Secret:
		return reflect.ValueOf(domain.NewSecret(text))
	case bool:
		return reflect.ValueOf(true)
	case *domain.Reality:
		return reflect.ValueOf(&domain.Reality{PublicKey: text})
	case []domain.TCPMask:
		return reflect.ValueOf([]domain.TCPMask{{Type: text}})
	case *domain.QuicParams:
		return reflect.ValueOf(&domain.QuicParams{Congestion: text})
	case []byte:
		return reflect.ValueOf([]byte(text))
	case int32:
		return reflect.ValueOf(int32(sentinelNumber))
	case int64:
		return reflect.ValueOf(int64(sentinelNumber))
	case uint64:
		return reflect.ValueOf(uint64(sentinelNumber))
	}
	panic("no sentinel for field " + name + ": add one")
}

func vlessHopWith(t *testing.T, tr domain.Transport, tls domain.TLS) domain.Hop {
	t.Helper()
	p, err := domain.NewProtocol(domain.VLESS, domain.NewSecret("6f1c1e52-0d3c-4b8e-9a51-2c1f7c0a5e11"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return domain.Hop{ID: "root", Endpoint: domain.Endpoint{Host: "x.example", Port: 443}, Protocol: p, Transport: tr, TLS: tls}
}

// expression says where one field of one kind must render. An empty path means the
// kind cannot express it, so a server that sets it is refused. value overrides the
// sentinel for a field that takes only certain values; shows is what then renders.
type expression struct{ path, value, shows string }

// transportFields is every Transport field of every kind. A field or kind added to
// the domain has no entry here and fails below until someone decides what it means.
var transportFields = map[domain.TransportKind]map[string]expression{
	domain.TCP: {"Path": {}, "Host": {}, "Service": {}, "Mode": {}, "Headers": {}, "Salamander": {}, "TCPMasks": {path: "finalmask.tcp"}, "QuicParams": {}},
	domain.WebSocket: {"Path": {path: "wsSettings.path"}, "Host": {path: "wsSettings.host"}, "Service": {}, "Mode": {},
		"Headers": {path: "wsSettings.headers"}, "Salamander": {}, "TCPMasks": {path: "finalmask.tcp"}, "QuicParams": {}},
	domain.GRPC: {"Path": {}, "Host": {path: "grpcSettings.authority"}, "Service": {path: "grpcSettings.serviceName"},
		"Mode": {path: "grpcSettings.multiMode", value: "multi", shows: "true"}, "Headers": {}, "Salamander": {}, "TCPMasks": {path: "finalmask.tcp"}, "QuicParams": {}},
	domain.XHTTP: {"Path": {path: "xhttpSettings.path"}, "Host": {path: "xhttpSettings.host"}, "Service": {},
		"Mode": {path: "xhttpSettings.mode"}, "Headers": {path: "xhttpSettings.headers"}, "Salamander": {}, "TCPMasks": {path: "finalmask.tcp"}, "QuicParams": {}},
	domain.Hysteria: {"Path": {}, "Host": {}, "Service": {}, "Mode": {}, "Headers": {}, "Salamander": {path: "finalmask.udp"},
		"TCPMasks": {}, "QuicParams": {path: "finalmask.quicParams"}},
}

func TestEveryTransportFieldRendersOrRefusesTheServer(t *testing.T) {
	for kind := domain.TCP; kind <= domain.Hysteria; kind++ {
		fields, ok := transportFields[kind]
		if !ok {
			t.Errorf("transport %d has no entry: say what each of its fields renders to", kind)
			continue
		}
		typ := reflect.TypeFor[domain.Transport]()
		for i := range typ.NumField() {
			name := typ.Field(i).Name
			if name == "Kind" {
				continue
			}
			want, ok := fields[name]
			if !ok {
				t.Errorf("transport %d: field %s has no entry: render it or refuse it", kind, name)
				continue
			}
			tr := domain.Transport{Kind: kind}
			field := reflect.ValueOf(&tr).Elem().Field(i)
			field.Set(sentinelFor(field, name))
			shows := "sentinel" + name
			if want.value != "" {
				field.SetString(want.value)
				shows = want.shows
			}
			stream, err := renderStream(vlessHopWith(t, tr, domain.TLS{}))
			switch {
			case want.path == "" && err == nil:
				t.Errorf("transport %d rendered %s=%v, which it cannot express, without refusing: %v", kind, name, field, stream)
			case want.path != "" && err != nil:
				t.Errorf("transport %d %s: %v", kind, name, err)
			case want.path != "" && !strings.Contains(streamPath(stream, want.path), shows):
				t.Errorf("transport %d dropped %s: %s is %q in %v", kind, name, want.path, streamPath(stream, want.path), stream)
			}
		}
	}
}

// tlsFields is every TLS field in each mode, keyed as transportFields is. Reality's
// own fields are in realityFields.
var tlsFields = map[string]map[string]string{
	"none": {"ServerName": "", "ALPN": "", "Fingerprint": "", "PinnedCertSHA256": "", "VerifyNames": "", "ECHConfigList": ""},
	"tls": {"ServerName": "tlsSettings.serverName", "ALPN": "tlsSettings.alpn", "Fingerprint": "tlsSettings.fingerprint",
		"PinnedCertSHA256": "tlsSettings.pinnedPeerCertSha256", "VerifyNames": "tlsSettings.verifyPeerCertByName",
		"ECHConfigList": "tlsSettings.echConfigList"},
	"reality": {"ServerName": "realitySettings.serverName", "ALPN": "", "Fingerprint": "realitySettings.fingerprint",
		"PinnedCertSHA256": "", "VerifyNames": "", "ECHConfigList": ""},
}

var realityFields = map[string]string{"PublicKey": "realitySettings.publicKey", "ShortID": "realitySettings.shortId", "SpiderX": "realitySettings.spiderX",
	"MLDSA65Verify": "realitySettings.mldsa65Verify"}

func TestEveryTLSFieldRendersOrRefusesTheServer(t *testing.T) {
	typ := reflect.TypeFor[domain.TLS]()
	for mode, fields := range tlsFields {
		for i := range typ.NumField() {
			name := typ.Field(i).Name
			if name == "Enabled" || name == "Reality" {
				continue // These two choose the mode.
			}
			path, ok := fields[name]
			if !ok {
				t.Errorf("TLS mode %s: field %s has no entry: render it or refuse it", mode, name)
				continue
			}
			tls := domain.TLS{Enabled: mode != "none"}
			if mode == "reality" {
				tls.Reality = &domain.Reality{PublicKey: "key"}
			}
			field := reflect.ValueOf(&tls).Elem().Field(i)
			field.Set(sentinelFor(field, name))
			stream, err := renderStream(vlessHopWith(t, domain.Transport{Kind: domain.TCP}, tls))
			switch {
			case path == "" && err == nil:
				t.Errorf("TLS mode %s rendered %s without refusing: %v", mode, name, stream)
			case path != "" && err != nil:
				t.Errorf("TLS mode %s %s: %v", mode, name, err)
			case path != "" && !strings.Contains(streamPath(stream, path), "sentinel"+name):
				t.Errorf("TLS mode %s dropped %s: %s is %q", mode, name, path, streamPath(stream, path))
			}
		}
	}
	reality := reflect.TypeFor[domain.Reality]()
	for i := range reality.NumField() {
		name := reality.Field(i).Name
		path, ok := realityFields[name]
		if !ok {
			t.Errorf("Reality field %s has no entry", name)
			continue
		}
		r := domain.Reality{}
		reflect.ValueOf(&r).Elem().Field(i).SetString("sentinel" + name)
		stream, err := renderStream(vlessHopWith(t, domain.Transport{Kind: domain.TCP}, domain.TLS{Enabled: true, Reality: &r}))
		if err != nil {
			t.Errorf("Reality %s: %v", name, err)
		} else if !strings.Contains(streamPath(stream, path), "sentinel"+name) {
			t.Errorf("Reality dropped %s: %s is %q", name, path, streamPath(stream, path))
		}
	}
}

// maskFields is the key each field of a mask renders under. A field added to one has no entry here
// and fails below.
var maskFields = map[reflect.Type]map[string]string{
	reflect.TypeFor[domain.FragmentMask](): {"Packets": "packets", "Length": "length", "Delay": "delay", "Lengths": "lengths", "Delays": "delays", "MaxSplit": "maxSplit"},
	reflect.TypeFor[domain.CustomTCPItem](): {"Delay": "delay", "Rand": "rand", "RandRange": "randRange", "Capture": "capture", "Reuse": "reuse",
		"Packet": "packet"},
	reflect.TypeFor[domain.QuicParams](): {"Congestion": "congestion", "BBRProfile": "bbrProfile", "BrutalUp": "brutalUp", "BrutalDown": "brutalDown",
		"BrutalDisableLossCompensation": "brutalDisableLossCompensation", "InitStreamReceiveWindow": "initStreamReceiveWindow",
		"MaxStreamReceiveWindow": "maxStreamReceiveWindow", "InitConnectionReceiveWindow": "initConnectionReceiveWindow",
		"MaxConnectionReceiveWindow": "maxConnectionReceiveWindow", "MaxIdleTimeout": "maxIdleTimeout", "KeepAlivePeriod": "keepAlivePeriod",
		"DisablePathMTUDiscovery": "disablePathMTUDiscovery", "DisableChromeParrot": "disableChromeParrot", "DisableGSO": "disableGSO",
		"DisableStatelessReset": "disableStatelessReset", "MaxIncomingStreams": "maxIncomingStreams"},
}

func TestEveryMaskFieldRenders(t *testing.T) {
	render := func(v reflect.Value) map[string]any {
		hop := vlessHopWith(t, domain.Transport{Kind: domain.TCP}, domain.TLS{})
		switch v := v.Interface().(type) {
		case domain.FragmentMask:
			hop.Transport.TCPMasks = []domain.TCPMask{{Type: domain.MaskFragment, Fragment: &v}}
		case domain.CustomTCPItem:
			hop.Transport.TCPMasks = []domain.TCPMask{{Type: domain.MaskHeaderCustom, HeaderCustom: &domain.HeaderCustomTCP{Servers: [][]domain.CustomTCPItem{{v}}}}}
		case domain.QuicParams:
			hop.Transport.Kind, hop.Transport.QuicParams = domain.Hysteria, &v
		}
		stream, err := renderStream(hop)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(stream["finalmask"])
		if err != nil {
			t.Fatal(err)
		}
		var mask struct {
			TCP []struct {
				Settings map[string]any
			}
			QuicParams map[string]any
		}
		if err := json.Unmarshal(body, &mask); err != nil {
			t.Fatal(err)
		}
		if mask.QuicParams != nil {
			return mask.QuicParams
		}
		settings := mask.TCP[0].Settings
		if servers, ok := settings["servers"]; ok {
			return servers.([]any)[0].([]any)[0].(map[string]any)
		}
		return settings
	}
	for typ, keys := range maskFields {
		for i := range typ.NumField() {
			name := typ.Field(i).Name
			key, ok := keys[name]
			if !ok {
				t.Errorf("%s: field %s has no entry", typ, name)
				continue
			}
			v := reflect.New(typ).Elem()
			v.Field(i).Set(sentinelFor(v.Field(i), name))
			want := map[string]any{key: "sentinel" + name}
			switch v.Field(i).Interface().(type) {
			case bool:
				want[key] = true
			case int32, int64, uint64:
				want[key] = float64(sentinelNumber)
			case []string:
				want[key] = []any{"sentinel" + name}
			case []byte:
				want["type"], want[key] = "base64", base64.StdEncoding.EncodeToString([]byte("sentinel"+name))
			}
			if got := render(v); !reflect.DeepEqual(got, want) {
				t.Errorf("%s.%s renders %v, want %v alone", typ, name, got, want)
			}
		}
	}
}

func TestSalamanderAndQuicShareOneFinalmask(t *testing.T) {
	hop := vlessHopWith(t, domain.Transport{Kind: domain.Hysteria, Salamander: domain.NewSecret("obfs"), QuicParams: &domain.QuicParams{Congestion: "bbr"}}, domain.TLS{Enabled: true})
	stream, err := renderStream(hop)
	if err != nil {
		t.Fatal(err)
	}
	want := obj{"udp": []obj{{"type": "salamander", "settings": obj{"password": "obfs"}}}, "quicParams": obj{"congestion": "bbr"}}
	if !reflect.DeepEqual(stream["finalmask"], want) {
		t.Errorf("finalmask %v, want %v", stream["finalmask"], want)
	}
}

func TestAVLESSHopCarriesItsEncryption(t *testing.T) {
	encryption := "mlkem768x25519plus.native.0rtt." + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	for _, want := range []string{"none", encryption} {
		hop := vlessHopWith(t, domain.Transport{Kind: domain.TCP}, domain.TLS{})
		var err error
		if hop.Protocol, err = hop.Protocol.WithVLESSEncryption(want); err != nil {
			t.Fatal(err)
		}
		out, err := renderHop("t", hop)
		if err != nil {
			t.Fatal(err)
		}
		if got := out["settings"].(obj)["vnext"].([]obj)[0]["users"].([]obj)[0]["encryption"]; got != want {
			t.Errorf("encryption %v, want %s", got, want)
		}
		if got := redactCredentials("unsupported "+encryption, testServer(t, hop)); want == encryption && strings.Contains(got, encryption) {
			t.Errorf("the reason quotes the encryption: %q", got)
		}
	}
}

func TestAHysteriaHopCarriesItsAuthAndOffersH3(t *testing.T) {
	p, err := domain.NewProtocol(domain.Hysteria2, domain.NewSecret("p@ss"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	hop := func(alpn ...string) domain.Hop {
		return domain.Hop{ID: "root", Endpoint: domain.Endpoint{Host: "x.example", Port: 8443}, Protocol: p,
			Transport: domain.Transport{Kind: domain.Hysteria}, TLS: domain.TLS{Enabled: true, ServerName: "x.example", ALPN: alpn}}
	}
	out, err := renderHop("t", hop())
	if err != nil {
		t.Fatal(err)
	}
	if out["protocol"] != "hysteria" || fmt.Sprint(out["settings"].(obj)["version"]) != "2" || out["settings"].(obj)["address"] != "x.example" {
		t.Errorf("outbound %v", out)
	}
	stream := out["streamSettings"].(obj)
	if got := streamPath(stream, "hysteriaSettings.auth"); got != "p@ss" {
		t.Errorf("auth %q", got)
	}
	if got := streamPath(stream, "tlsSettings.alpn"); got != "[h3]" {
		t.Errorf("no ALPN given: rendered %q, want h3 or the server refuses the handshake", got)
	}
	if _, err := renderHop("t", hop("h3", "h2")); err != nil {
		t.Errorf("ALPN naming h3: %v", err)
	}
	if _, err := renderHop("t", hop("h2")); err == nil {
		t.Error("ALPN without h3 rendered, and it can never connect")
	}
	short := hop()
	short.Transport.Salamander = domain.NewSecret("abc")
	if _, err := renderHop("t", short); err == nil {
		t.Error("a Salamander password the core fails on at every dial rendered")
	}
}

func TestSetFieldsNamesNoCredential(t *testing.T) {
	names := setFields(domain.Transport{Salamander: domain.NewSecret("hunter2")})
	if !slices.Equal(names, []string{"Salamander"}) {
		t.Errorf("names %q", names)
	}
}
