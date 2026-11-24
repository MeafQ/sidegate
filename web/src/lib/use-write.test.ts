import assert from "node:assert/strict"
import { after, afterEach, describe, mock, test } from "node:test"
import { createElement } from "react"
import { renderToString } from "react-dom/server"
import { onlineManager, QueryClient, QueryClientProvider, QueryObserver } from "@tanstack/react-query"
import type { GroupId, LineageId, View } from "./schema.ts"
import { isPending, type Action, type Answer, type ControlKey, type Form, type ReadSource, type Shown, type Toast, type Writes, type WritesIo } from "./write.ts"

const { pageModules } = await import(new URL("../../tests/page-modules.mjs", import.meta.url).href)
const loaded = await pageModules("/src/lib/use-write.ts", "/src/lib/api.ts")
const { writesFor, cacheReads, useControl, useForm } = loaded.modules[0] as {
  writesFor: (qc: QueryClient, io?: Omit<WritesIo, "reads">) => Writes
  cacheReads: (qc: QueryClient) => ReadSource
  useControl: <B, R>(action: Action<B, R>) => Shown<B, R>
  useForm: <D extends object>(form: Form<D, unknown, unknown>) => { shown: D; phase: string }
}
const { actions } = loaded.modules[1] as { actions: { tick: (g: GroupId, s: LineageId) => Action<{ ticked: boolean }, unknown>; settings: Action<Record<string, unknown>, unknown> } }
after(() => loaded.close())

const box = actions.tick("eu" as GroupId, "l-ams" as LineageId)
const view = (revision: number) => ({ revision }) as unknown as View
const flush = () => new Promise((r) => setImmediate(r))

describe("a way into a control", () => {
  test("is one its action declares, wherever a press or a form names it", () => {
    /* Never run: the compiler is the test, as the typecheck reads this file. */
    const typed = (writes: Writes, source: Action<{ name: string }, unknown, "rename" | "save">) => {
      writes.press(source, { name: "A" }, { what: "A", way: "rename" })
      // @ts-expect-error "bogus" is no way into the action's control
      writes.press(source, { name: "A" }, { what: "A", way: "bogus" })
      writes.pressForm({
        action: source, what: "A", blank: { name: "" }, stored: () => null, body: (shown) => shown, print: () => null, shows: () => false,
        // @ts-expect-error "bogus" is no way into the action's control
        way: "bogus",
      })
    }
    assert.equal(typeof typed, "function")
  })
})

/** A page's QueryClient whose state read the test answers by hand, and whose writes the test answers by hand. */
function page() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } })
  const reads: { resolve: (v: View) => void }[] = []
  const answers: ((a: Answer) => void)[] = []
  const said: string[] = []
  const io: Omit<WritesIo, "reads"> = {
    attempt: () => new Promise((resolve) => { answers.push(resolve) }),
    say: (toast) => { said.push(`${toast.what} · ${toast.why}`) },
    dismiss: () => {}, signIn: () => {}, navigate: () => {},
    newId: (() => { let n = 0; return () => `k${++n}` })(),
  }
  /** The state read as a page draws it: a mounted client and an observer, so a paused fetch goes on when the page is
   * back online and an invalidation refetches it. */
  const observe = () => {
    qc.mount()
    const observer = new QueryObserver(qc, { queryKey: ["state"], queryFn: () => new Promise<View>((resolve) => { reads.push({ resolve }) }), staleTime: 0 })
    const stop = observer.subscribe(() => {})
    return () => { stop(); qc.unmount() }
  }
  return { qc, io, reads, answers, said, observe }
}

afterEach(() => { mock.timers.reset(); onlineManager.setOnline(true) })

describe("the page's one store", () => {
  test("is one per page: the same QueryClient gets the same store, another page another", () => {
    const a = page()
    const b = page()
    assert.equal(writesFor(a.qc, a.io), writesFor(a.qc, a.io))
    assert.notEqual(writesFor(a.qc, a.io), writesFor(b.qc, b.io))
  })

  test("a control's record outlives the component that pressed it: a new mount draws it Pending", async () => {
    const p = page()
    p.qc.setQueryData(["state"], view(40))
    const writes = writesFor(p.qc, p.io)
    const Box = () => {
      const shown = useControl(box)
      return createElement("button", { "aria-busy": isPending(shown.phase), "aria-checked": shown.sent?.ticked ?? false })
    }
    const mount = () => renderToString(createElement(QueryClientProvider, { client: p.qc }, createElement(Box)))
    assert.equal(mount(), '<button aria-busy="false" aria-checked="false"></button>')
    writes.press(box, { ticked: true }, { what: "Amsterdam" })
    assert.equal(mount(), '<button aria-busy="true" aria-checked="true"></button>', "a second mount, after the first is gone")
    p.answers[0]({ kind: "yes", revision: 41, body: {} })
    await flush()
    assert.equal(mount(), '<button aria-busy="true" aria-checked="true"></button>', "answered at 41, the page holds 40")
    p.qc.setQueryData(["state"], view(41))
    assert.equal(mount(), '<button aria-busy="false" aria-checked="false"></button>', "settled on the view at 41, which is the daemon's")
  })

  test("a control's shown value is the same object until it changes, so a hook reading it renders once", () => {
    const p = page()
    const writes = writesFor(p.qc, p.io)
    writes.press(box, { ticked: true }, { what: "Amsterdam" })
    assert.equal(writes.shown(box.control), writes.shown(box.control))
  })
})

