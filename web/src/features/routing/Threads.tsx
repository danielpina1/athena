// The Threads tab: each conversation Athena follows, the Tickets subscribed to it and whether an Eval is deciding it.
import { useEffect, useMemo, useRef } from "react";
import { Link } from "react-router";
import type { ThreadView } from "@/gen/athena/v1/state_pb";
import { threadMatcher, ticketRoute } from "@/app/search";
import { unsubscribe } from "@/lib/commands/actions";
import type { AthenaData } from "@/lib/store/state";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Mono } from "@/ui/athena/Mono";
import { cn } from "@/ui/cn";
import { Confirm } from "@/ui/athena/Confirm";
import { Button } from "@/ui/primitives/button";
import { Select } from "@/ui/primitives/select";
import type { FilterParams } from "../tickets/Filters";
import type { RoutingKey } from "./Routing";
import { parseTicketChip } from "./outcome";
import { RoutingSearch, useShown } from "./shared";
import { useCommand } from "./useCommand";

export const THREAD_STATES = [
  { value: "evaluating", label: "Evaluating" },
  { value: "queued", label: "With queued messages" },
  { value: "open", label: "Open" },
  { value: "closed", label: "Closed" },
] as const;

export type ThreadFilter = { state?: string; q?: string };

/** The Threads the filter keeps: an Eval in progress first, then the most queued, then by key. */
export function selectThreads(threads: AthenaData["threads"], f: ThreadFilter): ThreadView[] {
  const q = f.q?.trim().toLowerCase();
  const onKey = threadMatcher(f.q ?? "");
  return [...threads.values()]
    .filter((t) => {
      if (f.state === "evaluating" && !t.eval) return false;
      if (f.state === "queued" && t.queued === 0) return false;
      if (f.state === "open" && t.closed) return false;
      if (f.state === "closed" && !t.closed) return false;
      return !q || onKey(t.key) || t.tickets.some((x) => x.toLowerCase().includes(q));
    })
    .sort((a, b) => Number(!!b.eval) - Number(!!a.eval) || b.queued - a.queued || a.key.localeCompare(b.key));
}

const FLAG_WORDS: Record<string, string> = {
  own: "own: this Ticket opened the Thread",
  bound: "bound: a Source bound it to the Thread",
  context: "context: follows the Thread for context only",
};

function Chip({ spec }: { spec: string }) {
  const { id, flags } = parseTicketChip(spec);
  return (
    <Link
      to={ticketRoute(id)}
      className="inline-flex min-h-11 items-center gap-1 border border-line-soft px-2 hover:border-line lg:min-h-7"
    >
      <Mono tone="default">{id}</Mono>
      {flags.map((f) => (
        <Mono key={f} title={FLAG_WORDS[f]} className="border-l border-line-soft pl-1">
          {f}
        </Mono>
      ))}
    </Link>
  );
}

function Row({ thread, selected }: { thread: ThreadView; selected: boolean }) {
  const { busy, run } = useCommand("Could not unsubscribe");
  const el = useRef<HTMLLIElement>(null);
  useEffect(() => {
    if (selected) el.current?.scrollIntoView?.({ block: "nearest" });
  }, [selected]);
  const key = useRef<HTMLSpanElement>(null);
  const n = thread.tickets.length;
  return (
    <li
      ref={el}
      data-thread-row={thread.key}
      aria-current={selected ? "true" : undefined}
      className={cn(
        "grid min-w-0 gap-2 border-b border-l-[3px] border-b-line-soft border-l-transparent px-4 py-3",
        selected && "border-l-text bg-surface-2",
      )}
    >
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
        <Mono ref={key} tabIndex={-1} data-thread-key tone="default" className="min-w-0 font-semibold break-all outline-offset-2">
          {thread.key}
        </Mono>
        {thread.eval && (
          <span className="border border-line bg-surface-2 px-1 font-mono text-xs">
            {`Evaluating${thread.eval.strength ? ` · ${thread.eval.strength}` : ""}`}
          </span>
        )}
        {thread.closed && <span className="border border-line px-1 font-mono text-xs">{`Closed: ${thread.closed}`}</span>}
      </div>
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
        <Mono>{`${thread.queued} queued`}</Mono>
        <Mono>{`${thread.decisions} ${thread.decisions === 1 ? "Decision" : "Decisions"}`}</Mono>
      </div>
      {n > 0 && (
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          {thread.tickets.map((t) => (
            <Chip key={t} spec={t} />
          ))}
          <Confirm
            size="sm"
            disabled={busy}
            label="Unsubscribe"
            confirmLabel={`Unsubscribe ${n === 1 ? "this Ticket" : `these ${n} Tickets`}`}
            onConfirm={() =>
              // The chip row goes away with the Tickets, and the focused button with it: focus the key instead.
              void run(() => unsubscribe(thread.key)).then((ok) => {
                if (ok) key.current?.focus();
              })
            }
          />
        </div>
      )}
    </li>
  );
}

export function Threads({ params, rows, total, selected }: { params: FilterParams<RoutingKey>; rows: ThreadView[]; total: number; selected?: string }) {
  const { shown, more } = useShown([params.sp.toString()]);
  const filtered = params.get("state") !== "" || params.get("hq") !== "";
  const list = useMemo(() => rows.slice(0, shown), [rows, shown]);
  return (
    <div>
      <p className="max-w-prose px-4 pt-4 text-muted">
        The GitHub issues and pull requests Athena follows, and which Tickets they wake when someone writes there.
      </p>
      <div className="grid gap-2 p-4 sm:grid-cols-2">
        <Select aria-label="Thread state" value={params.get("state")} onChange={(e) => params.set("state", e.target.value)}>
          <option value="">All Threads</option>
          {THREAD_STATES.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </Select>
        <RoutingSearch params={params} param="hq" label="Search Threads" placeholder="Search by key or Ticket" />
      </div>
      {total === 0 ? (
        <div className="px-4">
          <EmptyState title="No Threads yet." description="No GitHub issue or pull request is followed yet. Each one Athena starts following shows up here." />
        </div>
      ) : rows.length === 0 ? (
        <div className="px-4">
          <EmptyState
            title="No Threads match these filters."
            action={filtered ? <Button onClick={() => params.clear(["state", "hq"])}>Clear filters</Button> : undefined}
          />
        </div>
      ) : (
        <>
          <ul>
            {list.map((t) => (
              <Row key={t.key} thread={t} selected={t.key === selected} />
            ))}
          </ul>
          {rows.length > shown && (
            <div className="p-4">
              <Button onClick={more}>{`Show more (${rows.length - shown})`}</Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
