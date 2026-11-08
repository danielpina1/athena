// The Triggers tab: every incoming message, newest first, with what became of it. Evaluate again sends a Trigger back
// to Eval when nothing judged it (see canReevaluate).
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router";
import type { TriggerView } from "@/gen/athena/v1/state_pb";
import { TrustBadge } from "@/features/control/trust";
import { threadMatcher, threadRoute, ticketRoute } from "@/app/search";
import { reevaluate } from "@/lib/commands/actions";
import { toast } from "@/ui/primitives/toast";
import type { AthenaData } from "@/lib/store/state";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { Select } from "@/ui/primitives/select";
import { filteredText, type OutcomePart, parseOutcome } from "./outcome";
import { RoutingSearch, useShown } from "./shared";
import type { FilterParams } from "../tickets/Filters";
import type { RoutingKey } from "./Routing";
import { useCommand } from "./useCommand";

export const TRIGGER_OUTCOMES = [
  { value: "filtered", label: "Filtered" },
  { value: "reached", label: "Reached a Ticket" },
  { value: "attached", label: "Attached to a Ticket" },
  { value: "requeued", label: "Requeued" },
  { value: "none", label: "No outcome yet" },
] as const;

export type TriggerFilter = { outcome?: string; q?: string };

/** The Triggers the filter keeps, newest first. */
export function selectTriggers(triggers: AthenaData["triggers"], f: TriggerFilter): TriggerView[] {
  const q = f.q?.trim().toLowerCase();
  const onThread = threadMatcher(f.q ?? "");
  const ms = (t: TriggerView) => (t.at ? timestampMs(t.at) : 0);
  return [...triggers.values()]
    .filter((t) => {
      if (f.outcome) {
        const kinds = new Set(parseOutcome(t.outcome).map((p) => p.kind));
        if (f.outcome === "none" ? kinds.size > 0 : !kinds.has(f.outcome as OutcomePart["kind"])) return false;
      }
      if (!q) return true;
      // The Thread is matched as search() matches it: a GitHub URL or #12 finds that reference.
      return [t.text, t.id, t.source, t.actor].some((v) => v.toLowerCase().includes(q)) || onThread(t.thread);
    })
    .sort((a, b) => ms(b) - ms(a));
}

function TicketLinks({ ids }: { ids: string[] }) {
  return ids.map((id, i) => (
    <Fragment key={id}>
      {i > 0 && ", "}
      <Link to={ticketRoute(id)} className="font-mono text-xs font-semibold underline underline-offset-2">
        {id}
      </Link>
    </Fragment>
  ));
}

function Outcome({ outcome, owner }: { outcome: string; owner: boolean }) {
  const all = parseOutcome(outcome);
  // A Ticket a Trigger was attached to also reached it (Engine.Attach sets both): say "Attached" only.
  const attached = new Set(all.flatMap((p) => (p.kind === "attached" ? p.tickets : [])));
  const parts = all.flatMap((p): OutcomePart[] => {
    if (p.kind !== "reached") return [p];
    const tickets = p.tickets.filter((id) => !attached.has(id));
    return tickets.length > 0 ? [{ kind: "reached", tickets }] : [];
  });
  // The owner's own message is never routed: before the server recorded where it went, it has no outcome at all.
  if (parts.length === 0) return <span className="text-muted">{owner ? "Sent by you" : "No outcome yet"}</span>;
  return (
    <>
      {parts.map((p, i) => (
        <span key={i} className="min-w-0 break-words">
          {p.kind === "filtered" && filteredText(p.code)}
          {p.kind === "reached" && (
            <>
              {owner ? "Sent to" : "Reached"} <TicketLinks ids={p.tickets} />
            </>
          )}
          {p.kind === "attached" && (
            <>
              Attached to <TicketLinks ids={p.tickets} />
            </>
          )}
          {p.kind === "handedBack" && (
            <>
              Handed back by <TicketLinks ids={p.tickets} />
            </>
          )}
          {p.kind === "requeued" && "Requeued"}
        </span>
      ))}
    </>
  );
}

