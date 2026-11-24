import { useRef, useState, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import { ClipboardPasteIcon, ArrowDownIcon, ArrowUpIcon, ArrowUpRightIcon, CalendarPlusIcon, HistoryIcon, EllipsisIcon, LifeBuoyIcon, ListXIcon, RefreshCwIcon, RotateCcwIcon, Settings2Icon, Trash2Icon, UserIcon, type LucideIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { ChartContainer, type ChartConfig } from "@/components/ui/chart"
import { ActionCard, Fact, Rows, Tip, type Line } from "@/components/card"
import { FailedMark, failureOf, useRead } from "@/components/page"
import { DayLetter, FailoverMark, Readout, UseMark, usePlace, type ChartKey } from "@/components/server-stats"
import { ageLine, Quiet, RatingMark, Sign, Signed, TONE_TEXT, Toned } from "@/components/status"
import { actions, type RefreshWay } from "@/lib/api"
import { useNow } from "@/lib/clock"
import { ago, day, dayLetter, DEFAULT_REFRESH, failureWords, hoursWords, inUseWords, moment, refreshWords, size, sizeOf, spanOf, spanSpoken, spanWords, unitOf, type Span } from "@/lib/format"
import { skippedWords, type SkippedEntry } from "@/lib/feed"
import { RATING_WORDS } from "@/lib/health"
import { analyticsRead, useRefreshes } from "@/lib/queries"
import type { Failure, ServerView, SourceView } from "@/lib/schema"
import { isPending, useControls } from "@/lib/use-write"
import type { Shown } from "@/lib/write"
import { USER_AGENTS } from "@/lib/user-agents"
import { fetchedOf, lastWorked, leftAmount, standingOf, trafficTone, troubleOf, weekLayout, weekOf, weekRead, weekSpoken, workedMark, type Dot, type Expiry, type Refresh, type Tone, type Traffic, type Trouble, type Week, type WorkedMark } from "@/lib/troubles"
import { cn } from "@/lib/utils"

type Remote = Extract<SourceView["origin"], { kind: "remote" }>

/** The refusals a provider gives a User-Agent it doesn't answer to. */
const AGENT_REFUSALS: readonly Failure[] = ["http_401", "http_403", "http_404"]

/** The dot: the worst trouble. Every reason stands beside it in the heading, so it opens no card and takes no focus. While the first
 * refresh runs it is the faint Pending dot. */
function SourceDot({ dot }: { dot: Dot }) {
  return (
    <span role="img" aria-label={dot.kind === "first" ? "Refreshing…" : RATING_WORDS[dot.rating]} className="flex h-5 shrink-0 items-center">
      <RatingMark rating={dot.rating} className={cn(dot.kind === "first" && "motion-safe:animate-pulse")} />
    </span>
  )
}

/** Every fact stands on the heading's one centre line, 28px tall. */
const ON_LINE = "flex h-7 flex-row items-center gap-2 whitespace-nowrap"

/** Normal, a muted "Expires in" and its time plain. A trouble is its words, all of it in its colour. */
function ExpiryFact({ expiry, trouble }: { expiry: Extract<Expiry, { kind: "expires" | "expired" }>; trouble: Trouble | null }) {
  return (
    <Fact lines={[{ words: moment(expiry.at) }]} className={ON_LINE}>
      {trouble ? <Toned tone={trouble.tone}>{trouble.words}</Toned> : <span><Quiet>Expires in</Quiet> {expiry.kind === "expires" && spanWords(expiry.in)}</span>}
    </Fact>
  )
}

/** The share left of the limit, 64px wide, or 96px in the traffic's card. Under a tenth it is amber, and with nothing left it is
 * empty with a red mark where it starts. */
function TrafficBar({ share, tone, long = false }: { share: number; tone: Tone | null; long?: boolean }) {
  return (
    <span aria-hidden className={cn("bg-foreground/10 relative h-1 shrink-0 overflow-hidden rounded-full", long ? "w-24" : "w-16", tone === "bad" && "shadow-[inset_2px_0_0_var(--destructive)]")}>
      <span className={cn("block h-full", tone === "warn" ? "bg-warning" : "bg-muted-foreground")} style={{ width: `${share * 100}%` }} />
    </span>
  )
}

/** What is left's ink: a trouble's colour, or muted for a reading over a day old with none. */
const leftInk = (t: Extract<Traffic, { kind: "left" }>, trouble: Trouble | null) => cn(trouble ? TONE_TEXT[trouble.tone] : t.old && "text-muted-foreground")

/** The traffic's card: a long bar with "100 / 100 GB", then sent, received and the refill's date on one line, and the reading's age
 * once it is over a day old. The figure takes the heading's ink. */
function trafficLines(t: Extract<Traffic, { kind: "left" }>, trouble: Trouble | null): Line[] {
  const refillDay = t.refill && t.refills ? day(t.refills) : null
  const moved = [
    t.upload !== null && { icon: ArrowUpIcon, words: `sent ${size(t.upload)}`, shown: size(t.upload) },
    t.download !== null && { icon: ArrowDownIcon, words: `received ${size(t.download)}`, shown: size(t.download) },
    refillDay !== null && { icon: CalendarPlusIcon, words: `resets ${refillDay}`, shown: refillDay },
  ].filter((m) => m !== false)
  return [
    { words: `${sizeOf(t.left, t.limit)} left`, drawn: <span className="flex items-center gap-2"><TrafficBar share={t.share} tone={trafficTone(t)} long /><span className={leftInk(t, trouble)}>{sizeOf(t.left, t.limit, "/")}</span></span> },
    ...(moved.length > 0 ? [{
      words: moved.map((m) => m.words).join(", "),
      drawn: <span className="flex items-center gap-3">{moved.map(({ icon, shown, words }) => <span key={words} className="flex items-center gap-1"><Sign icon={icon} />{shown}</span>)}</span>,
    }] : []),
    ...(t.old ? [ageLine(`Read ${spanWords(t.old)} ago`, `${spanWords(t.old)} ago`)] : []),
  ]
}

/** The bar, then what is left, then the refill's sign and time. The bar says "left", so the words don't. A reading over a day old
 * mutes its value alone, and a trouble keeps its colour. A reading from before a refill date that has passed says nothing of what
 * is left. */
function TrafficFact({ traffic, trouble }: { traffic: Extract<Traffic, { kind: "left" | "unknown" }>; trouble: Trouble | null }) {
  if (traffic.kind === "unknown") {
    const due = day(traffic.due)
    return (
      <Fact label={`Traffic unknown: reset was due ${new Date(traffic.due).toLocaleDateString("en-US", { month: "long", day: "numeric" })}`}
        lines={[{
          words: `Reset was due ${due}, read ${spanWords(traffic.read)} ago`,
          drawn: <span className="flex items-center gap-3"><Signed icon={CalendarPlusIcon}><Quiet>{due}</Quiet></Signed><Signed icon={HistoryIcon}>{spanWords(traffic.read)} ago</Signed></span>,
        }]}
        className={ON_LINE}>
        <Quiet>— {unitOf(traffic.limit)}</Quiet>
      </Fact>
    )
  }
  return (
    <Fact lines={trafficLines(traffic, trouble)} className={ON_LINE}>
      <TrafficBar share={traffic.share} tone={trafficTone(traffic)} />
      <span className={leftInk(traffic, trouble)}>{leftAmount(traffic)}</span>
      <span className="sr-only">left</span>
      {traffic.refill && <span className="flex items-center gap-1"><Sign icon={CalendarPlusIcon} /><span className="sr-only">resets in </span><span>{spanWords(traffic.refill)}</span></span>}
    </Fact>
  )
}

/** When each week read was answered. While the read the next place asks for is out, the last answer stands, with its own time. */
const answered = new WeakMap<object, number>()

/** The read behind every source's week, and its answer: the buckets and the moment the read was answered, which is the week's now,
 * so a read the clock has passed never draws an idle tail it didn't measure. */
export function useWeekRead() {
  const now = useNow()
  const asked = analyticsRead(weekRead(now))
  const read = useQuery(asked)
  if (read.data && !read.isPlaceholderData) answered.set(read.data, read.dataUpdatedAt)
  const at = read.data && answered.get(read.data)
  return { asked, answer: read.data && at ? { buckets: read.data.buckets, at } : null }
}

/** The heading's strip: 100px, its seven days 2px apart. */
const STRIP = "w-[100px]"
const STRIP_WIDTH = 100
const STRIP_GAP = 2
/** The card's line: as wide as a server's chart, its days 4px apart, its band 24px tall. */
const CARD_WIDTH = 288
const CARD_GAP = 4
const BAND = 24

/** The week as a line of time, each day's hairline in the muted ink, today ending at the read, a 2px run over each stretch in use,
 * and a dashed amber line at the middle of each six hours that had a failover. `height` is its band, the line at its middle. */
function WeekLine({ week, width, gap, height, tick }: { week: Week; width: number; gap: number; height: number; tick: { width: number; dash: string } }) {
  const layout = weekLayout(week, width, gap)
  const mid = height / 2
  /* A line an odd number of pixels wide stands on a pixel's middle, so it fills whole pixels. */
  const across = tick.width % 2 === 1 ? 0.5 : 0
  return (
    <g shapeRendering="crispEdges">
      {layout.days.map((d, i) => d.x1 > d.x0 && <rect key={i} x={d.x0} y={mid - 0.5} width={d.x1 - d.x0} height={1} className="fill-muted-foreground/45" />)}
      {layout.runs.map((r, i) => <rect key={i} data-slot="week-run" x={r.x0} y={mid - 1} width={r.x1 - r.x0} height={2} rx={Math.min(1, (r.x1 - r.x0) / 2)} className="fill-muted-foreground" />)}
      {layout.failovers.map((x, i) => <line key={i} data-slot="week-failover" x1={x + across} x2={x + across} y1={0} y2={height} strokeWidth={tick.width} strokeDasharray={tick.dash} className="stroke-warning" />)}
    </g>
  )
}

const CHART: ChartConfig = {}
/** The week card's width, a server's card's. */
const WEEK_CARD = "w-[min(18rem,calc(100vw-4rem))]"

/** The week's card line: the strip's line larger, each day's two letters centred under it, today's bold, and the band on the place
 * the readout reads, through the letters. */
function WeekChart({ week, layout, at, plot }: { week: Week; layout: ReturnType<typeof weekLayout>; at: { x0: number; x1: number } | null; plot: ReturnType<typeof usePlace>["plotProps"] }) {
  return (
    <ChartContainer config={CHART} className="aspect-auto h-11 w-full">
      {/* The chart's frame holds what it draws in a box of no size, so the line takes the card's width itself. */}
      <div className={cn("relative h-11", WEEK_CARD)}>
        <svg aria-hidden viewBox={`0 0 ${CARD_WIDTH} 44`} className="block h-11 w-full touch-pan-y overflow-visible" {...plot}>
          {at && <rect data-slot="week-cursor" x={at.x0} y={0} width={Math.max(1, at.x1 - at.x0)} height={44} className="fill-border" />}
          <WeekLine week={week} width={CARD_WIDTH} gap={CARD_GAP} height={BAND} tick={{ width: 1, dash: "3 3" }} />
          {week.days.map((d, i) => <DayLetter key={d.start} x={layout.letters[i]} y={BAND + 6} letter={dayLetter(d.start)} today={i === week.days.length - 1} />)}
        </svg>
      </div>
    </ChartContainer>
  )
}

/** The week's fact and its card: the readout, "7d" over Failovers and In use, each led by the mark it counts where one is drawn,
 * then the same line larger with its days' letters. The pointer, a tap or, while the card is open, the arrow keys on the strip
 * pick six hours that have come, and the readout reads them; closing the card returns it to the week. */
function WeekCard({ week, children }: { week: Week; children: ReactNode }) {
  const layout = weekLayout(week, CARD_WIDTH, CARD_GAP)
  const { place, setPlace, plotProps, onKeyDown } = usePlace(layout.places.length, (e) => {
    const box = e.currentTarget.getBoundingClientRect()
    const x = (e.clientX - box.left) / (box.width / CARD_WIDTH)
    /* A place holds its left edge and not its right, which is the next one's. */
    const away = (p: { x0: number; x1: number }) => (x < p.x0 ? p.x0 - x : x >= p.x1 ? x - p.x1 + 1 : 0)
    return layout.places.reduce((best, p, i) => (away(p) < away(layout.places[best]) ? i : best), 0)
  })
  const [open, setOpen] = useState(false)
  const at = place === null ? null : layout.places[place] ?? null
  const keys: ChartKey[] = [
    { name: "Failovers", mark: week.failovers > 0 && <FailoverMark />, value: at ? at.place.failovers : week.failovers, fit: "00" },
    { name: "In use", mark: week.runs.length > 0 && <UseMark />, value: inUseWords(at ? at.place.inUse : week.inUse), fit: "00h" },
  ]
  const lines: Line[] = [{
    words: `Failovers ${week.failovers}, In use ${inUseWords(week.inUse)}`,
    drawn: (
      <span className={cn("flex flex-col gap-2", WEEK_CARD)}>
        <Readout window={at ? hoursWords(at.place.start, at.place.end) : "7d"} keys={keys} />
        <WeekChart week={week} layout={layout} at={at} plot={plotProps} />
      </span>
    ),
  }]
  const held = { open, onOpenChange: (on: boolean) => { setOpen(on); if (!on) setPlace(null) } }
  return <Fact label={weekSpoken(week)} lines={lines} held={held} onKeyDown={open ? onKeyDown : undefined} className={ON_LINE}>{children}</Fact>
}

/** "7d" muted, the week's 100px strip, then from 7 failovers their count in the trouble's colour. Its card is the week's readout
 * over the same line larger, for a week never used too. Until the read lands, "7d" stands with a skeleton the strip's width and a
 * spinner where the count goes, so nothing moves when it lands. A read that failed leaves the muted mark in the strip's place,
 * whose card holds the reason and Retry. */
function WeekFact({ week, asked, trouble }: { week: Week | null; asked: ReturnType<typeof analyticsRead>; trouble: Trouble | null }) {
  const failed = failureOf(useRead(asked))
  if (!week) {
    if (failed) return <FailedMark failed={failed} what="The week" className={ON_LINE} markClassName={cn(STRIP, "justify-center")}><span>7d</span></FailedMark>
    return (
      <span role="status" aria-label="Loading history…" className={ON_LINE}>
        <Quiet>7d</Quiet>
        <Skeleton className={cn("h-2 rounded-full", STRIP)} />
        <Spinner role={undefined} aria-label={undefined} aria-hidden className="text-muted-foreground size-3.5" />
      </span>
    )
  }
  const strip = (
    <>
      <Quiet>7d</Quiet>
      <svg data-slot="week-strip" aria-hidden width={STRIP_WIDTH} height={10} viewBox={`0 0 ${STRIP_WIDTH} 10`} className="shrink-0 overflow-visible">
        <WeekLine week={week} width={STRIP_WIDTH} gap={STRIP_GAP} height={10} tick={{ width: 2, dash: "4 2" }} />
      </svg>
      {trouble && <Toned tone={trouble.tone}>{week.failovers}</Toned>}
    </>
  )
  return <WeekCard week={week}>{strip}</WeekCard>
}

/** The failure's card: the reason, then rows of when a refresh last worked and the next try, or Refresh Off while paused, and
 * Retry. A refusal a User-Agent can answer puts the agents beside Retry, showing the stored one. A pick is this card's own, so it
 * goes when the card goes, however it goes, and when Retry is pressed. While Retry's write is out the agents show the one sent,
 * and both are Pending and take no click. */
function FailureCard({ remote, refresh, worked, nextIn, pending, spinning, sent, retry }: {
  remote: Remote; refresh: Extract<Refresh, { failing: true }>; worked: WorkedMark; nextIn: Span
  pending: boolean; spinning: boolean; sent: string | undefined; retry: (userAgent: string | undefined) => void
}) {
  const [draft, setDraft] = useState<string | null>(null)
  const agent = (pending ? sent : draft) ?? remote.userAgent
  const agents = AGENT_REFUSALS.includes(refresh.code ?? "timeout") ? [...new Set([remote.userAgent, ...USER_AGENTS, agent])] : null
  const last = <><span aria-hidden>{worked.shown}</span><span className="sr-only">{worked.heard}</span></>
  const rows: Line[] = [
    { name: "Last worked", words: worked.heard, drawn: worked.shown === "—" ? <Quiet>{last}</Quiet> : last },
    remote.paused ? { name: "Refresh", words: "Off" } : { name: "Next", words: `in ${spanWords(nextIn)}` },
  ]
  return (
    <>
      <Quiet>{refresh.code ? failureWords(refresh.code) : "Failed"}</Quiet>
      <Rows lines={rows} />
      <div className="flex items-center gap-2">
        {agents && (
          <Select value={agent} onValueChange={setDraft} disabled={pending}>
            <SelectTrigger size="sm" aria-label="User-Agent" className="min-w-0 flex-1 font-mono"><SelectValue /></SelectTrigger>
            <SelectContent>{agents.map((ua) => <SelectItem key={ua} value={ua} className="font-mono">{ua}</SelectItem>)}</SelectContent>
          </Select>
        )}
        <Button size="sm" variant="outline" className="shrink-0" disabled={pending} aria-busy={spinning || undefined} onClick={() => { setDraft(null); retry(agents ? agent : undefined) }}>
          {spinning ? <Spinner role={undefined} aria-label={undefined} aria-hidden /> : <RotateCcwIcon />}Retry
        </Button>
      </div>
    </>
  )
}

/** What a source's refresh is doing, as every way into it draws it. `first`: its first refresh runs, so nothing has worked or
 * failed yet. `running`: a press from this page, while its write is pending, and once it has landed until the daemon has checked
 * since; `by` is the way that pressed it. */
export type RefreshRun = { readonly kind: "first" } | { readonly kind: "idle" } | { readonly kind: "running"; readonly by: RefreshWay; readonly pending: boolean }

/** This page's press of a refresh. `out` from the press while its write is pending, with the control's landings then; `ran` once
 * it has landed, until the daemon's check moves off `at`. A write that ends without a landing, refused or with no answer, ends it. */
type Asked =
  | { readonly kind: "out"; readonly way: RefreshWay | null; readonly at: string | null; readonly landings: number }
  | { readonly kind: "ran"; readonly way: RefreshWay | null; readonly at: string | null }

/** The one place a press's note moves. The note takes its way from the store while the write is out, so the way pressed goes on
 * spinning once the store has let the write go. */
function asked(note: Asked | null, write: Pick<Shown<unknown, unknown, RefreshWay>, "phase" | "way" | "landings">, checkedAt: string | null): Asked | null {
  if (isPending(write.phase)) {
    if (write.phase === "unanswered") return note
    const way = write.way
    if (note?.kind !== "out") return { kind: "out", way, at: checkedAt, landings: write.landings }
    return note.way === way ? note : { ...note, way }
  }
  const next: Asked | null = note?.kind === "out" ? (write.landings > note.landings ? { kind: "ran", way: note.way, at: note.at } : null) : note
  return next?.kind === "ran" && next.at !== checkedAt ? null : next
}

/** A subscription's refresh, shared by its heading's Refresh and, when it lists no servers, by the failure in the table's place, so
 * both show the one write. Retry's one write stores the agent shown and refreshes with it, and is Pending until the view has
 * reached the revision the daemon answered at. Each press names its way in the store; the way pressed spins, and the others wait
 * at half strength. A write with no way spins on the heading's Refresh. Null for a pasted source, which has no refresh. */
export function useRefresh(source: SourceView) {
  const now = useNow()
  const remote = source.origin.kind === "remote" ? source.origin : null
  const controls = useControls()
  const action = actions.refresh(source.id)
  const write = controls.shown(action)
  const refreshes = useRefreshes(source.id, remote !== null)
  /* The view doesn't say a refresh is running, so once the write has landed the sign turns, from the page's own note, until the
   * daemon has checked since. */
  const [note, setNote] = useState<Asked | null>(null)
  const next = asked(note, write, remote?.fetch.checkedAt ?? null)
  if (next !== note) setNote(next)
  if (!remote) return null
  const { checkedAt, failures, failureCode } = remote.fetch
  const refresh: Refresh = failures > 0 ? { failing: true, code: failureCode } : { failing: false, at: checkedAt }
  const go = (way: RefreshWay, userAgent?: string) => controls.press(action, userAgent === undefined ? {} : { userAgent }, { what: source.name, way })
  const run: RefreshRun = fetchedOf(remote) === "first" ? { kind: "first" }
    : isPending(write.phase) || next?.kind === "ran" ? { kind: "running", by: next?.way ?? "heading", pending: isPending(write.phase) }
    : { kind: "idle" }
  return {
    remote, refresh, now, go, run,
    sent: write.sent?.userAgent,
    refreshesRead: refreshes.isSuccess,
    worked: refresh.failing ? workedMark(lastWorked(refreshes.data ?? [], remote.fetch), now) : null,
    nextIn: spanOf(Math.max(0, Date.parse(remote.dueAt) - now)),
  }
}

export type Refreshing = NonNullable<ReturnType<typeof useRefresh>>

/** The failure's own rows, its User-Agents and Retry, as its card holds them, wherever they stand. */
export function RefreshFailure({ r, way }: { r: Refreshing; way: RefreshWay }) {
  if (!r.refresh.failing || !r.worked) return null
  const out = r.run.kind === "running" && r.run.pending ? r.run : null
  return <FailureCard remote={r.remote} refresh={r.refresh} worked={r.worked} nextIn={r.nextIn} pending={out !== null} spinning={out?.by === way} sent={r.sent} retry={(userAgent) => r.go(way, userAgent)} />
}

/** Refresh: its sign and how long ago it last worked, and a click refreshes; its hover is rows of when it next runs and how often,
 * as Settings writes it, or that it is off while paused. While refreshes fail both
 * turn amber and the age is the last good list's, and a click opens the failure's card, unless the source lists no servers: the
 * failure then stands in its table's place, so the heading doesn't hold it twice, and Refresh is the one it is while refreshes
 * work, as beside the empty block's Refresh. Until the daemon has checked again, the
 * first refresh included, the button is at half strength and takes no press, as every button whose action is out, and the
 * spinner stands in its sign's place unless another way was pressed, as its card's Retry, which spins instead. The first says
 * nothing else, since nothing has worked yet. */
function RefreshControl({ r, listsNone }: { r: Refreshing; listsNone: boolean }) {
  const { remote, refresh, now, run, worked, nextIn } = r
  const { checkedAt } = remote.fetch
  const first = run.kind === "first"
  const turning = run.kind !== "idle"
  const spinning = run.kind === "running" && run.by === "heading"
  const interval = remote.refreshEverySeconds || remote.provider.suggestedRefreshSeconds || DEFAULT_REFRESH
  const next: Line[] = remote.paused ? [{ name: "Refresh", words: "Off" }]
    : [{ name: "Next", words: `in ${spanWords(nextIn)}` }, { name: "Refresh", words: `every ${refreshWords(interval)}` }]
  /* Until its refreshes are read, a failing button's age would be only a bound, so it waits with its sign alone. */
  const shown = worked ? r.refreshesRead && worked.shown !== "—" && <>{worked.shown}</>
    : first || checkedAt === null ? null
    : <>{ago(checkedAt, now)}</>
  const heard = refresh.failing ? "Refresh failing"
    : turning || checkedAt === null ? "Refreshing…"
    : now - Date.parse(checkedAt) < 60_000 ? "Refresh, last worked just now" : `Refresh, last worked ${spanSpoken(spanOf(now - Date.parse(checkedAt)))} ago`
  const button = (onClick?: () => void) => (
    <Button variant="outline" size={shown || spinning ? "default" : "icon"} className={cn(refresh.failing && "text-warning")} aria-label={heard} disabled={turning} onClick={onClick}>
      {first || spinning ? <Spinner role={undefined} aria-label={undefined} aria-hidden /> : <RefreshCwIcon />}
      {shown && <span className={cn(spinning && "motion-reduce:hidden")}>{shown}</span>}
      {spinning && <span className="hidden motion-reduce:inline">Refreshing…</span>}
    </Button>
  )
  if (!worked || !refresh.failing || listsNone) return <Tip lines={next}>{button(() => r.go("heading"))}</Tip>
  return (
    <Popover>
      <PopoverTrigger asChild>{button()}</PopoverTrigger>
      <PopoverContent align="end" collisionPadding={16} className="w-72 max-w-[calc(100vw-2rem)] gap-3 text-xs/relaxed">
        <RefreshFailure r={r} way="card" />
      </PopoverContent>
    </Popover>
  )
}

type Link = { label: string; href: string; icon: LucideIcon }

/** Wide: Settings, then Account and Support, then Delete. Narrow: a menu of them. A pasted source has Settings and
 * Delete at every width. */
/** A heading's Delete, a source's and a group's alike: the plain outlined icon button, which is not red, whose card names it. */
export function DeleteButton({ name, disabled, onDelete }: { name: string; disabled: boolean; onDelete: () => void }) {
  return <Tip lines={[{ words: "Delete" }]}><Button size="icon" variant="outline" aria-label={`Delete ${name}`} disabled={disabled} onClick={onDelete}><Trash2Icon /></Button></Tip>
}

function Actions({ source, remote, openSettings, onDelete, deleteWaits }: { source: SourceView; remote: Remote | null; openSettings: () => void; onDelete: () => void; deleteWaits: boolean }) {
  const settings = <Tip lines={[{ words: "Settings" }]}><Button size="icon" variant="outline" aria-label={`Settings for ${source.name}`} onClick={openSettings}><Settings2Icon /></Button></Tip>
  const remove = <DeleteButton name={source.name} disabled={deleteWaits} onDelete={onDelete} />
  if (!remote) return <>{settings}{remove}</>
  const links: Link[] = [
    remote.provider.profileUrl && { label: "Account", href: remote.provider.profileUrl, icon: UserIcon },
    remote.provider.supportUrl && { label: "Support", href: remote.provider.supportUrl, icon: LifeBuoyIcon },
  ].filter((l): l is Link => l !== "")
  return (
    <>
      {/* From 880px of page width the actions are buttons, and below it all but Refresh sit in a menu. */}
      <span className="hidden items-center gap-1.5 @min-[55rem]/main:flex">
        {settings}
        {links.map(({ label, href, icon: Icon }) => (
          <Tip key={label} lines={[{ words: label }]}>
            <Button asChild size="icon" variant="outline"><a href={href} target="_blank" rel="noreferrer" aria-label={label}><Icon /></a></Button>
          </Tip>
        ))}
        {remove}
      </span>
      <DropdownMenu modal={false}>
        <Tip lines={[{ words: "More" }]}>
          <DropdownMenuTrigger asChild><Button size="icon" variant="outline" aria-label={`More for ${source.name}`} className="@min-[55rem]/main:hidden"><EllipsisIcon /></Button></DropdownMenuTrigger>
        </Tip>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={openSettings}><Settings2Icon />Settings</DropdownMenuItem>
          {links.map(({ label, href, icon: Icon }) => (
            <DropdownMenuItem key={label} asChild><a href={href} target="_blank" rel="noreferrer"><Icon />{label}<ArrowUpRightIcon className="text-muted-foreground ml-auto" aria-label="Opens in new tab" /></a></DropdownMenuItem>
          ))}
          <DropdownMenuItem disabled={deleteWaits} onSelect={onDelete}><Trash2Icon />Delete</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </>
  )
}

/** The provider's announcement in one line, three on a phone. Only a line cut short opens its card, in full. */
function Announcement({ text }: { text: string }) {
  const [cut, setCut] = useState(false)
  const measured = useRef<ResizeObserver>(null)
  const measure = (el: HTMLSpanElement | null) => {
    measured.current?.disconnect()
    if (!el) return
    measured.current = new ResizeObserver(() => setCut(el.scrollHeight > el.clientHeight + 1 || el.scrollWidth > el.clientWidth + 1))
    measured.current.observe(el)
  }
  const line = <span ref={measure} className="text-muted-foreground line-clamp-1 whitespace-pre-line max-sm:line-clamp-3">{text}</span>
  if (!cut) return <p data-slot="announcement" className="w-fit max-w-full">{line}</p>
  return <Fact lines={[{ words: text }]} label={text} className="w-fit max-w-full">{line}</Fact>
}

/** A source's heading: the dot, the name, the facts, Refresh and the actions on one centre line, the actions at its right end. The
 * facts are server trouble, expiry, traffic and the week, in that order, and each fact the page doesn't have is left out. When they
 * don't all fit beside the name they go under it together, on a centre line of their own; on a phone they run under the actions
 * too. A subscription no refresh has worked for yet shows its dot, name, Refresh and the actions alone. `servers` is null until the
 * server list is read. A source that lists none says so in the table's place, so its heading doesn't. Its Delete waits while
 * another write of the source is out (`deleteWaits`). */
export function SourceHeading({ source, servers, refresh: r, name, openSettings, onDelete, deleteWaits = false }: {
  source: SourceView; servers: readonly ServerView[] | null; refresh: Refreshing | null; name: ReactNode; openSettings: () => void; onDelete: () => void; deleteWaits?: boolean
}) {
  const now = useNow()
  const { asked, answer } = useWeekRead()
  const week = answer ? weekOf(answer.buckets, source, answer.at) : null
  const { fetched, facts, troubles, dot } = standingOf(source, servers, week?.failovers ?? null, now)
  const remote = source.origin.kind === "remote" ? source.origin : null
  const servedTrouble = servers?.length === 0 ? null : troubles.find((t) => t.kind === "empty" || t.kind === "servers") ?? null
  const expiry = facts?.expiry?.kind === "expires" || facts?.expiry?.kind === "expired" ? facts.expiry : null
  const traffic = facts?.traffic?.kind === "left" || facts?.traffic?.kind === "unknown" ? facts.traffic : null
  return (
    <div data-slot="source-heading" data-control={`${actions.source(source.id).control} ${actions.refresh(source.id).control}`} className="flex flex-col gap-1">
      <div className="flex items-start gap-x-4 @max-[30rem]/main:grid @max-[30rem]/main:grid-cols-[minmax(0,1fr)_auto] @max-[30rem]/main:gap-y-1">
        <div className="flex min-w-0 flex-1 flex-wrap items-start gap-x-5 gap-y-1 @max-[30rem]/main:contents">
          <span className="flex min-w-0 max-w-full items-start gap-2 py-1">
            <SourceDot dot={dot} />
            {name}
          </span>
          {fetched === "worked" && (
            <div data-slot="source-facts" className="flex min-w-0 flex-wrap items-start gap-x-5 gap-y-1 @max-[30rem]/main:col-span-2 @max-[30rem]/main:row-start-2">
              {servedTrouble && <span className={ON_LINE}><Toned tone={servedTrouble.tone}>{servedTrouble.words}</Toned></span>}
              {expiry && <ExpiryFact expiry={expiry} trouble={troubleOf(troubles, "expiry")} />}
              {traffic && <TrafficFact traffic={traffic} trouble={troubleOf(troubles, "traffic")} />}
              <WeekFact week={week} asked={asked} trouble={troubleOf(troubles, "failovers")} />
            </div>
          )}
        </div>
        <span className="flex shrink-0 items-center gap-1.5 @max-[30rem]/main:col-start-2 @max-[30rem]/main:row-start-1">
          {r && <RefreshControl r={r} listsNone={servers?.length === 0} />}
          <Actions source={source} remote={remote} openSettings={openSettings} onDelete={onDelete} deleteWaits={deleteWaits} />
        </span>
      </div>
      {fetched === "worked" && remote?.provider.announcement && <Announcement text={remote.provider.announcement} />}
    </div>
  )
}

/** What fixes skipped entries, where something can: the provider's support, or pasting the links again. */
export type SkippedFix = { kind: "support"; href: string } | { kind: "paste"; open: () => void }

/** A quiet list mark for entries a feed listed and the gateway refused. Its card is rows: Skipped and their count, then each entry
 * and why, so the count and the reasons share one edge. With
 * `withConfig` each names its config, for the mark beside Search. A fix makes it a card that opens on a click and holds it. */
export function SkippedMark({ entries, withConfig = false, fix }: { entries: readonly SkippedEntry[]; withConfig?: boolean; fix?: SkippedFix | null }) {
  /* An entry reads as the table's names do, its tag muted after its config; its reason is muted beside it. */
  const named: Line[] = entries.map((e) => ({
    name: withConfig && e.config ? `${e.config} · ${e.name}` : e.name,
    nameDrawn: <span>{withConfig && e.config ? <>{e.config}<Quiet> · {e.name}</Quiet></> : e.name}</span>,
    words: e.reason, drawn: <Quiet>{e.reason}</Quiet>,
  }))
  const lines: Line[] = [{ name: "Skipped", mark: <Sign icon={ListXIcon} />, words: String(entries.length) }, ...named]
  const label = [skippedWords(entries.length), ...named.map((l) => `${l.name}: ${l.words}`)].join(". ")
  const mark = <ListXIcon aria-hidden className="size-3.5" />
  const quiet = "text-muted-foreground size-7 shrink-0 items-center justify-center font-normal"
  if (!fix) return <Fact lines={lines} label={label} className={quiet}>{mark}</Fact>
  const action = fix.kind === "support"
    ? <Button asChild size="sm" variant="outline" className="self-start"><a href={fix.href} target="_blank" rel="noreferrer"><LifeBuoyIcon />Support<ArrowUpRightIcon className="text-muted-foreground" aria-label="Opens in new tab" /></a></Button>
    : <Button size="sm" variant="outline" className="self-start" onClick={fix.open}><ClipboardPasteIcon />Paste again</Button>
  return <ActionCard lines={lines} label={label} className={quiet} action={action}>{mark}</ActionCard>
}
