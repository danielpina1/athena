import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";
import { fieldClass } from "@/ui/primitives/field";

export function Textarea({ className, rows = 3, ...props }: ComponentProps<"textarea">) {
  return (
    <textarea
      data-slot="textarea"
      rows={rows}
      className={cn(fieldClass, "field-sizing-content min-h-20 resize-y px-3 py-2 break-words", className)}
      {...props}
    />
  );
}
