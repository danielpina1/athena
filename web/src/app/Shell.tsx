// The signed-in app: the phone or desktop shell around the routes, the command palette, the shortcut list, and the
// keys that work everywhere (⌘K, ?, g n, g t, g g).
import { type ReactNode, useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router";
import type { LinkSource } from "@/app/ConnectionBanner";
import { DesktopShell } from "@/app/DesktopShell";
import { guardedNavigate } from "@/app/guardedNavigate";
import { Palette } from "@/app/Palette";
import { LayoutProvider, useIsDesktop } from "@/app/Panes";
import { PhoneShell } from "@/app/PhoneShell";
import { AppRoutes, navItemFor } from "@/app/routes";
import { SHORTCUTS, useShortcut } from "@/app/shortcuts";
import { Kbd } from "@/ui/athena/Kbd";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/ui/primitives/dialog";

function ShortcutList({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Keyboard shortcuts</DialogTitle>
          <DialogDescription>Single keys do nothing while you type in a field.</DialogDescription>
        </DialogHeader>
        <dl className="grid grid-cols-[auto_1fr] items-center gap-x-4 gap-y-2 text-sm">
          {SHORTCUTS.map((s) => (
            <div key={s.keys} className="contents">
              <dt>
                <Kbd className="text-text">{s.keys}</Kbd>
              </dt>
              <dd>{s.action}</dd>
            </div>
          ))}
        </dl>
      </DialogContent>
    </Dialog>
  );
}

/** Keeps the tab's title on the current section. */
function useDocumentTitle() {
  const { pathname } = useLocation();
  useEffect(() => {
    const item = navItemFor(pathname);
    document.title = item ? `${item.label} · Athena` : "Athena";
  }, [pathname]);
}

export function Shell({ link }: { link: LinkSource }) {
  const desktop = useIsDesktop();
  const navigate = useNavigate();
  const [palette, setPalette] = useState(false);
  const [help, setHelp] = useState(false);
  useDocumentTitle();

  useShortcut("mod+k", () => setPalette(true));
  useShortcut("?", () => setHelp(true));
  // "/" searches: it focuses the page's own list search where there is one (Tickets, Routing), else opens the palette.
  // The page's search box is found in the DOM rather than registered: a parent's shortcut registers after its
  // children's and would win over theirs.
  useShortcut("/", () => {
    const box = document.querySelector<HTMLInputElement>("input[data-list-search]");
    if (box) box.focus();
    else setPalette(true);
  });
  useShortcut("g n", () => guardedNavigate(navigate, "/"));
  useShortcut("g t", () => guardedNavigate(navigate, "/tickets"));
  useShortcut("g g", () => guardedNavigate(navigate, "/graph"));

  const openPalette = () => setPalette(true);
  const content: ReactNode = <AppRoutes />;
  return (
    <LayoutProvider value={desktop ? "desktop" : "phone"}>
      {desktop ? (
        <DesktopShell link={link} onOpenPalette={openPalette}>
          {content}
        </DesktopShell>
      ) : (
        <PhoneShell link={link} onOpenPalette={openPalette}>
          {content}
        </PhoneShell>
      )}
      <Palette open={palette} onOpenChange={setPalette} />
      <ShortcutList open={help} onOpenChange={setHelp} />
    </LayoutProvider>
  );
}
