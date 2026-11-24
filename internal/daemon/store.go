package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
)

const stateSchemaVersion = 15

const stateSchema = `
CREATE TABLE meta(key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE settings(id INTEGER PRIMARY KEY CHECK(id=1), body BLOB NOT NULL);
CREATE TABLE secrets(ref TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE sources(id TEXT PRIMARY KEY, body BLOB NOT NULL) WITHOUT ROWID;
CREATE TABLE fetch_state(source TEXT PRIMARY KEY, body BLOB NOT NULL) WITHOUT ROWID;
CREATE TABLE lineages(id TEXT PRIMARY KEY, source TEXT NOT NULL, presence INTEGER NOT NULL, country TEXT NOT NULL,
	body BLOB NOT NULL) WITHOUT ROWID;
CREATE INDEX lineages_source ON lineages(source, presence, country);
CREATE TABLE groups(id TEXT PRIMARY KEY, revision INTEGER NOT NULL, body BLOB NOT NULL) WITHOUT ROWID;
CREATE TABLE slots(id TEXT PRIMARY KEY, version INTEGER NOT NULL, deleting INTEGER NOT NULL, intent BLOB NOT NULL,
	allocation BLOB NOT NULL, carries BLOB NOT NULL, created INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE owners(id TEXT PRIMARY KEY, memory BLOB NOT NULL) WITHOUT ROWID;
CREATE TABLE pending(slot TEXT PRIMARY KEY, event TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE landings(seq INTEGER PRIMARY KEY, slot TEXT NOT NULL, event TEXT, at INTEGER NOT NULL);
CREATE INDEX landings_slot ON landings(slot, seq);
CREATE TABLE switch_events(id TEXT PRIMARY KEY, owner TEXT NOT NULL, intent INTEGER NOT NULL, reason INTEGER NOT NULL,
	requested_at INTEGER NOT NULL, applied_at INTEGER, body BLOB NOT NULL) WITHOUT ROWID;
CREATE INDEX switch_events_time ON switch_events(requested_at);
CREATE TABLE refresh_events(id INTEGER PRIMARY KEY, source TEXT NOT NULL, at INTEGER NOT NULL, outcome INTEGER NOT NULL,
	failure TEXT NOT NULL, wan_excluded INTEGER NOT NULL, added INTEGER NOT NULL, changed INTEGER NOT NULL, missing INTEGER NOT NULL,
	diagnostics BLOB, replaced BLOB);
CREATE INDEX refresh_events_time ON refresh_events(at);
CREATE TABLE request_receipts(request TEXT PRIMARY KEY, digest BLOB NOT NULL, revision INTEGER NOT NULL,
	at INTEGER NOT NULL, status INTEGER NOT NULL, etag TEXT NOT NULL, body BLOB) WITHOUT ROWID;
CREATE INDEX request_receipts_time ON request_receipts(at);
CREATE TABLE retired(id TEXT PRIMARY KEY, revision INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE records(lineage TEXT PRIMARY KEY, body BLOB NOT NULL) WITHOUT ROWID;
CREATE TABLE ranking(id INTEGER PRIMARY KEY CHECK(id=1), body BLOB NOT NULL);
` + switchAppliedIndex

// switchAppliedIndex serves the reads of switches applied in a window. It changes no row, so a file
// made without it gains it on open, at the same schema version.
const switchAppliedIndex = `
CREATE INDEX IF NOT EXISTS switch_events_applied ON switch_events(applied_at);
`

// store is state.db: intent, decisions, and the events that explain them. Its write
// connection is held only by the controller. queries gets a separate read-only pool,
// so a reader cannot begin a write transaction. No measurement sample is written here;
// that is metrics.db.
type store struct {
	writer *sql.DB
	owes   eraseDebt
}

// eraseDebt is what state.db owes since a commit forgot credentials. Their rows stay in the file's
// free pages until a vacuum rebuilds it, and in the WAL's older frames until a checkpoint empties
// it, which a reader older than the vacuum can hold up. A try that finds it busy owes only the
// checkpoint, so a retry doesn't write the whole file again. The meta row eraseOwedKey keeps the
// debt across a restart, which owes the vacuum again.
type eraseDebt uint8

const (
	nothingOwed eraseDebt = iota
	checkpointOwed
	vacuumOwed
)

func (d eraseDebt) String() string {
	return [...]string{"nothing", "the checkpoint", "the vacuum and the checkpoint"}[d]
}

// eraseOwedKey is the meta row a commit that forgets credentials sets in its own transaction, so
// a crash, a full disk or a busy reader can't lose the erase it owes. Only a finished erase clears it.
const eraseOwedKey = "erase_owed"

type queries struct{ reader *sql.DB }

// refreshEvent is one fetch outcome, committed with the lineage changes it caused.
// A failure the WAN explains is kept but never counted against the provider.
type refreshEvent struct {
	Source      domain.SourceID
	At          time.Time
	Outcome     catalogue.Outcome
	FailureCode catalogue.FailureCode
	WANExcluded bool
	Added       int
	Changed     int
	Missing     int
	Diagnostics []catalogue.Diagnostic
	Replaced    []catalogue.Replaced
}

// slotRow is one slot's durable row. Selection is what the slot carries, which is its owner's once
// evaluate has run, and LastServed is the exact ref the core last reported applied for it. Owes and
// Closed are its close debt (closeOwed). Every other decision belongs to the owner.
type slotRow struct {
	Slot       domain.Slot
	CreatedAt  time.Time
	Deleting   bool
	Selection  domain.Selection
	LastServed domain.ServerRef
	// Owes is a close the slot owes that its pending switch's own reason doesn't name: one inherited from
	// a switch that pending superseded, or one a switch left unattempted when it landed.
	Owes domain.EventID
	// Closed is the last close the runtime reported attempting for the slot.
	Closed domain.EventID
}

