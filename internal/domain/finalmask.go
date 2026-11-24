package domain

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A finalmask reshapes a hop's bytes below its transport and TLS. The rules here are the ones
// Xray v26.10.31 builds its config under (infra/conf/transport_finalmask.go), with bounds of our own
// on what a feed may make a server carry. A value they accept is one the core loads.

const (
	// MaxTCPMasks bounds one hop's TCP masks, in the order the core applies them: the last one
	// is closest to the wire.
	MaxTCPMasks = 4

	maxMaskList     = 16 // Entries of any list inside a mask.
	maxCustomPacket = 4096
	maxCustomRand   = 4096
)

// The TCP masks a server may carry.
const (
	MaskFragment     = "fragment"
	MaskHeaderCustom = "header-custom"
)

// TCPMask sets the one field its Type names.
type TCPMask struct {
	Type         string
	Fragment     *FragmentMask
	HeaderCustom *HeaderCustomTCP
}

// FragmentMask splits what the client writes into segments. A range is "n" or "a-b", in bytes or
// milliseconds. Lengths and Delays give each segment its own, and win over Length and Delay.
type FragmentMask struct {
	Packets         string // "" is every write, "tlshello" the TLS hello, and a range those writes by number.
	Length, Delay   string
	Lengths, Delays []string
	MaxSplit        string
}

// HeaderCustomTCP is the bytes each side exchanges before the stream, as sequences of items.
type HeaderCustomTCP struct {
	Clients, Servers, Errors [][]CustomTCPItem
}

// CustomTCPItem is one of random bytes, a fixed packet or a captured value sent again, after a
// delay. An item may be a delay alone.
type CustomTCPItem struct {
	Delay     string
	Rand      int32
	RandRange string // Of each random byte. Empty is 0-255.
	Capture   string
	Reuse     string
	Packet    []byte
}

// QuicParams tune a Hysteria hop's QUIC connection. Zero is the core's default. Xray's debug
// switch has no field: it sets environment variables for the whole core, and no feed may.
type QuicParams struct {
	Congestion                    string
	BBRProfile                    string
	BrutalUp, BrutalDown          string // In bits per second, as Xray's Bandwidth reads it.
	BrutalDisableLossCompensation bool
	InitStreamReceiveWindow       uint64
	MaxStreamReceiveWindow        uint64
	InitConnectionReceiveWindow   uint64
	MaxConnectionReceiveWindow    uint64
	MaxIdleTimeout                int64 // Seconds, as KeepAlivePeriod.
	KeepAlivePeriod               int64
	DisablePathMTUDiscovery       bool
	DisableChromeParrot           bool
	DisableGSO                    bool
	DisableStatelessReset         bool
	MaxIncomingStreams            int64
}

func (m TCPMask) Valid() bool {
	switch m.Type {
	case MaskFragment:
		return m.Fragment != nil && m.HeaderCustom == nil && m.Fragment.valid()
	case MaskHeaderCustom:
		return m.HeaderCustom != nil && m.Fragment == nil && m.HeaderCustom.valid()
	}
	return false
}

func (f FragmentMask) valid() bool {
	if f.Packets != "" && f.Packets != "tlshello" {
		if first, _, ok := maskRange(f.Packets); !ok || first == 0 {
			return false
		}
	}
	for _, r := range []string{f.Length, f.Delay, f.MaxSplit} {
		if !optionalRange(r) {
			return false
		}
	}
	if len(f.Lengths) > maxMaskList || len(f.Delays) > maxMaskList {
		return false
	}
	for _, r := range slices.Concat(f.Lengths, f.Delays) {
		if _, _, ok := maskRange(r); !ok {
			return false
		}
	}
	last := f.Length
	if len(f.Lengths) > 0 {
		last = f.Lengths[len(f.Lengths)-1]
	}
	shortest, _, ok := maskRange(last)
	return ok && shortest > 0 // The last length repeats to the end, and a zero one would never get there.
}

func (c HeaderCustomTCP) valid() bool {
	if len(c.Clients)+len(c.Servers)+len(c.Errors) == 0 {
		return false
	}
	for _, sequences := range [][][]CustomTCPItem{c.Clients, c.Servers, c.Errors} {
		if len(sequences) > maxMaskList {
			return false
		}
		for _, items := range sequences {
			if len(items) > maxMaskList || slices.ContainsFunc(items, func(i CustomTCPItem) bool { return !i.valid() }) {
				return false
			}
		}
	}
	return true
}

