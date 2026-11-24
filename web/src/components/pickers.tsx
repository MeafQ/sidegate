import { createContext, useContext, useLayoutEffect, useRef, useState, type ComponentProps, type KeyboardEvent, type MouseEvent, type ReactNode } from "react"
import { flushSync } from "react-dom"
import { XIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Command, CommandInput } from "@/components/ui/command"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { cn } from "@/lib/utils"

/** A toggle group's chosen item, plainly marked: the outline variant's muted fill barely shows on a card. */
export const CHOSEN = "text-muted-foreground data-[state=on]:border-primary data-[state=on]:bg-primary data-[state=on]:text-primary-foreground"

/** A choice of numbers as one outline toggle group, as a source's Refresh and Settings' Retention are: each offered value a button
 * in order, and a value it doesn't offer, such as a provider's 2 hours or a retention set by hand, a button of its own in order
 * while it is `kept`. `children` are buttons after the numbers, such as Refresh's Off. */
export function NumberChoices({ label, offered, kept, value, word, title, disabled, invalid = false, describedBy, onPick, children }: {
  label: string; offered: readonly number[]; kept: readonly (number | null)[]; value: string
  word: (n: number) => ReactNode; title?: (n: number) => string | undefined
  disabled: boolean; invalid?: boolean; describedBy?: string; onPick: (value: string) => void; children?: ReactNode
}) {
  const choices = [...new Set([...offered, ...kept.filter((n): n is number => n !== null && n > 0)])].toSorted((a, b) => a - b)
  return (
    <ToggleGroup type="single" variant="outline" size="sm" spacing={0} aria-label={label} disabled={disabled} value={value}
      aria-invalid={invalid || undefined} aria-describedby={describedBy} onValueChange={(v) => { if (v) onPick(v) }}>
      {choices.map((n) => (
        <ToggleGroupItem key={n} className={cn(CHOSEN, "gap-1")} value={String(n)} aria-invalid={invalid || undefined} title={title?.(n)}>{word(n)}</ToggleGroupItem>
      ))}
      {children}
    </ToggleGroup>
  )
}

/** The keys that move cmdk's highlight. */
export const moves = (e: KeyboardEvent) => ["ArrowDown", "ArrowUp", "Home", "End"].includes(e.key) || (e.ctrlKey && ["n", "p", "j", "k"].includes(e.key))

/** The rows cmdk's highlight moves over. */
const ROWS = '[cmdk-item=""]:not([aria-disabled="true"])'

/** cmdk's own stops, where its keys act. Whatever else takes the focus in a menu is one of its controls. */
const CMDK = "[cmdk-input], [cmdk-list]"

/** What a press lands on: the nearest of these around it, a control, or cmdk's root or list, which take a focus by their tabindex. */
const FOCUSABLE = "a[href], button, input, select, textarea, [tabindex], [contenteditable]"

/** What a press in a menu lands on: a control without the focus, which takes it, or nothing that may, so the focus stays where it
 * was, as it does on a control that has it. cmdk's list and root would take the focus from a press anywhere else, and a disabled
 * control takes none. A press in a text field is the field's, which also places its caret; one a control has cancelled, as the
 * search's × does to keep the focus in the search, stays cancelled; and one in a card the menu holds lands outside it. */
function pressed(e: MouseEvent<HTMLElement>) {
  if (!(e.target instanceof Element) || !e.currentTarget.contains(e.target) || e.defaultPrevented) return null
  const control = e.target.closest(FOCUSABLE)
  if (control?.matches("input, textarea")) return null
  if (control instanceof HTMLElement && !control.matches("[cmdk-root], [cmdk-list], :disabled") && control !== document.activeElement) return control
  e.preventDefault()
  return null
}

/** The search a menu's control gives the keys back to. */
export const searchOf = (el: Element | null) => el?.closest("[cmdk-root]")?.querySelector<HTMLElement>("[cmdk-input]")

/** How a key that moves the highlight goes on from a control to the search: the row cmdk steps from, so that a step reaches the
 * nearest row that way, the last before the control for a step down, the first after it for a step up, "" for none; and the key,
 * which for a step up from below every row is End. A step with no row that way goes nowhere, and a jump to an end needs no row
 * to start from. */
function handOff(e: KeyboardEvent, control: Element, rows: Element[]) {
  if (e.metaKey || e.key === "Home" || e.key === "End") return { from: "", key: e.key }
  const after = rows.findIndex((r) => control.compareDocumentPosition(r) & Node.DOCUMENT_POSITION_FOLLOWING)
  const valueOf = (row: Element | undefined) => row?.getAttribute("data-value") ?? ""
  if (e.key === "ArrowUp" || (e.ctrlKey && ["p", "k"].includes(e.key))) {
    return after === -1 ? { from: "", key: "End" } : after === 0 ? null : { from: valueOf(rows[after]), key: e.key }
  }
  return after === -1 ? null : { from: valueOf(rows[after - 1]), key: e.key }
}

