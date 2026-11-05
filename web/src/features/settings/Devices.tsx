// Settings › Devices: the passkeys that can sign in (name, added, last used; the one this session signed in with is
// "This device"), Revoke with confirmation, and Add a device: a single-use enrollment link from POST
// /auth/enroll-link (after a passkey step-up when the server asks for one), shown as a QR code and as the link with
// Copy, until it expires. The link carries its token in the fragment; it lives in this component's state only: never
// in this page's address or history, never logged.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useCallback, useEffect, useId, useState } from "react";
import type { Passkey } from "@/gen/athena/v1/commands_pb";
import { requestReauth } from "@/features/auth/ReauthDialog";
import { AuthError, createEnrollLink, type EnrollLink, getSession, requestSignOut, sessionEnded } from "@/lib/auth/session";
import { errorMessage, listPasskeys, revokePasskey } from "@/lib/commands/actions";
import { Confirm } from "@/ui/athena/Confirm";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { Waiting } from "@/ui/athena/Waiting";
import { Button } from "@/ui/primitives/button";
import { toast } from "@/ui/primitives/toast";
import { Block, Section } from "./Section";

export const EXPIRED = "Expired — create a new link";

export function Devices() {
  const [passkeys, setPasskeys] = useState<Passkey[]>();
  const [current, setCurrent] = useState<string>();

  const refresh = useCallback(
    () =>
      listPasskeys().then((r) => {
        if (r.ok && r.result?.case === "passkeys") setPasskeys(r.result.value.passkeys);
        else if (!r.ok) toast({ title: "Devices not loaded", description: errorMessage(r.code), tone: "danger" });
      }),
    [],
  );

  useEffect(() => {
    void refresh();
    // Which passkey this session signed in with; without an answer no device is marked.
    getSession().then(
      (s) => setCurrent(s.passkeyId),
      () => {},
    );
  }, [refresh]);

  return (
    <Section title="Devices" text="The passkeys that can sign in to Athena, one per device.">
      <Block title="Passkeys">
        {passkeys === undefined ? (
          <Waiting />
        ) : passkeys.length === 0 ? (
          <p className="text-sm text-muted">No passkeys yet.</p>
        ) : (
          <ul className="grid">
            {passkeys.map((p) => (
              <DeviceRow key={p.id} passkey={p} current={p.id === current} onRevoked={() => void refresh()} />
            ))}
          </ul>
        )}
      </Block>
      <AddDevice onDone={() => void refresh()} />
      <SignOut />
    </Section>
  );
}

/** Ends this device's session (the passkey stays: it can sign in again). */
function SignOut() {
  const [busy, setBusy] = useState(false);
  const out = async () => {
    setBusy(true);
    try {
      await requestSignOut();
    } catch (e) {
      setBusy(false);
      toast({ title: "Not signed out", description: linkFailure(e), tone: "danger" });
    }
  };
  return (
    <Block title="This session">
      <p className="text-sm text-muted">Signing out drops this device's unsent actions. Its passkey still signs in again.</p>
      <div>
        <Confirm size="sm" label="Sign out" confirmLabel="Sign out of this device" disabled={busy} onConfirm={() => void out()} />
      </div>
    </Block>
  );
}

function DeviceRow({ passkey, current, onRevoked }: { passkey: Passkey; current: boolean; onRevoked(): void }) {
  const id = useId();
  const [busy, setBusy] = useState(false);
  const revoke = async () => {
    setBusy(true);
    const r = await revokePasskey(passkey.id, { thisDevice: current });
    setBusy(false);
    if (r.ok && current) {
      // This device can no longer sign in: leave now rather than wait for the server's close.
      sessionEnded();
      return;
    }
    // The session ended first (the sign-in screen is showing): never let that screen read as success.
    if (r.code === "not_done") {
      toast({ title: "Not revoked", description: "You were signed out first. Sign in and revoke it again.", tone: "danger" });
      return;
    }
    if (r.code === "signed_out") {
      toast({
        title: "Revoke not confirmed",
        description: "You were signed out before Athena answered. Sign in and check the device list.",
        tone: "danger",
      });
      return;
    }
    if (r.ok) {
      toast({ title: "Passkey revoked", description: `${passkey.name || "The device"} can no longer sign in.` });
      onRevoked();
    } else {
      toast({ title: "Not revoked", description: errorMessage(r.code), tone: "danger" });
    }
  };
  return (
    <li aria-labelledby={`${id}-name`} className="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-2 border-b border-line-soft py-3">
      <div className="grid min-w-0 flex-1 gap-0.5">
        <span className="flex min-w-0 flex-wrap items-center gap-2">
          <span id={`${id}-name`} className="min-w-0 font-semibold break-words">
            {passkey.name || "Unnamed device"}
          </span>
          {current && <span className="rounded-xs border border-line px-1.5 font-mono text-xs">This device</span>}
        </span>
        <span className="flex min-w-0 flex-wrap gap-x-3">
          <Mono>
            Added <RelativeTime at={passkey.createdAt ? timestampMs(passkey.createdAt) : Number.NaN} />
          </Mono>
          {passkey.lastUsedAt ? (
            <Mono>
              Last used <RelativeTime at={timestampMs(passkey.lastUsedAt)} />
            </Mono>
          ) : (
            <Mono>Never used</Mono>
          )}
        </span>
        {current && <p className="text-sm text-muted">Revoking it signs this device out.</p>}
      </div>
      <Confirm
        size="sm"
        label="Revoke"
        confirmLabel={current ? "Revoke and sign out this device" : "Revoke this passkey"}
        disabled={busy}
        onConfirm={() => void revoke()}
      />
    </li>
  );
}

