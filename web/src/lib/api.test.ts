import assert from "node:assert/strict"
import { after, afterEach, before, beforeEach, describe, mock, test } from "node:test"
import { z } from "zod"
import type { Answer, Write } from "./write.ts"

type Transport = {
  attempt: (write: Write<unknown>, answer: z.ZodType<unknown>, heal: boolean, signal: AbortSignal) => Promise<Answer>
  session: () => Promise<void>
  read: <T>(schema: z.ZodType<T>, path: string) => Promise<T>
}

const { pageModules } = await import(new URL("../../tests/page-modules.mjs", import.meta.url).href)
let close: () => Promise<void>
let transport: Transport

type Sent = { url: string; method: string; headers: Record<string, string>; body: string | null }
let sent: Sent[] = []
let answer: (sent: Sent, signal: AbortSignal | null) => Promise<Response>
let token = "t1"

before(async () => {
  const loaded = await pageModules("/src/lib/api.ts")
  transport = loaded.modules[0]
  close = loaded.close
})
after(() => close())

beforeEach(() => {
  sent = []
  token = "t1"
  globalThis.fetch = (async (url: string, init: RequestInit = {}) => {
    const request = { url, method: init.method ?? "GET", headers: { ...(init.headers as Record<string, string>) }, body: (init.body as string | undefined) ?? null }
    sent.push(request)
    if (url.endsWith("api/session")) return Response.json({ user: "owner", csrf: token, version: "v0.1.0", release: "0123456789ab" })
    return answer(request, init.signal ?? null)
  }) as typeof fetch
})

const json = (status: number, body: unknown, headers: Record<string, string> = {}) => () => Promise.resolve(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } }))
const html = (status: number) => () => Promise.resolve(new Response("<h1>no</h1>", { status, headers: { "Content-Type": "text/html" } }))
const tick: Write<unknown> = { key: "k1", method: "PUT", path: "api/groups/eu/ticked/l-ams", bytes: '{"ticked":true}', precondition: null, made: null, pressed: { ticked: true }, edits: new Map() }
const Ticked = z.strictObject({ ticked: z.boolean() })
const once = (write: Write<unknown> = tick, heal = false) => transport.attempt(write, Ticked, heal, new AbortController().signal)

describe("one attempt of a write, parsed into an Answer", () => {
  const cases: [string, () => Promise<Response>, Answer][] = [
    ["a 2xx that fits, with its revision, is a yes", json(200, { ticked: true }, { "Sidegate-Revision": "42" }), { kind: "yes", revision: 42, body: { ticked: true } }],
    ["a 2xx with no Sidegate-Revision may have landed", json(200, { ticked: true }), { kind: "unknown", status: 200 }],
    ["a 2xx that doesn't fit its schema may have landed", json(200, { ticked: "yes" }, { "Sidegate-Revision": "42" }), { kind: "unknown", status: 200 }],
    ["a 2xx that isn't JSON may have landed", () => Promise.resolve(new Response("{", { status: 200, headers: { "Sidegate-Revision": "42" } })), { kind: "unknown", status: 200 }],
    ["the daemon's 412 names the stored revision in its body", json(412, { kind: "staleRevision", reason: "stale", message: "Changed by another editor.", revision: 6 }, { "Sidegate-Revision": "9" }), { kind: "conflict", revision: 9, stored: 6 }],
    ["the daemon's 412 names it in ETag alone", json(412, { kind: "staleRevision", reason: "stale", message: "Changed." }, { ETag: 'W/"7"', "Sidegate-Revision": "9" }), { kind: "conflict", revision: 9, stored: 7 }],
    ["the daemon's own 422 is its refusal, with its reason", json(422, { kind: "validation", reason: "serverGone", message: "No such server." }, { "Sidegate-Revision": "9" }), { kind: "refused", revision: 9, status: 422, code: "validation", reason: "serverGone", detail: null, message: "No such server." }],
    ["the daemon's 404 is its refusal", json(404, { kind: "validation", reason: "slotGone", message: "No such item." }, { "Sidegate-Revision": "9" }), { kind: "refused", revision: 9, status: 404, code: "validation", reason: "slotGone", detail: null, message: "No such item." }],
    ["the daemon's csrf refusal is a token to heal", json(403, { kind: "csrf", reason: "tokenExpired", message: "The page's token expired." }, { "Sidegate-Revision": "9" }), { kind: "token", status: 403 }],
    ["UniFi's 403 is a token to heal", html(403), { kind: "token", status: 403 }],
    ["UniFi's 401 is a token to heal", json(401, { error: "unauthorized" }), { kind: "token", status: 401 }],
    ["a 413 from in front stored nothing and says its code", html(413), { kind: "front", status: 413, retry: false }],
    ["a 429 from in front stored nothing and may take later", html(429), { kind: "front", status: 429, retry: true }],
    ["a plain 404 from in front stored nothing", html(404), { kind: "front", status: 404, retry: false }],
    ["a daemon refusal with no Sidegate-Revision can't be read", json(422, { kind: "validation", reason: "serverGone", message: "No such server." }), { kind: "unknown", status: 422 }],
    ["the daemon's 503 may have landed", json(503, { kind: "unavailable", reason: "unavailable", message: "Busy." }, { "Sidegate-Revision": "9" }), { kind: "unknown", status: 503 }],
    ["nginx's 502 may have landed", html(502), { kind: "unknown", status: 502 }],
    ["no connection may have landed", () => Promise.reject(new TypeError("fetch failed")), { kind: "unknown", status: 0 }],
  ]
  for (const [name, respond, expected] of cases) {
    test(name, async () => {
      answer = respond
      assert.deepEqual(await once(), expected)
    })
  }
})

