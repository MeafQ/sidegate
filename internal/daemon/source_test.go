package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
)

func acmeText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../catalogue/testdata/acme-shape.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func parsedAcme(t *testing.T, text string) catalogue.Document {
	t.Helper()
	doc, err := catalogue.Parse([]byte(text), catalogue.Detect, catalogue.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func rotated(text string) string { return strings.ReplaceAll(text, "aaaaaaaa-", "bbbbbbbb-") }

func checkedAt(at time.Time) catalogue.FetchState {
	now, _ := json.Marshal(at)
	var fs catalogue.FetchState
	if err := fs.UnmarshalBinary(fmt.Appendf(nil, `{"checked_at":%s,"updated_at":%s}`, now, now)); err != nil {
		panic(err)
	}
	return fs
}

// refreshed plays a successful fetch of source into the controller.
func refreshed(t *testing.T, c *controller, id domain.SourceID, doc catalogue.Document, at time.Time) {
	t.Helper()
	source := c.state.sources[id]
	refresh := catalogue.Refresh{Basis: catalogue.Basis{Source: id, Revision: source.Revision}, Outcome: catalogue.Updated, Document: doc, Next: checkedAt(at)}
	if err := c.applyRefresh(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
}

func remoteSource(t *testing.T, r *rig, id domain.SourceID) {
	t.Helper()
	if _, err := r.c.accept(context.Background(), PutSource{ID: id, Input: SourceInput{Name: "Acme", Remote: &RemoteInput{URL: domain.NewSecret("https://example.com/f"), RefreshEvery: 6 * time.Hour}}}); err != nil {
		t.Fatal(err)
	}
	r.wanReads(domain.WANUp)
	r.c.startFetches(r.clock)
	<-r.c.fetchJobs
}

// A first parse offers every server its document lists, and a parse moves no revision, since only
// an owner's write does.
func TestAFirstParseOffersEveryServer(t *testing.T) {
	r := newRig(t, t.TempDir())
	remoteSource(t, r, "acme")
	doc := parsedAcme(t, acmeText(t))
	for _, next := range []catalogue.Document{doc, doc, parsedAcme(t, rotated(acmeText(t)))} {
		refreshed(t, r.c, "acme", next, r.clock)
		if got := r.c.state.sources["acme"].Revision; got != 1 {
			t.Errorf("a parse moved the revision to %d", got)
		}
		if got := len(r.c.world.sourceMembers("acme")); got != 12 {
			t.Errorf("an Exit of the source allows %d servers, want all 12", got)
		}
	}
}

func TestASourceEditDropsARefreshInFlightAndFetchesAgain(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	remoteSource(t, r, "acme")
	doc := parsedAcme(t, acmeText(t))
	refreshed(t, r.c, "acme", doc, r.clock)

	r.clock = r.clock.Add(7 * time.Hour)
	r.c.startFetches(r.clock)
	job := <-r.c.fetchJobs
	current := r.c.state.sources["acme"]
	if _, err := r.c.accept(ctx, PutSource{ID: "acme", Expected: domain.IntentRevision(current.Revision),
		Input: SourceInput{Name: "Renamed", Remote: &RemoteInput{RefreshEvery: 6 * time.Hour}}}); err != nil {
		t.Fatal(err)
	}
	before := r.c.state.catalogueRevision
	if err := r.c.applyRefresh(ctx, catalogue.Refresh{Basis: job.Basis(), Outcome: catalogue.Updated, Document: parsedAcme(t, rotated(acmeText(t))), Next: checkedAt(r.clock)}); err != nil {
		t.Fatal(err)
	}
	if got := r.c.state.catalogueRevision; got != before {
		t.Errorf("a dropped refresh moved the catalogue to %d, was %d", got, before)
	}
	r.c.startFetches(r.clock)
	select {
	case next := <-r.c.fetchJobs:
		if next.Basis().Revision != r.c.state.sources["acme"].Revision {
			t.Errorf("the next fetch's basis = %+v", next.Basis())
		}
	default:
		t.Error("the dropped refresh was not fetched again")
	}
}

// A refresh that carries another User-Agent stores it and fetches with it in one write, with no
// precondition: a fetch in flight with the old agent is dropped, the next fetch is the new revision's,
// and an editor opened before it is stale. The stored agent alone moves nothing.
func TestARefreshWithAUserAgentStoresItAndFetchesWithIt(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	remoteSource(t, r, "acme")
	refreshed(t, r.c, "acme", parsedAcme(t, acmeText(t)), r.clock)
	r.clock = r.clock.Add(7 * time.Hour)
	r.c.startFetches(r.clock)
	inFlight := <-r.c.fetchJobs

	if _, err := r.c.accept(ctx, RefreshSource{Request: "same", ID: "acme", UserAgent: catalogue.DefaultUserAgent}); err != nil {
		t.Fatal(err)
	}
	if got := r.c.state.sources["acme"].Revision; got != 1 {
		t.Errorf("a refresh with the stored agent moved the revision to %d", got)
	}
	if _, err := r.c.accept(ctx, RefreshSource{Request: "hiddify", ID: "acme", UserAgent: "Hiddify/2.5.7"}); err != nil {
		t.Fatal(err)
	}
	stored := r.c.state.sources["acme"]
	remote, _ := stored.Origin.Remote()
	if remote.UserAgent != "Hiddify/2.5.7" || stored.Revision != 2 {
		t.Errorf("after the refresh the source has agent %q at revision %d, want Hiddify/2.5.7 at 2", remote.UserAgent, stored.Revision)
	}
	before := r.c.state.catalogueRevision
	if err := r.c.applyRefresh(ctx, catalogue.Refresh{Basis: inFlight.Basis(), Outcome: catalogue.Updated, Document: parsedAcme(t, rotated(acmeText(t))), Next: checkedAt(r.clock)}); err != nil {
		t.Fatal(err)
	}
	if got := r.c.state.catalogueRevision; got != before {
		t.Errorf("a fetch with the old agent was applied: catalogue %d, was %d", got, before)
	}
	r.c.startFetches(r.clock)
	select {
	case next := <-r.c.fetchJobs:
		if next.Basis().Revision != 2 {
			t.Errorf("the next fetch's basis = %+v, want revision 2", next.Basis())
		}
	default:
		t.Error("the refresh didn't fetch with the new agent")
	}
	r.close()
	r = newRigAt(t, r.dir, r.clock)
	if remote, _ := r.c.state.sources["acme"].Origin.Remote(); remote.UserAgent != "Hiddify/2.5.7" {
		t.Errorf("after a restart the agent is %q", remote.UserAgent)
	}
	if _, err := r.c.accept(ctx, PutSource{ID: "acme", Expected: 1, Input: SourceInput{Name: "Acme", Remote: &RemoteInput{RefreshEvery: 6 * time.Hour}}}); !isStale(err) {
		t.Errorf("a save opened before the refresh: %v, want stale", err)
	}
}

func isStale(err error) bool {
	var refusal *CommandError
	return errors.As(err, &refusal) && refusal.Kind == KindStale
}

func TestAMovedServerKeepsItsExitAndSaysReplaced(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	remoteSource(t, r, "acme")
	text := acmeText(t)
	refreshed(t, r.c, "acme", parsedAcme(t, text), r.clock)
	var id domain.LineageID
	for _, l := range r.c.world.catalogue.All() {
		if l.Label == "Germany 🇩🇪 · proxy" {
			id = l.ID
		}
	}
	exit := domain.ServerExit(id)
	r.putSlot("assigned", exitIntent(t, "Assigned", exit))
	if got, err := r.app.Replaced(ctx); err != nil || len(got) != 0 {
		t.Fatalf("replaced after the first refresh = %+v, %v, want none", got, err)
	}
	switches := len(r.switches())

	refreshed(t, r.c, "acme", parsedAcme(t, strings.ReplaceAll(text, ".fixture.example.net", ".moved.example.net")), r.clock.Add(time.Hour))

	if got := r.c.state.slots["assigned"].Slot.Intent.Exit(); got != exit {
		t.Errorf("exit = %+v, want %+v", got, exit)
	}
	if _, ok := r.c.world.Pool(exit).member(id); !ok {
		t.Errorf("the Exit no longer allows its server")
	}
	l, _ := r.c.world.catalogue.Lookup(id)
	_, hops := l.Server.Closure()
	if l.Presence != domain.Present || !strings.HasSuffix(hops[0].Endpoint.Host, ".moved.example.net") {
		t.Errorf("lineage %s: presence %d, host %s, want the moved server", id, l.Presence, hops[0].Endpoint.Host)
	}
	if got := len(r.c.world.catalogue.All()); got != 12 {
		t.Errorf("lineages = %d, want the same 12", got)
	}
	got, err := r.app.Replaced(ctx)
	if err != nil || len(got) != 12 {
		t.Fatalf("replaced = %+v, %v, want all 12", got, err)
	}
	found := slices.IndexFunc(got, func(e ReplacedEvent) bool { return e.Server == id })
	if found < 0 || got[found].Label != "Germany · proxy" || got[found].PreviousLabel != "Germany · proxy" || got[found].Source != "acme" {
		t.Errorf("replaced = %+v, want the moved server %s", got, id)
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "aaaaaaaa-") {
		t.Errorf("a credential reached the view: %s", body)
	}
	if n := len(r.switches()); n != switches {
		t.Errorf("switches = %d, was %d: the event must not be a switch", n, switches)
	}
}

func TestSourceCodecRoundTrip(t *testing.T) {
	for _, source := range []domain.Source{
		{ID: "a", Revision: 3, Name: "A", Origin: domain.RemoteOrigin(domain.Remote{Location: "l", UserAgent: "u", HWID: "h", RefreshEvery: time.Hour, Paused: true}),
			Feed: &domain.Feed{Configs: []domain.FeedConfig{{Key: "k", Place: 1, Name: "Config", Servers: 1}}}, Added: t0},
		{ID: "c", Revision: 1, Name: "C", Origin: domain.ManualOrigin(domain.Manual{Text: "t"})},
	} {
		data, err := encodeSource(source)
		if err != nil {
			t.Fatal(err)
		}
		back, err := decodeSource(source.ID, data)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, source) {
			t.Errorf("round trip: %+v\nwant %+v", back, source)
		}
	}
	if _, err := decodeSource("x", []byte(`{}`)); err == nil {
		t.Error("a source with no origin decoded")
	}
}

// A source keeps when it was created through a rename and a restart, and one stored before
// state.db kept that opens in the same schema version and sends it as null.
func TestASourceKeepsWhenItWasAdded(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	added := r.clock
	remoteSource(t, r, "s")
	r.clock = r.clock.Add(time.Hour)
	if _, err := r.c.accept(context.Background(), PutSource{ID: "s", Expected: 1, Input: SourceInput{Name: "Renamed", Remote: &RemoteInput{RefreshEvery: 6 * time.Hour}}}); err != nil {
		t.Fatal(err)
	}
	if got := sourceViewOf(t, r, "s").AddedAt; got == nil || !got.Equal(added) {
		t.Errorf("addedAt after a rename = %v, want %v", got, added)
	}
	r.close()
	r = newRigAt(t, dir, r.clock.Add(time.Minute))
	if got := sourceViewOf(t, r, "s").AddedAt; got == nil || !got.Equal(added) {
		t.Errorf("addedAt after a restart = %v, want %v", got, added)
	}
	mustExec(t, r.c.store.writer, `UPDATE sources SET body=json_remove(body, '$.added')`)
	r.close()
	r = newRigAt(t, dir, r.clock.Add(time.Minute))
	if view := sourceViewOf(t, r, "s"); view.AddedAt != nil || view.Name != "Renamed" {
		t.Errorf("a source stored without its added time reads as %+v, want addedAt null", view)
	}
	body, _ := json.Marshal(sourceViewOf(t, r, "s"))
	if !strings.Contains(string(body), `"addedAt":null`) {
		t.Errorf("the view sends %s, want addedAt null", body)
	}
}

// post sends a route that takes the CSRF check only, so it carries no key.
func post(t *testing.T, base, path, csrf, body string) answer {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+DefaultBasePath+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(CSRFHeader, csrf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header.Get("ETag"), string(text)}
}

func manualBody(t *testing.T, name string, text *string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"kind": "manual", "text": text})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"name":%q,"origin":%s}`, name, encoded)
}

func decoded[T any](t *testing.T, a answer) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(a.body), &out); err != nil {
		t.Fatalf("%s: %v", a.body, err)
	}
	return out
}

func TestAPastedSourceThroughTheAPI(t *testing.T) {
	h, server := newPageDaemon(t)
	create := map[string]string{"If-None-Match": "*"}
	text := acmeText(t)
	put := func(key string, headers map[string]string, body string) answer {
		return mutate(t, server.URL, http.MethodPut, "api/sources/acme", key, headers, body)
	}

	empty := "[]"
	if refused := put("k0", create, manualBody(t, "Acme", &empty)); refused.status != http.StatusUnprocessableEntity {
		t.Errorf("an empty paste: %+v", refused)
	}
	garbage := `[{"outbounds":[{"tag":"a","protocol":"tuic"}]}]`
	refused := put("k1", create, manualBody(t, "Acme", &garbage))
	if refused.status != http.StatusUnprocessableEntity || !strings.Contains(refused.body, string(catalogue.CodeUnsupportedProtocol)) {
		t.Errorf("a paste with no usable servers: %+v", refused)
	}
	if none := put("k2", create, manualBody(t, "Acme", nil)); none.status != http.StatusUnprocessableEntity {
		t.Errorf("a new source with no text: %+v", none)
	}

	withRule := `{"name":"Acme","origin":{"kind":"manual","text":"[]"},"pick":null}`
	if refused := put("k8", create, withRule); refused.status != http.StatusBadRequest {
		t.Errorf("a source with a rule: %+v", refused)
	}
	for i, path := range []string{"api/sources/acme/preview", "api/sources/preview"} {
		if gone := mutate(t, server.URL, http.MethodPost, path, fmt.Sprintf("p%d", i), nil, `{"text":"[]"}`); gone.status != http.StatusNotFound {
			t.Errorf("%s: %+v, want no such route", path, gone)
		}
	}

	created := put("k3", create, manualBody(t, "Acme", &text))
	if created.status != http.StatusOK {
		t.Fatalf("create: %+v", created)
	}
	view := decoded[SourceView](t, created)
	if view.Origin.Kind != "manual" || view.Origin.RemoteOriginDTO != nil || view.Revision != 1 || view.Stored != 12 || view.Feed == nil || len(view.Feed.Configs) != 6 {
		t.Fatalf("view = %+v", view)
	}
	if strings.Contains(created.body, "aaaaaaaa-") || strings.Contains(created.body, "fixture.example.net") {
		t.Error("the answer carries pasted text")
	}

	app, _ := h.current()
	state, _ := json.Marshal(app.Snapshot())
	if strings.Contains(string(state), "aaaaaaaa-") || !strings.Contains(string(state), `"kind":"manual"`) {
		t.Errorf("the published state carries pasted text or lost its origin")
	}
	page, err := app.Servers(context.Background(), PageQuery{Source: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if len(item.Seen) == 0 || item.Country.Source != "tagFlag" {
			t.Errorf("%s: seen %+v country %+v", item.Label, item.Seen, item.Country)
		}
	}
	if len(page.Items) != 12 {
		t.Errorf("servers: %d, want 12", len(page.Items))
	}

	if refresh := mutate(t, server.URL, http.MethodPost, "api/sources/acme/refresh", "k4", nil, ""); refresh.status != http.StatusUnprocessableEntity {
		t.Errorf("refreshing a pasted source: %+v", refresh)
	}
	ifMatch := map[string]string{"If-Match": fmt.Sprintf(`"%d"`, view.Revision)}
	remote := `{"name":"Acme","origin":{"kind":"remote","url":"https://example.com/f","userAgent":"","refreshEverySeconds":3600,"paused":false}}`
	if changed := put("k5", ifMatch, remote); changed.status != http.StatusUnprocessableEntity {
		t.Errorf("changing origin: %+v", changed)
	}

	renamed := decoded[SourceView](t, put("k6", ifMatch, manualBody(t, "Renamed", nil)))
	if renamed.Name != "Renamed" || renamed.Stored != 12 {
		t.Errorf("a null text kept nothing: %+v", renamed)
	}

	idsBefore := map[domain.LineageID]string{}
	for _, item := range page.Items {
		idsBefore[item.ID] = item.Label
	}
	next := rotated(text)
	ifMatch = map[string]string{"If-Match": fmt.Sprintf(`"%d"`, renamed.Revision)}
	saved := decoded[SourceView](t, put("k7", ifMatch, manualBody(t, "Renamed", &next)))
	if saved.Stored != 12 {
		t.Errorf("new text: %+v", saved)
	}
	again, err := app.Servers(context.Background(), PageQuery{Source: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range again.Items {
		if idsBefore[item.ID] != item.Label {
			t.Errorf("new text lost a lineage: %s %q, was %q", item.ID, item.Label, idsBefore[item.ID])
		}
	}
}

func TestASubscriptionsNameFollowsItsProviderUntilTyped(t *testing.T) {
	_, server := newPageDaemon(t)
	body := func(name string) string {
		return fmt.Sprintf(`{"name":%q,"origin":{"kind":"remote","url":"https://sub.example.net/secret-token","userAgent":"Happ/3.0.0","refreshEverySeconds":0,"paused":false}}`, name)
	}
	created := mutate(t, server.URL, http.MethodPut, "api/sources/s", "k1", map[string]string{"If-None-Match": "*"}, body(" "))
	if created.status != http.StatusOK {
		t.Fatalf("create: %+v", created)
	}
	if view := decoded[SourceView](t, created); view.Name != "sub.example.net" || view.NameSet {
		t.Errorf("no name, no title: %q set=%t", view.Name, view.NameSet)
	}
	if strings.Contains(created.body, "secret-token") {
		t.Error("the answer carries the URL's token")
	}
	typed := domain.Source{Name: "Mine", Origin: domain.RemoteOrigin(domain.Remote{Host: "sub.example.net"})}
	if got := sourceName(typed, "Acme"); got != "Mine" {
		t.Errorf("a typed name = %q", got)
	}
	typed.Name = ""
	if got := sourceName(typed, "Acme"); got != "Acme" {
		t.Errorf("a provider's title = %q", got)
	}
	if refused := mutate(t, server.URL, http.MethodPut, "api/sources/m", "k2", map[string]string{"If-None-Match": "*"}, manualBody(t, " ", ptrTo(acmeText(t)))); refused.status != http.StatusUnprocessableEntity {
		t.Errorf("pasted text with no name: %+v", refused)
	}
}

func TestASourcesRefreshesComeNewestFirst(t *testing.T) {
	h, server := newPageDaemon(t)
	text := acmeText(t)
	h.paused(func(app *App) {
		refreshed(t, app.control, "aurora", parsedAcme(t, text), time.Now().Add(-time.Hour))
		refreshed(t, app.control, "aurora", parsedAcme(t, rotated(text)), time.Now())
	})
	got := decoded[[]RefreshEventDTO](t, read(t, server.URL, "api/sources/aurora/refreshes"))
	if len(got) != 2 || got[0].Outcome != "updated" || !got[0].At.After(got[1].At) || got[1].Added != 12 || got[0].FailureCode != nil {
		t.Errorf("refreshes = %+v", got)
	}
	if gone := read(t, server.URL, "api/sources/nobody/refreshes"); gone.status != http.StatusNotFound {
		t.Errorf("a source that isn't there: %+v", gone)
	}
}

func read(t *testing.T, base, path string) answer {
	t.Helper()
	resp, err := http.Get(base + DefaultBasePath + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header.Get("ETag"), string(text)}
}

// A new source gets a device ID of its own and the default User-Agent, its URL comes back to the
// owner on its own route, and two sources named after one provider are told apart by a number.
func TestASourceHasItsOwnDeviceAndItsURLComesBack(t *testing.T) {
	h, server := newPageDaemon(t)
	create := map[string]string{"If-None-Match": "*"}
	body := func(host string) string {
		return fmt.Sprintf(`{"name":"","origin":{"kind":"remote","url":"https://%s/token","userAgent":"","refreshEverySeconds":0,"paused":false}}`, host)
	}
	first := decoded[SourceView](t, mutate(t, server.URL, http.MethodPut, "api/sources/one", "k1", create, body("aurora.example")))
	second := decoded[SourceView](t, mutate(t, server.URL, http.MethodPut, "api/sources/two", "k2", create, body("aurora.example")))
	if first.Origin.UserAgent != catalogue.DefaultUserAgent || first.Name != "aurora.example" || second.Name != "aurora.example 2" {
		t.Errorf("user agent %q, names %q and %q", first.Origin.UserAgent, first.Name, second.Name)
	}
	var devices []string
	h.paused(func(app *App) {
		for _, id := range []domain.SourceID{"one", "two"} {
			remote, _ := app.control.state.sources[id].Origin.Remote()
			devices = append(devices, app.control.state.secrets[remote.HWID].Reveal())
		}
	})
	if len(devices[0]) != 32 || devices[0] == devices[1] {
		t.Errorf("device IDs %q", devices)
	}
	url := decoded[struct {
		URL string `json:"url"`
	}](t, read(t, server.URL, "api/sources/one/url"))
	if url.URL != "https://aurora.example/token" {
		t.Errorf("url %q", url.URL)
	}
	if gone := read(t, server.URL, "api/sources/nobody/url"); gone.status != http.StatusNotFound {
		t.Errorf("no such source: %+v", gone)
	}
}

func sourceViewOf(t *testing.T, r *rig, id domain.SourceID) SourceView {
	t.Helper()
	r.c.publish(r.clock)
	for _, s := range r.app.Snapshot().Sources {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no source %s in the view", id)
	return SourceView{}
}

func configsReadAs(t *testing.T, view SourceView, want ...string) {
	t.Helper()
	got := []string{}
	for i, c := range view.Feed.Configs {
		if c.Place != i+1 {
			t.Errorf("config %s at row %d says place %d", c.Name, i+1, c.Place)
		}
		got = append(got, c.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("configs %q, want %q", got, want)
	}
}

// reorderedConfigs lists the document's configs last first.
func reorderedConfigs(t *testing.T, text string) string {
	t.Helper()
	var configs []json.RawMessage
	if err := json.Unmarshal([]byte(text), &configs); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(configs)
	out, err := json.Marshal(configs)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A source sends its order as a group does: its servers, best first, and firstChoice is the
// first of them. Its configs come in the order the provider lists them, across a restart, and a
// refresh that lists them in another order moves them.
func TestASourceSendsItsOrderAndItsConfigsInDocumentOrder(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	remoteSource(t, r, "acme")
	refreshed(t, r.c, "acme", parsedAcme(t, acmeText(t)), r.clock)
	pings := map[string]int{"Germany 🇩🇪 · proxy": 60, "Netherlands 🇳🇱 · proxy": 20, "Sweden 🇸🇪 · proxy": 50, "Bulgaria 🇧🇬 · proxy": 40, "USA 🇺🇸 · proxy": 90, "Romania 🇷🇴 · proxy": 30}
	var tests []domain.Signal
	withPing := map[int]domain.LineageID{}
	for _, l := range r.c.world.catalogue.All() {
		if ping, ok := pings[l.Label]; ok {
			tests = append(tests, r.tested(string(l.ID), ping))
			withPing[ping] = l.ID
		}
	}
	r.frame(domain.WANUp, tests...)
	var want []domain.LineageID
	for _, ping := range slices.Sorted(maps.Keys(withPing)) {
		want = append(want, withPing[ping])
	}
	view := sourceViewOf(t, r, "acme")
	if len(view.Order) != 12 || !slices.Equal(view.Order[:6], want) {
		t.Errorf("order %v, want all 12 servers, the six with a ping first by ping %v", view.Order, want)
	}
	if view.FirstChoice == nil || len(view.Order) == 0 || *view.FirstChoice != view.Order[0] {
		t.Errorf("first choice %v is not the first of order %v", view.FirstChoice, view.Order)
	}
	inDocument := []string{"Germany", "Netherlands", "Sweden", "Bulgaria", "USA", "Romania"}
	configsReadAs(t, view, inDocument...)

	r.close()
	r = newRigAt(t, dir, r.clock.Add(time.Minute))
	configsReadAs(t, sourceViewOf(t, r, "acme"), inDocument...)

	refreshed(t, r.c, "acme", parsedAcme(t, reorderedConfigs(t, acmeText(t))), r.clock)
	reversed := slices.Clone(inDocument)
	slices.Reverse(reversed)
	configsReadAs(t, sourceViewOf(t, r, "acme"), reversed...)
	r.close()
	r = newRigAt(t, dir, r.clock.Add(time.Minute))
	configsReadAs(t, sourceViewOf(t, r, "acme"), reversed...)
}

// A group's rule saves its protocol condition, the page reads it back, it resolves by it, and it
// comes back after a restart.
func TestAGroupsRuleSavesItsProtocol(t *testing.T) {
	_, server := newPageDaemon(t)
	body := func(not bool) string {
		return fmt.Sprintf(`{"name":"VLESS","members":{"sources":{"not":false,"values":["aurora"]},"countries":{"not":false,"values":[]},"protocols":{"not":%t,"values":["vless"]},"include":"","exclude":"","box":null}}`, not)
	}
	created := mutate(t, server.URL, http.MethodPut, "api/groups/vless", "g1", map[string]string{"If-None-Match": "*"}, strings.TrimSuffix(body(false), "}")+`,"ticked":[]}`)
	group := decoded[GroupView](t, created)
	if created.status != http.StatusOK || !slices.Equal(group.Members.Protocols.Values, []string{"vless"}) || group.Members.Protocols.Not || len(group.Resolved) == 0 {
		t.Fatalf("created %+v", created)
	}
	state := decoded[View](t, read(t, server.URL, "api/state"))
	i := slices.IndexFunc(state.Groups, func(g GroupView) bool { return g.ID == "vless" })
	if i < 0 || !slices.Equal(state.Groups[i].Members.Protocols.Values, []string{"vless"}) {
		t.Fatalf("api/state has the group as %+v", state.Groups)
	}
	updated := mutate(t, server.URL, http.MethodPut, "api/groups/vless", "g2", map[string]string{"If-Match": fmt.Sprintf(`"%d"`, group.Revision)}, body(true))
	if got := decoded[GroupView](t, updated); updated.status != http.StatusOK || !got.Members.Protocols.Not || len(got.Resolved) != 0 {
		t.Errorf("none of vless over vless servers: %+v", updated)
	}

	dir := t.TempDir()
	r := newRig(t, dir)
	r.seed(lineage(t, "a", "s", "DE", "a.example"))
	members, err := domain.AdmitMembers(domain.Members{Sources: domain.Among[domain.SourceID]{Values: []domain.SourceID{"s"}}, Protocols: domain.Among[domain.Protocol]{Values: []domain.Protocol{domain.VLESS}}})
	if err != nil {
		t.Fatal(err)
	}
	r.putGroup("g", "G", members)
	r.close()
	r = newRigAt(t, dir, r.clock)
	if got := r.c.world.groups["g"].Members.Protocols; !slices.Equal(got.Values, []domain.Protocol{domain.VLESS}) {
		t.Errorf("after a restart the group's protocols are %+v", got)
	}
}

// A tick saves at once over the page's route and moves no member while the box condition is unset.
// The preview's matched leaves the box to the page. A rule edit sets the box condition, and the
// members are then the ticked servers the other conditions match, or the unticked ones.
func TestATickChangesNoMemberUntilTheBoxConditionIsSet(t *testing.T) {
	_, server := newPageDaemon(t)
	members := func(box string) string {
		return `{"sources":{"not":false,"values":["aurora"]},"countries":{"not":false,"values":[]},"protocols":{"not":false,"values":[]},"include":"","exclude":"","box":` + box + `}`
	}
	created := mutate(t, server.URL, http.MethodPut, "api/groups/eu", "g1", map[string]string{"If-None-Match": "*"}, `{"name":"EU","members":`+members("null")+`,"ticked":[]}`)
	group := decoded[GroupView](t, created)
	all := group.Resolved
	if created.status != http.StatusOK || len(all) < 2 || !slices.Contains(all, "l-par") {
		t.Fatalf("created %+v", created)
	}
	ticked := mutate(t, server.URL, http.MethodPut, "api/groups/eu/ticked/l-par", "t1", nil, `{"ticked":true}`)
	after := decoded[GroupView](t, ticked)
	if ticked.status != http.StatusOK || !slices.Equal(after.Ticked, []domain.LineageID{"l-par"}) || !slices.Equal(after.Resolved, all) || after.Revision != group.Revision {
		t.Fatalf("a tick with the box unset: %+v", ticked)
	}
	state := decoded[View](t, read(t, server.URL, "api/state"))
	if i := slices.IndexFunc(state.Groups, func(g GroupView) bool { return g.ID == "eu" }); i < 0 || !slices.Equal(state.Groups[i].Ticked, []domain.LineageID{"l-par"}) {
		t.Fatalf("api/state has the group as %+v", state.Groups)
	}
	preview := decoded[GroupPreview](t, mutate(t, server.URL, http.MethodPost, "api/groups/preview", "p1", nil, `{"members":`+members(`"ticked"`)+`}`))
	if !slices.Equal(preview.Matched, all) {
		t.Errorf("preview matched %v, want every server the other conditions match, %v", preview.Matched, all)
	}
	edit := func(key, box string, revision domain.GroupRevision) GroupView {
		t.Helper()
		answer := mutate(t, server.URL, http.MethodPut, "api/groups/eu", key, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, revision)}, `{"name":"EU","members":`+members(box)+`}`)
		if answer.status != http.StatusOK {
			t.Fatalf("box %s: %+v", box, answer)
		}
		return decoded[GroupView](t, answer)
	}
	on := edit("g2", `"ticked"`, after.Revision)
	if !slices.Equal(on.Resolved, []domain.LineageID{"l-par"}) || on.Members.Box == nil || *on.Members.Box != "ticked" || !slices.Equal(on.Ticked, after.Ticked) {
		t.Errorf("box is ticked: %+v", on)
	}
	off := edit("g3", `"unticked"`, on.Revision)
	if want := slices.DeleteFunc(slices.Clone(all), func(l domain.LineageID) bool { return l == "l-par" }); !slices.Equal(off.Resolved, want) {
		t.Errorf("box is not ticked holds %v, want %v", off.Resolved, want)
	}
	unticked := decoded[GroupView](t, mutate(t, server.URL, http.MethodPut, "api/groups/eu/ticked/l-par", "t2", nil, `{"ticked":false}`))
	if len(unticked.Ticked) != 0 || !slices.Equal(unticked.Resolved, all) {
		t.Errorf("an untick under box is not ticked: %+v", unticked)
	}
	if bad := mutate(t, server.URL, http.MethodPut, "api/groups/eu", "g4", map[string]string{"If-Match": fmt.Sprintf(`"%d"`, off.Revision)}, `{"name":"EU","members":`+members(`"maybe"`)+`}`); bad.status != http.StatusUnprocessableEntity {
		t.Errorf("a fourth box condition: %+v", bad)
	}
	if gone := mutate(t, server.URL, http.MethodPut, "api/groups/eu/ticked/l-nowhere", "t3", nil, `{"ticked":true}`); gone.status != http.StatusUnprocessableEntity {
		t.Errorf("ticking a server the gateway doesn't know: %+v", gone)
	}
	if gone := mutate(t, server.URL, http.MethodPut, "api/groups/nobody/ticked/l-par", "t4", nil, `{"ticked":true}`); gone.status != http.StatusNotFound {
		t.Errorf("ticking in a group that is gone: %+v", gone)
	}
}

// A create carries the group's name, rule and ticks in one write, so a group made from a draft holds
// its servers from its first revision. It needs a filter, or Box is ticked with a server ticked: a
// create that could hold no server is refused and stores nothing. A replace carries no ticks.
func TestACreateCarriesItsRuleAndTicksInOneWrite(t *testing.T) {
	_, server := newPageDaemon(t)
	members := func(countries, box string) string {
		return `{"sources":{"not":false,"values":[]},"countries":{"not":false,"values":[` + countries + `]},"protocols":{"not":false,"values":[]},"include":"","exclude":"","box":` + box + `}`
	}
	revision := func() domain.IntentRevision { return decoded[View](t, read(t, server.URL, "api/state")).Revision }
	create := func(id, key, members, ticked string) answer {
		return mutate(t, server.URL, http.MethodPut, "api/groups/"+id, key, map[string]string{"If-None-Match": "*"}, `{"name":"`+id+`","members":`+members+`,"ticked":[`+ticked+`]}`)
	}
	for _, want := range []struct {
		name, members, ticked string
		ticks, resolved       []domain.LineageID
	}{
		{"rule", members(`"NL"`, "null"), ``, nil, []domain.LineageID{"l-ams"}},
		{"ticks", members(``, `"ticked"`), `"l-par","l-ams"`, []domain.LineageID{"l-ams", "l-par"}, []domain.LineageID{"l-ams", "l-par"}},
		{"both", members(`"NL"`, "null"), `"l-par"`, []domain.LineageID{"l-par"}, []domain.LineageID{"l-ams"}},
		{"absent", members(`"SE"`, "null"), ``, nil, nil},
	} {
		before := revision()
		created := create(want.name, "c-"+want.name, want.members, want.ticked)
		group := decoded[GroupView](t, created)
		if created.status != http.StatusOK || !slices.Equal(group.Ticked, want.ticks) || !slices.Equal(group.Resolved, want.resolved) || group.Revision != 1 {
			t.Errorf("%s: %+v, want ticks %v holding %v at revision 1", want.name, created, want.ticks, want.resolved)
		}
		if after := revision(); after != before+1 {
			t.Errorf("%s: the view moved from %d to %d, want one write", want.name, before, after)
		}
		state := decoded[View](t, read(t, server.URL, "api/state"))
		if i := slices.IndexFunc(state.Groups, func(g GroupView) bool { return g.ID == domain.GroupID(want.name) }); i < 0 || !slices.Equal(state.Groups[i].Ticked, want.ticks) {
			t.Errorf("%s: api/state has %+v", want.name, state.Groups)
		}
	}
	for _, want := range []struct{ name, members, ticked string }{
		{"nothing", members(``, "null"), ``},
		{"ticks-without-box", members(``, "null"), `"l-ams"`},
		{"box-without-ticks", members(``, `"ticked"`), ``},
		{"boxed-country-without-ticks", members(`"NL"`, `"ticked"`), ``},
	} {
		refused := create(want.name, "n-"+want.name, want.members, want.ticked)
		if refused.status != http.StatusUnprocessableEntity || !strings.Contains(refused.body, `"reason":"ruleMissing"`) {
			t.Errorf("%s: %+v, want ruleMissing", want.name, refused)
		}
		state := decoded[View](t, read(t, server.URL, "api/state"))
		if slices.ContainsFunc(state.Groups, func(g GroupView) bool { return g.ID == domain.GroupID(want.name) }) {
			t.Errorf("%s: a refused create stored a group", want.name)
		}
	}
	if gone := create("gone", "g-gone", members(``, `"ticked"`), `"l-nowhere"`); gone.status != http.StatusUnprocessableEntity || !strings.Contains(gone.body, `"reason":"serverGone"`) {
		t.Errorf("a create ticking a server the gateway doesn't know: %+v", gone)
	}
	if twice := create("twice", "g-twice", members(``, `"ticked"`), `"l-ams","l-ams"`); twice.status != http.StatusUnprocessableEntity || !strings.Contains(twice.body, `"reason":"malformed"`) {
		t.Errorf("a create ticking a server twice: %+v", twice)
	}
	for name, body := range map[string]string{"untold": `{"name":"untold","members":` + members(`"NL"`, "null") + `}`, "null": `{"name":"null","members":` + members(`"NL"`, "null") + `,"ticked":null}`} {
		if old := mutate(t, server.URL, http.MethodPut, "api/groups/"+name, "o-"+name, map[string]string{"If-None-Match": "*"}, body); old.status != http.StatusBadRequest || !strings.Contains(old.body, `"reason":"malformed"`) {
			t.Errorf("a create whose ticked is %s: %+v", name, old)
		}
	}
	replace := mutate(t, server.URL, http.MethodPut, "api/groups/ticks", "r-ticks", map[string]string{"If-Match": `"1"`}, `{"name":"ticks","members":`+members(``, `"ticked"`)+`,"ticked":[]}`)
	if replace.status != http.StatusBadRequest || !strings.Contains(replace.body, `"reason":"malformed"`) {
		t.Errorf("a replace carrying ticks: %+v", replace)
	}
	renamed := decoded[GroupView](t, mutate(t, server.URL, http.MethodPut, "api/groups/ticks", "r-name", map[string]string{"If-Match": `"1"`}, `{"name":"Picked","members":`+members(``, `"ticked"`)+`}`))
	if renamed.Name != "Picked" || !slices.Equal(renamed.Ticked, []domain.LineageID{"l-ams", "l-par"}) {
		t.Errorf("a replace keeps the ticks the create stored: %+v", renamed)
	}
}

// A server whose remarks and tag are each a flag alone is unnamed, so the page reads "Server" with no
// name rather than the dot between two flags it strips. Its flag gives the country.
func TestAServerNamedByFlagsAloneIsPublishedUnnamed(t *testing.T) {
	doc, err := catalogue.Parse([]byte(`[{"remarks":"🇸🇪","outbounds":[{"tag":"🇸🇪","protocol":"trojan","settings":{"servers":[{"address":"se.example.net","port":443,"password":"pw"}]}}]}]`),
		catalogue.Detect, catalogue.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	rec := catalogue.Reconcile(nil, doc, "s", 1, 1, t0, time.Hour)
	if len(rec.Changes) != 1 {
		t.Fatalf("%d servers, want one", len(rec.Changes))
	}
	view := serverView(rec.Changes[0].Lineage)
	body, _ := json.Marshal(struct {
		Label string   `json:"label"`
		Name  *NameDTO `json:"name"`
	}{view.Label, view.Name})
	if view.Label != "Server" || view.Name != nil || view.Home == nil || view.Country.Code == nil || *view.Country.Code != "SE" {
		t.Errorf("published %s, home %v, country %+v, want Server with no name at its home in SE", body, view.Home, view.Country)
	}
}

// A source deleted while its fetch is out and saved again under the same ID waits for that fetch,
// which comes back dropped, and is then fetched once, at its new revision.
func TestASourceSavedAgainMidFetchIsFetchedOnce(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	remoteSource(t, r, "acme")
	gone := r.c.state.sources["acme"]
	if _, err := r.c.accept(ctx, DeleteSource{ID: "acme", Expected: domain.IntentRevision(gone.Revision)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.accept(ctx, PutSource{ID: "acme", Input: SourceInput{Name: "Acme", Remote: &RemoteInput{URL: domain.NewSecret("https://example.com/f"), RefreshEvery: 6 * time.Hour}}}); err != nil {
		t.Fatal(err)
	}
	r.c.startFetches(r.clock)
	select {
	case job := <-r.c.fetchJobs:
		t.Fatalf("a second fetch of the source started while its first was out: %+v", job.Basis())
	default:
	}
	before := r.c.state.catalogueRevision
	if err := r.c.applyRefresh(ctx, catalogue.Refresh{Basis: catalogue.Basis{Source: "acme", Revision: gone.Revision}, Outcome: catalogue.Updated, Document: parsedAcme(t, acmeText(t)), Next: checkedAt(r.clock)}); err != nil {
		t.Fatal(err)
	}
	if got := r.c.state.catalogueRevision; got != before {
		t.Fatalf("the deleted source's fetch was applied: catalogue %d, was %d", got, before)
	}
	r.c.startFetches(r.clock)
	select {
	case job := <-r.c.fetchJobs:
		if job.Basis().Revision != r.c.state.sources["acme"].Revision {
			t.Fatalf("the fetch after it is of %+v, want the new revision", job.Basis())
		}
	default:
		t.Fatal("the source saved again was never fetched")
	}
	r.c.startFetches(r.clock)
	select {
	case job := <-r.c.fetchJobs:
		t.Fatalf("a third fetch started: %+v", job.Basis())
	default:
	}
}

// A source deleted while its fetch is out, saved again paused and refreshed by hand, waits for that
// fetch, which comes back dropped and leaves the refresh asked meanwhile: the source is then fetched
// once, at its new revision.
func TestAPausedSourceSavedAgainMidFetchKeepsItsRefresh(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	remoteSource(t, r, "acme")
	gone := r.c.state.sources["acme"]
	if _, err := r.c.accept(ctx, DeleteSource{ID: "acme", Expected: domain.IntentRevision(gone.Revision)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.accept(ctx, PutSource{ID: "acme", Input: SourceInput{Name: "Acme", Remote: &RemoteInput{URL: domain.NewSecret("https://example.com/f"), RefreshEvery: 6 * time.Hour, Paused: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.accept(ctx, RefreshSource{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	r.c.startFetches(r.clock)
	select {
	case job := <-r.c.fetchJobs:
		t.Fatalf("a second fetch of the source started while its first was out: %+v", job.Basis())
	default:
	}
	if err := r.c.applyRefresh(ctx, catalogue.Refresh{Basis: catalogue.Basis{Source: "acme", Revision: gone.Revision}, Outcome: catalogue.Updated, Document: parsedAcme(t, acmeText(t)), Next: checkedAt(r.clock)}); err != nil {
		t.Fatal(err)
	}
	var fetched []catalogue.Basis
	for range 2 {
		r.c.startFetches(r.clock)
		select {
		case job := <-r.c.fetchJobs:
			fetched = append(fetched, job.Basis())
		default:
		}
	}
	if want := (catalogue.Basis{Source: "acme", Revision: r.c.state.sources["acme"].Revision}); len(fetched) != 1 || fetched[0] != want {
		t.Fatalf("after the dropped fetch the source was fetched as %+v, want once as %+v", fetched, want)
	}
}
