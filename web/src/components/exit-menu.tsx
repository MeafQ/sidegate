import { useLayoutEffect, useRef, useState, type ComponentProps, type ReactNode, type SyntheticEvent } from "react"
import { Link, useNavigate } from "react-router"
import { BanIcon, CheckIcon, ChevronsUpDownIcon, CircleXIcon, GlobeIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { defaultFilter, useCommandState } from "cmdk"
import { CommandEmpty, CommandGroup, CommandItem, CommandList } from "@/components/ui/command"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Flag } from "@/components/flag"
import { NoMatch, ReadFailed, failureOf, type FailedRead, type Read } from "@/components/page"
import { ListSearch, Menu, moves, searchOf } from "@/components/pickers"
import { useStandings, type Exitable, type Standing } from "@/components/nav"
import { cardLines, GroupDot, RatingDot, STABILITY_HEADER, StabilityBars } from "@/components/server"
import { ageLine, PingFigures, Quiet, RatingMark, TONE_TEXT } from "@/components/status"
import { besideMenu, Fact, OptionFact, type Held } from "@/components/card"
import { stabilityOf, type Stability } from "@/lib/failures"
import { pingAge, serverStanding, STANDING_WORDS } from "@/lib/health"
import { useNow } from "@/lib/clock"
import { groupState, noServers, UNRATED } from "@/lib/troubles"
import type { Exit, GroupView, ServerView, SourceView } from "@/lib/schema"
import { exitServers, exitWords, sameExit } from "@/lib/slots"
import { cn } from "@/lib/utils"

/* The Servers table's facts, in its order, both against the right edge: a group's dot sums up the rest, with the reason on hover.
 * One width per column, so groups and servers line up. In the menu a phone gets, Ping is the p50 alone, as the table's is on a
 * phone, so a name keeps at least 182px of a 358px menu. */
export const COL = { ping: "w-24 @max-[24rem]/menu:w-12", stability: "w-16" }
const PING_WIDE = "@max-[24rem]/menu:hidden"
export const CELLS = "flex shrink-0 items-center justify-end text-right whitespace-nowrap tabular-nums [&>span]:shrink-0"
/* A card stands beside the menu, level with its option or its section's header, so it covers neither. */
const BESIDE_OPTION = besideMenu("[cmdk-item]")
const BESIDE_HEAD = besideMenu("[cmdk-group-heading]")
/* The item adds its own invisible check after the row, which would push the columns out of line with the headers. */
export const NO_TRAILING_CHECK = "[&>svg:last-child]:hidden"

/** What starts an option: its dot, or Direct's and Block's mark, and on the picked option the check in its place, so the check
 * takes no room of its own from the name. */
export function Lead({ chosen, children }: { chosen: boolean; children: ReactNode }) {
  return <span className="flex w-2 shrink-0 justify-center">{chosen ? <CheckIcon className="size-3.5 shrink-0" aria-hidden /> : children}</span>
}

/** A section's header: its name, and Ping and Stability over their cells. A name that wraps keeps them on its last line. */
export function Head({ title }: { title: ReactNode }) {
  return (
    <span className="flex items-end">
      <span className="flex-1 self-center">{title}</span>
      <span className={CELLS}>
        <span className={COL.ping}>Ping</span>
        <Fact lines={STABILITY_HEADER} label="Stability" side="right" beside={BESIDE_HEAD} className={cn(COL.stability, "-my-1.5 shrink-0 items-end justify-center")}>Stability</Fact>
      </span>
    </span>
  )
}

/** Which of the menu's cards is open and why: the keyboard's, which is the highlighted option's, or the one card its trigger
 * opened, by the pointer, a tap or the focus. One state, so the last of the two to act has the only open card. */
type Opened = { by: "keys" } | { by: "trigger"; card: string } | null
const endKeys = (o: Opened): Opened => (o?.by === "keys" ? null : o)

/** Which of the menu's cards is open, held by the menu so one is open at a time: an option's, or that of a control on an option's
 * foot, as a pick's chip. `held` gives a card its state. */
export function useOptionCards() {
  const [highlighted, setHighlighted] = useState("")
  const [opened, setOpened] = useState<Opened>(null)
  const held = (card: string): Held => ({
    open: opened?.by === "keys" ? highlighted === card : opened?.card === card,
    onOpenChange: (on) => setOpened((o) => (on ? { by: "trigger", card } : o?.by === "trigger" && o.card === card ? null : o)),
  })
  return { highlighted, setHighlighted, setOpened, held }
}
type OptionCards = ReturnType<typeof useOptionCards>

