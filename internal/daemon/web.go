package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	urlpath "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	"sidegate/internal/metrics"
	"sidegate/internal/unifi"
)

// AuthHeader is how nginx says this request passed UniFi's login. Its hook clears
// whatever the browser sent and sets it from the auth subrequest, so it is empty if that
// subrequest never ran: a changed auth contract closes the page rather than opening it.
//
// ProofHeader says the speaker is that hook, since a process allowed on the socket, such
// as any of nginx's user, could send AuthHeader itself. Its value is ProofPath's, which
// only root can read, in a hook file only nginx's master can read.
//
// CSRFHeader carries the daemon's own CSRF token on a write. UniFi's login runs a CSRF
// check of its own in front, on X-Csrf-Token, which the hook clears before the daemon. Ours
// stays, since UniFi's doesn't run for an API key or on a console with it turned off.
//
// RevisionHeader is on every answer to a write that takes a key: the view revision the
// write was committed at, which every command that runs moves, and the first
// answer's on a replay. The page settles a write once api/state's revision reaches it.
const (
	AuthHeader     = "X-Sidegate-Auth"
	ProofHeader    = "X-Sidegate-Proof"
	CSRFHeader     = "X-Sidegate-Csrf"
	RevisionHeader = "Sidegate-Revision"
)

const (
	maxBody       = 64 << 10
	maxSourceBody = 1 << 20 // A pasted document can be bigger than a form: the Acme shape alone is 53 KB.
	enqueueLimit  = 5 * time.Second
)

// ChangeEvent says which reads went stale. See web/API.md.
type ChangeEvent struct {
	ID       Cursor                `json:"id"`
	At       time.Time             `json:"at"`
	Revision domain.IntentRevision `json:"revision"`
	Kind     string                `json:"kind"`
	Slot     *domain.SlotID        `json:"slot"`
	Source   *domain.SourceID      `json:"source"`
	Resync   bool                  `json:"resync"`
}

// eventFeed fans invalidations out to open pages. It never blocks a commit: a client
// that falls behind is closed, and the page's reconnect fetches everything again.
type eventFeed struct {
	mu      sync.Mutex
	seq     uint64
	clients map[chan ChangeEvent]struct{}
}

const feedQueue = 32

func (f *eventFeed) emit(e ChangeEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	e.ID = Cursor(strconv.FormatUint(f.seq, 10))
	for client := range f.clients {
		select {
		case client <- e:
		default:
			delete(f.clients, client)
			close(client)
		}
	}
}

func (f *eventFeed) join() chan ChangeEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clients == nil {
		f.clients = map[chan ChangeEvent]struct{}{}
	}
	client := make(chan ChangeEvent, feedQueue)
	f.clients[client] = struct{}{}
	return client
}

func (f *eventFeed) leave(client chan ChangeEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.clients, client)
}

func (f *eventFeed) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for client := range f.clients {
		delete(f.clients, client)
		close(client)
	}
}

func (f *eventFeed) size() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.clients)
}

// Events is a stream of changes until ctx ends or the client falls behind. The page
// fetches everything after any reconnect, so nothing replays from an old cursor.
func (a *App) Events(ctx context.Context) <-chan ChangeEvent {
	client := a.feed.join()
	context.AfterFunc(ctx, func() { a.feed.leave(client) })
	return client
}

// keyLocks serializes requests under one key. The controller already does for a write;
// this also covers a probe, which runs outside it.
type keyLocks struct {
	mu   sync.Mutex
	held map[domain.RequestID]chan struct{}
}

