import * as TabsPrimitive from "@radix-ui/react-tabs";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// Tabs sit on a hairline; the active tab is marked by a 2 px ink rule, not a filled pill.
export const Tabs = TabsPrimitive.Root;

export function TabsList({ className, ...props }: ComponentProps<typeof TabsPrimitive.List>) {
  return (
    <TabsPrimitive.List
      data-slot="tabs-list"
      // Wraps on a narrow screen rather than scrolling sideways, like the toggle group.
      className={cn("flex min-w-0 flex-wrap items-stretch gap-x-2 border-b border-line-soft", className)}
      {...props}
    />
  );
}

export function TabsTrigger({ className, ...props }: ComponentProps<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      data-slot="tabs-trigger"
      className={cn(
        "inline-flex h-11 shrink-0 items-center gap-2 border-b-2 border-transparent px-2 text-body font-medium text-muted lg:h-9",
        "transition-colors duration-(--motion-fast) hover:text-text",
        "data-[state=active]:border-text data-[state=active]:font-semibold data-[state=active]:text-text",
        "disabled:pointer-events-none disabled:opacity-40",
        // Inside the trigger, so a neighbour or the list edge never clips it.
        "focus-visible:-outline-offset-2",
        className,
      )}
      {...props}
    />
  );
}

export function TabsContent({ className, ...props }: ComponentProps<typeof TabsPrimitive.Content>) {
  return <TabsPrimitive.Content data-slot="tabs-content" className={cn("pt-4", className)} {...props} />;
}
