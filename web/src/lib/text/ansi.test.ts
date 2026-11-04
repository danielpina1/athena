import { expect, test } from "vitest";
import { stripAnsi } from "./ansi";

test("strips colours, cursor moves, OSC titles and stray controls", () => {
  expect(stripAnsi("\u001b[31mred\u001b[0m and \u001b[1;32mgreen\u001b[0m")).toBe("red and green");
  expect(stripAnsi("a\u001b[2K\u001b[1Gb")).toBe("ab");
  expect(stripAnsi("\u001b]0;title\u0007text")).toBe("text");
  expect(stripAnsi("\u001b]8;;http://x\u001b\\link\u001b]8;;\u001b\\")).toBe("link");
  expect(stripAnsi("bell\u0007 nul\u0000 del\u007f")).toBe("bell nul del");
});

test("keeps tabs, newlines and any script", () => {
  expect(stripAnsi("a\tb\nc")).toBe("a\tb\nc");
  expect(stripAnsi("שלום עולם \u001b[4mabc\u001b[0m")).toBe("שלום עולם abc");
});
