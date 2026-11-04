import { cn } from "@/ui/cn";

// A stage label says whom a row waits on and what it needs. Orange fill marks only person stages (they need the
// owner); a running Ticket shows a ring; ended Tickets show their outcome.

const PERSON: Record<string, string> = {
  answer: "Needs your answer",
  approval: "Needs approval",
  review: "Ready for review",
  delivery: "Delivery failed",
  blocked: "Blocked",
  failed: "Failed",
};

const OUTCOME: Record<string, string> = {
  merged: "Merged",
  stopped: "Stopped",
  dropped: "Dropped",
  completed: "Completed",
  failed: "Failed",
  // Not in the stage table: a pull request closed without merging. Said plainly rather than hidden.
  closed: "Closed",
};

export type StageProps = {
  stage: string;
  action?: string;
  outcome?: string;
  /** The owner asked to stop it and it has not ended yet (the store's `stopping`). */
  stopping?: boolean;
};

/** The Global Constraints stage label for a Ticket's stage, action and outcome. */
export function stageLabel({ stage, action = "", outcome = "", stopping = false }: StageProps): string {
  if (stopping && stage !== "ended") return "Stopping…";
  switch (stage) {
    case "person":
      return PERSON[action] ?? "Needs you";
    case "working":
      return action === "queued" ? "Queued" : "Working";
    case "idle":
      return "Idle";
    case "ended":
      return OUTCOME[outcome] ?? "Ended";
    default:
      return stage || "Unknown";
  }
}

const base =
  "inline-flex h-5 max-w-full shrink-0 items-center gap-1 rounded-xs border px-2 text-xs font-semibold whitespace-nowrap";

function tone({ stage, action = "", outcome = "" }: StageProps): string {
  if (stage === "person") return "border-accent bg-accent text-accent-fg";
  if (stage === "working") return action === "queued" ? "border-line-soft text-muted" : "border-line text-text";
  if (stage === "ended") {
    if (outcome === "merged" || outcome === "completed") return "border-ok bg-ok text-bg";
    if (outcome === "failed") return "border-danger text-danger";
  }
  return "border-line-soft text-muted";
}

export function StageBadge({
  stage,
  action = "",
  outcome = "",
  stopping = false,
  className,
}: StageProps & { className?: string }) {
  const running = stage === "working" && action !== "queued";
  return (
    <span data-stage={stage} className={cn(base, tone({ stage, action, outcome }), className)}>
      {running && (
        <span
          data-testid="stage-ring"
          aria-hidden
          className="size-2 shrink-0 rounded-full border-[1.5px] border-current"
        />
      )}
      <span className="truncate">{stageLabel({ stage, action, outcome, stopping })}</span>
    </span>
  );
}
