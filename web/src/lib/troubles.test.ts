import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { describe, test } from "node:test"
import type { AnalyticsBucket, LineageId, Quota, RefreshEvent, ServerView, SourceView } from "./schema.ts"
import { groupState, lastWorked, standingOf, unifiStanding, weekLayout, weekOf, weekRead, weekSpoken, workedMark, type Week } from "./troubles.ts"

const sample = <T>(name: string): T => JSON.parse(readFileSync(new URL(`../../tests/wire/${name}.json`, import.meta.url), "utf8")) as T
const REMOTE = sample<SourceView>("SourceView.remote")
const MANUAL = sample<SourceView>("SourceView.manual")
const NOW = new Date(2026, 10, 13, 15, 0).getTime()
const at = (hours: number) => new Date(NOW + hours * 3600e3).toISOString()
const GB = 1024 ** 3

const health: ServerView["health"] = {
  verdict: "succeeded", checkedAt: at(-0.1), pingAt: at(-0.1), current: true, pingP50Ms: 80, pingP95Ms: 110,
  newConnectionP50Ms: 240, newConnectionP95Ms: 300, score: 87.2, factor: 1.09, incidents7d: 1, seenDays: 12, seenWorking7dMs: 7 * 86_400_000, lastFailureAt: "2026-11-12T10:00:00Z", firstSeenAt: "2026-10-01T00:00:00Z",
}
const server = (id: string, patch: Partial<ServerView["health"]> = {}) =>
  ({ id: id as LineageId, presence: "present", supported: true, udp: "passed", health: { ...health, ...patch } }) as ServerView
const down = (id: string) => server(id, { verdict: "failed" })
const unchecked = (id: string) => server(id, { verdict: "unknown", current: false, checkedAt: null })
const quiet = (id: string) => server(id, { current: false, checkedAt: at(-1) })
const UP = [server("a"), server("b")]

const NO_QUOTA: Quota = { upload: null, download: null, limit: null, expires: null, refills: null, observedAt: null }
type Fetch = Extract<SourceView["origin"], { kind: "remote" }>["fetch"]
const WORKING: Fetch = { checkedAt: at(-1), updatedAt: at(-1), failures: 0, failureCode: null }

/** A subscription whose refreshes work, with no traffic or expiry sent unless asked. */
function sub({ quota = {}, fetch = {} }: { quota?: Partial<Quota>; fetch?: Partial<Fetch> } = {}): SourceView {
  if (REMOTE.origin.kind !== "remote") throw new Error("SourceView.remote isn't remote")
  return { ...REMOTE, origin: { ...REMOTE.origin, fetch: { ...WORKING, ...fetch }, provider: { ...REMOTE.origin.provider, quota: { ...NO_QUOTA, ...quota } } } }
}
const of = (source: SourceView, servers: ServerView[] | null = UP, failovers: number | null = 0) => standingOf(source, servers, failovers, NOW)
const troubles = (source: SourceView, servers: ServerView[] | null = UP, failovers: number | null = 0) => of(source, servers, failovers).troubles.map((t) => `${t.tone} ${t.words}`)
const traffic = (quota: Partial<Quota>) => of(sub({ quota })).facts?.traffic

describe("a source's failovers in a week", () => {
  test("under 7 are no trouble, 7 to 20 amber and 21 or more red, in the dot and in words", () => {
    assert.deepEqual([troubles(sub(), UP, 6), of(sub(), UP, 6).dot.rating], [[], "good"])
    assert.deepEqual([troubles(sub(), UP, 7), of(sub(), UP, 7).dot], [["warn 7 failovers in 7d"], { kind: "rated", rating: "unstable", why: ["7 failovers in 7d"] }])
    assert.deepEqual(troubles(sub(), UP, 20), ["warn 20 failovers in 7d"])
    assert.deepEqual([troubles(sub(), UP, 21), of(sub(), UP, 21).dot.rating], [["bad 21 failovers in 7d"], "bad"])
  })
  test("are the week's, not the moves the daemon counts for the source's own slots, and count for nothing until the week is read", () => {
    assert.deepEqual(troubles({ ...sub(), moves7d: 30 }, UP, 7), ["warn 7 failovers in 7d"])
    assert.deepEqual([troubles({ ...sub(), moves7d: 30 }, UP, null), of(sub(), UP, null).dot.rating], [[], "good"])
  })
})

