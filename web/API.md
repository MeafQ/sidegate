# The page's API contract

This is what the web page needs from the daemon. `internal/daemon/web.go` serves it. The page parses every answer against `src/lib/schema.ts`, which holds the JSON shapes. `tests/e2e/fake-daemon.mjs` enforces the rules below in the end-to-end checks, `tests/e2e/contract.test.mjs` holds the Go handler and the fake to the same answers, and `src/lib/write.ts` is what the page does with each answer.

All paths are under the page's base path, for example `/sidegate/`. A response means durable acceptance, not applied readiness: what follows arrives through `api/events`.

## Routes

| Method and path | Request | 2xx answer |
|---|---|---|
| `GET api/session` | | `Session`: the user, the CSRF token, the daemon's `version`, a tag such as `v0.1.0` or `dev-…` from a checkout, and `release`, the name of the release under `releases/` it runs from, `null` outside one |
| `GET api/state` | | `View`: slots, sources and their revisions |
| `GET api/servers?limit=&cursor=&source=&country=` | | `ServerPage`: `next` is empty on the last page |
| `GET api/analytics?from=&to=&bucket=&source=&lineage=` | `bucket` as `300s`; the last two repeat | `AnalyticsReport` |
| `GET api/switches` | | `SwitchEvent[]` |
| `GET api/replaced` | | `ReplacedEvent[]`, newest first: servers a refresh kept across a changed connection on a credential or name match. Moves no slot |
| `GET api/settings` | | `SettingsView` |
| `PUT api/settings` | `Settings`, with `If-Match`. It keeps the stored `checkForUpdates` | `SettingsView` |
| `PUT api/settings/check-for-updates` | `{ checkForUpdates }`, no precondition | `SettingsView` |
| `GET api/update` | | `UpdateView`, see "Updates" |
| `POST api/update/check` | `{ auto }`, CSRF only | `UpdateView`, once the check ended. `422 noReleases` to a check that isn't automatic on a development build |
| `POST api/update` | `{ version, restartsCore }`, CSRF only, sent once | `202` `UpdateView` |
| `PUT api/unifi/key` | `{ key }`, no precondition | `{}`. `422 keyRefused` or `422 notAnswering`, see "UniFi's key" |
| `PUT api/slots/{id}` | `SlotInput`, with a precondition. An Exit is required | `SlotView` |
| `DELETE api/slots/{id}` | no body | any 2xx. `409 referenced` while UniFi routes point at it, as of the daemon's last read of its VPN Client, which a restart keeps |
| `POST api/slots/{id}/client/enable` | no body | `202`. `422` unless `unifi.client` is `disabled` |
| `POST api/slots/{id}/client/recreate` | no body | `202`. `422` unless `unifi.client` is `deleted` |
| `PUT api/sources/{id}` | `SourceInput`, with a precondition | `SourceView` |
| `DELETE api/sources/{id}` | no body | any 2xx. `409 referenced` naming the slots, see below |
| `POST api/sources/{id}/refresh` | no body, or `{ userAgent }`, no precondition | `202`. `422` on a pasted source. See "Saving a source" |
| `GET api/sources/{id}/refreshes` | | `RefreshEvent[]`, newest first. `404` for a source that isn't there |
| `GET api/sources/{id}/url` | | `{ url }`, the source's URL with its token, for its editor. `404` for a pasted source |
| `PUT api/groups/{id}` | `GroupCreate` with `If-None-Match: *`, `GroupInput` with `If-Match`. See "Filters" | `GroupView` |
| `DELETE api/groups/{id}` | no body | any 2xx. `409 referenced` naming the slots |
| `PUT api/groups/{id}/ticked/{id}` | `{ ticked }`, no precondition. The second id is the server's lineage | `GroupView` |
| `POST api/groups/{id}/preferred/{id}`, `POST api/sources/{id}/preferred/{id}` | no body, no precondition. The second id is the server's lineage | `ProbeResult`, the Test it ran. See "Preferring a server" |
| `DELETE api/groups/{id}/preferred/{id}?endsAt=`, `DELETE api/sources/{id}/preferred/{id}?endsAt=` | no body. `endsAt` is the preference's, as the view gave it | `{}`. See "Preferring a server" |
| `POST api/groups/preview` | `{ members }`, CSRF only | `GroupPreview` |
| `POST api/servers/{id}/probe` | no body | `ProbeResult`. `404` for a server with no route |
| `GET api/events` | | server-sent events, see below |

The page reads slots and sources from `api/state`, so there is no `GET api/slots` or `GET api/sources`. `bucket` must be a whole number of 5 minutes, or the read is refused with `400`. Each bucket starts at a multiple of `bucket` since 1970, whatever `from` is, and the first and last can hold less than a whole step. A probe of a server the core can't run, or that left its source, is a `404`. Any other server gets a probe route for the length of its test, and a test whose route doesn't come answers `unknown`, never `failed`.

