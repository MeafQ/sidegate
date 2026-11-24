package catalogue

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
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
	"slices"
	"strings"
	"testing"
	"time"
)

// xrayBin is the Xray v26 binary the live tests run, from XRAY_BIN.
func xrayBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN is not set")
	}
	return bin
}

func writeConfig(t *testing.T, dir, name string, config any) string {
	t.Helper()
	b, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// startXray runs Xray on config until the test ends and waits until it reports
// that it started.
func startXray(t *testing.T, dir, name string, config any) {
	t.Helper()
	logPath := filepath.Join(dir, name+".log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(xrayBin(t), "run", "-c", writeConfig(t, dir, name, config))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait(); log.Close() })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if out, _ := os.ReadFile(logPath); bytes.Contains(out, []byte("started")) {
			return
		}
	}
	out, _ := os.ReadFile(logPath)
	t.Fatalf("xray %s did not start:\n%s", name, out)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// selfSignedCert writes a certificate for name and returns its paths and the
// SHA-256 of its DER encoding, the value Hysteria's pinSHA256 carries.
func selfSignedCert(t *testing.T, dir, name string) (certPath, keyPath, pin string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return certPath, keyPath, hex.EncodeToString(sum[:])
}

// TestHysteria2PinsAgainstXray dials a loopback Hysteria2 server whose
// self-signed certificate names wrong.example through the outbound the
// catalogue renders from each link. Links ask for expected.example.
func TestHysteria2PinsAgainstXray(t *testing.T) {
	xrayBin(t)
	dir := t.TempDir()
	certPath, keyPath, pin := selfSignedCert(t, dir, "wrong.example")
	serverPort := freePort(t)
	startXray(t, dir, "server", map[string]any{
		"inbounds": []any{map[string]any{
			"listen": "127.0.0.1", "port": serverPort, "protocol": "hysteria",
			"settings": map[string]any{"version": 2, "users": []any{map[string]any{"auth": "p@ss"}}},
			"streamSettings": map[string]any{
				"network": "hysteria", "security": "tls", "hysteriaSettings": map[string]any{"version": 2},
				"tlsSettings": map[string]any{"certificates": []any{map[string]any{"certificateFile": certPath, "keyFile": keyPath}}},
			},
		}},
		"outbounds": []any{map[string]any{"protocol": "freedom", "settings": map[string]any{
			"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1"}}},
		}}},
	})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "reached") }))
	defer target.Close()
	link := func(insecure, pin string) string {
		return fmt.Sprintf("hysteria2://p%%40ss@127.0.0.1:%d/?sni=expected.example&insecure=%s&pinSHA256=%s#HY", serverPort, insecure, pin)
	}
	dial := func(t *testing.T, link string) bool {
		e, diags, err := parseOne(t, link)
		if err != nil {
			t.Fatalf("err = %v, diagnostics = %+v", err, diags)
		}
		_, hops := e.Server.Closure()
		clientPort := freePort(t)
		startXray(t, dir, fmt.Sprintf("client-%d", clientPort), map[string]any{
			"inbounds":  []any{map[string]any{"listen": "127.0.0.1", "port": clientPort, "protocol": "http"}},
			"outbounds": []any{hysteria2Outbound(hops[0])},
		})
		proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", clientPort))
		transport := &http.Transport{Proxy: http.ProxyURL(proxy)}
		defer transport.CloseIdleConnections()
		res, err := (&http.Client{Transport: transport, Timeout: 3 * time.Second}).Get(target.URL)
		if err != nil {
			return false
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return string(body) == "reached"
	}

	t.Run("insecure with the server's pin connects", func(t *testing.T) {
		if !dial(t, link("1", pin)) {
			t.Fatal("the pinned certificate did not connect")
		}
	})
	t.Run("insecure with another pin is refused by the core", func(t *testing.T) {
		if dial(t, link("1", strings.Repeat("00", 32))) {
			t.Fatal("a certificate that matches no pin connected")
		}
	})
	t.Run("a pin with verification on is not imported", func(t *testing.T) {
		_, diags, err := parseOne(t, link("0", pin))
		if !errors.Is(err, ErrNoUsableEntries) || !slices.Equal(diags, []Diagnostic{diag(0, CodeUnsupportedOption, "pinSHA256")}) {
			t.Fatalf("err = %v, diagnostics = %+v: a link that verifies the chain imported as pin-only", err, diags)
		}
	})
}

