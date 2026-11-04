// Pan and zoom for the graph's SVG: a pointer drag pans, the wheel (a trackpad pinch arrives as a wheel with ctrlKey)
// or a two-finger pinch zooms around the pointer, and +, - and 0 zoom in, out and back to the start. The scale stays
// between 0.25× and 4×. Keyboard zooms ease over 160 ms, except under prefers-reduced-motion, where every change is
// immediate.
//
// A press that moved is a drag, not a click: `wasDrag()` tells a node's click handler to ignore it. The pointer is
// captured only once the press has moved, so a plain click still reaches the node under it. A second finger starts a
// pinch outright (never a click): the content under the fingers' middle follows it while the spread scales it.
import { type RefObject, useCallback, useEffect, useRef, useState } from "react";
import { useShortcut } from "@/app/shortcuts";

export type View = { x: number; y: number; k: number };

export const MIN_ZOOM = 0.25;
export const MAX_ZOOM = 4;
/** One + or - press. */
const STEP = 1.25;
/** How far a press may move and still be a click. */
const SLOP = 4;
const EASE_MS = 160;

export const clampZoom = (k: number) => Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, k));

/** view zoomed by `factor` keeping the point (cx, cy) of the element where it is. */
export function zoomAt(view: View, factor: number, cx: number, cy: number): View {
  const k = clampZoom(view.k * factor);
  const f = k / view.k;
  return { k, x: cx - (cx - view.x) * f, y: cy - (cy - view.y) * f };
}

const easeOut = (t: number) => 1 - (1 - t) ** 3;

/** The lowest start zoom: a 12.5 px title stays at least 11 px on the screen. */
export const READABLE_ZOOM = 0.88;

/**
 * Where the graph starts, for a viewport `vw` wide and Tickets reaching `right`: fitted to the width but never below
 * READABLE_ZOOM, from the left edge. When they still do not fit, the desktop shows the newest (right) end; the phone
 * keeps the left edge, with the lane labels and the oldest Ticket in full, and the owner pans sideways.
 */
export function startView(vw: number, right: number, phone: boolean): View {
  if (vw <= 0 || right <= 0) return { k: 1, x: 0, y: 0 };
  const k = Math.min(1, Math.max(READABLE_ZOOM, vw / right));
  return { k, x: phone || right * k <= vw ? 0 : vw - right * k, y: 0 };
}

