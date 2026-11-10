// Settings › Execution, read-only: what each node runs sessions with, one block per node (this machine first and
// marked): its state when it is not online, where it runs, its slots and each runtime's version (a runtime that
// reports none has no row; "untested" when Athena was not verified against it). Slots and the default model are flags
// of `athena server` (internal/cli) and a node's slots of `athena node run`, not settings the dashboard can change, so
// this says where to change them.
import type { NodeStatus } from "@/gen/athena/v1/state_pb";
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { runtimeLabel } from "@/lib/text/runtime";
import { Waiting } from "@/ui/athena/Waiting";
import { Block, Section } from "./Section";

const STATE: Record<string, string> = { offline: "Offline", draining: "Draining", revoked: "Revoked" };

export function Execution() {
  const nodes = useAthena((s) => s.nodes);
  const synced = useAthena(selectSynced);
  const many = nodes.length > 1;
  return (
    <Section title="Execution" text={many ? "What each node runs sessions with." : "What this node runs sessions with."}>
      {!synced || nodes.length === 0 ? (
        <Block title="This node">
          <Waiting />
        </Block>
      ) : (
        nodes.map((n) => (
          <Block key={n.id || n.name} title={many ? `${n.name || "This machine"}${n.local ? " · this machine" : ""}` : "This node"}>
            <NodeFacts node={n} />
          </Block>
        ))
      )}
      <Block title="Where to change these">
        <p className="max-w-prose text-sm">
          Slots and the default model are set when the server starts:{" "}
          <code className="font-mono text-xs break-words">athena server --slots 4 --model sonnet</code>. Restart the
          server to apply a change.
        </p>
        {many && (
          <p className="max-w-prose text-sm">
            Another node's slots are set when it runs:{" "}
            <code className="font-mono text-xs break-words">athena node run --slots 2</code>. Restart it to apply a
            change.
          </p>
        )}
      </Block>
    </Section>
  );
}

function NodeFacts({ node }: { node: NodeStatus }) {
  const where = [node.os && node.arch ? `${node.os}/${node.arch}` : node.os, node.version].filter(Boolean).join(" · ");
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
      <dt className="text-muted">Node</dt>
      <dd className="min-w-0 font-mono break-words">{node.name || "This machine"}</dd>
      {STATE[node.state] && (
        <>
          <dt className="text-muted">State</dt>
          <dd>{STATE[node.state]}</dd>
        </>
      )}
      {where && (
        <>
          <dt className="text-muted">Runs on</dt>
          <dd className="min-w-0 font-mono break-words">{where}</dd>
        </>
      )}
      <dt className="text-muted">Slots</dt>
      <dd className="font-mono tabular-nums">{node.slots}</dd>
      {node.runtimes.length === 0
        ? node.claudeVersion && (
            <>
              <dt className="text-muted">Claude version</dt>
              <dd className="min-w-0 font-mono break-words">{node.claudeVersion}</dd>
            </>
          )
        : node.runtimes
            .filter((r) => r.version)
            .map((r) => (
              <div key={r.name} className="contents">
                <dt className="text-muted">{`${runtimeLabel(r.name)} version`}</dt>
                <dd className="min-w-0 font-mono break-words">
                  {r.version}
                  {!r.tested && (
                    <>
                      {" "}
                      <span className="text-xs text-warning">untested</span>
                    </>
                  )}
                </dd>
              </div>
            ))}
    </dl>
  );
}
