import { useEffect, useRef, useState, type ReactNode } from "react"
import { useNavigate } from "react-router"
import { ChevronRightIcon, CircleXIcon, EyeIcon, EyeOffIcon, LinkIcon, ListIcon, RotateCcwIcon, XIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupInput, InputGroupTextarea } from "@/components/ui/input-group"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { Textarea } from "@/components/ui/textarea"
import { ToggleGroupItem } from "@/components/ui/toggle-group"
import { NameEdit } from "@/components/name-edit"
import { failedWords } from "@/components/page"
import { CHOSEN, NumberChoices } from "@/components/pickers"
import { MarkIcon, Quiet } from "@/components/status"
import { actions, newId, type ItemWay } from "@/lib/api"
import { useNow } from "@/lib/clock"
import { counted, DEFAULT_REFRESH, refreshWords } from "@/lib/format"
import { useSourceUrl, useView } from "@/lib/queries"
import type { SourceId, SourceInput, SourceView, View } from "@/lib/schema"
import { readSourceText, type SourceText } from "@/lib/source-text"
import { USER_AGENTS } from "@/lib/user-agents"
import { expiryOf, refusalWords, trafficOf, type REFUSAL_WORDS } from "@/lib/troubles"
import { isPending, noAnswer, useControl, useForm } from "@/lib/use-write"
import type { Form, FormView } from "@/lib/write"
import { cn } from "@/lib/utils"

const REFRESH_CHOICES = [3600, 3 * 3600, 6 * 3600, 12 * 3600, 86_400]
const REFRESH_OFF = "off"
const UA_CUSTOM = "custom"
type Remote = Extract<SourceView["origin"], { kind: "remote" }>

/** Marks the refresh interval a subscription gets by default. */
const DefaultMark = () => <span className="size-1.5 rounded-full bg-current" aria-hidden />

/** The one field's mark, in its corner: a link for a URL, a list with the count of share links for pasted servers. Its label and
 * title say what it read. */
function TextMark({ typed }: { typed: SourceText }) {
  if (typed.kind !== "url" && typed.kind !== "pasted") return null
  const label = typed.kind === "url" ? "URL" : typed.links === null ? "Pasted text" : counted(typed.links, "link")
  return (
    <span role="img" aria-label={label} title={label} className="text-muted-foreground absolute right-2.5 bottom-2 flex items-center gap-1 text-[0.625rem] tabular-nums [&_svg]:size-3.5">
      {typed.kind === "url" ? <LinkIcon aria-hidden /> : <><ListIcon aria-hidden />{typed.links}</>}
    </span>
  )
}

/** A section that stays out of the way until asked for. */
function Fold({ label, open, disabled = false, onOpenChange, children }: { label: ReactNode; open: boolean; disabled?: boolean; onOpenChange: (open: boolean) => void; children: ReactNode }) {
  return (
    <Collapsible open={open} onOpenChange={onOpenChange} disabled={disabled} className="flex flex-col gap-3">
      <CollapsibleTrigger asChild>
        <Button variant="ghost" size="sm" className="text-muted-foreground hover:text-foreground -ml-2.5 w-fit">
          <ChevronRightIcon className={cn("transition-transform", open && "rotate-90")} aria-hidden />{label}
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent>{children}</CollapsibleContent>
    </Collapsible>
  )
}

type UrlCheck = { kind: "empty" } | { kind: "ok"; url: string } | { kind: "invalid"; reason: string }

