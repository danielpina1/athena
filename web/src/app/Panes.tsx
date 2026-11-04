// A view's panes. On the desktop (1024 px and up) a view with a list shows it in the 400 px list pane and its detail
// in the pane beside it; a view without one (New, Graph, Settings) spans both. On the phone there is one column: the
// detail when one is open (or when there is no list), the list otherwise.
import { createContext, type ReactNode, useContext, useSyncExternalStore } from "react";

export type Layout = "phone" | "desktop";

export const DESKTOP_QUERY = "(min-width: 1024px)";

const LayoutContext = createContext<Layout>("phone");
export const LayoutProvider = LayoutContext.Provider;
export const useLayout = () => useContext(LayoutContext);

/** Whether the media query matches, following resizes. */
export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (cb) => {
      const mq = typeof window.matchMedia === "function" ? window.matchMedia(query) : undefined;
      mq?.addEventListener("change", cb);
      return () => mq?.removeEventListener("change", cb);
    },
    () => (typeof window.matchMedia === "function" ? window.matchMedia(query).matches : false),
  );
}

/** Whether the viewport is desktop-wide, following resizes. */
export function useIsDesktop(): boolean {
  return useMediaQuery(DESKTOP_QUERY);
}

export function Panes({ list, detail, showDetail = false }: { list?: ReactNode; detail: ReactNode; showDetail?: boolean }) {
  const layout = useLayout();
  if (layout === "phone") {
    return <div data-pane={list && !showDetail ? "list" : "detail"}>{list && !showDetail ? list : detail}</div>;
  }
  if (!list) {
    return (
      <div data-pane="full" className="min-w-0 flex-1 overflow-y-auto">
        {detail}
      </div>
    );
  }
  return (
    <>
      <div data-pane="list" className="w-[400px] shrink-0 overflow-y-auto border-r border-line">
        {list}
      </div>
      <div data-pane="detail" className="min-w-0 flex-1 overflow-y-auto">
        {detail}
      </div>
    </>
  );
}
