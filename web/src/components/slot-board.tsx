import { useLayoutEffect, useRef, useState, type KeyboardEvent, type ReactNode } from "react"
import { Link } from "react-router"
import { ChevronsUpDownIcon, ExternalLinkIcon, FileQuestionIcon, PencilIcon, PlusIcon, Trash2Icon, XIcon } from "lucide-react"
import { AlertDialog, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle, AlertDialogTrigger } from "@/components/ui/alert-dialog"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ExitMenu } from "@/components/exit-menu"
import { OpenMenus, pressedServer, SwitchLine } from "@/components/switch-menu"
import { exitableId, useStandings } from "@/components/nav"
import { useNaming } from "@/components/name-edit"
import { EmptyState, failureOf, useRead, type Read } from "@/components/page"
import { ServerName } from "@/components/server"
import { UnifiCard } from "@/components/app-shell"
import { Spinner } from "@/components/ui/spinner"
import { Fact } from "@/components/card"
import { LAST_KNOWN, Pending, Quiet, RatingMark, SlotDot, Toned } from "@/components/status"
import { actions, UNIFI_SETTINGS, type SlotEdit } from "@/lib/api"
import { clock } from "@/lib/format"
import { pingPair } from "@/lib/health"
import { serversRead, useServerIndex, useView } from "@/lib/queries"
import type { ClientTrouble, Exit, ServerView, SlotInput, SlotView, View } from "@/lib/schema"
import { isPending, useControl, useControls, useUnsaved } from "@/lib/use-write"
import { CLIENT_TROUBLES, UNIFI_TROUBLES } from "@/lib/troubles"
import { boardBoxes, exitWords, nextSlotName, ownerOf, slotLine } from "@/lib/slots"
import { cn } from "@/lib/utils"


/** A slot's name being edited, or shown. The pencil starts it, and so does a taken name's Rename. */
function useRename(name: string, save: (name: string) => void) {
  const { naming, type, start, finish } = useNaming(null)
  return {
    naming, type, start: () => start(name),
    finish: (keep: boolean) => {
      const typed = finish(keep)?.trim()
      if (typed && typed !== name) save(typed)
    },
  }
}

/** The pencil renames. Enter or leaving saves, Esc doesn't. While a rename is out the name shows what was sent, and the
 * pencil's place holds the spinner. While any write of the slot is out the pencil takes no press, so nothing is typed that its
 * one control would drop. */
function SlotName({ slot, name, out, rename: { naming, type, start, finish } }: { slot: SlotView; name: string; out: SlotOut; rename: ReturnType<typeof useRename> }) {
  const renaming = out === "name"
  if (naming.kind === "showing") {
    return (
      <>
        <span className="min-w-0 truncate font-medium">{name}</span>
        <Button size="icon-xs" variant="ghost" className="text-muted-foreground hover:text-foreground shrink-0" aria-label={`Rename ${slot.name}`} title="Rename" disabled={out !== null} aria-busy={renaming || undefined} onClick={start}>
          {renaming ? <Spinner role={undefined} aria-label={undefined} aria-hidden /> : <PencilIcon />}
        </Button>
      </>
    )
  }
  return (
    <Input autoFocus aria-label={`Name of ${slot.name}`} className="h-6 min-w-0 flex-1 px-1.5 font-medium" value={naming.text} onFocus={(e) => e.target.select()} onChange={(e) => type(e.target.value)} onBlur={() => finish(true)}
      onKeyDown={(e) => { if (e.key === "Enter") finish(true); if (e.key === "Escape") finish(false) }} />
  )
}

/** The tile's last line, in the state `slotLine` names. The Exit menu above names where the slot is going, so the line names only
 * the server the daemon chose. What UniFi couldn't do stands in Trouble words, with its fix where the page has one. */
