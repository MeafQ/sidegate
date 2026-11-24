import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type PointerEvent as ReactPointerEvent, type ReactNode } from "react"
import { useQueryClient } from "@tanstack/react-query"
import { CircleXIcon } from "lucide-react"
import { CartesianGrid, Line, LineChart, ReferenceLine, usePlotArea, XAxis, YAxis } from "recharts"
import { ChartContainer, type ChartConfig } from "@/components/ui/chart"
import { Spinner } from "@/components/ui/spinner"
import { MarkSlot } from "@/components/card"
import { failedWords, ReadFailed } from "@/components/page"
import { Quiet } from "@/components/status"
import { cn } from "@/lib/utils"
import { BUCKET_SECONDS, dayTicks, lastWeek, serverHistory, serverWeek, type DayTick, type ServerPoint } from "@/lib/analytics"
import { useNow } from "@/lib/clock"
import { dayLetter, hoursWords, ms, placePair } from "@/lib/format"
import { useAnalytics } from "@/lib/queries"
import { keys } from "@/lib/keys"
import type { ServerView } from "@/lib/schema"

const AXIS = { tickLine: false, axisLine: false, tickMargin: 6 } as const

/** A day under a week chart, its two letters, today's in the text's ink and bold. */
export function DayLetter({ x, y, letter, today, textAnchor = "middle" }: { x?: number; y?: number; letter: string; today: boolean; textAnchor?: "start" | "middle" | "end" | "inherit" }) {
  return <text data-slot="day-letter" x={x} y={y} dy="0.71em" textAnchor={textAnchor} className={today ? "fill-foreground font-semibold" : "fill-muted-foreground"}>{letter}</text>
}

/** A day on the server chart's axis, at the bucket `dayTicks` stands it at. */
function DayTick({ x, y, payload, textAnchor, days }: {
  x?: number; y?: number; payload?: { value: string }; textAnchor?: "start" | "middle" | "end" | "inherit"; days: ReadonlyMap<string, DayTick>
}) {
  const day = payload && days.get(payload.value)
  return day ? <DayLetter x={x} y={y} textAnchor={textAnchor} letter={day.letter} today={day.today} /> : null
}

/** A tick on the server chart's ms axis, on one line however many digits it has, "4000 ms", 6px before the plot. */
function MsTick({ x, y, payload, unit = false }: { x?: number; y?: number; payload?: { value: number }; unit?: boolean }) {
  if (!payload) return null
  const words = unit ? (payload.value === 1 ? "ms" : "") : ms(payload.value)
  return <text x={x} y={y} dy="0.355em" textAnchor="end" className="fill-muted-foreground">{words}</text>
}

/** A value in a room as wide as the widest it can hold, `fit`, so nothing beside it moves as it changes. */
function Fitted({ fit, children }: { fit: string; children?: ReactNode }) {
  return (
    <span data-slot="chart-value" className="inline-grid *:col-start-1 *:row-start-1">
      <span aria-hidden data-slot="chart-fit" className="invisible">{fit}</span>
      <span>{children}</span>
    </span>
  )
}

/** One count a chart's legend line keys: the mark it counts where one is drawn, its name, and its value in a room as wide as `fit`. */
export type ChartKey = { name: string; mark?: ReactNode; value: ReactNode; fit: string }

/** A chart's readout, over its plot: the muted window it reads, "7d" or the place's six hours, over its legend line. Each key is its
 * mark, its muted name and its value 4px after it. The line is one line in every state, and nothing in it appears, vanishes or
 * moves. `held` keeps its room with nothing in it seen. */
