// The Needs you page: every approval, question, review and stuck Ticket that waits on the owner, approvals first, then
// answers, then the rest, each oldest first (selectNeedsYou). On the desktop j and k move between the cards and a, d
// and r act on the one in focus (each card registers its own keys).
//
// An approval the owner has just answered is not waiting on anyone, so the header and the sidebar count drop at once
// (R30), but its card stays, saying "Approved" or "Denied", until the server's Delta lands (R40).
import { timestampMs, type Timestamp } from "@bufbuild/protobuf/wkt";
import { useMemo, useRef, useState } from "react";
import type { ApprovalView, TicketSummary } from "@/gen/athena/v1/state_pb";
import { selectCounts, selectNeedsYou, type NeedsYouItem } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { useShortcut } from "@/app/shortcuts";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Button } from "@/ui/primitives/button";
import { AnswerCard } from "./AnswerCard";
import { ApprovalCard } from "./ApprovalCard";
import { ReviewCard } from "./ReviewCard";
import { StalledCard } from "./StalledCard";

/** Lists render at most this many rows, then "Show more". */
const PAGE = 200;

const ms = (t: Timestamp | undefined) => (t ? timestampMs(t) : 0);

function ItemCard({ item }: { item: NeedsYouItem }) {
  switch (item.kind) {
    case "approval":
      return item.approval ? <ApprovalCard ticket={item.ticket} approval={item.approval} /> : null;
    case "answer":
      return <AnswerCard key={item.ticket.attentionQuestion} ticket={item.ticket} />;
    case "review":
      return <ReviewCard ticket={item.ticket} />;
    default:
      return <StalledCard ticket={item.ticket} />;
  }
}

/** The live items with the approvals being answered put back among the approvals, oldest first. */
function withAnswered(
  live: NeedsYouItem[],
  answering: ReadonlyMap<string, boolean>,
  approvals: ReadonlyMap<string, ApprovalView>,
  tickets: ReadonlyMap<string, TicketSummary>,
) {
  if (answering.size === 0) return live;
  const answered: NeedsYouItem[] = [];
  for (const id of answering.keys()) {
    const approval = approvals.get(id);
    const ticket = approval && tickets.get(approval.ticketId);
    if (approval && ticket) answered.push({ kind: "approval", ticket, approval });
  }
  const merged = [...live.filter((i) => i.kind === "approval"), ...answered].sort(
    (a, b) => ms(a.approval?.createdAt) - ms(b.approval?.createdAt) || (a.ticket.id < b.ticket.id ? -1 : 1),
  );
  return [...merged, ...live.filter((i) => i.kind !== "approval")];
}

export function NeedsYou() {
  const live = useAthena((s) => selectNeedsYou(s));
  const answering = useAthena((s) => s.answering);
  const approvals = useAthena((s) => s.approvals);
  const tickets = useAthena((s) => s.tickets);
  const counts = useAthena((s) => selectCounts(s));
  const node = useAthena((s) => s.node);
  const [shown, setShown] = useState(PAGE);
  const list = useRef<HTMLUListElement>(null);

  const items = useMemo(() => withAnswered(live, answering, approvals, tickets), [live, answering, approvals, tickets]);

  const move = (by: 1 | -1) => {
    const cards = [...(list.current?.querySelectorAll<HTMLElement>("[data-needs-you-card]") ?? [])];
    if (cards.length === 0) return;
    const at = cards.findIndex((c) => c.contains(document.activeElement));
    const next = at < 0 ? (by === 1 ? 0 : cards.length - 1) : Math.min(cards.length - 1, Math.max(0, at + by));
    cards[next].focus();
  };
  useShortcut("j", () => move(1));
  useShortcut("k", () => move(-1));

  const header = [
    `${counts.needsYou} need you`,
    ...(node ? [node.name || "this machine", `${node.slotsInUse} of ${node.slots} running`] : []),
  ].join(" · ");

  return (
    <section className="grid min-w-0 content-start gap-4 p-4 lg:max-w-3xl lg:p-6">
      <div className="grid gap-1">
        <h1 className="text-xl font-semibold break-words">Needs you</h1>
        <p className="font-mono text-xs text-muted">{header}</p>
      </div>
      {items.length === 0 ? (
        <EmptyState
          title="Nothing needs you."
          description={`Today: ${counts.working} working, ${counts.endedToday} ended, ${counts.mergedToday} merged.`}
        />
      ) : (
        <>
          <ul ref={list} className="grid gap-3">
            {items.slice(0, shown).map((item) => (
              <li key={item.approval ? `approval:${item.approval.id}` : `${item.kind}:${item.ticket.id}`} className="min-w-0">
                <ItemCard item={item} />
              </li>
            ))}
          </ul>
          {items.length > shown && (
            <Button onClick={() => setShown((n) => n + PAGE)}>{`Show more (${items.length - shown})`}</Button>
          )}
        </>
      )}
    </section>
  );
}
