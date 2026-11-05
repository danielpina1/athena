import { useState, useSyncExternalStore } from "react";
import { cn } from "@/ui/cn";

// "4m ago", refreshed once a minute by one timer that every RelativeTime on the page shares. It is a local clock
// tick, not polling: nothing is fetched.

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
let now = Date.now();
let timer: ReturnType<typeof setInterval> | undefined;
const listeners = new Set<() => void>();

function subscribe(l: () => void) {
  listeners.add(l);
  if (timer === undefined) {
    timer = setInterval(() => {
      for (const fn of listeners) fn();
    }, MINUTE);
  }
  return () => {
    listeners.delete(l);
    if (listeners.size === 0 && timer !== undefined) {
      clearInterval(timer);
      timer = undefined;
    }
  };
}
// Fresh to the second whenever React asks, so a row mounted between ticks is not up to a minute behind; stable
// within the second, as useSyncExternalStore requires.
function snapshot() {
  const t = Date.now();
  if (Math.abs(t - now) >= 1000) now = t;
  return now;
}

/** Now, from the shared once-a-minute clock: a component that shows a time relative to it re-renders on each tick. */
export function useNow(): number {
  return useSyncExternalStore(subscribe, snapshot);
}

/** The shortest relative time, for a dense list: now, 2m, 3h, 4d. */
export function formatAgo(at: number, reference: number): string {
  const diff = Math.max(0, reference - at);
  if (diff < MINUTE) return "now";
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)}m`;
  if (diff < DAY) return `${Math.floor(diff / HOUR)}h`;
  return `${Math.floor(diff / DAY)}d`;
}

const day = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short" });
const dayYear = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short", year: "numeric" });
const full = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

/** Compact relative time: just now, 4m ago, 3h ago, 2d ago, in 5m; a date past a week. */
export function formatRelative(at: number, reference: number): string {
  const diff = reference - at;
  const abs = Math.abs(diff);
  if (abs < 45_000) return "just now";
  if (abs < 7 * DAY) {
    const [size, unit] = abs >= DAY ? [DAY, "d"] : abs >= HOUR ? [HOUR, "h"] : [MINUTE, "m"];
    const n = Math.max(1, Math.floor(abs / size));
    return diff > 0 ? `${n}${unit} ago` : `in ${n}${unit}`;
  }
  const date = new Date(at);
  return date.getFullYear() === new Date(reference).getFullYear() ? day.format(date) : dayYear.format(date);
}

export function RelativeTime({ at, className }: { at: Date | number; className?: string }) {
  const tick = useSyncExternalStore(subscribe, snapshot);
  // The shared clock can be up to a second behind (and, before any tick, as old as the chunk); a fresh reading at
  // mount keeps an exact boundary ("5m ago") from flooring one lower.
  const [mountedAt] = useState(() => Date.now());
  const ms = typeof at === "number" ? at : at.getTime();
  if (!Number.isFinite(ms)) {
    return <span className={cn("tabular-nums", className)}>—</span>;
  }
  const date = new Date(ms);
  return (
    <time dateTime={date.toISOString()} title={full.format(date)} className={cn("tabular-nums", className)}>
      {formatRelative(ms, Math.max(tick, mountedAt))}
    </time>
  );
}
