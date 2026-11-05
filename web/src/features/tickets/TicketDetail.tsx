// One Ticket: its header with the actions, what it waits on (the Needs you cards, inline), its latest Report (the
// report card), its facts, the Markdown documents it made (the Documents strip) and its tabs:
// Session (the live session and its composer), Timeline, Changes (a Ticket with a branch) and Files (what its directory
// holds, or what its worktree added or changed). ?file=<path> opens a file in the viewer, over the page. The Ticket's detail Deltas come from useWatch for as long as the page is open. The header's Reply (and
// r) opens the Session tab and puts the cursor in the composer; replying is the composer's.
import { ArrowLeftIcon } from "lucide-react";
import { lazy, Suspense, useCallback, useState } from "react";
import type { ReportView } from "@/gen/athena/v1/state_pb";
import { Link, useLocation, useSearchParams } from "react-router";
import type { ApprovalView, TicketSummary } from "@/gen/athena/v1/state_pb";
import { AnswerCard } from "@/features/needs-you/AnswerCard";
import { ApprovalCard } from "@/features/needs-you/ApprovalCard";
import { stopTicket } from "@/features/needs-you/stopTicket";
import { useShortcut } from "@/app/shortcuts";
import { Composer, interruptSession } from "@/features/session/Composer";
import { PinToggle } from "@/features/session/PinnedSessions";
import { LazySessionView } from "@/features/session/SessionFrame";
import { errorMessage, retry } from "@/lib/commands/actions";
import { type Artifact, type TicketFiles, useArtifact } from "@/lib/artifacts/api";
import { useWatch } from "@/lib/commands/useWatch";
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { stripAnsi } from "@/lib/text/ansi";
import { Confirm } from "@/ui/athena/Confirm";
import { Mono } from "@/ui/athena/Mono";
import { StageBadge } from "@/ui/athena/StageBadge";
import { Button } from "@/ui/primitives/button";
import { Skeleton } from "@/ui/primitives/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/ui/primitives/tabs";
import { toast } from "@/ui/primitives/toast";
import { ListSkeleton } from "./ArtifactFailure";
import { DocumentsStrip } from "./DocumentsStrip";
import { Facts } from "./Facts";
import { filterSearch } from "./Filters";
import { FilesView } from "./FilesView";

// The diff viewer is outside the initial bundle budget: its chunk loads when the Changes tab opens.
const DiffView = lazy(() => import("./DiffView"));
// The report card renders Markdown, which shares its chunk with the session view. The timeline is a chunk of its own.
const ReportCard = lazy(() => import("./ReportCard"));
const Timeline = lazy(() => import("./Timeline").then((m) => ({ default: m.Timeline })));
// The file viewer, a chunk of its own; it shares the Markdown renderer's.
const DocViewer = lazy(() => import("./DocViewer"));

/** Whether a Ticket has files to list: a Ticket without a repository, or one with a branch (its worktree). */
const hasFiles = (t: TicketSummary) => !!t.branch || t.repo === "";

/**
 * A number that grows when a Report arrives while the page is open: the files are read again then (a report is when
 * a worker has written its documents). The first Reports seen, when the detail loads, do not count.
 */
function useReportsSeen(reports: readonly ReportView[] | undefined): number {
  const last = reports?.at(-1);
  const key = last ? `${last.assignmentId}:${last.state}:${last.at?.seconds ?? 0}` : reports ? "none" : undefined;
  const [seen, setSeen] = useState<{ key?: string; bumps: number }>({ key, bumps: 0 });
  if (key !== undefined && key !== seen.key) setSeen({ key, bumps: seen.key === undefined ? seen.bumps : seen.bumps + 1 });
  return seen.bumps;
}

/** Brings an Assignment's Report into view: the card's, or an earlier one's, opened. */
function revealReport(assignmentId: string) {
  const el = document.getElementById(`report-${assignmentId}`);
  if (!el) return;
  const fold = el.closest("details");
  if (fold) fold.open = true;
  const still = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? true;
  el.scrollIntoView?.({ block: "start", behavior: still ? "auto" : "smooth" });
  el.focus({ preventScroll: true });
}

const COLUMN = "grid min-w-0 max-w-full content-start gap-4 p-4 lg:p-6";

/** The approval the Ticket waits on, or the one the owner is answering: its card says "Approved" until the Delta lands. */
function useApprovalOf(ticket: TicketSummary | undefined): ApprovalView | undefined {
  return useAthena((s) => {
    if (!ticket) return undefined;
    const pending = ticket.pendingApprovalId ? s.approvals.get(ticket.pendingApprovalId) : undefined;
    if (pending) return pending;
    for (const id of s.answering.keys()) {
      const a = s.approvals.get(id);
      if (a?.ticketId === ticket.id) return a;
    }
    return undefined;
  });
}

