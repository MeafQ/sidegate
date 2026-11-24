import type { FeedConfig, LineageId, ServerView, SourceView, View } from "./schema.ts"

/** One place a server holds in its source's document: a config's outbound, with the config's place and the outbound's position. */
export type Place = ServerView["seen"][number] & { place: number }

/** A config of a source's feed that holds a server, so its key names it in a server's places. */
export type KeyedConfig = FeedConfig & { key: LineageId }

/** A source's configs that hold a server, in its document's order. None until its feed is read. */
export const keyedConfigs = (source: Pick<SourceView, "feed">): KeyedConfig[] =>
  (source.feed?.configs ?? []).filter((c): c is KeyedConfig => c.key !== null)

/** A server's places in feed order: by its config's place in the document, then its position there. A config its source's
 * feed doesn't list has place 0, and goes after the rest. */
export function placesOf(server: Pick<ServerView, "seen">, configs: readonly KeyedConfig[]): Place[] {
  const placeOf = new Map(configs.map((c) => [c.key as string, c.place]))
  return server.seen.map((seen) => ({ ...seen, place: placeOf.get(seen.configKey) ?? 0 }))
    .toSorted((a, b) => (a.place || Infinity) - (b.place || Infinity) || a.position - b.position)
}

/** Feed order across a list: the daemon's source order, then each server's first place. */
export function feedOrder(view: Pick<View, "sources">) {
  const sourceAt = new Map(view.sources.map((s, i) => [s.id as string, i]))
  const configsOf = new Map(view.sources.map((s) => [s.id as string, keyedConfigs(s)]))
  const key = (s: ServerView) => {
    const first = placesOf(s, configsOf.get(s.source) ?? [])[0]
    return [sourceAt.get(s.source) ?? Infinity, first?.place || Infinity, first?.position ?? Infinity]
  }
  return (a: ServerView, b: ServerView) => {
    const [x, y] = [key(a), key(b)]
    return x[0] - y[0] || x[1] - y[1] || x[2] - y[2]
  }
}
