// The page's one store in a browser, against real React and real TanStack Query. Each test runs one case of
// tests/browser/cases.tsx in the page, served by Vite as the page's own modules.
import { mkdtempSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import path from "node:path"
import { after, before, test } from "node:test"
import { fileURLToPath } from "node:url"
import { chromium } from "playwright-core"
import { createServer } from "vite"

const ROOT = fileURLToPath(new URL("../../", import.meta.url))
const PORT = Number(process.env.E2E_PORT ?? 5311) + 30

let server
let browser
let page
let cache
const errors = []

before(async () => {
  cache = mkdtempSync(path.join(tmpdir(), "sidegate-store-"))
  server = await createServer({
    root: ROOT, base: "/", cacheDir: cache, logLevel: "warn",
    server: { host: "127.0.0.1", port: PORT, strictPort: true, hmr: false, watch: null },
    optimizeDeps: { entries: ["tests/browser/index.html"] },
  })
  await server.listen()
  browser = await chromium.launch()
  page = await browser.newPage()
  page.on("pageerror", (error) => errors.push(error))
  await page.goto(`http://127.0.0.1:${PORT}/tests/browser/index.html`)
  await page.waitForFunction(() => "cases" in window, null, { timeout: 60_000 })
})

after(async () => {
  await browser?.close()
  await server?.close()
  if (cache) rmSync(cache, { recursive: true, force: true })
})

const run = async (name, strict = true) => {
  await page.evaluate(([n, s]) => window.cases[n](s), [name, strict])
  if (errors.length > 0) throw errors[0]
}

test("a refused form's draft, kept while its form was closed, survives StrictMode opening the form again", () => run("strictModeKeepsARefusedDraft"))
test("a form closed while its write is out, as that write's refusal lands, keeps its draft and the refusal", () => run("closedAsItsRefusalLandsKeepsTheDraft"))
test("a form closed while its conflict waits for the read, which lands before the task ends, keeps its draft", () => run("closedAsItsConflictLandsKeepsTheDraft"))
for (const strict of [true, false]) {
  test(`a closed form's draft kept after no answer keeps what was typed after its press through its toast Retry's yes${strict ? ", in StrictMode" : ""}`, () => run("aClosedDraftsRetryAnsweredYesKeepsLaterTyping", strict))
  test(`a closed form's draft whose toast Retry's yes leaves only what is stored is released${strict ? ", in StrictMode" : ""}`, () => run("aClosedDraftsRetryLeavingOnlyWhatIsStoredReleasesIt", strict))
}
test("a yes whose read joined the first read of the state, which landed older, is read again", () => run("aYesJoiningTheFirstReadIsReadAgain"))
test("a write ended with no answer, whose read joined one begun before it ended, is read again", () => run("noAnswerJoiningTheFirstReadIsReadAgain"))
test("a read that fails after a yes is asked again after a wait", () => run("aFailedReadAfterAYesIsAskedAgain"))
