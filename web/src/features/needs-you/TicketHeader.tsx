import { timestampMs, type Timestamp } from "@bufbuild/protobuf/wkt";
import { type RefObject, useRef } from "react";
import { Link } from "react-router";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { stripAnsi } from "@/lib/text/ansi";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { StageBadge } from "@/ui/athena/StageBadge";
import { Truncate } from "@/ui/athena/Truncate";
import { useShortcut } from "@/app/shortcuts";

/** When a Timestamp is, for RelativeTime; NaN (shown as a dash) when the server sent none. */
export const timeOf = (ts: Timestamp | undefined) => (ts ? timestampMs(ts) : Number.NaN);

/**
 * The top of every Needs you card: the title and the stage, then the id, where it works, the model and how long it has
 * waited. `answered` shows an approval's Ticket as working again: the owner has said what it was waiting for.
 */
export function TicketHeader({
  ticket,
  since,
  answered = false,
  stopping = false,
}: {
  ticket: TicketSummary;
  since: number;
  answered?: boolean;
  stopping?: boolean;
}) {
  const where = ticket.repo || ticket.dir;
  return (
    <header className="grid gap-1">
      <div className="flex items-start justify-between gap-3">
        <h2 className="min-w-0 flex-1 text-body font-semibold">
          <Truncate text={stripAnsi(ticket.title)} lines={2} />
        </h2>
        <StageBadge
          stage={answered ? "working" : ticket.stage}
          action={answered ? "" : ticket.action}
          stopping={stopping}
          className="mt-0.5"
        />
      </div>
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0">
        <Link
          to={`/tickets/${ticket.id}`}
          className="inline-flex min-h-11 items-center font-mono text-xs font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0"
        >
          {ticket.id}
        </Link>
        {where && <Mono>{where}</Mono>}
        {ticket.model && <Mono>{ticket.model}</Mono>}
        <Mono>
          <RelativeTime at={since} />
        </Mono>
      </div>
    </header>
  );
}

/**
 * Whether focus is inside the element: the card the owner is on, which the keys a, d and r act on. Read when a key is
 * pressed, so it never needs to render.
 */
export function useFocusWithin<T extends HTMLElement>(): { ref: RefObject<T | null>; focused: () => boolean } {
  const ref = useRef<T>(null);
  return { ref, focused: () => !!ref.current && ref.current.contains(document.activeElement) };
}

/** Runs handler on `key` while focus is inside the card; ignored while typing (useShortcut) and when `when` is false. */
export function useCardShortcut(
  key: string,
  focused: () => boolean,
  handler: () => void,
  when: () => boolean = () => true,
) {
  useShortcut(key, handler, { when: () => focused() && when() });
}
