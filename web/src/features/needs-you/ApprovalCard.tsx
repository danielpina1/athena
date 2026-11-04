import { useRef, useState } from "react";
import { Kbd } from "@/ui/athena/Kbd";
import { Card } from "@/ui/athena/Card";
import { Mono } from "@/ui/athena/Mono";
import { Truncate } from "@/ui/athena/Truncate";
import { Button } from "@/ui/primitives/button";
import { Input } from "@/ui/primitives/input";
import { toast } from "@/ui/primitives/toast";
import type { ApprovalView, TicketSummary } from "@/gen/athena/v1/state_pb";
import { answerApproval, errorMessage } from "@/lib/commands/actions";
import { selectAnswering } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { timeOf, TicketHeader, useCardShortcut, useFocusWithin } from "./TicketHeader";

/**
 * A tool call the rules sent to the owner. The owner's tap shows "Approved" or "Denied" at once (the store's
 * `answering`), the card leaves when the server's Delta lands, and a failed command puts the live card back.
 */
export function ApprovalCard({
  ticket,
  approval,
  header = true,
}: {
  ticket: TicketSummary;
  approval: ApprovalView;
  /** False where the Ticket's own header is already above the card (its detail). */
  header?: boolean;
}) {
  const answer = useAthena((s) => selectAnswering(s, approval.id));
  const [note, setNote] = useState("");
  const busy = useRef(false);
  const { ref, focused } = useFocusWithin<HTMLDivElement>();

  const send = async (approve: boolean) => {
    // A second tap before the first has taken effect must not send a second answer.
    if (busy.current || answer !== undefined) return;
    busy.current = true;
    try {
      const r = await answerApproval(approval.id, approve, note.trim());
      if (!r.ok) {
        toast({ title: approve ? "Could not approve" : "Could not deny", description: errorMessage(r.code), tone: "danger" });
      }
    } finally {
      busy.current = false;
    }
  };

  const open = () => answer === undefined;
  useCardShortcut("a", focused, () => void send(true), open);
  useCardShortcut("d", focused, () => void send(false), open);

  return (
    <Card
      ref={ref}
      tone="attention"
      role="group"
      aria-label={ticket.title.slice(0, 200)}
      tabIndex={-1}
      data-needs-you-card={ticket.id}
      className="grid gap-3"
    >
      {header && <TicketHeader ticket={ticket} since={timeOf(approval.createdAt)} answered={answer !== undefined} />}
      <div className="grid gap-1">
        <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-body">
          <Mono tone="default" className="font-semibold">
            {approval.tool}
          </Mono>
          <span className="min-w-0 break-words">{approval.summary}</span>
        </p>
        {approval.rule && <Mono>Rule: {approval.rule}</Mono>}
      </div>
      {approval.inputRedacted && (
        <Truncate
          text={approval.inputRedacted}
          lines={6}
          className="rounded-xs bg-surface-2 p-2 font-mono text-xs whitespace-pre-wrap"
        />
      )}
      {answer === undefined ? (
        <>
          <label className="grid gap-1 text-sm text-muted">
            Note (optional)
            <Input value={note} onChange={(e) => setNote(e.target.value)} maxLength={500} autoComplete="off" />
          </label>
          <div className="grid grid-cols-2 gap-3">
            <Button onClick={() => void send(false)} aria-keyshortcuts="d">
              Deny
              <Kbd aria-hidden className="hidden lg:inline-flex">
                d
              </Kbd>
            </Button>
            <Button variant="accent" onClick={() => void send(true)} aria-keyshortcuts="a">
              Approve
              <Kbd aria-hidden className="hidden border-accent-fg text-accent-fg lg:inline-flex">
                a
              </Kbd>
            </Button>
          </div>
        </>
      ) : (
        <p role="status" className="border-t border-line pt-3 font-semibold">
          {answer ? "Approved" : "Denied"}
        </p>
      )}
    </Card>
  );
}
