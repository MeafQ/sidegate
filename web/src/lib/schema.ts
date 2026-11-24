import { z } from "zod"

/* The daemon's JSON DTOs (internal/daemon/web.go, internal/domain), lower-camel as the handler's notes ask. Every
 * response is parsed here, so pages trust these types. Every object is strict, because the page and the daemon ship
 * in one binary: a field Go adds and this file doesn't know fails tests instead of being dropped. */

export const SourceId = z.string().min(1).brand<"SourceId">()
export const LineageId = z.string().min(1).brand<"LineageId">()
export const SlotId = z.string().min(1).brand<"SlotId">()
export const GroupId = z.string().min(1).brand<"GroupId">()
export const EventId = z.string().min(1).brand<"EventId">()
export const Cursor = z.string().brand<"Cursor">()
export type SourceId = z.infer<typeof SourceId>
export type LineageId = z.infer<typeof LineageId>
export type SlotId = z.infer<typeof SlotId>
export type GroupId = z.infer<typeof GroupId>
export type EventId = z.infer<typeof EventId>
export type Cursor = z.infer<typeof Cursor>

const Time = z.iso.datetime({ offset: true })
const Seconds = z.number().nonnegative()
const Count = z.number().int().nonnegative()

export const Exit = z.discriminatedUnion("kind", [
  z.strictObject({ kind: z.literal("server"), server: LineageId }),
  z.strictObject({ kind: z.literal("group"), group: GroupId }),
  /** A whole subscription: the servers that pass its filter, failing over with its priority. */
  z.strictObject({ kind: z.literal("source"), source: SourceId }),
  z.strictObject({ kind: z.literal("direct") }),
  z.strictObject({ kind: z.literal("block") }),
])
export type Exit = z.infer<typeof Exit>

/** PUT /api/slots/{id}. */
export const SlotInput = z.strictObject({ name: z.string().min(1), exit: Exit })
export type SlotInput = z.infer<typeof SlotInput>
/** A slot's write as the page presses it: what it sends, and which part of it the reader changed. */
export type SlotEdit = { readonly input: SlotInput; readonly edit: "exit" | "name" }

export const Verdict = z.enum(["unknown", "succeeded", "failed"])
export type Verdict = z.infer<typeof Verdict>

export const DownReason = z.enum([
  "unselected", "deleting", "noUsableServer", "serverMissing", "serverRefused", "confirmedDead", "plumbingUnavailable", "noExit",
])
export type DownReason = z.infer<typeof DownReason>

/** A server, a reason the slot carries nothing, the WAN for Direct or the drop for Block. */
export const Selection = z.discriminatedUnion("kind", [
  z.strictObject({ kind: z.literal("up"), server: LineageId }),
  z.strictObject({ kind: z.literal("down"), reason: DownReason }),
  z.strictObject({ kind: z.literal("direct") }),
  z.strictObject({ kind: z.literal("block") }),
])
export type Selection = z.infer<typeof Selection>

export const RuntimePhase = z.enum(["closed", "ready", "unknown"])
export type RuntimePhase = z.infer<typeof RuntimePhase>

export const SlotReport = z.strictObject({
  phase: RuntimePhase,
  applied: Selection,
  networkId: z.string().nullable(),
  networkEstablished: z.boolean(),
  errorCode: z.string().nullable(),
})

/** known is false without an API key or before a first pass. Routes stay UniFi's: the page doesn't show them. */
/** What the last pass couldn't do for one VPN Client of ours: another network holds its name, UniFi's limit left no room, UniFi
 * refused the write, or, for a client kept after its slot went, Policy Table routes still point at it. */
export const ClientTrouble = z.enum(["nameTaken", "limitReached", "refused", "routed"])
export type ClientTrouble = z.infer<typeof ClientTrouble>

export const SlotUnifi = z.strictObject({
  name: z.string(),
  interface: z.string(),
  known: z.boolean(),
  client: z.enum(["present", "disabled", "deleted", "none"]),
  trouble: ClientTrouble.nullable(),
})
export type SlotUnifi = z.infer<typeof SlotUnifi>

/** UniFi's capacity. known is false until a pass has read the clients, and then free boxes read as unknown, never free. */
export const VpnClients = z.strictObject({
  known: z.boolean(),
  limit: Count,
  foreign: z.array(z.strictObject({ id: z.string(), name: z.string(), enabled: z.boolean(), trouble: ClientTrouble.nullable() })),
})
export type VpnClients = z.infer<typeof VpnClients>