func (k *keyLocks) lock(ctx context.Context, key domain.RequestID) (unlock func(), err error) {
	for {
		k.mu.Lock()
		if k.held == nil {
			k.held = map[domain.RequestID]chan struct{}{}
		}
		wait, busy := k.held[key]
		if !busy {
			done := make(chan struct{})
			k.held[key] = done
			k.mu.Unlock()
			return func() {
				k.mu.Lock()
				delete(k.held, key)
				k.mu.Unlock()
				close(done)
			}, nil
		}
		k.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

type web struct {
	app    *App
	base   string
	assets fs.FS
	token  func() string
	keys   keyLocks
	// replayed is called for an answer given from the store. Only a test sets it.
	replayed func(*http.Request)
}

// Handler serves the API in web/API.md and the page under the base path. It trusts only
// what nginx's hook says, and is bound only to a filesystem-protected Unix socket, never
// to a TCP port. A response means durable acceptance, not applied readiness: what follows
// arrives through api/events.
func (a *App) Handler(assets fs.FS) http.Handler {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return a.handler(assets, func() string { return token }, nil)
}

func (a *App) handler(assets fs.FS, token func() string, replayed func(*http.Request)) http.Handler {
	base := a.bootstrap.BasePath
	w := &web{app: a, base: base, assets: assets, token: token, replayed: replayed}
	mux := http.NewServeMux()
	read := func(pattern string, h func(*http.Request) (any, *Answer)) {
		mux.HandleFunc("GET "+base+pattern, func(rw http.ResponseWriter, r *http.Request) {
			value, refused := h(r)
			if refused != nil {
				writeAnswer(rw, refused)
				return
			}
			body, err := json.Marshal(value)
			if err != nil {
				writeAnswer(rw, unavailable())
				return
			}
			writeAnswer(rw, &Answer{Status: http.StatusOK, Body: body})
		})
	}
	write := func(pattern string, parse func(*http.Request, []byte) (Command, *Answer)) {
		mux.Handle(pattern, w.mutation(maxBody, parse))
	}
	read("api/session", w.session)
	read("api/state", func(*http.Request) (any, *Answer) { return a.Snapshot(), nil })
	read("api/servers", w.servers)
	read("api/analytics", w.analytics)
	read("api/switches", w.switches)
	read("api/replaced", w.replaced)
	read("api/sources/{id}/refreshes", w.refreshes)
	read("api/sources/{id}/url", w.sourceURL)
	read("api/settings", func(*http.Request) (any, *Answer) { return a.Settings(), nil })
	mux.HandleFunc("GET "+base+"api/events", w.events)
	write("PUT "+base+"api/settings", parsePutSettings)
	write("PUT "+base+"api/settings/check-for-updates", parseCheckForUpdates)
	mux.Handle("PUT "+base+"api/unifi/key", w.unifiKey())
	write("PUT "+base+"api/slots/{id}", parsePutSlot)
	write("DELETE "+base+"api/slots/{id}", func(r *http.Request, _ []byte) (Command, *Answer) {
		return DeleteSlot{ID: domain.SlotID(r.PathValue("id"))}, nil
	})
	write("POST "+base+"api/slots/{id}/client/enable", func(r *http.Request, _ []byte) (Command, *Answer) {
		return EnableClient{ID: domain.SlotID(r.PathValue("id"))}, nil
	})
	write("POST "+base+"api/slots/{id}/client/recreate", func(r *http.Request, _ []byte) (Command, *Answer) {
		return RecreateClient{ID: domain.SlotID(r.PathValue("id"))}, nil
	})
	mux.Handle("PUT "+base+"api/sources/{id}", w.mutation(maxSourceBody, w.parsePutSource))
	write("DELETE "+base+"api/sources/{id}", func(r *http.Request, _ []byte) (Command, *Answer) {
		return DeleteSource{ID: domain.SourceID(r.PathValue("id"))}, nil
	})
	write("POST "+base+"api/sources/{id}/refresh", parseRefreshSource)
	write("PUT "+base+"api/groups/{id}", parsePutGroup)
	write("DELETE "+base+"api/groups/{id}", func(r *http.Request, _ []byte) (Command, *Answer) {
		return DeleteGroup{ID: domain.GroupID(r.PathValue("id"))}, nil
	})
	write("PUT "+base+"api/groups/{id}/ticked/{server}", parseTickServer)
	mux.Handle("POST "+base+"api/groups/preview", w.unkeyed(http.StatusOK, w.previewGroup))
	read("api/update", w.update)
	mux.Handle("POST "+base+"api/update/check", w.unkeyed(http.StatusOK, w.checkForUpdates))
	mux.Handle("POST "+base+"api/update", w.unkeyed(http.StatusAccepted, w.pressUpdate))
	mux.Handle("POST "+base+"api/servers/{id}/probe", w.probe())
	group := func(r *http.Request) domain.Owner { return domain.GroupOwner(domain.GroupID(r.PathValue("id"))) }
	source := func(r *http.Request) domain.Owner { return domain.SourceOwner(domain.SourceID(r.PathValue("id"))) }
	mux.Handle("POST "+base+"api/groups/{id}/preferred/{server}", w.press(group))
	mux.Handle("POST "+base+"api/sources/{id}/preferred/{server}", w.press(source))
	write("DELETE "+base+"api/groups/{id}/preferred/{server}", parseClearPreference(group))
	write("DELETE "+base+"api/sources/{id}/preferred/{server}", parseClearPreference(source))
	noRoute := func(*http.Request, []byte) (Command, *Answer) {
		return nil, webRefusal(http.StatusNotFound, "validation", RefusedNoRoute, "No such route.")
	}
	mux.HandleFunc(base, func(rw http.ResponseWriter, r *http.Request) {
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead
		switch {
		case strings.HasPrefix(r.URL.Path, base+"api/") && safe:
			writeAnswer(rw, webRefusal(http.StatusNotFound, "validation", RefusedNoRoute, "No such route."))
		case strings.HasPrefix(r.URL.Path, base+"api/"):
			w.mutation(maxBody, noRoute).ServeHTTP(rw, r)
		case safe:
			w.page(rw, r)
		default:
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !a.vouchedFor(r) {
			http.Error(rw, "not authenticated", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(rw, r)
	})
}

// vouchedFor needs both headers: the proof says this is the hook, and the auth header says
// the hook's login subrequest ran. With no proof configured nobody is vouched for.
func (a *App) vouchedFor(r *http.Request) bool {
	return len(a.proof) > 0 && r.Header.Get(AuthHeader) != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get(ProofHeader)), a.proof) == 1
}

func webRefusal(status int, kind string, reason Reason, message string) *Answer {
	return answerOf(status, errorBody{Kind: kind, Reason: reason, Message: message})
}

func answerOf(status int, body errorBody) *Answer {
	encoded, _ := json.Marshal(body)
	return &Answer{Status: status, Body: encoded}
}

func unavailable() *Answer {
	return webRefusal(http.StatusServiceUnavailable, "unavailable", RefusedUnavailable, "The gateway couldn't answer that just now.")
}

func writeAnswer(rw http.ResponseWriter, a *Answer) {
	h := rw.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if a.ETag != "" {
		h.Set("ETag", a.ETag)
	}
	rw.WriteHeader(a.Status)
	rw.Write(a.Body)
}

// Session is api/session's answer: who is signed in, the daemon's CSRF token, and the build
// that answers, its version and the release it runs from.
type Session struct {
	User    string  `json:"user"`
	CSRF    string  `json:"csrf"`
	Version string  `json:"version"`
	Release *string `json:"release"`
}

func (w *web) session(*http.Request) (any, *Answer) {
	return Session{User: "owner", CSRF: w.token(), Version: w.app.updates.build.Version, Release: w.app.release}, nil
}

func (w *web) update(r *http.Request) (any, *Answer) {
	view, err := w.app.updates.view(r.Context())
	if err != nil {
		log.Printf("update: %v", err)
		return nil, unavailable()
	}
	return view, nil
}

// checkForUpdates checks at once for a press. For the page's own check, auto, the owner's setting and the
// day since the last one decide.
func (w *web) checkForUpdates(r *http.Request, body []byte) (any, *Answer) {
	var dto struct {
		Auto *bool `json:"auto"`
	}
	if refused := decode(body, &dto); refused != nil {
		return nil, refused
	}
	if dto.Auto == nil {
		return nil, webRefusal(http.StatusBadRequest, "validation", RefusedMalformed, "auto is true or false.")
	}
	updates := w.app.updates
	switch {
	case !updates.updates() && !*dto.Auto:
		return nil, noReleases()
	case !updates.updates(), *dto.Auto && !w.app.Settings().CheckForUpdates:
	default:
		if err := updates.check(r.Context(), *dto.Auto); err != nil {
			log.Printf("the check for a newer release: %v", err)
			return nil, unavailable()
		}
	}
	return w.update(r)
}

func (w *web) pressUpdate(r *http.Request, body []byte) (any, *Answer) {
	var dto struct {
		Version      *string `json:"version"`
		RestartsCore *bool   `json:"restartsCore"`
	}
	if refused := decode(body, &dto); refused != nil {
		return nil, refused
	}
	if dto.Version == nil || dto.RestartsCore == nil {
		return nil, webRefusal(http.StatusBadRequest, "validation", RefusedMalformed, "An update names its version and whether it restarts the core.")
	}
	view, refused := w.app.updates.press(r.Context(), *dto.Version, *dto.RestartsCore)
	return view, refused
}

func noReleases() *Answer {
	return webRefusal(http.StatusUnprocessableEntity, string(KindUnsupported), RefusedNoReleases, "This build takes no updates.")
}

func (w *web) servers(r *http.Request) (any, *Answer) {
	query := r.URL.Query()
	page := PageQuery{Cursor: Cursor(query.Get("cursor")), Source: domain.SourceID(query.Get("source")), Country: query.Get("country")}
	if text := query.Get("limit"); text != "" {
		limit, err := strconv.Atoi(text)
		if err != nil || limit < 1 {
			return nil, webRefusal(http.StatusBadRequest, "validation", RefusedBadQuery, "limit must be a positive number.")
		}
		page.Limit = limit
	}
	out, err := w.app.Servers(r.Context(), page)
	if err != nil {
		log.Printf("servers: %v", err)
		return nil, unavailable()
	}
	return out, nil
}

func (w *web) analytics(r *http.Request) (any, *Answer) {
	refuse := func(message string) (any, *Answer) {
		return nil, webRefusal(http.StatusBadRequest, "validation", RefusedBadQuery, message)
	}
	query := r.URL.Query()
	from, err := time.Parse(time.RFC3339Nano, query.Get("from"))
	if err != nil {
		return refuse("from must be a time.")
	}
	to, err := time.Parse(time.RFC3339Nano, query.Get("to"))
	if err != nil {
		return refuse("to must be a time.")
	}
	bucket, err := time.ParseDuration(query.Get("bucket"))
	if err != nil {
		return refuse("bucket must be a duration such as 300s.")
	}
	if (metrics.Window{From: from, To: to, Step: bucket}).Validate() != nil {
		return refuse("That range can't be charted: from before to, in steps of 5 minutes, and not too many of them.")
	}
	q := AnalyticsQuery{From: from, To: to, Bucket: bucket}
	for _, id := range query["source"] {
		q.Sources = append(q.Sources, domain.SourceID(id))
	}
	for _, id := range query["lineage"] {
		q.Lineages = append(q.Lineages, domain.LineageID(id))
	}
	report, err := w.app.Analytics(r.Context(), q, w.app.control.now())
	if err != nil {
		log.Printf("analytics: %v", err)
		return nil, unavailable()
	}
	return report, nil
}

func (w *web) switches(r *http.Request) (any, *Answer) {
	out, err := w.app.Switches(r.Context())
	if err != nil {
		log.Printf("switches: %v", err)
		return nil, unavailable()
	}
	return out, nil
}

func (w *web) refreshes(r *http.Request) (any, *Answer) {
	id := domain.SourceID(r.PathValue("id"))
	if _, ok := w.app.published.Load().sources[id]; !ok {
		return nil, webRefusal(http.StatusNotFound, "validation", RefusedSourceGone, "That source is gone.")
	}
	out, err := w.app.Refreshes(r.Context(), id)
	if err != nil {
		log.Printf("refreshes: %v", err)
		return nil, unavailable()
	}
	return out, nil
}

// sourceURL gives the owner back a source's URL, which carries its token, so an editor can show it.
func (w *web) sourceURL(r *http.Request) (any, *Answer) {
	source, ok := w.app.published.Load().sources[domain.SourceID(r.PathValue("id"))]
	remote, fetched := source.Origin.Remote()
	if !ok || !fetched {
		return nil, webRefusal(http.StatusNotFound, "validation", RefusedNotRemote, "That source has no URL.")
	}
	url, err := w.app.Queries.state.secret(r.Context(), remote.Location)
	if err != nil {
		log.Printf("source url: %v", err)
		return nil, unavailable()
	}
	return struct {
		URL string `json:"url"`
	}{url}, nil
}

func (w *web) replaced(r *http.Request) (any, *Answer) {
	out, err := w.app.Replaced(r.Context())
	if err != nil {
		log.Printf("replaced: %v", err)
		return nil, unavailable()
	}
	return out, nil
}

func (w *web) events(rw http.ResponseWriter, r *http.Request) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		writeAnswer(rw, unavailable())
		return
	}
	h := rw.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	io.WriteString(rw, ": connected\n\n")
	flusher.Flush()
	changes := w.app.Events(r.Context())
	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-changes:
			if !open {
				return
			}
			data, err := json.Marshal(e)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(rw, "id: %s\ndata: %s\n\n", e.ID, data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// checked runs a mutation's checks in API.md's order. None of these refusals is
// remembered under the key.
func (w *web) checked(rw http.ResponseWriter, r *http.Request, limit int64) (key domain.RequestID, body []byte, ok bool) {
	setRevision(rw, w.app.view.Load().Revision)
	if !w.csrf(rw, r) {
		return "", nil, false
	}
	key = domain.RequestID(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeAnswer(rw, webRefusal(http.StatusBadRequest, "validation", RefusedKeyMissing, "Idempotency-Key is required."))
		return "", nil, false
	}
	body, ok = readBody(rw, r, limit)
	return key, body, ok
}

func (w *web) csrf(rw http.ResponseWriter, r *http.Request) bool {
	given, want := r.Header.Get(CSRFHeader), w.token()
	if subtle.ConstantTimeCompare([]byte(given), []byte(want)) != 1 {
		writeAnswer(rw, webRefusal(http.StatusForbidden, "csrf", RefusedToken, "The page's token expired."))
		return false
	}
	return true
}

func readBody(rw http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, limit))
	if err != nil {
		writeAnswer(rw, webRefusal(http.StatusBadRequest, "validation", RefusedTooLarge, "That request is too large."))
		return nil, false
	}
	if len(body) > 0 {
		media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if media != "application/json" {
			writeAnswer(rw, webRefusal(http.StatusUnsupportedMediaType, "validation", RefusedNotJSON, "Send JSON."))
			return nil, false
		}
	}
	return body, true
}

func fingerprint(r *http.Request, body []byte) []byte {
	sum := sha256.New()
	parts := []string{r.Method, r.URL.Path, r.Header.Get("If-Match"), r.Header.Get("If-None-Match")}
	if r.URL.RawQuery != "" {
		parts = append(parts, r.URL.RawQuery)
	}
	for _, part := range parts {
		fmt.Fprintf(sum, "%d:%s,", len(part), part)
	}
	sum.Write(body)
	return sum.Sum(nil)
}

func (w *web) mutation(limit int64, parse func(*http.Request, []byte) (Command, *Answer)) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		key, body, ok := w.checked(rw, r, limit)
		if !ok {
			return
		}
		w.settle(rw, r, pageWrite{Key: key, Fingerprint: fingerprint(r, body), Decision: decided(parse(r, body))})
	})
}

