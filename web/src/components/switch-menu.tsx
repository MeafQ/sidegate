import { createContext, useContext, useEffect, useId, useState, type Dispatch, type ReactNode, type SetStateAction } from "react"
import { ChevronDownIcon, XIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { ButtonGroup } from "@/components/ui/button-group"
import { CommandEmpty, CommandGroup } from "@/components/ui/command"
import { Popover, PopoverTrigger } from "@/components/ui/popover"
import { Spinner } from "@/components/ui/spinner"
import { NoMatch } from "@/components/page"
import { CarryingMark } from "@/components/server"
import { Head, MenuFrame, ServerOption, ServerStats, useOptionCards } from "@/components/exit-menu"
import { TestAnswer } from "@/components/server-details"
import { Fact, Tip, type Beside, type Held } from "@/components/card"
import { LAST_KNOWN, Quiet } from "@/components/status"
import { actions } from "@/lib/api"
import { useNow } from "@/lib/clock"
import { drawnServer, followersOf, heldPick, listed, onPick, switchRow, type Owner } from "@/lib/slots"
import { REASON_WORDS, wearsOff } from "@/lib/format"
import { testWords } from "@/lib/health"
import { useControls } from "@/lib/use-write"
import type { Phase } from "@/lib/write"
import type { LineageId, Preferred, ProbeResult, ServerView, SlotId, SlotView, SourceView } from "@/lib/schema"
import { cn } from "@/lib/utils"

/** The refusals about the owner, not the server picked: their toast names the group or source. */
const OWNER_REFUSALS: ReadonlySet<string> = new Set(["groupGone", "sourceGone", "ownerIdle"])

/** The row the slots are on takes no press and stands at full strength with no hover ground, its card still opening, since cmdk
 * would draw it at half. */
const KEPT = "data-[disabled=true]:pointer-events-auto data-[disabled=true]:opacity-100 data-[disabled=true]:data-selected:bg-transparent"

/** Which box's menu of its owner's servers is open on the board, so every line of that owner lights with it and keeps its words'
 * room unseen. */
type OpenMenu = [SlotId | null, Dispatch<SetStateAction<SlotId | null>>]
const OpenMenuContext = createContext<OpenMenu | null>(null)

export function OpenMenus({ children }: { children: ReactNode }) {
  const open = useState<SlotId | null>(null)
  return <OpenMenuContext.Provider value={open}>{children}</OpenMenuContext.Provider>
}

function useOpenMenu(): OpenMenu {
  const open = useContext(OpenMenuContext)
  if (!open) throw new Error("A server line stands on the board, inside OpenMenus")
  return open
}

/** The owner's pick while it holds, gone at its end to the moment rather than at the clock's next half minute. */
function useHeldPick(preferred: Preferred | null, now: number): Preferred | null {
  const [over, setOver] = useState<string | null>(null)
  const held = heldPick(preferred, now)
  const ends = held?.endsAt ?? null
  useEffect(() => {
    if (ends === null) return
    const end = setTimeout(() => setOver(ends), Date.parse(ends) - Date.now())
    return () => clearTimeout(end)
  }, [ends])
  return held && held.endsAt !== over ? held : null
}

/** What a box on the owner's pick says, muted. When it wears off is said once, in the menu's chip. */
const PICK_WORDS = "Manual override"

/** What the line's card stands under: the line, and while the words show the words too, so the card never lies where a press on
 * them would land. Always given, so a change of what it stands under keeps the line's button, and its focus, where they are. */
const under = (words: boolean): Beside => (line) => (words ? (line.parentElement ?? line) : line).getBoundingClientRect()

/** The server a press of the owner is out for, whichever of its boxes sent it: its way names the box, then the server or `unpick`. */
export function pressedServer({ phase, way }: { phase: Phase; way: string | null }): LineageId | null {
  const out = phase === "out" && way !== null ? way.slice(way.indexOf(" ") + 1) : null
  return out === null || out === "unpick" ? null : (out as LineageId)
}

/** A box's server line on a group or a source: one button the line's width that opens the owner's servers, ending in the chevron.
 * `children` is the line's words and `shows` what they say, for its name. One write per owner is out at a time, a press or Clear,
 * whichever box it came from. From a press until its slot lands the line is the box's Switching to line: Pending's look in this
 * same button, which takes no press and whose card says it. It stays this one element, so the reader's focus stays on it through
 * the press, the landing and a failure. The answer is the owner's press's, whichever box pressed it and whatever was on screen
 * when it came: the store says a failure in the toast as the write settles, and every box's menu has it for its minute. A held
 * Enter's or Space's repeats open nothing, so a key held through a pick, its answer or its landing opens the menu only when
 * pressed again. Space pressed while the line waits presses nothing: the browser presses a button as Space comes up, which may
 * be once the line takes presses again. A box that lands while the press's write is out takes no press until it settles. While
 * Clear is out its box's line spins in the chevron's place once its menu is closed, and every other box of the owner stands at
 * half strength and takes no press. While one box's menu is open, every line of its owner takes the open line's ground. On the
 * owner's pick, the box says so on a line of its own under the server, which keeps its room unseen while the owner's menu is
 * open, and from a press on the pick until the box lands. While the words show, a press on them is a press on the line, and the
 * line's card stands under them. */
export function SwitchLine({ slot, owner, slots, servers, sources, shows, lastKnown = false, moving = false, children }: {
  slot: SlotView; owner: Owner; slots: readonly SlotView[]; servers: Map<LineageId, ServerView>; sources: Pick<SourceView, "id" | "name">[]; shows: string
  /** The line shows the server the box draws from the daemon's word alone, which its name and its card say. */
  lastKnown?: boolean
  /** The slot is switching, by the daemon's view or a press of its owner's: `children` is the server it is going to, or its
   * skeleton, and `shows` the sentence that says it. */
  moving?: boolean
  children: ReactNode
}) {
  const now = useNow()
  const controls = useControls()
  const action = actions.switchTo(owner.exit)
  const press = controls.shown(action)
  const [openMenu, setOpenMenu] = useOpenMenu()
  const open = openMenu === slot.id
  const setOpen = (on: boolean) => setOpenMenu((at) => (on ? slot.id : at === slot.id ? null : at))
  const ownerOpen = openMenu !== null && followersOf(owner.exit, slots).some((s) => s.id === openMenu)
  const pick = useHeldPick(owner.view.preferred, now)
  const onThePick = pick !== null && onPick(slot, pick)
  /* From a press on the pick until the box lands, the words keep their room unseen, so no box moves at the press or at a failure;
   * landed, the box gives the room up only where the server it landed on holds no pick. */
  const [kept, setKept] = useState(false)
  const keeps = moving && (kept || onThePick)
  if (keeps !== kept) setKept(keeps)
  const lineId = useId()
  /* The line's card, which never stands over its open menu: a last known server's, or the Switching to sentence. It stays mounted,
   * so a read that comes and goes keeps the line's focus, and it ends when a read confirms the server: a card that goes from under
   * the pointer hears no leave, so only a new hover opens it again. */
  const [card, setCard] = useState(false)
  if (card && !lastKnown && !moving) setCard(false)
  /* How many presses this box sent, and how many it had sent when the menu last opened: each press makes the next opening a fresh
   * menu, its search empty and its first pickable row highlighted, and leaves the menu it closed to fade as it was. */
  const [presses, setPresses] = useState(0)
  const [opened, setOpened] = useState(0)
  /* The owner's last press's Test, for the minute the store keeps it. */
  const result = press.answer
  /* The answer the menu draws: the owner's while the menu is open, and once it closes the one it had, so a menu fading out draws no
   * new answer. */
  const [drawn, setDrawn] = useState(result)
  if (open && drawn !== result) setDrawn(result)
  const mine = `${slot.id} `
  const busy = press.phase === "out"
  const clearing = busy && press.way === `${mine}unpick`
  const waits = busy && !clearing
  /* Half strength is for another box's Clear: a box landed while a press is out takes no press at full strength. */
  const dimmed = waits && pressedServer(press) === null

  /* The menu closes at the press, and every box of the owner is Switching to the server from there until its slot lands. A pick
   * the owner's one write refuses asks nothing. The focus goes to the line in the same step, so from the press the keys are the
   * line's and not the fading menu's. A failure opens nothing, so nothing takes a key or a press meant for something else. */
  const choose = (server: LineageId) => {
    const key = controls.press(action, server, {
      what: servers.get(server)?.label ?? owner.view.name, way: mine + server,
      refused: (refusal) => (OWNER_REFUSALS.has(refusal.reason) ? { what: owner.view.name, open: undefined } : undefined),
    })
    if (key === null) return
    setPresses((n) => n + 1)
    document.getElementById(lineId)?.focus()
    setOpen(false)
  }
  const clear = (held: Preferred) => controls.press(actions.unpick(owner.exit), held, { what: servers.get(held.server)?.label ?? owner.view.name, way: `${mine}unpick` })
  /* A failed Test keeps the chevron: its dot here would stand beside the ping of the server the slot is still on. */
  const end = moving ? <Spinner role={undefined} aria-label={undefined} aria-hidden className="text-muted-foreground size-3.5 motion-reduce:animate-none" />
    : clearing && !open ? <Spinner role={undefined} aria-label={undefined} aria-hidden className="text-muted-foreground size-3" />
    : <ChevronDownIcon aria-hidden className="text-muted-foreground -mr-[3px] -ml-1 size-3" />
  const noPress = moving || waits
  const worded = onThePick || keeps
  const unseen = moving || ownerOpen
  const underWords = worded && !unseen
  const named = moving ? shows : `${lastKnown ? LAST_KNOWN : "Server"} of ${owner.view.name}: ${shows}`
  return (
    <span role={moving ? "status" : undefined} className="flex min-w-0 flex-1 flex-col">
      <Popover open={open} onOpenChange={(on) => {
        /* A press or a tap on a waiting line opens its card, as Pending's does. */
        if (moving) { if (on) setCard(true); return }
        if (!(on && waits)) { setOpen(on); if (on) { setOpened(presses); setCard(false) } }
      }}>
        <Tip lines={[moving ? { words: shows } : { name: LAST_KNOWN, words: shows }]} opens="hover" held={{ open: (lastKnown || moving) && card && !open, onOpenChange: setCard }}
          beside={under(underWords)} level={underWords}>
          <PopoverTrigger asChild>
            <Button id={lineId} type="button" variant="ghost" data-slot="switch-line" aria-label={named} aria-disabled={noPress || undefined} aria-busy={(clearing && !open) || undefined}
              onKeyDown={(e) => { if (e.key === " " ? e.repeat || noPress : e.key === "Enter" && e.repeat) e.preventDefault() }}
              className={cn("-mx-1.5 h-6 w-[calc(100%+0.75rem)] min-w-0 justify-start gap-1.5 px-1.5 text-left font-normal", ownerOpen && "bg-muted", noPress && "hover:bg-transparent dark:hover:bg-transparent", dimmed && "opacity-50")}>
              {children}
              <span className="ml-auto flex shrink-0 items-center gap-1.5">{end}</span>
            </Button>
          </PopoverTrigger>
        </Tip>
        <SwitchMenu key={opened} open={open} owner={owner} slots={slots} servers={servers} sources={sources} pick={pick} clearing={clearing} result={drawn} now={now} onPick={choose} onClear={clear} />
      </Popover>
      {/* The line's label, as wide as the line: a press on it is the line's and the pointer on it lights the line's ground, but
        * only the line opens its card. The words under the name's first letter: past the line's 1px border, the flag and its gap. */}
      {worded && (
        <label htmlFor={lineId} className={cn("-mx-1.5 block px-1.5", unseen && "invisible")}>
          <Quiet className="block h-4 truncate pl-[23px] leading-4">{PICK_WORDS}</Quiet>
        </label>
      )}
    </span>
  )
}

/** The menu of an owner's servers, in the Exit menu's frame and rows: its pool in the daemon's order, headed on the line of Ping
 * and Stability by the owner's name and, when two or more follow it, every slot a pick moves. The row the slots are on has the
 * violet dot and takes no press. After a failed Test that row's Ping cell is the Test's for its minute, with its reason under the
 * server's name. The pick, while it holds, is a chip on its row's foot, whose × is Clear. */
function SwitchMenu({ open, owner, slots, servers, sources, pick, clearing, result, now, onPick, onClear }: {
  open: boolean; owner: Owner; slots: readonly SlotView[]; servers: Map<LineageId, ServerView>; sources: Pick<SourceView, "id" | "name">[]
  pick: Preferred | null; clearing: boolean; result: ProbeResult | null; now: number; onPick: (server: LineageId) => void; onClear: (pick: Preferred) => void
}) {
  const [search, setSearch] = useState("")
  const cards = useOptionCards()
  const followers = followersOf(owner.exit, slots)
  /* The violet mark says what the runtime reports in use. A row is kept, and the pick's chip stands on it, on what the boxes draw,
   * a last known server included. */
  const inUse = new Set(followers.flatMap((s) => (s.appliedServer ? [s.appliedServer] : [])))
  const drawn = new Set(followers.flatMap((s) => drawnServer(s) ?? []))
  const pool = owner.view.order.flatMap((id) => servers.get(id) ?? [])
  const sourceName = (s: ServerView) => sources.find((x) => x.id === s.source)?.name ?? "Removed"
  const on = (id: LineageId) => followers.filter((s) => s.appliedServer === id).map((s) => s.name).join(", ")
  /* The chip goes with its pick, run out or cleared from anywhere, and its card with it, so the pick made again opens none. */
  const chipped = pool.some((s) => drawn.has(s.id) && pick?.server === s.id) ? pick : null
  const chipCard = cards.held("pick")
  if (chipCard.open && !chipped) chipCard.onOpenChange(false)
  return (
    <MenuFrame open={open} search={search} setSearch={setSearch} cards={cards}>
      <CommandEmpty className="px-2.5 py-1.5 text-left"><NoMatch /></CommandEmpty>
      <CommandGroup heading={<Head title={<><span className="text-foreground">{owner.view.name}</span>{followers.length > 1 && ` · ${listed(followers.map((s) => s.name))}`}</>} />}>
        {pool.map((s) => {
          const row = switchRow(s.id, { inUse: drawn, clearing })
          const held = chipped?.server === s.id ? chipped : null
          const failure = result?.lineage === s.id ? testWords(result) : null
          const answer = failure && result && <span className="flex justify-end"><TestAnswer result={result} carded={false} /></span>
          const mark = inUse.has(s.id) ? <CarryingMark label={`In use: ${on(s.id)}`} /> : null
          return (
            <ServerOption key={s.id} server={s} source={sourceName(s)} mark={mark} under={failure && <Quiet>{failure.reason}</Quiet>} disabled={row !== "open"} className={cn(row === "kept" && KEPT)} onSelect={() => onPick(s.id)}
              foot={held && <PickChip pick={held} server={s} now={now} clearing={clearing} onClear={() => onClear(held)} card={chipCard} highlight={cards.setHighlighted} />}>
              {s.udp === "failed" && <Quiet className="shrink-0">UDP failed</Quiet>}
              <ServerStats server={s} now={now} hover={cards.held(`server:${s.id}`)} answer={answer} />
            </ServerOption>
          )
        })}
      </CommandGroup>
    </MenuFrame>
  )
}

/** A part of the pick's chip as a phone reaches it: the foot's 44 px, wholly under the row's first line, its padding box 22 px
 * inside its border and 11 px either side. */
const TARGET = "relative after:absolute after:inset-0 @max-[24rem]/menu:after:-inset-y-[11px]"

/** The picked row's foot: the pick as one chip of the filter chip's parts at the small size, named Manual, its word and its ×. The
 * word is a fact whose one card says when the pick wears off, one of the menu's cards (`card`). The × is Clear. They are two
 * stops side by side, the word first, with the menu's keys (`Menu`). With the pointer on the ×, as with the focus, no row is
 * highlighted, so Clear is the one thing that looks chosen, and that holds while its press is out, since every row then waits and
 * takes no highlight. Pressed, its spinner stands in the ×'s place. On a phone the two targets fill the foot's 44 px and meet at
 * the chip's divider: the word's as wide as the word, the ×'s from its own left edge 45 px wide, so a tap a little off the word
 * never clears. */
function PickChip({ pick, server, now, clearing, onClear, card, highlight }: {
  pick: Preferred; server: ServerView; now: number; clearing: boolean; onClear: () => void; card: Held; highlight: (row: string) => void
}) {
  const { shown, heard } = wearsOff(pick.endsAt, now)
  return (
    <span className="flex h-6 min-w-0 flex-1 items-center @max-[24rem]/menu:h-11">
      <ButtonGroup aria-label={REASON_WORDS.preferred}>
        <Fact asChild lines={[{ name: "Auto-resets", words: shown }]} label={`Manual override. Auto-resets ${heard}`} held={card}>
          <Button type="button" size="sm" variant="outline" className={cn(TARGET, "font-normal")}>{REASON_WORDS.preferred}</Button>
        </Fact>
        <Button type="button" size="icon-sm" variant="outline" aria-label={`Clear manual override on ${server.label}`} aria-busy={clearing || undefined}
          className={cn(TARGET, "@max-[24rem]/menu:after:-right-[22px]")}
          onPointerEnter={() => highlight("")}
          onClick={(e) => { e.stopPropagation(); if (!clearing) onClear() }}>
          {clearing ? <Spinner role={undefined} aria-label={undefined} aria-hidden /> : <XIcon aria-hidden />}
        </Button>
      </ButtonGroup>
    </span>
  )
}
