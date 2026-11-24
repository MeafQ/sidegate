package catalogue

import (
	"encoding/json"
	"strings"

	"sidegate/internal/domain"
)

type singBoxOutbound struct {
	Type, Tag, Server, Detour string
	ServerPort                flexString `json:"server_port"`
	UUID, Flow, Security      string
	AlterID                   flexString `json:"alter_id"`
	Password, Method, Plugin  string
	Username                  string
	Version                   string
	PacketEncoding            string          `json:"packet_encoding"`
	ServerPorts               flexList        `json:"server_ports"`
	Obfs                      json.RawMessage `json:"obfs"` // An object for hysteria2, a string for hysteria v1.
	TLS                       struct {
		Enabled    bool
		ServerName string `json:"server_name"`
		Insecure   bool
		ALPN       flexList
		UTLS       struct {
			Enabled     bool
			Fingerprint string
		} `json:"utls"`
		Reality struct {
			Enabled   bool
			PublicKey string `json:"public_key"`
			ShortID   string `json:"short_id"`
		}
		ECH struct {
			Enabled bool
			Config  json.RawMessage
		} `json:"ech"`
	}
	Transport struct {
		Type, Path, Host string
		ServiceName      string `json:"service_name"`
		Headers          map[string]listable
	}
}

// listable is sing-box's string-or-list option. A string is one whole value:
// header values carry commas and tildes ("en-US,en;q=0.9").
type listable []string

func (l *listable) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*l = nonEmpty([]string{s})
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*l = nonEmpty(list)
	return nil
}

var singBoxUtility = map[string]bool{"direct": true, "block": true, "dns": true, "selector": true, "urltest": true}

