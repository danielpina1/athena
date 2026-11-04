// The task graph (/graph, a lazy chunk): Tickets as nodes in lanes by repository (or by machine), placed by when
// they were created, joined by the Threads they share and the follow-ups routed to them. Drawn in SVG with no graph
// library: shape and colour by stage, orange only for what needs the owner, a ring for running.
//
//  - The toolbar (By repository / By machine, Needs you only, Graph / List) is in the URL; the scrubber's position
//    is local state. ▶ Replay plays the whole window in 10 s; under prefers-reduced-motion it jumps in ten steps.
//  - A node opens its Ticket: in a detail pane beside the graph on the desktop (?ticket=), as a navigation on the
//    phone. Hover or focus shows its title and id under the legend.
//  - GraphList is the accessible fallback: a toggle shows it on any size, and on the phone a screen reader reads it
//    first (it sits visually hidden before the graph).
import { PauseIcon, PlayIcon, XIcon } from "lucide-react";
import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router";
import { useLayout, useMediaQuery } from "@/app/Panes";
import { ticketRoute } from "@/app/search";
import { useShortcut } from "@/app/shortcuts";
import { selectNeedsYou } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { stageLabel } from "@/ui/athena/StageBadge";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { topModal } from "@/ui/primitives/dialog";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";
import { TicketDetail } from "../tickets/TicketDetail";
import { useFilterParams } from "../tickets/Filters";
import { GraphList } from "./GraphList";
import {
  type EdgeKind,
  type GEdge,
  type GNode,
  type GraphModel,
  type NodeLook,
  layout,
  timeRange,
  truncateTitle,
} from "./layout";
import { usePanZoom, type View } from "./usePanZoom";

const KEYS = ["groupBy", "needsYou", "view", "ticket"] as const;

/** ▶ Replay plays the whole window in this long. */
export const REPLAY_MS = 10_000;
/** Under reduced motion the replay jumps this many times, once a second. */
const REPLAY_JUMPS = 10;
const REDUCED_MOTION = "(prefers-reduced-motion: reduce)";

/** Characters of a title that fit a node (176 px at 12.5 px), and of its id and stage line. */
const TITLE_CHARS = 22;
const META_CHARS = 28;

const nodeName = (n: GNode) => `${n.title || "Untitled"}, ${n.id}, ${stageOf(n)}`;
const stageOf = (n: GNode) => stageLabel({ stage: n.ticket.stage, action: n.ticket.action, outcome: n.ticket.outcome });

// --- How things are drawn: the graph and the legend share these. ---

type BoxStyle = { box: string; text: string; meta: string; width: number; dash?: string };

const LOOK: Record<NodeLook, BoxStyle> = {
  running: { box: "fill-surface stroke-text", text: "fill-text", meta: "fill-muted", width: 2 },
  queued: { box: "fill-surface stroke-muted", text: "fill-text", meta: "fill-muted", width: 1, dash: "4 3" },
  waiting: { box: "fill-surface stroke-muted", text: "fill-text", meta: "fill-muted", width: 1, dash: "4 3" },
  // An ink edge on the orange: the node stands 3:1 off its lane in both themes (orange alone is 2.9:1 on #F5F5F5).
  "needs-you": { box: "fill-accent stroke-accent-fg", text: "fill-accent-fg", meta: "fill-accent-fg", width: 1 },
  review: { box: "fill-accent stroke-accent-fg", text: "fill-accent-fg", meta: "fill-accent-fg", width: 1 },
  ended: { box: "fill-surface-2 stroke-muted", text: "fill-muted", meta: "fill-muted", width: 1 },
};

type LineStyle = { stroke: string; width: number; dash?: string; arrow?: "ink" | "muted"; round?: boolean };

