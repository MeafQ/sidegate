//go:build lab

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A lab build lets a step hold a link's up inside its write, after the opening's confirmation and
// with the gate lock held, and times every up it writes. SIDEGATE_LAB_GATES names the step's
// directory. A hold file there names the link whose next up waits: the write touches held, waits for
// release, writes, and records in written what a read of the link then shows, still under the gate
// lock: up, down, or why the write or the read failed. With kill there too, the process then kills
// itself, as a daemon that dies right after its link up. Every up's microseconds go to ups.
func init() {
	if dir := os.Getenv("SIDEGATE_LAB_GATES"); dir != "" {
		writeLink = labHold(dir, writeLink)
	}
}

// labHold wraps next, the flag write, with the step's hold in dir.
func labHold(dir string, next func(link string, up bool) error) func(link string, up bool) error {
	at := func(name string) string { return filepath.Join(dir, name) }
	exists := func(name string) bool { _, err := os.Stat(at(name)); return err == nil }
	return func(link string, up bool) error {
		if !up {
			return next(link, up)
		}
		hold, _ := os.ReadFile(at("hold"))
		held := strings.TrimSpace(string(hold)) == link
		if held {
			_ = os.Remove(at("hold"))
			_ = os.WriteFile(at("held"), nil, 0o644)
			for !exists("release") {
				time.Sleep(10 * time.Millisecond)
			}
		}
		start := time.Now()
		err := next(link, up)
		took := time.Since(start).Microseconds()
		result := "ok"
		if err != nil {
			result = err.Error()
		}
		if f, ferr := os.OpenFile(at("ups"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ferr == nil {
			fmt.Fprintf(f, "%s %d %s\n", link, took, result)
			f.Close()
		}
		if held {
			if err == nil {
				switch up, rerr := linkIsUp(link); {
				case rerr != nil:
					result = "unread: " + rerr.Error()
				case up:
					result = "up"
				default:
					result = "down"
				}
			}
			_ = os.WriteFile(at("written"), []byte(result+"\n"), 0o644)
			if exists("kill") {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
		}
		return err
	}
}
