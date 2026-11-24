// Drives the built page against the fake daemon, seeded with a full gateway, and saves what it shows at desktop and phone
// widths: `node tests/e2e/shots.mjs [outdir] [scene]`. With `slots`, only a settled slot's line in its states; with `failure`,
// only a failing refresh's card in its states; with `add`, only the board's Add, its card, a slot's delete, the setup's steps and UniFi's cards; with `readme`,
// only the README's pictures: `node tests/e2e/shots.mjs ../docs/img readme`; with `about`, only Settings' About in its states; with `update`, only
// About's update line, its confirm and its marks in every state the daemon can answer.
import assert from "node:assert/strict"
import { mkdirSync, rmSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { chromium } from "playwright-core"
import { build } from "vite"
import { failureStates, groupView, inBuckets, acme as referenceFeed, ranked, server, slotView, source, startFakeDaemon, STEP_MS, troubledSources } from "./fake-daemon.mjs"

const ROOT = fileURLToPath(new URL("../../", import.meta.url))
const DIST = fileURLToPath(new URL("./.dist-shots/", import.meta.url))
const OUT = (process.argv[2] ?? fileURLToPath(new URL("../shots-r13", import.meta.url))).replace(/\/$/, "")
const PORT = Number(process.env.E2E_PORT ?? 5391)
const BASE = "/proxy/"
const WIDTHS = { desktop: { width: 1440, height: 900 }, phone: { width: 390, height: 844 } }
/** The server tables again at each width the phone layout was set for, and at a laptop's. */
const TABLE_WIDTHS = [360, 390, 768, 1280]
/** The Stability column's states, in the table and the Exit menu. */
const STABILITY_WIDTHS = [360, 768, 1280]
/** A source's heading in each state, and the list, at the drawing's widths. */
const HEADING_WIDTHS = [1280, 390]
/** A settled slot's line in its states, at a laptop's and a phone's widths. */
const SLOT_WIDTHS = [1280, 390]
/** A failing refresh's card in each of its states, over its heading, at a laptop's and a phone's widths. */
const FAILURE_WIDTHS = [1280, 390]
/** Add's card, a slot's delete, the setup's steps and the cards of one line or several: UniFi's in each trouble, a failed read's and Skipped's. */
const ADD_WIDTHS = [1280, 390]
/** The README's pictures: each name, its width and theme. */
/** Settings with About as a release, its update check off and Pending, a checkout build, waiting for its reads and with them failed, at a
 * laptop's and a phone's widths. */
const ABOUT_WIDTHS = [1280, 390]
/** About's update line in each state, at a laptop's and a phone's widths. */
const UPDATE_WIDTHS = [1280, 390]
const README_SHOTS = [["dashboard", 1280, "dark"], ["source", 1280, "dark"], ["events", 1280, "dark"]]
const SCENE = process.argv[3] ?? null
assert.ok(SCENE === null || ["slots", "failure", "add", "readme", "about", "update"].includes(SCENE), `no scene ${SCENE}`)
/** The widths a part of the run takes: every part runs with no scene named, and only its own part with one. */
const widths = (scene, list) => (SCENE === null || SCENE === scene ? list : [])

const H = (hours) => new Date(Date.now() - hours * 3600e3).toISOString()
const health = (ping) => ({ verdict: "succeeded", checkedAt: H(0.1), pingAt: H(0.1), current: true, pingP50Ms: ping, pingP95Ms: Math.round(ping * 1.4), newConnectionP50Ms: ping * 3, newConnectionP95Ms: ping * 5, score: Math.round(ping * 1.04), factor: 1.04, incidents7d: 0, seenDays: 5.2, seenWorking7dMs: 7 * 86_400_000, lastFailureAt: null, firstSeenAt: H(24 * 30) })
const silent = { verdict: "unknown", checkedAt: null, pingAt: null, current: false, pingP50Ms: null, pingP95Ms: null, newConnectionP50Ms: null, newConnectionP95Ms: null, score: null, factor: 1.25, incidents7d: 0, seenDays: 0, seenWorking7dMs: 0, lastFailureAt: null, firstSeenAt: H(24 * 30) }
const config = (key, name, place, servers) => ({ key, name, place, servers, skipped: [] })

/** The reference feed as the fake daemon sends it, 6 configs of 2 servers, with pings of its own and two never measured. */
const reference = referenceFeed(2)
const UNMEASURED = new Set(["Germany · EDGE-STANDBY", "Netherlands · EDGE-STANDBY"])
const acme = reference.servers.map((s) => ({ ...s, health: UNMEASURED.has(s.label) ? silent : health(s.health.pingP50Ms) }))
const [DE, NL, SE] = ["Germany · proxy", "Netherlands · proxy", "Sweden · proxy"].map((label) => acme.find((s) => s.label === label).id)
const ACME_ORDER = ranked(acme.map((s) => s.id), acme)
const aurora = [
  server("l-ams", "Amsterdam", "NL", 15, { health: { ...health(15), incidents7d: 2, lastFailureAt: H(30), factor: 1.3 } }),
  server("l-fra", "Frankfurt", "DE", 9, { health: health(9) }),
  server("l-par", "Paris", "FR", 19, { health: { ...health(19), verdict: "failed", incidents7d: 9, factor: 2.6 } }),
  server("l-ber", "Berlin", "DE", 13, { health: { ...health(13), current: false, checkedAt: H(12 * 24), pingAt: H(12 * 24), score: 16, factor: 1.19, seenDays: 0.4 } }),
]

const ACME = {
  ...reference.source, firstChoice: ACME_ORDER[0], order: ACME_ORDER,
  diagnostics: [{ entry: 4, code: "unsupported_protocol", field: "wireguard" }],
  origin: {
    ...source(2).origin,
    provider: { ...source(2).origin.provider, title: "Acme", announcement: "Maintenance on Sunday night.", quota: { upload: 2e9, download: 18e9, limit: 100e9, expires: H(-24 * 40), refills: H(-24 * 10), observedAt: H(1) } },
    fetch: { checkedAt: H(0.5), updatedAt: H(0.5), failures: 0, failureCode: null },
  },
}
const AURORA = { ...source(1), feed: { configs: [config("l-ams", "Amsterdam", 1, 1), config("l-fra", "Frankfurt", 2, 1), config("l-par", "Paris", 3, 1)] }, origin: { ...source(1).origin, fetch: { checkedAt: H(0.2), updatedAt: H(40), failures: 3, failureCode: "http_503" } } }

const unifi = (name, ordinal, rest = {}) => ({ name, interface: `pxa${ordinal}`, known: true, client: "present", trouble: null, ...rest })
const placed = (id, ordinal, input, rest = {}, unifiRest = {}) => ({ ...slotView(id, 3, input), appliedServer: rest.appliedServer ?? DE, server: rest.appliedServer ?? DE, ...rest, unifi: unifi(input.name, ordinal, unifiRest), backups: [NL, SE] })

const SLOTS = [
  placed("eu", 1, { name: "EU", exit: { kind: "group", group: "acme" } }),
  placed("nl", 2, { name: "NL only", exit: { kind: "server", server: NL } }, { appliedServer: NL }),
  placed("tv", 3, { name: "TV", exit: { kind: "block" } }, { appliedServer: null, server: null, backups: [] }),
  placed("down", 4, { name: "Backup", exit: { kind: "server", server: "l-par" } }, { status: "failed", downReason: "confirmedDead", appliedServer: null, server: null }),
  placed("off", 5, { name: "Lab", exit: { kind: "group", group: "acme" } }, { appliedServer: SE, server: SE, leavingBy: H(-0.005) }, { client: "disabled" }),
  placed("spare", 6, { name: "Spare", exit: { kind: "source", source: "aurora" } }, { appliedServer: "l-ams", server: "l-ams" }, { client: "deleted" }),
  placed("wan", 7, { name: "Guests", exit: { kind: "direct" } }, { appliedServer: null, server: null, backups: [] }),
]

const among = (values) => ({ not: false, values })
const rule = (sources, countries = [], box = null) => ({ sources: among(sources), countries: among(countries), protocols: among([]), include: "", exclude: "", box })
const GROUPS = [
  { ...groupView("acme", 1, { name: "Acme", members: rule(["acme"]) }, ["eu", "off"]), resolved: ACME_ORDER, order: ACME_ORDER, firstChoice: ACME_ORDER[0] },
  { ...groupView("germany", 1, { name: "Germany", members: rule([], ["DE"]) }, ["tv"]), resolved: [DE, "l-fra"], order: [DE, "l-fra"], firstChoice: DE },
  { ...groupView("mine", 2, { name: "Fast EU", members: rule([], [], "ticked"), ticked: [DE, "l-fra"] }), resolved: [DE, "l-fra"], order: [DE, "l-fra"], firstChoice: DE },
]

const SWITCHES = [
  { id: "s1", owner: { kind: "group", group: "acme" }, slots: ["eu"], intent: 1, from: { kind: "up", server: NL }, to: { kind: "up", server: DE }, reason: "failureMove", requestedAt: H(2), appliedAt: H(2),
    evidence: [{ server: NL, kind: "activeProbe", verdict: "failed", target: "gstatic-204", round: 1, at: H(2), pingMs: null, newConnectionMs: null }] },
  { id: "s2", owner: { kind: "group", group: "acme" }, slots: ["eu"], intent: 1, from: { kind: "down", reason: "unselected" }, to: { kind: "up", server: NL }, reason: "initialSelection", requestedAt: H(30), appliedAt: H(30), evidence: [] },
]

/** Acme's week, in the daemon's 15-minute steps: one server in use for three hours from noon on each of the last three days, and
 * two days ago a failover at 05:45 and another at 06:15, each in its own six hours, so its week's line has two dashed lines. */
const acmeSteps = [1, 2, 3].flatMap((daysAgo) => {
  const at = (h, m) => { const d = new Date(); d.setDate(d.getDate() - daysAgo); return d.setHours(h, m, 0, 0) }
  const step = (start, served, forced) => ({
    start, source: "acme", lineage: DE, country: "DE", uploaded: 0, downloaded: 0, probeSuccesses: 0, probeAttempts: 0, wanExcluded: 0, unknown: 0,
    pingMedianMs: null, pingP95Ms: null, newConnectionMedianMs: null, forcedSwitchAways: forced, servedSeconds: served, incidents: forced, managementTrafficIncluded: false,
  })
  return [
    ...Array.from({ length: 12 }, (_, i) => step(at(12, 0) + i * STEP_MS, 900, 0)),
    ...(daysAgo === 2 ? [step(at(5, 45), 0, 1), step(at(6, 15), 0, 1)] : []),
  ]
}).filter((s) => s.start <= Date.now())
const acmeWeek = (from, to, bucket, lineages = [], sources = []) => inBuckets(acmeSteps, from, to, bucket, lineages, sources)

/** EU on Acme, its pick held on Germany · proxy so its Manual line shows, and NL only on its server, both connected and settled:
 * as the runtime vouches for each server, after a pass that vouched for neither, and with EU's move still pending. */
const HELD = { server: DE, endsAt: H(-2) }
const unvouched = (slot, rest = {}) => ({ ...slot, appliedServer: null, runtime: { ...slot.runtime, phase: "unknown", applied: { kind: "down", reason: "unselected" } }, ...rest })
const SETTLED = SLOTS.slice(0, 2)
const SLOT_STATES = [
  ["settled", SETTLED],
  ["unconfirmed", SETTLED.map((s) => unvouched(s))],
  ["unconfirmed-moving", [unvouched(SETTLED[0], { pending: true }), unvouched(SETTLED[1])]],
]

/** EU and Home on Acme, on Germany · proxy, beside NL only on its own server: a press of Netherlands · proxy on EU's line, out,
 * landed, failed with its toast alone and then its menu opened by hand, failed while another box's Exit menu is open, and failed
 * while the reader was on All servers, its menu opened by hand back on the board. */
const PRESSED = [SLOTS[0], placed("home", 8, { name: "Home", exit: { kind: "group", group: "acme" } }), SLOTS[1]]

/** The pick's drawing's scene: EU and Home on the source Acme, on Germany · EDGE-STANDBY at the owner's 74 / 735 ms, first in
 * its order, and Lab on Fast EU; with `left`, the source's pick of that server ends that many ms from now. */
const ORIGIN = acme.find((s) => s.label === "Germany · EDGE-STANDBY").id
const onAcme = (id, ordinal, name) => placed(id, ordinal, { name, exit: { kind: "source", source: "acme" } }, { appliedServer: ORIGIN })
const PICK_SLOTS = [onAcme("eu", 1, "EU"), onAcme("home", 8, "Home"), placed("lab", 9, { name: "Lab", exit: { kind: "group", group: "mine" } }, { appliedServer: "l-fra" })]
const pickScene = (left) => ({
  catalogue: [...acme.map((s) => (s.id === ORIGIN ? { ...s, health: { ...health(74), pingP95Ms: 735 } } : s)), ...aurora],
  sources: new Map([["acme", { ...ACME, order: [ORIGIN, ...ACME_ORDER.filter((id) => id !== ORIGIN)], firstChoice: ORIGIN, preferred: left === null ? null : { server: ORIGIN, endsAt: new Date(Date.now() + left).toISOString() } }], ["aurora", AURORA]]),
  slots: new Map(PICK_SLOTS.map((x) => [x.id, x])),
  groups: new Map(GROUPS.map((g) => [g.id, g])),
  vpnClients: { known: true, limit: 8, foreign: [] },
})

/** The README's world: a group, a source and two servers as Exits, the last one closed on its dead server, and a week of moves
 * with their reasons, the newest a failover whose checks show the server it left failing and the one it took passing. */
const README_GROUP = { ...groupView("europe", 1, { name: "Europe", members: rule(["acme"]) }, ["home"]), resolved: ACME_ORDER, order: ACME_ORDER, firstChoice: ACME_ORDER[0] }
const README_SLOTS = [
  placed("home", 1, { name: "Home", exit: { kind: "group", group: "europe" } }),
  placed("office", 2, { name: "Office", exit: { kind: "source", source: "aurora" } }, { appliedServer: "l-fra", server: "l-fra", backups: ["l-ams"] }),
  placed("tv", 3, { name: "TV", exit: { kind: "server", server: NL } }, { appliedServer: NL, server: NL, backups: [] }),
  placed("backup", 4, { name: "Backup", exit: { kind: "server", server: "l-par" } }, { status: "failed", downReason: "confirmedDead", appliedServer: null, server: null, backups: [] }),
]
const probe = (server, verdict, round, hoursAgo, pingMs) => ({ server, kind: "activeProbe", verdict, target: "gstatic-204", round, at: H(hoursAgo), pingMs, newConnectionMs: pingMs === null ? null : pingMs * 3 })
const move = (id, owner, slots, from, to, reason, hoursAgo, evidence = []) => ({ id, owner, slots, intent: 1, from, to, reason, requestedAt: H(hoursAgo), appliedAt: H(hoursAgo - 0.001), evidence })
const up = (server) => ({ kind: "up", server })
const EUROPE = { kind: "group", group: "europe" }
const OFFICE = { kind: "source", source: "aurora" }
const README_SWITCHES = [
  move("r1", EUROPE, ["home"], up(NL), up(DE), "failureMove", 0.4,
    [probe(NL, "failed", 1, 0.4, null), probe(DE, "succeeded", 1, 0.4, 41), probe(NL, "failed", 2, 0.4 - 1 / 3600, null), probe(DE, "succeeded", 2, 0.4 - 1 / 3600, 40)]),
  move("r2", OFFICE, ["office"], up("l-ams"), up("l-fra"), "improvementMove", 5, [probe("l-ams", "succeeded", 3, 5, 31), probe("l-fra", "succeeded", 3, 5, 9)]),
  move("r3", EUROPE, ["home"], up(SE), up(NL), "preferred", 9),
  move("r4", OFFICE, ["office"], up("l-par"), up("l-ams"), "udpMove", 20, [probe("l-par", "failed", 1, 20, null), probe("l-ams", "succeeded", 1, 20, 15)]),
  move("r5", EUROPE, ["home"], up(DE), up(SE), "slownessMove", 31, [probe(DE, "succeeded", 1, 31, 412), probe(SE, "succeeded", 1, 31, 55)]),
  move("r6", OFFICE, ["office"], { kind: "down", reason: "unselected" }, up("l-par"), "initialSelection", 70),
  move("r7", EUROPE, ["home"], { kind: "down", reason: "unselected" }, up(DE), "initialSelection", 70),
]
const README_AURORA = { ...AURORA, origin: { ...AURORA.origin, fetch: { checkedAt: H(0.2), updatedAt: H(0.2), failures: 0, failureCode: null } } }

function seed(daemon) {
  daemon.set({
    analytics: acmeWeek,
    catalogue: [...acme, ...aurora],
    sources: new Map([["acme", ACME], ["aurora", AURORA]]),
    slots: new Map(SLOTS.map((s) => [s.id, s])),
    groups: new Map(GROUPS.map((g) => [g.id, g])),
    vpnClients: { known: true, limit: 8, foreign: [{ id: "n-office", name: "Office WireGuard", enabled: true }] },
    switches: SWITCHES,
  })
}

async function shoot(page, name, device, viewport = WIDTHS[device]) {
  const path = `${OUT}/${name}-${device}.png`
  // The page scrolls inside its pane, so only a viewport as tall as the pane's content shows all of it.
  const height = await page.evaluate(() => document.querySelector("[data-slot=main] > div:last-child")?.scrollHeight ?? 0)
  await page.setViewportSize({ width: viewport.width, height: Math.max(viewport.height, height + 60) })
  await page.waitForTimeout(150)
  await page.screenshot({ path })
  await page.setViewportSize(viewport)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)
  assert.ok(overflow <= 0, `${name} on ${device} scrolls sideways by ${overflow}px`)
  console.log(path)
}

