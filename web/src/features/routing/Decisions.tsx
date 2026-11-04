// The Decisions tab: what Eval decided about each batch of Triggers, with its reasons. An unavailable Decision (Eval
// could not decide) can be dismissed once the owner has seen it.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { Fragment, useMemo } from "react";
import { Link } from "react-router";
import type { DecisionView } from "@/gen/athena/v1/state_pb";
import { threadRoute, ticketRoute, triggerRoute } from "@/app/search";
import { dismiss } from "@/lib/commands/actions";
import type { AthenaData } from "@/lib/store/state";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { Button } from "@/ui/primitives/button";
import { Select } from "@/ui/primitives/select";
import type { FilterParams } from "../tickets/Filters";
import type { RoutingKey } from "./Routing";
import { canDismiss } from "./outcome";
import { useShown } from "./shared";
import { useCommand } from "./useCommand";

export const DECISION_FILTERS = [
  { value: "would-accept", label: "Would accept" },
  { value: "accept", label: "Accepted" },
  { value: "ignore", label: "Ignored" },
  { value: "stop", label: "Stops" },
  { value: "unavailable", label: "Unavailable" },
] as const;

export type DecisionFilter = { decision?: string };

/** The Decisions the filter keeps, newest first. */
export function selectDecisions(decisions: AthenaData["decisions"], f: DecisionFilter): DecisionView[] {
  const ms = (d: DecisionView) => (d.at ? timestampMs(d.at) : 0);
  return [...decisions.values()]
    .filter((d) => !f.decision || (f.decision === "would-accept" ? d.wouldAccept : d.kind === f.decision))
    .sort((a, b) => ms(b) - ms(a));
}

function Row({ decision: d }: { decision: DecisionView }) {
  const { busy, run } = useCommand("Could not dismiss");
  return (
    <li data-decision-row={d.id} className="grid min-w-0 gap-1 border-b border-b-line-soft px-4 py-3">
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5">
        <Mono>
          <RelativeTime at={d.at ? timestampMs(d.at) : Number.NaN} />
        </Mono>
        <Mono tone="default" className="font-semibold">
          {d.kind}
        </Mono>
        {d.wouldAccept && <span className="border border-line px-1 font-mono text-xs">would accept</span>}
        {d.dismissedAt && <span className="border border-line-soft px-1 font-mono text-xs text-muted">dismissed</span>}
      </div>
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-0.5 text-sm">
        {d.thread && (
          <Link to={threadRoute(d.thread)} className="font-mono text-xs break-all underline underline-offset-2">
            {d.thread}
          </Link>
        )}
        {d.ticketId && (
          <Link to={ticketRoute(d.ticketId)} className="font-mono text-xs font-semibold underline underline-offset-2">
            {d.ticketId}
          </Link>
        )}
        {d.triggers.length > 0 && (
          <span className="min-w-0 break-words">
            {d.triggers.length === 1 ? "Trigger " : "Triggers "}
            {d.triggers.map((id, i) => (
              <Fragment key={id}>
                {i > 0 && ", "}
                <Link to={triggerRoute(id)} className="font-mono text-xs underline underline-offset-2">
                  {id}
                </Link>
              </Fragment>
            ))}
          </span>
        )}
      </div>
      {d.reasons.length > 0 && (
        <ul className="grid min-w-0 gap-0.5">
          {d.reasons.map((r, i) => (
            <li key={i} className="min-w-0 break-words">
              <Mono tone="default" className="font-semibold">
                {r.code}
              </Mono>
              {Object.entries(r.values).map(([k, v]) => (
                <Mono key={k} className="ml-2">{`${k}=${v}`}</Mono>
              ))}
            </li>
          ))}
        </ul>
      )}
      {canDismiss(d) && (
        <div>
          <Button size="sm" disabled={busy} onClick={() => void run(() => dismiss(d.id))}>
            Dismiss
          </Button>
        </div>
      )}
    </li>
  );
}

export function Decisions({ params, rows, total }: { params: FilterParams<RoutingKey>; rows: DecisionView[]; total: number }) {
  const { shown, more } = useShown([params.sp.toString()]);
  const list = useMemo(() => rows.slice(0, shown), [rows, shown]);
  return (
    <div>
      <p className="max-w-prose px-4 pt-4 text-muted">
        When a GitHub message wasn&apos;t clearly addressed to Athena, what it decided to do and why.
      </p>
      <div className="p-4">
        <Select aria-label="Decision" value={params.get("decision")} onChange={(e) => params.set("decision", e.target.value)}>
          <option value="">All Decisions</option>
          {DECISION_FILTERS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </Select>
      </div>
      {total === 0 ? (
        <div className="px-4">
          <EmptyState title="No Decisions yet." description="Nothing has been decided yet. Each time Athena weighs a GitHub message, its decision and reasons show up here." />
        </div>
      ) : rows.length === 0 ? (
        <div className="px-4">
          <EmptyState
            title="No Decisions match this filter."
            action={<Button onClick={() => params.clear(["decision"])}>Clear filter</Button>}
          />
        </div>
      ) : (
        <>
          <ul>
            {list.map((d) => (
              <Row key={d.id} decision={d} />
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
