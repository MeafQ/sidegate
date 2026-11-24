package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"sidegate/internal/domain"
	"sidegate/internal/unifi"
)

const (
	clientPassEvery = time.Minute
	// clientRetryAfter is how soon a pass UniFi didn't answer runs again. UniFi can be busy for seconds
	// with a client the daemon has just made, and one such pass is not shown.
	clientRetryAfter = 5 * time.Second
)

// openSyncer builds the UniFi client from bootstrap. With no credential file there is
// none, and the page says so.
func openSyncer(b Bootstrap) (unifi.Syncer, error) {
	if b.NetworkCredentialFile == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(b.NetworkCredentialFile)
	if err != nil {
		return nil, errors.New("the Network API key file can't be read")
	}
	client, err := unifi.New(unifi.Config{BaseURL: b.NetworkEndpoint, Site: b.NetworkSite, APIKey: domain.NewSecret(strings.TrimSpace(string(raw)))})
	if err != nil {
		return nil, err
	}
	return client, nil
}

// startSyncer puts the accepted key in place and opens its syncer. A start comes up whatever state the
// key's files are in: when either step fails the daemon runs with no syncer, so the page says it has no
// key and a key pasted in Settings brings UniFi back. Nothing in the way is deleted, since the daemon
// didn't put it there. The accepted key waits beside it for a start once it's gone, and a save replaces it.
func startSyncer(b *Bootstrap, accepted uint64) unifi.Syncer {
	if err := settleKey(b, accepted); err != nil {
		log.Printf("unifi key: this start runs with no key, since the accepted one can't be put in place: %v", err)
		return nil
	}
	syncer, err := openSyncer(*b)
	if err != nil {
		log.Printf("unifi key: this start runs with no key: %v", err)
		return nil
	}
	return syncer
}

// unifiKey is the key the controller reaches UniFi with and what UniFi last said of it: no key, a key
// no pass has answered since the start, a key whose last pass or save worked, or a key UniFi refused or
// didn't answer. Once Run starts, only the controller holds it.
type unifiKey struct {
	syncer   unifi.Syncer // Nil: no key.
	trouble  UniFiTrouble // Empty once a pass, or the try before a save, worked.
	answered time.Time    // When UniFi last answered the key: zero since the start until it does.
}

// saved is a key the page's handler just found UniFi answer.
func (k *unifiKey) saved(syncer unifi.Syncer, at time.Time) {
	*k = unifiKey{syncer: syncer, answered: at}
}

// passed is one pass over UniFi with the key, which failed if err is set.
func (k *unifiKey) passed(err error, at time.Time) {
	switch {
	case err == nil:
		k.trouble, k.answered = "", at
	case refused(err):
		k.trouble = unifiKeyRefused
	default:
		k.trouble = unifiNotAnswering
	}
}

// refused is a pass UniFi answered with a refusal of the key. Any other failure is UniFi's silence.
func refused(err error) bool {
	var failure *unifi.Error
	return errors.As(err, &failure) && failure.Kind == unifi.Unauthorized
}

// shown is the trouble the page names, nil while the key works or no pass has said otherwise.
func (k unifiKey) shown() *UniFiTrouble {
	switch {
	case k.syncer == nil:
		return new(unifiNoKey)
	case k.trouble != "":
		return new(k.trouble)
	}
	return nil
}

// clientReport is one pass over UniFi. An unavailable app leaves Err set and the rest
// empty: nothing is known, so nothing that was known changes. From is the syncer that made
// it, so a pass with a key the owner has since replaced changes nothing.
type clientReport struct {
	Report unifi.Report
	Err    error
	From   unifi.Syncer
}

