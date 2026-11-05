// The signed-in app's routes and its navigation. Graph (app/routes/graph.tsx) and Settings are lazy chunks, and
// Settings › Prompts a further one. /design and /enroll render outside the session gate (App.tsx).
//
// Query contracts the palette already uses: /tickets/:id?tab=session opens a Ticket's live session, and
// /routing?tab=triggers&trigger=<id> and /routing?tab=threads&thread=<key> open one record (Task 14). On the Ticket
// views ?pin=T-1,T-2 pins sessions under the panes (1440 px and up). /settings/<section> opens one Settings section
// (general, github, notifications, devices, prompts, execution, system); /settings/prompts?part=<name> one prompt part.
import {
  InboxIcon,
  ListIcon,
  type LucideIcon,
  PlusIcon,
  RouteIcon,
  ServerIcon,
  SettingsIcon,
  WaypointsIcon,
} from "lucide-react";
import { lazy, type ReactNode, Suspense } from "react";
import { Link, matchPath, Route, Routes, useParams } from "react-router";
import { Panes, useLayout } from "@/app/Panes";
import { NeedsYou } from "@/features/needs-you/NeedsYou";
import { NewTask } from "@/features/new-task/NewTask";
import { Routing } from "@/features/routing/Routing";
import { PinnedSessions } from "@/features/session/PinnedSessions";
import { Nodes } from "@/features/nodes/Nodes";
import { TicketDetail } from "@/features/tickets/TicketDetail";
import { TicketList } from "@/features/tickets/TicketList";
import { selectCounts, selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Waiting } from "@/ui/athena/Waiting";

export type NavItem = {
  to: string;
  label: string;
  icon: LucideIcon;
  /** In the phone's bottom navigation (Routing and Nodes are reached from Settings › System and the palette). */
  phone: boolean;
  /** Matches only the path itself, not the paths under it. */
  end?: boolean;
};

export const NAV: readonly NavItem[] = [
  { to: "/", label: "Needs you", icon: InboxIcon, phone: true, end: true },
  { to: "/tickets", label: "Tickets", icon: ListIcon, phone: true },
  { to: "/new", label: "New", icon: PlusIcon, phone: true },
  { to: "/graph", label: "Graph", icon: WaypointsIcon, phone: true },
  { to: "/routing", label: "Routing", icon: RouteIcon, phone: false },
  { to: "/nodes", label: "Nodes", icon: ServerIcon, phone: false },
  { to: "/settings", label: "Settings", icon: SettingsIcon, phone: true },
];

/** The count a nav item shows: the Needs you list's length (approvals answered here leave it at once, R30). */
export function useNavCount(item: NavItem): number {
  const needsYou = useAthena((s) => selectCounts(s).needsYou);
  const synced = useAthena(selectSynced);
  // Before the first Snapshot the count is unknown: no badge rather than a false one.
  return item.to === "/" && synced ? needsYou : 0;
}

/** The nav item a path belongs to, if any. */
export function navItemFor(pathname: string): NavItem | undefined {
  return NAV.find((n) => matchPath({ path: n.to, end: n.end ?? false }, pathname));
}

/** A view that is not built yet: its name and what it will hold, in one sentence. */
export function Placeholder({ title, text, children }: { title: string; text: string; children?: ReactNode }) {
  return (
    <section className="grid min-w-0 content-start gap-2 p-4 lg:p-6">
      <h1 className="text-xl font-semibold break-words">{title}</h1>
      <p className="max-w-prose text-muted">{text}</p>
      {children}
    </section>
  );
}

function NothingSelected({ what }: { what: string }) {
  return (
    <div className="p-4 lg:p-6">
      <EmptyState title={`No ${what} selected`} description="Pick one from the list to see it here." />
    </div>
  );
}

function NeedsYouView() {
  return <Panes detail={<NeedsYou />} />;
}

/** The Ticket panes; on the desktop the pinned sessions (1440 px and up) sit under them. */
function TicketsView() {
  const { id } = useParams();
  const layout = useLayout();
  const panes = (
    <Panes
      list={<TicketList selectedId={id} />}
      detail={id ? <TicketDetail key={id} id={id} /> : <NothingSelected what="Ticket" />}
      showDetail={id !== undefined}
    />
  );
  if (layout === "phone") return panes;
  return (
    <div className="flex min-w-0 flex-1 flex-col">
      <div className="flex min-h-0 flex-1">{panes}</div>
      <PinnedSessions />
    </div>
  );
}

function NewView() {
  return <Panes detail={<NewTask />} />;
}

const GraphView = lazy(() => import("@/app/routes/graph"));
/** Settings is a lazy chunk too (Prompts a further one inside it): only routes.tsx imports features/settings. */
const SettingsView = lazy(() => import("@/features/settings/Settings").then((m) => ({ default: m.Settings })));

function RoutingView() {
  return <Panes detail={<Routing />} />;
}

function NodesView() {
  return <Panes detail={<Nodes />} />;
}

const linkClass =
  "inline-flex min-h-11 items-center font-semibold underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0";

function NotFound() {
  return (
    <Panes
      detail={
        <Placeholder title="Page not found" text="There is nothing at this address.">
          <p>
            <Link to="/" className={linkClass}>
              Go to Needs you
            </Link>
          </p>
        </Placeholder>
      }
    />
  );
}

/** The signed-in routes, rendered inside a shell. */
export function AppRoutes() {
  return (
    <Routes>
      <Route index element={<NeedsYouView />} />
      <Route path="tickets" element={<TicketsView />} />
      <Route path="tickets/:id" element={<TicketsView />} />
      <Route path="new" element={<NewView />} />
      <Route
        path="graph"
        element={
          <Suspense fallback={null}>
            <GraphView />
          </Suspense>
        }
      />
      <Route path="routing" element={<RoutingView />} />
      <Route path="nodes" element={<NodesView />} />
      <Route
        path="settings/:group?"
        element={
          <Suspense fallback={<Panes detail={<Waiting className="p-4 lg:p-6" />} />}>
            <SettingsView />
          </Suspense>
        }
      />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
