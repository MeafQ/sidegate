package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"sidegate/internal/domain"
)

// The switches list orders events of one millisecond by the number that ends their ID, not as text,
// and reads IDs that end in no number.
func TestTheSwitchesListOrdersAMillisecondBySequence(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir())
	body, err := encodeSwitch(domain.Switch{Slots: []domain.SlotID{"eu"}, From: domain.Direct(), To: domain.Blocked()})
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int{9, 10, 99, 100, 1, 2, 11, 999, 1000} {
		owner := fmt.Sprintf("slot/name-%d", 1001-seq)
		mustExec(t, r.c.store.writer, `INSERT INTO switch_events VALUES(?,?,1,?,?,?,?)`, fmt.Sprintf("%s-%d-%d", owner, t0.UnixMilli(), seq), owner, int(domain.ExitChanged), t0.UnixMilli(), t0.UnixMilli(), body)
	}
	for _, limit := range []int{1, 2, 5, 9} {
		events, err := r.app.Queries.state.switches(ctx, limit)
		if err != nil {
			t.Fatal(err)
		}
		var seqs []uint64
		for _, e := range events {
			seqs = append(seqs, eventSequence(e.ID))
		}
		if want := []uint64{1000, 999, 100, 99, 11, 10, 9, 2, 1}[:limit]; !slices.Equal(seqs, want) {
			t.Errorf("limit %d lists %v, want %v", limit, seqs, want)
		}
	}
	for _, id := range []string{"no-suffix", "pause", "numeric-tail-", "name42"} {
		mustExec(t, r.c.store.writer, `INSERT INTO switch_events VALUES(?,'slot/eu',1,?,?,?,?)`, id, int(domain.ExitChanged), t0.Add(-time.Second).UnixMilli(), t0.UnixMilli(), body)
	}
	if all, err := r.app.Queries.state.switches(ctx, 20); err != nil || len(all) != 13 {
		t.Fatalf("listed %d events, err %v", len(all), err)
	}
}

// After a restart with the clock set back, moves of one millisecond list in the order they were made,
// so the list's limit keeps a slot's latest move and drops an older one of another slot.
func TestTheSwitchesListKeepsTheLatestMovesAcrossARestartAndAClockSetBack(t *testing.T) {
	r := newRig(t, t.TempDir())
	electA(t, r)
	r.putSlot("west", exitIntent(t, "West", domain.BlockExit()))
	r.settle()
	at := r.clock.Add(time.Minute)
	r.clock = at
	r.setExit("eu", domain.DirectExit())
	r.settle()
	r.clock = at.Add(10 * time.Minute)
	r = r.reopened()
	r.settle()
	r.clock = at
	r.setExit("west", domain.DirectExit())
	r.settle()
	r.setExit("eu", domain.BlockExit())
	r.settle()
	for i := range 198 {
		r.clock = at.Add(time.Duration(i+1) * time.Second)
		r.setExit("eu", []domain.Exit{domain.DirectExit(), domain.BlockExit()}[i%2])
		r.settle()
	}
	events := r.switches()
	if len(events) != maxSwitchEvents {
		t.Fatalf("listed %d events, want %d", len(events), maxSwitchEvents)
	}
	var last []string
	for _, e := range events[maxSwitchEvents-2:] {
		last = append(last, fmt.Sprintf("%s to %s", e.Slots[0], e.To.Kind))
	}
	if want := []string{"eu to block", "west to direct"}; !slices.Equal(last, want) {
		t.Errorf("the list ends %v, want %v", last, want)
	}
}

// A server named nowhere can still have a home, and a stored lineage keeps it. A name with remarks
// always has one.
func TestAStoredHomeNeedsNoName(t *testing.T) {
	l := lineage(t, "a", "s", "DE", "a.example")
	l.Label, l.Naming = "Server 2", domain.Naming{Home: "k", Number: 2}
	body, err := encodeLineage(l)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeLineage("a", "s", body); err != nil || got.Naming != l.Naming {
		t.Errorf("read back %+v (%v), want %+v", got.Naming, err, l.Naming)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	fields["naming"] = json.RawMessage(`{"remarks":"Sweden"}`)
	body, _ = json.Marshal(fields)
	if _, err := decodeLineage("a", "s", body); err == nil {
		t.Error("a name with remarks and no home read back")
	}
}

// An allocation an older build wrote carries the binding's phase, which nothing reads. It loads,
// and the row's next write drops it.
func TestAnAllocationWithItsOldPhaseLoads(t *testing.T) {
	stored := `{"ordinal":1,"tunnel":"10.70.1.0/30","peerPort":51831,"tproxyPort":12341,"keys":{"HostPrivate":"h","ClientPrivate":"c","HostPublic":"H","ClientPublic":"C"},"networkId":"n1","phase":2}`
	a, err := decodeAllocation([]byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if a.Ordinal != 1 || a.Tunnel.String() != "10.70.1.0/30" || a.NetworkID != "n1" || a.Keys.ClientPublic != "C" {
		t.Fatalf("read back %+v", a)
	}
	written, err := encodeAllocation(a)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(written, &fields); err != nil {
		t.Fatal(err)
	}
	if _, kept := fields["phase"]; kept {
		t.Errorf("the next write kept the phase: %s", written)
	}
}
