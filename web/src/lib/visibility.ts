// Tells the server whether the owner is looking at this dashboard: Visibility{visible} on visibilitychange, focus and
// blur, and after every reconnect. Plan 3 holds back push notifications while a client reports visible, so a phone in
// the pocket gets them and an open desktop tab does not. Visible means shown and focused: a desktop tab behind another
// window is not being looked at. A change is told only when the value changes; a new connection always hears it.
import * as actions from "@/lib/commands/actions";
import type { LinkClient } from "@/lib/link/client";

export type VisibilityLink = Pick<LinkClient, "status" | "onStatus">;

const visibleNow = () => document.visibilityState === "visible" && document.hasFocus();

/** Starts reporting on link; returns the stop. send is the Visibility command (actions.visibility in the app). */
export function trackVisibility(link: VisibilityLink, send: (visible: boolean) => unknown = actions.visibility): () => void {
  let told: boolean | undefined;
  const tell = (always: boolean) => {
    const v = visibleNow();
    if (!always && v === told) return;
    // Not open: nothing is sent now, and the next open tells the value of that moment.
    if (link.status !== "open") return;
    told = v;
    send(v);
  };
  const onChange = () => tell(false);
  const offStatus = link.onStatus((s) => {
    if (s === "open") tell(true);
  });
  document.addEventListener("visibilitychange", onChange);
  window.addEventListener("focus", onChange);
  window.addEventListener("blur", onChange);
  if (link.status === "open") tell(true);
  return () => {
    offStatus();
    document.removeEventListener("visibilitychange", onChange);
    window.removeEventListener("focus", onChange);
    window.removeEventListener("blur", onChange);
  };
}
