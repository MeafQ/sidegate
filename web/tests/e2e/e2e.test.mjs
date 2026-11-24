// End-to-end checks of the page in a real browser, against a fake daemon serving a build of it. Run with
// `npm run test:e2e`.
import assert from "node:assert/strict"
import { rmSync } from "node:fs"
import { after, before, beforeEach, describe, test } from "node:test"
import { fileURLToPath } from "node:url"
import { chromium } from "playwright-core"
import { build } from "vite"
import { answered, startDaemon } from "./daemon-under-test.mjs"

const ROOT = fileURLToPath(new URL("../../", import.meta.url))
const DIST = fileURLToPath(new URL("./.dist-live/", import.meta.url))
const PORT = Number(process.env.E2E_PORT ?? 5311)
const LIVE_BASE = "/proxy/"

let browser
let daemon
/** A name that isn't loopback, so the browser treats the page as the gateway's plain-HTTP LAN address: no secure context. */
const LAN_HOST = "gateway.test"

before(async () => {
  rmSync(DIST, { recursive: true, force: true })
  process.env.BASE_PATH = LIVE_BASE
  await build({ root: ROOT, logLevel: "warn", build: { outDir: DIST, emptyOutDir: true } })
  delete process.env.BASE_PATH
  daemon = await startDaemon({ port: PORT, dist: DIST, base: LIVE_BASE })
  browser = await chromium.launch({ args: [`--host-resolver-rules=MAP ${LAN_HOST} 127.0.0.1`] })
})

after(async () => {
  await browser?.close()
  await daemon?.close()
  rmSync(DIST, { recursive: true, force: true })
})

/** A fresh browser context per check, so no check sees another's storage or session. */
async function open(url, { theme = "dark", width = 1440 } = {}) {
  const context = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: theme })
  await context.addInitScript((t) => localStorage.setItem("theme", t), theme)
  const page = await context.newPage()
  page.setDefaultTimeout(10_000)
  const consoleErrors = []
  page.on("console", (m) => { if (m.type() === "error") consoleErrors.push(m.text()) })
  await page.goto(url)
  return { page, consoleErrors, close: () => context.close() }
}

async function until(check, { timeout = 10_000, message = "condition" } = {}) {
  const end = Date.now() + timeout
  for (;;) {
    const value = await check()
    if (value) return value
    if (Date.now() > end) assert.fail(`timed out waiting for ${message}`)
    await new Promise((r) => setTimeout(r, 100))
  }
}


const SETTINGS_BODY = { analyticsRetentionDays: 4 }

const toasts = (page) => page.locator("[data-sonner-toast]")
const puts = (path) => daemon.log().filter((r) => r.method === "PUT" && r.path === LIVE_BASE + path)
/** Aurora's refreshes, apart from the check for a newer release the page makes as it loads. */
const refreshPosts = () => daemon.log().filter((r) => r.method === "POST" && r.path === LIVE_BASE + "api/sources/aurora/refresh")
const putsUnder = (prefix) => daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(LIVE_BASE + prefix))

/** Lets a write reach the daemon, then resets the connection: the write landed, and the page can't know. */
const landThenDrop = async (route) => { await route.fetch(); await route.abort("connectionreset") }

async function addMenu(page, item) {
  await page.getByRole("navigation", { name: "Main" }).getByRole("button", { name: "Add", exact: true }).click()
  await page.getByRole("menuitem", { name: item, exact: true }).click()
}

/** Opens a new group's draft from Add and ticks one server in it, which makes it a group the daemon will store. */
async function draftGroup(page) {
  await addMenu(page, "Group")
  await page.getByLabel("Name of New group").waitFor()
  await page.getByRole("checkbox", { name: "Select Paris", exact: true }).click()
}
const saveGroup = (page) => page.getByRole("group", { name: "Unsaved changes" }).getByRole("button", { name: "Save", exact: true })

/** A subscription's name follows its provider, so its dialog takes only a URL. */
async function addDialog(page, { url = "https://a.example/sub" } = {}) {
  await addMenu(page, "Source")
  await page.getByRole("dialog").getByLabel("Source", { exact: true }).fill(url)
  return page.getByRole("dialog").getByRole("button", { name: "Add", exact: true })
}

async function manualDialog(page, { name = "Borealis", text = "trojan://pw@b.example:443#Borealis one" } = {}) {
  await addMenu(page, "Source")
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Source", { exact: true }).fill(text)
  await dialog.getByLabel("Name").fill(name)
  return dialog.getByRole("button", { name: "Add", exact: true })
}

async function renameTile(page, from, to) {
  await page.getByRole("button", { name: `Rename ${from}` }).click()
  const input = page.getByLabel(`Name of ${from}`)
  await input.fill(to)
  await input.press("Enter")
}

const tile = (page, name) => page.getByRole("group", { name, exact: true })

/** A source page's edit that isn't its name, so Save comes up in the bar under the table. */
/** Aurora's settings, opened from its tab while they aren't. */
const settingsOf = (page) => page.getByRole("dialog", { name: "Settings" })
async function editRefresh(page, label = "12h") {
  const refresh = settingsOf(page).getByRole("radiogroup", { name: "Refresh" })
  if (!(await refresh.isVisible())) await page.getByRole("button", { name: /^Settings for / }).click()
  await refresh.getByRole("radio", { name: label, exact: true }).click()
}
const settingsSave = (page) => settingsOf(page).getByRole("button", { name: "Save", exact: true })
/** The list names Aurora as another writer renamed it, behind the settings while they're open. */
const theirsListed = (page) => page.getByRole("navigation", { name: "Main", includeHidden: true }).getByRole("link", { name: /Theirs \d/, includeHidden: true }).waitFor({ state: "attached" })

const added = () => daemon.sources().filter((s) => s.id !== "aurora")
const SOURCE_WRITE = /\/api\/sources\/[^/]+$/

/** WCAG contrast of an element's text against the first opaque background behind it. */
const contrastOf = (locator) => locator.evaluate((el) => {
  const canvas = document.createElement("canvas")
  canvas.width = canvas.height = 1
  const g = canvas.getContext("2d", { willReadFrequently: true })
  const rgb = (c) => { g.clearRect(0, 0, 1, 1); g.fillStyle = c; g.fillRect(0, 0, 1, 1); return [...g.getImageData(0, 0, 1, 1).data].slice(0, 3) }
  const lum = (c) => { const [r, gg, b] = c.map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4 }); return 0.2126 * r + 0.7152 * gg + 0.0722 * b }
  let node = el
  let bg = "rgba(0, 0, 0, 0)"
  while (node && /rgba\(0, 0, 0, 0\)|transparent/.test(bg)) { bg = getComputedStyle(node).backgroundColor; node = node.parentElement }
  const [hi, lo] = [lum(rgb(getComputedStyle(el).color)), lum(rgb(bg))].sort((a, b) => b - a)
  return (hi + 0.05) / (lo + 0.05)
})

