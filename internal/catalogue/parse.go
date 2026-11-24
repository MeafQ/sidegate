// Package catalogue owns provider interpretation, lineage continuity, and fetch
// progress. It cannot mutate slots, the database, the core, or the gateway.
// Callers see parsed documents, lineage changes, and deadlines; HTTP validators,
// backoff counters, and response headers never leave the package.
package catalogue

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"sidegate/internal/domain"
)

type Format uint8

const (
	Detect     Format = iota
	ShareLinks        // Plain or base64-wrapped share link lists.
	XrayJSON
	SingBoxJSON
	ClashYAML
	SIP008
)

// Limits bound one feed. Nodes bounds what a decoder may build from a JSON or
// YAML document: in JSON the values under the keys the parsers read, counted
// before decoding; in YAML every node, comment, document, and directive, charged
// by the parser as it builds them, since it builds the whole document.
// Entries bounds its servers, and Entries times GraphDepth bounds the outbounds,
// proxies, and configs it may list, since each server's closure has at most
// GraphDepth hops.
type Limits struct {
	CompressedBytes, DecodedBytes         int64
	Entries, GraphDepth, Nodes, Redirects int
	Timeout                               time.Duration
}

// DefaultLimits bounds one feed. Any zero field passed to Parse or NewFetcher
// takes its value from here.
func DefaultLimits() Limits {
	return Limits{
		CompressedBytes: 4 << 20,
		DecodedBytes:    8 << 20,
		Entries:         2000,
		GraphDepth:      domain.MaxHops,
		Nodes:           200_000,
		Redirects:       5,
		Timeout:         30 * time.Second,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.CompressedBytes <= 0 {
		l.CompressedBytes = d.CompressedBytes
	}
	if l.DecodedBytes <= 0 {
		l.DecodedBytes = d.DecodedBytes
	}
	if l.Entries <= 0 {
		l.Entries = d.Entries
	}
	if l.GraphDepth <= 0 || l.GraphDepth > domain.MaxHops {
		l.GraphDepth = d.GraphDepth
	}
	if l.Nodes <= 0 {
		l.Nodes = d.Nodes
	}
	if l.Redirects <= 0 {
		l.Redirects = d.Redirects
	}
	if l.Timeout <= 0 {
		l.Timeout = d.Timeout
	}
	return l
}

var (
	ErrTooLarge        = errors.New("catalogue: document exceeds the size limit")
	ErrTooManyEntries  = errors.New("catalogue: document exceeds the entry limit")
	ErrMalformed       = errors.New("catalogue: malformed document")
	ErrUnknownFormat   = errors.New("catalogue: unrecognised document format")
	ErrNoUsableEntries = errors.New("catalogue: document has no usable entries")
)

// DiagnosticCode is why the parser skipped an entry. It is a stable safe code, never a raw
// credential, URL, or input snippet. The page has words for each constant.
type DiagnosticCode string

const (
	CodeUnsupportedProtocol  DiagnosticCode = "unsupported_protocol"
	CodeUnsupportedTransport DiagnosticCode = "unsupported_transport"
	CodeUnsupportedSecurity  DiagnosticCode = "unsupported_security"
	CodeUnsupportedOption    DiagnosticCode = "unsupported_option"
	CodeUnsupportedCipher    DiagnosticCode = "unsupported_cipher"
	CodeUnsupportedPlugin    DiagnosticCode = "unsupported_plugin"
	CodeInsecureTLS          DiagnosticCode = "insecure_tls"
	CodeMalformedEntry       DiagnosticCode = "malformed_entry"
	CodeInvalidServer        DiagnosticCode = "invalid_server"
	CodeUnresolvedDialer     DiagnosticCode = "unresolved_dialer"
	CodeDuplicateTag         DiagnosticCode = "duplicate_tag" // Dialer references would be ambiguous.
)

type Diagnostic struct {
	Entry int
	Code  DiagnosticCode
	Field string // A fixed option name, never input text.
}

// Entry is one server, merged across every place the document lists it. Its Naming has no Home
// until Reconcile keys the configs, so home holds the index of its home config, or -1.
type Entry struct {
	Label    string
	Naming   domain.Naming
	Identity domain.Identity
	Server   domain.Server
	Country  domain.Country
	Seen     []Sighting
	home     int
}

// Sighting is domain.Occurrence before lineage IDs exist, with the config as an index
// into the document's reported configs.
type Sighting struct {
	Config      int
	ConfigName  string
	ConfigPlace int
	Tag         string
	Position    int
}

// config is what a parser reports about one config of the document.
type config struct {
	name    string
	place   int // Its item in the provider's list from 1, counting the items no config was reported for.
	roots   int // Proxy roots, valid or not.
	servers int // Proxy roots that became an entry.
	skipped []domain.Skip
}

// A Document represents a complete snapshot. A failed or truncated parse is not
// an empty successful source and can never mark all old lineages missing.
type Document struct {
	entries         []Entry
	configs         []config
	diagnostics     []Diagnostic
	explicitlyEmpty bool
}

func (d Document) Entries() []Entry          { return slices.Clone(d.entries) }
func (d Document) Diagnostics() []Diagnostic { return slices.Clone(d.diagnostics) }

// Parse reads one complete feed into entries merged by {ProviderID, Strong}. Entries the
// core cannot run become diagnostics beside the entries it can. When nothing is usable,
// Parse returns the diagnostics with ErrNoUsableEntries; only a feed that lists zero
// servers on purpose parses as an empty document.
func Parse(data []byte, format Format, limits Limits) (Document, error) {
	return parse(data, format, limits, &countryResolver{})
}

func parse(data []byte, format Format, limits Limits, countries *countryResolver) (Document, error) {
	limits = limits.withDefaults()
	if int64(len(data)) > limits.DecodedBytes {
		return Document{}, ErrTooLarge
	}
	b := &builder{limits: limits, elements: limits.Entries * limits.GraphDepth}
	if err := b.parse(data, format, true); err != nil {
		return Document{}, err
	}
	b.doc.merge()
	b.doc.name(countries)
	if len(b.doc.entries) == 0 && !b.doc.explicitlyEmpty {
		return b.doc, ErrNoUsableEntries
	}
	if len(b.doc.entries) > 0 {
		b.doc.explicitlyEmpty = false
	}
	return b.doc, nil
}

var clashProxies = regexp.MustCompile(`(?m)^proxies\s*:`)

func (b *builder) parse(data []byte, format Format, unwrap bool) error {
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if len(data) > 0 && (data[0] == '{' || data[0] == '[') {
		n, ok := jsonValueBound(data)
		if !ok {
			return ErrMalformed
		}
		if n > b.limits.Nodes {
			return ErrTooLarge
		}
	}
	if format == Detect {
		format = sniff(data)
	}
	switch format {
	case ShareLinks:
		if !bytes.Contains(data, []byte("://")) {
			if decoded, ok := decodeBase64(data); ok && unwrap {
				return b.parse(decoded, ShareLinks, false)
			}
			return ErrMalformed
		}
		return b.parseLinks(data)
	case XrayJSON:
		return b.parseXray(data)
	case SingBoxJSON:
		return b.parseSingBox(data)
	case ClashYAML:
		return b.parseClash(data)
	case SIP008:
		return b.parseSIP008(data)
	}
	if decoded, ok := decodeBase64(data); ok && unwrap {
		return b.parse(decoded, Detect, false)
	}
	return ErrUnknownFormat
}

func sniff(data []byte) Format {
	if len(data) == 0 {
		return Detect
	}
	if data[0] == '[' {
		return XrayJSON
	}
	if data[0] == '{' {
		var probe struct {
			Servers   json.RawMessage   `json:"servers"`
			Proxies   json.RawMessage   `json:"proxies"`
			Outbounds []json.RawMessage `json:"outbounds"`
		}
		if json.Unmarshal(data, &probe) != nil {
			return XrayJSON // Reported as malformed by the Xray parser.
		}
		switch {
		case probe.Servers != nil:
			return SIP008
		case probe.Proxies != nil:
			return ClashYAML
		}
		for _, o := range probe.Outbounds {
			var kind struct {
				Type     string `json:"type"`
				Protocol string `json:"protocol"`
			}
			if json.Unmarshal(o, &kind) == nil && kind.Type != "" && kind.Protocol == "" {
				return SingBoxJSON
			}
		}
		return XrayJSON
	}
	if clashProxies.Match(data) {
		return ClashYAML
	}
	if bytes.Contains(data, []byte("://")) {
		return ShareLinks
	}
	return Detect
}

// decodeList decodes a JSON list one element at a time, charging each to the
// document's element budget before building it. Absent or null is empty.
func decodeList[T any](b *builder, raw json.RawMessage) ([]T, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return nil, ErrMalformed
	}
	var raws []json.RawMessage
	for dec.More() {
		if err := b.charge(1); err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return nil, ErrMalformed
		}
		raws = append(raws, raw)
	}
	out := make([]T, len(raws)) // Sized once: growing a slice of large structs copies each one several times.
	for i, raw := range raws {
		if json.Unmarshal(raw, &out[i]) != nil {
			return nil, ErrMalformed
		}
	}
	return out, nil
}