/** The plain sentence for a failed link request. */
function linkFailure(e: unknown): string {
  if (e instanceof AuthError) return errorMessage(e.code);
  return "Could not reach Athena. Check your connection.";
}

/** Asks for a link; a step-up first when the server wants one (the same dialog the outbox uses). */
async function requestLink(): Promise<EnrollLink> {
  try {
    return await createEnrollLink();
  } catch (e) {
    if (!(e instanceof AuthError) || e.code !== "reauth_required") throw e;
    if (!(await requestReauth())) throw e;
    return await createEnrollLink();
  }
}

type Panel = { kind: "closed" } | { kind: "asking" } | { kind: "link"; link: EnrollLink };

function AddDevice({ onDone }: { onDone(): void }) {
  const [panel, setPanel] = useState<Panel>({ kind: "closed" });

  const create = async () => {
    setPanel({ kind: "asking" });
    try {
      setPanel({ kind: "link", link: await requestLink() });
    } catch (e) {
      if (e instanceof AuthError && e.code === "unauthenticated") sessionEnded();
      setPanel({ kind: "closed" });
      toast({ title: "No link created", description: linkFailure(e), tone: "danger" });
    }
  };

  return (
    <Block title="Add a device">
      {panel.kind === "link" ? (
        <LinkPanel
          key={panel.link.url}
          link={panel.link}
          onAgain={() => void create()}
          onDone={() => {
            setPanel({ kind: "closed" });
            onDone();
          }}
        />
      ) : (
        <>
          <p className="max-w-prose text-sm text-muted">
            Creates a single-use link that enrolls a passkey on another phone or computer.
          </p>
          <div>
            <Button variant="primary" disabled={panel.kind === "asking"} onClick={() => void create()}>
              Add a device
            </Button>
          </div>
        </>
      )}
    </Block>
  );
}

/** Seconds left until at, counted down by a local one-second timer (nothing is fetched). */
function useSecondsLeft(at: Date): number {
  const left = useCallback(() => Math.max(0, Math.ceil((at.getTime() - Date.now()) / 1000)), [at]);
  const [secs, setSecs] = useState(left);
  useEffect(() => {
    const t = setInterval(() => {
      const s = left();
      setSecs(s);
      if (s === 0) clearInterval(t);
    }, 1000);
    return () => clearInterval(t);
  }, [left]);
  return secs;
}

function clock(secs: number): string {
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = String(secs % 60).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
}

function LinkPanel({ link, onAgain, onDone }: { link: EnrollLink; onAgain(): void; onDone(): void }) {
  const secs = useSecondsLeft(link.expiresAt);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(link.url);
      toast({ title: "Link copied" });
    } catch {
      toast({ title: "Not copied", description: "Select the link and copy it.", tone: "danger" });
    }
  };

  if (secs === 0) {
    return (
      <div className="grid justify-items-start gap-3">
        <p className="font-medium">{EXPIRED}</p>
        <div className="flex flex-wrap gap-2">
          <Button variant="primary" onClick={onAgain}>
            Create a new link
          </Button>
          <Button variant="ghost" onClick={onDone}>
            Done
          </Button>
        </div>
      </div>
    );
  }
  return (
    <div className="grid min-w-0 gap-3 lg:grid-cols-[auto_1fr] lg:items-start lg:gap-6">
      <Qr text={link.url} />
      <div className="grid min-w-0 content-start gap-3">
        <p className="max-w-prose text-sm">
          Scan the code with the new device's camera, or open the link on it. The link works once.
        </p>
        <p data-volatile="enroll-link" className="min-w-0 rounded-md border border-line-soft bg-surface-2 px-3 py-2 font-mono text-xs break-all">
          {link.url}
        </p>
        <p data-volatile="enroll-expiry" className="font-mono text-sm tabular-nums">{`Expires in ${clock(secs)}`}</p>
        <div className="flex flex-wrap gap-2">
          <Button onClick={() => void copy()}>Copy</Button>
          <Button variant="ghost" onClick={onDone}>
            Done
          </Button>
        </div>
      </div>
    </div>
  );
}

/**
 * The link as a QR code: black modules on white in both themes (cameras read it best), with a 4-module quiet zone.
 * uqr loads with the first link, in its own chunk: the initial route does not carry it.
 */
function Qr({ text }: { text: string }) {
  const [qr, setQr] = useState<{ text: string; size: number; d: string }>();
  useEffect(() => {
    let live = true;
    void import("uqr").then(({ encode }) => {
      if (!live) return;
      const code = encode(text, { ecc: "M", border: 4 });
      let d = "";
      code.data.forEach((row, y) =>
        row.forEach((on, x) => {
          if (on) d += `M${x},${y}h1v1h-1z`;
        }),
      );
      setQr({ text, size: code.size, d });
    });
    return () => {
      live = false;
    };
  }, [text]);
  const box = "size-56 shrink-0 rounded-md border border-line-soft";
  if (!qr || qr.text !== text) return <div aria-hidden className={`${box} bg-surface-2`} />;
  return (
    <svg
      role="img"
      aria-label="QR code of the enrollment link"
      data-volatile="enroll-qr"
      viewBox={`0 0 ${qr.size} ${qr.size}`}
      shapeRendering="crispEdges"
      className={box}
    >
      <rect width={qr.size} height={qr.size} fill="#FFFFFF" />
      <path d={qr.d} fill="#000000" />
    </svg>
  );
}
