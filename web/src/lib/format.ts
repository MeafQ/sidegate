import type { DiagnosticCode, Failure, FailureCode, HttpFailure, Protocol, SignalKind, SwitchReason, Transport } from "./schema.ts"

export const PROTOCOL_WORDS = {
  vless: "VLESS", vmess: "VMess", trojan: "Trojan", shadowsocks: "Shadowsocks", socks: "SOCKS", http: "HTTP", hysteria2: "Hysteria2",
} as const satisfies Record<Protocol, string>

export const TRANSPORT_WORDS = { tcp: "TCP", ws: "WebSocket", grpc: "gRPC", xhttp: "XHTTP", hysteria: "Hysteria" } as const satisfies Record<Transport, string>

export const REASON_WORDS = {
  initialSelection: "Initial",
  failureMove: "Server failed",
  slownessMove: "Slow",
  improvementMove: "Lower ping",
  exitChanged: "Exit changed",
  serverLeftFeed: "Server removed",
  serverRefused: "Server unsupported",
  revisionChanged: "Server updated",
  recovery: "Recovered",
  udpMove: "UDP broken",
  preferred: "Manual",
} as const satisfies Record<SwitchReason, string>

export const REPLACED_WORDS = "Server replaced"

export const SIGNAL_WORDS = {
  tcpInfo: "TCP stats", outboundCounters: "Counters", coreWarning: "Xray warning", activeProbe: "Check", wanBaseline: "WAN baseline",
} as const satisfies Record<SignalKind, string>

export const DIAGNOSTIC_WORDS = {
  unsupported_protocol: "Unsupported protocol",
  unsupported_transport: "Unsupported transport",
  unsupported_security: "Unsupported security",
  unsupported_option: "Unsupported option",
  unsupported_cipher: "Unsupported cipher",
  unsupported_plugin: "Unsupported plugin",
  insecure_tls: "Insecure TLS",
  malformed_entry: "Malformed",
  invalid_server: "Invalid server",
  unresolved_dialer: "Unusable dialer",
  duplicate_tag: "Duplicate tag",
} as const satisfies Record<DiagnosticCode, string>

export const FAILURE_WORDS = {
  invalid_location: "Invalid URL",
  address_blocked: "Not a public address",
  too_many_redirects: "Too many redirects",
  redirect_refused: "Unsafe redirect",
  timeout: "Timeout",
  network: "Connection failed",
  too_large: "Too large",
  bad_encoding: "Unreadable answer",
  unexpected_304: "Unexpected HTTP 304",
  malformed: "Malformed answer",
  unknown_format: "Unknown format",
  too_many_entries: "Too many entries",
  no_usable_entries: "No usable servers",
} as const satisfies Record<FailureCode, string>

const isHttp = (code: Failure): code is HttpFailure => code.startsWith("http_")
export const failureWords = (code: Failure) => (isHttp(code) ? `HTTP ${code.slice("http_".length)}` : FAILURE_WORDS[code])

/** "18:04" today, "Nov 1, 18:04" another day. */
export function clock(iso: string, now = Date.now()): string {
  const d = new Date(iso)
  return d.toDateString() === new Date(now).toDateString() ? time(iso) : `${day(iso, now)}, ${time(iso)}`
}

/** Whether a date is more than 12 months from now, before or after, so that its year is worth saying. */
function yearAway(d: Date, now: number): boolean {
  const [before, after] = [new Date(now), new Date(now)]
  before.setFullYear(before.getFullYear() - 1)
  after.setFullYear(after.getFullYear() + 1)
  return d < before || d > after
}

/** "Nov 28", and "Nov 28, 2027" more than 12 months from now. */
export function day(iso: string, now = Date.now()): string {
  const d = new Date(iso)
  return d.toLocaleDateString("en-US", { month: "short", day: "numeric", ...(yearAway(d, now) && { year: "numeric" }) })
}

/** "14:05", in local time. */
export const time = (iso: string) => new Date(iso).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" })

/** A stretch of hours by its local day and its ends, "Mon 06:00–12:00", as a week chart's tooltip names the place it is on. */
export const hoursWords = (start: number, end: number) =>
  `${new Date(start).toLocaleDateString("en-US", { weekday: "short" })} ${time(new Date(start).toISOString())}–${time(new Date(end).toISOString())}`

/** How long something was in use, rounded down as `spanUnder` does, so it never says more than passed: "22h", "6d", "0m". */
export const inUseWords = (ms: number) => spanWords(spanUnder(ms) ?? { n: 0, unit: "m" })