describe("a source's traffic", () => {
  const limit = 100 * GB
  const left = (quota: Partial<Quota>) => { const t = traffic(quota); assert.equal(t?.kind, "left"); return t as Extract<typeof t, { kind: "left" }> }
  test("10% left is no trouble, and under 10% is amber, with the share left for its bar", () => {
    assert.deepEqual([left({ download: 90 * GB, limit }).left, left({ download: 90 * GB, limit }).share], [10 * GB, 0.1])
    assert.deepEqual(troubles(sub({ quota: { download: 90 * GB, limit } })), [])
    const low = of(sub({ quota: { upload: 1 * GB, download: 90 * GB, limit } }))
    assert.deepEqual(low.troubles, [{ kind: "traffic", tone: "warn", words: "9.0 GB left" }])
    assert.equal(low.dot.rating, "unstable")
  })
  test("at the limit or over it is red, 0 GB left in the limit's unit, with an empty bar", () => {
    for (const download of [limit, limit + GB]) {
      assert.deepEqual([left({ download, limit }).left, left({ download, limit }).share], [0, 0])
      assert.deepEqual(troubles(sub({ quota: { download, limit } })), ["bad 0 GB left"])
    }
  })
  test("a limit of zero, or none sent, is no limit: no traffic fact and never used up", () => {
    for (const none of [0, null]) {
      assert.deepEqual(traffic({ download: 500 * GB, limit: none }), { kind: "unlimited" })
      assert.deepEqual(troubles(sub({ quota: { download: 500 * GB, limit: none } })), [])
    }
  })
  test("traffic not sent is unreported", () => {
    assert.deepEqual(traffic({ limit }), { kind: "unreported" })
  })
  test("a refill shows only when it comes before the expiry, and counts hours on its last day", () => {
    assert.deepEqual(left({ download: GB, limit, refills: at(24 * 10), expires: at(24 * 40) }).refill, { n: 10, unit: "d" })
    assert.deepEqual(left({ download: GB, limit, refills: at(24 * 10) }).refill, { n: 10, unit: "d" })
    assert.deepEqual(left({ download: GB, limit, refills: at(5) }).refill, { n: 5, unit: "h" })
    assert.equal(left({ download: GB, limit, refills: at(24 * 40), expires: at(24 * 10) }).refill, null)
  })
  test("a reading over a day old says its age, and a newer one doesn't", () => {
    assert.deepEqual(left({ download: GB, limit, observedAt: at(-25) }).old, { n: 1, unit: "d" })
    assert.equal(left({ download: GB, limit, observedAt: at(-23) }).old, null)
  })
  test("a reading taken before a refill date that has since passed says nothing of what is left, and is no trouble, however new", () => {
    const stale = sub({ quota: { download: limit, limit, refills: at(-24), observedAt: at(-72) } })
    assert.deepEqual(of(stale).facts?.traffic, { kind: "unknown", limit, due: at(-24), read: { n: 3, unit: "d" } })
    assert.deepEqual([troubles(stale), of(stale).dot.rating], [[], "good"])
    assert.equal(traffic({ download: limit, limit, refills: at(-1), observedAt: at(-2) })?.kind, "unknown")
    assert.deepEqual(troubles(sub({ quota: { download: limit, limit, refills: at(-24), observedAt: at(-1) } })), ["bad 0 GB left"], "a reading taken after it stands")
  })
})

