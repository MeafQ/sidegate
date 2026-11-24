// Command controllab drives the real daemon in the lab VM without a browser: it opens
// the same App the page talks to and sends the same commands.
//
//	controllab unit                  print the core unit text
//	controllab serve                 run the daemon with the world in world.json
//	controllab apply FILE            have a running serve apply a change of world, as the page's edits do
//	controllab servers               print a running serve's servers, with supported and health as the page sees them
//	controllab unifi                 stand in for the Network Application, for the page lab
//	controllab switches [--since T]  print switch events, oldest first, one line each
//	controllab summary --from T --to T   per-provider analytics over a window
//	controllab json KIND [--from T --to T]  print switches, analytics, servers or replaced as JSON
//
// serve also writes $RUN/view.json and $RUN/clients.json, and takes one trigger: the file
// $RUN/refresh refreshes every source. It creates what world.json names and is missing,
// so a restarted daemon changes nothing.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"sidegate/internal/daemon"
	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
	"sidegate/lab/labnames"
)

const worldFile = "/etc/unifi-lab/world.json"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: controllab unit|serve|apply|unifi|servers|switches|summary|json")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	since := flags.String("since", "", "only events at or after this RFC 3339 time")
	from := flags.String("from", "", "window start, RFC 3339")
	to := flags.String("to", "", "window end, RFC 3339")
	flags.Parse(os.Args[2:])

	var err error
	switch os.Args[1] {
	case "unit":
		unit := gateway.CoreUnit(gateway.Options{DataDir: labnames.DataDir, RunDir: labnames.RunDir})
		_, err = os.Stdout.Write(unit)
	case "serve":
		err = serve(ctx)
	case "apply":
		err = askToApply(ctx, flags.Arg(0))
	case "unifi":
		err = standIn(ctx)
	case "servers":
		var answer []byte
		if answer, err = ask(ctx, http.MethodGet, nil); err == nil {
			_, err = os.Stdout.Write(answer)
		}
	case "switches":
		err = switches(ctx, *since)
	case "summary":
		err = summary(ctx, *from, *to)
	case "json":
		err = dump(ctx, flags.Arg(0), *from, *to)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "controllab:", err)
		os.Exit(1)
	}
}

// worldSpec is the lab's intent, as a page would have entered it. An Exit is "group:ID", or
// "server:LABEL" where LABEL is part of one server's label, since lineage IDs are hashes.
type worldSpec struct {
	Sources      []sourceSpec `json:"sources"`
	Groups       []groupSpec  `json:"groups"`
	Slots        []slotSpec   `json:"slots"`
	DeleteSlots  []string     `json:"deleteSlots"`
	DeleteGroups []string     `json:"deleteGroups"`
}

type sourceSpec struct {
	ID string `json:"id"`
}

type groupSpec struct {
	ID        string   `json:"id"`
	Countries []string `json:"countries"`
	Include   string   `json:"include"`
	Labels    []string `json:"labels"`
}

// slotSpec's Exit is group:<id>, server:<label>, source:<id>, direct or block.
type slotSpec struct {
	ID   string `json:"id"`
	Exit string `json:"exit"`
}

func serve(ctx context.Context) error {
	b, err := daemon.LoadBootstrap(labnames.DataDir)
	if err != nil {
		return err
	}
	app, err := daemon.Open(ctx, b)
	if err != nil {
		return err
	}
	defer app.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	go func() {
		w := lab{app: app, ctx: ctx}
		if err := w.seed(); err != nil {
			log.Printf("seed: %v", err)
		}
		w.listen()
	}()
	go writeOut(ctx, app)
	return <-done
}

// lab applies world specs to a running App. Requests are never replayed, since each is a new edit.
type lab struct {
	app *daemon.App
	ctx context.Context
}

var requests atomic.Uint64

func request(name string) domain.RequestID {
	return domain.RequestID(fmt.Sprintf("lab-%d-%d-%s", time.Now().UnixNano(), requests.Add(1), name))
}

