package catalogue

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

var hysteria2Forms = map[string]string{
	"link": "hysteria2://p%40ss@hy.example.net:8443/?sni=hy.example.net&alpn=h3&obfs=salamander&obfs-password=ob#HY",
	"hy2":  "hy2://p%40ss@hy.example.net:8443?sni=hy.example.net&alpn=h3&obfs=salamander&obfs-password=ob#HY",
	"xray": `{"outbounds":[{"tag":"HY","protocol":"hysteria","settings":{"version":2,"address":"hy.example.net","port":8443},
		"streamSettings":{"network":"hysteria","security":"tls","tlsSettings":{"serverName":"hy.example.net","alpn":["h3"]},
		"hysteriaSettings":{"version":2,"auth":"p@ss"},"finalmask":{"udp":[{"type":"salamander","settings":{"password":"ob"}}]}}}]}`,
	"sing-box": `{"outbounds":[{"type":"hysteria2","tag":"HY","server":"hy.example.net","server_port":8443,"password":"p@ss",
		"up_mbps":100,"down_mbps":500,"obfs":{"type":"salamander","password":"ob"},
		"tls":{"enabled":true,"server_name":"hy.example.net","alpn":["h3"]}}]}`,
	"clash": "proxies:\n- {name: HY, type: hysteria2, server: hy.example.net, port: 8443, password: p@ss, up: 100, down: 500,\n" +
		"   sni: hy.example.net, alpn: [h3], obfs: salamander, obfs-password: ob}\n",
}

func TestParseHysteria2(t *testing.T) {
	strong := map[domain.Digest]bool{}
	for name, body := range hysteria2Forms {
		t.Run(name, func(t *testing.T) {
			e, diags, err := parseOne(t, body)
			if err != nil || len(diags) > 0 {
				t.Fatalf("err = %v, diagnostics = %+v", err, diags)
			}
			_, hops := e.Server.Closure()
			h := hops[0]
			p, auth, _, _, _ := h.Protocol.Fields()
			if e.Label != "HY" || p != domain.Hysteria2 || auth.Reveal() != "p@ss" || h.Endpoint != (domain.Endpoint{Host: "hy.example.net", Port: 8443}) {
				t.Errorf("entry = %q %v %q %+v", e.Label, p, auth.Reveal(), h.Endpoint)
			}
			if h.Transport.Kind != domain.Hysteria || h.Transport.Salamander.Reveal() != "ob" {
				t.Errorf("transport = %v, salamander %q", h.Transport.Kind, h.Transport.Salamander.Reveal())
			}
			if !h.TLS.Enabled || h.TLS.ServerName != "hy.example.net" || !slices.Equal(h.TLS.ALPN, []string{"h3"}) {
				t.Errorf("tls = %+v", h.TLS)
			}
			strong[e.Identity.Strong] = true
		})
	}
	if len(strong) != 1 {
		t.Errorf("the forms of one server have %d strong identities, want 1", len(strong))
	}
}

