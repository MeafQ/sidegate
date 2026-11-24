package domain

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"
)

type SlotKeys struct {
	HostPrivate, ClientPrivate SecretRef
	HostPublic, ClientPublic   string
}

// Allocation commits before any external write. Ordinal is daemon-owned and is
// unrelated to UniFi's volatile wgcltN index. It is not reused before deletion ack.
type Allocation struct {
	Ordinal        uint8
	Tunnel         netip.Prefix
	PeerListenPort uint16
	TPROXYPort     uint16
	Keys           SlotKeys
	NetworkID      NetworkID // Empty until UniFi's _id is discovered and committed.
	// Unrouted is true once a pass found no route to NetworkID, or found it gone. Until then it is false,
	// which counts as routed, so a binding no pass has read can't pass a delete as free.
	Unrouted bool
}

// Allocate reserves the lowest ordinal no allocation in taken holds, including
// those of slots still being deleted, and derives the tunnel /30, pxa name, and ports from
// it. The controller calls it inside the transaction that creates the slot.
func Allocate(taken []Allocation, keys SlotKeys) (Allocation, error) {
	for ordinal := uint8(1); ordinal <= MaxSlots; ordinal++ {
		if slices.ContainsFunc(taken, func(a Allocation) bool { return a.Ordinal == ordinal }) {
			continue
		}
		return Allocation{
			Ordinal:        ordinal,
			Tunnel:         netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 70, ordinal, 0}), 30),
			PeerListenPort: 51830 + uint16(ordinal),
			TPROXYPort:     12340 + uint16(ordinal),
			Keys:           keys,
		}, nil
	}
	return Allocation{}, ErrNoAllocation
}

// MaxSlots is UniFi's VPN Client cap, the most this product could ever bind.
const MaxSlots = 8

var ErrNoAllocation = errors.New("every slot allocation is held")

// Slot's Version moves only when the owner writes this slot.
type Slot struct {
	ID         SlotID
	Version    IntentRevision
	Intent     SlotIntent
	Allocation Allocation
}

// Exit is where a slot's traffic goes: one server, one group, every server a source keeps, the WAN,
// or nowhere. Exactly one is set, by constructor. The zero Exit is none of them, and no intent holds it.
type Exit struct {
	server LineageID
	group  GroupID
	source SourceID
	off    offVPN
}

type offVPN uint8

const (
	onVPN offVPN = iota
	toWAN
	dropped
)

func ServerExit(id LineageID) Exit { return Exit{server: id} }
func GroupExit(id GroupID) Exit    { return Exit{group: id} }
func SourceExit(id SourceID) Exit  { return Exit{source: id} }
func DirectExit() Exit             { return Exit{off: toWAN} }
func BlockExit() Exit              { return Exit{off: dropped} }

func (e Exit) Server() (LineageID, bool) { return e.server, e.server != "" }
func (e Exit) Group() (GroupID, bool)    { return e.group, e.group != "" }
func (e Exit) Source() (SourceID, bool)  { return e.source, e.source != "" }
func (e Exit) Direct() bool              { return e.off == toWAN }
func (e Exit) Block() bool               { return e.off == dropped }

// Kind names the Exit's arm as the page does.
func (e Exit) Kind() string {
	switch {
	case e.server != "":
		return "server"
	case e.group != "":
		return "group"
	case e.source != "":
		return "source"
	case e.off == toWAN:
		return "direct"
	}
	return "block"
}

// OffVPN is the selection an Exit that sends traffic to no server fixes: Direct for the WAN, or
// Blocked for nowhere.
func (e Exit) OffVPN() (Selection, bool) { return Selection{off: e.off}, e.off != onVPN }

func (e Exit) valid() bool {
	set := 0
	for _, on := range []bool{e.server != "", e.group != "", e.source != "", e.off != onVPN} {
		if on {
			set++
		}
	}
	return set == 1
}

// Owner holds a selection and the policy memory behind it: a group or a source, which every slot on it
// follows, or one slot on its own. Exactly one field is set, by constructor.
type Owner struct {
	group  GroupID
	source SourceID
	slot   SlotID
}

func GroupOwner(id GroupID) Owner   { return Owner{group: id} }
func SourceOwner(id SourceID) Owner { return Owner{source: id} }
func SlotOwner(id SlotID) Owner     { return Owner{slot: id} }

