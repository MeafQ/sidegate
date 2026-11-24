import { useEffect, useLayoutEffect, useRef, useState } from "react"
import { PencilIcon, XIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { FieldError } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { cutToLines } from "@/lib/format"
import { useUnsaved } from "@/lib/use-write"

/** A name of up to `lines` lines, cut to fit, with the pencil after its last letter. It is measured again whenever its row resizes. */
function LongName({ name, lines, pending, waits, onRename }: { name: string; lines: number; pending: boolean; waits: boolean; onRename: () => void }) {
  const button = useRef<HTMLButtonElement>(null)
  const text = useRef<HTMLSpanElement>(null)
  const pencil = useRef<SVGSVGElement>(null)
  useLayoutEffect(() => {
    const [el, words, mark] = [button.current, text.current, pencil.current]
    if (!el || !words || !mark) return
    const lineHeight = parseFloat(getComputedStyle(el).lineHeight)
    const measure = (t: string, bare = false) => {
      words.textContent = t
      mark.style.display = bare ? "none" : ""
      return Math.round(el.getBoundingClientRect().height / lineHeight)
    }
    const fit = () => {
      words.textContent = cutToLines(name, lines, measure)
      mark.style.display = ""
    }
    fit()
    const row = new ResizeObserver(fit)
    if (el.parentElement?.parentElement) row.observe(el.parentElement.parentElement)
    return () => row.disconnect()
  }, [name, lines])
  return (
    <h2 className="min-w-0 text-sm font-semibold">
      <button ref={button} type="button" aria-label={`Rename ${name}`} disabled={pending || waits} aria-busy={pending || undefined} className="focus-visible:ring-ring/50 rounded-md text-left [overflow-wrap:anywhere] outline-none focus-visible:ring-2" onClick={onRename}>
        <span ref={text} />
        {pending ? <Spinner role={undefined} aria-label={undefined} aria-hidden className="text-muted-foreground ml-1.5 inline size-3.5 align-[-2px]" />
          : <PencilIcon ref={pencil} aria-hidden className="text-muted-foreground ml-1.5 inline size-3.5 align-[-2px]" />}
      </button>
    </h2>
  )
}

/** A name edited in place: shown, or being named with the text typed over what it began as. */
type Naming = { readonly kind: "showing" } | { readonly kind: "naming"; readonly text: string; readonly from: string }
const SHOWING: Naming = { kind: "showing" }

/** A name edited in place. `finish` ends the naming as it moves, before React draws it, so the blur that follows Enter or Esc
 * finds it over; it gives the text to keep, null for none, or undefined when the naming was over already. */
export function useNaming(open: string | null) {
  const [naming, setNaming] = useState<Naming>(open === null ? SHOWING : { kind: "naming", text: open, from: open })
  /* The naming as last moved, for a handler that runs before React has drawn it. */
  const now = useRef(naming)
  const move = (next: Naming) => {
    now.current = next
    setNaming(next)
  }
  useUnsaved(naming.kind === "naming" && naming.text !== naming.from)
  const finish = (keep: boolean): string | null | undefined => {
    const was = now.current
    if (was.kind !== "naming") return undefined
    move(SHOWING)
    return keep ? was.text : null
  }
  return { naming, start: (name: string) => move({ kind: "naming", text: name, from: name }), type: (text: string) => move({ kind: "naming", text, from: now.current.kind === "naming" ? now.current.from : text }), finish }
}

/** A heading renamed in place by its pencil, as a single control: the text is its own while the field is open, and Enter or
 * leaving the field gives it to `onFinish`, Esc gives null. With `lines`, the name wraps to that many lines before it is cut, and
 * a click on it renames. While the rename is out (`pending`) the name shows what was sent and the spinner stands in the
 * pencil's place. While another write of its control is out (`waits`) the pencil takes no press and doesn't spin. A name the
 * page refuses (`refuse`) keeps the field open, ringed, with the reason under it: Enter and leaving the field don't take it, and
 * the × beside it, as Esc does, puts the old name back. */
export function NameEdit({ shown, start, placeholder, refuse, pending = false, waits = false, startNaming = false, lines, onFinish }: {
  shown: string; start: string; placeholder?: string; refuse?: (text: string) => string | null; pending?: boolean; waits?: boolean; startNaming?: boolean; lines?: number
  onFinish: (name: string | null) => void
}) {
  const { naming, start: begin, type, finish: close } = useNaming(startNaming ? start : null)
  /* Focused once its effects run, not as it mounts, so a closing sheet's focus trap, which ends in the same commit, can't take
   * the focus back. */
  const field = useRef<HTMLInputElement>(null)
  const open = naming.kind === "naming"
  useEffect(() => { if (open) field.current?.focus() }, [open])
  const refused = naming.kind === "naming" ? refuse?.(naming.text) ?? null : null
  const finish = (keep: boolean) => {
    if (keep && refused !== null) return
    const named = close(keep)
    if (named !== undefined) onFinish(named)
  }
  const rename = () => begin(start)
  if (naming.kind === "showing" && lines) return <LongName name={shown} lines={lines} pending={pending} waits={waits} onRename={rename} />
  if (naming.kind === "showing") {
    return (
      <>
        <h2 className="truncate text-sm font-semibold">{shown}</h2>
        <Button size="icon-xs" variant="ghost" className="text-muted-foreground hover:text-foreground shrink-0" aria-label={`Rename ${shown}`} title="Rename" disabled={pending || waits} aria-busy={pending || undefined} onClick={rename}>
          {pending ? <Spinner role={undefined} aria-label={undefined} aria-hidden /> : <PencilIcon />}
        </Button>
      </>
    )
  }
  return (
    <span className="flex min-w-0 flex-col gap-1">
      <span className="flex min-w-0 items-center gap-1">
        <Input ref={field} aria-label={`Name of ${shown}`} className="h-6 w-48 px-1.5 text-sm font-semibold" value={naming.text} placeholder={placeholder}
          aria-invalid={refused !== null || undefined} aria-describedby={refused !== null ? "name-refused" : undefined}
          onFocus={(e) => e.target.select()} onChange={(e) => type(e.target.value)} onBlur={() => finish(true)}
          onKeyDown={(e) => { if (e.key === "Enter") finish(true); if (e.key === "Escape") finish(false) }} />
        {refused !== null && (
          <Button size="icon-xs" variant="ghost" className="text-muted-foreground hover:text-foreground shrink-0" aria-label="Cancel" title="Cancel"
            onPointerDown={(e) => e.preventDefault()} onClick={() => finish(false)}><XIcon /></Button>
        )}
      </span>
      {refused !== null && <FieldError id="name-refused">{refused}</FieldError>}
    </span>
  )
}
