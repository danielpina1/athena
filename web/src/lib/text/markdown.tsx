// A safe subset of Markdown, rendered to React elements: what an agent writes in its prose. Paragraphs, # to ###
// headings (small: they are emphasis in a log, not document headings), - * + and 1. lists with one level of nesting,
// fenced code, > quotes, --- rules, pipe tables, and inline `code`, **bold**, *italic* and [links](url).
//
// Input is hostile. Nothing here makes HTML from it: every piece of text reaches the page as a React text node, so a
// <script> stays the characters "<script>". Links open only for http, https and mailto, in a new tab with no opener;
// any other scheme renders as its label. Every block has dir="auto" and wraps anywhere; code scrolls inside its own
// block. Parsing is linear: a marker with no closer anywhere after it is remembered, so it is not searched for again,
// headings and table rows are read by hand, not by backtracking regexes, and a link's URL is looked for in at most
// 2 KiB.
import { type ReactNode, useMemo } from "react";
import { cn } from "@/ui/cn";

type ListBlock = { t: "list"; ordered: boolean; start: number; items: { text: string; sub?: ListBlock }[] };
type Block =
  | { t: "p"; text: string }
  | { t: "h"; level: number; text: string }
  | { t: "code"; text: string }
  | ListBlock
  | { t: "quote"; blocks: Block[] }
  | { t: "table"; head: string[]; rows: string[][] }
  | { t: "hr" };