// settle hands a write to the controller and answers with what it stored. Giving up on
// the queue promises that nothing was committed. Once the controller holds the write, a
// client that left still finds its answer under the key.
func (w *web) settle(rw http.ResponseWriter, r *http.Request, write pageWrite) {
	unlock, err := w.keys.lock(r.Context(), write.Key)
	if err != nil {
		writeAnswer(rw, unavailable())
		return
	}
	defer unlock()
	w.deliver(rw, r, write)
}

func (w *web) deliver(rw http.ResponseWriter, r *http.Request, write pageWrite) {
	reply := make(chan commandResult, 1)
	queued := time.NewTimer(enqueueLimit)
	defer queued.Stop()
	select {
	case w.app.commands <- envelope{page: &write, reply: reply}:
	case <-queued.C:
		writeAnswer(rw, webRefusal(http.StatusServiceUnavailable, "retryable", RefusedBusy, "The gateway is busy. Try again."))
		return
	case <-r.Context().Done():
		return
	}
	select {
	case result := <-reply:
		w.respond(rw, r, result)
	case <-r.Context().Done():
	}
}

// respond answers with what the controller decided. A refusal it didn't remember carries
// the revision it was decided at, as a receipt does; a failure keeps the one checked set.
func (w *web) respond(rw http.ResponseWriter, r *http.Request, result commandResult) {
	var refusal *CommandError
	switch {
	case errors.As(result.err, &refusal):
		setRevision(rw, result.receipt.Revision)
		writeAnswer(rw, refusal.answer())
	case result.err != nil:
		log.Printf("%s %s: %v", r.Method, r.URL.Path, result.err)
		writeAnswer(rw, webRefusal(http.StatusServiceUnavailable, "retryable", RefusedNotSaved, "The gateway couldn't save that. Try again."))
	default:
		if result.receipt.Replayed && w.replayed != nil {
			w.replayed(r)
		}
		setRevision(rw, result.receipt.Revision)
		writeAnswer(rw, result.receipt.Answer)
	}
}

