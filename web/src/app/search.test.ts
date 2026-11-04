import { create } from "@bufbuild/protobuf";
import { afterEach, beforeEach, expect, test } from "vitest";
import { SnapshotSchema } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";
import { normalizeThread, search } from "./search";

beforeEach(() => {
  useAthena.getState().applySnapshot(
    create(SnapshotSchema, {
      cursor: 1n,
      tickets: [
        { id: "T-1", title: "Ship the API" },
        { id: "T-12", title: "Fix the footer" },
      ],
      triggers: [
        { id: "tr-1", text: "Please fix the footer", thread: "github:acme/api#12" },
        { id: "tr-2", text: "unrelated" },
      ],
      threads: [{ key: "github:acme/api#12" }, { key: "github:acme/api#123" }, { key: "github:Other/web#12" }],
    }),
    "e1",
  );
});
afterEach(() => useAthena.getState().reset());

const keys = (q: string) =>
  search(useAthena.getState(), q)
    .filter((h) => h.kind === "thread")
    .map((h) => h.id);

test("search finds a thread by #12 and by URL", () => {
  // "#12" ends the key: it finds #12 on every repository, never #123.
  expect(keys("#12")).toEqual(["github:acme/api#12", "github:Other/web#12"]);
  for (const url of [
    "https://github.com/acme/api/issues/12",
    "https://github.com/Acme/API/pull/12",
    "github.com/acme/api/pull/12/files",
    "https://github.com/acme/api/issues/12#issuecomment-5",
  ]) {
    expect(keys(url), url).toEqual(["github:acme/api#12"]);
  }
  expect(keys("acme/api#12")).toEqual(["github:acme/api#12"]);
  expect(keys("github:acme/api#12")).toEqual(["github:acme/api#12"]);
  expect(keys("acme/api")).toEqual(["github:acme/api#12", "github:acme/api#123"]);
  expect(normalizeThread("GitHub:Acme/Api#12")).toBe("acme/api#12");
});

test("search finds Tickets by id or title and Triggers by text, each with the route that opens it", () => {
  const hits = search(useAthena.getState(), "footer");
  expect(hits).toEqual([
    { kind: "ticket", id: "T-12", title: "Fix the footer", to: "/tickets/T-12" },
    { kind: "trigger", id: "tr-1", text: "Please fix the footer", to: "/routing?tab=triggers&trigger=tr-1" },
  ]);
  expect(search(useAthena.getState(), "t-1").map((h) => h.id)).toEqual(["T-1", "T-12"]);
  expect(search(useAthena.getState(), "github:acme/api#12").at(-1)?.to).toBe("/routing?tab=threads&thread=github%3Aacme%2Fapi%2312");
});

test("search is empty for an empty query, skips one-letter Trigger text and caps each kind", () => {
  expect(search(useAthena.getState(), "  ")).toEqual([]);
  expect(search(useAthena.getState(), "u").filter((h) => h.kind === "trigger")).toEqual([]);
  const tickets = new Map(Array.from({ length: 50 }, (_, i) => [`T-${i}`, { id: `T-${i}`, title: "x" }] as const));
  const many = search({ tickets, triggers: new Map(), threads: new Map() } as never, "t-");
  expect(many).toHaveLength(20);
});
