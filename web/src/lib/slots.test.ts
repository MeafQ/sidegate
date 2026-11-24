import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { describe, mock, test } from "node:test"
import { expiryWords, standingOf } from "./troubles.ts"
import type { GroupView, Preferred, ServerView, SlotView, SourceView, VpnClients } from "./schema.ts"
import { appliedOn, boardBoxes, dotWords, downWords, drawnServer, exitServers, exitWords, followersOf, heldPick, listed, nextSlotName, onPick, ownerOf, slotDot, slotLine, switchRow, switchingTo, type LineFacts } from "./slots.ts"

const sample = <T>(name: string): T => JSON.parse(readFileSync(new URL(`../../tests/wire/${name}.json`, import.meta.url), "utf8")) as T
const SLOT = sample<SlotView>("SlotView.serving")
const slot = (id: string, minute: number): SlotView => ({ ...SLOT, id: id as SlotView["id"], name: id, createdAt: new Date(Date.UTC(2026, 10, 1, 0, minute)).toISOString() })
const clients = (known: boolean, ...foreign: string[]): VpnClients => ({ known, limit: 8, foreign: foreign.map((id) => ({ id, name: `client ${id}`, enabled: true, trouble: null })) })
const shape = (slots: SlotView[], c: VpnClients) => {
  const board = boardBoxes(slots, c)
  return { ...board, boxes: board.boxes.map((b) => (b.kind === "slot" ? b.slot.id : b.kind === "foreign" ? b.id : b.kind)) }
}

describe("boardBoxes", () => {
  test("foreign clients by network ID, then our slots oldest first, then free, 8 in all", () => {
    assert.deepEqual(shape([slot("b", 10), slot("a", 2)], clients(true, "n2", "n1")), {
      boxes: ["n1", "n2", "a", "b", "free", "free", "free", "free"], free: 4, overflow: 0,
    })
  })

  test("a new slot lands after the older ones, whatever its name", () => {
    assert.deepEqual(shape([slot("a", 30), slot("z", 1)], clients(true)).boxes.slice(0, 2), ["z", "a"])
  })

  test("two slots made in the same moment keep a stable order, by id", () => {
    assert.deepEqual(shape([slot("b", 1), slot("a", 1)], clients(true)).boxes.slice(0, 2), ["a", "b"])
  })

  test("without a read of UniFi, no box is free and foreign rows are absent", () => {
    assert.deepEqual(shape([slot("a", 1)], clients(false, "n1")), {
      boxes: ["a", "unknown", "unknown", "unknown", "unknown", "unknown", "unknown", "unknown"], free: 0, overflow: 0,
    })
  })

  test("past 8, every row shows, nothing is free, and the overflow is counted", () => {
    const eight = Array.from({ length: 8 }, (_, i) => slot(`s${i}`, i + 1))
    const board = shape(eight, clients(true, "n1", "n2"))
    assert.equal(board.boxes.length, 10)
    assert.deepEqual(board.boxes.slice(0, 2), ["n1", "n2"])
    assert.equal(board.free, 0)
    assert.equal(board.overflow, 2)
  })

  test("exactly 8 is full with no overflow", () => {
    const board = shape(Array.from({ length: 6 }, (_, i) => slot(`s${i}`, i + 1)), clients(true, "n1", "n2"))
    assert.equal(board.boxes.length, 8)
    assert.equal(board.free, 0)
    assert.equal(board.overflow, 0)
  })

  test("without a read of UniFi, slots past the limit still all show", () => {
    const board = shape(Array.from({ length: 9 }, (_, i) => slot(`s${i}`, i + 1)), clients(false))
    assert.equal(board.boxes.length, 9)
    assert.equal(board.free, 0)
    assert.equal(board.overflow, 1)
  })

  test("free and unknown boxes know their position past the taken ones", () => {
    const { boxes } = boardBoxes([slot("a", 1)], clients(false))
    assert.deepEqual(boxes[1], { kind: "unknown", position: 2 })
    assert.deepEqual(boxes[7], { kind: "unknown", position: 8 })
  })

  test("the wire's own samples draw a board", () => {
    assert.equal(boardBoxes([], sample<VpnClients>("VpnClients.unknown")).boxes.every((b) => b.kind === "unknown"), true)
    const known = boardBoxes([], sample<VpnClients>("VpnClients.known"))
    assert.deepEqual(known.boxes.map((b) => b.kind), ["foreign", ...Array<string>(7).fill("free")])
  })
})