func setRevision(rw http.ResponseWriter, revision domain.IntentRevision) {
	rw.Header().Set(RevisionHeader, strconv.FormatUint(uint64(revision), 10))
}

// outside is a page write its handler decides outside the controller, since that takes seconds. It
// runs under the key's lock, so a resend waits for the first rather than deciding again, and only when
// the key has no receipt, which the controller replays instead. decide returns nil once it has
// answered rw with something that promises nothing, such as a probe that didn't finish.
func (w *web) outside(name string, limit int64, decide func(http.ResponseWriter, *http.Request, []byte) pageDecision) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		key, body, ok := w.checked(rw, r, limit)
		if !ok {
			return
		}
		unlock, err := w.keys.lock(r.Context(), key)
		if err != nil {
			writeAnswer(rw, unavailable())
			return
		}
		defer unlock()
		write := pageWrite{Key: key, Fingerprint: fingerprint(r, body), Decision: replays{}}
		_, _, found, err := w.app.Queries.state.receipt(r.Context(), key)
		switch {
		case err != nil:
			log.Printf("%s: %v", name, err)
			writeAnswer(rw, unavailable())
			return
		case !found:
			if write.Decision = decide(rw, r, body); write.Decision == nil {
				return
			}
		}
		w.deliver(rw, r, write)
	})
}

