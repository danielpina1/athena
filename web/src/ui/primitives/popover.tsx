import * as PopoverPrimitive from "@radix-ui/react-popover";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";
import { usePortalContainer } from "@/ui/primitives/portal";

// Non-modal: a modal Radix popover mounts react-remove-scroll, which injects a <style> element the CSP forbids.
export function Popover(props: Omit<ComponentProps<typeof PopoverPrimitive.Root>, "modal">) {
  return <PopoverPrimitive.Root modal={false} {...props} />;
}
export const PopoverTrigger = PopoverPrimitive.Trigger;
export const PopoverAnchor = PopoverPrimitive.Anchor;

export function PopoverContent({
  className,
  align = "start",
  sideOffset = 4,
  ...props
}: ComponentProps<typeof PopoverPrimitive.Content>) {
  return (
    <PopoverPrimitive.Portal container={usePortalContainer()}>
      <PopoverPrimitive.Content
        data-slot="popover-content"
        align={align}
        sideOffset={sideOffset}
        collisionPadding={16}
        className={cn(
          "z-50 w-80 max-w-[calc(100vw-2rem)] rounded-md border border-line bg-surface p-4 text-text",
          "data-[state=open]:animate-pop-in data-[state=closed]:animate-pop-out",
          className,
        )}
        {...props}
      />
    </PopoverPrimitive.Portal>
  );
}