`udp` in `api/servers` is what a server's DNS checks over UDP add up to. Only a server a slot is on is checked. It is `passed` after a check passed, and one or two failures since don't change it. It is `failed` after three failures in a row, until one passes. It is `unknown` before the first check, and when the newest check couldn't say anything, as when every server fails at once. A slot moved off a `failed` server to one that `passed` shows the reason `udpMove`, and its open connections close as for a Failover.

Ids are the client's, random, and never derived from a name: a PUT to an unused id is a create. A source's `url` is `https` only: an `http` address is `422`, and a redirect to one fails the fetch with `redirect_refused`. It comes back only on its own route, never in a view, and `null` in a replace leaves the stored one as it is. Each fetched source sends a device ID of its own, made when it is created, for panels that limit devices. A source's `addedAt` is when it was created, kept across renames and restarts, and `null` for a source stored before the daemon kept it. `userAgent` is what the provider sees: empty means `Happ/3.0.0`, since some providers answer `404` to a name they don't know. A source whose name follows its provider's title or URL host, and finds that name taken, is numbered: `Aurora 2`.

## Filters

A group's `members` are its rule: conditions, joined by AND. Each set condition is `{ not, values }`: is any of the values, or with `not` none of them. Is any of no values isn't set. Is none of no values is set and passes every server, which is what a group that excluded only a deleted source is left with. `include` and `exclude` are case-insensitive RE2 patterns over a server's label and every tag it was seen under. `box` is the condition on a server's tick: `"ticked"`, `"unticked"`, or `null`, unset. Any other value is `422`. A group's `ticked` lists the servers ticked in it by lineage. A tick is a fact about the server in that group and changes no member while `box` is `null`. `PUT api/groups/{id}/ticked/{id}` saves one at once, `{ "ticked": true }` or `false`, with no precondition and with the group's `revision` unchanged, so a rule edit open beside it still saves. Ticking a server the gateway doesn't know, or a tombstone, is `422`. Unticking any is allowed. A create is `GroupCreate`, the name, the rule and `ticked` in one write, so the group and its ticks are stored together or not at all. `ticked` is a list, `[]` for none: a create without it, or with `null`, is `400 malformed`. Its ticks follow a tick's rules, and one named twice, empty or `null` is `422 malformed`. A create that no server could ever pass is `422 ruleMissing` and stores nothing: one with no condition set, or with `"box": "ticked"` and nothing ticked, whatever else is set. A rule no server matches today is stored, and so is one whose conditions can't both hold, such as an include and an exclude that are the same pattern, or ticks the other conditions leave out. A replace is `GroupInput`: it keeps the stored ticks, and one that sends `ticked` is `400`. Only a create is held to the rule: a replace or a source's delete can still leave a group with no server. A source has no rule: every present server it lists is offered, and a `SourceInput` that sends `pick` is `400`. The step to state version 14 drops each source's stored rule, so a group's rule then matches a server that rule left out.

On a group, conditions that are all unset, `box` included, match nothing, so a hand-picked group is `"box": "ticked"` alone, and `resolved` is the present servers every set condition matches. A group preview returns `matched`, the present servers every set condition but `box` matches, and the page applies `box` from the ticks it shows. A group stored before ticks keeps its conditions and reads its hand lists as nothing: a group of picks alone matches nothing until its servers are ticked and its box condition set. Each place in a server's `seen` is a config's outbound: `position` counts the config's proxy roots from 1 in provider order, valid or not, and `configKey` names the config across refreshes. A position only shows where a server sits: no rule, name or country reads it.

### The feed

A source's `feed` is what its latest document listed, and is `null` until a document is read: a subscription's next refresh after the step to state version 13, and pasted text when it is pasted again. Its `configs` come in the document's order, each with its `place`, its item in the document's list from 1, its `name`, `servers`, the proxy roots that became a server, and `skipped`, its other outbounds by `index` in the config's outbound list from 1. A `key` names the config as `configKey` does in `seen`, and is `null` for a config with no server. A skipped outbound has a `reason`: `helper`, a utility such as freedom or blackhole with its `protocol`; `hop`, one another outbound dials through, so it is part of that server; or `refused`, a proxy root the core can't run, with its `position` among the roots and the parser's `code` and `field`. So a config's roots are the indexes no helper or hop holds, in position order, and the page lays a config's outbounds out in feed order from `seen` and `skipped` alone. A config whose outbounds repeat a tag is refused whole: each of its roots is `refused` with `duplicate_tag` on `tag`. An item with no server the gateway can run, such as a config of only utility outbounds, still counts, so places can skip a number. A document that lists its configs in another order moves `place` and nothing else.

