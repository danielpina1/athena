import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { RuntimeStatusSchema } from "@/gen/athena/v1/state_pb";
import { QuotaBars, quotaLine } from "./QuotaBars";

const NOW = new Date(2026, 10, 6, 9, 0).getTime();
const MIN = 60_000;

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
});
afterEach(() => vi.useRealTimers());

const status = (init: MessageInitShape<typeof RuntimeStatusSchema>) =>
  create(RuntimeStatusSchema, { name: "codex", version: "0.160.1", ready: true, tested: true, measured: true, readAt: timestampFromMs(NOW - MIN), ...init });

const win = (name: string, used: number, resetsAt = NOW + 3 * 60 * MIN) => ({ name, used, resetsAt: timestampFromMs(resetsAt) });

const show = (init: MessageInitShape<typeof RuntimeStatusSchema>) => {
  const { container } = render(<QuotaBars status={status(init)} />);
  return { block: screen.getByRole("region", { name: init.name === "claude" ? "Claude Code" : "Codex" }), container };
};

test("one bar per window with its percent and reset, and the version and readiness", () => {
  const { block } = show({ windows: [win("5h", 0.42), win("7d", 0.12, new Date(2026, 10, 13, 8, 0).getTime())] });
  expect(within(block).getByText("0.160.1")).toBeTruthy();
  expect(within(block).queryByText("untested")).toBeNull();
  expect(within(block).getByText("Ready")).toBeTruthy();
  const five = within(block).getByRole("meter", { name: "Codex 5h used" });
  expect(five.getAttribute("aria-valuenow")).toBe("42");
  expect(within(block).getByText("42%")).toBeTruthy();
  expect(within(block).getByText("resets 12:00")).toBeTruthy();
  expect(within(block).getByText("resets Fri 13 Nov 08:00")).toBeTruthy();
  expect(within(block).queryByText(/as of/)).toBeNull();
  expect(within(block).queryByText("Not measured yet")).toBeNull();
});

test("quota bars: unmeasured, stale age, limited, amber at 85", () => {
  // Not measured yet: no bars.
  let { block } = show({ measured: false, readAt: undefined, windows: [] });
  expect(within(block).getByText("Not measured yet")).toBeTruthy();
  expect(within(block).queryByRole("meter")).toBeNull();
  document.body.innerHTML = "";

  // A reading older than 10 minutes says its age.
  ({ block } = show({ readAt: timestampFromMs(NOW - 25 * MIN), windows: [win("5h", 0.3)] }));
  expect(within(block).getByText("as of 25 min ago")).toBeTruthy();
  document.body.innerHTML = "";
  ({ block } = show({ readAt: timestampFromMs(NOW - 9 * MIN), windows: [win("5h", 0.3)] }));
  expect(within(block).queryByText(/as of/)).toBeNull();
  document.body.innerHTML = "";

  // Limited: amber, until when.
  ({ block } = show({ limitedUntil: timestampFromMs(new Date(2026, 10, 6, 14, 30).getTime()), windows: [win("5h", 1)] }));
  const limited = within(block).getByText("Limited until 14:30");
  expect(limited.classList.contains("text-warning")).toBe(true);
  document.body.innerHTML = "";

  // Amber from 85%, ink below.
  ({ block } = show({ windows: [win("5h", 0.85), win("7d", 0.84)] }));
  const fill = (name: string) => within(block).getByRole("meter", { name: `Codex ${name} used` }).firstElementChild;
  expect(fill("5h")?.classList.contains("bg-warning")).toBe(true);
  expect(fill("7d")?.classList.contains("bg-text")).toBe(true);
  expect(within(block).getByText("85%").classList.contains("text-warning")).toBe(true);
  expect(within(block).getByText("84%").classList.contains("text-warning")).toBe(false);
});

test("untested and not ready say so; an empty version says nothing", () => {
  let { block } = show({ version: "0.161.0", tested: false, why: "newer than the verified 0.160.1", windows: [] });
  expect(within(block).getByText("untested").classList.contains("text-warning")).toBe(true);
  expect(within(block).getByText("Ready")).toBeTruthy();
  expect(within(block).getByText("newer than the verified 0.160.1")).toBeTruthy();
  document.body.innerHTML = "";
  ({ block } = show({ name: "claude", version: "", tested: false, windows: [] }));
  expect(within(block).queryByText("untested")).toBeNull();
  document.body.innerHTML = "";
  ({ block } = show({ ready: false, why: "Codex is not logged in: run athena codex login", windows: [] }));
  expect(within(block).queryByText("Ready")).toBeNull();
  expect(within(block).getByText("Codex is not logged in: run athena codex login")).toBeTruthy();
});

test("no orange in quota", () => {
  const states = [
    status({ windows: [win("5h", 0.99), win("7d", 0.5)] }),
    status({ limitedUntil: timestampFromMs(NOW + 60 * MIN), windows: [win("5h", 1)] }),
    status({ measured: false, windows: [] }),
    status({ ready: false, why: "logged out", tested: false, version: "0.170.0", readAt: timestampFromMs(NOW - 60 * MIN), windows: [win("5h", 0.9)] }),
  ];
  for (const s of states) {
    const { container, unmount } = render(<QuotaBars status={s} />);
    expect(container.innerHTML).not.toMatch(/accent/);
    unmount();
  }
});

test("the sidebar's line is the fullest window, or what keeps the runtime from working", () => {
  expect(quotaLine(status({ windows: [win("5h", 0.42), win("7d", 0.12)] }), NOW)).toEqual({ text: "42% · 5h", warn: false });
  expect(quotaLine(status({ windows: [win("5h", 0.2), win("7d", 0.9)] }), NOW)).toEqual({ text: "90% · 7d", warn: true });
  expect(quotaLine(status({ limitedUntil: timestampFromMs(new Date(2026, 10, 6, 14, 30).getTime()), windows: [] }), NOW)).toEqual({
    text: "limited · 14:30",
    warn: true,
  });
  // Another day: its weekday, so the line fits the sidebar.
  expect(quotaLine(status({ limitedUntil: timestampFromMs(new Date(2026, 10, 9, 8, 0).getTime()), windows: [] }), NOW).text).toBe("limited · Mon");
  expect(quotaLine(status({ measured: false, windows: [] }), NOW)).toEqual({ text: "not measured", warn: false });
  expect(quotaLine(status({ ready: false, why: "x", windows: [win("5h", 0.1)] }), NOW)).toEqual({ text: "not ready", warn: true });
});