const EDGE: Record<EdgeKind, LineStyle & { label: string }> = {
  thread: { label: "Same Thread", stroke: "stroke-muted", width: 1.2, dash: "5 4" },
  "follow-up": { label: "Follow-up", stroke: "stroke-text", width: 1.5, dash: "8 3 2 3", arrow: "ink" },
  hand_off: { label: "Hand-off", stroke: "stroke-text", width: 1.5, arrow: "ink" },
  wait_for: { label: "Waits for", stroke: "stroke-muted", width: 1.5, dash: "1 4", arrow: "muted", round: true },
  ask_agent: { label: "Agents talking", stroke: "stroke-text", width: 1.2, dash: "5 4", arrow: "ink" },
  overlap: { label: "Same files touched", stroke: "stroke-accent", width: 2, dash: "2 3" },
};

/** The legend's Tasks, in the mockup's words (docs/design/mockups/task-graph.html), plus Ended. */
const TASK_KEY: ReadonlyArray<{ look: NodeLook; label: string }> = [
  { look: "running", label: "Running" },
  { look: "needs-you", label: "Needs you" },
  { look: "review", label: "Ready for review" },
  { look: "queued", label: "Queued · waiting" },
  { look: "ended", label: "Ended" },
];
const LINK_KEY: readonly EdgeKind[] = ["thread", "follow-up", "hand_off", "wait_for", "ask_agent", "overlap"];

function Box({ look, x, y, w, h }: { look: NodeLook; x: number; y: number; w: number; h: number }) {
  const s = LOOK[look];
  return (
    <>
      <rect x={x} y={y} width={w} height={h} className={s.box} strokeWidth={s.width} strokeDasharray={s.dash} />
      {/* Ready for review needs the owner (orange) and is told apart by an ink bar. */}
      {look === "review" && <rect x={x} y={y} width={4} height={h} className="fill-accent-fg" />}
    </>
  );
}

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
    pairs.set(k, [...(pairs.get(k) ?? []), e.id]);
  }
  const out = new Map<string, number>();
  for (const ids of pairs.values()) ids.forEach((id, i) => out.set(id, (i - (ids.length - 1) / 2) * 8));
  return out;
}

