// One Ticket: its header with the actions, what it waits on (the Needs you cards, inline), its facts and its tabs:
// Session (Task 12), Timeline, and Changes (a Ticket with a branch) or Files (one without a repository). The Ticket's
// detail Deltas come from useWatch for as long as the page is open.
import { ArrowLeftIcon } from "lucide-react";
import { lazy, Suspense, useRef, useState, type KeyboardEvent } from "react";
import { Link, useLocation, useSearchParams } from "react-router";
import type { ApprovalView, TicketSummary } from "@/gen/athena/v1/state_pb";
import { AnswerCard } from "@/features/needs-you/AnswerCard";
import { ApprovalCard } from "@/features/needs-you/ApprovalCard";
import { stopTicket } from "@/features/needs-you/stopTicket";
import { useShortcut } from "@/app/shortcuts";
import { errorMessage, interrupt, reply, retry } from "@/lib/commands/actions";
import { useWatch } from "@/lib/commands/useWatch";
import { useAthena } from "@/lib/store/state";
import { stripAnsi } from "@/lib/text/ansi";
import { Confirm } from "@/ui/athena/Confirm";
import { Mono } from "@/ui/athena/Mono";
import { StageBadge } from "@/ui/athena/StageBadge";
import { Button } from "@/ui/primitives/button";
import { Skeleton } from "@/ui/primitives/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/ui/primitives/tabs";
import { Textarea } from "@/ui/primitives/textarea";
import { toast } from "@/ui/primitives/toast";
import { ListSkeleton } from "./ArtifactFailure";
import { Facts } from "./Facts";
import { filterSearch } from "./Filters";
import { FilesView } from "./FilesView";
import { Timeline } from "./Timeline";

// The diff viewer is outside the initial bundle budget: its chunk loads when the Changes tab opens.
const DiffView = lazy(() => import("./DiffView"));

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

function Reply({ ticketId, onSent, onCancel }: { ticketId: string; onSent(): void; onCancel(): void }) {
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const busy = useRef(false);
  const send = async () => {
    const t = text.trim();
    if (!t || busy.current) return;
    busy.current = true;
    setSending(true);
    const r = await reply(ticketId, t).finally(() => {
      busy.current = false;
    });
    setSending(false);
    if (r.ok) onSent();
    else toast({ title: "Reply not sent", description: errorMessage(r.code), tone: "danger" });
  };
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key !== "Enter" || e.shiftKey || e.metaKey || e.ctrlKey || e.nativeEvent.isComposing) return;
    e.preventDefault();
    if (!e.repeat) void send();
  };
  return (
    <div className="grid gap-2">
      <Textarea
        aria-label="Your reply"
        autoFocus
        rows={2}
        value={text}
        disabled={sending}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKeyDown}
        placeholder="Enter sends, Shift+Enter adds a line"
      />
      <div className="grid grid-cols-2 gap-3">
        <Button onClick={onCancel} disabled={sending}>
          Cancel
        </Button>
        <Button variant="primary" onClick={() => void send()} disabled={sending || text.trim() === ""}>
          Send reply
        </Button>
      </div>
    </div>
  );
}

function Header({ ticket, back }: { ticket: TicketSummary; back: string }) {
  const stopping = useAthena((s) => s.stopping.has(ticket.id));
  // The reply box is closed, open (a fresh one each time Reply is pressed), or done.
  const [replyBox, setReply] = useState<"closed" | "open" | "sent">("closed");
  const ended = ticket.stage === "ended";
  const working = ticket.stage === "working";

  const run = async (done: string, failed: string, send: Promise<{ ok: boolean; code: string }>) => {
    const r = await send;
    if (r.ok) toast({ title: done });
    else toast({ title: failed, description: errorMessage(r.code), tone: "danger" });
  };
  const doInterrupt = () => void run("Interrupted", "Not interrupted", interrupt(ticket.id));

  const openReply = () => setReply("open");
  // r replies, ⌘⏎ interrupts; a Needs you card in focus keeps its own r.
  useShortcut("r", openReply, {
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
          <Button onClick={openReply} aria-keyshortcuts="r">
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
          <Button onClick={() => void run("Retrying", "Not retried", retry(ticket.id))}>Retry</Button>
        )}
        {working && (
          <Button onClick={doInterrupt} aria-keyshortcuts="Meta+Enter Control+Enter">
            Interrupt
          </Button>
        )}
      </div>
      {replyBox === "open" && <Reply ticketId={ticket.id} onSent={() => setReply("sent")} onCancel={() => setReply("closed")} />}
      {replyBox === "sent" && (
        <p role="status" className="border-t border-line pt-3 font-semibold">
          Reply sent
        </p>
      )}
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

function TicketTabs({ ticket }: { ticket: TicketSummary }) {
  const [params, setParams] = useSearchParams();
  const { hash } = useLocation();
  const detail = useAthena((s) => s.details.get(ticket.id));
  // Changes for a Ticket with a branch, Files for one without a repository; a repository not placed yet has neither.
  const third = ticket.branch ? "changes" : ticket.repo === "" ? "files" : undefined;
  const tabs = ["session", "timeline", ...(third ? [third] : [])];
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
        {third === "changes" && <TabsTrigger value="changes">Changes</TabsTrigger>}
        {third === "files" && <TabsTrigger value="files">Files</TabsTrigger>}
      </TabsList>
      <TabsContent value="session" className="grid min-w-0 gap-1">
        {/* Task 12 puts the live session here. */}
        <h2 className="text-lg font-semibold">Session</h2>
        <p className="text-muted">The live session appears here.</p>
      </TabsContent>
      <TabsContent value="timeline" className="min-w-0">
        {detail ? (
          <Timeline entries={detail.timeline} trimmed={detail.timelineTrimmed} />
        ) : (
          <ListSkeleton />
        )}
      </TabsContent>
      {third === "changes" && (
        <TabsContent value="changes" className="min-w-0">
          <Suspense fallback={<ListSkeleton />}>
            <DiffView ticketId={ticket.id} />
          </Suspense>
        </TabsContent>
      )}
      {third === "files" && (
        <TabsContent value="files" className="min-w-0">
          <FilesView ticketId={ticket.id} />
        </TabsContent>
      )}
    </Tabs>
  );
}

export function TicketDetail({ id }: { id: string }) {
  const { lost, resume } = useWatch(id);
  const [params] = useSearchParams();
  const summary = useAthena((s) => s.tickets.get(id) ?? s.details.get(id)?.summary);
  const detail = useAthena((s) => s.details.get(id));
  const loaded = useAthena((s) => s.epoch !== "");
  const approval = useApprovalOf(summary);
  const back = `/tickets${filterSearch(params)}`;

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

  return (
    <article data-testid="ticket-detail" className={COLUMN}>
      <Header ticket={summary} back={back} />
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
      <Facts ticket={summary} threads={detail?.threads ?? []} />
      <TicketTabs ticket={summary} />
    </article>
  );
}