describe("live, against a fake daemon", () => {
  beforeEach(() => daemon.reset())

  test("0. the fake daemon leaves a key free after a 5xx, so the same request sent again is handled as new", async () => {
    daemon.set({ writeFails: 1 })
    const put = () => fetch(daemon.url + "api/settings", {
      method: "PUT", body: JSON.stringify(SETTINGS_BODY),
      headers: { "X-Sidegate-Csrf": "token-1", "Content-Type": "application/json", "Idempotency-Key": "after-a-5xx", "If-Match": '"5"' },
    })
    assert.equal((await put()).status, 503)
    assert.equal(daemon.settings().revision, 5)
    assert.equal((await put()).status, 200)
    assert.equal(daemon.settings().revision, 6)
    assert.equal(daemon.log().filter((r) => r.replayed).length, 0, "it was no replay")
  })

  test("0. the fake daemon takes a mutation with no body and no Content-Type, and refuses a body that isn't JSON", async () => {
    const post = (headers, body) => fetch(daemon.url + "api/sources/aurora/refresh", {
      method: "POST", body, headers: { "X-Sidegate-Csrf": "token-1", "Idempotency-Key": crypto.randomUUID(), ...headers },
    })
    const bodyless = await post({})
    assert.equal(bodyless.status, 202, "refresh has no body, and needs no Content-Type")
    assert.equal(daemon.log().at(-1).contentType, null)
    assert.equal((await post({ "Content-Type": "text/plain" }, "hello")).status, 415, "a body must be JSON")
    assert.equal((await post({ "Idempotency-Key": "" })).status, 400, "the key is required with or without a body")
    assert.equal((await post({ "X-Sidegate-Csrf": "stale" })).status, 403, "and so is the token")
  })

  test("0. the fake daemon holds writes to the contract: a key and a JSON body on each, a replayed key answered as first", async () => {
    const put = (headers, body) => fetch(daemon.url + "api/settings", {
      method: "PUT", body, headers: { "X-Sidegate-Csrf": "token-1", "Content-Type": "application/json", "Idempotency-Key": "k1", ...headers },
    })
    const body = JSON.stringify(SETTINGS_BODY)
    const over5 = { "If-Match": '"5"' }
    assert.equal((await put({ ...over5, "Idempotency-Key": "" }, body)).status, 400, "no Idempotency-Key")
    assert.equal((await put({ ...over5, "Content-Type": "text/plain" }, body)).status, 415, "not JSON")
    assert.equal(daemon.settings().revision, 5, "neither was applied")
    const first = await put(over5, body)
    const again = await put(over5, body)
    assert.equal(first.status, 200)
    assert.equal(again.status, 200, "the replay gets the first answer, not the 412 a second write would")
    assert.deepEqual(await again.json(), await first.json())
    assert.equal(daemon.settings().revision, 6, "applied once")
    assert.equal((await put(over5, JSON.stringify({ ...SETTINGS_BODY, analyticsRetentionDays: 5 }))).status, 422, "the key can't carry another body")
    assert.equal((await put({ "If-Match": '"6"' }, body)).status, 422, "nor another revision")
    assert.equal((await put({ "If-None-Match": "*" }, body)).status, 422, "nor a create")
    assert.equal(daemon.log().filter((r) => r.replayed).length, 1, "only the same request is replayed")
  })

  test("0. the fake daemon's 412 names the revision stored when it refused", async () => {
    daemon.changeSettings({ analyticsRetentionDays: 4 })
    const res = await fetch(daemon.url + "api/settings", {
      method: "PUT", body: JSON.stringify(SETTINGS_BODY),
      headers: { "X-Sidegate-Csrf": "token-1", "Content-Type": "application/json", "Idempotency-Key": "k2", "If-Match": '"5"' },
    })
    assert.equal(res.status, 412)
    assert.equal(res.headers.get("etag"), '"6"')
    assert.deepEqual(await res.json(), { kind: "staleRevision", reason: "stale", message: "Changed by another editor.", revision: 6 })
  })

  test("1. a slot rename based on an old revision gets the conflict, and never overwrites", async () => {
    const { page, close } = await open(daemon.url)
    try {
      await tile(page, "EU").waitFor()
      // The open feed still tells the page of the rename, and Go follows it with more events than the fake does, so
      // every read of the state is refused until the page has sent its PUT.
      let sent = false
      page.on("request", (r) => { if (r.method() === "PUT" && r.url().endsWith("api/slots/eu")) sent = true })
      await page.route("**/api/state", (route) => (sent ? route.continue() : route.abort()))
      daemon.renameSlot("eu", "EU theirs")
      await renameTile(page, "EU", "EU mine")
      const [put] = await until(() => puts("api/slots/eu").length === 1 && answered(puts("api/slots/eu")), { message: "the PUT's answer" })
      assert.equal(put.ifMatch, '"3"', "If-Match carries the revision the tile showed")
      assert.equal(put.status, 412)
      const conflict = toasts(page).filter({ hasText: "changed to EU theirs" })
      await conflict.waitFor()
      assert.equal((await conflict.innerText()).replace(/\s+/g, " ").trim(), "EU changed to EU theirs Your value: EU mine Keep mine", "what is stored now, then the reader's own")
      assert.equal(daemon.slot("eu").name, "EU theirs", "the other writer's change stands")
      await tile(page, "EU theirs").waitFor()
    } finally {
      await close()
    }
  })

  /** Holds the page's reads of the settings back until it has sent a PUT, so the page saves over what it last read while another
   * writer has saved since. */
  async function unheardUntilSaved(page) {
    let sent = false
    page.on("request", (r) => { if (r.method() === "PUT" && r.url().endsWith("api/settings")) sent = true })
    await page.route("**/api/settings", (route) => (route.request().method() === "GET" && !sent ? route.abort() : route.continue()))
    return () => { sent = false }
  }
  const conflictToast = (page) => toasts(page).filter({ hasText: "changed in another tab" })
  const shownText = async (locator) => (await locator.innerText()).replace(/\s+/g, " ").trim()
  /** Whether the one conflict toast on screen says `words`. One read of every match, since a toast said again replaces its last,
   * and for a frame or two both are there. */
  const oneConflict = async (page, words) => {
    const shown = await conflictToast(page).allInnerTexts()
    return shown.length === 1 && shown[0].replace(/\s+/g, " ").trim() === words
  }

  const retentionOf = (page, word) => page.getByRole("radiogroup", { name: "Retention" }).getByRole("radio", { name: word, exact: true })
  const picked = async (page, word) => (await retentionOf(page, word).getAttribute("aria-checked")) === "true"

  test("1. a settings save over a revision another writer moved is a conflict: the pick stays, one toast names what is stored, and Save saves the pick over it", async () => {
    const { page, close } = await open(daemon.url + "settings")
    try {
      const save = page.getByRole("button", { name: "Save", exact: true }).last()
      await until(() => picked(page, "30d"), { message: "settings form" })
      await retentionOf(page, "90d").click()
      await unheardUntilSaved(page)
      daemon.changeSettings({ analyticsRetentionDays: 7 })
      await save.click()
      const conflict = conflictToast(page)
      await conflict.waitFor()
      assert.equal(await shownText(conflict), "Settings changed in another tab Retention 7d", "what is stored now, and no button")
      const [put] = puts("api/settings")
      assert.deepEqual([put.ifMatch, put.status], ['"5"', 412], "over the revision the page held")
      assert.equal(daemon.settings().analyticsRetentionDays, 7, "the other writer's save stands")
      assert.equal(await picked(page, "90d"), true, "the pick stays")
      await until(() => save.isEnabled(), { message: "Save is the conflict's one fix" })
      await save.click()
      await until(() => answered(puts("api/settings")) && puts("api/settings").length === 2, { message: "the second PUT's answer" })
      assert.deepEqual(puts("api/settings").map((r) => [r.ifMatch, r.status]), [['"5"', 412], ['"6"', 200]], "the pick saved over what is stored")
      assert.deepEqual(daemon.settings(), { analyticsRetentionDays: 90, checkForUpdates: true, revision: 7 }, "the reader's pick over the other writer's")
      await conflict.waitFor({ state: "detached" })
    } finally {
      await close()
    }
  })

  test("1. a third writer before the page reads again is a conflict again, never an overwrite", async () => {
    const { page, close } = await open(daemon.url + "settings")
    try {
      const save = page.getByRole("button", { name: "Save", exact: true }).last()
      await until(() => picked(page, "30d"), { message: "settings form" })
      await retentionOf(page, "90d").click()
      const again = await unheardUntilSaved(page)
      daemon.changeSettings({ analyticsRetentionDays: 7 })
      await save.click()
      await until(() => oneConflict(page, "Settings changed in another tab Retention 7d"), { message: "the first conflict" })
      again()
      daemon.changeSettings({ analyticsRetentionDays: 365 })
      await until(() => save.isEnabled(), { message: "Save after the first conflict" })
      await save.click()
      await until(() => puts("api/settings").length === 2 && answered(puts("api/settings")), { message: "the second PUT's answer" })
      assert.deepEqual(puts("api/settings").map((r) => [r.ifMatch, r.status]), [['"5"', 412], ['"6"', 412]], "each over the revision the page held")
      assert.equal(daemon.settings().analyticsRetentionDays, 365, "the third writer's change stands")
      await until(() => oneConflict(page, "Settings changed in another tab Retention 1y"), { message: "one conflict, the second, naming what moved" })
      assert.equal(await picked(page, "90d"), true, "the pick stays")
    } finally {
      await close()
    }
  })


  /** Aurora's settings saved while another writer's rename is unheard: the page's reads of the state are held back until its PUT. */
  async function sourceConflict(page) {
    await page.getByRole("button", { name: "Settings for Aurora" }).click()
    const refresh = settingsOf(page).getByRole("radiogroup", { name: "Refresh" }).getByRole("radio", { name: "12h", exact: true })
    await refresh.waitFor()
    await editRefresh(page)
    let sent = false
    page.on("request", (r) => { if (r.method() === "PUT" && r.url().endsWith("api/sources/aurora")) sent = true })
    await page.route("**/api/state", (route) => (sent ? route.continue() : route.abort()))
    await daemon.renameSource("aurora", "Theirs")
    const save = settingsSave(page)
    await save.click()
    const conflict = toasts(page).filter({ hasText: "changed in another tab" })
    await conflict.waitFor()
    assert.equal(await refresh.getAttribute("aria-checked"), "true", "the draft stays")
    return { refresh, save, conflict }
  }

  test("1. a source edit over another writer's unheard save is a conflict: one toast with no button, the draft kept, and Save saves it over what is stored", async () => {
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      const { save, conflict } = await sourceConflict(page)
      assert.equal((await conflict.innerText()).replace(/\s+/g, " ").trim(), "Aurora changed in another tab", "the reader's field didn't move, so no value is named")
      assert.equal(await conflict.getByRole("button").count(), 1, "its × alone")
      assert.equal(await save.isEnabled(), true)
      await save.click()
      await until(() => daemon.source("aurora").origin.refreshEverySeconds === 43200 && answered(puts("api/sources/aurora")), { message: "the save's answer" })
      assert.deepEqual(puts("api/sources/aurora").map((r) => [r.ifMatch, r.status]), [['"1"', 412], ['"2"', 200]])
      assert.equal(daemon.source("aurora").name, "Theirs", "their rename, untouched here, stands")
      await conflict.waitFor({ state: "detached" })
    } finally {
      await close()
    }
  })

  test("1. Cancel after a conflict drops the draft and its toast, and the settings open again on what is stored", async () => {
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      const { refresh, conflict } = await sourceConflict(page)
      await settingsOf(page).getByRole("button", { name: "Cancel", exact: true }).click()
      await settingsOf(page).waitFor({ state: "hidden" })
      await conflict.waitFor({ state: "detached" })
      await page.getByRole("button", { name: "Settings for Theirs" }).click()
      await until(async () => (await refresh.getAttribute("aria-checked")) === "false", { message: "the stored refresh in the form" })
      assert.equal(await settingsSave(page).isDisabled(), true, "nothing to save")
      assert.deepEqual(puts("api/sources/aurora").map((r) => [r.ifMatch, r.status]), [['"1"', 412]])
    } finally {
      await close()
    }
  })

  test("2. the change feed reconnects after a failed reconnect, and catches up on what it missed", async () => {
    const { page, close } = await open(daemon.url)
    try {
      await until(() => daemon.sseClients() === 1, { message: "the first SSE connection" })
      daemon.set({ eventsFail: 1 })
      daemon.dropEvents()
      daemon.renameSlot("eu", "EU while away")
      await tile(page, "EU while away").waitFor({ timeout: 20_000 })
      await until(() => daemon.sseClients() === 1, { message: "a live SSE connection again" })
    } finally {
      await close()
    }
  })

  test("2. a change made while the feed's first connection failed is caught up once it connects", async () => {
    daemon.set({ eventsFail: 1000 })
    const { page, close } = await open(daemon.url)
    try {
      await tile(page, "EU").waitFor()
      await until(() => daemon.log().some((r) => r.path.endsWith("api/events") && r.status === 502), { message: "a refused feed" })
      daemon.renameSlot("eu", "EU meanwhile")
      daemon.set({ eventsFail: 0 })
      await until(() => daemon.sseClients() === 1, { timeout: 20_000, message: "the feed connects" })
      await tile(page, "EU meanwhile").waitFor({ timeout: 5000 })
    } finally {
      await close()
    }
  })

  test("2. while the gateway refuses the feed, the page retries it with backoff, whatever the browser's own retry", async () => {
    const context = await browser.newContext()
    const page = await context.newPage()
    try {
      const attempts = []
      await page.route("**/api/events", (route) => {
        attempts.push(Date.now())
        if (attempts.length === 1) return route.fulfill({ status: 200, contentType: "text/event-stream", body: "retry: 100\n: connected\n\n" })
        return route.abort("connectionrefused")
      })
      await page.goto(daemon.url)
      await until(() => attempts.length > 0, { message: "the first connection" })
      await page.waitForTimeout(5000)
      const gaps = attempts.slice(1).map((t, i) => t - attempts[i])
      assert.ok(attempts.length <= 4, `${attempts.length} connections in 5 s, gaps ${gaps.join(", ")} ms`)
      assert.ok(gaps.slice(1).every((g, i) => g >= 900 * 2 ** (i + 1)), `each wait doubles: ${gaps.join(", ")} ms`)
    } finally {
      await context.close()
    }
  })

  test("2. a feed that opens and drops at once backs off to 30 s, and the wait starts over only after 30 s up", async () => {
    const context = await browser.newContext()
    await context.clock.install()
    const page = await context.newPage()
    try {
      const feed = { live: true, attempts: 0 }
      await page.route("**/api/events", (route) => {
        feed.attempts++
        return feed.live ? route.continue() : route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\n" })
      })
      await page.goto(daemon.url)
      await until(() => daemon.sseClients() === 1, { message: "the first SSE connection" })
      await page.clock.pauseAt(Date.now() + 1000)
      const states = () => daemon.log().filter((r) => r.path === LIVE_BASE + "api/state").length
      /** Moves the page's clock to just before the wait ends, then to its end: the feed connects again then, not before. */
      const waits = async (ms) => {
        const before = feed.attempts
        await page.waitForTimeout(500)
        await page.clock.runFor(ms - 1)
        await page.waitForTimeout(500)
        assert.equal(feed.attempts, before, `no connection before ${ms} ms`)
        await page.clock.runFor(1)
        await until(() => feed.attempts > before, { message: `a connection at ${ms} ms` })
      }
      feed.live = false
      daemon.dropEvents()
      const resyncsBefore = states()
      for (const ms of [1000, 2000, 4000, 8000, 16000, 30_000, 30_000]) await waits(ms)
      assert.ok(states() - resyncsBefore <= 7, `${states() - resyncsBefore} state reads for 7 connections`)
      feed.live = true
      await waits(30_000)
      await until(() => daemon.sseClients() === 1, { message: "a live connection" })
      await page.waitForTimeout(500)
      await page.clock.runFor(29_000)
      daemon.dropEvents()
      await waits(30_000)
      await until(() => daemon.sseClients() === 1, { message: "a live connection again" })
      await page.waitForTimeout(500)
      await page.clock.runFor(30_000)
      daemon.dropEvents()
      await waits(1000)
    } finally {
      await context.close()
    }
  })

  test("3. a failed session fetch is read again inside the same write, under its key, and lands once", async () => {
    const { page, close } = await open(daemon.url + "servers")
    try {
      /* The check for a newer release the page makes as it loads reads the token first, so it reads none and the write is the first to. */
      const load = /\/api\/(session|update\/check)$/
      await page.route(load, (route) => route.abort())
      await page.goto(daemon.url + "sources/aurora")
      const refresh = page.locator("[data-slot=source-heading]").getByRole("button", { name: /^Refresh/ })
      await refresh.waitFor()
      await page.waitForTimeout(500)
      await page.unroute(load)
      daemon.set({ sessionFails: 1 })
      await refresh.click()
      const posts = await until(() => {
        const p = refreshPosts()
        return p.some((r) => r.status === 202) && p
      }, { message: "the refresh lands" })
      await page.waitForTimeout(500)
      assert.deepEqual(refreshPosts().map((r) => r.status), [403, 202], "it lands once, after the attempt with no token")
      assert.ok(posts.every((r) => r.idempotencyKey === posts[0].idempotencyKey), "every attempt under its key")
      assert.equal(refreshPosts().at(-1).csrf, "token-1", "with the token read again")
      assert.equal(await toasts(page).count(), 0, "with nothing to say")
    } finally {
      await close()
    }
  })

  test("3. an expired CSRF token is fetched again and the write retried once, with the same idempotency key", async () => {
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      const refresh = page.locator("[data-slot=source-heading]").getByRole("button", { name: /^Refresh/ })
      await refresh.click()
      await until(() => refreshPosts().some((r) => r.status === 202), { message: "the first refresh" })
      /* Refresh takes no press until the daemon has checked again, which the fake never does. Leaving the tab and coming back
       * drops the page's note that a refresh runs, and keeps the token the page holds. */
      const list = page.getByRole("navigation", { name: "Main" })
      await list.getByRole("link", { name: /^All servers/ }).click()
      await list.locator('a[href$="/sources/aurora"]').click()
      await refresh.and(page.locator(":enabled")).waitFor()
      daemon.rotateToken()
      await refresh.click()
      const posts = await until(() => {
        const p = refreshPosts()
        return p.length >= 3 && answered(p)
      }, { message: "the retried refresh" })
      await page.waitForTimeout(500)
      const all = refreshPosts()
      assert.deepEqual(all.map((r) => r.status), [202, 403, 202], "one rejection, one retry")
      assert.equal(posts[1].idempotencyKey, posts[2].idempotencyKey, "the retry is the same write")
      assert.equal(posts[2].csrf, "token-2")
    } finally {
      await close()
    }
  })

  test("3. behind UniFi's CSRF check an add lands, with UniFi's token in X-Csrf-Token and the daemon's in its own header", async () => {
    daemon.set({ unifi: "unifi-1" })
    const { page, close } = await open(daemon.url + "servers")
    try {
      const add = await manualDialog(page)
      await add.click()
      await page.getByRole("dialog").waitFor({ state: "hidden" })
      const puts = daemon.log().filter((r) => r.method === "PUT")
      assert.deepEqual(puts.map((r) => [r.status, r.unifiCsrf, r.csrf]), [[200, "unifi-1", "token-1"]])
      assert.equal(daemon.sources().filter((s) => s.name === "Borealis").length, 1)
    } finally {
      await close()
    }
  })

  test("3. a stale UniFi token is fetched again and the write sent once more, under the same key, landing once", async () => {
    daemon.set({ unifi: "unifi-1" })
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      const refresh = page.locator("[data-slot=source-heading]").getByRole("button", { name: /^Refresh/ })
      /* A new UniFi session begins while the refresh is on its way, so it carries the old token. */
      await page.route("**/api/sources/aurora/refresh", async (route) => {
        daemon.set({ unifi: "unifi-2" })
        await route.continue()
      }, { times: 1 })
      await refresh.click()
      const posts = await until(() => {
        const p = refreshPosts()
        return p.length >= 2 && answered(p) && p
      }, { message: "the write sent again" })
      await page.waitForTimeout(500)
      assert.deepEqual(refreshPosts().map((r) => [r.status, r.unifiCsrf]), [[403, "unifi-1"], [202, "unifi-2"]], "UniFi's refusal, then the write with the new token")
      assert.equal(posts[0].idempotencyKey, posts[1].idempotencyKey, "the retry is the same write")
    } finally {
      await close()
    }
  })

  test("3. behind UniFi's CSRF check a group's create and the previews it asks carry UniFi's token too", async () => {
    daemon.set({ unifi: "unifi-1" })
    const { page, close } = await open(daemon.url + "servers")
    try {
      await draftGroup(page)
      await saveGroup(page).click()
      await page.waitForURL(/\/groups\/(?!new$)[^/]+$/)
      const writes = await until(() => answered(daemon.log().filter((r) => r.method !== "GET")), { message: "the group's writes answered" })
      assert.ok(writes.some((r) => r.path === LIVE_BASE + "api/groups/preview"), "a preview was asked")
      assert.ok(writes.some((r) => r.method === "PUT"), "the group was created")
      assert.deepEqual(writes.filter((r) => r.unifiCsrf !== "unifi-1" || r.status >= 300), [], "each carried UniFi's token and passed")
    } finally {
      await close()
    }
  })

  test("3. a page that has seen no UniFi token sends none", async () => {
    const { page, close } = await open(daemon.url + "servers")
    try {
      const add = await manualDialog(page)
      await add.click()
      await page.getByRole("dialog").waitFor({ state: "hidden" })
      const writes = daemon.log().filter((r) => r.method !== "GET")
      assert.ok(writes.length > 0, "the add was sent")
      assert.deepEqual(writes.filter((r) => r.unifiCsrf !== null), [], "no X-Csrf-Token on any write")
    } finally {
      await close()
    }
  })

  test("4. creating a group never targets an existing id", async () => {
    const { page, close } = await open(daemon.url + "servers")
    try {
      await page.locator("thead").getByRole("button", { name: "Filter servers by Country", exact: true }).click()
      await page.getByRole("dialog").getByRole("option").first().click()
      await page.keyboard.press("Escape")
      await page.getByRole("button", { name: "Save as group" }).click()
      const [put] = await until(() => answered(daemon.log().filter((r) => r.method === "PUT")), { message: "the create's answer" })
      assert.equal(put.ifNoneMatch, "*", "a create says so")
      assert.equal(put.ifMatch, null)
      assert.notEqual(put.path, LIVE_BASE + "api/groups/all-aurora")
      assert.equal(put.status, 200)
      await page.getByLabel("Name of New group").waitFor()
      assert.equal(daemon.group("all-aurora").name, "All Aurora", "the existing group is untouched")
    } finally {
      await close()
    }
  })

  test("4. a 412 on a new group's create is that create, landed earlier: no toast, no blame, and the page stays", async () => {
    const { page, close } = await open(daemon.url + "servers")
    try {
      await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: /All Aurora \d/ }).waitFor()
      const refused = { kind: "staleRevision", reason: "stale", message: "Precondition failed.", revision: 1 }
      let answered412 = 0
      await page.route(/\/api\/groups\/(?!preview$)[^/]+$/, (route) => {
        answered412++
        return route.fulfill({ status: 412, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify(refused) })
      })
      await draftGroup(page)
      await saveGroup(page).click()
      await until(() => answered412 === 1, { message: "the create's 412" })
      await page.waitForTimeout(1500)
      assert.equal(answered412, 1, "settled, not sent again")
      assert.equal(await toasts(page).count(), 0, "no toast, no blame")
      assert.equal(new URL(page.url()).pathname, LIVE_BASE + "groups/new", "the page stays where the user was")
      assert.equal(daemon.log().filter((r) => r.method === "PUT").length, 0, "the answer came before the daemon")
      assert.equal(await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: /New group/ }).count(), 0)
    } finally {
      await close()
    }
  })

  test("4. an add whose first answer is lost is sent again under its key, and the daemon's replay closes the dialog onto the one source", async () => {
    const { page, close } = await open(daemon.url + "servers")
    try {
      const add = await addDialog(page, { url: "https://b.example/sub" })
      await page.route(SOURCE_WRITE, landThenDrop, { times: 1 })
      await add.click()
      await page.getByRole("dialog").waitFor({ state: "hidden" })
      const writes = putsUnder("api/sources/")
      assert.deepEqual(writes.map((w) => [w.status, w.replayed]), [[200, false], [200, true]], "the second is the daemon replaying the first")
      assert.equal(writes[0].idempotencyKey, writes[1].idempotencyKey)
      assert.equal(writes[0].body, writes[1].body, "the same request byte for byte")
      assert.equal(added().length, 1, "added once")
      await page.waitForURL(new RegExp(`/sources/${added()[0].id}$`))
      assert.equal(await toasts(page).count(), 0, "no blame, no toast")
    } finally {
      await close()
    }
  })

  test("4. a rename whose answer is lost is sent again under its key, and the next rename saves over it with no conflict", async () => {
    daemon.set({ trimNames: true })
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      const name = page.getByRole("textbox", { name: /^Name of / })
      await page.getByRole("button", { name: "Rename Aurora" }).click()
      await name.fill(" Mine ")
      await page.route("**/api/sources/aurora", landThenDrop, { times: 1 })
      await name.press("Enter")
      await until(() => answered(puts("api/sources/aurora")) && puts("api/sources/aurora").length === 2, { message: "the replay" })
      await page.getByRole("button", { name: "Rename Mine" }).click()
      await name.fill("Mine again")
      await name.press("Enter")
      await until(() => daemon.source("aurora").name === "Mine again", { message: "the rename, which saves at once" })
      assert.equal(await toasts(page).count(), 0, "no conflict for the page's own write")
    } finally {
      await close()
    }
  })

  test("1. an edit whose answer was lost, edited again, saves over its own write without a conflict", async () => {
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      const save = settingsSave(page)
      await editRefresh(page, "12h")
      await page.route("**/api/sources/aurora", landThenDrop, { times: 1 })
      await save.click()
      await settingsOf(page).waitFor({ state: "hidden" })
      await editRefresh(page, "3h")
      await save.click()
      await settingsOf(page).waitFor({ state: "hidden" })
      assert.equal(daemon.source("aurora").origin.refreshEverySeconds, 10800)
      assert.deepEqual(puts("api/sources/aurora").map((r) => [r.status, r.replayed]), [[200, false], [200, true], [200, false]])
      assert.equal(await toasts(page).count(), 0, "the page's own write is no conflict")
    } finally {
      await close()
    }
  })

  test("1. an edit whose answer was lost, when someone saved after it: the replay settles on their view, and the next edit keeps their change", async () => {
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      const save = settingsSave(page)
      await editRefresh(page, "12h")
      await page.route("**/api/sources/aurora", async (route) => { await route.fetch(); await daemon.renameSource("aurora", "Theirs"); await route.abort("connectionreset") }, { times: 1 })
      await save.click()
      await settingsOf(page).waitFor({ state: "hidden" })
      await theirsListed(page)
      await editRefresh(page, "3h")
      await save.click()
      await settingsOf(page).waitFor({ state: "hidden" })
      assert.equal(daemon.source("aurora").name, "Theirs", "the other writer's change stands")
      assert.equal(daemon.source("aurora").origin.refreshEverySeconds, 10800)
      assert.equal(await toasts(page).filter({ hasText: "changed" }).count(), 0, "no conflict: the next save went over what the page read")
    } finally {
      await close()
    }
  })

  test("4. three saves of a setting, there and back and there again, are three writes", async () => {
    const { page, close } = await open(daemon.url + "settings")
    try {
      const save = page.getByRole("button", { name: "Save", exact: true }).last()
      await until(() => picked(page, "30d"), { message: "settings form" })
      for (const [i, word] of ["90d", "7d", "90d"].entries()) {
        await retentionOf(page, word).click()
        await save.click()
        await until(() => puts("api/settings").length === i + 1 && answered(puts("api/settings")) && daemon.settings().revision === 6 + i, { message: `save ${i + 1}` })
        await until(async () => (await picked(page, word)) && !(await save.isEnabled()), { message: `save ${i + 1} settled` })
      }
      const writes = puts("api/settings")
      assert.equal(new Set(writes.map((w) => w.idempotencyKey)).size, 3, "each click is its own action")
      assert.deepEqual(writes.map((w) => [w.status, w.replayed]), [[200, false], [200, false], [200, false]])
      assert.equal(daemon.settings().analyticsRetentionDays, 90)
    } finally {
      await close()
    }
  })

  test("4. adding a source never targets an existing id, and sends the trimmed URL", async () => {
    const { page, close } = await open(daemon.url + "servers")
    try {
      const add = await addDialog(page, { url: "  https://other.example/sub?t=1 " })
      await add.click()
      const [put] = await until(() => answered(daemon.log().filter((r) => r.method === "PUT")), { message: "the create's answer" })
      assert.equal(put.ifNoneMatch, "*")
      assert.notEqual(put.path, LIVE_BASE + "api/sources/aurora")
      assert.equal(JSON.parse(put.body).origin.url, "https://other.example/sub?t=1")
      assert.equal(put.status, 200)
    } finally {
      await close()
    }
  })

  test("6. a Test result stays on its server when the list reorders", async () => {
    const { page, close } = await open(daemon.url + "servers")
    try {
      const rowOf = (name) => page.locator("tbody tr").filter({ has: page.getByRole("button", { name, exact: true }) })
      await rowOf("Amsterdam").getByRole("button", { name: "Test" }).click()
      await page.getByText("111 ms", { exact: true }).waitFor()
      await rowOf("Paris").getByRole("button", { name: "Test" }).click()
      await page.getByText("222 ms", { exact: true }).waitFor()
      daemon.prependServer()
      await rowOf("Berlin").waitFor()
      const states = daemon.log().filter((r) => r.path === LIVE_BASE + "api/state").length
      daemon.renameSlot("eu", "EU renamed")
      await until(() => daemon.log().filter((r) => r.path === LIVE_BASE + "api/state").length > states, { message: "the view read again" })
      await page.locator("tbody tr", { hasText: "EU renamed" }).first().waitFor()
      assert.match(await rowOf("Amsterdam").innerText(), /111 ms/, "Berlin, best after Frankfurt, now sits where Amsterdam was")
      assert.match(await rowOf("Paris").innerText(), /222 ms/, "Paris, moved down to a row of its own, keeps its answer")
    } finally {
      await close()
    }
  })

  test("7. an answer the page can't read says the page is out of date, with Reload, and the details in the console", async () => {
    daemon.set({ badShape: true })
    const { page, consoleErrors, close } = await open(daemon.url)
    try {
      const alert = page.getByRole("alert").first()
      await alert.waitFor()
      const text = await alert.innerText()
      assert.equal(text.replace(/\s+/g, " ").trim(), "Page out of date Reload", "the reader is told what happened, and its one fix")
      assert.doesNotMatch(text, /invalid_value|"code"|"path"|expected|\[/, "no schema details for the reader")
      assert.ok(consoleErrors.some((e) => e.includes("wan")), "the details reach the console")
    } finally {
      await close()
    }
  })

  test("14. a refresh failure and a skipped entry read as the daemon's code in words, never raw", async () => {
    const aurora = daemon.source("aurora")
    daemon.set({ sources: new Map([["aurora", {
      ...aurora, origin: { ...aurora.origin, fetch: { ...aurora.origin.fetch, failures: 2, failureCode: "too_large" } }, diagnostics: [{ entry: 3, code: "unresolved_dialer", field: "dialerProxy" }],
    }]]) })
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      await page.getByRole("button", { name: "1 entry skipped. Entry 3: Unusable dialer" }).waitFor()
      await page.locator("[data-slot=source-heading]").getByRole("button", { name: /^Refresh/ }).click()
      await page.getByRole("dialog").getByText("Too large", { exact: true }).waitFor()
      const text = await page.evaluate(() => document.body.textContent)
      assert.doesNotMatch(text, /too_large|unresolved_dialer/)
    } finally {
      await close()
    }
  })

  test("a provider's 404 offers the User-Agents beside Retry, and Retry's one write stores the one picked and refreshes", async () => {
    const aurora = daemon.source("aurora")
    daemon.set({ sources: new Map([["aurora", { ...aurora, origin: { ...aurora.origin, fetch: { ...aurora.origin.fetch, failures: 1, failureCode: "http_404" } }, diagnostics: [] }]]) })
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      await page.locator("[data-slot=source-heading]").getByRole("button", { name: "Refresh failing" }).click()
      const why = page.getByRole("dialog").filter({ hasText: "HTTP 404" })
      await why.getByRole("combobox", { name: "User-Agent" }).click()
      await page.getByRole("option", { name: "v2rayNG/1.10.16" }).click()
      await why.getByRole("button", { name: "Retry", exact: true }).click()
      const [refresh] = await until(() => answered(refreshPosts()), { message: "Retry's refresh answered" })
      assert.deepEqual([refreshPosts().length, refresh.status, JSON.parse(refresh.body)], [1, 202, { userAgent: "v2rayNG/1.10.16" }], "one write, with the agent picked")
      assert.equal(daemon.source("aurora").origin.userAgent, "v2rayNG/1.10.16", "the agent stored")
      /* The fetch that follows may still fail, and Retry is back beside its code, or work, and the 404 is gone with its dialog. */
      const retryBack = why.getByRole("button", { name: "Retry", exact: true, disabled: false })
      await until(async () => (await why.count()) === 0 || (await retryBack.count()) === 1, { message: "Retry settled" })
      assert.equal(await toasts(page).count(), 0, "an answered Retry says nothing")
      assert.equal(puts("api/sources/aurora").length, 0, "no save of the source")
    } finally {
      await close()
    }
  })

  test("8. the favicon resolves under the base path on a deep link", async () => {
    const { page, close } = await open(daemon.url + "sources/aurora")
    try {
      const href = await page.evaluate(() => document.querySelector("link[rel=icon]")?.href)
      assert.equal(href, daemon.url + "favicon.svg")
      const res = await fetch(href)
      assert.equal(res.headers.get("content-type"), "image/svg+xml")
    } finally {
      await close()
    }
  })
})

