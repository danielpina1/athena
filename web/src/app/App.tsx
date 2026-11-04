import { lazy, Suspense, useState } from "react";
import { boot, type Runtime } from "@/app/boot";
import { SessionGate } from "@/app/SessionGate";
import { Enroll } from "@/features/auth/Enroll";
import { Toaster } from "@/ui/primitives/toast";

// /design is the Precision kitchen sink: shipped, unlinked, and a lazy chunk, so it costs the first screen nothing.
// The owner previews against the real built server and the screenshot baselines use it. Neither it nor /enroll needs
// a session. Task 9's router replaces this pathname check and goes inside the SessionGate.
const DesignPage = lazy(() => import("@/app/routes/design"));

export function App({ runtime }: { runtime?: Runtime }) {
  const [path, setPath] = useState(() => window.location.pathname);
  if (path === "/design") {
    return (
      <Suspense fallback={null}>
        <DesignPage />
      </Suspense>
    );
  }
  if (path === "/enroll") {
    return (
      <>
        <Enroll
          onDone={() => {
            history.replaceState(null, "", "/");
            setPath("/");
          }}
        />
        <Toaster />
      </>
    );
  }
  return (
    <>
      <SessionGate runtime={runtime ?? boot()}>
        <main>
          <h1>Athena</h1>
        </main>
      </SessionGate>
      <Toaster />
    </>
  );
}
