import { counted, DIAGNOSTIC_WORDS } from "./format.ts"
import type { CountrySource, DiagnosticCode, LineageId, SourceView } from "./schema.ts"

export const COUNTRY_SOURCE_WORDS: Record<CountrySource, string> = { rule: "From a rule", tagFlag: "From the tag's flag", configFlag: "From the config's flag", unknown: "Unknown" }

const skipReason = (code: DiagnosticCode, field: string) =>
  field && (code === "unsupported_option" || (code === "unsupported_protocol" && field !== "other"))
    ? `${DIAGNOSTIC_WORDS[code]} (${field})`
    : DIAGNOSTIC_WORDS[code]

/** A refused entry: its name, "proxy" or "Entry 9", why, "Insecure TLS", and the config that lists it, or null for one only a
 * diagnostic names. */
export type SkippedEntry = { config: string | null; name: string; reason: string }

/** A source's refused entries, each once: under the key of the config that lists it, or loose for a config with no server, which
 * has no heading, and for a diagnostic no config lists. A diagnostic numbers the proxy roots of the whole document from 0, in
 * document order, so its number finds the config's refusal it repeats. Helpers and hops are parts of a config, never skipped. */
export function skippedOf({ feed, diagnostics }: Pick<SourceView, "feed" | "diagnostics">): { byConfig: Map<LineageId, SkippedEntry[]>; loose: SkippedEntry[] } {
  const byConfig = new Map<LineageId, SkippedEntry[]>()
  const loose: SkippedEntry[] = []
  const listed = new Set<string>()
  let roots = 0
  for (const config of feed?.configs ?? []) {
    const refused = config.skipped.flatMap((skip) => (skip.reason === "refused" ? [skip] : []))
    const entries = refused.map((skip) => {
      listed.add(`${roots + skip.position - 1} ${skip.code}`)
      return { config: config.name || "Unnamed", name: skip.tag || `#${skip.position}`, reason: skipReason(skip.code, skip.field) }
    })
    roots += config.servers + refused.length
    if (entries.length === 0) continue
    if (config.key === null) loose.push(...entries)
    else byConfig.set(config.key, entries)
  }
  for (const d of diagnostics) {
    if (!listed.has(`${d.entry} ${d.code}`)) loose.push({ config: null, name: `Entry ${d.entry}`, reason: skipReason(d.code, d.field) })
  }
  return { byConfig, loose }
}

/** "1 entry skipped", "3 entries skipped". */
export const skippedWords = (n: number) => counted(n, "entry skipped", "entries skipped")