function Row({ trigger, selected, again }: { trigger: TriggerView; selected: boolean; again: boolean }) {
  const { busy, run } = useCommand("Could not evaluate again");
  const [sent, setSent] = useState(false);
  const el = useRef<HTMLLIElement>(null);
  useEffect(() => {
    if (selected) el.current?.scrollIntoView?.({ block: "nearest" });
  }, [selected]);
  return (
    <li
      ref={el}
      data-trigger-row={trigger.id}
      aria-current={selected ? "true" : undefined}
      className={cn(
        "grid min-w-0 gap-1 border-b border-l-[3px] border-b-line-soft border-l-transparent px-4 py-3",
        selected && "border-l-text bg-surface-2",
      )}
    >
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5">
        <Mono>
          <RelativeTime at={trigger.at ? timestampMs(trigger.at) : Number.NaN} />
        </Mono>
        <Mono tone="default">{trigger.source}</Mono>
        <Mono className="min-w-0 break-all">{trigger.actor}</Mono>
        <Mono>{trigger.kind}</Mono>
        <TrustBadge trust={trigger.trust} />
        {trigger.addressed && (
          <span className="border border-line px-1 font-mono text-xs" title={`Addressed to the agent (${trigger.addressed})`}>
            to agent
          </span>
        )}
      </div>
      {trigger.thread && (
        <Link to={threadRoute(trigger.thread)} className="min-w-0 font-mono text-xs break-all underline underline-offset-2">
          {trigger.thread}
        </Link>
      )}
      <p dir="auto" title={trigger.text} className="min-w-0 break-words">
        {trigger.text}
      </p>
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-sm">
        <Outcome outcome={trigger.outcome} owner={trigger.source === "owner"} />
        {again && !sent && (
          <Button
            size="sm"
            disabled={busy}
            onClick={() =>
              void run(() => reevaluate(trigger.id)).then((ok) => {
                if (!ok) return;
                setSent(true);
                toast({ title: "Queued for a fresh Eval" });
              })
            }
          >
            Evaluate again
          </Button>
        )}
        {again && sent && <span className="text-muted">Queued for a fresh Eval</span>}
      </div>
    </li>
  );
}

export function Triggers({
  params,
  rows,
  total,
  selected,
  again,
}: {
  params: FilterParams<RoutingKey>;
  rows: TriggerView[];
  total: number;
  selected?: string;
  /** Whether Evaluate again is offered for a Trigger. */
  again: (t: TriggerView) => boolean;
}) {
  const { shown, more } = useShown([params.sp.toString()]);
  const filtered = params.get("outcome") !== "" || params.get("tq") !== "";
  const list = useMemo(() => rows.slice(0, shown), [rows, shown]);
  return (
    <div>
      <p className="max-w-prose px-4 pt-4 text-muted">
        Every message that reached Athena, from you or from GitHub, and what became of it: a new Ticket, added to one, or
        set aside and why.
      </p>
      <div className="grid gap-2 p-4 sm:grid-cols-2">
        <Select aria-label="Outcome" value={params.get("outcome")} onChange={(e) => params.set("outcome", e.target.value)}>
          <option value="">All outcomes</option>
          {TRIGGER_OUTCOMES.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </Select>
        <RoutingSearch params={params} param="tq" label="Search Triggers" placeholder="Search by text, source, actor or Thread" />
      </div>
      {total === 0 ? (
        <div className="px-4">
          <EmptyState title="No Triggers yet." description="Nothing has arrived yet. Messages you send and GitHub mentions of the App show up here." />
        </div>
      ) : rows.length === 0 ? (
        <div className="px-4">
          <EmptyState
            title="No Triggers match these filters."
            action={filtered ? <Button onClick={() => params.clear(["outcome", "tq"])}>Clear filters</Button> : undefined}
          />
        </div>
      ) : (
        <>
          <ul>
            {list.map((t) => (
              <Row key={t.id} trigger={t} selected={t.id === selected} again={again(t)} />
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
