// Package daemon is the application boundary. Only its controller commits durable
// state to state.db. Other actors publish facts or accept immutable desired plans.
// Measurements go to metrics.db through the signal loop, never through here.
package daemon

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	"sidegate/internal/metrics"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

// Settings is what the owner sets apart from slots, sources and groups: how long analytics are
// kept, and whether the page checks for a newer release. A stored body decodes over DefaultSettings,
// so a field a later release added reads as its default. A source with no interval of its own or its
// provider's refreshes every catalogue.DefaultRefresh.
type Settings struct {
	AnalyticsRetentionDays int  `json:"analyticsRetentionDays"`
	CheckForUpdates        bool `json:"checkForUpdates"`
}

func DefaultSettings() Settings {
	return Settings{AnalyticsRetentionDays: 30, CheckForUpdates: true}
}

func (s Settings) validate() error {
	if s.AnalyticsRetentionDays < 1 || s.AnalyticsRetentionDays > 365 {
		return refuse(KindValidation, RefusedRetentionRange, "analyticsRetentionDays is outside 1..365.")
	}
	return nil
}

// budget is what probing may cost. Nothing the owner sets changes it. Four workers keep a search's
// periods with every swept server silent, each holding its worker the whole 4 s timeout.
var budget = probeBudget{Parallel: 4, AttemptsPerMinute: 24, HeartbeatsPerMinute: 60, SuspicionsPerMinute: 16, SuspicionBurst: 16, ImprovementsPerMinute: 6}

const (
	lineageGrace  = 72 * time.Hour
	fetchCapacity = 1
	evaluateEvery = 2 * time.Second
	publishEvery  = 5 * time.Second
	trimEvery     = 24 * time.Hour
	eraseRetry    = time.Minute
	eraseRetryMax = time.Hour
)

// App is the daemon. The page reads through its embedded Queries and Snapshot, and
// writes only through Execute.
type App struct {
	*Queries
	bootstrap Bootstrap
	lock      *os.File
	commands  chan envelope
	view      atomic.Pointer[View]
	published atomic.Pointer[published]
	control   *controller
	loop      *signalLoop
	writer    *metrics.Writer
	worker    *gateway.Worker
	fetcher   *catalogue.Fetcher
	manual    chan manualProbe
	feed      eventFeed
	page      fs.FS
	proof     []byte // Nil until the installer writes one, and then nobody is vouched for.
	updates   *updater
	release   *string
}

// Open recovers desired state and opens both stores. It performs no gateway mutation:
// Run starts the actors. A second daemon on the same data dir is refused.
func Open(ctx context.Context, b Bootstrap) (*App, error) {
	if b.DataDir == "" || b.RunDir == "" {
		return nil, errors.New("bootstrap needs a data dir and a run dir")
	}
	if len(b.ProbeTargets) == 0 || len(b.BaselineTargets) == 0 {
		return nil, errors.New("bootstrap needs probe and baseline targets")
	}
	if err := os.MkdirAll(b.DataDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(b.DataDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another daemon holds %s: %w", b.DataDir, err)
	}
	fail := func(err error) (*App, error) {
		lock.Close()
		return nil, err
	}
	st, q, err := openStore(ctx, b.DataDir)
	if err != nil {
		return fail(err)
	}
	state, recovered, err := st.recover(ctx, time.Now(), b.Timings.apply())
	if err != nil {
		st.close()
		q.close()
		return fail(fmt.Errorf("state.db: %w", err))
	}
	// Before metrics.db, so a start that fails there still pays the erase.
	if st.owes != nothingOwed {
		if err := st.erase(ctx); err != nil {
			log.Printf("state.db still holds what a delete forgot and owes %s, tried again once the daemon runs: %v", st.owes, err)
		}
	}
	options := metrics.DefaultOptions()
	options.Rollup = time.Duration(state.settings.AnalyticsRetentionDays) * 24 * time.Hour
	writer, err := metrics.OpenWriter(metricsPath(b.DataDir), options)
	if err != nil {
		st.close()
		q.close()
		return fail(err)
	}
	reader, err := metrics.OpenReader(metricsPath(b.DataDir), options)
	if err != nil {
		writer.Close()
		st.close()
		q.close()
		return fail(err)
	}
	worker, err := gateway.Open(gateway.Options{DataDir: b.DataDir, RunDir: b.RunDir, CloseFlows: closeFlows})
	if err != nil {
		writer.Close()
		reader.Close()
		st.close()
		q.close()
		return fail(err)
	}
	syncer := startSyncer(&b, state.acceptedKey)
	var rules []catalogue.CountryRule
	for _, r := range b.CountryRules {
		rules = append(rules, catalogue.CountryRule{Regex: r.Regex, Country: r.Country})
	}
	fetcher, err := catalogue.NewFetcher(catalogue.DefaultLimits(), catalogue.CountryPolicy{LabelRules: rules})
	if err != nil {
		writer.Close()
		reader.Close()
		st.close()
		q.close()
		return fail(err)
	}
	a := &App{
		Queries: &Queries{state: q, metrics: reader}, bootstrap: b, lock: lock, commands: make(chan envelope), writer: writer,
		worker: worker, fetcher: fetcher, manual: make(chan manualProbe), proof: loadProof(b.DataDir), release: runningRelease(b.DataDir),
		loop: newSignalLoop(writer, targetsOf(b.BaselineTargets), gateway.CoreLogPath(b.RunDir)),
	}
	a.SetBuild(Build{})
	a.control = newController(st, state, recovered, a, targetsOf(b.ProbeTargets))
	a.control.key.syncer = syncer
	if a.control.diffs, err = q.lastDiffs(ctx); err != nil {
		a.Close()
		return nil, fmt.Errorf("state.db: %w", err)
	}
	a.control.publish(time.Now())
	return a, nil
}

func targetsOf(specs []TargetSpec) []probeTarget {
	out := make([]probeTarget, 0, len(specs))
	for i, t := range specs {
		out = append(out, probeTarget{ID: domain.TargetID(fmt.Sprintf("t%d", i+1)), URL: t.URL, ExpectedStatus: t.Status})
	}
	return out
}

func metricsPath(dir string) string { return filepath.Join(dir, "metrics.db") }

// Run starts every actor and joins them on exit. A critical failure cancels the group.
// Exit leaves the core unit running and open gates open: a daemon restart or upgrade
// drops no flow, and the next Run adopts them.
func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := a.control
	goals := make(chan gateway.Plan, 1)
	reports := make(chan gateway.Report, 8)
	telemetry := make(chan gateway.Telemetry, 8)
	plans := make(chan observationPlan, 1)
	frames := make(chan signalFrame, 64)
	jobs := make(chan catalogue.Request, fetchCapacity)
	results := make(chan catalogue.Refresh, fetchCapacity)
	c.goals, c.plans, c.fetchJobs = goals, plans, jobs

	errs := make(chan error, 8)
	actors := 0
	start := func(name string, run func() error) {
		actors++
		go func() {
			err := run()
			if err != nil {
				err = fmt.Errorf("%s: %w", name, err)
			}
			errs <- err
		}()
	}
	// The writer outlives every actor that records, so the bytes of a download a stop cuts short
	// reach the disk and still count after the restart.
	writing, stopWriting := context.WithCancel(context.Background())
	defer stopWriting()
	wrote := make(chan error, 1)
	go func() { wrote <- a.writer.Run(writing) }()
	start("signals", func() error { return a.loop.run(ctx, plans, a.manual, telemetry, frames) })
	start("runtime", func() error { return a.worker.Run(ctx, goals, reports, telemetry) })
	start("fetch", func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case req := <-jobs:
				refresh := a.fetcher.Fetch(ctx, req)
				select {
				case results <- refresh:
				case <-ctx.Done():
					return nil
				}
			}
		}
	})
	start("http", func() error { return a.serveHTTP(ctx) })
	if a.keepsNginx(ctx) {
		start("nginx", func() error { return a.keepNginx(ctx) })
	}
	c.clientReports = make(chan clientReport, 1)
	syncer := c.key.syncer
	start("unifi", func() error { return runClients(ctx, syncer, c.syncers, c.wants, c.clientReports, time.After) })
	start("controller", func() error { return c.run(ctx, a.commands, results, frames, reports) })
	first := <-errs
	cancel()
	for range actors - 1 {
		if err := <-errs; first == nil {
			first = err
		}
	}
	stopWriting()
	if err := <-wrote; first == nil {
		first = err
	}
	return first
}

