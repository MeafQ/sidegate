import { DAY, counted, day, span, spanOf, spanSpoken, time } from "./format.ts"
import type { ServerView } from "./schema.ts"

const WEEK = 7 * DAY

/** How far the daemon trusts a server, as four bars read like phone signal. */
export type Bars = 0 | 1 | 2 | 3 | 4
export const MOST_BARS = 4

/** The daemon's factor is 1 plus the failures it expects of a server per day of use: a quarter for a server nothing is known of,
 * falling as it is seen working and rising with each failure. Each bar halves it. These are starting values, to check against real
 * servers. */
const STEPS: readonly (readonly [Bars, number])[] = [[4, 1 / 16], [3, 1 / 8], [2, 1 / 4], [1, 1]]
export const barsOf = (factor: number): Bars => STEPS.find(([, most]) => factor - 1 <= most)?.[0] ?? 0

/** One line of the bars' card: a muted name and its value, and the sentence a screen reader hears for it. */
type Line = { name: string; value: string; heard: string }

/** A server's bars and what they are made from, in the card's order: the week's failures, the time seen working, the last failure.
 * `heard` is the sentence a screen reader hears for the cell. */
export type Stability = { bars: Bars; lines: Line[]; heard: string }

/** The header's card, in the table and over the same column in the Exit menu: the top, the start and the bottom, by their bars. */
export const BARS_NAMED: readonly (readonly [Bars, string])[] = [[4, "Most trusted"], [2, "Starting level"], [0, "Least trusted"]]

/** "today 09:20", "Thu 14:10" within the week, "Nov 2, 21:15" before it, in local time. */
export function failedAt(iso: string, now: number): string {
  const at = new Date(iso)
  const midnight = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
  const days = Math.round((midnight(new Date(now)) - midnight(at)) / DAY)
  if (days === 0) return `today ${time(iso)}`
  if (days > 0 && days < 7) return `${at.toLocaleDateString("en-US", { weekday: "short" })} ${time(iso)}`
  return `${day(iso, now)}, ${time(iso)}`
}

const line = (name: string, value: string, heard: string): Line => ({ name, value, heard })

/** The factor the daemon gives a server it knows nothing of. */
const STARTING_FACTOR = 1.25

/** A server's stability, or where every server starts for one the daemon has no record of. */
export function stabilityOf(health: ServerView["health"] | null, now: number): Stability {
  const bars = barsOf(health?.factor ?? STARTING_FACTOR)
  const lines = linesOf(health, now)
  return { bars, lines, heard: [`Stability ${bars} of ${MOST_BARS}`, ...lines.map((l) => l.heard).filter(Boolean)].join(". ") + "." }
}

/** A dash where there is no reading: a server never checked, or not in the week, has no failures or time to count. */
function linesOf(health: ServerView["health"] | null, now: number): Line[] {
  const unread = (heard: string) => [line("Failures", "—", heard), line("Seen working", "—", "")]
  if (!health || health.checkedAt === null) return unread("Not checked yet")
  const { checkedAt, seenWorking7dMs: worked, incidents7d: n, lastFailureAt } = health
  const last = lastFailureAt === null ? [] : [line("Last failure", failedAt(lastFailureAt, now), `Last failure ${failedAt(lastFailureAt, now)}`)]
  if (now - Date.parse(checkedAt) >= WEEK) return [...unread("Not checked in the last 7 days"), ...last]
  const failures = n === 0 ? "No failure" : counted(n, "failure").replace(/^\d+/, n.toLocaleString("en-US"))
  return [
    line("Failures", n.toLocaleString("en-US"), `${failures} in the last 7 days`),
    worked === null ? line("Seen working", "—", "Time seen working not known")
      : worked === 0 ? line("Seen working", "0m", "Not seen working in the last 7 days")
      : line("Seen working", span(worked), `Seen working ${spanSpoken(spanOf(worked))}`),
    ...last,
  ]
}