// deleted is the row a delete leaves. Its closure is no owner's choice, and no owner follows it.
func (r slotRow) deleted() slotRow {
	r.Deleting, r.Selection = true, domain.Closed(domain.Deleting)
	return r
}

// remade is a create over a row still being deleted. It keeps the allocation, whose gate and client the
// next plan converges on, and forgets the client it may have lost. It stays on the deletion's closure,
// so its first move ends any flow the slot before it left on the allocation.
func (r slotRow) remade() slotRow {
	r.Deleting, r.LastServed = false, domain.ServerRef{}
	r.Slot.Allocation.NetworkID, r.Slot.Allocation.Unrouted = "", false
	return r
}

// withClient binds the row to UniFi's client id, as a pass that found no route to it or found one, and
// says whether that changed the row. A deleting row keeps what it had.
func (r slotRow) withClient(id domain.NetworkID, unrouted bool) (slotRow, bool) {
	a := r.Slot.Allocation
	if r.Deleting || a.NetworkID == id && a.Unrouted == unrouted {
		return r, false
	}
	r.Slot.Allocation.NetworkID, r.Slot.Allocation.Unrouted = id, unrouted
	return r, true
}

// landing is a slot showing the move it owes, or the runtime removing the slot when Move is nil. A slot
// is where its newest landing took it, so this, not the slots a switch names, is what a slot served.
type landing struct {
	Slot domain.SlotID
	Move *domain.Switch
	At   time.Time
}

// transaction is one commit. Everything in it lands together or not at all.
type transaction struct {
	Request        *Receipt
	RequestDigest  []byte
	Sources        []domain.Source
	DeletedSources []domain.SourceID
	Fetches        map[domain.SourceID]catalogue.FetchState
	DeletedFetches []domain.SourceID
	Lineages       []domain.Lineage
	Collected      []domain.LineageID // Deleted servers whose rows and records go.
	Groups         []domain.Group
	DeletedGroups  []domain.GroupID
	RefreshEvents  []refreshEvent
	Slots          []slotRow
	DeletedSlots   []domain.SlotID
	Owners         map[domain.Owner]policyMemory
	DeletedOwners  []domain.Owner
	Pending        map[domain.SlotID]domain.EventID
	Landings       []landing
	Switches       []domain.Switch
	Secrets        map[domain.SecretRef]domain.Secret
	DeletedSecrets []domain.SecretRef
	Retired        map[string]uint64
	Records        map[domain.LineageID]record
	Ranking        *rankedSite
	Settings       *Settings
	Meta           map[string]uint64
	DeletedMeta    []string
	// OwesErase is set on a commit that forgets credentials. A deleted row stays in the file's free
	// pages and in the WAL's older frames until erase rebuilds the file and empties the WAL.
	OwesErase bool
}

func openState(path, query string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?"+query)
}

const stateName = "state.db"

func statePath(dir string) string { return filepath.Join(dir, stateName) }

