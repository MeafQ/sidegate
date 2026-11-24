import { useSyncExternalStore } from "react"

let now = Date.now()
const listeners = new Set<() => void>()
let tick: ReturnType<typeof setInterval> | undefined

function subscribe(listener: () => void) {
  if (listeners.size === 0) {
    now = Date.now()
    tick = setInterval(() => {
      now = Date.now()
      listeners.forEach((l) => l())
    }, 30_000)
  }
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
    if (listeners.size === 0) clearInterval(tick)
  }
}

/** The time, again every half minute, so what reads it ("Refreshed 5 m ago", "1 in 12 h") keeps up. It is one clock for the whole
 * page: the list and a source's heading read the same week, so they count the same failovers. */
export const useNow = () => useSyncExternalStore(subscribe, () => now)
