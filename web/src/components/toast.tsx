import { useEffect, useLayoutEffect, useRef, useState } from "react"
import { LogInIcon, RotateCcwIcon, XIcon } from "lucide-react"
import { toast as sonner } from "sonner"
import { Fact } from "@/components/card"
import { MarkIcon } from "@/components/status"
import { Button } from "@/components/ui/button"
import { Toaster } from "@/components/ui/sonner"
import type { ControlKey, Toast } from "@/lib/write"
import { cn } from "@/lib/utils"

const ICONS = { Retry: <RotateCcwIcon />, Reload: <RotateCcwIcon />, "Sign in": <LogInIcon />, "Keep mine": null, Open: null } as const

/** A toast to say: a write's own, or the page's over another release, whose button reloads. `act` is what the button does. */
export type Said = Omit<Toast, "button"> & { readonly button: { readonly word: keyof typeof ICONS } | null }

/** The toast each control has on screen. Each toast gets an id of its own: sonner removes a dismissed id a frame or two
 * later, so a toast said again under the id its control's press just dismissed would be taken with the old one. */
const shown = new Map<ControlKey, string>()
let count = 0

/** Rule 2's one toast: the mark, `what`, the why in muted words, an optional muted second line, three lines at most with the
 * rest in its card, one button, and × on a toast that stays. A control has one toast at a time: a new one takes the place of its last,
 * and its next press dismisses it. */
export function say(toast: Said, act: () => void): void {
  dismiss(toast.id)
  const id = `${toast.id}#${++count}`
  shown.set(toast.id, id)
  const gone = () => { if (shown.get(toast.id) === id) shown.delete(toast.id) }
  sonner.custom(() => <ToastBody toast={toast} act={() => { sonner.dismiss(id); act() }} close={() => sonner.dismiss(id)} />, {
    id, duration: toast.stays ? Infinity : undefined, onDismiss: gone, onAutoClose: gone,
  })
}

export function dismiss(control: ControlKey): void {
  const id = shown.get(control)
  if (id === undefined) return
  shown.delete(control)
  sonner.dismiss(id)
}

function ToastBody({ toast, act, close }: { toast: Said; act: () => void; close: () => void }) {
  const lines = toast.muted !== null
  const text = useRef<HTMLDivElement>(null)
  const [cut, setCut] = useState(false)
  /* Three lines at most: the first line's two over a muted line, or three alone. What they cut is in the card, whose button
   * then holds the words, so the measure follows them there. */
  useLayoutEffect(() => {
    const box = text.current
    if (!box) return
    /* A clamped line's own sizes are its clamp's, so its whole height is read from an unclamped copy at its width. */
    const measure = () => setCut([...box.children].some((line) => {
      if (!(line instanceof HTMLElement)) return false
      const whole = line.cloneNode(true) as HTMLElement
      /* At the line's own width plus a pixel, since its width is read rounded, so a line that fits isn't read as wrapping. */
      Object.assign(whole.style, { display: "block", webkitLineClamp: "unset", position: "absolute", visibility: "hidden", width: `${line.offsetWidth + 1}px` })
      box.append(whole)
      const over = whole.offsetHeight > line.clientHeight + 1
      whole.remove()
      return over
    }))
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(box)
    return () => observer.disconnect()
  }, [toast, cut])
  const words = (
    <div ref={text} className="flex min-w-0 flex-col items-start gap-px">
      <span className={cn("[overflow-wrap:anywhere]", lines ? "line-clamp-2" : "line-clamp-3")}>
        <span className="font-medium">{toast.what}</span>
        {toast.why !== null && <>{" "}<span className="text-muted-foreground">{toast.why}</span></>}
      </span>
      {lines && <span className="text-muted-foreground line-clamp-1 [overflow-wrap:anywhere]">{toast.muted}</span>}
    </div>
  )
  const whole = [{ words: [toast.what, toast.why].filter((w) => w !== null).join(" ") }, ...(lines ? [{ words: toast.muted as string }] : [])]
  return (
    <div role={toast.mark === "danger" ? "alert" : "status"} data-toast={toast.mark} data-reports={toast.id}
      className="bg-popover text-popover-foreground flex w-full min-w-0 items-center gap-2.5 rounded-lg border py-2 pr-2.5 pl-3 text-xs/relaxed shadow-lg">
      <MarkIcon mark={toast.mark} />
      <div className="min-w-0 flex-1">{cut ? <Fact lines={whole} className="min-h-0 w-full">{words}</Fact> : words}</div>
      {toast.button && <Button size="sm" variant="outline" onClick={act}>{ICONS[toast.button.word]}{toast.button.word}</Button>}
      {toast.stays && (
        <button type="button" aria-label="Dismiss" onClick={close} className="text-muted-foreground hover:bg-muted -ml-0.5 inline-flex shrink-0 rounded-sm">
          <XIcon className="size-3" />
        </button>
      )}
    </div>
  )
}

