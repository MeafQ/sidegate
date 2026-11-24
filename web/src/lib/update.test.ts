import assert from "node:assert/strict"
import { describe, test } from "node:test"
import { UpdateOutcome, UpdateReason, type UpdateView } from "./schema.ts"
import { moving, pressedSince, standingOf } from "./update.ts"

const NOTES = "https://example.test/OWNER/sidegate/releases/tag/v0.2.0"
const GUIDE = "https://example.test/OWNER/sidegate/blob/v0.1.0/docs/operate.md"
const offered = { version: "v0.2.0", restartsCore: false, notes: NOTES }
const view = (patch: Partial<UpdateView> = {}): UpdateView => ({ version: "v0.1.0", releases: true, guide: GUIDE, check: { at: null, failure: null }, offered: null, running: null, last: null, ...patch })
const last = (outcome: UpdateOutcome, reason: UpdateReason | null = null, version = "v0.2.0"): UpdateView["last"] => ({ attempt: "a", version, at: "2026-11-04T12:00:00Z", outcome, reason, message: null })

describe("About's line", () => {
  test("is the version and Check at rest, and on a development build the version alone", () => {
    assert.deepEqual(standingOf(view()), { version: "v0.1.0", to: null, ended: null, press: { kind: "check", unchecked: null }, guide: GUIDE })
    assert.deepEqual(standingOf(view({ version: "dev-7990743", releases: false, guide: null })), { version: "dev-7990743", to: null, ended: null, press: null, guide: null })
  })

  test("offers a newer release, and while one runs names it, with its notes only while it is the one offered", () => {
    const newer = standingOf(view({ offered }))
    assert.deepEqual([newer.to, newer.press], [{ version: "v0.2.0", notes: NOTES }, { kind: "update", offer: offered, running: false }])
    const running = (patch: Partial<UpdateView>) => standingOf(view({ running: { attempt: "a", version: "v0.2.0" }, ...patch }))
    assert.deepEqual([running({ offered }).to, running({ offered }).press], [{ version: "v0.2.0", notes: NOTES }, { kind: "update", offer: offered, running: true }])
    assert.deepEqual(running({}).to, { version: "v0.2.0", notes: null }, "over SSH, with nothing offered")
    assert.deepEqual(running({ offered: { ...offered, version: "v0.3.0" } }).to, { version: "v0.2.0", notes: null }, "another one offered")
    assert.equal(running({ version: "v0.2.0" }).to, null, "the new release already answers")
    assert.deepEqual(running({ version: "v0.2.0" }).press, { kind: "update", offer: null, running: true })
  })

  test("says why the last check learned nothing only beside Check", () => {
    const check = { at: "2026-11-04T12:00:00Z", failure: "unreachable" } as const
    assert.deepEqual(standingOf(view({ check })).press, { kind: "check", unchecked: { words: "Could not reach GitHub", at: check.at } })
    assert.equal(standingOf(view({ check, offered })).press?.kind, "update")
  })

  test("draws every end of an update to the release offered, each with words, and Retry where another try is all there is to do", () => {
    for (const outcome of UpdateOutcome.options) {
      const { ended, press } = standingOf(view({ offered, last: last(outcome, outcome === "failed" ? "checksum" : null) }))
      const want = { landed: [undefined, "update"], landedWithError: ["untidy", "update"], putBackFailed: ["recovery", "update"], refused: ["failed", "retry"], rolledBack: ["failed", "retry"], failed: ["failed", "retry"] }[outcome]
      assert.deepEqual([ended?.kind, press?.kind], want, outcome)
      assert.ok(outcome === "landed" || ended?.words, outcome)
    }
    for (const reason of UpdateReason.options) {
      const { ended, press } = standingOf(view({ offered, last: last("failed", reason) }))
      if (reason === "offerChanged") assert.deepEqual([ended, press?.kind], [null, "update"], "what is offered now was checked since")
      else if (reason === "interrupted") assert.deepEqual([ended?.kind, press?.kind], ["interrupted", "update"])
      else assert.deepEqual([ended?.kind, press?.kind], ["failed", "retry"], reason)
    }
  })

  test("forgets an end a newer offer or a newer release answers", () => {
    assert.equal(standingOf(view({ offered: { ...offered, version: "v0.3.0" }, last: last("rolledBack") })).ended, null)
    assert.equal(standingOf(view({ version: "v0.2.0", last: last("rolledBack") })).ended, null, "installed over SSH since")
    assert.equal(standingOf(view({ version: "v0.10.0", last: last("failed", "moved", "v0.9.0") })).ended, null, "releases are ordered by their numbers")
    assert.equal(standingOf(view({ version: "v0.10.0", last: last("failed", "stopped", "v0.11.0") })).ended?.words, "Stopped")
    assert.equal(standingOf(view({ version: "v0.3.0", last: last("landedWithError") })).ended, null)
    assert.equal(standingOf(view({ version: "v0.3.0", last: last("putBackFailed") })).ended, null)
  })

  test("with nothing offered keeps an end beside Check: the update that found another release the latest, the untidy landing, the recovery needed", () => {
    for (const [patch, words] of [
      [{ last: last("failed", "offerChanged") }, "Release changed"],
      [{ version: "v0.2.0", last: last("landedWithError") }, "Cleanup failed"],
      [{ version: "v0.2.0", last: last("putBackFailed") }, "Recovery needed"],
      [{ last: last("putBackFailed") }, "Recovery needed"],
    ] as const) {
      const { ended, press } = standingOf(view(patch))
      assert.deepEqual([ended?.words, press?.kind], [words, "check"])
    }
  })
})

test("the view is read on while an update runs, and while one cut in its switch is still to say how it ended", () => {
  assert.equal(moving(view({ running: { attempt: "a", version: "v0.2.0" } })), true)
  assert.equal(moving(view({ last: last("failed", "interrupted") })), true)
  assert.equal(moving(view({ offered, last: last("rolledBack") })), false)
})

test("a press whose answer was lost started something when the view names an update it didn't before", () => {
  const before = view({ offered, last: last("rolledBack") })
  assert.equal(pressedSince(before, before), false)
  assert.equal(pressedSince(before, view({ running: { attempt: "b", version: "v0.2.0" } })), true)
  assert.equal(pressedSince(before, view({ last: { ...last("failed", "noStart")!, attempt: "b" } })), true)
  assert.equal(pressedSince(undefined, view()), false)
})
