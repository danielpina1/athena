import { useRef, useState } from "react";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import { errorMessage } from "@/lib/commands/actions";
import { toast } from "@/ui/primitives/toast";

/**
 * Runs one command at a time for a button: `busy` disables it while the command is in flight and a ref swallows a
 * second tap that lands before React re-renders. A failure is said in the one Toaster.
 */
export function useCommand(failed: string) {
  const [busy, setBusy] = useState(false);
  const running = useRef(false);
  /** Resolves true when the command succeeded. */
  const run = async (send: () => Promise<CommandResult>): Promise<boolean> => {
    if (running.current) return false;
    running.current = true;
    setBusy(true);
    try {
      const r = await send();
      if (!r.ok) toast({ title: failed, description: errorMessage(r.code), tone: "danger" });
      return r.ok;
    } finally {
      running.current = false;
      setBusy(false);
    }
  };
  return { busy, run };
}