func (a *App) Close() error {
	err := firstError(a.control.store.close(), a.writer.Close(), a.Queries.Close())
	a.lock.Close()
	return err
}

// Execute hands one command to the controller and waits for its durable commit. A
// response means durable acceptance, not applied readiness.
func (a *App) Execute(ctx context.Context, command Command) (Receipt, error) {
	return a.send(ctx, envelope{command: command})
}

func (a *App) send(ctx context.Context, e envelope) (Receipt, error) {
	reply := make(chan commandResult, 1)
	e.reply = reply
	select {
	case a.commands <- e:
	case <-ctx.Done():
		return Receipt{}, ctx.Err()
	}
	select {
	case r := <-reply:
		return r.receipt, r.err
	case <-ctx.Done():
		return Receipt{}, ctx.Err()
	}
}

// SetPage names the built page Handler serves. Run's listener serves it too.
func (a *App) SetPage(page fs.FS) { a.page = page }

// SetBuild says what this build was linked as: its version, which api/session answers, and where a
// release updates from.
func (a *App) SetBuild(build Build) { a.updates = newUpdater(a.bootstrap.DataDir, build, a.release) }

// Snapshot returns the latest published view, which the controller replaced after its
// last commit, so a read after a 2xx shows that write or a later one.
func (a *App) Snapshot() View { return *a.view.Load() }

// Settings returns the stored settings and their revision. It reads what the last
// commit published.
func (a *App) Settings() SettingsView { return *a.control.settingsView.Load() }

// Servers lists one page of servers, marking what only the controller knows: what the running core
// refused, what the UDP checks say, and each server's record.
func (a *App) Servers(ctx context.Context, page PageQuery) (ServerPage, error) {
	out, err := a.Queries.Servers(ctx, page)
	if err != nil {
		return out, err
	}
	p := a.published.Load()
	w := p.world
	now := a.control.now()
	for i, item := range out.Items {
		out.Items[i].Health = healthOf(p.records[item.ID], p.site, now)
		out.Items[i].Supported = w.supported(item.ID)
		out.Items[i].UDP = udpNames[p.udp[item.ID]]
	}
	return out, nil
}

// Probe runs one manual probe of a lineage's current revision through the probe inbound. A server with
// no route gets one for the length of the test.
func (a *App) Probe(ctx context.Context, id domain.LineageID) (ProbeResult, error) {
	s, err := a.probe(ctx, id)
	if err != nil {
		return ProbeResult{}, err
	}
	return probeResultOf(s), nil
}

// probe is Probe's signal, with the ref and core epoch a press checks before it moves anything.
func (a *App) probe(ctx context.Context, id domain.LineageID) (domain.Signal, error) {
	ref, ok := a.published.Load().testRef(id)
	if !ok {
		return domain.Signal{}, &CommandError{Kind: KindNotFound, Reason: RefusedUntestable, Message: "The core can't run that server, or it left its source."}
	}
	m := manualProbe{Server: ref, Reply: make(chan domain.Signal, 1)}
	select {
	case a.manual <- m:
	case <-ctx.Done():
		return domain.Signal{}, ctx.Err()
	}
	select {
	case s := <-m.Reply:
		return s, nil
	case <-ctx.Done():
		return domain.Signal{}, ctx.Err()
	}
}

func probeResultOf(s domain.Signal) ProbeResult {
	return ProbeResult{Lineage: s.Server.Lineage, Verdict: verdictNames[s.Verdict], PingMs: millis(s.Ping), NewConnectionMs: millis(s.NewConn), At: s.Stamp.At}
}

// ClientConfig is what a slot's UniFi VPN Client is created with.
type ClientConfig struct {
	PrivateKey    domain.Secret
	HostPublicKey string
	Address       string
	Endpoint      string
}

// ClientConfigOf reads a slot's client side from the last commit.
func (a *App) ClientConfigOf(id domain.SlotID) (ClientConfig, bool) {
	configs := a.control.clients.Load()
	if configs == nil {
		return ClientConfig{}, false
	}
	c, ok := (*configs)[id]
	return c, ok
}

type Receipt struct {
	Request  domain.RequestID
	Revision domain.IntentRevision // The view revision the write was committed at, or for a refusal the current one.
	Answer   *Answer               // A page write's HTTP answer, committed with the write.
	Replayed bool                  // This answer is an earlier request's.
}

// Answer is what a page write answered. It lands in the write's transaction, so a crash
// can't keep one without the other, and it is what a replay of the key gets.
type Answer struct {
	Status int
	ETag   string
	Body   []byte
}

// pageWrite is one mutation from the page, under its Idempotency-Key, and what its handler decided.
type pageWrite struct {
	Key         domain.RequestID
	Fingerprint []byte
	Decision    pageDecision
}

// pageDecision is what a handler decided of a page write outside the controller: runs, a Command to
// run; an *Answer to store, such as a refusal of what it couldn't parse or a probe's result; or
// replays, the receipt it found under the key.
type pageDecision interface{ decision() }

type runs struct{ Command }

type replays struct{}

func (runs) decision()    {}
func (replays) decision() {}
func (*Answer) decision() {}

// decided is what a parse returned: its refusal, or else its command.
func decided(command Command, refusal *Answer) pageDecision {
	if refusal != nil {
		return refusal
	}
	return runs{command}
}

type envelope struct {
	command Command
	page    *pageWrite
	reply   chan commandResult // Capacity 1; a departed caller cannot block commit.
}

type commandResult struct {
	receipt Receipt
	err     error
}

type durableState struct {
	revision          domain.IntentRevision
	runtimeGeneration uint64
	catalogueRevision uint64
	settingsRevision  uint64
	acceptedKey       uint64 // The revision of the last commit that accepted a UniFi key.
	settings          Settings
	sources           map[domain.SourceID]domain.Source
	fetches           map[domain.SourceID]catalogue.FetchState
	slots             map[domain.SlotID]slotRow
	owners            map[domain.Owner]policyMemory   // Exactly the owners some slot follows.
	enabling          map[domain.SlotID]bool          // Clients the owner asked to turn back on, until a pass shows them on or gone.
	pending           map[domain.SlotID]domain.Switch // The move each slot's readback hasn't shown yet.
	secrets           map[domain.SecretRef]domain.Secret
	retired           map[string]uint64 // The last revision of each deleted id, so its next one is higher.
	records           map[domain.LineageID]record
	ranking           rankedSite
	lastEvent         uint64 // The largest sequence a stored switch's ID ends in.
}

func retiredSlot(id domain.SlotID) string     { return "slot/" + string(id) }
func retiredSource(id domain.SourceID) string { return "source/" + string(id) }
func retiredGroup(id domain.GroupID) string   { return "group/" + string(id) }

