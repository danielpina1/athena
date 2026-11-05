import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// Loading placeholders, never spinners: a flat --surface-2 block in the shape of what is coming.
export function Skeleton({ className, ...props }: ComponentProps<"div">) {
  return (
    <div
      data-slot="skeleton"
      aria-hidden="true"
      className={cn("animate-pulse rounded-xs bg-surface-2", className)}
      {...props}
    />
  );
}
