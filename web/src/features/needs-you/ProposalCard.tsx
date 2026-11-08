import { useRef } from "react";
import { Card } from "@/ui/athena/Card";
import { Truncate } from "@/ui/athena/Truncate";
import { Button } from "@/ui/primitives/button";
import { toast } from "@/ui/primitives/toast";
import type { ProposalView, TicketSummary } from "@/gen/athena/v1/state_pb";
import { applyProposal, discardProposal, errorMessage } from "@/lib/commands/actions";
import { timeOf, TicketHeader } from "./TicketHeader";

const TITLE: Record<string, string> = {
  reply: "Reply held for you",
  push: "Push held for you",
  pull_request: "Pull request held for you",
};

/** A reply, push or pull request Athena holds until the owner applies or discards it. A failed one says why. */
export function ProposalCard({ ticket, proposal }: { ticket: TicketSummary; proposal: ProposalView }) {
  const busy = useRef(false);
  const failed = proposal.state === "failed";

  const send = async (apply: boolean) => {
    if (busy.current) return;
    busy.current = true;
    try {
      const r = await (apply ? applyProposal : discardProposal)(proposal.id);
      if (!r.ok) {
        toast({ title: apply ? "Could not apply" : "Could not discard", description: errorMessage(r.code), tone: "danger" });
      }
    } finally {
      busy.current = false;
    }
  };

  const title = TITLE[proposal.kind] ?? "Held for you";
  return (
    <Card
      tone="attention"
      role="group"
      aria-label={title}
      tabIndex={-1}
      data-needs-you-card={ticket.id}
      className="grid gap-3"
    >
      <TicketHeader ticket={ticket} since={timeOf(proposal.createdAt)} />
      <div className="grid gap-1">
        <p className="font-semibold">{title}</p>
        <p className="font-mono text-xs break-words text-muted">{proposal.repo}</p>
        {proposal.summary && proposal.summary !== proposal.detail && <p className="break-words">{proposal.summary}</p>}
      </div>
      {proposal.detail && (
        <Truncate text={proposal.detail} lines={6} className="rounded-xs bg-surface-2 p-2 font-mono text-xs whitespace-pre-wrap" />
      )}
      {failed && proposal.error && (
        <p role="alert" className="break-words text-danger">
          {proposal.error}
        </p>
      )}
      <div className="grid grid-cols-2 gap-3">
        <Button onClick={() => void send(false)}>Discard</Button>
        <Button variant="accent" onClick={() => void send(true)}>
          {failed ? "Try again" : "Apply"}
        </Button>
      </div>
    </Card>
  );
}
