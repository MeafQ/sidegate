package catalogue

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

const (
	realityKey = "ZCAAoWFiT2tR5b2hBh7RIuy6krPvCIrBp3kN4znYKOU"
	baseLink   = "vless://" + linkUUID + "@de1.example.net:443?type=tcp&security=reality&pbk=" + realityKey +
		"&fp=chrome&sni=www.example.com&sid=63ecb9e95ef79ad3&spx=%2F&flow=xtls-rprx-vision#DE"
)

func identityOf(t *testing.T, body string) domain.Identity {
	t.Helper()
	e, diags, err := parseOne(t, body)
	if err != nil || len(diags) > 0 {
		t.Fatalf("err = %v, diagnostics = %+v", err, diags)
	}
	return e.Identity
}

func TestIdentityIsFormatIndependent(t *testing.T) {
	xray := `{"outbounds": [{"tag": "anything", "protocol": "vless",
		"settings": {"vnext": [{"address": "de1.example.net", "port": 443, "users": [{"id": "` + linkUUID + `", "flow": "xtls-rprx-vision", "encryption": "none"}]}]},
		"streamSettings": {"network": "raw", "security": "reality", "realitySettings": {"serverName": "www.example.com",
			"fingerprint": "chrome", "publicKey": "` + realityKey + `", "shortId": "63ecb9e95ef79ad3", "spiderX": "/"}}}]}`
	link, json := identityOf(t, baseLink), identityOf(t, xray)
	if link.Strong != json.Strong || link.Endpoint != json.Endpoint || link.HostKey != json.HostKey {
		t.Fatal("the same server imported as a share link and as Xray JSON has different identities")
	}
}

func TestIdentityKeySensitivity(t *testing.T) {
	base := identityOf(t, baseLink)
	cases := []struct {
		name                               string
		from, to                           string
		strong, endpoint, host, realityTie bool // true: the key stays equal.
	}{
		{"remark", "#DE", "#Germany", true, true, true, true},
		{"credential rotation", linkUUID, "e5e5e5e5-0000-4000-8000-0000000000ff", false, true, true, true},
		{"fingerprint tweak", "fp=chrome", "fp=firefox", false, false, true, true},
		{"reality short id pick", "sid=63ecb9e95ef79ad3", "sid=0123456789abcdef", true, true, true, true},
		{"host change", "@de1.example.net", "@de9.example.net", false, false, false, true},
		{"host case", "@de1.example.net", "@DE1.Example.NET", true, true, true, true},
		{"reality server name pick", "sni=www.example.com", "sni=www.example.org", true, true, true, true},
		{"reality public key", realityKey, "JKfC68h07m-NaOnVzJqoBWenFpzidYSUGv2DaOFWE8Q", false, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := identityOf(t, strings.Replace(baseLink, c.from, c.to, 1))
			check := func(key string, same, want bool) {
				if same != want {
					t.Errorf("%s equal = %v, want %v", key, same, want)
				}
			}
			check("strong", got.Strong == base.Strong, c.strong)
			check("endpoint", got.Endpoint == base.Endpoint, c.endpoint)
			check("host", got.HostKey == base.HostKey, c.host)
			check("reality tie", got.RealityTie == base.RealityTie, c.realityTie)
		})
	}
}

