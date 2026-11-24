// Parses what the real daemon's query functions returned against the page's schema, so
// the Go shapes and schema.ts can't drift apart unnoticed. lab/verify-control.sh runs it
// on the files its contract step saved. Usage: check-daemon-json.mts DIR
import { readFileSync } from "node:fs"
import { join } from "node:path"
import { z } from "zod"
import { AnalyticsReport, ReplacedEvent, ServerPage, SwitchEvent, View } from "../src/lib/schema.ts"

const dir = process.argv[2]
if (!dir) throw new Error("usage: check-daemon-json.mts DIR")

const checks: [string, z.ZodType][] = [
  ["view.json", View],
  ["switches.json", z.array(SwitchEvent)],
  ["servers.json", ServerPage],
  ["replaced.json", z.array(ReplacedEvent)],
  ["analytics.json", AnalyticsReport],
]
let failed = false
for (const [file, schema] of checks) {
  const parsed = schema.safeParse(JSON.parse(readFileSync(join(dir, file), "utf8")))
  if (parsed.success) {
    console.log(`PASS contract: ${file} parses against schema.ts`)
  } else {
    failed = true
    console.log(`FAIL contract: ${file}: ${JSON.stringify(parsed.error.issues.slice(0, 5))}`)
  }
}
process.exit(failed ? 1 : 0)
