import * as TabsPrimitive from "@radix-ui/react-tabs";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// Tabs sit on a hairline; the active tab is marked by a 2 px ink rule, not a filled pill.
export const Tabs = TabsPrimitive.Root;

export function TabsList({ className, ...props }: ComponentProps<typeof TabsPrimitive.List>) {
  return (
    <TabsPrimitive.List
      data-slot="tabs-list"
      className={cn("flex min-w-0 items-stretch gap-4 overflow-x-auto overflow-y-hidden border-b border-line-soft [scrollbar-width:none]", className)}
      {...props}
    />
  );
}

export function TabsTrigger({ className, ...props }: ComponentProps<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      data-slot="tabs-trigger"
      className={cn(
        "inline-flex h-11 shrink-0 items-center gap-2 border-b-2 border-transparent text-body font-medium text-muted lg:h-9",
        "transition-colors duration-(--motion-fast) hover:text-text",
        "data-[state=active]:border-text data-[state=active]:font-semibold data-[state=active]:text-text",
        "disabled:pointer-events-none disabled:opacity-40",
        className,
      )}
      {...props}
    />
  );
}

export function TabsContent({ className, ...props }: ComponentProps<typeof TabsPrimitive.Content>) {
  return <TabsPrimitive.Content data-slot="tabs-content" className={cn("pt-4", className)} {...props} />;
}
