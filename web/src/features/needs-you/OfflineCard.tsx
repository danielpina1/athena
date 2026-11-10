// A Ticket whose node is offline (person stage "<name> is offline"). A Ticket in a repository can move: Retry elsewhere
// (primary) ends the stranded Assignment and starts again on another node from its last checkpoint; Wait (secondary)
// leaves it waiting for the node and hides the card from this view until the page reloads, sending nothing. A Ticket
// in a folder cannot move (the folder is on that machine only), so its card says only that it continues when the node
// is back.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useSyncExternalStore } from "react";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { errorMessage, retryElsewhere } from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { clockTime } from "@/lib/text/runtime";
import { Card } from "@/ui/athena/Card";
import { Button } from "@/ui/primitives/button";
import { toast } from "@/ui/primitives/toast";
import { timeOf, TicketHeader } from "./TicketHeader";

// The cards the owner chose to wait on, by Ticket and node: in memory only, so a reload shows them again.
let waited = new Set<string>();
const listeners = new Set<() => void>();
const waitKey = (t: TicketSummary) => `${t.id}@${t.nodeId}`;

function subscribe(l: () => void) {
  listeners.add(l);
  return () => void listeners.delete(l);
}

function wait(t: TicketSummary) {
  waited = new Set(waited).add(waitKey(t));
  for (const l of listeners) l();
}

/** Whether the owner chose to wait on this Ticket's node in this page's life: Needs you leaves its card out. */
export function useWaited(): (t: TicketSummary) => boolean {
  const set = useSyncExternalStore(subscribe, () => waited);
  return (t) => set.has(waitKey(t));
}

export function __resetWaitedForTest() {
  waited = new Set();
}

/** "14:02", as a <time>. */
export function Clock({ at }: { at: number }) {
  return <time dateTime={new Date(at).toISOString()}>{clockTime(at)}</time>;
}

export function OfflineCard({ ticket }: { ticket: TicketSummary }) {
  const stopping = useAthena((s) => s.stopping.has(ticket.id));
  const lastSeen = useAthena((s) => s.nodes.find((n) => n.id === ticket.nodeId)?.lastSeen);
  const name = ticket.nodeName || "Its node";
  const movable = ticket.repo !== "";

  const doRetry = async () => {
    const r = await retryElsewhere(ticket.id);
    if (r.ok) toast({ title: "Retrying elsewhere" });
    // A refusal carries the server's own sentence (the folder Ticket, a node that came back).
    else toast({ title: "Could not retry", description: r.message || errorMessage(r.code), tone: "danger" });
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
      {movable ? (
        <>
          <p>
            {`${name} is offline`}
            {lastSeen && (
              <>
                {" since "}
                <Clock at={timestampMs(lastSeen)} />
              </>
            )}
          </p>
          <div className="grid grid-cols-2 gap-3">
            <Button variant="outline" onClick={() => wait(ticket)}>
              Wait
            </Button>
            <Button variant="accent" onClick={() => void doRetry()} disabled={stopping}>
              Retry elsewhere
            </Button>
          </div>
        </>
      ) : (
        <p>{`This Ticket works in a folder on ${name}; it continues when ${name} is back.`}</p>
      )}
    </Card>
  );
}
