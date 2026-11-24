// A form's write end to end, against a fake daemon or the Go one: attempts whose answers are lost, keys the daemon forgot,
// writes held up on the way, and refusals. The machine behind them is tested in src/lib/write.test.ts.
import assert from "node:assert/strict"
import { rmSync } from "node:fs"
import { after, before, beforeEach, describe, test } from "node:test"
import { fileURLToPath } from "node:url"
import { chromium } from "playwright-core"
import { build } from "vite"
import { startDaemon } from "./daemon-under-test.mjs"

const ROOT = fileURLToPath(new URL("../../", import.meta.url))
const DIST = fileURLToPath(new URL("./.dist-save/", import.meta.url))
const PORT = Number(process.env.E2E_PORT ?? 5311) + 2
const BASE = "/proxy/"

let browser
let daemon

before(async () => {
  rmSync(DIST, { recursive: true, force: true })
  process.env.BASE_PATH = BASE
  await build({ root: ROOT, logLevel: "warn", build: { outDir: DIST, emptyOutDir: true } })
  delete process.env.BASE_PATH
  daemon = await startDaemon({ port: PORT, dist: DIST, base: BASE })
  browser = await chromium.launch()
})

after(async () => {
  await browser?.close()
  await daemon?.close()
  rmSync(DIST, { recursive: true, force: true })
})

async function open(path) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  const page = await context.newPage()
  page.setDefaultTimeout(10_000)
  await page.goto(daemon.url + path)
  return { page, close: () => context.close() }
}

async function until(check, message, timeout = 10_000) {
  const end = Date.now() + timeout
  for (;;) {
    const value = await check()
    if (value) return value
    if (Date.now() > end) assert.fail(`timed out waiting for ${message}`)
    await new Promise((r) => setTimeout(r, 100))
  }
}

const toasts = (page) => page.locator("[data-sonner-toast]")
const words = async (locator) => (await locator.innerText()).replace(/\s+/g, " ").trim()
const putsUnder = (prefix) => daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(BASE + prefix))
const answered = (writes) => writes.length > 0 && writes.every((w) => w.status !== 0)
const committed = (writes) => writes.filter((w) => w.status === 200 && !w.replayed)
/** Each attempt the page sends to `glob`, met in turn by `fates`, then let through. */
async function fated(page, glob, ...fates) {
  await page.route(glob, async (route) => {
    const fate = route.request().method() === "PUT" ? fates.shift() : undefined
    if (!fate) return route.continue()
    await fate(route)
  })
}
const landThenDrop = async (route) => { await route.fetch(); await route.abort("connectionreset") }
const dropBefore = (route) => route.abort("connectionreset")

const settingsDialog = (page) => page.getByRole("dialog", { name: "Settings" })
async function auroraSettings(page) {
  await page.getByRole("button", { name: /^Settings for / }).click()
  const refresh = settingsDialog(page).getByRole("radiogroup", { name: "Refresh" })
  await refresh.waitFor()
  return {
    pick: (label) => refresh.getByRole("radio", { name: label, exact: true }).click(),
    chosen: async () => (await refresh.getByRole("radio", { checked: true }).innerText()).trim(),
    save: settingsDialog(page).getByRole("button", { name: "Save", exact: true }),
  }
}
const LABELS = { 3600: "1h", 10800: "3h", 21600: "6h", 43200: "12h", 86400: "24h" }
const storedRefresh = () => LABELS[daemon.source("aurora").origin.refreshEverySeconds]

async function addDialog(page, text, name) {
  await page.getByRole("navigation", { name: "Main" }).getByRole("button", { name: "Add", exact: true }).click()
  await page.getByRole("menuitem", { name: "Source", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "Add source" })
  await dialog.getByLabel("Source", { exact: true }).fill(text)
  if (name) await dialog.getByLabel("Name").fill(name)
  return { dialog, add: dialog.getByRole("button", { name: "Add", exact: true }) }
}
const added = () => daemon.sources().filter((s) => s.id !== "aurora")

