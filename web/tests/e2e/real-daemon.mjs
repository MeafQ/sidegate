// The real daemon's handler and controller behind the fake daemon's interface, so a check written against the fake can
// run against the Go code too. It runs internal/daemon's TestPageDaemon, and calls its controls synchronously because the
// checks read the fake's state synchronously.
import { spawn, spawnSync } from "node:child_process"
import { mkdtempSync, rmSync } from "node:fs"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"

const REPO = fileURLToPath(new URL("../../../", import.meta.url))

export async function startRealDaemon({ port, dist, base }) {
  const dir = mkdtempSync(path.join(os.tmpdir(), "real-daemon-"))
  const binary = path.join(dir, "daemon.test")
  const built = spawnSync("go", ["test", "-c", "-o", binary, "./internal/daemon"], { cwd: REPO, stdio: "inherit" })
  if (built.status !== 0) throw new Error("the daemon's test binary didn't build")
  const child = spawn(binary, ["-test.run", "^TestPageDaemon$", "-test.timeout", "0"], {
    env: { ...process.env, PAGE_DAEMON_PORT: String(port), PAGE_DAEMON_DIST: dist, PAGE_DAEMON_BASE: base },
    stdio: ["ignore", "pipe", "inherit"],
  })
  await new Promise((resolve, reject) => {
    child.stdout.on("data", (chunk) => { if (String(chunk).includes("page daemon listening")) resolve() })
    child.on("exit", (code) => reject(new Error(`the daemon's test binary exited with ${code}`)))
  })

  const control = (name, { query = {}, body } = {}) => {
    const url = `http://127.0.0.1:${port}/__control/${name}?${new URLSearchParams(query)}`
    const args = ["-s", "--fail-with-body", "--noproxy", "*", "-X", "POST", "-d", JSON.stringify(body ?? {}), url]
    const { status, stdout, stderr } = spawnSync("curl", args, { encoding: "utf8", maxBuffer: 1 << 26 })
    if (status !== 0) throw new Error(`control ${name} failed: ${stdout}${stderr}`)
    return JSON.parse(stdout)
  }

  return {
    url: `http://127.0.0.1:${port}${base}`,
    reset: () => control("reset"),
    log: () => control("log") ?? [],
    slot: (id) => control("slot", { query: { id } }) ?? undefined,
    slots: () => control("slots"),
    group: (id) => control("group", { query: { id } }) ?? undefined,
    groups: () => control("groups"),
    renameGroup: (id, name) => control("renameGroup", { query: { id }, body: { name } }),
    deleteGroup: (id) => control("deleteGroup", { query: { id } }),
    settings: () => control("settings"),
    sseClients: () => control("sseClients"),
    set(patch) {
      if (patch.writes) control("forgetKeys")
      if (patch.sources) {
        for (const [id, sub] of patch.sources) {
          control("failFetch", { query: { id }, body: { failures: sub.origin.fetch.failures, failureCode: sub.origin.fetch.failureCode, diagnostics: sub.diagnostics } })
        }
      }
      control("set", { body: patch })
    },
    renameSlot: (id, name) => control("renameSlot", { query: { id }, body: { name } }),
    deleteSlot: (id) => control("deleteSlot", { query: { id } }),
    deleteSource: (id) => control("deleteSource", { query: { id } }),
    source: (id) => control("source", { query: { id } }) ?? undefined,
    secrets: (id) => control("secrets", { query: { id } }),
    sources: () => control("sources"),
    renameSource: (id, name) => control("renameSource", { query: { id }, body: { name } }),
    changeSettings: (patch) => control("changeSettings", { body: patch }),
    endUpdate: (end) => control("endUpdate", { body: end }),
    rotateToken: () => control("rotateToken"),
    prependServer: () => control("prependServer"),
    missServer: (id) => control("missServer", { query: { id } }),
    dropEvents: () => control("dropEvents"),
    reference: (id) => control("reference", { query: { id } }),
    close: () => new Promise((done) => {
      child.removeAllListeners("exit")
      child.on("exit", () => {
        rmSync(dir, { recursive: true, force: true })
        done()
      })
      child.kill()
    }),
  }
}
