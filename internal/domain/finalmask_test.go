package domain

import (
	"crypto/mlkem"
	"encoding/base64"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func fragment(f FragmentMask) TCPMask { return TCPMask{Type: MaskFragment, Fragment: &f} }

func custom(items ...CustomTCPItem) TCPMask {
	return TCPMask{Type: MaskHeaderCustom, HeaderCustom: &HeaderCustomTCP{Clients: [][]CustomTCPItem{items}}}
}

func TestTCPMaskValid(t *testing.T) {
	sequences := func(n int) TCPMask {
		return TCPMask{Type: MaskHeaderCustom, HeaderCustom: &HeaderCustomTCP{Servers: make([][]CustomTCPItem, n)}}
	}
	for _, c := range []struct {
		name string
		mask TCPMask
		ok   bool
	}{
		{"fragment of the hello", fragment(FragmentMask{Packets: "tlshello", Length: "100-200", Delay: "10-20", MaxSplit: "3"}), true},
		{"fragment of every packet", fragment(FragmentMask{Length: "1"}), true},
		{"fragment of a packet range", fragment(FragmentMask{Packets: "1-3", Length: "1-1", Delay: "0"}), true},
		{"lengths per segment", fragment(FragmentMask{Lengths: []string{"0", "5-10"}, Delays: []string{"0", "10"}}), true},
		{"lengths win over length", fragment(FragmentMask{Length: "0", Lengths: []string{"5"}}), true},
		{"sixteen lengths", fragment(FragmentMask{Lengths: slices.Repeat([]string{"5"}, 16)}), true},
		{"seventeen lengths", fragment(FragmentMask{Lengths: slices.Repeat([]string{"5"}, 17)}), false},
		{"seventeen delays", fragment(FragmentMask{Length: "5", Delays: slices.Repeat([]string{"5"}, 17)}), false},
		{"no length", fragment(FragmentMask{Packets: "tlshello"}), false},
		{"a length from zero", fragment(FragmentMask{Length: "0-5"}), false},
		{"a last length from zero", fragment(FragmentMask{Length: "5", Lengths: []string{"5", "0-5"}}), false},
		{"packets from zero", fragment(FragmentMask{Packets: "0-3", Length: "5"}), false},
		{"packets backwards", fragment(FragmentMask{Packets: "3-1", Length: "5"}), false},
		{"packets by name", fragment(FragmentMask{Packets: "http", Length: "5"}), false},
		{"a negative delay", fragment(FragmentMask{Length: "5", Delay: "-5"}), false},
		{"a delay backwards", fragment(FragmentMask{Length: "5", Delay: "5-1"}), false},
		{"a delay over int32", fragment(FragmentMask{Length: "5", Delay: "2147483648"}), false},
		{"a split that is no number", fragment(FragmentMask{Length: "5", MaxSplit: "x"}), false},
		{"fragment without settings", TCPMask{Type: MaskFragment}, false},
		{"both settings", TCPMask{Type: MaskFragment, Fragment: &FragmentMask{Length: "5"}, HeaderCustom: &HeaderCustomTCP{}}, false},
		{"another mask", TCPMask{Type: "sudoku", Fragment: &FragmentMask{Length: "5"}}, false},

		{"a packet", custom(CustomTCPItem{Packet: []byte{0, 255}, Delay: "1-2"}), true},
		{"random bytes", custom(CustomTCPItem{Rand: 4096, RandRange: "0-255", Capture: "_a1"}), true},
		{"a captured value", custom(CustomTCPItem{Reuse: "a"}), true},
		{"a delay alone", custom(CustomTCPItem{Delay: "10"}), true},
		{"an empty sequence", sequences(1), true},
		{"sixteen sequences", sequences(16), true},
		{"seventeen sequences", sequences(17), false},
		{"no sequence", sequences(0), false},
		{"seventeen items", custom(slices.Repeat([]CustomTCPItem{{Rand: 1}}, 17)...), false},
		{"nothing at all", custom(CustomTCPItem{}), false},
		{"a capture of nothing", custom(CustomTCPItem{Delay: "1", Capture: "a"}), false},
		{"two kinds", custom(CustomTCPItem{Rand: 1, Packet: []byte{1}}), false},
		{"a packet too long", custom(CustomTCPItem{Packet: make([]byte, 4097)}), false},
		{"too many random bytes", custom(CustomTCPItem{Rand: 4097}), false},
		{"negative random bytes", custom(CustomTCPItem{Rand: -1, Delay: "1"}), false},
		{"a random range over a byte", custom(CustomTCPItem{Rand: 1, RandRange: "0-256"}), false},
		{"a random range of nothing", custom(CustomTCPItem{Reuse: "a", RandRange: "0-5"}), false},
		{"a variable name", custom(CustomTCPItem{Reuse: "1a"}), false},
		{"a variable name with a newline", custom(CustomTCPItem{Reuse: "a\n"}), false},
		{"a delay that is no range", custom(CustomTCPItem{Rand: 1, Delay: "x"}), false},
	} {
		if got := c.mask.Valid(); got != c.ok {
			t.Errorf("%s: valid = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestQuicParamsValid(t *testing.T) {
	for _, c := range []struct {
		name string
		q    QuicParams
		ok   bool
	}{
		{"zero", QuicParams{}, true},
		{"everything", QuicParams{Congestion: "force-brutal", BBRProfile: "aggressive", BrutalUp: "100 mbps", BrutalDown: "1g",
			InitStreamReceiveWindow: 16384, MaxStreamReceiveWindow: 16384, InitConnectionReceiveWindow: 16384, MaxConnectionReceiveWindow: 1 << 63,
			MaxIdleTimeout: 120, KeepAlivePeriod: 60, MaxIncomingStreams: 8}, true},
		{"the shortest times", QuicParams{MaxIdleTimeout: 4, KeepAlivePeriod: 2}, true},
		{"force-brutal without a rate", QuicParams{Congestion: "force-brutal"}, false},
		{"cubic", QuicParams{Congestion: "cubic"}, false},
		{"upper case", QuicParams{Congestion: "BBR"}, false},
		{"a profile", QuicParams{BBRProfile: "turbo"}, false},
		{"brutal at the floor", QuicParams{BrutalUp: "524288"}, true},
		{"brutal under the floor", QuicParams{BrutalUp: "524287"}, false},
		{"brutal down under the floor", QuicParams{BrutalDown: "1kbps"}, false},
		{"brutal at zero", QuicParams{BrutalUp: "0"}, false},
		{"a small window", QuicParams{InitStreamReceiveWindow: 16383}, false},
		{"a small connection window", QuicParams{MaxConnectionReceiveWindow: 1}, false},
		{"a short idle", QuicParams{MaxIdleTimeout: 3}, false},
		{"a long idle", QuicParams{MaxIdleTimeout: 121}, false},
		{"a short keepalive", QuicParams{KeepAlivePeriod: 1}, false},
		{"a long keepalive", QuicParams{KeepAlivePeriod: 61}, false},
		{"a negative idle", QuicParams{MaxIdleTimeout: -5}, false},
		{"few streams", QuicParams{MaxIncomingStreams: 7}, false},
		{"negative streams", QuicParams{MaxIncomingStreams: -8}, false},
	} {
		if got := c.q.Valid(); got != c.ok {
			t.Errorf("%s: valid = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestBandwidth(t *testing.T) {
	for s, want := range map[string]uint64{"1000000": 125000, "8b": 1, "8 bps": 1, "8k": 1024, "8kb": 1024, "8 kbps": 1024, "100 mbps": 100 << 20 / 8,
		"1.5g": 3 << 30 / 16, "1tbps": 1 << 40 / 8, "0": 0} {
		if got, ok := Bandwidth(s); !ok || got != want {
			t.Errorf("%q = %d %v, want %d", s, got, ok, want)
		}
	}
	for _, s := range []string{"", "mbps", "-1", "1 mb/s", "1 Mbps", "1.2.3", "1pbps", "1e9", "nan", "inf", "99999999999999999999 tbps", "1\nmbps", "1 mbps "} {
		if got, ok := Bandwidth(s); ok {
			t.Errorf("%q = %d, want it refused", s, got)
		}
	}
}

func TestVLESSEncryption(t *testing.T) {
	dk, err := mlkem.GenerateKey768()
	if err != nil {
		t.Fatal(err)
	}
	x25519 := base64.RawURLEncoding.EncodeToString(slices.Repeat([]byte{9}, 32))
	kem := base64.RawURLEncoding.EncodeToString(dk.EncapsulationKey().Bytes())
	const native = "mlkem768x25519plus.native.0rtt."
	vless, err := NewProtocol(VLESS, NewSecret(testUUID), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{native + x25519, native + kem, native + x25519 + "." + kem, "mlkem768x25519plus.xorpub.1rtt." + x25519,
		"mlkem768x25519plus.random.1rtt.100-111-1111.75-0-111.50-0-3333." + kem} {
		p, err := vless.WithVLESSEncryption(s)
		if err != nil || p.VLESSEncryption().Reveal() != s {
			t.Errorf("%.60s: %q, %v", s, p.VLESSEncryption(), err)
		}
	}
	for _, s := range []string{"", "none", "None"} {
		p, err := vless.WithVLESSEncryption(s)
		if err != nil || p != vless {
			t.Errorf("%q is no encryption: %v", s, err)
		}
	}
	for name, s := range map[string]string{
		"another scheme":                     "aes-128-gcm",
		"no key part":                        "mlkem768x25519plus.native.0rtt",
		"padding and no key, a core panic":   native + "100-111-1111",
		"one short part and no key":          native + "abc",
		"another mode":                       "mlkem768x25519plus.plain.0rtt." + x25519,
		"another handshake":                  "mlkem768x25519plus.native.600s." + x25519,
		"another key length":                 native + x25519[:40],
		"a key that is not base64url":        native + strings.Repeat("+", 43),
		"padding after the key":              native + x25519 + ".100-111-1111",
		"padding of two numbers":             native + "100-111." + x25519,
		"padding that is no number":          native + "a-b-c." + x25519,
		"a first padding that may be absent": native + "50-111-1111." + x25519,
		"a short first padding":              native + "100-34-1111." + x25519,
		"padding over the limit":             native + "100-35-65554." + x25519,
		"a space":                            native + x25519 + " ",
		"a newline":                          native + x25519[:20] + "\n" + x25519[20:],
		"over the length":                    native + x25519 + "." + kem + "." + kem + "." + kem,
	} {
		if _, err := vless.WithVLESSEncryption(s); !errors.Is(err, ErrInvalidProtocol) {
			t.Errorf("%s: err = %v, want ErrInvalidProtocol", name, err)
		}
	}
	trojan, _ := NewProtocol(Trojan, NewSecret("pw"), "", "", "")
	if _, err := trojan.WithVLESSEncryption(native + x25519); !errors.Is(err, ErrInvalidProtocol) {
		t.Errorf("trojan took VLESS encryption: %v", err)
	}
	if p, err := trojan.WithVLESSEncryption("none"); err != nil || p != trojan {
		t.Errorf("trojan with none: %v", err)
	}
}

func TestNewServerHoldsEvasionOptionsToTheirHops(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(make([]byte, 1952))
	reality := func(verify string) func(*Hop) {
		return func(h *Hop) { h.TLS.Reality = &Reality{PublicKey: "pk", MLDSA65Verify: verify} }
	}
	ech := func(v string) func(*Hop) { return func(h *Hop) { h.TLS.ECHConfigList = v } }
	mask := fragment(FragmentMask{Length: "5"})
	masks := func(kind TransportKind, m ...TCPMask) func(*Hop) {
		return func(h *Hop) { h.Transport = Transport{Kind: kind, TCPMasks: m} }
	}
	encrypted := func(kind TransportKind, tls bool) func(*Hop) {
		return func(h *Hop) {
			p, _ := NewProtocol(VLESS, NewSecret(testUUID), "", "", VisionFlow)
			p, err := p.WithVLESSEncryption("mlkem768x25519plus.native.0rtt." + base64.RawURLEncoding.EncodeToString(slices.Repeat([]byte{9}, 32)))
			if err != nil {
				t.Fatal(err)
			}
			h.Protocol, h.Transport.Kind = p, kind
			if !tls {
				h.TLS = TLS{}
			}
		}
	}
	for _, c := range []struct {
		name     string
		hysteria bool
		change   func(*Hop)
		ok       bool
	}{
		{"masks on tcp", false, masks(TCP, mask), true},
		{"masks on websocket", false, masks(WebSocket, mask), true},
		{"masks on grpc", false, masks(GRPC, mask), true},
		{"masks on xhttp", false, masks(XHTTP, mask), true},
		{"masks on hysteria", true, masks(Hysteria, mask), false},
		{"four masks", false, masks(TCP, mask, mask, mask, mask), true},
		{"five masks", false, masks(TCP, mask, mask, mask, mask, mask), false},
		{"an invalid mask", false, masks(TCP, mask, fragment(FragmentMask{})), false},
		{"quic on hysteria", true, func(h *Hop) { h.Transport.QuicParams = &QuicParams{Congestion: "bbr"} }, true},
		{"invalid quic on hysteria", true, func(h *Hop) { h.Transport.QuicParams = &QuicParams{Congestion: "cubic"} }, false},
		{"quic on tcp", false, func(h *Hop) { h.Transport.QuicParams = &QuicParams{Congestion: "bbr"} }, false},
		{"a post-quantum key", false, reality(key), true},
		{"a short post-quantum key", false, reality(base64.RawURLEncoding.EncodeToString(make([]byte, 1951))), false},
		{"a padded post-quantum key", false, reality(key + "="), false},
		{"a post-quantum key over lines", false, reality(key[:10] + "\n" + key[10:]), false},
		{"ech", false, ech("example.com+https://1.1.1.1/dns-query"), true},
		{"ech on hysteria", true, ech("AEX+DQBB"), true},
		{"ech at its limit", false, ech(strings.Repeat("A", 4096)), true},
		{"ech over its limit", false, ech(strings.Repeat("A", 4097)), false},
		{"ech with a space", false, ech("AEX DQBB"), false},
		{"ech outside ascii", false, ech("AEXé"), false},
		{"ech with a control character", false, ech("AEX\x7f"), false},
		{"ech without tls", false, func(h *Hop) { h.TLS = TLS{ECHConfigList: "AEX+DQBB"} }, false},
		{"ech under reality", false, func(h *Hop) { reality("")(h); ech("AEX+DQBB")(h) }, false},
		{"vision over encryption without tls", false, encrypted(TCP, false), true},
		{"vision over encryption on xhttp", false, encrypted(XHTTP, true), true},
	} {
		h := vlessHop(t, "a", "")
		if c.hysteria {
			h = hysteriaHop(t)
		}
		c.change(&h)
		if _, err := NewServer(h.ID, []Hop{h}); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestAServerDetachesItsMasks(t *testing.T) {
	build := func() (Hop, Hop) {
		h := vlessHop(t, "a", "")
		h.Transport.TCPMasks = []TCPMask{fragment(FragmentMask{Lengths: []string{"5"}, Delays: []string{"6"}}), custom(CustomTCPItem{Packet: []byte{1}})}
		q := hysteriaHop(t)
		q.Transport.QuicParams = &QuicParams{Congestion: "bbr"}
		return h, q
	}
	spoil := func(h, q Hop) {
		h.Transport.TCPMasks[0].Fragment.Lengths[0] = "7"
		h.Transport.TCPMasks[0].Fragment.Delays[0] = "7"
		h.Transport.TCPMasks[1].HeaderCustom.Clients[0][0].Packet[0] = 7
		h.Transport.TCPMasks[1].HeaderCustom.Clients[0][0].Rand = 7
		h.Transport.TCPMasks[1].HeaderCustom.Clients[0] = nil
		h.Transport.TCPMasks[0] = TCPMask{}
		q.Transport.QuicParams.Congestion = "reno"
	}
	h, q := build()
	masked, err1 := NewServer("a", []Hop{h})
	tuned, err2 := NewServer("h", []Hop{q})
	if err := errors.Join(err1, err2); err != nil {
		t.Fatal(err)
	}
	hops := func() (Hop, Hop) {
		_, a := masked.Closure()
		_, b := tuned.Closure()
		return a[0], b[0]
	}
	spoil(h, q)
	spoil(hops())
	wantMasks, wantQuic := build()
	gotMasks, gotQuic := hops()
	if !reflect.DeepEqual(gotMasks.Transport, wantMasks.Transport) || !reflect.DeepEqual(gotQuic.Transport, wantQuic.Transport) {
		t.Errorf("a caller's write reached the server: %+v %+v", gotMasks.Transport, gotQuic.Transport)
	}
}
