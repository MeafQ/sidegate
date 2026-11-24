# Architecture

`sidegated` is a single static Go binary for the UniFi Cloud Gateway Ultra. It turns proxy subscriptions and server lists into standard UniFi WireGuard VPN Clients, keeping routing inside UniFi's Policy Table while Xray carries traffic and the daemon handles health checks, failover, analytics, and the embedded web UI.

A **slot** maps 1-to-1 to a UniFi VPN Client. Its **Exit** chooses where traffic goes: a server, a group, a source, **Direct** (WAN), or **Block**.

## Data path

Each slot `N` (1–8) uses a matching interface, subnet, and port set:

```text
UniFi Policy Table
  → UniFi WireGuard client (wgcltN)
  → loopback UDP 5183N
  → sidegate WireGuard interface (pxaN)
  → iptables NOTRACK + TPROXY → 127.0.0.1:1234N
  → Xray
  → proxy server (over WAN)
```

- **UniFi integration.** Each WireGuard VPN Client points at `127.0.0.1:5183N`, so UniFi manages it like any normal WireGuard tunnel.
- **Traffic interception.** `sidegate-*` iptables chains mark `pxaN` traffic as `NOTRACK` and forward TCP/UDP to Xray via `TPROXY`. UDP 443 is dropped so QUIC clients fall back cleanly to TCP.
- **Leak prevention.** An `iif pxaN unreachable` policy rule catches any packet not intercepted by `TPROXY`, so an iptables flush fails closed instead of leaking to WAN.
- **Return traffic.** Xray replies directly to the VPN Client's tunnel IP; UniFi's standard masquerade rule handles return routing to LAN clients.
- **Xray routing.** Each slot gets dedicated TCP/UDP inbounds and a balancer locked to its active server. **Direct** and **Block** route to Xray's `freedom` (blocking private/loopback targets) and `blackhole` outbounds so the tunnel stays up.
- **Scope.** IPv4 only.

## Fail-closed gates

A slot's **gate** is its `pxaN` interface (`UP` = open, `DOWN` = closed). The core invariant is that **a gate only opens after the runtime Worker verifies both Xray routing and kernel plumbing.**

- **Before opening:** The Worker rewrites Xray's routing rules from the current plan (with exponential backoff on repeated failure), reads back inbounds, rules, balancers, and route tests (`TestRoute` for TCP/UDP), verifies kernel links/rules/chains, and re-checks Xray under a gate lock immediately before bringing `pxaN` up.
- **While open:** Every reconciliation pass re-verifies state. Any failed check closes the affected gate; if state cannot be read for 30 s, all gates close.
- **If Xray crashes:** `sidegate-core.service` runs `sidegated close-gates` via `ExecStopPost`, immediately taking all `pxaN` links down even if `sidegated` is stopped or hung.
- **When all servers fail:** The gate closes, UniFi marks the VPN Client disconnected, and UniFi's own kill switch or fallback applies.
- **At boot:** No `pxaN` interface exists until verified, so all slots start fail-closed.

Server switches are hitless: add new outbound → switch balancer override → remove old outbound. Because Xray runs in its own systemd unit, restarting `sidegated` keeps active flows alive (without failover until the daemon returns and adopts the running core).

## Package layout

| Package | Responsibility |
|---|---|
| `cmd/sidegated` | CLI entry points: `serve`, `close-gates`, `status`, `install`, `upgrade`, `update`, `recover`, `uninstall` |
| `internal/domain` | Validated domain types (servers, groups, sources, slots, selections) |
| `internal/catalogue` | Subscription fetching (HTTPS-only with SSRF guards), feed parsing, and server lineage matching across renames |
| `internal/runtime` | The single Worker actor managing Xray, WireGuard links, `iptables`, and `ip rule` |
| `internal/unifi` | UniFi Network API client (manages only sidegate-owned VPN Clients; reads routes to guard deletions) |
| `internal/metrics` | `metrics.db` storage: batched samples, rollups, retention |
| `internal/daemon` | Controller, `state.db`, probing loop, selection policy, HTTP API, lifecycle, and updater |

State ownership is single-writer: the controller owns `state.db` (committing intent before acting), the metrics writer batches writes to `metrics.db` to reduce flash wear, and the runtime Worker keeps no durable state and reconstructs reality from Xray and the kernel.

A UniFi VPN Client's `_id` is preserved for the lifetime of its slot so Policy Table routes remain valid. sidegate refuses to delete any client targeted by an active route.