/** What the daemon refreshes a source with no interval of its own or its provider's: catalogue.DefaultRefresh. */
export const DEFAULT_REFRESH = 6 * 3600

/** A refresh interval as Settings writes it, "4h", and a day "24h". */
export const refreshWords = (seconds: number) => (seconds === 86_400 ? "24h" : every(seconds))

/** "6h", "30m", "1d". */
export function every(seconds: number): string {
  if (seconds % 86_400 === 0) return `${seconds / 86_400}d`
  if (seconds % 3600 === 0) return `${seconds / 3600}h`
  return `${Math.round(seconds / 60)}m`
}

const MINUTE = 60_000
const HOUR = 3_600_000
/** A day's two letters, "Tu", the one name a week's picture gives a day, the week's card and the charts' day axis alike. Two
 * tell every day of a week apart, where one letter gives two days T and two S. */
export const dayLetter = (at: number | string) => new Date(at).toLocaleDateString("en-US", { weekday: "short" }).slice(0, 2)

export const DAY = 86_400_000
const YEAR = 365.25 * DAY
const MONTH = YEAR / 12
const MONTHS_FROM_DAYS = 60

/** A length of time as the page says it. */
export type Span = { n: number; unit: "m" | "h" | "d" | "mo" | "y" }

/** Months and years are whole and rounded down, as a person says them, so a span under a year never reads "12mo". */
const long = (ms: number): Span => (ms < YEAR ? { n: Math.floor(ms / MONTH), unit: "mo" } : { n: Math.floor(ms / YEAR), unit: "y" })

/** Minutes under an hour and never under 1m, hours under a day, days under 60, then months and years. Minutes, hours and days are
 * rounded to the nearest, and the unit is chosen after rounding, so 23.6 hours is 1d. */
export function spanOf(ms: number): Span {
  const minutes = Math.round(ms / MINUTE)
  if (minutes < 60) return { n: Math.max(1, minutes), unit: "m" }
  const hours = Math.round(ms / HOUR)
  if (hours < 24) return { n: hours, unit: "h" }
  const days = Math.round(ms / DAY)
  return days < MONTHS_FROM_DAYS ? { n: days, unit: "d" } : long(ms)
}

/** The longest span `ms` passes, for "over N": each unit rounded down, so the words never claim more. Under a minute, none. */
export function spanUnder(ms: number): Span | null {
  const minutes = Math.floor(ms / MINUTE)
  if (minutes < 1) return null
  if (minutes < 60) return { n: minutes, unit: "m" }
  const hours = Math.floor(ms / HOUR)
  if (hours < 24) return { n: hours, unit: "h" }
  const days = Math.floor(ms / DAY)
  return days < MONTHS_FROM_DAYS ? { n: days, unit: "d" } : long(ms)
}

/** The page's one way to write a time: its unit against its number, "59m", "10h", "3d", "10mo", "2y". */
export const spanWords = ({ n, unit }: Span) => `${n}${unit}`
/** "3d": how long, whichever way it runs. */
export const span = (ms: number) => spanWords(spanOf(Math.abs(ms)))
/** "3h ago", and "just now" under a minute. */
export const ago = (iso: string, now = Date.now()) => (now - Date.parse(iso) < MINUTE ? "just now" : `${span(now - Date.parse(iso))} ago`)

/** How a time is heard: "3 days", "1 hour". */
const SPOKEN = { m: "minute", h: "hour", d: "day", mo: "month", y: "year" } as const satisfies Record<Span["unit"], string>
export const spanSpoken = ({ n, unit }: Span) => `${n} ${SPOKEN[unit]}${n === 1 ? "" : "s"}`

/** The shortest span `ms` stays within, for "within N": each unit rounded up and the unit chosen after rounding, so 59m10s is 1h
 * and 23h10m is 1d, and never under 1m. */
function spanWithin(ms: number): Span {
  const minutes = Math.max(1, Math.ceil(ms / MINUTE))
  if (minutes < 60) return { n: minutes, unit: "m" }
  const hours = Math.ceil(ms / HOUR)
  return hours < 24 ? { n: hours, unit: "h" } : { n: Math.ceil(ms / DAY), unit: "d" }
}

/** When a pick the daemon holds wears off, by its end: "in 2h", heard "in 2 hours". The end is the latest it can last,
 * since the daemon drops it sooner when the server fails or the boost runs out on a slower one. No pick lasts over a day (the
 * daemon's preferFor), so one made since the page's half-minute clock last ticked still reads 1d, not 2d. */
