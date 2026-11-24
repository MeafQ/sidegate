import { useEffect, useId, type ReactNode } from "react"
import { hashKey, QueryCache, useQuery, useQueryClient, type QueryKey, type UseQueryOptions } from "@tanstack/react-query"
import { CircleXIcon, RotateCcwIcon, XIcon } from "lucide-react"
import { Alert } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Spinner } from "@/components/ui/spinner"
import { ActionCard, Fact } from "@/components/card"
import { Quiet } from "@/components/status"
import { ApiError } from "@/lib/api"
import { cn } from "@/lib/utils"

/** The space between blocks on every page. Rows in a list and a toolbar above its table sit closer. */
export const BLOCK_GAP = "gap-4 md:gap-6"

/** Save and Discard for a tab's rule, at the end of its filter line, while it differs from what is stored. Nothing stands beside
 * them: a refused filter says why in its own editor. */
export function SaveActions({ open, busy, held, canSave, onDiscard, onSave }: {
  open: boolean
  /** Its Save is out: Save spins. */
  busy: boolean
  /** A write of what it saves is out, its Save's or another way's: Discard takes no press. */
  held: boolean
  canSave: boolean
  /** Absent while there is nothing to put back. */
  onDiscard?: () => void
  onSave: () => void
}) {
  if (!open) return null
  return (
    <div role="group" aria-label="Unsaved changes" className="ml-auto flex shrink-0 gap-1.5 whitespace-nowrap">
      {onDiscard && <Button variant="outline" disabled={held} onClick={onDiscard}>Discard</Button>}
      <Button disabled={!canSave} aria-busy={busy || undefined} onClick={onSave}>{busy && <ButtonSpinner />}Save</Button>
    </div>
  )
}

/** A list page's first row: its filters, exactly as tall as their controls. */
export function Toolbar({ children }: { children?: ReactNode }) {
  return <div className="flex min-h-7 min-w-0 flex-wrap items-start gap-2">{children}</div>
}

/** What a read that failed says, in the fewest words and the same words for every read: "Timeout" where no answer came, "HTTP 502"
 * for the status the answer had, and for an answer the page can't read, that the page is out of date. Anything else is a bug in the
 * page: the reader gets a word and the console the error. */
export function failedWords(error: unknown): string {
  if (!(error instanceof ApiError)) return "Failed to load"
  if (error.kind === "mismatch") return "Page out of date"
  return error.kind === "network" ? "Timeout" : `HTTP ${error.status}`
}

/** Which place's Retry was pressed for a failed read, by the error it answers, so one read's other Retries know it wasn't theirs. */
const pressedAt = new WeakMap<object, string>()

/** Retry for a read: neutral, and Pending while the retry is out. A read whose failure stands in more than one place has a Retry
 * in each; the one pressed spins and the others wait at half strength, so one press shows one spinner. `place` names where it
 * stands. An answer that doesn't fit the page's schema means the page is older or newer than the daemon, so its one fix is
 * Reload, which fetches the page the daemon serves. */
export function RetryRead({ error, retry, retrying, place }: { error: unknown; retry: () => void; retrying: boolean; place: string }) {
  const stale = error instanceof ApiError && error.kind === "mismatch"
  const key = typeof error === "object" && error !== null ? error : null
  const pressed = key && pressedAt.get(key)
  const spinning = retrying && (!pressed || pressed === place)
  const press = () => { if (key) pressedAt.set(key, place); retry() }
  return (
    <Button size="sm" variant="outline" className="shrink-0" disabled={retrying} aria-busy={spinning || undefined} onClick={stale ? () => window.location.reload() : press}>
      {spinning ? <ButtonSpinner /> : <RotateCcwIcon />}{stale ? "Reload" : "Retry"}
    </Button>
  )
}

/** A read that failed, where its value would have stood: the muted mark, the reason muted, and Retry. No red, since red and amber
 * belong to dots, bars and the week. A page's read is a muted row of its own; in a `row` of something drawn, as a table's line,
 * Retry stands at the row's right end; inside a `frame` that is kept, as a chart's, the three stand on one centred line. */
export function ReadFailed({ error, retry, retrying, place = "page" }: { error: unknown; retry: () => void; retrying: boolean; place?: "page" | "row" | "frame" }) {
  const here = useId()
  const known = error instanceof ApiError
  useEffect(() => { if (!known) console.error(error) }, [error, known])
  const words = failedWords(error)
  if (place !== "page") {
    return (
      <span role="alert" className={cn("text-muted-foreground flex items-center gap-2", place === "row" && "w-full")}>
        <CircleXIcon aria-hidden className="size-3.5 shrink-0" /><span className={cn(place === "row" && "min-w-0 flex-1")}>{words}</span><RetryRead error={error} retry={retry} retrying={retrying} place={here} />
      </span>
    )
  }
  return (
    <Alert className="bg-muted/50 text-muted-foreground flex min-h-10 items-center gap-2 py-1.5 pr-1.5 *:[svg]:translate-y-0">
      <CircleXIcon aria-hidden className="size-3.5 shrink-0" />
      <span className="min-w-0 flex-1">{words}</span>
      <RetryRead error={error} retry={retry} retrying={retrying} place={here} />
    </Alert>
  )
}

