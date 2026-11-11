// The breakdown editor (R21): a plan-first Ticket's split as a list of sub-task rows, for the owner to edit, approve or
// reject. A row's key is fixed; its title, instructions, areas (comma-separated), runtime and model can change, and its
// deps are chips of the other keys ("after api"). Two rows whose areas overlap each say so; that warns and never
// blocks. Rows are added up to the draft's max_children. Save sends the rows at the draft's revision; Approve and start
// saves an edited draft first. A draft that changed meanwhile is refused: the editor says so and reloads the store's.
// A failed plan offers Try again and Run as one ticket. A lazy chunk (LAZY_SENTINEL), opened from PlanCard.
import { create } from "@bufbuild/protobuf";
import { PlusIcon, TrashIcon, TriangleAlertIcon } from "lucide-react";
import { useId, useState } from "react";
import { type BreakdownView, SubTaskViewSchema } from "@/gen/athena/v1/state_pb";
import { approveBreakdown, errorMessage, rejectBreakdown, replan, updateBreakdown } from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { stripAnsi } from "@/lib/text/ansi";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { Dialog, DialogDescription, DialogTitle, Modal } from "@/ui/primitives/dialog";
import { Input } from "@/ui/primitives/input";
import { Select } from "@/ui/primitives/select";
import { Textarea } from "@/ui/primitives/textarea";
import { toast } from "@/ui/primitives/toast";

/** bundle-budget.mjs checks this chunk stays out of the initial route. */
export const LAZY_SENTINEL = "athena-lazy-chunk:breakdown-editor";

/** The server's refusal of a draft that changed meanwhile (internal/core/plan.go msgSplitStale). */
const STALE = "The split changed meanwhile; reload it and try again.";
/** internal/core/split.go DefaultMaxChildren, for a view that names no limit. */
const MAX_CHILDREN = 5;

type Row = { key: string; title: string; instructions: string; areas: string; deps: string[]; runtime: string; node: string; model: string };
type Draft = { revision: number; rows: Row[]; dirty: boolean };

const fromView = (v: BreakdownView): Draft => ({
  revision: v.revision,
  dirty: false,
  rows: v.tasks.map((t) => ({
    key: t.key,
    title: t.title,
    instructions: t.instructions,
    areas: t.areas.join(", "),
    deps: [...t.deps],
    runtime: t.runtime,
    node: t.node,
    model: t.model,
  })),
});

const areasOf = (text: string) =>
  text
    .split(",")
    .map((a) => a.trim())
    .filter((a) => a !== "");

/** One area as the server compares it (internal/core/split.go cleanArea). */
const clean = (a: string) => a.trim().replace(/^(\.\/)+/, "").replace(/\/+$/, "");
const overlap = (a: string, b: string) => {
  [a, b] = [clean(a), clean(b)];
  return a !== "" && b !== "" && (a === b || a.startsWith(`${b}/`) || b.startsWith(`${a}/`));
};

/** Each row's overlaps with the others, as internal/core/split.go Overlaps finds them: the earlier row's areas. */
function overlaps(rows: Row[]): Map<string, { other: string; areas: string[] }[]> {
  const out = new Map<string, { other: string; areas: string[] }[]>(rows.map((r) => [r.key, []]));
  const areas = rows.map((r) => areasOf(r.areas));
  rows.forEach((a, i) => {
    rows.slice(i + 1).forEach((b, k) => {
      const hit = areas[i].filter((x) => areas[i + 1 + k].some((y) => overlap(x, y)));
      if (hit.length === 0) return;
      out.get(a.key)!.push({ other: b.key, areas: hit });
      out.get(b.key)!.push({ other: a.key, areas: hit });
    });
  });
  return out;
}

/** A key no row has: sub-<n>, from the row count up. */
function newKey(rows: Row[]): string {
  for (let n = rows.length + 1; ; n++) if (!rows.some((r) => r.key === `sub-${n}`)) return `sub-${n}`;
}

const tasksOf = (rows: Row[]) =>
  rows.map((r) => create(SubTaskViewSchema, { ...r, title: r.title.trim(), instructions: r.instructions.trim(), areas: areasOf(r.areas), model: r.model.trim() }));

const label = "grid min-w-0 gap-1 text-sm font-semibold";

