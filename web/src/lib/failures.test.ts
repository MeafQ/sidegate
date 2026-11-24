process.env.TZ = "Asia/Tokyo"

import assert from "node:assert/strict"
import { describe, test } from "node:test"
import { barsOf, failedAt, stabilityOf } from "./failures.ts"
import type { ServerView } from "./schema.ts"

const [MIN, HOUR, DAY] = [60_000, 3_600_000, 86_400_000]
/** Friday 13 November 2026, 12:00 in Tokyo. */
const NOW = Date.parse("2026-11-13T03:00:00Z")
const ago = (ms: number) => new Date(NOW - ms).toISOString()
const THURSDAY = "2026-11-12T05:10:00Z"

const health = (patch: Partial<ServerView["health"]> = {}): ServerView["health"] => ({
  verdict: "succeeded", checkedAt: ago(5 * MIN), pingAt: ago(5 * MIN), current: true, pingP50Ms: 40, pingP95Ms: 56, newConnectionP50Ms: 120,
  newConnectionP95Ms: 200, score: 42, factor: 1.04, incidents7d: 0, seenDays: 5, seenWorking7dMs: 7 * DAY, lastFailureAt: null, firstSeenAt: ago(30 * DAY),
  ...patch,
})
const of = (patch: Partial<ServerView["health"]> = {}) => stabilityOf(health(patch), NOW)
const words = (patch: Partial<ServerView["health"]> = {}) => of(patch).lines.map((l) => `${l.name} ${l.value}`)

describe("a server's stability", () => {
  test("halves the failures the daemon expects per day of use for each bar: four at a sixteenth or under, none above one a day", () => {
    assert.deepEqual([1, 1.0625, 1.07, 1.125, 1.13, 1.25, 1.26, 2, 2.01].map(barsOf), [4, 4, 3, 3, 2, 2, 1, 1, 0])
  })
  test("starts where every server starts, two of four, for one the daemon knows nothing of or has no record of", () => {
    assert.equal(of({ factor: 1.25, checkedAt: null }).bars, 2)
    assert.deepEqual(stabilityOf(null, NOW), {
      bars: 2, lines: [{ name: "Failures", value: "—", heard: "Not checked yet" }, { name: "Seen working", value: "—", heard: "" }], heard: "Stability 2 of 4. Not checked yet.",
    })
  })
  test("its card is the week's failures, the time seen working and the last failure, as names and values, before the week too", () => {
    assert.deepEqual(words(), ["Failures 0", "Seen working 7d"])
    assert.deepEqual(words({ seenWorking7dMs: 2 * DAY }), ["Failures 0", "Seen working 2d"])
    assert.deepEqual(words({ seenWorking7dMs: 12 * HOUR }), ["Failures 0", "Seen working 12h"])
    assert.deepEqual(words({ seenWorking7dMs: 20 * MIN }), ["Failures 0", "Seen working 20m"])
    assert.deepEqual(words({ incidents7d: 3, lastFailureAt: THURSDAY }), ["Failures 3", "Seen working 7d", "Last failure Thu 14:10"])
    assert.deepEqual(words({ lastFailureAt: "2026-11-06T12:15:00Z" }), ["Failures 0", "Seen working 7d", "Last failure Nov 6, 21:15"])
    assert.deepEqual(words({ incidents7d: 1204 }), ["Failures 1,204", "Seen working 7d"])
  })
  test("a dash where there is no reading: never checked, not checked in the week, or no time given; zero when none was seen", () => {
    assert.deepEqual(words({ incidents7d: 4, seenWorking7dMs: 0, lastFailureAt: "2026-11-13T00:12:00Z" }), ["Failures 4", "Seen working 0m", "Last failure today 09:12"])
    assert.deepEqual(words({ incidents7d: 2, seenWorking7dMs: null }), ["Failures 2", "Seen working —"])
    assert.deepEqual(words({ checkedAt: null }), ["Failures —", "Seen working —"])
    assert.deepEqual(words({ checkedAt: ago(7 * DAY), lastFailureAt: "2026-11-02T12:15:00Z" }), ["Failures —", "Seen working —", "Last failure Nov 2, 21:15"])
    assert.deepEqual(words({ checkedAt: ago(7 * DAY - MIN) }), ["Failures 0", "Seen working 7d"], "checked within the week")
  })
  test("a screen reader hears its level and its card, with no unit against a number", () => {
    assert.equal(of({ factor: 1.1, seenWorking7dMs: 2 * DAY }).heard, "Stability 3 of 4. No failure in the last 7 days. Seen working 2 days.")
    assert.equal(of({ factor: 3, checkedAt: ago(8 * DAY) }).heard, "Stability 0 of 4. Not checked in the last 7 days.")
    assert.equal(of({ seenWorking7dMs: 0 }).heard, "Stability 4 of 4. No failure in the last 7 days. Not seen working in the last 7 days.")
    assert.equal(of({ incidents7d: 2, seenWorking7dMs: null, lastFailureAt: THURSDAY }).heard, "Stability 4 of 4. 2 failures in the last 7 days. Time seen working not known. Last failure Thu 14:10.")
  })
  test("the last failure is in local time: today, a weekday within the week, a date before it, with its year more than 12 months back", () => {
    assert.equal(failedAt("2026-11-13T00:20:00Z", NOW), "today 09:20")
    assert.equal(failedAt(THURSDAY, NOW), "Thu 14:10")
    assert.equal(failedAt("2026-11-07T01:00:00Z", NOW), "Sat 10:00")
    assert.equal(failedAt("2026-11-06T14:59:00Z", NOW), "Nov 6, 23:59", "the same weekday a week back is a date")
    assert.equal(failedAt("2026-11-02T12:15:00Z", NOW), "Nov 2, 21:15")
    assert.equal(failedAt("2025-11-13T03:00:00Z", NOW), "Nov 13, 12:00", "12 months back to the minute keeps no year")
    assert.equal(failedAt("2025-11-12T03:00:00Z", NOW), "Nov 12, 2025, 12:00")
  })
})
