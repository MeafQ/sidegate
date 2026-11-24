// The Go handler and the fake daemon must answer alike. One script of raw requests walks every route in ../../API.md
// against both, and each step compares status, kind, revision, ETag and the parts of the body the contract fixes.
// Every 2xx also parses against the schema the page uses.
import assert from "node:assert/strict"
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import os from "node:os"
import path from "node:path"
import { after, before, test } from "node:test"
import { z } from "zod"
import { AnalyticsReport, GroupPreview, GroupView, RefreshEvent, ServerPage, Session, SettingsView, SlotView, SourceView, SwitchEvent, UpdateView, View, ProbeResult } from "../../src/lib/schema.ts"
import { startFakeDaemon } from "./fake-daemon.mjs"
import { startRealDaemon } from "./real-daemon.mjs"

const BASE = "/sidegate/"
const FIRST_PORT = Number(process.env.E2E_PORT ?? 5461) + 8
const AURORA = "all-aurora"
const POOL = { name: "Asia", exit: { kind: "group", group: AURORA } }
const among = (...values) => ({ not: false, values })
const RULE = { sources: among("aurora"), countries: among(), protocols: among(), include: "", exclude: "", box: null }
/** A hand-picked group's rule: box is ticked, alone. */
const PICKED = { ...RULE, sources: among(), box: "ticked" }
const tick = (group, server, key, ticked = true) => write(`api/groups/${group}/ticked/${server}`, key, { body: { ticked } })
const ORIGIN = { kind: "remote", url: "https://borealis.example/s3cr3t-token", userAgent: "Happ/3.0.0", refreshEverySeconds: 3600, paused: false }
const INPUT = { name: "Borealis", origin: ORIGIN }
const SETTINGS = { analyticsRetentionDays: 30 }

let dist
let daemons

before(async () => {
  dist = mkdtempSync(path.join(os.tmpdir(), "contract-dist-"))
  mkdirSync(path.join(dist, "assets"))
  writeFileSync(path.join(dist, "index.html"), "<!doctype html><title>page</title>")
  writeFileSync(path.join(dist, "favicon.svg"), "<svg xmlns='http://www.w3.org/2000/svg'/>")
  writeFileSync(path.join(dist, "assets", "a.js"), "export {}")
  const started = await Promise.all([startFakeDaemon({ port: FIRST_PORT, dist, base: BASE }), startRealDaemon({ port: FIRST_PORT + 1, dist, base: BASE })])
  daemons = { fake: started[0], real: started[1] }
})

after(async () => {
  await Promise.all(Object.values(daemons ?? {}).map((d) => d.close()))
  rmSync(dist, { recursive: true, force: true })
})

/** Sends one request; the defaults are what the page sends on a mutation. */
async function send(daemon, { method = "GET", path: route, body, raw, headers = {}, key, csrf = "token-1", json = true }) {
  const mutation = method !== "GET"
  const all = { ...(mutation && csrf ? { "X-Sidegate-Csrf": csrf } : {}), ...(mutation && key ? { "Idempotency-Key": key } : {}), ...headers }
  const payload = raw ?? (body === undefined ? undefined : JSON.stringify(body))
  if (payload !== undefined && json) all["Content-Type"] = "application/json"
  const res = await fetch(daemon.url + route, { method, headers: all, body: payload })
  const text = await res.text()
  let parsed
  try { parsed = JSON.parse(text) } catch { parsed = undefined }
  return { status: res.status, etag: res.headers.get("etag"), type: res.headers.get("content-type"), revision: res.headers.get("sidegate-revision"), text, json: parsed }
}

const keyed = (request) => (request.method ?? "GET") !== "GET" && !["api/groups/preview", "api/update/check", "api/update"].includes(request.path)
const fingerprint = (request) => JSON.stringify([request.method, request.path, request.headers?.["If-Match"], request.headers?.["If-None-Match"], request.raw ?? JSON.stringify(request.body)])
/** A Test's answer is its result: it runs outside the daemon's commands and changes nothing to read. */
const isTest = (request) => /^api\/servers\/[^/]+\/probe$/.test(request.path)
/** What a daemon remembers under a key: none of the checks' refusals, no 5xx, no refusal of a reused key. */
const remembered = (request, answer) => request.key !== undefined && answer.status < 500 && answer.status !== 415 && answer.json?.kind !== "csrf"

/** Sends a keyed write between two reads of api/state, and holds its Sidegate-Revision to API.md: the view revision the
 * write was committed at, which is the view's right after it and past the one before it when the write was taken, or
 * for a replay the first answer's. The answer carries the read right after it as `after`. */
async function sendKeyed(daemon, request, firsts, who) {
  const before = (await send(daemon, { path: "api/state" })).json.revision
  const answer = await send(daemon, request)
  const state = (await send(daemon, { path: "api/state" })).json
  const after = state.revision
  const label = `${who}: ${request.method} ${request.path} (${request.key ?? "no key"}) answered ${answer.status}`
  assert.match(answer.revision ?? "", /^\d+$/, `${label} with no Sidegate-Revision`)
  const revision = Number(answer.revision)
  const first = request.key === undefined ? undefined : firsts.get(request.key)
  if (first?.fingerprint === fingerprint(request)) {
    assert.equal(revision, first.revision, `${label}: a replay names its first answer's revision`)
  } else {
    assert.ok(before <= revision, `${label}: Sidegate-Revision ${revision} is older than the view before it, ${before}`)
    assert.equal(revision, after, `${label}: Sidegate-Revision isn't the view's revision after the write`)
    if (answer.status < 300 && !isTest(request)) assert.ok(before < revision, `${label}: a write taken left the view at revision ${before}`)
    if (first === undefined && remembered(request, answer)) firsts.set(request.key, { fingerprint: fingerprint(request), revision })
  }
  return { ...answer, after: state }
}

const shapeOf = (answer) => ({
  status: answer.status, etag: answer.etag, kind: answer.json?.kind, reason: answer.json?.reason, detail: answer.json?.detail, revision: answer.json?.revision,
  json: answer.type?.startsWith("application/json") ?? false,
})

/** Runs one script on both daemons from their first state; `step` compares each request's answers. */
function contract(name, script) {
  test(name, async () => {
    for (const d of Object.values(daemons)) d.reset()
    const firsts = { fake: new Map(), real: new Map() }
    const sent = (who, request) => (keyed(request) ? sendKeyed(daemons[who], request, firsts[who], who) : send(daemons[who], request))
    const step = async (request, expected = {}) => {
      const [fake, real] = await Promise.all([sent("fake", request), sent("real", request)])
      const label = `${request.method ?? "GET"} ${request.path} (${request.key ?? "no key"})`
      assert.deepEqual(shapeOf(real), shapeOf(fake), `${label}: the two daemons differ. Real: ${real.text.slice(0, 300)}. Fake: ${fake.text.slice(0, 300)}`)
      if (expected.status !== undefined) assert.equal(real.status, expected.status, `${label}: ${real.text.slice(0, 300)}`)
      if (expected.kind !== undefined) assert.equal(real.json?.kind, expected.kind, label)
      if (expected.schema) {
        for (const [who, answer] of [["fake", fake], ["real", real]]) {
          const parsed = expected.schema.safeParse(answer.json)
          assert.ok(parsed.success, `${label}: ${who} doesn't fit the page's schema: ${parsed.success ? "" : z.prettifyError(parsed.error)}`)
        }
      }
      return { fake, real }
    }
    await script(step)
  })
}

const write = (path, key, extra = {}) => ({ method: "PUT", path, key, ...extra })

