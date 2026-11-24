package domain

import (
	"errors"
	"testing"
	"time"
)

const testUUID = "11111111-2222-4333-8444-555555555555"

func vlessHop(t *testing.T, id, via HopID) Hop {
	t.Helper()
	p, err := NewProtocol(VLESS, NewSecret(testUUID), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return Hop{ID: id, Endpoint: Endpoint{Host: "edge.example.net", Port: 443}, Protocol: p,
		Transport: Transport{Kind: TCP}, TLS: TLS{Enabled: true, ServerName: "edge.example.net"}, DialVia: via}
}

func TestNewProtocol(t *testing.T) {
	cases := []struct {
		name                     string
		p                        Protocol
		cred, user, cipher, flow string
		ok                       bool
	}{
		{"vless vision", VLESS, testUUID, "", "", VisionFlow, true},
		{"vless short id string", VLESS, "alice", "", "", "", true},
		{"vless unknown flow", VLESS, testUUID, "", "", "xtls-rprx-direct", false},
		{"vless empty id", VLESS, "", "", "", "", false},
		{"vmess auto", VMess, testUUID, "", "auto", "", true},
		{"vmess unknown cipher", VMess, testUUID, "", "rc4", "", false},
		{"trojan", Trojan, "secret", "", "", "", true},
		{"trojan flow", Trojan, "secret", "", "", VisionFlow, false},
		{"ss 2022", Shadowsocks, "a2V5", "", "2022-blake3-aes-128-gcm", "", true},
		{"ss stream cipher", Shadowsocks, "pw", "", "aes-256-cfb", "", false},
		{"socks anonymous", SOCKS, "", "", "", "", true},
		{"socks password without user", SOCKS, "pw", "", "", "", false},
		{"hysteria2", Hysteria2, "auth", "", "", "", true},
		{"hysteria2 without auth", Hysteria2, "", "", "", "", false},
		{"unknown protocol", Protocol(99), testUUID, "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewProtocol(c.p, NewSecret(c.cred), c.user, c.cipher, c.flow)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidProtocol) {
				t.Fatalf("err = %v, want ErrInvalidProtocol", err)
			}
		})
	}
}

func TestNewServerClosure(t *testing.T) {
	exit, relay := vlessHop(t, "exit", "relay"), vlessHop(t, "relay", "")
	exit.Transport.Headers = map[string]string{"Host": "a"}
	s, err := NewServer("exit", []Hop{relay, exit})
	if err != nil {
		t.Fatal(err)
	}
	exit.Transport.Headers["Host"] = "mutated"
	root, hops := s.Closure()
	if root != "exit" || len(hops) != 2 || hops[0].ID != "exit" || hops[1].ID != "relay" {
		t.Fatalf("closure = %q %+v, want exit then relay", root, hops)
	}
	if hops[0].Transport.Headers["Host"] != "a" {
		t.Fatal("server shares the caller's header map")
	}
	hops[0].TLS.ServerName = "mutated"
	if _, again := s.Closure(); again[0].TLS.ServerName != "edge.example.net" {
		t.Fatal("closure is not detached")
	}
}

func hysteriaHop(t *testing.T) Hop {
	t.Helper()
	p, err := NewProtocol(Hysteria2, NewSecret("auth"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return Hop{ID: "h", Endpoint: Endpoint{Host: "hy.example.net", Port: 443}, Protocol: p,
		Transport: Transport{Kind: Hysteria, Salamander: NewSecret("ob")}, TLS: TLS{Enabled: true, ServerName: "hy.example.net"}}
}

func TestNewServerAcceptsHysteria2(t *testing.T) {
	if _, err := NewServer("h", []Hop{hysteriaHop(t)}); err != nil {
		t.Fatal(err)
	}
}

func TestNewServerRejects(t *testing.T) {
	reality := vlessHop(t, "r", "")
	reality.TLS.Reality = &Reality{PublicKey: "pk"}
	reality.Transport.Kind = WebSocket
	vision := vlessHop(t, "v", "")
	vision.Protocol, _ = NewProtocol(VLESS, NewSecret(testUUID), "", "", VisionFlow)
	vision.Transport.Kind = GRPC
	pinnedPlain := vlessHop(t, "p", "")
	pinnedPlain.TLS = TLS{PinnedCertSHA256: []string{"ab"}}
	var long []Hop
	for i := range MaxHops + 1 {
		var via HopID
		if i < MaxHops {
			via = HopID(rune('a' + i + 1))
		}
		long = append(long, vlessHop(t, HopID(rune('a'+i)), via))
	}
	cases := []struct {
		name string
		root HopID
		hops []Hop
	}{
		{"empty", "x", nil},
		{"missing root", "nope", []Hop{vlessHop(t, "a", "")}},
		{"unresolved dialer", "a", []Hop{vlessHop(t, "a", "ghost")}},
		{"cycle", "a", []Hop{vlessHop(t, "a", "b"), vlessHop(t, "b", "a")}},
		{"unreachable hop", "a", []Hop{vlessHop(t, "a", ""), vlessHop(t, "b", "")}},
		{"duplicate id", "a", []Hop{vlessHop(t, "a", ""), vlessHop(t, "a", "")}},
		{"too deep", "a", long},
		{"reality over websocket", "r", []Hop{reality}},
		{"vision over grpc", "v", []Hop{vision}},
		{"pin without tls", "p", []Hop{pinnedPlain}},
		{"hysteria without tls", "h", []Hop{func() Hop { h := hysteriaHop(t); h.TLS = TLS{}; return h }()}},
		{"hysteria under reality", "h", []Hop{func() Hop { h := hysteriaHop(t); h.TLS.Reality = &Reality{PublicKey: "pk"}; return h }()}},
		{"hysteria2 over tcp", "h", []Hop{func() Hop { h := hysteriaHop(t); h.Transport = Transport{Kind: TCP}; return h }()}},
		{"vless over hysteria", "a", []Hop{func() Hop { h := vlessHop(t, "a", ""); h.Transport.Kind = Hysteria; return h }()}},
		{"salamander outside hysteria", "a", []Hop{func() Hop { h := vlessHop(t, "a", ""); h.Transport.Salamander = NewSecret("ob"); return h }()}},
		{"no port", "z", []Hop{func() Hop { h := vlessHop(t, "z", ""); h.Endpoint.Port = 0; return h }()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewServer(c.root, c.hops); !errors.Is(err, ErrInvalidServer) {
				t.Fatalf("err = %v, want ErrInvalidServer", err)
			}
		})
	}
}

// A source deleted while one of its servers was already missing keeps when that server went missing,
// which the page reads as how long it has been gone.
func TestOrphanedKeepsWhenALineageWentMissing(t *testing.T) {
	went, deleted := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	missing := Lineage{ID: "a", Presence: Missing, MissingSince: went, GraceUntil: went.Add(time.Hour)}
	if got := missing.Orphaned(deleted); got.Presence != Tombstone || !got.MissingSince.Equal(went) {
		t.Fatalf("orphaned: %v since %s, want a tombstone since %s", got.Presence, got.MissingSince, went)
	}
	present := Lineage{ID: "b", Presence: Present}
	if got := present.Orphaned(deleted); got.Presence != Tombstone || !got.MissingSince.Equal(deleted) {
		t.Fatalf("orphaned: %v since %s, want a tombstone since %s", got.Presence, got.MissingSince, deleted)
	}
}
