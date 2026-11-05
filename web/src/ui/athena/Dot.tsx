import { cn } from "@/ui/cn";

// The 9 px event dot of the timeline rail and the live feed. The tone says how an event went; the text beside it
// always says it too, so neither the colour nor the pulse carries meaning alone.
//   neutral: a hollow ring; ok: filled ink (the tokens' "ok" is ink, not green); warn: filled grey (interrupted,
//   expired, stopped: no amber token exists); danger: filled red; attention: filled orange with an ink edge (orange
//   alone is under 3:1 on white) and a pulsing ring, for the one thing waiting on the owner.

export type Tone = "neutral" | "ok" | "warn" | "danger" | "attention";

const TONE: Record<Tone, string> = {
  neutral: "border-muted bg-bg",
  ok: "border-ok bg-ok",
  warn: "border-muted bg-muted",
  danger: "border-danger bg-danger",
  attention: "border-accent-fg bg-accent text-accent-ink",
};

export function Dot({ tone, className }: { tone: Tone; className?: string }) {
  return (
    <span
      aria-hidden
      data-dot
      data-tone={tone}
      className={cn("relative block size-[9px] shrink-0 rounded-full border", TONE[tone], tone === "attention" && "athena-ring", className)}
    />
  );
}
