// Edits what the server tells one page, for screenshots of states the shared e2e server does not reach on its own: a
// runtime that is limited, untested or never measured, a stale quota reading, a failover and a review on a Ticket's
// timeline. Every frame of the page's link (/ws) passes through; the node of a Snapshot or a node Delta, and the
// detail Delta of a Ticket, go through the edit first. The page's own frames reach the server unchanged, so its
// Commands are real. Set it up before the page connects (before signedInPage); a reload brings a Snapshot that the
// edit's latest version shapes.
import { create, fromBinary, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import type { Page } from "@playwright/test";
import { FrameSchema } from "../src/gen/athena/v1/link_pb";
import {
  OfferListSchema,
  RuntimeStatusSchema,
  TimelineEntrySchema,
  type NodeStatus,
  type PromotionOffer,
  type ProposalView,
  type RepoView,
  type RuntimeStatus,
  type TicketDetail,
  type TicketSummary,
  type TimelineEntry,
  type TriggerView,
} from "../src/gen/athena/v1/state_pb";

export type LinkEdits = {
  /** Shapes the node the page is told about. */
  node?: (n: NodeStatus) => void;
  /** Shapes a Ticket's detail (its summary, timeline and reports). */
  detail?: (d: TicketDetail) => void;
  /** Shapes each Ticket summary (a Snapshot's, a Ticket Delta's and a detail's). */
  ticket?: (t: TicketSummary) => void;
  /** Shapes each Trigger (a Snapshot's and a Trigger Delta's). */
  trigger?: (t: TriggerView) => void;
  /** Shapes the repository list (a Snapshot's and a repos Delta's); it may push to or replace the entries. */
  repos?: (r: RepoView[]) => void;
  /** Shapes the held Proposals a Snapshot carries; it may push to or replace the entries. */
  proposals?: (p: ProposalView[]) => void;
  /** Shapes the promotion offers a Snapshot carries (and an offers Delta); it may push to or replace the entries. */
  offers?: (o: PromotionOffer[]) => void;
};

/** Routes the page's link through `edits`. The object is read on every frame, so a test may swap its edits. */
export async function editLink(page: Page, edits: LinkEdits): Promise<void> {
  await page.routeWebSocket(/\/ws$/, (ws) => {
    const server = ws.connectToServer();
    server.onMessage((message) => {
      if (typeof message === "string") {
        ws.send(message);
        return;
      }
      const f = fromBinary(FrameSchema, new Uint8Array(message));
      const body = f.body;
      if (body.case === "snapshot") {
        if (body.value.node) edits.node?.(body.value.node);
        body.value.tickets.forEach((t) => edits.ticket?.(t));
        body.value.triggers.forEach((t) => edits.trigger?.(t));
        edits.repos?.(body.value.repos);
        edits.proposals?.(body.value.proposals);
        if (body.value.offers) edits.offers?.(body.value.offers.offers);
        else if (edits.offers) {
          body.value.offers = create(OfferListSchema, {});
          edits.offers(body.value.offers.offers);
        }
      }
      if (body.case === "delta") {
        const change = body.value.change;
        if (change.case === "node") edits.node?.(change.value);
        if (change.case === "ticketDetail") {
          edits.detail?.(change.value);
          if (change.value.summary) edits.ticket?.(change.value.summary);
        }
        if (change.case === "ticket") edits.ticket?.(change.value);
        if (change.case === "trigger") edits.trigger?.(change.value);
        if (change.case === "repos") edits.repos?.(change.value.repos);
        if (change.case === "offers") edits.offers?.(change.value.offers);
      }
      ws.send(Buffer.from(toBinary(FrameSchema, f)));
    });
  });
}

export const runtimeStatus = (init: MessageInitShape<typeof RuntimeStatusSchema>): RuntimeStatus => create(RuntimeStatusSchema, init);

export const timelineEntry = (init: MessageInitShape<typeof TimelineEntrySchema>): TimelineEntry => create(TimelineEntrySchema, init);

/** A node of fixed name and load, so a screenshot does not depend on what the specs before it left running. */
export function fixedNode(runtimes: RuntimeStatus[]): (n: NodeStatus) => void {
  return (n) => {
    n.name = "studio";
    n.slots = 4;
    n.slotsInUse = 1;
    n.queued = 0;
    n.liveTicketIds = [];
    n.runtimes = runtimes;
  };
}