export const SlotView = z.strictObject({
  /** When it was added: what the page orders by, so nothing moves when another is added. */
  createdAt: Time,
  id: SlotId,
  version: Count,
  name: z.string(),
  exit: Exit,
  /** checking: none of its servers is ready yet, while one hasn't answered a check or one is ready and not yet taken. */
  status: z.enum(["serving", "checking", "failed", "direct", "block"]),
  /** Why a failed slot carries nothing. Waiting for a first server is checking, never a reason. */
  downReason: DownReason.exclude(["unselected", "deleting", "noExit"]).nullable(),
  /** Asked for, what readback shows, and when a running handover closes. */
  server: LineageId.nullable(),
  appliedServer: LineageId.nullable(),
  leavingBy: Time.nullable(),
  pending: z.boolean(),
  runtime: SlotReport,
  unifi: SlotUnifi,
  /** What the slot would fall back to, best first. */
  backups: z.array(LineageId),
})
export type SlotView = z.infer<typeof SlotView>

/** A saved condition on one field: is any of these, or none of them. No values means it isn't set. */
const among = <T extends z.ZodType>(value: T) => z.strictObject({ not: z.boolean(), values: z.array(value) })
const CountryCode = z.string().length(2)
export const Protocol = z.enum(["vless", "vmess", "trojan", "shadowsocks", "socks", "http", "hysteria2"])
export type Protocol = z.infer<typeof Protocol>

/** A box condition: a server's tick in the group, or not. Null is unset. */
export const Box = z.enum(["ticked", "unticked"]).nullable()
export type Box = z.infer<typeof Box>

/** A group's rule: its servers are those every set condition matches, the box among them. With none set, the box included, it
 * matches nothing, so a hand-picked group is "box is ticked" alone. */
export const Members = z.strictObject({
  sources: among(SourceId), countries: among(CountryCode), protocols: among(Protocol), include: z.string(), exclude: z.string(), box: Box,
})
export type Members = z.infer<typeof Members>

/** PUT /api/groups/{id} with If-Match: a replace, whose ticks stay as stored. */
export const GroupInput = z.strictObject({ name: z.string().min(1), members: Members })
export type GroupInput = z.infer<typeof GroupInput>

/** PUT /api/groups/{id} with If-None-Match: a create, its ticks in the one write. The daemon refuses one that can hold no server. */
export const GroupCreate = GroupInput.extend({ ticked: z.array(LineageId) })
export type GroupCreate = z.infer<typeof GroupCreate>

/** The server a group's or source's slots were moved to by hand, kept ahead by a boost that fades to nothing at endsAt. */
export const Preferred = z.strictObject({ server: LineageId, endsAt: Time })
export type Preferred = z.infer<typeof Preferred>

/** resolved comes from Go, because RE2 runs only there. */
export const GroupView = z.strictObject({
  id: GroupId,
  revision: Count,
  name: z.string(),
  members: Members,
  /** The servers ticked in this group. A tick is a fact about the server here, and changes no member while the box is unset. */
  ticked: z.array(LineageId),
  resolved: z.array(LineageId),
  usedBy: z.array(SlotId),
  /** Forced moves of the slots on this group over 7 days: failure, slowness or UDP. A move for lower ping keeps connections open. */
  moves7d: Count,
  /** The server a slot would use first: rank 1 of order. */
  firstChoice: LineageId.nullable(),
  /** Members in the group's order, servers it left lately last. */
  order: z.array(LineageId),
  preferred: Preferred.nullable(),
})
export type GroupView = z.infer<typeof GroupView>

/** POST /api/groups/preview: what a rule's conditions but its box match now. The page applies the box from the ticks it shows.
 * It stores nothing. */
export const GroupPreview = z.strictObject({ matched: z.array(LineageId) })
export type GroupPreview = z.infer<typeof GroupPreview>

export const Quota = z.strictObject({
  upload: Count.nullable(),
  download: Count.nullable(),
  /** 0 is unlimited, and null means the provider didn't say. */
  limit: Count.nullable(),
  expires: Time.nullable(),
  refills: Time.nullable(),
  observedAt: Time.nullable(),
})
export type Quota = z.infer<typeof Quota>

/** Why the parser skipped an entry. */
export const DiagnosticCode = z.enum([
  "unsupported_protocol", "unsupported_transport", "unsupported_security", "unsupported_option", "unsupported_cipher", "unsupported_plugin",
  "insecure_tls", "malformed_entry", "invalid_server", "unresolved_dialer", "duplicate_tag",
])
export type DiagnosticCode = z.infer<typeof DiagnosticCode>

