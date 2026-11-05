import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import { Markdown } from "./markdown";

const md = (text: string) => render(<Markdown text={text} />).container;

test("paragraphs, headings, emphasis and inline code", () => {
  const c = md("# Plan\n\nFirst **bold** and *italic* with `go test ./...` inside.\nSame paragraph.\n\n### Next\nMore");
  const heads = c.querySelectorAll("[data-md=heading]");
  expect([...heads].map((h) => h.textContent)).toEqual(["Plan", "Next"]);
  const p = c.querySelectorAll("p")[1];
  expect(p.querySelector("strong")?.textContent).toBe("bold");
  expect(p.querySelector("em")?.textContent).toBe("italic");
  expect(p.querySelector("code")?.textContent).toBe("go test ./...");
  expect(p.textContent).toBe("First bold and italic with go test ./... inside.\nSame paragraph.");
  expect(p.getAttribute("dir")).toBe("auto");
  // Headings are styled text in the log, not document headings.
  expect(c.querySelector("h1, h2, h3, h4, h5, h6")).toBeNull();
});

test("bullet and numbered lists, one level nested", () => {
  const c = md("Steps:\n- one\n- two\n  - two a\n  - two b\n* three\n\n1. first\n2. second\n   continued");
  const [ul, ol] = c.querySelectorAll(":scope > div > ul, :scope > div > ol");
  expect(ul.tagName).toBe("UL");
  expect([...ul.children].map((li) => li.firstChild?.textContent)).toEqual(["one", "two", "three"]);
  expect([...ul.querySelector("ul")!.querySelectorAll("li")].map((li) => li.textContent)).toEqual(["two a", "two b"]);
  expect(ol.tagName).toBe("OL");
  expect([...ol.children].map((li) => li.textContent)).toEqual(["first", "second\ncontinued"]);
});

test("a numbered list keeps its start", () => {
  const ol = md("3. c\n4. d").querySelector("ol")!;
  expect(ol.getAttribute("start")).toBe("3");
});

test("fenced code is a mono block that scrolls inside itself", () => {
  const c = md("Run:\n\n```bash\nmake check\n  **not bold** <b>x</b>\n```\nafter");
  const pre = c.querySelector("pre")!;
  expect(pre.textContent).toBe("make check\n  **not bold** <b>x</b>");
  expect(pre.querySelector("strong, b")).toBeNull();
  expect(pre.className).toContain("overflow-x-auto");
  expect(pre.className).toContain("font-mono");
  expect(pre.getAttribute("tabindex")).toBe("0");
  expect(c.textContent).toContain("after");
  // An unclosed fence runs to the end.
  expect(md("```\nopen").querySelector("pre")!.textContent).toBe("open");
});

test("links open safely, and only for http, https and mailto", () => {
  const c = md("[docs](https://go.dev/doc) [mail](mailto:a@b.c) [bad](javascript:alert(1)) [data](data:text/html,x) [rel](/etc/passwd)");
  const links = [...c.querySelectorAll("a")];
  expect(links.map((a) => a.getAttribute("href"))).toEqual(["https://go.dev/doc", "mailto:a@b.c"]);
  for (const a of links) {
    expect(a.getAttribute("target")).toBe("_blank");
    expect(a.getAttribute("rel")).toBe("noopener noreferrer");
  }
  expect(c.textContent).toContain("bad");
  expect(c.textContent).not.toContain("javascript:");
});

test("blockquotes and rules", () => {
  const c = md("> quoted **line**\n> two\n\n---\n\nend");
  const q = c.querySelector("blockquote")!;
  expect(q.textContent).toBe("quoted line\ntwo");
  expect(q.querySelector("strong")).toBeTruthy();
  expect(c.querySelector("hr")).toBeTruthy();
});

test("a table reads as a table", () => {
  const c = md("| File | Lines |\n| --- | ---: |\n| a.go | 12 |\n| b.go | 3 |");
  expect([...c.querySelectorAll("th")].map((t) => t.textContent)).toEqual(["File", "Lines"]);
  expect([...c.querySelectorAll("tbody tr")].map((r) => r.textContent)).toEqual(["a.go12", "b.go3"]);
});

