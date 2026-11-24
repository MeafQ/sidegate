package catalogue

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"

	"sidegate/internal/domain"
)

func (b *builder) parseLinks(data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), len(data)+1)
	place := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue // Blank lines and the comment headers some panels prepend.
		}
		place++
		d, err := parseLink(line)
		if err := b.addServer(place, d, err); err != nil {
			return err
		}
	}
	if scanner.Err() != nil {
		return ErrMalformed
	}
	return nil
}

func parseLink(line string) (draft, error) {
	scheme, _, ok := strings.Cut(line, "://")
	if !ok {
		return draft{}, reject(CodeMalformedEntry, "scheme")
	}
	switch strings.ToLower(scheme) {
	case "vless":
		return parseURLLink(line, domain.VLESS)
	case "trojan":
		return parseURLLink(line, domain.Trojan)
	case "vmess":
		payload := line[len("vmess://"):]
		if i := strings.IndexAny(payload, "?#"); i >= 0 {
			payload = payload[:i] // A base64 link may carry ?remarks= or a fragment after the JSON.
		}
		if strings.Contains(payload, "@") {
			return parseURLLink(line, domain.VMess) // The standard URI form; base64 has no '@'.
		}
		return parseVMessLink(payload)
	case "ss":
		return parseShadowsocksLink(line[len("ss://"):])
	case "hysteria2", "hy2":
		return parseHysteria2Link(line)
	}
	return draft{}, reject(CodeUnsupportedProtocol, protocolName(scheme))
}