describe("what the store reads of the query cache", () => {
  test("a read paused offline settles nothing though its refetch promise resolves; it settles once it goes on and lands", async () => {
    const p = page()
    const stop = p.observe()
    p.reads[0].resolve(view(40))
    await flush()
    const writes = writesFor(p.qc, p.io)
    writes.press(box, { ticked: true }, { what: "Amsterdam" })
    p.answers[0]({ kind: "yes", revision: 41, body: {} })
    await flush()
    onlineManager.setOnline(false)
    await p.qc.refetchQueries({ queryKey: ["state"], type: "all" })
    assert.equal(p.qc.getQueryState(["state"])?.fetchStatus, "paused")
    assert.equal(isPending(writes.shown(box.control).phase), true, "the refetch's promise resolved on a paused read")
    const asked = p.reads.length
    onlineManager.setOnline(true)
    await flush()
    assert.equal(p.reads.length, asked + 1, "the paused fetch went on")
    p.reads.at(-1)?.resolve(view(41))
    await flush()
    assert.equal(isPending(writes.shown(box.control).phase), false)
    stop()
  })

  test("a fetch an invalidation cancels never lands, so only the latest fetch of a read stamps a landing", async () => {
    const p = page()
    const heard: string[] = []
    cacheReads(p.qc).subscribe((e) => { heard.push(e.kind === "began" || e.kind === "ended" ? e.kind : `${e.kind} ${(e.data as View).revision}`) })
    const stop = p.observe()
    p.reads[0].resolve(view(40))
    await flush()
    void p.qc.invalidateQueries({ queryKey: ["state"] })
    await flush()
    const latest = p.qc.invalidateQueries({ queryKey: ["state"] })
    await flush()
    p.reads[1].resolve(view(41))
    p.reads[2].resolve(view(42))
    await latest
    await flush()
    assert.deepEqual(heard, ["began", "landed 40", "began", "began", "landed 42"])
    stop()
  })

  test("a write's read is asked though no component reads it now, and its landing settles the write", async () => {
    const p = page()
    let fetches = 0
    await p.qc.fetchQuery({ queryKey: ["state"], queryFn: () => Promise.resolve(view(++fetches === 1 ? 40 : 41)) })
    assert.equal(p.qc.getQueryCache().find({ queryKey: ["state"] })?.getObserversCount(), 0)
    const writes = writesFor(p.qc, p.io)
    writes.press(box, { ticked: true }, { what: "Amsterdam" })
    p.answers[0]({ kind: "yes", revision: 41, body: {} })
    await flush()
    assert.equal(fetches, 2, "the state was read again with no observer")
    assert.equal(isPending(writes.shown(box.control).phase), false)
  })

  test("a read cancelled before its first landing, with no observer, is asked again for the write that waits on it", async () => {
    mock.timers.enable({ apis: ["setTimeout"] })
    const p = page()
    const asked: ((v: View) => void)[] = []
    const first = p.qc.fetchQuery({ queryKey: ["state"], queryFn: () => new Promise<View>((resolve) => { asked.push(resolve) }) }).catch(() => null)
    const writes = writesFor(p.qc, p.io)
    writes.press(box, { ticked: true }, { what: "Amsterdam" })
    p.answers[0]({ kind: "yes", revision: 41, body: {} })
    await flush()
    const before = asked.length
    await p.qc.cancelQueries({ queryKey: ["state"] })
    await first
    assert.equal(p.qc.getQueryCache().find({ queryKey: ["state"] })?.getObserversCount(), 0)
    mock.timers.tick(10_000)
    await flush()
    assert.ok(asked.length > before, "asked again")
    asked.at(-1)?.(view(41))
    await flush()
    assert.equal(isPending(writes.shown(box.control).phase), false, "its landing settles the write")
  })

  test("data the page puts in the cache itself is not a read: it settles no write that ended unanswered", async () => {
    const p = page()
    p.qc.setQueryData(["state"], view(40))
    mock.timers.enable({ apis: ["setTimeout"] })
    const writes = writesFor(p.qc, p.io)
    writes.press(box, { ticked: true }, { what: "Amsterdam" })
    mock.timers.tick(30_000)
    mock.timers.reset()
    await flush()
    assert.deepEqual(p.said, ["Amsterdam · timed out"])
    p.qc.setQueryData(["state"], view(41))
    assert.equal(isPending(writes.shown(box.control).phase), true)
    const fresh = p.qc.fetchQuery({ queryKey: ["state"], queryFn: () => Promise.resolve(view(41)), staleTime: 0 })
    await fresh
    assert.equal(isPending(writes.shown(box.control).phase), false)
  })
})

