// Loads the page's own modules into a node test as Vite builds them: the `@/` alias, `import.meta.env` and TSX resolve,
// so a test calls api.ts or use-write.ts themselves and not a copy. External packages are node's own, the same
// instances the test imports.
import { fileURLToPath } from "node:url"
import { createServer } from "vite"

const WEB = fileURLToPath(new URL("../", import.meta.url))

export async function pageModules(...paths) {
  const vite = await createServer({
    configFile: false, root: WEB, appType: "custom", logLevel: "silent",
    resolve: { alias: { "@": `${WEB}src` } },
    server: { middlewareMode: true, hmr: false, watch: null },
    optimizeDeps: { noDiscovery: true, include: [] },
  })
  const modules = []
  for (const p of paths) modules.push(await vite.ssrLoadModule(p))
  return { modules, close: () => vite.close() }
}