type controller struct {
	store     *store
	state     durableState
	served    usage              // The week of switches, read again after a switch and each minute.
	tests     []domain.ServerRef // Servers a Test holds, which get a route until it answers.
	servedAt  time.Time
	world     world
	app       *App
	targets   []probeTarget
	timings   timings
	queue     *catalogue.Queue
	goals     chan gateway.Plan
	plans     chan observationPlan
	fetchJobs chan catalogue.Request
	now       func() time.Time
	newKeys   func() (domain.SlotKeys, map[domain.SecretRef]domain.Secret, error)

	evidence   *evidence
	last       gateway.Report
	planKey    string
	observeKey string
	eventSeq   uint64  // Goes on from the largest state.db holds, so a later move sorts after every stored one of its millisecond.
	life       [6]byte // Random to this process, so no switch takes the ID of one an earlier process made, whatever the clock did.
	lives      uint32  // Owner memories begun, so each life's campaign IDs are its own.
	diffs      map[domain.SourceID]refreshEvent
	lastTrim   time.Time
	eraseAt    time.Time     // When an owed erase is next tried.
	eraseWait  time.Duration // How long the last failed try waited for this one.
	lastPub    time.Time
	behind     bool // A command committed since the view was published: its turn publishes it once evaluated.
	recordsAt  time.Time
	rankingDue bool // The ranking target changed since state.db last had it.

	wants          chan []unifi.Want
	syncers        chan unifi.Syncer // A new key's syncer, for the UniFi actor.
	clientReports  chan clientReport
	wantsKey       string
	clientsKnown   bool // A pass has read UniFi, so foreign and owned mean something.
	foreign        []unifi.ForeignClient
	owned          map[domain.SlotID]unifi.Owned
	clientTroubles map[domain.SlotID]ClientTrouble // From the last pass that read UniFi, by the slot each client serves or served.
	key            unifiKey

	changes      []ChangeEvent // Waiting for the publish that makes them readable.
	seenSlots    map[domain.SlotID]string
	seenSubs     map[domain.SourceID]string
	seenGroups   map[domain.GroupID]string
	seenServers  string
	seenClients  string
	seenSettings uint64
	seenBatches  uint64
	analyticsAt  time.Time

	settingsView atomic.Pointer[SettingsView]
	clients      atomic.Pointer[map[domain.SlotID]ClientConfig]
}

func newController(st *store, state durableState, recovered world, app *App, targets []probeTarget) *controller {
	c := &controller{
		store: st, state: state, world: recovered, app: app, targets: targets, timings: app.bootstrap.Timings.apply(), queue: catalogue.NewQueue(), now: time.Now,
		newKeys: generateKeys, evidence: newEvidence(targets[0].ID, sitesOf(targets)),
		diffs: map[domain.SourceID]refreshEvent{},
		owned: map[domain.SlotID]unifi.Owned{}, seenSettings: state.settingsRevision, eventSeq: state.lastEvent,
		wants: make(chan []unifi.Want, 1), syncers: make(chan unifi.Syncer, 1),
	}
	rand.Read(c.life[:]) // Never fails since Go 1.24.
	for _, o := range slices.SortedFunc(maps.Keys(state.owners), func(a, b domain.Owner) int { return strings.Compare(a.String(), b.String()) }) {
		c.lives++
		m := state.owners[o]
		m.Campaigns = c.lives << 16
		state.owners[o] = m
	}
	c.evidence.book.records = state.records
	c.evidence.rankAs(state.ranking, targets)
	for id, source := range state.sources {
		if _, remote := source.Origin.Remote(); remote {
			c.queue.Set(id, c.due(source, state.fetches[id]))
		}
	}
	return c
}

// generateKeys makes both WireGuard key pairs of a new slot, stored under refs named
// for the keys themselves, so nothing about a ref needs a slot to exist yet. Private
// keys are clamped as `wg genkey` clamps them, since the kernel reads a key back clamped
// and the runtime compares what it reads with what it wrote.
func generateKeys() (domain.SlotKeys, map[domain.SecretRef]domain.Secret, error) {
	pair := func() (private, public string, err error) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", "", err
		}
		raw[0] &= 248
		raw[31] = raw[31]&127 | 64
		k, err := ecdh.X25519().NewPrivateKey(raw)
		if err != nil {
			return "", "", err
		}
		return base64.StdEncoding.EncodeToString(raw), base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
	}
	hostPrivate, hostPublic, err := pair()
	if err != nil {
		return domain.SlotKeys{}, nil, err
	}
	clientPrivate, clientPublic, err := pair()
	if err != nil {
		return domain.SlotKeys{}, nil, err
	}
	keys := domain.SlotKeys{HostPrivate: domain.SecretRef("wg/" + hostPublic), ClientPrivate: domain.SecretRef("wg/" + clientPublic), HostPublic: hostPublic, ClientPublic: clientPublic}
	return keys, map[domain.SecretRef]domain.Secret{keys.HostPrivate: domain.NewSecret(hostPrivate), keys.ClientPrivate: domain.NewSecret(clientPrivate)}, nil
}

func (c *controller) due(source domain.Source, fs catalogue.FetchState) time.Time {
	remote, ok := source.Origin.Remote()
	if !ok {
		return catalogue.Never
	}
	return fs.Due(source.ID, remote)
}

func (c *controller) run(ctx context.Context, commands <-chan envelope, refreshes <-chan catalogue.Refresh,
	frames <-chan signalFrame, reports <-chan gateway.Report) error {
	tick := time.NewTicker(evaluateEvery)
	defer tick.Stop()
	if err := c.settle(ctx); err != nil {
		return err
	}
	for {
		var err error
		select {
		case <-ctx.Done():
			return nil
		case e := <-commands:
			if err := c.take(ctx, e); err != nil {
				return err
			}
			continue
		case r := <-refreshes:
			err = c.applyRefresh(ctx, r)
		case f := <-frames:
			c.ingest(f)
		case r := <-reports:
			err = c.applyReport(ctx, r)
		case r := <-c.clientReports:
			if r.From == c.key.syncer {
				err = c.applyClients(ctx, r)
			}
		case <-tick.C:
		}
		if err == nil {
			err = c.settle(ctx)
		}
		if err != nil {
			return err
		}
	}
}

// take runs one command and answers its caller once the turn has published the view at the answer's
// revision, after the evaluate, so a press's move is staged there. A failure before that publication
// answers no success: the receipt stays, and a resend under its key gets it once a restarted daemon has
// evaluated what it committed. The rest of the turn runs after the answer, and an answer that made no
// revision waits on none of it.
func (c *controller) take(ctx context.Context, e envelope) error {
	var result commandResult
	if e.page != nil {
		result.receipt, result.err = c.acceptPage(ctx, *e.page)
	} else {
		result.receipt, result.err = c.accept(ctx, e.command)
	}
	if !c.behind {
		e.reply <- result
		return c.settle(ctx)
	}
	now := c.now()
	if err := c.present(ctx, now); err != nil {
		e.reply <- commandResult{err: err}
		return err
	}
	e.reply <- result
	return c.upkeep(ctx, now)
}

// settle runs after every event: decide, commit, publish. Nothing here waits on another
// actor. A write failure stops the daemon rather than let memory and state.db part.
func (c *controller) settle(ctx context.Context) error {
	now := c.now()
	if err := c.present(ctx, now); err != nil {
		return err
	}
	return c.upkeep(ctx, now)
}

// present evaluates, and publishes what a command committed if the evaluate didn't, so every view at a
// command's revision holds what the evaluate made of it.
func (c *controller) present(ctx context.Context, now time.Time) error {
	if err := c.evaluate(ctx, now); err != nil {
		return err
	}
	if c.behind {
		c.publish(now)
	}
	return nil
}

