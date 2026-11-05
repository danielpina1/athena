import { expect, test } from "vitest";
import { looksBinary, parseCsv, prettyJson, textLines } from "./docText";

test("CSV parses quoted cells, doubled quotes and line breaks inside quotes", () => {
  expect(parseCsv('a,b,c\n1,"two, three","say ""hi"""\r\n4,"five\nlines",6\n')).toEqual([
    ["a", "b", "c"],
    ["1", "two, three", 'say "hi"'],
    ["4", "five\nlines", "6"],
  ]);
  expect(parseCsv("")).toEqual([]);
  // A large file is not a table: the caller falls back to text.
  expect(parseCsv(Array.from({ length: 202 }, (_, i) => `${i},x`).join("\n"), 200)).toBeUndefined();
});

test("JSON is pretty-printed only when it parses", () => {
  expect(prettyJson('{"a":1,"b":[true,null]}')).toBe('{\n  "a": 1,\n  "b": [\n    true,\n    null\n  ]\n}');
  expect(prettyJson('{"a":')).toBeUndefined();
});

test("binary is text with NUL bytes or mostly replacement characters", () => {
  expect(looksBinary("plain text\nwith lines")).toBe(false);
  expect(looksBinary("PK\u0003\u0004\u0000\u0000")).toBe(true);
  expect(looksBinary("����ab")).toBe(true);
  expect(looksBinary("")).toBe(false);
});

test("lines: a final newline ends the last line, CRLF is one break", () => {
  expect(textLines("a\r\nb\n")).toEqual(["a", "b"]);
  expect(textLines("a\n\nb")).toEqual(["a", "", "b"]);
  expect(textLines("")).toEqual([""]);
});

test("a CSV too wide or with too many cells is not a table, and finding out is fast", () => {
  const t0 = performance.now();
  expect(parseCsv(",".repeat(1024 * 1024), 200)).toBeUndefined();
  expect(performance.now() - t0).toBeLessThan(500);
  expect(parseCsv(Array.from({ length: 51 }, (_, i) => `c${i}`).join(","), 200)).toBeUndefined();
  expect(parseCsv(Array.from({ length: 50 }, (_, i) => `c${i}`).join(","), 200)).toHaveLength(1);
  // 200 rows of 50 columns is 10,000 cells: a table; one more row of them is not.
  const row = Array.from({ length: 50 }, () => "x").join(",");
  expect(parseCsv(Array.from({ length: 200 }, () => row).join("\n"), 1000)).toHaveLength(200);
  expect(parseCsv(Array.from({ length: 201 }, () => row).join("\n"), 1000)).toBeUndefined();
});

test("JSON nested too deep, or too large once pretty, stays raw, and finding out is fast", () => {
  const t0 = performance.now();
  expect(prettyJson("[".repeat(5000) + "]".repeat(5000))).toBeUndefined();
  expect(prettyJson("[".repeat(65) + "]".repeat(65))).toBeUndefined();
  expect(prettyJson("[".repeat(64) + "]".repeat(64))).toBeDefined();
  // A string holding brackets does not count as nesting.
  expect(prettyJson(JSON.stringify({ s: "[".repeat(500) }))).toBeDefined();
  // 1 MiB of a flat array is more than 2 MiB pretty.
  const big = "[" + Array.from({ length: 512 * 1024 }, () => "0").join(",") + "]";
  expect(prettyJson(big)).toBeUndefined();
  expect(performance.now() - t0).toBeLessThan(1500);
  expect(prettyJson('{"a":[1,{"b":null}],"c":"x"}')).toBe(JSON.stringify(JSON.parse('{"a":[1,{"b":null}],"c":"x"}'), null, 2));
});
