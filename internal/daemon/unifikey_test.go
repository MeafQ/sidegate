package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUniFi is a Network Application on loopback TLS that accepts its good keys and counts what it is asked.
// Every error it answers names the key it was sent, as an app that echoes its input would. A key in
// badRequest is answered 400, the others it doesn't accept 401. A good key in once is accepted only on
// its first ask.
type fakeUniFi struct {
	*httptest.Server
	good       []string
	badRequest []string
	once       []string
	mu         sync.Mutex
	asks       map[string]int // By key.
}

func newFakeUniFi(t *testing.T, good ...string) *fakeUniFi {
	t.Helper()
	f := &fakeUniFi{good: good, asks: map[string]int{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-KEY")
		f.mu.Lock()
		f.asks[key]++
		revoked := slices.Contains(f.once, key) && f.asks[key] > 1
		f.mu.Unlock()
		echoed, _ := json.Marshal(map[string]any{"meta": map[string]string{"rc": "error", "msg": key, "field_name": key}, "data": []string{}, "code": key})
		switch {
		case slices.Contains(f.badRequest, key):
			rw.WriteHeader(http.StatusBadRequest)
			rw.Write(echoed)
			return
		case !slices.Contains(f.good, key) || revoked:
			rw.WriteHeader(http.StatusUnauthorized)
			rw.Write(echoed)
			return
		}
		if strings.Contains(r.URL.Path, "/v2/") {
			io.WriteString(rw, `[]`)
			return
		}
		io.WriteString(rw, `{"meta":{"rc":"ok"},"data":[]}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUniFi) asked(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asks[key]
}

type keyAnswer struct {
	status   int
	revision string
	body     string
	headers  string
}

func putKey(t *testing.T, base, idempotency, key string) keyAnswer {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"key": key})
	req, err := http.NewRequest(http.MethodPut, base+DefaultBasePath+"api/unifi/key", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(CSRFHeader, "token-1")
	req.Header.Set("Idempotency-Key", idempotency)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	headers := resp.Header.Clone()
	headers.Del("Date") // A replay carries the first answer's headers, but not its time.
	return keyAnswer{status: resp.StatusCode, revision: resp.Header.Get(RevisionHeader), body: string(text), headers: fmt.Sprint(headers)}
}

// keyedDaemon is the page's daemon talking to unifi, with stored as its key file, or no key when it is empty.
func keyedDaemon(t *testing.T, unifi *fakeUniFi, stored string) (*pageDaemon, *httptest.Server) {
	t.Helper()
	h, server := newPageDaemon(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stop()
	app := h.app
	app.bootstrap.NetworkEndpoint, app.bootstrap.NetworkSite = unifi.URL, "default"
	app.control.key.syncer = nil
	if stored != "" {
		path := filepath.Join(app.bootstrap.DataDir, networkKeyName)
		if err := os.WriteFile(path, []byte(stored), 0o600); err != nil {
			t.Fatal(err)
		}
		app.bootstrap.NetworkCredentialFile = path
		syncer, err := openSyncer(app.bootstrap)
		if err != nil {
			t.Fatal(err)
		}
		app.control.key.syncer = syncer
	}
	app.control.publish(time.Now())
	h.stop = h.start(app)
	return h, server
}

func storedKey(t *testing.T, app *App) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(app.bootstrap.DataDir, networkKeyName))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for limit := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(limit) {
			t.Fatal(what)
		}
	}
}

// A key UniFi refuses is answered as refused at the current revision, and the key that works stays
// stored and in use.
func TestARefusedKeyReplacesNothing(t *testing.T) {
	unifi := newFakeUniFi(t, "the-working-key")
	h, server := keyedDaemon(t, unifi, "the-working-key")
	app, _ := h.current()
	waitFor(t, "UniFi was never read with the stored key", func() bool { return app.Snapshot().VPNClients.Known })
	before := app.Snapshot().Revision
	a := putKey(t, server.URL, "refused", "a-mistyped-key")
	if a.status != http.StatusUnprocessableEntity || !strings.Contains(a.body, `"keyRefused"`) || a.revision != strconv.FormatUint(uint64(before), 10) {
		t.Fatalf("a refused key answered %+v at view revision %d", a, before)
	}
	if got := storedKey(t, app); got != "the-working-key" {
		t.Errorf("the stored key became %q", got)
	}
	asked := unifi.asked("a-mistyped-key")
	h.paused(func(app *App) {
		if app.Snapshot().UniFiTrouble != nil || app.Snapshot().Revision != before {
			t.Errorf("a refused key changed the view: trouble %v, revision %d", app.Snapshot().UniFiTrouble, app.Snapshot().Revision)
		}
		if app.control.key.syncer == nil {
			t.Error("a refused key dropped the working syncer")
		}
	})
	if again := putKey(t, server.URL, "refused", "a-mistyped-key"); again != a || unifi.asked("a-mistyped-key") != asked {
		t.Errorf("a resend under the same key answered %+v and asked UniFi again: %d, then %d", again, asked, unifi.asked("a-mistyped-key"))
	}
}

// UniFi's silence isn't a refusal of the key: the answer says UniFi didn't answer, and nothing changes.
func TestASilentUniFiReplacesNothing(t *testing.T) {
	unifi := newFakeUniFi(t, "the-working-key")
	h, server := keyedDaemon(t, unifi, "the-working-key")
	app, _ := h.current()
	unifi.Close()
	before := app.Snapshot().Revision
	a := putKey(t, server.URL, "silent", "the-working-key")
	if a.status != http.StatusUnprocessableEntity || !strings.Contains(a.body, `"notAnswering"`) || a.revision != strconv.FormatUint(uint64(before), 10) {
		t.Fatalf("a try UniFi didn't answer answered %+v", a)
	}
	if got := storedKey(t, app); got != "the-working-key" {
		t.Errorf("the stored key became %q", got)
	}
}

// On a daemon installed with no key, an accepted key is stored root-only, named in bootstrap.json for
// the next start, and its syncer reads UniFi at once. The answer's revision is a view with no trouble.
func TestAnAcceptedKeyIsStoredAndStartsTheSyncer(t *testing.T) {
	unifi := newFakeUniFi(t, "the-new-key")
	h, server := keyedDaemon(t, unifi, "")
	app, _ := h.current()
	if trouble := app.Snapshot().UniFiTrouble; trouble == nil || *trouble != unifiNoKey {
		t.Fatalf("a daemon with no key says %v", trouble)
	}
	a := putKey(t, server.URL, "accepted", " the-new-key\n")
	if a.status != http.StatusOK {
		t.Fatalf("an accepted key answered %+v", a)
	}
	path := filepath.Join(app.bootstrap.DataDir, networkKeyName)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || storedKey(t, app) != "the-new-key" {
		t.Fatalf("the key file: %v, %v, %q", info, err, storedKey(t, app))
	}
	b, err := LoadBootstrap(app.bootstrap.DataDir)
	if err != nil || b.NetworkCredentialFile != path {
		t.Errorf("bootstrap.json names %q, %v, want %s", b.NetworkCredentialFile, err, path)
	}
	h.paused(func(app *App) {
		view := app.Snapshot()
		answered, _ := strconv.ParseUint(a.revision, 10, 64)
		if uint64(view.Revision) < answered || answered == 0 || view.UniFiTrouble != nil || view.UniFi.AcceptedAt == nil {
			t.Errorf("at revision %d after an answer at %s: trouble %v, accepted at %v", view.Revision, a.revision, view.UniFiTrouble, view.UniFi.AcceptedAt)
		}
	})
	waitFor(t, "the new key's syncer never read UniFi", func() bool { return unifi.asked("the-new-key") >= 2 && app.Snapshot().VPNClients.Known })
	reopened, err := openSyncer(b)
	if err != nil || reopened == nil {
		t.Errorf("the next start opens no syncer: %v", err)
	}
}

// restart closes the daemon and opens it again on b, as the next start does with what the last one left on disk.
func restart(t *testing.T, h *pageDaemon, b Bootstrap) *App {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stop()
	h.app.Close()
	app, err := Open(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	h.app, h.handler = app, app.handler(os.DirFS(h.dist), h.currentToken, h.markReplayed)
	h.stop = h.start(app)
	return app
}

// holding lists the data dir's files, the key's own included, that hold text.
func holding(t *testing.T, dir, text string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if strings.Contains(string(raw), text) {
			found = append(found, filepath.Base(path))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// A save whose commit fails answers 503, so it must have changed nothing: the working key stays on disk
// for the next start, and no copy of the new one is left. The resend under the same key then lands it.
func TestAKeySaveThatFailsToCommitReplacesNothing(t *testing.T) {
	unifi := newFakeUniFi(t, "the-working-key", "the-new-key")
	h, server := keyedDaemon(t, unifi, "the-working-key")
	app, _ := h.current()
	waitFor(t, "UniFi was never read with the stored key", func() bool { return app.Snapshot().VPNClients.Known })
	h.paused(func(app *App) {
		mustExec(t, app.control.store.writer, `CREATE TRIGGER fail_commit BEFORE INSERT ON request_receipts BEGIN SELECT RAISE(ABORT,'commit fails'); END`)
	})
	before := app.Snapshot().Revision
	if a := putKey(t, server.URL, "fails", "the-new-key"); a.status != http.StatusServiceUnavailable {
		t.Fatalf("a save that couldn't commit answered %+v", a)
	}
	if got := storedKey(t, app); got != "the-working-key" {
		t.Errorf("after a 503 the key file holds %q", got)
	}
	if files := holding(t, app.bootstrap.DataDir, "the-new-key"); len(files) > 0 {
		t.Errorf("after a 503 the new key is in %v", files)
	}
	h.paused(func(app *App) {
		if app.Snapshot().Revision != before {
			t.Errorf("a 503 moved the revision from %d to %d", before, app.Snapshot().Revision)
		}
		mustExec(t, app.control.store.writer, `DROP TRIGGER fail_commit`)
	})
	app = restart(t, h, app.bootstrap)
	if got := storedKey(t, app); got != "the-working-key" {
		t.Errorf("the next start found %q", got)
	}
	if a := putKey(t, server.URL, "fails", "the-new-key"); a.status != http.StatusOK || storedKey(t, app) != "the-new-key" {
		t.Errorf("the resend answered %+v with %q stored", a, storedKey(t, app))
	}
}

// A crash after the commit that accepted a key and before its file changed leaves the key waiting beside
// the file, and the next start puts it in place and names it in bootstrap.json. A key whose commit never
// landed is deleted, whatever it is.
func TestAKeyCommittedBeforeACrashIsInPlaceAfterTheNextStart(t *testing.T) {
	for _, stored := range []string{"the-working-key", ""} {
		t.Run("stored "+strconv.Quote(stored), func(t *testing.T) {
			unifi := newFakeUniFi(t, "the-working-key", "the-new-key")
			h, server := keyedDaemon(t, unifi, stored)
			app, _ := h.current()
			a := putKey(t, server.URL, "accepted", "the-new-key")
			if a.status != http.StatusOK {
				t.Fatalf("the save answered %+v", a)
			}
			revision, _ := strconv.ParseUint(a.revision, 10, 64)
			path := filepath.Join(app.bootstrap.DataDir, networkKeyName)
			b := app.bootstrap
			if stored == "" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(bootstrapPath(b.DataDir), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				b.NetworkCredentialFile = ""
			} else if err := os.WriteFile(path, []byte(stored), 0o600); err != nil {
				t.Fatal(err)
			}
			for at, key := range map[uint64]string{revision: "the-new-key", revision + 1: "a-key-never-committed"} {
				if err := os.WriteFile(waitingKey(path, at), []byte(key), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			app = restart(t, h, b)
			if got := storedKey(t, app); got != "the-new-key" {
				t.Errorf("the next start found %q", got)
			}
			if files := holding(t, b.DataDir, "the-new-key"); !slices.Equal(files, []string{networkKeyName}) {
				t.Errorf("the accepted key is in %v", files)
			}
			if files := holding(t, b.DataDir, "a-key-never-committed"); len(files) > 0 {
				t.Errorf("a key never committed is in %v", files)
			}
			if loaded, err := LoadBootstrap(b.DataDir); err != nil || app.bootstrap.NetworkCredentialFile != path || (stored == "" && loaded.NetworkCredentialFile != path) {
				t.Errorf("the next start names %q, bootstrap.json %q, %v", app.bootstrap.NetworkCredentialFile, loaded.NetworkCredentialFile, err)
			}
			waitFor(t, "the next start never read UniFi with the accepted key", func() bool { return app.Snapshot().VPNClients.Known && app.Snapshot().UniFiTrouble == nil })
		})
	}
}

// A key accepted but kept from its file by something in the way doesn't stop the next start: the daemon
// comes up with no key, Settings says so, and once the way is clear a pasted key is stored and used.
func TestAStartComesUpWhenTheAcceptedKeyCantBePutInPlace(t *testing.T) {
	unifi := newFakeUniFi(t, "the-blocked-key", "the-next-key")
	h, server := keyedDaemon(t, unifi, "")
	app, _ := h.current()
	path := filepath.Join(app.bootstrap.DataDir, networkKeyName)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if a := putKey(t, server.URL, "blocked", "the-blocked-key"); a.status != http.StatusOK {
		t.Fatalf("the save answered %+v", a)
	}
	app = restart(t, h, app.bootstrap)
	resp, err := http.Get(server.URL + DefaultBasePath + "api/state")
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		UniFiTrouble *string `json:"unifiTrouble"`
	}
	err = json.NewDecoder(resp.Body).Decode(&view)
	resp.Body.Close()
	if err != nil || view.UniFiTrouble == nil || *view.UniFiTrouble != string(unifiNoKey) {
		t.Fatalf("the state after that start says %v, %v", view.UniFiTrouble, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if a := putKey(t, server.URL, "next", "the-next-key"); a.status != http.StatusOK || storedKey(t, app) != "the-next-key" {
		t.Fatalf("the save once the way was clear answered %+v with %q stored", a, storedKey(t, app))
	}
	if b, err := LoadBootstrap(app.bootstrap.DataDir); err != nil || b.NetworkCredentialFile != path {
		t.Errorf("bootstrap.json names %q, %v", b.NetworkCredentialFile, err)
	}
	if files := holding(t, app.bootstrap.DataDir, "the-blocked-key"); len(files) > 0 {
		t.Errorf("the blocked key is still in %v", files)
	}
	waitFor(t, "the pasted key never read UniFi", func() bool { return app.Snapshot().VPNClients.Known && app.Snapshot().UniFiTrouble == nil })
}

// No route, header, log line, refusal, receipt, event or file but the key's own ever carries a key the
// owner sent, whether UniFi refused it, rejected the request, didn't answer, or took it, and whether it
// was sent once or resent, or UniFi refused it on the first sync after it took it. UniFi echoes the key
// in every error it answers.
func TestTheKeyAppearsInNoLogRefusalEventOrRead(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	const refused, rejected, silent, accepted, malformed = "r3fused-k3y-0001", "r3jected-k3y-0005", "s1lent-k3y-0002", "acc3pted-k3y-0003", "two w0rds-k3y-0004"
	secrets := []string{refused, rejected, silent, accepted, "w0rds-k3y-0004"}
	unifi := newFakeUniFi(t, accepted)
	unifi.badRequest = []string{rejected}
	unifi.once = []string{accepted}
	h, server := keyedDaemon(t, unifi, "")
	app, _ := h.current()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stream bytes.Buffer
	var streamMu sync.Mutex
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+DefaultBasePath+"api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	seen := []string{"events headers: " + fmt.Sprint(resp.Header)}
	go func() {
		defer resp.Body.Close()
		chunk := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(chunk)
			streamMu.Lock()
			stream.Write(chunk[:n])
			streamMu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	for _, try := range []struct{ idempotency, key string }{{"k1", refused}, {"k1", refused}, {"k2", malformed}, {"k5", rejected}, {"k5", rejected}, {"k3", accepted}, {"k3", accepted}} {
		a := putKey(t, server.URL, try.idempotency, try.key)
		seen = append(seen, a.body, a.revision, a.headers)
	}
	if unifi.asked(rejected) == 0 {
		t.Fatal("UniFi was never sent the key it rejects, so this proved nothing")
	}
	waitFor(t, "the first sync after the accepted key was never refused", func() bool {
		trouble := app.Snapshot().UniFiTrouble
		return unifi.asked(accepted) >= 2 && trouble != nil && *trouble == unifiKeyRefused
	})
	silentUniFi := newFakeUniFi(t, silent)
	silentUniFi.Close()
	h.paused(func(app *App) { app.bootstrap.NetworkEndpoint = silentUniFi.URL })
	a := putKey(t, server.URL, "k4", silent)
	seen = append(seen, a.body, a.headers)
	if a.status != http.StatusUnprocessableEntity {
		t.Fatalf("the silent try answered %+v", a)
	}
	waitFor(t, "no event followed the accepted key", func() bool {
		streamMu.Lock()
		defer streamMu.Unlock()
		return strings.Contains(stream.String(), "data: ")
	})
	for _, route := range []string{"", "api/state", "api/settings", "api/session", "api/servers", "api/switches"} {
		resp, err := http.Get(server.URL + DefaultBasePath + route)
		if err != nil {
			t.Fatal(err)
		}
		text, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		seen = append(seen, route+": "+fmt.Sprint(resp.Header)+string(text))
	}
	h.paused(func(app *App) {
		err := filepath.WalkDir(app.bootstrap.DataDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Base(path) == networkKeyName {
				return err
			}
			raw, err := os.ReadFile(path)
			seen = append(seen, path+": "+string(raw))
			return err
		})
		if err != nil {
			t.Error(err)
		}
	})
	cancel()
	streamMu.Lock()
	seen = append(seen, "events: "+stream.String())
	streamMu.Unlock()
	logMu.Lock()
	seen = append(seen, "log: "+logs.String())
	logMu.Unlock()
	for _, text := range seen {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("the key %q appears in %.300s", secret, text)
			}
		}
	}
	if storedKey(t, app) != accepted {
		t.Errorf("the accepted key wasn't stored, so this proved nothing: %q", storedKey(t, app))
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
