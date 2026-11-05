// How the graph draws a Ticket and a link: the canvas and the legend share these, so the legend is the picture.
// Orange marks only what needs the owner; every pair meets its contrast minimum against a --bg lane.
import { stageLabel } from "@/ui/athena/StageBadge";
import type { EdgeKind, GNode, NodeLook } from "./layout";

type BoxStyle = { box: string; text: string; meta: string; width: number; dash?: string };

export const LOOK: Record<NodeLook, BoxStyle> = {
  running: { box: "fill-surface stroke-text", text: "fill-text", meta: "fill-muted", width: 2 },
  queued: { box: "fill-surface stroke-muted", text: "fill-text", meta: "fill-muted", width: 1, dash: "4 3" },
  waiting: { box: "fill-surface stroke-muted", text: "fill-text", meta: "fill-muted", width: 1, dash: "4 3" },
  // An ink edge on the orange: the node stands 3:1 off its lane in both themes (orange alone is 2.9:1 on #F5F5F5).
  "needs-you": { box: "fill-accent stroke-accent-fg", text: "fill-accent-fg", meta: "fill-accent-fg", width: 1 },
  review: { box: "fill-accent stroke-accent-fg", text: "fill-accent-fg", meta: "fill-accent-fg", width: 1 },
  ended: { box: "fill-surface-2 stroke-muted", text: "fill-muted", meta: "fill-muted", width: 1 },
};

export type LineStyle = { stroke: string; width: number; dash?: string; arrow?: "ink" | "muted"; round?: boolean };

export const EDGE: Record<EdgeKind, LineStyle & { label: string }> = {
  thread: { label: "Same Thread", stroke: "stroke-muted", width: 1.2, dash: "5 4" },
  "follow-up": { label: "Follow-up", stroke: "stroke-text", width: 1.5, dash: "8 3 2 3", arrow: "ink" },
  hand_off: { label: "Hand-off", stroke: "stroke-text", width: 1.5, arrow: "ink" },
  wait_for: { label: "Waits for", stroke: "stroke-muted", width: 1.5, dash: "1 4", arrow: "muted", round: true },
  ask_agent: { label: "Agents talking", stroke: "stroke-text", width: 1.2, dash: "5 4", arrow: "ink" },
  overlap: { label: "Same files touched", stroke: "stroke-accent", width: 2, dash: "2 3" },
};

/** The legend's Tasks, in the mockup's words (docs/design/mockups/task-graph.html), plus Ended. */
export const TASK_KEY: ReadonlyArray<{ look: NodeLook; label: string }> = [
  { look: "running", label: "Running" },
  { look: "needs-you", label: "Needs you" },
  { look: "review", label: "Ready for review" },
  { look: "queued", label: "Queued · waiting" },
  { look: "ended", label: "Ended" },
];
export const LINK_KEY: readonly EdgeKind[] = ["thread", "follow-up", "hand_off", "wait_for", "ask_agent", "overlap"];

export const stageOf = (n: GNode) =>
  stageLabel({ stage: n.ticket.stage, action: n.ticket.action, outcome: n.ticket.outcome });

export function Box({ look, x, y, w, h }: { look: NodeLook; x: number; y: number; w: number; h: number }) {
  const s = LOOK[look];
  return (
    <>
      <rect data-box x={x} y={y} width={w} height={h} className={s.box} strokeWidth={s.width} strokeDasharray={s.dash} />
      {/* Ready for review needs the owner (orange) and is told apart by an ink bar. */}
      {look === "review" && <rect x={x} y={y} width={4} height={h} className="fill-accent-fg" />}
    </>
  );
}

export function Markers({ id }: { id: string }) {
  return (
    <defs>
      {(["ink", "muted"] as const).map((tone) => (
        <marker
          key={tone}
          id={`${id}-${tone}`}
          viewBox="0 0 10 10"
          refX="9"
          refY="5"
          markerWidth="7"
          markerHeight="7"
          orient="auto-start-reverse"
        >
          <path d="M1 1L9 5L1 9" fill="none" strokeWidth="1.5" className={tone === "ink" ? "stroke-text" : "stroke-muted"} />
        </marker>
      ))}
    </defs>
  );
}

export function Line({ style, d, markers }: { style: LineStyle; d: string; markers: string }) {
  return (
    <path
      d={d}
      fill="none"
      className={style.stroke}
      strokeWidth={style.width}
      strokeDasharray={style.dash}
      strokeLinecap={style.round ? "round" : undefined}
      markerEnd={style.arrow ? `url(#${markers}-${style.arrow})` : undefined}
    />
  );
}