test("HTML in the text stays text", () => {
  const c = md('<img src=x onerror="alert(1)"> <script>alert(1)</script> **<i>x</i>**');
  expect(c.querySelector("img, script, i")).toBeNull();
  expect(c.textContent).toContain("<script>alert(1)</script>");
});

test("unmatched markers stay literal, escapes are honoured", () => {
  const c = md("2 * 3 = 6, a ** b, `open, \\*not italic\\*, snake_case_name");
  expect(c.querySelector("em, strong, code")).toBeNull();
  expect(c.textContent).toBe("2 * 3 = 6, a ** b, `open, *not italic*, snake_case_name");
});

test("hostile input renders quickly and wraps", () => {
  const evil = "*".repeat(20_000) + "`".repeat(5_000) + "[".repeat(5_000) + "x".repeat(10_000);
  const t0 = performance.now();
  const c = md(evil);
  expect(performance.now() - t0).toBeLessThan(1000);
  expect(c.textContent!.length).toBeGreaterThan(30_000);
  expect(c.querySelector("p")!.className).toContain("wrap-anywhere");
});

test("whitespace-heavy headings, table rows and quotes stay linear", () => {
  const spaces = " ".repeat(16_000);
  const inputs = [
    "# a" + spaces + "x",
    "# a" + spaces + "#",
    "| a |\n|" + spaces + "-x",
    "|" + spaces + "\n" + "|" + spaces + "-" + spaces + "|",
    "> > > # h" + spaces + "x",
  ];
  for (const text of inputs) {
    const t0 = performance.now();
    md(text);
    expect(performance.now() - t0, text.slice(0, 20)).toBeLessThan(150);
  }
  // A big table: its 2000 rows cost DOM, not parsing (quadratic parsing would take seconds).
  const t0 = performance.now();
  md("| a | b |\n| - | - |\n" + Array.from({ length: 2000 }, () => "| x | y |").join("\n"));
  expect(performance.now() - t0).toBeLessThan(1500);
});

test("links keep balanced parentheses", () => {
  const a = md("See [Go](https://en.wikipedia.org/wiki/Go_(programming_language)) now.").querySelector("a")!;
  expect(a.getAttribute("href")).toBe("https://en.wikipedia.org/wiki/Go_(programming_language)");
  expect(a.parentElement!.textContent).toBe("See Go now.");
});

test("a number in a paragraph does not start a list", () => {
  const c = md("It shipped in\n2024. A year later it was rewritten.");
  expect(c.querySelector("ol")).toBeNull();
  expect(c.querySelector("p")!.textContent).toBe("It shipped in\n2024. A year later it was rewritten.");
  // 1. still interrupts a paragraph, and N.<no space> is no item.
  expect(md("Steps:\n1. one").querySelector("ol")).toBeTruthy();
  expect(md("3.14 is pi").querySelector("ol")).toBeNull();
});

test("a document's headings are real headings, at every level, under the viewer's own", () => {
  const c = render(<Markdown text={"# Report\n\n## Findings\n\n#### Detail\n\n###### Small"} document />).container;
  const heads = [...c.querySelectorAll("h3, h4, h5, h6")];
  expect(heads.map((h) => [h.tagName, h.textContent])).toEqual([
    ["H3", "Report"],
    ["H4", "Findings"],
    ["H6", "Detail"],
    ["H6", "Small"],
  ]);
  expect(c.querySelector("[data-md=heading]")).toBeNull();
});

test("task list items show a box that says whether they are done", () => {
  const c = md("- [x] write the report\n- [ ] send it\n- plain item");
  const items = [...c.querySelectorAll("li")];
  expect(items.map((li) => li.querySelector("[data-task]")?.getAttribute("data-task") ?? null)).toEqual(["done", "open", null]);
  expect(items[0].textContent).toBe("Done: write the report");
  expect(items[1].textContent).toBe("Not done: send it");
  expect(items[2].textContent).toBe("plain item");
});

test("in the log, #### and deeper headings stay at ### size", () => {
  const c = md("### Three\n\n#### Four\n\n###### Six");
  const [three, four, six] = [...c.querySelectorAll("[data-md=heading]")];
  expect(four.className).toBe(three.className);
  expect(six.className).toBe(three.className);
  expect(c.querySelector("h1, h2, h3, h4, h5, h6")).toBeNull();
});
