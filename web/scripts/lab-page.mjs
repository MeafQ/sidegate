// Drives the real page on the real daemon in the lab, from lab/verify-page.sh and lab/step.sh page.
// LAB_ONLY names scenarios to run, comma-separated. Sources and a slot always come first, since
// everything else needs them. LAB_WORLD is the world it finds: page, the daemon with nothing in it, or
// update, the lifecycle suite's gateway on v0.0.1 with v0.0.2 published, whose one scenario is update.
import { spawnSync } from "node:child_process"
import { isDeepStrictEqual } from "node:util"
import { chromium } from "playwright-core"

const URL_ = process.env.LAB_PAGE_URL
const SHOTS = process.env.LAB_SHOTS
const VM = process.env.LAB_VM
const FEEDS = process.env.LAB_FEEDS
const GUEST = process.env.GUEST_DIR
const CORE_UNIT = process.env.LAB_CORE_UNIT
const WORLD = process.env.LAB_WORLD
const ONLY = process.env.LAB_ONLY ? process.env.LAB_ONLY.split(",") : null
const SERVER_OF = { "good-A": "srvA", "good-C": "srvC", "bad-B": "srvB", "bad-D": "srvD" }
const LAB_SERVER = /(good|bad)-[A-D]/

let failed = 0
const pass = (what) => console.log(`PASS browser: ${what}`)
const fail = (what) => { failed++; console.log(`FAIL browser: ${what}`) }
const check = (ok, what) => (ok ? pass(what) : fail(what))
const vm = (...args) => {
  const run = spawnSync("limactl", ["shell", "--workdir", "/", VM, "sudo", `${GUEST}/page.sh`, ...args], { encoding: "utf8" })
  console.log(run.stdout.trim())
  if (run.status !== 0) failed++
  return run.stdout
}

const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, colorScheme: "dark" })
const page = await context.newPage()
page.setDefaultTimeout(20_000)
const shot = (name) => page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: true })
const go = (path) => page.goto(URL_ + path)
const board = () => page.getByRole("list", { name: "VPN Clients" })
const tiles = () => board().getByRole("group")
const tile = (name) => board().getByRole("group", { name, exact: true })
const addSlot = () => board().getByRole("button", { name: "Add", exact: true })

const failure = await page.goto(URL_)
check(failure.status() === 401, `with no login nginx answers ${failure.status()}`)
await context.addCookies([{ name: "UNIFI_TOKEN", value: "lab-login", url: URL_ }])

const cdp = await context.newCDPSession(page)
await cdp.send("Network.enable")
const frames = []
cdp.on("Network.eventSourceMessageReceived", ({ data }) => { frames.push(JSON.parse(data)) })
let documents = 0
page.on("request", (r) => { if (r.resourceType() === "document") documents++ })
// The browser itself logs what this script provokes: the login's 401, the refused delete's 409, and while an update restarts the
// daemon nginx's 502s and the stream it cuts.
const PROVOKED = WORLD === "update" ? /401|502|ERR_INCOMPLETE_CHUNKED_ENCODING/ : /401|409/
const pageErrors = []
page.on("console", (m) => { if (m.type() === "error" && !PROVOKED.test(m.text())) pageErrors.push(m.text()) })

const nav = () => page.getByRole("navigation", { name: "Main" })
const saveBar = () => page.getByRole("group", { name: "Unsaved changes" })

// The nav's + Add menu is where a source and a group start.
async function addMenu(item) {
  await nav().getByRole("button", { name: "Add", exact: true }).click()
  await page.getByRole("menuitem", { name: item, exact: true }).click()
}

// A subscription is added by its URL alone in Add source's one field, and its name follows the provider's title.
async function addSource(url) {
  await addMenu("Source")
  const dialog = page.getByRole("dialog", { name: "Add source" })
  await dialog.getByLabel("Source", { exact: true }).fill(url)
  await dialog.getByRole("button", { name: "Add", exact: true }).click()
  await dialog.waitFor({ state: "hidden" })
}

// A source's or group's nav row is a link holding its status dot, name, server count and any trouble.
const navRow = (name) => nav().getByRole("link").filter({ hasText: name }).first()

// listed waits for a source's nav row to read its name and then its server count.
async function listed(name, count) {
  const deadline = Date.now() + 60_000
  let text = ""
  while (Date.now() < deadline) {
    text = (await navRow(name).innerText().catch(() => "")).replace(/\s+/g, " ")
    if (text.trim().startsWith(`${name} ${count}`)) return
    await page.waitForTimeout(500)
  }
  throw new Error(`the nav never listed ${name} with ${count} servers: '${text}'`)
}

