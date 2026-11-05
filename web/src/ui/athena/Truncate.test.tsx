import { render, screen } from "@testing-library/react";
import { expect, test } from "vitest";
import { Truncate } from "@/ui/athena/Truncate";

test("Truncate never widens its parent", () => {
  const hostile = "x".repeat(2000) + " " + "مرحبا".repeat(50);
  const { container } = render(<Truncate text={hostile} lines={2} />);
  const root = container.firstElementChild as HTMLElement;
  // jsdom has no layout: pin the classes that keep the text inside its column.
  expect(root.className).toMatch(/\bmin-w-0\b/);
  const text = screen.getByText(hostile);
  expect(text.className).toMatch(/\bline-clamp-2\b/);
  expect(text.className).toMatch(/\bbreak-words\b/);
  expect(text.className).toMatch(/wrap-anywhere/);
  expect(text.className).not.toMatch(/overflow-x-(auto|scroll)|whitespace-nowrap/);
  expect(text.getAttribute("dir")).toBe("auto");
});

test("Truncate clamps by the line count it is given", () => {
  const { container } = render(<Truncate text="short" lines={1} />);
  expect(container.querySelector(".line-clamp-1")).toBeTruthy();
});