describe("a source's expiry", () => {
  test("from 7 days it is amber, and before that no trouble", () => {
    assert.deepEqual(troubles(sub({ quota: { expires: at(24 * 8) } })), [])
    assert.deepEqual(troubles(sub({ quota: { expires: at(24 * 7) } })), ["warn Expires in 7d"])
    assert.deepEqual(troubles(sub({ quota: { expires: at(12) } })), ["warn Expires in 12h"])
  })
  test("in months from 60 days, and a year or more away it is far, which the heading doesn't show", () => {
    assert.deepEqual(of(sub({ quota: { expires: at(24 * 310) } })).facts?.expiry, { kind: "expires", at: at(24 * 310), in: { n: 10, unit: "mo" }, soon: false })
    assert.deepEqual(of(sub({ quota: { expires: at(24 * 366) } })).facts?.expiry, { kind: "far", at: at(24 * 366) })
  })
  test("once passed it is red, and no traffic shows", () => {
    const expired = of(sub({ quota: { expires: at(-48), download: 200 * GB, limit: 100 * GB } }))
    assert.deepEqual(expired.troubles.map((t) => `${t.tone} ${t.words}`), ["bad Expired 2d ago"])
    assert.equal(expired.facts?.traffic, null)
    assert.equal(expired.dot.rating, "bad")
  })
  test("none sent is unreported", () => {
    assert.deepEqual(of(sub()).facts?.expiry, { kind: "unreported" })
  })
})

describe("a source's servers", () => {
  test("one up and the rest never checked is no trouble, and the dot is green", () => {
    const one = of(sub(), [server("a"), unchecked("b"), unchecked("c")])
    assert.deepEqual([one.troubles, one.dot], [[], { kind: "rated", rating: "good", why: [] }])
  })
  test("none rated lately, or none ever checked, is no trouble, and the dot is grey", () => {
    for (const servers of [[quiet("a"), quiet("b")], [unchecked("a"), unchecked("b")], [quiet("a"), unchecked("b")]]) {
      const none = of(sub(), servers)
      assert.deepEqual([none.troubles, none.dot], [[], { kind: "rated", rating: "none", why: [] }])
    }
  })
  test("every server down is red, and one down while another is up or not rated is no trouble", () => {
    assert.deepEqual([troubles(sub(), [down("a"), down("b")]), of(sub(), [down("a"), down("b")]).dot.rating], [["bad No server up"], "bad"])
    assert.deepEqual(troubles(sub(), [server("a"), down("b")]), [])
    assert.deepEqual(troubles(sub(), [quiet("a"), down("b")]), [])
    assert.deepEqual(troubles(sub(), [unchecked("a"), down("b")]), [])
    assert.deepEqual(troubles(sub(), [server("a")]), [])
  })
  test("none at all is red for a pasted source, for a subscription once a refresh worked, and for one whose refreshes failed before any worked", () => {
    assert.deepEqual(troubles(MANUAL, []), ["bad No servers"])
    assert.deepEqual(troubles(sub(), []), ["bad No servers"])
    assert.deepEqual(troubles(sub({ fetch: { updatedAt: null, checkedAt: at(-0.1), failures: 1, failureCode: "timeout" } }), []), ["bad No servers", "warn Refresh failing"])
  })
  test("while its first refresh runs a subscription has no trouble and its dot is Pending", () => {
    const first = of(sub({ fetch: { updatedAt: null, checkedAt: null } }), [])
    assert.deepEqual([first.fetched, first.troubles, first.dot], ["first", [], { kind: "first", rating: "none", why: [] }])
    assert.equal(of(sub(), []).fetched, "worked")
    assert.equal(of(MANUAL, []).fetched, "worked")
    assert.equal(of(sub({ fetch: { updatedAt: null, checkedAt: at(-0.1), failures: 1, failureCode: "timeout" } }), []).fetched, "never")
  })
  test("until the server list is read, no source reads as having none", () => {
    assert.deepEqual([troubles(MANUAL, null), troubles(sub(), null)], [[], []])
  })
  test("the dot follows no first choice: a source whose first server is down and the rest up is Online", () => {
    const first = { ...sub(), firstChoice: "a" as LineageId }
    assert.equal(of(first, [down("a"), server("b"), server("c")]).dot.rating, "good")
  })
})

