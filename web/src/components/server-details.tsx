import { useState, type ReactNode } from "react"
import { GaugeIcon, HistoryIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Spinner } from "@/components/ui/spinner"
import { Fact, type Line } from "@/components/card"
import { StabilityCell } from "@/components/server"
import { ServerStats, useServerWeek, type ServerWeek } from "@/components/server-stats"
import { ageLine, RatingMark, Signed } from "@/components/status"
import { actions } from "@/lib/api"
import { stabilityOf } from "@/lib/failures"
import { RATING_WORDS, pingAge, pingPair, rate, serverStanding, testWords, type ServerRating } from "@/lib/health"
import { COUNTRY_SOURCE_WORDS } from "@/lib/feed"
import { Drawn } from "@/components/facet"
import { countryDrawing } from "@/components/flag"
import { PROTOCOL_WORDS, TRANSPORT_WORDS, ago, countryName, ms, msPair } from "@/lib/format"
import { useNow } from "@/lib/clock"
import { isPending, useControl } from "@/lib/use-write"
import type { ServerView, ProbeResult } from "@/lib/schema"

/** A Dot and its word, with the reason in its card under the Dot, where there is one. */
function DotWord({ rating, words, reason }: { rating: ServerRating; words: string; reason: Line[] }) {
  const drawn = <span className="inline-flex items-center gap-1.5"><RatingMark rating={rating} />{words}</span>
  if (reason.length === 0) return drawn
  return <Fact lines={reason} label={`${words}: ${reason.map((l) => l.words).join(", ")}`} className="-my-1.5 inline-flex justify-center align-middle">{drawn}</Fact>
}

/** Health is the Dot and its word. A server nothing checked lately has no reason beyond its age, which the subtitle holds. */
function Health({ server }: { server: ServerView }) {
  const standing = serverStanding(server)
  if (standing === "unsupported") return <DotWord rating="none" words="Unsupported" reason={[{ words: "Xray can't run it" }]} />
  if (standing !== "usable") return <DotWord rating="none" words="Missing" reason={server.missingSince ? [{ name: "Since", words: ago(server.missingSince) }] : []} />
  const { rating, why } = rate(server)
  return <DotWord rating={rating} words={RATING_WORDS[rating]} reason={rating === "none" ? [] : why.map((words) => ({ words }))} />
}

/** What a Test answered: the ping it got, or a Dot and its word with the reason in its card. Where one card serves a whole
 * option, as in a menu, the option's card holds the reason and the answer has none of its own (`carded` false). */
export function TestAnswer({ result, carded = true }: { result: ProbeResult; carded?: boolean }) {
  const failed = testWords(result)
  if (!failed) return <span className="whitespace-nowrap tabular-nums">{ms(result.pingMs)}</span>
  return <DotWord rating={failed.rating} words={failed.words} reason={carded ? [{ words: failed.reason }] : []} />
}

/** Test: the gauge, and the answer standing beside it for the minute the store keeps it (`ANSWER_MS`). A Test is a write whose
 * answer is a result, not a change to the view: the store keeps the last one, and this place shows one that came while it was
 * open, so a reading taken before it opened never reads as current. While it is out the answer's place spins and the gauge takes
 * no press. In the details it is a button with its word; in the table, where its header names it, the gauge alone. */
export function ServerTest({ server, worded = false }: { server: Pick<ServerView, "id" | "label">; worded?: boolean }) {
  const test = useControl(actions.probe(server.id))
  const [before] = useState(test.answer)
  const result = test.answer !== before ? test.answer : null
  const run = () => test.press(undefined, { what: server.label })
  return (
    <span className="flex h-6 items-center justify-end gap-1.5" data-control={actions.probe(server.id).control}>
      {isPending(test.phase) ? <span className="text-muted-foreground flex size-6 items-center justify-center" role="status" aria-label="Testing"><Spinner /></span>
        : result && <TestAnswer result={result} />}
      {worded ? <Button size="sm" variant="outline" disabled={isPending(test.phase)} onClick={run}><GaugeIcon />Test</Button>
        : <Button size="icon-xs" variant="ghost" aria-label="Test" title="Test" disabled={isPending(test.phase)} onClick={run}><GaugeIcon /></Button>}
    </span>
  )
}

/** A fact of the details as a row: its muted name, then its plain value 12px after the names' column. */
function Detail({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div data-slot="row" className="contents">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 truncate">{children}</span>
    </div>
  )
}

export function ServerDetails({ server, source, onClose }: { server: ServerView; source: string; onClose: () => void }) {
  const h = server.health
  const now = useNow()
  const [opening, setOpening] = useState(0)
  const read = useServerWeek(server, opening)
  /* Retry reads the week anew, and while it is out the failed read stands with its Retry Pending, so the dialog stays as it was. */
  const [last, setLast] = useState<Exclude<ServerWeek, { state: "loading" }> | null>(null)
  if (read.state !== "loading" && read !== last) setLast(read)
  const week = read.state === "loading" ? last ?? read : read
  const retry = { retry: () => setOpening((n) => n + 1), retrying: read.state === "loading" }
  const old = pingAge(h, now)
  return (
    /* It opens at once. The week's block waits in its own frame, so the read that lands doesn't resize it. */
    <Dialog open onOpenChange={(o) => { if (!o) onClose() }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{server.label}</DialogTitle>
          <div className="flex min-h-7 flex-wrap items-center justify-between gap-x-3 gap-y-1">
            <DialogDescription className="flex flex-wrap items-center gap-x-2">
              <span>{source} · {PROTOCOL_WORDS[server.protocol]} · {TRANSPORT_WORDS[server.transport]}</span>
              {h.checkedAt && <Signed icon={HistoryIcon} className="text-muted-foreground"><span className="sr-only">Checked </span>{ago(h.checkedAt, now)}</Signed>}
            </DialogDescription>
            {serverStanding(server) === "usable" && <ServerTest server={server} worded />}
          </div>
        </DialogHeader>
        <div data-slot="rows" className="grid grid-cols-[auto_minmax(0,1fr)] items-baseline gap-x-3 gap-y-1.5 tabular-nums">
          <Detail label="Country">
            <Fact lines={[{ words: COUNTRY_SOURCE_WORDS[server.country.source] }]} label={`${countryName(server.country.code)}: ${COUNTRY_SOURCE_WORDS[server.country.source]}`} className="-my-1.5 inline-flex justify-center align-middle">
              <Drawn facet="country" drawing={countryDrawing(server.country.code)} />
            </Fact>
          </Detail>
          <Detail label="Health"><Health server={server} /></Detail>
          <Detail label="Ping p50 / p95">
            {old ? <Fact lines={[ageLine(`Measured ${old}`, old)]} label={`${pingPair(h)}, measured ${old}`} className="text-muted-foreground -my-1.5 inline-flex justify-center align-middle">{pingPair(h)}</Fact> : pingPair(h)}
          </Detail>
          <Detail label="Connect p50 / p95">{msPair(h.newConnectionP50Ms, h.newConnectionP95Ms)}</Detail>
          <Detail label="UDP">{{ passed: "Passed", failed: "Failed", unknown: "—" }[server.udp]}</Detail>
          <Detail label="Stability"><StabilityCell stability={stabilityOf(h, now)} className="ml-0 justify-start" /></Detail>
        </div>
        <ServerStats server={server} week={week} retry={retry} />
      </DialogContent>
    </Dialog>
  )
}
