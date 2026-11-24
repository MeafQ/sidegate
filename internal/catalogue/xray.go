package catalogue

import (
	"encoding/json"
	"strings"

	"sidegate/internal/domain"
)

type xrayConfig struct {
	Remarks   string          `json:"remarks"`
	Outbounds json.RawMessage `json:"outbounds"`
}

type xrayOutbound struct {
	Tag            string          `json:"tag"`
	Protocol       string          `json:"protocol"`
	Settings       json.RawMessage `json:"settings"`
	StreamSettings xrayStream      `json:"streamSettings"`
	ProxySettings  struct {
		Tag string `json:"tag"`
	} `json:"proxySettings"`
}

type xrayStream struct {
	Network         string `json:"network"`
	Security        string `json:"security"`
	TLSSettings     xrayTLS
	RealitySettings xrayReality
	WSSettings      struct {
		Path, Host string
		Headers    map[string]string
	}
	GRPCSettings struct {
		ServiceName, Authority string
		MultiMode              bool
	}
	XHTTPSettings     xrayXHTTP
	SplitHTTPSettings xrayXHTTP
	TCPSettings       xrayRaw
	RawSettings       xrayRaw
	Sockopt           struct {
		DialerProxy string
	}
	HysteriaSettings struct {
		Version flexString
		Auth    string
	}
	FinalMask json.RawMessage
}

type xrayXHTTP struct {
	Path, Host, Mode string
	Extra            json.RawMessage
}

type xrayRaw struct {
	Header struct{ Type string }
}

type xrayTLS struct {
	ServerName, Fingerprint string
	ALPN                    []string
	AllowInsecure           bool
	PinnedPeerCertSha256    flexList
	VerifyPeerCertByName    flexList
	ECHConfigList           string
}

type xrayReality struct {
	ServerName, PublicKey, Password, ShortID, SpiderX, Fingerprint, Mldsa65Verify string
}

// flexList accepts a JSON list or one string of comma or tilde separated values.
type flexList []string

func (f *flexList) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*f = nonEmpty(strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '~' }))
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*f = nonEmpty(list)
	return nil
}

type xrayUser struct {
	ID, Flow, Encryption, Security string
	AlterID                        flexString `json:"alterId"`
	User, Pass                     string
}

type xrayServer struct {
	xrayUser
	Address, Password, Method string
	Port                      flexString
	Users                     []json.RawMessage
}

// xraySettings keeps the server and user lists raw: only one member is
// supported, so a long list is refused before any member is built.
type xraySettings struct {
	xrayServer
	Vnext, Servers []json.RawMessage
	Version        flexString // Hysteria only.
}

var xrayUtility = map[string]bool{"freedom": true, "blackhole": true, "dns": true, "loopback": true}

func (b *builder) parseXray(data []byte) error {
	var configs []xrayConfig
	if len(data) > 0 && data[0] == '[' {
		var list json.RawMessage
		if json.Unmarshal(data, &list) != nil {
			return ErrMalformed
		}
		var err error
		if configs, err = decodeList[xrayConfig](b, list); err != nil {
			return err
		}
		if len(configs) == 0 {
			b.doc.explicitlyEmpty = true
		}
	} else {
		var c xrayConfig
		if json.Unmarshal(data, &c) != nil {
			return ErrMalformed
		}
		configs = []xrayConfig{c}
	}
	for i, c := range configs {
		outbounds, err := decodeList[xrayOutbound](b, c.Outbounds)
		if err != nil {
			return err
		}
		if err := b.addXrayConfig(c.Remarks, i+1, outbounds); err != nil {
			return err
		}
	}
	return nil
}

