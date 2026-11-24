process.env.TZ = "Asia/Tokyo"

import assert from "node:assert/strict"
import { describe, test } from "node:test"
import { readdirSync, readFileSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { ago, clock, cutToLines, day, moment, ms, msPair, placePair, refreshWords, size, span, spanUnder, wearsOff } from "./format.ts"

const S = 1000
const H = 3600 * S
const D = 24 * H
const GB = 1024 ** 3

describe("a length of time", () => {
  test("has its unit against its number, and is never under 1m", () => {
    assert.equal(span(0), "1m")
    assert.equal(span(59 * S), "1m")
    assert.equal(span(29 * S), "1m")
  })
  test("is minutes under an hour, rounded, and an hour once it rounds to 60 minutes", () => {
    assert.equal(span(30 * 60 * S), "30m")
    assert.equal(span(59 * 60 * S + 29 * S), "59m")
    assert.equal(span(59 * 60 * S + 31 * S), "1h")
  })
  test("is hours under a day, and a day once it rounds to 24 hours", () => {
    assert.equal(span(23.4 * H), "23h")
    assert.equal(span(23.6 * H), "1d")
  })
  test("is days under 60, then whole months rounded down, and whole years from a year, so never 12mo", () => {
    assert.equal(span(40 * D), "40d")
    assert.equal(span(59.4 * D), "59d")
    assert.equal(span(60 * D), "1mo", "from 60 days whole months, rounded down")
    assert.equal(span(61 * D), "2mo")
    assert.equal(span(310 * D), "10mo")
    assert.equal(span(365 * D), "11mo")
    assert.equal(span(365.25 * D), "1y")
    assert.equal(span(1100 * D), "3y")
  })
  test("reads the same backwards", () => {
    assert.equal(span(-2 * D), "2d")
  })
  test("ago, is just now under a minute", () => {
    const now = Date.parse("2026-11-13T03:00:00Z")
    assert.equal(ago(new Date(now - 59 * S).toISOString(), now), "just now")
    assert.equal(ago(new Date(now - 3 * H).toISOString(), now), "3h ago")
    assert.equal(ago(new Date(now - 400 * D).toISOString(), now), "1y ago")
  })
  test("a pick wears off within the latest it can last, in the page's span words, and under a minute in 1m", () => {
    const now = Date.parse("2026-11-21T12:05:00Z")
    const at = (ms: number) => wearsOff(new Date(now + ms).toISOString(), now)
    assert.deepEqual(at(24 * H - S), { shown: "in 1d", heard: "in 1 day" }, "right after a day-long pick")
    assert.deepEqual(at(2 * H), { shown: "in 2h", heard: "in 2 hours" })
    assert.deepEqual(at(5 * 60 * S), { shown: "in 5m", heard: "in 5 minutes" })
    assert.deepEqual(at(20 * S), { shown: "in 1m", heard: "in 1 minute" })
  })
  test("a pick's time rounds up to the next step of its unit, so it never says less than is left, and never over a day, the longest a pick lasts", () => {
    const now = Date.parse("2026-11-21T12:05:00Z")
    const shown = (ms: number) => wearsOff(new Date(now + ms).toISOString(), now).shown.replace("in ", "")
    const M = 60 * S
    const cases: [number, string][] = [
      [S, "1m"], [59 * S, "1m"], [M, "1m"], [M + S, "2m"], [4 * M + 10 * S, "5m"], [5 * M, "5m"],
      [59 * M, "59m"], [59 * M + 10 * S, "1h"], [H, "1h"], [H + S, "2h"], [H + M, "2h"],
      [2 * H, "2h"], [2 * H + 29 * M, "3h"], [2 * H + S, "3h"],
      [23 * H, "23h"], [23 * H + 10 * M, "1d"], [D, "1d"], [D + 30 * S, "1d"],
    ]
    assert.deepEqual(cases.map(([ms]) => shown(ms)), cases.map(([, words]) => words))
    assert.equal(wearsOff(new Date(now + 2 * H + 29 * M).toISOString(), now).heard, "in 3 hours")
  })
  test("for over N, is the longest span it passes, each unit rounded down, and none under a minute", () => {
    assert.deepEqual([59 * S, 59.9 * 60 * S, 23.9 * H, 59.9 * D, 1094 * D].map(spanUnder), [null, { n: 59, unit: "m" }, { n: 23, unit: "h" }, { n: 59, unit: "d" }, { n: 2, unit: "y" }])
  })
})

describe("a size", () => {
  test("has one decimal under 100 GB and none from 100, judged after rounding", () => {
    assert.equal(size(99.94 * GB), "99.9 GB")
    assert.equal(size(99.95 * GB), "100 GB")
    assert.equal(size(18.6 * GB), "18.6 GB")
    assert.equal(size(512 * GB), "512 GB")
  })
  test("is TB from 1000 GB, judged after rounding", () => {
    assert.equal(size(999.4 * GB), "999 GB")
    assert.equal(size(999.95 * GB), "1.0 TB")
    assert.equal(size(2048 * GB), "2.0 TB")
  })
  test("is MB under 1 GB", () => {
    assert.equal(size(0.5 * GB), "512 MB")
    assert.equal(size(0.94 * GB), "963 MB")
    assert.equal(size(0.96 * GB), "1.0 GB")
    assert.equal(size(20 * 1024 ** 2), "20.0 MB")
  })
  test("reads zero as 0 MB, judged after rounding", () => {
    assert.equal(size(0), "0 MB")
    assert.equal(size(40 * 1024), "0 MB")
    assert.equal(size(0.4 * 1024 ** 2), "0.4 MB")
  })
})

describe("a date", () => {
  /** Friday 13 November 2026, 12:00 in Tokyo. */
  const NOW = Date.parse("2026-11-13T03:00:00Z")
  test("says its year only when it is more than 12 months from now, before or after", () => {
    assert.equal(day("2026-11-28T03:00:00Z", NOW), "Nov 28")
    assert.equal(day("2027-11-13T02:59:00Z", NOW), "Nov 13", "12 months ahead to the minute")
    assert.equal(day("2027-11-13T03:01:00Z", NOW), "Nov 13, 2027")
    assert.equal(day("2025-11-13T03:01:00Z", NOW), "Nov 13")
    assert.equal(day("2025-11-13T02:59:00Z", NOW), "Nov 13, 2025")
  })
  test("says it in every form the page uses", () => {
    assert.equal(moment("2026-11-12T00:20:00Z", NOW), "Nov 12, 09:20")
    assert.equal(moment("2030-03-02T15:00:00Z", NOW), "Mar 3, 2030, 00:00")
    assert.equal(clock("2026-11-13T00:20:00Z", NOW), "09:20", "today is the time alone")
    assert.equal(clock("2026-11-01T09:04:00Z", NOW), "Nov 1, 18:04")
    assert.equal(clock("2024-11-01T09:04:00Z", NOW), "Nov 1, 2024, 18:04")
  })
})

describe("a name cut to three lines", () => {
  /** Lines of 10 characters: a word moves whole to the next line unless it is longer than a line, and the pencil takes 2 more. */
  const measure = (text: string, bare = false) => {
    let [lines, used] = [1, 0]
    for (const word of (bare ? text : `${text} ✎✎`).split(" ")) {
      const room = used === 0 ? 10 : 10 - used - 1
      if (word.length <= room) used += (used === 0 ? 0 : 1) + word.length
      else if (word.length <= 10) [lines, used] = [lines + 1, word.length]
      else { lines += Math.ceil((word.length - room) / 10); used = (word.length - room) % 10 || 10 }
    }
    return lines
  }
  test("is whole while it fits", () => {
    assert.equal(cutToLines("Aurora fast", 3, measure), "Aurora fast")
  })
  test("is cut by the word, and a comma left at the cut goes", () => {
    assert.equal(cutToLines("Aurora fast, Abcdefghi more", 3, measure), "Aurora fast…")
    assert.equal(cutToLines("Aurora fast, Europe, and Asia Pacific", 3, measure), "Aurora fast, Europe…")
  })
  test("is cut by the letter when one word is too long for a line of its own, and a bracket, hyphen or full stop left at the cut goes", () => {
    assert.equal(cutToLines("Supercalifragilisticexpialidocious", 3, measure), "Supercalifragilisticexpial…")
    for (const mark of ["(", "-", "."]) assert.equal(cutToLines(`Abcdefghijklmnopqrstuvwxy${mark}z0123456789`, 3, measure), "Abcdefghijklmnopqrstuvwxy…", mark)
  })
})

test("a refresh interval reads as Settings writes it, a day as 24h", () => {
  assert.deepEqual([3600, 4 * 3600, 86_400, 2 * 86_400].map(refreshWords), ["1h", "4h", "24h", "2d"])
})

describe("a ping", () => {
  test("is written in whole ms, alone and as a pair, and is a dash with no reading", () => {
    assert.equal(ms(67.39099999999999), "67 ms")
    assert.equal(ms(null), "—")
    assert.equal(msPair(67.39099999999999, 69.601), "67 / 70 ms")
    assert.equal(msPair(67.4, null), "—")
  })
  test("on a chart's place is its pair, with the dash for a figure it lacks and nothing where it has neither", () => {
    assert.equal(placePair(70.2, 103.6), "70 / 104 ms")
    assert.equal(placePair(1204, 3980), "1204 / 3980 ms")
    assert.equal(placePair(76, null), "76 / — ms")
    assert.equal(placePair(null, 52), "— / 52 ms")
    assert.equal(placePair(null, null), "")
  })
  test("is rounded by the one writer: no other file rounds a figure in ms or writes the unit after a bare number", () => {
    const src = fileURLToPath(new URL("../", import.meta.url))
    const files = readdirSync(src, { recursive: true, encoding: "utf8" }).filter((f) => /\.tsx?$/.test(f) && !/\.test\.ts$/.test(f) && !f.endsWith("lib/format.ts"))
    const found = files.flatMap((f) => readFileSync(src + f, "utf8").split("\n").flatMap((line, i) =>
      /Math\.round\([^)]*(Ms|score)\b|\}\s?ms\b|String\(value\)\} ms/.test(line) ? [`${f}:${i + 1}`] : []))
    assert.deepEqual(found, [])
  })
})
