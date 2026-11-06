// One runtime of the node card: its name, version ("untested" in amber when Athena was not verified against it) and
// whether it is ready (or why not), then one bar per quota window with the percent used and when it resets. A limited
// runtime says until when, in amber; one never measured says so; a reading over 10 minutes old says its age. Bars are
// ink on a hairline track, amber from 85%. Never orange: a usage limit waits by itself and needs nobody.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useId } from "react";
import type { QuotaWindow, RuntimeStatus } from "@/gen/athena/v1/state_pb";
import { resetTime, runtimeLabel, shortResetTime } from "@/lib/text/runtime";
import { Mono } from "@/ui/athena/Mono";
import { useNow } from "@/ui/athena/RelativeTime";
import { cn } from "@/ui/cn";

const MINUTE = 60_000;
/** A reading older than this says its age. */
const FRESH_MS = 10 * MINUTE;
/** From this percent a window is amber. */
const WARN_PCT = 85;

const pctOf = (w: QuotaWindow) => Math.round(Math.min(Math.max(w.used, 0), 1) * 100);

/** "25 min ago", "3 h ago": how old a reading is. */
function age(ms: number): string {
  const min = Math.floor(ms / MINUTE);
  return min < 60 ? `${min} min ago` : `${Math.floor(min / 60)} h ago`;
}

/** Until when the runtime is limited, or undefined when it is not (or no longer is). */
function limitedUntil(s: RuntimeStatus, now: number): number | undefined {
  const until = s.limitedUntil ? timestampMs(s.limitedUntil) : undefined;
  return until !== undefined && until > now ? until : undefined;
}

/**
 * The sidebar's line for a runtime: its fullest window ("42% · 5h"), or what keeps it from working. warn: amber.
 */
export function quotaLine(s: RuntimeStatus, now: number): { text: string; warn: boolean } {
  if (!s.ready) return { text: "not ready", warn: true };
  const until = limitedUntil(s, now);
  // The sidebar is narrow: the time on the same day, else the weekday; the card has the rest.
  if (until !== undefined) return { text: `limited · ${shortResetTime(until, now)}`, warn: true };
  const live = s.windows.filter((w) => !w.stale);
  if (!s.measured || live.length === 0) return { text: "not measured", warn: false };
  const top = live.reduce((a, b) => (b.used > a.used ? b : a));
  return { text: `${pctOf(top)}% · ${top.name}`, warn: pctOf(top) >= WARN_PCT };
}

function Window({ runtime, w, now }: { runtime: string; w: QuotaWindow; now: number }) {
  const pct = pctOf(w);
  const warn = !w.stale && pct >= WARN_PCT;
  const resets = w.resetsAt ? timestampMs(w.resetsAt) : undefined;
  return (
    <>
      <Mono tone="default">{w.name}</Mono>
      <div
        role="meter"
        aria-label={`${runtime} ${w.name} used`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={w.stale ? 0 : pct}
        aria-valuetext={w.stale ? "Reset since it was read" : `${pct}%`}
        className="h-2 self-center bg-line-soft"
      >
        <div className={cn("h-full", warn ? "bg-warning" : "bg-text")} style={{ width: `${w.stale ? 0 : pct}%` }} />
      </div>
      <span className={cn("text-right font-mono text-xs tabular-nums", warn ? "text-warning" : "text-text")}>
        {w.stale ? "—" : `${pct}%`}
      </span>
      <Mono>{w.stale ? "reset since read" : resets !== undefined ? `resets ${resetTime(resets, now)}` : ""}</Mono>
    </>
  );
}

export function QuotaBars({ status }: { status: RuntimeStatus }) {
  const id = useId();
  const now = useNow();
  const label = runtimeLabel(status.name);
  const until = limitedUntil(status, now);
  const readAt = status.readAt ? timestampMs(status.readAt) : undefined;
  const old = status.measured && readAt !== undefined && now - readAt > FRESH_MS;
  return (
    <section aria-labelledby={id} data-runtime={status.name} className="grid min-w-0 gap-2">
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-2">
        <h3 id={id} className="font-semibold">
          {label}
        </h3>
        {status.version && <Mono>{status.version}</Mono>}
        {status.version && !status.tested && <span className="font-mono text-xs text-warning">untested</span>}
        <span className="ml-auto flex items-center gap-1.5 text-sm">
          <span aria-hidden className={cn("size-2 rounded-full", status.ready ? "bg-success" : "bg-warning")} />
          {status.ready ? "Ready" : "Not ready"}
        </span>
      </div>
      {status.why && <p className="text-sm break-words text-muted">{status.why}</p>}
      {until !== undefined && <p className="text-sm text-warning">{`Limited until ${resetTime(until, now)}`}</p>}
      {!status.measured ? (
        <p className="text-sm text-muted">Not measured yet</p>
      ) : (
        status.windows.length > 0 && (
          <div className="grid grid-cols-[auto_minmax(0,1fr)_auto_auto] items-baseline gap-x-3 gap-y-1.5">
            {status.windows.map((w) => (
              <Window key={w.name} runtime={label} w={w} now={now} />
            ))}
          </div>
        )
      )}
      {old && <p className="font-mono text-xs text-muted">{`as of ${age(now - readAt)}`}</p>}
    </section>
  );
}
