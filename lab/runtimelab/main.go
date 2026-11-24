// Command runtimelab drives the runtime Worker in the lab VM with a fixed plan:
// two slots over srvA and srvB, keys from topology.sh. It stands in for the
// controller, which owns plans in the daemon.
//
//	runtimelab unit                     print the core unit text
//	runtimelab once [--select slot=srv] [--from slot=srv] [--with servers] [--slots ids] [--through-restart] reconcile until a pass changes nothing
//	runtimelab run [--slots ids]        the long-running actor, one log line per pass
//	runtimelab teardown                 the empty plan: remove everything owned
//	runtimelab probe SERVER URL         GET URL through SERVER's probe route
//	runtimelab telemetry                print one outbound counter sample
//	runtimelab churn                    plans that change only probe roots, one log line per pass
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
	"sidegate/lab/labnames"
)

const (
	keys   = "/var/lib/unifi-lab"
	uuid   = "6f1c1e52-0d3c-4b8e-9a51-2c1f7c0a5e11"
	badKey = "lab-bad-shadowsocks-key"
)

var options = gateway.Options{DataDir: labnames.DataDir, RunDir: labnames.RunDir}

type labSlot struct {
	id       domain.SlotID
	ordinal  uint8
	tunnel   string
	selected string
}

var labSlots = []labSlot{
	{"slot1", 1, "10.70.1.0/30", "srvA"},
	{"slot2", 2, "10.70.2.0/30", "srvB"},
}

var labServers = map[string]string{"srvA": "198.51.100.10", "srvB": "198.51.100.20"}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: runtimelab unit|once|run|teardown|probe|telemetry|churn")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	selects := flags.String("select", "", "comma-separated slot=server overrides of the fixture's selection")
	with := flags.String("with", "", "comma-separated extra probe roots: hy, badkey, unrendered")
	from := flags.String("from", "", "comma-separated slot=server, what a slot serves now, for a move back to the fixture's selection")
	through := flags.Bool("through-restart", false, "keep polling while the core restarts, rather than end on the first pass that finds it so")
	only := flags.String("slots", "", "comma-separated lab slots the plan serves, the others removed; empty serves both")
	flags.Parse(os.Args[2:])

	var err error
	switch os.Args[1] {
	case "unit":
		unit := gateway.CoreUnit(options)
		_, err = os.Stdout.Write(unit)
	case "once":
		err = once(ctx, *selects, *from, *with, *only, *through)
	case "run":
		err = runForever(ctx, *selects, *only)
	case "teardown":
		err = teardown(ctx)
	case "probe":
		err = probe(ctx, flags.Arg(0), flags.Arg(1))
	case "telemetry":
		err = telemetry(ctx)
	case "churn":
		err = churn(ctx)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtimelab:", err)
		os.Exit(1)
	}
}

// ref digests everything server renders, so a changed certificate is a new
// revision with a new outbound tag, as the catalogue's digests are.
func ref(name string) domain.ServerRef {
	pin, _ := certPin(name)
	return domain.ServerRef{Lineage: domain.LineageID(name), Digest: sha256.Sum256([]byte(name + "|" + labServers[name] + "|" + uuid + "|" + pin))}
}

