import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// A hairline card. `attention` adds the 3 px accent rule on the left: the card holds something the owner must act
// on. `selected` is the ink rule a list uses for its current row.
export function Card({
  className,
  tone = "default",
  selected = false,
  ...props
}: ComponentProps<"div"> & { tone?: "default" | "attention"; selected?: boolean }) {
  return (
    <div
      data-slot="card"
      data-tone={tone}
      className={cn(
        "min-w-0 rounded-md border border-line bg-surface p-4 text-text",
        tone === "attention" && "border-l-[3px] border-l-accent",
        selected && "bg-surface-2",
        selected && tone !== "attention" && "border-l-[3px] border-l-text",
        className,
      )}
      {...props}
    />
  );
}
