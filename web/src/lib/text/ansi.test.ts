import { expect, test } from "vitest";
import { hasBidi, markBidi, stripAnsi } from "./ansi";

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

// U+202A–U+202E, U+2066–U+2069 and U+200E/U+200F/U+061C reorder what follows them ("Trojan Source").
const BIDI = "\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u200e\u200f\u061c";

test("titles and stream text lose bidirectional controls, keeping right-to-left text", () => {
  expect(stripAnsi(`a${BIDI}b`)).toBe("ab");
  expect(stripAnsi("admin\u202e\u2066 // check\u2069\u2066")).toBe("admin // check");
  expect(stripAnsi("שלום")).toBe("שלום");
});

test("markBidi shows each bidirectional control as a visible marker", () => {
  expect(markBidi("if (x\u202e) {}")).toBe("if (x⟪U+202E⟫) {}");
  expect(markBidi(BIDI)).toBe(
    "⟪U+202A⟫⟪U+202B⟫⟪U+202C⟫⟪U+202D⟫⟪U+202E⟫⟪U+2066⟫⟪U+2067⟫⟪U+2068⟫⟪U+2069⟫⟪U+200E⟫⟪U+200F⟫⟪U+061C⟫",
  );
  expect(markBidi("plain שלום")).toBe("plain שלום");
  expect(hasBidi("a\u2067b")).toBe(true);
  expect(hasBidi("a\u061cb")).toBe(true);
  expect(hasBidi("مرحبا")).toBe(false);
  expect(hasBidi("plain שלום")).toBe(false);
});
