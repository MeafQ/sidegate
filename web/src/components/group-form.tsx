import { useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { useBlocker, useLocation, useNavigate } from "react-router"
import { AlertDialog, AlertDialogCancel, AlertDialogContent, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { SaveActions } from "@/components/page"
import { NOT_SET, type Among, type FieldValue, type Pattern, type RuleKey } from "@/components/filter-chips"
import { NameEdit } from "@/components/name-edit"
import { Quiet } from "@/components/status"
import { actions, ApiError, errorWords, newId, type ItemWay } from "@/lib/api"
import { useGroupPreview, useSettled } from "@/lib/queries"
import { bare, conditioned, isMember } from "@/lib/membership"
import { isPending, noAnswer, useControl, useControls, useForm } from "@/lib/use-write"
import type { Form } from "@/lib/write"
import { cn } from "@/lib/utils"
import type { Box, GroupCreate, GroupId, GroupInput, GroupView, LineageId, Members, ServerView, View } from "@/lib/schema"

export const NO_MEMBERS: Members = { sources: { not: false, values: [] }, countries: NOT_SET, protocols: { not: false, values: [] }, include: "", exclude: "", box: null }

/** A group as its tab edits it: its name and each filter of its rule, a field each, so a conflict names the filters that moved. A
 * set's values are kept sorted, since their order means nothing. */
type GroupDraft = { name: string; pattern: Pattern; country: Among; source: Among; protocol: Among; box: Box }
const sorted = ({ not, values }: Among): Among => ({ not, values: values.toSorted() })
const draftOf = (g: GroupView): GroupDraft => ({
  name: g.name, pattern: { include: g.members.include, exclude: g.members.exclude }, country: sorted(g.members.countries), source: sorted(g.members.sources),
  protocol: sorted(g.members.protocols), box: g.members.box,
})
const membersOf = (d: GroupDraft): Members => ({
  sources: d.source as Members["sources"], countries: d.country as Members["countries"], protocols: d.protocol as Members["protocols"],
  include: d.pattern.include, exclude: d.pattern.exclude, box: d.box,
})
const BLANK: GroupDraft = draftOf({ name: "", members: NO_MEMBERS } as GroupView)
/** A filter is named in a conflict by its column, never by its chips. */
const FILTER_WORDS: Record<Exclude<keyof GroupDraft, "name">, string> = { pattern: "Name filter", country: "Country filter", source: "Source filter", protocol: "Protocol filter", box: "Selected filter" }

/** A group's rule as one form of the write path, over what is stored. Its name is in the form's body as stored: a rename is a
 * single control of its own. */
const groupForm = (id: GroupId, what: string): Form<GroupDraft, GroupInput, GroupView, ItemWay> => ({
  action: actions.group(id),
  way: "save",
  what,
  blank: BLANK,
  stored: (data) => {
    const group = (data as View | undefined)?.groups.find((g) => g.id === id)
    return group ? { value: draftOf(group), revision: group.revision } : null
  },
  body: (shown) => ({ name: shown.name.trim(), members: membersOf(shown) }),
  print: (field, value) => (field === "name" ? `Name ${value as string}` : FILTER_WORDS[field]),
  shows: () => false,
})

const sameJson = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b)
const RULE: Exclude<keyof GroupDraft, "name">[] = ["pattern", "country", "source", "protocol", "box"]

/** A box whose tick is out: whether it has been out a second, when it stands at half strength. */
export type Busy = { readonly slow: boolean }

/** What the table under a group needs: its rule's filters, what ends their line, whether each server's row stands dimmed, whether
 * it is ticked, whether its box is busy, what a click on its box does, and where its rows stand. */
export type GroupShown = {
  fields: FieldValue<RuleKey>[]
  /** Save and Discard while the draft differs, and how many servers are in. */
  end: ReactNode
  /** Outside the group, once anything is chosen: a new group's draft with no condition and no tick dims none. */
  dimmed: (id: ServerView["id"]) => boolean
  isTicked: (id: ServerView["id"]) => boolean
  busy: (id: ServerView["id"]) => Busy | undefined
  tick: (server: ServerView) => void
  /** The box's control, which a toast about its tick reports on. */
  control: (id: ServerView["id"]) => string
  placed: Placed
}

/** Where a group's rows stand: its members in the daemon's order, then the rest. A tick saves at once and can change who is in, so
 * the rows keep the places the rule's last saved revision gave them, and no box moves its row. */
export type Placed = { members: ReadonlySet<string>; order: GroupView["order"] }

function usePlaced(group: GroupView): Placed {
  const [placed, setPlaced] = useState(() => ({ revision: group.revision, members: new Set<string>(group.resolved), order: group.order }))
  if (placed.revision !== group.revision) setPlaced({ revision: group.revision, members: new Set(group.resolved), order: group.order })
  return placed
}

/** A stored group's settings, above its servers. A server is in when every set condition matches it, the box among them. */
export function GroupDetails({ group, view, servers, heading, naming, onNamed, below }: {
  group: GroupView
  view: View
  /** Null until the server list is read, when nothing counts them. */
  servers: ServerView[] | null
  /** `held`: a write of the group is out, so its Delete waits. */
  heading: (name: ReactNode, held: boolean) => ReactNode
  /** Just made, so its name opens ready to type over. */
  naming: boolean
  onNamed: () => void
  below: (shown: GroupShown) => ReactNode
}) {
  const now = view.groups.find((g) => g.id === group.id) ?? group
  const stored = draftOf(now)
  const form = useForm(groupForm(group.id, stored.name))
  const out = form.phase === "out"
  const draft = form.shown
  const set = (patch: Partial<GroupDraft>) => form.edit(patch)
  const members = membersOf(draft)
  const otherEdits = RULE.some((k) => !sameJson(draft[k], stored[k]))
  /* The rename is a single control, as a slot's is: the name over the group as stored, apart from the rule's draft. On a conflict
   * the name goes back to what is stored, and Keep mine renames over the group as it is then. */
  const rename = useControl(actions.group(group.id), "rename")
  const pressRename = (name: string) => rename.press({ name, members: now.members }, {
    what: stored.name, precondition: { kind: "replace", revision: now.revision },
    conflict: (data) => {
      const then = (data as View).groups.find((g) => g.id === group.id)
      if (!then) return null
      return { kind: "control", theirs: then.name, mine: name, again: { pressed: { name, members: then.members }, precondition: { kind: "replace", revision: then.revision } } }
    },
  })
  const { refused, matched } = useRuleRead(members, group.resolved)
  const canSave = form.dirty && !out && refused === null
  /* The rename, the rule's Save and the group's Delete are three ways into its one write: each spins only for its own press. */
  const save = () => { if (canSave) form.press() }
  /* A box shows the daemon's tick, or the one sent while its write is out. A tick saves at once, apart from the rule's Save. */
  const boxes = useControls()
  const boxOf = (id: ServerView["id"]) => boxes.shown(actions.tick(group.id, id))
  const isTicked = (id: ServerView["id"]) => boxOf(id).sent?.ticked ?? group.ticked.includes(id)
  const ticked = new Set((servers ?? []).map((s) => s.id).filter(isTicked))
  const busy = (id: ServerView["id"]): Busy | undefined => {
    const box = boxOf(id)
    return isPending(box.phase) ? { slow: box.slow } : undefined
  }
  const tick = (server: ServerView) => boxes.press(actions.tick(group.id, server.id), { ticked: !isTicked(server.id) }, { what: server.label })
  const placed = usePlaced(group)
  const isIn = (id: ServerView["id"]) => isMember(id, matched, members.box, ticked)
  return (
    <div className="flex flex-1 flex-col gap-4" data-control={actions.group(group.id).control}>
      {heading(
        /* A rename saves at once, like a slot's, and carries none of the rule's edits along. */
        <NameEdit shown={rename.sent?.name || stored.name} start={stored.name} startNaming={naming} pending={isPending(rename.phase) && rename.way === "rename"} waits={isPending(rename.phase)}
          onFinish={(name) => {
            onNamed()
            const typed = name?.trim()
            if (typed && typed !== stored.name) pressRename(typed)
          }} />,
        out,
      )}
      {below({
        dimmed: (id) => !isIn(id), isTicked, busy, tick, placed, control: (id) => actions.tick(group.id, id).control,
        fields: ruleFields(draft, set, refused),
        end: <RuleEnd open={otherEdits} busy={form.way === "save"} held={out} canSave={canSave} onDiscard={form.discard} onSave={save} count={refused === null && servers !== null ? heldCount(servers, isIn) : null} />,
      })}
    </div>
  )
}

/** Go alone runs the patterns, so the page asks it which servers the rule's conditions but the box match. A keystroke abandons
 * the read before it. Until Go first answers, `before` stands in for that answer. A pattern Go can't read is the one refusal a
 * rule's field takes: its chips ringed, their editor saying why. A rule with no condition holds no server, so it waits for no
 * answer: the last rule's never stands for it. */
function useRuleRead(members: Members, before: readonly string[]) {
  const asked = useSettled(members)
  const preview = useGroupPreview(asked)
  const none = !conditioned(members)
  const refused = !none && preview.state === "failed" && preview.error instanceof ApiError && preview.error.kind === "validation" ? errorWords(preview.error) : null
  const last = preview.state === "waiting" ? null : preview.matched
  const matched = useMemo(() => new Set<string>(none ? [] : last ?? before), [none, last, before])
  return { refused, matched }
}

/** A rule's filters, one field each, in the columns' order; `held` while they take no edit. */
const ruleFields = (draft: GroupDraft, set: (patch: Partial<GroupDraft>) => void, refused: string | null, held = false): FieldValue<RuleKey>[] => [
  { kind: "pattern", key: "name", value: draft.pattern, refusal: refused, held, onChange: (pattern) => set({ pattern }) },
  { kind: "set", key: "country", value: draft.country, held, onChange: (value) => set({ country: sorted(value) }) },
  { kind: "set", key: "source", value: draft.source, held, onChange: (value) => set({ source: sorted(value) }) },
  { kind: "set", key: "protocol", value: draft.protocol, held, onChange: (value) => set({ protocol: sorted(value) }) },
  { kind: "one", key: "box", value: draft.box, held, onChange: (box) => set({ box: box as Box }) },
]

/** How many servers the rule holds now, none gone. */
const heldCount = (servers: ServerView[], isIn: (id: ServerView["id"]) => boolean) => servers.filter((s) => s.presence !== "tombstone" && isIn(s.id)).length

/** What ends a rule's chip line: how many servers the rule holds, which a refused pattern leaves out, and before it, while there is
 * something to save, Discard while there is something to put back, and Save. The count alone stays on the line. */
function RuleEnd({ open, busy, held, canSave, onDiscard, onSave, count }: { open: boolean; busy: boolean; held: boolean; canSave: boolean; onDiscard?: () => void; onSave: () => void; count: number | null }) {
  if (!open && count === null) return null
  return (
    <div className={cn("ml-auto flex min-w-0 flex-wrap items-center justify-end gap-x-3 gap-y-2", open && "max-sm:basis-full")}>
      <SaveActions open={open} busy={busy} held={held} canSave={canSave} onDiscard={onDiscard} onSave={onSave} />
      {count !== null && <Quiet className="ml-auto shrink-0 tabular-nums">{count} in group</Quiet>}
    </div>
  )
}

/** The address of a new group before its Save. */
export const NEW_GROUP = "/groups/new"

/** A new group's draft: its name, its rule's filters and the servers ticked in it. It lives on the page alone until Save.
 * `byTick`: Box is ticked was set by the first tick, so the last box unticked takes it away while it is the only condition. */
type NewDraft = GroupDraft & { ticked: LineageId[]; byTick: boolean }

/** A new group as one form of the write path, on the draft's own control. Nothing is stored until Save, which sends the name, the
 * rule and the ticks in one create at an id the store picks; once the view has the group its last Save made, the form reads it as
 * stored. */
const newGroupForm = (opening: string, blank: NewDraft, what: string): Form<NewDraft, GroupCreate, GroupView> => ({
  action: actions.createGroup(opening),
  what,
  blank,
  stored: (data, made) => {
    const group = (data as View | undefined)?.groups.find((g) => g.id === made)
    return group ? { value: { ...draftOf(group), ticked: group.ticked, byTick: false }, revision: group.revision } : null
  },
  body: (shown) => ({ name: shown.name.trim(), members: membersOf(shown), ticked: shown.ticked }),
  /* A create meets no conflict: a 412 there is the create, landed earlier. */
  print: () => null,
  shows: () => false,
})

/** The first "New group" name no group has, nor any of `kept`. */
function freeName(groups: readonly GroupView[], kept: readonly string[] = []) {
  const taken = new Set([...groups.map((g) => g.name), ...kept].map((name) => name.toLowerCase()))
  let name = "New group"
  for (let n = 2; taken.has(name.toLowerCase()); n++) name = `New group ${n}`
  return name
}

/** A group before its Save: the group's own heading and table, holding nothing stored, so the list and the Exit menu don't have it.
 * Its name opens ready to type over. Its ticks are the draft's, a field of their own as a stored group's are: the first, on a draft
 * with no condition set, sets Box is ticked, since a hand-picked group is that condition alone, and no filter's edit moves a tick.
 * Save, once the draft could hold a server, makes the group in one write and opens its page. After no answer it sends the same
 * create while the draft holds what it sent; a draft that holds something else makes a new group at an id of its own, as an edited
 * Add source is a new add, and the last Save's group opens. Discard puts the filters and ticks back as the page opened and keeps
 * the name, as a stored group's Discard puts its rule back, on a create of its own: a Save from before it opens nothing. Leaving
 * drops the draft, and asks first once anything was typed or ticked. */
export function NewGroupDetails({ view, servers, heading, below }: {
  view: View
  servers: ServerView[]
  heading: (name: ReactNode) => ReactNode
  below: (shown: GroupShown) => ReactNode
}) {
  const navigate = useNavigate()
  const [free] = useState(() => freeName(view.groups))
  /* The draft's create, and the name it opens with: a Discard begins a new one under the name typed. */
  const [begun, setBegun] = useState(() => ({ opening: newId(), name: free }))
  const opening = begun.opening
  const blank: NewDraft = { ...BLANK, name: begun.name, ticked: [], byTick: false }
  /* Its toasts name the group as typed. */
  const [what, setWhat] = useState(free)
  const form = useForm(newGroupForm(opening, blank, what))
  const out = form.phase === "out"
  const draft = form.shown
  const members = membersOf(draft)
  const ticked = new Set<string>(draft.ticked)
  const { refused, matched } = useRuleRead(members, NOTHING_MATCHED)
  const canSave = !out && refused === null && !bare(members, ticked.size)
  const isIn = (id: ServerView["id"]) => isMember(id, matched, members.box, ticked)
  const chosen = conditioned(members) || ticked.size > 0
  /* Box is ticked set by a tick goes with the last box unticked while it is the only condition, so the draft is fresh again. One
   * the owner set or changed in Filter stays as set. */
  const tickedAlone = draft.byTick && members.box === "ticked" && !conditioned({ ...members, box: null })
  /* The name, the rule and the ticks go in the create, so the draft takes no edit while it is out: the group it opens holds all
   * the draft showed. */
  const change = (patch: Partial<NewDraft>) => {
    if (!out) form.edit(patch)
  }
  const tick = (server: ServerView) => {
    const on = !ticked.has(server.id)
    const next = on ? [...draft.ticked, server.id].toSorted() : draft.ticked.filter((l) => l !== server.id)
    if (on && ticked.size === 0 && !conditioned(members)) change({ ticked: next, box: "ticked", byTick: true })
    else change(next.length === 0 && tickedAlone ? { ticked: next, box: null, byTick: false } : { ticked: next })
  }
  const edit = (patch: Partial<NewDraft>) => change("box" in patch ? { ...patch, byTick: false } : patch)
  const made = !out && form.made !== null && view.groups.some((g) => g.id === form.made) ? form.made : undefined
  useEffect(() => { if (made !== undefined) navigate(`/groups/${made}`, { replace: true }) }, [made, navigate])
  const edited = RULE.some((k) => !sameJson(draft[k], blank[k])) || draft.ticked.length > 0
  /* Leaving asks while the name differs from the one the page opened with, not the one a Discard kept, or a filter or a tick is set. */
  const typed = edited || draft.name !== free
  /* A landed Save leaves without asking. */
  const blocker = useBlocker(({ currentLocation, nextLocation }) =>
    typed && !out && nextLocation.pathname !== currentLocation.pathname && nextLocation.pathname !== `/groups/${form.made}`)
  const leave = () => {
    form.discard()
    if (blocker.state === "blocked") blocker.proceed()
  }
  /* Discard stands while there is a filter or a tick to put back, or a Retry to take. */
  const offered = edited || form.unheard
  const discard = () => {
    form.discard()
    setBegun({ opening: newId(), name: draft.name })
  }
  return (
    <div className="flex flex-1 flex-col gap-4" data-control={actions.createGroup(opening).control}>
      {heading(
        <NameEdit shown={draft.name} start={draft.name} startNaming waits={out}
          onFinish={(name) => {
            const typed = name?.trim()
            if (out || !typed || typed === draft.name) return
            change({ name: typed })
            setWhat(typed)
          }} />,
      )}
      {below({
        dimmed: (id) => chosen && !isIn(id), isTicked: (id) => ticked.has(id), busy: () => (out ? CREATING : undefined), tick, placed: NOTHING_PLACED,
        control: () => actions.createGroup(opening).control,
        fields: ruleFields(draft, edit, refused, out),
        end: <RuleEnd open busy={out} held={out} canSave={canSave} onDiscard={offered ? discard : undefined} count={refused === null ? heldCount(servers, isIn) : null}
          onSave={() => { if (canSave) form.press() }} />,
      })}
      <AlertDialog open={blocker.state === "blocked"} onOpenChange={(open) => { if (!open && blocker.state === "blocked") blocker.reset() }}>
        <AlertDialogContent>
          <AlertDialogHeader className="place-items-start text-left">
            <AlertDialogTitle>Discard {draft.name}?</AlertDialogTitle>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <Button variant="destructive" onClick={leave}>Discard</Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

const NOTHING_MATCHED: readonly string[] = []
/** A draft's box while its create is out: it takes no tick. */
const CREATING: Busy = { slow: false }
const NOTHING_PLACED: Placed = { members: new Set(), order: [] }

/** Save as group: a group of All servers' filters, made at once and named for what's free. One control of the write path, so a
 * second make waits for the first, and the store picks its id. A make pressed again over the filters of the one whose fate is
 * unknown is that make, sent again with the name it was sent with under its key, though another writer has taken the name since.
 * The group of the last make opens once the view has it, unless the user has gone elsewhere since the click: they stay there, and
 * the toast's Open goes to it. The shell owns the one instance, so where the user is when the answer lands is never read by a page
 * that has gone. */
export function useGroupAdding() {
  const { key } = useLocation()
  const here = useRef(key)
  useEffect(() => { here.current = key }, [key])
  /* The names of the makes whose fate was unknown when the next was pressed, which the next default name passes, so two groups
   * made on purpose have two names. */
  const [unsure, setUnsure] = useState<readonly string[]>([])
  const controls = useControls()
  const make = controls.shown(actions.newGroup)
  const create = (groups: readonly GroupView[], members: Members) => {
    const kept = make.kept
    const again = kept !== null && sameJson(kept.members, members)
    const names = kept !== null && !again && noAnswer(make.phase) ? [...unsure, kept.name] : unsure
    const name = again ? kept.name : freeName(groups, names)
    const clicked = here.current
    setUnsure(names)
    controls.press(actions.newGroup, { name, members, ticked: [] }, {
      what: name, precondition: { kind: "create" },
      opens: (made) => ({ route: `/groups/${made}`, here: here.current === clicked, state: { naming: true } }),
    })
  }
  return { create, creating: make.phase === "out" }
}
