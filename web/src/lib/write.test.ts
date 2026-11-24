import assert from "node:assert/strict"
import { after, afterEach, beforeEach, describe, mock, test } from "node:test"
import type { GroupId, LineageId, SlotEdit, SlotId, SourceId, View } from "./schema.ts"
import { IDLE, NO_FLIGHT, READ_TIMEOUT_MS, REREAD_MS, answerOf, chased, flown, formConflict, isPending, moved, noAnswer, formDirty, step, toastOf, writeOf, type Action, type Answer, type Conflicted, type ControlKey, type Effect, type Ending, type Form, type Pending, type PressHow, type ReadName, type Reads, type Seq, type Write } from "./write.ts"
import { actions, closeModules, viewOf, world, type Fetch, type ModelDaemon } from "../../tests/write-world.ts"

const EU = "eu" as GroupId
const AMS = "l-ams" as LineageId
const PAR = "l-par" as LineageId
const tickPath = (server: LineageId) => `api/groups/eu/ticked/${server}`
const ticked = (w: ReturnType<typeof world>, server: LineageId) => viewOf(w.cached())?.items[tickPath(server)]?.value.ticked ?? false
const NO_CONNECTION: Answer = { kind: "unknown", status: 0 }

beforeEach(() => mock.timers.enable({ apis: ["setTimeout", "Date"] }))
afterEach(() => mock.timers.reset())
after(() => closeModules())

describe("the table of orderings", () => {
  test("an answer held while the reader clicks again: one request, the second press ignored, Pending until the read passes the answer", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    assert.equal(w.press(box, { ticked: true }), w.sent[0]?.write.key ?? "no write sent", "a press gives its write's key")
    assert.equal(w.press(box, { ticked: false }), null, "an ignored press gives none")
    assert.equal(w.sent.length, 1)
    assert.deepEqual(w.writes.shown(box.control).sent, { ticked: true })
    await w.land(0)
    assert.equal(isPending(w.writes.shown(box.control).phase), true, "answered at 41, the page holds 40")
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.equal(w.writes.shown(box.control).sent, null)
    assert.equal(ticked(w, AMS), true)
    assert.deepEqual(w.toasts(), [])
  })

  test("an answer held while another tab unticks: settled on the view past the answer, which shows the untick, with no toast", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    const answer = w.reach(0)
    w.daemon.other(tickPath(AMS), { ticked: false })
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), true)
    await answer()
    assert.equal(isPending(w.writes.shown(box.control).phase), false, "the view at 42 is past the answer's 41")
    assert.equal(ticked(w, AMS), false)
    assert.deepEqual(w.toasts(), [])
  })

  test("a retry whose first attempt landed: one key, two attempts, settled on the view at the first answer's revision", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    w.lose(0)
    await w.wait(1_000)
    assert.equal(w.sent.length, 2)
    assert.equal(w.sent[1].heal, true, "every resend rereads the session first")
    await w.land(1)
    await w.read()
    assert.deepEqual(w.keys(), ["k1", "k1"])
    assert.equal(w.daemon.commits.get("k1"), 1)
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.equal(ticked(w, AMS), true)
  })

  test("a retry whose first attempt never arrived: one key, settled once", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.refuse(0, NO_CONNECTION)
    await w.wait(1_000)
    await w.land(1)
    await w.read()
    await w.read()
    assert.deepEqual(w.keys(), ["k1", "k1"])
    assert.equal(w.daemon.commits.get("k1"), 1)
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.deepEqual(w.toasts(), [])
  })

  test("a replayed answer after another tab's change settles at once on the view the page holds, which shows the other tab's value", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    const answer = w.reach(0)
    for (let i = 0; i < 3; i++) w.daemon.other(tickPath(AMS), { ticked: i % 2 === 1 })
    await w.read()
    await answer()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.equal(ticked(w, AMS), false)
  })

  test("reads failing after a yes: Pending with no toast, settled by the first read at or past the answer", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.land(0)
    w.begin()
    w.begin()
    await w.wait(60_000)
    assert.equal(isPending(w.writes.shown(box.control).phase), true)
    assert.deepEqual(w.toasts(), [])
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
  })

  test("a read hanging for ever after a yes: still Pending, and no toast says otherwise", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.land(0)
    w.begin()
    await w.wait(120_000)
    assert.equal(isPending(w.writes.shown(box.control).phase), true)
    assert.deepEqual(w.toasts(), [])
  })

  test("a yes whose read is a second away: the control is slow once Pending a second, answered or not, and slow ends with it", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.land(0)
    assert.equal(w.writes.shown(box.control).slow, false, "answered at once, and waiting for its read")
    await w.wait(1_000)
    assert.deepEqual([isPending(w.writes.shown(box.control).phase), w.writes.shown(box.control).slow], [true, true], "still Pending a second after the press")
    await w.read()
    assert.deepEqual([isPending(w.writes.shown(box.control).phase), w.writes.shown(box.control).slow], [false, false])
    w.press(box, { ticked: false })
    await w.land(1)
    await w.read()
    await w.wait(1_000)
    assert.equal(w.writes.shown(box.control).slow, false, "a write settled inside its second never turns slow")
  })

  test("paused offline after a yes: a pause and a continue that bring old data settle nothing", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    const old = w.cached()
    w.press(box, { ticked: true })
    await w.land(0)
    w.begin()
    await w.begin().landWith(old)
    assert.equal(isPending(w.writes.shown(box.control).phase), true, "the data is still at 40")
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
  })

  test("unmounted and mounted again while out: the same record, the resend went, Pending shown on the new mount", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    const unmount = w.writes.subscribe(() => {})
    w.press(box, { ticked: true })
    unmount()
    await w.wait(1_000)
    assert.equal(w.sent.length, 2, "the resend went with no one watching")
    let heard = 0
    w.writes.subscribe(() => { heard++ })
    assert.deepEqual(w.writes.shown(box.control), { phase: "out", sent: { ticked: true }, way: null, slow: true, answer: null, refusal: null, landings: 0, saved: 0, made: null, kept: null })
    await w.land(1)
    await w.read()
    assert.ok(heard > 0)
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
  })

  test("UniFi signs the session out between attempts: 'session expired' with Sign in at once, and the control waits for a fresh read", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.refuse(0, NO_CONNECTION)
    await w.wait(1_000)
    await w.refuse(1, { kind: "token", status: 401 })
    assert.deepEqual(w.toasts(), ["EU · session expired · Sign in"])
    assert.equal(isPending(w.writes.shown(box.control).phase), true, "the first attempt may have landed")
    await w.wait(30_000)
    assert.equal(w.sent.length, 2, "nothing more is sent once UniFi refused the heal")
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.equal(ticked(w, AMS), false, "the view, unchanged")
    w.said[0].act()
    assert.equal(w.signedIn, 1)
  })

  test("a stale CSRF token heals at once: the session reread and a resend, and a yes settles with no toast", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.refuse(0, { kind: "token", status: 403 })
    assert.equal(w.sent.length, 2)
    assert.equal(w.sent[1].heal, true)
    assert.equal(w.sent[1].write.key, "k1")
    await w.land(1)
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.deepEqual(w.toasts(), [])
  })

  test("a 413 from in front is refused at once with its code, after one attempt", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.refuse(0, { kind: "front", status: 413, retry: false })
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.deepEqual(w.toasts(), ["EU · HTTP 413"])
    await w.wait(30_000)
    assert.equal(w.sent.length, 1)
  })

  test("a 429 from in front is refused at once, its toast's Retry resending under the same key", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.refuse(0, { kind: "front", status: 429, retry: true })
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.deepEqual(w.toasts(), ["EU · HTTP 429 · Retry"])
    w.said[0].act()
    assert.deepEqual(w.keys(), ["k1", "k1"])
    await w.land(1)
    await w.read()
    assert.equal(ticked(w, AMS), true)
  })

  test("a 502 or 503 answered every time: six attempts under one key, at 30 s one 'timed out' toast with Retry, which resends the same key", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    for (const [i, ms] of [1_000, 1_000, 2_000, 4_000, 7_000].entries()) {
      await w.refuse(i, { kind: "unknown", status: i % 2 === 0 ? 502 : 503 })
      await w.wait(ms)
    }
    assert.deepEqual(w.sent.map((s) => s.at), [0, 1_000, 2_000, 4_000, 8_000, 15_000])
    await w.wait(14_999)
    assert.deepEqual(w.toasts(), [])
    await w.wait(1)
    assert.deepEqual(w.toasts(), ["EU · timed out · Retry"])
    assert.equal(w.sent.every((s) => s.signal.aborted), true, "every attempt is aborted at 30 s")
    assert.deepEqual(w.asked, ["state"], "a fresh read is asked for")
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    w.said[0].act()
    assert.deepEqual(new Set(w.keys()), new Set(["k1"]))
    assert.equal(w.sent.length, 7)
  })

  test("a read that began before the 30 s end and lands after it settles nothing; the next one does", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    const early = w.begin()
    await w.wait(30_000)
    await early.land()
    assert.equal(isPending(w.writes.shown(box.control).phase), true, "its fetch began while the write was still out")
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
  })

  test("a refusal for an attempt already abandoned is dropped; the toast stays; a fresh read settles the control", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.wait(30_000)
    await w.refuse(2, { kind: "refused", revision: 40, status: 422, code: "validation", reason: "serverGone", detail: null, message: "That server is gone." })
    assert.deepEqual(w.toasts(), ["EU · timed out · Retry"])
    assert.equal(isPending(w.writes.shown(box.control).phase), true)
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
  })

  test("an answer to an attempt abandoned at 30 s, arriving once Retry's write is out, moves nothing: Retry waits for its own", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.wait(30_000)
    w.said[0].act()
    assert.equal(w.sent.length, 7)
    await w.refuse(2, { kind: "refused", revision: 40, status: 422, code: "validation", reason: "serverGone", detail: null, message: "That server is gone." })
    assert.deepEqual(w.toasts(), ["EU · timed out · Retry"])
    assert.equal(isPending(w.writes.shown(box.control).phase), true)
    assert.equal(noAnswer(w.writes.shown(box.control).phase), false, "Retry's write is out")
    await w.land(6)
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.equal(ticked(w, AMS), true)
  })

  test("an Exit pick answered after another tab saved the slot: 'changed to' theirs, 'Your value:' mine, and Keep mine sends my Exit over the stored slot, keeping the other tab's name, under a new key", async () => {
    const w = world((d) => d.items.set("api/slots/eu", { revision: 4, value: { name: "EU", exit: { kind: "group", group: "fast-eu" } } }))
    const slot = actions.slot("eu" as SlotId)
    const mine = { kind: "group", group: "steady" as GroupId } as const
    w.press(slot, { input: { name: "EU", exit: mine }, edit: "exit" }, {
      what: "EU", precondition: { kind: "replace", revision: 4 },
      conflict: (data): Conflicted<SlotEdit> | null => {
        const stored = viewOf(data)?.items["api/slots/eu"]
        if (!stored) return null
        return { kind: "control", theirs: String((stored.value.exit as { group: string }).group), mine: "steady", again: { pressed: { input: { name: String(stored.value.name), exit: mine }, edit: "exit" }, precondition: { kind: "replace", revision: stored.revision } } }
      },
    })
    w.daemon.other("api/slots/eu", { name: "Europe", exit: { kind: "group", group: "acme" } })
    await w.land(0)
    assert.deepEqual(w.toasts(), [], "nothing is said before the view shows what is stored")
    await w.read()
    assert.deepEqual(w.toasts(), ["EU · changed to acme · Your value: steady · Keep mine"])
    assert.equal(w.said[0].toast.stays, true)
    w.said[0].act()
    assert.equal(w.sent[1].write.key, "k2")
    assert.deepEqual(w.sent[1].write.precondition, { kind: "replace", revision: 5 })
    await w.land(1)
    await w.read()
    assert.deepEqual(viewOf(w.cached())?.items["api/slots/eu"]?.value, { name: "Europe", exit: mine })
  })

  test("a form's conflict: an untouched field shows the stored value, the toast names the touched field that moved with no button, and Save sends the edits over the new revision", async () => {
    const w = world()
    type D = { secondField: number; analyticsRetentionDays: number }
    const form: Form<D, D, unknown> = {
      action: actions.settings as Form<D, D, unknown>["action"], what: "Settings", blank: { secondField: 0, analyticsRetentionDays: 0 },
      stored: (data) => {
        const s = data as unknown as (D & { revision: number }) | undefined
        return s ? { value: { secondField: s.secondField, analyticsRetentionDays: s.analyticsRetentionDays }, revision: s.revision } : null
      },
      body: (shown) => ({ secondField: shown.secondField, analyticsRetentionDays: shown.analyticsRetentionDays }),
      print: (field, value) => (field === "analyticsRetentionDays" ? `Retention ${value}d` : `Second ${value}`),
      shows: () => false,
    }
    w.writes.edit(form.action, null, { analyticsRetentionDays: 60 })
    w.writes.pressForm(form)
    w.daemon.other("api/settings", { secondField: 3600, analyticsRetentionDays: 45 })
    await w.land(0)
    await w.read("settings")
    assert.deepEqual(w.toasts(), ["Settings · changed in another tab · Retention 45d"])
    assert.equal(w.said[0].toast.stays, true, "a form's conflict stays until its Save")
    const view = w.writes.form(form)
    assert.deepEqual(view.shown, { secondField: 3600, analyticsRetentionDays: 60 })
    assert.equal(view.dirty, true)
    assert.equal(view.phase, "idle")
    w.writes.pressForm(form)
    assert.ok(w.dismissed.includes(form.action.control), "Save dismisses the conflict's toast")
    assert.equal(w.sent[1].write.key, "k2")
    assert.deepEqual(w.sent[1].write.precondition, { kind: "replace", revision: 6 })
    assert.equal(w.sent[1].write.bytes, '{"secondField":3600,"analyticsRetentionDays":60}')
    await w.land(1)
    await w.read("settings")
    assert.equal(w.writes.form(form).dirty, false)
    assert.deepEqual(w.writes.form(form).shown, { secondField: 3600, analyticsRetentionDays: 60 })
  })

  test("a form's conflict that leaves no difference: another tab stored the reader's own value, so the write ends as a yes on the landmark, with no toast and the draft gone", async () => {
    const w = world()
    type D = { secondField: number; analyticsRetentionDays: number }
    const form: Form<D, D, unknown> = {
      action: actions.settings as Form<D, D, unknown>["action"], what: "Settings", blank: { secondField: 0, analyticsRetentionDays: 0 },
      stored: (data) => {
        const s = data as unknown as (D & { revision: number }) | undefined
        return s ? { value: { secondField: s.secondField, analyticsRetentionDays: s.analyticsRetentionDays }, revision: s.revision } : null
      },
      body: (shown) => ({ secondField: shown.secondField, analyticsRetentionDays: shown.analyticsRetentionDays }),
      print: (field, value) => (field === "analyticsRetentionDays" ? `Retention ${value}d` : `Second ${value}`),
      shows: () => false,
    }
    w.writes.edit(form.action, null, { analyticsRetentionDays: 60 })
    w.writes.pressForm(form)
    w.daemon.other("api/settings", { secondField: 3600, analyticsRetentionDays: 60 })
    await w.land(0)
    assert.equal(w.writes.form(form).phase === "out", true, "Pending until the view shows what is stored")
    await w.read("settings")
    assert.deepEqual(w.toasts(), [], "nothing the reader touched differs: no conflict to show")
    const view = w.writes.form(form)
    assert.equal(view.phase, "idle")
    assert.equal(view.dirty, false)
    assert.deepEqual([...view.touched], [], "the draft ended")
    assert.deepEqual(view.shown, { secondField: 3600, analyticsRetentionDays: 60 })
    assert.equal(w.sent.length, 1)
  })

  test("a field's value written with its keys in another order is the same value: the draft isn't dirty, and a conflict over it agrees", () => {
    const stored = { value: { refresh: { seconds: 43200, paused: false } }, revision: 2 }
    const draft = { touched: new Map([["refresh", { value: { paused: false, seconds: 43200 }, at: 1 as Seq }]]), since: 1 as Seq, way: null, creation: null }
    assert.equal(formDirty({ refresh: { seconds: 0, paused: false } }, stored, draft), false)
    assert.deepEqual(formConflict({ ...stored, value: { refresh: { seconds: 0, paused: false } } }, stored, { refresh: { paused: false, seconds: 43200 } }, ["refresh"], () => null), { kind: "agreed" })
  })

  test("two writes stamped the same millisecond, both refused: different keys, each control settled with its own toast", async () => {
    const w = world((d) => {
      d.refusing.set(tickPath(AMS), { status: 422, reason: "serverGone", message: "That server is gone." })
      d.refusing.set(tickPath(PAR), { status: 422, reason: "serverGone", message: "That server is gone." })
    })
    w.press(actions.tick(EU, AMS), { ticked: true }, { what: "Amsterdam" })
    w.press(actions.tick(EU, PAR), { ticked: true }, { what: "Paris" })
    assert.deepEqual(w.keys(), ["k1", "k2"])
    await w.land(1)
    assert.equal(isPending(w.writes.shown(actions.tick(EU, PAR).control).phase), false)
    assert.equal(isPending(w.writes.shown(actions.tick(EU, AMS).control).phase), true)
    await w.land(0)
    assert.deepEqual(w.toasts(), ["Paris · server gone", "Amsterdam · server gone"])
    assert.equal(isPending(w.writes.shown(actions.tick(EU, AMS).control).phase), false)
  })

  test("a 412 on a create is that create, landed earlier: settled on the landmark with no toast", async () => {
    const w = world()
    const add = actions.source("borealis" as SourceId)
    w.press(add, { name: "Borealis" }, { what: "Borealis", precondition: { kind: "create" } })
    w.daemon.items.set("api/sources/borealis", { revision: 1, value: { name: "Borealis" } })
    w.daemon.revision++
    await w.land(0)
    assert.equal(isPending(w.writes.shown(add.control).phase), true)
    await w.read()
    assert.equal(isPending(w.writes.shown(add.control).phase), false)
    assert.deepEqual(w.toasts(), [])
  })

  test("a tick's 422 settles at once on the view the page holds, in its reason's words", async () => {
    const w = world((d) => d.refusing.set(tickPath(AMS), { status: 422, reason: "serverGone", message: "That server is gone." }))
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.land(0)
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.deepEqual(w.toasts(), ["EU · server gone"])
    assert.equal(w.said[0].toast.stays, false, "with no button it leaves by itself")
  })
})

