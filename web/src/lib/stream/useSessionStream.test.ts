import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { fakeStreamLink, item, says, type FakeStreamLink } from "./testkit";
import { CREDIT_BATCH, MAX_ITEMS, connectStreams, itemKey, useSessionStream } from "./useSessionStream";

let link: FakeStreamLink;
let disconnect: () => void;

beforeEach(() => {
  link = fakeStreamLink("open");
  disconnect = connectStreams(link);
});
afterEach(() => disconnect());

const tick = () => act(() => new Promise((r) => setTimeout(r, 0)));

test("opens from the tail on mount and closes on unmount", async () => {
  const view = renderHook(() => useSessionStream("T-1"));
  expect(link.opens()).toEqual([{ ticketId: "T-1", fromOffset: -1n }]);
  view.unmount();
  await tick();
  expect(link.closes()).toEqual(["T-1"]);
});

test("a StrictMode-style remount sends nothing more", async () => {
  const a = renderHook(() => useSessionStream("T-1"));
  a.unmount();
  const b = renderHook(() => useSessionStream("T-1"));
  await tick();
  expect(link.opens()).toHaveLength(1);
  expect(link.closes()).toEqual([]);
  b.unmount();
  await tick();
  expect(link.closes()).toEqual(["T-1"]);
});

test("two views of one Ticket share its stream", async () => {
  const a = renderHook(() => useSessionStream("T-1"));
  const b = renderHook(() => useSessionStream("T-1"));
  expect(link.opens()).toHaveLength(1);
  act(() => link.items("T-1", says(2)));
  expect(b.result.current.items).toHaveLength(2);
  a.unmount();
  await tick();
  expect(link.closes()).toEqual([]);
  expect(b.result.current.items).toHaveLength(2);
});

test("stream dedupes resent items", () => {
  const { result } = renderHook(() => useSessionStream("T-1"));
  const line = [
    item({ offset: 10n, kind: "tool_call", tool: "Bash", toolUseId: "tu1", text: "ls" }),
    item({ offset: 10n, kind: "say", text: "listing" }),
  ];
  act(() => link.items("T-1", [item({ offset: 0n, kind: "init", text: "session s model m" }), line[0]]));
  // The rest of the line, resent whole, then the next line.
  act(() => link.items("T-1", [...line, item({ offset: 20n, kind: "say", text: "done" })]));
  expect(result.current.items.map((i) => `${i.offset}:${i.kind}`)).toEqual(["0:init", "10:tool_call", "10:say", "20:say"]);
  expect(itemKey(line[0])).not.toBe(itemKey(line[1]));
  expect(itemKey(line[1])).toBe(itemKey(item({ offset: 10n, kind: "say", text: "listing" })));
  expect(itemKey(line[1])).not.toBe(itemKey(item({ offset: 10n, kind: "say", text: "listinG" })));
});

test("stream keeps 2000 items", () => {
  const { result } = renderHook(() => useSessionStream("T-1"));
  const all = says(MAX_ITEMS + 150);
  act(() => {
    for (let i = 0; i < all.length; i += 50) link.items("T-1", all.slice(i, i + 50));
  });
  expect(MAX_ITEMS).toBe(2000);
  expect(result.current.items).toHaveLength(2000);
  expect(result.current.items[0].offset).toBe(1500n);
  expect(result.current.items.at(-1)?.offset).toBe(BigInt((MAX_ITEMS + 149) * 10));
});

test("re-opens from last offset after reconnect", () => {
  const { result } = renderHook(() => useSessionStream("T-1"));
  act(() => link.items("T-1", says(5)));
  expect(result.current.live).toBe(true);
  act(() => link.set("offline"));
  expect(result.current.live).toBe(false);
  act(() => link.set("connecting"));
  act(() => link.set("open"));
  expect(link.opens()).toEqual([
    { ticketId: "T-1", fromOffset: -1n },
    { ticketId: "T-1", fromOffset: 41n },
  ]);
  expect(result.current.live).toBe(true);
  expect(result.current.items).toHaveLength(5);
});

test("waits for the link before opening", () => {
  link.set("connecting");
  renderHook(() => useSessionStream("T-1"));
  expect(link.opens()).toEqual([]);
  act(() => link.set("open"));
  expect(link.opens()).toEqual([{ ticketId: "T-1", fromOffset: -1n }]);
});

test("grants credit in batches of 64", () => {
  renderHook(() => useSessionStream("T-1"));
  expect(CREDIT_BATCH).toBe(64);
  act(() => link.items("T-1", says(63)));
  expect(link.grants).toEqual([]);
  act(() => link.items("T-1", says(1, 1000)));
  expect(link.grants).toEqual([64]);
  // Resent items cost credit too: the server counted them.
  act(() => link.items("T-1", says(130)));
  expect(link.grants).toEqual([64, 64, 64]);
  // Credit does not carry over: a new connection starts from its own implicit budget.
  act(() => link.set("offline"));
  act(() => link.set("open"));
  act(() => link.items("T-1", says(63, 5000)));
  expect(link.grants).toEqual([64, 64, 64]);
  act(() => link.items("T-1", says(1, 9000)));
  expect(link.grants).toEqual([64, 64, 64, 64]);
});

test("items for a closed stream still count towards credit", async () => {
  const view = renderHook(() => useSessionStream("T-1"));
  view.unmount();
  await tick();
  act(() => link.items("T-1", says(64)));
  expect(link.grants).toEqual([64]);
});

test("a gap is kept, and a refused stream is not live", () => {
  const { result } = renderHook(() => useSessionStream("T-1"));
  act(() => link.push({ case: "streamGap", value: { ticketId: "T-1", from: 100n, to: 900n } }));
  expect(result.current.gap).toEqual({ from: 100n, to: 900n });
  expect(result.current.refused).toBe(false);
  act(() => link.push({ case: "streamClose", value: { ticketId: "T-1" } }));
  expect(result.current.live).toBe(false);
  expect(result.current.refused).toBe(true);
  // An offline link is not live either, but nothing was refused.
  act(() => link.set("offline"));
  act(() => link.set("open"));
  expect(result.current.refused).toBe(false);
  act(() => link.set("offline"));
  expect(result.current).toMatchObject({ live: false, refused: false });
  act(() => link.items("T-2", says(3)));
  expect(result.current.items).toEqual([]);
});
