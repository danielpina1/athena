// The Settings screen: /settings/<section>. On the desktop the section list sits in the list pane and the section
// beside it (General when none is named); on the phone the list comes first and a section opens on tap, with a link
// back. A section with unsaved changes asks before the owner follows a link away from it (any in-app link: the
// section list, the navigation), before the palette or a g shortcut navigates (guardedNavigate) and before the page
// unloads. Browser Back is not asked about: the router has already moved when it hears of it. The whole screen is a
// lazy chunk (routes.tsx), and Prompts a further one inside it.
import { lazy, type ReactNode, Suspense, useCallback, useEffect, useMemo, useState } from "react";
import { Link, Navigate, useLocation, useNavigate, useParams } from "react-router";
import { setNavigationGuard } from "@/app/guardedNavigate";
import { Panes, useLayout } from "@/app/Panes";
import { Waiting } from "@/ui/athena/Waiting";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/ui/primitives/dialog";
import { Devices } from "./Devices";
import { Execution } from "./Execution";
import { Notifications } from "./Notifications";
import { GeneralSettings, GitHubSettings } from "./PolicyForm";
import { UnsavedProvider } from "./SaveBar";
import { System } from "./System";

const Prompts = lazy(() => import("./Prompts"));

export const SECTIONS = [
  { id: "general", label: "General" },
  { id: "github", label: "Sources › GitHub" },
  { id: "notifications", label: "Notifications" },
  { id: "devices", label: "Devices" },
  { id: "prompts", label: "Prompts" },
  { id: "execution", label: "Execution" },
  { id: "system", label: "System" },
] as const;
type SectionId = (typeof SECTIONS)[number]["id"];

function SectionView({ id }: { id: SectionId }) {
  switch (id) {
    case "general":
      return <GeneralSettings />;
    case "github":
      return <GitHubSettings />;
    case "notifications":
      return <Notifications />;
    case "devices":
      return <Devices />;
    case "prompts":
      return (
        <Suspense fallback={<Waiting className="p-4 lg:p-6" />}>
          <Prompts />
        </Suspense>
      );
    case "execution":
      return <Execution />;
    case "system":
      return <System />;
  }
}

function SectionList({ selected }: { selected?: SectionId }) {
  return (
    <div className="grid min-w-0 content-start">
      <div className="grid gap-1 border-b border-line p-4">
        <h1 className="text-xl font-semibold">Settings</h1>
        <p className="max-w-prose text-muted">How Athena works, and this device.</p>
      </div>
      <nav aria-label="Settings sections">
        <ul>
          {SECTIONS.map((s) => (
            <li key={s.id}>
              <Link
                to={`/settings/${s.id}`}
                aria-current={s.id === selected ? "page" : undefined}
                className={cn(
                  "flex min-h-11 items-center border-b border-l-[3px] border-b-line-soft border-l-transparent px-4 py-2 font-semibold text-text hover:bg-surface-2",
                  s.id === selected && "border-l-text bg-surface-2",
                )}
              >
                {s.label}
              </Link>
            </li>
          ))}
        </ul>
      </nav>
    </div>
  );
}

/** Asks before leaving while a block has changes: in-app links (caught before the router sees them) and unloads. */
function UnsavedGuard({ children }: { children: ReactNode }) {
  const [dirty, setDirty] = useState<ReadonlyMap<string, string>>(new Map());
  const [leaving, setLeaving] = useState<string | null>(null);
  const navigate = useNavigate();
  const location = useLocation();

  const registry = useMemo(
    () => ({
      set(id: string, label: string, isDirty: boolean) {
        setDirty((m) => {
          if (isDirty === m.has(id)) return m;
          const next = new Map(m);
          if (isDirty) next.set(id, label);
          else next.delete(id);
          return next;
        });
      },
    }),
    [],
  );

  const here = location.pathname + location.search;
  useEffect(() => {
    if (dirty.size === 0) return;
    const onClick = (e: MouseEvent) => {
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      const a = (e.target as Element | null)?.closest?.("a[href]");
      if (!(a instanceof HTMLAnchorElement) || a.target === "_blank" || a.hasAttribute("download")) return;
      const url = new URL(a.href, window.location.href);
      if (url.origin !== window.location.origin) return;
      const to = url.pathname + url.search + url.hash;
      if (to === here) return;
      e.preventDefault();
      e.stopPropagation();
      setLeaving(to);
    };
    const onUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
    };
    // The palette and the g shortcuts navigate in code: they ask through the same dialog.
    const offGuard = setNavigationGuard((to) => {
      if (to === here) return false;
      setLeaving(to);
      return true;
    });
    document.addEventListener("click", onClick, true);
    window.addEventListener("beforeunload", onUnload);
    return () => {
      offGuard();
      document.removeEventListener("click", onClick, true);
      window.removeEventListener("beforeunload", onUnload);
    };
  }, [dirty.size, here]);

  const stay = useCallback(() => setLeaving(null), []);
  const leave = () => {
    const to = leaving;
    setLeaving(null);
    // The blocks unmount with their drafts; the guard must not ask again on the way out.
    setDirty(new Map());
    if (to) navigate(to);
  };
  const what = [...new Set(dirty.values())].join(", ");

  return (
    <UnsavedProvider value={registry}>
      {children}
      <Dialog open={leaving !== null} onOpenChange={(o) => !o && stay()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Discard unsaved changes?</DialogTitle>
            <DialogDescription>{`Your changes to ${what || "this section"} are not saved.`}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={stay}>
              Keep editing
            </Button>
            <Button variant="danger" onClick={leave}>
              Discard changes
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </UnsavedProvider>
  );
}

export function Settings() {
  const { group } = useParams();
  const layout = useLayout();
  const known = SECTIONS.find((s) => s.id === group)?.id;
  if (group !== undefined && !known) return <Navigate to="/settings" replace />;
  // The desktop always shows a section: General until another is picked.
  const shown: SectionId | undefined = known ?? (layout === "desktop" ? "general" : undefined);
  return (
    <UnsavedGuard>
      <Panes
        list={<SectionList selected={shown} />}
        detail={shown ? <SectionView key={shown} id={shown} /> : null}
        showDetail={shown !== undefined}
      />
    </UnsavedGuard>
  );
}