/** Why a refresh failed: a code, or an answer's HTTP status other than 200 or 304. */
export const FailureCode = z.enum([
  "invalid_location", "address_blocked", "too_many_redirects", "redirect_refused", "timeout", "network", "too_large", "bad_encoding",
  "unexpected_304", "malformed", "unknown_format", "too_many_entries", "no_usable_entries",
])
export type FailureCode = z.infer<typeof FailureCode>
export const HttpFailure = z.templateLiteral(["http_", z.int()])
export type HttpFailure = z.infer<typeof HttpFailure>
const Failure = z.union([FailureCode, HttpFailure])
export type Failure = z.infer<typeof Failure>

export const Diagnostic = z.strictObject({ entry: Count, code: DiagnosticCode, field: z.string() })
export type Diagnostic = z.infer<typeof Diagnostic>

/** One outbound of a config that became no server of its own, by why. index is its place in the config's outbound list from 1.
 * A helper such as freedom or blackhole is never a server. A hop is an outbound another dials through, so it is part of that server.
 * A refused root is a proxy the core can't run: position is its place among the roots, which it keeps, and code and field the parser's. */
export const Skip = z.discriminatedUnion("reason", [
  z.strictObject({ reason: z.literal("helper"), index: z.number().int().min(1), tag: z.string(), protocol: z.string() }),
  z.strictObject({ reason: z.literal("hop"), index: z.number().int().min(1), tag: z.string() }),
  z.strictObject({ reason: z.literal("refused"), index: z.number().int().min(1), tag: z.string(), position: z.number().int().min(1), code: DiagnosticCode, field: z.string() }),
])
export type Skip = z.infer<typeof Skip>

/** A config of the source's latest document, in the document's order. key is what its servers' Seen name, null with no server.
 * servers counts its proxy roots that became a server, and skipped lists the rest of its outbounds. */
export const FeedConfig = z.strictObject({ key: LineageId.nullable(), place: z.number().int().min(1), name: z.string(), servers: Count, skipped: z.array(Skip) })
export type FeedConfig = z.infer<typeof FeedConfig>

/** What the source's latest document listed. */
export const Feed = z.strictObject({ configs: z.array(FeedConfig) })
export type Feed = z.infer<typeof Feed>

const RemoteOrigin = z.strictObject({
  kind: z.literal("remote"),
  userAgent: z.string(),
  refreshEverySeconds: Seconds,
  paused: z.boolean(),
  provider: z.strictObject({
    title: z.string(),
    announcement: z.string(),
    supportUrl: z.string(),
    profileUrl: z.string(),
    suggestedRefreshSeconds: Seconds.nullable(),
    quota: Quota,
  }),
  fetch: z.strictObject({ checkedAt: Time.nullable(), updatedAt: Time.nullable(), failures: Count, failureCode: Failure.nullable() }),
  dueAt: Time,
})
export type RemoteOrigin = z.infer<typeof RemoteOrigin>

/** GET /api/sources/{id}/refreshes, newest first. A failure the WAN explains doesn't count against the provider. */
export const RefreshEvent = z.strictObject({
  at: Time,
  outcome: z.enum(["updated", "unchanged", "failed"]),
  failureCode: Failure.nullable(),
  wanExcluded: z.boolean(),
  added: Count,
  changed: Count,
  missing: Count,
})
export type RefreshEvent = z.infer<typeof RefreshEvent>

export const SourceView = z.strictObject({
  id: SourceId,
  revision: Count,
  name: z.string(),
  /** The owner typed this name. Otherwise it follows the provider's title or the URL's host. */
  nameSet: z.boolean(),
  /** A subscription can be an Exit, so it has what a group has. Its members are every present server it lists. */
  firstChoice: LineageId.nullable(),
  /** Its servers in its order, servers it left lately last. */
  order: z.array(LineageId),
  preferred: Preferred.nullable(),
  moves7d: Count,
  origin: z.discriminatedUnion("kind", [RemoteOrigin, z.strictObject({ kind: z.literal("manual") })]),
  /** Null until a document is read: a subscription's next refresh after the step to state version 13, pasted text when pasted again. */
  feed: Feed.nullable(),
  stored: Count,
  missing: Count,
  /** Every slot a delete of this source is refused for: its Exit is the source, one of its servers, or a group the delete
   * removes. */
  usedBy: z.array(SlotId),
  /** The groups a delete of this source removes with it. */
  removes: z.array(GroupId),
  diagnostics: z.array(Diagnostic),
  /** When it was created. Null for a source stored before the daemon kept this. */
  addedAt: Time.nullable(),
})
export type SourceView = z.infer<typeof SourceView>

