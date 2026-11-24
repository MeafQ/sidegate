import { useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { useLocation, useNavigate } from "react-router"
import { ClipboardPasteIcon, CheckIcon, FlagIcon, ListFilterIcon, PlusIcon, RefreshCwIcon, RssIcon, SearchIcon, ServerIcon, SquareCheckIcon, XIcon, type LucideIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupInput } from "@/components/ui/input-group"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { useAdd, useNewGroup } from "@/components/app-shell"
import { Fact, Tip, type Held, type Line } from "@/components/card"
import { DeleteDialog } from "@/components/delete-dialog"
import { Drawn, type Drawing, type FacetOption } from "@/components/facet"
import { FilterHeader, FilterLine, NOT_SET, anySet, type FieldKey, type FieldValue, type FilterField, type Pattern, type RuleKey } from "@/components/filter-chips"
import { countryDrawing } from "@/components/flag"
import { GroupDetails, NEW_GROUP, NewGroupDetails, NO_MEMBERS, type Busy, type GroupShown } from "@/components/group-form"
import { useExitables, type Exitable } from "@/components/nav"
import { EmptyState, Loaded, NoMatch, ReadFailed, Toolbar, type Read } from "@/components/page"
import { CarryingMark, GroupDot, STABILITY_HEADER, StabilityBars, StabilityCell } from "@/components/server"
import { ServerDetails, ServerTest } from "@/components/server-details"
import { ServerStats, useServerWeek } from "@/components/server-stats"
import { NotFound } from "@/components/slot-board"
import { SourceDetails } from "@/components/source-details"
import { DeleteButton, RefreshFailure, SkippedMark, SourceHeading, useRefresh, type Refreshing, type SkippedFix } from "@/components/source-heading"
import { ageLine, PingFigures, Quiet, RatingMark, Toned } from "@/components/status"
import { ApiError, errorWords } from "@/lib/api"
import { PROTOCOL_WORDS, TRANSPORT_WORDS, countryName } from "@/lib/format"
import { stabilityOf } from "@/lib/failures"
import { skippedOf } from "@/lib/feed"
import { RATING_WORDS, STANDING_WORDS, pingAge, pingPair, rate, serverStanding, type Rating, type ServerRating } from "@/lib/health"
import { feedOrder } from "@/lib/outbound"
import { groupsByHome, sectionsOf, type Section } from "@/lib/sections"
import { useNow } from "@/lib/clock"
import { stateRead, useGroupPreview, useSettled, useView } from "@/lib/queries"
import type { Box as BoxCondition, Exit, GroupView, Members, Protocol, ServerView, SlotView, SourceId, SourceView, View } from "@/lib/schema"
import { groupState } from "@/lib/troubles"
import { cn } from "@/lib/utils"

/** A group's heading, in one line: its dot, its name and its troubles in their words and colours, as a source's heading, then
 * Delete. The table below shows each server's dot and ping. A group has no week, so its failover trouble stands alone. */
/** A group's heading. Its Delete waits while another write of the group is out (`deleteWaits`). */
function GroupHeader({ group, name, servers, onDelete, deleteWaits }: { group: GroupView; name: ReactNode; servers: Map<ServerView["id"], ServerView>; onDelete: () => void; deleteWaits: boolean }) {
  const state = groupState(group, servers)
  return (
    <div data-slot="group-heading" className="flex min-w-0 flex-wrap items-center gap-x-2">
      <GroupDot state={state} />
      {name}
      {state.troubles.map((t) => <Toned key={t.kind} tone={t.tone} className="ml-3">{t.words}</Toned>)}
      <span className="ml-auto shrink-0">
        <DeleteButton name={group.name} disabled={deleteWaits} onDelete={onDelete} />
      </span>
    </div>
  )
}

/** Whether a slot's Exit is this source or group. */
const isExit = (row: Exitable, exit: Exit) =>
  row.kind === "source" ? exit.kind === "source" && exit.source === row.source.id : exit.kind === "group" && exit.group === row.group.id

/** The servers slots are on right now through this source or group. */
const inUseThrough = (row: Exitable, slots: SlotView[]) => new Set(slots.flatMap(({ exit, appliedServer }) => (isExit(row, exit) && appliedServer ? [appliedServer] : [])))

/** Which slots a server carries, and through what. Group or source membership alone isn't use. On `here`'s own tab, a slot whose
 * Exit is `here` needs no "via". */
function carrying(server: ServerView, slots: SlotView[], view: View, here?: Exitable): string[] {
  return slots.flatMap((slot) => {
    if (slot.exit.kind === "server") return slot.exit.server === server.id ? [slot.name] : []
    if (slot.appliedServer !== server.id) return []
    const { exit } = slot
    if (here && isExit(here, exit)) return [slot.name]
    const via = exit.kind === "group" ? view.groups.find((g) => g.id === exit.group)?.name : exit.kind === "source" ? view.sources.find((s) => s.id === exit.source)?.name : undefined
    return via ? [`${slot.name} via ${via}`] : [slot.name]
  })
}

/** The filters that hide rows and are never saved: All servers'. */
type ViewKey = "name" | "status" | "country" | "source" | "protocol"

/** Every server, with filters that only hide. While one a rule can hold is set, the chip line ends in Save as group, which makes a
 * group of those conditions and opens it to be named. Status stays behind, since no rule holds one. */
function AllServers({ servers, list, view }: { servers: ServerView[]; list: Read<unknown>; view: View }) {
  const newGroup = useNewGroup()
  const [name, setName] = useState<Pattern>({ include: "", exclude: "" })
  const named = useNamed(name)
  const [status, setStatus] = useState(NOT_SET)
  const [countries, setCountries] = useState(NOT_SET)
  const [sources, setSources] = useState(NOT_SET)
  const [protocols, setProtocols] = useState(NOT_SET)
  const fields: FieldValue<ViewKey>[] = [
    { kind: "pattern", key: "name", value: name, refusal: named.refusal, onChange: setName },
    { kind: "set", key: "status", value: status, onChange: setStatus },
    { kind: "set", key: "country", value: countries, onChange: setCountries },
    { kind: "set", key: "source", value: sources, onChange: setSources },
    { kind: "set", key: "protocol", value: protocols, onChange: setProtocols },
  ]
  const rule: Members = {
    sources: { not: sources.not, values: sources.values as SourceId[] }, countries, protocols: { not: protocols.not, values: protocols.values as Protocol[] },
    include: name.include, exclude: name.exclude, box: null,
  }
  /* A pattern Go refuses makes no group: its chip is ringed, its editor says why, and the line's end is empty. */
  const saveAs = !anySet(fields.filter((f) => f.key !== "status")) || named.refusal !== null ? null
    : <Button variant="outline" className="ml-auto" disabled={newGroup.creating || !named.current} onClick={() => newGroup.create(view.groups, rule)}>Save as group</Button>
  return <ServerBrowser pool={servers} list={list} view={view} order={BEST} filters={{ kind: "view", fields, end: saveAs, named: named.matched }} />
}

/** Which servers a name pattern matches, as the daemon reads it, so the rows it leaves are those a group saved from it holds.
 * The last answer stays while the next is read. `matched` is null while no pattern is set, before the first answer, and while
 * the daemon refuses the pattern. `current` says the answer shown is for the pattern as typed, over the rows on screen. */
function useNamed(name: Pattern) {
  const asked = useSettled(name)
  const set = asked.include !== "" || asked.exclude !== ""
  const preview = useGroupPreview({ ...NO_MEMBERS, include: asked.include, exclude: asked.exclude }, set)
  const read = preview.state === "read" ? preview.matched : null
  const matched = useMemo(() => (read ? new Set<string>(read) : null), [read])
  const typed = name.include !== "" || name.exclude !== ""
  const answered = asked.include === name.include && asked.exclude === name.exclude && preview.state === "read" && preview.current
  return {
    matched: typed && set ? matched : null,
    refusal: typed && preview.state === "failed" ? (preview.error instanceof ApiError ? errorWords(preview.error) : "Preview failed") : null,
    current: !typed || answered,
  }
}

/** A source's servers, every one it lists. Search alone narrows them: All servers with "Source is" filters them. Its skipped
 * entries' fix is the provider's support, when it sends one, or the pasted links. */
function SourceServers({ here, servers, list, view, pasteAgain }: { here: Extract<Exitable, { kind: "source" }>; servers: ServerView[]; list: Read<unknown>; view: View; pasteAgain: () => void }) {
  const fix = skippedFix(here.source, pasteAgain)
  return <ServerBrowser pool={servers} list={list} view={view} order={FEED} source={here.source} here={here} skipped={{ ...skippedOf(here.source), fix }} />
}

/** What a tab's rule filters do, all alike, so a tab can't mix a filter that hides with one that saves. On All servers they
 * hide rows and nothing saves them, and `end` can make a group of them. On a group's tab they are its saved rule, the box
 * column's among them: they dim the rows it leaves out, and `end` saves it. Hiding rows there is the search's alone. A source's
 * tab has none. */
type Filters =
  | { kind: "view"; fields: FieldValue<ViewKey>[]; end: ReactNode; named: ReadonlySet<string> | null }
  | { kind: "rule"; fields: FieldValue<RuleKey>[]; end: ReactNode }

/** What a table has beyond its dot, Country, Name, Protocol, Ping, Stability, In use and Test. A source's own table has no Source,
 * and only a group's has boxes. */
type Shape = { ofSource: boolean; ofGroup: boolean }

/** A row outside the group, but for its box: its words muted, as readable as any muted word, its flag and dot as they are. */
const OUTSIDE = "text-muted-foreground"
/** A group's boxes: whether each server's row stands dimmed, whether it is ticked, whether its box is busy, and the click that
 * ticks or unticks it. */
type Box = { dimmed: (s: ServerView) => boolean; isTicked: (s: ServerView) => boolean; busy: (s: ServerView) => Busy | undefined; tick: (s: ServerView) => void; control: (s: ServerView) => string }

/** What drawing a value reads: the names of the list's sources, and on a group's tab whether each server is ticked. */
type Names = { view: View; box?: Box }

/** What each column reads from the table around it. */
type Ctx = Names & {
  /** When the page drew, which the Stability cells read. */
  now: number
  /** Whether a row sits under its home config's heading, so its name is its tag alone. */
  underHome: (s: ServerView) => boolean
  /** The source or group whose tab this is. */
  here?: Exitable
  /** Servers that slots use right now through `here`. */
  active?: Set<ServerView["id"]>
  open: (s: ServerView) => void
}

/** One filter. `values` says which a server has and how each is drawn: its column's cells draw with `draw` too, so an option and a
 * chip look like the cell they filter. `offered` lists the values to offer, in order, from the servers an option counts; by default,
 * those they have, by their words. A filter without `values` is a pattern alone. */
type Facet = {
  key: FieldKey
  label: string
  values?: {
    of: (s: ServerView, n: Names) => string[]
    draw: (value: string, n: Names) => Drawing
    offered?: (servers: ServerView[], n: Names) => string[]
  }
}

const STATUSES = ["good", "bad", "none"] as const satisfies readonly ServerRating[]

const NAME: Facet = { key: "name", label: "Name" }
const STATUS: Facet = { key: "status", label: "Status", values: {
  of: (s) => [rate(s).rating],
  draw: (rating) => ({ words: RATING_WORDS[rating as Rating], mark: <RatingMark rating={rating as Rating} /> }),
  offered: () => [...STATUSES],
} }
const COUNTRY: Facet = { key: "country", label: "Country", values: {
  of: (s) => (s.country.code ? [s.country.code] : []),
  draw: (code) => countryDrawing(code || null),
} }
const SOURCE: Facet = { key: "source", label: "Source", values: {
  of: (s) => [s.source],
  draw: (id, n) => ({ words: n.view.sources.find((x) => x.id === id)?.name ?? "Removed" }),
  /* Every source, so a group's rule can name one that has no server now. */
  offered: (_, n) => n.view.sources.map((x) => x.id as string),
} }
const PROTOCOL: Facet = { key: "protocol", label: "Protocol", values: {
  of: (s) => [s.protocol],
  draw: (protocol) => ({ words: PROTOCOL_WORDS[protocol as Protocol] }),
} }
/** A box's two values, as the box column's filter offers them, each drawn as its box: ticked, or empty. */
const BOX_WORDS = { ticked: "Selected", unticked: "Not selected" } as const satisfies Record<NonNullable<BoxCondition>, string>
const boxValue = (ticked: boolean): NonNullable<BoxCondition> => (ticked ? "ticked" : "unticked")

const BOX: Facet = { key: "box", label: "Selection", values: {
  of: (s, n) => (n.box ? [boxValue(n.box.isTicked(s))] : []),
  draw: (value) => ({ words: BOX_WORDS[value as NonNullable<BoxCondition>], mark: <BoxMark ticked={value === "ticked"} /> }),
  offered: () => ["ticked", "unticked"],
} }

/** A filter's values to offer: what `offered` lists, then any value the filter holds that no server has now. Each counts the servers that have it. */
function optionsOf({ values }: Facet, chosen: string[], servers: ServerView[], n: Names): FacetOption[] {
  if (!values) return []
  const offered = values.offered?.(servers, n)
    ?? [...new Set(servers.flatMap((s) => values.of(s, n)))].toSorted((a, b) => values.draw(a, n).words.localeCompare(values.draw(b, n).words))
  return [...new Set([...offered, ...chosen])].map((value) => ({ value, drawing: values.draw(value, n), aside: servers.filter((s) => values.of(s, n).includes(value)).length }))
}

/** Whether a server passes a filter that hides. A name pattern passes what the daemon matched, `named`, and everything while it
 * hasn't said. */
function passes(field: FieldValue, s: ServerView, named: ReadonlySet<string> | null, n: Names): boolean {
  if (field.kind === "pattern") return named === null || named.has(s.id)
  const has = FACET_OF.get(field.key)?.values?.of(s, n) ?? []
  if (field.kind === "one") return field.value === null || has.includes(field.value)
  const { not, values } = field.value
  return values.length === 0 || has.some((v) => values.includes(v)) !== not
}

type ColumnKey = "box" | "status" | "country" | "name" | "source" | "protocol" | "ping" | "stability" | "inUse" | "test"

/** One column: whether a table has it, which filters its header opens, and the width below which it hides. No header sorts:
 * each tab has one order, `Order`. A hidden column's filters stay in the chip line's Filter button. A `compact` column's
 * header is `icon` alone, opening its filter, or nothing to see where it has none. `width` is the same on every tab, so a column
 * sits in one place whatever the rows say. A column hides below a width where Name keeps at least 128px. */
type Column = {
  key: ColumnKey
  label: string
  /** More of the header, where there's room for it. */
  detail?: string
  /** What the header's card says, opened by hover, keyboard focus and a tap. */
  about?: Line[]
  has?: (shape: Shape) => boolean
  filters?: Facet[]
  /** The class that hides it below its width. */
  hide?: string
  compact?: { icon?: LucideIcon }
  /** Set on the header. Name's is auto: it takes what the others leave, and truncates, so Ping and Stability stay in sight on a phone. */
  width: string
  head?: string
  cell?: string
  render: (s: ServerView, c: Ctx) => ReactNode
}

const sourceName = (view: View, s: ServerView) => view.sources.find((x) => x.id === s.source)?.name ?? "Removed"
/** Draws a value as `facet` does. Every cell of a filterable column goes through here. */
const draw = (facet: Facet, value: string, n: Names, wordsClassName?: string) =>
  facet.values && <Drawn facet={facet.key} drawing={facet.values.draw(value, n)} wordsClassName={wordsClassName} />

/** A server's box, ticked as the stock checkbox is while the server is ticked in the group. */
function BoxMark({ ticked }: { ticked: boolean }) {
  return (
    <span className={cn("flex size-4 shrink-0 items-center justify-center rounded-[4px] border", ticked ? "border-primary bg-primary text-primary-foreground" : "border-muted-foreground dark:bg-input/30")}>
      {ticked && <CheckIcon className="size-3.5" />}
    </span>
  )
}

/** The box: a server's tick in the group, saved at once. It says nothing of membership, which the rows' dimming does. A busy box
 * takes no click, and after a second stands at half strength, so nothing unconfirmed looks stored. Its tap area is 28px, past the
 * box's own 16px. */
function BoxCell({ server, box }: { server: ServerView; box: Box }) {
  const ticked = box.isTicked(server)
  const busy = box.busy(server)
  return (
    <button type="button" role="checkbox" aria-checked={ticked} aria-disabled={busy !== undefined || undefined} aria-busy={busy?.slow || undefined} aria-label={`Select ${server.label}`} data-control={box.control(server)} onClick={() => box.tick(server)}
      className={cn("relative flex rounded-[4px] outline-none after:absolute after:-inset-1.5 focus-visible:ring-2 focus-visible:ring-ring/50", busy?.slow && "opacity-50")}>
      <BoxMark ticked={ticked} />
    </button>
  )
}

/** The dot: a server's rating now, its word for a screen reader and on hover. A dormant one is greyed, and says how old its last result is. */
function StatusCell({ server, c }: { server: ServerView; c: Ctx }) {
  const { rating, why } = rate(server)
  return (
    <span role="img" aria-label={RATING_WORDS[rating]} title={[RATING_WORDS[rating], ...why].join(" · ")} className={cn("flex", !server.health.current && "opacity-50 grayscale")}>
      {draw(STATUS, rating, c, "sr-only")}
    </span>
  )
}

/** A name as its feed gives it: the home's remarks with "· tag" muted, as Protocol draws its transport, or under the home's heading
 * the tag alone. A name of one part, or none, is the label. */
function NameWords({ server, underHome }: { server: ServerView; underHome: boolean }) {
  const { name, number, label } = server
  const numbered = number === null ? "" : ` ${number}`
  if (underHome && name?.tag) return <>{name.tag}{numbered}</>
  if (name?.remarks && name.tag) return <>{name.remarks}<Quiet> · {name.tag}</Quiet>{numbered}</>
  return label
}

function NameCell({ server, c }: { server: ServerView; c: Ctx }) {
  const words = [STANDING_WORDS[serverStanding(server)], server.udp === "failed" ? "UDP failed" : null].filter((w) => w !== null)
  const underHome = c.underHome(server)
  const [wanted, setWanted] = useState(false)
  /* Reaching the name asks for the week anew, unless its card is open, so what an open card shows stays. */
  const [opening, setOpening] = useState<number | null>(null)
  const week = useServerWeek(server, opening)
  /* The card waits for the week, so it opens at its size, and a week with nothing in it opens none. */
  const ready = week.state !== "loading" && week.state !== "empty"
  const held: Held = { open: wanted && ready, onOpenChange: (on) => { if (on && !wanted) setOpening((n) => (n ?? 0) + 1); setWanted(on) } }
  return (
    <span className="flex min-w-0 items-center gap-2">
      {c.active?.has(server.id) && <CarryingMark label={`In use: ${carrying(server, c.view.slots, c.view, c.here).join(", ")}`} />}
      <Tip lines={[]} held={held} content={ready && <ServerStats server={server} week={week} card />}>
        {/* Its click opens the details, which stand in for the card. */}
        <button type="button" aria-label={underHome ? server.label : undefined} className="min-w-16 truncate text-left underline-offset-4 hover:underline" onClick={() => { setWanted(false); c.open(server) }}>
          <NameWords server={server} underHome={underHome} />
        </button>
      </Tip>
      {/* The words give way first, down to nothing, so the name keeps a readable width on a phone. */}
      {words.length > 0 && (
        <Quiet className="min-w-0 shrink-[100] truncate [&>*+*]:ml-2">
          {words.map((word) => <span key={word}>{word}</span>)}
        </Quiet>
      )}
    </span>
  )
}

/** Ping's p95, and its header's "p50 / p95", show from where its column widens. */
const PING_WIDE = "@max-[49.5rem]/main:hidden"

/** "25 / 35 ms", and the typical ping alone on a phone, against the column's right edge. A ping over a day old is muted, and its
 * card says how old. */
function PingCell({ health }: { health: ServerView["health"] }) {
  const age = pingAge(health)
  if (health.pingP50Ms === null || health.pingP95Ms === null) return <Quiet>—</Quiet>
  const words = <PingFigures p50={health.pingP50Ms} p95={health.pingP95Ms} p95ClassName={PING_WIDE} />
  if (!age) return <span title={pingPair(health)}>{words}</span>
  return <Fact lines={[ageLine(`Measured ${age}`, age)]} label={`${pingPair(health)}, measured ${age}`} className="text-muted-foreground -my-1.5 ml-auto w-fit justify-center"><span>{words}</span></Fact>
}


const COLUMNS: Column[] = [
  /* Like the dot's header, its button is pulled back over its own inset, so the icon's centre is the box's, 16px in. */
  { key: "box", label: "Selection", has: (t) => t.ofGroup, filters: [BOX], compact: { icon: SquareCheckIcon }, width: "w-7", head: "pl-2 pr-0 [&_button]:-ml-0.5",
    cell: "overflow-visible pl-2 pr-1", render: (s, c) => c.box && <BoxCell server={s} box={c.box} /> },
  /* The header's button is pulled back over its own inset, so its icon's centre is the dot's, 12px in. */
  { key: "status", label: "Status", filters: [STATUS], compact: { icon: ListFilterIcon }, width: "w-7", head: "pl-2 pr-0 [&_button]:-ml-1.5", cell: "pl-2 pr-1",
    render: (s, c) => <StatusCell server={s} c={c} /> },
  /* The flag alone: its country's name is for a screen reader and on hover, and the filter's options and chips add it. */
  { key: "country", label: "Country", filters: [COUNTRY], compact: { icon: FlagIcon }, width: "w-7", head: "px-1", cell: "px-1.5",
    render: (s, c) => draw(COUNTRY, s.country.code ?? "", c, "sr-only") },
  { key: "name", label: "Name", filters: [NAME], width: "w-auto", render: (s, c) => <NameCell server={s} c={c} /> },
  { key: "source", label: "Source", has: (t) => !t.ofSource, filters: [SOURCE], hide: "@max-[45.5rem]/main:hidden", width: "w-28",
    render: (s, c) => draw(SOURCE, s.source, c) },
  { key: "protocol", label: "Protocol", filters: [PROTOCOL], hide: "@max-[60.5rem]/main:hidden", width: "w-36",
    render: (s, c) => <>{draw(PROTOCOL, s.protocol, c)}<Quiet> · {TRANSPORT_WORDS[s.transport]}</Quiet></> },
  { key: "ping", label: "Ping", detail: "p50 / p95", width: "w-28 @max-[49.5rem]/main:w-16", head: "text-right", cell: "text-right tabular-nums",
    render: (s) => <PingCell health={s.health} /> },
  /* Narrow, Stability is 64px so Name keeps 142px at 360px. */
  { key: "stability", label: "Stability", about: STABILITY_HEADER, width: "w-24 @max-[51.5rem]/main:w-16", head: "text-right",
    render: (s, c) => <StabilityCell stability={stabilityOf(s.health, c.now)} /> },
  { key: "inUse", label: "In use", hide: "@max-[38.5rem]/main:hidden", width: "w-36",
    render: (s, c) => { const uses = carrying(s, c.view.slots, c.view, c.here); return uses.length ? uses.join(", ") : <Quiet>—</Quiet> } },
  { key: "test", label: "Test", hide: "@max-[29.5rem]/main:hidden", width: "w-24", head: "text-right", render: (s) => serverStanding(s) === "usable" && <ServerTest server={s} /> },
]
const FACET_OF = new Map(COLUMNS.flatMap((c) => c.filters ?? []).map((f) => [f.key, f]))

/** A section's heading, whose words run across the table. A column a narrow screen hides keeps a cell of its own, since spanning it
 * would add a column where it's hidden. So the words span the columns before the first that hides, and run on over the empty
 * cells after them. */
function HeadingRow({ columns, children }: { columns: Column[]; children: ReactNode }) {
  const hides = columns.findIndex((col) => col.hide)
  const span = hides === -1 ? columns.length : hides
  return (
    <TableRow className="bg-muted/40 hover:bg-muted/40">
      <TableHead scope="rowgroup" colSpan={span} className="h-8 overflow-visible">{children}</TableHead>
      {columns.slice(span).map((col) => <TableCell key={col.key} className={col.hide} />)}
    </TableRow>
  )
}

/** What a column's header holds: its filters, its card, or its words. `fields` is null for a header drawn before its tab is read,
 * which keeps its place and takes no click. */
function headerOf(col: Column, fields: FilterField[] | null): ReactNode {
  const filtered = fields === null ? (col.filters?.length ?? 0) > 0 : fields.length > 0
  if (col.compact) return <><span className="sr-only">{col.label}</span>{filtered && <FilterHeader label={col.label} fields={fields ?? []} icon={col.compact.icon} />}</>
  if (filtered) return <FilterHeader label={col.label} fields={fields ?? []} />
  if (col.about) return <Fact lines={col.about} label={col.label} side="left" className="ml-auto w-fit justify-center">{col.label}</Fact>
  return <>{col.label}{col.detail && <span className={PING_WIDE}> {col.detail}</span>}</>
}

/** The table's frame: its border and its header, the same whether its rows are on their way, failed to read, or here. A `line` in
 * place of the rows runs across the whole frame under the header: the failed read, or "No match" with its Clear at the end. */
function ServerTable({ columns, header, line, children }: { columns: Column[]; header: (col: Column) => ReactNode; line?: ReactNode; children?: ReactNode }) {
  return (
    <div className="overflow-hidden rounded-lg border">
      <Table aria-label="Servers" className="table-fixed">
        <TableHeader>
          <TableRow>
            {columns.map((col) => <TableHead key={col.key} className={cn(col.width, col.head, col.hide)}>{header(col)}</TableHead>)}
          </TableRow>
        </TableHeader>
        {children}
      </Table>
      {line && <div data-slot="table-line" className="flex min-h-10 items-center border-t px-2 py-1.5">{line}</div>}
    </div>
  )
}

/** Rows on their way: each keeps the places of the row it becomes, a faint dot, the flag, the name, a spinner where Ping's number
 * will stand and unlit bars. */
function PendingRows({ columns, count = 3 }: { columns: Column[]; count?: number }) {
  const place: Partial<Record<ColumnKey, ReactNode>> = {
    status: <span className="flex"><RatingMark rating="none" /></span>,
    country: <Skeleton className="size-3.5 rounded-full" />,
    name: <Skeleton className="h-2 w-3/5 rounded-full" />,
    source: <Skeleton className="h-2 w-12 rounded-full" />,
    protocol: <Skeleton className="h-2 w-16 rounded-full" />,
    ping: <span className="flex justify-end"><Spinner role={undefined} aria-label={undefined} aria-hidden className="text-muted-foreground size-3.5" /></span>,
    stability: <span className="flex justify-end"><StabilityBars bars={0} /></span>,
  }
  return (
    <TableBody role="rowgroup" aria-label="Loading servers…" aria-busy>
      {Array.from({ length: count }, (_, i) => (
        <TableRow key={i} data-slot="pending-row" className="hover:bg-transparent">
          {columns.map((col) => <TableCell key={col.key} className={cn("h-[2.3125rem]", col.cell, col.hide)}>{place[col.key]}</TableCell>)}
        </TableRow>
      ))}
    </TableBody>
  )
}

/** Each tab's one order. No header changes it, and nothing saves it. Rows that tie keep feed order. */
type Order =
  /** All servers: best first by score, a server with no score last. */
  | { kind: "best" }
  /** A group's tab: the members its rule held when it was saved, in the daemon's order, then the rows it leaves out. So neither an
   * edit nor a tick moves a row until the rule is saved. */
  | { kind: "group"; order: ServerView["id"][]; saved: (s: ServerView) => boolean }
  /** A source's tab: feed order, under its config headings where it has them. */
  | { kind: "feed" }
const BEST: Order = { kind: "best" }
const FEED: Order = { kind: "feed" }

/** Infinity, a value a row lacks, ties with itself and goes after every number. */
const ascending = (x: number, y: number) => (x === y ? 0 : x - y)

function arranged(rows: ServerView[], order: Order, view: View): ServerView[] {
  const feed = feedOrder(view)
  if (order.kind === "feed") return rows.toSorted(feed)
  const at = new Map(order.kind === "group" ? order.order.map((id, i) => [id, i]) : [])
  const key = (s: ServerView): [number, number] => order.kind === "best" ? [0, s.health.score ?? Infinity]
    : order.saved(s) ? [0, at.get(s.id) ?? Infinity] : [1, 0]
  return rows.toSorted((a, b) => {
    const [x, y] = [key(a), key(b)]
    return ascending(x[0], y[0]) || ascending(x[1], y[1]) || feed(a, b)
  })
}

/** Search and the table over some servers, in the tab's `order`, with the filters `filters` holds. The search is a view, never
 * saved. `box` gives a group's tab its box column: the servers not in the group show muted, all but their boxes. `source` is the source whose servers these are: when some config is home to more than
 * one, its rows group under their home configs. */
function ServerBrowser({ pool, list, box, view, order, source, here, filters, skipped }: {
  pool: ServerView[]
  /** The server list's read: until it lands the rows are Pending, and a read that failed stands in their place. */
  list: Read<unknown>
  box?: Box
  view: View
  order: Order
  source?: SourceView
  /** The source or group whose tab this is. The rows of the servers slots use through it are tinted, and their names marked. */
  here?: Exitable
  filters?: Filters
  /** The source's refused entries: each config's beside its heading, and the rest, a config's with no heading among them, beside
   * Search. */
  skipped?: ReturnType<typeof skippedOf> & { fix: SkippedFix | null }
}) {
  /* Beside a chip line, search starts as a button, so the line has room for a chip at every width. */
  const compactSearch = filters !== undefined
  const [search, setSearch] = useState("")
  const [searching, setSearching] = useState(false)
  const [serverOpen, setServerOpen] = useState<ServerView | null>(null)
  const names: Names = { view, box }
  const counted = pool
  const ordered = arranged(pool, order, view)
  const allFields: FilterField[] = (filters?.fields ?? []).map((field: FieldValue) => {
    const facet = FACET_OF.get(field.key) ?? { key: field.key, label: field.key }
    if (field.kind === "set") return { ...field, label: facet.label, options: optionsOf(facet, field.value.values, counted, names) }
    if (field.kind === "one") return { ...field, label: facet.label, options: optionsOf(facet, field.value === null ? [] : [field.value], counted, names) }
    return { ...field, label: facet.label }
  })
  const shape: Shape = { ofSource: source !== undefined, ofGroup: box !== undefined }
  const columns = COLUMNS.filter((col) => col.has?.(shape) ?? true)
  const fieldsOf = (col: Column) => allFields.filter((f) => col.filters?.some((facet) => facet.key === f.key))
  /* Every filter has its column on every tab. The chip line's Filter button holds them all, in the columns' order, so one a narrow
   * screen hides stays in reach. */
  const lineFields = columns.flatMap(fieldsOf)

  const words = search.trim().toLowerCase().split(/\s+/).filter(Boolean)
  /* Everything a row says, so anything in sight can find it. Each word narrows further. Only words: a flag is no text to match. */
  const searchText = (s: ServerView) => [
    s.label, countryName(s.country.code), s.country.code, PROTOCOL_WORDS[s.protocol], s.protocol, TRANSPORT_WORDS[s.transport], RATING_WORDS[rate(s).rating],
    STANDING_WORDS[serverStanding(s)],
    view.sources.find((x) => x.id === s.source)?.name, ...s.seen.flatMap((seen) => [seen.config, seen.tag]), ...carrying(s, view.slots, view, here),
  ].filter((x): x is string => typeof x === "string").join(" ").toLowerCase()
  /* A rule's filters only dim what they leave out, so its rows stay in sight to be picked. */
  const hiding = filters?.kind === "view" ? filters.fields : []
  const named = filters?.kind === "view" ? filters.named : null
  const shown = ordered
    .filter((s) => hiding.every((field) => passes(field, s, named, names)))
    .filter((s) => { const text = searchText(s); return words.every((word) => text.includes(word)) })

  const now = useNow()
  const grouped = source !== undefined && groupsByHome(pool)
  const active = here && inUseThrough(here, view.slots)
  const c: Ctx = { ...names, here, active, now, underHome: (s) => grouped && s.home !== null, open: setServerOpen }
  const sections: Section[] | null = grouped && shown.length > 0 ? sectionsOf(shown, source) : null
  const headed = new Set(grouped ? sectionsOf(pool, source).flatMap((s) => (s.heading.kind === "config" ? [s.heading.key as string] : [])) : [])
  const unheaded = skipped ? [...skipped.loose, ...[...skipped.byConfig].flatMap(([key, entries]) => (headed.has(key) ? [] : entries))] : []
  const row = (server: ServerView) => (
    <TableRow key={server.id} className={cn(active?.has(server.id) && "bg-success/15 hover:bg-success/20")}>
      {columns.map((col) => <TableCell key={col.key} className={cn("truncate", col.cell, col.hide, col.key !== "box" && box?.dimmed(server) && OUTSIDE)}>{col.render(server, c)}</TableCell>)}
    </TableRow>
  )
  return (
    <>
      <Toolbar>
        {compactSearch && !searching && search === "" ? <Button variant="outline" size="icon" aria-label="Search" title="Search" onClick={() => setSearching(true)}><SearchIcon /></Button> : (
          <InputGroup className={cn(compactSearch ? "w-40" : "w-56", "max-sm:w-32")}>
            <InputGroupAddon><SearchIcon /></InputGroupAddon>
            <InputGroupInput placeholder="Search" autoFocus={compactSearch} value={search} onChange={(e) => setSearch(e.target.value)} onBlur={() => setSearching(false)}
              onKeyDown={(e) => { if (e.key === "Escape" && compactSearch) { setSearch(""); e.currentTarget.blur() } }} />
            {search !== "" && (
              <InputGroupAddon align="inline-end">
                <InputGroupButton size="icon-xs" aria-label="Clear search" onMouseDown={(e) => e.preventDefault()} onClick={() => setSearch("")}><XIcon /></InputGroupButton>
              </InputGroupAddon>
            )}
          </InputGroup>
        )}
        {unheaded.length > 0 && <SkippedMark entries={unheaded} withConfig fix={skipped?.fix} />}
        {filters && <FilterLine fields={lineFields} end={filters.end} />}
      </Toolbar>
      <ServerTable columns={columns} header={(col) => headerOf(col, fieldsOf(col))}
        line={list.state === "failed" ? <ReadFailed place="row" {...list} />
          : list.state === "read" && shown.length === 0 ? <NoMatch onClear={search === "" ? undefined : () => setSearch("")} /> : undefined}>
          {list.state === "waiting" ? <PendingRows columns={columns} />
          : list.state === "failed" || shown.length === 0 ? null
          : sections ? sections.map((section) => (
            <TableBody key={section.heading.kind === "config" ? section.heading.key : "nameOnly"} className="border-t first:border-t-0">
              <HeadingRow columns={columns}>
                {section.heading.kind === "config" ? (
                  <span className="flex items-center gap-1">
                    {section.heading.name || "Unnamed"}
                    {skipped?.byConfig.has(section.heading.key) && <SkippedMark entries={skipped.byConfig.get(section.heading.key) ?? []} fix={skipped.fix} />}
                  </span>
                ) : "Other servers"}
              </HeadingRow>
              {section.rows.map(row)}
            </TableBody>
          )) : (
            <TableBody>{shown.map(row)}</TableBody>
          )}
      </ServerTable>
      {/* Another server's details are an opening of their own, from their own read. */}
      {serverOpen && <ServerDetails key={serverOpen.id} server={serverOpen} source={sourceName(view, serverOpen)} onClose={() => setServerOpen(null)} />}
    </>
  )
}

type Tab = "all" | "source" | "group"
/** Which tab an address is, known before the view says what it holds. */
const tabOf = (pathname: string): Tab => (pathname.startsWith("/sources/") ? "source" : pathname.startsWith("/groups/") ? "group" : "all")
const columnsOf = (tab: Tab) => COLUMNS.filter((col) => col.has?.({ ofSource: tab === "source", ofGroup: tab === "group" }) ?? true)

/** A tab while the daemon's view is on its way: on a source's or group's tab its heading's faint dot and name's place, then the
 * toolbar and the table's frame drawn, its rows Pending. */
function ServersWaiting({ tab }: { tab: Tab }) {
  const columns = columnsOf(tab)
  const filtered = tab !== "source"
  const frame = (
    <>
      <Toolbar>
        {filtered ? <Button variant="outline" size="icon" aria-label="Search" aria-disabled tabIndex={-1}><SearchIcon /></Button> : (
          <InputGroup className="w-56 max-sm:w-32" data-disabled>
            <InputGroupAddon><SearchIcon /></InputGroupAddon>
            <InputGroupInput placeholder="Search" disabled />
          </InputGroup>
        )}
        {filtered && <FilterLine fields={[]} />}
      </Toolbar>
      <ServerTable columns={columns} header={(col) => headerOf(col, filtered ? null : [])}><PendingRows columns={columns} /></ServerTable>
    </>
  )
  if (tab === "all") return frame
  return (
    <div className="flex flex-1 flex-col gap-4">
      <span className="flex h-9 items-center gap-2"><RatingMark rating="none" /><Skeleton className="h-2.5 w-32 rounded-full" /></span>
      <div className="flex flex-col gap-3 border-t pt-4">{frame}</div>
    </div>
  )
}

/** A source that lists no servers, in its table's place. Its first refresh running is waiting: the table's rows Pending. A refresh
 * that failed is the failure's own rows, its User-Agents and Retry, as Refresh's card holds them. One that worked and listed none
 * is the empty block with what fills it: Refresh, or pasting the links again. */
function NoServers({ refresh: r, pasteAgain }: { refresh: Refreshing | null; pasteAgain: () => void }) {
  if (r?.run.kind === "first") {
    const columns = columnsOf("source")
    return <ServerTable columns={columns} header={(col) => headerOf(col, [])}><PendingRows columns={columns} count={2} /></ServerTable>
  }
  if (r?.refresh.failing) {
    return (
      <EmptyState>
        <span className="flex w-64 max-w-full flex-col items-center gap-3 [&>div:last-child]:w-full [&>div:last-child]:justify-center">
          <RefreshFailure r={r} way="block" />
        </span>
      </EmptyState>
    )
  }
  const block = r?.run.kind === "running" && r.run.by === "block"
  return (
    <EmptyState icon={<ServerIcon />} title="No servers">
      {r ? <Button variant="outline" disabled={r.run.kind !== "idle"} aria-busy={block || undefined} onClick={() => r.go("block")}>{block ? <Spinner role={undefined} aria-label={undefined} aria-hidden /> : <RefreshCwIcon />}Refresh</Button>
        : <Button variant="outline" onClick={pasteAgain}><ClipboardPasteIcon />Paste again</Button>}
    </EmptyState>
  )
}

/** What fixes a source's skipped entries: the provider's support, when it sends one, or pasting the links again. */
const skippedFix = ({ origin }: SourceView, pasteAgain: () => void): SkippedFix | null =>
  origin.kind === "manual" ? { kind: "paste", open: pasteAgain } : origin.provider.supportUrl ? { kind: "support", href: origin.provider.supportUrl } : null

/** A source's tab: its heading, then its servers, or what stands in their place. Its heading and that place share one refresh. A
 * source that lists none keeps the mark of the entries it skipped above that place, since they say why. Its Delete's dialog is
 * the tab's own, so it goes with the tab. */
function SourceTab({ here, all, list, index, view }: { here: Extract<Exitable, { kind: "source" }>; all: ServerView[]; list: Read<ServerView[]>; index: Map<ServerView["id"], ServerView>; view: View }) {
  const [deleting, setDeleting] = useState(false)
  const r = useRefresh(here.source)
  const read = list.state === "read"
  const servers = all.filter((s) => s.source === here.source.id)
  const { loose, byConfig } = skippedOf(here.source)
  const skipped = [...loose, ...[...byConfig.values()].flat()]
  return (
    <>
    <SourceDetails sub={here.source}
      heading={(name, openSettings, held) => <SourceHeading source={here.source} servers={read ? here.members : null} refresh={r} name={name} openSettings={openSettings} onDelete={() => setDeleting(true)} deleteWaits={held} />}
      below={(_, pasteAgain) => (
        <div className="flex flex-col gap-3 border-t pt-4">
          {read && servers.length === 0 ? (
            <>
              {skipped.length > 0 && <Toolbar><SkippedMark entries={skipped} withConfig fix={skippedFix(here.source, pasteAgain)} /></Toolbar>}
              <NoServers refresh={r} pasteAgain={pasteAgain} />
            </>
          )
            : <SourceServers here={here} servers={servers} list={list} view={view} pasteAgain={pasteAgain} />}
        </div>
      )} />
    {deleting && <DeleteDialog doomed={{ kind: "source", source: here.source }} view={view} servers={index} list={list} onClose={() => setDeleting(false)} />}
    </>
  )
}

/** A group's servers under its heading, every one its sources list, each with its box, under the rule's filters. */
function GroupServers({ shown, all, list, view, here }: { shown: GroupShown; all: ServerView[]; list: Read<unknown>; view: View; here?: Exitable }) {
  return (
    <div className="flex flex-col gap-3 border-t pt-4">
      <ServerBrowser pool={all} list={list} view={view} order={{ kind: "group", order: shown.placed.order, saved: (s) => shown.placed.members.has(s.id) }} here={here}
        box={{ dimmed: (s) => shown.dimmed(s.id), isTicked: (s) => shown.isTicked(s.id), busy: (s) => shown.busy(s.id), tick: shown.tick, control: (s) => shown.control(s.id) }} filters={{ kind: "rule", fields: shown.fields, end: shown.end }} />
    </div>
  )
}

/** A group's tab: its heading and its servers. Its Delete's dialog is the tab's own, so it goes with the tab. */
function GroupTab({ here, all, list, index, view, naming, onNamed }: {
  here: Extract<Exitable, { kind: "group" }>; all: ServerView[]; list: Read<ServerView[]>; index: Map<ServerView["id"], ServerView>; view: View; naming: boolean; onNamed: () => void
}) {
  const [deleting, setDeleting] = useState(false)
  return (
    <>
      <GroupDetails group={here.group} view={view} servers={list.state === "read" ? list.data : null} naming={naming} onNamed={onNamed} heading={(name, held) => <GroupHeader group={here.group} name={name} servers={index} onDelete={() => setDeleting(true)} deleteWaits={held} />}
        below={(shown) => <GroupServers shown={shown} all={all} list={list} view={view} here={here} />} />
      {deleting && <DeleteDialog doomed={{ kind: "group", group: here.group }} view={view} servers={index} list={list} onClose={() => setDeleting(false)} />}
    </>
  )
}

/** A new group's tab before its Save: its name, then every server with its box, as a group's tab has them. */
function NewGroupTab({ all, list, view }: { all: ServerView[]; list: Read<ServerView[]>; view: View }) {
  return (
    <NewGroupDetails view={view} servers={list.state === "read" ? list.data : []} heading={(name) => <div data-slot="group-heading" className="flex min-h-7 min-w-0 flex-wrap items-center gap-x-2">{name}</div>}
      below={(shown) => <GroupServers shown={shown} all={all} list={list} view={view} />} />
  )
}

/** The servers of whatever the list has open: everything, one source, or one group. Each server shows once in any list. */
export function ServersPage() {
  const { pathname, state } = useLocation()
  const navigate = useNavigate()
  const add = useAdd()
  const view = useView()
  const { all, rows, index, list } = useExitables()
  const selected = rows.find((r) => r.to === pathname)
  /* A tab whose item has left the view, deleted from here or elsewhere, leaves for All servers. The last render says what the
   * tab showed, so an address that never had an item is Not found. */
  const showed = useRef<string | null>(null)
  useEffect(() => {
    const was = showed.current
    showed.current = selected ? pathname : null
    if (!selected && was === pathname && view.data) navigate("/servers")
  })
  /* A group made from the list arrives with its name ready to type. */
  const naming = (state as { naming?: boolean } | null)?.naming === true

  return (
    <div className="flex flex-1 flex-col gap-3">
      <Loaded query={stateRead} skeleton={<ServersWaiting tab={tabOf(pathname)} />}>
        {(data) => {
          if (pathname === NEW_GROUP) return <NewGroupTab all={all} list={list} view={data} />
          if (pathname !== "/servers" && !selected) return <NotFound />
          if (!selected) {
            if (rows.length === 0) return <EmptyState icon={<RssIcon />} title="No sources"><Button onClick={add}><PlusIcon />Add source</Button></EmptyState>
            if (list.state === "read" && all.length === 0) return <EmptyState icon={<ServerIcon />} title="No servers" />
            return <AllServers servers={all} list={list} view={data} />
          }
          if (selected.kind === "source") return <SourceTab key={selected.id} here={selected} all={all} list={list} index={index} view={data} />
          return <GroupTab key={selected.id} here={selected} all={all} list={list} index={index} view={data} naming={naming} onNamed={() => navigate(pathname, { replace: true, state: null })} />
        }}
      </Loaded>
    </div>
  )
}
