// Settings › Execution, read-only: what the node runs sessions with, each runtime's version included (a runtime that
// reports none has no row; "untested" when Athena was not verified against it). Slots and the default model are flags of
// `athena server` (internal/cli), not settings the dashboard can change, so this says where to change them.
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { runtimeLabel } from "@/lib/text/runtime";
import { Waiting } from "@/ui/athena/Waiting";
import { Block, Section } from "./Section";

export function Execution() {
  const node = useAthena((s) => s.node);
  const synced = useAthena(selectSynced);
  return (
    <Section title="Execution" text="What this node runs sessions with.">
      <Block title="This node">
        {!synced || !node ? (
          <Waiting />
        ) : (
          <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
            <dt className="text-muted">Node</dt>
            <dd className="min-w-0 font-mono break-words">{node.name || "This machine"}</dd>
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
        )}
      </Block>
      <Block title="Where to change these">
        <p className="max-w-prose text-sm">
          Slots and the default model are set when the server starts:{" "}
          <code className="font-mono text-xs break-words">athena server --slots 4 --model sonnet</code>. Restart the
          server to apply a change.
        </p>
      </Block>
    </Section>
  );
}
