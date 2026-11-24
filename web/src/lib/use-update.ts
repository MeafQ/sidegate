/* The updater as the page runs it: the automatic check, the presses of Check and Update, and the reload over another release. */
import { useEffect, useRef } from "react"
import { CancelledError, useIsMutating, useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query"
import { dismiss, say, type Said } from "@/components/toast"
import { ApiError, api, signIn, updateControls, type UpdateView } from "@/lib/api"
import { keys } from "@/lib/keys"
import { sessionRead, updateRead } from "@/lib/queries"
import { refusalWords, toastWhy } from "@/lib/troubles"
import { moving, pressedSince, type Offer } from "@/lib/update"
import { writesFor } from "@/lib/use-write"
import type { ControlKey } from "@/lib/write"

/** How often the view is read while it is about to change by itself. */
const POLL_MS = 2_000
const PRESS = ["update", "press"]
const CHECK = ["update", "check"]

/** Reads the view afresh, over what an answer brought: the last read asked is the last to land, whatever order answers came in. A
 * read a reconnect cancels is asked again. */
async function reread(qc: QueryClient, answered?: UpdateView): Promise<UpdateView> {
  for (;;) {
    await qc.cancelQueries({ queryKey: keys.update })
    if (answered) qc.setQueryData(updateRead.queryKey, answered)
    try {
      return await qc.fetchQuery({ ...updateRead, staleTime: 0 })
    } catch (error) {
      if (!(error instanceof CancelledError)) throw error
    }
  }
}

/** No answer came, the daemon couldn't say, or the page can't read what it said: the press may have started something. */
const unsure = (error: unknown): boolean => !(error instanceof ApiError) || error.kind === "network" || error.kind === "mismatch" || error.status >= 500

/** What a press that did nothing says, in the words a write's ending has. */
function ending(id: ControlKey, what: string, error: unknown): Said {
  const danger = (why: string, button: Said["button"] = null): Said => ({ id, mark: "danger", what, why, muted: null, button, stays: button !== null })
  if (!(error instanceof ApiError) || unsure(error)) return danger("timed out")
  if (error.kind === "login") return danger(error.status === 401 ? "session expired" : "UniFi refused", { word: "Sign in" })
  if (error.reason === null) return danger(`HTTP ${error.status}`)
  return danger(toastWhy(refusalWords({ reason: error.reason, detail: error.detail, message: error.message })))
}

/** Check for a newer release, as a press. Pending until what it found is shown. */
export function useCheck(): { readonly out: boolean; readonly press: () => void } {
  const qc = useQueryClient()
  const out = useIsMutating({ mutationKey: CHECK }) > 0
  const { mutate } = useMutation({
    mutationKey: CHECK, networkMode: "always",
    mutationFn: async () => {
      dismiss(updateControls.check)
      try {
        const answered = await api.checkForUpdate(false)
        await reread(qc, answered).catch(() => {})
      } catch (error) {
        say(ending(updateControls.check, "Check for updates", error), signIn)
      }
    },
  })
  return { out, press: () => { if (!out) mutate() } }
}

/** Update, confirmed: one press, sent once. Pending until the view says what became of it. One refused, or with no answer and
 * nothing in the view of it, is a toast, and the view shows what is offered now. */
export function useUpdate(): { readonly out: boolean; readonly press: (offer: Offer) => void } {
  const qc = useQueryClient()
  const out = useIsMutating({ mutationKey: PRESS }) > 0
  const { mutate } = useMutation({
    mutationKey: PRESS, networkMode: "always",
    mutationFn: async ({ version, restartsCore }: Offer) => {
      const before = qc.getQueryData(updateRead.queryKey)
      dismiss(updateControls.update)
      try {
        const answered = await api.update({ version, restartsCore })
        await reread(qc, answered).catch(() => {})
      } catch (error) {
        const after = await reread(qc).catch(() => null)
        if (unsure(error) && after && pressedSince(before, after)) return
        say(ending(updateControls.update, "Update", error), signIn)
      }
    },
  })
  return { out, press: (offer) => { if (!out) mutate(offer) } }
}

/** The app shell's part, on whichever tab: the page's one automatic check as it loads, the view read on while an update runs, and
 * the release that answers. Another release's is another page's: the tab reloads, or says so and waits to be told, while that would lose something of the
 * reader's. Each release is acted on once. */
export function useUpdateWatch(): void {
  const qc = useQueryClient()
  useQuery({ ...updateRead, refetchInterval: (query) => (query.state.data && moving(query.state.data) ? POLL_MS : false) })
  useEffect(() => { void api.checkForUpdate(true).then((answered) => reread(qc, answered)).catch(() => {}) }, [qc])
  const { data: session } = useQuery(sessionRead)
  const releases = useRef<{ readonly loaded: string | null; answered: string | null } | null>(null)
  useEffect(() => {
    if (!session) return
    const { release, version } = session
    releases.current ??= { loaded: release, answered: release }
    const known = releases.current
    if (release === known.answered) return
    known.answered = release
    dismiss(updateControls.release)
    if (release === known.loaded) return
    const reload = () => window.location.reload()
    if (writesFor(qc).unsaved()) say({ id: updateControls.release, mark: "ok", what: version, why: null, muted: null, button: { word: "Reload" }, stays: true }, reload)
    else reload()
  }, [session, qc])
}