function SubTask({
  row,
  others,
  warnings,
  onChange,
  onRemove,
}: {
  row: Row;
  others: string[];
  warnings: { other: string; areas: string[] }[];
  onChange(next: Row): void;
  onRemove(): void;
}) {
  const set = (field: keyof Row) => (e: { target: { value: string } }) => onChange({ ...row, [field]: e.target.value });
  const toggle = (key: string) =>
    onChange({ ...row, deps: row.deps.includes(key) ? row.deps.filter((d) => d !== key) : [...row.deps, key] });
  return (
    <fieldset data-subtask={row.key} aria-label={`Sub-task ${row.key}`} className="grid min-w-0 gap-3 border-t border-line py-4 first:border-t-0">
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <span className="font-mono text-sm font-semibold break-all">{row.key}</span>
        {others.map((k) => (
          <button
            key={k}
            type="button"
            aria-pressed={row.deps.includes(k)}
            onClick={() => toggle(k)}
            className={cn(
              "inline-flex min-h-11 items-center rounded-xs border border-line px-2 font-mono text-xs lg:min-h-7",
              "hover:bg-surface-2 aria-pressed:border-text aria-pressed:bg-text aria-pressed:text-bg",
            )}
          >
            {`after ${k}`}
          </button>
        ))}
        <Button variant="ghost" size="icon" className="ml-auto" aria-label={`Remove ${row.key}`} onClick={onRemove}>
          <TrashIcon aria-hidden />
        </Button>
      </div>
      <label className={label}>
        Title
        <Input dir="auto" value={row.title} onChange={set("title")} className="font-normal" />
      </label>
      <label className={label}>
        Instructions
        <Textarea dir="auto" rows={2} value={row.instructions} onChange={set("instructions")} className="font-normal" />
      </label>
      <label className={label}>
        Areas
        <Input value={row.areas} onChange={set("areas")} placeholder="web/src, docs" className="font-mono font-normal" />
      </label>
      <div className="grid min-w-0 gap-3 sm:grid-cols-2">
        <label className={label}>
          Runtime
          <Select value={row.runtime} onChange={set("runtime")} className="font-normal">
            <option value="">The lead's</option>
            <option value="auto">Auto</option>
            <option value="claude">Claude Code</option>
            <option value="codex">Codex</option>
          </Select>
        </label>
        <label className={label}>
          Model
          <Input value={row.model} onChange={set("model")} placeholder="Default" className="font-normal" />
        </label>
      </div>
      {warnings.map((w) => (
        <p key={w.other} className="flex min-w-0 items-start gap-2 text-sm">
          <TriangleAlertIcon aria-hidden className="mt-0.5 size-4 shrink-0 text-warning" />
          <span className="min-w-0 wrap-anywhere">{`Overlaps with ${w.other}: ${w.areas.join(", ")}`}</span>
        </p>
      ))}
    </fieldset>
  );
}

