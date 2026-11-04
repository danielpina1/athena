// A Ticket's diff and files, the one place the browser reads over HTTP besides auth (the three GET routes of
// docs/dev/dashboard.md). A 401 means the session ended under the open app: sign-in is shown (sessionEnded) and the
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

export type TicketFile = { path: string; size: number; modTime: string };

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

export async function getJson<T>(path: string, signal?: AbortSignal): Promise<T> {
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
  try {
    return (await res.json()) as T;
  } catch {
    throw new ArtifactError("internal", res.status);
  }
}

export type Artifact<T> =
  | { status: "loading" }
  | { status: "ready"; data: T }
  | { status: "error"; error: ArtifactError; retry(): void };

/** Reads path once when mounted (and again for another path or after retry). */
export function useArtifact<T>(path: string): Artifact<T> {
  const [state, setState] = useState<{ path: string; run: number; result?: { data: T } | { error: ArtifactError } }>({
    path,
    run: 0,
  });
  const retry = useCallback(() => setState((s) => ({ path: s.path, run: s.run + 1 })), []);

  const current = state.path === path ? state : { path, run: 0 };
  useEffect(() => {
    const ctl = new AbortController();
    getJson<T>(path, ctl.signal).then(
      (data) => setState({ path, run: current.run, result: { data } }),
      (e: unknown) => {
        if (ctl.signal.aborted) return;
        setState({ path, run: current.run, result: { error: e instanceof ArtifactError ? e : new ArtifactError("internal") } });
      },
    );
    return () => ctl.abort();
    // current.run changes with retry, path with the Ticket.
  }, [path, current.run]);

  const result = state.path === path ? state.result : undefined;
  if (!result) return { status: "loading" };
  if ("error" in result) return { status: "error", error: result.error, retry };
  return { status: "ready", data: result.data };
}