/** An option's Ping and Stability under one card, in the columns' order, which opens over either cell. The option's words carry
 * the card's sentence. Its tap area is the row's height. `answer` stands in the Ping cell in place of the ping: a Test's answer
 * or its spinner. */
function Stats({ health, age, stability, hover, answer }: { health: ServerView["health"] | null; age: string | null; stability: Stability; hover: Held; answer?: ReactNode }) {
  const lines = [...cardLines(stability), ...(age ? [ageLine(`Ping measured ${age}`, age)] : [])]
  const p50 = health?.pingP50Ms ?? null
  const p95 = health?.pingP95Ms ?? null
  const ping = answer ?? (p50 === null || p95 === null ? "—" : <PingFigures p50={p50} p95={p95} p95ClassName={PING_WIDE} />)
  return (
    <OptionFact lines={lines} side="right" beside={BESIDE_OPTION} {...hover} className={cn(CELLS, "-my-1.5 py-1.5")}>
      <span className={cn(COL.ping, (age !== null || ping === "—") && "text-muted-foreground")}>{ping}</span>
      <span className={cn(COL.stability, "flex justify-end")}><StabilityBars bars={stability.bars} /></span>
      <span className="sr-only">{stability.heard}</span>
    </OptionFact>
  )
}

export function ServerStats({ server, now, hover, answer }: { server: ServerView; now: number; hover: Held; answer?: ReactNode }) {
  const age = pingAge(server.health, now)
  return <Stats health={server.health} age={answer ? null : age} stability={stabilityOf(server.health, now)} hover={hover} answer={answer} />
}

/** A group's or source's facts are its first choice's, and a first choice the daemon has no record of stands where every server
 * starts. One with no first choice has none. */
function GroupStats({ group, servers, now, hover }: { group: Parameters<typeof groupState>[0]; servers: Map<ServerView["id"], ServerView>; now: number; hover: Held }) {
  const { first } = groupState(group, servers)
  if (first) return <ServerStats server={first} now={now} hover={hover} />
  if (group.firstChoice) return <Stats health={null} age={null} stability={stabilityOf(null, now)} hover={hover} />
  return <Dashes />
}

/** A number still to come: a spinner where the ping will stand, and unlit bars. */
const PendingStats = () => (
  <span className={CELLS}>
    <span className={cn(COL.ping, "flex justify-end")}><Spinner role={undefined} aria-label={undefined} aria-hidden className="text-muted-foreground size-3.5" /></span>
    <span className={cn(COL.stability, "flex justify-end")}><StabilityBars bars={0} /></span>
  </span>
)

/** Server options on their way: each keeps a server row's places, its faint dot, its name's and its numbers'. They stand whatever
 * the search holds, since what it will match isn't known yet. */
function PendingOptions() {
  return (
    <CommandGroup forceMount heading={<Head title="Servers" />}>
      {["w-28", "w-20", "w-24"].map((width) => (
        <CommandItem key={width} forceMount disabled value={`reading:${width}`} className={NO_TRAILING_CHECK}>
          <Lead chosen={false}><RatingMark rating="none" /></Lead>
          <Skeleton className={cn("h-2 rounded-full", width)} />
          <span className="flex-1" />
          <PendingStats />
        </CommandItem>
      ))}
    </CommandGroup>
  )
}

/** A number its read never brought: the muted cross where the spinner stood, and unlit bars. */
const FailedStats = () => (
  <span className={CELLS}>
    <span className={cn(COL.ping, "text-muted-foreground flex justify-end")}><CircleXIcon aria-hidden className="size-3.5" /></span>
    <span className={cn(COL.stability, "flex justify-end")}><StabilityBars bars={0} /></span>
  </span>
)

const Dashes = () => (
  <span className={CELLS}>
    <Quiet className={COL.ping}>—</Quiet>
    <Quiet className={COL.stability}>—</Quiet>
  </span>
)

/** A source or group in the menu, with its dot and, when it can't be picked, its Trouble words on a line of their own under its
 * name, as the list entry has them, so a name is cut only by its own length. An Exit with no servers can't be picked: Open stands
 * at its end, and a press anywhere on it, or Enter, opens its page, as Open does. Open is no stop of its own, since the row is.
 * A source whose first refresh runs can't be picked either, and is Pending: its faint dot, and a spinner and unlit bars where its
 * numbers will stand. */
