import { useState, useSyncExternalStore } from "react";
import { reauth } from "@/lib/auth/session";
import { Button } from "@/ui/primitives/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/ui/primitives/dialog";

// The step-up prompt. The outbox calls requestReauth when the server wants a fresh passkey for a sensitive command
// (boot.ts registers it with outbox.setReauthHandler); the dialog explains why, runs the ceremony, and answers true so
// the outbox resends the held command by its own id, or false (Cancel, Esc) so it fails with rollback. The request
// lives at module level so it does not depend on where the dialog is mounted.

let pending: { promise: Promise<boolean>; settle: (ok: boolean) => void } | null = null;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((l) => l());
const subscribe = (l: () => void) => (listeners.add(l), () => void listeners.delete(l));

/** Asks the owner for the passkey again; every ask made while the dialog is open shares its answer. */
export function requestReauth(): Promise<boolean> {
  if (pending) return pending.promise;
  let settle!: (ok: boolean) => void;
  const promise = new Promise<boolean>((r) => (settle = r));
  pending = {
    promise,
    settle: (ok) => {
      if (pending?.promise !== promise) return;
      pending = null;
      settle(ok);
      emit();
    },
  };
  emit();
  return promise;
}

/** Closes the dialog with "no": the owner was signed out, or left. */
export function cancelReauth(): void {
  pending?.settle(false);
}

export function ReauthDialog() {
  const open = useSyncExternalStore(subscribe, () => pending !== null);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);

  const confirm = async () => {
    setBusy(true);
    setFailed(false);
    try {
      await reauth();
      pending?.settle(true);
    } catch (e) {
      // Dismissing the browser's own prompt is an answer, not an error.
      setFailed(!(e instanceof Error && "code" in e && e.code === "cancelled"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && !busy && cancelReauth()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Confirm it is you</DialogTitle>
          <DialogDescription>
            This change is sensitive, so Athena asks for your passkey again before it goes through.
          </DialogDescription>
        </DialogHeader>
        {failed && (
          <p role="alert" className="text-sm text-danger">
            Could not confirm with your passkey. Try again.
          </p>
        )}
        <DialogFooter>
          <Button variant="ghost" disabled={busy} onClick={cancelReauth}>
            Cancel
          </Button>
          <Button variant="accent" disabled={busy} onClick={() => void confirm()}>
            Confirm with passkey
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
