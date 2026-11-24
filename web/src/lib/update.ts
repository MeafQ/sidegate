/* The updater as About draws it, pure: what stands after the version, from the daemon's one view. */
import type { CheckFailure, UpdateOutcome, UpdateReason, UpdateView } from "./schema.ts"
import { REFUSAL_WORDS } from "./troubles.ts"

export type Offer = NonNullable<UpdateView["offered"]>

/** Why the last check learned nothing. */
export const CHECK_WORDS = {
  unreachable: "Could not reach GitHub",
  clockWrong: "Clock is wrong",
  filesUnavailable: "Release files unavailable",
  malformed: REFUSAL_WORDS.unreadableText,
} as const satisfies Record<CheckFailure, string>

/** Why an update failed. */
export const REASON_WORDS = {
  noStart: "Failed to start",
  stopped: "Stopped",
  interrupted: "Interrupted",
  moved: REFUSAL_WORDS.stale,
  offerChanged: REFUSAL_WORDS.offerChanged,
  planChanged: REFUSAL_WORDS.offerChanged,
  notNewer: REFUSAL_WORDS.nothingNewer,
  noRoom: "Not enough space on /data",
  downloadFailed: "Download failed",
  checksum: "Checksum mismatch",
  wrongRelease: "Wrong release",
} as const satisfies Record<UpdateReason, string>

/** How an update that didn't end as `failed` ended. Landed is the new version alone, so it has no words. */
export const OUTCOME_WORDS = {
  landedWithError: "Cleanup failed",
  refused: "Refused",
  rolledBack: "Rolled back",
  putBackFailed: "Recovery needed",
} as const satisfies Record<Exclude<UpdateOutcome, "landed" | "failed">, string>

/** What the last update left to say. `failed`: the old release runs, untouched or put back, and another try is all there is to do.
 * `interrupted`: what it switched is still to be put back, or the put-back release is still to say it answers, so the view is read
 * on. `recovery`: neither release for sure, and the fix is over SSH. `untidy`: the new release runs, and a later step failed. */
export type Ended = { readonly kind: "failed" | "interrupted" | "recovery" | "untidy"; readonly words: string }

/** A release's three numbers, or null for a build that is none. */
const numbers = (version: string) => /^v(\d+)\.(\d+)\.(\d+)$/.exec(version)?.slice(1).map(Number) ?? null

function above(a: string, b: string): boolean {
  const [x, y] = [numbers(a), numbers(b)]
  if (x === null || y === null) return false
  const at = x.findIndex((n, i) => n !== y[i])
  return at >= 0 && (x[at] as number) > (y[at] as number)
}

function endOf(last: NonNullable<UpdateView["last"]>): Ended | null {
  switch (last.outcome) {
    case "landed": return null
    case "landedWithError": return { kind: "untidy", words: OUTCOME_WORDS.landedWithError }
    case "putBackFailed": return { kind: "recovery", words: OUTCOME_WORDS.putBackFailed }
    case "refused":
    case "rolledBack": return { kind: "failed", words: OUTCOME_WORDS[last.outcome] }
    case "failed": return { kind: last.reason === "interrupted" ? "interrupted" : "failed", words: REASON_WORDS[last.reason ?? "stopped"] }
  }
}

/** The last update's end while it is about what runs or is offered now. A release offered since speaks for itself, and so does a
 * newer one that runs, whoever brought it. One that found another release the latest is answered by the check that offers it. */
function standing(view: UpdateView): Ended | null {
  const { version, offered, last } = view
  const ended = last && endOf(last)
  if (!last || !ended) return null
  if (offered) return offered.version === last.version && last.reason !== "offerChanged" ? ended : null
  switch (ended.kind) {
    case "untidy": return last.version === version ? ended : null
    case "recovery": return above(version, last.version) ? null : ended
    case "interrupted":
    case "failed": return above(last.version, version) ? ended : null
  }
}

/** About's line. `to` is the release the arrow names, with its notes' page while it is the one offered. `press` is what can be
 * pressed: `update` the offer's Update, disabled and Pending while one runs; `retry` the same from the end's own card; `check` a
 * check, where nothing is offered; null on a development build. `unchecked` is why the last check learned nothing, said beside
 * Check alone. */
export type Standing = {
  readonly version: string
  readonly to: { readonly version: string; readonly notes: string | null } | null
  readonly ended: Ended | null
  readonly press:
    | { readonly kind: "update"; readonly offer: Offer | null; readonly running: boolean }
    | { readonly kind: "retry"; readonly offer: Offer }
    | { readonly kind: "check"; readonly unchecked: { readonly words: string; readonly at: string | null } | null }
    | null
  readonly guide: string | null
}

export function standingOf(view: UpdateView): Standing {
  const { version, releases, guide, check, offered, running } = view
  const target = running?.version ?? offered?.version ?? version
  const to = target === version ? null : { version: target, notes: offered?.version === target ? offered.notes : null }
  if (!releases) return { version, to: null, ended: null, press: null, guide: null }
  if (running) return { version, to, ended: null, press: { kind: "update", offer: offered, running: true }, guide }
  const ended = standing(view)
  if (offered) return { version, to, ended, press: ended?.kind === "failed" ? { kind: "retry", offer: offered } : { kind: "update", offer: offered, running: false }, guide }
  return { version, to, ended, press: { kind: "check", unchecked: check.failure && { words: CHECK_WORDS[check.failure], at: check.at } }, guide }
}

/** The view is about to change by itself, so the page reads on. */
export const moving = (view: UpdateView): boolean => view.running !== null || view.last?.reason === "interrupted"

/** Whether `after` names an update that `before` didn't: one that runs, or one that ended since. */
export const pressedSince = (before: UpdateView | undefined, after: UpdateView): boolean =>
  after.running !== null || (after.last !== null && after.last.attempt !== before?.last?.attempt)
