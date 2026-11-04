import { XIcon } from "lucide-react";
import { useEffect, useSyncExternalStore } from "react";
import { createPortal } from "react-dom";
import { cn } from "@/ui/cn";
import { usePortalContainer } from "@/ui/primitives/portal";

// A small own toaster (no sonner): an ink slip at the bottom, above the phone's navigation. A toast confirms what
// just happened in the words of the action that did it ("Approved"), and a failure says what to do next.

export type ToastTone = "default" | "danger";
export type ToastInput = {
  title: string;
  description?: string;
  tone?: ToastTone;
  action?: { label: string; onClick: () => void };
  /** Milliseconds on screen; 0 keeps it until dismissed. */
  duration?: number;
};
type Toast = ToastInput & { id: number };

const MAX = 3;
let toasts: Toast[] = [];
let nextId = 1;
const listeners = new Set<() => void>();
const emit = () => {
  for (const l of listeners) l();
};

export function toast(input: ToastInput): number {
  const id = nextId++;
  toasts = [...toasts, { duration: input.tone === "danger" ? 8000 : 4000, ...input, id }].slice(-MAX);
  emit();
  return id;
}

export function dismissToast(id: number) {
  toasts = toasts.filter((t) => t.id !== id);
  emit();
}

const subscribe = (l: () => void) => {
  listeners.add(l);
  return () => listeners.delete(l);
};
const snapshot = () => toasts;

export function Toaster() {
  const list = useSyncExternalStore(subscribe, snapshot);
  const container = usePortalContainer();
  return createPortal(
    <section
      aria-label="Notifications"
      className={cn(
        "pointer-events-none fixed inset-x-0 z-[60] flex flex-col items-center gap-2 px-4",
        "bottom-[calc(env(safe-area-inset-bottom)+5rem)] lg:inset-x-auto lg:right-6 lg:bottom-6 lg:items-end lg:px-0",
      )}
    >
      {list.map((t) => (
        <ToastItem key={t.id} toast={t} />
      ))}
    </section>,
    container ?? document.body,
  );
}

function ToastItem({ toast: t }: { toast: Toast }) {
  useEffect(() => {
    if (!t.duration) return;
    const timer = setTimeout(() => dismissToast(t.id), t.duration);
    return () => clearTimeout(timer);
  }, [t.id, t.duration]);

  return (
    <div
      role={t.tone === "danger" ? "alert" : "status"}
      className={cn(
        "pointer-events-auto flex w-full max-w-sm min-w-0 animate-pop-in items-start gap-3 rounded-md border py-3 pr-2 pl-4",
        // A failure is a paper slip with a danger rule (the rule keeps 3:1 on paper in both themes).
        t.tone === "danger" ? "border-danger border-l-4 bg-surface text-text" : "border-text bg-text text-bg",
      )}
    >
      <div className="min-w-0 flex-1 py-1">
        <p className="text-sm font-semibold break-words">{t.title}</p>
        {t.description && <p className="mt-1 text-sm break-words opacity-80">{t.description}</p>}
      </div>
      {t.action && (
        <button
          type="button"
          className={cn(
            "h-11 shrink-0 rounded-xs border border-current lg:h-9 px-3 text-sm font-semibold",
            t.tone === "danger" ? "hover:bg-text hover:text-bg" : "hover:bg-bg hover:text-text",
          )}
          onClick={() => {
            t.action?.onClick();
            dismissToast(t.id);
          }}
        >
          {t.action.label}
        </button>
      )}
      <button
        type="button"
        aria-label="Dismiss"
        className="inline-flex size-11 shrink-0 items-center justify-center rounded-xs opacity-80 lg:size-9 hover:opacity-100"
        onClick={() => dismissToast(t.id)}
      >
        <XIcon className="size-4" aria-hidden />
      </button>
    </div>
  );
}