describe("slotLine", () => {
  const id = (s: string) => s as ServerView["id"]
  const base = sample<{ items: ServerView[] }>("ServerPage.mixed").items[0]
  const servers = new Map([[id("a"), { ...base, id: id("a"), label: "Amsterdam" }]])
  const groups = [{ id: "mine" as GroupView["id"], name: "Mine", resolved: [id("a")] }]
  const facts: LineFacts = { picking: false, pressed: null, unifiTrouble: null, groups, sources: [], servers, list: "read", ownerTroubles: [] }
  const kind = (slot: SlotView, patch: Partial<LineFacts> = {}) => slotLine(slot, { ...facts, ...patch }).kind
  const unifi = (patch: Partial<SlotView["unifi"]>): SlotView => ({ ...SLOT, unifi: { ...SLOT.unifi, ...patch } })
  const EMPTY = [{ kind: "empty", tone: "bad", words: "No servers" }] as unknown as LineFacts["ownerTroubles"]

  test("a failed slot says why", () => {
    const dead = (exit: SlotView["exit"]) => slotLine({ ...SLOT, exit, status: "failed", downReason: "confirmedDead" }, facts)
    assert.deepEqual(dead({ kind: "group", group: groups[0].id }), { kind: "failed", reason: "All servers offline" })
    assert.deepEqual(dead({ kind: "source", source: base.source }), { kind: "failed", reason: "All servers offline" })
    assert.deepEqual(dead({ kind: "server", server: id("a") }), { kind: "failed", reason: "Server offline" })
    assert.deepEqual(slotLine({ ...SLOT, status: "failed", downReason: null }, facts), { kind: "failed", reason: "No available server" })
  })
  test("a switch off Direct or Block to no server yet says it waits for one", () => {
    assert.equal(downWords("noExit"), "Connecting…")
  })
  test("a pick out comes before every settled state, UniFi's first among them", () => {
    assert.equal(kind(unifi({ client: "disabled" }), { picking: true }), "picking")
    assert.equal(kind({ ...unifi({ client: "disabled" }), pending: true }), "clientDisabled")
    assert.equal(kind(unifi({ client: "deleted" })), "clientDeleted")
    assert.equal(kind(unifi({ trouble: "nameTaken" })), "clientTrouble")
    assert.equal(kind(unifi({ client: "none" })), "creatingClient")
    assert.equal(kind(unifi({ client: "none" }), { unifiTrouble: "noKey" }), "noClient", "UniFi can make none")
    assert.equal(kind(unifi({ client: "none", known: false })), "noClient")
  })
  test("a press of the owner's out is the move to its server on every box, whatever the box drew, past UniFi's trouble alone, until the box lands there", () => {
    const berlin = { ...base, id: id("b"), label: "Berlin" }
    const pressing = { pressed: id("b"), servers: new Map([...servers, [id("b"), berlin]]) }
    for (const slot of [SLOT, { ...SLOT, status: "checking" as const }, { ...SLOT, status: "failed" as const, downReason: "confirmedDead" as const }, { ...SLOT, runtime: { ...SLOT.runtime, networkEstablished: false } }]) {
      assert.deepEqual(slotLine(slot, { ...facts, ...pressing }), { kind: "switching", switching: { server: berlin, words: "Switching to Berlin" } }, slot.status)
    }
    assert.equal(kind({ ...SLOT, server: id("b"), pending: true }, pressing), "switching", "staged there, still landing")
    assert.deepEqual(slotLine(SLOT, { ...facts, pressed: id("a") }), { kind: "on", server: servers.get(id("a")), lastKnown: false }, "landed on the server pressed, whatever the write is still doing")
    assert.deepEqual(slotLine(SLOT, { ...facts, pressed: id("gone") }), { kind: "switching", switching: { server: null, words: "Switching" } }, "a server the page hasn't read")
    assert.equal(kind(unifi({ client: "disabled" }), pressing), "clientDisabled")
    assert.equal(kind(SLOT, { ...pressing, picking: true }), "picking")
  })
  test("then the daemon's move, its checks and its failure, a failed slot whose Exit has no servers in the list's words", () => {
    assert.equal(kind({ ...SLOT, pending: true }), "switching")
    assert.equal(kind({ ...SLOT, status: "checking" }), "checking")
    assert.equal(kind({ ...SLOT, status: "failed" }, { ownerTroubles: EMPTY }), "ownerEmpty")
    assert.equal(kind({ ...SLOT, status: "failed", exit: { kind: "server", server: id("a") } }, { ownerTroubles: EMPTY }), "failed", "a server Exit has no owner")
    assert.equal(kind({ ...SLOT, runtime: { ...SLOT.runtime, networkEstablished: false } }), "notConnected")
  })
  test("then its server: Pending while the list is read, the failed mark once that read failed, the dash where none is named", () => {
    assert.deepEqual(slotLine(SLOT, facts), { kind: "on", server: servers.get(id("a")), lastKnown: false })
    assert.equal(kind(SLOT, { list: "waiting" }), "readingServer")
    assert.deepEqual(slotLine(SLOT, { ...facts, list: "failed", servers: new Map() }), { kind: "serverUnread", server: "a" })
    assert.equal(kind(SLOT, { servers: new Map(), groups: [{ ...groups[0], resolved: [] }] }), "switching", "a server its Exit doesn't hold is a move")
    assert.equal(kind({ ...SLOT, exit: { kind: "block" }, status: "block", server: null, appliedServer: null }), "unplaced")
  })
})

