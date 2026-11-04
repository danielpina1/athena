// Pan and zoom for the graph's SVG: a pointer drag pans, the wheel (a trackpad pinch arrives as a wheel with ctrlKey)
// or a two-finger pinch zooms around the pointer, and +, - and 0 zoom in, out and back to the start. The scale stays
// between 0.25× and 4×. Keyboard zooms ease over 160 ms, except under prefers-reduced-motion, where every change is
// immediate.
//
// A press that moved is a drag, not a click: `wasDrag()` tells a node's click handler to ignore it. The pointer is
// captured only once the press has moved, so a plain click still reaches the node under it.
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
    let pinch: { d: number } | undefined;
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
      moved.current = false;
      start = local(p);
      if (pointers.size === 2) pinch = { d: spread().d };
    };
    const move = (e: Event) => {
      const p = e as PointerEvent;
      const prev = pointers.get(p.pointerId);
      if (!prev) return;
      const at = local(p);
      if (!moved.current && start && Math.hypot(at.x - start.x, at.y - start.y) > SLOP) {
        moved.current = true;
        el.setPointerCapture?.(p.pointerId);
      }
      if (!moved.current) return;
      if (pointers.size >= 2 && pinch) {
        pointers.set(p.pointerId, at);
        const s = spread();
        if (pinch.d > 0) setView(zoomAt(current.current, s.d / pinch.d, s.cx, s.cy));
        pinch = { d: s.d };
        return;
      }
      pointers.set(p.pointerId, at);
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
