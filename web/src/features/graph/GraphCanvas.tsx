// The graph's SVG: lanes, edges and nodes in one pan/zoom viewport, with the lane labels in a layer of their own that
// moves only up and down, so they stay at the left edge while the owner pans sideways.
//
//  - A node is an <a href="/tickets/<id>"> named "<full title>, <id>, <stage>"; Enter or a click opens it. Its cut
//    title and id line are drawn outside the link (aria-hidden, no pointer events), so the link's own text never
//    contradicts its name, and they are clipped to the node: wide glyphs (W, CJK) never draw past its edge.
//  - `hidden` (the phone, where a visually hidden list is what a screen reader reads): the SVG is aria-hidden and its
//    nodes leave the tab order; taps still open them.
//  - The SVG itself takes focus (tabIndex -1, with a focus ring) when the node a closed pane came from is gone.
//  - A newly selected node that is off-screen is panned into view, the least distance (a jump under reduced motion).
import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef } from "react";
import { ticketRoute } from "@/app/search";
import { cn } from "@/ui/cn";
import { Box, EDGE, LOOK, Line, Markers, stageOf } from "./draw";
import { type GEdge, type GNode, type GraphModel, truncateTitle } from "./layout";
import { startView, usePanZoom, type View } from "./usePanZoom";

/** Characters of a title that fit a node (176 px at 12.5 px), and of its id and stage line; the clip does the rest. */
const TITLE_CHARS = 22;
const META_CHARS = 26;
/** The clip's right margin inside a node, and beside a running node's ring (cx = w − 12, r = 4). */
const CLIP_MARGIN = 6;
const RING_MARGIN = 28;
/** A cut title fades out over this many px before the clip's edge, rather than ending in a sliced glyph. */
const FADE = 12;

export type CanvasProps = {
  model: GraphModel;
  selected: string;
  reducedMotion: boolean;
  phone: boolean;
  /** A visually hidden list is the reading order: keep the picture out of it. */
  hidden: boolean;
  onOpen(id: string): void;
  onHover(id: string | null): void;
  className?: string;
};

const nodeName = (n: GNode) => `${n.title || "Untitled"}, ${n.id}, ${stageOf(n)}`;

/**
 * The line from a to b (a is the older, so never to the right of b). Side by side: right edge to left edge, a curve.
 * Overlapping in x (stacked rows, or lanes apart): straight down (or up) through the middle of their overlap.
 * `offset` keeps two edges between the same pair apart.
 */
function edgePath(a: GNode, b: GNode, offset = 0): string {
  if (b.x >= a.x + a.w) {
    const [x1, y1, x2, y2] = [a.x + a.w, a.y + a.h / 2 + offset, b.x, b.y + b.h / 2 + offset];
    const dx = Math.max(24, (x2 - x1) / 2);
    return `M${x1} ${y1} C${x1 + dx} ${y1} ${x2 - dx} ${y2} ${x2} ${y2}`;
  }
  const x = (b.x + Math.min(a.x + a.w, b.x + b.w)) / 2 + offset;
  const down = b.y >= a.y;
  return `M${x} ${down ? a.y + a.h : a.y} L${x} ${down ? b.y : b.y + b.h}`;
}

/** Each edge's offset: edges between the same two Tickets spread 8 px apart. */
function offsets(edges: readonly GEdge[]): Map<string, number> {
  const pairs = new Map<string, string[]>();
  for (const e of edges) {
    const k = `${e.from}|${e.to}`;
    const ids = pairs.get(k);
    if (ids) ids.push(e.id);
    else pairs.set(k, [e.id]);
  }
  const out = new Map<string, number>();
  for (const ids of pairs.values()) ids.forEach((id, i) => out.set(id, (i - (ids.length - 1) / 2) * 8));
  return out;
}

