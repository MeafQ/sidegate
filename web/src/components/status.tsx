import type { ReactNode } from "react"
import { CircleAlertIcon, CircleCheckIcon, CircleDashedIcon, CircleIcon, CircleOffIcon, CircleXIcon, ClockIcon, HistoryIcon, type LucideIcon } from "lucide-react"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Fact, type Line } from "@/components/card"
import type { Rating } from "@/lib/health"
import type { Tone } from "@/lib/troubles"
import type { ServerView, SlotView } from "@/lib/schema"
import { msFigure } from "@/lib/format"
import { drawnServer, slotDot } from "@/lib/slots"
import { cn } from "@/lib/utils"

/* One vocabulary for every status: muted words, a mark carrying the colour. Nothing is tinted. */

export type Mark = "ok" | "warning" | "danger" | "working" | "waiting" | "queued" | "none" | "off"
const MARKS: Record<Mark, ReactNode> = {
  ok: <CircleCheckIcon className="text-success" aria-hidden />,
  warning: <CircleAlertIcon className="text-warning" aria-hidden />,
  danger: <CircleXIcon className="text-destructive" aria-hidden />,
  working: <Spinner />,
  waiting: <ClockIcon aria-hidden />,
  queued: <CircleDashedIcon aria-hidden />,
  none: <CircleIcon aria-hidden />,
  off: <CircleOffIcon aria-hidden />,
}

export function MarkIcon({ mark }: { mark: Mark }) {
  return <span className="inline-flex [&_svg]:size-3.5">{MARKS[mark]}</span>
}

const RATING_COLOUR = { good: "bg-success", unstable: "bg-warning", bad: "bg-destructive", none: "bg-muted-foreground/40" } as const satisfies Record<Rating, string>

/** One rating's dot. With a label it speaks for itself, and without one it is decoration. */
export function RatingMark({ rating, label, className }: { rating: Rating; label?: string; className?: string }) {
  const a11y = label ? { role: "img", "aria-label": label, title: label } : { "aria-hidden": true }
  return <span {...a11y} className={cn("size-2 shrink-0 rounded-full", RATING_COLOUR[rating], className)} />
}

const DOT: Record<SlotView["status"], string> = { serving: "bg-success", checking: "bg-muted-foreground/40", failed: "bg-destructive", block: "bg-muted-foreground", direct: "bg-muted-foreground" }

/** A slot's state as one dot. Colour alone can't say it, so its words are its label. Serving, it is the dot of the server its box
 * draws. */
export function SlotDot({ slot, servers }: { slot: SlotView; servers: Map<ServerView["id"], ServerView> }) {
  const drawn = drawnServer(slot)
  const { fill, words, pulse } = slotDot(slot, drawn ? servers.get(drawn) : undefined)
  const pulsing = pulse ? "motion-safe:animate-pulse" : undefined
  if (fill.kind === "server") return <RatingMark rating={fill.rating} label={words} className={pulsing} />
  const colour = fill.kind === "noClient" ? "bg-muted-foreground/40" : DOT[fill.status]
  return <span role="img" aria-label={words} title={words} className={cn("size-2 shrink-0 rounded-full", colour, pulsing)} />
}

/** The card's name for a server the box draws from the daemon's word alone, since no pass vouches for it now. */
export const LAST_KNOWN = "Last active server"

/** A quiet fact: muted text. */
export function Quiet({ children, className, title, "aria-label": label }: { children: ReactNode; className?: string; title?: string; "aria-label"?: string }) {
  return <span className={cn("text-muted-foreground", className)} title={title} aria-label={label}>{children}</span>
}

/** A trouble's colour, in the heading and the list alike. */
/** "25 / 35 ms" as a column draws it, with the p95 in `p95ClassName` so a narrow column can hide it. */
export function PingFigures({ p50, p95, p95ClassName }: { p50: number; p95: number; p95ClassName?: string }) {
  return <>{msFigure(p50)}<span className={p95ClassName}> / {msFigure(p95)}</span> ms</>
}

export const TONE_TEXT = { warn: "text-warning", bad: "text-destructive" } as const satisfies Record<Tone, string>

/** A trouble's words in its colour, with no mark beside them. */
export function Toned({ tone, children, className }: { tone: Tone; children: ReactNode; className?: string }) {
  return <span className={cn("whitespace-nowrap", TONE_TEXT[tone], className)}>{children}</span>
}

/** Waiting: a skeleton where a value will stand, or the value already known in muted words, and a spinner where its number goes.
 * Its sentence is its accessible name and its card, opened by hover, focus or a tap. Neither moves where motion is reduced. */
export function Pending({ words, value, className }: { words: string; value?: ReactNode; className?: string }) {
  return (
    <span role="status" className={cn("flex min-w-0 flex-1", className)}>
      <Fact label={words} lines={[{ words }]} className="min-h-6 min-w-0 flex-1 flex-row items-center gap-2">
        {value ? <Quiet className="min-w-0 truncate">{value}</Quiet> : <Skeleton data-slot="pending-value" className="h-2 w-3/5 rounded-full motion-reduce:animate-none" />}
        <Spinner role={undefined} aria-label={undefined} aria-hidden className="text-muted-foreground ml-auto size-3.5 motion-reduce:animate-none" />
      </Fact>
    </span>
  )
}

/** A muted sign that carries a quantity's kind: the refill's calendar, a reading's age, the next try's clock. */
export const Sign = ({ icon: Icon }: { icon: LucideIcon }) => <Icon aria-hidden className="text-muted-foreground size-3 shrink-0" />

/** A sign and its plain value. */
export function Signed({ icon, children, className }: { icon: LucideIcon; children?: ReactNode; className?: string }) {
  return <span className={cn("flex items-center gap-1 whitespace-nowrap", className)}><Sign icon={icon} />{children}</span>
}

/** A card's line for how old a reading is: the history sign and "3d ago". `words` is its sentence, "Measured 3d ago". */
export const ageLine = (words: string, ago: string): Line => ({ words, drawn: <Signed icon={HistoryIcon}>{ago}</Signed> })
