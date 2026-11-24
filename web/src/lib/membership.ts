import type { Box, Members } from "./schema.ts"

/** The membership rule: a server is in a group when every set condition but the box matches it, `matched`, and the box holds for
 * its tick. Unset, the box holds for every server. */
export const isMember = (id: string, matched: ReadonlySet<string>, box: Box, ticked: ReadonlySet<string>) =>
  matched.has(id) && (box === null || ticked.has(id) === (box === "ticked"))

const set = ({ not, values }: { not: boolean; values: readonly unknown[] }) => not || values.length > 0

/** Whether a rule sets any condition, the box included. With none it matches nothing. */
export const conditioned = (m: Members) => set(m.sources) || set(m.countries) || set(m.protocols) || m.include !== "" || m.exclude !== "" || m.box !== null

/** A new group that could hold no server, which the daemon won't make: no condition set, or the box ticked with nothing
 * ticked, since the conditions join by AND. Go's `Group.Bare`. */
export const bare = (m: Members, ticked: number) => !conditioned(m) || (m.box === "ticked" && ticked === 0)
