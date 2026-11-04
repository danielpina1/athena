// The session's composer: Enter sends a reply, Shift+Enter adds a line, ⌘⏎ (Ctrl+Enter) interrupts the turn. The
// text stays in the field until the reply's result is ok, so a failed send loses nothing. An ended Ticket takes no
// replies: the field is disabled with the reason.
import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { modKeyLabel } from "@/app/shortcuts";
import { errorMessage, interrupt, reply } from "@/lib/commands/actions";
import { Button } from "@/ui/primitives/button";
import { Textarea } from "@/ui/primitives/textarea";
import { toast } from "@/ui/primitives/toast";

export const ENDED_REASON = "This Ticket has ended, so it takes no more replies.";

/** Interrupts the Ticket's running turn and says how it went. */
export async function interruptSession(ticketId: string): Promise<void> {
  const r = await interrupt(ticketId);
  if (r.ok) toast({ title: "Interrupted" });
  else toast({ title: "Not interrupted", description: errorMessage(r.code), tone: "danger" });
}

export function Composer({ ticket, focus = false, onFocused }: { ticket: TicketSummary; focus?: boolean; onFocused?(): void }) {
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const busy = useRef(false);
  const field = useRef<HTMLTextAreaElement>(null);
  const ended = ticket.stage === "ended";
  const working = ticket.stage === "working";

  // The header's Reply (and r) asks for the field.
  useEffect(() => {
    if (!focus) return;
    field.current?.focus();
    onFocused?.();
  }, [focus, onFocused]);

  const send = async () => {
    const t = text.trim();
    if (!t || ended || busy.current) return;
    busy.current = true;
    setSending(true);
    const r = await reply(ticket.id, t).finally(() => {
      busy.current = false;
      setSending(false);
    });
    // Edits made while it was sending stay.
    if (r.ok) setText((cur) => (cur.trim() === t ? "" : cur));
    else toast({ title: "Reply not sent", description: errorMessage(r.code), tone: "danger" });
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key !== "Enter" || e.nativeEvent.isComposing) return;
    if (e.metaKey || e.ctrlKey) {
      // Handled here, so the page's own ⌘⏎ does not interrupt a second time.
      e.preventDefault();
      e.stopPropagation();
      if (!e.repeat && working) void interruptSession(ticket.id);
      return;
    }
    if (e.shiftKey) return;
    e.preventDefault();
    if (!e.repeat) void send();
  };

  return (
    <div className="grid min-w-0 gap-2">
      <Textarea
        ref={field}
        aria-label="Reply to the session"
        aria-describedby={ended ? `composer-ended-${ticket.id}` : undefined}
        rows={2}
        value={text}
        disabled={ended}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKeyDown}
        placeholder={ended ? undefined : `Enter sends, Shift+Enter adds a line, ${modKeyLabel("⏎")} interrupts`}
      />
      {ended && (
        <p id={`composer-ended-${ticket.id}`} className="text-sm text-muted">
          {ENDED_REASON}
        </p>
      )}
      <div className="flex flex-wrap justify-end gap-2">
        {working && (
          <Button onClick={() => void interruptSession(ticket.id)} aria-keyshortcuts="Meta+Enter Control+Enter">
            Interrupt
          </Button>
        )}
        <Button variant="primary" onClick={() => void send()} disabled={ended || sending || text.trim() === ""}>
          Send
        </Button>
      </div>
    </div>
  );
}