describe("a form's draft", () => {
  type D = { analyticsRetentionDays: number }
  const settings: Form<D, unknown, unknown> = {
    action: actions.settings as Form<D, unknown, unknown>["action"], what: "Settings", blank: { analyticsRetentionDays: 0 },
    stored: (data) => (data ? { value: { analyticsRetentionDays: (data as unknown as D).analyticsRetentionDays }, revision: data.revision } : null),
    body: (shown) => shown, print: () => null, shows: () => true,
  }
  const control = settings.action.control as ControlKey
  const fresh = () => {
    const p = page()
    p.qc.setQueryData(["settings"], { analyticsRetentionDays: 30, revision: 5 })
    return { p, writes: writesFor(p.qc, p.io) }
  }

  test("leaving a form with no write out drops its draft", async () => {
    const { writes } = fresh()
    const release = writes.mount(control, null)
    writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    release()
    await flush()
    assert.deepEqual(writes.form(settings).shown, { analyticsRetentionDays: 30 })
  })

  test("a form released and opened again in one task, as StrictMode does, keeps its draft", async () => {
    const { writes } = fresh()
    const release = writes.mount(control, null)
    writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    release()
    const again = writes.mount(control, null)
    await flush()
    assert.deepEqual(writes.form(settings).shown, { analyticsRetentionDays: 60 })
    again()
    await flush()
    assert.deepEqual(writes.form(settings).shown, { analyticsRetentionDays: 30 })
  })

  test("a draft whose write is out outlives its form, and a yes drops it", async () => {
    const { p, writes } = fresh()
    const release = writes.mount(control, null)
    writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    writes.pressForm(settings)
    release()
    assert.deepEqual(writes.form(settings).shown, { analyticsRetentionDays: 60 })
    p.answers[0]({ kind: "yes", revision: 41, body: { analyticsRetentionDays: 60, revision: 6 } })
    await flush()
    p.qc.setQueryData(["settings"], { analyticsRetentionDays: 60, revision: 6 })
    assert.equal(writes.form(settings).dirty, false)
    assert.equal(writes.form(settings).phase === "out", false)
  })

  test("a refusal keeps the draft for the form to show where its toast points", async () => {
    const { p, writes } = fresh()
    const release = writes.mount(control, null)
    writes.edit(settings.action, null, { analyticsRetentionDays: 999 })
    writes.pressForm(settings)
    release()
    p.answers[0]({ kind: "refused", revision: 40, status: 422, code: "validation", reason: "retentionRange", detail: null, message: "analyticsRetentionDays is outside 1..365." })
    await flush()
    const reopened = writes.form(settings)
    assert.deepEqual(reopened.shown, { analyticsRetentionDays: 999 })
    assert.deepEqual(reopened.refusal, { code: "validation", reason: "retentionRange", detail: null, message: "analyticsRetentionDays is outside 1..365.", status: 422, revision: 40, field: null })
  })

  /** The page's io, but the toasts kept whole, so a test presses their buttons as a reader does. */
  const withToasts = () => {
    const p = page()
    const toasts: Toast[] = []
    const dismissed: string[] = []
    p.qc.setQueryData(["settings"], { analyticsRetentionDays: 30, revision: 5 })
    const writes = writesFor(p.qc, { ...p.io, say: (toast) => { toasts.push(toast) }, dismiss: (id) => { dismissed.push(id) } })
    return { p, writes, toasts, dismissed }
  }
  /** Thirty seconds with no answer, on timers mocked since before the press. */
  const noAnswer = async () => {
    mock.timers.tick(30_000)
    await flush()
  }

  test("a draft whose form closes as its toast's Retry goes out, in one task, is held by that write and kept by its refusal", async () => {
    mock.timers.enable({ apis: ["setTimeout"] })
    const { p, writes, toasts } = withToasts()
    const release = writes.mount(control, null)
    writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    writes.pressForm(settings)
    await noAnswer()
    const retry = toasts.at(-1)?.button
    assert.equal(retry?.word, "Retry")
    release()
    writes.act(control, retry.act)
    await flush()
    assert.equal(p.answers.length, 7, "six attempts, then Retry's")
    p.answers[6]({ kind: "refused", revision: 40, status: 422, code: "validation", reason: "retentionRange", detail: null, message: "analyticsRetentionDays is outside 1..365." })
    await flush()
    assert.deepEqual(writes.form(settings).shown, { analyticsRetentionDays: 60 }, "the typing is where the toast points")
  })

  test("a draft whose form closes after a definite 429 as its toast's Retry goes out, in one task, is held by that write and kept by its refusal", async () => {
    const { p, writes, toasts } = withToasts()
    const release = writes.mount(control, null)
    writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    writes.pressForm(settings)
    p.answers[0]({ kind: "front", status: 429, retry: true })
    await flush()
    const retry = toasts.at(-1)?.button
    assert.equal(retry?.word, "Retry")
    release()
    writes.act(control, retry.act)
    await flush()
    assert.equal(p.answers.length, 2, "Retry's attempt")
    p.answers[1]({ kind: "refused", revision: 40, status: 422, code: "validation", reason: "retentionRange", detail: null, message: "analyticsRetentionDays is outside 1..365." })
    await flush()
    assert.deepEqual(writes.form(settings).shown, { analyticsRetentionDays: 60 }, "the typing is where the toast points")
  })

  test("a form closed while its write has no answer keeps its draft for when it opens again; one closed after a definite 429 drops it", async () => {
    mock.timers.enable({ apis: ["setTimeout"] })
    const unheard = withToasts()
    const release = unheard.writes.mount(control, null)
    unheard.writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    unheard.writes.pressForm(settings)
    await noAnswer()
    release()
    await flush()
    assert.deepEqual([unheard.writes.form(settings).shown, unheard.writes.form(settings).phase], [{ analyticsRetentionDays: 60 }, "unanswered"])
    unheard.writes.mount(control, null)
    assert.deepEqual(unheard.writes.form(settings).shown, { analyticsRetentionDays: 60 }, "there when it opens again")

    const refused = withToasts()
    const leave = refused.writes.mount(control, null)
    refused.writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    refused.writes.pressForm(settings)
    refused.p.answers[0]({ kind: "front", status: 429, retry: true })
    await flush()
    leave()
    await flush()
    assert.deepEqual(refused.writes.form(settings).shown, { analyticsRetentionDays: 30 }, "a 429 reached nothing: the typing goes as on any close")
  })

  test("a draft discarded while its no-answer toast offers Retry takes the toast with it, and nothing is sent after", async () => {
    mock.timers.enable({ apis: ["setTimeout"] })
    const { p, writes, toasts, dismissed } = withToasts()
    writes.mount(control, null)
    writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    writes.pressForm(settings)
    await noAnswer()
    const retry = toasts.at(-1)?.button
    assert.equal(retry?.word, "Retry")
    writes.discard(control, null)
    assert.deepEqual(dismissed.slice(-1), [control], "its toast goes")
    writes.act(control, retry.act)
    await flush()
    assert.equal(p.answers.length, 6, "a Retry pressed before the toast left sends nothing")
    assert.deepEqual(writes.form(settings).shown, { analyticsRetentionDays: 30 })
  })

  test("useForm draws what is stored under the touched fields", () => {
    const { p, writes } = fresh()
    writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    const Field = () => createElement("input", { value: useForm(settings).shown.analyticsRetentionDays, readOnly: true })
    assert.equal(renderToString(createElement(QueryClientProvider, { client: p.qc }, createElement(Field))), '<input readOnly="" value="60"/>')
  })

  test("holds something a reload would lose exactly while it differs from what is stored, a write is out or kept, or a component holds typing", async () => {
    const { p, writes } = fresh()
    const release = writes.mount(control, null)
    assert.equal(writes.unsaved(), false)
    writes.form(settings)
    writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    assert.equal(writes.unsaved(), true, "a pick not saved")
    writes.edit(settings.action, null, { analyticsRetentionDays: 30 })
    assert.equal(writes.unsaved(), false, "picked back")
    writes.edit(settings.action, null, { analyticsRetentionDays: 60 })
    release()
    await flush()
    assert.equal(writes.unsaved(), false, "left")

    p.qc.setQueryData(["state"], view(40))
    writes.press(box, { ticked: true }, { what: "Amsterdam" })
    assert.equal(writes.unsaved(), true, "a tick out")
    p.answers[0]({ kind: "front", status: 429, retry: true })
    await flush()
    assert.equal(writes.unsaved(), true, "kept for its Retry")
    writes.press(box, { ticked: true }, { what: "Amsterdam" })
    p.answers[1]({ kind: "yes", revision: 40, body: {} })
    await flush()
    assert.equal(writes.unsaved(), false, "landed")

    const held = [writes.hold(), writes.hold()]
    held[0]()
    assert.equal(writes.unsaved(), true)
    held[1]()
    assert.equal(writes.unsaved(), false)
  })
})
