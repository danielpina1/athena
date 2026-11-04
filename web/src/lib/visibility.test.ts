import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { LinkStatus } from "@/lib/link/client";
import { trackVisibility } from "./visibility";

/** A link whose status the test sets. */
function fakeLink(status: LinkStatus = "offline") {
  const cbs = new Set<(s: LinkStatus) => void>();
  return {
    status,
    onStatus(cb: (s: LinkStatus) => void) {
      cbs.add(cb);
      return () => cbs.delete(cb);
    },
    set(s: LinkStatus) {
      this.status = s;
      for (const cb of [...cbs]) cb(s);
    },
  };
}

let shown: "visible" | "hidden";
let focused: boolean;
let stop: () => void = () => {};

beforeEach(() => {
  shown = "visible";
  focused = true;
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => shown);
  vi.spyOn(document, "hasFocus").mockImplementation(() => focused);
});

afterEach(() => {
  stop();
  vi.restoreAllMocks();
});

const hide = () => {
  shown = "hidden";
  focused = false;
  document.dispatchEvent(new Event("visibilitychange"));
};
const show = () => {
  shown = "visible";
  focused = true;
  document.dispatchEvent(new Event("visibilitychange"));
};

test("visibility sent on change and after reconnect", () => {
  const link = fakeLink("open");
  const send = vi.fn();
  stop = trackVisibility(link, send);
  // What the server is told first, on an open link.
  expect(send.mock.calls).toEqual([[true]]);

  hide();
  show();
  window.dispatchEvent(new Event("focus"));
  expect(send.mock.calls).toEqual([[true], [false], [true]]); // focus while already visible: no repeat

  // On the desktop the page can stay visible while another window has the focus: the owner is not looking.
  focused = false;
  window.dispatchEvent(new Event("blur"));
  focused = true;
  window.dispatchEvent(new Event("focus"));
  expect(send.mock.calls.slice(3)).toEqual([[false], [true]]);

  // A reconnect is a new connection that knows nothing: always told, even with no change.
  send.mockClear();
  link.set("offline");
  link.set("open");
  link.set("offline");
  link.set("open");
  expect(send.mock.calls).toEqual([[true], [true]]);
});

test("a change while the link is down is told on the next open, once", () => {
  const link = fakeLink("offline");
  const send = vi.fn();
  stop = trackVisibility(link, send);
  hide();
  show();
  hide();
  expect(send).not.toHaveBeenCalled();
  link.set("connecting");
  link.set("open");
  expect(send.mock.calls).toEqual([[false]]);
});

test("stop removes the listeners", () => {
  const link = fakeLink("open");
  const send = vi.fn();
  trackVisibility(link, send)();
  send.mockClear();
  hide();
  link.set("offline");
  link.set("open");
  expect(send).not.toHaveBeenCalled();
});
