// A unified diff (git's, as the server cuts it) split into files and lines. Only the DiffView chunk imports this.
import { hasBidi, markBidi, stripEscapes } from "@/lib/text/ansi";

export type DiffLineKind = "hunk" | "add" | "del" | "ctx" | "note";
export type DiffLine = { kind: DiffLineKind; /** The line without its marker. */ text: string };
/** bidi: the file's name or lines had bidirectional controls, shown as markers (the view warns about it). */
export type FileSection = { path: string; binary: boolean; lines: DiffLine[]; bidi: boolean };

const ESC: Record<string, number> = { a: 7, b: 8, f: 12, n: 10, r: 13, t: 9, v: 11, "\\": 92, '"': 34 };

/**
 * A path as git writes it: bare, or C-quoted ("a/caf\303\251.txt": octal bytes of the UTF-8 name, and \t \n \" \\
 * for the rest). The quotes and escapes are decoded; a bare path is returned as it is.
 */
export function unquotePath(raw: string): string {
  if (raw.length < 2 || !raw.startsWith('"') || !raw.endsWith('"')) return raw;
  const bytes: number[] = [];
  const enc = new TextEncoder();
  const body = raw.slice(1, -1);
  for (let i = 0; i < body.length; i++) {
    const c = body[i];
    if (c !== "\\") {
      bytes.push(...enc.encode(c));
      continue;
    }
    const oct = /^[0-7]{1,3}/.exec(body.slice(i + 1));
    if (oct) {
      bytes.push(parseInt(oct[0], 8) & 255);
      i += oct[0].length;
    } else if (body[i + 1] in ESC) {
      bytes.push(ESC[body[i + 1]]);
      i++;
    } else {
      bytes.push(92);
    }
  }
  return new TextDecoder().decode(new Uint8Array(bytes));
}

const dropPrefix = (path: string, prefix: "a/" | "b/") => (path.startsWith(prefix) ? path.slice(2) : path);

const Q = String.raw`"(?:[^"\\]|\\.)*"`;
const HEADERS = [
  new RegExp(`^(${Q}) (${Q})$`),
  new RegExp(`^(a/.+) (${Q})$`),
  new RegExp(`^(${Q}) (b/.+)$`),
  /^(a\/.+) (b\/.+)$/,
];

function section(header: string): FileSection {
  const rest = header.slice("diff --git ".length);
  for (const re of HEADERS) {
    const m = re.exec(rest);
    if (m) return { path: dropPrefix(unquotePath(m[2]), "b/"), binary: false, lines: [], bidi: false };
  }
  return { path: "", binary: false, lines: [], bidi: false };
}

/**
 * The patch's files in order. Escapes are stripped, so no line can carry a terminal sequence; bidirectional controls
 * are shown as markers, in the lines and the names, and flag their file.
 */
export function parsePatch(patch: string): FileSection[] {
  const out: FileSection[] = [];
  let cur: FileSection | undefined;
  let inHunk = false;
  const lines = stripEscapes(patch).split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  for (const raw of lines) {
    const bidi = hasBidi(raw);
    const line = bidi ? markBidi(raw) : raw;
    if (line.startsWith("diff --git ")) {
      cur = section(line);
      if (bidi) cur.bidi = true;
      out.push(cur);
      inHunk = false;
      continue;
    }
    if (!cur) continue;
    if (bidi) cur.bidi = true;
    if (!inHunk) {
      if (line.startsWith("@@")) {
        inHunk = true;
        cur.lines.push({ kind: "hunk", text: line });
      } else if (line.startsWith("rename to ")) {
        cur.path = unquotePath(line.slice("rename to ".length));
      } else if (line.startsWith("+++ ") && line !== "+++ /dev/null") {
        cur.path = dropPrefix(unquotePath(line.slice(4)), "b/");
      } else if (line.startsWith("--- ") && line !== "--- /dev/null" && cur.path === "") {
        cur.path = dropPrefix(unquotePath(line.slice(4)), "a/");
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
  // A quoted name is decoded only now (git writes its UTF-8 as octal escapes): its controls go, its bidi is shown.
  for (const f of out) {
    const path = stripEscapes(f.path);
    f.path = displayPath(path);
    if (f.path !== path) f.bidi = true;
  }
  return out;
}

/** A path as the diff view shows it: bidirectional controls as markers. The file list uses it too, to match. */
export function displayPath(path: string): string {
  return hasBidi(path) ? markBidi(path) : path;
}
