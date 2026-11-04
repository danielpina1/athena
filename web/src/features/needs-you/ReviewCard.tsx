import { Link } from "react-router";
import { Card } from "@/ui/athena/Card";
import { Confirm } from "@/ui/athena/Confirm";
import { Button } from "@/ui/primitives/button";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";
import { stopTicket } from "./stopTicket";
import { timeOf, TicketHeader } from "./TicketHeader";

/** Finished work waiting for the owner's eyes: the pull request, or the local diff when there is none. */
export function ReviewCard({ ticket }: { ticket: TicketSummary }) {
  const stopping = useAthena((s) => s.stopping.has(ticket.id));
  return (
    <Card
      tone="attention"
      role="group"
      aria-label={ticket.title.slice(0, 200)}
      tabIndex={-1}
      data-needs-you-card={ticket.id}
      className="grid gap-3"
    >
      <TicketHeader ticket={ticket} since={timeOf(ticket.updatedAt)} stopping={stopping} />
      <p>Ready for review</p>
      <div className="grid grid-cols-2 gap-3">
        <Confirm
          label="Stop"
          confirmLabel="Stop this Ticket"
          disabled={stopping}
          onConfirm={() => void stopTicket(ticket.id)}
        />
        <Button variant="accent" asChild>
          {ticket.prUrl ? (
            <a href={ticket.prUrl} target="_blank" rel="noopener">
              Open pull request
            </a>
          ) : (
            <Link to={`/tickets/${ticket.id}#diff`}>Review the changes</Link>
          )}
        </Button>
      </div>
    </Card>
  );
}