// runClients is the one actor that calls UniFi, since its requests take seconds and the
// controller must not wait on them. A new set of wants starts a pass at once, and so does a
// new key's syncer. A pass also runs a minute after the last, to notice a route or a client the
// owner changed. It reads from the start, and until the controller has wanted something it only
// reads, so a daemon with no slots still knows what UniFi holds and changes none of it.
// With no syncer it waits for one. A pass UniFi didn't answer, after one it did or a refusal, isn't
// reported: it is logged and runs again within seconds, so the controller hears of UniFi's silence
// from its second pass in a row. after is the clock it waits on.
func runClients(ctx context.Context, syncer unifi.Syncer, syncers <-chan unifi.Syncer, wants <-chan []unifi.Want, out chan<- clientReport, after func(time.Duration) <-chan time.Time) error {
	var current []unifi.Want
	ready := false
	silent := 0 // Passes in a row with this syncer that UniFi didn't answer.
	for {
		wait := clientPassEvery
		if syncer != nil {
			report := clientReport{From: syncer}
			if ready {
				report.Report, report.Err = syncer.Reconcile(ctx, current)
			} else {
				report.Report.Inventory, report.Err = syncer.Inspect(ctx)
			}
			if report.Err == nil || refused(report.Err) {
				silent = 0
			} else {
				silent++
			}
			if silent == 1 {
				log.Printf("unifi: %v; trying again in %s", report.Err, clientRetryAfter)
				wait = clientRetryAfter
			} else {
				select {
				case out <- report:
				case <-ctx.Done():
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case current = <-wants:
			ready = true
		case syncer = <-syncers:
			silent = 0
		case <-after(wait):
		}
	}
}

// keyPath is the key's file: the one bootstrap.json names, or else network-key beside state.db.
func keyPath(b Bootstrap) string {
	if b.NetworkCredentialFile != "" {
		return b.NetworkCredentialFile
	}
	return filepath.Join(b.DataDir, networkKeyName)
}

// enablingMeta starts the meta row of each slot whose client the owner asked to turn back on, so a
// restart before the pass that does it still asks.
const enablingMeta = "enable_client:"

// acceptedKeyMeta is the meta row holding the revision of the last commit that accepted a key.
const acceptedKeyMeta = "unifi_key_revision"

// waitingKey is where a key UniFi accepted waits, beside the key's file, for the commit at revision.
// The key's file changes only once that commit has landed, so a save that answers 5xx changed nothing.
func waitingKey(path string, revision uint64) string {
	return path + "." + strconv.FormatUint(revision, 10)
}

// writeWaiting needs no temporary name: a waiting key is read only once its commit has landed, which
// is after this returns.
func writeWaiting(path string, key domain.Secret) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.WriteString(key.Reveal())
	}
	if err == nil {
		err = f.Sync()
	}
	if closed := f.Close(); err == nil {
		err = closed
	}
	if err == nil {
		err = syncDir(filepath.Dir(path))
	}
	return err
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	if closed := dir.Close(); err == nil {
		err = closed
	}
	return err
}

// settleKey moves the key waiting for the commit at revision accepted into the key's file, deletes
// every key waiting for a commit that never landed, and names a key the page saved in bootstrap.json
// when nothing names one. It runs after the commit that accepts a key and at each start, so a crash
// between that commit and the move leaves the move to the next start.
func settleKey(b *Bootstrap, accepted uint64) error {
	path := keyPath(*b)
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		at, waits := strings.CutPrefix(e.Name(), filepath.Base(path)+".")
		revision, err := strconv.ParseUint(at, 10, 64)
		if !waits || err != nil {
			continue
		}
		if revision == accepted {
			err = os.Rename(filepath.Join(dir, e.Name()), path)
		} else {
			err = os.Remove(filepath.Join(dir, e.Name()))
		}
		if err != nil {
			return err
		}
	}
	if b.NetworkCredentialFile != "" || accepted == 0 {
		return nil
	}
	fields := map[string]any{}
	raw, err := os.ReadFile(bootstrapPath(b.DataDir))
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fmt.Errorf("bootstrap.json: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	fields["networkCredentialFile"] = path
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(bootstrapPath(b.DataDir), append(data, '\n'), 0o600); err != nil {
		return err
	}
	b.NetworkCredentialFile = path
	return nil
}

// sendWants tells the UniFi actor when what a slot needs of its client changes.
func (c *controller) sendWants() {
	wants := []unifi.Want{}
	for _, id := range slices.Sorted(mapKeys(c.state.slots)) {
		row := c.state.slots[id]
		if row.Deleting {
			continue
		}
		want, err := c.wantOf(row)
		if err != nil {
			log.Printf("slot %s: client config: %v", id, err)
			continue
		}
		wants = append(wants, want)
	}
	key := wantsKey(wants)
	if key == c.wantsKey {
		return
	}
	c.wantsKey = key
	replace(c.wants, wants)
}

