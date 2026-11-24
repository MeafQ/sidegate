/* The page's one write path, its pure part: what a write is, the machine of one control's write, when it settles, and
 * what its toast says. No React, no fetch, no timers: use-write.ts runs it. */
import type { ZodType } from "zod"
import type { SettingsView, View } from "./schema.ts"
import { refusalWords, toastWhy } from "./troubles.ts"

/** A control's identity across mounts. Built only by `actions.*` in api.ts, so one control is never named two ways. */
export type ControlKey = string & { readonly __control: unique symbol }

/** A counter the store moves on every event. Two events never share a stamp, so no ordering is read from a clock. */
export type Seq = number & { readonly __seq: unique symbol }

/** The reads a write can settle on. Each carries a revision the daemon issued. */
export type ReadName = "state" | "settings"

/** How a press becomes a request. Two actions may share a control (a source's PUT and its DELETE), and the control
 * takes one write at a time whichever pressed it. `settles` names the read whose revision must reach the answer's;
 * "none" is a result and not a change (a Test). `W` is the ways into the control a press names, where it has more than one. */
export type Action<B, R, W extends string = string> = {
  readonly control: ControlKey
  /** The ways into the control, each a press of its own that alone spins. */
  readonly ways?: readonly W[]
  readonly method: "PUT" | "POST" | "DELETE"
  readonly path: string
  /** Where a press goes when what was pressed names it under `path`, as a press names the server it prefers. */
  readonly to?: (pressed: B) => string
  /** A create, at an id under `path` the store picks: a fresh one for each new request, and a lost create's own for a press of what
   * it sent, so what the daemon finds stored at an id is the one request sent there. `listed` is what the action's read holds, where
   * a lost create is found landed, since only this page knew its id. */
  readonly creates?: { readonly listed: (data: ReadData) => readonly { readonly id: string }[] }
  /** Which making of the item at `path` the action's read lists, by a mark the read gives it, or null while it lists none. Each
   * thing the control holds for the item, its draft and each way's write, is for the making listed when it began. */
  readonly creation?: (data: ReadData) => Made
  /** The JSON body from what was pressed, or undefined for a bodiless request. Called once, when the write is made. */
  readonly body: (pressed: B) => unknown
  readonly settles: ReadName | "none"
  /** Parses a 2xx body. A body that doesn't parse is version skew and reads as `unknown`: it may have landed. */
  readonly answer: ZodType<R>
  /** A yes that did nothing, in words: a press whose Test failed or couldn't run moved nothing. The toast says it, wherever the
   * reader is, once the write settles. Null: the yes did what was pressed. A method, so an action stands as one of any answer. */
  failed?(answer: R): Failed | null
}

/** Why a yes did nothing, as its toast says it: the word for what happened ("Failed") and its muted reason ("No response"). */
export type Failed = { readonly words: string; readonly reason: string }

/** A making of an item, as `Action.creation` marks it; null where the read listed none, or the action marks none. */
export type Made = string | null

/** A control's last answer and the action that gave it, whichever way into the control pressed it. Two actions can share a
 * control, as a press and its Clear do, and an answer is its own action's alone: a Clear's acknowledgement is no Test's result.
 * It stands ANSWER_MS from when it came. */
export type Answered = { readonly by: string; readonly body: unknown }

/** How long a control's answer stands once it came, whatever is on screen: a Test's is a one-off reading that nothing keeps
 * measuring, so it doesn't stay to read as current. */
export const ANSWER_MS = 60_000

const answerer = (action: { readonly method: string; readonly path: string }) => `${action.method} ${action.path}`

/** `action`'s own answer among its control's, or null when another action on the control answered last. */
export const answerOf = <B, R>(last: Answered | null, action: Action<B, R>): R | null => (last?.by === answerer(action) ? (last.body as R) : null)

/** A create must find nothing at its id; a replace names the revision it started from. */
export type Precondition = { readonly kind: "create" } | { readonly kind: "replace"; readonly revision: number }

/** The edits of a form's draft that a write's bytes hold: each field with the Seq of the edit whose value they carry. */
export type Edits = ReadonlyMap<string, Seq>
export const NO_EDITS: Edits = new Map()

/** One write as sent. `bytes` is the body's JSON as first sent and every resend sends exactly it, so the daemon's
 * fingerprint (method, path, If-Match, If-None-Match, body bytes) never drifts under one key. Anything else pressed is
 * a new Write under a new key. */
export type Write<B> = {
  readonly key: string
  readonly method: "PUT" | "POST" | "DELETE"
  readonly path: string
  readonly bytes: string | null
  readonly precondition: Precondition | null
  /** The id a create goes to, the store's; null for any other write. */
  readonly made: string | null
  /** What was pressed, for the control to show while the write is out. Never compared with anything stored. */
  readonly pressed: B
  /** Fixed when the bytes are made and kept through every resend and Retry of them, so a yes drops exactly these edits
   * and nothing typed after them. */
  readonly edits: Edits
}

/** What one HTTP attempt came to. Nothing with a status code or a Response crosses api.ts but these. `revision` is the
 * daemon's `Sidegate-Revision`: the view revision the write was committed at, the first answer's on a replay. */
export type Answer =
  | { readonly kind: "yes"; readonly revision: number; readonly body: unknown }
  /** 412 from the daemon, with the revision of the item it found stored. */
  | { readonly kind: "conflict"; readonly revision: number; readonly stored: number }
  /** The daemon's own 4xx: it looked at the key and stored nothing under it. `reason` names why for the page to word, `detail`
   * is a refused pattern's cause, and `message` is the daemon's sentence for a reason the page doesn't know. */
  | { readonly kind: "refused"; readonly revision: number; readonly status: number; readonly code: string; readonly reason: string; readonly detail: string | null; readonly message: string }
  /** A plain 4xx from in front of the daemon (413, 429): this attempt never reached it. `retry`: asking later may take. */
  | { readonly kind: "front"; readonly status: number; readonly retry: boolean }
  /** 401 or 403 from in front of the daemon, or the daemon's csrf refusal: this attempt stored nothing, and a session
   * reread may heal it. */
  | { readonly kind: "token"; readonly status: 401 | 403 }
  /** No connection, a 5xx, or a 2xx the page can't read: it may have landed. */
  | { readonly kind: "unknown"; readonly status: number }

