// The desktop sidebar (240 px): Athena's mark and name and the palette key, the navigation, the node with its live
// load, the live feed and today's counts. Every count is the length of the list it stands for (selectCounts).
import { useState } from "react";
import { NavLink } from "react-router";
import { ConnectionDot, type LinkSource } from "@/app/ConnectionBanner";
import { NAV, type NavItem, useNavCount } from "@/app/routes";
import { modKeyLabel } from "@/app/shortcuts";
import { selectCounts, selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { LiveFeed } from "@/features/activity/LiveFeed";
import { AthenaMark } from "@/ui/athena/AthenaMark";
import { Kbd } from "@/ui/athena/Kbd";
import { Waiting } from "@/ui/athena/Waiting";
import { cn } from "@/ui/cn";

/**
 * How many times n has risen since the store first synced: the badge remounts on each, so its pop plays once. The
 * first render, the count the first Snapshot brings and a fall do not count.
 */
function useRises(n: number): number {
  const synced = useAthena(selectSynced);
  const [seen, setSeen] = useState({ n, synced, rises: 0 });
  if (seen.n !== n || seen.synced !== synced) {
    const rises = seen.synced && synced && n > seen.n ? seen.rises + 1 : seen.rises;
    setSeen({ n, synced, rises });
    return rises;
  }
  return seen.rises;
}

/** A count that needs the owner: orange fill, ink text. Hidden at zero. It pops briefly when it goes up. */
export function NeedsCount({ n, className }: { n: number; className?: string }) {
  const rises = useRises(n);
  if (n <= 0) return null;
  return (
    <span
      key={rises}
      className={cn(
        "inline-block bg-accent px-1.5 font-mono text-xs font-semibold text-accent-fg tabular-nums",
        rises > 0 && "motion-pop",
        className,
      )}
    >
      {n}
    </span>
  );
}

function SideLink({ item }: { item: NavItem }) {
  const count = useNavCount(item);
  const Icon = item.icon;
  return (
    <li>
      <NavLink
        to={item.to}
        end={item.end}
        className={cn(
          "flex h-9 items-center gap-2 rounded-xs px-2 text-body text-text hover:bg-surface-2",
          "aria-[current=page]:bg-text aria-[current=page]:font-semibold aria-[current=page]:text-bg",
        )}
      >
        <Icon className="size-4 shrink-0" aria-hidden />
        <span className="flex-1">{item.label}</span>
        <NeedsCount n={count} />
      </NavLink>
    </li>
  );
}

const label = "px-4 pt-4 pb-1 font-mono text-xs tracking-wider text-muted uppercase";

function NodeCard() {
  const node = useAthena((s) => s.node);
  const synced = useAthena(selectSynced);
  return (
    <section aria-labelledby="sidebar-node" className="grid gap-1">
      <h2 id="sidebar-node" className={label}>
        Node
      </h2>
      {node ? (
        <div className="grid gap-1.5 px-4 text-sm">
          <div className="flex items-baseline justify-between gap-2">
            <span className="min-w-0 truncate font-semibold">{node.name || "This machine"}</span>
            <span className="font-mono text-xs tabular-nums">
              <span className="sr-only">Slots in use: </span>
              <span>{`${node.slotsInUse}/${node.slots}`}</span>
            </span>
          </div>
          {/* One segment per slot, filled while in use. */}
          <div aria-hidden className="flex h-1 gap-0.5">
            {Array.from({ length: Math.min(node.slots, 32) }, (_, i) => (
              <span key={i} className={cn("flex-1", i < node.slotsInUse ? "bg-text" : "bg-line-soft")} />
            ))}
          </div>
          <dl className="grid grid-cols-[1fr_auto] gap-x-2 font-mono text-xs text-muted">
            <dt>Queued</dt>
            <dd data-testid="queued" className="tabular-nums">
              {node.queued}
            </dd>
            <dt>Live Tickets</dt>
            <dd data-testid="live" className="tabular-nums">
              {node.liveTicketIds.length}
            </dd>
          </dl>
        </div>
      ) : synced ? (
        <p className="px-4 text-sm text-muted">Waiting for the node.</p>
      ) : (
        <Waiting className="px-4" />
      )}
    </section>
  );
}

function Today() {
  const merged = useAthena((s) => selectCounts(s).mergedToday);
  const open = useAthena((s) => selectCounts(s).open);
  const synced = useAthena(selectSynced);
  return (
    <section aria-labelledby="sidebar-today" className="grid gap-1 border-t border-line-soft pb-4">
      <h2 id="sidebar-today" className={label}>
        Today
      </h2>
      {!synced ? (
        <Waiting className="px-4" />
      ) : (
        <dl className="grid grid-cols-[1fr_auto] gap-x-2 px-4 font-mono text-xs text-muted">
          <dt>Merged</dt>
          <dd data-testid="merged" className="tabular-nums">
            {merged}
          </dd>
          <dt>Open</dt>
          <dd data-testid="open" className="tabular-nums">
            {open}
          </dd>
        </dl>
      )}
    </section>
  );
}

export function Sidebar({ link, onOpenPalette }: { link: LinkSource; onOpenPalette: () => void }) {
  return (
    <aside className="flex h-dvh w-[240px] shrink-0 flex-col overflow-y-auto border-r border-line">
      <div className="flex h-14 items-center justify-between gap-2 border-b border-line px-4">
        <span className="flex items-center gap-2 font-semibold">
          <AthenaMark />
          Athena
          <ConnectionDot link={link} />
        </span>
        <button
          type="button"
          onClick={onOpenPalette}
          className="inline-flex h-8 items-center rounded-xs px-1 hover:bg-surface-2"
        >
          <span className="sr-only">Command palette</span>
          <Kbd>{modKeyLabel("K")}</Kbd>
        </button>
      </div>
      <nav aria-label="Main" className="px-2 py-2">
        <ul className="grid gap-px">
          {NAV.map((item) => (
            <SideLink key={item.to} item={item} />
          ))}
        </ul>
      </nav>
      <NodeCard />
      <LiveFeed className="mt-4 min-h-40 flex-1 overflow-y-auto border-t border-line-soft px-4 py-4" />
      <Today />
    </aside>
  );
}
