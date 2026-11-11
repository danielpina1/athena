// The owner's actions, one typed helper per Command. Each goes through the outbox, so it takes effect exactly once and
// resolves with its CommandResult (never rejects: a failure is `{ ok: false, code }`; errorMessage says it to the
// owner). The few with an optimistic change make it at once and undo it on failure; the Deltas that follow are the
// truth either way.
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import type { AthenaState, Optimistic } from "@/lib/store/state";
import type { Action, Outbox, SubmitOptions } from "./outbox";

let current: Outbox | undefined;

/** The outbox the helpers use: the app sets it once at start (and tests per test). */
export function setOutbox(o: Outbox | undefined): void {
  current = o;
}

function submit(action: Action, opts?: SubmitOptions): Promise<CommandResult> {
  if (!current) throw new Error("commands: no outbox; call setOutbox at start");
  return current.submit(action, opts);
}

/** Runs mark as the optimistic change: its undo on failure, its commit on success. */
function undoable(mark: (s: AthenaState) => Optimistic): SubmitOptions {
  let change: Optimistic | undefined;
  return {
    optimistic: (s) => (change = mark(s)),
    rollback: () => change?.undo(),
    commit: () => change?.commit(),
  };
}

/**
 * runtime is claude, codex or auto (empty: the server's configured default). review is sent only when the owner chose
 * it: true pairs the task with a review, false with none, and left out the Policy's default decides.
 */
export function submitTask(t: {
  text: string;
  repo?: string;
  dir?: string;
  model?: string;
  runtime?: string;
  review?: boolean;
  /** A node id to run it on; left out, any node. */
  node?: string;
  /** Hold it while a planner drafts its split into child tickets, for the owner to approve. */
  planFirst?: boolean;
}) {
  const value = {
    text: t.text,
    repo: t.repo ?? "",
    dir: t.dir ?? "",
    model: t.model ?? "",
    runtime: t.runtime ?? "",
    node: t.node ?? "",
    planFirst: t.planFirst ?? false,
  };
  return submit({ case: "submitTask", value: t.review === undefined ? value : { ...value, review: t.review } });
}

type SubTasks = Extract<Action, { case: "updateBreakdown" }>["value"]["tasks"];

/** revision is the draft's (BreakdownView.revision) the owner edited; a draft changed meanwhile is refused. */
export function updateBreakdown(ticketId: string, revision: number, tasks: SubTasks) {
  return submit({ case: "updateBreakdown", value: { ticketId, revision, tasks } });
}

/** Splits the plan-first Ticket into its draft's sub-tasks, when the draft is still at revision. */
export function approveBreakdown(ticketId: string, revision: number) {
  return submit({ case: "approveBreakdown", value: { ticketId, revision } });
}

/** Runs the plan-first Ticket as one ticket. */
export function rejectBreakdown(ticketId: string) {
  return submit({ case: "rejectBreakdown", value: { ticketId } });
}

/** Asks the planner again for the split. */
export function replan(ticketId: string) {
  return submit({ case: "replan", value: { ticketId } });
}

/** Ends the child ticket so its lead and the dependents that waited for it go on without it. */
export function continueWithout(childId: string) {
  return submit({ case: "continueWithout", value: { ticketId: childId } });
}

export function reply(ticketId: string, text: string) {
  return submit({ case: "reply", value: { ticketId, text } });
}

/** Shows the Ticket as "Stopping…" until the Delta that ends it (or until the stop fails). */
export function stop(ticketId: string, reason: string) {
  return submit({ case: "stop", value: { ticketId, reason } }, undoable((s) => s.markStopping(ticketId)));
}

export function retry(ticketId: string) {
  return submit({ case: "retry", value: { ticketId } });
}

/** Retries a Ticket whose node is offline on another node. A Dir Ticket is refused (`invalid`, with the sentence). */
export function retryElsewhere(ticketId: string) {
  return submit({ case: "retryElsewhere", value: { ticketId } });
}

/** How long a join command works: one hour. */
const JOIN_TTL_SECONDS = 3600;

/**
 * A single-use join command for a new node (result `nodeToken`: the command and when it expires). Sensitive: the
 * outbox asks for the passkey and resends. The server ignores a name (R25), so none is sent. The token is in the
 * result only: the caller keeps it in its own state while it shows it, and nowhere else.
 */
export function createNodeToken() {
  return submit({ case: "createNodeToken", value: { ttlSeconds: JOIN_TTL_SECONDS } });
}

/** A draining node finishes its sessions and takes no new ones. */
export function setNodeDraining(nodeId: string, draining: boolean) {
  return submit({ case: "setNodeDraining", value: { nodeId, draining } });
}

/** Revokes a node: its sessions stop and it must join again. Sensitive: the outbox asks for the passkey and resends. */
export function revokeNode(nodeId: string) {
  return submit({ case: "revokeNode", value: { nodeId } });
}

export function interrupt(ticketId: string) {
  return submit({ case: "interrupt", value: { ticketId } });
}

/** Marks the approval answered and clears it from its Ticket at once; a failure puts both back. */
export function answerApproval(approvalId: string, approve: boolean, note = "") {
  return submit(
    { case: "answerApproval", value: { approvalId, approve, note } },
    undoable((s) => s.markApprovalAnswered(approvalId, approve)),
  );
}

