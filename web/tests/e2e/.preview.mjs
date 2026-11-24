// `npm run dev`: the page from Vite on port 8438, its API on the fake daemon seeded with a full gateway.
import { readFile } from "node:fs/promises"
import { fileURLToPath } from "node:url"
import { createServer } from "vite"
import { groupView, server, slotView, source, startFakeDaemon } from "./fake-daemon.mjs"

const ROOT = fileURLToPath(new URL("../../", import.meta.url))
const DIST = fileURLToPath(new URL("./.dist-preview/", import.meta.url))

const PORT = 5392
const BASE = "/sidegate/"


const H = (hours) => new Date(Date.now() - hours * 3600e3).toISOString()
const health = (p50, p95 = Math.round(p50 * 1.5)) => ({ verdict: "succeeded", checkedAt: H(0.1), pingAt: H(0.1), current: true, pingP50Ms: p50, pingP95Ms: p95, newConnectionP50Ms: p50 * 3, newConnectionP95Ms: p95 * 3, score: Math.round(p50 * 1.04), factor: 1.04, incidents7d: 0, seenDays: 5.2, seenWorking7dMs: 7 * 86_400_000, lastFailureAt: null, firstSeenAt: H(24 * 30) })
const config = (key, name, place, servers) => ({ key, name, place, servers, skipped: [] })

const COUNTRIES = [["bg", "Bulgaria", "BG", health(50, 90)], ["de", "Germany", "DE", health(66, 240)], ["nl", "Netherlands", "NL", health(72, 110)], ["se", "Sweden", "SE", health(100, 140)], ["us", "USA", "US", health(215, 270)], ["ro", "Romania", "RO", null]]
const unchecked = { verdict: "unknown", checkedAt: null, pingAt: null, current: false, pingP50Ms: null, pingP95Ms: null, newConnectionP50Ms: null, newConnectionP95Ms: null, score: null, factor: 1.25, incidents7d: 0, seenDays: 0, seenWorking7dMs: 0, lastFailureAt: null, firstSeenAt: H(24 * 30) }
const dead = { ...unchecked, verdict: "failed", checkedAt: H(0.1), current: true, incidents7d: 9 }
const NO_COUNTRY = { code: null, source: "unknown" }
const acme = [
  ...COUNTRIES.map(([id, label, code, h]) => server(id, label, code, h?.pingP50Ms ?? null, { source: "acme", health: h ?? dead, seen: [{ config: label.split(" ")[0], configKey: id, tag: "proxy", position: 1 }] })),
  ...COUNTRIES.map(([id, label, code]) => server(`o-${id}`, `EDGE-${label}`, code, null, { source: "acme", health: unchecked, seen: [{ config: label.split(" ")[0], configKey: id, tag: "origin", position: 2 }] })),
]
const aurora = [
  server("l-ams", "Amsterdam", null, null, { protocol: "trojan", country: NO_COUNTRY, health: { ...health(32, 48), checkedAt: H(0.3) } }),
  server("l-fra", "Frankfurt", null, null, { protocol: "trojan", country: NO_COUNTRY, health: { ...health(9, 14), checkedAt: H(72), current: false } }),
  server("l-par", "Paris", null, 19, { protocol: "hysteria2", transport: "hysteria", country: NO_COUNTRY, health: { ...health(19, 45), verdict: "failed", incidents7d: 4 } }),
]

const ACME = {
  ...source(2), id: "acme", name: "Acme", nameSet: false, firstChoice: "bg", order: ["bg", "de", "nl", "se", "us", "ro"], moves7d: 3, stored: 12,
  feed: { configs: COUNTRIES.map(([id, label], i) => config(id, label.split(" ")[0], i + 1, 2)) },
  diagnostics: [{ entry: 4, code: "unsupported_protocol", field: "wireguard" }],
  origin: {
    ...source(2).origin,
    refreshEverySeconds: 0,
    provider: { ...source(2).origin.provider, suggestedRefreshSeconds: 12 * 3600, title: "Acme", announcement: "Maintenance on Sunday night.", quota: { upload: 2e9, download: 18e9, limit: 100e9, expires: H(-24 * 40), refills: H(-24 * 10), observedAt: H(1) } },
    fetch: { checkedAt: H(0.5), updatedAt: H(12.5), failures: 0, failureCode: null },
    dueAt: H(-11.5),
  },
}
const AURORA = { ...source(1), name: "aurora.example", nameSet: false, firstChoice: "l-fra", order: ["l-fra", "l-ams", "l-par"], feed: { configs: [config("l-ams", "Amsterdam", 1, 1), config("l-fra", "Frankfurt", 2, 1), config("l-par", "Paris", 3, 1)] }, origin: { ...source(1).origin, provider: { ...source(1).origin.provider, title: "" }, fetch: { checkedAt: H(0.2), updatedAt: H(40), failures: 3, failureCode: "http_503" } } }

