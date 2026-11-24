import { useRef, useState } from "react"
import { ArrowRightIcon, CircleAlertIcon, CircleArrowUpIcon, ExternalLinkIcon, RefreshCwIcon } from "lucide-react"
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Fact } from "@/components/card"
import { ButtonSpinner, Cross, FailedMark, RetryButton, useRead } from "@/components/page"
import { Quiet, ageLine } from "@/components/status"
import { updateControls } from "@/lib/api"
import { useNow } from "@/lib/clock"
import { ago } from "@/lib/format"
import { updateRead } from "@/lib/queries"
import { standingOf, type Ended, type Offer, type Standing } from "@/lib/update"
import { useCheck, useUpdate } from "@/lib/use-update"
import { cn } from "@/lib/utils"

/** A newer release is offered, on what leads to it: the picture Update's button has. */
export const UpdateMark = ({ version, className }: { version: string; className?: string }) =>
  <CircleArrowUpIcon role="img" aria-label={`Update to ${version}`} className={cn("text-primary size-3.5 shrink-0", className)} />

/** A version's place while what says it is on its way. */
export const VersionWaiting = () => <span className="flex h-[1.625em] items-center"><Skeleton className="h-2 w-12 rounded-full" /></span>

/** A version line, in ink what runs, and after the arrow the one known and not in effect, muted. `heard` parts the two for a screen
 * reader. */
export function VersionLine({ running, next, heard }: { running: string; next: string; heard: string }) {
  return (
    <span className="flex items-center gap-1.5 tabular-nums">
      <span><span className="sr-only">Running </span>{running}</span>
      <ArrowRightIcon aria-hidden className="text-muted-foreground size-3.5 shrink-0" />
      <Quiet><span className="sr-only">{heard}</span>{next}</Quiet>
    </span>
  )
}

/** The confirm of the offer it opened with, which is what its press sends. Its one line is there only when slots will drop. */
function Confirm({ offer, onUpdate, onClose, onClosed }: { offer: Offer; onUpdate: () => void; onClose: () => void; onClosed: () => void }) {
  return (
    <AlertDialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <AlertDialogContent onCloseAutoFocus={(e) => { e.preventDefault(); onClosed() }}>
        <AlertDialogHeader>
          <AlertDialogTitle>{`Update to ${offer.version}?`}</AlertDialogTitle>
          {offer.restartsCore && <AlertDialogDescription>All slots will disconnect briefly.</AlertDialogDescription>}
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction onClick={onUpdate}>Update</AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

/** How the last update ended, the cross that stands where Update would, or before it while the gateway has still to come back. Its
 * card is the words, with the fix there is: Retry, which asks again, or the guide to the recovery over SSH. */
function EndedMark({ ended, guide, onRetry }: { ended: Ended; guide: string | null; onRetry: (() => void) | null }) {
  const action = ended.kind === "recovery" && guide !== null
    ? <Button asChild size="icon-sm" variant="outline" className="shrink-0"><a href={guide} target="_blank" rel="noreferrer" aria-label="Open recovery guide" title="Open recovery guide"><ExternalLinkIcon /></a></Button>
    : onRetry && <RetryButton onClick={onRetry} />
  return <Cross what="Update" words={ended.words} action={action ?? undefined} className={cn("min-w-7 justify-center", ended.kind === "interrupted" && "text-warning")} />
}

/** Why the last check learned nothing, beside Check, whose press is its fix. */
function Unchecked({ words, at }: { words: string; at: string | null }) {
  const now = useNow()
  const age = at && ago(at, now)
  return (
    <Fact label={`Check for updates: ${[words, age].filter(Boolean).join(", ")}`} className="text-muted-foreground min-w-7 items-center justify-center"
      lines={[{ words, drawn: <Quiet>{words}</Quiet> }, ...(age ? [ageLine(age, age)] : [])]}>
      <CircleAlertIcon aria-hidden className="size-3.5" />
    </Fact>
  )
}

function Check() {
  const check = useCheck()
  return (
    <Button variant="ghost" size="icon-sm" className="text-muted-foreground hover:text-foreground" aria-label="Check for updates" title="Check for updates" disabled={check.out} aria-busy={check.out || undefined} onClick={check.press}>
      {check.out ? <ButtonSpinner /> : <RefreshCwIcon />}
    </Button>
  )
}

/** sidegate's line: the version that runs, the release offered over it with its notes, and what can be pressed. It is as tall in
 * every state. On a phone Update is a line of its own, full width. */
function UpdateLine({ standing }: { standing: Standing }) {
  const { version, to, ended, press, guide } = standing
  const update = useUpdate()
  const [asking, setAsking] = useState<Offer | null>(null)
  const line = useRef<HTMLDivElement>(null)
  const pending = update.out || (press?.kind === "update" && press.running)
  const offer = press?.kind === "update" || press?.kind === "retry" ? press.offer : null
  const ask = offer && (() => setAsking(offer))
  /* A press that is out stands in Update, whatever the last one left. */
  const drawn = press?.kind === "retry" && update.out ? "update" : press?.kind
  return (
    <div ref={line} data-slot="update-line" data-control={`${updateControls.update} ${updateControls.check}`} className="flex min-h-7 flex-wrap items-center gap-x-2 gap-y-1">
      <span className="flex min-h-7 items-center gap-1.5">
        {to ? <VersionLine running={version} next={to.version} heard=", newer " /> : <span className="tabular-nums">{version}</span>}
        {to?.notes && <a href={to.notes} target="_blank" rel="noreferrer" className="text-muted-foreground hover:text-foreground shrink-0" aria-label={`Notes for ${to.version}`} title={`Notes for ${to.version}`}><ExternalLinkIcon className="size-3.5" aria-hidden /></a>}
      </span>
      {press?.kind === "check" && <Check />}
      {ended && !pending && <EndedMark ended={ended} guide={guide} onRetry={drawn === "retry" ? ask : null} />}
      {drawn === "update" && (
        <Button variant="outline" size="sm" className="max-md:mb-1 max-md:h-11 max-md:w-full" disabled={pending || ask === null} aria-busy={pending || undefined} onClick={ask ?? undefined}>
          {pending ? <ButtonSpinner /> : <CircleArrowUpIcon />}Update
        </Button>
      )}
      {press?.kind === "check" && press.unchecked && <Unchecked {...press.unchecked} />}
      {asking && (
        <Confirm offer={asking} onUpdate={() => update.press(asking)} onClose={() => setAsking(null)}
          onClosed={() => { const buttons = line.current?.querySelectorAll("button"); buttons?.item(buttons.length - 1)?.focus() }} />
      )}
    </div>
  )
}

/** sidegate as About shows it. While it is read, the version's place; a failed read is the cross whose card has Retry. */
export function SidegateFact() {
  const update = useRead(updateRead)
  if (update.state === "read") return <UpdateLine standing={standingOf(update.data)} />
  return (
    <div className="flex min-h-7 items-center">
      {update.state === "failed" ? <FailedMark failed={update} what="Version" /> : <VersionWaiting />}
    </div>
  )
}