describe("a refusal the control draws over the view", () => {
  const AURORA = "aurora" as SourceId
  const referenced = (d: ModelDaemon) => {
    d.items.set("api/sources/aurora", { revision: 1, value: { name: "Aurora" } })
    d.refusing.set("api/sources/aurora", { status: 409, code: "referenced", reason: "inUse", message: "Aurora is used by EU." })
  }
  const drawn = { what: "Aurora", shows: (refusal: { code: string }) => refusal.code === "referenced" }

  test("a delete refused as referenced past the view the page holds: Pending with its refusal, a read landing older settles nothing nor does one failing, each followed by the next, and the view at the refusal's revision settles it with no toast", async () => {
    const w = world(referenced)
    const remove = actions.deleteSource(AURORA)
    const old = w.cached()
    w.daemon.other("api/slots/eu", { exit: { kind: "source", source: "aurora" } })
    w.press(remove, undefined, drawn)
    await w.land(0)
    assert.deepEqual(w.writes.shown(remove.control).refusal, { code: "referenced", reason: "inUse", detail: null, message: "Aurora is used by EU.", status: 409, revision: 41, field: null })
    assert.equal(isPending(w.writes.shown(remove.control).phase), true, "refused at 41, the page holds 40")
    await w.running()?.landWith(old)
    assert.equal(isPending(w.writes.shown(remove.control).phase), true, "a read that lands older settles nothing")
    assert.equal(w.running(), null)
    await w.wait(1_000)
    assert.ok(w.running() !== null, "the view is read again a second later")
    await w.running()?.fail()
    assert.equal(isPending(w.writes.shown(remove.control).phase), true, "a failed read settles nothing")
    await w.wait(2_000)
    await w.running()?.land()
    assert.equal(isPending(w.writes.shown(remove.control).phase), false, "the view at 41 holds what the refusal named")
    assert.equal(w.writes.shown(remove.control).refusal?.code, "referenced")
    assert.deepEqual(w.toasts(), [])
    assert.equal(w.daemon.commits.size, 0)
  })

  test("a delete refused as referenced at the view the page holds settles at once, and a refusal it doesn't draw is its toast at once", async () => {
    const w = world(referenced)
    const remove = actions.deleteSource(AURORA)
    w.press(remove, undefined, drawn)
    await w.land(0)
    assert.equal(isPending(w.writes.shown(remove.control).phase), false, "refused at 40, the page holds 40")
    assert.equal(w.writes.shown(remove.control).refusal?.code, "referenced")
    w.daemon.other("api/slots/eu", { exit: { kind: "source", source: "aurora" } })
    w.press(remove, undefined, { what: "Aurora" })
    await w.land(1)
    assert.equal(isPending(w.writes.shown(remove.control).phase), false, "a toast's refusal needs no view")
    assert.deepEqual(w.toasts(), ["Aurora · in use"])
  })

  test("a refusal its control stopped drawing while the view caught up, its form closed meanwhile, is its toast once the view reaches it", async () => {
    const w = world(referenced)
    const remove = actions.deleteSource(AURORA)
    let open = true
    w.daemon.other("api/slots/eu", { exit: { kind: "source", source: "aurora" } })
    w.press(remove, undefined, { what: "Aurora", shows: (refusal) => open && refusal.code === "referenced" })
    await w.land(0)
    assert.equal(isPending(w.writes.shown(remove.control).phase), true, "refused at 41, the page holds 40")
    assert.deepEqual(w.toasts(), [])
    open = false
    await w.running()?.land()
    assert.equal(isPending(w.writes.shown(remove.control).phase), false)
    assert.equal(w.writes.shown(remove.control).refusal, null, "no control draws it")
    assert.deepEqual(w.toasts(), ["Aurora · in use"], "the toast says it")
  })
})

