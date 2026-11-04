// The graph as a list: the accessible fallback, and what a screen reader reads on the phone. Every node of the model,
// lane by lane (newest first), with its stage and its edges in words. Up to 200 rows, then "Show more".
import { useId, useMemo, useState } from "react";
import { Link } from "react-router";
import { ticketRoute } from "@/app/search";
import { Mono } from "@/ui/athena/Mono";
import { StageBadge } from "@/ui/athena/StageBadge";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import type { GEdge, GNode, GraphModel } from "./layout";

const PAGE = 200;

/** One edge as seen from `id`, in words. */
export function edgeInWords(e: GEdge, id: string): string {
  const out = e.from === id;
  const other = out ? e.to : e.from;
  const on = e.threads.length ? ` (${e.threads.join(", ")})` : "";
  switch (e.kind) {
    case "thread":
      return `Same Thread as ${other}${on}`;
    case "follow-up":
      return out ? `Follow-up routed to ${other}${on}` : `Follow-up from ${other}${on}`;
    case "hand_off":
      return out ? `Handed off to ${other}` : `Handed off from ${other}`;
    case "wait_for":
      return out ? `Waits for ${other}` : `${other} waits for it`;
    case "ask_agent":
      return `Agents talking with ${other}`;
    case "overlap":
      return `Touches the same files as ${other}`;
  }
}

function Row({ node, edges }: { node: GNode; edges: GEdge[] }) {
  const t = node.ticket;
  return (
    <li data-list-node={node.id} className="grid min-w-0 gap-1 border-b border-line-soft px-4 py-3">
      <Link
        to={ticketRoute(node.id)}
        title={node.title}
        className="flex min-h-11 min-w-0 items-center gap-2 font-semibold text-text underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0"
      >
        {node.needsYou && <span aria-hidden className="size-2 shrink-0 rounded-full bg-accent" />}
        <span dir="auto" className="line-clamp-2 min-w-0 flex-1 break-words wrap-anywhere">
          {node.title || "Untitled"}
        </span>
      </Link>
      <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
        <Mono tone="default">{node.id}</Mono>
        <StageBadge stage={t.stage} action={t.action} outcome={t.outcome} />
      </span>
      <ul className="grid gap-0.5 text-sm text-muted">
        {edges.length === 0 ? (
          <li>No links</li>
        ) : (
          edges.map((e) => (
            <li key={e.id} className="break-words wrap-anywhere">
              {edgeInWords(e, node.id)}
            </li>
          ))
        )}
      </ul>
    </li>
  );
}

export function GraphList({ model, className }: { model: GraphModel; className?: string }) {
  const [shown, setShown] = useState(PAGE);
  const ids = useId();
  const { lanes, rows, edgesOf } = useMemo(() => {
    const byLane = new Map<string, GNode[]>();
    for (const n of model.nodes) byLane.set(n.lane, [...(byLane.get(n.lane) ?? []), n]);
    const edgesOf = new Map<string, GEdge[]>();
    for (const e of model.edges) {
      edgesOf.set(e.from, [...(edgesOf.get(e.from) ?? []), e]);
      edgesOf.set(e.to, [...(edgesOf.get(e.to) ?? []), e]);
    }
    // Newest first in each lane.
    // `before`: the rows of the lanes above, for the 200-row page.
    const lanes: Array<{ lane: GraphModel["lanes"][number]; nodes: GNode[]; before: number }> = [];
    for (const l of model.lanes) {
      const prev = lanes[lanes.length - 1];
      lanes.push({ lane: l, nodes: [...(byLane.get(l.id) ?? [])].reverse(), before: prev ? prev.before + prev.nodes.length : 0 });
    }
    return { lanes, rows: model.nodes.length, edgesOf };
  }, [model]);

  return (
    <section aria-label="Tickets as a list" className={cn("min-w-0", className)}>
      {lanes.map(({ lane, nodes, before }, i) => {
        const visible = nodes.slice(0, Math.max(0, shown - before));
        if (nodes.length > 0 && visible.length === 0) return null;
        return (
          <section key={lane.id} aria-labelledby={`${ids}-lane-${i}`} className="min-w-0">
            <h2
              id={`${ids}-lane-${i}`}
              title={lane.title}
              className="border-b border-line bg-surface-2 px-4 py-2 font-mono text-xs break-words wrap-anywhere text-muted"
            >
              {lane.label}
            </h2>
            {nodes.length === 0 ? (
              <p className="px-4 py-3 text-sm text-muted">No Tickets</p>
            ) : (
              <ul className="min-w-0">
                {visible.map((n) => (
                  <Row key={n.id} node={n} edges={edgesOf.get(n.id) ?? []} />
                ))}
              </ul>
            )}
          </section>
        );
      })}
      {rows > shown && (
        <div className="p-4">
          <Button onClick={() => setShown((n) => n + PAGE)}>Show more</Button>
        </div>
      )}
    </section>
  );
}