func wantsKey(wants []unifi.Want) string {
	var b strings.Builder
	for _, w := range wants {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s|%t\n", w.Slot, w.Name, w.Bound, w.Endpoint, w.Address, w.DNS, w.Enable)
	}
	return b.String()
}

func (c *controller) wantOf(row slotRow) (unifi.Want, error) {
	a := row.Slot.Allocation
	dns, err := netip.ParseAddr(c.app.bootstrap.ClientDNS)
	if err != nil {
		return unifi.Want{}, errors.New("clientDns is not an address")
	}
	address := a.Tunnel.Addr().Next().Next()
	return unifi.Want{
		Slot: row.Slot.ID, Name: row.Slot.Intent.Name(), Bound: a.NetworkID,
		Endpoint: netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), a.PeerListenPort),
		Address:  netip.PrefixFrom(address, a.Tunnel.Bits()), DNS: dns,
		PeerPublicKey: a.Keys.HostPublic, PublicKey: a.Keys.ClientPublic, PrivateKey: c.state.secrets[a.Keys.ClientPrivate],
		Enable: c.state.enabling[row.Slot.ID],
	}, nil
}

// applyClients commits the _ids a pass found, and whether it found a route to each bound client, before
// anything can depend on them. It keeps what the page shows of UniFi: each client's trouble as a value,
// the problem and its error in the log.
func (c *controller) applyClients(ctx context.Context, r clientReport) error {
	c.key.passed(r.Err, c.now())
	if r.Err != nil {
		log.Printf("unifi: %v", r.Err)
		c.clientTroubles = nil
		c.publish(c.now())
		return nil
	}
	troubles := map[domain.SlotID]ClientTrouble{}
	reported := map[domain.SlotID]domain.NetworkID{}
	for _, s := range r.Report.Slots {
		if s.Problem != nil {
			log.Printf("unifi: slot %s: %v", s.Slot, s.Problem)
			if t, named := clientTroubles[s.Problem.Kind]; named {
				troubles[s.Slot] = t
			}
		}
		if s.NetworkID != "" {
			reported[s.Slot] = s.NetworkID
		}
	}
	var tx transaction
	for _, id := range slices.Sorted(mapKeys(c.state.slots)) {
		row := c.state.slots[id]
		client, ok := reported[id]
		if !ok {
			client = row.Slot.Allocation.NetworkID
		}
		if client == "" {
			continue
		}
		if next, changed := row.withClient(client, !r.Report.Inventory.Routed(client)); changed {
			tx.Slots = append(tx.Slots, next)
		}
	}
	for _, retained := range r.Report.Retained {
		if retained.Problem != nil {
			log.Printf("unifi: client %s: %v", retained.NetworkID, retained.Problem)
			if t, named := clientTroubles[retained.Problem.Kind]; named {
				troubles[retained.Slot] = t
			}
		}
	}
	owned := map[domain.SlotID]unifi.Owned{}
	for _, o := range r.Report.Inventory.Owned {
		owned[o.Slot] = o
	}
	var enabled []domain.SlotID
	for slot := range c.state.enabling {
		if o, ok := owned[slot]; !ok || o.Enabled == nil || *o.Enabled {
			enabled = append(enabled, slot)
			tx.DeletedMeta = append(tx.DeletedMeta, enablingMeta+string(slot))
		}
	}
	if len(tx.Slots) > 0 || len(enabled) > 0 {
		if err := c.store.commit(ctx, tx); err != nil {
			return err
		}
		for _, row := range tx.Slots {
			c.state.slots[row.Slot.ID] = row
		}
		for _, slot := range enabled {
			delete(c.state.enabling, slot)
		}
	}
	c.owned, c.clientTroubles, c.foreign, c.clientsKnown = owned, troubles, r.Report.Inventory.Foreign, true
	c.publish(c.now())
	return nil
}

// change queues an event for the next publish, which makes it readable.
func (c *controller) change(kind string, slot domain.SlotID, source domain.SourceID) {
	if kind == "switch" {
		c.servedAt = time.Time{}
	}
	e := ChangeEvent{Kind: kind}
	if slot != "" {
		e.Slot = &slot
	}
	if source != "" {
		e.Source = &source
	}
	c.changes = append(c.changes, e)
}

// analyticsEvery spaces out analytics events, whose reads are the page's costliest.
const analyticsEvery = time.Minute