/** A source URL as typed: surrounding spaces dropped, then an https address with a host, or why not. */
function checkUrl(text: string): UrlCheck {
  const trimmed = text.trim()
  if (!trimmed) return { kind: "empty" }
  if (!/^https:\/\//i.test(trimmed)) return { kind: "invalid", reason: "Must start with https://" }
  if (!URL.canParse(trimmed) || !new URL(trimmed).hostname) return { kind: "invalid", reason: "No host" }
  return { kind: "ok", url: trimmed }
}

/** How often a subscription is fetched, one field: zero follows the default, and paused is Off. */
type Refresh = { seconds: number; paused: boolean }
/** What a subscription's fetching takes, in the Add dialog and in a stored source's settings alike. */
type Fetching = { userAgent: string; refresh: Refresh }

const nameInUse = (view: View | undefined, name: string, self: SourceId | null) =>
  name.trim() !== "" && view?.sources.some((s) => s.id !== self && s.name.trim().toLowerCase() === name.trim().toLowerCase()) === true

/** The refresh interval: the provider's default marked, or else the daemon's fixed 6 h, Off for paused. Zero takes the default, so
 * its button is the one on, and choosing its button takes the default again. */
function RefreshField({ id, value, provider, disabled, refused = null, onChange }: { id: string; value: Refresh; provider: number | null; disabled: boolean; refused?: string | null; onChange: (refresh: Refresh) => void }) {
  const fallback = provider ?? DEFAULT_REFRESH
  const shown = value.seconds === 0 ? fallback : value.seconds
  return (
    <Field>
      <FieldLabel>Refresh<Quiet className="inline-flex items-center gap-1 font-normal"><DefaultMark />Default</Quiet></FieldLabel>
      <NumberChoices label="Refresh" offered={REFRESH_CHOICES} kept={[fallback, shown]} value={value.paused ? REFRESH_OFF : String(shown)} disabled={disabled}
        invalid={refused !== null} describedBy={refused !== null ? `${id}-error` : undefined}
        word={(s) => <>{refreshWords(s)}{s === fallback && <DefaultMark />}</>}
        title={(s) => (s !== fallback ? undefined : provider !== null ? "Default: the provider's interval" : "Default")}
        onPick={(v) => onChange(v === REFRESH_OFF ? { ...value, paused: true } : { paused: false, seconds: Number(v) === fallback ? 0 : Number(v) })}>
        <ToggleGroupItem className={CHOSEN} value={REFRESH_OFF} aria-invalid={refused !== null || undefined}>Off</ToggleGroupItem>
      </NumberChoices>
      {refused !== null && <FieldError id={`${id}-error`}>{refused}</FieldError>}
    </Field>
  )
}

/** The User-Agent: a known one from the list, or Custom with its own field. */
function UserAgentField({ id, value, disabled, refused = null, onChange }: { id: string; value: string; disabled: boolean; refused?: string | null; onChange: (userAgent: string) => void }) {
  const [custom, setCustom] = useState(false)
  const choice = custom || !USER_AGENTS.includes(value) ? UA_CUSTOM : value
  return (
    <Field>
      <FieldLabel htmlFor={id}>User-Agent</FieldLabel>
      <Select value={choice} disabled={disabled} onValueChange={(v) => { if (v === UA_CUSTOM) { setCustom(true) } else { setCustom(false); onChange(v) } }}>
        <SelectTrigger id={id} aria-label="User-Agent" className={cn("w-full", choice !== UA_CUSTOM && "font-mono")} aria-invalid={(refused !== null && choice !== UA_CUSTOM) || undefined}
          aria-describedby={refused !== null && choice !== UA_CUSTOM ? `${id}-error` : undefined}><SelectValue /></SelectTrigger>
        <SelectContent>
          {USER_AGENTS.map((ua) => <SelectItem key={ua} value={ua} className="font-mono">{ua}</SelectItem>)}
          <SelectItem value={UA_CUSTOM}>Custom</SelectItem>
        </SelectContent>
      </Select>
      {choice === UA_CUSTOM && <Input className="font-mono" aria-label="Custom User-Agent" value={value} placeholder="App/1.0" readOnly={disabled} onChange={(e) => onChange(e.target.value)}
        aria-invalid={refused !== null || undefined} aria-describedby={refused !== null ? `${id}-error` : undefined} />}
      {refused !== null && <FieldError id={`${id}-error`}>{refused}</FieldError>}
    </Field>
  )
}

/** The dialog a source's settings open in, to add one or to change one. While its write is out nothing closes it, Cancel, ×,
 * Escape and a click outside alike, so nothing that looks like stopping the write can be pressed. Its description is for a
 * screen reader. */
function SourceDialog({ title, description, held, onClose, children }: { title: string; description: string; held: boolean; onClose: () => void; children: ReactNode }) {
  const stay = (e: Event) => { if (held) e.preventDefault() }
  return (
    <Dialog open onOpenChange={(o) => { if (!o) onClose() }}>
      <DialogContent showCloseButton={!held} onEscapeKeyDown={stay} onInteractOutside={stay} className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-2xl">
        {held && <Button variant="ghost" size="icon-sm" className="absolute top-2 right-2" disabled><XIcon /><span className="sr-only">Close</span></Button>}
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription className="sr-only">{description}</DialogDescription>
        </DialogHeader>
        {children}
      </DialogContent>
    </Dialog>
  )
}

/** The last state of a write with no answer, under the field it was for: the no-answer toast's mark and its words. */
export const NoAnswer = ({ id }: { id?: string }) => (
  <p id={id} className="text-muted-foreground flex items-center gap-1.5 text-xs/relaxed"><MarkIcon mark="danger" />Request timed out</p>
)

// ───── a stored source ─────

/** A stored source's settings as one form: what its dialog holds, and its name as stored, since a rename from the heading is a
 * single control of its own. Its URL is null while it keeps what is stored, unseen, and the text that replaces it otherwise: fixed
 * as typed, never by a later read. Empty text keeps what is stored. */
type SourceDraft = Fetching & { name: string; url: string | null; text: string }

const storedDraft = (source: SourceView): SourceDraft => {
  const remote = source.origin.kind === "remote" ? source.origin : null
  return {
    name: source.nameSet ? source.name : "", url: null, text: "",
    userAgent: remote?.userAgent ?? USER_AGENTS[0], refresh: { seconds: remote?.refreshEverySeconds ?? 0, paused: remote?.paused ?? false },
  }
}

/** A source's conflict names each touched field that moved; a URL and pasted text are never printed. */
function printSource(field: keyof SourceDraft, value: SourceDraft[keyof SourceDraft]): string | null {
  switch (field) {
    case "url": case "text": return null
    case "name": return `Name ${value as string}`
    case "userAgent": return `User-Agent ${value as string}`
    case "refresh": {
      const { seconds, paused } = value as Refresh
      return `Refresh ${paused ? "off" : seconds === 0 ? "default" : refreshWords(seconds)}`
    }
  }
}

function sourceForm(source: SourceView): Form<SourceDraft, SourceInput, SourceView, ItemWay> {
  const kind = source.origin.kind
  return {
    action: actions.source(source.id),
    way: "save",
    what: source.name,
    blank: storedDraft(source),
    stored: (data) => {
      const stored = (data as View | undefined)?.sources.find((s) => s.id === source.id)
      return stored ? { value: storedDraft(stored), revision: stored.revision } : null
    },
    body: (shown) => {
      if (kind === "manual") return { name: shown.name, origin: { kind, text: shown.text.trim() ? shown.text : null } }
      const url = checkUrl(shown.url ?? "")
      return { name: shown.name, origin: { kind, url: url.kind === "ok" ? url.url : null, userAgent: shown.userAgent, refreshEverySeconds: shown.refresh.seconds, paused: shown.refresh.paused } }
    },
    print: printSource,
    /* The daemon's refusal names no field. The page's draft does: a write that changed one field was refused for it, and stands
     * under it; one that changed several is a toast, and the form stays as typed. */
    shows: (_, changed) => changed.size === 1,
    fieldOf: (_, changed) => (changed.size === 1 ? [...changed][0] : undefined),
    resentWhy: "changed since this Save",
  }
}

/** A rename's body: the name over the source as stored, every other field kept. */
const renamed = (source: SourceView, name: string): SourceInput => sourceForm(source).body({ ...storedDraft(source), name }, null)

/** A stored source's tab: its heading around its name, and its table. Either can open the settings, and the table can open them at
 * pasted links' Servers field, which is empty since the daemon never sends pasted text back. */
type Tab = {
  /** `held`: a write of the source is out, so its Delete waits. */
  heading: (name: ReactNode, openSettings: () => void, held: boolean) => ReactNode
  below: (openSettings: () => void, pasteAgain: () => void) => ReactNode
}

/** The stored URL carries a token, so it is a read of its own, asked only when the reader asks to see it. */
function UrlReveal({ shown, reveal, read, disabled, onReveal }: { shown: boolean; reveal: boolean; read: ReturnType<typeof useSourceUrl>; disabled: boolean; onReveal: (on: boolean) => void }) {
  if (reveal && read.isError && !read.isFetching) {
    const words = failedWords(read.error)
    return (
      <InputGroupButton size="icon-xs" aria-label={`URL: ${words}. Retry`} title={words} disabled={disabled} onClick={() => void read.refetch()}><CircleXIcon /></InputGroupButton>
    )
  }
  const reading = reveal && read.isFetching
  return (
    <InputGroupButton size="icon-xs" aria-label={shown ? "Hide URL" : "Show URL"} title={shown ? "Hide URL" : "Show URL"} disabled={reading || disabled} aria-busy={reading || undefined}
      onClick={() => onReveal(!shown)}>
      {reading ? <Spinner role={undefined} aria-label={undefined} aria-hidden /> : shown ? <EyeOffIcon /> : <EyeIcon />}
    </InputGroupButton>
  )
}

/** One opening of a source's settings, as its tab keeps it: where it opened, and the control's `saved` count then. */
type Opening = { readonly at: "all" | "servers"; readonly saved: number }

/** What one opening draws, from the store's form. `unheard`: its Save ended with no answer and is kept, so "Request timed out" and
 * its Retry. `sending`: its Save's attempts are out, unanswered, and nothing closes it, 30 s at most. `waiting`: a write of the source
 * is out and its Save isn't sending, so it is read-only and closes as ever. */
type Drawn = "editing" | "unheard" | "sending" | "waiting"

function drawn(form: FormView<SourceDraft, ItemWay>): Drawn {
  if (form.phase === "out") return form.sending && form.way === "save" ? "sending" : "waiting"
  return form.unheard ? "unheard" : "editing"
}

/** One opening of a source's settings: the form, and what this opening alone keeps, Advanced, the eye and whether URL text was typed
 * here. Its tab draws it only until a Save of its draft lands, so nothing in it runs after that. */
function SourceSettings({ source, at, onClose }: { source: SourceView; at: Opening["at"]; onClose: () => void }) {
  const form = useForm(sourceForm(source))
  const state = drawn(form)
  const out = form.phase === "out"
  const remote: Remote | null = source.origin.kind === "remote" ? source.origin : null
  const pasted = source.origin.kind === "manual"
  const [advanced, setAdvanced] = useState(false)
  /* The source's revision the eye was turned on at: once the page sees the source change, the eye is off. */
  const [eye, setEye] = useState<number | null>(null)
  /* URL text typed in this opening shows as typed; text kept from an earlier opening is dots until the eye shows it. */
  const [typed, setTyped] = useState(false)
  const draft = form.shown
  const shows = eye === source.revision && !out
  const read = useSourceUrl(source.id, source.revision, remote !== null && shows && draft.url === null)
  const set = (patch: Partial<SourceDraft>) => form.edit(patch)
  const url = checkUrl(draft.url ?? "")
  /* The one field a refused Save changed, where its refusal stands until the next edit. */
  const refusedAt = form.refusal?.field ?? null
  const refusal = (field: keyof SourceDraft) => (refusedAt === field && form.refusal !== null ? refusalWords(form.refusal) : null)
  const urlReason = url.kind === "invalid" ? url.reason : refusal("url")
  const canSave = form.dirty && !out && form.refusal === null && url.kind !== "invalid" && draft.userAgent.trim() !== ""
  /* With no edit since a Save got no answer, Save is that Save's Retry: its key, its bytes and the revision it was sent at. */
  const save = () => {
    if (!canSave) return
    if (form.retries) form.retry()
    else form.press()
  }
  /* Closing drops what wasn't saved, with the Save's Retry when it had no answer. A rename or Delete out meanwhile is left alone. */
  const close = () => {
    form.discard()
    onClose()
  }
  const shownUrl = draft.url === null ? (shows ? (read.data ?? "") : "") : typed || shows ? draft.url : ""
  /* Emptied, the stored URL is kept and masked again; put back as the eye showed it, it is kept too. */
  const typeUrl = (text: string) => {
    if (text === "") setEye(null)
    if (text === "" || (read.data !== undefined && text === read.data)) return set({ url: null })
    setTyped(true)
    set({ url: text })
  }
  const f = `source-${source.id}`
  return (
    <SourceDialog title="Settings" description={source.name} held={state === "sending"} onClose={close}>
      <div className="flex flex-col gap-4">
        <FieldGroup className="gap-4">
          {remote && (
            <div className="grid items-end gap-4 sm:grid-cols-2 lg:grid-cols-3">
              <Field>
                <FieldLabel htmlFor={`${f}-url`}>URL</FieldLabel>
                <InputGroup>
                  <InputGroupInput id={`${f}-url`} type="url" className="font-mono" autoComplete="off" value={shownUrl} placeholder="••••••••••••••••" readOnly={out}
                    aria-invalid={urlReason !== null || undefined} aria-describedby={urlReason !== null ? `${f}-url-error` : undefined}
                    onChange={(e) => typeUrl(e.target.value)} />
                  {(draft.url === null || !typed) && (
                    <InputGroupAddon align="inline-end">
                      <UrlReveal shown={shows} reveal={shows && draft.url === null} read={read} disabled={out} onReveal={(on) => setEye(on ? source.revision : null)} />
                    </InputGroupAddon>
                  )}
                </InputGroup>
                {urlReason !== null && <FieldError id={`${f}-url-error`}>{urlReason}</FieldError>}
              </Field>
              <RefreshField id={`${f}-refresh`} value={draft.refresh} provider={remote.provider.suggestedRefreshSeconds} disabled={out} refused={refusal("refresh")} onChange={(refresh) => set({ refresh })} />
            </div>
          )}
          {remote && (
            <Fold label="Advanced" open={advanced || refusedAt === "userAgent"} disabled={out} onOpenChange={setAdvanced}>
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                <UserAgentField id={`${f}-ua`} value={draft.userAgent} disabled={out} refused={refusal("userAgent")} onChange={(userAgent) => set({ userAgent })} />
              </div>
            </Fold>
          )}
          {pasted && (
            <Field>
              <FieldLabel htmlFor={`${f}-text`}>Servers</FieldLabel>
              <Textarea id={`${f}-text`} autoFocus={at === "servers"} className="max-h-48 font-mono" value={draft.text} placeholder="Unchanged" spellCheck={false} readOnly={out}
                aria-invalid={refusal("text") !== null || undefined} aria-describedby={refusal("text") !== null ? `${f}-text-error` : undefined}
                onChange={(e) => set({ text: e.target.value })} />
              {refusal("text") !== null && <FieldError id={`${f}-text-error`}>{refusal("text")}</FieldError>}
            </Field>
          )}
        </FieldGroup>
        {state === "unheard" && (
          <div className="flex items-center gap-2">
            <NoAnswer id={`${f}-no-answer`} />
            <Button size="sm" variant="outline" onClick={form.retry}><RotateCcwIcon />Retry</Button>
          </div>
        )}
        {remote?.fetch.updatedAt && <ProviderReports quota={remote.provider.quota} />}
        <div className="flex items-center justify-end gap-2">
          <Button variant="outline" disabled={state === "sending"} onClick={close}>Cancel</Button>
          <Button disabled={!canSave} aria-busy={form.way === "save" || undefined} onClick={save}>{form.way === "save" && <Spinner role={undefined} aria-label={undefined} aria-hidden />}Save</Button>
        </div>
      </div>
    </SourceDialog>
  )
}

/* The heading's rename is a single control, as a slot's is, whose refusal and conflict are toasts. The source's Delete is a third
 * way into its one write, beside the settings' Save, so each spins only for its own press and waits while another is out. */
function StoredSource({ source, tab }: { source: SourceView; tab: Tab }) {
  const view = useView()
  const rename = useControl(actions.source(source.id), "rename")
  const pressRename = (name: string) => rename.press(renamed(source, name), {
    what: source.name, precondition: { kind: "replace", revision: source.revision },
    conflict: (data) => {
      const then = (data as View).sources.find((s) => s.id === source.id)
      if (!then) return null
      return { kind: "control", theirs: then.name, mine: name, again: { pressed: renamed(then, name), precondition: { kind: "replace", revision: then.revision } } }
    },
  })
  const [opening, setOpening] = useState<Opening | null>(null)
  const openSettings = (at: Opening["at"] = "all") => setOpening({ at, saved: rename.saved })
  const base = storedDraft(source)
  const pasted = source.origin.kind === "manual"
  const nameControl = (
    <NameEdit lines={3} shown={rename.sent?.name || source.name} start={base.name} placeholder={source.name} pending={isPending(rename.phase) && rename.way === "rename"} waits={isPending(rename.phase)}
      refuse={(text) => (nameInUse(view.data, text, source.id) ? "Name in use" : null)}
      onFinish={(name) => {
        const typed = name?.trim()
        /* A subscription's name may be cleared, to follow its provider again; pasted servers keep one. */
        if (typed !== undefined && typed !== base.name && (typed !== "" || !pasted)) pressRename(typed)
      }} />
  )
  return (
    <div className="flex flex-1 flex-col gap-4">
      {tab.heading(nameControl, () => openSettings(), rename.phase === "out")}
      {tab.below(() => openSettings(), () => openSettings("servers"))}
      {/* An opening a Save of its draft has landed for is drawn no more, so it closes in the render that sees the landing. */}
      {opening !== null && opening.saved === rename.saved && <SourceSettings source={source} at={opening.at} onClose={() => setOpening(null)} />}
    </div>
  )
}

/** What the heading leaves out, so the owner knows the page won't warn of what it can't see: an expiry or traffic the provider
 * doesn't report. An expiry a year or more away is shown nowhere: providers send one as their "never". */
function ProviderReports({ quota }: { quota: Extract<SourceView["origin"], { kind: "remote" }>["provider"]["quota"] }) {
  const now = useNow()
  const unreported = [expiryOf(quota.expires, now).kind === "unreported" && "expiry", trafficOf(quota, now).kind === "unreported" && "data limit"].filter((w) => w !== false)
  return unreported.length > 0 && <div data-slot="provider-reports" className="text-muted-foreground text-xs/relaxed">Provider doesn't report {unreported.join(" or ")}</div>
}

/** A source's tab: its heading, then its servers. Its settings open in a dialog. */
export function SourceDetails({ sub, heading, below }: { sub: SourceView; heading: Tab["heading"]; below: Tab["below"] }) {
  return <StoredSource key={sub.id} source={sub} tab={{ heading, below }} />
}

// ───── Add source ─────

/** What Add source holds: the one field, a pasted source's name, and a subscription's fetching under Advanced. */
type AddDraft = Fetching & { text: string; name: string }
const BLANK_ADD: AddDraft = { text: "", name: "", userAgent: USER_AGENTS[0], refresh: { seconds: 0, paused: false } }

/** The field each of the daemon's refusals of an add is about. */
const ADD_FIELDS: Partial<Record<keyof typeof REFUSAL_WORDS, keyof AddDraft>> = {
  textMissing: "text", urlMissing: "text", notHttps: "text", unreadableText: "text", noUsableServers: "text",
  nameMissing: "name", badUserAgent: "userAgent", badRefresh: "refresh",
}

/** The add as sent: a subscription from one https URL, else the text as pasted. */
function addBody(shown: AddDraft): SourceInput {
  const typed = readSourceText(shown.text)
  if (typed.kind === "url") return { name: "", origin: { kind: "remote", url: typed.url, userAgent: shown.userAgent, refreshEverySeconds: shown.refresh.seconds, paused: shown.refresh.paused } }
  return { name: shown.name, origin: { kind: "manual", text: shown.text } }
}

/** One opening of Add source. Its control is the opening's own, so its draft, its "Request timed out" and its toast stay with it
 * while the id it adds at, the store's, changes. A key belongs to the whole request as sent: an Add of what the add with no answer sent
 * sends that request under its key, so an add that landed is found, not made twice; an Add of anything else is a new add, at a new
 * id under a new key. Answered, it closes onto the new source, and so does the last Add pressed when it lands later,
 * sent again by its toast's Retry or found by a read. An earlier Add that lands is a source of its own, and the dialog stays with
 * the last. */
function AddSource({ opening, onClose }: { opening: string; onClose: () => void }) {
  const navigate = useNavigate()
  const view = useView()
  const form = useForm<AddDraft, SourceInput, SourceView>({
    action: actions.addSource(opening), what: "Source", blank: BLANK_ADD,
    stored: () => null, body: addBody, print: () => null, shows: () => true, fieldOf: (reason) => ADD_FIELDS[reason as keyof typeof REFUSAL_WORDS],
  })
  const out = form.phase === "out"
  const [advanced, setAdvanced] = useState(false)
  const draft = form.shown
  const typed = readSourceText(draft.text)
  const pasted = typed.kind === "pasted"
  const taken = pasted && nameInUse(view.data, draft.name, null)
  const added = !out && form.made !== null && view.data?.sources.some((s) => s.id === form.made) === true ? form.made : undefined
  useEffect(() => {
    if (added === undefined) return
    onClose()
    navigate(`/sources/${added}`)
  })
  const canAdd = !out && form.refusal === null && (typed.kind === "url" ? draft.userAgent.trim() !== "" : pasted && draft.name.trim() !== "" && !taken)
  const add = () => { if (canAdd) form.press() }
  const problem = typed.kind === "refused" ? typed.reason : form.refusal ? refusalWords(form.refusal) : null
  /* With no answer the field is the reader's again, in focus. */
  const field = useRef<HTMLTextAreaElement>(null)
  const unanswered = noAnswer(form.phase)
  useEffect(() => { if (unanswered) field.current?.focus() }, [unanswered])
  const f = `add-${opening}`
  return (
    <SourceDialog title="Add source" description="A URL, or servers pasted as links or text" held={out} onClose={() => { form.discard(); onClose() }}>
      <div className="flex flex-col gap-4">
        <FieldGroup className="gap-4">
          <Field>
            <InputGroup className="relative">
              <InputGroupTextarea ref={field} id={`${f}-source`} aria-label="Source" className={cn("read-only:text-muted-foreground max-h-48 pr-10 font-mono", draft.text !== "" && "min-h-0")} value={draft.text} placeholder={"https://…\nvless:// ss:// trojan:// …"} spellCheck={false}
                readOnly={out} aria-invalid={problem !== null || undefined}
                aria-describedby={problem !== null ? `${f}-source-error` : unanswered ? `${f}-no-answer` : undefined}
                onChange={(e) => form.edit({ text: e.target.value })} />
              <TextMark typed={typed} />
            </InputGroup>
            {problem !== null && <FieldError id={`${f}-source-error`}>{problem}</FieldError>}
            {problem === null && unanswered && <NoAnswer id={`${f}-no-answer`} />}
          </Field>
          {pasted && (
            <Field className="max-w-60">
              <FieldLabel htmlFor={`${f}-name`}>Name</FieldLabel>
              <Input id={`${f}-name`} className="read-only:text-muted-foreground" value={draft.name} readOnly={out} aria-invalid={taken || undefined} aria-describedby={taken ? `${f}-name-error` : undefined}
                onChange={(e) => form.edit({ name: e.target.value })} />
              {taken && <FieldError id={`${f}-name-error`}>Name in use</FieldError>}
            </Field>
          )}
          {typed.kind === "url" && (
            <Fold label="Advanced" open={advanced} disabled={out} onOpenChange={setAdvanced}>
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                <UserAgentField id={`${f}-ua`} value={draft.userAgent} disabled={out} onChange={(userAgent) => form.edit({ userAgent })} />
                <div className="sm:col-span-2"><RefreshField id={`${f}-refresh`} value={draft.refresh} provider={null} disabled={out} onChange={(refresh) => form.edit({ refresh })} /></div>
              </div>
            </Fold>
          )}
        </FieldGroup>
        <div className="flex items-center justify-end gap-2">
          <Button variant="outline" disabled={out} onClick={() => { form.discard(); onClose() }}>Cancel</Button>
          <Button disabled={!canAdd} aria-busy={out || undefined} onClick={add}>{out && <Spinner role={undefined} aria-label={undefined} aria-hidden />}Add</Button>
        </div>
      </div>
    </SourceDialog>
  )
}

/** Adding a source: what opens one, and its dialog. A second can't open meanwhile, since the dialog is modal. */
export function useSourceAdding() {
  const [opening, setOpening] = useState<string | null>(null)
  const open = () => setOpening((o) => o ?? newId())
  const dialog = opening !== null && <AddSource key={opening} opening={opening} onClose={() => setOpening(null)} />
  return { open, dialog }
}
