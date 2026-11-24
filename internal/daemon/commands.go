package daemon

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

// Command is one owner write. It knows its idempotency key, what makes it the same request again,
// how it stages itself, and what it answers from that stage.
type Command interface {
	request() domain.RequestID
	digest() string
	perform(c *controller, s *stage) error
	answer(c *controller, s *stage) (int, any)
}

// PutSlot creates a slot when Expected is zero, and replaces one otherwise.
type PutSlot struct {
	Request  domain.RequestID
	Expected domain.IntentRevision
	ID       domain.SlotID
	Intent   domain.SlotIntent
}

type DeleteSlot struct {
	Request  domain.RequestID
	Expected domain.IntentRevision
	ID       domain.SlotID
}

// SourceInput is one owner write of a source, with exactly one of Remote or Manual.
type SourceInput struct {
	Name   string
	Remote *RemoteInput
	Manual *ManualInput
}

type RemoteInput struct {
	URL          domain.Secret // Empty keeps the stored one.
	UserAgent    string        // Empty is catalogue.DefaultUserAgent.
	RefreshEvery time.Duration // Zero follows the provider hint.
	Paused       bool
}

// ManualInput carries pasted text with the document the handler parsed from it, so a
// large paste can't stall the controller. A nil Document keeps the stored text.
type ManualInput struct {
	Text     domain.Secret
	Document *catalogue.Document
}

// PutSource creates a source when Expected is zero, and replaces one otherwise.
type PutSource struct {
	Request  domain.RequestID
	Expected domain.IntentRevision
	ID       domain.SourceID
	Input    SourceInput
}

type DeleteSource struct {
	Request  domain.RequestID
	Expected domain.IntentRevision
	ID       domain.SourceID
}

// RefreshSource fetches a source now, first storing UserAgent on it when that is set.
type RefreshSource struct {
	Request   domain.RequestID
	ID        domain.SourceID
	UserAgent string
}

// PutGroup creates a group when Expected is zero, and replaces one otherwise. Ticked is a create's,
// each server once: a replace keeps the ticks stored, which change only by TickServer.
type PutGroup struct {
	Request  domain.RequestID
	Expected domain.IntentRevision
	ID       domain.GroupID
	Name     string
	Members  domain.Members
	Ticked   []domain.LineageID
}

// TickServer ticks Server in Group, or unticks it.
type TickServer struct {
	Request domain.RequestID
	Group   domain.GroupID
	Server  domain.LineageID
	Ticked  bool
}

type DeleteGroup struct {
	Request  domain.RequestID
	Expected domain.IntentRevision
	ID       domain.GroupID
}

// EnableClient turns back on a slot's VPN Client the owner turned off in UniFi.
type EnableClient struct {
	Request domain.RequestID
	ID      domain.SlotID
}

// RecreateClient gives a slot whose VPN Client was deleted in UniFi a new one. The owner points its
// routes at the new one, since they went with the old.
type RecreateClient struct {
	Request domain.RequestID
	ID      domain.SlotID
}

// PutSettings replaces the retention. It keeps CheckForUpdates, which changes only by
// PutCheckForUpdates, so the box and the Retention form never write over each other.
type PutSettings struct {
	Request       domain.RequestID
	Expected      uint64
	RetentionDays int
}

// PutCheckForUpdates turns the page's check for a newer release on or off, at once and with no
// precondition, as a tick is.
type PutCheckForUpdates struct {
	Request domain.RequestID
	On      bool
}

func (cmd PutSlot) request() domain.RequestID       { return cmd.Request }
func (cmd DeleteSlot) request() domain.RequestID    { return cmd.Request }
func (cmd PutSource) request() domain.RequestID     { return cmd.Request }
func (cmd DeleteSource) request() domain.RequestID  { return cmd.Request }
func (cmd RefreshSource) request() domain.RequestID { return cmd.Request }
func (cmd PutGroup) request() domain.RequestID      { return cmd.Request }
func (cmd DeleteGroup) request() domain.RequestID   { return cmd.Request }
func (cmd TickServer) request() domain.RequestID    { return cmd.Request }
func (cmd PutSettings) request() domain.RequestID   { return cmd.Request }
func (cmd EnableClient) request() domain.RequestID  { return cmd.Request }
func (cmd RecreateClient) request() domain.RequestID {
	return cmd.Request
}
func (cmd PutCheckForUpdates) request() domain.RequestID { return cmd.Request }

