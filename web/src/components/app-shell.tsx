import { useEffect, useRef, useState, type ReactNode } from "react"
import { Link, Outlet, useLocation, useOutletContext } from "react-router"
import { useTheme } from "next-themes"
import { GlobeIcon, GlobeOffIcon, MenuIcon, MonitorIcon, MoonIcon, RouterIcon, SettingsIcon, SunIcon, UnplugIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Sheet, SheetContent, SheetDescription, SheetTitle } from "@/components/ui/sheet"
import { ActionCard, Fact, type Line } from "@/components/card"
import { NEW_GROUP, useGroupAdding } from "@/components/group-form"
import { Nav, useStandings, worstOf } from "@/components/nav"
import { useSourceAdding } from "@/components/source-details"
import { BLOCK_GAP } from "@/components/page"
import { RatingMark, TONE_TEXT, Toned } from "@/components/status"
import { UpdateMark } from "@/components/update"
import { clock } from "@/lib/format"
import { useLiveUpdates, useOffered, useView, type Connection } from "@/lib/queries"
import type { UnifiTrouble } from "@/lib/schema"
import { UNIFI_TROUBLES } from "@/lib/troubles"
import { useUpdateWatch } from "@/lib/use-update"
import { cn } from "@/lib/utils"

const WAN_STATES = {
  up: { words: "Up", icon: <GlobeIcon aria-hidden className="text-success" /> },
  down: { words: "Down", icon: <GlobeOffIcon aria-hidden className="text-destructive" /> },
  unknown: { words: "Unknown", icon: <GlobeIcon aria-hidden className="text-muted-foreground" /> },
} as const

/** What the page shows while it has lost the daemon: kept in place, at half strength. */
const STALE = "opacity-50"

/** A top bar mark: an icon with no words, whose card, the page's Card, says them. */
const TOP_MARK = "items-center justify-center [&_svg]:size-4"

export function WanStatus({ wan, stale = false }: { wan: keyof typeof WAN_STATES; stale?: boolean }) {
  const { words, icon } = WAN_STATES[wan]
  return <Fact label={`WAN ${words.toLowerCase()}`} lines={[{ name: "WAN", words }]} className={cn(TOP_MARK, "transition-opacity", stale && STALE)}>{icon}</Fact>
}

/** Nothing while connected. Disconnected, the unplug mark, whose card names since when. */
export function ConnectionMark({ connection }: { connection: Connection }) {
  if (connection.kind === "connected") return null
  const since = clock(connection.since)
  return (
    <Fact label={`Gateway unreachable since ${since}`} lines={[{ name: "Offline since", words: since }]} className={TOP_MARK}>
      <UnplugIcon aria-hidden className="text-warning" />
    </Fact>
  )
}

/** The id of Settings' UniFi part, where the UniFi mark's card leads. */
export const UNIFI_PART = "unifi"

/** UniFi's trouble, wherever it stands: a click opens its card, which names the trouble. A missing or refused key is fixed in
 * Settings, so its card holds Settings on its line, opening Settings at UniFi. UniFi not answering is fixed by nothing here, so its
 * card is a plain card of rows, as the daemon's mark's is: the trouble, and when UniFi last answered once it has since the daemon
 * started (`answered`). `disabled` is a control the trouble keeps from acting, as the board's Add box. */
export function UnifiCard({ trouble, answered, label, className, disabled, children }: { trouble: UnifiTrouble; answered: string | null; label: string; className?: string; disabled?: boolean; children: ReactNode }) {
  const { tone, words } = UNIFI_TROUBLES[trouble]
  const named: Line = { name: "UniFi", words, drawn: <Toned tone={tone}>{words}</Toned> }
  if (trouble === "notAnswering") {
    const last = answered && clock(answered)
    return (
      <Fact lines={last ? [named, { name: "Last answered", words: last }] : [named]} label={last ? `${label}. Last answered ${last}` : label} className={className} disabled={disabled}>
        {children}
      </Fact>
    )
  }
  return (
    <ActionCard label={label} lines={[named]} className={className} disabled={disabled}
      action={<Button asChild size="sm" variant="outline" className="shrink-0"><Link to={{ pathname: "/settings", hash: UNIFI_PART }}><SettingsIcon />Settings</Link></Button>}>
      {children}
    </ActionCard>
  )
}

/** Nothing while UniFi can make VPN Clients. In trouble, a router in the trouble's colour, whose card is UniFi's. */
export function UnifiMark({ trouble, answered, stale = false }: { trouble: UnifiTrouble | null; answered: string | null; stale?: boolean }) {
  if (trouble === null) return null
  const { tone, words } = UNIFI_TROUBLES[trouble]
  return (
    <UnifiCard trouble={trouble} answered={answered} label={`UniFi: ${words}`} className={cn(TOP_MARK, "transition-opacity", stale && STALE)}>
      <RouterIcon aria-hidden className={TONE_TEXT[tone]} />
    </UnifiCard>
  )
}

