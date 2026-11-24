package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// linkSample is one link as one RTM_NEWLINK message showed it. The kernel fills a message from the
// device in one call, so its up and its group are of one moment.
type linkSample struct{ up, ours bool }

// linkDump is one dump of every link, by name. A dump that isn't whole is an error. Tests read their
// sysNet directory instead.
var linkDump = dumpLinks

// sample is the final sample: every link from one dump, after the pass's last write.
func (k *kernel) sample() (map[string]linkSample, error) { return linkDump() }

// Netlink's numbers this parse reads (linux/netlink.h, linux/rtnetlink.h, linux/if_link.h).
const (
	nlmsgHeader  = 16
	nlmsgError   = 2
	nlmsgDone    = 3
	nlmFDumpIntr = 0x10
	rtmNewLink   = 16
	ifinfoHeader = 16
	iffUp        = 0x1
	iflaIfname   = 3
	iflaGroup    = 27
)

// parseLinkDump reads an RTM_GETLINK dump as NetlinkRIB returns it. Three dumps are no sample: one
// with a message marked NLM_F_DUMP_INTR, since the link list changed between its batches; one with an
// error message; and one that doesn't end on an NLMSG_DONE whose status is zero or more. NetlinkRIB
// stops at NLMSG_DONE whatever its status says, so the status is read here.
func parseLinkDump(dump []byte) (map[string]linkSample, error) {
	links := map[string]linkSample{}
	order := binary.NativeEndian
	for len(dump) > 0 {
		if len(dump) < nlmsgHeader {
			return nil, errors.New("link dump: a message shorter than its header")
		}
		size := int(order.Uint32(dump))
		kind, flags := order.Uint16(dump[4:]), order.Uint16(dump[6:])
		if size < nlmsgHeader || size > len(dump) {
			return nil, fmt.Errorf("link dump: a message of %d bytes in %d", size, len(dump))
		}
		body := dump[nlmsgHeader:size]
		if flags&nlmFDumpIntr != 0 {
			return nil, errors.New("link dump: interrupted, the links changed while it ran")
		}
		switch kind {
		case nlmsgDone:
			if len(body) < 4 || int32(order.Uint32(body)) < 0 {
				return nil, errors.New("link dump: ended on an error")
			}
			return links, nil
		case nlmsgError:
			return nil, errors.New("link dump: an error message")
		case rtmNewLink:
			name, l, err := parseLink(body)
			if err != nil {
				return nil, err
			}
			links[name] = l
		}
		dump = dump[min(align4(size), len(dump)):]
	}
	return nil, errors.New("link dump: no NLMSG_DONE")
}

// parseLink reads one RTM_NEWLINK body: its ifinfomsg, then its attributes.
func parseLink(body []byte) (string, linkSample, error) {
	order := binary.NativeEndian
	if len(body) < ifinfoHeader {
		return "", linkSample{}, errors.New("link dump: a link shorter than its ifinfomsg")
	}
	l := linkSample{up: order.Uint32(body[8:])&iffUp != 0}
	var name string
	for attrs := body[ifinfoHeader:]; len(attrs) >= 4; {
		size, kind := int(order.Uint16(attrs)), order.Uint16(attrs[2:])
		if size < 4 || size > len(attrs) {
			return "", linkSample{}, errors.New("link dump: a malformed attribute")
		}
		value := attrs[4:size]
		switch {
		case kind == iflaIfname:
			name = strings.TrimRight(string(value), "\x00")
		case kind == iflaGroup && len(value) == 4:
			l.ours = order.Uint32(value) == linkGroup
		}
		attrs = attrs[min(align4(size), len(attrs)):]
	}
	if name == "" {
		return "", linkSample{}, errors.New("link dump: a link with no name")
	}
	return name, l, nil
}

func align4(n int) int { return (n + 3) &^ 3 }