// probe's answer is the Test's result, which the controller only stores.
func (w *web) probe() http.Handler {
	return w.outside("probe", maxBody, func(rw http.ResponseWriter, r *http.Request, _ []byte) pageDecision {
		result, err := w.app.Probe(r.Context(), domain.LineageID(r.PathValue("id")))
		var refusal *CommandError
		switch {
		case errors.As(err, &refusal):
			return refusal.answer()
		case err != nil:
			writeAnswer(rw, webRefusal(http.StatusServiceUnavailable, "retryable", RefusedUnfinished, "The probe didn't finish. Try again."))
			return nil
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			writeAnswer(rw, unavailable())
			return nil
		}
		return &Answer{Status: http.StatusOK, Body: encoded}
	})
}

// press tests a server of a group or source, as a Test does, and only a pass goes to the controller,
// which moves the owner's slots to it. A failed or unknown Test moves nothing and is its answer.
func (w *web) press(owner func(*http.Request) domain.Owner) http.Handler {
	return w.outside("press", maxBody, func(rw http.ResponseWriter, r *http.Request, body []byte) pageDecision {
		if len(body) > 0 {
			return webRefusal(http.StatusBadRequest, "validation", RefusedMalformed, "A press has no body.")
		}
		sig, err := w.app.probe(r.Context(), domain.LineageID(r.PathValue("server")))
		var refusal *CommandError
		switch {
		case errors.As(err, &refusal):
			return refusal.answer()
		case err != nil:
			writeAnswer(rw, webRefusal(http.StatusServiceUnavailable, "retryable", RefusedUnfinished, "The test didn't finish. Try again."))
			return nil
		case sig.Verdict != domain.Succeeded:
			encoded, err := json.Marshal(probeResultOf(sig))
			if err != nil {
				writeAnswer(rw, unavailable())
				return nil
			}
			return &Answer{Status: http.StatusOK, Body: encoded}
		}
		return runs{PreferServer{Owner: owner(r), Signal: sig}}
	})
}

