import type { AnalyticsQuery, Members, SourceId } from "./schema.ts"

export const keys = {
  state: ["state"],
  servers: ["servers"],
  switches: ["switches"],
  replaced: ["replaced"],
  refreshes: (id: SourceId) => ["refreshes", id],
  anyRefreshes: ["refreshes"],
  settings: ["settings"],
  session: ["session"],
  update: ["update"],
  groupPreview: (members: Members, list: number) => ["groupPreview", members, list],
  analytics: (q: AnalyticsQuery) => ["analytics", q],
  sourceUrl: (id: SourceId, revision: number) => ["sourceUrl", id, revision],
} as const
