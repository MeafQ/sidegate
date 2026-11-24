import type { LineageId, ServerView, SourceView } from "./schema.ts"

/** Rows under one heading of a source's table: a home config's remarks, or "Other servers" for the servers no config is home to. */
export type Section = { heading: { kind: "config"; key: LineageId; name: string } | { kind: "nameOnly" }; rows: ServerView[] }

/** Whether a source's table groups its rows by home: when some config is home to more than one of its servers. */
export function groupsByHome(servers: readonly ServerView[]): boolean {
  const homed = servers.flatMap((s) => (s.home === null ? [] : [s.home]))
  return new Set(homed).size < homed.length
}

/** Rows under their home configs in feed order, each config's as it lists them, then the servers with no home in the order given.
 * A home the feed no longer lists, a missing server's, follows the ones it does, under the name the server last saw it by. A section
 * with no row is left out. */
export function sectionsOf(rows: readonly ServerView[], source: Pick<SourceView, "feed">): Section[] {
  const configs = new Map((source.feed?.configs ?? []).flatMap((c) => (c.key === null ? [] : [[c.key as string, { name: c.name, place: c.place }]])))
  const homes = new Map<string, Section>()
  const nameOnly: Section = { heading: { kind: "nameOnly" }, rows: [] }
  for (const s of rows) {
    if (s.home === null) {
      nameOnly.rows.push(s)
      continue
    }
    const name = configs.get(s.home)?.name ?? s.seen.find((seen) => seen.configKey === s.home)?.config ?? s.name?.remarks ?? ""
    const section = homes.get(s.home) ?? { heading: { kind: "config", key: s.home, name }, rows: [] }
    section.rows.push(s)
    homes.set(s.home, section)
  }
  const place = (section: Section) => (section.heading.kind === "config" ? configs.get(section.heading.key)?.place : undefined) ?? Infinity
  const position = (s: ServerView) => s.seen.find((seen) => seen.configKey === s.home)?.position ?? Infinity
  const listed = [...homes.values()].map((section) => ({ ...section, rows: section.rows.toSorted((a, b) => (position(a) === position(b) ? 0 : position(a) - position(b))) }))
  return [...listed.toSorted((a, b) => place(a) - place(b)), nameOnly].filter((section) => section.rows.length > 0)
}
