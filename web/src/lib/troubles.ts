import { counted, DAY, noSize, REASON_WORDS, size, spanOf, spanSpoken, spanUnder, spanWords, type Span } from "./format.ts"
import { rate, serverStanding, type Rating } from "./health.ts"
import type { AnalyticsBucket, AnalyticsQuery, ClientTrouble, Failure, GroupView, LineageId, Quota, RefreshEvent, ServerView, SourceView, UnifiTrouble } from "./schema.ts"

/** Amber or red. */
export type Tone = "warn" | "bad"

/** Each failover drops every connection of every slot on it: once a day is noticeable, three a day is a broken evening. */
export const FAILOVERS_WARN = 7
export const FAILOVERS_BAD = 21
const EXPIRY_WARN_DAYS = 7
const TRAFFIC_LOW = 0.1
const READING_OLD_MS = DAY

/** When the subscription ends, as the provider said. One a year or more away is `far`, and the heading doesn't show it. */
export type Expiry =
  | { kind: "unreported" }
  | { kind: "far"; at: string }
  | { kind: "expires"; at: string; in: Span; soon: boolean }
  | { kind: "expired"; at: string; ago: Span }

/** What is left of the traffic, as the provider last said. With no limit, or none reported, the heading has no traffic fact. A
 * reading taken before a refill date that has since passed is `unknown`: it no longer says what is left. Otherwise `refill` is how
 * long until it refills, when that comes before the expiry, and `old` the reading's age once it is over a day old. */
export type Traffic =
  | { kind: "unreported" }
  | { kind: "unlimited" }
  | { kind: "unknown"; limit: number; due: string; read: Span }
  | { kind: "left"; left: number; limit: number; share: number; upload: number | null; download: number | null; refills: string | null; refill: Span | null; old: Span | null }

/** A subscription's refreshes: the last worked, or the last ones failed, since one first worked or before. */
export type Refresh = { failing: false; at: string | null } | { failing: true; code: Failure | null }

/** What a source's or a group's troubles are read from. A group has servers and failovers alone. `failovers` is null until the
 * week is read. */
type Readings = {
  servers: { listed: number; down: number } | null
  expiry: Expiry | null
  traffic: Traffic | null
  failovers: number | null
  refresh: Refresh | null
}

export const expiryWords = (e: Extract<Expiry, { kind: "expires" | "expired" }>) =>
  e.kind === "expired" ? `Expired ${spanWords(e.ago)} ago` : `Expires in ${spanWords(e.in)}`

type Left = Pick<Extract<Traffic, { kind: "left" }>, "left" | "limit">

/** What is left: "100 GB", and "0 GB", in the limit's unit, with nothing left. The heading draws it after its bar, which says "left". */
export const leftAmount = ({ left, limit }: Left) => (left > 0 ? size(left) : noSize(limit))

/** "100 GB left", the list's trouble words. */
export const leftWords = (t: Left) => `${leftAmount(t)} left`

export const failoverTone = (n: number): Tone | null => (n >= FAILOVERS_BAD ? "bad" : n >= FAILOVERS_WARN ? "warn" : null)
export const trafficTone = (t: Extract<Traffic, { kind: "left" }>): Tone | null => (t.left <= 0 ? "bad" : t.share < TRAFFIC_LOW ? "warn" : null)

/** One thing wrong with a source or a group: its words in its tone's colour, the same in the list and the heading. */
export type Trouble = { kind: (typeof TROUBLES)[number]["kind"]; tone: Tone; words: string }

const NO_SERVERS = "No servers"

/** Every trouble there is, in the one order the list and the heading both give them. A server not rated lately is neither up nor
 * down, since the daemon probes only when it needs the answer, so only every server down is "No server up". */
const TROUBLES = [
  { kind: "empty", of: ({ servers: s }: Readings): [Tone, string] | null => (s?.listed === 0 ? ["bad", NO_SERVERS] : null) },
  { kind: "servers", of: ({ servers: s }: Readings): [Tone, string] | null => (s && s.listed > 0 && s.down === s.listed ? ["bad", "No server up"] : null) },
  { kind: "expiry", of: ({ expiry: e }: Readings): [Tone, string] | null =>
    e?.kind === "expired" ? ["bad", expiryWords(e)] : e?.kind === "expires" && e.soon ? ["warn", expiryWords(e)] : null },
  { kind: "traffic", of: ({ traffic: t }: Readings): [Tone, string] | null => {
    const tone = t?.kind === "left" ? trafficTone(t) : null
    return t?.kind === "left" && tone ? [tone, leftWords(t)] : null
  } },
  { kind: "failovers", of: ({ failovers }: Readings): [Tone, string] | null => {
    if (failovers === null) return null
    const tone = failoverTone(failovers)
    return tone && [tone, `${counted(failovers, "failover")} in 7d`]
  } },
  { kind: "refresh", of: ({ refresh }: Readings): [Tone, string] | null => (refresh?.failing ? ["warn", "Refresh failing"] : null) },
] as const

