// A Ticket's relatives in a split: its lead, and its child tickets, each with its stage and the sub-tasks it still
// waits for ("after a, b", by their keys). Nothing for a Ticket that is neither a lead nor a child.
import { useId } from "react";
import { Link } from "react-router";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";
import { stripAnsi } from "@/lib/text/ansi";
import { StageBadge } from "@/ui/athena/StageBadge";

const link =
  "inline-flex min-h-11 items-center font-mono text-xs font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0";
const row = "flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-sm";

function Stage({ t }: { t?: TicketSummary }) {
  return t ? <StageBadge stage={t.stage} action={t.action} outcome={t.outcome} node={t.nodeName} /> : null;
}

export function Relatives({ ticket }: { ticket: TicketSummary }) {
  const tickets = useAthena((s) => s.tickets);
  const heading = useId();
  if (!ticket.lead && ticket.children.length === 0) return null;
  const lead = tickets.get(ticket.lead);
  // A dep is named by its sub-task key, from the child summaries; by its id when none is known.
  const key = (id: string) => tickets.get(id)?.splitKey || id;
  return (
    <section aria-label="Relatives" className="grid min-w-0 gap-2">
      {ticket.lead && (
        <div className={row}>
          <span className="font-mono text-xs text-muted">Lead</span>
          <Link to={`/tickets/${encodeURIComponent(ticket.lead)}`} className={link}>
            {ticket.lead}
          </Link>
          {lead && <span dir="auto" className="min-w-0 wrap-anywhere">{stripAnsi(lead.title)}</span>}
          <Stage t={lead} />
        </div>
      )}
      {ticket.children.length > 0 && (
        <div className="grid min-w-0 gap-1">
          <h2 id={heading} className="font-mono text-xs text-muted">
            Child tickets
          </h2>
          <ul aria-labelledby={heading} className="grid min-w-0">
            {ticket.children.map((id) => {
              const c = tickets.get(id);
              return (
                <li key={id} className={`${row} border-t border-line-soft py-1`}>
                  <Link to={`/tickets/${encodeURIComponent(id)}`} className={link}>
                    {id}
                  </Link>
                  <span dir="auto" className="min-w-0 flex-1 wrap-anywhere">
                    {stripAnsi(c?.title ?? "")}
                  </span>
                  <Stage t={c} />
                  {!!c?.waiting.length && <span className="text-muted">{`after ${c.waiting.map(key).join(", ")}`}</span>}
                </li>
              );
            })}
          </ul>
        </div>
      )}
    </section>
  );
}
