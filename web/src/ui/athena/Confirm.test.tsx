import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, test, vi } from "vitest";
import { Confirm } from "@/ui/athena/Confirm";

afterEach(() => vi.useRealTimers());

const setup = () => {
  const onConfirm = vi.fn();
  render(<Confirm label="Stop" confirmLabel="Stop this Ticket" onConfirm={onConfirm} />);
  return onConfirm;
};

test("Confirm requires the second click", async () => {
  const user = userEvent.setup();
  const onConfirm = setup();
  await user.click(screen.getByRole("button", { name: "Stop" }));
  expect(onConfirm).not.toHaveBeenCalled();
  await new Promise((r) => setTimeout(r, 350));
  await user.click(screen.getByRole("button", { name: "Stop this Ticket" }));
  expect(onConfirm).toHaveBeenCalledTimes(1);
  // It disarms after confirming.
  expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
});

test("a double-click does not confirm", async () => {
  const user = userEvent.setup();
  const onConfirm = setup();
  await user.dblClick(screen.getByRole("button", { name: "Stop" }));
  expect(onConfirm).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: "Stop this Ticket" })).toBeTruthy();
});

// fireEvent, not userEvent: Testing Library's async wrapper waits on a real timer that fake timers never fire.
test("Confirm disarms on Escape and after a pause", () => {
  vi.useFakeTimers();
  const onConfirm = setup();
  fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  fireEvent.keyDown(screen.getByRole("button", { name: "Stop this Ticket" }), { key: "Escape" });
  expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  act(() => vi.advanceTimersByTime(5000));
  expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  expect(onConfirm).not.toHaveBeenCalled();
});
