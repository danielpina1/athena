import { cn } from "@/ui/cn";

// Said in place of a list, a count or an empty state before the first Snapshot (the store's `synced`): until the
// server has answered, the app does not know there is nothing, so it never says "nothing" or "0".
export const WAITING = "Waiting for the server.";

export function Waiting({ className }: { className?: string }) {
  return <p className={cn("text-sm text-muted", className)}>{WAITING}</p>;
}
