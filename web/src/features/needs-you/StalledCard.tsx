import { Card } from "@/ui/athena/Card";
import { Confirm } from "@/ui/athena/Confirm";
import { Truncate } from "@/ui/athena/Truncate";
import { Button } from "@/ui/primitives/button";
import { toast } from "@/ui/primitives/toast";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { errorMessage, retry } from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { stopTicket } from "./stopTicket";
import { timeOf, TicketHeader } from "./TicketHeader";

// No cause is claimed: the summary does not carry one.
const STOCK: Record<string, string> = {
  delivery: "Delivery failed. Open the Ticket to see what happened.",
  blocked: "Blocked. Open the Ticket to see why.",
  failed: "Failed. Open the Ticket to see what happened.",
};

/** A Ticket that is stuck (delivery failed, blocked or failed): the reason, Retry when it can be retried, and Stop. */
export function StalledCard({ ticket }: { ticket: TicketSummary }) {
  const stopping = useAthena((s) => s.stopping.has(ticket.id));

  const doRetry = async () => {
    const r = await retry(ticket.id);
    if (r.ok) toast({ title: "Retrying" });
    else toast({ title: "Could not retry", description: errorMessage(r.code), tone: "danger" });
  };

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
      {ticket.attentionQuestion ? (
        <div className="grid gap-1">
          <p className="font-mono text-xs text-muted">Last message</p>
          <Truncate text={ticket.attentionQuestion} lines={4} />
        </div>
      ) : (
        <p>{STOCK[ticket.action] ?? "Open the Ticket to see what happened."}</p>
      )}
      <div className={ticket.canRetry ? "grid grid-cols-2 gap-3" : "grid gap-3"}>
        <Confirm
          label="Stop"
          confirmLabel="Stop this Ticket"
          disabled={stopping}
          onConfirm={() => void stopTicket(ticket.id)}
        />
        {ticket.canRetry && (
          <Button variant="accent" onClick={() => void doRetry()} disabled={stopping}>
            Retry
          </Button>
        )}
      </div>
    </Card>
  );
}