/** Resend offsets from the first send, then the end. Six attempts in all. */
export const SCHEDULE_MS = [1_000, 2_000, 4_000, 8_000, 15_000] as const
export const END_MS = 30_000
export const SLOW_MS = 1_000
/** A read with no answer by then is a failed read (rule 4). */
export const READ_TIMEOUT_MS = 10_000
/** While a write waits on a read and the reads begun after it land behind it or fail, the next is asked this long after
 * the last one ended, the wait doubling up to READ_TIMEOUT_MS. */
export const REREAD_MS = 1_000

/** What the store knows of one read, from the query cache's events and never from a promise. `landedBeganAt` is the
 * Seq at which the fetch that brought `data` began; data the page put there itself moves it not. */
export type ReadMark<T> = { readonly data: T | undefined; readonly landedBeganAt: Seq | null }
export type Reads = { readonly state: ReadMark<View>; readonly settings: ReadMark<SettingsView> }
export type ReadData = View | SettingsView

/** The read a control waits on and the revision it must reach. Two numbers the daemon issued; the page only compares them. */
export type Landmark = { readonly read: ReadName; readonly revision: number }

/** A daemon refusal. `revision` is its `Sidegate-Revision`, the view revision it refused at: a view older than it may not show
 * what the refusal is about. */
export type Refusal = {
  readonly code: string; readonly reason: string; readonly detail: string | null; readonly message: string; readonly status: number; readonly revision: number
  /** The form field it is about, fixed by the press: the one field a source's Save changed, the field an add's reason names.
   * Null: a control's press, or a reason about every field. */
  readonly field: string | null
}

/** A conflict, read from the data that reached its landmark. A single control names both values and rebuilds the
 * reader's write over what is stored now (Keep mine). A form names what is stored and is fixed by its own Save. A form whose
 * touched fields all hold what is stored now has nothing to show: it ends as a yes. */
export type Conflicted<B> = Disagreed<B> | { readonly kind: "agreed" }
export type Disagreed<B> =
  | { readonly kind: "control"; readonly theirs: string; readonly mine: string; readonly again: { readonly pressed: B; readonly precondition: Precondition } }
  | { readonly kind: "form"; readonly stored: string | null }

/** The thing a refusal is about: its name, and the route of the page where it is fixed, when it has one. */
export type Refused = { readonly what: string; readonly open: string | undefined }

/** What a press says about itself. `what` is the toast's first words ("EU", "Settings"). */
export type PressHow<B, W extends string = string> = {
  readonly what: string
  readonly precondition?: Precondition
  /** The conflict's words. Null: the item is gone from the data, so it was deleted elsewhere. Absent: "changed in another tab". */
  readonly conflict?: (data: ReadData) => Conflicted<B> | null
  /** What a daemon refusal is about when it isn't what the press names: an Exit refused as it is picked. Its name is the
   * toast's what, and its page, where it is fixed, the route of Open. Undefined: the refusal names `what`, with no button. */
  readonly refused?: (refusal: Refusal) => Refused | undefined
  /** Where what a create made is, once it lands: opened at once, with `state`, while `here` says the reader is where they pressed,
   * and otherwise the "✓ what · Open" toast's. Absent: what lands is in sight, and nothing is said. */
  readonly opens?: (made: string | null) => { readonly route: string; readonly here: boolean; readonly state?: unknown }
  /** Whether the form draws this refusal itself, under its field. Otherwise the store says the toast. */
  readonly shows?: (refusal: Refusal) => boolean
  /** A form's press: the field a refusal with this reason is about, from the fields the press changed. */
  readonly about?: (reason: string) => string | null
  /** A form's press: its draft holds nothing to save over what is stored now. */
  readonly clean?: () => boolean
  /** A form's press: its form is closed now. A refusal the form draws that comes then is also said, at once. */
  readonly away?: () => boolean
  /** A form's press: the why of its conflict when it meets one sent again under its key after no answer, in place of "changed
   * in another tab", since the press it repeats is all it is sure of. */
  readonly resentWhy?: string
  /** Which of the control's ways pressed it, where it has more than one, so only that one is drawn Pending. */
  readonly way?: W
}

/** One control's write, present exactly while the control is Pending. Deleted when it settles, so a sent value after
 * settling can't be represented. */
export type Pending<B> = {
  readonly write: Write<B>
  readonly action: Action<B, unknown>
  readonly how: PressHow<B>
  /** The press's stamp. Timers and answers name it, so one record's events never reach another under the same key. */
  readonly startedAt: Seq
  /** Pending for a second or more: a drawing fact, never a transition. */
  readonly slow: boolean
  readonly phase:
    /** Attempts out by the schedule. `attempts` counts sends; `front` those answered from in front of the daemon, which
     * never reached it. While another may have, the write may have landed. `unsure`: it is a lost write sent again under
     * its key, which an earlier record's attempt may have landed. */
    | { readonly kind: "out"; readonly attempts: number; readonly front: number; readonly unsure: boolean }
    /** Answered at `since`. Pending until the read passes the landmark, then the ending is said. A read begun after
     * `since` shows the write, so the store keeps one out or due until then. */
    | { readonly kind: "waiting"; readonly landmark: Landmark; readonly then: Then; readonly since: Seq }
    /** Ended with its fate unknown: 30 s passed, or an attempt was refused in front after another may have landed.
     * Every attempt is aborted and the toast said. Settles on a read that began after `since`. */
    | { readonly kind: "unanswered"; readonly since: Seq }
}

type Out = Extract<Pending<unknown>["phase"], { kind: "out" }>

/** `refused`: the control drew the daemon's refusal over a view that may not yet hold what it is about; `said`: it was also said
 * when it came, since its form was closed. `resent`: the conflicting write was a lost one sent again under its key. `failed`: the
 * yes did nothing (`Action.failed`), which its arrival says. */
export type Then =
  | { readonly kind: "answered"; readonly failed: Failed | null } | { readonly kind: "conflict"; readonly resent: boolean } | { readonly kind: "gone" }
  | { readonly kind: "refused"; readonly refusal: Refusal; readonly said: boolean }

