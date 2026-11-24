package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	"sidegate/internal/metrics"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

// These types are the page's JSON, field for field: web/src/lib/schema.ts is the
// contract. No domain struct is marshaled to the page, so nothing here carries a
// credential, a raw provider value or a private type. The handlers in web.go will
// encode them as they are.

// SelectionDTO is schema.ts's Selection.
type SelectionDTO struct {
	Kind   string            `json:"kind"`
	Server *domain.LineageID `json:"server,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

// lineageOf is the server a selection serves, or nil when it is down.
func lineageOf(s domain.Selection) *domain.LineageID {
	server, ok := s.Server()
	if !ok {
		return nil
	}
	return &server.Lineage
}

var downNames = map[domain.DownReason]string{
	domain.Unselected: "unselected", domain.Deleting: "deleting", domain.NoUsableServer: "noUsableServer",
	domain.ServerMissing: "serverMissing", domain.ServerRefused: "serverRefused", domain.ConfirmedDead: "confirmedDead",
	domain.PlumbingUnavailable: "plumbingUnavailable", domain.NoExit: "noExit",
}

func selectionDTO(s domain.Selection) SelectionDTO {
	switch {
	case s.IsDirect():
		return SelectionDTO{Kind: "direct"}
	case s.IsBlocked():
		return SelectionDTO{Kind: "block"}
	}
	if reason, down := s.Down(); down {
		return SelectionDTO{Kind: "down", Reason: downNames[reason]}
	}
	return SelectionDTO{Kind: "up", Server: lineageOf(s)}
}

var verdictNames = map[domain.Verdict]string{domain.Unknown: "unknown", domain.Succeeded: "succeeded", domain.Failed: "failed"}

var phaseNames = map[gateway.Phase]string{gateway.Closed: "closed", gateway.Ready: "ready", gateway.Unknown: "unknown"}

// ExitDTO is schema.ts's Exit, one arm by Kind.
type ExitDTO struct {
	Kind   string           `json:"kind"`
	Server domain.LineageID `json:"server,omitempty"`
	Group  domain.GroupID   `json:"group,omitempty"`
	Source domain.SourceID  `json:"source,omitempty"`
}

func exitDTO(e domain.Exit) ExitDTO {
	dto := ExitDTO{Kind: e.Kind()}
	dto.Server, _ = e.Server()
	dto.Group, _ = e.Group()
	dto.Source, _ = e.Source()
	return dto
}

// admit takes the arm Kind names, and refuses one carrying another arm's field.
func (dto ExitDTO) admit() (domain.Exit, error) {
	exit := map[string]domain.Exit{"server": domain.ServerExit(dto.Server), "group": domain.GroupExit(dto.Group), "source": domain.SourceExit(dto.Source),
		"direct": domain.DirectExit(), "block": domain.BlockExit()}[dto.Kind]
	if exitDTO(exit) != dto {
		return domain.Exit{}, errors.New("an Exit is one server, group or source, Direct or Block")
	}
	return exit, nil
}

// SlotInputDTO is schema.ts's SlotInput.
type SlotInputDTO struct {
	Name string  `json:"name"`
	Exit ExitDTO `json:"exit"`
}

func (dto SlotInputDTO) admit() (domain.SlotIntent, error) {
	exit, err := dto.Exit.admit()
	if err != nil {
		return domain.SlotIntent{}, err
	}
	return domain.NewIntent(dto.Name, exit)
}

// slotState is the one place a slot's status word and down reason are computed. A slot on a server,
// group or source that nothing serves yet, because none of its servers is ready, is checking while
// one of them hasn't answered a check or one is ready and the election hasn't taken it. It fails with
// no usable server once every one has answered and none is ready, and an empty pool says why it is
// empty. The write that takes a slot off Direct or Block answers before policy closes it, so that
// selection reads as the closure it is about to be. A slot made over a deletion carries the deletion's
// closure until policy decides for it, so that closure is no decision either.
func (e *evaluation) slotState(intent domain.SlotIntent, s domain.Selection) (string, *string) {
	reason, down := s.Down()
	switch exit := intent.Exit(); {
	case exit.Direct():
		return "direct", nil
	case exit.Block():
		return "block", nil
	case s.OffVPN():
		reason = domain.NoExit
	case !down:
		return "serving", nil
	}
	if reason == domain.Unselected || reason == domain.Deleting || reason == domain.NoExit || reason == domain.NoUsableServer {
		switch {
		case len(e.pool.members) == 0:
			reason = e.pool.downReason()
		case e.checking():
			return "checking", nil
		default:
			reason = domain.NoUsableServer
		}
	}
	name := downNames[reason]
	return "failed", &name
}

// checking says the pool has a server no check has answered yet, or a ready one.
func (e *evaluation) checking() bool {
	return slices.ContainsFunc(e.pool.members, func(l domain.Lineage) bool {
		return e.ev.book.records[l.ID].standing() == unprobed || e.ready(l)
	})
}

// SlotReportDTO is schema.ts's SlotReport.
type SlotReportDTO struct {
	Phase              string       `json:"phase"`
	Applied            SelectionDTO `json:"applied"`
	NetworkID          *string      `json:"networkId"`
	NetworkEstablished bool         `json:"networkEstablished"`
	ErrorCode          *string      `json:"errorCode"`
}

// ClientState is a live slot's VPN Client as UniFi's last read found it. It is computed on each view,
// never stored.
type ClientState uint8

const (
	ClientUnread ClientState = iota // No key, or no pass has read UniFi yet.
	ClientNone                      // No client of ours serves the slot yet.
	ClientPresent
	ClientDisabled // The owner turned it off in UniFi.
	ClientDeleted  // It was bound, and UniFi no longer holds it as ours.
)

// clientNames are the wire's words. An unread client says none, beside known false.
var clientNames = map[ClientState]string{ClientUnread: "none", ClientNone: "none", ClientPresent: "present", ClientDisabled: "disabled", ClientDeleted: "deleted"}

// SlotUnifi is what UniFi shows for a slot. Known is false without an API key or before a first pass.
type SlotUnifi struct {
	Name      string         `json:"name"`
	Interface string         `json:"interface"`
	Known     bool           `json:"known"`
	Client    string         `json:"client"`
	Trouble   *ClientTrouble `json:"trouble"`
}

func (c *controller) slotUnifi(row slotRow) SlotUnifi {
	return SlotUnifi{Name: row.Slot.Intent.Name(), Interface: fmt.Sprintf("pxa%d", row.Slot.Allocation.Ordinal), Known: c.clientsKnown, Client: clientNames[c.clientState(row)], Trouble: c.clientTrouble(row.Slot.ID)}
}

func (c *controller) clientTrouble(slot domain.SlotID) *ClientTrouble {
	if t, ok := c.clientTroubles[slot]; ok {
		return &t
	}
	return nil
}

func (c *controller) clientState(row slotRow) ClientState {
	owned, have := c.owned[row.Slot.ID]
	switch {
	case !c.clientsKnown:
		return ClientUnread
	case have && owned.Enabled != nil && !*owned.Enabled:
		return ClientDisabled
	case have:
		return ClientPresent
	case row.Slot.Allocation.NetworkID != "":
		return ClientDeleted
	}
	return ClientNone
}

// ForeignClientDTO is a VPN Client that isn't ours, or ours kept after its slot went, with what keeps it.
type ForeignClientDTO struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Enabled bool           `json:"enabled"`
	Trouble *ClientTrouble `json:"trouble"`
}

// VpnClients is UniFi's capacity. Known is false without an API key or before a first pass.
// Foreign holds the read-only rows of the board, so a client of ours kept for its routes after
// its slot went is one too.
type VpnClients struct {
	Known   bool               `json:"known"`
	Limit   int                `json:"limit"`
	Foreign []ForeignClientDTO `json:"foreign"`
}

func vpnClientsDTO() VpnClients { return VpnClients{Limit: unifi.Limit, Foreign: []ForeignClientDTO{}} }

func (c *controller) vpnClients() VpnClients {
	view := vpnClientsDTO()
	if !c.clientsKnown {
		return view
	}
	view.Known = true
	for _, f := range c.foreign {
		view.Foreign = append(view.Foreign, ForeignClientDTO{ID: string(f.ID), Name: f.Name, Enabled: f.Enabled})
	}
	for slot, owned := range c.owned {
		if row, live := c.state.slots[slot]; !live || row.Deleting {
			view.Foreign = append(view.Foreign, ForeignClientDTO{ID: string(owned.NetworkID), Name: owned.Name, Enabled: owned.Enabled == nil || *owned.Enabled, Trouble: c.clientTrouble(slot)})
		}
	}
	slices.SortFunc(view.Foreign, func(a, b ForeignClientDTO) int { return strings.Compare(a.ID, b.ID) })
	return view
}

// SlotView is schema.ts's SlotView.
type SlotView struct {
	CreatedAt     time.Time             `json:"createdAt"`
	ID            domain.SlotID         `json:"id"`
	Version       domain.IntentRevision `json:"version"`
	Name          string                `json:"name"`
	Exit          ExitDTO               `json:"exit"`
	Status        string                `json:"status"`
	DownReason    *string               `json:"downReason"`
	Server        *domain.LineageID     `json:"server"`
	AppliedServer *domain.LineageID     `json:"appliedServer"`
	LeavingBy     *time.Time            `json:"leavingBy"`
	Pending       bool                  `json:"pending"`
	Runtime       SlotReportDTO         `json:"runtime"`
	Unifi         SlotUnifi             `json:"unifi"`
	Backups       []domain.LineageID    `json:"backups"`
}

// appliedOn is the server the slot's box draws as applied, at any revision of it: the server it draws
// while the slot serves, its move no longer pending, and that server is the one it is on. Every other
// state the box draws as Switching to, Checking or its trouble. slots.ts's appliedOn is the same.
func (v SlotView) appliedOn() (domain.LineageID, bool) {
	drawn := v.drawnServer()
	if v.Status != "serving" || v.Pending || drawn == nil || v.Server == nil || *drawn != *v.Server {
		return "", false
	}
	return *drawn, true
}

// drawnServer is the server the slot's box draws it on: the one the runtime reports, or, after a pass
// that could vouch for no server and with no move pending, the one the daemon has it on, which its last
// landing showed. slots.ts's drawnServer is the same.
func (v SlotView) drawnServer() *domain.LineageID {
	if v.AppliedServer == nil && v.Runtime.Phase == phaseNames[gateway.Unknown] && !v.Pending {
		return v.Server
	}
	return v.AppliedServer
}

// AmongDTO is schema.ts's among: is any of these, or with not none of them.
type AmongDTO[T any] struct {
	Not    bool `json:"not"`
	Values []T  `json:"values"`
}

func amongDTO[T comparable, W any](a domain.Among[T], wire func(T) W) AmongDTO[W] {
	out := AmongDTO[W]{Not: a.Not, Values: []W{}}
	for _, v := range a.Values {
		out.Values = append(out.Values, wire(v))
	}
	return out
}

func same[T any](v T) T { return v }

func amongOf[T comparable, W any](dto AmongDTO[W], parse func(W) (T, error)) (domain.Among[T], error) {
	out := domain.Among[T]{Not: dto.Not}
	for _, w := range dto.Values {
		v, err := parse(w)
		if err != nil {
			return domain.Among[T]{}, err
		}
		out.Values = append(out.Values, v)
	}
	return out, nil
}

func identity[T any](v T) (T, error) { return v, nil }

func protocolOf(name string) (domain.Protocol, error) {
	for p, n := range protocolNames {
		if n == name {
			return p, nil
		}
	}
	return 0, fmt.Errorf("%q is not a protocol", name)
}

func protocolName(p domain.Protocol) string { return protocolNames[p] }

// MembersDTO is schema.ts's Members.
type MembersDTO struct {
	Sources   AmongDTO[domain.SourceID] `json:"sources"`
	Countries AmongDTO[string]          `json:"countries"`
	Protocols AmongDTO[string]          `json:"protocols"`
	Include   string                    `json:"include"`
	Exclude   string                    `json:"exclude"`
	Box       *string                   `json:"box"` // Null is unset.
}

func membersDTO(m domain.Members) MembersDTO {
	include, exclude := m.Names.Patterns()
	return MembersDTO{Sources: amongDTO(m.Sources, same), Countries: amongDTO(m.Countries, same), Protocols: amongDTO(m.Protocols, protocolName),
		Include: include, Exclude: exclude, Box: boxDTO(m.Box)}
}

func boxDTO(b domain.Box) *string {
	if name, ok := boxNames[b]; ok {
		return &name
	}
	return nil
}

func (dto MembersDTO) admit() (domain.Members, error) {
	names, err := domain.NewFilter(dto.Include, dto.Exclude)
	if err != nil {
		return domain.Members{}, err
	}
	m := domain.Members{Names: names}
	if dto.Box != nil {
		if m.Box, err = boxOf(*dto.Box); err != nil || m.Box == domain.BoxUnset {
			return domain.Members{}, fmt.Errorf("%q is not a box condition", *dto.Box)
		}
	}
	if m.Sources, err = amongOf(dto.Sources, identity); err != nil {
		return domain.Members{}, err
	}
	if m.Countries, err = amongOf(dto.Countries, identity); err != nil {
		return domain.Members{}, err
	}
	if m.Protocols, err = amongOf(dto.Protocols, protocolOf); err != nil {
		return domain.Members{}, err
	}
	return domain.AdmitMembers(m)
}

// GroupInputDTO is schema.ts's GroupInput: a replace's body.
type GroupInputDTO struct {
	Name    string     `json:"name"`
	Members MembersDTO `json:"members"`
}

// GroupCreateDTO is schema.ts's GroupCreate: a create's body, the group's ticks with it.
type GroupCreateDTO struct {
	GroupInputDTO
	Ticked []domain.LineageID `json:"ticked"`
}

// GroupView is schema.ts's GroupView. Resolved comes from Go, because RE2 runs only here.
type GroupView struct {
	ID          domain.GroupID       `json:"id"`
	Revision    domain.GroupRevision `json:"revision"`
	Name        string               `json:"name"`
	Members     MembersDTO           `json:"members"`
	Ticked      []domain.LineageID   `json:"ticked"`
	Resolved    []domain.LineageID   `json:"resolved"`
	UsedBy      []domain.SlotID      `json:"usedBy"`
	Moves7d     int                  `json:"moves7d"`
	FirstChoice *domain.LineageID    `json:"firstChoice"`
	Order       []domain.LineageID   `json:"order"`
	Preferred   *PreferredDTO        `json:"preferred"`
}

// PreferredDTO is the server a group's or source's slots were moved to by hand, and when its boost ends.
type PreferredDTO struct {
	Server domain.LineageID `json:"server"`
	EndsAt time.Time        `json:"endsAt"`
}

// ownerFacts is what a group or source has in common as an Exit: how often it moved in the week, what
// a new slot on it would get, and the server its owner prefers until when.
type ownerFacts struct {
	Moves7d     int
	FirstChoice *domain.LineageID
	Order       []domain.LineageID // The owner's order, keep-out included.
	Preferred   *PreferredDTO
}

func groupView(g domain.Group, w world, f ownerFacts) GroupView {
	view := GroupView{ID: g.ID, Revision: g.Revision, Name: g.Name, Members: membersDTO(g.Members), Ticked: append([]domain.LineageID{}, g.Ticked...),
		Resolved: []domain.LineageID{}, UsedBy: []domain.SlotID{}, Moves7d: f.Moves7d, FirstChoice: f.FirstChoice,
		Order: append([]domain.LineageID{}, f.Order...), Preferred: f.Preferred}
	for _, l := range w.members[g.ID] {
		view.Resolved = append(view.Resolved, l.ID)
	}
	view.UsedBy = append(view.UsedBy, w.usedBy(g.ID)...)
	return view
}

// QuotaDTO is schema.ts's Quota.
type QuotaDTO struct {
	Upload     *uint64    `json:"upload"`
	Download   *uint64    `json:"download"`
	Limit      *uint64    `json:"limit"`
	Expires    *time.Time `json:"expires"`
	Refills    *time.Time `json:"refills"`
	ObservedAt *time.Time `json:"observedAt"`
}

// ProviderDTO is what a provider says about its source.
type ProviderDTO struct {
	Title                   string   `json:"title"`
	Announcement            string   `json:"announcement"`
	SupportURL              string   `json:"supportUrl"`
	ProfileURL              string   `json:"profileUrl"`
	SuggestedRefreshSeconds *float64 `json:"suggestedRefreshSeconds"`
	Quota                   QuotaDTO `json:"quota"`
}

// FetchDTO is the display-safe part of a source's fetch state.
type FetchDTO struct {
	CheckedAt   *time.Time             `json:"checkedAt"`
	UpdatedAt   *time.Time             `json:"updatedAt"`
	Failures    uint32                 `json:"failures"`
	FailureCode *catalogue.FailureCode `json:"failureCode"`
}

// DiagnosticDTO is schema.ts's Diagnostic.
type DiagnosticDTO struct {
	Entry int                      `json:"entry"`
	Code  catalogue.DiagnosticCode `json:"code"`
	Field string                   `json:"field"`
}

// RemoteOriginDTO is what a fetched source shows. The URL and device ID are write-only and
// never appear here.
type RemoteOriginDTO struct {
	UserAgent           string      `json:"userAgent"`
	RefreshEverySeconds float64     `json:"refreshEverySeconds"`
	Paused              bool        `json:"paused"`
	Provider            ProviderDTO `json:"provider"`
	Fetch               FetchDTO    `json:"fetch"`
	DueAt               time.Time   `json:"dueAt"`
}

// OriginDTO is schema.ts's origin. A manual source is its kind alone, and its text is
// write-only.
type OriginDTO struct {
	Kind string `json:"kind"`
	*RemoteOriginDTO
}

// FeedDTO is schema.ts's Feed: what the source's latest document listed.
type FeedDTO struct {
	Configs []FeedConfigDTO `json:"configs"`
}

type FeedConfigDTO struct {
	Key     *domain.LineageID `json:"key"`
	Place   int               `json:"place"`
	Name    string            `json:"name"`
	Servers int               `json:"servers"`
	Skipped []SkipDTO         `json:"skipped"`
}

// SkipDTO is one of schema.ts's Skip variants, by reason. A field another reason has is absent.
type SkipDTO struct {
	Reason   string  `json:"reason"`
	Index    int     `json:"index"`
	Tag      string  `json:"tag"`
	Protocol string  `json:"protocol,omitempty"`
	Position int     `json:"position,omitempty"`
	Code     string  `json:"code,omitempty"`
	Field    *string `json:"field,omitempty"`
}

var skipReasons = map[domain.SkipReason]string{domain.SkipHelper: "helper", domain.SkipHop: "hop", domain.SkipRefused: "refused"}

func feedDTO(f *domain.Feed) *FeedDTO {
	if f == nil {
		return nil
	}
	out := &FeedDTO{Configs: make([]FeedConfigDTO, 0, len(f.Configs))}
	for _, c := range f.Configs {
		row := FeedConfigDTO{Place: c.Place, Name: catalogue.PlainLabel(c.Name), Servers: c.Servers, Skipped: make([]SkipDTO, 0, len(c.Skipped))}
		if c.Key != "" {
			key := c.Key
			row.Key = &key
		}
		for _, k := range c.Skipped {
			skip := SkipDTO{Reason: skipReasons[k.Reason], Index: k.Index, Tag: k.Tag}
			switch k.Reason {
			case domain.SkipHelper:
				skip.Protocol = k.Protocol
			case domain.SkipRefused:
				field := k.Field
				skip.Position, skip.Code, skip.Field = k.Position, k.Code, &field
			}
			row.Skipped = append(row.Skipped, skip)
		}
		out.Configs = append(out.Configs, row)
	}
	return out
}

// SourceView is schema.ts's SourceView.
type SourceView struct {
	ID          domain.SourceID       `json:"id"`
	Revision    domain.SourceRevision `json:"revision"`
	Name        string                `json:"name"`
	NameSet     bool                  `json:"nameSet"` // The owner typed it. Otherwise it follows the provider.
	FirstChoice *domain.LineageID     `json:"firstChoice"`
	Order       []domain.LineageID    `json:"order"`
	Preferred   *PreferredDTO         `json:"preferred"`
	Moves7d     int                   `json:"moves7d"`
	Origin      OriginDTO             `json:"origin"`
	Feed        *FeedDTO              `json:"feed"` // Null until a document is read.
	Stored      int                   `json:"stored"`
	Missing     int                   `json:"missing"`
	UsedBy      []domain.SlotID       `json:"usedBy"`  // Every slot a delete of the source is refused for.
	Removes     []domain.GroupID      `json:"removes"` // The groups a delete of the source removes with it.
	Diagnostics []DiagnosticDTO       `json:"diagnostics"`
	AddedAt     *time.Time            `json:"addedAt"` // Null for a source stored before state.db kept it.
}

// View is schema.ts's View: what api/state returns, published after every commit.
type View struct {
	Revision     domain.IntentRevision `json:"revision"`
	GeneratedAt  time.Time             `json:"generatedAt"`
	Slots        []SlotView            `json:"slots"`
	Sources      []SourceView          `json:"sources"`
	Groups       []GroupView           `json:"groups"`
	WAN          string                `json:"wan"`
	VPNClients   VpnClients            `json:"vpnClients"`
	Core         CoreView              `json:"core"`
	UniFi        UniFiView             `json:"unifi"`
	UniFiTrouble *UniFiTrouble         `json:"unifiTrouble"`
}

// CoreView is schema.ts's CoreView: the release's pinned Xray and the Xray running, as their
// binaries say. They differ after an upgrade whose core restart didn't happen, until a restart.
// Each is null while unknown: running while no core runs.
type CoreView struct {
	Version *string `json:"version"`
	Running *string `json:"running"`
}

func coreView(v gateway.CoreVersions) CoreView {
	known := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	return CoreView{Version: known(v.Release), Running: known(v.Running)}
}

// UniFiView is schema.ts's UniFiView. AcceptedAt is when UniFi last answered the key, kept in
// memory: null from a start until the first pass UniFi answers.
type UniFiView struct {
	AcceptedAt *time.Time `json:"acceptedAt"`
}

// UniFiTrouble is what keeps UniFi from making or changing any VPN Client: the daemon has no API key, UniFi refused it, or
// UniFi's Network Application didn't answer the last pass. Slots keep serving through each. Null in the view when none.
type UniFiTrouble string

const (
	unifiNoKey        UniFiTrouble = "noKey"
	unifiKeyRefused   UniFiTrouble = "keyRefused"
	unifiNotAnswering UniFiTrouble = "notAnswering"
)

// ClientTrouble is what the last pass couldn't do for one VPN Client of ours: another network holds the slot's name, UniFi's
// limit left no room to create it, UniFi refused the write, or, for a client kept after its slot went, Policy Table routes
// still point at it. A client UniFi deleted is its client state, `deleted`, and no trouble. Null when none.
type ClientTrouble string

const (
	clientNameTaken    ClientTrouble = "nameTaken"
	clientLimitReached ClientTrouble = "limitReached"
	clientRefused      ClientTrouble = "refused"
	clientRouted       ClientTrouble = "routed"
)

// clientTroubles names the problems the page draws. The rest, a client gone from UniFi or a pass cut short, the page shows
// as the client's state or UniFi's trouble.
var clientTroubles = map[unifi.ProblemKind]ClientTrouble{
	unifi.NameCollision:   clientNameTaken,
	unifi.LimitReached:    clientLimitReached,
	unifi.RefusedByApp:    clientRefused,
	unifi.RoutedElsewhere: clientRouted,
}

var wanNames = map[domain.WANState]string{domain.WANUnknown: "unknown", domain.WANUp: "up", domain.WANDown: "down"}

// EvidenceDTO is schema.ts's EvidenceSummary.
type EvidenceDTO struct {
	Server          domain.LineageID `json:"server"`
	Kind            string           `json:"kind"`
	Verdict         string           `json:"verdict"`
	Target          string           `json:"target"`
	Round           uint8            `json:"round"`
	At              time.Time        `json:"at"`
	PingMs          *float64         `json:"pingMs"`
	NewConnectionMs *float64         `json:"newConnectionMs"`
}

var kindNames = map[domain.SignalKind]string{domain.TCPInfo: "tcpInfo", domain.OutboundCounters: "outboundCounters",
	domain.CoreWarning: "coreWarning", domain.ActiveProbe: "activeProbe", domain.WANBaseline: "wanBaseline"}

var reasonNames = map[domain.Reason]string{
	domain.InitialSelection: "initialSelection", domain.FailureMove: "failureMove", domain.SlownessMove: "slownessMove",
	domain.ImprovementMove: "improvementMove", domain.ExitChanged: "exitChanged",
	domain.ServerLeftFeed: "serverLeftFeed", domain.RefusedByCore: "serverRefused", domain.RevisionChanged: "revisionChanged",
	domain.Recovery: "recovery", domain.UDPMove: "udpMove", domain.PreferredMove: "preferred",
}

// OwnerDTO is who moved: a slot on its own, or a group or source with every slot on it.
type OwnerDTO struct {
	Kind   string          `json:"kind"`
	Slot   domain.SlotID   `json:"slot,omitempty"`
	Group  domain.GroupID  `json:"group,omitempty"`
	Source domain.SourceID `json:"source,omitempty"`
}

func ownerDTO(o domain.Owner) OwnerDTO {
	if id, ok := o.Group(); ok {
		return OwnerDTO{Kind: "group", Group: id}
	}
	if id, ok := o.Source(); ok {
		return OwnerDTO{Kind: "source", Source: id}
	}
	id, _ := o.Slot()
	return OwnerDTO{Kind: "slot", Slot: id}
}

// SwitchEvent is schema.ts's SwitchEvent: one move of one owner, with the slots it moved.
type SwitchEvent struct {
	ID          domain.EventID        `json:"id"`
	Owner       OwnerDTO              `json:"owner"`
	Slots       []domain.SlotID       `json:"slots"`
	Intent      domain.IntentRevision `json:"intent"`
	From        SelectionDTO          `json:"from"`
	To          SelectionDTO          `json:"to"`
	Reason      string                `json:"reason"`
	Evidence    []EvidenceDTO         `json:"evidence"`
	RequestedAt time.Time             `json:"requestedAt"`
	AppliedAt   *time.Time            `json:"appliedAt"`
}

func millis(d *time.Duration) *float64 {
	if d == nil {
		return nil
	}
	ms := float64(d.Microseconds()) / 1000
	return &ms
}

func switchEvent(s domain.Switch) SwitchEvent {
	e := SwitchEvent{ID: s.ID, Owner: ownerDTO(s.Owner), Slots: append([]domain.SlotID{}, s.Slots...), Intent: s.Intent, From: selectionDTO(s.From), To: selectionDTO(s.To),
		Reason: reasonNames[s.Reason], Evidence: []EvidenceDTO{}, RequestedAt: s.RequestedAt, AppliedAt: s.AppliedAt}
	for _, ev := range s.Evidence {
		e.Evidence = append(e.Evidence, EvidenceDTO{Server: ev.Server.Lineage, Kind: kindNames[ev.Kind], Verdict: verdictNames[ev.Verdict],
			Target: string(ev.Target), Round: ev.Round, At: ev.At, PingMs: millis(ev.Ping), NewConnectionMs: millis(ev.NewConn)})
	}
	return e
}

// SettingsView is schema.ts's SettingsView.
type SettingsView struct {
	Settings
	Revision uint64 `json:"revision"`
}

// ProbeResult is schema.ts's ProbeResult.
type ProbeResult struct {
	Lineage         domain.LineageID `json:"lineage"`
	Verdict         string           `json:"verdict"`
	PingMs          *float64         `json:"pingMs"`
	NewConnectionMs *float64         `json:"newConnectionMs"`
	At              time.Time        `json:"at"`
}

type AnalyticsQuery struct {
	From, To time.Time
	Bucket   time.Duration
	Sources  []domain.SourceID
	Lineages []domain.LineageID
}

// AnalyticsBucket is schema.ts's AnalyticsBucket: one lineage in one bucket.
type AnalyticsBucket struct {
	Start                     time.Time        `json:"start"`
	Source                    domain.SourceID  `json:"source"`
	Lineage                   domain.LineageID `json:"lineage"`
	Country                   string           `json:"country"`
	Uploaded                  uint64           `json:"uploaded"`
	Downloaded                uint64           `json:"downloaded"`
	ProbeSuccesses            uint64           `json:"probeSuccesses"`
	ProbeAttempts             uint64           `json:"probeAttempts"`
	WANExcluded               uint64           `json:"wanExcluded"`
	Unknown                   uint64           `json:"unknown"`
	PingMedianMs              *float64         `json:"pingMedianMs"`
	PingP95Ms                 *float64         `json:"pingP95Ms"`
	NewConnectionMedianMs     *float64         `json:"newConnectionMedianMs"`
	ForcedSwitchAways         uint64           `json:"forcedSwitchAways"`
	ServedSeconds             float64          `json:"servedSeconds"`
	Incidents                 int              `json:"incidents"`
	ManagementTrafficIncluded bool             `json:"managementTrafficIncluded"`
}

// AnalyticsReport is schema.ts's AnalyticsReport.
type AnalyticsReport struct {
	Buckets      []AnalyticsBucket `json:"buckets"`
	CoveredUntil time.Time         `json:"coveredUntil"`
	GapCount     uint64            `json:"gapCount"`
}

type Cursor string

type PageQuery struct {
	Source  domain.SourceID
	Country string
	Cursor  Cursor
	Limit   int
}

// CountryDTO says where a lineage's country came from.
type CountryDTO struct {
	Code   *string `json:"code"`
	Source string  `json:"source"`
}

// HealthDTO is schema.ts's server health. week.go fills it.
type HealthDTO struct {
	Verdict            string     `json:"verdict"`
	CheckedAt          *time.Time `json:"checkedAt"`
	PingAt             *time.Time `json:"pingAt"` // The newest sample to the ranking site. A heartbeat measures no ping.
	Current            bool       `json:"current"`
	PingP50Ms          *float64   `json:"pingP50Ms"`
	PingP95Ms          *float64   `json:"pingP95Ms"`
	NewConnectionP50Ms *float64   `json:"newConnectionP50Ms"`
	NewConnectionP95Ms *float64   `json:"newConnectionP95Ms"`
	Score              *float64   `json:"score"` // Ping times factor, in ms. Null with no sample to the ranking site.
	Factor             float64    `json:"factor"`
	Incidents7d        int        `json:"incidents7d"`
	SeenDays           float64    `json:"seenDays"`        // Days seen working, decayed: what pays the factor down.
	SeenWorking7dMs    *int64     `json:"seenWorking7dMs"` // Null until the record has kept it for the whole week or since the server was first seen.
	LastFailureAt      *time.Time `json:"lastFailureAt"`
	FirstSeenAt        *time.Time `json:"firstSeenAt"`
}

// SeenDTO is one place a server was seen.
type SeenDTO struct {
	Config    string           `json:"config"`
	ConfigKey domain.LineageID `json:"configKey"`
	Tag       string           `json:"tag"`
	Position  int              `json:"position"`
}

func seenDTO(o domain.Occurrence) SeenDTO {
	return SeenDTO{Config: catalogue.PlainLabel(o.ConfigName), ConfigKey: o.Config, Tag: o.Tag, Position: o.Position}
}

// NameDTO is schema.ts's Name: the parts of a server's name it has, each plain.
type NameDTO struct {
	Remarks *string `json:"remarks"`
	Tag     *string `json:"tag"`
}

func nameDTO(n domain.Name) *NameDTO {
	if n.IsZero() {
		return nil
	}
	part := func(text string) *string {
		if text == "" {
			return nil
		}
		plain := catalogue.PlainLabel(text)
		return &plain
	}
	return &NameDTO{Remarks: part(n.Remarks()), Tag: part(n.Tag())}
}

// ServerView is schema.ts's ServerView.
type ServerView struct {
	ID           domain.LineageID  `json:"id"`
	Source       domain.SourceID   `json:"source"`
	Label        string            `json:"label"`
	Name         *NameDTO          `json:"name"`
	Home         *domain.LineageID `json:"home"`
	Number       *int              `json:"number"`
	Country      CountryDTO        `json:"country"`
	Presence     string            `json:"presence"`
	Supported    bool              `json:"supported"`
	UDP          string            `json:"udp"`
	Seen         []SeenDTO         `json:"seen"`
	Protocol     string            `json:"protocol"`
	Transport    string            `json:"transport"`
	MissingSince *time.Time        `json:"missingSince"`
	Health       HealthDTO         `json:"health"`
}

type ServerPage struct {
	Items []ServerView `json:"items"`
	Next  Cursor       `json:"next"`
}

var presenceNames = map[domain.Presence]string{domain.Present: "present", domain.Missing: "missing", domain.Tombstone: "tombstone"}

var protocolNames = map[domain.Protocol]string{domain.VLESS: "vless", domain.VMess: "vmess", domain.Trojan: "trojan", domain.Shadowsocks: "shadowsocks",
	domain.SOCKS: "socks", domain.HTTPConnect: "http", domain.Hysteria2: "hysteria2"}

var transportNames = map[domain.TransportKind]string{domain.TCP: "tcp", domain.WebSocket: "ws", domain.GRPC: "grpc", domain.XHTTP: "xhttp", domain.Hysteria: "hysteria"}

var countrySources = map[domain.CountrySource]string{domain.CountryUnknown: "unknown", domain.CountryRule: "rule", domain.CountryTagFlag: "tagFlag",
	domain.CountryConfigFlag: "configFlag"}

// Queries answers the page's reads from state.db and metrics.db, read-only, so it can
// run beside a live daemon. App embeds it.
type Queries struct {
	state   *queries
	metrics *metrics.Reader
}

// OpenQueries opens both stores read-only. Open has already created them on a gateway.
func OpenQueries(b Bootstrap) (*Queries, error) {
	state, err := openQueries(b.DataDir)
	if err != nil {
		return nil, err
	}
	reader, err := metrics.OpenReader(metricsPath(b.DataDir), metrics.DefaultOptions())
	if err != nil {
		state.close()
		return nil, err
	}
	return &Queries{state: state, metrics: reader}, nil
}

func (q *Queries) Close() error {
	return firstError(q.state.close(), q.metrics.Close())
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

const (
	maxSwitchEvents = 200
	maxPage         = 200
)

// Switches lists the newest switch events, newest first.
func (q *Queries) Switches(ctx context.Context) ([]SwitchEvent, error) {
	events, err := q.state.switches(ctx, maxSwitchEvents)
	if err != nil {
		return nil, err
	}
	out := make([]SwitchEvent, 0, len(events))
	for _, e := range events {
		out = append(out, switchEvent(e))
	}
	return out, nil
}

// ReplacedEvent is schema.ts's ReplacedEvent: a refresh kept a server's identity across a
// changed connection, on a match of its credentials or name alone. It moves no slot.
type ReplacedEvent struct {
	Source        domain.SourceID  `json:"source"`
	Server        domain.LineageID `json:"server"`
	Label         string           `json:"label"`
	PreviousLabel string           `json:"previousLabel"`
	At            time.Time        `json:"at"`
}

// RefreshEventDTO is schema.ts's RefreshEvent: one fetch of a source. A failure the WAN explains
// doesn't count against the provider.
type RefreshEventDTO struct {
	At          time.Time              `json:"at"`
	Outcome     string                 `json:"outcome"`
	FailureCode *catalogue.FailureCode `json:"failureCode"`
	WANExcluded bool                   `json:"wanExcluded"`
	Added       int                    `json:"added"`
	Changed     int                    `json:"changed"`
	Missing     int                    `json:"missing"`
}

var outcomeNames = map[catalogue.Outcome]string{catalogue.Updated: "updated", catalogue.Unchanged: "unchanged", catalogue.FetchFailed: "failed"}

const maxRefreshEvents = 50

// Refreshes lists a source's newest fetches, newest first.
func (q *Queries) Refreshes(ctx context.Context, source domain.SourceID) ([]RefreshEventDTO, error) {
	return q.state.refreshes(ctx, source, maxRefreshEvents)
}

// Replaced lists the newest replaced servers, newest first.
func (q *Queries) Replaced(ctx context.Context) ([]ReplacedEvent, error) {
	return q.state.replaced(ctx, maxSwitchEvents)
}

// Servers lists one page of servers. Supported and health are the caller's to fill,
// since they live in the running daemon.
func (q *Queries) Servers(ctx context.Context, page PageQuery) (ServerPage, error) {
	limit := page.Limit
	if limit <= 0 || limit > maxPage {
		limit = maxPage
	}
	rows, err := q.state.lineagePage(ctx, page.Source, page.Country, string(page.Cursor), limit+1)
	if err != nil {
		return ServerPage{}, err
	}
	out := ServerPage{Items: []ServerView{}}
	if len(rows) > limit {
		rows = rows[:limit]
		out.Next = Cursor(rows[limit-1].ID)
	}
	for _, l := range rows {
		out.Items = append(out.Items, serverView(l))
	}
	return out, nil
}

func serverView(l domain.Lineage) ServerView {
	view := ServerView{ID: l.ID, Source: l.Source, Label: catalogue.PlainLabel(l.Label), Name: nameDTO(l.Naming.Name), Presence: presenceNames[l.Presence], Supported: true, UDP: udpNames[udpUnknown],
		Country: CountryDTO{Source: countrySources[l.Country.Source]}}
	view.Seen = make([]SeenDTO, 0, len(l.Seen))
	for _, o := range l.Seen {
		view.Seen = append(view.Seen, seenDTO(o))
	}
	if l.Naming.Home != "" {
		home := l.Naming.Home
		view.Home = &home
	}
	if l.Naming.Number > 0 {
		number := l.Naming.Number
		view.Number = &number
	}
	if l.Country.Code != "" {
		code := l.Country.Code
		view.Country.Code = &code
	}
	if !l.MissingSince.IsZero() {
		since := l.MissingSince
		view.MissingSince = &since
	}
	kind := l.Server.Kind()
	view.Protocol, view.Transport = protocolNames[kind.Protocol], transportNames[kind.Transport]
	return view
}

// Analytics joins measurements from metrics.db with dimensions and events from
// state.db. Neither store reads the other's schema; the join happens here.
//
// Probe counts are the probes metrics.db counts. Forced switch-aways count applied events, and a WAN
// outage counts for nobody. A slot still serving counts until now.
func (q *Queries) Analytics(ctx context.Context, query AnalyticsQuery, now time.Time) (AnalyticsReport, error) {
	series, err := q.metrics.Series(ctx, metrics.Window{From: query.From, To: query.To, Step: query.Bucket}, query.Lineages)
	if err != nil {
		return AnalyticsReport{}, err
	}
	dimensions, err := q.state.dimensions(ctx, query.Lineages)
	if err != nil {
		return AnalyticsReport{}, err
	}
	events, err := q.state.events(ctx, query.From, query.To, query.Bucket)
	if err != nil {
		return AnalyticsReport{}, err
	}
	served, err := usageSince(ctx, q.state.reader, query.From, minTime(query.To, now))
	if err != nil {
		return AnalyticsReport{}, err
	}
	return analyticsReport(series, dimensions, events, served, query), nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

type rowKey struct {
	start   int64
	lineage domain.LineageID
}

// analyticsReport is the pure join.
func analyticsReport(series metrics.Series, dimensions map[domain.LineageID]dimension, events eventCounts, served usage, query AnalyticsQuery) AnalyticsReport {
	report := AnalyticsReport{Buckets: []AnalyticsBucket{}, CoveredUntil: series.CoveredUntil, GapCount: series.Gaps}
	wanted := func(id domain.LineageID) (dimension, bool) {
		d, ok := dimensions[id]
		if !ok || (len(query.Sources) > 0 && !slices.Contains(query.Sources, d.Source)) ||
			(len(query.Lineages) > 0 && !slices.Contains(query.Lineages, id)) {
			return d, false
		}
		return d, true
	}
	rows := map[rowKey]*AnalyticsBucket{}
	row := func(start time.Time, id domain.LineageID, d dimension) *AnalyticsBucket {
		k := rowKey{start.UnixMilli(), id}
		r := rows[k]
		if r == nil {
			r = &AnalyticsBucket{Start: start, Source: d.Source, Lineage: id, Country: d.Country}
			rows[k] = r
		}
		return r
	}

	medians, p95s, newConns := map[rowKey][]float64{}, map[rowKey][]float64{}, map[rowKey][]float64{}
	for _, b := range series.Buckets {
		d, ok := wanted(b.Lineage)
		if !ok {
			continue
		}
		r := row(b.Start, b.Lineage, d)
		if b.Purpose == domain.Heartbeat {
			r.ProbeAttempts += b.Attempts
			r.ProbeSuccesses += b.Successes
			r.WANExcluded += b.WANExcluded
			r.Unknown += b.Unknown
		}
		r.ManagementTrafficIncluded = r.ManagementTrafficIncluded || b.Attempts > 0
		k := rowKey{b.Start.UnixMilli(), b.Lineage}
		if b.NewConnMedian != nil {
			newConns[k] = append(newConns[k], float64(b.NewConnMedian.Microseconds())/1000)
		}
		if b.PingP95 != nil {
			p95s[k] = append(p95s[k], float64(b.PingP95.Microseconds())/1000)
		}
		if b.PingMedian == nil {
			continue
		}
		medians[k] = append(medians[k], float64(b.PingMedian.Microseconds())/1000)
	}
	for k, r := range rows {
		r.PingMedianMs = medianOf(medians[k])
		r.PingP95Ms = medianOf(p95s[k])
		r.NewConnectionMedianMs = medianOf(newConns[k])
	}
	window := interval{query.From, query.To}
	for id := range served.servers {
		if d, ok := wanted(id); ok {
			for start, t := range steps(served.servers[id], window, query.Bucket) {
				row(time.UnixMilli(start).UTC(), id, d).ServedSeconds += t.Seconds()
			}
		}
	}
	for id := range served.incidents {
		if d, ok := wanted(id); ok {
			for start, n := range served.incidentSteps(id, window, query.Bucket) {
				row(time.UnixMilli(start).UTC(), id, d).Incidents += n
			}
		}
	}
	for _, t := range series.Traffic {
		if d, ok := wanted(t.Lineage); ok {
			r := row(t.Start, t.Lineage, d)
			r.Uploaded += t.Up
			r.Downloaded += t.Down
		}
	}
	for k, n := range events.ForcedSwitchAways {
		if d, ok := wanted(k.id); ok {
			row(time.UnixMilli(k.start).UTC(), k.id, d).ForcedSwitchAways += n
		}
	}
	for _, r := range rows {
		report.Buckets = append(report.Buckets, *r)
	}
	slices.SortFunc(report.Buckets, func(a, b AnalyticsBucket) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		if a.Lineage < b.Lineage {
			return -1
		}
		return 1
	})
	return report
}

func medianOf(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	m := values[len(values)/2]
	if len(values)%2 == 0 {
		m = (values[len(values)/2-1] + m) / 2
	}
	return &m
}

// sourceName is what a source is called: the owner's name, else its provider's title, else its URL's host.
func sourceName(source domain.Source, title string) string {
	if source.Name != "" {
		return source.Name
	}
	if title != "" {
		return title
	}
	remote, _ := source.Origin.Remote()
	return remote.Host
}

// sourceNames is what each source is called. A name that follows the provider and is already taken,
// by a name the owner typed or one earlier in ID order, gets the next free number.
func sourceNames(sources map[domain.SourceID]domain.Source, fetches map[domain.SourceID]catalogue.FetchState) map[domain.SourceID]string {
	out := map[domain.SourceID]string{}
	taken := map[string]bool{}
	for id, s := range sources {
		if s.Name != "" {
			out[id], taken[s.Name] = s.Name, true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(sources)) {
		if sources[id].Name != "" {
			continue
		}
		base := sourceName(sources[id], fetches[id].Provider().Title)
		name := base
		for n := 2; taken[name]; n++ {
			name = fmt.Sprintf("%s %d", base, n)
		}
		out[id], taken[name] = name, true
	}
	return out
}

// sourceView builds a source's public row.
func sourceView(source domain.Source, name string, fs catalogue.FetchState, w world, due time.Time, diff *refreshEvent, f ownerFacts) SourceView {
	c := w.catalogue
	view := SourceView{ID: source.ID, Revision: source.Revision, Name: name, NameSet: source.Name != "",
		FirstChoice: f.FirstChoice, Order: append([]domain.LineageID{}, f.Order...), Preferred: f.Preferred, Moves7d: f.Moves7d,
		Origin: OriginDTO{Kind: "manual"}, Feed: feedDTO(source.Feed), Diagnostics: []DiagnosticDTO{}, UsedBy: []domain.SlotID{}, Removes: []domain.GroupID{}}
	deletion := w.sourceDeletion(source.ID)
	view.UsedBy = append(view.UsedBy, deletion.usedBy...)
	view.Removes = append(view.Removes, slices.Sorted(maps.Keys(deletion.removed))...)
	if remote, ok := source.Origin.Remote(); ok {
		view.Origin = OriginDTO{Kind: "remote", RemoteOriginDTO: remoteOriginDTO(remote, fs, due)}
	}
	if !source.Added.IsZero() {
		view.AddedAt = ptrTo(source.Added)
	}
	for _, l := range c.Source(source.ID) {
		switch l.Presence {
		case domain.Present:
			view.Stored++
		case domain.Missing:
			view.Missing++
		}
	}
	if diff != nil {
		view.Diagnostics = diagnosticDTOs(diff.Diagnostics)
	}
	return view
}

func diagnosticDTOs(diagnostics []catalogue.Diagnostic) []DiagnosticDTO {
	out := make([]DiagnosticDTO, 0, len(diagnostics))
	for _, d := range diagnostics {
		out = append(out, DiagnosticDTO{Entry: d.Entry, Code: d.Code, Field: d.Field})
	}
	return out
}

func remoteOriginDTO(remote domain.Remote, fs catalogue.FetchState, due time.Time) *RemoteOriginDTO {
	meta, status := fs.Provider(), fs.Status()
	view := &RemoteOriginDTO{
		UserAgent: remote.UserAgent, RefreshEverySeconds: remote.RefreshEvery.Seconds(), Paused: remote.Paused, DueAt: due,
		Provider: ProviderDTO{Title: meta.Title, Announcement: meta.Announcement, SupportURL: meta.SupportURL, ProfileURL: meta.ProfileURL,
			Quota: QuotaDTO{Upload: meta.Quota.Upload, Download: meta.Quota.Download, Limit: meta.Quota.Limit, Expires: meta.Quota.Expires, Refills: meta.Quota.Refills}},
		Fetch: FetchDTO{Failures: status.Failures},
	}
	if meta.SuggestedRefresh > 0 {
		s := meta.SuggestedRefresh.Seconds()
		view.Provider.SuggestedRefreshSeconds = &s
	}
	if !meta.Quota.ObservedAt.IsZero() {
		at := meta.Quota.ObservedAt
		view.Provider.Quota.ObservedAt = &at
	}
	if !status.CheckedAt.IsZero() {
		at := status.CheckedAt
		view.Fetch.CheckedAt = &at
	}
	if !status.UpdatedAt.IsZero() {
		at := status.UpdatedAt
		view.Fetch.UpdatedAt = &at
	}
	if status.FailureCode != "" {
		code := status.FailureCode
		view.Fetch.FailureCode = &code
	}
	return view
}

func (e *CommandError) Error() string { return fmt.Sprintf("%s: %s", e.Kind, e.Message) }

var refusalStatus = map[CommandKind]int{
	KindValidation: http.StatusUnprocessableEntity, KindStale: http.StatusPreconditionFailed, KindUnsupported: http.StatusUnprocessableEntity,
	KindCapacity: http.StatusConflict, KindReferenced: http.StatusConflict, KindUnavailable: http.StatusServiceUnavailable, KindNotFound: http.StatusNotFound,
}

// answer is the refusal as web/API.md words it. A missing item is a validation refusal
// with a 404, which is how the page tells it from any other 404.
func (e *CommandError) answer() *Answer {
	body := errorBody{Kind: string(e.Kind), Reason: e.Reason, Message: e.Message}
	if e.Kind == KindNotFound {
		body.Kind = string(KindValidation)
	}
	a := &Answer{Status: refusalStatus[e.Kind]}
	if e.Kind == KindStale {
		body.Revision = &e.Revision
		a.ETag = fmt.Sprintf("%q", strconv.FormatUint(e.Revision, 10))
	}
	a.Body, _ = json.Marshal(body)
	return a
}
