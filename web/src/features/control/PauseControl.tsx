// "Pause Athena": the kill switch's button, in the desktop sidebar and the phone's top bar. It asks first, then pauses
// (no re-authentication: stopping is always safe). While Athena is paused the control is gone: the banner resumes.
import { PauseIcon } from "lucide-react";
import { useState } from "react";
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { errorMessage, pause } from "@/lib/commands/actions";
import { Button } from "@/ui/primitives/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/ui/primitives/dialog";
import { toast } from "@/ui/primitives/toast";

function PauseConfirm({ onClose }: { onClose: () => void }) {
  const [busy, setBusy] = useState(false);
  const confirm = async () => {
    setBusy(true);
    try {
      const r = await pause();
      if (r.ok) onClose();
      else
        toast({
          title: "Could not pause Athena",
          description: errorMessage(r.code),
          tone: "danger",
        });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Pause Athena?</DialogTitle>
          <DialogDescription>
            Running sessions stop at the end of their current turn, no new work starts, and replies wait. Nothing queued
            is lost.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="ghost" disabled={busy} onClick={() => onClose()}>
            Cancel
          </Button>
          <Button variant="primary" disabled={busy} onClick={() => void confirm()}>
            Pause
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function PauseControl({ variant }: { variant: "sidebar" | "phone" }) {
  const paused = useAthena((s) => s.control?.paused === true);
  const [open, setOpen] = useState(false);
  const synced = useAthena(selectSynced);
  if (!synced || (paused && !open)) return null;

  return (
    <>
      {variant === "phone" ? (
        <button
          type="button"
          aria-label="Pause Athena"
          onClick={() => setOpen(true)}
          className="inline-flex size-11 items-center justify-center rounded-md text-text hover:bg-surface-2"
        >
          <PauseIcon className="size-5" aria-hidden />
        </button>
      ) : (
        <button
          type="button"
          onClick={() => setOpen(true)}
          className="flex h-9 w-full items-center gap-2 rounded-xs px-2 text-body text-text hover:bg-surface-2"
        >
          <PauseIcon className="size-4 shrink-0" aria-hidden />
          Pause Athena
        </button>
      )}
      {open && <PauseConfirm onClose={() => setOpen(false)} />}
    </>
  );
}
