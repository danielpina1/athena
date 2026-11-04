import { useEffect, useRef, useState } from "react";
import { AuthError, conditionalAvailable, signIn } from "@/lib/auth/session";
import { Button } from "@/ui/primitives/button";
import { Input } from "@/ui/primitives/input";

// Sign-in with a passkey. A browser that can offer passkeys in a field's autofill gets the ceremony started at once,
// so the owner picks the passkey from the field; the button runs the ordinary prompt and is always there.
export function SignIn({ onSignedIn, notice }: { onSignedIn: () => void; notice?: string }) {
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const autofill = useRef<AbortController | null>(null);
  // The latest callback, so the autofill ceremony started once does not call a stale one.
  const done = useRef(onSignedIn);
  useEffect(() => {
    done.current = onSignedIn;
  });

  useEffect(() => {
    const ctl = new AbortController();
    autofill.current = ctl;
    void (async () => {
      if (!(await conditionalAvailable()) || ctl.signal.aborted) return;
      try {
        await signIn({ conditional: true, signal: ctl.signal });
        if (!ctl.signal.aborted) done.current();
      } catch {
        // Aborted by the button or by leaving the page, or the owner did not pick one: the button remains.
      }
    })();
    return () => ctl.abort();
  }, []);

  const click = async () => {
    autofill.current?.abort();
    setBusy(true);
    setFailed(false);
    try {
      await signIn();
      done.current();
    } catch (e) {
      setFailed(!(e instanceof AuthError && e.code === "cancelled"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="mx-auto grid w-full max-w-sm gap-4 px-4 py-16">
      <h1 className="text-2xl font-semibold">Athena</h1>
      {notice && <p className="font-semibold">{notice}</p>}
      <p className="text-muted">Sign in with the passkey you enrolled on this device.</p>
      <label className="grid gap-1 text-sm font-semibold">
        Passkey
        <Input autoComplete="username webauthn" placeholder="Choose a passkey" />
      </label>
      {failed && (
        <p role="alert" className="text-sm text-danger">
          Could not sign in. Try again, or run <code className="font-mono">athena web enroll</code> for a new link.
        </p>
      )}
      <Button variant="accent" disabled={busy} onClick={() => void click()}>
        Sign in with passkey
      </Button>
    </main>
  );
}
