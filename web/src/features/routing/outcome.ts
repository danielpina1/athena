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

/** A Decision's reason in a sentence, for the codes that have one; undefined for the rest. */
export function reasonSentence(code: string, values: Readonly<Record<string, string>>): string | undefined {
  if (code === "wip_brake") {
    const things = values.owed === "1" ? "thing already waits" : "things already wait";
    return `Held: ${values.owed} ${things} on you in ${values.repo} (limit ${values.ceiling}).`;
  }
  if (code === "autopilot_gone") return "Not started: this Autopilot entry is off or was removed.";
  return undefined;
}

/** What the Decisions say about each Trigger: whether one judged it and whether Eval was unavailable for it. */
export type Judgement = { judged: boolean; unavailable: boolean };

/** The Decisions indexed by Trigger id, once per change of the Decisions (not once per row). */
export function indexDecisions(decisions: Iterable<DecisionView>): Map<string, Judgement> {
  const by = new Map<string, Judgement>();
  for (const d of decisions) {
    const settling = SETTLING_KINDS.has(d.kind);
    const unavailable = d.kind === "unavailable";
    if (!settling && !unavailable) continue;
    for (const id of d.triggers) {
      const j = by.get(id) ?? { judged: false, unavailable: false };
      if (settling) j.judged = true;
      if (unavailable) j.unavailable = true;
      by.set(id, j);
    }
  }
  return by;
}

type Records = { threads: ReadonlyMap<string, ThreadView>; judged: ReadonlyMap<string, Judgement> };

const SETTLING_KINDS = new Set(["accept", "ignore"]);

/**
 * Whether the server accepts Evaluate again for this Trigger and the reference offers it (docs/reference/005-panel.md,
 * "Reading individual records": a Trigger with no effective judgement, that is one filtered by its Source, an
 * unavailable Eval, or a delivered message with neither a non-stop Decision nor an Executor acknowledgement).
 *
 *  - The server (core.Engine.Reevaluate, internal/core/evals.go) refuses a Trigger an Eval holds, and one without a
 *    Thread. The view does not say which Triggers an Eval holds, so a Thread with an Eval in progress is never offered.
 *  - "Effective judgement" follows the reference's wording, not core's internal `settled`: a Decision of kind accept
 *    or ignore that lists the Trigger. A stop or an unavailable Decision judges nothing. (A Trigger a Ticket was
 *    attached to is reached and attached, and its Eval ended with an ignore: judged.)
 *  - An Executor's acknowledgement is not in the view, so a delivered Trigger an Executor acknowledged is still
 *    offered; the server accepts that replay too.
 */
export function canReevaluate(t: TriggerView, { threads, judged }: Records): boolean {
  // An owner message went straight to its Ticket: there is no Thread (the server refuses) and nothing to judge.
  if (t.source === "owner" || !t.thread) return false;
  if (threads.get(t.thread)?.eval) return false;
  const j = judged.get(t.id);
  if (j?.judged) return false;
  const parts = parseOutcome(t.outcome);
  return parts.some((p) => p.kind === "filtered" || p.kind === "reached") || !!j?.unavailable;
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
