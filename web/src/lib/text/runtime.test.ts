import { expect, test } from "vitest";
import { resetTime, reviewByDefault, runtimeLabel } from "./runtime";

test("runtime labels are the Global Constraints copy, anything else as it is", () => {
  expect(runtimeLabel("claude")).toBe("Claude Code");
  expect(runtimeLabel("codex")).toBe("Codex");
  expect(runtimeLabel("auto")).toBe("Auto");
  expect(runtimeLabel("gemini")).toBe("gemini");
  expect(runtimeLabel("")).toBe("");
});

test("a reset today is its time; another day's has its date", () => {
  const now = new Date(2026, 10, 6, 9, 0).getTime();
  expect(resetTime(new Date(2026, 10, 6, 14, 5).getTime(), now)).toBe("14:05");
  expect(resetTime(new Date(2026, 10, 13, 8, 0).getTime(), now)).toBe("Fri 13 Nov 08:00");
});

test("the review default is the Policy's pairing, off when unknown", () => {
  expect(reviewByDefault('{"mode":"auto","pairing":{"review":"on","maxRounds":2}}')).toBe(true);
  expect(reviewByDefault('{"pairing":{"review":"off"}}')).toBe(false);
  expect(reviewByDefault("{}")).toBe(false);
  expect(reviewByDefault("not json")).toBe(false);
  expect(reviewByDefault(undefined)).toBe(false);
});
