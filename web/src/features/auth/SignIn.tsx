import { useEffect, useRef, useState } from "react";
import { AuthError, conditionalAvailable, signIn } from "@/lib/auth/session";
import { Button } from "@/ui/primitives/button";
import { Input } from "@/ui/primitives/input";

/** The server keeps a login ceremony (and its cookie) 5 minutes: the autofill one is renewed before that. */
export const REARM_BEFORE_EXPIRY_MS = 270_000;
const REARM_AFTER_CANCEL_MS = 1_000;
const REARM_AFTER_FAILURE_MS = 5_000;

type Autofill = { pause(): Promise<void>; resume(): void };

// Sign-in with a passkey. A browser that can offer passkeys in a field's autofill gets the ceremony started at once,
// so the owner picks the passkey from the field; the button runs the ordinary prompt and is always there.
// The autofill ceremony outlives the server's: it is started again after it ends for any reason but our own abort,
// and before the server forgets it. At most one login/begin is in flight at a time (the server keeps one ceremony per
// browser, in a cookie), so the button waits for the autofill run to settle before it begins its own.
export function SignIn({ onSignedIn, notice }: { onSignedIn: () => void; notice?: string }) {
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const autofill = useRef<Autofill | null>(null);
  // The latest callback, so a ceremony started once does not call a stale one.
  const done = useRef(onSignedIn);
  useEffect(() => {
    done.current = onSignedIn;
  });

  useEffect(() => {
    let live = true;
    let paused = false;
    let run: { ctl: AbortController; settled: Promise<void> } | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const after = (ms: number) => {
      clearTimeout(timer);
      timer = setTimeout(() => void refresh(), ms);
    };

    const begin = () => {
      const ctl = new AbortController();
      const settled = (async () => {
        try {
          await signIn({ conditional: true, signal: ctl.signal });
          if (!ctl.signal.aborted) done.current();
          return;
        } catch (e) {
          // Our own abort (the button, a renewal, leaving) ends it quietly.
          if (ctl.signal.aborted) return;
          const cancelled = e instanceof AuthError && e.code === "cancelled";
          if (!cancelled) setFailed(true);
          after(cancelled ? REARM_AFTER_CANCEL_MS : REARM_AFTER_FAILURE_MS);
        }
      })();
      run = { ctl, settled };
      after(REARM_BEFORE_EXPIRY_MS);
    };

    // Ends the current run, waits for it (so its login/begin cannot land after the next one's), then starts again.
    const refresh = async () => {
      const old = run;
      old?.ctl.abort();
      await old?.settled;
      if (live && !paused) begin();
    };

    autofill.current = {
      async pause() {
        paused = true;
        clearTimeout(timer);
        run?.ctl.abort();
        await run?.settled;
      },
      resume() {
        paused = false;
        if (live && run) void refresh();
      },
    };

    void conditionalAvailable().then((ok) => {
      if (ok && live && !paused) begin();
    });
    return () => {
      live = false;
      clearTimeout(timer);
      run?.ctl.abort();
      autofill.current = null;
    };
  }, []);

  const click = async () => {
    setBusy(true);
    setFailed(false);
    await autofill.current?.pause();
    try {
      await signIn();
      done.current();
    } catch (e) {
      setFailed(!(e instanceof AuthError && e.code === "cancelled"));
      autofill.current?.resume();
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
