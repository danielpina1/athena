import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { continueWithout, errorMessage, retry } from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { Card } from "@/ui/athena/Card";
import { Confirm } from "@/ui/athena/Confirm";
import { Truncate } from "@/ui/athena/Truncate";
import { Button } from "@/ui/primitives/button";
import { toast } from "@/ui/primitives/toast";
import { stopTicket } from "./stopTicket";
import { timeOf, TicketHeader } from "./TicketHeader";

/**
 * A lead whose child ticket failed or is blocked (R8): the lead's question, then Retry the child, Drop it (with the
 * siblings that wait for it) or Continue without it. The card is the lead's; the child has none of its own.
 */
export function ChildCard({ ticket }: { ticket: TicketSummary }) {
  const child = ticket.attentionChild;
  const stopping = useAthena((s) => s.stopping.has(child));
  const run = async (done: string, send: ReturnType<typeof retry>) => {
    const r = await send;
    if (r.ok) toast({ title: done });
    else toast({ title: "Not done", description: (r.code === "invalid" && r.message) || errorMessage(r.code), tone: "danger" });
  };
  return (
    <Card tone="attention" role="group" aria-label={ticket.title.slice(0, 200)} tabIndex={-1} data-needs-you-card={ticket.id} className="grid gap-3">
      <TicketHeader ticket={ticket} since={timeOf(ticket.updatedAt)} />
      <Truncate text={ticket.attentionQuestion} lines={4} />
      <div className="grid gap-3 sm:grid-cols-3">
        <Button variant="accent" onClick={() => void run("Retrying", retry(child))} disabled={stopping}>
          Retry
        </Button>
        <Confirm
          label="Drop"
          confirmLabel={`Drop ${child}? Child tickets that wait for it are dropped too.`}
          disabled={stopping}
          onConfirm={() => void stopTicket(child, "Dropped from Needs you")}
          className="h-auto min-h-11 py-2 whitespace-normal lg:min-h-9"
        />
        <Button onClick={() => void run("Going on without it", continueWithout(child))} disabled={stopping}>
          Continue without it
        </Button>
      </div>
    </Card>
  );
}
