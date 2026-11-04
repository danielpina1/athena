import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "@/app/App";
import { initTheme } from "@/ui/theme";
import "./index.css";

// Before the first render, so the first paint is already in the owner's theme.
initTheme();

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
