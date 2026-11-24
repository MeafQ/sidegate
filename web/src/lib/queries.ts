import { useEffect, useMemo, useState } from "react"
import { hashKey, queryOptions, useQuery, useQueryClient, type QueryKey } from "@tanstack/react-query"
import { z } from "zod"
import { api, read, type AnalyticsQuery, type LineageId, type ServerView } from "@/lib/api"
import { keys } from "@/lib/keys"
import { AnalyticsReport, RefreshEvent, ReplacedEvent, ServerPage, Session, SettingsView, SwitchEvent, UpdateView, View, type Members, type SourceId } from "@/lib/schema"

/** Which reads a change makes stale. An unknown kind, or a resync, refreshes everything. */
const STALE_BY_KIND: Record<string, QueryKey[]> = {
  slot: [keys.state],
  group: [keys.state],
  switch: [keys.switches, keys.state],
  source: [keys.state, keys.servers, keys.anyRefreshes],
  catalogue: [keys.servers, keys.replaced],
  settings: [keys.settings],
  /* New measurements: charts, and the Ping and health each server shows. */
  analytics: [["analytics"], keys.servers],
}

/** Whether what the page shows is the daemon's now: connected, or disconnected since the feed first failed. A
 * disconnected page knows whether the feed is back, and is connected again once a read of the state lands after that. */
export type Connection = { kind: "connected" } | { kind: "disconnected"; since: string; streamBack: boolean }

/** What moves the connection: the feed lost at a moment, the feed back, and a read of the state that landed. */
type FeedEvent = { kind: "lost"; at: string } | { kind: "back" } | { kind: "read" }

function connectionAfter(c: Connection, e: FeedEvent): Connection {
  switch (e.kind) {
    case "lost": return { kind: "disconnected", since: c.kind === "disconnected" ? c.since : e.at, streamBack: false }
    case "back": return c.kind === "disconnected" ? { ...c, streamBack: true } : c
    case "read": return c.kind === "disconnected" && c.streamBack ? { kind: "connected" } : c
  }
}

const STATE_HASH = hashKey(keys.state)

/** Keeps every page current from the daemon's event feed; a quiet gateway sends nothing. After a reconnect every
 * read is fetched again, since changes may have gone by unseen. A read still out from before is cancelled first: a refetch of a
 * query with nothing cached joins the request already out, whose answer may be older than the reconnect. */
export function useLiveUpdates(): Connection {
  const qc = useQueryClient()
  const [connection, setConnection] = useState<Connection>({ kind: "connected" })
  useEffect(() => {
    const step = (e: FeedEvent) => setConnection((c) => connectionAfter(c, e))
    /* Only a read counts: a write's answer patched into the state leaves the rest of it as old as it was. */
    const unwatch = qc.getQueryCache().subscribe((event) => {
      if (event.type !== "updated" || event.action.type !== "success" || event.action.manual || event.query.queryHash !== STATE_HASH) return
      step({ kind: "read" })
    })
    const unsubscribe = api.events({
      change: (event) => {
        const stale = event.resync ? undefined : STALE_BY_KIND[event.kind]
        if (!stale) void qc.invalidateQueries()
        else stale.forEach((queryKey) => void qc.invalidateQueries({ queryKey }))
      },
      lost: () => step({ kind: "lost", at: new Date().toISOString() }),
      resync: () => {
        step({ kind: "back" })
        void qc.cancelQueries().then(() => qc.invalidateQueries())
      },
    })
    return () => { unwatch(); unsubscribe() }
  }, [qc])
  return connection
}

/** Every read the page shows goes through `read`, so one with no answer in 10 s fails and its place shows the failed form with
 * Retry. A read is tried once: offline it fails, and is never parked to be sent later. */
const READ = { networkMode: "always", retry: false } as const

async function allServers(signal: AbortSignal): Promise<ServerView[]> {
  const items: ServerView[] = []
  let cursor = ""
  do {
    const page = await read(ServerPage, `api/servers?limit=500&cursor=${encodeURIComponent(cursor)}`, signal)
    items.push(...page.items)
    cursor = page.next
  } while (cursor)
  return items
}

function analyticsPath(q: AnalyticsQuery): string {
  const params = new URLSearchParams({ from: q.from, to: q.to, bucket: `${q.bucketSeconds}s` })
  q.lineages?.forEach((l) => params.append("lineage", l))
  return `api/analytics?${params}`
}

/** The two reads a write settles on. use-write.ts keeps both observed while the page is open, so a write's read is always asked. */
export const stateRead = queryOptions({ ...READ, queryKey: keys.state, queryFn: ({ signal }) => read(View, "api/state", signal) })
export const settingsRead = queryOptions({ ...READ, queryKey: keys.settings, queryFn: ({ signal }) => read(SettingsView, "api/settings", signal) })

