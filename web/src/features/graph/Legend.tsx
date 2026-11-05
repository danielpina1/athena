// The legend (the mockup's Tasks and Links, drawn with the canvas's own pieces) and, under it, the Ticket under the
// pointer or in focus: its id and lane, full title, stage and question.
import { useId } from "react";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { Box, EDGE, Line, LINK_KEY, Markers, stageOf, TASK_KEY } from "./draw";
import type { GNode } from "./layout";

export function Legend({ shown, laneTitle }: { shown?: GNode; laneTitle: string }) {
  const markers = useId().replace(/:/g, "");
  const t = shown?.ticket;
  return (
    <div className="grid content-start gap-4 p-4 text-xs">
      <section aria-label="Legend" className="grid gap-4">
        <div className="grid gap-2">
          <h2 className="font-mono text-xs tracking-wider text-muted uppercase">Tasks</h2>
          <ul className="grid gap-2">
            {TASK_KEY.map((k) => (
              <li key={k.look} className="flex items-center gap-2">
                <svg aria-hidden width="24" height="14" className="shrink-0">
                  <Box look={k.look} x={1} y={1} w={22} h={12} />
                </svg>
                <span data-legend>{k.label}</span>
              </li>
            ))}
          </ul>
        </div>
        <div className="grid gap-2">
          <h2 className="font-mono text-xs tracking-wider text-muted uppercase">Links</h2>
          <svg aria-hidden width="0" height="0" className="absolute">
            <Markers id={markers} />
          </svg>
          <ul className="grid gap-2">
            {LINK_KEY.map((kind) => (
              <li key={kind} className="flex items-center gap-2">
                <svg aria-hidden width="28" height="10" className="shrink-0">
                  <Line style={EDGE[kind]} d="M1 5L25 5" markers={markers} />
                </svg>
                <span data-legend>{EDGE[kind].label}</span>
              </li>
            ))}
          </ul>
        </div>
      </section>
      <section aria-label="Selected" className="grid min-w-0 gap-1 border-t border-line-soft pt-3">
        <h2 className="font-mono text-xs tracking-wider text-muted uppercase">Selected</h2>
        {shown && t ? (
          <>
            <Mono className="break-all">
              {shown.id} · {laneTitle}
            </Mono>
            <p dir="auto" title={shown.title} className="line-clamp-4 text-sm font-semibold break-words wrap-anywhere">
              {shown.title || "Untitled"}
            </p>
            <p className="text-muted">
              {stageOf(shown)} · created <RelativeTime at={shown.created} />
            </p>
            {t.attentionQuestion && (
              <p dir="auto" className="line-clamp-3 break-words wrap-anywhere text-muted">
                Asked: {t.attentionQuestion}
              </p>
            )}
          </>
        ) : (
          <p className="text-muted">Point at or focus a Ticket to see it here.</p>
        )}
      </section>
    </div>
  );
}