// seed creates only what the view doesn't have yet.
func (l lab) seed() error {
	body, err := os.ReadFile(worldFile)
	if err != nil {
		return err
	}
	var spec worldSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return err
	}
	view := l.app.Snapshot()
	spec.Sources = slices.DeleteFunc(spec.Sources, func(s sourceSpec) bool {
		return slices.ContainsFunc(view.Sources, func(v daemon.SourceView) bool { return string(v.ID) == s.ID })
	})
	spec.Groups = slices.DeleteFunc(spec.Groups, func(g groupSpec) bool {
		return slices.ContainsFunc(view.Groups, func(v daemon.GroupView) bool { return string(v.ID) == g.ID })
	})
	spec.Slots = slices.DeleteFunc(spec.Slots, func(s slotSpec) bool {
		return slices.ContainsFunc(view.Slots, func(v daemon.SlotView) bool { return string(v.ID) == s.ID })
	})
	return l.apply(spec)
}

// listen takes apply requests on a local socket, for as long as the daemon runs.
func (l lab) listen() {
	socket := filepath.Join(labnames.RunDir, "controllab.sock")
	os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		log.Printf("lab socket: %v", err)
		return
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(daemon.ServerPage{Items: allServers(l.ctx, l.app.Servers)})
			return
		}
		var spec worldSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := l.apply(spec); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
		}
	})}
	go func() {
		<-l.ctx.Done()
		server.Close()
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("lab socket: %v", err)
	}
}

func askToApply(ctx context.Context, file string) error {
	body, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	_, err = ask(ctx, http.MethodPost, strings.NewReader(string(body)))
	return err
}

// ask talks to a running serve over its local socket.
func ask(ctx context.Context, method string, body io.Reader) ([]byte, error) {
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(labnames.RunDir, "controllab.sock"))
	}}}
	req, err := http.NewRequestWithContext(ctx, method, "http://lab/", body)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(answer)))
	}
	return answer, nil
}

func (l lab) apply(spec worldSpec) error {
	for _, id := range spec.DeleteSlots {
		if _, err := l.execute(func() daemon.Command {
			slot, _ := l.slot(id)
			return daemon.DeleteSlot{Request: request("delete-slot"), ID: domain.SlotID(id), Expected: slot.Version}
		}); err != nil {
			return fmt.Errorf("delete slot %s: %w", id, err)
		}
	}
	for _, id := range spec.DeleteGroups {
		if _, err := l.execute(func() daemon.Command {
			group, _ := l.group(id)
			return daemon.DeleteGroup{Request: request("delete-group"), ID: domain.GroupID(id), Expected: domain.IntentRevision(group.Revision)}
		}); err != nil {
			return fmt.Errorf("delete group %s: %w", id, err)
		}
	}
	var fresh []string
	for _, s := range spec.Sources {
		if _, known := l.source(s.ID); !known {
			fresh = append(fresh, s.ID)
		}
		if err := l.putSource(s); err != nil {
			return fmt.Errorf("source %s: %w", s.ID, err)
		}
	}
	for _, id := range fresh {
		if !l.parsed(id) {
			return fmt.Errorf("source %s: no first parse within a minute", id)
		}
	}
	for _, g := range spec.Groups {
		if err := l.putGroup(g); err != nil {
			return fmt.Errorf("group %s: %w", g.ID, err)
		}
	}
	for _, s := range spec.Slots {
		if err := l.putSlot(s); err != nil {
			return fmt.Errorf("slot %s: %w", s.ID, err)
		}
	}
	return nil
}

