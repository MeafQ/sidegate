// What ends a write, and what a form or a press does once the reader has gone, end to end against a fake daemon or the Go one:
// a conflict's toast at every width, refusals from in front of the daemon, UniFi's login, an item deleted elsewhere, and answers
// that land after the reader left. The machine behind them is tested in src/lib/write.test.ts.
import assert from "node:assert/strict"
import { rmSync } from "node:fs"
import { after, before, beforeEach, describe, test } from "node:test"
import { fileURLToPath } from "node:url"
import { chromium } from "playwright-core"
import { build } from "vite"
import { startDaemon } from "./daemon-under-test.mjs"

const ROOT = fileURLToPath(new URL("../../", import.meta.url))
const DIST = fileURLToPath(new URL("./.dist-retry/", import.meta.url))
const PORT = Number(process.env.E2E_PORT ?? 5311) + 3
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

async function open(path, viewport = { width: 1440, height: 900 }) {
  const context = await browser.newContext({ viewport })
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
    await new Promise((r) => setTimeout(r, 50))
  }
}

const toasts = (page) => page.locator("[data-sonner-toast]")
const toastSaying = (page, text) => toasts(page).filter({ hasText: text })
const words = async (locator) => (await locator.innerText()).replace(/\s+/g, " ").trim()
const putsTo = (prefix) => daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(BASE + prefix))
/** Every PUT the page sent to `pattern`, reached the daemon or not. */
function sentPuts(page, pattern) {
  const sent = []
  page.on("request", (r) => { if (r.method() === "PUT" && pattern.test(r.url())) sent.push(r.headers()["idempotency-key"]) })
  return sent
}
/** Holds the page's reads of the state and the settings back until it has sent a PUT, so it saves over what it last read while
 * another writer has saved since. */
async function unheardUntilSaved(page) {
  let sent = false
  page.on("request", (r) => { if (r.method() === "PUT") sent = true })
  await page.route(/\/api\/(state|settings)$/, (route) => (route.request().method() === "GET" && !sent ? route.abort() : route.continue()))
}

const GROUP_WRITE = /\/api\/groups\/(?!preview$)[^/]+$/
/** A stored group's address, never the draft's. */
const STORED = /\/groups\/(?!new$)[^/]+$/

async function addMenu(page, item) {
  await page.getByRole("navigation", { name: "Main" }).getByRole("button", { name: "Add", exact: true }).click()
  await page.getByRole("menuitem", { name: item, exact: true }).click()
}

const EDITORS = {
  settings: {
    path: "settings",
    async open(page) {
      const retention = page.getByRole("radiogroup", { name: "Retention" })
      await until(async () => (await retention.getByRole("radio", { checked: true }).innerText()).trim() === "30d", "the settings form")
      await retention.getByRole("radio", { name: "7d", exact: true }).click()
      return page.locator("button[data-control=settings]")
    },
    other: () => daemon.changeSettings({ analyticsRetentionDays: 90 }),
  },
  source: {
    path: "sources/aurora",
    async open(page) {
      /* Below 880px of page width, Settings is in the heading's menu. */
      const opener = page.getByRole("button", { name: /^(Settings|More) for Aurora$/ }).first()
      const inMenu = /^More/.test(await opener.getAttribute("aria-label"))
      await opener.click()
      if (inMenu) await page.getByRole("menuitem", { name: "Settings" }).click()
      const dialog = page.getByRole("dialog", { name: "Settings" })
      await dialog.getByRole("radiogroup", { name: "Refresh" }).getByRole("radio", { name: "12h", exact: true }).click()
      return dialog.getByRole("button", { name: "Save", exact: true })
    },
    other: () => daemon.renameSource("aurora", "Theirs"),
  },
}

/** Whether all of an element is inside the viewport and is what the page shows there, not covered by something else. */
const onScreen = (locator) => locator.evaluate((el) => {
  const box = el.getBoundingClientRect()
  if (box.top < 0 || box.left < 0 || box.bottom > innerHeight || box.right > innerWidth) return false
  const top = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2)
  return top !== null && (el === top || el.contains(top) || top.contains(el))
})

