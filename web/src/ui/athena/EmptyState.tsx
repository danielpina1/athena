import type { ReactNode } from "react";
import { cn } from "@/ui/cn";

// An empty view is an invitation to act: what would be here, in one sentence, and the action that fills it.
export function EmptyState({
  title,
  description,
  action,
  className,
}: {
  title: string;
  description?: string;
  action?: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex min-w-0 flex-col items-start gap-3 border-t border-line py-8", className)}>
      <div className="grid gap-1">
        <h3 className="text-lg font-semibold break-words">{title}</h3>
        {description && <p className="max-w-prose text-body text-muted break-words">{description}</p>}
      </div>
      {action}
    </div>
  );
}
