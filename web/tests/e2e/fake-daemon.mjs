// A stand-in for the gateway daemon's HTTP API, serving a live build of the page. It enforces the contract in ../../API.md:
// CSRF and an Idempotency-Key on every mutation, JSON on one that has a body, a key replayed with the same request
// answered with its first response and refused with any other, If-Match on replace, If-None-Match: * on create, a 412
// that names the revision stored, and an SSE change feed. With `unifi` set to a token it also plays UniFi's login in front:
// see `unifiLogin`.
import fs from "node:fs"
import http from "node:http"
import path from "node:path"
import { checkKey, KEY_REUSED, rememberAnswer } from "./idempotency.mjs"

const H = (hours) => new Date(Date.now() - hours * 3600e3).toISOString()
const now = () => new Date().toISOString()

const NOT_SET = { not: false, values: [] }
const holds = (condition, value) => condition.values.length === 0 || condition.values.includes(value) !== condition.not

export const server = (id, label, cc, ping, rest = {}) => ({
  id, source: "aurora", label, name: { remarks: label, tag: null }, home: id, number: null, country: { code: cc, source: "rule" }, presence: "present", supported: true, udp: "passed",
  seen: [{ config: label, configKey: id, tag: "", position: 1 }],
  protocol: "vless", transport: "tcp", missingSince: null,
  health: { verdict: "succeeded", checkedAt: H(0.1), pingAt: ping === null ? null : H(0.1), current: true, pingP50Ms: ping, pingP95Ms: ping === null ? null : Math.round(ping * 1.4), newConnectionP50Ms: ping === null ? null : ping * 3, newConnectionP95Ms: ping === null ? null : ping * 5, score: ping === null ? null : Math.round(ping * 1.04), factor: 1.04, incidents7d: 0, seenDays: 5.2, seenWorking7dMs: 7 * 86_400_000, lastFailureAt: null, firstSeenAt: H(24 * 30) },
  ...rest,
})

/** Aurora over the default catalogue of three: by score Frankfurt, Amsterdam, Paris. */
export const source = (revision) => ({
  id: "aurora", revision, name: "Aurora", nameSet: true, firstChoice: "l-fra", order: ["l-fra", "l-ams", "l-par"], preferred: null, moves7d: 0,
  origin: {
    kind: "remote", userAgent: "Happ/3.0.0", refreshEverySeconds: 21600, paused: false,
    provider: {
      title: "Aurora", announcement: "", supportUrl: "", profileUrl: "", suggestedRefreshSeconds: null,
      quota: { upload: null, download: null, limit: null, expires: null, refills: null, observedAt: null },
    },
    fetch: { checkedAt: H(1), updatedAt: H(1), failures: 0, failureCode: null }, dueAt: H(-2),
  },
  feed: null, stored: 3, missing: 0, usedBy: [], removes: [], diagnostics: [], addedAt: H(24 * 30),
})

const ACME_FEED = JSON.parse(fs.readFileSync(new URL("../../../internal/catalogue/testdata/acme-shape.json", import.meta.url), "utf8"))
const FLAG = /[\u{1F1E6}-\u{1F1FF}]/gu
/** Go's PlainLabel: no flags, runs of space as one, and a label of nothing but flags kept. */
const plain = (label) => label.replace(FLAG, " ").split(/\s+/).filter(Boolean).join(" ") || label
/** Go's oneFlag: the one distinct flag a text carries, else null. */
const oneFlag = (text) => {
  const codes = new Set([...text.matchAll(/([\u{1F1E6}-\u{1F1FF}])([\u{1F1E6}-\u{1F1FF}])/gu)].map((m) => String.fromCharCode(m[1].codePointAt(0) - 0x1f1e6 + 65, m[2].codePointAt(0) - 0x1f1e6 + 65)))
  return codes.size === 1 ? [...codes][0] : null
}
/** Pings that rank the six servers named after a config in neither feed nor name order. */
const ACME_PING = { DE: 40, NL: 25, SE: 55, BG: 30, US: 120, RO: 35 }

const bytewise = (a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b))

/** Go's mostFrequent: the most frequent name, a tie to the bytewise least. */
const mostFrequent = (names) => {
  const counts = new Map()
  for (const n of names) counts.set(n, (counts.get(n) ?? 0) + 1)
  return [...counts].toSorted(([a, x], [b, y]) => y - x || bytewise(a, b))[0]?.[0]
}

/** Go's newName, as the API sends it: each part plain, and a part of flags alone no part. */
const newName = (remarks, tag) => {
  const part = (text) => (text && text.replace(FLAG, "").trim() ? plain(text) : null)
  return part(remarks) || part(tag) ? { remarks: part(remarks), tag: part(tag) } : null
}

/** Go's nameServers over one document, each server a list of { config, remarks, tag } places. A tag is a role when it sits on different
 * servers in different configs. A place is a home when its tag is a role, its config holds no other server, or its config is the only
 * one the server sits in. The home whose remarks, then tag, sort first names the server by the parts it has, which can be none, else
 * its most frequent tag alone. A part of flags alone is no part. Its country is the one flag its tags carry, else the home's remarks'. */
const nameServers = (places) => {
  const holds = new Map()
  const sittings = new Map()
  places.forEach((list, i) => list.forEach((p) => {
    holds.set(p.config, new Set([...(holds.get(p.config) ?? []), i]))
    if (!p.tag) return
    const s = sittings.get(p.tag) ?? { configs: new Set(), servers: new Set() }
    s.configs.add(p.config)
    s.servers.add(i)
    sittings.set(p.tag, s)
  }))
  const role = (tag) => (sittings.get(tag)?.configs.size ?? 0) > 1 && sittings.get(tag).servers.size > 1
  return places.map((list) => {
    const configs = new Set(list.map((p) => p.config))
    const tags = list.map((p) => p.tag).filter(Boolean)
    const home = list.filter((p) => role(p.tag) || holds.get(p.config).size === 1 || configs.size === 1)
      .toSorted((a, b) => bytewise(a.remarks, b.remarks) || bytewise(a.tag, b.tag))[0]
    const country = oneFlag(tags.join(" ")) ? { code: oneFlag(tags.join(" ")), source: "tagFlag" } : home && oneFlag(home.remarks) ? { code: oneFlag(home.remarks), source: "configFlag" } : { code: null, source: "unknown" }
    if (home) return { name: newName(home.remarks, home.tag), home: home.config, country }
    return { name: tags.length > 0 ? newName("", mostFrequent(tags)) : null, home: null, country }
  })
}

/** Go's configKeys for a first read: in document order, each config takes the least lineage ID among its roots that no earlier
 * config's key took. */
const configKeys = (configs) => {
  const taken = new Set()
  return configs.map((ids) => {
    const key = ids.toSorted(bytewise).find((id) => !taken.has(id))
    taken.add(key)
    return key
  })
}

/** The reference feed as Go sends it: one server per address, holding a place in every config it sits in, and each config keyed
 * as Go keys a new one. `proxy` is each config's place 1 and `EDGE-STANDBY` its place 2, and each config is home to the two it calls
 * by those roles, as Go names them. Labels and config names come without flags, as Go's PlainLabel sends them. */
export function acme(revision = 1) {
  const proxies = (c) => c.outbounds.filter((o) => o.settings?.vnext)
  const lineage = (o) => `lw-${o.settings.vnext[0].address.split(".")[0]}`
  const keys = configKeys(ACME_FEED.map((c) => proxies(c).map(lineage)))
  const places = new Map()
  ACME_FEED.forEach((c, i) => proxies(c).forEach((o, at) => {
    const address = o.settings.vnext[0].address
    const seen = { config: plain(c.remarks), configKey: keys[i], tag: o.tag, position: at + 1 }
    places.set(address, [...(places.get(address) ?? []), { seen, remarks: c.remarks }])
  }))
  const named = nameServers([...places.values()].map((sightings) => sightings.map((x) => ({ config: x.seen.configKey, remarks: x.remarks, tag: x.seen.tag }))))
  const servers = [...places].map(([address, sightings], i) => {
    const { name, home, country } = named[i]
    const label = [name?.remarks, name?.tag].filter(Boolean).join(" · ") || "Server"
    const seen = sightings.map((x) => x.seen)
    return server(`lw-${address.split(".")[0]}`, label, country.code, name?.tag === "proxy" ? ACME_PING[country.code] : ACME_PING[country.code] + 5, {
      source: "acme", protocol: "vless", transport: "xhttp", seen, name, home, number: null, country,
    })
  })
  const feed = {
    configs: ACME_FEED.map((c, i) => ({
      key: keys[i], place: i + 1, name: plain(c.remarks), servers: proxies(c).length,
      skipped: c.outbounds.flatMap((o, at) => (o.settings?.vnext ? [] : [{ reason: "helper", index: at + 1, tag: o.tag, protocol: o.protocol }])),
    })),
  }
  const sub = { ...source(revision), id: "acme", name: "Acme", firstChoice: null, order: [], feed, stored: servers.length }
  return { servers, source: sub }
}

const PROTOCOLS = ["vless", "vmess", "trojan", "shadowsocks", "socks", "http", "hysteria2"]
/** As RE2 reads a pattern: JavaScript's lookaround and backreferences don't compile there. */
/** What Go says of a pattern it can't compile, cut to its cause as the daemon cuts it, for the patterns the checks type; null for one
 * it reads. */