describe("a conflict is on screen", () => {
  beforeEach(() => daemon.reset())

  for (const [name, editor] of Object.entries(EDITORS)) {
    for (const viewport of [{ width: 1440, height: 900 }, { width: 1280, height: 720 }, { width: 390, height: 844 }, { width: 1440, height: 360 }]) {
      test(`16. a ${name} conflict at ${viewport.width}x${viewport.height} is a toast on screen, and Save, its one fix, is on screen and enabled`, async () => {
        const { page, close } = await open(editor.path, viewport)
        try {
          const save = await editor.open(page)
          await unheardUntilSaved(page)
          await editor.other()
          await save.click()
          const conflict = toastSaying(page, "changed in another tab")
          await conflict.waitFor()
          await until(() => onScreen(conflict), "the toast on screen", 2000)
          assert.equal(await conflict.getByRole("button", { name: /Keep mine|Retry/ }).count(), 0, "no button but its ×")
          await until(() => save.isEnabled(), "Save enabled")
          await save.evaluate((el) => el.scrollIntoView({ block: "center" }))
          assert.ok(await onScreen(save), "Save on screen, the toast clear of it")
        } finally {
          await close()
        }
      })
    }
  }
})

describe("what ends a write", () => {
  beforeEach(() => daemon.reset())

  test("16. a 413 from in front ends a save at once with its code and no Retry, the draft kept and nothing at the daemon", async () => {
    const { page, close } = await open("settings")
    try {
      const save = await EDITORS.settings.open(page)
      const sent = sentPuts(page, /api\/settings$/)
      await page.route("**/api/settings", (r) => (r.request().method() === "PUT" ? r.fulfill({ status: 413, contentType: "text/html", body: "<h1>too large</h1>" }) : r.continue()))
      await save.click()
      const said = toastSaying(page, "HTTP 413")
      await said.waitFor()
      assert.equal(await words(said), "Settings HTTP 413")
      assert.equal(sent.length, 1, "sent once")
      assert.equal(await page.getByRole("radiogroup", { name: "Retention" }).getByRole("radio", { name: "7d", exact: true }).getAttribute("aria-checked"), "true", "the pick stays")
      assert.equal(putsTo("api/settings").length, 0)
    } finally {
      await close()
    }
  })

  test("16. a 429 from in front ends a save with Retry, which sends the same write under its key", async () => {
    const { page, close } = await open("settings")
    try {
      const save = await EDITORS.settings.open(page)
      const sent = sentPuts(page, /api\/settings$/)
      let busy = true
      await page.route("**/api/settings", (r) => (r.request().method() === "PUT" && busy ? r.fulfill({ status: 429, contentType: "text/html", body: "<h1>slow down</h1>" }) : r.continue()))
      await save.click()
      const said = toastSaying(page, "HTTP 429")
      await said.waitFor()
      busy = false
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.settings().analyticsRetentionDays === 7, "Retry lands")
      assert.equal(new Set(sent).size, 1, "under one key")
    } finally {
      await close()
    }
  })

  test("16. UniFi's login signing an add out is healed once, then the toast says so with Sign in, the dialog kept as typed and nothing stored", async () => {
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      await dialog.getByLabel("Source", { exact: true }).fill("https://b.example/sub")
      const sent = sentPuts(page, /api\/sources\/[^/]+$/)
      await page.route(/\/api\/sources\/[^/]+$/, (r) => (r.request().method() === "PUT" ? r.fulfill({ status: 401, contentType: "text/html", body: "" }) : r.continue()))
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      const said = toastSaying(page, "session expired")
      await said.waitFor()
      assert.equal(await said.getByRole("button", { name: "Sign in" }).count(), 1)
      assert.equal(sent.length, 2, "healed once, with the session read again")
      assert.equal(new Set(sent).size, 1, "under one key")
      assert.equal(await dialog.getByLabel("Source", { exact: true }).inputValue(), "https://b.example/sub", "the dialog as typed")
      assert.equal(daemon.sources().length, 1, "nothing stored")
    } finally {
      await close()
    }
  })

  test("16. UniFi's login refusing after an earlier attempt may have landed ends the write unsure, and the fresh read finds the add and closes onto it", async () => {
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      await dialog.getByLabel("Source", { exact: true }).fill("https://b.example/sub")
      let attempts = 0
      await page.route(/\/api\/sources\/[^/]+$/, async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        if (++attempts === 1) { await r.fetch(); return r.abort("connectionreset") }
        return r.fulfill({ status: 403, contentType: "text/html", body: "" })
      })
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      await dialog.waitFor({ state: "hidden" })
      assert.equal(attempts, 2, "the refusal came to the resend")
      assert.equal(await toastSaying(page, "UniFi refused").count(), 0, "its toast goes with the dialog, since the add landed")
      const made = daemon.sources().filter((s) => s.id !== "aurora")
      assert.equal(made.length, 1, "the add that landed, once")
      await page.waitForURL(new RegExp(`/sources/${made[0].id}$`))
    } finally {
      await close()
    }
  })

  test("16. a source deleted elsewhere while its settings are open: Save says it was deleted, and nothing is made", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      const save = await EDITORS.source.open(page)
      await unheardUntilSaved(page)
      await daemon.deleteSlot("eu")
      await daemon.deleteSource("aurora")
      await save.click()
      const said = toastSaying(page, "was deleted")
      await said.waitFor()
      assert.equal(await words(said), "Aurora was deleted")
      assert.equal(daemon.source("aurora"), undefined, "not made again")
    } finally {
      await close()
    }
  })

  test("16. Cancel after an add got no answer takes its toast and its Retry, and nothing is sent after", async () => {
    const { page, close } = await open("servers")
    try {
      await page.clock.install()
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      await dialog.getByLabel("Source", { exact: true }).fill("https://b.example/sub")
      const sent = sentPuts(page, /api\/sources\/[^/]+$/)
      await page.route(/\/api\/sources\/[^/]+$/, (r) => (r.request().method() === "PUT" ? r.abort() : r.continue()))
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      await page.clock.fastForward(31_000)
      const said = toastSaying(page, "timed out")
      await said.waitFor()
      await dialog.getByRole("button", { name: "Cancel" }).click()
      await dialog.waitFor({ state: "hidden" })
      await said.waitFor({ state: "detached" })
      const count = sent.length
      await page.clock.fastForward(31_000)
      assert.equal(sent.length, count, "nothing sent after")
    } finally {
      await close()
    }
  })

  test("16. an add with no answer, edited, then sent again by its toast's Retry closes onto the one source Retry made", async () => {
    const { page, close } = await open("servers")
    try {
      await page.clock.install()
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      await field.fill("https://b.example/sub")
      let cut = true
      await page.route(/\/api\/sources\/[^/]+$/, (r) => (r.request().method() === "PUT" && cut ? r.abort() : r.continue()))
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      await page.clock.fastForward(31_000)
      const said = toastSaying(page, "timed out")
      await said.waitFor()
      await field.fill("https://c.example/sub")
      cut = false
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.sources().length === 2, "Retry lands")
      await dialog.waitFor({ state: "hidden" })
      const made = daemon.sources().filter((s) => s.id !== "aurora")
      assert.equal(made.length, 1, "one source, the one Retry sent")
      await page.waitForURL(new RegExp(`/sources/${made[0].id}$`))
    } finally {
      await close()
    }
  })

  test("16. an add's refusal that a toast's Retry gets after the reader typed a URL over the paste is its toast, and Add sends the URL", async () => {
    const { page, close } = await open("servers")
    try {
      await page.clock.install()
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      const add = dialog.getByRole("button", { name: "Add", exact: true })
      await field.fill("not servers")
      await dialog.getByLabel("Name", { exact: true }).fill("Junk")
      let cut = true
      await page.route(/\/api\/sources\/[^/]+$/, (r) => (r.request().method() === "PUT" && cut ? r.abort() : r.continue()))
      await add.click()
      await page.clock.fastForward(31_000)
      const said = toastSaying(page, "timed out")
      await said.waitFor()
      await field.fill("https://c.example/sub")
      assert.equal(await add.isEnabled(), true, "the URL can be added")
      cut = false
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.log().some((r) => r.method === "PUT" && r.status === 422), "the daemon refuses the paste Retry sent")
      await toastSaying(page, "Unknown format").waitFor()
      assert.equal(await field.inputValue(), "https://c.example/sub", "the URL kept")
      assert.equal(await dialog.locator("[data-slot=field-error]").count(), 0, "no refusal under the URL")
      assert.equal(await add.isEnabled(), true, "Add takes the URL")
      await add.click()
      await dialog.waitFor({ state: "hidden" })
      const made = daemon.sources().filter((s) => s.id !== "aurora")
      assert.equal(made.length, 1, "one source, the URL's")
      await page.waitForURL(new RegExp(`/sources/${made[0].id}$`))
    } finally {
      await close()
    }
  })

  for (const [way, refresh] of [["", false], [", a Refresh picked on the way", true]]) test(`16. an add's refusal that a toast's Retry gets after the reader went to a URL and back${way}, stands under the paste`, async () => {
    const { page, close } = await open("servers")
    try {
      await page.clock.install()
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      const add = dialog.getByRole("button", { name: "Add", exact: true })
      await field.fill("not servers")
      await dialog.getByLabel("Name", { exact: true }).fill("Junk")
      let cut = true
      await page.route(/\/api\/sources\/[^/]+$/, (r) => (r.request().method() === "PUT" && cut ? r.abort() : r.continue()))
      await add.click()
      await page.clock.fastForward(31_000)
      const said = toastSaying(page, "timed out")
      await said.waitFor()
      await field.fill("https://c.example/sub")
      if (refresh) {
        await dialog.getByRole("button", { name: "Advanced" }).click()
        await dialog.getByRole("radiogroup", { name: "Refresh" }).getByRole("radio", { name: "12h", exact: true }).click()
      }
      await field.fill("not servers")
      assert.equal(await dialog.getByLabel("Name", { exact: true }).inputValue(), "Junk", "the name kept")
      cut = false
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.log().some((r) => r.method === "PUT" && r.status === 422), "the daemon refuses the paste Retry sent")
      await dialog.locator("[data-slot=field-error]").filter({ hasText: "Unknown format" }).waitFor()
      assert.equal(await add.isEnabled(), false, "Add won't send the refused paste again")
      assert.equal(await toastSaying(page, "Unknown format").count(), 0, "no toast says the refusal")
    } finally {
      await close()
    }
  })

  test("16. an add that landed with its answers lost, edited before a read shows it, closes onto it once one does", async () => {
    const { page, close } = await open("servers")
    try {
      await page.clock.install()
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      await field.fill("https://b.example/sub")
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/state", async (r) => { await held; await r.continue().catch(() => {}) })
      await page.route(/\/api\/sources\/[^/]+$/, async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        await r.fetch()
        return r.abort("connectionreset")
      })
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      await page.clock.fastForward(31_000)
      await toastSaying(page, "timed out").waitFor()
      assert.equal(daemon.sources().length, 2, "the add landed")
      await field.fill("https://c.example/sub")
      release()
      await page.clock.fastForward(11_000)
      await dialog.waitFor({ state: "hidden" })
      const made = daemon.sources().filter((s) => s.id !== "aurora")
      assert.equal(made.length, 1, "one source")
      await page.waitForURL(new RegExp(`/sources/${made[0].id}$`))
    } finally {
      await close()
    }
  })

  test("16. a later add the daemon refuses keeps its text and its refusal when the earlier add, which landed unheard, turns up in the view", async () => {
    const { page, close } = await open("servers")
    try {
      await page.clock.install()
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      await field.fill("https://b.example/sub")
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/state", async (r) => { await held; await r.continue().catch(() => {}) })
      let first = null
      await page.route(/\/api\/sources\/[^/]+$/, async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        first ??= r.request().url()
        if (r.request().url() !== first) return r.continue()
        await r.fetch()
        return r.abort("connectionreset")
      })
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      await page.clock.fastForward(31_000)
      await toastSaying(page, "timed out").waitFor()
      assert.equal(daemon.sources().length, 2, "the first add landed")
      await field.fill("not servers")
      await dialog.getByLabel("Name", { exact: true }).fill("Junk")
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      await until(() => daemon.log().some((r) => r.method === "PUT" && r.status === 422), "the daemon refuses the second add")
      release()
      const landed = daemon.sources().find((s) => s.id !== "aurora")
      await page.locator(`nav a[href$="/sources/${landed.id}"]`).first().waitFor({ state: "attached" })
      await page.clock.fastForward(2_000)
      await page.waitForTimeout(300)
      assert.equal(await dialog.isVisible(), true, "the dialog stays with the add pressed last")
      assert.equal(await field.inputValue(), "not servers", "its text kept")
      assert.equal(await dialog.locator("[data-slot=field-error]").innerText(), "Unknown format", "its refusal kept")
    } finally {
      await close()
    }
  })

  test("16. Save as group pressed again over other filters while the first make landed unheard opens a group of the filters pressed last", async () => {
    const { page, close } = await open("servers")
    try {
      await page.clock.install()
      await page.locator("tbody tr").first().waitFor()
      const country = async (n) => {
        await page.locator("thead").getByRole("button", { name: "Filter servers by Country", exact: true }).click()
        await page.getByRole("dialog").getByRole("option").nth(n).click()
        await page.keyboard.press("Escape")
      }
      await country(0)
      let blind = true
      await page.route("**/api/state", (r) => (blind ? r.abort() : r.continue()))
      let first = null
      await page.route(GROUP_WRITE, async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        first ??= { body: r.request().postData(), id: decodeURIComponent(new URL(r.request().url()).pathname.split("/").at(-1)) }
        if (r.request().postData() !== first.body) return r.continue()
        await r.fetch()
        return r.abort("connectionreset")
      })
      await page.getByRole("button", { name: "Save as group" }).click()
      await page.clock.fastForward(31_000)
      await toastSaying(page, "timed out").waitFor()
      assert.ok(daemon.group(first.id), "the first make landed")
      await country(1)
      await page.getByRole("button", { name: "Save as group" }).click()
      blind = false
      for (let i = 0; i < 4 && !STORED.test(page.url()); i++) {
        await page.clock.fastForward(11_000)
        await page.waitForTimeout(300)
      }
      await page.waitForURL(STORED)
      const id = new URL(page.url()).pathname.split("/").at(-1)
      assert.equal(daemon.group(id).members.countries.values.length, 2, "the group opened holds both countries")
      assert.deepEqual([daemon.group(first.id).name, daemon.group(id).name], ["New group", "New group 2"], "two groups made on purpose, two names")
    } finally {
      await close()
    }
  })

  test("16. Save as group pressed again over the same filters after no answer sends the same make, though another writer took the name it sent, and makes one group", async () => {
    const { page, close } = await open("servers")
    try {
      await page.clock.install()
      await page.locator("tbody tr").first().waitFor()
      await page.locator("thead").getByRole("button", { name: "Filter servers by Country", exact: true }).click()
      await page.getByRole("dialog").getByRole("option").first().click()
      await page.keyboard.press("Escape")
      const before = new Set(daemon.groups().map((g) => g.id))
      let late = null
      let through = false
      await page.route(GROUP_WRITE, async (r) => {
        if (r.request().method() !== "PUT" || through) return r.continue()
        const headers = await r.request().allHeaders()
        if (headers["x-sidegate-csrf"]) late = { url: r.request().url(), headers, body: r.request().postData() }
        return r.abort()
      })
      await page.getByRole("button", { name: "Save as group" }).click()
      await page.clock.fastForward(31_000)
      await toastSaying(page, "timed out").waitFor()
      daemon.renameGroup(daemon.groups()[0].id, "New group")
      await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: /New group/ }).waitFor()
      through = true
      await page.getByRole("button", { name: "Save as group" }).click()
      await until(() => putsTo("api/groups/").some((w) => w.status === 200), "the make sent again lands")
      const delivered = await fetch(late.url, { method: "PUT", headers: late.headers, body: late.body })
      assert.equal(delivered.status, 200, "the first make, delivered late, is answered")
      const made = daemon.groups().filter((g) => !before.has(g.id))
      assert.equal(made.length, 1, "one group for one set of filters")
      assert.equal(made[0].name, "New group", "under the name it was first sent with")
    } finally {
      await close()
    }
  })

  test("16. Save as group over the filters of a group it made, deleted elsewhere since, makes a group at an id of its own under the name free then, and opens it", async () => {
    const { page, close } = await open("servers")
    try {
      const country = async () => {
        await page.locator("tbody tr").first().waitFor()
        await page.locator("thead").getByRole("button", { name: "Filter servers by Country", exact: true }).click()
        await page.getByRole("dialog").getByRole("option").first().click()
        await page.keyboard.press("Escape")
      }
      await country()
      await page.getByRole("button", { name: "Save as group" }).click()
      await page.waitForURL(STORED)
      const first = new URL(page.url()).pathname.split("/").at(-1)
      const main = page.getByRole("navigation", { name: "Main" })
      await main.getByRole("link", { name: /All servers/ }).click()
      await page.waitForURL(/\/servers$/)
      daemon.deleteGroup(first)
      daemon.renameGroup("all-aurora", "New group")
      await main.getByRole("link", { name: /New group/ }).waitFor()
      await until(async () => (await main.getByRole("link", { name: /New group/ }).count()) === 1, "the deleted group gone from the list")
      await country()
      await page.getByRole("button", { name: "Save as group" }).click()
      await page.waitForURL((url) => STORED.test(url.pathname) && !url.pathname.endsWith(`/${first}`))
      const id = new URL(page.url()).pathname.split("/").at(-1)
      assert.equal(daemon.group(first), undefined, "the deleted group isn't made again")
      assert.equal(daemon.group(id).name, "New group 2", "the name free then")
    } finally {
      await close()
    }
  })

  test("16. Save as group whose group another writer deleted before the view showed it stays on All servers with its filters, and opens nothing", async () => {
    const { page, close } = await open("servers")
    try {
      await page.locator("tbody tr").first().waitFor()
      await page.locator("thead").getByRole("button", { name: "Filter servers by Country", exact: true }).click()
      await page.getByRole("dialog").getByRole("option").first().click()
      await page.keyboard.press("Escape")
      const before = new Set(daemon.groups().map((g) => g.id))
      let blind = true
      await page.route("**/api/state", (r) => (blind ? r.abort() : r.continue()))
      const saveAs = page.getByRole("button", { name: "Save as group" })
      await saveAs.click()
      await until(() => putsTo("api/groups/").some((w) => w.status === 200), "the make lands")
      daemon.deleteGroup(daemon.groups().find((g) => !before.has(g.id)).id)
      blind = false
      await until(async () => STORED.test(new URL(page.url()).pathname) || ((await saveAs.count()) === 1 && (await saveAs.isEnabled())), "the make settled", 20_000)
      assert.match(new URL(page.url()).pathname, /\/servers$/, "the reader stays on All servers")
      assert.equal(await saveAs.count(), 1, "with the filters, Save as group at the chip line's end")
      assert.equal(await toasts(page).filter({ has: page.getByRole("button", { name: "Open" }) }).count(), 0, "and no Open to a group that has gone")
    } finally {
      await close()
    }
  })
})

