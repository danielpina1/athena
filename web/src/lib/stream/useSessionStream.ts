// useSessionStream(ticketId): a Ticket's live session output for as long as a screen shows it.
//
// One stream per Ticket, shared: a detail pane and a pinned session of the same Ticket hold one StreamOpen between
// them (the server keeps one stream per Ticket and connection, so a second open would replace the first, and the
// first view's close would end both). The first user opens it from the tail (from_offset -1); the last one closes it
// a tick later, so a StrictMode double mount or a quick move between two screens of the Ticket sends nothing.
//
// The link drops every stream with the connection. After a reconnect each stream re-opens from the offset after the
// last item it has: the next transcript line (the server's resume rule). Items of one line share its offset, and the
// server may send a line again when credit ran out in the middle of it, so items are kept once by
// offset:kind:tool_use_id:text-hash. The newest MAX_ITEMS stay.
//
// Credit is per connection and shared by all its streams (LinkClient.grantStream). Every STREAM item the server sent
// costs one, even a resent one or one for a stream already closed; items are paid back in batches of CREDIT_BATCH
// once they are rendered. A new connection starts from its own implicit budget, so what was owed is forgotten.
//
// This is separate from watching (useWatch): the stream is the session's output, the watch is the detail Deltas.
import { useCallback, useEffect, useSyncExternalStore } from "react";
import { Channel } from "@/gen/athena/v1/link_pb";
import type { StreamItem } from "@/gen/athena/v1/stream_pb";
import type { LinkClient, LinkStatus } from "@/lib/link/client";

/** What the streams need of the link. */
export type StreamLink = Pick<LinkClient, "status" | "onStatus" | "onFrame" | "send" | "grantStream">;

export type SessionStream = {
  items: StreamItem[];
  /** The latest stretch of the transcript the server skipped (byte offsets from..to). */
  gap?: { from: bigint; to: bigint };
  /** The stream is being served: the link is open and the server did not refuse it. */
  live: boolean;
};

/** The newest items a stream keeps. */
export const MAX_ITEMS = 2000;
/** Items paid back to the server at a time. */
export const CREDIT_BATCH = 64;

type Entry = {
  id: string;
  users: number;
  releasing?: ReturnType<typeof setTimeout>;
  items: StreamItem[];
  keys: Set<string>;
  gap?: { from: bigint; to: bigint };
  /** The server sent StreamClose: it will not serve this stream on this connection. */
  refused: boolean;
  /** Items received and not yet rendered (not yet owed back as credit). */
  unrendered: number;
  view: SessionStream;
};

let link: StreamLink | undefined;
let status: LinkStatus = "offline";
const entries = new Map<string, Entry>();
const listeners = new Map<string, Set<() => void>>();
/** Items rendered (or never to be rendered) and not yet granted back on this connection. */
let owed = 0;

const EMPTY: SessionStream = { items: [], live: false };

/** FNV-1a over the UTF-16 units: cheap and stable, enough to tell two texts of one line apart. */
function hash(s: string): string {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return (h >>> 0).toString(36);
}

/** The key an item is kept once by. */
export function itemKey(it: StreamItem): string {
  return `${it.offset}:${it.kind}:${it.toolUseId}:${hash(it.text)}`;
}

function notify(e: Entry) {
  e.view = { items: e.items, gap: e.gap, live: status === "open" && !e.refused };
  for (const l of listeners.get(e.id) ?? []) l();
}

function grantOwed() {
  while (owed >= CREDIT_BATCH && link?.grantStream(CREDIT_BATCH)) owed -= CREDIT_BATCH;
}

function open(e: Entry) {
  const last = e.items.at(-1)?.offset;
  link?.send(Channel.CONTROL, false, {
    case: "streamOpen",
    value: { ticketId: e.id, fromOffset: last === undefined ? -1n : last + 1n },
  });
}

