// A Ticket's live session, as its stream renders it: mono 13 px; the agent's words as text, its thinking collapsed,
// the init line muted, each tool call paired with its result in one row, the result as a closing line. A gap in the
// stream says how much output was skipped. A batch renders only its own rows: the rows already shown are memoised on
// their stream items, whose keys and stripped text are computed once per item. The view follows the tail while the owner is within 48 px of the bottom; scrolled
// up, it stays put and offers "Jump to latest". The ● live dot shows while the link is open and the Ticket works.
import { memo, type ReactNode, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { StreamItem } from "@/gen/athena/v1/stream_pb";
import { itemText } from "@/lib/stream/ansi";
import { itemKey, useSessionStream } from "@/lib/stream/useSessionStream";
import { useAthena } from "@/lib/store/state";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { ToolCall } from "./ToolCall";

/** Within this many px of the bottom counts as at the bottom. */
export const FOLLOW_PX = 48;

type Row =
  | { kind: "item"; key: string; item: StreamItem }
  | { kind: "tool"; key: string; call?: StreamItem; result?: StreamItem }
  | { kind: "gap"; key: string; bytes: bigint };

/** The stream as rows: a tool's result joins its call's row; the gap sits before the first item past it. */
export function buildRows(items: readonly StreamItem[], gap?: { from: bigint; to: bigint }): Row[] {
  const rows: Row[] = [];
  /** Each call's row index, by tool_use_id. */
  const calls = new Map<string, number>();
  let gapAt = gap ? items.findIndex((it) => it.offset >= gap.to) : -1;
  if (gap && gapAt < 0) gapAt = items.length;
  items.forEach((it, i) => {
    if (gap && i === gapAt) rows.push({ kind: "gap", key: "gap", bytes: gap.to - gap.from });
    const key = itemKey(it);
    if (it.kind === "tool_call") {
      if (it.toolUseId) calls.set(it.toolUseId, rows.length);
      rows.push({ kind: "tool", key, call: it });
      return;
    }
    if (it.kind === "tool_result") {
      const at = it.toolUseId ? calls.get(it.toolUseId) : undefined;
      const call = at === undefined ? undefined : rows[at];
      if (call?.kind === "tool" && !call.result) {
        // A new row object, so the memoised row sees the change.
        rows[at!] = { ...call, result: it };
      } else {
        rows.push({ kind: "tool", key, result: it });
      }
      return;
    }
    rows.push({ kind: "item", key, item: it });
  });
  if (gap && gapAt === items.length) rows.push({ kind: "gap", key: "gap", bytes: gap.to - gap.from });
  return rows;
}

/** A size of transcript as the gap line says it: 512 B, 1.5 KB, 12 KB, 3.0 MB. */
export function formatBytes(n: bigint): string {
  const b = n > 0n ? Number(n) : 0;
  if (b < 1024) return `${b} B`;
  const [v, unit] = b < 1024 * 1024 ? [b / 1024, "KB"] : [b / (1024 * 1024), "MB"];
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${unit}`;
}

const TEXT = "whitespace-pre-wrap wrap-anywhere";

function Item({ item }: { item: StreamItem }) {
  const text = itemText(item);
  switch (item.kind) {
    case "say":
      return (
        <p dir="auto" className={TEXT}>
          {text}
        </p>
      );
    case "thinking":
      return (
        <details className="group min-w-0">
          <summary className="min-h-11 cursor-pointer py-1 text-muted lg:min-h-0">Thinking</summary>
          <p dir="auto" className={cn(TEXT, "border-l border-line-soft pl-3 text-muted")}>
            {text}
          </p>
        </details>
      );
    case "init":
      return <p className={cn("truncate text-muted")}>{text}</p>;
    case "result": {
      const [head, ...rest] = text.split("\n");
      const body = rest.join("\n").trim();
      return (
        <div className="grid min-w-0 gap-1 border-t border-line pt-2">
          <p className={cn("font-semibold wrap-anywhere", item.isError && "text-danger")}>{head}</p>
          {body && (
            <p dir="auto" className={TEXT}>
              {body}
            </p>
          )}
        </div>
      );
    }
    default:
      return (
        <p dir="auto" className={cn(TEXT, "text-muted")}>
          {text}
        </p>
      );
  }
}

const RowView = memo(
  function RowView({ row, active }: { row: Row; active: boolean }) {
    switch (row.kind) {
      case "gap":
        return (
          <p data-gap role="note" className="rounded-xs border border-dashed border-line-soft px-3 py-2 font-sans font-semibold">
            Some output was skipped ({formatBytes(row.bytes)})
          </p>
        );
      case "tool":
        return <ToolCall call={row.call} result={row.result} active={active} />;
      default:
        return <Item item={row.item} />;
    }
  },
  // buildRows makes new row objects on every batch: compare what they show, so the rows already there stay put.
  ({ row: a, active: wasActive }, { row: b, active }) => {
    if (a.kind === "item" && b.kind === "item") return a.item === b.item;
    if (a.kind === "tool" && b.kind === "tool") return a.call === b.call && a.result === b.result && wasActive === active;
    if (a.kind === "gap" && b.kind === "gap") return a.bytes === b.bytes;
    return false;
  },
);

export function LiveDot() {
  return (
    <span className="inline-flex shrink-0 items-center gap-1.5 font-mono text-xs text-text">
      <span aria-hidden className="size-2 rounded-full bg-ok" />
      live
    </span>
  );
}

export type SessionViewProps = {
  ticketId: string;
  /** The pinned form: no heading of its own, and the log fills the space it is given. */
  compact?: boolean;
  /** Replaces the "Session" heading. */
  title?: ReactNode;
  /** Controls at the end of the heading row. */
  actions?: ReactNode;
};

export function SessionView({ ticketId, compact = false, title, actions }: SessionViewProps) {
  const { items, gap, live, refused } = useSessionStream(ticketId);
  const stage = useAthena((s) => (s.tickets.get(ticketId) ?? s.details.get(ticketId)?.summary)?.stage);
  const working = stage === "working";
  // A call without a result is still running until the Ticket ends (it may wait for an Approval) (R45).
  const active = stage !== "ended";
  const rows = useMemo(() => buildRows(items, gap), [items, gap]);

  const log = useRef<HTMLDivElement>(null);
  const following = useRef(true);
  const [atEnd, setAtEnd] = useState(true);
  const onScroll = () => {
    const el = log.current;
    if (!el) return;
    const near = el.scrollHeight - el.scrollTop - el.clientHeight <= FOLLOW_PX;
    following.current = near;
    setAtEnd(near);
  };
  useLayoutEffect(() => {
    const el = log.current;
    if (el && following.current) el.scrollTop = el.scrollHeight;
  }, [rows]);
  const jump = () => {
    const el = log.current;
    if (el) el.scrollTop = el.scrollHeight;
    following.current = true;
    setAtEnd(true);
  };

  return (
    <div className={cn("grid min-w-0 gap-2", compact && "flex min-h-0 flex-1 flex-col")}>
      <div className="flex min-h-6 min-w-0 items-center gap-3">
        <div className="min-w-0 flex-1">{title ?? (!compact && <h2 className="text-lg font-semibold">Session</h2>)}</div>
        {live && working && <LiveDot />}
        {actions}
      </div>
      <div className={cn("relative min-w-0", compact && "flex min-h-0 flex-1 flex-col")}>
        <div
          ref={log}
          role="log"
          aria-label="Session output"
          tabIndex={0}
          onScroll={onScroll}
          className={cn(
            "min-w-0 overflow-x-hidden overflow-y-auto rounded-xs border border-line-soft bg-surface p-3 font-mono text-sm",
            compact ? "min-h-0 flex-1" : "h-[60dvh] min-h-64",
          )}
        >
          {rows.length === 0 ? (
            <p className="text-muted">{refused ? "This session's output isn't available." : "No session output yet."}</p>
          ) : (
            <ol className="grid min-w-0 gap-2">
              {rows.map((row) => (
                <li key={row.key} className="min-w-0">
                  <RowView row={row} active={active} />
                </li>
              ))}
            </ol>
          )}
        </div>
        {!atEnd && (
          <Button
            variant="primary"
            size="sm"
            onClick={jump}
            className="absolute bottom-3 left-1/2 -translate-x-1/2"
          >
            Jump to latest
          </Button>
        )}
      </div>
    </div>
  );
}
