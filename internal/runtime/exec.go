package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// commandTimeout bounds every external tool call, so a hung ip, wg, iptables,
// systemctl, or xray process cannot hold the actor.
const commandTimeout = 10 * time.Second

// tools runs gateway tools. Each one holds lock open, when it is set, so a
// runtime killed while a tool runs leaves the lock held until that tool exits,
// and the next runtime, which waits for the lock, observes nothing the tool can
// still change. Each runs under its own coreutils `timeout -s KILL`, in its own
// process group, so it ends on commandTimeout, however wedged, whether or not the
// runtime that started it is still alive.
type tools struct{ lock *os.File }

// run executes one gateway tool. Stdin carries secrets (WireGuard private keys)
// and configs, so neither ever lands on disk or in argv.
func (t tools) run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	timer := []string{"-s", "KILL", strconv.Itoa(int(commandTimeout / time.Second)), name}
	cmd := exec.CommandContext(ctx, "timeout", append(timer, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if t.lock != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, t.lock)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