const FENCE = /^ {0,3}(`{3,}|~{3,})/;
const HEADING_OPEN = /^ {0,3}(#{1,6})(?:[ \t]|$)/;
const RULE = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
const QUOTE = /^ {0,3}> ?(.*)$/;
const ITEM = /^( *)([-*+]|\d{1,9}[.)])[ \t]+(.*)$/;
const SEP_CELL = /^:?-+:?$/;

const blank = (l: string) => l.trim() === "";
const indentOf = (l: string) => l.length - l.trimStart().length;
/** A heading line's level and text, or null: parsed by hand, so a long run of spaces costs one pass. */
function heading(line: string): { level: number; text: string } | null {
  const m = HEADING_OPEN.exec(line);
  if (!m) return null;
  let text = line.slice(m[0].length).trim();
  // A closing run of #s goes when a space (or nothing) comes before it.
  let j = text.length;
  while (j > 0 && text[j - 1] === "#") j--;
  if (j < text.length && (j === 0 || text[j - 1] === " " || text[j - 1] === "\t")) text = text.slice(0, j).trimEnd();
  return { level: Math.min(m[1].length, 3), text };
}

/** Whether a line is a table's separator row (| --- | :-: |): split and checked cell by cell, in one pass. */
function tableSep(line: string): boolean {
  if (!line.includes("-")) return false;
  return cells(line).every((c) => SEP_CELL.test(c));
}

const cells = (l: string) =>
  l
    .trim()
    .replace(/^\|/, "")
    .replace(/\|$/, "")
    .split("|")
    .map((c) => c.trim());
/** Whether a line ends a paragraph: a numbered item does only when it starts at 1, so "2024. A year" stays text. */
const startsBlock = (l: string) => {
  const item = ITEM.exec(l);
  if (item) return !/\d/.test(item[2]) || parseInt(item[2], 10) === 1;
  return FENCE.test(l) || heading(l) !== null || RULE.test(l) || QUOTE.test(l);
};

function parseList(lines: string[], i: number): [ListBlock, number] {
  const first = ITEM.exec(lines[i])!;
  const base = first[1].length;
  const ordered = /\d/.test(first[2]);
  const list: ListBlock = { t: "list", ordered, start: ordered ? parseInt(first[2], 10) : 1, items: [] };
  while (i < lines.length) {
    const line = lines[i];
    if (blank(line)) {
      // A blank line ends the list unless another item of it follows.
      let j = i + 1;
      while (j < lines.length && blank(lines[j])) j++;
      const next = j < lines.length ? ITEM.exec(lines[j]) : null;
      if (!next || next[1].length < base) break;
      i = j;
      continue;
    }
    const m = ITEM.exec(line);
    const last = list.items[list.items.length - 1];
    if (m && m[1].length >= base + 2 && last) {
      const subOrdered = /\d/.test(m[2]);
      last.sub ??= { t: "list", ordered: subOrdered, start: subOrdered ? parseInt(m[2], 10) : 1, items: [] };
      last.sub.items.push({ text: m[3] });
    } else if (m && m[1].length < base + 2 && /\d/.test(m[2]) === ordered) {
      list.items.push({ text: m[3] });
    } else if (!m && last && indentOf(line) > base) {
      // A continuation line of the last item (or of its last sub-item).
      const into = last.sub?.items[last.sub.items.length - 1] ?? last;
      into.text += "\n" + line.trim();
    } else {
      break;
    }
    i++;
  }
  return [list, i];
}

function parseBlocks(src: string, depth = 0): Block[] {
  const lines = src.replace(/\r\n?/g, "\n").split("\n");
  const out: Block[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (blank(line)) {
      i++;
      continue;
    }
    const fence = FENCE.exec(line);
    if (fence) {
      const close = new RegExp(`^ {0,3}${fence[1][0] === "`" ? "`" : "~"}{${fence[1].length},}\\s*$`);
      const body: string[] = [];
      i++;
      while (i < lines.length && !close.test(lines[i])) body.push(lines[i++]);
      i++;
      out.push({ t: "code", text: body.join("\n") });
      continue;
    }
    const h = heading(line);
    if (h) {
      out.push({ t: "h", level: h.level, text: h.text });
      i++;
      continue;
    }
    if (RULE.test(line)) {
      out.push({ t: "hr" });
      i++;
      continue;
    }
    if (QUOTE.test(line)) {
      const body: string[] = [];
      while (i < lines.length && QUOTE.test(lines[i])) body.push(QUOTE.exec(lines[i++])![1]);
      out.push(depth < 2 ? { t: "quote", blocks: parseBlocks(body.join("\n"), depth + 1) } : { t: "p", text: body.join("\n") });
      continue;
    }
    if (ITEM.test(line)) {
      const [list, next] = parseList(lines, i);
      out.push(list);
      i = next;
      continue;
    }
    if (line.includes("|") && i + 1 < lines.length && lines[i + 1].includes("|") && tableSep(lines[i + 1])) {
      const head = cells(line);
      const rows: string[][] = [];
      i += 2;
      while (i < lines.length && !blank(lines[i]) && lines[i].includes("|")) rows.push(cells(lines[i++]));
      out.push({ t: "table", head, rows });
      continue;
    }
    const para: string[] = [];
    while (i < lines.length && !blank(lines[i]) && (para.length === 0 || !startsBlock(lines[i]))) para.push(lines[i++]);
    out.push({ t: "p", text: para.join("\n") });
  }
  return out;
}

/** Where a link's (…) closes, its own balanced parentheses kept (Wikipedia's Go_(language)); -1 for none nearby. */
function closeParen(text: string, from: number): number {
  let depth = 0;
  const stop = Math.min(text.length, from + 2048);
  for (let k = from; k < stop; k++) {
    const c = text[k];
    if (c === "\n") return -1;
    if (c === "(") depth++;
    else if (c === ")") {
      if (depth === 0) return k;
      depth--;
    }
  }
  return -1;
}

const SAFE_URL = /^(?:https?:\/\/|mailto:)[^\s\u0000-\u001f]+$/i;
const PUNCT = /[\\`*_{}[\]()#+\-.!|>~<]/;

/** text's inline Markdown as React nodes; depth bounds the nesting, and links never nest. */
function inline(text: string, depth = 0, links = true): ReactNode[] {
  const out: ReactNode[] = [];
  let buf = "";
  /** Markers known to have no closer from where they were last looked for on. */
  const missing = new Set<string>();
  const flush = () => {
    if (buf) out.push(buf);
    buf = "";
  };
  const find = (marker: string, from: number) => {
    if (missing.has(marker)) return -1;
    const at = text.indexOf(marker, from);
    if (at < 0) missing.add(marker);
    return at;
  };
  let i = 0;
  while (i < text.length) {
    const c = text[i];
    if (c === "\\" && i + 1 < text.length && PUNCT.test(text[i + 1])) {
      buf += text[i + 1];
      i += 2;
      continue;
    }
    if (c === "`") {
      let n = 1;
      while (text[i + n] === "`") n++;
      const tick = "`".repeat(n);
      const end = find(tick, i + n);
      if (end > i + n) {
        flush();
        out.push(
          <code key={out.length} className="rounded-xs bg-surface-2 px-1 py-px font-mono text-[0.86em] wrap-anywhere">
            {text.slice(i + n, end).trim() || text.slice(i + n, end)}
          </code>,
        );
        i = end + n;
      } else {
        buf += tick;
        i += n;
      }
      continue;
    }
    if (depth < 4 && c === "*" && text[i + 1] === "*" && text[i + 2] && text[i + 2] !== " ") {
      const end = find("**", i + 2);
      if (end > i + 2 && text[end - 1] !== " ") {
        flush();
        out.push(
          <strong key={out.length} className="font-semibold">
            {inline(text.slice(i + 2, end), depth + 1, links)}
          </strong>,
        );
        i = end + 2;
        continue;
      }
      buf += "**";
      i += 2;
      continue;
    }
    if (depth < 4 && c === "*" && text[i + 1] && text[i + 1] !== " " && text[i + 1] !== "*") {
      const end = find("*", i + 1);
      if (end > i + 1 && text[end - 1] !== " ") {
        flush();
        out.push(<em key={out.length}>{inline(text.slice(i + 1, end), depth + 1, links)}</em>);
        i = end + 1;
        continue;
      }
    }
    if (links && depth < 4 && c === "[") {
      const mid = find("](", i + 1);
      const end = mid > i ? closeParen(text, mid + 2) : -1;
      if (mid > i && end > mid && !text.slice(i + 1, mid).includes("[")) {
        flush();
        const label = inline(text.slice(i + 1, mid), depth + 1, false);
        const url = text.slice(mid + 2, end).trim().split(/\s+/)[0].replace(/^<|>$/g, "");
        out.push(
          SAFE_URL.test(url) ? (
            <a
              key={out.length}
              href={url}
              target="_blank"
              rel="noopener noreferrer"
              className="underline decoration-muted underline-offset-2 hover:decoration-text"
            >
              {label}
            </a>
          ) : (
            <span key={out.length}>{label}</span>
          ),
        );
        i = end + 1;
        continue;
      }
    }
    buf += c;
    i++;
  }
  flush();
  return out;
}

const HEAD = ["", "text-[15px] font-semibold", "text-[14px] font-semibold", "text-[14px] font-semibold text-muted"];

function List({ list }: { list: ListBlock }) {
  const items = list.items.map((it, k) => (
    <li key={k} className="pl-1 whitespace-pre-wrap wrap-anywhere">
      {inline(it.text)}
      {it.sub && <List list={it.sub} />}
    </li>
  ));
  const cls = cn("grid gap-1 pl-5 marker:text-muted", list.ordered ? "list-decimal" : "list-disc");
  return list.ordered ? (
    <ol dir="auto" start={list.start === 1 ? undefined : list.start} className={cls}>
      {items}
    </ol>
  ) : (
    <ul dir="auto" className={cls}>
      {items}
    </ul>
  );
}

function BlockView({ block }: { block: Block }): ReactNode {
  switch (block.t) {
    case "p":
      return (
        <p dir="auto" className="whitespace-pre-wrap wrap-anywhere">
          {inline(block.text)}
        </p>
      );
    case "h":
      return (
        <p dir="auto" data-md="heading" className={cn("pt-1 wrap-anywhere", HEAD[block.level])}>
          {inline(block.text)}
        </p>
      );
    case "code":
      return (
        <pre
          tabIndex={0}
          aria-label="Code"
          className="max-w-full overflow-x-auto rounded-md border border-line-soft bg-surface-2 px-3 py-2 font-mono text-xs leading-[1.6] [tab-size:4]"
        >
          {block.text}
        </pre>
      );
    case "list":
      return <List list={block} />;
    case "quote":
      return (
        <blockquote dir="auto" className="grid gap-2 border-l-2 border-line-soft pl-3 text-muted">
          {block.blocks.map((b, k) => (
            <BlockView key={k} block={b} />
          ))}
        </blockquote>
      );
    case "table":
      return (
        <div tabIndex={0} role="group" aria-label="Table" className="max-w-full overflow-x-auto">
          <table dir="auto" className="min-w-full border-collapse text-[13px]">
            <thead>
              <tr>
                {block.head.map((c, k) => (
                  <th key={k} className="border-b border-line px-2 py-1 text-left font-semibold">
                    {inline(c)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {block.rows.map((r, k) => (
                <tr key={k}>
                  {r.map((c, j) => (
                    <td key={j} className="border-b border-line-soft px-2 py-1 align-top wrap-anywhere">
                      {inline(c)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
    case "hr":
      return <hr className="border-line-soft" />;
  }
}

/** text as Markdown: the safe subset above, in the session's prose type (sans, line-height 1.6). */
export function Markdown({ text, className }: { text: string; className?: string }) {
  const blocks = useMemo(() => parseBlocks(text), [text]);
  return (
    <div className={cn("grid min-w-0 gap-2 leading-[1.6]", className)}>
      {blocks.map((b, k) => (
        <BlockView key={k} block={b} />
      ))}
    </div>
  );
}