// upkeep is the rest of settle, which no view at a command's revision waits for.
func (c *controller) upkeep(ctx context.Context, now time.Time) error {
	if err := c.sendPlans(ctx); err != nil {
		return err
	}
	if err := c.writeRecords(ctx, now); err != nil {
		return err
	}
	c.sendWants()
	c.startFetches(now)
	if c.store.owes != nothingOwed && !now.Before(c.eraseAt) {
		c.erase(ctx, now)
	}
	if now.Sub(c.lastTrim) >= trimEvery {
		days := time.Duration(c.state.settings.AnalyticsRetentionDays) * 24 * time.Hour
		if err := c.store.trimEvents(ctx, now.Add(-days)); err != nil {
			return err
		}
		if err := c.collect(ctx, now.Add(-days)); err != nil {
			return err
		}
		c.lastTrim = now
	}
	if now.Sub(c.lastPub) >= publishEvery {
		c.publish(now)
	}
	return nil
}

// collect deletes the servers deleted before cut, with their records, once nothing names them: no
// row of state.db, no result since cut, and no sample metrics.db retains, which analytics still shows.
// A failed read of metrics.db only defers that to the next trim, since metrics.db is measurements.
func (c *controller) collect(ctx context.Context, cut time.Time) error {
	var old []domain.LineageID
	for _, l := range c.world.catalogue.All() {
		if l.Presence == domain.Tombstone && !l.MissingSince.After(cut) && !c.evidence.book.records[l.ID].Last.After(cut) {
			old = append(old, l.ID)
		}
	}
	if len(old) == 0 {
		return nil
	}
	named, err := c.store.named(ctx, old)
	if err != nil {
		return err
	}
	sampled, err := c.app.Queries.metrics.Named(ctx, old)
	if err != nil {
		log.Printf("deleted servers are collected at the next trim, since metrics.db couldn't say which it names: %v", err)
		return nil
	}
	gone := slices.DeleteFunc(old, func(id domain.LineageID) bool { return named[id] || sampled[id] })
	if len(gone) == 0 {
		return nil
	}
	if err := c.store.commit(ctx, transaction{Collected: gone}); err != nil {
		return err
	}
	c.setWorld(c.world.with(worldChange{gone: gone}))
	c.evidence.book.forget(gone)
	c.change("catalogue", "", "")
	return nil
}

func (c *controller) ingest(f signalFrame) {
	if f.Ranking != "" && f.Ranking != c.evidence.ranking {
		log.Printf("ranking target moved from %s to %s", c.evidence.ranking, f.Ranking)
	}
	if c.evidence.rankBy(f.Ranking, c.now()) {
		c.rankingDue = true
	}
	c.tests = f.Tests
	c.evidence.ingest(f)
}

// evaluate asks each owner's policy what it holds and moves the slots that follow it, and commits
// both before any effect, whether that is a switch or only durable memory, since leave can set LeaveBy
// with no switch. The records a crash mustn't lose go in the same commit, so a move keeps its incident. One switch moves every slot that was on the owner's selection. A slot that came to
// the owner on another selection takes the owner's in a switch of its own. Only a move off its server
// ends a preference, so a new revision of that server keeps it.
func (c *controller) evaluate(ctx context.Context, now time.Time) error {
	var tx transaction
	var events []domain.Switch
	for _, f := range followings(c.state.slots) {
		before := c.state.owners[f.owner]
		d := decide(now, f.owner, c.world.Pool(f.exit), before, c.evidence, c.timings)
		m := d.Memory
		if next := m.Campaign; next != nil && next.Round == 1 && (before.Campaign == nil || before.Campaign.ID != next.ID) {
			log.Printf("confirm %s owner=%s path=%d cause=%s incumbent=%s rivals=%d evidence=%s", next.ID, f.owner, next.Path, causeNames[next.Cause], next.Incumbent.Lineage, len(next.Rivals), describeEvidence(next.Evidence))
		}
		if d.Switch != nil {
			if before.Unshown != nil {
				m = supersede(now, m, *before.Unshown, before.Selection)
			}
			m.Selection, m.Unshown = d.Switch.Selection, &ownerMove{From: before.Selection, Reason: d.Switch.Reason}
			c.chargeMove(before.Selection, *d.Switch, now)
		}
		m.Hand = m.Hand.step(now, settledOn{m.Selection})
		if pick, picked := before.Hand.queued(); picked && !m.KeepOut[pick.Lineage].IsZero() {
			m.KeepOut = maps.Clone(m.KeepOut)
			delete(m.KeepOut, pick.Lineage)
		}
		var moved []domain.SlotID
		for _, id := range f.slots {
			switch from := c.state.slots[id].Selection; {
			case from == m.Selection:
			case d.Switch != nil && from == before.Selection:
				moved = append(moved, id)
			default:
				events = append(events, c.newSwitch(now, f.owner, []domain.SlotID{id}, from, m.Selection, joinReason(from), nil, time.Time{}))
			}
		}
		if len(moved) > 0 {
			events = append(events, c.newSwitch(now, f.owner, moved, before.Selection, m.Selection, d.Switch.Reason, d.Switch.Evidence, before.LeaveBy))
		}
		if !sameDurable(before, m) {
			if tx.Owners == nil {
				tx.Owners = map[domain.Owner]policyMemory{}
			}
			tx.Owners[f.owner] = m
		}
		c.state.owners[f.owner] = m
	}
	for _, e := range events {
		tx.Switches = append(tx.Switches, e)
		for _, id := range e.Slots {
			row := c.state.slots[id].switched(c.state.pendingOf(id), e)
			row.Selection = e.To
			c.state.slots[id] = row
			tx.Slots = append(tx.Slots, row)
			if tx.Pending == nil {
				tx.Pending = map[domain.SlotID]domain.EventID{}
			}
			tx.Pending[id] = e.ID
		}
	}
	if len(tx.Owners) == 0 && len(tx.Slots) == 0 {
		return nil
	}
	tx.Records = c.evidence.book.due(false)
	if err := c.store.commit(ctx, tx); err != nil {
		return err
	}
	c.evidence.book.written(tx.Records)
	for _, e := range events {
		for _, id := range e.Slots {
			c.state.pending[id] = e
			c.change("switch", id, "")
		}
		log.Printf("switch %s owner=%s slots=%v reason=%s from=%s to=%s evidence=%s", e.ID, e.Owner, e.Slots, reasonNames[e.Reason], describe(e.From), describe(e.To), describeEvidence(e.Evidence))
	}
	c.publish(now)
	return nil
}

// chargeMove charges the server a failure or slowness move leaves with the incident the move confirmed.
// A closure confirms nothing against the server, since nobody passed.
func (c *controller) chargeMove(from domain.Selection, p proposal, now time.Time) {
	left, wasServing := from.Server()
	to, serving := p.Selection.Server()
	if !wasServing || !serving || to.Lineage == left.Lineage {
		return
	}
	switch p.Reason {
	case domain.FailureMove:
		c.evidence.book.charge(left.Lineage, firstFailed(p.Evidence, left.Lineage, now))
	case domain.SlownessMove:
		c.evidence.book.slow(left.Lineage, now)
	}
}

