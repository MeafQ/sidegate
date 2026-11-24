import { cloneElement, useEffect, useRef, useState, useSyncExternalStore, type ComponentProps, type KeyboardEvent, type MouseEvent, type PointerEvent, type ReactElement, type ReactNode } from "react"
import { Popover as P, Slot } from "radix-ui"
import { cn } from "@/lib/utils"

/** How a card opens. A fact that does nothing else opens on hover, keyboard focus and a tap, and a second tap, a tap elsewhere or
 * Esc closes it. A button keeps its click and tap for its action, and opens its card on hover, keyboard focus or a long press. A
 * mark opens on hover alone, since what it says is in words beside it. A fact in a list's option, which takes no focus and whose
 * click picks the option, opens on hover and on a tap, which then doesn't pick, and its list holds whether it is open, so that
 * the list shows one option's card at a time and opens the highlighted option's for the keyboard. */
type Opens = "fact" | "button" | "hover" | "option"

/** One line of a card: its words, and how it is drawn when that is more than its words, as a legend's marks are. A line with a
 * `name` is a row: a muted name and a plain value, and the card lines its values up in one column, 12px after the names. A name
 * drawn otherwise, as a skipped entry's is, says so in `nameDrawn`. A row's `mark` stands in a 12px place before its name, and
 * where one row has a mark every row keeps the place, so the names line up. */
export type Line = { words: string; drawn?: ReactNode; name?: string; nameDrawn?: ReactNode; mark?: ReactNode }

/** A mark in the 12px place a row or a chart's key keeps before its name, so the names line up with a mark or without one. */
export const MarkSlot = ({ children }: { children?: ReactNode }) => <span aria-hidden className="inline-flex w-3 shrink-0 justify-center">{children}</span>

/** A row's two cells in its grid: the place for a mark where its rows have marks, the muted name, then the plain value. */
function Row({ line, marked }: { line: Line; marked: boolean }) {
  return (
    <span data-slot="row" className="contents">
      <span className={cn("flex items-center gap-1.5", line.nameDrawn === undefined && "text-muted-foreground")}>{marked && <MarkSlot>{line.mark}</MarkSlot>}{line.nameDrawn ?? line.name}</span>
      <span>{line.drawn ?? line.words}</span>
    </span>
  )
}

const ROWS = "grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 tabular-nums"

/** Rows outside a card's lines, as a popover that holds a control draws them: the same grid a card puts its named lines in. */
export function Rows({ lines, className }: { lines: readonly Line[]; className?: string }) {
  const marked = lines.some((l) => l.mark !== undefined)
  return <div data-slot="rows" className={cn(ROWS, "gap-y-1 text-left", className)}>{lines.map((line, i) => <Row key={i} line={line} marked={marked} />)}</div>
}

/** Where a card opens beside what it describes. On a phone there is no beside, so every card opens under it. */
export type Side = "left" | "right" | "bottom"

/** What a card stands beside on a wide screen, when that is more than its trigger: a menu's option's card stands beside the whole
 * menu, level with the option, so it covers no option. */
export type Beside = (trigger: Element) => DOMRect

/** On a phone a card stands under its trigger, 16px in from the edge of the screen the trigger is nearer, whatever its place in its
 * row: anchored there with no width, a card opening from it is pushed back in by the 16px it keeps from the edge. */
const PHONE_INSET = 16
const underOnPhone: Beside = (trigger) => {
  const t = trigger.getBoundingClientRect()
  const screen = document.documentElement.clientWidth
  return new DOMRect(t.x + t.width / 2 < screen / 2 ? PHONE_INSET : screen - PHONE_INSET, t.y, 0, t.height)
}

/** The anchor a card is placed against: its trigger, on a wide screen what it stands beside, and on a phone the screen's width at
 * the trigger's line. */
/** A card's popover parts share a scope of their own, so a card can stand on a control that is another popover's trigger, as a
 * slot's server line is, and the control still opens its own popover. */
const useCardScope = P.createPopoverScope()
type CardScope = ReturnType<typeof useCardScope>

function Anchored({ scope, trigger, beside, level, children }: { scope: CardScope; trigger: { current: Element | null }; beside?: Beside; level?: boolean; children: ReactElement }) {
  const wide = useWide()
  const against = wide || level ? beside : underOnPhone
  if (!against) return <P.Anchor {...scope} asChild>{children}</P.Anchor>
  const measured = { current: { getBoundingClientRect: () => (trigger.current ? against(trigger.current) : new DOMRect()) } }
  return <><P.Anchor {...scope} virtualRef={measured} />{children}</>
}

/** Beside the menu that holds the trigger, level with the trigger's row. */
export const besideMenu = (row: string): Beside => (trigger) => {
  const menu = (trigger.closest("[data-slot=popover-content]") ?? trigger).getBoundingClientRect()
  const line = (trigger.closest(row) ?? trigger).getBoundingClientRect()
  return new DOMRect(menu.x, line.y, menu.width, line.height)
}