function Markers({ id }: { id: string }) {
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

function Line({ style, d, markers }: { style: LineStyle; d: string; markers: string }) {
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

// --- The canvas ---

type CanvasProps = {
  model: GraphModel;
  selected: string;
  reducedMotion: boolean;
  onOpen(id: string): void;
  onHover(id: string | null): void;
  className?: string;
};

function Node({
  node,
  selected,
  onOpen,
  onHover,
  wasDrag,
}: Omit<CanvasProps, "model" | "reducedMotion" | "className" | "selected"> & {
  node: GNode;
  selected: boolean;
  wasDrag(): boolean;
}) {
  const s = LOOK[node.look];
  const { x, y, w, h } = node;
  return (
    <g>
      <a
        href={ticketRoute(node.id)}
        tabIndex={0}
        data-node={node.id}
        data-look={node.look}
        aria-label={nodeName(node)}
        aria-current={selected ? "page" : undefined}
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
        <title>{`${node.title || "Untitled"} · ${node.id}`}</title>
        <rect
          x={x - 4}
          y={y - 4}
          width={w + 8}
          height={h + 8}
          fill="none"
          strokeWidth={2}
          className={cn("stroke-text", selected ? "opacity-100" : "opacity-0 group-focus-visible:opacity-100")}
        />
        <Box look={node.look} x={x} y={y} w={w} h={h} />
        {node.look === "running" && (
          <circle data-ring cx={x + w - 12} cy={y + 12} r={4} fill="none" strokeWidth={1.5} className="stroke-text" />
        )}
      </a>
      {/* The cut title is for the eyes, outside the link: its name (aria-label) carries the whole title, and a
          cut word in the link's own text would not match it. */}
      <g aria-hidden data-node-text={node.id} pointerEvents="none">
        <text x={x + 12} y={y + 21} fontSize={12.5} fontWeight={600} className={cn("font-sans", s.text)}>
          {truncateTitle(node.title, node.look === "running" ? TITLE_CHARS - 2 : TITLE_CHARS)}
        </text>
        <text x={x + 12} y={y + 40} fontSize={10} className={cn("font-mono", s.meta)}>
          {truncateTitle(`${node.id} · ${stageOf(node)}`, META_CHARS)}
        </text>
      </g>
    </g>
  );
}

function GraphCanvas({ model, selected, reducedMotion, onOpen, onHover, className }: CanvasProps) {
  const ref = useRef<SVGSVGElement>(null);
  const markers = useId().replace(/:/g, "");
  const width = model.width;
  const right = model.nodes.reduce((r, n) => Math.max(r, n.x + n.w + 16), 0);
  // The start: the Tickets fit the width (zoomed out to 0.5× at most, so titles stay legible), from the left; when
  // they still do not fit, the newest (on the right) are in view.
  const home = useCallback((): View => {
    const vw = ref.current?.clientWidth ?? 0;
    if (vw <= 0 || right <= 0) return { k: 1, x: 0, y: 0 };
    const k = Math.min(1, Math.max(0.5, vw / right));
    return { k, x: Math.min(0, vw - right * k), y: 0 };
  }, [right]);
  const { view, setView, wasDrag } = usePanZoom(ref, { home, reducedMotion });
  const placed = useRef(false);
  useLayoutEffect(() => {
    if (placed.current) return;
    placed.current = true;
    setView(home());
  }, [home, setView]);

  const byId = useMemo(() => new Map(model.nodes.map((n) => [n.id, n])), [model]);
  const spread = useMemo(() => offsets(model.edges), [model]);
  const byLane = useMemo(() => {
    const m = new Map<string, GNode[]>();
    for (const n of model.nodes) m.set(n.lane, [...(m.get(n.lane) ?? []), n]);
    return m;
  }, [model]);

  return (
    <svg
      ref={ref}
      role="group"
      aria-label={`Task graph: ${model.nodes.length} Tickets in ${model.lanes.length} lanes`}
      className={cn("block h-full w-full cursor-grab touch-none select-none bg-bg active:cursor-grabbing", className)}
    >
      <Markers id={markers} />
      <g data-viewport transform={`translate(${view.x} ${view.y}) scale(${view.k})`}>
        <g aria-hidden>
          {model.lanes.map((l) => (
            <g key={l.id} data-lane={l.id} data-label={l.label}>
              {/* One background for every lane, hairlines between them: the orange overlap warning and the nodes keep
                  3:1 against it (on --surface-2 orange is 2.9:1). */}
              <rect x={0} y={l.y} width={width} height={l.height} className="fill-bg" />
              <line x1={0} x2={width} y1={l.y + l.height} y2={l.y + l.height} className="stroke-line-soft" />
              <text x={14} y={l.y + 20} fontSize={10.5} letterSpacing=".04em" className="fill-muted font-mono">
                <title>{l.title}</title>
                {truncateTitle(l.label, 72).toUpperCase()}
              </text>
            </g>
          ))}
        </g>
        <g aria-hidden>
          {model.edges.map((e: GEdge) => {
            const a = byId.get(e.from);
            const b = byId.get(e.to);
            if (!a || !b) return null;
            return (
              <g key={e.id} data-edge={e.kind}>
                <title>{`${EDGE[e.kind].label}: ${e.from} → ${e.to}${e.threads.length ? ` (${e.threads.join(", ")})` : ""}`}</title>
                <Line style={EDGE[e.kind]} d={edgePath(a, b, spread.get(e.id))} markers={markers} />
              </g>
            );
          })}
        </g>
        {model.lanes.map((l) =>
          byLane.has(l.id) ? (
            <g key={l.id} role="group" aria-label={l.label}>
              {byLane.get(l.id)?.map((n) => (
                <Node key={n.id} node={n} selected={n.id === selected} onOpen={onOpen} onHover={onHover} wasDrag={wasDrag} />
              ))}
            </g>
          ) : null,
        )}
      </g>
    </svg>
  );
}

// --- The legend, and what is under the pointer or in focus ---

function Legend({ shown, laneTitle }: { shown?: GNode; laneTitle: string }) {
  const markers = useId().replace(/:/g, "");
  const t = shown?.ticket;
  return (
    <div className="grid content-start gap-4 p-4 text-xs">
      <section aria-label="Legend" className="grid gap-4">
        <div className="grid gap-2">
          <h2 className="font-mono text-xs tracking-wider text-muted uppercase">Tasks</h2>
          <ul className="grid gap-2">
            {TASK_KEY.map((k) => (
              <li key={k.look} className="flex items-center gap-2">
                <svg aria-hidden width="24" height="14" className="shrink-0">
                  <Box look={k.look} x={1} y={1} w={22} h={12} />
                </svg>
                <span data-legend>{k.label}</span>
              </li>
            ))}
          </ul>
        </div>
        <div className="grid gap-2">
          <h2 className="font-mono text-xs tracking-wider text-muted uppercase">Links</h2>
          <svg aria-hidden width="0" height="0" className="absolute">
            <Markers id={markers} />
          </svg>
          <ul className="grid gap-2">
            {LINK_KEY.map((kind) => (
              <li key={kind} className="flex items-center gap-2">
                <svg aria-hidden width="28" height="10" className="shrink-0">
                  <Line style={EDGE[kind]} d="M1 5L25 5" markers={markers} />
                </svg>
                <span data-legend>{EDGE[kind].label}</span>
              </li>
            ))}
          </ul>
        </div>
      </section>
      <section aria-label="Selected" className="grid min-w-0 gap-1 border-t border-line-soft pt-3">
        <h2 className="font-mono text-xs tracking-wider text-muted uppercase">Selected</h2>
        {shown && t ? (
          <>
            <Mono className="break-all">
              {shown.id} · {laneTitle}
            </Mono>
            <p dir="auto" title={shown.title} className="line-clamp-4 text-sm font-semibold break-words wrap-anywhere">
              {shown.title || "Untitled"}
            </p>
            <p className="text-muted">
              {stageOf(shown)} · created <RelativeTime at={shown.created} />
            </p>
            {t.attentionQuestion && (
              <p dir="auto" className="line-clamp-3 break-words wrap-anywhere text-muted">
                Asked: {t.attentionQuestion}
              </p>
            )}
          </>
        ) : (
          <p className="text-muted">Point at or focus a Ticket to see it here.</p>
        )}
      </section>
    </div>
  );
}

// --- The time scrubber ---

function useClock(everyMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), everyMs);
    return () => clearInterval(id);
  }, [everyMs]);
  return now;
}

