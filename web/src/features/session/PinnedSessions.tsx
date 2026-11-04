// Pinned sessions: on a desktop 1440 px and wider, up to 3 live sessions side by side under the Ticket panes, pinned
// from a Ticket's header and kept in the URL (?pin=T-1,T-2) so the layout survives a reload and can be shared. Each
// is a compact, read-only SessionView (reply from the Ticket itself) and holds its Ticket's watch (useWatch).
// Narrower, the pin control is hidden and ?pin is ignored, but left in the URL.
import { PinIcon, XIcon } from "lucide-react";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router";
import { useMediaQuery } from "@/app/Panes";
import { filterSearch, PIN_PARAM, searchOf } from "@/features/tickets/Filters";
import { useWatch } from "@/lib/commands/useWatch";
import { stripAnsi } from "@/lib/stream/ansi";
import { useAthena } from "@/lib/store/state";
import { Mono } from "@/ui/athena/Mono";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { SessionView } from "./SessionView";

export const WIDE_QUERY = "(min-width: 1440px)";
export const MAX_PINS = 3;

/** The strip's columns, one per pin (classes, not a style: the CSP allows no inline styles). */
const COLUMNS = ["", "grid-cols-1", "grid-cols-2", "grid-cols-3"] as const;

/** The pinned Tickets the parameters name: each once, at most MAX_PINS, in order. */
export function readPins(sp: URLSearchParams): string[] {
  const ids = (sp.get(PIN_PARAM) ?? "").split(",").map((s) => s.trim()).filter(Boolean);
  return [...new Set(ids)].slice(0, MAX_PINS);
}

/** Whether pins show here (1440 px and wider), the pinned ids, and a toggle that keeps every other parameter. */
export function usePins() {
  const wide = useMediaQuery(WIDE_QUERY);
  const [params] = useSearchParams();
  const { pathname, search } = useLocation();
  const navigate = useNavigate();
  const pins = readPins(params);
  const toggle = (id: string) => {
    // From the location at the time of the click, not a render's copy of it.
    const next = new URLSearchParams(search);
    const now = readPins(next);
    const ids = now.includes(id) ? now.filter((p) => p !== id) : [...now, id].slice(0, MAX_PINS);
    if (ids.length > 0) next.set(PIN_PARAM, ids.join(","));
    else next.delete(PIN_PARAM);
    navigate({ pathname, search: searchOf(next) }, { replace: true });
  };
  return { wide, pins, toggle };
}

/** The detail header's pin control; nothing under 1440 px. */
export function PinToggle({ ticketId }: { ticketId: string }) {
  const { wide, pins, toggle } = usePins();
  if (!wide) return null;
  const pinned = pins.includes(ticketId);
  const full = !pinned && pins.length >= MAX_PINS;
  return (
    <Button
      aria-label="Pin session"
      aria-pressed={pinned}
      disabled={full}
      title={full ? `Up to ${MAX_PINS} sessions can be pinned` : undefined}
      onClick={() => toggle(ticketId)}
      className={cn(pinned && "border-text bg-stage")}
    >
      <PinIcon aria-hidden />
      Pin
    </Button>
  );
}

function PinnedSession({ id, onUnpin }: { id: string; onUnpin(): void }) {
  useWatch(id);
  const [params] = useSearchParams();
  const title = useAthena((s) => (s.tickets.get(id) ?? s.details.get(id)?.summary)?.title);
  const loaded = useAthena((s) => s.epoch !== "");
  const heading = (
    <div className="grid min-w-0">
      <Mono>{id}</Mono>
      {title !== undefined ? (
        <Link
          to={`/tickets/${encodeURIComponent(id)}${filterSearch(params)}`}
          dir="auto"
          className="truncate font-sans text-sm font-semibold underline decoration-line-soft underline-offset-4 hover:decoration-text"
        >
          {stripAnsi(title) || "Untitled"}
        </Link>
      ) : (
        <span className="font-sans text-sm text-muted">{loaded ? "Ticket not found" : "Loading…"}</span>
      )}
    </div>
  );
  return (
    <article aria-label={`Pinned session ${id}`} className="flex min-h-0 min-w-0 flex-col border-r border-line-soft p-3 last:border-r-0">
      <SessionView
        ticketId={id}
        compact
        title={heading}
        actions={
          <Button variant="ghost" size="icon" aria-label={`Unpin ${id}`} onClick={onUnpin}>
            <XIcon aria-hidden />
          </Button>
        }
      />
    </article>
  );
}

/** The pinned sessions, side by side; nothing under 1440 px or with no pins. */
export function PinnedSessions() {
  const { wide, pins, toggle } = usePins();
  if (!wide || pins.length === 0) return null;
  return (
    <section
      aria-label="Pinned sessions"
      className={cn("grid h-80 shrink-0 border-t border-line", COLUMNS[pins.length])}
    >
      {pins.map((id) => (
        <PinnedSession key={id} id={id} onUnpin={() => toggle(id)} />
      ))}
    </section>
  );
}
