import assert from "node:assert/strict"
import { readdirSync, readFileSync } from "node:fs"
import { test } from "node:test"
import type { z } from "zod"
import {
  AnalyticsReport, ChangeEvent, CheckFailure, DiagnosticCode, DownReason, FailureCode, GroupCreate, GroupInput, GroupPreview, GroupView, ProbeResult, ReplacedEvent, RuntimePhase, ServerPage, Session, SettingsView, SignalKind, UDPState,
  SlotInput, SlotUnifi, SlotView, SourceView, SwitchEvent, SwitchReason, UpdateOutcome, UpdateReason, UpdateView, Verdict, View, VpnClients,
} from "./schema.ts"
import { DIAGNOSTIC_WORDS, FAILURE_WORDS, failureWords } from "./format.ts"
import { REFUSAL_WORDS, refusalWords } from "./troubles.ts"

/* internal/daemon's TestWireSamples writes these from the real view functions, so a Go change without new samples fails
 * Go's test, and new samples without a schema change fail here. */
const dir = new URL("../../tests/wire/", import.meta.url)
const read = (name: string): unknown => JSON.parse(readFileSync(new URL(name, dir), "utf8"))
const samples = readdirSync(dir).filter((f) => f.endsWith(".json") && f !== "enums.json").map((f) => f.replace(/\.json$/, ""))

const SCHEMAS: Record<string, z.ZodType> = {
  AnalyticsReport, ChangeEvent, GroupCreate, GroupInput, GroupPreview, GroupView, ProbeResult, ReplacedEvent, ServerPage, Session, SettingsView,
  SlotInput, SlotUnifi, SlotView, SourceView, SwitchEvent, UpdateView, View, VpnClients,
}

test("every sample parses with its schema and comes back whole", () => {
  for (const name of samples) {
    const schema = SCHEMAS[name.split(".")[0] ?? ""]
    assert.ok(schema, `${name} has no schema here`)
    const sample = read(`${name}.json`)
    const parsed = schema.safeParse(sample)
    assert.ok(parsed.success, `${name}: ${parsed.success ? "" : JSON.stringify(parsed.error.issues)}`)
    assert.deepEqual(parsed.data, sample, `${name} didn't round-trip`)
  }
})

test("every schema has a sample", () => {
  for (const name of Object.keys(SCHEMAS)) assert.ok(samples.some((s) => s.startsWith(`${name}.`)), `no sample of ${name}`)
})

test("a field the schema doesn't know fails it", () => {
  const sample = read("SlotView.serving.json") as Record<string, unknown>
  assert.equal(SlotView.safeParse({ ...sample, extra: 1 }).success, false)
  assert.equal(SlotView.safeParse({ ...sample, unifi: { ...(sample["unifi"] as object), extra: 1 } }).success, false)
})

function collect(value: unknown, into: Map<string, Set<string>>, key = ""): void {
  if (Array.isArray(value)) value.forEach((v) => collect(v, into, key))
  else if (value !== null && typeof value === "object") Object.entries(value).forEach(([k, v]) => collect(v, into, k))
  else if (typeof value === "string") into.set(key, (into.get(key) ?? new Set()).add(value))
}

const within = (prefix: string) => {
  const seen = new Map<string, Set<string>>()
  for (const name of samples) if (name.startsWith(prefix)) collect(read(`${name}.json`), seen)
  return seen
}

test("every union arm has a sample", () => {
  const arms = (seen: Map<string, Set<string>>, key: string, want: string[]) => {
    for (const arm of want) assert.ok(seen.get(key)?.has(arm), `no sample has ${key} ${arm}`)
  }
  const slots = within("SlotView.")
  arms(slots, "kind", ["server", "group", "source", "direct", "block", "up", "down"])
  arms(slots, "status", ["serving", "failed", "direct", "block"])
  arms(slots, "client", ["present", "disabled", "deleted"])
  arms(within("SlotInput."), "kind", ["server", "group", "source", "direct", "block"])
  arms(within("SourceView."), "kind", ["remote", "manual"])
  arms(within("SwitchEvent."), "kind", ["up", "down", "slot", "group", "source"])
  const unread = read("SourceView.unread.json") as { feed: unknown }
  assert.equal(unread.feed, null, "a source before its first parse has no feed")
  const unknown = read("SlotUnifi.unknown.json") as { known: boolean; client: unknown }
  assert.equal(unknown.known, false)
  assert.equal(unknown.client, "none")
  assert.equal((read("VpnClients.unknown.json") as { known: boolean }).known, false)
})

test("every Go enum equals the schema's", () => {
  const enums = read("enums.json") as Record<string, string[]>
  const ours: Record<string, { options: readonly string[] }> = { DownReason, SwitchReason, SignalKind, Verdict, RuntimePhase, UDPState, FailureCode, DiagnosticCode, UpdateOutcome, UpdateReason, CheckFailure }
  assert.deepEqual(Object.keys(enums).filter((name) => name !== "RefusalReason").toSorted(), Object.keys(ours).toSorted())
  for (const [name, schema] of Object.entries(ours)) {
    assert.deepEqual([...schema.options].toSorted(), enums[name], `${name} differs between Go and schema.ts`)
  }
})

test("every code Go sends has words of its own, and the page has none for a code Go doesn't send", () => {
  const enums = read("enums.json") as Record<string, string[]>
  for (const [name, words] of [["FailureCode", FAILURE_WORDS], ["DiagnosticCode", DIAGNOSTIC_WORDS]] as const) {
    assert.deepEqual(Object.keys(words).toSorted(), enums[name], `${name}: the page's words and Go's codes differ`)
    assert.equal(new Set(Object.values(words)).size, Object.keys(words).length, `${name}: two codes share words`)
  }
  assert.equal(failureWords("http_503"), "HTTP 503")
  assert.equal(failureWords("http_418"), "HTTP 418", "any status reads as itself")
})

test("every reason the daemon refuses for has the page's words, and the page has none for a reason it doesn't give", () => {
  const enums = read("enums.json") as Record<string, string[]>
  assert.deepEqual(Object.keys(REFUSAL_WORDS).toSorted(), enums.RefusalReason, "the page's refusal words and Go's reasons differ")
  for (const [reason, words] of Object.entries(REFUSAL_WORDS)) assert.ok(words.split(" ").length <= 6 || reason === "notHttps", `${reason}: "${words}" is more than a few words`)
  assert.equal(REFUSAL_WORDS.sourceEmpty, "No servers", "a source with no servers reads as its trouble does")
  assert.equal(refusalWords({ reason: "badPattern", detail: "missing closing ]", message: "Include pattern: …" }), "Missing closing ]", "a refused pattern says Go's cause")
  assert.equal(refusalWords({ reason: "fromTomorrow", detail: null, message: "A sentence." }), "A sentence.", "a reason the page doesn't know is the daemon's message")
})
