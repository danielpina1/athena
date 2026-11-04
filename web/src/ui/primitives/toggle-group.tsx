import * as ToggleGroupPrimitive from "@radix-ui/react-toggle-group";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// A segmented control: hairline segments sharing their borders; the chosen segment is ink-filled. On a narrow
// screen the segments wrap onto a second row instead of scrolling sideways.
export function ToggleGroup({ className, ...props }: ComponentProps<typeof ToggleGroupPrimitive.Root>) {
  return (
    <ToggleGroupPrimitive.Root
      data-slot="toggle-group"
      className={cn("inline-flex max-w-full flex-wrap items-stretch pb-px pl-px", className)}
      {...props}
    />
  );
}

export function ToggleGroupItem({ className, ...props }: ComponentProps<typeof ToggleGroupPrimitive.Item>) {
  return (
    <ToggleGroupPrimitive.Item
      data-slot="toggle-group-item"
      className={cn(
        "-mb-px -ml-px inline-flex h-11 shrink-0 items-center justify-center gap-2 border border-line px-3 text-sm font-medium text-text lg:h-8",
        "first:rounded-l-md last:rounded-r-md",
        "transition-colors duration-(--motion-fast) hover:bg-surface-2",
        "data-[state=on]:bg-text data-[state=on]:text-bg data-[state=on]:font-semibold",
        "disabled:pointer-events-none disabled:opacity-40 [&_svg]:size-4",
        // The focus outline must sit above the neighbouring segments.
        "focus-visible:relative focus-visible:z-10",
        className,
      )}
      {...props}
    />
  );
}
