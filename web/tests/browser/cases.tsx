/* The page's one store in a browser, against real React and real TanStack Query: what no test without a DOM can run,
 * such as StrictMode's effects and the query cache joining a read already out. tests/e2e/store.test.mjs runs each case
 * by name and fails with the error it throws. */
import { StrictMode, type ReactNode } from "react"
import { flushSync } from "react-dom"
import { createRoot } from "react-dom/client"
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query"
import { actions } from "@/lib/api"
import { keys } from "@/lib/keys"
import { refusalWords } from "@/lib/troubles"
import type { GroupId, LineageId, View } from "@/lib/schema"
import { useControl, useForm, writesFor } from "@/lib/use-write"
import { isPending, noAnswer, type Answer, type Form, type Write } from "@/lib/write"

const turn = () => new Promise((resolve) => setTimeout(resolve, 0))

async function until(check: () => boolean, what: string, ms = 2_000) {
  const end = performance.now() + ms
  while (!check()) {
    if (performance.now() > end) throw new Error(`timed out waiting for ${what}`)
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
}

function expect(holds: boolean, what: string) {
  if (!holds) throw new Error(what)
}

/** A page: its QueryClient as main.tsx makes it, its one store with writes the case answers by hand, and a StrictMode
 * root, as main.tsx renders, unless `strict` is false. Reads of the state are answered by hand too, and each toast's
 * button is kept for the case to press. */
function page(strict = true) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 2_000 } } })
  const attempts: { write: Write<unknown>; answer: (answer: Answer) => void }[] = []
  const buttons: (() => void)[] = []
  let keyCount = 0
  const writes = writesFor(qc, {
    attempt: (write) => new Promise((answer) => { attempts.push({ write, answer }) }),
    say: (_, act) => { buttons.push(act) }, dismiss: () => {}, signIn: () => {}, navigate: () => {},
    newId: () => `k${++keyCount}`,
  })
  const reads: { resolve: (view: View) => void; reject: (error: Error) => void }[] = []
  const readState = () => new Promise<View>((resolve, reject) => { reads.push({ resolve, reject }) })
  const container = document.createElement("div")
  document.body.append(container)
  const root = createRoot(container)
  return {
    qc, writes, attempts, reads, readState, container, buttons,
    render: (node: ReactNode) => {
      const provided = <QueryClientProvider client={qc}>{node}</QueryClientProvider>
      root.render(strict ? <StrictMode>{provided}</StrictMode> : provided)
    },
    close: () => { root.unmount(); container.remove(); qc.clear() },
  }
}

const view = (revision: number) => ({ revision }) as unknown as View
const box = actions.tick("eu" as GroupId, "l-ams" as LineageId)

type Settings = { analyticsRetentionDays: number }
const settingsForm: Form<Settings, Settings, unknown> = {
  action: actions.settings as unknown as Form<Settings, Settings, unknown>["action"], what: "Settings", blank: { analyticsRetentionDays: 0 },
  stored: (data) => {
    const s = data as unknown as (Settings & { revision: number }) | undefined
    return s ? { value: { analyticsRetentionDays: s.analyticsRetentionDays }, revision: s.revision } : null
  },
  body: (shown) => shown, print: () => null, shows: () => true,
}

/** The settings form's one field, with the refusal it draws. */
function Retention() {
  const form = useForm(settingsForm)
  return <input aria-label="Retention" readOnly value={form.shown.analyticsRetentionDays} data-refusal={form.refusal ? refusalWords(form.refusal) : ""} />
}

/** Presses `action` with the store's timers held, so the case ends the write at 30 s when it chooses. */
function pressHeld(press: () => void): { end: () => void } {
  const held: { run: () => void; ms: number }[] = []
  const real = window.setTimeout
  window.setTimeout = ((run: () => void, ms: number) => {
    held.push({ run, ms })
    return real(() => {}, 2 ** 31 - 1)
  }) as typeof window.setTimeout
  try {
    press()
  } finally {
    window.setTimeout = real
  }
  return { end: () => held.find((t) => t.ms === 30_000)?.run() }
}

/** A box that reads the state as the page does, with the store's control for it. */
function Box({ readState }: { readState: () => Promise<View> }) {
  useQuery({ queryKey: keys.state, queryFn: readState })
  const shown = useControl(box)
  return <button aria-busy={isPending(shown.phase)}>Amsterdam</button>
}