// A feed that answers 404 to the page's default User-Agent until an edit picks another, and two that just answer.
async function sources() {
  await go("servers")
  await addSource(`${FEEDS}good?token=lab-s3cret`)
  // Until a fetch lands there is no provider title, so the source goes by its URL's host.
  await navRow(new URL(FEEDS).hostname).click()
  const failing = page.locator("[data-slot=source-heading]").getByRole("button", { name: "Refresh failing", exact: true })
  await failing.waitFor({ timeout: 60_000 })
  // Listing no servers, the source holds its failure in its table's place, and its heading's Refresh opens no card.
  const why = page.locator("main [data-slot=empty]").filter({ hasText: "HTTP 404" })
  await why.getByRole("combobox", { name: "User-Agent" }).click()
  await page.getByRole("option", { name: "v2rayNG/1.10.16", exact: true }).click()
  pass("the feed refuses the page's default User-Agent, and the failure in the table's place offers the others beside Retry")
  await shot("1-404")
  await why.getByRole("button", { name: "Retry", exact: true }).click()
  await listed("Good", 2)
  pass("Retry with User-Agent v2rayNG/1.10.16 stores it and refreshes in one write: the page lists the feed's two servers under its title, with no reload")

  const good = async () => (await state()).sources.find((x) => x.name === "Good").origin
  check((await good()).userAgent === "v2rayNG/1.10.16", "the daemon stored the agent Retry sent")
  await page.getByRole("button", { name: "Settings for Good", exact: true }).click()
  const settings = page.getByRole("dialog", { name: "Settings" })
  check(await settings.getByLabel("URL", { exact: true }).inputValue() === "", "the stored URL never comes back to the form")
  await settings.getByRole("radiogroup", { name: "Refresh" }).getByRole("radio", { name: "12h", exact: true }).click()
  await settings.getByRole("button", { name: "Save", exact: true }).click()
  await settings.waitFor({ state: "hidden", timeout: 30_000 })
  const saved = await good()
  check(saved.refreshEverySeconds === 12 * 3600 && saved.userAgent === "v2rayNG/1.10.16", `Settings saved a 12h refresh and kept the agent: ${saved.refreshEverySeconds} s, ${saved.userAgent}`)

  await addSource(`${FEEDS}bad`)
  await listed("Bad", 2)
  await addSource(`${FEEDS}acme`)
  await listed("Acme", 12)
  pass("the Acme-shaped feed lists its 12 servers")
  await shot("2-sources")
  check(!(await page.content()).includes("lab-s3cret"), "the feed's token is nowhere in the page")

  await addMenu("Group")
  await page.waitForURL((u) => u.pathname.endsWith("/groups/new"))
  const name = page.getByRole("textbox", { name: /^Name of New group/ })
  await name.fill("Lab servers")
  await name.press("Enter")
  await openFilter("Name")
  await textChip("Name matches").fill("(good|bad)-")
  await page.keyboard.press("Escape")
  await page.getByRole("group", { name: "Name matches filter" }).waitFor()
  await page.getByText("4 in group", { exact: true }).waitFor({ timeout: 30_000 })
  await saveBar().getByRole("button", { name: "Save", exact: true }).click()
  await page.waitForURL((u) => /\/groups\/(?!new$)[^/]+$/.test(u.pathname), { timeout: 30_000 })
  await saveBar().waitFor({ state: "hidden" })
  check((await state()).groups.some((g) => g.name === "Lab servers"), "a new group's draft at groups/new, its rule previewed through Go holding the 4 lab servers, saves as the daemon's group Lab servers and opens its tab")
}

// until calls read() every half second until it answers, or throws naming what never came.
async function until(what, ms, read) {
  const deadline = Date.now() + ms
  while (Date.now() < deadline) {
    const got = await read().catch(() => null)
    if (got) return got
    await page.waitForTimeout(500)
  }
  throw new Error(`${what} within ${ms / 1000} s`)
}

// daemonMove reads, from GET api/switches, the slot's first failureMove applied at or after killedAt (epoch
// seconds on the daemon's clock), and says whether it applied within limit seconds of it. No kill time or
// no such switch is a failure, so only the daemon's own time can pass row 7e's bound.
function daemonMove(switches, slot, killedAt, limit) {
  if (!Number.isFinite(killedAt)) return { ok: false, what: "no kill time read from the VM" }
  // Whole milliseconds, both clocks' precision, so the bound compares exactly and only the print rounds.
  const killedMs = Math.round(killedAt * 1000)
  const applied = switches
    .filter((e) => e.slots.includes(slot) && e.reason === "failureMove" && e.appliedAt)
    .map((e) => Date.parse(e.appliedAt))
    .filter((at) => at >= killedMs)
    .sort((a, b) => a - b)
  if (!applied.length) return { ok: false, what: `the daemon applied no failureMove for ${slot} after the kill at ${killedMs}: ${JSON.stringify(switches.filter((e) => e.slots.includes(slot)).slice(0, 4))}` }
  const ms = applied[0] - killedMs
  const ok = ms <= limit * 1000
  return { ok, what: `the daemon applied ${slot}'s failureMove ${(ms / 1000).toFixed(3)} s after the kill, ${ok ? "within" : "past"} ${limit} s` }
}

// The lab server a tile's server line says its slot is on, if any, from the applied server's title: a line saying where it is
// switching to names a server it isn't on yet.
const onServer = async (name) => {
  const line = tile(name).locator("span[title]").filter({ hasText: LAB_SERVER }).first()
  return (await line.count()) ? (await line.getAttribute("title")).match(LAB_SERVER)?.[0] : undefined
}
const serving = (name, ms = 60_000) => until(`${name}'s tile naming a lab server`, ms, () => onServer(name))

// The Exit menu on a slot's tile: Direct, Block, a source, a group or a server.
async function setExit(name, item) {
  await page.getByRole("combobox", { name: `Exit of ${name}` }).click()
  await page.getByRole("dialog").getByRole("option").filter({ hasText: item }).first().click()
  await page.getByRole("dialog").waitFor({ state: "hidden" })
}

