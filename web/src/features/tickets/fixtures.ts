// Test fixtures for the Ticket views: a store loaded with Tickets and a Ticket's detail Delta.
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { DeltaSchema, SnapshotSchema, type TicketDetailSchema } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";

export const at = (ms: number) => timestampFromMs(Date.parse("2026-11-04T09:00:00Z") + ms);

export function load(init: MessageInitShape<typeof SnapshotSchema>) {
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n, ...init }), "e1");
}

/** A ticket_detail Delta (seq 0), as the server sends it to a watching connection. */
export function detail(init: MessageInitShape<typeof TicketDetailSchema>) {
  useAthena.getState().applyDelta(create(DeltaSchema, { seq: 0n, change: { case: "ticketDetail", value: init } }));
}

/** Whether the element carries every one of the classes (jest-dom is not set up here). */
export function hasClasses(el: Element | null, ...names: string[]): boolean {
  return !!el && names.every((n) => el.classList.contains(n));
}