func decodeBase64(data []byte) ([]byte, bool) {
	compact := bytes.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, data)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if out, err := enc.DecodeString(string(compact)); err == nil && len(out) > 0 {
			return out, true
		}
	}
	return nil, false
}

// builder collects one document. Every parser reports each config it finds and each
// entry of that config through add, so entry indexes and the entry limit are enforced in
// one place. Every occurrence costs one entry, so a document holds at most Limits.Entries
// of them, however few servers they merge into.
type builder struct {
	limits   Limits
	doc      Document
	seen     int
	elements int // List items the document may still build.
}

func (b *builder) charge(n int) error {
	if n > b.elements {
		return ErrTooManyEntries
	}
	b.elements -= n
	return nil
}

// rejection is the reason one entry cannot become a server.
type rejection struct {
	code  DiagnosticCode
	field string
}

func (r rejection) Error() string { return string(r.code) + ":" + r.field }

func reject(code DiagnosticCode, field string) error { return rejection{code: code, field: field} }

// draft is a parsed entry before domain validation. label is the outbound's tag, or a
// plain server's name, and index its place in the config's outbound list from 1.
type draft struct {
	label      string
	index      int
	providerID string
	root       domain.HopID
	hops       []domain.Hop
}

// untagged gives a closure whose root outbound has no tag a root ID its chain doesn't use. Nothing can
// dial through an outbound with no tag, so the ID only has to be unique within its own closure.
func untagged(d draft) draft {
	if d.root != "" {
		return d
	}
	id := domain.HopID("proxy")
	for n := 2; slices.ContainsFunc(d.hops[1:], func(h domain.Hop) bool { return h.ID == id }); n++ {
		id = domain.HopID("proxy" + strconv.Itoa(n))
	}
	d.root, d.hops[0].ID = id, id
	return d
}