func certPin(name string) (string, error) {
	pemBytes, err := os.ReadFile(keys + "/" + name + ".crt")
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", fmt.Errorf("%s.crt holds no certificate", name)
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// server pins the lab server's self-signed certificate, the way a feed's
// allowInsecure entry is carried into Xray v26.
func server(name string) (domain.Server, error) {
	switch name {
	case "hy":
		spec, err := domain.NewProtocol(domain.Hysteria2, domain.NewSecret("hy-auth"), "", "", "")
		if err != nil {
			return domain.Server{}, err
		}
		return domain.NewServer("root", []domain.Hop{{ID: "root", Endpoint: domain.Endpoint{Host: "198.51.100.20", Port: 8443}, Protocol: spec,
			Transport: domain.Transport{Kind: domain.Hysteria, Salamander: domain.NewSecret("hy-mask")},
			TLS:       domain.TLS{Enabled: true, ServerName: "hy.lab"}}})
	case "badkey":
		spec, err := domain.NewProtocol(domain.Shadowsocks, domain.NewSecret(badKey), "", "2022-blake3-aes-128-gcm", "")
		if err != nil {
			return domain.Server{}, err
		}
		return domain.NewServer("root", []domain.Hop{{ID: "root", Endpoint: domain.Endpoint{Host: "198.51.100.20", Port: 8388}, Protocol: spec,
			Transport: domain.Transport{Kind: domain.TCP}}})
	case "unrendered":
		spec, err := domain.NewProtocol(domain.VLESS, domain.NewSecret(uuid), "", "", "")
		if err != nil {
			return domain.Server{}, err
		}
		return domain.NewServer("root", []domain.Hop{{ID: "root", Endpoint: domain.Endpoint{Host: "198.51.100.20", Port: 443}, Protocol: spec,
			Transport: domain.Transport{Kind: domain.TCP, Path: "/no-such-field-on-raw"}}})
	}
	protocol, err := domain.NewProtocol(domain.VLESS, domain.NewSecret(uuid), "", "", "xtls-rprx-vision")
	if err != nil {
		return domain.Server{}, err
	}
	pin, err := certPin(name)
	if err != nil {
		return domain.Server{}, err
	}
	return domain.NewServer("root", []domain.Hop{{
		ID: "root", Endpoint: domain.Endpoint{Host: labServers[name], Port: 443}, Protocol: protocol,
		Transport: domain.Transport{Kind: domain.TCP},
		TLS:       domain.TLS{Enabled: true, ServerName: name + ".lab", PinnedCertSHA256: []string{pin}},
	}})
}

func readKey(name string) (string, error) {
	b, err := os.ReadFile(keys + "/" + name)
	return strings.TrimSpace(string(b)), err
}

// plan builds the fixture's plan with extraProbes more probe roots, which no slot
// selects, and the extra servers named in with, which --select may choose. A slot
// only leaves out is a removal.
func plan(generation uint64, selects, from, with, only string, extraProbes int) (gateway.Plan, error) {
	chosen, prior := map[domain.SlotID]string{}, map[domain.SlotID]string{}
	for _, s := range labSlots {
		chosen[s.id] = s.selected
	}
	for _, kv := range strings.Split(selects, ",") {
		if slot, srv, ok := strings.Cut(kv, "="); ok {
			prior[domain.SlotID(slot)] = chosen[domain.SlotID(slot)]
			chosen[domain.SlotID(slot)] = srv
		}
	}
	for _, kv := range strings.Split(from, ",") {
		if slot, srv, ok := strings.Cut(kv, "="); ok {
			prior[domain.SlotID(slot)] = srv
		}
	}
	servers := map[domain.ServerRef]domain.Server{}
	for name := range labServers {
		s, err := server(name)
		if err != nil {
			return gateway.Plan{}, err
		}
		servers[ref(name)] = s
		for i := range extraProbes {
			if name == "srvA" {
				extra := fmt.Sprintf("probe%d", i)
				servers[domain.ServerRef{Lineage: domain.LineageID(extra), Digest: sha256.Sum256([]byte(extra))}] = s
			}
		}
	}
	for _, name := range strings.FieldsFunc(with, func(r rune) bool { return r == ',' }) {
		s, err := server(name)
		if err != nil {
			return gateway.Plan{}, err
		}
		servers[ref(name)] = s
	}
	var targets []gateway.SlotTarget
	var removals []domain.Slot
	served := strings.FieldsFunc(only, func(r rune) bool { return r == ',' })
	for _, s := range labSlots {
		if len(served) > 0 && !slices.Contains(served, string(s.id)) {
			removals = append(removals, domain.Slot{ID: s.id, Allocation: allocation(s, "")})
			continue
		}
		host, err := readKey(fmt.Sprintf("pxa%d.key", s.ordinal))
		if err != nil {
			return gateway.Plan{}, err
		}
		client, err := readKey(fmt.Sprintf("wgclt%d.pub", s.ordinal))
		if err != nil {
			return gateway.Plan{}, err
		}
		target := gateway.SlotTarget{
			Slot: domain.Slot{ID: s.id, Version: 1, Allocation: allocation(s, client)},
			Keys: gateway.KeyMaterial{Host: domain.NewSecret(host)}, Selection: domain.Serving(ref(chosen[s.id])),
		}
		// A move keeps the gate up because the controller allows the old server while it lands.
		if was, ok := prior[s.id]; ok && was != chosen[s.id] {
			target.Passage = gateway.MoveFrom(ref(was), time.Now().Add(30*time.Second))
		}
		targets = append(targets, target)
	}
	return gateway.NewPlan(generation, targets, servers, removals)
}

func allocation(s labSlot, clientPublic string) domain.Allocation {
	return domain.Allocation{
		Ordinal: s.ordinal, Tunnel: netip.MustParsePrefix(s.tunnel),
		PeerListenPort: 51830 + uint16(s.ordinal), TPROXYPort: 12340 + uint16(s.ordinal),
		Keys: domain.SlotKeys{ClientPublic: clientPublic},
	}
}

func emptyPlan() (gateway.Plan, error) {
	var removals []domain.Slot
	for _, s := range labSlots {
		removals = append(removals, domain.Slot{ID: s.id, Allocation: allocation(s, "")})
	}
	return gateway.NewPlan(1, nil, nil, removals)
}

// converge runs the Worker on p until a pass changes nothing, printing each
// pass. A pass that changed nothing and failed ends it, unless through is set
// and the core is only restarting. It returns the converged report and the number of changes made. The
// Worker keeps running, and sampling telemetry, until the process exits.
func converge(ctx context.Context, p gateway.Plan, telemetry chan gateway.Telemetry, through bool) (gateway.Report, int, error) {
	opts := options
	opts.Poll = time.Second
	w, err := gateway.Open(opts)
	if err != nil {
		return gateway.Report{}, 0, err
	}
	goals, reports := make(chan gateway.Plan, 1), make(chan gateway.Report)
	goals <- p
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, goals, reports, telemetry) }()
	changes := 0
	deadline := time.After(90 * time.Second)
	for pass := 1; ; pass++ {
		select {
		case <-deadline:
			return gateway.Report{}, changes, errors.New("no converged pass within 90 s")
		case err := <-done:
			return gateway.Report{}, changes, cmpErr(err, errors.New("runtime stopped"))
		case r := <-reports:
			printPass(pass, r)
			changes += len(r.Steps)
			fmt.Printf("changes: %d\n", changes)
			switch {
			case len(r.Steps) == 0 && r.Error == "" && judged(p, r):
				return r, changes, nil
			case len(r.Steps) == 0 && r.Error != "" && !(through && restarting(r.Error)):
				return r, changes, errors.New(r.Error)
			}
		}
	}
}

