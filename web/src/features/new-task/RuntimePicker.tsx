// The New screen's runtime: Auto · Claude Code · Codex, Auto by default (spec §6.1a). A runtime the node cannot start
// now is disabled, and says why twice: in its native tooltip, and in a line under the group that the segment names
// with aria-describedby (a disabled segment takes no hover on a phone, and the line is what a screen reader reads).
// The line names the runtime unless its why already does ("Codex is not installed…").
import { useId } from "react";
import type { RuntimeStatus } from "@/gen/athena/v1/state_pb";
import { RUNTIMES, runtimeLabel } from "@/lib/text/runtime";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";

const OPTIONS = ["auto", ...RUNTIMES] as const;

/**
 * Why a runtime cannot be chosen now, or undefined when it can. Auto always can. With no runtimes listed (an older
 * server, or before the first Snapshot) nothing is known, so every runtime is offered.
 */
export function unavailable(name: string, runtimes: readonly RuntimeStatus[]): string | undefined {
  if (name === "auto" || runtimes.length === 0) return undefined;
  const r = runtimes.find((s) => s.name === name);
  if (!r) return "Not set up on this node";
  return r.ready ? undefined : r.why || "Not ready";
}

export function RuntimePicker({
  value,
  onChange,
  runtimes,
}: {
  value: string;
  onChange(runtime: string): void;
  runtimes: readonly RuntimeStatus[];
}) {
  const id = useId();
  const why = OPTIONS.map((name) => ({ name, why: unavailable(name, runtimes) }));
  return (
    <div className="grid min-w-0 gap-1">
      <ToggleGroup
        type="single"
        aria-label="Runtime"
        value={value}
        // Pressing the chosen segment again keeps it: a runtime is always chosen.
        onValueChange={(v) => v && onChange(v)}
      >
        {why.map(({ name, why }) => (
          <ToggleGroupItem
            key={name}
            value={name}
            disabled={why !== undefined}
            title={why}
            aria-describedby={why ? `${id}-${name}` : undefined}
            // A disabled segment still takes the pointer, so its tooltip shows on hover.
            className="disabled:pointer-events-auto disabled:cursor-not-allowed"
          >
            {runtimeLabel(name)}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>
      {why.map(({ name, why }) =>
        why ? (
          <p key={name} id={`${id}-${name}`} className="text-sm font-normal break-words text-muted">
            {why.startsWith(runtimeLabel(name)) ? why : `${runtimeLabel(name)}: ${why}`}
          </p>
        ) : null,
      )}
    </div>
  );
}