contract("reads answer in the page's schema", async (step) => {
  await step({ path: "api/session" }, { status: 200, schema: Session })
  const state = await step({ path: "api/state" }, { status: 200, schema: View })
  for (const [who, answer] of Object.entries(state)) {
    for (const g of answer.json.groups) assert.equal(g.firstChoice, g.order[0] ?? null, `${who}: ${g.id}'s first choice is rank 1 of its order`)
    for (const src of answer.json.sources) assert.equal(src.firstChoice, src.order[0] ?? null, `${who}: ${src.id}'s first choice is rank 1 of its order`)
  }
  const servers = await step({ path: "api/servers?limit=500&cursor=" }, { status: 200, schema: ServerPage })
  for (const [who, answer] of Object.entries(servers)) {
    for (const s of answer.json.items) assert.equal(s.health.pingAt === null, s.health.pingP50Ms === null, `${who}: ${s.id} has a ping exactly when it says when it was measured`)
  }
  await step({ path: "api/servers?limit=500&source=aurora&country=NL" }, { status: 200, schema: ServerPage })
  await step({ path: "api/servers?limit=nonsense" }, { status: 400, kind: "validation" })
  const from = new Date(Date.now() - 3600e3).toISOString()
  const to = new Date().toISOString()
  await step({ path: `api/analytics?from=${from}&to=${to}&bucket=300s&source=aurora&lineage=l-ams` }, { status: 200, schema: AnalyticsReport })
  await step({ path: `api/analytics?from=${to}&to=${from}&bucket=300s` }, { status: 400, kind: "validation" })
  await step({ path: `api/analytics?from=${from}&to=${to}&bucket=7s` }, { status: 400, kind: "validation" })
  await step({ path: "api/switches" }, { status: 200, schema: z.array(SwitchEvent) })
  await step({ path: "api/settings" }, { status: 200, schema: SettingsView })
  await step({ path: "api/update" }, { status: 200, schema: UpdateView })
  await step({ path: "api/nothing" }, { status: 404, kind: "validation" })
})

contract("every mutation's checks run in order, and none is remembered", async (step) => {
  const put = { path: "api/settings", method: "PUT", body: SETTINGS, headers: { "If-Match": '"5"' } }
  await step({ ...put, csrf: "stale", key: "k" }, { status: 403, kind: "csrf" })
  await step({ ...put, csrf: null, key: "k" }, { status: 403, kind: "csrf" })
  await step({ ...put }, { status: 400, kind: "validation" })
  await step({ ...put, key: "k", json: false }, { status: 415, kind: "validation" })
  await step({ ...put, key: "k", raw: JSON.stringify(SETTINGS), headers: { ...put.headers, "Content-Type": "text/plain" }, json: false }, { status: 415, kind: "validation" })
  await step({ ...put, key: "k" }, { status: 200, schema: SettingsView })
  await step({ method: "DELETE", path: "api/slots/none", key: "no-type" }, { status: 404, kind: "validation" })
})

contract("settings: replace only, with revisions and replays", async (step) => {
  const body = SETTINGS
  await step(write("api/settings", "none", { body }), { status: 428, kind: "validation" })
  await step(write("api/settings", "create", { body, headers: { "If-None-Match": "*" } }), { status: 412, kind: "staleRevision" })
  await step(write("api/settings", "both", { body, headers: { "If-None-Match": "*", "If-Match": '"5"' } }), { status: 428, kind: "validation" })
  const over5 = { body, headers: { "If-Match": '"5"' } }
  const first = await step(write("api/settings", "a", over5), { status: 200, schema: SettingsView })
  assert.equal(first.real.json.revision, 6)
  const replay = await step(write("api/settings", "a", over5), { status: 200, schema: SettingsView })
  assert.deepEqual(replay.real.json, first.real.json, "a replay is the first answer")
  await step(write("api/settings", "a", { ...over5, body: { ...body, analyticsRetentionDays: 31 } }), { status: 422, kind: "validation" })
  await step(write("api/settings", "a", { body, headers: { "If-Match": '"6"' } }), { status: 422, kind: "validation" })
  const refused = await step(write("api/settings", "b", over5), { status: 412, kind: "staleRevision" })
  assert.equal(refused.real.json.revision, 6)
  assert.equal(refused.real.etag, '"6"')
  await step(write("api/settings", "c", { body, headers: { "If-Match": '"6"' } }), { status: 200 })
  const again = await step(write("api/settings", "b", over5), { status: 412, kind: "staleRevision" })
  assert.equal(again.real.json.revision, 6, "a refusal replays what it first named, though 7 is stored now")
  await step(write("api/settings", "weak", { body, headers: { "If-Match": 'W/"7"' } }), { status: 412, kind: "staleRevision" })
  await step(write("api/settings", "word", { body, headers: { "If-Match": '"seven"' } }), { status: 412, kind: "staleRevision" })
  await step(write("api/settings", "unread", { raw: "{", headers: { "If-Match": '"7"' } }), { status: 400, kind: "validation" })
})

contract("the update check: its own write, no precondition, kept by a settings replace", async (step) => {
  const off = await step(write("api/settings/check-for-updates", "off", { body: { checkForUpdates: false } }), { status: 200, schema: SettingsView })
  assert.deepEqual([off.real.json.checkForUpdates, off.real.json.revision], [false, 6], "turned off at the next settings revision")
  const read = await step({ path: "api/settings" }, { status: 200, schema: SettingsView })
  assert.equal(read.real.json.checkForUpdates, false)
  const kept = await step(write("api/settings", "retention", { body: { analyticsRetentionDays: 90 }, headers: { "If-Match": '"6"' } }), { status: 200, schema: SettingsView })
  assert.deepEqual([kept.real.json.analyticsRetentionDays, kept.real.json.checkForUpdates], [90, false], "a settings replace keeps the check as stored")
  await step(write("api/settings/check-for-updates", "same", { body: { checkForUpdates: false } }), { status: 200, schema: SettingsView })
  await step(write("api/settings", "with-check", { body: { ...SETTINGS, checkForUpdates: true }, headers: { "If-Match": '"8"' } }), { status: 400, kind: "validation" })
  await step(write("api/settings/check-for-updates", "empty", { body: {} }), { status: 400, kind: "validation" })
  await step(write("api/settings/check-for-updates", "word", { body: { checkForUpdates: "no" } }), { status: 400, kind: "validation" })
  await step(write("api/settings/check-for-updates", "extra", { body: { checkForUpdates: true, other: 1 } }), { status: 400, kind: "validation" })
})

/** What both daemons say of updates, without what each makes itself: times and ids. */
const updatesAlike = ({ fake, real }, label) => {
  const said = ({ json }) => ({
    ...json, guide: json.guide && json.guide.split("/sidegate")[1], check: { ...json.check, at: json.check.at !== null }, offered: json.offered && { ...json.offered, notes: json.offered.notes.split("/releases")[1] },
    running: json.running && { ...json.running, attempt: null }, last: json.last && { ...json.last, attempt: null, at: null },
  })
  assert.deepEqual(said(real), said(fake), label)
  return said(real)
}

