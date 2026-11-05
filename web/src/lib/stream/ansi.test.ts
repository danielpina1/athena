import { expect, test } from "vitest";
import { stripAnsi } from "./ansi";

test("stripAnsi removes CSI and OSC", () => {
  expect(stripAnsi("\u001b[1;31mFAIL\u001b[0m pkg/api")).toBe("FAIL pkg/api");
  expect(stripAnsi("\u001b[2K\u001b[1Gprogress")).toBe("progress");
  expect(stripAnsi("\u001b]0;window title\u0007after")).toBe("after");
  expect(stripAnsi("\u001b]8;;https://example.com\u001b\\link\u001b]8;;\u001b\\")).toBe("link");
  expect(stripAnsi("tabs\tand\nnewlines stay")).toBe("tabs\tand\nnewlines stay");
});