// restarting says a pass failed only because the core is restarting: its unit is between states, or
// nothing listens on its API yet. The Worker polls on through that, and with --through-restart so does
// the runner, until its deadline.
func restarting(err string) bool {
	return strings.Contains(err, "core unit is activating") || strings.Contains(err, "core unit is deactivating") ||
		strings.Contains(err, "core API: ") && strings.Contains(err, "connection refused")
}

// judged says the core has answered for every plan server: a route is up, or it was refused. Judging
// runs beside the passes, so a pass over a stopped core can change nothing and still have this to come.
func judged(p gateway.Plan, r gateway.Report) bool {
	for _, ref := range p.Servers() {
		routed := slices.ContainsFunc(r.Topology.Probes, func(route gateway.ProbeRoute) bool { return route.Server == ref })
		refused := slices.ContainsFunc(r.Refused, func(x gateway.RefusedServer) bool { return x.Server == ref })
		if !routed && !refused {
			return false
		}
	}
	return true
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func printPass(n int, r gateway.Report) {
	fmt.Printf("%s pass %d: %d steps\n", time.Now().Format("15:04:05.000"), n, len(r.Steps))
	for _, s := range r.Steps {
		fmt.Printf("  %s\n", s)
	}
	for _, x := range r.Refused {
		fmt.Printf("  refused %s: %s\n", x.Server.Lineage, x.Reason)
	}
	for _, s := range r.Slots {
		fmt.Printf("  slot %s phase %s\n", s.Slot, map[gateway.Phase]string{gateway.Closed: "closed", gateway.Ready: "ready", gateway.Unknown: "unknown"}[s.Phase])
		if s.ErrorCode != "" {
			fmt.Printf("  slot %s: %s\n", s.Slot, s.ErrorCode)
		}
	}
	if r.Error != "" {
		fmt.Printf("  error: %s\n", r.Error)
	}
}

func once(ctx context.Context, selects, from, with, only string, through bool) error {
	p, err := plan(1, selects, from, with, only, 0)
	if err != nil {
		return err
	}
	_, _, err = converge(ctx, p, nil, through)
	return err
}

func teardown(ctx context.Context) error {
	p, err := emptyPlan()
	if err != nil {
		return err
	}
	r, _, err := converge(ctx, p, nil, false)
	if err == nil && len(r.Deleted) != len(labSlots) {
		err = fmt.Errorf("deleted %v, want every lab slot", r.Deleted)
	}
	return err
}

func runForever(ctx context.Context, selects, only string) error {
	p, err := plan(1, selects, "", "", only, 0)
	if err != nil {
		return err
	}
	opts := options
	opts.Poll = 2 * time.Second
	w, err := gateway.Open(opts)
	if err != nil {
		return err
	}
	goals, reports := make(chan gateway.Plan, 1), make(chan gateway.Report)
	goals <- p
	go func() {
		for pass := 1; ; pass++ {
			select {
			case r := <-reports:
				printPass(pass, r)
			case <-ctx.Done():
				return
			}
		}
	}()
	return w.Run(ctx, goals, reports, nil)
}

func probe(ctx context.Context, name, url string) error {
	p, err := plan(1, "", "", "", "", 0)
	if err != nil {
		return err
	}
	r, _, err := converge(ctx, p, nil, false)
	if err != nil {
		return err
	}
	for _, route := range r.Topology.Probes {
		if route.Server != ref(name) {
			continue
		}
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
				return route.Dial(ctx, "tcp", address)
			},
		}}
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		fmt.Printf("probe %s: %s\n", name, strings.TrimSpace(string(body)))
		return err
	}
	return fmt.Errorf("no verified probe route for %s in topology", name)
}