const routes = (...kills) => kills.map((blocks, i) => ({ id: `r${i}`, name: ["Work laptop", "Apple TV", "Guests"][i], matches: ["MacBook · all traffic", "12 domains · any device", "Guests network"][i], enabled: true, killSwitch: blocks }))
const unifi = (name, ordinal, _list, rest = {}) => ({ name, interface: `pxa${ordinal}`, known: true, client: "present", ...rest })
const placed = (id, ordinal, input, list, rest = {}, unifiRest = {}) => ({ ...slotView(id, 3, input), createdAt: H(1000 - ordinal), backups: ["nl", "se"], appliedServer: rest.appliedServer ?? "de", server: rest.appliedServer ?? "de", ...rest, unifi: unifi(input.name, ordinal, list, unifiRest) })

const SLOTS = [
  placed("eu", 1, { name: "EU", exit: { kind: "source", source: "acme" } }, routes(true, true)),
  placed("nl", 2, { name: "NL only", exit: { kind: "server", server: "nl" } }, routes(false), { appliedServer: "nl" }),
  placed("tv", 3, { name: "TV", exit: { kind: "block" } }, routes(true, false), { status: "block", appliedServer: null, server: null, backups: [] }),
  placed("down", 4, { name: "Backup", exit: { kind: "server", server: "l-par" } }, routes(true), { status: "failed", downReason: "confirmedDead", appliedServer: null, server: null }),
  placed("off", 5, { name: "Lab", exit: { kind: "source", source: "acme" } }, [], { appliedServer: "de", server: "de", leavingBy: H(-0.005) }, { client: "disabled" }),
  placed("stream", 6, { name: "Streaming", exit: { kind: "group", group: "mine" } }, routes(false), { appliedServer: "l-fra", server: "l-fra" }),
  placed("guests", 7, { name: "Guests", exit: { kind: "direct" } }, routes(false), { status: "direct", appliedServer: null, server: null, backups: [] }),
]

const rule = (sources, countries = [], box = null) => ({ sources: { not: false, values: sources }, countries: { not: false, values: countries }, protocols: { not: false, values: [] }, include: "", exclude: "", box })
const GROUPS = [
  { ...groupView("germany", 1, { name: "Germany", members: rule([], ["DE"]) }), resolved: ["de"], order: ["de"], firstChoice: "de" },
  { ...groupView("mine", 2, { name: "Fast EU", members: rule([], [], "ticked"), ticked: ["de", "l-fra"] }), resolved: ["de", "l-fra"], order: ["de", "l-fra"], firstChoice: "de" },
]

const SWITCHES = [
  { id: "s1", owner: { kind: "slot", slot: "eu" }, slots: ["eu"], intent: 1, from: { kind: "up", server: "nl" }, to: { kind: "up", server: "de" }, reason: "failureMove", requestedAt: H(2), appliedAt: H(2),
    evidence: [{ server: "nl", kind: "activeProbe", verdict: "failed", target: "gstatic-204", round: 1, at: H(2), pingMs: null, newConnectionMs: null }] },
  ...[[8, "de", "nl", "slownessMove"], [8.4, "nl", "de", "improvementMove"], [40, "de", "nl", "failureMove"], [90, "de", "bg", "failureMove"]].map(([hours, from, to, reason], i) => ({ id: `eu${i}`, owner: { kind: "slot", slot: "eu" }, slots: ["eu"], intent: 1, from: { kind: "up", server: from }, to: { kind: "up", server: to }, reason, requestedAt: H(hours), appliedAt: H(hours), evidence: [] })),
  ...[[5, "de", "l-fra", "failureMove"], [50, "l-fra", "de", "improvementMove"]].map(([hours, from, to, reason], i) => ({ id: `st${i}`, owner: { kind: "slot", slot: "stream" }, slots: ["stream"], intent: 1, from: { kind: "up", server: from }, to: { kind: "up", server: to }, reason, requestedAt: H(hours), appliedAt: H(hours), evidence: [] })),
  { id: "s2", owner: { kind: "slot", slot: "eu" }, slots: ["eu"], intent: 1, from: { kind: "down", reason: "unselected" }, to: { kind: "up", server: "nl" }, reason: "initialSelection", requestedAt: H(30), appliedAt: H(30), evidence: [] },
]