describe("the ways into one control", () => {
  const AURORA = "aurora" as SourceId
  const PATH = "api/sources/aurora"
  const source = actions.source(AURORA)
  const remove = actions.deleteSource(AURORA)
  const refresh = actions.refresh(AURORA)
  const control = source.control
  type D = { url: string; userAgent: string }
  const settings: Form<D, D, unknown> = {
    action: source as Form<D, D, unknown>["action"], way: "save", what: "Aurora", blank: { url: "", userAgent: "" },
    stored: (data) => {
      const item = viewOf(data)?.items[PATH]
      return item ? { value: { url: "", userAgent: String(item.value.userAgent) }, revision: item.revision } : null
    },
    body: (shown) => shown, print: () => null, shows: () => true,
  }
  const stored = (d: ModelDaemon) => d.items.set(PATH, { revision: 1, value: { name: "Aurora", userAgent: "Happ" } })
  const rename = (w: ReturnType<typeof world>, revision = 1) => w.press(source, { name: "Borealis" }, { what: "Aurora", precondition: { kind: "replace", revision }, way: "rename" })
  /** The settings' Save of URL B, every attempt lost, and the read after its no answer: its toast's Retry, kept by the test. */
  async function saveUnheard(w: ReturnType<typeof world>) {
    w.writes.edit(source, "save", { url: "B" })
    w.writes.pressForm(settings)
    await w.wait(30_000)
    await w.read()
    const toast = w.said.at(-1)
    assert.equal(toast?.toast.button?.word, "Retry")
    assert.equal(w.writes.shown(control, "save").phase, "lost")
    return toast
  }

  test("another way's ending leaves a way's kept write: after a rename's 413, 429, 200, 412 or no answer, the settings' Retry sends their Save under its key", async () => {
    for (const ending of ["413", "429", "200", "412", "no answer"]) {
      const w = world(stored)
      const retry = await saveUnheard(w)
      const first = w.sent[0].write
      rename(w)
      const i = w.sent.length - 1
      if (ending === "413") await w.refuse(i, { kind: "front", status: 413, retry: false })
      if (ending === "429") await w.refuse(i, { kind: "front", status: 429, retry: true })
      if (ending === "200") await w.land(i)
      if (ending === "412") {
        w.daemon.other(PATH, { name: "Theirs" })
        await w.land(i)
      }
      if (ending === "no answer") await w.wait(30_000)
      await w.read()
      const save = w.writes.shown(control, "save")
      assert.equal(save.phase, "lost", `after the rename's ${ending}`)
      assert.deepEqual(save.kept, { url: "B", userAgent: "Happ" }, `after the rename's ${ending}`)
      assert.deepEqual([w.writes.form(settings).unheard, w.writes.form(settings).retries], [true, true], `after the rename's ${ending}`)
      const sent = w.sent.length
      ;(ending === "429" ? () => w.writes.retry(control, "save") : () => retry?.act())()
      const again = w.sent.at(-1)?.write
      assert.equal(w.sent.length, sent + 1)
      assert.deepEqual([again?.key, again?.bytes, again?.precondition], [first.key, first.bytes, first.precondition], `the Retry after the rename's ${ending}`)
    }
  })

  test("another way's press, answer or ending leaves the refusal a way's form draws; the form's own edit, discard and press clear it", async () => {
    for (const then of ["edit", "discard", "press"] as const) {
      const w = world((d) => {
        stored(d)
        d.refusing.set(PATH, { status: 422, reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." })
      })
      w.writes.edit(source, "save", { userAgent: "x".repeat(201) })
      w.writes.pressForm(settings)
      await w.land(0)
      assert.equal(w.writes.form(settings).refusal?.reason, "badUserAgent")
      w.daemon.refusing.delete(PATH)
      rename(w)
      await w.refuse(1, { kind: "front", status: 413, retry: false })
      w.press(remove, undefined, { what: "Aurora", way: "delete" })
      await w.refuse(2, { kind: "front", status: 429, retry: true })
      rename(w)
      await w.land(3)
      await w.read()
      assert.equal(w.writes.form(settings).refusal?.reason, "badUserAgent", "through a rename's 413, a Delete's 429 and a rename's landing")
      if (then === "edit") w.writes.edit(source, "save", { userAgent: "Clash" })
      if (then === "discard") w.writes.discard(control, "save")
      if (then === "press") w.writes.pressForm(settings)
      assert.equal(w.writes.form(settings).refusal, null, `the form's ${then} clears it`)
    }
  })

  test("a toast's Retry sends the write of the way that kept it, with that way's press", async () => {
    const w = world(stored)
    const save = await saveUnheard(w)
    rename(w)
    await w.refuse(6, { kind: "front", status: 429, retry: true })
    const renamed = w.said.at(-1)
    assert.equal(renamed?.toast.why, "HTTP 429", "the toast on screen is the rename's, the way that pressed last")
    renamed?.act()
    assert.deepEqual([w.sent.at(-1)?.write.key, w.writes.shown(control).way], [w.sent[6].write.key, "rename"])
    await w.refuse(7, { kind: "front", status: 429, retry: true })
    save?.act()
    assert.deepEqual([w.sent.at(-1)?.write.key, w.writes.shown(control).way], [w.sent[0].write.key, "save"], "the Save's held toast sends the Save, as the Save")
    await w.wait(30_000)
    await w.read()
    w.writes.retry(control, "save")
    assert.deepEqual([w.sent.at(-1)?.write.key, w.writes.shown(control).way], [w.sent[0].write.key, "save"], "and so does the form's own Retry")
  })

  test("discard drops its own way's kept write and refusal, and the toast only when its way pressed last", async () => {
    const w = world(stored)
    const save = await saveUnheard(w)
    rename(w)
    await w.refuse(6, { kind: "front", status: 429, retry: true })
    const renamed = w.said.at(-1)
    const dismissed = w.dismissed.length
    w.writes.discard(control, "save")
    assert.equal(w.dismissed.length, dismissed, "the rename's toast stays")
    assert.equal(w.writes.shown(control, "save").phase, "idle")
    const sent = w.sent.length
    save?.act()
    assert.equal(w.sent.length, sent, "the Save's Retry sends nothing")
    renamed?.act()
    assert.equal(w.sent.at(-1)?.write.key, w.sent[6].write.key, "the rename's Retry sends the rename")

    const v = world(stored)
    rename(v)
    await v.refuse(0, { kind: "front", status: 429, retry: true })
    await saveUnheard(v)
    v.writes.discard(control, "save")
    assert.deepEqual(v.dismissed.slice(-1), [control], "the Save pressed last, so its toast goes")
  })

  test("a form closed while its own way's write has no answer keeps its draft; closed after another way's no answer it lets it go", async () => {
    const w = world(stored)
    const release = w.writes.mount(control, "save")
    await saveUnheard(w)
    release()
    await w.flush()
    assert.equal(w.writes.form(settings).shown.url, "B", "the Save's own no answer")
    w.writes.discard(control, "save")

    const v = world(stored)
    const leave = v.writes.mount(control, "save")
    v.writes.edit(source, "save", { url: "B" })
    rename(v)
    await v.wait(30_000)
    await v.read()
    leave()
    await v.flush()
    assert.equal(v.writes.form(settings).shown.url, "", "a rename's no answer owes the draft nothing")
  })

  test("a Delete refused as referenced, pressed after the settings' Save had no answer, leaves that Save's kept write and the refusal under its field", async () => {
    const unheard = world(stored)
    await saveUnheard(unheard)
    unheard.daemon.refusing.set(PATH, { status: 409, code: "referenced", reason: "inUse", message: "Aurora is used by EU." })
    unheard.press(remove, undefined, { what: "Aurora", way: "delete", shows: (refusal) => refusal.code === "referenced" })
    await unheard.land(6)
    await unheard.read()
    assert.deepEqual([unheard.writes.shown(control, "delete").refusal?.code, unheard.writes.form(settings).unheard, unheard.writes.form(settings).retries], ["referenced", true, true])
    unheard.writes.retry(control, "save")
    assert.equal(unheard.sent.at(-1)?.write.key, unheard.sent[0].write.key, "the Save's Retry")

    const refused = world((d) => {
      stored(d)
      d.refusing.set(PATH, { status: 422, reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." })
    })
    refused.writes.edit(source, "save", { userAgent: "x".repeat(201) })
    refused.writes.pressForm(settings)
    await refused.land(0)
    refused.daemon.refusing.set(PATH, { status: 409, code: "referenced", reason: "inUse", message: "Aurora is used by EU." })
    refused.press(remove, undefined, { what: "Aurora", way: "delete", shows: (refusal) => refusal.code === "referenced" })
    await refused.land(1)
    await refused.read()
    assert.deepEqual([refused.writes.form(settings).refusal?.reason, refused.writes.shown(control, "delete").refusal?.code], ["badUserAgent", "referenced"], "each way's refusal its own")
  })

  test("a group's rule left after its Save's no answer keeps its typing and its Retry through a rename of the group refused in front", async () => {
    const GROUP = "api/groups/eu"
    const group = (actions as unknown as { group: (id: GroupId) => Action<Record<string, unknown>, unknown> }).group(EU)
    type R = { countries: string }
    const rule: Form<R, R, unknown> = {
      action: group as Form<R, R, unknown>["action"], way: "save", what: "EU", blank: { countries: "" },
      stored: (data) => {
        const item = viewOf(data)?.items[GROUP]
        return item ? { value: { countries: String(item.value.countries) }, revision: item.revision } : null
      },
      body: (shown) => shown, print: () => null, shows: () => false,
    }
    const w = world((d) => d.items.set(GROUP, { revision: 1, value: { name: "EU", countries: "" } }))
    const release = w.writes.mount(group.control, "save")
    w.writes.edit(group, "save", { countries: "FR" })
    w.writes.pressForm(rule)
    await w.wait(30_000)
    await w.read()
    release()
    await w.flush()
    w.press(group, { name: "Europe" }, { what: "EU", precondition: { kind: "replace", revision: 1 }, way: "rename" })
    await w.refuse(6, { kind: "front", status: 413, retry: false })
    w.writes.mount(group.control, "save")
    const back = w.writes.form(rule)
    assert.deepEqual([back.shown.countries, back.unheard], ["FR", true], "the rule as typed, with its no answer")
    w.writes.retry(group.control, "save")
    assert.equal(w.sent.at(-1)?.write.key, w.sent[0].write.key, "its Retry sends the rule's write")
  })

  test("a Retry of a form's write is sending whatever the draft holds since", async () => {
    const w = world(stored)
    await saveUnheard(w)
    w.writes.edit(source, "save", { userAgent: "Clash" })
    w.writes.retry(control, "save")
    assert.deepEqual([w.writes.form(settings).sending, w.writes.form(settings).way], [true, "save"], "an edit since")
    await w.wait(30_000)
    await w.read()
    w.writes.edit(source, "save", { userAgent: "Happ" })
    w.writes.retry(control, "save")
    assert.deepEqual([w.writes.form(settings).sending, w.writes.form(settings).way], [true, "save"], "an edit put back")
  })

  test("a form is unheard while its way keeps a write with no answer and none is out: not after a discard or a 429, again after another way's write ends", async () => {
    const w = world(stored)
    assert.equal(w.writes.form(settings).unheard, false)
    await saveUnheard(w)
    assert.equal(w.writes.form(settings).unheard, true)
    rename(w)
    assert.deepEqual([w.writes.form(settings).unheard, w.writes.form(settings).sending], [false, true], "the rename is out")
    await w.land(6)
    assert.deepEqual([w.writes.form(settings).unheard, w.writes.form(settings).sending], [false, false], "answered, the rename waits for the view")
    await w.read()
    assert.equal(w.writes.form(settings).unheard, true, "the rename ended")
    w.writes.discard(control, "save")
    assert.equal(w.writes.form(settings).unheard, false, "discarded")

    const v = world(stored)
    v.writes.edit(source, "save", { url: "B" })
    v.writes.pressForm(settings)
    await v.refuse(0, { kind: "front", status: 429, retry: true })
    assert.deepEqual([v.writes.form(settings).unheard, v.writes.shown(control, "save").kept !== null], [false, true], "a 429 reached nothing: kept for its toast, not unheard")
  })

  test("a form's write is sending from its press to its answer, and not while it waits for the view", async () => {
    const w = world(stored)
    w.writes.edit(source, "save", { url: "B" })
    w.writes.pressForm(settings)
    assert.equal(w.writes.form(settings).sending, true)
    await w.land(0)
    assert.deepEqual([w.writes.form(settings).sending, w.writes.form(settings).phase], [false, "out"])
    await w.read()
    assert.equal(w.writes.form(settings).sending, false)
  })

  test("a Save retries its write with no answer while the draft holds no edit since its press, through another writer's change and another way's press; any edit ends that", async () => {
    const w = world(stored)
    await saveUnheard(w)
    w.daemon.other(PATH, { userAgent: "Theirs" })
    await w.read()
    assert.deepEqual([w.writes.form(settings).shown.userAgent, w.writes.form(settings).retries], ["Theirs", true], "an untouched field follows the view")
    rename(w, 2)
    await w.refuse(6, { kind: "front", status: 413, retry: false })
    assert.equal(w.writes.form(settings).retries, true, "through a rename")
    w.writes.edit(source, "save", { url: "B" })
    assert.deepEqual([w.writes.form(settings).unheard, w.writes.form(settings).retries], [true, false], "the same URL typed again is an edit")
    w.writes.pressForm(settings)
    assert.deepEqual([w.sent.at(-1)?.write.key === w.sent[0].write.key, w.sent.at(-1)?.write.precondition], [false, { kind: "replace", revision: 2 }], "a new write over the view")
  })

  test("a press of the same request through another way after no answer goes under the kept key, and its landing forgets both", async () => {
    const w = world()
    w.press(refresh, {}, { what: "Aurora", way: "heading" })
    await w.wait(30_000)
    await w.read()
    assert.equal(w.writes.shown(refresh.control, "heading").phase, "lost")
    w.press(refresh, {}, { what: "Aurora", way: "card" })
    assert.equal(w.sent.at(-1)?.write.key, w.sent[0].write.key)
    await w.land(6)
    await w.read()
    assert.deepEqual([w.writes.shown(refresh.control, "heading").phase, w.writes.shown(refresh.control, "card").phase], ["idle", "idle"])
  })

  test("two ways keeping one write: the toast on screen sends it as the way that pressed last", async () => {
    const w = world()
    w.press(refresh, {}, { what: "Aurora", way: "heading" })
    await w.wait(30_000)
    await w.read()
    w.press(refresh, {}, { what: "Aurora", way: "card" })
    await w.wait(30_000)
    await w.read()
    w.said.at(-1)?.act()
    assert.deepEqual([w.sent.at(-1)?.write.key, w.writes.shown(refresh.control).way], [w.sent[0].write.key, "card"])
  })

  test("a press after another way's write landed is a new write, though its own way's last press of it got no answer", async () => {
    const w = world((d) => {
      stored(d)
      d.refusing.set(PATH, { status: 409, code: "referenced", reason: "inUse", message: "Aurora is used by EU." })
    })
    w.press(remove, undefined, { what: "Aurora", way: "delete" })
    w.lose(0)
    await w.wait(30_000)
    await w.read()
    assert.equal(w.writes.shown(control, "delete").phase, "lost")
    w.daemon.refusing.delete(PATH)
    rename(w)
    await w.land(6)
    await w.read()
    w.press(remove, undefined, { what: "Aurora", way: "delete" })
    assert.notEqual(w.sent.at(-1)?.write.key, w.sent[0].write.key, "not the refused Delete's key, whose 409 the daemon keeps")
    await w.land(7)
    await w.read()
    assert.equal(w.daemon.items.has(PATH), false)
  })

  test("a refused Delete leaves the settings' draft and kept Save; a landed one forgets both, so a source made again at its id inherits nothing", async () => {
    const w = world((d) => {
      stored(d)
      d.refusing.set(PATH, { status: 409, code: "referenced", reason: "inUse", message: "Aurora is used by EU." })
    })
    w.writes.mount(control, null)
    const save = await saveUnheard(w)
    w.press(remove, undefined, { what: "Aurora", way: "delete" })
    await w.land(6)
    await w.read()
    assert.deepEqual([w.writes.shown(control, "save").phase, w.writes.form(settings).shown.url], ["lost", "B"], "a refused Delete keeps both")
    w.daemon.refusing.delete(PATH)
    w.press(remove, undefined, { what: "Aurora", way: "delete" })
    await w.land(7)
    await w.read()
    w.daemon.other(PATH, { name: "Aurora", userAgent: "Clash" })
    await w.read()
    const again = w.writes.form(settings)
    assert.deepEqual([again.phase, again.shown, again.dirty, w.writes.shown(control, "save").kept], ["idle", { url: "", userAgent: "Clash" }, false, null])
    const sent = w.sent.length
    save?.act()
    assert.equal(w.sent.length, sent, "the deleted source's Retry sends nothing")
  })

  /** Another client deletes Aurora, unread by the page. */
  const deletedElsewhere = (w: ReturnType<typeof world>) => {
    w.daemon.items.delete(PATH)
    w.daemon.revision++
  }
  /** Another client makes Aurora again at its id, unread by the page. With no `addedAt`, the view can't tell it from the one before. */
  const madeAgain = (w: ReturnType<typeof world>, addedAt?: string) => {
    w.daemon.items.set(PATH, { revision: 2, value: { name: "Aurora", userAgent: "Clash", ...(addedAt && { addedAt }) } })
    w.daemon.revision++
  }
  /** The settings as a source made again at Aurora's id opens them: nothing of the Save before, typed or kept. */
  const clean = (w: ReturnType<typeof world>) => {
    const form = w.writes.form(settings)
    return [form.phase, form.shown.url, form.dirty, form.unheard, w.writes.shown(control, "save").kept]
  }
  const CLEAN = ["idle", "", false, false, null]

  test("a read that lists the source no more, or another made at its id, retires the settings' draft, kept Save and toast; a failed read or another writer's save of it leaves them", async () => {
    const w = world(stored)
    const retry = await saveUnheard(w)
    await w.begin().fail()
    w.daemon.other(PATH, { userAgent: "Clash" })
    await w.read()
    assert.deepEqual([w.writes.shown(control, "save").phase, w.writes.form(settings).shown.url], ["lost", "B"], "a failed read and another writer's save leave both")
    deletedElsewhere(w)
    const dismissed = w.dismissed.length
    await w.read()
    assert.deepEqual(clean(w), CLEAN, "the read that lists it no more")
    assert.deepEqual(w.dismissed.slice(dismissed), [control], "takes its toast")
    const sent = w.sent.length
    retry?.act()
    assert.equal(w.sent.length, sent, "whose Retry sends nothing")

    const v = world(stored)
    await saveUnheard(v)
    madeAgain(v, "later")
    await v.read()
    assert.deepEqual(clean(v), CLEAN, "a read that lists another made at its id")

    const typed = world(stored)
    typed.writes.edit(source, "save", { url: "B" })
    madeAgain(typed, "later")
    await typed.read()
    assert.deepEqual(clean(typed), CLEAN, "a draft never saved, typed over the one before")
  })

  test("a settings' Retry answered 404 retires the draft and kept Save, though the view then lists one made again at its id it can't tell apart", async () => {
    const w = world(stored)
    const retry = await saveUnheard(w)
    deletedElsewhere(w)
    retry?.act()
    await w.land(w.sent.length - 1)
    madeAgain(w)
    await w.running()?.land()
    assert.equal(w.toasts().at(-1), "Aurora · was deleted")
    assert.deepEqual(clean(w), CLEAN)
  })

  test("a landed Delete retires the settings' draft and kept Save, though the read after it lists one made again at its id it can't tell apart", async () => {
    const w = world(stored)
    const retry = await saveUnheard(w)
    w.press(remove, undefined, { what: "Aurora", way: "delete" })
    await w.land(w.sent.length - 1)
    madeAgain(w)
    await w.running()?.land()
    assert.deepEqual(clean(w), CLEAN)
    const sent = w.sent.length
    retry?.act()
    assert.equal(w.sent.length, sent, "the deleted source's Retry sends nothing")
  })

  test("a Save out when a read lists its source no more is left to its own ending, and its no answer retires its draft and Retry, saying nothing, before the read it asks for", async () => {
    const w = world(stored)
    w.writes.edit(source, "save", { url: "B" })
    w.writes.pressForm(settings)
    deletedElsewhere(w)
    await w.read()
    assert.equal(w.writes.form(settings).shown.url, "B", "out, it keeps its draft")
    const said = w.said.length
    await w.wait(30_000)
    assert.deepEqual(w.toasts().slice(said), [], "its no answer says nothing")
    assert.notEqual(w.running(), null, "the read its no answer asks for is held")
    assert.deepEqual(clean(w), ["unanswered", ...CLEAN.slice(1)])
    await w.running()?.land()
    assert.deepEqual(clean(w), CLEAN)
  })

  test("a Save out when a read lists another made at its id, its settings open or left, retires its draft and kept write at its own ending, its no answer or its 412, so a Save over the one made again sends nothing of it while the read after is held", async () => {
    for (const ending of ["no answer", "412", "no answer, left"] as const) {
      const w = world(stored)
      const release = w.writes.mount(control, "save")
      w.writes.edit(source, "save", { url: "B" })
      w.writes.pressForm(settings)
      deletedElsewhere(w)
      madeAgain(w, "later")
      await w.read()
      assert.equal(w.writes.form(settings).shown.url, "B", `${ending}: out, it keeps its draft`)
      if (ending === "no answer, left") {
        release()
        await w.flush()
      }
      const said = w.said.length
      if (ending === "412") await w.land(0)
      else await w.wait(30_000)
      assert.deepEqual(w.toasts().slice(said), [], `${ending}: its ending says nothing`)
      assert.notEqual(w.running(), null, `${ending}: the read its ending asks for is held`)
      if (ending === "no answer, left") w.writes.mount(control, "save")
      const form = w.writes.form(settings)
      assert.deepEqual([form.shown.url, form.dirty, form.unheard, w.writes.shown(control, "save").kept], ["", false, false, null], `${ending}: nothing of the Save before`)
      w.writes.edit(source, "save", { userAgent: "Stash" })
      w.writes.pressForm(settings)
      const sent = w.sent.at(-1)?.write
      assert.deepEqual([sent?.bytes, sent?.precondition, sent?.key === w.sent[0].write.key], [JSON.stringify({ url: "", userAgent: "Stash" }), { kind: "replace", revision: 2 }, false], `${ending}: the Save over the one made again`)
    }
  })

  test("a closed settings' Save or Retry answered after the view lists another made at its id: its no answer and its conflict say nothing, and every other answer, the daemon's or from in front of it, is said once for the source it was of, with no Retry, and nothing takes it", async () => {
    type Answering = (w: ReturnType<typeof world>, i: number) => Promise<void>
    const answers: [string, Answering, string[]][] = [
      ["the daemon's 422", async (w, i) => {
        w.daemon.refusing.set(PATH, { status: 422, reason: "unreadableText", message: "Unknown format." })
        await w.land(i)
      }, ["Aurora · unknown format"]],
      ["a proxy's 413", (w, i) => w.refuse(i, { kind: "front", status: 413, retry: false }), ["Aurora · HTTP 413"]],
      ["a proxy's 429", (w, i) => w.refuse(i, { kind: "front", status: 429, retry: true }), ["Aurora · HTTP 429"]],
      ["UniFi's 401, to the attempt sent again for a new session too", async (w, i) => {
        await w.refuse(i, { kind: "token", status: 401 })
        await w.refuse(i + 1, { kind: "token", status: 401 })
      }, ["Aurora · session expired · Sign in"]],
      ["the daemon's 503, then nothing for 30 s", async (w, i) => {
        await w.refuse(i, { kind: "unknown", status: 503 })
        await w.wait(30_000)
      }, []],
      ["the daemon's 412", (w, i) => w.land(i), []],
    ]
    for (const from of ["Save", "Retry"] as const) {
      for (const [name, answer, toasts] of answers) {
        const w = world(stored)
        if (from === "Save") {
          w.writes.edit(source, "save", { url: "B" })
          w.writes.pressForm(settings)
        } else (await saveUnheard(w))?.act()
        const i = w.sent.length - 1
        deletedElsewhere(w)
        madeAgain(w, "later")
        await w.read()
        const [said, dismissed] = [w.said.length, w.dismissed.length]
        await answer(w, i)
        await w.running()?.land()
        assert.deepEqual(w.toasts().slice(said), toasts, `${from}, ${name}: what is said`)
        assert.deepEqual(w.said.slice(said).map(({ toast }) => toast.stays), toasts.map((words) => words.endsWith("Sign in")), `${from}, ${name}: a toast with no button leaves by itself`)
        assert.deepEqual(w.dismissed.slice(dismissed), [], `${from}, ${name}: and nothing takes it`)
        assert.deepEqual([...clean(w), w.writes.form(settings).refusal], [...CLEAN, null], `${from}, ${name}: nothing of it is kept, its field error neither`)
      }
    }
  })

  test("settings opened again on a source made again keep what is typed there through the Save before's ending and the read that was held: after Cancel, or typed while that Save was out", async () => {
    const w = world(stored)
    const release = w.writes.mount(control, "save")
    w.writes.edit(source, "save", { url: "B" })
    w.writes.pressForm(settings)
    deletedElsewhere(w)
    madeAgain(w, "later")
    await w.read()
    await w.wait(30_000)
    const held = w.running()
    w.writes.discard(control, "save")
    release()
    await w.flush()
    w.writes.mount(control, "save")
    w.writes.edit(source, "save", { url: "D" })
    await held?.land()
    assert.deepEqual([w.writes.form(settings).shown.url, w.writes.form(settings).dirty], ["D", true], "typed after Cancel")

    const v = world(stored)
    v.writes.mount(control, "save")
    v.writes.edit(source, "save", { url: "B" })
    v.writes.pressForm(settings)
    deletedElsewhere(v)
    madeAgain(v, "later")
    await v.read()
    v.writes.discard(control, "save")
    v.writes.edit(source, "save", { url: "D" })
    await v.wait(30_000)
    assert.deepEqual([v.writes.form(settings).shown.url, v.writes.shown(control, "save").kept], ["D", null], "typed over the one made again while the Save before was out: its ending takes its Retry and leaves the typing")
    await v.running()?.land()
    assert.deepEqual([v.writes.form(settings).shown.url, v.writes.form(settings).dirty], ["D", true], "and the read after")
  })

  test("a group's Save answered 412, whose group the view then lists no more, says was deleted and retires the rule's draft", async () => {
    const GROUP = "api/groups/eu"
    const group = (actions as unknown as { group: (id: GroupId) => Action<Record<string, unknown>, unknown> }).group(EU)
    type R = { countries: string }
    const rule: Form<R, R, unknown> = {
      action: group as Form<R, R, unknown>["action"], way: "save", what: "EU", blank: { countries: "" },
      stored: (data) => {
        const item = viewOf(data)?.items[GROUP]
        return item ? { value: { countries: String(item.value.countries) }, revision: item.revision } : null
      },
      body: (shown) => shown, print: () => null, shows: () => false,
    }
    const w = world((d) => d.items.set(GROUP, { revision: 1, value: { name: "EU", countries: "" } }))
    w.writes.edit(group, "save", { countries: "FR" })
    w.daemon.other(GROUP, { countries: "DE" })
    w.writes.pressForm(rule)
    await w.land(0)
    w.daemon.items.delete(GROUP)
    w.daemon.revision++
    await w.running()?.land()
    assert.equal(w.toasts().at(-1), "EU · was deleted")
    assert.deepEqual([w.writes.form(rule).shown.countries, w.writes.form(rule).dirty], ["", false])
  })
})

describe("a refusal's field, fixed by its press", () => {
  const PATH = "api/sources/aurora"
  const source = actions.source("aurora" as SourceId)
  type D = { refresh: number; userAgent: string }
  /** A source's settings with two fields, drawing a refusal under the one field a Save changed, as the dialog's do. */
  const settings: Form<D, D, unknown> = {
    action: source as Form<D, D, unknown>["action"], way: "save", what: "Aurora", blank: { refresh: 0, userAgent: "" },
    stored: (data) => {
      const item = viewOf(data)?.items[PATH]
      return item ? { value: { refresh: Number(item.value.refresh), userAgent: String(item.value.userAgent) }, revision: item.revision } : null
    },
    body: (shown) => shown, print: () => null,
    shows: (_, changed) => changed.size === 1,
    fieldOf: (_, changed) => (changed.size === 1 ? [...changed][0] : undefined),
  }
  const refusing = (d: ModelDaemon) => {
    d.items.set(PATH, { revision: 1, value: { refresh: 3, userAgent: "Happ" } })
    d.refusing.set(PATH, { status: 422, reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." })
  }
  /** A Save of a bad User-Agent alone, its refusal held while `meanwhile` runs, then delivered. */
  async function refused(meanwhile: (w: ReturnType<typeof world>) => Promise<void>) {
    const w = world(refusing)
    w.writes.mount(source.control, "save")
    w.writes.edit(source, "save", { userAgent: "x".repeat(201) })
    w.writes.pressForm(settings)
    const answer = w.reach(0)
    await meanwhile(w)
    await answer()
    return w
  }

  test("a refusal of a form's press is about the one field it changed, whatever the view does after", async () => {
    const w = await refused(async (v) => {
      v.daemon.refusing.delete(PATH)
      v.daemon.other(PATH, { refresh: 12 })
      await v.read()
    })
    const form = w.writes.form(settings)
    assert.deepEqual([form.refusal?.reason, form.refusal?.field, [...form.changed]], ["badUserAgent", "userAgent", ["userAgent"]])
    assert.deepEqual(w.toasts(), [])
  })

  test("a drawn refusal that comes while its form is closed is said at once and kept for the form, and the view reaching it says nothing more", async () => {
    const w = world(refusing)
    const release = w.writes.mount(source.control, "save")
    w.writes.edit(source, "save", { userAgent: "x".repeat(201) })
    w.writes.pressForm(settings)
    release()
    w.daemon.other("api/slots/eu", { name: "EU later" })
    await w.land(0)
    assert.deepEqual(w.toasts(), ["Aurora · bad User-Agent"], "said as it came, the view still behind it")
    assert.equal(w.writes.form(settings).phase, "out", "waiting for the view")
    await w.read()
    w.writes.mount(source.control, "save")
    assert.deepEqual([w.toasts().length, w.writes.form(settings).refusal?.field], [1, "userAgent"], "once, and drawn when the form opens again")
  })

  test("a refusal said while its form was closed is said no more when the form, opened again, stops holding it before the view reaches it", async () => {
    const w = world(refusing)
    const release = w.writes.mount(source.control, "save")
    w.writes.edit(source, "save", { userAgent: "x".repeat(201) })
    w.writes.pressForm(settings)
    release()
    w.daemon.other("api/slots/eu", { name: "EU later" })
    await w.land(0)
    w.writes.mount(source.control, "save")
    w.writes.edit(source, "save", { userAgent: "Happ" })
    await w.read()
    assert.deepEqual([w.toasts(), w.writes.form(settings).refusal], [["Aurora · bad User-Agent"], null])
  })

  test("a drawn refusal that comes while its form is open is said when the view reaches it only if the form no longer holds what was refused", async () => {
    const w = world(refusing)
    w.writes.mount(source.control, "save")
    w.writes.edit(source, "save", { userAgent: "x".repeat(201) })
    w.writes.pressForm(settings)
    w.daemon.other("api/slots/eu", { name: "EU later" })
    await w.land(0)
    assert.deepEqual(w.toasts(), [], "drawn, not said")
    w.writes.edit(source, "save", { userAgent: "Happ" })
    await w.read()
    assert.deepEqual(w.toasts(), ["Aurora · bad User-Agent"])
  })

  test("a refusal stands while its field says what was sent, though a field the press didn't change moved by the view or by an edit", async () => {
    const byView = await refused(async (v) => {
      v.daemon.other(PATH, { refresh: 12 })
      await v.read()
    })
    assert.equal(byView.writes.form(settings).refusal?.field, "userAgent", "Refresh moved by another writer")
    const byEdit = await refused(async (v) => { v.writes.edit(source, "save", { refresh: 12 }) })
    assert.equal(byEdit.writes.form(settings).refusal?.field, "userAgent", "Refresh picked after the press")
    const putBack = await refused(async (v) => { v.writes.edit(source, "save", { userAgent: "Happ" }) })
    assert.deepEqual([putBack.writes.form(settings).refusal, putBack.toasts()], [null, ["Aurora · bad User-Agent"]], "the User-Agent put back: the refusal is of what it no longer says")
  })
})

describe("the cases from the reviews of rows 1 and 2", () => {
  test("a read of the state that never ends after a tick's yes: the box stays Pending, and the next read past the answer settles it", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.land(0)
    w.begin()
    await w.wait(30_000)
    assert.equal(isPending(w.writes.shown(box.control).phase), true)
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.equal(ticked(w, AMS), true)
  })

  test("a retry refused at UniFi's login after the first attempt landed, every read refused: the box stays Pending on the tick, never the old view, until a read shows the daemon's", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    w.lose(0)
    await w.wait(1_000)
    await w.refuse(1, { kind: "token", status: 401 })
    w.begin()
    w.begin()
    await w.wait(60_000)
    assert.deepEqual(w.writes.shown(box.control).sent, { ticked: true })
    assert.equal(isPending(w.writes.shown(box.control).phase), true)
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.equal(ticked(w, AMS), true, "the daemon holds it ticked, and so does the box")
  })

  test("going offline after a write's yes: a read that brings the old view settles nothing", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    const old = w.cached()
    w.press(box, { ticked: true })
    await w.land(0)
    await w.begin().landWith(old)
    await w.begin().landWith(old)
    assert.equal(isPending(w.writes.shown(box.control).phase), true)
    await w.read()
    assert.equal(ticked(w, AMS), true)
  })

  test("a retry whose first attempt never reached the daemon lands over another tab's newer change: settled on the view that shows it", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.refuse(0, NO_CONNECTION)
    w.daemon.other(tickPath(AMS), { ticked: false })
    await w.read()
    await w.wait(1_000)
    await w.land(1)
    assert.equal(isPending(w.writes.shown(box.control).phase), true, "answered at 42, the page holds 41")
    await w.read()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.equal(ticked(w, AMS), true)
    assert.deepEqual(w.toasts(), [])
  })

  test("after Retry stores an agent and the state reads fail: Retry stays Pending on the sent agent and takes no second press", async () => {
    const w = world((d) => d.items.set("api/sources/aurora/refresh", { revision: 1, value: { userAgent: "Happ/3.0.0" } }))
    const retry = actions.refresh("aurora" as SourceId)
    w.press(retry, { userAgent: "Clash" }, { what: "Aurora" })
    await w.land(0)
    w.begin()
    w.begin()
    assert.deepEqual(w.writes.shown(retry.control).sent, { userAgent: "Clash" })
    w.press(retry, { userAgent: "Happ/3.0.0" }, { what: "Aurora" })
    assert.equal(w.sent.length, 1)
    await w.read()
    assert.equal(isPending(w.writes.shown(retry.control).phase), false)
  })

  test("with the state read held, a 422 to Retry is said at once and Retry takes a press again", async () => {
    const w = world((d) => d.refusing.set("api/sources/aurora/refresh", { status: 422, reason: "badUserAgent", message: "A User-Agent is one line of at most 200 characters." }))
    const retry = actions.refresh("aurora" as SourceId)
    w.begin()
    w.press(retry, { userAgent: "x".repeat(201) }, { what: "Aurora" })
    await w.land(0)
    assert.deepEqual(w.toasts(), ["Aurora · bad User-Agent"])
    assert.equal(isPending(w.writes.shown(retry.control).phase), false)
    w.press(retry, { userAgent: "Clash" }, { what: "Aurora" })
    assert.equal(w.sent.length, 2)
  })

  test("two Exit picks stamped the same millisecond, one refused: the refused one settles, the other stays Pending for its own answer", async () => {
    const w = world((d) => {
      d.items.set("api/slots/eu", { revision: 1, value: { name: "EU" } })
      d.items.set("api/slots/lab", { revision: 1, value: { name: "Lab" } })
      d.refusing.set("api/slots/eu", { status: 422, reason: "groupEmpty", message: "That group has no servers. Change its filter first." })
    })
    const eu = actions.slot("eu" as SlotId)
    const lab = actions.slot("lab" as SlotId)
    const exit = { kind: "group", group: "empty" as GroupId } as const
    w.press(eu, { input: { name: "EU", exit }, edit: "exit" }, { what: "EU", precondition: { kind: "replace", revision: 1 } })
    w.press(lab, { input: { name: "Lab", exit }, edit: "exit" }, { what: "Lab", precondition: { kind: "replace", revision: 1 } })
    await w.land(0)
    assert.equal(isPending(w.writes.shown(eu.control).phase), false)
    assert.equal(isPending(w.writes.shown(lab.control).phase), true)
    await w.land(1)
    await w.read()
    assert.equal(isPending(w.writes.shown(lab.control).phase), false)
    assert.deepEqual(w.toasts(), ["EU · no servers"])
  })
})

type Settings = { secondField: number; analyticsRetentionDays: number }
const settingsForm: Form<Settings, Settings, unknown> = {
  action: actions.settings as Form<Settings, Settings, unknown>["action"], what: "Settings", blank: { secondField: 0, analyticsRetentionDays: 0 },
  stored: (data) => {
    const s = data as unknown as (Settings & { revision: number }) | undefined
    return s ? { value: { secondField: s.secondField, analyticsRetentionDays: s.analyticsRetentionDays }, revision: s.revision } : null
  },
  body: (shown) => ({ secondField: shown.secondField, analyticsRetentionDays: shown.analyticsRetentionDays }),
  print: (field, value) => (field === "analyticsRetentionDays" ? `Retention ${value}d` : `Second ${value}`),
  shows: () => false,
}

describe("the faults the foundation's review reproduced", () => {
  test("Retry after no answer sends the edit it held, and its yes keeps what was typed since", async () => {
    const w = world()
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    w.writes.pressForm(settingsForm)
    await w.wait(30_000)
    assert.deepEqual(w.toasts(), ["Settings · timed out · Retry"])
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 90 })
    w.said[0].act()
    const retried = w.sent.at(-1)
    assert.equal(retried?.write.key, "k1")
    assert.equal(retried?.write.bytes, '{"secondField":21600,"analyticsRetentionDays":60}', "Retry sends the bytes the toast named")
    await w.land(w.sent.length - 1)
    await w.read("settings")
    const form = w.writes.form(settingsForm)
    assert.equal(form.phase, "idle")
    assert.deepEqual(form.shown, { secondField: 21600, analyticsRetentionDays: 90 }, "the 90 typed after the write was made stays")
    assert.equal(form.dirty, true)
  })

  test("a yes drops the edits its write held and keeps a field typed while it was out", async () => {
    const w = world()
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    w.writes.pressForm(settingsForm)
    w.writes.edit(settingsForm.action, null, { secondField: 3600 })
    await w.land(0)
    await w.read("settings")
    const form = w.writes.form(settingsForm)
    assert.deepEqual([...form.touched], ["secondField"])
    assert.deepEqual(form.shown, { secondField: 3600, analyticsRetentionDays: 60 })
    assert.equal(form.dirty, true)
  })

  test("a yes whose read joined the page's first read, which lands older, is read again at once", async () => {
    const w = world(() => {}, { firstRead: true })
    const box = actions.tick(EU, AMS)
    const first = w.running()
    const before = w.daemon.view("state")
    w.press(box, { ticked: true })
    await w.land(0)
    assert.deepEqual(w.asked, ["state"])
    assert.equal(w.running(), first, "with nothing cached, the ask joined the first read")
    await first?.landWith(before)
    assert.equal(isPending(w.writes.shown(box.control).phase), true, "the view at 40 is older than the answer's 41")
    const again = w.running()
    assert.ok(again !== null && again !== first, "a read begun after the answer is out")
    await again.land()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
    assert.equal(ticked(w, AMS), true)
  })

  test("a write ended with no answer, whose read joined one begun before it ended, is read again at once", async () => {
    const w = world(() => {}, { firstRead: true })
    const box = actions.tick(EU, AMS)
    const first = w.running()
    w.press(box, { ticked: true })
    await w.wait(30_000)
    assert.deepEqual(w.toasts(), ["EU · timed out · Retry"])
    assert.equal(w.running(), first, "with nothing cached, the ask joined the first read")
    await first?.land()
    assert.equal(isPending(w.writes.shown(box.control).phase), true, "the first read began before the write ended")
    const again = w.running()
    assert.ok(again !== null && again !== first, "a read begun after the end is out")
    await again.land()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
  })

  test("a view that stays behind a yes is read at once, then 1, 2, 4 and 8 s after each read ends, then every 10 s, one read at a time", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    const old = w.cached()
    w.press(box, { ticked: true })
    await w.land(0)
    const begun = [Date.now()]
    await w.running()?.landWith(old)
    while (Date.now() < 60_000) {
      await w.wait(500)
      if (w.asked.length > begun.length) {
        begun.push(Date.now())
        await w.running()?.fail()
      }
    }
    assert.deepEqual(begun, [0, 1_000, 3_000, 7_000, 15_000, 25_000, 35_000, 45_000, 55_000])
    assert.equal(isPending(w.writes.shown(box.control).phase), true)
    assert.deepEqual(w.toasts(), [], "a yes is never called no answer")
    await w.wait(5_000)
    const hanging = w.running()
    assert.ok(hanging !== null, "the read asked at 65 s")
    await w.wait(30_000)
    assert.equal(w.asked.length, begun.length + 1, "nothing more is asked while that read is out")
    await hanging.land()
    assert.equal(isPending(w.writes.shown(box.control).phase), false)
  })
})