describe("live, from a plain-HTTP LAN address, as a gateway serves the page", () => {
  beforeEach(() => daemon.reset())
  const openOnLan = async (path) => {
    const opened = await open(daemon.url.replace("//127.0.0.1:", `//${LAN_HOST}:`) + path)
    assert.equal(await opened.page.evaluate(() => window.isSecureContext), false, "the page runs outside a secure context")
    return opened
  }

  test("Cancel closes the add dialog, whatever it holds, and nothing goes out", async () => {
    const { page, consoleErrors, close } = await openOnLan("servers")
    try {
      for (const text of ["https://a.example/sub", "trojan://pw@b.example:443#B"]) {
        await addMenu(page, "Source")
        await page.getByRole("dialog").getByLabel("Source", { exact: true }).fill(text)
        await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click()
        await page.getByRole("dialog").waitFor({ state: "detached" })
      }
      assert.equal(daemon.log().filter((r) => r.method === "PUT").length, 0)
      assert.deepEqual(consoleErrors, [])
    } finally {
      await close()
    }
  })

  test("Add, Group, a tick and Save make a server group and open it", async () => {
    const { page, close } = await openOnLan("servers")
    try {
      await draftGroup(page)
      await saveGroup(page).click()
      await page.waitForURL(/\/groups\/(?!new$)[^/]+$/)
      const put = putsUnder("api/groups/").at(-1)
      assert.equal(put?.status, 200, "the create reached the daemon")
      assert.equal(new URL(page.url()).pathname, put.path.replace("api/", ""))
    } finally {
      await close()
    }
  })

  test("a manual config adds", async () => {
    const { page, close } = await openOnLan("servers")
    try {
      await (await manualDialog(page)).click()
      await page.getByRole("dialog").waitFor({ state: "detached" })
      assert.equal(putsUnder("api/sources/").at(-1)?.status, 200)
    } finally {
      await close()
    }
  })
})