contract("updates: a check, one job at a time, and its end read from the view", async (step) => {
  const read = async (label) => updatesAlike(await step({ path: "api/update" }, { status: 200, schema: UpdateView }), label)
  const post = (path, body, expected) => step({ method: "POST", path, body }, expected)
  const press = (version, restartsCore = false) => ({ method: "POST", path: "api/update", body: { version, restartsCore } })
  const both = (change) => Object.values(daemons).forEach(change)
  assert.deepEqual(await read("at first"), { version: "v0.1.0", releases: true, guide: "/blob/v0.1.0/docs/operate.md", check: { at: false, failure: null }, offered: null, running: null, last: null })
  await step({ ...press("v0.2.0"), csrf: "stale" }, { status: 403, kind: "csrf" })
  await step({ method: "POST", path: "api/update/check", raw: "{}", json: false }, { status: 415, kind: "validation" })
  for (const body of [{}, { auto: "yes" }, { auto: true, more: 1 }, null]) await post("api/update/check", body, { status: 400, kind: "validation" })
  for (const body of [{}, { version: "v0.2.0" }, { restartsCore: false }, { version: "v0.2.0", restartsCore: null }, { version: 2, restartsCore: false }]) await post("api/update", body, { status: 400, kind: "validation" })
  await step(press("v0.2.0"), { status: 422, kind: "validation" })
  both((d) => d.set({ latest: "v0.2.0" }))
  const loaded = updatesAlike(await post("api/update/check", { auto: true }, { status: 200, schema: UpdateView }), "the page loaded")
  assert.deepEqual(loaded.offered, { version: "v0.2.0", restartsCore: false, notes: "/tag/v0.2.0" })
  both((d) => d.set({ latest: "v0.3.0" }))
  assert.equal(updatesAlike(await post("api/update/check", { auto: true }, { status: 200, schema: UpdateView }), "loaded again").offered.version, "v0.2.0", "a second load asks nobody")
  assert.equal(updatesAlike(await post("api/update/check", { auto: false }, { status: 200, schema: UpdateView }), "pressed").offered.version, "v0.3.0", "a press does")
  both((d) => d.set({ latest: null }))
  const failed = updatesAlike(await post("api/update/check", { auto: false }, { status: 200, schema: UpdateView }), "a failed check")
  assert.deepEqual([failed.check.failure, failed.offered.version], ["filesUnavailable", "v0.3.0"], "a failure keeps the offer")
  await step(press("v0.2.0"), { status: 422, kind: "validation" })
  await step(press("v0.3.0", true), { status: 422, kind: "validation" })
  const pressed = await step(press("v0.3.0"), { status: 202, schema: UpdateView })
  assert.equal(updatesAlike(pressed, "an update runs").running.version, "v0.3.0")
  const again = await step(press("v0.3.0"), { status: 202, schema: UpdateView })
  for (const who of ["fake", "real"]) assert.equal(again[who].json.running.attempt, pressed[who].json.running.attempt, `${who}: a press while one runs is answered with it`)
  both((d) => d.endUpdate({ outcome: "failed", reason: "checksum", message: "x" }))
  const ended = await read("it failed")
  assert.deepEqual([ended.running, ended.last.outcome, ended.last.reason, ended.offered.version], [null, "failed", "checksum", "v0.3.0"])
  const next = await step(press("v0.3.0"), { status: 202, schema: UpdateView })
  for (const who of ["fake", "real"]) assert.notEqual(next[who].json.running.attempt, pressed[who].json.running.attempt, `${who}: a press after it ended is a new attempt`)
  both((d) => d.endUpdate({ outcome: "rolledBack", message: "x" }))
  assert.deepEqual([(await read("it rolled back")).last.outcome], ["rolledBack"])
  await step(write("api/settings/check-for-updates", "updates-off", { body: { checkForUpdates: false } }), { status: 200 })
  both((d) => d.set({ latest: "v0.4.0" }))
  updatesAlike(await post("api/update/check", { auto: false }, { status: 200, schema: UpdateView }), "a press with the setting off")
  both((d) => d.set({ version: "dev-7990743" }))
  assert.deepEqual(await read("a development build"), { version: "dev-7990743", releases: false, guide: null, check: { at: false, failure: null }, offered: null, running: null, last: null })
  updatesAlike(await post("api/update/check", { auto: true }, { status: 200, schema: UpdateView }), "its page loaded")
  await post("api/update/check", { auto: false }, { status: 422, kind: "unsupported" })
  await step(press("v0.4.0"), { status: 422, kind: "unsupported" })
})

contract("updates: one that found another release the latest has its check run again within the day", async (step) => {
  const check = async (auto, label) => updatesAlike(await step({ method: "POST", path: "api/update/check", body: { auto } }, { status: 200, schema: UpdateView }), label)
  const read = async (label) => updatesAlike(await step({ path: "api/update" }, { status: 200, schema: UpdateView }), label)
  const press = (version) => step({ method: "POST", path: "api/update", body: { version, restartsCore: false } }, { status: 202, schema: UpdateView })
  const both = (change) => Object.values(daemons).forEach(change)
  /* Go's times are finer than the fake's milliseconds, and the two a rule compares are never in one. */
  const tick = () => new Promise((done) => setTimeout(done, 5))
  both((d) => d.set({ latest: "v0.2.0" }))
  await check(true, "the page loaded")
  await tick()
  await press("v0.2.0")
  both((d) => d.set({ latest: "v0.3.0" }))
  both((d) => d.endUpdate({ outcome: "failed", reason: "offerChanged", message: "x" }))
  assert.equal((await read("it found another")).offered, null, "v0.2.0 isn't the latest now")
  await tick()
  assert.equal((await check(true, "the page loaded again")).offered.version, "v0.3.0", "the automatic check fetched, within the day")
  both((d) => d.set({ latest: "v0.4.0" }))
  assert.equal((await check(true, "and again")).offered.version, "v0.3.0", "once")
  await press("v0.3.0")
  both((d) => d.endUpdate({ outcome: "failed", reason: "checksum", message: "x" }))
  assert.equal((await check(true, "after another failure")).offered.version, "v0.3.0", "which rechecks nothing")
  await press("v0.3.0")
  await tick()
  both((d) => d.set({ latest: "v0.3.0" }))
  await check(false, "pressed while it runs")
  both((d) => d.endUpdate({ outcome: "failed", reason: "offerChanged", message: "x" }))
  assert.equal((await read("it found another, and the check since is newer")).offered.version, "v0.3.0")
  both((d) => d.set({ latest: "v0.4.0" }))
  assert.equal((await check(true, "the page loaded then")).offered.version, "v0.3.0", "a check since the press isn't run again")
})

contract("every route that reads a body answers one of another form as Go does", async (step) => {
  const create = { "If-None-Match": "*" }
  const routes = [
    ["PUT", "api/settings", { "If-Match": '"5"' }], ["PUT", "api/settings/check-for-updates"], ["POST", "api/update/check"], ["POST", "api/update"], ["PUT", "api/unifi/key"],
    ["PUT", "api/slots/new", create], ["PUT", "api/slots/eu", { "If-Match": '"3"' }], ["POST", "api/groups/preview"], ["PUT", "api/groups/new", create],
    ["PUT", `api/groups/${AURORA}`, { "If-Match": '"1"' }], ["PUT", `api/groups/${AURORA}/ticked/l-ams`], ["PUT", "api/sources/new", create], ["PUT", "api/sources/aurora", { "If-Match": '"1"' }],
    ["POST", "api/sources/aurora/refresh"],
  ]
  const bodies = [null, {}, [], 5, "x", true, { name: null }, { name: "x", exit: null }, { name: "x", origin: null }, { name: "x", origin: { kind: "manual" } }, { name: "x", origin: { kind: "other" } }, { members: null }, { ticked: null }]
  for (const [method, path, headers = {}] of routes) {
    for (const body of bodies) {
      const answers = await step({ method, path, headers, body, key: `${method} ${path} ${JSON.stringify(body)}` })
      if (path === "api/settings/check-for-updates") assert.equal(answers.real.status, 400, JSON.stringify(body))
      for (const d of Object.values(daemons)) if (answers.real.status < 300) d.reset()
    }
  }
})

contract("the UniFi key: one line", async (step) => {
  await step(write("api/unifi/key", "two-lines", { body: { key: "two words" } }), { status: 400, kind: "validation" })
  await step(write("api/unifi/key", "extra", { body: { key: "k", other: 1 } }), { status: 400, kind: "validation" })
})

