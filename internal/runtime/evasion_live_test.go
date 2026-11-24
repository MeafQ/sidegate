package runtime

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
)

func rawURL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// vlessKeys makes one VLESS encryption: the server's decryption and the client's string.
func vlessKeys(t *testing.T) (decryption, encryption string) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return "mlkem768x25519plus.native.600s." + rawURL(key.Bytes()), "mlkem768x25519plus.native.0rtt." + rawURL(key.PublicKey().Bytes())
}

var liveHeader = obj{"type": "header-custom", "settings": obj{
	"clients": [][]obj{{{"type": "hex", "packet": "00ff47"}, {"rand": 4, "randRange": "48-57"}}},
	"servers": [][]obj{{{"type": "str", "packet": "hello"}, {"delay": "1-2", "packet": []int{1, 2, 3}}}},
	"errors":  [][]obj{{{"rand": 8}}}}}

var liveFragment = obj{"type": "fragment", "settings": obj{"packets": "tlshello", "lengths": []any{"20-40", 60}, "delays": []any{0, "1-2"}, "maxSplit": "3-6"}}

// TestEveryEvasionOptionLoadsInXray hands the pinned core one config of every option together, each
// parsed from the feed that carries it.
func TestEveryEvasionOptionLoadsInXray(t *testing.T) {
	bin := xrayBin(t)
	kem, err := mlkem.GenerateKey768()
	if err != nil {
		t.Fatal(err)
	}
	_, x25519 := vlessKeys(t)
	encryption := "mlkem768x25519plus.random.1rtt.100-111-1111.75-0-111.50-0-3333." + rawURL(kem.EncapsulationKey().Bytes())
	mldsa := rawURL(make([]byte, 1952))
	vless := func(encryption, flow string, stream obj) []byte {
		return xrayFeed(t, obj{"protocol": "vless", "streamSettings": stream, "settings": obj{"vnext": []obj{{"address": "198.51.100.10", "port": 443,
			"users": []obj{{"id": liveUser, "encryption": encryption, "flow": flow}}}}}})
	}
	masks := obj{"tcp": []obj{liveHeader, liveFragment}}
	servers := map[domain.ServerRef]domain.Server{}
	for name, feed := range map[string][]byte{
		"reality": vless(encryption, domain.VisionFlow, obj{"network": "raw", "security": "reality", "finalmask": masks, "realitySettings": obj{"serverName": "x.lab",
			"fingerprint": "chrome", "publicKey": "Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw", "shortId": "6ba85179e30d4fc2", "mldsa65Verify": mldsa}}),
		"plain vision": vless(x25519, domain.VisionFlow, obj{"network": "raw", "finalmask": masks}),
		"xhttp vision": vless(x25519, domain.VisionFlow, obj{"network": "xhttp", "security": "tls", "xhttpSettings": obj{"path": "/x"}, "finalmask": masks,
			"tlsSettings": obj{"serverName": "x.lab", "echConfigList": "AEX+DQBBqQAgACDxVzHvFTlX8mz5OVKkU5WyHVQ0Dy5nJcVkTmvOaZ2ZPAAEAAEAAQAMY2xvdWRmbGFyZS5jb20AAA=="}}),
		"ws":   vless("none", "", obj{"network": "ws", "security": "tls", "finalmask": masks, "tlsSettings": obj{"serverName": "x.lab", "echConfigList": "x.lab+https://1.1.1.1/dns-query"}}),
		"grpc": vless("none", "", obj{"network": "grpc", "security": "tls", "finalmask": masks, "tlsSettings": obj{"serverName": "x.lab"}}),
		"hysteria": xrayFeed(t, obj{"protocol": "hysteria", "settings": obj{"version": 2, "address": "198.51.100.10", "port": 443},
			"streamSettings": obj{"network": "hysteria", "security": "tls", "hysteriaSettings": obj{"version": 2, "auth": "p@ss"},
				"tlsSettings": obj{"serverName": "x.lab", "echConfigList": "x.lab+https://1.1.1.1/dns-query"},
				"finalmask": obj{"udp": []obj{{"type": "salamander", "settings": obj{"password": "obfs"}}}, "quicParams": obj{"debug": true,
					"congestion": "force-brutal", "bbrProfile": "aggressive", "brutalUp": "100 mbps", "brutalDown": "1g", "brutalDisableLossCompensation": true,
					"initStreamReceiveWindow": 16384, "maxStreamReceiveWindow": 32768, "initConnectionReceiveWindow": 16384, "maxConnectionReceiveWindow": 65536,
					"maxIdleTimeout": 30, "keepAlivePeriod": 10, "disablePathMTUDiscovery": true, "disableChromeParrot": true, "disableGSO": true,
					"disableStatelessReset": true, "maxIncomingStreams": 8}}}}),
	} {
		servers[testRef(name)] = parsedServer(t, feed, catalogue.XrayJSON)
	}
	plan := corePlan{Servers: servers}
	for ref := range servers {
		plan.Probes = append(plan.Probes, ref)
	}
	config, err := render(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, carried := range []string{`"fragment"`, `"header-custom"`, `"quicParams"`, `"salamander"`, mldsa, "echConfigList", encryption, x25519} {
		if !strings.Contains(string(config), carried) {
			t.Errorf("the config does not carry %.40s", carried)
		}
	}
	if strings.Contains(string(config), "debug") {
		t.Error("the config carries the feed's debug switch")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, config, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "run", "-test", "-c", path).CombinedOutput(); err != nil || !strings.Contains(string(out), "Configuration OK") {
		t.Fatalf("xray refused it: %v\n%s", err, out)
	}
}

// TestEvasionOptionsConnectLive connects through a provider that speaks each option, so one the
// renderer bends fails on the wire. The server reads no fragment: it is the client's alone.
func TestEvasionOptionsConnectLive(t *testing.T) {
	xrayBin(t)
	type options struct {
		decryption, encryption, flow string
		header, fragment, tls        bool
	}
	decryption, encryption := vlessKeys(t)
	_, stranger := vlessKeys(t)
	connect := func(t *testing.T, server, client options) error {
		cert, key, pin := testCert(t)
		port := freeTCPPort(t)
		inbound, outbound := obj{"network": "raw"}, obj{"network": "raw"}
		if server.tls {
			inbound["security"], inbound["tlsSettings"] = "tls", obj{"certificates": []obj{{"certificate": cert, "key": key}}}
			outbound["security"], outbound["tlsSettings"] = "tls", obj{"serverName": "live.lab", "pinnedPeerCertSha256": pin}
		}
		var masks []obj
		if server.header {
			inbound["finalmask"] = obj{"tcp": []obj{liveHeader}}
		}
		if client.header {
			masks = append(masks, liveHeader)
		}
		if client.fragment {
			masks = append(masks, liveFragment)
		}
		if masks != nil {
			outbound["finalmask"] = obj{"tcp": masks}
		}
		startXray(t, obj{"inbounds": []obj{{"listen": "127.0.0.1", "port": port, "protocol": "vless", "streamSettings": inbound,
			"settings": obj{"decryption": server.decryption, "clients": []obj{{"id": liveUser, "flow": server.flow}}}}}, "outbounds": freedomToLoopback})
		return reachesThrough(t, parsedServer(t, xrayFeed(t, obj{"protocol": "vless", "streamSettings": outbound, "settings": obj{"vnext": []obj{{
			"address": "127.0.0.1", "port": port, "users": []obj{{"id": liveUser, "encryption": client.encryption, "flow": client.flow}}}}}}), catalogue.XrayJSON))
	}
	for name, o := range map[string]options{
		"encryption":                           {decryption: decryption, encryption: encryption},
		"encryption and vision without tls":    {decryption: decryption, encryption: encryption, flow: domain.VisionFlow},
		"header and fragment":                  {decryption: "none", encryption: "none", header: true, fragment: true, tls: true},
		"header, fragment, encryption and tls": {decryption: decryption, encryption: encryption, flow: domain.VisionFlow, header: true, fragment: true, tls: true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := connect(t, o, o); err != nil {
				t.Errorf("rendered client does not connect: %v", err)
			}
		})
	}
	t.Run("a client without the header", func(t *testing.T) {
		if connect(t, options{decryption: "none", header: true}, options{encryption: "none"}) == nil {
			t.Error("connected, so the live test proves nothing about the header")
		}
	})
	t.Run("a client with another key", func(t *testing.T) {
		if connect(t, options{decryption: decryption}, options{encryption: stranger}) == nil {
			t.Error("connected, so the live test proves nothing about the encryption")
		}
	})
}

