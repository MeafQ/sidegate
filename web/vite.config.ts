import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// The daemon serves the page under https://<gateway>/<name>/. BASE_PATH is that prefix, with both slashes. Its
// default is the daemon's DefaultBasePath, and a Go test checks the two agree.
const base = process.env.BASE_PATH ?? "/sidegate/"

export default defineConfig({
  base,
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": path.resolve(import.meta.dirname, "./src") } },
})
