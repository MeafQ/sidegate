/* The page's one store of writes: one record per control for as long as the page is open, its timers and attempts,
 * what it reads of the query cache, and the two hooks components use. The machine it runs is write.ts's `step`. */
import { useEffect, useSyncExternalStore } from "react"
import { QueryObserver, useQueryClient, type QueryClient } from "@tanstack/react-query"
import { attempt, newId, signIn } from "@/lib/api"
import { dismiss as dismissToast, say as sayToast } from "@/components/toast"
import { keys } from "@/lib/keys"
import { settingsRead, stateRead } from "@/lib/queries"
import {
  ANSWER_MS, END_MS, IDLE, NO_EDITS, NO_FLIGHT, SCHEDULE_MS, SLOW_MS, answerOf, chased, flown, equal, formChanged, formConflict, formDirty, formShown, listed, moved, retiredToast, step, toastOf, waitsOn, writeOf,
  type Act, type Action, type Chase, type Flight, type Answer, type Answered, type ControlKey, type Draft, type Edits, type Effect, type Event, type Form, type FormView, type Made, type Pending,
  type Phase, type Place, type PlaceEvent, type PressHow, type ReadData, type ReadName, type Reads, type ReadSource, type Refusal, type Seq, type Shown, type Stored, type Writes,
  type WritesIo, type Write,
} from "@/lib/write"

export { isPending, noAnswer } from "@/lib/write"

/** The write is out, or answered and waiting for its read: its ending is still to come. */
const out = (record: Pending<unknown> | null | undefined) => record != null && record.phase.kind !== "unanswered"

/** A way into a control, as a press names it (`PressHow.way`, `Form.way`); null for a control with one way. */
type WayName = string | null

/** What one way into a control holds after its last accepted press. Only this way's next press replaces it, and only the endings
 * of its own write move `kept` and `refusal`: a press, answer or ending of another way into the control never writes it. */
type Last = {
  readonly action: Action<unknown, unknown>
  readonly how: PressHow<unknown>
  readonly write: Write<unknown>
  /** The making of the item `write` is to, as the view listed it at the write's first press. */
  readonly creation: Made
  /** `write`, ended with its fate unknown and kept for Retry and for a press of the same request. `unanswered`: an attempt may
   * have landed. Null while it is out, or once its fate is known. */
  readonly kept: { readonly unanswered: boolean } | null
  /** The daemon's refusal of `write` that this way draws, until its next press, its draft's next edit or end, or a discard. */
  readonly refusal: Refusal | null
}

/** A control's ways: each one's Last, and the way that pressed last, whose toast is the one on screen. */
type Ways = { latest: WayName; readonly last: Map<WayName, Last> }

const wayOf = (how: PressHow<unknown>): WayName => how.way ?? null

/** What an effect is run for: a write, its action and its press. */
type Subject = Pick<Pending<unknown>, "write" | "action" | "how" | "startedAt">