function formatTime(ms: number, withDay: boolean): string {
  return new Intl.DateTimeFormat(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    ...(withDay && { month: "short", day: "numeric" }),
  }).format(ms);
}

function Scrubber({
  start,
  end,
  at,
  playing,
  onScrub,
  onReplay,
}: {
  start: number;
  end: number;
  at: number | null;
  playing: boolean;
  onScrub(at: number | null): void;
  onReplay(): void;
}) {
  const withDay = end - start > 86_400_000;
  const value = at ?? end;
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 border-t border-line px-4 py-2 font-mono text-xs text-muted">
      {/* Wide enough for a date and a time, so the slider keeps its length whichever is shown. */}
      <span data-volatile className="min-w-[16ch] shrink-0">
        {formatTime(start, withDay)}
      </span>
      <input
        type="range"
        aria-label="Time"
        aria-valuetext={at === null ? "Now" : formatTime(value, true)}
        min={start}
        max={end}
        step={1000}
        value={value}
        disabled={end <= start}
        onChange={(e) => {
          const v = Number(e.target.value);
          onScrub(v >= end ? null : v);
        }}
        className="h-11 min-w-32 flex-1 accent-text lg:h-6"
      />
      <span data-volatile className="min-w-[16ch] shrink-0 text-right text-text">
        {at === null ? `now ${formatTime(end, false)}` : formatTime(value, withDay)}
      </span>
      <Button size="sm" onClick={onReplay} disabled={end <= start}>
        {playing ? <PauseIcon aria-hidden /> : <PlayIcon aria-hidden />}
        {playing ? "Pause" : "Replay"}
      </Button>
    </div>
  );
}