const WIDE = "(min-width: 768px)"
const subscribe = (change: () => void) => {
  const query = window.matchMedia(WIDE)
  query.addEventListener("change", change)
  return () => query.removeEventListener("change", change)
}
const useWide = () => useSyncExternalStore(subscribe, () => window.matchMedia(WIDE).matches)

const HOVER_OPEN_MS = 300
const HOVER_CLOSE_MS = 150
const LONG_PRESS_MS = 500

/** The press on a card's trigger: none; a pointer's, from its pointerdown until its click, or the blur after a press with none,
 * since a tap's focus and click can come frames after its pointerup; or a pointer's held long enough to open a button's card, whose
 * click then isn't the button's. A new press, a cancelled one or a click ends what came before; a blur ends a press, not a hold. */
type Press = { kind: "none" } | { kind: "down"; by: string } | { kind: "held"; by: string }

/** What the card's one timer will do: open it once a mouse has rested on the trigger or the card, close it once the mouse has
 * left them, or hold a press that lasts as a long press. A new one replaces what was due. */
type Due = "open" | "close" | "hold" | null
const DUE_MS = { open: HOVER_OPEN_MS, close: HOVER_CLOSE_MS, hold: LONG_PRESS_MS } as const

/** The pointer on a card: its press, and what is due. A mouse's enter and leave set what is due and restart its time, so a mouse
 * still moving over an option opens nothing; a long press's down does the same. `fire` is the time running out. */
type Pointer = { press: Press; due: Due }
type PointerStep = { type: "enter" } | { type: "leave" } | { type: "down"; by: string; long: boolean } | { type: "up" } | { type: "fire" } | { type: "cancel" } | { type: "blur" } | { type: "end" }

const NO_PRESS: Press = { kind: "none" }
const RESTING: Pointer = { press: NO_PRESS, due: null }

function moved({ press, due }: Pointer, e: PointerStep): Pointer {
  switch (e.type) {
    case "enter": return { press, due: "open" }
    case "leave": return { press, due: "close" }
    case "down": return { press: { kind: "down", by: e.by }, due: e.long ? "hold" : due }
    case "up": return press.kind === "held" ? { press, due } : { press, due: null }
    case "fire": return { press: due === "hold" && press.kind === "down" ? { kind: "held", by: press.by } : press, due: null }
    case "cancel": return RESTING
    case "blur": return { press: press.kind === "held" ? press : NO_PRESS, due }
    case "end": return { press: NO_PRESS, due }
  }
}

/** The steps that start what they make due over again. */
const restarts = (e: PointerStep) => e.type === "enter" || e.type === "leave" || (e.type === "down" && e.long)

/** Whether a card is open, when its owner holds it rather than the card. */
export type Held = { open: boolean; onOpenChange: (open: boolean) => void }

function useCard(opens: Opens, held?: Held) {
  const [own, setOwn] = useState(false)
  const open = held?.open ?? own
  const setOpen = held?.onOpenChange ?? setOwn
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  /** The pointer on the card, so a press's focus and click aren't taken for the keyboard's. */
  const pointer = useRef<Pointer>(RESTING)
  const step = (e: PointerStep) => {
    const was = pointer.current.due
    pointer.current = moved(pointer.current, e)
    const { due } = pointer.current
    if (due === was && !restarts(e)) return
    clearTimeout(timer.current)
    if (due) timer.current = setTimeout(() => { step({ type: "fire" }); setOpen(due !== "close") }, DUE_MS[due])
  }
  const trigger = useRef<Element | null>(null)
  const focuses = opens === "fact" || opens === "button"
  useEffect(() => () => clearTimeout(timer.current), [])
  const enter = (e: PointerEvent) => { if (e.pointerType === "mouse") step({ type: "enter" }) }
  const hover = {
    onPointerEnter: enter,
    onPointerLeave: (e: PointerEvent) => { if (e.pointerType === "mouse") step({ type: "leave" }) },
  }
  const triggerProps = {
    ref: (el: Element | null) => { trigger.current = el },
    ...hover,
    /* A key scrolls an option's list under a mouse at rest, which enters cells without the user moving it, so only a move opens. */
    ...(opens === "option" && { onPointerEnter: undefined, onPointerMove: (e: PointerEvent) => { if (!open) enter(e) } }),
    onPointerDown: (e: PointerEvent) => step({ type: "down", by: e.pointerType, long: opens === "button" && e.pointerType !== "mouse" }),
    onPointerUp: () => { if (opens === "button") step({ type: "up" }) },
    onPointerCancel: () => step({ type: "cancel" }),
    onContextMenu: (e: MouseEvent) => { if (opens === "button" && (pointer.current.press.kind === "none" || pointer.current.press.by !== "mouse")) e.preventDefault() },
    onFocus: () => { if (focuses && pointer.current.press.kind === "none") setOpen(true) },
    onBlur: () => {
      step({ type: "blur" })
      if (focuses) setOpen(false)
    },
    /** A long press opened the card, so the click that ends it isn't the button's. */
    onClick: (e: MouseEvent, action?: (e: MouseEvent) => void) => {
      const was = pointer.current.press
      step({ type: "end" })
      if (was.kind === "held") {
        e.preventDefault()
        return
      }
      const by = was.kind === "none" ? null : was.by
      action?.(e)
      if (opens === "option" && by !== "mouse") {
        e.stopPropagation()
        setOpen(!open)
      }
      if (opens !== "fact") return
      if (by === "mouse") setOpen(true)
      else setOpen(!open)
    },
  }
  const contentProps = {
    ...hover,
    /* A card closes when its trigger loses focus, so a press inside one that holds a chart keeps focus where it is: off the plot
     * it returns the chart to its week, and the card stays. */
    onPointerDown: (e: PointerEvent) => { if (e.currentTarget.querySelector("[data-slot=chart]")) e.preventDefault() },
    onOpenAutoFocus: (e: Event) => e.preventDefault(),
    onCloseAutoFocus: (e: Event) => e.preventDefault(),
    /* A tap on the trigger toggles it, so it mustn't also count as a tap elsewhere. */
    onInteractOutside: (e: Event) => { if (trigger.current?.contains(e.target as Node)) e.preventDefault() },
  }
  return { open, setOpen, trigger, triggerProps, contentProps }
}