function ExitOption({ row, standing, chosen, stats, onPick, onOpen, list }: {
  row: Exitable; standing: Standing; chosen: boolean; stats: ReactNode; onPick: () => void; onOpen: () => void; list: Read<unknown>
}) {
  const navigate = useNavigate()
  /* A source has none when its standing says so, which counts present servers once the server list answers, as the daemon
   * does. A group's are its own rule's, read with the view. */
  const empty = row.kind === "group" ? row.group.resolved.length === 0 : noServers(standing.troubles)
  if (standing.dot.kind === "first") {
    return (
      <CommandItem className={NO_TRAILING_CHECK} value={row.id} keywords={[row.name]} disabled>
        <Lead chosen={chosen}><GroupDot state={standing.dot} /></Lead>
        <span className="text-muted-foreground min-w-0 flex-1 truncate">{row.name}</span>
        <PendingStats />
      </CommandItem>
    )
  }
  return (
    <CommandItem className={cn(NO_TRAILING_CHECK, empty && "items-start")} value={row.id} keywords={[row.name]}
      onSelect={() => { if (!empty) return onPick(); onOpen(); navigate(row.to) }}>
      <span className="flex h-[1.625em] shrink-0 items-center"><Lead chosen={chosen}><GroupDot state={standing.dot} /></Lead></span>
      <span className="flex min-w-0 flex-1 flex-col">
        <span className={cn("truncate", empty && "text-muted-foreground")}>{row.name}</span>
        {empty && standing.troubles.length > 0 && (
          <span className="text-muted-foreground">{standing.troubles.map((t, i) => <span key={t.kind} className={TONE_TEXT[t.tone]}>{i > 0 && " · "}{t.words}</span>)}</span>
        )}
      </span>
      {empty ? (
        /* The row's own press opens the page, so Open's click stops before it and the page opens once. */
        <Button asChild size="sm" variant="outline" className="self-center">
          <Link to={row.to} tabIndex={-1} onClick={(e) => { e.stopPropagation(); onOpen() }}>Open</Link>
        </Button>
      ) : list.state === "read" ? stats : list.state === "failed" ? <FailedStats /> : <PendingStats />}
    </CommandItem>
  )
}

/** A server as a menu lists it: its dot, or the check on the picked one, its flag, a mark before its name where the menu has one,
 * its name and source on up to two lines, the source giving way first, and what the menu puts at its end, level with the first
 * line. `under` stands on a line of its own under the name, as an Exit's Trouble words do, pushing the rows below down. `foot` is
 * the row's last line, from the name's column to the row's end, for a control of its own (`OptionFoot`). */
export function ServerOption({ server, source, chosen = false, mark, under, foot, className, children, ...item }: {
  server: ServerView; source: string; chosen?: boolean; mark?: ReactNode; under?: ReactNode; foot?: ReactNode; children: ReactNode
} & Omit<ComponentProps<typeof CommandItem>, "value" | "keywords" | "children">) {
  const lead = <><Lead chosen={chosen}><RatingDot server={server} /></Lead><Flag code={server.country.code} />{mark}</>
  const value = `server:${server.id}`
  const keywords = [server.label, source]
  return (
    <>
      <CommandItem className={cn(NO_TRAILING_CHECK, "items-start", className)} value={value} keywords={keywords} {...item}>
        <span className="flex h-[1.625em] shrink-0 items-center gap-2">{lead}</span>
        {/* The source comes last, so it is what two lines cut first. */}
        <span className="flex min-w-0 flex-1 flex-col">
          <span className="line-clamp-2 [overflow-wrap:anywhere]">
            <span data-slot="option-name">{server.label}</span>
            <Quiet> · {source}</Quiet>
          </span>
          {under}
        </span>
        {children}
      </CommandItem>
      {foot && <OptionFoot value={value} keywords={keywords} lead={lead}>{foot}</OptionFoot>}
    </>
  )
}

/** An option's last line, which holds a control: outside the option, so the control is no part of the option's press or of its
 * disabled state, and level with it, from the name's column to the row's end, 4px under its first line. It goes with its option
 * when the search hides it, by cmdk's own filter. */
function OptionFoot({ value, keywords, lead, children }: { value: string; keywords: string[]; lead: ReactNode; children: ReactNode }) {
  const search = useCommandState((state) => state.search)
  if (search && defaultFilter(value, search, keywords) <= 0) return null
  return (
    <div data-slot="option-foot" className="-mt-0.5 flex gap-2 px-2.5 pb-1.5 text-xs/relaxed @max-[24rem]/menu:mt-0">
      <span aria-hidden className="invisible flex shrink-0 gap-2">{lead}</span>
      {children}
    </div>
  )
}

/** The read of a menu's list that failed, under its search and outside the list, so cmdk's highlight, filter and groups never
 * meet it: the reason and Retry on one line, whatever the search holds. Retry has the menu's keys (`Menu`). It holds
 * the keys only while it can act: once its read is out, or the read lands and it goes, they are back in the search. Named for
 * what it lists, as a dialog's failed row stands in its named section. */
