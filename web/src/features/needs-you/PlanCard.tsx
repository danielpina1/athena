import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { useWatch } from "@/lib/commands/useWatch";
import { useAthena } from "@/lib/store/state";
import { stripAnsi } from "@/lib/text/ansi";
import { Card } from "@/ui/athena/Card";
import { timeOf, TicketHeader } from "./TicketHeader";

/**
 * A plan-first Ticket whose split waits on the owner: a draft, with how many sub-tasks it has, or a failed plan, with
 * why. Both are in the Ticket's detail, which the card watches while it shows.
 */
export function PlanCard({ ticket }: { ticket: TicketSummary }) {
  useWatch(ticket.id);
  const plan = useAthena((s) => s.details.get(ticket.id)?.breakdown);
  const title = stripAnsi(ticket.title);
  const text =
    ticket.planState === "failed"
      ? `Planning ${title} failed${plan?.error ? `: ${plan.error}` : ""}`
      : `A split of ${title} waits for you${plan ? ` (${plan.tasks.length} sub-tasks)` : ""}`;
  return (
    <Card tone="attention" role="group" aria-label={ticket.title.slice(0, 200)} tabIndex={-1} data-needs-you-card={ticket.id} className="grid gap-3">
      <TicketHeader ticket={ticket} since={timeOf(ticket.updatedAt)} />
      <p dir="auto" className="wrap-anywhere">
        {stripAnsi(text)}
      </p>
    </Card>
  );
}