/** The replay: `at` from the window's start to now over REPLAY_MS, or in REPLAY_JUMPS steps under reduced motion. */
function useReplay(start: number, end: number, reducedMotion: boolean) {
  const [at, setAt] = useState<number | null>(null);
  const [playing, setPlaying] = useState(false);
  const span = useRef({ start, end });
  useEffect(() => {
    span.current = { start, end };
  });
  useEffect(() => {
    if (!playing) return;
    const { start, end } = span.current;
    const width = end - start;
    const done = () => {
      setAt(null);
      setPlaying(false);
    };
    if (reducedMotion || typeof requestAnimationFrame !== "function") {
      let i = 0;
      const id = setInterval(() => {
        i += 1;
        if (i >= REPLAY_JUMPS) done();
        else setAt(start + (width * i) / REPLAY_JUMPS);
      }, REPLAY_MS / REPLAY_JUMPS);
      return () => clearInterval(id);
    }
    let raf = 0;
    const t0 = performance.now();
    const step = (now: number) => {
      const p = (now - t0) / REPLAY_MS;
      if (p >= 1) return done();
      setAt(start + width * p);
      raf = requestAnimationFrame(step);
    };
    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
  }, [playing, reducedMotion]);

  return {
    at,
    playing,
    scrub(v: number | null) {
      setPlaying(false);
      setAt(v);
    },
    toggle() {
      if (playing) return setPlaying(false);
      setAt(span.current.start);
      setPlaying(true);
    },
  };
}

// --- The screen ---