function FailedList({ what, read: failed }: ListFailure) {
  const row = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    const el = row.current
    const giveBack = () => { if (el?.contains(document.activeElement)) searchOf(el)?.focus() }
    if (failed.retrying) giveBack()
    return giveBack
  }, [failed.retrying])
  return (
    <div ref={row} role="group" aria-label={what} className="flex min-h-9 items-center px-3.5 pt-1">
      <ReadFailed {...failed} place="row" />
    </div>
  )
}

/** The failed read of what a menu lists, and the name of what that is. */
type ListFailure = { what: string; read: FailedRead }

/** A key or a press part that comes to a menu fading out ends there: the menu does nothing with it, and nothing under it hears it. */
const takeNothing = (e: SyntheticEvent<HTMLElement>) => {
  if (e.currentTarget.dataset.state !== "closed") return
  e.preventDefault()
  e.stopPropagation()
}
const CLOSING_TAKES_NOTHING = {
  onKeyDownCapture: takeNothing, onPointerDownCapture: takeNothing, onPointerUpCapture: takeNothing,
  onMouseDownCapture: takeNothing, onMouseUpCapture: takeNothing, onClickCapture: takeNothing,
}

/** A menu's frame: the popover a phone gets 16px in from its edges, the search, the failed read of what it lists if it failed, and
 * the list in what's left of the screen, with one card of its rows open at a time. A tap or a click on that card's face closes it
 * and does nothing else: the card stands outside the menu's frame, and its click would reach its row through React's tree and
 * pick it. Every pick in the menu closes it, and from its first pick the menu takes no other: closed, by a pick or any other way,
 * it fades as every popover does and takes no key or press until it has gone. The popover's own state says the menu is closing,
 * so the rule needs no word from a pick, and a list a pick leaves open, as a filter's checklist, takes its next pick. Opened, the
 * menu has the keys in its search, opened again as it fades too, where the popover, still mounted, gives the focus nothing. */
export function MenuFrame({ open, search, setSearch, cards, failed = null, children }: { open: boolean; search: string; setSearch: (search: string) => void; cards: OptionCards; failed?: ListFailure | null; children: ReactNode }) {
  const { setOpened } = cards
  const content = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => { if (open) content.current?.querySelector<HTMLElement>("[cmdk-input]")?.focus({ preventScroll: true }) }, [open])
  return (
    <PopoverContent ref={content} align="start" collisionPadding={16} className="@container/menu w-[min(30rem,calc(100vw-2rem))] p-0" {...CLOSING_TAKES_NOTHING}>
      <Menu highlighted={cards.highlighted} onHighlight={cards.setHighlighted} onKeyDown={(e) => setOpened(moves(e) ? { by: "keys" } : endKeys)} onPointerMove={() => setOpened(endKeys)} onPointerDown={() => setOpened(endKeys)}
        onClickCapture={(e) => {
          if (e.target instanceof Node && e.currentTarget.contains(e.target)) return
          e.stopPropagation()
          setOpened(null)
        }}>
        <ListSearch placeholder="Search" value={search} onValueChange={setSearch} />
        {failed && <FailedList {...failed} />}
        {/* What's left of the screen after the search. */}
        <CommandList className="max-h-[min(20rem,calc(var(--radix-popover-content-available-height)-3rem))]">{children}</CommandList>
      </Menu>
    </PopoverContent>
  )
}

