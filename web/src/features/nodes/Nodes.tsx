// The Nodes screen: one card per node that runs sessions, this machine first. A card says the node's state (a pill:
// green online, amber draining, grey offline or revoked; orange stays for what needs the owner), where it runs and its
// version, its slots in use of its slots, the queue, the live Tickets and each runtime with its version, readiness and
// quota (QuotaBars). A card's menu drains or undrains the node, and revokes one that joined (after a confirmation).
// "Add a node" opens the join command (AddNode). Under the cards, the server's health checks: failing checks say what
// is wrong and since when; the passing ones fold into one line. Before the first Snapshot both say "Waiting for the
// server.". This view is a lazy chunk (LAZY_SENTINEL).
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { ChevronRightIcon, MoreHorizontalIcon } from "lucide-react";
import { useId, useState } from "react";
import { Link } from "react-router";
import type { HealthItem, NodeStatus } from "@/gen/athena/v1/state_pb";
import { errorMessage, revokeNode, setNodeDraining } from "@/lib/commands/actions";
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { clockTime } from "@/lib/text/runtime";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { Waiting } from "@/ui/athena/Waiting";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "@/ui/primitives/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/ui/primitives/dropdown-menu";
import { toast } from "@/ui/primitives/toast";
import { AddNode } from "./AddNode";
import { QuotaBars } from "./QuotaBars";

/** bundle-budget.mjs checks this chunk stays out of the initial route. */
export const LAZY_SENTINEL = "athena-lazy-chunk:nodes-view";

export function Nodes() {
  const nodes = useAthena((s) => s.nodes);
  const health = useAthena((s) => s.health);
  const synced = useAthena(selectSynced);
  return (
    <section aria-label="Nodes" data-chunk={LAZY_SENTINEL} className="grid min-w-0 content-start">
      <div className="grid gap-1 border-b border-line p-4 lg:px-6">
        <h1 className="text-xl font-semibold">Nodes</h1>
        <p className="max-w-prose text-muted">The machines that run sessions, with their load and health.</p>
      </div>
      <div className="grid min-w-0 gap-6 p-4 lg:max-w-3xl lg:px-6">
        {synced && nodes.length > 0 ? (
          <>
            <ul className="grid min-w-0 gap-4">
              {nodes.map((n) => (
                <li key={n.id || n.name} className="min-w-0">
                  <NodeCard node={n} />
                </li>
              ))}
            </ul>
            <AddNode />
          </>
        ) : (
          <Waiting />
        )}
        <Health items={synced ? health?.items : undefined} />
      </div>
    </section>
  );
}

const STATE: Record<string, { label: string; dot: string }> = {
  online: { label: "Online", dot: "bg-success" },
  draining: { label: "Draining", dot: "bg-warning" },
  offline: { label: "Offline", dot: "bg-muted" },
  revoked: { label: "Revoked", dot: "bg-muted" },
};

/** The node's state; an older server sends none for its one node, which is online. */
const stateOf = (n: NodeStatus) => (n.state in STATE ? n.state : "online");

function StatePill({ state }: { state: string }) {
  const s = STATE[state];
  return (
    <span data-node-state={state} className="inline-flex h-5 shrink-0 items-center gap-1.5 rounded-xs border border-line-soft px-2 text-xs font-semibold">
      <span aria-hidden className={cn("size-2 rounded-full", s.dot)} />
      {s.label}
    </span>
  );
}

