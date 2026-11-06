// The desktop shell (1024 px and up): the sidebar, then the view's panes (Panes: the 400 px list and the detail, or
// one pane across both), under the connection banner when the link is down.
import type { ReactNode } from "react";
import { ConnectionBanner, type LinkSource } from "@/app/ConnectionBanner";
import { UpdateBanner } from "@/app/UpdateBanner";
import { PausedBanner } from "@/features/control/PausedBanner";
import { Sidebar } from "@/app/Sidebar";

export function DesktopShell({
  link,
  onOpenPalette,
  children,
}: {
  link: LinkSource;
  onOpenPalette: () => void;
  children: ReactNode;
}) {
  return (
    <div className="flex h-dvh overflow-hidden">
      <Sidebar link={link} onOpenPalette={onOpenPalette} />
      <div className="flex min-w-0 flex-1 flex-col">
        <ConnectionBanner link={link} />
        <UpdateBanner />
        <PausedBanner />
        <main className="flex min-h-0 flex-1">{children}</main>
      </div>
    </div>
  );
}