describe("a source's refreshes", () => {
  test("every trouble at once comes in one order, servers, expiry, traffic, failovers, refresh, whatever its colour", () => {
    const everything = sub({ quota: { expires: at(7), download: 100 * GB, limit: 100 * GB }, fetch: { failures: 2, failureCode: "http_503" } })
    assert.deepEqual(troubles(everything, [down("a"), down("b")], 13), ["bad No server up", "warn Expires in 7h", "bad 0 GB left", "warn 13 failovers in 7d", "warn Refresh failing"])
  })
  test("until one worked, a subscription has no facts, and a failing one says it has no servers and its refresh fails", () => {
    const never = of(sub({ fetch: { updatedAt: null, checkedAt: at(-0.1), failures: 1, failureCode: "timeout" }, quota: { expires: at(-1) } }), [])
    assert.deepEqual([never.facts, never.troubles.map((t) => t.words), never.dot.rating], [null, ["No servers", "Refresh failing"], "bad"])
    assert.deepEqual(never.refresh, { failing: true, code: "timeout" })
  })
  test("a pasted source has no refresh, expiry or traffic", () => {
    const pasted = of(MANUAL)
    assert.deepEqual([pasted.refresh, pasted.facts?.expiry, pasted.facts?.traffic], [null, null, null])
  })
  const event = (hours: number, outcome: RefreshEvent["outcome"], wanExcluded = false): RefreshEvent =>
    ({ at: at(-hours), outcome, failureCode: null, wanExcluded, added: 0, changed: 0, missing: 0 })
  const failed = (n: number, every = 6) => Array.from({ length: n }, (_, i) => event(every * i, "failed"))
  /** When a refresh last worked, as the failure card shows it. */
  const worked = (refreshes: RefreshEvent[], fetch: Partial<Fetch> = {}, now = NOW) =>
    workedMark(lastWorked(refreshes, { ...WORKING, updatedAt: at(-24 * 60), failures: 1, ...fetch }), now).shown
  test("a refresh that worked among those sent says when it last worked", () => {
    assert.equal(worked([event(1, "failed"), event(2, "failed", true), event(3, "failed"), event(4, "unchanged"), event(5, "failed")]), "4h ago")
    assert.equal(worked([event(1, "failed"), event(2, "updated")]), "2h ago")
  })
  test("a source that never worked has the dash, whatever the daemon kept of its tries", () => {
    assert.equal(worked(failed(1), { updatedAt: null }), "—")
    assert.equal(worked([], { updatedAt: null, checkedAt: at(-3) }), "—", "all deleted by retention")
  })
  test("with none that worked among the 50 the daemon sends, it last worked before the oldest sent, never at the list's last change", () => {
    assert.equal(worked([...failed(51), event(6 * 51, "unchanged")].slice(0, 50)), "12d+ ago")
  })
  test("an empty history says only what the last try bounds", () => {
    assert.equal(worked([], { checkedAt: at(-24 * 40), updatedAt: at(-24 * 50) }), "40d+ ago", "a paused source whose whole history has expired")
    assert.equal(worked([], { checkedAt: at(-3) }), "3h+ ago", "never the list's last change")
  })
  test("a refresh last worked before both the oldest sent and the last try, and over says no more than that", () => {
    assert.equal(worked([event(1, "failed", true)], { checkedAt: at(-5) }), "5h+ ago", "a try the WAN explains leaves the last try alone")
    assert.equal(worked([event(24 * 11.9, "failed")], { checkedAt: at(-1) }), "11d+ ago")
    assert.equal(worked([], { checkedAt: new Date(NOW - 30e3).toISOString() }), "—", "under a minute, no bound worth its line")
    assert.equal(worked([], { checkedAt: new Date(NOW - 30e3).toISOString() }, NOW + 60e3), "1m+ ago")
    assert.equal(worked([], { checkedAt: null }), "—", "a source never tried has no time to bound")
  })
})