rmSync(DIST, { recursive: true, force: true })
mkdirSync(OUT, { recursive: true })
process.env.BASE_PATH = BASE
await build({ root: ROOT, logLevel: "warn", build: { outDir: DIST, emptyOutDir: true } })
const daemon = await startFakeDaemon({ port: PORT, dist: DIST, base: BASE })
const browser = await chromium.launch()
const errors = []
try {
  for (const width of widths("slots", SLOT_WIDTHS)) {
    for (const theme of ["dark", "light"]) {
      daemon.reset()
      seed(daemon)
      const viewport = { width, height: width < 768 ? 844 : 900 }
      const context = await browser.newContext({ viewport, colorScheme: theme, hasTouch: width < 768 })
      await context.addInitScript((t) => localStorage.setItem("theme", t), theme)
      const page = await context.newPage()
      page.setDefaultTimeout(10_000)
      page.on("console", (m) => { if (m.type() === "error" && !/ERR_INCOMPLETE_CHUNKED_ENCODING/.test(m.text())) errors.push(`${width} ${theme} slots: ${m.text()}`) })
      page.on("pageerror", (e) => errors.push(`${width} ${theme} slots: ${e.message}`))
      const at = `${width}-${theme}`
      const card = page.locator("[data-radix-popper-content-wrapper]")
      /* A card fades in, so a shot of one waits it out. */
      const save = async (name) => { await page.waitForTimeout(400); await page.screenshot({ path: `${OUT}/${name}-${at}.png` }); console.log(`${OUT}/${name}-${at}.png`) }
      for (const [name, slots] of SLOT_STATES) {
        daemon.set({ slots: new Map(slots.map((x) => [x.id, x])), groups: new Map(GROUPS.map((g) => [g.id, g.id === "acme" ? { ...g, preferred: HELD } : g])), vpnClients: { known: true, limit: 8, foreign: [] } })
        await page.goto(daemon.url)
        await page.getByRole("group", { name: "NL only" }).waitFor()
        await page.waitForTimeout(250)
        await shoot(page, `slot-${name}`, at, viewport)
        if (name !== "unconfirmed") continue
        const nl = page.getByRole("group", { name: "NL only" }).getByRole("button", { name: /Netherlands · proxy/ })
        if (width < 768) await nl.tap()
        else await nl.hover()
        await card.first().getByText(/Netherlands · proxy/).waitFor()
        await save("slot-unconfirmed-card")
        await page.mouse.move(0, 0)
        if (width < 768) await page.getByRole("heading").first().tap()
        await card.first().waitFor({ state: "detached" })
        const eu = page.getByRole("group", { name: "EU" }).getByRole("button", { name: /^(Server of Acme|Last active server of Acme|Switching)/ })
        if (width >= 768) {
          await eu.hover()
          await card.first().waitFor()
          await save("slot-unconfirmed-line-card")
          await page.mouse.move(0, 0)
          await card.first().waitFor({ state: "detached" })
        }
        if (/of Acme/.test(await eu.getAttribute("aria-label") ?? "")) {
          await eu.click()
          await page.getByRole("dialog").getByText("Stability").first().waitFor()
          await page.waitForTimeout(250)
          await save("slot-unconfirmed-menu")
          await page.keyboard.press("Escape")
        }
      }
      const eu = page.getByRole("group", { name: "EU" })
      const menu = page.locator("[data-radix-popper-content-wrapper]").filter({ has: page.locator("[cmdk-list]") })
      const target = menu.getByRole("option").filter({ hasText: "Netherlands · proxy" })
      const answered = () => page.waitForResponse((r) => r.url().includes("/preferred/") && r.status() === 200)
      /* The press waits here until it is let through. */
      const hold = async () => {
        let release
        const held = new Promise((r) => { release = r })
        await page.route((url) => url.pathname.includes("/preferred/"), async (route) => { await held; await route.continue().catch(() => {}) }, { times: 1 })
        return () => release()
      }
      const pressing = async (verdict) => {
        daemon.set({ slots: new Map(PRESSED.map((x) => [x.id, x])), groups: new Map(GROUPS.map((g) => [g.id, g])), vpnClients: { known: true, limit: 8, foreign: [] }, preferVerdict: verdict, holdPreferLanding: true })
        await page.goto(daemon.url)
        await eu.getByRole("button", { name: /^Server of Acme: / }).click()
        await target.waitFor()
      }
      /* A toast on its way out still matches until it is gone. */
      const failedToast = page.locator("[data-sonner-toast]:not([data-removed=true])").filter({ hasText: "failed" })
      const switching = async () => { for (const name of ["EU", "Home"]) await page.getByRole("group", { name }).getByRole("button", { name: /^Switching to Netherlands · proxy/ }).waitFor() }
      await pressing("succeeded")
      let release = await hold()
      await target.click()
      await menu.waitFor({ state: "detached" })
      await switching()
      await page.mouse.move(0, 0)
      await shoot(page, "slot-press-out", at, viewport)
      const passed = answered()
      release()
      await passed
      await page.waitForTimeout(250)
      await switching()
      daemon.readback("eu")
      daemon.readback("home")
      for (const name of ["EU", "Home"]) await page.getByRole("group", { name }).getByRole("button", { name: /^Server of Acme: Netherlands · proxy/ }).waitFor()
      await shoot(page, "slot-press-landed", at, viewport)
      await pressing("failed")
      await target.click()
      await menu.waitFor({ state: "detached" })
      await failedToast.getByText("No response", { exact: true }).waitFor()
      for (const name of ["EU", "Home"]) await page.getByRole("group", { name }).locator("[data-slot=switch-line]:not([aria-disabled])").waitFor()
      await page.mouse.move(0, 0)
      await save("slot-press-failed")
      await eu.getByRole("button", { name: /^Server of Acme: / }).click()
      await target.getByText("No response", { exact: true }).waitFor()
      await page.mouse.move(0, 0)
      await save("slot-press-failed-menu")
      await page.keyboard.press("Escape")
      await menu.waitFor({ state: "detached" })
      release = await hold()
      await eu.getByRole("button", { name: /^Server of Acme: / }).click()
      await target.click()
      await menu.waitFor({ state: "detached" })
      await switching()
      await page.getByRole("group", { name: "NL only" }).getByRole("combobox", { name: "Exit of NL only" }).click()
      await page.getByRole("dialog").first().waitFor()
      const failed = answered()
      release()
      await failed
      await failedToast.waitFor()
      await page.mouse.move(0, 0)
      await save("slot-press-failed-other-menu")
      await page.keyboard.press("Escape")
      await page.getByRole("dialog").waitFor({ state: "detached" })
      /* The same press failing while the reader is on All servers: its toast there, and back on the board the boxes on their
       * server and EU's menu opened by hand with the failure on its row. */
      const nav = page.getByRole("navigation", { name: "Main" })
      const goTo = async (link) => {
        if (width < 768) await page.getByRole("button", { name: "Menu" }).click()
        await link().click()
        await page.mouse.move(0, 0)
      }
      release = await hold()
      await eu.getByRole("button", { name: /^Server of Acme: / }).click()
      await target.click()
      await menu.waitFor({ state: "detached" })
      await switching()
      await failedToast.waitFor({ state: "detached" })
      await goTo(() => nav.locator('a[href$="/servers"]'))
      await page.getByRole("group", { name: "EU" }).waitFor({ state: "detached" })
      const away = answered()
      release()
      await away
      await failedToast.getByText("No response", { exact: true }).waitFor()
      await save("slot-press-failed-away")
      await goTo(() => nav.getByRole("link", { name: "Dashboard" }))
      for (const name of ["EU", "Home"]) await page.getByRole("group", { name }).locator("[data-slot=switch-line]:not([aria-disabled])").waitFor()
      await eu.getByRole("button", { name: /^Server of Acme: / }).click()
      await target.getByText("No response", { exact: true }).waitFor()
      await page.mouse.move(0, 0)
      await save("slot-press-failed-back-menu")
      await page.keyboard.press("Escape")
      await menu.waitFor({ state: "detached" })

      const word = menu.getByRole("group", { name: "Manual", exact: true }).getByRole("button", { name: /^Manual override\. Auto-resets / })
      const pickMenu = async () => {
        await eu.getByRole("button", { name: /^Server of Acme: / }).click()
        await menu.locator("[cmdk-group-heading]").waitFor()
        await page.mouse.move(0, 0)
      }
      /* A pick just made and one with 5 minutes left: the board, the menu, and the word's card, by the pointer on a wide screen
       * just after the pick and by a tap on a phone at 5 minutes, as the drawing has them. */
      for (const [time, left] of [["1d", 86_400_000 - 5_000], ["5m", 300_000]]) {
        daemon.set(pickScene(left))
        await page.goto(daemon.url)
        await eu.getByText("Manual override").waitFor()
        await page.waitForTimeout(250)
        await shoot(page, `pick-board-${time}`, at, viewport)
        if (width >= 768 && time === "1d") {
          const words = await eu.getByText("Manual override").boundingBox()
          await page.mouse.move(words.x + words.width / 2, words.y + words.height / 2)
          await page.waitForTimeout(250)
          await shoot(page, "pick-words-pointer", at, viewport)
        }
        await pickMenu()
        await word.waitFor()
        await save(`pick-menu-${time}`)
        if ((width < 768) !== (time === "5m")) continue
        if (width < 768) await word.tap()
        else await word.hover()
        await card.filter({ hasText: "Auto-resets" }).waitFor()
        await save(`pick-card-${time}`)
      }
      daemon.set(pickScene(86_400_000 - 5_000))
      await page.goto(daemon.url)
      await pickMenu()
      release = await hold()
      await menu.getByRole("button", { name: /^Clear manual override on / }).click()
      await menu.locator("button[aria-busy=true]").waitFor()
      await page.mouse.move(0, 0)
      await save("pick-clearing")
      release()
      await menu.getByRole("group", { name: "Manual", exact: true }).waitFor({ state: "detached" })
      daemon.set(pickScene(null))
      await page.goto(daemon.url)
      await eu.getByRole("button", { name: /^Server of Acme: / }).waitFor()
      await page.waitForTimeout(250)
      await shoot(page, "pick-none-board", at, viewport)
      await pickMenu()
      await save("pick-none-menu")
      await context.close()
    }
  }
  for (const [device, viewport] of widths("all", Object.entries(WIDTHS))) {
    daemon.reset()
    seed(daemon)
    const context = await browser.newContext({ viewport, colorScheme: "dark" })
    const page = await context.newPage()
    page.setDefaultTimeout(10_000)
    // The browser itself logs the refused save this script provokes, and the event stream the fake's reset cuts.
    page.on("console", (m) => { if (m.type() === "error" && !/412|ERR_INCOMPLETE_CHUNKED_ENCODING/.test(m.text())) errors.push(`${device}: ${m.text()}`) })
    page.on("pageerror", (e) => errors.push(`${device}: ${e.message}`))
    const go = async (path, ready) => { await page.goto(daemon.url + path); await page.getByText(ready, { exact: false }).first().waitFor(); await page.waitForTimeout(250) }

    await go("", "Office WireGuard")
    await shoot(page, "board", device)
    await page.getByRole("combobox", { name: "Exit of EU" }).click()
    await page.getByRole("dialog").getByText("Stability").first().waitFor()
    await shoot(page, "exit-menu", device)
    await page.keyboard.press("Escape")
    await page.locator("header").getByRole("button", { name: /^WAN/ }).hover()
    await page.locator("[data-radix-popper-content-wrapper]").filter({ hasText: "WAN" }).waitFor()
    await shoot(page, "top-bar-card", device)
    await page.mouse.move(0, 0)
    daemon.set({ wan: "down" })
    await go("", "Office WireGuard")
    await page.locator("header").getByRole("button", { name: "WAN down" }).waitFor()
    await shoot(page, "wan-down", device)
    daemon.set({ wan: "up" })

    daemon.set({ slots: new Map(SLOTS.slice(0, 2).map((s) => [s.id, s])), vpnClients: { known: true, limit: 8, foreign: [] } })
    await go("", "NL only")
    await shoot(page, "board-empty-slots", device)
    daemon.set({ slots: new Map(SLOTS.map((s) => [s.id, s])), vpnClients: { known: true, limit: 8, foreign: [{ id: "n-office", name: "Office WireGuard", enabled: true }] } })

    await go("servers", "Frankfurt")
    await page.getByRole("img", { name: "Unchecked" }).first().waitFor()
    await shoot(page, "servers", device)
    await page.locator("tbody tr").filter({ has: page.getByRole("button", { name: "Amsterdam", exact: true }) }).getByRole("button", { name: /^Stability \d of 4/ }).hover()
    await page.locator("[data-radix-popper-content-wrapper]").getByText("Last failure", { exact: true }).waitFor()
    await shoot(page, "stability-hover", device)
    await page.mouse.move(0, 0)
    await page.getByRole("button", { name: "Germany · proxy", exact: true }).click()
    await page.getByRole("dialog").getByText("Country", { exact: true }).waitFor()
    await shoot(page, "server-details", device)
    await page.getByRole("dialog").getByRole("button", { name: "Close" }).first().click()

    await go("groups/acme", "Germany · proxy")
    await shoot(page, "group", device)
    await go("servers", "Frankfurt")
    await page.getByRole("button", { name: "Filter servers by Country", exact: true }).click()
    await page.getByRole("dialog").getByRole("option", { name: /Germany/ }).click()
    await page.getByRole("group", { name: "Country filter" }).getByText("Germany").waitFor()
    await shoot(page, "country-filter", device)
    await page.keyboard.press("Escape")
    await go("sources/acme", "Sweden")
    await page.locator("[data-slot=source-heading]").getByRole("button", { name: /in the last 7 days/ }).hover()
    await page.locator("[data-radix-popper-content-wrapper]").getByText("In use", { exact: true }).waitFor()
    await shoot(page, "source-week", device)
    await page.mouse.move(0, 0)
    await go("sources/aurora", "Frankfurt")
    await page.locator("[data-slot=source-heading]").getByRole("button", { name: /not in use$/ }).hover()
    await page.locator("[data-radix-popper-content-wrapper]").getByText("In use", { exact: true }).waitFor()
    await shoot(page, "source-week-empty", device)
    await page.mouse.move(0, 0)
    await go("sources/acme", "Sweden")
    if (device === "phone") {
      await page.getByRole("button", { name: "More for Acme" }).click()
      await page.getByRole("menuitem", { name: "Settings" }).click()
    } else await page.getByRole("button", { name: "Settings for Acme" }).click()
    await page.getByRole("dialog", { name: "Settings" }).getByLabel("URL", { exact: true }).waitFor()
    await shoot(page, "source-settings", device)
    await page.keyboard.press("Escape")

    await go("events", "Server failed")
    await shoot(page, "events", device)

    daemon.set({ vpnClients: { known: true, limit: 8, foreign: Array.from({ length: 5 }, (_, i) => ({ id: `n${i}`, name: `Foreign ${i + 1}`, enabled: i !== 4 })) } })
    await go("", "Foreign 5")
    await shoot(page, "board-overflow", device)

    daemon.reset()
    await go("", "EU")
    await shoot(page, "board-unknown", device)
    await context.close()
  }
  for (const width of widths("all", TABLE_WIDTHS)) {
    daemon.reset()
    seed(daemon)
    const viewport = { width, height: 844 }
    const context = await browser.newContext({ viewport, colorScheme: "dark" })
    const page = await context.newPage()
    page.setDefaultTimeout(10_000)
    page.on("console", (m) => { if (m.type() === "error" && !/ERR_INCOMPLETE_CHUNKED_ENCODING/.test(m.text())) errors.push(`${width}: ${m.text()}`) })
    page.on("pageerror", (e) => errors.push(`${width}: ${e.message}`))
    for (const [name, path, ready] of [["servers", "servers", "Frankfurt"], ["group", "groups/acme", "Germany · proxy"], ["source", "sources/acme", "Sweden"], ["new-group", "groups/new", "Frankfurt"]]) {
      await page.goto(daemon.url + path)
      await page.getByText(ready, { exact: false }).first().waitFor()
      await page.waitForTimeout(250)
      await shoot(page, `table-${name}`, `${width}`, viewport)
    }
    await page.goto(daemon.url + "groups/acme")
    await page.getByText("Germany · proxy").first().waitFor()
    const boxes = page.locator("tbody").getByRole("checkbox")
    await boxes.first().click()
    await boxes.last().click()
    await shoot(page, "table-group-boxes", `${width}`, viewport)
    await page.locator("thead").getByRole("button", { name: "Filter servers by Selection", exact: true }).click()
    await page.getByRole("dialog").getByRole("option").first().waitFor()
    await shoot(page, "table-group-box-filter", `${width}`, viewport)
    await page.keyboard.press("Escape")
    await page.goto(daemon.url + "servers")
    await page.getByText("Frankfurt").first().waitFor()
    await page.getByRole("button", { name: "Filter", exact: true }).click()
    await page.getByRole("dialog").getByRole("radio", { name: "Country", exact: true }).click()
    await page.getByRole("dialog").getByRole("option", { name: /Germany/ }).click()
    await page.keyboard.press("Escape")
    await page.getByRole("button", { name: "Save as group" }).waitFor()
    await shoot(page, "table-servers-filtered", `${width}`, viewport)
    await context.close()
  }
  for (const width of widths("all", STABILITY_WIDTHS)) {
    for (const theme of ["dark", "light"]) {
      daemon.reset()
      daemon.set({ catalogue: failureStates().map((x) => x.server) })
      const viewport = { width, height: 844 }
      const context = await browser.newContext({ viewport, colorScheme: theme })
      await context.addInitScript((t) => localStorage.setItem("theme", t), theme)
      const page = await context.newPage()
      page.setDefaultTimeout(10_000)
      page.on("console", (m) => { if (m.type() === "error" && !/ERR_INCOMPLETE_CHUNKED_ENCODING/.test(m.text())) errors.push(`${width} ${theme}: ${m.text()}`) })
      page.on("pageerror", (e) => errors.push(`${width} ${theme}: ${e.message}`))
      await page.goto(daemon.url + "servers")
      await page.getByText("Estonia · proxy").first().waitFor()
      await page.waitForTimeout(250)
      await shoot(page, "stability-table", `${width}-${theme}`, viewport)
      const lithuania = page.locator("tbody tr").filter({ hasText: "Lithuania · proxy" }).getByRole("button", { name: /^Stability / })
      await lithuania.focus()
      await page.locator("[data-radix-popper-content-wrapper]").getByText("12h", { exact: true }).waitFor()
      await shoot(page, "stability-table-card", `${width}-${theme}`, viewport)
      await page.locator("thead").getByRole("button", { name: "Stability", exact: true }).focus()
      await page.locator("[data-radix-popper-content-wrapper]").getByText("Starting level", { exact: true }).waitFor()
      await shoot(page, "stability-header-card", `${width}-${theme}`, viewport)
      await page.goto(daemon.url)
      await page.getByRole("combobox", { name: "Exit of EU" }).click()
      await page.getByRole("dialog").getByText("Estonia · proxy").waitFor()
      await page.waitForTimeout(400)
      await shoot(page, "stability-menu", `${width}-${theme}`, viewport)
      await page.getByPlaceholder("Search").fill("proxy")
      await page.getByRole("dialog").getByRole("option").filter({ hasText: "Lithuania · proxy" }).locator("[data-slot=bars]").hover()
      await page.locator("[data-radix-popper-content-wrapper]").filter({ hasNot: page.getByRole("listbox") }).getByText("12h", { exact: true }).waitFor()
      /* Resizing would move the menu from under the pointer and close the card, and the menu fits as it is. */
      await page.screenshot({ path: `${OUT}/stability-menu-card-${width}-${theme}.png` })
      console.log(`${OUT}/stability-menu-card-${width}-${theme}.png`)
      await context.close()
    }
  }
  for (const width of widths("all", HEADING_WIDTHS)) {
    daemon.reset()
    const t = troubledSources()
    daemon.set({ catalogue: t.servers, sources: new Map(t.sources.map((x) => [x.id, x])), refreshes: t.refreshes, analytics: t.analytics, slots: new Map() })
    const viewport = { width, height: 844 }
    const context = await browser.newContext({ viewport, colorScheme: "dark" })
    const page = await context.newPage()
    page.setDefaultTimeout(10_000)
    page.on("console", (m) => { if (m.type() === "error" && !/ERR_INCOMPLETE_CHUNKED_ENCODING/.test(m.text())) errors.push(`${width} headings: ${m.text()}`) })
    page.on("pageerror", (e) => errors.push(`${width} headings: ${e.message}`))
    for (const id of t.ids) {
      await page.goto(daemon.url + `sources/${id}`)
      const heading = page.locator("[data-slot=source-heading]")
      await heading.waitFor()
      await page.waitForTimeout(400)
      await heading.screenshot({ path: `${OUT}/heading-${id}-${width}.png` })
      console.log(`${OUT}/heading-${id}-${width}.png`)
    }
    if (width < 768) await page.getByRole("button", { name: "Menu" }).click()
    await page.getByRole("navigation", { name: "Main" }).screenshot({ path: `${OUT}/list-${width}.png` })
    console.log(`${OUT}/list-${width}.png`)
    await context.close()
  }
  for (const width of widths("failure", FAILURE_WIDTHS)) {
    for (const theme of ["dark", "light"]) {
      daemon.reset()
      const t = troubledSources()
      const failed = (hours, failureCode = "http_503") => ({ at: H(hours), outcome: "failed", failureCode, wanExcluded: false, added: 0, changed: 0, missing: 0 })
      const unchanged = (hours) => ({ ...failed(hours), outcome: "unchanged", failureCode: null })
      const fetchOf = { soon: { failures: 18 }, stale: { failures: 51, failureCode: "http_503", checkedAt: H(0.2), updatedAt: H(24 * 30) }, unlimited: { failures: 2, failureCode: "timeout", checkedAt: H(3) } }
      t.refreshes.set("soon", [...Array.from({ length: 18 }, (_, i) => failed(0.2 + i * 0.3)), unchanged(6)])
      t.refreshes.set("stale", Array.from({ length: 50 }, (_, i) => failed(0.2 + 6 * i)))
      t.refreshes.set("unlimited", [failed(3, "timeout"), failed(9, "timeout"), unchanged(26)])
      const sources = t.sources.map((x) => (x.id in fetchOf ? { ...x, origin: { ...x.origin, paused: x.id === "unlimited", fetch: { ...x.origin.fetch, ...fetchOf[x.id] } } } : x))
      daemon.set({ catalogue: t.servers, sources: new Map(sources.map((x) => [x.id, x])), refreshes: t.refreshes, analytics: t.analytics, slots: new Map() })
      const viewport = { width, height: width < 768 ? 844 : 900 }
      const context = await browser.newContext({ viewport, colorScheme: theme, hasTouch: width < 768 })
      await context.addInitScript((th) => localStorage.setItem("theme", th), theme)
      const page = await context.newPage()
      page.setDefaultTimeout(10_000)
      page.on("console", (m) => { if (m.type() === "error" && !/ERR_INCOMPLETE_CHUNKED_ENCODING/.test(m.text())) errors.push(`${width} ${theme} failure: ${m.text()}`) })
      page.on("pageerror", (e) => errors.push(`${width} ${theme} failure: ${e.message}`))
      for (const [name, id, worked] of [["worked", "soon", "6h ago"], ["never", "broken", "—"], ["older", "stale", "12d+ ago"], ["paused", "unlimited", "1d ago"], ["agents", "limit", "6h ago"]]) {
        await page.goto(daemon.url + `sources/${id}`)
        const why = page.getByRole("dialog")
        if (name === "never") {
          /* A source that lists no servers holds its failure in the table's place alone: its heading's Refresh refreshes, and its
           * hover is the working Refresh's. */
          await page.locator("main [data-slot=empty]").getByRole("button", { name: "Retry", exact: true }).waitFor()
          await page.waitForTimeout(250)
          await page.screenshot({ path: `${OUT}/failure-never-block-${width}-${theme}.png` })
          console.log(`${OUT}/failure-never-block-${width}-${theme}.png`)
          if (width < 768) continue
          await page.locator("[data-slot=source-heading]").getByRole("button", { name: /^Refresh/ }).hover()
          await page.locator("[data-radix-popper-content-wrapper]").getByText("every", { exact: false }).waitFor()
          await page.waitForTimeout(400)
          await page.screenshot({ path: `${OUT}/failure-never-hover-${width}-${theme}.png` })
          console.log(`${OUT}/failure-never-hover-${width}-${theme}.png`)
          await page.mouse.move(0, 0)
          continue
        }
        await page.locator("[data-slot=source-heading]").getByRole("button", { name: /^Refresh/ }).click()
        await why.locator("[data-slot=rows]").getByText(worked, { exact: true }).waitFor()
        await page.waitForTimeout(250)
        await page.screenshot({ path: `${OUT}/failure-${name}-${width}-${theme}.png` })
        console.log(`${OUT}/failure-${name}-${width}-${theme}.png`)
      }
      await context.close()
    }
  }
  for (const width of widths("add", ADD_WIDTHS)) {
    for (const theme of ["dark", "light"]) {
      daemon.reset()
      seed(daemon)
      daemon.set({ slots: new Map(SLOTS.slice(0, 2).map((x) => [x.id, x])) })
      const viewport = { width, height: width < 768 ? 844 : 900 }
      const context = await browser.newContext({ viewport, colorScheme: theme, hasTouch: width < 768 })
      await context.addInitScript((t) => localStorage.setItem("theme", t), theme)
      const page = await context.newPage()
      page.setDefaultTimeout(10_000)
      page.on("console", (m) => { if (m.type() === "error" && !/502|ERR_INCOMPLETE_CHUNKED_ENCODING/.test(m.text())) errors.push(`${width} ${theme} add: ${m.text()}`) })
      page.on("pageerror", (e) => errors.push(`${width} ${theme} add: ${e.message}`))
      const at = `${width}-${theme}`
      const card = page.locator("[data-radix-popper-content-wrapper]")
      const add = page.getByRole("list", { name: "VPN Clients" }).getByRole("button", { name: /^Add/ })
      /* A card or a dialog fades in, so a shot of one waits it out. */
      const save = async (name) => { await page.waitForTimeout(400); await page.screenshot({ path: `${OUT}/${name}-${at}.png` }); console.log(`${OUT}/${name}-${at}.png`) }
      const board = async () => { await page.goto(daemon.url); await page.getByText("Office WireGuard").waitFor(); await add.scrollIntoViewIfNeeded(); await page.waitForTimeout(250) }

      await board()
      const box = await add.boundingBox()
      await add.click()
      const draft = page.getByRole("group", { name: "New slot" })
      await draft.waitFor()
      await save("add-card")
      assert.deepEqual(await draft.boundingBox(), box, `${at}: the new slot's card has the box's own place`)
      await page.keyboard.press("Escape")
      await draft.waitFor({ state: "detached" })
      await page.getByRole("button", { name: "Delete EU", exact: true }).click()
      await page.getByRole("alertdialog").waitFor()
      await save("slot-delete")
      await page.keyboard.press("Escape")
      await page.getByRole("alertdialog").waitFor({ state: "detached" })

      for (const trouble of ["noKey", "keyRefused", "notAnswering"]) {
        daemon.set({ unifiTrouble: trouble })
        await board()
        await add.click({ force: true })
        await card.waitFor()
        await save(`unifi-${trouble}-add`)
        await page.keyboard.press("Escape")
        await card.waitFor({ state: "detached" })
        await page.locator("header").getByRole("button", { name: /^UniFi: / }).click()
        await card.waitFor()
        await save(`unifi-${trouble}-mark`)
      }
      daemon.set({ unifiTrouble: "notAnswering", unifiAcceptedAt: null })
      await board()
      await page.locator("header").getByRole("button", { name: /^UniFi: / }).click()
      await card.waitFor()
      await save("unifi-notAnswering-unanswered-mark")
      daemon.set({ unifiTrouble: "noKey", sources: new Map(), slots: new Map(), groups: new Map() })
      await page.goto(daemon.url)
      await page.getByText("Get started", { exact: true }).waitFor()
      await save("get-started")
      daemon.set({ unifiTrouble: null })

      const t = troubledSources()
      daemon.set({ catalogue: t.servers, sources: new Map(t.sources.map((x) => [x.id, x.id === "pasted" ? { ...x, diagnostics: [{ entry: 2, code: "malformed_entry", field: "" }] } : x])), refreshes: t.refreshes, analytics: t.analytics, slots: new Map(), readFails: { "api/servers": 10 } })
      await page.goto(daemon.url + "sources/pasted")
      await page.getByRole("button", { name: "1 entry skipped. Entry 2: Malformed" }).click()
      await card.waitFor()
      await save("skipped-card")
      await page.keyboard.press("Escape")
      await card.waitFor({ state: "detached" })
      if (width < 768) await page.getByRole("button", { name: "Menu" }).click()
      await page.getByRole("navigation", { name: "Main" }).getByRole("button", { name: "All servers: HTTP 502", exact: true }).click()
      await card.getByRole("button", { name: "Retry" }).waitFor()
      await save("failed-read-card")
      await context.close()
    }
  }
  for (const width of widths("about", ABOUT_WIDTHS)) {
    for (const theme of ["dark", "light"]) {
      daemon.reset()
      seed(daemon)
      const viewport = { width, height: width < 768 ? 844 : 900 }
      const context = await browser.newContext({ viewport, colorScheme: theme, hasTouch: width < 768 })
      await context.addInitScript((t) => localStorage.setItem("theme", t), theme)
      const page = await context.newPage()
      page.setDefaultTimeout(10_000)
      page.on("console", (m) => { if (m.type() === "error" && !/502|ERR_INCOMPLETE_CHUNKED_ENCODING|ERR_FAILED/.test(m.text())) errors.push(`${width} ${theme} about: ${m.text()}`) })
      page.on("pageerror", (e) => errors.push(`${width} ${theme} about: ${e.message}`))
      const at = `${width}-${theme}`
      const about = page.locator("main [data-slot=card]").filter({ has: page.getByText("About", { exact: true }) })
      const settings = async (shown) => { await page.goto(daemon.url + "settings"); await about.getByText(shown, { exact: true }).waitFor(); await page.waitForTimeout(250) }

      await settings("v0.1.0")
      await shoot(page, "about-release", at, viewport)
      const box = about.getByRole("checkbox", { name: "Check for updates" })
      let landed
      const held = new Promise((r) => { landed = r })
      await page.route("**/api/settings/check-for-updates", async (route) => { await held; await route.continue().catch(() => {}) })
      await box.click()
      await page.waitForFunction(() => document.querySelector("[data-control=check-for-updates]")?.getAttribute("aria-busy") === "true")
      await shoot(page, "about-check-pending", at, viewport)
      landed()
      await page.waitForFunction(() => document.querySelector("[data-control=check-for-updates]")?.getAttribute("aria-disabled") === null)
      await page.unroute("**/api/settings/check-for-updates")
      await page.waitForTimeout(250)
      await shoot(page, "about-check-off", at, viewport)
      daemon.set({ version: "dev-7990743", release: null })
      await settings("dev-7990743")
      await shoot(page, "about-dev", at, viewport)
      /* The check the page makes as it loads would bring the view the read doesn't. */
      await page.route("**/api/update/check", (route) => route.abort())
      const releases = [daemon.holdReads("api/update"), daemon.holdReads("api/settings")]
      await page.goto(daemon.url + "settings")
      await about.locator("[data-slot=skeleton]").first().waitFor()
      await shoot(page, "about-waiting", at, viewport)
      for (const release of releases) release()
      daemon.set({ readFails: { "api/update": 1, "api/settings": 1 } })
      await page.goto(daemon.url + "settings")
      const failed = about.getByRole("button", { name: /^Version: / })
      await failed.waitFor()
      await shoot(page, "about-failed", at, viewport)
      if (width < 768) await failed.tap()
      else await failed.click()
      await page.locator("[data-radix-popper-content-wrapper]").getByRole("button", { name: "Retry" }).waitFor()
      await page.waitForTimeout(400)
      await page.screenshot({ path: `${OUT}/about-failed-card-${at}.png` })
      console.log(`${OUT}/about-failed-card-${at}.png`)
      await context.close()
    }
  }
  for (const width of widths("update", UPDATE_WIDTHS)) {
    for (const theme of ["dark", "light"]) {
      const viewport = { width, height: width < 768 ? 844 : 800 }
      const at = `${width}-${theme}`
      const phone = width < 768
      const found = { version: "v0.2.0", restartsCore: false }
      const last = (outcome, reason = null, version = "v0.2.0") => ({ attempt: "0123456789abcdef", version, at: H(0.05), outcome, reason, message: "x" })
      /** The gateway with a source that expires in 3 days, so the phone's Menu carries an amber dot, and the updater as `update` says. */
      const seeded = (update = {}, patch = {}) => {
        daemon.reset()
        const aurora = daemon.source("aurora")
        const soon = { ...aurora, origin: { ...aurora.origin, provider: { ...aurora.origin.provider, quota: { ...aurora.origin.provider.quota, expires: H(-71) } } } }
        daemon.set({ sources: new Map([["aurora", soon]]), latest: "v0.2.0", update: { startedAt: H(0.1), check: { at: H(0.1), failure: null }, found: null, running: null, pressedAt: null, last: null, ...update }, ...patch })
      }
      const context = await browser.newContext({ viewport, colorScheme: theme, hasTouch: phone })
      await context.addInitScript((t) => localStorage.setItem("theme", t), theme)
      const page = await context.newPage()
      page.setDefaultTimeout(10_000)
      page.on("console", (m) => { if (m.type() === "error" && !/502|409|ERR_INCOMPLETE_CHUNKED_ENCODING|ERR_FAILED|ERR_CONNECTION/.test(m.text())) errors.push(`${width} ${theme} update: ${m.text()}`) })
      page.on("pageerror", (e) => errors.push(`${width} ${theme} update: ${e.message}`))
      const about = page.locator("main [data-slot=card]").filter({ has: page.getByText("About", { exact: true }) })
      const line = about.locator("[data-slot=update-line]")
      const press = (target) => (phone ? target.tap() : target.click())
      const settle = async () => {
        await page.mouse.move(0, 0)
        await page.waitForTimeout(450)
      }
      const picture = async (name) => {
        await settle()
        await page.screenshot({ path: `${OUT}/${name}-${at}.png` })
        const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)
        assert.ok(overflow <= 0, `${name} at ${at} scrolls sideways by ${overflow}px`)
        console.log(`${OUT}/${name}-${at}.png`)
      }
      const settings = async (version = "v0.1.0") => {
        await page.goto(daemon.url + "settings")
        await line.filter({ hasText: version }).waitFor()
        await line.scrollIntoViewIfNeeded()
      }
      const updateButton = line.getByRole("button", { name: "Update", exact: true })
      const card = page.locator("[data-radix-popper-content-wrapper]")

      seeded()
      await settings()
      await picture("01-rest")
      let checked
      const held = new Promise((r) => { checked = r })
      await page.route("**/api/update/check", async (route) => { await held; await route.continue().catch(() => {}) })
      await press(line.getByRole("button", { name: "Check for updates", exact: true }))
      await picture("01-checking")
      checked()
      await updateButton.waitFor()
      await page.unroute("**/api/update/check")
      await picture("02-newer")
      if (phone) {
        await page.getByRole("button", { name: "Menu, update to v0.2.0" }).tap()
        await page.getByRole("dialog").getByRole("img", { name: "Update to v0.2.0" }).waitFor()
        await picture("02-newer-menu")
        await page.keyboard.press("Escape")
        await page.getByRole("dialog").waitFor({ state: "detached" })
      }
      await page.goto(daemon.url)
      await page.getByRole("group", { name: "EU" }).waitFor()
      await picture("02-newer-dashboard")
      await settings()
      const dialog = page.getByRole("alertdialog")
      await press(updateButton)
      await dialog.waitFor()
      await picture("03-confirm")
      await page.keyboard.press("Escape")
      await dialog.waitFor({ state: "detached" })
      await page.keyboard.press("Shift")
      await picture("05-declined")
      seeded({ found: { ...found, restartsCore: true } })
      await settings()
      await press(updateButton)
      await dialog.getByText("All slots will disconnect briefly.").waitFor()
      await picture("04-confirm-warn")

      seeded({ found })
      await settings()
      await press(updateButton)
      await press(dialog.getByRole("button", { name: "Update", exact: true }))
      await dialog.waitFor({ state: "detached" })
      await page.locator("[data-slot=update-line] button[aria-busy=true]").waitFor()
      await picture("06-running")
      daemon.stop()
      await page.getByRole("button", { name: /^Gateway unreachable/ }).waitFor()
      await picture("07-offline")
      daemon.endUpdate({ outcome: "landed" })
      daemon.set({ version: "v0.2.0", release: "ba9876543210" })
      daemon.start()
      await line.getByText("v0.2.0", { exact: true }).waitFor({ timeout: 20_000 })
      await page.getByRole("button", { name: /^Gateway unreachable/ }).waitFor({ state: "detached" })
      await line.getByRole("button", { name: "Check for updates", exact: true }).waitFor()
      await line.scrollIntoViewIfNeeded()
      await picture("08-landed")

      const ended = [["09-rolled-back", last("rolledBack")], ["10-refused", last("refused")], ["10-recovery", last("putBackFailed")], ["10-interrupted", last("failed", "interrupted")],
        ...["noStart", "stopped", "moved", "planChanged", "notNewer", "noRoom", "downloadFailed", "checksum", "wrongRelease"].map((reason) => [`10-failed-${reason}`, last("failed", reason)])]
      for (const [name, how] of ended) {
        seeded({ found, last: how })
        await settings()
        const cross = line.getByRole("button", { name: /^Update: / })
        if (name === "09-rolled-back") await picture(name)
        await press(cross)
        await card.first().waitFor()
        await page.waitForTimeout(450)
        await page.screenshot({ path: `${OUT}/${name}-card-${at}.png` })
        console.log(`${OUT}/${name}-card-${at}.png`)
      }
      for (const [name, update, patch] of [
        ["10-offer-changed", { found, last: last("failed", "offerChanged"), startedAt: H(1) }, { latest: "v0.2.0", settings: { ...daemon.settings(), checkForUpdates: false } }],
        ["10-untidy", { last: last("landedWithError") }, { version: "v0.2.0", release: "ba9876543210" }],
        ["10-recovery-new", { last: last("putBackFailed") }, { version: "v0.2.0", release: "ba9876543210" }],
      ]) {
        seeded(update, patch)
        await settings(patch.version)
        await picture(name)
        await press(line.getByRole("button", { name: /^Update: / }))
        await card.first().waitFor()
        await page.waitForTimeout(450)
        await page.screenshot({ path: `${OUT}/${name}-card-${at}.png` })
        console.log(`${OUT}/${name}-card-${at}.png`)
      }
      for (const failure of ["unreachable", "clockWrong", "filesUnavailable", "malformed"]) {
        seeded({ check: { at: H(3.2), failure } })
        await settings()
        const mark = line.getByRole("button", { name: /^Check for updates: / })
        if (failure === "unreachable") await picture("12-check-failed")
        await press(mark)
        await card.first().waitFor()
        await page.waitForTimeout(450)
        await page.screenshot({ path: `${OUT}/12-check-${failure}-card-${at}.png` })
        console.log(`${OUT}/12-check-${failure}-card-${at}.png`)
      }
      seeded({ running: { attempt: "0123456789abcdef", version: "v0.2.0" }, pressedAt: H(0.01) })
      await settings()
      await picture("06-running-over-ssh")
      seeded({ running: { attempt: "0123456789abcdef", version: "v0.2.0" }, pressedAt: H(0.01) }, { version: "v0.2.0", release: "ba9876543210" })
      await settings("v0.2.0")
      await picture("06-running-as-new")

      seeded({ found })
      await settings()
      await press(page.getByRole("radiogroup", { name: "Retention" }).getByRole("radio", { name: "90d", exact: true }))
      daemon.stop()
      await page.getByRole("button", { name: /^Gateway unreachable/ }).waitFor()
      daemon.set({ version: "v0.2.0", release: "ba9876543210" })
      daemon.start()
      await page.locator("[data-sonner-toast]").filter({ hasText: "v0.2.0" }).waitFor({ timeout: 20_000 })
      await page.getByRole("button", { name: /^Gateway unreachable/ }).waitFor({ state: "detached" })
      await line.scrollIntoViewIfNeeded()
      await picture("11-dirty")

      seeded({}, { version: "dev-3f9a2c1", release: null })
      await settings("dev-3f9a2c1")
      await picture("13-dev")

      await page.route("**/api/update/check", (route) => route.abort())
      seeded({ found })
      daemon.set({ readFails: { "api/update": 1 } })
      await page.goto(daemon.url + "settings")
      await about.getByRole("button", { name: "Version: HTTP 502" }).waitFor()
      await about.scrollIntoViewIfNeeded()
      await picture("14-unread")
      await page.unroute("**/api/update/check")

      seeded({ found })
      await settings()
      await page.route("**/api/update", (route) => (route.request().method() === "POST" ? route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ kind: "capacity", reason: "lifecycleRunning", message: "x" }) }) : route.continue()))
      await press(updateButton)
      await press(dialog.getByRole("button", { name: "Update", exact: true }))
      await page.locator("[data-sonner-toast]").waitFor()
      await picture("15-refused-busy")
      await context.close()
    }
  }
  for (const [name, width, theme] of widths("readme", README_SHOTS)) {
    daemon.reset()
    daemon.set({
      analytics: acmeWeek,
      catalogue: [...acme, ...aurora],
      sources: new Map([["acme", ACME], ["aurora", README_AURORA]]),
      slots: new Map(README_SLOTS.map((x) => [x.id, x])),
      groups: new Map([["europe", README_GROUP]]),
      vpnClients: { known: true, limit: 8, foreign: [] },
      switches: README_SWITCHES,
    })
    const viewport = { width, height: width < 768 ? 844 : 500 }
    const context = await browser.newContext({ viewport, colorScheme: theme, hasTouch: width < 768 })
    await context.addInitScript((t) => localStorage.setItem("theme", t), theme)
    const page = await context.newPage()
    page.setDefaultTimeout(10_000)
    page.on("console", (m) => { if (m.type() === "error" && !/ERR_INCOMPLETE_CHUNKED_ENCODING/.test(m.text())) errors.push(`readme ${name}: ${m.text()}`) })
    page.on("pageerror", (e) => errors.push(`readme ${name}: ${e.message}`))
    /* The page scrolls inside its pane, so the picture is as tall as the pane's content, set before any card opens. */
    const whole = async () => {
      const height = await page.evaluate(() => document.querySelector("[data-slot=main] > div:last-child")?.scrollHeight ?? 0)
      await page.setViewportSize({ width, height: Math.max(viewport.height, height + 60) })
      await page.waitForTimeout(150)
    }
    if (name === "source") {
      await page.goto(daemon.url + "sources/acme")
      await page.getByText("Sweden").first().waitFor()
      await whole()
      await page.locator("[data-slot=source-heading]").getByRole("button", { name: /in the last 7 days/ }).hover()
      await page.locator("[data-radix-popper-content-wrapper]").getByText("In use", { exact: true }).waitFor()
    } else if (name === "events") {
      await page.goto(daemon.url + "events")
      await page.getByText("Server failed").first().click()
      await page.getByRole("table").getByText("Passed").first().waitFor()
      await whole()
      await page.mouse.move(0, 0)
    } else {
      await page.goto(daemon.url)
      await page.getByRole("group", { name: "Backup" }).waitFor()
      await page.getByRole("group", { name: "Home" }).getByText("Germany · proxy").waitFor()
    }
    await page.waitForTimeout(500)
    assert.equal(await page.locator("[data-slot=skeleton], [aria-busy=true]").count(), 0, `readme ${name}: nothing is loading`)
    await page.screenshot({ path: `${OUT}/${name}.png` })
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)
    assert.ok(overflow <= 0, `readme ${name} scrolls sideways by ${overflow}px`)
    console.log(`${OUT}/${name}.png`)
    await context.close()
  }
  assert.deepEqual(errors, [], "the console stays clean")
} finally {
  await browser.close()
  await daemon.close()
  rmSync(DIST, { recursive: true, force: true })
}
