package daemon

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
)

// applyDocument is the one path a parsed document takes into the catalogue, from a refresh or a
// paste. It stages the lineages, and the source's Feed becomes what this document listed.
func (c *controller) applyDocument(s *stage, source *domain.Source, doc catalogue.Document, now time.Time, event *refreshEvent) {
	revision := c.state.catalogueRevision + 1
	rec := catalogue.Reconcile(c.world.catalogue.All(), doc, source.ID, source.Revision, domain.CatalogueRevision(revision), now, lineageGrace)
	var next []domain.Lineage
	introduced := map[domain.LineageID]record{}
	for _, change := range rec.Changes {
		next = append(next, change.Lineage)
		switch {
		case change.Previous == nil:
			event.Added++
			introduced[change.Lineage.ID] = record{FirstSeen: now, WorkedFrom: now}
		case change.Lineage.Presence == domain.Present:
			event.Changed++
		default:
			event.Missing++
		}
	}
	next = append(next, rec.Restated...)
	feed := rec.Feed
	source.Feed = &feed
	event.Diagnostics = doc.Diagnostics()
	event.Replaced = rec.Replaced
	s.tx.Lineages = next
	if len(introduced) > 0 {
		s.tx.Records = introduced
	}
	s.tx.Meta["catalogue_revision"] = revision
	s.next = s.next.with(worldChange{lineages: next, revision: domain.CatalogueRevision(revision)})
	id, recorded := source.ID, *event
	s.then(func() {
		c.state.catalogueRevision = revision
		c.diffs[id] = recorded
		maps.Copy(c.evidence.book.records, introduced)
		if len(next) > 0 {
			c.change("catalogue", "", "")
		}
	})
}

func (cmd PutSource) digest() string {
	text := fmt.Sprintf("source %s %d %s", cmd.ID, cmd.Expected, cmd.Input.Name)
	if r := cmd.Input.Remote; r != nil {
		text += fmt.Sprintf(" remote %s %s %d %v", r.URL.Reveal(), r.UserAgent, r.RefreshEvery, r.Paused)
	}
	if m := cmd.Input.Manual; m != nil {
		text += " manual " + m.Text.Reveal()
	}
	return text
}

func (cmd RefreshSource) perform(c *controller, s *stage) error {
	source, ok := c.state.sources[cmd.ID]
	if !ok {
		return refuse(KindNotFound, RefusedSourceGone, "That source is gone.")
	}
	remote, ok := source.Origin.Remote()
	if !ok {
		return refuse(KindValidation, RefusedNotRemote, "Only a source with a URL refreshes.")
	}
	if cmd.UserAgent != "" && cmd.UserAgent != remote.UserAgent {
		// Another agent is an owner's write, as a save's is, so it moves the source's revision and drops a fetch in flight with the old one.
		remote.UserAgent = cmd.UserAgent
		source.Origin = domain.RemoteOrigin(remote)
		source.Revision++
		s.tx.Sources = append(s.tx.Sources, source)
		s.then(func() { c.state.sources[cmd.ID] = source })
	}
	s.then(func() { c.queue.Set(source.ID, c.state.fetches[source.ID].ManualDue(s.now)) })
	return nil
}

// newDeviceID is a source's own device ID, for panels that limit devices. It is random, so it
// never names this gateway's serial, and two sources never look like one device.
func newDeviceID() domain.Secret {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return domain.NewSecret(hex.EncodeToString(raw))
}

func secretRefFor(id domain.SourceID, what string, value domain.Secret) domain.SecretRef {
	sum := sha256.Sum256([]byte(value.Reveal()))
	return domain.SecretRef("source/" + string(id) + "/" + what + "/" + hex.EncodeToString(sum[:8]))
}