describe("a landing's remainder", () => {
  const control = settingsForm.action.control
  const form = (w: ReturnType<typeof world>) => w.writes.form(settingsForm)
  const saved = (w: ReturnType<typeof world>) => w.writes.shown(control).saved
  /** Retention 60 saved, every attempt lost after the first reached the daemon, and the read after its no answer. */
  async function unheard(w: ReturnType<typeof world>) {
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    w.writes.pressForm(settingsForm)
    w.lose(0)
    await w.wait(30_000)
  }

  test("every landing counts, with or without a draft, and a box's moves no saved count", async () => {
    const w = world()
    const box = actions.tick(EU, AMS)
    w.press(box, { ticked: true })
    await w.land(0)
    await w.read()
    assert.deepEqual([w.writes.shown(box.control).landings, w.writes.shown(box.control).saved], [1, 0])
  })

  test("a landing drops only its own stamps; one that leaves nothing, or only fields that hold what is stored, is counted once and leaves that remainder", async () => {
    const w = world()
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    w.writes.pressForm(settingsForm)
    w.writes.edit(settingsForm.action, null, { secondField: 3600 })
    w.writes.edit(settingsForm.action, null, { secondField: 21600 })
    await w.land(0)
    await w.read("settings")
    assert.deepEqual([saved(w), [...form(w).touched], form(w).dirty], [1, ["secondField"], false])
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 90 })
    w.writes.pressForm(settingsForm)
    await w.land(1)
    await w.read("settings")
    assert.deepEqual([saved(w), [...form(w).touched]], [2, []])
  })

  test("a landing of a write from a discarded draft, of a rename, or one that leaves a difference counts no saved", async () => {
    const discarded = world()
    discarded.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    discarded.writes.pressForm(settingsForm)
    discarded.writes.discard(control, null)
    discarded.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    await discarded.land(0)
    await discarded.read("settings")
    assert.deepEqual([saved(discarded), form(discarded).shown.analyticsRetentionDays], [0, 60], "the draft typed after the discard is no write's")

    const renamed = world((d) => d.items.set("api/sources/aurora", { revision: 1, value: { name: "Aurora" } }))
    const source = actions.source("aurora" as SourceId)
    renamed.writes.edit(source, "save", { url: "B" })
    renamed.press(source, { name: "Borealis" }, { what: "Aurora", precondition: { kind: "replace", revision: 1 }, way: "rename" })
    await renamed.land(0)
    await renamed.read()
    assert.deepEqual([renamed.writes.shown(source.control).landings, renamed.writes.shown(source.control).saved], [1, 0], "a rename lands beside the draft")

    const differs = world()
    differs.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    differs.writes.pressForm(settingsForm)
    differs.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 90 })
    await differs.land(0)
    await differs.read("settings")
    assert.deepEqual([saved(differs), form(differs).shown.analyticsRetentionDays, form(differs).dirty], [0, 90, true])
  })

  test("in a held place a remainder that holds only what is stored goes, and one that differs is kept for the form", async () => {
    for (const [then, left] of [[60, []], [90, [90]]] as const) {
      const w = world()
      const release = w.writes.mount(control, null)
      w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
      w.writes.pressForm(settingsForm)
      w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 90 })
      w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: then })
      release()
      await w.land(0)
      await w.read("settings")
      w.daemon.other("api/settings", { analyticsRetentionDays: 45 })
      await w.read("settings")
      w.writes.mount(control, null)
      assert.deepEqual([...form(w).touched].map(() => form(w).shown.analyticsRetentionDays), left, `typed ${then} after the press`)
    }
  })

  test("a replayed landing against a later view keeps a re-pick that differs from it", async () => {
    const w = world()
    await unheard(w)
    w.daemon.other("api/settings", { analyticsRetentionDays: 45 })
    await w.read("settings")
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 90 })
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    w.said.at(-1)?.act()
    await w.land(6)
    assert.equal(form(w).phase, "idle", "the replay's revision is one the view holds")
    assert.deepEqual([form(w).shown.analyticsRetentionDays, form(w).dirty, saved(w)], [60, true, 0])
  })

  test("a landing that leaves only an edit put back is counted, and that remainder stays", async () => {
    const w = world()
    await unheard(w)
    await w.read("settings")
    w.writes.edit(settingsForm.action, null, { secondField: 3600 })
    w.writes.edit(settingsForm.action, null, { secondField: 21600 })
    w.said.at(-1)?.act()
    await w.land(6)
    assert.deepEqual([saved(w), [...form(w).touched], form(w).dirty], [1, ["secondField"], false])
  })

  test("a replayed landing leaves an open Retention's re-pick, which another writer's later change shows dirty", async () => {
    const w = world()
    w.writes.mount(control, null)
    await unheard(w)
    await w.read("settings")
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 7 })
    w.writes.edit(settingsForm.action, null, { analyticsRetentionDays: 60 })
    w.said.at(-1)?.act()
    await w.land(6)
    assert.deepEqual([form(w).shown.analyticsRetentionDays, form(w).dirty, saved(w)], [60, false, 1])
    w.daemon.other("api/settings", { analyticsRetentionDays: 7 })
    await w.read("settings")
    assert.deepEqual([form(w).shown.analyticsRetentionDays, form(w).dirty], [60, true], "the open form keeps the re-pick, Save enabled")
  })
})

