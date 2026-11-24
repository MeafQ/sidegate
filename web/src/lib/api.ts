import { z } from "zod"
import { testWords } from "@/lib/health"
import { refusalWords } from "@/lib/troubles"
import { END_MS, READ_TIMEOUT_MS, type Action, type Answer, type ControlKey, type ReadData, type Write } from "@/lib/write"
import type { OwnerExit } from "@/lib/slots"
import {
  ChangeEvent, GroupPreview, GroupView, ProbeResult, Session, SettingsView, SlotView, SourceView, UpdateView,
  type CheckForUpdates, type GroupCreate, type GroupId, type GroupInput, type LineageId, type Members, type Preferred, type Settings, type SlotEdit, type SlotId, type SlotInput, type SourceId, type SourceInput, type View,
} from "@/lib/schema"

export * from "@/lib/schema"

/** A new item's id, or an action's Idempotency-Key: random, never derived from a name, so no two can share one. It
 * draws on getRandomValues because a page on a plain-HTTP LAN address is no secure context, so crypto.randomUUID is missing. */
export function newId(): string {
  const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
  return Array.from(crypto.getRandomValues(new Uint8Array(10)), (b) => alphabet[b % 32]).join("")
}

/** What the page asks of the daemon beyond its reads and its writes: a group's preview, a check for a newer release, an update,
 * and the change feed. A write's response means durable acceptance, not applied readiness: what follows arrives through `events`. */
export interface Api {
  previewGroup(members: Members, signal?: AbortSignal): Promise<GroupPreview>
  /** Ends when the check did. `auto` is the page's own, which the daemon keeps to once a day and to its setting. */
  checkForUpdate(auto: boolean): Promise<UpdateView>
  /** One press of Update, sent once: what became of it is read from api/update. */
  update(confirmed: { version: string; restartsCore: boolean }): Promise<UpdateView>
  /** Calls `change` for each change, `lost` each time the stream fails, and `resync` after a reconnect, when changes may
   * have been missed. Returns the unsubscribe. */
  events(handlers: { change: (event: ChangeEvent) => void; lost: () => void; resync: () => void }): () => void
}

/** `login`: a 401 or 403 that names no kind, so the login stopped the request: UniFi's in front of the daemon, or the
 * daemon's own check that nginx vouched for one. */
export type ApiErrorKind = "validation" | "staleRevision" | "unsupported" | "capacity" | "referenced" | "unavailable" | "retryable" | "csrf" | "login" | "network" | "mismatch" | "unknown"

/** A failed read or preview. A daemon refusal names its `reason`, and a refused pattern its `detail`; anything else has neither. */
export class ApiError extends Error {
  readonly status: number
  readonly kind: ApiErrorKind
  readonly reason: string | null
  readonly detail: string | null
  constructor({ status, kind, message, reason = null, detail = null }: { status: number; kind: ApiErrorKind; message: string; reason?: string | null; detail?: string | null }) {
    super(message)
    this.status = status
    this.kind = kind
    this.reason = reason
    this.detail = detail
  }
}

/** What a failed read or preview says: a daemon refusal in its reason's words, anything else in its own. */
export const errorWords = (error: ApiError) => (error.reason === null ? error.message : refusalWords({ reason: error.reason, detail: error.detail, message: error.message }))

/** The page's prefix, e.g. "/sidegate/": Vite's `base`, set from BASE_PATH at build time. */
export const BASE = import.meta.env.BASE_URL

/** The daemon's own CSRF header, internal/daemon's CSRFHeader. */
const CSRF_HEADER = "X-Sidegate-Csrf"
/** The view revision a write's answer was committed at, internal/daemon's RevisionHeader. */
const REVISION_HEADER = "Sidegate-Revision"
const NO_ANSWER = "Gateway not answering"
/** A check answers in 15 s at most, and 20 s leaves its way there and back. */
const CHECK_MS = 20_000
/** A reason the page doesn't know, or none, is worded by the message. */
const ErrorBody = z.object({ kind: z.string(), reason: z.string().default(""), message: z.string(), detail: z.string().optional(), revision: z.number().int().nonnegative().optional() })
const MISMATCH = "Bad answer from the gateway: page and daemon may differ in version"
const SIGNED_OUT = "Signed out of UniFi. Sign in again."
const REFUSED = "Refused by UniFi. Sign in again."
const KINDS: readonly ApiErrorKind[] = ["validation", "staleRevision", "unsupported", "capacity", "referenced", "unavailable", "retryable", "csrf"]
const isKind = (k: string): k is ApiErrorKind => (KINDS as readonly string[]).includes(k)