describe("a failing refresh's Last worked, as a screen reader hears it", () => {
  const heard = (run: Parameters<typeof workedMark>[0]) => workedMark(run, NOW).heard
  test("says when a refresh last worked in words, its units whole", () => {
    assert.equal(heard({ kind: "at", at: NOW - 7 * 3600e3 }), "7 hours ago")
    assert.equal(heard({ kind: "before", at: NOW - 2 * 86_400_000 }), "over 2 days ago")
  })
  test("the one dash is heard as never or as not known", () => {
    assert.deepEqual([workedMark({ kind: "never" }, NOW), workedMark(null, NOW)], [{ shown: "—", heard: "never" }, { shown: "—", heard: "not known" }])
    assert.deepEqual(workedMark({ kind: "before", at: NOW - 30e3 }, NOW), { shown: "—", heard: "not known" }, "a bound under a minute")
  })
})

describe("a source's week", () => {
  const Q = 900e3
  const HOUR = 3600e3
  const inZone = (tz: string, run: () => void) => {
    const zone = process.env.TZ
    process.env.TZ = tz
    try { run() } finally { if (zone === undefined) delete process.env.TZ; else process.env.TZ = zone }
  }
  /** Zones a whole hour, half an hour and three quarters off UTC, and one with clock changes. */
  const ZONES = ["UTC", "Europe/Moscow", "Asia/Kolkata", "Pacific/Chatham", "America/Denver"]
  /** A local time on a day of November 2026 in the zone the test runs in. The 17th is a Tuesday. */
  const on = (day: number, hm: string) => { const [h, m] = hm.split(":").map(Number); return new Date(2026, 10, day, h, m).getTime() }
  const bucket = (start: number, servedSeconds: number, forcedSwitchAways = 0, lineage = "a", source = "s") =>
    ({ start: new Date(start).toISOString(), source, lineage, servedSeconds, forcedSwitchAways }) as AnalyticsBucket
  /** A bucket for each quarter hour from `from` to `to`, served for a minute of it. */
  const served = (from: number, to: number, lineage = "a") => { const out: AnalyticsBucket[] = []; for (let t = from; t < to; t += Q) out.push(bucket(t, 60, 0, lineage)); return out }
  const source = (addedAt: number | null = null) => ({ id: "s" as SourceView["id"], addedAt: addedAt === null ? null : new Date(addedAt).toISOString() })
  const hm = (t: number) => `${new Date(t).getDate()} ${new Date(t).toTimeString().slice(0, 5)}`
  const runsOf = (week: Week) => week.runs.map((r) => `${hm(r.from)}–${hm(r.to)}`)
  const placeOf = (week: Week, start: number) => week.places.find((p) => p.start === start)
  /** Case 1 of the drawing: added Monday 06:20, a bad morning, and read on Tuesday at 20:30. */
  const monday = () => [
    ...served(on(16, "06:30"), on(16, "09:30")), ...served(on(16, "10:00"), on(16, "13:30")), ...served(on(16, "14:00"), on(16, "23:15")), ...served(on(17, "00:45"), on(17, "07:00")),
    bucket(on(16, "07:00"), 60, 3), bucket(on(16, "08:15"), 60, 4), bucket(on(16, "14:00"), 60, 1),
  ]

  test("is the stretches of quarter hours in use, each broken where a quarter hour wasn't, in every zone", () => {
    for (const tz of ZONES) inZone(tz, () => {
      const week = weekOf(monday(), source(on(16, "06:20")), on(17, "20:30"))
      assert.deepEqual(runsOf(week), ["16 06:30–16 09:30", "16 10:00–16 13:30", "16 14:00–16 23:15", "17 00:45–17 07:00"], tz)
      assert.equal(week.inUse, 22 * HOUR, `${tz}: in use is the quarter hours in use`)
      assert.deepEqual([placeOf(week, on(16, "06:00"))?.inUse, placeOf(week, on(16, "06:00"))?.failovers], [5 * HOUR, 7], `${tz}: Monday 06 to 12`)
      assert.equal(week.failovers, 8, tz)
      assert.equal(weekSpoken(week), "8 failovers in the last 2 days, in use on 2 of them", tz)
    })
  })
  test("counts a quarter hour two of the source's servers served once, and takes no other source's", () => {
    inZone("Europe/Moscow", () => {
      const buckets = [...served(on(17, "06:00"), on(17, "08:00"), "a"), ...served(on(17, "07:00"), on(17, "09:00"), "b"), bucket(on(17, "12:00"), 900, 5, "c", "other")]
      const week = weekOf(buckets, source(), on(17, "20:30"))
      assert.deepEqual(runsOf(week), ["17 06:00–17 09:00"], "the union of its servers' quarter hours")
      assert.deepEqual([week.inUse, week.failovers], [3 * HOUR, 0])
    })
  })
  test("ends at the moment its read was answered: the quarter hour in progress counts up to it, and nothing after it counts", () => {
    for (const tz of ZONES) inZone(tz, () => {
      const at = on(17, "09:07")
      const week = weekOf([...served(on(17, "08:00"), on(17, "09:30")), bucket(on(17, "09:15"), 60, 2)], source(), at)
      assert.deepEqual(runsOf(week), ["17 08:00–17 09:07"], tz)
      assert.equal(week.inUse, 67 * 60e3, `${tz}: an hour and the seven minutes of the quarter hour in progress`)
      assert.equal(week.failovers, 0, `${tz}: no failover after the read`)
      assert.ok(week.places.filter((p) => p.start >= at).every((p) => p.inUse === 0 && p.failovers === 0), `${tz}: the places to come hold nothing`)
    })
  })
  test("is always the seven local days ending today, and what came before the source was added counts nothing", () => {
    for (const tz of ZONES) inZone(tz, () => {
      const week = weekOf([bucket(on(16, "03:00"), 60, 4), ...monday()], source(on(16, "06:20")), on(17, "20:30"))
      assert.deepEqual(week.days.map((d) => new Date(d.start).toDateString()), [11, 12, 13, 14, 15, 16, 17].map((d) => new Date(2026, 10, d).toDateString()), tz)
      assert.deepEqual(week.places.map((p) => new Date(p.start).getHours()), week.days.flatMap(() => [0, 6, 12, 18]), `${tz}: four places a day, from 00, 06, 12 and 18`)
      assert.equal(week.failovers, 8, `${tz}: Monday 00 to 06 ended before the source was added`)
      assert.equal(weekSpoken(weekOf([], source(on(16, "06:20")), on(17, "20:30"))), "No failovers in the last 2 days, not in use", tz)
      assert.equal(weekSpoken(weekOf([], source(on(17, "06:20")), on(17, "20:30"))), "No failovers today, not in use", tz)
      assert.equal(weekSpoken(weekOf(served(on(12, "10:00"), on(12, "11:00")), source(null), on(17, "20:30"))), "No failovers in the last 7 days, in use on 1 of them", tz)
    })
  })
  test("a day the clocks change on has four places, one an hour short or long, and every quarter hour of it counts", () => {
    inZone("America/Denver", () => {
      for (const { day, first } of [{ day: new Date(2026, 10, 1), first: 7 }, { day: new Date(2026, 2, 8), first: 5 }]) {
        const next = new Date(day).setDate(day.getDate() + 1)
        const steps: AnalyticsBucket[] = []
        for (let t = day.getTime(); t < next; t += Q) steps.push(bucket(t, 60, 1))
        const week = weekOf(steps, source(), next + 12 * HOUR)
        const places = week.places.filter((p) => p.start >= day.getTime() && p.start < next)
        assert.deepEqual(places.map((p) => (p.end - p.start) / HOUR), [first, 6, 6, 6], `${day.toDateString()}: its first place holds the hour the clocks changed`)
        assert.equal(places.reduce((n, p) => n + p.failovers, 0), steps.length, `${day.toDateString()}: all ${steps.length} quarter hours count`)
        assert.equal(week.inUse, next - day.getTime(), `${day.toDateString()}: in use the whole day`)
      }
    })
  })
  test("reads quarter hours from the first day's local midnight to the end of the place now is in, in a zone half an hour off UTC", () => {
    inZone("Asia/Kolkata", () => {
      const now = new Date(2026, 10, 16, 13, 20).getTime()
      assert.deepEqual(weekRead(now), { from: new Date(2026, 10, 10).toISOString(), to: new Date(2026, 10, 16, 18).toISOString(), bucketSeconds: 900 })
      assert.equal(weekRead(new Date(2026, 10, 16, 17, 59).getTime()).to, weekRead(now).to, "the same read within a place")
    })
  })
})