export type Event<B> =
  /** `unsure`: the write is a lost one sent again under its key, whose fate is unknown. */
  | { readonly kind: "press"; readonly write: Write<B>; readonly action: Action<B, unknown>; readonly how: PressHow<B>; readonly unsure: boolean; readonly at: Seq }
  | { readonly kind: "answer"; readonly of: Seq; readonly attempt: number; readonly answer: Answer; readonly at: Seq }
  /** Timer n of the schedule: 1..5 resend, 6 is the end at 30 s. */
  | { readonly kind: "timer"; readonly of: Seq; readonly n: number; readonly at: Seq }
  | { readonly kind: "slow"; readonly of: Seq }
  | { readonly kind: "read"; readonly at: Seq }

/** How a write ended, for its toast. */
export type Ending =
  | { readonly kind: "noAnswer" }
  | { readonly kind: "refused"; readonly refusal: Refusal }
  | { readonly kind: "front"; readonly status: number; readonly retry: boolean }
  | { readonly kind: "unifi"; readonly status: 401 | 403 }
  | { readonly kind: "gone" }
  | { readonly kind: "conflict"; readonly conflicted: Disagreed<unknown> | undefined; readonly resent: boolean }
  | { readonly kind: "landed"; readonly route: string }
  | { readonly kind: "failed"; readonly failed: Failed }

export type Effect =
  /** Send the record's write as attempt `attempt`; `heal` rereads api/session first. */
  | { readonly kind: "send"; readonly attempt: number; readonly heal: boolean }
  /** Arm the slow mark and the schedule's six timers from now. */
  | { readonly kind: "schedule" }
  /** Clear the timers and abort every attempt, so no answer arrives after the write left `out`. */
  | { readonly kind: "stop" }
  /** Ask the read again, so a landmark is reached by a refetch and not only by the event feed. */
  | { readonly kind: "refetch"; readonly read: ReadName }
  | { readonly kind: "say"; readonly ending: Ending }
  /** A pressed control's earlier toast goes. */
  | { readonly kind: "dismiss" }
  /** A yes: its answer is the control's result for ANSWER_MS, a Test's ping or a press's, named by the action that gave it. */
  | { readonly kind: "keep"; readonly answer: Answered }
  /** The form draws this refusal until its next edit or press; null: it draws none. */
  | { readonly kind: "refusal"; readonly refusal: Refusal | null }
  /** The write landed: the draft's edits its bytes held are dropped. */
  | { readonly kind: "landed"; readonly edits: Edits }
  /** The item the write is to is gone: what the control holds for it goes too, so one made again at its id inherits nothing. */
  | { readonly kind: "gone" }
  /** The write's fate is unknown: keep it, so Retry, or a press of the same request, sends it under its key. */
  | { readonly kind: "lost"; readonly kept: Kept }
  /** The write's fate is known: forget the lost one. */
  | { readonly kind: "found" }
  /** Open what landed: the reader is where they pressed it. */
  | { readonly kind: "open"; readonly route: string; readonly state: unknown }

/** A write kept after it ended with its fate unknown. `unanswered`: an attempt may have landed, so the control says No answer
 * and a press of the same request goes out unsure; otherwise none reached the daemon, as a refusal in front says, or no read
 * can show what it did, as of a Test, and it is kept for its toast's Retry alone. */
export type Kept = { readonly write: Write<unknown>; readonly unanswered: boolean }

export type Step<B> = { readonly next: Pending<B> | null; readonly effects: readonly Effect[] }

const same = <B>(record: Pending<B> | null): Step<B> => ({ next: record, effects: [] })

function revisionOf(data: ReadData | undefined): number | null {
  return data === undefined ? null : data.revision
}

/** Whether `record` settles on `reads`. Waiting: the read's revision has reached the landmark. Unanswered: a read that
 * began after `since` has landed. Nothing else settles a control. */
export function settled<B>(record: Pending<B>, reads: Reads): boolean {
  const phase = record.phase
  if (phase.kind === "waiting") {
    const revision = revisionOf(reads[phase.landmark.read].data)
    return revision !== null && revision >= phase.landmark.revision
  }
  if (phase.kind === "unanswered" && record.action.settles !== "none") {
    const began = reads[record.action.settles].landedBeganAt
    return began !== null && began > phase.since
  }
  return false
}

/** The item is gone, and what the control holds for it goes before the toast says so. */
const GONE: Effect[] = [{ kind: "gone" }, { kind: "say", ending: { kind: "gone" } }]

/** The ending a waiting record says when its landmark passes. A refusal the form drew while the view caught up is the toast's
 * once the form no longer holds what was refused, unless the toast said it already. */
function arrival<B>(record: Pending<B>, then: Then, reads: Reads): Effect[] {
  if (then.kind === "refused") {
    if (record.how.shows?.(then.refusal)) return []
    return [{ kind: "refusal", refusal: null }, ...(then.said ? [] : [{ kind: "say", ending: { kind: "refused", refusal: then.refusal } } as const])]
  }
  if (then.kind === "gone") return GONE
  if (then.kind === "conflict") {
    const data = record.action.settles === "none" ? undefined : reads[record.action.settles].data
    const conflicted = data === undefined ? undefined : record.how.conflict?.(data)
    if (conflicted?.kind === "agreed") return [{ kind: "landed", edits: record.write.edits }]
    if (conflicted === null) return GONE
    return [{ kind: "say", ending: { kind: "conflict", conflicted, resent: then.resent } }]
  }
  const made = record.write.made
  const data = record.action.settles === "none" ? undefined : reads[record.action.settles].data
  const landed = landing(record.write, record.how, made === null || (data !== undefined && record.action.creates?.listed(data).some((item) => item.id === made) === true))
  return then.failed === null ? landed : [...landed, { kind: "say", ending: { kind: "failed", failed: then.failed } }]
}

/** A landing: the draft's edits its bytes held go, and what it made is opened, or its toast says where it is, while the view
 * `lists` it. One another writer deleted before the view showed it opens nothing. A delete's landing is its item's end. */
