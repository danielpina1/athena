// A Ticket's live session, as its stream renders it, made to be read: the agent's words as Markdown in the UI's sans
// type, its thinking collapsed (and none when it has no text), the start of the session as a thin divider, the owner's
// messages as "You", each tool call paired with its result in one compact row (ToolCall), and the result as a closing
// "Done" block. Code, paths and commands stay mono. A gap in the stream says how much output was skipped. A batch
// renders only its own rows: the rows already shown are memoised on their stream items, whose keys and stripped text
// are computed once per item. The view follows the tail while the owner is within 48 px of the bottom; scrolled up, it
// stays put and offers "Jump to latest". The ● live dot shows while the link is open and the Ticket works. It is a lazy
// chunk: screens show it through LazySessionView (SessionFrame.tsx).
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { ChevronRightIcon, CircleCheckIcon, CircleXIcon } from "lucide-react";
import { memo, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { StreamItem } from "@/gen/athena/v1/stream_pb";
import { itemText } from "@/lib/stream/ansi";
import { itemKey, useSessionStream } from "@/lib/stream/useSessionStream";
import { useAthena } from "@/lib/store/state";
import { Markdown } from "@/lib/text/markdown";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { logClass, SessionFrame, type SessionViewProps } from "./SessionFrame";
import { ToolCall } from "./ToolCall";
import { modelName, parseResult } from "./tools";

/** Within this many px of the bottom counts as at the bottom. */
export const FOLLOW_PX = 48;

type Row =
  | { kind: "item"; key: string; item: StreamItem }
  | { kind: "tool"; key: string; call?: StreamItem; result?: StreamItem }
  | { kind: "gap"; key: string; bytes: bigint };

/** Tool calls that take no row (loading tool definitions says nothing to the owner), with their results. */
const HIDDEN_TOOLS = new Set(["ToolSearch"]);

/**
 * The stream as rows: a tool's result joins its call's row; the gap sits before the first item past it. Thinking
 * without text and the calls in HIDDEN_TOOLS take no row.
 */
export function buildRows(items: readonly StreamItem[], gap?: { from: bigint; to: bigint }): Row[] {
  const rows: Row[] = [];
  /** Each call's row index, by tool_use_id; -1 for a hidden call. */
  const calls = new Map<string, number>();
  let gapAt = gap ? items.findIndex((it) => it.offset >= gap.to) : -1;
  if (gap && gapAt < 0) gapAt = items.length;
  items.forEach((it, i) => {
    if (gap && i === gapAt) rows.push({ kind: "gap", key: "gap", bytes: gap.to - gap.from });
    const key = itemKey(it);
    if (it.kind === "thinking" && !it.text.trim()) return;
    if (it.kind === "tool_call") {
      if (HIDDEN_TOOLS.has(it.tool)) {
        if (it.toolUseId) calls.set(it.toolUseId, -1);
        return;
      }
      if (it.toolUseId) calls.set(it.toolUseId, rows.length);
      rows.push({ kind: "tool", key, call: it });
      return;
    }
    if (it.kind === "tool_result") {
      const at = it.toolUseId ? calls.get(it.toolUseId) : undefined;
      if (at === -1) return;
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

const TIME = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });

function Init({ item, text }: { item: StreamItem; text: string }) {
  const model = modelName(/\bmodel (\S+)/.exec(text)?.[1] ?? "");
  const at = item.at ? new Date(timestampMs(item.at)) : undefined;
  return (
    <div data-kind="init" className="flex min-w-0 items-center gap-3 py-1 text-xs text-muted">
      <span aria-hidden className="h-px min-w-6 flex-1 bg-line-soft" />
      <span className="min-w-0 truncate">
        Session started{model && ` · ${model}`}
        {at && (
          <>
            {" · "}
            <time dateTime={at.toISOString()}>{TIME.format(at)}</time>
          </>
        )}
      </span>
      <span aria-hidden className="h-px min-w-6 flex-1 bg-line-soft" />
    </div>
  );
}

function Result({ item, text }: { item: StreamItem; text: string }) {
  const [head, ...rest] = text.split("\n");
  const body = rest.join("\n").trim();
  const { title, meta } = parseResult(head, item.isError);
  const failed = title !== "Done";
  const Icon = failed ? CircleXIcon : CircleCheckIcon;
  return (
    <div data-kind="result" data-error={item.isError || undefined} className="grid min-w-0 gap-2 border-t border-line-soft pt-3">
      <p className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
        <Icon aria-hidden className={cn("size-4 shrink-0", failed ? "text-danger" : "text-ok")} />
        <span className={cn("font-semibold", failed && "text-danger")}>{title}</span>
        {meta && <span className="font-mono text-xs text-muted tabular-nums">{meta}</span>}
      </p>
      {body && <Markdown text={body} className="max-w-[75ch]" />}
    </div>
  );
}

function Item({ item }: { item: StreamItem }) {
  const text = itemText(item);
  switch (item.kind) {
    case "say":
      return <Markdown text={text} className="max-w-[75ch]" />;
    case "thinking":
      return (
        <details className="group min-w-0">
          <summary className="inline-flex min-h-11 cursor-pointer list-none items-center gap-1.5 text-sm text-muted lg:min-h-0 [&::-webkit-details-marker]:hidden">
            <ChevronRightIcon aria-hidden className="size-3.5 transition-transform duration-(--motion-fast) group-open:rotate-90" />
            Thinking
          </summary>
          <p dir="auto" className="mt-1 max-w-[75ch] border-l border-line-soft pl-3 text-sm whitespace-pre-wrap text-muted wrap-anywhere">
            {text}
          </p>
        </details>
      );
    case "init":
      return <Init item={item} text={text} />;
    case "result":
      return <Result item={item} text={text} />;
    case "user":
      return (
        <div data-kind="user" className="ml-auto grid w-fit max-w-[min(85%,75ch)] min-w-0 gap-0.5 rounded-md border border-line-soft bg-surface-2 px-3 py-2">
          <span className="text-xs font-medium text-muted">You</span>
          <p dir="auto" className="whitespace-pre-wrap wrap-anywhere">
            {text}
          </p>
        </div>
      );
    default:
      return (
        <p dir="auto" className="text-xs whitespace-pre-wrap text-muted wrap-anywhere">
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
    <SessionFrame compact={compact} title={title} actions={actions} live={live && working}>
      <div ref={log} role="log" aria-label="Session output" tabIndex={0} onScroll={onScroll} className={logClass(compact)}>
        {rows.length === 0 ? (
          <p className="text-muted">{refused ? "This session's output isn't available." : "No session output yet."}</p>
        ) : (
          // Prose breathes; a run of tool rows sits close together, as one list.
          <ol className="flex min-w-0 flex-col [&>li+li]:mt-4 [&>li[data-row=tool]+li[data-row=tool]]:mt-0.5">
            {rows.map((row) => (
              <li key={row.key} data-row={row.kind} className="min-w-0">
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
    </SessionFrame>
  );
}
