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
  RuntimeStatusSchema,
  TimelineEntrySchema,
  type NodeStatus,
  type RuntimeStatus,
  type TicketDetail,
  type TimelineEntry,
} from "../src/gen/athena/v1/state_pb";

export type LinkEdits = {
  /** Shapes the node the page is told about. */
  node?: (n: NodeStatus) => void;
  /** Shapes a Ticket's detail (its summary, timeline and reports). */
  detail?: (d: TicketDetail) => void;
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
      if (body.case === "snapshot" && body.value.node) edits.node?.(body.value.node);
      if (body.case === "delta") {
        const change = body.value.change;
        if (change.case === "node") edits.node?.(change.value);
        if (change.case === "ticketDetail") edits.detail?.(change.value);
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