// writeRecords commits what of the records a crash mustn't lose, and once an hour everything else.
func (c *controller) writeRecords(ctx context.Context, now time.Time) error {
	if c.recordsAt.IsZero() {
		c.recordsAt = now
	}
	flush := now.Sub(c.recordsAt) >= recordFlushEvery
	if flush {
		c.recordsAt = now
	}
	due := c.evidence.book.due(flush)
	if len(due) == 0 && !c.rankingDue {
		return nil
	}
	tx := transaction{Records: due}
	if c.rankingDue {
		ranking := c.evidence.rankedSite()
		tx.Ranking = &ranking
	}
	if err := c.store.commit(ctx, tx); err != nil {
		return err
	}
	c.evidence.book.written(due)
	c.rankingDue = false
	return nil
}

func (c *controller) newSwitch(now time.Time, o domain.Owner, slots []domain.SlotID, from, to domain.Selection, reason domain.Reason, evidence []domain.EvidenceSummary, leaveBy time.Time) domain.Switch {
	return domain.Switch{ID: c.eventID(o, now), Owner: o, Slots: slots, Intent: c.revisionOf(o), From: from, To: to, Reason: reason, Evidence: evidence,
		RequestedAt: now, AllowUntil: c.allowUntil(now, slots, from, leaveBy, to)}
}

func (c *controller) eventID(o domain.Owner, now time.Time) domain.EventID {
	c.eventSeq++
	return domain.EventID(fmt.Sprintf("%s-%d-%x-%d", o, now.UnixMilli(), c.life, c.eventSeq))
}

func (c *controller) revisionOf(o domain.Owner) domain.IntentRevision {
	if id, ok := o.Group(); ok {
		return domain.IntentRevision(c.world.groups[id].Revision)
	}
	if id, ok := o.Source(); ok {
		return domain.IntentRevision(c.state.sources[id].Revision)
	}
	id, _ := o.Slot()
	return c.state.slots[id].Slot.Version
}

// sameDurable compares what state.db holds of an owner's memory, so evaluate commits only a change.
func sameDurable(a, b policyMemory) bool {
	return a.Selection == b.Selection && a.LastServed == b.LastServed && a.LeaveBy.Equal(b.LeaveBy) && a.Backoff.same(b.Backoff) && a.RefilledAt.Equal(b.RefilledAt) &&
		maps.EqualFunc(a.KeepOut, b.KeepOut, time.Time.Equal) && (a.Unshown == nil) == (b.Unshown == nil) && (a.Unshown == nil || *a.Unshown == *b.Unshown) &&
		a.Hand.sameStored(b.Hand)
}

// allowUntil fixes how long a move from a server may take, as the earliest of the handover limit,
// any running LeaveBy, and the deadline of a pending move this one supersedes on any of its slots. So
// neither a landing move nor a chain of moves that never lands can extend what the core may stay on.
func (c *controller) allowUntil(now time.Time, slots []domain.SlotID, from domain.Selection, leaveBy time.Time, to domain.Selection) time.Time {
	_, fromServer := from.Server()
	_, toServer := to.Server()
	if !fromServer || !toServer {
		return time.Time{}
	}
	until := now.Add(c.timings.Handover)
	if !leaveBy.IsZero() && leaveBy.Before(until) {
		until = leaveBy
	}
	for _, id := range slots {
		if p, ok := c.state.pending[id]; ok && !p.AllowUntil.IsZero() && p.AllowUntil.Before(until) {
			until = p.AllowUntil
		}
	}
	return until
}

func describe(s domain.Selection) string {
	switch {
	case s.IsDirect():
		return "direct"
	case s.IsBlocked():
		return "block"
	}
	if reason, down := s.Down(); down {
		return "down:" + downNames[reason]
	}
	server, _ := s.Server()
	return string(server.Lineage)
}

func describeEvidence(evidence []domain.EvidenceSummary) string {
	var parts []string
	for _, e := range evidence {
		ping := "-"
		if e.Ping != nil {
			ping = e.Ping.Round(time.Millisecond).String()
		}
		parts = append(parts, fmt.Sprintf("%s/%s/%s/r%d/%s", e.Server.Lineage, kindNames[e.Kind], verdictNames[e.Verdict], e.Round, ping))
	}
	return strings.Join(parts, " ")
}

// sendPlans publishes a new runtime plan and observation plan when either changed. Both
// channels hold the newest only, since a plan replaces the last one whole. The newest
// plan's generation is durable before the runtime sees it.
func (c *controller) sendPlans(ctx context.Context) error {
	plan, key, err := c.desired()
	if err != nil {
		return err
	}
	if key != c.planKey {
		c.state.runtimeGeneration++
		if err := c.store.commit(ctx, transaction{Meta: map[string]uint64{"runtime_generation": c.state.runtimeGeneration}}); err != nil {
			return err
		}
		plan, _, err = c.desired()
		if err != nil {
			return err
		}
		c.planKey = key
		replace(c.goals, plan)
	}
	if observation, key := c.observation(); key != c.observeKey {
		c.observeKey = key
		replace(c.plans, observation)
	}
	return nil
}

func replace[T any](ch chan T, value T) {
	select {
	case <-ch:
	default:
	}
	ch <- value
}

// desired turns the probe routes and each slot's selection into the runtime plan. Each slot's current
// server, its exact prior and each server a Test holds join the routes, so a serving server is never
// dropped and a Test has a route for its length.
func (c *controller) desired() (gateway.Plan, string, error) {
	servers := make(map[domain.ServerRef]domain.Server, len(c.world.routes))
	for ref, l := range c.world.routes {
		servers[ref] = l.Server
	}
	include := func(ref domain.ServerRef) bool {
		if _, have := servers[ref]; have {
			return true
		}
		l, ok := c.world.catalogue.Lookup(ref.Lineage)
		if !ok || l.Ref != ref || l.Presence == domain.Tombstone {
			return false
		}
		servers[ref] = l.Server
		return true
	}
	for _, ref := range c.tests {
		include(ref)
	}
	var targets []gateway.SlotTarget
	var removals []domain.Slot
	var key strings.Builder
	for _, id := range slices.Sorted(mapKeys(c.state.slots)) {
		row := c.state.slots[id]
		if row.Deleting {
			removals = append(removals, row.Slot)
			fmt.Fprintf(&key, "remove %s\n", id)
			continue
		}
		selection := row.Selection
		if server, ok := selection.Server(); ok && !include(server) {
			selection = domain.Closed(domain.NoUsableServer)
		}
		// A gate opens only for a slot whose client's _id is durable.
		if c.key.syncer != nil && row.Slot.Allocation.NetworkID == "" {
			selection = domain.Closed(domain.PlumbingUnavailable)
		}
		operation := domain.EventID("")
		pending := c.state.pendingOf(id)
		if pending != nil {
			operation = pending.ID
		}
		passage := passageOf(selection, pending, row.LastServed, c.memoryOf(row).LeaveBy)
		prior, until, allowed := passage.Allows(selection)
		if allowed {
			include(prior)
		}
		targets = append(targets, gateway.SlotTarget{
			Slot: row.Slot, Selection: selection, Passage: passage, Operation: operation, CloseFlows: closeOwed(row, pending),
			Keys: gateway.KeyMaterial{
				Host:   c.state.secrets[row.Slot.Allocation.Keys.HostPrivate],
				Client: c.state.secrets[row.Slot.Allocation.Keys.ClientPrivate],
			},
		})
		fmt.Fprintf(&key, "slot %s v%d %s op=%s", id, row.Slot.Version, describe(selection), operation)
		if allowed {
			fmt.Fprintf(&key, " prior=%s.%x until=%d", prior.Lineage, prior.Digest[:6], until.UnixMilli())
		}
		key.WriteByte('\n')
	}
	for _, ref := range slices.SortedFunc(mapKeys(servers), compareRefs) {
		fmt.Fprintf(&key, "server %s.%x\n", ref.Lineage, ref.Digest[:6])
	}
	plan, err := gateway.NewPlan(c.state.runtimeGeneration, targets, servers, removals)
	return plan, key.String(), err
}