/** A card's lines, each in its own row and in the text's colour, the widest fitting the screen and wrapping. Named lines are rows,
 * their names in one muted column and their values in the next. A card that holds a control is read by a screen reader; one that
 * holds only words is not, since its trigger says them. */
function CardContent({ scope, lines, side = "bottom", level = false, className, children, ...props }: { scope: CardScope; lines: readonly Line[]; side?: Side; level?: boolean; children?: ReactNode } & Omit<ComponentProps<typeof P.Content>, "side">) {
  const wide = useWide() || level
  const named = lines.some((l) => l.name !== undefined)
  const marked = lines.some((l) => l.mark !== undefined)
  return (
    <P.Portal {...scope}>
      <P.Content {...scope} side={wide ? side : "bottom"} align={wide && side === "bottom" ? "center" : "start"} sideOffset={4} collisionPadding={PHONE_INSET} aria-hidden={children ? undefined : true}
        className={cn("bg-popover text-popover-foreground ring-foreground/10 data-open:animate-in data-open:fade-in-0 z-50 flex max-w-[min(20rem,calc(100vw-2rem))] flex-col gap-1 rounded-lg p-2.5 text-xs/relaxed wrap-anywhere shadow-md ring-1 outline-hidden",
          named && ROWS, className)}
        {...props}>
        {lines.map((line, i) => line.name !== undefined
          ? <Row key={i} line={line} marked={marked} />
          : <span key={i} className={cn(named && "col-span-2")}>{line.drawn ?? line.words}</span>)}
        {children && <span className={cn(named && "col-span-2", "flex flex-col gap-2")}>{children}</span>}
      </P.Content>
    </P.Portal>
  )
}

const spoken = (lines: readonly Line[]) => lines.map((l) => (l.name === undefined ? l.words : `${l.name} ${l.words}`)).join(". ")

/** A fact in a line of facts whose card says more. It does nothing else, so a tap opens its card, and it is a stop for the keyboard
 * in reading order. A screen reader hears its words and its card's lines. Its tap area is 28px tall. Each child is its own row, so a
 * run of words and elements goes in one element. `asChild` draws it as the one control it holds, as a chip's word is drawn, named
 * by `label`. */
export function Fact({ lines, className, label, side, beside, level, held, onKeyDown, disabled, asChild = false, children }: {
  lines: readonly Line[]; className?: string; label?: string; side?: Side; beside?: Beside
  /** Its owner holds whether it is open, as the week's does, which forgets its place when it closes. */
  held?: Held
  onKeyDown?: (e: KeyboardEvent) => void
  /** Its card stands beside it on a phone too, as on a wide screen, for a fact at a line's start whose card would cover what is under it. */
  level?: boolean
  /** A control that can't act now, whose card says why: it stays a stop, so the card opens as any fact's does. */
  disabled?: boolean
  asChild?: boolean
  children: ReactNode
}) {
  const { open, setOpen, trigger, triggerProps, contentProps } = useCard("fact", held)
  const scope = useCardScope(undefined)
  const own = { "aria-label": label, "aria-disabled": disabled || undefined, onKeyDown, ...triggerProps }
  return (
    <P.Root {...scope} open={open} onOpenChange={setOpen}>
      <Anchored scope={scope} trigger={trigger} beside={beside} level={level}>
        {asChild ? <Slot.Root className={className} {...own}>{children}</Slot.Root> : (
          <button type="button" {...own} className={cn("focus-visible:ring-ring/50 flex min-h-7 flex-col items-stretch rounded-md text-left outline-none focus-visible:ring-2", className)}>
            {children}
            {!label && <span className="sr-only">. {spoken(lines)}</span>}
          </button>
        )}
      </Anchored>
      <CardContent scope={scope} lines={lines} side={side} level={level} {...contentProps} />
    </P.Root>
  )
}

