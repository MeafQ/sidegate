import assert from "node:assert/strict"
import { describe, test } from "node:test"
import { readFileSync } from "node:fs"
import { pingAge, rate, serverStanding, type ServerRating } from "./health.ts"
import type { LineageId, ServerView, SlotView } from "./schema.ts"
import { slotDot } from "./slots.ts"

const H = (hours: number) => new Date(Date.now() - hours * 3600e3).toISOString()
const health: ServerView["health"] = {
  verdict: "succeeded", checkedAt: H(0.1), pingAt: H(0.1), current: true, pingP50Ms: 80, pingP95Ms: 110,
  newConnectionP50Ms: 240, newConnectionP95Ms: 300, score: 87.2, factor: 1.09, incidents7d: 1, seenDays: 12, seenWorking7dMs: 7 * 86_400_000, lastFailureAt: "2026-11-12T10:00:00Z", firstSeenAt: "2026-10-01T00:00:00Z",
}
const server = (id: string, patch: Partial<ServerView["health"]> = {}) =>
  ({ id: id as LineageId, presence: "present", supported: true, udp: "passed", health: { ...health, ...patch } }) as ServerView

describe("a server's dot", () => {
  test("is green for a pass in the last 10 minutes, however often it failed this week and however its ping spreads", () => {
    assert.deepEqual(rate(server("a", { incidents7d: 40, factor: 3.4, pingP50Ms: 80, pingP95Ms: 900, newConnectionP95Ms: 9000 })), { rating: "good", why: [] })
  })
  test("is red for a failure in the last 10 minutes", () => {
    assert.deepEqual(rate(server("a", { verdict: "failed", checkedAt: H(0.05) })), { rating: "bad", why: ["Not responding"] })
  })
  test("is grey with nothing in the last 10 minutes, whatever came before, with when it was last checked", () => {
    for (const verdict of ["succeeded", "failed"] as const) {
      assert.deepEqual(rate(server("a", { verdict, current: false, checkedAt: H(12 * 24) })), { rating: "none", why: ["Last checked 12d ago"] })
    }
    assert.deepEqual(rate(server("a", { verdict: "unknown", current: false, checkedAt: null })), { rating: "none", why: [] })
  })
})

describe("a serving slot's dot", () => {
  const SERVING = JSON.parse(readFileSync(new URL("../../tests/wire/SlotView.serving.json", import.meta.url), "utf8")) as SlotView
  test("is its server's dot, with the server's reasons", () => {
    for (const patch of [{}, { incidents7d: 9 }, { verdict: "failed" as const }, { current: false }, { current: false, checkedAt: null }]) {
      const s = server("a", patch)
      assert.deepEqual(slotDot(SERVING, s).fill, { kind: "server", rating: rate(s).rating }, JSON.stringify(patch))
    }
    assert.deepEqual(slotDot(SERVING, server("a", { current: false, checkedAt: H(2) })), { fill: { kind: "server", rating: "none" }, words: "Serving · Unchecked · Last checked 2h ago", pulse: false })
    assert.deepEqual(slotDot(SERVING, server("a")), { fill: { kind: "server", rating: "good" }, words: "Serving", pulse: false })
  })
})

describe("a server's dot, its Health filter value and a slot serving on it", () => {
  const SERVING = JSON.parse(readFileSync(new URL("../../tests/wire/SlotView.serving.json", import.meta.url), "utf8")) as SlotView
  const missing = (patch: Partial<ServerView["health"]> = {}): ServerView => ({ ...server("a", patch), presence: "missing", missingSince: H(1) })
  const never = { verdict: "unknown", current: false, checkedAt: null, pingAt: null } as const
  const cases: [string, ServerView, ServerRating][] = [
    ["present, passed", server("a"), "good"],
    ["present, failed", server("a", { verdict: "failed" }), "bad"],
    ["stale", server("a", { current: false, checkedAt: H(3) }), "none"],
    ["missing after a recent pass", missing({ checkedAt: H(0.1) }), "good"],
    ["missing after a recent failure", missing({ verdict: "failed", checkedAt: H(0.1) }), "bad"],
    ["missing and stale", missing({ current: false, checkedAt: H(3) }), "none"],
    ["unsupported, never probed", { ...server("a", never), supported: false }, "none"],
  ]
  test("are one value, read from the last 10 minutes' probes whatever the presence or support", () => {
    for (const [name, s, want] of cases) {
      assert.equal(rate(s).rating, want, name)
      assert.deepEqual(slotDot(SERVING, s).fill, { kind: "server", rating: want }, name)
    }
  })
})

describe("a ping", () => {
  test("a ping says when it was measured only past a day", () => {
    assert.equal(pingAge(health), null)
    assert.equal(pingAge({ ...health, checkedAt: H(12 * 24), pingAt: H(12 * 24) }), "12d ago")
  })
  test("a heartbeat since doesn't make an old ping new", () => {
    assert.equal(pingAge({ ...health, checkedAt: H(0.01), pingAt: H(72) }), "3d ago")
  })
})

describe("a server's standing", () => {
  test("is Unsupported before Missing, so a server nothing takes says why wherever it is named", () => {
    const of = (presence: ServerView["presence"], supported: boolean) => serverStanding({ presence, supported })
    assert.equal(of("present", true), "usable")
    assert.equal(of("missing", true), "missing")
    assert.equal(of("tombstone", true), "tombstone")
    for (const presence of ["present", "missing", "tombstone"] as const) assert.equal(of(presence, false), "unsupported", presence)
  })
})