// passageOf is how the plan lets the core reach selection: from the server readback last showed
// while a move onto another server is pending, or on a server the Exit no longer allows until leaveBy.
// It is the server readback last showed, not the pending move's From, because a second move can
// supersede one that hasn't landed.
func passageOf(selection domain.Selection, pending *domain.Switch, lastServed domain.ServerRef, leaveBy time.Time) gateway.Passage {
	to, serving := selection.Server()
	switch {
	case !serving:
		return gateway.Passage{}
	case pending != nil && !pending.AllowUntil.IsZero() && lastServed != (domain.ServerRef{}) && lastServed != to:
		return gateway.MoveFrom(lastServed, pending.AllowUntil)
	case !leaveBy.IsZero():
		return gateway.BridgeUntil(leaveBy)
	}
	return gateway.Passage{}
}

// observation is what signals may measure now: what the runtime verified, attributed to
// providers by the catalogue. Every probe route is on it, and so is each slot's current server.
func (c *controller) observation() (observationPlan, string) {
	now := c.now()
	p := observationPlan{
		Generation: c.state.runtimeGeneration, Topology: c.last.Topology, Sources: map[domain.ServerRef]domain.SourceID{},
		Serving: map[domain.ServerRef]bool{}, Endpoints: map[string][]domain.ServerRef{}, Targets: c.targets, Ranking: c.evidence.ranking, Budget: budget, UDPEvery: c.timings.UDPCheckEvery,
	}
	var key strings.Builder
	fmt.Fprintf(&key, "epoch=%s pid=%d budget=%+v\n", c.last.Topology.Epoch, c.last.Topology.CorePID, p.Budget)
	attribute := func(ref domain.ServerRef, l domain.Lineage) {
		p.Sources[ref] = l.Source
		root, hops := l.Server.Closure()
		for _, h := range hops {
			if h.ID == root {
				endpoint := fmt.Sprintf("%s:%d", h.Endpoint.Host, h.Endpoint.Port)
				if !slices.Contains(p.Endpoints[endpoint], ref) {
					p.Endpoints[endpoint] = append(p.Endpoints[endpoint], ref)
				}
			}
		}
	}
	for ref, l := range c.world.routes {
		attribute(ref, l)
	}
	for _, ref := range c.tests {
		if l, ok := c.world.catalogue.Lookup(ref.Lineage); ok && l.Ref == ref {
			attribute(ref, l)
		}
	}
	for _, row := range c.state.slots {
		if server, serving := row.Selection.Server(); serving {
			p.Serving[server] = true
			if l, ok := c.world.catalogue.Lookup(server.Lineage); ok {
				attribute(server, l)
			}
		}
	}
	var owners []evaluation
	for _, f := range followings(c.state.slots) {
		owners = append(owners, evaluation{now: now, who: f.owner, pool: c.world.Pool(f.exit), memory: c.state.owners[f.owner], ev: c.evidence, t: c.timings})
	}
	probes := planProbes(owners)
	p.Beats, p.Checks, p.Rounds, p.UDPRivals = probes.Beats, probes.Checks, probes.Rounds, probes.UDPRivals
	for _, ref := range slices.SortedFunc(mapKeys(p.UDPRivals), compareRefs) {
		fmt.Fprintf(&key, "udp %s.%x", ref.Lineage, ref.Digest[:6])
		for _, rival := range p.UDPRivals[ref] {
			fmt.Fprintf(&key, " %s.%x", rival.Lineage, rival.Digest[:6])
		}
		key.WriteByte('\n')
	}
	for _, ref := range slices.SortedFunc(mapKeys(p.Checks), compareRefs) {
		fmt.Fprintf(&key, "check %s.%x due=%d\n", ref.Lineage, ref.Digest[:6], p.Checks[ref].UnixMilli())
	}
	for _, r := range p.Rounds {
		fmt.Fprintf(&key, "round %s/%d path=%d due=%d passive=%t target=%s %s.%x", r.Campaign, r.Round, r.Path, r.Due.UnixMilli(), r.Passive, r.Target, r.Incumbent.Lineage, r.Incumbent.Digest[:6])
		for _, rival := range r.Rivals {
			fmt.Fprintf(&key, " %s.%x", rival.Lineage, rival.Digest[:6])
		}
		key.WriteByte('\n')
	}
	for ref := range p.Endpoints {
		slices.SortFunc(p.Endpoints[ref], compareRefs)
	}
	for _, ref := range slices.SortedFunc(mapKeys(p.Sources), compareRefs) {
		fmt.Fprintf(&key, "src %s.%x=%s\n", ref.Lineage, ref.Digest[:6], p.Sources[ref])
	}
	for _, ref := range slices.SortedFunc(mapKeys(p.Beats), compareRefs) {
		fmt.Fprintf(&key, "beat %s.%x=%s serving=%t\n", ref.Lineage, ref.Digest[:6], p.Beats[ref], p.Serving[ref])
	}
	for _, r := range c.last.Topology.Probes {
		fmt.Fprintf(&key, "route %s.%x\n", r.Server.Lineage, r.Server.Digest[:6])
	}
	for _, u := range c.last.Topology.Upstreams {
		fmt.Fprintf(&key, "up %s=%s.%x\n", u.Remote, u.Server.Lineage, u.Server.Digest[:6])
	}
	return p, key.String()
}

