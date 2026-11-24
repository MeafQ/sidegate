package catalogue

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"sidegate/internal/catalogue/internal/yaml"
	"sidegate/internal/domain"
)

type clashProxy struct {
	Name              string           `yaml:"name"`
	Type              string           `yaml:"type"`
	Server            string           `yaml:"server"`
	Port              string           `yaml:"port"`
	UUID              string           `yaml:"uuid"`
	Flow              string           `yaml:"flow"`
	Cipher            string           `yaml:"cipher"`
	AlterID           string           `yaml:"alterId"`
	Password          string           `yaml:"password"`
	Username          string           `yaml:"username"`
	TLS               bool             `yaml:"tls"`
	SNI               string           `yaml:"sni"`
	ServerName        string           `yaml:"servername"`
	SkipCertVerify    bool             `yaml:"skip-cert-verify"`
	Fingerprint       string           `yaml:"fingerprint"`
	ClientFingerprint string           `yaml:"client-fingerprint"`
	ALPN              []string         `yaml:"alpn"`
	Network           string           `yaml:"network"`
	Plugin            string           `yaml:"plugin"`
	DialerProxy       string           `yaml:"dialer-proxy"`
	PacketEncoding    string           `yaml:"packet-encoding"`
	Encryption        string           `yaml:"encryption"`
	WSOpts            clashHTTPOpts    `yaml:"ws-opts"`
	XHTTPOpts         clashHTTPOpts    `yaml:"xhttp-opts"`
	GRPCOpts          clashGRPCOpts    `yaml:"grpc-opts"`
	RealityOpts       clashRealityOpts `yaml:"reality-opts"`
	ECHOpts           clashECHOpts     `yaml:"ech-opts"`
	Ports             string           `yaml:"ports"`
	Obfs              string           `yaml:"obfs"`
	ObfsPassword      string           `yaml:"obfs-password"`
}

type clashHTTPOpts struct {
	Path    string            `yaml:"path"`
	Host    string            `yaml:"host"`
	Mode    string            `yaml:"mode"`
	Headers map[string]string `yaml:"headers"`
}

type clashGRPCOpts struct {
	ServiceName string `yaml:"grpc-service-name"`
}

type clashRealityOpts struct {
	PublicKey string `yaml:"public-key"`
	ShortID   string `yaml:"short-id"`
}

type clashECHOpts struct {
	Enable bool   `yaml:"enable"`
	Config string `yaml:"config"`
}

// parseClash imports the proxies list only. Proxy groups, rules, and DNS are
// the provider's routing and never become servers.
func (b *builder) parseClash(data []byte) error {
	var root yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.Budget(b.limits.Nodes)
	if err := dec.Decode(&root); err != nil {
		return yamlError(err)
	}
	for {
		// A feed is one document; a second one is a truncated or mixed response.
		// Empty ones, as a trailing "---" leaves, carry nothing.
		var next yaml.Node
		if err := dec.Decode(&next); err == io.EOF {
			break
		} else if err != nil {
			return yamlError(err)
		} else if !emptyDocument(&next) {
			return ErrMalformed
		}
	}
	budget := b.limits.Nodes
	if !withinBudget(&root, &budget, 0) {
		return ErrTooLarge
	}
	list, ok := proxyList(&root)
	if !ok {
		return ErrMalformed
	}
	if err := b.charge(len(list)); err != nil {
		return err
	}
	if len(list) == 0 {
		b.doc.explicitlyEmpty = true
	}
	proxies := make([]clashProxy, len(list))
	valid := make([]bool, len(proxies))
	byName, repeated := map[string]int{}, map[string]bool{}
	for i, n := range list {
		if n.Decode(&proxies[i]) == nil {
			valid[i] = true
			_, seen := byName[proxies[i].Name]
			repeated[proxies[i].Name] = repeated[proxies[i].Name] || seen
			byName[proxies[i].Name] = i
		}
	}
	for i, p := range proxies {
		if !valid[i] {
			if err := b.addServer(i+1, draft{}, reject(CodeMalformedEntry, "proxy")); err != nil {
				return err
			}
			continue
		}
		d, err := b.clashClosure(i, proxies, valid, byName, repeated)
		d.label = p.Name
		if err := b.addServer(i+1, d, err); err != nil {
			return err
		}
	}
	return nil
}

func yamlError(err error) error {
	if errors.Is(err, yaml.ErrBudget) {
		return ErrTooLarge
	}
	return ErrMalformed
}

func emptyDocument(n *yaml.Node) bool {
	return len(n.Content) == 1 && n.Content[0].Kind == yaml.ScalarNode && n.Content[0].Tag == "!!null" && n.Content[0].Value == ""
}

// proxyList returns the top-level proxies sequence. It fails when the key is
// absent, repeated, null, or not a sequence.
func proxyList(root *yaml.Node) ([]*yaml.Node, bool) {
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, false
	}
	var list *yaml.Node
	m := root.Content[0].Content
	for i := 0; i+1 < len(m); i += 2 {
		if m[i].Value != "proxies" {
			continue
		}
		if list != nil {
			return nil, false
		}
		if list = m[i+1]; list.Kind == yaml.AliasNode {
			list = list.Alias
		}
		if list == nil || list.Kind != yaml.SequenceNode {
			return nil, false
		}
	}
	if list == nil {
		return nil, false
	}
	return list.Content, true
}