function SlotState({ slot, view, servers, list, out, rename }: { slot: SlotView; view: View; servers: Map<ServerView["id"], ServerView>; list: Read<unknown>; out: SlotOut; rename: () => void }) {
  /* Enable and Recreate are one control, the VPN Client's, bodiless, so whether its write is out is `pending`. Remove is the
   * slot's own, as its Exit and name are, so it waits on `out` with them. */
  const controls = useControls()
  const client = controls.shown(actions.enableClient(slot.id))
  const press = (action: typeof actions.enableClient | typeof actions.deleteSlot) => controls.press(action(slot.id), undefined, { what: slot.name })
  const { standings } = useStandings()
  /* On a group or a source the line opens the owner's servers, a closed slot's included, and is Switching to while it moves. */
  const owner = ownerOf(slot.exit, view.groups, view.sources)
  const state = slotLine(slot, {
    picking: out === "exit", pressed: owner && pressedServer(controls.shown(actions.switchTo(owner.exit))), unifiTrouble: view.unifiTrouble, groups: view.groups,
    sources: view.sources, servers, list: failureOf(list) ? "failed" : list.state, ownerTroubles: standings.get(exitableId(slot.exit) ?? "")?.troubles ?? null,
  })
  const line = (shows: string, words: ReactNode, { lastKnown = false, moving = false } = {}) => (owner ? <SwitchLine slot={slot} owner={owner} slots={view.slots} servers={servers} sources={view.sources} shows={shows} lastKnown={lastKnown} moving={moving}>{words}</SwitchLine> : words)
  switch (state.kind) {
    case "picking": return <Pending words="Switching…" />
    case "clientDisabled":
      return <><span className="text-warning min-w-0 flex-1 truncate">VPN Client disabled</span><ActionButton pending={isPending(client.phase)} onClick={() => press(actions.enableClient)}>Enable</ActionButton></>
    case "clientDeleted": {
      /* Remove asks nothing: UniFi already took the client and its routes, so nothing is left to lose. */
      const busy = isPending(client.phase) || out !== null
      return (
        <>
          <span className="text-warning min-w-0 flex-1 truncate" title="Deleted in UniFi, with its routes">Deleted</span>
          <ActionButton pending={isPending(client.phase)} disabled={busy} onClick={() => press(actions.recreateClient)}>Recreate</ActionButton>
          <ActionButton pending={out === "remove"} disabled={busy} onClick={() => press(actions.deleteSlot)}>Remove</ActionButton>
        </>
      )
    }
    case "clientTrouble": {
      const { tone, words } = CLIENT_TROUBLES[state.trouble]
      return (
        <>
          <Toned tone={tone} className="min-w-0 flex-1 truncate">{words}</Toned>
          {state.trouble === "nameTaken" && <Button size="sm" variant="outline" disabled={out !== null} onClick={rename}>Rename</Button>}
        </>
      )
    }
    case "creatingClient": return <Pending words="Creating VPN Client…" />
    case "noClient": return <span className="text-warning truncate">No VPN Client</span>
    case "switching": {
      const { server, words } = state.switching
      const value = server && <ServerName id={server.id} servers={servers} sources={view.sources} withProvider={false} />
      if (!owner) return <Pending words={words} value={value} />
      /* On a group or a source the line itself waits, in Pending's look, so it is one control from a press to its landing. */
      return line(words, value ? <Quiet className="flex min-w-0 flex-1">{value}</Quiet> : <Skeleton data-slot="pending-value" className="h-2 w-3/5 rounded-full motion-reduce:animate-none" />, { moving: true })
    }
    case "checking": return <Pending words="Checking servers…" />
    case "ownerEmpty": return <><Toned tone="bad" className="min-w-0 flex-1 truncate">No servers</Toned><Button asChild size="sm" variant="outline"><Link to={routeOf(state.exit) ?? "/"}>Open</Link></Button></>
    case "failed": return line(state.reason, <span className="text-destructive truncate">{state.reason}</span>)
    case "notConnected": return <span className="text-warning truncate">Not connected</span>
    case "readingServer": return <Pending words="Loading servers…" />
    case "serverUnread": return <ServerName id={state.server} servers={servers} sources={view.sources} list={list} />
    case "unplaced": return <Quiet>—</Quiet>
    case "on": {
      const { label } = state.server
      /* A move in progress is what matters then, and a narrow tile has room for one. */
      const reading = slot.leavingBy ? `Switching by ${clock(slot.leavingBy)}` : pingPair(state.server.health)
      /* The Exit menu above already names the server, its flag with it. */
      if (slot.exit.kind === "server") {
        return (
          <>
            {state.lastKnown && <Fact lines={[{ name: LAST_KNOWN, words: label }]} label={`${LAST_KNOWN}: ${label}`} className="min-h-6 flex-row items-center"><Quiet>Last active</Quiet></Fact>}
            <Quiet className="ml-auto whitespace-nowrap tabular-nums">{reading}</Quiet>
          </>
        )
      }
      const name = <ServerName id={state.server.id} servers={servers} sources={view.sources} withProvider={false} />
      /* A server no pass vouches for now keeps its place in the muted ink, and its card says what it is. On a group or a source
       * the line is the menu's button, which carries the card itself. */
      const shown = !state.lastKnown ? <span className="flex min-w-0 flex-1" title={label}>{name}</span>
        : owner ? <Quiet className="flex min-w-0 flex-1">{name}</Quiet>
        : <Fact lines={[{ name: LAST_KNOWN, words: label }]} label={`${LAST_KNOWN}: ${label}`} className="min-h-6 min-w-0 flex-1 flex-row items-center"><Quiet className="flex min-w-0">{name}</Quiet></Fact>
      return line(label,
        <>
          {shown}
          <Quiet className="whitespace-nowrap tabular-nums">{reading}</Quiet>
        </>,
        { lastKnown: state.lastKnown },
      )
    }
  }
}