func (cmd PutSource) perform(c *controller, s *stage) error {
	current, exists := c.state.sources[cmd.ID]
	in := cmd.Input
	_, wasRemote := current.Origin.Remote()
	switch {
	case cmd.ID == "":
		return refuse(KindValidation, RefusedBadID, "A source needs an ID.")
	case strings.TrimSpace(in.Name) == "" && in.Remote == nil:
		return refuse(KindValidation, RefusedNoName, "A source without a URL needs a name.")
	case exists && cmd.Expected == 0:
		return stale(uint64(current.Revision))
	case !exists && cmd.Expected != 0:
		return refuse(KindNotFound, RefusedSourceGone, "That source is gone.")
	case exists && domain.IntentRevision(current.Revision) != cmd.Expected:
		return stale(uint64(current.Revision))
	case (in.Remote == nil) == (in.Manual == nil):
		return refuse(KindValidation, RefusedMalformed, "A source is either a URL or pasted text.")
	case exists && wasRemote != (in.Remote != nil):
		return refuse(KindValidation, RefusedOriginFixed, "A source can't change between a URL and pasted text.")
	}
	next := domain.SourceRevision(c.state.retired[retiredSource(cmd.ID)]) + 1
	if exists {
		next = current.Revision + 1
	}
	source := current
	source.ID, source.Revision, source.Name = cmd.ID, next, strings.TrimSpace(in.Name)
	if !exists {
		source.Added = s.now
	}
	secrets := map[domain.SecretRef]domain.Secret{}
	var dropped []domain.SecretRef
	replace := func(ref *domain.SecretRef, what string, value domain.Secret) bool {
		was := *ref
		*ref = secretRefFor(cmd.ID, what, value)
		secrets[*ref] = value
		if *ref == was {
			return false
		}
		if was != "" {
			dropped = append(dropped, was)
		}
		return true
	}
	fetchNow := false
	switch {
	case in.Remote != nil:
		remote, _ := current.Origin.Remote()
		if !exists && in.Remote.URL.IsZero() {
			return refuse(KindValidation, RefusedNoURL, "A new source needs a URL.")
		}
		agent := cmp.Or(in.Remote.UserAgent, catalogue.DefaultUserAgent)
		fetchNow = !exists || agent != remote.UserAgent
		remote.UserAgent, remote.RefreshEvery, remote.Paused = agent, in.Remote.RefreshEvery, in.Remote.Paused
		if !in.Remote.URL.IsZero() {
			fetchNow = replace(&remote.Location, "url", in.Remote.URL) || fetchNow
			if u, err := url.Parse(in.Remote.URL.Reveal()); err == nil {
				remote.Host = u.Hostname()
			}
		}
		if remote.HWID == "" {
			replace(&remote.HWID, "hwid", newDeviceID())
		}
		source.Origin = domain.RemoteOrigin(remote)
	case in.Manual.Document != nil:
		manual, _ := current.Origin.Manual()
		replace(&manual.Text, "text", in.Manual.Text)
		source.Origin = domain.ManualOrigin(manual)
		event := refreshEvent{Source: cmd.ID, At: s.now, Outcome: catalogue.Updated}
		c.applyDocument(s, &source, *in.Manual.Document, s.now, &event)
		s.tx.RefreshEvents = []refreshEvent{event}
	case !exists:
		return refuse(KindValidation, RefusedNoText, "A new source needs text.")
	}
	s.tx.Sources = append(s.tx.Sources, source)
	s.tx.Secrets, s.tx.DeletedSecrets = secrets, dropped
	s.then(func() {
		c.state.sources[cmd.ID] = source
		for ref, s := range secrets {
			c.state.secrets[ref] = s
		}
		for _, ref := range dropped {
			delete(c.state.secrets, ref)
		}
		if in.Remote != nil {
			due := c.due(source, c.state.fetches[cmd.ID])
			if fetchNow {
				due = c.state.fetches[cmd.ID].ManualDue(c.now())
			}
			c.queue.Set(cmd.ID, due)
		}
	})
	return nil
}

// perform keeps lineages as tombstones, so history still resolves them, and refuses while a slot
// would lose its Exit. A group the delete would remove goes in the same transaction, and others are
// rewritten there, so a stored group never names a removed source. A slot serving this source's
// server through a surviving group isn't referenced: the server tombstones and the slot moves.
func (cmd DeleteSource) perform(c *controller, s *stage) error {
	current, exists := c.state.sources[cmd.ID]
	switch {
	case !exists:
		return refuse(KindNotFound, RefusedSourceGone, "That source is gone.")
	case cmd.Expected != 0 && domain.IntentRevision(current.Revision) != cmd.Expected:
		return stale(uint64(current.Revision))
	}
	deletion := c.world.sourceDeletion(cmd.ID)
	removed, rewritten := deletion.removed, deletion.rewritten
	if len(deletion.usedBy) > 0 {
		return c.refuseReferenced("This source", deletion.usedBy)
	}
	key := retiredSource(cmd.ID)
	s.tx.Retired = map[string]uint64{key: uint64(current.Revision)}
	var deleted []domain.GroupID
	for _, id := range slices.Sorted(mapKeys(removed)) {
		deleted = append(deleted, id)
		s.tx.Retired[retiredGroup(id)] = uint64(c.world.groups[id].Revision)
	}
	var gone []domain.Lineage
	for _, l := range c.world.catalogue.Source(cmd.ID) {
		gone = append(gone, l.Orphaned(s.now))
	}
	s.tx.Lineages = gone
	s.tx.Groups, s.tx.DeletedGroups = rewritten, deleted
	s.tx.DeletedSources = []domain.SourceID{cmd.ID}
	s.tx.DeletedFetches = []domain.SourceID{cmd.ID}
	refs := originSecrets(current.Origin)
	s.tx.DeletedSecrets = refs
	s.tx.OwesErase = true
	s.next = s.next.with(worldChange{lineages: gone, groups: rewritten, removed: deleted})
	retired := s.tx.Retired
	s.then(func() {
		maps.Copy(c.state.retired, retired)
		delete(c.state.sources, cmd.ID)
		delete(c.state.fetches, cmd.ID)
		delete(c.diffs, cmd.ID)
		for _, ref := range refs {
			delete(c.state.secrets, ref)
		}
		c.queue.Remove(cmd.ID)
		c.change("catalogue", "", "")
	})
	return nil
}

func originSecrets(o domain.Origin) []domain.SecretRef {
	var refs []domain.SecretRef
	if r, ok := o.Remote(); ok {
		refs = append(refs, r.Location, r.HWID)
	}
	if m, ok := o.Manual(); ok {
		refs = append(refs, m.Text)
	}
	return slices.DeleteFunc(refs, func(ref domain.SecretRef) bool { return ref == "" })
}