export function wearsOff(endsAt: string, now: number): { shown: string; heard: string } {
  const left = spanWithin(Math.min(Date.parse(endsAt) - now, DAY))
  return { shown: `in ${spanWords(left)}`, heard: `in ${spanSpoken(left)}` }
}

const BYTES = { MB: 1024 ** 2, GB: 1024 ** 3, TB: 1024 ** 4 } as const
type SizeUnit = keyof typeof BYTES
const tenths = (v: number) => Math.round(v * 10) / 10
/** One decimal under 100 and none from 100, judged after rounding. */
const figure = (v: number) => (tenths(v) < 100 ? tenths(v).toFixed(1) : String(Math.round(v)))

/** MB under 1 GB, GB, and TB from 1000 GB, each judged after rounding. */
export function unitOf(bytes: number): SizeUnit {
  const gb = tenths(bytes / BYTES.GB)
  return gb >= 1000 ? "TB" : gb >= 1 ? "GB" : "MB"
}

/** Traffic as providers count it, in units of 1024. Zero is "0 MB". */
export function size(bytes: number): string {
  const unit = unitOf(bytes)
  return unit === "MB" && tenths(bytes / BYTES.MB) === 0 ? "0 MB" : `${figure(bytes / BYTES[unit])} ${unit}`
}

/** A part in its whole's unit: "100 of 250 GB", and "0 of 250 GB" for nothing, or "100 / 250 GB" with "/" between. */
export const sizeOf = (part: number, whole: number, between: "of" | "/" = "of") => `${part > 0 ? figure(part / BYTES[unitOf(whole)]) : "0"} ${between} ${size(whole)}`

/** Nothing, in a whole's unit: "0 GB". */
export const noSize = (whole: number) => `0 ${unitOf(whole)}`

/** "1 failover", "3 failovers". */
export const counted = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

/** What is left of a name cut to fit `lines` lines: by the word, or by the letter when the word at the cut is too long for a line of
 * its own, and a comma, opening bracket, hyphen or full stop left at the cut goes too. `measure` lays out a text and says how many lines it
 * takes, the pencil after it included unless `bare`. */
export function cutToLines(name: string, lines: number, measure: (text: string, bare?: boolean) => number): string {
  if (measure(name) <= lines) return name
  const cut = (n: number) => `${name.slice(0, n).replace(/[\s,.([{-]+$/u, "")}…`
  let [fits, over] = [0, name.length]
  while (over - fits > 1) {
    const mid = (fits + over) >> 1
    if (measure(cut(mid)) <= lines) fits = mid
    else over = mid
  }
  const start = name.lastIndexOf(" ", fits)
  if (start <= 0 || /\s/u.test(name[fits])) return cut(fits)
  const end = name.indexOf(" ", fits)
  return measure(name.slice(start + 1, end < 0 ? undefined : end), true) <= 1 ? cut(start) : cut(fits)
}

/** "Nov 23, 14:05", in local time, with the year more than 12 months from now. */
export const moment = (iso: string, now = Date.now()) => `${day(iso, now)}, ${time(iso)}`


/** A ping's figure in whole ms, "67": every ping and every time the page measures in ms is written from it. */
export const msFigure = (v: number) => String(Math.round(v))
/** "67 ms", or the dash where there is no reading. */
export const ms = (v: number | null) => (v === null ? "—" : `${msFigure(v)} ms`)
/** "67 / 70 ms": typical and bad case, or the dash where either is missing. */
export const msPair = (p50: number | null, p95: number | null) => (p50 === null || p95 === null ? "—" : `${msFigure(p50)} / ${msFigure(p95)} ms`)
/** A chart's place's p50 and p95, "76 / 104 ms", with the dash for the figure it lacks, "76 / — ms", or nothing where it has neither. */
export const placePair = (p50: number | null, p95: number | null) =>
  (p50 === null && p95 === null ? "" : `${p50 === null ? "—" : msFigure(p50)} / ${p95 === null ? "—" : msFigure(p95)} ms`)
export const percent = (v: number | null, digits = 1) => (v === null ? "—" : `${(v * 100).toFixed(digits)}%`)

const REGION = new Intl.DisplayNames(["en"], { type: "region" })
export const countryName = (code: string | null) => (code ? REGION.of(code) ?? code : "Unknown")
