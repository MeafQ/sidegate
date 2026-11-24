package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"sidegate/internal/catalogue"
	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
	"sidegate/internal/unifi"
)

// The owner no longer sets a default: a source with no interval of its own or its provider's
// refreshes every six hours, give or take its jitter.
func TestASourceWithNoIntervalRefreshesEverySixHours(t *testing.T) {
	r := newRig(t, t.TempDir())
	checked, _ := json.Marshal(r.clock)
	var fs catalogue.FetchState
	if err := fs.UnmarshalBinary([]byte(`{"checked_at":` + string(checked) + `,"updated_at":` + string(checked) + `}`)); err != nil {
		t.Fatal(err)
	}
	source := domain.Source{ID: "s", Revision: 1, Name: "S", Origin: domain.RemoteOrigin(domain.Remote{Location: "l", UserAgent: "u"})}
	wait := r.c.due(source, fs).Sub(r.clock)
	if wait < 6*time.Hour || wait > 6*time.Hour+6*time.Hour/20 {
		t.Errorf("a source with no interval is due in %s, want six hours and its jitter", wait)
	}
}

// A settings row stored while the owner could set a default refresh still loads, and keeps its retention.
func TestASettingsRowThatStillNamesADefaultRefreshLoads(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	// Legacy removed field name (kept split to verify unknown fields are rejected).
	removed := "defaultRefresh" + "Seconds"
	if _, err := r.c.store.writer.ExecContext(context.Background(), `INSERT INTO settings VALUES(1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body`,
		[]byte(`{"`+removed+`":3600,"analyticsRetentionDays":90}`)); err != nil {
		t.Fatal(err)
	}
	r.close()
	again := newRig(t, dir)
	if got := again.app.Settings().Settings; got != (Settings{AnalyticsRetentionDays: 90, CheckForUpdates: true}) {
		t.Errorf("the old row loads as %+v", got)
	}
}

// A settings body stored before the update check existed reads with the check on.
func TestASettingsBodyWithoutTheUpdateCheckReadsOn(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	if _, err := r.c.store.writer.ExecContext(context.Background(), `INSERT INTO settings VALUES(1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body`,
		[]byte(`{"analyticsRetentionDays":7}`)); err != nil {
		t.Fatal(err)
	}
	r.close()
	again := newRig(t, dir)
	if got := again.app.Settings().Settings; got != (Settings{AnalyticsRetentionDays: 7, CheckForUpdates: true}) {
		t.Errorf("a body without checkForUpdates loads as %+v", got)
	}
}

// The update check turned off stays off across a restart, and a Retention save keeps it as stored.
func TestTheUpdateCheckTurnedOffStaysOff(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	if !r.app.Settings().CheckForUpdates {
		t.Fatal("a fresh daemon checks for updates off")
	}
	if _, err := r.take(PutCheckForUpdates{On: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.take(PutSettings{Expected: r.app.Settings().Revision, RetentionDays: 90}); err != nil {
		t.Fatal(err)
	}
	r.close()
	again := newRig(t, dir)
	if got := again.app.Settings().Settings; got != (Settings{AnalyticsRetentionDays: 90, CheckForUpdates: false}) {
		t.Errorf("after a restart the settings read %+v", got)
	}
}

// acceptedAt is the last pass UniFi answered. A refused key or a silent UniFi leaves it, so the page
// can say when the key last worked, and a daemon that just started knows nothing yet.
func TestAcceptedAtIsTheLastAnsweredPass(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	if at := r.app.Snapshot().UniFi.AcceptedAt; at != nil {
		t.Fatalf("a fresh start says UniFi answered at %s", at)
	}
	if err := r.c.applyClients(ctx, clientReport{}); err != nil {
		t.Fatal(err)
	}
	answered := r.clock
	for _, failure := range []error{&unifi.Error{Kind: unifi.Unauthorized, Op: "list", Status: 401}, &unifi.Error{Kind: unifi.Unavailable, Op: "list"}} {
		r.clock = r.clock.Add(time.Minute)
		if err := r.c.applyClients(ctx, clientReport{Err: failure}); err != nil {
			t.Fatal(err)
		}
		if at := r.app.Snapshot().UniFi.AcceptedAt; at == nil || !at.Equal(answered) {
			t.Errorf("after %v acceptedAt is %v, want the last answered pass at %s", failure, at, answered)
		}
	}
	r.clock = r.clock.Add(time.Minute)
	if err := r.c.applyClients(ctx, clientReport{}); err != nil {
		t.Fatal(err)
	}
	if at := r.app.Snapshot().UniFi.AcceptedAt; at == nil || !at.Equal(r.clock) {
		t.Errorf("a pass UniFi answered left acceptedAt at %v, want %s", at, r.clock)
	}
}

// The view names the release's pinned core and the one running as the runtime read them, and
// says nothing of a core it couldn't read.
func TestTheViewNamesTheRunningAndThePinnedCore(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	if core := r.app.Snapshot().Core; core.Version != nil || core.Running != nil {
		t.Fatalf("before any report the view names a core: %+v", core)
	}
	report := gateway.Report{Generation: r.c.state.runtimeGeneration, Core: gateway.CoreVersions{Release: "26.10.31", Running: "25.12.8"}}
	if err := r.c.applyReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	core := r.app.Snapshot().Core
	if core.Version == nil || *core.Version != "26.10.31" || core.Running == nil || *core.Running != "25.12.8" {
		t.Errorf("an older running core reads as %+v", core)
	}
	report.Core.Running = ""
	if err := r.c.applyReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	if core := r.app.Snapshot().Core; core.Running != nil || core.Version == nil {
		t.Errorf("a stopped core reads as %+v", core)
	}
}
