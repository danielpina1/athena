import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";
import { fieldClass } from "@/ui/primitives/field";

export function Input({ className, type = "text", ...props }: ComponentProps<"input">) {
  return <input data-slot="input" type={type} className={cn(fieldClass, "h-11 px-3 lg:h-9", className)} {...props} />;
}