func (b *builder) next() (int, error) {
	if b.seen >= b.limits.Entries {
		return 0, ErrTooManyEntries
	}
	b.seen++
	return b.seen - 1, nil
}

// openConfig reports the config at place, and add then takes its roots in provider order.
func (b *builder) openConfig(name string, place int) int {
	b.doc.configs = append(b.doc.configs, config{name: strings.TrimSpace(name), place: place})
	return len(b.doc.configs) - 1
}

// addServer records a plain server: one config of one outbound, which the format names once. The
// name is the config's remarks, and the outbound has no tag.
func (b *builder) addServer(place int, d draft, err error) error {
	at := b.openConfig(d.label, place)
	d.label = ""
	return b.add(at, d, err)
}

// add records one root of a config from its draft or the error that rejected it. A rejected root
// keeps its position, and the config lists it as refused.
func (b *builder) add(at int, d draft, err error) error {
	index, limitErr := b.next()
	if limitErr != nil {
		return limitErr
	}
	c := &b.doc.configs[at]
	c.roots++
	position := c.roots
	refuse := func(r rejection) error {
		b.doc.diagnostics = append(b.doc.diagnostics, Diagnostic{Entry: index, Code: r.code, Field: r.field})
		c.skipped = append(c.skipped, domain.Skip{Index: max(d.index, 1), Tag: strings.TrimSpace(d.label), Reason: domain.SkipRefused,
			Position: position, Code: string(r.code), Field: r.field})
		return nil
	}
	if err != nil {
		var r rejection
		if !errors.As(err, &r) {
			r = rejection{code: CodeMalformedEntry}
		}
		return refuse(r)
	}
	if len(d.hops) > b.limits.GraphDepth {
		return refuse(rejection{code: CodeInvalidServer, field: "depth"})
	}
	server, err := domain.NewServer(d.root, d.hops)
	if err != nil {
		return refuse(rejection{code: CodeInvalidServer})
	}
	c.servers++
	b.doc.entries = append(b.doc.entries, Entry{Identity: identify(server, d.providerID), Server: server,
		Seen: []Sighting{{Config: at, ConfigName: c.name, ConfigPlace: c.place, Tag: strings.TrimSpace(d.label), Position: position}}})
	return nil
}

