import { ago, DAY, msPair } from "./format.ts"
import type { ProbeResult, ServerView } from "./schema.ts"

/** A server is never Unstable: that is a group's or source's trouble, its failovers. */
export type ServerRating = "good" | "bad" | "none"
export type Rating = ServerRating | "unstable"

export const RATING_WORDS = { good: "Online", unstable: "Unstable", bad: "Offline", none: "Unchecked" } as const satisfies Record<Rating, string>

/** What is true of a server now, for its dot, the Health filter and a slot serving on it: a pass in the last 10 minutes is good,
 * a failure in them bad, and anything else isn't rated, since a green dot on a server nothing probed lately would claim what nobody measured.
 * Missing and Unsupported are words beside the name, not dots. Its week is incidents7d's and factor's, not the dot's. */
export function rate({ health }: Pick<ServerView, "health">): { rating: ServerRating; why: string[] } {
  const { current, verdict, checkedAt } = health
  if (current && verdict === "succeeded") return { rating: "good", why: [] }
  if (current && verdict === "failed") return { rating: "bad", why: ["Not responding"] }
  return { rating: "none", why: checkedAt ? [`Last checked ${ago(checkedAt)}`] : [] }
}

/** A failed Test in words: its Dot's rating, its word and the reason its card holds. A pass is its ping and has none. */
export function testWords(result: ProbeResult): { rating: ServerRating; words: string; reason: string } | null {
  if (result.verdict === "succeeded") return null
  return result.verdict === "failed" ? { rating: "bad", words: "Failed", reason: "No response" } : { rating: "none", words: "Not run", reason: "No route" }
}

/** What a server can be to a slot, the first of these that holds: one Xray can't run, which no Exit takes; gone from its source's
 * feed, missing for now or for good, a tombstone that no list shows; or usable. A missing server Xray can't run is Unsupported
 * wherever it is named, since that is why nothing takes it. Its dot is `rate`'s, whatever its standing. */
export type ServerStanding = "unsupported" | "missing" | "tombstone" | "usable"

export const serverStanding = ({ presence, supported }: Pick<ServerView, "presence" | "supported">): ServerStanding =>
  !supported ? "unsupported" : presence === "present" ? "usable" : presence

/** A standing's word beside a server's name, where it has one. */
export const STANDING_WORDS = { unsupported: "Unsupported", missing: "Missing", tombstone: "Missing", usable: null } as const satisfies Record<ServerStanding, string | null>

/** "50 / 90 ms": typical and bad-case ping. */
export const pingPair = ({ pingP50Ms, pingP95Ms }: ServerView["health"]): string => msPair(pingP50Ms, pingP95Ms)

/** "12d ago" when the record's pings are over a day old, since a dormant server's ping is a memory. */
export const pingAge = ({ pingP50Ms, pingAt }: ServerView["health"], now = Date.now()): string | null =>
  pingP50Ms !== null && pingAt && now - Date.parse(pingAt) > DAY ? ago(pingAt, now) : null
