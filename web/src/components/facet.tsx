import { useState, type ReactNode } from "react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { CommandEmpty, CommandGroup, CommandItem, CommandList } from "@/components/ui/command"
import { Separator } from "@/components/ui/separator"
import { NoMatch } from "@/components/page"
import { ListSearch, Menu } from "@/components/pickers"
import { cn } from "@/lib/utils"

/** How a value looks wherever it shows: a table cell, a filter's option, a chip. Its words also sort and search it. */
export type Drawing = { words: string; mark?: ReactNode }

/** A value as its drawing says, inline so its text reads as one line. `wordsClassName` can hide the words from the eye, as a narrow
 * column does, and never from a screen reader. The gap before the words goes with them, so a mark alone fits a column its size. */
export function Drawn({ facet, drawing, wordsClassName }: { facet: string; drawing: Drawing; wordsClassName?: string }) {
  return (
    <span data-facet={facet} className="min-w-0 truncate" title={drawing.words}>
      {drawing.mark && <span aria-hidden className="inline-flex items-center align-middle">{drawing.mark}</span>}
      <span className={cn(drawing.mark && "ml-1.5", wordsClassName)}>{drawing.words}</span>
    </span>
  )
}

/** One value a filter offers, drawn as its column draws it, and what it says at its right: how many of the list's rows have it, or a
 * server's ping. */
export type FacetOption = { value: string; drawing: Drawing; aside: ReactNode }

/** A set's tick list, a stock checkbox per value, with its search where `searched`. A search nothing matches is a muted "No match"
 * with no button, since the search's × empties it; and while the search holds text the footer's Clear, which unpicks the values, is
 * hidden, so it never stands under "No match". */
export function Checklist({ facet, label, options, value, onChange, onClear, searched = true }: {
  facet: string; label: string; options: FacetOption[]; value: string[]; onChange: (value: string[]) => void; onClear?: () => void; searched?: boolean
}) {
  const [search, setSearch] = useState("")
  const [highlighted, setHighlighted] = useState("")
  return (
    <Menu highlighted={highlighted} onHighlight={setHighlighted}>
      {searched && <ListSearch placeholder={label} value={search} onValueChange={setSearch} />}
      {/* What's left of the screen after the search and Clear. */}
      <CommandList className="max-h-[min(20rem,calc(var(--radix-popover-content-available-height)-6rem))]">
        <CommandEmpty className="px-2.5 py-1.5 text-left"><NoMatch /></CommandEmpty>
        <CommandGroup>
          {options.map((o) => {
            const on = value.includes(o.value)
            return (
              <CommandItem key={o.value} value={`${o.drawing.words} ${o.value}`} aria-checked={on} className="[&>svg:last-child]:hidden"
                onSelect={() => onChange(on ? value.filter((v) => v !== o.value) : [...value, o.value])}>
                <Checkbox checked={on} tabIndex={-1} aria-hidden className="pointer-events-none" />
                <Drawn facet={facet} drawing={o.drawing} />
                <span className="text-muted-foreground ml-auto pl-3 tabular-nums">{o.aside}</span>
              </CommandItem>
            )
          })}
        </CommandGroup>
      </CommandList>
      {value.length > 0 && search === "" && (
        <>
          <Separator className="my-1" />
          <Button variant="ghost" className="w-full shrink-0" onClick={() => { onChange([]); onClear?.() }}>Clear</Button>
        </>
      )}
    </Menu>
  )
}