/** Whether a source or group lists no server at all, as its list entry and a slot's box on it both say. */
export const noServers = (troubles: readonly Trouble[]) => troubles.some((t) => t.kind === "empty")

function troublesOf(readings: Readings): Trouble[] {
  return TROUBLES.flatMap(({ kind, of }) => { const t = of(readings); return t ? [{ kind, tone: t[0], words: t[1] }] : [] })
}

/** UniFi's troubles, each of which keeps it from making any VPN Client: red where only the owner can end it, amber where UniFi
 * may come back by itself. */
export const UNIFI_TROUBLES: Record<UnifiTrouble, { tone: Tone; words: string }> = {
  noKey: { tone: "bad", words: "No API key" },
  keyRefused: { tone: "bad", words: "Key refused" },
  notAnswering: { tone: "warn", words: "Not answering" },
}

/** UniFi as the daemon last found it: in trouble, with when it last answered the key since the daemon started; or able to make
 * VPN Clients, the key answered at `at`, or not yet answered since the daemon started. */
export type UnifiStanding =
  | { kind: "trouble"; trouble: UnifiTrouble; lastWorked: string | null }
  | { kind: "answered"; at: string }
  | { kind: "unanswered" }

export const unifiStanding = ({ unifiTrouble, unifi }: { unifiTrouble: UnifiTrouble | null; unifi: { acceptedAt: string | null } }): UnifiStanding =>
  unifiTrouble ? { kind: "trouble", trouble: unifiTrouble, lastWorked: unifi.acceptedAt }
  : unifi.acceptedAt ? { kind: "answered", at: unifi.acceptedAt } : { kind: "unanswered" }

/** What UniFi couldn't do for one VPN Client of ours, in its box: each waits on the owner, in the page or in UniFi, while the slot
 * keeps serving. */
export const CLIENT_TROUBLES: Record<ClientTrouble, { tone: Tone; words: string }> = {
  nameTaken: { tone: "warn", words: "Name taken" },
  limitReached: { tone: "warn", words: "Limit reached" },
  refused: { tone: "warn", words: "UniFi refused" },
  routed: { tone: "warn", words: "Still routed" },
}

/** Why the daemon refused, in the page's words, one entry per reason Go gives (`Reason` in internal/daemon/refusals.go;
 * wire.test.ts holds the two lists equal). A reason that is a state the page already names takes that state's words. */
export const REFUSAL_WORDS = {
  noRoute: "Invalid request",
  badQuery: "Bad query",
  keyMissing: "Missing request key",
  keyReused: "Duplicate request",
  tokenExpired: "Token expired",
  tooLarge: "Too large",
  notJSON: "Invalid request",
  malformed: "Invalid request",
  preconditionMissing: "Out of date",
  stale: "Changed in another tab",
  unavailable: "Unavailable",
  busy: "Busy",
  notSaved: "Not saved",
  probeUnfinished: "Test unfinished",
  badId: "Bad ID",
  badUserAgent: "Bad User-Agent",
  badRefresh: "Invalid refresh interval",
  notHttps: "Must start with https://",
  slotGone: "Slot gone",
  groupGone: "Group gone",
  sourceGone: "Source gone",
  serverGone: "Server gone",
  notTestable: "Can't test",
  groupEmpty: NO_SERVERS,
  sourceEmpty: NO_SERVERS,
  inUse: "In use",
  routed: CLIENT_TROUBLES.routed.words,
  sourcesGone: "Sources gone",
  ownerIdle: "Not assigned to any slot",
  notMember: "Server not in group",
  serverChanged: REASON_WORDS.revisionChanged,
  failedSince: "Failed",
  clientsFull: CLIENT_TROUBLES.limitReached.words,
  slotsFull: "No free slot",
  clientNotDisabled: "Not disabled",
  clientPresent: "Still in UniFi",
  noReleases: "Development build",
  nothingNewer: "Nothing newer",
  offerChanged: "Release changed",
  lifecycleRunning: "Busy",
  nameMissing: "Required",
  urlMissing: "Required",
  textMissing: "Required",
  ruleMissing: "Add a filter or select servers",
  notRemote: "No URL",
  originFixed: "Cannot change source type",
  retentionRange: "1d to 365d",
  noUsableServers: "No usable servers",
  unreadableText: "Unknown format",
  badPattern: "Bad pattern",
  keyRefused: UNIFI_TROUBLES.keyRefused.words,
  notAnswering: UNIFI_TROUBLES.notAnswering.words,
} as const

