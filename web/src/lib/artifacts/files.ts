// What a Ticket's file is, by its name: how the viewer shows it, whether it is a document (listed first, and in the
// Documents strip when it is Markdown), and which listed file a session's absolute path names. In the main bundle: the
// Files tab and the strip use it; the viewer itself is a lazy chunk.
import type { TicketFile } from "./api";

/**
 * How the viewer shows a file: rendered Markdown, numbered lines (code, and plain text), pretty JSON, a CSV table, an
 * image, source text for what is never rendered (SVG and HTML can carry script), Download for a PDF, and for anything
 * else a look at its first bytes.
 */
export type FileKind = "markdown" | "code" | "json" | "csv" | "image" | "source" | "pdf" | "other";

const KINDS: Record<string, FileKind> = { md: "markdown", markdown: "markdown", json: "json", csv: "csv", pdf: "pdf" };
for (const e of ["png", "jpg", "jpeg", "gif", "webp"]) KINDS[e] = "image";
for (const e of ["svg", "html", "htm", "xhtml"]) KINDS[e] = "source";
for (const e of (
  "txt log text yaml yml toml ini cfg conf tsv xml ts tsx js jsx mjs cjs go mod sum py rb rs java kt swift c h cc cpp hpp " +
  "cs php sh bash zsh fish ps1 css scss less sql proto graphql lua pl r scala vue svelte diff patch gitignore lock"
).split(" "))
  KINDS[e] = "code";
/** Names without an extension that are text. */
const TEXT_NAMES = new Set(["makefile", "dockerfile", "readme", "license", "changelog", "authors", "notice", "procfile", "gemfile"]);

export const baseName = (path: string) => path.slice(path.lastIndexOf("/") + 1);

const ext = (path: string) => {
  const name = baseName(path);
  const dot = name.lastIndexOf(".");
  return dot < 0 ? "" : name.slice(dot + 1).toLowerCase();
};

export function fileKind(path: string): FileKind {
  const e = ext(path);
  if (e) return KINDS[e] ?? "other";
  return TEXT_NAMES.has(baseName(path).toLowerCase()) ? "code" : "other";
}

export const isMarkdown = (path: string) => fileKind(path) === "markdown";

/** Prose, tables, PDFs and images: what a report is made of. */
export function isDocument(path: string): boolean {
  const k = fileKind(path);
  return k === "markdown" || k === "csv" || k === "pdf" || k === "image" || ext(path) === "txt";
}

/** The documents, then the rest, each in the order given. */
export function documentsFirst<T extends Pick<TicketFile, "path">>(files: readonly T[]): T[] {
  return [...files.filter((f) => isDocument(f.path)), ...files.filter((f) => !isDocument(f.path))];
}

/** A Ticket's worktree in an absolute path: …/.athena/work/<slug>/. */
const WORKTREE = /\/\.athena\/work\/[^/]+\/(.+)$/;

/**
 * The listed path that file names: itself when listed; for an absolute path (a session's), the part after the
 * Ticket's worktree or its directory dir, else the longest listed path it ends with. undefined when none.
 */
export function resolveFile(file: string, files: readonly Pick<TicketFile, "path">[], dir: string): string | undefined {
  const listed = (p: string) => files.some((f) => f.path === p);
  if (listed(file)) return file;
  if (!file.startsWith("/")) return undefined;
  const inWorktree = WORKTREE.exec(file)?.[1];
  if (inWorktree && listed(inWorktree)) return inWorktree;
  const root = dir.replace(/\/+$/, "") + "/";
  if (dir && file.startsWith(root) && listed(file.slice(root.length))) return file.slice(root.length);
  let best: string | undefined;
  for (const f of files) {
    if (file.endsWith("/" + f.path) && (!best || f.path.length > best.length)) best = f.path;
  }
  return best;
}