/** A failover with its probe, so the events page has a switch row to open. */
const FAILOVER = (() => {
  const at = new Date(Date.now() - 2 * 3600e3).toISOString()
  return {
    id: "s1", owner: { kind: "slot", slot: "eu" }, slots: ["eu"], intent: 1, from: { kind: "up", server: "l-ams" }, to: { kind: "up", server: "l-fra" }, reason: "failureMove", requestedAt: at, appliedAt: at,
    evidence: [{ server: "l-ams", kind: "activeProbe", verdict: "failed", target: "gstatic-204", round: 1, at, pingMs: null, newConnectionMs: null }],
  }
})()

describe("the page on the fake daemon's seeds", () => {
  beforeEach(() => daemon.reset())

  test("4. a source whose name punctuates an existing one is added beside it", async () => {
    const { page, close } = await open(daemon.url + "servers")
    try {
      const add = await manualDialog(page, { name: "AURORA!", text: "trojan://pw@other.example:443#Other" })
      await add.click()
      await page.getByRole("dialog").waitFor({ state: "hidden" })
      const nav = page.getByRole("navigation", { name: "Main" })
      /* By the name alone: the mark before it is as many words as its level takes. */
      const named = (name) => nav.getByRole("link").filter({ has: page.getByText(name, { exact: true }) })
      await named("AURORA!").waitFor()
      await named("Aurora").waitFor()
    } finally {
      await close()
    }
  })

  test("5. the API key: a refused key stays in the ringed field with Save off until it changes, UniFi's silence is an amber line that Save retries, and a key taken empties the field to the dots", async () => {
    daemon.set({ unifiKeyAnswer: "keyRefused" })
    const { page, close } = await open(daemon.url + "settings")
    try {
      const part = page.locator("#unifi")
      const field = part.getByLabel("API key")
      const save = part.getByRole("button", { name: "Save", exact: true })
      await field.waitFor()
      assert.equal(await field.getAttribute("placeholder"), "••••••••••••••••", "a stored key is the dots")
      assert.equal(await save.isDisabled(), true, "Save is off while the field is empty")
      await field.fill("k3y-one")
      await save.click()
      await until(async () => (await field.getAttribute("aria-invalid")) === "true", { message: "the refused key's ring" })
      assert.equal(await part.locator("[data-slot=field-error]").innerText(), "Key refused")
      assert.equal(await field.inputValue(), "k3y-one", "the text kept")
      assert.equal(await save.isDisabled(), true, "Save off until the text changes")
      await field.fill("k3y-two")
      assert.equal(await field.getAttribute("aria-invalid"), null)
      assert.equal(await save.isEnabled(), true)
      daemon.set({ unifiKeyAnswer: "notAnswering" })
      await save.click()
      await until(async () => (await part.locator("[data-slot=field-error]").innerText().catch(() => "")) === "Not answering", { message: "UniFi's silence under the field" })
      assert.equal(await field.getAttribute("aria-invalid"), null, "silence is no bad input")
      assert.equal(await save.isEnabled(), true, "Save tries again")
      daemon.set({ unifiKeyAnswer: null })
      await save.click()
      await until(async () => (await field.inputValue()) === "" && (await part.locator("[data-slot=field-error]").count()) === 0, { message: "a key taken empties the field" })
      const keyed = daemon.log().filter((r) => r.path.endsWith("api/unifi/key"))
      assert.deepEqual(keyed.map((r) => [r.method, JSON.parse(r.body).key, r.status]), [["PUT", "k3y-one", 422], ["PUT", "k3y-two", 422], ["PUT", "k3y-two", 200]])
      assert.ok(daemon.log().filter((r) => !r.path.endsWith("api/unifi/key")).every((r) => !String(r.body ?? "").includes("k3y")), "no other request carries a key")
    } finally {
      await close()
    }
  })

  test("5. a key's save refused only in a toast, session expired, keeps the pasted key in the field", async () => {
    const { page, close } = await open(daemon.url + "settings")
    try {
      const part = page.locator("#unifi")
      const field = part.getByLabel("API key")
      await field.waitFor()
      await page.route("**/api/unifi/key", (route) => route.fulfill({ status: 401, contentType: "text/html", body: "" }))
      await field.fill("k3y-unsaved")
      await part.getByRole("button", { name: "Save", exact: true }).click()
      await page.locator("[data-sonner-toast]").filter({ hasText: /session expired/i }).waitFor()
      assert.equal(await field.inputValue(), "k3y-unsaved", "a key the daemon never stored stays in the field")
    } finally {
      await close()
    }
  })

  test("9. the source field checks a URL as a URL, with the reason shown", async () => {
    const { page, close } = await open(daemon.url + "servers")
    try {
      await addMenu(page, "Source")
      const url = page.getByRole("dialog").getByLabel("Source", { exact: true })
      const add = page.getByRole("dialog").getByRole("button", { name: "Add" })
      const error = page.getByRole("dialog").locator("[data-slot=field-error]")
      for (const [text, reason] of [["HTTPS://nova.example/sub?token=abc", null], [" https://nova.example/sub", null], ["http://nova.example/sub", "Must start with https://"]]) {
        await url.fill(text)
        assert.equal(await add.isEnabled(), reason === null, `Add for ${JSON.stringify(text)}`)
        assert.equal(await url.getAttribute("aria-invalid"), reason === null ? null : "true", `aria-invalid for ${JSON.stringify(text)}`)
        assert.equal(await error.count() > 0 ? (await error.innerText()).trim() : null, reason, `reason for ${JSON.stringify(text)}`)
      }
    } finally {
      await close()
    }
  })

  test("10. a screen reader hears each Stability cell as its level and its card, and the keyboard reaches each cell's card, whose lines have no full stop", async () => {
    const { page, close } = await open(daemon.url + "servers")
    const SENTENCE = /^Stability [0-4] of 4\. (No failure|\d[\d,]* failures?|Not checked) .+\.$/
    try {
      await page.locator("tbody [data-slot=bars]").first().waitFor()
      const heard = await page.locator("tbody [data-slot=bars]").evaluateAll((els) => els.map((el) => el.closest("button")?.getAttribute("aria-label") ?? ""))
      for (const sentence of heard) assert.match(sentence, SENTENCE, "a screen reader hears the cell's level and its card")
      const reached = new Set()
      for (let i = 0; i < 120; i++) {
        await page.keyboard.press("Tab")
        const cell = await page.evaluate(() => {
          const bars = document.activeElement?.closest("tbody td") ? document.activeElement.querySelector("[data-slot=bars]") : null
          return bars ? [...document.querySelectorAll("tbody [data-slot=bars]")].indexOf(bars) : -1
        })
        if (cell === -1) continue
        reached.add(cell)
        const lines = (await page.locator("[data-radix-popper-content-wrapper]").last().innerText()).split("\n").filter(Boolean)
        assert.ok(lines.length > 0 && lines.every((l) => !l.endsWith(".")), `focus opens ${heard[cell]}'s card, with no full stop: ${lines}`)
      }
      assert.deepEqual([...reached].toSorted((a, b) => a - b), heard.map((_, i) => i), "Tab reaches every cell")
    } finally {
      await close()
    }
  })

  test("11. warning text meets 4.5:1 in both themes", async () => {
    const aurora = daemon.source("aurora")
    daemon.set({ sources: new Map([["aurora", { ...aurora, origin: { ...aurora.origin, fetch: { ...aurora.origin.fetch, failures: 2, failureCode: "timeout" } }, diagnostics: [] }]]) })
    for (const theme of ["light", "dark"]) {
      const { page, close } = await open(daemon.url, { theme })
      try {
        const text = page.getByRole("navigation", { name: "Main" }).locator('a[href$="/sources/aurora"]').getByText("Refresh failing", { exact: true })
        await text.waitFor({ timeout: 8000 })
        const ratio = await contrastOf(text)
        assert.ok(ratio >= 4.5, `${theme}: ${ratio.toFixed(2)}:1`)
      } finally {
        await close()
      }
    }
  })

  test("12. no interactive content nests in a button or link, and a switch row still opens", async () => {
    daemon.set({ switches: [FAILOVER] })
    const { page, close } = await open(daemon.url + "events")
    try {
      await page.locator("li", { hasText: "Server failed" }).first().waitFor()
      const nested = await page.locator("button a, button button, a button, a a, button input, a input").count()
      assert.equal(nested, 0)
      await page.getByRole("button", { name: /Server failed/ }).first().click()
      await page.getByRole("columnheader", { name: "Signal" }).first().waitFor()
    } finally {
      await close()
    }
  })

  test("12. a switch row's time is under the pointer, so hovering it shows the full clock", async () => {
    daemon.set({ switches: [FAILOVER] })
    const { page, close } = await open(daemon.url + "events")
    try {
      const time = page.locator("li", { hasText: "Server failed" }).first().locator("span[title]").first()
      await time.waitFor()
      const box = await time.boundingBox()
      const hit = await page.evaluate(([x, y]) => document.elementFromPoint(x, y)?.closest("[title]")?.getAttribute("title") ?? null, [box.x + box.width / 2, box.y + box.height / 2])
      assert.equal(hit, await time.getAttribute("title"))
    } finally {
      await close()
    }
  })

  test("14. no page shows an internal identifier", async () => {
    const PROPER = new Set(["UniFi", "VMess", "WebSocket", "gRPC", "GeoIP", "WireGuard", "MacBook"])
    const found = {}
    daemon.set({ switches: [FAILOVER] })
    for (const path of ["", "servers", "sources/aurora", "groups/all-aurora", "events", "settings"]) {
      const { page, close } = await open(daemon.url + path)
      try {
        await page.waitForTimeout(1500)
        const text = await page.locator("body").innerText()
        const words = (text.match(/\b[A-Za-z]*[a-z][A-Z][A-Za-z]*\b|\b[a-z0-9]+_[a-z0-9_]+\b/g) ?? []).filter((w) => !PROPER.has(w))
        if (words.length) found[`/${path}`] = words
      } finally {
        await close()
      }
    }
    assert.deepEqual(found, {})
  })
})
