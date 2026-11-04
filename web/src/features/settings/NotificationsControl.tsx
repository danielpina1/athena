// Settings › Notifications: Web Push on this device, On or Off. Task 17's Settings screen takes it over.
import { useState, useSyncExternalStore } from "react";
import { disablePush, enablePush, onPushState, pushState, type PushState } from "@/lib/push";
import { toast } from "@/ui/primitives/toast";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";

const HINT: Record<PushState, string> = {
  on: "This device is told when a Ticket needs you and no Athena screen is open.",
  off: "Turn on to be told on this device when a Ticket needs you.",
  denied: "Notifications are blocked in this browser's site settings for Athena.",
  unsupported: "Not available in this browser; on iPhone and iPad, add Athena to the Home Screen first.",
};

export function NotificationsControl() {
  const state = useSyncExternalStore(onPushState, pushState);
  const [busy, setBusy] = useState(false);
  const fixed = state === "denied" || state === "unsupported";

  const set = async (want: "on" | "off") => {
    if (busy || want === state) return;
    setBusy(true);
    try {
      await (want === "on" ? enablePush() : disablePush());
    } catch (e) {
      toast({ title: "Notifications not changed", description: e instanceof Error ? e.message : String(e), tone: "danger" });
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
          {HINT[state]}
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
