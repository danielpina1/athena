// Test helpers for the shell: a viewport width (matchMedia), a link whose status a test sets, and a probe that shows
// the router's location.
import { useLocation } from "react-router";
import type { Frame } from "@/gen/athena/v1/link_pb";
import type { LinkStatus } from "@/lib/link/client";
import type { LinkSource } from "@/app/ConnectionBanner";

/** Makes window.matchMedia answer min-width queries as a viewport `width` px wide would. */
export function stubWidth(width: number) {
  window.matchMedia = ((query: string) => {
    const min = /\(min-width:\s*(\d+)px\)/.exec(query);
    return {
      matches: min ? width >= Number(min[1]) : false,
      media: query,
      onchange: null,
      addEventListener() {},
      removeEventListener() {},
      addListener() {},
      removeListener() {},
      dispatchEvent: () => false,
    } as MediaQueryList;
  }) as typeof window.matchMedia;
}

export type FakeLink = LinkSource & { set(s: LinkStatus): void; frame(): void };

export function fakeLink(initial: LinkStatus = "open"): FakeLink {
  let status = initial;
  const statusCbs = new Set<(s: LinkStatus) => void>();
  const frameCbs = new Set<(f: Frame) => void>();
  return {
    get status() {
      return status;
    },
    onStatus(cb) {
      statusCbs.add(cb);
      return () => statusCbs.delete(cb);
    },
    onFrame(cb) {
      frameCbs.add(cb);
      return () => frameCbs.delete(cb);
    },
    set(s) {
      status = s;
      for (const cb of statusCbs) cb(s);
    },
    frame() {
      for (const cb of frameCbs) cb({} as Frame);
    },
  };
}

/** Renders the current pathname and search, for assertions on navigation. */
export function Where() {
  const l = useLocation();
  return <output data-testid="where">{l.pathname + l.search}</output>;
}