export function startWrites(io: WritesIo): Writes {
  let seq = 0
  const stamp = () => ++seq as Seq
  const records = new Map<ControlKey, Pending<unknown>>()
  const ways = new Map<ControlKey, Ways>()
  const answers = new Map<ControlKey, Answered>()
  const landings = new Map<ControlKey, number>()
  const saved = new Map<ControlKey, number>()
  const drafts = new Map<ControlKey, Draft>()
  /** Each control's form, as last drawn: what its draft is of. */
  const forms = new Map<ControlKey, Form<object, unknown, unknown>>()
  /** How many components hold typing of their own. */
  let held = 0
  /** Each control whose actions mark which making of their item the view lists, by the action that marks it. */
  const markers = new Map<ControlKey, Action<unknown, unknown>>()
  /** Each control whose toast on screen waits for a press about the making its write was to, which that making's end takes. */
  const staying = new Set<ControlKey>()
  const places = new Map<ControlKey, Place>()
  const timers = new Map<ControlKey, ReturnType<typeof setTimeout>[]>()
  const attempts = new Map<ControlKey, AbortController[]>()
  /** Each control's Shown per way asked for, undefined for the way that pressed last, until the control changes. */
  const shownOf = new Map<ControlKey, Map<WayName | undefined, Shown<unknown, Answered>>>()
  const listeners = new Set<() => void>()
  let version = 0

  const flights: Record<ReadName, Flight> = { state: NO_FLIGHT, settings: NO_FLIGHT }
  const chases: Record<ReadName, Chase> = { state: IDLE, settings: IDLE }
  /** Each read's wait while its chase is backing off. */
  const waits: Record<ReadName, ReturnType<typeof setTimeout> | null> = { state: null, settings: null }
  let reads: Reads = { state: { data: io.reads.current("state") as Reads["state"]["data"], landedBeganAt: null }, settings: { data: io.reads.current("settings") as Reads["settings"]["data"], landedBeganAt: null } }

  const lastOf = (control: ControlKey, way: WayName | undefined): Last | undefined => {
    const all = ways.get(control)
    return all?.last.get(way === undefined ? all.latest : way)
  }
  const keepOn = (control: ControlKey, way: WayName, change: (last: Last) => Last) => {
    const last = ways.get(control)?.last.get(way)
    if (last) ways.get(control)?.last.set(way, change(last))
  }

  function changed(control: ControlKey | null) {
    if (control !== null) shownOf.delete(control)
    version++
    for (const listener of listeners) listener()
  }

  function shown(control: ControlKey, way?: WayName): Shown<unknown, Answered> {
    const cached = shownOf.get(control)?.get(way)
    if (cached) return cached
    const record = records.get(control)
    const last = lastOf(control, way)
    const phase: Phase = record ? (record.phase.kind === "unanswered" ? "unanswered" : "out") : last?.kept?.unanswered ? "lost" : "idle"
    const next: Shown<unknown, Answered> = {
      phase, sent: record?.write.pressed ?? null, way: record?.how.way ?? null, slow: record?.slow ?? false,
      answer: answers.get(control) ?? null, refusal: last?.refusal ?? null, landings: landings.get(control) ?? 0, saved: saved.get(control) ?? 0,
      made: last?.write.made ?? null, kept: last?.kept ? last.write.pressed : null,
    }
    shownOf.set(control, (shownOf.get(control) ?? new Map()).set(way, next))
    return next
  }

  function stop(control: ControlKey) {
    for (const timer of timers.get(control) ?? []) clearTimeout(timer)
    for (const controller of attempts.get(control) ?? []) controller.abort()
    timers.delete(control)
    attempts.delete(control)
  }

  function run(control: ControlKey, subject: Subject, effect: Effect) {
    const way = wayOf(subject.how)
    switch (effect.kind) {
      case "send": {
        const controller = new AbortController()
        attempts.set(control, [...(attempts.get(control) ?? []), controller])
        const of = subject.startedAt
        /* An answer after the record left `out`, or to an earlier press, is dropped by the machine: its stamp and phase. */
        io.attempt(subject.write, subject.action.answer, effect.heal, controller.signal)
          .catch((): Answer => ({ kind: "unknown", status: 0 }))
          .then((answer) => apply(control, { kind: "answer", of, attempt: effect.attempt, answer, at: stamp() }))
        return
      }
      case "schedule": {
        const of = subject.startedAt
        const at = (ms: number, event: () => Event<unknown>) => setTimeout(() => apply(control, event()), ms)
        /* Not stopped with the schedule: a write answered and waiting for its read is still Pending. The machine drops it for
         * a record that has gone, by its stamp. */
        at(SLOW_MS, () => ({ kind: "slow", of }))
        timers.set(control, [
          ...SCHEDULE_MS.map((ms, i) => at(ms, () => ({ kind: "timer", of, n: i + 1, at: stamp() }))),
          at(END_MS, () => ({ kind: "timer", of, n: 6, at: stamp() })),
        ])
        return
      }
      case "stop":
        return stop(control)
      case "refetch":
        return ask(effect.read)
      case "say": {
        const creation = ways.get(control)?.last.get(way)?.creation
        const retired = creation !== undefined && earlier(control, creation)
        const told = toastOf(control, subject.how, effect.ending, subject.write)
        const toast = retired ? retiredToast(told, effect.ending) : told
        if (toast === null) return
        const button = toast.button
        if (toast.stays && !retired) staying.add(control)
        else staying.delete(control)
        return io.say(toast, () => { if (button) act(control, button.act) })
      }
      case "dismiss":
        keepOn(control, way, (last) => ({ ...last, refusal: null }))
        return dismiss(control)
      case "keep": {
        const answer = effect.answer
        answers.set(control, answer)
        setTimeout(() => {
          if (answers.get(control) !== answer) return
          answers.delete(control)
          changed(control)
        }, ANSWER_MS)
        return
      }
      case "refusal":
        keepOn(control, way, (last) => ({ ...last, refusal: effect.refusal }))
        return
      case "landed": {
        landings.set(control, (landings.get(control) ?? 0) + 1)
        const draft = drafts.get(control)
        if (!draft) return
        const unsent = [...draft.touched].filter(([field, edit]) => edit.at !== effect.edits.get(field))
        if (unsent.length === 0) drafts.delete(control)
        else drafts.set(control, { ...draft, touched: new Map(unsent) })
        /* Pressed from this draft, it ends the draft's work once nothing it holds differs from what is stored. */
        const fromIt = [...effect.edits.values()].some((at) => at >= draft.since)
        if (fromIt && (unsent.length === 0 || subject.how.clean?.() === true)) saved.set(control, (saved.get(control) ?? 0) + 1)
        return
      }
      case "gone":
        return retire(control, ways.get(control)?.last.get(way)?.creation ?? null)
      case "lost":
        keepOn(control, way, (last) => ({ ...last, kept: { unanswered: effect.kept.unanswered } }))
        return
      case "found":
        for (const [name, last] of ways.get(control)?.last ?? []) {
          if (last.write.key === subject.write.key) keepOn(control, name, (l) => ({ ...l, kept: null }))
        }
        return
      case "open":
        return io.navigate(effect.route, effect.state)
    }
  }

  /** A lost create the read lists has landed: only this page knew its id. One with a write out waits for that write's ending. */
  function find(read: ReadName) {
    const data = reads[read].data
    if (data === undefined) return
    for (const [control, all] of [...ways]) {
      for (const last of [...all.last.values()]) {
        if (records.has(control) || last.kept === null || last.action.settles !== read) continue
        if (!last.action.creates?.listed(data).some((item) => item.id === last.write.made)) continue
        for (const effect of listed(last.write, last.how)) run(control, { ...last, startedAt: stamp() }, effect)
        changed(control)
      }
    }
  }

  /** The making of `action`'s item its read lists now: null while it lists none, undefined while the read is unread or the action
   * marks no making. */
  function makingOf(action: Action<unknown, unknown>): Made | undefined {
    if (action.creation === undefined || action.settles === "none") return undefined
    const data = reads[action.settles].data
    return data === undefined ? undefined : action.creation(data)
  }

  /** The making a draft or a way's write that `action` begins now is for, the one its read lists. From then on the control is
   * judged by the action's mark. */
  function makingFor(action: Action<unknown, unknown>): Made {
    if (action.creation !== undefined) markers.set(action.control, action)
    return makingOf(action) ?? null
  }

  /** Whether `creation` is a making of the control's item before the one the view lists. `ended`: a write was answered that its
   * item is gone, and that making is before too, though the view can't tell it from the one it lists. */
  function earlier(control: ControlKey, creation: Made, ended?: Made): boolean {
    const marker = markers.get(control)
    const now = marker && makingOf(marker)
    return creation === ended || (now !== undefined && creation !== now)
  }

  function dismiss(control: ControlKey) {
    staying.delete(control)
    io.dismiss(control)
  }

  /** The one place the store lets go of what a control holds for an earlier making of its item than the view lists: its draft,
   * each way's kept write and refusal, and their toast that waits for a press, so a Retry or a Save sends nothing of it to what
   * is at its id now. A toast that leaves by itself was said of the making it was about, and stays its time. What was begun on
   * the making the view lists stays. */
  function retire(control: ControlKey, ended?: Made) {
    const old = (creation: Made) => earlier(control, creation, ended)
    const all = ways.get(control)
    const dropped = [...(all?.last ?? [])].filter(([, last]) => old(last.creation)).map(([way]) => way)
    const draft = drafts.get(control)
    const stale = draft !== undefined && old(draft.creation)
    if (!stale && dropped.length === 0) return
    if (stale) drafts.delete(control)
    for (const way of dropped) all?.last.delete(way)
    if (all && dropped.includes(all.latest) && staying.has(control)) dismiss(control)
    if (all?.last.size === 0) ways.delete(control)
    changed(control)
  }

  /** A read retires what each control holds for a making the read lists no more. A control whose write is out keeps it until
   * that write's ending, which retires it then. */
  function outlived(read: ReadName) {
    for (const [control, marker] of [...markers]) {
      if (marker.settles === read && !out(records.get(control))) retire(control)
    }
  }

  /** Moves the draft's place; a draft whose place ends goes with its refusal. */
  function move(control: ControlKey, event: PlaceEvent) {
    const place = places.get(control) ?? null
    const next = moved(place, event)
    if (next !== null) {
      places.set(control, next)
      return
    }
    places.delete(control)
    if (place === null) return
    const draft = drafts.get(control)
    drafts.delete(control)
    if (draft) keepOn(control, draft.way, (last) => ({ ...last, refusal: null }))
    changed(control)
  }

  function apply(control: ControlKey, event: Event<unknown>) {
    const record = records.get(control) ?? null
    const { next, effects } = step(record, event, reads)
    if (next === null) records.delete(control)
    else records.set(control, next)
    const subject = next ?? record
    const savedBefore = saved.get(control)
    if (subject !== null) for (const effect of effects) run(control, subject, effect)
    if (!out(record) && out(next)) move(control, { kind: "sent" })
    if (out(record) && !out(next)) {
      retire(control)
      move(control, { kind: "ended", rest: drafts.has(control) && saved.get(control) === savedBefore })
    }
    if (next !== record || effects.length > 0) changed(control)
    if (subject !== null && subject.action.settles !== "none") chase(subject.action.settles)
  }

  function ask(read: ReadName) {
    flights[read] = flown(flights[read], { kind: "asked" })
    io.reads.refetch(read)
  }

  /** The latest stamp a read must begin after to settle a write waiting on `read`, or null when none waits. */
  function boundaryOf(read: ReadName): Seq | null {
    let latest: Seq | null = null
    for (const record of records.values()) {
      const waits = waitsOn(record)
      if (waits?.read === read && (latest === null || waits.since > latest)) latest = waits.since
    }
    return latest
  }

  /** Runs the read's chase, `due` when its wait is over, so that while a write waits on `read` a read begun after its boundary
   * is out or due, and never two at once. */
  function chase(read: ReadName, due = false) {
    const { next, effects } = chased(chases[read], boundaryOf(read), flights[read], due)
    chases[read] = next
    for (const effect of effects) {
      if (effect.kind === "ask") ask(read)
      else if (effect.kind === "wait") waits[read] = setTimeout(() => { waits[read] = null; chase(read, true) }, effect.ms)
      else {
        if (waits[read] !== null) clearTimeout(waits[read])
        waits[read] = null
      }
    }
  }

  io.reads.subscribe((event) => {
    const flight = flights[event.read]
    if (event.kind === "began") {
      flights[event.read] = flown(flight, { kind: "began", at: stamp() })
      return
    }
    if (event.kind === "set") {
      reads = { ...reads, [event.read]: { data: event.data, landedBeganAt: reads[event.read].landedBeganAt } }
    } else {
      flights[event.read] = flown(flight, { kind: "ended" })
      if (event.kind === "ended") return chase(event.read)
      reads = { ...reads, [event.read]: { data: event.data, landedBeganAt: flight.running } }
    }
    for (const [control, record] of [...records]) {
      if (record.action.settles === event.read) apply(control, { kind: "read", at: stamp() })
    }
    find(event.read)
    outlived(event.read)
    changed(null)
    chase(event.read)
  })

  /* A press of the same request as the write the control's last press kept goes under its key, as a landed write is then found,
   * whichever way pressed. Once another way has pressed, that way's write is the control's latest, and one kept before it is
   * sent again only by its own Retry. */
  function press<B>(action: Action<B, unknown>, pressed: B, how: PressHow<B>, edits: Edits = NO_EDITS): string | null {
    const record = records.get(action.control)
    if (record && record.phase.kind !== "unanswered") return null
    const latest = lastOf(action.control, undefined)
    const write = writeOf(action, pressed, how.precondition ?? null, edits, latest?.kept ? latest.write : undefined, io.newId)
    start(action, write, how, makingFor(action as Action<unknown, unknown>))
    return write.key
  }

  /** Sends `write`, for the item's making `creation`, as the control's one write; a write under a lost one's key may have landed
   * already. */
  function start<B>(action: Action<B, unknown>, write: Write<B>, how: PressHow<B>, creation: Made) {
    const control = action.control
    const all = ways.get(control) ?? { latest: null, last: new Map<WayName, Last>() }
    const unsure = [...all.last.values()].some((last) => last.kept?.unanswered === true && last.write.key === write.key)
    const way = wayOf(how as PressHow<unknown>)
    all.last.set(way, { action: action as Action<unknown, unknown>, how: how as PressHow<unknown>, write, creation, kept: null, refusal: null })
    all.latest = way
    ways.set(control, all)
    apply(control, { kind: "press", write, action: action as Action<unknown, unknown>, how: how as PressHow<unknown>, unsure, at: stamp() })
  }

  function act(control: ControlKey, act: Act) {
    switch (act.kind) {
      case "retry": {
        const record = records.get(control)
        if (record && record.phase.kind !== "unanswered") return
        /* Two ways can keep one write, as a press of another way's lost request does: the one on screen, the latest, sends it. */
        const latest = lastOf(control, undefined)
        const keeps = (last: Last | undefined) => last?.kept != null && last.write.key === act.write.key
        const holder = keeps(latest) ? latest : [...(ways.get(control)?.last.values() ?? [])].find(keeps)
        if (holder) start(holder.action, holder.write, holder.how, holder.creation)
        return
      }
      case "useMine": {
        const last = lastOf(control, undefined)
        if (last) press(last.action, act.pressed, { ...last.how, precondition: act.precondition })
        return
      }
      case "signIn":
        dismiss(control)
        return io.signIn()
      case "open":
        dismiss(control)
        return io.navigate(act.route)
    }
  }

  function storedOf<D extends object, B, R>(form: Form<D, B, R>, data?: ReadData): Stored<D> | null {
    return form.stored(data ?? (form.action.settles === "none" ? undefined : reads[form.action.settles].data), lastOf(form.action.control, form.way ?? null)?.write.made ?? null)
  }

  return {
    subscribe: (listener) => {
      listeners.add(listener)
      return () => { listeners.delete(listener) }
    },
    version: () => version,
    shown,
    press,
    act,
    retry: (control, way) => {
      const last = lastOf(control, way)
      if (last?.kept) act(control, { kind: "retry", write: last.write })
    },
    form: <D extends object, B, R, W extends string>(form: Form<D, B, R, W>): FormView<D, W> => {
      const control = form.action.control
      forms.set(control, form as unknown as Form<object, unknown, unknown>)
      const stored = storedOf(form)
      const draft = drafts.get(control)
      const now = shown(control, form.way ?? null)
      const changed = formChanged(form.blank, stored, draft)
      const record = records.get(control)
      const last = lastOf(control, form.way ?? null)
      const unheard = !out(record) && last?.kept?.unanswered === true
      const edits = [...(draft?.touched ?? [])]
      return {
        shown: formShown(form.blank, stored, draft),
        touched: new Set(draft?.touched.keys()) as ReadonlySet<string> as ReadonlySet<never>,
        changed: changed as ReadonlySet<never>,
        dirty: changed.size > 0,
        sending: record?.phase.kind === "out",
        unheard,
        retries: unheard && edits.every(([field, edit]) => last.write.edits.get(field) === edit.at),
        phase: now.phase,
        way: now.phase === "out" ? now.way as W | null : null,
        made: now.made,
        refusal: now.refusal,
      }
    },
    pressForm: (form) => {
      const control = form.action.control
      const stored = storedOf(form)
      const draft = drafts.get(control)
      const edits: Edits = new Map([...(draft?.touched ?? [])].map(([field, edit]) => [field, edit.at]))
      const sent = formShown(form.blank, stored, draft)
      const changed = formChanged(form.blank, stored, draft)
      /* A toast's Retry may send the write after the reader typed on, and then the refusal is of what its field no longer says. */
      const holds = (refusal: Refusal) => {
        const now = formShown(form.blank, storedOf(form), drafts.get(control)) as Record<string, unknown>
        return (refusal.field === null ? Object.keys(sent) : [refusal.field]).every((field) => equal(now[field], (sent as Record<string, unknown>)[field]))
      }
      press(form.action as Action<unknown, unknown>, form.body(sent, stored), {
        what: form.what,
        precondition: stored === null || form.action.creates ? { kind: "create" } : { kind: "replace", revision: stored.revision },
        about: (reason) => (form.fieldOf?.(reason, changed) as string | undefined) ?? null,
        clean: () => !formDirty(form.blank, storedOf(form), drafts.get(control)),
        away: () => places.get(control)?.kind !== "open",
        resentWhy: form.resentWhy,
        shows: (refusal) => form.shows(refusal, changed) && holds(refusal),
        way: form.way,
        conflict: (data) => formConflict(stored, storedOf(form, data), sent, edits.keys(), form.print),
      }, edits)
    },
    edit: (action, way, patch) => {
      const control = action.control
      const at = stamp()
      const draft = drafts.get(control)
      const touched = new Map(draft?.touched)
      for (const [field, value] of Object.entries(patch)) touched.set(field, { value, at })
      drafts.set(control, { touched, since: draft?.since ?? at, way: draft?.way ?? way, creation: draft?.creation ?? makingFor(action as Action<unknown, unknown>) })
      keepOn(control, way, (last) => ({ ...last, refusal: null }))
      changed(control)
    },
    unsaved: () =>
      held > 0 || records.size > 0 || [...ways.values()].some((all) => [...all.last.values()].some((last) => last.kept !== null))
      || [...drafts].some(([control, draft]) => {
        const form = forms.get(control)
        return form !== undefined && formDirty(form.blank, storedOf(form), draft)
      }),
    hold: () => {
      held++
      return () => { held-- }
    },
    /* A draft the reader drops takes its way's toast and Retry with it, so no Retry is left to send what they threw away. A write
     * another way into the control pressed, such as a rename beside the settings, isn't the draft's: its refusal, toast and Retry
     * stay. */
    discard: (control, way) => {
      drafts.delete(control)
      keepOn(control, way, (last) => ({ ...last, kept: null, refusal: null }))
      if (ways.get(control)?.latest === way) dismiss(control)
      changed(control)
    },
    mount: (control, way) => {
      move(control, { kind: "open" })
      return () => {
        const at = stamp()
        const owes = out(records.get(control)) ? "out" : lastOf(control, way)?.kept?.unanswered ? "kept" : null
        move(control, { kind: "close", owes, at })
        if (places.get(control)?.kind === "leaving") queueMicrotask(() => move(control, { kind: "over", of: at }))
      }
    },
  }
}

