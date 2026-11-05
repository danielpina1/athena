// The Nodes screen: the node that runs sessions (its slots in use of its slots, the queue, the live Tickets and the
// Claude version) and the server's health checks. Failing checks say what is wrong and since when; the passing ones
// fold into one line. Before the first Snapshot both say "Waiting for the server.".
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { ChevronRightIcon } from "lucide-react";
import { useId, useState } from "react";
import { Link } from "react-router";
import type { HealthItem, NodeStatus } from "@/gen/athena/v1/state_pb";
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { Waiting } from "@/ui/athena/Waiting";
import { cn } from "@/ui/cn";

export function Nodes() {
  const node = useAthena((s) => s.node);
  const health = useAthena((s) => s.health);
  const synced = useAthena(selectSynced);
  return (
    <section aria-label="Nodes" className="grid min-w-0 content-start">
      <div className="grid gap-1 border-b border-line p-4 lg:px-6">
        <h1 className="text-xl font-semibold">Nodes</h1>
        <p className="max-w-prose text-muted">The machine that runs sessions, with its load and health.</p>
      </div>
      <div className="grid min-w-0 gap-6 p-4 lg:max-w-3xl lg:px-6">
        {synced && node ? <NodeCard node={node} /> : <Waiting />}
        <Health items={synced ? health?.items : undefined} />
      </div>
    </section>
  );
}

const label = "font-mono text-xs tracking-wider text-muted uppercase";

function NodeCard({ node }: { node: NodeStatus }) {
  const id = useId();
  const name = node.name || "This machine";
  const used = Math.min(node.slotsInUse, node.slots);
  return (
    <section aria-labelledby={id} className="grid min-w-0 gap-4 rounded-md border border-line p-4">
      <h2 id={id} className="text-lg font-semibold break-words">
        {name}
      </h2>
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
        <dt className="text-muted">Claude version</dt>
        <dd className="min-w-0 font-mono break-words">{node.claudeVersion || "Not reported"}</dd>
      </dl>
    </section>
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
