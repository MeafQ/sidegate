import { ChevronsUpDownIcon, LayersIcon, ServerIcon } from "lucide-react"
import { AlertDialog, AlertDialogCancel, AlertDialogContent, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog"
import { Button, buttonVariants } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { ExitMenu } from "@/components/exit-menu"
import { ReadFailed, failureOf, useRead, type FailedRead, type Read } from "@/components/page"
import { useSlotWrite } from "@/components/slot-board"
import { Quiet, RatingMark, Signed, SlotDot } from "@/components/status"
import { actions, type ItemWay } from "@/lib/api"
import { stateRead } from "@/lib/queries"
import type { GroupView, ServerView, SlotView, SourceView, View } from "@/lib/schema"
import { isPending, useControl } from "@/lib/use-write"
import type { Shown } from "@/lib/write"
import { cn } from "@/lib/utils"

/** What a delete is of: a source or a group, as the view has it now. */
export type Doomed = { readonly kind: "source"; readonly source: SourceView } | { readonly kind: "group"; readonly group: GroupView }

/** A slot that would refuse the delete, with its own Exit select, so the reader repoints it here. The pick is the slot's own
 * write: while it is out the spinner stands in the place every row keeps at its end, so no select moves, and the select takes no other; once the view has the slot elsewhere
 * the daemon no longer lists it, and the row leaves. */
function Blocker({ slot, view, servers, list }: { slot: SlotView; view: View; servers: Map<ServerView["id"], ServerView>; list: Read<unknown> }) {
  const write = useSlotWrite(slot, servers)
  return (
    <li className="flex h-8 items-center gap-2">
      <SlotDot slot={slot} servers={servers} />
      <span className="min-w-0 flex-1 truncate">{slot.name}</span>
      <ExitMenu value={write.shown.exit} onChange={write.pick} busy={write.pending} groups={view.groups} servers={servers} sources={view.sources} list={list}
        label={`Exit of ${slot.name}`} className="w-40 shrink-0" />
      <span data-slot="repoint-place" className="flex size-3.5 shrink-0">{write.pending && <Spinner aria-label={`Repointing ${slot.name}`} className="text-muted-foreground size-3.5" />}</span>
    </li>
  )
}

/** A blocker the page is still reading: the faint dot, the name's place, and the select's frame and chevron around its value's. */
function BlockerWaiting() {
  return (
    <li className="flex h-8 items-center gap-2">
      <RatingMark rating="none" />
      <Skeleton className="h-2 w-16 rounded-full" />
      <span aria-hidden className={cn(buttonVariants({ variant: "outline" }), "pointer-events-none ml-auto w-40 justify-between font-normal")}>
        <Skeleton className="h-2 w-24 rounded-full" />
        <ChevronsUpDownIcon className="text-muted-foreground opacity-50" />
      </span>
      <span aria-hidden className="size-3.5 shrink-0" />
    </li>
  )
}

/** This dialog's Delete, apart from the item's other ways in. `out`: its press is out, and nothing closes the dialog. `reading`: the
 * daemon refused it as referenced, and the view is read to the revision it refused at, to show the slot that uses the item;
 * `failed`: that read failed. `idle`: none of these. */
type Deleting = { readonly kind: "idle" | "out" | "reading" } | { readonly kind: "failed"; readonly failed: FailedRead }

const deletingOf = (remove: Pick<Shown<void, unknown, ItemWay>, "phase" | "way" | "refusal">, failed: FailedRead | null): Deleting => {
  if (!isPending(remove.phase) || remove.way !== "delete") return { kind: "idle" }
  if (remove.refusal?.code !== "referenced") return { kind: "out" }
  return failed ? { kind: "failed", failed } : { kind: "reading" }
}

/** Rule 9's delete: every slot the daemon would refuse it for is a row under a muted "Used by", with its Exit there to repoint;
 * Delete enables once none is left. A source's delete says under a muted "Removes" what goes with it: its servers as a count,
 * and the groups by name. Delete is the source's or group's own write, Pending on its button, and the dialog stays until it is
 * answered; a refusal for a slot that came to use it in the meantime is that slot's row, with no toast. The write stays Pending
 * on that refusal until the view reaches the revision the daemon refused at, and until then the slot isn't in the view: its row
 * is a skeleton while the view is read, or the read that failed with its Retry. */
export function DeleteDialog({ doomed, view, servers, list, onClose }: {
  doomed: Doomed
  view: View
  servers: Map<ServerView["id"], ServerView>
  /** The server list's read, which names the servers a blocker's Exit menu lists. */
  list: Read<unknown>
  onClose: () => void
}) {
  const source = doomed.kind === "source" ? doomed.source : null
  const name = source?.name ?? (doomed.kind === "group" ? doomed.group.name : "")
  const remove = useControl(doomed.kind === "source" ? actions.deleteSource(doomed.source.id) : actions.deleteGroup(doomed.group.id), "delete")
  const usedBy = doomed.kind === "source" ? doomed.source.usedBy : doomed.group.usedBy
  const blockers = usedBy.flatMap((id) => view.slots.filter((s) => s.id === id))
  const groups = source ? view.groups.filter((g) => source.removes.includes(g.id)) : []
  const failed = failureOf(useRead(stateRead))
  /* Delete is one way into the source's or group's one write, beside its rename and Save: only its own press spins here. */
  const deleting = deletingOf(remove, failed)
  const unread = deleting.kind === "reading" || deleting.kind === "failed"
  const busy = deleting.kind === "out" || deleting.kind === "reading"
  const press = () => remove.press(undefined, { what: name, shows: (refusal) => refusal.code === "referenced" })
  return (
    <AlertDialog open onOpenChange={(open) => { if (!open && deleting.kind !== "out") onClose() }}>
      <AlertDialogContent>
        <AlertDialogHeader className="place-items-start text-left">
          <AlertDialogTitle>Delete {name}?</AlertDialogTitle>
        </AlertDialogHeader>
        {(blockers.length > 0 || unread) && (
          <section aria-label="Used by" className="flex flex-col gap-1 text-xs/relaxed">
            <Quiet>Used by</Quiet>
            <ul className="flex flex-col">
              {blockers.map((slot) => <Blocker key={slot.id} slot={slot} view={view} servers={servers} list={list} />)}
              {deleting.kind === "failed" ? <li className="flex h-8 items-center"><ReadFailed {...deleting.failed} place="row" /></li> : deleting.kind === "reading" && <BlockerWaiting />}
            </ul>
          </section>
        )}
        {blockers.length === 0 && !unread && source && (
          <section aria-label="Removes" className="flex flex-col gap-1 text-xs/relaxed">
            <Quiet>Removes</Quiet>
            <span className="flex flex-wrap items-center gap-x-4 gap-y-1">
              <Signed icon={ServerIcon}><span aria-label={`${source.stored} servers`}>{source.stored}</span></Signed>
              {groups.map((g) => <Signed key={g.id} icon={LayersIcon}>{g.name}</Signed>)}
            </span>
          </section>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={deleting.kind === "out"}>Cancel</AlertDialogCancel>
          <Button variant="destructive" disabled={blockers.length > 0 || isPending(remove.phase)} aria-busy={busy || undefined} onClick={press}>
            {busy && <Spinner role={undefined} aria-label={undefined} aria-hidden />}Delete
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