func TestParseHysteria2Diagnostics(t *testing.T) {
	link := hysteria2Forms["link"]
	cases := []struct {
		name, body string
		code       DiagnosticCode
		field      string
	}{
		{"port hopping link", strings.Replace(link, "#HY", "&mport=20000-30000#HY", 1), CodeUnsupportedOption, "mport"},
		{"gecko obfuscation", strings.Replace(link, "obfs=salamander", "obfs=gecko", 1), CodeUnsupportedOption, "obfs"},
		{"insecure without a pin", strings.Replace(link, "#HY", "&insecure=1#HY", 1), CodeInsecureTLS, "allowInsecure"},
		{"pin with verification on", strings.Replace(link, "#HY", "&pinSHA256="+strings.Repeat("ab", 32)+"#HY", 1), CodeUnsupportedOption, "pinSHA256"},
		{"hysteria v1 link", "hysteria://hy.example.net:443?auth=x#v1", CodeUnsupportedProtocol, "hysteria"},
		{"xray hysteria v1", strings.Replace(hysteria2Forms["xray"], `"version":2,"address"`, `"version":1,"address"`, 1), CodeUnsupportedOption, "version"},
		{"xray salamander packet size", strings.Replace(hysteria2Forms["xray"], `"password":"ob"`, `"password":"ob","packetSize":"512-1200"`, 1), CodeUnsupportedOption, "finalmask"},
		{"sing-box port hopping", strings.Replace(hysteria2Forms["sing-box"], `"server_port":8443`, `"server_ports":["20000:30000"]`, 1), CodeUnsupportedOption, "server_ports"},
		{"sing-box hysteria v1 with a string obfs", `{"outbounds":[{"type":"hysteria","tag":"v1","server":"hy.example.net","server_port":443,"obfs":"xplus"}]}`, CodeUnsupportedProtocol, "hysteria"},
		{"clash port hopping", strings.Replace(hysteria2Forms["clash"], "port: 8443", "port: 8443, ports: 20000-30000", 1), CodeUnsupportedOption, "ports"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags, err := parseOne(t, c.body)
			if !errors.Is(err, ErrNoUsableEntries) || !slices.Equal(diags, []Diagnostic{diag(0, c.code, c.field)}) {
				t.Fatalf("err = %v, diagnostics = %+v, want %+v", err, diags, diag(0, c.code, c.field))
			}
		})
	}
}

// hysteria2Outbound renders a Hysteria2 hop as an Xray v26 outbound, the shape
// infra/conf/hysteria.go and transport_internet.go read.
func hysteria2Outbound(h domain.Hop) map[string]any {
	_, auth, _, _, _ := h.Protocol.Fields()
	tls := map[string]any{"serverName": h.TLS.ServerName}
	if len(h.TLS.ALPN) > 0 {
		tls["alpn"] = h.TLS.ALPN
	}
	if len(h.TLS.PinnedCertSHA256) > 0 {
		tls["pinnedPeerCertSha256"] = strings.Join(h.TLS.PinnedCertSHA256, ",")
	}
	stream := map[string]any{
		"network": "hysteria", "security": "tls", "tlsSettings": tls,
		"hysteriaSettings": map[string]any{"version": 2, "auth": auth.Reveal()},
	}
	if !h.Transport.Salamander.IsZero() {
		stream["finalmask"] = map[string]any{"udp": []any{map[string]any{
			"type": "salamander", "settings": map[string]any{"password": h.Transport.Salamander.Reveal()},
		}}}
	}
	return map[string]any{
		"tag": "proxy", "protocol": "hysteria", "streamSettings": stream,
		"settings": map[string]any{"version": 2, "address": h.Endpoint.Host, "port": h.Endpoint.Port},
	}
}

// TestHysteria2LoadsInXray needs XRAY_BIN, an Xray v26 binary, and runs its
// config check on the outbound the catalogue's server renders to.
func TestHysteria2LoadsInXray(t *testing.T) {
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN is not set")
	}
	pin := "391df8c92c99edba0cc2d8a90c533a9e3ef69e7cbe6e6a6adaf1fcfc92ed5d7d"
	e, diags, err := parseOne(t, strings.Replace(hysteria2Forms["link"], "#HY", "&insecure=1&pinSHA256="+colons(pin)+"#HY", 1))
	if err != nil || len(diags) > 0 {
		t.Fatalf("err = %v, diagnostics = %+v", err, diags)
	}
	_, hops := e.Server.Closure()
	check := func(outbound map[string]any) (string, error) {
		config, err := json.Marshal(map[string]any{"outbounds": []any{outbound}})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, config, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bin, "run", "-test", "-c", path).CombinedOutput()
		return string(config) + "\n" + string(out), err
	}
	out, err := check(hysteria2Outbound(hops[0]))
	t.Log(out)
	if err != nil || !strings.Contains(out, "Configuration OK") {
		t.Fatalf("xray refused the rendered outbound: %v", err)
	}
	broken := hysteria2Outbound(hops[0])
	broken["settings"].(map[string]any)["version"] = 1
	if out, err := check(broken); err == nil {
		t.Fatalf("xray accepted a version 1 outbound, so the check proves nothing:\n%s", out)
	}
}