func openStore(ctx context.Context, dir string) (*store, *queries, error) {
	path := statePath(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	f.Close()
	writer, err := openState(path, "_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, err
	}
	writer.SetMaxOpenConns(1)
	s := &store{writer: writer}
	if err := s.migrate(ctx); err != nil {
		writer.Close()
		return nil, nil, fmt.Errorf("state.db: %w", err)
	}
	q, err := openQueries(dir)
	if err != nil {
		writer.Close()
		return nil, nil, err
	}
	return s, q, nil
}

func openQueries(dir string) (*queries, error) {
	reader, err := openState(statePath(dir), "mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)&_pragma=cache_size(-256)")
	if err != nil {
		return nil, err
	}
	reader.SetMaxOpenConns(2)
	return &queries{reader: reader}, nil
}

func (s *store) migrate(ctx context.Context) error {
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA cache_size=-512"} {
		if _, err := s.writer.ExecContext(ctx, p); err != nil {
			return err
		}
	}
	var version int
	if err := s.writer.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == stateSchemaVersion {
		_, err := s.writer.ExecContext(ctx, switchAppliedIndex)
		return err
	}
	if version != 0 {
		return fmt.Errorf("schema version %d, this build knows only %d: delete the file", version, stateSchemaVersion)
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, stateSchema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", stateSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// recover loads desired state and allocations, not an assertion that runtime is ready.
// Fetch states decode as catalogue.FetchState, so their Due survives the restart.
// Switches are history. Each slot still owes the move its own readback hasn't shown,
// which readback finishes and nothing replays. Transient probe rounds were never
// stored, and a clock that moved backwards can't leave a deadline or a keep-out longer than
// its own length from now. The world it returns is the catalogue, groups and Exits as stored, with nothing
// refused yet, since only a running core can say that.
func (s *store) recover(ctx context.Context, now time.Time, t timings) (durableState, world, error) {
	state, catalogue, groups, err := s.load(ctx, now, t)
	if err != nil {
		return state, world{}, err
	}
	return state, newWorld(catalogue, groups, map[domain.ServerRef]bool{}, usesOf(state.slots)), nil
}

func (s *store) load(ctx context.Context, now time.Time, t timings) (durableState, domain.Catalogue, map[domain.GroupID]domain.Group, error) {
	var none domain.Catalogue
	noGroups := map[domain.GroupID]domain.Group{}
	state := durableState{
		sources:  map[domain.SourceID]domain.Source{},
		fetches:  map[domain.SourceID]catalogue.FetchState{},
		slots:    map[domain.SlotID]slotRow{},
		owners:   map[domain.Owner]policyMemory{},
		enabling: map[domain.SlotID]bool{},
		pending:  map[domain.SlotID]domain.Switch{},
		secrets:  map[domain.SecretRef]domain.Secret{},
		retired:  map[string]uint64{},
		settings: DefaultSettings(),
	}
	var erasing uint64
	meta := map[string]*uint64{"revision": (*uint64)(&state.revision), "runtime_generation": &state.runtimeGeneration,
		"catalogue_revision": (*uint64)(&state.catalogueRevision), "settings_revision": &state.settingsRevision, acceptedKeyMeta: &state.acceptedKey, eraseOwedKey: &erasing}
	rows, err := s.writer.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return state, none, noGroups, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value int64
		if err := rows.Scan(&key, &value); err != nil {
			return state, none, noGroups, err
		}
		if p, ok := meta[key]; ok {
			*p = uint64(value)
		} else if slot, asked := strings.CutPrefix(key, enablingMeta); asked {
			state.enabling[domain.SlotID(slot)] = true
		}
	}
	rows.Close()
	if erasing != 0 {
		s.owes = vacuumOwed
	}

	var body []byte
	switch err := s.writer.QueryRowContext(ctx, `SELECT body FROM settings WHERE id=1`).Scan(&body); {
	case err == nil:
		if err := json.Unmarshal(body, &state.settings); err != nil {
			return state, none, noGroups, fmt.Errorf("settings: %w", err)
		}
	case !errors.Is(err, sql.ErrNoRows):
		return state, none, noGroups, err
	}
	switch err := s.writer.QueryRowContext(ctx, `SELECT body FROM ranking WHERE id=1`).Scan(&body); {
	case err == nil:
		if state.ranking, err = decodeRanking(body); err != nil {
			return state, none, noGroups, fmt.Errorf("ranking: %w", err)
		}
	case !errors.Is(err, sql.ErrNoRows):
		return state, none, noGroups, err
	}

	if err := scanAll(ctx, s.writer, `SELECT ref, value FROM secrets`, func(r *sql.Rows) error {
		var ref, value string
		if err := r.Scan(&ref, &value); err != nil {
			return err
		}
		state.secrets[domain.SecretRef(ref)] = domain.NewSecret(value)
		return nil
	}); err != nil {
		return state, none, noGroups, err
	}
	if err := scanAll(ctx, s.writer, `SELECT id, revision FROM retired`, func(r *sql.Rows) error {
		var id string
		var revision int64
		if err := r.Scan(&id, &revision); err != nil {
			return err
		}
		state.retired[id] = uint64(revision)
		return nil
	}); err != nil {
		return state, none, noGroups, err
	}
	if err := scanAll(ctx, s.writer, `SELECT id, body FROM sources`, func(r *sql.Rows) error {
		var id string
		var body []byte
		if err := r.Scan(&id, &body); err != nil {
			return err
		}
		source, err := decodeSource(domain.SourceID(id), body)
		if err != nil {
			return fmt.Errorf("source %s: %w", id, err)
		}
		state.sources[source.ID] = source
		return nil
	}); err != nil {
		return state, none, noGroups, err
	}
	if err := scanAll(ctx, s.writer, `SELECT source, body FROM fetch_state`, func(r *sql.Rows) error {
		var id string
		var body []byte
		if err := r.Scan(&id, &body); err != nil {
			return err
		}
		var fs catalogue.FetchState
		if err := fs.UnmarshalBinary(body); err != nil {
			return fmt.Errorf("fetch state %s: %w", id, err)
		}
		state.fetches[domain.SourceID(id)] = fs
		return nil
	}); err != nil {
		return state, none, noGroups, err
	}
	var lineages []domain.Lineage
	if err := scanAll(ctx, s.writer, `SELECT id, source, body FROM lineages`, func(r *sql.Rows) error {
		var id, source string
		var body []byte
		if err := r.Scan(&id, &source, &body); err != nil {
			return err
		}
		l, err := decodeLineage(domain.LineageID(id), domain.SourceID(source), body)
		if err != nil {
			return err
		}
		lineages = append(lineages, l)
		return nil
	}); err != nil {
		return state, none, noGroups, err
	}
	// A deleted source's servers are orphaned here as well as by the delete, so one deleted by a build
	// that kept their credentials loses them at the next start.
	var orphans []domain.Lineage
	for i, l := range lineages {
		if _, live := state.sources[l.Source]; !live {
			if orphan := l.Orphaned(now); !reflect.DeepEqual(orphan, l) {
				lineages[i] = orphan
				orphans = append(orphans, orphan)
			}
		}
	}
	if len(orphans) > 0 {
		if err := s.commit(ctx, transaction{Lineages: orphans, OwesErase: true}); err != nil {
			return state, none, noGroups, err
		}
	}
	catalogue := domain.NewCatalogue(domain.CatalogueRevision(state.catalogueRevision), lineages)
	groups := map[domain.GroupID]domain.Group{}
	if err := scanAll(ctx, s.writer, `SELECT id, revision, body FROM groups`, func(r *sql.Rows) error {
		var id string
		var revision int64
		var body []byte
		if err := r.Scan(&id, &revision, &body); err != nil {
			return err
		}
		g, err := decodeGroup(domain.GroupID(id), domain.GroupRevision(revision), body)
		if err != nil {
			return fmt.Errorf("group %s: %w", id, err)
		}
		groups[g.ID] = g
		return nil
	}); err != nil {
		return state, none, noGroups, err
	}
	if err := scanAll(ctx, s.writer, `SELECT id, version, deleting, intent, allocation, carries, created FROM slots`, func(r *sql.Rows) error {
		var id string
		var version, deleting, created int64
		var intent, allocation, carries []byte
		if err := r.Scan(&id, &version, &deleting, &intent, &allocation, &carries, &created); err != nil {
			return err
		}
		row := slotRow{Deleting: deleting != 0, CreatedAt: time.UnixMilli(created).UTC()}
		var err error
		row.Slot = domain.Slot{ID: domain.SlotID(id), Version: domain.IntentRevision(version)}
		if row.Slot.Intent, err = decodeIntent(intent); err != nil {
			return fmt.Errorf("slot %s intent: %w", id, err)
		}
		if row.Slot.Allocation, err = decodeAllocation(allocation); err != nil {
			return fmt.Errorf("slot %s allocation: %w", id, err)
		}
		if err := decodeCarries(carries, &row); err != nil {
			return fmt.Errorf("slot %s: %w", id, err)
		}
		state.slots[row.Slot.ID] = row
		return nil
	}); err != nil {
		return state, none, noGroups, err
	}
	if err := scanAll(ctx, s.writer, `SELECT id, memory FROM owners`, func(r *sql.Rows) error {
		var key string
		var body []byte
		if err := r.Scan(&key, &body); err != nil {
			return err
		}
		owner, err := domain.ParseOwner(key)
		if err != nil {
			return err
		}
		m, err := decodeMemory(body)
		if err != nil {
			return fmt.Errorf("owner %s: %w", key, err)
		}
		if leave := now.Add(t.Handover); m.LeaveBy.After(leave) {
			m.LeaveBy = leave
		}
		m.Backoff = m.Backoff.clamped(now, t)
		m.Hand = m.Hand.step(now, restarted{})
		limit := now.Add(keepOutFor)
		for l, until := range m.KeepOut {
			if until.After(limit) {
				m.KeepOut[l] = limit
			}
		}
		state.owners[owner] = m
		return nil
	}); err != nil {
		return state, none, noGroups, err
	}
	if state.records, err = readRecords(ctx, s.writer); err != nil {
		return state, none, noGroups, err
	}
	if err := s.writer.QueryRowContext(ctx, `SELECT COALESCE(MAX(`+eventSequenceSQL+`), 0) FROM switch_events`).Scan(&state.lastEvent); err != nil {
		return state, none, noGroups, err
	}
	owed := map[domain.EventID]domain.Switch{}
	if err := scanAll(ctx, s.writer, `SELECT id, owner, intent, reason, requested_at, applied_at, body FROM switch_events WHERE id IN (SELECT event FROM pending)`, func(r *sql.Rows) error {
		sw, err := scanSwitch(r)
		if err != nil {
			return err
		}
		owed[sw.ID] = sw
		return nil
	}); err != nil {
		return state, none, noGroups, err
	}
	err = scanAll(ctx, s.writer, `SELECT slot, event FROM pending`, func(r *sql.Rows) error {
		var slot, event string
		if err := r.Scan(&slot, &event); err != nil {
			return err
		}
		sw, ok := owed[domain.EventID(event)]
		if !ok {
			return fmt.Errorf("slot %s owes event %s, which is gone", slot, event)
		}
		state.pending[domain.SlotID(slot)] = sw
		return nil
	})
	return state, catalogue, groups, err
}

// eventSequenceSQL reads the counter at the end of a switch_events row's ID, which orders events within
// one millisecond.
const eventSequenceSQL = `CAST(substr(id, length(rtrim(id, '0123456789'))+1) AS INTEGER)`

// eventSequence reads the counter at the end of an event's ID.
func eventSequence(id domain.EventID) uint64 {
	n, _ := strconv.ParseUint(string(id[strings.LastIndexByte(string(id), '-')+1:]), 10, 64)
	return n
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func scanAll(ctx context.Context, db querier, query string, row func(*sql.Rows) error, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := row(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// scanSwitch reads a switch row: id, owner, intent, reason, requested_at, applied_at, body.
func scanSwitch(r *sql.Rows) (domain.Switch, error) {
	var id, owner string
	var intent, reason, requested int64
	var applied sql.NullInt64
	var body []byte
	if err := r.Scan(&id, &owner, &intent, &reason, &requested, &applied, &body); err != nil {
		return domain.Switch{}, err
	}
	return switchOf(id, owner, intent, reason, requested, applied, body)
}

func switchOf(id, owner string, intent, reason, requested int64, applied sql.NullInt64, body []byte) (domain.Switch, error) {
	var err error
	s := domain.Switch{ID: domain.EventID(id), Intent: domain.IntentRevision(intent), Reason: domain.Reason(reason), RequestedAt: time.UnixMilli(requested).UTC()}
	if s.Owner, err = domain.ParseOwner(owner); err != nil {
		return s, fmt.Errorf("switch %s: %w", id, err)
	}
	if applied.Valid {
		at := time.UnixMilli(applied.Int64).UTC()
		s.AppliedAt = &at
	}
	return s, decodeSwitch(body, &s)
}

// commit lands tx atomically and durably. The caller publishes in-memory state and
// starts effects only after it returns. Intent, switch evidence and applied events
// commit with synchronous=FULL.
func (s *store) commit(ctx context.Context, tx transaction) error {
	t, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	exec := func(query string, args ...any) {
		if err == nil {
			_, err = t.ExecContext(ctx, query, args...)
		}
	}
	for key, value := range tx.Meta {
		exec(`INSERT INTO meta VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, int64(value))
	}
	for _, key := range tx.DeletedMeta {
		exec(`DELETE FROM meta WHERE key=?`, key)
	}
	if tx.OwesErase {
		exec(`INSERT INTO meta VALUES(?,1) ON CONFLICT(key) DO UPDATE SET value=1`, eraseOwedKey)
	}
	if tx.Settings != nil {
		body, jerr := json.Marshal(*tx.Settings)
		if jerr != nil {
			return jerr
		}
		exec(`INSERT INTO settings VALUES(1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body`, body)
	}
	for ref, secret := range tx.Secrets {
		exec(`INSERT INTO secrets VALUES(?,?) ON CONFLICT(ref) DO UPDATE SET value=excluded.value`, string(ref), secret.Reveal())
	}
	for _, ref := range tx.DeletedSecrets {
		exec(`DELETE FROM secrets WHERE ref=?`, string(ref))
	}
	for id, revision := range tx.Retired {
		exec(`INSERT INTO retired VALUES(?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision`, id, int64(revision))
	}
	for _, source := range tx.Sources {
		body, jerr := encodeSource(source)
		if jerr != nil {
			return jerr
		}
		exec(`INSERT INTO sources VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body`, string(source.ID), body)
	}
	for _, id := range tx.DeletedSources {
		exec(`DELETE FROM sources WHERE id=?`, string(id))
	}
	for id, fs := range tx.Fetches {
		body, merr := fs.MarshalBinary()
		if merr != nil {
			return merr
		}
		exec(`INSERT INTO fetch_state VALUES(?,?) ON CONFLICT(source) DO UPDATE SET body=excluded.body`, string(id), body)
	}
	for _, id := range tx.DeletedFetches {
		exec(`DELETE FROM fetch_state WHERE source=?`, string(id))
	}
	for _, l := range tx.Lineages {
		body, eerr := encodeLineage(l)
		if eerr != nil {
			return eerr
		}
		exec(`INSERT INTO lineages VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET source=excluded.source, presence=excluded.presence, country=excluded.country, body=excluded.body`,
			string(l.ID), string(l.Source), int(l.Presence), l.Country.Code, body)
	}
	for _, id := range tx.Collected {
		exec(`DELETE FROM lineages WHERE id=?`, string(id))
		exec(`DELETE FROM records WHERE lineage=?`, string(id))
	}
	for _, g := range tx.Groups {
		body, jerr := encodeGroup(g)
		if jerr != nil {
			return jerr
		}
		exec(`INSERT INTO groups VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision, body=excluded.body`,
			string(g.ID), int64(g.Revision), body)
	}
	for _, id := range tx.DeletedGroups {
		exec(`DELETE FROM groups WHERE id=?`, string(id))
	}
	for _, e := range tx.RefreshEvents {
		diagnostics, jerr := json.Marshal(e.Diagnostics)
		if jerr != nil {
			return jerr
		}
		var replaced []byte
		if len(e.Replaced) > 0 {
			if replaced, jerr = json.Marshal(e.Replaced); jerr != nil {
				return jerr
			}
		}
		exec(`INSERT INTO refresh_events(source, at, outcome, failure, wan_excluded, added, changed, missing, diagnostics, replaced) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			string(e.Source), e.At.UnixMilli(), int(e.Outcome), e.FailureCode, boolInt(e.WANExcluded), e.Added, e.Changed, e.Missing, diagnostics, replaced)
	}
	for _, row := range tx.Slots {
		intent, e1 := encodeIntent(row.Slot.Intent)
		allocation, e2 := encodeAllocation(row.Slot.Allocation)
		carries, e3 := encodeCarries(row)
		if jerr := errors.Join(e1, e2, e3); jerr != nil {
			return jerr
		}
		exec(`INSERT INTO slots VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET version=excluded.version, deleting=excluded.deleting, intent=excluded.intent, allocation=excluded.allocation, carries=excluded.carries, created=excluded.created`,
			string(row.Slot.ID), int64(row.Slot.Version), boolInt(row.Deleting), intent, allocation, carries, row.CreatedAt.UnixMilli())
	}
	for _, id := range tx.DeletedSlots {
		exec(`DELETE FROM slots WHERE id=?`, string(id))
		exec(`DELETE FROM pending WHERE slot=?`, string(id))
	}
	for slot, event := range tx.Pending {
		exec(`INSERT INTO pending VALUES(?,?) ON CONFLICT(slot) DO UPDATE SET event=excluded.event`, string(slot), string(event))
	}
	for _, l := range tx.Landings {
		var event any
		if l.Move != nil {
			event = string(l.Move.ID)
			exec(`DELETE FROM pending WHERE slot=?`, string(l.Slot))
		}
		exec(`INSERT INTO landings(slot, event, at) VALUES(?,?,?)`, string(l.Slot), event, l.At.UnixMilli())
	}
	for id, r := range tx.Records {
		body, eerr := encodeRecord(r)
		if eerr != nil {
			return eerr
		}
		exec(`INSERT INTO records VALUES(?,?) ON CONFLICT(lineage) DO UPDATE SET body=excluded.body`, string(id), body)
	}
	if tx.Ranking != nil {
		body, jerr := encodeRanking(*tx.Ranking)
		if jerr != nil {
			return jerr
		}
		exec(`INSERT INTO ranking VALUES(1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body`, body)
	}
	for owner, m := range tx.Owners {
		memory, eerr := encodeMemory(m)
		if eerr != nil {
			return eerr
		}
		exec(`INSERT INTO owners VALUES(?,?) ON CONFLICT(id) DO UPDATE SET memory=excluded.memory`, owner.String(), memory)
	}
	for _, owner := range tx.DeletedOwners {
		exec(`DELETE FROM owners WHERE id=?`, owner.String())
	}
	for _, sw := range tx.Switches {
		body, eerr := encodeSwitch(sw)
		if eerr != nil {
			return eerr
		}
		var applied any
		if sw.AppliedAt != nil {
			applied = sw.AppliedAt.UnixMilli()
		}
		exec(`INSERT INTO switch_events VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET applied_at=excluded.applied_at`,
			string(sw.ID), sw.Owner.String(), int64(sw.Intent), int(sw.Reason), sw.RequestedAt.UnixMilli(), applied, body)
	}
	if tx.Request != nil {
		var answer Answer
		if tx.Request.Answer != nil {
			answer = *tx.Request.Answer
		}
		exec(`INSERT INTO request_receipts VALUES(?,?,?,?,?,?,?)`, string(tx.Request.Request), tx.RequestDigest, int64(tx.Request.Revision), time.Now().UnixMilli(),
			answer.Status, answer.ETag, answer.Body)
	}
	if err != nil {
		return err
	}
	if err := t.Commit(); err != nil {
		return err
	}
	if tx.OwesErase {
		s.owes = vacuumOwed
	}
	return nil
}

// erase rebuilds the file and empties the WAL, so nothing a commit deleted is left in either. A
// reader whose snapshot is older than the rebuild keeps the checkpoint from finishing, which SQLite
// reports only in its result row.
func (s *store) erase(ctx context.Context) error {
	if s.owes == vacuumOwed {
		if _, err := s.writer.ExecContext(ctx, "VACUUM"); err != nil {
			return err
		}
		s.owes = checkpointOwed
	}
	var busy, logged, moved int
	if err := s.writer.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logged, &moved); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("a reader held the WAL's older frames past the busy timeout")
	}
	if _, err := s.writer.ExecContext(ctx, `DELETE FROM meta WHERE key=?`, eraseOwedKey); err != nil {
		return err
	}
	s.owes = nothingOwed
	return nil
}

// receipt finds an earlier answer under a request ID.
func (s *store) receipt(ctx context.Context, request domain.RequestID) (Receipt, []byte, bool, error) {
	return readReceipt(ctx, s.writer, request)
}

// receipt reads an earlier answer from the read-only pool. A probe checks here, since it
// takes seconds and must not hold up the controller.
func (q *queries) receipt(ctx context.Context, request domain.RequestID) (Receipt, []byte, bool, error) {
	return readReceipt(ctx, q.reader, request)
}

func readReceipt(ctx context.Context, db *sql.DB, request domain.RequestID) (Receipt, []byte, bool, error) {
	var digest, body []byte
	var revision int64
	var status int
	var etag string
	err := db.QueryRowContext(ctx, `SELECT digest, revision, status, etag, body FROM request_receipts WHERE request=?`, string(request)).Scan(&digest, &revision, &status, &etag, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, nil, false, nil
	}
	if err != nil {
		return Receipt{}, nil, false, err
	}
	r := Receipt{Request: request, Revision: domain.IntentRevision(revision)}
	if status != 0 {
		r.Answer = &Answer{Status: status, ETag: etag, Body: body}
	}
	return r, digest, true, nil
}

// trimEvents deletes events older than before in small batches, and receipts past the
// page's retry horizon. A landing older than before goes once a later one at or before it takes
// the slot elsewhere, and a removal once nothing before it is left, so each slot keeps where it was at
// before. A switch goes once no slot owes it and no landing names it. It never vacuums on the control path.
func (s *store) trimEvents(ctx context.Context, before time.Time) error {
	const batch = 500
	cut := before.UnixMilli()
	for _, q := range []struct {
		query string
		cut   int64
	}{
		{`DELETE FROM landings WHERE seq IN (SELECT seq FROM landings l WHERE at<?1 AND (event IS NULL AND NOT EXISTS (SELECT 1 FROM landings o WHERE o.slot=l.slot AND o.seq<l.seq)
			OR EXISTS (SELECT 1 FROM landings n WHERE n.slot=l.slot AND n.seq>l.seq AND n.at<=?1)) LIMIT ?2)`, cut},
		{`DELETE FROM switch_events WHERE id IN (SELECT id FROM switch_events WHERE requested_at<?1 AND id NOT IN (SELECT event FROM pending)
			AND id NOT IN (SELECT event FROM landings WHERE event IS NOT NULL) LIMIT ?2)`, cut},
		{`DELETE FROM refresh_events WHERE id IN (SELECT id FROM refresh_events WHERE at<?1 LIMIT ?2)`, cut},
		{`DELETE FROM request_receipts WHERE request IN (SELECT request FROM request_receipts WHERE at<?1 LIMIT ?2)`, time.Now().Add(-48 * time.Hour).UnixMilli()},
	} {
		for {
			r, err := s.writer.ExecContext(ctx, q.query, q.cut, batch)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n < batch {
				break
			}
		}
	}
	return nil
}

// namingColumns are every column that can name a server by its lineage ID, as a JSON string or
// a key: a group's picks, a slot's Exit and what it carries, an owner's memory, a switch's servers
// and evidence, and a refresh's replaced servers. A lineage's own row isn't one: its config keys
// borrow lineage IDs and name configs.
var namingColumns = [][2]string{{"groups", "body"}, {"slots", "intent"}, {"slots", "carries"}, {"owners", "memory"},
	{"switch_events", "body"}, {"refresh_events", "replaced"}}

func (s *store) named(ctx context.Context, ids []domain.LineageID) (map[domain.LineageID]bool, error) {
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	var parts []string
	for _, c := range namingColumns {
		parts = append(parts, fmt.Sprintf(`SELECT j.atom FROM %[1]s, json_tree(%[1]s.%[2]s) j WHERE j.type='text' AND j.atom IN (SELECT value FROM json_each(?1))
			UNION SELECT j.key FROM %[1]s, json_tree(%[1]s.%[2]s) j WHERE j.key IN (SELECT value FROM json_each(?1))`, c[0], c[1]))
	}
	out := map[domain.LineageID]bool{}
	err = scanAll(ctx, s.writer, strings.Join(parts, " UNION "), func(r *sql.Rows) error {
		var id string
		err := r.Scan(&id)
		out[domain.LineageID(id)] = true
		return err
	}, string(list))
	return out, err
}

func (s *store) close() error   { return s.writer.Close() }
func (q *queries) close() error { return q.reader.Close() }

// dimension is what analytics needs to group a lineage, tombstones included.
type dimension struct {
	Source  domain.SourceID
	Country string
}

func readRecords(ctx context.Context, db querier) (map[domain.LineageID]record, error) {
	out := map[domain.LineageID]record{}
	err := scanAll(ctx, db, `SELECT lineage, body FROM records`, func(r *sql.Rows) error {
		var id string
		var body []byte
		if err := r.Scan(&id, &body); err != nil {
			return err
		}
		rec, err := decodeRecord(body)
		if err != nil {
			return fmt.Errorf("record %s: %w", id, err)
		}
		out[domain.LineageID(id)] = rec
		return nil
	})
	return out, err
}

// dimensions reads the grouping of lineages, or of every lineage when lineages is empty.
func (q *queries) dimensions(ctx context.Context, lineages []domain.LineageID) (map[domain.LineageID]dimension, error) {
	query, args := `SELECT id, source, country FROM lineages`, []any{}
	if len(lineages) > 0 {
		list, err := json.Marshal(lineages)
		if err != nil {
			return nil, err
		}
		query, args = query+` WHERE id IN (SELECT value FROM json_each(?))`, append(args, string(list))
	}
	out := map[domain.LineageID]dimension{}
	err := scanAll(ctx, q.reader, query, func(r *sql.Rows) error {
		var id, source, country string
		if err := r.Scan(&id, &source, &country); err != nil {
			return err
		}
		out[domain.LineageID(id)] = dimension{domain.SourceID(source), country}
		return nil
	}, args...)
	return out, err
}

// forcedReasons are the reasons a server was left because it had to be: it died, it
// got severely worse, its UDP broke, or its provider withdrew it. A rotation, a recovery or an
// improvement is nobody's fault.
var forcedReasons = map[domain.Reason]bool{domain.FailureMove: true, domain.SlownessMove: true, domain.UDPMove: true, domain.ServerLeftFeed: true}

type eventKey[T comparable] struct {
	start int64
	id    T
}

type eventCounts struct {
	ForcedSwitchAways map[eventKey[domain.LineageID]]uint64 // Applied events only, not proposals.
}

// events counts state.db's switch-aways per step over [from, to).
func (q *queries) events(ctx context.Context, from, to time.Time, step time.Duration) (eventCounts, error) {
	stepMs := step.Milliseconds()
	counts := eventCounts{map[eventKey[domain.LineageID]]uint64{}}
	err := scanAll(ctx, q.reader, `SELECT id, owner, intent, reason, requested_at, applied_at, body FROM switch_events
		WHERE applied_at IS NOT NULL AND applied_at>=? AND applied_at<?`, func(r *sql.Rows) error {
		s, err := scanSwitch(r)
		if err != nil {
			return err
		}
		if !forcedReasons[s.Reason] {
			return nil
		}
		left, ok := s.From.Server()
		if !ok {
			return nil
		}
		if next, ok := s.To.Server(); !ok || next.Lineage != left.Lineage {
			counts.ForcedSwitchAways[eventKey[domain.LineageID]{s.AppliedAt.UnixMilli() / stepMs * stepMs, left.Lineage}]++
		}
		return nil
	}, from.UnixMilli(), to.UnixMilli())
	return counts, err
}

// switches lists the newest applied or pending switch events, newest first, and the moves of one
// millisecond in the order they were made.
func (q *queries) switches(ctx context.Context, limit int) ([]domain.Switch, error) {
	var out []domain.Switch
	err := scanAll(ctx, q.reader, `SELECT id, owner, intent, reason, requested_at, applied_at, body FROM switch_events
		ORDER BY requested_at DESC, `+eventSequenceSQL+` DESC LIMIT ?`, func(r *sql.Rows) error {
		s, err := scanSwitch(r)
		if err != nil {
			return err
		}
		out = append(out, s)
		return nil
	}, limit)
	return out, err
}

// switchesSince lists the applied switch events from since on, in no order.
func switchesSince(ctx context.Context, db querier, since time.Time) ([]domain.Switch, error) {
	var out []domain.Switch
	err := scanAll(ctx, db, `SELECT id, owner, intent, reason, requested_at, applied_at, body FROM switch_events WHERE applied_at>=?`, func(r *sql.Rows) error {
		s, err := scanSwitch(r)
		out = append(out, s)
		return err
	}, since.UnixMilli())
	return out, err
}

// secret reads one stored secret.
func (q *queries) secret(ctx context.Context, ref domain.SecretRef) (string, error) {
	var value string
	err := q.reader.QueryRowContext(ctx, `SELECT value FROM secrets WHERE ref=?`, string(ref)).Scan(&value)
	return value, err
}

// landingsSince lists the landings from since on, and each slot's newest before it, as they were made.
func landingsSince(ctx context.Context, db querier, since time.Time) ([]landing, error) {
	var out []landing
	err := scanAll(ctx, db, `SELECT l.slot, l.at, l.event, e.owner, e.intent, e.reason, e.requested_at, e.applied_at, e.body
		FROM landings l LEFT JOIN switch_events e ON e.id=l.event
		WHERE l.at>=?1 OR l.seq IN (SELECT MAX(seq) FROM landings WHERE at<?1 GROUP BY slot) ORDER BY l.seq`, func(r *sql.Rows) error {
		var slot string
		var at int64
		var event, owner sql.NullString
		var intent, reason, requested, applied sql.NullInt64
		var body []byte
		if err := r.Scan(&slot, &at, &event, &owner, &intent, &reason, &requested, &applied, &body); err != nil {
			return err
		}
		l := landing{Slot: domain.SlotID(slot), At: time.UnixMilli(at).UTC()}
		if event.Valid {
			if !owner.Valid {
				return fmt.Errorf("slot %s landed event %s, which is gone", slot, event.String)
			}
			move, err := switchOf(event.String, owner.String, intent.Int64, reason.Int64, requested.Int64, applied, body)
			if err != nil {
				return err
			}
			l.Move = &move
		}
		out = append(out, l)
		return nil
	}, since.UnixMilli())
	return out, err
}

// lastDiffs reads what each source's last refresh with a new body changed.
func (q *queries) lastDiffs(ctx context.Context) (map[domain.SourceID]refreshEvent, error) {
	out := map[domain.SourceID]refreshEvent{}
	err := scanAll(ctx, q.reader, `SELECT source, at, added, changed, missing, diagnostics FROM refresh_events e
		WHERE outcome=? AND at=(SELECT MAX(at) FROM refresh_events WHERE source=e.source AND outcome=e.outcome)`, func(r *sql.Rows) error {
		var e refreshEvent
		var id string
		var at int64
		var diagnostics []byte
		if err := r.Scan(&id, &at, &e.Added, &e.Changed, &e.Missing, &diagnostics); err != nil {
			return err
		}
		e.Source, e.At = domain.SourceID(id), time.UnixMilli(at).UTC()
		if err := json.Unmarshal(diagnostics, &e.Diagnostics); err != nil {
			return err
		}
		out[e.Source] = e
		return nil
	}, int(catalogue.Updated))
	return out, err
}

// refreshes lists one source's fetch outcomes, newest first.
func (q *queries) refreshes(ctx context.Context, source domain.SourceID, limit int) ([]RefreshEventDTO, error) {
	out := []RefreshEventDTO{}
	err := scanAll(ctx, q.reader, `SELECT at, outcome, failure, wan_excluded, added, changed, missing FROM refresh_events WHERE source=? ORDER BY at DESC, id DESC LIMIT ?`, func(r *sql.Rows) error {
		var e RefreshEventDTO
		var at int64
		var outcome catalogue.Outcome
		var failure catalogue.FailureCode
		if err := r.Scan(&at, &outcome, &failure, &e.WANExcluded, &e.Added, &e.Changed, &e.Missing); err != nil {
			return err
		}
		e.At, e.Outcome = time.UnixMilli(at).UTC(), outcomeNames[outcome]
		if failure != "" {
			e.FailureCode = &failure
		}
		out = append(out, e)
		return nil
	}, string(source), limit)
	return out, err
}

// replaced lists the servers refreshes replaced, newest first.
func (q *queries) replaced(ctx context.Context, limit int) ([]ReplacedEvent, error) {
	out := []ReplacedEvent{}
	err := scanAll(ctx, q.reader, `SELECT source, at, replaced FROM refresh_events WHERE replaced IS NOT NULL ORDER BY at DESC, id DESC LIMIT ?`, func(r *sql.Rows) error {
		var source string
		var at int64
		var body []byte
		if err := r.Scan(&source, &at, &body); err != nil {
			return err
		}
		var replaced []catalogue.Replaced
		if err := json.Unmarshal(body, &replaced); err != nil {
			return err
		}
		for _, e := range replaced {
			out = append(out, ReplacedEvent{Source: domain.SourceID(source), Server: e.Lineage, Label: catalogue.PlainLabel(e.Label), PreviousLabel: catalogue.PlainLabel(e.Previous), At: time.UnixMilli(at).UTC()})
		}
		return nil
	}, limit)
	return out[:min(len(out), limit)], err
}

// lineagePage reads one page of the catalogue in ID order.
func (q *queries) lineagePage(ctx context.Context, source domain.SourceID, country, after string, limit int) ([]domain.Lineage, error) {
	var out []domain.Lineage
	err := scanAll(ctx, q.reader, `SELECT id, source, body FROM lineages WHERE id>?
		AND (?='' OR source=?) AND (?='' OR country=?) ORDER BY id LIMIT ?`, func(r *sql.Rows) error {
		var id, source string
		var body []byte
		if err := r.Scan(&id, &source, &body); err != nil {
			return err
		}
		l, err := decodeLineage(domain.LineageID(id), domain.SourceID(source), body)
		if err != nil {
			return err
		}
		out = append(out, l)
		return nil
	}, after, string(source), string(source), country, country, limit)
	return out, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
