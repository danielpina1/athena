import { createContext, useContext } from "react";

// Where overlays (dialog, sheet, menus, tooltips, toasts) portal to. Defaults to document.body; a themed region
// such as a /design panel provides its own element, so its overlays carry its theme.
export const PortalContainer = createContext<HTMLElement | null>(null);

export function usePortalContainer(): HTMLElement | undefined {
  return useContext(PortalContainer) ?? undefined;
}
