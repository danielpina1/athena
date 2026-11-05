// Settings › Notifications: Web Push on this device (On or Off, with why it cannot be when it cannot), a mute switch
// per kind of notification (the server's list; muting sends the whole muted list), and, on an iPhone or iPad outside
// the Home Screen app, the one step that makes notifications possible there.
import { useEffect, useId, useState, useSyncExternalStore } from "react";
import { errorMessage, setMuted } from "@/lib/commands/actions";
import { disablePush, enablePush, onPushError, onPushState, pushPending, pushState, type PushState } from "@/lib/push";
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { Waiting } from "@/ui/athena/Waiting";
import { Switch } from "@/ui/primitives/switch";
import { toast } from "@/ui/primitives/toast";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";
import { Block, Section } from "./Section";

const NOT_CHANGED = "Notifications not changed";

/** While the server has yet to hear of the change (offline: the outbox sends it once Athena is reachable). */
const PENDING: Partial<Record<PushState, string>> = {
  on: "On for this device; Athena is told when it is reachable.",
  off: "Off for this device; Athena is told when it is reachable.",
};

const HINT: Record<PushState, string> = {
  on: "This device is told when a Ticket needs you and no Athena screen is open.",
  off: "Turn on to be told on this device when a Ticket needs you.",
  denied: "Notifications are blocked in this browser's site settings for Athena.",
  unsupported: "Not available in this browser; on iPhone and iPad, add Athena to the Home Screen first.",
};

export const IOS_NOTE = "Add Athena to your Home Screen to get notifications on iPhone.";

/** The kinds as the Needs you labels say them. */
const KIND_LABEL: Record<string, string> = {
  approval: "Needs approval",
  answer: "Needs your answer",
  review: "Ready for review",
  delivery: "Delivery failed",
  blocked: "Blocked",
  failed: "Failed",
};

/** An iPhone or iPad (iPadOS Safari presents itself as a Mac with a touch screen). */
function isIOS(): boolean {
  const ua = navigator.userAgent;
  return /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && (navigator.maxTouchPoints ?? 0) > 1);
}

/** Opened from the Home Screen: Safari's navigator.standalone, or the display mode. */
function isStandalone(): boolean {
  if ((navigator as Navigator & { standalone?: boolean }).standalone === true) return true;
  return typeof window.matchMedia === "function" && window.matchMedia("(display-mode: standalone)").matches;
}

export function Notifications() {
  const [ios] = useState(() => isIOS() && !isStandalone());
  return (
    <Section title="Notifications" text="When this device tells you that a Ticket needs you.">
      <ThisDevice ios={ios} />
      <Kinds />
    </Section>
  );
}

function ThisDevice({ ios }: { ios: boolean }) {
  const state = useSyncExternalStore(onPushState, pushState);
  const pending = useSyncExternalStore(onPushState, pushPending);
  const [busy, setBusy] = useState(false);
  const id = useId();
  // The server refused a change after the control had let go of it.
  useEffect(() => onPushError((description) => toast({ title: NOT_CHANGED, description, tone: "danger" })), []);
  const fixed = state === "denied" || state === "unsupported";

  const set = async (want: "on" | "off") => {
    if (busy || want === state) return;
    setBusy(true);
    try {
      await (want === "on" ? enablePush() : disablePush());
    } catch (e) {
      toast({ title: NOT_CHANGED, description: e instanceof Error ? e.message : String(e), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Block title="This device">
      <div className="grid justify-items-start gap-2">
        <span id={`${id}-label`} className="sr-only">
          Notifications
        </span>
        <ToggleGroup
          type="single"
          aria-labelledby={`${id}-label`}
          aria-describedby={`${id}-hint`}
          aria-busy={busy || undefined}
          value={state === "on" ? "on" : "off"}
          // A tap on the chosen segment would clear the value: there is always one.
          onValueChange={(v) => v && void set(v as "on" | "off")}
          disabled={fixed || busy}
        >
          <ToggleGroupItem value="on">On</ToggleGroupItem>
          <ToggleGroupItem value="off">Off</ToggleGroupItem>
        </ToggleGroup>
        <p id={`${id}-hint`} className="max-w-prose min-w-0 text-sm text-muted">
          {(pending && PENDING[state]) || HINT[state]}
        </p>
        {ios && <p className="max-w-prose min-w-0 text-sm font-medium">{IOS_NOTE}</p>}
      </div>
    </Block>
  );
}

function Kinds() {
  const prefs = useAthena((s) => s.notifications);
  const synced = useAthena(selectSynced);
  // What the owner just switched, shown until the server's list catches up (or the change is refused).
  const [want, setWant] = useState<ReadonlyMap<string, boolean>>(new Map());
  const [seen, setSeen] = useState(prefs);
  if (seen !== prefs) {
    setSeen(prefs);
    setWant(new Map());
  }

  if (!synced || !prefs) {
    return (
      <Block title="Kinds">
        <Waiting />
      </Block>
    );
  }
  const muted = (kind: string) => want.get(kind) ?? prefs.muted.includes(kind);

  const toggle = async (kind: string, mute: boolean) => {
    const next = new Map(want).set(kind, mute);
    setWant(next);
    // The whole list, in the server's order of kinds: the command replaces it.
    const list = prefs.kinds.filter((k) => next.get(k) ?? prefs.muted.includes(k));
    const r = await setMuted(list);
    if (!r.ok) {
      setWant((w) => {
        const back = new Map(w);
        back.delete(kind);
        return back;
      });
      toast({ title: NOT_CHANGED, description: errorMessage(r.code), tone: "danger" });
    }
  };

  return (
    <Block title="Kinds">
      <p className="max-w-prose text-sm text-muted">A muted kind is never pushed to any device; it still shows in Needs you.</p>
      <ul className="grid">
        {prefs.kinds.map((kind) => {
          const label = KIND_LABEL[kind] ?? kind;
          return (
            <li key={kind} className="flex min-h-11 items-center justify-between gap-3 border-b border-line-soft py-1">
              <span className="min-w-0 break-words">{label}</span>
              <span className="flex items-center gap-2 text-sm text-muted">
                <span aria-hidden>{muted(kind) ? "Muted" : "On"}</span>
                <Switch
                  aria-label={`Mute ${label}`}
                  checked={muted(kind)}
                  onCheckedChange={(on) => void toggle(kind, on)}
                />
              </span>
            </li>
          );
        })}
      </ul>
    </Block>
  );
}
