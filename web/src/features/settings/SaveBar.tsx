// A settings block's save bar (reference panel principle 7), and the edit-and-save flow behind it. One bar per block
// (General, GitHub, each prompt part), shown only while the block has changes, a save runs or a save failed. Every
// save is sent against the revision the owner edited from; a stale one comes back `conflict`, which keeps the edits,
// says "Changed on another device" and offers Reload (drop the edits) or Overwrite (send them again on top of the
// fresh revision). Blocks report whether they have unsaved changes to the Unsaved context, which Settings uses to ask
// before the owner navigates away.
import { createContext, useCallback, useContext, useEffect, useId, useRef, useState } from "react";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import { errorMessage } from "@/lib/commands/actions";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";

export type SaveStatus = "idle" | "saving" | "failed" | "conflict" | "saved";

export const CONFLICT = "Changed on another device";

export type SaveBarProps = {
  /** The block's name: the bar is the region "Save <label>". */
  label: string;
  dirty: boolean;
  status: SaveStatus;
  /** Why the last save failed, in the owner's words. */
  error?: string;
  /** Why the changes cannot be saved as they are; Save is disabled while it is set. */
  invalid?: string;
  onSave(): void;
  onDiscard(): void;
  onReload(): void;
  onOverwrite(): void;
  className?: string;
};

/** Whether the bar shows: changes, a save running, or a save that failed. */
export function saveBarVisible(dirty: boolean, status: SaveStatus): boolean {
  return status === "saving" || status === "failed" || status === "conflict" || dirty;
}

export function SaveBar({ label, dirty, status, error, invalid, onSave, onDiscard, onReload, onOverwrite, className }: SaveBarProps) {
  const id = useId();
  if (!saveBarVisible(dirty, status)) return null;
  const saving = status === "saving";
  let message: string;
  let tone = "text-text";
  if (status === "conflict") message = CONFLICT;
  else if (status === "failed") {
    message = error ?? errorMessage("");
    tone = "text-danger";
  } else if (saving) message = "Saving…";
  else if (invalid) {
    message = invalid;
    tone = "text-danger";
  } else message = "Unsaved changes";

  return (
    <div
      role="region"
      aria-label={`Save ${label}`}
      className={cn(
        "sticky bottom-0 z-10 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 border-t border-line bg-surface px-4 py-3 lg:px-6",
        className,
      )}
    >
      <p id={id} role="status" className={cn("min-w-0 flex-1 text-sm font-medium break-words", tone)}>
        {message}
      </p>
      <div className="flex flex-wrap gap-2">
        {status === "conflict" ? (
          <>
            <Button size="sm" onClick={onReload} aria-describedby={id}>
              Reload
            </Button>
            <Button size="sm" variant="primary" onClick={onOverwrite} aria-describedby={id}>
              Overwrite
            </Button>
          </>
        ) : (
          <>
            <Button size="sm" variant="ghost" disabled={saving} onClick={onDiscard}>
              Discard
            </Button>
            <Button size="sm" variant="primary" disabled={saving || !!invalid} onClick={onSave} aria-describedby={id}>
              {status === "failed" ? "Try again" : "Save"}
            </Button>
          </>
        )}
      </div>
    </div>
  );
}

// --- unsaved changes ---

type UnsavedRegistry = { set(id: string, label: string, dirty: boolean): void };

const UnsavedContext = createContext<UnsavedRegistry | null>(null);
export const UnsavedProvider = UnsavedContext.Provider;

/** Tells the enclosing Settings screen whether this block has changes it would lose. */
export function useReportUnsaved(label: string, dirty: boolean): void {
  const registry = useContext(UnsavedContext);
  const id = useId();
  useEffect(() => {
    registry?.set(id, label, dirty);
  }, [registry, id, label, dirty]);
  useEffect(() => () => registry?.set(id, label, false), [registry, id, label]);
}

// --- the edit-and-save flow ---

