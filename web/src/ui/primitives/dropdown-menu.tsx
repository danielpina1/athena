import * as Menu from "@radix-ui/react-dropdown-menu";
import { CheckIcon } from "lucide-react";
import type { ComponentProps } from "react";
import { cn } from "@/ui/cn";
import { usePortalContainer } from "@/ui/primitives/portal";

// Non-modal: a modal Radix menu mounts react-remove-scroll, which injects a <style> element the CSP forbids.
export function DropdownMenu(props: Omit<ComponentProps<typeof Menu.Root>, "modal">) {
  return <Menu.Root modal={false} {...props} />;
}
export const DropdownMenuTrigger = Menu.Trigger;
export const DropdownMenuGroup = Menu.Group;
export const DropdownMenuRadioGroup = Menu.RadioGroup;

export function DropdownMenuContent({ className, sideOffset = 4, ...props }: ComponentProps<typeof Menu.Content>) {
  return (
    <Menu.Portal container={usePortalContainer()}>
      <Menu.Content
        data-slot="dropdown-menu-content"
        sideOffset={sideOffset}
        className={cn(
          "z-50 max-h-(--radix-dropdown-menu-content-available-height) min-w-48 overflow-y-auto",
          "rounded-md border border-line bg-surface p-1 text-text",
          "data-[state=open]:animate-pop-in data-[state=closed]:animate-pop-out",
          className,
        )}
        {...props}
      />
    </Menu.Portal>
  );
}

const itemClass = [
  "relative flex min-h-11 cursor-default select-none items-center gap-2 rounded-xs px-2 py-2 text-body outline-none lg:min-h-8 lg:py-1",
  "data-highlighted:bg-text data-highlighted:text-bg data-disabled:pointer-events-none data-disabled:opacity-40",
  "[&_svg]:size-4 [&_svg]:shrink-0",
].join(" ");

export function DropdownMenuItem({
  className,
  variant = "default",
  ...props
}: ComponentProps<typeof Menu.Item> & { variant?: "default" | "danger" }) {
  return (
    <Menu.Item
      data-slot="dropdown-menu-item"
      className={cn(
        itemClass,
        variant === "danger" && "text-danger data-highlighted:bg-danger data-highlighted:text-bg",
        className,
      )}
      {...props}
    />
  );
}

export function DropdownMenuCheckboxItem({
  className,
  children,
  ...props
}: ComponentProps<typeof Menu.CheckboxItem>) {
  return (
    <Menu.CheckboxItem className={cn(itemClass, "pl-8", className)} {...props}>
      <span className="absolute left-2 flex size-4 items-center justify-center">
        <Menu.ItemIndicator>
          <CheckIcon aria-hidden />
        </Menu.ItemIndicator>
      </span>
      {children}
    </Menu.CheckboxItem>
  );
}

export function DropdownMenuRadioItem({ className, children, ...props }: ComponentProps<typeof Menu.RadioItem>) {
  return (
    <Menu.RadioItem className={cn(itemClass, "pl-8", className)} {...props}>
      <span className="absolute left-2 flex size-4 items-center justify-center">
        <Menu.ItemIndicator>
          <span className="block size-2 bg-current" />
        </Menu.ItemIndicator>
      </span>
      {children}
    </Menu.RadioItem>
  );
}

export function DropdownMenuLabel({ className, ...props }: ComponentProps<typeof Menu.Label>) {
  return <Menu.Label className={cn("px-2 py-1 font-mono text-xs text-muted", className)} {...props} />;
}

export function DropdownMenuSeparator({ className, ...props }: ComponentProps<typeof Menu.Separator>) {
  return <Menu.Separator className={cn("-mx-1 my-1 h-px bg-line-soft", className)} {...props} />;
}

export function DropdownMenuShortcut({ className, ...props }: ComponentProps<"span">) {
  return <span className={cn("ml-auto pl-4 font-mono text-xs opacity-70", className)} {...props} />;
}