A server's `label` is its `name` and its `number`. `name` has the parts of its name the feed gives, each plain: `remarks`, its home config's remarks, and `tag`, its tag there. A part that is only flags is no part, so a server whose remarks and tag are each a flag has a null `name` and its flag only as its country. At least one of them is set, and a server with no home has only its most frequent `tag`. `name` is null when the feed names the server nowhere, and the label is "Server". `home` is the `configKey` of the config it is named from, which `seen` and the feed carry, or null. A server whose home has no remarks and calls it by no tag has a `home` and a null `name`. A share link, a Clash proxy, a sing-box outbound and a SIP008 server each give their one name as their config's remarks, and their place in `seen` has an empty `tag`. `number` is from 2, added to tell two servers of one name apart. `country.source` says where its country came from: `rule`, an owner's rule over its name, `tagFlag`, the one flag its tags carry, `configFlag`, the one flag its home's remarks carry, or `unknown`. The page draws a name from `name` and `number`, never by splitting `label`, and lists a source's servers under their `home` when one config is home to more than one of them.

The daemon answers only requests its nginx hook vouched for. The hook sets `X-Sidegate-Auth` from UniFi's login subrequest and `X-Sidegate-Proof` to a random value only root can read, replacing whatever the browser sent in either. Without both the answer is a `401` with no `kind`, which the page reads as coming from in front of the daemon. So being allowed on the socket isn't enough, and a changed login contract closes the page.

UniFi's login runs a CSRF check of its own in front of the daemon. A `PUT`, `POST` or `DELETE` must carry the session's UniFi token in `X-Csrf-Token`, or nginx answers `403` from the auth subrequest and the daemon never sees the request. nginx hands the token out in `X-Csrf-Token` on a 200, 201, 204, 206, 301, 302, 303, 304, 307 or 308 answer to a request that carried none, the codes nginx's `add_header` covers without `always`, so the page keeps the latest it has seen and sends it on every mutation, previews included. It sends none until it has seen one. The hook clears `X-Csrf-Token` before the daemon, which keeps its own check below, since UniFi's doesn't run for an API key or on a console with it turned off.

A `401` or `403` with no `kind` was refused in front of the daemon, so that attempt stored nothing. On the first such refusal the page reads `api/session` again, which brings both tokens anew, and sends the write at once under the same key; a failed read of `api/session` still sends it, so a signed-out session is answered by its own `401`. A second refusal ends the write: "session expired" or "UniFi refused" with Sign in, or, when an earlier attempt may have landed, no answer, settled by a fresh read.

## Every mutation

A mutation is anything but `GET`. Every answer to one that takes a key carries `Sidegate-Revision`, the view revision the write was committed at, and the first answer's for a replay. Every write the daemon takes moves the view's revision, one that changed nothing included, so a view at that revision shows the write. A `412`, a daemon refusal and each refusal in the list below carry it too. A `5xx` carries the current one, since it committed nothing. It is the number "Read your own write" compares.

```
HTTP/1.1 200 OK
Sidegate-Revision: 42
```

Checks run in this order, and none of the refusals in this list is remembered under the key:

1. `X-Sidegate-Csrf` must match the daemon's token, from `api/session`, or `403` with kind `csrf`. The page fetches `api/session` again and resends once under the same key.
2. `Idempotency-Key` must be present, or `400` with kind `validation`.
3. A request with a body must have `Content-Type: application/json`, or `415` with kind `validation`. A request with no body needs no `Content-Type`, and sends none: `DELETE`, probe and a refresh that carries no User-Agent are like this.

### Routes that take the CSRF check only

A preview stores nothing, so `POST api/groups/preview` takes `X-Sidegate-Csrf` and no `Idempotency-Key`. Its answer is never remembered. So do `POST api/update/check` and `POST api/update`: an update's result is read from `GET api/update` and not answered, so a lost answer needs no replay. None of the three carries `Sidegate-Revision`.

`PUT api/sources/{id}` accepts up to 1 MiB, because pasted text is big, and the nginx hook passes a body that size. Every other mutation accepts 64 KiB.

## Revisions and preconditions

A revision is a non-negative integer. At one id it counts up by one, also across a delete and a create at that id, so an old `If-Match` can never pass on a new item. A slot names it `version`. Sources, groups and settings name it `revision`. The page reads it from the JSON of a 2xx, so a 2xx needs no `ETag`.

A versioned PUT carries exactly one of these:

| Header | Meaning | Refused with |
|---|---|---|
| `If-None-Match: *` | Create: nothing is stored at this id | `412` if something is |
| `If-Match: "<revision>"` | Replace: this revision is stored | `412` if another is, `404` if nothing is |
| neither, or both | | `428` with kind `validation` |

Settings always exist, so their PUT is always a replace. `checkForUpdates` says whether the page checks for a newer release, and is `true` until the owner turns it off; a settings body stored before it existed reads `true`. Only `PUT api/settings/check-for-updates` changes it, `{ "checkForUpdates": false }` or `true`, at once and with no precondition, as a tick is. A `Settings` that sends it, or a check that leaves it out or sends anything but `true` or `false`, is `400 malformed`. Either write moves the settings `revision`, so a Retention edit open beside the box saves over the revision it reads then.

