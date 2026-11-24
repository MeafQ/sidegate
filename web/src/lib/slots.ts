import { rate, RATING_WORDS, type Rating } from "./health.ts"
import type { ClientTrouble, DownReason, Exit, GroupView, LineageId, Preferred, ServerView, SlotView, SourceView, UnifiTrouble, VpnClients } from "./schema.ts"
import { noServers, type Trouble } from "./troubles.ts"

/** One of the positions UniFi has for VPN Clients. `unknown` means the page can't tell who holds it, never that it's free. */
export type Box =
  | { kind: "slot"; slot: SlotView }
  | { kind: "foreign"; id: string; name: string; enabled: boolean; trouble: ClientTrouble | null }
  | { kind: "free"; position: number }
  | { kind: "unknown"; position: number }

export type Board = { boxes: Box[]; free: number; overflow: number }

const byId = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)

/** Our slots in the board's order, oldest first. */
const onBoard = (slots: readonly SlotView[]) => slots.toSorted((a, b) => byId(a.createdAt, b.createdAt) || byId(a.id, b.id))

/** Foreign clients by network ID, then our slots oldest first, then the rest, so a new slot lands where the free spot was and nothing moves. Past UniFi's limit every row still shows
 * and nothing is free. Without a read of UniFi, no position can be called free. */
export function boardBoxes(slots: readonly SlotView[], clients: VpnClients): Board {
  const ours: Box[] = onBoard(slots).map((slot) => ({ kind: "slot", slot }))
  const foreign: Box[] = clients.known
    ? clients.foreign.toSorted((a, b) => byId(a.id, b.id)).map((client) => ({ kind: "foreign", ...client }))
    : []
  const taken = ours.length + foreign.length
  const rest = Math.max(0, clients.limit - taken)
  const open: Box[] = Array.from({ length: rest }, (_, i) => ({ kind: clients.known ? "free" : "unknown", position: taken + i + 1 }))
  return { boxes: [...foreign, ...ours, ...open], free: clients.known ? rest : 0, overflow: Math.max(0, taken - clients.limit) }
}

const DOWN_WORDS = {
  unselected: "No server selected",
  deleting: "Deleting",
  noUsableServer: "No available server",
  serverMissing: "Server missing",
  serverRefused: "Server unsupported",
  confirmedDead: "All servers offline",
  plumbingUnavailable: "Tunnel down",
  /* A switch off Direct or Block names it: the slot is back on the VPN and a server isn't ready yet. */
  noExit: "Connecting…",
} as const satisfies Record<DownReason, string>

export function downWords(reason: keyof typeof DOWN_WORDS): string {
  return DOWN_WORDS[reason]
}

const STATUS_WORDS = { serving: "Serving", checking: "Checking", failed: "Failed", direct: "Direct", block: "Block" } as const satisfies Record<SlotView["status"], string>

export function failedReason(slot: SlotView): string {
  if (slot.downReason === "confirmedDead" && slot.exit.kind === "server") return "Server offline"
  return slot.downReason ? DOWN_WORDS[slot.downReason] : "No available server"
}

/** A slot's status dot for a screen reader: colour can't say it. */
export const dotWords = (slot: SlotView): string => STATUS_WORDS[slot.status]

/** What a slot's dot is drawn from: faint while UniFi has no client of it present; serving on a server the page has read, that
 * server's rating, so the box and the server's row never disagree; else its status's colour. */
export type SlotDotFill = { kind: "noClient" } | { kind: "server"; rating: Rating } | { kind: "status"; status: SlotView["status"] }

/** A slot's dot: its fill, its words, the server's reasons with them while it serves on one not rated good, and whether it
 * pulses, while its move is pending or it is checking. */
export function slotDot(slot: SlotView, server: ServerView | undefined): { fill: SlotDotFill; words: string; pulse: boolean } {
  const rated = slot.status === "serving" && server ? rate(server) : null
  const words = !rated || rated.rating === "good" ? dotWords(slot) : [dotWords(slot), RATING_WORDS[rated.rating], ...rated.why].join(" · ")
  const fill: SlotDotFill = slot.unifi.client !== "present" ? { kind: "noClient" } : rated ? { kind: "server", rating: rated.rating } : { kind: "status", status: slot.status }
  return { fill, words, pulse: slot.pending || slot.status === "checking" }
}

/** A slot's tile's last line, the first of these that holds: an Exit pick out; the VPN Client disabled or deleted in UniFi, with
 * its fix; what else UniFi couldn't do for it; no VPN Client yet, being made while UniFi can make one; a press of its owner's out
 * or the daemon switching; checking servers; a failed slot whose Exit has no servers; the slot's own failure; not connected; then
 * the server it is on, Pending while the server list is read, the failed mark once that read failed, and the dash where nothing
 * names one. A write out comes before every settled state, so its Pending is never hidden. */
