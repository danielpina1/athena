import { runtimeLabel } from "@/lib/text/runtime";
import { Mono } from "./Mono";

/**
 * The runtime a Ticket works on: "Codex", or "Codex · auto" when auto chose it. Before it started, the choice is all
 * there is ("Auto"). Empty from an older server, which sends neither.
 */
export function runtimeText(runtime: string, choice: string): string {
  const on = runtime || choice;
  if (!on) return "";
  return choice === "auto" && on !== "auto" ? `${runtimeLabel(on)} · auto` : runtimeLabel(on);
}

// The runtime in mono beside a Ticket's model (runtimeText); nothing when there is none.
export function RuntimeBadge({ runtime, choice, className }: { runtime: string; choice: string; className?: string }) {
  const text = runtimeText(runtime, choice);
  return text ? <Mono className={className}>{text}</Mono> : null;
}