A `412` names the revision stored at the moment of the refusal, because a later read may find a third writer's revision instead. It does so in the body and in `ETag`:

```
HTTP/1.1 412 Precondition Failed
ETag: "6"
Content-Type: application/json

{ "kind": "staleRevision", "message": "Changed by another editor.", "revision": 6 }
```

The page takes the body's `revision` first, then `ETag`, weak or strong. With neither, it reads the item to name the revision in the conflict it shows.

A `404` on a replace means the item is gone. A form that opened as a create goes back to creating at the same id with the draft kept, so the id must be free again after a delete. An editor of an existing item shows the failure and keeps the draft.

## Idempotency

The key belongs to one user action, and the page sends an action again unchanged when it can't tell whether it landed. These rules make that safe.

- **Fingerprint.** A key is tied to the method, path, `If-Match`, `If-None-Match` and body bytes of its first request.
- **Replay.** The same key with the same fingerprint gets the first answer's status, body and headers, with no second write and no fresh precondition check. A `412` and any other 4xx from the handler are replayed like a 2xx.
- **Reuse.** The same key with another fingerprint gets `422` with kind `validation`, and writes nothing.
- **5xx is not remembered.** The key stays free, so the resend is processed as a first request. A 5xx must therefore mean nothing was committed.
- **Durability.** The answer is stored in the same transaction as the write, so a crash can't commit one without the other.
- **Concurrency.** Two requests under one key are serialized: the second gets the first's answer.
- **Retention.** Keep a key for at least 24 hours, across restarts. The page sends a key again for at most 30 s by itself, and later only when the reader presses the same request again after it ended with no answer, a toast's Retry or an unchanged Add, while the page is open; nothing is kept across a reload. A resend after expiry is a first request again: its precondition still stops a second commit, a `412` on a create is read as that create, and on a replace the page shows a conflict naming what is stored, which is true whoever stored it.

A replay says only that that old write happened. It carries the first answer's `Sidegate-Revision`, so the page settles it on a view at or past that, which shows whatever came after.

## Read your own write

A read answered after a 2xx must show that write or a later one. `api/state` and `api/settings` can't come from a cache older than the last commit: the page settles a write only on a view at or past the answer's revision, and an older one would leave the write Pending until a newer read.

A write has settled on the page once `api/state`'s `revision` is at or past the answer's `Sidegate-Revision`, or for settings once `api/settings`' `revision` is at or past the answer's. Both numbers are the daemon's and only grow, so no ordering of tabs, replays or late answers can settle a write on a view older than it.

## The page's writes

A write goes again under its key at 1, 2, 4, 8 and 15 s after it was first sent, `api/session` read again first, until an answer ends it, and is ended at 30 s as "timed out". It ends at once on a `2xx`, a `412`, the daemon's own refusal, or a plain `4xx` from in front of the daemon (`413`, `429`), which stored nothing. It goes again on no connection, a `5xx` and a `2xx` it can't read, which may have landed. A `401` or `403` from in front, or the daemon's `csrf`, heals once with `api/session` read again, and then ends. A `412` on a create is read as that create landing earlier, since only this page knew the id. A read with no answer in 10 s is failed on the page. While a write waits on a read, the page keeps a read begun after the answer, or after the 30 s end, out or due: at once, and when one ends without settling it, the next 1, 2, 4 and 8 s later, then every 10 s, one at a time.

## Errors

Every error from the daemon has this body, with `revision` only on a `412` and `detail` only on a refused pattern:

```json
{ "kind": "validation", "reason": "sourceEmpty", "message": "That source has no servers now.", "revision": 6 }
```

`reason` names the cause, one name per cause, and the page words it from its one table, `REFUSAL_WORDS` in `src/lib/troubles.ts`. `message` is a sentence for the CLI and the log; the page shows it only for a reason it has no words for. The reasons are `Reason` in `internal/daemon/refusals.go`, and `tests/wire/enums.json` holds that list and the page's table equal:

