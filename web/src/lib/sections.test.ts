import assert from "node:assert/strict"
import { describe, test } from "node:test"
import { groupsByHome, sectionsOf, type Section } from "./sections.ts"
import type { LineageId, ServerView, SourceView } from "./schema.ts"

const config = (key: string, place: number, name: string) => ({ key: key as LineageId, name, place, servers: 2, skipped: [] })
const server = (id: string, home: string | null, seen: [string, string][] = []) => ({
  id: id as LineageId, label: id, home: home as LineageId | null, name: null,
  seen: seen.map(([configKey, config], i) => ({ config, configKey: configKey as LineageId, tag: id, position: i + 1 })),
}) as unknown as ServerView
const source = { feed: { configs: [config("se", 2, "Sweden"), config("de", 1, "Germany"), { key: null, name: "Empty", place: 3, servers: 0, skipped: [] }] } } as unknown as Pick<SourceView, "feed">
const shape = (sections: Section[]) => sections.map((s) => [s.heading.kind === "config" ? s.heading.name : "name only", s.rows.map((r) => r.id)])

describe("a source's sections", () => {
  test("group only when some config is home to two servers", () => {
    assert.equal(groupsByHome([server("a", "de"), server("b", "se"), server("c", null), server("d", null)]), false)
    assert.equal(groupsByHome([server("a", "de"), server("b", "de")]), true)
  })

  test("follow the feed's order of configs, keep the rows' order inside each, and end with the servers no config is home to", () => {
    const rows = [server("se-2", "se"), server("loose", null), server("de-1", "de"), server("se-1", "se"), server("de-2", "de")]
    assert.deepEqual(shape(sectionsOf(rows, source)), [["Germany", ["de-1", "de-2"]], ["Sweden", ["se-2", "se-1"]], ["name only", ["loose"]]])
  })

  test("order a config's rows as it lists them, whatever order they come in", () => {
    const at = (id: string, position: number) => ({ ...server(id, "se"), seen: [{ config: "Germany", configKey: "de" as LineageId, tag: id, position: 1 }, { config: "Sweden", configKey: "se" as LineageId, tag: id, position }] }) as ServerView
    assert.deepEqual(shape(sectionsOf([at("origin", 2), at("proxy", 1)], source)), [["Sweden", ["proxy", "origin"]]])
  })

  test("leave out a section with no row, and put a home the feed no longer lists after the rest, by the name it was seen by", () => {
    const rows = [server("gone", "old", [["old", "Norway"]]), server("se-1", "se")]
    assert.deepEqual(shape(sectionsOf(rows, source)), [["Sweden", ["se-1"]], ["Norway", ["gone"]]])
    assert.deepEqual(sectionsOf([], source), [])
  })
})
