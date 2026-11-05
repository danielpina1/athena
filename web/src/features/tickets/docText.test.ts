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
