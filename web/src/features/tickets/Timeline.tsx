// What happened on a Ticket, as a story on a rail: newest first, by day, each event in plain words (timelineModel),
// with its local time on the left and a dot whose tone says how it went. The approval waiting on the owner has the
// pulsing orange dot. Entries that arrive while the page is open wash in. Hostile text is clamped and stripped of
// escapes.
import { ChevronRightIcon } from "lucide-react";
import { useMemo, useState } from "react";
import type { TimelineEntry } from "@/gen/athena/v1/state_pb";
import { stripAnsi } from "@/lib/text/ansi";
import { cn } from "@/ui/cn";
import { Dot } from "@/ui/athena/Dot";
import { Mono } from "@/ui/athena/Mono";
import { useNow } from "@/ui/athena/RelativeTime";
import { Truncate } from "@/ui/athena/Truncate";
import { buildTimeline, clockTime, entryKey, type Row } from "./timelineModel";

const full = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

const link =
  "text-sm font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text";

function When({ at }: { at: number }) {
  return (
    <time
      dateTime={new Date(at).toISOString()}
      title={full.format(at)}
      className="pt-px text-right font-mono text-[11px] leading-[18px] text-muted tabular-nums"
    >
      {clockTime(at)}
    </time>
  );
}

function RowText({ row, onViewReport }: { row: Row; onViewReport?: (assignmentId: string) => void }) {
  const { view } = row;
  if (row.type === "group") {
    return (
      <details className="group/approvals min-w-0">
        <summary className="flex min-h-11 cursor-pointer list-none items-center gap-1 lg:min-h-0 [&::-webkit-details-marker]:hidden">
          <span className="text-sm font-semibold">{view.title}</span>
          <ChevronRightIcon aria-hidden className="size-3.5 text-muted transition-transform group-open/approvals:rotate-90" />
        </summary>
        <ul aria-label="What you approved" className="mt-1 grid gap-0.5">
          {row.items.map((item, i) => (
            <li key={i} className="font-mono text-xs break-all text-muted">
              {stripAnsi(item)}
            </li>
          ))}
        </ul>
      </details>
    );
  }
  return (
    <div className="grid min-w-0 gap-0.5">
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-2">
        <span className="text-sm font-semibold">{view.title}</span>
        {view.badge && <Mono>{view.badge}</Mono>}
      </div>
      {view.subtitle && <Truncate text={stripAnsi(view.subtitle)} lines={2} className="text-sm text-muted" />}
      {view.report && onViewReport && (
        <button
          type="button"
          onClick={() => onViewReport(row.entry.refId)}
          className={cn(link, "inline-flex min-h-11 items-center justify-self-start lg:min-h-0")}
        >
          View report
        </button>
      )}
    </div>
  );
}

export function Timeline({
  entries,
  trimmed,
  onViewReport,
}: {
  entries: readonly TimelineEntry[];
  trimmed: boolean;
  /** Shows the Report of an Assignment (the report card's), from a report row's "View report". */
  onViewReport?: (assignmentId: string) => void;
}) {
  // The shared clock: "Today" turns into "Yesterday" at midnight without a new entry.
  const now = useNow();
  const days = useMemo(() => buildTimeline(entries, now), [entries, now]);
  // The entries there at the first render are the history; any other one arrived while the page was open.
  const [initial] = useState(() => new Set(entries.map(entryKey)));
  const fresh = (row: Row) => (row.type === "group" ? row.entries : [row.entry]).some((e) => !initial.has(entryKey(e)));

  return (
    <div className="grid min-w-0 gap-4">
      {trimmed && (
        <p role="note" className="rounded-xs border border-line-soft px-3 py-2 text-sm text-muted">
          Older entries are hidden
        </p>
      )}
      {days.length === 0 ? (
        <p className="text-muted">Nothing has happened on this Ticket yet.</p>
      ) : (
        days.map((day) => (
          <div key={day.key} className="grid min-w-0 gap-2">
            <h3 className="pl-[56px] font-mono text-xs tracking-wider text-muted uppercase">{day.label}</h3>
            <ol className="grid min-w-0">
              {day.rows.map((row, i) => (
                <li
                  key={row.key}
                  data-testid="timeline-row"
                  className={cn(
                    "-mx-2 grid min-w-0 grid-cols-[40px_9px_minmax(0,1fr)] gap-x-3 rounded-xs px-2",
                    fresh(row) && "motion-wash",
                  )}
                >
                  <When at={row.at} />
                  {/* The rail: a 1 px line through the dots, from the first dot to the last. */}
                  <div className="relative flex justify-center">
                    {day.rows.length > 1 && (
                      <span
                        aria-hidden
                        className={cn(
                          "absolute left-1/2 w-px -translate-x-1/2 bg-line-soft",
                          i === 0 ? "top-[9px]" : "top-0",
                          i === day.rows.length - 1 ? "h-[9px]" : "bottom-0",
                        )}
                      />
                    )}
                    <Dot tone={row.view.tone} className="mt-[5px]" />
                  </div>
                  <div className="min-w-0 pb-4">
                    <RowText row={row} onViewReport={onViewReport} />
                  </div>
                </li>
              ))}
            </ol>
          </div>
        ))
      )}
    </div>
  );
}
