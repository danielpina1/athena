import { ChevronDownIcon } from "lucide-react";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";
import { fieldClass } from "@/ui/primitives/field";

// A styled native <select>: the phone's own picker, nothing injected into the document. `className` sizes the
// field (it lands on the wrapper); options are plain <option> and <optgroup> children. An empty-valued selected
// option reads as a placeholder.
export function Select({ className, children, ...props }: ComponentProps<"select">) {
  return (
    <div data-slot="select" className={cn("relative min-w-0", className)}>
      <select
        className={cn(
          fieldClass,
          "h-11 cursor-pointer appearance-none pr-9 pl-3 lg:h-9",
          "has-[option[value='']:checked]:text-muted [&_option]:bg-surface [&_option]:text-text",
        )}
        {...props}
      >
        {children}
      </select>
      <ChevronDownIcon
        aria-hidden
        className="pointer-events-none absolute top-1/2 right-3 size-4 -translate-y-1/2 text-muted"
      />
    </div>
  );
}
