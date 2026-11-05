// The viewer's text helpers, kept pure: a file's lines, JSON pretty-printed, a small CSV as rows, and whether text read
// as UTF-8 is really binary.

/** The lines of text: CRLF is one break, and a final newline ends the last line rather than starting one. */
export function textLines(text: string): string[] {
  const lines = text.replace(/\r\n?/g, "\n").split("\n");
  if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();
  return lines;
}

/** JSON nested deeper than this is shown raw. */
export const JSON_DEPTH = 64;
/** Pretty-printed JSON longer than this is shown raw. */
export const JSON_PRETTY_MAX = 2 * 1024 * 1024;

/** The deepest nesting of text's arrays and objects, outside strings: one linear pass, stopping past limit. */
function jsonDepth(text: string, limit: number): number {
  let depth = 0;
  let max = 0;
  let inString = false;
  for (let i = 0; i < text.length; i++) {
    const c = text.charCodeAt(i);
    if (inString) {
      if (c === 92) i++; // a backslash escapes the next character
      else if (c === 34) inString = false;
    } else if (c === 34) inString = true;
    else if (c === 91 || c === 123) {
      if (++depth > max && (max = depth) > limit) return max;
    } else if (c === 93 || c === 125) depth--;
  }
  return max;
}

class TooLarge extends Error {}

/** v as JSON indented two spaces, into out; throws TooLarge once out passes max characters. */
function pretty(v: unknown, indent: string, out: { parts: string[]; length: number }, max: number): void {
  const put = (s: string) => {
    out.parts.push(s);
    out.length += s.length;
    if (out.length > max) throw new TooLarge();
  };
  if (v === null || typeof v !== "object") {
    put(JSON.stringify(v) ?? "null");
    return;
  }
  const inner = indent + "  ";
  const entries: [string | null, unknown][] = Array.isArray(v) ? v.map((e) => [null, e]) : Object.entries(v as Record<string, unknown>);
  const [open, close] = Array.isArray(v) ? ["[", "]"] : ["{", "}"];
  if (entries.length === 0) {
    put(open + close);
    return;
  }
  put(open);
  entries.forEach(([k, e], i) => {
    put((i ? ",\n" : "\n") + inner + (k === null ? "" : JSON.stringify(k) + ": "));
    pretty(e, inner, out, max);
  });
  put("\n" + indent + close);
}

/**
 * text indented two spaces when it parses as JSON, else undefined; undefined too when it nests deeper than
 * JSON_DEPTH or would pass JSON_PRETTY_MAX characters (a small file of brackets grows with the square of its depth).
 */
export function prettyJson(text: string): string | undefined {
  if (jsonDepth(text, JSON_DEPTH) > JSON_DEPTH) return undefined;
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return undefined;
  }
  const out = { parts: [] as string[], length: 0 };
  try {
    pretty(value, "", out, JSON_PRETTY_MAX);
  } catch (e) {
    if (e instanceof TooLarge) return undefined;
    throw e;
  }
  return out.parts.join("");
}

/** A CSV wider than this, or with more cells in all, is shown as text. */
export const CSV_COLUMNS = 50;
export const CSV_CELLS = 10_000;

/**
 * text's CSV rows (RFC 4180: quoted cells may hold commas, line breaks and doubled quotes), or undefined past maxRows,
 * CSV_COLUMNS in a row or CSV_CELLS in all: a large file is shown as text instead. It stops at the first cell past a
 * cap, so a megabyte of commas costs one short pass.
 */
export function parseCsv(text: string, maxRows = Infinity): string[][] | undefined {
  const rows: string[][] = [];
  let row: string[] = [];
  let cell = "";
  let quoted = false;
  let cells = 0;
  let i = 0;
  /** Adds the cell; false past a cap. */
  const push = () => {
    row.push(cell);
    cell = "";
    return row.length <= CSV_COLUMNS && ++cells <= CSV_CELLS;
  };
  const endRow = () => {
    const ok = push();
    rows.push(row);
    row = [];
    return ok;
  };
  while (i < text.length) {
    const c = text[i];
    if (quoted) {
      if (c === '"' && text[i + 1] === '"') {
        cell += '"';
        i += 2;
        continue;
      }
      if (c === '"') quoted = false;
      else cell += c;
      i++;
      continue;
    }
    if (c === '"' && cell === "") quoted = true;
    else if (c === ",") {
      if (!push()) return undefined;
    } else if (c === "\n" || c === "\r") {
      if (c === "\r" && text[i + 1] === "\n") i++;
      if (!endRow() || rows.length > maxRows) return undefined;
    } else cell += c;
    i++;
  }
  if ((cell !== "" || row.length > 0) && !endRow()) return undefined;
  return rows.length > maxRows ? undefined : rows;
}

/** Whether text holds NUL characters, or is more than a tenth replacement characters: bytes that are not text. */
export function looksBinary(text: string): boolean {
  const sample = text.slice(0, 8192);
  if (sample.includes("\u0000")) return true;
  let bad = 0;
  for (const c of sample) if (c === "�") bad++;
  return sample.length > 0 && bad / sample.length > 0.1;
}
