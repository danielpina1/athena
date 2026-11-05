import { act, render, screen } from "@testing-library/react";
import { StrictMode } from "react";
import { create } from "@bufbuild/protobuf";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { TicketDetailSchema } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";
import * as actions from "./actions";
import { useWatch } from "./useWatch";

vi.mock("./actions", () => ({ watch: vi.fn(), unwatch: vi.fn(), onWatchLost: vi.fn() }));

const watch = vi.mocked(actions.watch);
const unwatch = vi.mocked(actions.unwatch);
const onWatchLost = vi.mocked(actions.onWatchLost);
const ok = { ok: true, code: "" };

let lostCb: ((id: string) => void) | undefined;

beforeEach(() => {
  vi.useFakeTimers();
  lostCb = undefined;
  watch.mockResolvedValue(ok as never);
  unwatch.mockResolvedValue(ok as never);
  onWatchLost.mockImplementation((cb) => {
    lostCb = cb;
    return () => {
      lostCb = undefined;
    };
  });
});
afterEach(() => {
  vi.clearAllMocks();
  vi.useRealTimers();
});

function Probe({ id }: { id: string }) {
  const w = useWatch(id);
  return (
    <button type="button" onClick={w.resume}>
      {w.lost ? "lost" : "live"}
    </button>
  );
}
const state = () => screen.getByRole("button").textContent;
const settle = () => act(() => vi.runAllTimersAsync());

test("the first subscriber watches and the last one unwatches", async () => {
  const a = render(<Probe id="T-1" />);
  const b = render(<Probe id="T-1" />);
  await settle();
  expect(watch).toHaveBeenCalledTimes(1);
  expect(watch).toHaveBeenCalledWith("T-1");

  a.unmount();
  await settle();
  expect(unwatch).not.toHaveBeenCalled();
  b.unmount();
  await settle();
  expect(unwatch).toHaveBeenCalledTimes(1);
  expect(unwatch).toHaveBeenCalledWith("T-1");
});

test("a StrictMode double mount sends no extra unwatch and watch pair", async () => {
  const view = render(
    <StrictMode>
      <Probe id="T-2" />
    </StrictMode>,
  );
  await settle();
  expect(watch).toHaveBeenCalledTimes(1);
  expect(unwatch).not.toHaveBeenCalled();
  view.unmount();
  await settle();
  expect(unwatch).toHaveBeenCalledTimes(1);
});

test("another Ticket is watched on its own", async () => {
  const view = render(<Probe id="T-3" />);
  view.rerender(<Probe id="T-4" />);
  await settle();
  expect(watch.mock.calls.map((c) => c[0])).toEqual(["T-3", "T-4"]);
  expect(unwatch.mock.calls.map((c) => c[0])).toEqual(["T-3"]);
  view.unmount();
});

test("a lost watch says so, and resume watches again", async () => {
  const view = render(<Probe id="T-5" />);
  await settle();
  expect(state()).toBe("live");

  act(() => lostCb?.("T-5"));
  expect(state()).toBe("lost");

  watch.mockClear();
  await act(async () => {
    screen.getByRole("button").click();
  });
  expect(watch).toHaveBeenCalledWith("T-5");
  expect(state()).toBe("live");
  view.unmount();
  await settle();
  expect(unwatch).toHaveBeenCalledWith("T-5");
});

test("a loss of another Ticket is not ours", async () => {
  const view = render(<Probe id="T-7" />);
  await settle();
  act(() => lostCb?.("T-other"));
  expect(state()).toBe("live");
  view.unmount();
});

test("a watch the server refuses counts as lost", async () => {
  watch.mockResolvedValue({ ok: false, code: "not_found" } as never);
  const view = render(<Probe id="T-6" />);
  await settle();
  expect(state()).toBe("lost");
  view.unmount();
  await settle();
  expect(unwatch).not.toHaveBeenCalled();
});

test("the last screen to let a Ticket go drops its detail; a watched one keeps it", async () => {
  const detail = (id: string) => create(TicketDetailSchema, { summary: { id } });
  useAthena.setState({ details: new Map([["T-7", detail("T-7")], ["T-8", detail("T-8")]]) });
  const has = (id: string) => useAthena.getState().details.has(id);
  const a = render(<Probe id="T-7" />);
  const b = render(<Probe id="T-7" />);
  const c = render(<Probe id="T-8" />);
  await settle();

  a.unmount();
  await settle();
  expect(has("T-7")).toBe(true); // b still shows it
  b.unmount();
  await settle();
  expect(has("T-7")).toBe(false);
  expect(has("T-8")).toBe(true);

  // A quick move between two screens of the same Ticket keeps it.
  c.unmount();
  render(<Probe id="T-8" />);
  await settle();
  expect(has("T-8")).toBe(true);
  useAthena.getState().reset();
});
