// "A new version is ready — Reload": shown under the connection banner when a newer app is waiting (lib/update.ts).
// Like the connection banner, its live region is always in the document, so showing it is announced once.
import { useSyncExternalStore } from "react";
import { appUpdates, type Updates } from "@/lib/update";

const none = () => () => {};
const notReady = () => false;

export function UpdateBanner({ updates = appUpdates() }: { updates?: Updates }) {
  const ready = useSyncExternalStore(updates?.subscribe ?? none, updates?.ready ?? notReady);
  return (
    <div
      role="status"
      className={ready ? "flex flex-wrap items-center gap-x-1 border-b border-line bg-surface-2 px-4 text-sm" : undefined}
    >
      {ready && (
        <>
          <span className="py-2 font-semibold">A new version is ready —</span>{" "}
          <button
            type="button"
            onClick={() => updates?.apply()}
            className="-mx-2 inline-flex min-h-11 items-center px-2 font-semibold text-text underline underline-offset-2 lg:min-h-8"
          >
            Reload
          </button>
        </>
      )}
    </div>
  );
}
