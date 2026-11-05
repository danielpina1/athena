// One tool call of a session with its result, paired by tool_use_id: a compact row that says what the agent did (a
// verb and a target, such as "Edited  api/handler.go") and how it went (a check and the time it took, or +N −M for a
// file change, a ✕ and the error's first line, or a pulsing dot while it runs). Opened, a body made for the tool: a
// terminal block for Bash, a diff for Edit, a checklist for TodoWrite, the summary for a Report. Rows start closed, and
// a body is built on its row's first open.
// Hostile text (a 1 MiB result, escapes, RTL) wraps inside its block, which scrolls on its own and takes focus so a
// keyboard can scroll it.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import {
  BotIcon,
  CheckIcon,
  ChevronRightIcon,
  FilePlusIcon,
  FileTextIcon,
  FlagIcon,
  GlobeIcon,
  ListChecksIcon,
  type LucideIcon,
  MessageCircleIcon,
  PencilIcon,
  PlugIcon,
  SearchIcon,
  TerminalIcon,
  WrenchIcon,
  XIcon,
} from "lucide-react";
import { type ReactNode, useState } from "react";
import { Link, useInRouterContext, useLocation } from "react-router";
import { fileLinkTo } from "@/features/tickets/fileLink";
import { isDocument } from "@/lib/artifacts/files";
import type { StreamItem } from "@/gen/athena/v1/stream_pb";
import { itemText } from "@/lib/stream/ansi";
import { Markdown } from "@/lib/text/markdown";
import { cn } from "@/ui/cn";
import {
  clipped,
  type Counts,
  describeTool,
  editDiff,
  editsOf,
  errorLine,
  formatDuration,
  linesOf,
  str,
  todoList,
  type ToolInput,
  type ToolKind,
  type ToolView,
  toolInput,
} from "./tools";

export type ToolStatus = "ok" | "error" | "running" | "none";

const ICONS: Record<ToolKind, LucideIcon> = {
  bash: TerminalIcon,
  read: FileTextIcon,
  write: FilePlusIcon,
  edit: PencilIcon,
  search: SearchIcon,
  web: GlobeIcon,
  todo: ListChecksIcon,
  agent: BotIcon,
  report: FlagIcon,
  ack: MessageCircleIcon,
  mcp: PlugIcon,
  other: WrenchIcon,
  hidden: WrenchIcon,
};

/** A block of tool text: mono, wrapping, scrolling inside itself. */
const BLOCK =
  "max-h-80 min-w-0 overflow-x-hidden overflow-y-auto rounded-md border border-line-soft bg-surface-2 px-3 py-2 font-mono text-xs leading-[1.6] [tab-size:4] whitespace-pre-wrap wrap-anywhere";
/** Write shows the start of the file. */
const WRITE_LINES = 20;

function Block({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <pre dir="auto" tabIndex={0} aria-label={label} className={cn(BLOCK, className)}>
      {children}
    </pre>
  );
}

function LineCounts({ counts }: { counts: Counts }) {
  return (
    <span data-counts className="font-mono" title={counts.about ? "About: the edit is longer than the dashboard was sent" : undefined}>
      {counts.about && "~"}
      <span className="text-success">+{counts.add}</span>
      {counts.del > 0 && <span className="ml-1.5 text-danger">−{counts.del}</span>}
    </span>
  );
}

function Status({ status, took, counts }: { status: ToolStatus; took?: string; counts?: ToolView["counts"] }) {
  switch (status) {
    case "ok":
      return (
        <span className="inline-flex items-center gap-1.5 text-xs text-muted tabular-nums">
          <CheckIcon aria-hidden className="size-3.5 text-success" />
          <span className="sr-only">Succeeded</span>
          {counts ? <LineCounts counts={counts} /> : took}
        </span>
      );
    case "error":
      return (
        <span className="inline-flex items-center gap-1 text-xs text-danger">
          <XIcon aria-hidden className="size-3.5" />
          failed
        </span>
      );
    case "running":
      return (
        <span className="inline-flex items-center gap-1.5 text-xs text-muted">
          <span aria-hidden className="size-1.5 animate-pulse rounded-full bg-muted" />
          running
        </span>
      );
    default:
      return <span className="text-xs text-muted">no result</span>;
  }
}

function Diff({ input, target }: { input: ToolInput; target: string }) {
  return (
    <>
      {editsOf(input).map(({ edit, path }, i) => (
        // A scroll region of its own: focusable, so a keyboard can scroll a long diff.
        <div key={i} role="group" tabIndex={0} aria-label={`Changes in ${target}`} className={cn(BLOCK, "grid content-start whitespace-normal")}>
          {editDiff(str(edit, "old_string"), str(edit, "new_string") || str(edit, "new_source")).map((l, k) => (
            <div
              key={k}
              data-line={l.sign === "+" ? "add" : l.sign === "−" ? "del" : "ctx"}
              className={cn("flex min-w-0", l.sign === "+" && "text-success", l.sign === "−" && "text-danger", l.sign === " " && "text-muted")}
            >
              <span aria-hidden={l.sign === " "} className="w-4 shrink-0 select-none">
                {l.sign}
              </span>
              <span className="min-w-0 flex-1 whitespace-pre-wrap wrap-anywhere">{l.text}</span>
            </div>
          ))}
          {input._lines && Object.keys(input._lines).some((k) => k.startsWith(path)) && (
            <div className="pt-1 text-muted">The edit is longer than shown.</div>
          )}
        </div>
      ))}
    </>
  );
}

