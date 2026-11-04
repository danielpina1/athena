import { act, fireEvent, render } from "@testing-library/react";
import { useRef } from "react";
import { describe, expect, test } from "vitest";
import { MAX_ZOOM, MIN_ZOOM, reveal, startView, usePanZoom, zoomAt } from "./usePanZoom";

function Probe({ onClick }: { onClick?: () => void }) {
  const ref = useRef<SVGSVGElement>(null);
  const { view, wasDrag } = usePanZoom(ref, { home: () => ({ x: 0, y: 0, k: 1 }), reducedMotion: true });
  return (
    <svg ref={ref} data-testid="svg">
      <g data-testid="view" data-k={view.k} data-x={view.x} data-y={view.y} />
      <rect data-testid="hit" onClick={() => !wasDrag() && onClick?.()} />
    </svg>
  );
}

const view = () => {
  const g = document.querySelector('[data-testid="view"]')!;
  return { k: Number(g.getAttribute("data-k")), x: Number(g.getAttribute("data-x")), y: Number(g.getAttribute("data-y")) };
};
const ptr = (id: number, x: number, y: number) => ({ pointerId: id, clientX: x, clientY: y, pointerType: "touch", button: 0 });

describe("usePanZoom", () => {
  test("zoomAt keeps the point under the pointer and clamps to 0.25–4", () => {
    const v = zoomAt({ x: 0, y: 0, k: 1 }, 2, 100, 50);
    expect(v).toEqual({ k: 2, x: -100, y: -50 });
    expect(zoomAt({ x: 0, y: 0, k: 1 }, 100, 0, 0).k).toBe(MAX_ZOOM);
    expect(zoomAt({ x: 0, y: 0, k: 1 }, 0.001, 0, 0).k).toBe(MIN_ZOOM);
  });

  test("the start view: from the left on the phone, legible, newest in view on the desktop", () => {
    // Fits: 1×, from the left.
    expect(startView(1200, 800, false)).toEqual({ k: 1, x: 0, y: 0 });
    // A bit too wide: zoomed out, never below 11 px titles (0.88×).
    expect(startView(1000, 1100, false).k).toBeCloseTo(1000 / 1100);
    const wide = startView(375, 1168, true);
    expect(wide.k).toBeGreaterThanOrEqual(0.88);
    expect(wide.x).toBe(0);
    // The desktop shows the newest (right) end when they still do not fit.
    const desk = startView(500, 1168, false);
    expect(desk.x).toBeCloseTo(500 - 1168 * desk.k);
  });

  test("reveal pans the least that brings a box into view, and nothing when it is in view", () => {
    const v = { x: 0, y: 0, k: 1 };
    expect(reveal(v, { x: 10, y: 10, w: 50, h: 20 }, 300, 200)).toBe(v);
    // Off to the right: its right edge lands 16 px inside.
    expect(reveal(v, { x: 400, y: 10, w: 50, h: 20 }, 300, 200)).toEqual({ x: -166, y: 0, k: 1 });
    // Off to the left and below, at 2×.
    expect(reveal({ x: 0, y: 0, k: 2 }, { x: -50, y: 150, w: 10, h: 10 }, 300, 200)).toEqual({ x: 116, y: -136, k: 2 });
  });

  test("a two-finger pinch zooms around its middle, and is never a click", () => {
    let clicks = 0;
    render(<Probe onClick={() => clicks++} />);
    const svg = document.querySelector('[data-testid="svg"]')!;
    act(() => {
      fireEvent.pointerDown(svg, ptr(1, 100, 100));
      fireEvent.pointerDown(svg, ptr(2, 102, 100));
      // The fingers spread from 2 px to 4 px apart: twice the zoom, around their middle (x 101 → 102).
      fireEvent.pointerMove(svg, ptr(2, 104, 100));
    });
    expect(view().k).toBeCloseTo(2);
    expect(view().x).toBeCloseTo(102 - 101 * 2);
    act(() => {
      fireEvent.pointerUp(svg, ptr(2, 104, 100));
      fireEvent.pointerUp(svg, ptr(1, 100, 100));
      fireEvent.click(document.querySelector('[data-testid="hit"]')!);
    });
    expect(clicks).toBe(0);
  });

  test("a drag pans; a press that does not move is a click", () => {
    let clicks = 0;
    render(<Probe onClick={() => clicks++} />);
    const svg = document.querySelector('[data-testid="svg"]')!;
    act(() => {
      fireEvent.pointerDown(svg, ptr(1, 10, 10));
      fireEvent.pointerMove(svg, ptr(1, 40, 30));
      fireEvent.pointerUp(svg, ptr(1, 40, 30));
    });
    expect(view()).toEqual({ k: 1, x: 30, y: 20 });
    act(() => {
      fireEvent.pointerDown(svg, ptr(1, 40, 30));
      fireEvent.pointerUp(svg, ptr(1, 40, 30));
      fireEvent.click(document.querySelector('[data-testid="hit"]')!);
    });
    expect(clicks).toBe(1);
  });
});
