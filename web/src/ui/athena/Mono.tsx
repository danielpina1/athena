import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// IBM Plex Mono for ids, commands, metadata and session output: tabular figures, muted unless it is the content.
export function Mono({
  className,
  tone = "muted",
  ...props
}: ComponentProps<"span"> & { tone?: "muted" | "default" }) {
  return (
    <span
      className={cn(
        "font-mono text-xs tabular-nums break-words",
        tone === "muted" ? "text-muted" : "text-text",
        className,
      )}
      {...props}
    />
  );
}
