// The Tickets page's list: every Ticket, most recently changed first, narrowed by the filters in the URL. Up to 200
// rows, then "Show more". On the desktop j and k move between rows, ⏎ opens the one in focus and / searches.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useMemo, useRef, useState } from "react";
import { Link } from "react-router";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { useShortcut } from "@/app/shortcuts";
import { selectTickets } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { StageBadge } from "@/ui/athena/StageBadge";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { Filters, filterSearch, hasFilters, readFilters, useFilterParams } from "./Filters";

/** Lists render at most this many rows, then "Show more". */
const PAGE = 200;

function Row({ ticket, selected, needsYou, search }: { ticket: TicketSummary; selected: boolean; needsYou: boolean; search: string }) {
  const where = ticket.repo || ticket.dir;
  return (
    <li className="min-w-0">
      <Link
        to={`/tickets/${encodeURIComponent(ticket.id)}${search}`}
        data-ticket-row={ticket.id}
        aria-current={selected ? "page" : undefined}
        className={cn(
          "grid min-h-11 min-w-0 gap-0.5 border-b border-l-[3px] border-b-line-soft border-l-transparent px-4 py-2 text-text hover:bg-surface-2",
          selected && "border-l-text bg-surface-2",
        )}
      >
        <span className="flex min-w-0 items-center gap-2">
          {needsYou && <span data-testid="needs-you-dot" aria-hidden className="size-2 shrink-0 rounded-full bg-accent" />}
          <span dir="auto" className="min-w-0 flex-1 truncate font-semibold">
            {ticket.title}
          </span>
          <StageBadge stage={ticket.stage} action={ticket.action} outcome={ticket.outcome} />
        </span>
        <span className="flex min-w-0 items-baseline gap-x-2">
          {where && <Mono className="min-w-0 truncate">{where}</Mono>}
          {ticket.model && <Mono className="shrink-0">{ticket.model}</Mono>}
          <Mono className="ml-auto shrink-0">
            <RelativeTime at={ticket.updatedAt ? timestampMs(ticket.updatedAt) : Number.NaN} />
          </Mono>
        </span>
      </Link>
    </li>
  );
}

export function TicketList({ selectedId }: { selectedId?: string }) {
  const { sp, clear } = useFilterParams();
  const filter = readFilters(sp);
  const tickets = useAthena((s) => s.tickets);
  const repos = useAthena((s) => s.repos);
  const needsYouIds = useAthena((s) => selectTickets(s, { stage: "needsYou" }));
  const rows = useAthena((s) => selectTickets(s, filter));
  const [shown, setShown] = useState(PAGE);
  const list = useRef<HTMLUListElement>(null);
  const search = useRef<HTMLInputElement>(null);

  const models = useMemo(() => [...new Set([...tickets.values()].map((t) => t.model).filter(Boolean))].sort(), [tickets]);
  const needsYou = useMemo(() => new Set(needsYouIds.map((t) => t.id)), [needsYouIds]);
  const linkSearch = filterSearch(sp);

  const links = () => [...(list.current?.querySelectorAll<HTMLElement>("[data-ticket-row]") ?? [])];
  const move = (by: 1 | -1) => {
    const all = links();
    if (all.length === 0) return;
    let at = all.findIndex((a) => a === document.activeElement);
    if (at < 0) at = all.findIndex((a) => a.getAttribute("aria-current") === "page");
    const next = at < 0 ? (by === 1 ? 0 : all.length - 1) : Math.min(all.length - 1, Math.max(0, at + by));
    all[next].focus();
  };
  useShortcut("j", () => move(1));
  useShortcut("k", () => move(-1));
  useShortcut("/", () => search.current?.focus());
  // ⏎ opens the row in focus: a focused link does that itself.

  const total = tickets.size;
  return (
    <section aria-label="Tickets" className="min-w-0">
      <div className="sticky top-0 z-10 grid gap-3 border-b border-line bg-bg p-4">
        <div className="flex items-baseline justify-between gap-2">
          <h1 className="text-xl font-semibold">Tickets</h1>
          <Mono>{`${rows.length} ${rows.length === 1 ? "Ticket" : "Tickets"}`}</Mono>
        </div>
        <Filters repos={repos} models={models} searchRef={search} />
      </div>
      {total === 0 ? (
        <div className="px-4">
          <EmptyState
            title="No Tickets yet."
            description="Describe the work and Athena opens a Ticket for it."
            action={
              <Button variant="primary" asChild>
                <Link to="/new">New task</Link>
              </Button>
            }
          />
        </div>
      ) : rows.length === 0 ? (
        <div className="px-4">
          <EmptyState
            title="No Tickets match these filters."
            action={hasFilters(sp) ? <Button onClick={clear}>Clear filters</Button> : undefined}
          />
        </div>
      ) : (
        <>
          <ul ref={list}>
            {rows.slice(0, shown).map((t) => (
              <Row key={t.id} ticket={t} selected={t.id === selectedId} needsYou={needsYou.has(t.id)} search={linkSearch} />
            ))}
          </ul>
          {rows.length > shown && (
            <div className="p-4">
              <Button onClick={() => setShown((n) => n + PAGE)}>{`Show more (${rows.length - shown})`}</Button>
            </div>
          )}
        </>
      )}
    </section>
  );
}
