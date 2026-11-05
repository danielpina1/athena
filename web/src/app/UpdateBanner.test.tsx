import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, test, vi } from "vitest";
import { watchUpdates } from "@/lib/update";
import { UpdateBanner } from "./UpdateBanner";

/** A service worker in a state; statechange is dispatched by the test. */
class FakeWorker extends EventTarget {
  postMessage = vi.fn();
  constructor(public state: ServiceWorkerState) {
    super();
  }
  become(state: ServiceWorkerState) {
    this.state = state;
    this.dispatchEvent(new Event("statechange"));
  }
}

class FakeRegistration extends EventTarget {
  installing: FakeWorker | null = null;
  waiting: FakeWorker | null = null;
  update = vi.fn(async () => {});
  /** The browser found a new sw.js: it installs, then waits. */
  found() {
    const w = new FakeWorker("installing");
    this.installing = w;
    this.dispatchEvent(new Event("updatefound"));
    return w;
  }
}

function fakeContainer(controlled: boolean) {
  const reg = new FakeRegistration();
  const container = Object.assign(new EventTarget(), {
    controller: controlled ? {} : null,
    register: vi.fn(async () => reg),
  });
  return { reg, container: container as unknown as ServiceWorkerContainer & typeof container };
}

const BANNER = "A new version is ready — Reload";

test("a new version waits, the banner offers Reload, and Reload activates it", async () => {
  const { reg, container } = fakeContainer(true);
  const reload = vi.fn();
  const updates = watchUpdates(container, reload);
  expect(container.register).toHaveBeenCalledWith("/sw.js", { scope: "/" });
  render(<UpdateBanner updates={updates} />);
  expect(screen.getByRole("status").textContent).toBe("");

  // The server was updated: the open tab keeps the old app and says so.
  await act(async () => {
    await Promise.resolve();
    const w = reg.found();
    reg.installing = null;
    reg.waiting = w;
    w.become("installed");
  });
  expect(screen.getByRole("status").textContent).toBe(BANNER);

  await userEvent.setup().click(screen.getByRole("button", { name: "Reload" }));
  expect(reg.waiting!.postMessage).toHaveBeenCalledWith({ type: "SKIP_WAITING" });
  expect(reload).not.toHaveBeenCalled();
  // The new worker took over: the page loads the new app.
  container.dispatchEvent(new Event("controllerchange"));
  expect(reload).toHaveBeenCalledTimes(1);
});

test("a worker already waiting when the page loads shows the banner at once", async () => {
  const { reg, container } = fakeContainer(true);
  reg.waiting = new FakeWorker("installed");
  const updates = watchUpdates(container, vi.fn());
  render(<UpdateBanner updates={updates} />);
  expect(await screen.findByText(/A new version is ready/)).toBeTruthy();
});

test("the first install says nothing and its takeover does not reload", async () => {
  const { reg, container } = fakeContainer(false);
  const reload = vi.fn();
  const updates = watchUpdates(container, reload);
  render(<UpdateBanner updates={updates} />);
  await act(async () => {
    await Promise.resolve();
    reg.found().become("installed");
  });
  expect(screen.getByRole("status").textContent).toBe("");
  // clients.claim() on the first activation changes the controller: no reload for that.
  container.dispatchEvent(new Event("controllerchange"));
  expect(reload).not.toHaveBeenCalled();
});

test("check asks the browser to look for a new sw.js", async () => {
  const { reg, container } = fakeContainer(true);
  const updates = watchUpdates(container, vi.fn());
  updates.check();
  await vi.waitFor(() => expect(reg.update).toHaveBeenCalledTimes(1));
});

test("another tab's Reload reloads this one", async () => {
  // Two tabs of the app: each has its own container, and they share the registration and its workers.
  const reg = new FakeRegistration();
  const tab = () =>
    Object.assign(new EventTarget(), { controller: {}, register: vi.fn(async () => reg) }) as unknown as ServiceWorkerContainer &
      EventTarget;
  const [a, b] = [tab(), tab()];
  const [reloadA, reloadB] = [vi.fn(), vi.fn()];
  const updatesA = watchUpdates(a, reloadA);
  const updatesB = watchUpdates(b, reloadB);
  await Promise.resolve();
  await Promise.resolve();
  const w = reg.found();
  reg.installing = null;
  reg.waiting = w;
  w.become("installed");
  expect(updatesA.ready() && updatesB.ready()).toBe(true);

  // A taps Reload: the worker activates and takes control of both tabs, and both load the new app.
  updatesA.apply();
  expect(w.postMessage).toHaveBeenCalledWith({ type: "SKIP_WAITING" });
  w.become("activated");
  a.dispatchEvent(new Event("controllerchange"));
  b.dispatchEvent(new Event("controllerchange"));
  expect(reloadA).toHaveBeenCalledTimes(1);
  expect(reloadB).toHaveBeenCalledTimes(1);

  // A further controllerchange never reloads a tab twice.
  b.dispatchEvent(new Event("controllerchange"));
  expect(reloadB).toHaveBeenCalledTimes(1);
});

test("Reload with the waiting worker already active (or gone) reloads at once", async () => {
  const { reg, container } = fakeContainer(true);
  const reload = vi.fn();
  const updates = watchUpdates(container, reload);
  await Promise.resolve();
  await Promise.resolve();
  const w = reg.found();
  w.become("installed");
  expect(updates.ready()).toBe(true);
  // Another tab activated it, but this tab missed the controllerchange.
  w.become("activated");
  updates.apply();
  expect(w.postMessage).not.toHaveBeenCalled();
  expect(reload).toHaveBeenCalledTimes(1);
});

test("a waiting worker that turns redundant takes the banner away", async () => {
  const { reg, container } = fakeContainer(true);
  const updates = watchUpdates(container, vi.fn());
  await Promise.resolve();
  await Promise.resolve();
  const w = reg.found();
  w.become("installed");
  expect(updates.ready()).toBe(true);
  w.become("redundant");
  expect(updates.ready()).toBe(false);
});