const TODO_MARK: Record<string, string> = { completed: "✓", in_progress: "●", pending: "○" };

function Todos({ input }: { input: ToolInput }) {
  return (
    <ul aria-label="Plan" className="grid gap-1">
      {todoList(input).map((t, i) => (
        <li
          key={i}
          data-todo={t.status}
          className={cn("flex min-w-0 gap-2", t.status === "completed" && "text-muted", t.status === "in_progress" && "font-medium")}
        >
          <span aria-hidden className="w-4 shrink-0 text-center font-mono">
            {TODO_MARK[t.status] ?? "○"}
          </span>
          <span className="sr-only">{t.status.replace("_", " ")}: </span>
          <span dir="auto" className="min-w-0 flex-1 wrap-anywhere">
            {t.content}
          </span>
        </li>
      ))}
    </ul>
  );
}

function InputList({ input, summary }: { input: ToolInput; summary: string }) {
  const entries = Object.entries(input).filter(([k]) => k !== "_lines");
  if (entries.length === 0) return summary ? <Block label="Input">{summary}</Block> : null;
  return (
    <dl aria-label="Input" className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 font-mono text-xs">
      {entries.map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="text-muted">{k}</dt>
          <dd dir="auto" className="min-w-0 whitespace-pre-wrap wrap-anywhere">
            {typeof v === "string" ? v : JSON.stringify(v)}
          </dd>
        </div>
      ))}
    </dl>
  );
}

/** A note under a value the dashboard was sent shortened. */
function Shortened({ children }: { children: ReactNode }) {
  return <p className="text-xs text-muted">{children}</p>;
}

/** Scrolls to the Ticket's latest Report card (TicketDetail, above the tabs) and focuses it. */
function revealLatestReport() {
  const el = document.querySelector<HTMLElement>("[data-report]");
  if (!el) return;
  const still = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? true;
  el.scrollIntoView?.({ block: "start", behavior: still ? "auto" : "smooth" });
  el.focus({ preventScroll: true });
}

/** The part of a body every tool shares: the result, or why there is none. */
function Output({ result, active, label = "Output" }: { result?: StreamItem; active: boolean; label?: string }) {
  if (!result) return <p className="text-xs text-muted">{active ? "Waiting for the result." : "No result was recorded."}</p>;
  return (
    <Block label={label} className={cn(result.isError && "text-danger")}>
      {itemText(result) || "(no output)"}
    </Block>
  );
}

function Body({ view, input, call, result, active }: { view: ToolView; input: ToolInput; call?: StreamItem; result?: StreamItem; active: boolean }) {
  const summary = call ? itemText(call) : "";
  const failure = result?.isError ? <Output result={result} active={active} label="Error" /> : null;
  switch (view.kind) {
    case "bash": {
      const out = result ? itemText(result) : "";
      return (
        <>
          <Block label="Terminal" className={cn(result?.isError && "border-danger")}>
            <span className="text-muted">$ </span>
            {str(input, "command") || summary}
            {out && "\n"}
            {out}
          </Block>
          {clipped(input, "command") && <Shortened>Shortened: the command is longer than shown.</Shortened>}
          {!result && <Output active={active} />}
        </>
      );
    }
    case "write": {
      const content = str(input, "content");
      const shown = content ? content.replace(/\n$/, "").split("\n").slice(0, WRITE_LINES) : [];
      const more = linesOf(input, "content", content) - shown.length;
      return (
        <>
          <Block label="Content">{shown.join("\n")}</Block>
          {more > 0 && <p className="text-xs text-muted">{`… ${more} more ${more === 1 ? "line" : "lines"}`}</p>}
          {failure}
        </>
      );
    }
    case "edit":
      return (
        <>
          <Diff input={input} target={view.target} />
          {failure}
        </>
      );
    case "todo":
      return (
        <>
          <Todos input={input} />
          {failure}
        </>
      );
    case "report": {
      const question = str(input, "question");
      return (
        <>
          <Markdown text={str(input, "summary") || summary} />
          {(clipped(input, "summary") || clipped(input, "question")) && (
            <Shortened>
              Shortened. The full report is on the Ticket.
              {document.querySelector("[data-report]") && (
                <>
                  {" "}
                  <button type="button" onClick={revealLatestReport} className="font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text">
                    Show the full report
                  </button>
                </>
              )}
            </Shortened>
          )}
          {question && <Markdown text={`**Question:** ${question}`} />}
          {failure}
        </>
      );
    }
    case "ack":
      return (
        <>
          <p dir="auto" className="whitespace-pre-wrap wrap-anywhere">
            {str(input, "text") || summary}
          </p>
          {failure}
        </>
      );
    case "agent":
      return (
        <>
          <Markdown text={str(input, "prompt") || summary} className="rounded-md border border-line-soft px-3 py-2 text-[13px]" />
          {clipped(input, "prompt") && <Shortened>Shortened: the prompt is longer than shown.</Shortened>}
          {result && !result.isError ? <Markdown text={itemText(result)} className="text-[13px]" /> : <Output result={result} active={active} />}
        </>
      );
    case "read":
    case "search":
    case "web":
      return <Output result={result} active={active} />;
    default:
      return (
        <>
          <InputList input={input} summary={summary} />
          <Output result={result} active={active} />
        </>
      );
  }
}