/** What a refusal says: its reason's words, a refused pattern's cause as Go gives it, or the daemon's own message for a reason
 * the page doesn't know. */
export function refusalWords(refusal: { readonly reason: string; readonly detail: string | null; readonly message: string }): string {
  if (refusal.reason === "badPattern" && refusal.detail) return refusal.detail.charAt(0).toUpperCase() + refusal.detail.slice(1)
  return Object.hasOwn(REFUSAL_WORDS, refusal.reason) ? REFUSAL_WORDS[refusal.reason as keyof typeof REFUSAL_WORDS] : refusal.message
}

/** A toast's why starts small, as each of its endings does ("timed out", "was deleted"), a name such as UniFi kept. */
export const toastWhy = (words: string) => (/^[A-Z][a-z]/.test(words) ? words.charAt(0).toLowerCase() + words.slice(1) : words)

/** The trouble of a kind, for the heading to draw where that fact stands. */
export const troubleOf = (troubles: readonly Trouble[], kind: Trouble["kind"]) => troubles.find((t) => t.kind === kind) ?? null

/** The dot: `rated`, the worst trouble, grey while no server is up, and the reasons its card names; or `first`, a source whose
 * first refresh is running, which has no trouble yet and draws the faint Pending dot. */
export type Dot = { kind: "rated"; rating: Rating; why: string[] } | { kind: "first"; rating: "none"; why: [] }

/** A dot with nothing to say yet, as an entry the page hasn't rated draws. */
export const UNRATED: Dot = { kind: "rated", rating: "none", why: [] }

function dotOf(troubles: readonly Trouble[], up: boolean): Dot {
  const why = troubles.map((t) => t.words)
  if (troubles.some((t) => t.tone === "bad")) return { kind: "rated", rating: "bad", why }
  if (troubles.length > 0) return { kind: "rated", rating: "unstable", why }
  return { kind: "rated", rating: up ? "good" : "none", why: [] }
}

/** Whether a slot could take over on this server now. A missing or unsupported one can't, whatever its dot says. */
const isUp = (s: ServerView) => serverStanding(s) === "usable" && rate(s).rating === "good"
const isDown = (s: ServerView) => rate(s).rating === "bad"

/** How far a source's refreshes have come: its first still running, none worked yet and the last failed, or one worked; a pasted
 * source has worked. Of a source's three states with no servers, `first` is the only one that is waiting. */
export type Fetched = "first" | "never" | "worked"

export const fetchedOf = (origin: SourceView["origin"]): Fetched =>
  origin.kind !== "remote" || origin.fetch.updatedAt !== null ? "worked" : origin.fetch.failures === 0 ? "first" : "never"

/** Everything a source's heading says, and the troubles its list entry repeats. `facts` is null for a subscription no refresh has
 * worked for yet, whose heading is its name, Refresh and actions. `expiry` and `traffic` are a subscription's, and `traffic` is
 * null once it has expired. */
export type Standing = {
  fetched: Fetched
  facts: { expiry: Expiry | null; traffic: Traffic | null } | null
  refresh: Refresh | null
  troubles: Trouble[]
  dot: Dot
}

type Remote = Extract<SourceView["origin"], { kind: "remote" }>

export function expiryOf(expires: string | null, now: number): Expiry {
  if (expires === null) return { kind: "unreported" }
  const ms = Date.parse(expires) - now
  if (ms <= 0) return { kind: "expired", at: expires, ago: spanOf(-ms) }
  const left = spanOf(ms)
  if (left.unit === "y") return { kind: "far", at: expires }
  return { kind: "expires", at: expires, in: left, soon: left.unit === "m" || left.unit === "h" || (left.unit === "d" && left.n <= EXPIRY_WARN_DAYS) }
}