describe("what one attempt sends", () => {
  test("its key, its exact bytes, both tokens and no precondition for a tick", async () => {
    await transport.session()
    answer = json(200, { ticked: true }, { "Sidegate-Revision": "1", "X-Csrf-Token": "unifi-1" })
    await once()
    await once()
    const [first, second] = sent.filter((s) => s.method === "PUT")
    assert.equal(first.headers["Idempotency-Key"], "k1")
    assert.equal(first.body, '{"ticked":true}')
    assert.equal(first.headers["Content-Type"], "application/json")
    assert.equal(first.headers["X-Sidegate-Csrf"], "t1")
    assert.equal(first.headers["If-Match"], undefined)
    assert.equal(first.headers["If-None-Match"], undefined)
    assert.equal(second.headers["X-Csrf-Token"], "unifi-1", "UniFi's token the first answer handed out goes on the next")
  })

  test("a replace sends If-Match, a create If-None-Match, a bodiless write no Content-Type", async () => {
    answer = json(200, {}, { "Sidegate-Revision": "1" })
    await transport.attempt({ ...tick, precondition: { kind: "replace", revision: 4 } }, z.unknown(), false, new AbortController().signal)
    await transport.attempt({ ...tick, precondition: { kind: "create" } }, z.unknown(), false, new AbortController().signal)
    await transport.attempt({ ...tick, method: "DELETE", bytes: null }, z.unknown(), false, new AbortController().signal)
    const [replace, create, del] = sent.filter((s) => s.method !== "GET")
    assert.equal(replace.headers["If-Match"], '"4"')
    assert.equal(create.headers["If-None-Match"], "*")
    assert.equal(del.body, null)
    assert.equal(del.headers["Content-Type"], undefined)
  })

  test("a healing attempt rereads api/session first and sends the new token", async () => {
    await transport.session()
    token = "t2"
    answer = json(200, { ticked: true }, { "Sidegate-Revision": "1" })
    sent = []
    await once(tick, true)
    assert.deepEqual(sent.map((s) => `${s.method} ${s.url}`), ["GET /api/session", `PUT /${tick.path}`])
    assert.equal(sent[1].headers["X-Sidegate-Csrf"], "t2")
  })

  test("a healing attempt whose session read fails still goes, so the refusal is the daemon's", async () => {
    const working = globalThis.fetch
    globalThis.fetch = ((url: string, init?: RequestInit) => (url.endsWith("api/session") ? Promise.reject(new TypeError("down")) : working(url, init))) as typeof fetch
    answer = json(403, { kind: "csrf", reason: "tokenExpired", message: "The page's token expired." }, { "Sidegate-Revision": "1" })
    assert.deepEqual(await once(tick, true), { kind: "token", status: 403 })
    assert.equal(sent.filter((s) => s.method === "PUT").length, 1)
  })
})

