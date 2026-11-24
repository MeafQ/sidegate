import { Fragment, useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { Link, useLocation, useNavigate } from "react-router"
import { ActivityIcon, ChevronDownIcon, LayersIcon, LayoutDashboardIcon, PlusIcon, ServerIcon, SettingsIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Item, ItemContent, ItemMedia, ItemTitle } from "@/components/ui/item"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { NEW_GROUP } from "@/components/group-form"
import { GroupDot } from "@/components/server"
import { FailedMark, failureOf, useRead } from "@/components/page"
import { useWeekRead } from "@/components/source-heading"
import { Quiet, RatingMark, TONE_TEXT } from "@/components/status"
import { UpdateMark } from "@/components/update"
import { useNow } from "@/lib/clock"
import type { Rating } from "@/lib/health"
import { groupState, noServers, standingOf, UNRATED, weekOf, type Dot, type Trouble } from "@/lib/troubles"
import { serversRead, stateRead, useOffered, useServerIndex, useView } from "@/lib/queries"
import type { AnalyticsBucket, Exit, GroupView, ServerView, SourceView } from "@/lib/schema"
import { cn } from "@/lib/utils"

/** What a slot can point at and holds servers: a whole subscription, or a set picked across them. */
export type Exitable =
  | { kind: "source"; id: string; to: string; name: string; source: SourceView; members: ServerView[]; resolved: ServerView["id"][] }
  | { kind: "group"; id: string; to: string; name: string; group: GroupView; members: ServerView[]; resolved: ServerView["id"][] }

/** The row an Exit stands on, by its id, when it is a source or a group. */
export const exitableId = (exit: Exit): string | null => (exit.kind === "source" ? `source:${exit.source}` : exit.kind === "group" ? `group:${exit.group}` : null)

/** Every source and group with the servers each holds now, as the list and the page both see them. */
export function useExitables() {
  const view = useView()
  const list = useRead(serversRead)
  const index = useServerIndex()
  const listed = list.state === "read" ? list.data : undefined
  const all = useMemo(() => (listed ?? []).filter((s) => s.presence !== "tombstone"), [listed])
  const rows = useMemo<Exitable[]>(() => [
    ...(view.data?.sources ?? []).map((source): Exitable => {
      const members = all.filter((s) => s.source === source.id)
      return { kind: "source", id: `source:${source.id}`, to: `/sources/${source.id}`, name: source.name, source, members, resolved: members.map((s) => s.id) }
    }),
    ...(view.data?.groups ?? []).map((group): Exitable => ({ kind: "group", id: `group:${group.id}`, to: `/groups/${group.id}`, name: group.name, group, resolved: group.resolved, members: group.resolved.flatMap((id) => { const s = index.get(id); return s ? [s] : [] }) })),
  ], [view.data, all, index])
  return { all, rows, index, list }
}

/** The mark, the accent's one fixed home. web/public/favicon.svg is the same drawing. */
export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" aria-hidden fill="currentColor" className={cn("text-brand", className)}>
      <path d="M10 4h12a6 6 0 0 1 6 6v4h-8a6 6 0 0 0-6 6v8h-4a6 6 0 0 1-6-6V10a6 6 0 0 1 6-6Z" />
      <path d="M20 20h8v2a6 6 0 0 1-6 6h-2Z" />
    </svg>
  )
}

/** A source's or group's dot and the troubles its entry names, as its heading draws them, from the same week's read. */
export type Standing = { dot: Dot; troubles: Trouble[] }

function standing(row: Exitable, servers: Map<ServerView["id"], ServerView> | null, week: { buckets: readonly AnalyticsBucket[]; at: number } | null, now: number): Standing {
  if (row.kind === "source") return standingOf(row.source, servers && row.members, week ? weekOf(week.buckets, row.source, week.at).failovers : null, now)
  const { rating, why, troubles } = groupState(row.group, servers)
  return { dot: { kind: "rated", rating, why }, troubles }
}

/** Every source's and group's standing, by its row's id, from one read of the week: the list, the Menu button and the Exit menu
 * draw this one result. */
export function useStandings() {
  const exitables = useExitables()
  const now = useNow()
  const week = useWeekRead().answer
  const { rows, index, list } = exitables
  /* An entry's servers are the server list's, so until it answers no source or group reads as having none. */
  const read = list.state === "read"
  const standings = useMemo(() => new Map(rows.map((row) => [row.id, standing(row, read ? index : null, week, now)])), [rows, index, read, week, now])
  return { ...exitables, standings }
}

/** The list's worst dot, red before amber, or null while nothing in it is in trouble. */
export function worstOf(standings: Map<string, Standing>): Extract<Rating, "bad" | "unstable"> | null {
  const ratings = [...standings.values()].map((s) => s.dot.rating)
  return ratings.includes("bad") ? "bad" : ratings.includes("unstable") ? "unstable" : null
}

/** An entry's count: a number, a spinner while it is still to come, or the muted dash once the read that brings it failed. */
type Count = number | "pending" | "failed"