// Each server's own ping and reliability, a daily rhythm, noise that repeats for a given bucket, and Paris dying 3 days ago.
const FAKE = { de: [24, 0.99, 900], nl: [31, 0.985, 400], bg: [50, 0.97, 250], se: [100, 0.98, 120], us: [215, 0.985, 60], ro: [90, 0.8, 10], "l-ams": [15, 0.96, 60], "l-fra": [9, 0.98, 90], "l-par": [19, 0.9, 30] }
const noise = (seed) => { const x = Math.sin(seed * 12.9898) * 43758.5453; return x - Math.floor(x) }
function fakeAnalytics(from, to, seconds, lineages) {
  const out = []
  const step = seconds * 1000
  const all = [...acme, ...aurora].filter((l) => FAKE[l.id] && (lineages.length === 0 || lineages.includes(l.id)))
  for (let t = Math.ceil(from / step) * step; t < to; t += step) {
    const hour = new Date(t).getUTCHours()
    const evening = 1 + 0.35 * Math.max(0, Math.sin(((hour - 12) / 12) * Math.PI))
    all.forEach((l, i) => {
      const [ping, up, mb] = FAKE[l.id]
      const r = noise(t / step + i * 101)
      const dead = l.id === "l-par" && t > Date.now() - 3 * 864e5
      const attempts = 4
      const successes = dead ? 0 : r > up ? 2 : 4
      const pingNow = dead ? null : Math.round(ping * evening * (0.9 + 0.3 * r))
      out.push({
        start: new Date(t).toISOString(), source: l.source, lineage: l.id, country: l.country?.code ?? "", uploaded: Math.round(mb * 1e6 * r * 0.1 * seconds / 3600), downloaded: Math.round(mb * 1e6 * r * seconds / 3600),
        probeSuccesses: successes, probeAttempts: attempts, wanExcluded: 0, unknown: 0, pingMedianMs: pingNow, pingP95Ms: pingNow === null ? null : Math.round(pingNow * (1.3 + r)),
        newConnectionMedianMs: pingNow === null ? null : pingNow * 3, forcedSwitchAways: r > 0.9 && (dead || r > up) ? 1 : 0, managementTrafficIncluded: false,
      })
    })
  }
  return out
}

const refresh = (hours, outcome, rest = {}) => ({ at: H(hours), outcome, failureCode: null, wanExcluded: false, added: 0, changed: 0, missing: 0, ...rest })
const REFRESHES = new Map([
  ["acme", [refresh(0.5, "unchanged"), refresh(12.5, "updated", { changed: 1 }), refresh(24.5, "unchanged"), refresh(36.5, "failed", { failureCode: "timeout" }), refresh(48.5, "updated", { added: 2, missing: 1 }), refresh(60.5, "failed", { failureCode: "network", wanExcluded: true }), refresh(72.5, "unchanged")]],
  ["aurora", [refresh(0.2, "failed", { failureCode: "http_503" }), refresh(6.2, "failed", { failureCode: "http_503" }), refresh(12.2, "failed", { failureCode: "http_503" }), refresh(40, "updated", { changed: 2 })]],
])

function seed(daemon) {
  daemon.set({
    catalogue: [...acme, ...aurora],
    sources: new Map([["acme", ACME], ["aurora", AURORA]]),
    secrets: new Map([["acme", { url: "https://sub.acme.example/s/fake-token-3f9a1c7e" }], ["aurora", { url: "https://aurora.example/sub/fake-token-a81d" }]]),
    slots: new Map(SLOTS.map((s) => [s.id, s])),
    groups: new Map(GROUPS.map((g) => [g.id, g])),
    vpnClients: { known: true, limit: 8, foreign: [{ id: "n-office", name: "Office WireGuard", enabled: true }] },
    switches: SWITCHES,
    refreshes: REFRESHES,
    analytics: fakeAnalytics,
  })
}

process.env.BASE_PATH = BASE
const daemon = await startFakeDaemon({ port: PORT, dist: DIST, base: BASE })
daemon.reset(); seed(daemon)
const favicon = await readFile(new URL("../../public/favicon.svg", import.meta.url), "utf8").catch(() => "<svg xmlns='http://www.w3.org/2000/svg'/>")
const vite = await createServer({
  root: ROOT, base: BASE, clearScreen: false,
  server: { host: "0.0.0.0", port: 8438, strictPort: true, proxy: { [`${BASE}api`]: { target: `http://127.0.0.1:${PORT}`, changeOrigin: true } } },
  plugins: [{
    name: "lan-routes",
    configureServer: (server) => { server.middlewares.use((req, res, next) => {
      if (req.url === "/sidegate") { res.writeHead(308, { Location: "/sidegate/" }); return res.end() }
      if (req.url === "/favicon.svg") { res.writeHead(200, { "Content-Type": "image/svg+xml" }); return res.end(favicon) }
      next()
    }) },
  }],
})
await vite.listen()
console.log(`page on http://127.0.0.1:8438${BASE}`)
