import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { createBrowserRouter, RouterProvider } from "react-router"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { ThemeProvider } from "next-themes"
import { Toasts } from "@/components/toast"
import { AppShell } from "@/components/app-shell"
import { pageQueryCache } from "@/components/page"
import { BASE } from "@/lib/api"
import { startPage } from "@/lib/use-write"
import { EventsPage } from "@/pages/events"
import { NotFound } from "@/components/slot-board"
import { ServersPage } from "@/pages/servers"
import { SlotsPage } from "@/pages/slots"
import { SettingsPage } from "@/pages/settings"
import "./index.css"

const queryClient = new QueryClient({
  queryCache: pageQueryCache(),
  defaultOptions: {
    queries: { staleTime: 2_000 },
  },
})

const router = createBrowserRouter([{
  element: <AppShell />,
  children: [
    { path: "/", element: <SlotsPage /> },
    { path: "/servers", element: <ServersPage /> },
    { path: "/sources/:id", element: <ServersPage /> },
    { path: "/groups/:id", element: <ServersPage /> },
    { path: "/events", element: <EventsPage /> },
    { path: "/settings", element: <SettingsPage /> },
    { path: "*", element: <NotFound /> },
  ],
}], { basename: BASE.replace(/\/$/, "") || "/" })

startPage(queryClient, (route, state) => void router.navigate(route, { state }))

const root = document.getElementById("root")
if (root) {
  createRoot(root).render(
    <StrictMode>
      {/* a client-rendered page never runs next-themes' inline script; as a data block React doesn't warn about it */}
      <ThemeProvider attribute="class" defaultTheme="dark" enableSystem disableTransitionOnChange scriptProps={{ type: "application/json" }}>
        <QueryClientProvider client={queryClient}>
          <RouterProvider router={router} />
          <Toasts />
        </QueryClientProvider>
      </ThemeProvider>
    </StrictMode>,
  )
}
