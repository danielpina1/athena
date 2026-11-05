// Test helper: a link that records what the stream code sends and lets a test push STREAM frames and set the status.
// Nothing in the app imports it.
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { FrameSchema, type Frame } from "@/gen/athena/v1/link_pb";
import { StreamItemSchema, type StreamItem } from "@/gen/athena/v1/stream_pb";
import type { LinkStatus } from "@/lib/link/client";
import type { StreamLink } from "./useSessionStream";

export type FakeStreamLink = StreamLink & {
  /** Every frame body sent, in order. */
  sent: Frame["body"][];
  /** Every STREAM credit grant, in order. */
  grants: number[];
  set(s: LinkStatus): void;
  push(body: MessageInitShape<typeof FrameSchema>["body"]): void;
  items(ticketId: string, items: StreamItem[]): void;
  opens(): { ticketId: string; fromOffset: bigint }[];
  closes(): string[];
};

export function fakeStreamLink(initial: LinkStatus = "open"): FakeStreamLink {
  let status = initial;
  const statusCbs = new Set<(s: LinkStatus) => void>();
  const frameCbs = new Set<(f: Frame) => void>();
  const link: FakeStreamLink = {
    sent: [],
    grants: [],
    get status() {
      return status;
    },
    onStatus(cb) {
      statusCbs.add(cb);
      return () => statusCbs.delete(cb);
    },
    onFrame(cb) {
      frameCbs.add(cb);
      return () => frameCbs.delete(cb);
    },
    send(_channel, _durable, body) {
      if (status !== "open") return false;
      link.sent.push(create(FrameSchema, { body }).body);
      return true;
    },
    grantStream(n) {
      if (status !== "open") return false;
      link.grants.push(n);
      return true;
    },
    set(s) {
      status = s;
      for (const cb of [...statusCbs]) cb(s);
    },
    push(body) {
      const f = create(FrameSchema, { body });
      for (const cb of [...frameCbs]) cb(f);
    },
    items(ticketId, items) {
      link.push({ case: "streamItems", value: { ticketId, items } });
    },
    opens() {
      return link.sent.flatMap((b) => (b.case === "streamOpen" ? [{ ticketId: b.value.ticketId, fromOffset: b.value.fromOffset }] : []));
    },
    closes() {
      return link.sent.flatMap((b) => (b.case === "streamClose" ? [b.value.ticketId] : []));
    },
  };
  return link;
}

const BASE = Date.parse("2026-11-04T09:00:00Z");

/** A StreamItem; `ms` dates it that long after the fixtures' base time. */
export function item(init: MessageInitShape<typeof StreamItemSchema> & { ms?: number }): StreamItem {
  const { ms = 0, ...rest } = init;
  return create(StreamItemSchema, { at: timestampFromMs(BASE + ms), ...rest });
}

/** n say items at offsets from, from + 10, …: one per transcript line. */
export function says(n: number, from = 0): StreamItem[] {
  return Array.from({ length: n }, (_, i) => item({ offset: BigInt(from + i * 10), kind: "say", text: `line ${from + i * 10}` }));
}
