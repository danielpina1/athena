// Who a Ticket or a Trigger came from, said only when it is not the owner. The classes are internal/policy's trust
// classes; the Policy maps each to a tool policy (standard or restricted), and only strangers are restricted unless
// the owner changed it.
import { parsePolicy } from "@/features/settings/policy";
import { useAthena } from "@/lib/store/state";
import { cn } from "@/ui/cn";

const TEXT: Record<string, string> = {
  allowed: "From your allow list",
  collaborator: "From a collaborator",
  stranger: "From outside your allow list",
  bot: "From a bot",
  ci: "From CI",
};

/** The badge's words, or "" for the owner and for a class this client does not know. */
export function trustText(trust: string): string {
  return TEXT[trust] ?? "";
}

/** Whether the Policy restricts the tools of sessions started on this trust class (the default: strangers only). */
export function isRestricted(trust: string, settingsJson: string | undefined): boolean {
  const map = settingsJson ? parsePolicy(settingsJson).trust : undefined;
  if (map && typeof map === "object" && !Array.isArray(map)) {
    const v = (map as Record<string, unknown>)[trust];
    if (v === "restricted" || v === "standard") return v === "restricted";
  }
  return trust === "stranger";
}

export function TrustBadge({ trust, ticket = false, className }: { trust: string; ticket?: boolean; className?: string }) {
  const json = useAthena((s) => s.policy?.settingsJson);
  const text = trustText(trust);
  if (!text) return null;
  return (
    <span data-trust={trust} className={cn("border border-line px-1 font-mono text-xs text-muted", className)}>
      {text}
      {ticket && isRestricted(trust, json) && " · restricted tools"}
    </span>
  );
}