/** A button whose write is out: the spinner before its word, and no press. */
function ActionButton({ pending, disabled = pending, onClick, children }: { pending: boolean; disabled?: boolean; onClick: () => void; children: string }) {
  return (
    <Button size="sm" variant="outline" disabled={disabled} aria-busy={pending || undefined} onClick={onClick}>
      {pending && <Spinner role={undefined} aria-label={undefined} aria-hidden />}{children}
    </Button>
  )
}

/** Where a refused Exit is fixed, when it has a page: a group's or a source's tab. */
const routeOf = (exit: Exit): string | undefined => (exit.kind === "group" ? `/groups/${exit.group}` : exit.kind === "source" ? `/sources/${exit.source}` : undefined)

/** The reasons the daemon refuses a picked Exit for (`admitExit` in internal/daemon/commands.go): each is about the Exit, not the slot. */
const EXIT_REFUSALS: ReadonlySet<string> = new Set(["groupGone", "groupEmpty", "sourceGone", "sourceEmpty", "serverGone"])

/** What a refusal of `exit` names when it is the Exit's own: the Exit and its page. Any other is about the slot. */
const refusedExit = (reason: string, exit: Exit, view: View | undefined, servers: Map<ServerView["id"], ServerView>) =>
  EXIT_REFUSALS.has(reason) ? { what: exitWords(exit, view?.groups ?? [], view?.sources ?? [], servers), open: routeOf(exit) } : undefined

/** What of a slot is out on its one control: an Exit pick, a rename, or its delete; null while nothing is. */
type SlotOut = SlotEdit["edit"] | "remove" | null

/** A slot's one write, its PUT, as its tile and a delete dialog's blocker row press it: the reader's change over the slot as the
 * page shows it. A conflict names what is stored now and rebuilds the reader's change over it, so Keep mine keeps whatever the
 * other writer changed beside it. */
export function useSlotWrite(slot: SlotView, servers: Map<ServerView["id"], ServerView>) {
  const write = useControl(actions.slot(slot.id))
  const view = useView().data
  const press = (input: SlotInput, edit: SlotEdit["edit"]) => write.press({ input, edit }, {
    what: slot.name,
    precondition: { kind: "replace", revision: slot.version },
    refused: (refusal) => (edit === "exit" ? refusedExit(refusal.reason, input.exit, view, servers) : undefined),
    conflict: (data) => {
      const now = (data as View).slots.find((s) => s.id === slot.id)
      if (!now) return null
      const words = (exit: Exit) => exitWords(exit, (data as View).groups, (data as View).sources, servers)
      const again: SlotInput = edit === "exit" ? { name: now.name, exit: input.exit } : { name: input.name, exit: now.exit }
      return {
        kind: "control",
        theirs: edit === "exit" ? words(now.exit) : now.name,
        mine: edit === "exit" ? words(input.exit) : input.name,
        again: { pressed: { input: again, edit }, precondition: { kind: "replace", revision: now.version } },
      }
    },
  })
  const shown = write.sent?.input ?? { name: slot.name, exit: slot.exit }
  const out: SlotOut = isPending(write.phase) ? write.sent?.edit ?? "remove" : null
  return {
    shown,
    pending: isPending(write.phase),
    out,
    pick: (exit: Exit) => press({ name: slot.name, exit }, "exit"),
    rename: (name: string) => press({ name, exit: slot.exit }, "name"),
  }
}

/** The question before a slot's delete: not asked, asked, or answered with its Delete, which keeps it open until the write ends. */
type DeleteQuestion = "none" | "asked" | "answered"

/** Deletes a slot, its VPN Client in UniFi with it, so it asks first. Delete is the slot's own write, Pending on its button, and the
 * dialog closes once it ends, however it ended: a landing takes the tile, and a refusal, as while UniFi routes point at the slot, is
 * its toast. It takes no press while any write of the slot or its VPN Client is out. */
