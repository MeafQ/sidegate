package runtime

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
)

const liveUser = "6f1c1e52-0d3c-4b8e-9a51-2c1f7c0a5e11"

func xrayBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN is not set")
	}
	return bin
}

// testCert makes a self-signed certificate, returned as PEM lines for Xray, and its pin.
func testCert(t *testing.T) (cert, key []string, pin string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "live.lab"}, DNSNames: []string{"live.lab"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	lines := func(kind string, b []byte) []string {
		return strings.Split(strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: b}))), "\n")
	}
	return lines("CERTIFICATE", der), lines("EC PRIVATE KEY", keyDER), hex.EncodeToString(sum[:])
}

func startXray(t *testing.T, config obj) *os.Process {
	t.Helper()
	dir := t.TempDir()
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(xrayBin(t), "run", "-c", path)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait(); log.Close() })
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if out, _ := os.ReadFile(log.Name()); strings.Contains(string(out), "started") {
			return cmd.Process
		}
	}
	out, _ := os.ReadFile(log.Name())
	t.Fatalf("xray did not start:\n%s\n%s", body, out)
	return nil
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func parsedServer(t *testing.T, feed []byte, format catalogue.Format) domain.Server {
	t.Helper()
	d, err := catalogue.Parse(feed, format, catalogue.Limits{})
	if err != nil || len(d.Entries()) != 1 {
		t.Fatalf("catalogue: %v, diagnostics %+v", err, d.Diagnostics())
	}
	return d.Entries()[0].Server
}

// xrayFeed writes one Xray outbound as a provider's feed.
func xrayFeed(t *testing.T, outbound obj) []byte {
	t.Helper()
	outbound["tag"] = "node"
	feed, err := json.Marshal(obj{"outbounds": []obj{outbound}})
	if err != nil {
		t.Fatal(err)
	}
	return feed
}

// reachesThrough renders server's root hop as the runtime would, and fetches a local
// page through it, by way of whatever provider inbound the caller started.
func reachesThrough(t *testing.T, server domain.Server) error {
	t.Helper()
	root, hops := server.Closure()
	if root != hops[0].ID || len(hops) != 1 {
		t.Fatalf("closure %v", hops)
	}
	out, err := renderHop("test", hops[0])
	if err != nil {
		t.Fatalf("renderHop: %v", err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "reached") }))
	defer target.Close()
	port := freeTCPPort(t)
	startXray(t, obj{"inbounds": []obj{{"listen": "127.0.0.1", "port": port, "protocol": "http"}}, "outbounds": []obj{out}})
	proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	tr := &http.Transport{Proxy: http.ProxyURL(proxy)}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get(target.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil && string(body) != "reached" {
		return fmt.Errorf("body %q", body)
	}
	return err
}

// freedomToLoopback lets a test's provider reach the local page, which Xray's
// freedom outbound otherwise refuses.
var freedomToLoopback = []obj{{"protocol": "freedom", "settings": obj{"finalRules": []obj{{"action": "allow", "ip": []string{"127.0.0.1"}}}}}}

// TestVLESSTransportsConnectLive starts a provider with each transport's wire field
// set, parses the matching feed, and connects, so a field the renderer drops fails
// on the wire.
func TestVLESSTransportsConnectLive(t *testing.T) {
	xrayBin(t)
	cases := map[string]obj{
		"xhttp stream-one": {"network": "xhttp", "xhttpSettings": obj{"path": "/x", "mode": "stream-one"}},
		"xhttp packet-up":  {"network": "xhttp", "xhttpSettings": obj{"path": "/x", "mode": "packet-up"}},
		"grpc multi":       {"network": "grpc", "grpcSettings": obj{"serviceName": "svc", "multiMode": true}},
		"ws":               {"network": "ws", "wsSettings": obj{"path": "/ws", "headers": obj{"X-Lab": "1"}}},
	}
	for name, transport := range cases {
		t.Run(name, func(t *testing.T) {
			cert, key, pin := testCert(t)
			port := freeTCPPort(t)
			inbound := obj{"security": "tls", "tlsSettings": obj{"certificates": []obj{{"certificate": cert, "key": key}}}}
			client := obj{"security": "tls", "tlsSettings": obj{"serverName": "live.lab", "pinnedPeerCertSha256": pin}}
			for k, v := range transport {
				inbound[k], client[k] = v, v
			}
			startXray(t, obj{"inbounds": []obj{{"listen": "127.0.0.1", "port": port, "protocol": "vless",
				"settings": obj{"decryption": "none", "clients": []obj{{"id": liveUser}}}, "streamSettings": inbound}},
				"outbounds": freedomToLoopback})
			server := parsedServer(t, xrayFeed(t, obj{"protocol": "vless", "streamSettings": client,
				"settings": obj{"vnext": []obj{{"address": "127.0.0.1", "port": port, "users": []obj{{"id": liveUser, "encryption": "none"}}}}}}), catalogue.XrayJSON)
			if err := reachesThrough(t, server); err != nil {
				t.Errorf("rendered client does not connect: %v", err)
			}
		})
	}
}

