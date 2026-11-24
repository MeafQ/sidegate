/** What Add source's one field holds. The page decides one thing: text that is one line and an https URL is a subscription, and one
 * line that is an http URL is refused. Anything else is pasted, for the daemon to read or refuse: share links, counted by their
 * `scheme://` lines, or Clash, sing-box or Xray text, which has none to count. */
export type SourceText =
  | { kind: "empty" }
  | { kind: "url"; url: string }
  | { kind: "pasted"; links: number | null }
  | { kind: "refused"; reason: string }

const WEB = /^https?:\/\//i
const LINK = /^[a-z][a-z0-9+.-]*:\/\//i

export function readSourceText(text: string): SourceText {
  const lines = text.split(/\r?\n/).map((l) => l.trim()).filter(Boolean)
  if (lines.length === 0) return { kind: "empty" }
  if (lines.length === 1) {
    const [line] = lines
    if (/^http:\/\//i.test(line)) return { kind: "refused", reason: "Must start with https://" }
    if (/^https:\/\//i.test(line) && URL.canParse(line) && new URL(line).hostname) return { kind: "url", url: line }
  }
  const links = lines.filter((l) => LINK.test(l) && !WEB.test(l)).length
  return { kind: "pasted", links: links > 0 ? links : null }
}