contract("slots: create, replace, delete, and revisions past a delete", async (step) => {
  const path = "api/slots/asia"
  await step(write(path, "s-none", { body: POOL }), { status: 428, kind: "validation" })
  await step(write(path, "s-gone", { body: POOL, headers: { "If-Match": '"1"' } }), { status: 404, kind: "validation" })
  await step(write(path, "s-zero", { body: POOL, headers: { "If-Match": '"0"' } }), { status: 404, kind: "validation" })
  await step(write(path, "s-unread", { raw: "not json", headers: { "If-None-Match": "*" } }), { status: 400, kind: "validation" })
  await step(write(path, "s-blank", { body: { ...POOL, name: "" }, headers: { "If-None-Match": "*" } }), { status: 422, kind: "validation" })
  const created = await step(write(path, "s-new", { body: POOL, headers: { "If-None-Match": "*" } }), { status: 200, schema: SlotView })
  assert.equal(created.real.json.version, 1)
  assert.equal(created.real.json.name, "Asia")
  assert.deepEqual(created.real.json.exit, POOL.exit)
  const replay = await step(write(path, "s-new", { body: POOL, headers: { "If-None-Match": "*" } }), { status: 200, schema: SlotView })
  assert.deepEqual(replay.real.json, created.real.json, "a replay is the first answer")
  const taken = await step(write(path, "s-taken", { body: POOL, headers: { "If-None-Match": "*" } }), { status: 412, kind: "staleRevision" })
  assert.equal(taken.real.json.revision, 1)
  const renamed = await step(write(path, "s-edit", { body: { ...POOL, name: "Far East" }, headers: { "If-Match": '"1"' } }), { status: 200, schema: SlotView })
  assert.equal(renamed.real.json.version, 2)
  const stale = await step(write(path, "s-stale", { body: POOL, headers: { "If-Match": '"1"' } }), { status: 412, kind: "staleRevision" })
  assert.equal(stale.real.json.revision, 2)
  assert.equal(stale.real.etag, '"2"')
  await step({ method: "DELETE", path, key: "s-del" }, { status: 200 })
  await step({ method: "DELETE", path, key: "s-del" }, { status: 200 })
  await step({ method: "DELETE", path, key: "s-del-2" }, { status: 404, kind: "validation" })
  await step(write(path, "s-after", { body: POOL, headers: { "If-Match": '"2"' } }), { status: 404, kind: "validation" })
  const reborn = await step(write(path, "s-reborn", { body: POOL, headers: { "If-None-Match": "*" } }), { status: 200, schema: SlotView })
  assert.equal(reborn.real.json.version, 3, "an id's revisions carry on past a delete")
  await step({ method: "DELETE", path, key: "s-del-3" }, { status: 200 })
  await new Promise((done) => setTimeout(done, 500))
  const later = await step(write(path, "s-later", { body: POOL, headers: { "If-None-Match": "*" } }), { status: 200, schema: SlotView })
  assert.equal(later.real.json.version, 4, "also once the delete has finished")
})

/** A slot waiting for its first server: neither failed nor serving, naming no server, with no server applied and the
 * runtime still on `applied`. */
const assertChecking = (slot, applied, label) => assert.deepEqual(
  { status: slot.status, downReason: slot.downReason, server: slot.server, appliedServer: slot.appliedServer, applied: slot.runtime.applied },
  { status: "checking", downReason: null, server: null, appliedServer: null, applied }, label)

contract("a slot's Exit must be usable when it is chosen", async (step) => {
  const create = (key, exit, rest = {}) => write("api/slots/new", key, { body: { ...POOL, exit, ...rest }, headers: { "If-None-Match": "*" } })
  await step(create("x-group", { kind: "group", group: "nobody" }), { status: 422, kind: "validation" })
  await step(create("x-server", { kind: "server", server: "nobody" }), { status: 422, kind: "validation" })
  await step(create("x-source", { kind: "source", source: "nobody" }), { status: 422, kind: "validation" })
  await step(create("x-arm", { kind: "group", server: "l-ams" }), { status: 422, kind: "validation" })
  await step(create("x-extra", { kind: "direct", group: AURORA }), { status: 422, kind: "validation" })
  await step(create("x-null", null), { status: 422, kind: "validation" })
  await step(create("x-old", POOL.exit, { paused: true }), { status: 400, kind: "validation" })
  await step(write("api/groups/empty", "x-empty-group", { body: { name: "Empty", members: { ...RULE, sources: among(), countries: among("SE") }, ticked: [] }, headers: { "If-None-Match": "*" } }), { status: 200, schema: GroupView })
  await step(create("x-empty", { kind: "group", group: "empty" }), { status: 422, kind: "validation" })
  const made = await step(create("x-ok", { kind: "source", source: "aurora" }), { status: 200, schema: SlotView })
  assert.deepEqual(made.real.json.exit, { kind: "source", source: "aurora" })
  for (const answer of [made.fake, made.real]) assertChecking(answer.json, { kind: "down", reason: "unselected" }, "a slot made on a source, before the election")
  const state = await step({ path: "api/state" }, { status: 200, schema: View })
  for (const answer of [state.fake, state.real]) {
    const aurora = answer.json.sources.find((x) => x.id === "aurora")
    assert.deepEqual([aurora.usedBy, aurora.removes], [["eu", "new"], [AURORA]], "every slot the delete is refused for, by the source or a group it removes, and that group")
  }
  await step({ method: "DELETE", path: "api/sources/aurora", key: "x-del" }, { status: 409, kind: "referenced" })
})

/** Waits for each daemon to report `kind` applied on a slot. The real daemon applies a new slot's Exit only once UniFi has
 * made its VPN Client and the runtime has run the plan, on actors of their own, and the fake does both at once. */
async function applied(id, kind) {
  for (const [who, daemon] of Object.entries(daemons)) {
    const deadline = Date.now() + 5000
    let slot
    while (Date.now() < deadline) {
      slot = (await send(daemon, { path: "api/state" })).json.slots.find((s) => s.id === id)
      if (slot?.runtime.applied.kind === kind) break
      await new Promise((done) => setTimeout(done, 10))
    }
    assert.equal(slot?.runtime.applied.kind, kind, `${who}: ${id} never had ${kind} applied: ${JSON.stringify(slot?.runtime)}`)
  }
}

contract("Direct and Block are Exits, and an ordinary replace", async (step) => {
  const path = "api/slots/wan"
  const made = await step(write(path, "d-new", { body: { ...POOL, exit: { kind: "direct" } }, headers: { "If-None-Match": "*" } }), { status: 200, schema: SlotView })
  assert.equal(made.real.json.status, "direct")
  assert.equal(made.real.json.downReason, null)
  const blocked = await step(write(path, "d-block", { body: { ...POOL, exit: { kind: "block" } }, headers: { "If-Match": '"1"' } }), { status: 200, schema: SlotView })
  assert.equal(blocked.real.json.status, "block")
  await applied("wan", "block")
  const back = await step(write(path, "d-back", { body: POOL, headers: { "If-Match": '"2"' } }), { status: 200, schema: SlotView })
  assert.deepEqual(back.real.json.exit, POOL.exit)
  for (const answer of [back.fake, back.real]) assertChecking(answer.json, { kind: "block" }, "a slot back from Block, before the election")
  await step(write(path, "d-odd", { body: { ...POOL, direct: true }, headers: { "If-Match": '"3"' } }), { status: 400, kind: "validation" })
})