export function Readout({ window, keys, held = false, children }: { window: string; keys: readonly ChartKey[]; held?: boolean; children?: ReactNode }) {
  return (
    <div data-slot="chart-readout" aria-live="polite" className="flex flex-col gap-0.5 tabular-nums">
      <span data-slot="chart-window" className="text-muted-foreground">{window}</span>
      <div data-slot="chart-legend" className={cn("flex items-center gap-x-3 whitespace-nowrap", held && "invisible")}>
        {keys.map(({ name, mark, value, fit }) => (
          <span key={name} data-slot="chart-key" className="inline-flex items-center gap-1.5">
            <MarkSlot>{mark}</MarkSlot>
            <span className="inline-flex items-center gap-1"><span data-slot="chart-name" className="text-muted-foreground">{name}</span><Fitted fit={fit}>{value}</Fitted></span>
          </span>
        ))}
        {children}
      </div>
    </div>
  )
}

/** A chart's key by its line's mark alone, as p50 and p95 are in the ping keys. */
const LineKey = ({ series }: { series: "pingMs" | "pingP95Ms" }) =>
  <span className="inline-flex items-center gap-1"><MarkSlot><LineMark series={series} /></MarkSlot><span data-slot="chart-name" className="text-muted-foreground">{PING[series].label}</span></span>

/** The server chart's ping keys as one group, in the details' own form, "Ping p50 / p95 38 / 52 ms": the muted "Ping", the p50 and
 * p95 keys 8px apart, each mark 4px before its own name and so nearer it than the name before, then their one value 8px after
 * p95, so it reads as the pair. Its room, "0000 / 0000 ms", ends the line, so at rest it is the card's own space after the keys.
 * A figure the place lacks is the muted dash, "76 / — ms". */
function PingKeys({ point, held }: { point: ServerPoint | null; held: boolean }) {
  const pair = point ? placePair(point.pingMs, point.pingP95Ms) : ""
  return (
    <span data-slot="chart-key" data-key="ping" className={cn("inline-flex items-center gap-2", held && "invisible")}>
      <span data-slot="chart-name" className="text-muted-foreground">Ping</span>
      <span className="inline-flex items-center gap-2"><LineKey series="pingMs" /><LineKey series="pingP95Ms" /></span>
      <Fitted fit="0000 / 0000 ms">{pair.split("—").flatMap((part, i) => (i === 0 ? [part] : [<Quiet key={i}>—</Quiet>, part]))}</Fitted>
    </span>
  )
}

/** What of a server's readout is seen: all of it over a plot, Failovers alone over the frame of a week with failovers and no reading,
 * and none over the frame of a week with nothing or one not read, which keeps the line's room. */
type Seen = "all" | "failovers" | "none"

/** A server's readout: "7d" and the week's failovers, or a place's six hours and its failovers and pair. The failover mark follows
 * the week, not the place. */
function ServerReadout({ points, place, seen }: { points: readonly ServerPoint[]; place: number | null; seen: Seen }) {
  const week = points.reduce((n, p) => n + p.forced, 0)
  const at = place === null ? null : points[place]
  const start = at && Date.parse(at.start)
  return (
    <Readout window={start ? hoursWords(start, start + BUCKET_SECONDS * 1000) : "7d"} held={seen === "none"}
      keys={[{ name: "Failovers", mark: week > 0 && <FailoverMark />, value: at ? at.forced : week, fit: "00" }]}>
      <PingKeys point={at} held={seen !== "all"} />
    </Readout>
  )
}

/** A dot only where a point stands alone. A line needs two points in a row, and a dormant server's probes are far apart. */
function lonePoints<T>(rows: T[], key: keyof T) {
  const at = (i: number) => rows[i]?.[key] ?? null
  return ({ cx, cy, index = 0, stroke }: { cx?: number; cy?: number; index?: number; stroke?: string }) => (
    <circle key={index} cx={cx} cy={cy} r={at(index) !== null && at(index - 1) === null && at(index + 1) === null ? 2 : 0} fill={stroke} />
  )
}

const PING = { pingMs: { label: "p50", color: "var(--foreground)" }, pingP95Ms: { label: "p95", color: "var(--muted-foreground)" } } satisfies ChartConfig

/** Each mark a chart draws, named the same way in its legend line and every chart: the p50 line, the p95 line, a failover's dashed
 * line and a source's run in use. */
