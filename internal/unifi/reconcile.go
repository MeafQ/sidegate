package unifi

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"sidegate/internal/domain"
)

// Limit is UniFi's cap on VPN Clients of any type, per help.ui.com. On 10.6.109
// only the UI enforces it: the API accepted 11 with a gateway adopted. So this
// client enforces it, counting the owner's clients too.
const Limit = 8

// Syncer is what the controller calls. An error of Kind Unavailable means keep
// serving and call again later. Nothing here touches the data path.
type Syncer interface {
	// Reconcile makes the owned VPN Clients match wants and reports each slot.
	// Commit every SlotResult.NetworkID durably before raising the slot's gate.
	Reconcile(ctx context.Context, wants []Want) (Report, error)
	// Inspect reads without writing, and without wants it can only go by the marker.
	// Use it to report on an uninstall. A pass reads Report.Inventory instead.
	Inspect(ctx context.Context) (Inventory, error)
}

var _ Syncer = (*Client)(nil)

// Want is one slot as UniFi must show it.
type Want struct {
	Slot domain.SlotID
	Name string
	// Bound is the committed _id, empty until first bound. A bound client that
	// disappeared is reported, not re-created, because the owner's Policy Table
	// routes went with it and a new _id wouldn't restore them.
	Bound         domain.NetworkID
	Endpoint      netip.AddrPort // The slot's pxaN, on 127.0.0.1.
	Address       netip.Prefix   // The client's own tunnel address.
	DNS           netip.Addr     // The app refuses a WireGuard client without one.
	PeerPublicKey string         // The host side of the slot's keys.
	PublicKey     string         // The client side.
	PrivateKey    domain.Secret
	// Enable turns a client the owner turned off in UniFi back on. It is set only on the owner's ask, so
	// the daemon never undoes a choice made in UniFi.
	Enable bool
}

type Action uint8

const (
	Unchanged Action = iota
	Created
	// Adopted means an existing owned client now serves this slot under a new
	// binding: a reply lost after a POST, or a reinstall that lost /data.
	Adopted
	Updated
)

type ProblemKind uint8

const (
	// LimitReached means no client was created, because Limit are in use.
	LimitReached ProblemKind = iota + 1
	// NameCollision means another network holds the name, so the slot keeps its
	// old name or has no client. The app itself accepts duplicates.
	NameCollision
	// ExternallyDeleted means a bound client is gone or no longer ours. Clear
	// Bound to create a new one; the owner must then re-point any routes.
	ExternallyDeleted
	// RoutedElsewhere means a slot was deleted while routes still target its client.
	RoutedElsewhere
	// RefusedByApp means the app refused the write. See Err.
	RefusedByApp
	// NotReached means the pass ended before this step, for the reason in Err.
	NotReached
)

type Problem struct {
	Kind   ProblemKind
	Detail string
	Routes []Route
	Err    error
}

func (p *Problem) Error() string {
	if p.Err != nil {
		return p.Detail + ": " + p.Err.Error()
	}
	return p.Detail
}

// SlotResult has one entry per Want, in order. NetworkID is empty when the slot
// has no client. A Problem can sit beside a NetworkID: a slot that can't take a
// new name still has its client, and its routes still work.
type SlotResult struct {
	Slot      domain.SlotID
	NetworkID domain.NetworkID
	Action    Action
	Problem   *Problem
}

// Retained is an owned client no want names, that this pass didn't delete.
type Retained struct {
	NetworkID domain.NetworkID
	Slot      domain.SlotID
	Name      string
	Problem   *Problem
}

type Report struct {
	Slots    []SlotResult
	Removed  []domain.NetworkID
	Retained []Retained
	// Inventory is what UniFi holds after the pass. It is empty if the pass ended early.
	Inventory Inventory
}

// Owned reports one VPN Client of ours. Enabled is nil when the app doesn't say.
type Owned struct {
	NetworkID domain.NetworkID
	Slot      domain.SlotID
	Name      string
	Enabled   *bool
	Routes    []Route
}

// ForeignClient is a VPN Client that isn't ours.
type ForeignClient struct {
	ID      domain.NetworkID
	Name    string
	Enabled bool
	Routes  []Route
}

type Inventory struct {
	Owned   []Owned
	Foreign []ForeignClient
	Clients int // Every VPN Client, ours and the owner's.
}

// Routed says whether a route targets client id. A pass lists every VPN Client UniFi holds, ours and
// the owner's, each with the routes to it, so a client it lists nowhere is gone and no route reaches it.
func (inv Inventory) Routed(id domain.NetworkID) bool {
	for _, o := range inv.Owned {
		if o.NetworkID == id && len(o.Routes) > 0 {
			return true
		}
	}
	for _, f := range inv.Foreign {
		if f.ID == id && len(f.Routes) > 0 {
			return true
		}
	}
	return false
}