func single(label string, hop domain.Hop) draft {
	hop.ID = "proxy"
	return draft{label: label, root: hop.ID, hops: []domain.Hop{hop}}
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 {
		return 0, reject(CodeMalformedEntry, "port")
	}
	return uint16(n), nil
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// parseURLLink reads the vless://, trojan://, and URI-form vmess:// links, which
// share the query vocabulary of Xray's share-link standard.
func parseURLLink(line string, protocol domain.Protocol) (draft, error) {
	line, label := cutRemark(line)
	u, err := url.Parse(line)
	if err != nil || u.User == nil || u.Hostname() == "" {
		return draft{}, reject(CodeMalformedEntry, "uri")
	}
	q, err := parseQuery(u.RawQuery)
	if err != nil {
		return draft{}, err
	}
	port, err := parsePort(u.Port())
	if err != nil {
		return draft{}, err
	}
	mask, ok := parseFinalMask([]byte(q.Get("fm")), false)
	if !ok {
		return draft{}, reject(CodeUnsupportedOption, "fm")
	}
	tr := transportOptions{
		network: q.Get("type"), headerType: q.Get("headerType"), path: q.Get("path"),
		host: q.Get("host"), service: q.Get("serviceName"), mode: q.Get("mode"), extras: q.Get("extra"),
	}
	if strings.EqualFold(tr.network, "grpc") {
		tr.host = q.Get("authority")
	}
	transport, err := tr.transport()
	if err != nil {
		return draft{}, err
	}
	mask.apply(&transport)
	security := q.Get("security")
	if protocol == domain.Trojan && security == "" {
		security = "tls"
	}
	serverName := q.Get("sni")
	if serverName == "" && strings.EqualFold(security, "reality") {
		serverName = u.Hostname() // The share-link standard's default for an omitted sni.
	}
	tls, err := tlsOptions{
		security: security, serverName: serverName, fingerprint: q.Get("fp"), alpn: splitList(q.Get("alpn")),
		publicKey: q.Get("pbk"), shortID: q.Get("sid"), spider: q.Get("spx"), mldsa65Verify: q.Get("pqv"),
		ech:      cmp.Or(q.Get("ech"), q.Get("echConfigList")),
		insecure: truthy(q.Get("allowInsecure")) || truthy(q.Get("insecure")),
		pins:     splitList(q.Get("pcs")), names: splitList(q.Get("vcn")),
	}.tls()
	if err != nil {
		return draft{}, err
	}
	var spec domain.ProtocolSpec
	switch protocol {
	case domain.VLESS:
		spec, err = newVLESS(u.User.Username(), q.Get("flow"), q.Get("encryption"))
	case domain.VMess:
		spec, err = newProtocol(domain.VMess, u.User.Username(), "", vmessCipher(q.Get("encryption")), "")
	default:
		spec, err = newProtocol(domain.Trojan, u.User.Username(), "", "", "")
	}
	if err != nil {
		return draft{}, err
	}
	return single(label, domain.Hop{
		Endpoint: domain.Endpoint{Host: u.Hostname(), Port: port}, Protocol: spec, Transport: transport, TLS: tls,
	}), nil
}

// parseHysteria2Link reads the Hysteria2 URI scheme: the userinfo is the auth
// string, the port defaults to 443, and TLS is implied. Bandwidth hints (up,
// down) only tune congestion control and are not part of the server.
func parseHysteria2Link(line string) (draft, error) {
	line, label := cutRemark(line)
	u, err := url.Parse(line)
	if err != nil || u.User == nil || u.Hostname() == "" {
		return draft{}, reject(CodeMalformedEntry, "uri")
	}
	q, err := parseQuery(u.RawQuery)
	if err != nil {
		return draft{}, err
	}
	if q.Get("mport") != "" {
		return draft{}, reject(CodeUnsupportedOption, "mport")
	}
	port := uint16(443)
	if u.Port() != "" {
		if port, err = parsePort(u.Port()); err != nil {
			return draft{}, err
		}
	}
	salamander, err := hysteriaObfs(q.Get("obfs"), q.Get("obfs-password"))
	if err != nil {
		return draft{}, err
	}
	mask, ok := parseFinalMask([]byte(q.Get("fm")), true)
	if !ok || salamander != "" && mask.salamander != "" && salamander != mask.salamander {
		return draft{}, reject(CodeUnsupportedOption, "fm") // Two passwords for the one mask.
	}
	auth := u.User.Username()
	if password, ok := u.User.Password(); ok {
		auth += ":" + password // The userpass form.
	}
	spec, err := newProtocol(domain.Hysteria2, auth, "", "", "")
	if err != nil {
		return draft{}, err
	}
	transport, err := transportOptions{network: "hysteria", salamander: cmp.Or(salamander, mask.salamander)}.transport()
	if err != nil {
		return draft{}, err
	}
	mask.apply(&transport)
	insecure, pins := truthy(q.Get("insecure")), splitList(q.Get("pinSHA256"))
	if len(pins) > 0 && !insecure {
		// Hysteria checks the pin on top of normal verification. An Xray leaf pin
		// replaces verification (transport/internet/tls/config.go, verifyPeerCert),
		// so it would import a server weaker than the link.
		return draft{}, reject(CodeUnsupportedOption, "pinSHA256")
	}
	tls, err := tlsOptions{
		security: "tls", serverName: q.Get("sni"), alpn: splitList(q.Get("alpn")),
		insecure: insecure, pins: pins, names: splitList(q.Get("vcn")), ech: cmp.Or(q.Get("ech"), q.Get("echConfigList")),
	}.tls()
	if err != nil {
		return draft{}, err
	}
	return single(label, domain.Hop{
		Endpoint: domain.Endpoint{Host: u.Hostname(), Port: port}, Protocol: spec, Transport: transport, TLS: tls,
	}), nil
}

// cutRemark splits off the fragment, which panels fill with display text that
// is often not escaped ("#DE 100% speed"). A remark that cannot be unescaped is
// kept as written.
func cutRemark(link string) (rest, label string) {
	rest, fragment, _ := strings.Cut(link, "#")
	if label, err := url.PathUnescape(fragment); err == nil {
		return rest, label
	}
	return rest, fragment
}

// parseQuery splits on '&' only. url.ParseQuery refuses ';', which panels leave
// raw in values such as path=/ws;ed=2048.
func parseQuery(raw string) (url.Values, error) {
	q := url.Values{}
	for part := range strings.SplitSeq(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		key, err1 := url.QueryUnescape(k)
		value, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			return nil, reject(CodeMalformedEntry, "query")
		}
		q.Add(key, value)
	}
	return q, nil
}

// flexString accepts a JSON string or number, as vmess links carry either.
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