function Node({
  node,
  selected,
  hidden,
  clip,
  onOpen,
  onHover,
  wasDrag,
}: {
  node: GNode;
  selected: boolean;
  hidden: boolean;
  clip: string;
  onOpen(id: string): void;
  onHover(id: string | null): void;
  wasDrag(): boolean;
}) {
  const s = LOOK[node.look];
  const { x, y, w, h } = node;
  return (
    <g>
      <a
        href={ticketRoute(node.id)}
        tabIndex={hidden ? -1 : 0}
        data-node={node.id}
        data-look={node.look}
        aria-label={nodeName(node)}
        aria-current={selected ? "true" : undefined}
        className="group cursor-pointer outline-none"
        onClick={(e) => {
          if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
          e.preventDefault();
          if (!wasDrag()) onOpen(node.id);
        }}
        onKeyDown={(e) => {
          if (e.key !== "Enter") return;
          e.preventDefault();
          onOpen(node.id);
        }}
        onFocus={() => onHover(node.id)}
        onBlur={() => onHover(null)}
        onMouseEnter={() => onHover(node.id)}
        onMouseLeave={() => onHover(null)}
      >
        <rect
          x={x - 4}
          y={y - 4}
          width={w + 8}
          height={h + 8}
          fill="none"
          strokeWidth={2}
          className={cn("stroke-text", selected ? "opacity-100" : "opacity-0 group-focus-visible:opacity-100")}
        />
        {/* A working Ticket's faint halo pulses (CSS only; still and invisible under reduced motion). */}
        {node.look === "running" && (
          <rect
            data-glow
            aria-hidden
            x={x - 3}
            y={y - 3}
            width={w + 6}
            height={h + 6}
            fill="none"
            strokeWidth={3}
            className="motion-glow stroke-text"
          />
        )}
        <Box look={node.look} x={x} y={y} w={w} h={h} />
        {node.look === "running" && (
          <circle data-ring cx={x + w - 12} cy={y + 12} r={4} fill="none" strokeWidth={1.5} className="stroke-text" />
        )}
      </a>
      {/* For the eyes, outside the link: its name carries the whole title. Clipped to the node's box. */}
      <g
        aria-hidden
        data-node-text={node.id}
        transform={`translate(${x} ${y})`}
        clipPath={`url(#${clip})`}
        mask={`url(#${clip}-fade)`}
        pointerEvents="none"
      >
        <text x={12} y={21} fontSize={12.5} fontWeight={600} className={cn("font-sans", s.text)}>
          {truncateTitle(node.title, node.look === "running" ? TITLE_CHARS - 2 : TITLE_CHARS)}
        </text>
        <text x={12} y={40} fontSize={10} className={cn("font-mono", s.meta)}>
          {truncateTitle(`${node.id} · ${stageOf(node)}`, META_CHARS)}
        </text>
      </g>
    </g>
  );
}