export type SlotLine =
  | { kind: "picking" }
  | { kind: "clientDisabled" }
  | { kind: "clientDeleted" }
  | { kind: "clientTrouble"; trouble: ClientTrouble }
  | { kind: "creatingClient" }
  | { kind: "noClient" }
  | { kind: "switching"; switching: Switching }
  | { kind: "checking" }
  | { kind: "ownerEmpty"; exit: OwnerExit }
  | { kind: "failed"; reason: string }
  | { kind: "notConnected" }
  | { kind: "readingServer" }
  | { kind: "serverUnread"; server: LineageId }
  | { kind: "unplaced" }
  | { kind: "on"; server: ServerView; lastKnown: boolean }

/** What a slot's line reads beside the slot: whether an Exit pick of it is out, the server a press of its owner is out for from
 * any of its boxes, UniFi's own trouble, the view's groups and sources, the servers by id and their list's read (`failed` once a
 * read of it failed, the last over what it shows included), and the troubles of the source or group its Exit is. */
export type LineFacts = {
  picking: boolean
  pressed: LineageId | null
  unifiTrouble: UnifiTrouble | null
  groups: Pick<GroupView, "id" | "name" | "resolved">[]
  sources: Pick<SourceView, "id" | "name">[]
  servers: Map<ServerView["id"], ServerView>
  list: "waiting" | "failed" | "read"
  ownerTroubles: readonly Trouble[] | null
}

export function slotLine(slot: SlotView, f: LineFacts): SlotLine {
  if (f.picking) return { kind: "picking" }
  const { client, known, trouble } = slot.unifi
  if (client === "disabled") return { kind: "clientDisabled" }
  if (client === "deleted") return { kind: "clientDeleted" }
  if (trouble) return { kind: "clientTrouble", trouble }
  if (client === "none") return known && !f.unifiTrouble ? { kind: "creatingClient" } : { kind: "noClient" }
  const switching = f.pressed !== null && appliedOn(slot) !== f.pressed ? toPressed(f.pressed, f.servers) : switchingTo(slot, f.groups, f.sources, f.servers)
  if (switching) return { kind: "switching", switching }
  if (slot.status === "checking") return { kind: "checking" }
  if (slot.status === "failed") {
    const { exit } = slot
    if ((exit.kind === "group" || exit.kind === "source") && f.ownerTroubles && noServers(f.ownerTroubles)) return { kind: "ownerEmpty", exit }
    return { kind: "failed", reason: failedReason(slot) }
  }
  if (known && slot.status === "serving" && !slot.runtime.networkEstablished) return { kind: "notConnected" }
  const drawn = drawnServer(slot)
  if (!drawn) return { kind: "unplaced" }
  if (f.list === "waiting") return { kind: "readingServer" }
  const server = f.servers.get(drawn)
  if (server) return { kind: "on", server, lastKnown: lastKnown(slot) }
  return f.list === "failed" ? { kind: "serverUnread", server: drawn } : { kind: "unplaced" }
}

/** The servers the Exit menu lists: every one its source still lists or misses, none it dropped for good. */
export function exitServers(servers: Iterable<ServerView>): ServerView[] {
  return [...servers].filter((s) => s.presence !== "tombstone")
}

/** An Exit in words: the group's name or the server's label. While the servers are unread a server has no words: its place is
 * Pending. */
export function exitWords(exit: Exit, groups: Pick<GroupView, "id" | "name">[], sources: Pick<SourceView, "id" | "name">[], servers: Map<ServerView["id"], ServerView>): string {
  if (exit.kind === "direct") return "Direct"
  if (exit.kind === "block") return "Block"
  if (exit.kind === "group") return groups.find((g) => g.id === exit.group)?.name ?? "Removed group"
  if (exit.kind === "source") return sources.find((x) => x.id === exit.source)?.name ?? "Removed source"
  return servers.get(exit.server)?.label ?? "Unknown server"
}

/** Whether `exit` lets a slot use the server `id`. A source's server the page hasn't read can't be told apart, so it counts. */
const allows = (exit: Exit, id: ServerView["id"], groups: Pick<GroupView, "id" | "resolved">[], servers: Map<ServerView["id"], ServerView>): boolean =>
  exit.kind === "server" ? exit.server === id
  : exit.kind === "group" ? (groups.find((g) => g.id === exit.group)?.resolved.includes(id) ?? false)
  : exit.kind === "source" ? (servers.get(id)?.source ?? exit.source) === exit.source
  : false

/** A move the daemon's view shows: the server it chose, once it chose one the Exit allows, and the sentence that says it. */
export type Switching = { server: ServerView | null; words: string }

/** What the daemon's view says a slot is switching to, or null when nothing is moving. The slot is moving while its
 * readback is pending, or while the server its box draws isn't the server its Exit has the daemon on. It names that
 * server once the daemon chose one the Exit allows, the Exit until then, and says "Switching" alone while the server that
 * names is one the page hasn't read. A checking or failed slot says its own state. */
