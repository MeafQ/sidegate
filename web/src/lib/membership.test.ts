import assert from "node:assert/strict"
import { describe, test } from "node:test"
import { bare, isMember } from "./membership.ts"
import type { Members } from "./schema.ts"

const matched = new Set(["fra", "ber", "par"])
const ticked = new Set(["fra", "ams"])
const members = (box: Parameters<typeof isMember>[2]) => ["fra", "ber", "par", "ams"].filter((id) => isMember(id, matched, box, ticked))

describe("isMember", () => {
  test("an unset box leaves ticks aside", () => {
    assert.deepEqual(members(null), ["fra", "ber", "par"])
  })
  test("ticked holds the ticked servers the other conditions match, and not ticked the rest", () => {
    assert.deepEqual(members("ticked"), ["fra"])
    assert.deepEqual(members("unticked"), ["ber", "par"])
  })
  test("a tick on a server the other conditions don't match makes no member", () => {
    assert.equal(isMember("ams", matched, "ticked", ticked), false)
  })
})

describe("bare", () => {
  const none = { not: false, values: [] }
  const rule = (patch: Partial<Members>): Members => ({ sources: none, countries: none, protocols: none, include: "", exclude: "", box: null, ...patch }) as Members
  test("a rule with no condition holds nothing, ticked or not", () => {
    assert.equal(bare(rule({}), 0), true)
    assert.equal(bare(rule({}), 2), true)
  })
  test("box is ticked needs a tick, whatever else is set", () => {
    assert.equal(bare(rule({ box: "ticked" }), 0), true)
    assert.equal(bare(rule({ box: "ticked" }), 1), false)
    assert.equal(bare(rule({ countries: { not: false, values: ["NL"] }, box: "ticked" }), 0), true)
    assert.equal(bare(rule({ sources: { not: true, values: [] } as Members["sources"], box: "ticked" }), 0), true)
  })
  test("any other condition is a rule, whatever it holds today", () => {
    assert.equal(bare(rule({ include: "nowhere" }), 0), false)
    assert.equal(bare(rule({ box: "unticked" }), 0), false)
    assert.equal(bare(rule({ countries: { not: false, values: ["NL"] } }), 0), false)
  })
})