| Reason | Cause |
|---|---|
| `noRoute` | No route at that path |
| `badQuery` | A read's query: `limit`, `from`, `to`, `bucket` or the range they make, or a Clear's `endsAt` |
| `keyMissing` | No `Idempotency-Key` |
| `keyReused` | The key was sent with another request |
| `tokenExpired` | `X-Sidegate-Csrf` isn't the daemon's token (kind `csrf`) |
| `tooLarge` | The body is over its limit |
| `notJSON` | A body that isn't `application/json` |
| `malformed` | A request not in the expected form, or a rule or Exit the page never sends |
| `preconditionMissing` | Neither or both of `If-Match` and `If-None-Match` |
| `stale` | Another revision is stored, or a press began before the owner's later press or Clear landed (`412`) |
| `unavailable`, `busy`, `notSaved`, `probeUnfinished` | The gateway couldn't answer, was busy, couldn't commit, or a Test didn't finish (`503`) |
| `badId` | An id that isn't 1 to 32 letters, digits, dashes and underscores |
| `badUserAgent` | A User-Agent that isn't one line of at most 200 characters |
| `badRefresh` | A negative refresh interval |
| `notHttps` | A source URL that isn't `https` |
| `slotGone`, `groupGone`, `sourceGone`, `serverGone` | What the write names isn't there, or is a tombstone |
| `notTestable` | A Test of a server the core can't run, or that left its source |
| `groupEmpty`, `sourceEmpty` | An Exit on a group or source no present server is in |
| `inUse` | A delete of a group or source a slot uses |
| `routed` | A delete of a slot UniFi routes point at |
| `sourcesGone` | A rule whose every source is gone |
| `ownerIdle` | A press on a group or source no slot follows |
| `notMember` | A press on a server the group or source doesn't use |
| `serverChanged` | A press whose server took a new revision, or whose core restarted, while its Test ran. Pressing again tests the new one |
| `failedSince` | A press whose server failed after its Test began |
| `clientsFull` | UniFi's limit of VPN Clients is reached |
| `slotsFull` | Every slot is taken |
| `clientNotDisabled`, `clientPresent` | Enable of a client that isn't disabled, Recreate of one still in UniFi |
| `noReleases` | A check or an update on a build that takes none: a development build, or one that doesn't run from `releases/` (kind `unsupported`) |
| `nothingNewer` | An update while no newer release is offered |
| `offerChanged` | An update whose `version` or `restartsCore` isn't what is offered now. The page reads `api/update` again |
| `lifecycleRunning` | An update while an install, upgrade or uninstall over SSH runs (`409`, kind `capacity`) |
| `nameMissing`, `urlMissing`, `textMissing` | A name, a new source's URL or a new paste's text is empty |
| `ruleMissing` | A new group that could hold no server: no condition set, or box is ticked with nothing ticked |
| `notRemote` | A refresh, or a URL read, of a pasted source |
| `originFixed` | A source changed between a URL and pasted text |
| `retentionRange` | Retention outside 1 to 365 days |
| `keyRefused`, `notAnswering` | UniFi refused the API key sent, or didn't answer its try (`422`) |
| `noUsableServers`, `unreadableText` | Pasted text with no server the gateway can use, or none it can read |
| `badPattern` | A name pattern Go can't compile. `detail` is Go's parse error cut to its cause in its fewest words: `missing closing ]`, `invalid repeat`, `lookahead not supported` |

| Kind | Status | The page treats it as |
|---|---|---|
| `validation` | `400`, `404`, `415`, `422`, `428` | The daemon's own refusal |
| `staleRevision` | `412` | A conflict |
| `unsupported` | `422` | The daemon's own refusal |
| `capacity` | `409` | The daemon's own refusal |
| `referenced` | `409` | The daemon's own refusal |
| `csrf` | `403` | Resent at once with a fresh token; a second ends the write |
| `unavailable` | `503` | Unknown: the write may have landed |
| `retryable` | `503` | Unknown: the write may have landed |

The JSON `kind` is how the page tells the daemon from a proxy. The daemon's own refusal ends that write, and the page says its reason in the page's words, under the field it names or as a toast. Any other 4xx, with no such body, came from something in front of the daemon and that attempt never reached it: the write ends at once with its status, a `429` with Retry, unless an earlier attempt may have landed, when it ends with no answer and settles on a fresh read. A 5xx, a lost connection and a 2xx whose body doesn't parse may have landed, so the write is sent again under its key at the next step of the schedule.

Never put a URL with a token, a credential, raw provider JSON, a private key, command output or an SQL error in `message`: the page shows it.

### Saving a source

A refresh moves no `revision`, so a source's `PUT` is refused only by another owner's write. A refresh that sends `userAgent` is one: the daemon stores the agent on the source and fetches with it in one command, under the refresh's key and with no precondition. The agent is checked as a save checks it, one line of at most 200 characters, or `422`, and empty is `Happ/3.0.0`. Another agent than the stored one moves the source's `revision`, so an editor opened before it is refused with `412`, and a fetch already running with the old agent is dropped. The stored agent moves nothing. Any other field is `400`. A `422` for pasted text with no usable servers carries `diagnostics`.

### Exits and switches

An Exit is one server, one group, one source, `direct` or `block`. A source as an Exit is a group of every present server it lists. `direct` sends the slot's new connections to the WAN and `block` drops them, with the tunnel up, no change in UniFi and no restart of the core. Their status is `direct` and `block`, `downReason` is null, and `runtime.applied.kind` says so once the core has it. Moving a slot onto, off or between them is an Exit change: open connections close, and the switch reads `exitChanged`, to or from `direct` or `block`. A slot back from them with no server ready yet selects `down` with `noExit` until one is. An Exit naming a group that resolves to nothing, or a source with no present server, is `422`.