/** The id of the row a menu highlights, which its search names its active option. */
const Active = createContext<string | undefined>(undefined)

/** A list built on cmdk, whose holder keeps the row it highlights, and which owns the keys of every control in it, whatever the
 * control: a header's Stability, the search's ×, a failed read's Retry, a pick's Clear, a row's Open link. A control's Enter and
 * Space are its own and never reach cmdk, which would pick the highlighted row. A held Enter's repeats pick nothing, wherever the
 * first press moved the focus. Focused, a control takes the highlight from the rows, so it is the one thing that looks chosen,
 * and the search gets it back when the focus does. A key that moves the highlight goes on to the search, as if pressed there,
 * from the control's place among the rows. The row the holder keeps is the one record of the highlight: cmdk draws it and Enter
 * picks the drawn row, and the search names it, since cmdk's own name for its active option misses a highlight set from outside
 * and one it sets itself as the search changes. A press puts the focus on the control it lands on, and anywhere else leaves it
 * where it was (`pressed`), so the search and the controls are the only places the keys can be: cmdk's list and root would take
 * it, and pick on Enter with a stale name for the row, or none. A control takes the focus as a press gives it, with no ring until
 * a key is used, but WebKit focuses no button a press lands on and gives the focus to cmdk's list, from where the menu moves it on
 * to the control. */
export function Menu({ highlighted, onHighlight, children, ...props }: Omit<ComponentProps<typeof Command>, "value" | "onValueChange" | "onMouseDown" | "onFocus"> & { highlighted: string; onHighlight: (row: string) => void }) {
  const held = useRef("")
  const pressedOn = useRef<HTMLElement | null>(null)
  const list = useRef<HTMLDivElement>(null)
  const [active, setActive] = useState<string>()
  /* Read again with the rows the holder passes, since a row's element comes and goes with them. */
  useLayoutEffect(() => setActive(highlighted ? list.current?.querySelector(`[cmdk-item][data-value="${CSS.escape(highlighted)}"]`)?.id : undefined), [highlighted, children])
  return (
    <Command value={highlighted} onValueChange={onHighlight} {...props} onMouseDown={(e) => { pressedOn.current = pressed(e) }}
      onFocus={(e) => { if (e.target.matches("[cmdk-root], [cmdk-list]")) pressedOn.current?.focus() }}>
      <div ref={list} className="contents"
        onFocus={(e) => {
          if (e.target.matches("[cmdk-input]")) {
            const row = held.current && e.currentTarget.querySelector(`${ROWS}[data-value="${CSS.escape(held.current)}"]`)
            if (row && highlighted === "") onHighlight(held.current)
            held.current = ""
          } else if (!e.target.matches(CMDK)) {
            if (highlighted !== "") held.current = highlighted
            onHighlight("")
          }
        }}
        onKeyDown={(e) => {
          if (!(e.target instanceof Element)) return
          if (e.target.matches(CMDK)) {
            if (e.key === "Enter" && e.repeat) e.preventDefault()
            return
          }
          if (e.key === "Enter" || e.key === " ") {
            e.stopPropagation()
            return
          }
          const search = e.currentTarget.querySelector<HTMLElement>("[cmdk-input]")
          if (!moves(e) || !search) return
          e.preventDefault()
          e.stopPropagation()
          const step = handOff(e, e.target, [...e.currentTarget.querySelectorAll(ROWS)])
          held.current = ""
          /* Drawn before cmdk reads which row it steps from. */
          flushSync(() => onHighlight(step?.from ?? ""))
          search.focus()
          if (step) search.dispatchEvent(new window.KeyboardEvent("keydown", { key: step.key, ctrlKey: e.ctrlKey, metaKey: e.metaKey, altKey: e.altKey, shiftKey: e.shiftKey, bubbles: true, cancelable: true }))
        }}>
        <Active value={active}>{children}</Active>
      </div>
    </Command>
  )
}

/** A list's search, with a × at its end while it holds text, which empties it and gives the keys back to the search, as every
 * search field has. */
export function ListSearch({ value, onValueChange, placeholder }: { value: string; onValueChange: (value: string) => void; placeholder: string }) {
  const active = useContext(Active)
  return (
    <div className="relative">
      {/* The input is cmdk's own, but for the option it names (`Menu`). */}
      <CommandInput asChild placeholder={placeholder} value={value} onValueChange={onValueChange} className={cn(value !== "" && "pr-6")}>
        <input aria-activedescendant={active} />
      </CommandInput>
      {value !== "" && (
        <span className="absolute top-1 right-1.5 flex h-8 items-center">
          <Button type="button" variant="ghost" size="icon-xs" aria-label="Clear search" className="text-muted-foreground hover:text-foreground" onMouseDown={(e) => e.preventDefault()}
            onClick={(e) => { searchOf(e.currentTarget)?.focus(); onValueChange("") }}><XIcon /></Button>
        </span>
      )}
    </div>
  )
}