const LineMark = ({ series }: { series: "pingMs" | "pingP95Ms" }) =>
  <span aria-hidden className={cn("inline-block w-3 shrink-0 rounded-full", series === "pingMs" ? "bg-foreground h-0.5" : "bg-muted-foreground h-px")} />
export const FailoverMark = () => <span aria-hidden className="border-warning inline-block h-2.5 w-0 shrink-0 border-l border-dashed" />
export const UseMark = () => <span aria-hidden className="bg-muted-foreground inline-block h-0.5 w-3 shrink-0 rounded-full" />

/** The place a week chart reads, or null for the whole week. A mouse on the plot picks the place under it and leaving lets it go. A
 * tap or a drag picks it too, and it stays after the finger lifts, until a press off the plot. On the focused chart the arrow keys
 * move it, and Home and End take the first and the last. `count` is how many places there are; `pick` is the place under a pointer. */
export function usePlace(count: number, pick: (e: ReactPointerEvent<Element>) => number) {
  const [place, setPlace] = useState<number | null>(null)
  const plot = useRef<Element | null>(null)
  const holding = place !== null
  useEffect(() => {
    if (!holding) return
    const off = (e: PointerEvent) => { if (!(e.target instanceof Node && plot.current?.contains(e.target))) setPlace(null) }
    document.addEventListener("pointerdown", off, true)
    return () => document.removeEventListener("pointerdown", off, true)
  }, [holding])
  const plotProps = {
    ref: (el: Element | null) => { plot.current = el },
    onPointerDown: (e: ReactPointerEvent<Element>) => setPlace(pick(e)),
    onPointerMove: (e: ReactPointerEvent<Element>) => setPlace(pick(e)),
    onPointerLeave: (e: ReactPointerEvent<Element>) => { if (e.pointerType === "mouse") setPlace(null) },
  }
  const onKeyDown = (e: KeyboardEvent) => {
    const last = count - 1
    const to = e.key === "ArrowRight" ? (place === null ? 0 : Math.min(last, place + 1))
      : e.key === "ArrowLeft" ? (place === null ? last : Math.max(0, place - 1))
      : e.key === "Home" ? 0 : e.key === "End" ? last : null
    if (to === null || count === 0) return
    e.preventDefault()
    setPlace(to)
  }
  return { place, setPlace, plotProps, onKeyDown }
}

/** The bucket a server's chart reads: a band in the border's ink as wide as the bucket, under the lines. */
function Band({ place, count }: { place: number | null; count: number }) {
  const plot = usePlotArea()
  if (place === null || !plot) return null
  const width = plot.width / count
  return <rect data-slot="chart-band" x={plot.x + width * place} y={plot.y} width={width} height={plot.height} className="fill-border" />
}

/** The plot's area as what a pointer or a finger picks a place on, the one thing in the chart that takes either. A vertical drag
 * still scrolls the page. */
function PlotArea(props: Omit<ReturnType<typeof usePlace>["plotProps"], "ref"> & { plotRef: (el: Element | null) => void }) {
  const plot = usePlotArea()
  const { plotRef, ...handlers } = props
  if (!plot) return null
  return <rect ref={plotRef} data-slot="chart-plot" x={plot.x} y={plot.y} width={plot.width} height={plot.height} fill="transparent" className="pointer-events-auto touch-pan-y" {...handlers} />
}

/** One server's last week as read: nothing in it, its failures and failovers, or those and its ping per 6 hours. */
export type ServerWeek = { state: "loading" } | { state: "failed"; error: unknown } | { state: "empty" } | { state: "facts" | "chart"; points: ServerPoint[] }

const LOADING: ServerWeek = { state: "loading" }

/** Reads a server's last week for the card or dialog's opening `opening`, a number each new opening changes, or for none while it
 * is null, so each opens at the size of what it shows. A week counts once a read has landed since the opening, or the one before
 * was still fresh when it asked; the query's count of landings tells, since a clock set back would put a landing before its
 * opening. What it shows then stays until the next opening, so no later read resizes what is open. */