contract("a slot's VPN Client is turned on or recreated only when it needs it", async (step) => {
  await step({ method: "POST", path: "api/slots/eu/client/enable", key: "c-on" }, { status: 422, kind: "validation" })
  await step({ method: "POST", path: "api/slots/eu/client/recreate", key: "c-new" }, { status: 422, kind: "validation" })
  await step({ method: "POST", path: "api/slots/nobody/client/enable", key: "c-gone" }, { status: 404, kind: "validation" })
})

contract("groups: create, replace, delete, and revisions past a delete", async (step) => {
  const path = "api/groups/mine"
  const body = { name: "Mine", members: PICKED, ticked: ["l-ams"] }
  await step(write(path, "g-none", { body }), { status: 428, kind: "validation" })
  await step(write(path, "g-gone", { body: { name: "Mine", members: PICKED }, headers: { "If-Match": '"1"' } }), { status: 404, kind: "validation" })
  await step(write(path, "g-blank", { body: { ...body, name: " " }, headers: { "If-None-Match": "*" } }), { status: 422, kind: "validation" })
  await step(write(path, "g-hidden", { body: { ...body, hidden: true }, headers: { "If-None-Match": "*" } }), { status: 400, kind: "validation" })
  await step(write(path, "g-bad", { body: { ...body, members: { ...RULE, include: "(" } }, headers: { "If-None-Match": "*" } }), { status: 422, kind: "validation" })
  await step(write(path, "g-country", { body: { ...body, members: { ...RULE, countries: among("DEU") } }, headers: { "If-None-Match": "*" } }), { status: 422, kind: "validation" })
  await step(write(path, "g-nosource", { body: { ...body, members: { ...RULE, sources: among("nobody") } }, headers: { "If-None-Match": "*" } }), { status: 422, kind: "validation" })
  await step(write(path, "g-box", { body: { ...body, members: { ...RULE, box: "maybe" } }, headers: { "If-None-Match": "*" } }), { status: 422, kind: "validation" })
  await step(write(path, "g-hand", { body: { ...body, members: { ...RULE, added: ["l-ams"] } }, headers: { "If-None-Match": "*" } }), { status: 400, kind: "validation" })
  const created = await step(write(path, "g-new", { body, headers: { "If-None-Match": "*" } }), { status: 200, schema: GroupView })
  assert.equal(created.real.json.revision, 1)
  const replay = await step(write(path, "g-new", { body, headers: { "If-None-Match": "*" } }), { status: 200, schema: GroupView })
  assert.deepEqual(replay.real.json, created.real.json)
  const stale = await step(write(path, "g-stale", { body: { name: "Mine", members: PICKED }, headers: { "If-Match": '"9"' } }), { status: 412, kind: "staleRevision" })
  assert.equal(stale.real.json.revision, 1)
  await step(write(path, "g-ticks", { body, headers: { "If-Match": '"1"' } }), { status: 400, kind: "validation" })
  await step(write(path, "g-ticks-stale", { body, headers: { "If-Match": '"9"' } }), { status: 400, kind: "validation" })
  const edited = await step(write(path, "g-edit", { body: { name: "Ours", members: { ...RULE, countries: among("NL") } }, headers: { "If-Match": '"1"' } }), { status: 200, schema: GroupView })
  assert.deepEqual(edited.real.json.resolved, ["l-ams"])
  assert.equal(edited.real.json.revision, 2)
  await step({ method: "DELETE", path, key: "g-del" }, { status: 200 })
  await step({ method: "DELETE", path, key: "g-del-2" }, { status: 404, kind: "validation" })
  const reborn = await step(write(path, "g-reborn", { body, headers: { "If-None-Match": "*" } }), { status: 200, schema: GroupView })
  assert.equal(reborn.real.json.revision, 3, "an id's revisions carry on past a delete")
})

contract("one referential rule: what a slot uses can't go, and the refusal says so", async (step) => {
  await step({ method: "DELETE", path: "api/sources/aurora", key: "r-source" }, { status: 409, kind: "referenced" })
  await step({ method: "DELETE", path: `api/groups/${AURORA}`, key: "r-group" }, { status: 409, kind: "referenced" })
  const body = { name: "Mine", members: PICKED, ticked: ["l-ams"] }
  await step(write("api/groups/mine", "r-mine", { body, headers: { "If-None-Match": "*" } }), { status: 200, schema: GroupView })
  const moved = { name: "EU", exit: { kind: "group", group: "mine" } }
  await step(write("api/slots/eu", "r-move", { body: moved, headers: { "If-Match": '"3"' } }), { status: 200, schema: SlotView })
  await step({ method: "DELETE", path: "api/groups/mine", key: "r-used" }, { status: 409, kind: "referenced" })
  const assigned = { name: "EU", exit: { kind: "server", server: "l-ams" } }
  await step(write("api/slots/eu", "r-assign", { body: assigned, headers: { "If-Match": '"4"' } }), { status: 200, schema: SlotView })
  await step({ method: "DELETE", path: "api/sources/aurora", key: "r-server" }, { status: 409, kind: "referenced" })
})

contract("a group's create carries its rule and its ticks in one write, and one that could hold no server is refused", async (step) => {
  const create = (id, members, ticked) => write(`api/groups/${id}`, `n-${id}`, { body: { name: id, members, ticked }, headers: { "If-None-Match": "*" } })
  const ruled = await step(create("rule", { ...RULE, sources: among(), countries: among("NL") }, []), { status: 200, schema: GroupView })
  assert.deepEqual([ruled.real.json.resolved, ruled.real.json.ticked], [["l-ams"], []], "a rule alone")
  const picked = await step(create("picked", PICKED, ["l-par", "l-ams"]), { status: 200, schema: GroupView })
  assert.deepEqual([picked.real.json.resolved, picked.real.json.ticked, picked.real.json.revision], [["l-ams", "l-par"], ["l-ams", "l-par"], 1], "ticks alone, stored with the create")
  const both = await step(create("both", { ...RULE, sources: among(), countries: among("NL") }, ["l-par"]), { status: 200, schema: GroupView })
  assert.deepEqual([both.real.json.resolved, both.real.json.ticked], [["l-ams"], ["l-par"]], "a rule and a tick that changes no member while the box is unset")
  const absent = await step(create("absent", { ...RULE, sources: among(), countries: among("SE") }, []), { status: 200, schema: GroupView })
  assert.deepEqual(absent.real.json.resolved, [], "a rule no server matches today, with no box condition, is kept")
  for (const [id, members, ticked] of [["nothing", { ...RULE, sources: among() }, []], ["unboxed", { ...RULE, sources: among() }, ["l-ams"]], ["unticked", PICKED, []], ["boxed", { ...PICKED, countries: among("NL") }, []]]) {
    const refused = await step(create(id, members, ticked), { status: 422, kind: "validation" })
    assert.equal(refused.real.json.reason, "ruleMissing", `${id}: a create that could hold no server`)
  }
  const gone = await step(create("gone", PICKED, ["l-nowhere"]), { status: 422, kind: "validation" })
  assert.equal(gone.real.json.reason, "serverGone")
  const twice = await step(create("twice", PICKED, ["l-ams", "l-ams"]), { status: 422, kind: "validation" })
  assert.equal(twice.real.json.reason, "malformed")
  const empty = await step(create("empty", PICKED, [null]), { status: 422, kind: "validation" })
  assert.equal(empty.real.json.reason, "malformed", "Go reads a null tick as an empty one")
  for (const [id, body] of [["untold", { name: "untold", members: PICKED }], ["null", { name: "null", members: PICKED, ticked: null }]]) {
    const old = await step(write(`api/groups/${id}`, `o-${id}`, { body, headers: { "If-None-Match": "*" } }), { status: 400, kind: "validation" })
    assert.equal(old.real.json.reason, "malformed", `${id}: a create carries ticked as a list`)
  }
  const state = await step({ path: "api/state" }, { status: 200, schema: View })
  assert.deepEqual(state.real.json.groups.map((g) => g.id).toSorted(), ["absent", AURORA, "both", "picked", "rule"], "a refused create stores nothing")
})

