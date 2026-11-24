package catalogue

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"sidegate/internal/domain"
)

// finalMask is what a stream's finalmask sets on its hop.
type finalMask struct {
	salamander string // Hysteria only, as quic is.
	quic       *domain.QuicParams
	tcp        []domain.TCPMask // Never with Hysteria.
}

// apply sets on t what the mask carries beside the Salamander password, which the transport takes.
func (m finalMask) apply(t *domain.Transport) { t.TCPMasks, t.QuicParams = m.tcp, m.quic }

// maxFinalMask bounds the document of one finalmask. A share link's is in no node budget, and a
// short list builds structs many times its length.
const maxFinalMask = 64 << 10

type xrayMask struct {
	Type     string
	Settings json.RawMessage
}

// parseFinalMask reads Xray's finalmask object. Each part changes what reaches the server, so one
// this can't carry whole is refused, never imported in part: every mask but the TCP fragment and
// header-custom ones and one Salamander with only a password, and every field not known here.
func parseFinalMask(raw []byte, hysteria bool) (finalMask, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return finalMask{}, true
	}
	var doc struct {
		TCP, UDP   []xrayMask
		QuicParams *xrayQuicParams
	}
	if !decodeStrict(raw, &doc) || len(doc.UDP) > 1 || len(doc.TCP) > domain.MaxTCPMasks {
		return finalMask{}, false
	}
	var m finalMask
	for _, udp := range doc.UDP {
		var s struct{ Password string }
		if !strings.EqualFold(udp.Type, "salamander") || !decodeStrict(udp.Settings, &s) || s.Password == "" {
			return finalMask{}, false
		}
		m.salamander = s.Password
	}
	for _, tcp := range doc.TCP {
		mask, ok := tcp.tcp()
		if !ok {
			return finalMask{}, false
		}
		m.tcp = append(m.tcp, mask)
	}
	if doc.QuicParams != nil {
		q := doc.QuicParams.params()
		if !q.Valid() {
			return finalMask{}, false
		}
		if q != (domain.QuicParams{}) {
			m.quic = &q
		}
	}
	if hysteria {
		return m, m.tcp == nil
	}
	return m, m.salamander == "" && m.quic == nil
}