// applyReport takes the runtime's readback: what is applied, which close each slot's Worker last
// attempted, what the core refused, which core is running. A slot shows the move it owes only in a report for that move's own operation, so a
// report about a superseded move updates what is observed and nothing else. The clocks start on the
// owner the slot follows now, which need not be the one that proposed the move. Every slot of one
// report reads the memory from before it, so however many show the move, its clocks start once.
func (c *controller) applyReport(ctx context.Context, r gateway.Report) error {
	if r.Error != "" {
		log.Printf("runtime: %s", r.Error)
	}
	for _, step := range r.Steps {
		log.Printf("runtime step: %s", step)
	}
	if r.Topology.Epoch != c.evidence.epoch {
		c.evidence.coreRestarted(r.Topology.Epoch)
	}
	coreChanged := r.Core != c.last.Core
	c.last = r
	refused := map[domain.ServerRef]bool{}
	for _, x := range r.Refused {
		refused[x.Server] = true
	}
	refusedChanged := !equalSets(refused, c.world.refused)
	if refusedChanged {
		c.setWorld(c.world.with(worldChange{refused: refused}))
	}
	now := c.now()
	var tx transaction
	for _, s := range r.Slots {
		row, ok := c.state.slots[s.Slot]
		if !ok {
			continue
		}
		dirty := false
		if server, serving := s.Applied.Server(); serving && row.LastServed != server {
			row.LastServed = server
			dirty = true
		}
		if next := row.acknowledged(c.state.pendingOf(s.Slot), s.FlowsClosed); next.Closed != row.Closed {
			row, dirty = next, true
		}
		if p, ok := c.state.pending[s.Slot]; ok && s.Operation == p.ID && reportsApplied(p, s) {
			if next := row.landed(p); next.Owes != row.Owes {
				row, dirty = next, true
			}
			tx.Landings = append(tx.Landings, landing{Slot: s.Slot, Move: &p, At: now})
			if p.AppliedAt == nil {
				at := now
				p.AppliedAt = &at
				tx.Switches = append(tx.Switches, p)
				for id, other := range c.state.pending {
					if other.ID == p.ID {
						c.state.pending[id] = p
					}
				}
				log.Printf("applied %s owner=%s after %s", p.ID, p.Owner, now.Sub(p.RequestedAt).Round(time.Millisecond))
			}
			o, _ := ownerOf(row)
			if m, held := c.state.owners[o]; held && m.Unshown != nil && m.Selection == p.To {
				m = appliedMemory(now, m, domain.Switch{From: m.Unshown.From, To: m.Selection, Reason: m.Unshown.Reason}, c.timings)
				m.Unshown, m.ReadThrough, m.UDPReadThrough = nil, c.evidence.heard, c.evidence.heard
				if tx.Owners == nil {
					tx.Owners = map[domain.Owner]policyMemory{}
				}
				tx.Owners[o] = m
			}
			c.change("switch", s.Slot, "")
		}
		if dirty {
			c.state.slots[s.Slot] = row
			tx.Slots = append(tx.Slots, row)
		}
	}
	for _, id := range r.Deleted {
		row, ok := c.state.slots[id]
		if !ok {
			continue
		}
		if !row.Deleting {
			// Made again before its removal was reported: the old one's traffic stopped before anything
			// the new one has landed in this report.
			tx.Landings = append([]landing{{Slot: id, At: now}}, tx.Landings...)
			continue
		}
		tx.DeletedSlots = append(tx.DeletedSlots, id)
		if tx.Retired == nil {
			tx.Retired = map[string]uint64{}
		}
		tx.Retired[retiredSlot(id)] = uint64(row.Slot.Version)
		tx.Landings = append(tx.Landings, landing{Slot: id, At: now})
		tx.DeletedSecrets = append(tx.DeletedSecrets, row.Slot.Allocation.Keys.HostPrivate, row.Slot.Allocation.Keys.ClientPrivate)
	}
	if len(tx.Slots) == 0 && len(tx.Landings) == 0 && len(tx.Switches) == 0 {
		if refusedChanged || coreChanged {
			c.publish(now)
		}
		return nil
	}
	if err := c.store.commit(ctx, tx); err != nil {
		return err
	}
	maps.Copy(c.state.owners, tx.Owners)
	for _, l := range tx.Landings {
		if l.Move != nil {
			delete(c.state.pending, l.Slot)
		}
	}
	for _, id := range tx.DeletedSlots {
		c.state.retired[retiredSlot(id)] = tx.Retired[retiredSlot(id)]
		delete(c.state.slots, id)
		delete(c.state.pending, id)
	}
	c.publish(now)
	return nil
}