export function Graph() {
  const layoutMode = useLayout();
  const desktop = layoutMode === "desktop";
  const navigate = useNavigate();
  const params = useFilterParams<(typeof KEYS)[number]>(KEYS);
  const { sp, set } = params;
  const groupBy = sp.get("groupBy") === "machine" ? "machine" : "repo";
  const needsYouOnly = sp.get("needsYou") === "1";
  const asList = sp.get("view") === "list";
  const selected = desktop ? (sp.get("ticket") ?? "") : "";
  const reducedMotion = useMediaQuery(REDUCED_MOTION);

  const tickets = useAthena((s) => s.tickets);
  const threads = useAthena((s) => s.threads);
  const decisions = useAthena((s) => s.decisions);
  const repos = useAthena((s) => s.repos);
  const machine = useAthena((s) => s.node?.name ?? "");
  const needsYouItems = useAthena(selectNeedsYou);
  const needsYou = useMemo(() => new Set(needsYouItems.map((i) => i.ticket.id)), [needsYouItems]);

  const now = useClock(60_000);
  const range = useMemo(() => timeRange(tickets.values(), now), [tickets, now]);
  const replay = useReplay(range.start, range.end, reducedMotion);
  const at = replay.at;

  const model = useMemo(
    () =>
      layout(tickets.values(), threads.values(), decisions.values(), {
        groupBy,
        now: new Date(range.end),
        ...(at !== null && { at: new Date(at) }),
        needsYou,
        needsYouOnly,
        repos,
        machine,
      }),
    [tickets, threads, decisions, groupBy, range.end, at, needsYou, needsYouOnly, repos, machine],
  );

  const [hovered, setHovered] = useState<string | null>(null);
  const shown = model.nodes.find((n) => n.id === (hovered ?? selected));
  const laneTitle = model.lanes.find((l) => l.id === shown?.lane)?.title ?? "";

  const open = (id: string) => {
    if (desktop) set("ticket", id);
    else navigate(ticketRoute(id));
  };
  const close = () => set("ticket", "");
  useShortcut("Escape", close, { when: () => !!selected && !topModal() });

  const header = (
    <div className="grid min-w-0 gap-3 border-b border-line p-4 lg:px-6">
      <div className="grid gap-1">
        <h1 className="text-xl font-semibold">Graph</h1>
        <p className="max-w-prose text-muted">How Tickets connect, lane by repository, placed by when they started.</p>
      </div>
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <ToggleGroup
          type="single"
          aria-label="Lanes"
          value={groupBy}
          onValueChange={(v) => v && set("groupBy", v === "machine" ? "machine" : "")}
        >
          <ToggleGroupItem value="repo">By repository</ToggleGroupItem>
          <ToggleGroupItem value="machine">By machine</ToggleGroupItem>
        </ToggleGroup>
        <ToggleGroup
          type="multiple"
          aria-label="Filter"
          value={needsYouOnly ? ["needsYou"] : []}
          onValueChange={(v) => set("needsYou", v.includes("needsYou") ? "1" : "")}
        >
          <ToggleGroupItem value="needsYou">Needs you only</ToggleGroupItem>
        </ToggleGroup>
        <ToggleGroup
          type="single"
          aria-label="Show as"
          value={asList ? "list" : "graph"}
          onValueChange={(v) => v && set("view", v === "list" ? "list" : "")}
        >
          <ToggleGroupItem value="graph">Graph</ToggleGroupItem>
          <ToggleGroupItem value="list">List</ToggleGroupItem>
        </ToggleGroup>
      </div>
    </div>
  );

  const empty =
    tickets.size === 0 ? (
      <EmptyState
        className="mx-4 lg:mx-6"
        title="No Tickets yet"
        description="Start a task and it shows here, in its repository's lane."
        action={
          <Button asChild>
            <Link to="/new">New task</Link>
          </Button>
        }
      />
    ) : model.nodes.length === 0 ? (
      <EmptyState className="mx-4 lg:mx-6" title="Nothing needs you" description="Tickets waiting on you show here." />
    ) : null;

  const scrubber = (
    <Scrubber
      start={range.start}
      end={range.end}
      at={at}
      playing={replay.playing}
      onScrub={replay.scrub}
      onReplay={replay.toggle}
    />
  );
  const canvas = (className: string) => (
    <GraphCanvas
      // A new grouping or filter is a new picture: it starts again from the fitted view.
      key={`${groupBy}:${needsYouOnly}`}
      model={model}
      selected={selected}
      reducedMotion={reducedMotion}
      onOpen={open}
      onHover={setHovered}
      className={className}
    />
  );

  if (!desktop) {
    return (
      <div className="grid min-w-0">
        {header}
        {empty ??
          (asList ? (
            <GraphList model={model} />
          ) : (
            <>
              {/* A screen reader reads the list; the picture is for the eyes. */}
              <div data-hidden-list className="sr-only focus-within:not-sr-only">
                <GraphList model={model} />
              </div>
              <div className="h-[60dvh] min-h-80 border-b border-line">{canvas("")}</div>
            </>
          ))}
        {tickets.size > 0 && scrubber}
        {!asList && !empty && (
          <div className="border-t border-line">
            <Legend shown={shown} laneTitle={laneTitle} />
          </div>
        )}
      </div>
    );
  }

  return (
    <div className="flex h-full min-h-0 min-w-0">
      <div className="flex min-w-0 flex-1 flex-col">
        {header}
        {empty ?? (
          <div className="flex min-h-0 flex-1">
            {asList ? (
              <div className="min-w-0 flex-1 overflow-y-auto">
                <GraphList model={model} />
              </div>
            ) : (
              <>
                <div className="min-w-0 flex-1">{canvas("")}</div>
                <div className="w-[230px] shrink-0 overflow-y-auto border-l border-line">
                  <Legend shown={shown} laneTitle={laneTitle} />
                </div>
              </>
            )}
          </div>
        )}
        {tickets.size > 0 && scrubber}
      </div>
      {selected && (
        <aside aria-label="Ticket" className="flex w-[480px] shrink-0 flex-col overflow-y-auto border-l border-line">
          <div className="flex items-center justify-between gap-2 border-b border-line-soft px-4 py-2">
            <Link
              to={ticketRoute(selected)}
              className="text-sm font-medium underline decoration-line-soft underline-offset-4 hover:decoration-text"
            >
              Open in Tickets
            </Link>
            <Button variant="ghost" size="icon" aria-label="Close" aria-keyshortcuts="Escape" onClick={close}>
              <XIcon aria-hidden />
            </Button>
          </div>
          <TicketDetail key={selected} id={selected} />
        </aside>
      )}
    </div>
  );
}

