import { useState } from "react";
import { AuthError, enroll } from "@/lib/auth/session";
import { Button } from "@/ui/primitives/button";
import { Input } from "@/ui/primitives/input";

// /enroll: the single-use link `athena web enroll` prints carries its token in the fragment. The fragment is read
// once and removed from the address straight away (replaceState), so it is not in the history, a screenshot or a
// shared tab; it goes to the server once, in a POST body, never in a URL and never logged.

let held: string | undefined;

/** The token from the address's fragment, which this removes. Later calls on the same page return the same token. */
function takeEnrollToken(): string {
  const raw = location.hash.slice(1);
  if (raw) {
    try {
      held = decodeURIComponent(raw);
    } catch {
      held = raw;
    }
    history.replaceState(null, "", location.pathname + location.search);
  }
  return held ?? "";
}

/** Drops the token held in memory: it is spent (or the page is done with it). */
export function forgetEnrollToken(): void {
  held = undefined;
}

export function deviceNameFromUserAgent(ua: string, touchPoints = 0): string {
  if (/iPhone/.test(ua)) return "iPhone";
  if (/iPad/.test(ua)) return "iPad";
  if (/Android/.test(ua)) return "Android phone";
  // iPadOS Safari presents itself as a Mac; only a touch screen gives it away.
  if (/Macintosh/.test(ua) && touchPoints > 1) return "iPad";
  if (/Macintosh|Mac OS X/.test(ua)) return "Mac";
  if (/Windows/.test(ua)) return "Windows PC";
  if (/Linux|X11/.test(ua)) return "Linux PC";
  return "This device";
}

const NEW_LINK = <code className="font-mono">athena web enroll</code>;

type Outcome = "missing" | "spent" | "enrolled" | "failed";

export function Enroll({ onDone }: { onDone: () => void }) {
  // Lazy and idempotent: StrictMode's second render finds the token already held.
  const [token] = useState(takeEnrollToken);
  const [name, setName] = useState(() => deviceNameFromUserAgent(navigator.userAgent, navigator.maxTouchPoints));
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome | undefined>(token ? undefined : "missing");

  const create = async () => {
    setBusy(true);
    // The token is spent by the first request, whatever happens next: there is no second try with it.
    forgetEnrollToken();
    try {
      await enroll(token, name.trim());
      onDone();
    } catch (e) {
      if (e instanceof AuthError && e.code === "enrolled") setOutcome("enrolled");
      else setOutcome(e instanceof AuthError && e.status === 401 ? "spent" : "failed");
      setBusy(false);
    }
  };

  return (
    <main className="mx-auto grid w-full max-w-sm gap-4 px-4 py-16">
      <h1 className="text-2xl font-semibold">Athena</h1>
      {outcome === undefined && (
        <>
          <p className="text-muted">Name this device, then create its passkey.</p>
          <label className="grid gap-1 text-sm font-semibold">
            Device name
            <Input value={name} maxLength={64} onChange={(e) => setName(e.target.value)} />
          </label>
          <Button variant="accent" disabled={busy || !name.trim()} onClick={() => void create()}>
            Create passkey
          </Button>
        </>
      )}
      {outcome === "missing" && (
        <p role="alert" className="font-semibold">
          This page needs the link from {NEW_LINK}. Run it to get a new one.
        </p>
      )}
      {outcome === "spent" && (
        <p role="alert" className="font-semibold">
          This link was already used or has expired. Run {NEW_LINK} for a new one.
        </p>
      )}
      {outcome === "failed" && (
        <p role="alert" className="font-semibold">
          The passkey was not created, and this link is now used. Run {NEW_LINK} for a new link.
        </p>
      )}
      {outcome === "enrolled" && (
        <>
          <p role="status" className="font-semibold">
            Passkey saved. Sign in to continue.
          </p>
          <Button variant="accent" onClick={onDone}>
            Go to sign in
          </Button>
        </>
      )}
    </main>
  );
}
