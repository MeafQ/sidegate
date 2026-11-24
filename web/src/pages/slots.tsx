import { Link } from "react-router"
import { ListChecksIcon, PlusIcon, RssIcon, SettingsIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { UNIFI_PART, useAdd } from "@/components/app-shell"
import { EmptyState, Loaded } from "@/components/page"
import { Board, BoardWaiting } from "@/components/slot-board"
import { stateRead } from "@/lib/queries"

/** The setup's two steps, each with the control that does it, while UniFi has no key and there is no source. */
function GetStarted({ add }: { add: () => void }) {
  return (
    <EmptyState icon={<ListChecksIcon />} title="Get started">
      <ol className="flex w-full max-w-72 flex-col gap-2 text-left">
        <li className="flex items-center justify-between gap-3">
          <span>1. Add your UniFi API key</span>
          <Button asChild variant="outline"><Link to={{ pathname: "/settings", hash: UNIFI_PART }}><SettingsIcon />Settings</Link></Button>
        </li>
        <li className="flex items-center justify-between gap-3">
          <span>2. Add a proxy source</span>
          <Button onClick={add}><PlusIcon />Add source</Button>
        </li>
      </ol>
    </EmptyState>
  )
}

/** What's connected to what, and whether it works. */
export function SlotsPage() {
  const add = useAdd()
  return (
    <div className="flex flex-col gap-4">
      <Loaded query={stateRead} skeleton={<BoardWaiting />}>
        {(data) => {
          if (data.sources.length > 0 || data.slots.length > 0) return <Board data={data} />
          if (data.unifiTrouble === "noKey") return <GetStarted add={add} />
          return <EmptyState icon={<RssIcon />} title="No sources"><Button onClick={add}><PlusIcon />Add source</Button></EmptyState>
        }}
      </Loaded>
    </div>
  )
}