func equalSets[K comparable](a, b map[K]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// reportsApplied says readback shows p's target: a closed gate for a closure, or an open
// gate on exactly its server, the WAN or the drop.
func reportsApplied(p domain.Switch, s gateway.SlotReport) bool {
	if _, down := p.To.Down(); down {
		return s.Phase == gateway.Closed
	}
	return s.Phase == gateway.Ready && s.Applied == p.To
}

// startFetches hands out what is due. Nothing is fetched until the WAN is known up, since a
// failure before then says nothing about the provider, unless no probe route exists: then only a
// baseline can prove the WAN, and baselines a network blocks would hold the first fetch forever.
func (c *controller) startFetches(now time.Time) {
	if !c.evidence.wan.up() && len(c.world.routes) > 0 {
		return
	}
	free := cap(c.fetchJobs) - len(c.fetchJobs)
	if free <= 0 {
		return
	}
	due, _ := c.queue.Next(now, free)
	for _, id := range due {
		source, ok := c.state.sources[id]
		remote, isRemote := source.Origin.Remote()
		if !ok || !isRemote {
			c.queue.Finished(id)
			continue
		}
		basis := catalogue.Basis{Source: id, Revision: source.Revision}
		c.fetchJobs <- catalogue.NewRequest(basis, remote.UserAgent, c.state.secrets[remote.Location], c.state.secrets[remote.HWID], c.state.fetches[id])
	}
}

// applyRefresh commits a fetch's lineage changes, the source's feed, next fetch state and
// event in one transaction, so a crash never keeps a validator whose body was not
// applied. A refresh of a deleted source or an older revision is dropped, and the deadline
// its save set meanwhile stands. A failure the WAN explains, because the WAN wasn't known up
// at some moment since the fetch began, leaves the fetch state alone, so no provider is
// backed off for it, and tries again soon.
func (c *controller) applyRefresh(ctx context.Context, r catalogue.Refresh) error {
	id := r.Basis.Source
	defer c.queue.Finished(id)
	source, ok := c.state.sources[id]
	if !ok || source.Revision != r.Basis.Revision {
		return nil
	}
	now := c.now()
	st := &stage{next: c.world, tx: transaction{Fetches: map[domain.SourceID]catalogue.FetchState{id: r.Next}, Meta: map[string]uint64{}}}
	event := refreshEvent{Source: id, At: now, Outcome: r.Outcome}
	switch r.Outcome {
	case catalogue.Updated:
		parsed := source
		c.applyDocument(st, &parsed, r.Document, now, &event)
		st.tx.Sources = []domain.Source{parsed}
		source = parsed
	case catalogue.FetchFailed:
		event.FailureCode = r.Next.Status().FailureCode
		event.WANExcluded = !c.evidence.wan.upThroughout(c.queue.Since(id))
	}
	st.tx.RefreshEvents = []refreshEvent{event}
	if event.WANExcluded {
		st.tx.Fetches = nil
	}
	if err := c.store.commit(ctx, st.tx); err != nil {
		return err
	}
	if event.WANExcluded {
		c.queue.Set(id, now.Add(time.Minute))
		return nil
	}
	c.state.fetches[id] = r.Next
	if r.Outcome == catalogue.Updated {
		c.state.sources[id] = source
		c.setWorld(st.next)
		st.finish()
	}
	c.queue.Set(id, c.due(source, r.Next))
	c.publish(now)
	return nil
}

func stale(revision uint64) error {
	return &CommandError{Kind: KindStale, Reason: RefusedStale, Message: "Changed by another editor.", Revision: revision}
}

// accept handles one command. The request ID deduplicates a retry after a caller gave
// up, and the same ID with different content is refused. Every write lands in one
// transaction with its receipt, and its turn republishes the view before the caller hears.
func (c *controller) accept(ctx context.Context, command Command) (Receipt, error) {
	sum := sha256.Sum256([]byte(command.digest()))
	digest := sum[:]
	request := command.request()
	if request != "" {
		earlier, stored, found, err := c.store.receipt(ctx, request)
		if err != nil {
			return Receipt{}, err
		}
		if found {
			if !slices.Equal(stored, digest) {
				return Receipt{}, refuse(KindValidation, RefusedKeyReused, keyReused)
			}
			return earlier, nil
		}
	}
	return c.perform(ctx, command, request, digest, false)
}

const keyReused = "That key was used for a different action."

// acceptPage is accept for the page: a key's first answer, refusals included, is stored
// with what it did and is all a replay gets. The refusal of a key reused for another
// request isn't remembered, since the key still belongs to its first.
func (c *controller) acceptPage(ctx context.Context, w pageWrite) (Receipt, error) {
	earlier, stored, found, err := c.store.receipt(ctx, w.Key)
	if err != nil {
		return Receipt{}, err
	}
	if found {
		if !slices.Equal(stored, w.Fingerprint) || earlier.Answer == nil {
			return Receipt{Revision: c.state.revision}, refuse(KindValidation, RefusedKeyReused, keyReused)
		}
		earlier.Replayed = true
		return earlier, nil
	}
	switch d := w.Decision.(type) {
	case *Answer:
		return c.remember(ctx, w.Key, w.Fingerprint, d)
	case runs:
		return c.perform(ctx, d.Command, w.Key, w.Fingerprint, true)
	}
	// The 48 h trim removed the receipt since its handler read it. Nothing is stored, so a resend
	// decides afresh.
	return Receipt{}, errors.New("the receipt to replay was trimmed")
}

func (c *controller) remember(ctx context.Context, key domain.RequestID, fingerprint []byte, answer *Answer) (Receipt, error) {
	receipt := Receipt{Request: key, Revision: c.state.revision, Answer: answer}
	if err := c.store.commit(ctx, transaction{Request: &receipt, RequestDigest: fingerprint}); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// perform runs one command: it stages what the command would make true, renders the answer from
// that, commits both, and only then makes them current. An uncommitted world is a discarded value.
// The view waits for the turn's settle, so none shows this revision without what its evaluate makes
// of the command.
func (c *controller) perform(ctx context.Context, command Command, request domain.RequestID, digest []byte, page bool) (Receipt, error) {
	now := c.now()
	// Every command that runs moves the view's revision, whatever it changed, so a view at the revision its
	// answer names shows it: the page settles a write on that number alone.
	revision := c.state.revision + 1
	s := &stage{now: now, next: c.world, tx: transaction{RequestDigest: digest, Meta: map[string]uint64{"revision": uint64(revision)}},
		receipt: Receipt{Request: request, Revision: revision}}
	if request != "" {
		s.tx.Request = &s.receipt
	}
	if err := command.perform(c, s); err != nil {
		var refusal *CommandError
		if page && errors.As(err, &refusal) {
			return c.remember(ctx, request, digest, refusal.answer())
		}
		return Receipt{}, err
	}
	if page {
		status, body := command.answer(c, s)
		encoded, err := json.Marshal(body)
		if err != nil {
			s.abandon()
			return Receipt{}, err
		}
		s.receipt.Answer = &Answer{Status: status, Body: encoded}
	}
	if err := c.store.commit(ctx, s.tx); err != nil {
		s.abandon()
		return Receipt{}, err
	}
	c.state.revision = revision
	c.setWorld(s.next)
	s.finish()
	c.behind = true
	if s.tx.OwesErase {
		c.erase(ctx, now)
	}
	return s.receipt, nil
}

// erase pays what a commit that forgot credentials owes. That commit has landed, so a failure is
// only logged. Each failed try waits twice as long as the last for the next, up to eraseRetryMax,
// since a reader can hold the checkpoint up for as long as it likes.
func (c *controller) erase(ctx context.Context, now time.Time) {
	if err := c.store.erase(ctx); err != nil {
		c.eraseWait = min(max(2*c.eraseWait, eraseRetry), eraseRetryMax)
		c.eraseAt = now.Add(c.eraseWait)
		log.Printf("state.db still holds what a delete forgot and owes %s, tried again in %s: %v", c.store.owes, c.eraseWait, err)
		return
	}
	c.eraseWait = 0
}

// publish replaces the immutable view the page reads. Its derived fields are computed
// here from current observations, never stored.
func (c *controller) publish(now time.Time) {
	c.lastPub, c.behind = now, false
	view := View{Revision: c.state.revision, GeneratedAt: now, Sources: []SourceView{}, Groups: []GroupView{},
		WAN: wanNames[c.evidence.wan.State], VPNClients: c.vpnClients(), Core: coreView(c.last.Core)}
	if !c.key.answered.IsZero() {
		at := c.key.answered
		view.UniFi.AcceptedAt = &at
	}
	view.UniFiTrouble = c.key.shown()
	clients := map[domain.SlotID]ClientConfig{}
	for id, row := range c.state.slots {
		a := row.Slot.Allocation
		clients[id] = ClientConfig{PrivateKey: c.state.secrets[a.Keys.ClientPrivate], HostPublicKey: a.Keys.HostPublic,
			Address: a.Tunnel.Addr().Next().Next().String(), Endpoint: fmt.Sprintf("127.0.0.1:%d", a.PeerListenPort)}
	}
	view.Slots = c.slotViews(now, c.world)
	names := sourceNames(c.state.sources, c.state.fetches)
	for _, id := range slices.Sorted(mapKeys(c.state.sources)) {
		source := c.state.sources[id]
		var diff *refreshEvent
		if d, ok := c.diffs[id]; ok {
			diff = &d
		}
		view.Sources = append(view.Sources, sourceView(source, names[id], c.state.fetches[id], c.world, c.due(source, c.state.fetches[id]), diff, c.facts(domain.SourceOwner(id), domain.SourceExit(id), c.world, now, view.Slots)))
	}
	for _, id := range slices.Sorted(mapKeys(c.world.groups)) {
		view.Groups = append(view.Groups, groupView(c.world.groups[id], c.world, c.facts(domain.GroupOwner(id), domain.GroupExit(id), c.world, now, view.Slots)))
	}
	c.app.view.Store(&view)
	c.app.published.Store(&published{world: c.world, sources: maps.Clone(c.state.sources), slots: maps.Clone(c.state.slots), udp: c.evidence.udpStates(),
		records: c.evidence.book.snapshot(), site: c.evidence.book.sites[c.evidence.ranking]})
	settings := SettingsView{Settings: c.state.settings, Revision: c.state.settingsRevision}
	c.settingsView.Store(&settings)
	c.clients.Store(&clients)
	c.noteChanges(view, now)
}

// slotViews is every slot but one being deleted, as the runtime last reported it, in ID order.
func (c *controller) slotViews(now time.Time, w world) []SlotView {
	reports := map[domain.SlotID]gateway.SlotReport{}
	for _, s := range c.last.Slots {
		reports[s.Slot] = s
	}
	views := []SlotView{}
	for _, id := range slices.Sorted(mapKeys(c.state.slots)) {
		if row := c.state.slots[id]; !row.Deleting {
			views = append(views, c.slotView(row, c.memoryOf(row), reports[id], now, w))
		}
	}
	return views
}

// slotView reads through w and the owner's memory, so a command can render from what it is about to
// commit.
func (c *controller) slotView(row slotRow, memory policyMemory, report gateway.SlotReport, now time.Time, w world) SlotView {
	selection := row.Selection
	intent := row.Slot.Intent
	allowed := w.Pool(intent.Exit())
	e := evaluation{now: now, pool: allowed, memory: memory, ev: c.evidence, t: c.timings}
	status, down := e.slotState(intent, selection)
	view := SlotView{
		CreatedAt: row.CreatedAt, ID: row.Slot.ID, Version: row.Slot.Version, Name: intent.Name(), Exit: exitDTO(intent.Exit()),
		Status: status, DownReason: down, Server: lineageOf(selection), AppliedServer: lineageOf(report.Applied), Backups: []domain.LineageID{},
		Runtime: SlotReportDTO{Phase: phaseNames[report.Phase], Applied: selectionDTO(report.Applied), NetworkEstablished: report.NetworkEstablished},
		Unifi:   c.slotUnifi(row),
	}
	if !memory.LeaveBy.IsZero() {
		at := memory.LeaveBy
		view.LeavingBy = &at
	}
	if id := row.Slot.Allocation.NetworkID; id != "" {
		s := string(id)
		view.Runtime.NetworkID = &s
	}
	if report.ErrorCode != "" {
		code := report.ErrorCode
		view.Runtime.ErrorCode = &code
	}
	_, view.Pending = c.state.pending[row.Slot.ID]
	current, _ := selection.Server()
	for _, l := range e.ranked(allowed.members) {
		if l.ID != current.Lineage {
			view.Backups = append(view.Backups, l.ID)
		}
	}
	return view
}