const cases: Record<string, (strict?: boolean) => Promise<void>> = {
  async strictModeKeepsARefusedDraft() {
    const p = page()
    p.qc.setQueryData(keys.settings, { analyticsRetentionDays: 30, checkForUpdates: true, revision: 5 })
    const control = settingsForm.action.control
    const release = p.writes.mount(control, null)
    p.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    p.writes.pressForm(settingsForm)
    release()
    p.attempts[0].answer({ kind: "refused", revision: 40, status: 422, code: "validation", reason: "retentionRange", detail: null, message: "analyticsRetentionDays is outside 1..365." })
    await turn()
    p.render(<Retention />)
    const input = () => p.container.querySelector("input")
    await until(() => input() !== null, "the form drawn")
    await turn()
    expect(input()?.value === "60", `the reopened form shows ${input()?.value}, not the 60 kept with its refusal`)
    expect(input()?.dataset.refusal === "1d to 365d", "the reopened form draws its refusal")
    p.render(null)
    await until(() => input() === null, "the form gone")
    await turn()
    expect(p.writes.form(settingsForm).shown.analyticsRetentionDays === 30, "a form closed with no write out drops its draft")
    p.close()
  },

  async closedAsItsRefusalLandsKeepsTheDraft() {
    const p = page()
    p.qc.setQueryData(keys.settings, { analyticsRetentionDays: 30, checkForUpdates: true, revision: 5 })
    const input = () => p.container.querySelector("input")
    flushSync(() => p.render(<Retention />))
    p.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    p.writes.pressForm(settingsForm)
    p.attempts[0].answer({ kind: "refused", revision: 40, status: 422, code: "validation", reason: "retentionRange", detail: null, message: "analyticsRetentionDays is outside 1..365." })
    await Promise.resolve()
    expect(p.writes.form(settingsForm).phase === "out", "the refusal is still on its way when the form closes")
    flushSync(() => p.render(null))
    await turn()
    const kept = p.writes.form(settingsForm)
    expect(kept.shown.analyticsRetentionDays === 60, `closed as its refusal landed, the draft shows ${kept.shown.analyticsRetentionDays}, not 60`)
    expect(kept.refusal?.reason === "retentionRange", "closed as its refusal landed, the refusal is kept")
    flushSync(() => p.render(<Retention />))
    await turn()
    expect(input()?.value === "60", `the reopened form shows ${input()?.value}, not the 60 kept with its refusal`)
    expect(input()?.dataset.refusal === "1d to 365d", "the reopened form draws its refusal")
    p.close()
  },

  async closedAsItsConflictLandsKeepsTheDraft() {
    const p = page()
    p.qc.setQueryData(keys.settings, { analyticsRetentionDays: 30, checkForUpdates: true, revision: 5 })
    const input = () => p.container.querySelector("input")
    flushSync(() => p.render(<Retention />))
    p.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    p.writes.pressForm(settingsForm)
    p.attempts[0].answer({ kind: "conflict", revision: 41, stored: 6 })
    await turn()
    expect(p.writes.form(settingsForm).phase === "out", "the conflict waits for the settings at 6 when the form closes")
    flushSync(() => p.render(null))
    p.qc.setQueryData(keys.settings, { analyticsRetentionDays: 45, checkForUpdates: true, revision: 6 })
    await turn()
    const kept = p.writes.form(settingsForm)
    expect(kept.phase !== "out", "the conflict ended on the settings at 6")
    expect(kept.shown.analyticsRetentionDays === 60 && kept.dirty, `closed as its conflict landed, the draft shows ${kept.shown.analyticsRetentionDays}, not 60 over the other tab's 45`)
    flushSync(() => p.render(<Retention />))
    await turn()
    expect(input()?.value === "60", `the reopened form shows ${input()?.value}, not the 60 kept through its conflict`)
    p.close()
  },

  async aClosedDraftsRetryAnsweredYesKeepsLaterTyping(strict) {
    const p = page(strict)
    p.qc.setQueryData(keys.settings, { analyticsRetentionDays: 30, checkForUpdates: true, revision: 5 })
    const input = () => p.container.querySelector("input")
    flushSync(() => p.render(<Retention />))
    p.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    const clock = pressHeld(() => p.writes.pressForm(settingsForm))
    p.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 90 })
    flushSync(() => p.render(null))
    await turn()
    clock.end()
    const kept = p.writes.form(settingsForm)
    expect(noAnswer(kept.phase) && kept.shown.analyticsRetentionDays === 90, `after no answer the closed draft shows ${kept.shown.analyticsRetentionDays}, not 90`)
    p.buttons.at(-1)?.()
    expect(p.attempts.length === 2 && p.attempts[1].write.key === p.attempts[0].write.key && p.attempts[1].write.bytes === p.attempts[0].write.bytes, "Retry sends the 60 again under its key")
    p.attempts[1].answer({ kind: "yes", revision: 41, body: { revision: 6 } })
    await turn()
    p.qc.setQueryData(keys.settings, { analyticsRetentionDays: 60, checkForUpdates: true, revision: 6 })
    await turn()
    expect(p.writes.form(settingsForm).phase !== "out", "Retry's yes settled on the settings at 6")
    flushSync(() => p.render(<Retention />))
    await turn()
    const reopened = p.writes.form(settingsForm)
    expect(input()?.value === "90" && reopened.dirty, `reopened after its Retry landed, the form shows ${input()?.value}${reopened.dirty ? "" : ", clean"}, not the 90 typed after the press`)
    p.close()
  },

  async aClosedDraftsRetryLeavingOnlyWhatIsStoredReleasesIt(strict) {
    const p = page(strict)
    p.qc.setQueryData(keys.settings, { analyticsRetentionDays: 30, checkForUpdates: true, revision: 5 })
    const input = () => p.container.querySelector("input")
    flushSync(() => p.render(<Retention />))
    p.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    const clock = pressHeld(() => p.writes.pressForm(settingsForm))
    p.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 90 })
    p.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    flushSync(() => p.render(null))
    await turn()
    clock.end()
    p.buttons.at(-1)?.()
    expect(p.attempts.length === 2 && p.attempts[1].write.key === p.attempts[0].write.key, "Retry sends the 60 again under its key")
    p.attempts[1].answer({ kind: "yes", revision: 41, body: { revision: 6 } })
    await turn()
    p.qc.setQueryData(keys.settings, { analyticsRetentionDays: 60, checkForUpdates: true, revision: 6 })
    await turn()
    expect(p.writes.form(settingsForm).phase !== "out", "Retry's yes settled on the settings at 6")
    p.qc.setQueryData(keys.settings, { analyticsRetentionDays: 45, checkForUpdates: true, revision: 7 })
    flushSync(() => p.render(<Retention />))
    await turn()
    const reopened = p.writes.form(settingsForm)
    expect(input()?.value === "45" && !reopened.dirty, `reopened after its Retry landed, the form shows ${input()?.value}${reopened.dirty ? ", dirty" : ""}, not the 45 another writer stored`)
    p.close()
  },

  async aYesJoiningTheFirstReadIsReadAgain() {
    const p = page()
    p.render(<Box readState={p.readState} />)
    await until(() => p.reads.length === 1, "the first read of the state")
    p.writes.press(box, { ticked: true }, { what: "Amsterdam" })
    p.attempts[0].answer({ kind: "yes", revision: 41, body: {} })
    await turn()
    p.reads[0].resolve(view(40))
    await until(() => p.reads.length === 2, "a read begun after the answer")
    expect(isPending(p.writes.shown(box.control).phase), "the view at 40 is older than the answer's 41")
    p.reads[1].resolve(view(41))
    await until(() => !isPending(p.writes.shown(box.control).phase), "the box settled on the view at 41")
    p.close()
  },

  async noAnswerJoiningTheFirstReadIsReadAgain() {
    const p = page()
    p.render(<Box readState={p.readState} />)
    await until(() => p.reads.length === 1, "the first read of the state")
    const clock = pressHeld(() => p.writes.press(box, { ticked: true }, { what: "Amsterdam" }))
    clock.end()
    expect(noAnswer(p.writes.shown(box.control).phase), "the write ended with no answer")
    p.reads[0].resolve(view(41))
    await until(() => p.reads.length === 2, "a read begun after the write ended")
    expect(isPending(p.writes.shown(box.control).phase), "the first read began before the write ended")
    p.reads[1].resolve(view(41))
    await until(() => !isPending(p.writes.shown(box.control).phase), "the box settled on the read begun after it ended")
    p.close()
  },

  async aFailedReadAfterAYesIsAskedAgain() {
    const p = page()
    p.render(<Box readState={p.readState} />)
    await until(() => p.reads.length === 1, "the first read of the state")
    p.reads[0].resolve(view(40))
    await until(() => p.qc.getQueryState(keys.state)?.status === "success", "the first read landed")
    p.writes.press(box, { ticked: true }, { what: "Amsterdam" })
    p.attempts[0].answer({ kind: "yes", revision: 41, body: {} })
    await until(() => p.reads.length === 2, "the read the answer asks for")
    p.reads[1].reject(new Error("no answer"))
    await turn()
    expect(p.reads.length === 2, "a failed read is asked again after a wait, not at once")
    await until(() => p.reads.length === 3, "the read asked again", 3_000)
    p.reads[2].resolve(view(41))
    await until(() => !isPending(p.writes.shown(box.control).phase), "the box settled")
    p.close()
  },
}

Object.assign(window, { cases })
