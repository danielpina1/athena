// The Routing screen: how incoming work was routed. Triggers (what arrived and what became of it), Threads (the
// conversations Athena follows) and Decisions (what Eval decided). The filters of each tab live in the URL, and a tab's
// count is the length of the list it shows after its filters; before the first Snapshot there is no count, only
// "Waiting for the server.". /routing?tab=threads&thread=<key> opens one record.
import { useMemo } from "react";
import { Navigate } from "react-router";
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { Mono } from "@/ui/athena/Mono";
import { Waiting } from "@/ui/athena/Waiting";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/ui/primitives/tabs";
import { useFilterParams } from "../tickets/Filters";
import { Decisions, selectDecisions } from "./Decisions";
import { canReevaluate, indexDecisions } from "./outcome";
import { selectThreads, Threads } from "./Threads";
import { selectTriggers, Triggers } from "./Triggers";

const TABS = ["triggers", "threads", "decisions"] as const;
type Tab = (typeof TABS)[number];

/** Every parameter a tab's filters use: all three tabs' counts follow their own filters, so all stay in the URL. */
const FILTER_KEYS = ["outcome", "tq", "state", "hq", "decision"] as const;
export type RoutingKey = (typeof FILTER_KEYS)[number] | "tab";

export function Routing() {
  const params = useFilterParams<RoutingKey>(FILTER_KEYS);
  const { sp, set } = params;
  const tab: Tab = TABS.find((t) => t === sp.get("tab")) ?? "triggers";

  const triggers = useAthena((s) => s.triggers);
  const threads = useAthena((s) => s.threads);
  const decisions = useAthena((s) => s.decisions);
  const synced = useAthena(selectSynced);

  const triggerRows = useMemo(
    () => selectTriggers(triggers, { outcome: sp.get("outcome") ?? "", q: sp.get("tq") ?? "" }),
    [triggers, sp],
  );
  const threadRows = useMemo(() => selectThreads(threads, { state: sp.get("state") ?? "", q: sp.get("hq") ?? "" }), [threads, sp]);
  // Looked up per row by canReevaluate: indexed once per change of the Decisions.
  const judged = useMemo(() => indexDecisions(decisions.values()), [decisions]);
  const decisionRows = useMemo(() => selectDecisions(decisions, { decision: sp.get("decision") ?? "" }), [decisions, sp]);

  // /routing?q=schedule:<slug> (Settings › Autopilot's "See its runs") is the Triggers search for that text.
  if (sp.has("q") && !sp.has("tq")) {
    const next = new URLSearchParams(sp);
    next.set("tq", next.get("q") ?? "");
    next.delete("q");
    return <Navigate to={{ search: `?${next.toString().replace(/%3A/gi, ":")}` }} replace />;
  }

  return (
    <section aria-label="Routing" className="min-w-0">
      <div className="grid gap-1 border-b border-line p-4">
        <h1 className="text-xl font-semibold">Routing</h1>
        <p className="max-w-prose text-muted">How incoming messages were routed to Tickets.</p>
      </div>
      <Tabs value={tab} onValueChange={(v) => set("tab", v)}>
        <TabsList aria-label="Routing records" className="px-2">
          <TabsTrigger value="triggers">
            Triggers {synced && <Mono>{triggerRows.length}</Mono>}
          </TabsTrigger>
          <TabsTrigger value="threads">
            Threads {synced && <Mono>{threadRows.length}</Mono>}
          </TabsTrigger>
          <TabsTrigger value="decisions">
            Decisions {synced && <Mono>{decisionRows.length}</Mono>}
          </TabsTrigger>
        </TabsList>
        <TabsContent value="triggers" className="pt-0">
          {synced ? (
            <Triggers
              params={params}
              rows={triggerRows}
              total={triggers.size}
              selected={sp.get("trigger") ?? undefined}
              again={(t) => canReevaluate(t, { threads, judged })}
            />
          ) : (
            <Waiting className="px-4 py-8" />
          )}
        </TabsContent>
        <TabsContent value="threads" className="pt-0">
          {synced ? (
            <Threads params={params} rows={threadRows} total={threads.size} selected={sp.get("thread") ?? undefined} />
          ) : (
            <Waiting className="px-4 py-8" />
          )}
        </TabsContent>
        <TabsContent value="decisions" className="pt-0">
          {synced ? (
            <Decisions params={params} rows={decisionRows} total={decisions.size} />
          ) : (
            <Waiting className="px-4 py-8" />
          )}
        </TabsContent>
      </Tabs>
    </section>
  );
}
