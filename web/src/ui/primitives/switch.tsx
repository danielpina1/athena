import * as SwitchPrimitive from "@radix-ui/react-switch";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// A square-shouldered switch: hairline track, ink thumb; on, the track fills with ink and the thumb turns paper.
export function Switch({ className, ...props }: ComponentProps<typeof SwitchPrimitive.Root>) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      className={cn(
        "peer relative inline-flex h-6 w-11 shrink-0 items-center rounded-md border border-line bg-surface p-[3px]",
        "transition-colors duration-(--motion-fast) data-[state=checked]:bg-text",
        "disabled:cursor-not-allowed disabled:opacity-40",
        // 44 px touch target on the phone without a 44 px switch.
        "before:absolute before:-inset-x-1 before:-inset-y-2.5 before:content-[''] lg:before:hidden",
        className,
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb
        className={cn(
          "pointer-events-none block size-4 rounded-xs bg-text transition-transform duration-(--motion-fast)",
          "data-[state=checked]:translate-x-5 data-[state=checked]:bg-bg",
        )}
      />
    </SwitchPrimitive.Root>
  );
}