func (cmd PutSlot) digest() string {
	wire, _ := encodeIntent(cmd.Intent)
	return fmt.Sprintf("slot %s %d %s", cmd.ID, cmd.Expected, wire)
}
func (cmd DeleteSlot) digest() string   { return fmt.Sprintf("delslot %s %d", cmd.ID, cmd.Expected) }
func (cmd DeleteSource) digest() string { return fmt.Sprintf("delsub %s %d", cmd.ID, cmd.Expected) }
func (cmd RefreshSource) digest() string {
	return fmt.Sprintf("refresh %s %q", cmd.ID, cmd.UserAgent)
}
func (cmd PutGroup) digest() string {
	wire, _ := encodeGroup(domain.Group{Name: cmd.Name, Members: cmd.Members, Ticked: cmd.Ticked})
	return fmt.Sprintf("group %s %d %s", cmd.ID, cmd.Expected, wire)
}
func (cmd DeleteGroup) digest() string { return fmt.Sprintf("delgroup %s %d", cmd.ID, cmd.Expected) }
func (cmd TickServer) digest() string {
	return fmt.Sprintf("tick %s %s %t", cmd.Group, cmd.Server, cmd.Ticked)
}
func (cmd EnableClient) digest() string   { return fmt.Sprintf("enable %s", cmd.ID) }
func (cmd RecreateClient) digest() string { return fmt.Sprintf("recreate %s", cmd.ID) }
func (cmd PutSettings) digest() string {
	return fmt.Sprintf("settings %d %d", cmd.Expected, cmd.RetentionDays)
}
func (cmd PutCheckForUpdates) digest() string { return fmt.Sprintf("checkforupdates %t", cmd.On) }

func answered() (int, any) { return http.StatusOK, struct{}{} }

func (cmd DeleteSlot) answer(*controller, *stage) (int, any)   { return answered() }
func (cmd DeleteSource) answer(*controller, *stage) (int, any) { return answered() }
func (cmd DeleteGroup) answer(*controller, *stage) (int, any)  { return answered() }
func (cmd RefreshSource) answer(*controller, *stage) (int, any) {
	return http.StatusAccepted, struct{}{}
}
func (cmd EnableClient) answer(*controller, *stage) (int, any) {
	return http.StatusAccepted, struct{}{}
}
func (cmd RecreateClient) answer(*controller, *stage) (int, any) {
	return http.StatusAccepted, struct{}{}
}

func (cmd PutSlot) answer(c *controller, s *stage) (int, any) {
	row := s.tx.Slots[0]
	var report gateway.SlotReport
	for _, r := range c.last.Slots {
		if r.Slot == row.Slot.ID {
			report = r
		}
	}
	memory := c.memoryOf(row)
	o, _ := ownerOf(row)
	if staged, ok := s.tx.Owners[o]; ok {
		memory = staged
	}
	return http.StatusOK, c.slotView(row, memory, report, s.now, s.next)
}

func (cmd PutSource) answer(c *controller, s *stage) (int, any) {
	source := s.tx.Sources[0]
	fs := c.state.fetches[source.ID]
	var diff *refreshEvent
	if d, ok := c.diffs[source.ID]; ok {
		diff = &d
	}
	if len(s.tx.RefreshEvents) > 0 {
		diff = &s.tx.RefreshEvents[0]
	}
	sources := maps.Clone(c.state.sources)
	sources[source.ID] = source
	return http.StatusOK, sourceView(source, sourceNames(sources, c.state.fetches)[source.ID], fs, s.next, c.due(source, fs), diff, c.facts(domain.SourceOwner(source.ID), domain.SourceExit(source.ID), s.next, s.now, c.slotViews(s.now, s.next)))
}

func (cmd PutGroup) answer(c *controller, s *stage) (int, any) {
	return http.StatusOK, groupView(s.next.groups[cmd.ID], s.next, c.facts(domain.GroupOwner(cmd.ID), domain.GroupExit(cmd.ID), s.next, s.now, c.slotViews(s.now, s.next)))
}

func (cmd TickServer) answer(c *controller, s *stage) (int, any) {
	return http.StatusOK, groupView(s.next.groups[cmd.Group], s.next, c.facts(domain.GroupOwner(cmd.Group), domain.GroupExit(cmd.Group), s.next, s.now, c.slotViews(s.now, s.next)))
}

func (cmd PutSettings) answer(_ *controller, s *stage) (int, any) { return settingsAnswer(s) }
func (cmd PutCheckForUpdates) answer(_ *controller, s *stage) (int, any) {
	return settingsAnswer(s)
}

func settingsAnswer(s *stage) (int, any) {
	return http.StatusOK, SettingsView{Settings: *s.tx.Settings, Revision: s.tx.Meta["settings_revision"]}
}