function patternCause(p) {
  if (/\(\?[=!]/.test(p)) return "lookahead not supported"
  if (/\(\?<[=!]/.test(p)) return "lookbehind not supported"
  if (/\\[1-9]/.test(p)) return "invalid escape"
  try { new RegExp(p) } catch (e) {
    const said = String(e.message)
    return /Unterminated character class/.test(said) ? "missing closing ]" : /Unterminated group/.test(said) ? "missing closing )"
      : /Unmatched '\)'/.test(said) ? "unexpected )" : /Nothing to repeat/.test(said) ? "nothing to repeat" : "unsupported syntax"
  }
  return null
}
/** As Go's encoding/json reads `value` into the type `shape` names: null leaves a value its zero and a pointer or a list
 * nil, which is null here; a field it doesn't know or a type that doesn't fit is no request of its form, undefined. A shape is
 * "string", "bool", "int", "float", "*string", a one-item array for a list, or an object of fields. */
const ZERO = { string: "", bool: false, int: 0, float: 0 }
const goDecode = (shape, value) => {
  if (shape === "*string") return value === null || typeof value === "string" ? value : undefined
  if (typeof shape === "string") {
    if (value === null) return ZERO[shape]
    return typeof value === typeof ZERO[shape] && (shape !== "int" || Number.isInteger(value)) ? value : undefined
  }
  if (Array.isArray(shape)) {
    if (value === null) return null
    if (!Array.isArray(value)) return undefined
    const items = value.map((item) => goDecode(shape[0], item))
    return items.includes(undefined) ? undefined : items
  }
  if (value !== null && (typeof value !== "object" || Array.isArray(value))) return undefined
  const given = value ?? {}
  if (Object.keys(given).some((key) => !(key in shape))) return undefined
  const out = {}
  for (const [key, field] of Object.entries(shape)) {
    out[key] = goDecode(field, key in given ? given[key] : null)
    if (out[key] === undefined) return undefined
  }
  return out
}
const AMONG = { not: "bool", values: ["string"] }
const UNFORMED = { status: 400, kind: "validation", reason: "malformed", message: "That request isn't in the expected form." }
const NO_RELEASES = { status: 422, kind: "unsupported", reason: "noReleases", message: "This build takes no updates." }
const SLOT = { name: "string", exit: { kind: "string", server: "string", group: "string", source: "string" } }
const SOURCE = { name: "string", origin: { kind: "string", url: "*string", userAgent: "string", refreshEverySeconds: "float", paused: "bool", text: "*string" } }
const MEMBERS = { sources: AMONG, countries: AMONG, protocols: AMONG, include: "string", exclude: "string", box: "*string" }
/** A rule as Go's decoder leaves it, a nil list as an empty one, or undefined. */
const decodeMembers = (members) => members && { ...members, ...Object.fromEntries(["sources", "countries", "protocols"].map((k) => [k, { ...members[k], values: members[k].values ?? [] }])) }
/** Why Go wouldn't admit a rule, as its refusal, or null. */
function membersRefusal(m) {
  const cause = [m.include, m.exclude].map((p) => patternCause(p ?? "")).find((c) => c !== null)
  if (cause) return { status: 422, kind: "validation", reason: "badPattern", detail: cause, message: `Include pattern: error parsing regexp: ${cause}.` }
  const malformed = ![undefined, null, "ticked", "unticked"].includes(m?.box) || m.countries.values.some((c) => c.length !== 2) || m.protocols.values.some((p) => !PROTOCOLS.includes(p))
  return malformed ? { status: 422, kind: "validation", reason: "malformed", message: "Those members can't be used." } : null
}

/** Stands in for Go: conditions match nothing until one is set, the box included. Is none of nothing is set. */
const isSet = (c) => c.not || c.values.length > 0
const conditioned = (m) => [m.sources, m.countries, m.protocols].some(isSet) || Boolean(m.include || m.exclude || m.box)
/** How the fake reads a name pattern, where RE2 and JavaScript agree. A check that needs them to differ sets `reads`. */
const readsLikeJs = (pattern, name) => new RegExp(pattern.replace(/^\(\?i\)/, ""), "i").test(name)
/** What every set condition but the box matches: present servers. */
const matchedBy = (m, catalogue, reads = readsLikeJs) => {
  if (!conditioned(m)) return []
  const hits = (re, l) => [l.label, ...l.seen.map((seen) => seen.tag)].some((name) => reads(re, name))
  return catalogue.filter((l) => l.presence === "present" && holds(m.sources, l.source) && holds(m.countries, l.country?.code ?? "") && holds(m.protocols, l.protocol) && (!m.include || hits(m.include, l)) && (!m.exclude || !hits(m.exclude, l))).map((l) => l.id)
}
/** The membership rule: what the conditions but the box match, that the box holds for its tick. */
const resolvedBy = (m, ticked, catalogue, reads) => matchedBy(m, catalogue, reads).filter((id) => !m.box || ticked.includes(id) === (m.box === "ticked"))

/** The fake reads a pasted list as Go does for what the contract sends: a line with a scheme is a server. */
const pasted = (text) => /:\/\//.test(text ?? "")

/** Stands in for Go's Exit admission: one arm, and only that arm's field. */
const EXIT_FIELDS = { server: ["server"], group: ["group"], source: ["source"], direct: [], block: [] }
const badExit = (exit) => !exit || !EXIT_FIELDS[exit.kind] || Object.keys(exit).some((k) => k !== "kind" && !EXIT_FIELDS[exit.kind].includes(k)) || EXIT_FIELDS[exit.kind].some((k) => !exit[k])

const running = ({ exit }) => (exit.kind === "direct"
  ? { status: "direct", downReason: null, server: null, appliedServer: null, applied: { kind: "direct" } }
  : exit.kind === "block" ? { status: "block", downReason: null, server: null, appliedServer: null, applied: { kind: "block" } }
  : { status: "serving", downReason: null, server: exit.server ?? "l-ams", appliedServer: exit.server ?? "l-ams", applied: { kind: "up", server: exit.server ?? "l-ams" } })

export const slotView = (id, version, input) => {
  const { applied, ...now } = running(input)
  return {
    id, version, createdAt: new Date().toISOString(), ...input, ...now, leavingBy: null, pending: false,
    runtime: { phase: "ready", applied, networkId: "n1", networkEstablished: true, errorCode: null },
    unifi: { name: input.name, interface: "pxa1", known: true, client: "present", trouble: null }, backups: ["l-fra"],
  }
}

/** A slot after a change: what UniFi holds for it stays, as it does on the gateway. */
const revised = (existing, id, version, input) => {
  const next = slotView(id, version, input)
  if (existing) next.createdAt = existing.createdAt
  return existing ? { ...next, runtime: { ...existing.runtime, applied: next.runtime.applied }, unifi: { ...existing.unifi, name: input.name } } : next
}

const AURORA_GROUP = "all-aurora"

const slotInput = (name) => ({ name, exit: { kind: "group", group: AURORA_GROUP } })

/** Stands in for Go's order: by score, lowest first, a server with no score last. */
export const ranked = (ids, catalogue) => ids.toSorted((a, b) => (catalogue.find((l) => l.id === a)?.health.score ?? Infinity) - (catalogue.find((l) => l.id === b)?.health.score ?? Infinity))

/** A source as Go sends it whenever its servers may have changed: `order` its present servers Xray can run by rank,
 * `firstChoice` rank 1. */
const ordered = (sub, catalogue) => {
  const present = catalogue.filter((l) => l.source === sub.id && l.presence === "present")
  const order = ranked(present.filter((l) => l.supported).map((l) => l.id), catalogue)
  return { ...sub, order, firstChoice: order[0] ?? null }
}

/** The default catalogue: by score Frankfurt, Amsterdam, Paris, so a group's order is neither config nor name order. */
const auroraServers = () => [server("l-ams", "Amsterdam", "NL", 15), server("l-fra", "Frankfurt", "DE", 9), server("l-par", "Paris", "FR", 19)]

/** A group as Go sends it, over `catalogue`: `resolved` what its rule holds there with its `ticked`, `order` those by rank,
 * `firstChoice` rank 1. */
export const groupView = (id, revision, input, usedBy = [], catalogue = auroraServers(), reads = undefined) => {
  const ticked = (input.ticked ?? []).toSorted()
  const members = { ...input.members, box: input.members.box ?? null }
  const resolved = resolvedBy(members, ticked, catalogue, reads)
  const order = ranked(resolved, catalogue)
  return { id, revision, name: input.name, members, ticked, resolved, usedBy, moves7d: 0, firstChoice: order[0] ?? null, order, preferred: null }
}

const auroraGroup = (usedBy) => groupView(AURORA_GROUP, 1, { name: "All Aurora", members: { sources: { not: false, values: ["aurora"] }, countries: NOT_SET, protocols: NOT_SET, include: "", exclude: "", box: null } }, usedBy)

const SETTINGS = { analyticsRetentionDays: 30, checkForUpdates: true }
/** The release's Xray, which the fake runs unless a check says an older one still does. */
export const XRAY = "26.10.31"

const GB = 1024 ** 3
const SIX_HOURS = 21600e3

/** The daemon's analytics step: it keeps 15-minute steps and sums them into the buckets a read asks for. */
export const STEP_MS = 900e3

/** Analytics steps of 15 minutes, as the daemon keeps them, summed into the read's buckets: counts and seconds add up, and each
 * ping is the mean of the steps that have one. Each step is a bucket's fields with a `start` in ms. */
export function inBuckets(steps, from, to, bucketSeconds, lineages = [], sources = []) {
  const size = bucketSeconds * 1000
  const buckets = new Map()
  for (const step of steps) {
    if (step.start < from || step.start >= to || (lineages.length > 0 && !lineages.includes(step.lineage)) || (sources.length > 0 && !sources.includes(step.source))) continue
    const start = Math.floor(step.start / size) * size
    const key = `${step.lineage}/${start}`
    const b = buckets.get(key) ?? { ...step, start, pings: [], p95s: [], forcedSwitchAways: 0, servedSeconds: 0, incidents: 0 }
    Object.assign(b, { forcedSwitchAways: b.forcedSwitchAways + step.forcedSwitchAways, servedSeconds: b.servedSeconds + step.servedSeconds, incidents: b.incidents + step.incidents })
    if (step.pingMedianMs !== null) b.pings.push(step.pingMedianMs)
    if (step.pingP95Ms !== null) b.p95s.push(step.pingP95Ms)
    buckets.set(key, b)
  }
  const mean = (xs) => (xs.length ? Math.round(xs.reduce((a, x) => a + x, 0) / xs.length) : null)
  return [...buckets.values()].map(({ pings, p95s, start, ...b }) => ({ ...b, start: new Date(start).toISOString(), pingMedianMs: mean(pings), pingP95Ms: mean(p95s) }))
}
const down = (s) => ({ ...s, health: { ...s.health, verdict: "failed" } })
const refused = (index, tag, position, code, field) => ({ reason: "refused", index, tag, position, code, field })

/** Servers in each state the Stability column draws, as Aurora's, each with the state it stands for and the daemon's factor for
 * it, which sets the bars. */
export function failureStates(at = Date.now()) {
  const H = (hours) => new Date(at - hours * 3600e3).toISOString()
  const D = (days) => H(24 * days)
  const base = server("x", "x", null, 40).health
  const quiet = { pingAt: null, pingP50Ms: null, pingP95Ms: null, newConnectionP50Ms: null, newConnectionP95Ms: null, score: null }
  const row = (id, label, cc, ping, state, health) => ({
    state, server: server(id, label, cc, ping, { health: { ...base, ...(ping === null ? quiet : server(id, label, cc, ping).health), ...health } }),
  })
  return [
    row("f-de", "Germany · proxy", "DE", 40, "no failure all week", { factor: 1.04 }),
    row("f-nl", "Netherlands · proxy", "NL", 25, "once all week", { factor: 1.1, incidents7d: 1, lastFailureAt: D(2) }),
    row("f-nl2", "Netherlands · proxy-2", "NL", 27, "twice all week", { factor: 1.2, incidents7d: 2, lastFailureAt: D(4) }),
    row("f-fi", "Finland · proxy", "FI", 38, "four times all week", { factor: 1.5, incidents7d: 4, lastFailureAt: D(1) }),
    row("f-fi2", "Finland · proxy-2", "FI", 44, "nine times all week", { factor: 1.9, incidents7d: 9, lastFailureAt: H(30) }),
    row("f-se", "Sweden · proxy", "SE", 51, "23 times all week", { factor: 3, incidents7d: 23, lastFailureAt: H(2) }),
    row("f-pl", "Poland · proxy", "PL", 33, "new, seen working 7 hours", { factor: 1.2, seenWorking7dMs: 7 * 3600e3, firstSeenAt: H(7.5) }),
    row("f-lt", "Lithuania · proxy", "LT", 36, "new, once in 12 hours", { factor: 1.6, incidents7d: 1, seenWorking7dMs: 12 * 3600e3, firstSeenAt: H(13), lastFailureAt: H(3) }),
    row("f-lv", "Latvia · proxy", "LV", 44, "new, three times in 3 days", { factor: 1.4, incidents7d: 3, seenWorking7dMs: 3 * 864e5, firstSeenAt: D(3.2), lastFailureAt: D(1) }),
    row("f-is", "Iceland · proxy", "IS", 48, "down five days of the week", { factor: 1.3, incidents7d: 1, seenWorking7dMs: 2 * 864e5, firstSeenAt: D(60), lastFailureAt: D(5.1) }),
    row("f-no", "Norway · proxy", "NO", null, "down all week", { factor: 2.5, verdict: "failed", seenWorking7dMs: 0, firstSeenAt: D(60), lastFailureAt: D(11) }),
    row("f-ee", "Estonia · proxy", "EE", null, "never checked", { factor: 1.25, verdict: "unknown", checkedAt: null, current: false, seenWorking7dMs: null, firstSeenAt: H(1) }),
    row("f-ie", "Ireland · proxy", "IE", null, "checked before, not this week", { factor: 1.25, verdict: "unknown", checkedAt: D(9), current: false, seenWorking7dMs: 0, firstSeenAt: D(60), lastFailureAt: D(20) }),
    row("f-dk", "Denmark · proxy", "DK", 70, "over 999", { factor: 8, incidents7d: 1200, lastFailureAt: H(0.2) }),
    row("f-cz", "Czechia · proxy", "CZ", 55, "listed all week, seen working 20 hours", { factor: 1.15, seenWorking7dMs: 20 * 3600e3, firstSeenAt: D(40) }),
    row("f-at", "Austria · proxy", "AT", 58, "not kept for the week yet", { factor: 1.3, incidents7d: 2, seenWorking7dMs: null, lastFailureAt: D(3) }),
  ]
}

/** A source in each state its heading draws, its servers, its week and its refreshes. `week` lists, for days ago from 1 to 6,
 * whether it was in use and its failovers: [daysAgo, used, failovers]. Today is left out, since its first six-hour step may not have
 * begun. */
export function troubledSources(at = Date.now()) {
  const H = (hours) => new Date(at - hours * 3600e3).toISOString()
  const midnight = (daysAgo) => { const d = new Date(at); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() - daysAgo); return d.getTime() }
  /* A six-hour step that starts on the day, so the week counts it there. */
  const stepOn = (daysAgo) => Math.ceil(midnight(daysAgo) / SIX_HOURS) * SIX_HOURS
  const remote = (id, name, { quota = {}, fetch = {}, announcement = "", links = false, ...rest } = {}) => {
    const base = source(1)
    return {
      ...base, id, name, nameSet: true, firstChoice: null, order: [], stored: 0, addedAt: H(24 * 60), ...rest,
      origin: {
        ...base.origin, dueAt: H(-3),
        provider: {
          ...base.origin.provider, title: name, announcement, supportUrl: links ? "https://support.example/help" : "", profileUrl: links ? "https://panel.example/me" : "",
          quota: { upload: null, download: null, limit: null, expires: null, refills: null, observedAt: H(1), ...quota },
        },
        fetch: { checkedAt: H(0.4), updatedAt: H(26), failures: 0, failureCode: null, ...fetch },
      },
    }
  }
  /** A source's servers, each its own config's unless `config` names the config it shares with its `tag`. */
  const on = (id, list) => list.map(({ config, tag = "", ...s }, i) => {
    const home = `${id}-${config ?? i}`
    return { ...s, id: `${id}-${i}`, source: id, home, name: { remarks: s.label, tag: tag || null }, label: tag ? `${s.label} · ${tag}` : s.label, seen: [{ config: s.label, configKey: home, tag, position: tag === "backup" ? 2 : 1 }] }
  })
  const three = () => [server("x", "Amsterdam", "NL", 15), server("x", "Frankfurt", "DE", 9), server("x", "Paris", "FR", 19)]
  const states = [
    { source: remote("steady", "Steady", { links: true, announcement: "Maintenance on Sunday night, 02:00 to 04:00 UTC. Servers in Frankfurt may drop for a few minutes.",
      quota: { upload: 2 * GB, download: 18 * GB, limit: 100 * GB, expires: H(-24 * 40), refills: H(-24 * 10) },
      feed: { configs: [{ key: "steady-0", place: 1, name: "Amsterdam", servers: 1, skipped: [] }, { key: "steady-1", place: 2, name: "Frankfurt", servers: 2, skipped: [refused(3, "wg", 3, "unsupported_protocol", "wireguard")] },
        { key: null, place: 3, name: "Warsaw", servers: 0, skipped: [refused(1, "proxy", 1, "insecure_tls", "allowInsecure")] }] },
      diagnostics: [{ entry: 3, code: "unsupported_protocol", field: "wireguard" }, { entry: 4, code: "insecure_tls", field: "allowInsecure" }, { entry: 9, code: "malformed_entry", field: "" }], moves7d: 2 }),
      servers: [server("x", "Amsterdam", "NL", 15), server("x", "Frankfurt", "DE", 9, { config: 1, tag: "proxy" }), server("x", "Frankfurt", "DE", 11, { config: 1, tag: "backup" })], week: [[1, true, 1], [2, true, 1], [3, true, 0], [4, true, 0], [5, true, 0]] },
    { source: remote("soon", "Soon", { quota: { upload: 1 * GB, download: 94 * GB, limit: 100 * GB, expires: H(-24 * 3) }, fetch: { failures: 3, failureCode: "http_503", checkedAt: H(0.2) }, moves7d: 9 }),
      servers: [three()[0], down(three()[1]), down(three()[2])], week: [[1, true, 3], [2, true, 2], [3, true, 4], [4, true, 0], [5, true, 0], [6, true, 0]] },
    { source: remote("expired", "Northwind Networks Premium Unlimited (Europe, North America and Asia-Pacific), Family Plan with six devices, renewed every year", {
      quota: { download: 200 * GB, limit: 100 * GB, expires: H(48) }, moves7d: 25 }),
      servers: three().map(down), week: [[1, true, 7], [2, true, 9], [3, true, 4], [4, true, 5]] },
    { source: remote("limit", "Limit", { quota: { upload: 10 * GB, download: 95 * GB, limit: 100 * GB, expires: H(-24 * 30), refills: H(-24 * 6), observedAt: H(24 * 3) }, fetch: { failures: 1, failureCode: "http_404", checkedAt: H(0.1) } }),
      servers: three(), week: [[1, true, 0], [3, true, 0]] },
    { source: remote("stale", "Stale", { quota: { upload: 10 * GB, download: 90 * GB, limit: 100 * GB, expires: H(-24 * 40), refills: H(24), observedAt: H(72) } }),
      servers: three(), week: [[1, true, 0], [2, true, 0], [3, true, 0], [4, true, 0], [5, true, 0], [6, true, 0]] },
    { source: remote("unlimited", "Unlimited", { quota: { download: 12 * GB, limit: 0, expires: H(-24 * 800), observedAt: H(50) }, addedAt: H(48) }),
      servers: three(), week: [[1, true, 0]] },
    { source: remote("unreported", "Unreported", { quota: { observedAt: null }, addedAt: new Date(Math.max(midnight(0), at - 3600e3)).toISOString() }), servers: three(), week: [] },
    { source: remote("new", "New", { fetch: { checkedAt: null, updatedAt: null }, quota: { observedAt: null } }), servers: [], week: [] },
    { source: remote("broken", "Broken", { fetch: { checkedAt: H(0.1), updatedAt: null, failures: 2, failureCode: "timeout" }, quota: { observedAt: null } }), servers: [], week: [] },
    { source: { ...source(1), id: "pasted", name: "Pasted", nameSet: true, origin: { kind: "manual" }, firstChoice: null, order: [], stored: 0, addedAt: H(24 * 9) }, servers: [], week: [] },
  ]
  const servers = states.flatMap((st) => on(st.source.id, st.servers))
  const refreshes = new Map(states.map(({ source: sub }) => {
    const failing = sub.origin.kind === "remote" ? sub.origin.fetch.failures : 0
    const failed = (i) => ({ at: H(i + 0.1), outcome: "failed", failureCode: sub.origin.fetch.failureCode, wanExcluded: false, added: 0, changed: 0, missing: 0 })
    return [sub.id, [...Array.from({ length: failing }, (_, i) => failed(i)), ...(sub.origin.kind === "remote" && sub.origin.fetch.updatedAt ? [{ at: H(6), outcome: "unchanged", failureCode: null, wanExcluded: false, added: 0, changed: 0, missing: 0 }] : [])]]
  }))
  const buckets = states.flatMap(({ source: sub, week }) => week.flatMap(([daysAgo, used, failovers]) => {
    const lineage = servers.find((s) => s.source === sub.id)?.id
    if (!lineage) return []
    const start = stepOn(daysAgo)
    const half = Math.ceil(failovers / 2)
    return [
      { start, servedSeconds: used ? 3 * 3600 : 0, forced: half },
      { start: start + SIX_HOURS, servedSeconds: used ? 3600 : 0, forced: failovers - half },
    ].filter((b) => b.start <= at).map((b) => ({
      start: new Date(b.start).toISOString(), source: sub.id, lineage, country: "NL", uploaded: 0, downloaded: 0, probeSuccesses: 0, probeAttempts: 0, wanExcluded: 0, unknown: 0,
      pingMedianMs: null, pingP95Ms: null, newConnectionMedianMs: null, forcedSwitchAways: b.forced, servedSeconds: b.servedSeconds, incidents: b.forced, managementTrafficIncluded: false,
    }))
  }))
  const analytics = (from, to, _bucket, lineages = [], sources = []) => buckets.filter((b) => Date.parse(b.start) >= from && Date.parse(b.start) < to
    && (lineages.length === 0 || lineages.includes(b.lineage)) && (sources.length === 0 || sources.includes(b.source)))
  return { sources: states.map((st) => ({ ...st.source, stored: st.servers.length })), servers, refreshes, analytics, ids: states.map((st) => st.source.id) }
}

const TYPES = { ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".woff2": "font/woff2", ".html": "text/html" }

const noVpnClients = { known: false, limit: 8, foreign: [] }

/** The statuses nginx's add_header acts on, with no `always`. */
const HANDED_OUT = [200, 201, 204, 206, 301, 302, 303, 304, 307, 308]
export const UNIFI_FORBIDDEN = JSON.stringify({ error: { code: 403, message: "Forbidden" } })

/** UniFi's login and nginx behavior as measured on a gateway: a PUT, POST or DELETE whose X-Csrf-Token isn't
 * the session's `token` gets UniFi's 403 before the daemon. An answer to a request that carried none hands the token out
 * in X-Csrf-Token. The hook clears X-Csrf-Token, so the daemon never sees it. Returns false once it has answered. */
export function unifiLogin(token, req, res) {
  const provided = req.headers["x-csrf-token"]
  delete req.headers["x-csrf-token"]
  if (provided !== token && !["GET", "HEAD", "OPTIONS"].includes(req.method)) {
    res.writeHead(403, { "Content-Type": "application/json" })
    res.end(UNIFI_FORBIDDEN)
    return false
  }
  if (provided === undefined) {
    const writeHead = res.writeHead.bind(res)
    res.writeHead = (code, headers = {}) => writeHead(code, HANDED_OUT.includes(code) ? { ...headers, "X-Csrf-Token": token } : headers)
  }
  return true
}

/** The routes that take the CSRF check and no key, so their answers are never replayed and name no revision. */
const UNKEYED = ["api/groups/preview", "api/update/check", "api/update"]

const RELEASE = /^v(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})$/
/** Go's releaseVersion.above. */
const above = (a, b) => {
  const [x, y] = [a, b].map((v) => RELEASE.exec(v)?.slice(1).map(Number))
  const at = x && y ? x.findIndex((n, i) => n !== y[i]) : -1
  return at >= 0 && x[at] > y[at]
}
const RELEASES = "https://example.test/OWNER/sidegate/releases"
const DAY = 24 * 3600e3