// TestHysteria2ConnectsLive parses a Hysteria2 link the way a feed carries it, with
// no ALPN, and connects through a Hysteria inbound on loopback.
func TestHysteria2ConnectsLive(t *testing.T) {
	xrayBin(t)
	for name, obfs := range map[string]string{"plain": "", "salamander": "obfs"} {
		t.Run(name, func(t *testing.T) {
			cert, key, pin := testCert(t)
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := pc.LocalAddr().(*net.UDPAddr).Port
			pc.Close()
			stream := obj{"network": "hysteria", "security": "tls", "hysteriaSettings": obj{"version": 2},
				"tlsSettings": obj{"certificates": []obj{{"certificate": cert, "key": key}}}}
			link := fmt.Sprintf("hysteria2://p%%40ss@127.0.0.1:%d/?sni=live.lab&insecure=1&pinSHA256=%s", port, pin)
			if obfs != "" {
				stream["finalmask"] = obj{"udp": []obj{{"type": "salamander", "settings": obj{"password": obfs}}}}
				link += "&obfs=salamander&obfs-password=" + obfs
			}
			startXray(t, obj{"inbounds": []obj{{"listen": "127.0.0.1", "port": port, "protocol": "hysteria",
				"settings": obj{"version": 2, "clients": []obj{{"auth": "p@ss"}}}, "streamSettings": stream}},
				"outbounds": freedomToLoopback})
			server := parsedServer(t, []byte(link+"#HY"), catalogue.ShareLinks)
			if err := reachesThrough(t, server); err != nil {
				t.Errorf("rendered client does not connect: %v", err)
			}
		})
	}
}

func TestAWrongHysteriaAuthDoesNotConnectLive(t *testing.T) {
	xrayBin(t)
	cert, key, pin := testCert(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	startXray(t, obj{"inbounds": []obj{{"listen": "127.0.0.1", "port": port, "protocol": "hysteria",
		"settings": obj{"version": 2, "clients": []obj{{"auth": "right"}}},
		"streamSettings": obj{"network": "hysteria", "security": "tls", "hysteriaSettings": obj{"version": 2},
			"tlsSettings": obj{"certificates": []obj{{"certificate": cert, "key": key}}}}}},
		"outbounds": freedomToLoopback})
	link := fmt.Sprintf("hysteria2://wrong@127.0.0.1:%d/?sni=live.lab&insecure=1&pinSHA256=%s#HY", port, pin)
	if err := reachesThrough(t, parsedServer(t, []byte(link), catalogue.ShareLinks)); err == nil {
		t.Error("a wrong auth connected, so the live test proves nothing about the credential")
	}
}

// TestEveryProtocolAndTransportLoadsInXray renders what the catalogue parses from a
// feed of each protocol over each transport and hands it to the pinned core.
func TestEveryProtocolAndTransportLoadsInXray(t *testing.T) {
	bin := xrayBin(t)
	for _, protocol := range []string{"vless", "vmess", "trojan", "shadowsocks", "socks", "http"} {
		for _, network := range []string{"raw", "ws", "grpc", "xhttp"} {
			t.Run(protocol+"/"+network, func(t *testing.T) {
				server := obj{"address": "198.51.100.10", "port": 443}
				settings := obj{"servers": []obj{server}}
				switch protocol {
				case "vless":
					server["users"] = []obj{{"id": liveUser, "encryption": "none"}}
					settings = obj{"vnext": []obj{server}}
				case "vmess":
					server["users"] = []obj{{"id": liveUser, "security": "auto"}}
					settings = obj{"vnext": []obj{server}}
				case "trojan":
					server["password"] = "pass"
				case "shadowsocks":
					server["method"], server["password"] = "aes-128-gcm", "pass"
				case "socks", "http":
					server["users"] = []obj{{"user": "u", "pass": "p"}}
				}
				stream := obj{"network": network, "security": "tls", "tlsSettings": obj{"serverName": "x.lab"}}
				switch network {
				case "ws":
					stream["wsSettings"] = obj{"path": "/ws", "host": "x.lab", "headers": obj{"X-Lab": "1"}}
				case "grpc":
					stream["grpcSettings"] = obj{"serviceName": "svc", "multiMode": true, "authority": "x.lab"}
				case "xhttp":
					stream["xhttpSettings"] = obj{"path": "/x", "mode": "stream-one", "host": "x.lab"}
				}
				parsed := parsedServer(t, xrayFeed(t, obj{"protocol": protocol, "settings": settings, "streamSettings": stream}), catalogue.XrayJSON)
				ref := testRef("matrix")
				config, err := render(corePlan{Servers: map[domain.ServerRef]domain.Server{ref: parsed}, Probes: []domain.ServerRef{ref}})
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(path, config, 0o600); err != nil {
					t.Fatal(err)
				}
				if out, err := exec.Command(bin, "run", "-test", "-c", path).CombinedOutput(); err != nil {
					t.Fatalf("xray refused it: %v\n%s", err, out)
				}
			})
		}
	}
}
