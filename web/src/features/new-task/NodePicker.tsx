// The New screen's node: Any node · each node, Any node by default (plan 6). It shows only with two nodes or more that
// are not revoked; a revoked node is not offered. An offline or draining node is listed but disabled and says so in
// the segment itself (a disabled segment takes no hover on a phone). This machine is marked.
import type { NodeStatus } from "@/gen/athena/v1/state_pb";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";

/** The nodes a task could be pinned to: every node that is not revoked. */
export const pinnable = (nodes: readonly NodeStatus[]) => nodes.filter((n) => n.state !== "revoked");

/** Why a node cannot take a task now ("offline", "draining"), or undefined when it can. */
export function nodeUnavailable(n: NodeStatus): string | undefined {
  return n.state === "offline" || n.state === "draining" ? n.state : undefined;
}

/** The node id to send: the choice while that node can take a task, else "" (any node). */
export function chosenNode(value: string, nodes: readonly NodeStatus[]): string {
  const n = pinnable(nodes).find((x) => x.id === value);
  return n && nodeUnavailable(n) === undefined && pinnable(nodes).length >= 2 ? value : "";
}

export function NodePicker({
  value,
  onChange,
  nodes,
}: {
  value: string;
  onChange(node: string): void;
  nodes: readonly NodeStatus[];
}) {
  const options = pinnable(nodes);
  if (options.length < 2) return null;
  return (
    <div className="grid gap-1 text-sm font-semibold">
      <span>Node</span>
      <ToggleGroup
        type="single"
        aria-label="Node"
        // "" is Any node: Radix takes an empty value as nothing chosen, so it goes by its own name.
        value={value || "any"}
        // Pressing the chosen segment again keeps it: a node, or any, is always chosen.
        onValueChange={(v) => v && onChange(v === "any" ? "" : v)}
      >
        <ToggleGroupItem value="any">Any node</ToggleGroupItem>
        {options.map((n) => {
          const why = nodeUnavailable(n);
          return (
            <ToggleGroupItem
              key={n.id}
              value={n.id}
              disabled={why !== undefined}
              className="max-w-full min-w-0 disabled:cursor-not-allowed"
            >
              <span className="min-w-0 truncate">{n.name || n.id}</span>
              {n.local && <span className="font-mono text-xs font-normal">this machine</span>}
              {why && <span className="font-mono text-xs font-normal">{why}</span>}
            </ToggleGroupItem>
          );
        })}
      </ToggleGroup>
    </div>
  );
}
