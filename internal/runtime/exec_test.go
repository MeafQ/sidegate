package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"testing"
	"time"
)

// TestMain runs as the fake core's `xray api` when a test asks it to. It gives a Mac a timeout that only runs the tool. A Mac has no coreutils, and the kill timer
// itself is proved in the lab (lab/verify-runtime.sh, step hung). It runs a test's script through
// sh, because macOS checks each new executable on its first run, one at a time across the system:
// about 0.3 s alone and seconds beside a parallel go test, longer than the deadlines tests here time.
func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeXrayEnv); dir != "" {
		os.Exit(fakeXrayAPI(dir, os.Args[1:], os.Stdin))
	}
	linkDump = sysLinks
	if goruntime.GOOS == "darwin" {
		dir, err := os.MkdirTemp("", "timeout")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(dir)
		timeout := filepath.Join(dir, "timeout")
		if err := os.WriteFile(timeout, []byte(fakeTimeout), 0o755); err != nil {
			panic(err)
		}
		if err := exec.Command(timeout, "-s", "KILL", "1", "true").Run(); err != nil {
			panic(err)
		}
		os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	m.Run()
}

// sysLinks is the final sample as a test's sysNet holds its links: each one's flags and its group.
func sysLinks() (map[string]linkSample, error) {
	entries, err := os.ReadDir(sysNet)
	if err != nil {
		return nil, err
	}
	links := map[string]linkSample{}
	for _, e := range entries {
		up, err := linkIsUp(e.Name())
		if err != nil {
			return nil, err
		}
		links[e.Name()] = linkSample{up: up, ours: linkOwned(e.Name())}
	}
	return links, nil
}

const fakeTimeout = `#!/bin/sh
shift 3
if tool=$(command -v "$1") && IFS= read -r first <"$tool" && [ "${first#'#!'}" != "$first" ]; then
	shift
	exec /bin/sh "$tool" "$@"
fi
exec "$@"
`

func TestEveryToolHoldsTheRuntimeLockUntilItIsGone(t *testing.T) {
	slowTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.lock")
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nsleep 1 >/dev/null 2>&1 &\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := lockFile(path, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (tools{lock}).run(context.Background(), nil, tool); err != nil {
		t.Fatal(err)
	}
	lock.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	stalls := 0
	if got, err := awaitLock(ctx, path, 100*time.Millisecond, func() { stalls++ }); got != nil || err != nil {
		got.Close()
		t.Fatal("a successor took the runtime lock while a process the tool left behind still ran")
	}
	if stalls == 0 {
		t.Error("a stalled wait for the runtime lock was never reported")
	}
	got, err := awaitLock(context.Background(), path, time.Hour, func() {})
	if err != nil || got == nil {
		t.Fatalf("the successor never got the lock: %v", err)
	}
	got.Close()
}

func TestCancellingAToolKillsItsWholeGroup(t *testing.T) {
	slowTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.lock")
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\ntrap '' HUP INT TERM\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := lockFile(path, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := (tools{lock}).run(ctx, nil, tool); err == nil {
		t.Fatal("a killed tool reported success")
	}
	lock.Close()
	if got, err := lockWithin(context.Background(), path, 2*time.Second); err != nil {
		t.Fatalf("a child of the cancelled tool still holds the lock: %v", err)
	} else {
		got.Close()
	}
}

func TestLockWithinGivesUpOnAHolderPastItsDeadline(t *testing.T) {
	slowTest(t)
	path := filepath.Join(t.TempDir(), "gates.lock")
	lock, err := lockFile(path, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if got, err := lockWithin(context.Background(), path, 100*time.Millisecond); err == nil {
		got.Close()
		t.Fatal("took a lock someone holds")
	}
}
