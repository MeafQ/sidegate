package daemon

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
	"sidegate/internal/unifi"
)

// The page settles a write once api/state's revision reaches the one its answer named, so every change a
// command makes to a published view must come with a higher revision. Each case runs one command the way
// the page sends it, no-ops included, and every command the package has must have a case.
func TestNoCommandChangesAPublishedViewAtTheSameRevision(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.c.clientsKnown = true
	off := false
	slot := func(id domain.SlotID) slotRow { return r.c.state.slots[id] }
	remote := func(agent string) SourceInput {
		return SourceInput{Name: "Acme", Remote: &RemoteInput{URL: domain.NewSecret("https://example.com/f"), UserAgent: agent, RefreshEvery: 6 * time.Hour}}
	}
	group := func(id domain.GroupID) domain.IntentRevision {
		return domain.IntentRevision(r.c.world.groups[id].Revision)
	}
	source := func(id domain.SourceID) domain.IntentRevision {
		return domain.IntentRevision(r.c.state.sources[id].Revision)
	}
	pass := func(id string) domain.Signal {
		ping := 30 * time.Millisecond
		r.clock = r.clock.Add(time.Second)
		return domain.Signal{Stamp: domain.Stamp{Epoch: r.c.evidence.epoch, At: r.clock}, Server: r.ref(id), Kind: domain.ActiveProbe,
			Verdict: domain.Succeeded, Purpose: domain.ManualProbe, Target: "t1", Ping: &ping, NewConn: &ping}
	}
	settings := func(days int) PutSettings {
		if days == 0 {
			days = r.c.state.settings.AnalyticsRetentionDays
		}
		return PutSettings{Expected: r.c.state.settingsRevision, RetentionDays: days}
	}

	cases := []struct {
		name    string
		prepare func()
		command func() Command
	}{
		{name: "a settings save", command: func() Command { return settings(60) }},
		{name: "a settings save of what is stored", command: func() Command { return settings(0) }},
		{name: "the update check turned off", command: func() Command { return PutCheckForUpdates{On: false} }},
		{name: "the update check left as stored", command: func() Command { return PutCheckForUpdates{On: r.c.state.settings.CheckForUpdates} }},
		{name: "a new slot", command: func() Command { return PutSlot{ID: "lab", Intent: elected(t, "good")} }},
		{name: "a slot renamed", command: func() Command {
			return PutSlot{ID: "lab", Expected: slot("lab").Slot.Version, Intent: exitIntent(t, "Lab", domain.GroupExit(sourceGroup("good")))}
		}},
		{name: "a slot saved as stored", command: func() Command {
			return PutSlot{ID: "lab", Expected: slot("lab").Slot.Version, Intent: slot("lab").Slot.Intent}
		}},
		{name: "a tick", command: func() Command { return TickServer{Group: sourceGroup("good"), Server: "b", Ticked: true} }},
		{name: "a tick of a ticked server", command: func() Command { return TickServer{Group: sourceGroup("good"), Server: "b", Ticked: true} }},
		{name: "a press", prepare: func() { r.frame(domain.WANUp, r.check("a", domain.Succeeded, 10)) }, command: func() Command { return PreferServer{Owner: domain.GroupOwner(sourceGroup("good")), Signal: pass("b")} }},
		{name: "a press on the preferred server", command: func() Command {
			return PreferServer{Owner: domain.GroupOwner(sourceGroup("good")), Signal: pass("b")}
		}},
		{name: "a stale clear", command: func() Command {
			return ClearPreference{Owner: domain.GroupOwner(sourceGroup("good")), Server: "a", EndsAt: r.clock}
		}},
		{name: "a clear", command: func() Command {
			p := storedBoost(r.memory("eu").Hand)
			return ClearPreference{Owner: domain.GroupOwner(sourceGroup("good")), Server: p.Server, EndsAt: p.endsAt()}
		}},
		{name: "a new group", command: func() Command {
			return PutGroup{ID: "de", Name: "Germany", Members: ruleMembers(t, rule{Countries: []string{"DE"}})}
		}},
		{name: "a group renamed", command: func() Command {
			return PutGroup{ID: "de", Expected: group("de"), Name: "DE", Members: ruleMembers(t, rule{Countries: []string{"DE"}})}
		}},
		{name: "a group deleted", command: func() Command { return DeleteGroup{ID: "de", Expected: group("de")} }},
		{name: "a new source", command: func() Command { return PutSource{ID: "acme", Input: remote("")} }},
		{name: "a refresh with the stored agent", command: func() Command { return RefreshSource{ID: "acme"} }},
		{name: "a refresh with another agent", command: func() Command { return RefreshSource{ID: "acme", UserAgent: "Hiddify/2.5.7"} }},
		{name: "a source renamed", command: func() Command {
			in := remote("Hiddify/2.5.7")
			in.Name = "Acme EU"
			return PutSource{ID: "acme", Expected: source("acme"), Input: in}
		}},
		{name: "a source deleted", command: func() Command { return DeleteSource{ID: "acme", Expected: source("acme")} }},
		{name: "a client turned back on", prepare: func() {
			r.c.owned["eu"] = unifi.Owned{NetworkID: slot("eu").Slot.Allocation.NetworkID, Slot: "eu", Enabled: &off}
		}, command: func() Command { return EnableClient{ID: "eu"} }},
		{name: "a client recreated", prepare: func() {
			row := slot("eu")
			row.Slot.Allocation.NetworkID = "n-gone"
			r.c.state.slots["eu"] = row
			delete(r.c.owned, "eu")
		}, command: func() Command { return RecreateClient{ID: "eu"} }},
		{name: "a slot deleted", command: func() Command { return DeleteSlot{ID: "lab", Expected: slot("lab").Slot.Version} }},
		{name: "a key UniFi accepted", command: func() Command {
			return PutUniFiKey{Key: domain.NewSecret("the-key"), Syncer: &recordingSyncer{routes: map[domain.SlotID][]unifi.Route{}}}
		}},
	}

	ran := map[string]bool{}
	for _, tc := range cases {
		if tc.prepare != nil {
			tc.prepare()
		}
		r.c.publish(r.clock)
		before, beforeSettings := r.app.Snapshot(), r.app.Settings()
		command := tc.command()
		receipt, err := r.takePage(pageWrite{Key: domain.RequestID(tc.name), Fingerprint: []byte(tc.name), Decision: runs{command}})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if receipt.Answer == nil || receipt.Answer.Status >= 300 {
			t.Fatalf("%s was refused: %s", tc.name, receipt.Answer.Body)
		}
		after, afterSettings := r.app.Snapshot(), r.app.Settings()
		ran[fmt.Sprintf("%T", command)] = true
		if receipt.Revision != after.Revision {
			t.Errorf("%s: answered at revision %d, published at %d", tc.name, receipt.Revision, after.Revision)
		}
		if after.Revision == before.Revision {
			if changed := viewChanges(t, before, after); len(changed) > 0 {
				t.Errorf("%s changed the view at revision %d: %s", tc.name, after.Revision, strings.Join(changed, ", "))
			}
		}
		if afterSettings.Revision == beforeSettings.Revision && !reflect.DeepEqual(afterSettings, beforeSettings) {
			t.Errorf("%s changed the settings at revision %d", tc.name, afterSettings.Revision)
		}
	}
	for _, command := range commandTypes(t) {
		if !ran["daemon."+command] {
			t.Errorf("no case runs %s", command)
		}
	}
}