/** Assigns an Exit: a group, a server that passes its filter, Direct or Block. */
export function ExitMenu({ value, onChange, groups, servers, sources, list, label, busy = false, className }: {
  value: Exit
  onChange: (exit: Exit) => void
  groups: GroupView[]
  servers: Map<ServerView["id"], ServerView>
  sources: SourceView[]
  /** The server list's read: until it answers its servers are Pending, and once it failed its reason and Retry stand under the search. */
  list: Read<unknown>
  label?: string
  /** A pick is out: the menu takes no other, and the value stays at full strength, since the line under it is what waits. */
  busy?: boolean
  className?: string
}) {
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState("")
  const failed = failureOf(list)
  const cards = useOptionCards()
  const { held, setOpened } = cards
  const listed = exitServers(servers.values())
  const now = useNow()
  const { rows, standings } = useStandings()
  const sourceRows = rows.flatMap((r) => (r.kind === "source" && sources.some((x) => x.id === r.source.id) ? [r] : []))
  const groupRows = rows.flatMap((r) => (r.kind === "group" && groups.some((g) => g.id === r.group.id) ? [r] : []))
  const standingOf = (row: Exitable): Standing => standings.get(row.id) ?? { dot: UNRATED, troubles: [] }
  const sourceName = (s: ServerView) => sources.find((x) => x.id === s.source)?.name ?? "Removed"
  const toggle = (on: boolean) => {
    if (on && busy) return
    setOpen(on)
    setOpened(null)
  }
  const pick = (exit: Exit) => {
    toggle(false)
    setSearch("")
    if (!sameExit(exit, value)) onChange(exit)
  }
  return (
    <Popover open={open} onOpenChange={toggle}>
      <PopoverTrigger asChild>
        <Button type="button" variant="outline" role="combobox" aria-expanded={open} aria-label={label} aria-disabled={busy || undefined} className={cn("min-w-0 justify-between font-normal", className)}>
          <span className="flex min-w-0 items-center gap-1.5">
            {value.kind === "direct" && <GlobeIcon className="text-muted-foreground" aria-hidden />}
            {value.kind === "block" && <BanIcon className="text-muted-foreground" aria-hidden />}
            {value.kind === "server" && servers.get(value.server) && <Flag code={(servers.get(value.server) as ServerView).country.code} />}
            {value.kind === "server" && list.state === "waiting" ? <Skeleton aria-label="Loading server…" className="h-2 w-20 rounded-full" />
              : value.kind === "server" && !servers.has(value.server) && failed ? <CircleXIcon className="text-muted-foreground" aria-hidden />
              : <span className="truncate">{exitWords(value, groups, sources, servers)}</span>}
          </span>
          <ChevronsUpDownIcon className="text-muted-foreground" aria-hidden />
        </Button>
      </PopoverTrigger>
      <MenuFrame open={open} search={search} setSearch={setSearch} cards={cards} failed={failed && { what: "Server list", read: failed }}>
        {list.state === "read" && !failed && <CommandEmpty className="px-2.5 py-1.5 text-left"><NoMatch /></CommandEmpty>}
        <CommandGroup>
          <CommandItem value="direct" keywords={["Direct"]} onSelect={() => pick({ kind: "direct" })}>
            <Lead chosen={value.kind === "direct"}><GlobeIcon className="text-muted-foreground -mx-0.5" aria-hidden /></Lead>
            <span className="flex-1">Direct</span>
          </CommandItem>
          <CommandItem value="block" keywords={["Block"]} onSelect={() => pick({ kind: "block" })}>
            <Lead chosen={value.kind === "block"}><BanIcon className="text-muted-foreground -mx-0.5" aria-hidden /></Lead>
            <span className="flex-1">Block</span>
          </CommandItem>
        </CommandGroup>
        {sourceRows.length > 0 && (
          <CommandGroup heading={<Head title="Sources" />}>
            {sourceRows.map((row) => {
              const counted = { ...row.source, resolved: listed.filter((s) => s.source === row.source.id).map((s) => s.id) }
              return <ExitOption key={row.id} row={row} standing={standingOf(row)} chosen={value.kind === "source" && value.source === row.source.id} onOpen={() => toggle(false)} list={list}
                onPick={() => pick({ kind: "source", source: row.source.id })} stats={<GroupStats group={counted} servers={servers} now={now} hover={held(row.id)} />} />
            })}
          </CommandGroup>
        )}
        {groupRows.length > 0 && (
          <CommandGroup heading={<Head title="Groups" />}>
            {groupRows.map((row) => (
              <ExitOption key={row.id} row={row} standing={standingOf(row)} chosen={value.kind === "group" && value.group === row.group.id} onOpen={() => toggle(false)} list={list}
                onPick={() => pick({ kind: "group", group: row.group.id })} stats={<GroupStats group={row.group} servers={servers} now={now} hover={held(row.id)} />} />
            ))}
          </CommandGroup>
        )}
        {list.state === "waiting" && <PendingOptions />}
        {listed.length > 0 && (
          <CommandGroup heading={<Head title="Servers" />}>
            {listed.map((s) => (
              <ServerOption key={s.id} server={s} source={sourceName(s)} chosen={value.kind === "server" && value.server === s.id} disabled={serverStanding(s) === "unsupported"} onSelect={() => pick({ kind: "server", server: s.id })}>
                {serverStanding(s) !== "usable" ? <Quiet>{STANDING_WORDS[serverStanding(s)]}</Quiet> : (
                  <>
                    {s.udp === "failed" && <Quiet className="shrink-0">UDP failed</Quiet>}
                    <ServerStats server={s} now={now} hover={held(`server:${s.id}`)} />
                  </>
                )}
              </ServerOption>
            ))}
          </CommandGroup>
        )}
      </MenuFrame>
    </Popover>
  )
}