export function useServerWeek(server: ServerView, opening: number | null): ServerWeek {
  const { from, to } = useMemo(() => lastWeek(), [])
  const asked = { from, to, bucketSeconds: BUCKET_SECONDS, lineages: [server.id] }
  const report = useAnalytics(asked, opening !== null)
  const query = useQueryClient().getQueryState(keys.analytics(asked))
  const landings = (query?.dataUpdateCount ?? 0) + (query?.errorUpdateCount ?? 0)
  const [opened, setOpened] = useState<{ opening: number; landings: number; week: ServerWeek } | null>(null)
  /* Enabling the read fetches a stale week. An opening while it is already enabled has to ask itself. */
  useEffect(() => {
    if (opening !== null && report.isStale && !report.isFetching) void report.refetch()
    // Each opening asks once, from what its own render read.
    // oxlint-disable-next-line react-hooks/exhaustive-deps
  }, [opening])
  if (opening === null) return LOADING
  const current = opened?.opening === opening ? opened : { opening, landings, week: LOADING }
  if (current.week.state !== "loading") return current.week
  const landed = landings > current.landings
  let week: ServerWeek = LOADING
  if (report.isFetching) week = LOADING
  else if (report.isError && landed) week = { state: "failed", error: report.error }
  /* The query keeps the last answer while a new one loads, and that answer is another server's. */
  else if (report.data && !report.isPlaceholderData && (landed || !report.isStale)) {
    const points = serverHistory(report.data.buckets, server.id, from, to, BUCKET_SECONDS)
    const shape = serverWeek(points)
    week = shape === "empty" ? { state: "empty" } : { state: shape, points }
  }
  if (current !== opened || week !== LOADING) setOpened({ ...current, week })
  return week
}

/** A server's name card's content: the readout's line with the four-digit pair, so it never wraps, and inside the 16px gutter. */
const CARD_WIDTH = "w-[min(20rem,calc(100vw-4rem))]"

/** The chart's frame with no line in it: the week's days, its failovers' dashed lines as a plot draws them, and what stands in
 * place of the lines. The ms axis draws its unit as its one tick, at the top, so it stands where a week with readings has its top
 * tick's unit and nothing moves between them. */
function EmptyChart({ points, children }: { points: ServerPoint[]; children: ReactNode }) {
  const days = dayTicks(points, useNow())
  return (
    <div data-slot="chart-frame" className="relative">
      <ChartContainer config={PING} className="aspect-auto h-32 w-full">
        <LineChart data={points} margin={{ left: 0, right: 4, top: 8 }} accessibilityLayer={false}>
          <CartesianGrid vertical={false} />
          <XAxis dataKey="start" {...AXIS} tickFormatter={dayLetter} scale="band" tick={<DayTick days={new Map(days.map((d) => [d.start, d]))} />} ticks={days.map((d) => d.start)} interval={0} />
          <YAxis {...AXIS} tickSize={0} width={56} domain={[0, 1]} allowDataOverflow ticks={[0, 1]} tick={<MsTick unit />} />
          {points.filter((p) => p.forced > 0).map((p) => <ReferenceLine key={p.start} x={p.start} stroke="var(--warning)" strokeDasharray="3 3" />)}
        </LineChart>
      </ChartContainer>
      <div className="absolute inset-0 bottom-6 left-14 flex items-center justify-center">{children}</div>
    </div>
  )
}

/** A week's ping per six hours under its readout. The place the pointer, a finger or the keys are on reads in the readout and is
 * the band on the plot. In the details the chart takes focus, so the arrow keys move the place, and focus leaving it returns to
 * the week. A press off the plot has let the place go already, as it starts and before it moves focus, so the blur is the keys'. */