// withinBudget counts nodes with aliases expanded, so an alias bomb exhausts the
// budget before any decode expands it.
func withinBudget(n *yaml.Node, budget *int, depth int) bool {
	*budget--
	if *budget < 0 || depth > 128 {
		return false
	}
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		return withinBudget(n.Alias, budget, depth+1)
	}
	for _, c := range n.Content {
		if !withinBudget(c, budget, depth+1) {
			return false
		}
	}
	return true
}

func (b *builder) clashClosure(i int, proxies []clashProxy, valid []bool, byName map[string]int, repeated map[string]bool) (draft, error) {
	d := draft{root: domain.HopID(proxies[i].Name)}
	for {
		if len(d.hops) >= b.limits.GraphDepth {
			return draft{}, reject(CodeInvalidServer, "depth")
		}
		p := proxies[i]
		hop, err := clashHop(p)
		if err != nil {
			return draft{}, err
		}
		d.hops = append(d.hops, hop)
		if p.DialerProxy == "" {
			return d, nil
		}
		if repeated[p.DialerProxy] {
			return draft{}, reject(CodeDuplicateTag, "dialer-proxy")
		}
		next, ok := byName[p.DialerProxy]
		if !ok || !valid[next] {
			return draft{}, reject(CodeUnresolvedDialer, "dialer-proxy") // Groups are not imported.
		}
		i = next
	}
}

func clashHop(p clashProxy) (domain.Hop, error) {
	var spec domain.ProtocolSpec
	var err error
	serverName := p.ServerName
	security, network, salamander := "", p.Network, ""
	if p.TLS {
		security = "tls"
	}
	switch strings.ToLower(p.Type) {
	case "vless":
		spec, err = newVLESS(p.UUID, p.Flow, p.Encryption)
	case "vmess":
		if aid := strings.TrimSpace(p.AlterID); aid != "" && aid != "0" {
			return domain.Hop{}, reject(CodeUnsupportedOption, "alterId")
		}
		spec, err = newProtocol(domain.VMess, p.UUID, "", vmessCipher(p.Cipher), "")
	case "trojan":
		security, serverName = "tls", p.SNI
		spec, err = newProtocol(domain.Trojan, p.Password, "", "", "")
	case "ss":
		if p.Plugin != "" {
			return domain.Hop{}, reject(CodeUnsupportedPlugin, "plugin")
		}
		var cipher string
		if cipher, err = shadowsocksCipher(p.Cipher); err == nil {
			spec, err = newProtocol(domain.Shadowsocks, p.Password, "", cipher, "")
		}
	case "socks5":
		serverName = p.SNI
		spec, err = newProtocol(domain.SOCKS, p.Password, p.Username, "", "")
	case "http":
		serverName = p.SNI
		spec, err = newProtocol(domain.HTTPConnect, p.Password, p.Username, "", "")
	case "hysteria2":
		if p.Ports != "" {
			return domain.Hop{}, reject(CodeUnsupportedOption, "ports")
		}
		if salamander, err = hysteriaObfs(p.Obfs, p.ObfsPassword); err != nil {
			return domain.Hop{}, err
		}
		security, serverName, network = "tls", p.SNI, "hysteria"
		spec, err = newProtocol(domain.Hysteria2, p.Password, "", "", "")
	default:
		return domain.Hop{}, reject(CodeUnsupportedProtocol, protocolName(p.Type))
	}
	if err != nil {
		return domain.Hop{}, err
	}
	if p.Server == "" {
		return domain.Hop{}, reject(CodeMalformedEntry, "server")
	}
	port, err := parsePort(p.Port)
	if err != nil {
		return domain.Hop{}, err
	}
	ech := ""
	if p.ECHOpts.Enable {
		if ech = p.ECHOpts.Config; ech == "" { // mihomo then asks the client's own resolver, which is no part of the server.
			return domain.Hop{}, reject(CodeUnsupportedOption, "ech-opts")
		}
	}
	tr := transportOptions{network: network, salamander: salamander}
	switch strings.ToLower(p.Network) {
	case "ws":
		tr.path, tr.headers = p.WSOpts.Path, p.WSOpts.Headers
	case "grpc":
		tr.service = p.GRPCOpts.ServiceName
	case "xhttp":
		tr.path, tr.host, tr.mode = p.XHTTPOpts.Path, p.XHTTPOpts.Host, p.XHTTPOpts.Mode
	}
	transport, err := tr.transport()
	if err != nil {
		return domain.Hop{}, err
	}
	opts := tlsOptions{
		security: security, serverName: serverName, fingerprint: p.ClientFingerprint, alpn: p.ALPN,
		insecure: p.SkipCertVerify,
	}
	if p.Fingerprint != "" {
		opts.pins = []string{p.Fingerprint} // mihomo's fingerprint pins the certificate's SHA-256.
	}
	if p.RealityOpts.PublicKey != "" {
		opts = tlsOptions{
			security: "reality", serverName: serverName, fingerprint: p.ClientFingerprint,
			publicKey: p.RealityOpts.PublicKey, shortID: p.RealityOpts.ShortID,
		}
	}
	opts.ech = ech
	tls, err := opts.tls()
	if err != nil {
		return domain.Hop{}, err
	}
	return domain.Hop{
		ID: domain.HopID(p.Name), Endpoint: domain.Endpoint{Host: p.Server, Port: port},
		Protocol: spec, Transport: transport, TLS: tls, DialVia: domain.HopID(p.DialerProxy),
	}, nil
}