export function startFakeDaemon({ port, dist, base }) {
  let s
  const sse = new Set()
  const reset = () => {
    for (const hold of Object.values(s?.readHolds ?? {})) hold.release()
    for (const res of sse) res.destroy()
    sse.clear()
    const catalogue = auroraServers()
    s = {
      token: "token-1", stopped: false, sessionFails: 0, writeFails: 0, eventsFail: 0, badShape: false, trimNames: false, cursor: 100, revision: 41,
      catalogue,
      slots: new Map([["eu", slotView("eu", 3, slotInput("EU"))]]),
      groups: new Map([[AURORA_GROUP, auroraGroup(["eu"])]]),
      writes: new Map(),
      deletedAt: new Map(),
      sources: new Map([["aurora", ordered(source(1), catalogue)]]),
      secrets: new Map([["aurora", { url: "https://aurora.example/sub" }]]),
      settings: { ...SETTINGS, revision: 5 },
      referenced: new Set(),
      switches: [],
      /** GET /api/replaced: servers a refresh kept across a changed connection. */
      replaced: [],
      wan: "up",
      vpnClients: noVpnClients,
      unifiTrouble: null,
      /** When UniFi last answered the key, or null since a start until it does. */
      unifiAcceptedAt: new Date(Date.now() - 60e3).toISOString(),
      /** How UniFi answers a try of a key: null takes it, `keyRefused` or `notAnswering` refuses it. */
      unifiKeyAnswer: null,
      core: { version: XRAY, running: XRAY },
      /** The build api/session names: a release's tag and content name, or a checkout's "dev-…" and null. */
      version: "v0.1.0",
      release: "0123456789ab",
      /** The release the releases address has marked Latest, or null for a 404, and whether it carries another core. */
      latest: "v0.1.0",
      latestRestartsCore: false,
      /** Why a check learns nothing, when that is no 404, or null. */
      checkFails: null,
      /** What the updater keeps: when the last check began, api/update's `check`, `running`, with when it was pressed, and `last`, and the release a check found. */
      update: { startedAt: null, check: { at: null, failure: null }, found: null, running: null, pressedAt: null, last: null },
      /** Milliseconds a GET of a path waits before it is answered, so a check can see what the page shows meanwhile. */
      /** UniFi's CSRF token while the fake plays UniFi's login in front of it, or null. */
      unifi: null,
      readDelay: {},
      /** The GETs of a path wait on `held` before they are answered, from `holdReads` until its release. */
      readHolds: {},
      /** Whether an Exit write's move waits for `readback` before the runtime shows it. */
      holdReadback: false,
      /** What a press's Test answers: `succeeded`, `failed` or `unknown`. Only a pass moves anything. */
      preferVerdict: "succeeded",
      /** Whether a passing press's move waits for `readback` before the runtime shows it. As Go's, the answer's revision has it
       * staged either way: its slots name the server. */
      holdPreferLanding: false,
      /** How many more GETs of a path answer 502, as the gateway's nginx does while the daemon won't answer it. */
      readFails: {},
      /** Whether reads of the analytics fail, as a daemon whose metrics store won't answer. */
      analyticsFails: false,
      /** How the daemon reads a name pattern, or undefined for JavaScript's reading. */
      reads: undefined,
      log: [],
    }
  }
  reset()

  /** Which slots use each group, as the daemon's view derives it. A source's are its deletion's, read with the view. */
  const refreshUsedBy = () => {
    const using = (id) => [...s.slots.values()].filter((slot) => slot.exit.kind === "group" && slot.exit.group === id).map((slot) => slot.id)
    for (const [id, g] of s.groups) s.groups.set(id, { ...g, usedBy: using(id) })
  }

  /** What deleting a source does, as Go's sourceDeletion: its servers go, the groups it would leave with no source or no tick
   * go with it, and every slot whose Exit is the source, one of its servers or one of those groups refuses it. */
  const deletionOf = (id) => {
    const serversGo = s.catalogue.filter((l) => l.source === id).map((l) => l.id)
    const ticksAlone = (m) => m.box === "ticked" && !conditioned({ ...m, box: null })
    const goes = [...s.groups.values()].filter((g) => (!g.members.sources.not && g.members.sources.values.length === 1 && g.members.sources.values[0] === id)
      || (ticksAlone(g.members) && g.ticked.length > 0 && g.ticked.every((l) => serversGo.includes(l))))
    const using = [...s.slots.values()].filter((slot) => (slot.exit?.kind === "group" && goes.some((g) => g.id === slot.exit.group)) || (slot.exit?.kind === "source" && slot.exit.source === id) || (slot.exit?.kind === "server" && serversGo.includes(slot.exit.server)))
    return { serversGo, goes, using }
  }
  /* As Go's: a preference is named only while a box of its owner draws that server applied: a serving slot whose move isn't
   * pending, on the server it reports. */
  /** As Go's DeleteSource, for the route and for another writer: refused while a slot uses what it removes, and else the
   * groups it leaves go and its servers' ticks with it. The refusal, or null once it is done. */
  const removeSource = (id) => {
    const existing = s.sources.get(id)
    if (!existing) return { status: 404, kind: "validation", reason: "sourceGone", message: "That source is gone." }
    const { serversGo, goes, using } = deletionOf(id)
    if (using.length) return { status: 409, kind: "referenced", reason: "inUse", message: `Used by ${using.map((slot) => slot.name).join(", ")}.` }
    for (const g of goes) {
      s.deletedAt.set(`group/${g.id}`, g.revision)
      s.groups.delete(g.id)
    }
    for (const g of s.groups.values()) {
      if (g.ticked.some((l) => serversGo.includes(l))) s.groups.set(g.id, { ...g, revision: g.revision + 1, ticked: g.ticked.filter((l) => !serversGo.includes(l)) })
    }
    s.catalogue = s.catalogue.map((l) => (l.source === id ? { ...l, presence: "tombstone" } : l))
    resolveGroups()
    s.deletedAt.set(id, existing.revision)
    s.sources.delete(id)
    s.secrets.delete(id)
    emit("source", { source: id })
    return null
  }
  /** Every group's members over the catalogue as it is now, as Go's view resolves them on each publish. */
  const resolveGroups = () => {
    for (const [id, g] of s.groups) {
      const { resolved, order, firstChoice } = groupView(id, g.revision, g, g.usedBy, s.catalogue, s.reads)
      s.groups.set(id, { ...g, resolved, order, firstChoice })
    }
  }
  /* The server a box draws, as slots.ts's drawnServer reads it: the runtime's, or after a pass that vouched for none, the slot's. */
  const drawn = (slot) => slot.appliedServer ?? (slot.runtime.phase === "unknown" && !slot.pending ? slot.server : null)
  const shownPreferred = (follows, preferred) =>
    preferred && [...s.slots.values()].some((slot) => follows(slot.exit) && slot.status === "serving" && !slot.pending && drawn(slot) === slot.server && drawn(slot) === preferred.server) ? preferred : null
  const sourceView = (sub) => {
    const { goes, using } = deletionOf(sub.id)
    return { ...sub, usedBy: using.map((slot) => slot.id), removes: goes.map((g) => g.id), preferred: shownPreferred((e) => e.kind === "source" && e.source === sub.id, sub.preferred ?? null) }
  }
  const groupOut = (g) => ({ ...g, preferred: shownPreferred((e) => e.kind === "group" && e.group === g.id, g.preferred ?? null) })

  const emit = (kind, ids = {}) => {
    s.revision++
    announce(kind, ids)
  }
  /** An event with no command behind it, which leaves the view's revision. */
  const announce = (kind, ids = {}) => {
    const event = { id: String(++s.cursor), at: now(), revision: s.revision, kind, slot: ids.slot ?? null, source: ids.source ?? null, resync: false }
    for (const res of sse) res.write(`id: ${event.id}\ndata: ${JSON.stringify(event)}\n\n`)
  }

  const view = () => ({
    revision: s.revision, generatedAt: now(), slots: [...s.slots.values()], sources: [...s.sources.values()].map(sourceView), groups: [...s.groups.values()].map(groupOut),
    wan: s.badShape ? "degraded" : s.wan, core: s.core, unifi: { acceptedAt: s.unifiAcceptedAt }, unifiTrouble: s.unifiTrouble,
    /* A row a test writes without a trouble has none. */
    vpnClients: { ...s.vpnClients, foreign: s.vpnClients.foreign.map((f) => ({ trouble: null, ...f })) },
  })

  /** As Go's: the last update, pressed no earlier than the check, found another release the latest than the one it offers. */
  const superseded = () => {
    const { found, last, startedAt } = s.update
    return last?.reason === "offerChanged" && last.version === found?.version && !(Date.parse(startedAt) > Date.parse(last.at))
  }
  /** As Go's UpdateView: a development build has no check and no update, and only a newer release is offered, unless the last
   * update found another one the latest. */
  const updateView = () => {
    const bare = { version: s.version, releases: false, guide: null, check: { at: null, failure: null }, offered: null, running: null, last: null }
    if (!RELEASE.test(s.version) || s.release === null) return bare
    const { check, found, running, last } = s.update
    const offered = found && above(found.version, s.version) && !superseded() ? { ...found, notes: `${RELEASES}/tag/${found.version}` } : null
    return { ...bare, releases: true, guide: RELEASES.replace(/releases$/, `blob/${s.version}/docs/operate.md`), check, offered, running, last: running ? null : last }
  }
  /** As Go's: an automatic check is due a day after the last one began, whatever came of it, or once the last update found
   * another release the latest. A failure keeps what was found. */
  const checkForUpdate = (auto) => {
    const due = s.update.startedAt === null || Date.now() - Date.parse(s.update.startedAt) >= DAY || superseded()
    if (auto && (!s.settings.checkForUpdates || !due)) return
    const failure = s.checkFails ?? (s.latest === null ? "filesUnavailable" : null)
    const found = failure ? s.update.found : { version: s.latest, restartsCore: s.latestRestartsCore }
    s.update = { ...s.update, startedAt: now(), check: { at: now(), failure }, found }
  }

  /** A versioned PUT: create with If-None-Match: *, replace with If-Match, nothing else. A refusal names the revision
   * stored at that moment, so the page never reads it later, after a third writer may have moved it on. */
  function precondition(req, existing, gone) {
    const create = req.headers["if-none-match"]
    const match = req.headers["if-match"]
    const stored = existing && (existing.version ?? existing.revision)
    const refused = (message) => ({ status: 412, kind: "staleRevision", reason: "stale", message, revision: stored })
    if (create === "*" && match === undefined) return existing ? refused("That id is taken.") : null
    const missing = preconditionMissing(req)
    if (missing) return missing
    if (!existing) return { status: 404, kind: "validation", reason: gone, message: "No such item." }
    if (match !== `"${stored}"`) return refused("Changed by another editor.")
    return null
  }

  /** Neither a create's If-None-Match: * alone nor an If-Match. */
  function preconditionMissing(req) {
    const create = req.headers["if-none-match"]
    const match = req.headers["if-match"]
    if (create === "*" && match === undefined) return null
    return create !== undefined || match === undefined ? { status: 428, kind: "validation", reason: "preconditionMissing", message: "If-Match or If-None-Match required." } : null
  }

  /** A name as the daemon stores it: trimmed, when the check asks the daemon to normalize. */
  const stored = (name) => (s.trimNames ? name.trim() : name)

  const http_ = http.createServer(async (req, res) => {
    const url = new URL(req.url, "http://x")
    let body = ""
    for await (const chunk of req) body += chunk
    const entry = {
      method: req.method, path: url.pathname, search: url.search, status: 0, ifMatch: req.headers["if-match"] ?? null, ifNoneMatch: req.headers["if-none-match"] ?? null,
      csrf: req.headers["x-sidegate-csrf"] ?? null, unifiCsrf: req.headers["x-csrf-token"] ?? null, idempotencyKey: req.headers["idempotency-key"] ?? null,
      contentType: req.headers["content-type"] ?? null, body, replayed: false,
    }
    s.log.push(entry)
    let write = null
    const json = (code, data, headers = {}) => {
      /* As Go's: every answer to a keyed write names the view revision, and a replay keeps the first answer's. */
      if (req.method !== "GET" && !UNKEYED.some((path) => url.pathname === base + path)) headers = { "Sidegate-Revision": String(s.revision), ...headers }
      entry.status = code
      if (write) rememberAnswer(s.writes, write.key, write.request, code, { code, data, headers })
      res.writeHead(code, { "Content-Type": "application/json", ...headers })
      res.end(JSON.stringify(data))
    }
    const fail = ({ status, kind, reason, message, detail, revision }) =>
      revision === undefined ? json(status, { kind, reason, message, ...(detail && { detail }) }) : json(status, { kind, reason, message, revision }, { ETag: `"${revision}"` })

    if (!url.pathname.startsWith(base)) { res.writeHead(404); return res.end() }
    if (s.unifi !== null && !unifiLogin(s.unifi, req, res)) { entry.status = 403; return }
    const rel = url.pathname.slice(base.length)
    /* A stopped daemon, as the gateway's nginx answers for it. */
    if (s.stopped && rel.startsWith("api/")) { entry.status = 502; res.writeHead(502, { "Content-Type": "text/html" }); return res.end("<h1>502 Bad Gateway</h1>") }
    if (req.method === "GET" && s.readFails[rel] > 0) {
      s.readFails[rel]--
      entry.status = 502
      res.writeHead(502, { "Content-Type": "text/html" })
      return res.end("<h1>502 Bad Gateway</h1>")
    }

    if (rel.startsWith("api/") && req.method !== "GET") {
      if (entry.csrf !== s.token) return fail({ status: 403, kind: "csrf", reason: "tokenExpired", message: "CSRF token expired." })
      const unkeyed = UNKEYED.includes(rel)
      if (!unkeyed && !entry.idempotencyKey) return fail({ status: 400, kind: "validation", reason: "keyMissing", message: "Idempotency-Key required." })
      if (body && !/^application\/json(;|$)/.test(entry.contentType ?? "")) return fail({ status: 415, kind: "validation", reason: "notJSON", message: "Send JSON." })
      if (!unkeyed) {
        const request = JSON.stringify([req.method, url.pathname, url.search, entry.ifMatch, entry.ifNoneMatch, body])
        const seen = checkKey(s.writes, entry.idempotencyKey, request)
        if (seen.kind === "reused") return fail({ status: 422, kind: "validation", reason: "keyReused", message: KEY_REUSED })
        if (seen.kind === "replay") {
          entry.replayed = true
          return json(seen.answer.code, seen.answer.data, seen.answer.headers)
        }
        write = { key: entry.idempotencyKey, request }
        if (s.writeFails > 0) {
          s.writeFails--
          return fail({ status: 503, kind: "unavailable", reason: "unavailable", message: "Controller busy." })
        }
      }
    }

    const GET = req.method === "GET"
    if (GET && s.readDelay[rel]) await new Promise((r) => setTimeout(r, s.readDelay[rel]))
    if (GET && s.readHolds[rel]) await s.readHolds[rel].held
    const parse = () => {
      try { return { value: JSON.parse(body) } } catch { return { problem: { status: 400, kind: "validation", reason: "malformed", message: "That request isn't in the expected form." } } }
    }

    if (GET && rel === "api/session") {
      if (s.sessionFails > 0) { s.sessionFails--; return fail({ status: 503, kind: "unavailable", reason: "unavailable", message: "Session store busy." }) }
      return json(200, { user: "owner", csrf: s.token, version: s.version, release: s.release })
    }
    if (GET && rel === "api/state") return json(200, view())
    if (GET && rel === "api/servers") {
      const limit = url.searchParams.get("limit")
      if (limit !== null && !(Number(limit) >= 1)) return fail({ status: 400, kind: "validation", reason: "badQuery", message: "limit must be a positive number." })
      return json(200, { items: s.catalogue, next: "" })
    }
    if (GET && rel === "api/analytics") {
      /* As the gateway's nginx answers for a daemon that fails the read. */
      if (s.analyticsFails) { res.writeHead(502, { "Content-Type": "text/html" }); return res.end("<h1>502 Bad Gateway</h1>") }
      const from = Date.parse(url.searchParams.get("from") ?? "")
      const to = Date.parse(url.searchParams.get("to") ?? "")
      const bucket = /^(\d+)s$/.exec(url.searchParams.get("bucket") ?? "")
      if (!(from < to) || !bucket || Number(bucket[1]) % 300 !== 0 || Number(bucket[1]) === 0) return fail({ status: 400, kind: "validation", reason: "badQuery", message: "That range can't be charted." })
      return json(200, { buckets: s.analytics?.(from, to, Number(bucket[1]), url.searchParams.getAll("lineage"), url.searchParams.getAll("source")) ?? [], coveredUntil: now(), gapCount: 0 })
    }
    if (GET && rel === "api/switches") return json(200, s.switches)
    if (GET && rel === "api/replaced") return json(200, s.replaced)
    if (GET && rel === "api/settings") return json(200, s.settings)
    if (rel === "api/settings" && req.method === "PUT") {
      const problem = precondition(req, s.settings, "malformed")
      if (problem) return fail(problem)
      const { value, problem: unread } = parse()
      if (unread) return fail(unread)
      const input = goDecode({ analyticsRetentionDays: "int" }, value)
      if (!input) return fail(UNFORMED)
      if (input.analyticsRetentionDays < 1 || input.analyticsRetentionDays > 365) return fail({ status: 422, kind: "validation", reason: "retentionRange", message: "analyticsRetentionDays is outside 1..365." })
      s.settings = { ...s.settings, ...input, revision: s.settings.revision + 1 }
      emit("settings")
      return json(200, s.settings)
    }
    /* As Go's: the check is true or false, present, with no precondition, and moves the settings revision. */
    if (rel === "api/settings/check-for-updates" && req.method === "PUT") {
      const { value, problem: unread } = parse()
      if (unread) return fail(unread)
      if (!goDecode({ checkForUpdates: "bool" }, value) || typeof value?.checkForUpdates !== "boolean") return fail(UNFORMED)
      s.settings = { ...s.settings, checkForUpdates: value.checkForUpdates, revision: s.settings.revision + 1 }
      emit("settings")
      return json(200, s.settings)
    }
    if (GET && rel === "api/update") return json(200, updateView())
    if (rel === "api/update/check" && req.method === "POST") {
      const { value, problem: unread } = parse()
      if (unread) return fail(unread)
      if (!goDecode({ auto: "bool" }, value) || typeof value?.auto !== "boolean") return fail(UNFORMED)
      if (updateView().releases) checkForUpdate(value.auto)
      else if (!value.auto) return fail(NO_RELEASES)
      return json(200, updateView())
    }
    /* As Go's: one update at a time. A press while one runs is answered with it, and its id is the daemon's. */
    if (rel === "api/update" && req.method === "POST") {
      const { value, problem: unread } = parse()
      if (unread) return fail(unread)
      if (!goDecode({ version: "string", restartsCore: "bool" }, value) || typeof value?.version !== "string" || typeof value.restartsCore !== "boolean") return fail(UNFORMED)
      const { releases, offered, running } = updateView()
      if (!releases) return fail(NO_RELEASES)
      if (!running) {
        if (!offered) return fail({ status: 422, kind: "validation", reason: "nothingNewer", message: "No newer release is offered." })
        if (offered.version !== value.version || offered.restartsCore !== value.restartsCore) return fail({ status: 422, kind: "validation", reason: "offerChanged", message: "That isn't what is offered now." })
        s.update = { ...s.update, running: { attempt: (++s.cursor).toString(16).padStart(16, "0"), version: offered.version }, pressedAt: now() }
      }
      return json(202, updateView())
    }
    /* As Go's: the key is tried on UniFi before it is stored, and never comes back. */
    if (rel === "api/unifi/key" && req.method === "PUT") {
      const { value, problem: unread } = parse()
      if (unread) return fail(unread)
      const key = typeof value?.key === "string" ? value.key.trim() : ""
      if (Object.keys(value ?? {}).length !== 1 || key === "" || /\s/.test(key)) return fail({ status: 400, kind: "validation", reason: "malformed", message: "The key must be one line." })
      if (s.unifiKeyAnswer === "keyRefused") return fail({ status: 422, kind: "validation", reason: "keyRefused", message: "UniFi refused that key." })
      if (s.unifiKeyAnswer === "notAnswering") return fail({ status: 422, kind: "validation", reason: "notAnswering", message: "UniFi didn't answer. Try again." })
      s.unifiTrouble = null
      s.unifiAcceptedAt = now()
      emit("slot")
      return json(200, {})
    }
    if (GET && rel === "api/events") {
      if (s.eventsFail > 0) { s.eventsFail--; entry.status = 502; res.writeHead(502, { "Content-Type": "text/html" }); return res.end("<h1>502 Bad Gateway</h1>") }
      entry.status = 200
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" })
      res.write(": connected\n\n")
      sse.add(res)
      req.on("close", () => sse.delete(res))
      return
    }
    const slotRoute = rel.match(/^api\/slots\/([^/]+)$/)
    if (slotRoute && req.method === "PUT") {
      const id = decodeURIComponent(slotRoute[1])
      const existing = s.slots.get(id)
      const problem = precondition(req, existing, "slotGone")
      if (problem) return fail(problem)
      const { value: input, problem: unread } = parse()
      if (unread) return fail(unread)
      if (!goDecode(SLOT, input)) return fail(UNFORMED)
      if (badExit(input?.exit)) return fail({ status: 422, kind: "validation", reason: "malformed", message: "An Exit is one server, group or source, Direct or Block." })
      if (!input.name?.trim()) return fail({ status: 422, kind: "validation", reason: "nameMissing", message: "A slot needs a name." })
      const exitChanged = JSON.stringify(existing?.exit) !== JSON.stringify(input.exit)
      if (exitChanged && input.exit.kind === "group" && !s.groups.has(input.exit.group)) return fail({ status: 422, kind: "validation", reason: "groupGone", message: "That group is gone." })
      if (exitChanged && input.exit.kind === "group" && s.groups.get(input.exit.group).resolved.length === 0) return fail({ status: 422, kind: "validation", reason: "groupEmpty", message: "That group has no servers. Change its filter first." })
      if (exitChanged && input.exit.kind === "source" && !s.sources.has(input.exit.source)) return fail({ status: 422, kind: "validation", reason: "sourceGone", message: "That source is gone." })
      if (exitChanged && input.exit.kind === "source" && !s.catalogue.some((l) => l.source === input.exit.source && l.presence === "present")) return fail({ status: 422, kind: "validation", reason: "sourceEmpty", message: "That source has no servers now." })
      if (exitChanged && input.exit.kind === "server" && !s.catalogue.some((l) => l.id === input.exit.server)) return fail({ status: 422, kind: "validation", reason: "serverGone", message: "That server is gone." })
      const made = revised(existing, id, (existing?.version ?? s.deletedAt.get(id) ?? 0) + 1, { ...input, name: stored(input.name) })
      /* Held, the runtime hasn't shown the move yet: it still reports what it had, and the move is pending. */
      const next = s.holdReadback && existing && exitChanged
        ? { ...made, appliedServer: existing.appliedServer, pending: true, runtime: { ...made.runtime, applied: existing.runtime.applied } }
        : made
      s.slots.set(id, next)
      refreshUsedBy()
      emit("slot", { slot: id })
      /* As Go's: the write answers before policy elects, so a slot coming onto the VPN reads checking in its answer, with
       * the runtime still on what it had. */
      const onVpn = (exit) => exit && exit.kind !== "direct" && exit.kind !== "block"
      const applied = existing?.runtime.applied ?? { kind: "down", reason: "unselected" }
      return json(200, onVpn(input.exit) && !onVpn(existing?.exit)
        ? { ...next, status: "checking", server: null, appliedServer: existing?.appliedServer ?? null, runtime: { ...next.runtime, applied } }
        : next)
    }
    if (slotRoute && req.method === "DELETE") {
      const id = decodeURIComponent(slotRoute[1])
      const existing = s.slots.get(id)
      if (!existing) return fail({ status: 404, kind: "validation", reason: "slotGone", message: "That slot is gone." })
      if (s.referenced.has(id)) return fail({ status: 409, kind: "referenced", reason: "routed", message: "UniFi routes still point at this slot." })
      s.deletedAt.set(id, existing.version)
      s.slots.delete(id)
      refreshUsedBy()
      emit("slot", { slot: id })
      return json(200, {})
    }
    const slotNames = (ids) => ids.map((id) => s.slots.get(id)?.name ?? id).join(", ")
    const groupRoute = rel.match(/^api\/groups\/([^/]+)$/)
    if (rel === "api/groups/preview" && req.method === "POST") {
      const { value, problem: unread } = parse()
      if (unread) return fail(unread)
      const input = goDecode({ members: MEMBERS }, value)
      if (!input) return fail(UNFORMED)
      input.members = decodeMembers(input.members)
      const refusedMembers = membersRefusal(input.members)
      if (refusedMembers) return fail(refusedMembers)
      return json(200, { matched: matchedBy(input.members, s.catalogue, s.reads) })
    }
    const tickRoute = rel.match(/^api\/groups\/([^/]+)\/ticked\/([^/]+)$/)
    if (tickRoute && req.method === "PUT") {
      const [id, server] = [decodeURIComponent(tickRoute[1]), decodeURIComponent(tickRoute[2])]
      const { value, problem: unread } = parse()
      if (unread) return fail(unread)
      const input = goDecode({ ticked: "bool" }, value)
      if (!input) return fail(UNFORMED)
      const existing = s.groups.get(id)
      if (!existing) return fail({ status: 404, kind: "validation", reason: "groupGone", message: "That group is gone." })
      const on = input.ticked === true
      const known = s.catalogue.find((l) => l.id === server)
      if (on && !existing.ticked.includes(server) && (!known || known.presence === "tombstone")) return fail({ status: 422, kind: "validation", reason: "serverGone", message: "That server is gone." })
      const ticked = on ? [...new Set([...existing.ticked, server])] : existing.ticked.filter((l) => l !== server)
      const next = groupView(id, existing.revision, { name: existing.name, members: existing.members, ticked }, existing.usedBy, s.catalogue, s.reads)
      s.groups.set(id, next)
      emit("group")
      return json(200, groupOut(next))
    }
    if (groupRoute && req.method === "PUT") {
      const id = decodeURIComponent(groupRoute[1])
      const existing = s.groups.get(id)
      const missing = preconditionMissing(req)
      if (missing) return fail(missing)
      const { value, problem: unread } = parse()
      if (unread) return fail(unread)
      /* As Go's: the body is read and its rule and ticks admitted before the stored revision is. A create carries its
       * ticks, [] for none; a replace keeps the stored ones and carries none. */
      const creates = req.headers["if-none-match"] === "*"
      const input = goDecode(creates ? { name: "string", members: MEMBERS, ticked: ["string"] } : { name: "string", members: MEMBERS }, value)
      const ticks = creates ? input?.ticked : []
      if (!input || !ticks) return fail(UNFORMED)
      input.members = decodeMembers(input.members)
      const refusedMembers = membersRefusal(input.members)
      if (refusedMembers) return fail(refusedMembers)
      if (ticks.some((l, i) => !l || ticks.indexOf(l) !== i)) return fail({ status: 422, kind: "validation", reason: "malformed", message: "A server is empty or repeated." })
      const problem = precondition(req, existing, "groupGone")
      if (problem) return fail(problem)
      if (!input.name?.trim()) return fail({ status: 422, kind: "validation", reason: "nameMissing", message: "A group needs a name." })
      const known = input.members.sources.values.filter((source) => s.sources.has(source))
      if (known.length === 0 && input.members.sources.values.length > 0) return fail({ status: 422, kind: "validation", reason: "sourcesGone", message: "Every source this group named is gone." })
      const members = { ...input.members, sources: { ...input.members.sources, values: known } }
      if (creates && ticks.some((l) => !s.catalogue.some((x) => x.id === l && x.presence !== "tombstone"))) return fail({ status: 422, kind: "validation", reason: "serverGone", message: "That server is gone." })
      if (creates && (!conditioned(members) || (members.box === "ticked" && ticks.length === 0))) return fail({ status: 422, kind: "validation", reason: "ruleMissing", message: "A new group needs a filter, or Box is ticked with a server ticked." })
      const next = groupView(id, (existing?.revision ?? s.deletedAt.get(`group/${id}`) ?? 0) + 1, { name: stored(input.name), members, ticked: creates ? ticks : existing.ticked }, existing?.usedBy ?? [], s.catalogue, s.reads)
      s.groups.set(id, next)
      emit("group")
      return json(200, groupOut(next))
    }
    if (groupRoute && req.method === "DELETE") {
      const id = decodeURIComponent(groupRoute[1])
      const existing = s.groups.get(id)
      if (!existing) return fail({ status: 404, kind: "validation", reason: "groupGone", message: "That group is gone." })
      if (existing.usedBy.length) return fail({ status: 409, kind: "referenced", reason: "inUse", message: `Used by ${slotNames(existing.usedBy)}.` })
      s.deletedAt.set(`group/${id}`, existing.revision)
      s.groups.delete(id)
      emit("group")
      return json(200, {})
    }
    const subRoute = rel.match(/^api\/sources\/([^/]+)$/)
    if (subRoute && req.method === "DELETE") {
      const problem = removeSource(decodeURIComponent(subRoute[1]))
      return problem ? fail(problem) : json(200, {})
    }
    if (subRoute && req.method === "PUT") {
      const id = decodeURIComponent(subRoute[1])
      const existing = s.sources.get(id)
      const problem = precondition(req, existing, "sourceGone")
      if (problem) return fail(problem)
      const { value, problem: unread } = parse()
      if (unread) return fail(unread)
      const input = goDecode(SOURCE, value)
      if (!input) return fail(UNFORMED)
      if (!["remote", "manual"].includes(input.origin.kind)) return fail({ status: 422, kind: "validation", reason: "malformed", message: "A source is either a URL or pasted text." })
      if (input.origin?.kind === "manual" && !input.name?.trim()) return fail({ status: 422, kind: "validation", reason: "nameMissing", message: "A source without a URL needs a name." })
      const revision = (existing?.revision ?? s.deletedAt.get(id) ?? 0) + 1
      const origin = input.origin
      if (existing && existing.origin.kind !== origin?.kind) return fail({ status: 422, kind: "validation", reason: "originFixed", message: "A source can't change between a URL and pasted text." })
      let next
      if (origin?.kind === "manual") {
        if (origin.text !== null && !pasted(origin.text)) return fail({ status: 422, kind: "validation", reason: "unreadableText", message: "That text isn't a list of servers this gateway can read." })
        if (!existing && origin.text === null) return fail({ status: 422, kind: "validation", reason: "textMissing", message: "A new source needs text." })
        next = { ...source(revision), id, name: stored(input.name), nameSet: true, origin: { kind: "manual" }, stored: 1, addedAt: existing?.addedAt ?? now() }
      } else {
        if (!existing && !origin?.url) return fail({ status: 422, kind: "validation", reason: "urlMissing", message: "A new source needs a URL." })
        if (origin.url && !/^https:\/\/[^/]/i.test(origin.url)) return fail({ status: 422, kind: "validation", reason: "notHttps", message: "The source URL must be an https address." })
        const base = existing ?? { ...source(revision), addedAt: now(), origin: { ...source(revision).origin, provider: { ...source(revision).origin.provider, title: "" } } }
        const host = () => new URL(origin.url ?? s.secrets.get(id)?.url).hostname
        next = { ...base, revision, id, name: stored(input.name) || base.origin.provider.title || host(), nameSet: input.name !== "" }
        next.origin = {
          ...next.origin, userAgent: origin.userAgent, refreshEverySeconds: origin.refreshEverySeconds || (next.origin.provider.suggestedRefreshSeconds ? 0 : 21600),
          paused: origin.paused,
        }
      }
      next = ordered(next, s.catalogue)
      s.sources.set(id, next)
      if (origin.kind === "remote") {
        s.secrets.set(id, { url: origin.url ?? s.secrets.get(id)?.url ?? null })
      }
      emit("source", { source: id })
      return json(200, sourceView(next))
    }
    const refreshes = rel.match(/^api\/sources\/([^/]+)\/refreshes$/)
    if (GET && refreshes) {
      const id = decodeURIComponent(refreshes[1])
      if (!s.sources.has(id)) return fail({ status: 404, kind: "validation", reason: "sourceGone", message: "That source is gone." })
      return json(200, s.refreshes?.get(id) ?? [])
    }
    const sourceUrl = rel.match(/^api\/sources\/([^/]+)\/url$/)
    if (GET && sourceUrl) {
      const secret = s.secrets.get(decodeURIComponent(sourceUrl[1]))
      return secret?.url ? json(200, { url: secret.url }) : fail({ status: 404, kind: "validation", reason: "notRemote", message: "That source has no URL." })
    }
    const recreate = rel.match(/^api\/slots\/([^/]+)\/client\/(enable|recreate)$/)
    if (recreate && req.method === "POST") {
      const target = s.slots.get(decodeURIComponent(recreate[1]))
      if (!target) return fail({ status: 404, kind: "validation", reason: "slotGone", message: "That slot is gone." })
      const needs = recreate[2] === "enable" ? "disabled" : "deleted"
      if (target.unifi.client !== needs) return fail({ status: 422, kind: "validation", reason: needs === "disabled" ? "clientNotDisabled" : "clientPresent", message: needs === "disabled" ? "This slot's VPN Client isn't turned off in UniFi." : "This slot's VPN Client is still in UniFi." })
      s.slots.set(target.id, { ...target, unifi: { ...target.unifi, client: "present" } })
      emit("slot", { slot: target.id })
      return json(202, {})
    }
    const refresh = rel.match(/^api\/sources\/([^/]+)\/refresh$/)
    if (refresh && req.method === "POST") {
      const target = s.sources.get(decodeURIComponent(refresh[1]))
      const { value, problem: unread } = body ? parse() : { value: {} }
      if (unread) return fail(unread)
      const input = goDecode({ userAgent: "*string" }, value)
      if (!input) return fail(UNFORMED)
      if (typeof input.userAgent === "string" && (input.userAgent.length > 200 || [...input.userAgent].some((c) => c < " " || c === "\x7f"))) {
        return fail({ status: 422, kind: "validation", reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." })
      }
      if (!target) return fail({ status: 404, kind: "validation", reason: "sourceGone", message: "That source is gone." })
      if (target.origin.kind !== "remote") return fail({ status: 422, kind: "validation", reason: "notRemote", message: "Only a source with a URL refreshes." })
      /* As Go's: another agent is stored with the refresh, with no precondition, and moves the source's revision. */
      const agent = typeof input.userAgent === "string" ? input.userAgent || "Happ/3.0.0" : target.origin.userAgent
      if (agent !== target.origin.userAgent) s.sources.set(target.id, { ...target, revision: target.revision + 1, origin: { ...target.origin, userAgent: agent } })
      emit("source", { source: target.id })
      return json(202, {})
    }
    const probe = rel.match(/^api\/servers\/([^/]+)\/probe$/)
    if (probe && req.method === "POST") {
      const id = probe[1]
      if (!s.catalogue.some((l) => l.id === id)) return fail({ status: 404, kind: "validation", reason: "notTestable", message: "The core can't run that server, or it left its source." })
      const value = id === "l-ams" ? 111 : 222
      return json(200, { lineage: id, verdict: "succeeded", pingMs: value, newConnectionMs: value, at: now() })
    }
    /* As Go's: a press tests the server first, and only a pass reaches the owner, which moves its slots there by hand. It
     * prefers the server unless it was first already, until a day from now. Clear drops only the preference it names. */
    const preferRoute = rel.match(/^api\/(groups|sources)\/([^/]+)\/preferred\/([^/]+)$/)
    if (preferRoute && (req.method === "POST" || req.method === "DELETE")) {
      const [kind, id, server] = [preferRoute[1], decodeURIComponent(preferRoute[2]), decodeURIComponent(preferRoute[3])]
      const store = kind === "groups" ? s.groups : s.sources
      const follows = (slot) => (kind === "groups" ? slot.exit.kind === "group" && slot.exit.group === id : slot.exit.kind === "source" && slot.exit.source === id)
      const gone = () => fail({ status: 404, kind: "validation", reason: kind === "groups" ? "groupGone" : "sourceGone", message: "That owner is gone." })
      if (req.method === "DELETE") {
        const endsAt = Date.parse(url.searchParams.get("endsAt") ?? "")
        if (Number.isNaN(endsAt)) return fail({ status: 400, kind: "validation", reason: "badQuery", message: "endsAt must be the preference's end, as the view gives it." })
        const owner = store.get(id)
        if (!owner) return gone()
        if (owner.preferred?.server === server && Date.parse(owner.preferred.endsAt) === endsAt) store.set(id, { ...owner, preferred: null })
        emit(kind === "groups" ? "group" : "source", kind === "groups" ? {} : { source: id })
        return json(200, {})
      }
      if (body) return fail({ status: 400, kind: "validation", reason: "malformed", message: "A press has no body." })
      const known = s.catalogue.find((l) => l.id === server)
      if (!known) return fail({ status: 404, kind: "validation", reason: "notTestable", message: "The core can't run that server, or it left its source." })
      const owner = store.get(id)
      if (!owner) return gone()
      const slots = [...s.slots.values()].filter(follows)
      if (slots.length === 0) return fail({ status: 422, kind: "validation", reason: "ownerIdle", message: "No slot uses it." })
      const members = kind === "groups" ? owner.resolved : s.catalogue.filter((l) => l.source === id && l.presence === "present").map((l) => l.id)
      if (!members.includes(server)) {
        return known.presence === "present"
          ? fail({ status: 422, kind: "validation", reason: "notMember", message: "That server isn't one it uses." })
          : fail({ status: 422, kind: "validation", reason: "serverGone", message: "That server is gone." })
      }
      const value = server === "l-ams" ? 111 : 222
      if (s.preferVerdict !== "succeeded") return json(200, { lineage: server, verdict: s.preferVerdict, pingMs: null, newConnectionMs: null, at: now() })
      const first = owner.order[0] === server
      /* As Go's book hears the press's pass: one pass leaves a failing record recovering, which the view gives as succeeded. */
      s.catalogue = s.catalogue.map((l) => (l.id === server ? { ...l, health: { ...l.health, verdict: "succeeded", current: true, checkedAt: now() } } : l))
      emit("catalogue")
      /* A move the runtime has landed, or one still landing, whose slots name the server and wait for their readback. */
      const move = (landed) => {
        const current = store.get(id)
        if (current.preferred?.server !== server) store.set(id, { ...current, preferred: first ? null : { server, endsAt: new Date(Date.now() + 86_400_000).toISOString() } })
        for (const slot of slots) {
          const now = s.slots.get(slot.id)
          s.slots.set(slot.id, landed ? { ...now, status: "serving", downReason: null, server, appliedServer: server, runtime: { ...now.runtime, applied: { kind: "up", server } } } : { ...now, server, pending: true })
        }
        emit("slot")
      }
      move(!s.holdPreferLanding)
      return json(200, { lineage: server, verdict: "succeeded", pingMs: value, newConnectionMs: value, at: now() })
    }
    if (rel.startsWith("api/")) return fail({ status: 404, kind: "validation", reason: "noRoute", message: "No such route." })

    const file = path.join(dist, rel)
    if (rel && fs.existsSync(file) && fs.statSync(file).isFile()) {
      entry.status = 200
      res.writeHead(200, { "Content-Type": TYPES[path.extname(file)] ?? "application/octet-stream" })
      return res.end(fs.readFileSync(file))
    }
    entry.status = 200
    res.writeHead(200, { "Content-Type": "text/html" })
    res.end(fs.readFileSync(path.join(dist, "index.html")))
  })

  return new Promise((resolve) => {
    http_.listen(port, "127.0.0.1", () => resolve({
      url: `http://127.0.0.1:${http_.address().port}${base}`,
      reset,
      log: () => s.log,
      slot: (id) => s.slots.get(id),
      slots: () => [...s.slots.values()],
      group: (id) => s.groups.get(id),
      groups: () => [...s.groups.values()],
      catalogue: () => s.catalogue,
      settings: () => s.settings,
      sseClients: () => sse.size,
      set: (patch) => Object.assign(s, patch),
      /** Holds every GET of `rel` not yet answered until the release, which answers each as things then stand. Held here, where
       * the answer is made, a change made meanwhile is in no answer before the release, whenever the page sent its read. */
      holdReads(rel) {
        let release
        const held = new Promise((done) => { release = done })
        s.readHolds[rel] = { held, release }
        return () => { delete s.readHolds[rel]; release() }
      },
      /** The runtime shows a held move: what the slot is on is now applied. */
      readback(id) {
        const current = s.slots.get(id)
        const { applied } = running(current)
        s.slots.set(id, { ...current, appliedServer: current.server, pending: false, runtime: { ...current.runtime, applied } })
        emit("slot", { slot: id })
      },
      /** Another writer renames a slot. */
      renameSlot(id, name) {
        const current = s.slots.get(id)
        s.slots.set(id, revised(current, id, current.version + 1, { name, exit: current.exit }))
        emit("slot", { slot: id })
      },
      /** Another writer deletes a slot. Its id's revisions carry on if it is ever created again. */
      deleteSlot(id) {
        s.deletedAt.set(id, s.slots.get(id).version)
        s.slots.delete(id)
        refreshUsedBy()
        emit("slot", { slot: id })
      },
      /** Another writer renames a group. */
      renameGroup(id, name) {
        const current = s.groups.get(id)
        s.groups.set(id, { ...current, name, revision: current.revision + 1 })
        emit("group")
      },
      /** Another writer deletes a group. Its id's revisions carry on if it is ever created again. */
      deleteGroup(id) {
        s.deletedAt.set(`group/${id}`, s.groups.get(id).revision)
        s.groups.delete(id)
        emit("group")
      },
      /** Another writer deletes a source. */
      deleteSource(id) {
        const problem = removeSource(id)
        if (problem) throw new Error(`deleteSource ${id}: ${problem.message}`)
      },
      source: (id) => s.sources.get(id),
      /** The write-only fields as stored: the view never shows them. */
      secrets: (id) => s.secrets.get(id),
      sources: () => [...s.sources.values()],
      /** Another writer renames a source. */
      renameSource(id, name) {
        const current = s.sources.get(id)
        s.sources.set(id, { ...current, name, revision: current.revision + 1 })
        emit("source", { source: id })
      },
      /** Another writer changes a slot, or deletes it with `null`. `announce` false is a change the page hasn't heard of yet. */
      changeSlot(id, patch, { announce = true } = {}) {
        const current = s.slots.get(id)
        if (patch === null) {
          s.deletedAt.set(id, current.version)
          s.slots.delete(id)
        } else s.slots.set(id, { ...current, ...patch, unifi: { ...current.unifi, name: patch.name ?? current.unifi.name }, version: current.version + 1 })
        refreshUsedBy()
        if (announce) emit("slot", { slot: id })
        else s.revision++
      },
      /** Another writer changes a setting. */
      changeSettings(patch) {
        s.settings = { ...s.settings, ...patch, revision: s.settings.revision + 1 }
        emit("settings")
      },
      /** A refresh changes the source's servers without moving its revision. `announce` false is one the page hasn't heard of. */
      refreshDocument(id, { addServer, announce = true } = {}) {
        const current = s.sources.get(id)
        if (addServer) s.catalogue = [...s.catalogue, addServer]
        s.sources.set(id, ordered(current, s.catalogue))
        if (announce) emit("source", { source: id })
      },
      /** The owner's UniFi routes come to point at a slot's VPN Client. */
      reference: (id) => { s.referenced.add(id) },
      /** The update that runs ends: `outcome`, and for failed a `reason`. */
      endUpdate({ outcome, reason = null, message = null }) {
        const { running: { attempt, version }, pressedAt } = s.update
        s.update = { ...s.update, running: null, last: { attempt, version, at: pressedAt, outcome, reason, message } }
      },
      rotateToken: () => { s.token = `token-${Number(s.token.split("-")[1]) + 1}` },
      /** The reference feed joins Aurora as a second source. The servers labelled in `missing` left the feed. */
      addAcme({ missing = [] } = {}) {
        const { servers, source: sub } = acme()
        s.catalogue = [...s.catalogue, ...servers.map((l) => (missing.includes(l.label) ? { ...l, presence: "missing", missingSince: new Date().toISOString() } : l))]
        s.sources.set(sub.id, ordered(sub, s.catalogue))
        s.secrets.set(sub.id, { url: "https://acme.example/sub" })
      },
      /** Another writer changes a group or a source, as a preference that ends or a slot that leaves it changes them. */
      changeOwner(kind, id, patch) {
        const store = kind === "group" ? s.groups : s.sources
        store.set(id, { ...store.get(id), ...patch })
        emit(kind, kind === "group" ? {} : { source: id })
      },
      /** Probes change what the daemon says of the servers. */
      updateServers(catalogue) {
        s.catalogue = catalogue
        emit("catalogue")
      },
      prependServer() {
        s.catalogue = [server("l-ber", "Berlin", "DE", 12), ...s.catalogue]
        s.sources.set("aurora", ordered(s.sources.get("aurora"), s.catalogue))
        emit("catalogue")
      },
      /** Server `id` left its source's feed: it stays in the catalogue, missing, and its source and groups rank it no
       * more. The real daemon's control commits no command, so the page hears of it and the view's revision stays. */
      missServer(id) {
        const left = s.catalogue.find((l) => l.id === id)
        s.catalogue = s.catalogue.map((l) => (l.id === id ? { ...l, presence: "missing", missingSince: now() } : l))
        const owner = s.sources.get(left.source)
        s.sources.set(owner.id, { ...ordered(owner, s.catalogue), stored: owner.stored - 1, missing: owner.missing + 1 })
        resolveGroups()
        announce("catalogue")
        announce("source", { source: owner.id })
        announce("group")
      },
      /** The daemon stops: the stream drops and every API request fails until `start`. */
      stop() {
        s.stopped = true
        for (const res of sse) res.destroy()
        sse.clear()
      },
      start: () => { s.stopped = false },
      dropEvents() {
        for (const res of sse) res.destroy()
        sse.clear()
      },
      close: () => new Promise((done) => {
        for (const res of sse) res.destroy()
        http_.closeAllConnections()
        http_.close(() => done())
      }),
    }))
  })
}
