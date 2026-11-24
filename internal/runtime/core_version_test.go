package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCore writes a script that says it is Xray at version, and logs each time it is asked.
func fakeXray(t *testing.T, path, version, log string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$1\" >>'" + log + "'\necho 'Xray " + version + " (Xray, Penetrates Everything.) 1a2b3c4 (go1.25.1 linux/arm64)'\necho 'A unified platform for anti-censorship.'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func asked(t *testing.T, log string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "version\n")
}

// settledUnit is what unit says once the version reads it starts have landed, and a read it starts again
// after one failed has landed too.
func settledUnit(t *testing.T, x *xray) (coreObservation, error) {
	t.Helper()
	_, _ = x.unit(context.Background())
	x.reads.Wait()
	obs, err := x.unit(context.Background())
	x.reads.Wait()
	return obs, err
}

// The running core's version comes from its process's own binary, so an older core that an
// upgrade left running names itself, and each binary is asked once while nothing changes.
func TestTheRunningAndThePinnedCoreEachNameTheirVersion(t *testing.T) {
	dir := t.TempDir()
	systemctl := filepath.Join(dir, "bin", "systemctl")
	if err := os.MkdirAll(filepath.Dir(systemctl), 0o755); err != nil {
		t.Fatal(err)
	}
	epoch := func(id string) {
		if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nprintf 'ActiveState=active\\nMainPID=4242\\nInvocationID="+id+"\\n'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	epoch("e1")
	t.Setenv("PATH", filepath.Dir(systemctl)+string(os.PathListSeparator)+os.Getenv("PATH"))
	data, pinnedLog, runningLog := filepath.Join(dir, "data"), filepath.Join(dir, "pinned.log"), filepath.Join(dir, "running.log")
	fakeXray(t, filepath.Join(data, "current", "xray"), "26.10.31", pinnedLog)
	old := filepath.Join(dir, "cores", "old", "xray")
	fakeXray(t, old, "25.12.8", runningLog)
	proc := filepath.Join(dir, "proc")
	if err := os.MkdirAll(filepath.Join(proc, "4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, filepath.Join(proc, "4242", "exe")); err != nil {
		t.Fatal(err)
	}
	x := newXray(Options{DataDir: data, RunDir: filepath.Join(dir, "run")})
	x.proc, x.api = proc, newCoreAPI(closedAddr(t))

	for range 2 {
		obs, _ := settledUnit(t, x)
		if obs.Versions != (CoreVersions{Release: "26.10.31", Running: "25.12.8"}) || !obs.Outdated {
			t.Fatalf("an older core left running reads as %+v, outdated %v", obs.Versions, obs.Outdated)
		}
	}
	if p, r := asked(t, pinnedLog), asked(t, runningLog); p != 1 || r != 1 {
		t.Errorf("two reads asked the pinned core %d times and the running one %d times, want once each", p, r)
	}

	if err := os.Remove(filepath.Join(proc, "4242", "exe")); err != nil {
		t.Fatal(err)
	}
	pinned, err := filepath.EvalSymlinks(filepath.Join(data, "current", "xray"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pinned, filepath.Join(proc, "4242", "exe")); err != nil {
		t.Fatal(err)
	}
	epoch("e2")
	obs, _ := settledUnit(t, x)
	if obs.Versions != (CoreVersions{Release: "26.10.31", Running: "26.10.31"}) || obs.Outdated {
		t.Errorf("the restarted core reads as %+v, outdated %v", obs.Versions, obs.Outdated)
	}
}

// A core that isn't running has no version, and a binary that doesn't say it is Xray has none either.
func TestNoRunningCoreNamesNoVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf 'ActiveState=inactive\\nMainPID=0\\nInvocationID=\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(data, "current"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "current", "xray"), []byte("#!/bin/sh\necho 'something else 1.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	x := newXray(Options{DataDir: data, RunDir: filepath.Join(dir, "run")})
	obs, err := settledUnit(t, x)
	if err != nil || obs.Versions != (CoreVersions{}) {
		t.Errorf("a stopped core beside a binary that isn't Xray reads as %+v, %v", obs.Versions, err)
	}
}

// A read of the running core's version that fails, since its process's binary is gone for a moment or
// it said something that isn't its version, is asked again on the next read of the same core.
func TestAFailedReadOfTheRunningVersionIsAskedAgain(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf 'ActiveState=active\\nMainPID=4242\\nInvocationID=e1\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	data := filepath.Join(dir, "data")
	fakeXray(t, filepath.Join(data, "current", "xray"), "26.10.31", filepath.Join(dir, "pinned.log"))
	running := filepath.Join(dir, "cores", "old", "xray")
	exe := filepath.Join(dir, "proc", "4242", "exe")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	x := newXray(Options{DataDir: data, RunDir: filepath.Join(dir, "run")})
	x.proc, x.api = filepath.Join(dir, "proc"), newCoreAPI(closedAddr(t))
	read := func() string {
		obs, _ := settledUnit(t, x)
		return obs.Versions.Running
	}
	if got := read(); got != "" {
		t.Fatalf("a core whose binary is gone reads as %q", got)
	}
	if err := os.MkdirAll(filepath.Dir(running), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(running, []byte("#!/bin/sh\necho 'starting up'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(running, exe); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "" {
		t.Fatalf("a core that says something else reads as %q", got)
	}
	fakeXray(t, running, "25.12.8", filepath.Join(dir, "running.log"))
	if got := read(); got != "25.12.8" {
		t.Errorf("the same core, once it answers, reads as %q", got)
	}
}

// A pass never waits on `xray version`: a binary that doesn't answer it holds no report, and the
// version reaches a report once the binary says it, with no poll to bring it.
func TestAVersionReadHoldsNoPass(t *testing.T) {
	slowTest(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf 'ActiveState=inactive\\nMainPID=0\\nInvocationID=\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	data, answer := filepath.Join(dir, "data"), filepath.Join(dir, "answer")
	if err := os.MkdirAll(filepath.Join(data, "current"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nwhile [ ! -e '" + answer + "' ]; do sleep 0.05; done\necho 'Xray 26.10.31 (Xray, Penetrates Everything.) 1a2b3c4 (go1.25.1 linux/arm64)'\n"
	if err := os.WriteFile(filepath.Join(data, "current", "xray"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeLinks(t, map[string]int{})
	w, err := Open(Options{DataDir: data, RunDir: filepath.Join(dir, "run"), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	goals, reports, done := make(chan Plan, 1), make(chan Report, 64), make(chan error, 1)
	go func() { done <- w.Run(ctx, goals, reports, nil) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
	plan, err := NewPlan(1, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	goals <- plan
	next := func(what string) Report {
		t.Helper()
		select {
		case r := <-reports:
			return r
		case <-time.After(3 * time.Second):
			t.Fatalf("no %s within 3 s", what)
			return Report{}
		}
	}
	if r := next("pass while the binary doesn't answer its version"); r.Core.Release != "" {
		t.Fatalf("a binary that hasn't answered reads as %q", r.Core.Release)
	}
	if err := os.WriteFile(answer, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for r := next("pass naming the version"); r.Core.Release != "26.10.31"; r = next("pass naming the version") {
	}
}

// A running core whose binary doesn't answer `xray version`, such as a replacement systemd just
// started, holds no plan: each plan is reported at once while the read is out, and the version
// reaches a report once the binary says it.
func TestARunningCoresVersionReadHoldsNoPlan(t *testing.T) {
	slowTest(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf 'ActiveState=active\\nMainPID=4242\\nInvocationID=e2\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	data, answer, running := filepath.Join(dir, "data"), filepath.Join(dir, "answer"), filepath.Join(dir, "cores", "new", "xray")
	fakeXray(t, filepath.Join(data, "current", "xray"), "26.10.31", filepath.Join(dir, "pinned.log"))
	if err := os.MkdirAll(filepath.Dir(running), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nwhile [ ! -e '" + answer + "' ]; do sleep 0.05; done\necho 'Xray 26.11.30 (Xray, Penetrates Everything.) 1a2b3c4 (go1.25.1 linux/arm64)'\n"
	if err := os.WriteFile(running, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(dir, "proc")
	if err := os.MkdirAll(filepath.Join(proc, "4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(running, filepath.Join(proc, "4242", "exe")); err != nil {
		t.Fatal(err)
	}
	fakeLinks(t, map[string]int{})
	w, err := Open(Options{DataDir: data, RunDir: filepath.Join(dir, "run"), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	w.core.proc, w.core.api = proc, fakeCore(t, func(string, []byte) ([]byte, string) { return nil, "0" })
	ctx, cancel := context.WithCancel(context.Background())
	goals, reports, done := make(chan Plan, 1), make(chan Report, 64), make(chan error, 1)
	go func() { done <- w.Run(ctx, goals, reports, nil) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
	reported := func(generation uint64) Report {
		t.Helper()
		plan, err := NewPlan(generation, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		goals <- plan
		for limit := time.After(time.Second); ; {
			select {
			case r := <-reports:
				if r.Generation == generation {
					return r
				}
			case <-limit:
				t.Fatalf("plan %d isn't reported within 1 s while the running core's version read is out", generation)
			}
		}
	}
	for generation := range uint64(3) {
		if r := reported(generation + 1); r.Core.Running != "" {
			t.Fatalf("a running core that hasn't answered reads as %q", r.Core.Running)
		}
	}
	if err := os.WriteFile(answer, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for limit := time.After(3 * time.Second); ; {
		select {
		case r := <-reports:
			if r.Core.Running == "26.11.30" {
				return
			}
		case <-limit:
			t.Fatal("the running core's version reached no report within 3 s of its answer")
		}
	}
}
