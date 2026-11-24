import assert from "node:assert/strict"
import { describe, test } from "node:test"
import { feedOrder, keyedConfigs, placesOf } from "./outbound.ts"
import type { LineageId, ServerView, SourceId, SourceView } from "./schema.ts"

const config = (key: string, place: number) => ({ key: key as LineageId, name: `Config ${key}`, place, servers: 1, skipped: [] })
const at = (configKey: string, tag: string, position: number) => ({ config: `Config ${configKey}`, configKey: configKey as LineageId, tag, position })
const server = (id: string, seen: ServerView["seen"], source = "s") => ({ id: id as LineageId, label: id, source: source as SourceId, seen }) as ServerView
const configs = [config("b", 2), config("a", 1)]

describe("a server's places", () => {
  test("come in feed order: the config's place in the document, then the outbound's position", () => {
    const s = server("x", [at("b", "proxy", 1), at("a", "Germany", 4), at("a", "EDGE-STANDBY", 2)])
    assert.deepEqual(placesOf(s, configs).map((p) => [p.place, p.position, p.tag]), [[1, 2, "EDGE-STANDBY"], [1, 4, "Germany"], [2, 1, "proxy"]])
  })

  test("order a list by source, then first place", () => {
    const sources = [{ id: "s" as SourceId, feed: { configs } }, { id: "t" as SourceId, feed: { configs: [config("c", 1)] } }] as unknown as SourceView[]
    const list = [server("late", [at("b", "proxy", 1)]), server("other", [at("c", "proxy", 1)], "t"), server("early", [at("a", "proxy", 3)])]
    assert.deepEqual(list.toSorted(feedOrder({ sources })).map((s) => s.id), ["early", "late", "other"])
  })

  test("read a config's place from its source's feed, which lists a config with no server without a key", () => {
    const feed = { configs: [config("a", 1), { key: null, name: "Empty", place: 2, servers: 0, skipped: [] }, config("b", 3)] }
    assert.deepEqual(keyedConfigs({ feed }).map((c) => [c.key, c.place]), [["a", 1], ["b", 3]])
    assert.deepEqual(keyedConfigs({ feed: null }), [])
  })
})
