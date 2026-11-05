import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeAll, expect, test, vi } from "vitest";
import DesignPage from "@/app/routes/design";

beforeAll(() => {
  // jsdom has no ResizeObserver; Radix positioning asks for one.
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

// Ruling R19: the CSP refuses <style> elements, and a refused one is a logged violation. No component may add one,
// whatever is open.
test("opening every overlay adds no <style> element to the document", async () => {
  const added: string[] = [];
  const observer = new MutationObserver((records) => {
    for (const r of records)
      for (const n of r.addedNodes) {
        if (n instanceof HTMLStyleElement) added.push(n.outerHTML.slice(0, 80));
        if (n instanceof Element) for (const s of n.querySelectorAll("style")) added.push(s.outerHTML.slice(0, 80));
      }
  });
  observer.observe(document, { childList: true, subtree: true });
  const before = document.querySelectorAll("style").length;

  const user = userEvent.setup();
  render(<DesignPage />);

  for (const name of ["Light theme", "Dark theme"]) {
    const panel = within(screen.getByRole("region", { name }));

    await user.click(panel.getByRole("button", { name: "Ticket actions" }));
    expect(await screen.findByRole("menu")).toBeTruthy();
    await user.keyboard("{Escape}");

    await user.click(panel.getByRole("button", { name: "Placement" }));
    expect(await screen.findByText(/Placed by the router/)).toBeTruthy();
    await user.keyboard("{Escape}");

    await user.hover(panel.getByRole("button", { name: "Notifications" }));
    expect(await screen.findByRole("tooltip")).toBeTruthy();
    await user.unhover(panel.getByRole("button", { name: "Notifications" }));

    await user.click(panel.getByRole("button", { name: "Show toast" }));
    await user.click(panel.getByRole("button", { name: "Failure" }));
    expect(screen.getAllByRole("status").length).toBeGreaterThan(0);

    await user.click(panel.getByRole("button", { name: "Rename Ticket" }));
    expect(screen.getByRole("dialog", { name: "Rename Ticket" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    await user.click(panel.getByRole("button", { name: "Answer on the phone" }));
    const sheet = screen.getByRole("dialog", { name: /Should the retry live/ });
    // A toast raised over the open sheet renders inside it.
    await user.click(within(sheet).getByRole("button", { name: "Save draft" }));
    expect(within(sheet).getByText("Draft saved")).toBeTruthy();
    await user.click(within(sheet).getByRole("button", { name: "Send answer" }));

    await user.click(panel.getByRole("button", { name: /Command palette/ }));
    expect(screen.getByRole("dialog", { name: "Command palette" })).toBeTruthy();
    await user.keyboard("{Escape}");
    fireEvent(screen.getByRole("dialog", { name: "Command palette" }), new Event("cancel", { cancelable: true }));
  }

  observer.disconnect();
  expect(added).toEqual([]);
  expect(document.querySelectorAll("style").length).toBe(before);
});