function etagRevision(res: Response): number | null {
  const match = /^(?:W\/)?"(\d+)"$/.exec(res.headers.get("ETag") ?? "")
  return match ? Number(match[1]) : null
}

/** UniFi's CSRF token, which nginx hands out on an answer to a request that carried none. Null on a page that has seen
 * none, from the dev server or a console with UniFi's check turned off. */
function unifiToken(res: Response): string | null {
  return res.headers.get("X-Csrf-Token") || null
}

/** What one attempt's response says of its write. A daemon refusal has a `kind`; an answer with none came from in front
 * of the daemon, which saw nothing. Only what may have landed is `unknown`. */
function answerOf(res: Response, text: string, schema: z.ZodType<unknown>): Answer {
  const header = res.headers.get(REVISION_HEADER)
  const revision = header !== null && /^\d+$/.test(header) ? Number(header) : null
  let body: unknown
  try { body = JSON.parse(text) } catch { body = undefined }
  if (res.ok) {
    const parsed = schema.safeParse(body)
    return parsed.success && revision !== null ? { kind: "yes", revision, body: parsed.data } : { kind: "unknown", status: res.status }
  }
  if (res.status >= 500) return { kind: "unknown", status: res.status }
  const daemon = ErrorBody.safeParse(body)
  if (!daemon.success) {
    if (res.status === 401 || res.status === 403) return { kind: "token", status: res.status }
    return { kind: "front", status: res.status, retry: res.status === 429 }
  }
  const { kind, reason, message } = daemon.data
  if (kind === "csrf") return { kind: "token", status: 403 }
  if (revision === null) return { kind: "unknown", status: res.status }
  const stored = res.status === 412 ? daemon.data.revision ?? etagRevision(res) : null
  if (stored !== null) return { kind: "conflict", revision, stored }
  return { kind: "refused", revision, status: res.status, code: kind, reason, detail: daemon.data.detail ?? null, message }
}

