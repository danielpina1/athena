// The signed-in app's routes and its navigation. The views here are placeholders (a heading and one sentence) until
// the task that builds each replaces it: Tickets (11), New (13), Routing (14), Graph (15),
// Nodes and Settings (17). /design and /enroll render outside the session gate (App.tsx).
//
// Query contracts the palette already uses: /tickets/:id?tab=session opens a Ticket's live session (Task 12), and
// /routing?tab=triggers&trigger=<id> and /routing?tab=threads&thread=<key> open one record (Task 14).
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
import { Panes } from "@/app/Panes";
import { NeedsYou } from "@/features/needs-you/NeedsYou";
import { selectCounts } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { EmptyState } from "@/ui/athena/EmptyState";

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
  return item.to === "/" ? needsYou : 0;
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

function TicketsView() {
  const { id } = useParams();
  return (
    <Panes
      list={<Placeholder title="Tickets" text="Every Ticket, open and ended, most recently changed first." />}
      detail={
        id ? (
          <Placeholder title={`Ticket ${id}`} text="Its timeline, live session and changes." />
        ) : (
          <NothingSelected what="Ticket" />
        )
      }
      showDetail={id !== undefined}
    />
  );
}

function NewView() {
  return <Panes detail={<Placeholder title="New task" text="Describe the work and Athena opens a Ticket for it." />} />;
}

const GraphView = lazy(() => import("@/app/routes/graph"));

function RoutingView() {
  return (
    <Panes
      list={<Placeholder title="Routing" text="Triggers, Threads and Decisions: how incoming work was routed." />}
      detail={<NothingSelected what="record" />}
    />
  );
}

function NodesView() {
  return (
    <Panes
      list={<Placeholder title="Nodes" text="The machines that run sessions, with their load." />}
      detail={<NothingSelected what="node" />}
    />
  );
}

const linkClass =
  "inline-flex min-h-11 items-center font-semibold underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0";

function SettingsView() {
  return (
    <Panes
      detail={
        <Placeholder title="Settings" text="Policy, notifications, prompts and this device.">
          <section aria-labelledby="settings-system" className="mt-4 grid gap-1 border-t border-line pt-4">
            <h2 id="settings-system" className="font-mono text-xs text-muted uppercase">
              System
            </h2>
            <ul className="grid">
              <li>
                <Link to="/routing" className={linkClass}>
                  Routing
                </Link>
              </li>
              <li>
                <Link to="/nodes" className={linkClass}>
                  Nodes
                </Link>
              </li>
            </ul>
          </section>
        </Placeholder>
      }
    />
  );
}

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
      <Route path="settings/:group?" element={<SettingsView />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
