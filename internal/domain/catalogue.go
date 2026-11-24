// Package domain contains values shared across ownership boundaries.
// Constructors copy slices and maps; accessors return detached values.
// Nothing here does I/O or knows a wire format.
package domain

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

type SourceID string
type LineageID string
type SlotID string
type RequestID string
type EventID string
type SecretRef string
type NetworkID string
type IntentRevision uint64
type SourceRevision uint64
type CatalogueRevision uint64
type Digest [32]byte

// CoreEpoch identifies one run of the proxy core. It is the core unit's systemd
// InvocationID, so it survives a daemon restart and changes on every core restart.
// Counters and probe results are only comparable within one epoch.
type CoreEpoch string

// NewLineageID derives the ID of a lineage first seen in the catalogue commit at,
// as the ordinal-th new entry of source. Catalogue revisions never repeat, so the
// result is unique without a reserved batch and lineage matching stays pure.
func NewLineageID(source SourceID, at CatalogueRevision, ordinal int) LineageID {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d/%d", source, at, ordinal))
	return LineageID(hex.EncodeToString(sum[:10]))
}

// ServerRef names one immutable server revision. IDs are parsed at HTTP, SQL, and
// process boundaries, never from display names.
type ServerRef struct {
	Lineage LineageID
	Digest  Digest // The full dependency closure with credentials and the Reality picks it carries.
}

type Endpoint struct {
	Host string // Canonical host or literal IP, never a URL.
	Port uint16
}

// Secret holds a credential. It has no exported field, so encoding/json writes {}
// and fmt prints the redaction. Only storage and wire encoders call Reveal.
type Secret struct{ value string }

func NewSecret(value string) Secret { return Secret{value: value} }
func (s Secret) Reveal() string     { return s.value }
func (s Secret) String() string     { return "[redacted]" }
func (s Secret) IsZero() bool       { return s.value == "" }

type Protocol uint8

const (
	VLESS Protocol = iota + 1
	VMess
	Trojan
	Shadowsocks
	SOCKS
	HTTPConnect
	Hysteria2 // Xray's hysteria outbound at version 2. The credential is the auth string.
)

// ProtocolSpec has no raw JSON extension field. The constructor rejects invalid
// credential, cipher, and flow combinations for the selected protocol.
type ProtocolSpec struct {
	protocol   Protocol
	credential Secret
	username   string
	cipher     string
	flow       string
	encryption Secret // VLESS only: the client's half of VLESS encryption. Empty is none.
}

func NewProtocol(p Protocol, credential Secret, username, cipher, flow string) (ProtocolSpec, error) {
	spec := ProtocolSpec{protocol: p, credential: credential, username: username, cipher: cipher, flow: flow}
	id := credential.Reveal()
	switch p {
	case VLESS:
		if !validUserID(id) || username != "" || cipher != "" || !slices.Contains(vlessFlows, flow) {
			return ProtocolSpec{}, ErrInvalidProtocol
		}
	case VMess:
		if !validUserID(id) || username != "" || !slices.Contains(vmessCiphers, cipher) || flow != "" {
			return ProtocolSpec{}, ErrInvalidProtocol
		}
	case Trojan:
		if id == "" || username != "" || cipher != "" || flow != "" {
			return ProtocolSpec{}, ErrInvalidProtocol
		}
	case Shadowsocks:
		if username != "" || !ShadowsocksCipher(cipher) || flow != "" {
			return ProtocolSpec{}, ErrInvalidProtocol
		}
		if id == "" && cipher != "none" {
			return ProtocolSpec{}, ErrInvalidProtocol
		}
	case SOCKS, HTTPConnect:
		if cipher != "" || flow != "" || (id != "" && username == "") {
			return ProtocolSpec{}, ErrInvalidProtocol
		}
	case Hysteria2:
		if id == "" || username != "" || cipher != "" || flow != "" {
			return ProtocolSpec{}, ErrInvalidProtocol
		}
	default:
		return ProtocolSpec{}, ErrInvalidProtocol
	}
	return spec, nil
}

var (
	ErrInvalidProtocol = errors.New("domain: invalid protocol settings")
	ErrInvalidServer   = errors.New("domain: invalid server closure")
)

// VisionFlow is the only VLESS flow the core accepts besides none.
const VisionFlow = "xtls-rprx-vision"

