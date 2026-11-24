package daemon

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"sidegate/internal/domain"
)

// The wire types below are state.db's own encoding of domain values. Nothing else
// reads them: the page has its DTOs, and domain values have private fields.

func digestText(d domain.Digest) string { return hex.EncodeToString(d[:]) }

func parseDigest(s string) (domain.Digest, error) {
	var d domain.Digest
	if s == "" {
		return d, nil
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(d) {
		return d, fmt.Errorf("digest %q", s)
	}
	copy(d[:], b)
	return d, nil
}

type refWire struct {
	Lineage domain.LineageID `json:"l"`
	Digest  string           `json:"d"`
}

func refToWire(r domain.ServerRef) refWire { return refWire{r.Lineage, digestText(r.Digest)} }
func refFromWire(w refWire) (domain.ServerRef, error) {
	d, err := parseDigest(w.Digest)
	return domain.ServerRef{Lineage: w.Lineage, Digest: d}, err
}

type transportWire struct {
	Kind       domain.TransportKind `json:"kind"`
	Path       string               `json:"path,omitempty"`
	Host       string               `json:"host,omitempty"`
	Service    string               `json:"service,omitempty"`
	Mode       string               `json:"mode,omitempty"`
	Headers    map[string]string    `json:"headers,omitempty"`
	Salamander string               `json:"salamander,omitempty"`
	TCPMasks   []tcpMaskWire        `json:"tcpMasks,omitempty"`
	QuicParams *quicWire            `json:"quic,omitempty"`
}

type tcpMaskWire struct {
	Type         string            `json:"type,omitempty"`
	Fragment     *fragmentWire     `json:"fragment,omitempty"`
	HeaderCustom *headerCustomWire `json:"headerCustom,omitempty"`
}

type fragmentWire struct {
	Packets  string   `json:"packets,omitempty"`
	Length   string   `json:"length,omitempty"`
	Delay    string   `json:"delay,omitempty"`
	Lengths  []string `json:"lengths,omitempty"`
	Delays   []string `json:"delays,omitempty"`
	MaxSplit string   `json:"maxSplit,omitempty"`
}

type headerCustomWire struct {
	Clients [][]customItemWire `json:"clients,omitempty"`
	Servers [][]customItemWire `json:"servers,omitempty"`
	Errors  [][]customItemWire `json:"errors,omitempty"`
}

type customItemWire struct {
	Delay     string `json:"delay,omitempty"`
	Rand      int32  `json:"rand,omitempty"`
	RandRange string `json:"randRange,omitempty"`
	Capture   string `json:"capture,omitempty"`
	Reuse     string `json:"reuse,omitempty"`
	Packet    []byte `json:"packet,omitempty"`
}

type quicWire struct {
	Congestion                    string `json:"congestion,omitempty"`
	BBRProfile                    string `json:"bbrProfile,omitempty"`
	BrutalUp                      string `json:"brutalUp,omitempty"`
	BrutalDown                    string `json:"brutalDown,omitempty"`
	BrutalDisableLossCompensation bool   `json:"brutalDisableLossCompensation,omitempty"`
	InitStreamReceiveWindow       uint64 `json:"initStreamReceiveWindow,omitempty"`
	MaxStreamReceiveWindow        uint64 `json:"maxStreamReceiveWindow,omitempty"`
	InitConnectionReceiveWindow   uint64 `json:"initConnectionReceiveWindow,omitempty"`
	MaxConnectionReceiveWindow    uint64 `json:"maxConnectionReceiveWindow,omitempty"`
	MaxIdleTimeout                int64  `json:"maxIdleTimeout,omitempty"`
	KeepAlivePeriod               int64  `json:"keepAlivePeriod,omitempty"`
	DisablePathMTUDiscovery       bool   `json:"disablePathMTUDiscovery,omitempty"`
	DisableChromeParrot           bool   `json:"disableChromeParrot,omitempty"`
	DisableGSO                    bool   `json:"disableGSO,omitempty"`
	DisableStatelessReset         bool   `json:"disableStatelessReset,omitempty"`
	MaxIncomingStreams            int64  `json:"maxIncomingStreams,omitempty"`
}

func transportToWire(t domain.Transport) transportWire {
	w := transportWire{Kind: t.Kind, Path: t.Path, Host: t.Host, Service: t.Service, Mode: t.Mode, Headers: t.Headers,
		Salamander: t.Salamander.Reveal(), QuicParams: (*quicWire)(t.QuicParams)}
	for _, m := range t.TCPMasks {
		mask := tcpMaskWire{Type: m.Type, Fragment: (*fragmentWire)(m.Fragment)}
		if c := m.HeaderCustom; c != nil {
			item := func(i domain.CustomTCPItem) customItemWire { return customItemWire(i) }
			mask.HeaderCustom = &headerCustomWire{Clients: sequences(c.Clients, item), Servers: sequences(c.Servers, item), Errors: sequences(c.Errors, item)}
		}
		w.TCPMasks = append(w.TCPMasks, mask)
	}
	return w
}

func (w transportWire) transport() domain.Transport {
	t := domain.Transport{Kind: w.Kind, Path: w.Path, Host: w.Host, Service: w.Service, Mode: w.Mode, Headers: w.Headers,
		Salamander: domain.NewSecret(w.Salamander), QuicParams: (*domain.QuicParams)(w.QuicParams)}
	for _, m := range w.TCPMasks {
		mask := domain.TCPMask{Type: m.Type, Fragment: (*domain.FragmentMask)(m.Fragment)}
		if c := m.HeaderCustom; c != nil {
			item := func(i customItemWire) domain.CustomTCPItem { return domain.CustomTCPItem(i) }
			mask.HeaderCustom = &domain.HeaderCustomTCP{Clients: sequences(c.Clients, item), Servers: sequences(c.Servers, item), Errors: sequences(c.Errors, item)}
		}
		t.TCPMasks = append(t.TCPMasks, mask)
	}
	return t
}

// sequences keeps an empty sequence, which holds its place among the others.
func sequences[From, To any](from [][]From, item func(From) To) [][]To {
	var out [][]To
	for _, items := range from {
		sequence := make([]To, 0, len(items))
		for _, i := range items {
			sequence = append(sequence, item(i))
		}
		out = append(out, sequence)
	}
	return out
}

// tlsWire's and realityWire's older fields have always been written whole, under the domain's names.
type tlsWire struct {
	Enabled          bool
	ServerName       string
	ALPN             []string
	Fingerprint      string
	PinnedCertSHA256 []string
	VerifyNames      []string
	Reality          *realityWire
	ECHConfigList    string `json:"ECHConfigList,omitempty"`
}

type realityWire struct {
	PublicKey     string
	ShortID       string
	SpiderX       string
	MLDSA65Verify string `json:"MLDSA65Verify,omitempty"`
}

func tlsToWire(t domain.TLS) tlsWire {
	return tlsWire{Enabled: t.Enabled, ServerName: t.ServerName, ALPN: t.ALPN, Fingerprint: t.Fingerprint, PinnedCertSHA256: t.PinnedCertSHA256,
		VerifyNames: t.VerifyNames, Reality: (*realityWire)(t.Reality), ECHConfigList: t.ECHConfigList}
}

func (w tlsWire) tls() domain.TLS {
	return domain.TLS{Enabled: w.Enabled, ServerName: w.ServerName, ALPN: w.ALPN, Fingerprint: w.Fingerprint, PinnedCertSHA256: w.PinnedCertSHA256,
		VerifyNames: w.VerifyNames, Reality: (*domain.Reality)(w.Reality), ECHConfigList: w.ECHConfigList}
}

type hopWire struct {
	ID         domain.HopID    `json:"id"`
	Host       string          `json:"host"`
	Port       uint16          `json:"port"`
	Protocol   domain.Protocol `json:"protocol"`
	Credential string          `json:"credential,omitempty"`
	Username   string          `json:"username,omitempty"`
	Cipher     string          `json:"cipher,omitempty"`
	Flow       string          `json:"flow,omitempty"`
	Encryption string          `json:"encryption,omitempty"`
	Transport  transportWire   `json:"transport"`
	TLS        tlsWire         `json:"tls"`
	DialVia    domain.HopID    `json:"via,omitempty"`
}

type identityWire struct {
	ProviderID string `json:"provider,omitempty"`
	Strong     string `json:"strong,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"`
	HostKey    string `json:"host,omitempty"`
	RealityTie string `json:"tie,omitempty"`
	Credential string `json:"credential,omitempty"`
	Name       string `json:"name,omitempty"`
}

// namingWire is a stored domain.Naming. Remarks name a server only from its home.
type namingWire struct {
	Remarks string           `json:"remarks,omitempty"`
	Tag     string           `json:"tag,omitempty"`
	Home    domain.LineageID `json:"home,omitempty"`
	Number  int              `json:"number,omitempty"`
}

func namingToWire(n domain.Naming) namingWire {
	return namingWire{Remarks: n.Name.Remarks(), Tag: n.Name.Tag(), Home: n.Home, Number: n.Number}
}

func (w namingWire) naming() (domain.Naming, error) {
	name, _ := domain.NewName(w.Remarks, w.Tag)
	if w.Remarks != "" && w.Home == "" || w.Number == 1 || w.Number < 0 {
		return domain.Naming{}, errors.New("invalid naming")
	}
	return domain.Naming{Name: name, Home: w.Home, Number: w.Number}, nil
}

type lineageWire struct {
	Label        string                `json:"label"`
	Naming       namingWire            `json:"naming"`
	Identity     identityWire          `json:"identity"`
	Ref          refWire               `json:"ref"`
	Root         domain.HopID          `json:"root"`
	Hops         []hopWire             `json:"hops"`
	Country      domain.Country        `json:"country"`
	Presence     domain.Presence       `json:"presence"`
	MissingSince time.Time             `json:"missingSince"`
	GraceUntil   time.Time             `json:"graceUntil"`
	Reality      []domain.RealityPicks `json:"reality,omitempty"`
	Seen         []domain.Occurrence   `json:"seen,omitempty"`
	Bare         *domain.Kind          `json:"bare,omitempty"` // A bare server's, in place of its hops.
}

func encodeLineage(l domain.Lineage) ([]byte, error) {
	root, hops := l.Server.Closure()
	var bare *domain.Kind
	if len(hops) == 0 {
		kind := l.Server.Kind()
		bare = &kind
	}
	w := lineageWire{Bare: bare,
		Label: l.Label, Naming: namingToWire(l.Naming), Ref: refToWire(l.Ref), Root: root, Country: l.Country, Presence: l.Presence,
		MissingSince: l.MissingSince, GraceUntil: l.GraceUntil, Reality: l.Reality, Seen: l.Seen,
		Identity: identityWire{
			ProviderID: l.Identity.ProviderID, Strong: digestText(l.Identity.Strong), Endpoint: digestText(l.Identity.Endpoint),
			HostKey: digestText(l.Identity.HostKey), RealityTie: digestText(l.Identity.RealityTie),
			Credential: digestText(l.Identity.CredentialKey), Name: digestText(l.Identity.NameKey),
		},
	}
	for _, h := range hops {
		protocol, credential, username, cipher, flow := h.Protocol.Fields()
		w.Hops = append(w.Hops, hopWire{
			ID: h.ID, Host: h.Endpoint.Host, Port: h.Endpoint.Port, Protocol: protocol, Credential: credential.Reveal(),
			Username: username, Cipher: cipher, Flow: flow, Encryption: h.Protocol.VLESSEncryption().Reveal(),
			Transport: transportToWire(h.Transport), TLS: tlsToWire(h.TLS), DialVia: h.DialVia,
		})
	}
	return json.Marshal(w)
}

func decodeLineage(id domain.LineageID, source domain.SourceID, data []byte) (domain.Lineage, error) {
	var w lineageWire
	if err := json.Unmarshal(data, &w); err != nil {
		return domain.Lineage{}, err
	}
	ref, err := refFromWire(w.Ref)
	if err != nil {
		return domain.Lineage{}, err
	}
	identity := domain.Identity{ProviderID: w.Identity.ProviderID}
	for _, d := range []struct {
		text string
		out  *domain.Digest
	}{{w.Identity.Strong, &identity.Strong}, {w.Identity.Endpoint, &identity.Endpoint}, {w.Identity.HostKey, &identity.HostKey}, {w.Identity.RealityTie, &identity.RealityTie},
		{w.Identity.Credential, &identity.CredentialKey}, {w.Identity.Name, &identity.NameKey}} {
		if *d.out, err = parseDigest(d.text); err != nil {
			return domain.Lineage{}, err
		}
	}
	hops := make([]domain.Hop, 0, len(w.Hops))
	for _, h := range w.Hops {
		spec, err := domain.NewProtocol(h.Protocol, domain.NewSecret(h.Credential), h.Username, h.Cipher, h.Flow)
		if err == nil {
			spec, err = spec.WithVLESSEncryption(h.Encryption)
		}
		if err != nil {
			return domain.Lineage{}, fmt.Errorf("lineage %s hop %s: %w", id, h.ID, err)
		}
		hops = append(hops, domain.Hop{
			ID: h.ID, Endpoint: domain.Endpoint{Host: h.Host, Port: h.Port}, Protocol: spec, Transport: h.Transport.transport(), TLS: h.TLS.tls(), DialVia: h.DialVia,
		})
	}
	var server domain.Server
	if w.Bare != nil && len(hops) == 0 && w.Presence == domain.Tombstone {
		server = domain.BareServer(*w.Bare)
	} else if server, err = domain.NewServer(w.Root, hops); err != nil {
		return domain.Lineage{}, fmt.Errorf("lineage %s: %w", id, err)
	}
	naming, err := w.Naming.naming()
	if err != nil {
		return domain.Lineage{}, fmt.Errorf("lineage %s: %w", id, err)
	}
	return domain.Lineage{
		ID: id, Source: source, Label: w.Label, Naming: naming, Identity: identity, Ref: ref, Server: server, Country: w.Country,
		Presence: w.Presence, MissingSince: w.MissingSince, GraceUntil: w.GraceUntil, Reality: w.Reality, Seen: w.Seen,
	}, nil
}

type amongWire[T comparable] struct {
	Not    bool `json:"not,omitempty"`
	Values []T  `json:"values,omitempty"`
}

func toAmong[T comparable](a domain.Among[T]) amongWire[T] { return amongWire[T]{a.Not, a.Values} }
func (w amongWire[T]) among() domain.Among[T]              { return domain.Among[T]{Not: w.Not, Values: w.Values} }

// sourceWire holds exactly one of Remote or Manual, as Origin does.
type sourceWire struct {
	Revision domain.SourceRevision `json:"revision"`
	Name     string                `json:"name"`
	Remote   *domain.Remote        `json:"remote,omitempty"`
	Manual   *domain.Manual        `json:"manual,omitempty"`
	Feed     *domain.Feed          `json:"feed,omitempty"`
	Added    time.Time             `json:"added,omitzero"` // Absent from a source stored before it was kept.
}

func encodeSource(s domain.Source) ([]byte, error) {
	w := sourceWire{Revision: s.Revision, Name: s.Name, Feed: s.Feed, Added: s.Added}
	if r, ok := s.Origin.Remote(); ok {
		w.Remote = &r
	}
	if m, ok := s.Origin.Manual(); ok {
		w.Manual = &m
	}
	return json.Marshal(w)
}

func decodeSource(id domain.SourceID, data []byte) (domain.Source, error) {
	var w sourceWire
	if err := json.Unmarshal(data, &w); err != nil {
		return domain.Source{}, err
	}
	s := domain.Source{ID: id, Revision: w.Revision, Name: w.Name, Feed: w.Feed, Added: w.Added}
	switch {
	case w.Remote != nil && w.Manual == nil:
		s.Origin = domain.RemoteOrigin(*w.Remote)
	case w.Manual != nil && w.Remote == nil:
		s.Origin = domain.ManualOrigin(*w.Manual)
	default:
		return domain.Source{}, errors.New("a source is exactly one of remote or manual")
	}
	return s, nil
}

// exitWire holds exactly one of a server, a group, a source, Direct or Block, as Exit does.
type exitWire struct {
	Server domain.LineageID `json:"server,omitempty"`
	Group  domain.GroupID   `json:"group,omitempty"`
	Source domain.SourceID  `json:"source,omitempty"`
	Direct bool             `json:"direct,omitempty"`
	Block  bool             `json:"block,omitempty"`
}

type intentWire struct {
	Name string   `json:"name"`
	Exit exitWire `json:"exit"`
}

func encodeIntent(i domain.SlotIntent) ([]byte, error) {
	e := i.Exit()
	w := intentWire{Name: i.Name(), Exit: exitWire{Direct: e.Direct(), Block: e.Block()}}
	w.Exit.Server, _ = e.Server()
	w.Exit.Group, _ = e.Group()
	w.Exit.Source, _ = e.Source()
	return json.Marshal(w)
}

func decodeIntent(data []byte) (domain.SlotIntent, error) {
	var w intentWire
	if err := json.Unmarshal(data, &w); err != nil {
		return domain.SlotIntent{}, err
	}
	var exit domain.Exit
	switch e := w.Exit; {
	case e.Server != "":
		exit = domain.ServerExit(e.Server)
	case e.Group != "":
		exit = domain.GroupExit(e.Group)
	case e.Source != "":
		exit = domain.SourceExit(e.Source)
	case e.Direct:
		exit = domain.DirectExit()
	case e.Block:
		exit = domain.BlockExit()
	}
	return domain.NewIntent(w.Name, exit)
}

type membersWire struct {
	Sources   amongWire[domain.SourceID] `json:"sources"`
	Countries amongWire[string]          `json:"countries"`
	Protocols amongWire[domain.Protocol] `json:"protocols"`
	Include   string                     `json:"include,omitempty"`
	Exclude   string                     `json:"exclude,omitempty"`
	Box       string                     `json:"box,omitempty"`
}

// groupWire is a group's body. ID and revision are columns. A body an earlier build wrote may hold
// "added" and "removed" in its members: they are read as nothing and go at the group's next write.
type groupWire struct {
	Name    string             `json:"name,omitempty"`
	Members membersWire        `json:"members"`
	Ticked  []domain.LineageID `json:"ticked,omitempty"`
}

// boxNames is how the store and the API name a box condition. Unset has no name.
var boxNames = map[domain.Box]string{domain.BoxTicked: "ticked", domain.BoxUnticked: "unticked"}

func boxOf(name string) (domain.Box, error) {
	if name == "" {
		return domain.BoxUnset, nil
	}
	for b, n := range boxNames {
		if n == name {
			return b, nil
		}
	}
	return 0, fmt.Errorf("%q is not a box condition", name)
}

func encodeGroup(g domain.Group) ([]byte, error) {
	m := g.Members
	include, exclude := m.Names.Patterns()
	return json.Marshal(groupWire{Name: g.Name, Members: membersWire{Sources: toAmong(m.Sources), Countries: toAmong(m.Countries),
		Protocols: toAmong(m.Protocols), Include: include, Exclude: exclude, Box: boxNames[m.Box]}, Ticked: g.Ticked})
}

func decodeGroup(id domain.GroupID, revision domain.GroupRevision, data []byte) (domain.Group, error) {
	var w groupWire
	if err := json.Unmarshal(data, &w); err != nil {
		return domain.Group{}, err
	}
	names, err := domain.NewFilter(w.Members.Include, w.Members.Exclude)
	if err != nil {
		return domain.Group{}, err
	}
	box, err := boxOf(w.Members.Box)
	if err != nil {
		return domain.Group{}, err
	}
	members, err := domain.AdmitMembers(domain.Members{Sources: w.Members.Sources.among(), Countries: w.Members.Countries.among(),
		Protocols: w.Members.Protocols.among(), Names: names, Box: box})
	if err != nil {
		return domain.Group{}, err
	}
	ticked, err := domain.AdmitTicked(w.Ticked)
	return domain.Group{ID: id, Revision: revision, Name: w.Name, Members: members, Ticked: ticked}, err
}

type selectionWire struct {
	Server *refWire          `json:"server,omitempty"`
	Down   domain.DownReason `json:"down"`
	Off    string            `json:"off,omitempty"`
}

func selectionToWire(s domain.Selection) selectionWire {
	switch server, ok := s.Server(); {
	case ok:
		ref := refToWire(server)
		return selectionWire{Server: &ref}
	case s.IsDirect():
		return selectionWire{Off: "direct"}
	case s.IsBlocked():
		return selectionWire{Off: "block"}
	}
	reason, _ := s.Down()
	return selectionWire{Down: reason}
}

func selectionFromWire(w selectionWire) (domain.Selection, error) {
	switch {
	case w.Off == "direct":
		return domain.Direct(), nil
	case w.Off == "block":
		return domain.Blocked(), nil
	case w.Off != "":
		return domain.Selection{}, fmt.Errorf("selection off %q is neither direct nor block", w.Off)
	case w.Server == nil:
		return domain.Closed(w.Down), nil
	}
	ref, err := refFromWire(*w.Server)
	if err != nil {
		return domain.Selection{}, err
	}
	return domain.Serving(ref), nil
}

type memoryWire struct {
	Selection  selectionWire                  `json:"selection"`
	LastServed refWire                        `json:"lastServed"`
	LeaveBy    time.Time                      `json:"leaveBy"`
	KeepOut    map[domain.LineageID]time.Time `json:"keepOut,omitempty"`
	Unshown    *moveWire                      `json:"unshown,omitempty"`
	Closures   int                            `json:"closures,omitempty"`
	Serving    time.Time                      `json:"servingSince,omitzero"`
	ReopenAt   time.Time                      `json:"reopenNotBefore,omitzero"`
	Proven     time.Time                      `json:"provenFailing,omitzero"`
	Refilled   time.Time                      `json:"refilledAt,omitzero"`
	Preferred  *preferenceWire                `json:"preferred,omitempty"`
	Pick       *refWire                       `json:"pick,omitempty"`
}

type preferenceWire struct {
	Server domain.LineageID `json:"server"`
	At     time.Time        `json:"at"`
	Size   float64          `json:"size"`
}

type moveWire struct {
	From   selectionWire `json:"from"`
	Reason domain.Reason `json:"reason"`
}

func encodeMemory(m policyMemory) ([]byte, error) {
	w := memoryWire{Selection: selectionToWire(m.Selection), LastServed: refToWire(m.LastServed), LeaveBy: m.LeaveBy, KeepOut: m.KeepOut,
		Closures: m.Backoff.Closures, Serving: m.Backoff.ServingSince, ReopenAt: m.Backoff.NotBefore, Proven: m.Backoff.ProvenFailing, Refilled: m.RefilledAt}
	if u := m.Unshown; u != nil {
		w.Unshown = &moveWire{selectionToWire(u.From), u.Reason}
	}
	w.Preferred, w.Pick = handToWire(m.Hand)
	return json.Marshal(w)
}

func decodeMemory(data []byte) (policyMemory, error) {
	var w memoryWire
	if err := json.Unmarshal(data, &w); err != nil {
		return policyMemory{}, err
	}
	selection, err := selectionFromWire(w.Selection)
	if err != nil {
		return policyMemory{}, err
	}
	last, err := refFromWire(w.LastServed)
	if err != nil {
		return policyMemory{}, err
	}
	m := policyMemory{Selection: selection, LastServed: last, LeaveBy: w.LeaveBy, KeepOut: w.KeepOut, RefilledAt: w.Refilled,
		Backoff: reopenBackoff{Closures: w.Closures, ServingSince: w.Serving, NotBefore: w.ReopenAt, ProvenFailing: w.Proven}}
	if u := w.Unshown; u != nil {
		from, err := selectionFromWire(u.From)
		if err != nil {
			return policyMemory{}, err
		}
		m.Unshown = &ownerMove{From: from, Reason: u.Reason}
	}
	if m.Hand, err = handFromWire(w.Preferred, w.Pick); err != nil {
		return policyMemory{}, err
	}
	return m, nil
}

// handToWire stores a queued pick as its move, with its boost beside it when it has one, and a held one as
// its boost alone.
func handToWire(h handPick) (*preferenceWire, *refWire) {
	switch at := h.at.(type) {
	case queuedPick:
		var boost *preferenceWire
		if at.boost.Size > 0 {
			boost = &preferenceWire{at.move.Lineage, at.boost.At, at.boost.Size}
		}
		return boost, new(refToWire(at.move))
	case heldPick:
		return &preferenceWire{at.server, at.boost.At, at.boost.Size}, nil
	}
	return nil, nil
}

// handFromWire reads back what handToWire stores. A move with a boost on another lineage is a shape no
// release wrote: it loads as the move with no boost, since a row refused stops the daemon at boot.
func handFromWire(boosted *preferenceWire, move *refWire) (handPick, error) {
	if move == nil {
		if boosted == nil {
			return handPick{}, nil
		}
		return handPick{at: heldPick{server: boosted.Server, boost: boost{At: boosted.At, Size: boosted.Size}}}, nil
	}
	ref, err := refFromWire(*move)
	if err != nil {
		return handPick{}, err
	}
	q := queuedPick{move: ref}
	if boosted != nil && boosted.Server == ref.Lineage {
		q.boost = boost{At: boosted.At, Size: boosted.Size}
	}
	return handPick{at: q}, nil
}

type carriesWire struct {
	Selection  selectionWire  `json:"selection"`
	LastServed refWire        `json:"lastServed"`
	Owes       domain.EventID `json:"owes,omitempty"`
	Closed     domain.EventID `json:"closed,omitempty"`
}

func encodeCarries(row slotRow) ([]byte, error) {
	return json.Marshal(carriesWire{selectionToWire(row.Selection), refToWire(row.LastServed), row.Owes, row.Closed})
}

func decodeCarries(data []byte, row *slotRow) error {
	var w carriesWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	row.Owes, row.Closed = w.Owes, w.Closed
	var err error
	if row.Selection, err = selectionFromWire(w.Selection); err != nil {
		return err
	}
	row.LastServed, err = refFromWire(w.LastServed)
	return err
}

type allocationWire struct {
	Ordinal        uint8            `json:"ordinal"`
	Tunnel         string           `json:"tunnel"`
	PeerListenPort uint16           `json:"peerPort"`
	TPROXYPort     uint16           `json:"tproxyPort"`
	Keys           domain.SlotKeys  `json:"keys"`
	NetworkID      domain.NetworkID `json:"networkId,omitempty"`
	Unrouted       bool             `json:"unrouted,omitempty"`
}

func encodeAllocation(a domain.Allocation) ([]byte, error) {
	return json.Marshal(allocationWire{a.Ordinal, a.Tunnel.String(), a.PeerListenPort, a.TPROXYPort, a.Keys, a.NetworkID, a.Unrouted})
}

func decodeAllocation(data []byte) (domain.Allocation, error) {
	var w allocationWire
	if err := json.Unmarshal(data, &w); err != nil {
		return domain.Allocation{}, err
	}
	tunnel, err := netip.ParsePrefix(w.Tunnel)
	if err != nil {
		return domain.Allocation{}, err
	}
	return domain.Allocation{Ordinal: w.Ordinal, Tunnel: tunnel, PeerListenPort: w.PeerListenPort, TPROXYPort: w.TPROXYPort,
		Keys: w.Keys, NetworkID: w.NetworkID, Unrouted: w.Unrouted}, nil
}

type evidenceWire struct {
	Server  refWire           `json:"server"`
	Kind    domain.SignalKind `json:"kind"`
	Verdict domain.Verdict    `json:"verdict"`
	Target  domain.TargetID   `json:"target,omitempty"`
	Round   uint8             `json:"round,omitempty"`
	At      time.Time         `json:"at"`
	NewConn *time.Duration    `json:"newConn,omitempty"`
	Ping    *time.Duration    `json:"ping,omitempty"`
}

type switchWire struct {
	Slots      []domain.SlotID `json:"slots"`
	From       selectionWire   `json:"from"`
	To         selectionWire   `json:"to"`
	Evidence   []evidenceWire  `json:"evidence,omitempty"`
	AllowUntil time.Time       `json:"allowUntil"`
}

func encodeSwitch(s domain.Switch) ([]byte, error) {
	w := switchWire{Slots: s.Slots, From: selectionToWire(s.From), To: selectionToWire(s.To), AllowUntil: s.AllowUntil}
	for _, e := range s.Evidence {
		w.Evidence = append(w.Evidence, evidenceWire{refToWire(e.Server), e.Kind, e.Verdict, e.Target, e.Round, e.At, e.NewConn, e.Ping})
	}
	return json.Marshal(w)
}

func decodeSwitch(data []byte, s *domain.Switch) error {
	var w switchWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	s.Slots, s.AllowUntil = w.Slots, w.AllowUntil
	var err error
	if s.From, err = selectionFromWire(w.From); err != nil {
		return err
	}
	if s.To, err = selectionFromWire(w.To); err != nil {
		return err
	}
	for _, e := range w.Evidence {
		ref, err := refFromWire(e.Server)
		if err != nil {
			return err
		}
		s.Evidence = append(s.Evidence, domain.EvidenceSummary{Server: ref, Kind: e.Kind, Verdict: e.Verdict, Target: e.Target, Round: e.Round, At: e.At, NewConn: e.NewConn, Ping: e.Ping})
	}
	return nil
}

// recordWire keeps stamps as Unix milliseconds and durations as microseconds, and groups samples by
// site, since the hourly flush rewrites every record that changed.
type recordWire struct {
	FirstSeen   int64               `json:"first,omitempty"`
	Samples     map[site][][3]int64 `json:"samples,omitempty"`
	Incidents   []int64             `json:"incidents,omitempty"`
	Outages     [][2]int64          `json:"outages,omitempty"`
	Past        float64             `json:"past,omitempty"`
	PastAt      int64               `json:"pastAt,omitempty"`
	Last        int64               `json:"last,omitempty"`
	Failures    int                 `json:"failures,omitempty"`
	Retries     int                 `json:"retries,omitempty"`
	SteppedAt   int64               `json:"steppedAt,omitempty"`
	Blamed      int                 `json:"blamed,omitempty"`
	BlamedSince int64               `json:"blamedSince,omitempty"`
	Streak      int                 `json:"streak,omitempty"`
	StreakAt    int64               `json:"streakAt,omitempty"`
	Seen        float64             `json:"seen,omitempty"`
	SeenAt      int64               `json:"seenAt,omitempty"`
	Revision    string              `json:"rev,omitempty"`
	Checked     int64               `json:"checked,omitempty"`
	Worked      [][2]int64          `json:"worked,omitempty"`
	WorkedFrom  int64               `json:"workedFrom,omitempty"`
}

func millisOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func timeOf(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func encodeRecord(r record) ([]byte, error) {
	w := recordWire{FirstSeen: millisOf(r.FirstSeen), Past: r.Past.Value, PastAt: millisOf(r.Past.At), Last: millisOf(r.Last), Failures: r.Failures, Retries: r.Retries,
		SteppedAt: millisOf(r.SteppedAt), Blamed: r.Blamed, BlamedSince: millisOf(r.BlamedSince), Streak: r.Streak, StreakAt: millisOf(r.StreakAt), Seen: r.Seen.Value, SeenAt: millisOf(r.Seen.At),
		Checked: millisOf(r.Checked), WorkedFrom: millisOf(r.WorkedFrom)}
	if r.Revision != (domain.Digest{}) {
		w.Revision = digestText(r.Revision)
	}
	for _, s := range r.Samples {
		if w.Samples == nil {
			w.Samples = map[site][][3]int64{}
		}
		w.Samples[s.Site] = append(w.Samples[s.Site], [3]int64{s.At.UnixMilli(), s.Ping.Microseconds(), s.NewConn.Microseconds()})
	}
	for _, at := range r.Incidents {
		w.Incidents = append(w.Incidents, millisOf(at))
	}
	for _, e := range r.Outages {
		w.Outages = append(w.Outages, [2]int64{millisOf(e.Start), millisOf(e.End)})
	}
	for _, i := range r.Worked {
		w.Worked = append(w.Worked, [2]int64{millisOf(i.from), millisOf(i.to)})
	}
	return json.Marshal(w)
}

type rankingWire struct {
	Site     site  `json:"site,omitempty"`
	Previous site  `json:"previous,omitempty"`
	Since    int64 `json:"since,omitempty"`
}

func encodeRanking(r rankedSite) ([]byte, error) {
	return json.Marshal(rankingWire{Site: r.Site, Previous: r.Previous, Since: millisOf(r.Since)})
}

func decodeRanking(data []byte) (rankedSite, error) {
	var w rankingWire
	err := json.Unmarshal(data, &w)
	return rankedSite{Site: w.Site, Previous: w.Previous, Since: timeOf(w.Since)}, err
}

func decodeRecord(data []byte) (record, error) {
	var w recordWire
	if err := json.Unmarshal(data, &w); err != nil {
		return record{}, err
	}
	r := record{FirstSeen: timeOf(w.FirstSeen), Past: decayed{w.Past, timeOf(w.PastAt)}, Last: timeOf(w.Last), Failures: w.Failures, Retries: w.Retries,
		SteppedAt: timeOf(w.SteppedAt), Blamed: w.Blamed, BlamedSince: timeOf(w.BlamedSince), Streak: w.Streak, StreakAt: timeOf(w.StreakAt), Seen: decayed{w.Seen, timeOf(w.SeenAt)},
		Checked: timeOf(w.Checked), WorkedFrom: timeOf(w.WorkedFrom)}
	var err error
	if r.Revision, err = parseDigest(w.Revision); err != nil {
		return record{}, err
	}
	for at, samples := range w.Samples {
		for _, s := range samples {
			r.Samples = append(r.Samples, sample{At: timeOf(s[0]), Site: at, Ping: time.Duration(s[1]) * time.Microsecond, NewConn: time.Duration(s[2]) * time.Microsecond})
		}
	}
	slices.SortStableFunc(r.Samples, func(a, b sample) int { return a.At.Compare(b.At) })
	for _, at := range w.Incidents {
		r.Incidents = append(r.Incidents, timeOf(at))
	}
	for _, e := range w.Outages {
		r.Outages = append(r.Outages, episode{timeOf(e[0]), timeOf(e[1])})
	}
	for _, i := range w.Worked {
		r.Worked = append(r.Worked, interval{timeOf(i[0]), timeOf(i[1])})
	}
	return r, nil
}