/** The card's menu: Drain or Undrain, and Revoke for a node that joined (this machine cannot be revoked). */
function NodeMenu({ node, name }: { node: NodeStatus; name: string }) {
  const [revoking, setRevoking] = useState(false);
  const [busy, setBusy] = useState(false);
  const draining = node.state === "draining";

  const drain = async (on: boolean) => {
    const r = await setNodeDraining(node.id, on);
    if (r.ok) toast({ title: on ? `Draining ${name}` : `${name} takes new sessions again` });
    else toast({ title: on ? "Not drained" : "Not undrained", description: r.message || errorMessage(r.code), tone: "danger" });
  };

  const revoke = async () => {
    setBusy(true);
    const r = await revokeNode(node.id);
    setBusy(false);
    setRevoking(false);
    if (r.ok) toast({ title: `${name} revoked` });
    else toast({ title: "Not revoked", description: r.message || errorMessage(r.code), tone: "danger" });
  };

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button size="icon" variant="ghost" aria-label={`Actions for ${name}`}>
            <MoreHorizontalIcon />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => void drain(!draining)}>{draining ? "Undrain" : "Drain"}</DropdownMenuItem>
          {!node.local && (
            // The Dialog stays outside the menu: the menu closing would take it along (dialog.tsx).
            <DropdownMenuItem variant="danger" onSelect={() => setRevoking(true)}>
              Revoke
            </DropdownMenuItem>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
      <Dialog open={revoking} onOpenChange={setRevoking}>
        <DialogContent>
          <DialogTitle>Revoke node</DialogTitle>
          <DialogDescription>{`Revoke ${name}? Its sessions stop, and it must join again to come back.`}</DialogDescription>
          <DialogFooter>
            <DialogClose asChild>
              <Button>Cancel</Button>
            </DialogClose>
            <Button variant="danger" disabled={busy} onClick={() => void revoke()}>
              {`Revoke ${name}`}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

const label = "font-mono text-xs tracking-wider text-muted uppercase";

function NodeCard({ node }: { node: NodeStatus }) {
  const id = useId();
  const name = node.name || "This machine";
  const used = Math.min(node.slotsInUse, node.slots);
  const state = stateOf(node);
  const seen = node.lastSeen ? timestampMs(node.lastSeen) : undefined;
  const where = [node.os && node.arch ? `${node.os}/${node.arch}` : node.os, node.version].filter(Boolean).join(" · ");
  return (
    <section aria-labelledby={id} data-node={node.id} className="grid min-w-0 gap-4 rounded-md border border-line p-4">
      <div className="grid min-w-0 gap-1">
        <div className="flex min-w-0 items-start gap-2">
          <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-2 gap-y-1">
            <h2 id={id} className="min-w-0 text-lg font-semibold break-words">
              {name}
            </h2>
            {node.local && <span className="rounded-xs border border-line px-1.5 font-mono text-xs">This machine</span>}
            <StatePill state={state} />
          </div>
          {state !== "revoked" && <NodeMenu node={node} name={name} />}
        </div>
        {where && <Mono data-volatile="where">{where}</Mono>}
        {state === "offline" && <Since text="Offline since" alone="Offline" at={seen} />}
        {state === "revoked" && <Since text="Revoked; last seen at" alone="Revoked" at={seen} />}
        {state === "draining" && <p className="text-sm text-muted">Finishing its sessions; it takes no new ones.</p>}
      </div>
      {state !== "revoked" && <Load node={node} used={used} />}
    </section>
  );
}

/** "Offline since 14:02", the time as a <time>; `alone` when it is not known. */
function Since({ text, alone, at }: { text: string; alone: string; at?: number }) {
  return (
    <p className="text-sm text-muted">
      {at === undefined ? (
        alone
      ) : (
        <>
          {`${text} `}
          <time dateTime={new Date(at).toISOString()}>{clockTime(at)}</time>
        </>
      )}
    </p>
  );
}

/** The node's slots in use, its queue, its live Tickets and its runtimes. */
function Load({ node, used }: { node: NodeStatus; used: number }) {
  return (
    <>
      <div className="grid gap-2">
        <p data-volatile="slots" className="text-sm">{`${node.slotsInUse} of ${node.slots} slots in use`}</p>
        <div
          role="meter"
          aria-label="Slots in use"
          aria-valuemin={0}
          aria-valuemax={node.slots}
          aria-valuenow={used}
          data-volatile="slots"
          className="flex h-2 gap-0.5"
        >
          {Array.from({ length: Math.min(node.slots, 64) }, (_, i) => (
            <span key={i} className={cn("flex-1", i < used ? "bg-text" : "bg-line-soft")} />
          ))}
        </div>
      </div>
      <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
        <dt className="text-muted">Queued</dt>
        <dd data-volatile="queued" className="font-mono tabular-nums">
          {node.queued}
        </dd>
        <dt className="text-muted">Live Tickets</dt>
        <dd data-volatile="live" className="min-w-0">
          {node.liveTicketIds.length === 0 ? (
            <span className="text-muted">None</span>
          ) : (
            <ul className="flex flex-wrap gap-x-3 gap-y-1">
              {node.liveTicketIds.map((t) => (
                <li key={t}>
                  <Link
                    to={`/tickets/${encodeURIComponent(t)}`}
                    className="inline-flex min-h-11 items-center font-mono underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0"
                  >
                    {t}
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </dd>
        {node.runtimes.length === 0 && (
          // An older server lists no runtimes: its Claude version is all there is.
          <>
            <dt className="text-muted">Claude version</dt>
            <dd className="min-w-0 font-mono break-words">{node.claudeVersion || "Not reported"}</dd>
          </>
        )}
      </dl>
      {node.runtimes.length > 0 && (
        <ul aria-label="Runtimes" className="grid gap-4 border-t border-line-soft pt-4">
          {node.runtimes.map((r) => (
            <li key={r.name} className="min-w-0">
              <QuotaBars status={r} />
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

function since(item: HealthItem) {
  return item.since ? <RelativeTime at={timestampMs(item.since)} /> : null;
}

function Health({ items }: { items?: HealthItem[] }) {
  const id = useId();
  const failing = items?.filter((i) => !i.ok) ?? [];
  const passing = items?.filter((i) => i.ok) ?? [];
  return (
    <section aria-labelledby={id} className="grid min-w-0 gap-2">
      <h2 id={id} className={label}>
        Health
      </h2>
      {items === undefined ? (
        <Waiting />
      ) : (
        <ul className="grid">
          {failing.map((i) => (
            <li key={i.name} className="grid min-w-0 gap-0.5 border-b border-line-soft py-2">
              <span className="flex min-w-0 flex-wrap items-baseline gap-x-2">
                <span aria-hidden className="size-2 shrink-0 self-center rounded-full bg-danger" />
                <span className="font-mono font-semibold break-words">{i.name}</span>
                {i.since && <Mono>Since {since(i)}</Mono>}
              </span>
              <span className="text-sm break-words text-danger">{i.message || "Failing"}</span>
            </li>
          ))}
          {passing.length > 0 && <Passing items={passing} all={failing.length === 0} />}
          {items.length === 0 && <li className="py-2 text-sm text-muted">No checks reported.</li>}
        </ul>
      )}
    </section>
  );
}

/** The passing checks, folded into one line that opens to list them. */
function Passing({ items, all }: { items: HealthItem[]; all: boolean }) {
  const [open, setOpen] = useState(false);
  const id = useId();
  return (
    <li className="border-b border-line-soft py-1">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={id}
        onClick={() => setOpen((o) => !o)}
        className="inline-flex min-h-11 items-center gap-1 text-sm text-text lg:min-h-8"
      >
        <ChevronRightIcon aria-hidden className={cn("size-4 transition-transform duration-(--motion-fast)", open && "rotate-90")} />
        {all ? "All checks passing" : `${items.length} other checks passing`}
      </button>
      {open && (
        <ul id={id} className="grid gap-1 pb-2 pl-5">
          {items.map((i) => (
            <li key={i.name} className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-sm">
              <span className="font-mono break-words">{i.name}</span>
              {i.message && <span className="text-muted break-words">{i.message}</span>}
            </li>
          ))}
        </ul>
      )}
    </li>
  );
}