export function trafficOf({ upload, download, limit, expires, refills, observedAt }: Quota, now: number): Traffic {
  if (upload === null && download === null) return { kind: "unreported" }
  if (!limit) return { kind: "unlimited" }
  const read = observedAt === null ? null : Date.parse(observedAt)
  if (refills !== null && read !== null && Date.parse(refills) <= now && read < Date.parse(refills)) return { kind: "unknown", limit, due: refills, read: spanOf(now - read) }
  const left = Math.max(0, limit - (upload ?? 0) - (download ?? 0))
  const refillMs = refills === null ? 0 : Date.parse(refills) - now
  const beforeExpiry = refills !== null && (expires === null || Date.parse(refills) < Date.parse(expires))
  return {
    kind: "left", left, limit, share: left / limit, upload, download, refills,
    refill: refillMs > 0 && beforeExpiry ? spanOf(refillMs) : null,
    old: read !== null && now - read > READING_OLD_MS ? spanOf(now - read) : null,
  }
}

const refreshOf = ({ fetch }: Remote): Refresh => (fetch.failures > 0 ? { failing: true, code: fetch.failureCode } : { failing: false, at: fetch.checkedAt })

/** A source as its heading, its list entry and its Exit option say it, from its servers and its week's failovers. Its dot is its
 * own troubles alone: no first choice, since the heading speaks of none. `servers` is null until the server list is read, and
 * `failovers` until the week is; until then neither the dot nor the list counts them. While its first refresh runs it has no
 * trouble and its dot is Pending. A refresh that failed before any worked lists no servers, which is a trouble. */
export function standingOf(source: SourceView, servers: readonly ServerView[] | null, failovers: number | null, now = Date.now()): Standing {
  const present = (servers ?? []).filter((s) => s.presence === "present")
  const remote = source.origin.kind === "remote" ? source.origin : null
  const fetched = fetchedOf(source.origin)
  const expiry = remote && expiryOf(remote.provider.quota.expires, now)
  const facts: Standing["facts"] = fetched !== "worked" ? null : {
    expiry,
    traffic: remote && expiry?.kind !== "expired" ? trafficOf(remote.provider.quota, now) : null,
  }
  const refresh = remote && refreshOf(remote)
  const troubles = troublesOf({
    servers: fetched === "first" || servers === null ? null : { listed: present.length, down: present.filter(isDown).length },
    expiry: facts?.expiry ?? null, traffic: facts?.traffic ?? null, failovers, refresh,
  })
  return { fetched, facts, refresh, troubles, dot: fetched === "first" ? { kind: "first", rating: "none", why: [] } : dotOf(troubles, present.some(isUp)) }
}

const WORSE: Record<Rating, number> = { none: 0, good: 1, unstable: 2, bad: 3 }

export type GroupState = { first: ServerView | undefined; up: number; total: number; troubles: Trouble[] } & Extract<Dot, { kind: "rated" }>

/** A group as a slot on it would find it: how many servers could take over now, whether all are down, failovers, and the server
 * a slot gets, which is its first row. The dot is that row's rating, Unchecked included, unless the group's own trouble is worse. `troubles` are its
 * own, for its list entry. `servers` is null until the server list is read, and until then the group has no servers trouble. */
export function groupState(group: Pick<GroupView, "resolved" | "firstChoice" | "moves7d">, servers: Map<LineageId, ServerView> | null): GroupState {
  const members = group.resolved.flatMap((id) => { const s = servers?.get(id); return s ? [s] : [] })
  const up = members.filter(isUp).length
  const total = group.resolved.length
  const first = group.firstChoice ? servers?.get(group.firstChoice) : undefined
  const troubles = troublesOf({
    servers: servers && { listed: total, down: members.filter(isDown).length },
    expiry: null, traffic: null, failovers: group.moves7d, refresh: null,
  })
  const own: Extract<Dot, { kind: "rated" }> = { kind: "rated", rating: troubles.some((t) => t.tone === "bad") ? "bad" : troubles.length > 0 ? "unstable" : "none", why: troubles.map((t) => t.words) }
  const row = first ? rate(first) : null
  const ownWins = row && WORSE[own.rating] > WORSE.good && WORSE[own.rating] >= WORSE[row.rating]
  const judged = !row || ownWins ? own : { kind: "rated" as const, rating: row.rating, why: [...own.why, ...row.why.map((w) => `First choice: ${w}`)] }
  return { first, up, total, ...judged, troubles }
}