describe("a draft's place", () => {
  const at = 3 as Seq
  test("a close owing no answer is kept; kept then open is open; kept then sent is held; held then ended is kept with a rest and gone without", () => {
    assert.deepEqual(moved({ kind: "kept" }, { kind: "open" }), { kind: "open", forms: 1 })
    assert.deepEqual(moved({ kind: "kept" }, { kind: "sent" }), { kind: "held" })
    assert.deepEqual(moved({ kind: "held" }, { kind: "ended", rest: true }), { kind: "kept" })
    assert.equal(moved({ kind: "held" }, { kind: "ended", rest: false }), null)
    assert.deepEqual(moved({ kind: "open", forms: 1 }, { kind: "close", owes: "out", at }), { kind: "held" })
    assert.deepEqual(moved({ kind: "open", forms: 1 }, { kind: "close", owes: "kept", at }), { kind: "kept" }, "closed while its way's write has no answer")
    assert.deepEqual(moved({ kind: "open", forms: 1 }, { kind: "close", owes: null, at }), { kind: "leaving", at })
  })
})

describe("a create, at an id the store picks", () => {
  /** A create of the model daemon's, found landed where its view holds an item under api/groups. */
  const make: Action<{ name: string }, unknown> = {
    control: "group:new" as ControlKey, method: "PUT", path: "api/groups", body: (pressed) => pressed, settles: "state", answer: undefined as never,
    creates: { listed: (data) => Object.keys(viewOf(data)?.items ?? {}).filter((path) => path.startsWith("api/groups/")).map((path) => ({ id: path.split("/").at(-1) as string })) },
  }
  const how: PressHow<{ name: string }> = { what: "New group", precondition: { kind: "create" } }
  /** Presses `name`, which reaches the daemon or not, and gets no answer: its read at 30 s lands. */
  async function unheard(w: ReturnType<typeof world>, name: string, reaches: boolean) {
    const from = w.sent.length
    w.press(make, { name }, how)
    if (reaches) w.lose(from)
    await w.wait(30_000)
    await w.running()?.land()
  }

  test("a press of what a lost create sent goes to its id under its key; one of anything else to an id and a key of its own", async () => {
    const w = world()
    await unheard(w, "A", false)
    assert.equal(w.writes.shown(make.control).phase, "lost")
    assert.deepEqual(w.writes.shown(make.control).kept, { name: "A" })
    w.press(make, { name: "A" }, how)
    assert.deepEqual([w.sent.at(-1)?.write.path, w.sent.at(-1)?.write.key], [w.sent[0].write.path, w.sent[0].write.key], "the same create")
    await w.wait(30_000)
    await w.running()?.land()
    w.press(make, { name: "B" }, how)
    assert.notEqual(w.sent.at(-1)?.write.path, w.sent[0].write.path, "another create, another id")
    assert.notEqual(w.sent.at(-1)?.write.key, w.sent[0].write.key)
  })

  test("a lost create the read lists has landed: its toast goes, it counts as a landing, and a press of the same after is a create of its own", async () => {
    const w = world()
    await unheard(w, "A", true)
    const first = w.sent[0].write
    assert.equal(isPending(w.writes.shown(make.control).phase), false)
    assert.ok(w.dismissed.includes(make.control), "its timed-out toast goes")
    assert.deepEqual([w.writes.shown(make.control).phase, w.writes.shown(make.control).kept, w.writes.shown(make.control).landings], ["idle", null, 1])
    assert.equal(w.writes.shown(make.control).made, first.made)
    w.press(make, { name: "A" }, how)
    assert.notEqual(w.sent.at(-1)?.write.path, first.path, "the group made is never made again at its id")
    assert.notEqual(w.sent.at(-1)?.write.key, first.key)
  })
})