export const serversRead = queryOptions({ ...READ, queryKey: keys.servers, queryFn: ({ signal }) => allServers(signal) })
/** The build that answers. The page reloads itself over another release's. The CSRF token api.ts keeps is read on its own. */
export const sessionRead = queryOptions({ ...READ, queryKey: keys.session, queryFn: ({ signal }) => read(Session, "api/session", signal) })
/** The build About shows, what is offered over it, and the update that runs or ended last. */
export const updateRead = queryOptions({ ...READ, queryKey: keys.update, queryFn: ({ signal }) => read(UpdateView, "api/update", signal) })
export const switchesRead = queryOptions({ ...READ, queryKey: keys.switches, queryFn: ({ signal }) => read(z.array(SwitchEvent), "api/switches", signal) })
export const analyticsRead = (q: AnalyticsQuery, enabled = true) =>
  queryOptions({ ...READ, queryKey: keys.analytics(q), queryFn: ({ signal }) => read(AnalyticsReport, analyticsPath(q), signal), placeholderData: (prev) => prev, enabled })

export const useView = () => useQuery(stateRead)
export const useServers = () => useQuery(serversRead)
export const useSwitches = () => useQuery(switchesRead)
export const useReplaced = () => useQuery({ ...READ, queryKey: keys.replaced, queryFn: ({ signal }) => read(z.array(ReplacedEvent), "api/replaced", signal) })
export const useRefreshes = (id: SourceId, enabled = true) =>
  useQuery({ ...READ, queryKey: keys.refreshes(id), queryFn: ({ signal }) => read(z.array(RefreshEvent), `api/sources/${id}/refreshes`, signal), enabled })
export const useSettings = () => useQuery(settingsRead)
/** The release offered over the one that runs, for the marks that lead to it. */
export const useOffered = () => useQuery(updateRead).data?.offered ?? null
/** A source's whole URL, read only while the reader asks to see it, and forgotten once no one shows it. */
/** A source's stored URL, read for the source as the page last saw it, at `revision`. */
export const useSourceUrl = (id: SourceId, revision: number, enabled: boolean) =>
  useQuery({ ...READ, queryKey: keys.sourceUrl(id, revision), queryFn: async ({ signal }) => (await read(z.strictObject({ url: z.string() }), `api/sources/${id}/url`, signal)).url, enabled, gcTime: 0, staleTime: Infinity })
export const useAnalytics = (q: AnalyticsQuery, enabled = true) => useQuery(analyticsRead(q, enabled))

/** A preview of a rule as the page has it: no answer yet; refused or failed, with what the last answer matched if one stands; or
 * an answer, `current` while it is for these members over this list and no newer list is on its way. */
export type PreviewRead =
  | { state: "waiting" }
  | { state: "failed"; error: unknown; matched: readonly LineageId[] | null }
  | { state: "read"; matched: readonly LineageId[]; current: boolean }

/** What the daemon says `members` resolve to over the server list the page holds. Each new list is asked about again, after it
 * arrives, so an answer never predates the rows it filters. The last answer stays while the next is read. */
export function useGroupPreview(members: Members, enabled = true): PreviewRead {
  const list = useServers()
  const preview = useQuery({
    queryKey: keys.groupPreview(members, list.dataUpdatedAt),
    queryFn: ({ signal }) => api.previewGroup(members, signal),
    enabled, retry: false, gcTime: 0, placeholderData: (previous) => previous,
  })
  const matched = preview.data?.matched ?? null
  if (preview.isError) return { state: "failed", error: preview.error, matched }
  if (matched === null) return { state: "waiting" }
  return { state: "read", matched, current: !list.isFetching && preview.isSuccess && !preview.isPlaceholderData && !preview.isFetching }
}

/** Servers by id, for every page that names a server from an id. */
export function useServerIndex(): Map<LineageId, ServerView> {
  const { data } = useServers()
  return useMemo(() => new Map((data ?? []).map((l) => [l.id, l])), [data])
}

/** A value that settles a moment after the typing stops, so a preview reads what was meant and not each keystroke. */
export function useSettled<T>(value: T, delay = 250): T {
  const [settled, setSettled] = useState(value)
  const key = JSON.stringify(value)
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), delay)
    return () => clearTimeout(timer)
    // The key stands for the value, so an equal value in a new object restarts nothing.
    // oxlint-disable-next-line react-hooks/exhaustive-deps
  }, [key, delay])
  return settled
}