/** A card on a control that has its own action: a button's longer name, or what its action will do. Its owner can hold whether it
 * is open, and draw more than lines in it, as a server name's week does: that content sets its own width, and the card is as wide
 * as it and no wider than the screen less 32px. `beside` and `level` place its card as they place a fact's (`Fact`). */
export function Tip({ lines, opens = "button", held, side, beside, level, content, children }: {
  lines: readonly Line[]; opens?: "button" | "hover"; held?: Held; side?: Side; beside?: Beside; level?: boolean; content?: ReactNode
  children: ReactElement<{ onClick?: (e: MouseEvent) => void }>
}) {
  const { open, setOpen, trigger, triggerProps, contentProps } = useCard(opens, held)
  const scope = useCardScope(undefined)
  const action = children.props.onClick
  return (
    <P.Root {...scope} open={open} onOpenChange={setOpen}>
      <Anchored scope={scope} trigger={trigger} beside={beside} level={level}>
        <Slot.Root {...triggerProps} onClick={(e: MouseEvent) => triggerProps.onClick(e, action)}>{cloneElement(children, { onClick: undefined })}</Slot.Root>
      </Anchored>
      {content === undefined ? <CardContent scope={scope} lines={lines} side={side} level={level} {...contentProps} />
        : <CardContent scope={scope} lines={lines} side={side} level={level} aria-hidden className="max-w-[calc(100vw-2rem)]" {...contentProps}>{content}</CardContent>}
    </P.Root>
  )
}

/** A fact in a list's option, whose click picks the option. The list holds whether its card is open, since the keyboard's
 * highlight, not focus, opens it, and only one option's card is open at a time. */
export function OptionFact({ lines, className, side, beside, children, ...held }: { lines: readonly Line[]; className?: string; side?: Side; beside?: Beside; children: ReactNode } & Held) {
  const { trigger, triggerProps, contentProps } = useCard("option", held)
  const scope = useCardScope(undefined)
  return (
    <P.Root {...scope} {...held}>
      <Anchored scope={scope} trigger={trigger} beside={beside}>
        <span {...triggerProps} className={className}>{children}</span>
      </Anchored>
      <CardContent scope={scope} lines={lines} side={side} {...contentProps} />
    </P.Root>
  )
}

/** A card that holds an action, opened by a click on what it describes, and closed by a second click, a click elsewhere or Esc. A
 * card of one line holds its action at that line's end; a card of several keeps it under them. A screen reader hears it by its
 * trigger's label. Closed, it gives the focus back to its trigger only while nothing else has taken it. */
export function ActionCard({ lines, label, className, side, action, disabled, children }: {
  lines: readonly Line[]; label: string; className?: string; side?: Side; action: ReactNode
  /** A control that can't act now, whose card says why and holds the fix: it still opens its card. */
  disabled?: boolean
  children: ReactNode
}) {
  const [open, setOpen] = useState(false)
  const trigger = useRef<HTMLButtonElement>(null)
  const scope = useCardScope(undefined)
  const only = lines.length === 1 ? lines[0] : null
  return (
    <P.Root {...scope} open={open} onOpenChange={setOpen}>
      <Anchored scope={scope} trigger={trigger}>
        <P.Trigger {...scope} asChild>
          <button ref={trigger} type="button" aria-label={label} aria-disabled={disabled || undefined} className={cn("focus-visible:ring-ring/50 flex min-h-7 rounded-md outline-none focus-visible:ring-2", className)}>{children}</button>
        </P.Trigger>
      </Anchored>
      {/* A link leaves for what it opens, so the card goes with it, as the top bar's stays on every page. */}
      <CardContent scope={scope} lines={only ? [] : lines} side={side} aria-label={label} onClick={(e) => { if (e.target instanceof Element && e.target.closest("a")) setOpen(false) }}
        onCloseAutoFocus={(e) => { if (document.activeElement !== null && document.activeElement !== document.body) e.preventDefault() }}>
        {only ? <span className="flex items-center gap-2">{only.name === undefined ? <span className="min-w-0 flex-1">{only.drawn ?? only.words}</span> : <Rows lines={lines} className="min-w-0 flex-1" />}{action}</span> : action}
      </CardContent>
    </P.Root>
  )
}
