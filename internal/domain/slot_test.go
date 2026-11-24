package domain

import (
	"errors"
	"testing"
)

func TestSlotAdmission(t *testing.T) {
	cases := []struct {
		name string
		slot string
		exit Exit
		ok   bool
	}{
		{"a group", "EU", GroupExit("g"), true},
		{"a server", "EU", ServerExit("a"), true},
		{"a source", "EU", SourceExit("s"), true},
		{"Direct", "EU", DirectExit(), true},
		{"Block", "EU", BlockExit(), true},
		{"no name", " ", GroupExit("g"), false},
		{"no Exit", "EU", Exit{}, false},
		{"an empty server is no Exit", "EU", ServerExit(""), false},
		{"both", "EU", Exit{server: "a", group: "g"}, false},
		{"a group sent to the WAN", "EU", Exit{group: "g", off: toWAN}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			intent, err := NewIntent(c.slot, c.exit)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok = %v", err, c.ok)
			}
			if err == nil && (intent.Name() != c.slot || intent.Exit() != c.exit) {
				t.Fatalf("intent = %+v", intent)
			}
		})
	}
	if _, ok := GroupExit("g").Server(); ok {
		t.Fatal("a group Exit named a server")
	}
	if _, ok := ServerExit("a").Group(); ok {
		t.Fatal("a server Exit named a group")
	}
	for key, want := range map[string]Owner{"group/g": GroupOwner("g"), "source/s": SourceOwner("s"), "slot/x": SlotOwner("x")} {
		if got, err := ParseOwner(key); err != nil || got != want || got.String() != key {
			t.Errorf("%s came back %+v, %v", key, got, err)
		}
	}
}

func TestAllocateTakesTheLowestFreeOrdinal(t *testing.T) {
	var taken []Allocation
	for want := uint8(1); want <= MaxSlots; want++ {
		a, err := Allocate(taken, SlotKeys{})
		if err != nil {
			t.Fatal(err)
		}
		if a.Ordinal != want || a.PeerListenPort != 51830+uint16(want) || a.TPROXYPort != 12340+uint16(want) {
			t.Fatalf("allocation %d = %+v", want, a)
		}
		taken = append(taken, a)
	}
	if _, err := Allocate(taken, SlotKeys{}); !errors.Is(err, ErrNoAllocation) {
		t.Fatalf("ninth allocation: %v", err)
	}
	a, err := Allocate(taken[1:], SlotKeys{})
	if err != nil || a.Ordinal != 1 || a.Tunnel.String() != "10.70.1.0/30" {
		t.Fatalf("freed ordinal: %+v, %v", a, err)
	}
}
