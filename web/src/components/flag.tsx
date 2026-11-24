import { countryName } from "@/lib/format"
import { cn } from "@/lib/utils"

/* Shipped with the page, as files: the gateway serves them, and no flag is fetched from anywhere else. */
const FILES = import.meta.glob<string>("/node_modules/circle-flags/flags/??.svg", { eager: true, query: "?url&no-inline", import: "default" })
const fileOf = (code: string) => FILES[`/node_modules/circle-flags/flags/${code.toLowerCase()}.svg`]

/** A country as a filter's option, a chip and a server's details draw it: its round flag and its name. */
export const countryDrawing = (code: string | null) => ({ words: countryName(code), mark: <Flag code={code} /> })

/** A country's round flag. No country, or one with no flag, gets the set's own unknown flag. */
export function Flag({ code, className }: { code: string | null; className?: string }) {
  const name = countryName(code)
  return <img src={(code ? fileOf(code) : undefined) ?? fileOf("xx")} alt={name} title={name} loading="lazy" className={cn("inline-block size-4 shrink-0", className)} />
}
