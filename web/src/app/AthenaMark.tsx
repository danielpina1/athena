import { type LinkSource, LINK_LABEL, useLinkStatus } from "@/app/ConnectionBanner";
import { selectCounts, selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { cn } from "@/ui/cn";

// Athena's mark, the one square beside its name. It says how Athena is and how the link is:
//   - open: still while nothing runs, breathing (a soft ring, about 2.4 s) while a Ticket works, orange with a quicker
//     ring while anything needs the owner;
//   - connecting: hollow, still; offline or signed out: red, still.
// The motion is CSS only (index.css) and stops under reduced motion. The link's state is in words for screen readers;
// the Needs you count and the live feed say the rest.

export type MarkState = "idle" | "working" | "needs-you";

export function useMarkState(): MarkState {
  return useAthena((s) => {
    if (!selectSynced(s)) return "idle";
    const c = selectCounts(s);
    return c.needsYou > 0 ? "needs-you" : c.working > 0 ? "working" : "idle";
  });
}

export function AthenaMark({ link, className }: { link: LinkSource; className?: string }) {
  const state = useMarkState();
  const status = useLinkStatus(link);
  const live = status === "open" && state !== "idle";
  return (
    <span className={cn("inline-flex items-center", className)}>
      <span aria-hidden data-mark data-state={state} data-link={status} className={cn("athena-mark", live && "athena-ring")} />
      <span className="sr-only">{LINK_LABEL[status]}</span>
    </span>
  );
}