function Header({ ticket, back, onReply }: { ticket: TicketSummary; back: string; onReply(): void }) {
  const stopping = useAthena((s) => s.stopping.has(ticket.id));
  const ended = ticket.stage === "ended";
  const working = ticket.stage === "working";

  const run = async (done: string, failed: string, send: Promise<{ ok: boolean; code: string }>) => {
    const r = await send;
    if (r.ok) toast({ title: done });
    else toast({ title: failed, description: errorMessage(r.code), tone: "danger" });
  };
  const doInterrupt = () => void interruptSession(ticket.id);

  // r replies, ⌘⏎ interrupts; a Needs you card in focus keeps its own r.
  useShortcut("r", onReply, {
    when: () => !ended && !document.activeElement?.closest("[data-needs-you-card]"),
  });
  useShortcut("mod+Enter", doInterrupt, { when: () => working });

  return (
    <header className="grid gap-3">
      <Link
        to={back}
        className="inline-flex min-h-11 items-center gap-1 self-start text-sm font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text lg:hidden"
      >
        <ArrowLeftIcon aria-hidden className="size-4" />
        Tickets
      </Link>
      <h1 dir="auto" className="min-w-0 text-xl font-semibold break-words wrap-anywhere">
        {stripAnsi(ticket.title) || "Untitled"}
      </h1>
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
        <Mono tone="default" className="break-all">
          {ticket.id}
        </Mono>
        <StageBadge stage={ticket.stage} action={ticket.action} outcome={ticket.outcome} stopping={stopping} />
      </div>
      <div className="flex flex-wrap gap-2">
        {!ended && (
          // The view's one primary action: Reply, unless the Ticket is stalled and Retry is what it needs.
          <Button variant={ticket.canRetry ? "outline" : "primary"} onClick={onReply} aria-keyshortcuts="r">
            Reply
          </Button>
        )}
        {!ended && (
          <Confirm
            label="Stop"
            confirmLabel="Stop this Ticket"
            disabled={stopping}
            onConfirm={() => void stopTicket(ticket.id, "Stopped from the Ticket page")}
          />
        )}
        {ticket.canRetry && (
          <Button variant="primary" onClick={() => void run("Retrying", "Not retried", retry(ticket.id))}>
            Retry
          </Button>
        )}
        {working && (
          <Button onClick={doInterrupt} aria-keyshortcuts="Meta+Enter Control+Enter">
            Interrupt
          </Button>
        )}
        <PinToggle ticketId={ticket.id} />
      </div>
    </header>
  );
}

function DetailSkeleton() {
  return (
    <div data-testid="ticket-detail" aria-busy="true" className={COLUMN}>
      <Skeleton className="h-7 w-2/3" />
      <Skeleton className="h-5 w-1/3" />
      <div className="grid gap-2">
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-4 w-5/6" />
        <Skeleton className="h-4 w-2/3" />
      </div>
      <Skeleton className="h-11 w-full" />
    </div>
  );
}

function TicketTabs({
  ticket,
  files,
  focusComposer,
  onComposerFocused,
}: {
  ticket: TicketSummary;
  files: Artifact<TicketFiles>;
  focusComposer: boolean;
  onComposerFocused(): void;
}) {
  const [params, setParams] = useSearchParams();
  const { hash } = useLocation();
  const detail = useAthena((s) => s.details.get(ticket.id));
  // Changes for a Ticket with a branch, Files for one with a branch or without a repository; a repository not placed
  // yet has neither.
  const changes = !!ticket.branch;
  const listed = hasFiles(ticket);
  const tabs = ["session", "timeline", ...(changes ? ["changes"] : []), ...(listed ? ["files"] : [])];
  const requested = params.get("tab") ?? (hash === "#diff" ? "changes" : "timeline");
  const tab = tabs.includes(requested) ? requested : "timeline";
  return (
    <Tabs
      value={tab}
      onValueChange={(v) =>
        setParams(
          (prev) => {
            const next = new URLSearchParams(prev);
            next.set("tab", v);
            return next;
          },
          { replace: true },
        )
      }
    >
      <TabsList aria-label="Ticket">
        <TabsTrigger value="session">Session</TabsTrigger>
        <TabsTrigger value="timeline">Timeline</TabsTrigger>
        {changes && <TabsTrigger value="changes">Changes</TabsTrigger>}
        {listed && <TabsTrigger value="files">Files</TabsTrigger>}
      </TabsList>
      <TabsContent value="session" className="grid min-w-0 gap-3">
        <LazySessionView ticketId={ticket.id} />
        <Composer ticket={ticket} focus={focusComposer} onFocused={onComposerFocused} />
      </TabsContent>
      <TabsContent value="timeline" className="min-w-0">
        {detail ? (
          <Suspense fallback={<ListSkeleton />}>
            <Timeline
              entries={detail.timeline}
              trimmed={detail.timelineTrimmed}
              onViewReport={revealReport}
              question={ticket.stage === "person" && ticket.action === "answer" ? ticket.attentionQuestion : ""}
            />
          </Suspense>
        ) : (
          <ListSkeleton />
        )}
      </TabsContent>
      {changes && (
        <TabsContent value="changes" className="min-w-0">
          <Suspense fallback={<ListSkeleton />}>
            <DiffView ticketId={ticket.id} />
          </Suspense>
        </TabsContent>
      )}
      {listed && (
        <TabsContent value="files" className="min-w-0">
          <FilesView ticketId={ticket.id} files={files} repository={!!ticket.branch} />
        </TabsContent>
      )}
    </Tabs>
  );
}

