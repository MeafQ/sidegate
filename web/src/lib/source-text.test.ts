import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { describe, test } from "node:test"
import { readSourceText } from "./source-text.ts"

/** What the daemon takes as pasted text, with a URL on a line of its own: the row 2 review's Clash document, whose url-test group
 * folds its URL onto its own line, and a sing-box document laid out the same way. */
const paste = (name: string) => readFileSync(new URL(`../../tests/paste/${name}`, import.meta.url), "utf8")
const CLASH_WITH_A_URL_LINE = paste("clash-url-line.yaml")
const SING_BOX_WITH_A_URL = paste("sing-box-url.json")

describe("Add source's one field", () => {
  test("one https URL makes a subscription, spaces and blank lines aside", () => {
    assert.deepEqual(readSourceText("  https://sub.example.com/api/sub/abc \n\n"), { kind: "url", url: "https://sub.example.com/api/sub/abc" })
    assert.deepEqual(readSourceText("https://sub.example.com/api/sub/abc\n   \n"), { kind: "url", url: "https://sub.example.com/api/sub/abc" })
  })
  test("one http URL is refused with why", () => {
    assert.deepEqual(readSourceText("http://sub.example.com/x"), { kind: "refused", reason: "Must start with https://" })
    assert.deepEqual(readSourceText(" HTTP://sub.example.com/x \n"), { kind: "refused", reason: "Must start with https://" })
  })
  test("share links are pasted, counted by their scheme:// lines, and a URL among them is the daemon's to read", () => {
    assert.deepEqual(readSourceText("vless://a@nl.example.net:443#NL\nss://b@de.example.net:8388#DE\n\ntrojan://c@se.example.net:443#SE\n"), { kind: "pasted", links: 3 })
    assert.deepEqual(readSourceText("vless://a@nl.example.net:443#NL\nhttps://sub.example.com/x\ntrojan://c@se.example.net:443#SE"), { kind: "pasted", links: 2 })
    assert.deepEqual(readSourceText("https://a.example\nhttps://b.example"), { kind: "pasted", links: null })
  })
  test("Clash, sing-box or Xray text is pasted with no count, a URL in it included", () => {
    assert.deepEqual(readSourceText("proxies:\n  - name: NL\n    type: vless\n    server: nl.example.net"), { kind: "pasted", links: null })
    assert.deepEqual(readSourceText('{ "outbounds": [] }'), { kind: "pasted", links: null })
    assert.deepEqual(readSourceText(CLASH_WITH_A_URL_LINE), { kind: "pasted", links: null })
    assert.deepEqual(readSourceText(SING_BOX_WITH_A_URL), { kind: "pasted", links: null })
  })
  test("anything else one line is pasted, for the daemon to refuse", () => {
    for (const text of ["https://", "not a list of servers"]) assert.deepEqual(readSourceText(text), { kind: "pasted", links: null }, text)
  })
  test("nothing typed is empty", () => {
    assert.deepEqual(readSourceText(" \n "), { kind: "empty" })
  })
})
