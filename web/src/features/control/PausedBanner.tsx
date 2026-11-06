// Shown above the content while Athena is paused. Orange: it is the owner who must act to start anything again.
// Resume is sensitive; the outbox asks for the passkey (the re-auth dialog) and resends, so a failure here is the
// owner declining or the server refusing.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useState } from "react";
import { errorMessage, resume } from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { Button } from "@/ui/primitives/button";
import { toast } from "@/ui/primitives/toast";

export function PausedBanner() {
  const control = useAthena((s) => s.control);
  const [busy, setBusy] = useState(false);
  if (!control?.paused) return null;

  const go = async () => {
    setBusy(true);
    try {
      const r = await resume();
      if (!r.ok) toast({ title: "Could not resume Athena", description: errorMessage(r.code), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      role="status"
      data-paused-banner
      className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b border-l-4 border-b-line border-l-accent bg-surface-2 px-4 py-2 text-sm"
    >
      <p className="min-w-0">
        <span className="font-semibold">
          Athena is paused since{" "}
          {control.since ? <RelativeTime at={timestampMs(control.since)} /> : "an unknown time"}.
        </span>{" "}
        Nothing new starts and replies wait.
      </p>
      <Button size="sm" disabled={busy} onClick={() => void go()} className="border-accent-ink text-accent-ink">
        Resume
      </Button>
    </div>
  );
}