const KEYS: Record<ReadName, readonly unknown[]> = { state: keys.state, settings: keys.settings }
const readOf = (key: readonly unknown[]): ReadName | null => (Object.keys(KEYS) as ReadName[]).find((read) => KEYS[read][0] === key[0] && key.length === 1) ?? null

/** The query cache as the store reads it: a fetch's start, landing and failure from the cache's own events, never a
 * promise. A pause stamps nothing; a paused fetch that goes on begins again. A fetch cancelled back to the state before
 * it ended with no data, as a failed one did. */
export function cacheReads(qc: QueryClient): ReadSource {
  return {
    current: (read) => qc.getQueryData(KEYS[read]),
    subscribe: (listener) => qc.getQueryCache().subscribe((event) => {
      if (event.type !== "updated") return
      const read = readOf(event.query.queryKey)
      if (read === null) return
      const action = event.action
      if (action.type === "fetch" || action.type === "continue") listener({ read, kind: "began" })
      else if (action.type === "success") listener({ read, kind: action.manual ? "set" : "landed", data: action.data })
      else if ((action.type === "error" || action.type === "setState") && event.query.state.fetchStatus === "idle") listener({ read, kind: "ended" })
    }),
    /* An invalidation skips a query that no one observes and that has never landed, as TanStack counts it disabled, so that
     * one is fetched by itself. */
    refetch: (read) => {
      const query = qc.getQueryCache().find({ queryKey: KEYS[read], exact: true })
      if (query?.getObserversCount() === 0 && query.isDisabled() && query.state.fetchStatus === "idle") void query.fetch().catch(() => {})
      else void qc.invalidateQueries({ queryKey: KEYS[read], refetchType: "all" })
    },
  }
}