export function reevaluate(triggerId: string) {
  return submit({ case: "reevaluate", value: { triggerId } });
}

export function dismiss(decisionId: string) {
  return submit({ case: "dismiss", value: { decisionId } });
}

export function unsubscribe(thread: string) {
  return submit({ case: "unsubscribe", value: { thread } });
}

/** base is the PolicyView revision the owner edited from; a stale one comes back `conflict`. */
export function setPolicy(base: number, json: string) {
  return submit({ case: "setPolicy", value: { baseRevision: base, settingsJson: json } });
}

/** Pauses Athena: sessions stop at the end of their turn, nothing new starts, replies wait. Needs no re-authentication. */
export function pause() {
  return submit({ case: "pause", value: {} });
}

/** Resumes Athena. Sensitive: the outbox asks for the passkey again (setReauthHandler) and resends. */
export function resume() {
  return submit({ case: "resume", value: {} });
}

/** Lets a held reply, push or pull request go. */
export function applyProposal(id: string) {
  return submit({ case: "applyProposal", value: { id } });
}

export function discardProposal(id: string) {
  return submit({ case: "discardProposal", value: { id } });
}

/** Writes a standing allow rule for the offered class. Sensitive: the outbox asks for the passkey and resends. */
export function allowAction(cls: string) {
  return submit({ case: "allowAction", value: { class: cls } });
}

export function dismissOffer(cls: string) {
  return submit({ case: "dismissOffer", value: { class: cls } });
}

/**
 * Starts the Ticket's detail Deltas (seq 0) on this connection. Not refcounted: a screen that can show one Ticket
 * twice (list pane and pinned session, say) counts its own users and calls watch on the first and unwatch on the last.
 * The outbox keeps the Ticket in watched() and re-sends the watch after every reconnect, until unwatch. A watch
 * refused after a reconnect (the Ticket is gone) drops from watched() and fires Outbox.onWatchLost; a refusal of
 * this call resolves its own promise `{ ok: false }`.
 */
export function watch(ticketId: string) {
  return submit({ case: "watchTicket", value: { ticketId } });
}

/** Stops the Ticket's detail Deltas and its re-watch after reconnects. Called by the last user (see watch). */
export function unwatch(ticketId: string) {
  return submit({ case: "unwatchTicket", value: { ticketId } });
}

/**
 * Calls cb with the Ticket's id when a watch the outbox re-sent after a reconnect is refused (the Ticket is gone, or
 * too many are watched). Returns the unsubscribe. useWatch is the one caller.
 */
export function onWatchLost(cb: (ticketId: string) => void): () => void {
  return current?.onWatchLost(cb) ?? (() => {});
}

/** sub is PushSubscription.toJSON(). */
export function pushSubscribe(sub: PushSubscriptionJSON) {
  return submit({
    case: "pushSubscribe",
    value: { endpoint: sub.endpoint ?? "", p256dh: sub.keys?.p256dh ?? "", auth: sub.keys?.auth ?? "" },
  });
}

export function pushUnsubscribe(endpoint: string) {
  return submit({ case: "pushUnsubscribe", value: { endpoint } });
}

/** The notification kinds the owner muted. */
export function setMuted(kinds: string[]) {
  return submit({ case: "setNotificationPrefs", value: { muted: kinds } });
}

/** Whether the dashboard is on screen: no push is sent while it reported visible in the last 30 s. */
export function visibility(visible: boolean) {
  return submit({ case: "visibility", value: { visible } });
}

export function listPasskeys() {
  return submit({ case: "listPasskeys", value: {} });
}

/**
 * thisDevice: the passkey this session signed in with. Its revoke ends the session, so it is dropped rather than
 * replayed if the session ends before its result (Outbox.dropSessionEnders).
 */
export function revokePasskey(id: string, opts: { thisDevice?: boolean } = {}) {
  return submit({ case: "revokePasskey", value: { id } }, { endsSession: opts.thisDevice === true });
}

export function getPrompt(part: string) {
  return submit({ case: "getPrompt", value: { part } });
}

/** base is the PromptsView revision the owner edited from. */
export function setPrompt(part: string, base: number, text: string) {
  return submit({ case: "setPrompt", value: { part, baseRevision: base, text } });
}

export function resetPrompt(part: string, base: number) {
  return submit({ case: "resetPrompt", value: { part, baseRevision: base } });
}

const MESSAGES: Record<string, string> = {
  unauthenticated: "You were signed out. Sign in again to continue.",
  reauth_required: "Confirm with your passkey to make this change.",
  invalid: "That request was not accepted. Check it and try again.",
  not_found: "It is no longer there. It may have ended or been removed.",
  conflict: "Changed on another device. Reload to see the latest.",
  unavailable: "The server is busy. Try again in a moment.",
  internal: "Something went wrong on the server. Try again.",
  // The outbox's own, for a command that ends the session (revoking this device) when the session ended first: sent
  // but unanswered (it may have run), or known not carried out.
  signed_out: "You were signed out before Athena answered. Sign in and check.",
  not_done: "You were signed out first, so it was not done. Sign in and try again.",
  // The outbox's own: a command that waited offline past the server's 24 h memory.
  expired: "It waited offline too long and was not sent. Try again.",
};

/** The plain sentence for a CommandResult code, for a toast or an inline error. */
export function errorMessage(code: string): string {
  return MESSAGES[code] ?? "Something went wrong. Try again.";
}