// addXrayConfig imports each proxy outbound that no other outbound dials through,
// together with its dialer chain, and lists the config's other outbounds as skipped.
// Routing, inbounds, and balancers stay with the provider.
func (b *builder) addXrayConfig(remarks string, place int, outbounds []xrayOutbound) error {
	byTag := make(map[string]*xrayOutbound, len(outbounds))
	used := map[string]bool{}
	repeated := false
	for i := range outbounds {
		o := &outbounds[i]
		if via := o.dialVia(); via != "" {
			used[via] = true
		}
		if o.Tag == "" {
			continue
		}
		_, seen := byTag[o.Tag]
		repeated = repeated || seen
		byTag[o.Tag] = o
	}
	at := b.openConfig(remarks, place)
	var roots []int
	for i := range outbounds {
		o := &outbounds[i]
		protocol := strings.ToLower(o.Protocol)
		switch {
		case xrayUtility[protocol]:
			b.skip(at, domain.Skip{Index: i + 1, Tag: o.Tag, Reason: domain.SkipHelper, Protocol: protocol})
		case used[o.Tag]:
			b.skip(at, domain.Skip{Index: i + 1, Tag: o.Tag, Reason: domain.SkipHop})
		default:
			roots = append(roots, i)
		}
	}
	if repeated {
		// A repeated tag makes every dialer reference ambiguous, so the config is refused whole, each root in its place.
		for _, i := range roots {
			if err := b.add(at, draft{label: outbounds[i].Tag, index: i + 1}, reject(CodeDuplicateTag, "tag")); err != nil {
				return err
			}
		}
		b.doc.configs[at].sortSkipped()
		return nil
	}
	for _, i := range roots {
		root := &outbounds[i]
		d, err := b.xrayClosure(root, byTag)
		d.label, d.index = root.Tag, i+1
		if err := b.add(at, d, err); err != nil {
			return err
		}
	}
	b.doc.configs[at].sortSkipped()
	return nil
}

func (o xrayOutbound) dialVia() string {
	if o.StreamSettings.Sockopt.DialerProxy != "" {
		return o.StreamSettings.Sockopt.DialerProxy
	}
	return o.ProxySettings.Tag
}

func (b *builder) xrayClosure(root *xrayOutbound, byTag map[string]*xrayOutbound) (draft, error) {
	d := draft{root: domain.HopID(root.Tag)}
	for o := root; ; {
		if len(d.hops) >= b.limits.GraphDepth {
			return draft{}, reject(CodeInvalidServer, "depth")
		}
		hop, err := xrayHop(*o)
		if err != nil {
			return draft{}, err
		}
		via := o.dialVia()
		next, ok := byTag[via]
		if ok && xrayUtility[strings.ToLower(next.Protocol)] {
			mask, fragments := fragmentDialer(*next)
			if !fragments || hop.Transport.Kind == domain.Hysteria {
				return draft{}, reject(CodeUnresolvedDialer, "dialerProxy")
			}
			// Last is closest to the wire, where the freedom outbound was.
			hop.Transport.TCPMasks = append(hop.Transport.TCPMasks, mask)
			hop.DialVia, via = "", ""
		}
		d.hops = append(d.hops, hop)
		if via == "" {
			return untagged(d), nil
		}
		if !ok {
			return draft{}, reject(CodeUnresolvedDialer, "dialerProxy")
		}
		o = next
	}
}

func xrayHop(o xrayOutbound) (domain.Hop, error) {
	var protocol domain.Protocol
	switch strings.ToLower(o.Protocol) {
	case "vless":
		protocol = domain.VLESS
	case "vmess":
		protocol = domain.VMess
	case "trojan":
		protocol = domain.Trojan
	case "shadowsocks":
		protocol = domain.Shadowsocks
	case "socks":
		protocol = domain.SOCKS
	case "http":
		protocol = domain.HTTPConnect
	case "hysteria":
		protocol = domain.Hysteria2
	default:
		return domain.Hop{}, reject(CodeUnsupportedProtocol, protocolName(o.Protocol))
	}
	var s xraySettings
	if len(o.Settings) > 0 && json.Unmarshal(o.Settings, &s) != nil {
		return domain.Hop{}, reject(CodeMalformedEntry, "settings")
	}
	server := s.xrayServer
	switch list := append(s.Vnext, s.Servers...); len(list) {
	case 0:
	case 1:
		server = xrayServer{}
		if json.Unmarshal(list[0], &server) != nil {
			return domain.Hop{}, reject(CodeMalformedEntry, "settings")
		}
	default:
		return domain.Hop{}, reject(CodeUnsupportedOption, "servers")
	}
	switch len(server.Users) {
	case 0:
	case 1:
		if json.Unmarshal(server.Users[0], &server.xrayUser) != nil {
			return domain.Hop{}, reject(CodeMalformedEntry, "settings")
		}
	default:
		return domain.Hop{}, reject(CodeUnsupportedOption, "users")
	}
	if server.Address == "" {
		return domain.Hop{}, reject(CodeMalformedEntry, "address")
	}
	port, err := parsePort(string(server.Port))
	if err != nil {
		return domain.Hop{}, err
	}
	if protocol == domain.Hysteria2 {
		switch stream := o.StreamSettings; {
		case string(s.Version) != "2":
			return domain.Hop{}, reject(CodeUnsupportedOption, "version") // Xray builds only version 2.
		case !strings.EqualFold(stream.Network, "hysteria") || string(stream.HysteriaSettings.Version) != "2":
			return domain.Hop{}, reject(CodeMalformedEntry, "hysteriaSettings")
		}
	}
	spec, err := xraySpec(protocol, server, o.StreamSettings)
	if err != nil {
		return domain.Hop{}, err
	}
	transport, tls, err := o.StreamSettings.options()
	if err != nil {
		return domain.Hop{}, err
	}
	return domain.Hop{
		ID: domain.HopID(o.Tag), Endpoint: domain.Endpoint{Host: server.Address, Port: port},
		Protocol: spec, Transport: transport, TLS: tls, DialVia: domain.HopID(o.dialVia()),
	}, nil
}

