import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { CHORD_MS, useShortcut } from "@/app/shortcuts";

afterEach(() => vi.useRealTimers());

test("shortcuts ignore typing in inputs", () => {
  const j = vi.fn();
  const esc = vi.fn();
  const interrupt = vi.fn();
  const off = vi.fn();
  function Keys() {
    useShortcut("j", j);
    useShortcut("Escape", esc);
    useShortcut("mod+Enter", interrupt);
    useShortcut("x", off, { when: () => false });
    return (
      <>
        <input aria-label="field" />
        <textarea aria-label="area" />
        <div aria-label="rich" contentEditable suppressContentEditableWarning>
          <span>text</span>
        </div>
      </>
    );
  }
  render(<Keys />);
  const field = screen.getByLabelText("field");
  const area = screen.getByLabelText("area");
  const rich = screen.getByLabelText("rich").firstElementChild!;

  for (const target of [field, area, rich]) fireEvent.keyDown(target, { key: "j" });
  expect(j).not.toHaveBeenCalled();
  fireEvent.keyDown(document.body, { key: "j" });
  expect(j).toHaveBeenCalledTimes(1);

  // Esc and ⌘⏎ still work from a text field.
  fireEvent.keyDown(field, { key: "Escape" });
  expect(esc).toHaveBeenCalledTimes(1);
  fireEvent.keyDown(area, { key: "Enter", metaKey: true });
  fireEvent.keyDown(area, { key: "Enter", ctrlKey: true });
  expect(interrupt).toHaveBeenCalledTimes(2);
  // A plain Enter in a field is typing.
  fireEvent.keyDown(area, { key: "Enter" });
  expect(interrupt).toHaveBeenCalledTimes(2);

  // `when` gates a shortcut.
  fireEvent.keyDown(document.body, { key: "x" });
  expect(off).not.toHaveBeenCalled();
});

test("a chord waits at most 1 s for its second key", () => {
  vi.useFakeTimers();
  const go = vi.fn();
  function Keys() {
    useShortcut("g n", go);
    return null;
  }
  render(<Keys />);
  fireEvent.keyDown(document.body, { key: "g" });
  vi.advanceTimersByTime(CHORD_MS + 1);
  fireEvent.keyDown(document.body, { key: "n" });
  expect(go).not.toHaveBeenCalled();

  fireEvent.keyDown(document.body, { key: "g" });
  vi.advanceTimersByTime(CHORD_MS - 100);
  fireEvent.keyDown(document.body, { key: "n" });
  expect(go).toHaveBeenCalledTimes(1);

  // Typing in between breaks the chord.
  render(<input aria-label="field" />);
  fireEvent.keyDown(document.body, { key: "g" });
  fireEvent.keyDown(screen.getByLabelText("field"), { key: "a" });
  fireEvent.keyDown(document.body, { key: "n" });
  expect(go).toHaveBeenCalledTimes(1);
});