function DeleteSlot({ slot, out, held }: { slot: SlotView; out: SlotOut; held: boolean }) {
  const controls = useControls()
  const [question, setQuestion] = useState<DeleteQuestion>("none")
  const removing = out === "remove"
  if (question === "answered" && !removing) setQuestion("none")
  const remove = () => {
    controls.press(actions.deleteSlot(slot.id), undefined, { what: slot.name })
    setQuestion("answered")
  }
  return (
    <AlertDialog open={question !== "none"} onOpenChange={(open) => { if (!removing) setQuestion(open ? "asked" : "none") }}>
      <AlertDialogTrigger asChild>
        <Button size="icon-xs" variant="ghost" className="text-muted-foreground hover:text-foreground" aria-label={`Delete ${slot.name}`} title="Delete" disabled={out !== null || held}><Trash2Icon className="size-3.5" /></Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader className="place-items-start text-left">
          <AlertDialogTitle>Delete {slot.name}?</AlertDialogTitle>
          <AlertDialogDescription>Removes the VPN Client from UniFi. Move any Policy Table routes off it first.</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={removing}>Cancel</AlertDialogCancel>
          <Button variant="destructive" disabled={removing} aria-busy={removing || undefined} onClick={remove}>
            {removing && <Spinner role={undefined} aria-label={undefined} aria-hidden />}Delete
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

/** A slot's tile. Its Exit and its name are one write, the slot's PUT, Pending where the change shows: an Exit pick is the line
 * under the select, which keeps the pick at full strength and takes no other; a rename is the name, its pencil the spinner. */
function SlotTile({ slot, view, servers, list }: { slot: SlotView; view: View; servers: Map<ServerView["id"], ServerView>; list: Read<unknown> }) {
  const write = useSlotWrite(slot, servers)
  const rename = useRename(slot.name, write.rename)
  const client = useControl(actions.enableClient(slot.id))
  return (
    <Card size="sm" className="min-w-0 gap-2 p-3" role="group" aria-label={slot.name} data-control={`${actions.slot(slot.id).control} ${actions.enableClient(slot.id).control}`}>
      <div className="flex h-6 items-center gap-2">
        <SlotDot slot={slot} servers={servers} />
        <SlotName slot={slot} name={write.shown.name} out={write.out} rename={rename} />
        <span className="ml-auto flex shrink-0 items-center gap-1.5">
          <DeleteSlot slot={slot} out={write.out} held={isPending(client.phase)} />
          <a href={UNIFI_SETTINGS} target="_blank" rel="noreferrer" className="text-muted-foreground hover:text-foreground" aria-label={`Open ${slot.name} in UniFi`} title="Open in UniFi"><ExternalLinkIcon className="size-3.5" aria-hidden /></a>
        </span>
      </div>
      <ExitMenu value={write.shown.exit} onChange={write.pick} busy={write.pending} groups={view.groups} servers={servers} sources={view.sources} list={list}
        label={`Exit of ${slot.name}`} className="w-full" />
      <div data-slot="slot-line" className="flex min-h-6 items-center gap-1.5"><SlotState slot={slot} view={view} servers={servers} list={list} out={write.out} rename={rename.start} /></div>
    </Card>
  )
}

/** A VPN Client someone else made in UniFi, or ours kept after its slot went. It takes a spot and nothing here touches it. A kept
 * one says in Trouble words what keeps it, which only UniFi can end. */
function OccupiedTile({ name, trouble }: { name: string; trouble: ClientTrouble | null }) {
  return (
    <Card size="sm" className="min-h-28 min-w-0 gap-2 border-dashed p-3 ring-0" role="group" aria-label={name}>
      <div className="flex h-6 items-center gap-2">
        <span className="bg-muted-foreground/40 size-2 shrink-0 rounded-full" aria-hidden />
        <span className="min-w-0 truncate font-medium">{name}</span>
        <a href={UNIFI_SETTINGS} target="_blank" rel="noreferrer" className="text-muted-foreground hover:text-foreground ml-auto shrink-0" aria-label={`Open ${name} in UniFi`} title="Open in UniFi"><ExternalLinkIcon className="size-3.5" aria-hidden /></a>
      </div>
      {trouble ? <Toned tone={CLIENT_TROUBLES[trouble].tone} className="truncate">{CLIENT_TROUBLES[trouble].words}</Toned> : <Quiet>Managed in UniFi</Quiet>}
    </Card>
  )
}

/** As tall as a tile, so the card Add becomes moves nothing. */
const ADD_BOX = "bg-muted text-foreground/70 dark:bg-card border-foreground/25 h-full min-h-29 w-full rounded-lg border border-dashed"

/** The board's last box, which creates a slot. While UniFi can make no VPN Client it can't, and its card says why in UniFi's
 * Trouble words. */
function AddBox({ view, servers, list }: { view: View; servers: Map<ServerView["id"], ServerView>; list: Read<unknown> }) {
  const trouble = view.unifiTrouble
  if (trouble) {
    return (
      <UnifiCard trouble={trouble} answered={view.unifi.acceptedAt} disabled label={`Add: UniFi ${UNIFI_TROUBLES[trouble].words}`}
        className={cn(buttonVariants({ variant: "ghost" }), ADD_BOX, "cursor-not-allowed flex-row items-center opacity-50 hover:bg-muted dark:hover:bg-card")}>
        <span className="flex items-center gap-1"><PlusIcon />Add</span>
      </UnifiCard>
    )
  }
  return <AddSlot view={view} servers={servers} list={list} />
}

/** A held Enter presses nothing again: one held on Cancel would open the card anew. */
const heldEnter = (e: KeyboardEvent) => { if (e.key === "Enter" && e.repeat) e.preventDefault() }

/** A slot being made, which the page alone holds until Create: its name and Exit, the name it opened with, and, once Create was
 * pressed, how many creates of the board had landed by then. An edit takes the press back: the create that lands then is another's. */
type AddingSlot = { readonly offered: string; readonly name: string; readonly exit: Exit; readonly pressedAt: number | null }

/** Add makes a VPN Client on the gateway, so a press of it writes nothing: the box becomes the new slot's card, in its place, and
 * the card's Create makes it. The card goes with its slot's landing, with Cancel, × and Escape, and with the box, when UniFi's
 * trouble takes its place or no VPN Client is left free. Gone, it leaves the keys on the box, unless something else has them. */
function AddSlot({ view, servers, list }: { view: View; servers: Map<ServerView["id"], ServerView>; list: Read<unknown> }) {
  /* Each Create makes a slot at an id of its own, the store's, so one after a create that landed is a second slot, never a 412. */
  const add = useControl(actions.newSlot)
  const creating = isPending(add.phase)
  const [adding, setAdding] = useState<AddingSlot | null>(null)
  if (adding !== null && adding.pressedAt !== null && add.landings > adding.pressedAt) setAdding(null)
  const box = useRef<HTMLButtonElement>(null)
  const open = adding !== null
  const was = useRef(open)
  useLayoutEffect(() => {
    if (was.current && !open && document.activeElement === document.body) box.current?.focus({ preventScroll: true })
    was.current = open
  }, [open])
  if (adding === null) {
    const offered = nextSlotName(view.slots, view.vpnClients)
    return (
      <Button ref={box} variant="ghost" className={cn(ADD_BOX, "aria-disabled:pointer-events-none aria-disabled:opacity-50 hover:bg-[color-mix(in_oklch,var(--muted),var(--foreground)_6%)] dark:hover:bg-[color-mix(in_oklch,var(--card),var(--foreground)_8%)]")}
        aria-disabled={creating || undefined} aria-busy={creating || undefined} data-control={actions.newSlot.control} onKeyDown={heldEnter}
        onClick={() => { if (!creating) setAdding({ offered, name: offered, exit: { kind: "block" }, pressedAt: null }) }}>
        {creating ? <Spinner role={undefined} aria-label={undefined} aria-hidden /> : <PlusIcon />}Add
      </Button>
    )
  }
  const create = () => {
    const name = adding.name.trim()
    const { exit } = adding
    if (creating || name === "") return
    setAdding({ ...adding, pressedAt: add.landings })
    add.press({ name, exit }, { what: name, precondition: { kind: "create" }, refused: (refusal) => refusedExit(refusal.reason, exit, view, servers) })
  }
  return <NewSlotCard adding={adding} creating={creating} edit={(patch) => setAdding({ ...adding, ...patch, pressedAt: null })} cancel={() => setAdding(null)} create={create} view={view} servers={servers} list={list} />
}

/** The new slot's card, in a tile's shape: its name ready to type over where the tile has its name, the `+` in its dot's place, its
 * Exit, and Cancel and Create on its line. Enter in the name creates nothing: Add leaves the keys there, so its second Enter would
 * make a slot. While its create is out the card takes no edit and stays. */
function NewSlotCard({ adding, creating, edit, cancel, create, view, servers, list }: {
  adding: AddingSlot; creating: boolean; edit: (patch: Pick<AddingSlot, "name"> | Pick<AddingSlot, "exit">) => void; cancel: () => void; create: () => void
  view: View; servers: Map<ServerView["id"], ServerView>; list: Read<unknown>
}) {
  useUnsaved(adding.name !== adding.offered || adding.exit.kind !== "block")
  /* The Exit menu is the card's by React's tree alone, so its keys come here too: its Escape closes the menu. */
  const onKeyDown = (e: KeyboardEvent<HTMLElement>) => {
    if (!(e.target instanceof Node && e.currentTarget.contains(e.target))) return
    heldEnter(e)
    if (e.key === "Escape" && !creating) cancel()
  }
  return (
    <Card size="sm" className="min-w-0 gap-2 p-3" role="group" aria-label="New slot" data-control={actions.newSlot.control} onKeyDown={onKeyDown}>
      <div className="flex h-6 items-center gap-2">
        <PlusIcon aria-hidden className="text-muted-foreground size-3.5 shrink-0" />
        <Input autoFocus aria-label="New slot name" className="h-6 flex-1 px-1.5 font-medium" value={adding.name} readOnly={creating} onFocus={(e) => e.target.select()} onChange={(e) => edit({ name: e.target.value })} />
        <Button type="button" size="icon-xs" variant="ghost" className="text-muted-foreground hover:text-foreground shrink-0" aria-label="Cancel new slot" title="Cancel" disabled={creating} onClick={cancel}><XIcon className="size-3.5" /></Button>
      </div>
      <ExitMenu value={adding.exit} onChange={(exit) => edit({ exit })} busy={creating} groups={view.groups} servers={servers} sources={view.sources} list={list} label="Exit of new slot" className="w-full" />
      <div className="flex min-h-6 items-center justify-end gap-1.5">
        <Button type="button" size="sm" variant="outline" disabled={creating} onClick={cancel}>Cancel</Button>
        <Button type="button" size="sm" disabled={creating || adding.name.trim() === ""} aria-busy={creating || undefined} onClick={create}>
          {creating && <Spinner role={undefined} aria-label={undefined} aria-hidden />}Create
        </Button>
      </div>
    </Card>
  )
}

export function Board({ data }: { data: View }) {
  const list = useRead(serversRead)
  const servers = useServerIndex()
  const board = boardBoxes(data.slots, data.vpnClients)
  return (
    <OpenMenus>
      <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4" role="list" aria-label="VPN Clients">
        {board.boxes.map((box) => box.kind === "slot"
          ? <SlotTile key={box.slot.id} slot={box.slot} view={data} servers={servers} list={list} />
          : box.kind === "foreign" ? <OccupiedTile key={box.id} name={box.name} trouble={box.trouble} /> : null)}
        {(!data.vpnClients.known || board.free > 0) && <AddBox view={data} servers={servers} list={list} />}
      </div>
    </OpenMenus>
  )
}

/** The board while the daemon's view is on its way: boxes in the board's own grid, each with a faint dot, its name's place, the Exit
 * select's frame and chevron with its value's place, and the server line Pending. */
export function BoardWaiting() {
  return (
    <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4" aria-label="VPN Clients">
      {["w-20", "w-14"].map((width) => (
        <Card key={width} size="sm" className="min-w-0 gap-2 p-3">
          <div className="flex h-6 items-center gap-2">
            <RatingMark rating="none" />
            <Skeleton className={cn("h-2 rounded-full", width)} />
          </div>
          <span aria-hidden className={cn(buttonVariants({ variant: "outline" }), "w-full min-w-0 justify-between font-normal")}>
            <Skeleton className="h-2 w-1/3 rounded-full" />
            <ChevronsUpDownIcon className="text-muted-foreground" />
          </span>
          <div data-slot="slot-line" className="flex h-6 items-center gap-1.5"><Pending words="Loading VPN Clients…" /></div>
        </Card>
      ))}
    </div>
  )
}

/** An address the page doesn't have. The top bar names it, so the block holds the mark and the way out. */
export function NotFound() {
  return (
    <EmptyState icon={<FileQuestionIcon aria-label="Not found" />}>
      <Button asChild variant="outline"><Link to="/">Dashboard</Link></Button>
    </EmptyState>
  )
}
