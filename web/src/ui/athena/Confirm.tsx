import { useEffect, useRef, useState } from "react";
import { cn } from "@/ui/cn";
import { Button, type ButtonProps } from "@/ui/primitives/button";

// A destructive action (Stop, Revoke, Reset prompt) in two clicks: the first arms the button and names exactly what
// will happen; the second does it. It disarms on Escape, on blur and after a few seconds, and a click within
// ARM_DELAY of arming (a double-click, a double-tap) does not count as the second.

const ARM_DELAY = 300;
const DISARM_AFTER = 4000;

export type ConfirmProps = Omit<ButtonProps, "onClick" | "children" | "variant"> & {
  label: string;
  /** What the second click does, spelled out: "Stop this Ticket". */
  confirmLabel: string;
  onConfirm: () => void;
};

export function Confirm({ label, confirmLabel, onConfirm, onBlur, onKeyDown, className, ...props }: ConfirmProps) {
  const [armedAt, setArmedAt] = useState<number | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => () => clearTimeout(timer.current), []);

  const disarm = () => {
    clearTimeout(timer.current);
    setArmedAt(null);
  };

  const click = () => {
    const t = performance.now();
    if (armedAt === null) {
      setArmedAt(t);
      clearTimeout(timer.current);
      timer.current = setTimeout(disarm, DISARM_AFTER);
      return;
    }
    if (t - armedAt < ARM_DELAY) return;
    disarm();
    onConfirm();
  };

  const armed = armedAt !== null;
  return (
    <>
      <Button
        {...props}
        variant={armed ? "danger" : "outline"}
        onClick={click}
        onBlur={(e) => {
          disarm();
          onBlur?.(e);
        }}
        onKeyDown={(e) => {
          // Esc disarms and nothing else: preventing the key's default keeps an enclosing modal from cancelling.
          if (e.key === "Escape" && armed) {
            e.preventDefault();
            e.stopPropagation();
            disarm();
          }
          onKeyDown?.(e);
        }}
        className={cn(armed && "bg-danger text-bg", className)}
      >
        {armed ? confirmLabel : label}
      </Button>
      <span role="status" className="sr-only">
        {armed ? `Press again to ${confirmLabel.toLowerCase()}.` : ""}
      </span>
    </>
  );
}