func (c *config) sortSkipped() {
	slices.SortStableFunc(c.skipped, func(a, b domain.Skip) int { return cmp.Compare(a.Index, b.Index) })
}

// skip lists an outbound of a config that is no proxy root.
func (b *builder) skip(at int, s domain.Skip) {
	s.Tag = strings.TrimSpace(s.Tag)
	b.doc.configs[at].skipped = append(b.doc.configs[at].skipped, s)
}

// protocolName keeps a diagnostic field safe: known names pass, others collapse.
func protocolName(name string) string {
	name = strings.ToLower(name)
	switch name {
	case "hysteria2", "hy2", "hysteria", "tuic", "wireguard", "wg", "ssr", "shadowsocksr", "snell",
		"ssh", "tor", "shadowtls", "anytls", "mieru", "juicity", "naive", "socks", "http", "https":
		return name
	}
	return "other"
}

// tlsOptions is the security part of an entry, in the vocabulary every format
// shares once its field names are mapped.
type tlsOptions struct {
	security                   string
	serverName, fingerprint    string
	alpn                       []string
	publicKey, shortID, spider string
	insecure                   bool
	pins, names                []string
	mldsa65Verify              string // Reality only.
	ech                        string // TLS only.
}

// tls maps provider security settings onto what Xray v26 can verify. An insecure
// flag survives only as a pin or a verified name; otherwise the entry is refused.
func (o tlsOptions) tls() (domain.TLS, error) {
	security := strings.ToLower(o.security)
	switch {
	case o.ech != "" && security != "tls":
		return domain.TLS{}, reject(CodeUnsupportedOption, "echConfigList")
	case o.mldsa65Verify != "" && security != "reality":
		return domain.TLS{}, reject(CodeUnsupportedOption, "mldsa65Verify")
	}
	switch security {
	case "", "none":
		return domain.TLS{}, nil
	case "tls":
		if o.ech != "" && !domain.ValidECHConfigList(o.ech) {
			return domain.TLS{}, reject(CodeMalformedEntry, "echConfigList")
		}
		pins := make([]string, 0, len(o.pins))
		for _, p := range o.pins {
			if p = strings.ReplaceAll(strings.TrimSpace(p), ":", ""); p == "" {
				continue
			}
			// Xray decodes each pin as hex, optionally colon-separated, and requires 32 bytes.
			sum, err := hex.DecodeString(p)
			if err != nil || len(sum) != sha256.Size {
				return domain.TLS{}, reject(CodeMalformedEntry, "pinnedPeerCertSha256")
			}
			pins = append(pins, hex.EncodeToString(sum))
		}
		names := nonEmpty(o.names)
		if o.insecure && len(pins) == 0 && len(names) == 0 {
			return domain.TLS{}, reject(CodeInsecureTLS, "allowInsecure")
		}
		fp := strings.ToLower(o.fingerprint)
		if fp != "" && fp != "unsafe" && !xrayFingerprints[fp] {
			return domain.TLS{}, reject(CodeUnsupportedOption, "fingerprint")
		}
		return domain.TLS{
			Enabled: true, ServerName: o.serverName, ALPN: nonEmpty(o.alpn),
			Fingerprint: fp, PinnedCertSHA256: nilIfEmpty(pins), VerifyNames: names, ECHConfigList: o.ech,
		}, nil
	case "reality":
		if o.publicKey == "" || o.serverName == "" {
			return domain.TLS{}, reject(CodeMalformedEntry, "reality")
		}
		fp := strings.ToLower(o.fingerprint)
		if fp == "" {
			fp = "chrome"
		}
		if fp == "hellogolang" || !xrayFingerprints[fp] {
			return domain.TLS{}, reject(CodeUnsupportedOption, "fingerprint")
		}
		shortID, err := realityShortID(o.shortID)
		if err != nil {
			return domain.TLS{}, err
		}
		if o.mldsa65Verify != "" && !domain.ValidMLDSA65Verify(o.mldsa65Verify) {
			return domain.TLS{}, reject(CodeMalformedEntry, "mldsa65Verify")
		}
		return domain.TLS{
			Enabled: true, ServerName: o.serverName, Fingerprint: fp,
			Reality: &domain.Reality{PublicKey: o.publicKey, ShortID: shortID, SpiderX: o.spider, MLDSA65Verify: o.mldsa65Verify},
		}, nil
	}
	return domain.TLS{}, reject(CodeUnsupportedSecurity, "security")
}