func (o Owner) Group() (GroupID, bool)   { return o.group, o.group != "" }
func (o Owner) Source() (SourceID, bool) { return o.source, o.source != "" }
func (o Owner) Slot() (SlotID, bool)     { return o.slot, o.slot != "" }

// String is the owner's key in state.db. IDs hold no slash, so the kind prefix can't be ambiguous.
func (o Owner) String() string {
	switch {
	case o.group != "":
		return "group/" + string(o.group)
	case o.source != "":
		return "source/" + string(o.source)
	}
	return "slot/" + string(o.slot)
}

func ParseOwner(key string) (Owner, error) {
	kind, id, _ := strings.Cut(key, "/")
	switch {
	case id == "":
	case kind == "group":
		return GroupOwner(GroupID(id)), nil
	case kind == "source":
		return SourceOwner(SourceID(id)), nil
	case kind == "slot":
		return SlotOwner(SlotID(id)), nil
	}
	return Owner{}, errors.New("an owner is group/<id>, source/<id> or slot/<id>")
}

// ErrNoName refuses a slot named only by blanks.
var ErrNoName = errors.New("a slot needs a name")

// SlotIntent is what the owner chose. Only NewIntent builds one, and the codec calls it.
type SlotIntent struct {
	name string
	exit Exit
}

func NewIntent(name string, exit Exit) (SlotIntent, error) {
	switch {
	case strings.TrimSpace(name) == "":
		return SlotIntent{}, ErrNoName
	case !exit.valid():
		return SlotIntent{}, errors.New("an Exit is one server, group or source, Direct or Block")
	}
	return SlotIntent{name: name, exit: exit}, nil
}

func (i SlotIntent) Name() string { return i.name }
func (i SlotIntent) Exit() Exit   { return i.exit }

type DownReason uint8

const (
	Unselected DownReason = iota // No decision yet; the zero Selection.
	Deleting
	NoUsableServer // Nothing the group allows is ready, or it allows nothing.
	ServerMissing  // A server Exit that isn't present.
	ServerRefused  // A server Exit the core can't run.
	ConfirmedDead
	PlumbingUnavailable
	NoExit // The slot just left Direct or Block and no server is ready yet, so nothing is wrong.
)

// Selection is one server, a down reason, the WAN or the drop. The zero value is Closed(Unselected).
// Policy chooses Direct or Blocked for a Direct or Block Exit, so every comparison of selections
// tells the two apart.
type Selection struct {
	server ServerRef
	down   DownReason
	off    offVPN
}

func Serving(ref ServerRef) Selection    { return Selection{server: ref} }
func Closed(reason DownReason) Selection { return Selection{down: reason} }
func Direct() Selection                  { return Selection{off: toWAN} }
func Blocked() Selection                 { return Selection{off: dropped} }

func (s Selection) Server() (ServerRef, bool) { return s.server, s.server != ServerRef{} }

// Down reports why the slot carries no traffic, or false when it is serving.
func (s Selection) Down() (DownReason, bool) {
	return s.down, s.server == ServerRef{} && s.off == onVPN
}

func (s Selection) IsDirect() bool  { return s.off == toWAN }
func (s Selection) IsBlocked() bool { return s.off == dropped }
func (s Selection) OffVPN() bool    { return s.off != onVPN }

type Reason uint8

const (
	InitialSelection Reason = iota + 1
	FailureMove
	SlownessMove
	ImprovementMove
	ExitChanged
	ServerLeftFeed
	RefusedByCore // The core refused the server. ServerRefused names the DownReason.
	RevisionChanged
	Recovery
	UDPMove       // The server's UDP broke and a rival's works. Its TCP may be fine, so this isn't a FailureMove.
	PreferredMove // The owner picked the server from its group or source by hand, and its check passed.
)

// Switch is one selection change of one owner, moving Slots together: every slot on a group at
// once, or a single slot that joined or left one. Intent and outcome share an ID, so retrying an
// apply never duplicates the event. Proposed is not displayed as applied before
// runtime readback. Evidence is copied in, so the event stays explainable after
// metrics.db retention drops the raw samples it cites. AllowUntil is how long the core
// may stay on the server it last reported, fixed when a move from a server is proposed.
type Switch struct {
	ID          EventID
	Owner       Owner
	Slots       []SlotID
	Intent      IntentRevision // The owner's revision: a slot's version, or a group's revision.
	From        Selection
	To          Selection
	Reason      Reason
	Evidence    []EvidenceSummary
	RequestedAt time.Time
	AppliedAt   *time.Time
	AllowUntil  time.Time
}