function liveApi(): Api & {
  attempt: (write: Write<unknown>, answer: z.ZodType<unknown>, heal: boolean, signal: AbortSignal) => Promise<Answer>
  session: () => Promise<string>
  read: <T>(schema: z.ZodType<T>, path: string, signal?: AbortSignal) => Promise<T>
} {
  /** The session's CSRF token, fetched once, failed at READ_TIMEOUT_MS as any read, so a held one never outlives the
   * write that waits on it. A failed fetch is forgotten, so the next write asks again; a held one is forgotten by `session` alone. */
  let csrf: Promise<string> | null = null
  /** The latest of UniFi's tokens the page has seen. Each mutation sends it, or none until one is seen. */
  let unifi: string | null = null
  function token(): Promise<string> {
    const pending = csrf ?? read(Session, "api/session").then((s) => s.csrf)
    csrf = pending
    pending.catch(() => { if (csrf === pending) csrf = null })
    return pending
  }

  /** The daemon's token is awaited first: fetching it can bring UniFi's. */
  async function tokens(): Promise<Record<string, string>> {
    const daemon = await token()
    return unifi === null ? { [CSRF_HEADER]: daemon } : { [CSRF_HEADER]: daemon, "X-Csrf-Token": unifi }
  }
  const heard = (res: Response) => { unifi = unifiToken(res) ?? unifi }
  const forToken = (error: ApiError) => error.kind === "csrf" || (error.kind === "login" && error.status === 403)

  type Request = { method?: string; path: string; body?: unknown; signal?: AbortSignal }

  async function call(method: string, path: string, headers: Record<string, string>, body: unknown, signal?: AbortSignal): Promise<Response> {
    try {
      const res = await fetch(BASE + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: "same-origin", signal })
      heard(res)
      return res
    } catch {
      throw new ApiError({ status: 0, kind: "network", message: NO_ANSWER })
    }
  }

  async function failure(res: Response): Promise<ApiError> {
    const parsed = ErrorBody.safeParse(await res.json().catch(() => null))
    if (parsed.success) {
      const { kind, reason, message, detail = null } = parsed.data
      return new ApiError({ status: res.status, kind: isKind(kind) ? kind : "unknown", reason, detail, message })
    }
    if (res.status === 401) return new ApiError({ status: 401, kind: "login", message: SIGNED_OUT })
    if (res.status === 403) return new ApiError({ status: 403, kind: "login", message: REFUSED })
    return new ApiError({ status: res.status, kind: "unknown", message: `HTTP ${res.status}` })
  }

  /** One request, sent once. A POST here carries both CSRF tokens and no Idempotency-Key. */
  async function request({ method = "GET", path, body, signal }: Request): Promise<Response> {
    const headers: Record<string, string> = { Accept: "application/json" }
    if (body !== undefined) headers["Content-Type"] = "application/json"
    const auth = method === "GET" ? {} : await tokens()
    const res = await call(method, path, { ...headers, ...auth }, body, signal)
    if (res.ok) return res
    throw await failure(res)
  }

  /** A response that doesn't fit its schema is a version skew between page and daemon: the reader gets a sentence,
   * the console gets the details. */
  async function get<T>(schema: z.ZodType<T>, req: Request): Promise<T> {
    const res = await request(req)
    /* A body cut off on the way, as a page leaving mid-read cuts it, is no answer, not an answer of another version. */
    const text = await res.text().catch(() => { throw new ApiError({ status: 0, kind: "network", message: NO_ANSWER }) })
    let body: unknown
    try { body = JSON.parse(text) } catch { body = undefined }
    const parsed = schema.safeParse(body)
    if (parsed.success) return parsed.data
    console.error(`${req.method ?? "GET"} ${req.path}: the answer doesn't match the page's schema\n${z.prettifyError(parsed.error)}`)
    throw new ApiError({ status: res.status, kind: "mismatch", message: MISMATCH })
  }

  /** Reads api/session again, which brings both tokens anew, and gives the daemon's. The one place a held token is dropped: a
   * write's heal and an unkeyed POST refused for its token both come here. */
  function session(): Promise<string> {
    csrf = null
    return token()
  }

  /** One HTTP attempt of `write` under its key, with no second try: the caller owns resends. `heal` rereads api/session
   * first and sends even when that read fails, so a signed-out session is answered by its 401 and not swallowed. */
  async function attempt(write: Write<unknown>, schema: z.ZodType<unknown>, heal: boolean, signal: AbortSignal): Promise<Answer> {
    const daemon = await (heal ? session() : token()).catch(() => null)
    const headers: Record<string, string> = { Accept: "application/json", "Idempotency-Key": write.key }
    if (daemon !== null) headers[CSRF_HEADER] = daemon
    if (unifi !== null) headers["X-Csrf-Token"] = unifi
    if (write.bytes !== null) headers["Content-Type"] = "application/json"
    if (write.precondition?.kind === "create") headers["If-None-Match"] = "*"
    if (write.precondition?.kind === "replace") headers["If-Match"] = `"${write.precondition.revision}"`
    try {
      const res = await fetch(BASE + write.path, { method: write.method, headers, body: write.bytes ?? undefined, credentials: "same-origin", signal })
      heard(res)
      return answerOf(res, await res.text(), schema)
    } catch {
      return { kind: "unknown", status: 0 }
    }
  }

  /** `send` with a limit of its own: no answer by `ms` is a network failure. */
  async function within<T>(ms: number, send: (signal: AbortSignal) => Promise<T>, signal?: AbortSignal): Promise<T> {
    const limit = new AbortController()
    const timer = setTimeout(() => limit.abort(), ms)
    try {
      return await send(signal ? AbortSignal.any([signal, limit.signal]) : limit.signal)
    } catch (error) {
      throw limit.signal.aborted ? new ApiError({ status: 0, kind: "network", message: NO_ANSWER }) : error
    } finally {
      clearTimeout(timer)
    }
  }

  /** A GET that fails when no answer has come in READ_TIMEOUT_MS (rule 4), so no read stands for ever. */
  const read = <T>(schema: z.ZodType<T>, path: string, signal?: AbortSignal): Promise<T> =>
    within(READ_TIMEOUT_MS, (limited) => get(schema, { path, signal: limited }), signal)

  /** A POST with no key. Refused for a token, by the daemon or by UniFi's login, it reached nothing: it reads api/session again,
   * which brings both anew, and goes once more. Anything else is its one answer. */
  async function unkeyed<T>(schema: z.ZodType<T>, path: string, body: unknown, signal?: AbortSignal): Promise<T> {
    try {
      return await get(schema, { method: "POST", path, body, signal })
    } catch (error) {
      if (!(error instanceof ApiError) || !forToken(error)) throw error
      void session()
      return get(schema, { method: "POST", path, body, signal })
    }
  }
  return {
    attempt,
    session,
    read,
    previewGroup: (members, signal) => unkeyed(GroupPreview, "api/groups/preview", { members }, signal),
    checkForUpdate: (auto) => within(CHECK_MS, (signal) => unkeyed(UpdateView, "api/update/check", { auto }, signal)),
    update: (confirmed) => within(END_MS, (signal) => unkeyed(UpdateView, "api/update", confirmed, signal)),
    /** The page owns the retry: EventSource's own ignores any backoff (every few seconds, or as often as the stream's
     * `retry:` says, while the gateway refuses) and gives up for good on a non-200 answer. So every failure closes the
     * stream and a new one opens after a wait that doubles to 30 s. The wait starts over only once a connection has
     * stayed up for 30 s: a feed that opens and drops at once is failing too, and each of its opens costs a resync.
     * Any connection that follows a failure, the first included, resyncs, since changes may have gone by unseen. */
    events({ change, lost, resync }) {
      let source: EventSource | undefined
      let retry: ReturnType<typeof setTimeout> | undefined
      let healthy: ReturnType<typeof setTimeout> | undefined
      let failures = 0
      const connect = () => {
        const current = new EventSource(BASE + "api/events")
        source = current
        current.onopen = () => {
          if (failures > 0) resync()
          healthy = setTimeout(() => { failures = 0 }, 30_000)
        }
        current.onmessage = (message) => {
          let data: unknown = null
          try { data = JSON.parse(message.data) } catch { /* a malformed frame is dropped, as an unknown one is */ }
          const parsed = ChangeEvent.safeParse(data)
          if (parsed.success) change(parsed.data)
        }
        current.onerror = () => {
          lost()
          clearTimeout(healthy)
          current.close()
          retry = setTimeout(connect, Math.min(30_000, 1000 * 2 ** failures++))
        }
      }
      connect()
      return () => {
        clearTimeout(retry)
        clearTimeout(healthy)
        source?.close()
      }
    },
  }
}