function landing<B>(write: Write<B>, how: PressHow<B>, lists: boolean): Effect[] {
  const opened = lists ? how.opens?.(write.made) : undefined
  const after: Effect[] = opened === undefined ? [] : opened.here ? [{ kind: "open", route: opened.route, state: opened.state }] : [{ kind: "say", ending: { kind: "landed", route: opened.route } }]
  return [{ kind: "landed", edits: write.edits }, ...(write.method === "DELETE" ? [{ kind: "gone" } as const] : []), ...after]
}

/** A lost create whose id the read lists landed: its toast and its Retry go, and its landing is a yes's. */
export const listed = <B>(write: Write<B>, how: PressHow<B>): Effect[] => [{ kind: "found" }, { kind: "dismiss" }, ...landing(write, how, true)]

/** Waits for `landmark`, or settles at once when the page already holds a view at or past it. */
function wait<B>(record: Pending<B>, landmark: Landmark, then: Then, reads: Reads, at: Seq): Step<B> {
  const next: Pending<B> = { ...record, phase: { kind: "waiting", landmark, then, since: at } }
  if (settled(next, reads)) return { next: null, effects: [{ kind: "stop" }, { kind: "found" }, ...arrival(next, then, reads), { kind: "refetch", read: landmark.read }] }
  return { next, effects: [{ kind: "stop" }, { kind: "found" }, { kind: "refetch", read: landmark.read }] }
}

/** Ends a write now: the daemon stored nothing under its key, or no attempt can have reached it. */
function refuse<B>(record: Pending<B>, ending: Ending, lost: boolean): Step<B> {
  const refetch: Effect[] = record.action.settles === "none" ? [] : [{ kind: "refetch", read: record.action.settles }]
  const drawn = ending.kind === "refused" && record.how.shows?.(ending.refusal) === true
  const shown: Effect[] = drawn && ending.kind === "refused" ? [{ kind: "refusal", refusal: ending.refusal }, ...said(record, ending)] : [{ kind: "say", ending }]
  return { next: null, effects: [{ kind: "stop" }, lost ? { kind: "lost", kept: { write: record.write, unanswered: false } } : { kind: "found" }, ...shown, ...refetch] }
}

/** A refusal the form draws is also said when it comes while the form is closed, so the reader hears of it where they are. */
const said = <B>(record: Pending<B>, ending: Ending): Effect[] => (record.how.away?.() === true ? [{ kind: "say", ending }] : [])

/** Ends a write whose fate is unknown: the toast now, the control on a read that begins after this. */
function abandon<B>(record: Pending<B>, ending: Ending, at: Seq): Step<B> {
  if (record.action.settles === "none") return refuse(record, ending, true)
  return {
    next: { ...record, phase: { kind: "unanswered", since: at } },
    effects: [{ kind: "stop" }, { kind: "lost", kept: { write: record.write, unanswered: true } }, { kind: "say", ending }, { kind: "refetch", read: record.action.settles }],
  }
}

/** Where a yes, a conflict or a daemon refusal of a replace is settled: the action's read at the daemon's number. */
function landmarkOf<B>(record: Pending<B>, answer: Extract<Answer, { revision: number }>): Landmark | null {
  if (record.action.settles === "none") return null
  if (record.action.settles === "state") return { read: "state", revision: answer.revision }
  if (answer.kind === "conflict") return { read: "settings", revision: answer.stored }
  const body = answer.kind === "yes" ? answer.body : null
  return typeof body === "object" && body !== null && "revision" in body && typeof body.revision === "number" ? { read: "settings", revision: body.revision } : null
}

function answered<B>(record: Pending<B>, phase: Out, event: Extract<Event<B>, { kind: "answer" }>, reads: Reads): Step<B> {
  const answer = event.answer
  switch (answer.kind) {
    case "yes": {
      const landmark = landmarkOf(record, answer)
      const keep: Effect = { kind: "keep", answer: { by: answerer(record.action), body: answer.body } }
      const then: Then = { kind: "answered", failed: record.action.failed?.(answer.body) ?? null }
      if (landmark === null) return { next: null, effects: [{ kind: "stop" }, { kind: "found" }, keep, ...arrival(record, then, reads)] }
      const waiting = wait(record, landmark, then, reads, event.at)
      return { ...waiting, effects: [keep, ...waiting.effects] }
    }
    case "conflict": {
      const landmark = landmarkOf(record, answer)
      if (landmark === null) return refuse(record, { kind: "conflict", conflicted: undefined, resent: phase.unsure }, false)
      /* Only this page knew a created id, and it sends one request to each, so something stored there is this create, landed by an
       * earlier attempt. */
      return wait(record, landmark, record.write.precondition?.kind === "create" ? { kind: "answered", failed: null } : { kind: "conflict", resent: phase.unsure }, reads, event.at)
    }
    case "refused": {
      const landmark = landmarkOf(record, answer)
      /* A replay would have answered this key's own delete, so a 404 to a replace or a delete is another writer's. */
      const removable = record.write.precondition?.kind === "replace" || record.write.method === "DELETE"
      if (answer.status === 404 && removable && landmark !== null) return wait(record, landmark, { kind: "gone" }, reads, event.at)
      const refusal: Refusal = { code: answer.code, reason: answer.reason, detail: answer.detail, message: answer.message, status: answer.status, revision: answer.revision, field: record.how.about?.(answer.reason) ?? null }
      /* The control draws its refusal over the view, which must reach the revision the daemon refused at to show what it names. */
      if (record.how.shows?.(refusal) && landmark !== null) {
        const told = said(record, { kind: "refused", refusal })
        const waiting = wait(record, landmark, { kind: "refused", refusal, said: told.length > 0 }, reads, event.at)
        return { ...waiting, effects: [{ kind: "refusal", refusal }, ...told, ...waiting.effects] }
      }
      return refuse(record, { kind: "refused", refusal }, false)
    }
    case "token":
      if (event.attempt === 1) {
        if (phase.attempts > 1) return { next: { ...record, phase: { ...phase, front: phase.front + 1 } }, effects: [] }
        return { next: { ...record, phase: { ...phase, attempts: 2, front: phase.front + 1 } }, effects: [{ kind: "send", attempt: 2, heal: true }] }
      }
      return front(record, phase, { kind: "unifi", status: answer.status }, event.at)
    case "front":
      return front(record, phase, { kind: "front", status: answer.status, retry: answer.retry }, event.at)
    case "unknown":
      return same(record)
  }
}

