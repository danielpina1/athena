import { timestampMs } from "@bufbuild/protobuf/wkt";
import {
  ArrowRightLeftIcon,
  CornerDownLeftIcon,
  FileTextIcon,
  GitForkIcon,
  type LucideIcon,
  MessageSquareIcon,
  ShieldCheckIcon,
  ZapIcon,
} from "lucide-react";
import { useMemo } from "react";
import type { TimelineEntry } from "@/gen/athena/v1/state_pb";
import { stripAnsi } from "@/lib/text/ansi";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { Truncate } from "@/ui/athena/Truncate";

const KINDS: Record<string, { icon: LucideIcon; label: string }> = {
  trigger: { icon: ZapIcon, label: "Trigger" },
  decision: { icon: GitForkIcon, label: "Decision" },
  input: { icon: MessageSquareIcon, label: "Input" },
  report: { icon: FileTextIcon, label: "Report" },
  assignment: { icon: ArrowRightLeftIcon, label: "Assignment" },
  approval: { icon: ShieldCheckIcon, label: "Approval" },
  reply: { icon: CornerDownLeftIcon, label: "Reply" },
};

const ms = (e: TimelineEntry) => (e.at ? timestampMs(e.at) : 0);

/** What happened on a Ticket, oldest first so the newest entry is last. Hostile text is clamped and stripped of escapes. */
export function Timeline({ entries, trimmed }: { entries: readonly TimelineEntry[]; trimmed: boolean }) {
  const sorted = useMemo(() => [...entries].sort((a, b) => ms(a) - ms(b)), [entries]);
  return (
    <div className="grid min-w-0 gap-3">
      {trimmed && (
        <p role="note" className="rounded-xs border border-line-soft px-3 py-2 text-sm text-muted">
          Older entries are hidden
        </p>
      )}
      {sorted.length === 0 ? (
        <p className="text-muted">Nothing has happened on this Ticket yet.</p>
      ) : (
        <ol className="grid min-w-0 gap-3">
          {sorted.map((e, i) => {
            const kind = KINDS[e.kind];
            const Icon = kind?.icon ?? FileTextIcon;
            return (
              <li key={`${i}:${e.refId}:${ms(e)}`} className="grid min-w-0 grid-cols-[1rem_1fr] gap-x-3 gap-y-1">
                <Icon aria-hidden className="mt-0.5 size-4 text-muted" />
                <div className="grid min-w-0 gap-1">
                  <div className="flex min-w-0 flex-wrap items-baseline gap-x-2">
                    <span className="text-sm font-semibold">{kind?.label ?? e.kind}</span>
                    <Mono>
                      <RelativeTime at={ms(e)} />
                    </Mono>
                  </div>
                  <Truncate text={stripAnsi(e.text)} lines={6} breaks />
                </div>
              </li>
            );
          })}
        </ol>
      )}
    </div>
  );
}