const live = liveApi()
export const api: Api = live
/** The write path's transport: one attempt of a write, a session reread, a read that ends at READ_TIMEOUT_MS. Only
 * use-write.ts calls these. */
export const { attempt, session, read } = live

const control = (key: string) => key as ControlKey
const accepted = z.unknown()
const whole = <B>(pressed: B) => pressed
const none = () => undefined
const ownerPath = (owner: OwnerExit) => (owner.kind === "group" ? `api/groups/${owner.group}` : `api/sources/${owner.source}`)
const ownerControl = (owner: OwnerExit) => control(`switch:${owner.kind}:${owner.kind === "group" ? owner.group : owner.source}`)

/** Where a create of each kind is found landed: the view's list of them. */
const SLOTS = { listed: (data: ReadData) => (data as View).slots }
const SOURCES = { listed: (data: ReadData) => (data as View).sources }
const GROUPS = { listed: (data: ReadData) => (data as View).groups }
/** Which making of source `id` the view lists: two at one id were made at different times. One stored before the daemon kept the
 * time is the one made before any it keeps. */
const sourceMade = (id: SourceId) => (data: ReadData) => {
  const source = (data as View).sources.find((s) => s.id === id)
  return source === undefined ? null : (source.addedAt ?? "")
}

/** The ways into a source's or a group's one write: the heading's rename, the settings' or the rule's Save, and Delete. */
const ITEM_WAYS = ["rename", "save", "delete"] as const
export type ItemWay = (typeof ITEM_WAYS)[number]
/** The ways into a source's one refresh: the heading's Refresh, the Retry in its card, or the Refresh or Retry in the table's place. */
const REFRESH_WAYS = ["heading", "card", "block"] as const
export type RefreshWay = (typeof REFRESH_WAYS)[number]