export function GraphCanvas({ model, selected, reducedMotion, phone, hidden, onOpen, onHover, className }: CanvasProps) {
  const ref = useRef<SVGSVGElement>(null);
  const uid = useId().replace(/:/g, "");
  const clip = `${uid}-clip`;
  const ringClip = `${uid}-clip-ring`;
  const width = model.width;
  const right = useMemo(() => model.nodes.reduce((r, n) => Math.max(r, n.x + n.w + 16), 0), [model]);
  const home = useCallback((): View => startView(ref.current?.clientWidth ?? 0, right, phone), [right, phone]);
  const { view, setView, wasDrag, revealBox } = usePanZoom(ref, { home, reducedMotion });
  const placed = useRef(false);
  useLayoutEffect(() => {
    if (placed.current) return;
    placed.current = true;
    setView(home());
  }, [home, setView]);

  const byId = useMemo(() => new Map(model.nodes.map((n) => [n.id, n])), [model]);
  // A newly selected Ticket (a click, or ?ticket= on load) is brought into view if it is off-screen: once per
  // selection, not again while the model changes under it (the replay).
  const latest = useRef({ byId, revealBox });
  useEffect(() => {
    latest.current = { byId, revealBox };
  });
  useEffect(() => {
    const n = latest.current.byId.get(selected);
    if (n) latest.current.revealBox(n);
  }, [selected]);
  const spread = useMemo(() => offsets(model.edges), [model]);
  const byLane = useMemo(() => {
    const m = new Map<string, GNode[]>();
    for (const n of model.nodes) {
      const list = m.get(n.lane);
      if (list) list.push(n);
      else m.set(n.lane, [n]);
    }
    return m;
  }, [model]);
  const size = model.nodes[0] ?? { w: 176, h: 52 };

  return (
    <svg
      ref={ref}
      data-canvas
      tabIndex={-1}
      role={hidden ? undefined : "group"}
      aria-hidden={hidden || undefined}
      aria-label={hidden ? undefined : `Task graph: ${model.nodes.length} Tickets in ${model.lanes.length} lanes`}
      className={cn(
        "block h-full w-full cursor-grab touch-none bg-bg select-none active:cursor-grabbing",
        // The canvas takes focus when a closed pane's node is gone: it shows where focus is.
        "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-text",
        className,
      )}
    >
      <Markers id={uid} />
      <defs>
        <clipPath id={clip}>
          <rect x={0} y={0} width={size.w - CLIP_MARGIN} height={size.h} />
        </clipPath>
        {/* A running node's title stops well clear of its ring. */}
        <clipPath id={ringClip}>
          <rect x={0} y={0} width={size.w - RING_MARGIN} height={size.h / 2} />
          <rect x={0} y={size.h / 2} width={size.w - CLIP_MARGIN} height={size.h / 2} />
        </clipPath>
        {/* The same boxes as luminance masks that fade their last FADE px: a cut title ends softly. */}
        {[
          { id: clip, top: size.w - CLIP_MARGIN },
          { id: ringClip, top: size.w - RING_MARGIN },
        ].map((m) => (
          <g key={m.id}>
            <linearGradient id={`${m.id}-fade-top`} gradientUnits="userSpaceOnUse" x1={m.top - FADE} x2={m.top} y1={0} y2={0}>
              <stop offset={0} stopColor="white" />
              <stop offset={1} stopColor="black" />
            </linearGradient>
            <linearGradient
              id={`${m.id}-fade-bottom`}
              gradientUnits="userSpaceOnUse"
              x1={size.w - CLIP_MARGIN - FADE}
              x2={size.w - CLIP_MARGIN}
              y1={0}
              y2={0}
            >
              <stop offset={0} stopColor="white" />
              <stop offset={1} stopColor="black" />
            </linearGradient>
            <mask id={`${m.id}-fade`} maskUnits="userSpaceOnUse" x={0} y={0} width={size.w} height={size.h}>
              <rect x={0} y={0} width={m.top} height={size.h / 2} fill={`url(#${m.id}-fade-top)`} />
              <rect x={0} y={size.h / 2} width={size.w - CLIP_MARGIN} height={size.h / 2} fill={`url(#${m.id}-fade-bottom)`} />
            </mask>
          </g>
        ))}
      </defs>
      <g data-viewport transform={`translate(${view.x} ${view.y}) scale(${view.k})`}>
        <g aria-hidden>
          {model.lanes.map((l) => (
            <g key={l.id} data-lane={l.id} data-label={l.label}>
              {/* One background for every lane, hairlines between them: the orange overlap warning and the nodes
                  keep 3:1 against it (on --surface-2 orange is 2.9:1). */}
              <rect x={0} y={l.y} width={width} height={l.height} className="fill-bg" />
              <line x1={0} x2={width} y1={l.y + l.height} y2={l.y + l.height} className="stroke-line-soft" />
            </g>
          ))}
        </g>
        <g aria-hidden>
          {model.edges.map((e) => {
            const a = byId.get(e.from);
            const b = byId.get(e.to);
            if (!a || !b) return null;
            return (
              <g key={e.id} data-edge={e.kind}>
                <title>{`${EDGE[e.kind].label}: ${e.from} → ${e.to}${e.threads.length ? ` (${e.threads.join(", ")})` : ""}`}</title>
                <Line style={EDGE[e.kind]} d={edgePath(a, b, spread.get(e.id))} markers={uid} />
              </g>
            );
          })}
        </g>
        {model.lanes.map((l) => {
          const nodes = byLane.get(l.id);
          return nodes ? (
            <g key={l.id} role={hidden ? undefined : "group"} aria-label={hidden ? undefined : l.label}>
              {nodes.map((n) => (
                <Node
                  key={n.id}
                  node={n}
                  selected={n.id === selected}
                  hidden={hidden}
                  clip={n.look === "running" ? ringClip : clip}
                  onOpen={onOpen}
                  onHover={onHover}
                  wasDrag={wasDrag}
                />
              ))}
            </g>
          ) : null;
        })}
      </g>
      {/* The lane labels follow the lanes up and down at the screen's left edge, with a halo over whatever passes
          under them. */}
      <g data-lane-labels aria-hidden pointerEvents="none" transform={`translate(0 ${view.y})`}>
        {model.lanes.map((l) => (
          <g key={l.id}>
            <title>{l.title}</title>
            <text
              x={12}
              y={(l.y + 20) * view.k}
              fontSize={10.5}
              letterSpacing=".04em"
              strokeWidth={3}
              strokeLinejoin="round"
              paintOrder="stroke"
              className="fill-muted stroke-bg font-mono"
            >
              {truncateTitle(l.label, 72).toUpperCase()}
            </text>
          </g>
        ))}
      </g>
    </svg>
  );
}
