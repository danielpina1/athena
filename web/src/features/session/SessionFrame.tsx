// The frame of a live session, in the main bundle: the heading row (the "Session" heading or a pinned session's own
// title, the ● live dot, the actions) and the log's box. The session view itself (SessionView, with its rows, tool
// bodies and Markdown) is a lazy chunk; LazySessionView shows this frame, empty, while it loads, and opens the stream
// at once so the output is there when the view is.
import { lazy, type ReactNode, Suspense } from "react";
import { useAthena } from "@/lib/store/state";
import { useSessionStream } from "@/lib/stream/useSessionStream";
import { cn } from "@/ui/cn";

export type SessionViewProps = {
  ticketId: string;
  /** The pinned form: no heading of its own, and the log fills the space it is given. */
  compact?: boolean;
  /** Replaces the "Session" heading. */
  title?: ReactNode;
  /** Controls at the end of the heading row. */
  actions?: ReactNode;
};

export function LiveDot() {
  return (
    <span className="inline-flex shrink-0 items-center gap-1.5 font-mono text-xs text-text">
      <span aria-hidden className="size-2 animate-pulse rounded-full bg-ok" />
      live
    </span>
  );
}

/** The log's box: sans prose, scrolling inside itself, never sideways. */
export function logClass(compact: boolean): string {
  return cn(
    "min-w-0 overflow-x-hidden overflow-y-auto rounded-md border border-line-soft bg-surface px-3 py-4 font-sans text-body sm:px-5",
    compact ? "min-h-0 flex-1" : "h-[60dvh] min-h-64",
  );
}

export function SessionFrame({ compact = false, title, actions, live, children }: Omit<SessionViewProps, "ticketId"> & { live?: boolean; children: ReactNode }) {
  return (
    <div className={cn("grid min-w-0 gap-2", compact && "flex min-h-0 flex-1 flex-col")}>
      <div className="flex min-h-6 min-w-0 items-center gap-3">
        <div className="min-w-0 flex-1">{title ?? (!compact && <h2 className="text-lg font-semibold">Session</h2>)}</div>
        {live && <LiveDot />}
        {actions}
      </div>
      <div className={cn("relative min-w-0", compact && "flex min-h-0 flex-1 flex-col")}>{children}</div>
    </div>
  );
}

const SessionView = lazy(() => import("./SessionView").then((m) => ({ default: m.SessionView })));

function Loading(props: SessionViewProps) {
  const { live } = useSessionStream(props.ticketId);
  const working = useAthena((s) => (s.tickets.get(props.ticketId) ?? s.details.get(props.ticketId)?.summary)?.stage) === "working";
  return (
    <SessionFrame {...props} live={live && working}>
      <div role="log" aria-label="Session output" aria-busy="true" tabIndex={0} className={logClass(props.compact ?? false)}>
        <p className="text-muted">Loading the session…</p>
      </div>
    </SessionFrame>
  );
}

/** A Ticket's live session (SessionView), loaded on first use. */
export function LazySessionView(props: SessionViewProps) {
  return (
    <Suspense fallback={<Loading {...props} />}>
      <SessionView {...props} />
    </Suspense>
  );
}
