import { dayLetter } from "./format.ts"
import type { AnalyticsBucket, LineageId } from "./schema.ts"

/** A server's week is 28 six-hour buckets counted in UTC. */
export const BUCKET_SECONDS = 6 * 3600
const BUCKET_MS = BUCKET_SECONDS * 1000

/** The last week, ending at the bucket boundary after now, so a query key stays the same within a bucket and the bucket now is in
 * is the last. */
export function lastWeek(now = Date.now()) {
  const end = (Math.floor(now / BUCKET_MS) + 1) * BUCKET_MS
  return { from: new Date(end - 7 * 86_400_000).toISOString(), to: new Date(end).toISOString() }
}

function steps(from: string, to: string, stepSeconds: number): number[] {
  const step = stepSeconds * 1000
  const out: number[] = []
  for (let t = Math.ceil(Date.parse(from) / step) * step; t < Date.parse(to); t += step) out.push(t)
  return out
}

export type ServerPoint = { start: string; pingMs: number | null; pingP95Ms: number | null; servedSeconds: number; forced: number }

/** What a server's week shows: nothing to say, its failovers over the chart's empty frame, or its ping chart. */
export function serverWeek(points: ServerPoint[]): "empty" | "facts" | "chart" {
  if (points.some((p) => p.pingMs !== null)) return "chart"
  return points.some((p) => p.forced > 0) ? "facts" : "empty"
}

/** One point per step of the window. A step with no bucket stays empty, so a chart shows the gap and doesn't draw across it. */
export function serverHistory(buckets: AnalyticsBucket[], lineage: LineageId, from: string, to: string, stepSeconds: number): ServerPoint[] {
  const mine = new Map(buckets.filter((b) => b.lineage === lineage).map((b) => [Date.parse(b.start), b]))
  return steps(from, to, stepSeconds).map((t) => {
    const b = mine.get(t)
    return { start: new Date(t).toISOString(), pingMs: b?.pingMedianMs ?? null, pingP95Ms: b?.pingP95Ms ?? null, servedSeconds: b?.servedSeconds ?? 0, forced: b?.forcedSwitchAways ?? 0 }
  })
}

/** A day letter on a server's week: the bucket it stands at, its letter and whether it is today's. */
export type DayTick = { start: string; letter: string; today: boolean }

/** The seven local dates ending today, each at the bucket that holds its local noon. While today's noon is still past the window,
 * today's letter stands at the bucket that holds the middle of the part of today the window holds. The eighth date a window off
 * UTC touches gets none. */
export function dayTicks(points: readonly { start: string }[], now: number): DayTick[] {
  const starts = points.map((p) => Date.parse(p.start))
  const end = (starts.at(-1) ?? 0) + BUCKET_MS
  const bucketOf = (t: number) => points.find((_, i) => starts[i] <= t && t < starts[i] + BUCKET_MS)
  return [6, 5, 4, 3, 2, 1, 0].flatMap((back) => {
    const date = new Date(now)
    date.setDate(date.getDate() - back)
    const noon = new Date(date).setHours(12, 0, 0, 0)
    const at = back === 0 && noon >= end ? (Math.max(new Date(date).setHours(0, 0, 0, 0), starts[0]) + end) / 2 : noon
    const bucket = bucketOf(at)
    return bucket ? [{ start: bucket.start, letter: dayLetter(date.getTime()), today: back === 0 }] : []
  })
}