export default function BreakdownEditor({ ticketId, onClose }: { ticketId: string; onClose(): void }) {
  const view = useAthena((s) => s.details.get(ticketId)?.breakdown);
  const title = useAthena((s) => s.tickets.get(ticketId)?.title ?? "");
  const [draft, setDraft] = useState<Draft>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const hint = useId();
  // The store's draft, until the owner edits it: a newer revision replaces an untouched one. Revisions only grow, also
  // across Replan, so a draft just saved here (the next revision) waits for its Delta rather than reverting to the old.
  if (view && (!draft || (!draft.dirty && draft.revision < view.revision))) setDraft(fromView(view));

  const rows = draft?.rows ?? [];
  const max = view?.maxChildren || MAX_CHILDREN;
  const warn = overlaps(rows);
  const edit = (next: Row[]) => draft && setDraft({ ...draft, rows: next, dirty: true });

  /** Sends one command; a refusal is said, and a stale draft reloads from the store. */
  const send = async (go: () => ReturnType<typeof replan>) => {
    const r = await go();
    if (r.ok) return true;
    const message = (r.code === "invalid" && r.message) || errorMessage(r.code);
    setError(message);
    const now = useAthena.getState().details.get(ticketId)?.breakdown;
    if (now && (message === STALE || now.revision !== draft?.revision)) setDraft(fromView(now));
    return false;
  };
  const act = async (steps: () => Promise<boolean>, done?: string) => {
    if (busy) return;
    setBusy(true);
    setError("");
    const ok = await steps().finally(() => setBusy(false));
    if (ok && done) {
      toast({ title: done });
      onClose();
    }
  };
  const save = async (d: Draft) => {
    if (!(await send(() => updateBreakdown(ticketId, d.revision, tasksOf(d.rows))))) return undefined;
    // The saved draft is the next revision, so an Approve before its Delta approves it; a newer Delta replaces it.
    const next = { ...d, revision: d.revision + 1, dirty: false };
    setDraft(next);
    return next;
  };
  const approve = async () => {
    if (!draft) return false;
    const d = draft.dirty ? await save(draft) : draft;
    return !!d && send(() => approveBreakdown(ticketId, d.revision));
  };
  const one = () => act(() => send(() => rejectBreakdown(ticketId)), "Running as one ticket");

  const failed = view?.state === "failed";
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <Modal
        data-chunk={LAZY_SENTINEL}
        className={cn(
          "m-0 h-dvh max-h-none w-full max-w-none border-0 open:animate-fade-in",
          "lg:my-0 lg:mr-0 lg:ml-auto lg:w-[min(48rem,62vw)] lg:border-l lg:border-line lg:open:animate-slide-in",
        )}
        bodyClassName="flex h-full min-h-0 flex-col"
      >
        <header className="grid gap-1 border-b border-line p-4 pr-14 lg:px-6">
          <DialogTitle>Review split</DialogTitle>
          <DialogDescription dir="auto" className="wrap-anywhere">
            {failed
              ? stripAnsi(title)
              : `${stripAnsi(title)}: each sub-task runs as a child ticket, after the ones it names.`}
          </DialogDescription>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto px-4 lg:px-6">
          {!view && <p className="py-4 text-muted">Loading the split…</p>}
          {failed && (
            <p dir="auto" className="py-4 wrap-anywhere">
              {stripAnsi(`Planning failed${view.error ? `: ${view.error}` : ""}`)}
            </p>
          )}
          {view && !failed && view.state !== "draft" && <p className="py-4 text-muted">This split is no longer a draft.</p>}
          {view?.state === "draft" && (
            <>
              {rows.map((r, i) => (
                <SubTask
                  key={r.key}
                  row={r}
                  others={rows.filter((o) => o.key !== r.key).map((o) => o.key)}
                  warnings={warn.get(r.key) ?? []}
                  onChange={(next) => edit(rows.map((x, k) => (k === i ? next : x)))}
                  onRemove={() =>
                    edit(rows.filter((x) => x.key !== r.key).map((x) => ({ ...x, deps: x.deps.filter((d) => d !== r.key) })))
                  }
                />
              ))}
              <div className="flex flex-wrap items-center gap-3 border-t border-line py-4">
                <Button
                  onClick={() => {
                    const key = newKey(rows);
                    edit([...rows, { key, title: "", instructions: "", areas: "", deps: [], runtime: "", node: "", model: "" }]);
                  }}
                  disabled={rows.length >= max}
                  aria-describedby={hint}
                >
                  <PlusIcon aria-hidden />
                  Add a sub-task
                </Button>
                <span id={hint} className="text-sm text-muted">{`At most ${max} sub-tasks.`}</span>
              </div>
            </>
          )}
        </div>
        <footer className="grid gap-3 border-t border-line p-4 pb-[max(1rem,env(safe-area-inset-bottom))] lg:px-6">
          {error && (
            <p role="alert" className="text-sm text-danger">
              {error}
            </p>
          )}
          <div className="grid grid-cols-2 gap-2 sm:flex sm:flex-wrap sm:justify-end">
            {failed ? (
              <>
                <Button onClick={() => void one()} disabled={busy}>
                  Run as one ticket
                </Button>
                <Button variant="accent" onClick={() => void act(() => send(() => replan(ticketId)), "Planning again")} disabled={busy}>
                  Try again
                </Button>
              </>
            ) : (
              view?.state === "draft" && (
                <>
                  <Button onClick={() => void act(async () => !!draft && !!(await save(draft)))} disabled={busy || !draft?.dirty}>
                    Save
                  </Button>
                  <Button onClick={() => void one()} disabled={busy}>
                    Run as one ticket
                  </Button>
                  <Button variant="accent" className="col-span-2" onClick={() => void act(approve, "Splitting into child tickets")} disabled={busy}>
                    Approve and start
                  </Button>
                </>
              )
            )}
          </div>
        </footer>
      </Modal>
    </Dialog>
  );
}