// execute retries a precondition that lost to the view's next publish, so build reads the revisions anew.
func (l lab) execute(build func() daemon.Command) (daemon.Receipt, error) {
	var receipt daemon.Receipt
	var err error
	for range 30 {
		if receipt, err = l.app.Execute(l.ctx, build()); err == nil || !strings.Contains(err.Error(), "stale") {
			return receipt, err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return receipt, err
}

func (l lab) slot(id string) (daemon.SlotView, bool) {
	view := l.app.Snapshot()
	i := slices.IndexFunc(view.Slots, func(v daemon.SlotView) bool { return string(v.ID) == id })
	if i < 0 {
		return daemon.SlotView{}, false
	}
	return view.Slots[i], true
}

func (l lab) group(id string) (daemon.GroupView, bool) {
	view := l.app.Snapshot()
	i := slices.IndexFunc(view.Groups, func(v daemon.GroupView) bool { return string(v.ID) == id })
	if i < 0 {
		return daemon.GroupView{}, false
	}
	return view.Groups[i], true
}

func (l lab) putSource(s sourceSpec) error {
	_, err := l.execute(func() daemon.Command {
		input := daemon.SourceInput{Name: s.ID, Remote: &daemon.RemoteInput{URL: domain.NewSecret(labnames.Feeds + s.ID)}}
		cmd := daemon.PutSource{Request: request("source-" + s.ID), ID: domain.SourceID(s.ID), Input: input}
		if v, ok := l.source(s.ID); ok {
			cmd.Expected = domain.IntentRevision(v.Revision)
		}
		return cmd
	})
	return err
}

func (l lab) putGroup(g groupSpec) error {
	members, ticked, err := l.members(g)
	if err != nil {
		return err
	}
	_, err = l.execute(func() daemon.Command {
		cmd := daemon.PutGroup{Request: request("group-" + g.ID), ID: domain.GroupID(g.ID), Name: g.ID, Members: members, Ticked: slices.Compact(slices.Sorted(slices.Values(ticked)))}
		if group, ok := l.group(g.ID); ok {
			cmd.Expected, cmd.Ticked = domain.IntentRevision(group.Revision), nil
		}
		return cmd
	})
	if err != nil {
		return err
	}
	group, _ := l.group(g.ID)
	ticks := map[domain.LineageID]bool{}
	for _, id := range group.Ticked {
		ticks[id] = false
	}
	for _, id := range ticked {
		ticks[id] = true
	}
	for _, id := range slices.Sorted(maps.Keys(ticks)) {
		if _, err := l.execute(func() daemon.Command {
			return daemon.TickServer{Request: request(fmt.Sprintf("tick-%s-%s-%t", g.ID, id, ticks[id])), Group: domain.GroupID(g.ID), Server: id, Ticked: ticks[id]}
		}); err != nil {
			return err
		}
	}
	return nil
}

// members is a group's rule, and the servers it ticks: its labels, when it names them, as "box is
// ticked" alone.
func (l lab) members(g groupSpec) (domain.Members, []domain.LineageID, error) {
	if len(g.Labels) > 0 {
		ids := make([]domain.LineageID, 0, len(g.Labels))
		for _, label := range g.Labels {
			id := l.lineage(label)
			if id == "" {
				return domain.Members{}, nil, fmt.Errorf("no server is labelled %q", label)
			}
			ids = append(ids, id)
		}
		return domain.Members{Box: domain.BoxTicked}, ids, nil
	}
	names, err := domain.NewFilter(g.Include, "")
	if err != nil {
		return domain.Members{}, nil, err
	}
	m := domain.Members{Countries: domain.Among[string]{Values: g.Countries}, Names: names}
	if !m.Conditioned() {
		for _, s := range l.app.Snapshot().Sources {
			m.Sources.Values = append(m.Sources.Values, s.ID)
		}
	}
	m, err = domain.AdmitMembers(m)
	return m, nil, err
}

func (l lab) putSlot(s slotSpec) error {
	var exit domain.Exit
	switch kind, value, _ := strings.Cut(s.Exit, ":"); kind {
	case "direct":
		exit = domain.DirectExit()
	case "block":
		exit = domain.BlockExit()
	case "source":
		exit = domain.SourceExit(domain.SourceID(value))
	case "group":
		exit = domain.GroupExit(domain.GroupID(value))
	case "server":
		id := l.lineage(value)
		if id == "" {
			return fmt.Errorf("no server is labelled %q", value)
		}
		exit = domain.ServerExit(id)
	default:
		return fmt.Errorf("unknown exit %q", s.Exit)
	}
	intent, err := domain.NewIntent(s.ID, exit)
	if err != nil {
		return err
	}
	_, err = l.execute(func() daemon.Command {
		cmd := daemon.PutSlot{Request: request("slot-" + s.ID), ID: domain.SlotID(s.ID), Intent: intent}
		if slot, ok := l.slot(s.ID); ok {
			cmd.Expected = slot.Version
		}
		return cmd
	})
	return err
}

func (l lab) source(id string) (daemon.SourceView, bool) {
	view := l.app.Snapshot()
	i := slices.IndexFunc(view.Sources, func(v daemon.SourceView) bool { return string(v.ID) == id })
	if i < 0 {
		return daemon.SourceView{}, false
	}
	return view.Sources[i], true
}

// parsed waits up to a minute for a new source's first parse, since a group or source Exit with no
// server yet is refused.
func (l lab) parsed(id string) bool {
	for deadline := time.Now().Add(time.Minute); l.ctx.Err() == nil && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if s, _ := l.source(id); s.Feed != nil {
			return true
		}
	}
	return false
}

// lineage waits up to a minute for a server whose label holds label, for a feed still being fetched.
func (l lab) lineage(label string) domain.LineageID {
	for deadline := time.Now().Add(time.Minute); l.ctx.Err() == nil && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		for _, s := range allServers(l.ctx, l.app.Servers) {
			if strings.Contains(s.Label, label) {
				return s.ID
			}
		}
	}
	return ""
}