function ModeToggle() {
  const { theme, setTheme } = useTheme()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" size="icon" aria-label="Theme" className="relative shrink-0">
          <SunIcon className="scale-100 rotate-0 transition-all dark:scale-0 dark:-rotate-90" />
          <MoonIcon className="absolute scale-0 rotate-90 transition-all dark:scale-100 dark:rotate-0" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuRadioGroup value={theme} onValueChange={setTheme}>
          <DropdownMenuRadioItem value="light"><SunIcon />Light</DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="dark"><MoonIcon />Dark</DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="system"><MonitorIcon />System</DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

type Shell = { add: () => void; newGroup: ReturnType<typeof useGroupAdding> }
/** Opens the Add dialog, which the shell owns so the list and any page can start one. */
export const useAdd = () => useOutletContext<Shell>().add
/** Makes a group through the shell's one maker, for Save as group. */
export const useNewGroup = () => useOutletContext<Shell>().newGroup

const TITLES: [RegExp, string][] = [[/^\/$/, "Dashboard"], [/^\/servers$/, "All servers"], [/^\/sources\//, "Source"], [/^\/groups\//, "Group"], [/^\/events$/, "Events"], [/^\/settings$/, "Settings"]]

/** The list on the left, and the page it opens on the right. On a phone the list is a sheet. */
export function AppShell() {
  const { pathname, hash } = useLocation()
  const view = useView()
  const connection = useLiveUpdates()
  useUpdateWatch()
  const offered = useOffered()
  const stale = connection.kind === "disconnected"
  const adding = useSourceAdding()
  const newGroup = useGroupAdding()
  /* The sheet is open on the page it was opened over, so a page opened from it, a new group's draft too, is in sight. */
  const [menuAt, setMenuAt] = useState<string | null>(null)
  const menu = menuAt === pathname
  const setMenu = (open: boolean) => setMenuAt(open ? pathname : null)
  const scroller = useRef<HTMLDivElement>(null)
  /* A new page starts at its top, or at the part its address names. */
  useEffect(() => {
    const part = hash ? document.getElementById(hash.slice(1)) : null
    if (part) part.scrollIntoView({ block: "start" })
    else scroller.current?.scrollTo({ top: 0 })
  }, [pathname, hash])
  const { rows, standings } = useStandings()
  /* An address that names no source or group the view holds is no page, as an address the page doesn't have. */
  const named = !view.data || !/^\/(sources|groups)\//.test(pathname) || pathname === NEW_GROUP || rows.some((r) => r.to === pathname)
  const title = (named && TITLES.find(([path]) => path.test(pathname))?.[1]) || "Not found"
  const worst = worstOf(standings)
  const nav = (reveal: typeof worst) => <div className={cn("h-full transition-opacity", stale && STALE)}><Nav onAdd={adding.open} reveal={reveal} /></div>

  return (
    <div className="bg-sidebar text-sidebar-foreground flex h-svh">
      <aside className="w-64 shrink-0 max-md:hidden">{nav(null)}</aside>
      <Sheet open={menu} onOpenChange={setMenu}>
        {/* A page opened from it keeps the focus it takes, as a new group's name does. */}
        <SheetContent side="left" className="bg-sidebar w-72 p-0" onCloseAutoFocus={(e) => { if (menuAt !== null) e.preventDefault() }}>
          <SheetTitle className="sr-only">Menu</SheetTitle>
          <SheetDescription className="sr-only">Pages, sources and groups</SheetDescription>
          {/* Following a link closes the sheet, so the page it opened is in sight. */}
          <div className="contents" onClick={(e) => { if (e.target instanceof Element && e.target.closest("a")) setMenu(false) }}>{nav(worst)}</div>
        </SheetContent>
      </Sheet>
      <main data-slot="main" className="bg-background text-foreground flex min-w-0 flex-1 flex-col overflow-hidden md:m-2 md:ml-0 md:rounded-xl md:shadow-sm">
        <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4 lg:px-6">
          {/* Folded away, the list still says its worst dot, and opens where that dot first stands. A newer release's mark has the other
              corner, on a disc of the ground that cuts it out of the lines. */}
          <Button variant="ghost" size="icon" className="relative -ml-2 md:hidden" aria-label={offered ? `Menu, update to ${offered.version}` : "Menu"} onClick={() => setMenu(true)}>
            <MenuIcon />
            {offered && <span className="bg-background absolute right-0.5 bottom-0.5 flex size-3 items-center justify-center rounded-full"><UpdateMark version={offered.version} className="size-2.5" /></span>}
            {worst && <RatingMark rating={worst} className="absolute top-1 right-1 size-1.5" />}
          </Button>
          <h1 className="truncate text-base font-semibold">{title}</h1>
          <span className="ml-auto flex items-center gap-3">
            <ConnectionMark connection={connection} />
            {view.data && <UnifiMark trouble={view.data.unifiTrouble} answered={view.data.unifi.acceptedAt} stale={stale} />}
            {view.data && <WanStatus wan={view.data.wan} stale={stale} />}
            <ModeToggle />
          </span>
        </header>
        {/* While a toast is shown the content has room under it as tall as the toasts (`Toasts` sets --toast-room). */}
        <div ref={scroller} data-scroller className="@container/main flex min-h-0 flex-1 flex-col overflow-y-auto overscroll-contain">
          <div className={cn("mx-auto flex w-full max-w-7xl flex-1 flex-col px-4 pt-4 pb-[max(1rem,var(--toast-room,0px))] text-xs/relaxed transition-opacity md:pt-6 md:pb-[max(1.5rem,var(--toast-room,0px))] lg:px-6", BLOCK_GAP, stale && STALE)}>
            <Outlet context={{ add: adding.open, newGroup } satisfies Shell} />
          </div>
        </div>
      </main>
      {adding.dialog}
    </div>
  )
}