describe("an answer after the reader has gone", () => {
  beforeEach(() => daemon.reset())

  /** Holds the page's next write to `pattern` until `release`. */
  async function holdWrite(page, pattern) {
    let release
    const held = new Promise((done) => { release = done })
    await page.route(pattern, async (r) => { if (r.method?.() === "PUT" || r.request().method() === "PUT") await held; await r.continue() })
    return () => release()
  }

  test("16. Save as group answered after the reader went elsewhere leaves them there, and the toast's Open goes to it", async () => {
    const { page, close } = await open("servers")
    try {
      await page.locator("tbody tr").first().waitFor()
      await page.locator("thead").getByRole("button", { name: "Filter servers by Country", exact: true }).click()
      await page.getByRole("dialog").getByRole("option").first().click()
      await page.keyboard.press("Escape")
      const release = await holdWrite(page, GROUP_WRITE)
      await page.getByRole("button", { name: "Save as group" }).click()
      await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Events" }).click()
      await page.waitForURL(/\/events$/)
      release()
      const said = toastSaying(page, "New group")
      await said.waitFor()
      assert.match(page.url(), /\/events$/, "the reader stays where they went")
      await said.getByRole("button", { name: "Open" }).click()
      await page.waitForURL(/\/groups\/[^/]+$/)
    } finally {
      await close()
    }
  })
})