var customVariable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (i CustomTCPItem) valid() bool {
	for _, name := range []string{i.Capture, i.Reuse} {
		if name != "" && !customVariable.MatchString(name) {
			return false
		}
	}
	if !optionalRange(i.Delay) || i.Rand < 0 || i.Rand > maxCustomRand || len(i.Packet) > maxCustomPacket {
		return false
	}
	if i.RandRange != "" {
		if _, last, ok := maskRange(i.RandRange); !ok || last > 255 || i.Rand == 0 {
			return false
		}
	}
	kinds := 0
	for _, set := range []bool{i.Rand > 0, len(i.Packet) > 0, i.Reuse != ""} {
		if set {
			kinds++
		}
	}
	return kinds == 1 || kinds == 0 && i.Capture == "" && i.Delay != ""
}

func optionalRange(s string) bool {
	_, _, ok := maskRange(s)
	return s == "" || ok
}

// maskRange reads "n" or "a-b" with 0 <= a <= b, within what the core holds in an int32.
func maskRange(s string) (first, last int32, ok bool) {
	a, b, isRange := strings.Cut(s, "-")
	if !isRange {
		b = a
	}
	lo, err1 := strconv.ParseUint(a, 10, 31)
	hi, err2 := strconv.ParseUint(b, 10, 31)
	return int32(lo), int32(hi), err1 == nil && err2 == nil && lo <= hi
}

const (
	minBrutal        = 65536 // Bytes per second.
	minReceiveWindow = 16384
)

func (q QuicParams) Valid() bool {
	if !slices.Contains([]string{"", "brutal", "reno", "bbr", "force-brutal"}, q.Congestion) ||
		!slices.Contains([]string{"", "conservative", "standard", "aggressive"}, q.BBRProfile) {
		return false
	}
	for _, b := range []string{q.BrutalUp, q.BrutalDown} {
		if b == "" {
			continue
		}
		if rate, ok := Bandwidth(b); !ok || rate < minBrutal {
			return false
		}
	}
	if q.Congestion == "force-brutal" && q.BrutalUp == "" {
		return false
	}
	for _, w := range []uint64{q.InitStreamReceiveWindow, q.MaxStreamReceiveWindow, q.InitConnectionReceiveWindow, q.MaxConnectionReceiveWindow} {
		if w != 0 && w < minReceiveWindow {
			return false
		}
	}
	within := func(v, low, high int64) bool { return v == 0 || v >= low && v <= high }
	return within(q.MaxIdleTimeout, 4, 120) && within(q.KeepAlivePeriod, 2, 60) && (q.MaxIncomingStreams == 0 || q.MaxIncomingStreams >= 8)
}

var (
	bandwidth      = regexp.MustCompile(`^([0-9.]{1,24}) *([kmgt]?)(?:b|bps)?$`)
	bandwidthUnits = map[string]float64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40}
)

// Bandwidth reads a lowercase rate in bits, such as "100 mbps", as bytes per second. A thousand
// is 1024, as the core counts it (infra/conf/transport_method.go).
func Bandwidth(s string) (bytesPerSecond uint64, ok bool) {
	m := bandwidth.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	bits := n * bandwidthUnits[m[2]]
	if bits >= 1<<63 {
		return 0, false
	}
	return uint64(bits) / 8, true
}

func (m TCPMask) clone() TCPMask {
	if m.Fragment != nil {
		f := *m.Fragment
		f.Lengths, f.Delays = slices.Clone(f.Lengths), slices.Clone(f.Delays)
		m.Fragment = &f
	}
	if m.HeaderCustom != nil {
		c := *m.HeaderCustom
		for _, list := range []*[][]CustomTCPItem{&c.Clients, &c.Servers, &c.Errors} {
			*list = slices.Clone(*list)
			for s, items := range *list {
				items = slices.Clone(items)
				for i := range items {
					items[i].Packet = slices.Clone(items[i].Packet)
				}
				(*list)[s] = items
			}
		}
		m.HeaderCustom = &c
	}
	return m
}