describe("the session", () => {
  test("a read of it that failed is read again by the next attempt, which goes with the token it brings", async () => {
    const working = globalThis.fetch
    let failing = true
    globalThis.fetch = ((url: string, init?: RequestInit) => (url.endsWith("api/session") && failing ? Promise.reject(new TypeError("down")) : working(url, init))) as typeof fetch
    await assert.rejects(transport.session())
    failing = false
    answer = json(200, { ticked: true }, { "Sidegate-Revision": "4" })
    assert.deepEqual(await once(), { kind: "yes", revision: 4, body: { ticked: true } })
    const put = sent.filter((s) => s.method === "PUT")
    assert.equal(put.length, 1)
    assert.equal(put[0].headers["X-Sidegate-Csrf"], "t1", "with a token read anew")
  })

  for (const [by, refusal] of [["the daemon's csrf refusal", json(403, { kind: "csrf", reason: "tokenExpired", message: "The page's token expired." })], ["UniFi's 403", html(403)]] as const) {
    test(`a preview refused by ${by} reads the session again and goes once more, with the new token`, async () => {
      await transport.session()
      token = "t2"
      let refused = false
      answer = (request) => (request.method === "POST" && !refused ? (refused = true, refusal()) : json(200, { matched: [] })())
      const preview = (transport as unknown as { api: { previewGroup: (members: unknown) => Promise<unknown> } }).api
      assert.deepEqual(await preview.previewGroup({}), { matched: [] })
      const posts = sent.filter((s) => s.method === "POST")
      assert.deepEqual(posts.map((d) => d.headers["X-Sidegate-Csrf"]), ["t1", "t2"], "refused with the old token, then sent with the new")
    })
  }

  test("a preview refused twice for its token is that refusal", async () => {
    await transport.session()
    answer = html(403)
    const preview = (transport as unknown as { api: { previewGroup: (members: unknown) => Promise<unknown> } }).api
    await assert.rejects(preview.previewGroup({}), (e: Error & { status: number }) => e.status === 403)
    assert.equal(sent.filter((s) => s.method === "POST").length, 2, "once more, and no further")
  })
})

describe("a read", () => {
  beforeEach(() => mock.timers.enable({ apis: ["setTimeout"] }))
  afterEach(() => mock.timers.reset())

  test("with no answer in 10 s is a failed read", async () => {
    answer = (_, signal) => new Promise((_resolve, reject) => signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError"))))
    const reading = transport.read(z.unknown(), "api/state")
    let ended = false
    const outcome = reading.then(() => "landed", (error: Error) => error.message).finally(() => { ended = true })
    mock.timers.tick(9_999)
    await new Promise((r) => setImmediate(r))
    assert.equal(ended, false, "still reading at 9,999 ms")
    mock.timers.tick(1)
    assert.equal(await outcome, "Gateway not answering")
  })

  test("of the session with no answer in 10 s fails, and the attempt waiting on it goes on and is answered", { timeout: 10_000 }, async () => {
    const working = globalThis.fetch
    globalThis.fetch = ((url: string, init?: RequestInit) => (url.endsWith("api/session")
      ? new Promise((_resolve, reject) => init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError"))))
      : working(url, init))) as typeof fetch
    answer = json(403, { kind: "csrf", reason: "tokenExpired", message: "The page's token expired." }, { "Sidegate-Revision": "1" })
    let ended = false
    const attempt = once(tick, true).finally(() => { ended = true })
    mock.timers.tick(9_999)
    await new Promise((r) => setImmediate(r))
    assert.equal(ended, false, "still reading the session at 9,999 ms")
    mock.timers.tick(10_001)
    assert.deepEqual(await attempt, { kind: "token", status: 403 })
    assert.equal(sent.filter((s) => s.method === "PUT").length, 1)
  })

  test("whose body is cut off on the way is a failed read, not a page out of date", async () => {
    const errors = mock.method(console, "error", () => {})
    answer = () => Promise.resolve(new Response(new ReadableStream({ start(c) { c.enqueue(new TextEncoder().encode('{"revis')); c.error(new TypeError("network error")) } }), { status: 200 }))
    await assert.rejects(transport.read(z.object({ revision: z.number() }), "api/state"), { kind: "network", message: "Gateway not answering" })
    assert.equal(errors.mock.callCount(), 0, "nothing said about versions")
    errors.mock.restore()
  })

  test("whose answer isn't the page's schema says the page is out of date", async () => {
    const errors = mock.method(console, "error", () => {})
    answer = json(200, { revision: "three" })
    await assert.rejects(transport.read(z.object({ revision: z.number() }), "api/state"), { kind: "mismatch" })
    assert.equal(errors.mock.callCount(), 1, "the details go to the console")
    errors.mock.restore()
  })

  test("that answers in time lands", async () => {
    answer = json(200, { revision: 3 })
    assert.deepEqual(await transport.read(z.object({ revision: z.number() }), "api/state"), { revision: 3 })
  })
})
