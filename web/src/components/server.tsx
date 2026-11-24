import { Fact, type Line } from "@/components/card"
import { Flag } from "@/components/flag"
import { FailedMark, failureOf, type Read } from "@/components/page"
import { Skeleton } from "@/components/ui/skeleton"
import { Quiet, RatingMark } from "@/components/status"
import { RATING_WORDS, rate } from "@/lib/health"
import { BARS_NAMED, MOST_BARS, type Bars, type Stability } from "@/lib/failures"
import type { Dot } from "@/lib/troubles"
import type { LineageId, ServerView, SourceView } from "@/lib/schema"
import { cn } from "@/lib/utils"

/** The mark of a server a slot is on now, before its name, so the row in use stands out at any width. */
export function CarryingMark({ label }: { label: string }) {
  return <span role="img" aria-label={label} title={label} className="bg-brand size-1.5 shrink-0 rounded-full" />
}

/** A server as people read it: its provider's label, and the provider's name in muted words after it. Until the server `list` is
 * read it is Pending in its shape, a flag's place and its name's; once that read has failed, the muted cross stands in the name's
 * place, its card holding the reason and Retry. */
export function ServerName({ id, servers, sources, active = false, withProvider = true, list }: {
  id: LineageId; servers: Map<LineageId, ServerView>; sources: Pick<SourceView, "id" | "name">[]; active?: boolean; withProvider?: boolean; list?: Read<unknown>
}) {
  const server = servers.get(id)
  const failed = list && failureOf(list)
  if (!server && failed) return <FailedMark failed={failed} what="Server" className="relative h-5" />
  if (!server && list?.state === "waiting") return <span role="status" aria-label="Loading servers…" className="inline-flex"><ServerNameWaiting /></span>
  if (!server) return <Quiet>Unknown server</Quiet>
  const provider = sources.find((s) => s.id === server.source)
  return (
    <span className="inline-flex min-w-0 items-center gap-1.5">
      {active && <CarryingMark label="Carrying traffic" />}
      <Flag code={server.country.code} />
      <span className={cn("truncate", server.presence !== "present" && "text-muted-foreground line-through")}>{server.label}</span>
      {withProvider && provider && <span className="text-muted-foreground shrink-0">· {provider.name}</span>}
    </span>
  )
}

/** A server's name while the list that names it is on its way: a flag's place and its name's, in skeleton. */
export function ServerNameWaiting({ width = "w-28" }: { width?: string }) {
  return (
    <span aria-hidden className="inline-flex items-center gap-1.5">
      <Skeleton className="size-3.5 shrink-0 rounded-full" /><Skeleton className={cn("h-2 rounded-full", width)} />
    </span>
  )
}

/** A server's rating. A dormant one is greyed, and its tooltip says how old its last result is. */
export function RatingDot({ server }: { server: ServerView }) {
  const { rating, why } = rate(server)
  return (
    <span className={cn("flex shrink-0", !server.health.current && "opacity-50 grayscale")} role="img" aria-label={RATING_WORDS[rating]} title={[RATING_WORDS[rating], ...why].join(" · ")}>
      <RatingMark rating={rating} />
    </span>
  )
}

/** A group's or source's dot, with the same reasons on hover. A source whose first refresh runs has the faint Pending dot. */
export function GroupDot({ state }: { state: Dot }) {
  const words = state.kind === "first" ? "Refreshing…" : RATING_WORDS[state.rating]
  return (
    <span className="flex shrink-0" role="img" aria-label={words} title={[words, ...state.why].join(" · ")}>
      <RatingMark rating={state.rating} className={cn(state.kind === "first" && "motion-safe:animate-pulse")} />
    </span>
  )
}

const BAR_HEIGHT = ["h-[3px]", "h-1.5", "h-[9px]", "h-3"] as const

/** Four bars read like phone signal, in the text's colour, lit from the left: never red or amber, since the dot says whether a
 * server works now. */
export function StabilityBars({ bars }: { bars: Bars }) {
  return (
    <span aria-hidden data-slot="bars" data-lit={bars} className="inline-flex h-3 w-[18px] shrink-0 items-end gap-0.5">
      {BAR_HEIGHT.map((height, i) => <span key={height} className={cn("w-[3px] rounded-[1px] bg-current", height, i >= bars && "opacity-20")} />)}
    </span>
  )
}

/** The one Stability card, wherever it opens: the table's header and the Exit menu's. Three marks and their names. */
export const STABILITY_HEADER: Line[] = BARS_NAMED.map(([bars, name]) => ({
  words: `${bars} of ${MOST_BARS}: ${name}`,
  drawn: <span className="flex items-center gap-1.5 whitespace-nowrap"><StabilityBars bars={bars} />{name}</span>,
}))

export const cardLines = (stability: Stability): Line[] => stability.lines.map(({ name, value }) => ({ name, words: value }))

/** A server's bars, against the column's right edge. A tap or keyboard focus opens its card beside it, and a screen reader hears
 * its level and its card. Its tap area is 28px tall and takes no more of the row than the bars. */
export function StabilityCell({ stability, className }: { stability: Stability; className?: string }) {
  return (
    <Fact lines={cardLines(stability)} label={stability.heard} side="left" className={cn("-my-1.5 ml-auto w-fit flex-row items-center justify-end", className)}>
      <StabilityBars bars={stability.bars} />
    </Fact>
  )
}
