import { Fragment, useLayoutEffect, useRef, useState, type RefObject } from "react"
import { ArrowRightIcon, ChevronDownIcon, CircleDashedIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Fact, type Line } from "@/components/card"
import type { Read } from "@/components/page"
import { ServerName, ServerNameWaiting } from "@/components/server"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { MarkIcon, Quiet, RatingMark, type Mark } from "@/components/status"
import { REASON_WORDS, REPLACED_WORDS, SIGNAL_WORDS, ago, clock, ms } from "@/lib/format"
import type { LineageId, ReplacedEvent, ServerView, Selection, SlotView, SourceView, SwitchEvent, SwitchReason, Verdict } from "@/lib/schema"
import type { ServerRating } from "@/lib/health"
import { downWords, listed } from "@/lib/slots"
import { cn } from "@/lib/utils"

const REASON_MARK: Record<SwitchReason, Mark> = {
  initialSelection: "none", failureMove: "danger", slownessMove: "warning", improvementMove: "ok", exitChanged: "none",
  serverLeftFeed: "warning", serverRefused: "danger", revisionChanged: "none", recovery: "ok", udpMove: "warning",
  preferred: "none",
}

/** A reason's mark, as its rail draws it and as the Reason select's option does, so an option looks like the row it filters. */
export function ReasonMark({ reason }: { reason: SwitchReason }) {
  return <span className={cn("inline-flex", REASON_MARK[reason] === "none" && "text-muted-foreground")}><MarkIcon mark={REASON_MARK[reason]} /></span>
}

const RESULT: Record<Verdict, { rating: ServerRating; words: string }> = {
  succeeded: { rating: "good", words: "Passed" }, failed: { rating: "bad", words: "Failed" }, unknown: { rating: "none", words: "Unknown" },
}

/** What names a server: the server list, the sources, and the list's read. */
type Names = { servers: Map<LineageId, ServerView>; sources: Pick<SourceView, "id" | "name">[]; list: Read<unknown> }

function SelectionWords({ selection, names }: { selection: Selection; names: Names }) {
  if (selection.kind === "down") return <Quiet>{downWords(selection.reason)}</Quiet>
  if (selection.kind === "direct") return <span>Direct</span>
  if (selection.kind === "block") return <span>Block</span>
  return (
    <span className="inline-flex min-w-0 flex-wrap items-center gap-x-2">
      <ServerName id={selection.server} {...names} />
    </span>
  )
}

type Signal = SwitchEvent["evidence"][number]

/** A column a narrow table can do without, in the order they go: what its header says and how its cell draws a signal. */
type Optional = { key: string; name: string; right?: boolean; mono?: boolean; words: (e: Signal) => string }
const OPTIONAL: Optional[] = [
  { key: "newConnection", name: "Connect", right: true, words: (e) => ms(e.newConnectionMs) },
  { key: "target", name: "Target", mono: true, words: (e) => e.target },
  { key: "round", name: "Round", right: true, words: (e) => (e.round ? String(e.round) : "—") },
  { key: "ping", name: "Ping", right: true, words: (e) => ms(e.pingMs) },
  { key: "signal", name: "Signal", words: (e) => SIGNAL_WORDS[e.kind] },
]

/** How many of the optional columns the table hides: as few as let it fit its frame without scrolling. It is measured whenever the
 * frame or the table changes size, with every column shown, hiding them in their order until it fits, and the cells are left as
 * measured, so the render that follows changes nothing on the screen. */
function useHidden(frame: RefObject<HTMLDivElement | null>) {
  const [hidden, setHidden] = useState(0)
  useLayoutEffect(() => {
    const el = frame.current
    const table = el?.querySelector("table")
    if (!el || !table) return
    const fit = () => {
      const cells = (key: string) => el.querySelectorAll<HTMLElement>(`[data-col=${key}]`)
      for (const c of OPTIONAL) for (const cell of cells(c.key)) cell.hidden = false
      let k = 0
      for (; k < OPTIONAL.length && table.offsetWidth > el.clientWidth; k++) for (const cell of cells(OPTIONAL[k].key)) cell.hidden = true
      setHidden(k)
    }
    const resized = new ResizeObserver(fit)
    resized.observe(el)
    resized.observe(table)
    return () => resized.disconnect()
  }, [frame])
  return new Set(OPTIONAL.slice(0, hidden).map((c) => c.key))
}

