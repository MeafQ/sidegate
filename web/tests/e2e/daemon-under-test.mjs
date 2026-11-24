// Which daemon a browser check runs against: the fake, or with E2E_DAEMON=real the Go handler.
import { startFakeDaemon } from "./fake-daemon.mjs"
import { startRealDaemon } from "./real-daemon.mjs"

export const REAL = process.env.E2E_DAEMON === "real"
export const startDaemon = REAL ? startRealDaemon : startFakeDaemon

/** The logged requests once the daemon has answered each, and null while any is out or none is logged: both daemons log a
 * request with status 0 until they answer it. */
export const answered = (requests) => (requests.length > 0 && requests.every((r) => r.status !== 0) ? requests : null)
