// One tool call of a session with its result, paired by tool_use_id: a collapsed row with the tool, the first line of
// its input and its status (ok, error, or running) with how long it took; opened, the whole input and the result.
// Hostile text (a 1 MiB result, escapes, RTL) wraps inside the row and scrolls inside its own block, which takes focus
// so a keyboard can scroll it.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { ChevronRightIcon } from "lucide-react";
import type { StreamItem } from "@/gen/athena/v1/stream_pb";
import { itemText } from "@/lib/stream/ansi";
import { cn } from "@/ui/cn";

export type ToolStatus = "ok" | "error" | "running" | "none";

/** A duration as the session view says it: 0.4 s, 1.2 s, 12 s, 2 m 5 s. */
export function formatDuration(ms: number): string {
  const s = Math.max(0, ms) / 1000;
  if (s < 10) return `${s.toFixed(1)} s`;
  if (s < 60) return `${Math.round(s)} s`;
  const m = Math.floor(s / 60);
  return `${m} m ${Math.round(s - m * 60)} s`;
}

function firstLine(s: string): string {
  return s.split("\n", 1)[0];
}

const BLOCK = "max-h-80 overflow-x-hidden overflow-y-auto whitespace-pre-wrap wrap-anywhere rounded-xs bg-surface-2 px-2 py-1";

/**
 * call is missing when it fell out of the kept items; result is missing while the tool runs. active says the Ticket
 * has not ended: a call without a result is running until then (waiting for an Approval too), and has none after.
 */
export function ToolCall({ call, result, active }: { call?: StreamItem; result?: StreamItem; active: boolean }) {
  const status: ToolStatus = result ? (result.isError ? "error" : "ok") : active ? "running" : "none";
  const took = call?.at && result?.at ? formatDuration(timestampMs(result.at) - timestampMs(call.at)) : undefined;
  const label = status === "none" ? "no result" : took ? `${status} · ${took}` : status;
  const input = call ? itemText(call) : "";
  return (
    <details data-testid="tool-call" data-status={status} className="group min-w-0 rounded-xs border border-line-soft">
      <summary className="flex min-h-11 min-w-0 cursor-pointer list-none items-center gap-2 px-2 py-1 lg:min-h-8 [&::-webkit-details-marker]:hidden">
        <ChevronRightIcon aria-hidden className="size-4 shrink-0 text-muted transition-transform duration-(--motion-fast) group-open:rotate-90" />
        <span className="shrink-0 font-semibold">{call?.tool || "Tool"}</span>
        <span dir="auto" className="min-w-0 flex-1 truncate text-muted">
          {firstLine(input)}
        </span>
        <span
          className={cn(
            "shrink-0 text-xs",
            status === "error" ? "text-danger" : status === "ok" ? "text-text" : "text-muted",
          )}
        >
          {label}
        </span>
      </summary>
      <div className="grid min-w-0 gap-2 border-t border-line-soft p-2">
        {call && (
          <pre dir="auto" tabIndex={0} aria-label="Input" className={cn(BLOCK, "font-mono")}>
            {input}
          </pre>
        )}
        {result ? (
          <pre dir="auto" tabIndex={0} aria-label="Result" className={cn(BLOCK, "font-mono", result.isError && "text-danger")}>
            {itemText(result) || "(no output)"}
          </pre>
        ) : (
          <p className="text-muted">{active ? "Waiting for the result." : "No result was recorded."}</p>
        )}
      </div>
    </details>
  );
}