// fingerprintForms carries one TLS or Reality fingerprint in each format.
func fingerprintForms(fp string) map[string]string {
	return map[string]string{
		"vless link":   "vless://" + linkUUID + "@a.example.net:443?security=tls&sni=a.example.net&fp=" + fp + "#x",
		"reality link": "vless://" + linkUUID + "@a.example.net:443?security=reality&pbk=" + realityKey + "&sni=www.example.com&fp=" + fp + "#x",
		"vmess base64": "vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"add":"a.example.net","port":443,"id":"`+linkUUID+`","tls":"tls","fp":"`+fp+`"}`)),
		"xray tls": `{"outbounds":[{"protocol":"vless","settings":{"address":"a.example.net","port":443,"id":"` + linkUUID + `"},
			"streamSettings":{"security":"tls","tlsSettings":{"fingerprint":"` + fp + `"}}}]}`,
		"xray reality": `{"outbounds":[{"protocol":"vless","settings":{"address":"a.example.net","port":443,"id":"` + linkUUID + `"},
			"streamSettings":{"security":"reality","realitySettings":{"serverName":"www.example.com","publicKey":"` + realityKey + `","fingerprint":"` + fp + `"}}}]}`,
		"xray hysteria2": strings.Replace(hysteria2Forms["xray"], `"alpn":["h3"]`, `"alpn":["h3"],"fingerprint":"`+fp+`"`, 1),
		"sing-box": `{"outbounds":[{"type":"vless","tag":"x","server":"a.example.net","server_port":443,"uuid":"` + linkUUID + `",
			"tls":{"enabled":true,"utls":{"enabled":true,"fingerprint":"` + fp + `"}}}]}`,
		"clash": "proxies:\n- {name: x, type: vless, server: a.example.net, port: 443, uuid: " + linkUUID + ", tls: true, client-fingerprint: " + fp + "}\n",
	}
}

func TestParseRefusesFingerprintsXrayRejects(t *testing.T) {
	for _, fp := range []string{"not-a-fingerprint", "chrome_psk"} {
		for name, body := range fingerprintForms(fp) {
			t.Run(fp+"/"+name, func(t *testing.T) {
				_, diags, err := parseOne(t, body)
				if !errors.Is(err, ErrNoUsableEntries) || !slices.Equal(diags, []Diagnostic{diag(0, CodeUnsupportedOption, "fingerprint")}) {
					t.Fatalf("err = %v, diagnostics = %+v", err, diags)
				}
			})
		}
	}
	for name, body := range fingerprintForms("Firefox") {
		t.Run("known/"+name, func(t *testing.T) {
			e, diags, err := parseOne(t, body)
			if err != nil || len(diags) > 0 {
				t.Fatalf("err = %v, diagnostics = %+v", err, diags)
			}
			if _, hops := e.Server.Closure(); hops[0].TLS.Fingerprint != "firefox" {
				t.Fatalf("fingerprint = %q, want firefox", hops[0].TLS.Fingerprint)
			}
		})
	}
	if _, diags, _ := parseOne(t, fingerprintForms("unsafe")["reality link"]); !slices.Equal(diags, []Diagnostic{diag(0, CodeUnsupportedOption, "fingerprint")}) {
		t.Fatalf("reality with the unsafe fingerprint: diagnostics = %+v", diags)
	}
}

// TestFingerprintsLoadInXray runs Xray's config check on every fingerprint the
// catalogue accepts, as TLS and as Reality, and on one it refuses.
func TestFingerprintsLoadInXray(t *testing.T) {
	bin := xrayBin(t)
	outbound := func(security, fp string) map[string]any {
		stream := map[string]any{"security": security}
		if security == "tls" {
			stream["tlsSettings"] = map[string]any{"serverName": "a.example.net", "fingerprint": fp}
		} else {
			stream["realitySettings"] = map[string]any{"serverName": "www.example.com", "publicKey": realityKey, "fingerprint": fp}
		}
		return map[string]any{
			"tag": security + "-" + fp, "protocol": "vless", "streamSettings": stream,
			"settings": map[string]any{"address": "a.example.net", "port": 443, "id": linkUUID, "encryption": "none"},
		}
	}
	check := func(outbounds []any) (string, error) {
		out, err := exec.Command(bin, "run", "-test", "-c", writeConfig(t, t.TempDir(), "config", map[string]any{"outbounds": outbounds})).CombinedOutput()
		return string(out), err
	}
	var accepted []any
	for fp := range xrayFingerprints {
		for _, security := range []string{"tls", "reality"} {
			if _, err := (tlsOptions{security: security, serverName: "s", publicKey: realityKey, fingerprint: fp}).tls(); err == nil {
				accepted = append(accepted, outbound(security, fp))
			}
		}
	}
	accepted = append(accepted, outbound("tls", "unsafe"), outbound("tls", ""))
	if out, err := check(accepted); err != nil {
		t.Fatalf("xray refused a fingerprint the catalogue accepts:\n%s", out)
	}
	if out, err := check([]any{outbound("tls", "chrome_psk")}); err == nil {
		t.Fatalf("xray accepted an unknown fingerprint, so the check proves nothing:\n%s", out)
	}
}

