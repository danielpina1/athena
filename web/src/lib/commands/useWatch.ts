// useWatch(ticketId): the Ticket's detail Deltas for as long as a screen shows it. One count per Ticket, so a detail
// pane and a pinned session of the same Ticket share one watch: the first user sends watch, the last sends unwatch.
// The last user's unwatch waits one tick, so a StrictMode double mount (unmount, then mount at once) or a quick move
// between two screens of the same Ticket sends nothing at all. The unwatch forgets the Ticket's detail in the store.
//
// A watch can be lost behind the screen's back: the outbox re-sends it after a reconnect and the server may refuse
// (the Ticket is gone, or too many are watched), or the first watch is refused. `lost` says so, and resume tries again.
import { useEffect, useSyncExternalStore } from "react";
import { useAthena } from "@/lib/store/state";
import { onWatchLost, unwatch, watch } from "./actions";

/** Screens showing each Ticket now. */
const users = new Map<string, number>();
/** Tickets a watch was sent for and not lost or unwatched since. */
const watching = new Set<string>();
/** Unwatches waiting their tick, by Ticket. */
const releasing = new Map<string, ReturnType<typeof setTimeout>>();

let lost: ReadonlySet<string> = new Set();
const listeners = new Set<() => void>();

function setLost(id: string, value: boolean) {
  if (lost.has(id) === value) return;
  const next = new Set(lost);
  if (value) next.add(id);
  else next.delete(id);
  lost = next;
  for (const l of listeners) l();
}

function subscribe(l: () => void) {
  listeners.add(l);
  return () => {
    listeners.delete(l);
  };
}

function markLost(id: string) {
  watching.delete(id);
  setLost(id, true);
}

function send(id: string) {
  watching.add(id);
  setLost(id, false);
  void watch(id).then((r) => {
    if (!r.ok) markLost(id);
  });
}

function acquire(id: string) {
  const pending = releasing.get(id);
  if (pending !== undefined) {
    clearTimeout(pending);
    releasing.delete(id);
  }
  users.set(id, (users.get(id) ?? 0) + 1);
  if (!watching.has(id)) send(id);
}

function release(id: string) {
  const n = (users.get(id) ?? 1) - 1;
  if (n > 0) {
    users.set(id, n);
    return;
  }
  users.delete(id);
  releasing.set(
    id,
    setTimeout(() => {
      releasing.delete(id);
      if (watching.delete(id)) void unwatch(id);
      setLost(id, false);
      // No screen shows it now: its detail goes too (ruling R28), and comes back with the next watch.
      useAthena.getState().forgetDetail(id);
    }, 0),
  );
}

export type Watch = {
  /** Live details stopped for this Ticket. */
  lost: boolean;
  /** Watches again after a loss. */
  resume(): void;
};

export function useWatch(ticketId: string): Watch {
  useEffect(() => {
    acquire(ticketId);
    return () => release(ticketId);
  }, [ticketId]);
  useEffect(
    () =>
      onWatchLost((id) => {
        if (id === ticketId) markLost(id);
      }),
    [ticketId],
  );
  const isLost = useSyncExternalStore(subscribe, () => lost.has(ticketId));
  return {
    lost: isLost,
    resume() {
      if ((users.get(ticketId) ?? 0) > 0 && !watching.has(ticketId)) send(ticketId);
    },
  };
}

/** Test only: forgets every count, watch and pending unwatch, so one test's Tickets cannot leak into the next. */
export function __resetWatchesForTest(): void {
  for (const t of releasing.values()) clearTimeout(t);
  releasing.clear();
  users.clear();
  watching.clear();
  lost = new Set();
  for (const l of listeners) l();
}
