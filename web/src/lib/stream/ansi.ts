// Session output is stripped of terminal escapes before display: no colours in v1. The one implementation lives in
// lib/text/ansi.ts (Task 11's titles, timeline and diff use it too); the session view imports it from here, with a
// per-item cache.
import type { StreamItem } from "@/gen/athena/v1/stream_pb";
import { stripAnsi } from "@/lib/text/ansi";

export { stripAnsi };

const texts = new WeakMap<StreamItem, string>();

/** The item's text without escapes, stripped once per item: a batch strips only its own items. */
export function itemText(it: StreamItem): string {
  let t = texts.get(it);
  if (t === undefined) {
    t = stripAnsi(it.text);
    texts.set(it, t);
  }
  return t;
}