## Selection and failover

The daemon makes all routing decisions (Xray's built-in observatory is disabled). A group or source maintains one shared selection for every slot assigned to it.

- **Active checks:** Serving servers get a heartbeat every 60 s (10 s when suspect) and a UDP check every 5 min. New servers run 11 checks over 2 days; failed servers back off from 1 min up to 6 h. All probes run through a dedicated loopback Xray inbound.
- **Passive signals:** Stalled TCP sockets, retransmits, uplink-without-downlink, and Xray dial warnings trigger confirmation probes — they never switch traffic unilaterally.
- **Two-round confirmation:** Failover requires two probe rounds (1 s apart) where the active server fails and the same candidate passes.
- **WAN outage protection:** Probes failing while WAN is down are never charged against servers.
- **Recovery probing:** Closed slots probe their top 4 candidates every 5 s and the rest every 20 s+.
- **Ranking & optimization:** Passing servers rank ahead of recovering and failed ones, ordered by ping + jitter weighted by recent incidents (with a 5 min cooldown after a failure). Every 30 min, serving groups compare against the top 2 rivals and switch after 3 consecutive wins.
- **Manual server override:** Picking a server inside a group or source probes it first and, if it passes, applies a score boost that decays over 24 hours (cleared immediately on any switch or manual reset). Automatic failover stays active.
- **Single-server exits:** Never fail over; the gate closes if the server dies.

Target SLA: with WAN up and at least one of the next three candidates healthy, new connections recover within 80 s of a failure.

## Authentication and security

sidegate has no separate user accounts — anyone authorized by UniFi's login can manage all settings and trigger updates.

- **Nginx auth hook:** Mounted inside UniFi's server block using UniFi's own `auth_request` include. If UniFi changes its auth contract, sidegate fails closed (`401`).
- **Local proof:** Nginx forwards `X-Sidegate-Auth` and a random root-only `X-Sidegate-Proof` token checked in constant time over a restricted Unix socket (`root` + nginx group).
- **Dual CSRF protection:** Writes must pass UniFi's `X-Csrf-Token` check in nginx and sidegate's per-process `X-Sidegate-Csrf` header. Cookies and UniFi tokens are stripped before reaching the daemon.
- **Credentials & ports:** The UniFi API key is stored at `/data/sidegate/network-key` (`0600`) and scrubbed from all responses, logs, and errors. Firewall rules block non-root access to Xray's loopback ports.
- **Updates:** Release authenticity relies on HTTPS to the pinned GitHub repository plus SHA-256 verification (no separate offline signing key).

See [web/API.md](../web/API.md) for the HTTP API reference.

## Lifecycle and self-recovering updates

- **Release layout:** Releases live in `/data/sidegate/releases/<id>/` and Xray binaries in `cores/<digest>/`. The `current` symlink switches atomically. Updates that keep the same Xray version restart only `sidegated`, leaving active connections untouched.
- **Plan-then-apply:** All CLI lifecycle commands dry-run by default and require `--yes` to apply, serialized by `/run/sidegate-lifecycle.lock`.
- **Detached updater:** Clicking **Update** launches a one-shot `sidegate-update` systemd job that outlives the daemon restart, verifies download size and SHA-256, and runs the new bundle's `sidegated upgrade`.
- **Rollback journal:** Before switching `current`, `upgrade` records the previous symlink, units, and binary snapshot under `/data/sidegate/rollback/`. If the new daemon fails to respond or a required Xray core won't stay up, the updater restores the previous release automatically.
- **Crash recovery:** `sidegated.service` runs `rollback/sidegated recover` in `ExecStartPre`, so a power loss or kill mid-upgrade automatically rolls back on the next boot or service restart.

## Unverified on systemd 257

The Lima VM lab runs systemd 245 while the gateway runs systemd 257. The first live release upgrade on a gateway should verify:

- `ExecStopPost` executes after a crash before `Restart=` brings Xray back.
- The `sidegate-update` unit preserves both `ExecStopPost` hooks across `daemon-reload`, runs them on `SIGKILL`, and skips missing binaries cleanly.
- `ConditionPathExists=|` triggers `sidegated.service` when either path exists, and `daemon-reload` inside `ExecStartPre` takes effect for the start in progress.
- `/etc/systemd/system` units survive a UniFi firmware update (otherwise re-register them via [After a UniFi firmware update](operate.md#after-a-unifi-firmware-update)).
