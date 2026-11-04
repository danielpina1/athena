import { Slot } from "@radix-ui/react-slot";
import { XIcon } from "lucide-react";
import {
  type ComponentProps,
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";
import { cn } from "@/ui/cn";
import { PortalContainer } from "@/ui/primitives/portal";

// Modal dialogs on the native <dialog> element and showModal(): the browser gives the top layer, the inert
// background, focus on open and Esc. Nothing is injected into the document (the CSP forbids <style> elements, which
// Radix's scroll lock adds); tokens.css locks page scroll with `body:has(dialog[open])`. Focus returns to the
// element that opened the dialog when it closes, or when the dialog unmounts while open.
//
// The <dialog> renders where DialogContent is, so it lives and dies with its parent. Do not put a DialogContent
// inside a DropdownMenuContent (or anything else that unmounts on close): the menu closing would take the dialog
// with it. Keep the Dialog outside the menu, controlled, and open it from the item's onSelect.

// The open modals, most recent last: the Toaster renders into the topmost one so toasts stay above its backdrop.
const modals: HTMLDialogElement[] = [];
const modalListeners = new Set<() => void>();
const modalsChanged = () => {
  for (const l of modalListeners) l();
};
export function subscribeModals(listener: () => void) {
  modalListeners.add(listener);
  return () => {
    modalListeners.delete(listener);
  };
}
export const topModal = (): HTMLDialogElement | null => modals.at(-1) ?? null;

function pushModal(d: HTMLDialogElement) {
  if (!modals.includes(d)) modals.push(d);
  modalsChanged();
}
function dropModal(d: HTMLDialogElement) {
  const i = modals.indexOf(d);
  if (i < 0) return;
  modals.splice(i, 1);
  modalsChanged();
}

type DialogState = {
  open: boolean;
  setOpen: (open: boolean) => void;
  titleId: string;
  descriptionId: string;
};

const DialogContext = createContext<DialogState | null>(null);

function useDialog(part: string): DialogState {
  const ctx = useContext(DialogContext);
  if (!ctx) throw new Error(`${part} must be inside <Dialog> or <Sheet>`);
  return ctx;
}

export function Dialog({
  open: controlled,
  defaultOpen = false,
  onOpenChange,
  children,
}: {
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  children: ReactNode;
}) {
  const [uncontrolled, setUncontrolled] = useState(defaultOpen);
  const open = controlled ?? uncontrolled;
  const setOpen = useCallback(
    (next: boolean) => {
      if (controlled === undefined) setUncontrolled(next);
      onOpenChange?.(next);
    },
    [controlled, onOpenChange],
  );
  const titleId = useId();
  const descriptionId = useId();
  return (
    <DialogContext.Provider value={{ open, setOpen, titleId, descriptionId }}>{children}</DialogContext.Provider>
  );
}

export function DialogTrigger({ asChild, onClick, ...props }: ComponentProps<"button"> & { asChild?: boolean }) {
  const { open, setOpen } = useDialog("DialogTrigger");
  const Comp = asChild ? Slot : "button";
  return (
    <Comp
      type={asChild ? undefined : "button"}
      aria-haspopup="dialog"
      aria-expanded={open}
      onClick={(e: React.MouseEvent<HTMLButtonElement>) => {
        onClick?.(e);
        if (!e.defaultPrevented) setOpen(true);
      }}
      {...props}
    />
  );
}

export function DialogClose({ asChild, onClick, ...props }: ComponentProps<"button"> & { asChild?: boolean }) {
  const { setOpen } = useDialog("DialogClose");
  const Comp = asChild ? Slot : "button";
  return (
    <Comp
      type={asChild ? undefined : "button"}
      onClick={(e: React.MouseEvent<HTMLButtonElement>) => {
        onClick?.(e);
        if (!e.defaultPrevented) setOpen(false);
      }}
      {...props}
    />
  );
}

function restoreFocus(opener: { current: HTMLElement | null }) {
  const target = opener.current;
  opener.current = null;
  if (target?.isConnected) target.focus();
}

export type ModalProps = Omit<ComponentProps<"dialog">, "open"> & {
  closeLabel?: string;
  /** Hide the corner close button (Esc and the backdrop still close). */
  hideClose?: boolean;
  /** Classes for the padded inner box; the <dialog> itself only frames it. */
  bodyClassName?: string;
};

/** The shared modal surface behind DialogContent and SheetContent. */
export function Modal({
  className,
  bodyClassName,
  children,
  closeLabel = "Close",
  hideClose = false,
  ...props
}: ModalProps) {
  const { open, setOpen, titleId, descriptionId } = useDialog("DialogContent");
  const ref = useRef<HTMLDialogElement | null>(null);
  const opener = useRef<HTMLElement | null>(null);
  // The dialog while it is shown. React detaches `ref` before an unmount's effect cleanups run, so they use this.
  const shown = useRef<HTMLDialogElement | null>(null);
  const [el, setEl] = useState<HTMLDialogElement | null>(null);
  const attach = useCallback((node: HTMLDialogElement | null) => {
    ref.current = node;
    setEl(node);
  }, []);

  useEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    if (open && !dialog.open) {
      opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      dialog.showModal();
      shown.current = dialog;
      pushModal(dialog);
      // showModal focuses the first focusable element; a child marked data-autofocus (a search field) wins.
      dialog.querySelector<HTMLElement>("[data-autofocus]")?.focus();
    } else if (!open && dialog.open) {
      dialog.close();
      shown.current = null;
      dropModal(dialog);
    }
  }, [open]);

  // Closed by any path (Esc, the backdrop, a Close button, the owner's state): give focus back to the opener.
  useEffect(() => {
    if (!open) restoreFocus(opener);
  }, [open]);

  // Unmounted while open (its parent went away): leave the modal stack and still give focus back.
  useEffect(
    () => () => {
      if (shown.current) dropModal(shown.current);
      shown.current = null;
      restoreFocus(opener);
    },
    [],
  );

  // A press that starts inside the box and ends on the backdrop (a text selection dragged out) is not a backdrop
  // click: only a press and a click both on the <dialog> element itself close it.
  const pressedBackdrop = useRef(false);

  return (
    <dialog
      ref={attach}
      aria-labelledby={titleId}
      aria-describedby={descriptionId}
      // cancel and close do not bubble in the DOM, but React re-dispatches them up its tree: a nested dialog's
      // events must not close this one.
      onCancel={(e) => {
        if (e.target !== e.currentTarget) return;
        e.preventDefault();
        setOpen(false);
      }}
      // The browser can close a modal on its own (a repeated Esc); keep the state in step.
      onClose={(e) => {
        if (e.target !== e.currentTarget) return;
        dropModal(e.currentTarget);
        if (open) setOpen(false);
      }}
      onPointerDown={(e) => {
        pressedBackdrop.current = e.target === e.currentTarget;
      }}
      onClick={(e) => {
        const backdrop = pressedBackdrop.current && e.target === e.currentTarget;
        pressedBackdrop.current = false;
        if (backdrop) setOpen(false);
      }}
      className={cn(
        "border border-line bg-surface p-0 text-text backdrop:bg-stage/80",
        "open:animate-pop-in backdrop:animate-fade-in",
        className,
      )}
      {...props}
    >
      {/* Overlays opened inside the dialog (a tooltip, a menu) portal into it, so they share its top layer. */}
      <PortalContainer.Provider value={el}>
        {open && <div className={cn("relative", bodyClassName)}>{children}</div>}
        {open && !hideClose && (
          <button
            type="button"
            onClick={() => setOpen(false)}
            aria-label={closeLabel}
            className="absolute top-2 right-2 inline-flex size-11 items-center justify-center rounded-md text-muted hover:bg-surface-2 hover:text-text lg:size-8"
          >
            <XIcon className="size-4" aria-hidden />
          </button>
        )}
      </PortalContainer.Provider>
    </dialog>
  );
}