// realityShortID is a short ID the way Xray reads it: up to 16 hex digits
// decoded into 8 bytes and zero-padded (infra/conf/transport_security.go). Case,
// trailing zero digits, and an empty ID are spellings of the same ID, so it is
// kept as all 16 lowercase digits.
func realityShortID(s string) (string, error) {
	var id [8]byte
	if len(s) > 2*len(id) {
		return "", reject(CodeMalformedEntry, "shortId")
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return "", reject(CodeMalformedEntry, "shortId")
	}
	return hex.EncodeToString(id[:]), nil
}

// xrayFingerprints are the names Xray v26 resolves through tls.GetFingerprint
// (transport/internet/tls/tls.go, PresetFingerprints, ModernFingerprints, and
// OtherFingerprints). Any other name fails its config build with unknown
// "fingerprint" (infra/conf/transport_security.go 184 for Reality, 356 for TLS).
// TLS also takes "unsafe"; Reality refuses "unsafe" and "hellogolang" (180).
var xrayFingerprints = func() map[string]bool {
	m := map[string]bool{}
	for _, name := range strings.Fields(`
		chrome firefox safari ios android edge 360 qq random randomized randomizednoalpn
		hellofirefox_120 hellofirefox_148 hellochrome_120 hellochrome_131 hellochrome_133 helloios_13
		helloios_14 helloedge_106 hellosafari_26_3 hello360_11_0 helloqq_11_1
		hellogolang hellorandomized hellorandomizedalpn hellorandomizednoalpn hellofirefox_auto
		hellofirefox_55 hellofirefox_56 hellofirefox_63 hellofirefox_65 hellofirefox_99 hellofirefox_102
		hellofirefox_105 hellochrome_auto hellochrome_58 hellochrome_62 hellochrome_70 hellochrome_72
		hellochrome_83 hellochrome_87 hellochrome_96 hellochrome_100 hellochrome_102 hellochrome_106_shuffle
		helloios_auto helloios_11_1 helloios_12_1 helloandroid_11_okhttp helloedge_85 helloedge_auto
		hellosafari_16_0 hellosafari_auto hello360_auto hello360_7_5 helloqq_auto
		hellochrome_100_psk hellochrome_112_psk_shuf hellochrome_114_padding_psk_shuf
		hellochrome_115_pq hellochrome_115_pq_psk hellochrome_120_pq`) {
		m[name] = true
	}
	return m
}()

type transportOptions struct {
	network, headerType               string
	path, host, service, mode, extras string
	headers                           map[string]string
	salamander                        string // Hysteria only.
}