A slot's `status` is `serving`, `checking`, `failed`, `direct` or `block`. A slot on a server, group or source that no ready server serves yet, a new one or one back from Direct or Block, the write's own answer included, is `checking` while one of its servers has no check result yet, or one is ready and the election hasn't taken it. It is `failed` with `noUsableServer` once every one has a result and none is ready, and `failed` with the pool's own reason (`serverMissing`, `serverRefused`, `noUsableServer`) when it has no server it can use. A `checking` or `failed` slot names no `server`, and `checking` has a null `downReason`, so a slot's `downReason` is never `unselected` or `noExit`; those stay in a selection.

A `SwitchEvent` names its `owner`: a slot that moved on its own, or a group or source with every slot on it in `slots`, once for the whole move. A slot joining a group that other slots use moves under that group with `slots` naming it alone.

`unifi.client` is the slot's VPN Client as UniFi last showed it: `present`, `disabled` by the owner in UniFi, `deleted` from UniFi after it was bound, or `none` yet. Nothing here writes to UniFi's routes.

`unifiTrouble` in `api/state` is what keeps UniFi from making or changing any VPN Client: `noKey` when the daemon has no API key, `keyRefused` when UniFi refused it on the last pass it answered, `notAnswering` when UniFi's Network Application didn't answer two passes in a row, and null once a pass worked. The first pass UniFi doesn't answer changes nothing here, so what was shown before stays through it. A refused key stays `keyRefused`, and after the daemon's start it stays null. The daemon runs that pass again 5 s later, or sooner when a slot changes what it needs of UniFi or a key is saved. Slots keep serving through each. `unifi.acceptedAt` is when UniFi last answered the key, kept in memory: null from the daemon's start until a pass UniFi answers, which runs a minute after the last.

### Preferring a server

A slot on a group or source can be put on one server of it by hand without giving up failover. `POST api/groups/{id}/preferred/{server}` (or `sources`) tests the server as `api/servers/{id}/probe` does, outside the daemon's commands, and answers that Test's `ProbeResult` under the request's key, so a resend gets the first answer. A Test that `failed` or that answered `unknown` moves nothing. A Test that `succeeded` moves every slot whose Exit is that group or source onto the server, in one switch whose `reason` is `preferred`, and a closed one reopens on it at once, inside its back-off and on a server whose record still stands recovering, since the Test is the evidence. Open connections close, as for an Exit change. The view published at the answer's revision has the move staged, every slot on the server, moving there or landed. A later press, a failover or a failed landing can change that after it. If the daemon can't save the move once the Test passed, it stops. The press answers `503 notSaved` if that goes out before the daemon closes the connection, and otherwise its caller hears nothing. Either way the page sends it again under the key, as a write that may have landed. Its answer was stored with the pick, so the key holds that answer though its caller heard none, and a resend under the key gets it once the daemon is back and has staged the move. A slot that joined the owner while the Test ran moves with it. A pass is still refused: a group or source that is gone (`404`), one no slot follows (`422 ownerIdle`), a server it doesn't use (`422 notMember`) or that left its source (`422 serverGone`), a server whose revision changed or whose core restarted during the Test (`422 serverChanged`), and one that failed since the Test began (`422 failedSince`). The owner's last word stands: a press whose Test began before the Test of the last press on the same group or source to land began, or before the last Clear that cleared its preference landed, moves nothing and is `412 stale`, at a revision that shows what the later word left. So presses count in the order they were pressed, whichever lands first. A Clear that cleared nothing is no word, and a slot that moves to another group or source takes no word with it. A server the core can't run is `404 notTestable` before any Test.

The move gives the server a boost, the gap between its score and the best member's as the Test left them, so it ties for first in `order`. It fades to nothing over 24 hours, slowly at first: three quarters is left after 12 hours. Order and comparison rounds read the server's score less what is left, so a faster rival needs less to win as time passes; failure, broken UDP and severe slowness read raw values. A press on a server whose UDP stands broken moves the slots as an Exit change onto it does, and the broken UDP's confirmation begins with the move, or once the WAN is proven when it isn't yet, so a rival whose UDP works takes them on with `udpMove`. A press on a server already first, or on the only one, moves the slots and holds nothing. A press on the server in use moves nothing and boosts it when it isn't first. A second press on the preferred server keeps its deadline, and a press on another replaces the preference.

