// The service worker's registration and the app's updates. After a server update an open tab keeps running the old
// app: the new worker installs and waits, the app says "A new version is ready — Reload", and the tap sends the
// waiting worker SKIP_WAITING; when it takes control, every tab that showed the banner reloads into the new app. The
// browser looks for a new sw.js on navigations; the app also asks after every reconnect (a server update restarts the
// server, so the link drops and comes back), never on a timer.

export type Updates = {
  /** Whether a new version is waiting. */
  ready(): boolean;
  /** Calls cb when ready() changes; returns the unsubscribe. */
  subscribe(cb: () => void): () => void;
  /** Activates the waiting version; the page reloads once it has control. */
  apply(): void;
  /** Asks the browser to look for a new sw.js now. */
  check(): void;
};

/** Registers /sw.js on container and follows its updates. reload is location.reload in the app. */
export function watchUpdates(container: ServiceWorkerContainer, reload: () => void): Updates {
  let waiting: ServiceWorker | null = null;
  let applying = false;
  let reloading = false;
  const listeners = new Set<() => void>();
  const setWaiting = (w: ServiceWorker | null) => {
    waiting = w;
    for (const l of [...listeners]) l();
  };
  /** One reload per page, whatever fires it: no loop of controllerchange and apply. */
  const reloadOnce = () => {
    if (reloading) return;
    reloading = true;
    reload();
  };
  // A worker that installs while another controls the page is an update; the very first install is not. One that
  // turns redundant (a newer build replaced it before activating) is no longer offered.
  const follow = (w: ServiceWorker | null) => {
    if (!w) return;
    const settle = () => {
      if (w.state === "installed" && container.controller) setWaiting(w);
      else if (w.state === "redundant" && waiting === w) setWaiting(null);
    };
    w.addEventListener("statechange", settle);
    settle();
  };
  // A takeover reloads every tab that knew of the update, the one that tapped Reload or any other: the new worker
  // deleted the old build's cache, so an old page would miss its lazy chunks. The first install's clients.claim()
  // changes the controller too, with no update known: that does not reload.
  container.addEventListener("controllerchange", () => {
    if (applying || waiting) reloadOnce();
  });

  const registered = container.register("/sw.js", { scope: "/" }).then((reg) => {
    if (reg.waiting && container.controller) setWaiting(reg.waiting);
    follow(reg.installing);
    reg.addEventListener("updatefound", () => follow(reg.installing));
    return reg;
  });
  registered.catch(() => {}); // no worker (private mode, blocked storage): the app runs without one

  return {
    ready: () => waiting !== null,
    subscribe(cb) {
      listeners.add(cb);
      return () => listeners.delete(cb);
    },
    apply() {
      // Gone, or already activated by another tab (this one missed or is past the controllerchange): just reload.
      if (!waiting || waiting.state !== "installed") {
        reloadOnce();
        return;
      }
      applying = true;
      waiting.postMessage({ type: "SKIP_WAITING" });
    },
    check() {
      void registered.then((reg) => reg.update()).catch(() => {});
    },
  };
}

let app: Updates | undefined;

/** The app's registration (main.tsx, production builds only: the dev server is never cached). */
export function registerServiceWorker(): void {
  if (app || !("serviceWorker" in navigator)) return;
  app = watchUpdates(navigator.serviceWorker, () => location.reload());
}

/** The app's updates, if a worker was registered. */
export function appUpdates(): Updates | undefined {
  return app;
}
