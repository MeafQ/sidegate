import { useEffect, useRef, useState, type ReactNode } from "react"
import { ExternalLinkIcon, RouterIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field"
import { InputGroup, InputGroupInput } from "@/components/ui/input-group"
import { Fact } from "@/components/card"
import { ButtonSpinner, FailedMark, Loaded, useRead } from "@/components/page"
import { UNIFI_PART } from "@/components/app-shell"
import { NumberChoices } from "@/components/pickers"
import { NoAnswer } from "@/components/source-details"
import { Quiet, TONE_TEXT, Toned } from "@/components/status"
import { SidegateFact, VersionLine, VersionWaiting } from "@/components/update"
import { UNIFI_SETTINGS, actions } from "@/lib/api"
import { useNow } from "@/lib/clock"
import { ago, every } from "@/lib/format"
import { settingsRead, updateRead, useView } from "@/lib/queries"
import type { Settings, SettingsView, View } from "@/lib/schema"
import { UNIFI_TROUBLES, unifiStanding, type UnifiStanding } from "@/lib/troubles"
import { isPending, noAnswer, useControl, useForm, useUnsaved } from "@/lib/use-write"
import type { Form } from "@/lib/write"
import { cn } from "@/lib/utils"

/** A Save on its control's line, 8px after it, as every card that saves has it. */
const SAVE_ROW = "flex items-center gap-2"

// ───── UniFi ─────

/** What the heading's router says: UniFi's trouble in its colour, green once UniFi answered the key, faint before anything is known. */
function routerTone(standing: UnifiStanding | null): string {
  if (standing?.kind === "trouble") return TONE_TEXT[UNIFI_TROUBLES[standing.trouble].tone]
  return standing?.kind === "answered" ? "text-success" : "text-muted-foreground/40"
}

/** The card's top line, as a slot box's: the router where the box has its dot, the name, UniFi's trouble in its colour, and Open in
 * UniFi at the end. While the key has worked since the daemon started, the trouble's words open a card with when it last did. */
function UnifiHeading({ view }: { view: View | undefined }) {
  const now = useNow()
  const standing = view ? unifiStanding(view) : null
  const trouble = standing?.kind === "trouble" ? UNIFI_TROUBLES[standing.trouble] : null
  const lastWorked = standing?.kind === "trouble" ? standing.lastWorked : null
  return (
    <div className="flex h-6 items-center gap-2">
      <RouterIcon aria-hidden className={cn("size-4 shrink-0", routerTone(standing))} />
      <CardTitle>UniFi</CardTitle>
      {trouble && (lastWorked
        ? <Fact lines={[{ name: "Last worked", words: ago(lastWorked, now) }]} side="right" level className="-my-0.5 justify-center"><Toned tone={trouble.tone}>{trouble.words}</Toned></Fact>
        : <Toned tone={trouble.tone}>{trouble.words}</Toned>)}
      <a href={UNIFI_SETTINGS} target="_blank" rel="noreferrer" className="text-muted-foreground hover:text-foreground ml-auto shrink-0" aria-label="Open UniFi" title="Open in UniFi"><ExternalLinkIcon className="size-3.5" aria-hidden /></a>
    </div>
  )
}

const KEY_FIELD = "unifi-key"
/** The refusals the key's field draws itself: UniFi refused the key it was sent, or didn't answer its try. */
const KEY_REFUSALS = new Set(["keyRefused", "notAnswering"])

/** The field that replaces the key. It is write-only: a stored key is the placeholder's dots, since no route returns it, and a pasted
 * key shows plain until it is saved, so a paste cut short shows and the browser offers no password to save over UniFi's login. Its
 * mono is compact at every width, so a key shows both its ends on a phone, and it stops at 320px, so Save stays beside it. The
 * daemon tries it on UniFi before storing it: a refusal keeps the text, in the invalid field, until it changes; UniFi's silence is
 * this press's amber line, and Save tries again. */
/** The key field's text: as typed, or as the last press sent it while the field still holds it, with the control's landings before
 * that press, so a landing since empties the field. */
type KeyDraft = { readonly kind: "typed"; readonly text: string } | { readonly kind: "sent"; readonly text: string; readonly landings: number }

function KeyField({ view }: { view: View | undefined }) {
  const write = useControl(actions.unifiKey)
  const [draft, setDraft] = useState<KeyDraft>({ kind: "typed", text: "" })
  const pending = isPending(write.phase)
  const reason = pending ? null : write.refusal?.reason ?? null
  /* A save that landed empties the field back to the dots. Any other ending keeps the text, a refusal only a toast says included. */
  const landed = draft.kind === "sent" && write.landings > draft.landings
  const text = landed ? "" : draft.text
  useUnsaved(text.trim() !== "")
  /* The field holds the key its last press sent until it is typed over or a landing empties it: only then are that key's refusal
   * and UniFi's silence over it lines under the field. */
  const holds = draft.kind === "sent" && !landed
  const refused = reason === "keyRefused" && holds
  const silent = reason === "notAnswering" && holds
  const line = refused || silent ? `${KEY_FIELD}-line` : write.phase === "lost" ? `${KEY_FIELD}-no-answer` : undefined
  const stored = view !== undefined && view.unifiTrouble !== "noKey"
  /* A refusal is the field's to draw only while the field that pressed is mounted and holds the key sent, when it comes and when
   * the view has caught up with it: a toast's Retry may send it after another was typed, or Settings may be left while the view
   * catches up, and then it is the toast's. */
  const now = useRef<KeyDraft | null>(draft)
  useEffect(() => {
    now.current = draft
    return () => { now.current = null }
  })
  const press = () => {
    const sent = text
    setDraft({ kind: "sent", text: sent, landings: write.landings })
    write.press(sent.trim(), {
      what: "API key",
      shows: (refusal) => KEY_REFUSALS.has(refusal.reason) && now.current?.kind === "sent" && now.current.text === sent,
    })
  }
  return (
    <Field>
      <FieldLabel htmlFor={KEY_FIELD}>API key</FieldLabel>
      <div className={SAVE_ROW}>
        <InputGroup className={cn("min-w-0 max-w-80 flex-1", pending && "text-muted-foreground")}>
          <InputGroupInput id={KEY_FIELD} type="text" autoComplete="off" autoCapitalize="off" spellCheck={false} className="font-mono text-xs/relaxed"
            placeholder={stored ? "••••••••••••••••" : undefined} value={text} readOnly={pending}
            aria-invalid={refused || undefined} aria-describedby={line ? `${KEY_FIELD}-help ${line}` : `${KEY_FIELD}-help`} onChange={(e) => setDraft({ kind: "typed", text: e.target.value })} />
        </InputGroup>
        <Button disabled={pending || text.trim() === "" || refused} aria-busy={pending || undefined} data-control={actions.unifiKey.control} onClick={press}>
          {pending && <ButtonSpinner />}Save
        </Button>
      </div>
      <FieldDescription id={`${KEY_FIELD}-help`}>Generate in UniFi Network under Settings → Control Plane → Integrations.</FieldDescription>
      {refused && <FieldError id={line}>{UNIFI_TROUBLES.keyRefused.words}</FieldError>}
      {silent && <FieldError id={line} className="text-warning">{UNIFI_TROUBLES.notAnswering.words}</FieldError>}
      {line === `${KEY_FIELD}-no-answer` && <NoAnswer id={line} />}
    </Field>
  )
}

function UnifiPart({ view }: { view: View | undefined }) {
  return (
    <Card id={UNIFI_PART} className="scroll-mt-4">
      <CardHeader><UnifiHeading view={view} /></CardHeader>
      <CardContent><KeyField view={view} /></CardContent>
    </Card>
  )
}

// ───── Analytics ─────

/** How long analytics are kept, in days, as a source's Refresh offers its intervals. */
const RETENTION_CHOICES = [7, 30, 90, 365]
const retentionWord = (days: number) => (days === 365 ? "1y" : every(days * 86_400))

type Retention = { days: number }

/** The settings as one form of the write path: Retention's pick over what is stored. A pick doesn't save by itself, since a slip
 * from 1y to 7d would prune what is kept. */
const SETTINGS: Form<Retention, Settings, SettingsView> = {
  action: actions.settings,
  what: "Settings",
  blank: { days: 30 },
  stored: (data) => {
    const settings = data as SettingsView | undefined
    return settings === undefined ? null : { value: { days: settings.analyticsRetentionDays }, revision: settings.revision }
  },
  body: (shown) => ({ analyticsRetentionDays: shown.days }),
  print: (_, value) => `Retention ${retentionWord(value)}`,
  shows: () => false,
}

function AnalyticsCard({ under, children }: { under?: ReactNode; children: ReactNode }) {
  return (
    <Card>
      <CardHeader><CardTitle>Analytics</CardTitle></CardHeader>
      <CardContent>
        <Field>
          <FieldLabel>Retention</FieldLabel>
          <div className={SAVE_ROW}>{children}</div>
          {under}
        </Field>
      </CardContent>
    </Card>
  )
}

function AnalyticsForm() {
  const form = useForm(SETTINGS)
  const out = form.phase === "out"
  return (
    <AnalyticsCard under={noAnswer(form.phase) && <NoAnswer />}>
      <NumberChoices label="Retention" offered={RETENTION_CHOICES} kept={[form.shown.days]} value={String(form.shown.days)} disabled={out}
        word={retentionWord} onPick={(v) => form.edit({ days: Number(v) })} />
      <Button disabled={!form.dirty || out} aria-busy={out || undefined} data-control={actions.settings.control} onClick={() => form.press()}>
        {out && <ButtonSpinner />}Save
      </Button>
    </AnalyticsCard>
  )
}

/** The form while the settings are on their way: the choices and Save drawn, neither taking a press. */
function AnalyticsWaiting() {
  return (
    <AnalyticsCard>
      <NumberChoices label="Retention" offered={RETENTION_CHOICES} kept={[]} value="" disabled word={retentionWord} onPick={() => {}} />
      <Button disabled>Save</Button>
    </AnalyticsCard>
  )
}

// ───── About ─────

const versionWord = (version: string) => `v${version}`

/** Xray as About shows it: the release's version not read; the running core's not read, since no core runs or its read hasn't
 * answered; the release running; or another version running, an older one an upgrade left. */
type Core = { kind: "unknown" } | { kind: "runningUnknown" } | { kind: "same"; version: string } | { kind: "older"; running: string; version: string }

function coreOf({ version, running }: View["core"]): Core {
  if (version === null) return { kind: "unknown" }
  if (running === null) return { kind: "runningUnknown" }
  return running === version ? { kind: "same", version } : { kind: "older", running, version }
}

const CHECK_FIELD = "check-for-updates"

/** Whether the page checks for a newer release: the stock box, saved at once, apart from Retention's Save, as a group's tick is. It
 * shows what is stored, or what it sent while its write is out. A busy box takes no click, and after a second stands at half
 * strength, so nothing unconfirmed looks stored. A refusal or no answer is its toast. While the settings are read it is drawn and
 * takes no click; a failed read is the cross in its place. */
function CheckForUpdatesBox() {
  const settings = useRead(settingsRead)
  const write = useControl(actions.checkForUpdates)
  const busy = isPending(write.phase)
  const stored = settings.state === "read" ? settings.data.checkForUpdates : null
  const on = write.sent?.checkForUpdates ?? stored ?? false
  return (
    <Field orientation="horizontal" className="gap-2">
      {settings.state === "failed"
        ? <FailedMark failed={settings} what="Check for updates" className="size-4 min-h-0 justify-center" />
        : <Checkbox id={CHECK_FIELD} checked={on} disabled={stored === null} aria-disabled={busy || undefined} aria-busy={(busy && write.slow) || undefined}
            data-control={actions.checkForUpdates.control} className={cn(busy && write.slow && "opacity-50")}
            onCheckedChange={() => { if (!busy) write.press({ checkForUpdates: !on }, { what: "Check for updates" }) }} />}
      <FieldLabel htmlFor={CHECK_FIELD} className="font-normal">Check for updates</FieldLabel>
    </Field>
  )
}

/** Xray and the version that runs, always plain. An older core is its version, an arrow, and the release's muted, the value known
 * but not in effect, with nothing offered for it. A version not read is the muted dash alone. */
function XrayFact({ view }: { view: View | undefined }) {
  if (view === undefined) return <VersionWaiting />
  const core = coreOf(view.core)
  if (core.kind === "unknown") return <Quiet aria-label="Version not known">—</Quiet>
  if (core.kind === "runningUnknown") return <Quiet aria-label="Running version not known">—</Quiet>
  if (core.kind === "same") return <span className="tabular-nums">{versionWord(core.version)}</span>
  return <VersionLine running={versionWord(core.running)} next={versionWord(core.version)} heard=", the release ships " />
}

function About({ view }: { view: View | undefined }) {
  const build = useRead(updateRead)
  /* A development build has no releases to check for. */
  const checks = build.state !== "read" || build.data.releases
  return (
    <Card>
      <CardHeader><CardTitle>About</CardTitle></CardHeader>
      <CardContent className="flex flex-col gap-3">
        <div className="flex flex-col gap-1"><div className="flex flex-col"><Quiet>sidegate</Quiet><SidegateFact /></div>{checks && <CheckForUpdatesBox />}</div>
        <div className="flex flex-col"><Quiet>Xray</Quiet><XrayFact view={view} /></div>
      </CardContent>
    </Card>
  )
}

export function SettingsPage() {
  const view = useView()
  return (
    <>
      <UnifiPart view={view.data} />
      <Loaded query={settingsRead} skeleton={<AnalyticsWaiting />}>
        {() => <AnalyticsForm />}
      </Loaded>
      <About view={view.data} />
    </>
  )
}
