import assert from "node:assert/strict"
import { test } from "node:test"
import { dayTicks, lastWeek, serverHistory, serverWeek } from "./analytics.ts"
import type { AnalyticsBucket } from "./schema.ts"

const lineage = "a" as AnalyticsBucket["lineage"]
const bucket = (start: string, servedSeconds: number, forcedSwitchAways: number, patch: Partial<AnalyticsBucket> = {}) =>
  ({ start, source: "s" as AnalyticsBucket["source"], lineage, servedSeconds, forcedSwitchAways, probeSuccesses: 60, probeAttempts: 60, pingMedianMs: 10, pingP95Ms: 20, ...patch }) as AnalyticsBucket
const STEP = 3600
const at = (minutes: number) => new Date(Date.UTC(2026, 0, 1, 0, minutes)).toISOString()

test("a step with no bucket stays an empty point", () => {
  const points = serverHistory([bucket(at(0), 3600, 0), bucket(at(120), 3600, 0)], lineage, at(0), at(180), STEP)
  assert.deepEqual(points.map((p) => p.pingMs), [10, null, 10])
})

test("a week opens a card for its failovers or its readings, and none for a server only served or only failing checks", () => {
  const unread = (patch: Partial<AnalyticsBucket>) => serverHistory([bucket(at(0), 3600, 0, { probeSuccesses: 0, pingMedianMs: null, pingP95Ms: null, ...patch })], lineage, at(0), at(60), STEP)
  assert.equal(serverWeek(unread({})), "empty")
  assert.equal(serverWeek(unread({ forcedSwitchAways: 2 })), "facts")
  assert.equal(serverWeek(serverHistory([], lineage, at(0), at(60), STEP)), "empty")
  assert.equal(serverWeek(serverHistory([bucket(at(0), 0, 0)], lineage, at(0), at(60), STEP)), "chart")
})

const inZone = (tz: string, run: () => void) => {
  const zone = process.env.TZ
  process.env.TZ = tz
  try { run() } finally { if (zone === undefined) delete process.env.TZ; else process.env.TZ = zone }
}
const SIX_HOURS = 6 * 3600
/** A server's week as its card draws it at `now`: each day letter, the local start of the bucket it stands at, and today's bold. */
const ticksAt = (now: number) => {
  const { from, to } = lastWeek(now)
  return dayTicks(serverHistory([], lineage, from, to, SIX_HOURS), now).map((t) => {
    const start = new Date(t.start)
    return `${t.today ? "*" : ""}${t.letter} ${start.toDateString().slice(0, 3)} ${start.toTimeString().slice(0, 5)}`
  })
}

test("a server's week names the seven local dates ending today, each at the bucket that holds its noon, at UTC+3 at 20:30", () => {
  inZone("Europe/Moscow", () => {
    assert.deepEqual(ticksAt(new Date(2026, 10, 17, 20, 30).getTime()),
      ["We Wed 09:00", "Th Thu 09:00", "Fr Fri 09:00", "Sa Sat 09:00", "Su Sun 09:00", "Mo Mon 09:00", "*Tu Tue 09:00"])
  })
})

test("before today's noon is in the window, today's letter stands at the middle of the part of today the window holds, at UTC+3 at 08:30", () => {
  inZone("Europe/Moscow", () => {
    assert.deepEqual(ticksAt(new Date(2026, 10, 17, 8, 30).getTime()),
      ["We Wed 09:00", "Th Thu 09:00", "Fr Fri 09:00", "Sa Sat 09:00", "Su Sun 09:00", "Mo Mon 09:00", "*Tu Tue 03:00"])
    assert.deepEqual(ticksAt(new Date(2026, 10, 17, 9, 0).getTime()).at(-1), "*Tu Tue 09:00", "from 09:00 the window holds today's noon")
  })
})

test("a server's week has seven letters at any hour in any zone, in order, the last today's and bold", () => {
  for (const tz of ["UTC", "Europe/Moscow", "Asia/Kolkata", "Pacific/Chatham", "America/Denver", "Pacific/Honolulu", "Pacific/Kiritimati"]) inZone(tz, () => {
    for (let minutes = 0; minutes < 24 * 60; minutes += 25) {
      const now = new Date(2026, 10, 17, 0, minutes).getTime()
      const ticks = dayTicks(serverHistory([], lineage, lastWeek(now).from, lastWeek(now).to, SIX_HOURS), now)
      const label = `${tz} at ${new Date(now).toTimeString().slice(0, 5)}`
      assert.deepEqual(ticks.map((t) => t.letter), ["We", "Th", "Fr", "Sa", "Su", "Mo", "Tu"], label)
      assert.deepEqual(ticks.map((t) => t.today), [false, false, false, false, false, false, true], label)
      assert.ok(ticks.every((t, i) => i === 0 || Date.parse(t.start) > Date.parse(ticks[i - 1].start)), `${label}: one letter a bucket, in order`)
    }
  })
})
