//go:build lab

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The lab's held up records what a read of the link shows once the write returns, so step coredeath
// counts the up as landed only when the link reads up between the stop hook's two sweeps, whatever the
// write said.
func TestTheLabHoldRecordsWhatTheLinkReadsAfterItsUp(t *testing.T) {
	flags := func(up bool) {
		t.Helper()
		body := map[bool]string{true: "0x1003\n", false: "0x1002\n"}[up]
		if err := os.WriteFile(filepath.Join(sysNet, "pxa1", "flags"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		next func(string, bool) error
		want string
	}{
		{"an up that lands", func(_ string, up bool) error { flags(up); return nil }, "up"},
		{"an up acknowledged and never applied", func(string, bool) error { return nil }, "down"},
		{"an up refused", func(string, bool) error { return syscall.EADDRINUSE }, syscall.EADDRINUSE.Error()},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeLinks(t, map[string]int{"pxa1": linkGroup})
			flags(false)
			dir := t.TempDir()
			for name, body := range map[string]string{"hold": "pxa1\n", "release": ""} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_ = labHold(dir, c.next)("pxa1", true)
			written, err := os.ReadFile(filepath.Join(dir, "written"))
			if got := strings.TrimSpace(string(written)); err != nil || got != c.want {
				t.Errorf("written %q, want %q (%v)", got, c.want, err)
			}
		})
	}
}
