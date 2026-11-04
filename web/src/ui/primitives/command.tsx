import { Command as CommandPrimitive } from "cmdk";
import { SearchIcon } from "lucide-react";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/ui/primitives/dialog";

export function Command({ className, ...props }: ComponentProps<typeof CommandPrimitive>) {
  return (
    <CommandPrimitive
      data-slot="command"
      className={cn("flex size-full flex-col overflow-hidden bg-surface text-text", className)}
      {...props}
    />
  );
}

export function CommandDialog({
  title = "Command palette",
  description = "Search for a page or an action.",
  children,
  ...props
}: ComponentProps<typeof Dialog> & { title?: string; description?: string }) {
  return (
    <Dialog {...props}>
      <DialogContent hideClose className="mt-[12dvh] mb-auto max-w-xl overflow-hidden" bodyClassName="block gap-0 p-0">
        <DialogTitle className="sr-only">{title}</DialogTitle>
        <DialogDescription className="sr-only">{description}</DialogDescription>
        <Command>{children}</Command>
      </DialogContent>
    </Dialog>
  );
}

export function CommandInput({ className, ...props }: ComponentProps<typeof CommandPrimitive.Input>) {
  return (
    <div className="flex items-center gap-2 border-b border-line px-4" data-slot="command-input-wrapper">
      <SearchIcon className="size-4 shrink-0 text-muted" aria-hidden />
      <CommandPrimitive.Input
        data-slot="command-input"
        data-autofocus
        className={cn(
          "h-12 w-full min-w-0 bg-transparent text-body text-text outline-none placeholder:text-muted",
          className,
        )}
        {...props}
      />
    </div>
  );
}

export function CommandList({ className, ...props }: ComponentProps<typeof CommandPrimitive.List>) {
  return (
    <CommandPrimitive.List
      data-slot="command-list"
      className={cn("max-h-80 scroll-py-1 overflow-x-hidden overflow-y-auto p-1", className)}
      {...props}
    />
  );
}

export function CommandEmpty(props: ComponentProps<typeof CommandPrimitive.Empty>) {
  return <CommandPrimitive.Empty className="px-3 py-6 text-sm text-muted" {...props} />;
}

export function CommandGroup({ className, ...props }: ComponentProps<typeof CommandPrimitive.Group>) {
  return (
    <CommandPrimitive.Group
      data-slot="command-group"
      className={cn(
        "overflow-hidden py-1 [&_[cmdk-group-heading]]:px-3 [&_[cmdk-group-heading]]:py-1",
        "[&_[cmdk-group-heading]]:font-mono [&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:text-muted",
        className,
      )}
      {...props}
    />
  );
}

export function CommandSeparator({ className, ...props }: ComponentProps<typeof CommandPrimitive.Separator>) {
  return <CommandPrimitive.Separator className={cn("-mx-1 my-1 h-px bg-line-soft", className)} {...props} />;
}

export function CommandItem({ className, ...props }: ComponentProps<typeof CommandPrimitive.Item>) {
  return (
    <CommandPrimitive.Item
      data-slot="command-item"
      className={cn(
        "relative flex min-h-11 cursor-default select-none items-center gap-2 rounded-xs px-3 text-body outline-none lg:min-h-9",
        "data-[selected=true]:bg-text data-[selected=true]:text-bg",
        "data-[disabled=true]:pointer-events-none data-[disabled=true]:opacity-40 [&_svg]:size-4 [&_svg]:shrink-0",
        className,
      )}
      {...props}
    />
  );
}

export function CommandShortcut({ className, ...props }: ComponentProps<"span">) {
  return <span className={cn("ml-auto font-mono text-xs opacity-70", className)} {...props} />;
}