/** One entry in the list. Under its name, only in trouble, its troubles in words in their colours with no mark, in the heading's
 * order. Each wraps whole, and a muted " ·" ends the line it parts. `aside` is a control in the count's place, outside the link,
 * as a failed read's mark is. `offered` is the release a newer one's mark names there. */
function ListItem({ to, label, count, media, troubles = [], at, rating, aside, offered }: { to: string; label: ReactNode; count?: Count; media?: ReactNode; troubles?: readonly Trouble[]; at: boolean; rating?: Rating; aside?: ReactNode; offered?: string }) {
  const item = (
    <Item asChild size="xs" data-rating={rating} className={cn("hover:bg-muted/60 scroll-mt-6", at && "bg-muted")}>
      <Link to={to} aria-current={at ? "page" : undefined}>
        {media && <ItemMedia className="h-[1.625em] self-start [&_svg:not([class*='size-'])]:size-3.5">{media}</ItemMedia>}
        <ItemContent className="min-w-0 gap-1">
          <ItemTitle className="w-full min-w-0 justify-between gap-2 font-normal">
            <span className="truncate">{label}</span>
            {aside ? <span aria-hidden className="size-3.5 shrink-0" /> : count === "pending" ? <CountSpinner /> : count !== undefined && <Quiet className="shrink-0 tabular-nums">{count === "failed" ? "—" : count}</Quiet>}
            {offered !== undefined && <UpdateMark version={offered} />}
          </ItemTitle>
          {troubles.length > 0 && (
            <span data-slot="troubles" className="text-muted-foreground">
              {troubles.map((t, i) => (
                <Fragment key={t.kind}>
                  {i > 0 && " "}
                  <span className="whitespace-nowrap"><span className={TONE_TEXT[t.tone]}>{t.words}</span>{i < troubles.length - 1 && " ·"}</span>
                </Fragment>
              ))}
            </span>
          )}
        </ItemContent>
      </Link>
    </Item>
  )
  if (!aside) return item
  return <div data-slot="card-row" className="relative">{item}<span className="absolute top-0.5 right-1 flex">{aside}</span></div>
}

const CountSpinner = () => <Spinner role={undefined} aria-label={undefined} aria-hidden className="text-muted-foreground size-3 shrink-0" />

/** The list while the daemon's view is on its way: All servers and the Sources heading drawn, entries in the shape they arrive in,
 * a faint dot and their name's place, and Add. Once that read has failed nothing waits: All servers' count is the muted dash, and
 * the page says why. */
function ListWaiting({ failed }: { failed: boolean }) {
  return (
    <>
      <ListItem to="/servers" at={false} label="All servers" count={failed ? "failed" : "pending"} media={<ServerIcon />} />
      {!failed && (
        <div className="flex flex-col gap-0.5">
          <Quiet className="px-2 pb-1">Sources</Quiet>
          {["w-20", "w-28", "w-16"].map((width) => (
            <div key={width} data-slot="list-waiting" className="flex h-[2.125rem] items-center gap-2 px-2">
              <RatingMark rating="none" />
              <Skeleton className={cn("h-2 rounded-full", width)} />
            </div>
          ))}
        </div>
      )}
      <Button variant="outline" className="w-full" disabled><PlusIcon />Add<ChevronDownIcon /></Button>
    </>
  )
}

/** How far the list's edge fades. */
const FADE_PX = 32

/** Fades the list's edge where more of it lies past, so no heading stands over nothing. `clear` scrolls a row out of the fade, so
 * a row whose card is open keeps its ink. */
function useFadedEdges() {
  const ref = useRef<HTMLDivElement>(null)
  const [edges, setEdges] = useState({ top: false, bottom: false })
  useEffect(() => {
    const viewport = ref.current?.querySelector<HTMLElement>("[data-slot=scroll-area-viewport]")
    if (!viewport) return
    const measure = () => {
      const { scrollTop, scrollHeight, clientHeight } = viewport
      setEdges({ top: scrollTop > 1, bottom: scrollHeight - scrollTop - clientHeight > 1 })
    }
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(viewport)
    if (viewport.firstElementChild) observer.observe(viewport.firstElementChild)
    viewport.addEventListener("scroll", measure, { passive: true })
    return () => { observer.disconnect(); viewport.removeEventListener("scroll", measure) }
  }, [])
  const top = edges.top ? `transparent, #000 ${FADE_PX}px` : "#000"
  const bottom = edges.bottom ? `#000 calc(100% - ${FADE_PX}px), transparent` : "#000"
  const clear = (row: Element) => {
    const viewport = ref.current?.querySelector<HTMLElement>("[data-slot=scroll-area-viewport]")
    if (!viewport) return
    const [r, v] = [row.getBoundingClientRect(), viewport.getBoundingClientRect()]
    const above = r.top - (v.top + FADE_PX)
    const below = r.bottom - (v.bottom - FADE_PX)
    if (above < 0) viewport.scrollTop += above
    else if (below > 0) viewport.scrollTop += below
  }
  return { ref, mask: `linear-gradient(to bottom, ${top}, ${bottom})`, clear }
}