describe("asking a read again", () => {
  const at = (n: number) => n as Seq
  test("a read begun after the boundary, or an ask joined to one out, covers it; one begun before, or none, is asked for", () => {
    const chasing = chased(IDLE, at(5), NO_FLIGHT, false)
    assert.deepEqual(chasing, { next: { kind: "chasing", boundary: 5, behind: 0 }, effects: [{ kind: "ask" }] })
    assert.deepEqual(chased(chasing.next, at(5), flown(NO_FLIGHT, { kind: "began", at: at(6) }), false).effects, [])
    assert.deepEqual(chased(chasing.next, at(5), flown(NO_FLIGHT, { kind: "asked" }), false).effects, [])
    assert.deepEqual(chased(chasing.next, at(5), flown(flown(NO_FLIGHT, { kind: "began", at: at(4) }), { kind: "ended" }), false).effects, [{ kind: "ask" }])
  })

  test("a read begun after the boundary that ended behind it backs off, doubling to the read timeout, and a new boundary starts afresh", () => {
    const behind = flown(flown(NO_FLIGHT, { kind: "began", at: at(6) }), { kind: "ended" })
    let backing = chased(IDLE, at(5), behind, false)
    const waits: number[] = []
    for (let i = 0; i < 6; i++) {
      assert.equal(backing.next.kind, "backingOff")
      waits.push((backing.effects[0] as { ms: number }).ms)
      assert.deepEqual(chased(backing.next, at(5), behind, false).effects, [], "one wait at a time")
      const due = chased(backing.next, at(5), behind, true)
      assert.deepEqual(due.effects, [{ kind: "ask" }])
      backing = chased(due.next, at(5), behind, false)
    }
    assert.deepEqual(waits, [REREAD_MS, 2 * REREAD_MS, 4 * REREAD_MS, 8 * REREAD_MS, READ_TIMEOUT_MS, READ_TIMEOUT_MS])
    assert.deepEqual(chased(backing.next, at(9), behind, false), { next: { kind: "chasing", boundary: 9, behind: 0 }, effects: [{ kind: "cancel" }, { kind: "ask" }] })
    assert.deepEqual(chased(backing.next, null, behind, false), { next: IDLE, effects: [{ kind: "cancel" }] })
  })
})

describe("the machine of one write", () => {
  test("a timer of an earlier press moves nothing on a later press's write", () => {
    const box = actions.tick(EU, AMS) as Action<unknown, unknown>
    const write: Write<unknown> = { key: "k2", method: "PUT", path: box.path, bytes: '{"ticked":true}', precondition: null, made: null, pressed: { ticked: true }, edits: new Map() }
    const record: Pending<unknown> = { write, action: box, how: { what: "EU" }, startedAt: 7 as Seq, slow: false, phase: { kind: "out", attempts: 1, front: 0, unsure: false } }
    const reads: Reads = { state: { data: undefined, landedBeganAt: null }, settings: { data: undefined, landedBeganAt: null } }
    for (const n of [1, 6]) assert.deepEqual(step(record, { kind: "timer", of: 3 as Seq, n, at: 9 as Seq }, reads), { next: record, effects: [] }, `timer ${n}`)
    assert.equal(step(record, { kind: "timer", of: 7 as Seq, n: 6, at: 9 as Seq }, reads).next?.phase.kind, "unanswered", "its own press's end ends it")
  })
})

describe("a press on a group's or source's server", () => {
  const reads: Reads = { state: { data: undefined, landedBeganAt: null }, settings: { data: undefined, landedBeganAt: null } }
  const owner = { kind: "source", source: "aurora" as SourceId } as const

  test("a press names its server in its path, sends no body, and is on the owner's one control", () => {
    const press = actions.switchTo(owner)
    assert.notEqual(press.control, actions.switchTo({ kind: "group", group: EU }).control, "a group's presses are its own")
    const write = writeOf(press, AMS, null, new Map(), undefined, () => "k1")
    assert.equal(write.path, "api/sources/aurora/preferred/l-ams")
    assert.equal(write.bytes, null)
  })

  test("Clear names the pick's server and end in its path, sends no body, and is on the press's control", () => {
    const unpick = actions.unpick(owner)
    assert.equal(unpick.control, actions.switchTo(owner).control, "a press and Clear never cross")
    const write = writeOf(unpick, { server: AMS, endsAt: "2026-11-21T11:05:00.000Z" }, null, new Map(), undefined, () => "k1")
    assert.deepEqual([write.method, write.path, write.bytes], ["DELETE", "api/sources/aurora/preferred/l-ams?endsAt=2026-11-21T11%3A05%3A00.000Z", null])
  })

  test("its answer stands at once, while the press stays Pending until the view reaches the answer's revision", () => {
    const action = actions.switchTo(owner) as Action<unknown, unknown>
    const write = writeOf(action, AMS, null, new Map(), undefined, () => "k1")
    const pressed = step(null, { kind: "press", write, action, how: { what: "Amsterdam" }, unsure: false, at: 1 as Seq }, reads)
    const answer = { lineage: AMS, verdict: "failed", pingMs: null, newConnectionMs: null, at: "2026-11-19T11:05:00.000Z" }
    const answered = step(pressed.next, { kind: "answer", of: 1 as Seq, attempt: 1, answer: { kind: "yes", revision: 42, body: answer }, at: 2 as Seq }, reads)
    assert.equal(answered.next?.phase.kind, "waiting")
    const kept = answered.effects.flatMap((e) => (e.kind === "keep" ? [e.answer] : []))
    assert.equal(kept.length, 1)
    assert.deepEqual(answerOf(kept[0], actions.switchTo(owner)), answer, "the press's answer is the press's")
  })

  test("a Test that failed or couldn't run is said once the view reaches the answer's revision, and a pass says nothing", () => {
    const action = actions.switchTo(owner) as Action<unknown, unknown>
    const write = writeOf(action, AMS, null, new Map(), undefined, () => "k1")
    const behind: Reads = { state: { data: { revision: 41 } as View, landedBeganAt: null }, settings: reads.settings }
    const there: Reads = { state: { data: { revision: 42 } as View, landedBeganAt: 3 as Seq }, settings: reads.settings }
    const said = (effects: readonly Effect[]) => effects.flatMap((e) => (e.kind === "say" ? [e.ending] : []))
    for (const [verdict, failed] of [["failed", { rating: "bad", words: "Failed", reason: "No response" }], ["unknown", { rating: "none", words: "Not run", reason: "No route" }], ["succeeded", null]] as const) {
      const pressed = step(null, { kind: "press", write, action, how: { what: "Amsterdam" }, unsure: false, at: 1 as Seq }, behind)
      const body = { lineage: AMS, verdict, pingMs: null, newConnectionMs: null, at: "2026-11-19T11:05:00.000Z" }
      const answered = step(pressed.next, { kind: "answer", of: 1 as Seq, attempt: 1, answer: { kind: "yes", revision: 42, body }, at: 2 as Seq }, behind)
      assert.deepEqual(said(answered.effects), [], `${verdict}: nothing is said while the boxes still wait`)
      const landed = step(answered.next, { kind: "read", at: 4 as Seq }, there)
      assert.equal(landed.next, null, `${verdict}: the press settles`)
      assert.deepEqual(said(landed.effects), failed === null ? [] : [{ kind: "failed", failed }], `${verdict}: what is said as it settles`)
    }
  })
})

