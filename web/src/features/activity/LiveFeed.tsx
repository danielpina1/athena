// The live feed: the important events across every Ticket as they happen (lib/store/activity), newest first. Each row
// links its Ticket and says what happened and how long ago. New rows wash in; a polite live region announces only the
// new ones, at most one announcement every few seconds, never the whole list. The desktop shows it in the sidebar;
// the phone at the bottom of Needs you, the latest 5 until "Show all".
import { useEffect, useId, useRef, useState } from "react";
import { Link } from "react-router";
import { ticketRoute } from "@/app/search";
import { type ActivityItem, useActivity } from "@/lib/store/activity";
import { selectCounts, selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { cn } from "@/ui/cn";
import { Dot } from "@/ui/athena/Dot";
import { formatAgo, useNow } from "@/ui/athena/RelativeTime";

/** The live region speaks at most this often. */
const ANNOUNCE_EVERY_MS = 5_000;

const say = (i: ActivityItem) => `${i.ticketId}: ${i.event}`;

/** An item as it is now: a repeat of its event makes it new again. */
const version = (i: ActivityItem) => `${i.id}:${i.count}`;

/** Announces the items that arrive after it mounts, throttled: a burst is one sentence with the latest. */
function Announcer({ items }: { items: readonly ActivityItem[] }) {
  const [message, setMessage] = useState("");
  const state = useRef<{ seen: Set<string>; pending: ActivityItem[]; last: number; timer?: ReturnType<typeof setTimeout> }>(
    { seen: new Set(items.map(version)), pending: [], last: 0 },
  );
  useEffect(() => {
    const st = state.current;
    for (let k = items.length - 1; k >= 0; k--) {
      const i = items[k];
      if (st.seen.has(version(i))) continue;
      st.seen.add(version(i));
      st.pending.push(i);
    }
    if (st.pending.length === 0 || st.timer !== undefined) return;
    st.timer = setTimeout(
      () => {
        st.timer = undefined;
        const burst = st.pending;
        st.pending = [];
        st.last = Date.now();
        const latest = burst[burst.length - 1];
        setMessage(burst.length === 1 ? say(latest) : `${burst.length} new events. Latest: ${say(latest)}`);
      },
      Math.max(0, st.last + ANNOUNCE_EVERY_MS - Date.now()),
    );
  }, [items]);
  useEffect(() => () => clearTimeout(state.current.timer), []);
  return (
    <p data-testid="live-announcer" aria-live="polite" className="sr-only">
      {message}
    </p>
  );
}

/** One event: what happened (with how many times in a row) and when, then the Ticket's id and title. */
function Row({ item, now, fresh }: { item: ActivityItem; now: number; fresh: boolean }) {
  return (
    <li className={cn("grid min-w-0 grid-cols-[9px_minmax(0,1fr)_auto] items-start gap-x-2.5 rounded-xs px-2 py-1.5", fresh && "motion-wash")}>
      <Dot tone={item.tone} className="mt-[5px]" />
      <div className="grid min-w-0">
        <span className="min-w-0 truncate text-sm font-medium">
          {item.event}
          {item.count > 1 && <span className="font-normal text-muted">{` ×${item.count}`}</span>}
        </span>
        <span className="flex min-w-0 items-baseline gap-1.5 text-xs text-muted">
          <Link
            to={ticketRoute(item.ticketId)}
            className="shrink-0 font-mono text-text underline decoration-line-soft underline-offset-4 hover:decoration-text"
          >
            {item.ticketId}
          </Link>
          <span dir="auto" className="min-w-0 truncate">
            {item.title}
          </span>
        </span>
      </div>
      <span className="pt-px font-mono text-[11px] text-muted tabular-nums">{formatAgo(item.at, now)}</span>
    </li>
  );
}

export function LiveFeed({ limit, className }: { /** Shows this many until "Show all". */ limit?: number; className?: string }) {
  const items = useActivity((s) => s.items);
  const working = useAthena((s) => selectCounts(s).working);
  const needsYou = useAthena((s) => selectCounts(s).needsYou);
  const synced = useAthena(selectSynced);
  const now = useNow();
  const [all, setAll] = useState(false);
  const heading = useId();
  // The items there when the feed first rendered are not new to the owner; any later one washes in.
  const [initial] = useState(() => new Set(items.map(version)));
  const shown = limit && !all ? items.slice(0, limit) : items;

  return (
    <section data-live-feed aria-labelledby={heading} className={cn("grid min-w-0 content-start gap-1", className)}>
      <div className="flex items-center justify-between gap-2">
        <h2 id={heading} className="flex items-center gap-2 font-mono text-xs tracking-wider text-muted uppercase">
          {(working > 0 || needsYou > 0) && (
            <span data-live-pulse aria-hidden className={cn("size-1.5 animate-pulse rounded-full", needsYou > 0 ? "bg-accent" : "bg-success")} />
          )}
          Live
        </h2>
        {synced && (
          // Both counts are the lengths of their lists (selectCounts): a Ticket waiting on the owner is not working.
          <span data-working className="font-mono text-[11px] whitespace-nowrap text-muted tabular-nums">
            {needsYou > 0 ? `${working} working · ${needsYou} need you` : `${working} working`}
          </span>
        )}
      </div>
      <Announcer items={items} />
      {items.length === 0 ? (
        <p className="py-1 text-sm text-muted">Quiet for now. New events will show here.</p>
      ) : (
        <ul aria-label="Live activity" className="-mx-2 grid min-w-0">
          {shown.map((i) => (
            <Row key={version(i)} item={i} now={now} fresh={!initial.has(version(i))} />
          ))}
        </ul>
      )}
      {limit !== undefined && !all && items.length > limit && (
        <button
          type="button"
          onClick={() => setAll(true)}
          className="inline-flex min-h-11 items-center justify-self-start text-sm font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text"
        >
          {`Show all (${items.length})`}
        </button>
      )}
    </section>
  );
}
