/* The table's and the random runs' world: the page's real store and actions, loaded as Vite builds them, against a
 * model daemon that keeps keys and revisions as API.md says, a model query cache, and node's mock timers. */
import { mock } from "node:test"
import type { GroupId, LineageId, Preferred, SettingsView, SlotEdit, SlotId, SourceId, View } from "../src/lib/schema.ts"
import type { OwnerExit } from "../src/lib/slots.ts"
import type { Action, Answer, ControlKey, PressHow, ReadData, ReadEvent, ReadName, Toast, Write, Writes, WritesIo } from "../src/lib/write.ts"

type Actions = {
  slot: (id: SlotId) => Action<SlotEdit, unknown>
  deleteSlot: (id: SlotId) => Action<void, unknown>
  tick: (group: GroupId, server: LineageId) => Action<{ ticked: boolean }, unknown>
  refresh: (id: SourceId) => Action<{ userAgent?: string }, unknown>
  settings: Action<Record<string, unknown>, unknown>
  source: (id: SourceId) => Action<Record<string, unknown>, unknown>
  deleteSource: (id: SourceId) => Action<void, unknown>
  probe: (id: LineageId) => Action<void, unknown>
  switchTo: (owner: OwnerExit) => Action<LineageId, unknown>
  unpick: (owner: OwnerExit) => Action<Preferred, unknown>
}

const { pageModules } = await import(new URL("./page-modules.mjs", import.meta.url).href)
const loaded = await pageModules("/src/lib/use-write.ts", "/src/lib/api.ts")
export const startWrites: (io: WritesIo) => Writes = loaded.modules[0].startWrites
export const actions: Actions = loaded.modules[1].actions
export const closeModules: () => Promise<void> = loaded.close

/** What the model daemon holds at one path: the item's revision and the JSON last stored there. */
type Item = { readonly revision: number; readonly value: Record<string, unknown> }
/** The view the model cache serves: the view revision and every item by path, which tests read as View. */
export type ModelView = { readonly revision: number; readonly items: Readonly<Record<string, Item>> }

const fingerprint = (w: Write<unknown>) => JSON.stringify([w.method, w.path, w.precondition, w.bytes])

/** The daemon as API.md has it: a key's first answer is all a replay gets, a create finds nothing at its id, a
 * replace names the stored revision, every commit moves the view's revision, a refresh moves none. */
export class ModelDaemon {
  revision = 40
  readonly items = new Map<string, Item>([["api/settings", { revision: 5, value: { secondField: 21600, analyticsRetentionDays: 30 } }]])
  readonly keys = new Map<string, { readonly request: string; readonly answer: Answer }>()
  /** Commits per key: a key commits once, whatever its attempts. */
  readonly commits = new Map<string, number>()
  /** A path the daemon refuses with its own 4xx, of `code` or else "validation". */
  readonly refusing = new Map<string, { readonly status: number; readonly reason: string; readonly message: string; readonly code?: string }>()
  reused = 0

  handle(write: Write<unknown>): Answer {
    const seen = this.keys.get(write.key)
    if (seen) {
      if (seen.request === fingerprint(write)) return seen.answer
      this.reused++
      return { kind: "refused", revision: this.revision, status: 422, code: "validation", reason: "keyReused", detail: null, message: "That key was used for a different action." }
    }
    const answer = this.perform(write)
    this.keys.set(write.key, { request: fingerprint(write), answer })
    return answer
  }

  private perform(write: Write<unknown>): Answer {
    const refusal = this.refusing.get(write.path)
    if (refusal) return { kind: "refused", revision: this.revision, status: refusal.status, code: refusal.code ?? "validation", reason: refusal.reason, detail: null, message: refusal.message }
    const item = this.items.get(write.path)
    const p = write.precondition
    if (p?.kind === "create" && item) return { kind: "conflict", revision: this.revision, stored: item.revision }
    if (p?.kind === "replace" && !item) return { kind: "refused", revision: this.revision, status: 404, code: "validation", reason: "slotGone", detail: null, message: "No such item." }
    if (p?.kind === "replace" && item && item.revision !== p.revision) return { kind: "conflict", revision: this.revision, stored: item.revision }
    /* A refresh alone moves nothing; one with another agent stores it on the source first, as a save does. */
    if (write.method === "POST" && write.bytes === null) return { kind: "yes", revision: this.revision, body: {} }
    if (write.method === "DELETE" && !item) return { kind: "refused", revision: this.revision, status: 404, code: "validation", reason: "slotGone", detail: null, message: "No such item." }
    this.commits.set(write.key, (this.commits.get(write.key) ?? 0) + 1)
    if (write.method === "DELETE") {
      this.items.delete(write.path)
      return { kind: "yes", revision: ++this.revision, body: {} }
    }
    const next = { revision: (item?.revision ?? 0) + 1, value: { ...item?.value, ...(JSON.parse(write.bytes ?? "{}") as Record<string, unknown>) } }
    this.items.set(write.path, next)
    return { kind: "yes", revision: ++this.revision, body: { ...next.value, revision: next.revision } }
  }

  /** Another tab's write: committed with no key the page knows. */
  other(path: string, value: Record<string, unknown>) {
    const item = this.items.get(path)
    this.items.set(path, { revision: (item?.revision ?? 0) + 1, value: { ...item?.value, ...value } })
    this.revision++
  }

  view(read: ReadName): ReadData {
    if (read === "settings") {
      const s = this.items.get("api/settings") as Item
      return { ...s.value, revision: s.revision } as unknown as SettingsView
    }
    /* The sources as the page's view lists them, each made when its stored value's addedAt says. */
    const sources = [...this.items].filter(([path]) => /^api\/sources\/[^/]+$/.test(path)).map(([path, item]) => ({ id: path.slice("api/sources/".length), addedAt: item.value.addedAt ?? null }))
    return { revision: this.revision, items: Object.fromEntries(this.items), sources } as unknown as View
  }
}