func (cmd PutSlot) perform(c *controller, s *stage) error {
	if err := validateSlotID(cmd.ID); err != nil {
		return err
	}
	row, exists := c.state.slots[cmd.ID]
	switch {
	case exists && cmd.Expected == 0 && !row.Deleting:
		return stale(uint64(row.Slot.Version))
	case !exists && cmd.Expected != 0, exists && row.Deleting && cmd.Expected != 0:
		return refuse(KindNotFound, RefusedSlotGone, "That slot is gone.")
	case exists && !row.Deleting && row.Slot.Version != cmd.Expected:
		return stale(uint64(row.Slot.Version))
	}
	live := exists && !row.Deleting
	before := row
	if !live || row.Slot.Intent.Exit() != cmd.Intent.Exit() {
		if err := admitExit(cmd.Intent.Exit(), c.world, c.state.sources); err != nil {
			return err
		}
	}
	next := domain.IntentRevision(c.state.retired[retiredSlot(cmd.ID)]) + 1
	switch {
	case exists && row.Deleting:
		// A delete frees its id at once for the page, while the runtime and UniFi may
		// still be taking it down.
		next = row.Slot.Version + 1
		row = row.remade()
	case exists:
		next = row.Slot.Version + 1
	default:
		var taken []domain.Allocation
		for _, other := range c.state.slots {
			taken = append(taken, other.Slot.Allocation)
		}
		if c.clientsKnown && len(c.foreign)+len(c.state.slots) >= unifi.Limit {
			return refuse(KindCapacity, RefusedClientsFull, "Every VPN Client UniFi allows is in use.")
		}
		keys, secrets, err := c.newKeys()
		if err != nil {
			return err
		}
		allocation, err := domain.Allocate(taken, keys)
		if errors.Is(err, domain.ErrNoAllocation) {
			return refuse(KindCapacity, RefusedSlotsFull, "Every slot is taken.")
		}
		if err != nil {
			return err
		}
		row = slotRow{Slot: domain.Slot{ID: cmd.ID, Allocation: allocation}}
		s.tx.Secrets = secrets
	}
	if !live {
		row.CreatedAt = s.now
	}
	exitMoved := !live || row.Slot.Intent.Exit() != cmd.Intent.Exit()
	row.Slot.Intent, row.Slot.Version = cmd.Intent, next
	c.rehome(s, before, live, row)
	s.tx.Slots = append(s.tx.Slots, row)
	if exitMoved {
		rows := maps.Clone(c.state.slots)
		rows[cmd.ID] = row
		s.next = s.next.with(worldChange{uses: usesOf(rows)})
	}
	secrets := s.tx.Secrets
	s.then(func() {
		c.state.slots[cmd.ID] = row
		for ref, secret := range secrets {
			c.state.secrets[ref] = secret
		}
	})
	return nil
}

// choiceChanged is what a new Exit does to an owner's memory: any improvement, preference, pick or last
// word was for what was chosen before, by the owner that chose it, and a finished refill was the old
// pool's. A failure campaign stays, since a failure is one under any Exit.
func (m policyMemory) choiceChanged(now time.Time) policyMemory {
	if m.Campaign.on(improvePath) {
		m.Campaign = nil
	}
	m.Hand, m.RefilledAt = m.Hand.step(now, choseAnew{}), time.Time{}
	return m
}

// admitExit refuses what the owner can't newly point a slot at. A server is valid until it is a
// tombstone, whatever group holds it. A group or source with no server it would use is refused, since
// a slot on it could only close.
func admitExit(exit domain.Exit, w world, sources map[domain.SourceID]domain.Source) error {
	if id, ok := exit.Group(); ok {
		if _, known := w.groups[id]; !known {
			return refuse(KindValidation, RefusedGroupGone, "That group is gone.")
		}
		if len(w.members[id]) == 0 {
			return refuse(KindValidation, RefusedGroupEmpty, "That group has no servers. Change its filter first.")
		}
	}
	if id, ok := exit.Source(); ok {
		if _, known := sources[id]; !known {
			return refuse(KindValidation, RefusedSourceGone, "That source is gone.")
		}
		if len(w.sourceMembers(id)) == 0 {
			return refuse(KindValidation, RefusedEmpty, "That source has no servers now.")
		}
	}
	if id, ok := exit.Server(); ok {
		if l, known := w.catalogue.Lookup(id); !known || l.Presence == domain.Tombstone {
			return refuse(KindValidation, RefusedServerGone, "That server is gone.")
		}
	}
	return nil
}