// maxKeyBody holds { "key": ... } around the longest key install takes.
const maxKeyBody = maxAPIKey + 64

// unifiKey tries the owner's key on UniFi with one read, and the controller stores it only if UniFi
// accepted it, so a mistyped paste never replaces a key that works. A refusal is remembered under the
// request's key, as any page write's is. A try the client gave up on decides nothing. Nothing here
// logs the key or answers with it.
func (w *web) unifiKey() http.Handler {
	return w.outside("unifi key", maxKeyBody, func(_ http.ResponseWriter, r *http.Request, body []byte) pageDecision {
		decision := decided(w.tryKey(r.Context(), body))
		if r.Context().Err() != nil {
			return nil
		}
		return decision
	})
}

func (w *web) tryKey(ctx context.Context, body []byte) (Command, *Answer) {
	var in struct {
		Key string `json:"key"`
	}
	if refused := decode(body, &in); refused != nil {
		return nil, refused
	}
	text := strings.TrimSpace(in.Key)
	if text == "" || strings.ContainsAny(text, " \t\r\n") {
		return nil, webRefusal(http.StatusBadRequest, "validation", RefusedMalformed, "The key must be one line.")
	}
	b := w.app.bootstrap
	client, err := unifi.New(unifi.Config{BaseURL: b.NetworkEndpoint, Site: b.NetworkSite, APIKey: domain.NewSecret(text)})
	if err != nil {
		log.Printf("unifi key: %v", err)
		return nil, unavailable()
	}
	var failure *unifi.Error
	switch err := client.Check(ctx); {
	case errors.As(err, &failure) && failure.Kind == unifi.Unauthorized:
		return nil, webRefusal(http.StatusUnprocessableEntity, "validation", RefusedKeyRefused, "UniFi refused that key.")
	case err != nil:
		log.Printf("unifi key: trying it: %v", err)
		return nil, webRefusal(http.StatusUnprocessableEntity, "validation", RefusedNotAnswering, "UniFi didn't answer. Try again.")
	}
	return PutUniFiKey{Key: domain.NewSecret(text), Syncer: client}, nil
}

// noRevision can't be stored. A precondition naming something unreadable, or revision 0
// of an item, refuses as any revision that isn't stored does.
const noRevision = domain.IntentRevision(^uint64(0))

func precondition(r *http.Request) (domain.IntentRevision, *Answer) {
	create := r.Header.Get("If-None-Match")
	match := r.Header.Get("If-Match")
	switch {
	case create == "*" && match == "":
		return 0, nil
	case create != "" || match == "":
		return 0, webRefusal(http.StatusPreconditionRequired, "validation", RefusedNoRevision, "Send If-Match, or If-None-Match: * to create.")
	}
	unquoted := strings.TrimSuffix(strings.TrimPrefix(match, `"`), `"`)
	revision, err := strconv.ParseUint(unquoted, 10, 64)
	if err != nil || unquoted == match {
		return noRevision, nil
	}
	return domain.IntentRevision(revision), nil
}

func decode(body []byte, into any) *Answer {
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	if d.Decode(into) != nil || d.More() {
		return webRefusal(http.StatusBadRequest, "validation", RefusedMalformed, "That request isn't in the expected form.")
	}
	return nil
}

func invalid(reason Reason, message string) *Answer {
	return webRefusal(http.StatusUnprocessableEntity, "validation", reason, message)
}

func parsePutSettings(r *http.Request, body []byte) (Command, *Answer) {
	expected, refused := precondition(r)
	if refused != nil {
		return nil, refused
	}
	if r.Header.Get("If-None-Match") == "*" {
		expected = noRevision // Settings always exist, so a create is always refused.
	}
	var dto struct {
		AnalyticsRetentionDays int `json:"analyticsRetentionDays"`
	}
	if refused := decode(body, &dto); refused != nil {
		return nil, refused
	}
	return PutSettings{Expected: uint64(expected), RetentionDays: dto.AnalyticsRetentionDays}, nil
}

func parseCheckForUpdates(_ *http.Request, body []byte) (Command, *Answer) {
	var dto struct {
		CheckForUpdates *bool `json:"checkForUpdates"`
	}
	if refused := decode(body, &dto); refused != nil {
		return nil, refused
	}
	if dto.CheckForUpdates == nil {
		return nil, webRefusal(http.StatusBadRequest, "validation", RefusedMalformed, "checkForUpdates is true or false.")
	}
	return PutCheckForUpdates{On: *dto.CheckForUpdates}, nil
}

// replaceable keeps revision 0 from reading as a create, which is what Expected 0 means
// to the controller. No slot, source or group is ever stored at revision 0.
func replaceable(r *http.Request, expected domain.IntentRevision) domain.IntentRevision {
	if expected == 0 && r.Header.Get("If-None-Match") != "*" {
		return noRevision
	}
	return expected
}