func TestCredentialKeyFollowsOnlyTheRootSecretAndUser(t *testing.T) {
	noFlow := strings.Replace(baseLink, "&flow=xtls-rprx-vision", "", 1)
	base := identityOf(t, noFlow)
	if base.CredentialKey == (domain.Digest{}) {
		t.Fatal("a server with a secret has no credential key")
	}
	for _, change := range [][2]string{{"@de1.example.net", "@de9.example.net"}, {":443", ":8443"}, {"type=tcp", "type=grpc"}, {realityKey, "JKfC68h07m-NaOnVzJqoBWenFpzidYSUGv2DaOFWE8Q"}} {
		if !strings.Contains(noFlow, change[0]) {
			t.Fatalf("%q is not in the base link", change[0])
		}
		if got := identityOf(t, strings.Replace(noFlow, change[0], change[1], 1)); got.CredentialKey != base.CredentialKey {
			t.Errorf("%q changed the credential key", change[1])
		}
	}
	if got := identityOf(t, strings.Replace(noFlow, linkUUID, "e5e5e5e5-0000-4000-8000-0000000000ff", 1)); got.CredentialKey == base.CredentialKey {
		t.Error("a rotated secret kept the credential key")
	}
	if strings.Contains(fmt.Sprintf("%+v", base), linkUUID) {
		t.Error("the identity prints the secret")
	}
	if got := identityOf(t, `{"outbounds": [{"protocol": "socks", "settings": {"servers": [{"address": "proxy.example.net", "port": 1080}]}}]}`); got.CredentialKey != (domain.Digest{}) {
		t.Error("a server with no credentials has a credential key, so every open proxy would share it")
	}
}

func TestIdentityWebSocketHostHeaderIsTheHost(t *testing.T) {
	link := "vless://" + linkUUID + "@cdn.example.net:443?type=ws&path=%2Fws&host=front.example.net&security=tls&sni=front.example.net#ws"
	xray := `{"outbounds": [{"tag": "proxy", "protocol": "vless",
		"settings": {"address": "cdn.example.net", "port": 443, "id": "` + linkUUID + `"},
		"streamSettings": {"network": "ws", "security": "tls", "tcpSettings": {"header": {"type": "none"}}, "rawSettings": {"header": {"type": "none"}},
			"wsSettings": {"path": "/ws", "headers": {"Host": "front.example.net"}}, "tlsSettings": {"serverName": "front.example.net"}}}]}`
	if identityOf(t, link).Strong != identityOf(t, xray).Strong {
		t.Fatal("a WebSocket Host header and a host parameter give different identities")
	}
	raw := strings.Replace(strings.Replace(xray, `"network": "ws"`, `"network": "raw"`, 1), `"wsSettings": {"path": "/ws", "headers": {"Host": "front.example.net"}}, `, "", 1)
	identityOf(t, raw)
}

func TestIdentityKeepsTheTLSServerName(t *testing.T) {
	link := "trojan://pw@de1.example.net:443?security=tls&sni=www.example.com#DE"
	a, b := identityOf(t, link), identityOf(t, strings.Replace(link, "www.example.com", "www.example.org", 1))
	if a.Strong == b.Strong || a.Endpoint == b.Endpoint || a.HostKey == b.HostKey {
		t.Fatal("a TLS server name change left an identity key equal")
	}
}

// Xray decodes a short ID into 8 zero-padded bytes, so these pairs are one ID.
var equivalentShortIDs = [][2]string{
	{"abcdef0123456789", "ABCDEF0123456789"},
	{"ab", "ab00000000000000"},
	{"", "0000000000000000"},
}

// Xray refuses each of these shortId values.
var invalidShortIDs = []string{"abc", "zz", " ab", "abcdef012345678901"}

func TestIdentityShortIDSpellings(t *testing.T) {
	for _, pair := range equivalentShortIDs {
		a, _, errA := parseOne(t, vless("node", "sid", pair[0]))
		b, _, errB := parseOne(t, vless("node", "sid", pair[1]))
		if errA != nil || errB != nil {
			t.Fatalf("%q, %q: %v, %v", pair[0], pair[1], errA, errB)
		}
		if a.Identity != b.Identity || revisionDigest(a.Identity.Strong, a.Server) != revisionDigest(b.Identity.Strong, b.Server) {
			t.Errorf("short IDs %q and %q give different identities or digests", pair[0], pair[1])
		}
	}
	for _, s := range invalidShortIDs {
		if _, diags, _ := parseOne(t, vless("node", "sid", url.QueryEscape(s))); !slices.Equal(diags, []Diagnostic{diag(0, CodeMalformedEntry, "shortId")}) {
			t.Errorf("short ID %q: diagnostics = %+v, want malformed shortId", s, diags)
		}
	}
}
