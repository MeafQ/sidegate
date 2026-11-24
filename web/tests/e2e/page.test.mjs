// The page the owner uses, end to end against the fake daemon: the board of slot tiles, the list, and each source's and
// group's page. Every answer on screen comes from the daemon, so these checks change what the daemon says and read what
// the page then says.
import assert from "node:assert/strict"
import { readFileSync, rmSync } from "node:fs"
import { after, before, beforeEach, describe, test } from "node:test"
import { fileURLToPath } from "node:url"
import { chromium, webkit } from "playwright-core"
import { build } from "vite"
import { failureStates, groupView, acme, server, slotView, source, startFakeDaemon, troubledSources } from "./fake-daemon.mjs"

const ROOT = fileURLToPath(new URL("../../", import.meta.url))
const DIST = fileURLToPath(new URL("./.dist-page/", import.meta.url))
const PORT = Number(process.env.E2E_PORT ?? 5311) + 4
const BASE = "/proxy/"

let browser
/** WebKit, launched by the first check that runs in it, since WebKit focuses no button a press lands on. */
let webkitBrowser
let daemon

before(async () => {
  rmSync(DIST, { recursive: true, force: true })
  process.env.BASE_PATH = BASE
  await build({ root: ROOT, logLevel: "warn", build: { outDir: DIST, emptyOutDir: true } })
  delete process.env.BASE_PATH
  daemon = await startFakeDaemon({ port: PORT, dist: DIST, base: BASE })
  browser = await chromium.launch()
})

after(async () => {
  await browser?.close()
  await webkitBrowser?.close()
  await daemon?.close()
  rmSync(DIST, { recursive: true, force: true })
})

beforeEach(() => daemon.reset())

/** The engines a press or a pointer is checked in: WebKit focuses no button a press lands on, where Chromium does, and the two
 * hear a pointer leave content that goes from under it differently. */
const ENGINES = ["chromium", "webkit"]

/** Opens a page; `clock` installs a clock the test can run on, at the time it gives or now; `engine` is "chromium" or "webkit";
 * `noCheck` fails the check for a newer release the page makes as it loads. */
async function open(path, { width = 1440, height = 900, theme = "dark", touch = false, clock = false, engine = "chromium", noCheck = false } = {}) {
  const context = await (engine === "webkit" ? (webkitBrowser ??= await webkit.launch()) : browser).newContext({ viewport: { width, height }, colorScheme: theme, hasTouch: touch })
  await context.addInitScript((t) => localStorage.setItem("theme", t), theme)
  if (clock) await context.clock.install(typeof clock === "number" ? { time: clock } : undefined)
  const page = await context.newPage()
  page.setDefaultTimeout(10_000)
  if (noCheck) await page.route("**/api/update/check", (route) => route.abort())
  await page.goto(daemon.url + path)
  return { page, context, close: () => context.close() }
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

/** The rows a menu draws highlighted, and the one its search names its active option, each by its value. */
const highlightOf = (menu) => menu.locator("[cmdk-root]").evaluate((root) => {
  const id = root.querySelector("[cmdk-input]").getAttribute("aria-activedescendant")
  return {
    drawn: [...root.querySelectorAll('[cmdk-item][aria-selected="true"]')].map((row) => row.getAttribute("data-value")),
    named: id ? (document.getElementById(id)?.getAttribute("data-value") ?? `no element ${id}`) : null,
  }
})

/** Holds every animation the page runs now where it stands, so a menu that fades as it closes stays there, closed. */
const freeze = (page) => page.evaluate(() => document.getAnimations().forEach((a) => a.pause()))
/** Holds the next popover to close at the first frame of its fade, however long the steps after take, so it stays there, closed. */
const freezeAsItCloses = (page) => page.evaluate(() => {
  const look = () => {
    const fading = [...document.querySelectorAll('[data-slot=popover-content][data-state="closed"]')].flatMap((e) => e.getAnimations())
    if (fading.length === 0) return requestAnimationFrame(look)
    fading.forEach((a) => a.pause())
  }
  requestAnimationFrame(look)
})
/** Lets every animation `freeze` held run on. */
const thaw = (page) => page.evaluate(() => document.getAnimations().forEach((a) => { if (a.playState === "paused") a.play() }))

/** A menu closed and still fading, and a menu open. */
const fading = (page) => page.locator('[data-slot=popover-content][data-state="closed"]').filter({ has: page.locator("[cmdk-list]") })
const opened = (page) => page.locator('[data-slot=popover-content][data-state="open"]').filter({ has: page.locator("[cmdk-list]") })

const toast = (page, text) => page.locator("[data-sonner-toast]").filter({ hasText: text })
const requests = (method, path) => daemon.log().filter((r) => r.method === method && r.path === BASE + path)
const board = (page) => page.getByRole("list", { name: "VPN Clients" })
const boxes = (page) => board(page).getByRole("group")
const tile = (page, name) => board(page).getByRole("group", { name, exact: true })
const addSlot = (page) => board(page).getByRole("button", { name: "Add", exact: true })
/** The card the board's Add becomes, and its Create, which makes the slot. */
const newSlot = (page) => board(page).getByRole("group", { name: "New slot", exact: true })
const createSlot = (page) => newSlot(page).getByRole("button", { name: "Create", exact: true })
/** The board's Add, its card created as it opens. */
async function addCreated(page) {
  await addSlot(page).click()
  await createSlot(page).click()
}
/** The question a slot's trash asks. */
const asking = (page) => page.getByRole("alertdialog")
/** The slots a press of Add created: a create is a slot's PUT that names no stored revision. */
const creates = () => daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(BASE + "api/slots/") && r.ifNoneMatch === "*")
const dot = (box, name) => box.getByRole("img", { name })
const nav = (page) => page.getByRole("navigation", { name: "Main" })
const saveBar = (page) => page.getByRole("group", { name: "Unsaved changes" })
const saveButton = (page) => saveBar(page).getByRole("button", { name: "Save", exact: true })
const discardOf = (page) => saveBar(page).getByRole("button", { name: "Discard", exact: true })

async function addMenu(page, item) {
  await nav(page).getByRole("button", { name: "Add", exact: true }).click()
  await page.getByRole("menuitem", { name: item, exact: true }).click()
}

/** Opens a new group's draft from Add, its name field open, and waits for its servers. */
async function draftGroup(page) {
  await addMenu(page, "Group")
  await page.getByLabel(/^Name of New group/).waitFor()
  await page.locator("tbody tr").first().waitFor()
}
/** A stored group's address, never the draft's. */
const STORED_GROUP = /\/groups\/(?!new$)[^/]+$/

/** Opens a column's filters from its header, or where a narrow screen hides the column, from the chip line's Filter button at its tab. */
async function openFilter(page, column) {
  const header = page.locator("thead").getByRole("button", { name: `Filter servers by ${column}`, exact: true })
  if (await header.isVisible()) await header.click()
  else {
    await page.getByRole("button", { name: "Filter", exact: true }).click()
    await page.getByRole("dialog").getByRole("radio", { name: column, exact: true }).click()
  }
  return page.getByRole("dialog")
}
/** A server's row, by the name it shows. A row's words also name the list's most stable server, so its text alone can't find it. */
const rowOf = (page, name) => page.locator("tbody tr").filter({ has: page.getByRole("button", { name, exact: true }) })
/** The class a group's tab dims a server it doesn't hold by. */
const OUTSIDE = "text-muted-foreground"
/** Whether a row is dimmed, as a group's tab dims a server it doesn't hold: its last cell is. */
const dimmed = async (page, name) => (await rowOf(page, name).locator("td").last().getAttribute("class")).includes(OUTSIDE)
/** A server's box on a group's tab, and whether it is ticked. */
const boxOf = (page, name) => page.getByRole("checkbox", { name: `Select ${name}`, exact: true })
const tickedOf = async (page, name) => (await boxOf(page, name).getAttribute("aria-checked")) === "true"
const textChip = (page, label) => page.getByRole("textbox", { name: label, exact: true })

/** What a group's table shows: its count, its chips as the eye reads them, a box as ☑ or ☐, and each row as `+:name`, with `-` for
 * a row its rule leaves out, dimmed. */
const tableNow = () => {
  const count = /\b\d+ (?:of \d+|in group)\b/.exec(document.body.innerText)?.[0] ?? null
  const seen = (chip) => {
    const copy = chip.cloneNode(true)
    copy.querySelectorAll(".sr-only").forEach((e) => e.remove())
    copy.querySelectorAll("[data-facet=box] > [aria-hidden]").forEach((mark) => mark.replaceWith(mark.querySelector("svg") ? "☑" : "☐"))
    return copy.textContent.replace(/\s+/g, " ").trim()
  }
  const chips = [...document.querySelectorAll('[aria-label="Filters"] > [aria-label$=" filter"]')].map(seen)
  const at = [...document.querySelectorAll("thead th")].findIndex((th) => th.innerText.trim().startsWith("Name"))
  const rows = [...document.querySelectorAll("tbody tr")].map((tr) => `${tr.cells[at].className.includes("text-muted-foreground") ? "-" : "+"}:${tr.cells[at].innerText.trim()}`)
  return JSON.stringify({ count, chips, rows })
}
const tableOf = async (page) => JSON.parse(await page.evaluate(tableNow))

/** Starts writing down the table each time the page changes. `tablesSeen` returns each distinct table in turn. */
async function recordTables(page) {
  await page.evaluate(`void (window.tableNow = ${tableNow})`)
  await page.evaluate(() => {
    const seen = [window.tableNow()]
    window.tablesSeen = seen
    new MutationObserver(() => { const t = window.tableNow(); if (t !== seen.at(-1)) seen.push(t) })
      .observe(document.body, { subtree: true, childList: true, characterData: true, attributes: true })
  })
}
const tablesSeen = async (page) => (await page.evaluate(() => window.tablesSeen)).map((t) => JSON.parse(t))

/** Waits until every read of `path` the daemon has had is answered, and the page has had a moment to show it. */
async function readsAnswered(page, path) {
  await until(() => requests("GET", path).every((r) => r.status !== 0), `the reads of ${path}`)
  await page.waitForTimeout(300)
}

const clients = (...names) => ({ known: true, limit: 8, foreign: names.map((name, i) => ({ id: `n${i}`, name, enabled: true })) })
const withUnifi = (id, rest = {}) => {
  const slot = daemon.slot(id)
  const slots = new Map([[id, { ...slot, unifi: { ...slot.unifi, known: true, client: "present", ...rest } }]])
  daemon.set({ slots })
}
/** Aurora's servers, each failing 9 times this week and answering now: rated Online. */
const shakyAurora = () => [server("l-ams", "Amsterdam", "NL", 15), server("l-fra", "Frankfurt", "DE", 9), server("l-par", "Paris", "FR", 19)]
  .map((s) => ({ ...s, health: { ...s.health, incidents7d: 9, factor: 2.6 } }))
/** Aurora with one server failing now. */
const auroraWithDown = (id) => [server("l-ams", "Amsterdam", "NL", 15), server("l-fra", "Frankfurt", "DE", 9), server("l-par", "Paris", "FR", 19)]
  .map((s) => (s.id === id ? { ...s, health: { ...s.health, verdict: "failed" } } : s))

/** The reviewer's ordering for a Delete refused as referenced: another writer points EU at the doomed thing, the daemon refuses
 * at the newer revision, and the read that follows lands with the older view, as one begun before the refusal can. */
/** Writes to `url` kept at the daemon's door until `release`, which lets every one waiting there through. */
function holdWrites(page, url) {
  const waiting = []
  return {
    install: () => page.route(url, async (r) => {
      if (r.request().method() === "GET") return r.continue()
      await new Promise((done) => { waiting.push(done) })
      await r.continue()
    }),
    release: async () => {
      await until(() => waiting.length > 0, "a write at the daemon's door")
      while (waiting.length > 0) waiting.shift()()
    },
  }
}

async function deleteRefusedOverAnOldRead(path, name, exit) {
  daemon.changeSlot("eu", { exit: { kind: "direct" } })
  const { page, close } = await open(path)
  try {
    await page.getByRole("button", { name: `Delete ${name}` }).click()
    const dialog = page.getByRole("alertdialog")
    const remove = dialog.getByRole("button", { name: "Delete", exact: true })
    await until(() => remove.isEnabled(), "Delete enables")
    const old = await fetch(daemon.url + "api/state").then((r) => r.json())
    daemon.changeSlot("eu", { exit }, { announce: false })
    let reads = 0
    await page.route("**/api/state", (r) => (++reads === 1 ? r.fulfill({ json: old }) : r.continue()))
    await remove.click()
    await until(() => daemon.log().some((r) => r.method === "DELETE" && r.status === 409), "the daemon refuses the delete")
    const usedBy = dialog.getByRole("region", { name: "Used by" })
    await until(() => reads >= 1, "the read after the refusal")
    await usedBy.locator("[data-slot=skeleton]").first().waitFor()
    assert.equal(await remove.getAttribute("aria-busy"), "true", "the old view lands and Delete is Pending while the blockers are read")
    assert.equal(await dialog.getByRole("button", { name: "Cancel" }).isEnabled(), true, "the delete was answered, so Cancel closes the dialog")
    await usedBy.getByRole("combobox", { name: "Exit of EU" }).waitFor({ timeout: 5_000 })
    assert.ok(reads >= 2, "the store reads the view again past the refusal")
    assert.equal(await usedBy.locator("[data-slot=skeleton]").count(), 0, "read, the blocker is EU's row")
    assert.equal(await remove.isDisabled(), true, "EU holds Delete back")
    assert.equal(await remove.getAttribute("aria-busy"), null, "and Delete is no longer Pending")
    assert.equal(await toast(page, "").count(), 0, "the refusal stands in the dialog alone")
  } finally {
    await close()
  }
}

describe("the board", () => {
  test("17. a tile per VPN Client: foreign ones first, read-only with real names, then our slots oldest first, then Add", async () => {
    const second = { ...slotView("a-later", 1, { name: "Second", exit: { kind: "group", group: "all-aurora" } }), createdAt: new Date(Date.now() + 60_000).toISOString() }
    second.unifi = { ...second.unifi, interface: "pxa2" }
    daemon.set({ slots: new Map([["a-later", second], ["eu", daemon.slot("eu")]]), vpnClients: clients("Office WireGuard") })
    const { page, close } = await open("")
    try {
      await board(page).getByText("Office WireGuard").waitFor()
      assert.equal(await boxes(page).count(), 3)
      const words = await boxes(page).allInnerTexts()
      assert.match(words[0], /^Office WireGuard/)
      assert.match(words[0], /Managed in UniFi/)
      assert.match(words[1], /^EU/, "the older slot first, though its id sorts later")
      assert.match(words[2], /^Second/)
      assert.equal(await boxes(page).nth(0).getByRole("combobox").count(), 0, "a foreign client takes no Exit")
      assert.equal(await boxes(page).nth(0).getByRole("button", { name: /^Rename/ }).count(), 0, "nor a name")
      await addSlot(page).waitFor()
      assert.doesNotMatch(await page.locator("body").innerText(), /VPN Clients \d|Serving \d|Attention|Healthy/)
      assert.equal(await page.getByRole("button", { name: "New slot" }).count() + await page.getByRole("link", { name: "New slot" }).count(), 0, "Add on the board is the only way to a slot")
    } finally {
      await close()
    }
  })

  test("17. a tile shows a status dot, name, Exit, active server and Ping, and no status in words", async () => {
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("Amsterdam").waitFor()
      const words = await box.innerText()
      for (const part of ["EU", "All Aurora", "Amsterdam", "15 / 21 ms"]) assert.ok(words.includes(part), `${part} in ${words}`)
      assert.doesNotMatch(words, /Serving/)
      assert.match(await dot(box, "Serving").getAttribute("class"), /bg-success/)
    } finally {
      await close()
    }
  })

  test("17. no tooltip opens over a slot's Exit menu, on hover or keyboard focus", async () => {
    const { page, close } = await open("")
    try {
      const exit = page.getByRole("combobox", { name: "Exit of EU" })
      await exit.hover()
      await page.waitForTimeout(800)
      assert.equal(await page.getByRole("tooltip").count(), 0, "on hover")
      await page.mouse.move(0, 0)
      await exit.focus()
      await page.waitForTimeout(800)
      assert.equal(await page.getByRole("tooltip").count(), 0, "on focus")
    } finally {
      await close()
    }
  })

  test("17. a serving slot's dot is its server's, the same as that server's row: red while it fails now", async () => {
    daemon.set({ catalogue: auroraWithDown("l-ams") })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("Amsterdam").waitFor()
      assert.match(await dot(box, "Serving · Offline · Not responding").getAttribute("class"), /bg-destructive/)
      assert.equal(await box.getByRole("img", { name: "Offline", exact: true }).count(), 0, "the server line has no dot of its own")
      await page.goto(daemon.url + "servers")
      await rowOf(page, "Amsterdam").getByRole("img", { name: "Offline", exact: true }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. a serving slot whose server answered now reads Online, however often it failed this week", async () => {
    daemon.set({ catalogue: shakyAurora() })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("Amsterdam").waitFor()
      assert.match(await dot(box, "Serving").getAttribute("class"), /bg-success/)
      assert.equal(await box.getByRole("img", { name: "Online", exact: true }).count(), 0, "the server line has no dot of its own")
      await page.goto(daemon.url + "servers")
      await rowOf(page, "Amsterdam").getByRole("img", { name: "Online", exact: true }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. a serving slot whose server nothing probed lately reads Unchecked, the same as that server's row", async () => {
    const ams = server("l-ams", "Amsterdam", "NL", 15)
    daemon.set({ catalogue: [{ ...ams, health: { ...ams.health, current: false, checkedAt: new Date(Date.now() - 2 * 3600e3).toISOString() } }, server("l-fra", "Frankfurt", "DE", 9), server("l-par", "Paris", "FR", 19)] })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("Amsterdam").waitFor()
      const colour = await dot(box, "Serving · Unchecked · Last checked 2h ago").getAttribute("class")
      assert.match(colour, /bg-muted-foreground\/40/)
      assert.doesNotMatch(colour, /bg-success/)
      assert.equal(await box.getByRole("img", { name: "Unchecked", exact: true }).count(), 0, "the server line has no dot of its own")
      await page.goto(daemon.url + "servers")
      await rowOf(page, "Amsterdam").getByRole("img", { name: "Unchecked", exact: true }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. without a read of UniFi nothing reads as free, and Add still asks Go for a slot", async () => {
    const { page, close } = await open("")
    try {
      await board(page).waitFor()
      assert.equal(await page.getByLabel("Notices").count(), 0, "the board has no strip: the Add box stands for the positions UniFi wasn't read for")
      assert.doesNotMatch(await board(page).innerText(), /free/i, "nothing reads as free")
      await addCreated(page)
      const put = await until(() => daemon.log().find((r) => r.method === "PUT" && r.path.includes("api/slots/")), "the create")
      assert.equal(put.ifNoneMatch, "*")
      assert.deepEqual(JSON.parse(put.body), { name: "Slot 1", exit: { kind: "block" } }, "a new slot has no Exit yet")
      await tile(page, "Slot 1").waitFor()
    } finally {
      await close()
    }
  })

  test("17. Go's capacity check has the last word on Add", async () => {
    const { page, close } = await open("")
    try {
      await addSlot(page).waitFor()
      await page.route("**/api/slots/*", (r) => r.fulfill({ status: 409, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "capacity", reason: "clientsFull", message: "Every VPN Client UniFi allows is in use." }) }))
      await addCreated(page)
      await toast(page, "limit reached").waitFor()
      assert.equal(daemon.slots().length, 1)
      assert.equal(await createSlot(page).isDisabled(), false, "Create takes a press again")
    } finally {
      await close()
    }
  })

  /** Holds the next slot write until it is released. */
  const heldWrite = async (page) => {
    let release
    const released = new Promise((r) => { release = r })
    await page.route("**/api/slots/*", async (route) => { await released; await route.continue().catch(() => {}) }, { times: 1 })
    return release
  }
  const onBox = (page) => addSlot(page).evaluate((el) => el === document.activeElement)
  const nameField = (page) => newSlot(page).getByRole("textbox", { name: "New slot name" })
  const exitField = (page) => newSlot(page).getByRole("combobox", { name: "Exit of new slot" })

  test("19. a press of Add writes nothing and turns the box into the new slot's card in its place, the name it would get selected and the keys in it, on Block; Create makes the slot, the card waiting until it lands, and the next Add offers the next name", async () => {
    const { page, close } = await open("")
    try {
      await addSlot(page).waitFor()
      const box = await addSlot(page).boundingBox()
      await addSlot(page).click()
      await newSlot(page).waitFor()
      assert.equal(await asking(page).count(), 0, "in place, with no dialog")
      const card = await newSlot(page).boundingBox()
      assert.deepEqual(card, box, "the card has the box's own place")
      assert.equal(await nameField(page).inputValue(), "Slot 1", "the name the slot would get")
      assert.equal(await nameField(page).evaluate((el) => el === document.activeElement && getSelection().toString() === el.value), true, "the keys in its name, all of it selected")
      assert.equal((await exitField(page).innerText()).trim(), "Block")
      assert.deepEqual(await newSlot(page).getByRole("button").evaluateAll((bs) => bs.map((b) => b.getAttribute("aria-label") ?? b.textContent)), ["Cancel new slot", "Cancel", "Create"])
      await page.waitForTimeout(300)
      assert.equal(creates().length, 0, "the press of Add wrote nothing")
      const release = await heldWrite(page)
      await createSlot(page).click()
      await until(async () => (await createSlot(page).getAttribute("aria-busy")) === "true", "Create spins while the create is out")
      for (const name of ["Cancel new slot", "Cancel", "Create"]) assert.equal(await newSlot(page).getByRole("button", { name, exact: true }).isDisabled(), true, `${name} takes no press`)
      assert.equal(await nameField(page).getAttribute("readonly"), "", "nor the name an edit")
      await page.keyboard.press("Escape")
      await nameField(page).press("Escape")
      await nameField(page).press("Enter")
      await exitField(page).click({ force: true })
      await page.waitForTimeout(200)
      assert.equal(await page.getByRole("dialog").count(), 0, "nor the Exit a pick")
      assert.equal(await newSlot(page).count(), 1, "and Escape leaves it")
      release()
      await tile(page, "Slot 1").waitFor()
      await newSlot(page).waitFor({ state: "detached" })
      assert.equal(creates().length, 1, "one slot made")
      assert.deepEqual(JSON.parse(creates()[0].body), { name: "Slot 1", exit: { kind: "block" } })
      await addSlot(page).click()
      assert.equal(await nameField(page).inputValue(), "Slot 2", "the next press names the next slot")
    } finally {
      await close()
    }
  })

  test("19. Create makes the slot with the name typed, trimmed, and the Exit picked, with a blank name Create takes no press, and Enter in the name makes none", async () => {
    const { page, close } = await open("")
    try {
      await addSlot(page).click()
      await nameField(page).fill("   ")
      assert.equal(await createSlot(page).isDisabled(), true, "Create takes no press on a blank name")
      await nameField(page).press("Enter")
      await exitField(page).click()
      await page.getByRole("dialog").locator('[data-value="group:all-aurora"]').click()
      await page.getByRole("dialog").waitFor({ state: "detached" })
      assert.equal((await exitField(page).innerText()).trim(), "All Aurora")
      await nameField(page).fill("  Office ")
      await page.waitForTimeout(200)
      await nameField(page).press("Enter")
      await page.waitForTimeout(200)
      assert.equal(creates().length, 0, "nothing written before Create, by Enter in the name neither")
      await createSlot(page).press("Enter")
      await tile(page, "Office").waitFor()
      await newSlot(page).waitFor({ state: "detached" })
      assert.deepEqual(creates().map((r) => JSON.parse(r.body)), [{ name: "Office", exit: { kind: "group", group: "all-aurora" } }])
    } finally {
      await close()
    }
  })

  test("19. a create the daemon refuses keeps the card as typed; a refused Exit's toast names the Exit, with Open to its page", async () => {
    const { page, close } = await open("")
    try {
      await page.route("**/api/slots/*", (r) => r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "groupEmpty", message: "This group has no servers." }) }))
      await addSlot(page).click()
      await nameField(page).fill("Office")
      await exitField(page).click()
      await page.getByRole("dialog").locator('[data-value="group:all-aurora"]').click()
      await createSlot(page).click()
      await toast(page, "All Aurora").waitFor()
      assert.equal(await toast(page, "All Aurora").getByRole("link", { name: "Open" }).or(toast(page, "All Aurora").getByRole("button", { name: "Open" })).count(), 1, "Open to the group's page")
      assert.equal(await nameField(page).inputValue(), "Office")
      assert.equal((await exitField(page).innerText()).trim(), "All Aurora")
      assert.equal(await createSlot(page).isEnabled(), true, "Create takes a press again")
    } finally {
      await close()
    }
  })

  test("19. after no answer the card stays as typed and the toast's Retry that lands closes it; edited since, it stays, and Create makes a second slot", async () => {
    const { page, close } = await open("", { clock: true })
    try {
      let down = true
      await page.route("**/api/slots/*", (r) => (down ? r.abort() : r.continue()))
      const said = toast(page, "timed out")
      const noAnswer = async () => {
        await until(() => createSlot(page).getAttribute("aria-busy"), "the create out")
        await page.clock.fastForward(31_000)
        await said.waitFor()
        assert.equal(await createSlot(page).isEnabled(), true, "Create takes a press again")
        down = false
      }
      await addSlot(page).click()
      await createSlot(page).click()
      await noAnswer()
      assert.equal(await nameField(page).inputValue(), "Slot 1")
      await said.getByRole("button", { name: "Retry" }).click()
      await tile(page, "Slot 1").waitFor()
      await newSlot(page).waitFor({ state: "detached" })

      down = true
      await addSlot(page).click()
      await createSlot(page).click()
      await noAnswer()
      await nameField(page).fill("Office")
      await said.getByRole("button", { name: "Retry" }).click()
      await tile(page, "Slot 2").waitFor()
      await page.waitForTimeout(200)
      assert.equal(await nameField(page).inputValue(), "Office", "the card holds what was typed since")
      await createSlot(page).click()
      await tile(page, "Office").waitFor()
      await newSlot(page).waitFor({ state: "detached" })
      assert.deepEqual([...daemon.slots()].length, 4, "EU, Slot 1, Slot 2 and Office")
    } finally {
      await close()
    }
  })

  test("19. Escape in the new slot's Exit menu closes the menu alone, and a press outside the card leaves it", async () => {
    const { page, close } = await open("")
    try {
      await addSlot(page).click()
      await nameField(page).fill("Office")
      await exitField(page).click()
      await opened(page).waitFor()
      await page.keyboard.press("Escape")
      await page.getByRole("dialog").waitFor({ state: "detached" })
      assert.equal(await nameField(page).inputValue(), "Office", "the card and its name stay")
      await page.mouse.click(700, 700)
      await page.waitForTimeout(200)
      assert.equal(await nameField(page).inputValue(), "Office")
      await exitField(page).focus()
      await page.keyboard.press("Escape")
      await newSlot(page).waitFor({ state: "detached" })
      assert.equal(creates().length, 0)
    } finally {
      await close()
    }
  })

  for (const engine of ENGINES) {
    for (const [way, act] of [
      ["Cancel", (page) => newSlot(page).getByRole("button", { name: "Cancel", exact: true }).click()],
      ["×", (page) => newSlot(page).getByRole("button", { name: "Cancel new slot" }).click()],
      ["Escape", (page) => page.keyboard.press("Escape")],
    ]) {
      test(`19. in ${engine}, ${way} drops the new slot's card, writes nothing and gives the keys back to Add, which opens it fresh`, async () => {
        const { page, close } = await open("", { engine })
        try {
          await addSlot(page).focus()
          await page.keyboard.press("Space")
          await nameField(page).fill("Office")
          await act(page)
          await newSlot(page).waitFor({ state: "detached" })
          assert.equal(await onBox(page), true, `after ${way} the keys are on Add`)
          await page.waitForTimeout(300)
          assert.equal(creates().length, 0, `${way} made no slot`)
          await addSlot(page).click()
          assert.equal(await nameField(page).inputValue(), "Slot 1")
        } finally {
          await close()
        }
      })
    }

    test(`19. in ${engine}, Enter held on Add or pressed twice opens the card and makes nothing, nor does Enter held in the name; held on Create, which Tab reaches, it makes one slot and opens no second card`, async () => {
      const { page, close } = await open("", { engine })
      try {
        /* Playwright sends every down of a key already down as a repeat. */
        const held = async () => {
          for (const stop = Date.now() + 700; Date.now() < stop;) { await page.keyboard.down("Enter"); await page.waitForTimeout(30) }
          await page.keyboard.up("Enter")
          await page.waitForTimeout(300)
        }
        await addSlot(page).focus()
        await held()
        assert.equal(await newSlot(page).count(), 1)
        assert.equal(creates().length, 0, "a held Enter made no slot")
        await page.keyboard.press("Escape")
        await newSlot(page).waitFor({ state: "detached" })
        await page.keyboard.press("Enter")
        await page.keyboard.press("Enter")
        await page.waitForTimeout(300)
        assert.equal(await nameField(page).evaluate((el) => el === document.activeElement), true, "two presses leave the keys in the name")
        assert.equal(creates().length, 0, "and made no slot")
        await held()
        assert.equal(creates().length, 0, "nor did Enter held in the name")
        for (const stop of ["Cancel new slot", "Exit of new slot", "Cancel", "Create"]) {
          await page.keyboard.press(engine === "webkit" ? "Alt+Tab" : "Tab")
          const at = await page.evaluate(() => document.activeElement.getAttribute("aria-label") ?? document.activeElement.textContent)
          assert.equal(at, stop)
        }
        await held()
        await tile(page, "Slot 1").waitFor()
        assert.equal(creates().length, 1, "one slot")
        assert.equal(await newSlot(page).count(), 0, "and Add, which has the keys again, took none of the repeats")
        assert.equal(await onBox(page), true)
      } finally {
        await close()
      }
    })

    test(`19. in ${engine}, once its slot lands the keys are on the board's Add`, async () => {
      const { page, close } = await open("", { engine })
      try {
        const release = await heldWrite(page)
        await addSlot(page).click()
        await createSlot(page).click()
        await until(() => createSlot(page).getAttribute("aria-busy"), "the create out")
        release()
        await tile(page, "Slot 1").waitFor()
        await newSlot(page).waitFor({ state: "detached" })
        assert.equal(await onBox(page), true)
        assert.equal(creates().length, 1)
      } finally {
        await close()
      }
    })
  }

  test("19. a board left while a create is out and come back to is Add, spinning and taking no press until the slot lands", async () => {
    const { page, close } = await open("")
    try {
      const release = await heldWrite(page)
      await addSlot(page).click()
      await createSlot(page).click()
      await until(() => createSlot(page).getAttribute("aria-busy"), "the create out")
      await nav(page).getByRole("link", { name: "All servers" }).click()
      await page.waitForURL(/\/servers$/)
      await nav(page).getByRole("link", { name: "Dashboard" }).click()
      await until(async () => (await addSlot(page).getAttribute("aria-busy")) === "true", "the box spins while the create is out")
      await addSlot(page).click({ force: true })
      await page.waitForTimeout(200)
      assert.equal(await newSlot(page).count(), 0, "and takes no press")
      release()
      await tile(page, "Slot 1").waitFor()
      await until(async () => (await addSlot(page).getAttribute("aria-busy")) === null, "the box done spinning")
      assert.equal(creates().length, 1)
    } finally {
      await close()
    }
  })

  /** Two slots on Direct, so the board's Add stands alone in its row, in the middle of a phone's screen. */
  const twoOnDirect = () => daemon.set({ slots: new Map(["EU", "Home"].map((name, i) => [name.toLowerCase(), slotView(name.toLowerCase(), 3 + i, { name, exit: { kind: "direct" } })])) })
  const middleOf = async (locator) => { const b = await locator.boundingBox(); return { x: b.x + b.width / 2, y: b.y + b.height / 2 } }

  for (const engine of ENGINES) {
    test(`19. in ${engine}, on a 390 by 664 phone a tap on Add's middle and two more on the same place make no VPN Client`, async () => {
      twoOnDirect()
      const { page, close } = await open("", { width: 390, height: 664, touch: true, engine })
      try {
        await addSlot(page).scrollIntoViewIfNeeded()
        const at = await middleOf(addSlot(page))
        await page.touchscreen.tap(at.x, at.y)
        await newSlot(page).waitFor()
        await page.touchscreen.tap(at.x, at.y)
        await page.waitForTimeout(150)
        await page.touchscreen.tap(at.x, at.y)
        await page.waitForTimeout(400)
        assert.equal(creates().length, 0, "the taps after the first made no VPN Client")
      } finally {
        await close()
      }
    })

    test(`19. in ${engine}, the phone's keyboard coming up over the new slot's card, going, and the phone turned leave the card as typed`, async () => {
      twoOnDirect()
      const { page, close } = await open("", { width: 390, height: 740, touch: true, engine })
      try {
        await addSlot(page).tap()
        await nameField(page).fill("Office")
        for (const size of [{ width: 390, height: 480 }, { width: 390, height: 740 }, { width: 740, height: 390 }]) {
          await page.setViewportSize(size)
          await page.waitForTimeout(150)
          assert.equal(await nameField(page).inputValue(), "Office", JSON.stringify(size))
        }
        assert.equal(creates().length, 0, "nothing written")
      } finally {
        await close()
      }
    })
  }

  const trash = (page, name) => tile(page, name).getByRole("button", { name: `Delete ${name}`, exact: true })
  const deletes = () => requests("DELETE", "api/slots/eu")

  test("19. a slot's trash asks before it deletes, the keys on Cancel, which writes nothing; Delete spins and nothing closes the dialog until the slot is gone", async () => {
    const { page, close } = await open("")
    try {
      const button = trash(page, "EU")
      const link = tile(page, "EU").getByRole("link", { name: "Open EU in UniFi" })
      await button.waitFor()
      assert.ok((await button.boundingBox()).x < (await link.boundingBox()).x, "beside the UniFi link")
      await button.click()
      await asking(page).waitFor()
      assert.equal((await asking(page).getByRole("heading").innerText()).trim(), "Delete EU?")
      await asking(page).getByText("Removes the VPN Client from UniFi. Move any Policy Table routes off it first.", { exact: true }).waitFor()
      const cancel = asking(page).getByRole("button", { name: "Cancel" })
      const remove = asking(page).getByRole("button", { name: "Delete", exact: true })
      assert.equal(await cancel.evaluate((el) => el === document.activeElement), true, "the keys on Cancel")
      await cancel.click()
      await asking(page).waitFor({ state: "detached" })
      assert.equal(await button.evaluate((el) => el === document.activeElement), true, "Cancel gives the keys back")
      assert.equal(deletes().length, 0, "and writes nothing")
      const release = await heldWrite(page)
      await button.click()
      await remove.click()
      await until(() => remove.getAttribute("aria-busy"), "Delete spins")
      assert.equal(await remove.isDisabled(), true)
      assert.equal(await cancel.isDisabled(), true)
      await page.keyboard.press("Escape")
      await page.waitForTimeout(200)
      assert.equal(await asking(page).count(), 1, "nothing closes it while the delete is out")
      release()
      await tile(page, "EU").waitFor({ state: "detached" })
      await asking(page).waitFor({ state: "detached" })
      assert.equal(deletes().length, 1)
      assert.equal(daemon.slot("eu"), undefined)
    } finally {
      await close()
    }
  })

  test("19. a delete of a slot UniFi routes to is refused: the dialog closes, the toast says still routed and the slot stays", async () => {
    const { page, close } = await open("")
    try {
      await page.route("**/api/slots/eu", (r) => r.request().method() === "DELETE" ? r.fulfill({ status: 409, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "referenced", reason: "routed", message: "UniFi routes still point at this slot." }) }) : r.continue())
      await trash(page, "EU").click()
      await asking(page).getByRole("button", { name: "Delete", exact: true }).click()
      await toast(page, "still routed").waitFor()
      await asking(page).waitFor({ state: "detached" })
      assert.equal(await trash(page, "EU").isEnabled(), true)
      assert.ok(daemon.slot("eu"))
    } finally {
      await close()
    }
  })

  test("19. a slot's trash takes no press while its Exit pick or its VPN Client's Enable is out", async () => {
    let { page, close } = await open("")
    try {
      await trash(page, "EU").waitFor()
      const release = await heldWrite(page)
      await pickExit(page, "direct")
      await until(() => trash(page, "EU").isDisabled(), "held by the pick")
      release()
      await until(() => trash(page, "EU").isEnabled(), "and back once it is answered")
    } finally {
      await close()
    }
    daemon.reset()
    withUnifi("eu", { client: "disabled" });
    ({ page, close } = await open(""))
    try {
      let release
      const released = new Promise((r) => { release = r })
      await page.route("**/api/slots/eu/**", async (route) => { await released; await route.continue().catch(() => {}) }, { times: 1 })
      await tile(page, "EU").getByRole("button", { name: "Enable", exact: true }).click()
      await until(() => trash(page, "EU").isDisabled(), "held by Enable")
      release()
      await until(() => trash(page, "EU").isEnabled(), "and back once it is answered")
    } finally {
      await close()
    }
  })

  test("17. a slot on a server Exit names the server in its menu alone: its line is the ping at the right, when it leaves by while it does, and Last active beside it when no pass vouches for the server", async () => {
    const eu = slotView("eu", 3, { name: "EU", exit: { kind: "server", server: "l-ams" } })
    const shows = async (slot) => {
      daemon.set({ slots: new Map([["eu", slot]]) })
      const { page, close } = await open("")
      const line = tile(page, "EU").locator("[data-slot=slot-line]")
      await line.getByText(/ms$|^Switching by/).waitFor()
      return { page, close, line }
    }
    let { page, close, line } = await shows(eu)
    try {
      assert.equal((await line.innerText()).trim(), "15 / 21 ms")
      assert.equal(await line.locator("img, svg").count(), 0, "no second flag")
      assert.match(await page.getByRole("combobox", { name: "Exit of EU" }).innerText(), /Amsterdam/)
      const [ping, row] = [await line.getByText("15 / 21 ms").boundingBox(), await line.boundingBox()]
      assert.ok(Math.abs(ping.x + ping.width - row.x - row.width) < 1, "against the line's right edge")
    } finally {
      await close()
    }
    ({ close, line } = await shows({ ...eu, leavingBy: new Date(Date.now() + 600e3).toISOString() }))
    try {
      assert.match((await line.innerText()).trim(), /^Switching by \S+/)
    } finally {
      await close()
    }
    ({ page, close, line } = await shows({ ...eu, appliedServer: null, runtime: { ...eu.runtime, phase: "unknown" } }))
    try {
      assert.equal((await line.innerText()).replace(/\s+/g, " ").trim(), "Last active 15 / 21 ms")
      await line.getByRole("button", { name: "Last active server: Amsterdam" }).hover()
      const card = page.locator("[data-radix-popper-content-wrapper]")
      await card.waitFor()
      assert.match((await card.innerText()).replace(/\s+/g, " "), /Last active server Amsterdam/)
    } finally {
      await close()
    }
  })

  test("17. a group the daemon refuses to make is the draft's toast, in its reason's words: the draft stays, its ticks with it, and no group is made", async () => {
    const { page, close } = await open("servers")
    try {
      await nav(page).getByRole("link", { name: /All Aurora/ }).waitFor()
      await page.route(/\/api\/groups\/(?!preview$)[^/]+$/, (route) => route.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "sourcesGone", message: "Every source this group named is gone. Choose a source." }) }))
      await draftGroup(page)
      await boxOf(page, "Paris").click()
      await saveButton(page).click()
      await toast(page, "sources gone").waitFor()
      assert.equal(new URL(page.url()).pathname, BASE + "groups/new", "the draft stays")
      assert.ok(await tickedOf(page, "Paris"), "its tick with it")
      assert.equal(await nav(page).getByRole("link", { name: /New group/ }).count(), 0)
      assert.equal(daemon.groups().length, 1)
    } finally {
      await close()
    }
  })

  test("17. past 8 every client shows and nothing is free", async () => {
    daemon.set({ vpnClients: clients(...Array.from({ length: 9 }, (_, i) => `Foreign ${i}`)) })
    const { page, close } = await open("")
    try {
      await until(async () => (await boxes(page).count()) === 10, "every client")
      assert.equal(await addSlot(page).count(), 0, "no way to ask for a position that isn't there")
    } finally {
      await close()
    }
  })

  test("17. an Exit with no servers keeps its row, muted, with its Trouble words under its name and Open at its end, and a press anywhere on it or Enter closes the menu and opens its page once, picking nothing", async () => {
    const among = (values) => ({ not: false, values })
    daemon.set({ groups: new Map([["none", groupView("none", 1, { name: "Nothing", members: { sources: among([]), countries: among(["ZZ"]), protocols: among([]), include: "", exclude: "", box: null } })]]) })
    const { page, close } = await open("")
    try {
      await page.evaluate(() => {
        const push = history.pushState.bind(history)
        window.pushes = 0
        history.pushState = (...args) => { window.pushes++; return push(...args) }
      })
      const option = page.getByRole("dialog").locator('[data-value="group:none"]')
      const openMenu = async () => {
        await page.getByRole("combobox", { name: "Exit of EU" }).click()
        await option.waitFor()
      }
      const opensOnce = async (how, press) => {
        const before = await page.evaluate(() => window.pushes)
        await press()
        await page.waitForURL(/\/groups\/none$/)
        await until(async () => (await page.getByRole("dialog").count()) === 0, `${how}: the menu closes`)
        assert.equal(await page.evaluate(() => window.pushes) - before, 1, `${how}: one navigation`)
        await page.goBack()
      }
      await openMenu()
      const [name, words] = await option.locator("span.flex-col > span").allInnerTexts()
      assert.deepEqual([name, words], ["Nothing", "No servers"], "its words stand on a line of their own under its name")
      assert.equal(await toneOf(option.getByText("No servers", { exact: true })), "bad")
      assert.equal(await toneOf(option.getByText("Nothing", { exact: true })), "muted", "its name is drawn as one that can't be picked")
      const openLink = option.getByRole("link", { name: "Open" })
      assert.equal(await openLink.getAttribute("href"), "/proxy/groups/none")
      assert.equal(await openLink.getAttribute("tabindex"), "-1", "the row is the one keyboard stop")
      assert.equal(await option.getByText("—").count(), 0, "Open stands where the dashes stood")
      assert.equal(await page.locator("[data-radix-popper-content-wrapper]").filter({ has: page.getByText("0", { exact: true }) }).count(), 0, "no card")
      await opensOnce("a press on its name", () => option.getByText("Nothing", { exact: true }).click())
      await openMenu()
      await opensOnce("a press on Open", () => openLink.click())
      await openMenu()
      await page.keyboard.type("Nothing")
      await until(async () => (await option.getAttribute("data-selected")) === "true", "the search highlights it")
      await opensOnce("Enter", () => page.keyboard.press("Enter"))
      assert.equal(requests("PUT", "api/slots/eu").length, 0, "nothing is picked")
    } finally {
      await close()
    }
  })

  test("17. a source whose servers are all missing has no servers in the Exit menu as in the list: its row says No servers with Open, and a press opens its page and picks nothing", async () => {
    daemon.set({ catalogue: daemon.catalogue().map((l) => (l.source === "aurora" ? { ...l, presence: "missing", missingSince: new Date().toISOString() } : l)) })
    const { page, close } = await open("")
    try {
      await nav(page).locator('a[href$="/sources/aurora"]').getByText("No servers", { exact: true }).waitFor()
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const option = page.getByRole("dialog").locator('[data-value="source:aurora"]')
      await option.getByRole("link", { name: "Open" }).waitFor({ timeout: 5000 })
      assert.equal(await option.getByText("No servers", { exact: true }).count(), 1, "its words under its name")
      assert.equal(await option.getByText("—").count(), 0, "Open stands where the dashes stood")
      await option.getByText("Aurora", { exact: true }).click()
      await page.waitForURL(/\/sources\/aurora$/)
      assert.equal(requests("PUT", "api/slots/eu").length, 0, "nothing is picked")
    } finally {
      await close()
    }
  })

  test("17. a failed box on an Exit with no servers says No servers in its Trouble words with Open to that Exit's page", async () => {
    const among = (values) => ({ not: false, values })
    daemon.set({
      groups: new Map([["none", groupView("none", 1, { name: "Nothing", members: { sources: among([]), countries: among(["ZZ"]), protocols: among([]), include: "", exclude: "", box: null } })]]),
      slots: new Map([["eu", { ...daemon.slot("eu"), exit: { kind: "group", group: "none" }, status: "failed", downReason: "noUsableServer", appliedServer: null, server: null }]]),
    })
    const { page, close } = await open("")
    try {
      const line = tile(page, "EU").locator("[data-slot=slot-line]")
      await line.getByText("No servers", { exact: true }).waitFor()
      assert.equal(await toneOf(line.getByText("No servers", { exact: true })), "bad")
      assert.equal((await line.innerText()).replace(/\s+/g, " ").trim(), "No servers Open", "the Trouble words and Open, as Enable is drawn")
      await line.getByRole("link", { name: "Open" }).click()
      await page.waitForURL(/\/groups\/none$/)
    } finally {
      await close()
    }
  })

  test("17. the Exit menu has Direct, Block, sources, groups and every server its sources list, with the Servers table's facts", async () => {
    daemon.set({
      catalogue: [server("l-ams", "Amsterdam", "NL", 15), server("l-fra", "Frankfurt", "DE", 9), server("l-ori", "EDGE-Amsterdam", "NL", 40)],
      groups: new Map([["all-aurora", groupView("all-aurora", 1, { name: "All Aurora", members: { sources: { not: false, values: ["aurora"] }, countries: { not: false, values: [] }, protocols: { not: false, values: [] }, include: "", exclude: "", box: null } }, ["eu"])], ["one", groupView("one", 1, { name: "Only one", members: { sources: { not: false, values: [] }, countries: { not: false, values: [] }, protocols: { not: false, values: [] }, include: "", exclude: "", box: "ticked" }, ticked: ["l-ams"] })]]),
    })
    const { page, close } = await open("")
    try {
      const menu = page.getByRole("combobox", { name: "Exit of EU" })
      await menu.click()
      const list = page.getByRole("dialog")
      await list.getByText("Frankfurt").waitFor()
      const words = await list.innerText()
      for (const exit of ["Direct", "Block", "Aurora", "All Aurora", "Only one"]) assert.ok(words.includes(exit), exit)
      for (const column of ["Ping", "Stability"]) assert.ok(words.includes(column), column)
      assert.doesNotMatch(words, /Score/)
      assert.doesNotMatch(words, /Up 7d/)
      assert.ok(words.includes("EDGE-Amsterdam"), "every server its source lists is offered")
      const all = list.getByRole("option", { name: /All Aurora/ })
      assert.match(await all.innerText(), /9 \/ 13 ms/, "a group's row has its first choice's ping")
      assert.match(await all.innerText(), /9 \/ 13 ms\s*Stability 4 of 4\. No failure in the last 7 days\. Seen working 7 days\.$/, "and its first choice's stability")
      await page.getByPlaceholder("Search").fill("nothing like it")
      await list.getByText("No match").waitFor()
      await page.getByPlaceholder("Search").fill("frank")
      await list.getByText("Frankfurt").click()
      const put = await until(() => requests("PUT", "api/slots/eu")[0], "the assignment")
      assert.equal(put.ifMatch, '"3"')
      assert.deepEqual(JSON.parse(put.body), { name: "EU", exit: { kind: "server", server: "l-fra" } })
      await until(async () => /Frankfurt/.test(await menu.innerText()), "the box's new Exit")
    } finally {
      await close()
    }
  })

  test("17. a board assignment beaten by another writer overwrites nothing, says what is stored over the reader's pick, and Keep mine puts the pick back over it, keeping the other writer's name", async () => {
    const { page, close } = await open("")
    try {
      const menu = page.getByRole("combobox", { name: "Exit of EU" })
      await menu.waitFor()
      await page.route("**/api/state", (r) => r.abort(), { times: 1 })
      await page.route("**/api/events", (r) => r.abort())
      daemon.renameSlot("eu", "Theirs")
      await menu.click()
      await page.getByRole("dialog").getByText("Frankfurt").click()
      const said = toast(page, "changed to")
      await said.waitFor()
      assert.equal(requests("PUT", "api/slots/eu")[0].status, 412)
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "EU changed to All Aurora Your value: Frankfurt Keep mine", "what is stored now, then the reader's pick")
      assert.equal(daemon.slot("eu").name, "Theirs", "nothing overwritten")
      await until(async () => (await shownWords(page.getByRole("combobox", { name: "Exit of Theirs" }))) === "All Aurora", "the select shows what is stored")
      await said.getByRole("button", { name: "Keep mine" }).click()
      await until(() => daemon.slot("eu").exit.server === "l-fra", "Keep mine stores the pick")
      const [first, again] = requests("PUT", "api/slots/eu")
      assert.notEqual(again.idempotencyKey, first.idempotencyKey, "a new write under a new key")
      assert.deepEqual(JSON.parse(again.body), { name: "Theirs", exit: { kind: "server", server: "l-fra" } }, "the pick over what is stored now, the other writer's name kept")
      assert.equal(daemon.slot("eu").name, "Theirs")
      await said.waitFor({ state: "detached" })
    } finally {
      await close()
    }
  })

  test("17. a phone shows every tile without sideways scrolling", async () => {
    daemon.set({ vpnClients: clients("Office WireGuard") })
    const { page, close } = await open("", { width: 390 })
    try {
      await board(page).getByText("Office WireGuard").waitFor()
      assert.equal(await boxes(page).count(), 2)
      await addSlot(page).waitFor()
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth))
    } finally {
      await close()
    }
  })

  test("17. renaming a slot on its tile leaves its Exit untouched", async () => {
    const { page, close } = await open("")
    try {
      await page.getByRole("button", { name: "Rename EU" }).click()
      const name = page.getByLabel("Name of EU")
      await name.fill("Europe")
      await name.press("Enter")
      const put = await until(() => requests("PUT", "api/slots/eu")[0], "the rename")
      assert.equal(put.ifMatch, '"3"')
      assert.deepEqual(JSON.parse(put.body), { name: "Europe", exit: { kind: "group", group: "all-aurora" } })
      await tile(page, "Europe").waitFor()
    } finally {
      await close()
    }
  })

  test("17. a slot leaving its server says when it closes, and a down one says why", async () => {
    daemon.set({ slots: new Map([["eu", { ...daemon.slot("eu"), leavingBy: new Date(Date.now() + 20_000).toISOString() }]]) })
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText(/^Switching by /).waitFor()
      withUnifi("eu")
      daemon.set({ slots: new Map([["eu", { ...daemon.slot("eu"), leavingBy: null, status: "failed", downReason: "confirmedDead", appliedServer: null, server: null }]]) })
      await page.reload()
      const reason = tile(page, "EU").getByText("All servers offline", { exact: true })
      await reason.waitFor()
      assert.match(await reason.getAttribute("class"), /text-destructive/, "the reason is a status line, not muted words")
      assert.doesNotMatch(await dot(tile(page, "EU"), "Failed").getAttribute("class"), /bg-success/, "a failed slot never looks like it serves")
    } finally {
      await close()
    }
  })

  /** Holds the page's next write of the slot until `release`, so a check sees the tile while it is out. */
  const holdWrite = async (page, answer = (r) => r.continue()) => {
    let release
    const held = new Promise((done) => { release = done })
    await page.route("**/api/slots/eu", async (r) => { await held; await answer(r) }, { times: 1 })
    return () => release()
  }
  /** Picks an Exit by its menu value, `server:<id>` or `source:<id>`. */
  const pickExit = async (page, value) => {
    await page.getByRole("combobox", { name: "Exit of EU" }).click()
    await page.getByRole("dialog").locator(`[data-value="${value}"]`).click()
  }
  /** The tile's last line: what it shows, and in brackets the sentence a waiting line says to a screen reader and on its card. */
  const lineOf = async (box) => {
    const pending = box.locator("[data-slot=slot-line] [role=status] button")
    if ((await pending.count()) === 0) return (await box.innerText()).split("\n").at(-1)
    const shown = (await pending.innerText()).trim()
    return `${shown ? `${shown} ` : ""}(${await pending.getAttribute("aria-label")})`
  }
  const lineIs = (box, want) => until(async () => (await lineOf(box)) === want, want)

  test("17. a chosen server waits from the click, then names it until the runtime shows it", async () => {
    daemon.set({ holdReadback: true })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("15 / 21 ms").waitFor()
      const release = await holdWrite(page)
      await pickExit(page, "server:l-fra")
      await lineIs(box, "(Switching…)")
      assert.equal(requests("PUT", "api/slots/eu").length, 0, "before the daemon has the write")
      assert.equal(await box.locator("[data-slot=slot-line] [data-slot=pending-value]").count(), 1, "a skeleton where the server will stand")
      release()
      await until(() => daemon.slot("eu").pending, "the daemon's move")
      await readsAnswered(page, "api/state")
      assert.equal(await lineOf(box), "Frankfurt (Switching to Frankfurt)", "the old server is never shown once the move is asked for")
      daemon.readback("eu")
      await box.getByText("9 / 13 ms").waitFor()
      assert.doesNotMatch(await box.innerText(), /Switching|Amsterdam/)
    } finally {
      await close()
    }
  })

  test("17. a chosen source waits with no name until the daemon names a server, then that server", async () => {
    daemon.set({ holdReadback: true })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("15 / 21 ms").waitFor()
      const release = await holdWrite(page)
      await pickExit(page, "source:aurora")
      await lineIs(box, "(Switching…)")
      release()
      await lineIs(box, "Amsterdam (Switching to Amsterdam)")
      daemon.readback("eu")
      await box.getByText("15 / 21 ms").waitFor()
      assert.equal(await box.locator("[role=status]").count(), 0, "settled, nothing waits")
    } finally {
      await close()
    }
  })

  test("17. a slot switching to a server while the server list's read has failed says Switching on its line and in its card, never Unknown server", async () => {
    const eu = slotView("eu", 3, { name: "EU", exit: { kind: "server", server: "l-ams" } })
    daemon.set({ slots: new Map([["eu", { ...eu, appliedServer: "l-fra", runtime: { ...eu.runtime, applied: { kind: "up", server: "l-fra" } } }]]), readFails: { "api/servers": 10 } })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await page.getByRole("combobox", { name: "Exit of EU" }).locator("svg.lucide-circle-x").waitFor()
      await lineIs(box, "(Switching)")
      await box.locator("[data-slot=slot-line] [role=status] button").click()
      assert.equal((await card(page).innerText()).trim(), "Switching", "its card says the same")
      assert.doesNotMatch(await box.innerText(), /Unknown server/)
    } finally {
      await close()
    }
  })

  test("17. a write whose answer is lost names the server once a read shows it landed", async () => {
    daemon.set({ holdReadback: true })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("15 / 21 ms").waitFor()
      const release = await holdWrite(page, async (r) => { await r.fetch(); await r.abort("connectionreset") })
      await pickExit(page, "server:l-fra")
      release()
      await until(() => daemon.slot("eu").pending, "the daemon's move")
      await readsAnswered(page, "api/state")
      await lineIs(box, "Frankfurt (Switching to Frankfurt)")
      daemon.readback("eu")
      await box.getByText("9 / 13 ms").waitFor()
    } finally {
      await close()
    }
  })

  test("17. while an Exit pick is out the select keeps it at full strength and takes no other pick, the line under it is Pending, and the pick is named once the daemon has it", async () => {
    daemon.set({ holdReadback: true })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("15 / 21 ms").waitFor()
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/slots/eu", async (r) => { const response = await r.fetch(); await held; await r.fulfill({ response }).catch(() => {}) })
      await pickExit(page, "server:l-fra")
      await until(() => daemon.slot("eu").version === 4, "Frankfurt landed, its answer held")
      await lineIs(box, "(Switching…)")
      const select = page.getByRole("combobox", { name: "Exit of EU" })
      assert.equal(await shownWords(select), "Frankfurt", "the select holds the pick")
      assert.equal(await select.getAttribute("aria-disabled"), "true", "and takes no other")
      assert.equal(await select.evaluate((el) => getComputedStyle(el).opacity), "1", "at full strength: the line is what waits")
      await select.click({ force: true })
      await page.waitForTimeout(200)
      assert.equal(await page.getByRole("dialog").count(), 0, "the menu doesn't open")
      release()
      await lineIs(box, "Frankfurt (Switching to Frankfurt)")
      assert.equal(await select.getAttribute("aria-disabled"), null, "answered, it takes a pick again")
      assert.ok(requests("PUT", "api/slots/eu").every((r) => r.idempotencyKey === requests("PUT", "api/slots/eu")[0].idempotencyKey), "one write, under one key")
      daemon.readback("eu")
      await box.getByText("9 / 13 ms").waitFor()
      assert.equal(await toast(page, "").count(), 0, "an answered pick says nothing")
    } finally {
      await close()
    }
  })

  for (const engine of ENGINES) {
    test(`17. in ${engine}, from a pick the Exit menu takes no other: Direct by keys, and once it is answered Home, ArrowDown and Enter as the menu fades, writes Direct alone`, async () => {
      const { page, close } = await open("", { engine })
      try {
        await page.getByRole("combobox", { name: "Exit of EU" }).focus()
        await page.keyboard.press("Enter")
        await page.getByRole("dialog").locator("[cmdk-input]").waitFor()
        await page.keyboard.press("Home")
        await page.keyboard.press("Enter")
        await freeze(page)
        await until(() => requests("PUT", "api/slots/eu").some((r) => r.status === 200), "Direct answered")
        const put = daemon.log().findIndex((r) => r.method === "PUT")
        await until(() => daemon.log().slice(put + 1).some((r) => r.method === "GET" && r.path.endsWith("/api/state") && r.status === 200), "a read of the state after Direct's answer")
        await page.waitForTimeout(200)
        assert.equal(await fading(page).count(), 1, "the menu still on screen, fading")
        await page.keyboard.press("Home")
        await page.keyboard.press("ArrowDown")
        await page.keyboard.press("Enter")
        await page.waitForTimeout(300)
        assert.deepEqual(requests("PUT", "api/slots/eu").map((r) => JSON.parse(r.body).exit), [{ kind: "direct" }], "Direct alone is written")
        await thaw(page)
        await page.locator("[data-radix-popper-content-wrapper]").filter({ has: page.locator("[cmdk-list]") }).waitFor({ state: "detached" })
      } finally {
        await close()
      }
    })
  }

  test("17. a tile's name and Exit are one write: while either is out no way into the other takes a press, the pencil, a taken name's Rename and Remove included", async () => {
    const holdPut = async (page) => {
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/slots/eu", async (r) => { if (r.request().method() === "PUT") await held; await r.continue().catch(() => {}) })
      return release
    }
    let { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("15 / 21 ms").waitFor()
      const release = await holdPut(page)
      await pickExit(page, "direct")
      await lineIs(box, "(Switching…)")
      const pencil = box.getByRole("button", { name: "Rename EU", exact: true })
      assert.equal(await pencil.isDisabled(), true, "the pencil takes no press while the pick is out")
      await pencil.click({ force: true })
      assert.equal(await box.getByRole("textbox", { name: "Name of EU" }).count(), 0, "so nothing can be typed that would be dropped")
      release()
      await until(() => pencil.isEnabled(), "answered, the pencil takes a press again")
    } finally {
      await close()
    }

    withUnifi("eu", { trouble: "nameTaken" });
    ({ page, close } = await open(""))
    try {
      const box = tile(page, "EU")
      const fix = box.locator("[data-slot=slot-line]").getByRole("button", { name: "Rename", exact: true })
      await fix.click()
      const release = await holdPut(page)
      await box.getByRole("textbox", { name: "Name of EU" }).fill("EU 2")
      await box.getByRole("textbox", { name: "Name of EU" }).press("Enter")
      await box.getByRole("button", { name: "Rename EU", exact: true, disabled: true }).waitFor()
      assert.equal(await fix.isDisabled(), true, "a taken name's Rename takes no press while the rename is out")
      release()
      await until(() => daemon.slot("eu").name === "EU 2", "the rename lands")
    } finally {
      await close()
    }

    daemon.reset()
    withUnifi("eu", { client: "deleted" });
    ({ page, close } = await open(""))
    try {
      const box = tile(page, "EU")
      const remove = box.getByRole("button", { name: "Remove", exact: true })
      await remove.waitFor()
      const release = await holdPut(page)
      await box.getByRole("button", { name: /^Rename/ }).click()
      await box.getByRole("textbox").fill("EU 3")
      await box.getByRole("textbox").press("Enter")
      await box.getByRole("button", { name: /^Rename/, disabled: true }).waitFor()
      assert.equal(await remove.isDisabled(), true, "Remove takes no press while the rename is out")
      assert.equal(await box.getByRole("button", { name: /^Delete / }).isDisabled(), true, "nor the trash")
      assert.equal(await remove.locator("[data-slot=spinner]").count(), 0, "and the rename's Pending stays at the name")
      release()
    } finally {
      await close()
    }
  })

  test("17. an Exit pick out is the tile's line before anything the slot is in, a disabled VPN Client's Enable included, which comes back once it is answered", async () => {
    withUnifi("eu", { client: "disabled" })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("VPN Client disabled", { exact: true }).waitFor()
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/slots/eu", async (r) => { await held; await r.continue().catch(() => {}) })
      await pickExit(page, "direct")
      await lineIs(box, "(Switching…)")
      assert.equal(await box.getByRole("button", { name: "Enable" }).count(), 0, "the pick's Pending stands in the line, not the client's trouble")
      release()
      await box.getByText("VPN Client disabled", { exact: true }).waitFor()
      await box.getByRole("button", { name: "Enable" }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. an Exit another tab sets after the pick landed is what the tile shows, though the server list is still read", async () => {
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("15 / 21 ms").waitFor()
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/servers?*", async (r) => { await held; await r.continue().catch(() => {}) })
      await page.reload()
      await box.waitFor()
      await pickExit(page, "block")
      await until(() => requests("PUT", "api/slots/eu").at(-1)?.status === 200, "the pick answered")
      daemon.changeSlot("eu", { exit: { kind: "direct" } })
      const select = page.getByRole("combobox", { name: "Exit of EU" })
      await until(async () => (await shownWords(select)) === "Direct", "the select shows the other tab's Exit")
      await until(async () => !/Switching/.test(await lineOf(box)), "and the line no longer speaks of the pick")
      release()
    } finally {
      await close()
    }
  })

  test("17. picks on two tiles refused at once each settle their own tile, with a toast each naming the Exit it refused", async () => {
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("15 / 21 ms").waitFor()
      daemon.set({ slots: new Map([...daemon.slots().map((x) => [x.id, x]), ["lab", { ...daemon.slot("eu"), id: "lab", name: "Lab", unifi: { ...daemon.slot("eu").unifi, name: "Lab", interface: "pxa2" } }]]) })
      await page.reload()
      await tile(page, "Lab").waitFor()
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/slots/*", async (r) => { await held; await r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "serverGone", message: "That server is gone." }) }) })
      await pickExit(page, "server:l-fra")
      await page.getByRole("dialog").waitFor({ state: "detached" })
      await page.getByRole("combobox", { name: "Exit of Lab" }).click()
      await page.getByRole("dialog").locator('[data-value="server:l-par"]').click()
      await lineIs(tile(page, "EU"), "(Switching…)")
      await lineIs(tile(page, "Lab"), "(Switching…)")
      release()
      await toast(page, "Frankfurt").waitFor()
      await toast(page, "Paris").waitFor()
      assert.equal(await page.locator("[data-sonner-toast]").filter({ hasText: /\b(EU|Lab)\b/ }).count(), 0, "neither toast names the slot whose pick was refused")
      for (const name of ["EU", "Lab"]) await until(async () => !/Switching/.test(await lineOf(tile(page, name))), `${name}'s line back`)
      assert.equal(await shownWords(page.getByRole("combobox", { name: "Exit of Lab" })), "All Aurora", "each select back to what is stored")
    } finally {
      await close()
    }
  })

  test("17. on a phone the toasts cover nothing they report on, and the board's Add scrolls clear of them; past three they stack", async () => {
    const names = ["Alpha", "Bravo", "Charlie", "Delta"]
    const slots = names.map((name, i) => {
      const v = slotView(name.toLowerCase(), 1, { name, exit: { kind: "group", group: "all-aurora" } })
      return [v.id, { ...v, createdAt: new Date(Date.now() + i * 60_000).toISOString(), unifi: { ...v.unifi, interface: `pxa${i + 2}` } }]
    })
    daemon.set({ slots: new Map([["eu", daemon.slot("eu")], ...slots]) })
    const { page, close } = await open("", { width: 390, clock: true })
    try {
      await tile(page, "Delta").waitFor()
      await page.route("**/api/slots/*", (r) => (r.request().method() === "PUT" ? r.abort() : r.continue()))
      const shown = page.locator("[data-sonner-toast]:not([data-removed=true]):not([data-visible=false])")
      const top = async () => Math.min(...(await shown.evaluateAll((all) => all.map((t) => t.getBoundingClientRect().top))))
      const clear = async (name) => {
        const box = await tile(page, name).boundingBox()
        return box.y + box.height <= await top()
      }
      for (const name of ["Delta", "Charlie"]) {
        await page.getByRole("combobox", { name: `Exit of ${name}` }).click()
        await page.getByRole("dialog").locator('[data-value="direct"]').click()
        await page.getByRole("dialog").waitFor({ state: "detached" })
      }
      await page.clock.fastForward(31_000)
      await toast(page, "Charlie").waitFor()
      await page.waitForTimeout(600)
      assert.ok(await clear("Delta") && await clear("Charlie"), "each box a toast reports on stands clear of the toasts")
      await page.locator("[data-scroller]").evaluate((el) => el.scrollTo({ top: el.scrollHeight }))
      await page.waitForTimeout(300)
      const add = await page.getByRole("button", { name: "Add", exact: true }).last().boundingBox()
      assert.ok(add.y + add.height <= await top(), "the board's Add scrolls clear of the toasts")
      for (const name of ["Bravo", "Alpha"]) {
        await page.getByRole("combobox", { name: `Exit of ${name}` }).click()
        await page.getByRole("dialog").locator('[data-value="direct"]').click()
        await page.getByRole("dialog").waitFor({ state: "detached" })
      }
      await page.clock.fastForward(31_000)
      await toast(page, "Alpha").waitFor()
      await until(async () => (await page.locator("[data-sonner-toast][data-expanded=false]").count()) === 4, "four toasts stack collapsed")
    } finally {
      await close()
    }
  })

  test("17. an Exit pick with no answer for 30 s ends: the tile shows a fresh read, the toast says timed out, and Retry sends the same pick under its key", async () => {
    const { page, close } = await open("", { clock: true })
    try {
      const box = tile(page, "EU")
      await box.getByText("15 / 21 ms").waitFor()
      let down = true
      await page.route("**/api/slots/eu", (r) => (down ? r.abort() : r.continue()))
      await pickExit(page, "server:l-fra")
      await lineIs(box, "(Switching…)")
      await page.clock.fastForward(31_000)
      const said = toast(page, "timed out")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "EU timed out Retry")
      await until(async () => !/Switching/.test(await lineOf(box)), "the tile shows a fresh read")
      assert.equal(await shownWords(page.getByRole("combobox", { name: "Exit of EU" })), "All Aurora")
      down = false
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.slot("eu").exit.server === "l-fra", "Retry lands the pick")
      const keys = new Set(requests("PUT", "api/slots/eu").map((r) => r.idempotencyKey))
      assert.equal(keys.size, 1, "under the key every attempt had")
    } finally {
      await close()
    }
  })

  test("17. a chosen source names no server while the server list is unread and the daemon keeps the old server", async () => {
    const borealis = { ...source(1), id: "borealis", name: "Borealis", firstChoice: "l-osl", order: ["l-osl"] }
    daemon.set({ catalogue: [...daemon.catalogue(), server("l-osl", "Oslo", "NO", 30, { source: "borealis" })], sources: new Map([...daemon.sources().map((x) => [x.id, x]), ["borealis", borealis]]) })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("15 / 21 ms").waitFor()
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/servers?*", async (r) => { await held; await r.continue() })
      await page.reload()
      await box.waitFor()
      await pickExit(page, "source:borealis")
      await until(() => daemon.slot("eu").exit.source === "borealis", "Borealis landed")
      await until(() => requests("PUT", "api/slots/eu").at(-1)?.status === 200, "Borealis answered")
      await readsAnswered(page, "api/state")
      assert.deepEqual([daemon.slot("eu").server, daemon.slot("eu").appliedServer, daemon.slot("eu").pending], ["l-ams", "l-ams", false], "the daemon kept Amsterdam")
      assert.equal(await lineOf(box), "(Loading servers…)", "an unread server isn't taken as Borealis's")
      release()
      await readsAnswered(page, "api/servers")
      await lineIs(box, "(Switching to Borealis)")
      assert.doesNotMatch(await box.innerText(), /Amsterdam/, "Amsterdam read as Aurora's")
    } finally {
      await close()
    }
  })

  test("17. once the server list's read has failed a slot on a server is the muted failed mark on its line and in its Exit, and the Exit menu's servers are the failed read with Retry, never Unknown server or the dash", async () => {
    daemon.set({ slots: new Map([["eu", slotView("eu", 3, { name: "EU", exit: { kind: "server", server: "l-ams" } })]]), readFails: { "api/servers": 10 } })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      const mark = box.locator("[data-slot=slot-line]").getByRole("button", { name: "Server: HTTP 502", exact: true })
      await mark.waitFor()
      assert.equal(await toneOf(mark), "muted", "muted, as every failed read's mark")
      assert.doesNotMatch(await box.innerText(), /Unknown server|—/)
      const exit = page.getByRole("combobox", { name: "Exit of EU" })
      assert.equal(await exit.locator("svg.lucide-circle-x").count(), 1, "the Exit names its server by the failed mark")
      await exit.click()
      const menu = page.getByRole("dialog")
      const failed = menu.getByRole("alert")
      await failed.waitFor()
      assert.equal((await failed.innerText()).replace(/\s+/g, " ").trim(), "HTTP 502 Retry", "the servers' place holds the reason and Retry")
      assert.equal(await menu.locator('[data-value^="reading:"]').count(), 0, "nothing waits once the read has failed")
      daemon.set({ readFails: {} })
      await failed.getByRole("button", { name: "Retry" }).click()
      await menu.locator('[data-value="server:l-ams"]').waitFor()
      await page.keyboard.press("Escape")
      await box.getByText("15 / 21 ms").waitFor()
      assert.equal(await shownWords(exit), "Amsterdam", "Retry reads the list, and the name stands")
    } finally {
      await close()
    }
  })

  test("17. Enter on the Exit menu's Retry for the failed server list reads the list again and picks nothing", async () => {
    daemon.set({ slots: new Map([["eu", slotView("eu", 3, { name: "EU", exit: { kind: "server", server: "l-ams" } })]]), readFails: { "api/servers": 10 } })
    const { page, close } = await open("")
    try {
      const exit = page.getByRole("combobox", { name: "Exit of EU" })
      await exit.locator("svg.lucide-circle-x").waitFor()
      await exit.click()
      const retry = page.getByRole("dialog").getByRole("alert").getByRole("button", { name: "Retry" })
      await retry.waitFor()
      const isFocused = () => retry.evaluate((el) => el === document.activeElement)
      for (let tabs = 0; tabs < 8 && !(await isFocused()); tabs++) await page.keyboard.press("Tab")
      assert.ok(await isFocused(), "Tab reaches Retry")
      daemon.set({ readFails: {} })
      const reads = requests("GET", "api/servers").length
      await page.keyboard.press("Enter")
      await until(() => requests("GET", "api/servers").length === reads + 1, "Enter on Retry reads the server list again")
      await page.getByRole("dialog").locator('[data-value="server:l-ams"]').waitFor()
      assert.equal(daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(BASE + "api/slots/")).length, 0, "no slot write")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-ams" }, "EU keeps its Exit")
    } finally {
      await close()
    }
  })

  test("17. a failed re-read of the server list keeps the servers it had in the Exit menu, its Servers hold the reason and Retry, and a search never says No match for the server the slot uses", async () => {
    daemon.set({ slots: new Map([["eu", slotView("eu", 3, { name: "EU", exit: { kind: "server", server: "l-ams" } })]]) })
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("15 / 21 ms").waitFor()
      daemon.set({ readFails: { "api/servers": 10 } })
      daemon.prependServer()
      await until(() => requests("GET", "api/servers").at(-1)?.status === 502, "the re-read failed")
      daemon.changeSlot("eu", { exit: { kind: "server", server: "l-ber" } })
      const exit = page.getByRole("combobox", { name: "Exit of EU" })
      await exit.locator("svg.lucide-circle-x").waitFor()
      await exit.click()
      const menu = page.getByRole("dialog")
      await menu.locator('[data-value="server:l-ams"]').waitFor()
      const retry = menu.getByRole("alert").getByRole("button", { name: "Retry" })
      assert.equal(await retry.count(), 1, "the failed re-read stands in Servers with Retry")
      await page.keyboard.type("Berlin")
      await page.waitForTimeout(300)
      assert.equal(await menu.getByText("No match").count(), 0, "what the list holds isn't known, so nothing says no server matches")
      assert.equal(await menu.getByRole("option").count(), 0, "the search still filters the servers the list had")
      assert.equal(await retry.count(), 1, "Retry stands whatever the search holds")
    } finally {
      await close()
    }
  })

  /** EU on Amsterdam, the server list read, then its re-read failed, and EU's Exit menu open on the servers the list had. */
  async function menuOverFailedReread(page) {
    await tile(page, "EU").getByText("15 / 21 ms").waitFor()
    daemon.set({ readFails: { "api/servers": 10 } })
    daemon.prependServer()
    await until(() => requests("GET", "api/servers").at(-1)?.status === 502, "the re-read failed")
    await page.getByRole("combobox", { name: "Exit of EU" }).click()
    const menu = page.getByRole("dialog")
    await menu.locator('[data-value="server:l-ams"]').waitFor()
    await menu.getByRole("alert").waitFor()
    return menu
  }
  const focused = (locator) => locator.evaluate((el) => el === document.activeElement)
  const slotWrites = () => daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(BASE + "api/slots/"))
  const onAmsterdam = () => ({ slots: new Map([["eu", slotView("eu", 3, { name: "EU", exit: { kind: "server", server: "l-ams" } })]]) })

  test("17. a failed re-read of the server list draws no column heading over a search that leaves no server, so Enter two Tabs from the search writes no Exit", async () => {
    daemon.set(onAmsterdam())
    const { page, close } = await open("")
    try {
      const menu = await menuOverFailedReread(page)
      await page.keyboard.type("Block")
      await page.waitForTimeout(300)
      assert.equal(await menu.locator("[cmdk-group]:not([hidden]) [cmdk-group-heading]").count(), 0, "no heading stands over no server")
      await page.keyboard.press("Tab")
      await page.keyboard.press("Tab")
      await page.keyboard.press("Enter")
      await page.waitForTimeout(500)
      assert.equal(slotWrites().length, 0, "no slot write")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-ams" }, "EU keeps its Exit")
    } finally {
      await close()
    }
  })

  test("17. Enter on the Stability header one Tab from the Exit menu's search does what a press on it does and writes no Exit", async () => {
    daemon.set(onAmsterdam())
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("15 / 21 ms").waitFor()
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const menu = page.getByRole("dialog")
      await menu.locator('[data-value="server:l-ams"]').waitFor()
      await page.keyboard.press("Tab")
      const stability = menu.locator('[cmdk-group-heading] button[aria-label="Stability"]').first()
      assert.ok(await focused(stability), "one Tab from the search reaches a header's Stability")
      const card = page.locator("[data-radix-popper-content-wrapper]").filter({ hasText: "Starting level" })
      await card.waitFor()
      await page.keyboard.press("Enter")
      await page.waitForTimeout(300)
      assert.equal(slotWrites().length, 0, "no slot write")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-ams" }, "EU keeps its Exit")
      assert.equal(await menu.isVisible(), true, "the menu stays open")
      assert.ok(await focused(stability), "the focus stays on Stability")
      await card.waitFor({ state: "hidden" })
      await page.keyboard.press("Enter")
      await card.waitFor()
    } finally {
      await close()
    }
  })

  test("17. Enter on the Exit menu's Clear search empties the search, gives the keys back to it and writes no Exit", async () => {
    daemon.set(onAmsterdam())
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("15 / 21 ms").waitFor()
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const menu = page.getByRole("dialog")
      const search = menu.locator("[cmdk-input]")
      await page.keyboard.type("Block")
      await page.keyboard.press("Tab")
      assert.ok(await focused(menu.getByRole("button", { name: "Clear search" })), "one Tab from the search reaches Clear search")
      await page.keyboard.press("Enter")
      await page.waitForTimeout(300)
      assert.equal(slotWrites().length, 0, "no slot write")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-ams" }, "EU keeps its Exit")
      assert.equal(await menu.isVisible(), true, "the menu stays open")
      assert.equal(await search.inputValue(), "", "Enter empties the search")
      assert.ok(await focused(search), "the keys are back in the search")
    } finally {
      await close()
    }
  })

  test("17. an arrow from the Exit menu's Retry for the failed server list gives the keys back to the search, so Enter picks the option the arrow highlighted", async () => {
    daemon.set(onAmsterdam())
    const { page, close } = await open("")
    try {
      const menu = await menuOverFailedReread(page)
      await page.keyboard.type("Paris")
      const retry = menu.getByRole("alert").getByRole("button", { name: "Retry" })
      for (let tabs = 0; tabs < 8 && !(await focused(retry)); tabs++) await page.keyboard.press("Tab")
      assert.ok(await focused(retry), "Tab reaches Retry")
      await page.keyboard.press("ArrowDown")
      assert.ok(await focused(menu.locator("[cmdk-input]")), "the arrow gives the keys back to the search")
      assert.equal(await menu.locator('[cmdk-item][aria-selected="true"]').getAttribute("data-value"), "server:l-par", "and highlights Paris")
      await page.keyboard.press("Enter")
      await until(() => slotWrites().length === 1, "Enter picks Paris")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-par" })
    } finally {
      await close()
    }
  })

  for (const [key, words] of [["End", "End"], ["Meta+ArrowDown", "Command+Down"]]) {
    test(`17. ${words} on the Exit menu's Retry gives the keys to the search, which highlights the last server, scrolls it into view and names it its active option, so Enter picks it`, async () => {
      const zurich = Array.from({ length: 30 }, (_, i) => server(`l-z${i}`, `Zurich ${i}`, "CH", 20 + i))
      daemon.set({ ...onAmsterdam(), catalogue: [...daemon.catalogue(), ...zurich] })
      const { page, close } = await open("")
      try {
        const menu = await menuOverFailedReread(page)
        const search = menu.locator("[cmdk-input]")
        const retry = menu.getByRole("alert").getByRole("button", { name: "Retry" })
        const last = menu.locator('[cmdk-item]:not([aria-disabled="true"])').last()
        await page.keyboard.press("Home")
        await page.keyboard.press("ArrowDown")
        assert.equal(await menu.locator('[cmdk-item][aria-selected="true"]').getAttribute("data-value"), "block", "Block is highlighted")
        for (let tabs = 0; tabs < 8 && !(await focused(retry)); tabs++) await page.keyboard.press("Tab")
        assert.ok(await focused(retry), "Tab reaches Retry")
        await page.keyboard.press(key)
        assert.ok(await focused(search), "the keys are back in the search")
        assert.equal(await last.getAttribute("aria-selected"), "true", "the last server is highlighted")
        assert.equal(await search.getAttribute("aria-activedescendant"), await last.getAttribute("id"), "the search names it its active option")
        assert.ok(await last.evaluate((row) => {
          const list = row.closest("[cmdk-list]").getBoundingClientRect(), r = row.getBoundingClientRect()
          return r.top >= list.top && r.bottom <= list.bottom
        }), "it is scrolled into view")
        await page.keyboard.press("Enter")
        await until(() => slotWrites().length === 1, "Enter picks the last server")
        assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-z29" })
      } finally {
        await close()
      }
    })
  }

  for (const [key, words, reached] of [["ArrowUp", "Up, with no row above it, highlights and names no row", null], ["Meta+ArrowUp", "Command+Up highlights and names the first row", "direct"]]) {
    test(`17. on the Exit menu's Retry, above every row, ${words}, and Enter writes the row the search names`, async () => {
      daemon.set(onAmsterdam())
      const { page, close } = await open("")
      try {
        const menu = await menuOverFailedReread(page)
        const retry = menu.getByRole("alert").getByRole("button", { name: "Retry" })
        await page.keyboard.press("Home")
        await page.keyboard.press("ArrowDown")
        assert.deepEqual(await highlightOf(menu), { drawn: ["block"], named: "block" }, "Block is highlighted and named")
        for (let tabs = 0; tabs < 8 && !(await focused(retry)); tabs++) await page.keyboard.press("Tab")
        assert.ok(await focused(retry), "Tab reaches Retry")
        await page.keyboard.press(key)
        assert.ok(await focused(menu.locator("[cmdk-input]")), "the keys are back in the search")
        assert.deepEqual(await highlightOf(menu), { drawn: reached ? [reached] : [], named: reached }, "the search names the row it draws highlighted")
        await page.keyboard.press("Enter")
        if (reached) {
          await until(() => slotWrites().length === 1, "Enter picks the named row")
          assert.deepEqual(daemon.slot("eu").exit, { kind: reached })
        } else {
          await page.waitForTimeout(300)
          assert.equal(slotWrites().length, 0, "no slot write")
          assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-ams" }, "EU keeps its Exit")
        }
      } finally {
        await close()
      }
    })
  }

  test("17. the Exit menu's search names the row it highlights as the menu opens and as typing narrows it, so Enter writes that row", async () => {
    daemon.set(onAmsterdam())
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("15 / 21 ms").waitFor()
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const menu = page.getByRole("dialog")
      await menu.locator('[data-value="server:l-ams"]').waitFor()
      assert.deepEqual(await highlightOf(menu), { drawn: ["direct"], named: "direct" }, "the menu opens on Direct and names it")
      await page.keyboard.type("Block")
      await until(async () => (await highlightOf(menu)).drawn[0] === "block", "the search highlights Block")
      assert.deepEqual(await highlightOf(menu), { drawn: ["block"], named: "block" }, "the search names Block")
      await page.keyboard.press("Enter")
      await until(() => slotWrites().length === 1, "Enter picks Block")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "block" })
    } finally {
      await close()
    }
  })

  test("17. Enter on the Exit menu's Clear search leaves the search naming the row it then highlights, the first, so a fresh Enter writes that row", async () => {
    daemon.set(onAmsterdam())
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("15 / 21 ms").waitFor()
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const menu = page.getByRole("dialog")
      const search = menu.locator("[cmdk-input]")
      await page.keyboard.type("Block")
      await until(async () => (await highlightOf(menu)).drawn[0] === "block", "the search highlights Block")
      await page.keyboard.press("Tab")
      assert.ok(await focused(menu.getByRole("button", { name: "Clear search" })), "one Tab from the search reaches Clear search")
      await page.keyboard.press("Enter")
      await until(async () => (await search.inputValue()) === "" && (await highlightOf(menu)).drawn[0] === "direct", "the emptied search highlights Direct")
      assert.ok(await focused(search), "the keys are back in the search")
      assert.deepEqual(await highlightOf(menu), { drawn: ["direct"], named: "direct" }, "the search names Direct")
      await page.keyboard.press("Enter")
      await until(() => slotWrites().length === 1, "a fresh Enter picks the named row")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "direct" })
    } finally {
      await close()
    }
  })

  const stability = (menu) => menu.locator('[cmdk-group-heading] button[aria-label="Stability"]').first()
  const stabilityCard = (page) => page.locator("[data-radix-popper-content-wrapper]").filter({ hasText: "Starting level" })
  /** Where the keys are: the rows the menu draws highlighted, the row the focused element names its active option, and what it is. */
  const keysOf = (page) => page.evaluate(() => {
    const el = document.activeElement
    const id = el.getAttribute("aria-activedescendant")
    return {
      drawn: [...document.querySelectorAll('[cmdk-item][aria-selected="true"]')].map((row) => row.getAttribute("data-value")),
      named: id ? (document.getElementById(id)?.getAttribute("data-value") ?? `no element ${id}`) : null,
      focus: el.hasAttribute("cmdk-input") ? "search" : el.hasAttribute("cmdk-list") ? "list" : el.hasAttribute("cmdk-root") ? "menu" : (el.getAttribute("aria-label") ?? el.textContent),
    }
  })
  /** Block typed, then Clear search with the keys on it, which empties the search and gives the keys back to it on Direct. */
  async function clearedFromBlock(page, menu) {
    await page.keyboard.type("Block")
    await until(async () => (await highlightOf(menu)).drawn[0] === "block", "the search highlights Block")
    await menu.getByRole("button", { name: "Clear search" }).focus()
    await page.keyboard.press("Enter")
    await until(async () => (await menu.locator("[cmdk-input]").inputValue()) === "" && (await highlightOf(menu)).drawn[0] === "direct", "the emptied search highlights Direct")
  }

  /** A press on what a menu holds that is no control, which picks nothing: on Direct after Clear search, let go over the search, as
   * a press that slid off; on a section's heading, let go over the search; and on the search's frame, around its field. */
  const pressesThatPickNothing = [
    ["Direct let go over the search, after Clear search", async (page, menu) => {
      await clearedFromBlock(page, menu)
      await menu.locator('[data-value="direct"]').hover()
      await page.mouse.down()
      await menu.locator("[cmdk-input]").hover()
      await page.mouse.up()
    }],
    ["the Sources heading let go over the search", async (page, menu) => {
      await menu.locator("[cmdk-group-heading]").getByText("Sources", { exact: true }).hover()
      await page.mouse.down()
      await menu.locator("[cmdk-input]").hover()
      await page.mouse.up()
    }],
    ["the search's frame", async (page, menu) => {
      await menu.locator("[data-slot=command-input-wrapper]").click({ position: { x: 2, y: 1 } })
    }],
  ]
  for (const engine of ENGINES) for (const [where, pressOn] of pressesThatPickNothing) {
    test(`17. a press on ${where} in the Exit menu, in ${engine}, writes nothing and leaves the keys where they were, naming the row drawn highlighted, so Enter writes that row`, async () => {
      daemon.set(onAmsterdam())
      const { page, close } = await open("", { engine })
      try {
        await tile(page, "EU").getByText("15 / 21 ms").waitFor()
        await page.getByRole("combobox", { name: "Exit of EU" }).click()
        const menu = page.getByRole("dialog")
        await menu.locator('[data-value="server:l-ams"]').waitFor()
        await pressOn(page, menu)
        await page.waitForTimeout(300)
        assert.equal(slotWrites().length, 0, "the press writes nothing")
        const keys = await keysOf(page)
        await page.keyboard.press("Enter")
        await until(() => slotWrites().length === 1, "Enter picks a row")
        assert.deepEqual(daemon.slot("eu").exit, { kind: keys.named }, "Enter writes the row the focused element names")
        assert.deepEqual(keys, { drawn: ["direct"], named: "direct", focus: "search" }, "the keys stay in the search, which names the row drawn highlighted")
      } finally {
        await close()
      }
    })
  }

  /** A press on a control in a menu: a click on a section heading's Stability, after Clear search too, where cmdk's own name for its
   * option is Block, and a tap on it on a phone. */
  const pressesOnStability = [
    ["a click on the Sources heading's Stability", {}, (page, menu) => stability(menu).click()],
    ["a click on Stability after Clear search", {}, async (page, menu) => {
      await clearedFromBlock(page, menu)
      await stability(menu).click()
    }],
    ["a tap on Stability on a phone", { width: 390, touch: true }, (page, menu) => stability(menu).tap()],
  ]
  for (const engine of ENGINES) for (const [how, view, pressOn] of pressesOnStability) {
    test(`17. ${how} in the Exit menu, in ${engine}, opens its card and puts the keys on it, so Enter works the card and writes no Exit`, async () => {
      daemon.set(onAmsterdam())
      const { page, close } = await open("", { ...view, engine })
      try {
        await tile(page, "EU").getByText("15 / 21 ms").waitFor()
        await page.getByRole("combobox", { name: "Exit of EU" }).click()
        const menu = page.getByRole("dialog")
        await menu.locator('[data-value="server:l-ams"]').waitFor()
        await pressOn(page, menu)
        await stabilityCard(page).waitFor()
        /* A mouse at rest on Stability opens its card too, once it has rested 300 ms. */
        await page.waitForTimeout(400)
        const keys = await keysOf(page)
        const ring = await stability(menu).evaluate((el) => el.matches(":focus-visible"))
        await page.keyboard.press("Enter")
        await page.waitForTimeout(300)
        assert.equal(slotWrites().length, 0, "no slot write")
        assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-ams" }, "EU keeps its Exit")
        assert.equal(await stabilityCard(page).count(), 0, "Enter closes the card")
        assert.equal(await menu.isVisible(), true, "the menu stays open")
        assert.deepEqual(keys, { drawn: [], named: null, focus: "Stability" }, "the press puts the keys on Stability, the one thing that looks chosen")
        assert.equal(ring, false, "with no focus ring, as a press's own focus has none")
      } finally {
        await close()
      }
    })
  }

  for (const engine of ENGINES) {
    test(`17. a click on the Exit menu's Retry for the failed server list, in ${engine}, reads the list again and leaves the keys in the search, naming no row, so Enter writes no Exit`, async () => {
      daemon.set(onAmsterdam())
      const { page, close } = await open("", { engine })
      try {
        const menu = await menuOverFailedReread(page)
        const reads = requests("GET", "api/servers").length
        await menu.getByRole("alert").getByRole("button", { name: "Retry" }).click()
        await until(() => requests("GET", "api/servers").length > reads, "a click on Retry reads the server list again")
        await page.waitForTimeout(300)
        const keys = await keysOf(page)
        await page.keyboard.press("Enter")
        await page.waitForTimeout(400)
        assert.equal(slotWrites().length, 0, "no slot write")
        assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-ams" }, "EU keeps its Exit")
        assert.deepEqual(keys, { drawn: [], named: null, focus: "search" }, "the keys are in the search, which names no row, as none is drawn highlighted")
      } finally {
        await close()
      }
    })
  }

  test("17. Shift+Tab from the Exit menu's Clear search back to the search it left brings back its highlight, so Enter picks the one match", async () => {
    daemon.set(onAmsterdam())
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("15 / 21 ms").waitFor()
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const menu = page.getByRole("dialog")
      const search = menu.locator("[cmdk-input]")
      await page.keyboard.type("Block")
      await until(async () => (await menu.locator('[data-value="block"]').getAttribute("aria-selected")) === "true", "the search highlights Block")
      await page.keyboard.press("Tab")
      assert.ok(await focused(menu.getByRole("button", { name: "Clear search" })), "one Tab from the search reaches Clear search")
      await page.keyboard.press("Shift+Tab")
      assert.ok(await focused(search), "Shift+Tab is back in the search")
      assert.equal(await search.inputValue(), "Block", "which still holds Block")
      await page.keyboard.press("Enter")
      await until(() => slotWrites().length === 1, "Enter picks Block")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "block" })
    } finally {
      await close()
    }
  })

  test("17. Enter held on the Exit menu's Clear search empties the search once and its repeats pick nothing, while a fresh Enter in the search picks", async () => {
    daemon.set(onAmsterdam())
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("15 / 21 ms").waitFor()
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const menu = page.getByRole("dialog")
      const search = menu.locator("[cmdk-input]")
      await page.keyboard.type("Block")
      await page.keyboard.press("Tab")
      assert.ok(await focused(menu.getByRole("button", { name: "Clear search" })), "one Tab from the search reaches Clear search")
      await page.keyboard.down("Enter")
      await page.keyboard.down("Enter")
      await page.keyboard.down("Enter")
      await page.keyboard.up("Enter")
      await page.waitForTimeout(300)
      assert.equal(slotWrites().length, 0, "no slot write")
      assert.equal(await menu.isVisible(), true, "the menu stays open")
      assert.equal(await search.inputValue(), "", "the search is empty")
      assert.ok(await focused(search), "the keys are back in the search")
      await page.keyboard.press("Enter")
      await until(() => slotWrites().length === 1, "a fresh Enter picks the highlighted option")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "direct" })
    } finally {
      await close()
    }
  })

  const withNothing = () => {
    const among = (values) => ({ not: false, values })
    daemon.set({ ...onAmsterdam(), groups: new Map([["none", groupView("none", 1, { name: "Nothing", members: { sources: among([]), countries: among(["ZZ"]), protocols: among([]), include: "", exclude: "", box: null } })]]) })
  }
  /** EU's Exit menu open with the focus on the empty group's Open link, as a press on it that slid off leaves it. */
  async function focusOnOpen(page) {
    await tile(page, "EU").getByText("15 / 21 ms").waitFor()
    await page.getByRole("combobox", { name: "Exit of EU" }).click()
    const menu = page.getByRole("dialog")
    const openLink = menu.locator('[data-value="group:none"]').getByRole("link", { name: "Open" })
    await openLink.focus()
    assert.ok(await focused(openLink), "the focus is on Open")
    return menu
  }

  test("17. Enter on a focused Open link in the Exit menu follows it once and picks nothing, whatever row the pointer highlights", async () => {
    withNothing()
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("15 / 21 ms").waitFor()
      await page.evaluate(() => {
        const push = history.pushState.bind(history)
        window.pushes = 0
        history.pushState = (...args) => { window.pushes++; return push(...args) }
      })
      const menu = await focusOnOpen(page)
      await menu.locator('[data-value="direct"]').hover()
      await page.keyboard.press("Enter")
      await page.waitForURL(/\/groups\/none$/)
      await page.waitForTimeout(300)
      assert.equal(await page.evaluate(() => window.pushes), 1, "one navigation")
      assert.equal(slotWrites().length, 0, "no slot write")
      assert.deepEqual(daemon.slot("eu").exit, { kind: "server", server: "l-ams" }, "EU keeps its Exit")
    } finally {
      await close()
    }
  })

  test("17. Home on a focused Open link in the Exit menu gives the keys back to the search and highlights the first option", async () => {
    withNothing()
    const { page, close } = await open("")
    try {
      const menu = await focusOnOpen(page)
      await page.keyboard.press("Home")
      assert.ok(await focused(menu.locator("[cmdk-input]")), "the keys are back in the search")
      assert.equal(await menu.locator('[cmdk-item][aria-selected="true"]').getAttribute("data-value"), "direct", "Direct is highlighted")
    } finally {
      await close()
    }
  })

  test("17. a Retry of the failed server list pressed from the keys keeps the keys in the Exit menu while it is out and once it lands", async () => {
    daemon.set({ ...onAmsterdam(), readFails: { "api/servers": 10 } })
    const { page, close } = await open("")
    try {
      const exit = page.getByRole("combobox", { name: "Exit of EU" })
      await exit.locator("svg.lucide-circle-x").waitFor()
      await exit.click()
      const menu = page.getByRole("dialog")
      const retry = menu.getByRole("alert").getByRole("button", { name: "Retry" })
      await retry.waitFor()
      /* The menu's own readers retry the failed read as they mount. */
      await readsAnswered(page, "api/servers")
      for (let tabs = 0; tabs < 8 && !(await focused(retry)); tabs++) await page.keyboard.press("Tab")
      assert.ok(await focused(retry), "Tab reaches Retry")
      let release
      const held = new Promise((done) => { release = done })
      let out = false
      await page.route("**/api/servers?*", async (r) => { out = true; await held; await r.continue() })
      daemon.set({ readFails: {} })
      await page.keyboard.press("Enter")
      await until(() => out, "Retry's read is out")
      const search = menu.locator("[cmdk-input]")
      await until(() => focused(search), "the keys back in the search while Retry waits")
      release()
      await menu.locator('[data-value="server:l-ams"]').waitFor()
      assert.ok(await focused(search), "and once its read lands")
      await page.keyboard.press("ArrowDown")
      const picked = await menu.locator('[cmdk-item][aria-selected="true"]').getAttribute("data-value")
      assert.ok(picked === "direct" || picked === "block", `an arrow highlights an option (${picked})`)
      await page.keyboard.press("Enter")
      await until(() => slotWrites().length === 1, "Enter picks it")
      assert.deepEqual(daemon.slot("eu").exit, { kind: picked })
    } finally {
      await close()
    }
  })

  test("17. the Exit menu's failed-read line and its Retry are named for the server list, first failure or re-read, searched or not", async () => {
    const line = (menu) => menu.getByRole("group", { name: "Server list", exact: true })
    const named = async (menu, when) => {
      assert.equal((await line(menu).getByRole("alert").innerText()).replace(/\s+/g, " ").trim(), "HTTP 502 Retry", `${when}: the line is in the group named Server list`)
      assert.equal(await line(menu).getByRole("button", { name: "Retry", exact: true }).count(), 1, `${when}: and its Retry`)
    }
    daemon.set({ ...onAmsterdam(), readFails: { "api/servers": 10 } })
    let { page, close } = await open("")
    try {
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const menu = page.getByRole("dialog")
      await menu.getByRole("alert").waitFor()
      await named(menu, "first failure")
      await page.keyboard.type("Berlin")
      await named(menu, "first failure, searched")
    } finally {
      await close()
    }
    daemon.set({ ...onAmsterdam(), readFails: {} });
    ({ page, close } = await open(""))
    try {
      const menu = await menuOverFailedReread(page)
      await named(menu, "re-read")
    } finally {
      await close()
    }
  })

  test("17. a failed re-read keeps the servers it had in the Exit menu's group named Servers, searched or not", async () => {
    daemon.set(onAmsterdam())
    const { page, close } = await open("")
    try {
      const menu = await menuOverFailedReread(page)
      const servers = menu.getByRole("group", { name: "Servers Ping Stability", exact: true })
      assert.equal(await servers.locator('[data-value="server:l-ams"]').count(), 1, "Amsterdam is in the Servers group")
      assert.equal(await servers.getByRole("option").count(), await menu.locator('[data-value^="server:"]').count(), "every server is")
      await page.keyboard.type("Amsterdam")
      await page.waitForTimeout(300)
      assert.equal(await menu.getByRole("option").count(), 1, "the search leaves Amsterdam")
      assert.equal(await servers.getByRole("option").count(), 1, "in the Servers group")
    } finally {
      await close()
    }
  })

  test("17. a Retry of the failed server list out while the board mounts: the board and the list both show the failed read until the Retry lands", async () => {
    daemon.set({ slots: new Map([["eu", slotView("eu", 3, { name: "EU", exit: { kind: "server", server: "l-ams" } })]]), readFails: { "api/servers": 10 } })
    const { page, close } = await open("servers")
    try {
      const retry = page.getByRole("main").getByRole("button", { name: "Retry" })
      await retry.waitFor()
      let release
      const held = new Promise((done) => { release = done })
      let out = false
      await page.route("**/api/servers?*", async (r) => { out = true; await held; await r.continue() })
      daemon.set({ readFails: {} })
      await retry.click()
      await until(() => out, "Retry's read is out")
      await nav(page).getByRole("link", { name: "Dashboard" }).click()
      const box = tile(page, "EU")
      await box.waitFor()
      await page.waitForTimeout(300)
      assert.equal(await nav(page).getByRole("button", { name: "All servers: HTTP 502", exact: true }).count(), 1, "the list shows the failed read")
      const exit = page.getByRole("combobox", { name: "Exit of EU" })
      assert.equal(await exit.locator("svg.lucide-circle-x").count(), 1, "the Exit shows the same failed read")
      assert.equal(await exit.locator("[data-slot=skeleton]").count(), 0, "and waits for nothing")
      assert.equal(await box.locator("[data-slot=slot-line]").getByRole("button", { name: "Server: HTTP 502", exact: true }).count(), 1, "the line is the failed mark")
      assert.equal(await box.getByRole("status", { name: "Loading servers…" }).count(), 0, "never Loading servers…")
      release()
      await box.getByText("15 / 21 ms").waitFor()
      assert.equal(await nav(page).getByRole("button", { name: "All servers: HTTP 502", exact: true }).count(), 0, "a read that lands clears the failure")
    } finally {
      await close()
    }
  })

  test("17. a refused Exit write gives the tile's line back", async () => {
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await box.getByText("15 / 21 ms").waitFor()
      const before = await lineOf(box)
      const release = await holdWrite(page, (r) => r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "serverGone", message: "That server is gone." }) }))
      await pickExit(page, "server:l-fra")
      await lineIs(box, "(Switching…)")
      release()
      await toast(page, "server gone").waitFor()
      await until(async () => (await lineOf(box)) === before, `the line back to ${before}`)
      assert.equal(daemon.slot("eu").exit.kind, "group")
    } finally {
      await close()
    }
  })

  test("17. a change the runtime hasn't read back is the tile's Pending line, its server muted and the spinner, its sentence in the line's name, and no word on the page", async () => {
    const eu = daemon.slot("eu")
    daemon.set({ slots: new Map([["eu", { ...eu, pending: true }]]) })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await lineIs(box, `Amsterdam (Switching to Amsterdam)`)
      const line = box.locator("[data-slot=slot-line]")
      assert.equal(await line.locator("[data-slot=spinner]").count(), 1, "the spinner where the ping goes")
      assert.doesNotMatch(await line.innerText(), /Applying|Switching/, "the sentence is the line's name and card, not words on the page")
    } finally {
      await close()
    }
  })

  test("17. a slot waiting for its first server waits, saying Checking servers…, with a quiet dot", async () => {
    daemon.set({ slots: new Map([["eu", { ...daemon.slot("eu"), status: "checking", downReason: null, server: null, appliedServer: null }]]) })
    const { page, close } = await open("")
    try {
      const box = tile(page, "EU")
      await lineIs(box, "(Checking servers…)")
      assert.doesNotMatch(await box.locator("[data-slot=slot-line]").innerHTML(), /text-(warning|destructive)/, "nothing is wrong yet")
      const colour = await dot(box, "Checking").getAttribute("class")
      assert.match(colour, /bg-muted-foreground\/40/)
      assert.match(colour, /animate-pulse/, "it is under way")
      assert.doesNotMatch(colour, /bg-(warning|destructive|success)/)
      assert.doesNotMatch(await box.innerText(), /No available server|Failed/)
    } finally {
      await close()
    }
  })

  test("17. removing a slot whose VPN Client UniFi deleted is Go's call, and its refusal says why", async () => {
    withUnifi("eu", { client: "deleted" })
    const { page, close } = await open("")
    try {
      await tile(page, "EU").getByText("Deleted", { exact: true }).waitFor()
      await page.route("**/api/slots/eu", (r) => r.request().method() === "DELETE" ? r.fulfill({ status: 409, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "referenced", reason: "routed", message: "UniFi routes still point at this slot." }) }) : r.continue())
      await tile(page, "EU").getByRole("button", { name: "Remove" }).click()
      await toast(page, "still routed").waitFor()
      assert.ok(daemon.slot("eu"))
    } finally {
      await close()
    }
  })
})

/** A source in each state its heading draws, troubledSources', with the list's group of 8 failovers. */
function seedTroubled(at = Date.now()) {
  const t = troubledSources(at)
  const rule = { sources: { not: false, values: ["soon"] }, countries: { not: false, values: [] }, protocols: { not: false, values: [] }, include: "", exclude: "", box: null }
  daemon.set({
    catalogue: t.servers, sources: new Map(t.sources.map((x) => [x.id, x])), refreshes: t.refreshes, analytics: t.analytics, slots: new Map(),
    groups: new Map([["shaky", { ...groupView("shaky", 1, { name: "Shaky", members: rule }, [], t.servers), moves7d: 8 }]]),
  })
  return t
}

const headingOf = (page) => page.locator("[data-slot=source-heading]")
/** The words an element shows, in order: no screen reader's words, nothing hidden at this width. */
const shownWords = (locator) => locator.evaluate((el) => {
  const words = []
  const walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT)
  for (let n = walk.nextNode(); n; n = walk.nextNode()) {
    const host = n.parentElement
    if (n.textContent.trim() && !host.closest(".sr-only") && host.checkVisibility()) words.push(n.textContent.trim())
  }
  return words.join(" ").replace(/\s+/g, " ")
})
/** Whether an element's text is in the warning or destructive token, muted, or plain. */
const toneOf = (locator) => locator.evaluate((el) => {
  const token = (name) => { const probe = document.createElement("span"); probe.style.color = `var(${name})`; document.body.append(probe); const c = getComputedStyle(probe).color; probe.remove(); return c }
  const colour = getComputedStyle(el).color
  return colour === token("--warning") ? "warn" : colour === token("--destructive") ? "bad" : colour === token("--muted-foreground") ? "muted" : "plain"
})
/** The card a fact, a button or a dot opens. */
const card = (page) => page.locator("[data-radix-popper-content-wrapper]").last()

/** A source's week, whose name is its sentence: a button. */
const weekButton = (page) => headingOf(page).getByLabel(/^(No failovers|\d+ failovers?) (today|in the last)/)
/** A chart's readout in `scope` as it reads: its window, then each key's words that are seen. */
const readoutOf = async (scope) => (await scope.locator("[data-slot=chart-readout]").innerText()).replace(/\s+/g, " ").trim()
/** Rests the pointer on the week card's line, at its `failover`th dashed line or at `x` px of its 288, and gives what the readout
 * then reads. */
async function weekPlace(page, at) {
  const line = card(page).locator("[data-slot=chart] svg").first()
  await line.waitFor()
  const box = await line.boundingBox()
  const x = typeof at === "number" ? at : Number(await card(page).locator("[data-slot=week-failover]").nth(at.failover).getAttribute("x1"))
  await page.mouse.move(box.x + (x * box.width) / 288, box.y + 6)
  await until(async () => (await card(page).locator("[data-slot=chart-window]").innerText()) !== "7d", "the readout reads the place")
  await page.waitForTimeout(50)
  return readoutOf(card(page))
}
/** How a chart's readout in `scope`, a card's wrapper or a dialog, stands: from each key's last name to its value, from p50's name
 * to p95's key, how many lines the legend takes, each key's left edge, how far it reaches past the content's right edge, the
 * plot's top, the tone of each dash in it and whether any stands outside a value. */
const readoutBoxes = (scope) => scope.evaluate((el) => {
  const box = (n) => n.getBoundingClientRect()
  const legend = el.querySelector("[data-slot=chart-legend]")
  const keys = [...legend.children].filter((k) => k.dataset.slot === "chart-key")
  const names = (k) => [...k.querySelectorAll("[data-slot=chart-name]")]
  const ping = legend.querySelector("[data-key=ping]")
  const frame = el.matches("[data-slot=dialog-content]") ? el : el.firstElementChild
  const edge = box(frame).right - parseFloat(getComputedStyle(frame).paddingRight)
  const dashes = [...legend.querySelectorAll("*")].filter((n) => [...n.childNodes].some((c) => c.nodeType === 3 && c.textContent.includes("—")))
  const muted = getComputedStyle(legend.querySelector("[data-slot=chart-name]")).color
  return {
    gaps: keys.map((k) => Math.round(box(k.querySelector("[data-slot=chart-value]")).left - box(names(k).at(-1)).right)),
    marks: ping ? names(ping).slice(1).map((n) => { const mark = n.previousElementSibling; return { own: Math.round(box(n).left - box(mark).right), before: Math.round(box(mark).left - box(n.parentElement.previousElementSibling ?? names(ping)[0]).right) } }) : null,
    lines: new Set([...legend.querySelectorAll("[data-slot=chart-name], [data-slot=chart-value]")].map((n) => Math.round(box(n).top))).size,
    lefts: keys.map((k) => Math.round(box(k).left)),
    past: Math.round(Math.max(...keys.map((k) => box(k).right)) - edge),
    top: Math.round(box(el.querySelector("[data-slot=chart]")).top),
    dashes: dashes.map((d) => (getComputedStyle(d).color === muted ? "muted" : "plain")),
    bare: dashes.filter((d) => !d.closest("[data-slot=chart-value]")).length,
  }
})
/** Each row a card or a popover in `scope` draws: its name's right edge and its value's left, and the row's words. */
const rowsOf = (scope) => scope.evaluate((el) => [...el.querySelectorAll("[data-slot=row]")]
  .map((row) => { const [name, value] = row.children; return { words: `${name.textContent} ${value.textContent}`.trim(), name: name.getBoundingClientRect().right, value: value.getBoundingClientRect().left, nameLeft: name.getBoundingClientRect().left } }))
/** The drawing's evening, Tuesday 17 November 2026 at 20:30 in Moscow, and a page opened then and there with its clock stopped. */
const EVENING = Date.parse("2026-11-17T20:30:00+03:00")
/** Runs `run` with this process in Moscow, as the page's zone is, and gives the process its own zone back after. */
async function inMoscow(run) {
  const zone = process.env.TZ
  process.env.TZ = "Europe/Moscow"
  try {
    return await run()
  } finally {
    if (zone === undefined) delete process.env.TZ
    else process.env.TZ = zone
  }
}
async function openAtEvening(path, width) {
  const context = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: "dark", timezoneId: "Europe/Moscow" })
  await context.addInitScript(() => localStorage.setItem("theme", "dark"))
  await context.clock.setFixedTime(EVENING)
  const page = await context.newPage()
  page.setDefaultTimeout(10_000)
  await page.goto(daemon.url + path)
  return { page, close: () => context.close() }
}
/** Refresh, whose name is what a screen reader hears. */
const refreshButton = (page) => headingOf(page).getByRole("button", { name: /^Refresh/ })
/** A failing refresh's card as it reads: the reason, then each row, its name and its value. */
const failureOf = async (why) => {
  await why.locator("[data-slot=rows]").waitFor()
  const reason = (await why.innerText()).split("\n")[0]
  const rows = await why.locator("[data-slot=row]").evaluateAll((all) => {
    const shown = (c) => [...c.childNodes].map((n) => (n.nodeType === Node.TEXT_NODE ? n.textContent : n.classList.contains("sr-only") ? "" : shown(n))).join("")
    return all.map((row) => [...row.children].map((c) => shown(c).trim()).join(" "))
  })
  return [reason, ...rows].join(" | ")
}

/** Each state as its heading reads after the name, its dot and its toned words, and its list entry. */
const STATES = {
  steady: { dot: "Online", words: "Expires in 40d 80.0 GB 10d 7d 24m ago Maintenance on Sunday night, 02:00 to 04:00 UTC. Servers in Frankfurt may drop for a few minutes.",
    tones: { "Expires in": "muted", "Expires in 40d": "plain", "80.0 GB": "plain", "10d": "plain", "7d": "muted", "24m ago": "plain" }, list: "3" },
  soon: { dot: "Unstable", words: "Expires in 3d 5.0 GB 7d 9 6h ago",
    tones: { "Expires in 3d": "warn", "5.0 GB": "warn", "7d": "muted", "9": "warn", "6h ago": "warn" }, listTones: { "5.0 GB left": "warn", "9 failovers in 7d": "warn", "Refresh failing": "warn" }, list: "3 Expires in 3d · 5.0 GB left · 9 failovers in 7d · Refresh failing" },
  expired: { dot: "Offline", words: "No server up Expired 2d ago 7d 25 24m ago",
    tones: { "No server up": "bad", "Expired 2d ago": "bad", "25": "bad" }, listTones: { "25 failovers in 7d": "bad" }, list: "3 No server up · Expired 2d ago · 25 failovers in 7d" },
  limit: { dot: "Offline", words: "Expires in 30d 0 GB 6d 7d 6h ago",
    tones: { "0 GB": "bad", "6d": "plain", "7d": "muted", "6h ago": "warn" }, listTones: { "0 GB left": "bad", "Refresh failing": "warn" }, list: "3 0 GB left · Refresh failing" },
  stale: { dot: "Online", words: "Expires in 40d — GB 7d 24m ago", tones: { "— GB": "muted", "7d": "muted" }, list: "3" },
  unlimited: { dot: "Online", words: "7d 24m ago", tones: { "7d": "muted" }, list: "3" },
  unreported: { dot: "Online", words: "7d 24m ago", tones: { "7d": "muted" }, list: "3" },
  new: { dot: "Refreshing…", week: false, words: "", tones: {}, list: "" },
  broken: { dot: "Offline", week: false, words: "", tones: {}, listTones: { "No servers": "bad", "Refresh failing": "warn" }, list: "No servers · Refresh failing" },
  pasted: { dot: "Offline", words: "7d", tones: { "7d": "muted" }, listTones: { "No servers": "bad" }, list: "No servers" },
}

describe("switching a group or a source to one of its servers", () => {
  const AURORA = "all-aurora"
  /** EU and Home on the group All Aurora, both on Amsterdam, second in its order; Lab on the source Aurora, on Frankfurt; Office on
   * Paris itself and Kids on Direct. */
  function owners({ order = ["l-fra", "l-ams", "l-par"] } = {}) {
    const at = (id, name, exit, minute, server) => ({ ...slotView(id, 3, { name, exit }), createdAt: new Date(Date.UTC(2026, 10, 1, 0, minute)).toISOString(), server, appliedServer: server, runtime: { ...slotView(id, 3, { name, exit }).runtime, applied: server ? { kind: "up", server } : { kind: "direct" } } })
    const group = { kind: "group", group: AURORA }
    const slots = [
      at("eu", "EU", group, 1, "l-ams"), at("home", "Home", group, 2, "l-ams"), at("lab", "Lab", { kind: "source", source: "aurora" }, 3, "l-fra"),
      at("office", "Office", { kind: "server", server: "l-par" }, 4, "l-par"), { ...at("kids", "Kids", { kind: "direct" }, 5, null), status: "direct" },
    ]
    const aurora = daemon.groups()[0]
    daemon.set({ slots: new Map(slots.map((x) => [x.id, x])), groups: new Map([[AURORA, { ...aurora, order, firstChoice: order[0], usedBy: ["eu", "home"] }]]) })
  }
  const line = (page, slot) => tile(page, slot).getByRole("button", { name: /^Server of / })
  const menu = (page) => page.locator("[data-radix-popper-content-wrapper]").filter({ has: page.locator("[cmdk-list]") })
  const row = (page, server) => menu(page).getByRole("option").filter({ hasText: server })
  const presses = (server) => requests("POST", `api/groups/${AURORA}/preferred/${server}`)
  /** Holds the next press until the returned release lets it through. */
  async function hold(page) {
    let release
    const released = new Promise((r) => { release = r })
    await page.route((url) => url.pathname.includes("/preferred/"), async (route) => { await released; await route.continue().catch(() => {}) }, { times: 1 })
    return () => release()
  }
  /** Holds every attempt of a press from now on until the returned release lets them through, so the page's resends under the
   * press's key wait too. With `answers`, each attempt reaches the daemon first and its answer is what waits. */
  async function holdAll(page, { answers = false } = {}) {
    let release
    const released = new Promise((r) => { release = r })
    await page.route((url) => url.pathname.includes("/preferred/"), async (route) => {
      const response = answers ? await route.fetch().catch(() => null) : null
      await released
      await (response ? route.fulfill({ response }) : route.continue()).catch(() => {})
    })
    return () => release()
  }
  const busy = async (locator) => (await locator.getAttribute("aria-disabled")) === "true"
  /** A held pick's words as a box shows them. */
  const MANUAL = /^Manual override$/
  /** The owner's pick of `server`, ending `hours` from `at`, as the daemon holds it; the view names it while a slot is on it. */
  const prefer = (kind, id, server, hours = 24, at = Date.now()) => {
    const store = kind === "group" ? daemon.groups().find((g) => g.id === id) : daemon.source(id)
    daemon.changeOwner(kind, id, { preferred: server && { server, endsAt: new Date(at + hours * 3600e3).toISOString() }, ...(kind === "group" && { order: store.order }) })
  }
  const words = (page, slot) => tile(page, slot).getByText(MANUAL)
  /** Which boxes say the pick, of every box on the board. */
  const saying = async (page) => {
    const said = []
    for (const slot of ["EU", "Home", "Lab", "Office", "Kids"]) if (await words(page, slot).isVisible()) said.push(slot)
    return said
  }
  const head = (page) => menu(page).locator("[cmdk-group-heading]")
  const clearButton = (page) => menu(page).getByRole("button", { name: /^Clear manual override on / })
  /** The picked row's foot, the line under it that holds the pick's chip. */
  const foot = (page) => menu(page).locator("[data-slot=option-foot]")
  /** The pick's chip, its word, and the one card the word opens. */
  const chip = (page) => menu(page).getByRole("group", { name: "Manual", exact: true })
  const chipWord = (page) => chip(page).getByRole("button", { name: /^Manual override\. Auto-resets / })
  const chipCard = (page) => page.locator("[data-radix-popper-content-wrapper]").filter({ hasText: "Auto-resets" })
  /** What the chip's card says, name and value. */
  const cardSays = (page) => chipCard(page).locator("[data-slot=row] > span").allInnerTexts()
  const unpicks = () => daemon.log().filter((r) => r.method === "DELETE" && r.path.includes("/preferred/"))
  const anyPress = () => daemon.log().filter((r) => r.method === "POST" && r.path.includes("/preferred/"))

  test("17. a box on a group or a source opens its owner's servers from its server line; a box on a server or Direct has a plain line and nothing to open", async () => {
    owners()
    const { page, close } = await open("")
    try {
      await line(page, "EU").waitFor()
      assert.equal(await tile(page, "Lab").getByRole("button", { name: "Server of Aurora: Frankfurt", exact: true }).count(), 1, "a source is an owner as a group is, and the line's name holds what it shows")
      assert.equal(await line(page, "EU").getAttribute("aria-label"), "Server of All Aurora: Amsterdam")
      for (const slot of ["Office", "Kids"]) assert.equal(await line(page, slot).count(), 0, `${slot} has no line to open`)
      assert.equal(await line(page, "EU").locator("svg.lucide-chevron-down").count(), 1, "the line ends in the chevron")
    } finally {
      await close()
    }
  })

  test("17. a press names every slot of its owner before it, lists the owner's servers alone in the daemon's order, and moves every slot of the owner and no other", async () => {
    owners()
    const { page, close } = await open("")
    try {
      await line(page, "EU").click()
      await menu(page).locator("[cmdk-group-heading]").waitFor()
      const listed = await menu(page).getByRole("option").locator("[data-slot=option-name]").allInnerTexts()
      assert.deepEqual(listed, ["Frankfurt", "Amsterdam", "Paris"], "the daemon's order, and no Direct, Block, source or group")
      await row(page, "Amsterdam").getByRole("img", { name: "In use: EU, Home" }).waitFor()
      assert.equal(await tile(page, "EU").getByRole("combobox", { name: "Exit of EU" }).innerText(), "All Aurora")
      await row(page, "Paris").click()
      await menu(page).waitFor({ state: "detached" })
      for (const slot of ["EU", "Home"]) await line(page, slot).getByText("Paris").waitFor()
      assert.equal(presses("l-par").length, 1)
      await tile(page, "Lab").getByText("Frankfurt").waitFor()
      assert.equal(await tile(page, "EU").getByRole("combobox", { name: "Exit of EU" }).innerText(), "All Aurora", "the Exit stays the group")
    } finally {
      await close()
    }
  })

  /** Each look the server lines of `slots` take from now on, as the page draws them: what the line says, its chevrons and its
   * spinners, and the spinners in any open menu. */
  async function watchLines(page, slots) {
    await page.evaluate((names) => {
      const seen = Object.fromEntries(names.map((name) => [name, []]))
      const count = (root, css) => root.querySelectorAll(css).length
      const look = (name) => {
        const at = document.querySelector(`[role=group][aria-label="${name}"] [data-slot=slot-line]`)
        if (!at) return "no line"
        const says = at.querySelector("[data-slot=switch-line]")?.getAttribute("aria-label") ?? at.querySelector("[role=status] button")?.getAttribute("aria-label") ?? at.textContent
        const menus = [...document.querySelectorAll("[data-radix-popper-content-wrapper]")].reduce((n, m) => n + count(m, "[data-slot=spinner]"), 0)
        return `${says} | chevron ${count(at, "svg.lucide-chevron-down")} | spinner ${count(at, "[data-slot=spinner]")} | menu spinner ${menus}`
      }
      const read = () => {
        for (const name of names) {
          const now = look(name)
          if (seen[name].at(-1) !== now) seen[name].push(now)
        }
      }
      read()
      new MutationObserver(read).observe(document.body, { subtree: true, childList: true, attributes: true, characterData: true })
      window.__lines = seen
    }, slots)
    return () => page.evaluate(() => window.__lines)
  }

  for (const first of ["the answer", "a landing"]) {
    test(`17. one press is one wait, ${first} first: the menu closes at the press, and every box of the owner is Switching to the server from there until its slot lands, never its old server and never a second spinner`, async () => {
      owners()
      daemon.set({ holdPreferLanding: true })
      const { page, close } = await open("")
      const release = await (first === "the answer" ? hold(page) : holdAll(page, { answers: true }))
      try {
        await line(page, "EU").waitFor()
        const lines = await watchLines(page, ["EU", "Home"])
        await line(page, "EU").click()
        await row(page, "Paris").click()
        await menu(page).waitFor({ state: "detached", timeout: 1000 })
        if (first === "the answer") assert.equal(presses("l-par").length, 0, "closed while the press is still on its way")
        for (const slot of ["EU", "Home"]) await tile(page, slot).getByRole("button", { name: "Switching to Paris" }).waitFor()
        assert.equal(await line(page, "Home").count(), 0, "the owner's other box takes no press")
        if (first === "a landing") {
          await until(() => presses("l-par").some((r) => r.status === 200), "the daemon took the press")
          daemon.readback("eu")
          await line(page, "EU").getByText("Paris").waitFor()
          assert.equal(await tile(page, "Home").getByRole("button", { name: "Switching to Paris" }).count(), 1, "Home still lands")
          assert.equal(await busy(line(page, "EU")), true, "EU, landed, takes no second press while the write is out")
          await line(page, "EU").click({ force: true })
          await painted(page)
          assert.equal(await menu(page).count(), 0, "and opens no menu")
          release()
          await until(async () => !(await busy(line(page, "EU"))), "the write settled")
          assert.equal(await tile(page, "Home").getByRole("button", { name: "Switching to Paris" }).count(), 1, "Home still lands once the write settled")
        } else {
          release()
          await until(() => presses("l-par").some((r) => r.status === 200), "the press answered")
          const pressed = daemon.log().findIndex((r) => r.method === "POST" && r.path.endsWith("/preferred/l-par"))
          await until(() => daemon.log().slice(pressed + 1).some((r) => r.method === "GET" && r.path.endsWith("/api/state") && r.status === 200), "a read of the state after the press")
          await page.waitForTimeout(200)
          await painted(page)
          for (const slot of ["EU", "Home"]) assert.equal(await tile(page, slot).getByRole("button", { name: "Switching to Paris" }).count(), 1, `${slot} is still Switching to Paris once the pass is answered`)
          daemon.readback("eu")
          await line(page, "EU").getByText("Paris").waitFor()
          assert.equal(await tile(page, "Home").getByRole("button", { name: "Switching to Paris" }).count(), 1, "Home still lands")
        }
        daemon.readback("home")
        await line(page, "Home").getByText("Paris").waitFor()
        const ONE_WAIT = [
          "Server of All Aurora: Amsterdam | chevron 1 | spinner 0 | menu spinner 0",
          "Switching to Paris | chevron 0 | spinner 1 | menu spinner 0",
          "Server of All Aurora: Paris | chevron 1 | spinner 0 | menu spinner 0",
        ]
        assert.deepEqual(await lines(), { EU: ONE_WAIT, Home: ONE_WAIT }, "every look of each box's line from before the press to its landing")
      } finally {
        await close()
      }
    })
  }

  test("17. a box's flag stands where it stood as its line goes from Switching to to the server landed", async () => {
    owners()
    daemon.set({ holdPreferLanding: true })
    const { page, close } = await open("")
    try {
      const flag = async () => {
        const box = await tile(page, "EU").locator("[data-slot=slot-line] img").boundingBox()
        return { x: Math.round(box.x * 2) / 2, y: Math.round(box.y * 2) / 2 }
      }
      await line(page, "EU").click()
      await row(page, "Paris").click()
      await tile(page, "EU").getByRole("button", { name: "Switching to Paris" }).waitFor()
      await until(() => presses("l-par").some((r) => r.status === 200), "the press answered")
      await page.mouse.move(0, 0)
      await painted(page)
      const pending = await flag()
      daemon.readback("eu")
      await line(page, "EU").getByText("Paris").waitFor()
      await painted(page)
      assert.deepEqual(await flag(), pending, "Paris's flag where it stood while EU was Switching to it")
    } finally {
      await close()
    }
  })

  /** The toast that says `server`'s check `words`, "failed" or "not run", over its muted `reason`. */
  const failedToast = (page, server, words = "failed", reason = "No response") => toast(page, server).filter({ hasText: words }).getByText(reason, { exact: true })

  test("17. a failed or unrun check moves nothing: the toast says the server and why, and the menu opened by hand has its row saying Failed or Not run with the reason under its name, and the row takes another press", async () => {
    owners()
    const { page, close } = await open("")
    try {
      for (const [verdict, words, reason] of [["failed", "Failed", "No response"], ["unknown", "Not run", "No route"]]) {
        daemon.set({ preferVerdict: verdict })
        await line(page, "EU").click()
        await row(page, "Paris").click()
        await failedToast(page, "Paris", words.toLowerCase(), reason).waitFor()
        await tile(page, "EU").getByText("Amsterdam").waitFor()
        assert.equal(await opened(page).count(), 0, "no menu opens")
        await settled(page)
        await line(page, "EU").click()
        await row(page, "Paris").getByText(words, { exact: true }).waitFor()
        assert.equal(await menu(page).getByText(reason, { exact: true }).count(), 1, "on its own row alone")
        assert.equal(await busy(row(page, "Paris")), false, "another pick is one row away")
        await page.keyboard.press("Escape")
        await menu(page).waitFor({ state: "detached" })
      }
    } finally {
      await close()
    }
  })

  test("17. on a phone a failed check's reason stands under its row's name, which stays whole, and the rows below move down for it", async () => {
    owners()
    const { page, close } = await open("", { width: 390 })
    try {
      for (const [verdict, words, reason] of [["failed", "Failed", "No response"], ["unknown", "Not run", "No route"]]) {
        daemon.set({ preferVerdict: verdict })
        await line(page, "EU").click()
        await row(page, "Frankfurt").click()
        await failedToast(page, "Frankfurt", words.toLowerCase(), reason).waitFor()
        await settled(page)
        await line(page, "EU").click()
        await row(page, "Frankfurt").getByText(reason, { exact: true }).waitFor()
        await row(page, "Frankfurt").getByText(words, { exact: true }).waitFor()
        const laid = await page.evaluate((reason) => {
          const options = [...document.querySelectorAll("[cmdk-item]")]
          const failed = options.find((o) => o.textContent.includes("Frankfurt"))
          const name = failed.querySelector("[data-slot=option-name]")
          const said = [...failed.querySelectorAll("span")].find((e) => e.textContent === reason).getBoundingClientRect()
          const named = name.getBoundingClientRect()
          const boxes = options.map((o) => o.getBoundingClientRect())
          return {
            whole: name.scrollWidth <= name.clientWidth,
            under: said.top >= named.bottom - 0.5,
            apart: boxes.every((b, k) => k === 0 || b.top >= boxes[k - 1].bottom - 0.5),
            floating: document.querySelectorAll("[data-radix-popper-content-wrapper]").length,
          }
        }, reason)
        assert.deepEqual(laid, { whole: true, under: true, apart: true, floating: 1 }, `${words}: the name whole, the reason under it, no row over another, no card but the menu`)
        await page.keyboard.press("Escape")
        await menu(page).waitFor({ state: "detached" })
      }
    } finally {
      await close()
    }
  })

  /** EU and Home on the source Acme, on the first of its 12 servers. */
  function onAcme() {
    daemon.addAcme()
    const first = daemon.source("acme").order[0]
    const exit = { kind: "source", source: "acme" }
    const at = (id, name, minute) => {
      const view = slotView(id, 3, { name, exit })
      return { ...view, createdAt: new Date(Date.UTC(2026, 10, 1, 0, minute)).toISOString(), server: first, appliedServer: first, runtime: { ...view.runtime, applied: { kind: "up", server: first } } }
    }
    daemon.set({ slots: new Map([at("eu", "EU", 1), at("home", "Home", 2)].map((x) => [x.id, x])) })
  }

  test("17. on a phone the menu shows every server's name and source whole, a long one on a second line, and Ping and Stability stand where a one-line row has them", async () => {
    owners()
    const relabel = { "l-par": { label: "Netherlands · EDGE-STANDBY", name: { remarks: "Netherlands", tag: "EDGE-STANDBY" } }, "l-ams": { label: "Frankfurt · backup", name: { remarks: "Frankfurt", tag: "backup" } } }
    daemon.set({
      catalogue: daemon.catalogue().map((l) => ({ ...l, ...relabel[l.id] })),
      sources: new Map([["aurora", { ...daemon.source("aurora"), name: "Steady" }]]),
    })
    const { page, close } = await open("", { width: 390 })
    try {
      await line(page, "EU").click()
      await row(page, "EDGE-STANDBY").waitFor()
      await until(() => menu(page).evaluate((el) => el.getAnimations({ subtree: true }).length === 0), "the menu to settle")
      const rows = await menu(page).evaluate((el) => [...el.querySelectorAll("[cmdk-item]")].map((o) => {
        const name = o.querySelector("[data-slot=option-name]")
        const words = name.parentElement
        const box = o.getBoundingClientRect()
        const at = (cell) => { const r = cell.getBoundingClientRect(); return [Math.round(r.left), Math.round(r.right), Math.round(r.top - box.top)] }
        const bars = o.querySelector("[data-slot=bars]").parentElement
        return {
          words: words.textContent,
          whole: name.scrollWidth <= name.clientWidth && words.scrollWidth <= words.clientWidth && words.scrollHeight <= words.clientHeight,
          lines: Math.round(words.getBoundingClientRect().height / parseFloat(getComputedStyle(words).lineHeight)),
          cells: [at(bars.previousElementSibling), at(bars)],
        }
      }))
      assert.deepEqual(rows.map((r) => [r.words, r.whole]), [["Frankfurt · Steady", true], ["Frankfurt · backup · Steady", true], ["Netherlands · EDGE-STANDBY · Steady", true]], "every name and source whole")
      assert.ok(rows[2].lines > 1, "the long name takes a second line")
      assert.equal(rows[0].lines, 1, "a short name keeps one line")
      for (const r of rows.slice(1)) assert.deepEqual(r.cells, rows[0].cells, `${r.words}: Ping and Stability where a one-line row has them`)
    } finally {
      await close()
    }
  })

  for (const engine of ENGINES) {
    test(`17. in ${engine}, the keyboard's focus stays on the pressing box's line through a press by Enter, its landing, and a failure the toast says`, async () => {
      owners()
      daemon.set({ holdPreferLanding: true })
      const { page, close } = await open("", { engine })
      try {
        const onEU = () => tile(page, "EU").locator("[data-slot=slot-line]").evaluate((el) => el.contains(document.activeElement))
        const pressByKeys = async (server) => {
          await page.keyboard.press("Enter")
          await menu(page).locator("[cmdk-input]").waitFor()
          await page.keyboard.type(server)
          await page.keyboard.press("Enter")
          await menu(page).waitFor({ state: "detached" })
        }
        await line(page, "EU").focus()
        await pressByKeys("Paris")
        await tile(page, "EU").getByRole("button", { name: "Switching to Paris" }).waitFor()
        await painted(page)
        assert.equal(await onEU(), true, "on EU's line at the press")
        await until(() => presses("l-par").some((r) => r.status === 200), "the press answered")
        daemon.readback("eu")
        daemon.readback("home")
        await line(page, "EU").getByText("Paris").waitFor()
        await painted(page)
        assert.equal(await onEU(), true, "on EU's line once it landed")

        daemon.set({ preferVerdict: "failed" })
        const release = await holdAll(page)
        await pressByKeys("Frankfurt")
        await tile(page, "Home").getByRole("button", { name: "Switching to Frankfurt" }).hover()
        await page.locator("[data-radix-popper-content-wrapper]").filter({ hasText: "Switching to Frankfurt" }).waitFor()
        release()
        await toast(page, "Frankfurt").filter({ hasText: "failed" }).waitFor()
        await painted(page)
        assert.equal(await onEU(), true, "on EU's line once the toast said its failure")
        assert.equal(await line(page, "EU").evaluate((el) => el === document.activeElement), true, "the line itself, so Enter opens its menu")
      } finally {
        await close()
      }
    })
  }

  /* By keys, Paris then Frankfurt as the menu fades; by the pointer, a click on Paris, then one on Frankfurt's row as it fades. */
  for (const [by, verdict] of [["keys", "succeeded"], ["keys", "failed"], ["pointer", "succeeded"]]) {
    test(`17. from a pick its menu takes no other: a second pick by ${by} as it closes sends nothing, and the pick ${verdict === "failed" ? "that fails is still said" : "is the one every box is Switching to"}`, async () => {
      owners()
      daemon.set({ holdPreferLanding: true, preferVerdict: verdict })
      const { page, close } = await open("", { noCheck: true })
      try {
        const release = verdict === "failed" ? await hold(page) : () => {}
        await line(page, "Home").click()
        await menu(page).locator("[cmdk-input]").waitFor()
        const frankfurt = await row(page, "Frankfurt").boundingBox()
        const lines = await watchLines(page, ["EU", "Home"])
        if (by === "keys") {
          await page.keyboard.press("End")
          await page.keyboard.press("Enter")
        } else await row(page, "Paris").click()
        await freeze(page)
        if (verdict === "succeeded") {
          await until(() => presses("l-par").some((r) => r.status === 200), "Paris answered")
          const pressed = daemon.log().findIndex((r) => r.method === "POST" && r.path.endsWith("/preferred/l-par"))
          await until(() => daemon.log().slice(pressed + 1).some((r) => r.method === "GET" && r.path.endsWith("/api/state") && r.status === 200), "a read of the state after Paris's answer")
          await page.waitForTimeout(200)
        }
        assert.equal(await fading(page).count(), 1, "the menu still on screen, fading")
        if (by === "keys") {
          await page.keyboard.press("Home")
          await page.keyboard.press("Enter")
        } else await page.mouse.click(frankfurt.x + frankfurt.width / 2, frankfurt.y + frankfurt.height / 2)
        await painted(page)
        release()
        if (verdict === "failed") {
          await failedToast(page, "Paris").waitFor()
          for (const slot of ["EU", "Home"]) await line(page, slot).getByText("Amsterdam").waitFor()
        } else {
          await page.waitForTimeout(300)
          daemon.readback("eu")
          daemon.readback("home")
          for (const slot of ["EU", "Home"]) await line(page, slot).getByText("Paris").waitFor()
          const seen = await lines()
          for (const slot of ["EU", "Home"]) assert.equal(seen[slot].some((look) => look.includes("Frankfurt")), false, `${slot} never Switching to Frankfurt`)
        }
        assert.deepEqual(daemon.log().filter((r) => r.method !== "GET").map((r) => `${r.method} ${r.path}`), [`POST ${BASE}api/groups/${AURORA}/preferred/l-par`], "Paris alone is sent, and nothing else")
      } finally {
        await close()
      }
    })
  }

  /* By keys or the pointer, answered at once or once its menu has gone. */
  for (const engine of ENGINES) for (const by of ["keys", "pointer"]) for (const late of [false, true]) {
    test(`17. in ${engine}, a failed check pressed by ${by}${late ? " and answered once its menu has gone" : ""} is the toast, the server and its reason, and opens nothing: both boxes are back on the server they never left, and the focus is ${by === "keys" ? "on the pressing line" : "where it was"}`, async () => {
      owners()
      daemon.set({ preferVerdict: "failed" })
      const { page, close } = await open("", { engine })
      try {
        const release = late ? await hold(page) : () => {}
        if (by === "keys") {
          await line(page, "EU").focus()
          await page.keyboard.press("Enter")
          await menu(page).locator("[cmdk-input]").waitFor()
          await page.keyboard.type("Paris")
          await page.keyboard.press("Enter")
        } else {
          await line(page, "EU").click()
          await row(page, "Paris").click()
        }
        await menu(page).waitFor({ state: "detached" })
        const focused = await page.evaluateHandle(() => document.activeElement)
        if (late) {
          await page.waitForTimeout(450)
          release()
        }
        await failedToast(page, "Paris").waitFor()
        for (const slot of ["EU", "Home"]) await line(page, slot).getByText("Amsterdam").waitFor()
        await painted(page)
        await page.waitForTimeout(200)
        assert.equal(await menu(page).count(), 0, "no menu opens")
        assert.equal(await page.evaluate((el) => document.activeElement === el, focused), true, "the focus where the press left it")
        if (by === "keys") assert.equal(await line(page, "EU").evaluate((el) => el === document.activeElement), true, "on EU's line, so Enter opens its menu")
        assert.equal(await line(page, "EU").getByRole("img", { name: /Failed|Not run/ }).count(), 0, "no Test dot beside the ping of the server EU still serves")
        assert.equal(await line(page, "EU").locator("svg.lucide-chevron-down").count(), 1, "the line keeps its chevron, the way to the answer")
        assert.equal(anyPress().length, 1)
      } finally {
        await close()
      }
    })
  }

  for (const engine of ENGINES) for (const by of ["keys", "pointer"]) {
    test(`17. in ${engine}, a check pressed by ${by} that fails while its menu fades is the toast alone: the fading menu draws no new answer`, async () => {
      owners()
      daemon.set({ preferVerdict: "failed" })
      const { page, close } = await open("", { engine })
      try {
        const release = await hold(page)
        if (by === "keys") {
          await line(page, "EU").focus()
          await page.keyboard.press("Enter")
          await menu(page).locator("[cmdk-input]").waitFor()
          await page.keyboard.type("Paris")
          await freezeAsItCloses(page)
          await page.keyboard.press("Enter")
        } else {
          await line(page, "EU").click()
          await row(page, "Paris").waitFor()
          await freezeAsItCloses(page)
          await row(page, "Paris").click()
        }
        await fading(page).waitFor()
        release()
        await failedToast(page, "Paris").waitFor()
        for (const slot of ["EU", "Home"]) await line(page, slot).getByText("Amsterdam").waitFor()
        await painted(page)
        assert.equal(await fading(page).count(), 1, "the menu still on screen, fading")
        assert.equal(await fading(page).getByText(/^(Failed|No response)$/).count(), 0, "the fading menu draws no new answer")
        await thaw(page)
        await menu(page).waitFor({ state: "detached" })
        await line(page, "EU").click()
        await row(page, "Paris").getByText("No response", { exact: true }).waitFor()
      } finally {
        await close()
      }
    })
  }

  for (const engine of ENGINES) for (const how of ["Escape", "a press outside it"]) {
    test(`17. in ${engine}, a server menu closed by ${how} and opened again by hand as it fades has the keys in its search, so the first letters typed filter it`, async () => {
      owners()
      const { page, close } = await open("", { engine })
      try {
        const search = menu(page).locator("[cmdk-input]")
        await line(page, "EU").click()
        await search.waitFor()
        await until(() => menu(page).evaluate((el) => el.getAnimations({ subtree: true }).length === 0), "the menu to settle")
        await freezeAsItCloses(page)
        if (how === "Escape") await page.keyboard.press("Escape")
        else await page.mouse.click(1300, 860)
        await fading(page).waitFor()
        await line(page, "EU").click()
        await opened(page).waitFor()
        await page.keyboard.type("Par")
        assert.equal(await search.inputValue(), "Par", "the letters in the search")
        assert.deepEqual(await menu(page).getByRole("option").locator("[data-slot=option-name]").allInnerTexts(), ["Paris"], "and the list filtered by them")
      } finally {
        await close()
      }
    })
  }

  for (const engine of ENGINES) {
    test(`17. in ${engine}, on a phone a failed check under a still pointer is the toast alone, and Enter then opens the menu by hand and sends nothing`, async () => {
      onAcme()
      daemon.set({ preferVerdict: "failed" })
      const { page, close } = await open("", { width: 390, engine })
      try {
        await line(page, "EU").click()
        await menu(page).locator("[cmdk-item]").last().scrollIntoViewIfNeeded()
        await menu(page).locator("[cmdk-item]").last().click()
        await toast(page, "failed").getByText("No response", { exact: true }).waitFor()
        await painted(page)
        await page.waitForTimeout(300)
        assert.equal(await menu(page).count(), 0, "no menu under the pointer")
        await page.keyboard.press("Enter")
        await menu(page).locator("[cmdk-input]").waitFor()
        await painted(page)
        await page.waitForTimeout(300)
        assert.equal(anyPress().length, 1, "the one press")
      } finally {
        await close()
      }
    })
  }

  test("17. the menu opened by hand after a failed check opens as every menu does, its search empty and its first pickable row highlighted, the failed row saying Failed with its reason, and it picks what the reader picks", async () => {
    owners()
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("")
    try {
      const search = menu(page).locator("[cmdk-input]")
      await line(page, "EU").click()
      await search.fill("Par")
      await row(page, "Paris").click()
      await failedToast(page, "Paris").waitFor()
      await settled(page)
      await line(page, "EU").click()
      await row(page, "Paris").getByText("Failed", { exact: true }).waitFor()
      assert.equal(await row(page, "Paris").getByText("No response", { exact: true }).count(), 1, "the reason under its name")
      assert.equal(await search.inputValue(), "", "the search empty")
      assert.deepEqual(await highlightOf(menu(page)), { drawn: ["server:l-fra"], named: "server:l-fra" }, "the first pickable row highlighted")
      await search.fill("Frankfurt")
      await page.keyboard.press("Escape")
      await menu(page).waitFor({ state: "detached" })
      await line(page, "EU").click()
      await row(page, "Frankfurt").waitFor()
      assert.deepEqual(await highlightOf(menu(page)), { drawn: ["server:l-fra"], named: "server:l-fra" }, "the one match highlighted")
      await page.keyboard.press("Enter")
      await failedToast(page, "Frankfurt").waitFor()
      await settled(page)
      await line(page, "EU").click()
      await row(page, "Paris").click()
      await failedToast(page, "Paris").waitFor()
      assert.deepEqual(anyPress().map((r) => r.path.split("/").at(-1)), ["l-par", "l-fra", "l-par"], "each server the reader picked")
    } finally {
      await close()
    }
  })


  /** The middle of where the open menu's first pickable row covers `under`, and that row's value. */
  const rowOver = async (page, under) => {
    await until(() => menu(page).evaluate((el) => el.getAnimations({ subtree: true }).length === 0), "the menu to settle")
    const box = await under.boundingBox()
    return menu(page).evaluate((root, box) => {
      for (const row of root.querySelectorAll('[cmdk-item]:not([aria-disabled="true"])')) {
        const r = row.getBoundingClientRect()
        const [top, bottom, left, right] = [Math.max(r.top, box.y), Math.min(r.bottom, box.y + box.height), Math.max(r.left, box.x), Math.min(r.right, box.x + box.width)]
        if (bottom - top > 4 && right - left > 4) return { x: (left + right) / 2, y: (top + bottom) / 2, value: row.getAttribute("data-value") }
      }
      return null
    }, box)
  }
  const slotWrites = () => daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(BASE + "api/slots/"))

  for (const engine of ENGINES) {
    test(`17. in ${engine}, on a phone a failed check of a server double clicked where its menu covers Add, answered between the clicks, is the toast alone: the menu fading out takes the second click, and a click once it has gone opens the new slot's card and makes nothing`, async () => {
      onAcme()
      daemon.set({ preferVerdict: "failed" })
      const { page, close } = await open("", { width: 390, engine })
      try {
        const release = await hold(page)
        await line(page, "EU").click()
        const at = await rowOver(page, addSlot(page))
        assert.ok(at, "a server's row over Add")
        await page.mouse.move(at.x, at.y)
        await page.mouse.down({ clickCount: 1 })
        await page.mouse.up({ clickCount: 1 })
        await freeze(page)
        release()
        await failedToast(page, "").waitFor()
        assert.equal(await fading(page).count(), 1, "the menu still on screen, fading")
        await page.mouse.down({ clickCount: 2 })
        await page.mouse.up({ clickCount: 2 })
        await painted(page)
        await page.waitForTimeout(300)
        assert.deepEqual(anyPress().map((r) => `server:${r.path.split("/").at(-1)}`), [at.value], "the server clicked, tested once")
        assert.equal(slotWrites().length, 0, "Add pressed nothing")
        assert.equal(await opened(page).count(), 0, "and no menu opens")
        assert.equal(await line(page, "EU").evaluate((el) => el === document.activeElement), true, "the focus on EU's line")
        await thaw(page)
        await menu(page).waitFor({ state: "detached" })
        await page.mouse.click(at.x, at.y)
        await newSlot(page).waitFor()
        await page.waitForTimeout(300)
        assert.equal(slotWrites().length, 0, "a click once the menu has gone opens the card, and makes nothing")
      } finally {
        await close()
      }
    })
  }

  /** The menu's row over Add, picked, then the same place tapped again `after` ms later and once more: the second tap may open the
   * new slot's card, and neither makes a VPN Client. */
  const strayTaps = async (page, at, after) => {
    await page.mouse.click(at.x, at.y)
    await page.waitForTimeout(after)
    await page.mouse.click(at.x, at.y)
    await page.waitForTimeout(150)
    await page.mouse.click(at.x, at.y)
    await page.waitForTimeout(400)
    assert.equal(creates().length, 0, `taps ${after} ms after the pick and after them made no VPN Client`)
  }
  /** A phone's screen as Safari leaves it under its bars, where the middle of the screen is where Add stands. */
  const PHONE = { width: 390, height: 664 }
  for (const engine of ENGINES) for (const after of [150, 500, 700]) {
    test(`19. in ${engine}, on a phone a server picked where its menu covers Add and the place tapped again ${after} ms later and once more makes no VPN Client`, async () => {
      onAcme()
      const { page, close } = await open("", { ...PHONE, engine })
      try {
        await line(page, "EU").click()
        const at = await rowOver(page, addSlot(page))
        assert.ok(at, "a server's row over Add")
        /* The Test held, as a slow one is, so the board stands as it did under the menu. */
        const release = await hold(page)
        await strayTaps(page, at, after)
        release()
        await until(() => anyPress().length > 0, "the held Test let through")
        await page.waitForTimeout(200)
        assert.deepEqual(anyPress().map((r) => `server:${r.path.split("/").at(-1)}`), [at.value], "the server picked, once")
        assert.equal(creates().length, 0, "and still no VPN Client")
      } finally {
        await close()
      }
    })

    test(`19. in ${engine}, on a phone an exit picked where the Exit menu covers Add and the place tapped again ${after} ms later and once more makes no VPN Client`, async () => {
      onAcme()
      /* EU on Direct, so its source's row is a change. */
      const eu = { ...slotView("eu", 3, { name: "EU", exit: { kind: "direct" } }), createdAt: new Date(Date.UTC(2026, 10, 1, 0, 1)).toISOString() }
      daemon.set({ slots: new Map([["eu", eu], ["home", daemon.slot("home")]]) })
      const { page, close } = await open("", { ...PHONE, engine })
      try {
        await page.getByRole("combobox", { name: "Exit of EU" }).click()
        const at = await rowOver(page, addSlot(page))
        assert.ok(at, "an exit's row over Add")
        await strayTaps(page, at, after)
        assert.equal(slotWrites().filter((r) => r.path.endsWith("/eu")).length, 1, "the exit picked, once")
      } finally {
        await close()
      }
    })
  }

  for (const engine of ENGINES) for (const end of ["failed", "landed"]) {
    test(`17. in ${engine}, Enter held through a pick and its ${end === "failed" ? "failed check" : "landing"} opens nothing, and pressed again it opens the menu`, async () => {
      owners()
      daemon.set(end === "failed" ? { preferVerdict: "failed" } : { holdPreferLanding: true })
      const { page, close } = await open("", { engine })
      try {
        /* Playwright sends every down of a key already down as a repeat. */
        const held = async (ms) => { for (const stop = Date.now() + ms; Date.now() < stop;) { await page.keyboard.down("Enter"); await page.waitForTimeout(30) } }
        await line(page, "EU").focus()
        await page.keyboard.press("Enter")
        await menu(page).locator("[cmdk-input]").waitFor()
        await page.keyboard.type("Paris")
        const release = await hold(page)
        await page.keyboard.down("Enter")
        await held(360)
        release()
        if (end === "failed") await failedToast(page, "Paris").waitFor()
        else {
          await until(() => presses("l-par").some((r) => r.status === 200), "the press answered")
          daemon.readback("eu")
          daemon.readback("home")
          await line(page, "EU").getByText("Paris").waitFor()
        }
        await held(300)
        assert.equal(await page.locator("[data-radix-popper-content-wrapper]").count(), 0, "nothing opens while Enter is held")
        await page.keyboard.up("Enter")
        await page.keyboard.press("Enter")
        await menu(page).locator("[cmdk-input]").waitFor()
        assert.equal(anyPress().length, 1, "the one press")
      } finally {
        await close()
      }
    })

    test(`17. in ${engine}, Space pressed on the waiting line and let go after its ${end === "failed" ? "failed check" : "landing"} opens nothing, and pressed again it opens the menu`, async () => {
      owners()
      daemon.set(end === "failed" ? { preferVerdict: "failed" } : { holdPreferLanding: true })
      const { page, close } = await open("", { engine })
      try {
        await line(page, "EU").focus()
        await page.keyboard.press("Enter")
        await menu(page).locator("[cmdk-input]").waitFor()
        await page.keyboard.type("Paris")
        const release = await hold(page)
        await page.keyboard.press("Enter")
        const waiting = tile(page, "EU").getByRole("button", { name: "Switching to Paris" })
        await waiting.waitFor()
        assert.equal(await waiting.evaluate((el) => el === document.activeElement && el.getAttribute("aria-disabled") === "true"), true, "the focus on the waiting line")
        await page.keyboard.down(" ")
        release()
        if (end === "failed") await failedToast(page, "Paris").waitFor()
        else {
          await until(() => presses("l-par").some((r) => r.status === 200), "the press answered")
          daemon.readback("eu")
          daemon.readback("home")
          await line(page, "EU").getByText("Paris").waitFor()
        }
        await until(async () => (await line(page, "EU").getAttribute("aria-disabled")) === null, "the line takes a press again")
        await page.keyboard.up(" ")
        await painted(page)
        await page.waitForTimeout(200)
        assert.equal(await page.locator("[data-radix-popper-content-wrapper]").count(), 0, "nothing opens as Space comes up")
        await page.keyboard.press(" ")
        await menu(page).locator("[cmdk-input]").waitFor()
        assert.equal(anyPress().length, 1, "the one press")
      } finally {
        await close()
      }
    })
  }

  test("17. on a phone a tap on the line right after a pick by keys that failed opens its menu", async () => {
    onAcme()
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("", { width: 390, touch: true })
    try {
      await line(page, "EU").tap()
      await menu(page).locator("[cmdk-input]").waitFor()
      await page.keyboard.press("End")
      await page.keyboard.press("Enter")
      await toast(page, "failed").getByText("No response", { exact: true }).waitFor()
      await line(page, "EU").tap()
      await menu(page).locator("[cmdk-input]").waitFor()
      assert.equal(anyPress().length, 1, "the one press")
    } finally {
      await close()
    }
  })

  /** Two frames, so an answer that came has been drawn and met. */
  const painted = (page) => page.evaluate(() => new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(done))))
  /** Until the owner's write is answered and read, so no box of it waits. */
  const settled = (page) => until(async () => !(await busy(line(page, "EU"))) && !(await busy(line(page, "Home"))) && (await line(page, "EU").getAttribute("aria-busy")) === null, "the owner's write settled")
  const stale = (route) => route.fulfill({
    status: 412, contentType: "application/json", headers: { "Sidegate-Revision": "1", ETag: '"1"' },
    body: JSON.stringify({ kind: "staleRevision", reason: "stale", message: "Changed by another editor.", revision: 1 }),
  })

  /** Whether the open menu's `server` row says a failed check, its word or its reason. */
  const failedRow = async (page, server) => (await row(page, server).getByText(/^(Failed|No response)$/).count()) > 0
  /** `slot`'s menu opened by hand, its `server` row waited for. */
  const openMenu = async (page, slot, server = "Paris") => {
    await line(page, slot).click()
    await row(page, server).waitFor()
  }
  const closeMenu = async (page) => {
    await page.keyboard.press("Escape")
    await menu(page).waitFor({ state: "detached" })
  }
  /** Off the board by the Menu's link to `route`, and the board gone. */
  const leave = async (page, route) => {
    await nav(page).locator(`a[href$="${route}"]`).click()
    await board(page).waitFor({ state: "detached" })
  }
  const back = async (page) => {
    await nav(page).getByRole("link", { name: "Dashboard" }).click()
    await board(page).waitFor()
  }
  /** EU's press of Paris, held at the daemon's door until the returned release lets it through. */
  async function pressHeld(page) {
    let release
    const released = new Promise((done) => { release = done })
    let held = 0
    await page.route((url) => url.pathname.endsWith("/preferred/l-par"), async (route) => { held++; await released; await route.continue().catch(() => {}) }, { times: 1 })
    await line(page, "EU").click()
    await row(page, "Paris").click()
    await until(() => held > 0, "EU's Paris held")
    return () => release()
  }
  /** Until Paris's `n`th press is answered. */
  const answered = (n = 1) => until(() => presses("l-par").filter((r) => r.status === 200).length >= n, `Paris's press ${n} answered`)

  test("17. a failed check is the owner's: one press, one toast, and the menu of every box of the owner opened by hand has it on its row", async () => {
    owners()
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("")
    try {
      await line(page, "Home").click()
      await row(page, "Paris").click()
      await failedToast(page, "Paris").waitFor()
      await settled(page)
      await painted(page)
      assert.equal(await toast(page, "Paris").count(), 1, "one toast for the owner's one press")
      for (const slot of ["Home", "EU"]) {
        await openMenu(page, slot)
        assert.equal(await row(page, "Paris").getByText("No response", { exact: true }).count(), 1, `${slot}'s menu has Paris's failure`)
        await closeMenu(page)
      }
    } finally {
      await close()
    }
  })

  test("17. an answered press of another server takes the last failure's reason away, and a return to the failed server brings it back", async () => {
    owners()
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("")
    /** EU's menu opened by hand once the owner's write settled and the toast said the failure of `server`'s `n`th check. */
    const failedThen = async (server, n) => {
      await until(() => anyPress().filter((r) => r.path.endsWith(`/${server}`)).length === n, `${server}'s check ${n}`)
      await failedToast(page, server === "l-par" ? "Paris" : "Frankfurt").waitFor()
      await settled(page)
      await line(page, "EU").click()
    }
    try {
      await line(page, "EU").click()
      await row(page, "Paris").click()
      await failedThen("l-par", 1)
      await row(page, "Paris").getByText("No response", { exact: true }).waitFor()
      await row(page, "Frankfurt").click()
      await failedThen("l-fra", 1)
      await row(page, "Frankfurt").getByText("No response", { exact: true }).waitFor()
      assert.equal(await failedRow(page, "Paris"), false, "only Frankfurt's row answers")
      await row(page, "Paris").click()
      await failedThen("l-par", 2)
      await row(page, "Paris").getByText("No response", { exact: true }).waitFor()
      assert.equal(await failedRow(page, "Frankfurt"), false, "only Paris's row answers")
    } finally {
      await close()
    }
  })

  test("17. a refused press, from this box or another, leaves the owner's failure standing", async () => {
    owners()
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("")
    try {
      await line(page, "EU").click()
      await row(page, "Paris").click()
      await failedToast(page, "Paris").waitFor()
      await settled(page)
      await menu(page).waitFor({ state: "detached" })
      for (const slot of ["Home", "EU"]) {
        let release
        const released = new Promise((done) => { release = done })
        await page.route((url) => url.pathname.endsWith("/preferred/l-fra"), async (route) => { await released; await stale(route) }, { times: 1 })
        await line(page, slot).click()
        await row(page, "Frankfurt").click()
        await toast(page, "changed in another tab").waitFor({ state: "detached" })
        release()
        await toast(page, "changed in another tab").waitFor()
        await settled(page)
        await menu(page).waitFor({ state: "detached" })
        await openMenu(page, "EU")
        await row(page, "Paris").getByText("Failed", { exact: true }).waitFor()
        assert.equal(await row(page, "Paris").getByText("No response", { exact: true }).count(), 1, `after ${slot}'s refused press`)
        assert.equal(await failedRow(page, "Frankfurt"), false, "a refusal is no answer")
        await closeMenu(page)
      }
    } finally {
      await close()
    }
  })

  test("17. a check that fails after the reader left the board and came back is the toast, both boxes stand on their server, and the menu opened by hand has it on its row", async () => {
    owners()
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("")
    try {
      const release = await pressHeld(page)
      await leave(page, "/servers")
      await back(page)
      for (const slot of ["EU", "Home"]) await tile(page, slot).getByRole("button", { name: "Switching to Paris" }).waitFor()
      release()
      await answered()
      await failedToast(page, "Paris").waitFor()
      await settled(page)
      for (const slot of ["EU", "Home"]) await tile(page, slot).getByText("Amsterdam").waitFor()
      assert.equal(await menu(page).count(), 0, "no menu opens")
      await openMenu(page, "EU")
      assert.equal(await row(page, "Paris").getByText("No response", { exact: true }).count(), 1, "Paris's row has the failure")
    } finally {
      await close()
    }
  })

  test("17. a check that fails while the reader is off the board is the toast where they are, the server and why; back on the board both boxes stand on their server and the menu opened by hand has it on its row", async () => {
    owners()
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("")
    try {
      for (const [n, away] of ["/servers", "/sources/aurora", "/settings"].entries()) {
        const release = await pressHeld(page)
        await toast(page, "Paris").waitFor({ state: "detached" })
        await leave(page, away)
        release()
        await answered(n + 1)
        await failedToast(page, "Paris").waitFor()
        assert.equal(await toast(page, "Paris").count(), 1, `${away}: the toast once`)
        await back(page)
        await settled(page)
        for (const slot of ["EU", "Home"]) await tile(page, slot).getByText("Amsterdam").waitFor()
        await openMenu(page, "EU")
        assert.equal(await row(page, "Paris").getByText("No response", { exact: true }).count(), 1, `${away}: Paris's row has the failure`)
        await closeMenu(page)
      }
    } finally {
      await close()
    }
  })

  test("17. a failure seen on its row stands after a round trip off the board, and goes at its minute", async () => {
    owners()
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("", { clock: true })
    try {
      await line(page, "EU").click()
      await row(page, "Paris").click()
      await failedToast(page, "Paris").waitFor()
      await settled(page)
      await openMenu(page, "EU")
      await row(page, "Paris").getByText("No response", { exact: true }).waitFor()
      await closeMenu(page)
      await leave(page, "/servers")
      await back(page)
      await openMenu(page, "EU")
      assert.equal(await row(page, "Paris").getByText("No response", { exact: true }).count(), 1, "the round trip keeps it")
      await closeMenu(page)
      await leave(page, "/servers")
      await page.clock.fastForward(61_000)
      await back(page)
      await openMenu(page, "EU")
      assert.equal(await failedRow(page, "Paris"), false, "back after its minute, the row is the server's own")
    } finally {
      await close()
    }
  })

  test("17. a check that passes while the reader is off the board says nothing, and back on the board both boxes are on the server", async () => {
    owners()
    const { page, close } = await open("")
    try {
      const release = await pressHeld(page)
      await leave(page, "/servers")
      release()
      await answered()
      await until(async () => daemon.slot("eu").appliedServer === "l-par", "EU on Paris")
      await back(page)
      for (const slot of ["EU", "Home"]) await tile(page, slot).getByText("Paris").waitFor()
      await settled(page)
      await painted(page)
      assert.equal(await toast(page, "Paris").count(), 0, "no toast")
    } finally {
      await close()
    }
  })

  test("17. a page reloaded while a check is out never heard the press: its boxes show the daemon's view, and nothing is said of the check", async () => {
    owners()
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("")
    try {
      await pressHeld(page)
      await page.reload()
      await board(page).waitFor()
      for (const slot of ["EU", "Home"]) await tile(page, slot).getByText("Amsterdam").waitFor()
      await settled(page)
      await painted(page)
      assert.equal(await toast(page, "Paris").count(), 0, "no toast")
      await openMenu(page, "EU")
      assert.equal(await failedRow(page, "Paris"), false, "no failure on its row")
    } finally {
      await close()
    }
  })

  test("17. the row the slots are on takes no press wherever it stands, at full strength with no hover ground", async () => {
    owners()
    const { page, close } = await open("")
    try {
      await line(page, "EU").click()
      const ams = row(page, "Amsterdam")
      await ams.getByRole("img", { name: "In use: EU, Home" }).waitFor()
      assert.equal(await busy(ams), true, "second in the order, and still no press")
      await ams.locator("[data-slot=option-name]").hover()
      await page.waitForTimeout(200)
      const look = await ams.evaluate((o) => ({ ground: getComputedStyle(o).backgroundColor, opacity: getComputedStyle(o).opacity }))
      assert.deepEqual(look, { ground: "rgba(0, 0, 0, 0)", opacity: "1" }, "full strength, no hover ground")
      await ams.click({ force: true })
      await page.waitForTimeout(300)
      assert.equal(presses("l-ams").length, 0, "the row in use sends nothing")
      assert.equal(await menu(page).isVisible(), true, "and closes nothing")
    } finally {
      await close()
    }
  })

  test("17. a held pick says Manual override under the server on every box of its owner on that server, heard the same, and on no other box", async () => {
    owners()
    const lab = daemon.slot("lab")
    daemon.set({ slots: new Map(daemon.slots().map((x) => [x.id, x.id === "lab" ? { ...lab, server: "l-ams", appliedServer: "l-ams", runtime: { ...lab.runtime, applied: { kind: "up", server: "l-ams" } } } : x])) })
    const { page, close } = await open("")
    try {
      await line(page, "EU").waitFor()
      assert.deepEqual(await saying(page), [], "no pick, no words")
      assert.doesNotMatch(await board(page).innerText(), /Manual|Pick|for now/, "nothing on the board speaks of picking before a pick")
      prefer("group", AURORA, "l-ams")
      await words(page, "EU").waitFor()
      assert.deepEqual(await saying(page), ["EU", "Home"], "the group's boxes, not Lab, on the same server by its source")
      assert.equal(await words(page, "EU").innerText(), "Manual override", "a day-long pick just made, with no time")
      assert.match(await tile(page, "EU").ariaSnapshot(), /Manual override/, "heard as a sentence")
      assert.doesNotMatch(await tile(page, "EU").ariaSnapshot(), /Manual ·/, "heard in words, not its mark")
      const name = await line(page, "EU").getByText("Amsterdam").boundingBox()
      const said = await words(page, "EU").evaluate((w) => { const r = document.createRange(); r.selectNodeContents(w); const b = r.getBoundingClientRect(); return { x: b.left, y: b.top } })
      assert.ok(said.y >= name.y + name.height - 0.5 && Math.abs(said.x - name.x) <= 0.5, `on its own line under the server, from the name's first letter: ${JSON.stringify([name, said])}`)
      prefer("group", AURORA, null)
      prefer("source", "aurora", "l-ams")
      await words(page, "Lab").waitFor()
      assert.deepEqual(await saying(page), ["Lab"], "a source's pick on its own box alone")
    } finally {
      await close()
    }
  })

  for (const engine of ENGINES) {
    test(`17. in ${engine}, at 1280 and 390 px, a click on a box's Manual override opens its menu as a click on the server line does, 4 px under the line; the words are no stop of their own, and the pointer on them lights the line's ground`, async () => {
      owners()
      prefer("group", AURORA, "l-ams")
      for (const width of [1280, 390]) {
        const { page, close } = await open("", { width, engine })
        /** The line's ground once its colour has stopped moving. */
        const ground = async () => {
          await page.waitForTimeout(300)
          return line(page, "EU").evaluate((b) => getComputedStyle(b).backgroundColor)
        }
        try {
          await words(page, "EU").waitFor()
          const said = await words(page, "EU").evaluate((w) => {
            const r = w.parentElement.getBoundingClientRect()
            const stops = "a[href], button, input, select, textarea, [tabindex]"
            return { x: r.left + r.width / 2, y: r.top + r.height / 2, stop: w.parentElement.matches(stops) || w.parentElement.querySelector(stops) !== null }
          })
          assert.equal(said.stop, false, `at ${width} px the words are no stop and hold none`)
          await page.mouse.click(said.x, said.y)
          await until(() => menu(page).count(), `at ${width} px a click on the words to open the menu`, 2_000)
          await head(page).waitFor()
          await until(() => menu(page).evaluate((el) => el.getAnimations({ subtree: true }).length === 0), "the menu to settle")
          assert.equal(await line(page, "EU").getAttribute("aria-expanded"), "true", "EU's own menu")
          const [server, opened] = await Promise.all([line(page, "EU").boundingBox(), menu(page).locator("[data-slot=popover-content]").boundingBox()])
          assert.ok(Math.abs(opened.y - (server.y + server.height) - 4) <= 0.5, `at ${width} px 4 px under the server line, over the words' room: ${JSON.stringify([server, opened])}`)
          await page.keyboard.press("Escape")
          await menu(page).waitFor({ state: "detached" })
          await page.mouse.move(0, 0)
          const rest = await ground()
          await line(page, "EU").hover()
          const lit = await ground()
          assert.notEqual(lit, rest, `at ${width} px the pointer on the line lights its ground`)
          await page.mouse.move(0, 0)
          assert.equal(await ground(), rest)
          await page.mouse.move(said.x, said.y)
          assert.equal(await ground(), lit, `at ${width} px the pointer on the words lights the line's ground as on the line`)
        } finally {
          await close()
        }
      }
    })
  }

  test("17. a pass puts the words on each box once it has landed: none while Switching to, then one box landed and one landing", async () => {
    owners()
    daemon.set({ holdPreferLanding: true })
    const { page, close } = await open("")
    try {
      await line(page, "EU").click()
      await row(page, "Paris").click()
      await menu(page).waitFor({ state: "detached" })
      for (const slot of ["EU", "Home"]) await tile(page, slot).getByRole("button", { name: "Switching to Paris" }).waitFor()
      await until(() => presses("l-par").some((r) => r.status === 200), "the press answered")
      assert.deepEqual(await saying(page), [], "none while every box lands")
      daemon.readback("eu")
      await words(page, "EU").waitFor()
      assert.deepEqual(await saying(page), ["EU"], "the landed box alone")
      daemon.readback("home")
      await words(page, "Home").waitFor()
      assert.deepEqual(await saying(page), ["EU", "Home"])
      await line(page, "EU").click()
      assert.equal(await busy(row(page, "Paris")), true, "the row the slots moved to takes no press")
      await row(page, "Paris").click({ force: true })
      await page.waitForTimeout(300)
      assert.equal(presses("l-par").length, 1, "the press that moved them, and no other")
    } finally {
      await close()
    }
  })

  test("17. on a phone a box on the picked server keeps its words' room, unseen, while its owner's press is out, so no box moves at a press, a failure or a landing on the pick, and one on a server that holds no pick gives it up as it lands", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    daemon.set({ holdPreferLanding: true })
    const { page, close } = await open("", { width: 390 })
    /** The heights of EU and Home, and where Lab under them starts. */
    const laid = async () => [...await Promise.all(["EU", "Home"].map(async (slot) => (await tile(page, slot).boundingBox()).height)), (await tile(page, "Lab").boundingBox()).y]
    const press = async (server) => {
      await line(page, "EU").click()
      await row(page, server).click()
      for (const slot of ["EU", "Home"]) await tile(page, slot).getByRole("button", { name: `Switching to ${server}` }).waitFor()
    }
    try {
      await words(page, "Home").waitFor()
      const held = await laid()
      daemon.set({ preferVerdict: "failed" })
      const release = await hold(page)
      await press("Paris")
      assert.deepEqual(await saying(page), [], "the words unseen while the press is out")
      assert.deepEqual(await laid(), held, "no box moves at the press")
      release()
      await failedToast(page, "Paris").waitFor()
      for (const slot of ["EU", "Home"]) await words(page, slot).waitFor()
      assert.deepEqual(await laid(), held, "nor at the failure")

      daemon.set({ preferVerdict: "succeeded" })
      await settled(page)
      await press("Paris")
      await until(() => presses("l-par").some((r) => r.status === 200), "Paris answered")
      await painted(page)
      assert.deepEqual(await laid(), held, "nor once it is answered")
      daemon.readback("eu")
      await words(page, "EU").waitFor()
      assert.deepEqual(await laid(), held, "nor as one box lands on the pick")
      daemon.readback("home")
      await words(page, "Home").waitFor()
      assert.deepEqual(await laid(), held, "nor as the other does")

      await settled(page)
      await press("Frankfurt")
      await until(() => presses("l-fra").some((r) => r.status === 200), "Frankfurt answered")
      await painted(page)
      assert.deepEqual(await laid(), held, "a press of the first server keeps the room while it is out")
      daemon.readback("eu")
      daemon.readback("home")
      for (const slot of ["EU", "Home"]) await line(page, slot).getByText("Frankfurt").waitFor()
      await painted(page)
      const [eu, home, lab] = await laid()
      assert.deepEqual([held[0] - eu, held[1] - home, held[2] - lab], [16, 16, 32], "landed on a server that holds no pick, each box gives up its 16 px once")
      assert.deepEqual(await saying(page), [], "and says nothing")
    } finally {
      await close()
    }
  })

  test("17. the words go after a failover off the picked server", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    const { page, close } = await open("")
    try {
      await words(page, "Home").waitFor()
      for (const id of ["eu", "home"]) daemon.changeSlot(id, { server: "l-fra", appliedServer: "l-fra", runtime: { ...daemon.slot(id).runtime, applied: { kind: "up", server: "l-fra" } } })
      await line(page, "Home").getByText("Frankfurt").waitFor()
      assert.deepEqual(await saying(page), [], "a failover ends it")
    } finally {
      await close()
    }
  })

  test("17. a box no pass vouches for keeps its server line with the name muted, its chevron, its Manual words and its menu's Clear, and says Last active server", async () => {
    owners()
    daemon.set({ catalogue: auroraWithDown("l-ams") })
    prefer("group", AURORA, "l-ams")
    const { page, close } = await open("")
    try {
      await words(page, "EU").waitFor()
      for (const id of ["eu", "home"]) daemon.changeSlot(id, { appliedServer: null, runtime: { ...daemon.slot(id).runtime, phase: "unknown", applied: { kind: "down", reason: "unselected" } } })
      const known = tile(page, "EU").getByRole("button", { name: /^Last active server of .+: Amsterdam$/ })
      await known.waitFor()
      assert.deepEqual(await saying(page), ["EU", "Home"], "the pick stands on both boxes")
      assert.equal(await tile(page, "EU").getByRole("status").count(), 0, "nothing waits or spins")
      assert.equal(await known.getByText("Amsterdam").evaluate((el) => el.closest(".text-muted-foreground") !== null), true, "the name in the muted ink")
      await dot(tile(page, "EU"), "Serving · Offline · Not responding").waitFor()
      await known.hover()
      await page.locator("[data-radix-popper-content-wrapper]").getByText("Last active server", { exact: true }).waitFor()
      await page.mouse.move(0, 0)
      await known.click()
      await row(page, "Paris").waitFor()
      await page.waitForTimeout(400)
      assert.equal(await page.locator("[data-radix-popper-content-wrapper]").getByText("Last active server", { exact: true }).isVisible(), false, "the line's card never stands over its menu")
      assert.equal(await busy(row(page, "Amsterdam")), true, "the row the boxes draw takes no press")
      await clearButton(page).waitFor()
      assert.equal(await busy(row(page, "Paris")), false, "another server takes a press")
      await row(page, "Amsterdam").click({ force: true })
      await page.waitForTimeout(300)
      assert.equal(presses("l-ams").length, 0, "no second pick of the server the boxes draw")
      await page.keyboard.press("Escape")
      await page.goto(daemon.url + `groups/${AURORA}`)
      await page.getByRole("button", { name: /^Delete / }).first().click()
      await dot(page.getByRole("alertdialog").getByRole("region", { name: "Used by" }), "Serving · Offline · Not responding").first().waitFor()
    } finally {
      await close()
    }
  })

  /** EU's slot as the daemon holds it, and a pass that reads nothing of it, which leaves its line on the last known server. */
  const unread = () => {
    const read = daemon.slot("eu")
    return { unknown: () => daemon.changeSlot("eu", { appliedServer: null, runtime: { ...read.runtime, phase: "unknown", applied: { kind: "down", reason: "unselected" } } }), confirm: () => daemon.changeSlot("eu", { appliedServer: read.appliedServer, runtime: read.runtime }) }
  }
  const lastKnown = (page) => tile(page, "EU").getByRole("button", { name: /^Last active server of .+: Amsterdam$/ })
  const lastKnownCard = (page) => page.locator("[data-radix-popper-content-wrapper]").getByText("Last active server", { exact: true })

  test("17. a read that confirms the last known server keeps the line's focus, so Enter opens its menu", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    const { page, close } = await open("")
    try {
      await words(page, "EU").waitFor()
      const slot = unread()
      slot.unknown()
      await lastKnown(page).focus()
      slot.confirm()
      await line(page, "EU").waitFor()
      assert.equal(await line(page, "EU").evaluate((el) => el === document.activeElement), true, "the line keeps its focus")
      await page.keyboard.press("Enter")
      await clearButton(page).waitFor()
    } finally {
      await close()
    }
  })

  /** Where the pointer rests when a read confirms the last known server: the line, which stays, or the card, which goes with no leave heard. */
  const confirmedUnder = [["line", lastKnown], ["card", lastKnownCard]]
  for (const engine of ENGINES) for (const [where, under] of confirmedUnder) {
    test(`17. in ${engine}, a read that confirms the last known server under the pointer on its ${where} ends the card, so a later unknown read opens none the pointer has left`, async () => {
      owners()
      prefer("group", AURORA, "l-ams")
      const { page, close } = await open("", { engine })
      try {
        await words(page, "EU").waitFor()
        const slot = unread()
        slot.unknown()
        await lastKnown(page).hover()
        await lastKnownCard(page).waitFor()
        await under(page).hover()
        await page.waitForTimeout(400)
        slot.confirm()
        await lastKnownCard(page).waitFor({ state: "detached" })
        await page.mouse.move(0, 0)
        await page.waitForTimeout(300)
        slot.unknown()
        await lastKnown(page).waitFor()
        await page.waitForTimeout(650)
        assert.equal(await lastKnownCard(page).isVisible(), false, "the card stays closed")
      } finally {
        await close()
      }
    })
  }

  for (const engine of ENGINES) {
    test(`17. in ${engine}, at 1280 and 390 px, on a last known server the pointer resting on a box's Manual override opens no card, and a click or a tap there opens the box's menu; the line's card opens from the line and stands clear of the words`, async () => {
      for (const width of [1280, 390]) {
        owners()
        prefer("group", AURORA, "l-ams")
        const { page, close } = await open("", { width, engine, touch: true })
        try {
          await words(page, "EU").waitFor()
          unread().unknown()
          await lastKnown(page).waitFor()
          const { letters, strip } = await words(page, "EU").evaluate((w) => {
            const box = (r) => ({ x: r.left, y: r.top, width: r.width, height: r.height })
            return { letters: box(w.getBoundingClientRect()), strip: box(w.parentElement.getBoundingClientRect()) }
          })
          const at = { x: letters.x + letters.width / 2, y: letters.y + letters.height / 2 }
          for (const press of ["click", "tap"]) {
            await page.mouse.move(at.x, at.y)
            await page.waitForTimeout(HOVER_SETTLES_MS)
            assert.equal(await lastKnownCard(page).isVisible(), false, `at ${width} px the pointer resting on the words opens no card`)
            if (press === "click") await page.mouse.click(at.x, at.y)
            else await page.touchscreen.tap(at.x, at.y)
            await until(() => menu(page).count(), `at ${width} px a ${press} on the words to open the menu`, 2_000)
            assert.equal(await lastKnown(page).getAttribute("aria-expanded"), "true", "EU's own menu")
            await page.keyboard.press("Escape")
            await menu(page).waitFor({ state: "detached" })
          }
          await page.mouse.move(0, 0)
          await lastKnown(page).hover()
          await lastKnownCard(page).waitFor()
          const card = await page.locator("[data-radix-popper-content-wrapper]").filter({ hasText: "Last active server" }).locator("[data-side]").first().boundingBox()
          assert.ok(card.y >= strip.y + strip.height, `at ${width} px the line's card stands clear of the words: ${JSON.stringify({ card, strip })}`)
          await page.mouse.move(at.x, at.y)
          await page.mouse.click(at.x, at.y)
          await until(() => menu(page).count(), `at ${width} px a click on the words, the line's card open a moment before, to open the menu`, 2_000)
        } finally {
          await close()
        }
      }
    })
  }

  test("17. on the page's clock a held pick's boxes say Manual override throughout, and its chip's card says when it wears off, in 1d, then 2h, 5m, and 1m under a minute, heard in whole words, each in one ink throughout, and nothing from the moment it ends", async () => {
    owners()
    const at = Date.now()
    /* The clock stops 5 s in, and the pick ends 15 s past the day from there, so the clock's half-minute ticks fall 10 to 20 s
     * either side of the end and only the end itself can take the words. */
    const base = at + 5_000
    const left = 24 * 3600e3 + 15_000
    prefer("group", AURORA, "l-ams", (base + left - at) / 3600e3, at)
    const { page, close } = await open("", { clock: at })
    /** The colour of the element that holds an element's words, however deep. */
    const inkOf = (el) => {
      let at = el
      while (at.firstElementChild) at = at.firstElementChild
      return getComputedStyle(at).color
    }
    /** What EU's menu says of the pick: its card's name and value, opened by the keyboard, and how the chip's word is heard; and
     * the ink of the card's time. */
    const menuSays = async () => {
      await line(page, "EU").click()
      await head(page).waitFor()
      let said = null
      if ((await chip(page).count()) > 0) {
        const heard = await chipWord(page).getAttribute("aria-label")
        await chipWord(page).focus()
        await chipCard(page).waitFor()
        const ink = await chipCard(page).locator("[data-slot=row] > span").last().evaluate(inkOf)
        said = { words: [...await cardSays(page), heard], ink }
        await page.keyboard.press("Escape")
        await chipCard(page).waitFor({ state: "detached" })
      }
      await page.keyboard.press("Escape")
      await menu(page).waitFor({ state: "detached" })
      return said
    }
    const boxes = async () => [await words(page, "EU").innerText(), await words(page, "Home").innerText()]
    const inks = () => Promise.all(["EU", "Home"].map((slot) => words(page, slot).evaluate(inkOf)))
    try {
      await words(page, "EU").waitFor()
      await page.clock.pauseAt(base)
      assert.deepEqual(await boxes(), ["Manual override", "Manual override"])
      const boxInk = await inks()
      const first = await menuSays()
      assert.deepEqual(first.words, ["Auto-resets", "in 1d", "Manual override. Auto-resets in 1 day"], "the card says the time, the word is heard with it")
      for (const [forward, shows, spoken] of [[left - 2 * 3600e3, "2h", "2 hours"], [115 * 60_000, "5m", "5 minutes"], [295_000, "1m", "1 minute"]]) {
        await page.clock.fastForward(forward)
        const said = await menuSays()
        assert.deepEqual(said.words, ["Auto-resets", `in ${shows}`, `Manual override. Auto-resets in ${spoken}`], `in ${shows}`)
        assert.equal(said.ink, first.ink, `the card's time has no ink of its own at ${shows}`)
        assert.deepEqual(await boxes(), ["Manual override", "Manual override"], `the boxes say no time at ${shows}`)
        assert.deepEqual(await inks(), boxInk, `the boxes' words have no ink of their own at ${shows}`)
      }
      await page.clock.fastForward(6_000)
      await until(async () => (await saying(page)).length === 0, "the words to go at the pick's end, not at the clock's next tick", 2_000)
      assert.equal(await menuSays(), null, "no chip after the end")
      assert.match(await line(page, "EU").innerText(), /Amsterdam/, "the box still on the server until the daemon moves it")
    } finally {
      await close()
    }
  })

  test("17. while the owner's menu is open every line of the owner is lit and its words keep their room unseen, so no box moves", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    const { page, close } = await open("")
    try {
      await words(page, "Home").waitFor()
      const heights = () => Promise.all(["EU", "Home", "Lab"].map(async (slot) => (await tile(page, slot).boundingBox()).height))
      const before = await heights()
      await line(page, "EU").click()
      await head(page).waitFor()
      await page.mouse.move(0, 0)
      assert.deepEqual(await heights(), before, "no box changes height under the menu")
      for (const slot of ["EU", "Home"]) assert.equal(await words(page, slot).evaluate((w) => getComputedStyle(w).visibility), "hidden", `${slot}'s words unseen`)
      const ground = (slot) => line(page, slot).evaluate((l) => getComputedStyle(l).backgroundColor)
      await until(async () => (await ground("Home")) === (await ground("EU")), "Home's line to take the open line's ground")
      assert.notEqual(await ground("Lab"), await ground("EU"), "Lab, on another owner, is not lit")
      await page.keyboard.press("Escape")
      await menu(page).waitFor({ state: "detached" })
      await words(page, "Home").waitFor()
      await page.mouse.move(0, 0)
      await until(async () => (await ground("Home")) === (await ground("Lab")), "closed, Home is no longer lit")
    } finally {
      await close()
    }
  })

  test("17. the menu's head is one line: the owner's name in the plain ink, then the slots a pick moves when two or more follow it, beside Ping and Stability", async () => {
    owners()
    const { page, close } = await open("")
    const heads = async (slot) => {
      await line(page, slot).click()
      await head(page).waitFor()
      const laid = await head(page).evaluate((h) => {
        const title = h.firstElementChild.firstElementChild
        const name = title.firstElementChild
        const [ping] = [...h.querySelectorAll("span")].filter((e) => e.textContent === "Ping")
        const [t, p] = [title.getBoundingClientRect(), ping.getBoundingClientRect()]
        const lines = Math.round(t.height / parseFloat(getComputedStyle(title).lineHeight))
        return { text: title.textContent, rows: h.children.length, lines, level: Math.abs(t.bottom - p.bottom) <= 2, plain: getComputedStyle(name).color !== getComputedStyle(title).color }
      })
      await page.keyboard.press("Escape")
      await menu(page).waitFor({ state: "detached" })
      return laid
    }
    try {
      assert.deepEqual(await heads("EU"), { text: "All Aurora · EU and Home", rows: 1, lines: 1, level: true, plain: true }, "two slots: their names follow the owner's, muted, on Ping's line")
      assert.deepEqual(await heads("Lab"), { text: "Aurora", rows: 1, lines: 1, level: true, plain: true }, "a source with one slot: its name alone")
      daemon.changeSlot("home", { exit: { kind: "source", source: "aurora" } })
      await until(async () => (await heads("EU")).text === "All Aurora", "one slot on the group: its name alone")
      await line(page, "EU").click()
      await head(page).waitFor()
      assert.doesNotMatch(await menu(page).innerText(), /Pick|for now|Manual/, "nothing in the menu speaks of picking before a pick")
    } finally {
      await close()
    }
  })

  test("17. on a phone the head with one slot and with five stays clear of the columns' heads and is cut nowhere", async () => {
    for (const five of [false, true]) {
      owners()
      if (five) for (const [id, name] of [["eu", "Living room"], ["home", "Office upstairs"], ["lab", "Kids tablet"], ["office", "Travel router"], ["kids", "Home"]]) {
        const x = daemon.slot(id)
        daemon.changeSlot(id, { name, exit: { kind: "group", group: AURORA }, status: "serving", server: "l-ams", appliedServer: "l-ams", runtime: { ...x.runtime, applied: { kind: "up", server: "l-ams" } } })
      } else daemon.changeSlot("home", { exit: { kind: "source", source: "aurora" } })
      const { page, close } = await open("", { width: 390 })
      try {
        await page.getByRole("button", { name: /^Server of / }).first().click()
        await head(page).waitFor()
        const laid = await head(page).evaluate((h) => {
          const who = h.firstElementChild.firstElementChild
          const [ping] = [...h.querySelectorAll("span")].filter((e) => e.textContent === "Ping")
          const [w, p] = [who.getBoundingClientRect(), ping.getBoundingClientRect()]
          const lines = Math.round(w.height / parseFloat(getComputedStyle(who).lineHeight))
          return { whole: who.scrollWidth <= who.clientWidth && h.scrollWidth <= h.clientWidth, clear: p.left >= w.right - 0.5, inside: w.right <= h.getBoundingClientRect().right + 0.5, lines }
        })
        assert.deepEqual({ ...laid, lines: laid.lines > 1 }, { whole: true, clear: true, inside: true, lines: five }, five ? "five slots wrap beside the columns' heads" : "one slot on one line")
        if (five) assert.equal(await head(page).locator(":scope > span > span").first().textContent(), "All Aurora · Living room, Office upstairs, Kids tablet, Travel router and Home")
      } finally {
        await close()
      }
    }
  })

  test("17. while a pick holds its row's foot is one chip under the name, named Manual, the word Manual and its ×, which is Clear: it drops the pick and moves no slot, its spinner in the ×'s place while it is out, and the menu stays open", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    const endsAt = daemon.groups().find((g) => g.id === AURORA).preferred.endsAt
    const { page, close } = await open("")
    try {
      await line(page, "EU").click()
      const ams = row(page, "Amsterdam")
      await chip(page).waitFor()
      assert.match(await foot(page).evaluate((f) => f.previousElementSibling.textContent), /^Amsterdam/, "on the picked row's foot")
      assert.equal(await foot(page).innerText(), "Manual", "the chip's word and nothing else: no sentence, no time, no Clear")
      assert.equal(await page.evaluate(() => [...document.querySelectorAll("body *")].filter((e) => e.checkVisibility({ visibilityProperty: true }) && [...e.childNodes].some((n) => n.nodeType === 3 && /Manual/.test(n.textContent))).length), 1, "Manual said once on screen, the boxes' words unseen under the menu")
      const [name, word] = await Promise.all([ams.locator("[data-slot=option-name]").boundingBox(), chipWord(page).boundingBox()])
      assert.ok(word.y >= name.y + name.height && word.x >= name.x - 0.5 && word.x <= name.x + 12, `under the name, from its column: ${JSON.stringify([name, word])}`)
      assert.equal(await chipWord(page).getAttribute("aria-label"), "Manual override. Auto-resets in 1 day", "the word heard with its card")
      assert.equal(await clearButton(page).getAttribute("aria-label"), "Clear manual override on Amsterdam")
      assert.equal(await clearButton(page).innerText(), "", "the × alone")
      assert.equal(await clearButton(page).locator("svg.lucide-x").count(), 1)
      assert.doesNotMatch(await menu(page).innerText(), /Pick|for now|Clear/, "the menu names no pick and says no Clear")
      let release
      const released = new Promise((r) => { release = r })
      await page.route((url) => url.pathname.includes("/preferred/"), async (route) => { await released; await route.continue().catch(() => {}) }, { times: 1 })
      await clearButton(page).click()
      await until(async () => (await clearButton(page).getAttribute("aria-busy")) === "true", "Clear's spinner")
      assert.equal(await clearButton(page).locator("svg.lucide-x").count(), 0, "the spinner in the ×'s place")
      for (const other of ["Frankfurt", "Paris"]) assert.equal(await busy(row(page, other)), true, `${other} waits`)
      assert.equal(await ams.evaluate((o) => getComputedStyle(o).opacity), "1", "the picked row stands at full strength")
      release()
      await foot(page).waitFor({ state: "detached" })
      assert.deepEqual(unpicks().map((r) => r.path + r.search), [`/proxy/api/groups/${AURORA}/preferred/l-ams?endsAt=${encodeURIComponent(endsAt)}`], "one DELETE with the view's endsAt")
      assert.equal(anyPress().length, 0, "no press")
      assert.equal(await menu(page).isVisible(), true, "the menu stays open")
      assert.equal(await clearButton(page).count(), 0, "the row is a plain row in use")
      assert.equal(await busy(ams), true, "and still takes no press")
      await page.keyboard.press("Escape")
      for (const slot of ["EU", "Home"]) assert.match(await line(page, slot).innerText(), /Amsterdam/, `${slot} stays on the server`)
      assert.deepEqual(await saying(page), [], "no box says Manual once it is cleared")
    } finally {
      await close()
    }
  })

  test("17. Clear is its own stop: Tab from the search reaches it, and Enter, Space and a click on it clear the pick and never pick the highlighted row", async () => {
    for (const how of ["Enter", "Space", "click"]) {
      owners()
      prefer("group", AURORA, "l-ams")
      const before = daemon.log().length
      const { page, close } = await open("")
      try {
        await line(page, "EU").click()
        await clearButton(page).waitFor()
        await page.keyboard.press("ArrowDown")
        assert.equal(await menu(page).locator("[cmdk-item][data-selected=true]").count(), 1, "a row is highlighted")
        if (how === "click") await clearButton(page).click()
        else {
          await menu(page).getByRole("combobox").focus()
          for (let i = 0; i < 4 && !(await clearButton(page).evaluate((b) => b === document.activeElement)); i++) await page.keyboard.press("Tab")
          assert.equal(await clearButton(page).evaluate((b) => b === document.activeElement), true, "Tab reaches Clear")
          await page.keyboard.press(how === "Space" ? " " : "Enter")
        }
        await foot(page).waitFor({ state: "detached" })
        await page.waitForTimeout(300)
        const sent = daemon.log().slice(before).filter((r) => r.path.includes("/preferred/"))
        assert.deepEqual(sent.map((r) => r.method), ["DELETE"], `${how}: one DELETE and no press`)
        assert.equal(await menu(page).isVisible(), true, `${how}: the menu stays open`)
      } finally {
        await close()
      }
    }
  })

  test("17. Clear is the one thing that looks chosen: with the focus or the pointer on it, or its answer out, no row is highlighted, and an arrow gives the list its highlight back", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    const before = daemon.log().length
    const { page, close } = await open("")
    try {
      const lit = () => menu(page).locator("[cmdk-item][data-selected=true]")
      const focused = (l) => l.evaluate((e) => e === document.activeElement)
      await line(page, "EU").click()
      await clearButton(page).waitFor()
      assert.equal(await lit().count(), 1, "the menu opens with its first pickable row highlighted, as every list on the page does")
      await menu(page).getByRole("combobox").focus()
      for (let i = 0; i < 4 && !(await focused(clearButton(page))); i++) await page.keyboard.press("Tab")
      assert.equal(await focused(clearButton(page)), true, "Tab reaches Clear")
      assert.equal(await lit().count(), 0, "Tab to Clear leaves no row highlighted")
      await page.keyboard.press("ArrowDown")
      assert.equal(await lit().count(), 1, "an arrow brings the highlight back")
      const nearest = await foot(page).evaluate((f) => [...f.closest("[cmdk-list]").querySelectorAll("[cmdk-item]:not([aria-disabled=true])")]
        .find((r) => f.compareDocumentPosition(r) & Node.DOCUMENT_POSITION_FOLLOWING).getAttribute("data-value"))
      assert.equal(await lit().getAttribute("data-value"), nearest, "on the nearest row below Clear")
      assert.equal(await focused(menu(page).getByRole("combobox")), true, "with the focus back in the search")
      await clearButton(page).hover()
      assert.equal(await lit().count(), 0, "the pointer on Clear leaves no row highlighted")
      await row(page, "Paris").hover()
      assert.equal(await lit().count(), 1, "the pointer on a row highlights it")
      for (let i = 0; i < 4 && !(await focused(clearButton(page))); i++) await page.keyboard.press("Tab")
      const release = await hold(page)
      await page.keyboard.press("Enter")
      await until(async () => (await clearButton(page).getAttribute("aria-busy")) === "true", "Clear's spinner")
      await row(page, "Paris").hover({ force: true })
      await page.keyboard.press("ArrowDown")
      assert.equal(await lit().count(), 0, "while the answer is out no row takes the highlight")
      release()
      await foot(page).waitFor({ state: "detached" })
      await page.waitForTimeout(300)
      const sent = daemon.log().slice(before).filter((r) => r.path.includes("/preferred/"))
      assert.deepEqual(sent.map((r) => r.method), ["DELETE"], "Enter on Clear clears and picks nothing")
    } finally {
      await close()
    }
  })

  test("17. Home, End and Ctrl+N, J, P and K on Clear give the keys back to the search and highlight the row they reach from Clear", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    const { page, close } = await open("")
    try {
      const focused = (l) => l.evaluate((e) => e === document.activeElement)
      const search = menu(page).getByRole("combobox")
      await line(page, "EU").click()
      await clearButton(page).waitFor()
      /* Amsterdam, which the slots are on, takes no press, so Frankfurt is first and above Clear, and Paris last and below it. */
      for (const [key, reached] of [["Home", "server:l-fra"], ["End", "server:l-par"], ["Control+n", "server:l-par"], ["Control+j", "server:l-par"], ["Control+p", "server:l-fra"], ["Control+k", "server:l-fra"]]) {
        await search.focus()
        for (let i = 0; i < 4 && !(await focused(clearButton(page))); i++) await page.keyboard.press("Tab")
        assert.equal(await focused(clearButton(page)), true, `${key}: Tab reaches Clear`)
        await page.keyboard.press(key)
        assert.equal(await focused(search), true, `${key}: the keys are back in the search`)
        assert.equal(await menu(page).locator("[cmdk-item][data-selected=true]").getAttribute("data-value"), reached, `${key}: the row it reaches from Clear is highlighted`)
      }
      assert.equal(unpicks().length + anyPress().length, 0, "no press and no Clear")
    } finally {
      await close()
    }
  })

  test("17. a held pick beside a failed Test keeps its chip, and its Clear one press away", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    daemon.set({ preferVerdict: "failed" })
    const { page, close } = await open("")
    try {
      await line(page, "EU").click()
      await row(page, "Paris").click()
      await failedToast(page, "Paris").waitFor()
      await settled(page)
      await line(page, "EU").click()
      await row(page, "Paris").getByText("No response", { exact: true }).waitFor()
      await chipWord(page).waitFor()
      assert.equal(await busy(clearButton(page)), false, "Clear is one press away again")
      assert.equal(unpicks().length, 0)
    } finally {
      await close()
    }
  })

  test("17. on a phone the chip's word and its × each have a target 44 px tall, wholly below the row's first line and in the foot, meeting at the chip's divider, the ×'s 45 px wide, so a tap a little off the word opens its card and clears nothing", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    const { page, close } = await open("", { width: 390, touch: true })
    try {
      await line(page, "EU").tap()
      await clearButton(page).waitFor()
      await until(() => menu(page).evaluate((el) => el.getAnimations({ subtree: true }).length === 0), "the menu to settle")
      /** The pixels a press lands on `b` in, counted from its middle up and down, and left and right. */
      const target = (b) => b.evaluate((b) => {
        const r = b.getBoundingClientRect()
        const [x, y] = [Math.floor(r.left + r.width / 2), Math.floor(r.top + r.height / 2)]
        const at = (px, py) => b.contains(document.elementFromPoint(px, py))
        let [top, bottom, left, right] = [y, y, x, x]
        while (at(x, top - 1)) top--
        while (at(x, bottom + 1)) bottom++
        while (at(left - 1, y)) left--
        while (at(right + 1, y)) right++
        bottom++
        right++
        const foot = b.closest("[data-slot=option-foot]").getBoundingClientRect()
        const option = b.closest("[data-slot=option-foot]").previousElementSibling
        const cells = option.querySelector("[data-slot=bars]").parentElement.parentElement.getBoundingClientRect()
        const name = option.querySelector("[data-slot=option-name]").parentElement.getBoundingClientRect()
        return { top, bottom, left, right, tall: bottom - top, wide: right - left, under: top >= Math.max(cells.bottom, name.bottom) - 0.5, inFoot: top >= foot.top - 0.5 && bottom <= foot.bottom + 0.5 }
      })
      const [word, x] = [await target(chipWord(page)), await target(clearButton(page))]
      assert.deepEqual([word.tall, word.under, word.inFoot], [44, true, true], `the word's target: ${JSON.stringify(word)}`)
      /* The × stands where the word's width puts it, between pixels, and a press finds it on every pixel it touches. */
      assert.deepEqual([x.tall, x.wide >= 45 && x.wide <= 46, x.under, x.inFoot], [44, true, true, true], `the ×'s target: ${JSON.stringify(x)}`)
      assert.equal(word.right, x.left, "the two meet at the chip's divider")
      const box = await chipWord(page).boundingBox()
      await page.touchscreen.tap(word.right - 2, box.y - 6)
      await chipCard(page).waitFor()
      await page.waitForTimeout(300)
      assert.equal(unpicks().length, 0, "a tap a little off the word, toward the × and above it, opens the card and clears nothing")
      const stats = row(page, "Amsterdam").locator("[data-slot=bars]")
      await stats.tap()
      await page.waitForTimeout(300)
      assert.equal(unpicks().length, 0, "a tap on Stability sends nothing")
    } finally {
      await close()
    }
  })

  test("17. the chip's word opens its one card, Auto-resets · in 1d, on hover and on keyboard focus, and on a phone on a tap, which a second tap undoes; a tap or a click on the word clears nothing and picks nothing", async () => {
    owners()
    prefer("group", AURORA, "l-ams")
    let { page, close } = await open("")
    try {
      await line(page, "EU").click()
      await chipWord(page).hover()
      await chipCard(page).waitFor()
      assert.deepEqual(await cardSays(page), ["Auto-resets", "in 1d"], "the card by hover")
      assert.equal(await page.locator("[data-radix-popper-content-wrapper]").filter({ hasText: "Manual" }).count(), 1, "no card says Manual but the menu that holds the chip")
      await page.mouse.move(0, 0)
      await chipCard(page).waitFor({ state: "detached" })
      await menu(page).getByRole("combobox").focus()
      for (let i = 0; i < 4 && !(await chipWord(page).evaluate((b) => b === document.activeElement)); i++) await page.keyboard.press("Tab")
      assert.equal(await chipWord(page).evaluate((b) => b === document.activeElement), true, "Tab from the search reaches the word")
      await chipCard(page).waitFor()
      assert.deepEqual(await cardSays(page), ["Auto-resets", "in 1d"], "the card by focus")
      await page.keyboard.press("Tab")
      assert.equal(await clearButton(page).evaluate((b) => b === document.activeElement), true, "then the ×")
      await chipCard(page).waitFor({ state: "detached" })
      await chipWord(page).click()
      await chipCard(page).waitFor()
      await page.waitForTimeout(300)
      assert.equal(unpicks().length + anyPress().length, 0, "a click on the word clears nothing and picks nothing")
      await close()
      ;({ page, close } = await open("", { width: 390, touch: true }))
      await line(page, "EU").tap()
      await chipWord(page).tap()
      await chipCard(page).waitFor()
      assert.deepEqual(await cardSays(page), ["Auto-resets", "in 1d"], "the card by a tap")
      const [word, card] = await Promise.all([chipWord(page).boundingBox(), chipCard(page).locator("[data-side]").first().boundingBox()])
      assert.ok(card.y >= word.y + word.height, "under the word")
      await chipWord(page).tap()
      await chipCard(page).waitFor({ state: "detached" })
      await page.waitForTimeout(300)
      assert.equal(await menu(page).isVisible(), true, "the menu stays open")
      assert.equal(unpicks().length + anyPress().length, 0, "two taps on the word clear nothing and pick nothing")
    } finally {
      await close()
    }
  })

  for (const engine of ENGINES) {
    test(`17. in ${engine}, at 1280 and 390 px, with a card of the menu's rows open, the chip's or Stability's, the first tap on a server's name picks it`, async () => {
      const ways = [["the chip's", "Auto-resets", (page) => chipWord(page)], ["Stability's", "Failures", (page) => row(page, "Paris").locator("[data-slot=bars]")]]
      for (const width of [1280, 390]) for (const [whose, says, cell] of ways) {
        owners()
        prefer("group", AURORA, "l-ams")
        const picked = presses("l-fra").length
        const { page, close } = await open("", { width, engine, touch: true })
        try {
          await line(page, "EU").tap()
          await settledMenu(page)
          await cell(page).tap()
          await until(async () => (await openCards(page)).includes(says), `a tap opens ${whose} card`)
          await row(page, "Frankfurt").locator("[data-slot=option-name]").tap()
          await until(() => presses("l-fra").length === picked + 1, `at ${width} px the first tap on Frankfurt's name, ${whose} card open, to pick it`, 2_000)
          await menu(page).waitFor({ state: "detached" })
        } finally {
          await close()
        }
      }
    })

    test(`17. in ${engine}, at 1280 and 390 px, once the chip has gone with its card open, its pick run out or cleared from elsewhere, the first tap on a server's name picks it`, async () => {
      for (const width of [1280, 390]) for (const ending of ["run out", "cleared"]) {
        owners()
        prefer("group", AURORA, "l-ams")
        const picked = presses("l-par").length
        const { page, close } = await open("", { width, engine, touch: true })
        try {
          await line(page, "EU").tap()
          await settledMenu(page)
          await chipWord(page).tap()
          await chipCard(page).waitFor()
          prefer("group", AURORA, ending === "run out" ? "l-ams" : null, 1 / 3600)
          await chip(page).waitFor({ state: "detached" })
          await chipCard(page).waitFor({ state: "detached" })
          await row(page, "Paris").locator("[data-slot=option-name]").tap()
          await until(() => presses("l-par").length === picked + 1, `at ${width} px, the pick ${ending}, the first tap on Paris's name to pick it`, 2_000)
        } finally {
          await close()
        }
      }
    })
  }

  test("17. at 1280 and 390 px, a chip that goes with its card open, its pick run out or cleared from elsewhere, takes its card with it: the pick made again opens no card by itself, and a tap on its word opens it", async () => {
    for (const width of [1280, 390]) for (const ending of ["run out", "cleared"]) {
      owners()
      prefer("group", AURORA, "l-ams")
      const { page, close } = await open("", { width, touch: true })
      try {
        await line(page, "EU").tap()
        await settledMenu(page)
        await chipWord(page).tap()
        await chipCard(page).waitFor()
        prefer("group", AURORA, ending === "run out" ? "l-ams" : null, 1 / 3600)
        await chip(page).waitFor({ state: "detached" })
        prefer("group", AURORA, "l-ams")
        await chipWord(page).waitFor()
        await page.waitForTimeout(400)
        assert.deepEqual(await openCards(page), [], `at ${width} px, the pick ${ending} and made again, no card opens by itself`)
        await chipWord(page).tap()
        await chipCard(page).waitFor()
      } finally {
        await close()
      }
    }
  })

  /** The first words of every card open on the page, the menu aside: "Auto-resets" for the chip's, "Failures" for Stability's. */
  const openCards = (page) => page.evaluate(() => [...document.querySelectorAll("[data-radix-popper-content-wrapper]")]
    .filter((w) => !w.querySelector("[cmdk-list]")).map((w) => w.innerText.split("\n")[0].trim()))
  /** Long enough for a card a hover was going to open or close to have done it. */
  const HOVER_SETTLES_MS = 1000
  const settledMenu = (page) => until(() => menu(page).evaluate((el) => el.getAnimations({ subtree: true }).length === 0), "the menu to settle")

  for (const engine of ENGINES) {
    test(`17. in ${engine}, at 1280 and 390 px, the chip's card is one of the menu's cards: with the word focused the pointer on Stability leaves Stability's card alone open, and with Stability's open the focus on the word leaves the chip's alone`, async () => {
      owners()
      prefer("group", AURORA, "l-ams")
      for (const width of [1280, 390]) {
        const { page, close } = await open("", { width, engine })
        try {
          await line(page, "EU").click()
          await settledMenu(page)
          await chipWord(page).focus()
          await chipCard(page).waitFor()
          await row(page, "Amsterdam").locator("[data-slot=bars]").hover()
          await until(async () => (await openCards(page)).includes("Failures"), "the pointer opens Stability's card")
          await page.waitForTimeout(HOVER_SETTLES_MS)
          assert.deepEqual(await openCards(page), ["Failures"], `at ${width} px the word focused, then the pointer on Stability: Stability's card alone`)
          await menu(page).locator("[cmdk-input]").focus()
          await chipWord(page).focus()
          await chipCard(page).waitFor()
          await page.waitForTimeout(HOVER_SETTLES_MS)
          assert.deepEqual(await openCards(page), ["Auto-resets"], `at ${width} px Stability's card open, then the focus on the word: the chip's card alone`)
        } finally {
          await close()
        }
      }
    })

    test(`17. in ${engine}, at 1280 and 390 px, a tap on a card of the menu's rows closes it and does nothing else, however it opened: the chip's by hover, focus or a tap, Stability's by hover or a tap`, async () => {
      owners()
      prefer("group", AURORA, "l-ams")
      for (const width of [1280, 390]) {
        const { page, close } = await open("", { width, engine, touch: true })
        const bars = () => row(page, "Paris").locator("[data-slot=bars]")
        const ways = [
          ["the chip's", "Auto-resets", "hover", () => chipWord(page).hover()],
          ["the chip's", "Auto-resets", "focus", () => chipWord(page).focus()],
          ["the chip's", "Auto-resets", "a tap", () => chipWord(page).tap()],
          ["Stability's", "Failures", "hover", () => bars().hover()],
          ["Stability's", "Failures", "a tap", () => bars().tap()],
        ]
        try {
          await line(page, "EU").tap()
          await settledMenu(page)
          for (const [whose, says, how, opens] of ways) {
            await opens()
            await until(async () => (await openCards(page)).includes(says), `${whose} card by ${how}`)
            const card = await page.locator("[data-radix-popper-content-wrapper]").filter({ hasText: says }).locator("[data-side]").first().boundingBox()
            const [x, y] = [card.x + 8, card.y + card.height / 2]
            if (how === "hover") {
              await page.mouse.move(x, y)
              await page.waitForTimeout(HOVER_SETTLES_MS)
            }
            await page.touchscreen.tap(x, y)
            await until(async () => (await openCards(page)).length === 0, `at ${width} px a tap on ${whose} card opened by ${how} to close it`)
            await page.waitForTimeout(300)
            assert.equal(anyPress().length + unpicks().length, 0, `at ${width} px a tap on ${whose} card opened by ${how} picks nothing and clears nothing`)
            assert.equal(await menu(page).isVisible(), true, "the menu stays open")
            await page.mouse.move(0, 0)
          }
        } finally {
          await close()
        }
      }
    })
  }

  test("17. a pick on a failing server reopens a closed source on it, and the box's dot reads the record that heard the pass", async () => {
    owners()
    const lab = daemon.slot("lab")
    daemon.set({
      slots: new Map(daemon.slots().map((x) => [x.id, x.id === "lab" ? { ...lab, status: "failed", downReason: "confirmedDead", server: null, appliedServer: null, runtime: { ...lab.runtime, applied: { kind: "direct" } } } : x])),
      catalogue: daemon.catalogue().map((l) => (l.source === "aurora" ? { ...l, health: { ...l.health, verdict: "failed" } } : l)),
    })
    const { page, close } = await open("")
    try {
      await line(page, "Lab").click()
      await row(page, "Paris").click()
      await menu(page).waitFor({ state: "detached" })
      await line(page, "Lab").getByText("Paris").waitFor()
      const dot = await tile(page, "Lab").getByRole("img").first().getAttribute("aria-label")
      assert.doesNotMatch(dot, /Offline|Not responding/, "one pass leaves the record recovering, which the view gives as succeeded")
    } finally {
      await close()
    }
  })

  test("17. a press the daemon refuses because the server failed after its check began says the server failed", async () => {
    owners()
    const { page, close } = await open("")
    try {
      await page.route((url) => url.pathname.endsWith(`/preferred/l-par`), (route) => route.fulfill({
        status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" },
        body: JSON.stringify({ kind: "validation", reason: "failedSince", message: "That server failed after its test began." }),
      }), { times: 1 })
      await line(page, "EU").click()
      await row(page, "Paris").click()
      await toast(page, "failed").waitFor()
      assert.equal((await toast(page, "failed").innerText()).replace(/\s+/g, " ").trim(), "Paris failed")
    } finally {
      await close()
    }
  })

  test("17. a press a later press or a Clear superseded is 412 stale: Changed in another tab in a toast, nothing moves, the menu stays closed, and the line takes another pick", async () => {
    owners()
    const { page, close } = await open("")
    try {
      await page.route((url) => url.pathname.endsWith(`/preferred/l-par`), (route) => route.fulfill({
        status: 412, contentType: "application/json", headers: { "Sidegate-Revision": "1", ETag: '"1"' },
        body: JSON.stringify({ kind: "staleRevision", reason: "stale", message: "Changed by another editor.", revision: 1 }),
      }), { times: 1 })
      await line(page, "EU").click()
      await row(page, "Paris").click()
      await toast(page, "changed in another tab").waitFor()
      for (const slot of ["EU", "Home"]) assert.match(await line(page, slot).innerText(), /Amsterdam/)
      for (const slot of ["EU", "Home"]) assert.equal(await line(page, slot).locator("svg.lucide-chevron-down").count(), 1)
      await menu(page).waitFor({ state: "detached" })
      await painted(page)
      assert.equal(await menu(page).count(), 0, "a refusal opens no menu")
      await line(page, "EU").click()
      assert.equal(await busy(row(page, "Paris")), false, "another pick is one row away")
      assert.equal(await row(page, "Paris").getByRole("img", { name: /Failed|Not run/ }).count(), 0, "a refusal is no Test's answer")
    } finally {
      await close()
    }
  })

})

describe("the list", () => {
  test("17. every row in the list, Add among them, spans the list with 8 px at its left and its right, from Dashboard to Settings, whether or not the list scrolls, whatever the scrollbar, at 360, 768 and 1280 px", async () => {
    const many = new Map(Array.from({ length: 30 }, (_, i) => [`s${i}`, { ...source(1), id: `s${i}`, name: `Source ${i}`, stored: 0 }]))
    for (const width of [360, 768, 1280]) for (const bar of ["a mouse's scrollbar", "a floating scrollbar"]) {
      const seen = []
      for (const [list, seed] of [["short", null], ["scrolling", many]]) {
        const label = `${list}, ${bar}`
        daemon.reset()
        if (seed) daemon.set({ sources: seed })
        const { page, close } = await open("", { width })
        try {
          /* A mouse's scrollbar takes room, where this browser's own floats over the list. */
          if (bar === "a mouse's scrollbar") await page.addStyleTag({ content: "::-webkit-scrollbar { width: 15px } ::-webkit-scrollbar-thumb { background: gray }" })
          if (width === 360) await page.getByRole("button", { name: "Menu" }).click()
          await nav(page).getByRole("link", { name: /All servers/ }).waitFor()
          const viewport = nav(page).locator("[data-slot=scroll-area-viewport]")
          assert.equal(await viewport.evaluate((el) => el.scrollHeight > el.clientHeight), seed !== null, `the ${label} list ${seed ? "scrolls" : "fits"} at ${width} px`)
          await viewport.hover()
          await until(async () => (await nav(page).locator("[data-slot=scroll-area-scrollbar]").count()) === (seed ? 1 : 0), `a scrollbar over the ${label} list ${seed ? "shows" : "doesn't show"} at ${width} px`)
          const entries = await nav(page).locator("a[data-slot=item], button:text-is('Add')").evaluateAll((els) => els.map((el) => {
            const [box, frame] = [el.getBoundingClientRect(), el.closest("nav").getBoundingClientRect()]
            return `${el.textContent.trim().split(/\d/)[0].trim()} ${Math.round(box.left - frame.left)}..${Math.round(box.right - frame.left)} ${Math.round(box.left - frame.left)}|${Math.round(frame.right - box.right)}`
          }))
          assert.ok(entries.some((e) => e.startsWith("Add ")), "Add is measured")
          for (const entry of entries) assert.ok(entry.endsWith(" 8|8"), `8 px at both sides at ${width} px, ${label}: ${entry}`)
          const spans = new Set(entries.map((e) => e.split(" ").at(-2)))
          assert.equal(spans.size, 1, `one width at ${width} px, ${label}: ${entries.join(", ")}`)
          seen.push(...spans)
        } finally {
          await close()
        }
      }
      assert.equal(new Set(seen).size, 1, `a scrolling list keeps the width of one that fits at ${width} px with ${bar}: ${seen.join(" vs ")}`)
    }
  })
})

describe("the list folded away", () => {
  test("17. on a phone the Menu button carries the list's worst dot, and Menu opens the list at the first entry whose dot has that colour", async () => {
    seedTroubled()
    const { page, close } = await open("", { width: 360 })
    try {
      const menu = page.getByRole("button", { name: "Menu" })
      await until(async () => (await menu.locator("span.bg-destructive").count()) === 1, "the Menu button's dot is red")
      await menu.click()
      const viewport = nav(page).locator("[data-slot=scroll-area-viewport]")
      const first = nav(page).locator("[data-rating=bad]").first()
      await first.waitFor()
      await until(async () => {
        const [entry, frame] = await Promise.all([first.boundingBox(), viewport.boundingBox()])
        return entry.y >= frame.y && entry.y + entry.height <= frame.y + frame.height
      }, "the first red entry is in sight")
      assert.ok(await viewport.evaluate((el) => el.scrollTop > 0), "the list opened scrolled to it")
    } finally {
      await close()
    }
  })
})

describe("a source's heading", () => {
  for (const theme of ["dark", "light"]) {
    test(`17. each state reads as written in the heading, each trouble in its colour, at 360, 768 and 1280 px, ${theme}`, async () => {
      for (const width of [360, 768, 1280]) {
        const t = seedTroubled()
        const states = STATES
        const { page, close } = await open("sources/steady", { width, theme })
        try {
          for (const id of t.ids) {
            const want = states[id]
            const name = t.sources.find((x) => x.id === id).name
            await page.goto(daemon.url + `sources/${id}`)
            const heading = headingOf(page)
            await heading.getByRole("button", { name: `Rename ${name}` }).waitFor()
            if (want.week !== false) await weekButton(page).waitFor()
            const shown = await shownWords(heading.getByRole("heading"))
            assert.ok(shown === name || (shown.endsWith("…") && name.startsWith(shown.slice(0, -1))), `${id} at ${width} px: its name reads ${shown}`)
            /* A failing refresh's age waits for its refreshes. */
            const after = async () => (await shownWords(heading)).slice(shown.length + 1)
            await until(async () => (await after()) === want.words, `${id} at ${width} px ${theme}: ${want.words}`).catch(() => {})
            assert.equal(await after(), want.words, `${id} at ${width} px ${theme}`)
            assert.equal(await heading.getByRole("img", { name: /^(Online|Unstable|Offline|Unchecked|Refreshing…)$/ }).count(), 1, `${id}: one dot, the faint Pending one while the first refresh runs`)
            if (id === "new") {
              const refresh = heading.getByRole("button", { name: "Refreshing…" })
              assert.deepEqual([await refresh.locator("[data-slot=spinner]").count(), await refresh.locator("svg.lucide-refresh-cw").count(), await refresh.isDisabled()], [1, 0, true], "while the first refresh runs Refresh is Pending as any button whose action is out: the spinner, and no press")
            }
            await heading.getByRole("img", { name: want.dot, exact: true }).waitFor()
            for (const [words, tone] of Object.entries(want.tones)) assert.equal(await toneOf(heading.getByText(words, { exact: true }).first()), tone, `${id} at ${width} px ${theme}: ${words}`)
            assert.equal(await heading.locator("svg.lucide-circle-alert, svg.lucide-circle-x").count(), 0, `${id}: no mark beside a trouble`)
            assert.doesNotMatch(await shownWords(heading), /\d (m|h|d|mo|y)\b/, `${id}: no time has a space before its unit`)
            assert.ok(await page.evaluate(() => document.scrollingElement.scrollWidth <= innerWidth), `${id} at ${width} px: no sideways scrolling`)
          }
        } finally {
          await close()
        }
      }
    })
  }

  for (const theme of ["dark", "light"]) {
    test(`17. the list shows each source's dot, name and count, and under them only its troubles in words with no mark and no bar, in the heading's words, colours and order, at 360, 768 and 1280 px, ${theme}`, async () => {
      for (const width of [360, 768, 1280]) {
        const t = seedTroubled()
        /* Limit with its expiry 3 days off: amber before red in the heading's order, and the list keeps that order. */
        const limit = daemon.source("limit")
        daemon.set({ sources: new Map(daemon.sources().map((x) => [x.id, x.id === "limit" ? { ...limit, origin: { ...limit.origin, provider: { ...limit.origin.provider, quota: { ...limit.origin.provider.quota, expires: new Date(Date.now() + 3 * 864e5).toISOString() } } } } : x])) })
        const states = STATES
        const { page, close } = await open("servers", { width, theme })
        try {
          if (width === 360) await page.getByRole("button", { name: "Menu" }).click()
          const list = nav(page)
          await list.locator('a[href$="/groups/shaky"]').waitFor()
          const listTones = (entry) => entry.locator("[data-slot=troubles]").evaluate((el) => {
            const token = (name) => { const probe = document.createElement("span"); probe.style.color = `var(${name})`; document.body.append(probe); const c = getComputedStyle(probe).color; probe.remove(); return c }
            const [bad, warn, muted] = [token("--destructive"), token("--warning"), token("--muted-foreground")]
            const tone = (e) => ({ [bad]: "bad", [warn]: "warn", [muted]: "muted" })[getComputedStyle(e).color] ?? getComputedStyle(e).color
            const words = [...el.querySelectorAll(":scope > span")].map((unit) => [unit.firstElementChild.textContent, tone(unit.firstElementChild)])
            const dots = [...el.querySelectorAll(":scope > span")].slice(0, -1).map((unit) => unit.lastChild.textContent === " ·" && tone(unit))
            return { words, dots }
          })
          for (const id of t.ids) {
            const want = states[id]
            const entry = list.locator(`a[href$="/sources/${id}"]`)
            const words = id === "limit" ? "3 Expires in 3d · 0 GB left · Refresh failing" : want.list
            assert.equal(await shownWords(entry), [t.sources.find((x) => x.id === id).name, words].filter(Boolean).join(" "), `${id}'s list entry at ${width} px`)
            await entry.getByRole("img", { name: want.dot, exact: true }).waitFor()
            assert.equal(await entry.locator("[data-slot=spinner]").count(), want.list === "" ? 1 : 0, `${id}: a spinner where its count goes while its first refresh runs`)
            assert.equal(await entry.locator("svg:not([data-slot=spinner])").count(), 0, `${id}: no mark in the list`)
            assert.equal(await entry.locator("[data-slot=progress]").count(), 0, `${id}: no bar`)
            if (!/[A-Za-z]/.test(words)) continue
            const { words: troubles, dots } = await listTones(entry)
            assert.ok(dots.every((tone) => tone === "muted"), `${id}: each " ·" is muted and ends its trouble`)
            const heading = { ...want.tones, ...want.listTones, ...(id === "limit" && { "Expires in 3d": "warn" }) }
            for (const [trouble, tone] of troubles) assert.equal(tone, heading[trouble], `${id}: ${trouble} in the heading's colour`)
          }
          assert.equal(await shownWords(list.locator('a[href$="/groups/shaky"]')), "Shaky 3 8 failovers in 7d", "a group's entry names its own troubles")
          assert.deepEqual((await listTones(list.locator('a[href$="/groups/shaky"]'))).words.map(([, tone]) => tone), ["warn"], "a group's amber trouble is amber")
          assert.equal(await shownWords(list.getByText("Sources", { exact: true })), "Sources", "the section's heading is its name alone")
          assert.equal(await list.getByText("servers", { exact: true }).count(), 0, "with no word for the count")
          assert.ok(await page.evaluate(() => document.scrollingElement.scrollWidth <= innerWidth), `${width} px: no sideways scrolling`)
        } finally {
          await close()
        }
      }
    })
  }

  test("17. until the server list lands every entry's count is one fixed-size spinner and none takes a servers trouble; once its read failed nothing in the list waits, each entry's count the muted dash, the failure in the page and on All servers as the muted cross whose card holds the reason and Retry, at 1280 and 390 px", async () => {
    seedTroubled()
    const shaky = daemon.group("shaky")
    daemon.set({ groups: new Map([["shaky", shaky], ["void", { ...shaky, id: "void", name: "Void", resolved: [], firstChoice: null, moves7d: 0 }]]) })
    const entries = (page) => nav(page).locator('a[href$="/servers"], a[href*="/sources/"], a[href*="/groups/"]')
    const counts = (page) => entries(page).evaluateAll((links) => links.map((a) => {
      const title = a.querySelector("[data-slot=item-title]")
      const spinner = title.querySelector("[data-slot=spinner]")
      const box = spinner && getComputedStyle(spinner)
      return { name: title.firstElementChild.textContent, count: spinner ? `spinner ${box.width}x${box.height}` : title.lastElementChild.textContent, troubles: a.querySelector("[data-slot=troubles]")?.textContent ?? "" }
    }))
    let release
    const held = new Promise((done) => { release = done })
    const { page, close } = await open("events")
    try {
      await page.route("**/api/servers?*", async (r) => { await held; await r.continue() })
      await page.reload()
      await nav(page).locator('a[href$="/groups/void"]').waitFor()
      const waiting = await counts(page)
      assert.equal(waiting.length, troubledSources().sources.length + 3, "All servers, every source and both groups")
      assert.deepEqual([...new Set(waiting.map((e) => e.count))], ["spinner 12pxx12px"], "every count, a group's and the longest name's too, is the same spinner")
      assert.ok(waiting.every((e) => !/No servers?( up)?\b/.test(e.troubles)), `no entry reads as having no servers: ${JSON.stringify(waiting.filter((e) => /No server/.test(e.troubles)))}`)
      release()
      await until(async () => (await nav(page).locator("[data-slot=spinner]").count()) === troubledSources().sources.filter((x) => x.origin.kind === "remote" && x.origin.fetch.updatedAt === null && x.origin.fetch.failures === 0).length, "the counts landed")
      const [shaky, empty] = ["Shaky", "Void"].map((name) => counts(page).then((all) => all.find((e) => e.name === name)))
      assert.match((await empty).troubles, /^No servers$/, "a group with none reads so once the list is read")
      assert.equal((await empty).count, "Void", "with no count beside its name, since its words say it")
      assert.equal((await shaky).count, "3", "a group with servers keeps its count")
    } finally {
      await close()
    }
    daemon.set({ readFails: { "api/servers": 10 } })
    const failed = await open("servers")
    try {
      await failed.page.getByRole("main").getByRole("button", { name: "Retry" }).waitFor()
      await failed.page.waitForTimeout(200)
      assert.equal(await nav(failed.page).locator("[data-slot=spinner], [data-slot=skeleton]").count(), 0, "nothing in the list waits for a read that has failed")
      const shown = await counts(failed.page)
      assert.deepEqual([...new Set(shown.filter((e) => e.name !== "All servers").map((e) => e.count))], ["—"], "every entry's count is the muted dash")
      assert.ok(shown.every((e) => !/No server/.test(e.troubles)), "and no entry reads as having no servers")
      assert.equal(await failed.page.getByRole("alert").count(), 1, "the failure stands once in the page")
      for (const width of [1280, 390]) {
        await failed.page.setViewportSize({ width, height: 844 })
        if (width < 768) await failed.page.getByRole("button", { name: "Menu" }).click()
        const list = failed.page.locator("nav[aria-label=Main]:visible")
        const mark = list.getByRole("button", { name: "All servers: HTTP 502", exact: true })
        await mark.waitFor()
        assert.equal(await toneOf(mark), "muted", `${width} px: All servers carries the muted cross`)
        const link = await list.locator('a[href$="/servers"]').elementHandle()
        const gap = await mark.locator("svg").evaluate((svg, a) => a.getBoundingClientRect().right - svg.getBoundingClientRect().right, link)
        await link.dispose()
        assert.ok(Math.abs(gap - 10) < 2, `${width} px: in the count's place, ${gap} px from the entry's right edge`)
        await mark.click()
        const why = card(failed.page)
        await why.getByRole("button", { name: "Retry" }).waitFor()
        assert.equal((await why.innerText()).replace(/\s+/g, " ").trim(), "HTTP 502 Retry", `${width} px: its card holds the reason and Retry`)
        assert.equal(await toneOf(why.getByText("HTTP 502", { exact: true })), "muted", `${width} px: the reason muted`)
        await failed.page.keyboard.press("Escape")
        if (width < 768) await failed.page.keyboard.press("Escape")
      }
      await failed.page.setViewportSize({ width: 1440, height: 900 })
      const line = failed.page.locator("[data-slot=table-line]")
      const [row, retryAt] = [await line.boundingBox(), await line.getByRole("button", { name: "Retry" }).boundingBox()]
      assert.ok(Math.abs(row.x + row.width - 8 - (retryAt.x + retryAt.width)) < 2, `Retry at the row's right end, as a page's failed row has it: ${JSON.stringify({ row, retryAt })}`)
      daemon.set({ readFails: {} })
      await failed.page.getByRole("main").getByRole("button", { name: "Retry" }).click()
      await nav(failed.page).locator('a[href$="/servers"]').getByText(String(troubledSources().servers.filter((x) => x.presence !== "tombstone").length), { exact: true }).waitFor()
    } finally {
      await failed.close()
    }
  })

  /** The phone's list in its sheet: its scroller, and where a row stands against the edges the list fades. */
  const sheetList = async (page) => {
    /* Short enough that the list scrolls whatever its entries say. */
    await page.setViewportSize({ width: 390, height: 520 })
    await page.getByRole("button", { name: "Menu" }).click()
    const list = page.locator("[data-slot=sheet-content] nav[aria-label=Main]")
    const viewport = list.locator("[data-slot=scroll-area-viewport]")
    await viewport.waitFor()
    await list.locator('a[href*="/groups/"]').first().waitFor()
    const place = (row) => viewport.evaluate((v, r) => {
      const [a, b] = [r.getBoundingClientRect(), v.getBoundingClientRect()]
      return { scrollTop: v.scrollTop, overflows: v.scrollHeight > v.clientHeight + 1, top: a.top - b.top, bottom: b.bottom - a.bottom, room: v.scrollHeight - v.clientHeight - v.scrollTop }
    }, row)
    return { list, viewport, place }
  }

  test("17. at 390 px the sheet opens at the first entry with the Menu's worst dot, and once the server list's read has failed it opens at its top, All servers and its cross in sight", async () => {
    seedTroubled()
    {
      const { page, close } = await open("events", { width: 390 })
      try {
        const { list, place } = await sheetList(page)
        const allServers = list.locator('a[href$="/servers"]')
        const handle = await allServers.elementHandle()
        await until(async () => (await place(handle)).scrollTop > 0, "the list scrolled to its first red entry")
      } finally {
        await close()
      }
    }
    daemon.set({ readFails: { "api/servers": 10 } })
    const { page, close } = await open("servers", { width: 390 })
    try {
      await page.getByRole("main").getByRole("button", { name: "Retry" }).waitFor()
      const { list, place } = await sheetList(page)
      const mark = list.getByRole("button", { name: "All servers: HTTP 502", exact: true })
      await mark.waitFor()
      await page.waitForTimeout(300)
      const at = await place(await list.locator('a[href$="/servers"]').elementHandle())
      assert.ok(at.overflows, "the list is longer than the sheet")
      assert.equal(at.scrollTop, 0, "the sheet opens at its top")
      assert.ok(at.top >= 0, "All servers stands in sight")
    } finally {
      await close()
    }
  })

  test("17. at 390 px a row whose card is open stands clear of the list's faded edge, so it keeps its full ink", async () => {
    seedTroubled()
    daemon.set({ readFails: { "api/servers": 10 } })
    const { page, close } = await open("events", { width: 390 })
    try {
      const { list, viewport, place } = await sheetList(page)
      const mark = list.getByRole("button", { name: "All servers: HTTP 502", exact: true })
      await mark.waitFor()
      const row = await list.locator("[data-slot=card-row]").elementHandle()
      /* All servers half under the top edge's fade, its cross still in sight. */
      await viewport.evaluate((v) => { v.scrollTop = 14 })
      await until(async () => { const at = await place(row); return at.top < 32 && at.scrollTop > 0 }, `the row under the top edge's fade: ${JSON.stringify(await place(row))}`)
      await mark.click()
      await card(page).getByRole("button", { name: "Retry" }).waitFor()
      await page.waitForTimeout(200)
      const at = await place(row)
      assert.ok(at.scrollTop === 0 || at.top >= 32 - 1, `clear of the top edge's fade, or no fade there: ${JSON.stringify(at)}`)
      assert.ok(at.room <= 1 || at.bottom >= 32 - 1, `clear of the bottom edge's fade: ${JSON.stringify(at)}`)
      assert.equal(await mark.getAttribute("aria-expanded"), "true", "its card stays open")
    } finally {
      await close()
    }
  })

  test("17. once the view's read has failed the list waits for nothing: no entry skeletons and All servers' count the muted dash", async () => {
    daemon.set({ badShape: true })
    const { page, close } = await open("")
    try {
      await page.getByRole("button", { name: "Reload" }).waitFor()
      await page.waitForTimeout(200)
      assert.equal(await nav(page).locator("[data-slot=spinner], [data-slot=skeleton], [data-slot=list-waiting]").count(), 0, "no spinner and no entry in its waiting shape")
      assert.equal(await shownWords(nav(page).locator('a[href$="/servers"]')), "All servers —")
    } finally {
      await close()
    }
  })

  test("17. a heading with server trouble, expiry, traffic and a week holds one centre line at 1280 px, the bar and the week beside their words, and at 1024 and 390 px Refresh and the menu stay on the name's line with the facts under it, in both themes", async () => {
    /** Steady with every server down, and with or without the provider's Account and Support. */
    const seed = (links) => {
      const t = seedTroubled()
      const steady = daemon.source("steady")
      const provider = links ? steady.origin.provider : { ...steady.origin.provider, profileUrl: "", supportUrl: "" }
      daemon.set({
        catalogue: t.servers.map((x) => (x.source === "steady" ? { ...x, health: { ...x.health, verdict: "failed" } } : x)),
        sources: new Map(daemon.sources().map((x) => [x.id, x.id === "steady" ? { ...steady, origin: { ...steady.origin, provider } } : x])),
      })
    }
    for (const theme of ["dark", "light"]) {
      for (const [links, width] of [[false, 1280], [true, 1280], [false, 1024], [false, 390]]) {
        seed(links)
        const { page, close } = await open("sources/steady", { width, theme })
        const where = `${theme} ${width} px${links ? " with Account and Support" : ""}`
        try {
          const heading = headingOf(page)
          await weekButton(page).waitFor()
          await heading.getByText("No server up", { exact: true }).waitFor()
          const boxes = await heading.evaluate((el) => {
            const box = (e) => { const r = e.getBoundingClientRect(); return { top: r.top, bottom: r.bottom, right: r.right, centre: (r.top + r.bottom) / 2, height: r.height } }
            return {
              name: box(el.querySelector("h2")),
              dot: box(el.querySelector("[role=img]")),
              marks: [...el.querySelectorAll("[data-slot=source-facts] span[aria-hidden], [data-slot=week-strip]")].filter((m) => m.getBoundingClientRect().width >= 64).map(box),
              facts: [...el.querySelector("[data-slot=source-facts]").children].map(box),
              actions: [...el.querySelectorAll("button, a")].filter((b) => b.offsetWidth > 0 && /^(Refresh|More for|Settings for|Delete|Account|Support)\b/.test(b.ariaLabel ?? b.textContent)).map(box),
              heading: box(el),
            }
          })
          const onNameLine = (b) => Math.abs(b.top - boxes.name.top) <= 6
          assert.ok(boxes.actions.length >= 2 && boxes.actions.every(onNameLine), `${where}: Refresh and the actions are on the name's line`)
          assert.ok(Math.max(...boxes.actions.map((b) => b.right)) >= boxes.heading.right - 1, `${where}: the actions end the line`)
          if (width === 1280 && !links) {
            assert.ok(boxes.facts.every(onNameLine), `${where}: the facts sit between the name and the actions`)
            const centres = [boxes.dot, ...boxes.facts, ...boxes.actions, ...boxes.marks].map((b) => b.centre)
            assert.ok(Math.max(...centres) - Math.min(...centres) <= 1.5, `${where}: the dot, the facts, the bar, the week, Refresh and the actions share one centre line: ${centres.join(", ")}`)
            assert.equal(boxes.marks.length, 2, `${where}: the bar and the week's strip stand on the line`)
            assert.ok(boxes.facts.every((b) => b.height <= 28), `${where}: no fact takes a second line`)
          }
          if (width < 1280) assert.ok(boxes.facts.every((b) => b.top >= boxes.name.bottom), `${where}: every fact is under the name`)
          assert.ok(await page.evaluate(() => document.scrollingElement.scrollWidth <= innerWidth), `${where}: no sideways scrolling`)
        } finally {
          await close()
        }
      }
    }
  })

  test("17. Delete on a group's heading is the source's Delete: the plain outlined icon, not red, with no ground of its own", async () => {
    const { page, close } = await open("sources/aurora", { width: 1280 })
    const look = (button) => button.evaluate((el) => {
      const style = getComputedStyle(el)
      return { variant: el.dataset.variant, color: style.color, ground: style.backgroundColor, border: style.borderColor, radius: style.borderRadius, size: `${el.offsetWidth}x${el.offsetHeight}` }
    })
    try {
      const ofSource = page.getByRole("button", { name: "Delete Aurora" })
      await ofSource.waitFor()
      const source = await look(ofSource)
      await page.goto(daemon.url + "groups/all-aurora")
      const ofGroup = page.getByRole("button", { name: "Delete All Aurora" })
      await ofGroup.waitFor()
      assert.deepEqual(await look(ofGroup), source, "one form on both headings")
      assert.equal(source.variant, "outline")
      assert.notEqual(await toneOf(ofGroup), "bad", "Delete isn't red")
    } finally {
      await close()
    }
  })

  test("17. from 880 px a source's actions are Refresh, Settings, Account, Support, then Delete, not red; below, Refresh and a menu of them; a pasted source keeps Settings and Delete", async () => {
    seedTroubled()
    const actions = (page) => headingOf(page).locator("button:visible, a:visible").evaluateAll((els) => els.map((el) => el.getAttribute("aria-label") ?? el.textContent.trim()))
    const wide = await open("sources/steady", { width: 1280 })
    try {
      const heading = headingOf(wide.page)
      await heading.getByRole("link", { name: "Account" }).waitFor()
      const shown = (await actions(wide.page)).filter((n) => !/^(Rename|Expires|80\.0|2 failovers|Maintenance)/.test(n))
      assert.deepEqual(shown, ["Refresh, last worked 24 minutes ago", "Settings for Steady", "Account", "Support", "Delete Steady"])
      assert.equal(await heading.getByRole("link", { name: "Account" }).getAttribute("href"), "https://panel.example/me")
      const look = (control) => control.evaluate((el) => { const { color, backgroundColor, borderColor } = getComputedStyle(el); return { color, backgroundColor, borderColor } })
      const settings = await look(heading.getByRole("button", { name: "Settings for Steady" }))
      for (const name of ["Account", "Support"]) assert.deepEqual(await look(heading.getByRole("link", { name })), settings, `${name} looks like Settings`)
      assert.notEqual(await toneOf(heading.getByRole("button", { name: "Delete Steady" })), "bad", "Delete isn't red")
      await wide.page.goto(daemon.url + "sources/unlimited")
      await heading.getByRole("button", { name: "Delete Unlimited" }).waitFor()
      assert.equal(await heading.getByRole("link").count(), 0, "Account and Support show only when the provider sends them")
    } finally {
      await wide.close()
    }
    const narrow = await open("sources/steady", { width: 360 })
    try {
      const heading = headingOf(narrow.page)
      await heading.getByRole("button", { name: "More for Steady" }).click()
      assert.deepEqual(await narrow.page.getByRole("menuitem").allInnerTexts(), ["Settings", "Account", "Support", "Delete"])
      await narrow.page.getByRole("menuitem", { name: "Settings" }).click()
      await narrow.page.getByRole("dialog", { name: "Settings" }).getByLabel("URL", { exact: true }).waitFor()
      await narrow.page.keyboard.press("Escape")
      await narrow.page.goto(daemon.url + "sources/pasted")
      await heading.getByRole("button", { name: "Delete Pasted" }).waitFor()
      assert.equal(await heading.getByRole("button", { name: "Settings for Pasted" }).isVisible(), true, "a pasted source keeps Settings as a button")
      assert.equal(await heading.getByRole("button", { name: /^(More for |Refresh)/ }).count(), 0, "and has no Refresh and no menu")
    } finally {
      await narrow.close()
    }
  })

  test("17. a fact opens its card on hover and on keyboard focus, the dot opens none, and focus moves through the name, each fact with a card, Refresh and the actions", async () => {
    seedTroubled()
    const { page, close } = await open("sources/steady", { width: 1280 })
    try {
      const heading = headingOf(page)
      await weekButton(page).waitFor()
      await heading.getByRole("button", { name: "Rename Steady" }).focus()
      const order = []
      for (let i = 0; i < 9; i++) {
        order.push(await page.evaluate(() => {
          const el = document.activeElement
          const words = [...el.childNodes].filter((n) => !(n instanceof Element && n.classList.contains("sr-only"))).map((n) => n.textContent.trim()).filter(Boolean).join(" ")
          return el.getAttribute("aria-label") ?? words
        }))
        if (i === 1) assert.match(await card(page).innerText(), /^[A-Z][a-z]{2} \d{1,2}, \d\d:\d\d$/, "focus opens the expiry's card, its date and time, with no year inside 12 months")
        await page.keyboard.press("Tab")
      }
      const want = ["Rename Steady", "Expires in 40d", "80.0 GB", "2 failovers in the last 7 days, in use on 5 of them", "Refresh, last worked 24 minutes ago", "Settings for Steady", "Account", "Support", "Delete Steady"]
      assert.ok(order.every((w, i) => w.startsWith(want[i])), `focus moves ${order.join(" | ")}`)
      await page.mouse.move(0, 0)
      await heading.getByRole("button", { name: /^80\.0 GB left/ }).hover()
      await until(async () => /^80\.0 \/ 100 GB\n2\.0 GB\s*18\.0 GB\s*[A-Z][a-z]{2} \d{1,2}$/.test(await card(page).innerText().catch(() => "")), "the traffic's card on hover")
      assert.equal(await card(page).locator("svg").count(), 3, "sent, received and the refill each beside its sign")
      assert.match(await heading.getByRole("button", { name: /^80\.0 GB left/ }).evaluate((el) => el.textContent), /sent 2\.0 GB, received 18\.0 GB, resets [A-Z][a-z]{2} \d{1,2}/, "the signs are heard as words")
      await page.mouse.move(0, 0)
      await page.goto(daemon.url + "sources/soon")
      await heading.getByRole("img", { name: "Unstable" }).hover()
      await page.waitForTimeout(600)
      assert.equal(await page.locator("[data-radix-popper-content-wrapper]").count(), 0, "the dot opens no card: each reason stands beside it in the heading")
      assert.equal(await heading.getByRole("img", { name: "Unstable" }).evaluate((el) => el.tabIndex), -1, "the dot takes no focus")
    } finally {
      await close()
    }
  })

  test("17. on touch a tap opens a fact's card, and a second tap, a tap elsewhere or Esc closes it; a long press shows Refresh's card and refreshes nothing, and a tap refreshes", async () => {
    seedTroubled()
    const { page, context, close } = await open("sources/steady", { width: 360, touch: true })
    try {
      const heading = headingOf(page)
      const expiry = heading.getByRole("button", { name: /^Expires in 40d/ })
      const open = () => card(page).isVisible().catch(() => false)
      await expiry.tap()
      await until(open, "a tap opens the card")
      assert.equal((await expiry.boundingBox()).height >= 28, true, "its tap area is 28px tall")
      await expiry.tap()
      await until(async () => !(await open()), "a second tap closes it")
      await expiry.tap()
      await until(open, "a tap opens it again")
      await page.getByRole("table", { name: "Servers" }).tap({ position: { x: 10, y: 10 } })
      await until(async () => !(await open()), "a tap elsewhere closes it")
      await expiry.tap()
      await until(open, "and again")
      await page.keyboard.press("Escape")
      await until(async () => !(await open()), "Esc closes it")

      const refresh = refreshButton(page)
      const box = await refresh.boundingBox()
      const cdp = await context.newCDPSession(page)
      const at = [{ x: box.x + box.width / 2, y: box.y + box.height / 2 }]
      await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: at })
      await page.waitForTimeout(800)
      await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] })
      await until(async () => (await card(page).innerText().catch(() => "")).replace(/\s+/g, " ").trim() === "Next in 3h Refresh every 6h", "a long press shows Refresh's card, its next run and how often")
      await page.waitForTimeout(300)
      assert.equal(requests("POST", "api/sources/steady/refresh").length, 0, "a long press refreshes nothing")
      await page.getByRole("table", { name: "Servers" }).tap({ position: { x: 10, y: 10 } })
      await refresh.tap()
      await until(() => requests("POST", "api/sources/steady/refresh").length === 1, "a tap refreshes")
      assert.equal(await open(), false, "a tap shows no card")
    } finally {
      await close()
    }
  })

  test("17. on touch a long press the browser cancels takes no later tap from its button: the next tap refreshes", async () => {
    seedTroubled()
    const { page, context, close } = await open("sources/steady", { width: 360, touch: true })
    try {
      const refresh = refreshButton(page)
      await refresh.waitFor()
      const box = await refresh.boundingBox()
      const cdp = await context.newCDPSession(page)
      await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: box.x + box.width / 2, y: box.y + box.height / 2 }] })
      await page.waitForTimeout(800)
      await cdp.send("Input.dispatchTouchEvent", { type: "touchCancel", touchPoints: [] })
      await until(() => card(page).isVisible().catch(() => false), "the long press showed Refresh's card")
      await page.getByRole("table", { name: "Servers" }).tap({ position: { x: 10, y: 10 } })
      await refresh.tap()
      await until(() => requests("POST", "api/sources/steady/refresh").length === 1, "the tap refreshes")
    } finally {
      await close()
    }
  })

  test("17. on touch a tap opens a fact's card though the page is busy past a frame after the finger lifts, before the tap's click", async () => {
    seedTroubled()
    const { page, close } = await open("sources/steady", { width: 360, touch: true })
    try {
      const expiry = headingOf(page).getByRole("button", { name: /^Expires in 40d/ })
      await expiry.waitFor()
      await page.evaluate(() => window.addEventListener("pointerup", () => { const end = performance.now() + 40; while (performance.now() < end); }))
      for (let i = 0; i < 3; i++) {
        await page.evaluate(() => document.activeElement?.blur())
        await expiry.tap()
        await until(() => card(page).isVisible().catch(() => false), `tap ${i + 1} opens the card`, 3000)
        await page.keyboard.press("Escape")
        await until(async () => !(await card(page).isVisible().catch(() => false)), "Esc closes it")
      }
    } finally {
      await close()
    }
  })

  test("17. a long name takes three lines and is cut by the word, the pencil after its last letter, and renaming holds the whole name", async () => {
    const t = seedTroubled()
    const full = t.sources.find((x) => x.id === "expired").name
    const { page, close } = await open("sources/expired", { width: 360 })
    try {
      const name = headingOf(page).getByRole("button", { name: `Rename ${full}` })
      await name.waitFor()
      const { lines, text, pencilLast } = await name.evaluate((el) => {
        const lineHeight = parseFloat(getComputedStyle(el).lineHeight)
        const [box, pencil] = [el.getBoundingClientRect(), el.querySelector("svg").getBoundingClientRect()]
        return { lines: Math.round(box.height / lineHeight), text: el.querySelector("span").textContent, pencilLast: pencil.top >= box.bottom - lineHeight - 1 }
      })
      assert.equal(lines, 3)
      assert.ok(text.endsWith("…") && !/[,.([{-]…$/.test(text), `cut with nothing hanging: ${text}`)
      assert.ok(full.startsWith(text.slice(0, -1)) && /^[\s,]/.test(full.slice(text.length - 1)), `cut at a word: ${text}`)
      assert.ok(pencilLast, "the pencil is on the last line")
      await name.click()
      assert.equal(await page.getByRole("textbox", { name: `Name of ${full}` }).inputValue(), full)
    } finally {
      await close()
    }
  })

  test("17. the week's card is its readout, 7d over Failovers and In use each after its mark, over the strip's line with the seven days' letters, today's bold and last, the readout reading the six hours under the pointer and the band through the letters; a week never used opens its own; and its place is Pending until the read lands", async () => {
    seedTroubled()
    daemon.set({ readDelay: { "api/analytics": 1500 } })
    const { page, close } = await open("sources/steady", { width: 1280 })
    try {
      const heading = headingOf(page)
      const refresh = refreshButton(page)
      await refresh.waitFor()
      const before = (await refresh.boundingBox()).x
      const reading = heading.getByRole("status", { name: "Loading history…" })
      await reading.waitFor()
      assert.equal(await shownWords(reading), "7d", "while it is read, 7d stands with a skeleton the strip's width and a spinner where the count goes")
      assert.equal(Math.round((await reading.locator("[data-slot=skeleton]").boundingBox()).width), 100)
      await reading.locator("[data-slot=spinner]").waitFor()
      await weekButton(page).waitFor()
      assert.equal((await refresh.boundingBox()).x, before, "nothing moves when it lands")
      const strip = await weekButton(page).evaluate((el) => {
        const line = el.querySelector("[data-slot=week-strip]")
        const label = line.previousElementSibling
        return { width: line.getBoundingClientRect().width, gap: line.getBoundingClientRect().left - label.getBoundingClientRect().right, label: label.textContent }
      })
      assert.deepEqual(strip, { width: 100, gap: 8, label: "7d" }, "a muted 7d and the 100 px strip beside it")
      await weekButton(page).hover()
      await card(page).locator("[data-slot=chart-readout]").waitFor()
      const counts = await card(page).locator("[data-slot=chart-key]").evaluateAll((all) => all.map((k) =>
        `${k.querySelector(".border-dashed") ? "¦" : k.querySelector(".bg-muted-foreground") ? "—" : " "}${k.innerText.replace(/\s+/g, " ").trim()}`))
      assert.deepEqual(counts, ["¦Failovers 2", "—In use 2h"], "each count after the mark it counts, In use rounded down")
      assert.equal(await readoutOf(card(page)), "7d Failovers 2 In use 2h", "at rest the readout reads the week")
      const letters = await card(page).locator("[data-slot=day-letter]").evaluateAll((all) => all.map((t) => (t.classList.contains("font-semibold") ? `*${t.textContent}` : t.textContent)).join(" "))
      assert.match(letters, /^(\w\w ){6}\*\w\w$/, `seven days across, today last and bold: ${letters}`)
      assert.equal(new Set(letters.replace("*", "").split(" ")).size, 7, "each told apart from the others")
      assert.equal(await card(page).locator("[data-slot=week-failover]").count(), 2, "a dashed line at each six hours with a failover")
      assert.match(await weekPlace(page, { failover: 0 }), /^\w{3} \d\d:\d\d–\d\d:\d\d Failovers 1 In use 15m$/, "its six hours, then its failovers and its time in use")
      const band = await card(page).locator("[data-slot=week-cursor]").evaluate((r) => Number(r.getAttribute("height")))
      assert.equal(band, 44, "the band runs through the day letters")
      assert.match(await weekPlace(page, 2), /^\w{3} 00:00–06:00 Failovers 0 In use 0m$/, "six hours with nothing in them: their hours and nothing counted")
      await page.mouse.move(0, 0)
      daemon.set({ readDelay: {} })
      await page.goto(daemon.url + "sources/unreported")
      const bare = headingOf(page).getByRole("button", { name: "No failovers today, not in use" })
      await bare.hover()
      await card(page).locator("[data-slot=chart-readout]").waitFor()
      assert.equal(await readoutOf(card(page)), "7d Failovers 0 In use 0m", "a week never used opens its card too")
      assert.equal(await card(page).locator("[data-slot=day-letter]").count(), 7, "over its seven days")
    } finally {
      await close()
    }
  })

  test("17. in Dubai and Denver, at 00:05 and 14:00, the week's strip is 100 px for a source of any age, six whole days 2 px apart, then today ending at the read, each edge on a whole pixel, and the count 8 px after it", async () => {
    const zone = process.env.TZ
    const strip = (page) => weekButton(page).evaluate((el) => {
      const svg = el.querySelector("[data-slot=week-strip]")
      const days = [...svg.querySelectorAll("rect:not([data-slot])")].map((r) => [Number(r.getAttribute("x")), Number(r.getAttribute("width"))])
      const runs = [...svg.querySelectorAll("[data-slot=week-run]")].map((r) => [Number(r.getAttribute("x")), Number(r.getAttribute("width"))])
      const lines = [...svg.querySelectorAll("[data-slot=week-failover]")].map((l) => Number(l.getAttribute("x1")))
      const day = new Date(); const since = Date.now() - day.setHours(0, 0, 0, 0)
      /* Today's hairline is as long as today has been, so in its first minutes it has no pixel and isn't drawn. */
      const start = Math.round(6 * (88 / 7 + 2))
      const [before, today] = [days.filter(([x]) => x < start), days.filter(([x]) => x >= start)]
      const end = today.length === 1 && today[0][0] === start ? today[0][0] + today[0][1] : start
      return {
        width: Math.round(svg.getBoundingClientRect().width), days: before.length, whole: [...days.flat(), ...runs.flat(), ...lines].every(Number.isInteger), runs: runs.every(([, w]) => w >= 1),
        today: today.length <= 1 && Math.abs(end - Math.round(start + (since / 86_400_000) * (88 / 7))) <= 1, usedToday: runs.some(([x]) => x >= start),
        countGap: Math.round(svg.nextElementSibling.getBoundingClientRect().left - svg.getBoundingClientRect().right), count: svg.nextElementSibling.textContent,
      }
    })
    try {
      for (const tz of ["Asia/Dubai", "America/Denver"]) {
        process.env.TZ = tz
        for (const [h, m] of [[0, 5], [14, 0]]) {
          const now = new Date(2026, 10, 20, h, m).getTime()
          const label = `${tz} at ${String(h).padStart(2, "0")}:${String(m).padStart(2, "0")}`
          const t = seedTroubled(now)
          const today = new Date(now).setHours(0, 0, 0, 0)
          daemon.set({
            sources: new Map(t.sources.map((x) => [x.id, x.id === "unlimited" ? { ...x, addedAt: new Date(Math.max(today, now - 3600e3)).toISOString() } : x])),
            analytics: (...args) => {
              const all = t.analytics(...args).filter((b) => b.source !== "unlimited")
              const lineage = t.servers.find((s) => s.source === "unlimited").id
              const [from, to] = args
              const at = Math.floor(Math.max(today, now - 1000) / 900e3) * 900e3
              return at >= from && at < to ? [...all, { ...all[0], start: new Date(at).toISOString(), source: "unlimited", lineage, forcedSwitchAways: 8, incidents: 8, servedSeconds: 600 }] : all
            },
          })
          const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, colorScheme: "dark", timezoneId: tz })
          try {
            await context.clock.install({ time: now })
            const page = await context.newPage()
            page.setDefaultTimeout(10_000)
            await page.goto(daemon.url + "sources/unlimited")
            await weekButton(page).waitFor()
            assert.deepEqual(await strip(page), { width: 100, days: 6, whole: true, runs: true, today: true, usedToday: true, countGap: 8, count: "8" }, `${label}: a source added today, the whole week, the days before it bare, today's use drawn, its count right after the strip`)
            await page.goto(daemon.url + "sources/soon")
            await weekButton(page).waitFor()
            assert.deepEqual(await strip(page), { width: 100, days: 6, whole: true, runs: true, today: true, usedToday: false, countGap: 8, count: "9" }, `${label}: a full week's, the same`)
          } finally {
            await context.close()
          }
        }
      }
    } finally {
      if (zone === undefined) delete process.env.TZ
      else process.env.TZ = zone
    }
  })

  test("17. in UTC, Dubai, Kolkata, Chatham and Denver, at 00:20, 09:00 and 23:50, the week's letters are the seven local days ending today, today's bold, and a failover's tooltip names its local six hours", async () => {
    const zone = process.env.TZ
    const aurora = daemon.source("aurora")
    const bucket = (start) => ({ start: new Date(start).toISOString(), source: "aurora", lineage: "l-ams", country: "NL", uploaded: 0, downloaded: 0, probeSuccesses: 0, probeAttempts: 0, wanExcluded: 0, unknown: 0,
      pingMedianMs: null, pingP95Ms: null, newConnectionMedianMs: null, forcedSwitchAways: 2, servedSeconds: 600, incidents: 2, managementTrafficIncluded: false })
    const hour = (t) => new Date(t).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" })
    try {
      for (const tz of ["UTC", "Asia/Dubai", "Asia/Kolkata", "Pacific/Chatham", "America/Denver"]) {
        process.env.TZ = tz
        const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, colorScheme: "light", timezoneId: tz })
        try {
          const page = await context.newPage()
          page.setDefaultTimeout(10_000)
          for (const [h, m] of [[0, 20], [9, 0], [23, 50]]) {
            const now = new Date(2026, 10, 17, h, m).getTime()
            const failover = Math.floor((now - 26 * 3600e3) / 900e3) * 900e3
            const start = new Date(failover).setHours(Math.floor(new Date(failover).getHours() / 6) * 6, 0, 0, 0)
            const end = new Date(start).setHours(new Date(start).getHours() + 6)
            const label = `${tz} at ${hour(now)}`
            daemon.set({ sources: new Map([["aurora", { ...aurora, addedAt: null }]]), analytics: () => [bucket(failover)] })
            await context.clock.setFixedTime(now)
            await page.goto(daemon.url + "sources/aurora")
            await weekButton(page).hover()
            await card(page).locator("[data-slot=day-letter]").first().waitFor()
            const letters = await card(page).locator("[data-slot=day-letter]").evaluateAll((all) => all.map((t) => (t.classList.contains("font-semibold") ? `*${t.textContent}` : t.textContent)))
            const days = [6, 5, 4, 3, 2, 1, 0].map((n) => { const d = new Date(now); d.setDate(d.getDate() - n); return `${n === 0 ? "*" : ""}${d.toLocaleDateString("en-US", { weekday: "short" }).slice(0, 2)}` })
            assert.deepEqual(letters, days, `${label}: the seven local days ending today`)
            const words = `${new Date(start).toLocaleDateString("en-US", { weekday: "short" })} ${hour(start)}–${hour(end)}`
            assert.equal(await weekPlace(page, { failover: 0 }), `${words} Failovers 2 In use 15m`, `${label}: its local six hours`)
            await page.mouse.move(0, 0)
          }
        } finally {
          await context.close()
        }
      }
    } finally {
      if (zone === undefined) delete process.env.TZ
      else process.env.TZ = zone
    }
  })

  test("17. the week ends at the moment its read was answered: as the page's clock runs on within six hours, today's line and its hours to hover stay where the read left them", async () => {
    seedTroubled()
    const morning = new Date(); morning.setHours(7, 0, 0, 0)
    const { page, close } = await open("sources/steady", { width: 1280, clock: morning.getTime() })
    const today = () => card(page).locator("[data-slot=chart] svg rect:not([data-slot])").last().evaluate((r) => Number(r.getAttribute("x")) + Number(r.getAttribute("width")))
    try {
      await weekButton(page).hover()
      await card(page).locator("[data-slot=chart-readout]").waitFor()
      const read = await today()
      await page.clock.runFor(4 * 3600e3)
      await page.mouse.move(0, 0)
      await page.waitForTimeout(400)
      await weekButton(page).hover()
      await card(page).locator("[data-slot=chart-readout]").waitFor()
      assert.equal(await today(), read, "today's line ends where the read was answered, four hours ago by the page's clock")
      assert.match(await weekPlace(page, 287), /^\w{3} 06:00–12:00 /, "the last place to hover is the one the read was answered in")
    } finally {
      await close()
    }
  })

  test("17. on a phone a tap on the week card's line reads its place and keeps it after the finger lifts, and closing the card returns to 7d", async () => {
    seedTroubled()
    const { page, close } = await open("sources/steady", { width: 390, touch: true })
    try {
      await weekButton(page).tap()
      await card(page).locator("[data-slot=chart-readout]").waitFor()
      const window = () => card(page).locator("[data-slot=chart-window]").innerText()
      const box = await card(page).locator("[data-slot=chart] svg").first().boundingBox()
      const x = Number(await card(page).locator("[data-slot=week-failover]").first().getAttribute("x1"))
      await page.touchscreen.tap(box.x + (x * box.width) / 288, box.y + 6)
      await until(async () => (await window()) !== "7d", "a tap reads its place")
      const read = await readoutOf(card(page))
      assert.match(read, /^\w{3} \d\d:\d\d–\d\d:\d\d Failovers 1 In use 15m$/)
      await page.waitForTimeout(600)
      assert.equal(await readoutOf(card(page)), read, "the place stays after the finger lifts, and the card with it")
      await page.touchscreen.tap(box.x + (x * box.width) / 288, box.y + 6)
      await page.waitForTimeout(600)
      assert.equal(await readoutOf(card(page)), read, "a second tap on the same place reads it still")
      const first = await window()
      await page.touchscreen.tap(box.x + ((x - 60) * box.width) / 288, box.y + 6)
      await until(async () => ![first, "7d"].includes(await window()), "a tap on another place reads that place")
      await page.waitForTimeout(600)
      assert.ok(![first, "7d"].includes(await window()), "and keeps it")
      await page.getByRole("table", { name: "Servers" }).tap({ position: { x: 10, y: 10 } })
      await until(async () => (await page.locator("[data-radix-popper-content-wrapper]").count()) === 0, "a tap elsewhere closes the card")
      await weekButton(page).tap()
      await card(page).locator("[data-slot=chart-readout]").waitFor()
      assert.equal(await window(), "7d", "opened again, it reads the week")
    } finally {
      await close()
    }
  })

  test("17. on a phone a tap inside the week card off its line, on its window, its legend or its edge, keeps it open and returns it to 7d and the week's figures, and a tap outside closes it", async () => {
    seedTroubled()
    const { page, close } = await open("sources/steady", { width: 390, touch: true })
    try {
      await weekButton(page).tap()
      await card(page).locator("[data-slot=chart-readout]").waitFor()
      const week = await readoutOf(card(page))
      assert.match(week, /^7d Failovers \d+ In use \S+$/)
      const box = await card(page).locator("[data-slot=chart] svg").first().boundingBox()
      const x = Number(await card(page).locator("[data-slot=week-failover]").first().getAttribute("x1"))
      const edge = await card(page).locator("> *").boundingBox()
      for (const [what, tap] of [
        ["its window", () => card(page).locator("[data-slot=chart-window]").tap()],
        ["its legend", () => card(page).locator("[data-slot=chart-legend]").tap()],
        ["its edge", () => page.touchscreen.tap(edge.x + 3, edge.y + edge.height - 3)],
      ]) {
        await page.touchscreen.tap(box.x + (x * box.width) / 288, box.y + 6)
        await until(async () => (await readoutOf(card(page))) !== week, `${what}: a tap on the line reads its place`)
        await page.waitForTimeout(100)
        await tap()
        await until(async () => (await readoutOf(card(page))) === week, `a tap on ${what} returns to 7d and the week's figures`)
        await page.waitForTimeout(600)
        assert.equal(await page.locator("[data-radix-popper-content-wrapper]").count(), 1, `the card stays open after a tap on ${what}`)
        assert.equal(await readoutOf(card(page)), week)
      }
      await page.getByRole("table", { name: "Servers" }).tap({ position: { x: 10, y: 10 } })
      await until(async () => (await page.locator("[data-radix-popper-content-wrapper]").count()) === 0, "a tap outside closes the card")
    } finally {
      await close()
    }
  })

  test("17. while the week's card is open from the keyboard, the arrow keys on the strip move its place, and closing it returns to 7d", async () => {
    seedTroubled()
    const { page, close } = await open("sources/steady", { width: 1280 })
    try {
      await weekButton(page).focus()
      await card(page).locator("[data-slot=chart-readout]").waitFor()
      const window = () => card(page).locator("[data-slot=chart-window]").innerText()
      await page.keyboard.press("ArrowLeft")
      await until(async () => (await window()) !== "7d", "ArrowLeft from the week takes the last place")
      const last = await window()
      await page.keyboard.press("ArrowLeft")
      await until(async () => (await window()) !== last, "and moves on")
      await page.keyboard.press("End")
      await until(async () => (await window()) === last, "End takes the last again")
      assert.equal(await card(page).locator("[data-slot=week-cursor]").count(), 1, "its band stands on the line")
      await page.keyboard.press("Escape")
      await until(async () => (await page.locator("[data-radix-popper-content-wrapper]").count()) === 0, "Esc closes the card")
      await weekButton(page).blur()
      await weekButton(page).focus()
      await card(page).locator("[data-slot=chart-readout]").waitFor()
      assert.equal(await window(), "7d", "opened again, it reads the week")
    } finally {
      await close()
    }
  })

  test("17. in a failing refresh's card each value stands 12 px after the names", async () => {
    seedTroubled()
    const { page, close } = await open("sources/soon", { width: 1280 })
    const read = async () => {
      await page.evaluate(() => Promise.all(document.getAnimations().filter((a) => a.effect?.getComputedTiming().iterations !== Infinity).map((a) => a.finished.catch(() => {}))))
      const rows = await rowsOf(page.locator("body"))
      const names = Math.max(...rows.map((r) => r.name))
      return { words: rows.map((r) => r.words), gaps: [...new Set(rows.map((r) => Math.round(r.value - names)))] }
    }
    try {
      await refreshButton(page).click()
      await until(async () => (await read()).words.length === 2, "the failing refresh's rows, Last worked and Next")
      assert.deepEqual((await read()).gaps, [12])
    } finally {
      await close()
    }
  })

  test("17. traffic read 3 days ago with nothing left reads its figure in trouble's colour in its card, as the heading does, with its age, and what was sent stays plain", async () => {
    seedTroubled()
    const { page, close } = await open("sources/limit", { width: 1280 })
    try {
      const traffic = headingOf(page).locator("[data-slot=source-facts] button").filter({ hasText: "left" })
      assert.equal(await toneOf(traffic.getByText("0 GB", { exact: true })), "bad", "the heading's figure is red")
      await traffic.hover()
      await card(page).getByText("3d ago", { exact: true }).waitFor()
      assert.equal(await card(page).locator("svg.lucide-history").count(), 1, "the age beside the history sign")
      assert.equal(await toneOf(card(page).getByText(/^0 \/ 100 GB$/)), "bad", "the figure in the card is red too")
      assert.equal(await toneOf(card(page).getByText("10.0 GB", { exact: true })), "plain")
    } finally {
      await close()
    }
  })

  test("17. a week the page can't read keeps 7d and the muted mark in its place, whose card holds the read's reason muted and Retry on one line, and the dot and the list carry no failover trouble", async () => {
    seedTroubled()
    daemon.set({ analyticsFails: true })
    const { page, close } = await open("sources/soon", { width: 1280 })
    try {
      const mark = headingOf(page).getByRole("button", { name: "The week: HTTP 502", exact: true })
      await mark.waitFor()
      assert.equal(await shownWords(mark), "7d")
      assert.equal(await mark.locator("svg.lucide-circle-x").count(), 1, "the muted mark in the strip's place")
      assert.equal(Math.round((await mark.locator("svg.lucide-circle-x").locator("..").boundingBox()).width), 100, "which keeps the strip's 100px, so the heading doesn't move")
      assert.equal(await toneOf(mark), "muted", "no red")
      await mark.click()
      const retry = card(page).getByRole("button", { name: "Retry" })
      await retry.waitFor()
      assert.equal((await card(page).innerText()).replace(/\s+/g, " ").trim(), "HTTP 502 Retry")
      const reason = card(page).getByText("HTTP 502", { exact: true })
      assert.equal(await toneOf(reason), "muted", "the reason muted, as every failed read's")
      const [said, button] = [await reason.boundingBox(), await retry.boundingBox()]
      assert.ok(Math.abs((said.y + said.height / 2) - (button.y + button.height / 2)) < 2 && button.x > said.x + said.width, "Retry beside the reason, on its line")
      assert.equal(await shownWords(nav(page).locator('a[href$="/sources/soon"]')), "Soon 3 Expires in 3d · 5.0 GB left · Refresh failing", "the list counts no failovers")
      daemon.set({ analyticsFails: false })
      await retry.click()
      await weekButton(page).waitFor()
    } finally {
      await close()
    }
  })

  test("17. a source's failovers are its week's, whatever its slots point at, alike in its words, its marks, its dot and the list", async () => {
    const t = seedTroubled()
    const rule = { sources: { not: false, values: ["steady"] }, countries: { not: false, values: [] }, protocols: { not: false, values: [] }, include: "", exclude: "", box: null }
    daemon.set({
      sources: new Map(t.sources.map((x) => [x.id, x.id === "steady" ? { ...x, moves7d: 0 } : x])),
      groups: new Map([["from-steady", { ...groupView("from-steady", 1, { name: "From Steady", members: rule }, [], t.servers), moves7d: 7 }]]),
      analytics: (...args) => {
        const all = t.analytics(...args)
        const first = all.find((b) => b.source === "steady")
        return all.map((b) => (b.source === "steady" ? { ...b, forcedSwitchAways: b === first ? 7 : 0 } : b))
      },
    })
    const { page, close } = await open("sources/steady", { width: 1280 })
    try {
      const heading = headingOf(page)
      const week = weekButton(page)
      await week.waitFor()
      assert.equal(await week.getAttribute("aria-label"), "7 failovers in the last 7 days, in use on 5 of them")
      assert.equal(await shownWords(week), "7d 7", "the count follows the strip")
      assert.equal(await toneOf(week.getByText("7", { exact: true })), "warn")
      assert.equal(await toneOf(week.getByText("7d", { exact: true })), "muted")
      await heading.getByRole("img", { name: "Unstable", exact: true }).waitFor()
      await week.hover()
      assert.match(await weekPlace(page, { failover: 0 }), /^\w{3} \d\d:\d\d–\d\d:\d\d Failovers 7 /, "its card's six hours")
      const entry = nav(page).locator('a[href$="/sources/steady"]')
      await until(async () => (await shownWords(entry)) === "Steady 3 7 failovers in 7d", "the list says it as the heading does")
      await entry.getByRole("img", { name: "Unstable", exact: true }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. until the week is read, the dot and the list count no failovers, so they never show the daemon's own count first", async () => {
    const t = seedTroubled()
    daemon.set({ sources: new Map(t.sources.map((x) => [x.id, x.id === "steady" ? { ...x, moves7d: 30 } : x])), readDelay: { "api/analytics": 1500 } })
    const { page, close } = await open("sources/steady", { width: 1280 })
    try {
      const entry = nav(page).locator('a[href$="/sources/steady"]')
      await entry.waitFor()
      const seen = new Set()
      const look = async () => seen.add(`${await shownWords(entry)} ${await entry.getByRole("img").getAttribute("aria-label")} ${await headingOf(page).getByRole("img", { name: /^(Online|Unstable|Offline)$/ }).getAttribute("aria-label")}`)
      await look()
      while (!(await weekButton(page).isVisible())) await look()
      await look()
      assert.deepEqual([...seen], ["Steady 3 Online Online"], "before and after the week lands")
    } finally {
      await close()
    }
  })

  test("17. an open tab's week takes in each cell as the clock passes it", async () => {
    seedTroubled()
    const next = new Date().setHours(Math.floor(new Date().getHours() / 6) * 6 + 6, 0, 0, 0)
    const used = {
      start: new Date(next).toISOString(), source: "steady", lineage: "steady-1", country: "DE", uploaded: 0, downloaded: 0, probeSuccesses: 0, probeAttempts: 0, wanExcluded: 0, unknown: 0,
      pingMedianMs: null, pingP95Ms: null, newConnectionMedianMs: null, forcedSwitchAways: 0, servedSeconds: 3600, incidents: 0, managementTrafficIncluded: false,
    }
    daemon.set({ analytics: (from, to) => [used].filter((b) => Date.parse(b.start) >= from && Date.parse(b.start) < to) })
    const { page, close } = await open("sources/steady", { width: 1280, clock: true })
    try {
      const week = weekButton(page)
      assert.equal(await week.getAttribute("aria-label"), "No failovers in the last 7 days, not in use")
      await page.clock.fastForward(next + 3600e3 - (await page.evaluate(() => Date.now())))
      await until(async () => (await week.getAttribute("aria-label").catch(() => "")) === "No failovers in the last 7 days, in use on 1 of them", "the cell the clock passed, with no reload")
    } finally {
      await close()
    }
  })

  test("17. an expiry a year or more away, a provider's never, is shown nowhere: not in the heading, its cards or the settings", async () => {
    seedTroubled()
    const { page, close } = await open("sources/unlimited", { width: 1280 })
    try {
      const heading = headingOf(page)
      await weekButton(page).waitFor()
      assert.doesNotMatch(await heading.innerText(), /Expire/)
      await page.getByRole("button", { name: "Settings for Unlimited" }).click()
      const settings = page.getByRole("dialog", { name: "Settings" })
      await settings.getByLabel("URL", { exact: true }).waitFor()
      assert.doesNotMatch(await settings.innerText(), /Expire|20[0-9][0-9]/, "no date in the settings")
    } finally {
      await close()
    }
  })

  test("17. Refresh's hover is two rows, Next and how often as Settings writes it, or Refresh Off while paused, each value 12 px after the names", async () => {
    seedTroubled()
    const { page, close } = await open("sources/unlimited", { width: 1280 })
    /* Every line of the card, a nameless one too, since only named lines are rows. */
    const lines = () => card(page).locator("> *").evaluate((el) => [...el.children]
      .map((line) => (line.dataset.slot === "row" ? [...line.children] : [line]).map((part) => part.textContent).join(" ").trim()), null, { timeout: 500 }).catch(() => [])
    try {
      const refresh = refreshButton(page)
      await refresh.focus()
      await until(async () => (await lines()).join("|") === "Next in 3h|Refresh every 6h", "when it runs next and how often")
      const rows = await rowsOf(page.locator("body"))
      assert.deepEqual(rows.map((r) => Math.round(r.value - Math.max(...rows.map((x) => x.name)))), [12, 12], "each value 12 px after the names' column")
      assert.equal(await toneOf(card(page).getByText("Next", { exact: true })), "muted", "the names muted")
      assert.equal(await toneOf(card(page).getByText("every 6h", { exact: true })), "plain", "the values plain")
      await refresh.blur()
      const x = daemon.source("unlimited")
      const at = (seconds, paused = false, suggested = null) => daemon.set({ sources: new Map(daemon.sources().map((y) => [y.id, y.id === "unlimited"
        ? { ...x, origin: { ...x.origin, refreshEverySeconds: seconds, paused, provider: { ...x.origin.provider, suggestedRefreshSeconds: suggested } } } : y])) })
      at(86_400)
      await page.reload()
      await refresh.focus()
      await until(async () => (await lines()).at(-1) === "Refresh every 24h", "a day as Settings writes it")
      await refresh.blur()
      at(0, false, 43_200)
      await page.reload()
      await refresh.focus()
      await until(async () => (await lines()).at(-1) === "Refresh every 12h", "none of its own: the provider's")
      await refresh.blur()
      at(x.origin.refreshEverySeconds, true)
      await page.reload()
      await refresh.focus()
      await until(async () => (await lines()).join("|") === "Refresh Off", "paused: Refresh Off alone")
    } finally {
      await close()
    }
  })

  test("17. a mark keeps its colour in every state of the button it stands in: Refresh's amber sign at rest, hovered, pressed, open, focused and while its Retry runs", async () => {
    seedTroubled()
    const { page, close } = await open("sources/soon", { width: 1280 })
    try {
      const refresh = refreshButton(page)
      const sign = refresh.locator("svg").first()
      await until(async () => (await shownWords(refresh)) === "6h ago", "the failing button")
      const tones = async () => [await toneOf(sign), await toneOf(refresh.getByText("6h ago"))].join(" ")
      assert.equal(await tones(), "warn warn", "at rest")
      await refresh.hover()
      assert.equal(await tones(), "warn warn", "hovered")
      await page.mouse.down()
      assert.equal(await tones(), "warn warn", "pressed")
      await page.mouse.up()
      await until(async () => (await refresh.getAttribute("aria-expanded")) === "true", "its card open")
      assert.equal(await tones(), "warn warn", "open")
      await page.keyboard.press("Escape")
      await refresh.focus()
      assert.equal(await tones(), "warn warn", "focused")
      await page.route((url) => url.pathname.endsWith("/api/sources/soon/refresh"), () => {})
      await refresh.click()
      await card(page).getByRole("button", { name: "Retry", exact: true }).click()
      await until(() => refresh.isDisabled(), "Refresh waits while its Retry runs")
      assert.equal(await toneOf(refresh.locator("svg").first()), "warn", "its sign stays amber while its Retry runs")
    } finally {
      await close()
    }
  })

  test("17. one press is one Pending: the way pressed spins, and every other way into the same refresh or read waits at half strength, with no spinner and no press", async () => {
    const spins = (button) => button.locator("[data-slot=spinner]").count()
    const waits = async (button, what) => {
      assert.equal(await button.isDisabled(), true, `${what} takes no press`)
      assert.equal(await spins(button), 0, `${what} doesn't spin`)
      await until(async () => (await button.evaluate((el) => getComputedStyle(el).opacity)) === "0.5", `${what} at half strength`)
    }
    const hold = (page, path) => page.route((url) => url.pathname.endsWith(path), () => {})
    const t = seedTroubled()
    {
      const { page, close } = await open("sources/soon", { width: 1280 })
      try {
        await hold(page, "/api/sources/soon/refresh")
        const refresh = refreshButton(page)
        await until(async () => (await shownWords(refresh)) === "6h ago", "the failing button")
        await refresh.click()
        const retry = card(page).getByRole("button", { name: "Retry", exact: true })
        await retry.click()
        await until(async () => (await spins(retry)) === 1, "Refresh's card's Retry, pressed, spins")
        await waits(refresh, "the heading's Refresh")
        assert.equal(await refresh.locator("svg.lucide-refresh-cw").count(), 1, "it keeps its sign")
      } finally {
        await close()
      }
    }
    {
      const { page, close } = await open("sources/broken", { width: 1280 })
      try {
        await hold(page, "/api/sources/broken/refresh")
        const retry = page.locator("[data-slot=empty]").getByRole("button", { name: "Retry", exact: true })
        await retry.click()
        await until(async () => (await spins(retry)) === 1, "the Retry in the table's place, pressed, spins")
        await waits(refreshButton(page), "the heading's Refresh")
      } finally {
        await close()
      }
    }
    const hourAgo = new Date(Date.now() - 3600e3).toISOString()
    daemon.set({ sources: new Map(t.sources.map((x) => [x.id, x.id === "new" ? { ...x, origin: { ...x.origin, fetch: { ...x.origin.fetch, checkedAt: hourAgo, updatedAt: hourAgo } } } : x])) })
    for (const pressed of ["block", "heading"]) {
      const { page, close } = await open("sources/new", { width: 1280 })
      try {
        await hold(page, "/api/sources/new/refresh")
        const [block, heading] = [page.locator("[data-slot=empty]").getByRole("button", { name: "Refresh", exact: true }), refreshButton(page)]
        const [press, other] = pressed === "block" ? [block, heading] : [heading, block]
        await press.click()
        await until(async () => (await spins(press)) === 1, `the ${pressed} Refresh, pressed, spins`)
        await waits(other, `the ${pressed === "block" ? "heading's" : "table's place's"} Refresh`)
      } finally {
        await close()
      }
    }
    daemon.set({ readFails: { "api/servers": 10 } })
    for (const pressed of ["row", "list"]) {
      const { page, close } = await open("servers", { width: 1280 })
      try {
        const row = page.locator("[data-slot=table-line]").getByRole("button", { name: "Retry" })
        await row.waitFor()
        await page.route("**/api/servers?*", () => {})
        const mark = nav(page).getByRole("button", { name: "All servers: HTTP 502", exact: true })
        if (pressed === "row") await row.click()
        await mark.click()
        const inCard = card(page).getByRole("button", { name: "Retry" })
        if (pressed === "list") await inCard.click()
        const [press, other] = pressed === "row" ? [row, inCard] : [inCard, row]
        await until(async () => (await spins(press)) === 1, `the server list's Retry in the ${pressed}, pressed, spins`)
        await waits(other, `its Retry in the ${pressed === "row" ? "list's card" : "page's row"}`)
      } finally {
        await close()
      }
    }
  })

  test("17. a failed read has one form wherever it stands: its reason muted and Retry the outline button in full ink, in a page's row, the week's card, Refresh's card and the empty block", async () => {
    seedTroubled()
    const { page, close } = await open("sources/soon", { width: 1280 })
    const form = async (place, where) => {
      const retry = place.getByRole("button", { name: "Retry", exact: true })
      await retry.waitFor()
      const reason = place.getByText(/^(HTTP \d+|Timeout)$/).first()
      assert.deepEqual([await toneOf(reason), await toneOf(retry)], ["muted", "plain"], `${where}: the reason muted, Retry in full ink`)
    }
    try {
      await refreshButton(page).click()
      await form(card(page), "Refresh's card")
      await page.keyboard.press("Escape")
      await page.goto(daemon.url + "sources/broken")
      await form(page.locator("main [data-slot=empty]"), "the empty block")
      daemon.set({ analyticsFails: true })
      await page.goto(daemon.url + "sources/soon")
      await headingOf(page).getByRole("button", { name: /^The week: / }).click()
      await form(card(page), "the week's card")
      daemon.set({ readFails: { "api/servers": 10 } })
      await page.goto(daemon.url + "servers")
      await form(page.locator("main [data-slot=table-line]"), "a page's row")
    } finally {
      await close()
    }
  })

  test("17. a failing refresh is its sign and the last good list's age in amber, and opens why: the reason, its rows and Retry, with the User-Agents beside it on a 401, 403 or 404", async () => {
    seedTroubled()
    const { page, close } = await open("sources/soon", { width: 1280 })
    try {
      const why = page.getByRole("dialog")
      const refresh = refreshButton(page)
      await until(async () => (await shownWords(refresh)) === "6h ago", "the button's age is the last good list's")
      assert.equal(await refresh.getAttribute("aria-label"), "Refresh failing")
      assert.equal(await toneOf(refresh.getByText("6h ago")), "warn")
      assert.equal(await toneOf(refresh.locator("svg")), "warn", "its sign is amber too")
      await refresh.click()
      assert.equal(await failureOf(why), "HTTP 503 | Last worked 6h ago | Next in 3h", "three failures, and no count of them")
      assert.equal(await why.getByRole("combobox").count(), 0, "a 503 offers no User-Agent")
      await why.getByRole("button", { name: "Retry", exact: true }).click()
      await until(() => requests("POST", "api/sources/soon/refresh").length === 1, "Retry refreshes")
      assert.equal(requests("POST", "api/sources/soon/refresh")[0].body, "", "with no agent offered, the refresh carries none")
      await page.keyboard.press("Escape")
      await page.goto(daemon.url + "sources/broken")
      const block = page.locator("main [data-slot=empty]")
      assert.equal(await failureOf(block), "Timeout | Last worked — | Next in 3h", "listing no servers, in the table's place")
      assert.equal(await toneOf(block.getByText("—", { exact: true })), "muted", "never worked: the muted dash")
      assert.equal(await shownWords(refreshButton(page)), "", "never worked: the sign alone")
      await page.keyboard.press("Escape")
      await page.goto(daemon.url + "sources/limit")
      await refreshButton(page).click()
      assert.equal(await failureOf(why), "HTTP 404 | Last worked 6h ago | Next in 3h")
      assert.doesNotMatch(await why.innerText(), /try another|Settings/)
      await why.getByRole("combobox", { name: "User-Agent" }).click()
      await page.getByRole("option", { name: "Hiddify/2.5.7" }).click()
      await why.getByRole("button", { name: "Retry", exact: true }).click()
      await until(() => requests("POST", "api/sources/limit/refresh").length === 1, "Retry refreshes")
      assert.deepEqual(JSON.parse(requests("POST", "api/sources/limit/refresh")[0].body), { userAgent: "Hiddify/2.5.7" }, "the refresh carries the agent shown")
      assert.equal(daemon.source("limit").origin.userAgent, "Hiddify/2.5.7", "and the daemon stored it")
      assert.equal(requests("PUT", "api/sources/limit").length, 0, "in one write: no save of the source")
    } finally {
      await close()
    }
  })

  for (const width of [1280, 390]) {
    test(`23. at ${width}, a failing source that lists no servers holds its failure and Retry once, in its table's place: its heading's Refresh stays amber, refreshes at a press and opens no card`, async () => {
      seedTroubled()
      const { page, close } = await open("sources/broken", { width, touch: width < 768 })
      try {
        const block = page.locator("main [data-slot=empty]")
        const retry = block.getByRole("button", { name: "Retry", exact: true })
        await retry.waitFor()
        const refresh = refreshButton(page)
        assert.equal(await refresh.getAttribute("aria-label"), "Refresh failing")
        assert.equal(await toneOf(refresh.locator("svg")), "warn", "its sign amber")
        if (width >= 768) {
          await refresh.hover()
          await until(async () => (await card(page).innerText().catch(() => "")).replace(/\s+/g, " ") === "Next in 3h Refresh every 6h", "its hover, when it runs next and how often, as a working Refresh's")
          await page.mouse.move(0, 0)
          await card(page).waitFor({ state: "detached" })
        }
        const path = "/api/sources/broken/refresh"
        await page.route((url) => url.pathname.endsWith(path), () => {})
        const sent = page.waitForRequest((r) => r.method() === "POST" && new URL(r.url()).pathname.endsWith(path), { timeout: 2000 })
        if (width < 768) await refresh.tap()
        else await refresh.click()
        assert.equal((await sent).postData() ?? "", "", "a press refreshes, with the stored agent")
        await until(async () => (await refresh.locator("[data-slot=spinner]").count()) === 1, "the heading's Refresh, pressed, spins")
        assert.equal(await retry.isDisabled(), true, "the table's place's Retry waits")
        assert.equal(await page.getByRole("dialog").count(), 0, "no card opened")
        assert.equal(await page.getByRole("button", { name: "Retry", exact: true }).count(), 1, "one Retry on the page")
      } finally {
        await close()
      }
    })
  }

  test("23. a failing refresh's Last worked is heard as what it is, its dash as never or as not known, and seen as before", async () => {
    const t = seedTroubled()
    const { page, close } = await open("sources/broken", { width: 1280 })
    const heard = async (place) => (await place.locator("[data-slot=rows]").ariaSnapshot()).replace(/\s+/g, " ")
    try {
      const block = page.locator("main [data-slot=empty]")
      await block.locator("[data-slot=rows]").waitFor()
      assert.match(await heard(block), /Last worked never Next in 3h/, "a source no refresh ever worked for")
      assert.doesNotMatch(await heard(block), /—/)
      assert.equal(await failureOf(block), "Timeout | Last worked — | Next in 3h", "seen: the dash")
      const why = page.getByRole("dialog")
      await page.goto(daemon.url + "sources/soon")
      await until(async () => (await shownWords(refreshButton(page))) === "6h ago", "the failing button")
      await refreshButton(page).click()
      assert.equal(await failureOf(why), "HTTP 503 | Last worked 6h ago | Next in 3h")
      assert.match(await heard(why), /Last worked 6 hours ago Next in 3h/, "a known time, its units whole")
      t.refreshes.set("soon", [])
      daemon.set({ refreshes: t.refreshes, sources: new Map(t.sources.map((x) => [x.id, x.id === "soon" ? { ...x, origin: { ...x.origin, fetch: { ...x.origin.fetch, checkedAt: null } } } : x])) })
      await page.goto(daemon.url + "sources/soon")
      await refreshButton(page).click()
      assert.equal(await failureOf(why), "HTTP 503 | Last worked — | Next in 3h")
      assert.match(await heard(why), /Last worked not known Next in 3h/, "a source that worked once, with nothing left to say when")
    } finally {
      await close()
    }
  })

  /** Opens Limit's failing refresh at 1280 px and picks an agent in its card. */
  const pickAgent = async (page, agent) => {
    await refreshButton(page).click()
    const why = page.getByRole("dialog")
    await why.getByRole("combobox", { name: "User-Agent" }).click()
    await page.getByRole("option", { name: agent, exact: true }).click()
    return why
  }
  /** Holds each write to Limit, a save or a refresh, until `release` is called, and counts them. */
  const holdWrites = async (page) => {
    const held = { count: 0, release: null }
    await page.route(/\/api\/sources\/limit(\/refresh)?$/, async (route) => {
      if (route.request().method() === "GET") return route.continue()
      held.count++
      await new Promise((go) => { held.release = go })
      await route.continue()
    })
    return held
  }

  test("17. while Retry's write is out the agents and Retry are Pending and take no click, so the agent shown is the one sent and stored", async () => {
    seedTroubled()
    const { page, close } = await open("sources/limit", { width: 1280, theme: "light" })
    try {
      const held = await holdWrites(page)
      const why = await pickAgent(page, "Hiddify/2.5.7")
      const retry = why.getByRole("button", { name: "Retry", exact: true })
      const agents = why.getByRole("combobox", { name: "User-Agent" })
      await retry.click()
      await until(() => held.release, "the write is held")
      await until(async () => (await agents.isDisabled()) && (await retry.isDisabled()), "the agents and Retry are closed while the write is out")
      assert.equal(await retry.getAttribute("aria-busy"), "true", "Retry is Pending")
      assert.equal(await retry.locator("[data-slot=spinner]").count(), 1, "a spinner where Retry's sign was")
      assert.equal(await shownWords(agents), "Hiddify/2.5.7", "the agent sent stays in sight")
      await agents.click({ force: true }).catch(() => {})
      assert.equal(await page.getByRole("option", { name: "v2rayN/7.13.2" }).count(), 0, "no other agent can be picked")
      held.release()
      await until(() => requests("POST", "api/sources/limit/refresh").some((r) => r.status === 202), "the refresh lands")
      await until(async () => !(await retry.isDisabled()), "Retry opens again")
      assert.equal(held.count, 1, "one write")
      assert.equal(requests("PUT", "api/sources/limit").length, 0, "no save of the source")
      assert.equal(daemon.source("limit").origin.userAgent, "Hiddify/2.5.7")
      assert.equal(await shownWords(agents), "Hiddify/2.5.7", "the agent shown is the one stored")
    } finally {
      await close()
    }
  })

  test("17. Retry's agent is stored though another tab saved the source while its write was out, and no Settings opens", async () => {
    seedTroubled()
    const a = await open("sources/limit", { width: 1280, theme: "light" })
    const b = await open("sources/limit", { width: 1280, theme: "light" })
    try {
      const held = await holdWrites(a.page)
      const why = await pickAgent(a.page, "Hiddify/2.5.7")
      await why.getByRole("button", { name: "Retry", exact: true }).click()
      await until(() => held.release, "A's write is held")
      await b.page.getByRole("button", { name: "Rename Limit", exact: true }).click()
      const name = headingOf(b.page).getByRole("textbox")
      await name.fill("Other tab")
      await name.press("Enter")
      await until(() => daemon.source("limit").name === "Other tab", "B saved its rename")
      held.release()
      await until(() => requests("POST", "api/sources/limit/refresh").some((r) => r.status === 202), "A's refresh lands")
      await until(() => daemon.source("limit").origin.userAgent === "Hiddify/2.5.7", "A's agent is stored")
      assert.equal(daemon.source("limit").name, "Other tab", "B's rename stays")
      await a.page.waitForTimeout(300)
      assert.equal(await a.page.getByRole("dialog", { name: "Settings" }).count(), 0, "no conflict to settle")
      assert.equal(await shownWords(why.getByRole("combobox", { name: "User-Agent" })), "Hiddify/2.5.7")
    } finally {
      await a.close()
      await b.close()
    }
  })

  test("17. a refused Retry is a toast, and the agents show the stored one again", async () => {
    seedTroubled()
    const { page, close } = await open("sources/limit", { width: 1280, theme: "light" })
    try {
      await page.route(`**${BASE}api/sources/limit/refresh`, (route) => route.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." }) }))
      const why = await pickAgent(page, "Hiddify/2.5.7")
      await why.getByRole("button", { name: "Retry", exact: true }).click()
      await page.locator("[data-sonner-toast]").filter({ hasText: "bad User-Agent" }).waitFor()
      await until(async () => (await shownWords(why.getByRole("combobox", { name: "User-Agent" }))) === "Happ/3.0.0", "the stored agent shows again")
      assert.equal(daemon.source("limit").origin.userAgent, "Happ/3.0.0")
    } finally {
      await close()
    }
  })

  test("17. Retry's agent is a draft of the open card: closing it drops a pick", async () => {
    seedTroubled()
    const { page, close } = await open("sources/limit", { width: 1280, theme: "light" })
    try {
      await pickAgent(page, "Hiddify/2.5.7")
      await page.keyboard.press("Escape")
      await page.getByRole("dialog").waitFor({ state: "detached" })
      await refreshButton(page).click()
      assert.equal(await shownWords(page.getByRole("dialog").getByRole("combobox", { name: "User-Agent" })), "Happ/3.0.0", "a pick the card closed on is gone")
      assert.equal(requests("POST", "api/sources/limit/refresh").length, 0)
    } finally {
      await close()
    }
  })

  test("17. Retry's agent is the open card's own: a refresh that works takes the card and the pick with it, and the next failure's card shows and sends the stored agent", async () => {
    const t = seedTroubled()
    const sources = new Map(t.sources.map((x) => [x.id, x]))
    const fetched = (fetch) => {
      const limit = daemon.source("limit")
      sources.set("limit", { ...limit, origin: { ...limit.origin, fetch: { ...limit.origin.fetch, ...fetch } } })
      daemon.set({ sources })
      daemon.refreshDocument("limit")
    }
    const { page, close } = await open("sources/limit", { width: 1280, theme: "light" })
    try {
      await pickAgent(page, "Hiddify/2.5.7")
      fetched({ failures: 0, failureCode: null, checkedAt: new Date().toISOString(), updatedAt: new Date().toISOString() })
      await headingOf(page).getByRole("button", { name: /^Refresh, last worked/ }).waitFor()
      assert.equal(await page.getByRole("dialog").count(), 0, "the card went with the failure")
      fetched({ failures: 1, failureCode: "http_404", checkedAt: new Date(Date.now() + 1000).toISOString() })
      await refreshButton(page).click()
      const agents = page.getByRole("dialog").getByRole("combobox", { name: "User-Agent" })
      assert.equal(await shownWords(agents), "Happ/3.0.0", "the new failure's card shows the stored agent, not the pick its card went with")
      await page.getByRole("dialog").getByRole("button", { name: "Retry", exact: true }).click()
      await until(() => requests("POST", "api/sources/limit/refresh").length === 1, "Retry refreshes")
      assert.deepEqual(JSON.parse(requests("POST", "api/sources/limit/refresh")[0].body), { userAgent: "Happ/3.0.0" }, "with the stored agent")
      assert.equal(daemon.source("limit").origin.userAgent, "Happ/3.0.0")
    } finally {
      await close()
    }
  })

  test("17. Retry's agent after a Retry and a Settings save is the one Settings stored, and Retry sends it", async () => {
    seedTroubled()
    const { page, close } = await open("sources/limit", { width: 1280, theme: "light" })
    try {
      const agents = page.getByRole("dialog").getByRole("combobox", { name: "User-Agent" })
      await pickAgent(page, "Hiddify/2.5.7")
      await page.getByRole("dialog").getByRole("button", { name: "Retry", exact: true }).click()
      await until(() => daemon.source("limit").origin.userAgent === "Hiddify/2.5.7", "Retry stored its agent")
      await until(async () => !(await page.getByRole("dialog").getByRole("button", { name: "Retry", exact: true }).isDisabled()), "Retry is answered")
      assert.equal(await shownWords(agents), "Hiddify/2.5.7", "the stored agent")
      await page.keyboard.press("Escape")
      await page.getByRole("button", { name: "Settings for Limit" }).click()
      const settings = page.getByRole("dialog", { name: "Settings" })
      await settings.getByRole("button", { name: "Advanced", exact: true }).click()
      await settings.getByRole("combobox", { name: "User-Agent" }).click()
      await page.getByRole("option", { name: "v2rayN/7.13.2", exact: true }).click()
      await settings.getByRole("button", { name: "Save", exact: true }).click()
      await until(() => daemon.source("limit").origin.userAgent === "v2rayN/7.13.2", "Settings stored its agent")
      await settings.waitFor({ state: "hidden" })
      /* Retry's refresh runs, and fails again, so Refresh takes a press once more. */
      const limit = daemon.source("limit")
      daemon.set({ sources: new Map(daemon.sources().map((x) => [x.id, x.id === "limit" ? { ...limit, origin: { ...limit.origin, fetch: { ...limit.origin.fetch, checkedAt: new Date().toISOString() } } } : x])) })
      daemon.refreshDocument("limit")
      await until(async () => !(await refreshButton(page).isDisabled()), "the refresh it ran has checked")
      await refreshButton(page).click()
      assert.equal(await shownWords(agents), "v2rayN/7.13.2", "the card opened again shows the agent Settings stored")
      await page.getByRole("dialog").getByRole("button", { name: "Retry", exact: true }).click()
      await until(() => requests("POST", "api/sources/limit/refresh").length === 2, "Retry refreshes again")
      assert.deepEqual(JSON.parse(requests("POST", "api/sources/limit/refresh")[1].body), { userAgent: "v2rayN/7.13.2" }, "with the agent shown")
      assert.equal(daemon.source("limit").origin.userAgent, "v2rayN/7.13.2", "and Settings' agent stays")
    } finally {
      await close()
    }
  })

  test("17. after Retry stores an agent while every read of the state fails, Retry stays Pending on the agent sent and sends nothing over the stored one, until a read lands", async () => {
    seedTroubled()
    const { page, close } = await open("sources/limit", { width: 1280, theme: "light", clock: true })
    try {
      const why = await pickAgent(page, "Hiddify/2.5.7")
      const retry = why.getByRole("button", { name: "Retry", exact: true })
      const agents = why.getByRole("combobox", { name: "User-Agent" })
      await page.route("**/api/state", (r) => r.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ kind: "unavailable", reason: "unavailable", message: "State busy." }) }))
      await retry.click()
      await until(() => daemon.source("limit").origin.userAgent === "Hiddify/2.5.7", "Retry stored its agent")
      await page.waitForTimeout(1500)
      assert.equal(await retry.isDisabled(), true, "Retry waits for a read that shows what it stored")
      assert.equal(await retry.getAttribute("aria-busy"), "true")
      assert.equal(await shownWords(agents), "Hiddify/2.5.7", "the agent sent stays in sight")
      await retry.click({ force: true }).catch(() => {})
      await page.waitForTimeout(300)
      assert.equal(requests("POST", "api/sources/limit/refresh").length, 1, "nothing goes again over the stored agent")
      await page.unroute("**/api/state")
      await page.clock.fastForward(10_000)
      await until(async () => !(await retry.isDisabled()), "a read lands and Retry is answered")
      assert.equal(await shownWords(agents), "Hiddify/2.5.7", "the agent shown is the one stored")
    } finally {
      await close()
    }
  })

  test("17. with every read of the state held, a refused Retry is its toast at once, and Retry and Refresh take a press again", async () => {
    seedTroubled()
    const { page, close } = await open("sources/limit", { width: 1280, theme: "light" })
    try {
      const why = await pickAgent(page, "Hiddify/2.5.7")
      await page.route("**/api/state", () => new Promise(() => {}))
      await page.route(`**${BASE}api/sources/limit/refresh`, (route) => route.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." }) }))
      const retry = why.getByRole("button", { name: "Retry", exact: true })
      await retry.click()
      await toast(page, "bad User-Agent").waitFor({ timeout: 3_000 })
      await until(async () => !(await retry.isDisabled()), "Retry takes a press again")
      assert.equal(await retry.locator("[data-slot=spinner]").count(), 0, "no spinner on Retry")
      await page.keyboard.press("Escape")
      const refresh = refreshButton(page)
      assert.equal(await refresh.isDisabled(), false, "Refresh takes a press")
      assert.equal(await refresh.locator("[data-slot=spinner]").count(), 0, "and isn't turning for a refresh that was refused")
    } finally {
      await close()
    }
  })

  test("17. Retry's agent while its write is out is the one sent, whatever another tab stores, and the stored one once it is answered", async () => {
    seedTroubled()
    const a = await open("sources/limit", { width: 1280, theme: "light" })
    const b = await open("sources/limit", { width: 1280, theme: "light" })
    /* Makes A read the state again, as a return to its tab does, once it holds `agent`. */
    const readAgain = async (agent) => {
      await a.page.waitForTimeout(2_100)
      const read = a.page.waitForResponse(async (r) => r.url().endsWith("api/state") && (await r.text()).includes(`"userAgent":"${agent}"`))
      await a.page.evaluate(() => window.dispatchEvent(new Event("visibilitychange")))
      await read
      await a.page.waitForTimeout(200)
    }
    const bRetries = async (agent) => {
      const before = requests("POST", "api/sources/limit/refresh").length
      await b.page.getByRole("dialog").getByRole("combobox", { name: "User-Agent" }).click()
      await b.page.getByRole("option", { name: agent, exact: true }).click()
      await b.page.getByRole("dialog").getByRole("button", { name: "Retry", exact: true }).click()
      await until(() => requests("POST", "api/sources/limit/refresh").length === before + 1 && daemon.source("limit").origin.userAgent === agent, `B stored ${agent}`)
    }
    try {
      const held = await holdWrites(a.page)
      await refreshButton(a.page).click()
      const why = a.page.getByRole("dialog")
      const agents = why.getByRole("combobox", { name: "User-Agent" })
      const aRetries = async () => {
        const count = held.count
        await why.getByRole("button", { name: "Retry", exact: true }).click()
        await until(() => held.count === count + 1, "A's write is held")
      }
      const aAnswered = async (body) => {
        const before = requests("POST", "api/sources/limit/refresh").length
        held.release()
        await until(() => requests("POST", "api/sources/limit/refresh").length === before + 1, "A's refresh lands")
        assert.deepEqual(JSON.parse(requests("POST", "api/sources/limit/refresh").at(-1).body), body)
        await until(async () => !(await agents.isDisabled()), "A's Retry is answered")
      }
      await aRetries()
      await refreshButton(b.page).click()
      await bRetries("v2rayN/7.13.2")
      await readAgain("v2rayN/7.13.2")
      assert.ok(await agents.isDisabled(), "A's agents take no click while its write is out")
      assert.equal(await shownWords(agents), "Happ/3.0.0", "A shows the agent it sent, not B's")
      await aAnswered({ userAgent: "Happ/3.0.0" })
      assert.equal(await shownWords(agents), "Happ/3.0.0", "A shows the stored agent, its own")
      await agents.click()
      await a.page.getByRole("option", { name: "Hiddify/2.5.7", exact: true }).click()
      await aRetries()
      await aAnswered({ userAgent: "Hiddify/2.5.7" })
      await bRetries("v2rayN/7.13.2")
      await readAgain("v2rayN/7.13.2")
      assert.equal(await shownWords(agents), "v2rayN/7.13.2", "A's card, still open, follows the agent stored after its Retry")
    } finally {
      await a.close()
      await b.close()
    }
  })

  test("17. a failing run longer than the refreshes the daemon sends says it last worked before the oldest sent, and never takes the list's last change for a refresh that worked", async () => {
    const t = seedTroubled()
    const hoursAgo = (h) => new Date(Date.now() - h * 3600e3).toISOString()
    const steady = daemon.source("steady")
    daemon.set({ sources: new Map(daemon.sources().map((x) => [x.id, x.id === "steady" ? { ...steady, origin: { ...steady.origin, fetch: { ...steady.origin.fetch, updatedAt: hoursAgo(24 * 30), failures: 51, failureCode: "http_503" } } } : x])) })
    const run = [...Array.from({ length: 51 }, (_, i) => ({ at: hoursAgo(6 * i), outcome: "failed", failureCode: "http_503", wanExcluded: false, added: 0, changed: 0, missing: 0 })),
      { at: hoursAgo(6 * 51), outcome: "unchanged", failureCode: null, wanExcluded: false, added: 0, changed: 0, missing: 0 }]
    t.refreshes.set("steady", run.slice(0, 50))
    const { page, close } = await open("sources/steady", { width: 1280 })
    try {
      const why = page.getByRole("dialog")
      await refreshButton(page).click()
      await why.getByText("12d+ ago", { exact: true }).waitFor()
      assert.match(await failureOf(why), /^HTTP 503 \| Last worked 12d\+ ago \| Next in /)
    } finally {
      await close()
    }
  })

  /** Pauses a source and sets its fetch state and its refreshes, as the daemon keeps them once its retention has deleted the oldest. */
  const retained = (id, fetch, refreshes) => {
    const t = seedTroubled()
    const x = daemon.source(id)
    daemon.set({ sources: new Map(daemon.sources().map((y) => [y.id, y.id === id ? { ...x, origin: { ...x.origin, paused: true, fetch: { ...x.origin.fetch, ...fetch } } } : y])) })
    t.refreshes.set(id, refreshes)
  }
  const hoursAgo = (h) => new Date(Date.now() - h * 3600e3).toISOString()

  test("17. a source whose whole refresh history the daemon deleted still says when a refresh last worked", async () => {
    retained("steady", { failures: 1, failureCode: "timeout", checkedAt: hoursAgo(24 * 40), updatedAt: hoursAgo(24 * 50) }, [])
    const { page, close } = await open("sources/steady", { width: 1280 })
    try {
      const why = page.getByRole("dialog")
      await refreshButton(page).click()
      await until(() => requests("GET", "api/sources/steady/refreshes").length > 0, "the history is read")
      await why.getByText("40d+ ago", { exact: true }).waitFor({ timeout: 3000 })
      assert.equal(await failureOf(why), "Timeout | Last worked 40d+ ago | Refresh Off")
    } finally {
      await close()
    }
  })

  test("17. Refresh is Pending from the click until the daemon has checked again, the spinner in its sign's place, its label staying, and where motion is reduced the label says Refreshing…; a screen reader hears Refreshing… either way", async () => {
    for (const reducedMotion of ["no-preference", "reduce"]) {
      seedTroubled()
      const { page, close } = await open("sources/steady", { width: 1280 })
      try {
        await page.emulateMedia({ reducedMotion })
        const refresh = refreshButton(page)
        assert.equal(await refresh.getAttribute("aria-label"), "Refresh, last worked 24 minutes ago")
        await refresh.click()
        await until(async () => (await refresh.getAttribute("aria-label")) === "Refreshing…", `${reducedMotion}: Refreshing… is heard`)
        assert.equal(await shownWords(refresh), reducedMotion === "reduce" ? "Refreshing…" : "24m ago", `${reducedMotion}: the label`)
        assert.equal(await refresh.locator("svg.lucide-refresh-cw").count(), 0, `${reducedMotion}: the spinner stands in the sign's place`)
        assert.equal(await refresh.locator("[data-slot=spinner]").evaluate((el) => getComputedStyle(el).animationName !== "none"), reducedMotion !== "reduce", `${reducedMotion}: the spinner turns where motion isn't reduced`)
        await until(() => requests("POST", "api/sources/steady/refresh").every((r) => r.status !== 0), `${reducedMotion}: the refresh answered`)
        await page.waitForTimeout(200)
        assert.equal(await refresh.getAttribute("aria-label"), "Refreshing…", `${reducedMotion}: still running once its write has landed`)
        assert.deepEqual([await refresh.isDisabled(), await refresh.evaluate((el) => getComputedStyle(el).opacity)], [true, "0.5"], `${reducedMotion}: at half strength and taking no press for as long as it runs`)
        const sent = requests("POST", "api/sources/steady/refresh").length
        await refresh.click({ force: true })
        await page.waitForTimeout(200)
        assert.equal(requests("POST", "api/sources/steady/refresh").length, sent, `${reducedMotion}: a press while it runs sends nothing`)
        const steady = daemon.source("steady")
        daemon.set({ sources: new Map(daemon.sources().map((x) => [x.id, x.id === "steady" ? { ...steady, origin: { ...steady.origin, fetch: { ...steady.origin.fetch, checkedAt: new Date().toISOString() } } } : x])) })
        daemon.refreshDocument("steady")
        await until(async () => (await shownWords(refresh).catch(() => "")) === "just now", `${reducedMotion}: the time it checked`)
        assert.equal(await refresh.getAttribute("aria-label"), "Refresh, last worked just now")
      } finally {
        await close()
      }
    }
  })

  test("17. a refresh the daemon refuses after another change moved the view takes a press again, with no spinner", async () => {
    seedTroubled()
    const { page, close } = await open("sources/steady", { width: 1280 })
    try {
      const refresh = refreshButton(page)
      await until(async () => (await refresh.getAttribute("aria-label")) === "Refresh, last worked 24 minutes ago", "Refresh at rest")
      let answer
      const answered = new Promise((done) => { answer = done })
      await page.route(`**${BASE}api/sources/steady/refresh`, async (route) => {
        await answered
        const { revision } = await fetch(daemon.url + "api/state").then((r) => r.json())
        await route.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": String(revision) }, body: JSON.stringify({ kind: "validation", reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." }) })
      })
      await refresh.click()
      await until(async () => (await refresh.getAttribute("aria-label")) === "Refreshing…", "the press is out")
      const read = page.waitForResponse(async (r) => r.url().endsWith("api/state") && (await r.text()).includes('"name":"Elsewhere"'))
      daemon.renameSource("soon", "Elsewhere")
      await read
      await page.waitForTimeout(200)
      answer()
      await toast(page, "bad User-Agent").waitFor({ timeout: 3_000 })
      await until(async () => !(await refresh.isDisabled()), "Refresh takes a press again")
      assert.equal(await refresh.locator("[data-slot=spinner]").count(), 0, "and isn't turning for a refresh that was refused")
      assert.equal(await refresh.getAttribute("aria-label"), "Refresh, last worked 24 minutes ago")
    } finally {
      await close()
    }
  })

  test("17. each skipped entry shows once: under its config's heading, and the rest, with their configs, beside Search, each with its reason and the provider's Support", async () => {
    seedTroubled()
    const { page, close } = await open("sources/steady", { width: 1280 })
    const said = async () => (await card(page).innerText().catch(() => "")).replace(/\s+/g, " ").trim()
    try {
      const frankfurt = page.locator("tbody th", { hasText: "Frankfurt" }).getByRole("button", { name: "1 entry skipped. wg: Unsupported protocol (wireguard)" })
      await frankfurt.click()
      await until(async () => (await said()) === "Skipped 1 wg Unsupported protocol (wireguard) Support", "the config's mark: Skipped and its count, the entry and why, and Support")
      assert.equal(await card(page).getByRole("link", { name: /Support/ }).getAttribute("href"), "https://support.example/help")
      assert.equal(await card(page).locator("svg.lucide-list-x").count(), 1, "the count beside the list mark")
      await page.keyboard.press("Escape")
      const loose = page.getByRole("button", { name: "2 entries skipped. Warsaw · proxy: Insecure TLS. Entry 9: Malformed" })
      await loose.click()
      await until(async () => (await said()) === "Skipped 2 Warsaw · proxy Insecure TLS Entry 9 Malformed Support", "the mark beside Search")
      const rows = await rowsOf(card(page))
      assert.deepEqual(new Set(rows.map((r) => Math.round(r.value))).size, 1, "the count and the reasons share one edge")
      assert.deepEqual(new Set(rows.slice(1).map((r) => Math.round(r.nameLeft))).size, 1, "the entries line up with Skipped's name")
      assert.equal(await page.getByRole("button", { name: /entr(y|ies) skipped/ }).count(), 2)
      await page.keyboard.press("Escape")
      await page.mouse.move(0, 0)
      assert.equal(await toneOf(loose), "muted", "a quiet mark, with no warning colour")
      assert.equal(await page.locator("svg.lucide-circle-slash").count(), 0, "the ban sign is Block's alone")
    } finally {
      await close()
    }
  })

  test("17. pasted links' skipped entry and their empty block offer Paste again, which opens their settings at the Servers field, focused", async () => {
    seedTroubled()
    const pasted = daemon.source("pasted")
    daemon.set({ sources: new Map(daemon.sources().map((x) => [x.id, x.id === "pasted" ? { ...pasted, diagnostics: [{ entry: 2, code: "malformed_entry", field: "" }] } : x])) })
    const { page, close } = await open("sources/pasted", { width: 1280 })
    try {
      const servers = page.getByRole("dialog", { name: "Settings" }).getByRole("textbox", { name: "Servers" })
      await page.getByRole("button", { name: "1 entry skipped. Entry 2: Malformed" }).click()
      await card(page).getByRole("button", { name: "Paste again" }).click()
      await until(() => servers.evaluate((el) => el === document.activeElement), "the card's Paste again focuses the Servers field")
      await page.keyboard.press("Escape")
      await page.locator("main [data-slot=empty]").getByRole("button", { name: "Paste again" }).click()
      await until(() => servers.evaluate((el) => el === document.activeElement), "the empty block's Paste again focuses it too")
    } finally {
      await close()
    }
  })

  test("17. a provider's announcement that fits its line is words, with no card", async () => {
    seedTroubled()
    const { page, close } = await open("sources/steady", { width: 1280 })
    try {
      const text = "Maintenance on Sunday night, 02:00 to 04:00 UTC. Servers in Frankfurt may drop for a few minutes."
      await headingOf(page).getByText(text).waitFor()
      assert.equal(await headingOf(page).getByRole("button", { name: text }).count(), 0, "a line shown whole opens nothing")
    } finally {
      await close()
    }
  })

  test("17. the announcement is one line under the heading, three on a phone, and a tap opens a cut one in full", async () => {
    seedTroubled()
    const text = "Maintenance on Sunday night, 02:00 to 04:00 UTC. Servers in Frankfurt may drop for a few minutes, and Amsterdam follows on Monday night at the same hours, while the provider moves both to a new data centre."
    const steady = daemon.source("steady")
    daemon.set({ sources: new Map(daemon.sources().map((x) => [x.id, x.id === "steady" ? { ...steady, origin: { ...steady.origin, provider: { ...steady.origin.provider, announcement: text } } } : x])) })
    const { page, close } = await open("sources/steady", { width: 360, touch: true })
    try {
      const line = headingOf(page).getByRole("button", { name: text })
      const shownLines = await line.locator("span").first().evaluate((el) => Math.round(el.getBoundingClientRect().height / parseFloat(getComputedStyle(el).lineHeight)))
      assert.ok(shownLines >= 1 && shownLines <= 3, `${shownLines} lines`)
      await line.tap()
      await until(async () => (await card(page).innerText().catch(() => "")) === text, "the announcement in full")
    } finally {
      await close()
    }
  })

  test("17. a card fits the screen's width and wraps, breaking a word too long for it", async () => {
    seedTroubled()
    const text = `Maintenance${"x".repeat(160)}`
    const steady = daemon.source("steady")
    daemon.set({ sources: new Map(daemon.sources().map((x) => [x.id, x.id === "steady" ? { ...steady, origin: { ...steady.origin, provider: { ...steady.origin.provider, announcement: text } } } : x])) })
    const { page, close } = await open("sources/steady", { width: 360, touch: true })
    try {
      await headingOf(page).getByRole("button", { name: text }).tap()
      await until(async () => (await card(page).innerText().catch(() => "")) === text, "the announcement's card")
      const fit = await card(page).evaluate((el) => {
        const box = el.firstElementChild
        const words = document.createRange()
        words.selectNodeContents(box)
        return { right: Math.max(box.getBoundingClientRect().right, words.getBoundingClientRect().right), width: innerWidth, overflow: box.scrollWidth - box.clientWidth }
      })
      assert.ok(fit.right <= fit.width && fit.overflow <= 0, `the card's words stay on the screen: ${JSON.stringify(fit)}`)
    } finally {
      await close()
    }
  })
})

describe("cards on a phone", () => {
  test("17. on a phone a card stands under what opened it, 16 px in from the edge of the screen that is nearer", async () => {
    seedTroubled()
    const { page, close } = await open("sources/steady", { width: 390 })
    try {
      const under = async (trigger) => {
        const [t, c] = [await trigger.boundingBox(), await card(page).locator(":scope > *").boundingBox()]
        assert.ok(c.y >= t.y + t.height, "under it, never over it")
        return c
      }
      const mark = page.locator("button:has(svg.lucide-list-x)").first()
      await mark.click()
      await card(page).getByRole("link", { name: /Support/ }).waitFor()
      assert.equal(Math.round((await under(mark)).x), 16, "a mark left of the middle: its card's left edge 16 px in")
      await page.keyboard.press("Escape")
      const globe = page.locator("header").getByRole("button", { name: "WAN up" })
      await globe.click()
      await until(async () => (await card(page).innerText().catch(() => "")).includes("WAN"), "the globe's card")
      const right = await under(globe)
      assert.equal(Math.round(right.x + right.width), 390 - 16, "a mark right of the middle: its card's right edge 16 px in")
    } finally {
      await close()
    }
  })
})

describe("a source's page", () => {
  const settingsDialog = (page) => page.getByRole("dialog", { name: "Settings" })
  const refreshRadio = (page, label) => settingsDialog(page).getByRole("radiogroup", { name: "Refresh" }).getByRole("radio", { name: label, exact: true })
  const openAurora = async (page) => {
    await page.getByRole("button", { name: "Settings for Aurora" }).click()
    await refreshRadio(page, "12h").waitFor()
  }
  const sourceSave = (page) => settingsDialog(page).getByRole("button", { name: "Save", exact: true })
  /** Another writer saves Aurora, unheard: no event, so the page holds the view from before. */
  const changeAuroraUnheard = async (origin) => {
    const { revision } = await (await fetch(daemon.url + "api/state")).json()
    const a = daemon.source("aurora")
    daemon.set({ sources: new Map([...daemon.sources().map((x) => [x.id, x]), ["aurora", { ...a, origin: { ...a.origin, ...origin }, revision: a.revision + 1 }]]), revision: revision + 1 })
  }
  /** The daemon's refusal of a User-Agent at view revision `revision`, as a route answers for it. */
  const badAgent = (revision) => ({
    status: 422, contentType: "application/json", headers: { "Sidegate-Revision": String(revision) },
    body: JSON.stringify({ kind: "validation", reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." }),
  })
  /** Types `text` as a custom User-Agent under Advanced, and gives its field. */
  const customAgent = async (page, text) => {
    const dialog = settingsDialog(page)
    await dialog.getByRole("button", { name: "Advanced" }).click()
    await dialog.getByRole("combobox", { name: "User-Agent" }).click()
    await page.getByRole("option", { name: "Custom" }).click()
    const agent = dialog.getByLabel("Custom User-Agent")
    await agent.fill(text)
    return agent
  }
  const MINE = "https://mine.example/sub"
  const THEIRS = "https://theirs.example/sub"
  const OTHER = "https://other.example/sub"
  /** Another writer stores Aurora's URL or origin fields, and the page hears of it and reads the view. */
  const storedElsewhere = async (page, { url, ...origin }) => {
    if (url) daemon.secrets("aurora").url = url
    await changeAuroraUnheard(origin)
    daemon.changeSlot("eu", { name: "EU elsewhere" })
    await readsAnswered(page, "api/state")
  }
  /** Every PUT of Aurora the page sends, whether it reaches the daemon or not: `answer` says what becomes of each, by its body. */
  const sourcePuts = async (page, answer) => {
    const sent = []
    await page.route("**/api/sources/aurora", (r) => {
      if (r.request().method() !== "PUT") return r.continue()
      const headers = r.request().headers()
      const body = JSON.parse(r.request().postData())
      sent.push({ key: headers["idempotency-key"], ifMatch: headers["if-match"], body, rename: renaming(body) })
      const how = answer(body)
      if (how === "lose") return r.fetch().then(() => r.abort())
      if (how instanceof Promise) return how.then(() => r.continue())
      return how === "drop" ? r.abort() : how === "pass" ? r.continue() : r.fulfill(how)
    })
    return sent
  }
  /** A gate a PUT waits at until `open`. */
  const gate = () => {
    let open
    const held = new Promise((done) => { open = done })
    return { held, open }
  }
  const urlReads = () => requests("GET", "api/sources/aurora/url").length
  /** The change feed drops and comes back, on the page's clock, so the page reads every query again. */
  const reconnect = async (page) => {
    const before = requests("GET", "api/events").length
    daemon.dropEvents()
    await page.clock.fastForward(1_100)
    await until(() => requests("GET", "api/events").length > before && daemon.sseClients() > 0, "the feed back")
    await readsAnswered(page, "api/state")
  }
  const showUrl = async (page) => {
    await settingsDialog(page).getByRole("button", { name: "Show URL" }).click()
    await until(async () => (await settingsDialog(page).getByLabel("URL", { exact: true }).inputValue()) !== "", "the eye shows the URL")
  }
  const inlineRetry = (page) => settingsDialog(page).getByRole("button", { name: "Retry" })
  const TOO_LARGE = { status: 413, contentType: "text/html", body: "<h1>413 Request Entity Too Large</h1>" }
  /** The heading's rename to Borealis, which sends no URL, as a Save that changed one does. */
  const renaming = (body) => body.name === "Borealis" && body.origin.url === null
  const renameAurora = async (page) => {
    await page.locator("[data-slot=source-heading]").locator("button[aria-label^='Rename ']").click()
    await page.getByLabel("Name of Aurora").fill("Borealis")
    await page.keyboard.press("Enter")
  }
  /** The 30 s of a Save whose attempts all go unanswered, on the page's clock: its toast, past one a Retry dismissed on its way out. */
  const noAnswer = async (page) => {
    await page.clock.fastForward(31_000)
    const said = page.locator('[data-sonner-toast][data-removed="false"]').filter({ hasText: "timed out" })
    await until(async () => (await said.count()) === 1, "one no-answer toast")
    return said
  }

  test("17. a source's Save with no answer for 30 s: the dialog editable with Request timed out, the toast's Retry sends the same save under its key, and what was picked since stays", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let down = true
      await page.route("**/api/sources/aurora", (r) => (r.request().method() === "PUT" && down ? r.abort() : r.continue()))
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      assert.equal(await sourceSave(page).getAttribute("aria-busy"), "true", "Save is Pending")
      await page.keyboard.press("Escape")
      assert.equal(await settingsDialog(page).isVisible(), true, "nothing closes it while its Save is out")
      await page.clock.fastForward(31_000)
      const said = toast(page, "timed out")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Aurora timed out Retry")
      await settingsDialog(page).getByText("Request timed out", { exact: true }).waitFor()
      await refreshRadio(page, "3h").click()
      down = false
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.source("aurora").origin.refreshEverySeconds === 43200, "Retry lands the 12h")
      assert.equal(new Set(requests("PUT", "api/sources/aurora").map((r) => r.idempotencyKey)).size, 1, "under the key every attempt had")
      await until(async () => (await refreshRadio(page, "3h").getAttribute("aria-checked")) === "true" && (await sourceSave(page).isEnabled()), "the 3h picked since stays, to be saved")
    } finally {
      await close()
    }
  })

  test("17. a source's tab left while its Save is out, then refused, shows its typing and the refusal when its settings open again", async () => {
    const { page, close } = await open("servers")
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let release
      let refused = false
      const held = new Promise((done) => { release = done })
      await page.route("**/api/sources/aurora", async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        await held
        await r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "badRefresh", message: "Below zero" }) })
        refused = true
      })
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      release()
      await until(() => refused, "the refusal")
      await toast(page, "invalid refresh interval").waitFor()
      assert.equal((await toast(page, "invalid refresh interval").innerText()).replace(/\s+/g, " ").trim(), "Aurora invalid refresh interval", "its toast said it on All servers")
      await page.waitForTimeout(200)
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      assert.equal(await refreshRadio(page, "12h").getAttribute("aria-checked"), "true", "the typing kept")
      assert.equal(await settingsDialog(page).locator("[data-slot=field-error]").innerText(), "Invalid refresh interval", "and the refusal under it")
      assert.equal(await toast(page, "invalid refresh interval").count(), 1, "said once")
      assert.equal(daemon.source("aurora").origin.refreshEverySeconds, 21600, "nothing stored")
    } finally {
      await close()
    }
  })

  test("17. settings left after their Save's no answer open again on its typing, and its Retry is theirs", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      const keys = new Set()
      await page.route("**/api/sources/aurora", (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        keys.add(r.request().headers()["idempotency-key"])
        return r.abort()
      })
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      const said = await noAnswer(page)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      assert.equal(await refreshRadio(page, "Off").getAttribute("aria-checked"), "true", "the typing kept")
      await settingsDialog(page).getByText("Request timed out", { exact: true }).waitFor()
      await said.getByRole("button", { name: "Retry" }).click()
      await until(async () => (await sourceSave(page).getAttribute("aria-busy")) === "true", "the Retry is the settings' Save, Pending")
      await page.clock.fastForward(31_000)
      await settingsDialog(page).getByText("Request timed out", { exact: true }).waitFor()
      assert.equal(await refreshRadio(page, "Off").getAttribute("aria-checked"), "true")
      assert.equal(keys.size, 1, "every attempt under the first key")
    } finally {
      await close()
    }
  })

  test("17. a refusal of a Save left behind is a toast while reads fail, and its ring stands on a reopening before they return", async () => {
    const { page, close } = await open("servers")
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let release
      const held = new Promise((done) => { release = done })
      let refusedAt = 0
      let refused = false
      await page.route("**/api/sources/aurora", async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        await held
        await r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": String(refusedAt) }, body: JSON.stringify({ kind: "validation", reason: "badRefresh", message: "Below zero" }) })
        refused = true
      })
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      let down = true
      await page.route("**/api/state", (r) => (down ? r.abort() : r.continue()))
      refusedAt = (await (await fetch(daemon.url + "api/state")).json()).revision + 1
      release()
      await until(() => refused, "the refusal")
      await toast(page, "invalid refresh interval").waitFor()
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      assert.equal(await refreshRadio(page, "12h").getAttribute("aria-checked"), "true", "the typing kept")
      assert.equal(await settingsDialog(page).locator("[data-slot=field-error]").innerText(), "Invalid refresh interval", "the ring before the reads return")
      daemon.changeSlot("eu", { name: "EU later" }, { announce: false })
      down = false
      await until(async () => (await sourceSave(page).getAttribute("aria-busy")) === null, "the view at the refusal")
      assert.equal(await settingsDialog(page).locator("[data-slot=field-error]").innerText(), "Invalid refresh interval", "and after")
      assert.equal(await toast(page, "invalid refresh interval").count(), 1, "said once")
    } finally {
      await close()
    }
  })

  test("17. a Retry pressed elsewhere and refused is said once and keeps the typing", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let refuse = false
      await page.route("**/api/sources/aurora", (r) => (r.request().method() !== "PUT" ? r.continue() : !refuse ? r.abort()
        : r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "badRefresh", message: "Below zero" }) })))
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      const said = await noAnswer(page)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      refuse = true
      await said.getByRole("button", { name: "Retry" }).click()
      await toast(page, "invalid refresh interval").waitFor()
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      assert.equal(await refreshRadio(page, "12h").getAttribute("aria-checked"), "true", "the typing kept")
      assert.equal(await settingsDialog(page).locator("[data-slot=field-error]").innerText(), "Invalid refresh interval")
      assert.equal(await toast(page, "invalid refresh interval").count(), 1, "said once")
    } finally {
      await close()
    }
  })

  test("17. a refusal said while its settings were closed stands under its field on reopening after another writer moved a field the draft put back", async () => {
    await changeAuroraUnheard({ refreshEverySeconds: 10800 })
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let refuse = false
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/sources/aurora", async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        if (!refuse) return r.abort()
        await held
        await r.fulfill(badAgent(1))
      })
      await refreshRadio(page, "12h").click()
      await refreshRadio(page, "3h").click()
      const agent = await customAgent(page, "A".repeat(201))
      await sourceSave(page).click()
      const said = await noAnswer(page)
      refuse = true
      await said.getByRole("button", { name: "Retry" }).click()
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      release()
      await toast(page, "bad User-Agent").waitFor()
      await changeAuroraUnheard({ refreshEverySeconds: 43200 })
      daemon.changeSlot("eu", { name: "EU later" })
      await readsAnswered(page, "api/state")
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      await until(async () => (await agent.getAttribute("aria-invalid")) === "true", "the User-Agent ringed, Advanced open")
      assert.equal(await page.locator(`[id="${await agent.getAttribute("aria-describedby")}"]`).innerText(), "Bad User-Agent")
      assert.equal(await refreshRadio(page, "3h").getAttribute("aria-checked"), "true", "the 3h put back")
      assert.equal(await sourceSave(page).isDisabled(), true)
      assert.equal(await toast(page, "bad User-Agent").count(), 1, "said once")
    } finally {
      await close()
    }
  })

  test("17. a refusal said while Settings were closed stands under its field after a heading rename refused before they open again", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let refuse = false
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/sources/aurora", async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        if (JSON.parse(r.request().postData()).name === "Borealis") return r.fulfill({ status: 413, contentType: "text/html", body: "<h1>413 Request Entity Too Large</h1>" })
        if (!refuse) return r.abort()
        await held
        await r.fulfill(badAgent(1))
      })
      const agent = await customAgent(page, "A".repeat(201))
      await sourceSave(page).click()
      const said = await noAnswer(page)
      refuse = true
      await said.getByRole("button", { name: "Retry" }).click()
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      release()
      await toast(page, "bad User-Agent").waitFor()
      await changeAuroraUnheard({ refreshEverySeconds: 43200 })
      daemon.changeSlot("eu", { name: "EU later" })
      await readsAnswered(page, "api/state")
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      const heading = page.locator("[data-slot=source-heading]")
      await heading.locator("button[aria-label^='Rename ']").click()
      await page.getByLabel("Name of Aurora").fill("Borealis")
      await page.keyboard.press("Enter")
      await toast(page, "HTTP 413").waitFor()
      await openAurora(page)
      await until(async () => (await agent.getAttribute("aria-invalid")) === "true", "the User-Agent ringed, Advanced open")
      assert.equal(await page.locator(`[id="${await agent.getAttribute("aria-describedby")}"]`).innerText(), "Bad User-Agent")
      assert.equal(await sourceSave(page).isDisabled(), true)
      assert.equal(await refreshRadio(page, "12h").getAttribute("aria-checked"), "true", "Refresh is the other writer's")
      assert.equal(await toast(page, "bad User-Agent").count(), 1, "said once")
    } finally {
      await close()
    }
  })

  test("17. after no answer, Save of the unedited draft is its Retry, so a URL another writer stored meets a conflict and stays", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let mode = "drop"
      const sent = await sourcePuts(page, () => mode)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await url.fill(MINE)
      await sourceSave(page).click()
      await noAnswer(page)
      await storedElsewhere(page, { url: THEIRS })
      mode = "pass"
      await sourceSave(page).click()
      const said = toast(page, "changed since this Save")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Aurora changed since this Save", "no field it can print moved")
      assert.deepEqual([...new Set(sent.map((w) => w.key))].length, 1, "every attempt and the Retry under one key")
      assert.deepEqual([...new Set(sent.map((w) => w.ifMatch))], ['"1"'], "at the revision it was first sent at")
      assert.ok(requests("PUT", "api/sources/aurora").some((r) => r.status === 412), "the daemon finds another writer's")
      assert.equal(daemon.secrets("aurora").url, THEIRS, "their URL stays")
      assert.equal(await url.inputValue(), MINE, "the draft stays")
      assert.equal(await sourceSave(page).isEnabled(), true)
    } finally {
      await close()
    }
  })

  test("17. after no answer, Save of an unedited draft is its Retry though another writer changed its User-Agent too, so their URL stays; the next Save goes over theirs", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let mode = "drop"
      const sent = await sourcePuts(page, () => mode)
      await settingsDialog(page).getByLabel("URL", { exact: true }).fill(MINE)
      await sourceSave(page).click()
      await noAnswer(page)
      await storedElsewhere(page, { url: THEIRS, userAgent: "Other/1.0" })
      mode = "pass"
      await sourceSave(page).click()
      await toast(page, "changed since this Save").waitFor()
      assert.deepEqual([[...new Set(sent.map((w) => w.key))].length, [...new Set(sent.map((w) => JSON.stringify(w.body)))].length, [...new Set(sent.map((w) => w.ifMatch))]], [1, 1, ['"1"']], "one key, one body, the first revision")
      assert.equal(sent[0].body.origin.userAgent, "Happ/3.0.0", "the body as first sent, with the old User-Agent")
      assert.deepEqual([daemon.secrets("aurora").url, daemon.source("aurora").origin.userAgent], [THEIRS, "Other/1.0"])
      await sourceSave(page).click()
      await until(() => daemon.secrets("aurora").url === MINE, "the next Save goes over what is stored")
      assert.notEqual(sent.at(-1).key, sent[0].key, "a new write")
      assert.equal(sent.at(-1).ifMatch, '"2"')
      assert.equal(daemon.source("aurora").origin.userAgent, "Other/1.0", "their User-Agent, untouched here, stands")
    } finally {
      await close()
    }
  })

  test("17. a heading rename refused while Settings' Save has no answer leaves that Save's Request timed out and Retry, so their unedited Save meets another writer's URL and it stays", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let mode = "drop"
      const sent = await sourcePuts(page, (body) => (renaming(body) ? TOO_LARGE : mode))
      await settingsDialog(page).getByLabel("URL", { exact: true }).fill(MINE)
      await sourceSave(page).click()
      await noAnswer(page)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      await storedElsewhere(page, { url: THEIRS, userAgent: "Other/1.0" })
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await renameAurora(page)
      await toast(page, "HTTP 413").waitFor()
      await openAurora(page)
      const dialog = settingsDialog(page)
      const url = dialog.getByLabel("URL", { exact: true })
      assert.equal(await url.inputValue(), "", "a URL typed in an earlier opening is dots")
      await dialog.getByRole("button", { name: "Show URL" }).click()
      assert.equal(await url.inputValue(), MINE, "the draft kept")
      await dialog.getByText("Request timed out", { exact: true }).waitFor()
      assert.equal(await dialog.getByRole("button", { name: "Retry" }).isVisible(), true, "its Retry beside it")
      assert.equal(await toast(page, "HTTP 413").count(), 1, "the toast on screen is the rename's")
      mode = "pass"
      await sourceSave(page).click()
      await toast(page, "changed since this Save").waitFor()
      const saves = sent.filter((w) => !w.rename)
      assert.deepEqual([[...new Set(saves.map((w) => w.key))].length, [...new Set(saves.map((w) => JSON.stringify(w.body)))].length, [...new Set(saves.map((w) => w.ifMatch))]], [1, 1, ['"1"']], "one key, one body, the first revision")
      assert.ok(requests("PUT", "api/sources/aurora").some((r) => r.status === 412))
      assert.deepEqual([daemon.secrets("aurora").url, daemon.source("aurora").origin.userAgent], [THEIRS, "Other/1.0"], "theirs stays")
      assert.equal(await url.inputValue(), MINE)
    } finally {
      await close()
    }
  })

  test("17. a heading rename that lands after Settings' Save had no answer leaves that Save's Retry, and its replay meets the rename as a conflict; the next Save keeps the new name", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let mode = "drop"
      const sent = await sourcePuts(page, (body) => (renaming(body) ? "pass" : mode))
      await settingsDialog(page).getByLabel("URL", { exact: true }).fill(MINE)
      await sourceSave(page).click()
      await noAnswer(page)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await renameAurora(page)
      await until(() => daemon.source("aurora").name === "Borealis", "the rename lands")
      await readsAnswered(page, "api/state")
      await page.getByRole("button", { name: "Settings for Borealis" }).click()
      await refreshRadio(page, "12h").waitFor()
      mode = "pass"
      await sourceSave(page).click()
      const said = toast(page, "changed since this Save")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Aurora changed since this Save", "true after the owner's own rename too")
      const saves = sent.filter((w) => !w.rename)
      assert.deepEqual([[...new Set(saves.map((w) => w.key))].length, [...new Set(saves.map((w) => w.ifMatch))]], [1, ['"1"']])
      assert.equal(daemon.secrets("aurora").url, "https://aurora.example/sub", "nothing stored over the rename")
      await sourceSave(page).click()
      await until(() => daemon.secrets("aurora").url === MINE, "the next Save stores the URL")
      const last = sent.filter((w) => !w.rename).at(-1)
      assert.deepEqual([last.key === saves[0].key, last.ifMatch, last.body.name], [false, '"2"', "Borealis"], "a new write at the rename's revision, its name kept")
    } finally {
      await close()
    }
  })

  test("17. a rename's Retry leaves a URL put back as a keep, so another writer's URL survives the next Save", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      let renames = "drop"
      const sent = await sourcePuts(page, (body) => (renaming(body) ? renames : "pass"))
      await renameAurora(page)
      const unheard = await noAnswer(page)
      await openAurora(page)
      const dialog = settingsDialog(page)
      const url = dialog.getByLabel("URL", { exact: true })
      await dialog.getByRole("button", { name: "Show URL" }).click()
      await until(async () => (await url.inputValue()) === "https://aurora.example/sub", "the eye shows the stored URL")
      await url.fill(MINE)
      await url.fill("https://aurora.example/sub")
      renames = "pass"
      await unheard.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.source("aurora").name === "Borealis", "the rename's Retry lands")
      await storedElsewhere(page, { url: THEIRS })
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      await until(() => daemon.source("aurora").origin.refreshEverySeconds === 43200, "the Save lands")
      assert.deepEqual([sent.at(-1).body.origin.refreshEverySeconds, sent.at(-1).body.origin.url], [43200, null], "the Save's, the URL put back no change")
      assert.equal(daemon.secrets("aurora").url, THEIRS, "their URL stays")
    } finally {
      await close()
    }
  })

  test("17. a URL put back as the eye showed it, or emptied, keeps the stored one through every Retry, and emptying it masks it", async () => {
    for (const how of ["put back", "emptied"]) {
      const { page, close } = await open("sources/aurora", { clock: true })
      try {
        await openAurora(page)
        const sent = await sourcePuts(page, () => "drop")
        const dialog = settingsDialog(page)
        const url = dialog.getByLabel("URL", { exact: true })
        await dialog.getByRole("button", { name: "Show URL" }).click()
        await until(async () => (await url.inputValue()) === "https://aurora.example/sub", "the eye shows the stored URL")
        await url.fill(MINE)
        await url.fill(how === "put back" ? "https://aurora.example/sub" : "")
        if (how === "emptied") assert.equal(await url.inputValue(), "", "masked again")
        await refreshRadio(page, "Off").click()
        await sourceSave(page).click()
        const said = await noAnswer(page)
        await said.getByRole("button", { name: "Retry" }).click()
        await noAnswer(page)
        assert.ok(sent.length > 6, `${how}: every attempt and the Retry's`)
        assert.deepEqual([...new Set(sent.map((w) => w.body.origin.url))], [null], `${how}: no PUT sends a URL`)
      } finally {
        await close()
      }
    }
  })

  test("17. a source's landed Save closes its settings and leaves no URL read", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      await showUrl(page)
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      await settingsDialog(page).waitFor({ state: "detached" })
      const reads = urlReads()
      await reconnect(page)
      assert.equal(urlReads(), reads, "no URL read after the dialog went")
      await openAurora(page)
      assert.equal(await settingsDialog(page).getByLabel("URL", { exact: true }).inputValue(), "", "reopened with dots")
    } finally {
      await close()
    }
  })

  test("17. a Retry's replayed 200 after a reconnect closes the settings with no URL read from its receipt on", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      const door = gate()
      let how = "lose"
      await sourcePuts(page, () => how)
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      await until(() => daemon.source("aurora").origin.paused, "Off lands, its replies lost")
      await readsAnswered(page, "api/state")
      const said = await noAnswer(page)
      await showUrl(page)
      how = door.held
      await said.getByRole("button", { name: "Retry" }).click()
      await until(async () => (await sourceSave(page).getAttribute("aria-busy")) === "true", "the Retry out")
      await reconnect(page)
      const reads = urlReads()
      door.open()
      await settingsDialog(page).waitFor({ state: "detached" })
      await page.waitForTimeout(300)
      assert.equal(urlReads(), reads, "no URL read from the receipt on, the closing render included")
      await reconnect(page)
      assert.equal(urlReads(), reads, "nor after another reconnect")
    } finally {
      await close()
    }
  })

  test("17. a Save landed with its answers lost keeps Request timed out and Retry until Retry's own answer closes it", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let how = "lose"
      await sourcePuts(page, () => how)
      await showUrl(page)
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      await until(() => daemon.source("aurora").origin.paused, "Off lands, its replies lost")
      await noAnswer(page)
      await readsAnswered(page, "api/state")
      await page.clock.fastForward(1_000)
      await settingsDialog(page).getByText("Request timed out", { exact: true }).waitFor()
      assert.equal(await sourceSave(page).isDisabled(), true, "the read shows Off, so nothing is left to save")
      how = "pass"
      await inlineRetry(page).click()
      await settingsDialog(page).waitFor({ state: "detached" })
    } finally {
      await close()
    }
  })

  test("17. settings opened again while their Save is out wait on it and close when it lands", async () => {
    const { page, close } = await open("servers")
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      const door = gate()
      await sourcePuts(page, () => door.held)
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      assert.equal(await sourceSave(page).getAttribute("aria-busy"), "true", "sending")
      assert.equal(await settingsDialog(page).getByRole("button", { name: "Show URL" }).isDisabled(), true, "the eye waits")
      await page.keyboard.press("Escape")
      assert.equal(await settingsDialog(page).isVisible(), true, "nothing closes it while its Save is sent")
      door.open()
      await settingsDialog(page).waitFor({ state: "detached" })
    } finally {
      await close()
    }
  })

  test("17. a URL a Save replaced is read anew when the eye asks", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await openAurora(page)
      await showUrl(page)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await url.fill(MINE)
      await sourceSave(page).click()
      await settingsDialog(page).waitFor({ state: "detached" })
      const reads = urlReads()
      await openAurora(page)
      assert.equal(await url.inputValue(), "", "dots")
      await showUrl(page)
      assert.equal(await url.inputValue(), MINE, "the URL stored now")
      assert.equal(urlReads(), reads + 1, "read anew")
    } finally {
      await close()
    }
  })

  test("17. settings opened after a rename's no answer say no Request timed out", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await sourcePuts(page, () => "drop")
      await renameAurora(page)
      await noAnswer(page)
      await readsAnswered(page, "api/state")
      await page.clock.fastForward(1_000)
      await openAurora(page)
      assert.equal(await settingsDialog(page).getByText("Request timed out", { exact: true }).count(), 0, "the rename's toast says it, the settings don't")
      assert.equal(await inlineRetry(page).count(), 0)
    } finally {
      await close()
    }
  })

  test("17. settings opened before the toast's Retry close when that Retry lands, and read no URL after", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let how = "drop"
      await sourcePuts(page, () => how)
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      const said = await noAnswer(page)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      await settingsDialog(page).getByText("Request timed out", { exact: true }).waitFor()
      await showUrl(page)
      const door = gate()
      how = door.held
      await said.getByRole("button", { name: "Retry" }).click()
      await until(async () => (await sourceSave(page).getAttribute("aria-busy")) === "true", "sending")
      assert.equal(await settingsDialog(page).getByRole("button", { name: /URL/ }).isDisabled(), true, "the eye waits")
      assert.equal(await settingsDialog(page).getByLabel("URL", { exact: true }).inputValue(), "", "masked while it is out")
      const reads = urlReads()
      door.open()
      await settingsDialog(page).waitFor({ state: "detached" })
      await page.waitForTimeout(300)
      assert.equal(urlReads(), reads, "no URL read after")
    } finally {
      await close()
    }
  })

  test("17. a URL put back keeps the stored one through every Retry, and only a Retry's answer closes the settings", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let how = "drop"
      const sent = await sourcePuts(page, () => how)
      await showUrl(page)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await url.fill(MINE)
      await url.fill("https://aurora.example/sub")
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      const said = await noAnswer(page)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      how = "lose"
      await said.getByRole("button", { name: "Retry" }).click()
      await openAurora(page)
      await until(() => daemon.source("aurora").origin.paused, "the Retry commits, its replies lost")
      await noAnswer(page)
      await readsAnswered(page, "api/state")
      await page.clock.fastForward(1_000)
      await settingsDialog(page).getByText("Request timed out", { exact: true }).waitFor()
      assert.equal(await inlineRetry(page).isVisible(), true, "a read that shows what it sent closes nothing")
      how = "pass"
      await inlineRetry(page).click()
      await settingsDialog(page).waitFor({ state: "detached" })
      assert.deepEqual([...new Set(sent.map((w) => w.body.origin.url))], [null], "every PUT keeps the stored URL")
    } finally {
      await close()
    }
  })

  test("17. an emptied URL keeps the stored one and masks it, and only its Save's answer closes the settings", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let how = "drop"
      const sent = await sourcePuts(page, () => how)
      await showUrl(page)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await url.fill("")
      assert.equal(await settingsDialog(page).getByRole("button", { name: "Show URL" }).isVisible(), true, "masked, the eye off")
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      const said = await noAnswer(page)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      how = "lose"
      await said.getByRole("button", { name: "Retry" }).click()
      await openAurora(page)
      await until(() => daemon.source("aurora").origin.paused, "the Retry commits, its replies lost")
      await noAnswer(page)
      await readsAnswered(page, "api/state")
      await page.clock.fastForward(1_000)
      await showUrl(page)
      assert.equal(await url.inputValue(), "https://aurora.example/sub", "the stored URL kept")
      how = "pass"
      await inlineRetry(page).click()
      await settingsDialog(page).waitFor({ state: "detached" })
      assert.deepEqual([...new Set(sent.map((w) => w.body.origin.url))], [null])
    } finally {
      await close()
    }
  })

  test("17. settings left after no answer keep a URL typed since through that Save's landing elsewhere", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let how = "drop"
      const sent = await sourcePuts(page, () => how)
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      const said = await noAnswer(page)
      await settingsDialog(page).getByLabel("URL", { exact: true }).fill(MINE)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      how = "pass"
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.source("aurora").origin.paused, "the toast's Retry on All servers stores Off")
      await readsAnswered(page, "api/state")
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      assert.equal(await url.inputValue(), "", "dots, with the eye")
      await settingsDialog(page).getByRole("button", { name: "Show URL" }).click()
      assert.equal(await url.inputValue(), MINE, "the eye shows what was typed")
      await sourceSave(page).click()
      await until(() => daemon.secrets("aurora").url === MINE, "Save sends it")
      assert.equal(sent.at(-1).body.origin.url, MINE)
    } finally {
      await close()
    }
  })

  test("17. a URL put back stays a keep after no answer, and another writer's URL survives the next Save", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let how = "lose"
      const sent = await sourcePuts(page, () => how)
      await showUrl(page)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await url.fill(MINE)
      await url.fill("https://aurora.example/sub")
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      await until(() => daemon.source("aurora").origin.paused, "Off lands, its replies lost")
      await noAnswer(page)
      await storedElsewhere(page, { url: THEIRS })
      await page.clock.fastForward(1_000)
      assert.equal(await sourceSave(page).isDisabled(), true, "nothing left to save")
      how = "pass"
      await inlineRetry(page).click()
      await settingsDialog(page).waitFor({ state: "detached" })
      await openAurora(page)
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      await until(() => daemon.source("aurora").origin.refreshEverySeconds === 43200, "the next Save lands")
      assert.equal(sent.at(-1).body.origin.url, null)
      assert.equal(daemon.secrets("aurora").url, THEIRS, "their URL survives")
    } finally {
      await close()
    }
  })

  test("17. a Save seen stored before its no answer keeps a Retry the keyboard reaches inside Settings", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let how = "lose"
      const sent = await sourcePuts(page, () => how)
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      await until(() => daemon.source("aurora").origin.paused, "Off lands, its replies lost")
      await readsAnswered(page, "api/state")
      await page.route("**/api/state", (r) => r.abort())
      await noAnswer(page)
      assert.equal(await sourceSave(page).isDisabled(), true, "Save has nothing to save")
      how = "pass"
      for (let i = 0; i < 20 && !(await inlineRetry(page).evaluate((b) => b === document.activeElement)); i++) await page.keyboard.press("Tab")
      assert.equal(await inlineRetry(page).evaluate((b) => b === document.activeElement), true, "Tab reaches Retry inside the dialog")
      const before = sent.length
      await page.keyboard.press("Enter")
      await settingsDialog(page).waitFor({ state: "detached" })
      assert.deepEqual([new Set(sent.map((w) => w.key)).size, sent.length > before], [1, true], "Enter sent it again, under the first key")
      await page.clock.fastForward(1_000)
      await until(async () => (await toast(page, "").count()) === 0, "no toast left: the Retry took the no-answer toast, and its landing says nothing")
    } finally {
      await close()
    }
  })

  test("17. a Save answered while state reads fail leaves its settings closable, masked and reading no URL, on the first press or on a Retry", async () => {
    for (const retried of [false, true]) {
      daemon.reset()
      const { page, close } = await open("sources/aurora", { clock: true })
      try {
        await openAurora(page)
        let how = retried ? "drop" : "pass"
        await sourcePuts(page, () => how)
        await showUrl(page)
        await refreshRadio(page, "Off").click()
        await page.route("**/api/state", (r) => r.abort())
        await sourceSave(page).click()
        if (retried) {
          await noAnswer(page)
          how = "pass"
          await inlineRetry(page).click()
        }
        await until(() => daemon.source("aurora").origin.paused, "the 200")
        await until(async () => (await sourceSave(page).getAttribute("aria-busy")) === "true" && (await page.getByRole("button", { name: "Cancel" }).isEnabled()), "waiting for the view, closable")
        const url = settingsDialog(page).getByLabel("URL", { exact: true })
        assert.equal(await url.inputValue(), "", `${retried ? "a Retry" : "a first press"}: masked`)
        assert.equal(await settingsDialog(page).getByRole("button", { name: /URL/ }).isDisabled(), true, "the eye waits")
        const reads = urlReads()
        await page.clock.fastForward(180_000)
        assert.equal(urlReads(), reads, "no URL read while the view is behind")
        await page.keyboard.press("Escape")
        await settingsDialog(page).waitFor({ state: "detached" })
      } finally {
        await close()
      }
    }
  })

  test("17. settings typed again after a discarded Save say no Request timed out, and their Save is new", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      const sent = await sourcePuts(page, () => "drop")
      await page.route("**/api/state", (r) => r.abort())
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await url.fill(MINE)
      await sourceSave(page).click()
      await noAnswer(page)
      await settingsDialog(page).getByRole("button", { name: "Cancel" }).click()
      await settingsDialog(page).waitFor({ state: "detached" })
      await openAurora(page)
      assert.equal(await settingsDialog(page).getByText("Request timed out", { exact: true }).count(), 0, "Cancel dropped the Save's Retry")
      await url.fill(MINE)
      assert.equal(await settingsDialog(page).getByText("Request timed out", { exact: true }).count(), 0)
      await sourceSave(page).click()
      await until(() => sent.length === 7, "the new Save")
      assert.notEqual(sent.at(-1).key, sent[0].key, "a new write")
    } finally {
      await close()
    }
  })

  test("17. settings opened after a Save whose Retry also had no answer and was discarded say no Request timed out", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      await sourcePuts(page, () => "drop")
      await page.route("**/api/state", (r) => r.abort())
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      await noAnswer(page)
      await inlineRetry(page).click()
      await noAnswer(page)
      await settingsDialog(page).getByRole("button", { name: "Cancel" }).click()
      await settingsDialog(page).waitFor({ state: "detached" })
      await openAurora(page)
      assert.equal(await settingsDialog(page).getByText("Request timed out", { exact: true }).count(), 0)
      assert.equal(await refreshRadio(page, "6h").getAttribute("aria-checked"), "true", "what is stored")
    } finally {
      await close()
    }
  })

  test("17. another writer storing what an unanswered Save sent leaves its Retry, whose own 412 closes the settings", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let how = "drop"
      const sent = await sourcePuts(page, () => how)
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      await noAnswer(page)
      await storedElsewhere(page, { paused: true })
      await page.clock.fastForward(1_000)
      await settingsDialog(page).getByText("Request timed out", { exact: true }).waitFor()
      assert.equal(await inlineRetry(page).isVisible(), true, "a read that shows what it sent closes nothing")
      how = "pass"
      await inlineRetry(page).click()
      await settingsDialog(page).waitFor({ state: "detached" })
      assert.equal(sent.at(-1).ifMatch, '"1"', "the Retry at the first revision")
      assert.ok(requests("PUT", "api/sources/aurora").some((r) => r.status === 412), "its 412 agrees")
      assert.equal(await toast(page, "changed").count(), 0, "no toast")
    } finally {
      await close()
    }
  })

  test("17. a Save refused in front after an attempt may have landed keeps its typing, and Save with no edit is its Retry", async () => {
    for (const status of [403, 413]) {
      daemon.reset()
      const { page, close } = await open("sources/aurora", { clock: true })
      try {
        await openAurora(page)
        let how = "drop"
        const sent = await sourcePuts(page, () => how)
        const url = settingsDialog(page).getByLabel("URL", { exact: true })
        await url.fill(MINE)
        await sourceSave(page).click()
        await until(() => sent.length === 1, "the first attempt, dropped")
        how = { status, contentType: "text/html", body: `<h1>${status}</h1>` }
        await page.clock.fastForward(1_100)
        await settingsDialog(page).getByText("Request timed out", { exact: true }).waitFor()
        assert.equal(await url.inputValue(), MINE, `${status}: the typing kept`)
        how = "pass"
        await sourceSave(page).click()
        await until(() => daemon.secrets("aurora").url === MINE, "the Retry lands")
        assert.equal(new Set(sent.map((w) => w.key)).size, 1, `${status}: Save sent the first key`)
      } finally {
        await close()
      }
    }
  })

  test("17. a URL's Retry that meets another writer's rename is the conflict toast, and the dialog stays as typed", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let how = "drop"
      await sourcePuts(page, () => how)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await url.fill(MINE)
      await sourceSave(page).click()
      await noAnswer(page)
      const a = daemon.source("aurora")
      daemon.set({ sources: new Map([["aurora", { ...a, name: "Theirs", revision: a.revision + 1 }]]) })
      how = "pass"
      await inlineRetry(page).click()
      await toast(page, "changed since this Save").waitFor()
      assert.equal(await settingsDialog(page).isVisible(), true)
      assert.equal(await url.inputValue(), MINE)
      assert.equal(await sourceSave(page).isEnabled(), true)
    } finally {
      await close()
    }
  })

  test("17. settings opened during a Retry from elsewhere hold only until it is answered, their URL masked", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      let how = "drop"
      await sourcePuts(page, () => how)
      await settingsDialog(page).getByLabel("URL", { exact: true }).fill(MINE)
      await sourceSave(page).click()
      await page.clock.fastForward(5_000)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      const said = await noAnswer(page)
      const door = gate()
      how = door.held
      await said.getByRole("button", { name: "Retry" }).click()
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      assert.equal(await sourceSave(page).getAttribute("aria-busy"), "true", "sending")
      assert.equal(await url.inputValue(), "", "the URL typed before is dots")
      await page.keyboard.press("Escape")
      assert.equal(await settingsDialog(page).isVisible(), true, "held while sent")
      await page.route("**/api/state", (r) => r.abort())
      door.open()
      await until(() => daemon.secrets("aurora").url === MINE, "the 200")
      await until(async () => (await page.getByRole("button", { name: "Cancel" }).isEnabled()), "answered, it closes as ever")
      assert.equal(await url.inputValue(), "", "still masked")
      await page.keyboard.press("Escape")
      await settingsDialog(page).waitFor({ state: "detached" })
    } finally {
      await close()
    }
  })

  test("17. after no answer an invalid URL typed since leaves Retry reachable, and the old Save's landing keeps it", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let how = "drop"
      await sourcePuts(page, () => how)
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      await noAnswer(page)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await url.fill("http://not-https")
      assert.equal(await sourceSave(page).isDisabled(), true, "the invalid URL blocks Save")
      how = "pass"
      await inlineRetry(page).click()
      await until(() => daemon.source("aurora").origin.paused, "the Retry lands Off")
      await readsAnswered(page, "api/state")
      assert.equal(await settingsDialog(page).isVisible(), true, "the URL typed since is left to save")
      assert.equal(await url.inputValue(), "http://not-https")
      assert.equal(await refreshRadio(page, "Off").getAttribute("aria-checked"), "true")
    } finally {
      await close()
    }
  })

  test("17. a Save answered while reads fail masks the URL it showed and reads it no more", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let how = "drop"
      await sourcePuts(page, () => how)
      await refreshRadio(page, "Off").click()
      await sourceSave(page).click()
      await noAnswer(page)
      await showUrl(page)
      await page.route("**/api/state", (r) => r.abort())
      how = "pass"
      await inlineRetry(page).click()
      await until(() => daemon.source("aurora").origin.paused, "the 200")
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await until(async () => (await url.inputValue()) === "", "masked once it went out")
      const reads = urlReads()
      await page.clock.fastForward(60_000)
      assert.equal(urlReads(), reads, "and read no more")
    } finally {
      await close()
    }
  })

  test("17. a source deleted after its settings' Save had no answer leaves nothing to a source made again at its id", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("servers", { clock: true })
    try {
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      await sourcePuts(page, () => "drop")
      await settingsDialog(page).getByLabel("URL", { exact: true }).fill(MINE)
      await sourceSave(page).click()
      await noAnswer(page)
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await page.unroute("**/api/sources/aurora")
      await page.getByRole("button", { name: "Delete Aurora" }).click()
      const confirm = page.getByRole("alertdialog").getByRole("button", { name: "Delete", exact: true })
      await until(() => confirm.isEnabled(), "Delete enables")
      await confirm.click()
      await page.waitForURL(/\/servers$/)
      assert.equal(daemon.source("aurora"), undefined)
      const made = await fetch(daemon.url + "api/sources/aurora", {
        method: "PUT", headers: { "Content-Type": "application/json", "If-None-Match": "*", "Idempotency-Key": "another-client", "X-Sidegate-Csrf": "token-1" },
        body: JSON.stringify({ name: "Aurora", origin: { kind: "remote", url: THEIRS, userAgent: "Happ/3.0.0", refreshEverySeconds: 21600, paused: false } }),
      })
      assert.equal(made.status, 200, "another client makes a source at the same id")
      await nav(page).locator(`a[href$="/sources/aurora"]`).waitFor()
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      await openAurora(page)
      const dialog = settingsDialog(page)
      assert.equal(await dialog.getByText("Request timed out", { exact: true }).count(), 0, "no old Retry")
      assert.equal(await sourceSave(page).isDisabled(), true, "its own clean fields")
      await showUrl(page)
      assert.equal(await dialog.getByLabel("URL", { exact: true }).inputValue(), THEIRS, "its own URL")
    } finally {
      await close()
    }
  })

  /** Another client: Aurora's DELETE, and Aurora made again at its id with their URL. */
  const elsewhere = {
    remove: () => fetch(daemon.url + "api/sources/aurora", { method: "DELETE", headers: { "If-Match": '"1"', "Idempotency-Key": "another-client-delete", "X-Sidegate-Csrf": "token-1" } }),
    make: (name = "Aurora") => fetch(daemon.url + "api/sources/aurora", {
      method: "PUT", headers: { "Content-Type": "application/json", "If-None-Match": "*", "Idempotency-Key": "another-client-make", "X-Sidegate-Csrf": "token-1" },
      body: JSON.stringify({ name, origin: { kind: "remote", url: THEIRS, userAgent: "Happ/3.0.0", refreshEverySeconds: 21600, paused: false } }),
    }),
  }
  /** Aurora's Settings at its Save of my URL, every PUT dropped until `how` says else, left for All servers after its no answer. */
  const savedUnheardThenLeft = async (page) => {
    const how = { now: "drop" }
    const sent = await sourcePuts(page, () => how.now)
    await nav(page).locator(`a[href$="/sources/aurora"]`).click()
    await openAurora(page)
    await settingsDialog(page).getByLabel("URL", { exact: true }).fill(MINE)
    await sourceSave(page).click()
    await noAnswer(page)
    await page.goBack()
    await page.waitForURL(/\/servers$/)
    return { how, sent }
  }
  /** The source made again opens clean, and its Refresh alone saved keeps its own URL. */
  const madeAgainKeepsItsUrl = async (page, sent) => {
    await nav(page).locator(`a[href$="/sources/aurora"]`).waitFor()
    await nav(page).locator(`a[href$="/sources/aurora"]`).click()
    await openAurora(page)
    const dialog = settingsDialog(page)
    assert.deepEqual([await dialog.getByText("Request timed out", { exact: true }).count(), await inlineRetry(page).count(), await sourceSave(page).isDisabled()], [0, 0, true], "no old Retry, and its own clean fields")
    await refreshOnlyKeepsItsUrl(page, sent)
  }
  /** In the open settings of the source made again, its Refresh alone saved keeps its own URL. */
  const refreshOnlyKeepsItsUrl = async (page, sent) => {
    await refreshRadio(page, "12h").click()
    await sourceSave(page).click()
    await until(() => daemon.source("aurora").origin.refreshEverySeconds === 43200, "its 12h lands")
    assert.deepEqual([sent.at(-1).body.origin.url, sent.at(-1).ifMatch], [null, '"2"'], "the Save keeps the stored URL, at the one made again's revision")
    assert.equal(daemon.secrets("aurora").url, THEIRS, "the one made again keeps its own URL")
  }
  /** Aurora's Settings at its Save of my URL, which `answer` keeps out while another client deletes Aurora and makes Aurora II at
   * its id with their URL. The page reads Aurora II with that Save still out, as the dialog's name says, and every read of the
   * view after it is held until `free`. */
  const madeAgainUnderTheSave = async (page, answer) => {
    await watchToasts(page)
    const sent = await sourcePuts(page, answer)
    await nav(page).locator(`a[href$="/sources/aurora"]`).click()
    await openAurora(page)
    await settingsDialog(page).getByLabel("URL", { exact: true }).fill(MINE)
    const deleted = daemon.holdReads("api/state")
    await sourceSave(page).click()
    await until(() => sent.length > 0, "the Save is out")
    assert.equal((await elsewhere.remove()).status, 200, "another client deletes Aurora")
    assert.equal((await elsewhere.make("Aurora II")).status, 200, "and makes Aurora II at its id with their URL")
    assert.deepEqual([daemon.source("aurora").revision, daemon.secrets("aurora").url], [2, THEIRS])
    deleted()
    await settingsDialog(page).getByText("Aurora II", { exact: true }).waitFor()
    const free = daemon.holdReads("api/state")
    assert.equal(await sourceSave(page).getAttribute("aria-busy"), "true", "the Save before is still out")
    return { sent, free }
  }
  /** The settings over Aurora II once the Save before has ended: nothing of that Save, typed or kept, and no toast of it. */
  const nothingOfTheSaveBefore = async (page) => {
    const dialog = settingsDialog(page)
    await until(async () => (await sourceSave(page).getAttribute("aria-busy")) !== "true", "the Save before ends")
    assert.deepEqual([
      await dialog.getByLabel("URL", { exact: true }).inputValue(), await dialog.getByText("Request timed out", { exact: true }).count(), await inlineRetry(page).count(), await sourceSave(page).isDisabled(),
    ], ["", 0, 0, true], "my URL, its Request timed out and Retry go, and Save waits for an edit")
    await page.waitForTimeout(500)
    assert.deepEqual((await toastsMounted(page)).filter((words) => /timed out|changed in another tab/.test(words)), [], "no toast of the Save before, for a frame or longer")
  }
  /** From now on, the words of every toast the page mounts, however briefly. */
  const watchToasts = (page) => page.evaluate(() => {
    window.toastsMounted = []
    new MutationObserver((changes) => {
      for (const node of changes.flatMap((change) => [...change.addedNodes])) {
        if (!(node instanceof Element)) continue
        for (const body of [node, ...node.querySelectorAll("[data-reports]")]) if (body.matches("[data-reports]")) window.toastsMounted.push(body.textContent)
      }
    }).observe(document.body, { childList: true, subtree: true })
  })
  const toastsMounted = (page) => page.evaluate(() => window.toastsMounted)

  test("17. a source another client deleted after its settings' Save had no answer: the read that lists it no more takes that Save's Retry, and one made again at its id keeps its URL through a Refresh-only Save", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("servers", { clock: true })
    try {
      const { how, sent } = await savedUnheardThenLeft(page)
      assert.equal((await elsewhere.remove()).status, 200, "another client deletes Aurora")
      await nav(page).locator(`a[href$="/sources/aurora"]`).waitFor({ state: "detached" })
      const said = page.locator('[data-sonner-toast][data-removed="false"]').filter({ hasText: "timed out" })
      await until(async () => (await said.count()) === 0, "the deleted source's Retry goes with it")
      assert.equal((await elsewhere.make()).status, 200, "and makes a source at the same id")
      how.now = "pass"
      await madeAgainKeepsItsUrl(page, sent)
    } finally {
      await close()
    }
  })

  test("17. a source another client deleted, whose settings' Retry is answered 404 before the page reads it gone: one made again at its id keeps its URL through a Refresh-only Save", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("servers", { clock: true })
    try {
      const { how, sent } = await savedUnheardThenLeft(page)
      const free = daemon.holdReads("api/state")
      assert.equal((await elsewhere.remove()).status, 200, "another client deletes Aurora")
      how.now = "pass"
      await toast(page, "timed out").getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.log().some((r) => r.method === "PUT" && r.path === BASE + "api/sources/aurora" && r.status === 404), "the Retry is answered 404")
      free()
      await toast(page, "was deleted").waitFor()
      assert.equal((await elsewhere.make()).status, 200, "another client makes a source at the same id")
      await madeAgainKeepsItsUrl(page, sent)
    } finally {
      await close()
    }
  })

  test("17. a Save with no answer after the page read Aurora II made at its id leaves nothing of it before the next read, and a Refresh-only Save keeps Aurora II's URL", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("servers", { clock: true })
    try {
      const how = { now: "drop" }
      const { sent, free } = await madeAgainUnderTheSave(page, () => how.now)
      await page.clock.fastForward(31_000)
      await nothingOfTheSaveBefore(page)
      how.now = "pass"
      await refreshOnlyKeepsItsUrl(page, sent)
      assert.notEqual(sent.at(-1).key, sent[0].key, "under a key of its own")
      free()
    } finally {
      await close()
    }
  })

  test("17. a Save answered 412 after the page read Aurora II made at its id leaves nothing of it before the next read, and a Refresh-only Save keeps Aurora II's URL", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("servers", { clock: true })
    try {
      const held = gate()
      const { sent, free } = await madeAgainUnderTheSave(page, (body) => (body.origin.url === MINE ? held.held : "pass"))
      held.open()
      await until(() => daemon.log().some((r) => r.method === "PUT" && r.path === BASE + "api/sources/aurora" && r.status === 412), "the Save before reaches the daemon after Aurora II and is answered 412")
      await nothingOfTheSaveBefore(page)
      await refreshOnlyKeepsItsUrl(page, sent)
      free()
    } finally {
      await close()
    }
  })

  test("17. a Save refused on All servers after the page read Aurora II made at its id: its refusal is one toast for a toast's whole time, and Aurora II's settings open clean", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("servers", { clock: true })
    try {
      await watchToasts(page)
      const held = gate()
      let revision = 0
      await page.route("**/api/sources/aurora", async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        await held.held
        await r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": String(revision) }, body: JSON.stringify({ kind: "validation", reason: "badRefresh", message: "Below zero" }) })
      })
      const link = nav(page).locator(`a[href$="/sources/aurora"]`)
      await link.click()
      await openAurora(page)
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      assert.equal((await elsewhere.remove()).status, 200, "another client deletes Aurora")
      assert.equal((await elsewhere.make("Aurora II")).status, 200, "and makes Aurora II at its id")
      await until(async () => (await link.innerText()).includes("Aurora II"), "the page reads Aurora II")
      revision = (await (await fetch(daemon.url + "api/state")).json()).revision
      held.open()
      const said = toast(page, "invalid refresh interval").and(page.locator('[data-removed="false"]'))
      await said.waitFor()
      await page.clock.fastForward(3_500)
      assert.deepEqual([await said.count(), await said.isVisible()], [1, true], "still there 3.5 s on")
      await page.clock.fastForward(1_500)
      await until(async () => (await said.count()) === 0, "and gone by itself after its time")
      assert.equal((await toastsMounted(page)).filter((words) => words.includes("invalid refresh interval")).length, 1, "said once")
      await link.click()
      await openAurora(page)
      assert.deepEqual([
        await refreshRadio(page, "6h").getAttribute("aria-checked"), await settingsDialog(page).locator("[data-slot=field-error]").count(), await sourceSave(page).isDisabled(),
      ], ["true", 0, true], "Aurora II's own Refresh, no refusal, and Save waits for an edit")
    } finally {
      await close()
    }
  })

  /** Aurora's Settings' Save of 12h, on All servers by its ending: from the dialog, or from the toast's Retry after its no answer.
   * That PUT is held at the proxy while another client deletes Aurora and makes Aurora II at its id, and once the page reads
   * Aurora II the proxy refuses it with its HTML 413: one toast of Aurora for its whole time, and Aurora II's settings clean. */
  const tooLargeAfterAuroraII = async (page, from) => {
    await watchToasts(page)
    const held = gate()
    let [drop, out] = [from === "Retry", 0]
    await page.route("**/api/sources/aurora", async (r) => {
      if (r.request().method() !== "PUT") return r.continue()
      if (drop) return r.abort()
      out++
      await held.held
      await r.fulfill(TOO_LARGE)
    })
    const link = nav(page).locator(`a[href$="/sources/aurora"]`)
    await link.click()
    await openAurora(page)
    await refreshRadio(page, "12h").click()
    await sourceSave(page).click()
    if (from === "Retry") await noAnswer(page)
    await page.goBack()
    await page.waitForURL(/\/servers$/)
    if (from === "Retry") {
      drop = false
      await toast(page, "timed out").getByRole("button", { name: "Retry" }).click()
    }
    await until(() => out === 1, `the ${from} is held at the proxy`)
    assert.equal((await elsewhere.remove()).status, 200, "another client deletes Aurora")
    assert.equal((await elsewhere.make("Aurora II")).status, 200, "and makes Aurora II at its id")
    await until(async () => (await link.innerText()).includes("Aurora II"), "the page reads Aurora II")
    held.open()
    const said = toast(page, "HTTP 413").and(page.locator('[data-removed="false"]'))
    await said.waitFor()
    assert.match(await said.innerText(), /^Aurora\b(?! II)/, "of Aurora")
    await page.clock.fastForward(3_500)
    assert.deepEqual([await said.count(), await said.isVisible()], [1, true], "still there 3.5 s on")
    await page.clock.fastForward(1_500)
    await until(async () => (await said.count()) === 0, "and gone by itself after its time")
    assert.equal((await toastsMounted(page)).filter((words) => words.includes("HTTP 413")).length, 1, "said once")
    await link.click()
    await openAurora(page)
    assert.deepEqual([
      await refreshRadio(page, "6h").getAttribute("aria-checked"), await settingsDialog(page).getByText("Request timed out", { exact: true }).count(), await sourceSave(page).isDisabled(),
    ], ["true", 0, true], "Aurora II's own Refresh, no old Retry, and Save waits for an edit")
  }

  for (const from of ["Save", "Retry"]) {
    test(`17. a ${from} refused by the proxy's 413 on All servers after the page read Aurora II made at its id: its refusal is one toast of Aurora for a toast's whole time, and Aurora II's settings open clean`, async () => {
      daemon.changeSlot("eu", { exit: { kind: "direct" } })
      const { page, close } = await open("servers", { clock: true })
      try {
        await tooLargeAfterAuroraII(page, from)
      } finally {
        await close()
      }
    })
  }

  test("17. Aurora II's settings opened again after the Save before had no answer keep the URL typed there when the held read lands", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("servers", { clock: true })
    try {
      const { free } = await madeAgainUnderTheSave(page, () => "drop")
      await page.clock.fastForward(31_000)
      await until(async () => (await sourceSave(page).getAttribute("aria-busy")) !== "true", "the Save before ends")
      await settingsDialog(page).getByRole("button", { name: "Cancel" }).click()
      await settingsDialog(page).waitFor({ state: "detached" })
      await openAurora(page)
      const url = settingsDialog(page).getByLabel("URL", { exact: true })
      await url.fill(OTHER)
      assert.deepEqual([await url.inputValue(), await sourceSave(page).isEnabled()], [OTHER, true], "typed for Aurora II")
      assert.equal(requests("GET", "api/state").some((r) => r.status === 0), true, "a read held at the daemon")
      free()
      await readsAnswered(page, "api/state")
      assert.deepEqual([await url.inputValue(), await sourceSave(page).isEnabled()], [OTHER, true], "and kept when it lands")
    } finally {
      await close()
    }
  })

  test("17. a source's Save refused at a revision the view hasn't reached shows its refusal under its field at once, and the form takes typing again once the view reaches it", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await openAurora(page)
      const { revision } = await (await fetch(daemon.url + "api/state")).json()
      let at = revision
      await page.route("**/api/sources/aurora", (r) => (r.request().method() !== "PUT" ? r.continue()
        : r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": String(at) }, body: JSON.stringify({ kind: "validation", reason: "badRefresh", message: "Below zero" }) })))
      const error = settingsDialog(page).locator("[data-slot=field-error]")
      const free = daemon.holdReads("api/state")
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      await error.waitFor()
      assert.equal(await error.innerText(), "Invalid refresh interval")
      await until(async () => (await refreshRadio(page, "24h").isEnabled()) && (await sourceSave(page).getAttribute("aria-busy")) === null, "refused at the view's revision, the form takes edits with every read held")
      assert.equal(await sourceSave(page).isDisabled(), true, "and Save waits for an edit of the refused field")
      free()
      at = revision + 1
      await refreshRadio(page, "24h").click()
      assert.equal(await error.count(), 0, "an edit clears it")
      const asked = requests("GET", "api/state").length
      await sourceSave(page).click()
      await error.waitFor()
      assert.equal(await sourceSave(page).getAttribute("aria-busy"), "true", "past the view, Save is Pending while the view is read")
      await until(() => requests("GET", "api/state").length > asked, "the view read again past the refusal")
      assert.equal(await sourceSave(page).getAttribute("aria-busy"), "true", "a view older than the refusal frees nothing")
      daemon.changeSlot("eu", { name: "EU later" }, { announce: false })
      await until(async () => (await refreshRadio(page, "24h").isEnabled()) && (await sourceSave(page).getAttribute("aria-busy")) === null, "the view at the refusal frees the form")
      assert.equal(await error.innerText(), "Invalid refresh interval", "the refusal stays under the field")
      assert.equal(await toast(page, "").count(), 0, "and is no toast")
    } finally {
      await close()
    }
  })

  test("17. a source's rename, its Save and its Delete are one write: while one is out the others take no press, and only the one pressed is Pending", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("sources/aurora")
    try {
      const heading = page.locator("[data-slot=source-heading]")
      const name = heading.locator("button[aria-label^='Rename ']")
      const remove = heading.locator("button[aria-label^='Delete ']")
      const door = holdWrites(page, "**/api/sources/aurora")
      await door.install()
      await name.click()
      await page.getByLabel("Name of Aurora").fill("Borealis")
      await page.keyboard.press("Enter")
      await until(async () => (await name.getAttribute("aria-busy")) === "true", "the rename is Pending")
      assert.equal(await remove.isDisabled(), true, "Delete waits while the rename is out")
      await page.getByRole("button", { name: "Settings for Aurora" }).click()
      await refreshRadio(page, "12h").waitFor()
      assert.equal(await sourceSave(page).getAttribute("aria-busy"), null, "the dialog's Save isn't Pending for the rename")
      assert.equal(await sourceSave(page).isDisabled(), true, "and takes no press")
      await page.keyboard.press("Escape")
      await settingsDialog(page).waitFor({ state: "detached" })
      assert.match(await name.getAttribute("aria-label"), /Borealis/, "the dialog closes, and the rename's name stays")
      await door.release()
      await until(() => daemon.source("aurora").name === "Borealis", "the rename lands")
      await until(async () => (await name.getAttribute("aria-busy")) === null && (await remove.isEnabled()), "the rename landed, both take a press")

      await page.getByRole("button", { name: "Settings for Borealis" }).click()
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      await until(async () => (await sourceSave(page).getAttribute("aria-busy")) === "true", "Save is Pending")
      assert.equal(await name.getAttribute("aria-busy"), null, "the name isn't Pending for the dialog's Save")
      assert.equal(await name.isDisabled(), true, "and takes no press")
      assert.equal(await remove.isDisabled(), true, "nor does Delete")
      await door.release()
      await settingsDialog(page).waitFor({ state: "detached" })

      await until(() => remove.isEnabled(), "Save landed, Delete takes a press")
      await remove.click()
      const dialog = page.getByRole("alertdialog")
      const confirm = dialog.getByRole("button", { name: "Delete", exact: true })
      await until(() => confirm.isEnabled(), "Delete enables")
      await confirm.click()
      await until(async () => (await confirm.getAttribute("aria-busy")) === "true", "Delete is Pending")
      assert.equal(await name.getAttribute("aria-busy"), null, "the name isn't Pending for the Delete")
      assert.equal(await name.isDisabled(), true, "and takes no press")
      assert.equal(await settingsDialog(page).count(), 0, "the settings saved before stay closed")
      await door.release()
      await page.waitForURL(/\/servers$/)
      assert.equal(daemon.source("aurora"), undefined)
    } finally {
      await close()
    }
  })

  test("17. a source's Delete opened while its rename has no answer is not that rename's: Delete takes no press and isn't Pending, and Cancel closes it", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      const heading = page.locator("[data-slot=source-heading]")
      await page.route("**/api/sources/aurora", (r) => (r.request().method() === "PUT" ? r.abort() : r.continue()))
      await heading.locator("button[aria-label^='Rename ']").click()
      await page.getByLabel("Name of Aurora").fill("Borealis")
      await page.keyboard.press("Enter")
      await page.route("**/api/state", (r) => r.abort())
      await page.clock.fastForward(31_000)
      await toast(page, "timed out").waitFor()
      const remove = heading.locator("button[aria-label^='Delete ']")
      await until(() => remove.isEnabled(), "Delete takes a press once the rename has ended unanswered")
      await remove.click()
      const dialog = page.getByRole("alertdialog")
      const confirm = dialog.getByRole("button", { name: "Delete", exact: true })
      await confirm.waitFor()
      assert.equal(await confirm.getAttribute("aria-busy"), null, "the rename's write is not the Delete's Pending")
      assert.equal(await confirm.isDisabled(), true, "and Delete waits for it")
      await dialog.getByRole("button", { name: "Cancel" }).click()
      await dialog.waitFor({ state: "detached" })
      assert.ok(daemon.source("aurora"), "nothing deleted")
    } finally {
      await close()
    }
  })

  test("17. a source's settings opened and closed while its rename has no answer leave that rename's toast and its Retry", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      const heading = page.locator("[data-slot=source-heading]")
      let down = true
      await page.route("**/api/sources/aurora", (r) => (r.request().method() === "PUT" && down ? r.abort() : r.continue()))
      await heading.locator("button[aria-label^='Rename ']").click()
      await page.getByLabel("Name of Aurora").fill("Borealis")
      await page.keyboard.press("Enter")
      await page.route("**/api/state", (r) => (down ? r.abort() : r.continue()))
      await page.clock.fastForward(31_000)
      const unanswered = toast(page, "timed out")
      await unanswered.waitFor()
      await page.getByRole("button", { name: /^Settings for / }).click()
      await refreshRadio(page, "12h").waitFor()
      await page.keyboard.press("Escape")
      await settingsDialog(page).waitFor({ state: "detached" })
      await page.clock.fastForward(1000)
      assert.equal(await unanswered.count(), 1, "the rename's toast stays")
      down = false
      await unanswered.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.source("aurora").name === "Borealis", "Retry sends the rename")
    } finally {
      await close()
    }
  })

  test("17. a source's rename that meets another writer's is a single control's conflict: the name goes back to what is stored, the toast says theirs and mine with Keep mine, and Keep mine renames over the source as stored", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      const name = page.locator("[data-slot=source-heading]").locator("button[aria-label^='Rename ']")
      await name.click()
      await page.getByLabel("Name of Aurora").fill("Mine")
      let theirs = false
      await page.route("**/api/sources/aurora", async (r) => {
        if (r.request().method() === "PUT" && !theirs) {
          theirs = true
          daemon.renameSource("aurora", "Theirs")
        }
        await r.continue()
      })
      await page.keyboard.press("Enter")
      const said = toast(page, "changed to")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Aurora changed to Theirs Your value: Mine Keep mine")
      await until(async () => (await name.getAttribute("aria-label")) === "Rename Theirs", "the name goes back to what is stored")
      assert.equal(await page.getByRole("button", { name: /^(Save|Discard)$/ }).count(), 0, "no Save and no Discard")
      const origin = daemon.source("aurora").origin
      await said.getByRole("button", { name: "Keep mine" }).click()
      await until(() => daemon.source("aurora").name === "Mine", "Keep mine renames over what is stored")
      await until(async () => (await name.getAttribute("aria-label")) === "Rename Mine", "the heading shows it")
      assert.deepEqual(daemon.source("aurora").origin, origin, "and nothing else of the source moves")
      await until(async () => (await toast(page, "").count()) === 0, "Keep mine takes its toast")
    } finally {
      await close()
    }
  })

  test("17. a pasted source's Servers text the daemon refuses is the stock invalid field with its reason tied to it, once, and Save disabled until the next edit", async () => {
    seedTroubled()
    const { page, close } = await open("sources/pasted", { width: 1280 })
    try {
      await page.getByRole("button", { name: "Settings for Pasted" }).click()
      const servers = settingsDialog(page).getByRole("textbox", { name: "Servers" })
      await servers.fill("not a valid server")
      await sourceSave(page).click()
      await until(async () => (await servers.getAttribute("aria-invalid")) === "true", "the Servers field ringed")
      const reason = await servers.getAttribute("aria-describedby")
      assert.ok(reason, "its reason is tied to it")
      assert.equal(await page.locator(`[id="${reason}"]`).innerText(), "Unknown format")
      assert.equal(await settingsDialog(page).getByText("Unknown format", { exact: true }).count(), 1, "once")
      assert.equal(await sourceSave(page).isDisabled(), true, "Save disabled while the refused text stands")
      assert.equal(await toast(page, "").count(), 0, "and no toast")
      await servers.fill("trojan://pw@b.example:443#Borealis")
      assert.equal(await servers.getAttribute("aria-invalid"), null, "an edit clears it")
      assert.equal(await sourceSave(page).isEnabled(), true)
    } finally {
      await close()
    }
  })

  test("17. a subscription's Save refused for one changed field rings that field with the reason under it; refused with two changed it is a toast, and the dialog stays as typed", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await openAurora(page)
      await page.route("**/api/sources/aurora", (r) => (r.request().method() !== "PUT" ? r.continue()
        : r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." }) })))
      await settingsDialog(page).getByRole("button", { name: "Advanced" }).click()
      await settingsDialog(page).getByRole("combobox", { name: "User-Agent" }).click()
      await page.getByRole("option", { name: "Custom" }).click()
      const agent = settingsDialog(page).getByLabel("Custom User-Agent")
      await agent.fill("Mine/1.0")
      await sourceSave(page).click()
      await until(async () => (await agent.getAttribute("aria-invalid")) === "true", "the User-Agent ringed")
      assert.equal(await page.locator(`[id="${await agent.getAttribute("aria-describedby")}"]`).innerText(), "Bad User-Agent")
      assert.equal(await settingsDialog(page).locator("[data-slot=field-error]").count(), 1, "once")
      assert.equal(await toast(page, "").count(), 0)
      await agent.fill("Mine/2.0")
      await refreshRadio(page, "12h").click()
      await sourceSave(page).click()
      await toast(page, "bad User-Agent").waitFor()
      assert.equal(await settingsDialog(page).locator("[data-slot=field-error]").count(), 0, "a refusal that names no one field is the toast")
      assert.equal(await agent.inputValue(), "Mine/2.0", "the dialog stays as typed")
      assert.equal(await refreshRadio(page, "12h").getAttribute("aria-checked"), "true")
    } finally {
      await close()
    }
  })

  test("17. a refusal of the one field a Save changed stands under it after another writer moved a field it didn't touch", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      const { revision } = await (await fetch(daemon.url + "api/state")).json()
      let refuse = false
      await page.route("**/api/sources/aurora", (r) => (r.request().method() !== "PUT" ? r.continue() : !refuse ? r.abort() : r.fulfill(badAgent(revision))))
      const agent = await customAgent(page, "A".repeat(201))
      await sourceSave(page).click()
      const said = await noAnswer(page)
      await changeAuroraUnheard({ refreshEverySeconds: 43200 })
      daemon.changeSlot("eu", { name: "EU later" })
      await until(async () => (await refreshRadio(page, "12h").getAttribute("aria-checked")) === "true", "the other writer's Refresh read")
      refuse = true
      await said.getByRole("button", { name: "Retry" }).click()
      await until(async () => (await agent.getAttribute("aria-invalid")) === "true", "the User-Agent ringed")
      assert.equal(await page.locator(`[id="${await agent.getAttribute("aria-describedby")}"]`).innerText(), "Bad User-Agent")
      assert.equal(await toast(page, "bad User-Agent").count(), 0, "and no toast")
      assert.equal(await sourceSave(page).isDisabled(), true, "Save waits for the next edit")
      assert.equal(await refreshRadio(page, "12h").getAttribute("aria-checked"), "true", "Refresh is the other writer's")
    } finally {
      await close()
    }
  })

  test("17. a refusal drawn while the view lags stands under its press's field, though a field put back has since moved under it", async () => {
    await changeAuroraUnheard({ refreshEverySeconds: 10800 })
    const { page, close } = await open("sources/aurora")
    try {
      await openAurora(page)
      let release
      const held = new Promise((done) => { release = done })
      let refusedAt = 0
      await page.route("**/api/sources/aurora", async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        await held
        await r.fulfill(badAgent(refusedAt))
      })
      await refreshRadio(page, "12h").click()
      await refreshRadio(page, "3h").click()
      const agent = await customAgent(page, "A".repeat(201))
      await sourceSave(page).click()
      await changeAuroraUnheard({ refreshEverySeconds: 43200 })
      daemon.changeSlot("eu", { name: "EU later" })
      await readsAnswered(page, "api/state")
      refusedAt = (await (await fetch(daemon.url + "api/state")).json()).revision + 1
      const free = daemon.holdReads("api/state")
      release()
      await until(async () => (await agent.getAttribute("aria-invalid")) === "true", "the User-Agent ringed while the view is behind the refusal")
      assert.equal(await page.locator(`[id="${await agent.getAttribute("aria-describedby")}"]`).innerText(), "Bad User-Agent")
      assert.equal(await sourceSave(page).getAttribute("aria-busy"), "true", "Pending while the view catches up")
      free()
      daemon.changeSlot("eu", { name: "EU later still" }, { announce: false })
      await until(async () => (await sourceSave(page).getAttribute("aria-busy")) === null, "the view at the refusal frees the form")
      assert.equal(await agent.getAttribute("aria-invalid"), "true", "the ring stays under the User-Agent")
      assert.equal(await refreshRadio(page, "3h").getAttribute("aria-checked"), "true", "the 3h put back, which another writer has since moved")
      assert.equal(await toast(page, "bad User-Agent").count(), 0)
    } finally {
      await close()
    }
  })

  test("17. a Save's refusal that arrives after its fields were edited since is a toast, and the new draft stays saveable", async () => {
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      await openAurora(page)
      let refuse = false
      await page.route("**/api/sources/aurora", (r) => (r.request().method() !== "PUT" ? r.continue() : !refuse ? r.abort() : r.fulfill(badAgent(1))))
      const dialog = settingsDialog(page)
      const original = daemon.source("aurora").origin.userAgent
      const agent = await customAgent(page, "A".repeat(201))
      await sourceSave(page).click()
      const said = await noAnswer(page)
      const url = dialog.getByLabel("URL", { exact: true })
      await url.fill("https://aurora.example/other")
      await agent.fill(original)
      refuse = true
      await said.getByRole("button", { name: "Retry" }).click()
      await toast(page, "bad User-Agent").waitFor()
      assert.equal(await dialog.locator("[data-slot=field-error]").count(), 0, "no field holds a refusal of what it no longer says")
      assert.equal(await url.getAttribute("aria-invalid"), null, "the URL typed since isn't ringed")
      assert.equal(await sourceSave(page).isEnabled(), true, "and it can be saved")
    } finally {
      await close()
    }
  })

  test("17. a source's Delete refused as referenced is the Delete's alone: its settings opened after the Delete is cancelled show no refusal", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("sources/aurora")
    try {
      await page.getByRole("button", { name: "Delete Aurora" }).click()
      const dialog = page.getByRole("alertdialog")
      const confirm = dialog.getByRole("button", { name: "Delete", exact: true })
      await until(() => confirm.isEnabled(), "Delete enables")
      daemon.changeSlot("eu", { exit: { kind: "source", source: "aurora" } }, { announce: false })
      await confirm.click()
      await dialog.getByRole("region", { name: "Used by" }).getByText("EU", { exact: true }).waitFor()
      await dialog.getByRole("button", { name: "Cancel" }).click()
      await dialog.waitFor({ state: "detached" })
      await openAurora(page)
      assert.equal(await settingsDialog(page).locator("[data-slot=field-error]").count(), 0, "no refusal under the settings")
      assert.equal(await settingsDialog(page).getByText(/Used by/).count(), 0, "the Delete's words stay with the Delete")
      assert.equal(await sourceSave(page).getAttribute("aria-busy"), null)
    } finally {
      await close()
    }
  })

  test("17. a source's Save whose conflict leaves no difference is no conflict: another writer stored the same refresh, and the dialog closes with no toast", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await openAurora(page)
      await refreshRadio(page, "12h").click()
      let theirs = false
      await page.route("**/api/sources/aurora", async (r) => {
        if (r.request().method() === "PUT" && !theirs) {
          theirs = true
          await changeAuroraUnheard({ refreshEverySeconds: 43200 })
        }
        await r.continue()
      })
      await sourceSave(page).click()
      await until(() => requests("PUT", "api/sources/aurora").some((r) => r.status === 412), "the daemon finds another writer's")
      await settingsDialog(page).waitFor({ state: "detached" })
      await page.waitForTimeout(300)
      assert.equal(await toast(page, "").count(), 0, "nothing to say")
      assert.equal(requests("PUT", "api/sources/aurora").length, 1)
    } finally {
      await close()
    }
  })

  test("17. a source's Save over another writer's is a conflict: untouched fields take what is stored, the draft stays, and the toast names the touched field that moved, never the URL", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await openAurora(page)
      await refreshRadio(page, "12h").click()
      await settingsDialog(page).getByLabel("URL", { exact: true }).fill("https://mine.example/sub")
      await changeAuroraUnheard({ paused: true, userAgent: "Other/1.0" })
      await sourceSave(page).click()
      const said = toast(page, "changed in another tab")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Aurora changed in another tab Refresh off", "the refresh that moved, as stored; the URL never")
      assert.doesNotMatch(await said.innerText(), /mine\.example|example/, "no URL")
      await until(async () => (await refreshRadio(page, "Off").getAttribute("aria-checked")) === "false" && (await refreshRadio(page, "12h").getAttribute("aria-checked")) === "true", "the draft stays")
      assert.equal(await sourceSave(page).isEnabled(), true)
      await sourceSave(page).click()
      await until(() => daemon.source("aurora").origin.refreshEverySeconds === 43200, "Save sends the draft over what is stored")
      assert.equal(daemon.source("aurora").origin.paused, false, "the reader's refresh, one field, over theirs")
      assert.equal(daemon.source("aurora").origin.userAgent, "Other/1.0", "the other writer's User-Agent, untouched here, stands")
      assert.equal(daemon.secrets("aurora").url, "https://mine.example/sub")
    } finally {
      await close()
    }
  })

  test("17. a source's settings save from their dialog with no rule, the tab has no Save and no settings, and Refresh off pauses the source", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await rowOf(page, "Paris").waitFor()
      for (const name of ["URL", "User-Agent"]) assert.equal(await page.getByLabel(name, { exact: true }).count(), 0, `no ${name} on the tab`)
      assert.equal(await page.getByRole("radiogroup", { name: "Refresh" }).count(), 0, "no Refresh on the tab")
      assert.equal(await page.getByRole("button", { name: /^(Save|Discard)$/ }).count(), 0, "no Save and no Discard on the tab")
      await page.getByRole("button", { name: "Settings for Aurora" }).click()
      const dialog = page.getByRole("dialog", { name: "Settings" })
      await dialog.getByLabel("URL", { exact: true }).waitFor()
      await dialog.getByRole("radiogroup", { name: "Refresh" }).getByRole("radio", { name: "Off" }).click()
      await dialog.getByRole("button", { name: "Save", exact: true }).click()
      await until(() => requests("PUT", "api/sources/aurora").length === 1, "the save")
      const body = JSON.parse(requests("PUT", "api/sources/aurora")[0].body)
      assert.deepEqual(Object.keys(body).toSorted(), ["name", "origin"])
      assert.equal(body.origin.paused, true)
      await dialog.waitFor({ state: "hidden" })
      await page.getByRole("button", { name: "Settings for Aurora" }).click()
      assert.equal(await dialog.getByRole("radio", { name: "Off" }).getAttribute("aria-checked"), "true")
      await page.keyboard.press("Escape")
      await dialog.waitFor({ state: "hidden" })
      await refreshButton(page).focus()
      await until(async () => (await card(page).innerText().catch(() => "")).replace(/\s+/g, " ").trim() === "Refresh Off", "Refresh's card says it is off")
    } finally {
      await close()
    }
  })

  test("17. every server a source lists shows on All servers and on its source's tab, none dimmed and none ticked", async () => {
    daemon.set({ catalogue: [server("l-ams", "Amsterdam", "NL", 15), server("l-ori", "EDGE-Amsterdam", "NL", 40)] })
    const { page, close } = await open("servers")
    try {
      await rowOf(page, "EDGE-Amsterdam").waitFor()
      await nav(page).locator(`a[href$="/sources/aurora"]`).click()
      const row = rowOf(page, "EDGE-Amsterdam")
      await row.waitFor()
      assert.doesNotMatch(await row.getAttribute("class"), /opacity-50/)
      assert.equal(await page.getByRole("checkbox").count(), 0, "a source's tab has no boxes")
      assert.doesNotMatch(await page.locator("main").innerText(), /kept/i, "nothing says kept")
    } finally {
      await close()
    }
  })

  test("17. at 390 px a server's name keeps a readable width beside its status words, and opens its details", async () => {
    const lost = server("l-ams", "Amsterdam", "NL", 15, { presence: "missing", udp: "failed", missingSince: new Date().toISOString() })
    daemon.set({ catalogue: [lost, server("l-fra", "Frankfurt", "DE", 9), server("l-par", "Paris", "FR", 19)] })
    const { page, close } = await open("sources/aurora", { width: 390 })
    try {
      const name = page.getByRole("button", { name: "Amsterdam", exact: true })
      await name.waitFor()
      assert.ok((await name.boundingBox()).width >= 60, `the name is ${(await name.boundingBox()).width}px wide`)
      assert.ok(await rowOf(page, "Amsterdam").getByText("Missing", { exact: true }).count(), "the status words are still there, truncated")
      const [words, cell] = await rowOf(page, "Amsterdam").getByText("Missing", { exact: true }).locator("..").evaluate((el) => [el.getBoundingClientRect().right, el.closest("td").getBoundingClientRect().right])
      assert.ok(words <= cell + 0.5, `the status words give way inside the name's cell: they end at ${words}px, the cell at ${cell}px`)
      assert.ok(await page.evaluate(() => document.scrollingElement.scrollWidth <= innerWidth), "no sideways scrolling")
      await name.click()
      await page.getByRole("dialog").getByText("Amsterdam", { exact: true }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. a source can be added as pasted text, and its text never comes back", async () => {
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog")
      await dialog.getByLabel("Source", { exact: true }).fill("trojan://pw@paste.example.net:443#Paste one")
      await dialog.getByLabel("Name").fill("Pasted")
      assert.equal(requests("POST", "api/sources/preview").length, 0, "pasted text goes out only with Add")
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      await dialog.waitFor({ state: "hidden" })
      const stored = daemon.sources().find((s) => s.name === "Pasted")
      assert.equal(stored.origin.kind, "manual")
      assert.doesNotMatch(await page.locator("body").innerText(), /paste\.example\.net/)
      await nav(page).getByRole("link", { name: /Pasted/ }).click()
      await page.getByRole("button", { name: "Settings for Pasted" }).click()
      const text = page.getByRole("dialog", { name: "Settings" }).locator(`[id="source-${stored.id}-text"]`)
      await text.waitFor()
      assert.equal(await text.inputValue(), "", "write-only")
      assert.equal(await page.getByLabel("URL").count(), 0, "a source can't change origin")
    } finally {
      await close()
    }
  })

  test("17. a source's Delete lists each slot it would be refused for with its own Exit; repointing one there is its write, Pending in the place kept at the row's end, and takes the row away; then Removes names what goes, and Delete takes the source", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await page.getByRole("button", { name: "Delete Aurora" }).click()
      const dialog = page.getByRole("alertdialog")
      const usedBy = dialog.getByRole("region", { name: "Used by" })
      await usedBy.getByText("EU", { exact: true }).waitFor()
      const remove = dialog.getByRole("button", { name: "Delete", exact: true })
      assert.equal(await remove.isDisabled(), true, "Delete waits while a slot would refuse it")
      assert.equal(await shownWords(usedBy.getByRole("combobox", { name: "Exit of EU" })), "All Aurora", "the slot's own Exit, a group the delete removes")
      assert.equal(await dialog.getByRole("region", { name: "Removes" }).count(), 0)
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/slots/eu", async (r) => { await held; await r.continue() }, { times: 1 })
      const select = usedBy.getByRole("combobox", { name: "Exit of EU" })
      let before = null
      await until(async () => {
        const now = await select.boundingBox()
        const still = before !== null && now.x === before.x && now.width === before.width
        before = now
        return still
      }, "the dialog has opened to its size")
      await usedBy.getByRole("combobox", { name: "Exit of EU" }).click()
      await page.locator("[data-radix-popper-content-wrapper]").locator('[data-value="direct"]').click()
      await usedBy.getByLabel("Repointing EU").waitFor()
      const during = await usedBy.getByRole("combobox", { name: "Exit of EU" }).boundingBox()
      assert.deepEqual([during.x, during.width], [before.x, before.width], "the spinner takes the place the row kept for it, and the select doesn't move")
      assert.equal(await usedBy.getByRole("combobox", { name: "Exit of EU" }).getAttribute("aria-disabled"), "true", "the row takes no second pick")
      assert.equal(await shownWords(usedBy.getByRole("combobox", { name: "Exit of EU" })), "Direct", "it shows the pick at full strength")
      release()
      await usedBy.waitFor({ state: "detached" })
      assert.deepEqual(daemon.slot("eu").exit, { kind: "direct" }, "the slot repointed")
      const removes = dialog.getByRole("region", { name: "Removes" })
      assert.equal((await removes.innerText()).replace(/\s+/g, " ").trim(), "Removes 3 All Aurora", "its servers as a count and the group by name")
      await until(() => remove.isEnabled(), "Delete enables")
      await remove.click()
      await until(() => new URL(page.url()).pathname === BASE + "servers", "back to All servers")
      assert.equal(daemon.source("aurora"), undefined)
      assert.equal(await toast(page, "").count(), 0, "a delete that landed says nothing")
    } finally {
      await close()
    }
  })

  test("17. a Delete refused for a slot the view doesn't list yet stands in the dialog: its blockers Pending while read, the failed read with Retry, and Delete takes no press until they are read", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("sources/aurora")
    try {
      await page.getByRole("button", { name: "Delete Aurora" }).click()
      const dialog = page.getByRole("alertdialog")
      const remove = dialog.getByRole("button", { name: "Delete", exact: true })
      await dialog.getByRole("region", { name: "Removes" }).waitFor()
      await until(() => remove.isEnabled(), "Delete enables")
      daemon.changeSlot("eu", { exit: { kind: "source", source: "aurora" } }, { announce: false })
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/state", async (r) => { await held; await r.fulfill({ status: 502, body: "Bad Gateway" }) })
      await remove.click()
      await until(() => daemon.log().some((r) => r.method === "DELETE" && r.status === 409), "the daemon refuses the delete")
      const usedBy = dialog.getByRole("region", { name: "Used by" })
      await usedBy.locator("[data-slot=skeleton]").first().waitFor()
      assert.equal(await dialog.getByRole("region", { name: "Removes" }).count(), 0, "nothing says the source is free")
      assert.equal(await remove.isDisabled(), true, "Delete takes no press while its blockers are read")
      assert.equal(await remove.getAttribute("aria-busy"), "true", "and is Pending")
      release()
      const failed = usedBy.getByRole("alert")
      await failed.getByText("HTTP 502", { exact: true }).waitFor()
      assert.equal(await remove.isDisabled(), true, "Delete takes no press while its blockers are unread")
      await page.unroute("**/api/state")
      await failed.getByRole("button", { name: "Retry" }).click()
      await usedBy.getByRole("combobox", { name: "Exit of EU" }).waitFor()
      assert.equal(await remove.isDisabled(), true, "read, the blocker holds Delete back")
      assert.equal(await toast(page, "").count(), 0, "the refusal stands in the dialog alone")
      assert.ok(daemon.source("aurora"))
    } finally {
      await close()
    }
  })

  test("17. a source's Delete refused for a slot whose read lands older than the refusal reads the view again until EU's row stands, Delete held back", () =>
    deleteRefusedOverAnOldRead("sources/aurora", "Aurora", { kind: "source", source: "aurora" }))

  test("17. a Delete with no answer for 30 s is its toast over the open dialog, whose Retry takes the press and sends the delete again under its key, the dialog staying", async () => {
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("sources/aurora", { clock: true })
    try {
      const sent = []
      let down = true
      await page.route("**/api/sources/aurora", (r) => {
        if (r.request().method() !== "DELETE") return r.continue()
        sent.push(r.request().headers()["idempotency-key"])
        return down ? r.abort() : r.continue()
      })
      await page.getByRole("button", { name: "Delete Aurora" }).click()
      const dialog = page.getByRole("alertdialog")
      await dialog.getByRole("region", { name: "Removes" }).waitFor()
      await dialog.getByRole("button", { name: "Delete", exact: true }).click()
      await page.clock.fastForward(31_000)
      const said = toast(page, "timed out")
      await said.waitFor()
      const before = sent.length
      await said.getByRole("button", { name: "Retry" }).click({ timeout: 2_000 })
      await until(() => sent.length > before, "Retry sends the delete again")
      assert.equal(await dialog.isVisible(), true, "the dialog stays under the toast")
      down = false
      await page.clock.fastForward(1_000)
      await until(() => new URL(page.url()).pathname === BASE + "servers", "the resend lands and the tab leaves")
      assert.equal(new Set(sent).size, 1, "one key for every attempt")
    } finally {
      await close()
    }
  })

  test("17. after a Delete has taken its source to All servers, another source picked opens and stays open", async () => {
    daemon.addAcme()
    daemon.changeSlot("eu", { exit: { kind: "direct" } })
    const { page, close } = await open("sources/aurora")
    try {
      await page.getByRole("button", { name: "Delete Aurora" }).click()
      await page.getByRole("alertdialog").getByRole("button", { name: "Delete", exact: true }).click()
      await until(() => new URL(page.url()).pathname === BASE + "servers", "back to All servers")
      const visits = []
      page.on("framenavigated", (frame) => { if (frame === page.mainFrame()) visits.push(new URL(frame.url()).pathname) })
      await nav(page).locator(`a[href="${BASE}sources/acme"]`).click()
      await headingOf(page).getByText("Acme", { exact: true }).waitFor()
      await page.waitForTimeout(300)
      assert.deepEqual(visits, [BASE + "sources/acme"], "the pick opens its tab, and nothing sends it back")
    } finally {
      await close()
    }
  })
})

describe("the Servers table", () => {
  const H = (hours) => new Date(Date.now() - hours * 3600e3).toISOString()
  const names = (page) => page.locator("tbody tr td:nth-child(3)").allInnerTexts()

  test("17. Status, Country, Name, Source, Protocol, Ping, Stability and In use, with no Score, best first", async () => {
    const { page, close } = await open("servers")
    try {
      await rowOf(page, "Frankfurt").waitFor()
      const headers = (await page.locator("thead th").allInnerTexts()).map((h) => h.replace(/\s+/g, " ").trim())
      assert.deepEqual(headers.slice(0, 8), ["Status", "Country", "Name", "Source", "Protocol", "Ping p50 / p95", "Stability", "In use"], "Source shows with one source")
      assert.ok(!headers.some((x) => /Score|Up 7d/.test(x)), "All servers has no Score")
      assert.match(await rowOf(page, "Frankfurt").innerText(), /VLESS · TCP/)
      assert.equal(await page.getByRole("button", { name: "Group by config" }).count(), 0)
      assert.deepEqual((await names(page)).map((x) => x.trim()), ["Frankfurt", "Amsterdam", "Paris"], "best first by score")
    } finally {
      await close()
    }
  })

  test("17. a server both missing and unsupported reads Unsupported alike in its row, its details and the Exit menu, which takes no pick of it, and offers no Test", async () => {
    daemon.set({ catalogue: daemon.catalogue().map((l) => (l.id === "l-par" ? { ...l, presence: "missing", missingSince: new Date(Date.now() - 5 * 3600e3).toISOString(), supported: false } : l)) })
    const { page, close } = await open("servers")
    try {
      const row = rowOf(page, "Paris")
      await row.waitFor()
      const words = await row.innerText()
      assert.match(words, /Unsupported/)
      assert.doesNotMatch(words, /Missing/, "one word for the one standing")
      assert.equal(await row.getByRole("button", { name: /^Test/ }).count(), 0, "no Test")
      await row.getByRole("button", { name: "Paris", exact: true }).click()
      const dialog = page.getByRole("dialog", { name: "Paris" })
      const health = dialog.locator("[data-slot=row]").filter({ hasText: "Health" })
      await health.waitFor()
      assert.equal((await health.innerText()).replace(/\s+/g, " ").trim(), "Health Unsupported", "the details say what the row says")
      assert.equal(await dialog.getByRole("button", { name: /^Test/ }).count(), 0, "no Test in the details")
      await page.keyboard.press("Escape")
      await page.goto(daemon.url)
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const option = page.getByRole("dialog").locator('[data-value="server:l-par"]')
      await option.waitFor()
      assert.match(await option.innerText(), /Unsupported/)
      assert.doesNotMatch(await option.innerText(), /Missing/)
      assert.equal(await option.getAttribute("aria-disabled"), "true", "the menu takes no pick of it")
    } finally {
      await close()
    }
  })

  test("17. no header sorts and no table has a # column, on any tab", async () => {
    const order = (page) => page.locator("tbody tr").evaluateAll((trs) => trs.map((tr) => tr.innerText.replace(/\s+/g, " ").trim()))
    const { page, close } = await open("servers")
    try {
      for (const tab of ["servers", "groups/all-aurora", "sources/aurora"]) {
        await page.goto(daemon.url + tab)
        await rowOf(page, "Paris").waitFor()
        const headers = await page.locator("thead th").evaluateAll((ths) => ths.map((th) => th.textContent.trim()))
        assert.ok(!headers.includes("#"), `${tab}: no #, ${headers}`)
        assert.equal(await page.getByRole("button", { name: /^Sort/ }).count(), 0, `${tab}: no sort button`)
        const before = await order(page)
        for (const th of await page.locator("thead th").all()) {
          if (!(await th.isVisible())) continue
          await th.click({ position: { x: 2, y: 2 } })
          const filter = th.getByRole("button", { name: /^Filter servers by / })
          if (await filter.count()) {
            await filter.click()
            await page.getByRole("dialog").waitFor()
            await page.keyboard.press("Escape")
            await page.getByRole("dialog").waitFor({ state: "detached" })
          }
        }
        assert.deepEqual(await order(page), before, `${tab}: clicking every header leaves the rows as they were`)
      }
    } finally {
      await close()
    }
  })

  test("17. a Test's answer stands beside its gauge: the ping, or a Dot and Failed or Not run with the reason in its card; in the details the reading's age stands in the subtitle and Test has its word", async () => {
    const answer = { verdict: "succeeded" }
    const { page, close } = await open("servers", { width: 1280 })
    await page.route((url) => url.pathname.endsWith("/probe"), (route) => route.fulfill({ status: 200, contentType: "application/json", headers: { "Sidegate-Revision": "1" },
      body: JSON.stringify({ lineage: "l-fra", verdict: answer.verdict, pingMs: answer.verdict === "succeeded" ? 222 : null, newConnectionMs: null, at: new Date().toISOString() }) }))
    try {
      const row = rowOf(page, "Frankfurt")
      await row.getByRole("button", { name: "Test", exact: true }).click()
      await row.getByText("222 ms", { exact: true }).waitFor()
      for (const [verdict, words, reason] of [["failed", "Failed", "No response"], ["unknown", "Not run", "No route"]]) {
        answer.verdict = verdict
        await row.getByRole("button", { name: "Test", exact: true }).click()
        const shown = row.getByRole("button", { name: `${words}: ${reason}` })
        await shown.hover()
        await until(async () => (await card(page).innerText().catch(() => "")) === reason, `${words}'s reason in its card`)
        await page.mouse.move(0, 0)
      }
      await row.getByRole("button", { name: "Frankfurt", exact: true }).click()
      const dialog = page.getByRole("dialog", { name: "Frankfurt" })
      await dialog.getByText("Country").waitFor()
      assert.equal(await dialog.locator("svg.lucide-history").count(), 1, "the reading's age beside the history sign")
      const labels = await dialog.locator("[data-slot=row] > span:first-child").allInnerTexts()
      assert.ok(labels.includes("Health") && !labels.includes("Checked"), `no Checked row: ${labels.join(", ")}`)
      await dialog.getByRole("button", { name: "Test", exact: true }).click()
      await dialog.getByRole("button", { name: "Not run: No route" }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. a Test with no answer for 30 s is its toast over the open details, whose Retry takes the press, and the details stay to show the answer", async () => {
    const { page, close } = await open("servers", { width: 1280, clock: true })
    try {
      let down = true
      await page.route("**/api/servers/l-fra/probe", (r) => (down ? r.abort() : r.continue()))
      await rowOf(page, "Frankfurt").getByRole("button", { name: "Frankfurt", exact: true }).click()
      const dialog = page.getByRole("dialog", { name: "Frankfurt" })
      await dialog.getByText("Country").waitFor()
      await dialog.getByRole("button", { name: "Test", exact: true }).click()
      await page.clock.fastForward(31_000)
      const said = toast(page, "timed out")
      await said.waitFor()
      down = false
      await said.getByRole("button", { name: "Retry" }).click({ timeout: 2_000 })
      await dialog.getByText("222 ms", { exact: true }).waitFor()
      assert.equal(await dialog.isVisible(), true, "the press on the toast leaves the details open")
    } finally {
      await close()
    }
  })

  test("17. a Test's answer stands its minute and then goes, so a one-off answer never reads as current", async () => {
    const { page, close } = await open("servers", { width: 1280, clock: true })
    try {
      const row = rowOf(page, "Frankfurt")
      await row.getByRole("button", { name: "Test", exact: true }).click()
      await row.getByText("222 ms", { exact: true }).waitFor()
      await page.clock.fastForward(30_000)
      assert.equal(await row.getByText("222 ms", { exact: true }).count(), 1, "half a minute on, it stands")
      await page.clock.fastForward(31_000)
      await row.getByText("222 ms", { exact: true }).waitFor({ state: "detached" })
    } finally {
      await close()
    }
  })

  test("17. a server's dialog gives the reason for its status to the keyboard and a screen reader", async () => {
    const frankfurt = server("l-fra", "Frankfurt", "DE", 9)
    daemon.set({ catalogue: [server("l-ams", "Amsterdam", "NL", 15), { ...frankfurt, health: { ...frankfurt.health, verdict: "failed" } }] })
    const { page, close } = await open("servers")
    try {
      await rowOf(page, "Frankfurt").getByRole("button", { name: "Frankfurt", exact: true }).click()
      await page.getByRole("dialog").getByText("Offline", { exact: true }).waitFor()
      let reached = false
      for (let i = 0; i < 20 && !reached; i++) {
        await page.keyboard.press("Tab")
        reached = await page.evaluate(() => document.activeElement?.getAttribute("aria-label") === "Offline: Not responding")
      }
      assert.ok(reached, "Tab reaches the status, and its reason is part of its name")
      await until(async () => (await card(page).innerText().catch(() => "")) === "Not responding", "focus opens the reason's card")
    } finally {
      await close()
    }
  })

  test("17. a server with failovers and no reading this week shows Failovers and their count over the empty frame and its failover lines, in its name's card and its details alike, and one with only failed checks opens no card", async () => {
    const health = { ...server("l-fra", "Frankfurt", "DE", 9).health, incidents7d: 3 }
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9, { health }), server("l-ams", "Amsterdam", "NL", 15, { health: { ...health, incidents7d: 4 } })], analytics: failoversOnly })
    const { page, close } = await open("servers")
    try {
      await page.getByRole("button", { name: "Frankfurt", exact: true }).hover()
      const card = page.locator("[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
      await card.locator("[data-slot=chart-frame]").waitFor()
      assert.equal(await readoutOf(card), "7d Failovers 2", "the window and the week's failovers, the ping keys held unseen")
      assert.equal(await card.locator("[data-slot=chart-frame] .recharts-reference-line").count(), 2, "the frame draws the week's two failover lines")
      await card.getByLabel("No reading in 7 days").waitFor()
      await page.mouse.move(0, 0)
      await card.waitFor({ state: "detached" })
      await page.getByRole("button", { name: "Frankfurt", exact: true }).click()
      const dialog = page.getByRole("dialog")
      await dialog.locator("[data-slot=chart-frame]").waitFor()
      assert.equal(await readoutOf(dialog), "7d Failovers 2", "the details read the same")
      assert.equal(await dialog.locator("[data-slot=chart-frame] .recharts-reference-line").count(), 2)
      await page.keyboard.press("Escape")
      await page.mouse.move(0, 0)
      await page.getByRole("button", { name: "Amsterdam", exact: true }).hover()
      await until(() => daemon.log().filter((r) => r.method === "GET" && r.path === BASE + "api/analytics").length >= 2, "Amsterdam's week asked for")
      await page.waitForTimeout(700)
      assert.equal(await page.locator("[data-radix-popper-content-wrapper]").count(), 0, "failed checks alone open no card that says Failovers 0")
    } finally {
      await close()
    }
  })

  /** A week of pings for Frankfurt, read slower than the hover card's open delay, and the heights `selector` takes frame by frame. */
  const at = (h) => new Date(Math.floor((Date.now() - h * 3600e3) / 21600e3) * 21600e3).toISOString()
  const pinged = (lineage) => (h) => ({
    start: at(h), source: "aurora", lineage, country: "DE", uploaded: 0, downloaded: 0, probeSuccesses: 10, probeAttempts: 10, wanExcluded: 0, unknown: 0,
    pingMedianMs: 9, pingP95Ms: 13, newConnectionMedianMs: 27, forcedSwitchAways: 0, servedSeconds: 21600, incidents: 0, managementTrafficIncluded: false,
  })
  const weekOfPings = (from, to, step, lineages) => (lineages.includes("l-fra") ? [0, 6, 12, 24, 30, 48].map(pinged("l-fra")) : [])
  /** Frankfurt's week with two failovers in it and no reading, and Amsterdam's with failed checks alone. */
  const failoversOnly = (from, to, step, lineages) => [30, 80].map((h) => ({ ...pinged("l-fra")(h), pingMedianMs: null, pingP95Ms: null, probeSuccesses: 0, forcedSwitchAways: 1 }))
    .concat([{ ...pinged("l-ams")(30), pingMedianMs: null, pingP95Ms: null, probeSuccesses: 0 }]).filter((b) => lineages.includes(b.lineage))
  const SLOW_READ = { "api/analytics": 800 }
  const watchHeights = (page, selector) => page.evaluate((sel) => {
    window.heights = []
    const watch = window.watch = (window.watch ?? 0) + 1
    const frame = () => {
      if (window.watch !== watch) return
      const el = document.querySelector(sel)
      const h = el ? el.offsetHeight : null
      if (window.heights.length === 0 || window.heights.at(-1) !== h) window.heights.push(h)
      requestAnimationFrame(frame)
    }
    frame()
  }, selector)
  const heightsSeen = (page) => page.evaluate(() => window.heights)

  test("17. hovering a server with nothing in its week opens no card", async () => {
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9), server("l-ams", "Amsterdam", "NL", 15)], analytics: weekOfPings, readDelay: SLOW_READ })
    const { page, close } = await open("servers")
    try {
      const name = page.getByRole("button", { name: "Amsterdam", exact: true })
      await name.waitFor()
      await watchHeights(page, "[data-radix-popper-content-wrapper]")
      await name.hover()
      await until(() => daemon.log().some((r) => r.method === "GET" && r.path === BASE + "api/analytics"), "the week asked for")
      await page.waitForTimeout(SLOW_READ["api/analytics"] + 600)
      assert.deepEqual(await heightsSeen(page), [null], "no card, not even while the week is read")
    } finally {
      await close()
    }
  })

  test("17. hovering a server with a week of pings opens its card once, at one size", async () => {
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: weekOfPings, readDelay: SLOW_READ })
    const { page, close } = await open("servers")
    try {
      const name = page.getByRole("button", { name: "Frankfurt", exact: true })
      await name.waitFor()
      await watchHeights(page, "[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
      await name.hover()
      const card = page.locator("[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
      await card.getByText("Failovers").waitFor()
      await page.waitForTimeout(500)
      assert.equal(await card.locator("svg.recharts-surface").count(), 1, "the card holds the chart")
      const heights = await heightsSeen(page)
      assert.equal(heights.length, 2, `closed, then one height: ${heights}`)
      assert.equal(heights[0], null)
    } finally {
      await close()
    }
  })

  test("17. a server name's card reads 7d and the week's failovers over its chart at rest, names the seven local days ending today, today's bold, and on a place reads its six hours, its failovers and its pair in whole ms, in the line that was there", async () => {
    const fractional = (from, to, step, lineages) => weekOfPings(from, to, step, lineages).map((b) => ({ ...b, pingMedianMs: 67.39099999999999, pingP95Ms: 69.601 }))
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: fractional })
    const { page, close } = await open("servers")
    try {
      await page.getByRole("button", { name: "Frankfurt", exact: true }).hover()
      const card = page.locator("[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
      await card.locator("svg.recharts-surface").waitFor()
      assert.equal(await readoutOf(card), "7d Failovers 0 Ping p50 p95", "at rest: the window, the week's failovers and the ping keys, the pair's place empty")
      const window = card.locator("[data-slot=chart-window]")
      assert.equal(await toneOf(window), "muted", "the window is muted, as a source's week has it")
      assert.ok((await window.boundingBox()).y + 4 < (await card.locator("[data-slot=chart-legend]").boundingBox()).y, "it heads the keys")
      assert.ok((await card.locator("[data-slot=chart-legend]").boundingBox()).y < (await card.locator("[data-slot=chart]").boundingBox()).y, "the keys stand over the plot")
      const rest = await readoutBoxes(card)
      const chart = card.locator("[data-slot=chart]")
      const box = await chart.boundingBox()
      for (const x of [0.9, 0.92]) await page.mouse.move(box.x + box.width * x, box.y + box.height * 0.5)
      await until(async () => (await window.innerText()) !== "7d", "the readout reads the place")
      const words = await readoutOf(card)
      assert.match(words, /^\w{3} \d\d:\d\d–\d\d:\d\d Failovers 0 Ping p50 p95 67 \/ 70 ms$/, `its six hours, and its pair in whole ms: ${words}`)
      assert.equal(await card.locator("[data-slot=chart-band]").count(), 1, "the band marks the place")
      assert.deepEqual((await readoutBoxes(card)).lefts, rest.lefts, "no key moves")
      assert.equal(await page.locator(".recharts-tooltip-wrapper, [data-slot=week-tip]").count(), 0, "no tooltip stands anywhere")
      await page.mouse.move(box.x + 2, box.y - 4)
      await until(async () => (await window.innerText()) === "7d", "a mouse leaving the plot returns to the week at once")
      const letters = await card.locator("[data-slot=day-letter]").evaluateAll((all) => all.map((t) => (t.classList.contains("font-semibold") ? `*${t.textContent}` : t.textContent)))
      const days = [6, 5, 4, 3, 2, 1, 0].map((n) => { const d = new Date(); d.setDate(d.getDate() - n); return `${n === 0 ? "*" : ""}${d.toLocaleDateString("en-US", { weekday: "short" }).slice(0, 2)}` })
      assert.deepEqual(letters, days, "the seven local days ending today, today's bold")
    } finally {
      await close()
    }
  })

  /** A week at the drawing's evening: four digits in the last six hours with two failovers, p50 alone in the six before, none in the
   * six before those, then 70 / 104 ms, and 60 / 80 ms from there back. */
  const EVENING_PLACE = Math.floor(EVENING / 21600e3) * 21600e3
  const slowWeek = (from, to, step, lineages) => (lineages.includes("l-fra") ? Array.from({ length: 28 }, (_, i) => ({
    ...pinged("l-fra")(0), start: new Date(EVENING_PLACE - i * 21600e3).toISOString(), forcedSwitchAways: i === 0 ? 2 : 0,
    ...[{ pingMedianMs: 1204, pingP95Ms: 3980 }, { pingMedianMs: 1204, pingP95Ms: null }, { pingMedianMs: null, pingP95Ms: null, probeSuccesses: 0 }, { pingMedianMs: 70, pingP95Ms: 104 }][i] ?? { pingMedianMs: 60, pingP95Ms: 80 },
  })) : [])
  /** Rests the pointer on the place `back` six hours before the last of the chart in `scope`, and waits for the readout to read it. */
  const placeAt = async (page, scope, back) => {
    const box = await scope.locator("[data-slot=chart]").boundingBox()
    const x = box.x + 56 + ((27 - back + 0.5) * (box.width - 56 - 4)) / 28
    const before = await readoutOf(scope)
    for (const dx of [0, 1]) await page.mouse.move(x + dx, box.y + 40)
    await until(async () => (await readoutOf(scope)) !== before, "the readout reads the place")
    return readoutOf(scope)
  }

  test("17. a server's readout holds one line through rest, a place, one with no reading, 1204 / 3980 ms and 1204 / — ms, in the name's card at 1280 and 390 px and the details at 390 px: no key moves, each value 4 px after its name and the pair 8 px after p95, each line's mark 4 px before its name and 8 px after the name before it, nothing leaves the card, the chart's top stays, and a missing figure is the muted dash", async () => {
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: slowWeek })
    await inMoscow(async () => {
      for (const [width, where] of [[1280, "card"], [390, "card"], [390, "details"]]) {
        const { page, close } = await openAtEvening("servers", width)
        try {
          const name = page.getByRole("button", { name: "Frankfurt", exact: true })
          let scope
          if (where === "card") {
            await name.hover()
            scope = page.locator("[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
          } else {
            await name.click()
            scope = page.getByRole("dialog")
          }
          await scope.locator("svg.recharts-surface").waitFor()
          await scope.evaluate((el) => Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished)))
          const label = `${where} at ${width} px`
          assert.equal(await readoutOf(scope), "7d Failovers 2 Ping p50 p95", `${label}: at rest`)
          const rest = await readoutBoxes(scope)
          assert.deepEqual({ gaps: rest.gaps, marks: rest.marks, lines: rest.lines, bare: rest.bare }, { gaps: [4, 8], marks: [{ own: 4, before: 8 }, { own: 4, before: 8 }], lines: 1, bare: 0 }, `${label}: at rest`)
          assert.ok(rest.past <= 0, `${label}: the line inside the card at rest, ${rest.past} px past`)
          for (const [back, want, dashes] of [[3, "Failovers 0 Ping p50 p95 70 / 104 ms", []], [2, "Failovers 0 Ping p50 p95", []], [0, "Failovers 2 Ping p50 p95 1204 / 3980 ms", []], [1, "Failovers 0 Ping p50 p95 1204 / — ms", ["muted"]]]) {
            const words = await placeAt(page, scope, back)
            assert.match(words, new RegExp(`^\\w{3} \\d\\d:\\d\\d–\\d\\d:\\d\\d ${want.replace(/[/]/g, "\\/")}$`), `${label}: ${words}`)
            const now = await readoutBoxes(scope)
            assert.deepEqual({ gaps: now.gaps, marks: now.marks, lines: now.lines, lefts: now.lefts, top: now.top, dashes: now.dashes, bare: now.bare },
              { gaps: [4, 8], marks: [{ own: 4, before: 8 }, { own: 4, before: 8 }], lines: 1, lefts: rest.lefts, top: rest.top, dashes, bare: 0 }, `${label}, ${want}`)
            assert.ok(now.past <= 0, `${label}, ${want}: nothing leaves the card, ${now.past} px past`)
          }
        } finally {
          await close()
        }
      }
    })
  })

  test("17. on a phone a tap on a server's chart reads its place and keeps it after the finger lifts, and a tap off the plot returns to 7d", async () => {
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: weekOfPings })
    const pinged = /^\w{3} \d\d:\d\d–\d\d:\d\d Failovers 0 Ping p50 p95 9 \/ 13 ms$/
    for (const where of ["details", "name's card"]) {
      const { page, context, close } = await open("servers", { width: 390, touch: true })
      try {
        const name = page.getByRole("button", { name: "Frankfurt", exact: true })
        let scope
        if (where === "details") {
          await name.tap()
          scope = page.getByRole("dialog")
        } else {
          const at = await name.boundingBox()
          const cdp = await context.newCDPSession(page)
          await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: at.x + 10, y: at.y + at.height / 2 }] })
          await page.waitForTimeout(800)
          await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] })
          scope = page.locator("[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
        }
        await scope.locator("svg.recharts-surface").waitFor()
        await scope.evaluate((el) => Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished)))
        const window = scope.locator("[data-slot=chart-window]")
        const box = await scope.locator("[data-slot=chart]").boundingBox()
        const placeX = (back) => box.x + 56 + ((27 - back + 0.5) * (box.width - 60)) / 28
        const y = box.y + 40
        await page.touchscreen.tap(placeX(0), y)
        await until(async () => (await window.innerText()) !== "7d", `${where}: a tap reads its place`)
        const read = await readoutOf(scope)
        assert.match(read, pinged, where)
        await page.waitForTimeout(600)
        assert.equal(await readoutOf(scope), read, `${where}: the place stays after the finger lifts`)
        assert.equal(await scope.locator("[data-slot=chart-band]").count(), 1, `${where}: and its band`)
        await page.touchscreen.tap(placeX(0), y)
        await page.waitForTimeout(600)
        assert.equal(await readoutOf(scope), read, `${where}: a second tap on the same place reads it still`)
        await page.touchscreen.tap(placeX(2), y)
        await until(async () => ![read, "7d"].includes(await window.innerText()) && pinged.test(await readoutOf(scope)), `${where}: a tap on another place reads that place`)
        await page.waitForTimeout(600)
        assert.match(await readoutOf(scope), pinged, `${where}: and keeps it`)
        assert.notEqual(await readoutOf(scope), read, where)
        await page.touchscreen.tap(placeX(0), box.y + box.height - 6)
        await until(async () => (await window.innerText()) === "7d", `${where}: a tap on the axis returns to the week`)
        await page.touchscreen.tap(placeX(0), y)
        await until(async () => (await window.innerText()) !== "7d", `${where}: after the axis, a tap on the plot reads its place again`)
        await page.waitForTimeout(600)
        assert.equal(await readoutOf(scope), read, `${where}: and keeps it`)
        await (where === "details" ? scope.getByText("Country", { exact: true }) : window).tap()
        await until(async () => (await window.innerText()) === "7d", `${where}: a tap off the chart returns to the week`)
        assert.equal(await scope.locator("[data-slot=chart-band]").count(), 0, where)
        await page.touchscreen.tap(placeX(0), y)
        await until(async () => (await readoutOf(scope)) === read, `${where}: and the next tap on the plot reads its place`)
        assert.equal(await scope.count(), 1, `${where}: open through every tap`)
      } finally {
        await close()
      }
    }
  })

  test("17. the arrow keys move the place on a server's focused chart in its details, Home and End take the ends, and leaving it returns to 7d", async () => {
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: slowWeek })
    await inMoscow(async () => {
      const { page, close } = await openAtEvening("servers", 1280)
      try {
        await page.getByRole("button", { name: "Frankfurt", exact: true }).click()
        const dialog = page.getByRole("dialog")
        await dialog.locator("svg.recharts-surface").waitFor()
        const chart = dialog.getByRole("group", { name: "The week by six hours" })
        await chart.focus()
        const window = dialog.locator("[data-slot=chart-window]")
        await page.keyboard.press("ArrowLeft")
        await until(async () => /1204 \/ 3980 ms$/.test(await readoutOf(dialog)), "from the week, ArrowLeft takes the last place")
        await page.keyboard.press("ArrowLeft")
        await until(async () => /1204 \/ — ms$/.test(await readoutOf(dialog)), "and the one before it")
        await page.keyboard.press("ArrowRight")
        await until(async () => /1204 \/ 3980 ms$/.test(await readoutOf(dialog)), "ArrowRight moves back")
        await page.keyboard.press("Home")
        const first = await window.innerText()
        await page.keyboard.press("ArrowLeft")
        await page.waitForTimeout(100)
        assert.equal(await window.innerText(), first, "the first place is the end")
        await page.keyboard.press("Tab")
        await until(async () => (await window.innerText()) === "7d", "leaving the chart returns to the week")
      } finally {
        await close()
      }
    })
  })

  test("17. a server's dialog opens at once, its week's frame spinning, and keeps its height while the week is read", async () => {
    for (const [label, analytics] of [["no probes", () => []], ["a week of pings", weekOfPings]]) {
      daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics, readDelay: SLOW_READ })
      const { page, close } = await open("servers")
      try {
        const name = page.getByRole("button", { name: "Frankfurt", exact: true })
        await name.waitFor()
        await watchHeights(page, "[data-slot=dialog-content]")
        await name.click()
        await page.getByRole("dialog").getByRole("status", { name: "Loading 7-day history" }).waitFor()
        assert.equal(await page.getByRole("dialog").getByText("Country", { exact: true }).isVisible(), true, `${label}: its facts stand while the week is read`)
        await (label === "no probes" ? page.getByRole("dialog").getByLabel("No reading in 7 days") : page.getByRole("dialog").getByText("Failovers")).waitFor()
        await page.waitForTimeout(500)
        const heights = await heightsSeen(page)
        assert.equal(heights.length, 2, `${label}: closed, then one height: ${heights}`)
        assert.equal(heights[0], null, label)
      } finally {
        await close()
      }
    }
  })

  /** Where and in what ink the details' chart draws its unit: the right end and the middle of its topmost axis label that ends in
   * ms, from the chart's own corner, and its fill. */
  const unitOf = async (page) => {
    const chart = page.getByRole("dialog").locator("[data-slot=chart]")
    const corner = await chart.boundingBox()
    const labels = await chart.locator(".recharts-yAxis-tick-labels text").evaluateAll((all) =>
      all.filter((t) => /ms$/.test(t.textContent ?? "")).map((t) => { const b = t.getBoundingClientRect(); return { right: b.right, middle: b.top + b.height / 2, fill: getComputedStyle(t).fill } }))
    const top = labels.sort((a, b) => a.middle - b.middle)[0]
    return top && { right: Math.round(top.right - corner.x), middle: Math.round(top.middle - corner.y), fill: top.fill }
  }

  test("17. a server's details with nothing in their week draw the chart's frame, its days, its ms and the muted dash", async () => {
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: () => [] })
    const { page, close } = await open("servers")
    try {
      await page.getByRole("button", { name: "Frankfurt", exact: true }).click()
      const frame = page.getByRole("dialog").locator("[data-slot=chart-frame]")
      await frame.getByLabel("No reading in 7 days").waitFor()
      const words = (await frame.innerText()).replace(/\s+/g, " ").trim()
      assert.match(words, /\bms\b/, `the axis's unit: ${words}`)
      assert.match(words, /—$/, "the muted dash")
      /* The week is 28 six-hour buckets aligned to UTC, which touch eight local days off UTC: the axis names the seven ending today. */
      assert.equal(await frame.locator("[data-slot=day-letter]").count(), 7, "and the week's seven days")
    } finally {
      await close()
    }
  })

  test("17. a server's week chart draws its ms in one place whether the week has readings, has none, or failed to read", async () => {
    const at = {}
    try {
    for (const [state, set] of [["readings", { analytics: weekOfPings, analyticsFails: false }], ["none", { analytics: () => [], analyticsFails: false }], ["failed", { analyticsFails: true }]]) {
      daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], ...set })
      const { page, close } = await open("servers")
      try {
        await page.getByRole("button", { name: "Frankfurt", exact: true }).click()
        const dialog = page.getByRole("dialog")
        await (state === "readings" ? dialog.locator(".recharts-line-curve").first() : state === "none" ? dialog.getByLabel("No reading in 7 days") : dialog.getByRole("button", { name: "Retry" })).waitFor({ state: "attached" })
        /* The dialog's opening zoom ends first. */
        await page.waitForTimeout(400)
        at[state] = await unitOf(page)
        assert.ok(at[state], `the ${state} chart draws its ms`)
      } finally {
        await close()
      }
    }
    } finally {
      daemon.set({ analyticsFails: false })
    }
    assert.deepEqual(at.none, at.readings, "a week with no reading draws its ms where the top tick's stands, in its ink")
    assert.deepEqual(at.failed, at.readings, "a week that failed to read draws its ms there too")
  })

  /** Past the two seconds a read stays fresh. */
  const STALE_MS = 2300

  test("17. a server's dialog opened again after its week went stale shows the new read and not the stale week, and keeps one height", async () => {
    for (const width of [360, 768, 1280]) {
      daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: () => [], readDelay: SLOW_READ })
      const { page, close } = await open("servers", { width })
      try {
        const name = page.getByRole("button", { name: "Frankfurt", exact: true })
        await name.click()
        await page.getByRole("dialog").getByLabel("No reading in 7 days").waitFor()
        await page.keyboard.press("Escape")
        await page.getByRole("dialog").waitFor({ state: "hidden" })
        await page.mouse.move(0, 0)
        await page.waitForTimeout(STALE_MS)
        daemon.set({ analytics: weekOfPings })
        await watchHeights(page, "[data-slot=dialog-content]")
        await name.click()
        await page.getByRole("dialog").getByText("Failovers").waitFor()
        await page.waitForTimeout(500)
        assert.equal(await page.getByRole("dialog").getByLabel("No reading in 7 days").count(), 0, `${width} px: the new week's pings, not the stale empty week`)
        assert.ok(await page.getByRole("dialog").locator(".recharts-line").count() > 0, `${width} px: the new week's ping lines`)
        const heights = await heightsSeen(page)
        assert.equal(heights.length, 2, `${width} px: closed, then one height: ${heights}`)
        assert.equal(heights[0], null)
      } finally {
        await close()
      }
    }
  })

  test("17. a hover card opened after its week went stale, on returning to the tab, waits for the new read and opens once at its size", async () => {
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: failoversOnly, readDelay: SLOW_READ })
    const { page, close } = await open("servers")
    try {
      const name = page.getByRole("button", { name: "Frankfurt", exact: true })
      const card = page.locator("[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
      await name.hover()
      await card.getByText("Failovers").waitFor()
      await nav(page).locator('a[href$="/sources/aurora"]').click()
      await page.getByPlaceholder("Search").waitFor()
      await page.waitForTimeout(STALE_MS)
      daemon.set({ analytics: weekOfPings })
      await page.goBack()
      await name.waitFor()
      await watchHeights(page, "[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
      await name.hover()
      await card.locator("svg.recharts-surface").waitFor()
      await page.waitForTimeout(500)
      const heights = await heightsSeen(page)
      assert.equal(heights.length, 2, `closed, then one height: ${heights}`)
      assert.equal(heights[0], null)
    } finally {
      await close()
    }
  })

  test("17. hovering a name again after its week went stale reads the week again", async () => {
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: failoversOnly, readDelay: SLOW_READ })
    const { page, close } = await open("servers")
    try {
      const name = page.getByRole("button", { name: "Frankfurt", exact: true })
      const card = page.locator("[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
      await name.hover()
      await card.getByText("Failovers").waitFor()
      await page.mouse.move(0, 0)
      await card.waitFor({ state: "detached" })
      await page.waitForTimeout(STALE_MS)
      daemon.set({ analytics: weekOfPings })
      await watchHeights(page, "[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
      await name.focus()
      await card.locator("svg.recharts-surface").waitFor()
      await page.waitForTimeout(500)
      const heights = await heightsSeen(page)
      assert.equal(heights.length, 2, `closed, then one height: ${heights}`)
      assert.equal(heights[0], null)
    } finally {
      await close()
    }
  })

  test("17. search reads only the words a row shows", async () => {
    const { page, close } = await open("servers")
    try {
      await rowOf(page, "Paris").waitFor()
      await page.getByRole("button", { name: "Search", exact: true }).click()
      await page.getByPlaceholder("Search").fill("false")
      await page.getByText("No match").waitFor()
      await page.getByPlaceholder("Search").fill("france")
      await until(async () => (await page.locator("tbody tr").count()) === 1, "only Paris")
    } finally {
      await close()
    }
  })

  test("17. the dot's header filters All servers by status, hiding rows, and a source's tab has no Status filter", async () => {
    daemon.set({ catalogue: auroraWithDown("l-fra") })
    const { page, close } = await open("servers")
    try {
      await rowOf(page, "Paris").waitFor()
      const dialog = await openFilter(page, "Status")
      assert.deepEqual((await dialog.getByRole("option").allInnerTexts()).map((x) => x.replace(/\s+/g, " ").trim()), ["Online 2", "Offline 1", "Unchecked 0"])
      await dialog.getByRole("option", { name: /Offline/ }).click()
      await page.keyboard.press("Escape")
      await until(async () => (await page.locator("tbody tr").count()) === 1, "only the failing server")
      await page.getByRole("group", { name: "Status filter" }).waitFor()
      await page.goto(daemon.url + "sources/aurora")
      await rowOf(page, "Paris").waitFor()
      assert.equal(await page.getByRole("button", { name: "Filter servers by Status" }).count(), 0, "a source's tab has no Status filter")
    } finally {
      await close()
    }
  })

  test("17. a dormant server reads Unchecked, greyed, with its age, and its ping is muted, its card saying when it was measured", async () => {
    const health = { ...server("l-ams", "Amsterdam", "NL", 15).health, current: false, checkedAt: H(12 * 24), pingAt: H(12 * 24) }
    daemon.set({ catalogue: [server("l-ams", "Amsterdam", "NL", 15, { health }), server("l-fra", "Frankfurt", "DE", 9)] })
    const { page, close } = await open("servers")
    try {
      const row = rowOf(page, "Amsterdam")
      const mark = row.getByRole("img", { name: "Unchecked" })
      await mark.waitFor()
      assert.equal(await mark.getAttribute("title"), "Unchecked · Last checked 12d ago")
      assert.match(await mark.getAttribute("class"), /grayscale/)
      const old = row.getByRole("button", { name: "15 / 21 ms, measured 12d ago", exact: true })
      assert.match(await old.getAttribute("class"), /text-muted-foreground/)
      await old.focus()
      await until(async () => (await card(page).innerText().catch(() => "")) === "12d ago", "its card says how old")
      assert.doesNotMatch(await shownWords(row), /ago/, "the age is in the card alone")
      const fresh = rowOf(page, "Frankfurt").locator("td span[title='9 / 13 ms']")
      assert.equal(await fresh.count(), 1, "a current ping has no age")
      assert.doesNotMatch(await fresh.getAttribute("class") ?? "", /text-muted-foreground/)
    } finally {
      await close()
    }
  })

  test("17. a muted ping is one line, as tall as a current one's, at 1280 and 390 px", async () => {
    const health = { ...server("l-ams", "Amsterdam", "NL", 15).health, pingAt: H(12 * 24) }
    daemon.set({ catalogue: [server("l-ams", "Amsterdam", "NL", 15, { health }), server("l-fra", "Frankfurt", "DE", 9)] })
    /** The lines the words an element shows take, and their height from the first line's top to the last's bottom, wherever its box is. */
    const linesOf = (locator) => locator.evaluate((el) => {
      const rects = []
      for (const walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT); walk.nextNode();) {
        const range = document.createRange()
        range.selectNodeContents(walk.currentNode)
        rects.push(...[...range.getClientRects()].filter((r) => r.width > 0))
      }
      const tops = new Set(rects.map((r) => Math.round(r.top)))
      return { lines: tops.size, height: Math.round(Math.max(...rects.map((r) => r.bottom)) - Math.min(...rects.map((r) => r.top))) }
    })
    const heights = {}
    for (const width of [1280, 390]) {
      const { page, close } = await open("servers", { width })
      try {
        const old = rowOf(page, "Amsterdam").getByRole("button", { name: "15 / 21 ms, measured 12d ago", exact: true })
        await old.waitFor()
        const fresh = rowOf(page, "Frankfurt").locator("td span[title='9 / 13 ms']")
        heights[width] = { muted: await linesOf(old), current: await linesOf(fresh) }
      } finally {
        await close()
      }
    }
    assert.deepEqual(heights, Object.fromEntries(Object.entries(heights).map(([w, h]) => [w, { muted: { ...h.current, lines: 1 }, current: { ...h.current, lines: 1 } }])),
      "the muted ping and a current one are each one line, of one height")
  })

  test("17. a group's tab lists its members in the order the daemon sends, then the rows its rule leaves out in feed order", async () => {
    daemon.addAcme()
    const among = (values) => ({ not: false, values })
    const members = { sources: among(["acme"]), countries: among(["DE", "NL", "SE"]), protocols: among([]), include: "", exclude: "", box: null }
    const sent = groupView("lw", 1, { name: "Acme Europe", members }, [], daemon.catalogue())
    /* The daemon puts a member the group left in the last 5 minutes last, so its order is neither best first nor feed order. */
    const order = [...sent.order.slice(1), sent.order[0]]
    daemon.set({ groups: new Map([["lw", { ...sent, order, firstChoice: order[0] }]]) })
    const { servers, source: lw } = acme()
    const place = new Map(lw.feed.configs.map((c) => [c.key, c.place]))
    const first = (s) => s.seen.map((x) => [place.get(x.configKey), x.position]).toSorted((a, b) => a[0] - b[0] || a[1] - b[1])[0]
    const left = servers.filter((s) => !order.includes(s.id)).toSorted((a, b) => first(a)[0] - first(b)[0] || first(a)[1] - first(b)[1])
    const label = (id) => daemon.catalogue().find((s) => s.id === id).label
    const { page, close } = await open("groups/lw")
    try {
      await rowOf(page, "USA · proxy").waitFor()
      assert.equal(order.length, 6, `the rule holds six servers: ${order}`)
      const want = [...order.map((id) => `+:${label(id)}`), ...["Amsterdam", "Frankfurt", "Paris"].map((n) => `-:${n}`), ...left.map((s) => `-:${s.label}`)]
      assert.deepEqual((await tableOf(page)).rows.map((r) => r.replace(/\n.*/s, "")), want)
    } finally {
      await close()
    }
  })

  test("17. at 360 px the row a group is on is marked and tinted, though it isn't the first row and In use is hidden", async () => {
    const { page, close } = await open("groups/all-aurora", { width: 360 })
    try {
      await rowOf(page, "Paris").waitFor()
      assert.deepEqual((await page.locator("tbody tr td:nth-child(4)").allInnerTexts()).map((x) => x.trim()), ["Frankfurt", "Amsterdam", "Paris"], "the daemon's order puts the server in use second")
      assert.equal(await page.locator("thead th", { hasText: "In use" }).isVisible(), false, "In use is hidden")
      const marks = page.locator("tbody").getByRole("img", { name: /^In use: / })
      assert.equal(await marks.count(), 1)
      assert.equal(await marks.getAttribute("aria-label"), "In use: EU")
      assert.ok(await rowOf(page, "Amsterdam").getByRole("img", { name: "In use: EU" }).isVisible(), "the mark is on the row in use, in sight")
      const looks = await page.locator("tbody tr").evaluateAll((trs) => trs.map((tr) => getComputedStyle(tr).backgroundColor))
      assert.ok(looks[1] !== looks[0] && looks[0] === looks[2], `only the row in use is tinted: ${looks}`)
      const [mark, name] = await rowOf(page, "Amsterdam").evaluate((tr) => [tr.querySelector("[role=img][aria-label^='In use']"), tr.querySelector("td:nth-child(4) button")].map((el) => el.getBoundingClientRect().toJSON()))
      assert.ok(mark.width > 0 && mark.right <= name.left, "the mark sits before the name")
    } finally {
      await close()
    }
  })

  test("17. a group's header line is its dot, its name and its troubles in their words and colours, as a source's, with no count or ping, and a quiet week nothing", async () => {
    const group = daemon.group("all-aurora")
    daemon.set({ groups: new Map([["all-aurora", { ...group, moves7d: 2 }]]) })
    const { page, close } = await open("groups/all-aurora")
    try {
      const heading = page.locator("[data-slot=group-heading]")
      await heading.getByRole("img").first().waitFor()
      await page.locator("tbody tr").first().waitFor()
      assert.doesNotMatch(await heading.innerText(), /up|ms|\d/, "the table shows each server's dot and ping")
      assert.equal(await page.getByText(/failover/).count(), 0, "under 7 failovers is quiet")
      daemon.set({ groups: new Map([["all-aurora", { ...group, moves7d: 8 }]]) })
      await page.reload()
      const trouble = page.getByText("8 failovers in 7d", { exact: true }).first()
      await trouble.waitFor()
      assert.equal(await toneOf(trouble), "warn")
    } finally {
      await close()
    }
  })

  test("17. a group's header dot is its first choice's, and its count agrees with its rows", async () => {
    daemon.set({ catalogue: auroraWithDown("l-fra") })
    const { page, close } = await open("groups/all-aurora")
    try {
      const rows = page.locator("tbody tr")
      await until(async () => (await rows.count()) === 3, "the group's three rows")
      await rowOf(page, "Frankfurt").getByRole("img", { name: "Offline", exact: true }).waitFor()
      for (const name of ["Amsterdam", "Paris"]) await rowOf(page, name).getByRole("img", { name: "Online", exact: true }).waitFor()
      const header = page.locator("[data-slot=group-heading]").getByRole("img").first()
      await until(async () => (await header.getAttribute("title")) === "Offline · First choice: Not responding", "the dot is its first choice's")
    } finally {
      await close()
    }
  })

  test("17. a group reads Unchecked while nothing probed its first choice lately", async () => {
    const fra = server("l-fra", "Frankfurt", "DE", 9)
    daemon.set({ catalogue: [server("l-ams", "Amsterdam", "NL", 15), { ...fra, health: { ...fra.health, current: false, checkedAt: H(3) } }, server("l-par", "Paris", "FR", 19)] })
    const { page, close } = await open("groups/all-aurora")
    try {
      await rowOf(page, "Frankfurt").getByRole("img", { name: "Unchecked", exact: true }).waitFor()
      assert.equal(await page.locator("[data-slot=group-heading]").getByRole("img").first().getAttribute("title"), "Unchecked · First choice: Last checked 3h ago")
    } finally {
      await close()
    }
  })

  test("17. a server's details show no score, their facts as rows each value 12 px after the names, and the readout the name's card has over the chart, its days told apart", async () => {
    daemon.set({ catalogue: [server("l-fra", "Frankfurt", "DE", 9)], analytics: weekOfPings })
    const { page, close } = await open("servers")
    try {
      await page.getByRole("button", { name: "Frankfurt" }).click()
      const dialog = page.getByRole("dialog")
      await dialog.locator("svg.recharts-surface").waitFor()
      const words = await dialog.innerText()
      assert.doesNotMatch(words, /Score|× 1\.04|Failures/, "no score: Ping and Stability are its parts; and no Failures count")
      /* The dialog's opening zoom ends first, so every box is measured at one scale. */
      await dialog.evaluate((el) => Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished)))
      const rows = await rowsOf(dialog)
      assert.deepEqual(rows.map((r) => r.words.split(" ")[0]), ["Country", "Health", "Ping", "Connect", "UDP", "Stability"])
      const names = Math.max(...rows.map((r) => r.name))
      assert.deepEqual(rows.map((r) => Math.round(r.value - names)), rows.map(() => 12), "each value 12 px after the names' column, the bars too")
      const readout = await readoutOf(dialog)
      assert.equal(readout, "7d Failovers 0 Ping p50 p95", "the readout, its window once")
      assert.equal(await toneOf(dialog.locator("[data-slot=chart-window]")), "muted")
      assert.doesNotMatch(words, /Incidents|Up 7d/)
      const days = await dialog.locator(".recharts-xAxis text").evaluateAll((ticks) => ticks.map((t) => t.textContent))
      const named = days.slice(1)
      assert.equal(new Set(named).size, named.length, `the axis tells its days apart: ${days.join(" ")}`)
      assert.ok(days.every((d) => /^[A-Z][a-z]$/.test(d)), `each by its two letters: ${days.join(" ")}`)
      await page.keyboard.press("Escape")
      await page.mouse.move(0, 0)
      await page.getByRole("button", { name: "Frankfurt" }).hover()
      const card = page.locator("[data-radix-popper-content-wrapper]:has([data-slot=chart-readout])")
      await card.locator("svg.recharts-surface").waitFor()
      assert.equal(await readoutOf(card), readout, "the card and the details read one line")
    } finally {
      await close()
    }
  })

  test("17. a source's week reads its days in use from served time and its failovers from the analytics, and says when nothing used it", async () => {
    const at = (h) => new Date(Math.floor((Date.now() - h * 3600e3) / 21600e3) * 21600e3).toISOString()
    const bucket = (start, servedSeconds, forcedSwitchAways, probeSuccesses) => ({
      start, source: "aurora", lineage: "l-fra", country: "DE", uploaded: 0, downloaded: 0, probeSuccesses, probeAttempts: servedSeconds / 60, wanExcluded: 0, unknown: 0,
      pingMedianMs: 9, pingP95Ms: 13, newConnectionMedianMs: 27, forcedSwitchAways, servedSeconds, incidents: forcedSwitchAways, managementTrafficIncluded: false,
    })
    daemon.set({ analytics: () => [bucket(at(30), 21600, 2, 359), bucket(at(6), 10800, 0, 179)] })
    const { page, close } = await open("sources/aurora")
    try {
      const week = weekButton(page)
      assert.match(await week.getAttribute("aria-label"), /^2 failovers in the last 7 days, in use on [12] of them$/)
      await week.hover()
      assert.match(await weekPlace(page, { failover: 0 }), /^\w{3} \d\d:\d\d–\d\d:\d\d Failovers 2 In use 15m$/, "its six hours with failovers, and the quarter hour in use")
      assert.doesNotMatch(await card(page).innerText(), /%|Probe| h in use/)
      daemon.set({ analytics: () => [] })
      await page.reload()
      await until(async () => (await weekButton(page).getAttribute("aria-label").catch(() => "")) === "No failovers in the last 7 days, not in use", "nothing used it")
    } finally {
      await close()
    }
  })
})

describe("the Stability column", () => {
  const LAST = /^Last failure (today|Mon|Tue|Wed|Thu|Fri|Sat|Sun|[A-Z][a-z]{2} \d{1,2},) \d\d:\d\d$/
  /** Each state's lit bars and what a screen reader hears of its card, in the card's order. */
  const CELLS = {
    "Germany · proxy": [4, ["No failure in 7d", "Seen working 7d"]],
    "Netherlands · proxy": [3, ["1 failure in 7d", "Seen working 7d", LAST]],
    "Netherlands · proxy-2": [2, ["2 failures in 7d", "Seen working 7d", LAST]],
    "Finland · proxy": [1, ["4 failures in 7d", "Seen working 7d", LAST]],
    "Finland · proxy-2": [1, ["9 failures in 7d", "Seen working 7d", LAST]],
    "Sweden · proxy": [0, ["23 failures in 7d", "Seen working 7d", LAST]],
    "Poland · proxy": [2, ["No failure in 7d", "Seen working 7h"]],
    "Lithuania · proxy": [1, ["1 failure in 7d", "Seen working 12h", LAST]],
    "Latvia · proxy": [1, ["3 failures in 7d", "Seen working 3d", LAST]],
    "Iceland · proxy": [1, ["1 failure in 7d", "Seen working 2d", LAST]],
    "Norway · proxy": [0, ["No failure in 7d", "Not seen working in 7d", LAST]],
    "Estonia · proxy": [2, ["Not checked yet"]],
    "Ireland · proxy": [2, ["Not checked in 7d", LAST]],
    "Denmark · proxy": [0, ["1,200 failures in 7d", "Seen working 7d", LAST]],
    "Czechia · proxy": [2, ["No failure in 7d", "Seen working 20h"]],
    "Austria · proxy": [1, ["2 failures in 7d", "Time seen working not known", LAST]],
  }
  const UNITS = { m: "minute", h: "hour", d: "day" }
  const spoken = (line) => (line instanceof RegExp ? line.source.slice(1, -1)
    : line.replace(/ in 7d$/, " in the last 7 days").replace(/(\d+)(m|h|d)$/, (_, n, u) => `${n} ${UNITS[u]}${n === "1" ? "" : "s"}`).replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
  /** The sentence a screen reader hears for a state's cell. */
  const heard = (label) => new RegExp(`^Stability ${CELLS[label][0]} of 4\\. ${CELLS[label][1].map(spoken).join("\\. ")}\\.$`)
  /** What a state's card shows for each sentence heard: a name and its value. */
  const shownOf = (line) => line instanceof RegExp ? [line]
    : line === "Not checked yet" || line === "Not checked in 7d" ? ["Failures —", "Seen working —"]
    : line === "No failure in 7d" ? ["Failures 0"]
    : /^[\d,]+ failures? in 7d$/.test(line) ? [`Failures ${line.split(" ")[0]}`]
    : line === "Not seen working in 7d" ? ["Seen working 0m"]
    : line === "Time seen working not known" ? ["Seen working —"]
    : [line]
  const shown = (label) => CELLS[label][1].flatMap(shownOf)
  /** A card's lines, each name joined to its value, as the card's grid shows them side by side. */
  const paired = (lines) => lines.reduce((out, l, i) => (i % 2 === 0 ? [...out, l] : [...out.slice(0, -1), `${out.at(-1)} ${l}`]), [])
  /** The first line of a state's card. */
  const first = (label) => shown(label)[0]
  const seed = () => daemon.set({ catalogue: failureStates().map((x) => x.server) })
  /** Where an element's words or marks end, wherever its box does. */
  const right = (locator) => locator.evaluate((el) => { const range = document.createRange(); range.selectNodeContents(el); return range.getBoundingClientRect().right })
  const menuOption = (page) => (label) => page.getByRole("dialog").getByRole("option").filter({ hasText: `${label} · Aurora` })
  /** The lit bars of a row's or an option's cell, and their right edge. */
  const bars = (within) => within.locator("[data-slot=bars]").evaluate((el) => ({ lit: Number(el.dataset.lit), right: el.getBoundingClientRect().right }))
  /** Opens the Exit menu and waits for it to finish zooming in, so what it measures is where it rests. */
  const openMenu = async (page) => {
    await page.goto(daemon.url)
    await page.getByRole("combobox", { name: "Exit of EU" }).click()
    await menuOption(page)("Estonia · proxy").waitFor()
    await until(() => page.getByRole("dialog").evaluate((el) => el.getAnimations({ subtree: true }).length === 0), "the menu to settle")
    return page.getByRole("dialog")
  }
  /** The tone of every empty cell's dash in a row or an option. */
  const dashTones = (within) => within.getByText("—", { exact: true }).evaluateAll((els) => els.map((el) => {
    const probe = document.createElement("span")
    probe.style.color = "var(--muted-foreground)"
    document.body.append(probe)
    const muted = getComputedStyle(probe).color
    probe.remove()
    return getComputedStyle(el).color === muted ? "muted" : "plain"
  }))

  for (const width of [360, 768, 1280]) {
    test(`17. at ${width} px in both themes each state reads as written in the table and the Exit menu, Ping and the bars against the right edge`, async () => {
      for (const theme of ["dark", "light"]) {
        seed()
        const { page, close } = await open("servers", { width, theme })
        try {
          await rowOf(page, "Estonia · proxy").waitFor()
          const head = (name) => page.locator("thead th").filter({ hasText: name })
          const [stabilityRight, pingRight] = [await right(head("Stability").getByRole("button")), await right(head("Ping"))]
          for (const [label, [lit]] of Object.entries(CELLS)) {
            const cell = await bars(rowOf(page, label))
            assert.equal(cell.lit, lit, `${theme} table: ${label}'s bars`)
            assert.ok(Math.abs(cell.right - stabilityRight) <= 1, `${theme} table: ${label}'s bars end at the header's right edge, ${cell.right} and ${stabilityRight}`)
            assert.equal(await rowOf(page, label).getByRole("button", { name: heard(label) }).count(), 1, `${theme}: a screen reader hears ${label}'s cell as its level and its card`)
          }
          assert.ok(Math.abs((await right(rowOf(page, "Germany · proxy").locator("td").nth(5))) - pingRight) <= 1, `${theme}: Ping ends at its header's right edge`)
          assert.deepEqual(await dashTones(rowOf(page, "Estonia · proxy")), ["muted", "muted"], `${theme}: Ping's and In use's empty dashes are muted`)

          const list = await openMenu(page)
          const heading = list.locator("[cmdk-group-heading]").filter({ hasText: "Servers" })
          assert.match(await heading.innerText(), /^Servers\s*Ping\s*Stability$/, "a header line over the cells")
          const menuRight = await right(heading.locator('button[aria-label="Stability"]'))
          const menuPingRight = await right(heading.getByText("Ping", { exact: true }))
          for (const [label, [lit]] of Object.entries(CELLS)) {
            const cell = await bars(menuOption(page)(label))
            assert.equal(cell.lit, lit, `${theme} menu: ${label}'s bars`)
            assert.ok(Math.abs(cell.right - menuRight) <= 1, `${theme} menu: ${label}'s bars end at the header's right edge`)
            const named = list.getByRole("option", { name: new RegExp(`${label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")} · Aurora.*${heard(label).source.slice(1)}`) })
            assert.equal(await named.count(), 1, `${theme}: in the menu a screen reader hears ${label}'s cell`)
          }
          assert.ok(Math.abs((await right(menuOption(page)("Germany · proxy").getByText("40 / 56 ms", { exact: true }))) - menuPingRight) <= 1, `${theme} menu: Ping ends at its header's right edge`)
          assert.deepEqual(await dashTones(menuOption(page)("Estonia · proxy")), ["muted"], `${theme}: in the menu Ping's empty dash is muted`)
          const group = await list.getByRole("option").filter({ hasText: "All Aurora" }).evaluate((el) => [el.textContent, el.querySelector("[data-slot=bars]")?.dataset.lit])
          assert.deepEqual([group[0].endsWith("Stability 2 of 4. Not checked yet."), group[1]], [true, "2"], `${theme}: a first choice the daemon has no record of stands where every server starts: ${group}`)
        } finally {
          await close()
        }
      }
    })
  }

  test("17. the bars are the text's colour, never a trouble's, with lit and unlit at least 3:1 apart in both themes, on a plain row and the highlighted option", async () => {
    for (const theme of ["dark", "light"]) {
      seed()
      const { page, close } = await open("servers", { width: 1280, theme })
      /** The contrast between a cell's lit and unlit bars, each blended over what is behind it, and whether the lit one is the text's colour. */
      const contrast = (within) => within.locator("[data-slot=bars]").evaluate((el) => {
        const canvas = document.createElement("canvas").getContext("2d", { willReadFrequently: true })
        const rgb = (colour) => { canvas.clearRect(0, 0, 1, 1); canvas.fillStyle = colour; canvas.fillRect(0, 0, 1, 1); return [...canvas.getImageData(0, 0, 1, 1).data.slice(0, 3)] }
        let behind = el
        while (behind && ["rgba(0, 0, 0, 0)", "transparent"].includes(getComputedStyle(behind).backgroundColor)) behind = behind.parentElement
        const back = rgb(getComputedStyle(behind ?? document.body).backgroundColor)
        const [lit, unlit] = [el.children[0], el.children[3]]
        const shown = (bar) => { const c = rgb(getComputedStyle(bar).backgroundColor); const a = Number(getComputedStyle(bar).opacity); return c.map((v, i) => v * a + back[i] * (1 - a)) }
        const lum = (c) => { const [r, g, b] = c.map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4 }); return 0.2126 * r + 0.7152 * g + 0.0722 * b }
        const [a, b] = [lum(shown(lit)), lum(shown(unlit))]
        return { ratio: (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05), text: getComputedStyle(lit).backgroundColor === getComputedStyle(el).color }
      })
      try {
        const row = rowOf(page, "Lithuania · proxy")
        await row.waitFor()
        const plain = await contrast(row)
        assert.ok(plain.ratio >= 3, `${theme} table: lit and unlit ${plain.ratio.toFixed(2)}:1`)
        assert.ok(plain.text, `${theme} table: a lit bar is the text's colour`)
        await openMenu(page)
        await page.getByPlaceholder("Search").fill("Lithuania")
        const option = menuOption(page)("Lithuania · proxy")
        await option.hover()
        await until(async () => (await option.getAttribute("data-selected")) === "true", "the option is highlighted")
        const selected = await contrast(option)
        assert.ok(selected.ratio >= 3, `${theme} highlighted option: lit and unlit ${selected.ratio.toFixed(2)}:1`)
        assert.ok(selected.text, `${theme} highlighted option: a lit bar is the text's colour`)
      } finally {
        await close()
      }
    }
  })

  test("17. a cell's card gives the week's failures, the time seen working and the last failure as names and values, by pointer and by keyboard, and every server has one", async () => {
    seed()
    const { page, close } = await open("servers", { width: 1280 })
    const cardLines = async () => paired((await card(page).innerText()).split("\n").map((l) => l.trim()).filter(Boolean))
    const matches = (lines, want) => lines.length === want.length && want.every((w, i) => (w instanceof RegExp ? w.test(lines[i]) : w === lines[i]))
    try {
      await rowOf(page, "Estonia · proxy").waitFor()
      for (const label of Object.keys(CELLS)) {
        const want = shown(label)
        const cell = rowOf(page, label).getByRole("button", { name: /^Stability / })
        if (label === "Lithuania · proxy") await cell.hover()
        else await cell.focus()
        await until(async () => matches(await cardLines().catch(() => []), want), `${label}'s card`)
        assert.ok((await cardLines()).every((l) => !l.endsWith(".")), `${label}: no full stop`)
        await cell.blur()
        await page.mouse.move(0, 0)
        await until(async () => !(await card(page).isVisible().catch(() => false)), `${label}'s card closes`)
      }
      const name = rowOf(page, "Lithuania · proxy").getByRole("button", { name: "Lithuania · proxy", exact: true })
      await name.focus()
      await page.keyboard.press("Tab")
      assert.match(await page.evaluate(() => document.activeElement?.getAttribute("aria-label")), heard("Lithuania · proxy"), "Tab moves from the name to the cell")
      await until(async () => (await cardLines().catch(() => []))[0] === first("Lithuania · proxy"), "keyboard focus opens the card")
    } finally {
      await close()
    }
  })

  test("17. a ping over a day old is muted, and its card says how old", async () => {
    seed()
    const health = (x) => x.server.health
    daemon.set({ catalogue: failureStates().map((x) => (x.server.id === "f-de" ? { ...x.server, health: { ...health(x), pingAt: new Date(Date.now() - 12 * 864e5).toISOString() } } : x.server)) })
    const { page, close } = await open("servers", { width: 1280 })
    try {
      const ping = rowOf(page, "Germany · proxy").getByRole("button", { name: /^40 \/ 56 ms, measured 12d ago$/ })
      await ping.focus()
      await until(async () => (await card(page).innerText().catch(() => "")) === "12d ago", "its card")
      assert.equal(await card(page).locator("svg.lucide-history").count(), 1, "the age beside the history sign")
      assert.equal(await toneOf(ping), "muted")
      await openMenu(page)
      await page.getByPlaceholder("Search").fill("Germany")
      await menuOption(page)("Germany · proxy").locator("[data-slot=bars]").hover()
      await until(async () => (await menuCardLines(page)).at(-1) === "12d ago", "in the Exit menu the old ping's age ends the option's card")
    } finally {
      await close()
    }
  })

  /** The Stability header in the table, and over the same column in the Exit menu's Servers section, whose heading cmdk hides from
   * a screen reader, so it is found by its label. */
  const headers = (page) => ({
    table: page.locator("thead").getByRole("button", { name: "Stability", exact: true }),
    menu: page.getByRole("dialog").locator("[cmdk-group-heading]").filter({ hasText: "Servers" }).locator('button[aria-label="Stability"]'),
  })
  const HEADER_CARD = /^Most trusted\nStarting level\nLeast trusted$/
  const headerCard = (page) => page.locator("[data-radix-popper-content-wrapper]").filter({ hasText: "Starting level" })

  test("17. the Stability header's card names the top, the start and the bottom by their bars, and nothing else, opened by the mouse and the keyboard, in the table and the Exit menu", async () => {
    seed()
    const { page, close } = await open("servers", { width: 1280 })
    try {
      const { table } = headers(page)
      await table.hover()
      await until(() => headerCard(page).isVisible().catch(() => false), "the mouse opens the table header's card")
      assert.match(await headerCard(page).innerText(), HEADER_CARD)
      assert.deepEqual(await headerCard(page).locator("[data-slot=bars]").evaluateAll((els) => els.map((el) => Number(el.dataset.lit))), [4, 2, 0], "the top, the start and the bottom with their bars")
      await page.mouse.move(0, 0)
      await until(async () => !(await headerCard(page).isVisible().catch(() => false)), "it closes")
      await table.focus()
      await until(() => headerCard(page).isVisible().catch(() => false), "keyboard focus opens it")
      await table.blur()

      await openMenu(page)
      const { menu } = headers(page)
      await menu.hover()
      await until(() => headerCard(page).isVisible().catch(() => false), "the mouse opens the menu header's card")
      await page.mouse.move(0, 0)
      await until(async () => !(await headerCard(page).isVisible().catch(() => false)), "it closes")
      await page.getByPlaceholder("Search").focus()
      for (let i = 0; i < 4 && (await page.evaluate(() => document.activeElement?.getAttribute("aria-label"))) !== "Stability"; i++) await page.keyboard.press("Tab")
      await until(() => headerCard(page).isVisible().catch(() => false), "Tab from the search reaches a header, and focus opens its card")
      assert.ok(await page.getByRole("listbox").isVisible(), "the menu stays open")
    } finally {
      await close()
    }
  })

  for (const width of [360, 1280]) {
    test(`17. at ${width} px a tap opens the Stability header's card in the table and the Exit menu, and a second tap closes it`, async () => {
      seed()
      const { page, close } = await open("servers", { width, touch: true })
      try {
        const { table } = headers(page)
        await table.tap()
        await until(() => headerCard(page).isVisible().catch(() => false), "a tap opens the table header's card")
        const fit = await headerCard(page).evaluate((el) => el.firstElementChild.getBoundingClientRect())
        assert.ok(fit.width <= Math.min(320, width - 32) + 0.5 && fit.left >= 0 && fit.right <= width, `the card fits: ${JSON.stringify(fit)}`)
        await table.tap()
        await until(async () => !(await headerCard(page).isVisible().catch(() => false)), "a second tap closes it")
        await openMenu(page)
        const { menu } = headers(page)
        await menu.tap()
        await until(() => headerCard(page).isVisible().catch(() => false), "a tap opens the menu header's card")
        assert.ok(await page.getByRole("listbox").isVisible(), "the menu stays open")
        assert.equal(requests("PUT", "api/slots/eu").length, 0, "the tap picks nothing")
        await menu.tap()
        await until(async () => !(await headerCard(page).isVisible().catch(() => false)), "a second tap closes it")
      } finally {
        await close()
      }
    })
  }

  test("17. on a phone a tap opens a cell's card and a second tap or a tap elsewhere closes it, and the cell's tap area is 28 px tall", async () => {
    seed()
    const { page, close } = await open("servers", { width: 360, touch: true })
    const opened = () => card(page).isVisible().catch(() => false)
    try {
      const cell = rowOf(page, "Lithuania · proxy").getByRole("button", { name: /^Stability / })
      await cell.waitFor()
      assert.ok((await cell.boundingBox()).height >= 28, "its tap area is 28 px tall")
      assert.equal(await page.locator("tbody tr").first().evaluate((tr) => tr.getBoundingClientRect().height) <= 41, true, "and takes no more of the row than its bars")
      await cell.tap()
      await until(async () => (await opened()) && paired((await card(page).innerText()).split("\n").filter(Boolean)).join("|").startsWith("Failures 1|Seen working 12h|"), "a tap opens the card")
      await cell.tap()
      await until(async () => !(await opened()), "a second tap closes it")
      await cell.tap()
      await until(opened, "a tap opens it again")
      await page.getByRole("heading", { name: "All servers" }).tap()
      await until(async () => !(await opened()), "a tap elsewhere closes it")
    } finally {
      await close()
    }
  })

  /** All servers, a group's tab and both kinds of source's tab, each with the name of a row it shows. */
  const acmeTabs = () => {
    daemon.addAcme()
    const among = (values) => ({ not: false, values })
    daemon.set({ groups: new Map([["lw", groupView("lw", 1, { name: "Acme", members: { sources: among(["acme"]), countries: among([]), protocols: among([]), include: ".", exclude: "", box: null } }, [], daemon.catalogue())]]) })
    return { "servers": "Amsterdam", "groups/lw": "USA · proxy", "sources/acme": "USA · proxy", "sources/aurora": "Amsterdam" }
  }

  test("17. Stability leaves Name its earlier width on All servers, a group's tab and a source's tab, at 360, 390 and 768 px", async () => {
    const tabs = acmeTabs()
    /* Measured at fc91f35, the commit before the column, less the 4px the dot's column took since, and at 768 the 24px Test's
     * answer took since, beside its gauge: All servers and a source's tab, then a group's. */
    const WAS = { 360: [142, 110], 390: [172, 140], 768: [190, 158] }
    const { page, close } = await open("servers", { width: 360 })
    const failures = []
    try {
      for (const width of [360, 390, 768]) {
        await page.setViewportSize({ width, height: 900 })
        for (const [tab, name] of Object.entries(tabs)) {
          await page.goto(daemon.url + tab)
          await page.getByRole("button", { name, exact: true }).first().waitFor()
          const got = (await page.locator("thead th").filter({ hasText: /^Name/ }).boundingBox()).width
          const was = WAS[width][tab.startsWith("groups/") ? 1 : 0]
          if (got < was) failures.push(`${tab} at ${width} px: Name ${got}px, was ${was}px`)
        }
      }
    } finally {
      await close()
    }
    assert.deepEqual(failures, [])
  })

  test("17. Stability leaves the Exit menu's names their earlier widths, at 360, 390 and 768 px", async () => {
    acmeTabs()
    /* Measured at 4b32daa, the Exit menu before the column. */
    const WAS = { 360: { Servers: 62, Groups: 86, Sources: 86 }, 390: { Servers: 92, Groups: 116, Sources: 116 }, 768: { Servers: 214, Groups: 238, Sources: 238 } }
    const { page, close } = await open("", { width: 360 })
    const failures = []
    try {
      for (const width of [360, 390, 768]) {
        await page.setViewportSize({ width, height: 900 })
        await page.goto(daemon.url)
        await page.getByRole("combobox", { name: "Exit of EU" }).click()
        await page.getByRole("dialog").getByRole("option").first().waitFor()
        await until(() => page.getByRole("dialog").evaluate((el) => el.getAnimations({ subtree: true }).length === 0), "the menu to settle")
        const names = await page.getByRole("dialog").evaluate((el) => [...el.querySelectorAll("[cmdk-group]")].flatMap((g) => {
          const section = g.querySelector("[cmdk-group-heading]")?.textContent.match(/^(Servers|Groups|Sources)/)?.[1]
          return section ? [...g.querySelectorAll("[cmdk-item] > span.flex-1")].map((s) => [section, s.textContent, s.getBoundingClientRect().width]) : []
        }))
        assert.ok(names.length > 0, "the menu lists names")
        for (const [section, label, got] of names) if (got < WAS[width][section]) failures.push(`at ${width} px: ${label} ${got}px, was ${WAS[width][section]}px`)
      }
    } finally {
      await close()
    }
    assert.deepEqual(failures, [])
  })

  test("17. in the 358 px menu a phone gets, a name keeps the drawing's 182 px, since the picked option's check stands where its dot stands and takes no room of its own", async () => {
    const LONG = "Northwind Networks Premium Unlimited"
    daemon.set({ sources: new Map([["aurora", { ...daemon.source("aurora"), name: LONG }]]) })
    const { page, close } = await open("", { width: 390 })
    try {
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      const menu = page.getByRole("dialog")
      const option = menu.locator("[data-value='source:aurora']")
      await option.waitFor()
      await until(() => menu.evaluate((el) => el.getAnimations({ subtree: true }).length === 0), "the menu to settle")
      assert.equal(Math.round((await menu.boundingBox()).width), 358, "the menu a phone gets")
      const name = option.getByText(LONG, { exact: true })
      const kept = (await name.boundingBox()).width
      assert.ok(kept >= 182, `the name keeps ${kept}px`)
      const picked = menu.getByRole("option", { name: "All Aurora" })
      assert.equal(await picked.getByRole("img").count(), 0, "the picked option's check stands in its dot's place")
      const dot = await option.getByRole("img").first().boundingBox()
      const check = await picked.locator("svg.lucide-check").first().boundingBox()
      assert.ok(Math.abs(dot.x + dot.width / 2 - (check.x + check.width / 2)) < 1, "the check's centre is a dot's")
      const servers = await menu.locator("[data-slot=option-name]").all()
      assert.ok(servers.length > 0, "the menu lists servers")
      for (const words of servers) assert.ok(await words.evaluate((el) => el.scrollWidth <= el.clientWidth), `a server's name is cut by the source after it: ${await words.innerText()}`)
    } finally {
      await close()
    }
  })

  /** A card the menu opened, which isn't the menu itself. */
  const menuCard = (page) => page.locator("[data-radix-popper-content-wrapper]").filter({ hasNot: page.getByRole("listbox") })
  const menuCardLines = async (page) => paired((await menuCard(page).innerText().catch(() => "")).split("\n").map((l) => l.trim()).filter(Boolean))

  test("17. in the Exit menu on a phone a tap on an option's Ping or bars opens its card and picks nothing, a second tap closes it, and a tap elsewhere on the row picks the server", async () => {
    seed()
    const { page, close } = await open("", { width: 360, touch: true })
    try {
      await openMenu(page)
      const option = menuOption(page)("Lithuania · proxy")
      for (const cell of [option.locator("[data-slot=bars]"), option.getByText("36 / 50 ms", { exact: true })]) {
        await cell.tap()
        await until(async () => (await menuCardLines(page))[0] === first("Lithuania · proxy"), "a tap on either cell opens the option's card")
        assert.ok(await page.getByRole("listbox").isVisible(), "the menu stays open")
        assert.equal(requests("PUT", "api/slots/eu").length, 0, "the tap picks nothing")
        await cell.tap()
        await until(async () => (await menuCard(page).count()) === 0, "a second tap closes it")
      }
      await option.getByText("Lithuania · proxy · Aurora", { exact: true }).tap()
      const put = await until(() => requests("PUT", "api/slots/eu")[0], "a tap on the name picks the server")
      assert.deepEqual(JSON.parse(put.body).exit, { kind: "server", server: "f-lt" })
    } finally {
      await close()
    }
  })

  for (const engine of ENGINES) {
    test(`17. in the Exit menu on a phone, in ${engine}, a tap on an option's card closes it and does nothing else, and with the card open the first tap on another option's name picks that server and the first tap on an empty group's Open opens its page`, async () => {
      seed()
      const among = (values) => ({ not: false, values })
      const nothing = groupView("none", 1, { name: "Nothing", members: { sources: among([]), countries: among(["ZZ"]), protocols: among([]), include: "", exclude: "", box: null } })
      daemon.set({ groups: new Map([...daemon.groups().map((g) => [g.id, g]), ["none", nothing]]) })
      const { page, close } = await open("", { width: 360, touch: true, engine })
      const opensCard = async () => {
        await menuOption(page)("Lithuania · proxy").locator("[data-slot=bars]").tap()
        await until(async () => (await menuCardLines(page))[0] === first("Lithuania · proxy"), "a tap on the bars opens the option's card")
      }
      /** A point on another server option's name, clear of the open card, and that server. */
      const onAnotherName = () => page.getByRole("listbox").evaluate((list) => {
        for (const option of list.querySelectorAll("[cmdk-item]")) {
          const name = option.querySelector("[data-slot=option-name]")
          if (!name || name.textContent === "Lithuania · proxy") continue
          const r = name.getBoundingClientRect()
          const [x, y] = [r.left + 4, r.top + r.height / 2]
          if (option.contains(document.elementFromPoint(x, y))) return { x, y, server: option.getAttribute("data-value").replace(/^server:/, "") }
        }
        return null
      })
      try {
        await openMenu(page)
        await opensCard()
        const card = await menuCard(page).locator("[data-side]").first().boundingBox()
        await page.touchscreen.tap(card.x + 8, card.y + card.height / 2)
        await until(async () => (await menuCard(page).count()) === 0, "a tap on the card closes it")
        await page.waitForTimeout(300)
        assert.equal(requests("PUT", "api/slots/eu").length, 0, "the tap on the card picked nothing")
        assert.ok(await page.getByRole("listbox").isVisible(), "the menu stays open")
        await opensCard()
        await page.getByRole("dialog").locator('[data-value="group:none"]').getByRole("link", { name: "Open" }).tap()
        await page.waitForURL(/\/groups\/none$/, { timeout: 2_000 })
        assert.equal(requests("PUT", "api/slots/eu").length, 0, "Open picked nothing")
        await openMenu(page)
        await opensCard()
        const at = await onAnotherName()
        assert.ok(at, "another server's name clear of the card")
        await page.touchscreen.tap(at.x, at.y)
        const put = await until(() => requests("PUT", "api/slots/eu")[0], "the first tap on another name, the card open, to pick that server", 2_000)
        assert.deepEqual(JSON.parse(put.body).exit, { kind: "server", server: at.server })
      } finally {
        await close()
      }
    })
  }

  for (const width of [360, 1280]) {
    test(`17. in the Exit menu at ${width} px the option the arrow keys highlight shows its card, and Enter still picks it`, async () => {
      seed()
      const { page, close } = await open("", { width })
      const highlighted = () => page.getByRole("dialog").locator("[cmdk-item][data-selected=true]").innerText()
      try {
        await openMenu(page)
        const search = page.getByPlaceholder("Search")
        await search.pressSequentially("proxy-2")
        assert.match(await highlighted(), /proxy-2 · Aurora/, "typing highlights an option")
        assert.equal(await menuCard(page).count(), 0, "typing opens no card")
        await search.fill("proxy")
        for (let i = 0; !(await highlighted()).includes("Lithuania · proxy"); i++) {
          assert.ok(i < 30, "the arrow keys reach Lithuania")
          await page.keyboard.press("ArrowDown")
        }
        await until(async () => (await menuCardLines(page))[0] === first("Lithuania · proxy"), "the highlighted option shows its card")
        await page.keyboard.press("ArrowDown")
        await until(async () => (await menuCardLines(page))[0] !== first("Lithuania · proxy") && (await menuCard(page).count()) === 1, "the card follows the highlight")
        await page.keyboard.press("ArrowUp")
        await until(async () => (await menuCardLines(page))[0] === first("Lithuania · proxy"), "and comes back with it")
        await page.keyboard.press("Enter")
        const put = await until(() => requests("PUT", "api/slots/eu")[0], "Enter picks the server")
        assert.deepEqual(JSON.parse(put.body).exit, { kind: "server", server: "f-lt" })
      } finally {
        await close()
      }
    })
  }

  /** The menu's open cards: each one's first line, and whether that line is in front, not under another card. */
  const menuCards = (page) => page.evaluate(() => [...document.querySelectorAll("[data-radix-popper-content-wrapper]")]
    .filter((wrapper) => !wrapper.querySelector("[role=listbox]"))
    .map((wrapper) => {
      /* A named line is its name and its value, two cells of the card's grid. */
      const first = wrapper.querySelector("[role=dialog] > span")
      const cells = first.children.length > 0 ? [...first.children] : [first]
      const box = cells[0].getBoundingClientRect()
      return { first: cells.map((c) => c.textContent).join(" "), inFront: wrapper.contains(document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2)) }
    }))
  /** Long enough for a hover's delay to open a card that was going to open. */
  const HOVER_SETTLES_MS = 1000

  for (const width of [360, 1280]) {
    test(`17. in the Exit menu at ${width} px a key that moves the highlight from under the mouse shows only the highlighted option's card`, async () => {
      seed()
      const { page, close } = await open("", { width })
      const highlighted = () => page.getByRole("dialog").locator("[cmdk-item][data-selected=true]").innerText()
      try {
        await openMenu(page)
        await page.getByPlaceholder("Search").pressSequentially("proxy")
        await menuOption(page)("Lithuania · proxy").locator("[data-slot=bars]").hover()
        await until(async () => (await menuCards(page))[0]?.first === first("Lithuania · proxy"), "the mouse opens Lithuania's card")
        await page.keyboard.press("End")
        await until(async () => (await highlighted()).includes("Austria · proxy"), "End highlights Austria")
        await page.waitForTimeout(HOVER_SETTLES_MS)
        const cards = await menuCards(page)
        assert.ok(cards.length === 1 && cards[0].first === first("Austria · proxy") && cards[0].inFront, `only the highlighted option's card is open: ${JSON.stringify(cards)}`)
        await page.keyboard.press("Enter")
        const put = await until(() => requests("PUT", "api/slots/eu")[0], "Enter picks the server")
        assert.deepEqual(JSON.parse(put.body).exit, { kind: "server", server: "f-at" })
      } finally {
        await close()
      }
    })
  }

  test("17. in the Exit menu a mouse hover over another option's cells closes the card the arrow keys opened", async () => {
    seed()
    const { page, close } = await open("", { width: 1280 })
    const highlighted = () => page.getByRole("dialog").locator("[cmdk-item][data-selected=true]").innerText()
    try {
      await openMenu(page)
      await page.getByPlaceholder("Search").pressSequentially("proxy")
      for (let i = 0; !(await highlighted()).includes("Lithuania · proxy"); i++) {
        assert.ok(i < 30, "the arrow keys reach Lithuania")
        await page.keyboard.press("ArrowDown")
      }
      await until(async () => (await menuCards(page))[0]?.first === first("Lithuania · proxy"), "the highlighted option shows its card")
      await menuOption(page)("Latvia · proxy").locator("[data-slot=bars]").hover()
      await until(async () => (await menuCards(page))[0]?.first === first("Latvia · proxy"), "the mouse opens Latvia's card")
      await page.waitForTimeout(HOVER_SETTLES_MS)
      assert.deepEqual(await menuCards(page), [{ first: first("Latvia · proxy"), inFront: true }], "only the hovered option's card is open")
    } finally {
      await close()
    }
  })
})

describe("the reference feed", () => {
  const column = (page, n) => page.locator(`tbody tr:not(:has(> th)) td:nth-child(${n})`).allInnerTexts()
  const row = rowOf

  test("17. each config of the feed is keyed as Go keys a new one, by the least server ID among its roots no earlier config took", () => {
    const { servers, source: lw } = acme()
    assert.deepEqual(lw.feed.configs.map((c) => c.key), ["lw-node1", "lw-node10", "lw-node11", "lw-node12", "lw-node2", "lw-node3"])
    for (const s of servers) for (const seen of s.seen) assert.ok(lw.feed.configs.some((c) => c.key === seen.configKey && c.name === seen.config), `${s.id} in ${seen.config}`)
    for (const s of servers) assert.equal(lw.feed.configs.find((c) => c.key === s.home)?.name, s.name.remarks, `${s.label}'s home`)
  })

  test("17. a source's tab shows its 12 servers as 6 configs of 2 in feed order, each row its tag alone, with no Outbound and no Source", async () => {
    daemon.addAcme()
    const { page, close } = await open("sources/acme")
    try {
      await row(page, "USA · proxy").waitFor()
      const shown = await page.locator("tbody").evaluateAll((bodies) => bodies.map((b) => [b.querySelector("th[scope=rowgroup]")?.innerText.trim(),
        [...b.querySelectorAll("tr:not(:has(> th))")].map((tr) => tr.cells[2].innerText.trim())]))
      assert.deepEqual(shown, ["Germany", "Netherlands", "Sweden", "Bulgaria", "USA", "Romania"].map((c) => [c, ["proxy", "EDGE-STANDBY"]]),
        "the configs in feed order, each row its tag alone, as its config lists them, though the first config lists some EDGE-STANDBY first and every proxy scores better")
      assert.equal(await page.getByRole("checkbox").count(), 0, "no ticks")
      const headers = (await page.locator("thead th").allInnerTexts()).map((h) => h.trim())
      assert.ok(!headers.some((h) => /^(Outbound|Source)/.test(h)), `no Outbound and no Source: ${headers}`)
    } finally {
      await close()
    }
  })

  test("17. a source's tab with no config headings lists its servers in feed order, not best first", async () => {
    const config = (key, name, place) => ({ key, name, place, servers: 1, skipped: [] })
    daemon.set({ sources: new Map([["aurora", { ...source(1), feed: { configs: [config("l-par", "Paris", 1), config("l-ams", "Amsterdam", 2), config("l-fra", "Frankfurt", 3)] } }]]) })
    const { page, close } = await open("sources/aurora")
    try {
      await row(page, "Paris").waitFor()
      assert.equal(await page.locator("th[scope=rowgroup]").count(), 0, "a flat list")
      assert.deepEqual((await column(page, 3)).map((x) => x.trim()), ["Paris", "Amsterdam", "Frankfurt"], "by config place, though Frankfurt scores best and the catalogue lists Amsterdam first")
    } finally {
      await close()
    }
  })

  test("17. a source whose configs are each home to one server lists full names, and other tables show the tag muted", async () => {
    daemon.addAcme()
    const { page, close } = await open("sources/aurora")
    try {
      await row(page, "Amsterdam").waitFor()
      assert.equal(await page.locator("th[scope=rowgroup]").count(), 0, "a flat list")
      for (const tab of ["servers", "sources/acme"]) {
        await page.goto(daemon.url + tab)
        const name = page.getByRole("button", { name: "Sweden · proxy", exact: true })
        await name.waitFor()
        const tag = await name.evaluate((el) => [...el.querySelectorAll("*")].find((x) => x.textContent.trim() === "· proxy")?.className ?? "")
        if (tab === "servers") assert.match(tag, /text-muted-foreground/, "the tag is muted after the config's remarks")
        else assert.equal(await name.innerText(), "proxy", "under its config the row is its tag alone")
      }
    } finally {
      await close()
    }
  })

  test("17. the Test column is named in its header, as the details name their Test button", async () => {
    const { page, close } = await open("servers", { width: 1280 })
    try {
      const header = page.locator("thead th").last()
      await header.waitFor()
      assert.equal((await header.innerText()).trim(), "Test")
    } finally {
      await close()
    }
  })

  test("17. from 360 to 1600 px every column but Name has one width on All servers, a group's tab and both kinds of source's tab, each header button lies in its column, and a column that hides shows from one content width up without leaving a group's Name under 128px", async () => {
    daemon.addAcme()
    const among = (values) => ({ not: false, values })
    daemon.set({ groups: new Map([["lw", groupView("lw", 1, { name: "Acme", members: { sources: among(["acme"]), countries: among([]), protocols: among([]), include: ".", exclude: "", box: null } }, [], daemon.catalogue())]]) })
    const TABS = { "servers": "Amsterdam", "groups/lw": "USA · proxy", "sources/acme": "USA · proxy", "sources/aurora": "Amsterdam" }
    const SAME = ["Status", "Country", "Source", "Protocol", "Ping", "Stability", "In use", "Test"]
    const HIDES = ["Source", "Protocol", "In use", "Test"]
    const WIDTHS = Array.from({ length: 125 }, (_, i) => 360 + i * 10)
    const headers = () => [...document.querySelectorAll("thead th")].filter((th) => th.offsetWidth > 0).map((th) => {
      const column = th.getBoundingClientRect()
      const astray = [...th.querySelectorAll("button")].filter((b) => b.offsetWidth > 0).flatMap((b) => {
        const box = b.getBoundingClientRect()
        const hit = document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2)?.closest("button")
        return [
          ...(box.left < column.left - 0.5 || box.right > column.right + 0.5 ? [`${b.ariaLabel} spans ${box.left}-${box.right} in ${column.left}-${column.right}`] : []),
          ...(hit === b ? [] : [`the centre of ${b.ariaLabel} is ${hit?.ariaLabel ?? "no button"}`]),
        ]
      })
      return { label: th.textContent.trim(), width: column.width, astray }
    })
    const main = () => document.querySelector("[data-slot=main] > div:last-child").getBoundingClientRect().width
    const { page, close } = await open("servers", { width: WIDTHS[0] })
    const failures = []
    const widths = new Map(WIDTHS.map((w) => [w, new Map()]))
    /* For each tab and column that hides, the content widths where it shows and where it doesn't. */
    const seen = new Map()
    try {
      for (const [tab, name] of Object.entries(TABS)) {
        await page.setViewportSize({ width: WIDTHS[0], height: 900 })
        await page.goto(daemon.url + tab)
        await page.getByRole("button", { name, exact: true }).first().waitFor()
        for (const width of WIDTHS) {
          await page.setViewportSize({ width, height: 900 })
          const shown = await page.evaluate(headers)
          for (const { label, astray } of shown) for (const why of astray) failures.push(`${tab} at ${width} px, ${label}: ${why}`)
          const of = (label) => shown.find((h) => h.label.startsWith(label))
          const kept = ["Status", "Country", "Name", "Ping", "Stability"]
          for (const label of kept) if (!of(label)) failures.push(`${tab} at ${width} px hides ${label}`)
          for (const label of SAME) if (of(label)) widths.get(width).set(label, [...(widths.get(width).get(label) ?? []), `${tab} ${of(label).width}`])
          const content = await page.evaluate(main)
          for (const label of HIDES) seen.set(`${tab} ${label}`, [...(seen.get(`${tab} ${label}`) ?? []), [content, Boolean(of(label))]])
          if (tab.startsWith("groups/") && HIDES.some(of) && of("Name").width < 128) failures.push(`${tab} at ${width} px shows ${HIDES.filter(of)} and leaves Name ${of("Name").width}px`)
          if (width === WIDTHS.at(-1)) for (const label of HIDES) if (!of(label) && !(label === "Source" && tab.startsWith("sources/"))) failures.push(`${tab} at ${width} px hides ${label}`)
        }
      }
    } finally {
      await close()
    }
    for (const [width, columns] of widths) for (const [label, tabs] of columns) {
      if (new Set(tabs.map((t) => t.split(" ")[1])).size > 1) failures.push(`${label} at ${width} px: ${tabs.join(", ")}`)
    }
    for (const [what, at] of seen) {
      const from = Math.min(...at.filter(([, shows]) => shows).map(([content]) => content))
      const hidden = at.filter(([content, shows]) => !shows && content >= from)
      if (hidden.length) failures.push(`${what} shows from ${from}px of content but hides at ${hidden.map(([content]) => content)}`)
    }
    assert.deepEqual(failures, [])
  })

  test("17. no source lists an outbound or the feed's make-up, and a server's dialog lists no config and writes nothing of how it is named, and draws its country as the table does, with where it came from in its card", async () => {
    daemon.addAcme()
    const { servers } = acme()
    const { page, close } = await open("sources/acme")
    try {
      await row(page, "USA · proxy").waitFor()
      await refreshButton(page).focus()
      await until(async () => /^Next in \d+(m|h) Refresh every \d+(m|h|d)$/.test((await card(page).innerText().catch(() => "")).replace(/\s+/g, " ").trim()), "Refresh's card is when it runs next and how often")
      assert.doesNotMatch(await page.locator("main").innerText(), /Helper|freedom|blackhole|dials through/, "no helper or hop is listed")
      assert.equal(await page.getByRole("button", { name: /skipped/ }).count(), 0, "helpers aren't skipped entries")
      await page.keyboard.press("Escape")
      for (const [name, country] of [["Sweden · proxy", "Sweden"], ["Sweden · EDGE-STANDBY", "Sweden"], ["Bulgaria · proxy", "Bulgaria"]]) {
        await page.getByRole("button", { name, exact: true }).click()
        const dialog = page.getByRole("dialog")
        const words = await dialog.innerText()
        assert.doesNotMatch(words, /Named from|(^|\n)(Name|Config|Outbound)\s/, `${name}'s details write nothing of how it is named`)
        assert.ok(servers.find((x) => x.label === name).seen.length > 1, `${name} sits in more than one config`)
        assert.doesNotMatch(words, /Configs/, `${name}: the details list no config, since the name says its home`)
        assert.equal(await dialog.getByRole("listitem").count(), 0, `${name}: no config is listed`)
        assert.match(words, /\nTest\nCountry\n/, `${name}: Country leads the details, right under the title's line`)
        const where = dialog.getByRole("button", { name: `${country}: From the tag's flag`, exact: true })
        assert.equal(await shownWords(where), country, `${name}: its country's name beside its flag, where it came from not written`)
        assert.equal(await where.locator("img").count(), 1, `${name}: the flag`)
        await where.focus()
        await until(async () => (await card(page).innerText().catch(() => "")) === "From the tag's flag", `${name}: where it came from is in the country's card`)
        await page.keyboard.press("Escape")
        await card(page).waitFor({ state: "detached" })
        await page.keyboard.press("Escape")
        await dialog.waitFor({ state: "detached" })
      }
      assert.doesNotMatch(await headingOf(page).innerText(), /kept|keeps|default/i)
    } finally {
      await close()
    }
  })

  test("17. every filter's options and chips draw a value as its column's cells do, but Country's cell is its flag alone, left of the name, where its options and chips add the name", async () => {
    daemon.addAcme()
    const { page, close } = await open("servers")
    /* Each value drawn under `selector`: its filter, its words and whether they're in sight, its mark, and an option's count. */
    const drawn = (selector) => page.evaluate((sel) => [...document.querySelectorAll(`${sel} [data-facet]`)].map((el) => ({
      facet: el.dataset.facet, words: el.lastElementChild.textContent, shown: el.lastElementChild.getBoundingClientRect().width > 1, mark: el.querySelector(":scope > [aria-hidden]")?.innerHTML ?? "",
      count: el.closest("[role=option]")?.querySelector(":scope > .tabular-nums").textContent ?? null, ticked: el.closest("[role=option]")?.ariaChecked === "true",
    })), selector)
    try {
      for (const [tab, facets] of [["servers", ["status", "country", "source", "protocol"]], ["sources/acme", []]]) {
        await page.goto(daemon.url + tab)
        await row(page, "USA · proxy").waitFor()
        const flags = await page.locator("tbody tr:not(:has(> th))").evaluateAll((trs) => trs.map((tr) => {
          const [flag, ...more] = tr.querySelectorAll("img")
          const name = tr.querySelector("td button[type=button]")
          return { more: more.length, gap: name.getBoundingClientRect().left - flag.getBoundingClientRect().right, alt: flag.alt, title: flag.title }
        }))
        assert.ok(flags.every((f) => f.more === 0 && f.gap >= 0 && f.gap < 24), `${tab}: one flag a row, directly left of its name: ${JSON.stringify(flags)}`)
        assert.ok(flags.every((f) => f.alt && f.alt === f.title), `${tab}: a flag names its country on hover`)
        const nameAt = await page.locator("thead th").evaluateAll((ths) => ths.findIndex((th) => th.textContent.trim().startsWith("Name")) + 1)
        const names = await column(page, nameAt)
        assert.ok(names.length > 0 && names.every((n) => !/[\u{1F1E6}-\u{1F1FF}]/u.test(n)), `${tab}: no flag in a name, ${names}`)
        const cells = await drawn("tbody")
        const countries = cells.filter((c) => c.facet === "country")
        assert.ok(countries.length === flags.length && countries.every((c) => !c.shown && c.words), `${tab}: a Country cell shows no word, and keeps its name for a screen reader`)
        const looks = new Map()
        for (const { facet, words, mark } of cells) looks.set(`${facet} ${words}`, [...new Set([...(looks.get(`${facet} ${words}`) ?? []), mark])])
        for (const [value, marks] of looks) assert.equal(marks.length, 1, `${tab}: every cell draws ${value} alike`)
        const same = (where) => ({ facet, words, mark }) => assert.deepEqual(looks.get(`${facet} ${words}`), [mark], `${tab}: ${where} draws ${facet} ${words} as its cells do`)
        const offered = new Set()
        for (const header of await page.getByRole("button", { name: /^Filter servers by / }).all()) {
          if (!(await header.isVisible())) continue
          await header.click()
          const dialog = page.getByRole("dialog")
          const tabs = await dialog.locator('[aria-label="Filter"]').getByRole("radio").all()
          for (const filter of tabs.length ? tabs : [null]) {
            await filter?.click()
            const options = (await drawn("[role=dialog] [role=option]")).filter((o) => o.count !== "0")
            options.forEach(({ facet, words, mark }) => same("an option")({ facet, words, mark }))
            assert.ok(options.every((o) => o.shown), `${tab}: an option shows its words`)
            const pick = options.find((o) => !o.ticked)
            if (!pick) continue
            offered.add(pick.facet)
            await dialog.getByRole("option").filter({ has: page.locator(`[data-facet=${pick.facet}]`) }).filter({ hasText: pick.words }).first().click()
          }
          await page.keyboard.press("Escape")
          await dialog.waitFor({ state: "detached" })
        }
        assert.deepEqual([...offered].toSorted(), facets.toSorted(), `${tab}: every filterable column offers its values`)
        const chips = await drawn('[aria-label="Filters"]')
        chips.forEach(({ facet, words, mark }) => same("a chip")({ facet, words, mark }))
        assert.ok(chips.every((c) => c.shown), `${tab}: a chip shows its words`)
        assert.deepEqual([...new Set(chips.map((c) => c.facet))].toSorted(), facets.toSorted(), `${tab}: a chip for each filter set`)
      }
    } finally {
      await close()
    }
  })

  test("17. All servers and a group's tab start their chip line with Filter at 360, 768 and 1280 px, which opens all of the tab's filters in its columns' order, and the headers keep theirs", async () => {
    daemon.addAcme()
    const among = (values) => ({ not: false, values })
    daemon.set({ groups: new Map([["lw", groupView("lw", 1, { name: "Acme", members: { sources: among(["acme"]), countries: among([]), protocols: among([]), include: "", exclude: "", box: null } }, [], daemon.catalogue())]]) })
    for (const width of [360, 768, 1280]) {
      for (const [tab, labels] of [["servers", ["Status", "Country", "Name", "Source", "Protocol"]], ["groups/lw", ["Selection", "Country", "Name", "Source", "Protocol"]]]) {
        const { page, close } = await open(tab, { width })
        try {
          await row(page, "USA · proxy").waitFor()
          const button = page.getByRole("button", { name: "Filter", exact: true })
          assert.ok(await button.isVisible(), `${tab} at ${width} px: Filter in sight`)
          const [at, line] = [await button.boundingBox(), await page.getByRole("group", { name: "Filters", exact: true }).boundingBox()]
          assert.ok(at.x + at.width <= line.x && Math.abs(at.y + at.height / 2 - (line.y + line.height / 2)) < 2, `${tab} at ${width} px: Filter starts the chip line`)
          await button.click()
          const dialog = page.getByRole("dialog")
          assert.deepEqual((await dialog.locator('[aria-label="Filter"]').getByRole("radio").allInnerTexts()).map((x) => x.trim()), labels, `${tab} at ${width} px: every filter the tab has`)
          const box = await dialog.boundingBox()
          assert.ok(box.x >= 0 && box.x + box.width <= width, `${tab} at ${width} px: the popover fits the screen`)
          await page.keyboard.press("Escape")
          await dialog.waitFor({ state: "detached" })
          assert.ok(await page.locator("thead").getByRole("button", { name: "Filter servers by Country", exact: true }).isVisible(), `${tab} at ${width} px: Country's header still opens its filter`)
          assert.equal(await page.getByRole("button", { name: /^Filter servers by / }).evaluateAll((bs) => bs.filter((b) => !b.closest("thead")).length), 0, `${tab} at ${width} px: no column's filter is offered outside its header`)
        } finally {
          await close()
        }
      }
    }
  })

  test("17. at 360 and 390 px the chips wrap and every chip's × is inside the screen: a group's five saved filters, and Country beside an open search that matches nothing; each can be removed", async () => {
    daemon.addAcme()
    const among = (values) => ({ not: false, values })
    const members = { sources: among(["acme"]), countries: among(["DE", "NL"]), protocols: among(["vless"]), include: "an", exclude: "EDGE", box: null }
    daemon.set({ groups: new Map([["lw", groupView("lw", 1, { name: "Acme", members }, [], daemon.catalogue())]]) })
    const inReach = async (page, at) => {
      const line = page.getByRole("group", { name: "Filters", exact: true })
      const boxes = await line.getByRole("button", { name: /^Remove / }).evaluateAll((bs) => bs.map((b) => { const r = b.getBoundingClientRect(); return [r.left, r.right, innerWidth] }))
      assert.ok(boxes.length > 0, at)
      assert.ok(boxes.every(([left, right, screen]) => left >= 0 && right <= screen), `every × inside the screen, ${at}: ${JSON.stringify(boxes)}`)
      assert.ok(await line.evaluate((el) => el.scrollWidth <= el.clientWidth), `the line hides nothing sideways, ${at}`)
      assert.ok(await page.evaluate(() => document.scrollingElement.scrollWidth <= innerWidth), `the page doesn't scroll sideways, ${at}`)
    }
    for (const width of [360, 390]) {
      const { page, close } = await open("groups/lw", { width })
      try {
        await rowOf(page, "Germany · proxy").waitFor()
        const removes = page.getByRole("group", { name: "Filters", exact: true }).getByRole("button", { name: /^Remove / })
        assert.equal(await removes.count(), 5)
        await inReach(page, `a group's five filters at ${width} px`)
        for (let left = 5; left > 0; left--) {
          await removes.first().click()
          await until(async () => (await removes.count()) === left - 1, `${left - 1} chips left`)
        }
      } finally {
        await close()
      }
      const all = await open("servers", { width })
      try {
        await rowOf(all.page, "Amsterdam").waitFor()
        await openFilter(all.page, "Country")
        await all.page.getByRole("dialog").getByRole("option", { name: /Germany/ }).click()
        await all.page.keyboard.press("Escape")
        await all.page.getByRole("button", { name: "Search" }).click()
        await all.page.getByPlaceholder("Search").fill("zzzz")
        await all.page.getByText("No match", { exact: true }).waitFor()
        await inReach(all.page, `Country beside an open search at ${width} px`)
      } finally {
        await all.close()
      }
    }
  })

  test("17. a source's tab has search and no filter, its headers plain words but Stability's card, at 360 and 1280 px", async () => {
    daemon.addAcme()
    const swiss = new Set(["lw-node9", "lw-node10"])
    daemon.set({ catalogue: daemon.catalogue().map((l) => (swiss.has(l.id) ? { ...l, country: { code: "CH", source: "rule" } } : l)) })
    for (const width of [360, 1280]) {
      const { page, close } = await open("sources/acme", { width })
      try {
        await row(page, "USA · proxy").waitFor()
        assert.equal(await page.getByRole("button", { name: /^Filter servers by / }).count(), 0, `${width} px: no header opens a filter`)
        assert.equal(await page.getByRole("button", { name: "Filter", exact: true }).count(), 0, `${width} px: no Filter button`)
        assert.equal(await page.getByRole("group", { name: "Filters", exact: true }).count(), 0, `${width} px: no chip line`)
        assert.deepEqual(await page.locator("thead button").evaluateAll((bs) => bs.map((b) => b.ariaLabel)), ["Stability"], `${width} px: every header is plain words but Failed 7 d's hover`)
        for (const header of ["Name", "Ping", "Stability"]) assert.ok(await page.locator("thead th", { hasText: header }).isVisible(), `${width} px: ${header}`)
        assert.ok(await page.evaluate(() => document.scrollingElement.scrollWidth <= innerWidth), `${width} px: no sideways scrolling`)
        await page.getByPlaceholder("Search").fill("switzerland")
        await until(async () => (await page.locator("tbody tr:not(:has(> th))").count()) === 2, `${width} px: search finds the two servers a rule puts in Switzerland, which no name or tag says`)
        assert.equal(requests("PUT", "api/sources/acme").length, 0)
      } finally {
        await close()
      }
    }
  })

  test("17. a source's tab with no row in sight keeps its header widths, at 1280 and 390 px", async () => {
    daemon.addAcme()
    const headers = (page) => page.locator("thead th").evaluateAll((ths) => ths.filter((th) => th.offsetWidth > 0).map((th) => [th.textContent.trim(), th.getBoundingClientRect().width]))
    for (const width of [1280, 390]) {
      const { page, close } = await open("sources/acme", { width })
      try {
        await row(page, "USA · proxy").waitFor()
        const before = await headers(page)
        await page.getByPlaceholder("Search").fill("no-such-name")
        await page.getByText("No match", { exact: true }).waitFor()
        assert.deepEqual(await headers(page), before, `${width} px: every header keeps its width with no row in sight`)
      } finally {
        await close()
      }
    }
  })
})

describe("a group's page", () => {
  const among = (values) => ({ not: false, values })
  const rule = (sources, countries = [], box = null) => ({ sources: among(sources), countries: among(countries), protocols: among([]), include: "", exclude: "", box })
  const seed = () => daemon.set({
    groups: new Map([
      ["all-aurora", groupView("all-aurora", 1, { name: "All Aurora", members: rule(["aurora"]) }, ["eu"])],
      ["germany", groupView("germany", 2, { name: "Germany", members: rule([], ["DE"]) })],
      ["mine", groupView("mine", 1, { name: "Mine", members: rule([], [], "ticked"), ticked: ["l-ams", "l-fra"] }, ["eu"])],
    ]),
  })
  const row = rowOf

  test("17. the list names each group with its members, and a group's table says which slots use what", async () => {
    seed()
    const { page, close } = await open("groups/all-aurora")
    try {
      await row(page, "Amsterdam").waitFor()
      const link = (id) => nav(page).locator(`a[href$="/groups/${id}"]`)
      assert.match(await link("all-aurora").innerText(), /All Aurora\s+3/)
      assert.match(await link("mine").innerText(), /Mine\s+2/)
      await link("germany").waitFor()
      assert.match(await row(page, "Amsterdam").innerText(), /\bEU\b/)
      assert.doesNotMatch(await row(page, "Amsterdam").innerText(), /via/, "the group's own tab needs no via")
      assert.doesNotMatch(await row(page, "Frankfurt").innerText(), /EU/, "only the server the slot is on")
    } finally {
      await close()
    }
  })

  test("17. In use says via what a slot reaches a server, except on that source's or group's own tab", async () => {
    seed()
    const inUse = async (path, exit) => {
      if (exit) daemon.set({ slots: new Map([["eu", slotView("eu", 3, { name: "EU", exit })]]) })
      const { page, close } = await open(path)
      try {
        await row(page, "Amsterdam").getByText(/^EU/).waitFor()
        return (await row(page, "Amsterdam").innerText()).match(/EU[^\n\t]*/)[0].trim()
      } finally {
        await close()
      }
    }
    assert.equal(await inUse("servers"), "EU via All Aurora", "All servers")
    assert.equal(await inUse("groups/mine"), "EU via All Aurora", "another group's tab")
    assert.equal(await inUse("sources/aurora"), "EU via All Aurora", "the source the group reaches it through")
    assert.equal(await inUse("groups/all-aurora"), "EU", "the group's own tab")
    assert.equal(await inUse("sources/aurora", { kind: "source", source: "aurora" }), "EU", "the source's own tab")
    assert.equal(await inUse("groups/all-aurora", { kind: "source", source: "aurora" }), "EU via Aurora", "a group's tab, for a slot on a source")
  })

  test("17. a group's Source filter offers every source, one with no server included", async () => {
    daemon.set({ sources: new Map([["aurora", source(1)], ["empty", { ...source(1), id: "empty", name: "Empty", firstChoice: null, order: [], stored: 0 }]]) })
    const { page, close } = await open("groups/all-aurora", { width: 390 })
    try {
      await row(page, "Amsterdam").waitFor()
      const dialog = await openFilter(page, "Source")
      const options = (await dialog.getByRole("option").allInnerTexts()).map((x) => x.replace(/\s+/g, " ").trim())
      assert.deepEqual(options, ["Aurora 3", "Empty 0"])
      await dialog.getByRole("option", { name: /Empty/ }).click()
      await page.keyboard.press("Escape")
      await page.getByRole("group", { name: "Source filter" }).getByText("Empty").waitFor()
    } finally {
      await close()
    }
  })

  test("17. with no groups, Add still offers a group, and makes one", async () => {
    daemon.set({ groups: new Map(), slots: new Map() })
    const { page, close } = await open("servers")
    try {
      await nav(page).getByRole("link", { name: /Aurora/ }).waitFor()
      assert.equal(await nav(page).getByText("Groups", { exact: true }).count(), 0)
      await draftGroup(page)
      await boxOf(page, "Paris").click()
      await saveButton(page).click()
      await page.waitForURL(STORED_GROUP)
      await nav(page).getByText("Groups", { exact: true }).waitFor()
    } finally {
      await close()
    }
  })

  /** What the group writes sent: creates and replaces, not the previews. */
  const groupPuts = () => daemon.log().filter((r) => r.method === "PUT" && /\/api\/groups\/[^/]+$/.test(r.path))
  const add = (page) => nav(page).getByRole("button", { name: "Add", exact: true })

  test("17. Add, Group opens a draft that stores nothing: its name in focus, Add the list's current entry, no entry of its own, no option in the Exit menu, and Save off", async () => {
    for (const width of [1440, 390]) {
      const { page, context, close } = await open("servers", { width })
      try {
        await page.locator("tbody tr").first().waitFor()
        if (width < 768) await page.getByRole("button", { name: "Menu" }).click()
        await draftGroup(page)
        await until(() => page.evaluate(() => document.activeElement?.getAttribute("aria-label") === "Name of New group"), `${width} px: the name in focus`)
        await page.keyboard.type("Hand picked")
        await page.keyboard.press("Enter")
        await page.getByRole("heading", { name: "Hand picked" }).waitFor()
        assert.ok(await saveButton(page).isDisabled(), `${width} px: Save off while the draft holds nothing`)
        await boxOf(page, "Paris").click()
        if (width < 768) await page.getByRole("button", { name: "Menu" }).click()
        assert.equal(await add(page).getAttribute("aria-current"), "page", `${width} px: Add is the current entry`)
        assert.equal(await nav(page).getByRole("link", { name: /Hand picked|New group/ }).count(), 0, `${width} px: no entry of its own`)
        if (width < 768) await page.keyboard.press("Escape")
        const other = await context.newPage()
        await other.goto(daemon.url)
        await other.getByRole("combobox", { name: "Exit of EU" }).click()
        await other.getByRole("dialog").getByRole("option", { name: /All Aurora/ }).waitFor()
        assert.equal(await other.getByRole("dialog").getByRole("option", { name: /Hand picked|New group/ }).count(), 0, `${width} px: no option in the Exit menu`)
        assert.deepEqual(groupPuts(), [], `${width} px: nothing stored`)
        assert.ok(await tickedOf(page, "Paris"), `${width} px: the draft is as it was`)
      } finally {
        await close()
      }
      daemon.reset()
    }
  })

  test("17. a draft's boxes take no tick while its Save is out, so the group it opens holds every tick it showed", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.press("Enter")
      await boxOf(page, "Paris").click()
      await until(async () => await saveButton(page).isEnabled(), "Save on with a tick")
      const door = holdWrites(page, /\/api\/groups\/(?!preview$)[^/]+$/)
      await door.install()
      await saveButton(page).click()
      await boxOf(page, "Amsterdam").click({ force: true })
      await page.waitForTimeout(200)
      assert.equal(await tickedOf(page, "Amsterdam"), false, "a tick while the create is out isn't taken")
      assert.equal(await boxOf(page, "Amsterdam").getAttribute("aria-disabled"), "true", "and its box says so")
      await door.release()
      await page.waitForURL(STORED_GROUP)
      const id = new URL(page.url()).pathname.split("/").at(-1)
      assert.deepEqual(daemon.group(id).ticked, ["l-par"])
      assert.equal(await tickedOf(page, "Amsterdam"), false)
    } finally {
      await close()
    }
  })

  test("17. a draft's filters take no edit while its Save is out: the × of the Selection is Selected a tick set is off, and the group it opens has nothing left to save", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.press("Enter")
      await boxOf(page, "Paris").click()
      await until(async () => await saveButton(page).isEnabled(), "Save on with a tick")
      const door = holdWrites(page, /\/api\/groups\/(?!preview$)[^/]+$/)
      await door.install()
      await saveButton(page).click()
      const remove = page.getByRole("button", { name: "Remove Selection filter" })
      assert.equal(await remove.isDisabled(), true, "the chip's × takes no press while the create is out")
      await remove.click({ force: true })
      await page.waitForTimeout(200)
      assert.equal(await tickedOf(page, "Paris"), true, "Paris stays ticked")
      await door.release()
      await page.waitForURL(STORED_GROUP)
      const id = new URL(page.url()).pathname.split("/").at(-1)
      assert.deepEqual([daemon.group(id).ticked, daemon.group(id).members.box], [["l-par"], "ticked"])
      await page.getByRole("group", { name: "Selection filter" }).waitFor()
      assert.equal(await saveBar(page).count(), 0, "nothing left to save on the group it opens")
    } finally {
      await close()
    }
  })

  test("17. a draft's Save after no answer, ticked since, makes the group it sent and opens it with every tick", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await draftGroup(page)
      await page.keyboard.press("Enter")
      await boxOf(page, "Paris").click()
      await until(async () => await saveButton(page).isEnabled(), "Save on with a tick")
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/state", async (r) => { await held; await r.continue().catch(() => {}) })
      let first = null
      await page.route(/\/api\/groups\/(?!preview$)[^/]+$/, async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        first ??= r.request().postData()
        if (r.request().postData() !== first) return r.continue()
        await r.fetch()
        return r.abort("connectionreset")
      })
      await saveButton(page).click()
      await page.clock.fastForward(31_000)
      await toast(page, "timed out").waitFor()
      await boxOf(page, "Amsterdam").click()
      await until(async () => await tickedOf(page, "Amsterdam"), "Amsterdam ticked after no answer")
      await saveButton(page).click()
      await until(() => groupPuts().some((w) => w.status === 200 && JSON.parse(w.body).ticked.length === 2) || groupPuts().some((w) => w.status === 412), "the second Save answered")
      release()
      for (let i = 0; i < 4 && !STORED_GROUP.test(page.url()); i++) {
        await page.clock.fastForward(11_000)
        await page.waitForTimeout(300)
      }
      await page.waitForURL(STORED_GROUP)
      const id = new URL(page.url()).pathname.split("/").at(-1)
      assert.deepEqual(daemon.group(id).ticked.toSorted(), ["l-ams", "l-par"], "the group opened holds both ticks")
      assert.ok(await tickedOf(page, "Paris") && (await tickedOf(page, "Amsterdam")), "and its tab shows them")
    } finally {
      await close()
    }
  })

  /** A draft ticked at Paris whose Save lands at the daemon and loses every answer, with the view's reads held: Request timed out. */
  async function draftLandedUnheard(page, name = "") {
    await draftGroup(page)
    await page.keyboard.type(name)
    await page.keyboard.press("Enter")
    await boxOf(page, "Paris").click()
    await until(async () => await saveButton(page).isEnabled(), "Save on with a tick")
    let release
    const held = new Promise((done) => { release = done })
    await page.route("**/api/state", async (r) => { await held; await r.continue().catch(() => {}) })
    const creates = []
    await page.route(/\/api\/groups\/(?!preview$)[^/]+$/, async (r) => {
      if (r.request().method() !== "PUT" || r.request().headers()["if-none-match"] !== "*") return r.continue()
      creates.push({ path: new URL(r.request().url()).pathname, key: r.request().headers()["idempotency-key"] })
      await r.fetch()
      return r.abort("connectionreset")
    })
    await saveButton(page).click()
    await page.clock.fastForward(31_000)
    await toast(page, "timed out").waitFor()
    return { creates, release }
  }
  const opensStored = async (page) => {
    for (let i = 0; i < 6 && !STORED_GROUP.test(page.url()); i++) {
      await page.clock.fastForward(11_000)
      await page.waitForTimeout(300)
    }
    await page.waitForURL(STORED_GROUP)
    return new URL(page.url()).pathname.split("/").at(-1)
  }

  test("17. a draft's Save after no answer over a draft ticked and unticked back to what it sent sends that create again under its key, and one group is made", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      const before = new Set(daemon.groups().map((g) => g.id))
      const { creates, release } = await draftLandedUnheard(page)
      await boxOf(page, "Amsterdam").click()
      await until(async () => await tickedOf(page, "Amsterdam"), "Amsterdam ticked after no answer")
      await boxOf(page, "Amsterdam").click()
      await until(async () => !(await tickedOf(page, "Amsterdam")), "and unticked")
      const sent = creates.length
      await saveButton(page).click()
      await until(() => creates.length > sent, "the second Save sent")
      release()
      await page.clock.fastForward(31_000)
      const id = await opensStored(page)
      assert.equal(new Set(creates.map((c) => c.path)).size, 1, "one id")
      assert.equal(new Set(creates.map((c) => c.key)).size, 1, "one key")
      assert.deepEqual(daemon.groups().filter((g) => !before.has(g.id)).map((g) => g.id), [id], "one group, the one opened")
    } finally {
      await close()
    }
  })

  test("17. a draft's create found by a read after no answer takes its toast and its Retry with it, so a rename of the group it opens leaves neither", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      const { release } = await draftLandedUnheard(page)
      release()
      const id = await opensStored(page)
      await page.getByRole("button", { name: "Rename New group" }).click()
      await page.getByLabel("Name of New group").fill("Paris team")
      await page.keyboard.press("Enter")
      await until(() => daemon.group(id).name === "Paris team", "the rename lands")
      await page.waitForTimeout(300)
      assert.equal(await toast(page, "timed out").count(), 0, "no timed-out toast for a create the view holds")
      assert.equal(await page.getByRole("button", { name: "Retry" }).count(), 0, "and no Retry to send it again")
    } finally {
      await close()
    }
  })

  test("17. after no answer, Discard gives the draft a create of its own: the earlier create found by a later read opens nothing, and the name typed is kept", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      const { release } = await draftLandedUnheard(page, "Fast EU")
      await discardOf(page).click()
      await until(async () => (await toast(page, "timed out").count()) === 0 && !(await tickedOf(page, "Paris")), "Discard takes the toast and the tick")
      await boxOf(page, "Frankfurt").click()
      await until(async () => await tickedOf(page, "Frankfurt"), "Frankfurt ticked after Discard")
      release()
      const listed = nav(page).getByRole("link", { name: /Fast EU/ })
      for (let i = 0; i < 6 && (await listed.count()) === 0; i++) {
        await page.clock.fastForward(11_000)
        await page.waitForTimeout(300)
      }
      await listed.waitFor()
      await page.waitForTimeout(300)
      assert.equal(new URL(page.url()).pathname, BASE + "groups/new", "the read that lists the earlier group leaves the draft open")
      assert.deepEqual([await tickedOf(page, "Paris"), await tickedOf(page, "Frankfurt")], [false, true], "with what the owner did since")
      assert.equal(await page.getByRole("heading", { name: "Fast EU" }).count(), 1, "and the name typed")
    } finally {
      await close()
    }
  })

  test("17. after no answer, a draft unticked back to fresh by hand still offers Discard beside Retry, and Discard takes the Retry", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await draftLandedUnheard(page)
      await boxOf(page, "Paris").click()
      await until(async () => !(await tickedOf(page, "Paris")), "Paris unticked by hand")
      await page.getByRole("button", { name: "Retry" }).waitFor()
      assert.equal(await discardOf(page).count(), 1, "Discard stands while there is a Retry to take")
      await discardOf(page).click()
      await page.getByRole("button", { name: "Retry" }).waitFor({ state: "detached" })
      assert.equal(await discardOf(page).count(), 0, "then nothing is left for it")
    } finally {
      await close()
    }
  })

  test("17. a draft's rule with no condition holds no server at once, its preview unread: the Selection chip's × dims every row while ticks remain, and Discard lights every row", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.press("Enter")
      for (const [act, why, lit] of [[() => page.getByRole("button", { name: "Remove Selection filter" }).click(), "the Selection chip's ×", false], [() => discardOf(page).click(), "Discard", true]]) {
        await boxOf(page, "Paris").click()
        await boxOf(page, "Amsterdam").click()
        await page.getByText("2 in group", { exact: true }).waitFor()
        let release
        const held = new Promise((done) => { release = done })
        await page.route("**/api/groups/preview", async (route) => { await held; await route.continue().catch(() => {}) })
        await act()
        await page.getByRole("group", { name: "Selection filter" }).waitFor({ state: "detached" })
        await page.waitForTimeout(400)
        assert.equal(await page.getByText("0 in group", { exact: true }).count(), 1, `${why}: no server, with no read`)
        for (const name of ["Paris", "Amsterdam", "Frankfurt"]) assert.equal(await dimmed(page, name), !lit, `${why}: ${name} ${lit ? "lit" : "dimmed"}`)
        release()
        await page.unroute("**/api/groups/preview")
        if (!lit) {
          for (const name of ["Paris", "Amsterdam"]) await boxOf(page, name).click()
          await until(async () => !(await tickedOf(page, "Paris")) && !(await tickedOf(page, "Amsterdam")), "unticked for the next round")
        }
      }
    } finally {
      await close()
    }
  })

  test("17. a draft's Save turns on with a first tick, which sets Selection is Selected, and sends the name and its ticks in one create that opens the group holding them", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.type("Picked")
      await page.keyboard.press("Enter")
      assert.ok(await saveButton(page).isDisabled())
      await boxOf(page, "Paris").click()
      await page.getByRole("group", { name: "Selection filter" }).waitFor()
      await until(async () => !(await dimmed(page, "Paris")) && (await dimmed(page, "Amsterdam")), "Paris in, Amsterdam out")
      await boxOf(page, "Paris").click()
      await until(async () => await saveButton(page).isDisabled(), "Save off once nothing is ticked")
      await boxOf(page, "Paris").click()
      await boxOf(page, "Amsterdam").click()
      await page.getByText("2 in group", { exact: true }).waitFor()
      assert.ok(await saveButton(page).isEnabled())
      await saveButton(page).click()
      await page.waitForURL(STORED_GROUP)
      const [put] = groupPuts()
      assert.equal(groupPuts().length, 1, "one write")
      assert.equal(put.ifNoneMatch, "*", "a create")
      const id = new URL(page.url()).pathname.split("/").at(-1)
      const stored = daemon.group(id)
      assert.deepEqual([stored.name, stored.members.box, stored.ticked, stored.resolved.toSorted()], ["Picked", "ticked", ["l-ams", "l-par"], ["l-ams", "l-par"]])
      await nav(page).getByRole("link", { name: /Picked/ }).getByText("2", { exact: true }).waitFor()
      assert.ok(await tickedOf(page, "Paris") && (await tickedOf(page, "Amsterdam")), "the group's tab has its ticks")
      assert.equal(await add(page).getAttribute("aria-current"), null, "Add is no longer the current entry")
    } finally {
      await close()
    }
  })

  /** A draft back to fresh: no chip, no box ticked, "0 in group", Save off, and no row dimmed. */
  async function fresh(page, why) {
    await page.getByRole("group", { name: "Selection filter" }).waitFor({ state: "detached" })
    assert.deepEqual((await tableOf(page)).chips, [], `${why}: no chip`)
    assert.equal(await page.locator("tbody").getByRole("checkbox", { checked: true }).count(), 0, `${why}: no box ticked`)
    await page.getByText("0 in group", { exact: true }).waitFor()
    assert.ok(await saveButton(page).isDisabled(), `${why}: Save off`)
    for (const name of ["Paris", "Amsterdam", "Frankfurt"]) assert.equal(await dimmed(page, name), false, `${why}: ${name} not dimmed`)
  }

  test("17. a fresh draft dims no row; its first tick sets Selection is Selected, a change or a clear of that filter leaves every box as it was, and unticking the last box removes the chip a tick set", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.press("Enter")
      await fresh(page, "a fresh draft")
      await boxOf(page, "Paris").click()
      await boxOf(page, "Amsterdam").click()
      await page.getByText("2 in group", { exact: true }).waitFor()
      assert.equal(await page.getByRole("group", { name: "Selection filter" }).count(), 1, "the first tick set the chip")
      assert.ok(await dimmed(page, "Frankfurt"), "once a box is ticked, a row outside is dimmed")
      const both = async () => [await tickedOf(page, "Paris"), await tickedOf(page, "Amsterdam"), await tickedOf(page, "Frankfurt")]
      const box = await openFilter(page, "Selection")
      await box.getByRole("option", { name: /^Selected/ }).click()
      await box.getByRole("option", { name: /^Not selected/ }).click()
      await page.keyboard.press("Escape")
      await page.getByRole("button", { name: "Edit Selection filter, Selection is Not selected", exact: true }).waitFor()
      assert.deepEqual(await both(), [true, true, false], "a change of Selection leaves every box as it was")
      await page.getByRole("button", { name: "Remove Selection filter" }).click()
      await page.getByRole("group", { name: "Selection filter" }).waitFor({ state: "detached" })
      assert.deepEqual(await both(), [true, true, false], "its × leaves every box as it was")
      await page.getByText("0 in group", { exact: true }).waitFor()
      assert.ok(await saveButton(page).isDisabled(), "ticks with no condition hold no server")
      for (const name of ["Paris", "Frankfurt"]) assert.ok(await dimmed(page, name), `ticks with no condition: ${name} dimmed, as a stored group's are`)
      await boxOf(page, "Frankfurt").click()
      await until(async () => await tickedOf(page, "Frankfurt"), "Frankfurt ticked")
      assert.equal(await page.getByRole("group", { name: "Selection filter" }).count(), 0, "a later tick sets no chip the owner cleared")
      for (const name of ["Paris", "Amsterdam", "Frankfurt"]) await boxOf(page, name).click()
      await fresh(page, "every box unticked")
      await boxOf(page, "Paris").click()
      await page.getByRole("group", { name: "Selection filter" }).waitFor()
      assert.ok(await saveButton(page).isEnabled(), "the next first tick sets the chip again")
      await boxOf(page, "Paris").click()
      await fresh(page, "the last box unticked")
    } finally {
      await close()
    }
  })

  test("17. on a draft, a Selection is Selected set in Filter stays when the last box is unticked, and its × keeps the ticks", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.press("Enter")
      await (await openFilter(page, "Selection")).getByRole("option", { name: /^Selected/ }).click()
      await page.keyboard.press("Escape")
      await boxOf(page, "Paris").click()
      await page.getByText("1 in group", { exact: true }).waitFor()
      await boxOf(page, "Paris").click()
      await page.getByText("0 in group", { exact: true }).waitFor()
      assert.equal(await page.getByRole("group", { name: "Selection filter" }).count(), 1, "the chip set in Filter stays")
      await boxOf(page, "Paris").click()
      await page.getByRole("button", { name: "Remove Selection filter" }).click()
      await page.getByRole("group", { name: "Selection filter" }).waitFor({ state: "detached" })
      assert.ok(await tickedOf(page, "Paris"), "its × keeps the tick")
    } finally {
      await close()
    }
  })

  test("17. at 390 px a draft's first tick, which sets the chip, leaves every row where it was", async () => {
    const { page, close } = await open("servers", { width: 390 })
    try {
      await page.getByRole("button", { name: "Menu" }).click()
      await draftGroup(page)
      await page.keyboard.press("Enter")
      const top = async () => (await rowOf(page, "Amsterdam").boundingBox()).y
      const before = await top()
      await boxOf(page, "Amsterdam").click()
      await page.getByRole("group", { name: "Selection filter" }).waitFor()
      assert.equal(await top(), before, "the row under the finger stays")
    } finally {
      await close()
    }
  })

  test("17. a draft's rule turns Save on with how many servers it holds now, none included, and saves with no tick", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.press("Enter")
      await page.getByText("0 in group", { exact: true }).waitFor()
      assert.ok(await saveButton(page).isDisabled(), "no rule yet")
      await openFilter(page, "Name")
      await textChip(page, "Name matches").fill("nowhere")
      await page.keyboard.press("Escape")
      await until(async () => await saveButton(page).isEnabled(), "Save on for a rule")
      assert.equal(await page.getByText("0 in group", { exact: true }).count(), 1, "a rule that holds none today says so, and can still be saved")
      await page.getByRole("button", { name: /^Remove Name/ }).click()
      await (await openFilter(page, "Country")).getByRole("option", { name: /Netherlands/ }).click()
      await page.keyboard.press("Escape")
      await page.getByText("1 in group", { exact: true }).waitFor()
      await saveButton(page).click()
      await page.waitForURL(STORED_GROUP)
      const stored = daemon.group(new URL(page.url()).pathname.split("/").at(-1))
      assert.deepEqual([stored.name, stored.members.countries.values, stored.members.box, stored.ticked, stored.resolved], ["New group", ["NL"], null, [], ["l-ams"]])
    } finally {
      await close()
    }
  })

  test("17. Discard on a draft puts its filters and ticks back as it opened and stays on it with the name typed; leaving then asks, and the question's Discard leaves", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.type("Fast EU")
      await page.keyboard.press("Enter")
      assert.equal(await discardOf(page).count(), 0, "a draft with only its name has nothing for Discard to put back")
      await (await openFilter(page, "Country")).getByRole("option", { name: /Netherlands/ }).click()
      await page.keyboard.press("Escape")
      await boxOf(page, "Paris").click()
      await boxOf(page, "Amsterdam").click()
      await until(async () => (await tickedOf(page, "Paris")) && (await tickedOf(page, "Amsterdam")), "two ticks")
      await discardOf(page).click()
      await page.waitForTimeout(300)
      assert.equal(new URL(page.url()).pathname, BASE + "groups/new", "Discard stays on the draft")
      await fresh(page, "Discard")
      assert.equal(await discardOf(page).count(), 0, "nothing left for Discard to put back")
      assert.equal(await page.getByRole("alertdialog").count(), 0, "Discard asks nothing")
      assert.equal(await page.getByRole("heading", { name: "Fast EU" }).count(), 1, "the name typed is kept")
      await nav(page).getByRole("link", { name: "Events" }).click()
      const ask = page.getByRole("alertdialog")
      await ask.getByRole("heading", { name: "Discard Fast EU?" }).waitFor()
      await ask.getByRole("button", { name: "Discard" }).click()
      await page.waitForURL(/\/events$/)
      assert.deepEqual(groupPuts(), [], "nothing is stored")
    } finally {
      await close()
    }
  })

  test("17. after Discard, a draft renamed back to the name it opened with leaves at once", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.type("Fast EU")
      await page.keyboard.press("Enter")
      await boxOf(page, "Paris").click()
      await until(() => tickedOf(page, "Paris"), "Paris ticked")
      await discardOf(page).click()
      await fresh(page, "Discard")
      await page.getByRole("button", { name: "Rename Fast EU" }).click()
      await page.getByLabel("Name of Fast EU").fill("New group")
      await page.keyboard.press("Enter")
      await page.getByRole("button", { name: "Rename New group" }).waitFor()
      await nav(page).getByRole("link", { name: "Events" }).click()
      await page.waitForURL(/\/events$/)
      assert.equal(await page.getByRole("alertdialog").count(), 0, "nothing typed, set or ticked: no question")
    } finally {
      await close()
    }
  })

  test("17. leaving an untouched draft goes at once; leaving a typed or ticked one asks first, Cancel keeps it and Discard drops it", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.press("Enter")
      await nav(page).getByRole("link", { name: "Events" }).click()
      await page.waitForURL(/\/events$/)
      assert.equal(await page.getByRole("alertdialog").count(), 0, "nothing typed or ticked: no question")
      for (const touch of [async () => { await page.keyboard.type("Mine"); await page.keyboard.press("Enter") }, () => boxOf(page, "Paris").click()]) {
        await draftGroup(page)
        await touch()
        await nav(page).getByRole("link", { name: "Events" }).click()
        const ask = page.getByRole("alertdialog")
        await ask.getByRole("heading", { name: /^Discard (Mine|New group)\?$/ }).waitFor()
        await ask.getByRole("button", { name: "Cancel" }).click()
        await ask.waitFor({ state: "detached" })
        assert.equal(new URL(page.url()).pathname, BASE + "groups/new", "Cancel stays")
        await nav(page).getByRole("link", { name: "Events" }).click()
        await ask.getByRole("button", { name: "Discard" }).click()
        await page.waitForURL(/\/events$/)
        await page.goBack()
        await page.waitForURL(/\/groups\/new$/)
        await page.locator("[data-slot=group-heading]").waitFor()
        assert.equal(await page.getByRole("heading", { name: "Mine" }).count(), 0, "the draft left with the page")
        assert.equal(await tickedOf(page, "Paris"), false, "the draft left with the page")
        await nav(page).getByRole("link", { name: "Events" }).click()
        await page.waitForURL(/\/events$/)
      }
      assert.deepEqual(groupPuts(), [])
    } finally {
      await close()
    }
  })

  test("17. nothing of a draft survives a reload", async () => {
    const { page, close } = await open("servers")
    try {
      await draftGroup(page)
      await page.keyboard.type("Mine")
      await page.keyboard.press("Enter")
      await boxOf(page, "Paris").click()
      await page.reload()
      await page.getByLabel("Name of New group").waitFor()
      assert.equal(await tickedOf(page, "Paris"), false)
      assert.ok(await saveButton(page).isDisabled())
      assert.deepEqual(groupPuts(), [])
    } finally {
      await close()
    }
  })

  /** The table's top edge and the left edge of each header in sight. */
  const tableEdges = (page) => page.getByRole("table", { name: "Servers" }).evaluate((t) => ({
    top: t.getBoundingClientRect().top, columns: [...t.querySelectorAll("thead th")].filter((th) => th.offsetWidth > 0).map((th) => th.getBoundingClientRect().left),
  }))

  test("17. a new group's table keeps its top edge and its columns' left edges through the first filter and the first box ticked, with no line above it, at 1280 px", async () => {
    daemon.addAcme()
    for (const width of [1280]) {
      for (const first of ["filter", "pick"]) {
        const { page, close } = await open("servers")
        try {
          await addMenu(page, "Group")
          await page.getByLabel("Name of New group").waitFor()
          await page.setViewportSize({ width, height: 900 })
          await rowOf(page, "Amsterdam").waitFor()
          assert.doesNotMatch(await page.locator("main").innerText(), /No members/, `${width} px: no line says the group is empty`)
          const before = await tableEdges(page)
          const filter = async () => {
            await openFilter(page, "Source")
            await page.getByRole("dialog").getByRole("option", { name: /Acme/ }).click()
            await page.keyboard.press("Escape")
            await page.getByRole("group", { name: "Source filter" }).waitFor()
            await page.waitForTimeout(300)
          }
          const pick = async () => {
            await boxOf(page, "Amsterdam").click()
            await until(async () => tickedOf(page, "Amsterdam"), "Amsterdam ticked")
          }
          for (const [what, step] of first === "filter" ? [["the first filter", filter], ["the first box ticked", pick]] : [["the first box ticked", pick], ["the first filter", filter]]) {
            await step()
            await page.waitForTimeout(200)
            assert.deepEqual(await tableEdges(page), before, `${width} px, ${first} first: the table stays put through ${what}`)
          }
        } finally {
          await close()
        }
      }
    }
  })

  test("17. a group's tab starts with a box per server and All servers and a source's tab with the dot, at 360 and 1280 px, and no filter is Picked", async () => {
    seed()
    for (const width of [360, 1280]) {
      for (const tab of ["servers", "groups/mine", "groups/all-aurora", "sources/aurora"]) {
        const { page, close } = await open(tab, { width })
        try {
          await rowOf(page, "Paris").waitFor()
          const group = tab.startsWith("groups/")
          assert.equal(await page.locator("tbody").getByRole("checkbox").count(), group ? 3 : 0, `${tab} at ${width} px: ${group ? "a box per server" : "no box"}`)
          const first = await page.locator("thead th").first().innerText()
          assert.equal(first.trim(), group ? "Selection" : "Status", `${tab} at ${width} px: the first column`)
          if (tab === "sources/aurora") continue
          await page.getByRole("button", { name: "Filter", exact: true }).click()
          const radios = await page.getByRole("dialog").locator('[aria-label="Filter"]').getByRole("radio").allInnerTexts()
          assert.deepEqual(radios.map((r) => r.trim()), group ? ["Selection", "Country", "Name", "Source", "Protocol"] : ["Status", "Country", "Name", "Source", "Protocol"], `${tab} at ${width} px: the filters, in the columns' order`)
        } finally {
          await close()
        }
      }
    }
  })

  test("17. a tick with no box condition saves at once and changes no lit row, the count among them, and no Save; Selected in the Selection header dims every unticked row and shows Save; after Save the members are the ticked servers the rule matches, and with Not selected the unticked ones; no click moves a row", async () => {
    daemon.set({ groups: new Map([["eu", groupView("eu", 1, { name: "Europe", members: rule([], ["DE", "NL"]) })]]) })
    const { page, close } = await open("groups/eu")
    try {
      await rowOf(page, "Amsterdam").waitFor()
      const looks = () => page.locator("tbody tr").evaluateAll((trs, outside) => Object.fromEntries(trs.map((tr) => {
        const box = tr.querySelector("[role=checkbox]")
        const [boxCell, last] = [tr.cells[0], tr.cells[tr.cells.length - 1]].map((td) => (td.className.includes(outside) ? "dim" : "lit"))
        return [box.ariaLabel.replace("Select ", ""), `${box.ariaChecked} ${boxCell}/${last}`]
      })), OUTSIDE)
      const order = async () => (await tableOf(page)).rows.map((r) => r.slice(2))
      const before = await order()
      assert.deepEqual(await looks(), { Frankfurt: "false lit/lit", Amsterdam: "false lit/lit", Paris: "false lit/dim" })
      await boxOf(page, "Frankfurt").click()
      await until(() => JSON.stringify(daemon.group("eu").ticked) === '["l-fra"]', "the tick stored by the daemon at once")
      await boxOf(page, "Paris").click()
      await until(() => JSON.stringify(daemon.group("eu").ticked) === '["l-fra","l-par"]', "a tick on a row the rule leaves out stored too")
      assert.deepEqual(await looks(), { Frankfurt: "true lit/lit", Amsterdam: "false lit/lit", Paris: "true lit/dim" }, "ticks change no lit row")
      assert.equal(await saveBar(page).count(), 0, "a tick shows no Save or Discard")
      assert.equal((await tableOf(page)).count, "2 in group", "nor the count")
      assert.equal(daemon.group("eu").revision, 1, "a tick keeps the rule's revision")

      const dialog = await openFilter(page, "Selection")
      await dialog.getByRole("option", { name: /^Selected/ }).click()
      await page.keyboard.press("Escape")
      await until(async () => (await looks()).Amsterdam === "false lit/dim", "Selected dims the unticked row")
      assert.deepEqual(await looks(), { Frankfurt: "true lit/lit", Amsterdam: "false lit/dim", Paris: "true lit/dim" }, "the ticked row the rule leaves out stays dimmed")
      await saveBar(page).waitFor()
      assert.equal((await tableOf(page)).count, "1 in group")
      assert.deepEqual(await order(), before, "neither a tick nor an unsaved filter moves a row")
      await saveButton(page).click()
      await saveBar(page).waitFor({ state: "hidden" })
      assert.deepEqual([daemon.group("eu").members.box, daemon.group("eu").resolved], ["ticked", ["l-fra"]], "the ticked servers the other conditions match")

      const box = await openFilter(page, "Selection")
      await box.getByRole("option", { name: /^Selected/ }).click()
      await box.getByRole("option", { name: /^Not selected/ }).click()
      await page.keyboard.press("Escape")
      await until(async () => (await looks()).Frankfurt === "true lit/dim" && (await looks()).Amsterdam === "false lit/lit", "Not selected lights the unticked row")
      await saveButton(page).click()
      await saveBar(page).waitFor({ state: "hidden" })
      assert.deepEqual([daemon.group("eu").members.box, daemon.group("eu").resolved], ["unticked", ["l-ams"]], "the unticked ones")
      await until(async () => (await order())[0] === "Amsterdam", "a saved rule places its members first")
      const saved = await order()

      await boxOf(page, "Amsterdam").click()
      await until(async () => (await looks()).Amsterdam === "true lit/dim", "under a box condition a tick changes who is in at once")
      assert.equal(await saveBar(page).count(), 0, "still with no Save")
      assert.deepEqual(daemon.group("eu").resolved, [], "and the daemon's members follow")
      await page.waitForTimeout(300)
      assert.deepEqual(await order(), saved, "a tick that changes who is in moves no row")
    } finally {
      await close()
    }
  })

  test("17. a tick stays across a reload and a refresh of its source, a new server comes in unticked, and an untick saves at once", async () => {
    daemon.set({ groups: new Map([["all", groupView("all", 1, { name: "All Aurora", members: rule(["aurora"], [], "unticked"), ticked: ["l-fra"] })]]) })
    const { page, close } = await open("groups/all")
    try {
      await rowOf(page, "Frankfurt").waitFor()
      await until(async () => (await tickedOf(page, "Frankfurt")) && (await dimmed(page, "Frankfurt")), "the stored tick, and Not selected leaves it out")
      await page.reload()
      await rowOf(page, "Frankfurt").waitFor()
      await until(async () => (await tickedOf(page, "Frankfurt")) && (await dimmed(page, "Frankfurt")), "the same after a reload")
      daemon.refreshDocument("aurora", { addServer: server("l-ber", "Berlin", "DE", 12) })
      await rowOf(page, "Berlin").waitFor()
      await until(async () => !(await tickedOf(page, "Berlin")) && !(await dimmed(page, "Berlin")), "the refresh's new server is unticked, so in")
      assert.ok(await tickedOf(page, "Frankfurt"), "and the ticked one stays ticked")
      await boxOf(page, "Frankfurt").click()
      await until(async () => !(await tickedOf(page, "Frankfurt")) && !(await dimmed(page, "Frankfurt")), "an untick puts it back")
      await until(() => daemon.group("all").ticked.length === 0, "stored at once")
      assert.equal(await saveBar(page).count(), 0, "with no Save")
    } finally {
      await close()
    }
  })

  /** Each tick request the daemon has had for `server` in `group`: what it asked, and its Idempotency-Key. */
  const tickRequests = (group, server) => requests("PUT", `api/groups/${group}/ticked/${server}`).map((r) => ({ ticked: JSON.parse(r.body).ticked, key: r.idempotencyKey, status: r.status }))
  const boxOpacity = (page, name) => boxOf(page, name).evaluate((el) => getComputedStyle(el).opacity)
  /** A box as it looks: ticked, closed to clicks, busy, and its strength. */
  const boxLooks = (page, name) => boxOf(page, name).evaluate((el) => [el.ariaChecked, el.ariaDisabled, el.ariaBusy, getComputedStyle(el).opacity].map(String).join(" "))
  const SETTLED_TICKED = "true null null 1"
  const SETTLED_UNTICKED = "false null null 1"
  const BUSY_TICKED = "true true true 0.5"
  const tickedGroup = () => daemon.set({ groups: new Map([["all", groupView("all", 1, { name: "All Aurora", members: rule(["aurora"], [], "ticked") })]]) })
  /** A route that holds `server`'s tick requests in `all` while `hold(n)` says so for the nth, and records each one's key. */
  const holdTicks = async (page, server, hold) => {
    let release
    const held = new Promise((r) => { release = r })
    const keys = []
    await page.route(`**/api/groups/all/ticked/${server}`, async (r) => {
      keys.push(r.request().headers()["idempotency-key"])
      await hold(keys.length, r, held)
    })
    return { keys, release }
  }
  const failReads = (page) => page.route("**/api/state", (r) => r.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ kind: "unavailable", reason: "unavailable", message: "State busy." }) }))

  test("17. a tick the daemon stored stays ticked and lit, busy at half strength, while every read of the state after it fails, and settles when one lands", async () => {
    tickedGroup()
    const { page, close } = await open("groups/all", { clock: true })
    try {
      await rowOf(page, "Amsterdam").waitFor()
      await until(async () => dimmed(page, "Amsterdam"), "Selection is Selected leaves the unticked server out")
      let failed = 0
      await page.route("**/api/state", (r) => { failed++; return r.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ kind: "unavailable", reason: "unavailable", message: "State busy." }) }) })
      await boxOf(page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.includes("l-ams"), "the tick stored")
      await until(() => failed > 0, "a read of the state after it, failing")
      await page.waitForTimeout(1500)
      assert.equal(await boxLooks(page, "Amsterdam"), BUSY_TICKED, "the click shown, busy, since no read has confirmed it")
      assert.ok(!(await dimmed(page, "Amsterdam")), "and so in")
      assert.equal(await toast(page, "").count(), 0, "with nothing to say")
      await page.unroute("**/api/state")
      await page.clock.fastForward(10_000)
      await until(async () => (await boxLooks(page, "Amsterdam")) === SETTLED_TICKED, "a read that lands settles the box on what it says")
      assert.ok(!(await dimmed(page, "Amsterdam")))
    } finally {
      await close()
    }
  })

  test("17. a click on a busy box sends nothing: three quick clicks with the first unanswered send one tick, and the box shows it until it settles", async () => {
    tickedGroup()
    const { page, close } = await open("groups/all")
    try {
      await rowOf(page, "Amsterdam").waitFor()
      const { release } = await holdTicks(page, "l-ams", async (n, r, held) => { if (n === 1) await held; await r.continue() })
      await boxOf(page, "Amsterdam").click()
      for (let i = 0; i < 2; i++) await boxOf(page, "Amsterdam").click({ force: true })
      assert.equal(await boxLooks(page, "Amsterdam"), "true true null 1", "the first click shown, closed to clicks")
      release()
      await until(async () => (await boxLooks(page, "Amsterdam")) === SETTLED_TICKED, "the answer and the read after it settle the box")
      assert.deepEqual(tickRequests("all", "l-ams").map((r) => r.ticked), [true], "the clicks on the busy box sent nothing")
      await boxOf(page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.length === 0, "a click on the settled box unticks")
      assert.deepEqual(tickRequests("all", "l-ams").map((r) => r.ticked), [true, false])
      await until(async () => (await boxLooks(page, "Amsterdam")) === SETTLED_UNTICKED, "and settles")
    } finally {
      await close()
    }
  })

  test("17. a click on a box busy in one tab sends nothing, and the box settles on what the daemon holds after another tab's untick", async () => {
    tickedGroup()
    const a = await open("groups/all")
    const b = await open("groups/all")
    try {
      await rowOf(a.page, "Amsterdam").waitFor()
      await rowOf(b.page, "Amsterdam").waitFor()
      const { release } = await holdTicks(a.page, "l-ams", async (n, r, held) => { const response = await r.fetch(); await held; await r.fulfill({ response }).catch(() => {}) })
      await boxOf(a.page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.includes("l-ams"), "A's tick stored, its answer held")
      await boxOf(a.page, "Amsterdam").click({ force: true })
      assert.ok(await tickedOf(a.page, "Amsterdam"), "A's untick on its busy box is not taken")
      await until(async () => tickedOf(b.page, "Amsterdam"), "B sees the tick")
      await boxOf(b.page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.length === 0, "B's untick stored")
      await boxOf(a.page, "Amsterdam").click({ force: true })
      assert.ok(await tickedOf(a.page, "Amsterdam"), "A's busy box took neither click")
      /* Ticked and closed to clicks, however long the steps took: past a second it is also busy, as the next test has it. */
      assert.equal((await boxLooks(a.page, "Amsterdam")).split(" ").slice(0, 2).join(" "), "true true", "A shows its click, closed to clicks")
      release()
      await until(async () => (await boxLooks(a.page, "Amsterdam")) === SETTLED_UNTICKED, "A's box settles on what the daemon holds")
      /* Past a second unanswered A's tick goes again under its key, which the daemon answers from its record and doesn't apply. */
      const applied = requests("PUT", "api/groups/all/ticked/l-ams").filter((r) => !r.replayed).map((r) => JSON.parse(r.body).ticked)
      assert.deepEqual(applied, [true, false], "A's tick and B's untick, and nothing more")
      await boxOf(a.page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.includes("l-ams"), "a click on A's settled box ticks")
    } finally {
      await a.close()
      await b.close()
    }
  })

  test("17. a tick answered again under its key after another tab unticked stays busy at half strength while reads fail, and settles on the untick", async () => {
    tickedGroup()
    const a = await open("groups/all", { clock: true })
    const b = await open("groups/all")
    try {
      await rowOf(a.page, "Amsterdam").waitFor()
      await rowOf(b.page, "Amsterdam").waitFor()
      let unticked
      const bUnticked = new Promise((r) => { unticked = r })
      const { keys } = await holdTicks(a.page, "l-ams", async (n, r, held) => {
        if (n > 1) {
          await bUnticked
          return r.continue().catch(() => {})
        }
        const response = await r.fetch()
        await held
        await r.fulfill({ response }).catch(() => {})
      })
      await failReads(a.page)
      await boxOf(a.page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.includes("l-ams"), "A's tick stored, its answer withheld")
      await until(async () => tickedOf(b.page, "Amsterdam"), "B sees the tick")
      await boxOf(b.page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.length === 0, "B's untick stored")
      unticked()
      await until(() => tickRequests("all", "l-ams").some((r, i) => i > 0 && r.status === 200 && r.ticked), "A's tick sent again, and answered with the first answer")
      await a.page.waitForTimeout(1500)
      assert.ok(keys.length >= 2 && keys.every((key) => key === keys[0]), "every attempt under its key")
      assert.deepEqual(daemon.group("all").ticked, [], "the daemon holds B's untick")
      assert.equal(await boxLooks(a.page, "Amsterdam"), BUSY_TICKED, "A shows its click as unconfirmed")
      assert.equal(await toast(a.page, "").count(), 0, "with nothing to say")
      await a.page.unroute("**/api/state")
      await a.page.clock.fastForward(10_000)
      await until(async () => (await boxLooks(a.page, "Amsterdam")) === SETTLED_UNTICKED, "the read that lands settles A's box on the untick")
    } finally {
      await a.close()
      await b.close()
    }
  })

  test("17. a box still busy when its tab is left is busy and closed to clicks when the tab is come back to, and its one writer settles it", async () => {
    tickedGroup()
    const { page, close } = await open("groups/all", { clock: true })
    try {
      await rowOf(page, "Amsterdam").waitFor()
      const { keys, release } = await holdTicks(page, "l-ams", async (n, r, held) => { if (n <= 2) await held; await r.continue().catch(() => {}) })
      await boxOf(page, "Amsterdam").click()
      await until(async () => (await boxOf(page, "Amsterdam").getAttribute("aria-busy")) === "true", "busy a second")
      await nav(page).getByRole("link", { name: /All servers/ }).click()
      await boxOf(page, "Amsterdam").waitFor({ state: "hidden" })
      await nav(page).getByRole("link", { name: /All Aurora/ }).click()
      await rowOf(page, "Amsterdam").waitFor()
      assert.equal(await boxOf(page, "Amsterdam").getAttribute("aria-busy"), "true", "still busy on the way back")
      assert.equal(await boxLooks(page, "Amsterdam"), BUSY_TICKED)
      await boxOf(page, "Amsterdam").click({ force: true })
      await boxOf(page, "Amsterdam").click({ force: true })
      assert.deepEqual([...new Set(keys)], [keys[0]], "the clicks on it sent nothing new: only its own resends, under its key")
      release()
      await until(async () => (await boxLooks(page, "Amsterdam")) === SETTLED_TICKED, "its one writer's tick settles it")
      const ticks = keys.length
      assert.deepEqual([...new Set(keys)], [keys[0]], "under its key")
      await boxOf(page, "Amsterdam").click()
      await until(async () => (await boxLooks(page, "Amsterdam")) === SETTLED_UNTICKED, "an untick settles")
      await page.clock.fastForward(30_000)
      await page.waitForTimeout(500)
      assert.deepEqual([daemon.group("all").ticked, keys.length], [[], ticks + 1], "and no old writer reverses it")
    } finally {
      await close()
    }
  })

  test("17. a tick the daemon hasn't answered keeps no other box waiting, and after a second its box stands at half strength and busy until the answer", async () => {
    tickedGroup()
    const { page, close } = await open("groups/all")
    try {
      await rowOf(page, "Amsterdam").waitFor()
      let release
      const held = new Promise((r) => { release = r })
      await page.route("**/api/groups/all/ticked/l-ams", async (r) => { await held; await r.continue().catch(() => {}) })
      await boxOf(page, "Amsterdam").click()
      await boxOf(page, "Paris").click()
      await until(() => daemon.group("all").ticked.includes("l-par"), "Paris stored while Amsterdam waits")
      await until(async () => (await boxOf(page, "Amsterdam").getAttribute("aria-busy")) === "true", "a second unanswered marks the box busy")
      assert.deepEqual([await tickedOf(page, "Amsterdam"), await boxOpacity(page, "Amsterdam")], [true, "0.5"], "the click it waits on, at half strength")
      assert.deepEqual([await boxOf(page, "Paris").getAttribute("aria-busy"), await boxOpacity(page, "Paris")], [null, "1"], "the stored one at full strength")
      assert.equal(await toast(page, "").count(), 0, "waiting says nothing")
      release()
      await until(async () => (await boxOf(page, "Amsterdam").getAttribute("aria-busy")) === null, "the answer ends the wait")
      assert.deepEqual([daemon.group("all").ticked, await boxOpacity(page, "Amsterdam")], [["l-ams", "l-par"], "1"])
    } finally {
      await close()
    }
  })

  test("17. a tick answered only after the tab's rule edit was saved keeps both the tick and the rule", async () => {
    daemon.set({ groups: new Map([["all", groupView("all", 1, { name: "All Aurora", members: rule(["aurora"]) })]]) })
    const { page, close } = await open("groups/all")
    try {
      await rowOf(page, "Amsterdam").waitFor()
      await (await openFilter(page, "Selection")).getByRole("option", { name: /^Selected/ }).click()
      await page.keyboard.press("Escape")
      await saveBar(page).waitFor()
      let release
      const held = new Promise((r) => { release = r })
      let answered = false
      await page.route("**/api/groups/all/ticked/l-ams", async (r) => { const response = await r.fetch(); await held; await r.fulfill({ response }); answered = true })
      await boxOf(page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.includes("l-ams"), "the daemon stored the tick, its answer held")
      await saveButton(page).click()
      await saveBar(page).waitFor({ state: "hidden" })
      release()
      await until(() => answered, "the tick's answer, older than the rule")
      await page.waitForTimeout(500)
      const g = daemon.group("all")
      assert.deepEqual([g.revision, g.members.box, g.ticked, g.resolved], [2, "ticked", ["l-ams"], ["l-ams"]], "the daemon holds both")
      assert.equal(await saveBar(page).count(), 0, "the page still shows the rule saved")
      assert.equal((await tableOf(page)).chips[0], "Selection is ☑")
      assert.ok((await tickedOf(page, "Amsterdam")) && !(await dimmed(page, "Amsterdam")) && (await dimmed(page, "Paris")), "and the tick under it")
      assert.equal(await page.getByRole("alert").count(), 0, "with no conflict")
    } finally {
      await close()
    }
  })

  test("17. a tick unanswered for a second goes again under its key, and the answer settles the box with no toast", async () => {
    tickedGroup()
    const { page, close } = await open("groups/all")
    try {
      await rowOf(page, "Amsterdam").waitFor()
      let release
      const held = new Promise((r) => { release = r })
      let seen = 0
      await page.route("**/api/groups/all/ticked/l-ams", async (r) => { if (seen++ === 0) await held; await r.continue().catch(() => {}) })
      await boxOf(page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.includes("l-ams"), "the tick sent again a second later, and stored")
      await until(async () => (await boxOf(page, "Amsterdam").getAttribute("aria-disabled")) === null, "its answer ends the wait")
      release()
      await page.waitForTimeout(500)
      const sent = tickRequests("all", "l-ams")
      assert.ok(sent.length >= 1 && sent.every((r) => r.key === sent[0].key && r.ticked), "every request is the one tick, under one key")
      assert.equal(seen, 2, "sent twice from the page")
      assert.ok((await tickedOf(page, "Amsterdam")) && !(await dimmed(page, "Amsterdam")), "the box settles ticked and in")
      assert.equal(await toast(page, "").count(), 0, "with no toast for the request abandoned")
    } finally {
      await close()
    }
  })

  test("17. a tick answered with a 503, or whose connection drops, goes again under its key and settles with no toast", async () => {
    tickedGroup()
    const { page, close } = await open("groups/all")
    try {
      await rowOf(page, "Amsterdam").waitFor()
      let go
      const later = new Promise((r) => { go = r })
      const unavailable = await holdTicks(page, "l-ams", async (n, r) => n === 1
        ? r.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ kind: "unavailable", reason: "unavailable", message: "Daemon busy." }) })
        : later.then(() => r.continue().catch(() => {})))
      const dropped = await holdTicks(page, "l-par", async (n, r) => (n === 1 ? r.abort("connectionreset") : later.then(() => r.continue().catch(() => {}))))
      await boxOf(page, "Amsterdam").click()
      await boxOf(page, "Paris").click()
      await until(async () => (await boxLooks(page, "Amsterdam")) === BUSY_TICKED && (await boxLooks(page, "Paris")) === BUSY_TICKED, "both busy after their failures")
      assert.deepEqual(daemon.group("all").ticked, [], "nothing stored yet")
      go()
      await until(async () => (await boxLooks(page, "Amsterdam")) === SETTLED_TICKED && (await boxLooks(page, "Paris")) === SETTLED_TICKED, "both sent again, stored and settled")
      assert.deepEqual(daemon.group("all").ticked, ["l-ams", "l-par"])
      for (const { keys } of [unavailable, dropped]) assert.ok(keys.length >= 2 && keys.every((key) => key === keys[0]), "each sent again under its key")
      assert.equal(await toast(page, "").count(), 0, "with no toast")
    } finally {
      await close()
    }
  })

  test("17. a read of the state that never ends after a tick's yes keeps the box busy only until a later read lands", async () => {
    tickedGroup()
    const { page, close } = await open("groups/all", { clock: true })
    try {
      await rowOf(page, "Amsterdam").waitFor()
      let hanging = true
      let hung = 0
      await page.route("**/api/state", (r) => {
        if (!hanging) return r.continue().catch(() => {})
        hung++
        return new Promise(() => {})
      })
      await boxOf(page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.includes("l-ams"), "the tick stored and answered")
      await until(() => hung > 0, "a read of the state after it, which never ends")
      await page.waitForTimeout(1500)
      assert.equal(await boxLooks(page, "Amsterdam"), BUSY_TICKED, "busy while no read has brought the tick")
      hanging = false
      await page.clock.fastForward(10_000)
      await until(async () => (await boxLooks(page, "Amsterdam")) === SETTLED_TICKED, "the hung read fails at 10 s, a read after it lands, and the box settles")
      assert.equal(await toast(page, "").count(), 0, "with nothing to say")
    } finally {
      await close()
    }
  })

  test("17. a tick resent and refused at UniFi's login after its first attempt landed stays on what was sent while every read is refused, says session expired with Sign in, and settles on the daemon's tick", async () => {
    tickedGroup()
    const { page, close } = await open("groups/all", { clock: true })
    try {
      await rowOf(page, "Amsterdam").waitFor()
      const { keys } = await holdTicks(page, "l-ams", async (n, r) => {
        if (n > 1) return r.fulfill({ status: 401, contentType: "text/html", body: "<html>Unauthorized</html>" })
        await r.fetch()
        await r.abort("connectionreset")
      })
      await page.route("**/api/state", (r) => r.fulfill({ status: 401, contentType: "text/html", body: "<html>Unauthorized</html>" }))
      await boxOf(page, "Amsterdam").click()
      await until(() => daemon.group("all").ticked.includes("l-ams"), "the first attempt stored the tick, its answer lost")
      const said = toast(page, "Amsterdam")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Amsterdam session expired Sign in", "UniFi refused the resend")
      assert.ok(keys.length >= 2 && keys.every((key) => key === keys[0]), "the resend went under its key")
      await page.waitForTimeout(500)
      assert.equal(await boxLooks(page, "Amsterdam"), BUSY_TICKED, "the box keeps what was sent, which the daemon holds, while every read is refused")
      await page.unroute("**/api/state")
      await page.clock.fastForward(10_000)
      await until(async () => (await boxLooks(page, "Amsterdam")) === SETTLED_TICKED, "a read that lands settles it on the daemon's tick")
      await page.route("**/login?**", (r) => r.fulfill({ status: 200, contentType: "text/html", body: "<title>UniFi</title>" }))
      await said.getByRole("button", { name: "Sign in" }).click()
      await page.waitForURL("**/login?**")
      const back = new URL(page.url())
      assert.deepEqual([back.pathname, back.searchParams.get("redirect")], ["/login", new URL(daemon.url).pathname + "groups/all"], "Sign in opens UniFi's login, which comes back here")
    } finally {
      await close()
    }
  })

  test("17. going offline after a tick's yes leaves the box busy on what was sent, and it settles once the page is back online and a read lands", async () => {
    tickedGroup()
    const { page, context, close } = await open("groups/all", { clock: true })
    try {
      await rowOf(page, "Amsterdam").waitFor()
      let hanging = true
      await page.route("**/api/state", (r) => (hanging ? new Promise(() => {}) : r.continue().catch(() => {})))
      await boxOf(page, "Amsterdam").click()
      await until(() => tickRequests("all", "l-ams").some((r) => r.status === 200), "the tick answered yes")
      await context.setOffline(true)
      hanging = false
      await page.clock.fastForward(10_000)
      await page.waitForTimeout(1500)
      assert.equal(await boxLooks(page, "Amsterdam"), BUSY_TICKED, "offline, the box isn't settled on the view from before the tick")
      await context.setOffline(false)
      await page.clock.fastForward(10_000)
      await until(async () => (await boxLooks(page, "Amsterdam")) === SETTLED_TICKED, "back online, a read lands and settles it")
    } finally {
      await close()
    }
  })

  test("17. stopping the daemon marks the top bar within 10 s with since when, dims what the page shows without moving it, and the mark goes once the reads after its start land", async () => {
    const { page, close } = await open("")
    try {
      await tile(page, "EU").waitFor()
      const mark = page.getByRole("button", { name: /^Gateway unreachable/ })
      const looks = (selectors) => page.evaluate((selectors) => selectors.map((selector) => {
        const el = document.querySelector(selector)
        const r = el.getBoundingClientRect()
        let opacity = 1
        for (let n = el; n; n = n.parentElement) opacity *= Number(getComputedStyle(n).opacity)
        return { box: [r.x, r.y, r.width, r.height].join(), opacity }
      }), selectors)
      const SHOWN = ['header button[aria-label="WAN up"]', "nav[aria-label=Main]", "[data-slot=main] > div > div"]
      const before = await looks(SHOWN)
      assert.equal(await mark.count(), 0, "no mark while connected")
      assert.deepEqual(before.map((l) => l.opacity), [1, 1, 1], "nothing dimmed while connected")
      daemon.stop()
      const clock = () => new Date().toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" })
      const early = clock()
      await mark.waitFor({ timeout: 10_000 })
      const said = await mark.getAttribute("aria-label")
      assert.ok([early, clock()].map((t) => `Gateway unreachable since ${t}`).includes(said), said)
      await mark.hover()
      await until(async () => (await card(page).innerText().catch(() => "")).replace(/\s+/g, " ") === `Offline since ${said.split("since ")[1]}`, "its card, the page's Card, names since when")
      await page.mouse.move(0, 0)
      await until(async () => (await looks(SHOWN)).every((l) => l.opacity === 0.5), "the WAN globe, the list and the page dimmed")
      assert.deepEqual((await looks(SHOWN)).map((l) => l.box), before.map((l) => l.box), "nothing moved")
      assert.equal((await looks(['header button[aria-label^="Gateway unreachable"]']))[0].opacity, 1, "the mark itself at full strength")

      daemon.set({ readDelay: { "api/state": 1500 } })
      const restarted = daemon.log().length
      const stateRead = () => daemon.log().slice(restarted).find((r) => r.path === BASE + "api/state" && r.status !== 502)
      daemon.start()
      await until(stateRead, "the reads after the start asked", 20_000)
      daemon.stop()
      await until(() => stateRead().status === 200, "the read asked before the second stop landed")
      await page.waitForTimeout(300)
      assert.equal(await mark.count(), 1, "a read landing after a second drop leaves the mark")
      assert.equal(await mark.getAttribute("aria-label"), said, "a read landing after a second drop leaves the page disconnected since the first")

      daemon.start()
      await until(() => daemon.sseClients() > 0, "the stream back again", 20_000)
      assert.equal(await mark.count(), 1, "still marked while the reads after the start are out")
      await mark.waitFor({ state: "detached", timeout: 5000 })
      assert.deepEqual((await looks(SHOWN)).map((l) => l.opacity), [1, 1, 1], "nothing dimmed once connected")
    } finally {
      await close()
    }
  })

  test("17. with the stream back and every read answering 502, the mark stays with its first time, and the next state read that lands clears it", async () => {
    const { page, close } = await open("")
    try {
      await tile(page, "EU").waitFor()
      const mark = page.getByRole("button", { name: /^Gateway unreachable/ })
      daemon.stop()
      await mark.waitFor({ timeout: 10_000 })
      const said = await mark.getAttribute("aria-label")

      const READS = ["api/session", "api/state", "api/servers", "api/switches", "api/replaced", "api/settings", "api/update"]
      daemon.set({ readFails: Object.fromEntries(READS.map((path) => [path, Infinity])) })
      const restarted = daemon.log().length
      const failedStateReads = () => daemon.log().slice(restarted).filter((r) => r.path === BASE + "api/state" && r.status === 502).length
      daemon.start()
      await until(() => daemon.sseClients() > 0, "the stream back", 20_000)
      await until(() => failedStateReads() >= 1, "the state read after the start answered 502, with no retry of its own")
      await page.waitForTimeout(500)
      assert.equal(await mark.count(), 1, "a failed state read leaves the mark")
      assert.equal(await mark.getAttribute("aria-label"), said, "a failed state read leaves the page disconnected since the first")

      daemon.set({ readFails: {} })
      daemon.renameSlot("eu", "EU")
      await mark.waitFor({ state: "detached", timeout: 5000 })
    } finally {
      await close()
    }
  })

  test("17. a first state read asked before the stream dropped and answered after it is back clears no mark: the page reads the state again and shows the change made meanwhile", async () => {
    const { page, close } = await open("")
    try {
      await tile(page, "EU").waitFor()
      let release
      const held = new Promise((r) => { release = r })
      let first = true
      await page.route("**/api/state", async (route) => {
        if (!first) return route.continue().catch(() => {})
        first = false
        const response = await route.fetch()
        await held
        await route.fulfill({ response }).catch(() => {})
      })
      const asked = daemon.log().length
      const stateReads = () => daemon.log().slice(asked).filter((r) => r.method === "GET" && r.path === BASE + "api/state")
      await page.reload({ waitUntil: "commit" })
      await until(() => stateReads().length === 1 && daemon.sseClients() > 0, "the first state read answered by the daemon, held, and the stream open")
      const mark = page.getByRole("button", { name: /^Gateway unreachable/ })
      daemon.set({ eventsFail: 1 })
      daemon.dropEvents()
      await mark.waitFor({ timeout: 10_000 })
      daemon.renameSlot("eu", "Europe")
      await until(() => daemon.sseClients() > 0, "the stream back", 20_000)
      release()
      await tile(page, "Europe").waitFor()
      assert.ok(stateReads().length >= 2, "the state read again after the stream came back")
      await mark.waitFor({ state: "detached", timeout: 5000 })
    } finally {
      await close()
    }
  })

  test("17. until the daemon answers what a group's rule matches, its saved members stand in, and a tick still saves at once", async () => {
    daemon.set({ groups: new Map([["one", groupView("one", 1, { name: "Paris", members: rule(["aurora"], [], "ticked"), ticked: ["l-par"] })]]) })
    const { page, close } = await open("")
    try {
      let release
      const held = new Promise((r) => { release = r })
      await page.route("**/api/groups/preview", async (route) => { await held; await route.continue() })
      await page.goto(daemon.url + "groups/one")
      await rowOf(page, "Paris").waitFor()
      assert.deepEqual([await dimmed(page, "Paris"), await dimmed(page, "Frankfurt")], [false, true], "the saved members before the answer")
      await boxOf(page, "Frankfurt").click()
      await until(() => JSON.stringify(daemon.group("one").ticked) === '["l-fra","l-par"]', "the tick stored at once")
      assert.equal(await saveBar(page).count(), 0, "with no Save")
      release()
      await until(async () => !(await dimmed(page, "Frankfurt")) && (await dimmed(page, "Amsterdam")), "the answer lights the ticked server the rule matches")
      daemon.set({ groups: new Map([["de", groupView("de", 1, { name: "Germany", members: rule(["aurora"], ["DE"]) })]]) })
      let again
      const heldAgain = new Promise((r) => { again = r })
      await page.unroute("**/api/groups/preview")
      await page.route("**/api/groups/preview", async (route) => { await heldAgain; await route.continue() })
      await page.goto(daemon.url + "groups/de")
      await rowOf(page, "Paris").waitFor()
      assert.deepEqual([await dimmed(page, "Frankfurt"), await dimmed(page, "Paris")], [false, true], "with no box condition too, only the saved members before the answer")
      again()
    } finally {
      await close()
    }
  })

  test("17. the box column's filter picks as Country does, a stock checkbox per row with its words and count, offers Selected and Not selected with no search, sets the rule's box condition with one picked and none with both or neither, and its chip draws the box", async () => {
    daemon.set({ groups: new Map([["mixed", groupView("mixed", 1, { name: "Mixed", members: rule(["aurora"]), ticked: ["l-par"] })]]) })
    const { page, close } = await open("groups/mixed")
    try {
      await rowOf(page, "Paris").waitFor()
      /** Each option as the set filters draw one: its words, its count, the stock checkboxes in it and how many are ticked, and any other box or check drawn. */
      const optionsOf = (dialog) => dialog.getByRole("option").evaluateAll((os) => os.map((o) => [
        o.querySelector("[data-facet]").textContent, o.querySelector(":scope > .tabular-nums").textContent,
        o.querySelectorAll("[data-slot=checkbox]").length, o.querySelectorAll("[data-slot=checkbox][data-state=checked]").length,
        [...o.querySelectorAll(".size-4.border:not([data-slot=checkbox]), :scope > svg.lucide-check")].filter((e) => e.checkVisibility()).length,
      ]))
      const country = await openFilter(page, "Country")
      await country.getByRole("option", { name: /Germany/ }).click()
      assert.deepEqual((await optionsOf(country)).find((o) => o[0] === "Germany").slice(2), [1, 1, 0], "Country picks with one stock checkbox per row")
      await country.getByRole("option", { name: /Germany/ }).click()
      await page.keyboard.press("Escape")
      await country.waitFor({ state: "detached" })
      const dialog = await openFilter(page, "Selection")
      const options = () => optionsOf(dialog)
      assert.deepEqual(await options(), [["Selected", "1", 1, 0, 0], ["Not selected", "2", 1, 0, 0]], "the words alone, with its count and one stock checkbox, none picked")
      assert.equal(await dialog.locator("input").count(), 0, "no search for two values")
      assert.equal(await dialog.getByRole("radio", { name: "is none of" }).count(), 0, "two values need no is none of")
      await dialog.getByRole("option", { name: /^Not selected/ }).click()
      await until(async () => JSON.stringify((await options()).map((o) => o[3])) === "[0,1]", "the picked one ticked, as Country's are")
      await page.keyboard.press("Escape")
      await until(async () => JSON.stringify((await tableOf(page)).rows) === JSON.stringify(["+:Frankfurt", "+:Amsterdam", "-:Paris"]), "Not selected dims the ticked row and hides none")
      await saveBar(page).waitFor()
      assert.equal((await tableOf(page)).chips[0], "Selection is ☐", "its chip first, in the columns' order, its value the box alone")
      assert.equal(await page.getByRole("button", { name: "Edit Selection filter, Selection is Not selected", exact: true }).count(), 1, "the chip's name says its value in words")
      const again = await openFilter(page, "Selection")
      await again.getByRole("option", { name: /^Selected/ }).click()
      await until(async () => JSON.stringify((await optionsOf(again)).map((o) => o[3])) === "[1,1]", "both stay picked while the list is open")
      await until(async () => (await tableOf(page)).rows.every((r) => r.startsWith("+")), "both picked is no box condition")
      assert.deepEqual((await tableOf(page)).chips.filter((c) => c.startsWith("Selection")), [], "and no Selection chip")
      await again.getByRole("option", { name: /^Not selected/ }).click()
      await page.keyboard.press("Escape")
      await until(async () => (await tableOf(page)).chips[0] === "Selection is ☑", "the one left picked is the condition")
      await page.getByRole("button", { name: "Remove Selection filter" }).click()
      await until(async () => (await tableOf(page)).rows.every((r) => r.startsWith("+")), "every row lit again")
      await saveBar(page).waitFor({ state: "hidden" })
    } finally {
      await close()
    }
  })

  test("17. the filters' popover fits its tabs with every filter set, each dot inside its tab, so a dot adds no width, at 1280 and 390 px", async () => {
    const among = (values) => ({ not: false, values })
    daemon.set({ groups: new Map([
      ["all-set", groupView("all-set", 1, { name: "All set", members: { sources: among(["aurora"]), countries: among(["DE"]), protocols: among(["vless"]), include: "a", exclude: "", box: "ticked" }, ticked: ["l-fra"] })],
      ["none-set", groupView("none-set", 1, { name: "None set", members: { sources: among([]), countries: among([]), protocols: among([]), include: "", exclude: "", box: null } })],
    ]) })
    const { page, close } = await open("groups/all-set")
    /* The popover's opening zoom ends first, so every tab is measured at one scale. */
    const opened = (tabs) => tabs.evaluate((el) => Promise.all(el.closest("[data-slot=popover-content]").getAnimations({ subtree: true }).map((a) => a.finished)))
    const widths = async (group) => {
      await page.goto(daemon.url + `groups/${group}`)
      await rowOf(page, "Frankfurt").waitFor()
      await page.getByRole("button", { name: "Filter", exact: true }).click()
      const tabs = page.getByRole("dialog").locator('[aria-label="Filter"]')
      await opened(tabs)
      const shown = await tabs.evaluate((el) => [...el.children].map((c) => c.getBoundingClientRect().width))
      await page.keyboard.press("Escape")
      await page.getByRole("dialog").waitFor({ state: "detached" })
      return shown
    }
    try {
      const [dotted, plain] = [await widths("all-set"), await widths("none-set")]
      assert.ok(dotted.every((w, i) => Math.abs(w - plain[i]) <= 1), `each tab is as wide with its dot as without: ${dotted} and ${plain}`)
      for (const width of [1280, 390]) {
        await page.setViewportSize({ width, height: 844 })
        await page.goto(daemon.url + "groups/all-set")
        await rowOf(page, "Frankfurt").waitFor()
        await page.getByRole("button", { name: "Filter", exact: true }).click()
        const tabs = page.getByRole("dialog").locator('[aria-label="Filter"]')
        await opened(tabs)
        const fit = await tabs.evaluate((el) => {
          const box = el.closest("[data-slot=popover-content]").getBoundingClientRect()
          const style = getComputedStyle(el.closest("[data-slot=popover-content]"))
          const items = [...el.children].map((c) => c.getBoundingClientRect())
          return { dots: el.querySelectorAll("[aria-hidden].rounded-full").length, overflow: el.scrollWidth - el.clientWidth, right: Math.round(box.right - parseFloat(style.paddingRight) - items.at(-1).right) }
        })
        assert.equal(fit.dots, 5, `${width} px: every tab carries its dot`)
        assert.ok(fit.overflow <= 0 && fit.right >= 0, `${width} px: the tabs fit inside the popover: ${JSON.stringify(fit)}`)
        await page.keyboard.press("Escape")
        await page.getByRole("dialog").waitFor({ state: "detached" })
      }
    } finally {
      await close()
    }
  })

  test("17. one chip shows whole after Filter on All servers and a group's tab at 360, 768 and 1280 px, with nothing scrolled, what ends the line in reach and Name as wide", async () => {
    daemon.addAcme()
    const { page, close } = await open("servers")
    const chips = () => page.getByRole("group", { name: "Filters", exact: true })
    const nameWidth = async () => (await page.locator("thead th").filter({ hasText: "Name" }).boundingBox()).width
    const tabs = [
      { tab: "All servers", ends: [page.getByRole("button", { name: "Save as group", exact: true })] },
      /* A draft's Save and count stand from the start, as there is always something to save. */
      { tab: "a new group's draft", ends: [saveButton(page), page.getByText(/^\d+ in group$/)], open: async () => { await addMenu(page, "Group"); await page.getByLabel("Name of New group").waitFor() }, stays: true },
    ]
    try {
      await rowOf(page, "Amsterdam").waitFor()
      for (const { tab, ends, open, stays } of tabs) {
        await page.setViewportSize({ width: 1440, height: 900 })
        await open?.()
        for (const width of [360, 768, 1280]) {
          await page.setViewportSize({ width, height: 900 })
          await rowOf(page, "Amsterdam").waitFor()
          const name = await nameWidth()
          for (const [column, option] of [["Country", /Germany/], ["Source", /Acme/], ["Protocol", /VLESS/]]) {
            const at = `${column} on ${tab} at ${width} px`
            await openFilter(page, column)
            await page.getByRole("dialog").getByRole("option", { name: option }).click()
            await page.keyboard.press("Escape")
            await page.getByRole("dialog").waitFor({ state: "detached" })
            await ends[0].waitFor()
            const [chip, line, filter] = [await page.getByRole("group", { name: `${column} filter` }).boundingBox(), await chips().boundingBox(), await page.getByRole("button", { name: "Filter", exact: true }).boundingBox()]
            assert.equal(await chips().evaluate((el) => el.scrollLeft), 0, `nothing scrolled: ${at}`)
            assert.ok(chip.x >= line.x - 0.5 && chip.x + chip.width <= line.x + line.width + 0.5, `the chip shows whole: ${at}, ${chip.x}-${chip.x + chip.width} in ${line.x}-${line.x + line.width}`)
            assert.ok(filter.x >= 0 && filter.x + filter.width <= chip.x && Math.abs(filter.y - chip.y) < 1, `Filter starts the chip's row: ${at}`)
            for (const end of ends) {
              const box = await end.boundingBox()
              assert.ok(box.x >= 0 && box.x + box.width <= width, `${await end.innerText()} in reach: ${at}`)
            }
            assert.equal(await nameWidth(), name, `Name as wide as with no chip: ${at}`)
            await page.getByRole("button", { name: `Remove ${column} filter` }).click()
            if (stays) await page.getByRole("group", { name: `${column} filter` }).waitFor({ state: "detached" })
            else await ends[0].waitFor({ state: "detached" })
          }
        }
      }
    } finally {
      await close()
    }
  })

  test("17. with search open on a new group's unsaved Source at 320, 360 and 390 px, its count, Save and Discard keep apart and a tap at Save's or Discard's centre reaches it", async () => {
    daemon.addAcme()
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Group")
      await page.getByLabel("Name of New group").waitFor()
      await openFilter(page, "Source")
      await page.getByRole("dialog").getByRole("option", { name: /Acme/ }).click()
      await page.keyboard.press("Escape")
      await page.getByRole("dialog").waitFor({ state: "detached" })
      await page.getByRole("button", { name: "Search", exact: true }).click()
      await page.getByPlaceholder("Search").fill("proxy")
      const count = page.getByText(/^\d+ in group$/)
      for (const width of [320, 360, 390]) {
        await page.setViewportSize({ width, height: 900 })
        await saveButton(page).waitFor()
        const boxes = await page.evaluate(() => {
          const bar = document.querySelector('[aria-label="Unsaved changes"]')
          const at = (el) => { const r = el.getBoundingClientRect(); return { left: r.left, right: r.right, top: r.top, bottom: r.bottom } }
          const [discard, save] = [...bar.querySelectorAll("button")]
          const count = [...document.querySelectorAll("*")].find((el) => /^\d+ in group$/.test(el.textContent) && el.children.length === 0)
          const hit = (el) => { const r = el.getBoundingClientRect(); const top = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2); return top?.closest("button") === el }
          return { discard: at(discard), save: at(save), count: at(count), hits: { Discard: hit(discard), Save: hit(save) } }
        })
        const apart = (a, b) => a.right <= b.left || b.right <= a.left || a.bottom <= b.top || b.bottom <= a.top
        for (const name of ["Discard", "Save"]) {
          assert.ok(boxes.hits[name], `${width} px: ${name}'s centre reaches ${name}: ${JSON.stringify(boxes)}`)
          assert.ok(apart(boxes[name.toLowerCase()], boxes.count), `${width} px: the count keeps off ${name}: ${JSON.stringify(boxes)}`)
        }
        assert.ok(boxes.count.left >= 0 && boxes.count.right <= width, `${width} px: the count shows whole: ${JSON.stringify(boxes.count)}`)
        assert.ok(await count.isVisible(), `${width} px: the count shows`)
      }
    } finally {
      await close()
    }
  })

  /** The names of the rows a table shows, or on a group's tab those it holds. */
  const namesShown = (page, held = false) => page.locator("tbody tr").evaluateAll((trs, [held, outside]) => trs
    .filter((tr) => !held || !tr.cells[tr.cells.length - 1].className.includes(outside)).map((tr) => tr.querySelector("td button[type=button]:not([role=checkbox])").textContent).toSorted(), [held, OUTSIDE])
  const saveAs = (page) => page.getByRole("button", { name: "Save as group", exact: true })

  test("17. on All servers, Country is Germany then Save as group opens a new group to name, whose members are the rows that were shown", async () => {
    daemon.addAcme()
    const { page, close } = await open("servers")
    try {
      await row(page, "Amsterdam").waitFor()
      assert.equal(await saveAs(page).count(), 0, "nothing to save before a filter is set")
      await openFilter(page, "Country")
      await page.getByRole("dialog").getByRole("option", { name: /Germany/ }).click()
      await page.keyboard.press("Escape")
      await until(async () => (await page.locator("tbody tr").count()) === 3, "Germany's servers alone")
      const shown = await namesShown(page)
      const [last, end] = [await page.getByRole("group", { name: "Filters", exact: true }).boundingBox(), await saveAs(page).boundingBox()]
      assert.ok(end.x >= last.x + last.width, "Save as group ends the chip line")
      await saveAs(page).click()
      const name = page.getByLabel("Name of New group")
      await name.waitFor()
      assert.ok(await name.evaluate((el) => el === document.activeElement), "its name is ready to type")
      const id = new URL(page.url()).pathname.split("/").pop()
      const { members } = daemon.group(id)
      assert.deepEqual(members, { sources: { not: false, values: [] }, countries: { not: false, values: ["DE"] }, protocols: { not: false, values: [] }, include: "", exclude: "", box: null })
      await until(async () => JSON.stringify(await namesShown(page, true)) === JSON.stringify(shown), `the group holding ${shown}`)
    } finally {
      await close()
    }
  })

  test("17. Save as group carries Name, Source and Protocol and not Status, its rule matches a name as the filter did, without case, and a pattern Go refuses rings its chip, says why in its editor and makes no group", async () => {
    daemon.addAcme()
    const groupCount = async () => (await (await fetch(daemon.url + "api/state")).json()).groups.length
    const groups = await groupCount()
    const { page, close } = await open("servers")
    const choose = async (column, option) => {
      await openFilter(page, column)
      await page.getByRole("dialog").getByRole("option", { name: option }).click()
      await page.keyboard.press("Escape")
      await page.getByRole("dialog").waitFor({ state: "detached" })
    }
    const named = async (pattern) => {
      await openFilter(page, "Name")
      await textChip(page, "Name matches").fill(pattern)
      await page.keyboard.press("Escape")
      await page.getByRole("dialog").waitFor({ state: "detached" })
    }
    try {
      await row(page, "Amsterdam").waitFor()
      await choose("Status", /Online/)
      assert.equal(await saveAs(page).count(), 0, "no rule holds a status")
      await choose("Source", /Acme/)
      await choose("Protocol", /VLESS/)
      await named("(?=edge)")
      const chip = page.getByRole("button", { name: "Edit Name matches filter" })
      await until(async () => (await chip.getAttribute("aria-invalid")) === "true", "the refused chip ringed")
      assert.equal(await page.getByText("Lookahead not supported", { exact: true }).count(), 0, "no words on the line")
      assert.equal(await saveAs(page).count(), 0, "a refused pattern offers no Save as group")
      await chip.click()
      const editor = page.getByRole("dialog")
      assert.equal(await editor.locator("[data-slot=field-error]").innerText(), "Lookahead not supported", "the editor says why the pattern is refused")
      assert.equal(await textChip(page, "Name matches").getAttribute("aria-invalid"), "true")
      await page.keyboard.press("Escape")
      await editor.waitFor({ state: "detached" })
      assert.equal(await groupCount(), groups, "no group made")
      await page.getByRole("button", { name: "Remove Name matches filter" }).click()
      await named("edge")
      await until(async () => (await page.locator("tbody tr").count()) === 6, "the same six rows, though the pattern is lower case")
      const shown = await namesShown(page)
      await saveAs(page).click()
      await page.getByLabel("Name of New group").waitFor()
      const { members } = daemon.group(new URL(page.url()).pathname.split("/").pop())
      assert.deepEqual(members, { sources: { not: false, values: ["acme"] }, countries: { not: false, values: [] }, protocols: { not: false, values: ["vless"] }, include: "edge", exclude: "", box: null })
      await until(async () => JSON.stringify(await namesShown(page, true)) === JSON.stringify(shown), `the group holding ${shown}`)
    } finally {
      await close()
    }
  })

  test("17. All servers shows the rows the daemon's reading of a Name pattern matches, not JavaScript's, and Save as group holds those rows", async () => {
    daemon.addAcme()
    const dash = /\p{Pd}/u
    /* RE2 reads \p{Pd} as the dashes, which no case folds. JavaScript without the u flag reads it as the letters "p{Pd}". */
    daemon.set({ reads: (pattern, name) => (pattern === "\\p{Pd}" ? dash.test(name) : new RegExp(pattern, "i").test(name)) })
    const expected = daemon.catalogue().filter((l) => [l.label, ...l.seen.map((seen) => seen.tag)].some((name) => dash.test(name))).length
    assert.ok(expected > 0 && expected < daemon.catalogue().length, `a pattern that matches some rows: ${expected}`)
    const { page, close } = await open("servers")
    try {
      await row(page, "Amsterdam").waitFor()
      await openFilter(page, "Name")
      await textChip(page, "Name matches").fill("\\p{Pd}")
      await page.keyboard.press("Escape")
      await until(async () => (await page.locator("tbody tr").count()) === expected, `the ${expected} rows the daemon matched`)
      assert.equal(await row(page, "Amsterdam").count(), 0, "a row the daemon doesn't match is hidden")
      const shown = await namesShown(page)
      await saveAs(page).click()
      await page.getByLabel("Name of New group").waitFor()
      const { members } = daemon.group(new URL(page.url()).pathname.split("/").pop())
      assert.equal(members.include, "\\p{Pd}")
      await until(async () => JSON.stringify(await namesShown(page, true)) === JSON.stringify(shown), `the group holding ${shown}`)
    } finally {
      await close()
    }
  })

  test("17. a source refresh that adds a server the Name pattern matches asks the daemon again, and Save as group waits for that answer and holds the rows it then shows", async () => {
    daemon.addAcme()
    const { page, close } = await open("servers")
    try {
      await row(page, "Amsterdam").waitFor()
      await openFilter(page, "Name")
      await textChip(page, "Name matches").fill("edge")
      await page.keyboard.press("Escape")
      await until(async () => (await page.locator("tbody tr").count()) === 6 && (await saveAs(page).isEnabled()), "six rows and Save as group")
      const asked = requests("POST", "api/groups/preview").length
      let release
      const held = new Promise((r) => { release = r })
      await page.route("**/api/groups/preview", async (route) => { await held; await route.continue() })
      const [acmeServer] = daemon.catalogue().filter((l) => l.source === "acme")
      daemon.refreshDocument("acme", { addServer: { ...acmeServer, id: "lw-added", label: "EDGE Added" } })
      await until(async () => (await nav(page).getByRole("link", { name: /All servers/ }).innerText()).includes(String(daemon.catalogue().length)), "the new total")
      await until(async () => await saveAs(page).isDisabled(), "Save as group waits for the new answer")
      release()
      await until(async () => (await page.locator("tbody tr").count()) === 7 && (await saveAs(page).isEnabled()), "the added server shown, and Save as group again")
      assert.equal(requests("POST", "api/groups/preview").length, asked + 1, "the pattern asked once more")
      const shown = await namesShown(page)
      await saveAs(page).click()
      await page.getByLabel("Name of New group").waitFor()
      const { resolved } = daemon.group(new URL(page.url()).pathname.split("/").pop())
      assert.ok(resolved.includes("lw-added"), "the group holds the added server")
      await until(async () => JSON.stringify(await namesShown(page, true)) === JSON.stringify(shown), `the group holding ${shown}`)
    } finally {
      await close()
    }
  })

  test("17. a source refresh while the Name pattern's first answer is on its way asks again over the new list, and Save as group holds the rows it then shows", async () => {
    daemon.addAcme()
    const { page, close } = await open("servers")
    try {
      await row(page, "Amsterdam").waitFor()
      let release
      let computed = false
      const held = new Promise((r) => { release = r })
      await page.route("**/api/groups/preview", async (route) => {
        const response = await route.fetch()
        computed = true
        await held
        await route.fulfill({ response }).catch(() => {})
      }, { times: 1 })
      await openFilter(page, "Name")
      await textChip(page, "Name matches").fill("edge")
      await page.keyboard.press("Escape")
      await until(() => computed, "the daemon answered over the old list")
      const [acmeServer] = daemon.catalogue().filter((l) => l.source === "acme")
      daemon.refreshDocument("acme", { addServer: { ...acmeServer, id: "lw-added", label: "EDGE Added" } })
      await until(async () => (await nav(page).getByRole("link", { name: /All servers/ }).innerText()).includes(String(daemon.catalogue().length)), "the new total")
      release()
      await until(async () => await saveAs(page).isEnabled(), "Save as group")
      const shown = await namesShown(page)
      assert.equal(shown.length, 7, `the added server shown: ${shown}`)
      await saveAs(page).click()
      await page.getByLabel("Name of New group").waitFor()
      const { resolved } = daemon.group(new URL(page.url()).pathname.split("/").pop())
      assert.equal(resolved.length, shown.length, "the group holds the rows that were shown")
    } finally {
      await close()
    }
  })

  test("17. while a source refresh's new server list is on its way, Save as group waits for it, then holds the rows it shows", async () => {
    daemon.addAcme()
    const { page, close } = await open("servers")
    try {
      await row(page, "Amsterdam").waitFor()
      await openFilter(page, "Name")
      await textChip(page, "Name matches").fill("edge")
      await page.keyboard.press("Escape")
      await until(async () => (await page.locator("tbody tr").count()) === 6 && (await saveAs(page).isEnabled()), "six rows and Save as group")
      let release
      let listsHeld = 0
      const held = new Promise((r) => { release = r })
      await page.route("**/api/servers?*", async (route) => { listsHeld++; await held; await route.continue() })
      const [acmeServer] = daemon.catalogue().filter((l) => l.source === "acme")
      daemon.refreshDocument("acme", { addServer: { ...acmeServer, id: "lw-added", label: "EDGE Added" } })
      await until(() => listsHeld > 0, "the new list asked for")
      for (const end = Date.now() + 1500; Date.now() < end; await new Promise((r) => setTimeout(r, 50))) {
        assert.ok(await saveAs(page).isDisabled(), "Save as group waits for the new list")
      }
      release()
      await until(async () => (await page.locator("tbody tr").count()) === 7 && (await saveAs(page).isEnabled()), "the added server shown, and Save as group again")
      const shown = await namesShown(page)
      await saveAs(page).click()
      await page.getByLabel("Name of New group").waitFor()
      const { resolved } = daemon.group(new URL(page.url()).pathname.split("/").pop())
      assert.equal(resolved.length, shown.length, "the group holds the rows that were shown")
    } finally {
      await close()
    }
  })

  test("17. a source refresh that adds a server a group's Name pattern matches shows it among the group's members", async () => {
    daemon.addAcme()
    const members = { sources: { not: false, values: ["acme"] }, countries: { not: false, values: [] }, protocols: { not: false, values: [] }, include: "edge", exclude: "", box: null }
    daemon.set({ groups: new Map([["edge", groupView("edge", 1, { name: "Edge", members }, [], daemon.catalogue())]]) })
    const { page, close } = await open("servers")
    try {
      const answered = page.waitForResponse((r) => r.url().endsWith("/api/groups/preview"))
      await nav(page).getByRole("link", { name: /Edge/ }).click()
      await answered
      await until(async () => (await namesShown(page, true)).length === 6, "the six servers the rule holds")
      const [acmeServer] = daemon.catalogue().filter((l) => l.source === "acme")
      daemon.refreshDocument("acme", { addServer: { ...acmeServer, id: "lw-added", label: "EDGE Added" } })
      await until(async () => (await namesShown(page, true)).length === 7, "the added server among the members")
    } finally {
      await close()
    }
  })

  test("17. a group's tab shows the servers it doesn't hold after its members, dimmed but for their boxes, so any can be ticked, and a box never moves its row", async () => {
    daemon.set({ groups: new Map([["fr", groupView("fr", 1, { name: "Ticked", members: rule(["aurora"], [], "ticked"), ticked: ["l-fra"] })]]) })
    const { page, close } = await open("groups/fr")
    try {
      await row(page, "Paris").waitFor()
      const cells = async (n) => (await page.locator(`tbody tr td:nth-child(${n})`).allInnerTexts()).map((x) => x.trim())
      assert.deepEqual((await cells(4)).map((x) => x.split(/\s/)[0]), ["Frankfurt", "Amsterdam", "Paris"])
      const dims = await row(page, "Paris").evaluate((tr, outside) => [tr, ...tr.cells].map((el) => el.className.includes(outside)), OUTSIDE)
      assert.deepEqual(dims, [false, false, ...dims.slice(2).map(() => true)], "a row the group leaves out is dimmed in every cell but its box")
      await boxOf(page, "Paris").click()
      await until(async () => !(await dimmed(page, "Paris")), "a ticked server joins")
      await until(() => daemon.group("fr").order.join() === "l-fra,l-par", "the daemon ranks it among the members")
      await page.waitForTimeout(300)
      assert.deepEqual((await cells(4)).map((x) => x.split(/\s/)[0]), ["Frankfurt", "Amsterdam", "Paris"], "a tick never moves its row")
      for (const filter of ["Picked", "Status"]) assert.equal(await page.getByRole("button", { name: `Filter servers by ${filter}` }).count(), 0, `no ${filter} header on a rule's tab`)
      await boxOf(page, "Paris").click()
      await until(async () => dimmed(page, "Paris"), "an unticked server leaves")
      assert.deepEqual((await cells(4)).map((x) => x.split(/\s/)[0]), ["Frankfurt", "Amsterdam", "Paris"], "an untick never moves its row either")
    } finally {
      await close()
    }
  })

  /** Picks a value in a column's filter on a group's tab, a rule edit of the form. */
  const pickIn = async (page, column, option) => {
    const dialog = await openFilter(page, column)
    await dialog.getByRole("option", { name: option }).click()
    await page.keyboard.press("Escape")
    await dialog.waitFor({ state: "detached" })
  }
  /** Another writer saves a group's rule, unheard: no event, so the page holds the view from before. */
  const changeGroupUnheard = async (id, members) => {
    const { revision } = await (await fetch(daemon.url + "api/state")).json()
    const g = daemon.group(id)
    daemon.set({ groups: new Map([...daemon.groups().map((x) => [x.id, x]), [id, groupView(id, g.revision + 1, { name: g.name, members }, g.usedBy)]]), revision: revision + 1 })
  }

  test("17. a group's Save with no answer for 30 s ends with the toast's Retry, and Retry sends the same rule under its key, keeping what was typed since", async () => {
    seed()
    const { page, close } = await open("groups/all-aurora", { clock: true })
    try {
      await row(page, "Paris").waitFor()
      let down = true
      await page.route("**/api/groups/all-aurora", (r) => (r.request().method() === "PUT" && down ? r.abort() : r.continue()))
      await pickIn(page, "Country", /France/)
      await saveButton(page).click()
      assert.equal(await saveButton(page).getAttribute("aria-busy"), "true", "Save is Pending")
      await page.clock.fastForward(31_000)
      const said = toast(page, "timed out")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "All Aurora timed out Retry")
      await pickIn(page, "Protocol", /VLESS/)
      down = false
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.group("all-aurora").members.countries.values.includes("FR"), "Retry lands the rule")
      assert.equal(new Set(requests("PUT", "api/groups/all-aurora").map((r) => r.idempotencyKey)).size, 1, "under the key every attempt had")
      assert.deepEqual(daemon.group("all-aurora").members.protocols.values, [], "what was typed since wasn't in it")
      await until(async () => (await saveButton(page).isEnabled()) && (await page.getByRole("button", { name: /^Edit Protocol/ }).count()) === 1, "what was typed since stays, to be saved")
    } finally {
      await close()
    }
  })

  test("17. after no answer, an unchanged group rule's Save over another writer's change is a new write at their revision", async () => {
    seed()
    const { page, close } = await open("groups/all-aurora", { clock: true })
    try {
      await row(page, "Paris").waitFor()
      let drop = true
      const sent = []
      await page.route("**/api/groups/all-aurora", (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        sent.push({ key: r.request().headers()["idempotency-key"], ifMatch: r.request().headers()["if-match"] })
        return drop ? r.abort() : r.continue()
      })
      await pickIn(page, "Country", /France/)
      await saveButton(page).click()
      await page.clock.fastForward(31_000)
      await toast(page, "timed out").waitFor()
      daemon.changeOwner("group", "all-aurora", { name: "Theirs", revision: daemon.group("all-aurora").revision + 1 })
      await readsAnswered(page, "api/state")
      drop = false
      await saveButton(page).click()
      await until(() => daemon.group("all-aurora").members.countries.values.includes("FR"), "the rule stored over theirs")
      assert.notEqual(sent.at(-1).key, sent[0].key, "a new write")
      assert.equal(sent.at(-1).ifMatch, `"${daemon.group("all-aurora").revision - 1}"`, "at their revision")
      assert.equal(daemon.group("all-aurora").name, "Theirs", "their name stands")
    } finally {
      await close()
    }
  })

  test("17. a group's rule left after its Save's no answer finds its typing there when the group opens again", async () => {
    seed()
    const { page, close } = await open("groups/all-aurora", { clock: true })
    try {
      await row(page, "Paris").waitFor()
      await page.route("**/api/groups/all-aurora", (r) => (r.request().method() === "PUT" ? r.abort() : r.continue()))
      await pickIn(page, "Country", /France/)
      await saveButton(page).click()
      await page.clock.fastForward(31_000)
      await toast(page, "timed out").waitFor()
      await nav(page).getByRole("link", { name: "Events" }).click()
      await page.waitForURL(/\/events$/)
      await page.clock.fastForward(1_000)
      await nav(page).getByRole("link", { name: /All Aurora/ }).click()
      await page.getByRole("button", { name: /^Edit Country/ }).waitFor()
      assert.equal(await saveButton(page).isEnabled(), true, "the rule as typed, to be saved")
      assert.deepEqual(daemon.group("all-aurora").members.countries.values, [], "nothing stored")
    } finally {
      await close()
    }
  })

  test("17. a group's tab left while its Save is out, then refused, shows its rule as typed when opened again, and the refusal is its toast", async () => {
    seed()
    const { page, close } = await open("groups/all-aurora")
    try {
      await row(page, "Paris").waitFor()
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/groups/all-aurora", async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        await held
        await r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "sourcesGone", message: "No source left." }) })
      })
      await pickIn(page, "Country", /France/)
      await saveButton(page).click()
      await nav(page).getByRole("link", { name: "Events" }).click()
      await page.waitForURL(/\/events$/)
      release()
      const said = toast(page, "sources gone")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "All Aurora sources gone")
      await nav(page).getByRole("link", { name: /All Aurora/ }).click()
      await page.getByRole("button", { name: /^Edit Country/ }).waitFor()
      assert.equal(await saveButton(page).isEnabled(), true, "the rule as typed, to be saved")
      assert.deepEqual(daemon.group("all-aurora").members.countries.values, [], "nothing stored")
    } finally {
      await close()
    }
  })

  test("17. a group's rename that meets another writer's is a single control's conflict: the name goes back to what is stored, the toast says theirs and mine with Keep mine, and Keep mine renames over the group as stored", async () => {
    const { page, close } = await open("groups/all-aurora")
    try {
      const heading = page.locator("[data-slot=group-heading]")
      const name = heading.locator("button[aria-label^='Rename ']")
      await name.click()
      await heading.getByLabel("Name of All Aurora").fill("Mine")
      let theirs = false
      await page.route("**/api/groups/all-aurora", async (r) => {
        if (r.request().method() === "PUT" && !theirs) {
          theirs = true
          daemon.renameGroup("all-aurora", "Theirs")
        }
        await r.continue()
      })
      await page.keyboard.press("Enter")
      const said = toast(page, "changed to")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "All Aurora changed to Theirs Your value: Mine Keep mine")
      await until(async () => (await name.getAttribute("aria-label")) === "Rename Theirs", "the name goes back to what is stored")
      assert.equal(await page.getByRole("button", { name: /^(Save|Discard)$/ }).count(), 0, "no Save and no Discard")
      const members = daemon.group("all-aurora").members
      await said.getByRole("button", { name: "Keep mine" }).click()
      await until(() => daemon.group("all-aurora").name === "Mine", "Keep mine renames over what is stored")
      await until(async () => (await name.getAttribute("aria-label")) === "Rename Mine", "the heading shows it")
      assert.deepEqual(daemon.group("all-aurora").members, members, "and the rule doesn't move")
    } finally {
      await close()
    }
  })

  test("17. a group's rename beside rule edits saves the name alone, over the rule as stored, and the edits stay to be saved", async () => {
    seed()
    const { page, close } = await open("groups/germany")
    try {
      await row(page, "Frankfurt").waitFor()
      await pickIn(page, "Protocol", /VLESS/)
      const heading = page.locator("[data-slot=group-heading]")
      await heading.locator("button[aria-label^='Rename ']").click()
      await heading.getByLabel("Name of Germany").fill("Deutschland")
      await page.keyboard.press("Enter")
      await until(() => daemon.group("germany").name === "Deutschland", "the rename lands at once")
      assert.deepEqual(daemon.group("germany").members.protocols.values, [], "with the rule as stored")
      await until(async () => (await saveButton(page).isEnabled()), "the rule's edit stays, to be saved")
      await saveButton(page).click()
      await until(() => daemon.group("germany").members.protocols.values.length === 1, "Save lands the rule")
      assert.equal(daemon.group("germany").name, "Deutschland", "over the new name")
    } finally {
      await close()
    }
  })

  test("17. a group's Save whose conflict leaves no difference is no conflict: another writer stored the same rule, and the form settles with no toast", async () => {
    seed()
    const { page, close } = await open("groups/germany")
    try {
      await row(page, "Frankfurt").waitFor()
      await pickIn(page, "Protocol", /VLESS/)
      const mine = { ...daemon.group("germany").members, protocols: among(["vless"]) }
      let theirs = false
      await page.route("**/api/groups/germany", async (r) => {
        if (r.request().method() === "PUT" && !theirs) {
          theirs = true
          await changeGroupUnheard("germany", mine)
        }
        await r.continue()
      })
      await saveButton(page).click()
      await until(() => requests("PUT", "api/groups/germany").some((r) => r.status === 412), "the daemon finds another writer's")
      await saveBar(page).waitFor({ state: "detached" })
      await page.waitForTimeout(300)
      assert.equal(await toast(page, "").count(), 0, "nothing to say")
      assert.equal(requests("PUT", "api/groups/germany").length, 1)
    } finally {
      await close()
    }
  })

  test("17. a group's Save over a rule another writer saved is a conflict: untouched filters take what is stored, the draft stays, and one toast names the filters that moved, never their chips", async () => {
    seed()
    const { page, close } = await open("groups/germany")
    try {
      await row(page, "Frankfurt").waitFor()
      await pickIn(page, "Source", /Aurora/)
      await changeGroupUnheard("germany", { ...rule(["acme"], ["NL"]), protocols: among(["vless"]) })
      await saveButton(page).click()
      const said = toast(page, "changed in another tab")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Germany changed in another tab Source filter", "the touched filter that moved, by name, and no chip")
      assert.equal(await said.getByRole("button", { name: /Keep mine|Retry/ }).count(), 0, "Save is the one fix")
      await until(async () => (await page.getByRole("button", { name: /^Edit Country/ }).innerText()).includes("Netherlands"), "the untouched Country takes what is stored")
      assert.match(await page.getByRole("button", { name: /^Edit Source/ }).innerText(), /Aurora/, "the draft stays")
      assert.equal(await saveButton(page).isEnabled(), true)
      await saveButton(page).click()
      await until(() => daemon.group("germany").members.sources.values.includes("aurora"), "Save sends the draft over what is stored")
      assert.deepEqual(daemon.group("germany").members.countries.values, ["NL"], "the other writer's Country stands")
      await said.waitFor({ state: "detached" })
    } finally {
      await close()
    }
  })

  test("17. Discard after a Save that got no answer takes the toast's Retry with it, and nothing is sent after", async () => {
    seed()
    const { page, close } = await open("groups/all-aurora", { clock: true })
    try {
      await row(page, "Paris").waitFor()
      await page.route("**/api/groups/all-aurora", (r) => (r.request().method() === "PUT" ? r.abort() : r.continue()))
      await pickIn(page, "Country", /France/)
      await saveButton(page).click()
      await page.clock.fastForward(31_000)
      const said = toast(page, "timed out")
      await said.waitFor()
      const sent = requests("PUT", "api/groups/all-aurora").length
      await saveBar(page).getByRole("button", { name: "Discard", exact: true }).click()
      await said.waitFor({ state: "detached" })
      await saveBar(page).waitFor({ state: "hidden" })
      await page.clock.fastForward(31_000)
      assert.equal(requests("PUT", "api/groups/all-aurora").length, sent, "nothing sent after")
    } finally {
      await close()
    }
  })

  for (const width of [1280, 390]) {
    test(`17. at ${width} px a group's tab counts its members at its filter line's end, with nothing to save too, and Discard and Save stand beside the count while there is`, async () => {
      seed()
      const { page, close } = await open("groups/all-aurora", { width })
      try {
        await row(page, "Paris").waitFor()
        const count = page.getByText(/^\d+ in group$/)
        await page.getByText("3 in group", { exact: true }).waitFor()
        assert.equal(await saveBar(page).count(), 0, "with no Save")
        const filter = page.getByRole("button", { name: "Filter", exact: true })
        const middle = async (el) => { const b = await el.boundingBox(); return b.y + b.height / 2 }
        assert.ok(Math.abs(await middle(count) - await middle(filter)) < 2, "on Filter's own line")
        const end = async () => { const b = await count.boundingBox(); return Math.round(b.x + b.width) }
        const table = await page.getByRole("table", { name: "Servers" }).boundingBox()
        assert.ok(Math.abs(await end() - table.x - table.width) < 2, "at its end")
        const at = await end()
        const dialog = await openFilter(page, "Country")
        await dialog.getByRole("option", { name: /France/ }).click()
        await page.keyboard.press("Escape")
        await page.getByText("1 in group", { exact: true }).waitFor()
        assert.ok(Math.abs(await middle(count) - await middle(saveButton(page))) < 2, "Save beside the count")
        assert.equal(await end(), at, "which keeps its end")
        await discardOf(page).click()
        await saveBar(page).waitFor({ state: "hidden" })
        await page.getByText("3 in group", { exact: true }).waitFor()
      } finally {
        await close()
      }
    })
  }

  test("17. a group's tab counts no members before its servers are read", async () => {
    const { page, release, close } = await openHolding("groups/all-aurora", "api/servers")
    try {
      await page.locator("[data-slot=pending-row]").first().waitFor()
      await page.waitForTimeout(300)
      assert.equal(await page.getByText(/in group$/).count(), 0)
      await release()
      await page.getByText("3 in group", { exact: true }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. Protocol is set from its header and saved in a group's rule", async () => {
    seed()
    const { page, close } = await open("groups/all-aurora")
    try {
      await row(page, "Paris").waitFor()
      const dialog = await openFilter(page, "Protocol")
      await dialog.getByRole("radio", { name: "is none of" }).click()
      await dialog.getByRole("option", { name: /VLESS/ }).click()
      await page.keyboard.press("Escape")
      await page.getByRole("group", { name: "Protocol filter" }).getByText("is not").waitFor()
      await saveButton(page).click()
      await until(() => daemon.group("all-aurora").members.protocols.values.length === 1, "the saved rule")
      assert.deepEqual(daemon.group("all-aurora").members.protocols, { not: true, values: ["vless"] })
    } finally {
      await close()
    }
  })

  test("17. a group's Save goes from the draft straight to the saved rule, never back to the old one", async () => {
    seed()
    const { page, close } = await open("groups/all-aurora")
    try {
      await row(page, "Paris").waitFor()
      const old = await tableOf(page)
      const dialog = await openFilter(page, "Country")
      await dialog.getByRole("option", { name: /France/ }).click()
      await page.keyboard.press("Escape")
      await page.getByText("1 in group", { exact: true }).waitFor()
      await until(() => saveButton(page).isEnabled(), "Save to come on")
      await page.waitForTimeout(400)
      const drafted = await tableOf(page)
      daemon.set({ readDelay: { "api/state": 800 } })
      await recordTables(page)
      await saveButton(page).click()
      await until(() => daemon.group("all-aurora").members.countries.values.length === 1, "the saved rule")
      await saveBar(page).waitFor({ state: "hidden" })
      await readsAnswered(page, "api/state")
      const tables = await tablesSeen(page)
      const saved = tables.at(-1)
      assert.equal(drafted.count, "1 in group")
      assert.ok(tables.every((t) => t.count === drafted.count), `the new count stays, never the old count: ${JSON.stringify(tables)}`)
      const rule = ({ chips, rows }) => JSON.stringify({ chips, rows })
      assert.ok(!tables.some((t) => t.rows.join() === old.rows.join() || t.chips.join() === old.chips.join()), `never the old rule: ${JSON.stringify(tables)}`)
      assert.ok(tables.every((t) => rule(t) === rule(drafted) || rule(t) === rule(saved)), `the draft, then the saved rule, and nothing between: ${JSON.stringify(tables)}`)
    } finally {
      await close()
    }
  })

  test("17. a group edited back to its old rule while the page's read still holds that rule is unsaved, not saved", async () => {
    seed()
    const { page, close } = await open("groups/all-aurora")
    try {
      await row(page, "Paris").waitFor()
      const france = async () => {
        const dialog = await openFilter(page, "Country")
        await dialog.getByRole("option", { name: /France/ }).click()
        await page.keyboard.press("Escape")
      }
      await france()
      await until(() => saveButton(page).isEnabled(), "Save to come on")
      daemon.set({ readDelay: { "api/state": 3000 } })
      await saveButton(page).click()
      await until(() => daemon.group("all-aurora").members.countries.values.length === 1, "France saved")
      await saveBar(page).waitFor({ state: "hidden" })
      await france()
      await saveBar(page).waitFor({ timeout: 1000 })
      await readsAnswered(page, "api/state")
      assert.ok(await saveBar(page).isVisible(), "still unsaved once the read brings France")
      daemon.set({ readDelay: {} })
      await until(() => saveButton(page).isEnabled(), "Save to come on")
      await saveButton(page).click()
      await until(() => daemon.group("all-aurora").members.countries.values.length === 0, "the old rule saved again")
    } finally {
      await close()
    }
  })

  test("17. a group's Delete lists the slot using it; a refusal for a slot that came to use it meanwhile is that slot's row, with no toast; with none left the group goes", async () => {
    seed()
    const { page, close } = await open("groups/mine")
    try {
      await page.getByRole("button", { name: "Delete Mine" }).click()
      const dialog = page.getByRole("alertdialog")
      await dialog.getByRole("region", { name: "Used by" }).getByText("EU", { exact: true }).waitFor()
      assert.equal(await dialog.getByRole("button", { name: "Delete", exact: true }).isDisabled(), true)
      await dialog.getByRole("button", { name: "Cancel" }).click()
      await dialog.waitFor({ state: "detached" })
      assert.ok(daemon.group("mine"))
      await page.goto(daemon.url + "groups/germany")
      await page.getByRole("button", { name: "Delete Germany" }).click()
      assert.deepEqual((await dialog.innerText()).split("\n").map((l) => l.trim()).filter(Boolean), ["Delete Germany?", "Cancel", "Delete"], "free: the title and the buttons")
      daemon.changeSlot("eu", { exit: { kind: "group", group: "germany" } }, { announce: false })
      await dialog.getByRole("button", { name: "Delete", exact: true }).click()
      const usedBy = dialog.getByRole("region", { name: "Used by" })
      await usedBy.getByText("EU", { exact: true }).waitFor()
      assert.equal(await toast(page, "").count(), 0, "the refusal is EU's row, with no toast")
      assert.ok(daemon.group("germany"))
      await usedBy.getByRole("combobox", { name: "Exit of EU" }).click()
      await page.locator("[data-radix-popper-content-wrapper]").locator('[data-value="direct"]').click()
      await usedBy.waitFor({ state: "detached" })
      await dialog.getByRole("button", { name: "Delete", exact: true }).click()
      await until(() => new URL(page.url()).pathname === BASE + "servers", "back to All servers")
      await nav(page).locator(`a[href$="/groups/germany"]`).waitFor({ state: "detached" })
      assert.equal(daemon.group("germany"), undefined)
    } finally {
      await close()
    }
  })

  test("17. a group's Delete refused for a slot whose read lands older than the refusal reads the view again until EU's row stands, Delete held back", () =>
    deleteRefusedOverAnOldRead("groups/all-aurora", "All Aurora", { kind: "group", group: "all-aurora" }))

  test("17. a group's rename, its rule's Save and its Delete are one write: while one is out the others take no press, and only the one pressed is Pending", async () => {
    seed()
    const { page, close } = await open("groups/germany")
    try {
      await row(page, "Frankfurt").waitFor()
      const heading = page.locator("[data-slot=group-heading]")
      const name = heading.locator("button[aria-label^='Rename ']")
      const remove = heading.locator("button[aria-label^='Delete ']")
      const door = holdWrites(page, "**/api/groups/germany")
      await door.install()
      await name.click()
      await heading.getByLabel("Name of Germany").fill("Deutschland")
      await page.keyboard.press("Enter")
      await until(async () => (await name.getAttribute("aria-busy")) === "true", "the rename is Pending")
      assert.equal(await remove.isDisabled(), true, "Delete waits while the rename is out")
      await door.release()
      await until(() => daemon.group("germany").name === "Deutschland", "the rename lands")
      await until(async () => (await name.getAttribute("aria-busy")) === null && (await remove.isEnabled()), "the rename landed, both take a press")

      await pickIn(page, "Protocol", /VLESS/)
      await saveButton(page).click()
      await until(async () => (await saveButton(page).getAttribute("aria-busy")) === "true", "Save is Pending")
      assert.equal(await name.getAttribute("aria-busy"), null, "the name isn't Pending for the rule's Save")
      assert.equal(await name.isDisabled(), true, "and takes no press")
      assert.equal(await remove.isDisabled(), true, "nor does Delete")
      await door.release()
      await until(() => daemon.group("germany").members.protocols.values.length === 1, "the rule lands")

      await until(() => remove.isEnabled(), "Save landed, Delete takes a press")
      await remove.click()
      const dialog = page.getByRole("alertdialog")
      const confirm = dialog.getByRole("button", { name: "Delete", exact: true })
      await until(() => confirm.isEnabled(), "Delete enables")
      await confirm.click()
      await until(async () => (await confirm.getAttribute("aria-busy")) === "true", "Delete is Pending")
      assert.equal(await name.getAttribute("aria-busy"), null, "the name isn't Pending for the Delete")
      assert.equal(await name.isDisabled(), true, "and takes no press")
      await door.release()
      await page.waitForURL(/\/servers$/)
      assert.equal(daemon.group("germany"), undefined)
    } finally {
      await close()
    }
  })

  test("17. a stored group's rule with a pattern Go refuses keeps Save off, and nothing beside it", async () => {
    const { page, close } = await open("groups/all-aurora")
    try {
      await rowOf(page, "Paris").waitFor()
      await openFilter(page, "Name")
      await textChip(page, "Name matches").fill("(")
      const reason = page.getByRole("dialog").locator("[data-slot=field-error]")
      await until(async () => (await reason.count()) === 1 && (await reason.innerText()) === "Missing closing )", "Go's cause under the pattern")
      assert.equal(await saveButton(page).isDisabled(), true, "Save disabled while a filter is refused")
      assert.equal(await page.getByText(/in group$/).count(), 0, "no count without its preview")
    } finally {
      await close()
    }
  })

  test("17. a new group previews through Go, stores nothing until Save, and saves its name and rule in one create under a fresh id", async () => {
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Group")
      const name = page.getByRole("textbox", { name: /^Name of / })
      await page.getByLabel("Name of New group").waitFor()
      await name.fill("Mine")
      await name.press("Enter")
      await page.getByRole("heading", { name: "Mine" }).waitFor()
      await openFilter(page, "Source")
      await page.getByRole("option", { name: /Aurora/ }).click()
      await page.keyboard.press("Escape")
      await openFilter(page, "Country")
      await page.getByRole("option", { name: /Germany/ }).click()
      await page.keyboard.press("Escape")
      await openFilter(page, "Name")
      await textChip(page, "Name matches").fill("(")
      const reason = page.getByRole("dialog").locator("[data-slot=field-error]")
      await until(async () => (await reason.count()) === 1 && (await reason.innerText()) === "Missing closing )", "Go's cause under the pattern")
      assert.equal(await saveButton(page).isDisabled(), true, "Save disabled while a filter is refused")
      assert.equal(await saveBar(page).innerText(), "Discard\nSave", "nothing beside Save")
      assert.equal(await page.getByText(/in group$/).count(), 0, "no count without its preview")
      await textChip(page, "Name matches").fill("")
      await until(async () => (await reason.count()) === 0, "the preview to recover")
      await page.keyboard.press("Escape")
      assert.ok(daemon.log().some((r) => r.method === "POST" && r.path === BASE + "api/groups/preview"), "Go previewed the rule")
      assert.deepEqual(daemon.log().filter((r) => r.method === "PUT" && r.path.includes("api/groups/")), [], "nothing stored before Save")
      await saveButton(page).click()
      await page.waitForURL(STORED_GROUP)
      const puts = daemon.log().filter((r) => r.method === "PUT" && r.path.includes("api/groups/"))
      assert.equal(puts.length, 1, "one write")
      const [put] = puts
      assert.equal(put.ifNoneMatch, "*")
      assert.notEqual(put.path, BASE + "api/groups/all-aurora")
      assert.equal(new URL(page.url()).pathname, put.path.replace("api/", ""), "the group's own address")
      assert.deepEqual(JSON.parse(put.body), { name: "Mine", members: rule(["aurora"], ["DE"]), ticked: [] })
      await page.locator("[data-slot=group-heading]").getByRole("heading", { name: "Mine" }).waitFor()
    } finally {
      await close()
    }
  })
})

/** Opens `path` with every GET of `held` held until the returned `release`, which answers them from the daemon. */
async function openHolding(path, held, { width = 1440 } = {}) {
  const context = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: "dark" })
  const page = await context.newPage()
  page.setDefaultTimeout(10_000)
  const waiting = []
  let releasing = false
  await page.route((url) => url.pathname.startsWith(BASE + held), (route) => {
    if (route.request().method() !== "GET" || releasing) return route.fallback()
    waiting.push(route)
  })
  await page.goto(daemon.url + path)
  const release = async () => {
    releasing = true
    for (const route of waiting.splice(0)) await route.fallback()
  }
  return { page, release, close: () => context.close() }
}

/** Where each element stands on screen, rounded to the pixel, by a name for it. */
const boxesOf = async (named) => Object.fromEntries(await Promise.all(Object.entries(named).map(async ([name, locator]) => {
  const box = await locator.boundingBox()
  assert.ok(box, `${name} is on screen`)
  return [name, [box.x, box.y, box.width, box.height].map(Math.round).join(",")]
})))

describe("waiting, a failed read and an empty list", () => {
  test("3. a week's place is 100 px in every form: a young week's strip stands in the skeleton's place, and nothing in the heading moves as it waits, lands or fails", async () => {
    seedTroubled()
    const parts = (page) => {
      const heading = headingOf(page)
      return { name: heading.getByText("Unlimited", { exact: true }).first(), refresh: refreshButton(page), settings: heading.getByRole("button", { name: "Settings for Unlimited" }), week: heading.getByText("7d", { exact: true }) }
    }
    const placeOf = async (locator) => { const b = await locator.boundingBox(); return { x: Math.round(b.x), width: Math.round(b.width) } }
    {
      const { page, release, close } = await openHolding("sources/unlimited", "api/analytics", { width: 1280 })
      try {
        const reading = headingOf(page).getByRole("status", { name: "Loading history…" })
        await reading.waitFor()
        const before = await boxesOf(parts(page))
        const waiting = await placeOf(reading.locator("[data-slot=skeleton]"))
        assert.equal(waiting.width, 100, "the waiting form's place is the strip's 100 px")
        await release()
        await weekButton(page).waitFor()
        assert.deepEqual(await placeOf(weekButton(page).locator("[data-slot=week-strip]")), waiting, "a young week's strip stands in the same 100 px, its days before it was added bare")
        assert.deepEqual(await boxesOf(parts(page)), before, "nothing in the heading moves as it lands")
      } finally {
        await close()
      }
    }
    daemon.set({ analyticsFails: true })
    const { page, close } = await open("sources/unlimited", { width: 1280 })
    try {
      const mark = headingOf(page).getByRole("button", { name: /^The week: / })
      await mark.waitFor()
      assert.equal((await placeOf(mark.locator("svg").locator(".."))).width, 100, "a week that failed keeps the place")
    } finally {
      await close()
    }
  })

  test("3. what arrives moves nothing: a source's heading when its week lands, the server table when its list lands, and a server's details when its week is read again", async () => {
    seedTroubled()
    {
      const { page, release, close } = await openHolding("sources/steady", "api/analytics")
      try {
        const heading = headingOf(page)
        const reading = heading.getByRole("status", { name: "Loading history…" })
        await reading.waitFor()
        const parts = () => ({
          name: heading.getByText("Steady", { exact: true }).first(), expiry: heading.getByRole("button", { name: /^Expires in/ }),
          refresh: refreshButton(page), settings: heading.getByRole("button", { name: "Settings for Steady" }), week: heading.getByText("7d", { exact: true }),
          announcement: heading.getByText(/^Maintenance on Sunday/),
        })
        const before = await boxesOf(parts())
        const strip = (await reading.locator("[data-slot=skeleton]").boundingBox()).x
        await release()
        await weekButton(page).waitFor()
        assert.deepEqual(await boxesOf(parts()), before, "the heading stays where it stood")
        assert.equal(Math.round((await weekButton(page).locator("[data-slot=week-strip]").boundingBox()).x), Math.round(strip), "the strip starts where its skeleton did")
      } finally {
        await close()
      }
    }
    {
      const { page, release, close } = await openHolding("servers", "api/servers")
      try {
        const pending = page.locator("[data-slot=pending-row]")
        await pending.first().waitFor()
        const frame = () => ({
          search: page.getByRole("button", { name: "Search", exact: true }), filter: page.getByRole("button", { name: "Filter", exact: true }),
          ...Object.fromEntries(["Name", "Source", "Protocol", "Stability"].map((name) => [name, page.locator("thead th").filter({ hasText: name })])),
        })
        const before = await boxesOf(frame())
        const firstRow = Math.round((await pending.first().boundingBox()).y)
        await release()
        await page.locator("tbody tr:not([data-slot=pending-row])").first().waitFor()
        assert.equal(await pending.count(), 0)
        assert.deepEqual(await boxesOf(frame()), before, "the toolbar and the header stay where they stood")
        assert.equal(Math.round((await page.locator("tbody tr").first().boundingBox()).y), firstRow, "the first row stands where its Pending row did")
      } finally {
        await close()
      }
    }
    daemon.set({ analyticsFails: true })
    const { page, close } = await open("sources/steady")
    try {
      await rowOf(page, "Amsterdam").getByRole("button", { name: "Amsterdam", exact: true }).focus()
      await page.keyboard.press("Enter")
      const dialog = page.getByRole("dialog", { name: "Amsterdam" })
      const retry = dialog.getByRole("button", { name: "Retry" })
      await retry.waitFor()
      /* The dialog's opening zoom ends first. */
      await page.waitForTimeout(400)
      const frame = dialog.locator("[data-slot=chart-frame]")
      assert.equal((await frame.getByRole("alert").innerText()).replace(/\s+/g, " ").trim(), "HTTP 502 Retry", "the read's reason in its fewest words, with Retry, in the chart's frame")
      const [inner, outer] = [await frame.getByRole("alert").boundingBox(), await frame.boundingBox()]
      assert.ok(Math.abs(inner.x + inner.width / 2 - (outer.x + 56 + (outer.width - 56) / 2)) < 2, "on a centred line in the frame")
      assert.equal(await readoutOf(dialog), "7d", "a week not read brings no count: its keys' line is held unseen over the frame")
      const parts = () => ({ dialog, chart: dialog.locator("[data-slot=chart-frame], [data-slot=chart]").first(), country: dialog.getByText("Country", { exact: true }) })
      const before = await boxesOf(parts())
      let answer
      await page.route((url) => url.pathname.startsWith(BASE + "api/analytics"), (route) => { answer = route })
      daemon.set({ analyticsFails: false })
      await retry.click()
      await until(async () => (await retry.getAttribute("aria-busy")) === "true", "Retry is Pending while it is out")
      assert.deepEqual(await boxesOf(parts()), before, "the failed form stays while the retry is out")
      await until(() => answer, "the retry's read")
      await answer.fallback()
      await retry.waitFor({ state: "detached" })
      assert.deepEqual(await boxesOf(parts()), before, "the week that lands takes the failed form's place, at its size")
    } finally {
      await close()
    }
  })

  test("3. a server's details open on their week's failed read though the clock was set back while it was out", async () => {
    daemon.set({ analyticsFails: true })
    const { page, close } = await open("servers", { clock: true })
    try {
      const name = rowOf(page, "Frankfurt").getByRole("button", { name: "Frankfurt", exact: true })
      await name.waitFor()
      let asked
      await page.route((url) => url.pathname.startsWith(BASE + "api/analytics"), (route) => { asked = route })
      await name.focus()
      await page.keyboard.press("Enter")
      await until(() => asked, "the details' read of the week")
      await page.clock.setSystemTime(await page.evaluate(() => Date.now() - 1000))
      await asked.fallback()
      await page.getByRole("dialog", { name: "Frankfurt" }).getByRole("button", { name: "Retry" }).waitFor()
    } finally {
      await close()
    }
  })

  test("3. a second server's details open on its own week's failed read, though the first's, closed, was still out when it was asked for", async () => {
    const { page, close } = await open("servers")
    try {
      const nameOf = (server) => rowOf(page, server).getByRole("button", { name: server, exact: true })
      await nameOf("Frankfurt").waitFor()
      await nameOf("Frankfurt").focus()
      await page.keyboard.press("Enter")
      const frankfurt = page.getByRole("dialog", { name: "Frankfurt" })
      await frankfurt.waitFor()
      await page.keyboard.press("Escape")
      await frankfurt.waitFor({ state: "detached" })
      await page.waitForTimeout(2500)
      let held
      await page.route((url) => url.pathname.startsWith(BASE + "api/analytics"), (route) => {
        const lineage = new URL(route.request().url()).searchParams.get("lineage")
        if (lineage === "l-par") return route.fulfill({ status: 502, contentType: "text/html", body: "<h1>502 Bad Gateway</h1>" })
        held = route
      })
      await nameOf("Frankfurt").focus()
      await page.keyboard.press("Enter")
      await until(() => held, "Frankfurt's week asked for again, and held")
      await page.keyboard.press("Escape")
      await frankfurt.waitFor({ state: "detached" })
      await nameOf("Paris").focus()
      await page.keyboard.press("Enter")
      await page.getByRole("dialog", { name: "Paris" }).getByRole("button", { name: "Retry" }).waitFor({ timeout: 5000 })
      await held.fallback()
    } finally {
      await close()
    }
  })

  test("3. Settings shows the last failure of its read, though it came while Settings wasn't open, and its fix with it", async () => {
    daemon.set({ readFails: { "api/settings": 1 } })
    const { page, close } = await open("settings")
    try {
      const main = page.getByRole("main")
      await main.getByRole("button", { name: "Retry" }).waitFor()
      await nav(page).getByRole("link", { name: "Dashboard" }).click()
      await tile(page, "EU").waitFor()
      const reads = () => requests("GET", "api/settings")
      daemon.changeSettings({ unread: true })
      await until(() => reads().length === 2 && reads().every((r) => r.status !== 0), "a read of settings the page can't read")
      let release
      const held = new Promise((done) => { release = done })
      let out = false
      await page.route("**/api/settings", async (r) => { out = true; await held; await r.continue() })
      daemon.changeSettings({ unread: true })
      await until(() => out, "the next read of settings is out")
      await nav(page).getByRole("link", { name: "Settings" }).click()
      await main.getByRole("button", { name: "Reload" }).waitFor()
      assert.equal(await main.getByText("HTTP 502").count(), 0, "not the failure before it")
      assert.equal(await main.getByText("Page out of date").count(), 1, "the last failure's words")
      release()
    } finally {
      await close()
    }
  })

  test("4. a read with no answer becomes the failed form at 10 s, in the muted mark and words, and its Retry reads again; offline, a read fails at once", async () => {
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: "dark" })
    try {
      const page = await context.newPage()
      let asked = 0
      let answering = false
      await page.route((url) => url.pathname === BASE + "api/switches", (route) => {
        asked++
        if (answering) return route.fallback()
      })
      const started = Date.now()
      await page.goto(daemon.url + "events")
      await page.getByRole("list", { name: "Loading events" }).waitFor()
      await page.waitForTimeout(5_000)
      assert.equal(await page.getByRole("alert").count(), 0, "waiting under 10 s is Pending")
      const failed = page.getByRole("alert")
      await failed.waitFor({ timeout: 12_000 })
      const after = Date.now() - started
      assert.ok(after >= 9_500 && after < 13_000, `it failed at 10 s, not at ${after} ms`)
      assert.equal((await failed.innerText()).replace(/\s+/g, " ").trim(), "Timeout Retry")
      assert.equal(await toneOf(failed.getByText("Timeout")), "muted", "no red")
      assert.equal(await failed.locator("svg.lucide-circle-x").count(), 1, "the muted mark")
      assert.equal(asked, 1, "no automatic retry")
      answering = true
      await failed.getByRole("button", { name: "Retry" }).click()
      await page.locator("[data-slot=empty]").getByText("No events yet", { exact: true }).waitFor()
      assert.equal(asked, 2, "Retry read again")
      await context.setOffline(true)
      /* Settings is read since the page opened, as the write path keeps it, so its page reads it again when it opens. */
      const read = page.waitForEvent("requestfailed", { predicate: (r) => r.url().endsWith("api/settings"), timeout: 3_000 })
      await nav(page).getByRole("link", { name: "Settings" }).click()
      await read
      assert.equal(await page.getByRole("radiogroup", { name: "Retention" }).count(), 1, "offline, the read fails at once and isn't parked, and what the page had stays")
    } finally {
      await context.close()
    }
  })

  test("6. an emptied list has one compact form: a search's No match clears the search alone and keeps the chips; in a popover it has no button and its footer Clear hides while the search holds text", async () => {
    const { page, close } = await open("servers")
    try {
      const filter = await openFilter(page, "Country")
      await filter.getByRole("option", { name: /Germany/ }).click()
      await page.keyboard.press("Escape")
      await page.getByRole("button", { name: "Search", exact: true }).click()
      await page.getByPlaceholder("Search").fill("zzzz")
      const line = page.locator("[data-slot=table-line]")
      await line.getByText("No match", { exact: true }).waitFor()
      await line.getByRole("button", { name: "Clear" }).click()
      await page.locator("tbody tr").first().waitFor()
      await page.getByRole("button", { name: "Edit Country filter" }).waitFor()
      const popover = await openFilter(page, "Country")
      await popover.getByPlaceholder("Country").fill("zzzz")
      await popover.getByText("No match", { exact: true }).waitFor()
      assert.equal(await popover.getByRole("button", { name: "Clear", exact: true }).count(), 0, "no Clear under No match")
      await popover.getByRole("button", { name: "Clear search" }).click()
      await popover.getByRole("button", { name: "Clear", exact: true }).waitFor()
      assert.equal(await popover.getByRole("option", { name: /Germany/ }).getAttribute("aria-checked"), "true", "the pick stays")
    } finally {
      await close()
    }
  })

  test("6. Enter on a filter's Clear search empties the search and Enter on its footer Clear unpicks its values, and neither ticks the highlighted value", async () => {
    const { page, close } = await open("servers")
    try {
      const popover = await openFilter(page, "Country")
      const germany = popover.getByRole("option", { name: /Germany/ })
      await germany.click()
      const focused = (l) => l.evaluate((e) => e === document.activeElement)
      const search = popover.getByPlaceholder("Country")
      await search.fill("Ger")
      await search.focus()
      await page.keyboard.press("Tab")
      assert.ok(await focused(popover.getByRole("button", { name: "Clear search" })), "Tab from the search reaches Clear search")
      await page.keyboard.press("Enter")
      await until(async () => (await search.inputValue()) === "", "Enter empties the search")
      assert.ok(await focused(search), "the keys are back in the search")
      assert.equal(await germany.getAttribute("aria-checked"), "true", "the pick stays")
      const clear = popover.getByRole("button", { name: "Clear", exact: true })
      for (let i = 0; i < 4 && !(await focused(clear)); i++) await page.keyboard.press("Tab")
      assert.ok(await focused(clear), "Tab reaches the footer's Clear")
      await page.keyboard.press("Enter")
      await clear.waitFor({ state: "detached" })
      assert.equal(await popover.locator('[cmdk-item][aria-checked="true"]').count(), 0, "Enter on Clear unpicks every value and ticks none")
    } finally {
      await close()
    }
  })

  test("6. an arrow up from a filter's footer Clear, below every value, gives the keys back to the search, which highlights the last value and names it its active option", async () => {
    const { page, close } = await open("servers")
    try {
      const popover = await openFilter(page, "Country")
      await popover.getByRole("option", { name: /Germany/ }).click()
      const focused = (l) => l.evaluate((e) => e === document.activeElement)
      const search = popover.getByPlaceholder("Country")
      const clear = popover.getByRole("button", { name: "Clear", exact: true })
      await search.focus()
      for (let i = 0; i < 4 && !(await focused(clear)); i++) await page.keyboard.press("Tab")
      assert.ok(await focused(clear), "Tab reaches the footer's Clear")
      await page.keyboard.press("ArrowUp")
      assert.ok(await focused(search), "the keys are back in the search")
      const last = popover.locator('[cmdk-item]:not([aria-disabled="true"])').last()
      assert.equal(await last.getAttribute("aria-selected"), "true", "the last value is highlighted")
      assert.equal(await search.getAttribute("aria-activedescendant"), await last.getAttribute("id"), "the search names it its active option")
    } finally {
      await close()
    }
  })

  test("6. an arrow down from a filter's footer Clear, below every value, gives the keys back to the search with no value highlighted or named, so Enter ticks none", async () => {
    const { page, close } = await open("servers")
    try {
      const popover = await openFilter(page, "Country")
      const germany = popover.getByRole("option", { name: /Germany/ })
      await germany.click()
      const focused = (l) => l.evaluate((e) => e === document.activeElement)
      const search = popover.getByPlaceholder("Country")
      const clear = popover.getByRole("button", { name: "Clear", exact: true })
      await search.focus()
      await page.keyboard.press("Home")
      const first = await popover.locator('[cmdk-item]:not([aria-disabled="true"])').first().getAttribute("data-value")
      assert.deepEqual(await highlightOf(popover), { drawn: [first], named: first }, "Home highlights and names the first value")
      for (let i = 0; i < 4 && !(await focused(clear)); i++) await page.keyboard.press("Tab")
      assert.ok(await focused(clear), "Tab reaches the footer's Clear")
      await page.keyboard.press("ArrowDown")
      assert.ok(await focused(search), "the keys are back in the search")
      assert.deepEqual(await highlightOf(popover), { drawn: [], named: null }, "no value is highlighted or named")
      await page.keyboard.press("Enter")
      await page.waitForTimeout(300)
      assert.deepEqual(await popover.locator('[cmdk-item][aria-checked="true"]').evaluateAll((rows) => rows.map((r) => r.getAttribute("data-value"))),
        [await germany.getAttribute("data-value")], "Enter ticks no value, and Germany stays the one ticked")
    } finally {
      await close()
    }
  })
})

describe("Dashboard", () => {
  test("17. a foreign client is no warning", async () => {
    daemon.set({ vpnClients: clients("Office WireGuard") })
    const { page, close } = await open("")
    try {
      await dot(board(page), "Serving").first().waitFor()
      assert.equal(await page.getByLabel("Notices").count(), 0)
    } finally {
      await close()
    }
  })

  test("17. the root is the one board, and the old pages are gone", async () => {
    const { page, close } = await open("")
    try {
      await board(page).waitFor()
      assert.equal(await nav(page).getByRole("link", { name: "Dashboard" }).count(), 1)
      assert.equal(await nav(page).getByRole("link", { name: "Overview" }).count(), 0)
      assert.equal(await nav(page).getByRole("link", { name: "Slots" }).count(), 0)
      await page.goto(daemon.url + "slots")
      await page.locator("header").getByRole("heading", { name: "Not found" }).waitFor()
      await page.locator("[data-slot=empty]").getByRole("link", { name: "Dashboard" }).waitFor()
      assert.equal(page.url(), daemon.url + "slots", "no redirect")
      for (const gone of ["sources/nothing", "groups/nothing"]) {
        await page.goto(daemon.url + gone)
        await page.locator("[data-slot=empty]").getByRole("link", { name: "Dashboard" }).waitFor()
        assert.equal(await page.locator("header h1").innerText(), "Not found", `${gone}: an address that names no source or group is titled as one the page doesn't have`)
      }
    } finally {
      await close()
    }
  })

  test("17. with nothing set up the Dashboard is the compact empty block, its mark, No sources and Add source, and nothing else", async () => {
    daemon.set({ sources: new Map(), slots: new Map(), groups: new Map() })
    const { page, close } = await open("")
    try {
      const empty = page.locator("[data-slot=empty]")
      await empty.getByText("No sources", { exact: true }).waitFor()
      assert.equal(await empty.locator("svg.lucide-rss").count(), 1, "its mark")
      assert.ok((await empty.boundingBox()).height < 200, "compact, not a full-height panel")
      await empty.getByRole("button", { name: "Add source" }).click()
      await page.getByRole("dialog", { name: "Add source" }).getByLabel("Source", { exact: true }).waitFor()
      assert.equal((await empty.innerText()).replace(/\s+/g, " ").trim(), "No sources Add source")
    } finally {
      await close()
    }
  })

  test("17. with nothing set up and no UniFi key the Dashboard is two steps, Settings at its UniFi part, then Add source; with a key it is No sources", async () => {
    daemon.set({ sources: new Map(), slots: new Map(), groups: new Map(), unifiTrouble: "noKey" })
    const { page, close } = await open("")
    try {
      const empty = page.locator("[data-slot=empty]")
      await empty.getByText("Get started", { exact: true }).waitFor()
      assert.deepEqual((await empty.getByRole("listitem").allInnerTexts()).map((t) => t.replace(/\s+/g, " ").trim()), ["1. Add your UniFi API key Settings", "2. Add a proxy source Add source"])
      await empty.getByRole("button", { name: "Add source" }).click()
      const dialog = page.getByRole("dialog", { name: "Add source" })
      await dialog.getByLabel("Source", { exact: true }).waitFor()
      await page.keyboard.press("Escape")
      await dialog.waitFor({ state: "detached" })
      await empty.getByRole("link", { name: "Settings" }).click()
      await page.waitForURL(/\/settings#unifi$/)
      await page.getByLabel("API key").waitFor()
      for (const trouble of ["keyRefused", "notAnswering", null]) {
        daemon.set({ unifiTrouble: trouble })
        await page.goto(daemon.url)
        await empty.getByText("No sources", { exact: true }).waitFor()
        assert.equal(await empty.getByText("Get started").count(), 0, String(trouble))
      }
    } finally {
      await close()
    }
  })

  test("17. each trouble lives with what it describes: no strip over the board, the WAN on the globe, a disabled client on its box with Enable, a failed slot's dot as red as its words", async () => {
    daemon.set({ vpnClients: clients(), wan: "down" })
    withUnifi("eu", { client: "disabled" })
    const { page, close } = await open("")
    try {
      const eu = tile(page, "EU")
      await eu.getByText("VPN Client disabled", { exact: true }).waitFor()
      await eu.getByRole("button", { name: "Enable" }).waitFor()
      await page.locator("header").getByRole("button", { name: "WAN down" }).waitFor()
      assert.equal(await page.getByLabel("Notices").count(), 0, "no strip")
      assert.equal(await page.getByRole("alert").count(), 0, "no alert over the board")
      withUnifi("eu")
      daemon.set({ slots: new Map([["eu", { ...daemon.slot("eu"), status: "failed", downReason: "confirmedDead", appliedServer: null, server: null }]]) })
      await page.reload()
      const reason = eu.getByText("All servers offline", { exact: true })
      await reason.waitFor()
      assert.match(await reason.getAttribute("class"), /text-destructive/)
      assert.match(await dot(eu, "Failed").getAttribute("class"), /bg-destructive/, "the dot is as red as its words")
    } finally {
      await close()
    }
  })

  test("17. what UniFi couldn't do for one VPN Client stands on its box in amber Trouble words, a taken name with Rename, which opens the name; a client kept for its routes says so on its row; the view carries no sentences", async () => {
    const shown = { nameTaken: "Name taken", limitReached: "Limit reached", refused: "UniFi refused" }
    for (const [trouble, words] of Object.entries(shown)) {
      daemon.set({ vpnClients: { known: true, limit: 8, foreign: [{ id: "n-kept", name: "Old EU", enabled: true, trouble: "routed" }] } })
      withUnifi("eu", { trouble, client: trouble === "nameTaken" ? "present" : "none" })
      const { page, close } = await open("")
      try {
        const eu = tile(page, "EU")
        const said = eu.locator("[data-slot=slot-line]").getByText(words, { exact: true })
        await said.waitFor()
        assert.equal(await toneOf(said), "warn", `${trouble}: amber`)
        const fixes = await eu.locator("[data-slot=slot-line]").getByRole("button").allInnerTexts()
        assert.deepEqual(fixes, trouble === "nameTaken" ? ["Rename"] : [], `${trouble}: its fix where the page has one`)
        if (trouble === "nameTaken") {
          const fix = eu.getByRole("button", { name: "Rename", exact: true })
          assert.match(await fix.getAttribute("data-variant") ?? await fix.getAttribute("class"), /outline/, "the small outline button, as every fix's")
          await fix.click()
          const field = eu.getByRole("textbox", { name: "Name of EU" })
          await field.waitFor()
          assert.ok(await field.evaluate((el) => el === document.activeElement), "the name's field, focused")
          await page.keyboard.press("Escape")
        }
        const kept = board(page).getByRole("group", { name: "Old EU", exact: true })
        assert.equal(await toneOf(kept.getByText("Still routed", { exact: true })), "warn", "a kept client says what keeps it")
        assert.equal(await kept.getByText("Managed in UniFi").count(), 0)
        const body = await page.evaluate(async (url) => (await fetch(url + "api/state")).json(), daemon.url)
        assert.ok(!("warnings" in body), "the view has no warnings")
      } finally {
        await close()
      }
    }
  })

  /** Whether two parts of a card stand on one line: their boxes share a band of its height. */
  const sameLine = async (a, b) => {
    const [x, y] = [await a.boundingBox(), await b.boundingBox()]
    return x.y < y.y + y.height && y.y < x.y + x.height
  }
  const cardWords = async (page) => (await card(page).innerText()).replace(/\s+/g, " ").trim()
  const slotCreates = () => daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(BASE + "api/slots/"))

  for (const width of [1280, 390]) {
    test(`20. at ${width}, a missing or refused key is a mark beside the globe in red, whose card is one line, "UniFi", the trouble and Settings on it, opening Settings at UniFi; meanwhile Add can't create a slot and its card is the same card`, async () => {
      const { page, close } = await open("", { width })
      try {
        const header = page.locator("header")
        await header.getByRole("button", { name: "WAN up" }).waitFor()
        assert.equal(await header.getByRole("button", { name: /^UniFi/ }).count(), 0, "no mark while UniFi makes VPN Clients")
        await addSlot(page).waitFor()
        for (const [trouble, words] of [["noKey", "No API key"], ["keyRefused", "Key refused"]]) {
          daemon.set({ unifiTrouble: trouble })
          await page.goto(daemon.url)
          const mark = header.getByRole("button", { name: `UniFi: ${words}`, exact: true })
          await mark.waitFor()
          assert.equal((await mark.innerText()).trim(), "", `${trouble}: a mark with no words`)
          assert.match(await mark.locator("svg").getAttribute("class"), /text-destructive/, `${trouble}: in red`)
          const add = board(page).getByRole("button", { name: /^Add/ })
          assert.equal(await add.getAttribute("aria-disabled"), "true", `${trouble}: Add can't create a slot`)
          for (const [where, trigger] of [["Add", add], ["the mark", mark]]) {
            await trigger.scrollIntoViewIfNeeded()
            await trigger.click({ force: true })
            const settings = card(page).getByRole("link", { name: "Settings" })
            await settings.waitFor()
            assert.equal(await cardWords(page), `UniFi ${words} Settings`, `${trouble}, ${where}: its card names the trouble and holds Settings`)
            assert.ok(await sameLine(card(page).getByText("UniFi", { exact: true }), settings) && await sameLine(card(page).getByText(words, { exact: true }), settings), `${trouble}, ${where}: Settings on the card's one line`)
            if (where === "Add") {
              await page.keyboard.press("Escape")
              await card(page).waitFor({ state: "detached" })
            }
          }
          assert.equal(slotCreates().length, 0, `${trouble}: no slot was created`)
          await card(page).getByRole("link", { name: "Settings" }).click()
          await page.waitForURL(/\/settings#unifi$/)
          await page.locator("#unifi").getByText(words, { exact: true }).waitFor()
          assert.equal(await card(page).count(), 0, `${trouble}: the card went with the link`)
        }
        daemon.set({ unifiTrouble: null, vpnClients: clients() })
        await page.goto(daemon.url + "settings")
        await until(async () => (await page.locator("#unifi").locator("[data-slot=card-header]").innerText()).trim() === "UniFi", "UniFi accepted: the name alone, no trouble's words")
        assert.equal(await header.getByRole("button", { name: /^UniFi/ }).count(), 0, "the mark goes with the trouble")
      } finally {
        await close()
      }
    })

    test(`20. at ${width}, UniFi not answering is an amber mark whose card is a plain card of rows, "UniFi" "Not answering" and "Last answered" with the time, and no button; the Add box's card is the same`, async () => {
      const answered = new Date(Date.now() - 5 * 60e3).toISOString()
      daemon.set({ unifiTrouble: "notAnswering", unifiAcceptedAt: answered })
      const { page, close } = await open("", { width })
      try {
        const at = await page.evaluate((iso) => new Date(iso).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" }), answered)
        const mark = page.locator("header").getByRole("button", { name: `UniFi: Not answering. Last answered ${at}`, exact: true })
        await mark.waitFor()
        assert.equal((await mark.innerText()).trim(), "", "a mark with no words")
        assert.match(await mark.locator("svg").getAttribute("class"), /text-warning/, "in amber")
        const add = board(page).getByRole("button", { name: /^Add/ })
        assert.equal(await add.getAttribute("aria-disabled"), "true", "Add can't create a slot")
        for (const [where, trigger] of [["the mark", mark], ["Add", add]]) {
          await trigger.scrollIntoViewIfNeeded()
          await trigger.click({ force: true })
          await until(async () => (await cardWords(page).catch(() => "")) === `UniFi Not answering Last answered ${at}`, `${where}: its card is the trouble and when UniFi last answered`)
          assert.deepEqual(await card(page).locator("[data-slot=row] > span").allInnerTexts(), ["UniFi", "Not answering", "Last answered", at], `${where}: two rows, names and values`)
          assert.equal(await card(page).locator("button, a").count(), 0, `${where}: no button, since Settings fixes nothing`)
          await page.keyboard.press("Escape")
          await card(page).waitFor({ state: "detached" })
        }
        assert.equal(slotCreates().length, 0, "no slot was created")
        daemon.set({ unifiAcceptedAt: null })
        await page.goto(daemon.url)
        const unanswered = page.locator("header").getByRole("button", { name: "UniFi: Not answering", exact: true })
        await unanswered.click()
        await until(async () => (await cardWords(page).catch(() => "")) === "UniFi Not answering", "UniFi unanswered since the daemon started: its card is the trouble alone")
      } finally {
        await close()
      }
    })
  }

  test("20. a failed read's card is one line, its reason and Retry on it, as UniFi's; a card of several rows keeps its action under them", async () => {
    seedTroubled()
    const pasted = daemon.source("pasted")
    daemon.set({ readFails: { "api/servers": 10 }, sources: new Map(daemon.sources().map((x) => [x.id, x.id === "pasted" ? { ...pasted, diagnostics: [{ entry: 2, code: "malformed_entry", field: "" }] } : x])) })
    const { page, close } = await open("sources/pasted", { width: 1280 })
    try {
      await nav(page).getByRole("button", { name: "All servers: HTTP 502", exact: true }).click()
      const retry = card(page).getByRole("button", { name: "Retry" })
      await retry.waitFor()
      assert.equal(await cardWords(page), "HTTP 502 Retry")
      assert.ok(await sameLine(card(page).getByText("HTTP 502", { exact: true }), retry), "Retry on the reason's line")
      await page.keyboard.press("Escape")
      await card(page).waitFor({ state: "detached" })
      await page.getByRole("button", { name: "1 entry skipped. Entry 2: Malformed" }).click()
      const paste = card(page).getByRole("button", { name: "Paste again" })
      await paste.waitFor()
      const rows = await card(page).locator("[data-slot=row]").evaluateAll((all) => all.flatMap((r) => [...r.children].map((c) => c.getBoundingClientRect().bottom)))
      assert.ok((await paste.boundingBox()).y >= Math.max(...rows), "Paste again under the rows")
    } finally {
      await close()
    }
  })

  test("23. a card that holds an action is a dialog named by its trigger's label: UniFi's trouble, a failed read's and Skipped's", async () => {
    seedTroubled()
    const pasted = daemon.source("pasted")
    daemon.set({ unifiTrouble: "keyRefused", readFails: { "api/servers": 10 }, sources: new Map(daemon.sources().map((x) => [x.id, x.id === "pasted" ? { ...pasted, diagnostics: [{ entry: 2, code: "malformed_entry", field: "" }] } : x])) })
    const { page, close } = await open("sources/pasted", { width: 1280 })
    try {
      for (const name of ["UniFi: Key refused", "All servers: HTTP 502", "1 entry skipped. Entry 2: Malformed"]) {
        await page.getByRole("button", { name, exact: true }).click()
        await page.getByRole("dialog", { name, exact: true }).waitFor({ timeout: 2000 })
        await page.keyboard.press("Escape")
        await card(page).waitFor({ state: "detached" })
      }
    } finally {
      await close()
    }
  })

  test("23. in Chromium, an action card closed by Esc gives the focus back to its trigger only while nothing else has taken it: focus moved at once to the Add box keeps there, and Enter opens its card", async () => {
    daemon.set({ unifiTrouble: "keyRefused" })
    const { page, close } = await open("", { width: 1280 })
    try {
      const mark = page.locator("header").getByRole("button", { name: "UniFi: Key refused", exact: true })
      const add = board(page).getByRole("button", { name: /^Add/ })
      await add.waitFor()
      const focused = () => page.evaluate(() => document.activeElement?.getAttribute("aria-label") ?? document.activeElement?.tagName)
      await mark.click()
      await card(page).getByRole("link", { name: "Settings" }).waitFor()
      await page.keyboard.press("Escape")
      await card(page).waitFor({ state: "detached" })
      await until(async () => (await focused()) === "UniFi: Key refused", "nothing took the focus: it is back on the mark")
      await mark.click()
      await card(page).getByRole("link", { name: "Settings" }).waitFor()
      await card(page).evaluate((wrapper, box) => {
        const watch = new MutationObserver(() => { if (!wrapper.isConnected) { watch.disconnect(); box.focus() } })
        watch.observe(document.body, { childList: true, subtree: true })
      }, await add.elementHandle())
      await page.keyboard.press("Escape")
      await card(page).waitFor({ state: "detached" })
      await page.waitForTimeout(100)
      assert.equal(await focused(), "Add: UniFi Key refused", "the focus stays on the Add box that took it as the card went")
      await page.keyboard.press("Enter")
      await until(async () => (await cardWords(page).catch(() => "")) === "UniFi Key refused Settings", "Enter opens the Add box's card")
      assert.equal(slotCreates().length, 0, "and creates no slot")
    } finally {
      await close()
    }
  })

  for (const [wan, words, said, colour] of [["up", "WAN up", "Up", /text-success/], ["down", "WAN down", "Down", /text-destructive/], ["unknown", "WAN unknown", "Unknown", /text-muted-foreground/]]) {
    test(`17. the top bar shows ${words} as an icon with no text`, async () => {
      daemon.set({ wan })
      const { page, close } = await open("")
      try {
        const icon = page.locator("header").getByRole("button", { name: words, exact: true })
        await icon.waitFor()
        assert.equal((await icon.innerText()).trim(), "")
        assert.match(await icon.locator("svg").getAttribute("class"), colour)
        await icon.hover()
        await until(async () => (await card(page).innerText().catch(() => "")).replace(/\s+/g, " ") === `WAN ${said}`, "its card, the page's Card, says it")
      } finally {
        await close()
      }
    })
  }
})

describe("Events", () => {
  const at = new Date(Date.now() - 2 * 3600e3).toISOString()
  const event = (id, reason, evidence) => ({
    id, owner: { kind: "slot", slot: "eu" }, slots: ["eu"], intent: 1, from: { kind: "up", server: "l-ams" }, to: { kind: "up", server: "l-fra" }, reason, requestedAt: at, appliedAt: at, evidence,
  })
  const probe = { server: "l-ams", kind: "activeProbe", verdict: "failed", target: "gstatic-204", round: 1, at, pingMs: null, newConnectionMs: null }

  test("17. a switch with no signals has no chevron and doesn't open, and the slot's name is plain text", async () => {
    daemon.set({ switches: [event("s1", "exitChanged", []), event("s2", "failureMove", [probe])] })
    const { page, close } = await open("events")
    try {
      const row = (words) => page.locator("li").filter({ hasText: words })
      await row("Exit changed").waitFor()
      assert.equal(await row("Exit changed").getByRole("button").count(), 0, "nothing to open")
      assert.equal(await row("Exit changed").locator("svg.lucide-chevron-down").count(), 0, "no chevron")
      assert.doesNotMatch(await page.locator("ol").innerText(), /No signals/)
      await row("Exit changed").click()
      assert.equal(await page.getByRole("columnheader", { name: "Signal" }).count(), 0, "a click opens nothing")
      assert.equal(await row("Server failed").locator("svg.lucide-chevron-down").count(), 1)
      const slot = row("Exit changed").getByText("EU", { exact: true })
      const look = () => slot.evaluate((el) => { const s = getComputedStyle(el); return [s.color, s.textDecorationLine] })
      const before = await look()
      await slot.hover()
      assert.deepEqual(await look(), before, "no underline or colour change on hover")
      assert.equal(before[1], "none")
      await row("Server failed").getByRole("button", { name: "Server failed" }).click()
      await page.getByRole("columnheader", { name: "Signal" }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. a switch a person made reads Manual beside the muted circle and names every slot it moved", async () => {
    const eu = daemon.slot("eu")
    daemon.set({
      slots: new Map([["eu", eu], ["home", { ...eu, id: "home", name: "Home" }]]),
      switches: [{ ...event("s1", "preferred", []), owner: { kind: "group", group: "all-aurora" }, slots: ["eu", "home"] }, event("s2", "failureMove", [probe])],
    })
    const { page, close } = await open("events")
    try {
      const row = page.locator("li").filter({ hasText: "Manual" })
      await row.getByText("EU and Home", { exact: true }).waitFor()
      assert.equal(await row.locator(".text-muted-foreground svg.lucide-circle").count(), 1, "the muted circle Exit changed has")
      assert.equal(await page.locator("li").filter({ hasText: "Server failed" }).getByText("EU", { exact: true }).count(), 1, "a slot's own move names it alone")
    } finally {
      await close()
    }
  })

  test("17. a server a refresh kept across a changed connection reads Server replaced, its source, and its names, and nothing under them", async () => {
    const at = new Date(Date.now() - 3600e3).toISOString()
    daemon.set({ replaced: [{ source: "aurora", server: "l-fra", label: "Frankfurt", previousLabel: "Frankfurt old", at }, { source: "aurora", server: "l-ams", label: "Amsterdam", previousLabel: "Amsterdam", at }] })
    const { page, close } = await open("events")
    try {
      const rows = page.locator("li").filter({ hasText: "Server replaced" })
      await rows.first().waitFor()
      assert.deepEqual((await rows.allInnerTexts()).map((t) => t.replace(/\s+/g, " ").trim()), ["Server replaced Aurora Frankfurt old Frankfurt 1h ago", "Server replaced Aurora Amsterdam 1h ago"])
    } finally {
      await close()
    }
  })

  test("17. while the server list is unread a move's servers are Pending in their shape, a flag's place and a name's, never Unknown server", async () => {
    daemon.set({ switches: [event("s1", "failureMove", [probe])] })
    const { page, release, close } = await openHolding("events", "api/servers")
    try {
      const row = page.locator("li").filter({ hasText: "Server failed" })
      await row.waitFor()
      assert.equal(await row.getByRole("status", { name: "Loading servers…" }).count(), 2, "from and to, each in its shape")
      assert.equal(await row.locator("[data-slot=skeleton]").count(), 4, "a flag's place and a name's for each")
      assert.doesNotMatch(await page.locator("main").innerText(), /Unknown server/)
      await release()
      await row.getByText("Amsterdam").waitFor()
      assert.equal(await row.getByRole("status", { name: "Loading servers…" }).count(), 0)
    } finally {
      await close()
    }
  })

  test("17. once the server list's read has failed a move's servers are the muted failed mark, whose card holds the reason and Retry, never Unknown server", async () => {
    daemon.set({ switches: [event("s1", "failureMove", [probe])], readFails: { "api/servers": 10 } })
    for (const width of [1280, 390]) {
      const { page, close } = await open("events", { width })
      try {
        const row = page.locator("li").filter({ hasText: "Server failed" })
        const marks = row.getByRole("button", { name: "Server: HTTP 502", exact: true })
        await until(async () => (await marks.count()) === 2, `${width} px: from and to, each the failed mark`)
        assert.doesNotMatch(await page.locator("main").innerText(), /Unknown server/)
        assert.equal(await toneOf(marks.first()), "muted", `${width} px: muted, as every failed read's mark`)
        await marks.first().click()
        const why = card(page)
        await why.getByRole("button", { name: "Retry" }).waitFor()
        assert.equal((await why.innerText()).replace(/\s+/g, " ").trim(), "HTTP 502 Retry", `${width} px: its card holds the reason and Retry`)
        assert.equal(await row.getByRole("table").count(), 0, `${width} px: the click opened the card, not the move's signals`)
        if (width === 390) {
          daemon.set({ readFails: {} })
          await why.getByRole("button", { name: "Retry" }).click()
          await row.getByText("Amsterdam").first().waitFor()
          assert.equal(await marks.count(), 0, "Retry reads the list, and the names stand")
        }
      } finally {
        await close()
      }
    }
  })

  test("17. a mark keeps its colour in every state of the menu item it stands in: a reason's mark in the Reason menu at rest, under the pointer and under the keyboard", async () => {
    daemon.set({ switches: [event("s1", "exitChanged", []), event("s2", "failureMove", [probe])] })
    const { page, close } = await open("events")
    try {
      await page.getByRole("combobox", { name: "Reason" }).click()
      const option = page.getByRole("option", { name: "Server failed" })
      const mark = option.locator("svg").first()
      await mark.waitFor()
      assert.equal(await toneOf(mark), "bad", "at rest")
      await option.hover()
      await until(async () => (await option.getAttribute("data-highlighted")) !== null, "highlighted under the pointer")
      assert.equal(await toneOf(mark), "bad", "under the pointer")
      /* The keyboard's highlight is read with the pointer off the list, so no pointer move can take the highlight back. */
      const lit = () => page.locator("[role=option][data-highlighted]").evaluateAll((els) => els.map((el) => el.textContent).join())
      await page.mouse.move(0, 0)
      await until(async () => (await lit()) === "", "the pointer's highlight gone with the pointer")
      while ((await option.getAttribute("data-highlighted")) === null) {
        const was = await lit()
        assert.notEqual(was, "UDP broken", "the keyboard passes Server failed")
        await page.keyboard.press("ArrowDown")
        await until(async () => (await lit()) !== was, "the keyboard moves the highlight")
      }
      assert.equal(await toneOf(mark), "bad", "under the keyboard")
    } finally {
      await close()
    }
  })

  test("17. a signal's columns show wherever they fit, measured, and the Result's card holds only those that don't", async () => {
    const shown = { server: "l-ams", kind: "activeProbe", verdict: "succeeded", target: "gstatic-204", round: 2, at, pingMs: 38, newConnectionMs: 120 }
    daemon.set({ switches: [event("s1", "failureMove", [shown])] })
    const ALL = ["Signal", "Server", "Target", "Round", "Ping", "Connect", "Result", "At"]
    const { page, close } = await open("events", { width: 1280 })
    try {
      for (const [width, headers, carded] of [[1280, ALL, []], [390, ["Server", "Result", "At"], ["Signal Check", "Target gstatic-204", "Round 2", "Ping 38 ms", "Connect 120 ms"]]]) {
        await page.setViewportSize({ width, height: 900 })
        await page.goto(daemon.url + "events")
        await page.getByRole("button", { name: "Server failed" }).click()
        const table = page.locator("li table")
        await table.waitFor()
        /* It is measured when the browser next lays the page out, before it paints. */
        await page.evaluate(() => new Promise((painted) => requestAnimationFrame(() => requestAnimationFrame(painted))))
        assert.deepEqual(await table.getByRole("columnheader").allInnerTexts(), headers, `at ${width} px`)
        assert.ok(await table.evaluate((el) => el.parentElement.scrollWidth <= el.parentElement.clientWidth), `at ${width} px the table fits without scrolling`)
        const result = table.getByRole("cell").filter({ hasText: "Passed" })
        if (carded.length === 0) {
          assert.equal(await result.getByRole("button").count(), 0, `at ${width} px the Result has no card, since nothing is hidden`)
          continue
        }
        await result.getByRole("button").click()
        await until(async () => (await card(page).innerText().catch(() => "")).split("\n").map((l) => l.trim()).filter(Boolean).join(" ") === carded.join(" "), `at ${width} px its card holds what the table hides`)
      }
    } finally {
      await close()
    }
  })
})

describe("Settings", () => {
  const retention = (page, word) => page.getByRole("radiogroup", { name: "Retention" }).getByRole("radio", { name: word, exact: true })
  const picked = async (page) => (await page.getByRole("radiogroup", { name: "Retention" }).getByRole("radio", { checked: true }).innerText()).trim()
  const settingsSave = (page) => page.locator("button[data-control=settings]")
  const settingsRead = (page) => until(async () => (await picked(page)) === "30d", "the settings form")

  test("17. an earlier key's refusal that arrives after another key was typed is a toast, and the typed key stays saveable", async () => {
    daemon.set({ unifiKeyAnswer: "keyRefused" })
    const { page, close } = await open("settings", { clock: true })
    try {
      const part = page.locator("#unifi")
      const field = part.getByLabel("API key")
      const save = part.getByRole("button", { name: "Save", exact: true })
      let cut = true
      await page.route("**/api/unifi/key", (r) => (cut ? r.abort() : r.continue()))
      await field.fill("k3y-one")
      await save.click()
      await page.clock.fastForward(31_000)
      const said = toast(page, "timed out")
      await said.waitFor()
      await until(async () => (await field.getAttribute("readonly")) === null, "the field the reader's again")
      await field.fill("k3y-two")
      cut = false
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => requests("PUT", "api/unifi/key").some((r) => r.status === 422), "the daemon refuses the first key")
      const refused = page.locator("[data-sonner-toast]").filter({ hasText: /^API key/ }).filter({ hasNotText: "timed out" })
      await refused.waitFor()
      assert.ok(!(await refused.innerText()).includes("k3y"), "the toast names no key")
      assert.equal(await field.inputValue(), "k3y-two")
      assert.equal(await field.getAttribute("aria-invalid"), null, "the typed key isn't the refused one")
      assert.equal(await save.isEnabled(), true)
    } finally {
      await close()
    }
  })

  /** Key `k3y-one` gets no answer, the field unlocks, `between` runs, then `k3y-two` is typed and the first key's Retry is pressed,
   * which UniFi `answer`s. Returns the field, its Save and the toast that refusal is. */
  async function earlierKeyRetried(page, answer, between = async () => {}) {
    const part = () => page.locator("#unifi")
    let cut = true
    await page.route("**/api/unifi/key", (r) => (cut ? r.abort() : r.continue()))
    await part().getByLabel("API key").fill("k3y-one")
    await part().getByRole("button", { name: "Save", exact: true }).click()
    await page.clock.fastForward(31_000)
    const said = toast(page, "timed out")
    await said.waitFor()
    await until(async () => (await part().getByLabel("API key").getAttribute("readonly")) === null, "the field the reader's again")
    await between()
    await part().getByLabel("API key").fill("k3y-two")
    daemon.set({ unifiKeyAnswer: answer })
    cut = false
    await said.getByRole("button", { name: "Retry" }).click()
    await until(() => requests("PUT", "api/unifi/key").some((r) => r.status === 422), "the daemon refuses the first key")
    const refused = page.locator("[data-sonner-toast]").filter({ hasText: /^API key/ }).filter({ hasNotText: "timed out" })
    return { field: part().getByLabel("API key"), save: part().getByRole("button", { name: "Save", exact: true }), refused, line: part().locator("[data-slot=field-error]") }
  }

  test("17. an earlier key's refusal after Settings were left and opened again, another key typed, is a toast, and the typed key stays saveable", async () => {
    const { page, close } = await open("settings", { clock: true })
    try {
      const { field, save, refused, line } = await earlierKeyRetried(page, "keyRefused", async () => {
        await nav(page).getByRole("link", { name: /All servers/ }).click()
        await page.waitForURL(/\/servers$/)
        await nav(page).getByRole("link", { name: "Settings" }).click()
        await page.waitForURL(/\/settings$/)
      })
      await refused.waitFor()
      assert.ok(!(await refused.innerText()).includes("k3y"), "the toast names no key")
      assert.equal(await field.inputValue(), "k3y-two")
      assert.equal(await field.getAttribute("aria-invalid"), null, "the typed key isn't the refused one")
      assert.equal(await line.count(), 0)
      assert.equal(await save.isEnabled(), true)
    } finally {
      await close()
    }
  })

  test("17. UniFi's silence over an earlier key, sent after another was typed, is a toast and no amber line under the key typed", async () => {
    const { page, close } = await open("settings", { clock: true })
    try {
      const { field, save, refused, line } = await earlierKeyRetried(page, "notAnswering")
      await refused.waitFor()
      assert.ok(!(await refused.innerText()).includes("k3y"), "the toast names no key")
      assert.equal(await field.inputValue(), "k3y-two")
      assert.equal(await line.count(), 0, "no line under a key that wasn't sent")
      assert.equal(await save.isEnabled(), true)
    } finally {
      await close()
    }
  })

  for (const answer of ["keyRefused", "notAnswering"]) {
    test(`17. a key's ${answer} the view must catch up on, Settings left meanwhile, is a toast, and Settings opened again show no line`, async () => {
      daemon.set({ unifiKeyAnswer: answer })
      const { page, close } = await open("settings")
      try {
        const part = page.locator("#unifi")
        await part.getByLabel("API key").fill("k3y-one")
        const release = daemon.holdReads("api/state")
        daemon.changeSlot("eu", { name: "EU 2" }, { announce: false })
        await part.getByRole("button", { name: "Save", exact: true }).click()
        await until(() => requests("PUT", "api/unifi/key").some((r) => r.status === 422), "UniFi's answer, at a revision the view hasn't reached")
        assert.equal(await part.getByRole("button", { name: "Save", exact: true }).getAttribute("aria-busy"), "true", "the field waits for the view")
        await nav(page).getByRole("link", { name: /All servers/ }).click()
        await page.waitForURL(/\/servers$/)
        release()
        const said = page.locator("[data-sonner-toast]").filter({ hasText: /^API key/ })
        await said.waitFor()
        assert.ok(!(await said.innerText()).includes("k3y"), "the toast names no key")
        await nav(page).getByRole("link", { name: "Settings" }).click()
        await page.waitForURL(/\/settings$/)
        await part.getByLabel("API key").waitFor()
        assert.equal(await part.locator("[data-slot=field-error]").count(), 0, "no line under a field that holds no key")
        assert.equal(await part.getByLabel("API key").inputValue(), "")
      } finally {
        await close()
      }
    })
  }

  test("17. a save whose conflict leaves no difference is no conflict: another writer stored the same Retention, and the form settles with no toast", async () => {
    const { page, close } = await open("settings")
    try {
      await settingsRead(page)
      await retention(page, "90d").click()
      let theirs = false
      await page.route("**/api/settings", async (r) => {
        if (r.request().method() === "PUT" && !theirs) {
          theirs = true
          daemon.changeSettings({ analyticsRetentionDays: 90 })
        }
        await r.continue()
      })
      await settingsSave(page).click()
      await until(() => requests("PUT", "api/settings").some((r) => r.status === 412), "the daemon finds another writer's")
      await until(async () => (await settingsSave(page).getAttribute("aria-busy")) === null, "the save ends")
      await page.waitForTimeout(300)
      assert.equal(await toast(page, "").count(), 0, "nothing to say")
      assert.equal(await settingsSave(page).isDisabled(), true, "nothing left to save")
      assert.equal(await picked(page), "90d")
      assert.equal(requests("PUT", "api/settings").length, 1)
    } finally {
      await close()
    }
  })

  test("17. a save with no answer for 30 s ends with the toast's Retry, the form editable again, and Retry sends the same save under its key, keeping what was picked since", async () => {
    const { page, close } = await open("settings", { clock: true })
    try {
      await settingsRead(page)
      let down = true
      await page.route("**/api/settings", (r) => (r.request().method() === "PUT" && down ? r.abort() : r.continue()))
      await retention(page, "90d").click()
      await settingsSave(page).click()
      assert.equal(await settingsSave(page).getAttribute("aria-busy"), "true", "Save is Pending")
      assert.equal(await retention(page, "7d").isDisabled(), true, "the form takes no pick while it is out")
      await page.clock.fastForward(31_000)
      const said = toast(page, "timed out")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Settings timed out Retry")
      await retention(page, "7d").click()
      down = false
      await said.getByRole("button", { name: "Retry" }).click()
      await until(() => daemon.settings().analyticsRetentionDays === 90, "Retry lands the save, without what was picked since")
      assert.equal(new Set(requests("PUT", "api/settings").map((r) => r.idempotencyKey)).size, 1, "under the key every attempt had")
      await until(async () => (await settingsSave(page).isEnabled()) && (await picked(page)) === "7d", "what was picked since stays, to be saved")
    } finally {
      await close()
    }
  })

  test("17. after no answer, an unchanged Retention Save over another writer's change is a new write at their revision", async () => {
    const { page, close } = await open("settings", { clock: true })
    try {
      await settingsRead(page)
      let drop = true
      const sent = []
      await page.route("**/api/settings", (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        sent.push({ key: r.request().headers()["idempotency-key"], ifMatch: r.request().headers()["if-match"] })
        return drop ? r.abort() : r.continue()
      })
      await retention(page, "90d").click()
      await settingsSave(page).click()
      await page.clock.fastForward(31_000)
      await toast(page, "timed out").waitFor()
      daemon.changeSettings({ analyticsRetentionDays: 7 })
      await readsAnswered(page, "api/settings")
      drop = false
      await settingsSave(page).click()
      await until(() => daemon.settings().analyticsRetentionDays === 90, "the pick stored over theirs")
      assert.notEqual(sent.at(-1).key, sent[0].key, "a new write")
      assert.deepEqual([sent[0].ifMatch, sent.at(-1).ifMatch], ['"5"', '"6"'], "at their revision")
    } finally {
      await close()
    }
  })

  test("17. Retention left after its Save's no answer finds its pick and Request timed out when Settings open again; left after a 429 it is what is stored", async () => {
    const { page, close } = await open("settings", { clock: true })
    try {
      await settingsRead(page)
      let answer = "none"
      await page.route("**/api/settings", (r) => (r.request().method() !== "PUT" ? r.continue() : answer === "none" ? r.abort()
        : r.fulfill({ status: 429, contentType: "text/html", body: "<h1>429 Too Many Requests</h1>" })))
      await retention(page, "90d").click()
      await settingsSave(page).click()
      await page.clock.fastForward(31_000)
      await toast(page, "timed out").waitFor()
      await nav(page).getByRole("link", { name: "Events" }).click()
      await page.waitForURL(/\/events$/)
      await page.clock.fastForward(1_000)
      await nav(page).getByRole("link", { name: "Settings" }).click()
      await until(async () => (await picked(page)) === "90d", "the pick kept")
      await page.getByText("Request timed out", { exact: true }).waitFor()

      await retention(page, "7d").click()
      answer = "429"
      await settingsSave(page).click()
      await toast(page, "HTTP 429").waitFor()
      await nav(page).getByRole("link", { name: "Events" }).click()
      await page.waitForURL(/\/events$/)
      await page.clock.fastForward(1_000)
      await nav(page).getByRole("link", { name: "Settings" }).click()
      await until(async () => (await picked(page)) === "30d", "a 429 reached nothing, so leaving drops the pick")
    } finally {
      await close()
    }
  })

  test("17. a form left while its save is out, then refused, shows its typing when opened again, and the refusal is its toast", async () => {
    const { page, close } = await open("settings")
    try {
      await settingsRead(page)
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/settings", async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        await held
        await r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", reason: "retentionRange", message: "Retention is too long." }) })
      })
      await retention(page, "90d").click()
      await settingsSave(page).click()
      await nav(page).getByRole("link", { name: "Events" }).click()
      await page.waitForURL(/\/events$/)
      release()
      const said = toast(page, "1d to 365d")
      await said.waitFor()
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Settings 1d to 365d")
      await nav(page).getByRole("link", { name: "Settings" }).click()
      await until(async () => (await picked(page)) === "90d", "the pick kept where the toast points")
      assert.equal(await settingsSave(page).isEnabled(), true)
      assert.equal(daemon.settings().analyticsRetentionDays, 30, "nothing stored")
    } finally {
      await close()
    }
  })

  test("17. About has no Revision line", async () => {
    const { page, close } = await open("settings")
    try {
      await page.getByText("Xray", { exact: true }).waitFor()
      assert.equal(await page.getByText("Revision", { exact: true }).count(), 0)
    } finally {
      await close()
    }
  })

  test("17. About names sidegate's version as the build says it, a release's tag or a checkout's, and a failed read of it is the cross whose card retries", async () => {
    const about = (page) => page.locator("main [data-slot=card]").filter({ has: page.getByText("About", { exact: true }) })
    for (const version of ["v0.1.0", "dev-7990743"]) {
      daemon.set({ version, release: version === "v0.1.0" ? "0123456789ab" : null })
      const { page, close } = await open("settings")
      try {
        const shown = about(page).getByText(version, { exact: true })
        await shown.waitFor()
        assert.equal(await shownWords(shown.locator("xpath=ancestor::*[@data-slot='update-line']/..")), `sidegate ${version}`)
        assert.equal(await toneOf(shown), "plain")
      } finally {
        await close()
      }
    }
    daemon.set({ version: "v0.1.0", release: "0123456789ab", readFails: { "api/update": 1 } })
    const { page, close } = await open("settings", { noCheck: true })
    try {
      await about(page).getByRole("button", { name: "Version: HTTP 502" }).click()
      await page.locator("[data-radix-popper-content-wrapper]").getByRole("button", { name: "Retry" }).click()
      await about(page).getByText("v0.1.0", { exact: true }).waitFor()
    } finally {
      await close()
    }
  })

  test("17. About draws the Xray that runs in the same ink whether or not it is the release's, and an older one's release muted", async () => {
    const inks = {}
    for (const running of ["26.10.31", "25.12.8"]) {
      daemon.set({ core: { version: "26.10.31", running } })
      const { page, close } = await open("settings")
      try {
        const about = page.locator("main [data-slot=card]").filter({ has: page.getByText("About", { exact: true }) })
        const runs = about.getByText(`v${running}`).last()
        await runs.waitFor()
        inks[running] = await toneOf(runs)
        if (running !== "26.10.31") {
          assert.equal(await toneOf(about.getByText("v26.10.31").last()), "muted", "the release's version, known and not in effect")
          assert.equal(await shownWords(runs.locator("..")), "v25.12.8 v26.10.31")
          assert.equal((await runs.locator("..").textContent()).replace(/\s+/g, " "), "Running v25.12.8, the release ships v26.10.31")
        }
      } finally {
        await close()
      }
    }
    assert.deepEqual(inks, { "26.10.31": "plain", "25.12.8": "plain" })
  })
})

describe("Updates", () => {
  const FOUND = { version: "v0.2.0", restartsCore: false }
  const ago = (hours) => new Date(Date.now() - hours * 3600e3).toISOString()
  /** The updater as a check made lately left it: nothing is fetched as the page loads. */
  const updater = (update = {}) => daemon.set({ latest: "v0.2.0", update: { startedAt: ago(0.1), check: { at: ago(0.1), failure: null }, found: null, running: null, pressedAt: null, last: null, ...update } })
  const last = (outcome, reason = null, version = "v0.2.0") => ({ attempt: "0123456789abcdef", version, at: ago(0.05), outcome, reason, message: "for the log" })
  const line = (page) => page.locator("[data-slot=update-line]")
  const button = (page, name) => line(page).getByRole("button", { name, exact: true })
  const updateButton = (page) => button(page, "Update")
  const checkButton = (page) => button(page, "Check for updates")
  const confirm = (page) => page.getByRole("alertdialog", { name: "Update to v0.2.0?" })
  const mark = (page) => page.getByRole("img", { name: "Update to v0.2.0" })
  const card = (page) => page.locator("[data-radix-popper-content-wrapper]")
  const presses = () => requests("POST", "api/update")
  const checks = () => requests("POST", "api/update/check")
  const retention = (page, word) => page.getByRole("radiogroup", { name: "Retention" }).getByRole("radio", { name: word, exact: true })
  const picked = async (page) => (await page.getByRole("radiogroup", { name: "Retention" }).getByRole("radio", { checked: true }).innerText()).trim()
  const unplugged = (page) => page.getByRole("button", { name: /^Gateway unreachable/ })
  const loads = (page) => page.evaluate(() => performance.getEntriesByType("navigation")[0].type)
  async function press(page) {
    await updateButton(page).click()
    await confirm(page).getByRole("button", { name: "Update", exact: true }).click()
    await confirm(page).waitFor({ state: "detached" })
  }
  /** The daemon stops until the page has noticed, and comes back as `build`, whatever the page then does. */
  async function restart(page, build = {}) {
    daemon.stop()
    await unplugged(page).waitFor()
    daemon.set(build)
    daemon.start()
  }
  const NEW = { version: "v0.2.0", release: "ba9876543210" }

  test("17. each load of the page makes one automatic check, on Dashboard too, with both tokens and no key, and the daemon keeps it to one a day and to the box", async () => {
    daemon.set({ latest: "v0.2.0", unifi: "unifi-1" })
    const { page, close } = await open("")
    try {
      await mark(page).first().waitFor()
      assert.deepEqual(checks().map(({ body, csrf, unifiCsrf, idempotencyKey, status }) => ({ body, csrf, unifiCsrf, idempotencyKey, status })), [{ body: '{"auto":true}', csrf: "token-1", unifiCsrf: "unifi-1", idempotencyKey: null, status: 200 }])
      daemon.set({ latest: "v0.3.0" })
      await page.reload()
      await mark(page).first().waitFor()
      await until(() => checks().length === 2, "the next load's own check")
      await page.waitForTimeout(300)
      assert.equal(await mark(page).count(), 1, "within the day nothing is fetched")
      assert.equal(checks().length, 2)
      assert.equal(presses().length, 0)
    } finally {
      await close()
    }
  })

  test("17. a newer release is the mark on Settings, at 390 on Menu too beside the outage dot, and About's arrow, notes and Update in place of Check; the mark adds no word", async () => {
    updater({ found: FOUND })
    const aurora = daemon.source("aurora")
    daemon.set({ sources: new Map([["aurora", { ...aurora, origin: { ...aurora.origin, provider: { ...aurora.origin.provider, quota: { ...aurora.origin.provider.quota, expires: ago(-71) } } } }]]) })
    for (const width of [1280, 390]) {
      const { page, close } = await open("settings", { width, touch: width < 768 })
      try {
        await updateButton(page).waitFor()
        assert.equal(await shownWords(line(page)), "v0.1.0 v0.2.0 Update")
        assert.equal((await line(page).textContent()).replace(/\s+/g, " "), "Running v0.1.0, newer v0.2.0Update")
        assert.equal(await toneOf(line(page).getByText("v0.2.0")), "muted")
        assert.equal(await line(page).getByRole("link", { name: "Notes for v0.2.0" }).getAttribute("href"), "https://example.test/OWNER/sidegate/releases/tag/v0.2.0")
        assert.equal(await checkButton(page).count(), 0, "no check while one is offered")
        const size = await updateButton(page).boundingBox()
        const room = await line(page).boundingBox()
        if (width < 768) {
          assert.deepEqual([size.height, size.width], [44, room.width], "a line of its own, 44 px tall")
          const menu = page.getByRole("button", { name: "Menu, update to v0.2.0" })
          const [disc, dot] = await menu.evaluate((b) => ["svg[role=img]", "span.size-1\\.5"].map((q) => b.querySelector(q).getBoundingClientRect().toJSON()))
          assert.ok(dot.bottom <= disc.top || dot.left >= disc.right, `the dot and the mark don't meet: ${JSON.stringify([dot, disc])}`)
          await menu.tap()
          await page.getByRole("dialog").getByRole("link", { name: "Settings" }).getByRole("img", { name: "Update to v0.2.0" }).waitFor()
        } else {
          assert.ok(size.height === 24 && room.height === 28, `Update's ${size.height} px keeps the line's 28: ${room.height}`)
          const entry = nav(page).getByRole("link", { name: "Settings" })
          assert.equal(await entry.getByRole("img", { name: "Update to v0.2.0" }).count(), 1)
          assert.equal((await entry.innerText()).trim(), "Settings")
          await nav(page).getByRole("link", { name: "Dashboard" }).click()
          await tile(page, "EU").waitFor()
          assert.equal(await mark(page).count(), 1, "on every tab")
        }
      } finally {
        await close()
      }
    }
  })

  test("17. Update asks once, Cancel focused; Cancel and Escape send nothing and give the focus back, and only an update that restarts Xray has the line", async () => {
    for (const restartsCore of [false, true]) {
      daemon.reset()
      updater({ found: { ...FOUND, restartsCore } })
      const { page, close } = await open("settings")
      try {
        for (const leave of ["Cancel", "Escape"]) {
          await updateButton(page).click()
          await confirm(page).waitFor()
          assert.equal(await shownWords(confirm(page)), restartsCore ? "Update to v0.2.0? All slots will disconnect briefly. Cancel Update" : "Update to v0.2.0? Cancel Update")
          assert.equal(await page.evaluate(() => document.activeElement?.textContent), "Cancel")
          await page.keyboard.press(leave === "Cancel" ? "Enter" : "Escape")
          await confirm(page).waitFor({ state: "detached" })
          await until(() => updateButton(page).evaluate((b) => document.activeElement === b), "the focus back on Update")
        }
        assert.equal(presses().length, 0)
        await press(page)
        await until(() => presses().length === 1, "the press")
        const [{ body, csrf, idempotencyKey, status }] = presses()
        assert.deepEqual([JSON.parse(body), csrf, idempotencyKey, status], [{ version: "v0.2.0", restartsCore }, "token-1", null, 202])
      } finally {
        await close()
      }
    }
  })

  test("17. while an update runs Update stays, disabled, its spinner in its icon's place, on whichever tab it is come back to, and the view is read every 2 s; at rest it isn't", async () => {
    updater({ found: FOUND })
    const { page, close } = await open("settings", { clock: true })
    const reads = () => requests("GET", "api/update").length
    try {
      await updateButton(page).waitFor()
      const rest = reads()
      await page.clock.fastForward(10_000)
      await page.waitForTimeout(300)
      assert.equal(reads(), rest, "nothing runs, so nothing is read again")
      await press(page)
      const running = async () => [await updateButton(page).isDisabled(), await updateButton(page).getAttribute("aria-busy"), await updateButton(page).locator("svg").count(), await shownWords(line(page))]
      await until(async () => (await running())[0], "Pending")
      await until(() => requests("GET", "api/update").length > rest, "the view read after the press")
      assert.deepEqual(await running(), [true, "true", 1, "v0.1.0 v0.2.0 Update"])
      assert.equal(await checkButton(page).count(), 0)
      await nav(page).getByRole("link", { name: "Events" }).click()
      for (let i = 0; i < 3; i++) {
        const before = reads()
        await page.clock.fastForward(2_000)
        await until(() => reads() > before, "the next read, on another tab")
      }
      await nav(page).getByRole("link", { name: "Settings" }).click()
      await updateButton(page).waitFor()
      assert.deepEqual(await running(), [true, "true", 1, "v0.1.0 v0.2.0 Update"])
      assert.equal(presses().length, 1)
      daemon.endUpdate({ outcome: "rolledBack" })
      await page.clock.fastForward(2_000)
      await button(page, "Update: Rolled back").waitFor()
      const ended = reads()
      await page.clock.fastForward(10_000)
      await page.waitForTimeout(300)
      assert.ok(reads() <= ended + 1, "read no more once it ended")
    } finally {
      await close()
    }
  })

  test("17. the page goes away and comes back: the daemon's mark and the dimming while it is gone, then the tab reloads itself onto the new release, whose version is all that says so", async () => {
    updater({ found: FOUND })
    for (const path of ["settings", ""]) {
      daemon.reset()
      updater({ found: FOUND })
      const { page, close } = await open(path)
      try {
        await mark(page).waitFor()
        if (path === "settings") {
          await press(page)
          await until(() => updateButton(page).isDisabled(), "running")
        }
        assert.equal(await loads(page), "navigate")
        daemon.stop()
        await unplugged(page).waitFor()
        if (path === "settings") {
          assert.equal(await updateButton(page).isDisabled(), true, "still Pending while the daemon is away")
          assert.equal(await shownWords(line(page)), "v0.1.0 v0.2.0 Update")
          daemon.endUpdate({ outcome: "landed" })
        }
        daemon.set(NEW)
        daemon.start()
        await until(async () => (await loads(page).catch(() => null)) === "reload", "the tab reloaded itself", 20_000)
        await unplugged(page).waitFor({ state: "detached" })
        await mark(page).waitFor({ state: "detached" })
        if (path === "") await nav(page).getByRole("link", { name: "Settings" }).click()
        await checkButton(page).waitFor()
        assert.equal(await shownWords(line(page)), "v0.2.0")
        assert.equal(await toast(page, "").count(), 0)
        assert.ok(page.url().endsWith(path === "" ? "/settings" : path))
      } finally {
        await close()
      }
    }
  })

  test("17. a reconnect to the same release reloads nothing, and one to a release that went back before the page was told reloads nothing", async () => {
    updater({ found: FOUND })
    const { page, close } = await open("settings")
    try {
      await updateButton(page).waitFor()
      await restart(page)
      await unplugged(page).waitFor({ state: "detached" })
      await page.waitForTimeout(300)
      assert.equal(await loads(page), "navigate")

      await retention(page, "90d").click()
      await restart(page, NEW)
      await toast(page, "v0.2.0").waitFor({ timeout: 20_000 })
      await unplugged(page).waitFor({ state: "detached" })
      await restart(page, { version: "v0.1.0", release: "0123456789ab" })
      await toast(page, "v0.2.0").waitFor({ state: "detached", timeout: 20_000 })
      await unplugged(page).waitFor({ state: "detached" })
      assert.equal(await toast(page, "").count(), 0, "put back: the page is this release's")
      assert.equal(await loads(page), "navigate")
    } finally {
      await close()
    }
  })

  for (const [what, path, edit, kept] of [
    ["a pick not saved", "settings", (page) => retention(page, "90d").click(), async (page) => assert.equal(await picked(page), "90d")],
    ["a key typed", "settings", (page) => page.getByLabel("API key").fill("k3y"), async (page) => assert.equal(await page.getByLabel("API key").inputValue(), "k3y")],
    ["a name typed", "sources/aurora", async (page) => {
      await page.getByRole("button", { name: /^Rename / }).click()
      await page.getByLabel(/^Name of /).fill("Boreal")
    }, async (page) => assert.equal(await page.getByLabel(/^Name of /).inputValue(), "Boreal")],
    ["a write out", "settings", async (page) => {
      await page.route("**/api/settings/check-for-updates", () => {})
      await page.getByRole("checkbox", { name: "Check for updates" }).click()
    }, async (page) => assert.equal(await page.getByRole("checkbox", { name: "Check for updates" }).isChecked(), false)],
  ]) {
    test(`17. a page with ${what} isn't reloaded: a toast names the release that came back and its Reload does it`, async () => {
      const { page, close } = await open(path)
      try {
        await edit(page)
        await restart(page, NEW)
        const said = toast(page, "v0.2.0")
        await said.waitFor({ timeout: 20_000 })
        assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "v0.2.0 Reload")
        assert.equal(await said.locator("[data-toast=ok]").count(), 1)
        assert.equal(await said.getByRole("button", { name: "Dismiss" }).count(), 1, "it stays until pressed or dismissed")
        await unplugged(page).waitFor({ state: "detached" })
        assert.equal(await loads(page), "navigate")
        await kept(page)
        if (path === "settings") {
          await line(page).getByText("v0.2.0", { exact: true }).waitFor()
          assert.equal(await checkButton(page).count(), 1, "About is the new daemon's")
        }
        await said.getByRole("button", { name: "Reload" }).click()
        await until(async () => (await loads(page).catch(() => null)) === "reload", "reloaded")
      } finally {
        await close()
      }
    })
  }

  test("17. an edit put back as it was doesn't hold the reload", async () => {
    const { page, close } = await open("settings")
    try {
      await retention(page, "90d").click()
      await retention(page, "30d").click()
      await page.getByLabel("API key").fill("k3y")
      await page.getByLabel("API key").fill("")
      await restart(page, NEW)
      await until(async () => (await loads(page).catch(() => null)) === "reload", "reloaded", 20_000)
    } finally {
      await close()
    }
  })

  const FAILED = [
    ["refused", null, "Refused"], ["rolledBack", null, "Rolled back"], ["failed", "noStart", "Failed to start"], ["failed", "stopped", "Stopped"], ["failed", "moved", "Changed in another tab"],
    ["failed", "planChanged", "Release changed"], ["failed", "notNewer", "Nothing newer"], ["failed", "noRoom", "Not enough space on /data"], ["failed", "downloadFailed", "Download failed"],
    ["failed", "checksum", "Checksum mismatch"], ["failed", "wrongRelease", "Wrong release"],
  ]
  test("17. an update that failed is the muted cross in Update's place, whose card is the reason and Retry, which asks as it stands now", async () => {
    for (const [outcome, reason, words] of FAILED) {
      daemon.reset()
      updater({ found: FOUND, last: last(outcome, reason) })
      const { page, close } = await open("settings")
      try {
        const cross = button(page, `Update: ${words}`)
        await cross.waitFor()
        assert.equal(await updateButton(page).count(), 0)
        assert.equal(await toneOf(cross), "muted")
        assert.equal(await shownWords(line(page)), "v0.1.0 v0.2.0")
        await cross.click()
        assert.equal(await shownWords(card(page)), `${words} Retry`)
        if (words !== "Rolled back") continue
        updater({ found: { ...FOUND, restartsCore: true }, last: last(outcome, reason) })
        await page.reload()
        await cross.click()
        await card(page).getByRole("button", { name: "Retry" }).click()
        await confirm(page).getByText("All slots will disconnect briefly.").waitFor()
        await card(page).waitFor({ state: "detached", timeout: 2000 })
        await page.keyboard.press("Escape")
        await until(() => cross.evaluate((b) => document.activeElement === b), "the focus back on the cross")
        await cross.click()
        await card(page).getByRole("button", { name: "Retry" }).click()
        await confirm(page).getByRole("button", { name: "Update", exact: true }).click()
        await until(() => updateButton(page).isDisabled().catch(() => false), "running again, in Update")
        assert.equal(await cross.count(), 0)
        assert.deepEqual(presses().map((r) => JSON.parse(r.body)), [{ version: "v0.2.0", restartsCore: true }])
      } finally {
        await close()
      }
    }
  })

  test("17. one cut in its switch is the amber cross before Update, read on until it says how it ended; recovery needed links its guide; a landing whose cleanup failed says so beside Check", async () => {
    updater({ found: FOUND, last: last("failed", "interrupted") })
    const { page, close } = await open("settings")
    try {
      const cut = button(page, "Update: Interrupted")
      await cut.waitFor()
      assert.equal(await updateButton(page).isEnabled(), true)
      assert.equal(await cut.evaluate((b) => getComputedStyle(b).color), await page.evaluate(() => { const x = document.body.appendChild(Object.assign(document.createElement("i"), { className: "text-warning" })); const c = getComputedStyle(x).color; x.remove(); return c }))
      updater({ found: FOUND, last: last("putBackFailed") })
      const needed = button(page, "Update: Recovery needed")
      await needed.waitFor()
      assert.equal(await updateButton(page).isEnabled(), true)
      await needed.click()
      assert.equal(await shownWords(card(page)), "Recovery needed")
      assert.equal(await card(page).getByRole("link", { name: "Open recovery guide" }).getAttribute("href"), "https://example.test/OWNER/sidegate/blob/v0.1.0/docs/operate.md")
      await page.keyboard.press("Escape")
      daemon.set(NEW)
      updater({ last: last("landedWithError") })
      await page.reload()
      const untidy = button(page, "Update: Cleanup failed")
      await untidy.click()
      assert.equal(await shownWords(card(page)), "Cleanup failed")
      assert.equal(await shownWords(line(page)), "v0.2.0")
      assert.equal(await checkButton(page).count(), 1)
      assert.equal(await mark(page).count(), 0)
    } finally {
      await close()
    }
  })

  test("17. an update that found another release the latest takes its offer away, says so beside Check, and the next load's check offers the other", async () => {
    updater({ found: FOUND })
    const { page, close } = await open("settings")
    try {
      await press(page)
      await until(() => updateButton(page).isDisabled(), "running")
      daemon.set({ latest: "v0.3.0" })
      daemon.endUpdate({ outcome: "failed", reason: "offerChanged" })
      await button(page, "Update: Release changed").waitFor()
      assert.equal(await shownWords(line(page)), "v0.1.0")
      assert.equal(await mark(page).count(), 0)
      await page.reload()
      await updateButton(page).waitFor()
      assert.equal(await shownWords(line(page)), "v0.1.0 v0.3.0 Update")
    } finally {
      await close()
    }
  })

  test("17. a press the daemon refuses is its toast, and About shows what is offered now", async () => {
    updater({ found: FOUND })
    const { page, close } = await open("settings")
    const said = async (text) => {
      await toast(page, text).waitFor()
      return (await toast(page, text).innerText()).replace(/\s+/g, " ").trim()
    }
    try {
      await updateButton(page).click()
      await confirm(page).waitFor()
      updater({ found: { version: "v0.3.0", restartsCore: true } })
      await confirm(page).getByRole("button", { name: "Update", exact: true }).click()
      assert.equal(await said("release changed"), "Update release changed")
      await line(page).getByText("v0.3.0").waitFor()
      assert.deepEqual(presses().map((r) => [r.status, JSON.parse(r.body)]), [[422, { version: "v0.2.0", restartsCore: false }]], "what its confirm said")

      for (const [status, body, words] of [
        [409, { kind: "capacity", reason: "lifecycleRunning", message: "x" }, "Update busy"],
        [401, null, "Update session expired Sign in"],
        [429, null, "Update HTTP 429"],
      ]) {
        await page.route("**/api/update", (r) => (r.request().method() === "POST" ? r.fulfill({ status, contentType: body ? "application/json" : "text/html", body: body ? JSON.stringify(body) : "<h1>no</h1>" }) : r.continue()))
        await updateButton(page).click()
        await page.getByRole("alertdialog").getByRole("button", { name: "Update", exact: true }).click()
        assert.equal(await said(words.replace("Update ", "").replace(" Sign in", "")), words)
        await until(() => updateButton(page).isEnabled(), "Update again")
        await page.unroute("**/api/update")
      }
      assert.equal(presses().length, 1, "each of those stopped in front, and none was sent again")
    } finally {
      await close()
    }
  })

  test("17. a press sends once: one whose answer is lost is read from the view, and said only when nothing there is of it", async () => {
    updater({ found: FOUND })
    const { page, close } = await open("settings")
    try {
      let sent = 0
      let lands = true
      await page.route("**/api/update", async (r) => {
        if (r.request().method() !== "POST") return r.continue()
        sent++
        if (lands) await r.fetch()
        return r.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ kind: "unavailable", reason: "unavailable", message: "x" }) })
      })
      await press(page)
      await until(() => updateButton(page).isDisabled(), "running, as the view says")
      await page.waitForTimeout(500)
      assert.equal(await toast(page, "").count(), 0)
      daemon.endUpdate({ outcome: "failed", reason: "checksum" })
      await button(page, "Update: Checksum mismatch").click()
      lands = false
      await card(page).getByRole("button", { name: "Retry" }).click()
      await confirm(page).getByRole("button", { name: "Update", exact: true }).click()
      await toast(page, "timed out").waitFor()
      assert.equal((await toast(page, "timed out").innerText()).replace(/\s+/g, " ").trim(), "Update timed out")
      await button(page, "Update: Checksum mismatch").waitFor()
      assert.equal(sent, 2)
    } finally {
      await close()
    }
  })

  test("17. a press refused for its token reads the session again and goes once more, and the daemon takes one", async () => {
    updater({ found: FOUND })
    const { page, close } = await open("settings")
    try {
      await updateButton(page).waitFor()
      daemon.rotateToken()
      await press(page)
      await until(() => presses().length === 2, "sent again")
      await until(() => updateButton(page).isDisabled(), "running")
      assert.deepEqual(presses().map((r) => [r.csrf, r.status]), [["token-1", 403], ["token-2", 202]])
    } finally {
      await close()
    }
  })

  test("17. Check is Pending until what it found is shown; found nothing newer, it is as it was, a failure is the muted mark beside it, and it always fetches", async () => {
    daemon.set({ settings: { ...daemon.settings(), checkForUpdates: false } })
    const { page, close } = await open("settings")
    try {
      await checkButton(page).waitFor()
      let answer
      const held = new Promise((done) => { answer = done })
      await page.route("**/api/update/check", async (r) => { await held; await r.continue() })
      await checkButton(page).click()
      await until(async () => (await checkButton(page).getAttribute("aria-busy")) === "true", "Pending")
      assert.equal(await checkButton(page).isDisabled(), true)
      answer()
      await until(() => checkButton(page).isEnabled(), "answered")
      await page.unroute("**/api/update/check")
      assert.equal(await shownWords(line(page)), "v0.1.0")
      assert.deepEqual(checks().map((r) => r.body), ['{"auto":true}', '{"auto":false}'])

      for (const [failure, words] of [["unreachable", "Could not reach GitHub"], ["clockWrong", "Clock is wrong"], ["filesUnavailable", "Release files unavailable"], ["malformed", "Unknown format"]]) {
        daemon.set({ checkFails: failure })
        await checkButton(page).click()
        const why = line(page).getByRole("button", { name: new RegExp(`^Check for updates: ${words}, `) })
        await why.waitFor()
        assert.equal(await toneOf(why), "muted")
        assert.equal(await shownWords(line(page)), "v0.1.0")
      }
      await line(page).getByRole("button", { name: /^Check for updates: / }).click()
      assert.match(await shownWords(card(page)), /^Unknown format (now|\d+\w+ ago|just now)/)
      daemon.set({ checkFails: null, latest: "v0.2.0" })
      await checkButton(page).click()
      await updateButton(page).waitFor()
      assert.equal(await shownWords(line(page)), "v0.1.0 v0.2.0 Update")
      assert.equal(await line(page).getByRole("button").count(), 1, "the offer stands alone")

      daemon.reset()
      await page.reload()
      await checkButton(page).waitFor()
      await page.route("**/api/update/check", (r) => r.abort())
      await checkButton(page).click()
      await toast(page, "timed out").waitFor()
      assert.equal((await toast(page, "timed out").innerText()).replace(/\s+/g, " ").trim(), "Check for updates timed out")
      assert.equal(await checkButton(page).isEnabled(), true)
    } finally {
      await close()
    }
  })

  test("17. a development build's About is its version and Xray's: no Check, no box, no Update, no mark", async () => {
    updater({ found: FOUND })
    daemon.set({ version: "dev-7990743", release: null })
    const { page, close } = await open("settings")
    try {
      await line(page).getByText("dev-7990743", { exact: true }).waitFor()
      assert.equal(await shownWords(line(page).locator("xpath=ancestor::*[@data-slot='card']")), "About sidegate dev-7990743 Xray v26.10.31")
      assert.equal(await line(page).getByRole("button").count(), 0)
      assert.equal(await mark(page).count(), 0)
    } finally {
      await close()
    }
  })
})

describe("adding a source", () => {
  test("17. Add offers Source and Group, and Add source is one field that names no kind: a URL shows a link and Advanced, share links a list and their count, other text the list alone", async () => {
    const { page, close } = await open("servers")
    try {
      await nav(page).getByRole("button", { name: "Add", exact: true }).click()
      assert.deepEqual(await page.getByRole("menuitem").allInnerTexts(), ["Source", "Group"])
      await page.getByRole("menuitem", { name: "Source", exact: true }).click()
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      await field.waitFor()
      const mark = dialog.locator("[data-slot=field] [role=img]")
      const advanced = dialog.getByRole("button", { name: "Advanced" })
      const name = dialog.getByLabel("Name", { exact: true })
      assert.equal(await mark.count(), 0, "an empty field has no mark")
      await field.fill("https://sub.example.com/api/sub/abc")
      assert.equal(await mark.getAttribute("aria-label"), "URL")
      assert.equal(await mark.getAttribute("title"), "URL")
      await advanced.waitFor()
      assert.equal(await name.count(), 0, "a subscription's name follows its provider")
      await field.fill("vless://a@nl.example.net:443#NL\nss://b@de.example.net:8388#DE\ntrojan://c@se.example.net:443#SE")
      assert.equal(await mark.getAttribute("aria-label"), "3 links")
      assert.equal(await shownWords(mark), "3")
      assert.equal(await advanced.count(), 0, "pasted servers have nothing to fetch")
      await name.waitFor()
      await field.fill("proxies:\n  - name: NL\n    type: vless")
      assert.equal(await mark.getAttribute("aria-label"), "Pasted text")
      assert.equal(await shownWords(mark), "", "no count for Clash, sing-box or Xray text")
      await field.fill("http://sub.example.com/api/sub/abc")
      assert.equal(await dialog.locator("[data-slot=field-error]").innerText(), "Must start with https://")
      assert.equal(await dialog.getByRole("button", { name: "Add", exact: true }).isEnabled(), false)
      assert.doesNotMatch(await dialog.innerText(), /Subscription|Server Links|Pasted/, "no kind is named")
    } finally {
      await close()
    }
  })

  test("17. Add refuses only one http URL: one https URL is a subscription, and Clash or sing-box text with a URL in it, or share links with a URL among them, go to the daemon as pasted", async () => {
    const paste = (file) => readFileSync(new URL(`../paste/${file}`, import.meta.url), "utf8")
    const { page, close } = await open("servers", { width: 1280, theme: "light" })
    try {
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      const mark = dialog.locator("[data-slot=field] [role=img]")
      const add = dialog.getByRole("button", { name: "Add", exact: true })
      const error = dialog.locator("[data-slot=field-error]")
      const name = dialog.getByLabel("Name", { exact: true })
      for (const [what, text, kind] of [
        ["one URL with a trailing newline and spaces", "https://sub.example.com/api/sub/abc  \n  \n", "URL"],
        ["the review's Clash document", paste("clash-url-line.yaml"), "Pasted text"],
        ["a sing-box document with a URL in it", paste("sing-box-url.json"), "Pasted text"],
        ["share links with a URL line among them", "vless://a@nl.example.net:443#NL\nhttps://sub.example.com/x\ntrojan://c@se.example.net:443#SE", "2 links"],
      ]) {
        await field.fill(text)
        assert.equal(await mark.getAttribute("aria-label"), kind, what)
        assert.equal(await error.count(), 0, `${what}: nothing under the field`)
        if (kind !== "URL") await name.fill("Pasted")
        assert.equal(await add.isEnabled(), true, `${what}: Add takes it`)
      }
      await field.fill(paste("clash-url-line.yaml"))
      await name.fill("Clash")
      await add.click()
      await until(() => daemon.sources().some((x) => x.name === "Clash"), "the Clash document is added")
      const put = daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(BASE + "api/sources/")).at(-1)
      assert.deepEqual(JSON.parse(put.body).origin, { kind: "manual", text: paste("clash-url-line.yaml") }, "as pasted")
    } finally {
      await close()
    }
  })

  const addDialogOf = (page) => page.getByRole("dialog", { name: "Add source" })
  const sourcePuts = () => daemon.log().filter((r) => r.method === "PUT" && r.path.startsWith(BASE + "api/sources/"))
  const addedIds = () => daemon.sources().map((x) => x.id).filter((id) => id !== "aurora")

  test("17. while Add is out the dialog is Pending and nothing closes it, Cancel, ×, Escape or a click outside; answered, it closes onto the new source", async () => {
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Source")
      const dialog = addDialogOf(page)
      const field = dialog.getByLabel("Source", { exact: true })
      await field.fill("https://sub.example.com/api/sub/abc")
      let release
      const held = new Promise((done) => { release = done })
      await page.route(/\/api\/sources\/[^/]+$/, async (r) => { if (r.request().method() === "PUT") await held; await r.continue() })
      const add = dialog.getByRole("button", { name: "Add", exact: true })
      await add.click()
      assert.equal(await add.getAttribute("aria-busy"), "true", "Add is Pending")
      assert.equal(await add.isDisabled(), true, "and takes no second press")
      assert.equal(await field.getAttribute("readonly"), "", "the field read-only")
      assert.equal(await dialog.getByRole("button", { name: "Cancel" }).isDisabled(), true)
      assert.equal(await dialog.getByRole("button", { name: "Close" }).isDisabled(), true, "its × off")
      assert.equal(await dialog.getByRole("button", { name: "Advanced" }).isDisabled(), true)
      await page.keyboard.press("Escape")
      await page.mouse.click(5, 5)
      assert.equal(await dialog.isVisible(), true, "Escape and a click outside leave it")
      release()
      await dialog.waitFor({ state: "hidden" })
      const [id] = addedIds()
      await page.waitForURL(new RegExp(`/sources/${id}$`))
      assert.equal(await toast(page, "").count(), 0, "no toast: the new source is in sight")
    } finally {
      await close()
    }
  })

  test("17. an add with no answer for 30 s: the field editable with Request timed out under it in the toast's mark, and Add unchanged sends the same request under the same key, making one source", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await addMenu(page, "Source")
      const dialog = addDialogOf(page)
      const field = dialog.getByLabel("Source", { exact: true })
      await field.fill("https://sub.example.com/api/sub/abc")
      let down = true
      await page.route(/\/api\/sources\/[^/]+$/, (r) => (r.request().method() === "PUT" && down ? r.abort() : r.continue()))
      const add = dialog.getByRole("button", { name: "Add", exact: true })
      await add.click()
      await page.clock.fastForward(31_000)
      const line = dialog.getByText("Request timed out", { exact: true })
      await line.waitFor()
      const said = toast(page, "timed out")
      const markOf = (where) => where.locator("svg").first().evaluate((svg) => svg.getAttribute("class"))
      assert.equal(await markOf(line), await markOf(said), "the no-answer toast's mark")
      assert.equal(await field.getAttribute("readonly"), null, "the field editable again")
      assert.match(await field.getAttribute("aria-describedby"), /no-answer/)
      assert.equal((await said.innerText()).replace(/\s+/g, " ").trim(), "Source timed out Retry")
      down = false
      await add.click()
      await dialog.waitFor({ state: "hidden" })
      const puts = sourcePuts()
      assert.equal(new Set(puts.map((r) => r.idempotencyKey)).size, 1, "the same key throughout")
      assert.equal(new Set(puts.map((r) => r.path)).size, 1, "at the same id")
      assert.equal(addedIds().length, 1, "one source")
      await said.waitFor({ state: "detached" })
    } finally {
      await close()
    }
  })

  test("17. an add that landed though every answer was lost is found by the fresh read at 30 s: the dialog closes onto it, one source", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await addMenu(page, "Source")
      const dialog = addDialogOf(page)
      await dialog.getByLabel("Source", { exact: true }).fill("https://sub.example.com/api/sub/abc")
      await page.route(/\/api\/sources\/[^/]+$/, async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        await r.fetch()
        await r.abort("connectionreset")
      })
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      await page.clock.fastForward(31_000)
      await dialog.waitFor({ state: "hidden" })
      const [id] = addedIds()
      await page.waitForURL(new RegExp(`/sources/${id}$`))
      assert.equal(addedIds().length, 1, "one source")
      assert.equal(new Set(sourcePuts().map((r) => r.idempotencyKey)).size, 1, "every attempt under one key")
      assert.equal(sourcePuts().filter((r) => r.status === 200 && !r.replayed).length, 1, "committed once")
    } finally {
      await close()
    }
  })

  test("17. an add whose first try landed unheard: edited after Request timed out it is a new add, at a new id under a new key, so it makes a second source; one try that never landed makes one", async () => {
    for (const firstLanded of [true, false]) {
      daemon.reset()
      const { page, close } = await open("servers", { clock: true })
      try {
        await addMenu(page, "Source")
        const dialog = addDialogOf(page)
        const field = dialog.getByLabel("Source", { exact: true })
        await field.fill("https://first.example/sub")
        const sent = []
        page.on("request", (r) => { if (r.method() === "PUT" && /\/api\/sources\/[^/]+$/.test(r.url())) sent.push({ path: new URL(r.url()).pathname, key: r.headers()["idempotency-key"], url: JSON.parse(r.postData()).origin.url }) })
        let lost = true
        let reads = true
        await page.route(/\/api\/sources\/[^/]+$/, async (r) => {
          if (r.request().method() !== "PUT" || !lost) return r.continue()
          if (firstLanded) await r.fetch()
          await r.abort("connectionreset")
        })
        await page.route("**/api/state", (r) => (reads ? r.continue() : r.abort()))
        reads = !firstLanded
        await dialog.getByRole("button", { name: "Add", exact: true }).click()
        await page.clock.fastForward(31_000)
        await dialog.getByText("Request timed out", { exact: true }).waitFor()
        lost = false
        await field.fill("https://second.example/sub")
        await dialog.getByRole("button", { name: "Add", exact: true }).click()
        reads = true
        await dialog.waitFor({ state: "hidden" })
        const first = sent.filter((r) => r.url === "https://first.example/sub")
        const second = sent.filter((r) => r.url === "https://second.example/sub")
        assert.notEqual(first[0].path, second[0].path, "a new id")
        assert.notEqual(first[0].key, second[0].key, "under a new key")
        assert.equal(new Set(first.map((r) => r.key)).size, 1, "the first try's attempts under one key")
        assert.equal(addedIds().length, firstLanded ? 2 : 1, firstLanded ? "the first landed, so two" : "the first never landed, so one")
        assert.equal(sourcePuts().filter((r) => r.status === 200 && !r.replayed).length, addedIds().length, "no request committed twice")
      } finally {
        await close()
      }
    }
  })

  test("17. a name another source has is the ringed field with Name in use under it, once, and Add disabled; a rename to it stays open and ringed, Enter refused, and its × puts the old name back; a source's own name in other letters is no clash", async () => {
    daemon.addAcme()
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Source")
      const dialog = addDialogOf(page)
      await dialog.getByLabel("Source", { exact: true }).fill("vless://a@nl.example.net:443#NL")
      const name = dialog.getByLabel("Name", { exact: true })
      await name.fill("acme")
      assert.equal(await name.getAttribute("aria-invalid"), "true")
      assert.equal(await dialog.locator("[data-slot=field-error]").innerText(), "Name in use")
      assert.equal(await dialog.getByText("Name in use").count(), 1, "said once")
      assert.equal(await dialog.getByRole("button", { name: "Add", exact: true }).isDisabled(), true)
      await name.fill("Mine")
      assert.equal(await dialog.getByRole("button", { name: "Add", exact: true }).isEnabled(), true)
      await dialog.getByRole("button", { name: "Cancel" }).click()
      await page.goto(daemon.url + "sources/aurora")
      const field = page.getByRole("textbox", { name: /^Name of / })
      await page.getByRole("button", { name: "Rename Aurora" }).click()
      await field.fill("Acme")
      await page.getByText("Name in use").waitFor()
      assert.equal(await field.getAttribute("aria-invalid"), "true")
      await field.press("Enter")
      assert.equal(await field.isVisible(), true, "Enter is refused")
      assert.equal(requests("PUT", "api/sources/aurora").length, 0)
      await page.getByRole("button", { name: "Cancel", exact: true }).click()
      await page.getByRole("button", { name: "Rename Aurora" }).waitFor()
      await page.getByRole("button", { name: "Rename Aurora" }).click()
      await field.fill("AURORA")
      assert.equal(await page.getByText("Name in use").count(), 0, "its own name is no clash")
      await field.press("Enter")
      await until(() => daemon.source("aurora").name === "AURORA", "renamed")
    } finally {
      await close()
    }
  })

  test("17. an Add pressed again after no answer with its URL spaced is the same add, and the daemon's refusal of it stands under the field", async () => {
    const { page, close } = await open("servers", { clock: true })
    try {
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      let refuse = false
      const sent = []
      await page.route("**/api/sources/*", async (r) => {
        if (r.request().method() !== "PUT") return r.continue()
        sent.push({ key: r.request().headers()["idempotency-key"], url: r.request().url(), body: r.request().postData() })
        if (!refuse) return r.abort()
        const { revision } = await (await fetch(daemon.url + "api/state")).json()
        return r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": String(revision) }, body: JSON.stringify({ kind: "validation", reason: "notHttps", message: "The source URL must be an https address." }) })
      })
      await field.fill("https://example.invalid/%")
      const add = dialog.getByRole("button", { name: "Add", exact: true })
      await add.click()
      await page.clock.fastForward(31_000)
      await toast(page, "timed out").waitFor()
      await field.fill("  https://example.invalid/%  ")
      refuse = true
      await add.click()
      const error = dialog.locator("[data-slot=field-error]")
      await error.waitFor()
      assert.equal(await error.innerText(), "Must start with https://", "under the field")
      assert.deepEqual([new Set(sent.map((w) => w.key)).size, new Set(sent.map((w) => w.url)).size, new Set(sent.map((w) => w.body)).size], [1, 1, 1], "the same request, at the same id, under its key")
      assert.equal(await add.isDisabled(), true, "Add waits for the next edit")
      assert.equal(await toast(page, "https").count(), 0, "no toast")
    } finally {
      await close()
    }
  })

  test("17. an add refused at a revision the view hasn't reached shows its refusal under the field at once, and the field takes typing again once the view reaches it", async () => {
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      const { revision } = await (await fetch(daemon.url + "api/state")).json()
      const free = daemon.holdReads("api/state")
      daemon.changeSlot("eu", { name: "EU later" }, { announce: false })
      await page.route("**/api/sources/*", (r) => (r.request().method() !== "PUT" ? r.continue()
        : r.fulfill({ status: 422, contentType: "application/json", headers: { "Sidegate-Revision": String(revision + 1) }, body: JSON.stringify({ kind: "validation", reason: "unreadableText", message: "That text isn't a list of servers this gateway can read." }) })))
      await field.fill("not a list of servers")
      await dialog.getByLabel("Name", { exact: true }).fill("Odd")
      const add = dialog.getByRole("button", { name: "Add", exact: true })
      await add.click()
      const error = dialog.locator("[data-slot=field-error]")
      await error.waitFor()
      assert.equal(await error.innerText(), "Unknown format", "under the field at once")
      assert.equal(await add.getAttribute("aria-busy"), "true", "past the view, Add is Pending while the view is read")
      free()
      await until(async () => (await add.getAttribute("aria-busy")) === null, "the view at the refusal frees the form")
      await field.fill("trojan://pw@odd.example:443#Odd")
      assert.equal(await error.count(), 0, "the field takes typing, and an edit clears the refusal")
      assert.equal(await toast(page, "").count(), 0, "no toast")
    } finally {
      await close()
    }
  })

  test("17. what the daemon refuses stays in the field with its refusal under it, not in a toast, until the next edit", async () => {
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      await field.fill("not a list of servers")
      await dialog.getByLabel("Name", { exact: true }).fill("Odd")
      await dialog.getByRole("button", { name: "Add", exact: true }).click()
      const error = dialog.locator("[data-slot=field-error]")
      await until(async () => (await error.count()) > 0, "the refusal under the field")
      assert.equal(await error.innerText(), "Unknown format")
      assert.equal(await field.inputValue(), "not a list of servers", "the text stays")
      assert.equal(await field.getAttribute("aria-invalid"), "true")
      assert.equal(await toast(page, "That text").count(), 0, "no toast")
      await field.fill("trojan://pw@odd.example:443#Odd")
      assert.equal(await error.count(), 0, "an edit clears it")
    } finally {
      await close()
    }
  })
})

describe("the Ink-Stays Rule on the write path's controls", () => {
  /** A text's or a mark's colour as drawn once its transitions have ended. */
  const inkOf = (locator) => locator.evaluate(async (el) => {
    await Promise.all(el.getAnimations().filter((a) => a instanceof CSSTransition).map((a) => a.finished.catch(() => {})))
    return getComputedStyle(el).color
  })
  /** The ink of each of `marks` at rest, focused, hovered and, last, pressed, the pointer left down on the control. */
  const states = async (page, control, marks) => {
    const inks = async () => Promise.all(marks.map(inkOf))
    const rest = await inks()
    await control.focus()
    const focused = await inks()
    await control.hover()
    const hovered = await inks()
    await page.mouse.down()
    const pressed = await inks()
    return { rest, focused, hovered, pressed }
  }
  const same = (got, what) => {
    for (const state of ["focused", "hovered", "pressed"]) assert.deepEqual(got[state], got.rest, `${what} ${state}`)
  }

  test("17. a mark keeps its colour in every state of the toast's buttons, its button and its ×", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await page.locator("[data-slot=source-heading]").locator("button[aria-label^='Rename ']").click()
      await page.getByLabel("Name of Aurora").fill("Mine")
      let theirs = false
      await page.route("**/api/sources/aurora", async (r) => {
        if (r.request().method() === "PUT" && !theirs) {
          theirs = true
          daemon.renameSource("aurora", "Theirs")
        }
        await r.continue()
      })
      await page.keyboard.press("Enter")
      const said = toast(page, "changed to")
      await said.waitFor()
      const mine = said.getByRole("button", { name: "Keep mine" })
      const rest = await inkOf(mine)
      await mine.focus()
      assert.equal(await inkOf(mine), rest, "Keep mine focused")
      await mine.hover()
      assert.equal(await inkOf(mine), rest, "Keep mine hovered")
      const dismiss = said.getByRole("button", { name: "Dismiss" })
      same(await states(page, dismiss, [dismiss.locator("svg")]), "the toast's ×")
      await page.mouse.up()
    } finally {
      await close()
    }
  })

  test("17. a mark keeps its colour in every state of a delete dialog's row: its Exit at rest, hovered, pressed, focused and open, and its pick while it is out", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await page.getByRole("button", { name: "Delete Aurora" }).click()
      const usedBy = page.getByRole("alertdialog").getByRole("region", { name: "Used by" })
      const exit = usedBy.getByRole("combobox", { name: "Exit of EU" })
      await exit.waitFor()
      const marks = [exit.locator("span").first(), exit.locator("svg").last()]
      const got = await states(page, exit, marks)
      same(got, "the row's Exit")
      await page.mouse.up()
      await until(async () => (await exit.getAttribute("aria-expanded")) === "true", "its menu open")
      assert.deepEqual(await Promise.all(marks.map(inkOf)), got.rest, "the row's Exit open")
      let release
      const held = new Promise((done) => { release = done })
      await page.route("**/api/slots/eu", async (r) => { await held; await r.continue() }, { times: 1 })
      await page.locator("[data-radix-popper-content-wrapper]").locator('[data-value="direct"]').click()
      await usedBy.getByLabel("Repointing EU").waitFor()
      assert.equal(await inkOf(exit.getByText("Direct")), got.rest[0], "the pick while it is out keeps the value's ink")
      release()
    } finally {
      await close()
    }
  })

  test("17. a source's Refresh choices keep their ink focused, hovered and pressed", async () => {
    const { page, close } = await open("sources/aurora")
    try {
      await page.getByRole("button", { name: "Settings for Aurora" }).click()
      const choice = page.getByRole("dialog", { name: "Settings" }).getByRole("radiogroup", { name: "Refresh" }).getByRole("radio", { name: "Off" })
      same(await states(page, choice, [choice]), "Off")
    } finally {
      await close()
    }
  })

  test("17. Add and a source's Save keep their ink while Pending, the spinner in it, and a refused field keeps its text's and its label's", async () => {
    const { page, close } = await open("servers")
    try {
      await addMenu(page, "Source")
      const dialog = page.getByRole("dialog", { name: "Add source" })
      const field = dialog.getByLabel("Source", { exact: true })
      await field.fill("not a list of servers")
      await dialog.getByLabel("Name", { exact: true }).fill("Odd")
      const add = dialog.getByRole("button", { name: "Add", exact: true })
      const typed = await inkOf(field)
      const name = dialog.getByText("Name", { exact: true })
      const label = await inkOf(name)
      const ink = await inkOf(add)
      let release
      const held = new Promise((done) => { release = done })
      await page.route(/\/api\/sources\/[^/]+$/, async (r) => { if (r.request().method() === "PUT") await held; await r.continue() })
      await add.click()
      await add.locator("[data-slot=spinner]").waitFor()
      assert.deepEqual([await inkOf(add), await inkOf(add.locator("[data-slot=spinner]"))], [ink, ink], "Add and its spinner while it is out")
      release()
      await until(async () => (await field.getAttribute("aria-invalid")) === "true", "the refused field ringed")
      assert.equal(await inkOf(field), typed, "the refused text keeps its ink")
      assert.equal(await inkOf(name), label, "the label keeps its colour")
    } finally {
      await close()
    }
    seedTroubled()
    const settings = await open("sources/pasted", { width: 1280 })
    try {
      await settings.page.getByRole("button", { name: "Settings for Pasted" }).click()
      const dialog = settings.page.getByRole("dialog", { name: "Settings" })
      const servers = dialog.getByRole("textbox", { name: "Servers" })
      await servers.fill("not a valid server")
      const save = dialog.getByRole("button", { name: "Save", exact: true })
      const label = dialog.getByText("Servers", { exact: true })
      const [ink, typed, labelInk] = [await inkOf(save), await inkOf(servers), await inkOf(label)]
      let release
      const held = new Promise((done) => { release = done })
      await settings.page.route("**/api/sources/pasted", async (r) => { if (r.request().method() === "PUT") await held; await r.continue() })
      await save.click()
      await save.locator("[data-slot=spinner]").waitFor()
      assert.deepEqual([await inkOf(save), await inkOf(save.locator("[data-slot=spinner]"))], [ink, ink], "Save and its spinner while it is out")
      release()
      await until(async () => (await servers.getAttribute("aria-invalid")) === "true", "the refused Servers text ringed")
      assert.deepEqual([await inkOf(servers), await inkOf(label)], [typed, labelInk], "the refused text and its label keep their ink")
    } finally {
      await settings.close()
    }
  })
})