contract("a create's tick must name a server the gateway still has, and a missing one holds no member", async (step) => {
  for (const d of Object.values(daemons)) d.missServer("l-par")
  const missing = await step(write("api/groups/away", "m-away", { body: { name: "Away", members: PICKED, ticked: ["l-ams", "l-par"] }, headers: { "If-None-Match": "*" } }), { status: 200, schema: GroupView })
  for (const [who, answer] of Object.entries(missing)) assert.deepEqual([answer.json.ticked, answer.json.resolved], [["l-ams", "l-par"], ["l-ams"]], `${who}: a missing server's tick is kept and holds no member`)
  await step(write("api/slots/eu", "m-direct", { body: { name: "EU", exit: { kind: "direct" } }, headers: { "If-Match": '"3"' } }), { status: 200, schema: SlotView })
  await step({ method: "DELETE", path: "api/sources/aurora", key: "m-source" }, { status: 200 })
  const gone = await step(write("api/groups/late", "m-late", { body: { name: "Late", members: PICKED, ticked: ["l-ams"] }, headers: { "If-None-Match": "*" } }), { status: 422, kind: "validation" })
  assert.equal(gone.real.json.reason, "serverGone", "a deleted source's server is a tombstone")
  const state = await step({ path: "api/state" }, { status: 200, schema: View })
  for (const [who, answer] of Object.entries(state)) assert.deepEqual(answer.json.groups.map((g) => g.id), [], `${who}: the refused create stored nothing`)
})

contract("a group's routes read any JSON body as Go decodes it, before the revision", async (step) => {
  const stale = await step(write(`api/groups/${AURORA}`, "j-stale", { raw: "null", headers: { "If-Match": '"99"' } }), { status: 412, kind: "staleRevision" })
  assert.equal(stale.real.json.revision, 1)
  const odd = ["null", "[]", "5", '"x"', "{}", '{"members":null}', '{"name":"x","members":{"countries":null,"sources":{"values":null}}}', '{"name":"x","members":{"countries":{"values":[null]}}}',
    '{"name":"x","members":{"box":null,"include":null}}', '{"name":"x","members":{"sources":[]}}', '{"name":"x","members":{"not":1}}', '{"ticked":null}', '{"ticked":"yes"}', '{"ticked":[]}']
  for (const [i, raw] of odd.entries()) {
    for (const [at, headers] of [["stale", { "If-Match": '"99"' }], ["now", { "If-Match": '"1"' }], ["new", { "If-None-Match": "*" }]]) {
      await step(write(at === "new" ? `api/groups/odd-${i}` : `api/groups/${AURORA}`, `j-${at}-${i}`, { raw, headers }))
    }
    await step(write(`api/groups/${AURORA}/ticked/l-ams`, `j-tick-${i}`, { raw }))
    await step({ method: "POST", path: "api/groups/preview", raw })
  }
})

contract("deleting a source empties what a surviving group held of it", async (step) => {
  await step(write("api/groups/de", "e-de", { body: { name: "DE", members: { ...RULE, sources: among(), countries: among("DE") }, ticked: [] }, headers: { "If-None-Match": "*" } }), { status: 200, schema: GroupView })
  await step(write("api/slots/eu", "e-direct", { body: { name: "EU", exit: { kind: "direct" } }, headers: { "If-Match": '"3"' } }), { status: 200, schema: SlotView })
  await step({ method: "DELETE", path: "api/sources/aurora", key: "e-source" }, { status: 200 })
  const state = await step({ path: "api/state" }, { status: 200, schema: View })
  for (const [who, answer] of Object.entries(state)) {
    const de = answer.json.groups.find((g) => g.id === "de")
    assert.deepEqual([de?.resolved, de?.order, de?.firstChoice], [[], [], null], `${who}: the group stays and holds no server`)
  }
})

/** Opens the change feed of every daemon, and collects each one's event kinds until `until` holds for both or a second passes. */
async function listen() {
  const feeds = await Promise.all(Object.entries(daemons).map(async ([who, d]) => {
    const stream = new AbortController()
    const res = await fetch(d.url + "api/events", { signal: stream.signal })
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    const feed = { who, frames: "", stream, reader, decoder }
    while (!feed.frames.includes(": connected\n\n")) feed.frames += decoder.decode((await reader.read()).value)
    return feed
  }))
  const kinds = (feed) => [...feed.frames.matchAll(/data: (\{.*\})\n\n/g)].map((m) => JSON.parse(m[1]).kind)
  return {
    async until(holds) {
      const deadline = Date.now() + 1000
      await Promise.all(feeds.map(async (feed) => {
        while (!holds(kinds(feed)) && Date.now() < deadline) {
          const next = await Promise.race([feed.reader.read(), new Promise((r) => setTimeout(() => r(null), deadline - Date.now()))])
          if (next?.value) feed.frames += feed.decoder.decode(next.value)
        }
      }))
      for (const feed of feeds) feed.stream.abort()
      return Object.fromEntries(feeds.map((feed) => [feed.who, kinds(feed)]))
    },
  }
}

contract("a server that leaves its feed is news, and leaves its source's count and order", async (step) => {
  const before = await step({ path: "api/state" }, { status: 200, schema: View })
  const feeds = await listen()
  for (const d of Object.values(daemons)) d.missServer("l-par")
  const heard = await feeds.until((kinds) => ["catalogue", "group", "source"].every((k) => kinds.includes(k)))
  for (const [who, kinds] of Object.entries(heard)) for (const kind of ["catalogue", "group", "source"]) assert.ok(kinds.includes(kind), `${who}: the page hears of it as ${kind}: ${kinds}`)
  const after = await step({ path: "api/state" }, { status: 200, schema: View })
  const without = (order) => order.filter((id) => id !== "l-par")
  for (const [who, answer] of Object.entries(after)) {
    const was = before[who].json
    const aurora = answer.json.sources.find((src) => src.id === "aurora")
    const order = without(was.sources.find((src) => src.id === "aurora").order)
    assert.deepEqual([aurora.stored, aurora.missing, aurora.order, aurora.firstChoice], [2, 1, order, order[0]], `${who}: Aurora counts it missing and ranks it no more`)
    assert.deepEqual(answer.json.groups.find((g) => g.id === AURORA).order, without(was.groups.find((g) => g.id === AURORA).order), `${who}: nor does the group`)
    assert.equal(answer.json.revision, was.revision, `${who}: no command was committed`)
  }
})

contract("a group preview stores nothing", async (step) => {
  const path = "api/groups/preview"
  await step({ method: "POST", path, csrf: "wrong", body: { members: RULE } }, { status: 403, kind: "csrf" })
  const matched = await step({ method: "POST", path, body: { members: { ...RULE, countries: among("NL", "DE"), box: "ticked" } } }, { status: 200, schema: GroupPreview })
  assert.deepEqual(matched.real.json.matched, ["l-ams", "l-fra"], "the box is the page's to apply")
  const boxAlone = await step({ method: "POST", path, body: { members: { ...PICKED, box: "unticked" } } }, { status: 200, schema: GroupPreview })
  assert.deepEqual(boxAlone.real.json.matched, ["l-ams", "l-fra", "l-par"], "a box condition alone is a condition")
  await step({ method: "POST", path, body: { members: { ...RULE, removed: ["l-fra"] } } }, { status: 400, kind: "validation" })
  const named = await step({ method: "POST", path, body: { members: { ...RULE, sources: among(), include: "FRANK" } } }, { status: 200, schema: GroupPreview })
  assert.deepEqual(named.real.json.matched, ["l-fra"], "a name pattern alone is a condition, matched without case")
  const none = await step({ method: "POST", path, body: { members: { ...RULE, sources: among() } } }, { status: 200, schema: GroupPreview })
  assert.deepEqual(none.real.json.matched, [], "no condition set matches nothing")
  for (const include of ["(", "(?=x)", "[a"]) {
    const refused = await step({ method: "POST", path, body: { members: { ...RULE, include } } }, { status: 422, kind: "validation" })
    assert.equal(refused.real.json.reason, "badPattern", `${include}: a pattern Go can't read names that`)
    assert.ok(refused.real.json.detail, `${include}: and its cause`)
  }
  await step({ method: "POST", path, body: { members: { ...RULE, protocols: among("wireguard") } } }, { status: 422, kind: "validation" })
})