// decodeStrict decodes the one JSON value raw holds into v, with no field v lacks.
func decodeStrict(raw []byte, v any) bool {
	if len(raw) > maxFinalMask {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

// xrayQuicParams is Xray's QuicParamsConfig. Debug is read and dropped: the core sets its own
// environment from it.
type xrayQuicParams struct {
	Debug                                                   json.RawMessage
	Congestion, BBRProfile, BrutalUp, BrutalDown            string
	BrutalDisableLossCompensation                           bool
	InitStreamReceiveWindow, MaxStreamReceiveWindow         uint64
	InitConnectionReceiveWindow, MaxConnectionReceiveWindow uint64
	MaxIdleTimeout, KeepAlivePeriod                         int64
	DisablePathMTUDiscovery, DisableChromeParrot            bool
	DisableGSO, DisableStatelessReset                       bool
	MaxIncomingStreams                                      int64
}

func (q xrayQuicParams) params() domain.QuicParams {
	return domain.QuicParams{
		Congestion: strings.ToLower(q.Congestion), BBRProfile: strings.ToLower(q.BBRProfile),
		BrutalUp: bandwidth(q.BrutalUp), BrutalDown: bandwidth(q.BrutalDown),
		BrutalDisableLossCompensation: q.BrutalDisableLossCompensation,
		InitStreamReceiveWindow:       q.InitStreamReceiveWindow,
		MaxStreamReceiveWindow:        q.MaxStreamReceiveWindow,
		InitConnectionReceiveWindow:   q.InitConnectionReceiveWindow,
		MaxConnectionReceiveWindow:    q.MaxConnectionReceiveWindow,
		MaxIdleTimeout:                q.MaxIdleTimeout,
		KeepAlivePeriod:               q.KeepAlivePeriod,
		DisablePathMTUDiscovery:       q.DisablePathMTUDiscovery,
		DisableChromeParrot:           q.DisableChromeParrot,
		DisableGSO:                    q.DisableGSO,
		DisableStatelessReset:         q.DisableStatelessReset,
		MaxIncomingStreams:            q.MaxIncomingStreams,
	}
}

// bandwidth is a rate the way Xray reads it: case and the space around it carry nothing, and zero
// is the same as none.
func bandwidth(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if rate, ok := domain.Bandwidth(s); ok && rate == 0 {
		return ""
	}
	return s
}

func (x xrayMask) tcp() (domain.TCPMask, bool) {
	m := domain.TCPMask{Type: strings.ToLower(x.Type)}
	switch m.Type {
	case domain.MaskFragment:
		var s struct {
			Packets         string
			Length, Delay   maskRange
			Lengths, Delays []maskRange
			MaxSplit        maskRange
		}
		if !decodeStrict(x.Settings, &s) {
			return domain.TCPMask{}, false
		}
		return fragmentMask(domain.FragmentMask{Packets: s.Packets, Length: s.Length.orNone(), Delay: s.Delay.orNone(),
			Lengths: rangeList(s.Lengths), Delays: rangeList(s.Delays), MaxSplit: s.MaxSplit.orNone()})
	case domain.MaskHeaderCustom:
		var s struct{ Clients, Servers, Errors [][]xrayCustomItem }
		if !decodeStrict(x.Settings, &s) {
			return domain.TCPMask{}, false
		}
		m.HeaderCustom = &domain.HeaderCustomTCP{}
		for _, list := range []struct {
			from [][]xrayCustomItem
			to   *[][]domain.CustomTCPItem
		}{{s.Clients, &m.HeaderCustom.Clients}, {s.Servers, &m.HeaderCustom.Servers}, {s.Errors, &m.HeaderCustom.Errors}} {
			for _, items := range list.from {
				sequence := make([]domain.CustomTCPItem, 0, len(items))
				for _, i := range items {
					item, ok := i.item()
					if !ok {
						return domain.TCPMask{}, false
					}
					sequence = append(sequence, item)
				}
				*list.to = append(*list.to, sequence)
			}
		}
	}
	return m, m.Valid()
}

func fragmentMask(f domain.FragmentMask) (domain.TCPMask, bool) {
	f.Packets = strings.ToLower(f.Packets)
	m := domain.TCPMask{Type: domain.MaskFragment, Fragment: &f}
	return m, m.Valid()
}

type xrayCustomItem struct {
	Delay                maskRange
	Rand                 int32
	RandRange            *maskRange
	Capture, Type, Reuse string
	Transform, Packet    json.RawMessage
}

// item reads one header-custom item. Xray counts a packet that is there as one even when it
// holds no bytes, and takes one kind for each item. A random range says nothing without Rand.
func (i xrayCustomItem) item() (domain.CustomTCPItem, bool) {
	if len(i.Transform) > 0 && string(i.Transform) != "null" || len(i.Packet) > 0 && (i.Rand > 0 || i.Reuse != "") {
		return domain.CustomTCPItem{}, false
	}
	packet, ok := customPacket(i.Packet, i.Type)
	if !ok {
		return domain.CustomTCPItem{}, false
	}
	item := domain.CustomTCPItem{Delay: i.Delay.orNone(), Rand: i.Rand, Capture: i.Capture, Reuse: i.Reuse, Packet: packet}
	if i.RandRange != nil && i.Rand > 0 {
		item.RandRange = i.RandRange.String()
	}
	return item, true
}

// customPacket decodes a packet as Xray's PraseByteSlice does. An array packet may be a JSON string
// too, which encoding/json reads as base64.
func customPacket(raw json.RawMessage, kind string) ([]byte, bool) {
	var packet []byte
	var text string
	var err error
	switch strings.ToLower(kind) {
	case "", "array":
		if len(raw) > 0 {
			err = json.Unmarshal(raw, &packet)
		}
		return packet, err == nil
	case "str", "hex", "base64":
		if json.Unmarshal(raw, &text) != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	switch strings.ToLower(kind) {
	case "hex":
		packet, err = hex.DecodeString(text)
	case "base64":
		packet, err = base64.StdEncoding.DecodeString(text)
	default:
		packet = []byte(text)
	}
	return packet, err == nil
}

// maskRange is Xray's Int32Range: a number, or a string of one number or of two in either order.
// Empty is zero. A negative one is refused, since nothing it bounds can be.
type maskRange struct{ low, high int64 }

func (r *maskRange) UnmarshalJSON(data []byte) error {
	var f flexString
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	if f == "" {
		*r = maskRange{}
		return nil
	}
	a, b, isRange := strings.Cut(string(f), "-")
	if !isRange {
		b = a
	}
	low, err1 := strconv.ParseUint(a, 10, 31)
	high, err2 := strconv.ParseUint(b, 10, 31)
	if err1 != nil || err2 != nil {
		return reject(CodeMalformedEntry, "range")
	}
	*r = maskRange{int64(min(low, high)), int64(max(low, high))}
	return nil
}

func (r maskRange) String() string {
	if r.low == r.high {
		return strconv.FormatInt(r.low, 10)
	}
	return strconv.FormatInt(r.low, 10) + "-" + strconv.FormatInt(r.high, 10)
}

// orNone is r for a field whose absence is zero.
func (r maskRange) orNone() string {
	if r == (maskRange{}) {
		return ""
	}
	return r.String()
}

func rangeList(list []maskRange) []string {
	var out []string
	for _, r := range list {
		out = append(out, r.String())
	}
	return out
}

// fragmentDialer reads a freedom outbound that only fragments the TCP connections dialled through it,
// which is what panels made before Xray had the fragment mask. One that does more, or dials through
// another outbound, is no mask. Its own domain strategy and sockopt say how the client resolves and
// opens a socket, which is no part of the server.
func fragmentDialer(o xrayOutbound) (domain.TCPMask, bool) {
	var s struct {
		Fragment *struct {
			Packets                    string
			Length, Interval, MaxSplit maskRange
		}
		DomainStrategy, TargetStrategy string
		UserLevel                      json.RawMessage
		Redirect                       string
		ProxyProtocol                  uint32
		Noises, FinalRules             []json.RawMessage
	}
	stream := o.StreamSettings
	mask, ok := parseFinalMask(stream.FinalMask, false)
	plain := strings.EqualFold(stream.Security, "none") || stream.Security == ""
	switch strings.ToLower(stream.Network) {
	case "", "tcp", "raw":
	default:
		plain = false
	}
	if !ok || mask.tcp != nil || !plain || o.dialVia() != "" {
		return domain.TCPMask{}, false
	}
	if !strings.EqualFold(o.Protocol, "freedom") || !decodeStrict(o.Settings, &s) || s.Fragment == nil ||
		s.Redirect != "" || s.ProxyProtocol != 0 || len(s.Noises) > 0 || len(s.FinalRules) > 0 {
		return domain.TCPMask{}, false
	}
	return fragmentMask(domain.FragmentMask{Packets: s.Fragment.Packets, Length: s.Fragment.Length.orNone(),
		Delay: s.Fragment.Interval.orNone(), MaxSplit: s.Fragment.MaxSplit.orNone()})
}