describe("a settled slot no pass vouches for", () => {
  const id = (s: string) => s as ServerView["id"]
  const base = sample<{ items: ServerView[] }>("ServerPage.mixed").items[0]
  const servers = new Map([[id("a"), { ...base, id: id("a"), label: "Amsterdam" }], [id("b"), { ...base, id: id("b"), label: "Berlin" }]])
  const groups = [{ id: "mine" as GroupView["id"], name: "Mine", resolved: [id("a"), id("b")] }]
  const facts: LineFacts = { picking: false, pressed: null, unifiTrouble: null, groups, sources: [], servers, list: "read", ownerTroubles: [] }
  /* The runtime's last pass could vouch for no server: none applied, its phase unknown. */
  const unvouched: SlotView = { ...SLOT, appliedServer: null, runtime: { ...SLOT.runtime, phase: "unknown", applied: { kind: "down", reason: "unselected" } } }

  test("isn't switching: its line stands on its last known server, which its box draws as applied", () => {
    assert.equal(switchingTo(unvouched, groups, [], servers), null)
    assert.deepEqual(slotLine(unvouched, facts), { kind: "on", server: servers.get(id("a")), lastKnown: true })
    assert.equal(drawnServer(unvouched), "a")
    assert.equal(appliedOn(unvouched), "a", "so the owner's pick on it stands")
  })
  test("a move still pending in a pass that vouches for nothing still switches", () => {
    assert.equal(slotLine({ ...unvouched, pending: true }, facts).kind, "switching")
    assert.equal(drawnServer({ ...unvouched, pending: true }), null)
  })
  test("a slot whose applied server isn't its server still switches, whatever the phase", () => {
    assert.equal(slotLine({ ...unvouched, appliedServer: id("b") }, facts).kind, "switching")
  })
  test("a server the runtime proved keeps its plain line when only its link went unsampled", () => {
    assert.deepEqual(slotLine({ ...SLOT, runtime: { ...SLOT.runtime, phase: "unknown" } }, facts), { kind: "on", server: servers.get(id("a")), lastKnown: false })
  })
  test("a ready pass that read nothing of the core draws no server", () => {
    assert.equal(drawnServer({ ...unvouched, runtime: { ...unvouched.runtime, phase: "ready" } }), null)
  })
})

describe("slotDot", () => {
  test("is faint while UniFi has no client of the slot present, whatever it serves on, and pulses while it moves or checks", () => {
    const disabled = { ...SLOT, unifi: { ...SLOT.unifi, client: "disabled" as const } }
    assert.deepEqual(slotDot(disabled, undefined).fill, { kind: "noClient" })
    assert.equal(slotDot({ ...SLOT, pending: true }, undefined).pulse, true)
    assert.equal(slotDot({ ...SLOT, status: "checking" }, undefined).pulse, true)
    assert.deepEqual(slotDot({ ...SLOT, status: "failed" }, undefined), { fill: { kind: "status", status: "failed" }, words: "Failed", pulse: false })
  })
})