contract("a tick saves at once, keeps the revision, and changes no member until the box condition does", async (step) => {
  const path = "api/groups/eu"
  await step(write(path, "b-new", { body: { name: "EU", members: RULE, ticked: [] }, headers: { "If-None-Match": "*" } }), { status: 200, schema: GroupView })
  const ticked = await step(tick("eu", "l-par", "b-tick"), { status: 200, schema: GroupView })
  assert.deepEqual([ticked.real.json.ticked, ticked.real.json.resolved, ticked.real.json.revision], [["l-par"], ["l-ams", "l-fra", "l-par"], 1], "a tick with the box unset")
  const replay = await step(tick("eu", "l-par", "b-tick"), { status: 200, schema: GroupView })
  assert.deepEqual(replay.real.json, ticked.real.json, "a replay is the first answer")
  await step(tick("eu", "l-nowhere", "b-unknown"), { status: 422, kind: "validation" })
  await step(tick("nobody", "l-par", "b-nobody"), { status: 404, kind: "validation" })
  await step(write("api/groups/eu/ticked/l-ams", "b-odd", { body: { ticked: "yes" } }), { status: 400, kind: "validation" })
  const on = await step(write(path, "b-on", { body: { name: "EU", members: { ...RULE, box: "ticked" } }, headers: { "If-Match": '"1"' } }), { status: 200, schema: GroupView })
  assert.deepEqual([on.real.json.resolved, on.real.json.revision], [["l-par"], 2], "box is ticked: the ticked servers the other conditions match")
  const off = await step(write(path, "b-off", { body: { name: "EU", members: { ...RULE, box: "unticked" } }, headers: { "If-Match": '"2"' } }), { status: 200, schema: GroupView })
  assert.deepEqual(off.real.json.resolved, ["l-ams", "l-fra"], "box is not ticked: the unticked ones")
  const untick = await step(tick("eu", "l-par", "b-untick", false), { status: 200, schema: GroupView })
  assert.deepEqual([untick.real.json.ticked, untick.real.json.resolved], [[], ["l-ams", "l-fra", "l-par"]], "an untick under box is not ticked brings the server in")
  const state = await step({ path: "api/state" }, { status: 200, schema: View })
  assert.deepEqual(state.real.json.groups.find((g) => g.id === "eu").ticked, [])
})

contract("a slot UniFi routes point at can't be deleted", async (step) => {
  for (const d of Object.values(daemons)) d.reference("eu")
  await step({ method: "DELETE", path: "api/slots/eu", key: "ref" }, { status: 409, kind: "referenced" })
  await step({ method: "DELETE", path: "api/slots/eu", key: "ref" }, { status: 409, kind: "referenced" })
})

contract("sources: pasted text is one origin, and a source has no rule and no preview", async (step) => {
  const path = "api/sources/pasted"
  const create = { "If-None-Match": "*" }
  const manual = (text, rest = {}) => ({ name: "Pasted", origin: { kind: "manual", text }, ...rest })
  const LINK = "trojan://pw@paste.example.net:443?security=none#Paste"
  await step(write(path, "p-none", { body: manual(null), headers: create }), { status: 422, kind: "validation" })
  await step(write(path, "p-bad", { body: manual("not a list"), headers: create }), { status: 422, kind: "validation" })
  const created = await step(write(path, "p-new", { body: manual(LINK), headers: create }), { status: 200, schema: SourceView })
  assert.equal(created.real.json.origin.kind, "manual")
  assert.doesNotMatch(created.real.text, /paste\.example\.net/)
  await step({ method: "POST", path: `${path}/refresh`, key: "p-ref" }, { status: 422, kind: "validation" })
  await step(write(path, "p-origin", { body: INPUT, headers: { "If-Match": '"1"' } }), { status: 422, kind: "validation" })
  await step({ method: "POST", path: `${path}/preview`, key: "p-preview", body: { text: LINK } }, { status: 404, kind: "validation" })
  await step({ method: "POST", path: "api/sources/preview", key: "p-text", body: { text: LINK } }, { status: 404, kind: "validation" })
  await step(write(path, "p-rule", { body: manual(null, { pick: null }), headers: { "If-Match": '"1"' } }), { status: 400, kind: "validation" })
  const saved = await step(write(path, "p-name", { body: manual(null, { name: "Renamed" }), headers: { "If-Match": '"1"' } }), { status: 200, schema: SourceView })
  assert.equal(saved.real.json.name, "Renamed")
  for (const [i, file] of ["clash-url-line.yaml", "sing-box-url.json"].entries()) {
    const text = readFileSync(new URL(`../paste/${file}`, import.meta.url), "utf8")
    const taken = await step(write(`api/sources/document-${i}`, `p-doc-${i}`, { body: manual(text), headers: create }), { status: 200, schema: SourceView })
    assert.equal(taken.real.json.origin.kind, "manual", `${file}, with a URL on a line of its own, is pasted text the daemon reads`)
  }
})