func realityOutbound(sid string) map[string]any {
	return map[string]any{
		"protocol": "vless", "settings": map[string]any{"address": "de1.example.net", "port": 443, "id": linkUUID, "encryption": "none"},
		"streamSettings": map[string]any{"security": "reality", "realitySettings": map[string]any{
			"serverName": "www.example.com", "publicKey": realityKey, "fingerprint": "chrome", "shortId": sid}},
	}
}

func xrayAccepts(t *testing.T, config any) bool {
	t.Helper()
	out, err := exec.Command(xrayBin(t), "run", "-test", "-c", writeConfig(t, t.TempDir(), "config", config)).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "shortId") {
		t.Fatalf("xray failed for another reason:\n%s", out)
	}
	return err == nil
}

// The catalogue accepts exactly the short IDs Xray does, and stores each in a
// spelling Xray reads as the same ID.
func TestShortIDsAgainstXray(t *testing.T) {
	var spellings []string
	for _, pair := range equivalentShortIDs {
		spellings = append(spellings, pair[0], pair[1])
	}
	for _, s := range append(spellings, invalidShortIDs...) {
		stored, err := realityShortID(s)
		if accepted := xrayAccepts(t, map[string]any{"outbounds": []any{realityOutbound(s)}}); accepted != (err == nil) {
			t.Fatalf("short ID %q: xray accepts = %t, catalogue err = %v", s, accepted, err)
		}
		if err == nil && !xrayAccepts(t, map[string]any{"outbounds": []any{realityOutbound(stored)}}) {
			t.Fatalf("short ID %q: xray refuses the stored %q", s, stored)
		}
	}
}

// A refresh that only respells a short ID keeps the lineage and its digest.
func TestReconcileShortIDSpellingKeepsTheDigest(t *testing.T) {
	for _, pair := range equivalentShortIDs {
		t.Run(pair[0]+"_to_"+pair[1], func(t *testing.T) {
			for _, s := range pair {
				if !xrayAccepts(t, map[string]any{"outbounds": []any{realityOutbound(s)}}) {
					t.Fatalf("xray refuses short ID %q", s)
				}
			}
			sim := newSim(t, vless("node", "sid", pair[0]))
			id := sim.present()["node"]
			before := sim.lineages[id].Ref
			for _, hour := range []int{1, 2, 25} {
				sim.now = t0.Add(time.Duration(hour) * time.Hour)
				if r := sim.refresh(vless("node", "sid", pair[1])); len(r.Changes) != 0 {
					t.Fatalf("hour %d: %d changes for a respelled short ID", hour, len(r.Changes))
				}
			}
			if after := sim.lineages[id].Ref; after != before {
				t.Fatalf("equivalent short ID spelling changed the digest: %x -> %x", before.Digest, after.Digest)
			}
		})
	}
}

// The reviewer's chained feed loads in Xray in both of its forms, and neither
// stored pick is ever absent for a day.
func TestReconcileChainedRealityPicksSeenIndependently(t *testing.T) {
	sim := newSim(t, chainedReality("aa", "bb"))
	id := sim.present()["node"]
	before := sim.lineages[id].Ref
	for hour := 1; hour <= 25; hour++ {
		first, second := "aa", "cc"
		if hour%2 == 0 {
			first, second = "cc", "bb"
		}
		feed := chainedReality(first, second)
		if hour <= 2 {
			if out, err := exec.Command(xrayBin(t), "run", "-test", "-c", writeConfig(t, t.TempDir(), "config", json.RawMessage(feed))).CombinedOutput(); err != nil {
				t.Fatalf("%s", out)
			}
		}
		sim.now = t0.Add(time.Duration(hour) * time.Hour)
		sim.refresh(feed)
		if sim.lineages[id].Ref != before {
			t.Fatalf("hour %d: changed digest even though neither stored pick was absent for 24 hours", hour)
		}
	}
}