// A column's filters open from its header.
const openFilter = (column) => page.getByRole("button", { name: `Filter servers by ${column}`, exact: true }).click()
// A saved filter's chip in the line above the table opens the same editor.
const editChip = (label) => page.getByRole("button", { name: `Edit ${label} filter`, exact: true }).click()
const textChip = (label) => page.getByRole("dialog").getByRole("textbox", { name: label, exact: true })
const words = async (locator) => (await locator.innerText()).replace(/\s+/g, " ").trim()
async function allServers() {
  const items = []
  let cursor = ""
  do {
    const got = await (await context.request.get(`${URL_}api/servers?limit=500&cursor=${encodeURIComponent(cursor)}`)).json()
    items.push(...got.items)
    cursor = got.next
  } while (cursor)
  return items
}
const inVm = (cmd) => spawnSync("limactl", ["shell", "--workdir", "/", VM, "sudo", "sh", "-c", cmd], { encoding: "utf8" }).stdout.trim()
const allLabels = new Map()
const state = async () => (await context.request.get(URL_ + "api/state")).json()
const slotOf = async (id) => (await state()).slots.find((x) => x.id === id)
const retentionDays = async () => (await (await context.request.get(URL_ + "api/settings")).json()).analyticsRetentionDays
// A write as the page sends one, with UniFi's CSRF token from a read and the daemon's from api/session.
async function write(method, path) {
  const session = await context.request.get(URL_ + "api/session")
  const headers = { "X-Sidegate-Csrf": (await session.json()).csrf, "X-Csrf-Token": session.headers()["x-csrf-token"], "Idempotency-Key": `lab-${Date.now()}` }
  const res = await context.request.fetch(URL_ + path, { method, headers })
  return { status: res.status(), body: await res.text() }
}
const save = async () => {
  await saveBar().getByRole("button", { name: "Save", exact: true }).click()
  await saveBar().waitFor({ state: "hidden", timeout: 30_000 })
}

// A key is enough for the daemon to read UniFi, so with no slot yet the board is already whole.
async function slot() {
  await go("")
  await board().getByText("Office WireGuard").waitFor({ timeout: 30_000 })
  check(await tiles().count() === 1 && await addSlot().count() === 1 && await page.getByText("UniFi not read: free VPN Clients unknown").count() === 0,
    "with no slot the board reads UniFi: the foreign client's tile, and Add for a free one")
  await addSlot().click()
  await board().getByRole("group", { name: "New slot", exact: true }).getByRole("button", { name: "Create", exact: true }).click()
  await tile("Slot 1").waitFor({ timeout: 30_000 })
  check(/Block/.test(await page.getByRole("combobox", { name: "Exit of Slot 1" }).innerText()), "Add and Create on the board made Slot 1, on Block")
  await page.getByRole("button", { name: "Rename Slot 1" }).click()
  const name = page.getByLabel("Name of Slot 1")
  await name.fill("EU")
  await name.press("Enter")
  await tile("EU").waitFor({ timeout: 30_000 })
  const id = (await state()).slots.find((s) => s.name === "EU")?.id
  if (!id) throw new Error("the daemon has no slot named EU after the rename")
  pass(`renamed it EU on its tile, and the daemon stored it as ${id}`)
  await setExit("EU", "Lab servers")
  await tile("EU").getByText("Not connected", { exact: true }).waitFor({ timeout: 120_000 })
  pass("on Lab servers the slot serves, and with no client on its keys its tile says Not connected")
  await shot("3-slot")
  vm("connect", id)
  const on = await serving("EU")
  pass(`once a client holds the slot's keys, the open page shows it on ${on}`)
  for (const x of await allServers()) allLabels.set(x.id, x.label.match(LAB_SERVER)?.[0] ?? x.label)
  return { id }
}

