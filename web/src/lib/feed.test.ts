import assert from "node:assert/strict"
import { describe, test } from "node:test"
import { skippedOf } from "./feed.ts"
import { ServerView, SourceView, type DiagnosticCode, type FeedConfig, type LineageId } from "./schema.ts"

const { acme } = await import(new URL("../../tests/e2e/fake-daemon.mjs", import.meta.url).href)

const reference = () => {
  const { servers, source } = acme() as { servers: unknown[]; source: unknown }
  return { servers: servers.map((s) => ServerView.parse(s)), source: SourceView.parse(source) }
}

describe("the reference feed", () => {
  test("reads 6 configs of 13 servers, 12 distinct", () => {
    const { source } = reference()
    assert.ok(source.feed)
    assert.deepEqual([source.feed.configs.length, source.feed.configs.every((c) => c.servers === 13), source.stored], [6, true, 12])
    assert.ok(source.feed.configs.every((c) => c.skipped.length === 5 && c.skipped.every((s) => s.reason === "helper")))
  })

  test("names each server from the config that calls it by a role, with its country from its tags' flag", () => {
    const { servers, source } = reference()
    const sweden = source.feed?.configs[2] as FeedConfig
    for (const role of ["proxy", "EDGE-STANDBY"]) {
      const server = servers.find((s) => s.label === `Sweden · ${role}`) as ServerView
      assert.deepEqual([server.name, server.home, server.country], [{ remarks: "Sweden", tag: role }, sweden.key, { code: "SE", source: "tagFlag" }])
    }
    assert.equal(servers.length, 12)
  })
})

describe("a source's skipped entries", () => {
  const refused = (index: number, tag: string, position: number, code: "insecure_tls" | "unsupported_protocol", field: string) =>
    ({ reason: "refused", index, tag, position, code, field }) as const
  const feed = {
    configs: [
      { key: "a" as LineageId, place: 1, name: "Sweden", servers: 2, skipped: [{ reason: "helper", index: 4, tag: "DIRECT", protocol: "freedom" } as const, refused(3, "proxy", 3, "insecure_tls", "allowInsecure")] },
      { key: null, place: 2, name: "Warsaw", servers: 0, skipped: [refused(1, "", 1, "unsupported_protocol", "wireguard")] },
      { key: "c" as LineageId, place: 3, name: "Edge", servers: 1, skipped: [{ reason: "hop", index: 2, tag: "relay" } as const] },
    ],
  }
  test("show once each: a config's own refusals under it, a config with no server's and a diagnostic no config lists loose", () => {
    const diagnostics = [{ entry: 2, code: "insecure_tls", field: "allowInsecure" }, { entry: 3, code: "unsupported_protocol", field: "wireguard" }, { entry: 9, code: "malformed_entry", field: "" }] as const
    const { byConfig, loose } = skippedOf({ feed, diagnostics: [...diagnostics] })
    assert.deepEqual([...byConfig], [["a", [{ config: "Sweden", name: "proxy", reason: "Insecure TLS" }]]])
    assert.deepEqual(loose, [{ config: "Warsaw", name: "#1", reason: "Unsupported protocol (wireguard)" }, { config: null, name: "Entry 9", reason: "Malformed" }])
  })
  test("an unsupported option or protocol names the one it found, except a protocol that is only other", () => {
    const reasons = (code: DiagnosticCode, ...fields: string[]) => skippedOf({ feed: null, diagnostics: fields.map((field) => ({ entry: 1, code, field })) }).loose.map((s) => s.reason)
    assert.deepEqual(reasons("unsupported_option", "finalmask", "other", ""), ["Unsupported option (finalmask)", "Unsupported option (other)", "Unsupported option"])
    assert.deepEqual(reasons("unsupported_protocol", "wireguard", "other", ""), ["Unsupported protocol (wireguard)", "Unsupported protocol", "Unsupported protocol"])
    assert.deepEqual(reasons("insecure_tls", "allowInsecure"), ["Insecure TLS"])
  })
  test("before a feed is read, every diagnostic is loose", () => {
    assert.deepEqual(skippedOf({ feed: null, diagnostics: [{ entry: 3, code: "unresolved_dialer", field: "dialerProxy" }] }).loose, [{ config: null, name: "Entry 3", reason: "Unusable dialer" }])
  })
})