`preferred` on a `GroupView` and a `SourceView` is `{ server, endsAt }` while a preference holds and a slot whose Exit is that group or source is given, in the same view, as applied on that server, at any revision of it, else null. A slot is given as applied on a server while its `status` is `serving`, its `pending` is false, and its `appliedServer` is its `server`: what its box draws as the server it is on. So the owner's `preferred` and its boxes never disagree. It is null at the press's own revision, where the move is staged, while every slot still lands, its gate closed or open, on a read that says nothing of what the core applied, and once no slot of the owner is on that server, as when the only slot on it changes its Exit. Any move off that server ends it: a failover reads `failureMove`, a lower ping `improvementMove`, never `preferred`. A new revision of the same server keeps it, and the view names it once a slot reports the new revision. It also ends at `endsAt`, within 2 s, and when a slot's Exit changes away from the owner, and a daemon restart keeps it with its deadline. `DELETE api/groups/{id}/preferred/{server}?endsAt=...` clears it only while it names that server with that `endsAt`; otherwise it changes nothing. Either way it answers `{}` at a new revision, which brings the page what stands, and it never moves a slot. The page's own press is how it knows a Test is out: the view says nothing of one.

### Updates

`GET api/update` is all there is to know of them, and both POSTs answer with the same `UpdateView`:

```ts
{
  version: "v0.1.0",            // what runs, or "dev-…"
  releases: true,               // false on a development build: the rest is null, with no check and no update
  guide: "https://…",           // docs/operate.md of the release that runs, which says how to recover over SSH
  check: { at, failure },       // the last check that ended. failure is null, unreachable, clockWrong, filesUnavailable (404) or malformed
  offered: { version, restartsCore, notes } | null,   // a newer release, and its page
  running: { attempt, version } | null,               // the update that runs, or is being launched, is queued or is being stopped
  last: { attempt, version, at, outcome, reason, message } | null,   // the last that ended, pressed at `at`. null while one runs
}
```

**The check.** `{ "auto": true }` is the page's own, sent when its app shell loads, on whichever tab. The daemon fetches only if `checkForUpdates` is on and no check started in the last 24 hours, or none since an update that ended `offerChanged`, and else answers what it knows. `{ "auto": false }` is a press and always fetches. Overlapping checks are one fetch. Its start is stored before it, so a restart in the middle counts, and a failure keeps what was offered. A check answers in 15 s at most, and a check that finds nothing is `200` with its `failure`.

A check is one `GET <releases>/latest/download/sidegate.json` over HTTPS to the address the build was linked with, at most 5 redirects and 64 KiB, with `User-Agent: sidegated` and no other header of its own. It tells GitHub the gateway's WAN address, that sidegate runs behind it, and when the page was opened or the check pressed. It names neither the version that runs nor anything that identifies the gateway. An update also tells it which release is downloaded and when. A page that is never opened sends nothing, and with `checkForUpdates` off neither does one that is opened.

**The update.** `POST api/update` names what its confirm said, the offered `version` and `restartsCore`, or it is refused with `offerChanged`. The daemon makes the attempt's id and stores it before it starts the job. One update runs at a time: a press while one runs, of whatever version, is answered with it in `running`, and a press after it ended is a new attempt. The page sends a press once, never again by itself but where its token was refused, which reached nothing, and reads `api/update` for what became of it, through the daemon's restart, where reads fail for a few seconds. A `503` or a lost answer may have started it. A job systemd didn't start is a `202` whose `last` is `failed`, `noStart`.

| `outcome` | What it left |
|---|---|
| `landed` | The new release, which the session's `release` and `version` then name |
| `landedWithError` | The new release, and a later step, such as removing the old one, failed |
| `refused`, `failed` | The old release, untouched. Only `failed` has a `reason` |
| `rolledBack` | The old release, put back, and it answers |
| `putBackFailed` | Neither for sure: recovery needed over SSH, as `guide` says. A recovery over SSH isn't an attempt, so this stays the last one until the next |

| `reason` | Cause |
|---|---|
| `noStart`, `stopped` | The job never ran, or ended before it said how |
| `interrupted` | It was cut in its switch, and that is still to be put back, or the old release put back is still to say it answers |
| `moved` | Another release is current than the one the press updated from |
| `offerChanged` | Another release is the latest now. It isn't offered until the next check, which the next load makes, within the day too |
| `notNewer`, `planChanged` | The release isn't above the one that runs, or the core's restart isn't as confirmed |
| `noRoom` | `/data` has less than three times the bundle's size and 64 MiB free |
| `downloadFailed` | The manifest or the bundle couldn't be had |
| `checksum` | The bundle's size or SHA-256 isn't the manifest's |
| `wrongRelease` | The bundle's own `sidegated` says another version or releases address |

`message` is a sentence for the log, as an error's is.

The whole answer is one moment's: a read while an update is launched or ends, over SSH too, never joins one's `last` to another's state, and an update that was launched is in `running` until it ends. `interrupted` isn't an update's last word: the same attempt reads how it ended once the recovery, or the old release's daemon, has said.

### UniFi's key

`PUT api/unifi/key` replaces the Network API key with `{ "key": "..." }`, one line, spaces around it dropped. The daemon tries the key on UniFi with one read before storing it, so a key UniFi refuses is `422 keyRefused` and one whose try UniFi didn't answer is `422 notAnswering`, each at the current revision, and the stored key stays and keeps working. An accepted key is written root-only where `bootstrap.json` names it, or to `network-key` beside `state.db`, which `bootstrap.json` then names; it takes over at once, with no restart, and the answer's revision is a view with no `unifiTrouble`. No route returns the key or any part of it, and no log line, refusal or event carries it.