describe("a source's week as drawn", () => {
  const Q = 900e3
  const inZone = (tz: string, run: () => void) => {
    const zone = process.env.TZ
    process.env.TZ = tz
    try { run() } finally { if (zone === undefined) delete process.env.TZ; else process.env.TZ = zone }
  }
  const on = (day: number, hm: string) => { const [h, m] = hm.split(":").map(Number); return new Date(2026, 10, day, h, m).getTime() }
  const bucket = (start: number, servedSeconds: number, forcedSwitchAways = 0) =>
    ({ start: new Date(start).toISOString(), source: "s", lineage: "a", servedSeconds, forcedSwitchAways }) as AnalyticsBucket
  const served = (from: number, to: number) => { const out: AnalyticsBucket[] = []; for (let t = from; t < to; t += Q) out.push(bucket(t, 60)); return out }
  const source = { id: "s" as SourceView["id"], addedAt: null }
  const whole = (layout: ReturnType<typeof weekLayout>) => [...layout.days, ...layout.runs, ...layout.places].flatMap((m) => [m.x0, m.x1]).concat(layout.failovers)

  test("in the heading's 100 px, seven days 2 px apart on whole pixels, today ending at the read, and a run at least a pixel wide", () => {
    for (const tz of ["UTC", "Europe/Moscow", "Pacific/Chatham", "America/Denver"]) inZone(tz, () => {
      for (const hour of [0, 3, 9, 13, 20, 23]) {
        const at = on(17, `${hour}:30`)
        const week = weekOf([bucket(on(14, "10:00"), 60), ...served(on(15, "00:00"), on(15, "00:30"))], source, at)
        const layout = weekLayout(week, 100, 2)
        const label = `${tz} at ${hour}:30`
        assert.ok(whole(layout).every(Number.isInteger), `${label}: every edge on a whole pixel`)
        assert.deepEqual(layout.days.slice(0, 6).map((d) => [d.x0, d.x1]), [0, 1, 2, 3, 4, 5].map((i) => [Math.round(i * (88 / 7 + 2)), Math.round(i * (88 / 7 + 2) + 88 / 7)]), `${label}: six whole days`)
        assert.equal(layout.days[6].x1, Math.round(6 * (88 / 7 + 2) + ((hour + 0.5) / 24) * (88 / 7)), `${label}: today ends at the read`)
        assert.ok(layout.runs.every((r) => r.x1 - r.x0 >= 1), `${label}: a quarter hour is a pixel`)
        assert.ok(layout.places.every((p) => p.x1 <= layout.days[6].x1), `${label}: no place to hover after the read`)
      }
    })
  })
  test("a failover stands at the middle of the part of its six hours that has come", () => {
    inZone("Europe/Moscow", () => {
      const at = on(17, "20:30")
      const week = weekOf([bucket(on(16, "07:00"), 60, 7), bucket(on(17, "19:00"), 60, 1)], source, at)
      const span = 288 - 6 * 4
      const x = (day: number, hours: number) => Math.round(day * (span / 7 + 4) + (hours / 24) * (span / 7))
      assert.deepEqual(weekLayout(week, 288, 4).failovers, [x(5, 9), x(6, 19.25)], "Monday's at 09:00, and today's at the middle of 18:00 to 20:30")
      assert.deepEqual(weekLayout(week, 288, 4).letters, [0, 1, 2, 3, 4, 5, 6].map((d) => d * (span / 7 + 4) + span / 14), "each day's letter centred under its whole day")
    })
  })
  test("draws a run across midnight as a piece in each day it touches", () => {
    inZone("Asia/Kolkata", () => {
      const week = weekOf(served(on(15, "22:00"), on(16, "02:00")), source, on(17, "20:30"))
      assert.equal(week.runs.length, 1)
      const layout = weekLayout(week, 100, 2)
      assert.equal(layout.runs.length, 2)
      assert.deepEqual([layout.runs[0].x1, layout.runs[1].x0], [layout.days[4].x1, layout.days[5].x0], "each piece meets its day's edge")
    })
  })
})

