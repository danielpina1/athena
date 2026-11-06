import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import { RuntimeBadge } from "./RuntimeBadge";

const text = (runtime: string, choice: string) => render(<RuntimeBadge runtime={runtime} choice={choice} />).container.textContent;

test("badge shows runtime and auto choice", () => {
  expect(text("codex", "codex")).toBe("Codex");
  expect(text("codex", "auto")).toBe("Codex · auto");
  expect(text("claude", "auto")).toBe("Claude Code · auto");
  // Not started yet: the choice is all there is, said once.
  expect(text("auto", "auto")).toBe("Auto");
  expect(text("", "codex")).toBe("Codex");
  // An older server sends neither.
  expect(text("", "")).toBe("");
  const el = render(<RuntimeBadge runtime="codex" choice="auto" />).container.firstElementChild;
  expect(el?.classList.contains("font-mono")).toBe(true);
});