contract("sources: secrets go in and never come out", async (step) => {
  const path = "api/sources/borealis"
  await step(write(path, "b-none", { body: INPUT }), { status: 428, kind: "validation" })
  await step(write(path, "b-gone", { body: INPUT, headers: { "If-Match": '"1"' } }), { status: 404, kind: "validation" })
  await step(write(path, "b-nourl", { body: { ...INPUT, origin: { ...ORIGIN, url: null } }, headers: { "If-None-Match": "*" } }), { status: 422, kind: "validation" })
  const created = await step(write(path, "b-new", { body: INPUT, headers: { "If-None-Match": "*" } }), { status: 200, schema: SourceView })
  assert.equal(created.real.json.revision, 1)
  assert.equal(created.real.json.origin.userAgent, "Happ/3.0.0")
  assert.doesNotMatch(created.real.text, /s3cr3t-token/)
  await step(write(path, "b-new", { body: INPUT, headers: { "If-None-Match": "*" } }), { status: 200 })
  const kept = await step(write(path, "b-keep", { body: { ...INPUT, origin: { ...ORIGIN, url: null }, name: "Renamed" }, headers: { "If-Match": '"1"' } }), { status: 200, schema: SourceView })
  assert.equal(kept.real.json.revision, 2)
  for (const d of Object.values(daemons)) assert.equal(d.secrets("borealis").url, ORIGIN.url, "null leaves the stored URL")
  const stale = await step(write(path, "b-stale", { body: INPUT, headers: { "If-Match": '"1"' } }), { status: 412, kind: "staleRevision" })
  assert.equal(stale.real.json.revision, 2)
  await step(write(path, "b-hwid", { body: { ...INPUT, origin: { ...ORIGIN, url: null, hwid: "device-1" } }, headers: { "If-Match": '"2"' } }), { status: 400, kind: "validation" })
  const url = await step({ path: `${path}/url` }, { status: 200, schema: z.strictObject({ url: z.string() }) })
  assert.equal(url.real.json.url, ORIGIN.url)
  await step({ path: "api/sources/none/url" }, { status: 404, kind: "validation" })
  await step({ path: `${path}/refreshes` }, { status: 200, schema: z.array(RefreshEvent) })
  await step({ path: "api/sources/none/refreshes" }, { status: 404, kind: "validation" })
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref" }, { status: 202 })
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref" }, { status: 202 })
  await step({ method: "POST", path: "api/sources/none/refresh", key: "b-ref-gone" }, { status: 404, kind: "validation" })
  const agentOf = async () => (await step({ path: "api/state" }, { status: 200, schema: View })).real.json.sources.find((x) => x.id === "borealis")
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref-same", body: { userAgent: "Happ/3.0.0" } }, { status: 202 })
  assert.equal((await agentOf()).revision, 2, "a refresh with the stored agent leaves the source's revision")
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref-ua", body: { userAgent: "Hiddify/2.5.7" } }, { status: 202 })
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref-ua", body: { userAgent: "Hiddify/2.5.7" } }, { status: 202 })
  const agented = await agentOf()
  assert.deepEqual([agented.origin.userAgent, agented.revision], ["Hiddify/2.5.7", 3], "a refresh with another agent stores it, with no precondition, and moves the revision once")
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref-ua", body: { userAgent: "v2rayN/7.13.2" } }, { status: 422, kind: "validation" })
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref-long", body: { userAgent: "x".repeat(201) } }, { status: 422, kind: "validation" })
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref-line", body: { userAgent: "Happ\n3" } }, { status: 422, kind: "validation" })
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref-key", body: { userAgent: "Happ/3.0.0", url: "https://x.example" } }, { status: 400, kind: "validation" })
  await step({ method: "POST", path: `${path}/refresh`, key: "b-ref-default", body: { userAgent: "" } }, { status: 202 })
  assert.equal((await agentOf()).origin.userAgent, "Happ/3.0.0", "an empty agent is the default, as a save's is")
  await step(write(path, "b-ref-stale", { body: { ...INPUT, origin: { ...ORIGIN, url: null } }, headers: { "If-Match": '"2"' } }), { status: 412, kind: "staleRevision" })
  await step({ method: "DELETE", path, key: "b-del" }, { status: 200 })
  await step({ method: "DELETE", path, key: "b-del-2" }, { status: 404, kind: "validation" })
  const reborn = await step(write(path, "b-reborn", { body: INPUT, headers: { "If-None-Match": "*" } }), { status: 200, schema: SourceView })
  assert.equal(reborn.real.json.revision, 5, "an id's revisions carry on past a delete")
  const state = await step({ path: "api/state" }, { status: 200, schema: View })
  assert.doesNotMatch(state.real.text, /s3cr3t-token/)
})

contract("the probe answers once per key", async (step) => {
  const first = await step({ method: "POST", path: "api/servers/l-ams/probe", key: "p" }, { status: 200, schema: ProbeResult })
  assert.equal(first.real.json.lineage, "l-ams")
  const replay = await step({ method: "POST", path: "api/servers/l-ams/probe", key: "p" }, { status: 200, schema: ProbeResult })
  assert.deepEqual(replay.real.json, first.real.json)
  await step({ method: "POST", path: "api/servers/l-fra/probe", key: "p" }, { status: 422, kind: "validation" })
  await step({ method: "POST", path: "api/servers/l-none/probe", key: "p-none" }, { status: 404, kind: "validation" })
  await step({ method: "POST", path: "api/servers/l-none/probe", key: "p-none" }, { status: 404, kind: "validation" })
})

contract("a press tests the server and moves the owner's slots, once per key", async (step) => {
  const press = (owner, server, key) => ({ method: "POST", path: `api/${owner}/preferred/${server}`, key })
  const first = await step(press(`groups/${AURORA}`, "l-fra", "pr"), { status: 200, schema: ProbeResult })
  assert.equal(first.real.json.lineage, "l-fra")
  for (const [who, answer] of Object.entries(first)) {
    assert.equal(answer.after.slots.find((slot) => slot.id === "eu").server, "l-fra", `${who}: the read right after the press, at its revision, has the move staged`)
  }
  const replay = await step(press(`groups/${AURORA}`, "l-fra", "pr"), { status: 200, schema: ProbeResult })
  assert.deepEqual(replay.real.json, first.real.json)
  await step(press(`groups/${AURORA}`, "l-none", "pr-none"), { status: 404, kind: "validation" })
  await step(press("groups/gone", "l-ams", "pr-gone"), { status: 404, kind: "validation" })
  await step(press("sources/aurora", "l-ams", "pr-idle"), { status: 422, kind: "validation" })
})

contract("Clear names the deadline it drops, and moves nothing", async (step) => {
  const clear = (server, endsAt, key) => ({ method: "DELETE", path: `api/groups/${AURORA}/preferred/${server}${endsAt === null ? "" : `?endsAt=${encodeURIComponent(endsAt)}`}`, key })
  await step(clear("l-fra", new Date().toISOString(), "cl"), { status: 200 })
  await step(clear("l-fra", null, "cl-none"), { status: 400, kind: "validation" })
  await step({ method: "DELETE", path: `api/groups/gone/preferred/l-fra?endsAt=${encodeURIComponent(new Date().toISOString())}`, key: "cl-gone" }, { status: 404, kind: "validation" })
  const state = await step({ path: "api/state" }, { status: 200, schema: View })
  for (const [who, answer] of Object.entries(state)) {
    assert.equal(answer.json.slots.find((s) => s.id === "eu").server, "l-ams", `${who}: Clear moved nothing`)
  }
})

contract("a route that doesn't exist refuses under the same checks", async (step) => {
  await step({ method: "POST", path: "api/nothing", csrf: "stale", key: "n" }, { status: 403, kind: "csrf" })
  await step({ method: "POST", path: "api/nothing", key: "n" }, { status: 404, kind: "validation" })
  await step({ method: "POST", path: "api/state", key: "n2" }, { status: 404, kind: "validation" })
  await step({ method: "POST", path: "api/nothing", key: "n" }, { status: 404, kind: "validation" })
})

test("the page's files and a deep link come from under the base path, on both", async () => {
  for (const d of Object.values(daemons)) {
    for (const [route, type] of [["", "text/html"], ["slots/eu", "text/html"], ["favicon.svg", "image/svg+xml"], ["assets/a.js", "text/javascript"]]) {
      const res = await fetch(d.url + route)
      assert.equal(res.status, 200, route)
      assert.ok(res.headers.get("content-type").startsWith(type), `${route}: ${res.headers.get("content-type")}`)
    }
  }
})

test("a change event arrives over the stream on both, in the same frame form", async () => {
  for (const d of Object.values(daemons)) {
    d.reset()
    const stream = new AbortController()
    const res = await fetch(d.url + "api/events", { signal: stream.signal })
    assert.equal(res.status, 200)
    assert.match(res.headers.get("content-type"), /^text\/event-stream/)
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    let frames = ""
    const readUntil = async (pattern) => {
      while (!pattern.test(frames)) {
        const { value, done } = await reader.read()
        if (done) throw new Error("the stream closed")
        frames += decoder.decode(value)
      }
    }
    await readUntil(/: connected\n\n/)
    d.renameSlot("eu", "Moved")
    await readUntil(/id: \S+\ndata: \{.*"kind":"slot".*\}\n\n/)
    const data = /data: (\{.*"kind":"slot".*\})\n\n/.exec(frames)[1]
    const event = JSON.parse(data)
    assert.equal(event.slot, "eu")
    assert.equal(event.resync, false)
    stream.abort()
  }
})