/** Six hours of one local day, from 00, 06, 12 or 18 by its clock to the next, an hour short or long where the clocks change in
 * it: its failovers and how long the source was in use in it, in ms. A place that ended before the source was added, or that
 * starts after the read, holds nothing. */
export type Place = { start: number; end: number; failovers: number; inUse: number }

/** A stretch of quarter hours in use, from the first one's start to the last one's end, or to the read in the quarter hour in
 * progress. */
export type UseRun = { from: number; to: number }

/** The source's seven local days, today last, from the analytics of its servers, as the read answered at `at` has them: nothing
 * after `at` is drawn or counted. A quarter hour is in use when any of the source's servers served in it, so `inUse` is wall time,
 * a quarter hour two servers served counted once. `used` is how many days it was in use, `counted` how many have come since it
 * was added, and `failovers` how many its places hold. */
export type Week = {
  at: number
  days: { start: number; end: number }[]
  places: Place[]
  runs: UseRun[]
  inUse: number
  used: number
  counted: number
  failovers: number
}

const midnight = (t: number) => { const d = new Date(t); d.setHours(0, 0, 0, 0); return d.getTime() }
const daysBefore = (t: number, n: number) => { const d = new Date(t); d.setDate(d.getDate() - n); return d.getTime() }
const daysAfter = (t: number, n: number) => daysBefore(t, -n)

/** The seven days' local midnights, oldest first. */
export function weekStarts(now = Date.now()): number[] {
  const today = midnight(now)
  return [6, 5, 4, 3, 2, 1, 0].map((n) => daysBefore(today, n))
}

/** The local hours a day's places start at. */
const PLACE_HOURS = [0, 6, 12, 18]

/** Where a day's places start and its last ends: its 00, 06, 12 and 18 by the local clock, then the next day's midnight. */
const edgesOf = (day: number) => [...PLACE_HOURS.map((h) => new Date(day).setHours(h, 0, 0, 0)), daysAfter(day, 1)]

/** The analytics' step. The daemon starts each step at a multiple of it since 1970, and every zone's clock hours fall on one, so
 * a step lies inside one place. */
const STEP = 900
const STEP_MS = STEP * 1000

/** The read behind every source's week, all sources at once, so the list and each heading count the same failovers: steps from
 * the first of its days to the end of the place now is in, so its key stays the same within a place. */
export function weekRead(now = Date.now()): AnalyticsQuery {
  const to = edgesOf(midnight(now)).find((e) => e > now) ?? now
  return { from: new Date(weekStarts(now)[0]).toISOString(), to: new Date(to).toISOString(), bucketSeconds: STEP }
}

/** One source's week from the read of every source's, answered at `at`. A source whose added time isn't known counts the whole
 * week. */
export function weekOf(buckets: readonly AnalyticsBucket[], { id, addedAt }: Pick<SourceView, "id" | "addedAt">, at: number): Week {
  const days = weekStarts(at).map((start) => ({ start, end: daysAfter(start, 1) }))
  const added = addedAt === null ? null : Date.parse(addedAt)
  const places: Place[] = days.flatMap(({ start }) => {
    const edges = edgesOf(start)
    return PLACE_HOURS.map((_, i) => ({ start: edges[i], end: edges[i + 1], failovers: 0, inUse: 0 }))
  })
  const counts = (p: Place) => p.start < at && (added === null || p.end > added)
  const used = new Set<number>()
  for (const b of buckets) {
    const step = Date.parse(b.start)
    const place = b.source === id && step < at && places.find((p) => p.start <= step && step < p.end)
    if (!place || !counts(place)) continue
    place.failovers += b.forcedSwitchAways
    if (b.servedSeconds > 0) used.add(step)
  }
  const runs: UseRun[] = []
  for (const step of [...used].toSorted((a, b) => a - b)) {
    const to = Math.min(step + STEP_MS, at)
    const place = places.find((p) => p.start <= step && step < p.end)
    if (place) place.inUse += to - step
    const last = runs.at(-1)
    if (last?.to === step) last.to = to
    else runs.push({ from: step, to })
  }
  const inDay = (day: Week["days"][number]) => places.filter((p) => p.start >= day.start && p.start < day.end)
  const come = days.filter((day) => inDay(day).some(counts))
  return {
    at, days, places, runs,
    inUse: places.reduce((n, p) => n + p.inUse, 0),
    used: come.filter((day) => inDay(day).some((p) => p.inUse > 0)).length,
    counted: come.length,
    failovers: places.reduce((n, p) => n + p.failovers, 0),
  }
}