// The marker is how a client stays ours across a rename, a daemon that lost /data,
// and a UI save. The app drops unknown fields, and keeps this one in manual mode,
// where nothing reads it. The UI omits it on save, which the app takes as no change.
const (
	markerPrefix = "sidegate-"
	markerSuffix = ".conf"
)

func markerOf(slot domain.SlotID) string { return markerPrefix + string(slot) + markerSuffix }

func (n network) markedSlot() (domain.SlotID, bool) {
	// In file mode this field is the name of a file the owner uploaded.
	if n.Purpose != purposeVPNClient || n.VPNType != "wireguard-client" || n.Mode != "manual" {
		return "", false
	}
	// A filename alone could be the owner's. No client of theirs points at this gateway's loopback.
	if n.PeerIP != "127.0.0.1" {
		return "", false
	}
	rest, ok := strings.CutPrefix(n.Filename, markerPrefix)
	if !ok {
		return "", false
	}
	slot, ok := strings.CutSuffix(rest, markerSuffix)
	if !ok || slot == "" {
		return "", false
	}
	return domain.SlotID(slot), true
}

func (n network) foreign() ForeignClient {
	return ForeignClient{ID: domain.NetworkID(n.ID), Name: n.Name, Enabled: n.Enabled == nil || *n.Enabled}
}

// slotPlan is the decision for one want. A nil client with no problem is a create.
type slotPlan struct {
	want    Want
	client  *network
	problem *Problem
}

type plan struct {
	slots   []slotPlan
	orphans []network // Ours, and no want claims them.
	foreign []network // VPN Clients that aren't ours and that no want claims.
	clients int
}

// decide is the whole ownership policy. It reads only what the app returned.
func decide(nets []network, wants []Want) plan {
	var p plan
	claimed := map[string]bool{}
	byID := map[string]*network{}
	for i := range nets {
		byID[nets[i].ID] = &nets[i]
	}
	marked := func(slot domain.SlotID) *network {
		var found *network
		for i := range nets {
			if s, ok := nets[i].markedSlot(); ok && s == slot && !claimed[nets[i].ID] {
				if found == nil || nets[i].ID < found.ID {
					found = &nets[i]
				}
			}
		}
		return found
	}

	// A bound client claims first, so a later marker match can't take it.
	matches := make([]*network, len(wants))
	lost := make([]bool, len(wants))
	for i, w := range wants {
		if w.Bound == "" {
			continue
		}
		n, ok := byID[string(w.Bound)]
		if ok {
			s, hasMarker := n.markedSlot()
			if (hasMarker && s == w.Slot) || (n.Purpose == purposeVPNClient && n.PeerPublicKey == w.PeerPublicKey) {
				matches[i] = n
				claimed[n.ID] = true
				continue
			}
		}
		lost[i] = true
	}
	for i, w := range wants {
		if matches[i] != nil {
			continue
		}
		if n := marked(w.Slot); n != nil {
			matches[i] = n
			claimed[n.ID] = true
			lost[i] = false
		}
	}
	// A reinstall has new slot IDs. An owned client under the same name keeps its
	// _id, and so the owner's routes, by being adopted instead of re-created.
	wanted := map[domain.SlotID]bool{}
	for _, w := range wants {
		wanted[w.Slot] = true
	}
	for i, w := range wants {
		if matches[i] != nil || lost[i] {
			continue
		}
		for j := range nets {
			s, ok := nets[j].markedSlot()
			if ok && !wanted[s] && !claimed[nets[j].ID] && strings.EqualFold(nets[j].Name, w.Name) {
				matches[i] = &nets[j]
				claimed[nets[j].ID] = true
				break
			}
		}
	}

	taken := map[string]bool{}
	for _, n := range nets {
		_, ours := n.markedSlot()
		if !ours && !claimed[n.ID] {
			taken[strings.ToLower(n.Name)] = true
		}
		if n.Purpose != purposeVPNClient {
			continue
		}
		p.clients++
		if !ours && !claimed[n.ID] {
			p.foreign = append(p.foreign, n)
		}
		if ours && !claimed[n.ID] {
			p.orphans = append(p.orphans, n)
		}
	}
	for i, w := range wants {
		sp := slotPlan{want: w, client: matches[i]}
		switch {
		case lost[i]:
			sp.problem = &Problem{Kind: ExternallyDeleted, Detail: fmt.Sprintf("VPN Client %s of slot %q is gone or no longer ours", w.Bound, w.Name)}
		case taken[strings.ToLower(w.Name)]:
			sp.problem = &Problem{Kind: NameCollision, Detail: fmt.Sprintf("another network is already named %q", w.Name)}
		}
		taken[strings.ToLower(w.Name)] = true
		p.slots = append(p.slots, sp)
	}
	return p
}

