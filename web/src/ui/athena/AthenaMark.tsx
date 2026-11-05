import { selectCounts, selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { cn } from "@/ui/cn";

// Athena's mark, the square beside its name: still while nothing runs, breathing (a soft ring, about 2.4 s) while a
// Ticket works, and orange with a quicker ring while anything needs the owner. The motion is CSS only (index.css) and
// stops under reduced motion; the Needs you count and the live feed say the same in words.

export type MarkState = "idle" | "working" | "needs-you";

export function useMarkState(): MarkState {
  return useAthena((s) => {
    if (!selectSynced(s)) return "idle";
    const c = selectCounts(s);
    return c.needsYou > 0 ? "needs-you" : c.working > 0 ? "working" : "idle";
  });
}

export function AthenaMark({ className }: { className?: string }) {
  const state = useMarkState();
  return <span aria-hidden data-mark data-state={state} className={cn("athena-mark", state !== "idle" && "athena-ring", className)} />;
}
