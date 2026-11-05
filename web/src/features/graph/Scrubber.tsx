// The time scrubber and its replay. The position is local state, never the URL. ▶ Replay plays the whole window in
// 10 s; under prefers-reduced-motion it jumps in ten steps, one a second; at the end it is back to now.
import { PauseIcon, PlayIcon } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { Button } from "@/ui/primitives/button";

/** ▶ Replay plays the whole window in this long. */
export const REPLAY_MS = 10_000;
/** Under reduced motion the replay jumps this many times, once a second. */
const REPLAY_JUMPS = 10;
/** The layout is redrawn at most this many times over a replay: `at` snaps to 1/200 of the window. */
const STEPS = 200;

/** `at` snapped to a step of the window (null stays null, now), so replay frames reuse one layout per step. */
export function snapToStep(at: number | null, start: number, end: number): number | null {
  if (at === null) return null;
  const step = Math.max(1, (end - start) / STEPS);
  return Math.min(end, start + Math.round((at - start) / step) * step);
}

/** Date.now(), updated every `everyMs`. */
export function useClock(everyMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), everyMs);
    return () => clearInterval(id);
  }, [everyMs]);
  return now;
}

function formatTime(ms: number, withDay: boolean): string {
  return new Intl.DateTimeFormat(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    ...(withDay && { month: "short", day: "numeric" }),
  }).format(ms);
}

export function Scrubber({
  start,
  end,
  at,
  playing,
  onScrub,
  onReplay,
}: {
  start: number;
  end: number;
  at: number | null;
  playing: boolean;
  onScrub(at: number | null): void;
  onReplay(): void;
}) {
  const withDay = end - start > 86_400_000;
  const value = at ?? end;
  return (
    <div className="flex min-w-0 items-center gap-3 border-t border-line bg-bg px-4 py-1 font-mono text-xs text-muted lg:py-2">
      {/* On a wide screen, wide enough for a date and a time, so the slider keeps its length whichever is shown.
          Narrower (the phone, or beside an open pane) the start is left out: the slider's value text says when. */}
      <span data-volatile className="hidden shrink-0 xl:inline xl:min-w-[16ch]">
        {formatTime(start, withDay)}
      </span>
      <input
        type="range"
        aria-label="Time"
        aria-valuetext={at === null ? "Now" : formatTime(value, true)}
        min={start}
        max={end}
        step={1000}
        value={value}
        disabled={end <= start}
        onChange={(e) => {
          const v = Number(e.target.value);
          onScrub(v >= end ? null : v);
        }}
        className="h-11 min-w-16 flex-1 accent-text lg:h-6"
      />
      <span data-volatile className="shrink-0 text-right text-text xl:min-w-[16ch]">
        {at === null ? `now ${formatTime(end, false)}` : formatTime(value, withDay)}
      </span>
      <Button size="sm" onClick={onReplay} disabled={end <= start}>
        {playing ? <PauseIcon aria-hidden /> : <PlayIcon aria-hidden />}
        {playing ? "Pause" : "Replay"}
      </Button>
    </div>
  );
}

/** The replay: `at` from the window's start to now over REPLAY_MS, or in REPLAY_JUMPS steps under reduced motion. */
export function useReplay(start: number, end: number, reducedMotion: boolean) {
  const [at, setAt] = useState<number | null>(null);
  const [playing, setPlaying] = useState(false);
  const span = useRef({ start, end });
  useEffect(() => {
    span.current = { start, end };
  });
  useEffect(() => {
    if (!playing) return;
    const { start, end } = span.current;
    const width = end - start;
    const done = () => {
      setAt(null);
      setPlaying(false);
    };
    if (reducedMotion || typeof requestAnimationFrame !== "function") {
      let i = 0;
      const id = setInterval(() => {
        i += 1;
        if (i >= REPLAY_JUMPS) done();
        else setAt(start + (width * i) / REPLAY_JUMPS);
      }, REPLAY_MS / REPLAY_JUMPS);
      return () => clearInterval(id);
    }
    let raf = 0;
    const t0 = performance.now();
    const step = (now: number) => {
      const p = (now - t0) / REPLAY_MS;
      if (p >= 1) return done();
      setAt(start + width * p);
      raf = requestAnimationFrame(step);
    };
    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
  }, [playing, reducedMotion]);

  return {
    at,
    playing,
    scrub(v: number | null) {
      setPlaying(false);
      setAt(v);
    },
    toggle() {
      if (playing) return setPlaying(false);
      setAt(span.current.start);
      setPlaying(true);
    },
  };
}
