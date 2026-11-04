// Settings › Notifications: Web Push on this device, On or Off. Task 17's Settings screen takes it over.
import { useEffect, useState, useSyncExternalStore } from "react";
import { disablePush, enablePush, onPushError, onPushState, pushPending, pushState, type PushState } from "@/lib/push";
import { toast } from "@/ui/primitives/toast";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";

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

export function NotificationsControl() {
  const state = useSyncExternalStore(onPushState, pushState);
  const pending = useSyncExternalStore(onPushState, pushPending);
  const [busy, setBusy] = useState(false);
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
    <section aria-labelledby="settings-notifications" className="mt-4 grid gap-2 border-t border-line pt-4">
      <h2 id="settings-notifications" className="font-mono text-xs text-muted uppercase">
        Notifications
      </h2>
      <div className="grid justify-items-start gap-2">
        <p id="settings-notifications-hint" className="max-w-prose min-w-0 text-sm text-muted">
          {(pending && PENDING[state]) || HINT[state]}
        </p>
        <ToggleGroup
          type="single"
          aria-labelledby="settings-notifications"
          aria-describedby="settings-notifications-hint"
          aria-busy={busy || undefined}
          value={state === "on" ? "on" : "off"}
          // A tap on the chosen segment would clear the value: there is always one.
          onValueChange={(v) => v && void set(v as "on" | "off")}
          disabled={fixed || busy}
        >
          <ToggleGroupItem value="on">On</ToggleGroupItem>
          <ToggleGroupItem value="off">Off</ToggleGroupItem>
        </ToggleGroup>
      </div>
    </section>
  );
}