var (
	vlessFlows         = []string{"", VisionFlow, VisionFlow + "-udp443"}
	vmessCiphers       = []string{"auto", "aes-128-gcm", "chacha20-poly1305", "none", "zero"}
	shadowsocksCiphers = []string{
		"aes-128-gcm", "aes-256-gcm", "chacha20-poly1305", "xchacha20-poly1305", "none",
		"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305",
	}
)

// ShadowsocksCipher reports whether method is an AEAD or 2022 method Xray
// implements. Stream ciphers such as aes-256-cfb were removed from the core.
func ShadowsocksCipher(method string) bool { return slices.Contains(shadowsocksCiphers, method) }

// validUserID accepts a UUID or the 1-30 byte string the core maps to a UUIDv5.
func validUserID(id string) bool {
	if len(id) >= 1 && len(id) <= 30 {
		return true
	}
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case !strings.ContainsRune("0123456789abcdefABCDEF", r):
			return false
		}
	}
	return true
}

// WithVLESSEncryption is p with the client's encryption string, the one "xray vlessenc" prints.
// Empty and "none" are no encryption, which every protocol takes.
func (p ProtocolSpec) WithVLESSEncryption(encryption string) (ProtocolSpec, error) {
	if strings.EqualFold(encryption, "none") {
		encryption = ""
	}
	if encryption != "" && (p.protocol != VLESS || !validVLESSEncryption(encryption)) {
		return ProtocolSpec{}, ErrInvalidProtocol
	}
	p.encryption = NewSecret(encryption)
	return p, nil
}

func (p ProtocolSpec) VLESSEncryption() Secret { return p.encryption }

const (
	maxVLESSEncryption = 4096
	vlessKeyMin        = 20 // A shorter segment is padding (infra/conf/vless.go).
	vlessPaddingMin    = 18 + 17
	vlessPaddingMax    = 18 + 65535
)