export function switchingTo(slot: SlotView, groups: Pick<GroupView, "id" | "name" | "resolved">[], sources: Pick<SourceView, "id" | "name">[], servers: Map<ServerView["id"], ServerView>): Switching | null {
  if (slot.status === "checking" || slot.status === "failed") return null
  const chosen = slot.server !== null && allows(slot.exit, slot.server, groups, servers) ? slot.server : null
  if (!slot.pending && drawnServer(slot) === chosen) return null
  const server = chosen === null ? null : servers.get(chosen) ?? null
  const unread = slot.exit.kind === "server" && !servers.has(slot.exit.server)
  const to = server?.label ?? (unread ? null : exitWords(slot.exit, groups, sources, servers))
  return { server, words: to === null ? "Switching" : `Switching to ${to}` }
}

/** The move a press of the slot's owner is out for: to the server it picked, as the daemon's view shows it once the Test passes,
 * so one wait runs from the press until the slot lands, which ends it whatever the press's write is still doing. */
function toPressed(id: LineageId, servers: Map<ServerView["id"], ServerView>): Switching {
  const server = servers.get(id) ?? null
  return { server, words: server ? `Switching to ${server.label}` : "Switching" }
}

/** A new slot's name: the lowest "Slot N" no VPN Client on the board has. It reaches UniFi, so it stays plain. */
export function nextSlotName(slots: readonly Pick<SlotView, "name">[], clients: Pick<VpnClients, "foreign">): string {
  const taken = new Set([...slots.map((s) => s.name), ...clients.foreign.map((c) => c.name)])
  for (let n = 1; ; n++) if (!taken.has(`Slot ${n}`)) return `Slot ${n}`
}

export const sameExit = (a: Exit, b: Exit): boolean => JSON.stringify(a) === JSON.stringify(b)


/** An Exit that is a group or a source: an owner whose slots a press can put on one of its servers. */
export type OwnerExit = Extract<Exit, { kind: "group" | "source" }>

/** A group or source as a press reads it: its servers in the daemon's order, and the server picked by hand and its end while the
 * pick holds on a slot of it. */
export type Owner = { exit: OwnerExit; view: Pick<GroupView | SourceView, "name" | "order" | "preferred"> }

/** The owner a slot's Exit names, or null for a server, Direct, Block, or an owner gone from the view. */
export function ownerOf(exit: Exit, groups: readonly GroupView[], sources: readonly SourceView[]): Owner | null {
  if (exit.kind === "group") { const view = groups.find((g) => g.id === exit.group); return view ? { exit, view } : null }
  if (exit.kind === "source") { const view = sources.find((x) => x.id === exit.source); return view ? { exit, view } : null }
  return null
}

/** The slots whose Exit is `owner`, in the board's order, whether serving, landing or closed: every slot a press moves. */
export const followersOf = (owner: OwnerExit, slots: readonly SlotView[]): SlotView[] => onBoard(slots).filter((s) => sameExit(s.exit, owner))

/** "EU", "EU and Home", "EU, Home and Lab". */
export const listed = (names: readonly string[]): string => (names.length < 2 ? names.join("") : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`)

/** The owner's pick while it holds: the daemon drops it at its end, and the page doesn't show it past that while the drop is on
 * its way. */
export const heldPick = (preferred: Preferred | null, now: number): Preferred | null =>
  preferred !== null && Date.parse(preferred.endsAt) > now ? preferred : null

/** Whether a slot's box draws its server from the daemon's word alone: the runtime's last pass could vouch for no server, and
 * no move is pending, so the slot is where its last landing showed it. */
export const lastKnown = (slot: SlotView): boolean => slot.appliedServer === null && slot.runtime.phase === "unknown" && !slot.pending

/** The server a slot's box draws it on: the one the runtime reports, or its last known one. Go's `SlotView.drawnServer` is the
 * same. */
export const drawnServer = (slot: SlotView): LineageId | null => slot.appliedServer ?? (lastKnown(slot) ? slot.server : null)

/** The server a slot's box draws as applied: the one it draws while the slot serves, its move no longer pending, and that
 * server is the one the daemon has it on. Go's `SlotView.appliedOn` is the same. */
export const appliedOn = (slot: SlotView): LineageId | null => {
  const drawn = drawnServer(slot)
  return slot.status === "serving" && !slot.pending && drawn !== null && drawn === slot.server ? drawn : null
}

/** Whether a slot is on its owner's pick: applied on the picked server, as its box draws it, and not still landing there. */
export const onPick = (slot: SlotView, pick: Preferred | null): boolean => pick !== null && appliedOn(slot) === pick.server

/** What a server's row in the menu of an owner's servers does with a press. `open` takes it. `kept`, a server the slots are on,
 * takes none at full strength, and holds Clear when it is the pick. While Clear is out the kept row stays, and every other
 * `waiting`, at half strength. A press closes the menu, so no row stands while one is out. */
export type SwitchRow = "open" | "kept" | "waiting"

export function switchRow(server: LineageId, { inUse, clearing }: { inUse: ReadonlySet<LineageId>; clearing: boolean }): SwitchRow {
  if (inUse.has(server)) return "kept"
  return clearing ? "waiting" : "open"
}