// viewChanges names the paths at which two views differ, apart from when each was made.
func viewChanges(t *testing.T, before, after View) []string {
	t.Helper()
	before.GeneratedAt, after.GeneratedAt = time.Time{}, time.Time{}
	tree := func(v View) any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	var paths []string
	var walk func(path string, a, b any)
	walk = func(path string, a, b any) {
		switch a := a.(type) {
		case map[string]any:
			if b, ok := b.(map[string]any); ok {
				keys := slices.Sorted(func(yield func(string) bool) {
					for k := range a {
						if !yield(k) {
							return
						}
					}
					for k := range b {
						if _, both := a[k]; !both && !yield(k) {
							return
						}
					}
				})
				for _, k := range keys {
					walk(path+"."+k, a[k], b[k])
				}
				return
			}
		case []any:
			if b, ok := b.([]any); ok && len(a) == len(b) {
				for i := range a {
					walk(fmt.Sprintf("%s[%d]", path, i), a[i], b[i])
				}
				return
			}
		}
		if !reflect.DeepEqual(a, b) {
			paths = append(paths, fmt.Sprintf("%s %v → %v", strings.TrimPrefix(path, "."), a, b))
		}
	}
	walk("", tree(before), tree(after))
	return paths
}

// commandTypes reads the package's own source for every type with a perform method, so a command
// added later fails the test until it has a case.
func commandTypes(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "perform" || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if id, ok := fn.Recv.List[0].Type.(*ast.Ident); ok {
				types = append(types, id.Name)
			}
		}
	}
	if len(types) == 0 {
		t.Fatal("found no command in the package")
	}
	return slices.Sorted(slices.Values(types))
}