func telemetry(ctx context.Context) error {
	p, err := plan(1, "", "", "", "", 0)
	if err != nil {
		return err
	}
	samples := make(chan gateway.Telemetry, 1)
	r, _, err := converge(ctx, p, samples, false)
	if err != nil {
		return err
	}
	fmt.Printf("core pid %d epoch %s\n", r.Topology.CorePID, r.Topology.Epoch)
	select {
	case s := <-samples:
		for _, sig := range s.Signals {
			fmt.Printf("counter %s up %d down %d epoch %s\n", sig.Server.Lineage, *sig.Up, *sig.Down, sig.Stamp.Epoch)
		}
		return nil
	case <-time.After(15 * time.Second):
		return errors.New("no telemetry sample within 15 s")
	}
}

// churn serves the fixture's plan, adds a probe root every 3 s, then drops them
// all and stays through their drain. Slot selections never change.
func churn(ctx context.Context) error {
	first, err := plan(1, "", "", "", "", 0)
	if err != nil {
		return err
	}
	opts := options
	opts.Poll = 2 * time.Second
	w, err := gateway.Open(opts)
	if err != nil {
		return err
	}
	goals, reports := make(chan gateway.Plan, 1), make(chan gateway.Report)
	goals <- first
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, goals, reports, nil) }()
	go func() {
		for pass := 1; ; pass++ {
			select {
			case r := <-reports:
				printPass(pass, r)
			case <-ctx.Done():
				return
			}
		}
	}()
	send := func(generation uint64, extras int) error {
		time.Sleep(3 * time.Second)
		next, err := plan(generation, "", "", "", "", extras)
		if err != nil {
			return err
		}
		fmt.Printf("%s plan %d: %d extra probe roots\n", time.Now().Format("15:04:05.000"), generation, extras)
		goals <- next
		return nil
	}
	for extras := 1; extras <= 8; extras++ {
		if err := send(uint64(extras+1), extras); err != nil {
			return err
		}
	}
	if err := send(10, 0); err != nil {
		return err
	}
	time.Sleep(35 * time.Second)
	fmt.Printf("%s churn done\n", time.Now().Format("15:04:05.000"))
	return <-done
}