/** Toasts shown open, one above the other; more than this stack as the stock collapsed stack does and open on a tap. */
const OPEN_TOASTS = 3
/** What stands between the last thing scrolled clear and the toasts' top. */
const CLEAR = 8

/** Where a toast's top stands once it has moved into place: the stock toaster lays each out from the list's bottom edge by its
 * offset when open and by the gap per toast before it when stacked, so this reads that layout and not a frame of its motion. */
function settledTop(list: Element, toast: HTMLElement): number {
  const style = getComputedStyle(toast)
  const px = (name: string, from: CSSStyleDeclaration = style) => Number.parseFloat(from.getPropertyValue(name)) || 0
  const open = toast.dataset.expanded === "true"
  const front = toast.dataset.front === "true"
  const height = open || front ? px("--initial-height") || toast.offsetHeight : px("--front-toast-height", getComputedStyle(list))
  const lift = open ? px("--offset") : px("--toasts-before") * px("--gap", getComputedStyle(list))
  return list.getBoundingClientRect().bottom - lift - height
}

/** The page's toaster. While any toast is shown the page's scrolling content has room under it as tall as the toasts, so all of
 * it can be scrolled clear of them, and when a toast appears the control any shown toast reports on is scrolled clear if it would
 * stand under them. A control names itself for this with `data-control`, its ControlKeys parted by spaces. */
export function Toasts() {
  const host = useRef<HTMLDivElement>(null)
  const [stacked, setStacked] = useState(false)
  useEffect(() => {
    const root = host.current
    if (!root) return
    const checked = new Set<Element>()
    const asked = { frame: 0 }
    const settle = () => {
      const list = root.querySelector("[data-sonner-toaster]")
      const shown = [...root.querySelectorAll<HTMLElement>("[data-sonner-toast]")].filter((t) => t.dataset.removed !== "true")
      setStacked(shown.length > OPEN_TOASTS)
      const toasts = shown.filter((t) => t.dataset.visible !== "false")
      const scroller = document.querySelector<HTMLElement>("[data-scroller]")
      if (!scroller) return
      const top = list && toasts.length > 0 ? Math.min(...toasts.map((t) => settledTop(list, t))) : null
      const bottom = scroller.getBoundingClientRect().bottom
      scroller.style.setProperty("--toast-room", `${top === null ? 0 : Math.max(0, bottom - top) + CLEAR}px`)
      for (const gone of [...checked]) if (!gone.isConnected) checked.delete(gone)
      const appeared = toasts.filter((t) => t.dataset.mounted === "true" && !checked.has(t))
      if (top === null || appeared.length === 0) return
      for (const t of appeared) checked.add(t)
      const span = toasts.map((t) => t.getBoundingClientRect())
      const [left, right] = [Math.min(...span.map((r) => r.left)), Math.max(...span.map((r) => r.right))]
      const under = toasts.flatMap((t) => {
        const control = t.querySelector<HTMLElement>("[data-reports]")?.dataset.reports
        const anchor = control ? [...document.querySelectorAll<HTMLElement>(`[data-control~="${CSS.escape(control)}"]`)].find((a) => a.offsetParent !== null) : undefined
        const r = anchor?.getBoundingClientRect()
        return r && r.right > left && r.left < right && r.bottom > top - CLEAR ? [r.bottom - (top - CLEAR)] : []
      })
      if (under.length > 0) scroller.scrollBy({ top: Math.max(...under) })
    }
    const measure = () => {
      cancelAnimationFrame(asked.frame)
      asked.frame = requestAnimationFrame(settle)
    }
    const observer = new MutationObserver(measure)
    observer.observe(root, { subtree: true, childList: true, attributes: true, attributeFilter: ["style", "data-mounted", "data-removed", "data-expanded", "data-front", "data-visible"] })
    window.addEventListener("resize", measure)
    return () => {
      cancelAnimationFrame(asked.frame)
      observer.disconnect()
      window.removeEventListener("resize", measure)
      document.querySelector<HTMLElement>("[data-scroller]")?.style.removeProperty("--toast-room")
    }
  }, [])
  return <div ref={host} className="contents"><Toaster position="bottom-right" expand={!stacked} /></div>
}