export function usePanZoom(
  ref: RefObject<Element | null>,
  { home, reducedMotion }: { home: () => View; reducedMotion: boolean },
) {
  const [view, setViewState] = useState<View>({ x: 0, y: 0, k: 1 });
  const current = useRef(view);
  const frame = useRef(0);
  const moved = useRef(false);
  const homeRef = useRef(home);
  const motion = useRef(!reducedMotion);
  useEffect(() => {
    homeRef.current = home;
    motion.current = !reducedMotion;
  });

  const setView = useCallback((v: View) => {
    cancelAnimationFrame(frame.current);
    current.current = v;
    setViewState(v);
  }, []);

  /** Moves to `to`, easing when motion is allowed. */
  const go = useCallback(
    (to: View) => {
      if (!motion.current || typeof requestAnimationFrame !== "function") return setView(to);
      cancelAnimationFrame(frame.current);
      const from = current.current;
      const t0 = performance.now();
      const step = (now: number) => {
        const t = easeOut(Math.min(1, (now - t0) / EASE_MS));
        const v = { x: from.x + (to.x - from.x) * t, y: from.y + (to.y - from.y) * t, k: from.k + (to.k - from.k) * t };
        current.current = v;
        setViewState(v);
        if (t < 1) frame.current = requestAnimationFrame(step);
      };
      frame.current = requestAnimationFrame(step);
    },
    [setView],
  );

  const center = () => {
    const el = ref.current;
    return el ? { cx: el.clientWidth / 2, cy: el.clientHeight / 2 } : { cx: 0, cy: 0 };
  };
  const zoomBy = (factor: number) => {
    const { cx, cy } = center();
    go(zoomAt(current.current, factor, cx, cy));
  };
  const reset = useCallback(() => go(homeRef.current()), [go]);

  useShortcut("+", () => zoomBy(STEP));
  useShortcut("=", () => zoomBy(STEP));
  useShortcut("-", () => zoomBy(1 / STEP));
  useShortcut("0", reset);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const pointers = new Map<number, { x: number; y: number }>();
    let start: { x: number; y: number } | undefined;
    let pinch: { d: number; cx: number; cy: number } | undefined;
    const local = (e: { clientX: number; clientY: number }) => {
      const r = el.getBoundingClientRect();
      return { x: e.clientX - r.left, y: e.clientY - r.top };
    };
    const spread = () => {
      const [a, b] = [...pointers.values()];
      return { d: Math.hypot(a.x - b.x, a.y - b.y), cx: (a.x + b.x) / 2, cy: (a.y + b.y) / 2 };
    };

    const down = (e: Event) => {
      const p = e as PointerEvent;
      if (p.pointerType === "mouse" && p.button !== 0) return;
      pointers.set(p.pointerId, local(p));
      if (pointers.size === 1) {
        // One finger or the mouse: a click until it moves past the slop.
        moved.current = false;
        start = local(p);
      } else if (pointers.size === 2) {
        // A second finger: a pinch, never a click. Both pointers are held from here on.
        moved.current = true;
        pinch = spread();
        for (const id of pointers.keys()) el.setPointerCapture?.(id);
      }
    };
    const move = (e: Event) => {
      const p = e as PointerEvent;
      const prev = pointers.get(p.pointerId);
      if (!prev) return;
      const at = local(p);
      pointers.set(p.pointerId, at);
      if (pinch && pointers.size >= 2) {
        // The point under the old middle follows the fingers to the new middle, scaled by how far they spread.
        const s = spread();
        const v = current.current;
        const k = clampZoom(pinch.d > 0 ? v.k * (s.d / pinch.d) : v.k);
        const f = k / v.k;
        setView({ k, x: s.cx - (pinch.cx - v.x) * f, y: s.cy - (pinch.cy - v.y) * f });
        pinch = s;
        return;
      }
      if (!moved.current && start && Math.hypot(at.x - start.x, at.y - start.y) > SLOP) {
        moved.current = true;
        el.setPointerCapture?.(p.pointerId);
      }
      if (!moved.current) return;
      const v = current.current;
      setView({ ...v, x: v.x + at.x - prev.x, y: v.y + at.y - prev.y });
    };
    const up = (e: Event) => {
      const p = e as PointerEvent;
      pointers.delete(p.pointerId);
      if (pointers.size < 2) pinch = undefined;
      if (pointers.size === 0) start = undefined;
    };
    const wheel = (e: Event) => {
      const w = e as WheelEvent;
      w.preventDefault();
      const at = local(w);
      // A pinch on a trackpad sends small deltas with ctrlKey; a mouse wheel sends larger ones.
      const factor = Math.exp(-w.deltaY * (w.ctrlKey ? 0.01 : 0.0015));
      setView(zoomAt(current.current, factor, at.x, at.y));
    };

    el.addEventListener("pointerdown", down);
    el.addEventListener("pointermove", move);
    el.addEventListener("pointerup", up);
    el.addEventListener("pointercancel", up);
    el.addEventListener("wheel", wheel, { passive: false });
    return () => {
      el.removeEventListener("pointerdown", down);
      el.removeEventListener("pointermove", move);
      el.removeEventListener("pointerup", up);
      el.removeEventListener("pointercancel", up);
      el.removeEventListener("wheel", wheel);
      cancelAnimationFrame(frame.current);
    };
  }, [ref, setView]);

  return { view, setView, reset, zoomBy, wasDrag: () => moved.current };
}