export type SaveBlockOptions<T> = {
  label: string;
  /** The server's value, undefined until known. */
  server: T | undefined;
  /** The revision the server's value is at. */
  revision: number | undefined;
  equal(a: T, b: T): boolean;
  /** Sends draft as a change from base; resolves with the command's result (never rejects). */
  send(draft: T, base: number): Promise<CommandResult>;
  /** Why draft cannot be saved, if it cannot. */
  validate?(draft: T): string | undefined;
  /**
   * Whether a draft based on base may move to revision without a conflict: the revisions between came from this
   * page's other blocks, so no other device changed anything.
   */
  rebase?(base: number, revision: number): boolean;
};

export type SaveBlock<T> = {
  /** What the block shows: the owner's draft, else the server's value. */
  value: T | undefined;
  edit(next: T | ((current: T) => T)): void;
  dirty: boolean;
  bar: Omit<SaveBarProps, "label" | "className">;
};

/**
 * One block's draft and its save. The draft starts at the first edit, from the revision then current (the base of
 * the save). After a successful save the draft stays on screen until the server's revision moves past the base (the
 * Delta), so the old value never flashes back. A conflict keeps the draft; Reload drops it and Overwrite sends it on
 * top of the current revision.
 */
export function useSaveBlock<T>(opts: SaveBlockOptions<T>): SaveBlock<T> {
  const { label, server, revision, equal, send, validate, rebase } = opts;
  const [draft, setDraft] = useState<{ value: T; base: number } | null>(null);
  const [status, setStatus] = useState<SaveStatus>("idle");
  const [error, setError] = useState<string>();
  /** The revision the last successful save made, when its result says (a Policy save does). */
  const [savedRevision, setSavedRevision] = useState<number>();
  const running = useRef(false);

  // The save landed and the server moved on: show the server's value again. (Adjusted while rendering, as React
  // recommends for state that follows a prop.)
  if (status === "saved" && draft && revision !== undefined && revision !== draft.base) {
    setDraft(null);
    setStatus("idle");
  }
  // The revision moved under an unsaved draft, by this page's own other block: the draft follows it.
  if (status === "idle" && draft && revision !== undefined && revision !== draft.base && rebase?.(draft.base, revision)) {
    setDraft({ value: draft.value, base: revision });
  }

  const value = draft?.value ?? server;
  const dirty = !!draft && server !== undefined && !equal(draft.value, server) && status !== "saved";
  const invalid = draft ? validate?.(draft.value) : undefined;
  useReportUnsaved(label, dirty || status === "conflict" || status === "failed");

  const edit = useCallback(
    (next: T | ((current: T) => T)) => {
      setDraft((d) => {
        const current = d?.value ?? server;
        if (current === undefined || revision === undefined) return d;
        const v = typeof next === "function" ? (next as (c: T) => T)(current) : next;
        // A save just landed and its Delta has not: the next edit starts from it, on the revision that save made.
        const base = d && status !== "saved" ? d.base : status === "saved" ? (savedRevision ?? revision) : revision;
        return { value: v, base };
      });
      setStatus((s) => (s === "saved" || s === "failed" ? "idle" : s));
    },
    [server, revision, status, savedRevision],
  );

  const run = async (value: T, base: number) => {
    if (running.current) return;
    running.current = true;
    setStatus("saving");
    setError(undefined);
    try {
      const r = await send(value, base);
      if (r.ok) {
        setDraft({ value, base });
        setSavedRevision(r.result?.case === "policyRevision" ? r.result.value : undefined);
        setStatus("saved");
      } else if (r.code === "conflict") {
        setStatus("conflict");
      } else {
        setError(errorMessage(r.code));
        setStatus("failed");
      }
    } finally {
      running.current = false;
    }
  };

  const discard = () => {
    setDraft(null);
    setStatus("idle");
    setError(undefined);
  };

  return {
    value,
    edit,
    dirty: dirty || status === "conflict" || status === "failed",
    bar: {
      dirty,
      status,
      error,
      invalid,
      onSave: () => draft && !invalid && void run(draft.value, draft.base),
      onDiscard: discard,
      onReload: discard,
      onOverwrite: () => {
        if (!draft || revision === undefined) return;
        setDraft({ value: draft.value, base: revision });
        void run(draft.value, revision);
      },
    },
  };
}
