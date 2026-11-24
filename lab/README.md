# Lab

A Lima VM that runs design B's data path on kernel 5.4, so it can be proved before anything touches the gateway. The VM's root namespace plays the UCG Ultra. Network namespaces play the LAN client, the internet, two VLESS servers and a plain WireGuard server.

Needs Lima 2.x and Go on a Mac. The VM is Ubuntu 20.04 arm64 with its 5.4 kernel, iptables-legacy and Xray v26.10.31.

## Use

```sh
lab/up.sh               # create or start the unifi-lab VM, then provision it
lab/verify-datapath.sh  # rebuild the topology, run every check, write lab/results/datapath-<date>.md
lab/verify-runtime.sh   # let the daemon's runtime build the slots, rerun checks b-h, write lab/results/runtime-<date>.md
lab/verify-control.sh   # run the daemon's controller against a real core on stand-in servers, write lab/results/control-<date>.md
lab/verify-page.sh      # run sidegated behind nginx, drive the real page in a browser, write lab/results/page-<date>.md
lab/verify-lifecycle.sh # launch the update job, paste the one command, take a bundle through install, reboots, upgrades cut short, uninstall and updates, write lab/results/lifecycle-<date>.md
lab/probe-routes.sh     # time one on-demand probe route on the pinned core beside 12 and 64, print what lab/results/probe-routes-<date>.md reports
lab/step.sh SUITE STEP  # run named steps alone: control, runtime, page, lifecycle or checks
lab/down.sh             # delete the VM and lab/.state
```

`verify-datapath.sh`, `verify-runtime.sh` and `verify-control.sh` exit non-zero if any check fails. All scripts are safe to rerun. `verify-runtime.sh` restarts the VM first, so the runtime installs from a fresh boot where table 100 has never existed. It then runs `topology.sh --no-slots`, which leaves out pxaN, the slot rules and the gateway Xray, so the runtime builds them.

Every control step builds the world it needs, so `lab/step.sh control kill` runs alone. `--keep` leaves that world running for the next call, which saves a rebuild. `lab/step.sh page board pause` runs those browser scenarios after the sources and slot they need, and `lab/step.sh page update` the update's world alone. A lifecycle step that follows a reboot can't run this way, and a runtime or lifecycle snapshot that doesn't follow a fresh boot can show what an earlier suite left in `/run` or ip6tables.

To try the page by hand, `lab/live.sh LAN_IP` runs the page lab's daemon, nginx and stand-ins and serves the page on `http://LAN_IP:5465/sidegate/` with the stand-in login, until Ctrl-C. It tunnels to the VM's nginx on 127.0.0.1:5466, and `live-proxy.py` adds the login cookie. Each start begins with the sources and history the last one left, and `--fresh` begins with none. Its start marks the daemon's `state.db` and `metrics.db` as the live page's, and the next world reset, whichever suite or start runs it, moves them to `/var/lib/unifi-lab/live` in the VM, where the next start takes them back. A daemon built with another schema refuses them, and then only `--fresh` starts. With `--real-targets` the bootstrap has no `probeTargets` or `baselineTargets`, so the daemon checks servers against its built-in public test pages and a real subscription added on the page can pass. The suites keep the lab targets.

To rebuild only the topology, or to run one check by hand:

```sh
limactl shell unifi-lab sudo /opt/unifi-lab/topology.sh
limactl shell unifi-lab sudo /opt/unifi-lab/checks.sh c
```

## Checks

The quick check is `scripts/quick.sh`, or `scripts/quick.sh go` or `page` for one half. It stops at the first failure, and `E2E_PORT` moves the page checks' ports. Its Go half is `scripts/check-skeleton.sh` (gofmt, `go vet ./...`, and the arm64 gateway build), `scripts/check-writes.sh`, and `go test -short ./...`. Its page half is typecheck, lint, unit tests, fuzz smoke (`node tests/e2e/fuzz.mjs --smoke`), contract tests, and browser checks against the fake daemon (`npm run test:e2e`). A Go test that waits 100 ms or more on the real clock calls `slowTest`, so only the full Go suite runs it.