// patch lists the fields that differ, so a write leaves all others as they are.
func patch(n network, w Want, nameFree bool) map[string]any {
	fields := map[string]any{}
	set := func(field string, differs bool, value any) {
		if differs {
			fields[field] = value
		}
	}
	set("name", nameFree && n.Name != w.Name, w.Name)
	set("wireguard_client_mode", n.Mode != "manual", "manual")
	set("wireguard_client_peer_ip", n.PeerIP != w.Endpoint.Addr().String(), w.Endpoint.Addr().String())
	set("wireguard_client_peer_port", n.PeerPort != int(w.Endpoint.Port()), int(w.Endpoint.Port()))
	set("wireguard_client_peer_public_key", n.PeerPublicKey != w.PeerPublicKey, w.PeerPublicKey)
	set("wireguard_public_key", n.PublicKey != w.PublicKey, w.PublicKey)
	set("x_wireguard_private_key", string(n.PrivateKey) != w.PrivateKey.Reveal(), w.PrivateKey.Reveal())
	set("ip_subnet", n.Subnet != w.Address.String(), w.Address.String())
	set("dhcpd_dns_1", n.DNS != w.DNS.String(), w.DNS.String())
	set("wireguard_client_configuration_filename", n.Filename != markerOf(w.Slot), markerOf(w.Slot))
	set("enabled", w.Enable && n.Enabled != nil && !*n.Enabled, true)
	return fields
}

// creation is what the UI's manual form sends, with the marker added.
func creation(w Want) map[string]any {
	fields := patch(network{}, w, true)
	fields["purpose"] = purposeVPNClient
	fields["vpn_type"] = "wireguard-client"
	fields["enabled"] = true
	fields["wireguard_client_preshared_key_enabled"] = false
	fields["interface_mtu_enabled"] = false
	fields["mss_clamp"] = "auto"
	fields["mss_clamp_ipv6"] = "auto"
	return fields
}

func (c *Client) Inspect(ctx context.Context) (Inventory, error) {
	nets, err := c.networks(ctx)
	if err != nil {
		return Inventory{}, err
	}
	var inv Inventory
	for _, n := range nets {
		if n.Purpose != purposeVPNClient {
			continue
		}
		inv.Clients++
		s, ours := n.markedSlot()
		if !ours {
			inv.Foreign = append(inv.Foreign, n.foreign())
			continue
		}
		inv.Owned = append(inv.Owned, Owned{NetworkID: domain.NetworkID(n.ID), Slot: s, Name: n.Name, Enabled: n.Enabled})
	}
	return inv, c.readRoutes(ctx, &inv)
}

// readRoutes fills in the routes of every client inv lists, the owner's too, since a slot whose client
// the owner made theirs still can't go while a route targets it.
func (c *Client) readRoutes(ctx context.Context, inv *Inventory) error {
	targets := map[string]bool{}
	for _, o := range inv.Owned {
		targets[string(o.NetworkID)] = true
	}
	for _, f := range inv.Foreign {
		targets[string(f.ID)] = true
	}
	routes, err := c.routes(ctx, targets)
	if err != nil {
		return err
	}
	for _, r := range routes {
		for i := range inv.Owned {
			if r.Target == inv.Owned[i].NetworkID {
				inv.Owned[i].Routes = append(inv.Owned[i].Routes, r)
			}
		}
		for i := range inv.Foreign {
			if r.Target == inv.Foreign[i].ID {
				inv.Foreign[i].Routes = append(inv.Foreign[i].Routes, r)
			}
		}
	}
	return nil
}