describe("a form's write, against a fake daemon", () => {
  beforeEach(() => daemon.reset())

  test("15. a save whose attempts are lost, landed or not, is sent again under its key and commits once; the next edit saves over it with no conflict", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      const editor = await auroraSettings(page)
      await fated(page, "**/api/sources/aurora", landThenDrop, dropBefore)
      await editor.pick("12h")
      await editor.save.click()
      await settingsDialog(page).waitFor({ state: "hidden" })
      const writes = putsUnder("api/sources/aurora")
      assert.equal(new Set(writes.map((w) => w.idempotencyKey)).size, 1, "one key throughout")
      assert.equal(committed(writes).length, 1, "committed once")
      assert.equal(storedRefresh(), "12h")
      const next = await auroraSettings(page)
      await next.pick("3h")
      await next.save.click()
      await settingsDialog(page).waitFor({ state: "hidden" })
      assert.equal(storedRefresh(), "3h")
      assert.equal(await toasts(page).count(), 0, "no conflict over the page's own write")
    } finally {
      await close()
    }
  })

  test("15. an add whose key the daemon forgot, sent again after it landed, meets its own create: a 412 on a create is that create, and the dialog closes onto one source", async () => {
    const { page, close } = await open("servers")
    try {
      const { dialog, add } = await addDialog(page, "trojan://pw@b.example:443#Borealis one", "Borealis")
      await fated(page, /\/api\/sources\/[^/]+$/, async (route) => { await route.fetch(); daemon.set({ writes: new Map() }); await route.abort("connectionreset") })
      await add.click()
      await dialog.waitFor({ state: "hidden" })
      assert.deepEqual(putsUnder("api/sources/").map((w) => [w.status, w.ifNoneMatch]), [[200, "*"], [412, "*"]], "its own create, found by its precondition")
      assert.equal(added().length, 1, "one source")
      await page.waitForURL(new RegExp(`/sources/${added()[0].id}$`))
      assert.equal(await toasts(page).count(), 0, "no conflict, no blame")
    } finally {
      await close()
    }
  })

  test("15. an edit whose key the daemon forgot, sent again after it landed, meets its own write: nothing it touched differs from what is stored, so it is no conflict, and the dialog closes with no toast", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      const editor = await auroraSettings(page)
      await fated(page, "**/api/sources/aurora", async (route) => { await route.fetch(); daemon.set({ writes: new Map() }); await route.abort("connectionreset") })
      await editor.pick("12h")
      await editor.save.click()
      await settingsDialog(page).waitFor({ state: "hidden" })
      assert.deepEqual(putsUnder("api/sources/aurora").map((w) => [w.status, w.ifMatch]), [[200, '"1"'], [412, '"1"']], "its own write, met by its precondition")
      await page.waitForTimeout(300)
      assert.equal(await toasts(page).count(), 0, "no conflict to show")
      assert.equal(daemon.source("aurora").origin.refreshEverySeconds, 43200)
    } finally {
      await close()
    }
  })

  test("15. an add the daemon refuses keeps its typing with the reason under the field, and the correction is a new add at a new key", async () => {
    const { page, close } = await open("servers")
    try {
      const { dialog, add } = await addDialog(page, "not a list of servers", "Odd")
      await add.click()
      const error = dialog.locator("[data-slot=field-error]")
      await error.waitFor()
      assert.equal(await dialog.getByLabel("Source", { exact: true }).inputValue(), "not a list of servers")
      await dialog.getByLabel("Source", { exact: true }).fill("trojan://pw@odd.example:443#Odd")
      await add.click()
      await dialog.waitFor({ state: "hidden" })
      const writes = putsUnder("api/sources/")
      assert.equal(new Set(writes.map((w) => w.idempotencyKey)).size, 2, "a new key")
      assert.deepEqual(writes.map((w) => w.status), [422, 200])
      assert.equal(added().length, 1)
    } finally {
      await close()
    }
  })

  test("15. a save answered 503 is sent again under its key, which the 5xx left free, and commits once", async () => {
    const { page, close } = await open("settings")
    try {
      const retention = page.getByRole("radiogroup", { name: "Retention" })
      await until(async () => (await retention.getByRole("radio", { checked: true }).innerText()).trim() === "30d", "the settings form")
      await fated(page, "**/api/settings", (route) => route.fulfill({ status: 503, contentType: "text/html", body: "<h1>busy</h1>" }))
      await retention.getByRole("radio", { name: "90d", exact: true }).click()
      await page.locator("button[data-control=settings]").click()
      await until(() => daemon.settings().analyticsRetentionDays === 90 && answered(putsUnder("api/settings")), "the resend lands")
      const writes = putsUnder("api/settings")
      assert.equal(writes.length, 1, "the 503 never reached the daemon")
      assert.equal(committed(writes).length, 1)
      await until(async () => (await page.locator("button[data-control=settings]").isDisabled()), "settled")
      assert.equal(await toasts(page).count(), 0)
    } finally {
      await close()
    }
  })

  test("15. a save refused with 422, pressed again unchanged, is a new write under a new key", async () => {
    const { page, close } = await open("settings")
    try {
      const retention = page.getByRole("radiogroup", { name: "Retention" })
      const save = page.locator("button[data-control=settings]")
      await until(async () => (await retention.getByRole("radio", { checked: true }).innerText()).trim() === "30d", "the settings form")
      const keys = []
      page.on("request", (r) => { if (r.method() === "PUT") keys.push(r.headers()["idempotency-key"]) })
      await fated(page, "**/api/settings", (route) => route.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "retentionRange", message: "analyticsRetentionDays is outside 1..365." }) }))
      await retention.getByRole("radio", { name: "90d", exact: true }).click()
      await save.click()
      await toasts(page).filter({ hasText: "1d to 365d" }).waitFor()
      assert.equal(await retention.getByRole("radio", { name: "90d", exact: true }).getAttribute("aria-checked"), "true", "the pick stays")
      await save.click()
      await until(() => daemon.settings().analyticsRetentionDays === 90, "the second press lands")
      assert.equal(keys.length, 2)
      assert.notEqual(keys[0], keys[1], "the refusal stored nothing under the first key, so the next press is its own write")
    } finally {
      await close()
    }
  })

  test("15. a 412 naming no revision is the daemon's refusal, in its reason's words, and the draft stays", async () => {
    const { page, close } = await open("settings")
    try {
      const retention = page.getByRole("radiogroup", { name: "Retention" })
      await until(async () => (await retention.getByRole("radio", { checked: true }).innerText()).trim() === "30d", "the settings form")
      await fated(page, "**/api/settings", (route) => route.fulfill({ status: 412, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "staleRevision", reason: "stale", message: "Changed by another editor." }) }))
      await retention.getByRole("radio", { name: "90d", exact: true }).click()
      await page.locator("button[data-control=settings]").click()
      const said = toasts(page).filter({ hasText: "changed in another tab" })
      await said.waitFor()
      assert.equal(await words(said), "Settings changed in another tab")
      assert.equal(await retention.getByRole("radio", { name: "90d", exact: true }).getAttribute("aria-checked"), "true")
      assert.equal(await page.locator("button[data-control=settings]").isEnabled(), true)
    } finally {
      await close()
    }
  })

  test("the update check's box saves at once, apart from Retention's pick, and Retention's Save keeps it", async () => {
    const { page, close } = await open("settings")
    try {
      const retention = page.getByRole("radiogroup", { name: "Retention" })
      const box = page.getByRole("checkbox", { name: "Check for updates" })
      const save = page.locator("button[data-control=settings]")
      await until(async () => (await box.getAttribute("aria-checked")) === "true", "the box as stored")
      await retention.getByRole("radio", { name: "90d", exact: true }).click()
      await box.click()
      await until(() => daemon.settings().checkForUpdates === false, "the box's write lands")
      await until(async () => (await box.getAttribute("aria-checked")) === "false" && (await box.getAttribute("aria-disabled")) === null, "the box settles off")
      assert.equal(daemon.settings().analyticsRetentionDays, 30, "the box carried no Retention pick")
      assert.equal(await retention.getByRole("radio", { name: "90d", exact: true }).getAttribute("aria-checked"), "true", "the pick stays")
      assert.equal(await save.isEnabled(), true, "Save still holds the pick")
      await save.click()
      await until(() => daemon.settings().analyticsRetentionDays === 90, "Retention saves")
      assert.equal(daemon.settings().checkForUpdates, false, "Retention's Save keeps the box as stored")
      assert.equal(await toasts(page).count(), 0, "no conflict")
      await page.reload()
      await until(async () => (await box.getAttribute("aria-checked")) === "false", "the box reads off after a reload")
    } finally {
      await close()
    }
  })

  test("the update check's box shows what it sent while its write is out, and takes no second press", async () => {
    const { page, close } = await open("settings")
    try {
      const box = page.getByRole("checkbox", { name: "Check for updates" })
      await until(async () => (await box.getAttribute("aria-checked")) === "true", "the box as stored")
      let release
      const held = new Promise((r) => { release = r })
      const sent = []
      await page.route("**/api/settings/check-for-updates", async (route) => {
        sent.push([route.request().headers()["idempotency-key"], route.request().postData()])
        await held
        await route.continue().catch(() => {})
      })
      await box.click()
      await until(async () => (await box.getAttribute("aria-checked")) === "false" && (await box.getAttribute("aria-disabled")) === "true", "the box Pending, showing what it sent")
      await until(async () => (await box.getAttribute("aria-busy")) === "true", "a slow write marks the box busy", 3000)
      await box.click({ force: true })
      release()
      await until(() => daemon.settings().checkForUpdates === false, "the write lands")
      await until(async () => (await box.getAttribute("aria-disabled")) === null, "the box settles")
      assert.deepEqual(new Set(sent.map(String)), new Set([String(sent[0])]), "every attempt is the one press's write, under its key")
      assert.equal(JSON.parse(sent[0][1]).checkForUpdates, false)
      assert.equal(await box.getAttribute("aria-checked"), "false")
    } finally {
      await close()
    }
  })

  test("the update check's box refused goes back to what is stored, and its toast says so", async () => {
    const { page, close } = await open("settings")
    try {
      const box = page.getByRole("checkbox", { name: "Check for updates" })
      await until(async () => (await box.getAttribute("aria-checked")) === "true", "the box as stored")
      await fated(page, "**/api/settings/check-for-updates", (route) => route.fulfill({ status: 400, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "malformed", message: "checkForUpdates is true or false." }) }))
      await box.click()
      await toasts(page).filter({ hasText: "Check for updates" }).waitFor()
      await until(async () => (await box.getAttribute("aria-checked")) === "true" && (await box.getAttribute("aria-disabled")) === null, "the box back as stored")
      assert.equal(daemon.settings().checkForUpdates, true)
    } finally {
      await close()
    }
  })

  test("15. a write held on the way past its first resend commits once: the resend goes under the same key, and its answer ends the held attempt", async () => {
    const { page, close } = await open("settings")
    try {
      const retention = page.getByRole("radiogroup", { name: "Retention" })
      await until(async () => (await retention.getByRole("radio", { checked: true }).innerText()).trim() === "30d", "the settings form")
      const keys = []
      page.on("request", (r) => { if (r.method() === "PUT") keys.push(r.headers()["idempotency-key"]) })
      await fated(page, "**/api/settings", async (route) => { await new Promise((r) => setTimeout(r, 1500)); await route.continue().catch(() => {}) })
      await retention.getByRole("radio", { name: "90d", exact: true }).click()
      await page.locator("button[data-control=settings]").click()
      await until(() => daemon.settings().analyticsRetentionDays === 90 && answered(putsUnder("api/settings")), "the resend lands")
      await page.waitForTimeout(1000)
      const writes = putsUnder("api/settings")
      assert.ok(keys.length >= 2, "the resend went")
      assert.equal(new Set([...keys, ...writes.map((w) => w.idempotencyKey)]).size, 1, "one key")
      assert.equal(committed(writes).length, 1, "committed once")
      await until(async () => (await page.locator("button[data-control=settings]").isDisabled()), "settled")
      assert.equal(await toasts(page).count(), 0, "no conflict")
    } finally {
      await close()
    }
  })
})
