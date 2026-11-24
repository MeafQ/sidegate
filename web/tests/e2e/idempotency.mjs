/** The daemon's rule for one Idempotency-Key, as the fake daemon keeps it. API.md says what each case means for the real
 * daemon. An answer is remembered as { request, answer }. */

export const KEY_REUSED = "Idempotency-Key reused for another request."

/** What a request under `key` gets: handled as new, the first answer again, or refused for changing the request. */
export function checkKey(answers, key, request) {
  const first = answers.get(key)
  if (!first) return { kind: "first" }
  return first.request === request ? { kind: "replay", answer: first.answer } : { kind: "reused" }
}

/** A 5xx leaves the key free, so the request sent again is handled as new. Anything else is its answer for good. */
export function rememberAnswer(answers, key, request, status, answer) {
  if (status < 500) answers.set(key, { request, answer })
}
