// Render cost: a new batch renders only its own rows; the rows already shown are left alone (up to 2000 of them, in
// up to four views at once).
import { act, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { at, load } from "@/features/tickets/fixtures";
import * as ansi from "@/lib/stream/ansi";
import { fakeStreamLink, item, says, type FakeStreamLink } from "@/lib/stream/testkit";
import { connectStreams } from "@/lib/stream/useSessionStream";
import { useAthena } from "@/lib/store/state";
import { ToolCall } from "./ToolCall";
import { SessionView } from "./SessionView";

vi.mock("@/lib/stream/ansi", async (orig) => {
  const real = await orig<typeof import("@/lib/stream/ansi")>();
  return { ...real, itemText: vi.fn(real.itemText) };
});
vi.mock("./ToolCall", () => ({ ToolCall: vi.fn(() => null), formatDuration: () => "" }));

let link: FakeStreamLink;
let disconnect: () => void;

beforeEach(() => {
  link = fakeStreamLink("open");
  disconnect = connectStreams(link);
  load({ tickets: [{ id: "T-1", title: "Ship", stage: "working", updatedAt: at(0), createdAt: at(0) }] });
});
afterEach(() => {
  disconnect();
  vi.clearAllMocks();
  useAthena.getState().reset();
});

test("a new batch does not re-render the rows already shown", () => {
  const text = vi.mocked(ansi.itemText);
  const tool = vi.mocked(ToolCall);
  render(<SessionView ticketId="T-1" />);
  act(() =>
    link.items("T-1", [
      ...says(50),
      item({ offset: 600n, kind: "tool_call", tool: "Bash", toolUseId: "tu1", text: "ls" }),
      item({ offset: 610n, kind: "tool_call", tool: "Read", toolUseId: "tu2", text: "a.go" }),
    ]),
  );
  expect(text).toHaveBeenCalledTimes(50);
  expect(tool).toHaveBeenCalledTimes(2);
  text.mockClear();
  tool.mockClear();

  act(() => link.items("T-1", says(3, 1000)));
  expect(text).toHaveBeenCalledTimes(3);
  expect(tool).not.toHaveBeenCalled();

  // A result re-renders only its call's row.
  act(() => link.items("T-1", [item({ offset: 2000n, kind: "tool_result", toolUseId: "tu1", text: "ok" })]));
  expect(text).toHaveBeenCalledTimes(3);
  expect(tool).toHaveBeenCalledTimes(1);
  expect(tool.mock.calls[0][0]).toMatchObject({ call: { toolUseId: "tu1" }, result: { text: "ok" } });
});