// validVLESSEncryption takes what the core's config takes (infra/conf/vless.go and proxy/vless/encryption):
// the mode, the handshake, then padding before at least one key. With no key its config test panics
// instead of refusing, which is no verdict on any server tested beside it.
func validVLESSEncryption(s string) bool {
	if !printable(s, maxVLESSEncryption) {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 4 || parts[0] != "mlkem768x25519plus" ||
		!slices.Contains([]string{"native", "xorpub", "random"}, parts[1]) || !slices.Contains([]string{"0rtt", "1rtt"}, parts[2]) {
		return false
	}
	parts = parts[3:]
	first := slices.IndexFunc(parts, func(p string) bool { return len(p) >= vlessKeyMin })
	if first < 0 {
		return false
	}
	total := 0
	for i, p := range parts[:first] {
		fields := strings.Split(p, "-")
		if len(fields) < 3 {
			return false
		}
		var n [3]int
		for j := range n {
			var err error
			if n[j], err = strconv.Atoi(fields[j]); err != nil {
				return false
			}
		}
		if i == 0 && (n[0] < 100 || n[1] < vlessPaddingMin || n[2] < vlessPaddingMin) {
			return false
		}
		if i%2 == 0 {
			total += max(n[1], n[2]) // The segments alternate between a length and a gap.
		}
	}
	for _, p := range parts[first:] {
		key, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil || len(key) != 32 && len(key) != 1184 { // X25519 or ML-KEM-768.
			return false
		}
	}
	return total <= vlessPaddingMax
}

// printable says s is no longer than limit and all visible ASCII.
func printable(s string, limit int) bool {
	return len(s) <= limit && strings.IndexFunc(s, func(r rune) bool { return r <= ' ' || r > '~' }) < 0
}

func (p ProtocolSpec) Fields() (Protocol, Secret, string, string, string) {
	return p.protocol, p.credential, p.username, p.cipher, p.flow
}

type TransportKind uint8

const (
	TCP TransportKind = iota + 1
	WebSocket
	GRPC
	XHTTP
	Hysteria // QUIC; carries only the Hysteria2 protocol.
)

type Transport struct {
	Kind       TransportKind
	Path       string
	Host       string
	Service    string
	Mode       string // XHTTP mode, or "multi" for gRPC multi mode. Empty is the default.
	Headers    map[string]string
	Salamander Secret      // Hysteria only: the Salamander obfuscation password. Empty is none.
	TCPMasks   []TCPMask   // Every kind but Hysteria, which has no TCP.
	QuicParams *QuicParams // Hysteria only.
}

type Reality struct {
	PublicKey     string
	ShortID       string
	SpiderX       string
	MLDSA65Verify string // The key of the certificate's second, post-quantum signature. Empty checks none.
}

const mldsa65PublicKey = 1952

// ValidMLDSA65Verify reports whether s is a key the core takes: an ML-DSA-65 public key in unpadded
// base64url.
func ValidMLDSA65Verify(s string) bool {
	if !printable(s, base64.RawURLEncoding.EncodedLen(mldsa65PublicKey)) {
		return false // DecodeString skips line breaks.
	}
	key, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(key) == mldsa65PublicKey
}

const maxECHConfigList = 4096

// ValidECHConfigList reports whether s can be an ECH setting: a config list in base64, or the DNS server
// that gives one. The core fails the handshake on one it can't use, and never falls back to a plain name.
func ValidECHConfigList(s string) bool { return s != "" && printable(s, maxECHConfigList) }

// TLS never disables verification: Xray v26 removed allowInsecure. A provider's
// insecure flag is honored only as a certificate pin or a verified peer name.
type TLS struct {
	Enabled          bool
	ServerName       string
	ALPN             []string
	Fingerprint      string
	PinnedCertSHA256 []string // Lowercase hex SHA-256 of the peer certificate.
	VerifyNames      []string // Names the peer certificate must carry instead of ServerName.
	Reality          *Reality
	ECHConfigList    string // Never with Reality. Empty is no ECH.
}

type HopID string

type Hop struct {
	ID        HopID
	Endpoint  Endpoint
	Protocol  ProtocolSpec
	Transport Transport
	TLS       TLS
	DialVia   HopID // Empty means the daemon's WAN egress, not a provider route.
}

// Server is an immutable, validated outbound closure. Provider inbounds, routing,
// listeners, API settings, and resource paths never enter it. A bare server has no closure,
// only its Kind.
type Server struct {
	root HopID
	hops []Hop
	kind Kind
}

type Kind struct {
	Protocol  Protocol
	Transport TransportKind
}

func (s Server) Kind() Kind {
	if len(s.hops) == 0 {
		return s.kind
	}
	return Kind{Protocol: s.hops[0].Protocol.protocol, Transport: s.hops[0].Transport.Kind}
}

// Bare is s with nothing that reaches it: no endpoint, credential or option, only its Kind,
// which the page still shows. Only a tombstone's server is bare, since nothing dials it again.
func (s Server) Bare() Server { return Server{kind: s.Kind()} }

func BareServer(k Kind) Server { return Server{kind: k} }

// MaxHops bounds a closure: a root plus its dialer chain.
const MaxHops = 8

// NewServer accepts exactly the hops reachable from root through DialVia. The
// chain must end at the WAN egress, so every hop has at most one dependency and
// the closure is a path.
func NewServer(root HopID, hops []Hop) (Server, error) {
	if len(hops) == 0 || len(hops) > MaxHops {
		return Server{}, ErrInvalidServer
	}
	byID := make(map[HopID]Hop, len(hops))
	for _, h := range hops {
		if h.ID == "" || !validHop(h) {
			return Server{}, ErrInvalidServer
		}
		if _, dup := byID[h.ID]; dup {
			return Server{}, ErrInvalidServer
		}
		byID[h.ID] = h
	}
	chain := make([]Hop, 0, len(hops))
	for id := root; id != ""; {
		h, ok := byID[id]
		if !ok || len(chain) == len(hops) {
			return Server{}, ErrInvalidServer // Unresolved reference or a cycle.
		}
		chain = append(chain, cloneHop(h))
		id = h.DialVia
	}
	if len(chain) != len(hops) {
		return Server{}, ErrInvalidServer // Hops outside the closure.
	}
	return Server{root: root, hops: chain}, nil
}

// Closure returns the root and its hops in dial order: the root first, then the
// hop it dials through, ending at the hop that dials the WAN.
func (s Server) Closure() (HopID, []Hop) {
	hops := make([]Hop, len(s.hops))
	for i, h := range s.hops {
		hops[i] = cloneHop(h)
	}
	return s.root, hops
}

func validHop(h Hop) bool {
	if h.Endpoint.Host == "" || h.Endpoint.Port == 0 || h.Protocol.protocol == 0 {
		return false
	}
	if h.Transport.Kind < TCP || h.Transport.Kind > Hysteria {
		return false
	}
	if hysteria := h.Transport.Kind == Hysteria; hysteria != (h.Protocol.protocol == Hysteria2) ||
		(hysteria && (!h.TLS.Enabled || h.TLS.Reality != nil || len(h.Transport.TCPMasks) > 0)) ||
		(!hysteria && (!h.Transport.Salamander.IsZero() || h.Transport.QuicParams != nil)) {
		return false // Xray dials Hysteria only over TLS and QUIC, and Salamander exists only there.
	}
	if len(h.Transport.TCPMasks) > MaxTCPMasks || slices.ContainsFunc(h.Transport.TCPMasks, func(m TCPMask) bool { return !m.Valid() }) {
		return false
	}
	if q := h.Transport.QuicParams; q != nil && !q.Valid() {
		return false
	}
	if ech := h.TLS.ECHConfigList; ech != "" && (!h.TLS.Enabled || h.TLS.Reality != nil || !ValidECHConfigList(ech)) {
		return false
	}
	if r := h.TLS.Reality; r != nil {
		if r.MLDSA65Verify != "" && !ValidMLDSA65Verify(r.MLDSA65Verify) {
			return false
		}
		if !h.TLS.Enabled || r.PublicKey == "" || h.TLS.ServerName == "" || h.Transport.Kind == WebSocket {
			return false
		}
	}
	if (len(h.TLS.PinnedCertSHA256) > 0 || len(h.TLS.VerifyNames) > 0) && (!h.TLS.Enabled || h.TLS.Reality != nil) {
		return false
	}
	// Vision needs a layer to see through: TLS directly over TCP, or VLESS encryption over anything.
	if strings.HasPrefix(h.Protocol.flow, VisionFlow) && h.Protocol.encryption.IsZero() && (h.Transport.Kind != TCP || !h.TLS.Enabled) {
		return false
	}
	return true
}

func cloneHop(h Hop) Hop {
	h.Transport.Headers = maps.Clone(h.Transport.Headers)
	h.Transport.TCPMasks = slices.Clone(h.Transport.TCPMasks)
	for i, m := range h.Transport.TCPMasks {
		h.Transport.TCPMasks[i] = m.clone()
	}
	if h.Transport.QuicParams != nil {
		q := *h.Transport.QuicParams
		h.Transport.QuicParams = &q
	}
	h.TLS.ALPN = slices.Clone(h.TLS.ALPN)
	h.TLS.PinnedCertSHA256 = slices.Clone(h.TLS.PinnedCertSHA256)
	h.TLS.VerifyNames = slices.Clone(h.TLS.VerifyNames)
	if h.TLS.Reality != nil {
		r := *h.TLS.Reality
		h.TLS.Reality = &r
	}
	return h
}

// Identity holds the match keys of one entry, strongest first.
type Identity struct {
	ProviderID string
	Strong     Digest
	Endpoint   Digest
	HostKey    Digest // Protocol, address, port, transport, and TLS server name.
	RealityTie Digest // Never an independent matching tier.
	// The two weakest keys share no part of the connection, so a server that moved
	// and rotated can still match. CredentialKey digests the root hop's secret and
	// user. NameKey digests the server's folded Name, not Lineage.Label, so a number
	// never matches. Each is zero when the entry has none.
	CredentialKey Digest
	NameKey       Digest
}

// CountrySource says where a server's country came from.
type CountrySource uint8

const (
	CountryUnknown    CountrySource = iota
	CountryRule                     // An owner's rule that matches the server's name.
	CountryTagFlag                  // The one flag the server's tags carry.
	CountryConfigFlag               // The one flag its home config's remarks carry.
)

// Country is the first of an owner's rule, the tags' flag and the home config's flag that gives
// one. Two different flags at a step give nothing at that step.
type Country struct {
	Code   string
	Source CountrySource
}

// Name is what a feed calls a server: its home config's remarks, its tag there, or both. The zero
// Name is a server the feed names nowhere. NewName builds no Name with neither part.
type Name struct{ remarks, tag string }

func NewName(remarks, tag string) (Name, bool) {
	return Name{remarks: remarks, tag: tag}, remarks != "" || tag != ""
}

func (n Name) Remarks() string { return n.remarks }
func (n Name) Tag() string     { return n.tag }
func (n Name) IsZero() bool    { return n == Name{} }

// String joins the parts that are there with " · ".
func (n Name) String() string {
	if n.remarks != "" && n.tag != "" {
		return n.remarks + " · " + n.tag
	}
	return n.remarks + n.tag
}

// Naming is where a server's label came from. A Name with remarks has a Home, the config it is
// named from, and a home with no remarks that calls the server by no tag gives a Home and no Name.
// A Name without a Home is the server's most frequent tag alone. Number is added from 2 to tell two
// servers of one name apart.
type Naming struct {
	Name   Name
	Home   LineageID
	Number int
}

// Presence is where a lineage stands in its source: Present while the source lists it, Missing from the
// first refresh that doesn't and matched again by identity until GraceUntil, and a Tombstone from the
// source's refresh after that or from its deletion, never matched again. Listed, Unlisted, Buried and
// Orphaned are its only moves.
type Presence uint8

const (
	Present Presence = iota + 1
	Missing
	Tombstone
)

type Lineage struct {
	ID           LineageID
	Source       SourceID
	Label        string
	Naming       Naming
	Identity     Identity
	Ref          ServerRef
	Server       Server
	Country      Country
	Presence     Presence
	MissingSince time.Time
	GraceUntil   time.Time // Identity matching only; never authorizes a slot's traffic.
	// The Reality server names and short IDs the stored server carries are
	// picks, not identity. Reality records how long each has been absent from
	// refreshes, one entry per Reality hop in dial order. Nil means the latest
	// refresh carried every pick.
	Reality []RealityPicks
	Seen    []Occurrence // From the latest refresh that listed it. Not cleared when it goes missing.
}

// Listed is l on a refresh of its source that lists it.
func (l Lineage) Listed() Lineage {
	l.Presence, l.MissingSince, l.GraceUntil = Present, time.Time{}, time.Time{}
	return l
}

// Unlisted is l on a refresh of its source that doesn't list it: a present lineage goes missing from now,
// matchable again until grace is over. It says whether l moved.
func (l Lineage) Unlisted(now time.Time, grace time.Duration) (Lineage, bool) {
	if l.Presence != Present {
		return l, false
	}
	l.Presence, l.MissingSince, l.GraceUntil = Missing, now, now.Add(grace)
	return l, true
}

// Buried is l at a refresh of its source once its grace is over, a tombstone since it went missing. It
// says whether l moved.
func (l Lineage) Buried(now time.Time) (Lineage, bool) {
	if l.Presence != Missing || now.Before(l.GraceUntil) {
		return l, false
	}
	l.Presence = Tombstone
	return l, true
}

// Orphaned is l once its source is deleted, at the deletion: a tombstone that keeps what names it
// in events and analytics, with its server bare and no identity. The server held its credentials,
// and the identity digests them and could only match it again, which a tombstone never is. One
// already missing stays missing since it went.
func (l Lineage) Orphaned(at time.Time) Lineage {
	if l.Presence == Present {
		l.MissingSince = at
	}
	l.Presence = Tombstone
	l.Server, l.Identity, l.GraceUntil = l.Server.Bare(), Identity{}, time.Time{}
	return l
}

// Protocol is what the server's root hop speaks.
func (l Lineage) Protocol() Protocol { return l.Server.Kind().Protocol }

// names is what a name condition reads: the label and every tag the server was seen under.
func (l Lineage) names() []string {
	out := []string{l.Label}
	for _, o := range l.Seen {
		out = append(out, o.Tag)
	}
	return out
}

// RealityPicks records the absences of one Reality hop's picks.
type RealityPicks struct {
	ServerName, ShortID PickAbsence
}

// State is where the hop's picks stand together: rotated once either is, carried while both are.
func (p RealityPicks) State(now time.Time) PickState {
	return max(p.ServerName.State(now), p.ShortID.State(now))
}

// PickAbsence counts the refreshes since a Reality pick last appeared. The zero
// value means the latest refresh carried it.
type PickAbsence struct {
	Since  time.Time // The first refresh without it.
	Misses int       // Refreshes without it since, counted up to pickRefreshes.
}

// A stored Reality pick that no refresh has carried for pickWindow, over at least
// pickRefreshes refreshes, has been rotated out of the server's accepted list.
const (
	pickWindow    = 24 * time.Hour
	pickRefreshes = 3
)

// PickState is where a stored Reality pick stands. The values run from carried to rotated, so the
// later of two is the worse.
type PickState uint8

const (
	PickCarried PickState = iota // The latest refresh carried it.
	PickMissing                  // Refreshes since have not, too few or too briefly to call it rotated.
	PickRotated                  // Rotated out of the server's accepted list.
)

// State is where the pick stands at now.
func (a PickAbsence) State(now time.Time) PickState {
	switch {
	case a.Since.IsZero():
		return PickCarried
	case a.Misses >= pickRefreshes && now.Sub(a.Since) >= pickWindow:
		return PickRotated
	}
	return PickMissing
}

// Refreshed is the absence after a refresh at now that carried the pick or did not.
func (a PickAbsence) Refreshed(carried bool, now time.Time) PickAbsence {
	switch {
	case carried:
		return PickAbsence{}
	case a.Since.IsZero():
		return PickAbsence{Since: now, Misses: 1}
	case a.Misses < pickRefreshes:
		a.Misses++
	}
	return a
}

type Quota struct {
	Upload, Download, Limit *uint64 // Nil is unknown; zero does not mean unknown.
	Expires, Refills        *time.Time
	ObservedAt              time.Time
}

// ProviderMetadata is what a provider says about the source in its response.
// The catalogue owns the current value; the page and analytics only read it.
type ProviderMetadata struct {
	Title, Announcement, SupportURL, ProfileURL string
	SuggestedRefresh                            time.Duration
	Quota                                       Quota
}

// Catalogue indexes are derived once per successful refresh. Lookups never scan
// the whole catalogue in a probe tick. Source and country indexes narrow a rule.
type Catalogue struct {
	revision  CatalogueRevision
	lineages  map[LineageID]Lineage
	bySource  map[SourceID][]LineageID
	byCountry map[string][]LineageID
}

func NewCatalogue(rev CatalogueRevision, entries []Lineage) Catalogue {
	c := Catalogue{
		revision: rev, lineages: make(map[LineageID]Lineage, len(entries)),
		bySource: map[SourceID][]LineageID{}, byCountry: map[string][]LineageID{},
	}
	for _, l := range entries {
		c.lineages[l.ID] = l
	}
	for _, id := range slices.Sorted(maps.Keys(c.lineages)) {
		l := c.lineages[id]
		c.bySource[l.Source] = append(c.bySource[l.Source], id)
		if l.Country.Code != "" {
			c.byCountry[l.Country.Code] = append(c.byCountry[l.Country.Code], id)
		}
	}
	return c
}

// With is this catalogue with these lineages laid over it, at rev. The indexes are computed again.
func (c Catalogue) With(rev CatalogueRevision, changed []Lineage) Catalogue {
	merged := maps.Clone(c.lineages)
	if merged == nil {
		merged = map[LineageID]Lineage{}
	}
	for _, l := range changed {
		merged[l.ID] = l
	}
	return NewCatalogue(rev, slices.Collect(maps.Values(merged)))
}

func (c Catalogue) Without(gone []LineageID) Catalogue {
	kept := maps.Clone(c.lineages)
	for _, id := range gone {
		delete(kept, id)
	}
	return NewCatalogue(c.revision, slices.Collect(maps.Values(kept)))
}

func (c Catalogue) Revision() CatalogueRevision { return c.revision }
func (c Catalogue) Lookup(id LineageID) (Lineage, bool) {
	l, ok := c.lineages[id]
	return l, ok
}

// All lists every lineage in ID order, tombstones included.
func (c Catalogue) All() []Lineage {
	out := make([]Lineage, 0, len(c.lineages))
	for _, id := range slices.Sorted(maps.Keys(c.lineages)) {
		out = append(out, c.lineages[id])
	}
	return out
}

func (c Catalogue) Source(id SourceID) []Lineage {
	out := make([]Lineage, 0, len(c.bySource[id]))
	for _, l := range c.bySource[id] {
		out = append(out, c.lineages[l])
	}
	return out
}
