import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";

// Hairline by default. `accent` is for the one action that answers the owner's attention (Approve, Send an
// answer); `primary` is the ink-filled main action of a view that needs no one; `danger` is for destructive
// actions, which go through Confirm.
export const buttonVariants = cva(
  [
    "inline-flex shrink-0 select-none items-center justify-center gap-2 whitespace-nowrap rounded-md border",
    "font-sans font-semibold transition-colors duration-(--motion-fast)",
    "disabled:pointer-events-none disabled:opacity-40",
    "[&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0",
  ],
  {
    variants: {
      variant: {
        outline: "border-line bg-surface text-text hover:bg-surface-2 active:bg-stage",
        accent: "border-accent bg-accent text-accent-fg hover:border-text",
        primary: "border-text bg-text text-bg hover:bg-muted hover:border-muted",
        ghost: "border-transparent bg-transparent text-text hover:bg-surface-2 active:bg-stage",
        danger: "border-danger bg-surface text-danger hover:bg-danger hover:text-bg",
      },
      // 44 px touch targets on the phone; denser on the desktop.
      size: {
        md: "h-11 px-4 text-body lg:h-9 lg:px-3",
        sm: "h-11 px-3 text-sm lg:h-7 lg:px-2",
        icon: "size-11 lg:size-9",
      },
    },
    defaultVariants: { variant: "outline", size: "md" },
  },
);

export type ButtonProps = ComponentProps<"button"> & VariantProps<typeof buttonVariants> & { asChild?: boolean };

export function Button({ className, variant, size, asChild = false, type = "button", ...props }: ButtonProps) {
  const Comp = asChild ? Slot : "button";
  return (
    <Comp
      data-slot="button"
      type={asChild ? undefined : type}
      className={cn(buttonVariants({ variant, size }), className)}
      {...props}
    />
  );
}
