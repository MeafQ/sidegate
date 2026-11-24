import assert from "node:assert/strict"
import { after, before, describe, test } from "node:test"

import { ServerView, SourceView } from "./schema.ts"

const { acme, server, startFakeDaemon } = await import(new URL("../../tests/e2e/fake-daemon.mjs", import.meta.url).href)

const H = (hours: number) => new Date(Date.now() - hours * 3600e3).toISOString()

type Daemon = { url: string; set: (patch: object) => void; addAcme: (how?: { missing?: string[] }) => void; refreshDocument: (id: string, how: object) => void; close: () => Promise<void>
  source: (id: string) => SourceView & { version?: number } }

describe("the fake daemon's source, as Go sends it", () => {
  let daemon: Daemon
  before(async () => { daemon = await startFakeDaemon({ port: 0, dist: "", base: "/" }) })
  after(() => daemon.close())

  test("orders the present servers Xray can run", () => {
    daemon.set({ catalogue: [server("l-par", "Paris", "FR", 19)] })
    daemon.refreshDocument("aurora", { announce: false })
    const was = daemon.source("aurora")
    assert.deepEqual([was.order, was.firstChoice], [["l-par"], "l-par"])
    const after = (addServer: unknown) => {
      daemon.refreshDocument("aurora", { announce: false, addServer })
      const { order, firstChoice } = daemon.source("aurora")
      return [order, firstChoice]
    }
    assert.deepEqual(after(server("l-gone", "Gone", "NL", 5, { presence: "missing", missingSince: H(1) })), [["l-par"], "l-par"], "a faster missing server")
    assert.deepEqual(after(server("l-ss", "Kyiv", "UA", 3, { supported: false })), [["l-par"], "l-par"], "a faster unsupported server")
    assert.deepEqual(after(server("l-riga", "Riga", "LV", 4)), [["l-riga", "l-par"], "l-riga"], "a faster server joins, as every server of a source is offered")
  })

  test("refuses a save that still carries a rule, as Go does", async () => {
    daemon.addAcme()
    const was = daemon.source("acme")
    const { csrf } = (await (await fetch(`${daemon.url}api/session`)).json()) as { csrf: string }
    const res = await fetch(`${daemon.url}api/sources/acme`, {
      method: "PUT", headers: { "Content-Type": "application/json", "If-Match": `"${was.version ?? was.revision}"`, "X-Sidegate-Csrf": csrf, "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify({ name: was.name, origin: { kind: "remote", url: null, userAgent: "Happ/3.0.0", refreshEverySeconds: 0, paused: false }, pick: null }),
    })
    assert.equal(res.status, 400, await res.clone().text())
  })
})

describe("the fake daemon's reference feed", () => {
  test("is 6 configs of 13 places over 12 servers, with proxy at place 1 and EDGE-STANDBY at 2, as the page reads them", () => {
    const { servers, source } = acme()
    SourceView.parse(source)
    const read: ServerView[] = servers.map((s: unknown) => ServerView.parse(s))
    assert.equal(read.length, 12)
    assert.deepEqual(source.feed.configs.map((c: { place: number }) => c.place), [1, 2, 3, 4, 5, 6])
    const places = read.flatMap((s) => s.seen)
    assert.equal(places.length, 6 * 13)
    assert.deepEqual([...new Set(places.filter((p) => p.position === 1).map((p) => p.tag))], ["proxy"])
    assert.deepEqual([...new Set(places.filter((p) => p.position === 2).map((p) => p.tag))], ["EDGE-STANDBY"])
  })
})