export function TicketDetail({ id }: { id: string }) {
  const { lost, resume } = useWatch(id);
  const [params, setParams] = useSearchParams();
  const [focusComposer, setFocusComposer] = useState(false);
  const onComposerFocused = useCallback(() => setFocusComposer(false), []);
  const summary = useAthena((s) => s.tickets.get(id) ?? s.details.get(id)?.summary);
  const detail = useAthena((s) => s.details.get(id));
  const loaded = useAthena(selectSynced);
  const approval = useApprovalOf(summary);
  const back = `/tickets${filterSearch(params)}`;
  // The Ticket's files, read once for the Documents strip, the Files tab and the viewer, and again after a new Report.
  const listed = !!summary && hasFiles(summary);
  const files = useArtifact<TicketFiles>(listed ? `/api/tickets/${encodeURIComponent(id)}/files` : "", useReportsSeen(detail?.reports));
  const file = listed ? params.get("file") : null;
  const setFile = useCallback(
    (path: string | null) =>
      setParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          if (path === null) next.delete("file");
          else next.set("file", path);
          return next;
        },
        { replace: true },
      ),
    [setParams],
  );
  const closeFile = useCallback(() => setFile(null), [setFile]);

  if (!summary) {
    if (!loaded) return <DetailSkeleton />;
    return (
      <div data-testid="ticket-detail" className={COLUMN}>
        <div className="grid gap-1">
          <h1 className="text-xl font-semibold">Ticket not found</h1>
          <p className="max-w-prose text-muted">It may have been removed, or the link is wrong.</p>
        </div>
        <Button asChild className="justify-self-start">
          <Link to="/tickets">Back to Tickets</Link>
        </Button>
      </div>
    );
  }

  const reply = () => {
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.set("tab", "session");
        return next;
      },
      { replace: true },
    );
    setFocusComposer(true);
  };

  return (
    <article data-testid="ticket-detail" className={COLUMN}>
      <Header ticket={summary} back={back} onReply={reply} />
      {lost && (
        <div role="status" className="flex flex-wrap items-center gap-3 rounded-xs border border-line px-3 py-2 text-sm">
          <span className="min-w-0 flex-1">Live details stopped for this Ticket.</span>
          <Button size="sm" onClick={resume}>
            Resume
          </Button>
        </div>
      )}
      {summary.stage === "person" && summary.action === "approval" && approval && (
        <ApprovalCard ticket={summary} approval={approval} header={false} />
      )}
      {summary.stage === "person" && summary.action === "answer" && (
        <AnswerCard key={summary.attentionQuestion} ticket={summary} header={false} />
      )}
      {detail && detail.reports.length > 0 && (
        <Suspense fallback={<Skeleton className="h-32 w-full" />}>
          <ReportCard
            reports={detail.reports}
            repo={summary.repo}
            needsYou={summary.stage === "person" && summary.action !== "approval" && summary.action !== "delivery"}
            ended={summary.stage === "ended"}
            showQuestion={!(summary.stage === "person" && summary.action === "answer")}
            onReply={summary.stage === "ended" ? undefined : reply}
          />
        </Suspense>
      )}
      <Facts ticket={summary} threads={detail?.threads ?? []} />
      {listed && <DocumentsStrip files={files} />}
      <TicketTabs ticket={summary} files={files} focusComposer={focusComposer} onComposerFocused={onComposerFocused} />
      {file && (
        <Suspense fallback={null}>
          <DocViewer ticketId={summary.id} file={file} files={files} dir={summary.dir} onClose={closeFile} onResolve={setFile} />
        </Suspense>
      )}
    </article>
  );
}
