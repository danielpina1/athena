// What became of a Trigger, in words, and what can be done about it. The server sends one string per Trigger
// (internal/hub/views.go triggerView): space-separated parts, each `filtered:<code>`, `reached:T-1,T-2`,
// `attached:T-1`, `handedBack:T-1` or `requeued`.
import type { DecisionView, ThreadView, TriggerView } from "@/gen/athena/v1/state_pb";

export type OutcomePart =
  | { kind: "filtered"; code: string }
  | { kind: "reached" | "attached" | "handedBack"; tickets: string[] }
  | { kind: "requeued" };

export function parseOutcome(outcome: string): OutcomePart[] {
  const parts: OutcomePart[] = [];
  for (const raw of outcome.split(/\s+/).filter(Boolean)) {
    const i = raw.indexOf(":");
    const head = i < 0 ? raw : raw.slice(0, i);
    const rest = i < 0 ? "" : raw.slice(i + 1);
    if (head === "filtered") parts.push({ kind: "filtered", code: rest });
    else if (head === "reached" || head === "attached" || head === "handedBack") {
      parts.push({ kind: head, tickets: rest.split(",").filter(Boolean) });
    } else if (head === "requeued") parts.push({ kind: "requeued" });
  }
  return parts;
}

/**
 * Filter codes in short sentences. The server emits one today: core.CodeNotListening (internal/core/intake.go), when
 * no Source listens to the Trigger's Thread. An unknown code reads as itself with the underscores spaced out.
 */
const FILTER_WORDS: Record<string, string> = {
  not_listening: "no source listens to this thread",
};

export function filterInWords(code: string): string {
  return FILTER_WORDS[code] ?? code.replaceAll("_", " ");
}

/** "Filtered: no source listens to this thread". */
export function filteredText(code: string): string {
  return `Filtered: ${filterInWords(code)}`;
}

type Records = { threads: ReadonlyMap<string, ThreadView>; decisions: ReadonlyMap<string, DecisionView> };

const SETTLING_KINDS = new Set(["accept", "ignore"]);

/**
 * Whether the server accepts Evaluate again for this Trigger and the reference offers it (docs/reference/005-panel.md,
 * "Reading individual records": a Trigger with no effective judgement, that is one filtered by its Source, an
 * unavailable Eval, or a delivered message with neither a non-stop Decision nor an Executor acknowledgement).
 *
 *  - The server (core.Engine.Reevaluate, internal/core/evals.go) refuses a Trigger an Eval holds, and one without a
 *    Thread. The view does not say which Triggers an Eval holds, so a Thread with an Eval in progress is never offered.
 *  - "Effective judgement" is a Decision of kind accept or ignore that lists the Trigger; stop and unavailable
 *    judge nothing (core `settled`).
 *  - An Executor's acknowledgement is not in the view, so a delivered Trigger an Executor acknowledged is still
 *    offered; the server accepts that replay too.
 */
export function canReevaluate(t: TriggerView, { threads, decisions }: Records): boolean {
  if (!t.thread) return false;
  if (threads.get(t.thread)?.eval) return false;
  const parts = parseOutcome(t.outcome);
  const filtered = parts.some((p) => p.kind === "filtered");
  const delivered = parts.some((p) => p.kind === "reached");
  let unavailable = false;
  for (const d of decisions.values()) {
    if (!d.triggers.includes(t.id)) continue;
    if (SETTLING_KINDS.has(d.kind)) return false;
    if (d.kind === "unavailable") unavailable = true;
  }
  return filtered || unavailable || delivered;
}

/** Dismiss: the server dismisses only an unavailable Decision (core.Engine.Dismiss); a dismissed one is done. */
export function canDismiss(d: DecisionView): boolean {
  return d.kind === "unavailable" && !d.dismissedAt;
}

export type TicketChip = { id: string; flags: string[] };

/** "T-0001(own,bound)" as the Thread view lists its subscribed Tickets. */
export function parseTicketChip(s: string): TicketChip {
  const m = /^([^(]*)(?:\(([^)]*)\))?$/.exec(s);
  return { id: m?.[1] ?? s, flags: (m?.[2] ?? "").split(",").filter(Boolean) };
}