### Xray's version

`core.version` in `api/state` is the release's pinned Xray and `core.running` the one running, each as its binary says, such as `26.10.31`. Either is null while unknown, `running` while no core runs. They differ after an upgrade whose core restart didn't happen, until the next upgrade restarts it. The daemon reads each core run's version once, in the background, so a binary that doesn't answer holds nothing up and leaves its version null. No route restarts the core.

`unifi.trouble` on a slot is what the last pass that read UniFi couldn't do for its VPN Client: `nameTaken` when another network holds the slot's name, so its client keeps the old name or has none; `limitReached` when UniFi's limit left no room to create it; `refused` when UniFi refused the write; null otherwise. A client kept after its slot went is a row of `vpnClients.foreign` whose `trouble` is `routed` while Policy Table routes still point at it, or `refused` when UniFi refused its delete. A client UniFi deleted is `client: "deleted"`, never a trouble. Each problem's words and error go to the daemon's log.

### Health and the week

A server's `health` reads its record, which outlives restarts and refreshes: its last result and when (`current` while that is under 10 minutes old), the median and p95 of its pings and new connections to the ranking site whatever their age, and `pingAt` the newest of those samples (a heartbeat measures no ping, so this can be older than the last result). `score` is its ping plus jitter times `factor`, in ms, null with no ping to the ranking site. `factor` is one plus its expected incidents per day: 1.25 unknown, down toward about 1.02 as it is seen working, up with each incident, every part halving in a week. `incidents7d` counts its outages in the week. `seenDays` is its days seen working, by heartbeat while it serves or by passing a comparison, halving in a week like the rest: what pays `factor` down. `seenWorking7dMs` is how long its checks found it up in the last 7 days, not decayed: the time between two passes with no failure between them, at most 30 minutes a pass, so neither the time since it was added nor the time it was checked. It is null until the record has kept it for the whole week, or since the server was first seen, which a record written before the field was kept does from its next result. `lastFailureAt` is when its newest failure began, an outage's start or a slowness move off it, whatever its age, and `firstSeenAt` when the daemon first saw the server, by a refresh that listed it or its first result. A group's and a source's `moves7d` counts its forced moves (failure, slowness, UDP) once per move, and `firstChoice` is rank 1 of its `order`. A group's `order` is its members, and a source's its present servers, by class (passed or never probed, then recovering, then failed), each by score, and those it left in the last 5 minutes last. A `preferred` server's score counts less what is left of its boost, and it wins a tie. An analytics bucket's `servedSeconds` is how long slots used the server in it, each from its own readback of the move onto it, and `incidents` the failure and slowness moves off it. Its `probeAttempts` and `probeSuccesses` count the heartbeats of the server while a slot served it, one a minute and one every 10 s under suspicion, and `wanExcluded` and `unknown` those that could judge nothing. Its pings come from the full probes: checks, rounds and Tests.

### What a slot uses

One rule covers groups and sources. A `DELETE` of a group a slot uses is `409 referenced`, and so is a `DELETE` of a source when a slot's Exit is the source, one of its servers, or a group the delete would remove. The message names the slots, and `usedBy` on each lists them before the delete is asked: on a group, the slots whose Exit is the group; on a source, every slot its delete would be refused for, by the source, a server or a group, so the page draws each as a row to repoint. A source's `removes` names the groups its delete would remove with it. Deleting a source leaves its servers as tombstones that keep their `label`, `name`, `country`, `protocol` and `transport`, so history still names them, and nothing they were reached by. The 2xx says the delete landed. What it removed leaves state.db's free space then, or, when a page read in progress or a full disk holds that up, within a minute of it clearing. A tombstone leaves `api/servers` once it was deleted longer ago than `analyticsRetentionDays`, with no result since, no sample analytics can still show, and nothing else still names it. It rewrites the groups that name it and removes one whose sources condition named only it, because an unset condition means any source, and one that is `"box": "ticked"` alone and loses all its ticks. A `PUT` of a group naming a source that is gone drops that source, and a rule left with none is `422`, so the owner chooses. An Exit naming a missing group or a tombstoned server is `422`.

## Events

`api/events` is a stream of `id:` and `data:` frames, where the data is a `ChangeEvent`. A quiet gateway sends nothing. `kind` says which reads went stale: `slot`, `switch`, `source`, `group`, `catalogue`, `settings` or `analytics`. An unknown kind, or `resync: true`, makes the page fetch everything again.

The page runs its own retry: every failure closes the stream, and a new one opens after a wait that doubles up to 30 s. After any failure it fetches everything again when the next connection opens, so it doesn't rely on `Last-Event-ID`.