export const WanState = z.enum(["unknown", "up", "down"])
export type WanState = z.infer<typeof WanState>

/** What keeps UniFi from making or changing any VPN Client: no API key, a refused key, or no answer from UniFi's Network
 * Application. Slots keep serving through each. */
export const UnifiTrouble = z.enum(["noKey", "keyRefused", "notAnswering"])
export type UnifiTrouble = z.infer<typeof UnifiTrouble>

/** The release's pinned Xray and the one running, each as its binary says. They differ after an upgrade whose core restart didn't
 * happen, until a restart. Null while unknown: the running one while no core runs or its version isn't read yet. */
export const CoreView = z.strictObject({ version: z.string().nullable(), running: z.string().nullable() })
export type CoreView = z.infer<typeof CoreView>

/** acceptedAt: when UniFi last answered the key, null since the daemon started until it does. */
export const UnifiView = z.strictObject({ acceptedAt: Time.nullable() })
export type UnifiView = z.infer<typeof UnifiView>

export const View = z.strictObject({
  revision: Count,
  generatedAt: Time,
  slots: z.array(SlotView),
  sources: z.array(SourceView),
  groups: z.array(GroupView),
  wan: WanState,
  vpnClients: VpnClients,
  core: CoreView,
  unifi: UnifiView,
  unifiTrouble: UnifiTrouble.nullable(),
})
export type View = z.infer<typeof View>

export const Transport = z.enum(["tcp", "ws", "grpc", "xhttp", "hysteria"])
export type Transport = z.infer<typeof Transport>
export const Presence = z.enum(["present", "missing", "tombstone"])
export type Presence = z.infer<typeof Presence>

/** What a server's DNS checks over UDP add up to. It says nothing of TCP. */
export const UDPState = z.enum(["passed", "failed", "unknown"])
export type UDPState = z.infer<typeof UDPState>

/** One place a server holds in its source: a config's outbound. position is its place among the config's proxy roots from 1, in
 * provider order, before anything is validated, so it keeps gaps. */
export const Seen = z.strictObject({ config: z.string(), configKey: LineageId, tag: z.string(), position: z.number().int().min(1) })
export type Seen = z.infer<typeof Seen>

/** A server's name as its feed gives it, each part plain: its home config's remarks, its tag there, or both. A server with no home is
 * its most frequent tag alone. No name has neither part: a server the feed names nowhere has none, and its label is "Server", though it
 * can still have a home. */
export const Name = z.union([
  z.strictObject({ remarks: z.string(), tag: z.string().nullable() }),
  z.strictObject({ remarks: z.null(), tag: z.string() }),
])
export type Name = z.infer<typeof Name>

/** Where a server's country came from: an owner's rule over its name, the one flag its tags carry, or the one flag its home's remarks
 * carry. */
export const CountrySource = z.enum(["rule", "tagFlag", "configFlag", "unknown"])
export type CountrySource = z.infer<typeof CountrySource>

