// The Ticket's Report, read properly: the latest one first, its state, time and pull request, then the worker's summary
// as Markdown (the session view's safe renderer), clamped to about 12 lines with "Show the full report" expanding it in
// place. A question is a highlighted line with a Reply that opens the composer. Earlier Reports fold under their count.
// A lazy chunk (with the Markdown renderer); TicketDetail shows a placeholder while it loads.
import { useLayoutEffect, useRef, useState } from "react";
import type { ReportView } from "@/gen/athena/v1/state_pb";
import { stripAnsi } from "@/lib/text/ansi";
import { Markdown } from "@/lib/text/markdown";
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { cn } from "@/ui/cn";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { Button } from "@/ui/primitives/button";
import { clockTime } from "./timelineModel";

const STATE: Record<string, string> = {
  ready: "Ready for review",
  waiting: "Waiting on you",
  progress: "Progress",
  blocked: "Blocked",
  failed: "Failed",
  dropped: "Finished",
};

/** A summary this long is clamped before the browser can measure it (and in tests, which have no layout). */
const LONG_LINES = 12;
const LONG_CHARS = 1200;
/** The clamp, in em of the body text: about 12 lines of the prose's 1.6 line height. */
const CLAMP_EM = 19;

const link = "font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text";

function Pill({ state, ended, attention }: { state: string; ended: boolean; attention: boolean }) {
  const label = ended && state === "ready" ? "Finished" : (STATE[state] ?? "Report");
  return (
    <span
      data-state
      className={cn(
        "inline-flex h-5 shrink-0 items-center rounded-xs border px-2 text-xs font-semibold whitespace-nowrap",
        attention
          ? "border-accent bg-accent text-accent-fg"
          : state === "failed" || state === "blocked"
            ? "border-danger text-danger"
            : state === "ready" || state === "dropped"
              ? "border-ok bg-ok text-bg"
              : "border-line-soft text-muted",
      )}
    >
      {label}
    </span>
  );
}

function Meta({ report, repo }: { report: ReportView; repo: string }) {
  const at = report.at ? timestampMs(report.at) : NaN;
  return (
    <span className="flex min-w-0 flex-wrap items-baseline gap-x-2 font-mono text-xs text-muted">
      {Number.isFinite(at) && (
        <span>
          <RelativeTime at={at} /> · {clockTime(at)}
        </span>
      )}
      {report.pull > 0 &&
        (repo ? (
          <a href={`https://github.com/${repo}/pull/${report.pull}`} target="_blank" rel="noopener noreferrer" className={cn(link, "text-xs")}>
            {`PR #${report.pull}`}
          </a>
        ) : (
          <span>{`PR #${report.pull}`}</span>
        ))}
    </span>
  );
}

/** The summary as Markdown, clamped to about 12 lines until the owner asks for all of it. */
function Summary({ text }: { text: string }) {
  const ref = useRef<HTMLDivElement>(null);
  const long = text.split("\n").length > LONG_LINES || text.length > LONG_CHARS;
  const [overflows, setOverflows] = useState<boolean | undefined>(undefined);
  const [open, setOpen] = useState(false);

  useLayoutEffect(() => {
    const el = ref.current;
    // Without layout (no height to measure) the length decides. The full height is measured clamped or not.
    if (!el || el.clientHeight === 0) return;
    setOverflows(el.scrollHeight > CLAMP_EM * parseFloat(getComputedStyle(el).fontSize) + 1);
  }, [text]);

  const clampable = overflows ?? long;
  const clamped = clampable && !open;
  return (
    <div className="grid min-w-0 gap-2">
      <div
        ref={ref}
        data-testid="report-body"
        data-clamped={clamped}
        className={cn("min-w-0 text-body", clamped && "max-h-[19em] overflow-hidden", !clamped && "max-h-none")}
      >
        <Markdown text={stripAnsi(text)} />
      </div>
      {clampable && (
        <button
          type="button"
          aria-expanded={open}
          onClick={() => setOpen((o) => !o)}
          className={cn(link, "inline-flex min-h-11 items-center justify-self-start text-sm lg:min-h-0")}
        >
          {open ? "Show less" : "Show the full report"}
        </button>
      )}
    </div>
  );
}

function Earlier({ reports, repo }: { reports: ReportView[]; repo: string }) {
  return (
    <details className="group/earlier border-t border-line-soft">
      <summary className="flex min-h-11 cursor-pointer items-center px-4 text-sm font-medium text-text lg:min-h-9">
        {`Earlier reports (${reports.length})`}
      </summary>
      <div className="grid gap-4 px-4 pb-4">
        {reports.map((r) => (
          <article
            key={r.assignmentId}
            id={`report-${r.assignmentId}`}
            tabIndex={-1}
            data-report
            aria-label={`Report ${STATE[r.state] ?? ""}`}
            className="grid min-w-0 gap-2 border-t border-line-soft pt-3 first:border-t-0 first:pt-0"
          >
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
              <Pill state={r.state} ended={false} attention={false} />
              <Meta report={r} repo={repo} />
            </div>
            <Summary text={r.summary} />
          </article>
        ))}
      </div>
    </details>
  );
}

export type ReportCardProps = {
  /** The Ticket's Reports, newest last (TicketDetail.reports). */
  reports: readonly ReportView[];
  /** The Ticket's repository (owner/name), for the pull request's link. */
  repo: string;
  /** The Ticket waits on the owner: the latest Report's state is orange. */
  needsYou?: boolean;
  ended?: boolean;
  /** Shows the latest Report's question with a Reply (off while the inline answer card asks it). */
  showQuestion?: boolean;
  onReply?: () => void;
};

export default function ReportCard({ reports, repo, needsYou = false, ended = false, showQuestion = true, onReply }: ReportCardProps) {
  if (reports.length === 0) return null;
  const latest = reports[reports.length - 1];
  const earlier = reports.slice(0, -1).reverse();
  const question = showQuestion ? latest.question.trim() : "";
  return (
    <section
      aria-label="Report"
      id={`report-${latest.assignmentId}`}
      tabIndex={-1}
      data-report
      className="min-w-0 rounded-md border border-line bg-surface outline-none focus-visible:outline-2"
    >
      <header className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 border-b border-line-soft px-4 py-2.5">
        <h2 className="sr-only">Report</h2>
        <Pill state={latest.state} ended={ended} attention={needsYou && !ended} />
        <Meta report={latest} repo={repo} />
      </header>
      <div className="px-4 py-3">
        {latest.summary.trim() ? <Summary key={latest.assignmentId} text={latest.summary} /> : <p className="text-muted">No summary.</p>}
      </div>
      {question && (
        <div
          role="note"
          aria-label="Question"
          className="mx-4 mb-3 flex min-w-0 flex-wrap items-center gap-3 rounded-xs border-l-[3px] border-l-accent bg-surface-2 px-3 py-2"
        >
          <p dir="auto" className="min-w-0 flex-1 font-semibold break-words wrap-anywhere">
            {stripAnsi(question)}
          </p>
          {onReply && (
            <Button size="sm" variant="primary" onClick={onReply}>
              Reply
            </Button>
          )}
        </div>
      )}
      {earlier.length > 0 && <Earlier reports={earlier} repo={repo} />}
    </section>
  );
}