/** A refusal from in front speaks for its own attempt alone. Settled now when no other attempt can have landed. */
function front<B>(record: Pending<B>, phase: Out, ending: Ending, at: Seq): Step<B> {
  const mayHaveLanded = phase.unsure || phase.attempts > phase.front + 1
  if (mayHaveLanded) return abandon(record, ending, at)
  return refuse(record, ending, ending.kind === "front" && ending.retry)
}

/** The one place a write changes state. */
export function step<B>(record: Pending<B> | null, event: Event<B>, reads: Reads): Step<B> {
  switch (event.kind) {
    case "press": {
      if (record !== null && record.phase.kind !== "unanswered") return same(record)
      const next: Pending<B> = { write: event.write, action: event.action, how: event.how, startedAt: event.at, slow: false, phase: { kind: "out", attempts: 1, front: 0, unsure: event.unsure } }
      return { next, effects: [{ kind: "dismiss" }, { kind: "send", attempt: 1, heal: false }, { kind: "schedule" }] }
    }
    case "answer":
      if (record === null || record.startedAt !== event.of || record.phase.kind !== "out") return same(record)
      return answered(record, record.phase, event, reads)
    case "timer": {
      if (record === null || record.startedAt !== event.of || record.phase.kind !== "out") return same(record)
      if (event.n < 6) {
        const attempt = record.phase.attempts + 1
        return { next: { ...record, phase: { ...record.phase, attempts: attempt } }, effects: [{ kind: "send", attempt, heal: true }] }
      }
      return abandon(record, { kind: "noAnswer" }, event.at)
    }
    case "slow":
      if (record === null || record.startedAt !== event.of || record.slow) return same(record)
      return { next: { ...record, slow: true }, effects: [] }
    case "read": {
      if (record === null || !settled(record, reads)) return same(record)
      const phase = record.phase
      return { next: null, effects: phase.kind === "waiting" ? arrival(record, phase.then, reads) : [] }
    }
  }
}

// ───── asking a read again ─────

/** One read's fetches as the store hears them. `running`: the Seq the fetch now out began at. `ended`: the Seq the last fetch to
 * end began at. `asked`: the store asked for the read and no fetch has begun or ended since, so the ask joined the fetch out or
 * its own is still to begin. */
export type Flight = { readonly running: Seq | null; readonly ended: Seq | null; readonly asked: boolean }
export const NO_FLIGHT: Flight = { running: null, ended: null, asked: false }

/** The one place a read's flight moves: a fetch began at `at`, a fetch ended with data or none, or the store asked. */
export function flown(flight: Flight, event: { readonly kind: "began"; readonly at: Seq } | { readonly kind: "ended" } | { readonly kind: "asked" }): Flight {
  switch (event.kind) {
    case "began":
      return { running: event.at, ended: flight.ended, asked: false }
    case "ended":
      return { running: null, ended: flight.running ?? flight.ended, asked: false }
    case "asked":
      return { ...flight, asked: true }
  }
}

/** Asking a read again until the writes waiting on it settle. `boundary` is the latest stamp one of them waits for a read to
 * begin after; `behind` counts the reads begun after it that ended and settled nothing. */
export type Chase =
  /** No write waits on the read. */
  | { readonly kind: "idle" }
  /** A read is asked whenever none begun after the boundary is out and no ask is joined to one still out. */
  | { readonly kind: "chasing"; readonly boundary: Seq; readonly behind: number }
  /** A read begun after the boundary ended behind it: the next is asked once its wait is over, REREAD_MS doubled for each read
   * behind before it, up to READ_TIMEOUT_MS, so a view that stays behind is read at most once per wait. */
  | { readonly kind: "backingOff"; readonly boundary: Seq; readonly behind: number }

export const IDLE: Chase = { kind: "idle" }

export type ChaseEffect = { readonly kind: "ask" } | { readonly kind: "wait"; readonly ms: number } | { readonly kind: "cancel" }

/** The one place a read's chase moves: on a look at the boundary the waiting writes give and at the read's flight, and `due`
 * when its wait is over. A changed boundary starts the chase afresh. */
export function chased(chase: Chase, boundary: Seq | null, flight: Flight, due: boolean): { readonly next: Chase; readonly effects: readonly ChaseEffect[] } {
  const cancel: ChaseEffect[] = []
  let next = chase
  if ((chase.kind === "idle" ? null : chase.boundary) !== boundary) {
    if (chase.kind === "backingOff") cancel.push({ kind: "cancel" })
    next = boundary === null ? IDLE : { kind: "chasing", boundary, behind: 0 }
  } else if (due && chase.kind === "backingOff") next = { kind: "chasing", boundary: chase.boundary, behind: chase.behind }
  if (next.kind !== "chasing") return { next, effects: cancel }
  if ((flight.running !== null && flight.running > next.boundary) || flight.asked) return { next, effects: cancel }
  if (due) return { next, effects: [...cancel, { kind: "ask" }] }
  if (flight.ended === null || flight.ended < next.boundary) return { next, effects: [...cancel, { kind: "ask" }] }
  return { next: { ...next, kind: "backingOff", behind: next.behind + 1 }, effects: [...cancel, { kind: "wait", ms: Math.min(REREAD_MS * 2 ** next.behind, READ_TIMEOUT_MS) }] }
}

/** The read a record waits on and the stamp a read must begin after to settle it, or null while it waits on none. */
export function waitsOn<B>(record: Pending<B>): { readonly read: ReadName; readonly since: Seq } | null {
  const phase = record.phase
  if (phase.kind === "out" || record.action.settles === "none") return null
  return { read: record.action.settles, since: phase.since }
}

/** The write a press makes. A press of the same request as the control's lost write sends that write again under its
 * key, so a landed write is found and not made twice; anything else is a new write under a new key. A create's request is its
 * bytes: one the same as a lost create's goes to that create's id. */