func TestQuicParamsAndSalamanderConnectLive(t *testing.T) {
	xrayBin(t)
	cert, key, pin := testCert(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	startXray(t, obj{"inbounds": []obj{{"listen": "127.0.0.1", "port": port, "protocol": "hysteria",
		"settings": obj{"version": 2, "clients": []obj{{"auth": "p@ss"}}},
		"streamSettings": obj{"network": "hysteria", "security": "tls", "hysteriaSettings": obj{"version": 2},
			"tlsSettings": obj{"certificates": []obj{{"certificate": cert, "key": key}}},
			"finalmask":   obj{"udp": []obj{{"type": "salamander", "settings": obj{"password": "obfs"}}}}}}},
		"outbounds": freedomToLoopback})
	fm := `{"udp":[{"type":"salamander","settings":{"password":"obfs"}}],"quicParams":{"debug":true,"congestion":"force-brutal","brutalUp":"50 mbps",` +
		`"initStreamReceiveWindow":65536,"maxIdleTimeout":10,"keepAlivePeriod":5,"disableChromeParrot":true}}`
	link := fmt.Sprintf("hysteria2://p%%40ss@127.0.0.1:%d/?sni=live.lab&insecure=1&pinSHA256=%s&fm=%s#HY", port, pin, strings.ReplaceAll(fm, " ", "%20"))
	if err := reachesThrough(t, parsedServer(t, []byte(link), catalogue.ShareLinks)); err != nil {
		t.Errorf("rendered client does not connect: %v", err)
	}
}