The slow checks run from `web/` or from the repo root with the Lima VM up:

| Check | Covers | Command |
|---|---|---|
| Full Go suite | everything `-short` skips (live controller and timed tests) | `go test ./... -timeout 30m -count=1` |
| Install script cuts | `install.sh` cut at every byte | `(cd lab/labweb && go test -race -count=1 ./...)` |
| Browser checks on Go daemon | `e2e`, `save`, and `retry` against `internal/daemon` | `npm run test:real` |
| Fuzz | randomized two-press and concurrent-write sequences | `npm run test:fuzz` |
| Runtime lab | Xray + kernel plumbing on kernel 5.4 | `lab/verify-runtime.sh` |
| Control lab | controller against a real Xray core and stand-in servers | `lab/verify-control.sh` |
| Page lab | built UI against real daemon behind nginx | `lab/verify-page.sh` |
| Lifecycle lab | install, update job, rollback recovery, and uninstall | `lab/verify-lifecycle.sh` |
| Data path | TPROXY + WireGuard leak checks | `lab/verify-datapath.sh` |
| Network Application lab | UniFi Network API contract checks | `lab/net/verify.sh` |

## Network Application lab

`lab/net/` is a second, separate VM, `unifi-lab-net`, that runs Network Application 10.6.109 for `internal/unifi`. It never touches the `unifi-lab` VM.

```sh
lab/net/verify.sh  # create the VM if needed, wipe the app, run every step, rewrite internal/unifi/testdata/contract.json, write lab/results/net-<date>.md
lab/net/stop.sh    # stop the VM and keep its disk
lab/net/down.sh    # delete the VM
```

`verify.sh` exits non-zero if any check fails. `go test -short ./internal/unifi` replays the recorded contract and needs no VM.

To keep the VM off your LAN, set `LAB_BLOCK_SUBNET` to its IPv4 subnet, such as `192.0.2.0/24`, when you create the VM. The VM then rejects packets to it from its first boot, and later runs with the variable set check the guard. The VM uses public resolvers rather than the Mac's, which can be a gateway on that LAN. `netlab` and `unifi.New` both refuse any address but loopback.

What it proves and doesn't:

- It proves what the app accepts, stores, returns and refuses, with no gateway and with a simulated UXG adopted over inform (`informsim`).
- A plain app has no UniFi Core, so it can't validate an API key. `netlab` signs in with a session instead, in lab code only.
- It can't prove that a real gateway renders a client into a working `wgcltN`. The app won't list a console model such as UDRULT, and a UXG takes its rendered config over a channel the simulator doesn't speak, so the config itself is never seen.

## Files

