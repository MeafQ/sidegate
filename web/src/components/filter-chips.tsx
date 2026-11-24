import { useState, type ReactNode } from "react"
import { ChevronDownIcon, ListFilterIcon, PlusIcon, XIcon, type LucideIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { FieldError } from "@/components/ui/field"
import { ButtonGroup } from "@/components/ui/button-group"
import { Input } from "@/components/ui/input"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { Checklist, Drawn, type Drawing, type FacetOption } from "@/components/facet"
import { CHOSEN } from "@/components/pickers"
import { cn } from "@/lib/utils"

/** One saved condition. Is any of nothing isn't set. Is none of nothing is, and passes everything. */
export type Among = { not: boolean; values: string[] }
export const NOT_SET: Among = { not: false, values: [] }

/** What a name must match and what it must not. An empty side isn't set. */
export type Pattern = { include: string; exclude: string }

/** A condition as compared with another: order doesn't matter. */
export const sameness = <T,>({ not, values }: { not: boolean; values: T[] }) => ({ not, values: values.toSorted() })
const conditionSet = ({ not, values }: { not: boolean; values: unknown[] }) => not || values.length > 0

/** Every filter the server table offers. Each opens from its column's header, and all from the chip line's Filter button. `box` is
 * a group's box column: whether each server is ticked in the group. */
export type FieldKey = "name" | "status" | "country" | "source" | "protocol" | "box"
/** What a group's rule can hold. A status is only how a server is now, so no rule saves one. */
export type RuleKey = "name" | "country" | "source" | "protocol" | "box"

type SetValue<K> = { kind: "set"; key: K; value: Among; held?: boolean; onChange: (value: Among) => void }
/** One value or none, as a box condition is. */
type OneValue<K> = { kind: "one"; key: K; value: string | null; held?: boolean; onChange: (value: string | null) => void }
/** `refusal`: why the daemon can't read the pattern, said in its editor, its chips ringed. */
type PatternValue<K> = { kind: "pattern"; key: K; value: Pattern; refusal?: string | null; held?: boolean; onChange: (value: Pattern) => void }
/** A filter as a tab holds it: a set of values, one value, or a pattern pair. The table adds the rest from the column it filters.
 * `held`: it takes no edit now, as a new group's while its create is out, so its header, its chips and their × are disabled. */
export type FieldValue<K extends FieldKey = FieldKey> = SetValue<K> | OneValue<K> | PatternValue<K>

/** A filter as the table offers it: its label, and a set's or a one's values drawn as its column's cells draw them. */
export type FilterField =
  | SetValue<FieldKey> & { label: string; options: FacetOption[] }
  | OneValue<FieldKey> & { label: string; options: FacetOption[] }
  | PatternValue<FieldKey> & { label: string }

type Side = keyof Pattern
const SIDE_WORDS: Record<Side, string> = { include: "matches", exclude: "doesn't match" }

/** Is none of nothing excludes nothing, so it says it lets everything through until a value is picked. */
const verbOf = ({ not, values }: Among) => (not ? (values.length === 0 ? "is any" : values.length === 1 ? "is not" : "is none of") : values.length > 1 ? "is any of" : "is")

function isSet(field: FieldValue): boolean {
  switch (field.kind) {
    case "set": return conditionSet(field.value)
    case "one": return field.value !== null
    case "pattern": return field.value.include !== "" || field.value.exclude !== ""
  }
}
export const anySet = (fields: FieldValue[]) => fields.some(isSet)

/** A set's tick list under "is any of / is none of". */
function SetEditor({ field }: { field: Extract<FilterField, { kind: "set" }> }) {
  const { label } = field
  return (
    <div className="flex flex-col gap-2">
      <ToggleGroup type="single" variant="outline" size="sm" spacing={0} aria-label={`${label} condition`} value={field.value.not ? "none" : "any"}
        onValueChange={(v) => { if (v) field.onChange({ ...field.value, not: v === "none" }) }}>
        <ToggleGroupItem className={CHOSEN} value="any">is any of</ToggleGroupItem>
        <ToggleGroupItem className={CHOSEN} value="none">is none of</ToggleGroupItem>
      </ToggleGroup>
      <div className="-mx-2.5 -mb-2.5">
        <Checklist facet={field.key} label={label} options={field.options} value={field.value.values} onChange={(values) => field.onChange({ ...field.value, values })} onClear={() => field.onChange(NOT_SET)} />
      </div>
    </div>
  )
}

/** A one-value filter's values, picked as every set's are: a stock checkbox per value, its words and its count, with no search and
 * no "is any of / is none of", since there are two. One picked is the condition; both or neither is none, and both stay ticked
 * while the list is open. The checkbox is the value's drawing here, so the words stand alone; its chip draws the value whole. */
function OneEditor({ field }: { field: Extract<FilterField, { kind: "one" }> }) {
  const [both, setBoth] = useState(false)
  const every = field.options.map((o) => o.value)
  const picked = field.value !== null ? [field.value] : both ? every : []
  const options = field.options.map((o) => ({ ...o, drawing: { words: o.drawing.words } }))
  const pick = (values: string[]) => {
    setBoth(values.length > 1)
    field.onChange(values.length === 1 ? values[0] : null)
  }
  return (
    <div className="-mx-2.5 -mb-2.5">
      <Checklist facet={field.key} label={field.label} options={options} value={picked} onChange={pick} searched={false} />
    </div>
  )
}

/** The two-way toggle every set filter has, matches or doesn't match, above one pattern field. Turning it carries the pattern
 * across; where a saved rule holds both, it shows the other side's own. */
function PatternEditor({ field }: { field: Extract<FilterField, { kind: "pattern" }> }) {
  const { label, value, onChange } = field
  const [side, setSide] = useState<Side>(value.include === "" && value.exclude !== "" ? "exclude" : "include")
  const turn = (to: Side) => {
    if (to === side) return
    setSide(to)
    if (value[to] === "") onChange({ ...value, [to]: value[side], [side]: "" })
  }
  return (
    <div className="flex flex-col gap-2">
      <ToggleGroup type="single" variant="outline" size="sm" spacing={0} aria-label={`${label} condition`} value={side} onValueChange={(v) => { if (v) turn(v as Side) }}>
        <ToggleGroupItem className={CHOSEN} value="include">{SIDE_WORDS.include}</ToggleGroupItem>
        <ToggleGroupItem className={CHOSEN} value="exclude">{SIDE_WORDS.exclude}</ToggleGroupItem>
      </ToggleGroup>
      <Input className="h-7 font-mono" aria-label={`${label} ${SIDE_WORDS[side]}`} placeholder="Text or regex" spellCheck={false}
        aria-invalid={field.refusal ? true : undefined} aria-describedby={field.refusal ? `${field.key}-refused` : undefined}
        value={value[side]} onChange={(e) => onChange({ ...value, [side]: e.target.value })} />
      {field.refusal && <FieldError id={`${field.key}-refused`} className="-mt-1">{field.refusal}</FieldError>}
    </div>
  )
}

/** Filters in one popover, a tab for each when there are several: a column's, or all of a tab's. */
function Editors({ fields }: { fields: FilterField[] }) {
  const [open, setOpen] = useState<string>(() => (fields.find(isSet) ?? fields[0]).key)
  const field = fields.find((f) => f.key === open) ?? fields[0]
  return (
    <div className="flex flex-col gap-3">
      {fields.length > 1 && (
        <ToggleGroup type="single" variant="outline" size="sm" spacing={0} aria-label="Filter" value={field.key} onValueChange={(v) => { if (v) setOpen(v) }}>
          {/* A set filter's dot stands under its word, inside the tab, so five tabs fit the popover with any dots. */}
          {fields.map((f) => <ToggleGroupItem key={f.key} className={cn(CHOSEN, "relative")} value={f.key}>{f.label}{isSet(f) && <span aria-hidden className="absolute bottom-0.5 left-1/2 size-1 -translate-x-1/2 rounded-full bg-current" />}</ToggleGroupItem>)}
        </ToggleGroup>
      )}
      {field.kind === "pattern" ? <PatternEditor key={field.key} field={field} /> : field.kind === "one" ? <OneEditor key={field.key} field={field} /> : <SetEditor key={field.key} field={field} />}
    </div>
  )
}

function EditorPopover({ fields, open, onOpenChange, children }: { fields: FilterField[]; open?: boolean; onOpenChange?: (open: boolean) => void; children: ReactNode }) {
  return (
    <Popover open={open} onOpenChange={onOpenChange}>
      <PopoverTrigger asChild>{children}</PopoverTrigger>
      <PopoverContent align="start" collisionPadding={16} className="w-80 max-w-[calc(100vw-2rem)]">
        <Editors fields={fields} />
      </PopoverContent>
    </Popover>
  )
}

/** A column's header that opens its filters: its label, or over a narrow column `icon` alone, with the label as its title. */
export function FilterHeader({ label, fields, icon: Icon }: { label: string; fields: FilterField[]; icon?: LucideIcon }) {
  const on = anySet(fields)
  const Mark = Icon ?? (on ? ListFilterIcon : ChevronDownIcon)
  const button = (
    <Button variant="ghost" size={Icon ? "icon-xs" : "sm"} className={cn(!Icon && "-ml-2", on ? "text-foreground" : "text-inherit")} aria-label={`Filter servers by ${label}`} title={Icon ? label : undefined}
      disabled={held(fields)} {...(fields.length === 0 && { "aria-disabled": true, tabIndex: -1 })}>
      {!Icon && label}<Mark className={cn(!on && "text-muted-foreground")} />
    </Button>
  )
  /* Drawn before the tab is read, it stands where it will and opens nothing. */
  return fields.length === 0 ? button : <EditorPopover fields={fields}>{button}</EditorPopover>
}

const oneDrawing = (field: Extract<FilterField, { kind: "one" }>): Drawing => {
  const value = field.value ?? ""
  return field.options.find((x) => x.value === value)?.drawing ?? { words: value }
}

/** What a chip says: a pattern as the regex it is, a set as its values drawn, a one as its mark alone where it has one, since the
 * mark is the value; its words go to the chip's name. */
function readout(field: FilterField, side?: Side): ReactNode {
  if (field.kind === "pattern") return <><span className="text-muted-foreground">{field.label} {SIDE_WORDS[side as Side]}</span> <span className="font-mono">{field.value[side as Side]}</span></>
  if (field.kind === "one") {
    const drawing = oneDrawing(field)
    return <><span className="text-muted-foreground">{field.label} is</span> <Drawn facet={field.key} drawing={drawing} wordsClassName={drawing.mark ? "sr-only" : undefined} /></>
  }
  const chosen = field.value.values.map((v) => <Drawn key={v} facet={field.key} drawing={field.options.find((x) => x.value === v)?.drawing ?? { words: "Removed" }} />)
  return <><span className="text-muted-foreground">{field.label} {verbOf(field.value)}</span> {chosen.length > 2 ? `${chosen.length} selected` : chosen}</>
}

/** Every one of the filters takes no edit now. */
const held = (fields: FilterField[]) => fields.length > 0 && fields.every((f) => f.held === true)

type Chip = { id: string; field: FilterField; side?: Side }
const chipsOf = (field: FilterField): Chip[] => field.kind !== "pattern" ? [{ id: field.key, field }]
  : (["include", "exclude"] as const).map((side) => ({ id: `${field.key}:${side}`, field, side }))
const chipSet = ({ field, side }: Chip) => (side ? field.kind === "pattern" && field.value[side] !== "" : isSet(field))
function cleared({ field, side }: Chip) {
  if (field.kind === "set") field.onChange(NOT_SET)
  else if (field.kind === "one") field.onChange(null)
  else field.onChange({ ...field.value, [side as Side]: "" })
}

/** The tab's filters behind one Filter button at the line's start, then those that are set as chips, each removable and opening its
 * editor. A chip stays while its editor is open, so emptying one doesn't drop it mid-edit. The chips wrap: where they don't fit
 * beside Filter they take a row of their own, and a chip wider than the screen is cut before its ×, so every × is in reach. A tab
 * whose filters are its saved rule puts what saves it at the line's end, as `end`. */
export function FilterLine({ fields, end }: { fields: FilterField[]; end?: ReactNode }) {
  const [editing, setEditing] = useState<string | null>(null)
  const chips = fields.flatMap(chipsOf).filter((c) => chipSet(c) || editing === c.id)
  return (
    <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
      {/* As wide as Filter and its chips, so what saves goes to a row of its own before it cuts the first chip. */}
      <div className="flex min-w-0 flex-auto flex-wrap items-center gap-2">
        {fields.length === 0 ? <Button variant="outline" className="shrink-0" aria-disabled tabIndex={-1}><PlusIcon />Filter</Button> : (
          <EditorPopover fields={fields}>
            <Button variant="outline" className="shrink-0" disabled={held(fields)}><PlusIcon />Filter</Button>
          </EditorPopover>
        )}
        <div role="group" aria-label="Filters" className="flex max-w-full min-w-0 flex-wrap items-center gap-2">
          {chips.map((chip) => {
            /* A refused chip takes the stock invalid ring, the whole chip, its × too. */
            const refused = chip.field.kind === "pattern" && chip.field.refusal ? true : undefined
            return (
              <ButtonGroup key={chip.id} className="max-w-full min-w-0" aria-label={`${chip.field.label}${chip.side ? ` ${SIDE_WORDS[chip.side]}` : ""} filter`}>
                <EditorPopover fields={[chip.field]} open={editing === chip.id} onOpenChange={(on) => setEditing(on ? chip.id : null)}>
                  <Button variant="outline" aria-invalid={refused} disabled={chip.field.held} className="flex max-w-80 min-w-0 shrink items-center gap-1 overflow-hidden font-normal" aria-label={`Edit ${chip.field.label}${chip.side ? ` ${SIDE_WORDS[chip.side]}` : ""} filter${chip.field.kind === "one" ? `, ${chip.field.label} is ${oneDrawing(chip.field).words}` : ""}`}>
                    <span className="flex min-w-0 items-center gap-1 truncate">{readout(chip.field, chip.side)}</span>
                  </Button>
                </EditorPopover>
                <Button variant="outline" size="icon" aria-invalid={refused} disabled={chip.field.held} className="shrink-0" aria-label={`Remove ${chip.field.label}${chip.side ? ` ${SIDE_WORDS[chip.side]}` : ""} filter`} onClick={() => { cleared(chip); setEditing(null) }}><XIcon /></Button>
              </ButtonGroup>
            )
          })}
        </div>
      </div>
      {end}
    </div>
  )
}