/** The whole app's list: the dashboard, then every source and group, then the pages about the daemon itself. `reveal` opens it at
 * the first entry whose dot has that colour, as the Menu button's dot promises. */
export function Nav({ onAdd, reveal = null }: { onAdd: () => void; reveal?: Rating | null }) {
  const { pathname } = useLocation()
  const navigate = useNavigate()
  const view = useView()
  const offered = useOffered()
  const viewFailed = useRead(stateRead).state === "failed"
  const { all, rows, standings, list: servers } = useStandings()
  const listFailed = failureOf(servers)
  /* Until the server list lands every count waits alike, and once its read failed none does. */
  const listed = (n: number): Count => (servers.state === "read" ? n : servers.state === "failed" ? "failed" : "pending")
  /* Opened only once the menu has closed, since an open menu holds focus and the new group's name needs it. */
  const draftGroup = useRef(false)
  const { ref: list, mask, clear } = useFadedEdges()
  const read = view.data !== undefined
  /* An entry's dot is its own once the server list is read. Until then, and while that read has failed, the list opens at its
   * top, where All servers says why every count is a dash. */
  const revealed = servers.state === "read" ? reveal : null
  useEffect(() => {
    if (revealed && read) list.current?.querySelector(`[data-rating=${revealed}]`)?.scrollIntoView({ block: "start" })
  }, [revealed, read, list])
  const sources = rows.filter((r) => r.kind === "source")
  const groups = rows.filter((r) => r.kind === "group")
  const section = (title: string, list: Exitable[]) => list.length > 0 && (
    <div className="flex flex-col gap-0.5">
      <Quiet className="px-2 pb-1">{title}</Quiet>
      {list.map((row) => {
        const { dot, troubles } = standings.get(row.id) ?? { dot: UNRATED, troubles: [] }
        /* "No servers" already says the count. */
        const count = dot.kind === "first" && servers.state !== "failed" ? "pending" : noServers(troubles) ? undefined : listed(row.members.length)
        return <ListItem key={row.id} to={row.to} at={pathname === row.to} label={row.name} count={count} media={<GroupDot state={dot} />} troubles={troubles} rating={dot.rating} />
      })}
    </div>
  )
  return (
    <nav aria-label="Main" className="flex h-full min-h-0 flex-col gap-3 p-2 text-xs/relaxed">
      <Link to="/" className="flex items-center gap-2 px-2 pt-1.5 pb-1">
        <Logo className="size-7 shrink-0" />
        <span className="truncate text-base font-semibold">sidegate</span>
      </Link>
      <ListItem to="/" at={pathname === "/"} label="Dashboard" media={<LayoutDashboardIcon />} />
      {/* The scrollbar floats over the list and takes no room, so a row here is as wide as Dashboard. The margin and padding leave
          room for a focused entry's ring, and the block display lets a long name be cut where the scroller's table would grow. */}
      <ScrollArea ref={list} style={{ ["--edges" as string]: mask }} className="-mx-2 -my-1 min-h-0 flex-1 [&_[data-slot=scroll-area-viewport]]:[mask-image:var(--edges)] [&_[data-slot=scroll-area-viewport]>div]:block!">
        <div className="flex flex-col gap-3 px-2 py-1" onClick={(e) => { const row = e.target instanceof Element && e.target.closest("[data-slot=card-row]"); if (row) clear(row) }}>
          {!view.data ? <ListWaiting failed={viewFailed} /> : (
            <>
              {/* The server list's failed read stands on All servers too, since a phone's sheet covers the page that says it. */}
              <ListItem to="/servers" at={pathname === "/servers"} label="All servers" count={listed(all.length)} media={<ServerIcon />}
                aside={listFailed && <FailedMark failed={listFailed} what="All servers" className="px-1.5" />} />
              {section("Sources", sources)}
              {section("Groups", groups)}
              {/* A new group's draft has no entry of its own until it is saved, so Add stands for it. */}
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="outline" aria-current={pathname === NEW_GROUP ? "page" : undefined} className={cn("w-full", pathname === NEW_GROUP && "border-transparent bg-muted dark:bg-muted")}><PlusIcon />Add<ChevronDownIcon /></Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="start" onCloseAutoFocus={(e) => { if (draftGroup.current) { e.preventDefault(); navigate(NEW_GROUP) } draftGroup.current = false }}>
                  <DropdownMenuItem onSelect={onAdd}><PlusIcon />Source</DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => { draftGroup.current = true }}><LayersIcon />Group</DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </>
          )}
        </div>
      </ScrollArea>
      <div className="flex flex-col gap-0.5">
        <ListItem to="/events" at={pathname === "/events"} label="Events" media={<ActivityIcon />} />
        <ListItem to="/settings" at={pathname === "/settings"} label="Settings" media={<SettingsIcon />} offered={offered?.version} />
      </div>
    </nav>
  )
}