func parsePutSlot(r *http.Request, body []byte) (Command, *Answer) {
	expected, refused := precondition(r)
	if refused != nil {
		return nil, refused
	}
	var dto SlotInputDTO
	if refused := decode(body, &dto); refused != nil {
		return nil, refused
	}
	intent, err := dto.admit()
	if err != nil {
		return nil, admission(err)
	}
	return PutSlot{Expected: replaceable(r, expected), ID: domain.SlotID(r.PathValue("id")), Intent: intent}, nil
}

// parsePutGroup compiles the filter here, so a bad pattern is a 422 before the controller sees it. A
// create carries its ticks, [] for none; a replace carries none.
func parsePutGroup(r *http.Request, body []byte) (Command, *Answer) {
	expected, refused := precondition(r)
	if refused != nil {
		return nil, refused
	}
	var dto GroupCreateDTO
	var into any = &dto.GroupInputDTO
	creates := r.Header.Get("If-None-Match") == "*"
	if creates {
		into = &dto
	}
	if refused := decode(body, into); refused != nil {
		return nil, refused
	}
	if creates && dto.Ticked == nil { // Absent and null both decode to nil; [] doesn't.
		return nil, webRefusal(http.StatusBadRequest, "validation", RefusedMalformed, "A new group's ticked is a list, [] for none.")
	}
	members, err := dto.Members.admit()
	if err != nil {
		return nil, admission(err)
	}
	ticked, err := domain.AdmitTicked(dto.Ticked)
	if err != nil {
		return nil, admission(err)
	}
	return PutGroup{Expected: replaceable(r, expected), ID: domain.GroupID(r.PathValue("id")), Name: dto.Name, Members: members, Ticked: ticked}, nil
}

func parseTickServer(r *http.Request, body []byte) (Command, *Answer) {
	var dto struct {
		Ticked bool `json:"ticked"`
	}
	if refused := decode(body, &dto); refused != nil {
		return nil, refused
	}
	return TickServer{Group: domain.GroupID(r.PathValue("id")), Server: domain.LineageID(r.PathValue("server")), Ticked: dto.Ticked}, nil
}

// parseClearPreference reads which preference the page showed: its server, and its end in endsAt.
func parseClearPreference(owner func(*http.Request) domain.Owner) func(*http.Request, []byte) (Command, *Answer) {
	return func(r *http.Request, _ []byte) (Command, *Answer) {
		endsAt, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("endsAt"))
		if err != nil {
			return nil, webRefusal(http.StatusBadRequest, "validation", RefusedBadQuery, "endsAt must be the preference's end, as the view gives it.")
		}
		return ClearPreference{Owner: owner(r), Server: domain.LineageID(r.PathValue("server")), EndsAt: endsAt}, nil
	}
}

// parseRefreshSource reads a refresh's optional body, a User-Agent to store and fetch with.
func parseRefreshSource(r *http.Request, body []byte) (Command, *Answer) {
	cmd := RefreshSource{ID: domain.SourceID(r.PathValue("id"))}
	if len(body) == 0 {
		return cmd, nil
	}
	var dto struct {
		UserAgent *string `json:"userAgent"`
	}
	if refused := decode(body, &dto); refused != nil {
		return nil, refused
	}
	if dto.UserAgent != nil {
		if refused := checkUserAgent(*dto.UserAgent); refused != nil {
			return nil, refused
		}
		cmd.UserAgent = cmp.Or(*dto.UserAgent, catalogue.DefaultUserAgent)
	}
	return cmd, nil
}

// sentence words a domain admission error for the page. These errors quote what the page
// sent and nothing stored.
func sentence(err error) string {
	text := err.Error()
	return strings.ToUpper(text[:1]) + text[1:] + "."
}

// sourceInputDTO is schema.ts's SourceInput.
type sourceInputDTO struct {
	Name   string         `json:"name"`
	Origin originInputDTO `json:"origin"`
}

// originInputDTO is either arm of schema.ts's origin, by Kind. A null URL or text leaves the stored one.
type originInputDTO struct {
	Kind                string  `json:"kind"`
	URL                 *string `json:"url"`
	UserAgent           string  `json:"userAgent"`
	RefreshEverySeconds float64 `json:"refreshEverySeconds"`
	Paused              bool    `json:"paused"`
	Text                *string `json:"text"`
}

const maxUserAgent = 200

func checkUserAgent(agent string) *Answer {
	if len(agent) > maxUserAgent || strings.ContainsFunc(agent, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		return invalid(RefusedBadUserAgent, "A User-Agent is one line of at most 200 characters.")
	}
	return nil
}

