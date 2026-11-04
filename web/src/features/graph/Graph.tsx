// The task graph (/graph, a lazy chunk): Tickets as nodes in lanes by repository (or by machine), placed by when
// they were created, joined by the Threads they share and the follow-ups routed to them. Drawn in SVG with no graph
// library (GraphCanvas): shape and colour by stage, orange only for what needs the owner, a ring for running.
//
//  - The toolbar (By repository / By machine, Needs you only, Graph / List) is in the URL; the scrubber's position
//    is local state (Scrubber).
//  - A node opens its Ticket: in a detail pane beside the graph on the desktop (?ticket=; the legend steps aside
//    while it is open, so the graph keeps its width, and closing it puts focus back on the node), as a navigation on
//    the phone. Hover or focus shows its title and id under the legend.
//  - GraphList is the accessible fallback: a toggle shows it on any size. On the phone it is also there visually
//    hidden, and it alone is what a screen reader reads: the picture is aria-hidden there.
//  - The phone's graph view fills the screen between the top bar and the bottom navigation, the scrubber always in
//    view above the navigation, the legend folded into a disclosure. Nothing below it needs the page to scroll.
import { XIcon } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router";
import { useLayout, useMediaQuery } from "@/app/Panes";
import { ticketRoute } from "@/app/search";
import { useShortcut } from "@/app/shortcuts";
import { selectNeedsYou } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Button } from "@/ui/primitives/button";
import { topModal } from "@/ui/primitives/dialog";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";
import { TicketDetail } from "../tickets/TicketDetail";
import { useFilterParams } from "../tickets/Filters";
import { GraphCanvas } from "./GraphCanvas";
import { GraphList } from "./GraphList";
import { layout, timeRange } from "./layout";
import { Legend } from "./Legend";
import { Scrubber, snapToStep, useClock, useReplay } from "./Scrubber";

export { REPLAY_MS } from "./Scrubber";

const KEYS = ["groupBy", "needsYou", "view", "ticket"] as const;
const REDUCED_MOTION = "(prefers-reduced-motion: reduce)";

/**
 * The phone's graph view: at least the viewport less the top bar (48 px) and the bottom navigation (57 px) and their
 * safe areas (PhoneShell), so the scrubber sits just above the navigation. An open legend makes it taller, and the
 * page then scrolls from the header and the legend.
 */
const PHONE_FILL = "min-h-[calc(100dvh_-_105px_-_env(safe-area-inset-top)_-_env(safe-area-inset-bottom))]";

/** The node a closed pane should give focus back to, or the canvas when it is gone. */
function focusNode(id: string) {
  const node = [...document.querySelectorAll<SVGElement>("svg [data-node]")].find((n) => n.dataset.node === id);
  (node ?? document.querySelector<SVGElement>("svg[data-canvas]"))?.focus();
}

export function Graph() {
  const desktop = useLayout() === "desktop";
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
  // While the replay plays, `at` snaps to 1/200 of the window: frames within a step reuse one layout.
  const modelAt = replay.playing ? snapToStep(at, range.start, range.end) : at;

  const model = useMemo(
    () =>
      layout(tickets.values(), threads.values(), decisions.values(), {
        groupBy,
        now: new Date(range.end),
        ...(modelAt !== null && { at: new Date(modelAt) }),
        needsYou,
        needsYouOnly,
        repos,
        machine,
      }),
    [tickets, threads, decisions, groupBy, range.end, modelAt, needsYou, needsYouOnly, repos, machine],
  );

  const [hovered, setHovered] = useState<string | null>(null);
  const shown = model.nodes.find((n) => n.id === (hovered ?? selected));
  const laneTitle = model.lanes.find((l) => l.id === shown?.lane)?.title ?? "";

  // Closing the pane gives focus back to the node that opened it.
  const opener = useRef("");
  const refocus = useRef(false);
  const open = (id: string) => {
    if (!desktop) return navigate(ticketRoute(id));
    opener.current = id;
    set("ticket", id);
  };
  const close = () => {
    refocus.current = true;
    opener.current ||= selected;
    set("ticket", "");
  };
  useEffect(() => {
    if (selected || !refocus.current) return;
    refocus.current = false;
    focusNode(opener.current);
  }, [selected]);
  useShortcut("Escape", close, { when: () => !!selected && !topModal() });

  const header = (
    <div className="grid min-w-0 shrink-0 gap-3 border-b border-line p-4 lg:px-6">
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
  const canvas = (hidden: boolean, className?: string) => (
    <GraphCanvas
      // A new grouping or filter is a new picture: it starts again from the fitted view.
      key={`${groupBy}:${needsYouOnly}`}
      model={model}
      selected={selected}
      reducedMotion={reducedMotion}
      phone={!desktop}
      hidden={hidden}
      onOpen={open}
      onHover={setHovered}
      className={className}
    />
  );

  if (!desktop) {
    if (empty || asList) {
      return (
        <div className="grid min-w-0">
          {header}
          {empty ?? <GraphList model={model} />}
          {tickets.size > 0 && scrubber}
        </div>
      );
    }
    return (
      <div className={`flex min-w-0 flex-col ${PHONE_FILL}`}>
        {header}
        {/* What a screen reader reads (the picture is aria-hidden here); shown when a keyboard reaches it. Left out
            while the replay plays, which redraws it every step. */}
        {!replay.playing && (
          <div data-hidden-list className="sr-only focus-within:not-sr-only">
            <GraphList model={model} />
          </div>
        )}
        {/* Absolute: the column's height is a minimum, so a percentage height would not resolve. */}
        <div className="relative min-h-60 flex-1">{canvas(true, "absolute inset-0")}</div>
        <div className="shrink-0">{scrubber}</div>
        <details className="group shrink-0 border-t border-line">
          <summary className="flex min-h-11 cursor-pointer items-center px-4 font-mono text-xs tracking-wider text-muted uppercase">
            Legend
          </summary>
          <Legend shown={shown} laneTitle={laneTitle} />
        </details>
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
                <div className="min-w-0 flex-1">{canvas(false)}</div>
                {/* The pane shows the Ticket in full: the legend steps aside so the graph keeps its width. */}
                {!selected && (
                  <div className="w-[230px] shrink-0 overflow-y-auto border-l border-line">
                    <Legend shown={shown} laneTitle={laneTitle} />
                  </div>
                )}
              </>
            )}
          </div>
        )}
        {tickets.size > 0 && scrubber}
      </div>
      {selected && (
        // At most 36 % of the screen: at 1024 px the graph keeps about 400 px beside the sidebar.
        <aside
          aria-label="Ticket"
          className="flex w-[min(480px,36vw)] shrink-0 flex-col overflow-y-auto border-l border-line"
        >
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