export const ServerView = z.strictObject({
  id: LineageId,
  source: SourceId,
  /** The name's parts joined with " · ", then the number. */
  label: z.string(),
  name: Name.nullable(),
  /** The key of the config it is named from, which seen and the feed carry, set too when that config names it nowhere. Null when no
   * config is its home, so it is named by its tag alone or not at all. */
  home: LineageId.nullable(),
  /** Added from 2 to tell two servers of one name apart. */
  number: z.number().int().min(2).nullable(),
  country: z.strictObject({ code: z.string().length(2).nullable(), source: CountrySource }),
  presence: Presence,
  supported: z.boolean(),
  udp: UDPState,
  seen: z.array(Seen),
  /** What the page filters and shows on. */
  protocol: Protocol,
  transport: Transport,
  missingSince: Time.nullable(),
  health: z.strictObject({
    verdict: Verdict,
    checkedAt: Time.nullable(),
    /** The newest ping to the current site. A heartbeat since measured no ping, so this can be older than checkedAt. */
    pingAt: Time.nullable(),
    /** A result in the last 10 minutes. Otherwise what's here is the last week's record, not how it is now. */
    current: z.boolean(),
    /** Typical and bad-case ping over 7 days. */
    pingP50Ms: z.number().nonnegative().nullable(),
    pingP95Ms: z.number().nonnegative().nullable(),
    newConnectionP50Ms: z.number().nonnegative().nullable(),
    newConnectionP95Ms: z.number().nonnegative().nullable(),
    /** Ping times factor, in ms, what the order ranks on. Null with no ping on record. */
    score: z.number().nonnegative().nullable(),
    /** What the ping counts as: 1.25 unknown, falling as the server is seen working, rising with incidents. */
    factor: z.number().min(1),
    incidents7d: Count,
    /** Days seen working, by heartbeat while serving or by passing a comparison, halving in a week: what pays the factor down. */
    seenDays: z.number().nonnegative(),
    /** How long its checks found it up in the last 7 days, in ms. Null until the daemon has kept it for the whole week, or since it was first seen. */
    seenWorking7dMs: z.number().int().nonnegative().nullable(),
    /** When its newest failure began: an outage's start, or a slowness move off it. */
    lastFailureAt: Time.nullable(),
    firstSeenAt: Time.nullable(),
  }),
})
export type ServerView = z.infer<typeof ServerView>

export const ServerPage = z.strictObject({ items: z.array(ServerView), next: Cursor })
export type ServerPage = z.infer<typeof ServerPage>

/** POST /api/servers/{id}/probe, a manual probe. */
export const ProbeResult = z.strictObject({
  lineage: LineageId,
  verdict: Verdict,
  pingMs: z.number().nonnegative().nullable(),
  newConnectionMs: z.number().nonnegative().nullable(),
  at: Time,
})
export type ProbeResult = z.infer<typeof ProbeResult>

export const AnalyticsBucket = z.strictObject({
  start: Time,
  source: SourceId,
  lineage: LineageId,
  country: z.string(),
  uploaded: Count,
  downloaded: Count,
  probeSuccesses: Count,
  probeAttempts: Count,
  wanExcluded: Count,
  unknown: Count,
  pingMedianMs: z.number().nonnegative().nullable(),
  pingP95Ms: z.number().nonnegative().nullable(),
  newConnectionMedianMs: z.number().nonnegative().nullable(),
  forcedSwitchAways: Count,
  /** How long a slot used this server in the bucket, and the failure or slowness moves off it. */
  servedSeconds: Seconds,
  incidents: Count,
  managementTrafficIncluded: z.boolean(),
})
export type AnalyticsBucket = z.infer<typeof AnalyticsBucket>

export const AnalyticsReport = z.strictObject({
  buckets: z.array(AnalyticsBucket),
  coveredUntil: Time,
  gapCount: Count,
})
export type AnalyticsReport = z.infer<typeof AnalyticsReport>

export type AnalyticsQuery = { from: string; to: string; bucketSeconds: number; lineages?: LineageId[] }

export const SwitchReason = z.enum([
  "initialSelection", "failureMove", "slownessMove", "improvementMove", "exitChanged", "serverLeftFeed", "serverRefused",
  "revisionChanged", "recovery", "udpMove", "preferred",
])
export type SwitchReason = z.infer<typeof SwitchReason>

export const SignalKind = z.enum(["tcpInfo", "outboundCounters", "coreWarning", "activeProbe", "wanBaseline"])
export type SignalKind = z.infer<typeof SignalKind>

export const EvidenceSummary = z.strictObject({
  server: LineageId,
  kind: SignalKind,
  verdict: Verdict,
  target: z.string(),
  round: Count,
  at: Time,
  pingMs: z.number().nonnegative().nullable(),
  newConnectionMs: z.number().nonnegative().nullable(),
})
export type EvidenceSummary = z.infer<typeof EvidenceSummary>

/** Who moved: a slot on its own, or a group or source with every slot on it. */
export const SwitchOwner = z.discriminatedUnion("kind", [
  z.strictObject({ kind: z.literal("slot"), slot: SlotId }),
  z.strictObject({ kind: z.literal("group"), group: GroupId }),
  z.strictObject({ kind: z.literal("source"), source: SourceId }),
])
export type SwitchOwner = z.infer<typeof SwitchOwner>

/** domain.Switch, from GET /api/switches: one move of one owner, once, with the slots it moved. */
export const SwitchEvent = z.strictObject({
  id: EventId,
  owner: SwitchOwner,
  slots: z.array(SlotId),
  intent: Count,
  from: Selection,
  to: Selection,
  reason: SwitchReason,
  evidence: z.array(EvidenceSummary),
  requestedAt: Time,
  appliedAt: Time.nullable(),
})
export type SwitchEvent = z.infer<typeof SwitchEvent>

