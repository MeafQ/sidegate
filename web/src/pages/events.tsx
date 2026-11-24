import { useState } from "react"
import { ActivityIcon } from "lucide-react"
import { Card, CardContent } from "@/components/ui/card"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { EmptyState, Loaded, NoMatch, Toolbar, useRead } from "@/components/page"
import { ReasonMark, SwitchTimeline, TimelineWaiting } from "@/components/switch-timeline"
import { REASON_WORDS } from "@/lib/format"
import { serversRead, switchesRead, useReplaced, useServerIndex, useSwitches, useView } from "@/lib/queries"
import type { SwitchReason } from "@/lib/schema"

const ALL = "all"

export function EventsPage() {
  const switches = useSwitches()
  const replaced = useReplaced().data ?? []
  const view = useView()
  const servers = useServerIndex()
  /* Once the server list's read has failed, each server a move names is its failed mark, never "Unknown server". */
  const list = useRead(serversRead)
  const [slot, setSlot] = useState(ALL)
  const [reason, setReason] = useState(ALL)
  const slots = view.data?.slots ?? []
  /* With nothing to filter the selects leave. */
  const none = switches.data?.length === 0 && replaced.length === 0
  return (
    <>
      {!none && (
        <Toolbar>
          <Select value={slot} onValueChange={setSlot}>
            <SelectTrigger size="sm" aria-label="Slot" className="min-w-28"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>Slot: all</SelectItem>
              {slots.map((s) => <SelectItem key={s.id} value={s.id}>{s.name}</SelectItem>)}
            </SelectContent>
          </Select>
          <Select value={reason} onValueChange={setReason}>
            <SelectTrigger size="sm" aria-label="Reason" className="min-w-28"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>Reason: all</SelectItem>
              {(Object.entries(REASON_WORDS) as [SwitchReason, string][]).map(([k, words]) => <SelectItem key={k} value={k}><ReasonMark reason={k} />{words}</SelectItem>)}
            </SelectContent>
          </Select>
        </Toolbar>
      )}
      <Loaded query={switchesRead} skeleton={<Card><CardContent><TimelineWaiting /></CardContent></Card>}>
        {(events) => {
          if (none) return <EmptyState icon={<ActivityIcon />} title="No events yet" />
          const shown = events.filter((e) => (slot === ALL || e.slots.some((id) => id === slot)) && (reason === ALL || e.reason === reason))
          /* A replaced server has no slot and no reason, so any filter hides it. */
          const shownReplaced = slot === ALL && reason === ALL ? replaced : []
          return (
            <Card>
              <CardContent>
                {shown.length + shownReplaced.length
                  ? <SwitchTimeline key={`${slot}/${reason}`} events={shown} replaced={shownReplaced} names={{ servers, sources: view.data?.sources ?? [], list }} slots={slots} limit={25} />
                  : <NoMatch onClear={() => { setSlot(ALL); setReason(ALL) }} />}
              </CardContent>
            </Card>
          )
        }}
      </Loaded>
    </>
  )
}
