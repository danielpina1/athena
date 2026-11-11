import { lazy, Suspense, useState } from "react";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { useWatch } from "@/lib/commands/useWatch";
import { useAthena } from "@/lib/store/state";
import { stripAnsi } from "@/lib/text/ansi";
import { Card } from "@/ui/athena/Card";
import { Button } from "@/ui/primitives/button";
import { timeOf, TicketHeader } from "./TicketHeader";

// The breakdown editor is a lazy chunk: it loads when Review split opens it.
const BreakdownEditor = lazy(() => import("@/features/split/BreakdownEditor"));

/**
 * A plan-first Ticket whose split waits on the owner: a draft, with how many sub-tasks it has, or a failed plan, with
 * why. Both are in the Ticket's detail, which the card watches while it shows. Review split opens the editor.
 */
export function PlanCard({ ticket, header = true }: { ticket: TicketSummary; /** False under the Ticket's own header. */ header?: boolean }) {
  useWatch(ticket.id);
  const [reviewing, setReviewing] = useState(false);
  const plan = useAthena((s) => s.details.get(ticket.id)?.breakdown);
  const title = stripAnsi(ticket.title);
  const text =
    ticket.planState === "failed"
      ? `Planning ${title} failed${plan?.error ? `: ${plan.error}` : ""}`
      : `A split of ${title} waits for you${plan ? ` (${plan.tasks.length} sub-tasks)` : ""}`;
  return (
    <Card tone="attention" role="group" aria-label={ticket.title.slice(0, 200)} tabIndex={-1} data-needs-you-card={ticket.id} className="grid gap-3">
      {header && <TicketHeader ticket={ticket} since={timeOf(ticket.updatedAt)} />}
      <p dir="auto" className="wrap-anywhere">
        {stripAnsi(text)}
      </p>
      <Button variant="accent" className="justify-self-start" onClick={() => setReviewing(true)}>
        Review split
      </Button>
      {reviewing && (
        <Suspense fallback={null}>
          <BreakdownEditor ticketId={ticket.id} onClose={() => setReviewing(false)} />
        </Suspense>
      )}
    </Card>
  );
}