/** A mark's left and right edge in px. */
export type Edges = { x0: number; x1: number }

/** Where a week's marks stand across `width` px, its seven days `gap` px apart, each edge on a whole pixel: each day's line, ending
 * at the read today; each run's piece in each day it touches, at least a pixel wide; each failover at the middle of the part of
 * its place that has come; each place that has started, for its hover; and the middle of each whole day, for its letter. */
export function weekLayout(week: Week, width: number, gap: number) {
  const span = (width - 6 * gap) / 7
  const x = (d: number, t: number) => { const { start, end } = week.days[d]; return Math.round(d * (span + gap) + ((t - start) / (end - start)) * span) }
  const dayOf = (t: number) => week.days.findIndex((day) => day.start <= t && t < day.end)
  const days: Edges[] = week.days.map((day, d) => ({ x0: x(d, day.start), x1: x(d, Math.min(day.end, week.at)) }))
  const runs: Edges[] = week.runs.flatMap((run) => week.days.flatMap((day, d) => {
    if (run.to <= day.start || run.from >= day.end) return []
    const x0 = x(d, Math.max(run.from, day.start))
    return [{ x0, x1: Math.max(x0 + 1, x(d, Math.min(run.to, day.end))) }]
  }))
  const started = week.places.filter((p) => p.start < week.at)
  const shown = (p: Place) => ({ start: p.start, end: Math.min(p.end, week.at) })
  return {
    days,
    runs,
    failovers: started.filter((p) => p.failovers > 0).map((p) => { const { start, end } = shown(p); return x(dayOf(start), (start + end) / 2) }),
    places: started.map((place) => ({ place, x0: x(dayOf(place.start), place.start), x1: x(dayOf(place.start), shown(place).end) })),
    letters: week.days.map((_, d) => d * (span + gap) + span / 2),
  }
}

/** The week as a screen reader hears it: "2 failovers in the last 7 days, in use on 6 of them". */
export function weekSpoken({ used, counted: days, failovers }: Pick<Week, "used" | "counted" | "failovers">): string {
  const when = days === 1 ? "today" : `in the last ${days} days`
  const use = used === 0 ? "not in use" : days === 1 ? "in use" : `in use on ${used} of them`
  return `${failovers === 0 ? "No failovers" : counted(failovers, "failover")} ${when}, ${use}`
}

/** When a failing source's refresh last worked, as far as the daemon's reads prove it: at a time, before one, or never. */
export type LastWorked = { kind: "at" | "before"; at: number } | { kind: "never" } | null

/** When a refresh last worked, from the refreshes the daemon sent and its fetch state. The refreshes are its newest 50 of those it
 * keeps, and it deletes them past its retention, so only a refresh that worked among them gives the time. Otherwise one last
 * worked before both the oldest sent and `checkedAt`, its last try. */
export function lastWorked(refreshes: readonly RefreshEvent[], { checkedAt, updatedAt }: Remote["fetch"]): LastWorked {
  const worked = refreshes.find((r) => r.outcome !== "failed")
  if (worked) return { kind: "at", at: Date.parse(worked.at) }
  const tries = [refreshes.at(-1)?.at, checkedAt].flatMap((t) => (t ? [Date.parse(t)] : []))
  return updatedAt === null ? { kind: "never" } : tries.length > 0 ? { kind: "before", at: Math.min(...tries) } : null
}

/** When a failing refresh last worked, as its Last worked row draws it and as a screen reader hears it after the row's name. */
export type WorkedMark = { shown: string; heard: string }

/** When a failing refresh last worked: a dash if never or not known, each heard as what it is. A bound under a minute says
 * nothing the reason doesn't, so it waits for the minute. */
export function workedMark(lastWorked: LastWorked, now = Date.now()): WorkedMark {
  const over = lastWorked?.kind === "before" ? spanUnder(now - lastWorked.at) : null
  const exact = lastWorked?.kind === "at" ? spanOf(Math.max(0, now - lastWorked.at)) : null
  return exact ? { shown: `${spanWords(exact)} ago`, heard: `${spanSpoken(exact)} ago` }
    : over ? { shown: `${spanWords(over)}+ ago`, heard: `over ${spanSpoken(over)} ago` }
    : lastWorked?.kind === "never" ? { shown: "—", heard: "never" }
    : { shown: "—", heard: "not known" }
}
