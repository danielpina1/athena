import * as DialogPrimitive from "@radix-ui/react-dialog";
import { XIcon } from "lucide-react";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";
import { DialogOverlay } from "@/ui/primitives/dialog";
import { usePortalContainer } from "@/ui/primitives/portal";

// The phone's bottom sheet: it rises from the bottom edge, clears the home indicator, and keeps its actions within
// thumb reach. On the desktop it stays a bottom-anchored panel, capped to a readable width.
export const Sheet = DialogPrimitive.Root;
export const SheetTrigger = DialogPrimitive.Trigger;
export const SheetClose = DialogPrimitive.Close;

export function SheetContent({
  className,
  children,
  closeLabel = "Close",
  ...props
}: ComponentProps<typeof DialogPrimitive.Content> & { closeLabel?: string }) {
  return (
    <DialogPrimitive.Portal container={usePortalContainer()}>
      <DialogOverlay />
      <DialogPrimitive.Content
        data-slot="sheet-content"
        className={cn(
          "fixed inset-x-0 bottom-0 z-50 mx-auto flex max-h-[85dvh] w-full max-w-xl flex-col",
          "rounded-t-md border border-b-0 border-line bg-surface text-text",
          "pb-[env(safe-area-inset-bottom)]",
          "data-[state=open]:animate-rise-in data-[state=closed]:animate-rise-out",
          className,
        )}
        {...props}
      >
        <div aria-hidden className="mx-auto mt-2 h-1 w-10 shrink-0 rounded-xs bg-line-soft" />
        <div className="min-h-0 flex-1 overflow-y-auto px-4 pt-3 pb-4">{children}</div>
        <DialogPrimitive.Close
          className="absolute top-2 right-2 inline-flex size-11 items-center justify-center rounded-md text-muted hover:bg-surface-2 hover:text-text"
          aria-label={closeLabel}
        >
          <XIcon className="size-4" aria-hidden />
        </DialogPrimitive.Close>
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  );
}

export function SheetHeader({ className, ...props }: ComponentProps<"div">) {
  return <div data-slot="sheet-header" className={cn("grid gap-1 pr-12 pb-4", className)} {...props} />;
}

export function SheetFooter({ className, ...props }: ComponentProps<"div">) {
  return <div data-slot="sheet-footer" className={cn("flex flex-col gap-2 pt-4", className)} {...props} />;
}

export function SheetTitle({ className, ...props }: ComponentProps<typeof DialogPrimitive.Title>) {
  return <DialogPrimitive.Title className={cn("text-lg font-semibold break-words", className)} {...props} />;
}

export function SheetDescription({ className, ...props }: ComponentProps<typeof DialogPrimitive.Description>) {
  return <DialogPrimitive.Description className={cn("text-sm text-muted", className)} {...props} />;
}