func (o transportOptions) transport() (domain.Transport, error) {
	if host, ok := o.headers["Host"]; ok {
		o.host = orDefault(o.host, host)
		o.headers = maps.Clone(o.headers)
		delete(o.headers, "Host")
		if len(o.headers) == 0 {
			o.headers = nil
		}
	}
	switch strings.ToLower(o.network) {
	case "", "tcp", "raw":
		if o.headerType != "" && !strings.EqualFold(o.headerType, "none") {
			return domain.Transport{}, reject(CodeUnsupportedOption, "headerType")
		}
		return domain.Transport{Kind: domain.TCP}, nil
	case "ws", "websocket":
		return domain.Transport{Kind: domain.WebSocket, Path: orDefault(o.path, "/"), Host: o.host, Headers: o.headers}, nil
	case "grpc", "gun":
		mode := ""
		switch strings.ToLower(o.mode) {
		case "", "gun":
		case "multi":
			mode = "multi"
		default:
			return domain.Transport{}, reject(CodeUnsupportedOption, "mode")
		}
		return domain.Transport{Kind: domain.GRPC, Service: o.service, Host: o.host, Mode: mode}, nil
	case "xhttp", "splithttp":
		if o.extras != "" {
			return domain.Transport{}, reject(CodeUnsupportedOption, "extra")
		}
		mode := strings.ToLower(o.mode)
		switch mode {
		case "", "auto":
			mode = ""
		case "packet-up", "stream-up", "stream-one":
		default:
			return domain.Transport{}, reject(CodeUnsupportedOption, "mode")
		}
		return domain.Transport{Kind: domain.XHTTP, Path: orDefault(o.path, "/"), Host: o.host, Mode: mode, Headers: o.headers}, nil
	case "hysteria":
		return domain.Transport{Kind: domain.Hysteria, Salamander: domain.NewSecret(o.salamander)}, nil
	}
	return domain.Transport{}, reject(CodeUnsupportedTransport, "network")
}

// shadowsocksCipher maps provider spellings onto the core's method names.
// hysteriaObfs maps a Hysteria2 obfs type and password onto the Salamander
// password Xray's finalmask carries. Salamander is the only obfuscation it has.
func hysteriaObfs(kind, password string) (string, error) {
	switch strings.ToLower(kind) {
	case "", "none":
		return "", nil
	case "salamander":
		if password == "" {
			return "", reject(CodeMalformedEntry, "obfs-password")
		}
		return password, nil
	}
	return "", reject(CodeUnsupportedOption, "obfs")
}

func shadowsocksCipher(method string) (string, error) {
	method = strings.ToLower(strings.TrimSpace(method))
	switch method {
	case "chacha20-ietf-poly1305":
		method = "chacha20-poly1305"
	case "xchacha20-ietf-poly1305":
		method = "xchacha20-poly1305"
	case "plain":
		method = "none"
	}
	if !domain.ShadowsocksCipher(method) {
		return "", reject(CodeUnsupportedCipher, "method")
	}
	return method, nil
}

func newProtocol(p domain.Protocol, credential, username, cipher, flow string) (domain.ProtocolSpec, error) {
	spec, err := domain.NewProtocol(p, domain.NewSecret(credential), username, cipher, flow)
	if err != nil {
		return domain.ProtocolSpec{}, reject(CodeInvalidServer, "credentials")
	}
	return spec, nil
}

func vmessCipher(security string) string {
	if security == "" {
		return "auto"
	}
	return strings.ToLower(security)
}

func newVLESS(id, flow, encryption string) (domain.ProtocolSpec, error) {
	spec, err := newProtocol(domain.VLESS, id, "", "", flow)
	if err != nil {
		return domain.ProtocolSpec{}, err
	}
	if spec, err = spec.WithVLESSEncryption(encryption); err != nil {
		return domain.ProtocolSpec{}, reject(CodeUnsupportedOption, "encryption")
	}
	return spec, nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func nonEmpty(values []string) []string {
	var out []string
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func nilIfEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return values
}

func splitList(v string) []string { return nonEmpty(strings.Split(v, ",")) }