export const viewOf = (data: ReadData | undefined) => data as unknown as ModelView | undefined

/** One attempt the store sent, held until the test says what became of it. */
export type Sent = {
  readonly write: Write<unknown>
  readonly heal: boolean
  readonly signal: AbortSignal
  readonly at: number
  readonly answer: (answer: Answer) => void
}

/** One fetch of a read, begun and not yet landed. Only the latest fetch of a read lands or fails. */
export type Fetch = {
  /** Its place in the world's order of fetches begun and toasts said. */
  readonly began: number
  /** It lands with what the daemon holds by then. */
  readonly land: () => Promise<void>
  /** It lands with data from before, as a read served from an old copy does. */
  readonly landWith: (data: ReadData | undefined) => Promise<void>
  /** It fails: no answer, or an error. */
  readonly fail: () => Promise<void>
}

/** The world for one sequence, under mock setTimeout and Date: Date.now() is ms since the sequence began. With
 * `firstRead`, the page holds no state yet and its first read of it is out, as when a page has just opened. */
export function world(seed: (daemon: ModelDaemon) => void = () => {}, { firstRead = false } = {}) {
  const daemon = new ModelDaemon()
  seed(daemon)
  const sent: Sent[] = []
  const said: { readonly toast: Toast; readonly act: () => void; readonly order: number }[] = []
  let order = 0
  const dismissed: ControlKey[] = []
  const asked: ReadName[] = []
  const listeners = new Set<(event: ReadEvent) => void>()
  const cache: Record<ReadName, ReadData | undefined> = { state: firstRead ? undefined : daemon.view("state"), settings: daemon.view("settings") }
  let keys = 0
  const fetches: Record<ReadName, number> = { state: 0, settings: 0 }
  const running: Record<ReadName, Fetch | null> = { state: null, settings: null }
  let signedIn = 0
  const flush = () => new Promise((r) => setImmediate(r))
  const emit = (event: ReadEvent) => {
    if (event.kind === "landed" || event.kind === "set") cache[event.read] = event.data
    for (const listener of listeners) listener(event)
  }
  /** A fetch of `read` begins now. As in the query cache, a fetch begun while one is out cancels it. */
  const begin = (read: ReadName): Fetch => {
    const fetch = ++fetches[read]
    emit({ read, kind: "began" })
    const end = (event: () => ReadEvent) => async () => {
      if (fetch === fetches[read] && running[read] === handle) {
        running[read] = null
        emit(event())
      }
      await flush()
    }
    const landWith = (data: () => ReadData | undefined) => end(() => ({ read, kind: "landed", data: data() }))
    const handle: Fetch = { began: ++order, land: landWith(() => daemon.view(read)), landWith: (data) => landWith(() => data)(), fail: end(() => ({ read, kind: "ended" })) }
    running[read] = handle
    return handle
  }
  const io: WritesIo = {
    attempt: (write, _answer, heal, signal) => new Promise((resolve) => { sent.push({ write, heal, signal, at: Date.now(), answer: resolve }) }),
    reads: {
      current: (read) => cache[read],
      subscribe: (listener) => { listeners.add(listener); return () => { listeners.delete(listener) } },
      /* As the query cache's invalidation: with nothing cached, a fetch already out is joined; else one begins. */
      refetch: (read) => {
        asked.push(read)
        if (running[read] === null || cache[read] !== undefined) begin(read)
      },
    },
    say: (toast, act) => { said.push({ toast, act, order: ++order }) },
    dismiss: (id) => { dismissed.push(id) },
    signIn: () => { signedIn++ },
    navigate: () => {},
    newId: () => `k${++keys}`,
  }
  const writes = startWrites(io)
  if (firstRead) begin("state")

  return {
    daemon, writes, sent, said, dismissed, asked, flush,
    get signedIn() { return signedIn },
    press<B>(action: Action<B, unknown>, pressed: B, how: PressHow<B> = { what: "EU" }) { return writes.press(action, pressed, how) },
    /** Moves mock time on, firing the store's timers on the way. */
    async wait(ms: number) {
      mock.timers.tick(ms)
      await flush()
    },
    /** Attempt i reaches the daemon and its answer comes back. */
    async land(i: number) { sent[i].answer(daemon.handle(sent[i].write)); await flush() },
    /** Attempt i reaches the daemon; its answer never comes back. */
    lose(i: number) { daemon.handle(sent[i].write) },
    /** Attempt i reaches the daemon now; its answer comes back when the test calls the result. */
    reach(i: number) {
      const answer = daemon.handle(sent[i].write)
      return async () => { sent[i].answer(answer); await flush() }
    },
    /** Attempt i comes back with `answer` from in front of the daemon, or with no connection, having reached nothing. */
    async refuse(i: number, answer: Answer) { sent[i].answer(answer); await flush() },
    /** A fetch of `read` begins now and lands later. */
    begin: (read: ReadName = "state") => begin(read),
    /** The fetch of `read` that is out, the store's own included, or null. */
    running: (read: ReadName = "state") => running[read],
    /** A whole read of `read`: begins and lands now. */
    async read(read: ReadName = "state") { await begin(read).land() },
    cached: (read: ReadName = "state") => cache[read],
    toasts: () => said.map(({ toast }) => [toast.what, toast.why, toast.muted, toast.button?.word ?? null].filter((x) => x !== null).join(" · ")),
    keys: () => sent.map((s) => s.write.key),
  }
}
