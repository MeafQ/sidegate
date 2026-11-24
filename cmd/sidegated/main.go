// Command sidegated is the gateway daemon. sidegate keeps each UniFi VPN client on a working
// proxy exit.
//
//	sidegated serve        what sidegated.service runs
//	sidegated close-gates --run-dir DIR  the core unit's ExecStopPost; downs our pxa links
//	sidegated status       what is installed, then GET /api/state over the daemon socket
//	sidegated install [--bundle TARBALL] [--api-key-stdin] [--yes]
//	sidegated upgrade --bundle TARBALL [--yes] [--expect RELEASE] [--restarts-core=yes|no] [--lock-fd N] [--outcome FILE]
//	sidegated uninstall [--purge] [--yes]
//	sidegated recover [--yes]   what the daemon unit runs before each start, from /data/sidegate/rollback
//	sidegated update [--yes]    the latest release, from the installed sidegated
//	sidegated apply-update --attempt ID   the update job, which update and the page start
//	sidegated settle-update     what the update job's unit runs after it, from /data/sidegate/rollback
//	sidegated version
//
// Every command takes --data-dir (default /data/sidegate), and no flag or word it doesn't list.
// Lifecycle commands print their plan and change nothing without --yes. Run them with the sidegated from the
// bundle in hand, and update with the installed one. install with no --bundle puts back what a firmware update removed.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"sidegate/internal/daemon"
	gateway "sidegate/internal/runtime"
)

// scripts/build-page.sh fills page/ before a release build. Without it the daemon still
// runs, and answers that this build has no page.
//
//go:embed all:page
var pageFiles embed.FS

// scripts/build-bundle.sh sets both: the version, a tag such as v0.1.0 for a release, and the
// releases page a release trusts for its updates. A checkout build has no page.
var (
	version  = "dev"
	releases string
)

var build = daemon.Build{Version: version, Releases: releases}

func builtPage() (fs.FS, error) { return fs.Sub(pageFiles, "page") }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "sidegate keeps each UniFi VPN client on a working proxy exit.\nusage: sidegated serve|close-gates|status|install|upgrade|uninstall|recover|update|version [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Each command defines only its own flags and takes no other argument, so a stray word, which
	// would end parsing and drop every flag after it, or another command's flag is refused.
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	dataDir := flags.String("data-dir", "/data/sidegate", "persistent state and releases")
	bundle := func() *string { return flags.String("bundle", "", "staged release for install or upgrade") }
	yes := func() *bool { return flags.Bool("yes", false, "apply the printed lifecycle plan") }
	var run func() error
	switch command {
	case "serve":
		run = func() error { return serve(ctx, *dataDir) }
	case "close-gates":
		runDir := flags.String("run-dir", "", "the runtime's run dir, which holds the gate lock close-gates waits on")
		run = func() error { return closeGates(ctx, *runDir) }
	case "status":
		run = func() error { return status(ctx, *dataDir, os.Stdout) }
	case "install":
		bundle, yes := bundle(), yes()
		keyFromStdin := flags.Bool("api-key-stdin", false, "install reads UniFi's API key from stdin")
		run = func() error {
			request := daemon.LifecycleRequest{Action: daemon.Install, DataDir: *dataDir, BundlePath: *bundle, Build: build}
			if *keyFromStdin {
				request.APIKey = os.Stdin
			}
			return lifecycle(ctx, request, *yes)
		}
	case "upgrade":
		bundle, yes := bundle(), yes()
		expect := flags.String("expect", "", "upgrade refuses, as moved, unless this release is current")
		var restartsCore *bool
		flags.Func("restarts-core", "yes or no: upgrade refuses, as planChanged, unless its plan agrees", func(answer string) error {
			if answer != "yes" && answer != "no" {
				return errors.New("want yes or no")
			}
			yes := answer == "yes"
			restartsCore = &yes
			return nil
		})
		lockFD := flags.Int("lock-fd", 0, "the caller holds the lifecycle lock on this descriptor")
		outcome := flags.String("outcome", "", "upgrade writes {outcome, message} to this file when it ends")
		run = func() error {
			return lifecycle(ctx, daemon.LifecycleRequest{Action: daemon.Upgrade, DataDir: *dataDir, BundlePath: *bundle,
				Expect: *expect, RestartsCore: restartsCore, LockFD: *lockFD, Outcome: *outcome}, *yes)
		}
	case "uninstall":
		yes := yes()
		purge := flags.Bool("purge", false, "uninstall also removes saved sources, keys and our VPN Clients no route targets")
		run = func() error {
			return lifecycle(ctx, daemon.LifecycleRequest{Action: daemon.Uninstall, DataDir: *dataDir, Purge: *purge}, *yes)
		}
	case "recover":
		yes := yes()
		run = func() error {
			return lifecycle(ctx, daemon.LifecycleRequest{Action: daemon.Recover, DataDir: *dataDir}, *yes)
		}
	case "update":
		yes := yes()
		run = func() error { return daemon.RunUpdate(ctx, *dataDir, build, *yes, os.Stdout) }
	case "apply-update":
		attempt := flags.String("attempt", "", "the attempt to apply, whose record names the rest")
		run = func() error { return daemon.ApplyUpdate(ctx, *dataDir, build, *attempt, os.Stdout) }
	case "settle-update":
		run = func() error { return daemon.SettleUpdate(ctx, *dataDir, os.Stdout) }
	case "version":
		run = func() error {
			fmt.Println(version)
			if releases != "" {
				fmt.Println(releases)
			}
			return nil
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		os.Exit(2)
	}
	flags.Parse(os.Args[2:])
	if flags.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s takes flags only, not %q\n", command, flags.Arg(0))
		flags.Usage()
		os.Exit(2)
	}
	err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, dataDir string) error {
	app, err := open(ctx, dataDir)
	if err != nil {
		return err
	}
	defer app.Close()
	return app.Run(ctx)
}