const pages = new WeakMap<QueryClient, Writes>()

/** The page's one store, one per QueryClient, so a write lives as long as the page and never as long as a mount. `startPage`
 * makes it, or a test with its own `io`. */
export function writesFor(qc: QueryClient, io?: Omit<WritesIo, "reads">): Writes {
  let writes = pages.get(qc)
  if (!writes) {
    if (!io) throw new Error("startPage makes the page's store before the page renders")
    writes = startWrites({ ...io, reads: cacheReads(qc) })
    pages.set(qc, writes)
  }
  return writes
}

/** Makes the page's store beside its router, before the page renders. A toast's Open goes through `navigate`. The two reads a
 * write settles on stay observed for as long as the page is open, so the read a write asks for is always fetched, though no
 * component on screen reads it. */
export function startPage(qc: QueryClient, navigate: WritesIo["navigate"]): Writes {
  new QueryObserver(qc, stateRead).subscribe(() => {})
  new QueryObserver(qc, settingsRead).subscribe(() => {})
  return writesFor(qc, { attempt, signIn, newId, say: sayToast, dismiss: dismissToast, navigate })
}

/** A control with no draft: a select, a box, a button. It shows the daemon's view, or `sent` while its write is out. `way`
 * names it among the ways into a control that has more than one: its presses carry it, and it shows only their refusals. */