export function writeOf<B>(action: Action<B, unknown>, pressed: B, precondition: Precondition | null, edits: Edits, lost: Write<unknown> | undefined, newId: () => string): Write<B> {
  const body = action.body(pressed)
  const bytes = body === undefined ? null : JSON.stringify(body)
  const made = action.creates ? (lost !== undefined && lost.made !== null && sameRequest(lost, { method: action.method, path: lost.path, bytes, precondition }) ? lost.made : newId()) : null
  const request = { method: action.method, path: made === null ? action.to?.(pressed) ?? action.path : `${action.path}/${made}`, bytes, precondition }
  return { ...request, made, key: lost !== undefined && sameRequest(lost, request) ? lost.key : newId(), pressed, edits }
}

type Request = Pick<Write<unknown>, "method" | "path" | "bytes" | "precondition">

/** Whether two writes are the same request as sent: method, path, precondition and bytes. The only equality the path has. */
export function sameRequest(a: Request, b: Request): boolean {
  const pre = (p: Precondition | null) => (p === null ? "" : p.kind === "create" ? "*" : String(p.revision))
  return a.method === b.method && a.path === b.path && a.bytes === b.bytes && pre(a.precondition) === pre(b.precondition)
}

// ───── a form's draft ─────

/** What is stored now, read from the page's view. Null while nothing is: an add, or an item deleted elsewhere. */
export type Stored<D> = { readonly value: D; readonly revision: number }

/** A form's draft: the fields the reader touched, each with the Seq of its last edit, over the stored value. `since` is the Seq of
 * the edit that began it: a write pressed from it holds an edit no older, one pressed from a draft dropped before holds none.
 * `way` is its form's, whose refusal its edits and its end clear. `creation` is the making of the item it was begun over. */
export type Draft = { readonly touched: ReadonlyMap<string, { readonly value: unknown; readonly at: Seq }>; readonly since: Seq; readonly way: string | null; readonly creation: Made }

/** Where a form's draft is, which names who may end it. A draft no form has opened has no place, and nothing here
 * ends it. Each fact is fixed when it happens, so no later look at the store decides a draft. */
export type Place =
  /** In `forms` open forms. The reader saves, discards or edits it. */
  | { readonly kind: "open"; readonly forms: number }
  /** Its last form closed at `at` with no write out. It ends when that task is over, unless a form is back in it, as
   * StrictMode's rerun of an effect brings it back. */
  | { readonly kind: "leaving"; readonly at: Seq }
  /** Its last form closed while its write was out, or a toast's Retry sent its write again while it was kept or leaving.
   * Only that write's ending decides it. */
  | { readonly kind: "held" }
  /** Its write ended in a refusal, a conflict or no answer while its form was closed, or its form closed while that form's write
   * had no answer. It waits for its form, so the reader finds their typing where the toast points, or for the toast's Retry to
   * send its write again. */
  | { readonly kind: "kept" }

export type PlaceEvent =
  | { readonly kind: "open" }
  /** A form closed. `owes`: the control's write was out or waiting ("out"), or the form's way keeps its write unanswered ("kept"). */
  | { readonly kind: "close"; readonly owes: "out" | "kept" | null; readonly at: Seq }
  /** The task of the close at `of` is over. */
  | { readonly kind: "over"; readonly of: Seq }
  /** The draft's write went out. */
  | { readonly kind: "sent" }
  /** The draft's write ended. `rest`: a draft is left that this ending didn't save. */
  | { readonly kind: "ended"; readonly rest: boolean }

/** The one place a draft's fate moves. Null: it has no place, and a draft that had one ends with it, its refusal too. */
export function moved(place: Place | null, event: PlaceEvent): Place | null {
  switch (event.kind) {
    case "open":
      return { kind: "open", forms: place?.kind === "open" ? place.forms + 1 : 1 }
    case "close":
      if (place?.kind !== "open") return place
      if (place.forms > 1) return { kind: "open", forms: place.forms - 1 }
      if (event.owes === "out") return { kind: "held" }
      return event.owes === "kept" ? { kind: "kept" } : { kind: "leaving", at: event.at }
    case "over":
      return place?.kind === "leaving" && place.at === event.of ? null : place
    case "sent":
      return place?.kind === "kept" || place?.kind === "leaving" ? { kind: "held" } : place
    case "ended":
      if (place?.kind !== "held") return place
      return event.rest ? { kind: "kept" } : null
  }
}

/** An untouched field reads what is stored, so when a conflict's landmark passes, the fields the reader never touched
 * take the stored values by derivation. */
export function formShown<D extends object>(blank: D, stored: Stored<D> | null, draft: Draft | undefined): D {
  const shown: Record<string, unknown> = { ...(stored?.value ?? blank) as Record<string, unknown> }
  for (const [field, { value }] of draft?.touched ?? []) shown[field] = value
  return shown as D
}

/** Two field values alike in every part, whatever order their keys were written in. */
export function equal(a: unknown, b: unknown): boolean {
  if (a === b) return true
  if (typeof a !== "object" || typeof b !== "object" || a === null || b === null || Array.isArray(a) !== Array.isArray(b)) return false
  const [x, y] = [a as Record<string, unknown>, b as Record<string, unknown>]
  const keys = Object.keys(x)
  return keys.length === Object.keys(y).length && keys.every((k) => Object.hasOwn(y, k) && equal(x[k], y[k]))
}

/** The touched fields that differ from what is stored, or from the blank form when nothing is. */
export function formChanged<D extends object>(blank: D, stored: Stored<D> | null, draft: Draft | undefined): ReadonlySet<keyof D> {
  const base = (stored?.value ?? blank) as Record<string, unknown>
  return new Set([...(draft?.touched ?? [])].filter(([field, { value }]) => !equal(value, base[field])).map(([field]) => field as keyof D))
}

export const formDirty = <D extends object>(blank: D, stored: Stored<D> | null, draft: Draft | undefined): boolean => formChanged(blank, stored, draft).size > 0

/** The form's conflict words: the touched fields whose stored value moved under the reader, as stored now. A masked
 * field prints as null and is left out. Agreed when every touched field as sent is what is stored now. Null when the item
 * is gone. */