- `lima.yaml` is the VM template. `up.sh` and `down.sh` manage the VM. `lib.sh` holds their shared helpers.
- `names.sh` holds the names the lab shares with the daemon: its folders, binary, units, files, socket and login headers. `lib.sh` and the guest scripts source it, and `labnames/` is the lab programs' copy, which a quick test holds to both.
- `provision.sh` runs in the VM. It installs packages, switches iptables to legacy and installs the Xray `deploy/xray.pin` pins, by digest.
- `iproute2.sh` runs in the VM. It builds iproute2 5.9, the gateway's, and 6.1 by digest, once, for step named.
- `topology.sh` runs in the VM. It tears down and rebuilds the namespaces, the UniFi-like rules and the two slots.
- `checks.sh` runs in the VM, one check per call. Its environment can point it at plumbing the daemon built.
- `runtime.sh` runs in the VM, one runtime step per call: fresh boot, install, idempotency, egress guard, root-only core listeners, IPv6 listen-port guard, flush repair, conflict, another owner's rule, probe routes, telemetry, hot switch, routing drift, daemon restart, idle CPU, plans that change only probe roots, flapping, rewrites that can't land, brief foreign rules, a held gate lock, an API stall, slow reads, a held listen port, a hot add whose UDP port another process holds (`heldadd`), a core outage, core stop and crash, core death during a link up, orphaned and wedged tools, a crash right after a link is created, marked foreign state, foreign rules read whole, a failed rule delete, refused servers, the renderer against the pinned core, named markers, teardown.
- `runtimelab/` stands in for the controller. It drives the runtime Worker with a fixed two-slot plan.
- `control.sh` runs in the VM, one controller step per call, each building the world it asks for. With production's failure timings and a real core it proves: killed, silent, 300 ms added and 25% loss replaced (`kill`, `blackhole`, `degrade`, `loss`), an Exit change that keeps the gate up, leaves the client and routes alone and closes the slot's flows (`exitchange`), no SYN retry for a connection opened during any of 20 Exit changes, with the datagrams each loses counted (`swapgap`), a move whose UDP relisten loses the port to another process, closing the gate until the port is free (`heldmove`), the 30 s deadline, through an edit and a restart (`deadline`), a stalled `xray run -test` (`stalltest`), wedged reads (`wedge`), Pause (`pause`), Direct with a real core and a stand-in that counts UniFi writes (`direct`), a move closing a slot's open connections and nothing else's (`closeflows`), a slot's UDP on a port below 1024 (`lowudp`), a server's broken UDP moving a slot only when a rival's works and every server's moving none (`udpbroken`), what a UDP check costs (`udpcost`), `tcp_info` under a lossy upload (`passive`), a WAN outage (`wan`), blocked and blipping baselines (`blocked`, `blip`), a sole member's recovery (`recover`), a server Exit leaving the feed (`assigned`), quota and expiry in the view (`quota`), a daemon killed and restarted (`restart`), CPU and memory (`cost`), saving query results for the page's schema check (`contract`), three slots on one dead server (`three`), eight dying at once (`storm`), a whole group going silent at 12 and 64 kept (`allsilent`, `allsilent64`), one censored probe target (`badtarget`), a jittery server that stays (`jitter`), a closed slot's sweep at 12 and 64 kept (`closed12`, `sweep64`), core memory at 64 roots by 8 hops (`memory`), and the Acme-shaped feed (`acme`).
- `controllab/` stands in for the page. It runs the real daemon with a world from `world.json`, applies changes to it over a local socket as the page's edits do, and prints switch events and analytics from the query functions. `controllab unifi` stands in for the Network Application, for the page lab.
- `page.sh` runs in the VM, one page step per call: nginx with the hook and a stand-in login, the stand-in Network Application, the auth contract, the daemon under `sidegated serve`, writes under UniFi's CSRF check and the daemon's, a 900 KiB pasted source through nginx, forged writes, `sidegated status`, killing or slowing a server, moving a server in the feed, and a search for secrets in the logs. `web/scripts/lab-page.mjs` drives the real page against it: the sources and a group through the + Add menu, a 404 answered by Retry's User-Agent, a source's settings saved, a slot added on the board beside a foreign client, the Exit menu, a filter preview, a group edit, a group's delete waiting on the slot that uses it, Block, Direct, the switch menu on a group and a source, Settings, a live switch, Server replaced and a provider's analytics. Its scenario `update` runs last, on `lifecycle.sh`'s gateway in place of `page.sh`'s: `v0.0.1` installed by the one command with a slot that serves, `offer` publishing `v0.0.2`, and the page's Check, Update and confirm, the page going away with the daemon and reloading itself onto `v0.0.2`. The slot carries traffic before and after, and the uninstall leaves what the snapshot before it had.
- `unifi-login.sh` is UniFi's login as both suites play it, as measured on the gateway: the cookie, the 10 s auth cache, UniFi's CSRF check and the `X-Csrf-Token` nginx hands out.
- `lifecycle.sh` runs in the VM and refuses to run anywhere else, because it uses the gateway's real paths. One lifecycle step per call. Step `job` runs the daemon's `systemd-run` line for the update job from `sglc-launcher`, a stand-in daemon. The job runs `daemon-reload` and restarts the launcher, as an update restarts the daemon, and lives on. A second launch while it runs is refused, a binary that isn't there is refused with no unit made, a job that fails is collected and the next launch runs, and `RuntimeMaxSec` ends a sleeper. It prints the systemd version it ran on: the VM has 245, the gateway 257. Step `command` types the one command from the release's `NOTES.md` into root's login shell on a terminal, against the stand-in release server, whose certificate the VM's roots hold. `unarrived` cuts `install.sh` at every byte of its last line, inside the braces it opens with and part way, with its length declared and without, over HTTP/2 and HTTP/1.1, and serves it empty, missing and redirected to `http`, each as the stand-in's own record of its answers says. The script says nothing, nothing is left on the gateway, and curl names each error but a clean end. `unchecked` does that to the bundle and `SHA256SUMS`, where the script refuses. `install` installs `v0.0.1` on a gateway with no data dir, `again` converges and stays enabled at boot, `other` is refused over another release in both directions, keeps only its log and moves by the `update --yes` it names, and `together` runs two at once, one of which is `busy`. The last two uninstall and compare with the pre-install snapshot. The releases are `v0.0.1` and `v0.0.2`, built by `scripts/build-bundle.sh` at the stand-in's address, `v0.0.3`, whose daemon hangs, and `v0.0.4`, with three's core, with the files `scripts/release-files.sh` writes in the VM, where the bundle's own `sidegated` runs. The bundle's own `sidegated` runs with no lab flags, on `/data/sidegate` and `/data/unifi-core/config/http`, against the system's nginx with a stand-in for UniFi's config. A boot unit rebuilds the lab's network before the daemon starts, as a booted gateway would have it, and another samples the gates and the core from early boot. Steps `cut kill`, `missing`, `gone`, `prior`, `core` and `recovery` kill an upgrade from release two with SIGKILL mid-switch: while a daemon that exits owns `current`, with units systemd holds that differ from two's, while one that hangs does and its binary or its whole release directory is gone, with release two's own daemon broken, and once the core runs a new core. `recovery` then kills the recovery as well, after it put `current` back and before systemd read the units, and starts the daemon. `cut reboot`, the VM's reboot and `back` do it across a reboot. `deadcore` upgrades to a core that exits at once, `downcore` upgrades to a new core, and back, where the slot's only server is one both cores refuse and no core runs, over an upgrade killed before its switch while one did, `deliberate` upgrades and downgrades over SSH after a cut-short switch, and `busy` runs two upgrades at once over a half-written journal. Step `update` installs `v0.0.1` by the one command and, with a slot configured, updates it over SSH and through the page's routes: `plan`, `refused` (each refusal, the last with the disk filled), `busy`, `moved`, `nostart`, `hung`, `unsaid` (killed, with the lock held as the old daemon comes back), `land`, `core` and `uninstall`, and `paths`, in a data dir with every kind of character a unit can carry. `paths` installs, upgrades, cuts and recovers in data and run dirs with every kind of character a unit can carry, and sees the others refused. Each ends with release two answering the signed-in page, its slot carrying traffic, its source's servers and the outcome its daemon wrote. `lab/lib.sh`'s `lifecycle_bundles` builds the four bundles the other steps use: one and two by `scripts/build-bundle.sh`, three with the pinned Xray plus bytes after its end as another core, and four with a core that exits at once, each daemon built with `go build -overlay` pinning its own core. `verify-lifecycle.sh` refuses to start beside another suite, and the stand-ins are removed at the end.
- `labweb/` is the target server. It echoes the source address it sees, serves sized downloads and serves the stand-in feeds over HTTPS on 11.0.0.80, with a certificate `topology.sh` makes and adds to the VM's roots, since the daemon fetches only HTTPS.
- `labweb` also stands in for GitHub's releases at `https://11.0.0.80/OWNER/sidegate/releases`, on the feeds' address and certificate, from `/etc/unifi-lab/releases/<tag>/`. `latest/download/<file>` redirects to `download/<tag>/<file>` for the highest tag, which redirects to `/release-assets/<tag>/<file>`. `labweb release-mode <file pattern> <mode> [n]` makes the matching files misbehave until `labweb release-mode` clears it, and `topology.sh` clears it too. It refuses a mode it can't serve, such as a typo or a cut past the end of a file the pattern names in any release, so the step fails instead of passing. A refused mode leaves the earlier one in effect and says which that is. `served`, beside the tags, records each answer: the file, the mode it was served in, `whole` or `error`, and the status. `labweb/install_test.go` runs `scripts/release-files.sh`, `scripts/release-latest.sh` and the one command from `NOTES.md`, `curl ... | sh`, against this stand-in on the Mac, for each mode the one command must survive. A bundle with a stub `sidegated`, `id`, `uname` and a directory for `/data` play the gateway. The stubs log each run and whether its stdin was the pipe. It cuts `install.sh` at every byte, close-delimited and with its length declared, over HTTP/1.1 and HTTP/2, with dash and bash as sh. The quick check's `-short` run cuts at each line's ends and middle and at every byte of the last two lines.

  | Mode | The matching file |
  |---|---|
  | `404` | answers 404 at `download/<tag>/<file>` |
  | `changed` | has its middle byte flipped, at the same length |
  | `cut [n]` | sends its full Content-Length, then drops the connection after `n` bytes, all but `-n`, or half |
  | `short [n]` | sends the same first bytes with no Content-Length and ends cleanly, close-delimited on HTTP/1.1; curl exits 0 |
  | `slow [n]` | comes at `n` bytes a second, 16 KiB by default |
  | `endless` | comes whole, then zeros without end, with no Content-Length |
  | `http` | redirects to its asset over `http` |
  | `empty` | answers 200 with no body |