describe("every toast the page can say", () => {
  const id = "slot:eu" as ControlKey
  const write: Write<unknown> = { key: "k1", method: "PUT", path: "api/slots/eu", bytes: "{}", precondition: null, made: null, pressed: {}, edits: new Map() }
  /* An Exit pick as the slot's tile presses it: the daemon's Exit reasons name the Exit picked and Open its page, the rest the slot. */
  const pick: PressHow<unknown>["refused"] = (r) => (r.reason === "groupEmpty" || r.reason === "sourceEmpty" ? { what: "Empty", open: r.reason === "groupEmpty" ? "/groups/empty" : undefined } : undefined)
  const cases: [Ending, PressHow<unknown>["refused"], string][] = [
    [{ kind: "noAnswer" }, undefined, "danger EU · timed out · Retry, stays"],
    [{ kind: "front", status: 413, retry: false }, undefined, "danger EU · HTTP 413, leaves"],
    [{ kind: "front", status: 429, retry: true }, undefined, "danger EU · HTTP 429 · Retry, stays"],
    [{ kind: "refused", refusal: { code: "validation", reason: "sourceEmpty", detail: null, message: "That source has no servers now.", status: 422, revision: 1, field: null } }, undefined, "danger EU · no servers, leaves"],
    [{ kind: "refused", refusal: { code: "validation", reason: "sourceEmpty", detail: null, message: "That source has no servers now.", status: 422, revision: 1, field: null } }, pick, "danger Empty · no servers, leaves"],
    [{ kind: "refused", refusal: { code: "validation", reason: "groupEmpty", detail: null, message: "That group has no servers. Change its filter first.", status: 422, revision: 1, field: null } }, pick, "danger Empty · no servers · Open, stays"],
    [{ kind: "refused", refusal: { code: "busy", reason: "busy", detail: null, message: "Busy.", status: 409, revision: 1, field: null } }, pick, "danger EU · busy, leaves"],
    [{ kind: "refused", refusal: { code: "validation", reason: "clientsFull", detail: null, message: "Every VPN Client UniFi allows is in use.", status: 409, revision: 1, field: null } }, undefined, "danger EU · limit reached, leaves"],
    [{ kind: "refused", refusal: { code: "validation", reason: "badPattern", detail: "missing closing ]", message: "Include pattern: error parsing regexp: missing closing ]: `[a`.", status: 422, revision: 1, field: null } }, undefined, "danger EU · missing closing ], leaves"],
    [{ kind: "refused", refusal: { code: "validation", reason: "aReasonOfTomorrow", detail: null, message: "A sentence the page has no words for.", status: 422, revision: 1, field: null } }, undefined, "danger EU · A sentence the page has no words for., leaves"],
    [{ kind: "unifi", status: 401 }, undefined, "danger EU · session expired · Sign in, stays"],
    [{ kind: "unifi", status: 403 }, undefined, "danger EU · UniFi refused · Sign in, stays"],
    [{ kind: "gone" }, undefined, "danger EU · was deleted, leaves"],
    [{ kind: "conflict", conflicted: undefined, resent: false }, undefined, "danger EU · changed in another tab, leaves"],
    [{ kind: "conflict", conflicted: { kind: "form", stored: "Retention 1y" }, resent: false }, undefined, "danger EU · changed in another tab · Retention 1y, stays"],
    [{ kind: "conflict", conflicted: { kind: "form", stored: "Retention 1y" }, resent: true }, undefined, "danger EU · changed since this Save · Retention 1y, stays"],
    [{ kind: "conflict", conflicted: { kind: "form", stored: null }, resent: true }, undefined, "danger EU · changed since this Save, stays"],
    [{ kind: "conflict", conflicted: { kind: "control", theirs: "Acme", mine: "Fast EU", again: { pressed: {}, precondition: { kind: "replace", revision: 5 } } }, resent: true }, undefined, "danger EU · changed to Acme · Your value: Fast EU · Keep mine, stays"],
    [{ kind: "landed", route: "/groups/new" }, undefined, "ok EU · Open, stays"],
    [{ kind: "failed", failed: { words: "Failed", reason: "No response" } }, undefined, "danger EU · failed · No response, leaves"],
  ]
  for (const [ending, refused, words] of cases) {
    test(words, () => {
      const t = toastOf(id, { what: "EU", refused, resentWhy: "changed since this Save" }, ending, write)
      assert.equal(`${t.mark} ${[t.what, t.why, t.muted, t.button?.word ?? null].filter((x) => x !== null).join(" · ")}, ${t.stays ? "stays" : "leaves"}`, words)
    })
  }
})