func (c *Client) Reconcile(ctx context.Context, wants []Want) (Report, error) {
	nets, err := c.networks(ctx)
	if err != nil {
		return Report{}, err
	}
	p := decide(nets, wants)
	var report Report
	inv := Inventory{Clients: p.clients}
	for _, n := range p.foreign {
		inv.Foreign = append(inv.Foreign, n.foreign())
	}
	var halted error

	// Deletes run first, because a freed client is capacity for this pass's creates.
	for _, n := range p.orphans {
		slot, _ := n.markedSlot()
		retained := Retained{NetworkID: domain.NetworkID(n.ID), Slot: slot, Name: n.Name}
		if halted != nil {
			retained.Problem = &Problem{Kind: NotReached, Detail: "not checked", Err: halted}
			report.Retained = append(report.Retained, retained)
			continue
		}
		problem, err := c.removeOwned(ctx, n)
		halted = fatal(err)
		if problem != nil {
			retained.Problem = problem
			report.Retained = append(report.Retained, retained)
			inv.Owned = append(inv.Owned, Owned{NetworkID: retained.NetworkID, Slot: slot, Name: n.Name, Enabled: n.Enabled})
			continue
		}
		report.Removed = append(report.Removed, retained.NetworkID)
		inv.Clients--
	}

	// Updates run before creates, in case an update fails and the pass ends with
	// nothing new. Results stay in want order.
	report.Slots = make([]SlotResult, len(p.slots))
	for i, sp := range p.slots {
		if sp.client == nil {
			continue
		}
		result := SlotResult{Slot: sp.want.Slot, NetworkID: domain.NetworkID(sp.client.ID), Problem: sp.problem}
		fields := patch(*sp.client, sp.want, sp.problem == nil)
		name, enabled := sp.client.Name, sp.client.Enabled
		if sp.want.Bound != result.NetworkID {
			result.Action = Adopted
		}
		switch {
		case len(fields) == 0:
		case halted != nil:
			result.Problem = &Problem{Kind: NotReached, Detail: "not updated", Err: halted}
		default:
			if err := c.updateNetwork(ctx, sp.client.ID, fields); err != nil {
				halted = fatal(err)
				result.Problem = writeProblem("update refused", err)
				break
			}
			if result.Action == Unchanged {
				result.Action = Updated
			}
			if _, renamed := fields["name"]; renamed {
				name = sp.want.Name
			}
			if _, on := fields["enabled"]; on {
				enabled = &on
			}
		}
		report.Slots[i] = result
		inv.Owned = append(inv.Owned, Owned{NetworkID: result.NetworkID, Slot: sp.want.Slot, Name: name, Enabled: enabled})
	}
	for i, sp := range p.slots {
		if sp.client != nil {
			continue
		}
		result := SlotResult{Slot: sp.want.Slot, Problem: sp.problem}
		switch {
		case sp.problem != nil:
		case halted != nil:
			result.Problem = &Problem{Kind: NotReached, Detail: "not created", Err: halted}
		case inv.Clients >= Limit:
			result.Problem = &Problem{Kind: LimitReached, Detail: fmt.Sprintf("%d of %d VPN Clients are in use, %d of them the owner's", inv.Clients, Limit, len(inv.Foreign))}
		default:
			id, err := c.createNetwork(ctx, creation(sp.want))
			if err != nil {
				halted = fatal(err)
				result.Problem = writeProblem("create refused", err)
			} else {
				result.NetworkID, result.Action = id, Created
				inv.Clients++
				enabled := true
				inv.Owned = append(inv.Owned, Owned{NetworkID: id, Slot: sp.want.Slot, Name: sp.want.Name, Enabled: &enabled})
			}
		}
		report.Slots[i] = result
	}
	if halted != nil {
		return report, halted
	}
	if err := c.readRoutes(ctx, &inv); err != nil {
		return report, err
	}
	report.Inventory = inv
	return report, nil
}

// removeOwned reads the routes right before the delete, because the app answers
// ok to deleting a routed client and silently deletes the owner's route with it.
func (c *Client) removeOwned(ctx context.Context, n network) (*Problem, error) {
	routes, err := c.routes(ctx, map[string]bool{n.ID: true})
	if err != nil {
		return &Problem{Kind: NotReached, Detail: "could not prove that no route targets it, so it stays", Err: err}, err
	}
	if len(routes) > 0 {
		return &Problem{Kind: RoutedElsewhere, Detail: fmt.Sprintf("%d Policy Table routes still target %q; remove them in UniFi first", len(routes), n.Name), Routes: routes}, nil
	}
	err = c.deleteNetwork(ctx, n.ID)
	var e *Error
	if errors.As(err, &e) && e.Code == "api.err.IdInvalid" {
		return nil, nil // Already gone, which is what this was for.
	}
	if err != nil {
		return writeProblem("delete refused", err), err
	}
	return nil, nil
}

func writeProblem(detail string, err error) *Problem {
	if fatal(err) != nil {
		return &Problem{Kind: NotReached, Detail: detail, Err: err}
	}
	return &Problem{Kind: RefusedByApp, Detail: detail, Err: err}
}

// fatal keeps an error that ends the pass. The app refusing one write doesn't.
func fatal(err error) error {
	if err == nil || isKind(err, Rejected) {
		return nil
	}
	return err
}

// String keeps a print of the Client from showing the key.
func (c *Client) String() string { return "unifi.Client{" + c.base + "}" }