/** A refresh kept a server's identity across a changed connection, from GET /api/replaced. */
export const ReplacedEvent = z.strictObject({
  source: SourceId,
  server: LineageId,
  label: z.string(),
  previousLabel: z.string(),
  at: Time,
})
export type ReplacedEvent = z.infer<typeof ReplacedEvent>

export const ChangeEvent = z.strictObject({
  id: Cursor,
  at: Time,
  revision: Count,
  kind: z.string(),
  slot: SlotId.nullable(),
  source: SourceId.nullable(),
  resync: z.boolean(),
})
export type ChangeEvent = z.infer<typeof ChangeEvent>

/** PUT /api/settings: the Retention form's one field. A replace keeps checkForUpdates, which changes only by its own PUT. */
export const Settings = z.strictObject({
  analyticsRetentionDays: z.number().int().min(1).max(365),
})
export type Settings = z.infer<typeof Settings>

/** PUT /api/settings/check-for-updates: whether the page checks for a newer release, saved at once with no precondition. */
export const CheckForUpdates = z.strictObject({ checkForUpdates: z.boolean() })
export type CheckForUpdates = z.infer<typeof CheckForUpdates>

/** The stored settings and their revision, which a PUT of Settings names in If-Match. */
export const SettingsView = Settings.extend({ checkForUpdates: z.boolean(), revision: Count })
export type SettingsView = z.infer<typeof SettingsView>

/** Who is signed in, the daemon's CSRF token, and the build that answers: its version, a tag such as "v0.1.0" or "dev-…" from a
 * checkout, and the release under releases/ it runs from, null for a daemon run from anywhere else. */
export const Session = z.strictObject({ user: z.string(), csrf: z.string(), version: z.string(), release: z.string().nullable() })
export type Session = z.infer<typeof Session>

/** How an update ended: as its upgrade said, or failed for a reason. */
export const UpdateOutcome = z.enum(["landed", "landedWithError", "refused", "rolledBack", "putBackFailed", "failed"])
export type UpdateOutcome = z.infer<typeof UpdateOutcome>

export const UpdateReason = z.enum([
  "noStart", "stopped", "interrupted", "moved", "offerChanged", "notNewer", "planChanged", "noRoom", "downloadFailed", "checksum", "wrongRelease",
])
export type UpdateReason = z.infer<typeof UpdateReason>

/** Why the last check for a newer release learned nothing. */
export const CheckFailure = z.enum(["unreachable", "clockWrong", "filesUnavailable", "malformed"])
export type CheckFailure = z.infer<typeof CheckFailure>

/** GET /api/update, and what both its POSTs answer. `releases` is false on a development build, which has no check and no update.
 * `guide` is the running release's own page on recovering over SSH, null on a development build.
 * `check` is the last check that ended, `offered` a release newer than `version`, whose page is `notes`, `running` the update that runs
 * and `last` the last that ended. A `reason` is null but on failed. */
export const UpdateView = z.strictObject({
  version: z.string(),
  releases: z.boolean(),
  guide: z.url().nullable(),
  check: z.strictObject({ at: Time.nullable(), failure: CheckFailure.nullable() }),
  offered: z.strictObject({ version: z.string(), restartsCore: z.boolean(), notes: z.url() }).nullable(),
  running: z.strictObject({ attempt: z.string(), version: z.string() }).nullable(),
  last: z.strictObject({
    attempt: z.string(), version: z.string(), at: Time, outcome: UpdateOutcome, reason: UpdateReason.nullable(), message: z.string().nullable(),
  }).nullable(),
})
export type UpdateView = z.infer<typeof UpdateView>

/** PUT /api/sources/{id}. A null URL or text keeps what is stored. A subscription's empty name follows the provider's title or the URL's
 * host, and its refresh of 0 follows the provider's interval. */
export const SourceInput = z.strictObject({
  name: z.string(),
  origin: z.discriminatedUnion("kind", [
    z.strictObject({ kind: z.literal("remote"), url: z.url().nullable(), userAgent: z.string().min(1), refreshEverySeconds: Seconds, paused: z.boolean() }),
    z.strictObject({ kind: z.literal("manual"), text: z.string().nullable() }),
  ]),
})
export type SourceInput = z.infer<typeof SourceInput>