// A Ticket's worktree in a path: …/.athena/work/<slug>/.
const IN_WORKTREE = /\/\.athena\/work\/[^/]+\/(.+)$/;

/** Open, beside a Write or Edit row of a document: the file in the Ticket's viewer (its path in the worktree, if it is). */
function OpenFile({ ticketId, path }: { ticketId: string; path: string }) {
  const here = useLocation();
  const file = IN_WORKTREE.exec(path)?.[1] ?? path;
  return (
    <Link
      to={fileLinkTo(here, file, `/tickets/${encodeURIComponent(ticketId)}`)}
      aria-label={`Open ${file}`}
      className="inline-flex min-h-11 shrink-0 items-center px-2 text-xs font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-8"
    >
      Open
    </Link>
  );
}

/**
 * call is missing when it fell out of the kept items; result is missing while the tool runs. active says the Ticket
 * has not ended: a call without a result is running until then (waiting for an Approval too), and has none after.
 * With ticketId, a Write or Edit of a document has an Open link to the Ticket's file viewer, beside the row (a link
 * inside its summary would be a control inside a control).
 */
export function ToolCall({ call, result, active, ticketId }: { call?: StreamItem; result?: StreamItem; active: boolean; ticketId?: string }) {
  const routed = useInRouterContext();
  const status: ToolStatus = result ? (result.isError ? "error" : "ok") : active ? "running" : "none";
  const took = call?.at && result?.at ? formatDuration(timestampMs(result.at) - timestampMs(call.at)) : undefined;
  const input = toolInput(call);
  const view = describeTool(call?.tool ?? "", input, call ? itemText(call) : "");
  const Icon = ICONS[view.kind];
  const error = status === "error" && result ? errorLine(itemText(result)) : "";
  // The body is built on the first open, not for every closed row: a long session has hundreds of them.
  const [opened, setOpened] = useState(false);
  const written = (view.kind === "write" || view.kind === "edit") && call?.tool !== "NotebookEdit" ? str(input, "file_path") : "";
  const row = (
    <details
      data-testid="tool-call"
      data-status={status}
      data-tool={view.kind}
      onToggle={(e) => e.currentTarget.open && setOpened(true)}
      className="group -mx-2 min-w-0"
    >
      <summary
        className={cn(
          "grid min-h-11 cursor-pointer list-none grid-cols-[16px_16px_minmax(0,1fr)_auto] items-center gap-x-2 rounded-md px-2 py-1.5",
          "transition-colors duration-(--motion-fast) hover:bg-surface-2 group-open:bg-surface-2 lg:min-h-8 lg:py-1 [&::-webkit-details-marker]:hidden",
        )}
      >
        <ChevronRightIcon aria-hidden className="size-4 text-muted transition-transform duration-(--motion-fast) group-open:rotate-90" />
        <Icon aria-hidden className={cn("size-4", status === "error" ? "text-danger" : "text-muted")} />
        <span className="grid min-w-0 sm:flex sm:items-baseline sm:gap-2">
          <span dir="auto" className="min-w-0 truncate font-medium sm:max-w-[65%] sm:shrink-0">
            {view.label}
          </span>
          {view.target && (
            <span dir="auto" className="min-w-0 flex-1 truncate font-mono text-xs text-muted">
              {view.target}
            </span>
          )}
        </span>
        <Status status={status} took={took} counts={view.counts} />
        {error && (
          <span dir="auto" className="col-start-3 col-end-5 truncate text-xs text-danger">
            {error}
          </span>
        )}
      </summary>
      {opened && (
        <div className="grid min-w-0 gap-2 pt-2 pr-2 pb-3 pl-2 sm:pl-14">
          <Body view={view} input={input} call={call} result={result} active={active} />
        </div>
      )}
    </details>
  );
  if (!ticketId || !routed || !written || status === "error" || !isDocument(written)) return row;
  return (
    <div className="flex min-w-0 items-start">
      <div className="min-w-0 flex-1">{row}</div>
      <OpenFile ticketId={ticketId} path={written} />
    </div>
  );
}