func allServers(ctx context.Context, read func(context.Context, daemon.PageQuery) (daemon.ServerPage, error)) []daemon.ServerView {
	var out []daemon.ServerView
	for cursor := daemon.Cursor(""); ; {
		page, err := read(ctx, daemon.PageQuery{Cursor: cursor})
		if err != nil {
			return out
		}
		out = append(out, page.Items...)
		if page.Next == "" {
			return out
		}
		cursor = page.Next
	}
}

// writeOut publishes what the shell steps read, and watches for their triggers.
func writeOut(ctx context.Context, app *daemon.App) {
	for ctx.Err() == nil {
		view := app.Snapshot()
		writeJSON("view.json", view)
		clients := map[string]map[string]string{}
		for _, slot := range view.Slots {
			if c, ok := app.ClientConfigOf(slot.ID); ok {
				clients[string(slot.ID)] = map[string]string{"private": c.PrivateKey.Reveal(), "hostPublic": c.HostPublicKey, "address": c.Address, "endpoint": c.Endpoint}
			}
		}
		writeJSON("clients.json", clients)
		if _, err := os.Stat(filepath.Join(labnames.RunDir, "refresh")); err == nil {
			os.Remove(filepath.Join(labnames.RunDir, "refresh"))
			for _, source := range view.Sources {
				if _, err := app.Execute(ctx, daemon.RefreshSource{Request: request("refresh"), ID: source.ID}); err != nil {
					log.Printf("refresh %s: %v", source.ID, err)
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func writeJSON(name string, value any) {
	body, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		return
	}
	tmp := filepath.Join(labnames.RunDir, "."+name)
	if os.WriteFile(tmp, body, 0o600) == nil {
		os.Rename(tmp, filepath.Join(labnames.RunDir, name))
	}
}

func queries() (*daemon.Queries, error) {
	b, err := daemon.LoadBootstrap(labnames.DataDir)
	if err != nil {
		return nil, err
	}
	return daemon.OpenQueries(b)
}

// labels names a lineage by its feed label, since the IDs are hashes.
func labels(ctx context.Context, q *daemon.Queries) func(domain.LineageID) string {
	names := map[domain.LineageID]string{}
	for _, l := range allServers(ctx, q.Servers) {
		names[l.ID] = strings.ReplaceAll(l.Label, " ", "/")
	}
	return func(id domain.LineageID) string {
		if name, ok := names[id]; ok {
			return name
		}
		return string(id)
	}
}

func switches(ctx context.Context, since string) error {
	q, err := queries()
	if err != nil {
		return err
	}
	defer q.Close()
	events, err := q.Switches(ctx)
	if err != nil {
		return err
	}
	var from time.Time
	if since != "" {
		if from, err = time.Parse(time.RFC3339Nano, since); err != nil {
			return err
		}
	}
	label := labels(ctx, q)
	named := func(s daemon.SelectionDTO) string {
		switch {
		case s.Kind == "down":
			return "down(" + s.Reason + ")"
		case s.Server == nil:
			return s.Kind
		}
		return "[" + label(*s.Server) + "]"
	}
	slices.Reverse(events)
	for _, e := range events {
		if e.RequestedAt.Before(from) {
			continue
		}
		applied := "unapplied"
		if e.AppliedAt != nil {
			applied = strconv.FormatFloat(float64(e.AppliedAt.UnixMilli())/1000, 'f', 3, 64)
		}
		evidence := make([]string, 0, len(e.Evidence))
		for _, ev := range e.Evidence {
			latency := "-"
			if ev.PingMs != nil {
				latency = fmt.Sprintf("%.0fms", *ev.PingMs)
			}
			evidence = append(evidence, fmt.Sprintf("%s:%s:%s:r%d:%s", label(ev.Server), ev.Kind, ev.Verdict, ev.Round, latency))
		}
		fmt.Printf("%s applied=%s slot=%s reason=%s %s -> %s evidence=[%s]\n", e.RequestedAt.Format("15:04:05.000"), applied, strings.Join(slotNames(e.Slots), ","), e.Reason,
			named(e.From), named(e.To), strings.Join(evidence, " "))
	}
	return nil
}

func window(from, to string) (time.Time, time.Time, error) {
	start, err := time.Parse(time.RFC3339Nano, from)
	if err != nil {
		return start, start, err
	}
	end, err := time.Parse(time.RFC3339Nano, to)
	return start, end, err
}

// summary folds a window's buckets per provider, the way the page's summarize does.
func summary(ctx context.Context, from, to string) error {
	q, err := queries()
	if err != nil {
		return err
	}
	defer q.Close()
	start, end, err := window(from, to)
	if err != nil {
		return err
	}
	report, err := q.Analytics(ctx, daemon.AnalyticsQuery{From: start, To: end, Bucket: 5 * time.Minute}, time.Now())
	if err != nil {
		return err
	}
	type total struct {
		attempts, successes, excluded, unknown, forced, up, down uint64
		latency                                                  []float64
	}
	totals := map[string]*total{}
	for _, b := range report.Buckets {
		t := totals[string(b.Source)]
		if t == nil {
			t = &total{}
			totals[string(b.Source)] = t
		}
		t.attempts += b.ProbeAttempts
		t.successes += b.ProbeSuccesses
		t.excluded += b.WANExcluded
		t.unknown += b.Unknown
		t.forced += b.ForcedSwitchAways
		t.up += b.Uploaded
		t.down += b.Downloaded
		if b.PingMedianMs != nil {
			t.latency = append(t.latency, *b.PingMedianMs)
		}
	}
	median := func(values []float64) string {
		if len(values) == 0 {
			return "null"
		}
		slices.Sort(values)
		return fmt.Sprintf("%.2f", values[len(values)/2])
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range totals {
			if !yield(k) {
				return
			}
		}
	}) {
		t := totals[name]
		availability := "null"
		if t.attempts > 0 {
			availability = fmt.Sprintf("%.3f", float64(t.successes)/float64(t.attempts))
		}
		fmt.Printf("provider=%s availability=%s attempts=%d successes=%d wanExcluded=%d unknown=%d latencyMs=%s forcedAways=%d up=%d down=%d\n",
			name, availability, t.attempts, t.successes, t.excluded, t.unknown, median(t.latency), t.forced, t.up, t.down)
	}
	fmt.Printf("coveredUntil=%s gaps=%d\n", report.CoveredUntil.Format(time.RFC3339), report.GapCount)
	return nil
}

func dump(ctx context.Context, kind, from, to string) error {
	q, err := queries()
	if err != nil {
		return err
	}
	defer q.Close()
	var value any
	switch kind {
	case "switches":
		value, err = q.Switches(ctx)
	case "servers":
		value = daemon.ServerPage{Items: allServers(ctx, q.Servers)}
	case "replaced":
		value, err = q.Replaced(ctx)
	case "analytics":
		start, end, werr := window(from, to)
		if werr != nil {
			return werr
		}
		value, err = q.Analytics(ctx, daemon.AnalyticsQuery{From: start, To: end, Bucket: 5 * time.Minute}, time.Now())
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(value)
}

func slotNames(ids []domain.SlotID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}
