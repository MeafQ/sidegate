package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadTimings(t *testing.T, timingsJSON string) (timings, error) {
	t.Helper()
	dir := t.TempDir()
	body := `{"timings": ` + timingsJSON + `}`
	if err := os.WriteFile(filepath.Join(dir, "bootstrap.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := LoadBootstrap(dir)
	return b.Timings.apply(), err
}

func TestBootstrapOverridesSingleTimings(t *testing.T) {
	got, err := loadTimings(t, `{"handover": "60s", "settleFor": "1500ms"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultTimings()
	want.Handover, want.SettleFor = time.Minute, 1500*time.Millisecond
	if got != want {
		t.Fatalf("timings %+v, want %+v", got, want)
	}
}

func TestBootstrapRefusesBadTimings(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":  `{"gainFraction": "1s"}`,
		"zero":           `{"handover": "0s"}`,
		"negative":       `{"handover": "-1s"}`,
		"not a duration": `{"handover": "soon"}`,
		"not a string":   `{"handover": 60}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadTimings(t, body); err == nil || !strings.Contains(err.Error(), "bootstrap.json") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestNoBootstrapTimingsMeansProduction(t *testing.T) {
	b, err := LoadBootstrap(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Timings.apply(); got != defaultTimings() {
		t.Fatalf("timings %+v", got)
	}
}
