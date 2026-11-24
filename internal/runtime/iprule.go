package runtime

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// ipRule is one line of `ip -N rule show`, read whole: iproute2 prints selectors in
// a fixed order, so where a word sits says nothing unless every word before it was
// understood.
type ipRule struct {
	text       string // Normalized the way render writes it.
	not        bool
	from, to   string
	iif, oif   string
	marked     bool
	mark, mask uint32
	table      string
	packetOnly bool // A selector only the packet decides: tos, ipproto, ports, or one this reader does not know.
}

// ruleArgs lists the words iproute2 follows with one value.
var ruleArgs = []string{"from", "to", "tos", "fwmark", "iif", "oif", "uidrange", "ipproto", "sport", "dport", "tun_id",
	"lookup", "goto", "realms", "suppress_prefixlength", "suppress_ifgroup", "proto", "map-to", "nat"}

// parseRule reads one rule. "[detached]" says only that a device does not exist yet,
// and "[unresolved]" that a goto's target does not.
func parseRule(line string) (ipRule, int, error) {
	prefText, rest, ok := strings.Cut(line, ":")
	pref, err := strconv.Atoi(prefText)
	if !ok || err != nil {
		return ipRule{}, 0, fmt.Errorf("ip rule: unexpected line %q", line)
	}
	r := ipRule{from: "all", to: "all"}
	var words []string
	f := slices.DeleteFunc(strings.Fields(rest), func(w string) bool { return strings.HasPrefix(w, "[") })
	for i := 0; i < len(f); i++ {
		w := f[i]
		switch {
		case w == "not":
			r.not = true
		case slices.Contains(ruleArgs, w) && i+1 < len(f):
			i++
			r.set(w, f[i])
			words = append(words, w, f[i])
			continue
		case w == rtnUnreachable:
			w = "unreachable"
		case w == "l3mdev":
			r.packetOnly = true
		case strings.Trim(w, "0123456789") != "":
			// A rule type by name is an action, and any other word may select packets.
			r.packetOnly = r.packetOnly || !slices.Contains(ruleTypes, w)
		}
		words = append(words, w)
	}
	r.text = prefText + ": " + strings.Join(words, " ")
	return r, pref, nil
}

var ruleTypes = []string{"unicast", "local", "broadcast", "multicast", "blackhole", "unreachable", "prohibit", "throw", "nop"}

func (r *ipRule) set(key, value string) {
	switch key {
	case "from":
		r.from = value
	case "to":
		r.to = value
	case "iif":
		r.iif = value
	case "oif":
		r.oif = value
	case "lookup":
		r.table = value
	case "fwmark":
		mark, mask, hasMask := strings.Cut(value, "/")
		m, err := strconv.ParseUint(mark, 0, 32)
		k := uint64(0xffffffff)
		if err == nil && hasMask {
			k, err = strconv.ParseUint(mask, 0, 32)
		}
		if err != nil {
			r.packetOnly = true
			return
		}
		r.marked, r.mark, r.mask = true, uint32(m), uint32(k)
	case "tos", "uidrange", "ipproto", "sport", "dport", "tun_id":
		r.packetOnly = true
	}
}

// match is how a rule's selectors answer for one class of packets. Selectors AND, so
// the lowest answer is the rule's.
type match uint8

const (
	never match = iota
	sometimes
	always
)

// selects answers for packets arriving on link, whose WireGuard allowed-ips keep
// their source inside tunnel, carrying mark. A zero tunnel is one this pass does not
// know. Routing on input has no output device, so an oif never matches.
func (r ipRule) selects(link string, tunnel netip.Prefix, mark uint32) match {
	m := always
	if r.iif != "" && r.iif != link || r.oif != "" || r.marked && mark&r.mask != r.mark {
		return never
	}
	if r.packetOnly || r.to != "all" {
		m = sometimes
	}
	if r.from == "all" {
		return m
	}
	from, err := netip.ParsePrefix(r.from)
	if addr, addrErr := netip.ParseAddr(r.from); err != nil && addrErr == nil {
		from, err = addr.Prefix(addr.BitLen())
	}
	switch {
	case err != nil || !tunnel.IsValid():
		return min(m, sometimes)
	case !from.Overlaps(tunnel):
		return never
	case from.Bits() <= tunnel.Bits():
		return m
	}
	return sometimes
}

// canMatch reports whether the rule can catch pxa traffic before our own rules do.
// Packets from a pxa link carry no mark or, once TPROXY has marked them, exactly
// tproxyMark: the mark is set with a full mask, first in mangle PREROUTING. reach
// holds our links and the names we want. The local table only ever answers for the
// gateway's own addresses, so its rule catches nothing of ours.
func (r ipRule) canMatch(reach map[string]netip.Prefix) bool {
	if r.table == "255" {
		return false
	}
	for link, tunnel := range reach {
		for _, mark := range []uint32{0, tproxyMark} {
			m := r.selects(link, tunnel, mark)
			if r.not && m != always || !r.not && m != never {
				return true
			}
		}
	}
	return false
}