const scenarios = {
  // The foreign client read-only with its real name, then our slot, and Add, all from what the daemon reads.
  async board() {
    await go("")
    await board().getByText("Office WireGuard").waitFor({ timeout: 90_000 })
    await serving("EU")
    check(await tiles().count() === 2, "the board has a tile per VPN Client: the foreign one and EU")
    const words = await tiles().allInnerTexts()
    check(/^Office WireGuard/.test(words[0]) && /^EU/.test(words[1]), `the foreign client comes first, then our slot: ${words[1].split("\n").slice(0, 4).join(" · ")}`)
    check(await tile("EU").getByRole("img", { name: /^Serving/ }).count() === 1 && LAB_SERVER.test(words[1]) && /Lab servers/.test(words[1]) && /\d+ \/ \d+ ms|—/.test(words[1]),
      "its tile shows name, Exit, status dot, active server and Ping")
    const foreign = tile("Office WireGuard")
    check(/Managed in UniFi/.test(words[0]) && await foreign.getByRole("combobox").count() === 0 && await foreign.getByRole("button", { name: /^Rename/ }).count() === 0,
      "the foreign tile takes no Exit and no name")
    check(await addSlot().count() === 1, "Add is there for a free VPN Client")
    await shot("board")
  },

  // Direct, Block, sources, groups and the servers that pass their filter, from the real daemon's answer. The EDGE-STANDBY servers are in their source's list.
  async exitmenu() {
    await go("")
    await page.getByRole("combobox", { name: "Exit of EU" }).click()
    const list = page.getByRole("dialog")
    await list.getByText("Lab servers").waitFor()
    await list.getByText("Sweden").first().waitFor()
    const words = await list.innerText()
    check(["Direct", "Block", "Good", "Acme", "Lab servers", "Sweden"].every((w) => words.includes(w)), "the menu has Direct, Block, sources, groups and servers")
    check(words.includes("Sweden · EDGE-STANDBY"), "every server a source lists is in the menu")
    check(words.includes("Stability") && !words.includes("Score"), "the menu's columns are Ping and Stability, with no Score")
    await shot("exit-menu")
    await page.keyboard.press("Escape")
    await navRow("Acme").click()
    await page.getByRole("table", { name: "Servers" }).getByRole("button", { name: "Sweden · EDGE-STANDBY", exact: true }).waitFor()
    check(await page.locator("tbody tr[class*='opacity-50']").count() === 0, "the EDGE-STANDBY servers are in their source's list, none dimmed")
    const headers = await page.locator("thead th").evaluateAll((ths) => ths.map((th) => th.textContent.trim()))
    check(!headers.includes("#") && await page.getByRole("button", { name: /^Sort/ }).count() === 0, `the source's tab has no # and no header that sorts: ${headers.join(", ")}`)
    const nameColumn = headers.indexOf("Name") + 1
    const sections = await page.locator("tbody").evaluateAll((bodies, column) => bodies.map((b) => [b.querySelector("th[scope=rowgroup]")?.innerText.trim(),
      [...b.querySelectorAll(`tr:not(:has(> th)) td:nth-child(${column}) button`)].map((name) => name.ariaLabel ?? name.innerText.trim())]), nameColumn)
    check(sections.length === 6 && sections.every(([heading, rows]) => heading && rows.length === 2), `the source's tab shows 6 configs of 2 servers: ${JSON.stringify(sections)}`)
    const acme = (await state()).sources.find((x) => x.name === "Acme")
    const listed = (await allServers()).filter((x) => x.source === acme.id && x.home !== null)
    const at = (x) => x.seen.find((seen) => seen.configKey === x.home)?.position ?? Infinity
    const want = (acme.feed?.configs ?? []).filter((c) => listed.some((x) => x.home === c.key))
      .map((c) => [c.name, listed.filter((x) => x.home === c.key).toSorted((a, b) => at(a) - at(b)).map((x) => x.label)])
    check(isDeepStrictEqual(sections, want), `each config's servers come under it as the daemon's feed lists them: ${JSON.stringify(sections)}${isDeepStrictEqual(sections, want) ? "" : `, want ${JSON.stringify(want)}`}`)
  },

  // What the daemon sends about Acme's feed, as its tab shows it: Refresh's card with when it runs next and how often, or Refresh
  // Off once Settings pauses it, where servers' names and countries came from, and search as its only way to narrow the rows.
  async feed() {
    const source = (await state()).sources.find((x) => x.name === "Acme")
    if (!source?.feed) throw new Error("the daemon sends no feed for Acme")
    const servers = (await allServers()).filter((x) => x.source === source.id && x.presence === "present")
    const { configs } = source.feed
    await go("servers")
    await navRow("Acme").click()
    const refresh = page.locator("[data-slot=source-heading]").getByRole("button", { name: /^Refresh/ })
    const refreshCard = async () => {
      await refresh.focus()
      const card = page.locator("[data-radix-popper-content-wrapper]").last()
      await card.waitFor()
      const said = await words(card)
      await refresh.blur()
      return said
    }
    const origin = async () => (await state()).sources.find((x) => x.id === source.id).origin
    const was = await origin()
    // Every interval Settings offers is whole hours, and the daemon's own for a source with none and no provider's is 6h.
    const interval = was.refreshEverySeconds || was.provider.suggestedRefreshSeconds || 6 * 3600
    const runs = await refreshCard()
    check(new RegExp(`^Next in \\d+(m|h|d) Refresh every ${interval / 3600}h$`).test(runs), `Refresh's card is two rows, when it runs next and the ${interval / 3600}h the daemon refreshes it by: '${runs}'`)
    check(configs.length > 0 && !(await page.locator("main").innerText()).includes(`${configs.length} configs`), "the feed's make-up is on no line of the page")
    await shot("feed")

    const setRefresh = async (radio) => {
      await page.getByRole("button", { name: "Settings for Acme", exact: true }).click()
      const settings = page.getByRole("dialog", { name: "Settings" })
      await settings.getByRole("radiogroup", { name: "Refresh" }).getByRole("radio", { name: radio, exact: true }).click()
      await settings.getByRole("button", { name: "Save", exact: true }).click()
      await settings.waitFor({ state: "hidden", timeout: 30_000 })
    }
    await setRefresh("Off")
    const paused = await origin()
    const off = await until("Refresh's card to say it is off", 30_000, async () => { const said = await refreshCard(); return said === "Refresh Off" ? said : null }).catch(() => null)
    check(paused.paused && off !== null, `paused in Settings, the daemon holds it paused and Refresh's card is the one row Refresh Off: ${paused.paused}, '${off ?? await refreshCard()}'`)
    await setRefresh(`${interval / 3600}h`)
    const back = await origin()
    check(!back.paused && back.refreshEverySeconds === was.refreshEverySeconds, `back on ${interval / 3600}h the daemon holds it as before: paused ${back.paused}, ${back.refreshEverySeconds} s`)

    const COUNTRY = { rule: "From a rule", tagFlag: "From the tag's flag", configFlag: "From the config's flag", unknown: "Unknown" }
    const region = new Intl.DisplayNames(["en"], { type: "region" })
    for (const source of new Set(servers.map((x) => x.country.source))) {
      const server = servers.find((x) => x.country.source === source)
      const country = `${server.country.code ? region.of(server.country.code) : "Unknown"}: ${COUNTRY[source]}`
      await page.getByRole("table", { name: "Servers" }).getByRole("button", { name: server.label, exact: true }).first().click()
      const dialog = page.getByRole("dialog", { name: server.label })
      await dialog.waitFor()
      check(await dialog.getByRole("button", { name: country, exact: true }).count() === 1, `${server.label}'s details say where its country came from: '${country}'`)
      await page.keyboard.press("Escape")
      await dialog.waitFor({ state: "hidden" })
    }

    const filters = await page.getByRole("button", { name: /^Filter servers by / }).count()
    check(filters === 0, `the source's tab has no filter: ${filters} headers open one`)
    const rows = await page.locator("tbody tr:not(:has(> th))").count()
    await page.getByPlaceholder("Search").fill(servers[0].label)
    await until("the search to narrow the rows", 30_000, async () => (await page.locator("tbody tr:not(:has(> th))").count()) < rows || null)
    pass("the source's tab narrows its rows by search alone")
    await page.getByPlaceholder("Search").fill("")
  },

  // An edit reaches the slot using the group. A slot in use is what the daemon's refusal of a delete names, and what the page's
  // Delete waits on, with its Exit there to repoint.
  async group(s) {
    await go("servers")
    await navRow("Lab servers").click()
    await page.locator("tbody tr").filter({ has: page.getByRole("img", { name: "In use: EU", exact: true }) }).first().waitFor({ timeout: 30_000 })
    pass("the group's table says which server EU is on through it")
    await editChip("Name matches")
    await textChip("Name matches").fill("(good|bad)-[A-C]")
    await page.keyboard.press("Escape")
    await page.getByText("3 in group", { exact: true }).waitFor({ timeout: 30_000 })
    await save()
    pass("the edit saved, and Go's preview said 3 servers first")
    await editChip("Name matches")
    await textChip("Name matches").fill("(good|bad)-")
    await page.keyboard.press("Escape")
    await page.getByText("4 in group", { exact: true }).waitFor({ timeout: 30_000 })
    await save()
    const group = (await state()).groups.find((g) => g.name === "Lab servers")
    const refused = await write("DELETE", `api/groups/${group.id}`)
    check(refused.status === 409 && JSON.parse(refused.body).message.includes("EU"), `the daemon refuses a delete of a group a slot uses, naming the slot: ${refused.status} ${refused.body}`)
    await page.getByRole("button", { name: "Delete Lab servers" }).click()
    const doom = page.getByRole("alertdialog")
    const usedBy = doom.getByRole("region", { name: "Used by" })
    await usedBy.getByText("EU", { exact: true }).waitFor()
    check(await doom.getByRole("button", { name: "Delete", exact: true }).isDisabled(), "the page's Delete names EU under Used by and waits for it to move")
    await shot("group-used")
    await usedBy.getByRole("combobox", { name: "Exit of EU" }).click()
    await page.getByRole("dialog").getByRole("option").filter({ hasText: "Good" }).first().click()
    await until("EU's row to leave Used by", 30_000, async () => (await usedBy.count()) === 0)
    await doom.getByRole("button", { name: "Delete", exact: true, disabled: false }).waitFor()
    check((await slotOf(s.id)).exit.kind === "source", "repointed from the dialog to Good, EU leaves Used by and Delete enables")
    await doom.getByRole("button", { name: "Cancel", exact: true }).click()
    await doom.waitFor({ state: "hidden" })
    check(await navRow("Lab servers").count() === 1, "Cancel keeps the group")
    await go("")
    await setExit("EU", "Lab servers")
    await serving("EU")
    await shot("group-kept")
  },

  // Block holds the slot's traffic with its tunnel up, keeps the VPN Client and edits nothing in UniFi, and the way back serves again.
  async block(s) {
    await go("")
    const routes = vm("routes")
    await setExit("EU", "Block")
    await tile("EU").getByRole("img", { name: "Block", exact: true }).waitFor({ timeout: 30_000 })
    pass("on Block the tile's dot says Block")
    await shot("block")
    check(vm("link", s.id).includes("link=up"), "the kernel keeps the tunnel up on Block")
    check(vm("client", "EU").includes("client=1"), "UniFi still holds the VPN Client")
    check(vm("routes") === routes, "UniFi's routes are as they were")
    await setExit("EU", "Lab servers")
    const on = await serving("EU")
    check(vm("link", s.id).includes("link=up"), `back on Lab servers it serves on ${on}, the tunnel up`)
  },

  // Direct applies at once and edits nothing in UniFi: the stand-in holds the same routes after it.
  async direct(s) {
    await go("")
    const routes = vm("routes")
    await setExit("EU", "Direct")
    await tile("EU").getByRole("img", { name: "Direct", exact: true }).waitFor({ timeout: 30_000 })
    await shot("direct")
    check(vm("link", s.id).includes("link=up"), "the tunnel stays up on Direct")
    check(vm("routes") === routes, "UniFi's routes are as they were")
    await setExit("EU", "Lab servers")
    await serving("EU")
    check(vm("routes") === routes, "and after the way back")
  },

  // The menu on a slot's server line, on a group and on a source: a pick closes the menu, and the box is Switching to the server
  // until the slot lands; a pick the real daemon's check passes moves the slot, leaves its Exit and holds for up to a day, as the
  // box says, until Clear; a pick of a dead server fails its check, says so in the toast and on its row in the menu opened by
  // hand, and moves nothing.
  async switch(s) {
    await go("")
    const line = (owner) => tile("EU").getByRole("button", { name: new RegExp(`^Server of ${owner}: `) })
    const menu = page.locator("[data-radix-popper-content-wrapper]").filter({ has: page.locator("[cmdk-list]") })
    const row = (server) => menu.getByRole("option").filter({ hasText: server })
    const open = async (owner) => {
      await line(owner).click()
      await menu.locator("[cmdk-group-heading]").waitFor()
      const head = await menu.locator("[cmdk-group-heading] > span > span").first().textContent()
      check(head === owner && !/Pick|for now/.test(await menu.innerText()), `the menu's head is one line naming ${owner}, and the menu names no pick: '${head}'`)
    }
    const picked = () => tile("EU").getByText(/^Manual override$/)
    const pickOf = async (kind, name) => (await state())[kind].find((x) => x.name === name).preferred
    const before = await serving("EU")
    await open("Lab servers")
    const named = async () => (await menu.getByRole("option").locator("[data-slot=option-name]").allInnerTexts()).map((x) => x.match(LAB_SERVER)?.[0] ?? x)
    const pool = await named()
    const group = (await state()).groups.find((g) => g.name === "Lab servers")
    const order = group.order.map((id) => allLabels.get(id)).filter((x) => LAB_SERVER.test(x))
    check(isDeepStrictEqual(pool, order), `the group's menu lists its servers alone, in the daemon's order: ${pool.join(", ")}${isDeepStrictEqual(pool, order) ? "" : `, the daemon's ${order.join(", ")}`}`)
    await row(before).getByRole("img", { name: "In use: EU", exact: true }).waitFor()
    pass(`the row EU is on, ${before}, says so`)
    await shot("switch-menu")

    const dead = pool.find((x) => x !== before)
    vm("kill", SERVER_OF[dead])
    await row(dead).click()
    await menu.waitFor({ state: "detached" })
    const pending = await tile("EU").getByRole("button", { name: new RegExp(`^Switching to .*${dead}`) }).waitFor({ timeout: 10_000 }).then(() => true, () => false)
    await page.locator("[data-sonner-toast]").filter({ hasText: dead }).filter({ hasText: "failed" }).getByText("No response", { exact: true }).waitFor({ timeout: 60_000 })
    const nothingOpened = await menu.count() === 0
    await line("Lab servers").click()
    await row(dead).getByText("Failed", { exact: true }).waitFor()
    check(pending && nothingOpened && await row(dead).getByText("No response", { exact: true }).count() === 1 && await onServer("EU") === before,
      `a pick of ${dead}, stopped, closes the menu with EU Switching to it, then fails the daemon's check: the toast says ${dead} failed, No response, nothing opens, EU stays on ${before}, and the menu opened by hand says Failed, No response on its row`)
    vm("start-server", SERVER_OF[dead])

    // A pick holds a boost only while its own Test leaves it behind the best ready member. The lab's servers score within a
    // tenth of a millisecond, and one Test moves a short record's median past the others, so the last in the order could come
    // out first and hold nothing. 25 ms on what the pick sends puts its Test above its median, far under every slowness line.
    const chosen = pool.findLast((x) => x !== before && x !== dead)
    vm("delay", SERVER_OF[chosen], "25ms")
    await row(chosen).click()
    await menu.waitFor({ state: "detached", timeout: 60_000 })
    await until(`EU's tile on ${chosen}`, 60_000, async () => (await onServer("EU")) === chosen)
    const after = await slotOf(s.id)
    check(after.exit.kind === "group" && allLabels.get(after.appliedServer) === chosen, `a pick of ${chosen} passes its check and moves EU there, its Exit still the group: ${JSON.stringify({ exit: after.exit, applied: allLabels.get(after.appliedServer) ?? after.appliedServer })}`)
    const says = await picked().waitFor({ timeout: 30_000 }).then(() => true, () => false)
    const held = await pickOf("groups", "Lab servers")
    const words = says ? await picked().innerText() : null
    check(words === "Manual override" && allLabels.get(held?.server) === chosen, `the daemon holds ${chosen} as the group's pick, and EU's box says Manual override under it: ${words}, ${JSON.stringify(held)}, ${JSON.stringify(await slotOf(s.id).then(({ status, pending, server, appliedServer }) => ({ status, pending, server: allLabels.get(server), applied: allLabels.get(appliedServer) })))}`)
    await shot("switch-picked")
    vm("undelay", SERVER_OF[chosen])

    await open("Lab servers")
    const heard = await menu.getByRole("group", { name: "Manual", exact: true }).getByRole("button", { name: /^Manual override\. Auto-resets / }).getAttribute("aria-label")
    check(heard === "Manual override. Auto-resets in 1 day", `the picked row's chip is Manual, heard with when the pick wears off: ${heard}`)
    await menu.getByRole("button", { name: new RegExp(`^Clear manual override on .*${chosen}`) }).click()
    await menu.getByRole("button", { name: /^Clear manual override on / }).waitFor({ state: "detached", timeout: 30_000 })
    await page.keyboard.press("Escape")
    await menu.waitFor({ state: "detached" })
    await picked().waitFor({ state: "detached", timeout: 30_000 })
    const settled = await until("EU's tile on the server the daemon puts it on after Clear", 60_000, async () => {
      const now = await slotOf(s.id)
      const on = allLabels.get(now.appliedServer)
      return on && (await onServer("EU")) === on ? { on, pick: await pickOf("groups", "Lab servers"), exit: now.exit.kind } : null
    })
    check(settled.pick === null && settled.exit === "group" && await picked().count() === 0,
      `Clear drops the group's pick on the daemon and the box's Manual, and EU stands where the daemon puts it, on ${settled.on}, its Exit still the group`)

    await setExit("EU", "Good")
    const fromSource = await serving("EU")
    await open("Good")
    const goods = await named()
    check(goods.length === 2 && goods.every((x) => x.startsWith("good-")), `a source's menu lists its own servers alone: ${goods.join(", ")}`)
    const other = goods.find((x) => x !== fromSource)
    await row(other).click()
    await menu.waitFor({ state: "detached", timeout: 60_000 })
    await until(`EU's tile on ${other}`, 60_000, async () => (await onServer("EU")) === other)
    // A pick of the source's best ready server holds nothing, and a slot that comes onto Good keeps the server it was on, so
    // which it is varies: the box says the pick exactly when the daemon holds it.
    const agree = await until("EU's box to agree with the daemon on Good's pick", 30_000, async () => {
      const held = allLabels.get((await pickOf("sources", "Good"))?.server) === other
      return held === (await picked().count() === 1) ? { held } : null
    }).catch(() => null)
    check((await slotOf(s.id)).exit.kind === "source" && agree !== null,
      `on the source Good, a pick moves EU from ${fromSource} to ${other}, its Exit still the source, and its box says Manual as the daemon holds it: ${agree ? (agree.held ? "held" : "not held") : "they disagree"}`)
    await setExit("EU", "Lab servers")
    await serving("EU")
    check(await picked().count() === 0, "back on Lab servers, with no pick of the group's, EU's box says no Manual")
  },

  async forged(s) {
    vm("forged", s.id)
  },

  // Settings as the real daemon on Linux reads its world: UniFi answering the stand-in's key, a key saved through it, Retention
  // picked and saved, and About's Xray the version the core it runs says it is.
  async settings() {
    await go("")
    await page.locator("header").getByRole("button", { name: "WAN up" }).waitFor()
    check(await page.locator("header").getByRole("button", { name: /^UniFi/ }).count() === 0, "with UniFi answering, the header has no UniFi mark")
    await nav().getByRole("link", { name: "Settings" }).click()
    const unifi = page.locator(`#unifi`)
    await unifi.getByText("UniFi", { exact: true }).waitFor()
    const router = await unifi.locator("svg.lucide-router").getAttribute("class")
    const key = unifi.getByLabel("API key", { exact: true })
    check(/text-success/.test(router) && (await words(unifi.locator("[data-slot=card-header]"))) === "UniFi" && await key.getAttribute("placeholder") === "••••••••••••••••",
      "the UniFi card's router is green with no trouble, and the stored key is the field's dots")
    const accepted = (await state()).unifi.acceptedAt
    // lab/page.sh's UNIFI_KEY, which its secrets step looks for in the logs.
    await key.fill("lab-stand-in-key")
    await unifi.getByRole("button", { name: "Save", exact: true }).click()
    await until("the key's field back to its dots", 30_000, async () => (await key.inputValue()) === "")
    const now = await state()
    check(now.unifiTrouble === null && Date.parse(now.unifi.acceptedAt) > Date.parse(accepted), `a key saved through the page is tried on UniFi and stored: accepted at ${now.unifi.acceptedAt}, after ${accepted}`)

    const retention = page.getByRole("radiogroup", { name: "Retention" })
    const was = await retentionDays()
    const pick = was === 90 ? 365 : 90
    await retention.getByRole("radio", { name: pick === 365 ? "1y" : `${pick}d`, exact: true }).click()
    await page.locator("button[data-control=settings]").click()
    await until("Retention saved", 30_000, async () => (await retentionDays()) === pick)
    pass(`Retention picked and saved: the daemon keeps ${pick} days, from ${was}`)

    const said = inVm(`"$(readlink /proc/$(systemctl show -p MainPID --value ${CORE_UNIT})/exe)" version | head -1`)
    const version = said.split(/\s+/)[1]
    const about = page.locator("main [data-slot=card]").filter({ has: page.getByText("About", { exact: true }) })
    await about.getByText(`v${version}`, { exact: true }).waitFor({ timeout: 30_000 })
    const core = (await state()).core
    check(/^\d+\.\d+\.\d+$/.test(version) && core.running === version && core.version === version && (await words(about)) === `About sidegate dev Xray v${version}`,
      `About shows the Xray the core runs, as it says itself in the VM, '${said}': v${version}, plain, with no other, under a development build's version alone`)
    await shot("settings")
  },

  // A real server dies with the page open, and the switch arrives with no reload. The slot carries no traffic, so
  // row 7e's bound applies to the daemon's own switch time: it finds a dead server within 80 s. The page gets
  // 10 s more to show it.
  async failover(s) {
    await go("")
    const before = await serving("EU")
    const load = documents
    const framesBefore = frames.length
    const vmKilledAt = Number(vm("kill", SERVER_OF[before]).match(/stopped \S+ at epoch (\d+(?:\.\d+)?)/)?.[1])
    const killedAt = Date.now()
    const after = await until(`EU's tile off ${before}`, 90_000, async () => { const now = await onServer("EU"); return now && now !== before ? now : null })
    pass(`${((Date.now() - killedAt) / 1000).toFixed(1)} s after killing ${before} the open page shows ${after}`)
    const move = daemonMove(await (await context.request.get(URL_ + "api/switches")).json(), s.id, vmKilledAt, 80)
    check(move.ok, move.what)
    check(documents === load, "the page never reloaded")
    const seen = frames.slice(framesBefore).map((f) => f.kind)
    check(seen.includes("switch"), `the switch arrived as SSE frames: ${[...new Set(seen)].join(", ")}`)
    await shot("switched")
    vm("start-server", SERVER_OF[before])
    await nav().getByRole("link", { name: "Events" }).click()
    await page.getByText("Server failed").first().waitFor()
    pass("the events page says why it moved")
    await shot("events")
  },

  // The feed moves a server, and the Events list says so without calling it a switch.
  async replaced() {
    vm("move-acme")
    await go("servers")
    await navRow("Acme").click()
    await page.locator("[data-slot=source-heading]").getByRole("button", { name: /^Refresh/ }).click()
    await page.waitForTimeout(2000)
    await go("events")
    await page.getByText("Server replaced").first().waitFor({ timeout: 30_000 })
    pass("Events shows 'Server replaced'")
    await shot("replaced")
  },

  // The real daemon's analytics on the week of the provider EU is on: its days in use.
  async analytics() {
    await go("")
    const on = await serving("EU")
    const provider = on.startsWith("good-") ? "Good" : "Bad"
    await go("servers")
    await navRow(provider).click()
    const week = page.locator("[data-slot=source-heading]").getByRole("button", { name: /^(No failovers|\d+ failovers?) (today|in the last)/ })
    let text = ""
    /* The week is read again as the clock passes each of its cells, so each poll reads its sentence again. */
    const used = await until(`${provider}'s week showing a day in use`, 240_000, async () => {
      text = (await week.getAttribute("aria-label").catch(() => null)) ?? ""
      return /, in use( on [1-7] of them)?$/.test(text) ? text : undefined
    }).catch(() => null)
    check(used !== null, `${provider}'s week shows the provider's real days in use: ${used ?? text}`)
    await shot("analytics")
  },
}

// The one press and the one confirm: Check finds v0.0.2 on the stand-in, the confirm says what it would cost, the page goes away
// with the daemon and reloads itself onto the new release.
async function update() {
  await go("settings")
  const line = page.locator("[data-slot=update-line]")
  const offered = page.getByRole("img", { name: "Update to v0.0.2" })
  const press = line.getByRole("button", { name: "Update", exact: true })
  const checkNow = line.getByRole("button", { name: "Check for updates", exact: true })
  const session = async () => (await context.request.get(URL_ + "api/session")).json()
  const heard = async () => (await line.textContent()).replace(/\s+/g, " ").trim()
  await line.getByText("v0.0.1", { exact: true }).waitFor()
  check((await words(line)) === "v0.0.1" && await offered.count() === 0, "About shows v0.0.1, with nothing offered since the day's check found nothing newer")
  await checkNow.click()
  await press.waitFor({ timeout: 30_000 })
  check((await heard()) === "Running v0.0.1, newer v0.0.2Update" && await offered.count() === 1 && await checkNow.count() === 0, "a press of Check offers v0.0.2, and the mark stands on Settings")
  const notes = await line.getByRole("link", { name: "Notes for v0.0.2" }).getAttribute("href")
  check(notes === new URL("/OWNER/sidegate/releases/tag/v0.0.2", FEEDS).href, `its notes are the release's page: ${notes}`)
  await shot("update-offered")
  await press.click()
  const dialog = page.getByRole("alertdialog", { name: "Update to v0.0.2?" })
  await dialog.waitFor()
  check((await words(dialog)) === "Update to v0.0.2? Cancel Update", "its confirm has no warning, since both releases run one core")
  await shot("update-confirm")
  const before = { session: await session(), documents }
  const posts = []
  page.on("request", (r) => { if (r.method() === "POST" && r.url().endsWith("/api/update")) posts.push(r.postData()) })
  await dialog.getByRole("button", { name: "Update", exact: true }).click()
  await page.waitForFunction(() => document.querySelector("[data-slot=update-line] button[aria-busy=true]")?.disabled)
  check((await heard()) === "Running v0.0.1, newer v0.0.2Update", "confirmed, Update stays, disabled and Pending")
  await shot("update-running")
  const pressed = Date.now()
  await page.getByRole("button", { name: /^Gateway unreachable since / }).waitFor({ timeout: 120_000 })
  pass(`${((Date.now() - pressed) / 1000).toFixed(1)} s later the page has lost the daemon`)
  await shot("update-offline")
  await until("the page reloaded itself", 60_000, async () => documents === before.documents + 1)
  await line.getByText("v0.0.2", { exact: true }).waitFor({ timeout: 30_000 })
  await checkNow.waitFor({ timeout: 60_000 })
  const now = await session()
  check((await words(line)) === "v0.0.2" && await offered.count() === 0 && now.version === "v0.0.2" && now.release !== before.session.release,
    `${((Date.now() - pressed) / 1000).toFixed(1)} s after the press the page has reloaded itself once, About shows v0.0.2, which answers as ${now.release}, and the mark is gone`)
  const view = await (await context.request.get(URL_ + "api/update")).json()
  check(view.running === null && view.last.outcome === "landed" && view.last.version === "v0.0.2", `the daemon says it landed: ${JSON.stringify(view.last)}`)
  check(posts.length === 1 && posts[0] === '{"version":"v0.0.2","restartsCore":false}' && documents === before.documents + 1, `one press was sent: ${posts.join(" ")}`)
  check(await page.locator("[data-sonner-toast]").count() === 0, "and nothing was said")
  await shot("update-landed")
}

async function run(name, scenario) {
  console.log(`--- ${name}`)
  try {
    await scenario()
  } catch (error) {
    fail(`${name}: ${error.message.split("\n").slice(0, 4).join(" ")}`)
    await shot(`failure-${name}`).catch(() => {})
  }
}

try {
  if (WORLD === "update") await run("update", update)
  else {
    await sources()
    const s = await slot()
    for (const [name, scenario] of Object.entries(scenarios)) if (!ONLY || ONLY.includes(name)) await run(name, () => scenario(s))
  }
  check(pageErrors.length === 0, `no console errors${pageErrors.length ? `: ${pageErrors.slice(0, 3).join(" | ")}` : ""}`)
} catch (error) {
  fail(error.message.split("\n").slice(0, 4).join(" "))
  await shot("failure").catch(() => {})
}
await browser.close()
process.exit(failed ? 1 : 0)