// open is the daemon serve runs, with this build's page and version.
func open(ctx context.Context, dataDir string) (*daemon.App, error) {
	b, err := daemon.LoadBootstrap(dataDir)
	if err != nil {
		return nil, err
	}
	page, err := builtPage()
	if err != nil {
		return nil, err
	}
	app, err := daemon.Open(ctx, b)
	if err != nil {
		return nil, err
	}
	app.SetPage(page)
	app.SetBuild(build)
	return app, nil
}

// closeGates reads no state file, so the stop hook works even when /data or the
// daemon's bootstrap is unreadable.
func closeGates(ctx context.Context, runDir string) error {
	if runDir == "" {
		return errors.New("close-gates needs --run-dir")
	}
	return gateway.CloseGates(ctx, runDir)
}

// status asks the running daemon over its socket with the hook's own proof, which only
// root can read.
func status(ctx context.Context, dataDir string, out io.Writer) error {
	daemon.InstallStatus(ctx, dataDir, out)
	b, err := daemon.LoadBootstrap(dataDir)
	if err != nil {
		return err
	}
	client := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", b.ListenSocket)
	}}, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sidegated"+b.BasePath+"api/state", nil)
	if err != nil {
		return err
	}
	if err := daemon.VouchLocally(dataDir, req.Header); err != nil {
		return fmt.Errorf("the nginx proof can't be read: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("the daemon isn't answering on %s: %w", b.ListenSocket, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the daemon answered HTTP %d", resp.StatusCode)
	}
	var view daemon.View
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		return fmt.Errorf("the daemon's answer can't be read: %w", err)
	}
	printStatus(out, view)
	return nil
}

func printStatus(out io.Writer, view daemon.View) {
	fmt.Fprintf(out, "wan %s, revision %d\n", view.WAN, view.Revision)
	for _, s := range view.Slots {
		network := "no VPN Client"
		if s.Runtime.NetworkID != nil {
			network = "VPN Client " + *s.Runtime.NetworkID
		}
		applied := "none"
		if s.AppliedServer != nil {
			applied = string(*s.AppliedServer)
		}
		client := s.Unifi.Client
		if s.Unifi.Trouble != nil {
			client += ", " + string(*s.Unifi.Trouble)
		}
		fmt.Fprintf(out, "slot %s %q %s %s applied=%s %s, client %s\n", s.ID, s.Name, s.Status, s.Runtime.Phase, applied, network, client)
	}
	for _, s := range view.Sources {
		fetch := "pasted"
		if s.Origin.RemoteOriginDTO != nil {
			fetch = "ok"
			if s.Origin.Fetch.FailureCode != nil {
				fetch = string(*s.Origin.Fetch.FailureCode)
			}
		}
		fmt.Fprintf(out, "source %s %q %d servers, %d missing, last fetch %s\n", s.ID, s.Name, s.Stored, s.Missing, fetch)
	}
	for _, f := range view.VPNClients.Foreign {
		if f.Trouble != nil {
			fmt.Fprintf(out, "client %s %q %s\n", f.ID, f.Name, *f.Trouble)
		}
	}
}

// lifecycle with --yes plans again under the lifecycle lock, so the plan it prints is the one it runs.
func lifecycle(ctx context.Context, request daemon.LifecycleRequest, apply bool) error {
	request.Out = os.Stdout
	if apply {
		if runtime.GOOS != "linux" || os.Geteuid() != 0 {
			return errors.New("applying a lifecycle plan needs root on the gateway")
		}
		return daemon.RunLifecycle(ctx, request)
	}
	plan, err := daemon.PlanLifecycle(ctx, request)
	if err != nil {
		return err
	}
	plan.Print(os.Stdout)
	fmt.Println("Nothing changed. Run again with --yes to apply.")
	return nil
}