/** A seeded generator, so a failing run names its seed and replays. */
function random(seed: number) {
  let a = seed >>> 0
  const next = () => {
    a = (a + 0x6d2b79f5) >>> 0
    let t = a
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
  return { next, pick: <T>(items: readonly T[]): T => items[Math.floor(next() * items.length)] as T }
}

const RUNS = 10_000
const STEPS = 30

/** How a form's write ended after its last form closed while it was out, told by the toast it said. */
type Away = "yes" | "refused" | "conflict" | "noAnswer" | "other"
const awayOf = (why: string | null | undefined): Away =>
  why === undefined ? "yes" : why === "timed out" ? "noAnswer" : why === "changed in another tab" ? "conflict"
    : why === null || /^(HTTP |session expired|UniFi refused|was deleted)/.test(why) ? "other" : "refused"

describe("random runs", () => {
  test(`${RUNS} random sequences of press, edit, answer, timer, read and another tab keep every invariant, and settle once reads land`, async () => {
    const exits = ["fast-eu", "steady", "acme"] as const
    const boxes = [actions.tick(EU, AMS), actions.tick(EU, PAR)]
    const slot = actions.slot("eu" as SlotId)
    const remove = actions.deleteSlot("eu" as SlotId)
    const doomed = actions.deleteSource("aurora" as SourceId)
    const form = settingsForm.action
    const controls = [...boxes.map((b) => b.control), slot.control, doomed.control, form.control]
    const pathOf = new Map<string, ControlKey>([...boxes.map((b) => [b.path, b.control] as const), [slot.path, slot.control], [doomed.path, doomed.control], [form.path, form.control]])
    const readOf = (control: ControlKey): ReadName => (control === form.control ? "settings" : "state")
    const fingerprint = (write: Write<unknown>) => JSON.stringify([write.method, write.path, write.precondition, write.bytes])
    const fields = ["analyticsRetentionDays", "secondField"] as const
    let steps = 0
    /** How often the source's delete was refused as referenced at a revision past the view the page held. */
    let refusedAhead = 0
    /** How often a yes to a write out when its form closed left what was typed after its press. */
    let restKept = 0
    /** Per ending: how often a form's write ended so after its last form closed while it was out, how often that
     * close came as the write's answer or its read was landing, before the task's microtasks ran, and how often that
     * write was a toast's Retry sent while its form was closed. */
    const ended = {
      closed: { yes: 0, refused: 0, conflict: 0, noAnswer: 0, other: 0 },
      racing: { yes: 0, refused: 0, conflict: 0, noAnswer: 0, other: 0 },
      retried: { yes: 0, refused: 0, conflict: 0, noAnswer: 0, other: 0 },
    }

    for (let run = 0; run < RUNS; run++) {
      mock.timers.reset()
      mock.timers.enable({ apis: ["setTimeout", "Date"] })
      const seed = run + 1
      const r = random(seed)
      const firstRead = r.next() < 0.25
      const w = world((d) => {
        d.items.set(slot.path, { revision: 1, value: { name: "EU", exit: { kind: "group", group: "fast-eu" } } })
        d.items.set(doomed.path, { revision: 1, value: { name: "Aurora" } })
        d.refusing.set(doomed.path, { status: 409, code: "referenced", reason: "inUse", message: "Aurora is used by EU." })
      }, { firstRead })
      const answered = new Set<number>()
      const held: { land: () => Promise<void> }[] = []
      const keyOf = new Map<string, string>()
      /** Per control: the highest revision a delivered yes or conflict named for its write now out, in its own read. */
      const landmark = new Map<ControlKey, number>()
      /** Per control: its write now out was answered 404, and waits to be shown deleted. */
      const gone = new Set<ControlKey>()
      /** Per control: when its write now out was pressed, and the order of the toast that ended it with its fate unknown. */
      const outSince = new Map<ControlKey, number>()
      const unansweredAt = new Map<ControlKey, number>()
      /** The form's typing: each field's last edit, and the edits some write was made with. */
      const typed = new Map<string, { readonly value: number; readonly edit: number }>()
      const sentEdits = new Set<number>()
      let edits = 0
      /** The count of edits when the form's last write was pressed: an edit after it is in no write. */
      let pressedAfter = 0
      let event = 0
      const landed: Record<ReadName, Fetch | null> = { state: null, settings: null }
      const trace: string[] = []
      const note = (what: string) => { trace.push(`${event + 1} @${Date.now()}: ${what}`) }
      const fail = (what: string) => assert.fail(`seed ${seed}, event ${event}: ${what}\n${trace.join("\n")}`)
      /** The releases of the settings forms open now; one is open as the run begins. */
      const forms = [w.writes.mount(form.control, null)]
      /** The draft while no form is open and a write decides it: held while that write is out, then kept, and the
       * touched fields it held when its last form closed. `racing`: that close came just as the write was ending.
       * `retried`: the write out is one a toast sent again while the draft was kept. */
      let away: { kind: "held" | "kept"; draft: string; racing: boolean; retried: boolean } | null = null
      const awayKind = () => away?.kind
      let heard = 0
      const draftOf = () => {
        const f = w.writes.form(settingsForm)
        return JSON.stringify([...f.touched].sort().map((field) => [field, f.shown[field]]))
      }
      const close = (racing: boolean) => {
        const out = w.writes.form(settingsForm).phase === "out"
        const unheard = noAnswer(w.writes.form(settingsForm).phase)
        note(`close a settings form${out ? " while its write is out" : unheard ? " while its write has no answer" : ""}${racing ? ", as it ends" : ""}`)
        forms.splice(Math.floor(r.next() * forms.length), 1)[0]()
        if (forms.length > 0) return
        away = out ? { kind: "held", draft: draftOf(), racing, retried: false } : unheard ? { kind: "kept", draft: draftOf(), racing: false, retried: false } : null
        if (!out && !unheard) typed.clear()
      }

      const live = () => w.sent.map((s, i) => ({ s, i })).filter(({ s, i }) => !s.signal.aborted && !answered.has(i))
      /** `between` runs once the answer is on its way and before the store has taken it. */
      const deliver = async (i: number, answer: Answer, between?: () => void) => {
        const s = w.sent[i]
        note(`attempt ${i} (${s.write.key}${s.signal.aborted ? ", aborted" : ""}) answered ${JSON.stringify(answer)}`)
        answered.add(i)
        const control = pathOf.get(s.write.path) as ControlKey
        if (!s.signal.aborted) {
          const body = answer.kind === "yes" ? (answer.body as { revision?: number }) : null
          const drawn = control === doomed.control && answer.kind === "refused" && answer.code === "referenced"
          const reached = control === form.control ? (answer.kind === "conflict" ? answer.stored : body?.revision) : answer.kind === "yes" || answer.kind === "conflict" || drawn ? answer.revision : undefined
          if (reached !== undefined) landmark.set(control, Math.max(landmark.get(control) ?? 0, reached))
          if (answer.kind === "refused" && answer.status === 404) gone.add(control)
          if (drawn && answer.revision > ((w.cached() as { revision: number } | undefined)?.revision ?? 0)) refusedAhead++
        }
        s.answer(answer)
        if (between) {
          await Promise.resolve()
          between()
        }
        await w.flush()
      }
      const land = async (read: ReadName, how: "land" | "old" | "fail") => {
        const fetch = w.running(read)
        if (fetch === null) return
        note(`the ${read} read begun at ${fetch.began} ${how === "fail" ? "fails" : how === "old" ? "lands with old data" : "lands"}`)
        if (how !== "fail") landed[read] = fetch
        if (how === "land") await fetch.land()
        else if (how === "old") await fetch.landWith(w.cached(read))
        else await fetch.fail()
      }
      const snapshot = () => new Map(controls.map((c) => [c, { pending: isPending(w.writes.shown(c).phase), unanswered: noAnswer(w.writes.shown(c).phase) }]))

      const check = (before: Map<ControlKey, { pending: boolean; unanswered: boolean }>) => {
        event++
        for (const { s } of live()) {
          if (Date.now() - s.at >= 30_000) fail(`attempt under ${s.write.key} still out ${Date.now() - s.at} ms after it was sent`)
        }
        for (const control of controls) {
          const keys = new Set(live().filter(({ s }) => pathOf.get(s.write.path) === control).map(({ s }) => s.write.key))
          if (keys.size > 1) fail(`${control} has ${keys.size} writes out`)
          const shown = w.writes.shown(control)
          const mine = w.sent.filter((s) => pathOf.get(s.write.path) === control).at(-1)
          if (shown.sent !== null && JSON.stringify(shown.sent) !== JSON.stringify(mine?.write.pressed)) fail(`${control} shows ${JSON.stringify(shown.sent)}, not its write's body`)
          const was = before.get(control)
          const out = shown.phase === "out"
          if (out && (!was?.pending || was.unanswered)) outSince.set(control, Date.now())
          const since = outSince.get(control)
          if (out && since !== undefined && Date.now() - since >= 30_000 && !landmark.has(control) && !gone.has(control)) fail(`${control} is still out ${Date.now() - since} ms after its press, unanswered`)
          if (was?.pending && !isPending(shown.phase)) {
            const read = readOf(control)
            const reached = landmark.get(control)
            const revision = (w.cached(read) as { revision: number } | undefined)?.revision ?? 0
            if (reached !== undefined && revision < reached) fail(`${control} settled on ${read} ${revision}, older than its answer's ${reached}`)
            const ended = unansweredAt.get(control)
            const fetch = landed[read]
            if (was.unanswered && ended !== undefined && (fetch === null || fetch.began < ended)) fail(`${control} settled unanswered on a read that began before it ended`)
            const key = w.sent.filter((s) => pathOf.get(s.write.path) === control).at(-1)?.write.key
            if (key !== undefined && w.daemon.commits.has(key) && reached === undefined && !gone.has(control) && fetch === null) fail(`${control} settled with its write landed, on no answer and no read`)
          }
          if (!isPending(shown.phase)) { landmark.delete(control); gone.delete(control); unansweredAt.delete(control); outSince.delete(control) }
          if (shown.phase === "unanswered" && !was?.unanswered) unansweredAt.set(control, w.said.filter((s) => s.toast.id === control).at(-1)?.order ?? 0)
        }
        const was = before.get(form.control)
        const out = w.writes.form(settingsForm).phase === "out"
        if (away?.kind === "held" && was?.pending && !was.unanswered && !out) {
          const how = awayOf(w.said.slice(heard).filter((s) => s.toast.id === form.control).at(-1)?.toast.why)
          note(`the form's write ended: ${how}`)
          ended.closed[how]++
          if (away.racing) ended.racing[how]++
          if (away.retried) ended.retried[how]++
          if (how === "yes") {
            /* What was typed after the press is left, unless none of it differs from what is stored. */
            const after = [...typed].filter(([, { edit }]) => edit > pressedAfter).sort(([a], [b]) => a.localeCompare(b))
            const stored = w.cached("settings") as unknown as Record<string, number>
            const rest = after.some(([field, { value }]) => stored[field] !== value) ? JSON.stringify(after.map(([field, { value }]) => [field, value])) : "[]"
            if (draftOf() !== rest) fail(`a yes to a write out when its form closed left the draft ${draftOf()}, not ${rest}`)
            if (rest === "[]") {
              away = null
              typed.clear()
            } else {
              restKept++
              away = { ...away, kind: "kept", draft: rest, racing: false }
            }
          } else away = { ...away, kind: "kept", racing: false }
        }
        if (away?.kind === "kept" && out) away = { ...away, kind: "held", retried: true }
        if (away?.racing) away = { ...away, racing: false }
        if (away !== null && draftOf() !== away.draft) fail(`the draft ${away.kind} with no form open is ${draftOf()}, not the ${away.draft} it held when its form closed`)
        if (away === null && forms.length === 0 && draftOf() !== "[]") fail(`the draft ${draftOf()} outlived its form, closed with no write out`)
        if (away === null && forms.length === 0 && w.writes.form(settingsForm).refusal !== null) fail("a refusal outlived its form, closed with no write out")
        heard = w.said.length
        const shown = w.writes.form(settingsForm).shown as Record<string, number>
        for (const [field, { value, edit }] of typed) {
          if (!sentEdits.has(edit) && shown[field] !== value) fail(`the form shows ${field} ${shown[field]}, not the ${value} typed and in no write`)
        }
        for (const s of w.sent) {
          const known = keyOf.get(s.write.key)
          if (known !== undefined && known !== fingerprint(s.write)) fail(`key ${s.write.key} carried two requests`)
          keyOf.set(s.write.key, fingerprint(s.write))
        }
        for (const [key, n] of w.daemon.commits) if (n > 1) fail(`key ${key} committed ${n} times`)
        if (w.daemon.reused > 0) fail("a key was reused for another request")
        landed.state = null
        landed.settings = null
      }

      /** A Retry sent as another tab wrote or the daemon began refusing, which the next step answers with its form still closed. */
      let retryDue: number | null = null
      for (let n = 0; n < STEPS; n++) {
        const before = snapshot()
        const choice = r.next()
        if (retryDue !== null) {
          const i = retryDue
          retryDue = null
          await deliver(i, w.daemon.handle(w.sent[i].write))
        } else if (choice < 0.16) {
          const box = r.pick([...boxes, null])
          const control = box?.control ?? slot.control
          const wasUnanswered = noAnswer(w.writes.shown(control).phase) && !live().some(({ s }) => pathOf.get(s.write.path) === control)
          const last = w.sent.filter((s) => pathOf.get(s.write.path) === control).at(-1)
          const count = w.sent.length
          note(`press ${control}`)
          if (box === null) {
            const stored = viewOf(w.cached())?.items[slot.path]
            const exit = { kind: "group", group: r.pick(exits) as GroupId } as const
            w.press(slot, { input: { name: "EU", exit }, edit: "exit" }, {
              what: "EU", precondition: stored ? { kind: "replace", revision: stored.revision } : { kind: "create" },
              conflict: (data) => {
                const now = viewOf(data)?.items[slot.path]
                if (!now) return null
                return { kind: "control", theirs: String((now.value.exit as { group: string }).group), mine: exit.group, again: { pressed: { input: { name: String(now.value.name), exit }, edit: "exit" as const }, precondition: { kind: "replace", revision: now.revision } } }
              },
            })
          } else {
            w.press(box, { ticked: r.next() < 0.5 })
          }
          const made = w.sent[count]
          if (made && wasUnanswered && last && w.sent.length > count) {
            const same = fingerprint(made.write) === fingerprint(last.write)
            if (same !== (made.write.key === last.write.key)) fail(`a press after no answer: same request ${same}, same key ${made.write.key === last.write.key}`)
          }
        } else if (choice < 0.22) {
          if (forms.length === 0) {
            note("open a settings form")
            forms.push(w.writes.mount(form.control, null))
            away = null
          } else if (r.next() < 0.6) {
            const field = r.pick(fields)
            const value = field === "analyticsRetentionDays" ? r.pick([30, 60, 90]) : r.pick([3600, 21600])
            note(`type ${field} ${value}`)
            w.writes.edit(form, null, { [field]: value })
            typed.set(field, { value, edit: ++edits })
          } else {
            /* The page's Save takes a press only while the draft differs from what is stored, so one that doesn't is typed first. */
            if (!w.writes.form(settingsForm).dirty) {
              const value = r.pick([60, 90].filter((v) => v !== w.writes.form(settingsForm).shown.analyticsRetentionDays))
              note(`type analyticsRetentionDays ${value}`)
              w.writes.edit(form, null, { analyticsRetentionDays: value })
              typed.set("analyticsRetentionDays", { value, edit: ++edits })
            }
            const count = w.sent.length
            note("press Save")
            w.writes.pressForm(settingsForm)
            if (w.sent.length > count) {
              for (const { edit } of typed.values()) sentEdits.add(edit)
              pressedAfter = edits
            }
          }
        } else if (choice < 0.47) {
          const out = live()
          if (out.length > 0) {
            const { s, i } = r.pick(out)
            const fate = r.next()
            if (fate < 0.35) await deliver(i, w.daemon.handle(s.write))
            else if (fate < 0.5) { note(`attempt ${i} (${s.write.key}) reaches the daemon, its answer lost`); w.daemon.handle(s.write); answered.add(i) }
            else if (fate < 0.6) { note(`attempt ${i} (${s.write.key}) reaches the daemon, its answer held`); const answer = w.daemon.handle(s.write); answered.add(i); held.push({ land: () => deliver(i, answer) }) }
            else if (fate < 0.7) await deliver(i, NO_CONNECTION)
            else if (fate < 0.78) await deliver(i, { kind: "unknown", status: 503 })
            else if (fate < 0.85) await deliver(i, { kind: "token", status: r.pick([401, 403] as const) })
            else await deliver(i, { kind: "front", status: r.pick([413, 429]), retry: r.next() < 0.5 })
          }
        } else if (choice < 0.53 && held.length > 0) {
          await held.splice(Math.floor(r.next() * held.length), 1)[0].land()
        } else if (choice < 0.58) {
          const mine = live().filter(({ s }) => s.write.path === form.path)
          const how = r.next()
          const retry = w.said.filter((s) => s.toast.id === form.control).at(-1)
          if (awayKind() === "kept" && retry?.toast.button?.word === "Retry" && how < 0.5) {
            note("toast button Retry on the settings form, closed")
            retry.act()
            if (how < 0.35 && w.sent.at(-1)?.write.path === form.path) {
              if (how < 0.2) {
                note("another tab writes the settings as Retry goes out")
                w.daemon.other(form.path, { analyticsRetentionDays: r.pick([45, 75]), secondField: r.pick([7200, 43200]) })
              } else {
                note("the daemon refuses api/settings as Retry goes out")
                w.daemon.refusing.set(form.path, { status: 422, reason: "groupEmpty", message: "That group has no servers. Change its filter first." })
              }
              retryDue = w.sent.length - 1
            }
            await w.flush()
          } else if (awayKind() === "held" && how < 0.5) {
            note("wait 30000 with the settings form closed")
            await w.wait(30_000)
          } else if (forms.length === 0 || (forms.length === 1 && how < 0.2)) {
            note("open a settings form")
            forms.push(w.writes.mount(form.control, null))
            away = null
          } else if (mine.length > 0 && how < 0.5) {
            const { s, i } = r.pick(mine)
            await deliver(i, w.daemon.handle(s.write), () => close(true))
          } else if (w.writes.form(settingsForm).phase === "out" && w.running("settings") === null && how < 0.7) {
            close(true)
            w.begin("settings")
            await land("settings", "land")
          } else {
            close(false)
            await w.flush()
          }
        } else if (choice < 0.63) {
          const ms = r.pick([100, 1_000, 2_000, 5_000, 16_000])
          note(`wait ${ms}`)
          await w.wait(ms)
        } else if (choice < 0.7) {
          const path = r.pick([...boxes.map((b) => b.path), slot.path, form.path])
          note(`another tab writes ${path}`)
          w.daemon.other(path, path === slot.path ? { name: r.pick(["EU", "Europe"]), exit: { kind: "group", group: r.pick(exits) } }
            : path === form.path ? { analyticsRetentionDays: r.pick([30, 45, 60]) } : { ticked: r.next() < 0.5 })
        } else if (choice < 0.78) {
          const read = r.pick(["state", "settings"] as const)
          const fetch = w.begin(read)
          note(`a ${read} read begins at ${fetch.began}`)
          const how = r.next()
          if (how < 0.6) await land(read, "land")
          else if (how < 0.7) await land(read, "old")
        } else if (choice < 0.9) {
          const how = r.next()
          await land(r.pick(["state", "settings"] as const), how < 0.7 ? "land" : how < 0.85 ? "old" : "fail")
        } else if (choice < 0.905) {
          note("press Delete on slot:eu")
          w.press(remove, undefined, { what: "EU" })
        } else if (choice < 0.91) {
          note("press Delete on the source Aurora")
          w.press(doomed, undefined, { what: "Aurora", shows: (refusal) => refusal.code === "referenced" })
        } else if (choice < 0.92) {
          const path = r.pick([...boxes.map((b) => b.path), slot.path, form.path])
          const refusing = w.daemon.refusing.has(path)
          note(`the daemon ${refusing ? "stops refusing" : "refuses"} ${path}`)
          if (refusing) w.daemon.refusing.delete(path)
          else w.daemon.refusing.set(path, { status: 422, reason: "groupEmpty", message: "That group has no servers. Change its filter first." })
        } else {
          const button = w.said.filter((s) => s.toast.button !== null).at(-1)
          if (button) { note(`toast button ${button.toast.button?.word} on ${button.toast.id}`); button.act(); await w.flush() }
        }
        check(before)
        steps++
      }

      note("every read lands from here on")
      for (let i = 0; i < 12 && controls.some((c) => isPending(w.writes.shown(c).phase)); i++) {
        const before = snapshot()
        await w.wait(11_000)
        for (const read of ["state", "settings"] as const) await land(read, "land")
        check(before)
      }
      const stuck = controls.filter((c) => isPending(w.writes.shown(c).phase))
      if (stuck.length > 0) fail(`${stuck.join(", ")} still Pending after two minutes of reads landing`)
    }
    assert.ok(steps >= RUNS * STEPS)
    assert.ok(refusedAhead > 0, "no run refused the source's delete past the view the page held")
    assert.ok(restKept > 0, "no run's yes to a write out when its form closed left what was typed after its press")
    for (const how of ["yes", "refused", "conflict", "noAnswer"] as const) {
      assert.ok(ended.closed[how] > 0, `no run ended a write out when its form closed with ${how}: ${JSON.stringify(ended)}`)
      assert.ok(ended.retried[how] > 0, `no run ended a toast's Retry of a draft whose form was closed with ${how}: ${JSON.stringify(ended)}`)
    }
    for (const how of ["yes", "refused", "conflict"] as const) {
      assert.ok(ended.racing[how] > 0, `no run closed a form just as its write ended with ${how}: ${JSON.stringify(ended)}`)
    }
  })
})