/** Every write the page makes, by control. Two actions on one control share its key, so a source's save and its delete
 * can't both be out. Components press these through use-write.ts and never send them themselves. */
export const actions = {
  /** A slot's PUT carries its name and its Exit; `edit` says which the reader changed, so its Pending stands where that shows. */
  slot: (id: SlotId): Action<SlotEdit, SlotView> => ({ control: control(`slot:${id}`), method: "PUT", path: `api/slots/${id}`, body: ({ input }) => input, settles: "state", answer: SlotView }),
  deleteSlot: (id: SlotId): Action<void, unknown> => ({ control: control(`slot:${id}`), method: "DELETE", path: `api/slots/${id}`, body: none, settles: "state", answer: accepted }),
  /** The board's one Add: a new slot at an id of its own, on one control so a second Add waits for the first. */
  newSlot: { control: control("slot:new"), method: "PUT", path: "api/slots", creates: SLOTS, body: whole<SlotInput>, settles: "state", answer: SlotView } satisfies Action<SlotInput, SlotView>,
  enableClient: (id: SlotId): Action<void, unknown> => ({ control: control(`client:${id}`), method: "POST", path: `api/slots/${id}/client/enable`, body: none, settles: "state", answer: accepted }),
  recreateClient: (id: SlotId): Action<void, unknown> => ({ control: control(`client:${id}`), method: "POST", path: `api/slots/${id}/client/recreate`, body: none, settles: "state", answer: accepted }),
  /** A stored source's settings and its rename. */
  source: (id: SourceId): Action<SourceInput, SourceView, ItemWay> => ({ control: control(`source:${id}`), ways: ITEM_WAYS, method: "PUT", path: `api/sources/${id}`, creation: sourceMade(id), body: whole, settles: "state", answer: SourceView }),
  /** One opening of Add source, on a control of its own apart from the id it adds at, which changes with the request. */
  addSource: (opening: string): Action<SourceInput, SourceView> => ({ control: control(`add:${opening}`), method: "PUT", path: "api/sources", creates: SOURCES, body: whole, settles: "state", answer: SourceView }),
  deleteSource: (id: SourceId): Action<void, unknown, ItemWay> => ({ control: control(`source:${id}`), ways: ITEM_WAYS, method: "DELETE", path: `api/sources/${id}`, creation: sourceMade(id), body: none, settles: "state", answer: accepted }),
  refresh: (id: SourceId): Action<{ userAgent?: string }, unknown, RefreshWay> => ({
    control: control(`refresh:${id}`), ways: REFRESH_WAYS, method: "POST", path: `api/sources/${id}/refresh`, settles: "state", answer: accepted,
    body: ({ userAgent }) => (userAgent === undefined ? undefined : { userAgent }),
  }),
  group: (id: GroupId): Action<GroupInput, GroupView, ItemWay> => ({ control: control(`group:${id}`), ways: ITEM_WAYS, method: "PUT", path: `api/groups/${id}`, body: whole, settles: "state", answer: GroupView }),
  deleteGroup: (id: GroupId): Action<void, unknown, ItemWay> => ({ control: control(`group:${id}`), ways: ITEM_WAYS, method: "DELETE", path: `api/groups/${id}`, body: none, settles: "state", answer: accepted }),
  /** A new group's Save: its name, rule and ticks in one create, on the control the group keeps once it is stored. */
  createGroup: (opening: string): Action<GroupCreate, GroupView> => ({ control: control(`new-group:${opening}`), method: "PUT", path: "api/groups", creates: GROUPS, body: whole, settles: "state", answer: GroupView }),
  /** Save as group: a new group at once, on one control so a second waits for the first. */
  newGroup: { control: control("group:new"), method: "PUT", path: "api/groups", creates: GROUPS, body: whole<GroupCreate>, settles: "state", answer: GroupView } satisfies Action<GroupCreate, GroupView>,
  tick: (group: GroupId, server: LineageId): Action<{ ticked: boolean }, GroupView> => ({
    control: control(`tick:${group}:${server}`), method: "PUT", path: `api/groups/${group}/ticked/${server}`, body: ({ ticked }) => ({ ticked }), settles: "state", answer: GroupView,
  }),
  settings: { control: control("settings"), method: "PUT", path: "api/settings", body: whole, settles: "settings", answer: SettingsView } satisfies Action<Settings, SettingsView>,
  /** About's box: the update check on or off, at once, apart from Retention's Save. */
  checkForUpdates: { control: control("check-for-updates"), method: "PUT", path: "api/settings/check-for-updates", body: whole<CheckForUpdates>, settles: "settings", answer: SettingsView } satisfies Action<CheckForUpdates, SettingsView>,
  /** A key for UniFi, tried on UniFi before it is stored. The key is the body alone and never comes back. */
  unifiKey: { control: control("unifi:key"), method: "PUT", path: "api/unifi/key", body: (key) => ({ key }), settles: "state", answer: accepted } satisfies Action<string, unknown>,
  /** A press on one of a group's or source's servers: the daemon tests it, and on a pass moves the owner's slots there and
   * boosts it, a boost that fades. Its answer is the Test's, and a Test that failed or couldn't run moved nothing. One control
   * per owner, so while one press is out every other way in waits. */
  switchTo: (owner: OwnerExit): Action<LineageId, ProbeResult> => ({
    control: ownerControl(owner), method: "POST", path: `${ownerPath(owner)}/preferred`, to: (server) => `${ownerPath(owner)}/preferred/${server}`, body: none, settles: "state", answer: ProbeResult,
    failed: testWords,
  }),
  /** Clear: drops the owner's pick the view names, by its server and its end, and moves no slot. On the press's control, so a
   * press and Clear never cross. */
  unpick: (owner: OwnerExit): Action<Preferred, unknown> => ({
    control: ownerControl(owner), method: "DELETE", path: `${ownerPath(owner)}/preferred`, body: none, settles: "state", answer: accepted,
    to: ({ server, endsAt }) => `${ownerPath(owner)}/preferred/${server}?endsAt=${encodeURIComponent(endsAt)}`,
  }),
  /** A Test: its answer is the result, and it changes nothing to read. */
  probe: (id: LineageId): Action<void, ProbeResult> => ({ control: control(`probe:${id}`), method: "POST", path: `api/servers/${id}/probe`, body: none, settles: "none", answer: ProbeResult }),
} as const

/** What the page says beside its writes, each under a control of its own: a press of Update, a press of Check, and the release that
 * answers now. */
export const updateControls = { update: control("update"), check: control("update-check"), release: control("release") } as const

/** Network's Settings root, where UniFi edits routes and makes API keys. The exact pages aren't verified, and firmware moves them,
 * so the slot boxes and Settings' UniFi card both open the root. */
export const UNIFI_SETTINGS = "/network/default/settings"

/** UniFi OS's own login page, at the console's root, and the parameter it returns by. Neither is measured on a gateway yet:
 * the lab's stand-in login is a cookie and has no page. */
const UNIFI_LOGIN = "/login"
const UNIFI_RETURN = "redirect"

/** Opens UniFi's login, which comes back to the page the reader is on. */
export function signIn(): void {
  const here = window.location.pathname + window.location.search + window.location.hash
  window.location.assign(`${UNIFI_LOGIN}?${new URLSearchParams({ [UNIFI_RETURN]: here })}`)
}
