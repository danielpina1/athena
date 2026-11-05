import { cn } from "@/ui/cn";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogTitle,
  DialogTrigger,
  Modal,
  type ModalProps,
} from "@/ui/primitives/dialog";
import type { ComponentProps } from "react";

// The phone's bottom sheet: a native modal <dialog> pinned to the bottom edge. It rises in, clears the home
// indicator, and keeps its actions within thumb reach. On the desktop it stays a bottom panel of readable width.
export const Sheet = Dialog;
export const SheetTrigger = DialogTrigger;
export const SheetClose = DialogClose;

export function SheetContent({ className, bodyClassName, children, ...props }: ModalProps) {
  return (
    <Modal
      className={cn(
        "m-0 mx-auto mt-auto max-h-[85dvh] w-full max-w-xl overflow-y-auto rounded-t-md border-b-0",
        "pb-[env(safe-area-inset-bottom)] open:animate-rise-in",
        className,
      )}
      bodyClassName={cn("px-4 pt-3 pb-4", bodyClassName)}
      {...props}
    >
      <div aria-hidden className="mx-auto mb-3 h-1 w-10 rounded-xs bg-line-soft" />
      {children}
    </Modal>
  );
}

export function SheetHeader({ className, ...props }: ComponentProps<"div">) {
  return <div data-slot="sheet-header" className={cn("grid gap-1 pr-12 pb-4", className)} {...props} />;
}

export function SheetFooter({ className, ...props }: ComponentProps<"div">) {
  return <div data-slot="sheet-footer" className={cn("flex flex-col gap-2 pt-4", className)} {...props} />;
}

export function SheetTitle({ className, ...props }: ComponentProps<typeof DialogTitle>) {
  return <DialogTitle className={cn("text-lg font-semibold break-words", className)} {...props} />;
}

export function SheetDescription({ className, ...props }: ComponentProps<typeof DialogDescription>) {
  return <DialogDescription className={cn("text-sm text-muted", className)} {...props} />;
}