func xraySpec(protocol domain.Protocol, s xrayServer, stream xrayStream) (domain.ProtocolSpec, error) {
	switch protocol {
	case domain.Hysteria2:
		return newProtocol(protocol, stream.HysteriaSettings.Auth, "", "", "")
	case domain.VLESS:
		return newVLESS(s.ID, s.Flow, s.Encryption)
	case domain.VMess:
		if aid := string(s.AlterID); aid != "" && aid != "0" {
			return domain.ProtocolSpec{}, reject(CodeUnsupportedOption, "alterId")
		}
		return newProtocol(protocol, s.ID, "", vmessCipher(s.Security), "")
	case domain.Trojan:
		return newProtocol(protocol, s.Password, "", "", "")
	case domain.Shadowsocks:
		cipher, err := shadowsocksCipher(s.Method)
		if err != nil {
			return domain.ProtocolSpec{}, err
		}
		return newProtocol(protocol, s.Password, "", cipher, "")
	}
	return newProtocol(protocol, s.Pass, s.User, "", "")
}

func (s xrayStream) options() (domain.Transport, domain.TLS, error) {
	mask, ok := parseFinalMask(s.FinalMask, strings.EqualFold(s.Network, "hysteria"))
	if !ok {
		return domain.Transport{}, domain.TLS{}, reject(CodeUnsupportedOption, "finalmask")
	}
	tr := transportOptions{network: s.Network, salamander: mask.salamander}
	switch strings.ToLower(s.Network) {
	case "", "tcp", "raw":
		tr.headerType = orDefault(s.RawSettings.Header.Type, s.TCPSettings.Header.Type)
	case "ws":
		tr.path, tr.host, tr.headers = s.WSSettings.Path, s.WSSettings.Host, s.WSSettings.Headers
	case "grpc":
		tr.service, tr.host = s.GRPCSettings.ServiceName, s.GRPCSettings.Authority
		if s.GRPCSettings.MultiMode {
			tr.mode = "multi"
		}
	case "xhttp", "splithttp":
		x := s.XHTTPSettings
		if x.Path == "" && x.Host == "" && x.Mode == "" {
			x = s.SplitHTTPSettings
		}
		tr.path, tr.host, tr.mode = x.Path, x.Host, x.Mode
		if extra := strings.TrimSpace(string(x.Extra)); extra != "" && extra != "{}" && extra != "null" {
			tr.extras = "set"
		}
	}
	transport, err := tr.transport()
	if err != nil {
		return domain.Transport{}, domain.TLS{}, err
	}
	mask.apply(&transport)
	t, r := s.TLSSettings, s.RealitySettings
	opts := tlsOptions{
		security: s.Security, serverName: t.ServerName, fingerprint: t.Fingerprint, alpn: t.ALPN,
		insecure: t.AllowInsecure, pins: t.PinnedPeerCertSha256, names: t.VerifyPeerCertByName,
	}
	switch strings.ToLower(s.Security) { // Xray reads the settings of the security in use alone.
	case "tls":
		opts.ech = t.ECHConfigList
	case "reality":
		opts = tlsOptions{
			security: s.Security, serverName: r.ServerName, fingerprint: r.Fingerprint,
			publicKey: orDefault(r.PublicKey, r.Password), shortID: r.ShortID, spider: r.SpiderX, mldsa65Verify: r.Mldsa65Verify,
		}
	}
	tls, err := opts.tls()
	return transport, tls, err
}