func (b *builder) parseSingBox(data []byte) error {
	var doc struct {
		Outbounds json.RawMessage `json:"outbounds"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return ErrMalformed
	}
	outbounds, err := decodeList[singBoxOutbound](b, doc.Outbounds)
	if err != nil {
		return err
	}
	byTag := make(map[string]*singBoxOutbound, len(outbounds))
	used, repeated := map[string]bool{}, map[string]bool{}
	for i := range outbounds {
		o := &outbounds[i]
		if o.Detour != "" {
			used[o.Detour] = true
		}
		if o.Tag == "" {
			continue
		}
		_, seen := byTag[o.Tag]
		repeated[o.Tag] = repeated[o.Tag] || seen
		byTag[o.Tag] = o
	}
	for i := range outbounds {
		o := &outbounds[i]
		if singBoxUtility[strings.ToLower(o.Type)] || used[o.Tag] {
			continue
		}
		d, err := b.singBoxClosure(o, byTag, repeated)
		d.label = o.Tag
		if err := b.addServer(i+1, d, err); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) singBoxClosure(root *singBoxOutbound, byTag map[string]*singBoxOutbound, repeated map[string]bool) (draft, error) {
	d := draft{root: domain.HopID(root.Tag)}
	for o := root; ; {
		if len(d.hops) >= b.limits.GraphDepth {
			return draft{}, reject(CodeInvalidServer, "depth")
		}
		hop, err := singBoxHop(*o)
		if err != nil {
			return draft{}, err
		}
		d.hops = append(d.hops, hop)
		if o.Detour == "" {
			return untagged(d), nil
		}
		if repeated[o.Detour] {
			return draft{}, reject(CodeDuplicateTag, "detour")
		}
		next, ok := byTag[o.Detour]
		if !ok || singBoxUtility[strings.ToLower(next.Type)] {
			return draft{}, reject(CodeUnresolvedDialer, "detour")
		}
		o = next
	}
}

// singBoxECH is the base64 of an ECH config sing-box writes as PEM, in one string or a list of lines.
// It is empty when the outbound carries none, as one that asks a resolver or names a file does.
func singBoxECH(raw json.RawMessage) string {
	var lines listable
	if len(raw) == 0 || json.Unmarshal(raw, &lines) != nil {
		return ""
	}
	var body strings.Builder
	for line := range strings.Lines(strings.Join(lines, "\n")) {
		if line = strings.TrimSpace(line); !strings.HasPrefix(line, "-----") {
			body.WriteString(line)
		}
	}
	return body.String()
}

func singBoxHop(o singBoxOutbound) (domain.Hop, error) {
	var spec domain.ProtocolSpec
	var err error
	network, salamander := o.Transport.Type, ""
	switch strings.ToLower(o.Type) {
	case "vless":
		if o.PacketEncoding != "" && o.PacketEncoding != "xudp" {
			return domain.Hop{}, reject(CodeUnsupportedOption, "packet_encoding")
		}
		spec, err = newProtocol(domain.VLESS, o.UUID, "", "", o.Flow)
	case "vmess":
		if aid := string(o.AlterID); aid != "" && aid != "0" {
			return domain.Hop{}, reject(CodeUnsupportedOption, "alter_id")
		}
		spec, err = newProtocol(domain.VMess, o.UUID, "", vmessCipher(o.Security), "")
	case "trojan":
		spec, err = newProtocol(domain.Trojan, o.Password, "", "", "")
	case "shadowsocks":
		if o.Plugin != "" {
			return domain.Hop{}, reject(CodeUnsupportedPlugin, "plugin")
		}
		var cipher string
		if cipher, err = shadowsocksCipher(o.Method); err == nil {
			spec, err = newProtocol(domain.Shadowsocks, o.Password, "", cipher, "")
		}
	case "socks":
		if o.Version != "" && o.Version != "5" {
			return domain.Hop{}, reject(CodeUnsupportedOption, "version")
		}
		spec, err = newProtocol(domain.SOCKS, o.Password, o.Username, "", "")
	case "http":
		spec, err = newProtocol(domain.HTTPConnect, o.Password, o.Username, "", "")
	case "hysteria2":
		if len(o.ServerPorts) > 0 {
			return domain.Hop{}, reject(CodeUnsupportedOption, "server_ports")
		}
		var obfs struct{ Type, Password string }
		if len(o.Obfs) > 0 && json.Unmarshal(o.Obfs, &obfs) != nil {
			return domain.Hop{}, reject(CodeMalformedEntry, "obfs")
		}
		if salamander, err = hysteriaObfs(obfs.Type, obfs.Password); err != nil {
			return domain.Hop{}, err
		}
		network = "hysteria"
		spec, err = newProtocol(domain.Hysteria2, o.Password, "", "", "")
	default:
		return domain.Hop{}, reject(CodeUnsupportedProtocol, protocolName(o.Type))
	}
	if err != nil {
		return domain.Hop{}, err
	}
	if o.Server == "" {
		return domain.Hop{}, reject(CodeMalformedEntry, "server")
	}
	port, err := parsePort(string(o.ServerPort))
	if err != nil {
		return domain.Hop{}, err
	}
	tr := transportOptions{network: network, path: o.Transport.Path, service: o.Transport.ServiceName, salamander: salamander}
	for name, values := range o.Transport.Headers {
		switch {
		case name == "Host" && len(values) > 0:
			tr.host = values[0]
		case len(values) == 1:
			if tr.headers == nil {
				tr.headers = map[string]string{}
			}
			tr.headers[name] = values[0]
		case len(values) > 1:
			return domain.Hop{}, reject(CodeUnsupportedOption, "headers") // Xray sends one value per header.
		}
	}
	transport, err := tr.transport()
	if err != nil {
		return domain.Hop{}, err
	}
	t := o.TLS
	opts := tlsOptions{serverName: t.ServerName, alpn: t.ALPN, insecure: t.Insecure}
	if t.ECH.Enabled {
		if opts.ech = singBoxECH(t.ECH.Config); opts.ech == "" {
			return domain.Hop{}, reject(CodeUnsupportedOption, "ech")
		}
	}
	if t.UTLS.Enabled {
		opts.fingerprint = t.UTLS.Fingerprint
	}
	switch {
	case t.Reality.Enabled:
		opts.security, opts.publicKey, opts.shortID = "reality", t.Reality.PublicKey, t.Reality.ShortID
	case t.Enabled:
		opts.security = "tls"
	}
	tls, err := opts.tls()
	if err != nil {
		return domain.Hop{}, err
	}
	return domain.Hop{
		ID: domain.HopID(o.Tag), Endpoint: domain.Endpoint{Host: o.Server, Port: port},
		Protocol: spec, Transport: transport, TLS: tls, DialVia: domain.HopID(o.Detour),
	}, nil
}
