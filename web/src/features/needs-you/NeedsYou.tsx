// The Needs you page: every approval, question, review, stuck Ticket, Ticket whose node is offline, split to approve and
// lead whose child failed that waits on the owner, approvals first, then answers, then the rest, each oldest first (selectNeedsYou). An offline card the owner
// chose to wait on is left out until the page reloads (OfflineCard). On the desktop j and k move between the cards and a, d
// and r act on the one in focus (each card registers its own keys).
//
// An approval the owner has just answered is not waiting on anyone, so the header and the sidebar count drop at once
// (R30), but its card stays, saying "Approved" or "Denied", until the server's Delta lands (R40).
import { timestampMs, type Timestamp } from "@bufbuild/protobuf/wkt";
import { useMemo, useRef, useState } from "react";
import type { ApprovalView, NodeStatus, TicketSummary } from "@/gen/athena/v1/state_pb";
import { selectCounts, selectNeedsYou, selectSynced, type NeedsYouItem } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { useLayout } from "@/app/Panes";
import { useShortcut } from "@/app/shortcuts";
import { LiveFeed } from "@/features/activity/LiveFeed";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Waiting } from "@/ui/athena/Waiting";
import { Button } from "@/ui/primitives/button";
import { AnswerCard } from "./AnswerCard";
import { BrakeNotice } from "./BrakeNotice";
import { ChildCard } from "./ChildCard";
import { ApprovalCard } from "./ApprovalCard";
import { OfferCard } from "./OfferCard";
import { OfflineCard, useWaited } from "./OfflineCard";
import { PlanCard } from "./PlanCard";
import { ProposalCard } from "./ProposalCard";
import { ReviewCard } from "./ReviewCard";
import { StalledCard } from "./StalledCard";

/** Lists render at most this many rows, then "Show more". */
const PAGE = 200;

const ms = (t: Timestamp | undefined) => (t ? timestampMs(t) : 0);

function ItemCard({ item }: { item: NeedsYouItem }) {
  switch (item.kind) {
    case "approval":
      return item.approval ? <ApprovalCard ticket={item.ticket} approval={item.approval} /> : null;
    case "proposal":
      return item.proposal ? <ProposalCard ticket={item.ticket} proposal={item.proposal} /> : null;
    case "answer":
      return <AnswerCard key={item.ticket.attentionQuestion} ticket={item.ticket} />;
    case "review":
      return <ReviewCard ticket={item.ticket} />;
    case "offline":
      return <OfflineCard ticket={item.ticket} />;
    case "plan":
      return <PlanCard ticket={item.ticket} />;
    case "child":
      return <ChildCard ticket={item.ticket} />;
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

/** The header's node and load: the one node's name, or how many nodes are up, then the sessions running of the slots. */
function load(nodes: readonly NodeStatus[]): string[] {
  if (nodes.length === 0) return [];
  if (nodes.length === 1) {
    const n = nodes[0];
    return [n.name || "this machine", `${n.slotsInUse} of ${n.slots} running`];
  }
  const up = nodes.filter((n) => n.state === "online" || n.state === "draining" || n.state === "");
  const used = up.reduce((a, n) => a + n.slotsInUse, 0);
  const slots = up.reduce((a, n) => a + n.slots, 0);
  return [`${up.length} of ${nodes.filter((n) => n.state !== "revoked").length} nodes up`, `${used} of ${slots} running`];
}

export function NeedsYou() {
  const live = useAthena((s) => selectNeedsYou(s));
  const answering = useAthena((s) => s.answering);
  const approvals = useAthena((s) => s.approvals);
  const tickets = useAthena((s) => s.tickets);
  const counts = useAthena((s) => selectCounts(s));
  const nodes = useAthena((s) => s.nodes);
  const repos = useAthena((s) => s.repos);
  const offers = useAthena((s) => s.offers);
  const synced = useAthena(selectSynced);
  const [shown, setShown] = useState(PAGE);
  // The desktop has the live feed in its sidebar; the phone has it here, under the cards.
  const phone = useLayout() === "phone";
  const list = useRef<HTMLUListElement>(null);

  const waited = useWaited();
  const all = useMemo(() => withAnswered(live, answering, approvals, tickets), [live, answering, approvals, tickets]);
  const items = all.filter((i) => i.kind !== "offline" || !waited(i.ticket));

  const move = (by: 1 | -1) => {
    const cards = [...(list.current?.querySelectorAll<HTMLElement>("[data-needs-you-card]") ?? [])];
    if (cards.length === 0) return;
    const at = cards.findIndex((c) => c.contains(document.activeElement));
    const next = at < 0 ? (by === 1 ? 0 : cards.length - 1) : Math.min(cards.length - 1, Math.max(0, at + by));
    cards[next].focus();
  };
  useShortcut("j", () => move(1));
  useShortcut("k", () => move(-1));

  const header = [`${counts.needsYou} need you`, ...load(nodes)].join(" · ");

  return (
    <section className="grid min-w-0 content-start gap-4 p-4 lg:max-w-3xl lg:p-6">
      <div className="grid gap-1">
        <h1 className="text-xl font-semibold break-words">Needs you</h1>
        {synced && <p className="font-mono text-xs text-muted">{header}</p>}
      </div>
      {synced && <BrakeNotice repos={repos} />}
      {!synced ? (
        <Waiting className="border-t border-line py-8" />
      ) : items.length === 0 && offers.length === 0 ? (
        <EmptyState
          title="Nothing needs you."
          description={`Today: ${counts.working} working, ${counts.endedToday} ended, ${counts.mergedToday} merged.`}
        />
      ) : (
        <>
          <ul ref={list} className="grid gap-3">
            {items.slice(0, shown).map((item) => (
              <li key={item.approval ? `approval:${item.approval.id}` : item.proposal ? `proposal:${item.proposal.id}` : `${item.kind}:${item.ticket.id}`} className="min-w-0">
                <ItemCard item={item} />
              </li>
            ))}
          </ul>
          {items.length > shown && (
            <Button onClick={() => setShown((n) => n + PAGE)}>{`Show more (${items.length - shown})`}</Button>
          )}
          {offers.length > 0 && (
            <ul aria-label="Always allow" className="grid gap-3">
              {offers.map((o) => (
                <li key={o.class} className="min-w-0">
                  <OfferCard offer={o} />
                </li>
              ))}
            </ul>
          )}
        </>
      )}
      {phone && synced && <LiveFeed limit={5} className="mt-4 border-t border-line pt-4" />}
    </section>
  );
}