describe("dotWords", () => {
  test("a slot's dot says its state", () => {
    assert.equal(dotWords({ ...SLOT, status: "serving" }), "Serving")
    assert.equal(dotWords({ ...SLOT, status: "checking" }), "Checking")
    assert.equal(dotWords({ ...SLOT, status: "failed", downReason: "confirmedDead" }), "Failed")
    assert.equal(dotWords({ ...SLOT, status: "direct" }), "Direct")
    assert.equal(dotWords({ ...SLOT, status: "block" }), "Block")
  })
})

describe("exitServers", () => {
  test("lists every server its source lists or misses, and no tombstone", () => {
    const server = sample<{ items: ServerView[] }>("ServerPage.mixed").items[0]
    const servers = [
      { ...server, id: "k" as ServerView["id"], presence: "present" as const },
      { ...server, id: "m" as ServerView["id"], presence: "missing" as const },
      { ...server, id: "t" as ServerView["id"], presence: "tombstone" as const },
    ]
    assert.deepEqual(exitServers(servers).map((s) => s.id), ["k", "m"])
  })
})

describe("nextSlotName", () => {
  test("takes the lowest number no VPN Client on the board has", () => {
    assert.equal(nextSlotName([], { foreign: [] }), "Slot 1")
    assert.equal(nextSlotName([{ name: "Slot 1" }, { name: "EU" }], { foreign: [{ id: "n", name: "Slot 2", enabled: true, trouble: null }] }), "Slot 3")
  })
})

describe("exitWords", () => {
  test("says a group or subscription is gone and a server unknown, and doesn't claim that while servers are unread", () => {
    const id = "x" as ServerView["id"]
    assert.equal(exitWords({ kind: "group", group: "g" as GroupView["id"] }, [], [], new Map()), "Removed group")
    assert.equal(exitWords({ kind: "source", source: "s" as SourceView["id"] }, [], [], new Map()), "Removed source")
    assert.equal(exitWords({ kind: "server", server: id }, [], [], new Map()), "Unknown server")
  })
  test("names what it finds", () => {
    assert.equal(exitWords({ kind: "direct" }, [], [], new Map()), "Direct")
    assert.equal(exitWords({ kind: "block" }, [], [], new Map()), "Block")
    assert.equal(exitWords({ kind: "source", source: "s" as SourceView["id"] }, [], [{ id: "s" as SourceView["id"], name: "Aurora" }], new Map()), "Aurora")
  })
})

