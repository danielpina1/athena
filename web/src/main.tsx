import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "@/app/App";
import { registerServiceWorker } from "@/lib/update";
import { initTheme } from "@/ui/theme";
import "./index.css";

// Before the first render, so the first paint is already in the owner's theme.
initTheme();
// The service worker (offline shell, push, updates) only in a build: the dev server's files must never be cached.
if (!import.meta.env.DEV) registerServiceWorker();

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