describe("a group's dot", () => {
  const servers = new Map(UP.map((s) => [s.id, s]))
  const group = (moves7d: number) => groupState({ resolved: ["a", "b"] as LineageId[], firstChoice: "a" as LineageId, moves7d }, servers)
  test("is Online under 7 failovers in a week, Unstable from 7 and Offline from 21", () => {
    assert.equal(group(6).rating, "good")
    assert.deepEqual([group(7).rating, group(7).why], ["unstable", ["7 failovers in 7d"]])
    assert.deepEqual([group(20).rating, group(21).rating], ["unstable", "bad"])
    assert.deepEqual(group(21).why, ["21 failovers in 7d"])
  })
})

describe("a group's dot agrees with its rows", () => {
  const state = (servers: ServerView[], moves7d = 0) =>
    groupState({ resolved: servers.map((s) => s.id), firstChoice: servers[0]?.id ?? null, moves7d }, new Map(servers.map((s) => [s.id, s])))
  test("a group takes its first choice's dot", () => {
    assert.deepEqual([state(UP).up, state(UP).rating], [2, "good"])
  })
  test("a group whose first choice nothing probed lately is Unchecked, with the reason", () => {
    const quiet = server("a", { current: false, checkedAt: new Date(Date.now() - 3 * 3600e3).toISOString() })
    const quietFirst = state([quiet, server("b"), server("c")])
    assert.deepEqual([quietFirst.rating, quietFirst.why], ["none", ["First choice: Last checked 3h ago"]])
  })
  test("a group's own trouble wins when it is worse, and its list entry names its own troubles", () => {
    const none = state([down("a"), down("b")], 9)
    assert.deepEqual([none.rating, none.why, none.troubles.map((t) => `${t.tone} ${t.words}`)], ["bad", ["No server up", "9 failovers in 7d"], ["bad No server up", "warn 9 failovers in 7d"]])
    const one = state([server("a"), down("b")])
    assert.deepEqual([one.rating, one.why, one.troubles], ["good", [], []])
    const unknown = state([down("a"), quiet("b")])
    assert.deepEqual([unknown.rating, unknown.troubles], ["bad", []])
  })
  test("a group with no server is Offline, \"No servers\", as a source with none is", () => {
    const empty = state([], 0)
    assert.deepEqual([empty.rating, empty.troubles.map((t) => `${t.tone} ${t.words}`)], ["bad", ["bad No servers"]])
  })
})

describe("UniFi's standing", () => {
  test("is its trouble, with when the key last worked, before whether the key was answered", () => {
    const at = "2026-11-21T10:00:00Z"
    assert.deepEqual(unifiStanding({ unifiTrouble: "notAnswering", unifi: { acceptedAt: at } }), { kind: "trouble", trouble: "notAnswering", lastWorked: at })
    assert.deepEqual(unifiStanding({ unifiTrouble: null, unifi: { acceptedAt: at } }), { kind: "answered", at })
    assert.deepEqual(unifiStanding({ unifiTrouble: null, unifi: { acceptedAt: null } }), { kind: "unanswered" })
  })
})
