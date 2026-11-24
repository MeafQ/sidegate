import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { test } from "node:test"

const read = (path: string) => readFileSync(new URL(path, import.meta.url), "utf8")
const contract = read("../../API.md")
/** The page's routes: its writes and the preview in api.ts, its reads in queries.ts. */
const client = read("./api.ts") + read("./queries.ts")

test("API.md lists every route the page calls", () => {
  const routes = [...client.matchAll(/["`](api\/[^"`?]*)/g)].map((m) => (m[1] ?? "").replace(/\$\{[^}]+\}/g, "{id}"))
  assert.ok(routes.length >= 12, `found only ${routes.length} routes`)
  for (const route of routes) assert.ok(contract.includes(route), `API.md doesn't list ${route}`)
})

test("API.md lists every error kind the page tells apart", () => {
  const kinds = /const KINDS[^=]*= \[([^\]]+)\]/.exec(client)?.[1]?.match(/[a-zA-Z]+/g) ?? []
  assert.ok(kinds.length >= 8, `found only ${kinds.length} kinds`)
  for (const kind of kinds) assert.ok(contract.includes(`\`${kind}\``), `API.md doesn't list ${kind}`)
})

test("API.md covers each part of the contract", () => {
  const parts = ["If-Match", "If-None-Match", "ETag", "Idempotency-Key", "X-Sidegate-Csrf", "Replay", "5xx is not remembered", "Read your own write", "404", "Retention"]
  for (const part of parts) assert.ok(contract.includes(part), `API.md doesn't cover ${part}`)
})