export function formConflict<D extends object>(before: Stored<D> | null, after: Stored<D> | null, sent: D, touched: Iterable<string>, print: (field: keyof D, value: D[keyof D]) => string | null): Conflicted<never> | null {
  if (after === null) return null
  const was = (before?.value ?? {}) as Record<string, unknown>
  const now = after.value as Record<string, unknown>
  const mine = sent as Record<string, unknown>
  const fields = [...touched]
  if (fields.every((f) => equal(mine[f], now[f]))) return { kind: "agreed" }
  const words = fields.filter((f) => !equal(was[f], now[f])).map((f) => print(f as keyof D, now[f] as D[keyof D])).filter((w): w is string => w !== null)
  return { kind: "form", stored: words.length === 0 ? null : words.join(" · ") }
}

// ───── the store, as use-write.ts runs it ─────

/** What a read did, as the store hears it: a fetch began (or a paused one went on), a fetch ended with no data (it
 * failed, or was cancelled back), a fetch brought data, or the page put data there itself, which brings nothing new
 * from the daemon. */
export type ReadEvent =
  | { readonly read: ReadName; readonly kind: "began" }
  | { readonly read: ReadName; readonly kind: "ended" }
  | { readonly read: ReadName; readonly kind: "landed" | "set"; readonly data: ReadData | undefined }

export type ReadSource = {
  /** What the page holds before any event. */
  readonly current: (read: ReadName) => ReadData | undefined
  readonly subscribe: (listener: (event: ReadEvent) => void) => () => void
  /** Asks a fresh read. With nothing cached, a fetch already out may be joined instead. */
  readonly refetch: (read: ReadName) => void
}

/** What the store needs from outside, so a test runs it with a model daemon, a model cache and mock timers. */
export type WritesIo = {
  readonly attempt: (write: Write<unknown>, answer: Action<unknown, unknown>["answer"], heal: boolean, signal: AbortSignal) => Promise<Answer>
  readonly reads: ReadSource
  readonly say: (toast: Toast, act: () => void) => void
  readonly dismiss: (id: ControlKey) => void
  readonly signIn: () => void
  /** Goes to a route, with the history state it opens with. */
  readonly navigate: (route: string, state?: unknown) => void
  /** A fresh Idempotency-Key, or a fresh id for a create. */
  readonly newId: () => string
}

/** Where a control's write is, as the control draws it. `out`: sent, or answered and waiting for its read. `unanswered`: it
 * ended with its fate unknown, and its record waits for a read begun after, while a press (Retry) is taken again. `lost`:
 * settled with its fate unknown, and a press of the same request resends it under its key. `idle`: none of these, though a
 * write refused in front may be kept for its toast's Retry. */
export type Phase = "idle" | "out" | "unanswered" | "lost"

/** The control holds its write's record, out or unanswered: Pending, and a control takes no press of its own. */
export const isPending = (phase: Phase) => phase === "out" || phase === "unanswered"
/** Its last write's fate is unknown: the no-answer mark. */
export const noAnswer = (phase: Phase) => phase === "unanswered" || phase === "lost"

/** What a control shows. `sent` is the body of the write it holds while Pending, or null for a bodiless one. `way` is the
 * press's, while it is Pending. Phase "lost", `refusal`, `made` and `kept` are one way's: the way the reader names, or the way
 * that pressed last. */
export type Shown<B, R, W extends string = string> = {
  readonly phase: Phase
  readonly sent: B | null
  readonly way: W | null
  readonly slow: boolean
  readonly answer: R | null
  readonly refusal: Refusal | null
  /** How many of the control's writes have landed, so a control that keeps its own draft clears it on a landing and on
   * nothing else. */
  readonly landings: number
  /** How many of the control's landings left the draft they were pressed from with nothing to save over the view they settled on. */
  readonly saved: number
  /** The id the control's last create went to, out, settled or landed; null before its first. */
  readonly made: string | null
  /** What the control's lost write pressed, while it is kept: a press of the same request is that write. */
  readonly kept: B | null
}

/** A form: its fields over what is stored now, read from the action's own read. */
export type Form<D extends object, B, R, W extends string = string> = {
  readonly action: Action<B, R, W>
  /** Which way into its control the form is, where the control has more than one: only it spins for the form's Save, and the
   * form draws only the refusals of its own presses. One of the action's own ways, never a word that widens them. */
  readonly way?: NoInfer<W>
  readonly what: string
  /** What a form with nothing stored starts from. */
  readonly blank: D
  /** What is stored now, from the action's read, and for a create at `made`, the id its last press went to. Null: nothing is, so
   * a press creates. */
  readonly stored: (data: ReadData | undefined, made: string | null) => Stored<D> | null
  /** The body a shown draft sends, made at the press. */
  readonly body: (shown: D, stored: Stored<D> | null) => B
  /** A field's words for the conflict toast, or null for a masked field. */
  readonly print: (field: keyof D, value: D[keyof D]) => string | null
  /** Which of the daemon's refusals the form draws under its field, given the fields the refused write changed. The rest
   * are toasts. */
  readonly shows: (refusal: Refusal, changed: ReadonlySet<keyof D>) => boolean
  /** The field a refusal is about, from its reason and the fields the refused press changed: a refusal that a toast's Retry gets
   * still stands under the form while that field says what the Retry sent. Undefined: every field. */
  readonly fieldOf?: (reason: string, changed: ReadonlySet<keyof D>) => keyof D | undefined
  /** The why of a conflict met by its write sent again under its key after no answer. Absent: "changed in another tab". */
  readonly resentWhy?: string
}

export type FormView<D, W extends string = string> = {
  readonly shown: D
  readonly touched: ReadonlySet<keyof D>
  /** The touched fields that differ from what is stored: where a refusal of a write of them stands. */
  readonly changed: ReadonlySet<keyof D>
  readonly dirty: boolean
  /** The control's write has attempts out, unanswered. Waiting for the view is not sending. */
  readonly sending: boolean
  /** The form's way keeps its write unanswered, and the control's write isn't out. A definite 429 is not this. */
  readonly unheard: boolean
  /** `unheard`, and the draft holds no edit since that write was pressed: a Save now is its Retry. */
  readonly retries: boolean
  /** `out`: the form is read-only and Save takes no press. Unanswered or lost: the 30 s mark under the field, and Save is
   * taken again, a press of the same request resending it. */
  readonly phase: Phase
  /** The way that pressed the write out, so only it spins; null while none is. */
  readonly way: W | null
  /** For a create, the id its last press went to. */
  readonly made: string | null
  readonly refusal: Refusal | null
}

