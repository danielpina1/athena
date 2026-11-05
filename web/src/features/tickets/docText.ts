// The viewer's text helpers, kept pure: a file's lines, JSON pretty-printed, a small CSV as rows, and whether text read
// as UTF-8 is really binary.

/** The lines of text: CRLF is one break, and a final newline ends the last line rather than starting one. */
export function textLines(text: string): string[] {
  const lines = text.replace(/\r\n?/g, "\n").split("\n");
  if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();
  return lines;
}

/** text indented two spaces when it parses as JSON, else undefined. */
export function prettyJson(text: string): string | undefined {
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return undefined;
  }
}

/**
 * text's CSV rows (RFC 4180: quoted cells may hold commas, line breaks and doubled quotes), or undefined past maxRows:
 * a large file is shown as text instead.
 */
export function parseCsv(text: string, maxRows = Infinity): string[][] | undefined {
  const rows: string[][] = [];
  let row: string[] = [];
  let cell = "";
  let quoted = false;
  let i = 0;
  const endRow = () => {
    row.push(cell);
    rows.push(row);
    row = [];
    cell = "";
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
      row.push(cell);
      cell = "";
    } else if (c === "\n" || c === "\r") {
      if (c === "\r" && text[i + 1] === "\n") i++;
      endRow();
      if (rows.length > maxRows) return undefined;
    } else cell += c;
    i++;
  }
  if (cell !== "" || row.length > 0) endRow();
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