func parseVMessLink(payload string) (draft, error) {
	raw, ok := decodeBase64([]byte(payload))
	if !ok {
		return draft{}, reject(CodeMalformedEntry, "payload")
	}
	var v struct {
		PS, Add, ID, Scy, Net, Type, Host, Path, TLS, SNI, ALPN, FP string
		Port, Aid, AllowInsecure                                    flexString
	}
	if json.Unmarshal(raw, &v) != nil || v.Add == "" {
		return draft{}, reject(CodeMalformedEntry, "payload")
	}
	port, err := parsePort(string(v.Port))
	if err != nil {
		return draft{}, err
	}
	if aid := strings.TrimSpace(string(v.Aid)); aid != "" && aid != "0" {
		return draft{}, reject(CodeUnsupportedOption, "alterId")
	}
	tr := transportOptions{network: v.Net, path: v.Path, host: v.Host}
	switch strings.ToLower(v.Net) {
	case "", "tcp", "raw":
		tr.headerType = v.Type
	case "grpc":
		tr.service, tr.mode, tr.path, tr.host = v.Path, v.Type, "", ""
	case "xhttp", "splithttp":
		tr.mode = v.Type
	}
	transport, err := tr.transport()
	if err != nil {
		return draft{}, err
	}
	tls, err := tlsOptions{
		security: v.TLS, serverName: v.SNI, fingerprint: v.FP, alpn: splitList(v.ALPN),
		insecure: truthy(string(v.AllowInsecure)),
	}.tls()
	if err != nil {
		return draft{}, err
	}
	spec, err := newProtocol(domain.VMess, v.ID, "", vmessCipher(v.Scy), "")
	if err != nil {
		return draft{}, err
	}
	return single(v.PS, domain.Hop{
		Endpoint: domain.Endpoint{Host: strings.Trim(v.Add, "[]"), Port: port}, Protocol: spec, Transport: transport, TLS: tls,
	}), nil
}

// parseShadowsocksLink reads SIP002 (base64 or plain userinfo, the latter used by
// 2022 methods) and the legacy form that base64-encodes the whole authority.
func parseShadowsocksLink(rest string) (draft, error) {
	rest, label := cutRemark(rest)
	rest, rawQuery, _ := strings.Cut(rest, "?")
	rest = strings.TrimSuffix(rest, "/")
	q, err := parseQuery(rawQuery)
	if err != nil {
		return draft{}, err
	}
	if q.Get("plugin") != "" {
		return draft{}, reject(CodeUnsupportedPlugin, "plugin")
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		decoded, ok := decodeBase64([]byte(rest))
		if !ok {
			return draft{}, reject(CodeMalformedEntry, "payload")
		}
		rest = string(decoded)
		if at = strings.LastIndex(rest, "@"); at < 0 {
			return draft{}, reject(CodeMalformedEntry, "payload")
		}
	}
	userinfo, err := url.PathUnescape(rest[:at])
	if err != nil {
		return draft{}, reject(CodeMalformedEntry, "userinfo")
	}
	hostport := rest[at+1:]
	if !strings.Contains(userinfo, ":") {
		decoded, ok := decodeBase64([]byte(userinfo))
		if !ok {
			return draft{}, reject(CodeMalformedEntry, "userinfo")
		}
		userinfo = string(decoded)
	}
	method, password, ok := strings.Cut(userinfo, ":")
	if !ok {
		return draft{}, reject(CodeMalformedEntry, "userinfo")
	}
	host, portText, err := net.SplitHostPort(hostport)
	if err != nil || host == "" {
		return draft{}, reject(CodeMalformedEntry, "address")
	}
	port, err := parsePort(portText)
	if err != nil {
		return draft{}, err
	}
	cipher, err := shadowsocksCipher(method)
	if err != nil {
		return draft{}, err
	}
	spec, err := newProtocol(domain.Shadowsocks, password, "", cipher, "")
	if err != nil {
		return draft{}, err
	}
	return single(label, domain.Hop{
		Endpoint: domain.Endpoint{Host: host, Port: port}, Protocol: spec, Transport: domain.Transport{Kind: domain.TCP},
	}), nil
}