/** The spinner inside a Pending button, whose own name is what a reader hears. */
export const ButtonSpinner = () => <Spinner role={undefined} aria-label={undefined} aria-hidden />

/** Retry that asks before it acts, so it is never Pending itself. */
export const RetryButton = ({ onClick }: { onClick: () => void }) => <Button size="sm" variant="outline" className="shrink-0" onClick={onClick}><RotateCcwIcon />Retry</Button>

/** What failed where its place is one fact wide: the muted cross there, a button whose card, opened on a click, is one line, the
 * reason muted, with its fix, `action`, on it. With no fix on the page it is a plain card, opened as any fact's. What names the place, as
 * the week's "7d", stands before the cross, and `markClassName` gives the cross the room its value would have taken, so nothing
 * moves. */
export function Cross({ what, words, action, className, markClassName, children }: { what: string; words: string; action?: ReactNode; className?: string; markClassName?: string; children?: ReactNode }) {
  const shared = { lines: [{ words, drawn: <Quiet>{words}</Quiet> }], label: `${what}: ${words}`, className: cn("text-muted-foreground items-center", className) }
  const mark = <>{children}<span className={cn("flex items-center", markClassName)}><CircleXIcon aria-hidden className="size-3.5" /></span></>
  return action === undefined ? <Fact {...shared} className={cn("justify-center", shared.className)}>{mark}</Fact> : <ActionCard {...shared} action={action}>{mark}</ActionCard>
}

/** A read that failed, as its place's cross, whose fix is Retry. */
export function FailedMark({ failed, what, ...place }: { failed: FailedRead; what: string; className?: string; markClassName?: string; children?: ReactNode }) {
  const here = useId()
  return <Cross what={what} words={failedWords(failed.error)} action={<RetryRead {...failed} place={here} />} {...place} />
}

/** A read that failed, with its reason, and whether its Retry is out. */
export type FailedRead = { error: unknown; retry: () => void; retrying: boolean }

/** A read as the page shows it: waiting for its first answer; failed with nothing to show, a retry of it included; or read, with
 * `stale` the failure of a later read while what it shows is older. */
export type Read<T> = { state: "waiting" } | ({ state: "failed" } & FailedRead) | { state: "read"; data: T; stale: FailedRead | null }

/** Each cached query's last failure, as the cache settles it, whoever reads it then or none. A retry puts a query with nothing to
 * show back to pending and drops its error, so the reason is kept here, with the query and not with one place that shows it: while
 * the retry is out every place shows the failed form, its Retry Pending, a place that mounts meanwhile too. A read that lands drops
 * it, and a new key is a new query. */
const lastFailure = new WeakMap<object, unknown>()

/** The page's query cache, which keeps each query's last failure. */
export const pageQueryCache = () => new QueryCache({
  onError: (error, query) => { lastFailure.set(query, error) },
  onSuccess: (_, query) => { lastFailure.delete(query) },
})

export function useRead<T, K extends QueryKey>(options: UseQueryOptions<T, Error, T, K>): Read<T> {
  const query = useQuery(options)
  const cached = useQueryClient().getQueryCache().get(hashKey(options.queryKey))
  const retry = () => void query.refetch()
  const failed = query.isError ? { error: query.error, retry, retrying: query.isFetching } : null
  if (query.data !== undefined) return { state: "read", data: query.data, stale: failed }
  if (failed) return { state: "failed", ...failed }
  const last = cached && lastFailure.get(cached)
  if (last !== undefined && query.isFetching) return { state: "failed", error: last, retry, retrying: true }
  return { state: "waiting" }
}

/** The failure a read shows: its own with nothing to show, or a later read's over what it shows. */
export const failureOf = (read: Read<unknown>): FailedRead | null => (read.state === "failed" ? read : read.state === "read" ? read.stale : null)

/** A query's three states: its waiting shape while it loads, the failed read where it would stand, then its content. */
export function Loaded<T, K extends QueryKey>({ query, skeleton, children }: { query: UseQueryOptions<T, Error, T, K>; skeleton: ReactNode; children: (data: T) => ReactNode }) {
  const read = useRead(query)
  if (read.state === "read") return children(read.data)
  if (read.state === "failed") return <ReadFailed {...read} />
  return skeleton
}

/** An empty list, one compact block at the top of where its rows go: a mark, one word, and the one action that fills it. */
export function EmptyState({ icon, title, children }: { icon?: ReactNode; title?: string; children?: ReactNode }) {
  return (
    <Empty className="flex-none gap-3 border border-dashed p-6">
      {(icon || title) && (
        <EmptyHeader>
          {icon && <EmptyMedia className="text-muted-foreground mb-0">{icon}</EmptyMedia>}
          {title && <EmptyTitle className="text-xs/relaxed font-normal">{title}</EmptyTitle>}
        </EmptyHeader>
      )}
      {children && <EmptyContent>{children}</EmptyContent>}
    </Empty>
  )
}

/** A list emptied by a search: muted "No match", and Clear, which empties the search alone. Inside a popover it has no button,
 * since the search's own × empties it. */
export function NoMatch({ onClear }: { onClear?: () => void }) {
  return (
    <span className="flex min-h-7 w-full items-center justify-between gap-2">
      <Quiet>No match</Quiet>
      {onClear && <Button size="sm" variant="ghost" onClick={onClear}><XIcon />Clear</Button>}
    </span>
  )
}
