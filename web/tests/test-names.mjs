// Loaded with `node --import ./tests/test-names.mjs` before a test file, it gathers the file's test names in
// `globalThis.testNames` and runs none of them. node:test becomes a stand-in that records each name, so no hook builds the page
// or starts a browser.
import { registerHooks } from "node:module"

const names = []
globalThis.testNames = names
const STAND_IN = `
const names = globalThis.testNames
export function test(name) { names.push(name) }
export function describe(_name, ...rest) { rest.find((a) => typeof a === "function")() }
export function before() {}
export function after() {}
export function beforeEach() {}
export function afterEach() {}
export const mock = {}
`

registerHooks({
  resolve: (specifier, context, next) =>
    specifier === "node:test" ? { url: `data:text/javascript,${encodeURIComponent(STAND_IN)}`, shortCircuit: true } : next(specifier, context),
})
