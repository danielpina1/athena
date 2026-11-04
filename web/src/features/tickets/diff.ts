// A unified diff (git's, as the server cuts it) split into files and lines. Only the DiffView chunk imports this.
import { stripAnsi } from "@/lib/text/ansi";

export type DiffLineKind = "hunk" | "add" | "del" | "ctx" | "note";
export type DiffLine = { kind: DiffLineKind; /** The line without its marker. */ text: string };
export type FileSection = { path: string; binary: boolean; lines: DiffLine[] };

const HEADER = /^diff --git a\/(.+) b\/(.+)$/;

function section(header: string): FileSection {
  const m = HEADER.exec(header);
  return { path: m?.[2] ?? "", binary: false, lines: [] };
}

/** The patch's files in order. Escapes are stripped, so no line can carry a terminal sequence. */
export function parsePatch(patch: string): FileSection[] {
  const out: FileSection[] = [];
  let cur: FileSection | undefined;
  let inHunk = false;
  const lines = stripAnsi(patch).split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  for (const line of lines) {
    if (line.startsWith("diff --git ")) {
      cur = section(line);
      out.push(cur);
      inHunk = false;
      continue;
    }
    if (!cur) continue;
    if (!inHunk) {
      if (line.startsWith("@@")) {
        inHunk = true;
        cur.lines.push({ kind: "hunk", text: line });
      } else if (line.startsWith("rename to ")) {
        cur.path = line.slice("rename to ".length);
      } else if (line.startsWith("+++ b/")) {
        cur.path = line.slice("+++ b/".length);
      } else if (line.startsWith("--- a/") && cur.path === "") {
        cur.path = line.slice("--- a/".length);
      } else if (line.startsWith("Binary files ") || line.startsWith("GIT binary patch")) {
        cur.binary = true;
      }
      continue;
    }
    const marker = line[0];
    const text = line.slice(1);
    if (line.startsWith("@@")) cur.lines.push({ kind: "hunk", text: line });
    else if (marker === "+") cur.lines.push({ kind: "add", text });
    else if (marker === "-") cur.lines.push({ kind: "del", text });
    else if (marker === "\\") cur.lines.push({ kind: "note", text: line.slice(2) });
    else cur.lines.push({ kind: "ctx", text });
  }
  return out;
}
