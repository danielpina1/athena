import { useLayoutEffect, useRef, useState } from "react";
import { cn } from "@/ui/cn";
import { Popover, PopoverContent, PopoverTrigger } from "@/ui/primitives/popover";

// Clamps hostile or huge text (a 2,000-character title, an unbroken token, RTL) to a number of lines. It wraps
// anywhere rather than widening its column, and when the clamp hides text a "Show all" popover holds the rest.

const CLAMP = {
  1: "line-clamp-1",
  2: "line-clamp-2",
  3: "line-clamp-3",
  4: "line-clamp-4",
  5: "line-clamp-5",
  6: "line-clamp-6",
} as const;

export type TruncateProps = {
  text: string;
  lines?: keyof typeof CLAMP;
  className?: string;
  /** Keeps the text's own line breaks (a report, a tool result) instead of running them together. */
  breaks?: boolean;
};

export function Truncate({ text, lines = 2, className, breaks = false }: TruncateProps) {
  const ref = useRef<HTMLParagraphElement>(null);
  const [clamped, setClamped] = useState(false);

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => setClamped(el.scrollHeight > el.clientHeight + 1);
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [text, lines]);

  return (
    <div className={cn("min-w-0 max-w-full", className)}>
      <p ref={ref} dir="auto" className={cn(CLAMP[lines], "break-words wrap-anywhere", breaks && "whitespace-pre-line")}>
        {text}
      </p>
      {clamped && (
        <Popover>
          <PopoverTrigger className="mt-1 inline-flex min-h-11 items-center text-sm font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0">
            Show all
          </PopoverTrigger>
          <PopoverContent className="max-h-[60dvh] w-[min(36rem,calc(100vw-2rem))] overflow-y-auto">
            <p dir="auto" className="text-body break-words wrap-anywhere whitespace-pre-wrap">
              {text}
            </p>
          </PopoverContent>
        </Popover>
      )}
    </div>
  );
}