func validateSlotID(id domain.SlotID) error { return validateID(string(id), "slot") }

func validateID(id, noun string) error {
	if len(id) == 0 || len(id) > 32 {
		return refuse(KindValidation, RefusedBadID, fmt.Sprintf("A %s ID is 1 to 32 characters.", noun))
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return refuse(KindValidation, RefusedBadID, fmt.Sprintf("A %s ID is letters, digits, dashes and underscores.", noun))
		}
	}
	return nil
}

func (cmd DeleteSlot) perform(c *controller, s *stage) error {
	row, exists := c.state.slots[cmd.ID]
	switch {
	case !exists || row.Deleting:
		return refuse(KindNotFound, RefusedSlotGone, "That slot is gone.")
	case cmd.Expected != 0 && row.Slot.Version != cmd.Expected:
		return stale(uint64(row.Slot.Version))
	case len(c.owned[cmd.ID].Routes) > 0,
		c.key.syncer != nil && row.Slot.Allocation.NetworkID != "" && !row.Slot.Allocation.Unrouted:
		return refuse(KindReferenced, RefusedRouted, "UniFi routes still point at this slot. Remove them in UniFi first.")
	}
	before := row
	row = row.deleted()
	c.rehome(s, before, true, row)
	s.tx.Slots = append(s.tx.Slots, row)
	rows := maps.Clone(c.state.slots)
	rows[cmd.ID] = row
	s.next = s.next.with(worldChange{uses: usesOf(rows)})
	s.then(func() { c.state.slots[cmd.ID] = row })
	return nil
}

// clientOf finds a live slot whose client is in the state a command needs.
func (c *controller) clientOf(id domain.SlotID, want ClientState, reason Reason, refusal string) (slotRow, error) {
	row, ok := c.state.slots[id]
	switch {
	case !ok || row.Deleting:
		return row, refuse(KindNotFound, RefusedSlotGone, "That slot is gone.")
	case c.clientState(row) != want:
		return row, refuse(KindValidation, reason, refusal)
	}
	return row, nil
}

func (cmd EnableClient) perform(c *controller, s *stage) error {
	if _, err := c.clientOf(cmd.ID, ClientDisabled, RefusedNotDisabled, "This slot's VPN Client isn't turned off in UniFi."); err != nil {
		return err
	}
	s.tx.Meta[enablingMeta+string(cmd.ID)] = 1
	s.then(func() { c.state.enabling[cmd.ID] = true })
	return nil
}

func (cmd RecreateClient) perform(c *controller, s *stage) error {
	row, err := c.clientOf(cmd.ID, ClientDeleted, RefusedClientExists, "This slot's VPN Client is still in UniFi.")
	if err != nil {
		return err
	}
	row, _ = row.withClient("", false)
	s.tx.Slots = append(s.tx.Slots, row)
	s.then(func() { c.state.slots[cmd.ID] = row })
	return nil
}

func (cmd PutSettings) perform(c *controller, s *stage) error {
	if cmd.Expected != c.state.settingsRevision {
		return stale(c.state.settingsRevision)
	}
	settings := c.state.settings
	settings.AnalyticsRetentionDays = cmd.RetentionDays
	if err := settings.validate(); err != nil {
		return err
	}
	c.storeSettings(s, settings)
	return nil
}

func (cmd PutCheckForUpdates) perform(c *controller, s *stage) error {
	settings := c.state.settings
	settings.CheckForUpdates = cmd.On
	c.storeSettings(s, settings)
	return nil
}

// storeSettings stages settings at the next settings revision. Every write moves it, one that
// changed nothing included, so the page settles a write on a read at or past its answer.
func (c *controller) storeSettings(s *stage, settings Settings) {
	next := c.state.settingsRevision + 1
	s.tx.Settings = &settings
	s.tx.Meta["settings_revision"] = next
	s.then(func() {
		c.state.settings, c.state.settingsRevision = settings, next
		c.app.writer.SetRollupRetention(time.Duration(settings.AnalyticsRetentionDays) * 24 * time.Hour)
	})
}

// refuseReferenced is one 409 for groups and sources alike, naming the slots by what the owner
// calls them.
func (c *controller) refuseReferenced(what string, slots []domain.SlotID) error {
	names := make([]string, 0, len(slots))
	for _, id := range slots {
		names = append(names, c.state.slots[id].Slot.Intent.Name())
	}
	return refuse(KindReferenced, RefusedInUse, fmt.Sprintf("%s is used by %s. Change their Exit first.", what, strings.Join(names, ", ")))
}