export function DialogContent({ className, bodyClassName, ...props }: ModalProps) {
  return (
    <Modal
      className={cn("m-auto max-h-[calc(100dvh-2rem)] w-[calc(100%-2rem)] max-w-lg overflow-y-auto rounded-md", className)}
      bodyClassName={cn("grid gap-4 p-6", bodyClassName)}
      {...props}
    />
  );
}

export function DialogHeader({ className, ...props }: ComponentProps<"div">) {
  return <div data-slot="dialog-header" className={cn("grid gap-1 pr-10", className)} {...props} />;
}

export function DialogFooter({ className, ...props }: ComponentProps<"div">) {
  return (
    <div
      data-slot="dialog-footer"
      className={cn("flex flex-col-reverse gap-2 sm:flex-row sm:justify-end", className)}
      {...props}
    />
  );
}

export function DialogTitle({ className, ...props }: ComponentProps<"h2">) {
  const { titleId } = useDialog("DialogTitle");
  return <h2 id={titleId} className={cn("text-lg font-semibold break-words", className)} {...props} />;
}

export function DialogDescription({ className, ...props }: ComponentProps<"p">) {
  const { descriptionId } = useDialog("DialogDescription");
  return <p id={descriptionId} className={cn("text-sm text-muted", className)} {...props} />;
}