describe("switchingTo", () => {
  const id = (s: string) => s as ServerView["id"]
  const base = sample<{ items: ServerView[] }>("ServerPage.mixed").items[0]
  const servers = new Map([["ams", "Amsterdam", "aurora"], ["fra", "Frankfurt", "aurora"], ["osl", "Oslo", "other"]].map(([s, label, source]) => [id(s), { ...base, id: id(s), label, source: source as SourceView["id"] }]))
  const groups = [{ id: "eu" as GroupView["id"], name: "Europe", resolved: [id("ams"), id("fra")] }]
  const sources = [{ id: "aurora" as SourceView["id"], name: "Aurora" }]
  /* Serving Amsterdam, applied. */
  const on = { ...SLOT, exit: { kind: "group" as const, group: groups[0].id }, status: "serving" as const, server: id("ams"), appliedServer: id("ams"), pending: false }
  const line = (slot: SlotView, known = servers) => {
    const moving = switchingTo(slot, groups, sources, known)
    return moving && `${moving.server?.label ?? "-"} ${moving.words}`
  }
  const toFra = { kind: "server" as const, server: id("fra") }
  const toAurora = { kind: "source" as const, source: sources[0].id }

  test("says nothing while the runtime shows the server the daemon is on", () => {
    assert.equal(line(on), null)
    assert.equal(line({ ...on, exit: toAurora }), null, "an Exit that still allows the server moves nothing")
    assert.equal(line({ ...on, exit: toAurora }, new Map()), null, "nor does a source whose servers aren't read")
  })
  test("names the Exit while the daemon is still on a server it doesn't allow", () => {
    assert.equal(line({ ...on, exit: toFra }), "- Switching to Frankfurt", "the Exit's words, with no server chosen")
    assert.equal(line({ ...on, exit: toAurora, server: id("osl"), appliedServer: id("osl") }), "- Switching to Aurora", "a server outside the source isn't the choice")
    assert.equal(line({ ...on, exit: toAurora, server: null, pending: true }), "- Switching to Aurora")
  })
  test("names the server the daemon chose until the runtime shows it", () => {
    assert.equal(line({ ...on, exit: toFra, server: id("fra"), pending: true }), "Frankfurt Switching to Frankfurt")
    assert.equal(line({ ...on, exit: toFra, server: id("fra") }), "Frankfurt Switching to Frankfurt", "the runtime still shows the old server")
    assert.equal(line({ ...on, exit: toAurora, server: id("fra"), pending: true }), "Frankfurt Switching to Frankfurt")
    assert.equal(line({ ...on, server: id("fra"), appliedServer: id("fra"), pending: true }), "Frankfurt Switching to Frankfurt", "a move the readback hasn't shown")
    assert.equal(line({ ...on, exit: toAurora, server: id("fra"), appliedServer: id("fra") }), null)
  })
  test("says Switching alone while the server it names can't be read", () => {
    const toAms = { kind: "server" as const, server: id("ams") }
    assert.equal(line({ ...on, exit: toAms, server: id("ams"), appliedServer: id("fra") }, new Map()), "- Switching", "never Unknown server")
    assert.equal(line({ ...on, exit: toAms, server: null, pending: true }, new Map()), "- Switching")
    assert.equal(line({ ...on, exit: toAurora, server: id("osl"), appliedServer: id("fra"), pending: true }, new Map()), "- Switching to Aurora", "an Exit whose name is read keeps it")
  })
  test("gives the line back to a checking or failed slot", () => {
    const landed = { ...on, exit: toAurora, server: null, pending: true }
    assert.equal(line({ ...landed, status: "checking", downReason: null }), null)
    assert.equal(line({ ...landed, status: "failed", downReason: "noUsableServer" }), null)
  })
  test("Direct and Block switch until the runtime leaves the server and shows the move", () => {
    const direct = { ...on, exit: { kind: "direct" as const }, status: "direct" as const, server: null }
    assert.equal(line(direct), "- Switching to Direct")
    assert.equal(line({ ...direct, appliedServer: null, pending: true }), "- Switching to Direct")
    assert.equal(line({ ...direct, appliedServer: null }), null)
    assert.equal(line({ ...direct, exit: { kind: "block" }, status: "block", appliedServer: null }), null, "a closed Block slot isn't moving")
  })
})

describe("a subscription's expiry, as the list and the source's heading say it", () => {
  const remote = sample<SourceView>("SourceView.remote")
  const expires = new Date(2026, 10, 13, 23, 0)
  const said = (now: Date) => {
    if (remote.origin.kind !== "remote") throw new Error("SourceView.remote isn't remote")
    const quota = { upload: null, download: null, limit: null, expires: expires.toISOString(), refills: null, observedAt: null }
    const source: SourceView = { ...remote, origin: { ...remote.origin, fetch: { ...remote.origin.fetch, failures: 0 }, provider: { ...remote.origin.provider, quota } } }
    mock.timers.enable({ apis: ["Date"], now })
    try {
      const { facts, troubles } = standingOf(source, [], null, now.getTime())
      const expiry = facts?.expiry
      assert.ok(expiry && (expiry.kind === "expires" || expiry.kind === "expired"))
      return {
        list: troubles.find((t) => t.kind === "expiry")?.words,
        heading: expiryWords(expiry),
      }
    } finally {
      mock.timers.reset()
    }
  }

  test("three days before, it expires", () => {
    assert.deepEqual(said(new Date(2026, 10, 10, 23, 0)), { list: "Expires in 3d", heading: "Expires in 3d" })
  })
  test("on its last day, until its time has passed, it expires, in hours and then minutes", () => {
    for (const [now, left] of [[new Date(2026, 10, 13, 0, 0), "23h"], [new Date(2026, 10, 13, 12, 0), "11h"], [new Date(expires.getTime() - 1), "1m"]] as const) {
      assert.deepEqual(said(now), { list: `Expires in ${left}`, heading: `Expires in ${left}` }, now.toString())
    }
  })
  test("once its time has passed, it expired", () => {
    for (const [now, ago] of [[expires, "1m"], [new Date(expires.getTime() + 1), "1m"], [new Date(2026, 10, 14, 12, 0), "13h"]] as const) {
      assert.deepEqual(said(now), { list: `Expired ${ago} ago`, heading: `Expired ${ago} ago` }, now.toString())
    }
  })
})

