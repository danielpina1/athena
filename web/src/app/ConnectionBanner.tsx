// The link's health, said quietly: Athena's mark in the shell (AthenaMark), and a banner only when the link is not open. A reconnect that
// takes under a second says nothing at all; a longer one says "Connecting…", and an offline link says the control
// server is unreachable and when the last update arrived. Signed out is the sign-in screen's (the session gate's).
import { useSyncExternalStore } from "react";
import type { LinkClient, LinkStatus } from "@/lib/link/client";

/** What the shell needs of the link: its status, and its frames for the time of the last update. */
export type LinkSource = Pick<LinkClient, "status" | "onStatus" | "onFrame">;

/** A not-open status shows nothing for this long. */
export const BANNER_DELAY_MS = 1000;

export function useLinkStatus(link: LinkSource): LinkStatus {
  return useSyncExternalStore(
    (cb) => link.onStatus(() => cb()),
    () => link.status,
  );
}

/** The link's state in words, for screen readers (Athena's mark shows it). */
export const LINK_LABEL: Record<LinkStatus, string> = {
  open: "Connected",
  connecting: "Connecting",
  offline: "Offline",
  signedOut: "Signed out",
};


const clock = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" });

/**
 * What the banner shows. downSince is when the link last left open (cleared only by open or signed out): the
 * client flips between offline and connecting on every retry, and those flips neither restart the delay nor hide
 * the banner. sawOffline says this outage has had an offline moment, after which the copy stays "unreachable —
 * retrying" (a connecting attempt is the retrying) instead of changing on every flip. lastUpdate is when the last
 * data frame came (LinkClient.onFrame never passes Ping or Pong).
 */
type Health = { status: LinkStatus; downSince?: number; late: boolean; sawOffline: boolean; lastUpdate?: number; everOpen: boolean };

const isDown = (s: LinkStatus) => s === "connecting" || s === "offline";

/** One watcher per link, for the app's life: it follows the status, times the delay, and notes each data frame. */
const watchers = new WeakMap<LinkSource, { get(): Health; subscribe(cb: () => void): () => void }>();

function watch(link: LinkSource) {
  let w = watchers.get(link);
  if (w) return w;
  let health: Health = { status: link.status, late: false, sawOffline: false, everOpen: link.status === "open" };
  let timer: ReturnType<typeof setTimeout> | undefined;
  const listeners = new Set<() => void>();
  const set = (next: Health) => {
    health = next;
    for (const l of listeners) l();
  };
  const follow = (status: LinkStatus) => {
    if (!isDown(status)) {
      clearTimeout(timer);
      timer = undefined;
      set({ ...health, status, downSince: undefined, late: false, sawOffline: false, everOpen: health.everOpen || status === "open" });
      return;
    }
    const sawOffline = health.sawOffline || status === "offline";
    if (health.downSince !== undefined) {
      set({ ...health, status, sawOffline });
      return;
    }
    const downSince = Date.now();
    timer = setTimeout(() => set({ ...health, late: true }), BANNER_DELAY_MS);
    set({ ...health, status, downSince, late: false, sawOffline });
  };
  follow(link.status);
  link.onStatus(follow);
  // A data frame (Welcome, Snapshot, Delta, result…) means the state was current then. Not shown while the link is
  // open, so nothing re-renders for it.
  link.onFrame(() => {
    health = { ...health, lastUpdate: Date.now() };
  });
  w = {
    get: () => health,
    subscribe(cb) {
      listeners.add(cb);
      return () => listeners.delete(cb);
    },
  };
  watchers.set(link, w);
  return w;
}

/**
 * The link's status as the shell shows it (Athena's mark): once the link has been open, a drop shorter than
 * BANNER_DELAY_MS, the banner's own delay, still shows open, so a reconnect blip flashes nothing. Before the first
 * open it is the link's own status: the mark never says Connected before it has been.
 */
export function useShownLinkStatus(link: LinkSource): LinkStatus {
  const w = watch(link);
  const { status, late, everOpen } = useSyncExternalStore(w.subscribe, w.get);
  return everOpen && isDown(status) && !late ? "open" : status;
}

/**
 * The live region is always in the document (empty while the link is fine), so showing the banner, and the flips of
 * a retrying link, never mount a new region to be announced again.
 */
export function ConnectionBanner({ link }: { link: LinkSource }) {
  const w = watch(link);
  const { status, late, sawOffline, lastUpdate } = useSyncExternalStore(w.subscribe, w.get);
  const shown = isDown(status) && late;
  return (
    <div role="status" className={shown ? "border-b border-line bg-surface-2 px-4 py-2 text-sm" : undefined}>
      {shown &&
        (sawOffline ? (
          <>
            <span className="font-semibold">Control server unreachable — retrying</span>
            {lastUpdate !== undefined && (
              <span className="text-muted">
                {" · Last update "}
                <time dateTime={new Date(lastUpdate).toISOString()}>{clock.format(lastUpdate)}</time>
              </span>
            )}
          </>
        ) : (
          "Connecting…"
        ))}
    </div>
  );
}