export function useControl<B, R, W extends string = string>(action: Action<B, R, W>, way?: NoInfer<W>): Shown<B, R, W> & { readonly press: (pressed: B, how: Omit<PressHow<B, W>, "way">) => void } {
  const writes = writesFor(useQueryClient())
  const shown = useSyncExternalStore(writes.subscribe, () => writes.shown(action.control, way ?? null), () => writes.shown(action.control, way ?? null))
  return {
    ...(shown as Shown<B, never, W>),
    answer: answerOf(shown.answer, action),
    press: (pressed, how) => writes.press(action as Action<B, unknown>, pressed, { ...how, way }),
  }
}

/** Controls a component draws together, such as a table's boxes: one subscription, each read and pressed by its action. A press
 * gives the key of the write it sent, or null when the control took no press. */
export function useControls(): {
  readonly shown: <B, R, W extends string>(action: Action<B, R, W>) => Shown<B, R, W>
  readonly press: <B, R, W extends string>(action: Action<B, R, W>, pressed: B, how: PressHow<B, NoInfer<W>>) => string | null
} {
  const writes = writesFor(useQueryClient())
  useSyncExternalStore(writes.subscribe, writes.version, writes.version)
  return {
    shown: <B, R, W extends string>(action: Action<B, R, W>) => {
      const shown = writes.shown(action.control)
      return { ...(shown as Shown<B, never, W>), answer: answerOf(shown.answer, action) }
    },
    press: <B, R, W extends string>(action: Action<B, R, W>, pressed: B, how: PressHow<B, NoInfer<W>>) => writes.press(action as Action<B, unknown>, pressed, how),
  }
}

/** Typing a component holds itself, outside a form: while `dirty`, the page doesn't reload by itself. */
export function useUnsaved(dirty: boolean): void {
  const writes = writesFor(useQueryClient())
  useEffect(() => (dirty ? writes.hold() : undefined), [writes, dirty])
}

/** A form: touched fields over what is stored now, and its Save. */
export function useForm<D extends object, B, R, W extends string = string>(form: Form<D, B, R, W>): FormView<D, W> & {
  readonly edit: (patch: Partial<D>) => void
  readonly discard: () => void
  readonly press: () => void
  readonly retry: () => void
} {
  const writes = writesFor(useQueryClient())
  useSyncExternalStore(writes.subscribe, writes.version, writes.version)
  const control = form.action.control
  const way = form.way ?? null
  useEffect(() => writes.mount(control, way), [writes, control, way])
  return {
    ...writes.form(form),
    edit: (patch) => writes.edit(form.action, way, patch as Record<string, unknown>),
    discard: () => writes.discard(control, way),
    press: () => writes.pressForm(form),
    retry: () => writes.retry(control, way),
  }
}