func (w *web) parsePutSource(r *http.Request, body []byte) (Command, *Answer) {
	expected, refused := precondition(r)
	if refused != nil {
		return nil, refused
	}
	var dto sourceInputDTO
	if refused := decode(body, &dto); refused != nil {
		return nil, refused
	}
	input := SourceInput{Name: dto.Name}
	switch dto.Origin.Kind {
	case "remote":
		remote, refused := dto.Origin.remote()
		if refused != nil {
			return nil, refused
		}
		input.Remote = remote
	case "manual":
		input.Manual = &ManualInput{}
		if text := dto.Origin.Text; text != nil {
			doc, refused := w.parseText(*text)
			if refused != nil {
				return nil, refused
			}
			input.Manual.Text, input.Manual.Document = domain.NewSecret(*text), &doc
		}
	default:
		return nil, invalid(RefusedMalformed, "A source is either a URL or pasted text.")
	}
	return PutSource{Expected: replaceable(r, expected), ID: domain.SourceID(r.PathValue("id")), Input: input}, nil
}

func (dto originInputDTO) remote() (*RemoteInput, *Answer) {
	input := &RemoteInput{UserAgent: dto.UserAgent, Paused: dto.Paused,
		RefreshEvery: time.Duration(dto.RefreshEverySeconds * float64(time.Second))}
	if dto.RefreshEverySeconds < 0 {
		return nil, invalid(RefusedBadRefresh, "Refresh interval can't be negative.")
	}
	if refused := checkUserAgent(dto.UserAgent); refused != nil {
		return nil, refused
	}
	if dto.URL != nil {
		u, err := url.Parse(*dto.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return nil, invalid(RefusedNotHTTPS, "The source URL must be an https address.")
		}
		input.URL = domain.NewSecret(*dto.URL)
	}
	return input, nil
}

// parseText runs on the handler's goroutine, so a large paste can't stall the controller.
// Text with no usable servers is refused with its diagnostics, even a list that is empty
// on purpose, because pasting it would mark every stored server missing.
func (w *web) parseText(text string) (catalogue.Document, *Answer) {
	doc, err := w.app.fetcher.ParseText(text)
	if err == nil && len(doc.Entries()) > 0 {
		return doc, nil
	}
	body := errorBody{Kind: "validation", Reason: RefusedNoServers, Message: "That text has no servers this gateway can use.", Diagnostics: diagnosticDTOs(doc.Diagnostics())}
	if err != nil && !errors.Is(err, catalogue.ErrNoUsableEntries) {
		body.Reason, body.Message = RefusedUnreadable, "That text isn't a list of servers this gateway can read."
	}
	return catalogue.Document{}, answerOf(http.StatusUnprocessableEntity, body)
}

var pageTypes = map[string]string{".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".woff2": "font/woff2", ".html": "text/html; charset=utf-8"}

// page serves a built file, or index.html for a deep link. Vite hashes everything under
// assets/, so those files can be kept for good and only index.html must stay fresh.
func (w *web) page(rw http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(urlpath.Clean(r.URL.Path), strings.TrimSuffix(w.base, "/"))
	name = strings.TrimPrefix(name, "/")
	if w.assets == nil {
		http.Error(rw, "this build has no page", http.StatusServiceUnavailable)
		return
	}
	data, err := fs.ReadFile(w.assets, name)
	if err != nil || name == "" {
		name = "index.html"
		if data, err = fs.ReadFile(w.assets, name); err != nil {
			http.Error(rw, "this build has no page", http.StatusServiceUnavailable)
			return
		}
	}
	h := rw.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	if contentType, known := pageTypes[urlpath.Ext(name)]; known {
		h.Set("Content-Type", contentType)
	} else if contentType := mime.TypeByExtension(urlpath.Ext(name)); contentType != "" {
		h.Set("Content-Type", contentType)
	}
	if strings.HasPrefix(name, "assets/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	rw.Write(data)
}

// serveHTTP binds the Unix socket and serves until ctx ends. The directory is ours and
// open to enter; the socket's own mode decides who may connect. With no proof it stays
// root's alone, and a missing proof costs the page without stopping the data path.
func (a *App) serveHTTP(ctx context.Context) error {
	socket := a.bootstrap.ListenSocket
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	mode := os.FileMode(0o600)
	switch name := a.bootstrap.ListenGroup; {
	case name != "" && len(a.proof) == 0:
		log.Printf("no nginx proof at %s: the socket stays root's and the page stays closed", ProofPath(a.bootstrap.DataDir))
	case name != "":
		group, err := user.LookupGroup(name)
		if err != nil {
			return fmt.Errorf("listen group: %w", err)
		}
		gid, err := strconv.Atoi(group.Gid)
		if err != nil {
			return fmt.Errorf("listen group: %w", err)
		}
		if err := os.Chown(socket, 0, gid); err != nil {
			return err
		}
		mode = 0o660
	}
	if err := os.Chmod(socket, mode); err != nil {
		return err
	}
	ctx, end := context.WithCancel(ctx)
	var confirming sync.WaitGroup
	defer confirming.Wait()
	defer end()
	// The socket listens, so a release a recovery put back answers.
	confirming.Go(func() { ConfirmPutBack(ctx, a.bootstrap.DataDir, a.updates.lockPath, a.release) })
	server := &http.Server{Handler: a.Handler(a.page), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { server.Close() })
	defer stop()
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
