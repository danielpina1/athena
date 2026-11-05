import type { ReactNode } from "react";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";

// What went wrong and how to fix it, without apology: a title, the detail in mono (an error code or the server's
// words), and Retry when retrying can help.
export function ErrorState({
  title,
  detail,
  onRetry,
  action,
  className,
}: {
  title: string;
  detail?: string;
  onRetry?: () => void;
  action?: ReactNode;
  className?: string;
}) {
  return (
    <div role="alert" className={cn("flex min-w-0 flex-col items-start gap-3 border-t border-danger py-8", className)}>
      <div className="flex items-center gap-2">
        <span aria-hidden className="size-2 bg-danger" />
        <h3 className="text-lg font-semibold break-words">{title}</h3>
      </div>
      {detail && (
        <p className="w-full max-w-prose rounded-xs bg-surface-2 px-3 py-2 font-mono text-xs break-words text-text">
          {detail}
        </p>
      )}
      {(onRetry || action) && (
        <div className="flex flex-wrap gap-2">
          {onRetry && <Button onClick={onRetry}>Retry</Button>}
          {action}
        </div>
      )}
    </div>
  );
}
