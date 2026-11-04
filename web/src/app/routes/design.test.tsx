import { render, screen, within } from "@testing-library/react";
import { beforeAll, expect, test, vi } from "vitest";
import DesignPage from "@/app/routes/design";

beforeAll(() => {
  // jsdom has no ResizeObserver; Radix ScrollArea needs one.
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

test("the design page renders every section in both themes", () => {
  render(<DesignPage />);
  for (const name of ["Light theme", "Dark theme"]) {
    const panel = screen.getByRole("region", { name });
    for (const title of ["Colour", "Type", "Stage", "Buttons", "Fields", "Choices", "Overlays", "Feedback", "Content"]) {
      expect(within(panel).getByRole("heading", { name: title })).toBeTruthy();
    }
    expect(within(panel).getAllByText("Needs approval").length).toBeGreaterThan(0);
  }
  expect(screen.getByRole("region", { name: "Dark theme" }).dataset.theme).toBe("dark");
});