// noteChanges compares what was just published with what the page last heard about, so
// every way a slot or a source can change says so, and nothing else does.
func (c *controller) noteChanges(view View, now time.Time) {
	slots := make(map[domain.SlotID]string, len(view.Slots))
	for _, s := range view.Slots {
		// A backup's rank moves with every Ping sample. Its membership is news.
		s.Backups = slices.Sorted(slices.Values(s.Backups))
		encoded, _ := json.Marshal(s)
		slots[s.ID] = string(encoded)
		if c.seenSlots != nil && c.seenSlots[s.ID] != slots[s.ID] {
			c.change("slot", s.ID, "")
		}
	}
	for id := range c.seenSlots {
		if _, kept := slots[id]; !kept {
			c.change("slot", id, "")
		}
	}
	sources := make(map[domain.SourceID]string, len(view.Sources))
	for _, s := range view.Sources {
		encoded, _ := json.Marshal(s)
		sources[s.ID] = string(encoded)
		if c.seenSubs != nil && c.seenSubs[s.ID] != sources[s.ID] {
			c.change("source", "", s.ID)
		}
	}
	for id := range c.seenSubs {
		if _, kept := sources[id]; !kept {
			c.change("source", "", id)
		}
	}
	groups := make(map[domain.GroupID]string, len(view.Groups))
	for _, g := range view.Groups {
		encoded, _ := json.Marshal(g)
		groups[g.ID] = string(encoded)
		if c.seenGroups != nil && c.seenGroups[g.ID] != groups[g.ID] {
			c.change("group", "", "")
		}
	}
	for id := range c.seenGroups {
		if _, kept := groups[id]; !kept {
			c.change("group", "", "")
		}
	}
	// The board's foreign rows, UniFi's trouble and the core's versions aren't in any slot, so a
	// change to them says "slot" with no slot.
	encoded, _ := json.Marshal(struct {
		Clients VpnClients
		Trouble *UniFiTrouble
		Core    CoreView
	}{view.VPNClients, view.UniFiTrouble, view.Core})
	if c.seenClients != string(encoded) {
		if c.seenClients != "" {
			c.change("slot", "", "")
		}
		c.seenClients = string(encoded)
	}
	if key := c.world.serverKey() + fmt.Sprint(c.evidence.udpStates()); key != c.seenServers {
		if c.seenServers != "" {
			c.change("catalogue", "", "")
		}
		c.seenServers = key
	}
	c.seenSlots, c.seenSubs, c.seenGroups = slots, sources, groups
	if c.state.settingsRevision != c.seenSettings {
		c.seenSettings = c.state.settingsRevision
		c.change("settings", "", "")
	}
	if batches := c.app.writer.Batches(); batches != c.seenBatches && now.Sub(c.analyticsAt) >= analyticsEvery {
		c.seenBatches, c.analyticsAt = batches, now
		c.change("analytics", "", "")
	}
	for _, e := range c.changes {
		e.At, e.Revision = now, c.state.revision
		c.app.feed.emit(e)
	}
	c.changes = c.changes[:0]
}

// PutUniFiKey stores a key UniFi has just accepted and hands its syncer to the UniFi actor, which
// passes at once. The page's handler tried the key, since UniFi can take seconds.
type PutUniFiKey struct {
	Key    domain.Secret
	Syncer unifi.Syncer
}

func (PutUniFiKey) request() domain.RequestID { return "" }

// digest never holds the key. A page write's own fingerprint is what a retry is matched on.
func (PutUniFiKey) digest() string                        { return "unifi key" }
func (PutUniFiKey) answer(*controller, *stage) (int, any) { return answered() }

func (cmd PutUniFiKey) perform(c *controller, s *stage) error {
	revision := uint64(s.receipt.Revision)
	waiting := waitingKey(keyPath(c.app.bootstrap), revision)
	if err := writeWaiting(waiting, cmd.Key); err != nil {
		os.Remove(waiting)
		return err
	}
	s.tx.Meta[acceptedKeyMeta] = revision
	s.otherwise(func() { os.Remove(waiting) })
	s.then(func() {
		c.state.acceptedKey = revision
		if err := settleKey(&c.app.bootstrap, revision); err != nil {
			log.Printf("unifi key: the accepted key waits for the next start to put it in place: %v", err)
		}
		c.key.saved(cmd.Syncer, s.now)
		replace(c.syncers, cmd.Syncer)
	})
	return nil
}