describe("a press on an owner's servers", () => {
  const FAST = { kind: "group", group: "fast" } as SlotView["exit"]
  const on = (id: string, minute: number, exit: SlotView["exit"], server: string | null): SlotView => ({ ...slot(id, minute), exit, appliedServer: server as SlotView["appliedServer"] })

  test("a press names and moves every slot whose Exit is the owner, in the board's order, whether serving, landing or closed", () => {
    const slots = [on("Home", 5, FAST, "fra"), on("Lab", 1, { kind: "source", source: "steady" } as SlotView["exit"], "fra"), on("EU", 2, FAST, "ams"), on("Travel", 9, FAST, null)]
    assert.deepEqual(followersOf(FAST as never, slots).map((s) => s.name), ["EU", "Home", "Travel"])
    assert.equal(listed(["EU"]), "EU")
    assert.equal(listed(["EU", "Home"]), "EU and Home")
    assert.equal(listed(["Living room", "Office upstairs", "Kids tablet", "Travel router", "Home"]), "Living room, Office upstairs, Kids tablet, Travel router and Home")
  })

  test("only a group or a source in the view owns a slot: a server, Direct, Block and a gone owner have none", () => {
    const group = { id: "fast", name: "Fast EU", order: [] } as unknown as GroupView
    const source = { id: "steady", name: "Steady", order: [] } as unknown as SourceView
    assert.equal(ownerOf(FAST as never, [group], [source])?.view.name, "Fast EU")
    assert.equal(ownerOf({ kind: "source", source: "steady" } as never, [group], [source])?.view.name, "Steady")
    for (const exit of [{ kind: "server", server: "fra" }, { kind: "direct" }, { kind: "block" }, { kind: "group", group: "gone" }]) assert.equal(ownerOf(exit as never, [group], [source]), null, exit.kind)
  })

  const ids = (...xs: string[]) => xs as never[]
  const row = (server: string, over: Partial<Parameters<typeof switchRow>[1]> = {}) => switchRow(server as never, { inUse: new Set(ids("proxy")), clearing: false, ...over })

  test("a row the slots are on takes no press, wherever it stands; every other row does", () => {
    assert.deepEqual(["proxy", "backup", "limit"].map((s) => row(s)), ["kept", "open", "open"])
    assert.deepEqual(["proxy", "backup", "limit"].map((s) => row(s, { inUse: new Set(ids("proxy", "backup")) })), ["kept", "kept", "open"], "while slots land, each server they are on")
  })

  test("while Clear is out the row in use stays as it is and every other row waits", () => {
    assert.deepEqual(["proxy", "backup", "limit"].map((s) => row(s, { clearing: true })), ["kept", "waiting", "waiting"])
  })
})

describe("the owner's pick on a box", () => {
  const AT = Date.parse("2026-11-20T12:00:00Z")
  const pick = (server: string, hours: number): Preferred => ({ server: server as Preferred["server"], endsAt: new Date(AT + hours * 3600e3).toISOString() })
  const H = 3600e3

  test("a pick holds until its end and not past it, through the whole day, and no pick holds nothing", () => {
    const day = pick("a", 24)
    for (const hours of [0, 6, 12, 23]) assert.deepEqual(heldPick(day, AT + hours * H), day, `${hours} hours on`)
    for (const hours of [24, 25]) assert.equal(heldPick(day, AT + hours * H), null, `${hours} hours on`)
    assert.equal(heldPick(null, AT), null)
  })

  test("a box carries it only while its slot is applied on the picked server, never while it lands, fails or is elsewhere", () => {
    const on = pick("a", 24)
    assert.equal(onPick(SLOT, on), true, "serving on the picked server")
    assert.equal(onPick(SLOT, pick("b", 24)), false, "on another server, as after a failover")
    assert.equal(onPick(SLOT, null), false, "no pick, as after Clear or its end")
    assert.equal(onPick({ ...SLOT, pending: true }, on), false, "its readback still pending")
    assert.equal(onPick({ ...SLOT, server: "b" as SlotView["server"] }, on), false, "the daemon has it on its way to another server")
    assert.equal(onPick({ ...SLOT, appliedServer: "b" as SlotView["appliedServer"], server: "a" as SlotView["server"] }, on), false, "still landing on the pick")
    assert.equal(onPick({ ...SLOT, status: "failed" }, on), false, "closed")
  })
})