/** A signal's Result: the Dot and its word. Where the table hides columns, its card holds them. */
function Result({ signal, hidden }: { signal: Signal; hidden: readonly Optional[] }) {
  const { rating, words } = RESULT[signal.verdict]
  const drawn = <span className="flex items-center gap-1.5 whitespace-nowrap"><RatingMark rating={rating} />{words}</span>
  if (hidden.length === 0) return drawn
  const lines: Line[] = hidden.map((c) => ({ name: c.name, words: c.words(signal), drawn: c.mono ? <span className="font-mono">{c.words(signal)}</span> : undefined }))
  return <Fact lines={lines} label={words} className="-my-1.5 justify-center">{drawn}</Fact>
}

/** In the table's order, the columns this width shows. */
const IN_ORDER = ["signal", "target", "round", "ping", "newConnection"]

function Evidence({ event, names }: { event: SwitchEvent; names: Names }) {
  const frame = useRef<HTMLDivElement>(null)
  const hides = useHidden(frame)
  const column = (key: string) => OPTIONAL.find((c) => c.key === key) as Optional
  const shown = (key: string) => !hides.has(key)
  const hidden = IN_ORDER.filter((key) => hides.has(key)).map(column)
  const head = (key: string) => <TableHead data-col={key} hidden={!shown(key)} className={cn(column(key).right && "text-right")}>{column(key).name}</TableHead>
  const cell = (key: string, e: Signal) => (
    <TableCell data-col={key} hidden={!shown(key)} className={cn(column(key).right && "text-right tabular-nums", column(key).mono && "font-mono", column(key).words(e) === "—" && "text-muted-foreground")}>{column(key).words(e)}</TableCell>
  )
  const [signal, ...rest] = IN_ORDER
  return (
    <div className="px-3 pb-3">
      <div ref={frame} className="overflow-hidden rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              {head(signal)}
              <TableHead>Server</TableHead>
              {rest.map((key) => <Fragment key={key}>{head(key)}</Fragment>)}
              <TableHead>Result</TableHead>
              <TableHead className="text-right">At</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {event.evidence.map((e, i) => (
              <TableRow key={i}>
                {cell(signal, e)}
                <TableCell><ServerName id={e.server} {...names} withProvider={false} /></TableCell>
                {rest.map((key) => <Fragment key={key}>{cell(key, e)}</Fragment>)}
                <TableCell><Result signal={e} hidden={hidden} /></TableCell>
                <TableCell className="text-right tabular-nums">{clock(e.at)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}

/** A switch, naming every slot it moved. One with signals opens on a click anywhere to show them; one without has nothing to open. */
function Row({ event, names, moved, last }: { event: SwitchEvent; names: Names; moved: string; last: boolean }) {
  const [open, setOpen] = useState(false)
  const opens = event.evidence.length > 0
  return (
    <li className="relative flex gap-3">
      <div className="flex flex-col items-center pt-2.5">
        {/* Until the runtime applies the move its mark is Pending, then its reason's. */}
        {event.appliedAt === null ? <Spinner aria-label="Applying" className="text-muted-foreground size-3.5" /> : <ReasonMark reason={event.reason} />}
        {!last && <span className="bg-border mt-1 w-px flex-1" aria-hidden />}
      </div>
      <Collapsible open={open} onOpenChange={setOpen} className={cn("min-w-0 flex-1 rounded-md", open && "bg-muted/40", !last && "mb-1")}>
        {/* The trigger holds only the reason; its ::after stretches over the row so a click anywhere opens it, and the
            time sits above that layer, so hovering it shows its title. */}
        <div className={cn("relative flex w-full min-w-0 items-start gap-3 rounded-md px-3 py-2", opens && "hover:bg-muted/50 has-focus-visible:ring-ring/50 has-focus-visible:ring-2")}>
          <div className="flex min-w-0 flex-1 flex-col gap-0.5">
            <div className="flex flex-wrap items-center gap-x-2">
              {opens ? (
                <CollapsibleTrigger asChild>
                  <button type="button" className="text-left font-medium outline-none after:absolute after:inset-0 after:rounded-md">{REASON_WORDS[event.reason]}</button>
                </CollapsibleTrigger>
              ) : <span className="font-medium">{REASON_WORDS[event.reason]}</span>}
              {moved && <span className="text-muted-foreground">{moved}</span>}
            </div>
            {JSON.stringify(event.from) !== JSON.stringify(event.to) && (
              <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
                <SelectionWords selection={event.from} names={names} />
                <ArrowRightIcon className="text-muted-foreground size-3.5 shrink-0" aria-label="to" />
                <SelectionWords selection={event.to} names={names} />
              </div>
            )}
          </div>
          <span className="text-muted-foreground relative shrink-0 tabular-nums" title={clock(event.requestedAt)}>{ago(event.requestedAt)}</span>
          {opens && <ChevronDownIcon className={cn("text-muted-foreground mt-0.5 size-4 shrink-0 transition-transform", open && "rotate-180")} aria-hidden />}
        </div>
        {opens && <CollapsibleContent><Evidence event={event} names={names} /></CollapsibleContent>}
      </Collapsible>
    </li>
  )
}

function ReplacedRow({ event, names, last }: { event: ReplacedEvent; names: Names; last: boolean }) {
  return (
    <li className="relative flex gap-3">
      <div className="flex flex-col items-center pt-2.5">
        <MarkIcon mark="warning" />
        {!last && <span className="bg-border mt-1 w-px flex-1" aria-hidden />}
      </div>
      <div className={cn("flex min-w-0 flex-1 items-start gap-3 px-3 py-2", !last && "mb-1")}>
        <div className="flex min-w-0 flex-1 flex-col gap-0.5">
          <div className="flex flex-wrap items-center gap-x-2">
            <span className="font-medium">{REPLACED_WORDS}</span>
            <span className="text-muted-foreground">{names.sources.find((s) => s.id === event.source)?.name}</span>
          </div>
          <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
            {event.previousLabel !== event.label && <>
              <span>{event.previousLabel}</span>
              <ArrowRightIcon className="text-muted-foreground size-3.5 shrink-0" aria-label="to" />
            </>}
            <span>{event.label}</span>
          </div>
        </div>
        <span className="text-muted-foreground shrink-0 tabular-nums" title={clock(event.at)}>{ago(event.at)}</span>
      </div>
    </li>
  )
}

type Entry = { at: number; key: string } & ({ event: SwitchEvent } | { replaced: ReplacedEvent })

/** Switches and replaced servers newest first. A switch has its reason, from and to, and the evidence behind it on demand. */
export function SwitchTimeline({ events, replaced = [], names, slots, limit }: { events: SwitchEvent[]; replaced?: ReplacedEvent[]; names: Names; slots?: Pick<SlotView, "id" | "name">[]; limit?: number }) {
  const entries: Entry[] = [
    ...events.map((event) => ({ at: Date.parse(event.requestedAt), key: event.id, event })),
    ...replaced.map((r) => ({ at: Date.parse(r.at), key: `${r.server}/${r.at}`, replaced: r })),
  ].sort((a, b) => b.at - a.at)
  const [shown, setShown] = useState(limit ?? entries.length)
  const visible = entries.slice(0, shown)
  return (
    <div className="flex flex-col gap-2">
      <ol className="flex flex-col">
        {visible.map((e, i) => "event" in e
          ? <Row key={e.key} event={e.event} names={names} moved={listed((slots ?? []).filter((s) => e.event.slots.includes(s.id)).map((s) => s.name))} last={i === visible.length - 1} />
          : <ReplacedRow key={e.key} event={e.replaced} names={names} last={i === visible.length - 1} />)}
      </ol>
      {shown < entries.length && <Button variant="ghost" size="sm" className="self-start" onClick={() => setShown(entries.length)}>Show all {entries.length}</Button>}
    </div>
  )
}

/** The timeline while its moves are on their way: rows in the shape they arrive in, a faint rail mark, the reason's place and the
 * time's, and the from → to line with each server's flag and name in skeleton. */
export function TimelineWaiting() {
  const server = (width: string) => <ServerNameWaiting width={width} />
  const rows = [["w-36", "w-28", "w-32"], ["w-28", "w-24", "w-36"]]
  return (
    <ol className="flex flex-col" aria-label="Loading events">
      {rows.map(([reason, from, to], i) => (
        <li key={reason + from} className="relative flex gap-3">
          <div className="flex flex-col items-center pt-2.5">
            <CircleDashedIcon aria-hidden className="text-muted-foreground/50 size-3.5" />
            {i < rows.length - 1 && <span className="bg-border mt-1 w-px flex-1" aria-hidden />}
          </div>
          <div className={cn("flex min-w-0 flex-1 items-start gap-3 px-3 py-2", i < rows.length - 1 && "mb-1")}>
            <div className="flex min-w-0 flex-1 flex-col gap-0.5">
              <span className="flex h-[1.625em] items-center"><Skeleton className={cn("h-2 rounded-full", reason)} /></span>
              <span className="flex h-[1.625em] items-center gap-2">{server(from)}<ArrowRightIcon aria-hidden className="text-muted-foreground size-3.5 shrink-0" />{server(to)}</span>
            </div>
            <span className="flex h-[1.625em] items-center"><Skeleton className="h-2 w-10 rounded-full" /></span>
          </div>
        </li>
      ))}
    </ol>
  )
}