function WeekPlot({ points, card }: { points: ServerPoint[]; card: boolean }) {
  const days = dayTicks(points, useNow())
  const { place, setPlace, plotProps: { ref, ...handlers }, onKeyDown } = usePlace(points.length, (e) => {
    const box = e.currentTarget.getBoundingClientRect()
    return Math.min(points.length - 1, Math.max(0, Math.floor(((e.clientX - box.left) / box.width) * points.length)))
  })
  const keys = card ? {} : { tabIndex: 0, role: "group", "aria-label": "The week by six hours", onKeyDown, onBlur: () => setPlace(null) }
  return (
    <>
      <ServerReadout points={points} place={place} seen="all" />
      {/* Recharts draws each mark, the axes' too, in a focusable layer over the plot's area, so the pointer passes through all it
        * draws: a press on a dot would take focus from the chart and count as off the plot. */}
      <ChartContainer config={PING} className="focus-visible:ring-ring/50 aspect-auto h-32 w-full rounded-md outline-none focus-visible:ring-2 [&_svg]:pointer-events-none" {...keys}>
        <LineChart data={points} margin={{ left: 0, right: 4, top: 8 }} accessibilityLayer={false}>
          <CartesianGrid vertical={false} />
          <XAxis dataKey="start" {...AXIS} tickFormatter={dayLetter} scale="band" tick={<DayTick days={new Map(days.map((d) => [d.start, d]))} />} ticks={days.map((d) => d.start)} interval={0} />
          <YAxis {...AXIS} tickSize={0} width={56} tick={<MsTick />} />
          <Band place={place} count={points.length} />
          {points.filter((p) => p.forced > 0).map((p) => <ReferenceLine key={p.start} x={p.start} stroke="var(--warning)" strokeDasharray="3 3" />)}
          <Line dataKey="pingP95Ms" type="monotone" stroke="var(--color-pingP95Ms)" strokeWidth={1.5} dot={lonePoints(points, "pingP95Ms")} isAnimationActive={false} />
          <Line dataKey="pingMs" type="monotone" stroke="var(--color-pingMs)" strokeWidth={2} dot={lonePoints(points, "pingMs")} isAnimationActive={false} />
          <PlotArea plotRef={ref} {...handlers} />
        </LineChart>
      </ChartContainer>
    </>
  )
}

/** A server's week from heartbeats and rounds, under its readout: a gap has no probe, a line marks a failover, and the readout's
 * keys name each mark. The name's card (`card`) and the details draw the same. A week with failovers and no reading keeps the
 * chart's frame with its failovers' lines and a muted dash, under its failovers' count; one with nothing keeps the frame with the
 * dash, one still read keeps it with a spinner, and one that failed to read keeps it with the failed read and, where `retry` is given,
 * Retry, each under the readout's room. */
export function ServerStats({ server, week, card = false, retry }: {
  server: ServerView; week: ServerWeek; card?: boolean; retry?: { retry: () => void; retrying: boolean }
}) {
  const { from, to } = useMemo(() => lastWeek(), [])
  const none = useMemo(() => serverHistory([], server.id, from, to, BUCKET_SECONDS), [server.id, from, to])
  if (week.state === "failed" && !retry) return <span className="text-muted-foreground flex items-center gap-2"><CircleXIcon aria-hidden className="size-3.5 shrink-0" />{failedWords(week.error)}</span>
  const block = (inside: ReactNode) => <div className={cn("flex flex-col gap-2", card && CARD_WIDTH)}>{inside}</div>
  const framed = (points: ServerPoint[], seen: Seen, inside: ReactNode) => block(<><ServerReadout points={points} place={null} seen={seen} /><EmptyChart points={points}>{inside}</EmptyChart></>)
  const noReading = <Quiet aria-label="No reading in 7 days">—</Quiet>
  if (week.state === "loading") return framed(none, "none", <Spinner aria-label="Loading 7-day history" className="text-muted-foreground" />)
  if (week.state === "failed") return framed(none, "none", retry && <ReadFailed place="frame" error={week.error} retry={retry.retry} retrying={retry.retrying} />)
  if (week.state === "empty") return framed(none, "none", noReading)
  if (week.state === "facts") return framed(week.points, "failovers", noReading)
  return block(<WeekPlot points={week.points} card={card} />)
}