- `step.sh` runs on the Mac and runs named steps alone.
- `live.sh` runs on the Mac and serves the live page on a LAN address through `live-proxy.py`.

## Topology

| Name | Role | Address |
|---|---|---|
| `lan` | client behind `br0` | 192.168.1.10 |
| root | gateway: `br0` 192.168.1.1, WAN `eth4` 192.168.100.10 | |
| `inet` | internet router | 192.168.100.1, 198.51.100.1, 203.0.113.1 |
| `srvA`, `srvB` | VLESS + TLS + Vision servers | 198.51.100.10, 198.51.100.20 |
| `wgsrv` | plain WireGuard server, the baseline | 198.51.100.30 |
| `web` | labweb, iperf3, and stand-ins for the resolvers a UDP check asks | 203.0.113.80 to .83, 8.8.8.8, 9.9.9.9, and 11.0.0.80 for the feeds and releases, from `control.sh` and `lifecycle.sh` |
| `srvC` to `srvG` | more VLESS servers, from `control.sh` only | 198.51.100.40 to .80 |
| `farm` | one core on many addresses, for worlds with many servers | 198.51.100.101 up, exiting from .100 |
| `lew` | one Reality core for the Acme-shaped feed, whose credentials are fake | 198.51.100.201 to .212, and .240 for a moved server |

UniFi traffic routes are emulated by destination. 203.0.113.81 is routed to `wgclt1` (slot 1), .82 to `wgclt2` (slot 2) and .83 to `wgclt3` (plain WireGuard). .80 goes direct.

Addresses in the feeds stay inside this table, so nothing a lab server dials leaves the VM.

## Where the lab differs from the gateway

- Main table. The VM's main table holds Lima's default route. So pref 32000 is `lookup main suppress_prefixlength 0`, table 201 holds only the lab prefixes, and a pref 40000 `lookup main` keeps the VM reachable.
- Kill switch. When a VPN Client goes down, UniFi's reaction is emulated by removing `default dev wgclt1` from table 181. What UniFi really does is not measured yet.
- iptables. The VM has iptables 1.8.4 legacy. The gateway has 1.8.7 legacy.
- CPU. Throughput numbers come from a VM on Apple silicon. They compare paths with each other and say nothing about the Cortex-A53.
