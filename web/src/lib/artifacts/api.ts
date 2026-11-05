// A Ticket's diff and files, the one place the browser reads over HTTP besides auth (the three GET routes of
// docs/dev/dashboard.md; a file is read to view with ?view=1 on its route). A 401 means the session ended under the open app: sign-in is shown (sessionEnded) and the
// call fails with code "unauthenticated". Nothing is polled: a read happens when a view asks for it, or on Retry.
import { useCallback, useEffect, useState } from "react";
import { sessionEnded } from "@/lib/auth/session";

export type DiffFile = {
  path: string;
  oldPath?: string;
  /** A, M, D, R, C or T. */
  status: string;
  added: number;
  deleted: number;
  binary: boolean;
};

export type Diff = { base: string; branch: string; files: DiffFile[]; patch: string; truncated: boolean };

/**
 * A file of a Ticket. status is set for a Ticket with a repository: A, M, R, C or T (the branch changes it) or "?" (new
 * in the worktree, not committed).
 */
export type TicketFile = { path: string; status?: string; size: number; modTime: string };

export type TicketFiles = { files: TicketFile[]; truncated: boolean };

/** The server's `code` (unauthenticated, not_found, internal), or "network" when the request did not get an answer. */
export class ArtifactError extends Error {
  constructor(
    readonly code: string,
    readonly status = 0,
  ) {
    super(code);
    this.name = "ArtifactError";
  }
}

/** Each segment is escaped, the slashes between them are not: the route takes a path. */
export const fileHref = (ticketId: string, path: string) =>
  `/api/tickets/${encodeURIComponent(ticketId)}/files/${path.split("/").map(encodeURIComponent).join("/")}`;

/** The file to read in the page: text as text/plain (its first 1 MiB), an allowed image as itself. */
export const viewHref = (ticketId: string, path: string) => `${fileHref(ticketId, path)}?view=1`;

/** A GET that answered 2xx, or the ArtifactError it failed with. */
async function request(path: string, signal?: AbortSignal): Promise<Response> {
  let res: Response;
  try {
    res = await fetch(path, { credentials: "same-origin", cache: "no-store", signal });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw new ArtifactError("network");
  }
  if (!res.ok) {
    let code = "internal";
    try {
      code = ((await res.json()) as { code?: string }).code ?? code;
    } catch {
      // not JSON: the status says enough
    }
    if (res.status === 401) {
      code = "unauthenticated";
      sessionEnded();
    }
    throw new ArtifactError(code, res.status);
  }
  return res;
}

export async function getJson<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await request(path, signal);
  try {
    return (await res.json()) as T;
  } catch {
    throw new ArtifactError("internal", res.status);
  }
}

/** A file read to view: its text (the first 1 MiB at most), its whole size, and whether the text was cut. */
export type FileText = { text: string; size: number; truncated: boolean };

export async function getText(path: string, signal?: AbortSignal): Promise<FileText> {
  const res = await request(path, signal);
  try {
    const text = await res.text();
    const size = Number(res.headers.get("X-Athena-Size"));
    return { text, size: Number.isFinite(size) && size > 0 ? size : text.length, truncated: res.headers.get("X-Athena-Truncated") === "true" };
  } catch {
    throw new ArtifactError("internal", res.status);
  }
}

export type Artifact<T> =
  | { status: "loading" }
  | { status: "ready"; data: T; reload?(): void }
  | { status: "error"; error: ArtifactError; retry(): void };

/**
 * Reads path once when mounted (and again for another path or key, or after retry or reload), with read (getJson by
 * default). An empty path reads nothing and stays loading.
 */
export function useArtifact<T>(path: string, key: string | number = 0, read: (path: string, signal: AbortSignal) => Promise<T> = getJson): Artifact<T> {
  const id = `${path}\u0000${key}`;
  const [state, setState] = useState<{ id: string; run: number; result?: { data: T } | { error: ArtifactError } }>({
    id,
    run: 0,
  });
  const retry = useCallback(() => setState((s) => ({ id: s.id, run: s.run + 1 })), []);

  const current = state.id === id ? state : { id, run: 0 };
  useEffect(() => {
    if (!path) return;
    const ctl = new AbortController();
    read(path, ctl.signal).then(
      (data) => {
        if (!ctl.signal.aborted) setState({ id, run: current.run, result: { data } });
      },
      (e: unknown) => {
        if (ctl.signal.aborted) return;
        setState({ id, run: current.run, result: { error: e instanceof ArtifactError ? e : new ArtifactError("internal") } });
      },
    );
    return () => ctl.abort();
    // current.run changes with retry, id with the Ticket or the key; read is a module function.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, current.run]);

  const result = state.id === id ? state.result : undefined;
  if (!result) return { status: "loading" };
  if ("error" in result) return { status: "error", error: result.error, retry };
  return { status: "ready", data: result.data, reload: retry };
}
