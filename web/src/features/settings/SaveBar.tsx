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
  const reload = useRef<HTMLButtonElement>(null);
  const focusHeading = useContext(HeadingFocusContext);
  const visible = saveBarVisible(dirty, status);
  const was = useRef({ status, visible });
  useEffect(() => {
    const before = was.current;
    was.current = { status, visible };
    // Save gave way to Reload and Overwrite: the keyboard goes to Reload, not to the page.
    if (status === "conflict" && before.status !== "conflict") reload.current?.focus();
    // The save is done and the bar went with the focused button: back to the block's heading.
    else if (before.visible && !visible && before.status === "saving") {
      const active = document.activeElement;
      if (!active || active === document.body) focusHeading();
    }
  }, [status, visible, focusHeading]);
  if (!visible) return null;
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
            <Button ref={reload} size="sm" onClick={onReload} aria-describedby={id}>
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

/** Focuses the enclosing section's heading (Section provides it). */
export const HeadingFocusContext = createContext<() => void>(() => {});

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
};

/** A decision sent against a revision (Keep my text, Use the new default, Reset). */
export type BlockAction = (base: number) => Promise<CommandResult>;

export type SaveBlock<T> = {
  /** What the block shows: the owner's draft, else the server's value. */
  value: T | undefined;
  edit(next: T | ((current: T) => T)): void;
  dirty: boolean;
  /** A save or an action is on its way: the block's other buttons wait. */
  busy: boolean;
  /**
   * Sends a decision against the current revision, one at a time (undefined while another runs). A conflict goes to
   * the bar like a save's: Reload drops it, Overwrite sends it again on the fresh revision. Success drops the draft,
   * unless `held` says the decision keeps that value (Keep my text): the editor then holds it, saved, until the
   * server's revision moves, as after a normal save.
   */
  act(action: BlockAction, held?: T): Promise<CommandResult | undefined>;
  bar: Omit<SaveBarProps, "label" | "className">;
};

type Draft<T> = {
  value: T;
  base: number;
  /**
   * The value of this block's own save that the draft was edited on top of, before that save's revision was known
   * (a prompt save returns none). The next revision change that brings exactly this value is that save: the draft
   * follows it instead of conflicting with itself.
   */
  onOwn?: T;
};

/**
 * One block's draft and its save. The draft starts at the first edit, from the revision then current (the base of
 * the save). After a successful save the draft stays on screen until the server's revision moves past the base (the
 * Delta), so the old value never flashes back. A conflict keeps the draft; Reload drops it and Overwrite sends it on
 * top of the current revision.
 */
export function useSaveBlock<T>(opts: SaveBlockOptions<T>): SaveBlock<T> {
  const { label, server, revision, equal, send, validate } = opts;
  const [draft, setDraft] = useState<Draft<T> | null>(null);
  const [status, setStatus] = useState<SaveStatus>("idle");
  const [error, setError] = useState<string>();
  /** The revision the last successful save made, when its result says (a Policy save does). */
  const [savedRevision, setSavedRevision] = useState<number>();
  const running = useRef(false);
  /** The decision a conflict refused, for Overwrite; none when the conflict was a save's. */
  const refused = useRef<BlockAction | null>(null);

  // (Adjusted while rendering, as React recommends for state that follows a prop.)
  if (draft && revision !== undefined && revision !== draft.base) {
    if (status === "saved") {
      // The save landed and the server moved on: show the server's value again.
      setDraft(null);
      setStatus("idle");
    } else if (draft.onOwn !== undefined) {
      // Edited on top of this block's own save: the revision that brings that save's value is ours to build on.
      const ours = server !== undefined && equal(server, draft.onOwn);
      setDraft({ value: draft.value, base: ours ? revision : draft.base });
    }
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
        if (d && status === "saved") {
          // A save just landed and its Delta has not: build on it, on the revision it made when that is known.
          return savedRevision !== undefined ? { value: v, base: savedRevision } : { value: v, base: d.base, onOwn: d.value };
        }
        return d ? { ...d, value: v } : { value: v, base: revision };
      });
      setStatus((s) => (s === "saved" || s === "failed" ? "idle" : s));
    },
    [server, revision, status, savedRevision],
  );

  const begin = () => {
    if (running.current) return false;
    running.current = true;
    setStatus("saving");
    setError(undefined);
    return true;
  };

  const run = async (value: T, base: number) => {
    if (!begin()) return;
    refused.current = null;
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

  const runAction = async (action: BlockAction, base: number, held?: T) => {
    if (!begin()) return undefined;
    try {
      const r = await action(base);
      refused.current = r.ok || r.code !== "conflict" ? null : action;
      if (r.ok && held !== undefined) setDraft({ value: held, base });
      else if (r.ok) setDraft(null);
      // A refusal other than a conflict is the caller's to say (a toast): the bar's Try again is for a draft.
      setStatus(r.ok ? (held !== undefined ? "saved" : "idle") : r.code !== "conflict" ? "idle" : "conflict");
      return r;
    } finally {
      running.current = false;
    }
  };

  const discard = () => {
    refused.current = null;
    setDraft(null);
    setStatus("idle");
    setError(undefined);
  };

  return {
    value,
    edit,
    dirty: dirty || status === "conflict" || status === "failed",
    busy: status === "saving",
    act: (action, held) => (revision === undefined ? Promise.resolve(undefined) : runAction(action, revision, held)),
    bar: {
      dirty,
      status,
      error,
      invalid,
      onSave: () => draft && !invalid && void run(draft.value, draft.base),
      onDiscard: discard,
      onReload: discard,
      onOverwrite: () => {
        if (revision === undefined) return;
        if (refused.current) {
          void runAction(refused.current, revision);
          return;
        }
        if (!draft) return;
        setDraft({ value: draft.value, base: revision });
        void run(draft.value, revision);
      },
    },
  };
}