export type Writes = {
  readonly subscribe: (listener: () => void) => () => void
  /** Moves on every change the store sees, its reads included. */
  readonly version: () => number
  /** The same object until the control changes, for useSyncExternalStore. Its answer is whichever action answered last. `way`
   * names whose lost write, refusal and create it shows; undefined, the way that pressed last. */
  readonly shown: (control: ControlKey, way?: string | null) => Shown<unknown, Answered>
  /** Gives the key of the write it sends, or null when ignored, as it is while the control's write is out or answered. After one
   * ended unanswered, the same request resends it under its key. The press's way is one of the action's own. */
  readonly press: <B, W extends string>(action: Action<B, unknown, W>, pressed: B, how: PressHow<B, NoInfer<W>>) => string | null
  readonly form: <D extends object, B, R, W extends string>(form: Form<D, B, R, W>) => FormView<D, W>
  readonly pressForm: <D extends object, B, R, W extends string>(form: Form<D, B, R, W>) => void
  /** An edit of the form that is `way` into the action's control. */
  readonly edit: <B, W extends string>(action: Action<B, unknown, W>, way: NoInfer<W> | null, patch: Record<string, unknown>) => void
  /** Whether a reload of the page would lose something of the reader's: a draft that differs from what is stored, a write out or
   * kept for its Retry, or typing a component holds. */
  readonly unsaved: () => boolean
  /** Typing a component holds outside a form, until the release. */
  readonly hold: () => () => void
  /** Drops the draft and `way`'s refusal and its write that got no answer, so its Retry sends nothing, and that write's toast
   * when `way` pressed last. */
  readonly discard: (control: ControlKey, way: string | null) => void
  /** A form that is `way` into the control opened; the release closes it. Its draft's place, and `moved`, say what that does to
   * the draft. */
  readonly mount: (control: ControlKey, way: string | null) => () => void
  /** What a toast's button does; the toast holds only the Act. */
  readonly act: (control: ControlKey, act: Act) => void
  /** Sends `way`'s write that ended with no answer again under its key, as its toast's Retry does. */
  readonly retry: (control: ControlKey, way: string | null) => void
}

// ───── the toast ─────

/** What a toast's button does. The store performs it; the toast only names it. */
export type Act =
  /** Send the same write under the same key. */
  | { readonly kind: "retry"; readonly write: Write<unknown> }
  /** Send the reader's change rebuilt over what is stored now, under a new key. */
  | { readonly kind: "useMine"; readonly pressed: unknown; readonly precondition: Precondition }
  | { readonly kind: "signIn" }
  | { readonly kind: "open"; readonly route: string }

/** Rule 2's one toast. `mark` carries the colour. `why` is fixed per ending, or the words of the daemon's reason for its own refusal.
 * `muted` is a second line that wraps and is never cut. The button's word is short and fixed, and no name goes in it.
 * A toast with a button, or a form's conflict, stays until pressed or dismissed. */
export type Toast = {
  readonly id: ControlKey
  readonly mark: "danger" | "ok"
  readonly what: string
  readonly why: string | null
  readonly muted: string | null
  readonly button: { readonly word: "Retry" | "Sign in" | "Keep mine" | "Open"; readonly act: Act } | null
  readonly stays: boolean
}

/** The toast for an ending of `write`. Pure, so a table test reads every toast the page can say. */
export function toastOf(id: ControlKey, how: Pick<PressHow<unknown>, "what" | "refused" | "resentWhy">, ending: Ending, write: Write<unknown>): Toast {
  const what = how.what
  const danger = (why: string, button: Toast["button"] = null, muted: string | null = null, stays = button !== null, named = what): Toast => ({ id, mark: "danger", what: named, why, muted, button, stays })
  switch (ending.kind) {
    case "noAnswer":
      return danger("timed out", { word: "Retry", act: { kind: "retry", write } })
    case "front":
      return danger(`HTTP ${ending.status}`, ending.retry ? { word: "Retry", act: { kind: "retry", write } } : null)
    case "refused": {
      const about = how.refused?.(ending.refusal)
      const button: Toast["button"] = about?.open === undefined ? null : { word: "Open", act: { kind: "open", route: about.open } }
      return danger(toastWhy(refusalWords(ending.refusal)), button, null, button !== null, about?.what ?? what)
    }
    case "unifi":
      return danger(ending.status === 401 ? "session expired" : "UniFi refused", { word: "Sign in", act: { kind: "signIn" } })
    case "gone":
      return danger("was deleted")
    case "conflict": {
      const c = ending.conflicted
      if (c === undefined) return danger("changed in another tab")
      if (c.kind === "form") return danger(ending.resent && how.resentWhy !== undefined ? how.resentWhy : "changed in another tab", null, c.stored, true)
      return danger(`changed to ${c.theirs}`, { word: "Keep mine", act: { kind: "useMine", pressed: c.again.pressed, precondition: c.again.precondition } }, `Your value: ${c.mine}`)
    }
    case "landed":
      return { id, mark: "ok", what, why: null, muted: null, button: { word: "Open", act: { kind: "open", route: ending.route } }, stays: true }
    case "failed":
      return danger(toastWhy(ending.failed.words), null, ending.failed.reason)
  }
}

/** What is said of an ending of a write to a making of its item the view lists no more, as `toast` would say it of the making it
 * was to. A no answer and a conflict say nothing: one offers to send again what the making's end let go, the other is about what
 * is at the id now. Every other answer is said once, of the making it was to; a refusal from in front leaves by itself, as its
 * Retry would send nothing. */
export function retiredToast(toast: Toast, ending: Ending): Toast | null {
  switch (ending.kind) {
    case "noAnswer":
    case "conflict":
      return null
    case "front":
      return { ...toast, button: null, stays: false }
    case "refused":
    case "unifi":
    case "gone":
    case "landed":
    case "failed":
      return toast
  }
}
