import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// Native overflow with Precision scrollbars (tokens.css). Focusable by default, so a keyboard can scroll it.
export function ScrollArea({ className, tabIndex = 0, ...props }: ComponentProps<"div">) {
  return (
    <div
      data-slot="scroll-area"
      tabIndex={tabIndex}
      className={cn("min-w-0 overflow-auto overscroll-contain", className)}
      {...props}
    />
  );
}
