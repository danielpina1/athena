import { cn } from "@/ui/cn";

/** The one-line warning on a file whose name or lines had bidirectional controls, now shown as ⟪U+…⟫ markers. */
export function BidiWarning({ className }: { className?: string }) {
  return (
    <p role="note" className={cn("rounded-xs border border-danger px-3 py-1 text-sm text-danger", className)}>
      Hidden text-direction characters, shown as ⟪U+…⟫: this may read differently than it runs.
    </p>
  );
}
