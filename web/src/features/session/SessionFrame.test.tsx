import { render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { at, load } from "@/features/tickets/fixtures";
import { fakeStreamLink, type FakeStreamLink } from "@/lib/stream/testkit";
import { connectStreams } from "@/lib/stream/useSessionStream";
import { useAthena } from "@/lib/store/state";
import { LazySessionView } from "./SessionFrame";

let link: FakeStreamLink;
let disconnect: () => void;

beforeEach(() => {
  link = fakeStreamLink("open");
  disconnect = connectStreams(link);
  load({ tickets: [{ id: "T-1", title: "Ship", stage: "working", updatedAt: at(0), createdAt: at(0) }] });
});
afterEach(() => {
  disconnect();
  useAthena.getState().reset();
});

test("the frame shows and the stream opens while the session view loads", async () => {
  render(<LazySessionView ticketId="T-1" />);
  expect(screen.getByRole("heading", { name: "Session", level: 2 })).toBeTruthy();
  const loading = screen.getByRole("log", { name: "Session output" });
  expect(loading.getAttribute("aria-busy")).toBe("true");
  expect(link.opens()).toEqual([{ ticketId: "T-1", fromOffset: -1n }]);
  expect(await screen.findByText("No session output yet.")).toBeTruthy();
  expect(screen.getByRole("log", { name: "Session output" }).hasAttribute("aria-busy")).toBe(false);
  // The view took over the stream: still one open.
  expect(link.opens()).toHaveLength(1);
});
