// Sequences of presses on the real page, on every form that saves over a revision or creates. Each step is one press: the
// reader edits first (e) or presses unchanged (s), and every attempt of that press meets one fate: it lands and its answer
// is lost (L), it never arrives (N), it lands and a proxy answers 502 (R), UniFi refuses it in front of the daemon (P), the
// daemon refuses it (V), or it is answered (Y); on Add source an attempt can also be held at the door and land once the next
// press has ended (D), an add that landed where the page can't see it. A press is sent again under its key at 1, 2, 4, 8 and
// 15 s, so L, N, R and D end with "timed out" at 30 s, and the next step's edit comes after it; the harness runs the page's clock on until each press has
// ended. A final press over a good network ends each run. Every sequence of two steps runs alone, a sample of three, and a
// sample with someone else's save or forgotten keys at one of its presses. A source's settings also take, after a step, the
// reader leaving the tab and coming back (/l), pressing the no-answer toast's Retry on All servers (/a), or a rename from the
// heading that lands (/h) or is refused in front (/H); an unchanged step whose Save has nothing to save presses the dialog's
// own Retry.
// Exits 1 on any violation: a page error; one request committed twice; a save to another item; an add that made a second
// source though nothing in it changed after no answer; a conflict with no other writer; a last save
// neither stored, nor said refused or in conflict, nor overwritten by a writer that came after it was stored; a Retry, or a
// source's unchanged Save after no answer, sent under another key, body or revision than the write that had no answer. A
// conflict with no other writer is the reader's own: that replay after their own rename landed. Exits 1 too when
// some ending of a write, or an edit after no answer, was never reached on a form that can reach it.
// Run with `npm run test:fuzz`; E2E_PORT moves its ports, FUZZ_SAMPLE its sample, and FUZZ_ONLY keeps the runs whose
// "form:mode:steps" starts with it ("sourceAdd:none:sL") and prints each. `--smoke` runs one sequence per form, with a third writer, and a
// stale selector fails it in seconds.
import { rmSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { chromium } from "playwright-core"
import { build } from "vite"
import { startFakeDaemon } from "./fake-daemon.mjs"

const ROOT = fileURLToPath(new URL("../../", import.meta.url))
const DIST = fileURLToPath(new URL("./.dist-fuzz/", import.meta.url))
const BASE = "/proxy/"
const FIRST_PORT = Number(process.env.E2E_PORT ?? 5311) + 5
const WORKERS = 4
const SAMPLE = Number(process.env.FUZZ_SAMPLE ?? 0.2)

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function until(check, what) {
  const end = Date.now() + 6000
  for (;;) {
    const value = await check()
    if (value) return value
    if (Date.now() > end) throw new Error(`timed out: ${what}`)
    await sleep(50)
  }
}

const SOURCE_WRITE = /\/api\/sources\/[^/]+$/
const idOf = (path) => path?.split("/").pop()

async function addDialog(page, item) {
  await page.getByRole("navigation", { name: "Main" }).getByRole("button", { name: "Add", exact: true }).click()
  await page.getByRole("menuitem", { name: item, exact: true }).click()
}

const REFRESH = ["12h", "3h", "1h", "24h"]
/** Retention's picks, none of them what the third writer stores. */
const RETENTION = ["90d", "1y"]
const RETENTION_WORDS = { 7: "7d", 30: "30d", 90: "90d", 365: "1y" }
const LABELS = { 3600: "1h", 10800: "3h", 21600: "6h", 43200: "12h", 86400: "24h" }

/** `stored` reads the field from the daemon at the item the page last wrote. `creates`: each add is an item of its own.
 * `endings`: what a press on it can end in. */
const EDITORS = {
  create: {
    url: "servers", glob: SOURCE_WRITE, closes: true, creates: true,
    ready: async (page) => {
      await addDialog(page, "Source")
      await page.getByRole("dialog").getByLabel("Source", { exact: true }).fill("trojan://pw@b.example:443#Borealis one")
    },
    type: (page, value) => page.getByRole("dialog").getByLabel("Name").fill(value),
    save: (page) => page.getByRole("dialog").getByRole("button", { name: /^(Add|Save)$/ }),
    value: (i) => `V${i}`,
    stored: (d, path) => d.source(idOf(path))?.name ?? null,
    third: (d, path) => { if (d.source(idOf(path))) d.renameSource(idOf(path), "Third") },
    fates: "LNRPVYD",
    endings: ["yes", "noAnswer", "unifi", "refused"],
  },
  settings: {
    url: "settings", glob: "**/api/settings",
    ready: (page) => until(async () => (await page.getByRole("radiogroup", { name: "Retention" }).getByRole("radio", { checked: true }).innerText()).trim() === "30d", "the editor"),
    type: (page, word) => page.getByRole("radiogroup", { name: "Retention" }).getByRole("radio", { name: word, exact: true }).click(),
    save: (page) => page.locator("button[data-control=settings]"),
    value: (i) => RETENTION[i % RETENTION.length],
    stored: (d) => RETENTION_WORDS[d.settings().analyticsRetentionDays] ?? String(d.settings().analyticsRetentionDays),
    third: (d) => d.changeSettings({ analyticsRetentionDays: 7 }),
    fates: "LNRPVY",
    endings: ["yes", "noAnswer", "unifi", "refused", "conflict"],
  },
  sourceEdit: {
    /* Opened from All servers, so the browser's Back leaves the source's tab as a reader does. */
    url: "servers", glob: "**/api/sources/aurora", closes: true,
    ready: async (page) => {
      if (!(await page.locator("[data-slot=source-heading]").count())) await page.getByRole("navigation", { name: "Main" }).locator('a[href$="/sources/aurora"]').click()
      await page.getByRole("button", { name: /^Settings for / }).click()
      await page.getByRole("dialog", { name: "Settings" }).getByRole("radiogroup", { name: "Refresh" }).waitFor()
    },
    retry: (page) => page.getByRole("dialog", { name: "Settings" }).getByRole("button", { name: "Retry", exact: true }),
    rename: async (page, name) => {
      await page.locator("[data-slot=source-heading]").locator("button[aria-label^='Rename ']").click()
      await page.getByLabel(/^Name of /).fill(name)
      await page.keyboard.press("Enter")
    },
    type: (page, label) => page.getByRole("dialog", { name: "Settings" }).getByRole("radiogroup", { name: "Refresh" }).getByRole("radio", { name: label, exact: true }).click(),
    save: (page) => page.getByRole("dialog", { name: "Settings" }).getByRole("button", { name: "Save", exact: true }),
    value: (i) => REFRESH[i % REFRESH.length],
    stored: (d) => LABELS[d.source("aurora").origin.refreshEverySeconds],
    third: (d) => d.renameSource("aurora", "Third"),
    fates: "LNRPVY",
    endings: ["yes", "noAnswer", "unifi", "refused", "conflict"],
  },
  sourceAdd: {
    url: "servers", glob: SOURCE_WRITE, closes: true, creates: true,
    ready: (page) => addDialog(page, "Source"),
    type: (page, value) => page.getByRole("dialog").getByLabel("Source", { exact: true }).fill(value),
    save: (page) => page.getByRole("dialog").getByRole("button", { name: /^(Add|Save)$/ }),
    value: (i) => `https://v${i}.example/sub`,
    stored: (d, path) => d.secrets(idOf(path))?.url ?? null,
    third: (d, path) => { if (d.source(idOf(path))) d.renameSource(idOf(path), "Third") },
    fates: "LNRPVYD",
    endings: ["yes", "noAnswer", "unifi", "refused"],
  },
}

const PROXY = { contentType: "text/html", body: "<h1>proxy</h1>" }
const REFUSAL = { status: 422, contentType: "application/json", headers: { "Sidegate-Revision": "1" }, body: JSON.stringify({ kind: "validation", message: "Refused by the fuzz." }) }

async function run(daemon, browser, name, sequence, mode) {
  daemon.reset()
  const editor = EDITORS[name]
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  await context.clock.install()
  const page = await context.newPage()
  page.setDefaultTimeout(6000)
  await page.goto(daemon.url + editor.url)
  const errors = []
  const trace = []
  const endings = []
  page.on("pageerror", (e) => errors.push(e.message))
  /** The fate every attempt of the press now out meets, the press's number from 1, and whether its first attempt has gone. */
  let fate = "Y"
  let presses = 0
  let first = false
  /** Every PUT at the door: the press it is for, the Save's own or another way's, with its key, revision and body. */
  const puts = []
  let pressing = "save"
  let lastPath = null
  /** D's attempts, held at the door until the press after theirs has ended. */
  let late = []
  const land = async (routes) => {
    for (const route of routes) {
      await route.fetch().catch(() => null)
      await route.abort().catch(() => {})
    }
  }
  /** What the page's field held at the daemon when the third writer wrote, if it did. */
  let before
  const at = Number(mode.replace(/\D/g, ""))
  const meanwhile = () => {
    if (mode.startsWith("forget") && presses === at) daemon.set({ writes: new Map() })
    if (mode.startsWith("third") && presses === at) {
      before = editor.stored(daemon, lastPath)
      editor.third(daemon, lastPath)
    }
  }
  /* After its await an attempt's route may already have ended, the page having given up on it, so what it does then can't land. */
  const meet = async (route) => {
    lastPath = new URL(route.request().url()).pathname
    const headers = route.request().headers()
    puts.push({ press: presses, how: pressing, key: headers["idempotency-key"], ifMatch: headers["if-match"] ?? null, body: route.request().postData() })
    const once = first ? () => {} : meanwhile
    first = true
    if (fate === "L") { await route.fetch(); once(); return route.abort("connectionreset").catch(() => {}) }
    if (fate === "N") { once(); return route.abort("connectionreset") }
    if (fate === "R") { await route.fetch(); once(); return route.fulfill({ status: 502, ...PROXY }).catch(() => {}) }
    if (fate === "P") { once(); return route.fulfill({ status: 403, ...PROXY }) }
    if (fate === "V") { once(); return route.fulfill(REFUSAL) }
    if (fate === "D") { once(); late.push(route); return }
    once()
    return route.continue()
  }
  /** The PUT handlers still acting. An attempt the page gave up on, at its timeout or at no answer, may still be at the door. */
  const acting = new Set()
  await page.route(editor.glob, (route) => {
    if (route.request().method() !== "PUT") return route.continue()
    const act = meet(route)
    acting.add(act)
    act.then(() => acting.delete(act), () => acting.delete(act))
    return act
  })
  /** The toasts on screen, and not those a press dismissed that are still leaving. */
  const toasts = (words) => page.locator("[data-sonner-toast]:not([data-removed=true])").filter({ hasText: words })
  try {
    await editor.ready(page)
    const save = editor.save(page)
    const gone = async () => editor.closes && !(await page.getByRole("dialog").count())
    const noAnswer = async () => (await toasts("timed out").count()) > 0
    /** How the press ended, by what the page says: nothing on a yes, a toast or the reason under the field otherwise. */
    const ending = async () => {
      if (await gone()) return "yes"
      if (await noAnswer()) return "noAnswer"
      if (await toasts("changed").count()) return "conflict"
      if (await toasts(/UniFi refused|session expired/).count()) return "unifi"
      if ((await toasts("Refused by the fuzz").count()) || (await page.locator("[data-slot=field-error]").filter({ hasText: "Refused by the fuzz" }).count())) return "refused"
      return "yes"
    }
    /** Runs the page's clock on until the press has ended: its button no longer Pending, or its dialog closed. */
    const ended = async () => {
      for (let step = 0; step < 60; step++) {
        if (await gone()) return
        if ((await save.getAttribute("aria-busy").catch(() => null)) === null) { await sleep(60); return }
        await page.clock.runFor(1000)
        await sleep(25)
      }
      throw new Error("a press that never ended")
    }
    let last = null
    let values = 0
    let afterNoAnswer = 0
    /** The Save's write that ended with no answer, which a Retry and an unchanged Save send again; whether a rename of the reader's
     * own landed since; and whether a conflict met is that write's replay after it. */
    let kept = null
    let renamedSince = false
    let own = false
    const same = (a, b) => a.key === b.key && a.ifMatch === b.ifMatch && a.body === b.body
    const problems = []
    const edit = async () => {
      if (await noAnswer()) {
        afterNoAnswer++
        trace.push("edit after no answer")
      } else trace.push("edit")
      last = editor.value(++values)
      await editor.type(page, last)
    }
    const press = async (edited, meets) => {
      if (edited || last === null) await edit()
      /* With nothing left to save, an unchanged step presses the dialog's own Retry when it offers one. */
      const retry = editor.retry?.(page)
      const button = !edited && retry && (await save.isDisabled().catch(() => true)) && (await retry.count()) > 0 ? retry : save
      if (await button.isDisabled().catch(() => true)) { trace.push("disabled"); return }
      fate = meets
      presses++
      first = false
      pressing = "save"
      const held = late
      late = []
      await button.click()
      trace.push(`click${button === retry ? " Retry" : ""} ${meets}`)
      await ended()
      const how = await ending()
      endings.push(how)
      await land(held)
      await Promise.all(acting)
      const mine = puts.filter((w) => w.press === presses && w.how === "save")
      /* Only a source's settings send an unchanged Save as its Retry; every other form's Save goes over the view, as before. */
      if (kept && !edited && retry && !mine.every((w) => same(w, kept))) problems.push("a Retry or an unchanged Save after no answer went out under another key, body or revision")
      if (how === "conflict" && kept && renamedSince && mine.length > 0 && mine.every((w) => same(w, kept))) own = true
      if (how === "noAnswer") kept = mine[0] ?? kept
      else {
        kept = null
        renamedSince = false
      }
    }
    /** The reader leaves the source's tab, by the browser's Back, and comes back to its settings, or does what `away` says first. */
    const leave = async (away = async () => {}) => {
      await page.goBack()
      await page.waitForURL(/\/servers$/)
      await away()
      await editor.ready(page)
    }
    /** Runs the page's clock on until nothing pressed is at the door or waiting there. */
    const settle = async () => {
      for (let step = 0; step < 40; step++) {
        await page.clock.runFor(1000)
        await sleep(25)
        if (acting.size === 0 && (await page.locator("[aria-busy=true]").count()) === 0) return
      }
    }
    const interlude = async (code) => {
      trace.push(`/${code}`)
      first = true
      if (code === "l") return leave()
      if (code === "a") {
        return leave(async () => {
          const said = toasts("timed out").getByRole("button", { name: "Retry" })
          if (!(await said.count())) return
          fate = "Y"
          pressing = "retry"
          const from = puts.length
          await said.click()
          await settle()
          if (kept && !puts.slice(from).every((w) => same(w, kept))) problems.push("a Retry or an unchanged Save after no answer went out under another key, body or revision")
          kept = null
          renamedSince = false
        })
      }
      await leave(async () => {
        await page.getByRole("navigation", { name: "Main" }).locator('a[href$="/sources/aurora"]').click()
        fate = code === "h" ? "Y" : "P"
        pressing = "rename"
        await editor.rename(page, `Renamed ${presses}`)
        await settle()
        if (code === "h" && kept && daemon.source("aurora")?.name === `Renamed ${presses}`) renamedSince = true
      })
    }
    for (const [i, step] of sequence.entries()) {
      if (await gone()) break
      const [code, after] = step.split("/")
      await press(code[0] === "e" || i === 0, code[1])
      if (after && editor.rename && !(await gone())) await interlude(after)
    }
    if (!(await gone())) await press(sequence.at(-1)[0] === "e", "Y")
    await land(late)
    await sleep(200)
    await Promise.all(acting)
    await page.unroute(editor.glob)
    const conflicts = await toasts("changed").count()
    const said = conflicts > 0 || endings.at(-1) === "unifi" || endings.at(-1) === "refused"
    const stored = editor.stored(daemon, lastPath)
    const writes = daemon.log().filter((r) => r.method === "PUT")
    const committed = writes.filter((r) => r.status === 200 && !r.replayed)
    const perKey = new Map()
    for (const write of committed) perKey.set(write.idempotencyKey, (perKey.get(write.idempotencyKey) ?? 0) + 1)
    const items = new Set(committed.map((r) => r.path))
    if (errors.length) problems.push("page error")
    if ([...perKey.values()].some((n) => n > 1)) problems.push("one request committed twice")
    if (!editor.creates && items.size > 1) problems.push("a save went to another item")
    if (editor.creates && items.size > 1 + afterNoAnswer) problems.push("an add made a second source with nothing in it changed after no answer")
    if (conflicts && mode === "none" && !own) problems.push("a conflict with no other writer")
    /* Another writer's value is the last word only when what it overwrote was the reader's last save, already stored. */
    if (stored !== last && !said && before !== last) problems.push("the last save is neither stored, nor said refused or in conflict, nor overwritten by a later writer")
    return {
      name, sequence: sequence.join(" "), mode, last, stored, before, problems, trace, endings, afterNoAnswer,
      puts: writes.map((r) => `${r.status}${r.replayed ? "r" : ""}${r.ifNoneMatch ? "*" : r.ifMatch}`),
    }
  } catch (e) {
    return { name, sequence: sequence.join(" "), mode, problems: [`harness: ${e.message.split("\n")[0]}`], trace, endings, afterNoAnswer: 0, puts: [] }
  } finally {
    await context.close()
  }
}

function sequences(fates, length) {
  const steps = [...fates].flatMap((fate) => [`e${fate}`, `s${fate}`])
  let all = [[]]
  for (let i = 0; i < length; i++) all = all.flatMap((prefix) => steps.map((step) => [...prefix, step]))
  return all
}

/** A small deterministic generator, so a failing sample can be repeated. */
function sampler(seed) {
  let a = seed
  return () => {
    a = (a + 0x6d2b79f5) | 0
    let t = Math.imul(a ^ (a >>> 15), 1 | a)
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

const random = sampler(16)
const smoke = process.argv.includes("--smoke")
const jobs = []
if (smoke) {
  for (const [name, editor] of Object.entries(EDITORS)) jobs.push({ name, sequence: editor.fates.includes("D") ? ["sD", "sN", "eL"] : ["eN", "eL", "sV"], mode: "third2" })
  jobs.push({ name: "sourceEdit", sequence: ["eN/h", "sY"], mode: "none" }, { name: "sourceEdit", sequence: ["eL/a", "eN/H", "sN/l", "sY"], mode: "none" })
} else {
  for (const [name, editor] of Object.entries(EDITORS)) {
    for (const sequence of sequences(editor.fates, 2)) {
      jobs.push({ name, sequence, mode: "none" })
      for (const mode of ["third1", "third2", "third3", "forget1", "forget2"]) if (random() < SAMPLE) jobs.push({ name, sequence, mode })
    }
    for (const sequence of sequences(editor.fates, 3)) if (random() < SAMPLE / 2) jobs.push({ name, sequence, mode: "none" })
    /* A step that ends with no answer, then the reader away, a Retry elsewhere, or a rename, then any step. */
    if (editor.rename) {
      for (const [first, then] of sequences(editor.fates, 2)) {
        if (!"LNR".includes(first[1])) continue
        for (const after of ["l", "a", "h", "H"]) if (random() < SAMPLE * 2) jobs.push({ name, sequence: [`${first}/${after}`, then], mode: random() < 0.5 ? "none" : "third2" })
      }
    }
  }
}
const ONLY = process.env.FUZZ_ONLY
if (ONLY) jobs.splice(0, jobs.length, ...jobs.filter((job) => `${job.name}:${job.mode}:${job.sequence.join(" ")}`.startsWith(ONLY)))

process.env.BASE_PATH = BASE
rmSync(DIST, { recursive: true, force: true })
await build({ root: ROOT, logLevel: "warn", build: { outDir: DIST, emptyOutDir: true } })

const started = Date.now()
const results = []
let next = 0
await Promise.all(Array.from({ length: WORKERS }, async (_, i) => {
  const daemon = await startFakeDaemon({ port: FIRST_PORT + i, dist: DIST, base: BASE })
  const browser = await chromium.launch()
  try {
    while (next < jobs.length) {
      const job = jobs[next++]
      results.push(await run(daemon, browser, job.name, job.sequence, job.mode))
      if (results.length % 200 === 0) console.log(`${results.length}/${jobs.length}`)
    }
  } finally {
    await browser.close()
    await daemon.close()
  }
}))
rmSync(DIST, { recursive: true, force: true })

const bad = results.filter((r) => r.problems.length)
const groups = new Map()
for (const r of bad) {
  for (const p of r.problems) {
    const key = `${r.name} / ${r.mode.replace(/\d/, "")} / ${p}`
    groups.set(key, [...(groups.get(key) ?? []), r])
  }
}
/* Each form reaches every ending it can, and an edit after no answer, on purpose: a run that never does proves nothing of it. */
const unreached = []
if (!smoke) {
  for (const [name, editor] of Object.entries(EDITORS)) {
    const runs = results.filter((r) => r.name === name)
    if (runs.length === 0) continue
    for (const how of editor.endings) if (!runs.some((r) => r.endings.includes(how))) unreached.push(`${name}: no press ended ${how}`)
    if (!runs.some((r) => r.afterNoAnswer > 0)) unreached.push(`${name}: no edit after no answer`)
  }
}
if (smoke || ONLY) for (const r of results) console.log(r.name, r.sequence, JSON.stringify(r.puts), "last", r.last, "stored", r.stored, "endings", r.endings.join(","), "trace", r.trace.join(","))
const third = results.filter((r) => r.mode.startsWith("third"))
console.log(`${results.length} runs in ${Math.round((Date.now() - started) / 1000)} s, ${third.length} with a third writer: ${bad.length} violations, ${unreached.length} unreached`)
for (const [key, runs] of [...groups].sort()) {
  const first = runs[0]
  console.log(String(runs.length).padStart(4), key, "| e.g.", first.sequence, first.mode, JSON.stringify(first.puts), "last", first.last, "stored", first.stored, "before", first.before, "trace", first.trace.join(","))
}
for (const line of unreached) console.log("unreached:", line)
process.exit(bad.length || unreached.length ? 1 : 0)
