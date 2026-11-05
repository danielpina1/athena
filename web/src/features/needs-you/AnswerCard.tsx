import { type KeyboardEvent, useRef, useState } from "react";
import { Card } from "@/ui/athena/Card";
import { Truncate } from "@/ui/athena/Truncate";
import { Button } from "@/ui/primitives/button";
import { Textarea } from "@/ui/primitives/textarea";
import { toast } from "@/ui/primitives/toast";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { errorMessage, reply } from "@/lib/commands/actions";
import { timeOf, TicketHeader, useCardShortcut, useFocusWithin } from "./TicketHeader";

/** A question the session is waiting on, with the composer that answers it: Enter sends, Shift+Enter adds a line. */
export function AnswerCard({
  ticket,
  header = true,
}: {
  ticket: TicketSummary;
  /** False where the Ticket's own header is already above the card (its detail). */
  header?: boolean;
}) {
  const [text, setText] = useState("");
  const busy = useRef(false);
  const [sending, setSending] = useState(false);
  const [sent, setSent] = useState(false);
  const box = useRef<HTMLTextAreaElement>(null);
  const { ref, focused } = useFocusWithin<HTMLDivElement>();

  useCardShortcut("r", focused, () => box.current?.focus(), () => !sent);

  const send = async () => {
    const answer = text.trim();
    // Two Enters before a re-render must not send twice.
    if (!answer || busy.current) return;
    busy.current = true;
    setSending(true);
    const r = await reply(ticket.id, answer).finally(() => {
      busy.current = false;
    });
    setSending(false);
    if (r.ok) {
      setText("");
      setSent(true);
    } else {
      toast({ title: "Answer not sent", description: errorMessage(r.code), tone: "danger" });
    }
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key !== "Enter" || e.shiftKey || e.nativeEvent.isComposing) return;
    e.preventDefault();
    if (e.repeat) return;
    void send();
  };

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
      {header && <TicketHeader ticket={ticket} since={timeOf(ticket.updatedAt)} />}
      <Truncate text={ticket.attentionQuestion || "The session is waiting for your answer."} lines={6} />
      {sent ? (
        <p role="status" className="border-t border-line pt-3 font-semibold">
          Answer sent
        </p>
      ) : (
        <div className="grid gap-2">
          <Textarea
            ref={box}
            aria-label="Your answer"
            rows={2}
            value={text}
            disabled={sending}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder="Enter sends, Shift+Enter adds a line"
          />
          <Button variant="accent" onClick={() => void send()} disabled={sending || text.trim() === ""} aria-keyshortcuts="r">
            Send answer
          </Button>
        </div>
      )}
    </Card>
  );
}
