import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// A key cap: a soft hairline box around a mono glyph. Desktop-only hints hide themselves on the phone with
// `hidden lg:inline-flex` at the call site.
export function Kbd({ className, ...props }: ComponentProps<"kbd">) {
  return (
    <kbd
      className={cn(
        "inline-flex h-5 min-w-5 items-center justify-center rounded-xs border border-line-soft px-1 font-mono text-xs font-medium text-muted",
        className,
      )}
      {...props}
    />
  );
}