function add(e: Entry, items: StreamItem[]) {
  e.unrendered += items.length;
  const fresh = items.filter((it) => {
    const k = itemKey(it);
    if (e.keys.has(k)) return false;
    e.keys.add(k);
    return true;
  });
  if (fresh.length === 0) return;
  let next = e.items.concat(fresh);
  if (next.length > MAX_ITEMS) {
    const drop = next.length - MAX_ITEMS;
    for (let i = 0; i < drop; i++) e.keys.delete(itemKey(next[i]));
    next = next.slice(drop);
  }
  e.items = next;
}

function onFrame(f: Parameters<Parameters<StreamLink["onFrame"]>[0]>[0]) {
  switch (f.body.case) {
    case "streamItems": {
      const { ticketId, items } = f.body.value;
      const e = entries.get(ticketId);
      if (!e) {
        // Nobody shows it (a late frame after the close): owed all the same.
        owed += items.length;
        grantOwed();
        return;
      }
      add(e, items);
      notify(e);
      return;
    }
    case "streamGap": {
      const e = entries.get(f.body.value.ticketId);
      if (!e) return;
      e.gap = { from: f.body.value.from, to: f.body.value.to };
      notify(e);
      return;
    }
    case "streamClose": {
      const e = entries.get(f.body.value.ticketId);
      if (!e) return;
      e.refused = true;
      notify(e);
      return;
    }
  }
}

function onStatus(s: LinkStatus) {
  const was = status;
  status = s;
  if (s === "open" && was !== "open") {
    // A fresh connection: no stream is open on it and its credit starts afresh.
    owed = 0;
    for (const e of entries.values()) {
      e.unrendered = 0;
      e.refused = false;
      open(e);
    }
  }
  for (const e of entries.values()) notify(e);
}

/** Serves the streams from link: the app calls it once at start (tests per test). Returns the disconnect. */
export function connectStreams(l: StreamLink): () => void {
  __resetStreamsForTest();
  link = l;
  status = l.status;
  const offFrame = l.onFrame(onFrame);
  const offStatus = l.onStatus(onStatus);
  return () => {
    offFrame();
    offStatus();
    if (link === l) {
      __resetStreamsForTest();
      link = undefined;
    }
  };
}

function acquire(id: string) {
  let e = entries.get(id);
  if (e?.releasing !== undefined) {
    clearTimeout(e.releasing);
    e.releasing = undefined;
  }
  if (!e) {
    e = { id, users: 0, items: [], keys: new Set(), refused: false, unrendered: 0, view: EMPTY };
    entries.set(id, e);
    if (status === "open") open(e);
    notify(e);
  }
  e.users++;
}

function release(id: string) {
  const e = entries.get(id);
  if (!e) return;
  if (--e.users > 0) return;
  e.releasing = setTimeout(() => {
    if (entries.get(id) !== e || e.users > 0) return;
    entries.delete(id);
    if (status === "open") link?.send(Channel.CONTROL, false, { case: "streamClose", value: { ticketId: id } });
    owed += e.unrendered;
    grantOwed();
    for (const l of listeners.get(id) ?? []) l();
  }, 0);
}

/** The items rendered since the last call are owed back. */
function rendered(id: string) {
  const e = entries.get(id);
  if (!e || e.unrendered === 0) return;
  owed += e.unrendered;
  e.unrendered = 0;
  grantOwed();
}

export function useSessionStream(ticketId: string): SessionStream {
  useEffect(() => {
    acquire(ticketId);
    return () => release(ticketId);
  }, [ticketId]);
  const subscribe = useCallback(
    (cb: () => void) => {
      let set = listeners.get(ticketId);
      if (!set) listeners.set(ticketId, (set = new Set()));
      set.add(cb);
      return () => {
        set.delete(cb);
        if (set.size === 0) listeners.delete(ticketId);
      };
    },
    [ticketId],
  );
  const view = useSyncExternalStore(subscribe, () => entries.get(ticketId)?.view ?? EMPTY);
  // After the render that shows them, the items are paid for.
  useEffect(() => rendered(ticketId), [ticketId, view]);
  return view;
}

/** Test only (and connectStreams): forgets every stream and the credit owed. */
export function __resetStreamsForTest(): void {
  for (const e of entries.values()) if (e.releasing !== undefined) clearTimeout(e.releasing);
  entries.clear();
  owed = 0;
}
